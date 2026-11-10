//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是 US-25C 追加迁移 `0013_agent_bundle_payloads.sql` 的**有库**用例（T004）：
// 在隔离 schema 上从 v12 前向迁到 v13，核对新表的列 / 可空性 / CHECK / 索引与
// `data-model.md` §1 逐项一致，并钉住「契约基线 `schema.pg.sql` 不改写、新结构只在
// 迁移 0013 中定义」（D-25C-02）。
// The database-backed acceptance for migration 0013: forward-migrate an isolated schema
// from v12 to v13 and read the resulting catalog back.

// migrationColumn 读一列的 `data_type` / `is_nullable` / `column_default`（限定当前
// 隔离 schema）。列不存在时返回 found=false，由调用方断言存在性。
// migrationColumn reads one column's catalog row from the current schema.
func migrationColumn(t *testing.T, ctx context.Context, conn *sql.Conn, table, column string) (dataType, nullable string, def sql.NullString, found bool) {
	t.Helper()
	err := conn.QueryRowContext(ctx, `
		SELECT data_type, is_nullable, column_default
		  FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name = $1
		   AND column_name = $2`, table, column).Scan(&dataType, &nullable, &def)
	if err == sql.ErrNoRows {
		return "", "", sql.NullString{}, false
	}
	require.NoError(t, err)
	return dataType, nullable, def, true
}

// migrationCheckDefs 读一张表上全部 CHECK 约束的定义（按当前 schema 过滤——同名表在
// 别的隔离 schema 里普遍存在，不过滤会读到别人的定义）。
// migrationCheckDefs returns every CHECK constraint definition on one table in the
// current schema.
func migrationCheckDefs(t *testing.T, ctx context.Context, conn *sql.Conn, table string) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		  FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		 WHERE n.nspname = current_schema()
		   AND t.relname = $1
		   AND c.contype = 'c'
		 ORDER BY c.conname`, table)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var defs []string
	for rows.Next() {
		var def string
		require.NoError(t, rows.Scan(&def))
		defs = append(defs, def)
	}
	require.NoError(t, rows.Err())
	return defs
}

// migrationIndexDef 读一条索引的定义；不存在返回 found=false（可选索引据此判"存在才核对"）。
// migrationIndexDef reads one index definition from the current schema.
func migrationIndexDef(t *testing.T, ctx context.Context, conn *sql.Conn, index string) (string, bool) {
	t.Helper()
	var def string
	err := conn.QueryRowContext(ctx, `
		SELECT indexdef
		  FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND indexname = $1`, index).Scan(&def)
	if err == sql.ErrNoRows {
		return "", false
	}
	require.NoError(t, err)
	return def, true
}

// TestMigration0013_Idempotent 判 v12 存量库前向迁到 v13 后：版本记录恰好一条、重复
// 执行是幂等 no-op，且 0013 脚本本身可重入（`IF NOT EXISTS` 纪律，不是"只第一次能过"）。
//
// 逆改点：0013 的 DDL 去掉 `IF NOT EXISTS` → 直接重执行脚本处红；把版本记录改成每次
// 都插入 → `count(version = 13)` 断言红；漏写脚本文件 → 前向迁移直接报错红。
// Reverse-edit points: dropping IF NOT EXISTS makes the direct re-exec fail; appending a
// version row per run breaks the count assertion; a missing script fails the forward step.
func TestMigration0013_Idempotent(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()

	// 存量库当前停在 v12：先迁到 12 再前向迁 13，走的是真实升级路径而不是空库全量建。
	// A deployed database sits at v12; migrate to 12 first, then forward to 13.
	require.NoError(t, ensureSchema(ctx, conn, 12))
	require.NoError(t, ensureSchema(ctx, conn, 13))
	require.NoError(t, ensureSchema(ctx, conn, 13), "重复执行必须是幂等 no-op")

	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 13, version)

	var records int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_version WHERE version = 13`).Scan(&records))
	require.Equal(t, 1, records, "重复迁移不得追加第二条 v13 版本记录")

	// 脚本自身幂等：运行器不会重跑已应用版本，但迁移纪律要求脚本可重入。直接再执行
	// 一遍 DDL，证明重复执行不报错、不重复建对象。
	// The runner never re-applies a version, but the scripts must stay re-runnable.
	script, description, err := migrationScript(13)
	require.NoError(t, err)
	require.Contains(t, description, "0013_agent_bundle_payloads", "v13 文件名必须是只追加的 0013 迁移")
	_, err = conn.ExecContext(ctx, script)
	require.NoError(t, err, "0013 脚本重复执行必须不报错、不重复建对象")

	records = 0
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_version WHERE version = 13`).Scan(&records))
	require.Equal(t, 1, records, "直接重执行脚本不写版本记录")
}

// TestMigration0013_Columns 判迁移 0013 的结构与 `data-model.md` §1 一致：新表
// `agent_bundle_payloads` 的八列、可空性、主键、索引；`worker_nodes` 的可空心跳间隔列
// 与其正数 CHECK（含写入行为）；`resource_pointers` 的键集索引；`task_executions` 的
// 可空钉住哈希列；时间列不设数据库 DEFAULT（毫秒值由 Go 写入）。
//
// 逆改点：把任一列改成可空 / NOT NULL、去掉 CHECK、去掉索引、给 created_at 加
// DEFAULT → 对应断言红；把 CHECK 放宽到允许 0 / 负数 → 写入行为断言红。
// Reverse-edit points: flipping nullability, dropping a CHECK or index, or adding a
// DEFAULT to created_at each turns a named assertion red.
func TestMigration0013_Columns(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, 13))

	// ① 新表 agent_bundle_payloads：列类型与可空性逐列核对。
	columns := []struct {
		name     string
		dataType string
		nullable string
	}{
		{"bundle_hash", "text", "NO"},
		{"agent", "text", "NO"},
		{"agent_uid", "text", "NO"},
		{"format_version", "integer", "NO"},
		{"payload", "bytea", "NO"},
		{"payload_sha256", "text", "NO"},
		{"size_bytes", "bigint", "NO"},
		{"created_at", "bigint", "NO"},
	}
	for _, column := range columns {
		dataType, nullable, _, found := migrationColumn(t, ctx, conn, "agent_bundle_payloads", column.name)
		require.True(t, found, "agent_bundle_payloads.%s 必须存在", column.name)
		require.Equal(t, column.dataType, dataType, "agent_bundle_payloads.%s 类型", column.name)
		require.Equal(t, column.nullable, nullable, "agent_bundle_payloads.%s 可空性", column.name)
	}

	// 内容寻址：bundle_hash 是主键（同哈希内容只存一份）。
	var pkDef string
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		  FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		 WHERE n.nspname = current_schema()
		   AND t.relname = 'agent_bundle_payloads'
		   AND c.contype = 'p'`).Scan(&pkDef))
	require.Contains(t, pkDef, "PRIMARY KEY (bundle_hash)", "载荷必须以 bundle_hash 为主键")

	// 时间列由 Go 写毫秒值——不得新增数据库 DEFAULT 先例。
	_, _, createdAtDefault, found := migrationColumn(t, ctx, conn, "agent_bundle_payloads", "created_at")
	require.True(t, found)
	require.False(t, createdAtDefault.Valid, "created_at 不得设数据库 DEFAULT（毫秒值由 Go 写入）")

	// 按 Agent 诊断的索引。
	indexDef, ok := migrationIndexDef(t, ctx, conn, "idx_agent_bundle_payloads_agent")
	require.True(t, ok, "agent_bundle_payloads 必须建 (agent, created_at DESC) 索引")
	require.Contains(t, indexDef, "(agent, created_at DESC)")

	// ② worker_nodes.heartbeat_interval_seconds：可空整数 + 正数 CHECK。
	dataType, nullable, _, found := migrationColumn(t, ctx, conn, "worker_nodes", "heartbeat_interval_seconds")
	require.True(t, found, "worker_nodes.heartbeat_interval_seconds 必须存在")
	require.Equal(t, "integer", dataType)
	require.Equal(t, "YES", nullable, "旧行缺值必须允许 NULL")

	checkDefs := migrationCheckDefs(t, ctx, conn, "worker_nodes")
	checkJoined := ""
	for _, def := range checkDefs {
		checkJoined += def + "\n"
	}
	require.Contains(t, checkJoined, "heartbeat_interval_seconds IS NULL", "CHECK 必须允许 NULL")
	require.Contains(t, checkJoined, "heartbeat_interval_seconds > 0", "CHECK 必须拒绝非正数")

	// 写入行为：NULL 与正整数可写；0 / 负数被 CHECK 拒绝（0 → NULL 是登记层的语义，
	// 不是列层的语义）。
	insertWorker := func(instanceID string, interval any) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
			                         started_at, last_heartbeat_at, heartbeat_interval_seconds)
			VALUES ($1, 'migration-worker', 'host-a', 'dev', 13, 1000, 1000, $2)`, instanceID, interval)
		return err
	}
	require.NoError(t, insertWorker("m13-null", nil), "迁移前的旧行形态（NULL）必须可写")
	require.NoError(t, insertWorker("m13-positive", 30), "正整数间隔必须可写")
	require.Error(t, insertWorker("m13-zero", 0), "0 必须被 CHECK 拒绝")
	require.Error(t, insertWorker("m13-negative", -1), "负数必须被 CHECK 拒绝")

	// ③ resource_pointers 的 (updated_at, resource_id) 键集增量索引。
	indexDef, ok = migrationIndexDef(t, ctx, conn, "idx_resource_pointers_kind_updated_id")
	require.True(t, ok, "resource_pointers 必须建键集增量索引")
	require.Contains(t, indexDef, "(resource_kind, updated_at, resource_id)",
		"索引必须是 (resource_kind, updated_at, resource_id)——(updated_at, resource_id) 键集按前三列过滤后严格前进")

	// ④ task_executions.pinned_bundle_hash：可空，NULL = 非钉住领取（走当前指针语义）。
	dataType, nullable, _, found = migrationColumn(t, ctx, conn, "task_executions", "pinned_bundle_hash")
	require.True(t, found, "task_executions.pinned_bundle_hash 必须存在")
	require.Equal(t, "text", dataType)
	require.Equal(t, "YES", nullable, "非钉住领取必须是 NULL，不回填历史行")

	// ⑤ 可选索引：data-model §1.2 标为可选（支撑非 NULL 段扫描）。存在才核对形状，
	// 缺席不算失败——是否建它由实现按扫描计划决定，判据只钉"建了就必须是对的"。
	// The scheduled-tasks scan index is optional; when present its shape is pinned.
	if def, ok := migrationIndexDef(t, ctx, conn, "idx_scheduled_tasks_next_fire_task"); ok {
		require.Contains(t, def, "(next_fire_at, task_id)")
		require.Contains(t, def, "WHERE (next_fire_at IS NOT NULL)", "扫描索引必须带 NULL 段排除谓词")
	}
}

// TestMigration0013_SchemaVersion13 判版本链推进到 13，并且 `docs/contracts/schema.pg.sql`
// 零 diff：契约基线仍与 `0001_postgres_v1.sql` 逐字节相等，新表 / 新列只出现在迁移
// 0013 中（D-25C-02：已发布契约与迁移只追加、不得回头改写）。
//
// 逆改点：SupportedSchemaVersion 留在 12 / 新建库停在 12 → 版本断言红；把 0013 的
// 新结构抄进 schema.pg.sql → 契约冻结断言红。
// Reverse-edit points: leaving SupportedSchemaVersion at 12 fails the version assertion;
// rewriting the v1 contract fails the freeze assertion.
func TestMigration0013_SchemaVersion13(t *testing.T) {
	require.Equal(t, 13, SupportedSchemaVersion, "US-25C 把 PostgreSQL 谱系推进到 v13")

	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion))

	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 13, version)

	var description string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT description FROM schema_version WHERE version = 13`).Scan(&description),
		"新建库的版本历史必须包含 v13 记录")
	require.Contains(t, description, "0013_agent_bundle_payloads")

	// 迁移后的库真的带上了新表（版本号与结构是同一件事）。
	var payloadTable sql.NullString
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT to_regclass('agent_bundle_payloads')::text`).Scan(&payloadTable))
	require.True(t, payloadTable.Valid, "v13 库必须存在 agent_bundle_payloads")

	// 契约冻结：schema.pg.sql 与 v1 基线逐字节相等，且不含 0013 的新结构。
	contract, err := os.ReadFile("../../../docs/contracts/schema.pg.sql")
	require.NoError(t, err, "契约文件 docs/contracts/schema.pg.sql 必须可读")
	baseline, err := os.ReadFile("migrations/0001_postgres_v1.sql")
	require.NoError(t, err)
	require.Equal(t, baseline, contract, "v1 契约基线必须保持冻结（迁移只追加，不改写 schema.pg.sql）")
	for _, literal := range []string{"agent_bundle_payloads", "pinned_bundle_hash", "heartbeat_interval_seconds"} {
		require.NotContains(t, string(contract), literal,
			"%s 只允许定义在迁移 0013 中，不得写进 v1 契约", literal)
	}
}
