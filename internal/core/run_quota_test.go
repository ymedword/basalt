package core_test

// 本文件守 Run 级任务次数接线的**运行入口语义**（US-16 N-01）：端口被调用几次、在
// 什么时刻被调用、拒绝时留下多少痕迹、预留标识怎么进账本。适配器侧的幂等键与结算
// 口径在 `internal/app/run_quota_test.go`，真实 PostgreSQL 的限额效果在
// `internal/app/run_quota_integration_test.go`——三层各判一件事，不重复。
//
// 逆改点（实现改坏时哪条断言红）：
//   - 把计费挪到 `StartRun` 之后 → `TestRunService_RunQuotaRefusalLeavesNoTrace` 的
//     "零账本行"断言红（拒绝了却留下 running 行）；
//   - 把预留标识丢掉不写账本 → `TestRunService_RunQuotaReservationReachesLedger` 红；
//   - 每轮模型调用都记一次任务 → `TestRunService_RunQuotaChargesOncePerRun` 红；
//   - 计费早于快照落库 → `TestRunService_RunQuotaSitsBetweenEvidenceAndLedger` 的
//     "调用时刻快照已落库、账本尚未创建"断言红；
//   - 拒绝时把它降级成 Run failed 结果 → 第一个用例的 `require.Error` 红（拒绝是
//     函数级错误，调用方据此映射 429，而不是"跑失败了"）。
//
// The verdict: the port is charged exactly once per admitted run, after the evidence
// writes and before the ledger row, and a refusal leaves no trace at all.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errRunQuotaDenied 是本文件用的拒绝哨兵（core 不依赖 callers，哨兵由用例自造）。
// errRunQuotaDenied stands in for the callers sentinel core must not import.
var errRunQuotaDenied = errors.New("run quota denied")

// recordingRunQuota 记录每次调用的入参，并按脚本返回结果。
// recordingRunQuota records every call and returns a scripted outcome.
type recordingRunQuota struct {
	calls []core.RunQuotaRequest

	err         error
	reservation string

	// 调用时刻的现场（顺序判据）：快照份数与账本行数。
	snapshotsAt int
	runsAt      int
	store       *fakeStore
}

// ChargeRun 记录入参、采下现场，再返回脚本结果。
// ChargeRun records the request, samples the store, then returns the scripted outcome.
func (q *recordingRunQuota) ChargeRun(_ context.Context, req core.RunQuotaRequest) (core.RunQuotaResult, error) {
	q.calls = append(q.calls, req)
	if q.store != nil {
		q.snapshotsAt = q.store.snapshotsOf()
		q.runsAt = q.store.runsOf()
	}
	if q.err != nil {
		return core.RunQuotaResult{}, q.err
	}
	return core.RunQuotaResult{ReservationID: q.reservation}, nil
}

// TestRunService_RunQuotaRefusalLeavesNoTrace 判拒绝路径：函数级 error 上抛，
// 账本零行、用户消息零条、模型零调用——被任务配额拒下的 Run 不得先跑再算。
// TestRunService_RunQuotaRefusalLeavesNoTrace: a refusal leaves no ledger row, no user
// message and no model call.
func TestRunService_RunQuotaRefusalLeavesNoTrace(t *testing.T) {
	q := &recordingRunQuota{err: errRunQuotaDenied}
	h := newLoopHarness(t, withRunQuota(q))
	h.provider.onCall(0, textResponse("done"))

	_, err := h.service.Run(ctx, h.req("跑一下"))
	require.ErrorIs(t, err, errRunQuotaDenied, "拒绝必须是函数级错误，不是 Run failed 结果")
	require.Len(t, q.calls, 1)

	require.Equal(t, 0, h.store.runsOf(), "被拒的 Run 不得留下账本行")
	require.Equal(t, 0, h.store.countByRole(t, h.sessionID, core.RoleUser), "用户消息不得先写")
	require.Equal(t, 0, h.provider.callCount(), "零模型调用")
}

// TestRunService_RunQuotaReservationReachesLedger 判预留标识入账：账本行的
// `reservation_id` 必须等于端口返回的标识——管理面靠它把 Run 与预留行对上。
// The reservation id returned by the port must land on the ledger row.
func TestRunService_RunQuotaReservationReachesLedger(t *testing.T) {
	q := &recordingRunQuota{reservation: "res-run-1"}
	h := newLoopHarness(t, withRunQuota(q))
	h.provider.onCall(0, textResponse("done"))

	res, err := h.service.Run(ctx, h.req("跑一下"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	rec, ok := h.store.runs[res.RunID]
	require.True(t, ok, "账本行必须存在")
	require.Equal(t, "res-run-1", rec.ReservationID, "预留标识必须写进账本行")
}

// TestRunService_RunQuotaRequestCarriesIdentityAndRunID 判端口入参：记到哪个调用方、
// 按哪个 run_id 派生幂等键——两者都错一处，限额就会记到别人头上或无法重放去重。
// The port must receive the caller identity and the generated run id.
func TestRunService_RunQuotaRequestCarriesIdentityAndRunID(t *testing.T) {
	q := &recordingRunQuota{}
	h := newLoopHarness(t, withRunQuota(q))
	h.provider.onCall(0, textResponse("done"))

	req := h.req("跑一下")
	req.RunID = "run-preset-7"
	res, err := h.service.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "run-preset-7", res.RunID, "调用方给了 run_id 就原样用")

	require.Len(t, q.calls, 1)
	require.Equal(t, "run-preset-7", q.calls[0].RunID, "幂等键的派生源必须是本次 Run 的 run_id")
	require.Equal(t, req.Principal.CallerID, q.calls[0].Principal.CallerID, "归属必须是本次 Run 的调用方")
}

// TestRunService_RunQuotaChargesOncePerRun 判计数单位：一次逻辑 Run 恰一次，哪怕
// 循环跑了两轮模型调用、执行了一次工具。
// One logical run costs one task regardless of how many model/tool rounds it takes.
func TestRunService_RunQuotaChargesOncePerRun(t *testing.T) {
	q := &recordingRunQuota{reservation: "res-run-2"}
	h := newLoopHarness(t, withRunQuota(q))
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("done"))

	_, err := h.service.Run(ctx, h.req("读一下"))
	require.NoError(t, err)
	require.Equal(t, 2, h.provider.callCount(), "前置：确实跑了两轮模型调用")
	require.Len(t, q.calls, 1, "两轮调用仍然只算一次任务")
}

// TestRunService_RunQuotaSitsBetweenEvidenceAndLedger 判计时点：调用发生时配置快照
// 已落库（证据先行）、账本行尚未创建（拒绝不留下 running 行）。两端各有理由——
// 早于快照会把"根本没跑"的 Run 算成消耗，晚于账本能被钻空子。
// The charge happens after the snapshot and before the ledger row.
func TestRunService_RunQuotaSitsBetweenEvidenceAndLedger(t *testing.T) {
	q := &recordingRunQuota{reservation: "res-run-3"}
	h := newLoopHarness(t, withRunQuota(q))
	q.store = h.store
	h.provider.onCall(0, textResponse("done"))

	_, err := h.service.Run(ctx, h.req("跑一下"))
	require.NoError(t, err)

	require.Equal(t, 1, q.snapshotsAt, "计费时配置快照必须已经落库")
	require.Equal(t, 0, q.runsAt, "计费时账本行必须还没创建（拒绝因此不留 running 行）")
	require.Equal(t, 1, h.store.runsOf(), "放行之后账本行照常落下")
}
