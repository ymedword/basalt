package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 本文件是提示词面的组装（颗粒度文档 §2.3 流程 C）：四段 = 段 1 系统提示词 / 段 2 记忆注入
// （运行级冻结文本，空则无痕）/ 段 3 对话历史 / 段 4 工具定义；两级裁剪与上下文准入；
// token 估算器 v1。
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
	// filter 是注入时的来源闸门；nil = 未接线（知识库未启用），行为与接线前逐字一致。
	// 压缩投影与降级后的原始历史都经这里过滤——`Build` 内唯一落点（C-26）。
	filter SourceFilter
}

// NewPromptBuilder 组装提示词构建器。
// NewPromptBuilder assembles the prompt builder.
func NewPromptBuilder(resolver ToolResolver) *PromptBuilder {
	return &PromptBuilder{resolver: resolver}
}

// WithSourceFilter 接上注入时的来源闸门并返回自身（链式，保持既有构造函数签名）。
// 知识库启用而闸门未接线时的**启动拒绝**在装配层判定——本层只负责"接了就一定用"。
// WithSourceFilter wires the injection-time source gate; the fail-closed startup check
// lives in the assembly layer.
func (b *PromptBuilder) WithSourceFilter(filter SourceFilter) *PromptBuilder {
	b.filter = filter
	return b
}

// Prompt 是一轮组装好的完整提示词（四段）。
// Prompt is the complete prompt assembled for one round.
type Prompt struct {
	// System 是段 1 系统提示词（快照冻结的段一正文）+ 段 2 记忆注入（本次运行生效的核心区
	// 文本，运行开始时读一次、运行内冻结）；核心区为空则段 2 **不产生任何字符**（不加分隔
	// 符、不留空白）——与无记忆时逐字节相同。两段合成一个字段。
	System string
	// History 是段 3 对话历史（经两级裁剪）；当前用户消息在末位——裁剪从最老处进行，
	// 它天然不可裁。
	History []Message
	// Tools 是段 4 工具定义；**空池为 nil**（请求体不带 tools 键——客户端行为逐字不变）。
	Tools []ToolSpec
	// EvidenceRefs 是本次组装**实际进入模型输入**的来源集合——由代码算出，不由模型写：
	// 采纳的近期消息引用 ∪ 实际注入摘要的 `IncludedRefs` ∪ 结构化状态引用 ∪ 本次注入的
	// 知识片段引用（最后一个集合是 US-16 V-6 的扩展：知识正文进了模型输入，转述就必须
	// 与原文同受失效规则约束——裁决见 run-state 2026-10-30）。
	// 摘要的**审计集 `SourceRefs` 不参与**：它含此后失效的引用，混进来会让回复继承
	// "曾经失效"的标签，下一轮被整条过滤。循环据此给 assistant 消息打引用。
	// EvidenceRefs is the code-computed set of sources that actually entered the model
	// input; the summary's audit set never joins it.
	EvidenceRefs []EvidenceRef
}

// —— 装配记录的段落标签与裁剪策略版本（证据字面量，随段落结构 / 裁剪策略变更换值）——
// Evidence labels for the assembly record's final segments; they change only when the
// segment structure or the trim strategy changes.
const (
	segSystem    = "system"    // 段 1 + 段 4：系统提示 + 工具定义
	segMemory    = "memory"    // 段 2：记忆冻结
	segKnowledge = "knowledge" // 段 2½：知识注入（本次运行检索所得片段）
	segState     = "state"     // 结构化状态块
	segSummary   = "summary"   // 滚动摘要
	segRecent    = "recent"    // 最近完整工具组（含当前消息）

	// trimStrategyVersion 是历史裁剪策略的版本：按轮数保留 + 按**完整工具交互组**丢弃。
	// 策略一改即换字面量，装配记录里的旧读数才有可比性。
	trimStrategyVersion = "turn-group/v1"
)

// 摘要注入的 fail-closed 判据：只接受**已通过注入前校验**（`Validity == SummaryValid`）
// 的条目。零值 `Validity` 表示未校验，与 `SummaryStale` 一样**不得**进入 prompt——把它
// 当默认可注入，等于让未经撤权复核的正文进模型输入（C-26）。
// Only a summary whose validity was verified may enter the prompt; an unverified one is
// treated exactly like a stale one.
var errSummaryNotInjectable = errors.New("上下文装配：摘要未通过注入前校验（Validity 非 valid），不得注入")

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

// Build 组装一轮提示词：段 1 取快照 → 段 2 拼生效记忆 → 段 4 取工具规格 → 段 3 两级裁剪 →
// 估算与准入，并产出本轮装配记录（`AssemblyRecord`）。
// Build assembles one round's prompt — segments first, then two-level history trimming,
// then the admission check — and returns the evidence of that assembly.
//
// `ctx` **只**供来源闸门（`SourceFilter`）的库内校验携带取消 / 截止 / 追踪标识；
// **不得**用它承载业务状态（原则 VII）。`inputs` 只加结构化状态、滚动摘要与近期消息；
// 系统提示 / 记忆 / 工具定义 / 预算仍由 `RunEnv` 冻结提供。
//
// **error 的来源**：① 不可裁部分裁剪后仍超 `budget.context_tokens`（拒绝启动、零 LLM
// 调用——返回的文本点名该配置键，排障的人要能一眼看到该调哪个旋钮）；② 本次还没有接线
// 的装配输入被显式传入（摘要 / 结构化状态）——**宁可报错，不得静默丢弃**。
// Errors come from the admission check or from an unwired-but-requested input; a
// silently dropped segment would falsify the evidence.
func (b *PromptBuilder) Build(ctx context.Context, env *RunEnv, inputs ContextInputs) (Prompt, AssemblyRecord, error) {
	if env == nil {
		return Prompt{}, AssemblyRecord{}, errors.New("prompt build: nil run env")
	}
	if inputs.Summary != nil && inputs.Summary.Validity != SummaryValid {
		return Prompt{}, AssemblyRecord{}, errSummaryNotInjectable
	}
	stateBlock := renderStateBlock(inputs.State)
	p := Prompt{
		// 段序：系统 + 工具定义 → 记忆冻结 → 结构化状态 → 滚动摘要 → 最近完整工具组。
		// 前四段合成 `System`（协议层只有一个 system 消息位），历史仍走 `History`。
		System: composeSystem(env, stateBlock, summaryBlock(inputs.Summary)),
		// 段 3 一级裁剪：按轮数保留
		History: trimByTurns(recentMessages(env, inputs), maxHistoryTurns(env)),
		Tools:   b.toolSpecs(env), // 段 4；空池 → nil
	}

	limit := contextLimit(env.Budget.ContextTokens)
	// 段 3 二级裁剪：按完整工具交互组丢弃；读数只覆盖这一级（`max_history_turns` 是保留
	// 策略，不是容量裁剪，不在这份记录的口径里）。
	before := estimateTokens(p)
	trim, err := trimByTokens(&p, limit)
	if err != nil {
		return Prompt{}, AssemblyRecord{}, err
	}

	// 来源闸门：**唯一落点**——压缩投影与降级后的原始历史都从这里过（C-26）。
	// 未接线（`filter == nil`）时逐字保持接线前的行为：不做来源校验、不改任何正文。
	history := p.History
	if b.filter != nil {
		filtered, err := b.filter.Filter(ctx, env.Req.Principal, history)
		if err != nil {
			return Prompt{}, AssemblyRecord{}, err
		}
		p.History = filtered
	}

	after := estimateTokens(p)
	if after > limit {
		// 不可裁部分（段 1 + 段 2 + 段 4 + 当前消息）本身超限：**拒绝启动**，零 LLM 调用。
		return Prompt{}, AssemblyRecord{}, fmt.Errorf(
			"上下文超限：不可裁部分估算 %d tokens 超过 budget.context_tokens=%d（含余量 %d）",
			after, env.Budget.ContextTokens, ReserveTokens)
	}
	// 知识注入面同样参与引用传播：这条回复可能转述了片段正文，撤权后转述必须与原文
	// 一起被替换（C-26）；同一版本的多条片段由 `mergeEvidenceRefs` 折叠成一条。
	// The injection payload joins the propagated refs so a revoked source takes the
	// paraphrase down with the original.
	p.EvidenceRefs = mergeEvidenceRefs(
		evidenceRefsOf(history, p.History, b.filter != nil, inputs),
		env.knowledge.Refs(),
	)
	return p, assemblyRecord(env, p, before, after, trim, inputs), nil
}

// evidenceRefsOf 算出本次模型输入**实际包含**的来源集合（C-26）：
// 采纳的近期消息 ∪ 实际注入摘要的 `IncludedRefs` ∪ 结构化状态引用。
//
// 两个细节都不能省：
//   - 被闸门**整条替换**的消息不贡献引用——它的正文已经不在输入里，再传播出去会让
//     下一轮把一条本身安全的回复按"依赖了失效来源"过滤掉；
//   - 摘要只用 `IncludedRefs`，**审计集 `SourceRefs` 不参与**（同上，反向亦然）。
//
// evidenceRefsOf unions the references that actually entered the model input; a message
// the gate replaced contributes nothing, and the summary's audit set never joins it.
func evidenceRefsOf(original, kept []Message, filtered bool, inputs ContextInputs) []EvidenceRef {
	sets := make([][]EvidenceRef, 0, 4)
	for i, m := range kept {
		if filtered && i < len(original) && original[i].ID == m.ID && original[i].Content != m.Content {
			continue
		}
		if len(m.EvidenceRefs) > 0 {
			sets = append(sets, m.EvidenceRefs)
		}
	}
	if inputs.Summary != nil {
		sets = append(sets, inputs.Summary.IncludedRefs)
	}
	if inputs.State != nil {
		sets = append(sets, inputs.State.EvidenceRefs)
	}
	return mergeEvidenceRefs(sets...)
}

// renderStateBlock 把结构化状态渲染成代码保真的文本块（段 3）。空状态返回空串——
// 不产生标题、不留空行，与"没有这一段"逐字节相同。
// renderStateBlock renders the structured state; an empty state yields no characters.
func renderStateBlock(state *StructuredState) string {
	if state == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 结构化状态（代码保真，不是模型转述）")
	wrote := false
	if state.Goal != "" {
		b.WriteString("\n目标：")
		b.WriteString(state.Goal)
		wrote = true
	}
	if len(state.Constraints) > 0 {
		b.WriteString("\n硬约束与否决条件：")
		for _, c := range state.Constraints {
			b.WriteString("\n- ")
			b.WriteString(c.Text)
			if c.MessageID != "" {
				b.WriteString("（")
				b.WriteString(c.MessageID)
				b.WriteString("）")
			}
		}
		wrote = true
	}
	if len(state.OpenItems) > 0 {
		b.WriteString("\n未解项：")
		for _, item := range state.OpenItems {
			b.WriteString("\n- ")
			b.WriteString(item.Text)
			if item.MessageID != "" {
				b.WriteString("（")
				b.WriteString(item.MessageID)
				b.WriteString("）")
			}
		}
		wrote = true
	}
	if !wrote {
		return ""
	}
	return b.String()
}

// summaryBlock 把**已通过校验**的摘要渲染成注入段（段 4）。摘要正文是派生视图，
// 注入时必须自己声明这一点，免得模型把它当消息原文复述出去。
// summaryBlock labels the summary as a derived view at injection time.
func summaryBlock(summary *CompactionSummary) string {
	if summary == nil || summary.Text == "" {
		return ""
	}
	return "## 早前对话摘要（派生视图，不是消息原文，也不是引用依据）\n" + summary.Text
}

// FixedTokens 用与 `Build` **同一个**估算器（`EstimateVersion`）现算不可裁段落的开销，
// 循环把它填进 `CompactRequest.Fixed`。零值 = 未接线，压缩引擎必须拒绝（**不得**猜）。
//
// `State` 恒为 0：结构化状态来自 `ContextInputs`，本方法的签名里没有它——接线层在拿到
// 状态块时自行累加 `State` 并同步 `Total`（同一次装配必须与 `Build` 的读数一致）。
// FixedTokens reuses the very same estimator as Build; the state part is added by the
// wiring layer, which is the only one that knows the state block.
func (b *PromptBuilder) FixedTokens(env *RunEnv) FixedParts {
	if env == nil {
		return FixedParts{}
	}
	tools := 0
	for _, spec := range b.toolSpecs(env) {
		tools += toolOverheadTokens + estimateText(spec.Name) + estimateText(spec.Description) +
			estimateText(string(spec.Parameters))
	}
	memory := 0
	if env.memory != "" {
		memory = estimateText(env.memory)
	}
	parts := FixedParts{
		System: estimateText(sectionOne(env)),
		Memory: memory,
		Tools:  tools,
	}
	// 段 1 + 段 2 的合串含分隔符；分隔符计入 Total，不摊到任何单段。
	// 知识注入（段 2½）同理只进 `Total`：`FixedParts` 的五个字段是冻结形状
	// （设计 §3.1.0），**不得**为它加第六个字段；`Total` 才是压缩判定的输入。
	// The knowledge segment lands in Total only: FixedParts keeps its frozen five fields.
	parts.Total = estimateText(fixedSystem(env)) + parts.Tools + parts.State
	return parts
}

// fixedSystem 拼段 1 + 段 2：段 1 取快照冻结的段一正文（**不重读**），段 2 取本次运行
// 冻结的核心区文本（**运行开始时读一次、运行内冻结**——本函数每轮重组装，但**不重读**
// 载体：文本来自 `RunEnv` 的注入位）；核心区为空则**不产生任何字符**。
// fixedSystem composes segments 1 and 2; an empty memory adds nothing at all.
func fixedSystem(env *RunEnv) string {
	return composeSystem(env, "", "")
}

// composeSystem 按段序拼系统侧的五段：系统提示 → 记忆冻结 → 知识注入 → 结构化状态 →
// 滚动摘要。空段**不产生任何字符**（不加分隔符、不留空白）——"没有这一段"与"段为空"在
// 协议上必须逐字节相同，否则历史运行与新增接线后的运行没法对比。
// composeSystem joins the five system-side segments in order; an absent segment adds
// nothing at all.
func composeSystem(env *RunEnv, state, summary string) string {
	system := sectionOne(env)
	for _, extra := range []string{env.memory, env.knowledge.Render(), state, summary} {
		if extra == "" {
			continue
		}
		system += "\n\n" + extra
	}
	return system
}

// sectionOne 包含冻结定义与服务端本次运行的触发来源；来源只由运行请求的渠道和
// 调度器触发类型映射，不解析用户消息，也不把提示词当成权限控制。
// sectionOne joins the frozen definition and the server-side run origin. User text
// is never used to infer the origin; this is guidance, not an authorization boundary.
func sectionOne(env *RunEnv) string {
	definition := ""
	if env.Snapshot != nil {
		definition = env.Snapshot.SectionOne
	}
	origin := promptRunOrigin(env.Req)
	if origin == "" {
		return definition
	}
	if definition == "" {
		return origin
	}
	return definition + "\n\n" + origin
}

func promptRunOrigin(req RunRequest) string {
	const prefix = "## 本次运行来源\n"
	switch req.Channel {
	case "scheduler":
		switch req.Trigger {
		case TriggerCron:
			return prefix + "定时任务到期触发。请根据本次任务消息执行。"
		case TriggerManual:
			return prefix + "用户手动运行了定时任务。请根据本次任务消息执行。"
		default:
			return prefix + "来自调度器，但触发方式未确认。请根据本次任务消息执行。"
		}
	case "web", "cli":
		return prefix + "来自用户或业务接口的即时请求。请根据本次消息的实际意图处理，不要仅因存在定时流程就自行执行它。"
	default:
		return ""
	}
}

// recentMessages 取本次组装的近期消息：`ContextInputs.Recent` 非 nil 时**就用它**
// （显式传空切片 = 本次没有近期消息），nil 才回落到会话历史——把空切片当 nil 会把
// "明确没有消息"错当成"用会话历史"。
// recentMessages honours an explicit Recent (including an empty one) and falls back to
// the session history only when it is nil.
func recentMessages(env *RunEnv, inputs ContextInputs) []Message {
	if inputs.Recent != nil {
		return inputs.Recent
	}
	return sessionMessages(env)
}

// assemblyRecord 组装本轮装配记录：预算、估算器版本、裁剪结果与本轮实际注入的段落。
// 与调用关联的字段（RunID / StepID / LLMCallID / Purpose / Agent / ModelVersion）与
// `RecordID` / `CreatedAt` 由循环与写入器补——`Build` 拿不到它们。
//
// `Compression` 只填 `Decision=none`、`Enabled=false`：本层只能证明"这一轮没有摘要可注入"，
// 并不知道压缩为什么没发生（未启用 / 未触发 / 降级）——`Enabled` 与 `ReasonCode` 归接线层，
// 本层**不得**编一个原因码。
// assemblyRecord records what this layer can prove; the correlation fields and the
// compression outcome belong to the loop and the wiring layer.
func assemblyRecord(env *RunEnv, p Prompt, before, after int, trim trimOutcome, inputs ContextInputs) AssemblyRecord {
	rec := AssemblyRecord{
		Budget: ContextBudget{
			ContextTokens:   effectiveContextTokens(env.Budget.ContextTokens),
			ProtocolReserve: ReserveTokens,
			// OutputReserve 只来自 `compaction.output_reserve_tokens`，本层没有该配置来源；
			// 0 = 未接线，**不是**默认预留（也不得用 ReserveTokens 顶替，C-31）。
			EstimatorVersion: EstimateVersion,
		},
		EstimatorVersion: EstimateVersion,
		Compression:      CompressionRecord{Decision: DecisionNone},
		HistoryTrim: HistoryTrimRecord{
			TokensBefore: before,
			TokensAfter:  after,
		},
	}
	if trim.DroppedGroups > 0 {
		rec.HistoryTrim.DroppedGroups = trim.DroppedGroups
		// 标识取自被丢弃消息自身的 `Message.ID`（会话 = messages.id，无状态 =
		// StatelessMessageID）——本层持有的是消息本身，不需要事后回填或猜测。
		rec.HistoryTrim.FirstDroppedMessageID = trim.FirstDroppedID
		rec.HistoryTrim.CutMessageID = trim.CutID
		rec.HistoryTrim.ReasonCode = HistoryTrimReasonGroupDropped
		rec.HistoryTrim.StrategyVersion = trimStrategyVersion
	}
	rec.FinalSegments = finalSegments(env, inputs, p)
	return rec
}

// finalSegments 列出本轮**实际**进入 prompt 的段落与顺序：系统 + 工具定义（合段）→ 记忆冻结
// → 知识注入 → 结构化状态 → 滚动摘要 → 最近完整工具组。未接线的段落不出现——记录只写
// 真发生的事（知识注入被轮内复核全部剔除时不出现，与"没有知识"同形）。
// finalSegments lists the segments that actually entered the prompt, in order; segments
// that are not wired — or whose chunks were all revalidated away — do not appear.
func finalSegments(env *RunEnv, inputs ContextInputs, p Prompt) []FinalSegment {
	segments := make([]FinalSegment, 0, 6)
	add := func(id string) {
		segments = append(segments, FinalSegment{ID: id, Order: len(segments) + 1})
	}
	// 段 `system` 是"系统提示 + 工具定义"的合段标签：只看这两处有没有内容，
	// 不看 `p.System`（它此时已并入记忆 / 状态 / 摘要，否则空系统提示会被误记为存在）。
	if sectionOne(env) != "" || len(p.Tools) > 0 {
		add(segSystem)
	}
	if env != nil && env.memory != "" {
		add(segMemory)
	}
	if env != nil && !env.knowledge.Empty() {
		add(segKnowledge)
	}
	if renderStateBlock(inputs.State) != "" {
		add(segState)
	}
	if summaryBlock(inputs.Summary) != "" {
		add(segSummary)
	}
	if len(p.History) > 0 {
		add(segRecent)
	}
	return segments
}

// toolSpecs 取本轮的段 4 工具定义：准入时钉住的规格优先（14D / C4——同一次运行的
// 提示词与实际调用的实例必须是同一代），未钉时才逐名经 ToolResolver.Specs 查表
// （不遍历、不推断）。`ToolSet` 为空 → nil（**空池**：请求体不带 tools 键）。
// toolSpecs prefers the admission-time pinned specs so the prompt and the instances
// actually called stay the same generation; an empty pool yields nil.
func (b *PromptBuilder) toolSpecs(env *RunEnv) []ToolSpec {
	if len(env.ToolSet) == 0 {
		return nil
	}
	if env.pinnedSpecs != nil {
		return env.pinnedSpecs
	}
	if b.resolver == nil {
		return nil
	}
	return b.resolver.Specs(env.ToolSet)
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

// effectiveContextTokens 补齐 `context_tokens` 的缺省值：未填时的缺省由服务层合并预算时
// 补，这里兜一道底，免得零值把"没配预算"误判成"预算为零"。
// effectiveContextTokens defends the zero budget so an un-merged one never reads as zero.
func effectiveContextTokens(contextTokens int) int {
	if contextTokens <= 0 {
		return defaultContextTokens
	}
	return contextTokens
}

// contextLimit 算出裁剪与准入共用的生效上限 = `context_tokens` − 余量。
// contextLimit is the effective cap shared by trimming and admission.
func contextLimit(contextTokens int) int {
	if limit := effectiveContextTokens(contextTokens) - ReserveTokens; limit > 0 {
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

// trimOutcome 是第二级裁剪的**真实结果**：被整组丢弃的完整工具交互组数，以及首条被
// 丢弃消息与切点（最后一条被丢弃消息）的标识——两者都取自消息自身的 `Message.ID`。
// trimOutcome records what level-two trimming actually dropped: the number of complete
// tool-interaction groups plus the first dropped and cut message ids.
type trimOutcome struct {
	DroppedGroups  int    // 被整组丢弃的完整工具交互组数（单条消息不计入）
	FirstDroppedID string // 首条被丢弃消息的标识
	CutID          string // 切点：最后一条被丢弃消息的标识
}

// trimByTokens 第二级裁剪：仍超预算 → 按**完整工具交互组**从最老处继续丢，直到估算进预算
// 或除不可裁部分外无物可丢（可跨轮：某轮可能只剩 user 消息）。**当前消息不可裁**——
// 受保护段从最后一条 user 消息起，丢弃永不越过它。
//
// **标识缺失即拒绝**（fail-closed）：被丢弃的消息没有 `ID` 时返回错误而不是留空的
// `FirstDroppedMessageID` / `CutMessageID`——装配记录的这两个字段要的是真实主键，
// 猜一个或写空都会让"丢了哪条"无法复核。
// Dropping a message without an ID is refused: the assembly record's first-dropped and
// cut ids must be real keys, never guessed and never left empty.
func trimByTokens(p *Prompt, limit int) (trimOutcome, error) {
	protected := protectedFrom(p.History)
	var out trimOutcome
	for protected > 0 && estimateTokens(*p) > limit {
		drop := dropUnit(p.History, protected)
		for _, m := range p.History[:drop] {
			if m.ID == "" {
				return trimOutcome{}, errors.New(
					"历史裁剪: 被丢弃的消息缺少标识——装配记录需要真实 message_id，不得猜测或留空")
			}
		}
		if out.FirstDroppedID == "" {
			out.FirstDroppedID = p.History[0].ID
		}
		out.CutID = p.History[drop-1].ID
		if drop > 1 {
			out.DroppedGroups++
		}
		p.History = p.History[drop:]
		protected -= drop
	}
	return out, nil
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
	total := estimateText(p.System) // 段 1 + 段 2（段 2 非空时一并计入）
	for _, m := range p.History {
		total += EstimateMessage(m)
	}
	for _, t := range p.Tools {
		total += toolOverheadTokens + estimateText(t.Name) + estimateText(t.Description) +
			estimateText(string(t.Parameters))
	}
	return total
}

// EstimateMessage 估算**单条消息**出站字段的 token 数：`role` + `content` + `tool_calls`
// （`id` / `name` / `arguments`）+ `tool_call_id` + `reasoning_content` + `provider_extra`
// 的 JSON 文本，外加该条的固定包裹开销。
//
// 导出是为了让 US-16 的压缩引擎与装配记录**共用同一实现**（§3.1.11 的 `Estimator`
// 要求"与装配记录同源、带版本"）：压缩包若另写一份估算，触发线与准入线会各说各话。
// 本函数的算术与 `estimateTokens` 逐项一致，两者**不得**各自演化。
// EstimateMessage estimates one message's outbound fields. It is exported so the
// compaction engine shares this very implementation instead of growing a second one.
func EstimateMessage(m Message) int {
	total := messageOverheadTokens + estimateText(m.Role) + estimateText(m.Content) +
		estimateText(m.ToolCallID) + estimateText(m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		// arguments 是**原样保存的 JSON 字符串**（不解析、不重序列化），直接按文本估。
		total += estimateText(tc.ID) + estimateText(tc.Name) + estimateText(tc.Arguments)
	}
	return total + estimateJSON(m.ProviderExtra)
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
