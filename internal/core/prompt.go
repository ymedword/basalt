package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// 本文件是提示词面的组装（颗粒度文档 §2.3 流程 C）：四段 = 段 1 系统提示词 / 段 2 记忆注入
// （本 US 恒空）/ 段 3 对话历史 / 段 4 工具定义；两级裁剪与上下文准入；token 估算器 v1。
// This file builds the prompt (flow C): four segments, two-level trimming, context
// admission, and the v1 token estimator.
//
// 一条硬约束贯穿：**不可裁部分 = 段 1 + 段 2 + 段 4 + 当前消息**（约束 14）。裁剪只从最老处
// 动刀，任何一级都**不得**拆散一个完整工具交互组——拆半分出去，供应商会以 400 拒收整段历史。
// Hard rule: the system prompt, the tool definitions and the current message are never
// trimmed, and no trimming step may ever split a complete tool-interaction group.

// PromptBuilder 组装一轮提示词（流程 C）。它只读快照与解析器，**不重读** AGENT.md
// （段 1 取快照冻结的正文——读即所记；运行中途改盘不换配置）。
// PromptBuilder assembles one round's prompt from the frozen snapshot and the resolver.
type PromptBuilder struct {
	resolver ToolResolver
}

// NewPromptBuilder 组装提示词构建器。
// NewPromptBuilder assembles the prompt builder.
func NewPromptBuilder(resolver ToolResolver) *PromptBuilder {
	return &PromptBuilder{resolver: resolver}
}

// Prompt 是一轮组装好的完整提示词（四段）。
// Prompt is the complete prompt assembled for one round.
type Prompt struct {
	// System 是段 1 系统提示词（快照冻结的段一正文）+ 段 2 记忆注入（本 US 恒空——装配位
	// 保留、不产生文本；故两者合成一个字段，注入接上时无需改结构）。
	System string
	// History 是段 3 对话历史（经两级裁剪）；当前用户消息在末位——裁剪从最老处进行，
	// 它天然不可裁。
	History []Message
	// Tools 是段 4 工具定义；**空池为 nil**（请求体不带 tools 键——客户端行为逐字不变）。
	Tools []ToolSpec
}

// ToolSpec 是工具定义的**出站形态**：提示词段 4 与请求体 `tools` 键共用同一份
// {名字 / 描述 / 参数 schema}——段 4 与出站编码不得各取一份（两处会漂移）。
// ToolSpec is the outbound shape of a tool definition, shared by prompt segment 4 and
// the request body's tools key so the two can never drift apart.
type ToolSpec struct {
	Name        string          // 工具名（注册名，与调用名逐字一致）
	Description string          // 描述（段 4 与出站编码共用）
	Parameters  json.RawMessage // 工具的 InputSchema，原样透传（不解析、不重序列化）
}

// 估算器与裁剪的固定口径（§6.2 固定口径 9）。
// Fixed parameters of the estimator and the trimming levels (§6.2 clause 9).
const (
	// EstimateVersion 是 token 估算器的版本——写进日志与评测结果文件，**不落库**
	// （无对应列）；口径变了就换新版本字，估算读数才有可比性。
	// **导出**：§6.2 注 9 要求它与 `ReserveTokens` 一并写进 evals 结果文件，包外的
	// 写入方按引用取值（不另抄一份字面量——口径一改就必然有一处漏改）。
	// Exported so the eval runner can write the very same literal rather than a copy.
	EstimateVersion = "v1"
	// ReserveTokens 是上下文准入的余量，用于吸收协议包装开销（§6.2 固定口径 9）。
	ReserveTokens = 1024
	// messageOverheadTokens 是每条消息的固定包裹开销（role 键 + 分隔符）。
	messageOverheadTokens = 4
	// toolOverheadTokens 是每个工具定义的固定包裹开销（function 嵌套 + 括号）。
	toolOverheadTokens = 8
	// defaultMaxHistoryTurns 是轮数裁剪的缺省上限（装载层已填——这里只兜无 Profile 的面）。
	defaultMaxHistoryTurns = 20
)

// Build 组装一轮提示词：段 1 取快照 → 段 2 恒空 → 段 4 取工具规格 → 段 3 两级裁剪 →
// 估算与准入。
// Build assembles one round's prompt: segments first, then two-level history trimming,
// then the admission check.
//
// **error 只有一个来源**：裁剪到只剩不可裁部分仍超 `budget.context_tokens`（拒绝启动、
// 零 LLM 调用）。返回的 error 文本点名该配置键——排障的人要能一眼看到该调哪个旋钮。
// The single error source is the admission check; its text names the config key so the
// operator knows exactly which knob to turn.
func (b *PromptBuilder) Build(env *RunEnv) (Prompt, error) {
	if env == nil {
		return Prompt{}, errors.New("prompt build: nil run env")
	}
	system := ""
	if env.Snapshot != nil {
		system = env.Snapshot.SectionOne // 段 1：快照冻结的段一正文（**不重读**）
	}
	p := Prompt{
		System:  system, // 段 2（记忆注入）本 US 恒空——不产生文本，装配位保留
		History: trimByTurns(sessionMessages(env), maxHistoryTurns(env)),
		Tools:   b.toolSpecs(env.ToolSet), // 段 4；空池 → nil
	}

	limit := contextLimit(env.Budget.ContextTokens)
	trimByTokens(&p, limit)
	if n := estimateTokens(p); n > limit {
		// 不可裁部分（段 1 + 段 2 + 段 4 + 当前消息）本身超限：**拒绝启动**，零 LLM 调用。
		return Prompt{}, fmt.Errorf(
			"上下文超限：不可裁部分估算 %d tokens 超过 budget.context_tokens=%d（含余量 %d）",
			n, env.Budget.ContextTokens, ReserveTokens)
	}
	return p, nil
}

// toolSpecs 取本轮的段 4 工具定义：逐名经 ToolResolver.Specs 查表（不遍历、不推断），
// `ToolSet` 为空 → nil（**空池**：请求体不带 tools 键）。
// toolSpecs resolves segment-4 tool definitions by name; an empty pool yields nil.
func (b *PromptBuilder) toolSpecs(names []string) []ToolSpec {
	if len(names) == 0 || b.resolver == nil {
		return nil
	}
	return b.resolver.Specs(names)
}

// sessionMessages 取会话历史（只读面）。
// sessionMessages reads the session history through its read-only face.
func sessionMessages(env *RunEnv) []Message {
	if env.Session == nil {
		return nil
	}
	return env.Session.Messages()
}

// maxHistoryTurns 取轮数裁剪上限。缺省 20 由**装载层**填（US-3：`settings.max_history_turns`
// 未声明 → 20），故这里**不得**把显式 0 改写成 20——那是丢失"显式零值"的形态。
// The loader fills the documented default; this function must never rewrite an explicit
// zero into it, which would silently discard an operator's intent.
func maxHistoryTurns(env *RunEnv) int {
	if env.Snapshot != nil && env.Snapshot.Profile != nil {
		return env.Snapshot.Profile.Settings.MaxHistoryTurns
	}
	return defaultMaxHistoryTurns
}

// contextLimit 算出裁剪与准入共用的生效上限 = `context_tokens` − 余量。
// `context_tokens` 未填时的缺省值由服务层合并预算时补；这里兜一道底，免得零值把
// "没配预算"误判成"预算为零"。
// contextLimit is the effective cap shared by trimming and admission; the zero value is
// defended here so an un-merged budget never reads as "budget is zero".
func contextLimit(contextTokens int) int {
	if contextTokens <= 0 {
		contextTokens = defaultContextTokens
	}
	if limit := contextTokens - ReserveTokens; limit > 0 {
		return limit
	}
	return 0
}

// trimByTurns 第一级裁剪：保留最近 `maxTurns` **轮**（轮 = 以 user 消息为界）——**不得**
// 按消息条数计（一轮里的 assistant + tool 可能有很多条）。历史不足 `maxTurns` 轮则全留；
// `maxTurns <= 0`（装载层显式保留的零值：不留历史轮）的下限是**当前消息及其之后的尾巴**。
// Level one: keep the last maxTurns rounds, counted by user messages, never by message
// count. A non-positive cap keeps only the current message and the tail after it.
func trimByTurns(msgs []Message, maxTurns int) []Message {
	if maxTurns <= 0 {
		return tailFromLastUser(msgs)
	}
	rounds := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != RoleUser {
			continue
		}
		rounds++
		if rounds == maxTurns {
			return msgs[i:]
		}
	}
	return msgs
}

// tailFromLastUser 返回从最后一条 user 消息起的尾巴（无 user 消息则原样返回——宁可多留，
// 不可把"当前消息"裁掉）。
// tailFromLastUser returns the tail starting at the last user message.
func tailFromLastUser(msgs []Message) []Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			return msgs[i:]
		}
	}
	return msgs
}

// trimByTokens 第二级裁剪：仍超预算 → 按**完整工具交互组**从最老处继续丢，直到估算进预算
// 或除不可裁部分外无物可丢（可跨轮：某轮可能只剩 user 消息）。**当前消息不可裁**——
// 受保护段从最后一条 user 消息起，丢弃永不越过它。
// Level two: keep dropping the oldest complete units until the estimate fits or nothing
// but the untrimmable part is left. Dropping never crosses the current message.
func trimByTokens(p *Prompt, limit int) {
	protected := protectedFrom(p.History)
	for protected > 0 && estimateTokens(*p) > limit {
		drop := dropUnit(p.History, protected)
		p.History = p.History[drop:]
		protected -= drop
	}
}

// protectedFrom 返回受保护段的起点（最后一条 user 消息的下标；无 user 消息时保护末条）。
// protectedFrom returns the index where the untrimmable tail begins.
func protectedFrom(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			return i
		}
	}
	if len(msgs) > 0 {
		return len(msgs) - 1
	}
	return 0
}

// dropUnit 返回从最老处可整组丢弃的长度：首条是"带 tool_calls 的 assistant" → 连同其后
// **紧邻**的 tool 段整组丢（**不得拆散**——半组历史会被供应商拒收）；否则单条丢。
// 组尾判定不会越过受保护段。
// dropUnit returns the size of the oldest droppable unit: a whole tool-interaction group,
// or a single message.
func dropUnit(msgs []Message, protected int) int {
	n := 1
	if msgs[0].Role == RoleAssistant && len(msgs[0].ToolCalls) > 0 {
		for n < len(msgs) && n < protected && msgs[n].Role == RoleTool {
			n++
		}
	}
	return n
}

// estimateTokens 估算一次出站请求的 token 数（版本 `EstimateVersion`）。
// 覆盖面 = **完整出站字段**（§2.4 注 9）：段 1 / 段 2 正文、历史每条的 `role` + `content` +
// `tool_calls`（`name` + `arguments`）+ `tool_call_id` + `reasoning_content` + `provider_extra`
// 的 JSON 文本、段 4 每个工具的 `name` + `description` + `parameters`，外加每条消息 / 每个
// 工具的固定包裹开销。
// estimateTokens covers every outbound field plus fixed per-message and per-tool overhead.
func estimateTokens(p Prompt) int {
	total := estimateText(p.System) // 段 1 + 段 2（恒空）
	for _, m := range p.History {
		total += messageOverheadTokens + estimateText(m.Role) + estimateText(m.Content) +
			estimateText(m.ToolCallID) + estimateText(m.ReasoningContent)
		for _, tc := range m.ToolCalls {
			// arguments 是**原样保存的 JSON 字符串**（不解析、不重序列化），直接按文本估。
			total += estimateText(tc.ID) + estimateText(tc.Name) + estimateText(tc.Arguments)
		}
		total += estimateJSON(m.ProviderExtra)
	}
	for _, t := range p.Tools {
		total += toolOverheadTokens + estimateText(t.Name) + estimateText(t.Description) +
			estimateText(string(t.Parameters))
	}
	return total
}

// estimateJSON 估算扩展字段映射的 JSON 文本量；空映射记 0。
// 序列化失败（只可能来自循环引用）记 0：估算器**不得**因为一个估不动的小字段把裁剪卡死。
// json.Marshal 对 map 键排序，故同输入同读数（确定性是夹具的判据之一）。
// estimateJSON estimates the JSON text of the provider-extras map; a failure estimates to
// zero rather than blocking trimming.
func estimateJSON(v map[string]any) int {
	if len(v) == 0 {
		return 0
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return estimateText(string(raw))
}

// estimateText 按"CJK 类字符 1 token/字 + 其余 4 字节/token（向上取整）"估算一段文本。
// 该口径**不宣称**是保守上界（§2.4 注 9：原建议"字节数 / 2 即上界"已被否决）；它只承诺
// 版本固定、覆盖完整出站字段、同输入同读数——超发风险由余量与按组裁剪共同吸收。
// estimateText is the v1 ratio: one token per CJK rune, four bytes per token otherwise.
func estimateText(s string) int {
	if s == "" {
		return 0
	}
	wide, narrow := 0, 0
	for _, r := range s {
		if isWideRune(r) {
			wide++
			continue
		}
		narrow += utf8.RuneLen(r)
	}
	return wide + (narrow+3)/4
}

// isWideRune 判定 CJK 类字符（中文 / 日文假名 / 韩文）：这类字符的信息密度约为每字一 token，
// 按字节折算会显著低估。
// isWideRune reports CJK-class runes, whose information density is about one token each.
func isWideRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}
