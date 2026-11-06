package app

// 本文件是诊断事件在**运行路径**上的生产端（US-16 / T095；颗粒度文档 §2.9、
// R23 裁决 3）：`core` 只依赖标准库（宪法「架构约束」），**不得**为发诊断 import
// `internal/diagnostics`；`provider.ProviderService` 已承担两阶段审计，也不在其中插
// 旁路。装配层于是用一层装饰器把"观测"挂在运行期模型客户端的最外面。
//
// 三条边界：
//
//   - **只观测、不介入**：每次 `Chat` 原样调用内层并原样返回，响应与错误都不加工
//     （错误链保真，宪法 II 的"唯一追加路径"不受影响）；
//   - **诊断可丢弃**：写失败在 `Recorder` 里只 WARN，本层**不**把任何诊断结果带进
//     返回值——这就是"诊断写失败不阻塞 Run"的实现位置；
//   - **成败都记**（V-8）：成功事件不带原因码，失败事件带 `llm_call_failed`——该码
//     只表示"调用发出后没拿到可用响应"。被审计预写拦下、被凭据检查点拒绝、路由查表
//     不中这一类**根本没有发出**的失败不记事件：它们的痕迹由审计 fail-closed 路径与
//     对应日志回答，记成失败事件等于凭一条不存在的调用冒充供应商故障（§2.9.1）。
//     `decision` 在两态都留空：模型调用的成败不是一次策略判定（allow / deny / trim /
//     degrade 都不成立），把失败写成 `deny` 会把"供应商故障"误导成"策略拒绝"。
//   - **抽取只转发不记事件**（US-22）：`ChatMemoryExtract` 固定 `round=0`，低于诊断步号
//     下限 `>=1`；抽取的观测由 `llm_calls` 行与 WARN / ERROR 日志承担。
//
// This file is the diagnostics producer on the run path. The decorator observes and
// never intervenes; failures now carry llm_call_failed, while `decision` stays empty in
// both states because a call's outcome is not a policy decision.

import (
	"context"
	"errors"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// chatAttemptID 是对话调用的尝试标识，与配额幂等键 `run:<id>:chat:<round>:a1` 的末位
// 同一口径（C-29）：一轮恰好一次真实尝试，将来引入重发必须先让本常量随尝试递增，
// 否则诊断会把多次尝试折叠成同一 attempt。
// chatAttemptID mirrors the attempt slot of the quota idempotency key.
const chatAttemptID = "a1"

// diagnosticRecorder 是诊断写入的**消费方端口**（`*diagnostics.Recorder` 结构化满足）：
// 本包只依赖"记录一条事件"这一个动作，不依赖诊断包的实现细节。
// diagnosticRecorder is the write port; the app package depends on one action only.
type diagnosticRecorder interface {
	Record(ctx context.Context, ev diagnostics.DiagnosticEvent)
}

// diagnosticProviderClient 在运行期模型客户端外面记一条 `llm_call` 诊断事件。
// diagnosticProviderClient records one llm_call event around the run's model client.
type diagnosticProviderClient struct {
	inner    core.ProviderClient
	recorder diagnosticRecorder
}

// 编译期断言：诊断装饰器同样实现抽取面（US-22 §3.1.6）。生产运行入口外面总套着本
// 装饰器；不实现时抽取的类型断言恒失败，抽取在生产环境永不可达。
// Compile-time assertion that the diagnostics decorator also implements extraction.
var (
	_ core.ProviderClient  = (*diagnosticProviderClient)(nil)
	_ core.MemoryExtractor = (*diagnosticProviderClient)(nil)
)

// newDiagnosticProviderClient 装配装饰器；`recorder` 为 nil 时**原样返回内层客户端**
// ——诊断未启用的实例因此与接线前逐字一致（零额外分配、零额外调用）。
// newDiagnosticProviderClient returns the inner client untouched when diagnostics are
// off, so an instance without them behaves exactly as before.
func newDiagnosticProviderClient(inner core.ProviderClient, recorder diagnosticRecorder) core.ProviderClient {
	if recorder == nil || inner == nil {
		return inner
	}
	return &diagnosticProviderClient{inner: inner, recorder: recorder}
}

// Chat 转调内层并记一条 `llm_call` 事件（成败都记）：关联锚点是 `run_id`（Run 级事件
// 的主锚点），`step_id` 与 `seq` 都取轮号（同一 Run 内单调，与装配 / 配额键同一口径），
// `attempt_id` 固定 `a1`，`actor` 留空（本层不知道是调度、恢复还是调用方触发——归属由
// `runs.actor` 回答，**不得**在这里猜）。
// Chat forwards to the inner client and records one llm_call event either way: run_id
// is the anchor, round is the step and the local sequence, and the actor stays empty.
func (c *diagnosticProviderClient) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	resp, err := c.inner.Chat(ctx, req)
	// 没有 `run_id` 就不发：诊断**不得**为了凑关联伪造锚点（§2.9 逐字）。失败分支
	// 同样适用——没有锚点的失败事件进不了任何 Run 的时间线。轮号 < 1 的调用会被
	// `ProviderService` 在预写之前拒绝，同样属于"从未发出"。
	// Without a run id there is no anchor to attach the event to, and anchors are
	// never fabricated to satisfy the contract.
	if req.RunID == "" || req.Round < 1 {
		return resp, err
	}
	// 从未发出的失败不记事件（见文件头第三条边界）：`llm_call_failed` 回答的是"发出后
	// 没拿到可用响应"，而这四类拒绝都发生在发出之前，记成模型失败会让排障面把装配 /
	// 凭据 / 审计问题误读成供应商故障。
	// Never-sent failures produce no event; llm_call_failed is for calls that went out.
	if err != nil && callNeverIssued(err) {
		return resp, err
	}
	ev := diagnostics.DiagnosticEvent{
		Kind:        diagnostics.KindLLMCall,
		Correlation: diagnostics.Correlation{RunID: req.RunID},
		StepID:      strconv.Itoa(req.Round),
		AttemptID:   chatAttemptID,
		Seq:         int64(req.Round),
		Attrs:       map[string]any{},
	}
	if err != nil {
		ev.ReasonCode = diagnostics.ReasonLLMCallFailed
	}
	c.recorder.Record(ctx, ev)
	return resp, err
}

// ChatMemoryExtract 只转发、**不记** `llm_call` 诊断事件（US-22 §3.1.6）：抽取调用
// 固定 `round=0`，低于诊断步号下限 `>=1`，本 US 不新增诊断步号；抽取的观测由
// `llm_calls` 行与 WARN / ERROR 日志承担。内层不支持抽取时返回装配缺陷哨兵，
// **不得**换客户端或退回 `Chat`。
// ChatMemoryExtract forwards only and records no llm_call event: extraction uses
// round 0, below the diagnostic step floor of 1, and this US adds no step id.
func (c *diagnosticProviderClient) ChatMemoryExtract(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	extractor, ok := c.inner.(core.MemoryExtractor)
	if !ok {
		return core.ChatResponse{}, core.ErrLearningExtractorUnavailable
	}
	return extractor.ChatMemoryExtract(ctx, req)
}

// callNeverIssued 判定一次失败是否发生在**调用发出之前**。四个哨兵各自对应一条确定
// 未发出的拒绝路径：审计预写失败（fail-closed）、路由查表不中、Run 凭据已撤销、托管
// 凭据当前主密钥下不可读。前两个与 `core.callNotIssued` 同一口径（配额结算据此归还
// 预留）；后两个来自本装饰器内层的凭据检查点，同样在发出之前。
// callNeverIssued reports failures raised strictly before the request went out; the
// quota settlement uses the same first two sentinels.
func callNeverIssued(err error) bool {
	return errors.Is(err, core.ErrAuditWrite) ||
		errors.Is(err, core.ErrProviderNotConfigured) ||
		errors.Is(err, core.ErrCredentialRevoked) ||
		errors.Is(err, core.ErrCredentialUnavailable)
}
