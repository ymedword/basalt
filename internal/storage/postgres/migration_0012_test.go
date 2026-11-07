//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// 本文件是 US-24 追加迁移 `0012_distributed_execution.sql` 的**有库**用例（T003）：
// 在隔离 schema 上先迁到 v11、写入真实在途行，再前向迁到 v12，逐条核对回填、
// 闭集扩展与 `resource_id` 编码。断言以**迁移后的数据库读数**为准，不以代码意图为准。
// The database-backed acceptance for migration 0012: migrate an isolated schema to v11,
// write real in-flight rows, forward-migrate to v12 and read the results back.
//
// 租约领取 / 续约的并发语义由 T005 的 `TestLease_*` 钉住；本文件固定回填范围与
// `resource_id` 的编码等价格式（模块卡 §3.4 末的 m4 契约）。

// leaseFixture 是 execution_leases 一行的读数快照。
type leaseFixture struct {
	Owner      string
	Epoch      int64
	AcquiredAt int64
	RenewedAt  int64
	ExpiresAt  int64
	ReleasedAt sql.NullInt64
}

// readLease 读一条租约；不存在返回 false（调用方据此断言"不应回填"）。
// 参数取 `Queryer`（`*sql.Conn` / `*DB` / `*sql.Tx` 都满足）：停机迁移用例在关闭 v11
// 连接之后必须经 `*DB` 读同一批行。
func readLease(t *testing.T, ctx context.Context, q Queryer, kind, resourceID string) (leaseFixture, bool) {
	t.Helper()
	var row leaseFixture
	err := q.QueryRowContext(ctx, `
SELECT owner_worker_id, ownership_epoch, acquired_at, renewed_at, expires_at, released_at
  FROM execution_leases WHERE kind = $1 AND resource_id = $2`, kind, resourceID).
		Scan(&row.Owner, &row.Epoch, &row.AcquiredAt, &row.RenewedAt, &row.ExpiresAt, &row.ReleasedAt)
	if err == sql.ErrNoRows {
		return leaseFixture{}, false
	}
	require.NoError(t, err)
	return row, true
}

// dbNowMillis 读数据库时钟的毫秒值——与迁移脚本 `(extract(epoch from now()) * 1000)::bigint`
// 同一表达式，判"立即过期"时不拿测试进程时钟比较。
// dbNowMillis uses the database clock, exactly like the backfill scripts.
func dbNowMillis(t *testing.T, ctx context.Context, conn *sql.Conn) int64 {
	t.Helper()
	var now int64
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT (extract(epoch from now()) * 1000)::bigint`).Scan(&now))
	return now
}

// seedV12Operation 写一条管理操作行：resource_versions / flow_definitions 的 operation_id
// 外键要求它先存在（迁移不会替测试补这一行）。
func seedV12Operation(t *testing.T, ctx context.Context, conn *sql.Conn, operationID string) {
	t.Helper()
	_, err := conn.ExecContext(ctx, `
INSERT INTO management_operations(operation_id, action, target_kind, target_id, actor_subject,
                                  auth_method, source, status, created_at)
VALUES ($1, 'resource.publish', 'agent', 'reader', 'local-admin', 'local_trust', 'cli', 'success', 1000)`,
		operationID)
	require.NoError(t, err)
}

// TestMigration0012_BackfillsInFlightLeases 判三类在途行各回填一条**立即过期**的
// `legacy` 租约、关闭态根与已过 deadline 的根**不回填**、重复迁移幂等。
//
// 逆改点：去掉任一 INSERT 回填 → 对应用例红；把 `now()` 改成 `now()+ttl` → "立即过期"
// 断言红；把 flow 根过滤放宽到子任务 → `v12-child` 的"不应存在"断言红；
// 去掉 deadline 谓词 → `v12-root-past-deadline` 的"不应存在"断言红（FR-24-28）。
// Reverse-edit points: drop a backfill, use now()+ttl, widen the root filter to child
// rows, or drop the deadline predicate — each turns a named assertion red.
func TestMigration0012_BackfillsInFlightLeases(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, 11))

	// 三类在途 / 终态行与四类"不应回填"的 Flow 根。时间列取远未来 / 远过去的大整数，
	// 判据只看是否被回填与回填时刻，不看夹具的业务时间。
	// In-flight vs terminal rows; four Flow roots that must stay untouched.
	//
	// QueryRow 一律**单条语句**：pgx 在 extended 协议下一次只接受一条命令
	// （"cannot insert multiple commands into a prepared statement"），多语句只能走
	// 无参数的 ExecContext（simple 协议）。
	// QueryRow accepts exactly one statement under the extended protocol; multi-statement
	// fixtures must use ExecContext with no arguments.
	var startedExecutionID, doneExecutionID int64
	_, err := conn.ExecContext(ctx, `
INSERT INTO runs(run_id, agent, channel, user_id, trigger, status, iterations, tool_calls,
                 started_at, caller_id, authorizing_kind, actor)
VALUES ('v12-run-active', 'reader', 'web', 'original-user', 'user', 'running', 1, 1,
        1000, 'local-admin', 'admin-builtin', 'admin-cli'),
       ('v12-run-done', 'reader', 'web', 'original-user', 'user', 'completed', 1, 1,
        1000, 'local-admin', 'admin-builtin', 'admin-cli');
INSERT INTO scheduled_tasks(task_id, agent_name, cron, zone, message, definition_hash,
                            created_at, updated_at, caller_id, authorizing_kind)
VALUES ('v12-task', 'reader', '0 * * * * *', 'UTC', 'ping', 'hash-v12', 1000, 1000,
        'local-admin', 'admin-builtin')`)
	require.NoError(t, err)
	require.NoError(t, conn.QueryRowContext(ctx, `
INSERT INTO task_executions(task_id, status, trigger, started_at)
VALUES ('v12-task', 'started', 'cron', 1000) RETURNING id`).Scan(&startedExecutionID))
	require.NoError(t, conn.QueryRowContext(ctx, `
INSERT INTO task_executions(task_id, status, trigger, started_at)
VALUES ('v12-task', 'completed', 'manual', 1000) RETURNING id`).Scan(&doneExecutionID))

	seedV12Operation(t, ctx, conn, "op-v12-flow")
	_, err = conn.ExecContext(ctx, `
INSERT INTO flow_definitions(definition_uid, version, name, content_hash, definition_content,
                             contract_content, created_at, operation_id)
VALUES ('flw_v12', 1, 'v12-flow', 'hash-v12-flow', '{}', '{}', 1000, 'op-v12-flow');
INSERT INTO flow_tasks(task_id, root_task_id, parent_task_id, caller_id, authorizing_kind,
                       authorizing_key_id, definition_uid, definition_version, contract_version,
                       status, state_version, execution_epoch, current_revision, cancel_requested,
                       close_intent, deadline, created_at, updated_at, next_eligible_at,
                       idempotency_key, request_hash, depth)
VALUES ('v12-root', 'v12-root', NULL, 'local-admin', 'admin-builtin', NULL,
        'flw_v12', 1, 1, 'queued', 1, 1, 0, false, 'none', 9100000000000, 1000, 1000, 1000,
        'idem-v12-root', 'hash-v12-root', 0),
       ('v12-child', 'v12-root', 'v12-root', 'local-admin', 'admin-builtin', NULL,
        'flw_v12', 1, 1, 'queued', 1, 1, 0, false, 'none', 9100000000000, 1000, 1000, 1000,
        'idem-v12-child', 'hash-v12-child', 1),
       ('v12-root-cancelling', 'v12-root-cancelling', NULL, 'local-admin', 'admin-builtin', NULL,
        'flw_v12', 1, 1, 'cancelling', 1, 1, 0, true, 'cancel', 9100000000000, 1000, 1000, 1000,
        'idem-v12-cancelling', 'hash-v12-cancelling', 0),
       ('v12-root-abandon', 'v12-root-abandon', NULL, 'local-admin', 'admin-builtin', NULL,
        'flw_v12', 1, 1, 'blocked', 1, 1, 0, true, 'abandon', 9100000000000, 1000, 1000, 1000,
        'idem-v12-abandon', 'hash-v12-abandon', 0),
       ('v12-root-past-deadline', 'v12-root-past-deadline', NULL, 'local-admin', 'admin-builtin', NULL,
        'flw_v12', 1, 1, 'queued', 1, 1, 0, false, 'none', 500, 100, 100, 100,
        'idem-v12-past-deadline', 'hash-v12-past-deadline', 0);
INSERT INTO flow_task_contracts(task_id, contract_version, definition_uid, definition_version,
                                content, content_hash)
SELECT t.task_id, 1, 'flw_v12', 1, '{}', 'hash-contract-' || t.task_id
  FROM flow_tasks t WHERE t.definition_uid = 'flw_v12';
INSERT INTO flow_task_revisions(task_id, revision, contract_version, checkpoint, created_at)
SELECT t.task_id, 0, 1,
       jsonb_build_object('format_version', '1', 'task_id', t.task_id, 'revision', 0),
       1000
  FROM flow_tasks t WHERE t.definition_uid = 'flw_v12'`)
	require.NoError(t, err)

	require.NoError(t, ensureSchema(ctx, conn, 12))

	for _, want := range []struct{ kind, id string }{
		{"run", "v12-run-active"},
		{"schedule_execution", strconv.FormatInt(startedExecutionID, 10)},
		{"flow_root", "v12-root"},
	} {
		row, ok := readLease(t, ctx, conn, want.kind, want.id)
		require.Truef(t, ok, "%s/%s 必须回填租约", want.kind, want.id)
		require.Equal(t, "legacy", row.Owner, "%s/%s 的遗留所有者", want.kind, want.id)
		require.Zero(t, row.Epoch, "%s/%s 的遗留代次为 0", want.kind, want.id)
		require.False(t, row.ReleasedAt.Valid, "%s/%s 不得写成已释放", want.kind, want.id)
		require.Equal(t, row.AcquiredAt, row.RenewedAt, "一次迁移一个时刻")
		require.Equal(t, row.AcquiredAt, row.ExpiresAt, "遗留租约必须立即过期，不是 now()+ttl")
		require.LessOrEqual(t, row.ExpiresAt, dbNowMillis(t, ctx, conn),
			"%s/%s 的 expires_at 必须不晚于数据库当前时刻", want.kind, want.id)
	}
	for _, absent := range []struct{ kind, id string }{
		{"run", "v12-run-done"},
		{"schedule_execution", strconv.FormatInt(doneExecutionID, 10)},
		{"flow_root", "v12-child"},
		{"flow_root", "v12-root-cancelling"},
		{"flow_root", "v12-root-abandon"},
		{"flow_root", "v12-root-past-deadline"},
	} {
		_, ok := readLease(t, ctx, conn, absent.kind, absent.id)
		require.Falsef(t, ok, "%s/%s 不在回填范围，不得出现租约", absent.kind, absent.id)
	}

	// 三类回填共用同一迁移时刻（同一事务内 now() 恒定）。
	var distinctInstants int
	require.NoError(t, conn.QueryRowContext(ctx, `
SELECT count(DISTINCT acquired_at) FROM execution_leases WHERE owner_worker_id = 'legacy'`).
		Scan(&distinctInstants))
	require.Equal(t, 1, distinctInstants, "三类回填必须共用同一迁移时刻")

	var total int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM execution_leases`).Scan(&total))
	require.Equal(t, 3, total, "回填只产生三条租约")

	// 重复迁移幂等：不新增租约、版本仍为 12。
	require.NoError(t, ensureSchema(ctx, conn, 12), "重复执行必须是幂等 no-op")
	var afterRepeat int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM execution_leases`).Scan(&afterRepeat))
	require.Equal(t, total, afterRepeat)
	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 12, version)
}

// TestMigration0012_ResourceKindDomain 判 `resource_kind` 闭集扩到 `agent_bundle` 后：
// 两类新行可写、复合 CHECK 的 skill 支覆盖 agent_bundle、既有 kind 的合法 / 非法
// 判定与升级前一致，且三条约束**名字**与隔离 schema 实测一致（名字不对时
// DROP IF EXISTS 会静默空转，直到插入期才暴露）。
//
// 逆改点：只改列级 CHECK 不改复合 CHECK → agent_bundle 合法行插入红；漏改
// resource_pointers → 指针行插入红；删掉任一 DROP/ADD 对 → 约束数量断言红。
// Reverse-edit points: leave the compound CHECK untouched, miss resource_pointers, or drop
// one DROP/ADD pair — each turns a named assertion red.
func TestMigration0012_ResourceKindDomain(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, 11))
	require.NoError(t, ensureSchema(ctx, conn, 12))
	seedV12Operation(t, ctx, conn, "op-v12-resource")

	// 合法：agent_bundle 需要 manifest_json 与 storage_path（与 skill 同一支）。
	var bundleVersionID int64
	require.NoError(t, conn.QueryRowContext(ctx, `
INSERT INTO resource_versions(resource_kind, resource_uid, resource_id, version_no, content_hash,
                              manifest_json, storage_path, created_at, operation_id)
VALUES ('agent_bundle', 'rs_v12bundle', 'reader', 1, 'hash-v12-bundle',
        '{"files":[]}', 'bundles/reader/hash-v12-bundle', 1000, 'op-v12-resource')
RETURNING id`).Scan(&bundleVersionID))
	_, err := conn.ExecContext(ctx, `
INSERT INTO resource_pointers(resource_kind, resource_id, resource_uid, published_version_id,
                              updated_at, operation_id)
VALUES ('agent_bundle', 'reader', 'rs_v12bundle', $1, 1000, 'op-v12-resource')`, bundleVersionID)
	require.NoError(t, err)

	// 非法：agent_bundle 缺 manifest_json；未知 kind；既有 kind 的旧规则不变。
	for name, stmt := range map[string]string{
		"agent_bundle_missing_manifest": `
INSERT INTO resource_versions(resource_kind, resource_uid, resource_id, version_no, content_hash,
                              storage_path, created_at, operation_id)
VALUES ('agent_bundle', 'rs_v12bundle2', 'reader', 1, 'hash-v12-bundle2',
        'bundles/reader/hash-v12-bundle2', 1000, 'op-v12-resource')`,
		"unknown_kind": `
INSERT INTO resource_versions(resource_kind, resource_uid, resource_id, version_no, content_hash,
                              definition_yaml, created_at, operation_id)
VALUES ('mystery_kind', 'rs_v12mystery', 'reader', 1, 'hash-v12-mystery', 'x: 1',
        1000, 'op-v12-resource')`,
		"model_connection_missing_yaml": `
INSERT INTO resource_versions(resource_kind, resource_uid, resource_id, version_no, content_hash,
                              manifest_json, storage_path, created_at, operation_id)
VALUES ('model_connection', 'rs_v12model', 'model', 1, 'hash-v12-model',
        '{}', 'somewhere', 1000, 'op-v12-resource')`,
		"skill_missing_storage_path": `
INSERT INTO resource_versions(resource_kind, resource_uid, resource_id, version_no, content_hash,
                              manifest_json, created_at, operation_id)
VALUES ('skill', 'rs_v12skill', 'skill', 1, 'hash-v12-skill', '{}', 1000, 'op-v12-resource')`,
		"pointer_notify_channel": `
INSERT INTO resource_pointers(resource_kind, resource_id, published_version_id, updated_at, operation_id)
VALUES ('notify_channel', 'channel', $1, 1000, 'op-v12-resource')`,
	} {
		var err error
		if name == "pointer_notify_channel" {
			_, err = conn.ExecContext(ctx, stmt, bundleVersionID)
		} else {
			_, err = conn.ExecContext(ctx, stmt)
		}
		require.Errorf(t, err, "%s 必须被 CHECK 拒绝", name)
	}

	// 既有两类 kind 的合法行仍可写：闭集只加宽，不放宽各自的内容要求。
	_, err = conn.ExecContext(ctx, `
INSERT INTO resource_versions(resource_kind, resource_uid, resource_id, version_no, content_hash,
                              definition_yaml, created_at, operation_id)
VALUES ('model_connection', 'rs_v12model', 'model', 1, 'hash-v12-model',
        'provider: stub', 1000, 'op-v12-resource')`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
INSERT INTO resource_versions(resource_kind, resource_uid, resource_id, version_no, content_hash,
                              manifest_json, storage_path, created_at, operation_id)
VALUES ('skill', 'rs_v12skill', 'skill', 1, 'hash-v12-skill-ok',
        '{}', 'skills/skill', 1000, 'op-v12-resource')`)
	require.NoError(t, err)

	// 约束名与实测一致：两张表上涉及 resource_kind 的 CHECK 恰好三条、名字逐字匹配。
	// 名字若与实测不符，DROP IF EXISTS 静默空转，这里会看到旧约束仍在或重复约束。
	//
	// **必须**按 schema 过滤：`pg_class.relname` 在全库范围内同名同表遍地都是（同库
	// 其他隔离 schema / public 里的旧版本表），不过滤会把别处的旧约束读进来、在 map
	// 里按同名键覆盖掉本 schema 的定义——实测症状就是"插入 agent_bundle 成功，读回的
	// 定义却没有 agent_bundle"。
	// The query must be scoped to the current schema: identically named tables in other
	// schemas would overwrite this schema's definitions in the map.
	rows, err := conn.QueryContext(ctx, `
SELECT t.relname, c.conname, pg_get_constraintdef(c.oid)
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
  JOIN pg_namespace n ON n.oid = t.relnamespace
 WHERE n.nspname = current_schema()
   AND t.relname IN ('resource_versions', 'resource_pointers')
   AND c.contype = 'c'
   AND pg_get_constraintdef(c.oid) LIKE '%resource_kind%'
 ORDER BY t.relname, c.conname`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	got := map[string]string{}
	for rows.Next() {
		var table, name, def string
		require.NoError(t, rows.Scan(&table, &name, &def))
		got[table+"."+name] = def
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 3, "涉及 resource_kind 的 CHECK 必须恰好三条，实测：%v", got)
	for _, key := range []string{
		"resource_versions.resource_versions_resource_kind_check",
		"resource_versions.resource_versions_check",
		"resource_pointers.resource_pointers_resource_kind_check",
	} {
		require.Contains(t, got, key, "约束名与隔离 schema 实测不一致——DROP IF EXISTS 会静默空转")
	}
	require.Contains(t, got["resource_versions.resource_versions_resource_kind_check"], "agent_bundle")
	require.Contains(t, got["resource_versions.resource_versions_check"], "agent_bundle")
	require.Contains(t, got["resource_pointers.resource_pointers_resource_kind_check"], "agent_bundle")
}

// TestLeaseResourceIDEncoding 固定 `resource_id` 的编码等价格式：SQL 侧 `id::text`
// 必须与 Go 侧 `strconv.FormatInt(id, 10)` 逐字相同（无前导零 / 无符号），迁移回填的
// 历史执行因此与新 Worker 领取时算出同一个 `resource_id`——否则同一执行会出现两条租约。
//
// 逆改点：把回填的 `id::text` 改成其他格式（前导零 / 十六进制），或在 Go 侧改用带前缀
// 的格式化 → 等值断言红；把 run / flow_root 的编码改成拼接 kind 前缀 → "原样写入"断言红。
// 选 2^53+1（9007199254740993）是为了让任何浮点中转在此现形。
// Reverse-edit points: change either side's format — the equivalence assertion goes red.
func TestLeaseResourceIDEncoding(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, 11))

	const largeID int64 = 9007199254740993 // 2^53+1：浮点中转会丢精度
	_, err := conn.ExecContext(ctx, `
INSERT INTO scheduled_tasks(task_id, agent_name, cron, zone, message, definition_hash,
                            created_at, updated_at, caller_id, authorizing_kind)
VALUES ('v12-enc', 'reader', '0 * * * * *', 'UTC', 'ping', 'hash-v12-enc', 1000, 1000,
        'local-admin', 'admin-builtin');
INSERT INTO task_executions(id, task_id, status, trigger, started_at)
VALUES (1, 'v12-enc', 'completed', 'manual', 1000),
       (42, 'v12-enc', 'completed', 'manual', 1000),
       (9007199254740993, 'v12-enc', 'started', 'manual', 1000);
INSERT INTO runs(run_id, agent, channel, user_id, trigger, status, iterations, tool_calls,
                 started_at, caller_id, authorizing_kind, actor)
VALUES ('scheduler-1776000000123-abc123', 'reader', 'scheduler', 'scheduler', 'cron', 'running',
        1, 1, 1000, 'local-admin', 'admin-builtin', 'scheduler')`)
	require.NoError(t, err)

	// SQL 编码：id::text 与 Go 格式化逐字一致（含大整数与常规值）。
	for _, id := range []int64{1, 42, largeID, 9223372036854775807} {
		var encoded string
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT $1::bigint::text`, id).Scan(&encoded))
		require.Equal(t, strconv.FormatInt(id, 10), encoded)
		require.NotContains(t, encoded, "+")
		require.NotContains(t, encoded, " ")
	}

	require.NoError(t, ensureSchema(ctx, conn, 12))

	// 回填用 id::text：历史执行的租约 resource_id 必须与 Go 侧格式化结果一致。
	row, ok := readLease(t, ctx, conn, "schedule_execution", strconv.FormatInt(largeID, 10))
	require.True(t, ok, "回填的 schedule_execution 租约必须按 id::text 编码")
	require.Equal(t, "legacy", row.Owner)

	var backfilled string
	require.NoError(t, conn.QueryRowContext(ctx, `
SELECT resource_id FROM execution_leases WHERE kind = 'schedule_execution'`).Scan(&backfilled))
	require.Equal(t, strconv.FormatInt(largeID, 10), backfilled)

	// run / flow_root 是 text 原样写入：不附加 kind 前缀、不改写大小写。
	runRow, ok := readLease(t, ctx, conn, "run", "scheduler-1776000000123-abc123")
	require.True(t, ok, "run 租约必须原样使用 run_id")
	require.Equal(t, "legacy", runRow.Owner)
}

// upgradeReclaimTTL 是停机迁移用例里回收器领取后拿到的有效期（与被领资源无关，
// 只要求为正；真正判"能不能领"的是回填租约的 `expires_at`）。
const upgradeReclaimTTL = 30 * time.Second

// upgradeReclaimWorker 是新进程的首个 Worker 实例标识。
func upgradeReclaimWorker() core.WorkerIdentity {
	return core.WorkerIdentity{
		InstanceID:    "worker-upgraded",
		Name:          "upgraded",
		Version:       "test",
		SchemaVersion: SupportedSchemaVersion,
	}
}

// firstReclaimRound 复刻 `internal/app.ReclaimOnce` 的 A1 → A2 → A3 顺序，用于
// **停机迁移后的首轮回收**判据。
//
// 为什么不直接调用 `ReclaimOnce`：它在 `internal/app`，而本包被 `internal/app` 导入，
// 反向 import 会成环；`ReclaimOnce` 的循环 / 上限 / 装配本身由
// `internal/app/reclaimer_test.go` 的用例钉住。这里用的是它**逐条调用的同一组导出
// 原语**，判的是"升级回填出来的行能不能被首轮回收收尾"，与会话、cron 的装配无关。
//
// Why not call ReclaimOnce: the assembly layer imports this package, so the reverse
// import would cycle. This helper replays the exact primitives ReclaimOnce calls.
func firstReclaimRound(t *testing.T, db *DB, worker core.WorkerIdentity,
	wantTakeover bool,
) core.ReclaimReport {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := NewReclaimerStore(db)
	leases := NewLeaseStore(db)
	var report core.ReclaimReport

	runs, err := store.ClaimExpired(ctx, core.LeaseRun, worker, 64, upgradeReclaimTTL)
	require.NoError(t, err)
	for _, lease := range runs {
		require.Equal(t, wantTakeover, lease.TookOver, "run/%s 的接管标记", lease.ResourceID)
		part, err := RecoverRun(ctx, db, logger, lease.LeaseToken, now)
		require.NoError(t, err)
		report.Runs += part.ClosedRuns
		report.PendingAudit += part.Unknowns
	}

	executions, err := store.ClaimExpired(ctx, core.LeaseScheduleExecution, worker, 64, upgradeReclaimTTL)
	require.NoError(t, err)
	for _, lease := range executions {
		require.Equal(t, wantTakeover, lease.TookOver, "schedule_execution/%s 的接管标记", lease.ResourceID)
		status, err := RecoverExecution(ctx, db, lease.LeaseToken, now)
		require.NoError(t, err)
		if status != "" {
			report.Executions++
		}
	}

	roots, err := store.ClaimExpired(ctx, core.LeaseFlowRoot, worker, 64, upgradeReclaimTTL)
	require.NoError(t, err)
	for _, lease := range roots {
		require.Equal(t, wantTakeover, lease.TookOver, "flow_root/%s 的接管标记", lease.ResourceID)
		// A3 的分路（`app.reclaimFlowRoot` 的判定顺序）：先取证，Isolated 才允许推进；
		// 本夹具的根是 queued、无 attempt、无未决动作，落"交还"支——状态推进归 Flow
		// Worker，回收器只负责把执行权交还出去。
		evidence, err := RecoverFlowRoot(ctx, db, lease.ResourceID)
		require.NoError(t, err)
		require.True(t, evidence.Isolated, "根子树不得有仍有效的 run 租约")
		require.True(t, evidence.Joined, "根子树不得有 prepared / running 的 attempt")
		require.False(t, evidence.Unresolved, "根子树不得有未决动作")
		require.NotEqual(t, flow.Blocked, evidence.Task.Status, "queued 根走交还支，不进 Recover")
		require.NoError(t, leases.Release(ctx, lease.LeaseToken))
		report.FlowRoots++
	}
	return report
}

// createUpgradeFlowRoot 用真实 `FlowStore.Create` 写一条 queued 根。deadline 拉长到
// 30 分钟：迁移 0012 只回填 `deadline > now` 的根，夹具不能卡在 60 秒边界上。
// The deadline is widened so the backfill predicate (`deadline > now`) is not flaky.
func createUpgradeFlowRoot(t *testing.T, s *FlowStore, def flow.DefinitionVersion,
	id, key string,
) {
	t.Helper()
	req := flowCreateRequest(def, id, key)
	req.Deadline = req.CreatedAt + (30 * time.Minute).Milliseconds()
	_, err := s.Create(t.Context(), req)
	require.NoError(t, err, "创建 Flow 根 %s", id)
}

// TestUpgrade_StopTheWorldLeaseBackfill 判**停机迁移**的升级路径（FR-24-31 / 模块卡
// §6.1 第 10 条）：v11 库里的三类在途行（running Run / started 执行 / 非终态 Flow 根）
// 与两类关闭态根 → **关闭 v11 连接**（停机迁移，不模拟在线混跑）→ 新进程 `Open` 把库
// 带到 v12 → 首轮回收把三条回填租约全部合法领取并收尾，关闭态根零租约，迁移后新建的
// 活租约一字不动，重复迁移不产生重复租约。
//
// 逆改点：去掉任一类的 INSERT 回填 → 对应"首轮收尾"断言红；把回填写成
// `released_at = now()` → `TookOver` 断言红（不是真接管）；把关闭态根也回填 →
// "零租约"断言红；把 `expires_at` 写成 `now()+ttl` → 首轮领取不到、收尾断言红。
// Reverse-edit points: drop a backfill, mark it released, backfill a closed root, or
// future-date the lease — each turns a named assertion red.
func TestUpgrade_StopTheWorldLeaseBackfill(t *testing.T) {
	ctx := context.Background()

	// ① v11 库：三类在途行 + 两类关闭态根。关闭态覆盖迁移脚本的两个过滤条件
	//    （`cancel_requested` / `close_intent <> 'none'`）；已过 deadline 的一类由
	//    `TestMigration0012_BackfillsInFlightLeases` 钉住，本用例不重复。
	conn, paths := migrationConnWithSchema(t)
	require.NoError(t, ensureSchema(ctx, conn, 11))
	require.NoError(t, conn.Close())

	// Flow 根必须走**真实写入路径**（`FlowStore.Publish` / `Create`）：检查点与契约
	// 哈希都有严格的自洽校验，手写最小 JSON 会在首轮取证时被判
	// `ErrCheckpointUnsupported`——那不是本用例要测的东西。
	// 这里用一个**不跑迁移**的句柄（`openPool` + 裸 `DB`）把库留在 v11。
	// Flow roots are written through the real store: the checkpoint and contract hashes
	// are self-validating, and a hand-written minimal JSON would fail evidence reading.
	v11Cfg := Config{
		DSN:            withSearchPath(t, testDSN(t), paths...),
		ConnectTimeout: 5 * time.Second,
	}
	v11Pool, err := openPool(ctx, v11Cfg)
	require.NoError(t, err)
	v11 := &DB{pool: v11Pool, cfg: v11Cfg, now: time.Now}

	var startedExecutionID int64
	_, err = v11Pool.ExecContext(ctx, `
INSERT INTO runs(run_id, agent, channel, user_id, trigger, status, iterations, tool_calls,
                 started_at, caller_id, authorizing_kind, actor)
VALUES ('v12u-run-active', 'reader', 'web', 'original-user', 'user', 'running', 1, 1,
        1000, 'local-admin', 'admin-builtin', 'admin-cli');
INSERT INTO scheduled_tasks(task_id, agent_name, cron, zone, message, definition_hash,
                            created_at, updated_at, caller_id, authorizing_kind)
VALUES ('v12u-task', 'reader', '0 * * * * *', 'UTC', 'ping', 'hash-v12u', 1000, 1000,
        'local-admin', 'admin-builtin')`)
	require.NoError(t, err)
	require.NoError(t, v11Pool.QueryRowContext(ctx, `
INSERT INTO task_executions(task_id, status, trigger, started_at)
VALUES ('v12u-task', 'started', 'cron', 1000) RETURNING id`).Scan(&startedExecutionID))

	// 一个 Flow 根 + 两类关闭态根（cancelling / close_intent<>'none'）。
	definition := flowStoreDefinition()
	definition.Limits.MaxDurationSeconds = 1800
	definition.Contract.Budget.MaxDurationSeconds = 1800
	flowStore := NewFlowStore(v11)
	flowVersion := publishFlowStoreDefinition(t, flowStore, definition, 0)
	createUpgradeFlowRoot(t, flowStore, flowVersion, "v12u-root", "idem-v12u-root")
	createUpgradeFlowRoot(t, flowStore, flowVersion, "v12u-root-cancelling", "idem-v12u-cancelling")
	createUpgradeFlowRoot(t, flowStore, flowVersion, "v12u-root-closed", "idem-v12u-abandon")
	_, err = v11Pool.ExecContext(ctx, `
UPDATE flow_tasks SET status = 'cancelling', cancel_requested = true, close_intent = 'cancel'
 WHERE task_id = 'v12u-root-cancelling';
UPDATE flow_tasks SET status = 'blocked', cancel_requested = true, close_intent = 'abandon'
 WHERE task_id = 'v12u-root-closed'`)
	require.NoError(t, err)

	// 停机：v11 连接池先关闭，迁移期间没有任何其它连接参与。
	require.NoError(t, v11Pool.Close(), "停机迁移：v11 连接必须在迁移前关闭")

	// ② 新进程打开同一个 schema：`Open` 内部的 `Migrate` 就是 11 → 12 的前向迁移。
	db := openMigratedTestDB(t, Config{
		DSN:            withSearchPath(t, testDSN(t), paths...),
		ConnectTimeout: 5 * time.Second,
	})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	version, err := schemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version, "停机迁移必须把库带到当前支持版本")

	// 三条回填租约就位、两类关闭态根零租约——这是首轮回收的前提，先钉住前提再判回收。
	for _, want := range []struct{ kind, id string }{
		{"run", "v12u-run-active"},
		{"schedule_execution", strconv.FormatInt(startedExecutionID, 10)},
		{"flow_root", "v12u-root"},
	} {
		row, ok := readLease(t, ctx, db, want.kind, want.id)
		require.Truef(t, ok, "%s/%s 必须被迁移回填", want.kind, want.id)
		require.Equal(t, "legacy", row.Owner)
		require.Zero(t, row.Epoch)
		require.False(t, row.ReleasedAt.Valid, "回填不得写成已释放（那会把接管伪装成交棒）")
	}
	for _, absent := range []struct{ kind, id string }{
		{"flow_root", "v12u-root-cancelling"},
		{"flow_root", "v12u-root-closed"},
	} {
		_, ok := readLease(t, ctx, db, absent.kind, absent.id)
		require.Falsef(t, ok, "%s/%s 是关闭态根，不得回填租约", absent.kind, absent.id)
	}

	// 迁移后新建的活租约：用来证明回收只作用于回填行。
	_, err = db.ExecContext(ctx, `
INSERT INTO runs(run_id, agent, channel, user_id, trigger, status, iterations, tool_calls,
                 started_at, caller_id, authorizing_kind, actor)
VALUES ('v12u-run-live', 'reader', 'web', 'original-user', 'user', 'running', 1, 1,
        1000, 'local-admin', 'admin-builtin', 'admin-cli')`)
	require.NoError(t, err)
	live, err := NewLeaseStore(db).Acquire(ctx, core.LeaseRun, "v12u-run-live",
		core.WorkerIdentity{InstanceID: "worker-live", Name: "live"}, upgradeReclaimTTL)
	require.NoError(t, err)
	require.Equal(t, int64(1), live.OwnershipEpoch)

	// ③ 首轮回收：三条回填租约都被合法领取（真接管）并收尾。
	worker := upgradeReclaimWorker()
	report := firstReclaimRound(t, db, worker, true)
	require.Equal(t, 1, report.Runs, "回填的 Run 必须被收尾")
	require.Equal(t, 1, report.Executions, "回填的执行行必须被收尾")
	require.Equal(t, 1, report.FlowRoots, "回填的 Flow 根必须被处理")

	// Run 收尾为 failed / worker_lost，执行者改写为 recovery（归属列不动）。
	var runStatus, runReason, runActor, runCaller string
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT status, reason, actor, caller_id FROM runs WHERE run_id = 'v12u-run-active'`).
		Scan(&runStatus, &runReason, &runActor, &runCaller))
	require.Equal(t, string(core.StatusFailed), runStatus)
	require.Equal(t, workerLostReason, runReason)
	require.Equal(t, string(core.ActorRecovery), runActor)
	require.Equal(t, "local-admin", runCaller, "收尾不得改写归属")

	// 执行行落四值域的 failed + worker_lost（无 Run 行的那一支）。
	var execStatus string
	var execErr sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT status, error_message FROM task_executions WHERE id = $1`, startedExecutionID).
		Scan(&execStatus, &execErr))
	require.Equal(t, string(core.ExecutionFailed), execStatus)
	require.Equal(t, workerLostReason, execErr.String)

	// 三类租约：属主换成新 Worker、代次 0 → 1、已交还。
	for _, want := range []struct {
		kind core.LeaseKind
		id   string
	}{
		{core.LeaseRun, "v12u-run-active"},
		{core.LeaseScheduleExecution, strconv.FormatInt(startedExecutionID, 10)},
		{core.LeaseFlowRoot, "v12u-root"},
	} {
		row, ok := readLease(t, ctx, db, string(want.kind), want.id)
		require.Truef(t, ok, "%s/%s 的租约行必须仍在", want.kind, want.id)
		require.Equal(t, worker.InstanceID, row.Owner, "%s/%s 必须由新 Worker 领取", want.kind, want.id)
		require.Equal(t, int64(1), row.Epoch, "%s/%s 的接管代次必须 0 → 1", want.kind, want.id)
		require.True(t, row.ReleasedAt.Valid, "%s/%s 收尾后必须交还租约", want.kind, want.id)
	}

	// Flow 根只交还执行权，**不**由回收器推进：状态与状态版本一字不动（推进归 Flow Worker）。
	var rootStatus string
	var rootStateVersion int64
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT status, state_version FROM flow_tasks WHERE task_id = 'v12u-root'`).
		Scan(&rootStatus, &rootStateVersion))
	require.Equal(t, "queued", rootStatus, "queued 根的交还支不推进状态")
	require.Equal(t, int64(1), rootStateVersion)

	// 收尾只作用于回填行：迁移后新建的活租约与它的 running 行一字不动。
	liveRow, ok := readLease(t, ctx, db, "run", "v12u-run-live")
	require.True(t, ok)
	require.Equal(t, "worker-live", liveRow.Owner)
	require.False(t, liveRow.ReleasedAt.Valid, "活租约不得被回收")
	require.Equal(t, int64(1), liveRow.Epoch)
	var liveStatus string
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT status FROM runs WHERE run_id = 'v12u-run-live'`).Scan(&liveStatus))
	require.Equal(t, string(core.StatusRunning), liveStatus, "活 Worker 的在跑行不得被改写")

	var total int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM execution_leases`).Scan(&total))
	require.Equal(t, 4, total, "三条回填 + 一条活租约，没有第二条")

	// ④ 第二轮：Run 与执行行已终态，不再被领取；queued 根按 `released_at` 谓词再领一次，
	//    但那是**交棒**（TookOver=false），不是接管——重复回收不产生新租约、不重复改状态。
	second := firstReclaimRound(t, db, worker, false)
	require.Zero(t, second.Runs)
	require.Zero(t, second.Executions)
	require.Equal(t, 1, second.FlowRoots)

	// ⑤ 重复迁移幂等：不新增租约、版本仍是当前支持版本。
	require.NoError(t, ensureSchema(ctx, db.pool, SupportedSchemaVersion), "重复执行必须是幂等 no-op")
	var afterRepeat int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM execution_leases`).Scan(&afterRepeat))
	require.Equal(t, total, afterRepeat, "重复迁移不得产生重复租约")
	repeatVersion, err := schemaVersion(ctx, db)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, repeatVersion)
}

// TestMigration0012_DiagnosticReasonCodeDomain 判 0012 把 `diagnostic_events` 的原因码
// 闭集扩展到 `recovery_unknown`（FR-24-26 / 模块卡 §2.3.5 A1）：迁移后新值可写、
// 既有值不受影响、未知值仍被 CHECK 拒绝（fail-closed）。
//
// 逆改点：删掉 0012 的 DROP/ADD CONSTRAINT → `recovery_unknown` 插入被旧闭集拒绝，
// 首条断言红；把 ADD 写成不做 DROP 的同名约束 → 迁移本身在 v11 库上报"已存在"；
// 去掉 CHECK 整条 → 第三条断言（未知值被拒）红。
// Reverse-edit points: dropping the widening rejects the new code; adding without the
// DROP fails the v11→v12 upgrade; removing the CHECK lets a bogus code through.
func TestMigration0012_DiagnosticReasonCodeDomain(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, 11))
	require.NoError(t, ensureSchema(ctx, conn, 12))

	// 最小合法行：除主键与时间外，锚点列（run_id）必须有一项非空。
	insert := func(eventID, reason string) error {
		_, err := conn.ExecContext(ctx, `
INSERT INTO diagnostic_events(event_id, schema_version, kind, occurred_at, run_id, attrs, reason_code)
VALUES ($1, 1, 'tool_call', 1000, 'v12-diag-run', '{}'::jsonb, $2)`, eventID, reason)
		return err
	}
	require.NoError(t, insert("ev_v12_recovery_unknown", "recovery_unknown"),
		"0012 必须把 recovery_unknown 纳入原因码闭集")
	require.NoError(t, insert("ev_v12_existing_code", "llm_call_failed"),
		"扩展只能是加宽：既有值域必须仍被接受")
	require.Error(t, insert("ev_v12_bogus_code", "bogus_reason"),
		"未知原因码必须继续被 CHECK 拒绝（fail-closed）")

	// 约束必须存在且处于 NOT VALID：历史行保持可读，判据只作用于新写入。
	var validated bool
	require.NoError(t, conn.QueryRowContext(ctx, `
SELECT convalidated FROM pg_constraint
 WHERE conrelid = 'diagnostic_events'::regclass AND conname = 'diagnostic_events_reason_code_check'`).
		Scan(&validated))
	require.False(t, validated, "约束保持 NOT VALID，与 0006 口径一致")
}
