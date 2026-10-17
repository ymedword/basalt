package core

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 本文件是运行面契约（字段字逐字取自 docs/contracts/run-and-errors.md §1.1——该节是
// RunRequest / RunResult / RunEnv / ToolEnv 字段字的**唯一**事实源）+ 运行环境的出站
// 组装与终态构造（技术方案 §2.2.1；流程 A~G 见 docs/us/us4-react-loop.md §2.3）。
// This file is the run-surface contract (field words verbatim from contract §1.1, the single
// source of truth) plus the run environment's outbound assembly and outcome constructors.

// 预算的缺省上限（契约 §6）：运维定的值，模型与工具参数只能缩短不能放大。
// Default budget caps (contract §6); callers may only shorten them.
const (
	// defaultToolTimeout 是单工具超时的缺省上限（30 秒）。
	defaultToolTimeout = 30 * time.Second
	// defaultMaxOutputBytes 是单工具输出的硬上限（256 KB，读取中限流）。
	defaultMaxOutputBytes = 256 * 1024
	// defaultContextTokens 是上下文预算的缺省上限（32768）。生效值由服务层合并预算时算好；
	// 提示词准入另有一道兜底，免得未合并的零值被读成"预算为零"。
	defaultContextTokens = 32768
)

// roleSystem 是系统提示词消息的角色字。
// **不导出**：导出的角色常量三件（RoleUser / RoleAssistant / RoleTool）覆盖会话历史里
// 出现的三种角色，而系统提示词来自快照、**不进**会话历史，没有跨包消费者。
// roleSystem is the role literal of the system prompt message. It stays unexported
// because the system prompt comes from the snapshot and never enters session history.
const roleSystem = "system"

// RunRequest 承载一次运行的全部输入（显式类型化参数，不放在 context 里——宪法 VII）。
// RunRequest carries every input of one run as an explicit typed value.
type RunRequest struct {
	Agent      string // Agent 名（Profile 解析的键）
	SessionID  string // 会话标识；无状态调用时为空
	Message    string // 用户消息
	Channel    string // 来源渠道：cli / web / scheduler / admin
	UserID     string // 用户标识
	Budget     Budget // 执行预算（见契约 §6）
	ConfigHash string // 由运行时填充的配置快照版本（调用方不填）
}

// RunStatus 是运行状态的五值字面量（契约 §2；与审计状态四值是**两套**，不得混用）。
// RunStatus is the five-value literal set of run states, distinct from the audit states.
type RunStatus string

const (
	// StatusRunning 是进入运行即置的起始态（先落库再执行）。
	StatusRunning RunStatus = "running"
	// StatusCompleted 是模型给出无工具调用的最终回答。
	StatusCompleted RunStatus = "completed"
	// StatusLimitReached 是达迭代上限或工具调用上限——**预期终止，不是错误**（契约 §1.3）。
	StatusLimitReached RunStatus = "limit_reached"
	// StatusCancelled 是被取消（ctx 取消 / 截止时间到）。
	StatusCancelled RunStatus = "cancelled"
	// StatusFailed 是运行期致命错误（上下文准入拒绝、消息写入失败、模型调用失败等）。
	StatusFailed RunStatus = "failed"
)

// RunResult 是运行的完整结果：无论成功失败都返回它，函数级 error 只表达"调用方问题"。
// **不含**未导出字段——它是跨包返回面（app / channel 消费）。
// RunResult is the complete outcome of one run; a function-level error only reports
// caller-side problems.
type RunResult struct {
	RunID      string    // 本次运行标识
	Status     RunStatus // 终止状态（见契约 §2）
	FinalText  string    // 最终回答文本；limit_reached / cancelled 时可能为空
	Iterations int       // 实际消耗的 ReAct 轮次（= 已收到模型响应的轮次数）
	ToolCalls  int       // 实际执行的工具调用次数
	ConfigHash string    // 本次运行使用的配置快照版本
	Reason     string    // 非正常终止的可读原因（limit_reached / failed 时必填）
}

// RunEnv 是循环内部持有的运行环境：由 AgentService 依 RunRequest 与配置快照构造，值传递。
// 五个导出字段是契约 §1.1 的字段字**逐字**（不增不改）；三个非导出字段是冻结签名下的
// 注入位——`client()` 与 `ToolEnv(ctx, runID)` 都不收"客户端 / Agent 目录 / 记忆文本"参数，
// 而这三样恰恰只有构造方知道；它们不是契约字段，也不进任何出站形态。
// RunEnv is the run environment held by the loop. Its exported fields are the contract
// §1.1 set verbatim; the unexported fields are injection points forced by the frozen
// method signatures and never appear in any outbound shape.
type RunEnv struct {
	Req      RunRequest
	Snapshot *ConfigSnapshot // 本次运行冻结的配置快照（agent_name + hash + content；中途改盘不换配置）
	Session  *Session        // 会话历史读写；无状态调用时为仅内存的临时会话
	Budget   Budget          // RunRequest.Budget 与 Profile 覆盖合并后的实际预算（契约 §6）
	ToolSet  []string        // 本轮可用工具名（Profile 的 tools 解析结果，空 = 无工具）

	llmClient ProviderClient // 唯一的模型调用入口（实现为 ProviderService：路由 + 两阶段审计在其内）
	agentDir  string         // Agent 目录绝对路径（ToolEnv.AgentDir）；空串 = 无 Agent 上下文

	// memory 是本次运行生效的核心区文本（运行开始时读一次、运行内冻结）——**不是契约字段、
	// 不进任何出站形态**；经段 2 拼进 `Prompt.System` 的是它的内容，不是这个字段本身。
	// 空串 = 无记忆（段 2 不产生任何字符——与无记忆逐字节相同）。
	// memory is the core-region text that takes effect for this run, read once at run
	// start and frozen for the rest of it; an empty string means no memory at all.
	memory string
}

// Budget 是一次运行的执行预算五件（契约 §6）。**值语义**：计数由循环持有、预算只读。
// 数值入口是 basalt.yaml 的 budget 段；Profile 只能收紧迭代上限。
// Budget is the five-item execution budget of one run; counts live in the loop and the
// budget itself is read-only.
type Budget struct {
	MaxIterations  int           // 迭代上限（缺省 10；Profile 可收紧）
	MaxToolCalls   int           // 工具调用次数上限（缺省 50）
	ToolTimeout    time.Duration // 单工具超时（缺省 30 秒）
	MaxOutputBytes int           // 单工具输出上限（缺省 256 KB）
	ContextTokens  int           // 上下文 token 硬上限（缺省 32768）
}

// ToolEnv 是传给每个工具的执行环境：由循环在每次调用前构造、值传递。
// 字段字逐字取自契约 §1.1——工具的 Execute **必须**显式接收它，业务参数不走包级变量与 context。
// ToolEnv is the execution environment passed to every tool, built by the loop before each
// call and passed by value.
type ToolEnv struct {
	RunID          string        // 关联审计的 Run
	SessionID      string        // 可为空（无状态调用）
	AgentName      string        // 空串 = 无 Agent 上下文（如 CLI 顶层调用），**不是**异常
	AgentDir       string        // Agent 目录绝对路径；空串 = 无上下文
	Timeout        time.Duration // 本次调用生效的超时（工具默认 / Profile 覆盖 / 剩余预算三者取最小，见契约 §6）
	MaxOutputBytes int           // 输出截断上限
}

// NewRunID 生成运行标识：`<渠道>-<毫秒时间戳>-<6 位 base36 小写>`（契约 §5）。
// 随机段由调用方传入——本函数**不取真实时间、不自取随机源**（时间显式传入；随机段归
// 命令面生成），同输入同输出可复现。
// NewRunID builds the run identifier; the random suffix is supplied by the caller so this
// function stays deterministic.
func NewRunID(channel string, now time.Time, rand6 string) string {
	return fmt.Sprintf("%s-%d-%s", channel, now.UnixMilli(), rand6)
}

// ToolCallsExhausted 判定工具调用预算是否耗尽（`n` = 已执行的调用数）。
// 只在"要执行工具**之前**"判——预算耗尽**不**禁止"不使用工具的模型响应"（契约 §6）。
// ToolCallsExhausted reports whether the tool-call budget is used up for n executed calls.
func (b Budget) ToolCallsExhausted(n int) bool { return n >= b.MaxToolCalls }

// client 返回唯一的模型调用入口（实现为 ProviderService——路由与两阶段审计都在它内部，
// 循环**不自建**第二套审计）。
// client returns the single model-call entry point; the loop never audits by itself.
func (e *RunEnv) client() ProviderClient { return e.llmClient }

// llmEnv 组装一次模型调用的完整入参：路由与审计关联组取自本运行环境（Provider 是快照
// 冻结的名字，**只查表不推断**），协议内容组取自本轮组装好的提示词。
// llmEnv assembles the complete input of one model call: routing and audit bindings from
// this run environment, protocol payload from the prompt built for this round.
func (e *RunEnv) llmEnv(runID string, round int, p Prompt) ChatRequest {
	ref := e.providerRef()
	msgs := make([]Message, 0, len(p.History)+1)
	msgs = append(msgs, Message{Role: roleSystem, Content: p.System}) // 段 1 + 段 2（记忆核心区；空记忆时与段 1 逐字节相同）
	msgs = append(msgs, p.History...)
	return ChatRequest{
		Provider:   ref.Name,
		RunID:      runID,
		SessionID:  e.Req.SessionID,
		Round:      round,
		ConfigHash: e.snapshotHash(),
		Model:      ref.Model,
		Messages:   msgs,
		Tools:      p.Tools,
	}
}

// assistantMessage 把模型响应转成待落库的助手消息：工具调用 id、思考内容与供应商扩展
// 字段**原样带上**（三条硬约束——丢了 id 会话不可恢复，丢了 reasoning_content 供应商 400）。
// assistantMessage turns a model response into the assistant message to persist, carrying
// tool-call ids, reasoning content and provider extras through untouched.
func (e *RunEnv) assistantMessage(resp ChatResponse) Message {
	return Message{
		Role:             RoleAssistant,
		Content:          resp.Content,
		ToolCalls:        resp.ToolCalls,
		ReasoningContent: resp.ReasoningContent,
		ProviderExtra:    resp.ProviderExtra,
	}
}

// ToolEnv 构造本次工具调用的执行环境（`runID` 显式传参——RunEnv 的字段字不含 RunID，
// 而审计关联必须有它；**不落**包级变量与 context）。
// ToolEnv builds the execution environment of one tool call. The run id is passed in
// explicitly because the RunEnv field set has no room for it.
func (e *RunEnv) ToolEnv(ctx context.Context, runID string) ToolEnv {
	return ToolEnv{
		RunID:          runID,
		SessionID:      e.Req.SessionID,
		AgentName:      e.agentName(),
		AgentDir:       e.agentDir,
		Timeout:        e.toolTimeout(ctx),
		MaxOutputBytes: e.maxOutputBytes(),
	}
}

// complete 组装"正常完成"终态：FinalText 是最后一轮的模型文本。
// complete assembles the completed outcome with the final answer text.
func (e *RunEnv) complete(text string, iterations, toolCalls int) RunResult {
	return e.outcome(StatusCompleted, text, "", iterations, toolCalls)
}

// limitReached 组装"达上限"终态——**预期终止、不是错误**（契约 §1.3）。
// 冻结签名收不下"已得部分文本"，故 FinalText 恒空；Reason 由循环点名（迭代上限 / 工具预算）。
// limitReached assembles the limit-reached outcome, an expected termination rather than
// an error.
func (e *RunEnv) limitReached(reason string, iterations, toolCalls int) RunResult {
	return e.outcome(StatusLimitReached, "", reason, iterations, toolCalls)
}

// cancelled 组装"已取消"终态。Reason 取 ctx 原因（流程 B ①）——截止时间到记契约 §6 的
// 时限字面量，显式取消记 ctx 的原样原因（契约 §1.1：Reason 仅 limit_reached / failed 必填）。
// cancelled assembles the cancelled outcome; the reason derives from the context cause.
func (e *RunEnv) cancelled(cause error, iterations, toolCalls int) RunResult {
	reason := ""
	if cause != nil {
		reason = cause.Error()
		if errors.Is(cause, context.DeadlineExceeded) {
			reason = reasonRunTimeout
		}
	}
	return e.outcome(StatusCancelled, "", reason, iterations, toolCalls)
}

// fail 组装"运行失败"终态：Reason 是错误摘要（流程 B ③）——点名字面量（如
// budget.context_tokens）随错误原文一并带出，调用方据此排障。
// fail assembles the failed outcome with the error summary as its reason.
func (e *RunEnv) fail(err error, iterations, toolCalls int) RunResult {
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	return e.outcome(StatusFailed, "", reason, iterations, toolCalls)
}

// reasonRunTimeout 是 Run 自身时限耗尽的 Reason 字面量（契约 §6 的 RunTimeout 行）。
const reasonRunTimeout = "超过运行时限"

// outcome 组装终态 RunResult 的公共部分。
// **不填 RunID**：RunEnv 的字段字没有它（契约 §1.1），由运行入口持本地变量、返回前填充。
// outcome fills the common part of a terminal RunResult; the run id is left to the
// entry point, which is the only holder of it.
func (e *RunEnv) outcome(status RunStatus, text, reason string, iterations, toolCalls int) RunResult {
	return RunResult{
		Status:     status,
		FinalText:  text,
		Iterations: iterations,
		ToolCalls:  toolCalls,
		ConfigHash: e.snapshotHash(),
		Reason:     reason,
	}
}

// snapshotHash 取本次运行冻结的快照哈希（与 runs / llm_calls 的 config_hash 同源）。
// 无快照 = 空串——终态组装**不得**因缺快照而 panic（正常路径由运行入口保证非 nil）。
// snapshotHash returns the frozen snapshot hash, or an empty string when there is none.
func (e *RunEnv) snapshotHash() string {
	if e.Snapshot == nil {
		return ""
	}
	return e.Snapshot.Hash
}

// providerRef 取冻结快照里的 provider 绑定（名 + 模型）——路由只查表、不按模型名猜供应商。
// providerRef returns the frozen provider binding; routing never guesses by model name.
func (e *RunEnv) providerRef() ProviderRef {
	if e.Snapshot == nil || e.Snapshot.Profile == nil {
		return ProviderRef{}
	}
	return e.Snapshot.Profile.Provider
}

// agentName 取本次运行的 Agent 名：以**快照冻结**的名字为准（工具据它做跨 Agent 授权判定）。
// agentName returns the frozen agent name used by tools for cross-agent authorization.
func (e *RunEnv) agentName() string {
	if e.Snapshot == nil {
		return ""
	}
	return e.Snapshot.AgentName
}

// toolTimeout 计算单工具生效超时 = min(缺省 30s, 剩余 Run 预算)——预算只可缩短不可放大。
// 「剩余」只能由截止时间与当刻比较得出：它不是业务时间戳（业务时间仍一律显式传入），
// 而是资源时限的换算。
// toolTimeout computes the effective per-tool timeout: the smaller of the default and the
// remaining run budget. "Remaining" is a resource deadline conversion, not business time.
func (e *RunEnv) toolTimeout(ctx context.Context) time.Duration {
	t := e.Budget.ToolTimeout
	if t <= 0 || t > defaultToolTimeout {
		t = defaultToolTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remain := time.Until(deadline); remain < t {
			t = remain
		}
	}
	return t
}

// maxOutputBytes 计算单工具输出的生效上限 = min(缺省 256 KB, 配置值)——预算只可缩短不可放大。
// maxOutputBytes computes the effective output cap: the smaller of the default and the
// configured value.
func (e *RunEnv) maxOutputBytes() int {
	n := e.Budget.MaxOutputBytes
	if n <= 0 || n > defaultMaxOutputBytes {
		n = defaultMaxOutputBytes
	}
	return n
}
