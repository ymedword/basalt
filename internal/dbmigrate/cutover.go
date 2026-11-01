package dbmigrate

// 本文件是**切换事实**与**回退窗口**的唯一判定点（颗粒度文档 §2.15 / §3.1.8，
// C-13 / C-39 / C-43）。两条纪律：
//  1. 切换事实只认 `storage_cutover` 行——目标库连得通、业务数据已导入，都**不**等于
//     切换发生过；"配置里改了 DSN"更不是证据。
//  2. `first_write_at` 一旦非空，回退窗口即关闭，回退入口**必须**先过
//     `RequireOpenRollbackWindow`，此后只允许前滚。
//
// This file is the single decision point for the cutover fact and the rollback window.
// A reachable, fully imported target is still not a switched instance: only the
// storage_cutover row says so. Once first_write_at is set the window is closed for good,
// and every rollback entry point must pass RequireOpenRollbackWindow first.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ymedword/basalt/internal/storage/postgres"
)

var (
	// ErrNoCutoverMarker 表示目标库没有 `storage_cutover` 行：DSN 可用与已导入的业务数据
	// 都**不**构成切换事实，目标实例不得据以宣称"已切换"。
	// ErrNoCutoverMarker means the target has no cutover row; a live DSN and imported
	// rows are not evidence of a switch.
	ErrNoCutoverMarker = errors.New("dbmigrate: 目标库没有 storage_cutover 记录——DSN 与已导入的数据都不等于切换事实")

	// ErrCutoverBatchRepeated 表示同一导出批次（同一 `manifest.json`）已经切换过；
	// 切换事实一次性，不得在库里留下第二行同批记录。
	// ErrCutoverBatchRepeated means this exact export batch was already switched once.
	ErrCutoverBatchRepeated = errors.New("dbmigrate: 同一导出批次已切换过，切换记录不得重复")

	// ErrRollbackWindowClosed 表示首次成功业务写入已经落库（`first_write_at` 非空），
	// 回退窗口关闭，只允许前滚；新数据只在目标库，重启旧 SQLite 不是无损回退。
	// ErrRollbackWindowClosed means the first business write landed: forward only.
	ErrRollbackWindowClosed = errors.New("dbmigrate: 首次成功业务写入后回退窗口已关闭，只允许前滚")
)

// cutoverUniqueConstraint 是 `storage_cutover(manifest_sha256)` 的唯一索引名
// （`internal/storage/postgres/migrations/0001_postgres_v1.sql`）；重复切换靠它兜底。
const cutoverUniqueConstraint = "ux_storage_cutover_manifest"

// CutoverRecord 是 `storage_cutover` 的一行：切换证据，不做业务解释。
// `FirstWriteAt` 为 nil 表示回退窗口仍开；非 nil 即只允许前滚。
// CutoverRecord is one storage_cutover row; a nil FirstWriteAt means the window is open.
type CutoverRecord struct {
	CutoverID           string
	SourceSchemaVersion int
	ManifestSHA256      string
	CutoverAt           int64 // Unix 毫秒（BIGINT），不得换成 timestamptz / now()
	Operator            string
	FirstWriteAt        *int64 // Unix 毫秒；nil = 回退窗口仍开
}

// RollbackWindowOpen 判回退窗口是否仍开：只有 `first_write_at IS NULL` 才是开着的。
// 该标记**只**证明数据库业务写入（且只限经 `MarkFirstWrite` 的写路径）；工作区文件与
// 外部副作用的干净与否由三维判据另行取证（C-43 / T067），不能用它替代。
func (r CutoverRecord) RollbackWindowOpen() bool { return r.FirstWriteAt == nil }

// CutoverRequest 是写切换事实的输入：清单目录（绑定切换批次）、操作者与显式时刻。
// 时刻显式传入而不读 `now()`，与 `MarkFirstWrite` 同一口径，测试可注入。
// CutoverRequest is the input to the cutover write: the manifest directory that binds the
// batch, the operator, and an explicit instant.
type CutoverRequest struct {
	ManifestDir string
	Operator    string
	CutoverAt   time.Time
}

// WriteCutover 写一条切换事实：`storage_cutover` 行（`first_write_at` 保持 NULL）。
//
// 绑定关系由函数自己建立而不靠调用方自觉：清单必须能被 `ReadManifest` 接受
// （`format_version=1`、源谱系 v6），`manifest_sha256` 取清单文件本身的摘要，
// `source_schema_version` 取清单声明值。同一批次的第二行被唯一索引拒绝并映射为
// `ErrCutoverBatchRepeated`。
//
// WriteCutover inserts the cutover fact with first_write_at left NULL. The binding to an
// export batch is established here, not trusted from the caller: the manifest must validate,
// and its file digest and declared source version are recorded.
func WriteCutover(ctx context.Context, db *postgres.DB, req CutoverRequest) (CutoverRecord, error) {
	if db == nil {
		return CutoverRecord{}, errors.New("dbmigrate: 写切换记录需要已装配的目标库句柄")
	}
	operator := strings.TrimSpace(req.Operator)
	if operator == "" {
		return CutoverRecord{}, errors.New("dbmigrate: 切换记录必须写明操作者（token 指纹 / cli:用户 / migration）")
	}
	if req.CutoverAt.IsZero() {
		return CutoverRecord{}, errors.New("dbmigrate: 切换时刻必须显式给出，空时刻会让切换证据失真")
	}
	dir := strings.TrimSpace(req.ManifestDir)
	if dir == "" {
		return CutoverRecord{}, errors.New("dbmigrate: 切换必须绑定一个导出清单目录")
	}

	manifest, err := ReadManifest(dir)
	if err != nil {
		return CutoverRecord{}, err
	}
	manifestSHA, err := manifestFileSHA256(dir)
	if err != nil {
		return CutoverRecord{}, err
	}

	millis := req.CutoverAt.UTC().UnixMilli()
	record := CutoverRecord{
		CutoverID:           "cut_" + strconv.FormatInt(millis, 10),
		SourceSchemaVersion: manifest.SourceSchemaVersion,
		ManifestSHA256:      manifestSHA,
		CutoverAt:           millis,
		Operator:            operator,
	}
	_, err = db.ExecContext(ctx, `INSERT INTO storage_cutover
		(cutover_id, source_schema_version, manifest_sha256, cutover_at, operator, first_write_at)
		VALUES ($1, $2, $3, $4, $5, NULL)`,
		record.CutoverID, record.SourceSchemaVersion, record.ManifestSHA256, record.CutoverAt, record.Operator)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == cutoverUniqueConstraint {
			return CutoverRecord{}, fmt.Errorf("%w: manifest_sha256=%s", ErrCutoverBatchRepeated, record.ManifestSHA256)
		}
		return CutoverRecord{}, fmt.Errorf("dbmigrate: 写切换记录: %w", err)
	}
	return record, nil
}

// RequireCutover 读**当前**切换行（与 `MarkFirstWrite` 同一排序：`cutover_at` 降序再按
// `cutover_id` 降序）；没有行即 `ErrNoCutoverMarker`。回退后再切换会产生新行，当前行
// 随之前移，历史行不被改写。
//
// RequireCutover reads the current cutover row using the same ordering as MarkFirstWrite,
// and refuses with ErrNoCutoverMarker when none exists.
func RequireCutover(ctx context.Context, db *postgres.DB) (CutoverRecord, error) {
	if db == nil {
		return CutoverRecord{}, errors.New("dbmigrate: 读切换记录需要已装配的目标库句柄")
	}
	var (
		record     CutoverRecord
		firstWrite sql.NullInt64
	)
	err := db.QueryRowContext(ctx, `SELECT cutover_id, source_schema_version, manifest_sha256,
			cutover_at, operator, first_write_at
		FROM storage_cutover
		ORDER BY cutover_at DESC, cutover_id DESC
		LIMIT 1`).
		Scan(&record.CutoverID, &record.SourceSchemaVersion, &record.ManifestSHA256,
			&record.CutoverAt, &record.Operator, &firstWrite)
	if errors.Is(err, sql.ErrNoRows) {
		return CutoverRecord{}, fmt.Errorf("%w（目标库可连不代表它被切换过）", ErrNoCutoverMarker)
	}
	if err != nil {
		return CutoverRecord{}, fmt.Errorf("dbmigrate: 读切换记录: %w", err)
	}
	if firstWrite.Valid {
		value := firstWrite.Int64
		record.FirstWriteAt = &value
	}
	return record, nil
}

// RequireOpenRollbackWindow 是**所有**回退入口的第一道门（C-13）：`first_write_at` 非空即
// `ErrRollbackWindowClosed`，后续三维判据（C-43）也不得越过它。窗口开只说明数据库业务
// 写入维度干净，**不**代表可以自动回退——文件与外部副作用仍须逐维取证。
//
// RequireOpenRollbackWindow is the first gate of every rollback entry point; an open window
// is necessary but not sufficient for an automatic rollback.
func RequireOpenRollbackWindow(record CutoverRecord) error {
	if record.FirstWriteAt == nil {
		return nil
	}
	return fmt.Errorf("%w: cutover_id=%s first_write_at=%d",
		ErrRollbackWindowClosed, record.CutoverID, *record.FirstWriteAt)
}

// manifestFileSHA256 取 `<dir>/manifest.json` 文件内容本身的摘要：切换记录绑定的是
// 操作者能独立重算的同一份清单字节，而不是再序列化一遍的内存结构。
// manifestFileSHA256 digests the manifest file's bytes, the same bytes an operator can
// recompute independently.
func manifestFileSHA256(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("dbmigrate: 打开导出目录: %w", err)
	}
	defer func() { _ = root.Close() }()

	raw, err := root.ReadFile(ManifestFileName)
	if err != nil {
		return "", fmt.Errorf("dbmigrate: 读清单: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
