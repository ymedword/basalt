//go:build integration

package app

// 本文件用**真实 PostgreSQL + 真实 callers.Limiter + 生产运行入口**判 N-01 的四条
// 硬要求（裁决原文：限额达到后新任务被拒；重复请求和崩溃恢复不会重复计费或绕过限额）：
//
//  1. 限额达到 → 新任务在 [5] 被拒：不落账本行、不调模型、不写审计，额度原样；
//  2. 同一 run_id 重放 → 任务账本仍只记 1 次（幂等键 `run:<run_id>:admit`），
//     第二条账本行被仓储主键拒绝，重放因此既不能重复计费也不能绕过限额；
//  3. 崩溃恢复 → 结算前崩溃的 pending 预留继续占额度（新任务照旧被拒），重跑同一
//     run_id 只结算一次，预留标识与账本行对得上；
//  4. 零限额 → 失败关闭（零不等于不限量），且调用方之间互相隔离。
//
// 逆改点（实现改坏时哪条断言红）：
//   - 把计费挪到 StartRun 之后 → 用例 1 的"零账本行"与"零模型调用"断言红；
//   - 幂等键带 run 内序号或时间戳 → 用例 2 的"任务账本恰 1"断言红；
//   - 结算失败时 Release → 用例 3 的"pending 占额度"断言红；
//   - 零限额被当成不限量 → 用例 4 从拒绝变成执行完成。
//
// The verdict, against a real PostgreSQL: the task count is enforced before any business
// write, replayed run ids never double-charge, and a crash between reserve and settle
// neither loses the charge nor lets the next run through.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// TestRunQuota_LimitReachedRefusesNewTask 守限额耗尽：第 N+1 个任务在准入期被拒，
// 痕迹为零——账本、模型调用、审计都不该出现，额度也不再增长。
// TestRunQuota_LimitReachedRefusesNewTask: the N+1-th task is refused with no trace.
func TestRunQuota_LimitReachedRefusesNewTask(t *testing.T) {
	h := newRunHarness(t)
	const caller = runHarnessCaller
	h.setTaskLimit(t, caller, 2)

	first, err := h.invoker.Run(ctx, h.requestFor(caller))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, first.Status)

	second, err := h.invoker.Run(ctx, h.requestFor(caller))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, second.Status)
	require.Equal(t, 2, h.provider.calls(), "前置：两个任务各跑了一次模型调用")

	// 预留标识与账本行对得上（管理面据此把 Run 与 quota_reservations 关联）。
	require.NotEmpty(t, h.runReservationID(t, first.RunID), "账本行必须记下本次任务的预留")
	require.NotEqual(t, h.runReservationID(t, first.RunID), h.runReservationID(t, second.RunID),
		"两个逻辑 Run 各有一条预留")

	third, err := h.invoker.Run(ctx, h.requestFor(caller))
	require.ErrorIs(t, err, callers.ErrQuotaExhausted, "限额耗尽必须是可映射的哨兵")
	require.Empty(t, third.RunID, "被拒的任务没有运行标识")
	require.Equal(t, 2, h.countRuns(t, caller), "被拒的任务不得留下账本行")
	require.Equal(t, 2, h.provider.calls(), "被拒的任务不得发出模型调用")
	require.Equal(t, 0, h.tools.invocationCount(), "被拒的任务不得执行工具")

	used, reserved := h.taskUsage(t, caller)
	require.Equal(t, int64(2), used, "两个已完成任务各记 1 次")
	require.Equal(t, int64(0), reserved, "预留已全部结算，没有残留 pending")
	require.Equal(t, map[string]int{"committed": 2}, h.taskReservations(t, caller))

	// 调用方之间隔离：另一个调用方的额度不受影响。
	h.ensureCaller(t, "c2")
	other, err := h.invoker.Run(ctx, h.requestFor("c2"))
	require.NoError(t, err, "一个调用方耗尽不得影响另一个")
	require.Equal(t, core.StatusCompleted, other.Status)
	c2Used, _ := h.taskUsage(t, "c2")
	require.Equal(t, int64(1), c2Used)
}

// TestRunQuota_DuplicateRunRequestChargedOnce 守重复请求：同一 run_id 的第二条请求
// 在账本主键处失败，任务账本仍只有 1 次——重放不产生第二笔消耗，也没能借重放绕过
// 限额再跑一次业务。
// A replayed run id is charged exactly once and never executes a second time.
func TestRunQuota_DuplicateRunRequestChargedOnce(t *testing.T) {
	h := newRunHarness(t)
	const caller = runHarnessCaller
	h.setTaskLimit(t, caller, 5)

	req := h.requestFor(caller)
	req.RunID = "run-n01-dup"
	first, err := h.invoker.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, first.Status)
	require.Equal(t, 1, h.provider.calls())

	second, err := h.invoker.Run(ctx, req)
	require.Error(t, err, "同一 run_id 不得产生第二条账本行")
	require.NotErrorIs(t, err, callers.ErrQuotaExhausted, "这不是配额拒绝，而是账本主键拒绝")
	require.Empty(t, second.RunID)
	require.Equal(t, 1, h.provider.calls(), "重放不得再执行业务")

	used, reserved := h.taskUsage(t, caller)
	require.Equal(t, int64(1), used, "重复请求不得重复计费")
	require.Equal(t, int64(0), reserved)
	require.Equal(t, map[string]int{"committed": 1}, h.taskReservations(t, caller))
	require.Equal(t, 1, h.countRuns(t, caller))
}

// TestRunQuota_CrashRecoverySettlesPendingWithoutBypass 守崩溃恢复：预留在 [5] 落下、
// 结算前崩溃时，pending 继续占额度（新任务照旧被拒）；重跑同一 run_id 收敛到同一条
// 预留并只结算一次——既不重复计费，也不因为"反正没跑成"放行下一个任务。
// A crash between reserve and settle keeps the quota occupied; the retry settles it once.
func TestRunQuota_CrashRecoverySettlesPendingWithoutBypass(t *testing.T) {
	h := newRunHarness(t)
	const caller = runHarnessCaller
	h.setTaskLimit(t, caller, 1)
	const runID = "run-n01-recover"

	// 模拟崩溃现场：同键预留落下、Commit 没来得及发。生产路径的键由 run_id 派生，
	// 这里手工复用同一派生规则（`run:<run_id>:admit`）。
	ticket, err := h.limiter.AdmitCall(ctx, h.principal(caller), callers.ClassRun,
		callers.Cost{Tasks: 1}, "run:"+runID+":admit")
	require.NoError(t, err)

	used, reserved := h.taskUsage(t, caller)
	require.Equal(t, int64(0), used, "崩溃现场：还没结算")
	require.Equal(t, int64(1), reserved, "pending 预留必须占额度")

	// pending 期间，另一个 run_id 的新任务照旧被拒——崩溃不是放行理由。
	_, err = h.invoker.Run(ctx, h.requestFor(caller))
	require.ErrorIs(t, err, callers.ErrQuotaExhausted, "pending 占额度，限额不得被绕过")
	require.Equal(t, 0, h.countRuns(t, caller))

	// 恢复：同一 run_id 重跑，预留收敛到同一行、结算一次。
	req := h.requestFor(caller)
	req.RunID = runID
	res, err := h.invoker.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	used, reserved = h.taskUsage(t, caller)
	require.Equal(t, int64(1), used, "重跑只结算一次")
	require.Equal(t, int64(0), reserved, "预留已收口")
	require.Equal(t, map[string]int{"committed": 1}, h.taskReservations(t, caller),
		"同一条预留被复用，没有第二条")
	require.Equal(t, ticket.ReservationID, h.runReservationID(t, runID),
		"账本行记的正是恢复的那条预留")

	// 额度已用完：再来一个新任务仍然被拒（恢复没有白送一次额度）。
	_, err = h.invoker.Run(ctx, h.requestFor(caller))
	require.ErrorIs(t, err, callers.ErrQuotaExhausted)
}

// TestRunQuota_ZeroLimitFailsClosed 守零限额：失败关闭——不是"不限量"，也不落到
// "先跑后算"。零限额是缺省列值，这条用例同时是升级后果的书面证据。
// A zero task limit fails closed rather than meaning unlimited.
func TestRunQuota_ZeroLimitFailsClosed(t *testing.T) {
	h := newRunHarness(t)
	const caller = runHarnessCaller
	h.setTaskLimit(t, caller, 0)

	_, err := h.invoker.Run(ctx, h.requestFor(caller))
	require.ErrorIs(t, err, callers.ErrQuotaExhausted)
	require.Equal(t, 0, h.countRuns(t, caller))
	require.Equal(t, 0, h.provider.calls())
	require.Empty(t, h.taskReservations(t, caller), "零限额连预留行都不该写")
}
