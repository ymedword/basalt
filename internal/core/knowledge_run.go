package core

// 本文件是知识库**接入 Run** 的端口与注入载体（US-16 V-6，2026-10-30 用户裁决）。
//
// 三条边界：
//
//  1. **core 不依赖知识包**（包纪律：core 只依赖标准库）：检索 / 精确读取 / 引用解析都在
//     `internal/knowledge`，本包只声明形状，实现由装配层适配（`app.knowledgeRunSource`）；
//  2. **注入面是运行级冻结文本**（与记忆同构）：检索在 Run 准入之后、第一轮组装之前发生，
//     结果写成 `RunEnv.knowledge`；每轮组装**不重读**载体。与记忆不同的是**权限不冻结**：
//     每轮组装前复核一次片段的当前可见性，撤权 / 删除 / 换版立即从下一轮提示词里消失
//     （C-26 "每次精确读取必须重查当前 ACL"的注入面实现）；
//  3. **空注入逐字节无痕**：没有片段时不产生标题、分隔符或占位正文——与未接线知识库的
//     运行逐字节相同（spec US1 场景 4："注入面保持为空且不产生占位正文"）。
//
// This file declares the run-side knowledge port and the injection payload. The core
// package must not import the knowledge domain, so only shapes live here; the assembly
// layer adapts the real retriever, exact read and citation resolver.

import (
	"context"
	"strings"
)

// EvidenceKindKnowledge 是 `message_evidence_refs.kind` 的知识类持久化取值
// （契约 schema 的 `knowledge / memory / tool / file` 四值之一）。
// 引用写入侧对空 kind 失败关闭，**不得**静默记成别的种类。
// EvidenceKindKnowledge is the persisted kind literal for knowledge references.
const EvidenceKindKnowledge = "knowledge"

// 知识库管理面的三个动作与三个目标类型（run-and-errors §9.3 / openapi
// OperationView.target_kind）。字面量只此一处：写入端引用常量、读侧过滤器用同一组
// 值，两边各写一遍字符串就会漂成"写得进、筛不出"。
// The three knowledge management actions and their target kinds; the literals live
// here only, so the writer and the read-side filter cannot drift apart.
const (
	// ActionKnowledgeUpload 是**上传新源文件**并摄取（target_id = 源文件相对路径）。
	ActionKnowledgeUpload = "knowledge_document.upload"
	// ActionKnowledgeReindex 是**重建**某文档的索引（新版本行；target_id = document_id）。
	ActionKnowledgeReindex = "knowledge_document.reindex"
	// ActionKnowledgeDescription 是改集合简介（target_id = collection_id）。
	ActionKnowledgeDescription = "knowledge_collection.description_update"

	// TargetKindKnowledgeUpload 是上传动作的目标类型：落点是**源文件**，与既有文档分开，
	// 因为首次上传时还没有 document_id 可引用。
	TargetKindKnowledgeUpload = "knowledge_upload"
	// TargetKindKnowledgeDocument 是重建动作的目标类型（target_id = document_id）。
	TargetKindKnowledgeDocument = "knowledge_document"
	// TargetKindKnowledgeCollection 是集合简介动作的目标类型（target_id = collection_id）。
	TargetKindKnowledgeCollection = "knowledge_collection"
)

// KnowledgeChunk 是本次运行**实际进入上下文**的一个知识片段。
//
// 字段纪律：`Text` 只进模型上下文——**不得**写进日志、诊断事件或装配记录（C-08 同理：
// 来源路径只允许出现在提示词与本结构里，不进运行日志）。
// KnowledgeChunk is one chunk that actually entered the run context; its text never
// reaches logs, diagnostics or assembly records.
type KnowledgeChunk struct {
	// Marker 是注入与引用共用的标记（`c1`、`c2`……），也是引用解析时的 `ChunkID`。
	// Marker is the citation marker shared by the prompt and the resolver.
	Marker string
	// SourcePath 是工作区允许根内的相对路径（渲染进上下文，便于模型与读者定位）。
	SourcePath string
	// HeadingPath 是标题路径（"A > B > C"），可空。
	HeadingPath string
	// Text 是片段正文——只进模型上下文。
	Text string
	// Ref 是该片段的来源引用（文档 + 版本 + 内容哈希），由**代码**算出，供
	// `SourceFilter` 失效复核与消息来源落库使用。
	Ref EvidenceRef
}

// Locate 返回片段的定位标签（路径 › 标题）；两段都空时返回空串。
// Locate renders the chunk's location label; both parts empty yields an empty string.
func (c KnowledgeChunk) Locate() string {
	switch {
	case c.SourcePath != "" && c.HeadingPath != "":
		return c.SourcePath + " › " + c.HeadingPath
	case c.SourcePath != "":
		return c.SourcePath
	default:
		return c.HeadingPath
	}
}

// KnowledgeInjection 是本次运行注入面：按名次排好的片段。
// 零值 / nil 与"没有知识"逐字节相同（`Render` 返回空串）。
// KnowledgeInjection is the run's injection payload; nil renders no characters at all.
type KnowledgeInjection struct {
	// Chunks 是按检索名次排好的片段（标记随之递增）。
	Chunks []KnowledgeChunk
}

// Empty 报告本次运行是否没有可注入的片段。
// Empty reports whether there is nothing to inject.
func (k *KnowledgeInjection) Empty() bool { return k == nil || len(k.Chunks) == 0 }

// Render 把注入面渲染成系统侧的一段文本：标题 + 引用规则 + 逐条片段。
// 没有片段时返回空串——**不产生**任何字符（与未接线知识库的运行逐字节相同）。
//
// 引用规则的写法与 `knowledge.ResolveCitations` 的词法约定对齐：标记以独立标识符或
// 显式包裹（`[c1]` / `（来源 c1）`）出现即视为引用，未提供的标识进 `Unresolved`。
// Render returns an empty string when there is nothing to inject; absent knowledge adds
// no characters at all.
func (k *KnowledgeInjection) Render() string {
	if k.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 知识库检索结果（本次运行实际进入上下文的片段）")
	b.WriteString("\n以下片段由知识库检索得到，是本轮的资料依据；引用某条结论时在句末标注对应标记" +
		"（例如（来源 c1））。没有标注标记的结论不得声称来自资料。")
	for _, chunk := range k.Chunks {
		b.WriteString("\n\n[")
		b.WriteString(chunk.Marker)
		b.WriteString("]")
		if locate := chunk.Locate(); locate != "" {
			b.WriteString(" ")
			b.WriteString(locate)
		}
		b.WriteString("\n")
		b.WriteString(chunk.Text)
	}
	return b.String()
}

// Refs 返回注入面覆盖的来源引用（同一版本的多条片段只算一条）。
// 顺序即片段顺序，便于装配记录与消息来源落库对账。
// Refs returns the source references covered by the injection, deduplicated per version.
func (k *KnowledgeInjection) Refs() []EvidenceRef {
	if k.Empty() {
		return nil
	}
	refs := make([]EvidenceRef, 0, len(k.Chunks))
	for _, chunk := range k.Chunks {
		refs = append(refs, chunk.Ref)
	}
	return mergeEvidenceRefs(refs)
}

// KnowledgeRunRequest 是一次 Run 级检索请求（注入面装配时交给装配层实现）。
// KnowledgeRunRequest is one prompt-bound retrieval issued by the run entry.
type KnowledgeRunRequest struct {
	// Principal 是本 Run 准入时冻结的调用方身份（**显式传参**，不经 context）。
	Principal Principal
	// RunID 是装配记录与诊断事件的关联主锚点（必填）。
	RunID string
	// Agent 是 Agent 名：检索范围按它声明的 `knowledge:` 集合解析（D-19）。
	Agent string
	// Query 是检索查询（本 Run 的用户消息）。
	Query string
	// Budget 是切片的 token 预算上限（`KnowledgeBudget` 的派生值）。
	Budget int
}

// KnowledgeRevalidateRequest 是"注入面现在还有哪些片段仍可按原样使用"的复核请求。
// KnowledgeRevalidateRequest asks which injected chunks are still usable as-is.
type KnowledgeRevalidateRequest struct {
	// Principal 是本次 Run 冻结的调用方身份。
	Principal Principal
	// RunID 是关联标识（诊断事件的主锚点）。
	RunID string
	// Chunks 是当前注入面里的片段。
	Chunks []KnowledgeChunk
}

// KnowledgeAnswerRequest 是答案产出后的引用复核请求。
// KnowledgeAnswerRequest is the citation review issued once the answer exists.
type KnowledgeAnswerRequest struct {
	// Principal / RunID 是关联标识（RunID 必填）。
	Principal Principal
	RunID     string
	// Answer 是本次运行的最终回答正文。
	Answer string
	// Chunks 是**实际进入本次运行上下文**的片段（不得换成重新检索的结果）。
	Chunks []KnowledgeChunk
}

// KnowledgeAnswerReport 是引用复核的计数摘要：**只给计数，不含正文**。
// `Supported` 表示"可定位且通过确定性核对"，不等于原文支持模型的一切表述（C-33）。
// KnowledgeAnswerReport counts the four citation classes; locating never implies
// entailment beyond the deterministic checks.
type KnowledgeAnswerReport struct {
	// Supported 是可定位且通过确定性核对的主张数。
	Supported int
	// Unsupported 是与来源矛盾、或引用本身无法定位的主张数。
	Unsupported int
	// Unverified 是可定位但无法确定性判定蕴含的主张数。
	Unverified int
	// Unresolved 是引用了本次上下文没有读过的片段标识数。
	Unresolved int
}

// KnowledgeRunSource 是 Run 侧知识链路的装配层端口（实现放 `internal/app`）。
//
// 三个方法对应运行链路的三个时刻：装配（检索 + 精确读取校验 + 标记）、轮内复核、
// 终态复核（引用落位）。实现**必须**让"无权限 / 无声明集合"表现为空注入 + nil error
// ——那是合法状态，不是基础设施失败；存储违约才返回 error。
// KnowledgeRunSource is the run-side knowledge port. An empty injection with a nil error
// is the honest result of "no permission / no declared collections"; only infrastructure
// failures return an error.
type KnowledgeRunSource interface {
	// RetrieveForRun 解析范围、检索并把命中精确读取校验后落成注入面。
	// 没有可注入片段时返回 (nil, nil)。
	RetrieveForRun(ctx context.Context, req KnowledgeRunRequest) (*KnowledgeInjection, error)
	// RevalidateChunks 按当前 ACL / 删除 / 版本状态复核注入面，返回仍可用的片段。
	// 撤权 / 删除 / 换版属**结论**（剔除该片段、不返回 error）。
	RevalidateChunks(ctx context.Context, req KnowledgeRevalidateRequest) (*KnowledgeInjection, error)
	// ResolveAnswer 把答案里的引用标记与本次上下文的片段逐条比对。
	ResolveAnswer(ctx context.Context, req KnowledgeAnswerRequest) (KnowledgeAnswerReport, error)
}

// knowledgeBudgetDivisor 把上下文上限折成知识注入的切片预算（1/4）。
//
// 派生决定（run-state 2026-10-30）：§3.3 没有"知识注入预算"这项配置，本 US **不新增**
// 配置键；预算按 `contextLimit(context_tokens) / 4` 派生——足以放下典型 8 条片段，
// 又不至于压过最近历史。**不得**把它读成"知识最多占四分之一"以外的承诺。
// The derived slice budget: a quarter of the context limit. No new config key exists for
// it, and the quotient is not a promise about any other share.
const knowledgeBudgetDivisor = 4

// KnowledgeBudget 返回本次运行的知识切片预算（token）；上下文上限为零时返回 0
// （领域按"不裁剪"处理——上限缺失本身就是装载层的错误，不在这里再编一个数）。
// KnowledgeBudget derives the slice budget from the run's context limit.
func KnowledgeBudget(contextTokens int) int {
	return contextLimit(contextTokens) / knowledgeBudgetDivisor
}
