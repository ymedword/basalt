package app

// 本文件是**会话压缩的装配层**（T090 / 设计 §3.1.11、C-25）：把 `internal/compaction`
// 的引擎接到循环、提示词构建器与审计 / 配额的既有端口上。它只做三件事——
// 装闸门（审计 + 配额）、按 Run 构造压缩器（摘要只查准入时冻结的那张映射表）、
// 把来源闸门交给 `PromptBuilder`（压缩投影与降级回退走同一处过滤）。
//
// 本文件**不**实现压缩策略、不重写摘要模板、不自己拼 SQL：那些分别归
// `internal/compaction` 与 `internal/storage/postgres`。装配层的职责边界是"接对线"——
// 少接一根（审计 / 配额 / 来源校验）都必须**拒绝启动**，而不是运行期静默降级。
//
// This file wires the compaction engine into the loop, the prompt builder and the existing
// audit / quota ports. It adds no policy of its own: compaction strategies live in
// internal/compaction, persistence in internal/storage/postgres. A missing port is a
// startup refusal, never a silent runtime degradation.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// compactionAuditTimeout 是压缩摘要调用审计写入的独立短超时，与对话调用的审计写入
// 同口径（契约 §4.2「缺省 5 秒」，不新增配置键）。被取消的调用同样要落终态。
// The independent short timeout of the summary audit writes, matching the chat path.
const compactionAuditTimeout = 5 * time.Second

// compactionAuditPort 是压缩摘要调用的审计端口（消费方声明、`*postgres.LLMCallRepo`
// 实现）：预写 `purpose='compaction'` 的 pending 行与终态回填。窄端口让装配层可以在
// 不连库的用例里被完整驱动——判的是接线，不是存储。
// The narrow audit port of the summary call; the PostgreSQL repo is the production one.
type compactionAuditPort interface {
	// BeginCompactionCall 预写一行 `purpose='compaction'` 的 pending 审计；返回行标识。
	BeginCompactionCall(ctx context.Context, call core.LLMCall) (int64, error)
	// FinishCall 把终态写回同一行（三列 Token 平铺，与审计表列同构）。
	FinishCall(ctx context.Context, id int64, result core.LLMCallResult) error
}

// compactionQuotaPort 是配额闸门的窄端口（消费方声明、`*callers.Limiter` 实现）。
// 只声明压缩真正用到的三件事：预留、结算、释放。`AdmitCall` 是**每次真正发出**的
// 调用一次（C-29 / C-35），压缩不获取也不释放 Run 并发名额。
// The three quota operations the summary call actually uses.
type compactionQuotaPort interface {
	AdmitCall(ctx context.Context, p core.Principal, class callers.RequestClass, cost callers.Cost, idem string) (callers.Ticket, error)
	Commit(ctx context.Context, t callers.Ticket, usage callers.Usage) (callers.Settlement, error)
	Release(ctx context.Context, t callers.Ticket, reason callers.ReleaseReason) error
}

// compactionWiringDeps 是压缩接线的全部输入。`Validity` 可空：知识库的运行时来源校验
// 尚未单独接线时，装配层据此**拒绝**"启用了压缩却没有来源闸门"的组合（C-26）。
// Every input of the compaction wiring; a nil Validity is only legal while compaction is off.
type compactionWiringDeps struct {
	Enabled       bool
	Policy        compaction.Policy
	OutputReserve int
	Messages      compaction.MessageReader
	Store         compaction.CompactionStore
	Validity      compaction.ValidityChecker
	Audit         compactionAuditPort
	Quota         compactionQuotaPort
	Now           func() time.Time
	Logger        *slog.Logger
	// Diagnostics 是配额判定诊断（V-8，可空）：摘要调用的 `quota_decision` 与对话
	// 调用走同一份事件契约，`class=compaction` 让"摘要花了多少额度"可与对话分开聚合。
	// Diagnostics records the summary call's quota verdict; nil means none.
	Diagnostics diagnosticRecorder
}

// compactionWiring 是装配完成后的接线面：`Assemble` 按 Run 构造压缩器，
// `SourceFilter` 交给提示词构建器，`OutputReserve` 交给循环（C-31：只来自
// `compaction.output_reserve_tokens`，不得用协议余量顶替）。
// The assembled wiring: per-run assembly, the injection-time filter and the output reserve.
type compactionWiring struct {
	enabled       bool
	outputReserve int
	policy        compaction.Policy
	messages      compaction.MessageReader
	store         compaction.CompactionStore
	validity      compaction.ValidityChecker
	audit         compactionAuditPort
	quota         compactionQuotaPort
	now           func() time.Time
	logger        *slog.Logger
	diagnostics   diagnosticRecorder

	// filter 是注入时的来源闸门（`compaction.SourceGate`）：压缩投影与降级回退共用它，
	// 未接来源校验时为 nil，`Build` 因此逐字保持接线前的行为。
	filter core.SourceFilter
}

// newCompactionWiring 装配压缩接线。`Enabled` 为真时**每一个**依赖都必须到位，
// 缺一件即返回错误由调用方拒绝启动——半套接线等于"审计 / 配额 / 失效过滤三者之一
// 静默消失"，那正是 C-25 / C-26 要求失败关闭的地方。
// newCompactionWiring refuses to assemble a half-wired compaction: with the switch on,
// every port is mandatory.
func newCompactionWiring(deps compactionWiringDeps) (*compactionWiring, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	w := &compactionWiring{
		enabled:       deps.Enabled,
		outputReserve: deps.OutputReserve,
		policy:        deps.Policy,
		messages:      deps.Messages,
		store:         deps.Store,
		validity:      deps.Validity,
		audit:         deps.Audit,
		quota:         deps.Quota,
		now:           deps.Now,
		logger:        deps.Logger,
		diagnostics:   deps.Diagnostics,
	}
	if deps.Validity != nil {
		// 来源闸门无论压缩开关都接上：降级回退的原始历史同样不得漏出失效来源。
		// The injection-time gate is wired regardless of the switch: the degraded path
		// must obey the same validity rules.
		w.filter = compaction.SourceGate{Validator: deps.Validity}
	}
	if !deps.Enabled {
		return w, nil
	}

	// —— 以下为"启用了压缩"的必填清单：缺一项即启动拒绝，错误一次列全 ——
	// With compaction on, a missing port is a startup refusal; every gap is listed at once.
	missing := make([]string, 0, 5)
	if deps.Messages == nil {
		missing = append(missing, "会话消息只读端口")
	}
	if deps.Store == nil {
		missing = append(missing, "压缩条目存储")
	}
	if deps.Validity == nil {
		// 没有来源校验器时，摘要生成前的失效过滤与降级回退的注入过滤都会**静默消失**
		// ——那会让撤权来源经摘要重新进入模型输入（C-26 明令禁止）。
		missing = append(missing, "来源校验器（ValidityChecker）")
	}
	if deps.Audit == nil {
		missing = append(missing, "摘要调用审计端口")
	}
	if deps.Quota == nil {
		missing = append(missing, "摘要调用配额端口")
	}
	if deps.OutputReserve <= 0 {
		// `output_reserve_tokens` 为 0 会让压缩后准入线失真（C-31 的判定链少一项），
		// 配置层已拒绝，这里守住"装配面不能把它填成 0"。
		missing = append(missing, "compaction.output_reserve_tokens（必须为正）")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("会话压缩已启用但接线不完整，拒绝启动：缺少 %v", missing)
	}
	return w, nil
}

// SourceFilter 返回注入时的来源闸门；未接来源校验时返回 nil（`PromptBuilder` 据此
// 保持接线前的行为）。返回的是接口值，**不得**把 nil 具体类型装进接口。
// SourceFilter returns the injection-time gate, or a nil interface when unwired.
func (w *compactionWiring) SourceFilter() core.SourceFilter {
	if w == nil {
		return nil
	}
	return w.filter
}

// OutputReserve 返回本轮回复的预留（只来自 `compaction.output_reserve_tokens`）。
// OutputReserve is the reply budget, taken from compaction.output_reserve_tokens only.
func (w *compactionWiring) OutputReserve() int {
	if w == nil {
		return 0
	}
	return w.outputReserve
}

// Assemble 按本次 Run 的冻结环境构造压缩器；返回 nil = 本次 Run 不压缩。
//
// 三条纪律：
//   - 摘要客户端**只取** `env.SummaryProvider()`（准入时按目录冻结的裸协议客户端），
//     不按名字重新解析、也不退回 `ProviderClient()`（后者是对话审计视图，会写出
//     `purpose='chat'` 的行并以 round>=1 为由拒绝摘要调用）；
//   - `Policy.SummaryModel` 没有独立配置键，按 Run 取 `env.ProviderModel()`（模块卡
//     "摘要默认复用该 Run 的 Provider 映射与模型"）；
//   - 未接资源的轻路径（CLI / 评测 / 手工构造）没有摘要客户端，返回 nil——行为与
//     接线前逐字一致，**不**在此处制造一次注定失败的调用。
//
// The compactor is built per run so the summary call can only use the provider client
// pinned at admission; a run without resources simply does not compact.
func (w *compactionWiring) Assemble(env *core.RunEnv) core.ContextCompactor {
	if w == nil || !w.enabled || env == nil {
		return nil
	}
	client := env.SummaryProvider()
	model := env.ProviderModel()
	if client == nil || model == "" {
		return nil
	}
	policy := w.policy
	policy.SummaryModel = model

	gate := &summaryCallGate{
		wiring:    w,
		principal: env.Req.Principal,
		provider:  providerNameOf(env),
	}
	summarizer := compaction.NewModelSummarizer(
		map[string]core.ProviderClient{model: client}, model, gate)
	return compaction.NewCompactor(compaction.CompactorDeps{
		Messages:   w.messages,
		Store:      w.store,
		Summarizer: summarizer,
		Validator:  w.validity,
		Clock:      w.now,
		Policy:     policy,
	})
}

// providerNameOf 取快照冻结的 Provider 名（只用于审计行的归属列，不参与路由）。
// providerNameOf reads the frozen provider name for the audit row only; it never routes.
func providerNameOf(env *core.RunEnv) string {
	if env == nil || env.Snapshot == nil || env.Snapshot.Profile == nil {
		return ""
	}
	return env.Snapshot.Profile.Provider.Name
}

// summaryCallGate 实现 `compaction.SummaryCallGate`：**先留痕再发出**。
//
// 顺序与依据（与 `knowledge/ingest.go` 的摄取路径同一口径）：先配额预留，再预写审计，
// 最后才允许调用。两种失败都**不发出**调用：
//   - 预留失败 → 连审计行都不写（"无行 = 确定没执行"，不制造注定 unknown 的 pending 行）；
//   - 预写失败 → 立刻 `Release(ReleaseAuditFailed)` 归还预留，不留占额度的孤儿。
//
// 卡片 §3.1.11 把这两步并列写成"先写 pending、再 AdmitCall"；本实现取"先预留、后预写"，
// 差别只在**失败时的副作用**（预留失败不再留下一条永远不会执行的 pending 行），
// C-25 的硬约束"审计在调用之前"两序皆满足。该取舍记入 run-state 的派生裁决。
//
// The gate reserves quota, then pre-writes the pending audit row, and only then lets the
// call out; either failure suppresses the call.
type summaryCallGate struct {
	wiring    *compactionWiring
	principal core.Principal
	provider  string
}

// Begin 完成"预留 + 预写"。任何一步失败都返回错误，`ModelSummarizer` 据此**不发出**调用。
// Begin reserves quota and writes the pending row; any failure suppresses the call.
func (g *summaryCallGate) Begin(ctx context.Context, call compaction.SummaryCall) (compaction.SummaryTicket, error) {
	if !g.principal.Valid() {
		// 身份零值 = 无归属的调用：fail-closed，连预留都不做（C-28）。
		return nil, core.ErrUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ticket, err := g.wiring.quota.AdmitCall(ctx, g.principal, callers.ClassCompaction,
		callers.Cost{Tokens: summaryReserveTokens(call.RequestBytes)},
		summaryIdempotencyKey(call))
	// 配额判定留痕（V-8）：摘要不是对话轮（`round` 记 0），因此事件只带 Run 锚点与
	// 类目，不带 step_id——伪造一个轮次会让它与对话轮的事件混在一起。
	// The summary is not a chat round, so the event carries no step id.
	recordQuotaDecision(ctx, g.wiring.diagnostics, quotaDecision{
		RunID: call.RunID,
		Class: "compaction",
		Err:   err,
	})
	if err != nil {
		return nil, fmt.Errorf("摘要调用配额预留失败: %w", err)
	}

	auditCtx, cancel := compactionAuditContext(ctx)
	defer cancel()
	id, err := g.wiring.audit.BeginCompactionCall(auditCtx, core.LLMCall{
		RunID:     call.RunID,
		SessionID: call.SessionID,
		// 摘要不是对话轮：`round` 记 0（DDL 对它没有 CHECK），轮次语义由 `attempt_id`
		// 承担——每轮一次真实尝试一个标识（C-29 的键末位）。
		Round:     0,
		Provider:  g.provider,
		Model:     call.Model,
		CreatedAt: g.wiring.now(),
	})
	if err != nil {
		// 预写失败 ⇒ 调用不得发出（宪法 V）：把刚拿到的预留按"审计失败"归还。
		g.wiring.releaseQuota(ctx, ticket, callers.ReleaseAuditFailed)
		return nil, fmt.Errorf("摘要调用审计预写失败: %w", err)
	}
	return &summaryCallTicket{gate: g, callID: id, ticket: ticket}, nil
}

// summaryCallTicket 是一次已预写、已预留的摘要调用的回执。
// The ticket of one pre-written, quota-reserved summary call.
type summaryCallTicket struct {
	gate   *summaryCallGate
	callID int64
	ticket callers.Ticket
}

// Complete 回填终态并结算配额。审计写失败**不改变**调用结论（调用已经发生），只记
// ERROR 并由返回值把"审计没写进去"暴露给调用方。
//
// 配额处置三分（C-29）：成功且有用量 → `Commit`；成功且**确认**零用量 → `Release`
// (zero_usage)；失败 → 保持 pending（可能已消耗、结果未知，**不得**按零归还）。
//
// Complete writes the terminal state and settles the reservation; a failed audit write
// never rewrites the call outcome.
func (t *summaryCallTicket) Complete(ctx context.Context, res compaction.SummaryCallResult) error {
	auditCtx, cancel := compactionAuditContext(ctx)
	finishErr := t.gate.wiring.audit.FinishCall(auditCtx, t.callID, core.LLMCallResult{
		Status:           res.Status,
		PromptTokens:     res.Usage.PromptTokens,
		CompletionTokens: res.Usage.CompletionTokens,
		TotalTokens:      res.Usage.TotalTokens,
		ErrorMessage:     errorTextOf(res.Err),
		ErrorKind:        res.ErrorKind,
	})
	cancel()
	if finishErr != nil {
		t.gate.wiring.logger.Error("摘要调用审计终态回填失败——行保留 pending，待启动恢复标 unknown",
			"id", t.callID, "error", finishErr)
	}

	switch {
	case res.Status != core.LLMCallSuccess:
		// 调用已发出、结局失败且用量未知：保持 pending，由管理面核对收口（C-29）。
		t.gate.wiring.logger.Warn("摘要调用失败——配额预留保持 pending",
			"provider", t.gate.provider, "error", res.Err)
	case res.Usage.TotalTokens > 0:
		t.gate.wiring.commitQuota(ctx, t.ticket, callers.Usage{Tokens: int64(res.Usage.TotalTokens)})
	default:
		t.gate.wiring.releaseQuota(ctx, t.ticket, callers.ReleaseZeroUsage)
	}
	return finishErr
}

// releaseQuota 归还预留：失败只记 WARN（预留留在 pending，由管理面核对），
// 且使用与父上下文解耦的短超时——取消不该让归还半途而废。
// releaseQuota returns the reservation; a failure is a warning, never a panic.
func (w *compactionWiring) releaseQuota(ctx context.Context, ticket callers.Ticket, reason callers.ReleaseReason) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compactionAuditTimeout)
	defer cancel()
	if err := w.quota.Release(settleCtx, ticket, reason); err != nil {
		w.logger.Warn("摘要调用配额归还失败——预留保持 pending", "reason", string(reason), "error", err)
	}
}

// commitQuota 结算实际用量：失败只记 WARN（同上，预留留给管理面核对）。
// commitQuota settles the actual usage; a failure is a warning.
func (w *compactionWiring) commitQuota(ctx context.Context, ticket callers.Ticket, usage callers.Usage) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compactionAuditTimeout)
	defer cancel()
	if _, err := w.quota.Commit(settleCtx, ticket, usage); err != nil {
		w.logger.Warn("摘要调用配额结算失败——预留保持 pending", "error", err)
	}
}

// compactionAuditContext 是审计写入的上下文（契约 §4.2：WithoutCancel + 独立短超时）。
// compactionAuditContext decouples the audit write from the caller's cancellation.
func compactionAuditContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), compactionAuditTimeout)
}

// summaryIdempotencyKey 按 C-29 的冻结规则生成摘要调用的幂等键：
// `run:<run_id>:compact:<compaction_attempt_id>`——`run_id` 是**触发**该次压缩的 Run，
// 末位是这一次真正发出的尝试标识（重提同一条摘要不再发调用，因此不复用键）。
// summaryIdempotencyKey follows the frozen C-29 key layout for summary calls.
func summaryIdempotencyKey(call compaction.SummaryCall) string {
	return "run:" + call.RunID + ":compact:" + call.AttemptID
}

// summaryReserveTokens 把请求体积折算成预留 Token 数（至少 1：`Cost` 要求恰好一维非零）。
// 估算只是**预留**的上界锚点，结算一律以供应商返回的 usage 为准（C-29）。
// summaryReserveTokens turns the request size into a reservation of at least one token.
func summaryReserveTokens(requestBytes int) int64 {
	if requestBytes < 0 {
		requestBytes = 0
	}
	// 4 字节 ≈ 1 Token 的保守近似（英文按 4、中文按 1~2；此处取偏保守的下界再抬到 1）。
	tokens := int64((requestBytes + 3) / 4)
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// errorTextOf 取错误原文；nil 时为空串（成功行不写错误列）。
// errorTextOf returns the raw error text, empty for a successful call.
func errorTextOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// compactionPolicyOf 把配置段投影成引擎策略（**不补默认值**：配置层已校验完整）。
// 除 `keep_recent_groups` 外，任一零值都表示"没写"——引擎的 `valid()` 会拒绝，
// 而不是拿零值跑起来（D-24）。`keep_recent_groups` 的缺省 4 已在装载层落值
// （2026-11-05 用户裁决，D-24 的唯一例外），装配层只做投影、不另补一份缺省。
// compactionPolicyOf projects the config section as-is; nothing is defaulted here.
// keep_recent_groups already got its load-time default (4); this layer stays a projection.
func compactionPolicyOf(cfg cli.CompactionConfig) compaction.Policy {
	return compaction.Policy{
		TriggerRatio:      cfg.TriggerRatio,
		SafetyMarginRatio: cfg.SafetyMarginRatio,
		KeepRecentGroups:  cfg.KeepRecentGroups,
		SummaryMaxTokens:  cfg.SummaryMaxTokens,
		// `SummaryInputBytes` 没有对应的配置键（`CompactionConfig` 无该字段）：保持
		// 0 = 不限，`valid()` 也不把它算作必填。不在此处凭记忆铸一个上限。
		// SummaryInputBytes has no config key; zero means unlimited and is valid.
		SummaryInputBytes: 0,
		SummaryTimeout:    time.Duration(cfg.TimeoutSeconds) * time.Second,
		StrategyVersion:   cfg.StrategyVersion,
		PromptVersion:     cfg.PromptVersion,
	}
}
