//go:build integration

package app

// 本文件守 C-34 的端到端后果（US-16 §4 的 `TestTx_UnknownRetryDoesNotDuplicateBusinessRow`）：
// 提交确认丢失（服务端已提交、客户端只看到错误）时，**不得**把它当成"已回滚"去重放业务
// 事务。判据落在三处后果上——未知结局必须显式上报、业务行只出现一次、不得继续发出模型
// 调用。协议本身的分类（三态、核查、只打主库）由 `internal/storage/postgres` 的
// `TestTx_*` 逐个钉住；本用例判的是**上层没有被"看起来失败了"骗过去**。
// The end-to-end consequence of C-34: a lost commit ack is an unknown outcome, so nothing
// upstream may replay the business transaction or keep calling the model.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// TestTx_UnknownRetryDoesNotDuplicateBusinessRow：装上"确认丢失"钩子（真实提交、真实
// xid、只丢确认）后跑一次完整运行。钩子挂在**业务事务**（账本行 + 清单 + 引用）之前
// ——N-01 起运行路径前面还有配额事务，钩子若不精确挂载就会打中预留提交，判的就不再是
// C-34 保护的那笔业务写（见下一个用例）。
//
// 逆改点：`withTx` 把任何提交错误都当 `TxRolledBack` 返回（或配额路径改按"失败即重试"
// 重放）→ 上层把这次运行当成"没落账"继续走，业务行出现第二条、模型调用照常发出，
// 两条后果断言同时红；`withTx` 把未知结局静默当成功 → `ErrTxOutcomeUnknown` 从错误链
// 里消失，第一条断言红。
//
// The lost-ack hook commits for real; the assertions cover what upstream must not infer.
func TestTx_UnknownRetryDoesNotDuplicateBusinessRow(t *testing.T) {
	h := newRunHarnessWith(t, runHarnessOptions{ArmLostAckBeforeStartRun: true})

	_, err := h.service.Run(ctx, h.requestFor("c1"))
	require.ErrorIs(t, err, postgres.ErrTxOutcomeUnknown, "未知结果必须显式上报")
	require.Equal(t, 1, h.countRuns(t, "c1"), "不得盲目重放业务事务")
	require.Zero(t, h.provider.calls(), "提交结果未知时不得继续发出模型调用")
}

// TestTx_UnknownReservationCommitRefusesRun 守 N-01 引入的**第一个**提交：任务次数的
// 预留提交丢确认时，Run 在准入期就被拒——业务行零条、模型零调用、额度不得被当成
// "没扣过"再扣一次；重跑同一 run_id 收敛到同一预留并只结算一次。
//
// 逆改点：预留的未知结局被当成"没落账"直接放行 → 第二次 Run 成功、任务账本记 2 次；
// 重跑时换新幂等键 → 任务账本同样记 2 次，两条断言之一红。
//
// The very first commit on the run path (the task reservation) must also fail closed, and
// a retry of the same run id must converge on the same reservation.
func TestTx_UnknownReservationCommitRefusesRun(t *testing.T) {
	h := newRunHarness(t)
	// 钩子只作用一次：它打中的是本次预留的提交。
	postgres.ArmLoseNextCommitAck(t, h.db)

	req := h.requestFor("c1")
	req.RunID = "run-n01-lost-ack"
	_, err := h.service.Run(ctx, req)
	require.ErrorIs(t, err, postgres.ErrTxOutcomeUnknown, "预留提交的未知结局必须显式上报")
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
