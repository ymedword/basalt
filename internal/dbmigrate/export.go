package dbmigrate

// 本文件是 SQLite v6 的**只读导出**（C-30 / §2.11.2 导出段）：先按 `VACUUM INTO` 落一份
// 停机快照，再**只读打开快照**（不是源库）逐表读行、按实际存储类编码、算三个摘要，
// 最后写 `manifest.json`。源库只被读，不被改；快照单独留档，供重试时重签清单。
// This file is the read-only SQLite v6 export: snapshot with VACUUM INTO, open the snapshot
// read-only, encode every row by its actual storage class, then write the manifest.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrSourceVersionUnsupported 是源库版本不受支持的哨兵：v5 必须先由当前发布版升级到 v6，
// v7 高于本工具的谱系。两种情况都**不得**自动修补或降级猜测。
var ErrSourceVersionUnsupported = errors.New("dbmigrate: 源库 schema 版本不受支持")

// driverName 是 SQLite 驱动在 database/sql 里的注册名。导出端只认识 SQLite 源，
// 因此这里固定驱动名——目标侧（PostgreSQL）不经本包连接。
const driverName = "sqlite"

// snapshotFileName 是停机快照在导出目录里的固定名字。
const snapshotFileName = "source-snapshot.db"

// Exporter 把一份 SQLite 源库导成 JSONL + 清单。
// Exporter exports one SQLite source into JSONL plus a manifest.
type Exporter struct {
	src    *sql.DB
	target int
}

// NewExporter 绑定源库句柄与**本工具支持的源谱系版本**；版本门在 `Export` 里判，
// 构造时不碰数据库（构造失败与否不该取决于连接时序）。
// NewExporter binds the source handle and the only accepted source version; the version gate
// runs in Export, so construction never touches the database.
func NewExporter(src *sql.DB, target int) *Exporter {
	return &Exporter{src: src, target: target}
}

// Export 把源库导到 `outDir`：快照 → 只读逐表编码 → JSONL + `manifest.json`。
// 只填本包能证明的字段：批次号与快照摘要在这里生成；`source_instance_id` 来自名册，
// 归 `db export`（T070）在拿到名册后补写，本包不猜实例身份。
// Export writes the snapshot, the JSONL files and the manifest; the instance identity comes
// from the roster and is filled by the CLI layer, never guessed here.
func (e *Exporter) Export(ctx context.Context, outDir string) (Manifest, error) {
	version, err := e.sourceVersion(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if version != e.target {
		return Manifest{}, fmt.Errorf("%w: 源库是 v%d，本工具只导出 v%d（v5 需先由当前发布版升级；更高的版本不接受）",
			ErrSourceVersionUnsupported, version, e.target)
	}

	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("dbmigrate: 建导出目录: %w", err)
	}
	// 落盘一律经 `os.Root` 限定在导出目录内：文件名虽是常量，但目录由调用方给定，
	// 根限定让写入不可能落到目录之外。
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return Manifest{}, fmt.Errorf("dbmigrate: 打开导出目录: %w", err)
	}
	defer func() { _ = root.Close() }()

	snapshotPath := filepath.Join(outDir, snapshotFileName)
	snapshotSHA, err := e.snapshot(ctx, root, snapshotPath)
	if err != nil {
		return Manifest{}, err
	}

	snapshot, err := openReadOnlySnapshot(ctx, snapshotPath)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = snapshot.Close() }()

	names, err := sourceTables(ctx, snapshot)
	if err != nil {
		return Manifest{}, err
	}

	batchID, err := newBatchID()
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{
		FormatVersion:        ManifestFormatVersion,
		MigrationBatchID:     batchID,
		SourceSchemaVersion:  version,
		SourceSnapshotSHA256: snapshotSHA,
		GeneratedAt:          time.Now().UTC(),
		Tables:               make([]TableManifest, 0, len(names)),
	}

	for _, name := range names {
		table, err := e.exportTable(ctx, snapshot, root, name)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Tables = append(manifest.Tables, table)
	}

	if err := WriteManifest(outDir, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// sourceVersion 读源库的 `schema_version` 最高版本；版本表缺失（或零行）返回 0，
// 由调用方按"不受支持"处理——**不**把"没有版本"当成"最新版本"。
func (e *Exporter) sourceVersion(ctx context.Context) (int, error) {
	var exists int
	if err := e.src.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&exists); err != nil {
		return 0, fmt.Errorf("dbmigrate: 查询源库版本表: %w", err)
	}
	if exists == 0 {
		return 0, fmt.Errorf("%w: 源库没有 schema_version 表", ErrSourceVersionUnsupported)
	}
	var version int
	if err := e.src.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("dbmigrate: 读取源库版本: %w", err)
	}
	return version, nil
}

// snapshot 用 `VACUUM INTO` 落停机快照并返回快照文件的 SHA-256。
// 快照文件已存在即拒绝——**不**覆盖上一次的快照（那会让"重试用同一份快照"变成空话）。
// 目标路径作为**绑定参数**传入（`VACUUM INTO ?`），不拼字符串。
func (e *Exporter) snapshot(ctx context.Context, root *os.Root, path string) (string, error) {
	if _, err := root.Stat(snapshotFileName); err == nil {
		return "", fmt.Errorf("dbmigrate: 快照文件已存在，拒绝覆盖: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("dbmigrate: 检查快照文件: %w", err)
	}
	if _, err := e.src.ExecContext(ctx, "VACUUM INTO ?", filepath.ToSlash(path)); err != nil {
		return "", fmt.Errorf("dbmigrate: 生成停机快照: %w", err)
	}
	return fileSHA256(root, snapshotFileName)
}

// openReadOnlySnapshot 只读打开快照文件（不是源库），并设 `query_only`。
// 只读是边界，不是纪律：句柄本身不允许写，后面的读不可能改到数据。
func openReadOnlySnapshot(ctx context.Context, path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=query_only(1)"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 打开快照: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dbmigrate: 连接快照: %w", err)
	}
	return db, nil
}

// OpenSourceReadOnly 只读打开一份 SQLite 源库，供 `db export` 在静止窗口**之前**建立
// 连接（句柄在 A2 之后由调用方关闭）。
//
// 三条实测依据（T070，2026-10-29，Windows + modernc.org/sqlite）：
//  1. 只读句柄关闭时**不** checkpoint：`basalt.db` 的字节与 `-wal` 都不变——"源库只被读，
//     不被改"因此是可验证的事实，而不是注释里的承诺；
//  2. 打开本身会建 `basalt.db-wal`（0 字节）与 `basalt.db-shm`，关闭后两者仍在。所以连接
//     **必须**在 A1 之前打开：窗口内打开会让 A1 与 A2 的源库文件身份不等；关闭后文件
//     不回退，`db switch` 的重盘看到的仍是同一份身份；
//  3. `query_only` pragma 会挡 `VACUUM INTO`（实测 `attempt to write a readonly database`），
//     因此这里**不**设该 pragma——只读边界由 `mode=ro` 打开标志本身提供。
//
// OpenSourceReadOnly must be established before A1 (opening creates -wal/-shm) and skips
// the query_only pragma because VACUUM INTO would then fail.
func OpenSourceReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("dbmigrate: 只读打开源库需要库文件路径")
	}
	db, err := sql.Open(driverName, "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 打开源库: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dbmigrate: 连接源库 %s: %w", path, err)
	}
	return db, nil
}

// sourceTables 列出快照里的业务表，按 `MigrationOrder` 排定次序。
// 顺序里没有的表即 schema 漂移——**不**按表名字典序硬塞进迁移。
func sourceTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name <> 'schema_version' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 列出源表: %w", err)
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("dbmigrate: 读表名: %w", err)
		}
		present[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbmigrate: 遍历源表: %w", err)
	}

	ordered := make([]string, 0, len(present))
	known := make(map[string]struct{}, len(migrationOrder))
	for _, name := range migrationOrder {
		known[name] = struct{}{}
		if _, ok := present[name]; ok {
			ordered = append(ordered, name)
		}
	}
	for name := range present {
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("dbmigrate: 源库有迁移顺序里没有的表 %q——先补拓扑序再导出", name)
		}
	}
	return ordered, nil
}

// columnInfo 是 `PRAGMA table_xinfo` 里本包用得到的那几列。
type columnInfo struct {
	Name    string
	Decl    string
	NotNull bool
	PK      int
	Hidden  int
}

// exportTable 导一张表：列元数据 → 逐行编码 → JSONL → 三个摘要。
func (e *Exporter) exportTable(ctx context.Context, db *sql.DB, root *os.Root, table string) (TableManifest, error) {
	columns, err := tableColumns(ctx, db, table)
	if err != nil {
		return TableManifest{}, err
	}
	pk := primaryKeyColumns(columns)
	if len(pk) == 0 {
		return TableManifest{}, fmt.Errorf("dbmigrate: 表 %q 没有主键——ID 集合摘要无从计算", table)
	}

	names := make([]string, len(columns))
	for i, column := range columns {
		names[i] = column.Name
	}
	rows, err := readTableRows(ctx, db, table, columns)
	if err != nil {
		return TableManifest{}, err
	}

	idSet, err := DigestIDSet(pk, rows)
	if err != nil {
		return TableManifest{}, fmt.Errorf("dbmigrate: 表 %q 的 ID 集合摘要: %w", table, err)
	}
	digest := DigestRows(rows)
	if digest.Err != nil {
		return TableManifest{}, fmt.Errorf("dbmigrate: 表 %q 的行摘要: %w", table, digest.Err)
	}

	jsonlSHA, err := writeJSONL(root, table+".jsonl", rows, names)
	if err != nil {
		return TableManifest{}, fmt.Errorf("dbmigrate: 表 %q 的 JSONL: %w", table, err)
	}

	meta := make([]ColumnMeta, len(columns))
	for i, column := range columns {
		pgType, encoding, err := mapColumnType(table, column.Name, column.Decl)
		if err != nil {
			return TableManifest{}, fmt.Errorf("dbmigrate: 表 %q 的列 %q: %w", table, column.Name, err)
		}
		meta[i] = ColumnMeta{
			Name:       column.Name,
			SQLiteDecl: column.Decl,
			PGType:     pgType,
			Nullable:   !column.NotNull,
			Encoding:   encoding,
		}
	}

	return TableManifest{
		Name:        table,
		RowCount:    int64(len(rows)),
		Columns:     meta,
		IDSetSHA256: idSet,
		RowSHA256:   digest.RowSHA256,
		JSONLSHA256: jsonlSHA,
	}, nil
}

// tableColumns 读表的可写列（`hidden` 非零的生成列 / 隐藏列一律排除——PG 侧会重算）。
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]columnInfo, error) {
	// PRAGMA 不支持参数绑定，表名只能拼进去；表名来自 sqlite_master 且已按迁移顺序校验。
	rows, err := db.QueryContext(ctx, `PRAGMA table_xinfo(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 读表 %q 的列: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var columns []columnInfo
	for rows.Next() {
		var (
			cid     int
			name    string
			decl    string
			notNull int
			dflt    any
			pk      int
			hidden  int
		)
		if err := rows.Scan(&cid, &name, &decl, &notNull, &dflt, &pk, &hidden); err != nil {
			return nil, fmt.Errorf("dbmigrate: 读表 %q 的列定义: %w", table, err)
		}
		if hidden != 0 {
			continue
		}
		columns = append(columns, columnInfo{Name: name, Decl: decl, NotNull: notNull != 0, PK: pk, Hidden: hidden})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbmigrate: 遍历表 %q 的列: %w", table, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("dbmigrate: 表 %q 没有可写列", table)
	}
	return columns, nil
}

// primaryKeyColumns 按 `pk` 序号排主键列（复合主键的列序即声明序，不是表内物理序）。
func primaryKeyColumns(columns []columnInfo) []string {
	var pk []string
	for order := 1; ; order++ {
		found := ""
		for _, column := range columns {
			if column.PK == order {
				found = column.Name
				break
			}
		}
		if found == "" {
			return pk
		}
		pk = append(pk, found)
	}
}

// selectAllQuery 构造整表读取语句。标识符无法作为绑定参数，这是本包**唯一**一处把
// 名字拼进 SQL 的地方，安全依据是三层：表名来自 `sqlite_master` 且已按迁移顺序白名单
// 校验（`sourceTables`），列名来自同一张表的 `PRAGMA table_xinfo`，两者都经 `quoteIdent`
// 双引号包裹并把内部双引号成对转义。
// Identifiers cannot be bound as parameters; this is the package's only SQL interpolation
// point, and it only ever sees names proven to belong to the frozen table set.
func selectAllQuery(table string, columns []columnInfo) string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdent(column.Name)
	}
	return "SELECT " + strings.Join(quoted, ", ") + " FROM " + quoteIdent(table)
}

// readTableRows 按列序读全表，逐值按**实际存储类**编码。
// 存储类与声明不符、NOT NULL 列出现 NULL 一律中止——**不**隐式 CAST、**不**补默认值。
func readTableRows(ctx context.Context, db *sql.DB, table string, columns []columnInfo) ([]Row, error) {
	rows, err := db.QueryContext(ctx, selectAllQuery(table, columns))
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 读表 %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Row
	for index := 0; rows.Next(); index++ {
		holders := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range holders {
			ptrs[i] = &holders[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("dbmigrate: 表 %q 第 %d 行: %w", table, index+1, err)
		}
		row := make(Row, len(columns))
		for i, column := range columns {
			value, err := encodeStoredValue(table, column, index+1, holders[i])
			if err != nil {
				return nil, err
			}
			row[column.Name] = value
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbmigrate: 遍历表 %q: %w", table, err)
	}
	return out, nil
}

// encodeStoredValue 把 `database/sql` 取回的值按实际存储类编码，并核对与声明类型是否相容。
func encodeStoredValue(table string, column columnInfo, rowNumber int, value any) (EncodedValue, error) {
	_, expected, err := mapColumnType(table, column.Name, column.Decl)
	if err != nil {
		return EncodedValue{}, fmt.Errorf("dbmigrate: 表 %q 的列 %q: %w", table, column.Name, err)
	}

	var encoded EncodedValue
	switch v := value.(type) {
	case nil:
		if column.NotNull {
			return EncodedValue{}, fmt.Errorf("%w: 表 %q 的列 %q 第 %d 行是 NULL，但列声明 NOT NULL",
				ErrInvalidValue, table, column.Name, rowNumber)
		}
		return Null(), nil
	case int64:
		encoded = Int(v)
	case float64:
		encoded = Real(v)
	case string:
		encoded = Text(v)
	case []byte:
		encoded = Bytes(v)
	case bool:
		if v {
			encoded = Int(1)
		} else {
			encoded = Int(0)
		}
	default:
		return EncodedValue{}, fmt.Errorf("%w: 表 %q 的列 %q 第 %d 行是 %T，无法无损编码",
			ErrInvalidValue, table, column.Name, rowNumber, value)
	}

	if encoded.Tag != expected {
		return EncodedValue{}, fmt.Errorf("%w: 表 %q 的列 %q 第 %d 行实际存储类 %q 与声明的 %q 不符——不隐式 CAST",
			ErrInvalidValue, table, column.Name, rowNumber, encoded.Tag, expected)
	}
	return encoded, nil
}

// quoteIdent 用双引号包住标识符（内部双引号成对转义）。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// newBatchID 生成迁移批次号（16 字节随机数的十六进制）。
func newBatchID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("dbmigrate: 生成批次号: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// fileSHA256 算 `root` 下 `name` 的原始字节 SHA-256。
func fileSHA256(root *os.Root, name string) (string, error) {
	file, err := root.Open(name)
	if err != nil {
		return "", fmt.Errorf("dbmigrate: 打开文件算摘要: %w", err)
	}
	defer func() { _ = file.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", fmt.Errorf("dbmigrate: 算文件摘要: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
