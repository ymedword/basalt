//go:build integration

package app

// 本文件守 C-34 的端到端后果（US-16 §4 的 `TestTx_UnknownRetryDoesNotDuplicateBusinessRow`）：
// 提交确认丢失（服务端已提交、客户端只看到错误）时，**不得**把它当成"已回滚"去重放业务
// 事务。协议本身的分类（三态、核查、只打主库）由 `internal/storage/postgres` 的
// `TestTx_*` 逐个钉住；本用例判的是**上层没有被"看起来失败了"骗过去**。
//
// US-24 起这两条用例的**注入点语义**变了（Q21：任务次数记账并入准入事务）：
// 运行路径上不再有"预留提交 → 业务提交"两笔，`StartRun` 是唯一的第一笔提交。
// 判据本身不退化，只是按两支行分明：
//
//   - 回查**查得到** Run 行（本文件第一条）：准入已提交，按一次执行继续——不是重放，
//     因为判定依据是新快照里**可见**的行，而不是"猜它可能提交了"；
//   - 回查**查不到**（本文件第二条）：结局仍未证实（C-34），按失败关闭返回**未知分类**，
//     既不判"未落"，也不退款、不重试。
//
// The end-to-end consequence of C-34: a lost commit ack is an unknown outcome, so nothing
// upstream may replay the business transaction or keep calling the model.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// TestTx_UnknownRetryDoesNotDuplicateBusinessRow：装上"确认丢失"钩子（真实提交、真实
// xid、只丢确认）后跑一次完整运行。钩子挂在**准入事务**之前——US-24 起 Run 行、清单、
// 引用、预留结算与 run 租约都在 `StartRun` 的同一事务里提交，它既是第一笔也是唯一一笔。
//
// 判据：回查按 run_id 读到 Run 行即证明已准入，**按一次执行继续**；业务行只出现一次、
// 模型只被调用一次、任务次数只结算一次——既没有盲目重放，也没有把已提交的 Run 丢成
// 永远 running 的孤儿行。
//
// 逆改点：`LookupAdmittedRun` 只读租约、忽略 Run 行（零 owner 装配查不到）→ 运行被拒，
// 模型零调用、Run 行停在 running；`StartRun` 把未知结局当已回滚 → 第二次重放，业务行
// 出现第二条、模型调用出现两次；`StartRun` 把未知结局静默当成功 → 回查支根本不会被走到。
//
// The lost-ack hook commits for real; a visible run row proves admission, so the run
// continues exactly once instead of being replayed or abandoned.
func TestTx_UnknownRetryDoesNotDuplicateBusinessRow(t *testing.T) {
	h := newRunHarnessWith(t, runHarnessOptions{ArmLostAckBeforeStartRun: true})

	res, err := h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err, "回查证实已准入：按一次执行继续，不得把已提交的 Run 判成失败")
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, 1, h.countRuns(t, "c1"), "不得盲目重放业务事务")
	require.Equal(t, 1, h.provider.calls(), "不得把已提交的 Run 再跑第二遍")

	used, reserved := h.taskUsage(t, "c1")
	require.Equal(t, int64(1), used, "同一次准入只结算一次")
	require.Equal(t, int64(0), reserved)
	require.Equal(t, map[string]int{"committed": 1}, h.taskReservations(t, "c1"),
		"回查成功不得补记第二条预留")
}

// TestTx_UnknownReservationCommitRefusesRun：同一个钩子换成"服务端最终回滚、客户端
// 同样只看到确认丢失"（`AbortNextCommitAfterAckLoss`）——回查查不到任何行。
//
// US-24 Q21 起任务次数的预留本就与准入同事务，所以这一支守的正是"第一笔提交的未知
// 结局"：查无行**不是**"未提交"的证据（C-34），只能按失败关闭——业务行零条、模型零
// 调用、额度不得被当成"没扣过"再扣一次；对外保持**未知分类**（不是 404）。
//
// 重跑同一 run_id（钩子已用掉）必须收敛：只结算一次、只落一条 Run。换新幂等键才会
// 记账两次，因此"重跑收敛"本身就是幂等键没变的证据。
//
// 逆改点：未知结局被当成"没落账"直接放行 → 第一次 Run 成功、第二次记账 2 次；
// 回查查无行改判"未提交"并重试 → 同样记 2 次；把回查错误当成 404 上报 → 未知分类
// 从错误链里消失，web 会把"结局未定"错报成"没有这个 Run"。
//
// The very first commit on the run path must also fail closed when its outcome stays
// unproven, and a retry of the same run id must converge on the same reservation.
func TestTx_UnknownReservationCommitRefusesRun(t *testing.T) {
	h := newRunHarness(t)
	// 钩子只作用一次：它打中的是本次准入事务的提交。
	postgres.ArmAbortNextCommitAfterAckLoss(t, h.db)

	req := h.requestFor("c1")
	req.RunID = "run-n01-lost-ack"
	_, err := h.service.Run(ctx, req)
	require.ErrorIs(t, err, postgres.ErrTxOutcomeUnknown,
		"回查查无行时未知结局必须原样上报，不得改判成'未提交'")
	require.Equal(t, 0, h.countRuns(t, "c1"), "未知结局不得放行到业务写")
	require.Zero(t, h.provider.calls())

	used, reserved := h.taskUsage(t, "c1")
	require.Equal(t, int64(0), used, "未知结局不得被当成已结算")
	require.LessOrEqual(t, reserved, int64(1), "最多一条待收口的预留，不得重复占用")

	// 重跑同一 run_id（钩子已用掉）：收敛到同一预留、只结算一次。
	res, err := h.service.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	used, reserved = h.taskUsage(t, "c1")
	require.Equal(t, int64(1), used, "重跑只结算一次")
	require.Equal(t, int64(0), reserved)
	require.Equal(t, 1, h.countRuns(t, "c1"))
}
