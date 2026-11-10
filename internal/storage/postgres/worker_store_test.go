//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

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

// 本段是 US-25C 概览读数的有库用例（模块卡 §2.3.9 / FR-25C-30 / FR-25C-31）：
// 三类状态、全库计数与有界明细来自同一条语句同一快照，逐行阈值取该行自己的心跳
// 间隔，旧行回退部署默认。状态一律由数据库时钟判定，页面不得用浏览器时钟重算。
// US-25C overview acceptance: three server-computed states, database-wide counts, a
// bounded detail page from one snapshot, per-row thresholds, and legacy-row fallback.

// seedOverviewRow 插入一行登记快照，心跳时间按"距今 age"倒推；interval 为 nil 时写
// NULL（模拟迁移 0013 之前的旧行）。
// seedOverviewRow inserts one diagnostic row; a nil interval writes NULL (pre-0013 row).
func seedOverviewRow(t *testing.T, store *WorkerStore, instanceID, name string, interval *int, age time.Duration, stopped bool) {
	t.Helper()
	_, err := store.db.ExecContext(context.Background(), `
INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
                         started_at, last_heartbeat_at, stopped_at, heartbeat_interval_seconds)
VALUES ($1, $2, 'host-test', 'v-test', $3,
        (extract(epoch FROM clock_timestamp()) * 1000)::bigint - 5000,
        (extract(epoch FROM clock_timestamp()) * 1000)::bigint - $4,
        CASE WHEN $5::boolean
             THEN (extract(epoch FROM clock_timestamp()) * 1000)::bigint
             ELSE NULL END,
        $6)`,
		instanceID, name, SupportedSchemaVersion, age.Milliseconds(), stopped, interval)
	require.NoError(t, err)
}

// TestWorkerOverview_StatusBuckets 判三类服务端状态的判定与排序：心跳在阈值内是
// active，超过阈值是 stale，有 stopped_at 一律 stopped（哪怕心跳刚刚刷新）；明细按
// active → stale → stopped、同组内心跳新的在前排列。
//
// 逆改点：让 stopped 被心跳"复活"（先判心跳再判 stopped_at）→ 停止行断言红；
// 去掉逐行阈值直接用固定 30s → 阈值用例红；排序去掉状态优先级 → 顺序断言红。
// Reverse-edit points: a stopped row revived by a fresh heartbeat, a fixed threshold,
// or a status-agnostic ordering all turn the matching assertion red.
func TestWorkerOverview_StatusBuckets(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	ten := 10 // 阈值 max(3×10, 30) = 30s
	seedOverviewRow(t, store, "wrk_active_new", "worker-a", &ten, 1*time.Second, false)
	seedOverviewRow(t, store, "wrk_active_old", "worker-a", &ten, 20*time.Second, false)
	seedOverviewRow(t, store, "wrk_stale", "worker-b", &ten, 90*time.Second, false)
	seedOverviewRow(t, store, "wrk_stopped", "worker-c", &ten, 1*time.Second, true)

	overview, err := store.ReadWorkerOverview(ctx, 200)
	require.NoError(t, err)
	require.False(t, overview.AsOf.IsZero(), "快照时刻必须由数据库给出")
	require.Equal(t, core.WorkerOverviewCounts{Active: 2, Stale: 1, Stopped: 1}, overview.Counts)
	require.Equal(t, 4, overview.Total, "total 是三类计数之和")
	require.False(t, overview.Truncated)

	require.Len(t, overview.Nodes, 4)
	order := make([]string, 0, len(overview.Nodes))
	for _, node := range overview.Nodes {
		order = append(order, node.InstanceID)
	}
	require.Equal(t, []string{"wrk_active_new", "wrk_active_old", "wrk_stale", "wrk_stopped"}, order,
		"排序固定 active → stale → stopped，同组内 last_heartbeat_at DESC")

	status := map[string]string{}
	for _, node := range overview.Nodes {
		status[node.InstanceID] = node.Status
	}
	require.Equal(t, "active", status["wrk_active_new"])
	require.Equal(t, "active", status["wrk_active_old"], "20s < 30s 阈值仍是 active")
	require.Equal(t, "stale", status["wrk_stale"], "90s > 30s 阈值是 stale（待确认，不是已死）")
	require.Equal(t, "stopped", status["wrk_stopped"], "stopped_at 优先于心跳新鲜度")

	require.InDelta(t, 90, overview.Nodes[2].HeartbeatAgeSeconds, 5, "心跳年龄按数据库时钟算")
	require.NotNil(t, overview.Nodes[0].HeartbeatIntervalSeconds)
	require.Equal(t, 10, *overview.Nodes[0].HeartbeatIntervalSeconds)
}

// TestWorkerOverview_CountsAreAggregates 判计数是全库聚合、明细是有界页：250 行只
// 返回 10 行明细，但三类计数与 total 必须仍是 250，且 truncated 如实为 true——计数
// 不得由明细推算。
//
// 逆改点：用 len(rows) 当计数 → 250 断言红；去掉 LIMIT → truncated 断言红；
// 把 total 写成"未 stopped 数"→ total 断言红。
// Reverse-edit points: deriving counts from the returned page, dropping the LIMIT, or
// redefining total as "not stopped" all turn the matching assertion red.
func TestWorkerOverview_CountsAreAggregates(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	_, err := store.db.ExecContext(ctx, `
INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
                         started_at, last_heartbeat_at, stopped_at, heartbeat_interval_seconds)
SELECT 'wrk_bulk_' || g, 'worker-bulk', 'host-bulk', 'v-bulk', $1,
       (extract(epoch FROM clock_timestamp()) * 1000)::bigint - 5000,
       (extract(epoch FROM clock_timestamp()) * 1000)::bigint - (g % 50),
       NULL, 10
  FROM generate_series(1, 250) AS g`, SupportedSchemaVersion)
	require.NoError(t, err)

	overview, err := store.ReadWorkerOverview(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 250, overview.Counts.Active, "计数是全库聚合，不是明细行数")
	require.Equal(t, 0, overview.Counts.Stale)
	require.Equal(t, 0, overview.Counts.Stopped)
	require.Equal(t, 250, overview.Total)
	require.Len(t, overview.Nodes, 10, "明细按 limit 有界")
	require.True(t, overview.Truncated, "total > len(nodes) 必须如实标注截断")
}

// TestWorkerOverview_CountsAndDetailSameSnapshot 判"计数、明细与全库活跃租约数同一
// 条语句同一快照"：读取进行中从另一连接提交新行 / 新租约，本次读数不得看见它们
// （快照已冻结），下一次读数必须看见。判定方式是观察 pg_stat_activity 确认语句在
// 服务端处理，再插入。注意 extended protocol 首次执行要先走 Parse/Describe 往返：
// `active` 可能出现在执行期快照之前，此时插入会被本次读数看见——那不是产品缺陷，
// 而是没有构造出"中途"窗口。因此每轮先断言三个读数自洽（明细条数 = min(总数,
// limit)、租约数与提交位置一致），未命中窗口就清理中途行后重试，最多 4 轮；分步
// 读取的实现会在自洽断言或租约断言上立即红。租约用不带 worker_nodes 行的
// `legacy-owner` 播种：全库数必须直接聚合 `execution_leases`，按节点求和会把它算成 0。
//
// 逆改点：拆成"先查计数、再查明细（带 LIMIT）"两条语句 → 中途插入的行会进计数
// 却不进明细（或反之），自洽断言红；把全库租约数留在第二条语句（或按节点求和）
// → ActiveLeases 断言红。
// Reverse-edit points: splitting counts and details into two statements lets the
// mid-statement row land in one but not the other; a second-statement or per-node
// lease total turns the ActiveLeases assertions red.
func TestWorkerOverview_CountsAndDetailSameSnapshot(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	const seedRows = 30000
	_, err := store.db.ExecContext(ctx, `
INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
                         started_at, last_heartbeat_at, stopped_at, heartbeat_interval_seconds)
SELECT 'wrk_seed_' || g, 'worker-seed', 'host-seed', 'v-seed', $1,
       (extract(epoch FROM clock_timestamp()) * 1000)::bigint - 5000,
       (extract(epoch FROM clock_timestamp()) * 1000)::bigint - (g % 40),
       NULL, 10
  FROM generate_series(1, $2) AS g`, SupportedSchemaVersion, seedRows)
	require.NoError(t, err)

	// 两条没有登记行的 legacy 租约：它们证明全库活跃租约数是直接聚合
	// `execution_leases`（§2.3.9 L918-919），不是 Nodes 各行之和。
	_, err = store.db.ExecContext(ctx, `
INSERT INTO execution_leases(kind, resource_id, owner_worker_id, ownership_epoch,
                             acquired_at, renewed_at, expires_at, released_at)
SELECT 'run', 'lease_seed_' || g, 'legacy-owner', 0, t.ms, t.ms, t.ms + 3600000, NULL
  FROM generate_series(1, 2) AS g
 CROSS JOIN (SELECT (extract(epoch FROM clock_timestamp()) * 1000)::bigint AS ms) t`)
	require.NoError(t, err)

	type readResult struct {
		overview core.WorkerOverview
		err      error
	}

	limit := seedRows + 1
	// 预热一次：让连接池里的连接先完成 Parse/Describe，缩小"active 但尚未取快照"
	// 的窗口，让"中途插入"更容易命中。
	_, err = store.ReadWorkerOverview(ctx, 10)
	require.NoError(t, err)

	accepted := false
	for attempt := 1; attempt <= 4 && !accepted; attempt++ {
		midWorker := fmt.Sprintf("wrk_mid_statement_%d", attempt)
		midLease := fmt.Sprintf("lease_mid_statement_%d", attempt)
		done := make(chan readResult, 1)
		go func() {
			overview, err := store.ReadWorkerOverview(context.Background(), limit)
			done <- readResult{overview: overview, err: err}
		}()

		// 轮询确认概览语句在服务端处理（本测试进程的其他连接除外）。
		observed := false
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var running bool
			require.NoError(t, store.db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM pg_stat_activity
     WHERE pid <> pg_backend_pid()
       AND state = 'active'
       AND query LIKE '%WITH bounds AS MATERIALIZED%'
       AND query NOT LIKE '%pg_stat_activity%'
)`).Scan(&running))
			if running {
				observed = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		require.True(t, observed, "必须观察到概览语句在服务端执行；否则用例没有判到快照边界")

		// 在语句处理期间提交的新行与新租约：**同一事务**提交。分开提交会留出一个
		// 「worker 行已可见、租约尚未可见」的窗口——此时 Total = seedRows+1 而
		// ActiveLeases = 2 是**同一快照**的合法读数，却会被下面的「两者都在」断言
		// 误判为红（2026-11-10 全量 test-pg 实测复现）。同事务提交让快照只能看见
		// 两者或都看不见。
		// Both mid rows commit atomically; otherwise a legal single-snapshot reading
		// between the two commits would trip the "saw both" assertion.
		ten := 10
		_, err = store.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
                         started_at, last_heartbeat_at, stopped_at, heartbeat_interval_seconds)
VALUES ($1, 'worker-mid', 'host-test', 'v-test', $2,
        (extract(epoch FROM clock_timestamp()) * 1000)::bigint - 5000,
        (extract(epoch FROM clock_timestamp()) * 1000)::bigint,
        NULL, $3)`, midWorker, SupportedSchemaVersion, ten); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
INSERT INTO execution_leases(kind, resource_id, owner_worker_id, ownership_epoch,
                             acquired_at, renewed_at, expires_at, released_at)
SELECT 'run', $1, 'legacy-owner', 0, t.ms, t.ms, t.ms + 3600000, NULL
  FROM (SELECT (extract(epoch FROM clock_timestamp()) * 1000)::bigint AS ms) t`, midLease)
			return err
		})
		require.NoError(t, err)

		result := <-done
		require.NoError(t, result.err)
		overview := result.overview
		// 三个读数必须同源：明细条数 = min(总数, limit)。分步读取在插入落进两次读
		// 之间时，计数与明细会在这里分叉。
		require.Equal(t, min(overview.Total, limit), len(overview.Nodes),
			"明细条数与总数必须自洽（同一快照）；分步读取会在这里分叉")
		if overview.Total == seedRows {
			// 本轮插入确实落在执行期快照之后：卡面要求的"中途插入不受影响"成立。
			require.EqualValues(t, 2, overview.ActiveLeases,
				"全库活跃租约数必须来自同一语句快照：快照后提交的租约不得计入，legacy 持有者必须计入")
			accepted = true
			continue
		}
		// 快照落在两行同事务提交之后（解析 / 绑定与执行之间的调度窗口）：本轮没有
		// 构造出"中途"窗口。三个读数仍须一致地看见中途行（同事务提交保证二者同现），
		// 否则分步读取已暴露；清理后重试。
		require.Equal(t, seedRows+1, overview.Total, "每轮读数只能看到快照位置决定的那一态")
		require.EqualValues(t, 3, overview.ActiveLeases,
			"快照在提交之后的读数必须一致地看见中途行：计数与租约不得来自不同快照")
		_, err = store.db.ExecContext(ctx, `DELETE FROM execution_leases WHERE resource_id = $1`, midLease)
		require.NoError(t, err)
		_, err = store.db.ExecContext(ctx, `DELETE FROM worker_nodes WHERE instance_id = $1`, midWorker)
		require.NoError(t, err)
	}
	require.True(t, accepted, "4 轮都未构造出语句执行中途的插入窗口；调大 seedRows 或检查语句执行时长")

	next, err := store.ReadWorkerOverview(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, seedRows+1, next.Total, "下一次读数必须看见已提交的新行")
	require.True(t, next.Truncated)
	require.EqualValues(t, 3, next.ActiveLeases, "下一次读数必须看见已提交的新租约")
}

// TestWorkerOverview_SameNameKeptSeparate 判身份是 instance_id：两个实例共用同一个
// worker_name 各自成行、各自计数，页面不得按名称合并，也不得由行数推断物理机数。
//
// 逆改点：按 worker_name 分组 / 去重 → 行数与 total 断言红。
// Reverse-edit points: grouping rows by worker_name turns the counts red.
func TestWorkerOverview_SameNameKeptSeparate(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	ten := 10
	seedOverviewRow(t, store, "wrk_same_a", "worker-shared", &ten, 1*time.Second, false)
	seedOverviewRow(t, store, "wrk_same_b", "worker-shared", &ten, 2*time.Second, false)

	overview, err := store.ReadWorkerOverview(ctx, 200)
	require.NoError(t, err)
	require.Equal(t, 2, overview.Total, "同名实例各自计数")
	require.Len(t, overview.Nodes, 2)
	require.Equal(t, "worker-shared", overview.Nodes[0].Name)
	require.Equal(t, "worker-shared", overview.Nodes[1].Name)
	require.NotEqual(t, overview.Nodes[0].InstanceID, overview.Nodes[1].InstanceID, "同名不是同进程")
}

// TestWorkerOverview_PerInstanceThreshold 判阈值按行计算：同样 45s 未心跳，间隔
// 10s 的实例是 stale（阈值 30s），间隔 60s 的实例是 active（阈值 180s）——不得拿
// 管理台进程自己的间隔套给所有实例。
//
// 逆改点：改用单一部署阈值 → 慢心跳行被误判 stale，本用例红。
// Reverse-edit points: a single deployment-wide threshold misjudges the slow-beat row.
func TestWorkerOverview_PerInstanceThreshold(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	ten, sixty := 10, 60
	seedOverviewRow(t, store, "wrk_fast", "worker-fast", &ten, 45*time.Second, false)
	seedOverviewRow(t, store, "wrk_slow", "worker-slow", &sixty, 45*time.Second, false)

	overview, err := store.ReadWorkerOverview(ctx, 200)
	require.NoError(t, err)
	require.Equal(t, core.WorkerOverviewCounts{Active: 1, Stale: 1, Stopped: 0}, overview.Counts)
	status := map[string]string{}
	for _, node := range overview.Nodes {
		status[node.InstanceID] = node.Status
	}
	require.Equal(t, "stale", status["wrk_fast"], "3×10s=30s 阈值：45s 未心跳是 stale")
	require.Equal(t, "active", status["wrk_slow"], "3×60s=180s 阈值：45s 未心跳仍 active")
}

// TestWorkerOverview_LegacyRowUsesDeploymentDefault 判旧行（间隔列为 NULL）回退构造
// 期注入的部署默认，并在明细里把间隔置空（未知），而不是伪造 0。同一个 100s 的
// 心跳年龄在部署默认 60s 下是 active（阈值 180s），在默认 10s 下是 stale（阈值 30s）。
//
// 逆改点：旧行间隔填 0 → nil 断言红；不注入默认、写死 10 → 默认 60 的 store 红。
// Reverse-edit points: fabricating a 0 interval, or ignoring the injected default,
// turns the legacy assertions red.
func TestWorkerOverview_LegacyRowUsesDeploymentDefault(t *testing.T) {
	db := newTxTestDB(t)
	slow := NewWorkerStore(db, WithDefaultHeartbeatInterval(60))
	seedOverviewRow(t, slow, "wrk_legacy", "worker-legacy", nil, 100*time.Second, false)

	overview, err := slow.ReadWorkerOverview(context.Background(), 200)
	require.NoError(t, err)
	require.Equal(t, 60, slow.DefaultHeartbeatIntervalSeconds())
	require.Equal(t, core.WorkerOverviewCounts{Active: 1}, overview.Counts,
		"部署默认 60s → 阈值 180s，100s 未心跳仍 active")
	require.Len(t, overview.Nodes, 1)
	require.Nil(t, overview.Nodes[0].HeartbeatIntervalSeconds, "旧行没有登记间隔，不得伪造 0")

	fast := NewWorkerStore(db, WithDefaultHeartbeatInterval(10))
	overview, err = fast.ReadWorkerOverview(context.Background(), 200)
	require.NoError(t, err)
	require.Equal(t, core.WorkerOverviewCounts{Stale: 1}, overview.Counts,
		"部署默认 10s → 阈值 30s，同一行变 stale")
	require.Nil(t, overview.Nodes[0].HeartbeatIntervalSeconds)
}

// TestRegister_PersistsHeartbeatInterval 判登记把实例自报的间隔落库：正数原样写入，
// 0 写 NULL（迁移前旧行语义），重复登记覆盖间隔列——只改间隔、不改身份与心跳列。
//
// 逆改点：忽略 HeartbeatIntervalSeconds → 25 断言红；把 0 写成 0 而不是 NULL →
// NULL 断言红（CHECK 也会拒绝 0）。
// Reverse-edit points: dropping the column from the upsert, or storing 0 instead of
// NULL, turns the persistence assertions red.
func TestRegister_PersistsHeartbeatInterval(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	worker := core.WorkerIdentity{
		InstanceID: "wrk_interval", Name: "worker-interval", Hostname: "host-i",
		Version: "v-test", SchemaVersion: SupportedSchemaVersion,
		HeartbeatIntervalSeconds: 25,
	}
	require.NoError(t, store.Register(ctx, worker))
	var interval sql.NullInt64
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT heartbeat_interval_seconds FROM worker_nodes WHERE instance_id = $1`,
		worker.InstanceID).Scan(&interval))
	require.True(t, interval.Valid)
	require.Equal(t, int64(25), interval.Int64)

	worker.HeartbeatIntervalSeconds = 0
	require.NoError(t, store.Register(ctx, worker))
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT heartbeat_interval_seconds FROM worker_nodes WHERE instance_id = $1`,
		worker.InstanceID).Scan(&interval))
	require.False(t, interval.Valid, "0 写 NULL：迁移前旧行语义，不是伪造 0")

	worker.HeartbeatIntervalSeconds = 5
	require.NoError(t, store.Register(ctx, worker))
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT heartbeat_interval_seconds FROM worker_nodes WHERE instance_id = $1`,
		worker.InstanceID).Scan(&interval))
	require.Equal(t, int64(5), interval.Int64, "重复登记覆盖间隔列")
	require.Equal(t, 1, countWorkerNodes(t, store))
}

// TestRegister_RejectsInvalidInterval 判负数在应用层入口就被拒绝、零行落库；绕过
// 应用层的负数 INSERT 由数据库 CHECK 兜底——装配缺陷不得等到写库才暴露，也不得
// 静默落成"永远 stale"的诊断行。
//
// 逆改点：去掉应用层校验 → 入口错误断言红（错误信息不再含"心跳间隔"）。
// Reverse-edit points: dropping the entry guard lets the raw CHECK surface instead and
// turns the entry-level assertion red.
func TestRegister_RejectsInvalidInterval(t *testing.T) {
	store := NewWorkerStore(newTxTestDB(t))
	ctx := context.Background()
	err := store.Register(ctx, core.WorkerIdentity{
		InstanceID: "wrk_negative", Name: "worker-negative",
		SchemaVersion: SupportedSchemaVersion, HeartbeatIntervalSeconds: -1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "心跳间隔", "负数必须在应用层拒绝")
	require.Equal(t, 0, countWorkerNodes(t, store), "被拒绝的登记不得落库")

	_, err = store.db.ExecContext(ctx, `
INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
                         started_at, last_heartbeat_at, stopped_at, heartbeat_interval_seconds)
VALUES ('wrk_raw_negative', 'worker-raw', 'host', 'v', $1, 1, 1, NULL, -1)`,
		SupportedSchemaVersion)
	require.Error(t, err)
	require.Contains(t, err.Error(), "worker_nodes_heartbeat_interval_seconds_check",
		"数据库 CHECK 是最后一道兜底")
}
