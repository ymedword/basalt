package dbmigrate

// 本文件是 SQLite v6 dump 的**单事务导入**（C-12 / C-30 / 颗粒度文档 §2.11.2 导入段）：
// 先在事务外做一次预检（只为尽早失败，**不是**并发判据），再在**一个事务**里取
// `pg_advisory_xact_lock`、**锁内重新断言**（schema v1 / 业务表为空 / 无 cutover）、
// 按迁移拓扑序逐表插入（显式 ID、按清单列元数据转换）、非空表把 identity 序列推到
// max(id)。任一步失败整体回滚为空业务表，schema 保留。
// 导入端**不写** `first_write_at`——它归切换后的首个业务事务（图 2 [7]）。
// This file is the single-transaction importer: an early preflight, then one transaction
// that takes the advisory lock, re-asserts inside the lock, inserts in topology order and
// resets identity sequences. first_write_at is never written here.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/storage/postgres"
)

// 导入拒绝的哨兵：每一种都对应 §2.11.3 失败矩阵的一行，调用方按哨兵分流。
// Each sentinel maps to one row of the refusal matrix (§2.11.3).
var (
	// ErrTargetNotEmpty 表示目标库已有业务数据——导入只接受空库，**不**覆盖、**不**合并。
	ErrTargetNotEmpty = errors.New("dbmigrate: 目标库业务表非空，拒绝导入")
	// ErrTargetHasCutover 表示目标库已有 cutover 记录——切换过的库不得再被导入覆盖。
	ErrTargetHasCutover = errors.New("dbmigrate: 目标库已有 cutover 记录，拒绝导入")
	// ErrTargetSchemaMismatch 表示目标库 schema 版本不是本工具要求的 v1。
	ErrTargetSchemaMismatch = errors.New("dbmigrate: 目标库 schema 版本不是 v1")
	// ErrDigestMismatch 表示 dump 的 JSONL 与清单摘要不符（被改写 / 截断 / 换行）。
	// 文案里保留英文短语 "digest mismatch"：颗粒度文档 §4.4 R4 的用例按这个短语断言。
	ErrDigestMismatch = errors.New("dbmigrate: digest mismatch（dump 摘要与清单不符）")
)

const (
	// importLockSeed 是导入 advisory lock 的固定种子；键经 sha256 派生而不是硬编码
	// 魔术数字，派生方式与 `postgres.advisoryKey` 相同（前缀不同即不撞键）。
	// The import advisory key is derived from a fixed seed, never a magic literal.
	importLockSeed = "basalt/db-import"

	// importBatchRows 是一条 INSERT 携带的行数上限：多行 VALUES 摊薄往返，
	// 同时把参数个数（行数 × 列数）压在 PG 上限之内。
	// importBatchRows bounds one multi-row INSERT, keeping parameters under PG's limit.
	importBatchRows = 200
)

// importAdvisoryKey 是导入串行化的固定锁键（int64）。
// importAdvisoryKey is the fixed advisory lock key that serialises imports.
var importAdvisoryKey = advisoryKeyFrom(importLockSeed)

// advisoryKeyFrom 把种子派生为 int64：sha256 前 8 字节按大端读取。
// 与存储层 `advisoryKey` 同一派生方式；键只用于 `pg_advisory_xact_lock`，**不落库**。
//
// 分两段 32 位拼接而不是 `int64(binary.BigEndian.Uint64(...))`：后者是 uint64→int64 的
// 溢出转换（gosec G115，门禁零命中），而这里想要的正是"任意 8 字节的二进制解释"——
// 拼接得到的位型与整体转换完全一致，同时不用抑制注释（与 `postgres.advisoryKey` 同法）。
// Two 32-bit halves keep the exact bit pattern without a wrapping conversion.
func advisoryKeyFrom(seed string) int64 {
	sum := sha256.Sum256([]byte(seed))
	hi := int64(binary.BigEndian.Uint32(sum[:4]))
	lo := int64(binary.BigEndian.Uint32(sum[4:8]))
	return hi<<32 | lo
}

// Importer 把一份 dump 导入**空的** PostgreSQL v1 目标库。
// Importer loads one dump into an empty PostgreSQL v1 target.
type Importer struct {
	db       *postgres.DB
	dir      string
	manifest Manifest
}

// NewImporter 绑定目标句柄、dump 目录与清单；构造时不碰数据库。
// 清单是列元数据的**唯一**来源——目标表的 `information_schema` 不参与解码。
// NewImporter binds the target, the dump directory and the manifest; construction never
// touches the database, and the manifest is the only column-metadata source.
func NewImporter(db *postgres.DB, dir string, manifest Manifest) *Importer {
	return &Importer{db: db, dir: dir, manifest: manifest}
}

// Import 执行导入：清单自检 → 重算摘要 → 事务外预检 → 单事务（锁内断言 + 插入 + 序列）。
// 失败时报告里的 `Failures` 留表名与原因；`Rows` 只在**提交确认**后填写。
// Import runs manifest validation, digest re-verification, the preflight and the single
// transaction; row counts are only reported once the commit is confirmed.
func (im *Importer) Import(ctx context.Context) (report ImportReport, err error) {
	started := time.Now()
	report = ImportReport{Rows: map[string]int64{}, Failures: map[string]string{}}
	defer func() { report.Duration = time.Since(started) }()

	if err := im.manifest.Validate(); err != nil {
		return report, err
	}
	tables, err := im.plan()
	if err != nil {
		return report, err
	}
	if err := im.verifyDigests(tables); err != nil {
		return report, err
	}
	// 预检只为尽早失败；并发安全判据是锁内的那次重新断言（§2.11.2）。
	if err := im.preflight(ctx); err != nil {
		return report, err
	}

	rows := make(map[string]int64, len(tables))
	outcome, err := im.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, importAdvisoryKey); err != nil {
			return fmt.Errorf("dbmigrate: 取导入 advisory 锁: %w", err)
		}
		if err := assertImportTarget(ctx, tx); err != nil {
			return err
		}
		for _, table := range tables {
			loaded, err := im.importTable(ctx, tx, table)
			if err != nil {
				return err
			}
			rows[table.Name] = loaded
		}
		return nil
	})
	if err != nil {
		// 报告要如实回答"这次导入失败了吗"：失败原因记在 import 名下，
		// 行数只在**提交确认**后才填（见下方），不留"看着像导了一半"的数字。
		report.Failures["import"] = err.Error()
		return report, err
	}
	if outcome != postgres.TxCommitted {
		return report, fmt.Errorf("dbmigrate: 导入事务未确认提交（结局 %s）", outcome)
	}
	for name, count := range rows {
		report.Rows[name] = count
	}
	return report, nil
}

// plan 把清单里的表按 `migrationOrder` 排定导入次序，并做三项清单自检：
// 表必须在迁移顺序里、列必须来自已知的目标类型、生成列**不得**出现在 columns[]。
// 清单缺某张表时按"该表不导入"处理——目标库的业务表空表断言覆盖全部 32 张。
// plan orders the manifest's tables by the frozen topology and runs three checks whose
// failure means the manifest itself is not trustworthy.
func (im *Importer) plan() ([]TableManifest, error) {
	position := make(map[string]int, len(migrationOrder))
	for i, name := range migrationOrder {
		position[name] = i
	}
	ordered := make([]TableManifest, 0, len(im.manifest.Tables))
	for _, table := range im.manifest.Tables {
		if _, ok := position[table.Name]; !ok {
			return nil, fmt.Errorf("%w: 清单里的表 %q 不在迁移顺序里——先补拓扑序",
				ErrInvalidValue, table.Name)
		}
		if err := checkManifestColumns(table); err != nil {
			return nil, err
		}
		ordered = append(ordered, table)
	}
	sortTablesByTopology(ordered, position)
	return ordered, nil
}

// checkManifestColumns 判一张表的列元数据能否直接用于 INSERT：目标类型必须认识、
// 生成列**不得**出现、PG 新增列的补值列**不得**与源列重名（重名即两份来源打架）。
func checkManifestColumns(table TableManifest) error {
	for _, column := range table.Columns {
		if !knownPGTypes[column.PGType] {
			return fmt.Errorf("%w: 表 %q 的列 %q 目标类型 %q 不认识",
				ErrInvalidValue, table.Name, column.Name, column.PGType)
		}
		if generatedColumns[table.Name][column.Name] {
			return fmt.Errorf("%w: 表 %q 的列 %q 是生成列，由 PG 重算，不得导入",
				ErrInvalidValue, table.Name, column.Name)
		}
	}
	for _, fill := range pgOnlyColumns[table.Name] {
		for _, column := range table.Columns {
			if column.Name == fill.column {
				return fmt.Errorf("%w: 表 %q 的列 %q 是 PG 新增列，清单不得声称来自源库",
					ErrInvalidValue, table.Name, column.Name)
			}
		}
	}
	return nil
}

// knownPGTypes 是清单 `pg_type` 允许的字面量集合（与 `decltype.go` 的常量同源）。
// knownPGTypes is the closed set of target types a manifest may name.
var knownPGTypes = map[string]bool{
	pgTypeBigInt:  true,
	pgTypeDouble:  true,
	pgTypeText:    true,
	pgTypeBytea:   true,
	pgTypeNumeric: true,
	pgTypeBoolean: true,
	pgTypeJSONB:   true,
}

// generatedColumns 是 PG 侧由表达式重算的列：**不得**出现在清单里、**不得**被赋值。
// `kb_chunks` 不参与迁移，列出它是为把"生成列不导入"的口径写全。
// generatedColumns must never be imported; PostgreSQL recomputes them.
var generatedColumns = map[string]map[string]bool{
	"messages":  {"tsv": true},
	"kb_chunks": {"tsv": true},
}

// sourcePrimaryKeys 是每张源表的主键列序（与两份契约的 PRIMARY KEY 声明逐字一致）。
// 导入端用它重算 `IDSetSHA256`——清单不存主键列，这是**唯一**的来源；
// `TestSourcePrimaryKeys_MatchContracts` 机器判卷，契约改而本表没跟上即红。
// The importer needs the key order to re-derive IDSetSHA256; a test pins this table to
// both frozen contracts. (名字与 export.go 的 `primaryKeyColumns` 派生函数区分开。)
var sourcePrimaryKeys = map[string][]string{
	"sessions":                     {"session_id"},
	"messages":                     {"id"},
	"runs":                         {"run_id"},
	"config_snapshots":             {"id"},
	"llm_calls":                    {"id"},
	"tool_invocations":             {"id"},
	"scheduled_tasks":              {"task_id"},
	"task_executions":              {"id"},
	"schedule_deliveries":          {"run_id"},
	"resource_identities":          {"resource_uid"},
	"agent_resource_bindings":      {"agent_uid", "resource_kind", "ref_name"},
	"notify_channels":              {"resource_uid"},
	"resource_connection_tests":    {"resource_kind", "resource_uid"},
	"network_domain_policy":        {"policy_scope"},
	"execution_policy":             {"policy_scope"},
	"network_exceptions":           {"exception_id"},
	"instance_secrets":             {"scope"},
	"mcp_credential_sources":       {"server_uid", "binding_key"},
	"memory_entries":               {"id"},
	"management_operations":        {"operation_id"},
	"resource_versions":            {"id"},
	"resource_pointers":            {"resource_kind", "resource_id"},
	"credentials":                  {"credential_id"},
	"credential_versions":          {"credential_id", "version_no"},
	"resource_version_credentials": {"version_id", "credential_id"},
	"run_resource_manifests":       {"run_id"},
	"run_resource_refs":            {"run_id", "resource_kind", "resource_id"},
	"run_credential_refs":          {"run_id", "credential_id", "resource_kind", "resource_uid"},
	"run_notify_refs":              {"run_id", "agent_uid", "channel_uid"},
	"notify_deliveries":            {"tool_invocation_id"},
	"agent_lifecycle":              {"agent_name"},
	"agent_archives":               {"archive_id"},
}

// columnFill 是一条 PG 新增列的定值规则；值可以由源行推出（如 `runs.actor` 按 channel）。
// columnFill pins one target-only column; the value may depend on the source row.
type columnFill struct {
	column string
	value  func(Row) (any, error)
}

// constantFill 是"常量定值"的简写。
func constantFill(column string, value any) columnFill {
	return columnFill{column: column, value: func(Row) (any, error) { return value, nil }}
}

// pgOnlyColumns 是 PG 侧新增、SQLite 源没有的列，按 §3.4.3 逐列定值。
// 置空与错填都会撞 CHECK 约束或把旧 Run 的执行者标错，因此这里**不**用默认值兜底。
// pgOnlyColumns fills the target-only columns exactly as §3.4.3 prescribes.
var pgOnlyColumns = map[string][]columnFill{
	"runs": {
		constantFill("caller_id", "local-admin"),
		constantFill("authorizing_kind", "admin-builtin"),
		constantFill("key_id", nil),
		constantFill("reservation_id", nil),
		{column: "actor", value: runActor},
	},
	"scheduled_tasks": {
		constantFill("caller_id", "local-admin"),
		constantFill("authorizing_kind", "admin-builtin"),
		constantFill("authorizing_key_id", nil),
	},
	"memory_entries": {
		// 旧行一律"已生效的导入记忆"：state=active、source=legacy_import（§3.4.3）；
		// valid_from 取 created_at（与 MemoryStore 的 active 路径同一口径），
		// content_hash 是正文的 sha256（与写入路径同一算法）。
		constantFill("state", "active"),
		constantFill("source", "legacy_import"),
		{column: "valid_from", value: func(row Row) (any, error) { return rowInt64(row, "created_at") }},
		{column: "content_hash", value: memoryContentHash},
	},
}

// runActorByChannel 是 §3.4.3 的存量 Run 执行者映射：SQLite 期没有调用方 Key，
// 授权来源统一 `admin-builtin`，但执行者必须保留调度 / 管理入口的来源。
var runActorByChannel = map[string]string{
	"scheduler": "scheduler",
	"admin":     "admin-cli",
	"cli":       "admin-cli",
	"web":       "caller",
}

// runActor 按源行的 channel 定 `runs.actor`；认不出的 channel 一律失败（**不**猜）。
func runActor(row Row) (any, error) {
	channel, err := rowText(row, "channel")
	if err != nil {
		return nil, err
	}
	actor, ok := runActorByChannel[channel]
	if !ok {
		return nil, fmt.Errorf("%w: runs.channel=%q 没有对应的执行者映射", ErrInvalidValue, channel)
	}
	return actor, nil
}

// memoryContentHash 是记忆正文的 sha256（64 位小写十六进制），与 `memory_entries.content_hash`
// 及写入路径的算法一致——迁移行与运行期写入的行必须同形。
func memoryContentHash(row Row) (any, error) {
	content, err := rowText(row, "content")
	if err != nil {
		return nil, err
	}
	return sha256Hex([]byte(content)), nil
}

// rowText 取源行的文本列（标签必须是 t，**不**替调用方做类型转换）。
func rowText(row Row, column string) (string, error) {
	value, ok := row[column]
	if !ok || value.IsNull() || value.Tag != TagText {
		return "", fmt.Errorf("%w: 源行缺少可用的文本列 %q", ErrInvalidValue, column)
	}
	return string(value.Raw), nil
}

// rowInt64 取源行的整数列。
func rowInt64(row Row, column string) (any, error) {
	value, ok := row[column]
	if !ok || value.IsNull() || value.Tag != TagInteger {
		return nil, fmt.Errorf("%w: 源行缺少可用的整数列 %q", ErrInvalidValue, column)
	}
	parsed, err := strconv.ParseInt(string(value.Raw), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: 列 %q 的整数字面量 %q 无法解析", ErrInvalidValue, column, value.Raw)
	}
	return parsed, nil
}

// verifyDigests 重算每张表的文件 / 行 / ID 集合摘要并与清单比对——防篡改步骤，
// 任何一项不符都整体拒绝，**不**"先导进去再说"。
func (im *Importer) verifyDigests(tables []TableManifest) error {
	root, err := os.OpenRoot(im.dir)
	if err != nil {
		return fmt.Errorf("dbmigrate: 打开 dump 目录: %w", err)
	}
	defer func() { _ = root.Close() }()

	for _, table := range tables {
		name := table.Name + ".jsonl"
		fileSHA, err := fileSHA256(root, name)
		if err != nil {
			return fmt.Errorf("dbmigrate: 表 %q: %w", table.Name, err)
		}
		if fileSHA != table.JSONLSHA256 {
			return fmt.Errorf("%w: 表 %q 的文件摘要 %s，清单声明 %s",
				ErrDigestMismatch, table.Name, fileSHA, table.JSONLSHA256)
		}
		columns := table.ColumnNames()
		rows, err := readJSONL(root, name, columns)
		if err != nil {
			return fmt.Errorf("dbmigrate: 表 %q: %w", table.Name, err)
		}
		if int64(len(rows)) != table.RowCount {
			return fmt.Errorf("%w: 表 %q 实际 %d 行，清单声明 %d 行",
				ErrDigestMismatch, table.Name, len(rows), table.RowCount)
		}
		digest := DigestRows(rows)
		if digest.Err != nil {
			return fmt.Errorf("dbmigrate: 表 %q 的行摘要: %w", table.Name, digest.Err)
		}
		if digest.RowSHA256 != table.RowSHA256 {
			return fmt.Errorf("%w: 表 %q 的行摘要 %s，清单声明 %s",
				ErrDigestMismatch, table.Name, digest.RowSHA256, table.RowSHA256)
		}
		idSet, err := DigestIDSet(sourcePrimaryKeys[table.Name], rows)
		if err != nil {
			return fmt.Errorf("dbmigrate: 表 %q 的 ID 集合摘要: %w", table.Name, err)
		}
		if idSet != table.IDSetSHA256 {
			return fmt.Errorf("%w: 表 %q 的 ID 集合摘要 %s，清单声明 %s",
				ErrDigestMismatch, table.Name, idSet, table.IDSetSHA256)
		}
	}
	return nil
}

// preflight 在事务外做一次目标检查：schema v1、业务表为空、无 cutover。
// 结论只用于尽早失败——真正的判据必须在锁内重新断言。
func (im *Importer) preflight(ctx context.Context) error {
	version, err := im.db.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("dbmigrate: 读目标库 schema 版本: %w", err)
	}
	return assertTarget(ctx, im.db, version)
}

// assertImportTarget 是锁内的重新断言：锁内看到的才是并发安全的结论。
func assertImportTarget(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("dbmigrate: 读目标库 schema 版本: %w", err)
	}
	return assertTarget(ctx, tx, version)
}

// assertTarget 判三件事：schema 版本是 v1、没有 cutover、32 张业务表全空。
// 业务表清单取 `migrationOrder`——不在其中的表（PG 新增表）不属导入面。
func assertTarget(ctx context.Context, q postgres.Queryer, version int) error {
	if version != postgres.SupportedSchemaVersion {
		return fmt.Errorf("%w: 实际 v%d，需要 v%d（先跑 db migrate）",
			ErrTargetSchemaMismatch, version, postgres.SupportedSchemaVersion)
	}
	var hasCutover bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM storage_cutover)`).Scan(&hasCutover); err != nil {
		return fmt.Errorf("dbmigrate: 查 cutover: %w", err)
	}
	if hasCutover {
		return ErrTargetHasCutover
	}
	for _, table := range migrationOrder {
		var nonEmpty bool
		if err := q.QueryRowContext(ctx, emptinessQuery(table)).Scan(&nonEmpty); err != nil {
			return fmt.Errorf("dbmigrate: 查表 %q 是否为空: %w", table, err)
		}
		if nonEmpty {
			return fmt.Errorf("%w: 表 %q 已有数据", ErrTargetNotEmpty, table)
		}
	}
	return nil
}

// emptinessQuery 构造"表非空即真"的探测语句。表名来自 `migrationOrder` 白名单，
// 经 `quoteIdent` 加引号——与 `selectAllQuery` 同一安全依据。
func emptinessQuery(table string) string {
	return `SELECT EXISTS (SELECT 1 FROM ` + quoteIdent(table) + `)`
}

// importTable 在一张表上插入全部行并把 identity 序列推到该列的当前 max。
func (im *Importer) importTable(ctx context.Context, tx *sql.Tx, table TableManifest) (int64, error) {
	rows, err := readTableDump(im.dir, table)
	if err != nil {
		return 0, err
	}
	if err := insertRows(ctx, tx, table, rows); err != nil {
		return 0, fmt.Errorf("dbmigrate: 表 %q: %w", table.Name, err)
	}
	if err := resetIdentity(ctx, tx, table.Name, len(rows)); err != nil {
		return 0, fmt.Errorf("dbmigrate: 表 %q 的序列: %w", table.Name, err)
	}
	return int64(len(rows)), nil
}

// readTableDump 读一张表的 JSONL（列序以清单为准，缺列 / 多列都拒绝）。
func readTableDump(dir string, table TableManifest) ([]Row, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 打开 dump 目录: %w", err)
	}
	defer func() { _ = root.Close() }()
	return readJSONL(root, table.Name+".jsonl", table.ColumnNames())
}

// insertRows 按 `importBatchRows` 分批写多行 VALUES：参数逐列绑定，
// 只有标识符经 `quoteIdent` 拼进去（表名 / 列名都来自清单与白名单）。
func insertRows(ctx context.Context, tx *sql.Tx, table TableManifest, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	fills := pgOnlyColumns[table.Name]
	columns := make([]string, 0, len(table.Columns)+len(fills))
	for _, column := range table.Columns {
		columns = append(columns, column.Name)
	}
	for _, fill := range fills {
		columns = append(columns, fill.column)
	}

	for start := 0; start < len(rows); start += importBatchRows {
		end := min(start+importBatchRows, len(rows))
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*len(columns))
		for _, row := range batch {
			for _, column := range table.Columns {
				value, ok := row[column.Name]
				if !ok {
					return fmt.Errorf("%w: 缺列 %q", ErrInvalidValue, column.Name)
				}
				arg, err := valueArg(column, value)
				if err != nil {
					return err
				}
				args = append(args, arg)
			}
			for _, fill := range fills {
				arg, err := fill.value(row)
				if err != nil {
					return err
				}
				args = append(args, arg)
			}
		}
		query := insertQuery(table.Name, columns, len(batch))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("插入第 %d–%d 行: %w", start+1, end, err)
		}
	}
	return nil
}

// insertQuery 构造一条多行 INSERT 语句：`INSERT INTO "t" ("c1", …) VALUES ($1, …), …`。
// 标识符无法作为绑定参数，这是导入侧**唯一**一处把名字拼进 SQL 的地方（与导出侧的
// `selectAllQuery` 同形态），安全依据是两层：表名来自 `migrationOrder` 白名单；列名来自
// 清单，经 `quoteIdent` 双引号包裹、内部双引号成对转义后只能作为**一个**标识符出现，
// 无法逃逸引号（清单里的列名与目标表不符时由 PG 直接报错，fail-closed）。值一律走 `$n`
// 绑定。拼接收在一个构造器里，评审只看这一处。
// Identifiers cannot be bound; this constructor is the importer's only interpolation point,
// every name it sees is a frozen table name or an escaped (quoted) manifest column, and all
// values are bound as $n.
func insertQuery(table string, columns []string, rowCount int) string {
	return insertPrefix(table, columns) + valueGroups(rowCount, len(columns))
}

// insertPrefix 构造 `INSERT INTO "t" ("c1", "c2") VALUES `。
func insertPrefix(table string, columns []string) string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdent(column)
	}
	return "INSERT INTO " + quoteIdent(table) + " (" + strings.Join(quoted, ", ") + ") VALUES "
}

// valueGroups 构造 `($1, $2), ($3, $4), …` 的占位符序列。
func valueGroups(rows, columns int) string {
	var builder strings.Builder
	param := 1
	for row := 0; row < rows; row++ {
		if row > 0 {
			builder.WriteString(", ")
		}
		builder.WriteByte('(')
		for column := 0; column < columns; column++ {
			if column > 0 {
				builder.WriteString(", ")
			}
			builder.WriteByte('$')
			builder.WriteString(strconv.Itoa(param))
			param++
		}
		builder.WriteByte(')')
	}
	return builder.String()
}

// valueArg 把一个编码值转成驱动参数：**先**核对标签与清单 `encoding` 一致，
// 再按目标类型转换——boolean 只接受 0 / 1（其余拒绝，**不**隐式 CAST）。
func valueArg(column ColumnMeta, value EncodedValue) (any, error) {
	if value.IsNull() {
		return nil, nil
	}
	if value.Tag != column.Encoding {
		return nil, fmt.Errorf("%w: 列 %q 的值标签 %q 与清单 encoding %q 不符",
			ErrInvalidValue, column.Name, value.Tag, column.Encoding)
	}
	switch value.Tag {
	case TagInteger:
		literal := string(value.Raw)
		if column.PGType == pgTypeBoolean {
			switch literal {
			case "0":
				return false, nil
			case "1":
				return true, nil
			default:
				return nil, fmt.Errorf("%w: 列 %q 的 boolean 只接受 0 / 1，实际 %q",
					ErrInvalidValue, column.Name, literal)
			}
		}
		parsed, err := strconv.ParseInt(literal, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: 列 %q 的整数字面量 %q 无法解析",
				ErrInvalidValue, column.Name, literal)
		}
		return parsed, nil
	case TagReal:
		bits, err := strconv.ParseUint(string(value.Raw), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: 列 %q 的浮点位模式 %q 无法解析",
				ErrInvalidValue, column.Name, value.Raw)
		}
		return math.Float64frombits(bits), nil
	case TagText:
		// text / numeric / jsonb 都按文本参数发送，由 PG 按目标列类型解析。
		return string(value.Raw), nil
	case TagBytes:
		decoded, err := base64.StdEncoding.DecodeString(string(value.Raw))
		if err != nil {
			return nil, fmt.Errorf("%w: 列 %q 的 base64 字面量无法解码",
				ErrInvalidValue, column.Name)
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("%w: 列 %q 的标签 %q 未知", ErrInvalidValue, column.Name, value.Tag)
	}
}

// identityColumnsQuery 列出表上由 identity 生成的列（`GENERATED ... AS IDENTITY`）。
// 必须查目录、**不**能按列名猜：`management_operations` 的主键是 `operation_id text`、
// 根本没有 `id` 列，而 `pg_get_serial_sequence` 对不存在的列是**报错**（42703）而不是
// 返回 NULL——按名字写死会让整笔导入在提交前失败。
const identityColumnsQuery = `
SELECT a.attname
FROM pg_attribute a
WHERE a.attrelid = $1::regclass
  AND a.attidentity <> ''
  AND a.attnum > 0
  AND NOT a.attisdropped
ORDER BY a.attnum`

// resetIdentity 把每张表的 identity 序列推到该列的当前 max。空表**跳过**（`max` 为 NULL，
// 不传进 `setval`）；没有 identity 列的表同样跳过。
// 序列前进不是事务性操作，失败重试留下空洞是**允许**的——**不**回拨、**不**改写已插入行。
// resetIdentity only advances sequences for non-empty identity tables; gaps are allowed.
func resetIdentity(ctx context.Context, tx *sql.Tx, table string, rowCount int) error {
	if rowCount == 0 {
		return nil
	}
	columns, err := identityColumns(ctx, tx, table)
	if err != nil {
		return err
	}
	for _, column := range columns {
		var sequence sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT pg_get_serial_sequence($1, $2)`, table, column).Scan(&sequence); err != nil {
			return fmt.Errorf("查列 %q 的 identity 序列: %w", column, err)
		}
		if !sequence.Valid {
			continue
		}
		var maxValue sql.NullInt64
		if err := tx.QueryRowContext(ctx, maxColumnQuery(table, column)).Scan(&maxValue); err != nil {
			return fmt.Errorf("查 max(%s): %w", column, err)
		}
		if !maxValue.Valid {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`SELECT setval($1::regclass, $2, true)`, sequence.String, maxValue.Int64); err != nil {
			return fmt.Errorf("重置序列 %s: %w", sequence.String, err)
		}
	}
	return nil
}

// identityColumns 列出表上的 identity 列名（物理列序）。`defer` 在函数内关闭结果集——
// 事务里只有一条连接，带着未关闭的 rows 再发查询会把连接占死。
func identityColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, identityColumnsQuery, table)
	if err != nil {
		return nil, fmt.Errorf("查 identity 列: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读 identity 列: %w", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 identity 列: %w", err)
	}
	return columns, nil
}

// maxColumnQuery 构造 `SELECT max("col") FROM "t"`；表名来自 `migrationOrder` 白名单，
// 列名来自 `pg_attribute`，两者都经 `quoteIdent` 包裹。
func maxColumnQuery(table, column string) string {
	return `SELECT max(` + quoteIdent(column) + `) FROM ` + quoteIdent(table)
}

// sortTablesByTopology 按 `position` 给出的拓扑序原地排序（稳定的插入排序，
// 表数只有 32 张，不引入额外依赖）。
func sortTablesByTopology(tables []TableManifest, position map[string]int) {
	for i := 1; i < len(tables); i++ {
		for j := i; j > 0 && position[tables[j].Name] < position[tables[j-1].Name]; j-- {
			tables[j], tables[j-1] = tables[j-1], tables[j]
		}
	}
}
