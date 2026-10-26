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
	RunID     string  // 调用方指定；空 = 由 AgentService 生成（web / cli 保持空）
	Agent     string  // Agent 名（Profile 解析的键）
	SessionID string  // 会话标识；无状态调用时为空
	Message   string  // 用户消息
	Channel   string  // 来源渠道：cli / web / scheduler / admin
	UserID    string  // 用户标识
	Trigger   Trigger // 空 = 由 Channel 推导；scheduler 渠道填 cron / manual
	// RequireExistingSession 为真时，会话不存在 → *SessionNotFoundError（HTTP 消息面）；
	// 为假时会话缺失由 Run 自动创建（CLI / 调度器）。两种情况都拒绝已归档会话，
	// 但仅当 SessionID != "" 且装配了 SessionStatusStore；无状态调用（空会话）不适用。
	RequireExistingSession bool
	Budget                 Budget // 执行预算（见契约 §6）
	ConfigHash             string // 由运行时填充的配置快照版本（调用方不填）
	// SuppressNotify 为真时本 Run **不向模型提供** `notify` 工具，且通知授权固定为
	// `none`（授权层兜底）——由调度器在任务配置了系统代投时设置：正文直接返回给系统，
	// 由系统投递，模型不再自行推送（R7 / 宪法约束 11）。
	// When set, the notify tool is removed from this run's ToolSet and the notify grant is
	// forced to none; the scheduler sets it whenever the task delivers its result itself.
	SuppressNotify bool
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
	// ManifestHash 是本次运行准入清单的哈希（契约 §1.1）；旧版未记录时为空。
	// ManifestHash is the admission manifest hash; empty when the run predates it.
	ManifestHash string
	Reason       string // 非正常终止的可读原因（limit_reached / failed 时必填）
}

// AgentUnavailableError 表示生命周期门禁拒绝新运行（契约 run-and-errors §9 的
// 409 `AGENT_UNAVAILABLE`）：状态文件缺失 / 损坏 / 状态非 active 一律失败关闭——
// **不得**回退为"目录存在即 active"。
//
// 文案纪律：`Reason` 是自产分类（含修复入口），**不得**拼接底层错误原文——workspace 的
// 读取错误可能带绝对路径；底层错误只经 `Unwrap` 供 `errors.Is` / `errors.As` 分类与排障。
// AgentUnavailableError is the lifecycle gate's refusal (409 AGENT_UNAVAILABLE). The
// message is self-authored (category plus repair hint); the underlying error is reachable
// only through Unwrap so absolute paths never leak into the text.
type AgentUnavailableError struct {
	Name   string     // Agent 名（原样带回，便于排障）
	State  AgentState // 已知状态（disabled / archived）；缺失或损坏时为空
	Reason string     // 自产分类与修复入口（英文；不含底层错误原文）
	Kind   AgentUnavailableKind
	Cause  error // 原因链：只经 Unwrap 暴露
}

// AgentUnavailableKind 是生命周期拒绝的**机器可判**分类：界面文案由 web 层按它映射成
// 中文，**不得**按英文 `Reason` 做字符串匹配（文案一改，匹配就悄悄失效）。内部原因
// 分类与日志继续保持英文。
// AgentUnavailableKind is the machine-judgeable category. The web layer maps it to
// Chinese copy; matching on the English Reason text is forbidden.
type AgentUnavailableKind string

// 生命周期拒绝的五类（`other` 是零值兜底：状态既不是已知非 active 状态，也没有更具体的
// 分类——界面给出保守文案而不是空白）。
// The five refusal categories; other is the conservative fallback.
const (
	AgentUnavailableMissing  AgentUnavailableKind = "missing"
	AgentUnavailableCorrupt  AgentUnavailableKind = "corrupt"
	AgentUnavailableDisabled AgentUnavailableKind = "disabled"
	AgentUnavailableArchived AgentUnavailableKind = "archived"
	AgentUnavailableOther    AgentUnavailableKind = "other"
)

// KindOrDefault 返回分类；零值按 `other` 处理。
// KindOrDefault returns other for the zero value.
func (e *AgentUnavailableError) KindOrDefault() AgentUnavailableKind {
	if e.Kind == "" {
		return AgentUnavailableOther
	}
	return e.Kind
}

// Error 返回自产的拒绝文案。
// Error renders the self-authored refusal text.
func (e *AgentUnavailableError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "lifecycle state " + string(e.State)
	}
	return "agent unavailable: " + e.Name + " (" + reason + ")"
}

// Unwrap 暴露原因链：`errors.Is(err, fs.ErrNotExist)` 因此仍可判定"文件缺失"。
// Unwrap exposes the cause so a missing state file stays recognizable via errors.Is.
func (e *AgentUnavailableError) Unwrap() error { return e.Cause }

// ResourceUnavailableError 表示准入时无法固定已发布的资源版本（契约 run-and-errors
// §9 的 500 `INTERNAL`）：Provider / MCP / 技能的版本指针缺失、内容与发布哈希不符、
// 运行时装配失败等——一律失败关闭，**不得**退回工作副本继续跑（那等于绕过发布闸门）。
//
// 文案纪律同 AgentUnavailableError：`Reason` 含修复入口，底层错误只经 `Unwrap`。
// ResourceUnavailableError reports that admission could not pin the published resource
// versions (500 INTERNAL). It fails closed rather than falling back to work copies; the
// cause is exposed only through Unwrap so internal paths never reach the client.
type ResourceUnavailableError struct {
	Name   string // Agent 名（原样带回，便于排障）
	Reason string // 自产分类与修复入口（不含底层错误原文）
	Cause  error  // 原因链：只经 Unwrap 暴露
}

// Error 返回自产的拒绝文案。
// Error renders the self-authored refusal text.
func (e *ResourceUnavailableError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "published resource version unavailable"
	}
	return "resource unavailable: " + e.Name + " (" + reason + ")"
}

// Unwrap 暴露原因链，供上层 `errors.Is` / `errors.As` 分类与排障。
// Unwrap exposes the cause for classification and troubleshooting.
func (e *ResourceUnavailableError) Unwrap() error { return e.Cause }

// RunEnv 是循环内部持有的运行环境：由 AgentService 依 RunRequest 与配置快照构造，值传递。
// 七个导出字段是契约 §1.1 的字段字**逐字**（v3 增加 `AgentUID`）；三个非导出字段是冻结
// 签名下的注入位——`client()` 与 `ToolEnv(ctx, runID)` 都不收"客户端 / Agent 目录 /
// 记忆文本"参数，而这三样恰恰只有构造方知道；它们不是契约字段，也不进任何出站形态。
// RunEnv is the run environment held by the loop. Its exported fields are the contract
// §1.1 set verbatim (v3 adds AgentUID); the unexported fields are injection points forced
// by the frozen method signatures and never appear in any outbound shape.
type RunEnv struct {
	Req RunRequest
	// AgentUID 是准入时从生命周期状态文件读取的稳定身份：资源资格判定只用它、不用名称。
	// 空串 = 本次运行未接线生命周期（手工构造的测试环境），资格判定必须据此拒绝。
	AgentUID AgentUID
	Snapshot *ConfigSnapshot // 本次运行冻结的配置快照（agent_name + hash + content；中途改盘不换配置）
	// Resources 是准入时固定的资源版本清单（与 runs 行同事务写入）；未接线的装配路径
	// 为空指针（历史运行读作"未记录"，**不补造**）。循环不按它路由——路由走快照与
	// ToolSet；它是这次运行的资源账随运行携带的只读副本（约束 46：经 RunEnv 显式传递，
	// 不经 context）。
	Resources *RunResourceManifest
	Session   *Session // 会话历史读写；无状态调用时为仅内存的临时会话
	Budget    Budget   // RunRequest.Budget 与 Profile 覆盖合并后的实际预算（契约 §6）
	ToolSet   []string // 本轮可用工具名（Profile 的 tools 解析结果，空 = 无工具）

	// llmClient 是唯一的模型调用入口：资源接线后取**准入固定的** ProviderService 视图
	// （路由表只含本次 Provider，两阶段审计仍在其内）；未接线时取装配期的全局服务。
	// llmClient is the sole model-call entry: the pinned ProviderService view when
	// resources are wired, the assembly-time global service otherwise.
	llmClient ProviderClient
	agentDir  string // Agent 目录绝对路径（ToolEnv.AgentDir）；空串 = 无 Agent 上下文
	// skillDirs 是准入固定的技能发布目录（`ResolvedResources.SkillDirs` 的稳定快照）——
	// 文件工具只在这里列出的版本目录内读；未接资源时为 nil，版本树整体不可读。
	// skillDirs freezes the admitted skill version directories for the file tools.
	skillDirs []string

	// memory 是本次运行生效的核心区文本（运行开始时读一次、运行内冻结）——**不是契约字段、
	// 不进任何出站形态**；经段 2 拼进 `Prompt.System` 的是它的内容，不是这个字段本身。
	// 空串 = 无记忆（段 2 不产生任何字符——与无记忆逐字节相同）。
	// memory is the core-region text that takes effect for this run, read once at run
	// start and frozen for the rest of it; an empty string means no memory at all.
	memory string

	// pinnedTools / pinnedSpecs 是准入时钉住的工具实例与规格（14D / C4）：注册池可能被
	// 受控重建整组换掉，本次运行手里的仍是准入那一刻的实例。nil = 未钉（零值单测），
	// 执行器与提示词构建回落到按名解析的既有路径。
	// pinnedTools/pinnedSpecs are the instances and specs frozen at admission; nil means
	// the run was not admitted through the pinning seam.
	pinnedTools map[string]Tool
	pinnedSpecs []ToolSpec

	// notifyMode 是准入时冻结的通知模式；零值 = 未接三态解析，按兼容规则读作
	// `all_eligible`（见 EffectiveNotifyMode）。
	// notifyMode is the frozen notification mode; the zero value falls back to the
	// compatibility default all_eligible.
	notifyMode NotifyMode

	// notifyChannels 是准入时冻结的**具体渠道 UID** 集合（R4.7）：`all_eligible` 枚举
	// 「共享 ∪ 本人专属」、`allowlist` 只取声明的 UID，都在准入时落成具体值。工具调用
	// 只能看到这一份副本，运行中途改 AGENT.md 不改变本次运行的可发送面。
	// notifyChannels is the concrete channel-uid set frozen at admission; tool calls see
	// this copy only, and a mid-run file edit cannot change it.
	notifyChannels []string
}

// EffectiveNotifyMode 返回通知意图的**有效值**：显式声明原样返回，未声明按兼容规则
// 推断（`notify_channels` 空 → `all_eligible`，非空 → `allowlist`）。
//
// 规则放在 core 而不是装配层，是因为它有两个读者：准入冻结（`app.ResolveNotifyMode`）
// 与管理台只读视图（`web.ProfileView.notify_mode`，契约要求"显式或兼容推断"的有效值）。
// 各写一份推断就是第二套约定，视图与准入会静默分叉。
// EffectiveNotifyMode is the one compatibility rule shared by admission and the console
// views; a second copy in either place would drift silently.
//
// 它**不**校验矛盾声明：`none` / `all_eligible` 配非空渠道属于非法，由准入侧拒绝。
// It does not validate contradictions; admission refuses those.
func EffectiveNotifyMode(mode NotifyMode, channels []string) NotifyMode {
	if mode != "" {
		return mode
	}
	if len(channels) > 0 {
		return NotifyModeAllowlist
	}
	return NotifyModeAllEligible
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
	RunID     string // 关联审计的 Run
	SessionID string // 可为空（无状态调用）
	AgentName string // 空串 = 无 Agent 上下文（如 CLI 顶层调用），**不是**异常
	AgentDir  string // Agent 目录绝对路径；空串 = 无上下文
	// AgentUID 是本次运行的 Agent 稳定身份（准入时固定）；资源范围判定用 UID、不用名称。
	AgentUID       AgentUID
	Timeout        time.Duration // 本次调用生效的超时（工具默认 / Profile 覆盖 / 剩余预算三者取最小，见契约 §6）
	MaxOutputBytes int           // 输出截断上限
	// SkillDirs 是本次 Run 准入绑定的技能发布目录（`storage_path` 字面量）——文件工具据此
	// 只读放行**当前 Run 绑定的版本**；空 = 无绑定（版本树一律拒）。
	// SkillDirs carries the run-bound skill version directories to the file tools.
	SkillDirs []string
	// RunBound 标明本次调用来自一次 Run（由循环构造 ToolEnv 时置 true）。文件工具据此拒绝
	// 技能工作副本（`.basalt/skills`）——Run 只读准入时固定的发布版本，工作副本的后续修改
	// 不属于本次运行。**不得**由 RunID 是否为空或 SkillDirs 是否为空推断（F-04 B1）。
	// RunBound marks environments built by a run: file tools must not fall back to the
	// skill work copies, even when the run binds no skill at all.
	RunBound bool
	// NotifyMode 是本次 Run 冻结的通知模式（none / all_eligible / allowlist）。
	// `none` 让 notify 工具在任何查表与外呼之前返回固定的 `notify_disabled`。
	// NotifyMode is the frozen notification mode; none short-circuits the notify tool
	// with the fixed notify_disabled failure.
	NotifyMode NotifyMode
	// NotifyChannels 是本次 Run 冻结的**具体渠道 UID** 集合（准入时枚举，R4.7）。
	// `RunBound` 为 true 时，notify 工具只放行 UID 在集合内、且当前范围仍对本 Agent
	// 有资格的渠道；空集合**不**表示「不限制」——它表示这次准入没有拿到任何渠道。
	// 工具**不得**在调用时回读 AGENT.md 或「临时枚举注册表」来扩大授权。
	// NotifyChannels is the frozen channel-uid set; for run-bound calls an empty set means
	// nothing is authorized, never "unrestricted".
	NotifyChannels []string
	// ToolInvocationID 是本次调用在 `tool_invocations` 里的行 id：执行器预写审计成功后
	// 注入，工具**不得**自行生成或改写。通知工具据此写与调用一对一的 `notify_deliveries`
	// 派生行；0 表示没有审计行（如非 Run 直调），此时不写投递记录。
	// ToolInvocationID is the audit row id injected by the executor after the fail-closed
	// pre-write; 0 means no audit row exists, so no delivery record is written.
	ToolInvocationID int64
	// Tools 是本次 Run 准入时**钉住的工具实例**（14D / C4）：执行器按名先查这里，未命中
	// 才回落到注册池。nil = 未钉（零值单测 / 非 Run 直调），行为与加钉之前逐字相同。
	// 它随 ToolEnv 显式传递——**不**进 context（宪法 VII）。
	// Tools carries the pinned instances of this run; a nil map falls back to the
	// registry exactly as before.
	Tools map[string]Tool
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
		AgentUID:       e.AgentUID,
		Timeout:        e.toolTimeout(ctx),
		MaxOutputBytes: e.maxOutputBytes(),
		SkillDirs:      e.skillDirs,
		// 循环构造的 ToolEnv 一律属于一次 Run（F-04 B1）：标记与是否绑定技能无关。
		// Every environment built here belongs to a run, with or without skill bindings.
		RunBound:       true,
		NotifyMode:     EffectiveNotifyMode(e.notifyMode, e.notifyChannels),
		NotifyChannels: e.notifyChannels,
		// 钉住的实例随运行环境交给执行器（14D / C4）：注册池后续被换掉也改不到本次运行。
		// The pinned instances travel to the executor; a later registry swap cannot
		// retarget this run.
		Tools: e.pinnedTools,
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
