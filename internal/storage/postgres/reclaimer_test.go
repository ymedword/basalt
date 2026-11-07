//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是**作用域回收**的有库验收（US-24 T017 / FR-24-14 / FR-24-15 / FR-24-16 /
// C-06 / C-08 / 模块卡 §4.4.4）。
//
// 三条被固定的边界：
//
//  1. 回收只作用于**已过期或已释放**的租约；心跳 / 启动时间再旧也不构成死亡证据
//     （C-06）。用例把活租约的 `worker_nodes.last_heartbeat_at` 故意留在很久以前，
//     过期租约的心跳反而是刚刷新的——两个方向都只由 `expires_at` 决定结果。
//  2. `schedule_execution` 过期后执行行必须离开 `started`，且状态只落四值域
//     （`limit_reached → completed`、无 Run → `failed + worker_lost`，FR-24-14 / FR-24-39）。
//  3. 审计 `pending` 归一化只覆盖**本次合法领取的 run_id 集合**；集合外的 pending 行
//     一字不改（FR-24-16 / C-08）。
//
// 组合面（A1 → A2 → A3 → B1 的完整一轮）由 `internal/app` 的 `ReclaimOnce` 用例覆盖：
// `internal/app` 依赖本包，本包测试不能反向 import 装配层（会成环），所以这里按模块卡
// §2.3.5 的固定配对直接调用作用域原语。该落位适配已登记 run-state §1.3。
// Composition (the full A1→B1 round) is covered from internal/app; this package cannot
// import the assembly layer without an import cycle, so it drives the same primitives
// in their documented order.
//
// 逆改点：把领取 SQL 的 `expires_at <= db_ms` 去掉 → 活租约被收尾，用例 1 红；
// 把归一化语句的 `run_id = ANY($1)` 去掉 → 集合外的 pending 变 unknown，用例 3 红；
// 把 `schedule_execution` 域整段删掉 → 执行行永远 started，用例 2 红。
// Reverse edits: dropping the expiry predicate, the run-id scope, or the whole
// schedule_execution domain turns exactly one case red.

const (
	// reclaimTestTTL 是回收器领取后拿到的有效期：足够长，不会被用例自身耗时撞到过期。
	reclaimTestTTL = 30 * time.Second

	// reclaimWorkerInstance 是回收器用例固定使用的实例标识。
	reclaimWorkerInstance = "worker-reclaimer"
)

// reclaimIdentity 取回收器的 Worker 身份。
func reclaimIdentity() core.WorkerIdentity {
	return core.WorkerIdentity{InstanceID: reclaimWorkerInstance, Name: "reclaimer"}
}

// seedReclaimRun 建会话 + `running` 的 Run 行，并由 owner 领到 run 租约。
// 夹具直接写行：回收用例判的是"已存在的运行行如何被收尾"，不走准入。
func seedReclaimRun(t *testing.T, db *DB, runID, sessionID, owner string) core.Lease {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	_, err := db.pool.ExecContext(ctx, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at, caller_id)
		VALUES ($1, 'demo', 'web', 'u1', 'active', 0, $2, $2, 'c1')`, sessionID, now)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, $2, 'demo', 'web', 'u1', 'user', 'running', $3,
		        'local-admin', 'admin-builtin', NULL, 'admin-cli')`, runID, sessionID, now)
	require.NoError(t, err)
	return mustAcquire(t, NewLeaseStore(db), core.LeaseRun, runID, owner)
}

// seedHeartbeat 写一行 Worker 登记行；`heartbeatAgo` 决定 `last_heartbeat_at` 离现在多远。
// 心跳只作诊断：无论新旧都不参与"谁还活着"的判定（FR-24-01 / C-06）。
func seedHeartbeat(t *testing.T, db *DB, instanceID string, heartbeatAgo time.Duration) {
	t.Helper()
	at := time.Now().UTC().Add(-heartbeatAgo).UnixMilli()
	_, err := db.pool.ExecContext(context.Background(), `
INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
                         started_at, last_heartbeat_at)
VALUES ($1, $1, 'host', 'test', $2, $3, $3)`,
		instanceID, SupportedSchemaVersion, at)
	require.NoError(t, err)
}

// recoverRunDomain 执行 run 域的一轮：ClaimExpired → 逐条 RecoverRun（模块卡 §2.3.5 A1）。
func recoverRunDomain(t *testing.T, db *DB, limit int) []core.Lease {
	t.Helper()
	ctx := context.Background()
	claims, err := NewReclaimerStore(db).ClaimExpired(ctx, core.LeaseRun,
		reclaimIdentity(), limit, reclaimTestTTL)
	require.NoError(t, err)
	for _, lease := range claims {
		_, err := RecoverRun(ctx, db, slog.Default(), lease.LeaseToken, time.Now().UTC())
		require.NoError(t, err)
	}
	return claims
}

// TestReclaimer_DoesNotTouchLiveLease 判"活租约零改动、过期租约被合法收尾"
// （模块卡 §4.4.4 / C-06 / FR-24-15）。
//
// 两种方向的对照都在同一个夹具里：活租约的心跳**很久以前**、过期租约的心跳**刚刚**；
// 如果实现按心跳推断死亡，两个断言会同时翻转成相反的结论。
func TestReclaimer_DoesNotTouchLiveLease(t *testing.T) {
	db := newTxTestDB(t)
	live := seedReclaimRun(t, db, "run-live", "s-live", "worker-live")
	seedReclaimRun(t, db, "run-dead", "s-dead", "worker-dead")
	seedHeartbeat(t, db, "worker-live", 24*time.Hour) // 心跳很旧，但租约仍有效
	seedHeartbeat(t, db, "worker-dead", time.Second)  // 心跳很新，但租约已过期
	require.Equal(t, int64(1), live.OwnershipEpoch, "夹具断言：首次领取是 epoch 1")

	expireLease(t, NewLeaseStore(db), core.LeaseRun, "run-dead")

	claims := recoverRunDomain(t, db, 32)
	require.Len(t, claims, 1, "只有过期租约被领取")
	require.Equal(t, "run-dead", claims[0].ResourceID)
	require.True(t, claims[0].TookOver, "过期未释放是接管")

	require.Equal(t, string(core.StatusRunning), runStatusOf(t, db, "run-live"),
		"活租约不得被回收（心跳陈旧不构成死亡证据）")
	require.Equal(t, string(core.StatusFailed), runStatusOf(t, db, "run-dead"))
	require.Equal(t, workerLostReason, runReasonOf(t, db, "run-dead"))
	requireLeaseReleased(t, db, core.LeaseRun, "run-live", false)
	requireLeaseReleased(t, db, core.LeaseRun, "run-dead", true)
}

// TestReclaimer_ClosesExecutionAfterLeaseExpiry 判 `schedule_execution` 域崩溃回收
// （模块卡 §2.3.5 A2 / FR-24-14 / FR-24-39）：过期执行行不得永远 `started`，状态只落
// 四值域；仍有效的执行租约一字不动。
func TestReclaimer_ClosesExecutionAfterLeaseExpiry(t *testing.T) {
	db := newTxTestDB(t)
	ctx := context.Background()
	worker := core.WorkerIdentity{InstanceID: "worker-a", Name: "worker-a"}
	tasks := NewTaskStore(db)

	taskPortSeedTask(t, db, "task-missing-run", "demo", "*/5 * * * * *", "m1")
	taskPortSeedTask(t, db, "task-completed-run", "demo", "*/5 * * * * *", "m2")
	taskPortSeedTask(t, db, "task-live-run", "demo", "*/5 * * * * *", "m3")

	// ① 无 Run：执行者失联且无从查证 → failed + worker_lost。
	missingID, _, err := tasks.StartExecution(ctx, core.ExecutionRecord{
		TaskID: "task-missing-run", RunID: "run-does-not-exist", Trigger: core.TriggerManual,
		StartedAt: time.Now().UTC(),
	}, worker, reclaimTestTTL)
	require.NoError(t, err)

	// ② Run 已达标完成：执行行按四值域记 completed，不是故障。
	now := time.Now().UTC().UnixMilli()
	_, err = db.pool.ExecContext(ctx, `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at, finished_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ('run-completed', NULL, 'demo', 'scheduler', 'scheduler', 'manual', 'completed', $1, $1,
		        'local-admin', 'admin-builtin', NULL, 'scheduler')`, now)
	require.NoError(t, err)
	completedID, _, err := tasks.StartExecution(ctx, core.ExecutionRecord{
		TaskID: "task-completed-run", RunID: "run-completed", Trigger: core.TriggerManual,
		StartedAt: time.Now().UTC(),
	}, worker, reclaimTestTTL)
	require.NoError(t, err)

	// ③ 活租约：租约未过期，回收器不得碰。
	_, _, err = tasks.StartExecution(ctx, core.ExecutionRecord{
		TaskID: "task-live-run", RunID: "run-live-exec", Trigger: core.TriggerManual,
		StartedAt: time.Now().UTC(),
	}, worker, reclaimTestTTL)
	require.NoError(t, err)

	expireLease(t, NewLeaseStore(db), core.LeaseScheduleExecution, executionResourceID(missingID))
	expireLease(t, NewLeaseStore(db), core.LeaseScheduleExecution, executionResourceID(completedID))

	claims, err := NewReclaimerStore(db).ClaimExpired(ctx, core.LeaseScheduleExecution,
		reclaimIdentity(), 32, reclaimTestTTL)
	require.NoError(t, err)
	require.Len(t, claims, 2, "只领取过期的执行租约")
	for _, lease := range claims {
		status, err := RecoverExecution(ctx, db, lease.LeaseToken, time.Now().UTC())
		require.NoError(t, err)
		require.NotEmpty(t, status, "每一笔过期执行行都必须被收尾")
		if lease.ResourceID == executionResourceID(missingID) {
			require.Equal(t, core.ExecutionFailed, status)
		} else {
			require.Equal(t, core.ExecutionCompleted, status, "limit_reached / completed 都落 completed")
		}
	}

	requireExecutionRow(t, db, missingID, string(core.ExecutionFailed), workerLostReason)
	requireExecutionRow(t, db, completedID, string(core.ExecutionCompleted), "")
	requireExecutionStillStarted(t, db, "task-live-run", "活租约的执行行不得被收尾")
	requireLeaseReleased(t, db, core.LeaseScheduleExecution, executionResourceID(missingID), true)
}

// TestReclaimer_AuditNormalizationScopedToClaimedRuns 判审计归一化的作用域
// （模块卡 §2.3.5 A1 步骤 ① / FR-24-16 / C-08）：本次领取的 Run 的 pending 行归一化为
// `unknown`，集合外的 pending 行**一字不改**。
func TestReclaimer_AuditNormalizationScopedToClaimedRuns(t *testing.T) {
	db := newTxTestDB(t)
	seedReclaimRun(t, db, "run-claimed", "s-claimed", "worker-claimed")
	seedReclaimRun(t, db, "run-other", "s-other", "worker-other")
	seedPendingAudit(t, db, "run-claimed")
	seedPendingAudit(t, db, "run-other")

	expireLease(t, NewLeaseStore(db), core.LeaseRun, "run-claimed")
	claims := recoverRunDomain(t, db, 32)
	require.Len(t, claims, 1)
	require.Equal(t, "run-claimed", claims[0].ResourceID)

	requireAuditNormalized(t, db, "run-claimed")
	requireAuditStillPending(t, db, "run-other")
}

// seedPendingAudit 给一个 Run 各写一条 pending 的模型调用与工具调用审计行。
func seedPendingAudit(t *testing.T, db *DB, runID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	_, err := db.pool.ExecContext(ctx, `INSERT INTO llm_calls
		(run_id, round, provider, model, duration_ms, status, created_at)
		VALUES ($1, 1, 'stub', 'stub-model', 0, 'pending', $2)`, runID, now)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO tool_invocations
		(run_id, tool_name, status, started_at)
		VALUES ($1, 'shell', 'pending', $2)`, runID, now)
	require.NoError(t, err)
}

// requireAuditNormalized 断言该 Run 的两类审计行都已归一化为 unknown（含 error_kind /
// recovered_at），即"我们确实收尾了它"。
func requireAuditNormalized(t *testing.T, db *DB, runID string) {
	t.Helper()
	for _, query := range []string{
		`SELECT status, COALESCE(error_kind, ''), COALESCE(recovered_at, 0) FROM llm_calls WHERE run_id = $1`,
		`SELECT status, COALESCE(error_kind, ''), COALESCE(recovered_at, 0) FROM tool_invocations WHERE run_id = $1`,
	} {
		var (
			status    string
			errorKind string
			recovered int64
		)
		require.NoError(t, db.pool.QueryRowContext(context.Background(), query, runID).
			Scan(&status, &errorKind, &recovered))
		require.Equal(t, string(core.LLMCallUnknown), status, "领取域内必须归一化")
		require.Equal(t, string(core.ErrorKindUnknown), errorKind)
		require.NotZero(t, recovered, "归一化必须落 recovered_at")
	}
}

// requireAuditStillPending 断言集合外的 pending 行没被改写——这是 C-08 的核心判据。
func requireAuditStillPending(t *testing.T, db *DB, runID string) {
	t.Helper()
	for _, query := range []string{
		`SELECT status, COALESCE(error_kind, ''), COALESCE(recovered_at, 0) FROM llm_calls WHERE run_id = $1`,
		`SELECT status, COALESCE(error_kind, ''), COALESCE(recovered_at, 0) FROM tool_invocations WHERE run_id = $1`,
	} {
		var (
			status    string
			errorKind string
			recovered int64
		)
		require.NoError(t, db.pool.QueryRowContext(context.Background(), query, runID).
			Scan(&status, &errorKind, &recovered))
		require.Equal(t, string(core.LLMCallPending), status, "集合外的 pending 行不得被改写")
		require.Empty(t, errorKind)
		require.Zero(t, recovered)
	}
}

// runReasonOf 读运行行的 reason（回收文案的机器可读字面量）。
func runReasonOf(t *testing.T, db *DB, runID string) string {
	t.Helper()
	var reason string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT COALESCE(reason, '') FROM runs WHERE run_id = $1`, runID).Scan(&reason))
	return reason
}

// executionResourceID 是 `schedule_execution` 租约的资源标识形态（执行行主键的文本）。
func executionResourceID(executionID int64) string {
	return strconv.FormatInt(executionID, 10)
}

// requireExecutionRow 断言执行行的终态与错误文案。
func requireExecutionRow(t *testing.T, db *DB, executionID int64, status, errMsg string) {
	t.Helper()
	var (
		gotStatus string
		gotErr    string
	)
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT status, COALESCE(error_message, '') FROM task_executions WHERE id = $1`,
		executionID).Scan(&gotStatus, &gotErr))
	require.Equal(t, status, gotStatus)
	require.Equal(t, errMsg, gotErr)
}

// requireExecutionStillStarted 断言某任务当前仍有一条 started 执行行（活租约未被收尾）。
func requireExecutionStillStarted(t *testing.T, db *DB, taskID, msg string) {
	t.Helper()
	var status string
	require.NoError(t, db.pool.QueryRowContext(context.Background(), `
SELECT status FROM task_executions
 WHERE task_id = $1 AND status = 'started'`, taskID).Scan(&status))
	require.Equal(t, string(core.ExecutionStarted), status, msg)
}

// requireLeaseReleased 断言租约行的释放状态。
func requireLeaseReleased(t *testing.T, db *DB, kind core.LeaseKind, resourceID string, want bool) {
	t.Helper()
	var released *int64
	require.NoError(t, db.pool.QueryRowContext(context.Background(), `
SELECT released_at FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(kind), resourceID).Scan(&released))
	require.Equal(t, want, released != nil, "租约释放状态不符（released_at=%v）", released)
}

// TestDelivery_RecoveryScopedToReclaimedRun 判投递恢复的**作用域**（FR-24-21 / C-15 /
// 模块卡 §4.3）：A1 只结算自己合法领取的那条 Run 的 pending 投递；另一条仍有效的 Run
// 的 pending 行一字不动。
//
// 逆改点：把 `RecoverRun` 里的 `settleDeliveriesInTx(runID, …)` 换回全局
// `DeliveryStore.Recover`（无作用域）→ `run-live` 的 pending 被改写成 skipped，
// 第二个断言红。这条判据保护的是"替别人宣布没发出去"——重复通知之外最坏的一类
// 跨 Worker 误写。
func TestDelivery_RecoveryScopedToReclaimedRun(t *testing.T) {
	db := newTxTestDB(t)
	ctx := context.Background()
	deliveryPortSeedTask(t, db, "task-delivery-scope", "demo", "0 8 * * * *", "每日汇总")
	deliveries := NewDeliveryStore(db)

	seedReclaimRun(t, db, "run-claimed", "s-claimed", "worker-claimed")
	seedReclaimRun(t, db, "run-live", "s-live", "worker-live")
	for _, runID := range []string{"run-claimed", "run-live"} {
		require.NoError(t, deliveries.Prepare(ctx, core.ScheduleDeliveryRecord{
			RunID:       runID,
			TaskID:      "task-delivery-scope",
			Agent:       "demo",
			AgentUID:    core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"),
			ChannelUID:  core.ResourceUID("rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"),
			ChannelName: "alerts",
			CreatedAt:   time.Now().UTC(),
		}))
	}
	expireLease(t, NewLeaseStore(db), core.LeaseRun, "run-claimed")

	claims, err := NewReclaimerStore(db).ClaimExpired(ctx, core.LeaseRun,
		reclaimIdentity(), 32, reclaimTestTTL)
	require.NoError(t, err)
	require.Len(t, claims, 1, "只有过期租约被领取")
	require.Equal(t, "run-claimed", claims[0].ResourceID)

	report, err := RecoverRun(ctx, db, slog.Default(), claims[0].LeaseToken, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, 1, report.DeliveriesSkipped)

	claimed, err := deliveries.Get(ctx, "run-claimed")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySkipped, claimed.Status)
	require.Equal(t, "运行未完成，未投递", claimed.Detail)
	require.True(t, claimed.AttemptedAt.IsZero(), "收尾不得重发")

	live, err := deliveries.Get(ctx, "run-live")
	require.NoError(t, err)
	require.Equal(t, core.DeliveryPending, live.Status, "活 Run 的 pending 投递不得被改判")
	require.True(t, live.FinishedAt.IsZero())
	require.Equal(t, string(core.StatusRunning), runStatusOf(t, db, "run-live"))
	requireLeaseReleased(t, db, core.LeaseRun, "run-live", false)
}

// settleOutcome 是后台 B2 结算的结果。
type settleOutcome struct {
	settled int
	err     error
}

// scheduleRenewal 是一条持锁未提交的执行租约续约，用于判"回收器是否排在续约后面"。
// The conn stays open for the whole case; the transaction ends by commit or rollback.
type scheduleRenewal struct {
	conn *sql.Conn
	tx   *sql.Tx
	pid  int
}

// commit 提交续约（到期被推后，属主仍然有效）。
func (r *scheduleRenewal) commit() error { return r.tx.Commit() }

// close 未结束的事务必须先回滚——`database/sql` 的连接关闭要等事务收尾。
func (r *scheduleRenewal) close() error {
	_ = r.tx.Rollback() // 已提交时返回 ErrTxDone，忽略即可
	return r.conn.Close()
}

// openScheduleRenewal 在一条**已过期**的执行租约上打开未提交的续约并把到期推到
// ~55s 之后。语句与 `LeaseStore.Renew` 同形，只把"现在"取到 5 秒前——测试无法稳定地
// 把墙钟卡在到期前后，被验的是回收器获锁后的重判，不是续约自身的校验时序。
// openScheduleRenewal holds the lease row lock with an uncommitted renewal.
func openScheduleRenewal(t *testing.T, db *DB, resourceID string) *scheduleRenewal {
	t.Helper()
	conn, err := db.pool.Conn(t.Context())
	require.NoError(t, err)

	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	res, err := tx.ExecContext(t.Context(), `
WITH now AS (SELECT ((extract(epoch from clock_timestamp()) * 1000)::bigint - 5000) AS ms)
UPDATE execution_leases l
   SET renewed_at = now.ms, expires_at = now.ms + 60000
  FROM now
 WHERE l.kind = $1 AND l.resource_id = $2
   AND l.released_at IS NULL AND l.expires_at > now.ms`,
		string(core.LeaseScheduleExecution), resourceID)
	require.NoError(t, err, "续约事务必须命中同一行（持有行锁）")
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)

	var pid int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	return &scheduleRenewal{conn: conn, tx: tx, pid: pid}
}

// requireSettleBlockedByRenewal 断言 B2 的结算确实排在续约事务后面。读数用
// `pg_blocking_pids(pid)`：行被未提交事务更新时，后到的 `FOR UPDATE` 等的是
// **更新事务号**（locktype=transactionid，没有 relation），按表名过滤会漏掉。
// requireSettleBlockedByRenewal reads pg_blocking_pids because a FOR UPDATE against an
// updated row waits on the transaction id, not a relation lock.
func requireSettleBlockedByRenewal(t *testing.T, db *DB, renewalPid int, done <-chan settleOutcome) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		require.NoError(t, db.pool.QueryRowContext(context.Background(), `
SELECT count(*) FROM pg_stat_activity a
 WHERE $1 = ANY (pg_blocking_pids(a.pid))`, renewalPid).Scan(&blocked))
		if blocked > 0 {
			return
		}
		select {
		case out := <-done:
			t.Fatalf("B2 结算在续约提交前就返回了（没有排在续约后面）：%+v", out)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("5s 内没有观察到排在续约事务后面的 B2 结算")
}

// TestReclaimer_OrphanDeliveriesWaitForConcurrentRenewal 判 B2 的**锁内重判**
// （review ② 复现项 1 的同类路径 / 模块卡 §2.3.5 B2 / FR-24-39）：候选谓词
// `NOT EXISTS(仍有效的 schedule_execution 租约)` 是快照读，看不到一条**未提交**的
// 合法续约。逐行结算前必须先锁租约行、按获锁后的数据库时钟重判——否则会把活属主正在
// 推进的预写投递标成 skipped，等于替别人宣布"没发出去"。
//
// 同一轮里放一条**真失联**的对照行：它必须照常结算。这样"跳过"判的是所有权，而不是
// 回收器整体不工作。
//
// 逆改点：删掉 `SettleOrphanDeliveries` 里的 `lockOrphanDeliveryLeaseRecheck` →
// 活属主的预写行在续约提交前被结算，`run-b2-live` 的 pending 断言红。
func TestReclaimer_OrphanDeliveriesWaitForConcurrentRenewal(t *testing.T) {
	db := newTxTestDB(t)
	ctx := context.Background()
	tasks := NewTaskStore(db)
	deliveries := NewDeliveryStore(db)
	worker := core.WorkerIdentity{InstanceID: "worker-b2", Name: "worker-b2"}

	// seedOrphanDelivery 造一条"Run 尚未创建"的 pending 投递，并把它的执行租约改到过期。
	seedOrphanDelivery := func(taskID, runID string, createdAt time.Time) {
		t.Helper()
		deliveryPortSeedTask(t, db, taskID, "demo", "0 8 * * * *", "每日汇总")
		executionID, _, err := tasks.StartExecution(ctx, core.ExecutionRecord{
			TaskID: taskID, RunID: runID, Status: core.ExecutionStarted,
			Trigger: core.TriggerCron, StartedAt: createdAt,
		}, worker, reclaimTestTTL)
		require.NoError(t, err)
		require.NoError(t, deliveries.Prepare(ctx, core.ScheduleDeliveryRecord{
			RunID: runID, TaskID: taskID, Agent: "demo",
			ExecutionID: executionID, CreatedAt: createdAt,
		}))
		expireLease(t, NewLeaseStore(db), core.LeaseScheduleExecution, executionResourceID(executionID))
	}

	// 活属主：租约在快照里已过期，但它正做一笔未提交的续约，Run 行还没建出来。
	seedOrphanDelivery("task-b2-live", "run-b2-live", opNow)
	// 对照：真失联（没有续约），必须在同一轮结算。
	seedOrphanDelivery("task-b2-dead", "run-b2-dead", opNow.Add(time.Minute))

	resourceID := executionResourceIDOfTask(t, db, "task-b2-live")
	renewal := openScheduleRenewal(t, db, resourceID)
	defer func() { _ = renewal.close() }()

	done := make(chan settleOutcome, 1)
	go func() {
		settled, err := SettleOrphanDeliveries(context.Background(), db, time.Now().UTC(), 8)
		done <- settleOutcome{settled: settled, err: err}
	}()
	requireSettleBlockedByRenewal(t, db, renewal.pid, done)

	require.NoError(t, renewal.commit())
	out := <-done
	require.NoError(t, out.err)
	require.Equal(t, 1, out.settled, "只有真失联的那条被结算")

	live, err := deliveries.Get(ctx, "run-b2-live")
	require.NoError(t, err)
	require.Equal(t, core.DeliveryPending, live.Status, "活属主的预写投递不得被结算")
	require.Empty(t, live.Detail)
	require.True(t, live.FinishedAt.IsZero())

	dead, err := deliveries.Get(ctx, "run-b2-dead")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySkipped, dead.Status)
	require.Equal(t, orphanDeliveryDetail, dead.Detail)
	require.False(t, dead.FinishedAt.IsZero())
}

// executionResourceIDOfTask 读某任务唯一一条 started 执行行的租约资源标识。
func executionResourceIDOfTask(t *testing.T, db *DB, taskID string) string {
	t.Helper()
	var id int64
	require.NoError(t, db.pool.QueryRowContext(context.Background(), `
SELECT id FROM task_executions WHERE task_id = $1 AND status = $2`,
		taskID, string(core.ExecutionStarted)).Scan(&id))
	return executionResourceID(id)
}
