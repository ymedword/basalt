package core

import (
	"context"
	"time"
)

// 本文件是上下文装配与压缩的**消费方契约**（设计 §3.1.0）：`core` 只声明它需要什么，
// 实现放在各自的领域包（`internal/compaction` / `internal/knowledge`），由 `app` 装配。
// `core` 仍只依赖标准库——不 import 任何领域包（宪法「架构约束」第 1 条、原则 IX
// 「接口定义在消费方」）。
// This file declares the consumer-side contracts of context assembly and compaction.
// core only declares what it needs; the implementations live in their own domain
// packages and are wired by app. core keeps depending on the standard library only.

// EvidenceRef 是一条**来源引用**：只记定位与复核所需的三个标识，**不含正文**。
// 知识库侧的引用就是本类型（`knowledge` 声明 `type EvidenceRef = core.EvidenceRef`），
// 故 core 只声明形状、不依赖知识包。
// EvidenceRef identifies one source without carrying its text, so the reference can
// travel through core without pulling in the knowledge package.
type EvidenceRef struct {
	// Kind 是**来源种类**：`message_evidence_refs.kind` 的持久化取值（契约冻结的
	// knowledge / memory / tool / file）。设计卡 §3.1.0 的 `EvidenceRef` 只列了后三个
	// 字段，但冻结契约的落库列要求它；按"固定字面量以 contracts 为准"补此字段
	// （裁决记录见 specs/016 的 run-state R19）。
	//   - 写库时**必须**给出：空值由写侧 fail-closed 拒绝，**不得**静默记成 knowledge；
	//   - `Valid()` 不要求它——引用本身的可用性只看 ID / VersionID，kind 只在持久化
	//     与读面注记里起作用。
	// Kind is the source class persisted in message_evidence_refs.kind. The design card
	// omits it, the frozen contract requires it; the writer rejects an empty kind rather
	// than guessing, and Valid() still checks only ID/VersionID.
	Kind        string
	ID          string // 来源标识（资料 / 片段 / 记忆条目的稳定 ID）
	VersionID   string // 版本标识（换版即换值——失效判定按它复核）
	ContentHash string // 内容哈希（同一版本的内容是否被改写；可留空）
}

// Valid 判定引用是否可用：`ID` 与 `VersionID` **必须**都非空。空 ID 无法定位、
// 空版本无法复核失效——两者缺一即"无引用"，**不得**当成通配引用放行。
// Valid reports whether the reference can be resolved and re-checked.
func (r EvidenceRef) Valid() bool { return r.ID != "" && r.VersionID != "" }

// SummaryValidity 表达"这份摘要**这一次**能不能注入"，只有两个取值。
// 它**不**承载撤权原因（原因进 `AssemblyRecord.Compression.ReasonCode` 与诊断），
// 也**不得**与审计状态或知识库的 `ValidityResult` 混用——那是两个不同的问题。
// SummaryValidity says whether a summary may be injected this time; it never carries
// the reason, and it is not an audit state.
type SummaryValidity string

const (
	// SummaryValid 表示摘要通过注入前校验，可进入 prompt。
	SummaryValid SummaryValidity = "valid"
	// SummaryStale 表示摘要不可注入（来源失效 / 链资格不通过）——**不得**进入 prompt。
	SummaryStale SummaryValidity = "stale"
)

// Valid 拒绝零值：未填的 `Validity` 不是"默认可注入"，而是**未校验**——按不可注入处理。
// Valid rejects the zero value: an unset validity means "not verified", never "injectable".
func (v SummaryValidity) Valid() bool { return v == SummaryValid || v == SummaryStale }

// ConstraintRef 是一条**用户明示的硬约束 / 否定条件**：代码保真其原话与来源消息标识，
// 不交给摘要模型改写（C-23）。`Text` 是用户原话，**不得**被模型重写。
// ConstraintRef preserves the user's verbatim constraint and the message it came from;
// the text is never rewritten by a model.
type ConstraintRef struct {
	Text      string // 用户原话（逐字保留）
	MessageID string // 来源消息标识（证据锚点）
}

// OpenItem 是一条**未解项**：用户或任务提出但尚未完成的事项。代码只记录，不推断完成状态
// （未完成 / 未知项**不得**被标成完成，C-23）。
// OpenItem is an unresolved item; code records it and never infers completion.
type OpenItem struct {
	Text      string // 事项原话
	MessageID string // 来源消息标识
}

// StructuredState 是代码保真的**结构化状态块**：目标 / 硬约束 / 未解项 / 证据引用。
// 本 US 只声明最小形状供 `PromptBuilder` 组装；C 的任务状态（`TaskSummary`）交付后并入
// 同一块，**不**提前实现。
// StructuredState is the code-preserved state block. This US only declares the minimal
// shape; the task state delivered by a later module joins the same block later.
type StructuredState struct {
	Goal         string          // 用户目标原话
	Constraints  []ConstraintRef // 硬约束与否定条件
	OpenItems    []OpenItem      // 未解项
	EvidenceRefs []EvidenceRef   // 状态块引用的来源
}

// CompactionSummary 是**core 侧的派生视图值**：把压缩条目投影成 `PromptBuilder` 能直接
// 注入的形状，避免 core 依赖 `internal/compaction`。它只是摘要，**不是**消息——
// **不得**写回消息仓储、**不得**成为第二套追加路径（C-22）。
//
// `SourceRefs` 与 `IncludedRefs` 是两个**不同**的集合，**不得**互相冒充：
//   - `SourceRefs` 是**审计集**：生成时由代码算出的完整集合，含此后失效 / 被剔除的引用，
//     **不**随撤权重写，**不**参与注入判定；
//   - `IncludedRefs` 是**当前依赖集**：正文真正含其原文的来源，注入与失效判定**只看它**。
//
// CompactionSummary is the core-side projection of a compaction entry. SourceRefs is the
// audit set (never gates injection); IncludedRefs is the current dependency set (gates it).
type CompactionSummary struct {
	CompactionID         string          // 压缩条目标识（装配记录按它回查）
	ChainEpoch           int             // 链代次（从 1 起；重建新根 = 当前最大 +1）
	TriggerRunID         string          // 触发本次压缩的 Run（**不是**覆盖范围边界）
	CoveredFromMessageID string          // 覆盖起点（含）
	CutMessageID         string          // 覆盖终点（含）
	FirstKeptMessageID   string          // 保留段首条（= 切点之后第一条完整工具组的起点）
	Text                 string          // 摘要正文（派生文本——**不得**当消息返回）
	SourceRefs           []EvidenceRef   // 审计集（含失效 / 被剔除项）
	IncludedRefs         []EvidenceRef   // 当前依赖集（注入判据）
	Validity             SummaryValidity // 注入前校验的结论
	StrategyVersion      string          // 切点 / 预算策略版本
	PromptVersion        string          // 摘要模板版本
}

// ContextInputs 是 `PromptBuilder` 相对既有输入**新增**的三样东西：结构化状态、
// 滚动摘要与近期消息。它**不携带**系统提示 / 记忆 / 工具定义 / 预算——那四样仍由
// `RunEnv` 冻结提供，段序与预算归 `PromptBuilder` 持有（C-31）。
// ContextInputs adds only the state block, the rolling summary and the recent messages;
// everything else still comes from the frozen RunEnv.
type ContextInputs struct {
	State   *StructuredState   // nil = 无结构化状态段
	Summary *CompactionSummary // nil = 无滚动摘要段
	// Recent 为 nil 时取 `env.Session.Messages()`；显式传空切片表示"本次没有近期消息"
	// （两者语义不同，**不得**把空切片当成 nil 回落到会话历史）。
	Recent []Message
}

// ContextBudget 是上下文预算的**显式**四元组（C-31）。判定链只有一条：
// `limit = ContextTokens − ProtocolReserve`、`usable_input = limit − OutputReserve −
// safety_margin`（safety margin 由压缩引擎按 `compaction.safety_margin_ratio` 现算，
// 不属本类型）。
//
// **`OutputReserve` 只能来自 `compaction.output_reserve_tokens`**：它是"给对话回复留的
// 空间"，**不得**复用 `ProtocolReserve`（协议包装余量），也**不得**取
// `summary_max_tokens`（那只是摘要请求自身的 `max_tokens`）。
// ContextBudget is the explicit four-tuple of the budget chain. OutputReserve comes from
// compaction.output_reserve_tokens only; it is neither the protocol reserve nor the
// summary cap.
type ContextBudget struct {
	ContextTokens    int    // `budget.context_tokens`
	ProtocolReserve  int    // = 常量 `ReserveTokens`（**不是**配置键）
	OutputReserve    int    // `compaction.output_reserve_tokens`（零值 = 未接线，不是默认预留）
	EstimatorVersion string // 估算器版本（= 常量 `EstimateVersion`）
}

// Limit 返回裁剪与准入共用的生效上限 `ContextTokens − ProtocolReserve`。
// **不得**在这里再减 `OutputReserve`：输出预留只影响"可用输入"的下游判定，
// 把它折进 `Limit` 会让压缩触发点与准入口径各说各话（C-31）。
// Limit is the raw cap shared by trimming and admission; the output reserve is applied
// downstream, never folded into it.
func (b ContextBudget) Limit() int { return b.ContextTokens - b.ProtocolReserve }

// FixedParts 是**不可裁段落**的 token 估算：系统提示 + 记忆冻结 + 工具定义 + 结构化状态。
// 由 `PromptBuilder.FixedTokens` 用与 `Build` **同一个**估算器现算，循环填进
// `CompactRequest.Fixed`；零值 = **未接线**，压缩引擎必须拒绝（**不得**猜固定开销）。
// FixedParts is the token estimate of the untrimmable segments. A zero value means "not
// wired" and the compactor must refuse rather than guess.
type FixedParts struct {
	System int // 段 1：快照冻结的系统提示
	Memory int // 段 2：本次运行冻结的核心区文本
	Tools  int // 段 4：工具定义（含每个工具的包裹开销）
	State  int // 结构化状态块（接线层随 `ContextInputs.State` 一并填写）
	Total  int // 上述各项之和（段 1 + 段 2 的分隔符计入 Total，不摊到任何单段）
}

// HistorySource 是**当前 Run 的只读内存历史**端口：`*core.Session` 直接满足
// （`Messages()` 返回副本）。无状态 Run 的消息**从不**落 `messages` 表，故这条端口是
// 无状态范围的**唯一**消息来源——**不得**改成从存储反查（也查不到）。
// 显式参数传递、不走 `context`（原则 VII）。
// HistorySource is the read-only in-memory history of the current run. For a stateless
// run it is the only source of messages; nothing is ever read back from storage.
type HistorySource interface {
	Messages() []Message
}

// CompactRequest 是一次压缩的完整输入。**不含**消息正文副本（引擎按 `History` 就地读取）。
// 携带 `Principal` 是为了让摘要前的来源过滤有确定身份（`!Principal.Valid()` → 拒绝）。
// CompactRequest is the complete input of one compaction; it carries the principal so the
// pre-summary source filter has a definite identity.
type CompactRequest struct {
	Principal Principal // 本次 Run 准入时冻结的身份
	RunID     string    // 触发压缩的 Run
	SessionID string    // 空 = 无状态范围（此时 `History` 是唯一消息来源）
	AttemptID string    // 本次实际尝试的标识（每次真实尝试一个）
	Budget    ContextBudget
	Fixed     FixedParts
	History   HistorySource // **必填**——缺它即 `ErrScopeInvalid`
	// EstimatorVersion 与 `Budget.EstimatorVersion` 同值，单独留存便于引擎在预算未接线时
	// 仍能如实记账（两者不一致即接线错误，引擎拒绝）。
	EstimatorVersion string
}

// CompactResult 是一次压缩的结局：成功给条目，降级给原因码（**不得**返回半成品摘要）。
// CompactResult is one compaction outcome: either an entry or a degradation reason code.
type CompactResult struct {
	Entry      *CompactionSummary // 成功时的条目；降级时为 nil
	Degraded   bool               // 真 = 本 Run 走既有整组丢弃 / 显式拒绝
	ReasonCode string             // 降级原因码（`Degraded` 为真时**必须**非空）
}

// ContextCompactor 是 `core` 声明的**压缩消费方端口**，由 `internal/compaction` 实现、
// `app` 装配；循环在 `PromptBuilder` 组装前调用。未装配时返回"不需要压缩"，
// 行为与现状完全一致（P-12）。
// ContextCompactor is the consumer-side compaction port; core never imports the
// implementation. Without one wired the loop behaves exactly as before.
type ContextCompactor interface {
	Compact(ctx context.Context, req CompactRequest) (CompactResult, error)
}

// SourceFilter 是**注入时的来源闸门**：按每条消息的 `EvidenceRefs` 重查 ACL / 删除 /
// 版本，失效的**整条**替换为不可用标记（不改写存储、不改消息正文语义）。
// 压缩投影与降级后的原始历史走**同一处**过滤——这是"降级同样遵守失效规则"的落点。
//
// `p` **必须**是本次 Run 准入时冻结的身份（由 `Build` 显式传入），**不得**从 `ctx` /
// 共享状态 / 会话补齐；`!p.Valid()` → `ErrUnauthorized`（fail-closed，不产出 prompt）。
// SourceFilter is the injection-time gate for evidence validity. The principal must be
// the one frozen at admission; an invalid one fails closed.
type SourceFilter interface {
	Filter(ctx context.Context, p Principal, msgs []Message) ([]Message, error)
}

// —— 装配记录（`context_assembly_records` 的 Go 形状，§3.4.1）——
// Assembly record shapes mirroring the context_assembly_records columns.

// AssemblyPurpose 是装配记录的用途字面量，与 `context_assembly_records.purpose` 的
// CHECK（`chat` / `rerank` / `summary` / `retrieval`）逐字一致。词表外的取值由数据库
// 拒绝，本类型**不**另立取值。
// AssemblyPurpose mirrors the purpose CHECK of context_assembly_records verbatim.
type AssemblyPurpose string

const (
	PurposeChat    AssemblyPurpose = "chat"
	PurposeRerank  AssemblyPurpose = "rerank"
	PurposeSummary AssemblyPurpose = "summary"
	// PurposeRetrieval 是**检索装配**（知识库候选 / 命中 / 决定落记录）：`candidates` /
	// `hits` / `decisions` 只可能由检索写入，把它并进 chat / summary 会让记录语义失真，
	// 故它是独立的持久化取值（US-16 裁决 2026-10-28）。
	// PurposeRetrieval is the retrieval assembly: candidates/hits/decisions only ever come
	// from retrieval, so it is its own persisted value instead of being folded into chat.
	PurposeRetrieval AssemblyPurpose = "retrieval"
)

// CompressionDecision 是一次压缩在本轮装配里的去向，四值（§3.4.1 冻结）。
// CompressionDecision is what compaction did in this assembly, one of four frozen values.
type CompressionDecision string

const (
	// DecisionNone 表示本轮没有压缩（未触发 / 未装配）——**必须**同时给原因，不写占位零值。
	DecisionNone CompressionDecision = "none"
	// DecisionCompressed 表示摘要已成功注入。
	DecisionCompressed CompressionDecision = "compressed"
	// DecisionDegraded 表示压缩失败 / 被超越，已降级到整组丢弃或显式拒绝。
	DecisionDegraded CompressionDecision = "degraded"
	// DecisionRejected 表示不可裁部分本身超限，本 Run 被拒绝启动。
	DecisionRejected CompressionDecision = "rejected"
)

// HistoryTrimReasonGroupDropped 是本 US 唯一的裁剪原因码：按**完整工具交互组**丢弃。
// HistoryTrimReasonGroupDropped is the only trim reason code this US emits.
const HistoryTrimReasonGroupDropped = "group_dropped"

// AssemblyCandidate 是一条**候选**来源（检索面）：只记标识、分数与决定，不存正文。
// 字段与 `context_assembly_records.candidates` 的口径逐项对应（§3.4 字段口径）。
// AssemblyCandidate is one retrieval candidate: ids, scores and the ACL decision only.
type AssemblyCandidate struct {
	ChunkID        string  // 片段标识
	VersionID      string  // 版本标识
	ContentHash    string  // 内容哈希（可选）
	TokenEstimate  int     // 该片段的 token 估算
	RetrievalScore float64 // 检索分（RRF 等融合分）
	RerankScore    float64 // 重排分（未重排时为零值；是否走过重排由 `Purpose` 与 `Decisions` 表达，**不得**用零值反推）
	Rank           int     // 最终名次
	ACLDecision    string  // ACL 决定（`allow` / `deny`）
}

// CompressionRecord 是本轮压缩的**真实结果**（**不得**写占位零值）：成功记条目引用与
// 覆盖范围，降级记原因码，未发生压缩时 `Decision = none` 并给原因。
// CompressionRecord records the real compaction outcome: entry reference on success, a
// reason code on degradation, and decision=none with a reason when nothing happened.
type CompressionRecord struct {
	Enabled              bool
	Decision             CompressionDecision
	CompactionID         string
	CoveredFromMessageID string
	CutMessageID         string
	FirstKeptMessageID   string
	StrategyVersion      string
	PromptVersion        string
	TokensBefore         int
	TokensAfter          int
	ReasonCode           string
}

// HistoryTrimRecord 是本轮历史裁剪的**真实结果**：被丢弃的完整工具组数、切点与前后
// token 估算。消息标识取自消息自身的 `Message.ID`（会话范围 = `messages.id` 的十进制串；
// 无状态范围 = `StatelessMessageID`）——`Build` 在丢弃无标识消息时 **fail-closed**，
// 不猜、不留空。
// HistoryTrimRecord records the real trim outcome. The message ids come from
// Message.ID; Build fails closed rather than trimming a message whose id is unknown.
type HistoryTrimRecord struct {
	DroppedGroups         int    // 被丢弃的**完整工具交互组**数（单条消息不计入）
	FirstDroppedMessageID string // 首条被丢弃消息的标识（Build 从 Message.ID 取得）
	CutMessageID          string // 切点：最后一条被丢弃消息的标识（同上）
	ReasonCode            string // `group_dropped`；未裁剪时留空
	TokensBefore          int    // 裁剪前估算
	TokensAfter           int    // 裁剪后估算
	StrategyVersion       string // 裁剪策略版本（换策即换字面量）
}

// FinalSegment 是装配结果里的一个段落：只存**标识与顺序**，不存正文。
// FinalSegment keeps only the segment id and its order.
type FinalSegment struct {
	ID    string // 段落标识
	Order int    // 顺序（从 1 起）
}

// AssemblyRecord 是一轮上下文装配的证据：注入了什么、裁掉了什么、为什么。
// **不复制原文**——原文留在消息仓储与知识库，记录只承载标识、名次、分数、决定与原因码。
//
// 谁填哪些字段（接线约定）：
//   - `PromptBuilder.Build` 填本轮装配能自证的部分：`Budget` / `EstimatorVersion` /
//     `HistoryTrim` / `Compression` / `FinalSegments`；
//   - 循环与接线层填与调用关联的字段：`RunID` / `StepID` / `LLMCallID` / `Purpose` /
//     `Agent` / `ModelVersion`——它们只有调用方知道（`Build` 拿不到 Run 标识与轮次）；
//   - 写入器填 `RecordID` 与 `CreatedAt` 并落库。
//
// AssemblyRecord is the evidence of one assembly round. Build fills what it can prove
// about the assembly; the loop stamps the call correlation; the recorder assigns the id
// and the timestamp.
type AssemblyRecord struct {
	RecordID         string
	RunID            string
	StepID           string
	LLMCallID        int64 // 0 = 尚未分配（模型调用发生在装配之后）
	Purpose          AssemblyPurpose
	Agent            string
	PromptVersion    string
	ModelVersion     string
	Budget           ContextBudget
	EstimatorVersion string
	Candidates       []AssemblyCandidate
	Hits             []string
	Decisions        []string
	ReasonCodes      []string
	Compression      CompressionRecord
	HistoryTrim      HistoryTrimRecord
	FinalSegments    []FinalSegment
	CreatedAt        time.Time
}

// AssemblyRecorder 是装配记录的写入端口（消费方声明，实现由存储层提供）。
//
// **不返回错误**是契约的一部分：装配记录是可丢弃的证据面——写失败必须降级为 WARN，
// **不得**让一次 Run 因"证据没写进去"而失败（与审计 fail-closed 的分工相反）。写入器
// 负责补 `RecordID` / `CreatedAt` 并落库；循环只负责把调用关联填全再交出去。
//
// 与 `knowledge.AssemblyRecorder` 同形：记录结构在 `core`，两个消费方各自声明需要的
// 端口，同一个存储实现同时满足——**不是**两套约定。
// AssemblyRecorder persists assembly evidence. Having no error return is part of the
// contract: a failed write must degrade to a warning, never fail the run.
type AssemblyRecorder interface {
	Record(ctx context.Context, record AssemblyRecord)
}

// mergeEvidenceRefs 按**首次出现顺序**合并去重来源引用（同 ID + 同版本 + 同哈希视为同一条）。
// 顺序稳定是判据之一：装配记录与 assistant 消息的引用集合必须可复现，不得依赖 map 遍历。
// mergeEvidenceRefs unions the references in first-seen order; map iteration order never
// leaks into the result.
func mergeEvidenceRefs(sets ...[]EvidenceRef) []EvidenceRef {
	var out []EvidenceRef
	seen := make(map[EvidenceRef]struct{})
	for _, set := range sets {
		for _, ref := range set {
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			out = append(out, ref)
		}
	}
	return out
}
