package web

// 本文件是历史回查三条端点的**响应投影**（US-16 / T056）：形状逐字段取自冻结契约
// `docs/contracts/openapi.yaml`（`HistoryHit` / `HistorySearchResult` /
// `MessageDetail` / `MessageEvidenceRefView`），沿用两条既有视图口径——数组恒为
// `[]`（不是 `null`）、时间经 `formatTime` 渲染为 RFC3339 UTC（零值空串）。
//
// 两条**投影级**约束（不是格式化偏好）：
//
//  1. `HistoryHit` 只投影指针与片段：领域类型 `history.HistoryHit` 本来就没有正文位，
//     投影层**不得**从任何缓存 / 旁路补上正文——"命中不是引用依据"靠形状保证。
//  2. `MessageDetail.evidence_refs` 的 `validity` 只来自**当次** `Check` 的结论：
//     投影层**不**推算、**不**缓存、**不**把缺判定写成"有效"；`reason` 取冻结四值 +
//     空串（`valid` 为真时），与 `ValidityView` 同一张枚举。
//
// This file projects the three history endpoints. Hits carry no content by shape, and
// an evidence ref's validity comes only from the current Check — never inferred, never
// cached, never defaulted to valid.

import (
	"github.com/ymedword/basalt/internal/history"
)

// HistoryHitView 是 `HistoryHit`（契约必填：message_id / session_id / run_id / role /
// snippet / created_at；`score` 可选——首批关键词路径的分数口径由实现方给出）。
// HistoryHitView mirrors the contract's HistoryHit.
type HistoryHitView struct {
	MessageID string  `json:"message_id"`
	SessionID string  `json:"session_id"`
	RunID     string  `json:"run_id"`
	Role      string  `json:"role"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"`
}

// HistorySearchResultView 是 `HistorySearchResult`（必填：hits；`truncated` 可选、
// 默认 false）。`truncated` 恒出站：它是"本页不是全部"的唯一信号，省掉会让调用方
// 以为已穷尽（§2.10.4）。
// HistorySearchResultView always emits truncated; it is the only "not the whole page"
// signal the caller gets.
type HistorySearchResultView struct {
	Hits      []HistoryHitView `json:"hits"`
	Truncated bool             `json:"truncated"`
}

// historySearchResultView 投影一页检索结果：命中恒为 `[]`，片段原样出站。
// historySearchResultView renders hits as [] and passes snippets through.
func historySearchResultView(result history.SearchResult) HistorySearchResultView {
	hits := make([]HistoryHitView, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, HistoryHitView{
			MessageID: hit.MessageID,
			SessionID: hit.SessionID,
			RunID:     hit.RunID,
			Role:      hit.Role,
			Snippet:   hit.Snippet,
			Score:     hit.Score,
			CreatedAt: formatTime(hit.CreatedAt),
		})
	}
	return HistorySearchResultView{Hits: hits, Truncated: result.Truncated}
}

// MessageEvidenceRefView 是 `MessageEvidenceRefView`（必填：ref / validity）：
// 引用本身 + **当次**重查的结论。
// MessageEvidenceRefView pairs the reference with its freshly checked verdict.
type MessageEvidenceRefView struct {
	Ref      EvidenceRefView `json:"ref"`
	Validity ValidityView    `json:"validity"`
}

// MessageDetailView 是 `MessageDetail`（必填：message_id / session_id / run_id / seq /
// role / content / created_at / truncated）；`tool_call_id` 与 `evidence_refs` 可缺省
// ——没有工具调用的消息不带前者，没有来源引用的消息不带后者（空数组与"没有引用"
// 在契约里是两回事，前者会被读成"带了一组空引用"）。
// MessageDetailView mirrors the contract's MessageDetail; absent optional fields stay
// absent rather than being emitted as empty values.
type MessageDetailView struct {
	MessageID    string                   `json:"message_id"`
	SessionID    string                   `json:"session_id"`
	RunID        string                   `json:"run_id"`
	Seq          int64                    `json:"seq"`
	Role         string                   `json:"role"`
	Content      string                   `json:"content"`
	Truncated    bool                     `json:"truncated"`
	ToolCallID   string                   `json:"tool_call_id,omitempty"`
	CreatedAt    string                   `json:"created_at"`
	EvidenceRefs []MessageEvidenceRefView `json:"evidence_refs,omitempty"`
}

// messageDetailView 投影一条消息：原文与元数据逐字照搬，引用标注由调用方先算好。
// messageDetailView copies content and metadata verbatim; the annotations arrive
// pre-computed from the handler.
func messageDetailView(view history.MessageView, refs []MessageEvidenceRefView) MessageDetailView {
	return MessageDetailView{
		MessageID:    view.MessageID,
		SessionID:    view.SessionID,
		RunID:        view.RunID,
		Seq:          view.Seq,
		Role:         view.Role,
		Content:      view.Content,
		Truncated:    view.Truncated,
		ToolCallID:   view.ToolCallID,
		CreatedAt:    formatTime(view.CreatedAt),
		EvidenceRefs: refs,
	}
}
