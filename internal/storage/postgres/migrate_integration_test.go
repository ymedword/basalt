//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是迁移的**有库**用例：空库全量建 v1 与 advisory 锁串行。每个用例在测试库的
// 独立 schema 内建表，`search_path` 额外挂上扩展 schema——DDL 里的 `vector(n)` 与
// `gin_trgm_ops` 都按 search_path 解析。
// The database-backed half of the migration gate: fresh v1 and advisory-lock
// serialization, each inside an isolated schema.

// migrationConn 返回钉在独立 schema 上、且 `search_path` 含扩展 schema 的连接。
// migrationConn pins the connection to a fresh schema whose search_path also contains
// the extension schemas the DDL references.
func migrationConn(t *testing.T) *sql.Conn {
	t.Helper()
	conn, schema := isolatedSchemaConn(t)

	paths := []string{schema}
	seen := map[string]bool{schema: true}
	for _, ext := range []string{"vector", "pg_trgm"} {
		extSchema := extensionSchema(t, conn, ext)
		if seen[extSchema] {
			continue
		}
		seen[extSchema] = true
		paths = append(paths, extSchema)
	}
	_, err := conn.ExecContext(context.Background(), "SET search_path TO "+strings.Join(paths, ", "))
	require.NoError(t, err)

	// 穿透守卫：扩展装在 public 里，public 必须留在 search_path 上供 `vector(n)` /
	// `gin_trgm_ops` 解析；但路径上**不得**有第二个含 Basalt 表的 schema。否则迁移的
	// `to_regclass('schema_version')` 会穿透读到它，`ensureSchema` 判定"已是目标版本"
	// 直接 no-op——本 schema 零表却返回 nil，版本断言照样绿（2026-10-28 全量 -race
	// 套件实测：`require.Len(tables, 52)` 才把它抓出来）。
	// The guard refuses a second Basalt-bearing schema on search_path: migration would
	// read its schema_version through search_path and no-op while this schema stays empty.
	ctx := context.Background()
	var current string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&current))
	require.Equal(t, schema, current, "迁移目标 schema 必须是 search_path 的第一项")
	for _, name := range []string{"schema_version", "callers"} {
		var shadow sql.NullString
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, name).Scan(&shadow))
		require.False(t, shadow.Valid,
			"search_path 已能解析到 %s 表（%s）——迁移会在别的 schema 上空转", name, shadow.String)
	}
	return conn
}

// TestPostgres_FreshSchemaIsV1 判空库从 v1 基线起建、一次建到**当前支持版本**：版本记录
// 逐条写 `schema_version`（v1 → 各自的内嵌脚本），契约里的表全部就位；重跑是幂等
// no-op，不追加版本记录。
//
// 用例名由 `harness/stages/us16.json` 的 `required_tests` 固定为 V1——它判的仍是
// "空库从 v1 基线建起"，只是 D-19 把链延伸到 v2、R59 的补列修复延伸到 v3；改名会让
// 既定判据失联。版本断言取常量而非字面量，新增迁移时随链前移。
// 逆改点：把 target 写死成 0 / 把版本记录 INSERT 去掉 → 版本断言红；去掉 DDL 的
// `ON CONFLICT DO NOTHING` 或重跑时不判 current → 版本记录数断言红。
func TestPostgres_FreshSchemaIsV1(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()

	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion), "空库必须建到当前支持版本")

	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version, "PG 谱系自 1 起，不得沿用 SQLite 的 6")

	// 版本链逐条可溯源：每一版一条记录、描述指向该版的内嵌脚本。
	// 逆改点：把 0002 的描述写错 / 跳版 → 本断言红。
	wantDescription := map[int]string{
		1:  "migrations/0001_postgres_v1.sql",
		2:  "migrations/0002_agent_binding_knowledge_kind.sql",
		3:  "migrations/0003_knowledge_source_text_repair.sql",
		4:  "migrations/0004_knowledge_version_revision.sql",
		5:  "migrations/0005_knowledge_collection_description.sql",
		6:  "migrations/0006_observability.sql",
		7:  "migrations/0007_history_retention.sql",
		8:  "migrations/0008_flow.sql",
		9:  "migrations/0009_sandbox_execution.sql",
		10: "migrations/0010_skill_admission.sql",
	}
	rows, err := conn.QueryContext(ctx, `SELECT version, description FROM schema_version ORDER BY version`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	seen := map[int]string{}
	for rows.Next() {
		var v int
		var d string
		require.NoError(t, rows.Scan(&v, &d))
		seen[v] = d
	}
	require.NoError(t, rows.Err())
	require.Equal(t, wantDescription, seen, "版本链必须逐版可溯源到内嵌脚本")

	tables, err := tableNames(ctx, conn)
	require.NoError(t, err)
	require.Len(t, tables, 82, "原77表、0009 的隔离执行账本与 0010 的准入四表必须全部存在")
	for _, name := range []string{
		"callers", "sessions", "messages", "runs", "kb_chunks", "kb_chunk_embeddings",
		"compaction_entries", "context_assembly_records", "diagnostic_events", "storage_cutover",
		"history_retention_runs", "history_retention_rollups",
		"sandbox_executions",
		"skill_admission_candidates", "skill_admission_evaluations",
		"skill_admission_decisions", "skill_admission_publications",
		"flow_definitions", "flow_pointers", "flow_tasks", "flow_task_contracts", "flow_task_revisions", "flow_transitions", "flow_node_attempts", "flow_actions", "flow_action_attempts", "flow_evidence_refs", "flow_claims", "flow_claim_evidence", "flow_claim_dependencies", "flow_artifacts", "flow_artifact_claims", "flow_verifications", "flow_budget_allocations",
	} {
		require.Contains(t, tables, name)
	}

	// 内置 `local-admin` 调用方行必须随建库就位（`runs` / `sessions` /
	// `scheduled_tasks` 的外键指向 `callers`，缺行时内置管理员的每一次写入都会被
	// 外键拒绝）。逆改点：删掉 `seedBuiltinCaller` 调用 → 本断言红（0 行）。
	var kind, name, status string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT kind, name, status FROM callers WHERE caller_id = 'local-admin'`).
		Scan(&kind, &name, &status))
	require.Equal(t, "builtin", kind)
	require.Equal(t, "local-admin", name)
	require.Equal(t, "active", status)

	// 索引代际与基线操作都**不**由迁移器写（契约尾部第 3、4 条）：建库后两表必须为零行。
	// 首代际归 `serve` 启动装配的 `GenerationManager.Ensure`（embedding 配置可用时），基线行
	// 归同一启动流程的 `runBaseline`（无活可做时不写行）。逆改点：把任一动作塞进
	// `ensureSchema` → 对应断言红（1 行）。
	var generations, operations int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(*) FROM kb_index_generations`).Scan(&generations))
	require.Zero(t, generations, "迁移器不写索引代际——零行是建库后的正常读数")
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(*) FROM management_operations`).Scan(&operations))
	require.Zero(t, operations, "迁移器不写管理操作行")

	// 幂等：再跑一次既不报错也不重复记录版本。
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion))
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM schema_version`).Scan(&count))
	require.Equal(t, len(wantDescription), count, "版本记录只追加一次")

	// 种子**只补缺失行、不覆盖既有行**：停用是管理决定，重启不得把它悄悄复活。
	// 逆改点：把 `ON CONFLICT (caller_id) DO NOTHING` 改成 DO UPDATE → 本断言红。
	_, err = conn.ExecContext(ctx, `UPDATE callers SET status = 'disabled' WHERE caller_id = 'local-admin'`)
	require.NoError(t, err)
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion))
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT status FROM callers WHERE caller_id = 'local-admin'`).Scan(&status))
	require.Equal(t, "disabled", status, "既有行（含管理决定）不得被启动覆盖")
}

// bindingInsertSQL 是绑定投影的一行插入（v1 与 v2 的闭集对照用）：`resource_kind`
// 由参数给出，其余列取满足 NOT NULL / 主键的固定值——本表无外键，不依赖其他表。
// bindingInsertSQL inserts one projection row with the kind parameterized.
const bindingInsertSQL = `INSERT INTO agent_resource_bindings
	(agent_uid, agent_name, resource_kind, ref_name, resource_uid, content_hash, projected_at)
	VALUES ('au_0000000000000000000000d19', 'writer', $1, 'team', 'col_00000000000000000000000000', 'h', 0)`

// TestPostgres_UpgradeV1ToV2WidensBindingKinds 判 1 → 2 的**实际升级**（D-19）：
// 只建到 v1 时闭集拒绝 `knowledge_collection`（旧 CHECK 生效）；迁到当前支持版本后
// 同一行可写入，且版本记录停在 2。两步之间同一条 INSERT 先红后绿，证明加宽来自迁移
// 本身而不是"库天生就宽"。
// 逆改点：删掉 0002 的 ADD CONSTRAINT（或把新值从闭集里去掉）→ 迁移后的 INSERT 仍报
// `agent_resource_bindings_resource_kind_check`，本用例红。
func TestPostgres_UpgradeV1ToV2WidensBindingKinds(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()

	require.NoError(t, ensureSchema(ctx, conn, 1), "先只建到 v1 基线")
	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 1, version)

	_, err = conn.ExecContext(ctx, bindingInsertSQL, "knowledge_collection")
	require.Error(t, err, "v1 闭集必须拒绝 knowledge_collection")
	require.Contains(t, err.Error(), "agent_resource_bindings_resource_kind_check",
		"拒绝必须来自闭集约束，不是别的失败")

	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion), "v1 库必须能前向迁到当前版本")
	version, err = schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)

	// 同一行（同一主键）在 v2 必须可写——闭集确实加宽了。
	_, err = conn.ExecContext(ctx, bindingInsertSQL, "knowledge_collection")
	require.NoError(t, err, "v2 闭集必须接受 knowledge_collection")

	// 加宽不等于放开：闭集之外的值仍然被拒。
	_, err = conn.ExecContext(ctx, bindingInsertSQL, "not_a_kind")
	require.Error(t, err, "闭集之外的类别必须继续被拒")
	require.Contains(t, err.Error(), "agent_resource_bindings_resource_kind_check")
}

// TestPostgres_UpgradeV1ToV2RollsBackOnFailure 判 1 → 2 升级**失败时整体回滚**：另一条
// 连接持表锁时，0002 的 ALTER TABLE 在会话 `lock_timeout` 上失败；此时版本必须仍停在 1、
// 闭集仍是 v1 的窄集（DROP 与 ADD 同事务，不得留下半截结构）；释放锁后重试成功。
// 逆改点：把 0002 的两条 ALTER 拆到两个事务（或去掉 ensureSchema 的事务）→ 失败后版本
// 或闭集会出现半截状态，两个"回滚后"断言必有一个红。
func TestPostgres_UpgradeV1ToV2RollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	conn := migrationConn(t)
	require.NoError(t, ensureSchema(ctx, conn, 1), "先只建到 v1 基线")

	var schema string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema))

	// 阻塞方用独立连接、独立事务持 ACCESS SHARE 锁：ALTER TABLE 要 ACCESS EXCLUSIVE，
	// 两者冲突。锁的是**本用例 schema** 内的表——阻塞方不钉 search_path，必须带 schema 限定。
	blockerPool := testPool(t)
	blocker, err := blockerPool.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blocker.Close()) })
	blockerTx, err := blocker.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blockerTx.Rollback() })
	_, err = blockerTx.ExecContext(ctx,
		`LOCK TABLE "`+schema+`".agent_resource_bindings IN ACCESS SHARE MODE`)
	require.NoError(t, err)

	_, err = conn.ExecContext(ctx, `SET lock_timeout = '300ms'`)
	require.NoError(t, err)
	err = ensureSchema(ctx, conn, SupportedSchemaVersion)
	require.Error(t, err, "表被锁住时升级必须失败，不得静默半迁")
	require.Contains(t, err.Error(), "0002_agent_binding_knowledge_kind",
		"失败必须来自 v2 脚本这一步")

	// 回滚证据 ①：版本记录没有前进。
	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 1, version, "失败的迁移不得留下版本记录")
	// 回滚证据 ②：闭集没有加宽（DROP 之后若没有回滚，v1 约束会永久消失）。
	_, err = conn.ExecContext(ctx, bindingInsertSQL, "knowledge_collection")
	require.Error(t, err, "失败的迁移不得留下半截约束")
	require.Contains(t, err.Error(), "agent_resource_bindings_resource_kind_check")

	// 释放锁后重试必须成功——失败不留下不可恢复的半截结构。
	require.NoError(t, blockerTx.Rollback())
	_, err = conn.ExecContext(ctx, `SET lock_timeout = 0`)
	require.NoError(t, err)
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion))
	version, err = schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)
	_, err = conn.ExecContext(ctx, bindingInsertSQL, "knowledge_collection")
	require.NoError(t, err)
}

// TestPostgres_UpgradeLegacyV1RepairsSourceText 判 R59 的**前向迁移路径**：v1 基线曾被
// 就地改写（加上 `kb_document_versions.source_text`），改写之前建过库的实例版本记录停在
// 1 且没有该列——`ensureSchema` 只跑 `current+1..target`、不会重跑 v1，因此必须有一条
// 追加脚本把列补回来，否则这些库一读知识库就报 `column "source_text" does not exist`。
//
// 夹具：先建到 v1，再 DROP COLUMN 模拟改写前的结构（版本记录保持 1，与真实旧库一致）。
// 判据分三段：迁移前 SELECT 该列必须失败（证明夹具确实复现了旧结构）、迁移后必须可读、
// 列的 `data_type` / `is_nullable` 必须与契约一致（证明修复的是同一列而不是近似替身）。
// 逆改点：删掉 0003（或让 `SupportedSchemaVersion` 跳过它）→ 迁移后 SELECT 仍报列不存在，
// 第二段断言红；把 0003 写成 `NOT NULL DEFAULT` → 紧约束下既有行全被拒、第三段红。
func TestPostgres_UpgradeLegacyV1RepairsSourceText(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()

	require.NoError(t, ensureSchema(ctx, conn, 1), "先只建到 v1 基线")
	_, err := conn.ExecContext(ctx, `ALTER TABLE kb_document_versions DROP COLUMN source_text`)
	require.NoError(t, err, "夹具：去掉 source_text，复现 R59 之前的 v1 结构")

	// 判别力前置：旧结构确实读不到这一列，否则下面的"迁移后能读"证明不了修复。
	// 用聚合而不是 `LIMIT 1`：空表上 `SELECT <列>` 即使列存在也是 `no rows`，两种失败
	// 混在一起就判不出是"缺列"还是"没数据"——聚合恒返回一行，报错只可能来自列本身。
	var probe int64
	err = conn.QueryRowContext(ctx, `SELECT count(source_text) FROM kb_document_versions`).Scan(&probe)
	require.Error(t, err, "夹具判别力前置：旧结构的 SELECT 必须失败")
	require.Contains(t, err.Error(), "source_text")

	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 1, version, "旧库的版本记录停在 1")

	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion), "旧 v1 库必须能前向迁到当前版本")

	version, err = schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)

	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(source_text) FROM kb_document_versions`).Scan(&probe),
		"迁移后 source_text 必须可读——旧库不得停在缺列状态")
	var dataType, nullable string
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'kb_document_versions'
		  AND column_name = 'source_text'`).Scan(&dataType, &nullable))
	require.Equal(t, "text", dataType, "补回的列必须与契约同型")
	require.Equal(t, "YES", nullable, "列必须可空：NULL 表示该版本未保留原文")
}

// TestPostgres_MigrateTakesAdvisoryLock 判迁移真的取**本 schema** 的 advisory 锁：另一条
// 连接先持同一把锁时，迁移连接必须在锁上等待，按会话 `lock_timeout` 失败而不是照建表；
// 锁释放后重试成功。同 schema 互斥是新锁定的第一半判据（第二半见
// TestPostgres_Migrate_DifferentSchemasRunInParallel）。
// 逆改点：删掉 `pg_advisory_xact_lock` 那条语句 → 本用例拿到 nil error 而红（阻塞方
// 持锁期间迁移照常建表）；把键改成别的种子 / 不按 schema 派生 → 同样红。
func TestPostgres_MigrateTakesAdvisoryLock(t *testing.T) {
	ctx := context.Background()
	conn := migrationConn(t)

	// 目标 schema 从连接本身取：锁键必须按它派生，才能与 ensureSchema 取到同一把锁。
	var schema string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema))

	// 阻塞方用独立连接、独立事务持**本 schema** 的锁：键按目标 schema 派生，不是全库一把。
	blockerPool := testPool(t)
	blocker, err := blockerPool.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blocker.Close()) })
	blockerTx, err := blocker.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blockerTx.Rollback() })
	_, err = blockerTx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationAdvisoryKeyFor(schema))
	require.NoError(t, err)

	// 会话级 lock_timeout：让"等待"变成可判定的失败，而不是把测试挂住。
	_, err = conn.ExecContext(ctx, `SET lock_timeout = '300ms'`)
	require.NoError(t, err)
	err = ensureSchema(ctx, conn, SupportedSchemaVersion)
	require.Error(t, err, "持锁期间迁移必须等锁，不得照常建表")
	require.Contains(t, err.Error(), "advisory", "失败必须来自迁移锁这一步")

	// 释放锁后重试必须成功——失败不能留下半截结构。
	require.NoError(t, blockerTx.Rollback())
	_, err = conn.ExecContext(ctx, `SET lock_timeout = 0`)
	require.NoError(t, err)
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion))

	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)
}

// TestPostgres_Migrate_DifferentSchemasRunInParallel 判迁移锁按 schema 隔离：另一个 schema
// 持着它的迁移锁时，本 schema 的迁移必须照常建表完成——全库固定键会让它一直等到会话
// `lock_timeout` 失败。这是"不同 schema 可并行"的直接判据。
// 逆改点：把 migrationAdvisoryKeyFor 改回全库固定键 → 本用例拿到 lock timeout 而红。
func TestPostgres_Migrate_DifferentSchemasRunInParallel(t *testing.T) {
	ctx := context.Background()

	// 阻塞方：在**另一个**隔离 schema 上持它的迁移锁。
	otherConn := migrationConn(t)
	var otherSchema string
	require.NoError(t, otherConn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&otherSchema))
	blockerPool := testPool(t)
	blocker, err := blockerPool.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blocker.Close()) })
	blockerTx, err := blocker.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blockerTx.Rollback() })
	_, err = blockerTx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationAdvisoryKeyFor(otherSchema))
	require.NoError(t, err)

	// 被测：本用例 schema 的迁移。锁等待上限故意设小——若两把键相同，必然失败。
	conn := migrationConn(t)
	_, err = conn.ExecContext(ctx, `SET lock_timeout = '300ms'`)
	require.NoError(t, err)
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion),
		"别的 schema 持锁不得挡住本 schema 的迁移")

	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)
}

// TestPostgres_Migrate_RejectsVersionTableOutsideTargetSchema 判 search_path 回退被拒绝：
// 目标 schema 是空的，但 search_path 上挂着另一个已迁移的 schema，`schema_version` 会
// 解析到后者——迁移若照常执行，就会在本 schema 建表、按别处判版本（或直接 no-op）。
// 用例要求明确报错，且目标 schema 一张表都不许留下。
// 逆改点：去掉 migrationTargetSchema 的归属核对 → 本用例拿到 nil error（no-op 放行）而红。
func TestPostgres_Migrate_RejectsVersionTableOutsideTargetSchema(t *testing.T) {
	ctx := context.Background()

	ownerConn := migrationConn(t)
	require.NoError(t, ensureSchema(ctx, ownerConn, SupportedSchemaVersion))
	var owner string
	require.NoError(t, ownerConn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&owner))

	targetConn := migrationConn(t)
	var target string
	require.NoError(t, targetConn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&target))
	_, err := targetConn.ExecContext(ctx,
		`SELECT set_config('search_path', $1, false)`, target+", "+owner)
	require.NoError(t, err)

	err = ensureSchema(ctx, targetConn, SupportedSchemaVersion)
	require.Error(t, err, "版本表在别的 schema 时必须拒绝迁移")
	require.Contains(t, err.Error(), "search_path 回退", "失败必须来自归属核对")

	// 拒绝不得留下半截结构：目标 schema 仍然零表。
	var tables int
	require.NoError(t, targetConn.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = $1 AND table_type = 'BASE TABLE'`, target).Scan(&tables))
	require.Zero(t, tables, "拒绝后目标 schema 不得留下任何表")
}
