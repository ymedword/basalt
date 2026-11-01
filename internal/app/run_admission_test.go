//go:build integration

package app

// R18 / C-35 —— 准入顺序与名额生命周期（颗粒度文档 §4.4）。判据的主角是生产路径：
// `web.RunService` → `runAdmission.run` → `core.AgentService.Run` → 自实现循环。
// **测试不代调 `ReleaseRun`**：名额的归还必须来自 `runAdmission` 的 `defer`，否则
// "生产路径漏释放"这条缺陷在这批用例里再也测不出来（C-35 的原始理由）。
// The cases below judge the production path; none of them releases the run slot itself.
//
// 逆改点（§4.4 原文）：
//   - 在 `AdmitCall` 里再 `Acquire` 并发 → 多步 Run 撞自己的名额；
//   - 速率判定放回 `AdmitRun` → 后台调度把 HTTP 桶吃光；
//   - HTTP 摄取不经过 `AdmitIngress` → 上传永不被限速；
//   - 生产路径漏 `ReleaseRun` → 名额不归零、下一个 Run 被并发上限挡住。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// TestIngress_InternalCallsDoNotRecharge —— 一次 Run 内的多次模型调用**不得**再扣
// 入口速率（入口速率只属于 HTTP 边界，图 6 [1]）。
//
// 逆改点：把 `AdmitIngress` 挪进循环或运行入口 → 桶容量为 1 时第二次内部调用被拒，
// `provider.calls()` 停在 1 且 Run 失败。
// Mutation: charging ingress per model call drains the caller's own budget mid-run.
func TestIngress_InternalCallsDoNotRecharge(t *testing.T) {
	h := newRunHarness(t)
	h.setIngressQPS(t, "c1", callers.IngressRun, 1)
	h.provider.respondWithToolCalls(3) // 一次 Run 内 4 次模型调用

	_, err := h.service.Run(ctx, h.requestFor("c1")) // 只有一个 HTTP 请求
	require.NoError(t, err)
	require.Equal(t, 4, h.provider.calls(), "内部调用不得再扣入口速率")
	require.Zero(t, h.ingressRejections(t, "c1"), "多步 Run 不得把自己的速率额度吃光")
}

// TestIngress_SchedulerBypassesIngressBucket —— 后台触发没有 HTTP 入口，**不得**扣
// 入口速率（C-35：调度 / 恢复不扣），但要占名额。
//
// 逆改点：把速率判定挪进 `AdmitRun` → 第二次触发被桶拒（容量 1），`calls()` 停在 1。
// Mutation: moving the rate check into AdmitRun makes background triggers drain the
// HTTP bucket of the external callers.
func TestIngress_SchedulerBypassesIngressBucket(t *testing.T) {
	h := newRunHarness(t)
	h.ensureCaller(t, "caller-2") // 夹具：调用方行与缺省限额（不是判定面）
	h.setIngressQPS(t, "caller-2", callers.IngressRun, 1)

	h.fire(t, "caller-2")
	h.fire(t, "caller-2")
	require.Equal(t, 2, h.provider.calls(), "后台触发没有 HTTP 入口，不得扣入口速率")
	require.Zero(t, h.ingressRejections(t, "caller-2"))
}

// TestIngress_RateDeniedDoesNotTakeRunSlot —— 图 6 [1] 的速率拒绝必须发生在 [3] 之前：
// 被限流的请求**不**进入运行入口，因此也不占并发名额（FR-034 / SC-003）。
//
// `internal/web` 的 `ingress_run_test.go` 用真实路由表 + 记录式运行入口判同一性质；
// 这里从**真实 RunService + 真实限额器**再判一遍：生产链路里 [2] 与 [3] 的先后
// 不依赖中间件与服务的相对位置。
// 逆改点：把 `AdmitIngress` 挪到 `RunService.Run` 之后 → 第二次请求先占名额再被拒，
// `concurrencyInUse` 在拒绝后不为零。
// The web test pins the route table; this one pins the real service behind it.
func TestIngress_RateDeniedDoesNotTakeRunSlot(t *testing.T) {
	h := newRunHarness(t)
	h.setIngressQPS(t, "c1", callers.IngressRun, 1)

	result, err := h.postChat(ctx, "c1")
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, result.Status, "阳性对照：第一次请求必须真的跑完")
	before := h.concurrencyInUse(t, "c1")

	_, err = h.postChat(ctx, "c1")
	require.ErrorIs(t, err, callers.ErrRateLimited, "第二次请求必须被入口速率拒绝")
	require.Equal(t, before, h.concurrencyInUse(t, "c1"), "被入口速率拒绝时尚未占名额")
}

// TestRunService_QuotaDeniedReleasesRunSlot —— 图 6 [5] 的配额拒绝是**准入期拒绝**
// （走错误通道），但 [3] 已经占住的名额必须由生产路径归还。
//
// 逆改点：`runAdmission` 只在成功路径归还 → 第二个 Run 被自己的名额挡住。
// Mutation: releasing only on success strands the slot after a quota refusal.
func TestRunService_QuotaDeniedReleasesRunSlot(t *testing.T) {
	h := newRunHarness(t)
	h.setMaxConcurrent(t, "c1", 1)
	h.setLimit(t, "c1", 1) // 配额不足

	_, err := h.service.Run(ctx, h.requestFor("c1"))
	require.ErrorIs(t, err, callers.ErrQuotaExhausted) // 准入期拒绝：仍走错误通道
	require.Zero(t, h.concurrencyInUse(t, "c1"), "配额拒绝后生产路径必须归还名额")

	h.setLimit(t, "c1", 1000)
	h.provider.respondWithText("ok")
	res, err := h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err, "名额已归零，下一个 Run 必须可准入")
	require.Equal(t, core.StatusCompleted, res.Status, "下一个 Run 必须真的跑完")
	require.NotEmpty(t, res.FinalText, "并且产出预期结果")
}

// TestRunService_UnknownUsageReleasesRunSlot —— 调用超时、用量未知时配额预留保持
// `pending`（C-29），但**名额**必须归还：未结算的预留不得钉死并发（C-35）。
//
// 逆改点：把名额归还挂在"结算成功"分支上 → 超时那次把名额永久钉住。
// Mutation: tying the slot release to a successful settlement leaks it on timeout.
func TestRunService_UnknownUsageReleasesRunSlot(t *testing.T) {
	h := newRunHarness(t)
	h.setMaxConcurrent(t, "c1", 1)
	h.setLimit(t, "c1", 1000)
	h.provider.failAfterTimeout() // 调用超时、用量未知 → 配额保持 pending

	res, err := h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err, "运行期 Provider 失败走 RunResult，不经 error（契约 §2）")
	require.Equal(t, core.StatusFailed, res.Status)
	require.NotEmpty(t, res.Reason, "failed 必须带 Reason（契约 §2 表）")
	require.Equal(t, int64(100), h.usageReserved(t, "c1"), "用量未知的预留继续占额度")
	require.Zero(t, h.concurrencyInUse(t, "c1"), "名额必须由生产路径归还")

	h.provider.respondWithText("ok")
	res, err = h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err, "未结算的预留不得阻塞后续 Run 的名额")
	require.Equal(t, core.StatusCompleted, res.Status, "下一个 Run 必须真的执行完成")
	require.NotEmpty(t, res.FinalText)
}

// TestRunService_SessionRejectedReleasesRunSlot —— 图 6 [4] 的会话串行拒绝发生在
// [3] 之后：既然 [3] 已经占了名额，[4] 拒绝就必须归还，且不得留下任何预留。
//
// 逆改点：先判会话再占名额（顺序反了）→ `before` 恒为 0，`[4] 拒绝必须归还` 失去
// 判别力；漏归还 → 第二次 Run 被名额挡住。
// Mutation: leaking the slot on a session-serial refusal, or reserving before [4].
func TestRunService_SessionRejectedReleasesRunSlot(t *testing.T) {
	h := newRunHarness(t)
	h.setMaxConcurrent(t, "c1", 1)
	h.sessions.MarkBusy(t, "s1") // [4] 会话串行拒绝：同会话并行 409
	before := h.concurrencyInUse(t, "c1")

	_, err := h.service.Run(ctx, h.requestForSession("c1", "s1"))
	require.ErrorIs(t, err, core.ErrConflict) // 准入期拒绝：仍走错误通道
	require.Equal(t, before, h.concurrencyInUse(t, "c1"), "[4] 拒绝必须归还 [3] 已占的名额")
	require.Zero(t, h.usageReserved(t, "c1"), "被 [4] 拒绝时不得留下预留")

	h.provider.respondWithText("ok")
	res, err := h.service.Run(ctx, h.requestForSession("c1", "s2"))
	require.NoError(t, err, "名额已归零，下一个 Run 必须可准入")
	require.Equal(t, core.StatusCompleted, res.Status, "下一个 Run 必须真的执行完成")
	require.NotEmpty(t, res.FinalText)
}

// TestRunService_CancelReleasesRunSlot —— 取消是运行期终止（`Status=cancelled`、
// `error=nil`），名额同样必须归还。
//
// 逆改点：归还前先判 `ctx.Err()` → 取消路径永久泄漏名额（`ReleaseRun` 因此不看 ctx）。
// Mutation: consulting cancellation before releasing leaks a slot per cancel.
func TestRunService_CancelReleasesRunSlot(t *testing.T) {
	h := newRunHarness(t)
	h.setMaxConcurrent(t, "c1", 1)
	h.provider.blockUntilCanceled()

	ctxRun, cancel := context.WithCancel(ctx)
	go func() { h.provider.waitFirstCall(); cancel() }()
	res, err := h.service.Run(ctxRun, h.requestFor("c1"))
	require.NoError(t, err, "取消是运行期终止，走 RunResult（契约 §2：cancelled → nil）")
	require.Equal(t, core.StatusCancelled, res.Status)
	require.Zero(t, h.concurrencyInUse(t, "c1"), "取消路径同样必须归还名额")

	h.provider.respondWithText("ok")
	res, err = h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "取消后下一个 Run 必须真的执行完成")
	require.NotEmpty(t, res.FinalText)
}

// TestRunService_PanicReleasesRunSlot —— 工具执行 panic 是运行期致命错误：
// `Status=failed` + `Reason` 点名 panic，且名额必须归还。
//
// 逆改点：`recover` 放在归还之前（或没有 recover）→ 名额泄漏 / 进程崩溃。
// Mutation: recovering before the deferred release, or not recovering at all.
func TestRunService_PanicReleasesRunSlot(t *testing.T) {
	h := newRunHarness(t)
	h.setMaxConcurrent(t, "c1", 1)
	// 武装工具只是"执行时 panic"；**模型必须先要求调用它**，武装才可能被触发。
	// 夹具默认剧本是一次纯文本回答（其余用例按 1 次模型调用计账），因此这里显式给出
	// 一次工具调用。断言与颗粒度文档 §4.4 逐条一致。
	// Arming only panics if the model actually calls the tool; the default script is a
	// plain answer, so the tool round is requested explicitly here.
	h.provider.respondWithToolCalls(1)
	h.tools.panicOnInvoke() // 工具执行 panic，由 RunService 的 recover 收口

	res, err := h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err, "panic 是运行期致命错误，走 RunResult.Status=failed")
	require.Equal(t, core.StatusFailed, res.Status)
	require.Contains(t, res.Reason, "panic", "Reason 必须点名 panic，便于与超时 / 预算失败区分")
	require.Zero(t, h.concurrencyInUse(t, "c1"), "panic 路径也必须归还名额")

	h.tools.restore()
	h.provider.respondWithText("ok")
	res, err = h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "panic 后下一个 Run 必须真的执行完成")
	require.NotEmpty(t, res.FinalText)
}

// —— C-05 / C-02：配额预留的生命周期（服务级，经真实循环与真实 ProviderService）——

// TestQuota_ReserveBeforeModelCall —— 预留**必须先于**任何受控消耗（C-05）：模型调用
// 发生的那一刻，账本上已经能看到这笔预留。
//
// 逆改点：把 `AdmitCall` 挪到 `client.Chat` 之后 → 调用时读到的预留为 0。
// Mutation: reserving after the call makes the observed reservation zero.
func TestQuota_ReserveBeforeModelCall(t *testing.T) {
	h := newRunHarness(t)
	h.setLimit(t, "c1", 1000)

	var reservedAtCall int64
	observed := false
	h.provider.observe(func(core.ChatRequest) {
		reserved, err := h.reservedTokens("c1")
		require.NoError(t, err)
		reservedAtCall = reserved
		observed = true
	})

	res, err := h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.True(t, observed, "夹具必须真的观察到那次模型调用")
	require.Equal(t, int64(100), reservedAtCall, "模型调用发生时预留必须已经落账")

	used, reserved := h.usageRow(t, "c1")
	require.Equal(t, int64(10), used, "调用成功后按供应商用量结算（脚本响应 10 token）")
	require.Zero(t, reserved, "结算完成后预留归零")
}

// TestQuota_AuditFailureReleasesReservation —— 审计预写失败是"确定未发出"（宪法 V
// 的 fail-closed，契约 §7.2「无行 = 确定没执行」）：调用**不得**发出，预留必须按
// 零消耗归还（C-29）。
//
// 逆改点：预写失败仍发出调用 → `provider.calls()` 非零；按"结局未知"保持 pending →
// `reserved` 非零。
// Mutation: issuing the call anyway, or holding the reservation as unknown.
func TestQuota_AuditFailureReleasesReservation(t *testing.T) {
	h := newRunHarness(t)
	h.audit.FailNext(core.ErrAuditWrite) // 预写 pending 审计失败
	// 运行期失败沿用既有契约：Run 返回 (RunResult, nil)，失败体现在 Status，
	// 不把模型审计失败抬成函数级 error（与 US-4 起的 StatusFailed 形状一致）。
	res, err := h.service.Run(ctx, h.requestFor("c1"))
	require.NoError(t, err)
	require.Equal(t, core.StatusFailed, res.Status, "审计预写失败按运行失败终态返回")
	require.Contains(t, res.Reason, "审计")
	require.Zero(t, h.provider.calls(), "审计预写失败不得发出模型调用")

	used, reserved := h.usageRow(t, "c1")
	require.Zero(t, used)
	require.Zero(t, reserved, "确认零消耗必须 Release，不得留 pending")
}

// TestQuota_StorageFailureRejectsNewRun —— PG 故障时必须**拒绝新增受控消耗**，
// 不得放行、不得静默降级（C-02）。
//
// 逆改点：把 `Reserve` 的错误吞掉继续跑 → 准入返回成功、模型调用被发出。
// Mutation: swallowing the store error lets the run proceed without a reservation.
func TestQuota_StorageFailureRejectsNewRun(t *testing.T) {
	h := newRunHarness(t)
	h.quotas.failReserve(true)

	_, err := h.service.Run(ctx, h.requestFor("c1"))
	require.ErrorIs(t, err, errRunHarnessInjected, "配额存储故障必须拒绝新 Run")
	require.Zero(t, h.provider.calls(), "准入被拒时不得发出模型调用")
	require.Zero(t, h.concurrencyInUse(t, "c1"), "拒绝不得泄漏已占的名额")
}
