//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `WorkerStore` 的**有库**用例（US-24 T006 / FR-24-01）：登记、诊断心跳、
// 幂等停机，以及"Name 允许重复、InstanceID 才是身份"这条边界。时间一律取数据库
// 时钟；需要检验"心跳确实刷新"时用 SQL 把旧时间压到 0 再触发，不靠 sleep。
// Database-backed acceptance for WorkerStore: register, diagnostic heartbeat, idempotent
// stop, and the "names repeat, instance ids do not" boundary.

// workerNodeRow 是一行登记快照（只取用例判定的列）。
type workerNodeRow struct {
	name          string
	hostname      string
	buildVersion  string
	schemaVersion int
	startedAt     int64
	heartbeatAt   int64
	stoppedAt     sql.NullInt64
}

// readWorkerNode 读回一行；行不存在直接红——用例不存在"可选缺行"的分支。
func readWorkerNode(t *testing.T, store *WorkerStore, instanceID string) workerNodeRow {
	t.Helper()
	var row workerNodeRow
	err := store.db.QueryRowContext(context.Background(), `
SELECT worker_name, hostname, build_version, schema_version, started_at,
       last_heartbeat_at, stopped_at
  FROM worker_nodes
 WHERE instance_id = $1`, instanceID).Scan(
		&row.name, &row.hostname, &row.buildVersion, &row.schemaVersion,
		&row.startedAt, &row.heartbeatAt, &row.stoppedAt)
	require.NoError(t, err)
	return row
}

// countWorkerNodes 数登记行（判"重复登记是覆盖，不是追加"）。
func countWorkerNodes(t *testing.T, store *WorkerStore) int {
	t.Helper()
	var n int
	require.NoError(t, store.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM worker_nodes`).Scan(&n))
	return n
}

// TestWorkerRegistry_RegisterHeartbeatStop 判登记写入全部诊断列、心跳只刷新心跳列、
// 停机幂等保留首次时间、停机后拒绝心跳、同实例重复登记清除停机标记，以及入口对
// 空标识 / 非正 schema 版本的拒绝。
//
// 逆改点：把心跳 UPDATE 顺带写 schema_version → 99 断言红；把 Stop 的 COALESCE
// 换成直接覆盖 → 幂等断言红；把"已停止"分支改成照常刷新 → 停机后心跳断言红；
// 去掉 Register 的 stopped_at = NULL → 复登断言红；去掉空标识 / 版本校验 →
// 对应拒绝断言红。
// Reverse-edit points: any diagnostic column touched by heartbeat, a non-idempotent
// stop, a heartbeat that revives a stopped row, or a re-register that keeps stopped_at
// turns the matching assertion red.
func TestWorkerRegistry_RegisterHeartbeatStop(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	worker := core.WorkerIdentity{
		InstanceID:    "wrk_alpha",
		Name:          "worker-alpha",
		Hostname:      "host-a",
		Version:       "0.0.0-test",
		SchemaVersion: SupportedSchemaVersion,
	}

	require.NoError(t, store.Register(ctx, worker))
	row := readWorkerNode(t, store, worker.InstanceID)
	require.Equal(t, "worker-alpha", row.name)
	require.Equal(t, "host-a", row.hostname)
	require.Equal(t, "0.0.0-test", row.buildVersion)
	require.Equal(t, SupportedSchemaVersion, row.schemaVersion, "schema 版本只在注册时写入")
	require.Positive(t, row.startedAt, "started_at 必须落数据库时钟")
	require.Positive(t, row.heartbeatAt, "注册即写首个心跳时间")
	require.False(t, row.stoppedAt.Valid, "新登记实例未停机")

	// 心跳只刷新 last_heartbeat_at：先把两列都改到可判定的值，再触发一次。
	_, err := store.db.ExecContext(ctx,
		`UPDATE worker_nodes SET last_heartbeat_at = 0, schema_version = 99 WHERE instance_id = $1`,
		worker.InstanceID)
	require.NoError(t, err)
	require.NoError(t, store.Heartbeat(ctx, worker.InstanceID))
	row = readWorkerNode(t, store, worker.InstanceID)
	require.Positive(t, row.heartbeatAt, "心跳必须刷新 last_heartbeat_at")
	require.Equal(t, 99, row.schemaVersion, "心跳不得改写在注册时写入的 schema 版本")

	err = store.Heartbeat(ctx, "wrk_missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "未登记")

	require.NoError(t, store.Stop(ctx, worker.InstanceID))
	row = readWorkerNode(t, store, worker.InstanceID)
	require.True(t, row.stoppedAt.Valid, "停机必须落 stopped_at")

	// 幂等：先把停机时间改成可判定的值，第二次 Stop 不得覆盖。
	_, err = store.db.ExecContext(ctx,
		`UPDATE worker_nodes SET stopped_at = 12345 WHERE instance_id = $1`, worker.InstanceID)
	require.NoError(t, err)
	require.NoError(t, store.Stop(ctx, worker.InstanceID), "重复停机是幂等 no-op")
	row = readWorkerNode(t, store, worker.InstanceID)
	require.Equal(t, int64(12345), row.stoppedAt.Int64, "重复 Stop 保留首次 stopped_at")

	err = store.Heartbeat(ctx, worker.InstanceID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "已停止")

	// 同实例重新登记（进程重启复用标识）：覆盖诊断列、清除停机标记、不新增行。
	worker.Hostname = "host-a-reborn"
	worker.Version = "0.0.1-test"
	require.NoError(t, store.Register(ctx, worker))
	require.Equal(t, 1, countWorkerNodes(t, store), "重复登记必须是覆盖")
	row = readWorkerNode(t, store, worker.InstanceID)
	require.False(t, row.stoppedAt.Valid, "重新登记视为重新上线")
	require.Equal(t, "host-a-reborn", row.hostname)
	require.Equal(t, "0.0.1-test", row.buildVersion)

	err = store.Stop(ctx, "wrk_missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "未登记")

	require.Error(t, store.Register(ctx, core.WorkerIdentity{Name: "no-id", SchemaVersion: SupportedSchemaVersion}),
		"空实例标识必须在入口拒绝")
	require.Error(t, store.Register(ctx, core.WorkerIdentity{InstanceID: "wrk_no_schema", Name: "no-schema"}),
		"非正 schema 版本必须在入口拒绝")
}

// TestWorkerRegistry_DuplicateNameAllowed 判 Name 只是诊断列：两个实例共用同一个
// worker_name 都能落库、心跳互不覆盖、重复登记同一 instance_id 仍是覆盖。
//
// 逆改点：把 worker_name 当唯一键（唯一索引 / 按名 upsert）→ 第二条登记红；
// 让心跳按 worker_name 匹配 → 另一行的心跳时间被顺带刷新，断言红。
// Reverse-edit points: treating worker_name as a key, or matching heartbeats by name,
// turns the duplicate-name assertions red.
func TestWorkerRegistry_DuplicateNameAllowed(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	alpha := core.WorkerIdentity{
		InstanceID: "wrk_dup_a", Name: "shared-name", Hostname: "host-a",
		Version: "v1", SchemaVersion: SupportedSchemaVersion,
	}
	beta := core.WorkerIdentity{
		InstanceID: "wrk_dup_b", Name: "shared-name", Hostname: "host-b",
		Version: "v2", SchemaVersion: SupportedSchemaVersion,
	}
	require.NoError(t, store.Register(ctx, alpha))
	require.NoError(t, store.Register(ctx, beta), "重复 Worker Name 必须允许")
	require.Equal(t, 2, countWorkerNodes(t, store))
	require.Equal(t, "shared-name", readWorkerNode(t, store, alpha.InstanceID).name)
	require.Equal(t, "shared-name", readWorkerNode(t, store, beta.InstanceID).name)

	// 只给 alpha 心跳：beta 的行一个字节都不应被动到。
	_, err := store.db.ExecContext(ctx,
		`UPDATE worker_nodes SET last_heartbeat_at = 0 WHERE instance_id IN ($1, $2)`,
		alpha.InstanceID, beta.InstanceID)
	require.NoError(t, err)
	require.NoError(t, store.Heartbeat(ctx, alpha.InstanceID))
	require.Positive(t, readWorkerNode(t, store, alpha.InstanceID).heartbeatAt)
	require.Zero(t, readWorkerNode(t, store, beta.InstanceID).heartbeatAt,
		"按 instance_id 匹配，同名的另一实例不得被顺带刷新")

	// 同实例重复登记仍是覆盖：行数不变，只有该实例的列被更新。
	alpha.Version = "v3"
	require.NoError(t, store.Register(ctx, alpha))
	require.Equal(t, 2, countWorkerNodes(t, store))
	require.Equal(t, "v3", readWorkerNode(t, store, alpha.InstanceID).buildVersion)
	require.Equal(t, "v2", readWorkerNode(t, store, beta.InstanceID).buildVersion)
}
