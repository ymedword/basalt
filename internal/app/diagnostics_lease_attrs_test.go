//go:build integration

package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 本文件是 US-24 T032 / C-23 / FR-24-26 的判据：诊断事件的归属两项
// （`worker_instance_id` / `ownership_epoch`）**只在写入进程确实持有该 Run 租约时**
// 出现；归属代次随交接递增；事件只承诺**局部有序**——不得引入跨节点全局序号，也不得
// 把接收（写入）顺序当执行顺序。
// 逆改点（三条，任一即可让本用例红）：
//   - 去掉写入路径的归属比对（只按租约行回填）→ "旧属主迟到事件不得被标成新属主"红；
//   - 把归属两项从白名单删掉 → 读回来的 attrs 全部缺键（读面二次脱敏会丢弃）；
//   - 引入全局序号或按写入顺序返回 → 乱序写入的读数与 seq 断言红。
//
// TestDiagnostics_LeaseAttrsWithoutGlobalSequence pins the lease attribution attrs and
// proves there is neither a global sequence nor arrival-order semantics.

// diagClock 是可回拨的测试时钟：把"写入顺序"和"发生顺序"拉开或颠倒。
// diagClock is a rewindable clock, used to separate arrival order from occurrence order.
type diagClock struct{ at time.Time }

func (c *diagClock) now() time.Time { return c.at }

// seedDiagnosticRun 种一条 running 的 Run 行与其会话，**不建租约**：租约由用例显式
// 领取，归属两项的判据正是"有没有租约、属主是谁"。
// seedDiagnosticRun seeds a running run row without any lease.
func seedDiagnosticRun(t *testing.T, db *postgres.DB, runID, sessionID string) {
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
}

// diagEventByStep 按步骤标识取事件；缺失即失败（避免用下标猜顺序）。
func diagEventByStep(t *testing.T, events []diagnostics.DiagnosticEvent, step string) diagnostics.DiagnosticEvent {
	t.Helper()
	for _, ev := range events {
		if ev.StepID == step {
			return ev
		}
	}
	t.Fatalf("没有找到 step_id=%q 的诊断事件（共 %d 条）", step, len(events))
	return diagnostics.DiagnosticEvent{}
}

// diagRecordLLM 记一条带步骤标识的模型调用事件（最小合法形状）。
func diagRecordLLM(rec *diagnostics.Recorder, runID, step string, seq int64) {
	rec.Record(context.Background(), diagnostics.DiagnosticEvent{
		Kind:        diagnostics.KindLLMCall,
		Correlation: diagnostics.Correlation{RunID: runID},
		Actor:       diagnostics.ActorCaller,
		StepID:      step,
		Seq:         seq,
		Decision:    diagnostics.DecisionAllow,
	})
}

// TestDiagnostics_LeaseAttrsWithoutGlobalSequence 覆盖四组判据：
// ① 本进程持租约 → 归属两项落在 attrs 上；② 无租约 / 旧属主迟到 → 不写归属两项；
// ③ 交接后代次递增且旧事件保持原代次；④ 写入顺序不等于发生顺序、seq 只是局部序号。
func TestDiagnostics_LeaseAttrsWithoutGlobalSequence(t *testing.T) {
	ctx := context.Background()
	db := pgtest.NewTestDB(t)
	leases := postgres.NewLeaseStore(db)

	const (
		runID   = "run-lease-attrs"
		otherID = "run-without-lease"
	)
	alpha := core.WorkerIdentity{InstanceID: "worker-alpha", Name: "alpha"}
	beta := core.WorkerIdentity{InstanceID: "worker-beta", Name: "beta"}
	seedDiagnosticRun(t, db, runID, "s-lease-attrs")
	seedDiagnosticRun(t, db, otherID, "s-no-lease")

	// 每个 Worker 一套**生产装配**（`newDiagnosticsPortsWithMetrics`）：这里判的就是
	// 装配把进程身份传进写入路径，而不是另拼一个测试专用的仓储。
	clock := &diagClock{at: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)}
	alphaRec, reader, err := newDiagnosticsPortsWithMetrics(cli.DiagnosticsConfig{}, db, alpha,
		clock.now, slog.New(slog.DiscardHandler), nil)
	require.NoError(t, err)

	// ① alpha 领租约（epoch 1），事件带归属两项。
	alphaLease, err := leases.Acquire(ctx, core.LeaseRun, runID, alpha, time.Minute)
	require.NoError(t, err)
	require.EqualValues(t, 1, alphaLease.OwnershipEpoch)

	diagRecordLLM(alphaRec, runID, "alpha-before", 1)

	events, err := reader.List(ctx, diagnostics.Query{RunID: runID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, alpha.InstanceID, events[0].Attrs["worker_instance_id"],
		"持有租约的进程必须把自己写进归属")
	require.EqualValues(t, 1, events[0].Attrs["ownership_epoch"])

	// ② 同一个进程写**没有租约**的 Run：attrs 不得凭空长出归属两项。
	// 同一毫秒之外再写，避免依赖 event_id 定序。
	clock.at = clock.at.Add(time.Second)
	diagRecordLLM(alphaRec, otherID, "unleased", 1)
	events, err = reader.List(ctx, diagnostics.Query{RunID: otherID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotContains(t, events[0].Attrs, "worker_instance_id", "无租约的 Run 不得带归属")
	require.NotContains(t, events[0].Attrs, "ownership_epoch")

	// ③ 交棒给 beta：正常交棒同样推进代次（接管走同一递增路径，由租约用例另行钉住）。
	require.NoError(t, leases.Release(ctx, alphaLease.LeaseToken))
	betaLease, err := leases.Acquire(ctx, core.LeaseRun, runID, beta, time.Minute)
	require.NoError(t, err)
	require.EqualValues(t, 2, betaLease.OwnershipEpoch, "交接后代次必须递增")

	betaRec, _, err := newDiagnosticsPortsWithMetrics(cli.DiagnosticsConfig{}, db, beta,
		clock.now, slog.New(slog.DiscardHandler), nil)
	require.NoError(t, err)

	// 旧属主在失去租约之后仍可能冒出一条迟到的诊断：**不得**被标成新属主。
	clock.at = clock.at.Add(time.Second)
	diagRecordLLM(alphaRec, runID, "alpha-late", 2)
	clock.at = clock.at.Add(time.Second)
	diagRecordLLM(betaRec, runID, "beta-after", 3)

	// ④ 把时钟回拨再写一条：写入顺序是 alpha-late → beta-after → beta-backdated，
	// 发生顺序却是 beta-backdated → alpha-late → beta-after。读数必须按发生顺序，
	// 也就是**接收顺序不等于执行顺序**。
	clock.at = clock.at.Add(-2 * time.Second)
	diagRecordLLM(betaRec, runID, "beta-backdated", 4)

	events, err = reader.List(ctx, diagnostics.Query{RunID: runID})
	require.NoError(t, err)
	require.Equal(t, []string{"alpha-before", "beta-backdated", "alpha-late", "beta-after"},
		diagSteps(events), "读序必须按 occurred_at，不是写入 / 接收顺序")

	before := diagEventByStep(t, events, "alpha-before")
	require.Equal(t, alpha.InstanceID, before.Attrs["worker_instance_id"])
	require.EqualValues(t, 1, before.Attrs["ownership_epoch"], "旧事件保持原代次，不被新租约改写")

	late := diagEventByStep(t, events, "alpha-late")
	require.NotContains(t, late.Attrs, "worker_instance_id",
		"失去租约的旧属主不得把迟到事件标成新属主")
	require.NotContains(t, late.Attrs, "ownership_epoch")

	backdated := diagEventByStep(t, events, "beta-backdated")
	require.Equal(t, beta.InstanceID, backdated.Attrs["worker_instance_id"])
	require.EqualValues(t, 2, backdated.Attrs["ownership_epoch"])

	after := diagEventByStep(t, events, "beta-after")
	require.Equal(t, beta.InstanceID, after.Attrs["worker_instance_id"])
	require.EqualValues(t, 2, after.Attrs["ownership_epoch"])

	// ⑤ 没有跨节点全局序号：`seq` 仍只是同一 correlation 内的局部序号（两个不同 Run
	// 各自从 1 起），归属两项之外不得出现任何排序 / 计数用的新键。
	require.EqualValues(t, 1, before.Seq)
	require.EqualValues(t, 1, diagEventByStep(t, mustList(t, ctx, reader, otherID), "unleased").Seq,
		"seq 是局部序号：另一个 Run 的同一序号不构成全局序")
	for _, ev := range events {
		require.NotContains(t, ev.Attrs, "seq", "不得把局部序号塞进 attrs 冒充全局序")
		require.NotContains(t, ev.Attrs, "global_seq")
		require.NotContains(t, ev.Attrs, "global_order")
	}
}

// mustList 读一个 Run 的事件页；失败即中止用例。
func mustList(t *testing.T, ctx context.Context, reader *diagnostics.Reader, runID string) []diagnostics.DiagnosticEvent {
	t.Helper()
	events, err := reader.List(ctx, diagnostics.Query{RunID: runID})
	require.NoError(t, err)
	return events
}

// diagSteps 把一页事件投影成步骤标识列表，让顺序断言可逐项对照。
func diagSteps(events []diagnostics.DiagnosticEvent) []string {
	steps := make([]string, len(events))
	for i, ev := range events {
		steps[i] = ev.StepID
	}
	return steps
}
