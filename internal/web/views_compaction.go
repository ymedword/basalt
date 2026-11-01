package web

// 本文件是压缩只读视图端点的**响应投影**（US-16 / T089）：形状逐字段取自冻结契约
// `docs/contracts/openapi.yaml` 的 `CompactionView` / `CompactionSummaryView`。
//
// 三条**投影级**约束（不是格式化偏好）：
//
//  1. **全部字段恒出站**：契约虽只把七项标为必填，但管理台面板要靠 `status` +
//     `reason_code` 显示降级原因、靠 `source_refs` / `included_refs` 区分审计集与
//     当前依赖集——省掉任何一个都会让面板安静地少一列。`omitempty` 在这里是 bug。
//  2. **两个引用集合分开出站**：`source_refs`（审计集：生成时涉及的全部引用，含此后
//     失效 / 被剔除的）与 `included_refs`（当前依赖集：正文里真正含其原文的来源）
//     **不得**合并成一个布尔或一个集合——注入资格只看后者，前者只证明"曾涉及什么"。
//  3. **摘要只是派生视图**：`summary_view.summary` 原样出站，`truncated` 恒 false
//     ——读取侧不截断也不修补，截断或补全会让面板上的摘要看起来像"原文的一部分"。
//     面板另以文案声明它**不是**引用依据（引用必须回 `POST /sources/view` 校验）。
//
// This file projects the read-only compaction view. Every contract field is emitted,
// the two reference sets stay separate, and the summary is passed through untouched:
// it is a derived view, never a citation.

import (
	"github.com/ymedword/basalt/internal/compaction"
)

// CompactionSummaryView 是 `CompactionSummaryView`（必填：compaction_id / summary /
// truncated）。
// CompactionSummaryView mirrors the contract's summary sub-view.
type CompactionSummaryView struct {
	CompactionID string `json:"compaction_id"`
	Summary      string `json:"summary"`
	Truncated    bool   `json:"truncated"`
}

// CompactionView 是 `CompactionView`：一条压缩条目的只读派生形态。
// 字段顺序与契约一致，便于逐行对照。
// CompactionView mirrors the contract's CompactionView; field order follows the schema.
type CompactionView struct {
	CompactionID         string                `json:"compaction_id"`
	ScopeSessionID       string                `json:"scope_session_id"`
	ScopeRunID           string                `json:"scope_run_id"`
	ChainEpoch           int                   `json:"chain_epoch"`
	ParentCompactionID   string                `json:"parent_compaction_id"`
	TriggerRunID         string                `json:"trigger_run_id"`
	AttemptID            string                `json:"attempt_id"`
	CoveredFromMessageID string                `json:"covered_from_message_id"`
	CutMessageID         string                `json:"cut_message_id"`
	FirstKeptMessageID   string                `json:"first_kept_message_id"`
	StrategyVersion      string                `json:"strategy_version"`
	PromptVersion        string                `json:"prompt_version"`
	Model                string                `json:"model"`
	TokensBefore         int                   `json:"tokens_before"`
	TokensAfter          int                   `json:"tokens_after"`
	Status               string                `json:"status"`
	ReasonCode           string                `json:"reason_code"`
	SourceRefs           []EvidenceRefView     `json:"source_refs"`
	IncludedRefs         []EvidenceRefView     `json:"included_refs"`
	CreatedAt            string                `json:"created_at"`
	StaleAt              string                `json:"stale_at"`
	SummaryView          CompactionSummaryView `json:"summary_view"`
}

// compactionViews 投影一页压缩条目：条目不重排、不筛选（次序与代次由存储层固定），
// 引用集合各自恒为 `[]`，时间经 `formatTime` 渲染为 RFC3339 UTC（零值空串）。
// compactionViews projects one page verbatim; the store already fixed order and epoch.
func compactionViews(entries []compaction.CompactionEntry) []CompactionView {
	views := make([]CompactionView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, CompactionView{
			CompactionID:         entry.CompactionID,
			ScopeSessionID:       entry.Scope.SessionID,
			ScopeRunID:           entry.Scope.RunID,
			ChainEpoch:           entry.ChainEpoch,
			ParentCompactionID:   entry.ParentCompactionID,
			TriggerRunID:         entry.TriggerRunID,
			AttemptID:            entry.AttemptID,
			CoveredFromMessageID: entry.CoveredFromMessageID,
			CutMessageID:         entry.CutMessageID,
			FirstKeptMessageID:   entry.FirstKeptMessageID,
			StrategyVersion:      entry.StrategyVersion,
			PromptVersion:        entry.PromptVersion,
			Model:                entry.Model,
			TokensBefore:         entry.TokensBefore,
			TokensAfter:          entry.TokensAfter,
			Status:               entry.Status,
			ReasonCode:           entry.ReasonCode,
			SourceRefs:           evidenceRefViews(entry.SourceRefs),
			IncludedRefs:         evidenceRefViews(entry.IncludedRefs),
			CreatedAt:            formatTime(entry.CreatedAt),
			StaleAt:              formatTime(entry.StaleAt),
			SummaryView: CompactionSummaryView{
				CompactionID: entry.CompactionID,
				Summary:      entry.Summary,
				// 读取侧不截断：存了什么就出什么，截断要由未来的存储层显式标记。
				// No read-side truncation: what was persisted is what ships.
				Truncated: false,
			},
		})
	}
	return views
}
