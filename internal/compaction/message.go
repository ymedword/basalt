package compaction

import (
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是压缩引擎内部的**消息视图、估算器与投影拼装**（设计 §3.1.11 / T135）。
// 消息视图不新增存储：会话范围的行来自 `message_evidence_refs` 与 `messages`，
// 无状态范围来自 `core.Message` 的内存副本。
// The message view, the versioned estimator and the projection assembly. Nothing here
// adds storage: session rows come from the database, stateless ones from memory.

// StoredMessage 是引擎内部的一条消息视图（**不新增存储**）。`ToolGroupID` 由
// `assistant(tool_calls)` 与其工具结果绑定生成，切点判定只看它——拆散工具组会被供应商
// 拒收，故切点必须落在组边界上。
// StoredMessage is the engine's message view. ToolGroupID binds an assistant tool-call
// message to its tool results so cut points can only land on group boundaries.
type StoredMessage struct {
	MessageID        string
	Seq              int64
	RunID            string
	Role             string
	Content          string
	ToolCallID       string
	ToolCalls        []core.ToolCall
	ReasoningContent string
	// Truncated 标记该工具结果在**读入时**已被截断（带 `[truncated N chars]` 标记）；
	// 摘要输入据此决定是否再截，避免二次截断把标识切掉。
	// Truncated marks a tool result already truncated on read.
	Truncated bool
	// ToolGroupID 是工具交互组标识：同一组的 assistant(tool_calls) 与全部 tool 结果共用一个值；
	// 非工具消息按自身 MessageID 单独成组（组内只有一条）。
	// ToolGroupID is shared by an assistant tool-call message and every tool result that
	// answers it; any other message forms a single-message group of its own.
	ToolGroupID string
	// SourceRefs 是该消息携带的来源引用（会话范围来自持久化，无状态范围来自
	// `core.Message.EvidenceRefs`）。
	// SourceRefs are the references the message carries.
	SourceRefs []core.EvidenceRef
}

// AsMessage 把消息视图还原成 `core.Message`（估算、投影与过滤共用同一形状）。
// AsMessage maps the stored view back to core.Message so estimation, projection and
// filtering all see one shape.
func (m StoredMessage) AsMessage() core.Message {
	return core.Message{
		ID:               m.MessageID,
		Role:             m.Role,
		Content:          m.Content,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
		ReasoningContent: m.ReasoningContent,
		EvidenceRefs:     m.SourceRefs,
	}
}

// StatelessMessageID 返回无状态 Run 内一条消息的临时标识（`msg/<runID>/<pos>`，pos 从 1 起）。
// 公式**唯一定义**在 `core.StatelessMessageID`：`Session.AppendMessage` 是唯一追加路径，
// 且 core **不得** import 本包；本函数只是一行委托，不得复制第二份公式。
// StatelessMessageID delegates to core so the formula exists exactly once.
func StatelessMessageID(runID string, pos int) string { return core.StatelessMessageID(runID, pos) }

// AssignToolGroups 给一组消息补上 `ToolGroupID`：`assistant` 带 `tool_calls` 时与紧随其后、
// 逐个匹配 `tool_call_id` 的 `tool` 结果成组；其余消息按 `MessageID` 单独成组。
// 匹配失败（缺结果 / 结果不匹配）时该条自成一组——**不猜**归属，切点会因此退回到更保守的位置。
// AssignToolGroups binds an assistant tool-call message to the tool results that answer it;
// an unmatched message forms its own group instead of guessing an owner.
func AssignToolGroups(msgs []StoredMessage) []StoredMessage {
	out := make([]StoredMessage, len(msgs))
	copy(out, msgs)
	for i := 0; i < len(out); i++ {
		if out[i].ToolGroupID != "" {
			continue
		}
		if out[i].Role != core.RoleAssistant || len(out[i].ToolCalls) == 0 {
			out[i].ToolGroupID = out[i].MessageID
			continue
		}
		want := make(map[string]struct{}, len(out[i].ToolCalls))
		for _, tc := range out[i].ToolCalls {
			want[tc.ID] = struct{}{}
		}
		group := out[i].MessageID
		out[i].ToolGroupID = group
		for j := i + 1; j < len(out) && out[j].Role == core.RoleTool; j++ {
			if _, ok := want[out[j].ToolCallID]; !ok {
				break
			}
			out[j].ToolGroupID = group
			delete(want, out[j].ToolCallID)
		}
	}
	return out
}

// Estimator 是**带版本**的预算估算器：摘要请求与投影估算共用同一实现，版本随装配记录与
// 条目留痕（C-31）。实现必须与 `core.PromptBuilder` 的估算**同源**，否则触发线与准入线
// 会各说各话。
// Estimator is the versioned token estimator shared by summary input and projection
// estimates; it must be the very same estimator the prompt builder uses.
type Estimator interface {
	Estimate(messages []StoredMessage) int
	Version() string
}

// MessageEstimator 是 v1 估算器：逐条委托 `core.EstimateMessage`，与装配记录同源。
// 版本字取 `core.EstimateVersion`——换口径即换版本，读数才有可比性。
// MessageEstimator delegates to core per message; its version literal is core's, so a
// changed rounding can never be compared against old readings by accident.
type MessageEstimator struct{}

// Version 返回估算器版本（= `core.EstimateVersion`）。
// Version returns the estimator version.
func (MessageEstimator) Version() string { return core.EstimateVersion }

// Estimate 估算一组消息的 token 数（按消息顺序累加，不做任何裁剪或去重）。
// Estimate sums the messages in order; it never trims or deduplicates.
func (MessageEstimator) Estimate(messages []StoredMessage) int {
	total := 0
	for _, m := range messages {
		total += core.EstimateMessage(m.AsMessage())
	}
	return total
}

// ContextSegment 是投影拼装出的一段输入：只带**段落标识**与正文，不带预算结论——
// 预算与拒绝由 core 的 `PromptBuilder` 判定，本层**不得**自行裁剪。
// ContextSegment is one assembled segment; budget decisions stay in core.
type ContextSegment struct {
	ID      string
	Content string
}

// 段落标识与 `core` 的 `final_segments` 字面量一致（持久化取值，不另立词表）。
// Segment ids match the persisted final_segments literals.
const (
	SegSystem  = "system"
	SegMemory  = "memory"
	SegState   = "state"
	SegSummary = "summary"
	SegRecent  = "recent"
)

// Projection 负责**按段序拼装**输入：系统（含工具定义）→ 记忆冻结 → 结构化状态 →
// 滚动摘要 → 最近完整工具组。它**不**判预算、**不**裁剪——超限时由 `PromptBuilder`
// 走整组丢弃 / 显式拒绝（C-31）。
// Projection assembles the segments in order and never trims.
type Projection struct{}

// Assemble 按固定段序拼装输入；空段落**不出现**在结果里（记录只写真发生的事）。
// Assemble lays the segments out in order and omits empty ones.
func (Projection) Assemble(system, memory, state string, chain []CompactionEntry, recent []StoredMessage) []ContextSegment {
	segments := make([]ContextSegment, 0, 5)
	if system != "" {
		segments = append(segments, ContextSegment{ID: SegSystem, Content: system})
	}
	if memory != "" {
		segments = append(segments, ContextSegment{ID: SegMemory, Content: memory})
	}
	if strings.TrimSpace(state) != "" {
		segments = append(segments, ContextSegment{ID: SegState, Content: state})
	}
	if text := chainSummaryText(chain); text != "" {
		segments = append(segments, ContextSegment{ID: SegSummary, Content: text})
	}
	if len(recent) > 0 {
		segments = append(segments, ContextSegment{ID: SegRecent, Content: ""})
	}
	return segments
}

// chainSummaryText 取链上**最后一条** committed 条目的摘要正文作为滚动摘要段。
// 链按父到子排列（`LatestCommittedChain` 的返回顺序）；空链返回空串，不产生段落。
// chainSummaryText takes the tail entry's summary text; an empty chain adds no segment.
func chainSummaryText(chain []CompactionEntry) string {
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Status == StatusCommitted && chain[i].Summary != "" {
			return chain[i].Summary
		}
	}
	return ""
}
