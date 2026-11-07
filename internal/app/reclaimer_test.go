//go:build integration

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 本文件是回收器**装配面**的三条判据（US-24 T017 / FR-24-02 / FR-24-17 / C-07 / C-12）：
//
//   - `TestStartup_DoesNotRewriteOtherWorkersRows`：启动链的首轮 `ReclaimOnce` 只收尾
//     合法领取到的过期租约；另一个活 Worker 的 running 行、pending 审计与租约一字不动。
//     逆改点：把 `newLeaseReclaimer` 换回全局恢复扫描（旧 `db.Recover`）→ 活行被标
//     failed、pending 变 unknown，用例红。
//   - `TestReclaimer_RunsWithoutCronAndIsBounded`：回收循环不依赖 cron，按自己的节奏
//     运行，且每轮都带**条数上限**与**独立超时**。逆改点：把上限写死成无界（或把
//     `context.WithTimeout` 去掉）→ 条数 / 超时断言红；把循环挂到 cron 上 → 没有
//     cron 引擎时一轮都不会发生，用例超时。
//   - `TestReclaimer_EmitsDiagnosticsForUnreplayedActions`：A1 归一化出的未重放动作
//     逐条落成 `recovery_unknown` 诊断事件（评审 IDX-28）。逆改点：不装配或删掉
//     `recordUnreplayedActions` 调用 → 事件数为 0，用例红。
//
// 存储层的三条作用域用例在 `internal/storage/postgres/reclaimer_test.go`（该包不能反向
// import 本装配层）；这里只判装配与循环本身。A3 的取证与状态判定由 T022 / T023 的
// Flow 用例覆盖，本文件不重复构造 `flow.Service` 的完整装配。

// startupReclaimTTL 是启动用例里回收器领取后的有效期。
const startupReclaimTTL = 30 * time.Second

// TestStartup_DoesNotRewriteOtherWorkersRows 判"启动只碰合法领取的过期租约"
// （FR-24-02 / C-07）：活 Worker 的 running 行、pending 审计与租约零改动，过期租约的
// 那条 Run 被收为 failed / worker_lost。
func TestStartup_DoesNotRewriteOtherWorkersRows(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	appSeedRun(t, db, "run-live-other", "s-live-other", "worker-other")
	appSeedPendingAudit(t, db, "run-live-other")
	appSeedRun(t, db, "run-expired", "s-expired", "worker-gone")
	expireLeaseForTest(t, db, core.LeaseRun, "run-expired")

	identity := core.WorkerIdentity{
		InstanceID:    "worker-new",
		Name:          "new",
		Version:       "test",
		SchemaVersion: postgres.SupportedSchemaVersion,
	}
	// 本用例的库里没有 flow_root 租约，A3 不会被触达；`Flows` 只作为装配前提存在。
	reclaimer, err := newLeaseReclaimer(reclaimerDeps{
		DB:       db,
		Flows:    &flow.Service{},
		Leases:   postgres.NewLeaseStore(db),
		Worker:   identity,
		LeaseTTL: startupReclaimTTL,
		Learning: postgres.NewLearningRepo(db),
	})
	require.NoError(t, err)

	a := &App{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rt, err := a.startWorkerRuntime(ctx, db, identity, reclaimer)
	require.NoError(t, err)
	t.Cleanup(func() {
		// 父 ctx 在 Cleanup 之前已被取消；Stop 用独立预算等循环退出与下线标记。
		require.NoError(t, rt.Stop(context.Background()))
	})

	// ① 活 Worker 的在跑行：状态、reason、审计、租约全部零改动。
	require.Equal(t, string(core.StatusRunning), appRunStatus(t, db, "run-live-other"),
		"启动不得改写其他活 Worker 的在跑行")
	require.Empty(t, appRunReason(t, db, "run-live-other"))
	require.Equal(t, string(core.LLMCallPending), appAuditStatus(t, db, "llm_calls", "run-live-other"),
		"活 Worker 的 pending 审计不得被启动归一化")
	require.Equal(t, string(core.LLMCallPending), appAuditStatus(t, db, "tool_invocations", "run-live-other"),
		"活 Worker 的 pending 审计不得被启动归一化")
	requireLeaseUntouched(t, db, core.LeaseRun, "run-live-other", "worker-other")

	// ② 合法领取到的过期租约：首轮回收同步完成收尾。
	require.Equal(t, string(core.StatusFailed), appRunStatus(t, db, "run-expired"))
	require.Equal(t, "worker_lost", appRunReason(t, db, "run-expired"))

	// ③ 启动链确实执行过：本进程已登记，且尚未标记下线。
	require.Equal(t, 1, appWorkerNodeCount(t, db, identity.InstanceID),
		"首轮回收的入口与登记都在 startWorkerRuntime 里，登记行是它跑过的证据")
}

// TestReclaimer_EmitsDiagnosticsForUnreplayedActions 判 A1 的"写诊断事件"
// （模块卡 §2.3.5 A1，评审 IDX-28）：接管收尾把 pending 审计归一化为 `unknown` 之后，
// 每条**未重放**的动作必须在 `diagnostic_events` 里留下一条可回指的记录——
// `actor=recovery`、`reason_code=recovery_unknown`、`audit_ref` 指回原审计行，
// 且归属两项由租约行回填成**本次接管**的实例与代次。
//
// 逆改点：删掉 `recordUnreplayedActions` 的调用或 `Diagnostics` 装配 → 事件数为 0；
// 把事件写在事务提交之前 → 回滚分支留下伪证据（另一条用例的损坏夹具会命中）；
// 用运行期生产者（actor 空、无原因码）→ actor / reason_code 断言红。
func TestReclaimer_EmitsDiagnosticsForUnreplayedActions(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	// 一条过期租约（会被接管）+ 一条活租约（不得被碰，也不得产生事件）。
	appSeedRun(t, db, "run-taken-over", "s-taken-over", "worker-gone")
	appSeedPendingAudit(t, db, "run-taken-over")
	expireLeaseForTest(t, db, core.LeaseRun, "run-taken-over")
	appSeedRun(t, db, "run-still-live", "s-still-live", "worker-live")
	appSeedPendingAudit(t, db, "run-still-live")

	identity := core.WorkerIdentity{
		InstanceID:    "worker-reclaim-diag",
		Name:          "reclaim-diag",
		Version:       "test",
		SchemaVersion: postgres.SupportedSchemaVersion,
	}
	recorder := diagnostics.NewRecorder(diagnostics.RecorderOptions{
		Store:  postgres.NewDiagnosticStore(db).WithWorker(identity.InstanceID),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	reclaimer, err := newLeaseReclaimer(reclaimerDeps{
		DB:          db,
		Flows:       &flow.Service{},
		Leases:      postgres.NewLeaseStore(db),
		Worker:      identity,
		LeaseTTL:    startupReclaimTTL,
		Learning:    postgres.NewLearningRepo(db),
		Diagnostics: recorder,
	})
	require.NoError(t, err)

	report, err := reclaimer.ReclaimOnce(ctx, 32)
	require.NoError(t, err)
	require.Equal(t, 1, report.Runs)
	require.Equal(t, 2, report.PendingAudit, "两条 pending 审计行被归一化")

	// 被接管的 Run：每条动作一条事件，形状与契约一致。
	llmID, toolID := appAuditIDs(t, db, "run-taken-over")
	events := appDiagnosticEvents(t, db, "run-taken-over")
	require.Len(t, events, 2, "每条未重放动作一条诊断事件（模型 + 工具）")
	byKind := map[string]appDiagnosticEvent{}
	for _, ev := range events {
		byKind[ev.kind] = ev
		require.Equal(t, "recovery", ev.actor, "事件必须标明是恢复路径，不是运行期生产者")
		require.Equal(t, "recovery_unknown", ev.reasonCode,
			"未重放动作只允许契约里的 recovery_unknown，不得借用执行失败的原因码")
		require.Equal(t, identity.InstanceID, ev.attrs["worker_instance_id"],
			"归属回填必须是本次接管的实例")
		require.Equal(t, float64(2), ev.attrs["ownership_epoch"],
			"归属代次必须是本次领取递增后的 2（不是旧属主的 1）")
	}
	llm, ok := byKind["llm_call"]
	require.True(t, ok, "模型调用动作必须落成 llm_call 事件")
	require.Equal(t, "llm_calls:"+strconv.FormatInt(llmID, 10), llm.auditRef,
		"audit_ref 必须回指原审计行（table:id）")
	require.Equal(t, "1", llm.stepID, "轮号进 step_id，与运行期 llm_call 事件同一口径")
	tool, ok := byKind["tool_call"]
	require.True(t, ok, "工具调用动作必须落成 tool_call 事件")
	require.Equal(t, "tool_invocations:"+strconv.FormatInt(toolID, 10), tool.auditRef)
	require.Equal(t, "shell", tool.attrs["tool"], "工具名进 attrs，与 tool_call 事件同一白名单")

	// 活租约的 Run：pending 审计与事件面都不得被碰。
	require.Equal(t, string(core.LLMCallPending), appAuditStatus(t, db, "llm_calls", "run-still-live"))
	require.Empty(t, appDiagnosticEvents(t, db, "run-still-live"),
		"没有合法领取到租约的 Run 不得出现恢复事件")
}

// appAuditIDs 读某 Run 的模型调用 / 工具调用审计行主键。
func appAuditIDs(t *testing.T, db *postgres.DB, runID string) (int64, int64) {
	t.Helper()
	var llmID, toolID int64
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT id FROM llm_calls WHERE run_id = $1`, runID).Scan(&llmID))
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT id FROM tool_invocations WHERE run_id = $1`, runID).Scan(&toolID))
	return llmID, toolID
}

// appDiagnosticEvent 是断言用的最小投影（不引诊断包的读面装配）。
type appDiagnosticEvent struct {
	kind       string
	actor      string
	reasonCode string
	auditRef   string
	stepID     string
	attrs      map[string]any
}

// appDiagnosticEvents 读某 Run 的全部诊断事件（按 kind 排序，便于逐条断言）。
func appDiagnosticEvents(t *testing.T, db *postgres.DB, runID string) []appDiagnosticEvent {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
SELECT kind, COALESCE(actor, ''), COALESCE(reason_code, ''), COALESCE(audit_ref, ''),
       COALESCE(step_id, ''), attrs::text
  FROM diagnostic_events WHERE run_id = $1 ORDER BY kind`, runID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []appDiagnosticEvent
	for rows.Next() {
		var (
			ev     appDiagnosticEvent
			rawStr string
		)
		require.NoError(t, rows.Scan(&ev.kind, &ev.actor, &ev.reasonCode, &ev.auditRef, &ev.stepID, &rawStr))
		require.NoError(t, json.Unmarshal([]byte(rawStr), &ev.attrs))
		out = append(out, ev)
	}
	require.NoError(t, rows.Err())
	return out
}

// recordingReclaimer 记录每一轮的上限与超时预算，并在前两轮向 `signal` 发信号。
// recordingReclaimer records the per-round bound and signals the first two rounds.
type recordingReclaimer struct {
	mu        sync.Mutex
	limits    []int
	deadlines []time.Duration
	rounds    int
	signal    chan struct{}
}

func newRecordingReclaimer() *recordingReclaimer {
	return &recordingReclaimer{signal: make(chan struct{}, 2)}
}

func (r *recordingReclaimer) ReclaimOnce(ctx context.Context, limit int) (core.ReclaimReport, error) {
	deadline, hasDeadline := ctx.Deadline()
	budget := time.Duration(0)
	if hasDeadline {
		budget = time.Until(deadline)
	}
	r.mu.Lock()
	r.rounds++
	r.limits = append(r.limits, limit)
	r.deadlines = append(r.deadlines, budget)
	round := r.rounds
	r.mu.Unlock()
	if round <= 2 {
		r.signal <- struct{}{}
	}
	return core.ReclaimReport{}, nil
}

func (r *recordingReclaimer) snapshot() (int, []int, []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rounds, append([]int(nil), r.limits...), append([]time.Duration(nil), r.deadlines...)
}

// recordingRegistry 是心跳 / 下线面的记录器；本用例只需要它能被 Stop 调通。
type recordingRegistry struct {
	mu          sync.Mutex
	registerHit int
	heartbeatN  int
	stopN       int
}

func (r *recordingRegistry) Register(context.Context, core.WorkerIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerHit++
	return nil
}

func (r *recordingRegistry) Heartbeat(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.heartbeatN++
	return nil
}

func (r *recordingRegistry) Stop(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopN++
	return nil
}

// TestReclaimer_RunsWithoutCronAndIsBounded 判回收循环独立于 cron 且有界（FR-24-17 / C-12）：
// 没有任何 cron 引擎参与，循环仍按自己的节奏跑；每轮携带调用方给的条数上限与独立超时。
func TestReclaimer_RunsWithoutCronAndIsBounded(t *testing.T) {
	const (
		interval = 5 * time.Millisecond
		limit    = 7
	)

	reclaimer := newRecordingReclaimer()
	registry := &recordingRegistry{}
	rt := newWorkerRuntime(core.WorkerIdentity{InstanceID: "worker-loop"}, registry, reclaimer, nil)
	rt.StartReclaimer(t.Context(), interval, limit)

	// 等前两轮：循环只靠自己的 ticker 驱动（本用例没有启动 Scheduler / cron）。
	deadline := time.After(5 * time.Second)
	for hits := 0; hits < 2; {
		select {
		case <-reclaimer.signal:
			hits++
		case <-deadline:
			rounds, _, _ := reclaimer.snapshot()
			t.Fatalf("回收循环未按自身节奏运行：5 秒内只跑了 %d 轮", rounds)
		}
	}

	rounds, limits, budgets := reclaimer.snapshot()
	require.GreaterOrEqual(t, rounds, 2)
	require.Len(t, limits, rounds)
	require.Len(t, budgets, rounds)
	for i := range limits {
		require.Equal(t, limit, limits[i], "每轮都必须带条数上限（第 %d 轮）", i)
		require.Greater(t, budgets[i], time.Duration(0), "每轮都必须有独立超时（第 %d 轮）", i)
		require.LessOrEqual(t, budgets[i], reclaimRoundTimeout, "单轮超时不得超过固定上限")
	}

	// 停机先停领取：Stop 之后不再有新一轮。
	require.NoError(t, rt.Stop(context.Background()))
	beforeStop, _, _ := reclaimer.snapshot()
	time.Sleep(4 * interval)
	afterStop, _, _ := reclaimer.snapshot()
	require.Equal(t, beforeStop, afterStop, "Stop 之后回收循环不得再领一轮")
	require.Equal(t, 1, registry.stopN, "停机必须写下线标记")
}

// TestReclaimer_CountsPerResourceFailures 判 `ReclaimReport.Errors` 的契约
// （模块卡 §3.1.1：「错误只计数，详细错误进日志」）：单条收尾失败**计入 Errors**、
// 不拖垮同轮其余条目、不交还它自己的租约（下一轮仍 TookOver=true 再试）。
//
// 失败夹具用真损坏：`messages.tool_calls` 只由本仓写入，解不开就是真损坏
// （`parseToolCalls` 不猜）；它让 `RecoverRun` 的半截组扫描报错并整笔回滚。
// The broken fixture is genuine data corruption, not a stubbed failure.
func TestReclaimer_CountsPerResourceFailures(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	appSeedRun(t, db, "run-broken-audit", "s-broken", "worker-broken")
	appSeedRun(t, db, "run-healthy", "s-healthy", "worker-healthy")
	appSeedMalformedToolCalls(t, db, "run-broken-audit", "s-broken")
	expireLeaseForTest(t, db, core.LeaseRun, "run-broken-audit")
	expireLeaseForTest(t, db, core.LeaseRun, "run-healthy")

	identity := core.WorkerIdentity{
		InstanceID:    "worker-counter",
		Name:          "counter",
		Version:       "test",
		SchemaVersion: postgres.SupportedSchemaVersion,
	}
	reclaimer, err := newLeaseReclaimer(reclaimerDeps{
		DB:       db,
		Flows:    &flow.Service{},
		Leases:   postgres.NewLeaseStore(db),
		Worker:   identity,
		LeaseTTL: startupReclaimTTL,
		Learning: postgres.NewLearningRepo(db),
	})
	require.NoError(t, err)

	report, err := reclaimer.ReclaimOnce(ctx, 32)
	require.NoError(t, err, "单条失败不得让整轮报错")

	require.Equal(t, 1, report.Errors, "损坏的那条必须计入 Errors，不能静默")
	require.Equal(t, 1, report.Runs, "同轮的健康条目照常收尾")
	require.Equal(t, string(core.StatusRunning), appRunStatus(t, db, "run-broken-audit"),
		"失败的一轮整笔回滚：Run 仍 running，下一轮再试")
	requireLeaseClaimedButUnreleased(t, db, core.LeaseRun, "run-broken-audit", identity.InstanceID)
	require.Equal(t, string(core.StatusFailed), appRunStatus(t, db, "run-healthy"))
}

// appSeedMalformedToolCalls 写一条 `tool_calls` 无法解析的 assistant 消息——模拟真损坏。
func appSeedMalformedToolCalls(t *testing.T, db *postgres.DB, runID, sessionID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO messages
		(session_id, run_id, seq, role, content, tool_calls, created_at)
		VALUES ($1, $2, 1, 'assistant', '', '{broken', $3)`,
		sessionID, runID, time.Now().UTC().UnixMilli())
	require.NoError(t, err)
}

// requireLeaseClaimedButUnreleased 断言租约已被本次领取（属主换成了回收器）但仍未释放。
func requireLeaseClaimedButUnreleased(t *testing.T, db *postgres.DB, kind core.LeaseKind, resourceID, owner string) {
	t.Helper()
	var (
		gotOwner string
		released *int64
	)
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT owner_worker_id, released_at
  FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(kind), resourceID).Scan(&gotOwner, &released))
	require.Equal(t, owner, gotOwner, "失败的一条仍归回收器持有")
	require.Nil(t, released, "失败的一条不得交还租约（下一轮必须仍 TookOver=true）")
}

// appSeedRun 建会话 + `running` 的 Run 行，并由 owner 领到 run 租约。
func appSeedRun(t *testing.T, db *postgres.DB, runID, sessionID, owner string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	_, err := db.ExecContext(ctx, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at, caller_id)
		VALUES ($1, 'demo', 'web', 'u1', 'active', 0, $2, $2, 'local-admin')`, sessionID, now)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, $2, 'demo', 'web', 'u1', 'user', 'running', $3,
		        'local-admin', 'admin-builtin', NULL, 'admin-cli')`, runID, sessionID, now)
	require.NoError(t, err)
	_, err = postgres.NewLeaseStore(db).Acquire(ctx, core.LeaseRun, runID,
		core.WorkerIdentity{InstanceID: owner, Name: owner}, startupReclaimTTL)
	require.NoError(t, err)
}

// appSeedPendingAudit 给一个 Run 各写一条 pending 的模型调用与工具调用审计行。
func appSeedPendingAudit(t *testing.T, db *postgres.DB, runID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	_, err := db.ExecContext(ctx, `INSERT INTO llm_calls
		(run_id, round, provider, model, duration_ms, status, created_at)
		VALUES ($1, 1, 'stub', 'stub-model', 0, 'pending', $2)`, runID, now)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO tool_invocations
		(run_id, tool_name, status, started_at)
		VALUES ($1, 'shell', 'pending', $2)`, runID, now)
	require.NoError(t, err)
}

// expireLeaseForTest 把租约改到过去。**只用于制造夹具**：生产路径的过期由数据库时钟
// 自然发生，回收只按 `expires_at <= db_now` 判定。
func expireLeaseForTest(t *testing.T, db *postgres.DB, kind core.LeaseKind, resourceID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
UPDATE execution_leases
   SET expires_at = (extract(epoch from clock_timestamp()) * 1000)::bigint - 1000
 WHERE kind = $1 AND resource_id = $2`, string(kind), resourceID)
	require.NoError(t, err)
}

// appRunStatus 读运行行状态。
func appRunStatus(t *testing.T, db *postgres.DB, runID string) string {
	t.Helper()
	var status string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&status))
	return status
}

// appRunReason 读运行行的 reason。
func appRunReason(t *testing.T, db *postgres.DB, runID string) string {
	t.Helper()
	var reason string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT COALESCE(reason, '') FROM runs WHERE run_id = $1`, runID).Scan(&reason))
	return reason
}

// appAuditStatus 读某 Run 在指定审计表里的状态（表名来自固定枚举，不拼用户输入）。
func appAuditStatus(t *testing.T, db *postgres.DB, table, runID string) string {
	t.Helper()
	var status string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status FROM `+table+` WHERE run_id = $1`, runID).Scan(&status))
	return status
}

// requireLeaseUntouched 断言租约仍归原属主、代次未变、未释放。
func requireLeaseUntouched(t *testing.T, db *postgres.DB, kind core.LeaseKind, resourceID, owner string) {
	t.Helper()
	var (
		gotOwner string
		epoch    int64
		released *int64
	)
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT owner_worker_id, ownership_epoch, released_at
  FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(kind), resourceID).Scan(&gotOwner, &epoch, &released))
	require.Equal(t, owner, gotOwner, "活租约的属主不得被启动改写")
	require.Equal(t, int64(1), epoch, "活租约的代次不得被启动递增")
	require.Nil(t, released, "活租约不得被启动释放")
}

// appWorkerNodeCount 数某实例的登记行（`stopped_at` 为空表示仍在线）。
func appWorkerNodeCount(t *testing.T, db *postgres.DB, instanceID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM worker_nodes WHERE instance_id = $1 AND stopped_at IS NULL`,
		instanceID).Scan(&n))
	return n
}

// ---- B1 / B2 与执行行四值域（US-24 T018 / FR-24-39）-----------------------------
//
// 三个用例共同固定回收器的**收尾面**：
//
//   - `TestReclaimer_TerminalResidueAndPreRunDelivery`：终态残留（含 `limit_reached`）
//     的三张引用表与 pending 审计归零；Run 创建前预写、属主已失联的 pending 投递结算
//     为 `skipped`，终态 Run 的 pending 投递结算为 `unknown`；两者都不重发。
//   - `TestReclaimer_LeavesLiveExecutionContextAlone`：执行租约**仍有效**时，预写投递
//     与执行行一字不动（所有权必须显式判，幂等不等于第一次写就是对的）。
//   - `TestReclaimer_ExecutionStatusStaysInFourValues`：执行行只落
//     `started` / `completed` / `failed` / `skipped`；`limit_reached → completed`、
//     `cancelled → failed`、无 Run → `failed + worker_lost`。
//
// 逆改点：B1 的终态集合漏 `limit_reached` → ① 的 pending 审计永不归一化；B1 只释放
// `run_resource_refs` → ② 的凭据 / 通知引用永不释放；B2 写成 `unknown` → ③ 的状态
// 断言红；B2 去掉 `schedule_execution` 所有权谓词 → 第二个用例的 pending 被改成
// `skipped`；`executionOutcomeFor` 把 Run 状态原样写进执行行 → 第三个用例的
// `limit_reached` 取值不在四值域内。

// appReclaimerIdentity 是这些用例共用的回收器身份。
func appReclaimerIdentity() core.WorkerIdentity {
	return core.WorkerIdentity{
		InstanceID:    "worker-residue",
		Name:          "residue",
		Version:       "test",
		SchemaVersion: postgres.SupportedSchemaVersion,
	}
}

// appNewReclaimer 组装一个只依赖库的回收器。这些用例的库里没有 `flow_root` 租约，
// A3 不会被触达；`Flows` 只作为装配前提存在。
func appNewReclaimer(t *testing.T, db *postgres.DB) *leaseReclaimer {
	t.Helper()
	reclaimer, err := newLeaseReclaimer(reclaimerDeps{
		DB:       db,
		Flows:    &flow.Service{},
		Leases:   postgres.NewLeaseStore(db),
		Worker:   appReclaimerIdentity(),
		LeaseTTL: startupReclaimTTL,
		Learning: postgres.NewLearningRepo(db),
	})
	require.NoError(t, err)
	return reclaimer
}

// TestReclaimer_TerminalResidueAndPreRunDelivery 判 B1 覆盖四类终态残留与 B2 的
// 「Run 创建前预写」域（FR-24-39 / C-29 / 模块卡 §2.3.5 B1、B2）。
func TestReclaimer_TerminalResidueAndPreRunDelivery(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	// ① `limit_reached` 不是失败，但它的残留同样必须有人收（漏这一支就永远留着）。
	appSeedTerminalRun(t, db, "run-limit", core.StatusLimitReached)
	appSeedPendingAudit(t, db, "run-limit")

	// ② 只有凭据 / 通知两类引用未释放：三张表的释放语句任何一张漏掉都会留下一类。
	appSeedTerminalRun(t, db, "run-refs", core.StatusCompleted)
	appSeedCredentialRef(t, db, "run-refs")
	appSeedNotifyRef(t, db, "run-refs")

	// ③ 调度器在 Run 准入**之前**写了 pending 投递，随后属主失联（执行租约过期）。
	appSeedScheduledTask(t, db, "task-pre-run")
	preRunExec := appSeedStartedExecution(t, db, "task-pre-run", sql.NullString{}, "worker-gone")
	appSeedPendingDelivery(t, db, "run-pre-run", "task-pre-run", preRunExec)
	expireLeaseForTest(t, db, core.LeaseScheduleExecution, strconv.FormatInt(preRunExec, 10))

	// ④ 终态 Run 的 pending 投递：模型已出正文，投递是否发出无从证明。
	appSeedTerminalRun(t, db, "run-done", core.StatusCompleted)
	appSeedScheduledTask(t, db, "task-done")
	appSeedPendingDelivery(t, db, "run-done", "task-done", 0)

	report, err := appNewReclaimer(t, db).ReclaimOnce(ctx, 32)
	require.NoError(t, err)
	require.GreaterOrEqual(t, report.PendingAudit, 2, "① 的两条 pending 审计必须被归一化")

	// B1 修复后残留归零。
	require.Zero(t, appPendingAuditCount(t, db, "run-limit"), "终态残留的 pending 审计必须归零")
	require.Equal(t, string(core.LLMCallUnknown), appAuditStatus(t, db, "llm_calls", "run-limit"))
	require.Zero(t, appUnreleasedRefCount(t, db, "run-refs"), "三张引用表都必须释放")

	// ③ → skipped；④ → unknown。两者都没有发过：attempted_at 仍为空。
	preRun := appDeliveryRecord(t, db, "run-pre-run")
	require.Equal(t, core.DeliverySkipped, preRun.Status)
	require.Equal(t, "运行未创建，未投递", preRun.Detail)
	require.True(t, preRun.AttemptedAt.IsZero(), "B2 不得重发")
	require.True(t, preRun.FinishedAt.After(time.UnixMilli(0)), "结算必须落收尾时刻")

	done := appDeliveryRecord(t, db, "run-done")
	require.Equal(t, core.DeliveryUnknown, done.Status)
	require.Equal(t, "进程在投递完成前退出，投递结果未知", done.Detail)
	require.True(t, done.AttemptedAt.IsZero(), "B1 不得重发")

	// ③ 的执行行由 A2 收尾：无 Run → failed + worker_lost（执行行不得永远 started）。
	require.Equal(t, string(core.ExecutionFailed), appExecutionStatus(t, db, preRunExec))
	require.Equal(t, "worker_lost", appExecutionError(t, db, preRunExec))

	// 第二轮：没有任何残留再被修复——这是"正常路径命中数为 0"的可执行判据。
	second, err := appNewReclaimer(t, db).ReclaimOnce(ctx, 32)
	require.NoError(t, err)
	require.Zero(t, second.PendingAudit)
	require.Zero(t, second.Runs)
	require.Zero(t, second.Executions)
}

// TestReclaimer_LeavesLiveExecutionContextAlone 判 B2 的所有权判据（FR-24-39 / C-29）：
// `execution_id` 对应的 `schedule_execution` 租约**仍有效**时，预写投递是属主正在推进
// 的行，回收器一眼都不能看。
func TestReclaimer_LeavesLiveExecutionContextAlone(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	appSeedScheduledTask(t, db, "task-live-pre-run")
	executionID := appSeedStartedExecution(t, db, "task-live-pre-run", sql.NullString{}, "worker-live")
	appSeedPendingDelivery(t, db, "run-live-pre-run", "task-live-pre-run", executionID)

	_, err := appNewReclaimer(t, db).ReclaimOnce(ctx, 32)
	require.NoError(t, err)

	live := appDeliveryRecord(t, db, "run-live-pre-run")
	require.Equal(t, core.DeliveryPending, live.Status, "活执行上下文的预写投递不得被结算")
	require.True(t, live.FinishedAt.IsZero())

	require.Equal(t, string(core.ExecutionStarted), appExecutionStatus(t, db, executionID),
		"活执行行不得被收尾")
	requireLeaseUntouched(t, db, core.LeaseScheduleExecution,
		strconv.FormatInt(executionID, 10), "worker-live")
}

// TestReclaimer_ExecutionStatusStaysInFourValues 判 A2 的映射（FR-24-39 / §2.3.5 A2）：
// 执行行只有四值，Run 状态**不得**原样搬过去。
func TestReclaimer_ExecutionStatusStaysInFourValues(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	// ① `limit_reached` 是达标完成，不是故障。
	appSeedTerminalRun(t, db, "run-limit-exec", core.StatusLimitReached)
	appSeedScheduledTask(t, db, "task-limit-exec")
	limitExec := appSeedStartedExecution(t, db, "task-limit-exec",
		sql.NullString{String: "run-limit-exec", Valid: true}, "worker-a")

	// ② 用户取消 → failed。
	appSeedTerminalRun(t, db, "run-cancelled-exec", core.StatusCancelled)
	appSeedScheduledTask(t, db, "task-cancelled-exec")
	cancelledExec := appSeedStartedExecution(t, db, "task-cancelled-exec",
		sql.NullString{String: "run-cancelled-exec", Valid: true}, "worker-b")

	// ③ 无 Run 行：没有任何证据说明这次执行正常结束过 → failed + worker_lost。
	appSeedScheduledTask(t, db, "task-orphan-exec")
	orphanExec := appSeedStartedExecution(t, db, "task-orphan-exec", sql.NullString{}, "worker-c")

	for _, id := range []int64{limitExec, cancelledExec, orphanExec} {
		expireLeaseForTest(t, db, core.LeaseScheduleExecution, strconv.FormatInt(id, 10))
	}

	report, err := appNewReclaimer(t, db).ReclaimOnce(ctx, 32)
	require.NoError(t, err)
	require.Equal(t, 3, report.Executions, "三条过期执行行都必须被收尾")

	require.Equal(t, string(core.ExecutionCompleted), appExecutionStatus(t, db, limitExec))
	require.Empty(t, appExecutionError(t, db, limitExec))
	require.Equal(t, string(core.ExecutionFailed), appExecutionStatus(t, db, cancelledExec))
	require.Equal(t, string(core.ExecutionFailed), appExecutionStatus(t, db, orphanExec))
	require.Equal(t, "worker_lost", appExecutionError(t, db, orphanExec))

	// 全表四值断言：任何执行行都不得出现在四值域之外（`limit_reached` 之类会违反
	// 列语义，也会让调度成功率统计把达标完成记成故障）。
	fourValues := []string{
		string(core.ExecutionStarted), string(core.ExecutionCompleted),
		string(core.ExecutionFailed), string(core.ExecutionSkipped),
	}
	statuses := appAllExecutionStatuses(t, db)
	require.NotEmpty(t, statuses)
	for _, status := range statuses {
		require.Contains(t, fourValues, status, "执行行状态只能落在四值域内")
	}
}

// TestReclaimer_TerminalResidueWaitsForConcurrentRenewal 判 B1 的**锁内重判**（review ②
// 复现项 1 / FR-24-39）：终态残留的入选谓词是事务快照上的 `NOT EXISTS(仍有效租约)`，
// 一条**尚未提交**的合法续约在快照里不可见。逐行处理前必须先在该 Run 的租约行上加锁、
// 用获锁后的数据库时钟重判 `expires_at`，再决定动不动业务行。缺这一步时，回收器会抢在
// 续约提交前把活租约覆盖的通知引用收掉（review 现场读数：`RefsReleased=1`，且续约提交
// 后租约仍有效——两者同时成立即矛盾）。
//
// 逆改点：删掉 `reclaimOneTerminalRun` 开头的 `lockLeaseRecheck` → 回收器不再排在续约
// 后面，`us24RequireBlockedByRenewal` 会先收到提前返回的读数并判红。
func TestReclaimer_TerminalResidueWaitsForConcurrentRenewal(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	ctx := t.Context()
	const runID = "run-terminal-renew"

	appSeedTerminalRun(t, env.db, runID, core.StatusCompleted)
	appSeedNotifyRef(t, env.db, runID)

	identity := core.WorkerIdentity{InstanceID: "worker-terminal-owner", Name: "terminal-owner"}
	_, err := postgres.NewLeaseStore(env.db).Acquire(ctx, core.LeaseRun, runID, identity, 2*time.Second)
	require.NoError(t, err)

	var oldExpiry int64
	require.NoError(t, env.raw.QueryRowContext(ctx, `
SELECT expires_at FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(core.LeaseRun), runID).Scan(&oldExpiry))

	// 续约在旧租约仍有效时开始，但**先不提交**：它把到期推到 ~55s 之后并持有行锁。
	renewal := us24OpenRenewal(t, env, runID, identity)
	defer func() { _ = renewal.Close() }()

	// 等墙钟越过旧到期时刻：此后扫描快照看不到在途续约，会把该 Run 选进残留集。
	require.Eventually(t, func() bool {
		var expired bool
		err := env.raw.QueryRowContext(ctx, `
SELECT (extract(epoch FROM clock_timestamp()) * 1000)::bigint > $1`, oldExpiry).Scan(&expired)
		return err == nil && expired
	}, 5*time.Second, 25*time.Millisecond)

	type residueOutcome struct {
		report postgres.TerminalResidueReport
		err    error
	}
	done := make(chan residueOutcome, 1)
	go func() {
		report, err := postgres.NewReclaimerStore(env.db).
			ReclaimTerminalResidue(context.Background(), 4, time.Now())
		done <- residueOutcome{report: report, err: err}
	}()
	us24RequireBlockedByRenewal(t, env, renewal.PID(), done)

	require.NoError(t, renewal.Commit())
	out := <-done
	require.NoError(t, out.err)
	require.Zero(t, out.report.Runs, "活租约下的残留本轮不得计入修复")
	require.Zero(t, out.report.RefsReleased, "续约已提交：不得释放活租约的引用")

	var released, live bool
	require.NoError(t, env.raw.QueryRowContext(ctx,
		`SELECT released_at IS NOT NULL FROM run_notify_refs WHERE run_id = $1`,
		runID).Scan(&released))
	require.NoError(t, env.raw.QueryRowContext(ctx, `
SELECT released_at IS NULL AND expires_at > (extract(epoch FROM clock_timestamp()) * 1000)::bigint
  FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(core.LeaseRun), runID).Scan(&live))
	require.False(t, released, "通知引用必须仍挂在活租约的 Run 上")
	require.True(t, live, "续约提交后的租约必须仍然有效")
}

// appSeedTerminalRun 直插一行指定终态的 Run（无会话、无租约、已 finished）。
// 这些用例判的是"已存在的行如何被收尾"，不走准入，所以只补齐 PG 的归属 NOT NULL 列。
func appSeedTerminalRun(t *testing.T, db *postgres.DB, runID string, status core.RunStatus) {
	t.Helper()
	now := time.Now().UTC().UnixMilli()
	_, err := db.ExecContext(context.Background(), `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at, finished_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, NULL, 'demo', 'scheduler', 'scheduler', 'cron', $2, $3, $3,
		        'local-admin', 'admin-builtin', NULL, 'scheduler')`,
		runID, string(status), now)
	require.NoError(t, err)
}

// appSeedScheduledTask 直插一条 active 任务行（`schedule_deliveries` / `task_executions`
// 都有指向它的外键）。
func appSeedScheduledTask(t *testing.T, db *postgres.DB, taskID string) {
	t.Helper()
	now := time.Now().UTC().UnixMilli()
	_, err := db.ExecContext(context.Background(), `INSERT INTO scheduled_tasks
		(task_id, agent_name, cron, zone, message, deliver_to, enabled, definition_hash,
		 created_at, updated_at, caller_id, authorizing_kind)
		VALUES ($1, 'demo', '0 */5 * * * *', 'UTC', '汇总', '', true, 'hash',
		        $2, $2, 'local-admin', 'admin-builtin')`, taskID, now)
	require.NoError(t, err)
}

// appSeedStartedExecution 直插一条 `started` 执行行，并由 owner 领到执行租约。
func appSeedStartedExecution(t *testing.T, db *postgres.DB, taskID string, runID sql.NullString,
	owner string,
) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO task_executions
		(task_id, run_id, status, trigger, started_at)
		VALUES ($1, $2, 'started', 'cron', $3) RETURNING id`,
		taskID, runID, time.Now().UTC().UnixMilli()).Scan(&id))
	_, err := postgres.NewLeaseStore(db).Acquire(ctx, core.LeaseScheduleExecution,
		strconv.FormatInt(id, 10), core.WorkerIdentity{InstanceID: owner, Name: owner}, startupReclaimTTL)
	require.NoError(t, err)
	return id
}

// appSeedPendingDelivery 直插一条 pending 投递行。executionID 为 0 时写 NULL
// （迁移遗留 / 终态行的形态）。
func appSeedPendingDelivery(t *testing.T, db *postgres.DB, runID, taskID string, executionID int64) {
	t.Helper()
	var execArg any
	if executionID > 0 {
		execArg = executionID
	}
	_, err := db.ExecContext(context.Background(), `INSERT INTO schedule_deliveries
		(run_id, task_id, agent_name, agent_uid, channel_uid, channel_name, status, detail,
		 execution_id, created_at)
		VALUES ($1, $2, 'demo', '', $3, 'alerts', 'pending', '', $4, $5)`,
		runID, taskID, appResidueChannelUID, execArg, time.Now().UTC().UnixMilli())
	require.NoError(t, err)
}

// appResidueChannelUID / appResidueAgentUID 是投递夹具的合法资源 UID 字面量。
const (
	appResidueChannelUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"
	appResidueAgentUID   = "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
)

// appSeedCredentialRef 写一条未释放的托管凭据引用。
func appSeedCredentialRef(t *testing.T, db *postgres.DB, runID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO run_credential_refs
		(run_id, credential_id, credential_version, resource_kind, resource_uid, resource_name)
		VALUES ($1, 'cred-1', 1, 'model_connection', $2, 'connection')`,
		runID, appResidueChannelUID)
	require.NoError(t, err)
}

// appSeedNotifyRef 写一条未释放的通知渠道引用。
func appSeedNotifyRef(t *testing.T, db *postgres.DB, runID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO run_notify_refs
		(run_id, agent_uid, channel_uid, channel_name)
		VALUES ($1, $2, $3, 'alerts')`, runID, appResidueAgentUID, appResidueChannelUID)
	require.NoError(t, err)
}

// appDeliveryRecord 用生产读取面读回一条投递行。
func appDeliveryRecord(t *testing.T, db *postgres.DB, runID string) core.ScheduleDeliveryRecord {
	t.Helper()
	rec, err := postgres.NewDeliveryStore(db).Get(context.Background(), runID)
	require.NoError(t, err)
	return rec
}

// appPendingAuditCount 数某 Run 仍为 pending 的审计行（两张表合计）。
func appPendingAuditCount(t *testing.T, db *postgres.DB, runID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT (SELECT count(*) FROM llm_calls WHERE run_id = $1 AND status = 'pending')
     + (SELECT count(*) FROM tool_invocations WHERE run_id = $1 AND status = 'pending')`,
		runID).Scan(&n))
	return n
}

// appUnreleasedRefCount 数某 Run 在三张引用表里仍未释放的行数。
func appUnreleasedRefCount(t *testing.T, db *postgres.DB, runID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT (SELECT count(*) FROM run_resource_refs WHERE run_id = $1 AND released_at IS NULL)
     + (SELECT count(*) FROM run_credential_refs WHERE run_id = $1 AND released_at IS NULL)
     + (SELECT count(*) FROM run_notify_refs WHERE run_id = $1 AND released_at IS NULL)`,
		runID).Scan(&n))
	return n
}

// appExecutionStatus 读执行行状态。
func appExecutionStatus(t *testing.T, db *postgres.DB, executionID int64) string {
	t.Helper()
	var status string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status FROM task_executions WHERE id = $1`, executionID).Scan(&status))
	return status
}

// appExecutionError 读执行行的错误文案（未写为 NULL）。
func appExecutionError(t *testing.T, db *postgres.DB, executionID int64) string {
	t.Helper()
	var msg string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT COALESCE(error_message, '') FROM task_executions WHERE id = $1`,
		executionID).Scan(&msg))
	return msg
}

// appAllExecutionStatuses 读出全表执行行状态（四值断言用）。
func appAllExecutionStatuses(t *testing.T, db *postgres.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT status FROM task_executions`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []string
	for rows.Next() {
		var status string
		require.NoError(t, rows.Scan(&status))
		out = append(out, status)
	}
	require.NoError(t, rows.Err())
	return out
}
