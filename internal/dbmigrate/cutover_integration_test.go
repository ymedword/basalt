//go:build integration

package dbmigrate

// 本文件判卷**切换事实**与**回退窗口**（颗粒度文档 §2.15，C-13 / C-39 / C-43；T066）：
// 切换只认 `storage_cutover` 行，`first_write_at` 由首个业务事务写入，写后只前滚。
// 两个用例都走真实 SQLite v6 夹具 → 真实 `Exporter` / `Importer` → 临时 PG，
// 不用手写假 dump，也不把切换判据缩成"DSN 能连"。
// Both cases run the real fixture through the real exporter and importer into a
// throwaway PostgreSQL schema; the cutover fact is never reduced to "the DSN works".

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// TestCutover_RequiresMarkerNotJustDSN 目标库连得通、业务数据也已导入，都**不**等于
// 切换发生过（C-39）：切换事实只认 `storage_cutover` 行。切换记录在写的那一刻
// `first_write_at` 必须为 NULL（留给首个业务事务），同一导出批次不得切换两次。
// 逆改点：`RequireCutover` 读不到行时返回零值记录 + nil → 第一段断言红；
// 切换顺带写 `first_write_at` → 第二段断言红；
// 不把唯一索引冲突映射成哨兵 → 第三段 `ErrorIs` 红（拒绝仍在，错误身份丢失）。
func TestCutover_RequiresMarkerNotJustDSN(t *testing.T) {
	source := migrationFixture(t)
	seedMigrationRows(t, source)
	dump, manifest := exportFixture(t, source)

	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dump, manifest).Import(importCtx)
	require.NoError(t, err)

	// 数据已在目标库、连接可用——仍然不是"已切换"。
	_, err = RequireCutover(importCtx, db)
	require.ErrorIs(t, err, ErrNoCutoverMarker,
		"已导入不等于已切换；切换事实只认 storage_cutover 行")
	require.Contains(t, err.Error(), "storage_cutover", "错误必须点名缺席的切换记录")

	switchedAt := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	record, err := WriteCutover(importCtx, db, CutoverRequest{
		ManifestDir: dump,
		Operator:    "cli:test-operator",
		CutoverAt:   switchedAt,
	})
	require.NoError(t, err)
	require.Equal(t, "cut_"+strconv.FormatInt(switchedAt.UnixMilli(), 10), record.CutoverID)
	require.Equal(t, SourceSchemaVersion, record.SourceSchemaVersion, "源谱系版本取清单声明值")
	require.Equal(t, switchedAt.UnixMilli(), record.CutoverAt, "cutover_at 必须写 BIGINT 毫秒")
	require.Equal(t, "cli:test-operator", record.Operator)
	require.Nil(t, record.FirstWriteAt, "切换只写切点，first_write_at 必须留给首个业务事务")
	require.True(t, record.RollbackWindowOpen(), "刚切换完仍在回退窗口")
	require.NoError(t, RequireOpenRollbackWindow(record))

	// 回读逐字段一致；manifest_sha256 绑定清单**文件本身**（操作者可独立重算）。
	stored, err := RequireCutover(importCtx, db)
	require.NoError(t, err)
	require.Equal(t, record, stored)
	raw, err := os.ReadFile(filepath.Join(dump, ManifestFileName))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(sum[:]), stored.ManifestSHA256,
		"切换记录必须绑定导出清单文件的摘要")

	// 同一导出批次不得切换两次（切换事实一次性）。
	_, err = WriteCutover(importCtx, db, CutoverRequest{
		ManifestDir: dump,
		Operator:    "cli:test-operator",
		CutoverAt:   switchedAt.Add(time.Minute),
	})
	require.ErrorIs(t, err, ErrCutoverBatchRepeated)
	var rows int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM storage_cutover`).Scan(&rows))
	require.Equal(t, 1, rows, "被拒的重复切换不得留下第二行")
}

// TestRollback_AfterFirstWriteForwardOnly 首个成功业务事务写过 `first_write_at` 后，
// 回退窗口关闭，只剩前滚（C-13 / C-43）。新数据只在目标库，重启旧 SQLite 不是回退。
// 逆改点：切换时就写 `first_write_at` → 第一段"仍在窗口"断言红；
// `RollbackWindowOpen` 恒为真 → 第二段断言红；
// `RequireOpenRollbackWindow` 不看标记 → `ErrorIs(ErrRollbackWindowClosed)` 红。
func TestRollback_AfterFirstWriteForwardOnly(t *testing.T) {
	source := migrationFixture(t)
	seedMigrationRows(t, source)
	dump, manifest := exportFixture(t, source)

	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dump, manifest).Import(importCtx)
	require.NoError(t, err)

	record, err := WriteCutover(importCtx, db, CutoverRequest{
		ManifestDir: dump,
		Operator:    "cli:test-operator",
		CutoverAt:   time.Date(2026, 10, 28, 13, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.NoError(t, RequireOpenRollbackWindow(record), "首次成功业务写入前仍在回退窗口")

	// 首个业务事务：新会话与 first_write_at 同一事务落库（C-13）。
	firstWrite := time.Date(2026, 10, 28, 13, 5, 0, 0, time.UTC)
	outcome, err := db.Tx(importCtx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
			VALUES ('session-new', 'demo', 'cli', 'u1', 'active', 0, 2000, 2000)`); err != nil {
			return err
		}
		return db.MarkFirstWrite(ctx, tx, firstWrite)
	})
	require.NoError(t, err)
	require.Equal(t, postgres.TxCommitted, outcome)

	stored, err := RequireCutover(importCtx, db)
	require.NoError(t, err)
	require.NotNil(t, stored.FirstWriteAt, "首个业务事务必须留下首写标记")
	require.Equal(t, firstWrite.UTC().UnixMilli(), *stored.FirstWriteAt, "first_write_at 必须写 BIGINT 毫秒")
	require.False(t, stored.RollbackWindowOpen(), "首次成功业务写入后只前滚")
	require.ErrorIs(t, RequireOpenRollbackWindow(stored), ErrRollbackWindowClosed)
	require.Contains(t, RequireOpenRollbackWindow(stored).Error(), stored.CutoverID,
		"拒绝必须指出是哪一次切换")

	// 新行只在目标库：重启旧 SQLite 会丢掉它，因此不得宣称无损回退。
	var targetCount, sourceCount int
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT count(*) FROM sessions WHERE session_id = 'session-new'`).Scan(&targetCount))
	require.Equal(t, 1, targetCount)
	require.NoError(t, source.QueryRowContext(importCtx,
		`SELECT count(*) FROM sessions WHERE session_id = 'session-new'`).Scan(&sourceCount))
	require.Zero(t, sourceCount, "旧库没有这行——重启旧 SQLite 不是无损回退")
}
