package web

// 本文件是 US-22 受控自我进化的**只读学习查询面**（颗粒度文档 §3.1.10 / FR-22-11）：
// 状态、抽取列表与详情、方法草案列表与详情，共五条 GET。**没有写端点**——确认与删除
// 复用既有记忆管理动作，启用只改 `basalt.yaml`；本文件不提供晋升、重试或发布，
// 端口 `core.LearningAdmin` 本身也没有写方法。
//
// 三条边界在这里守：
//  1. **失败关闭**：端口未装配 → 固定 500；存储错误 → 500 并记日志，**不得**用空列表
//     冒充"没有抽取"——那会让"存储挂了"看起来像"学习没跑过"。
//  2. **纯请求非法不触达端口**：`state` 只接受五字面量，`limit` 只接受 1–200（缺省 50）；
//     越界 400 且零端口调用——与 CLI 的检查同一口径，不把过滤错误翻译成空页。
//  3. **只投影复核所需字段**：候选的来源调用方 / Key 不对外输出（引用抽取行已给出
//     来源）；草案列表不带正文，正文只在草案详情出站。
// The read-only learning surface of US-22: five GET rows and no write endpoint. Store
// failures fail closed (500, never an empty list); pure request errors are rejected
// before the port with the same closed sets the CLI enforces.
//
// 视图结构放在本文件而不是 `views.go`：五个端点与它们的投影是一份完整契约，集中摆放
// 便于与 `docs/contracts/openapi.yaml` 的 learning 段逐字段对号。
// The view structs live beside their handlers so the whole learning contract reads in
// one place next to the matching OpenAPI section.

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// defaultLearningListLimit / maxLearningListLimit 与存储层缺省同值（§3.1.10：limit
	// 1–200，缺省 50）。HTTP 层显式落到缺省值，响应不依赖存储层的兜底。
	// The HTTP layer applies the documented default itself instead of relying on the
	// store's clamp.
	defaultLearningListLimit = 50
	maxLearningListLimit     = 200

	// messageLearningExtractionNotFound / messageLearningDraftNotFound 是详情端点 404
	// 的固定文案；两种资源分开说，不合并成"某种学习记录不存在"。
	messageLearningExtractionNotFound = "学习抽取记录不存在"
	messageLearningDraftNotFound      = "学习方法草案不存在"
	// messageLearningLimitRange 与压缩链端点同口径：越界是请求形状错误，不是空页。
	messageLearningLimitRange = "limit 必须是 1 到 200 之间的整数"
	// messageLearningStateInvalid 的前缀后接原样取值；只报"取值非法"，不列内部规则。
	messageLearningStateInvalid = "state 取值非法："
)

// learningExtractionStates 是抽取状态闭集（与 memory_extractions.status 的 CHECK 同源，
// 也与 CLI 的 `learningStates` 同一批字面量）。空的 state 表示不过滤。
// learningExtractionStates mirrors the extraction status CHECK set; empty means no filter.
var learningExtractionStates = []string{"pending", "success", "failed", "skipped", "unknown"}

// LearningStatusView 是 `GET /api/v1/learning/status` 的响应体（契约
// LearningStatusView）：与 CLI `learning status --json` 的字段逐项同源。
// LearningStatusView mirrors the CLI's status projection field for field.
type LearningStatusView struct {
	Enabled             bool     `json:"enabled"`
	Agents              []string `json:"agents"`
	UnknownAgents       []string `json:"unknown_agents"`
	RuleVersion         string   `json:"rule_version"`
	PromptVersion       string   `json:"prompt_version"`
	AttemptsUsed        int      `json:"attempts_used"`
	AttemptLimit        int      `json:"attempt_limit"`
	Exhausted           bool     `json:"exhausted"`
	BudgetWindowSeconds int64    `json:"budget_window_seconds"`
	PendingExtractions  int      `json:"pending_extractions"`
	UnknownExtractions  int      `json:"unknown_extractions"`
	OrphanDrafts        int      `json:"orphan_drafts"`
	LastExtractionAt    string   `json:"last_extraction_at"`
}

// LearningExtractionView 是一行抽取账本的出站投影。`existing_entries` 与 `config_hash`
// 是内部计数 / 装配哈希，不参与页面复核，故不出站（与 CLI 的机器投影同字段集）。
// One extraction row; the internal counters and the assembly hash stay inside.
type LearningExtractionView struct {
	ExtractionID          string `json:"extraction_id"`
	Agent                 string `json:"agent"`
	SessionID             string `json:"session_id"`
	RunID                 string `json:"run_id"`
	LastAttemptRunID      string `json:"last_attempt_run_id"`
	OriginCallerID        string `json:"origin_caller_id"`
	OriginKeyID           string `json:"origin_key_id"`
	Status                string `json:"status"`
	WindowFromMessageID   int64  `json:"window_from_message_id"`
	WindowToMessageID     int64  `json:"window_to_message_id"`
	WindowMessages        int    `json:"window_messages"`
	DroppedWindowMessages int    `json:"dropped_window_messages"`
	PriorMessages         int    `json:"prior_messages"`
	Attempts              int    `json:"attempts"`
	ItemsTotal            int    `json:"items_total"`
	ItemsAccepted         int    `json:"items_accepted"`
	ItemsDropped          int    `json:"items_dropped"`
	PromptVersion         string `json:"prompt_version"`
	RuleVersion           string `json:"rule_version"`
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	FailureClass          string `json:"failure_class"`
	ErrorMessage          string `json:"error_message"`
	CreatedAt             string `json:"created_at"`
	LastAttemptAt         string `json:"last_attempt_at"`
	FinishedAt            string `json:"finished_at"`
	VerifiedAt            string `json:"verified_at"`
}

// LearningExtractionListView 是列表端点的响应体：只回账本行，**不**展开候选与草案
// （展开只发生在详情；列表页不需要逐行三条子查询）。
// The list response stays flat; candidates and drafts are expanded by the detail row.
type LearningExtractionListView struct {
	Extractions []LearningExtractionView `json:"extractions"`
}

// LearningCandidateView 是候选的复核投影：证据逐字，来源关系可追溯。来源调用方 / Key
// 不在此出现——它们属于抽取行（来源身份），不是候选属性。
// A reviewable candidate projection; the origin identity stays on the extraction row.
type LearningCandidateView struct {
	EntryID               int64                  `json:"entry_id"`
	State                 string                 `json:"state"`
	Content               string                 `json:"content"`
	Kind                  string                 `json:"kind"`
	Certainty             string                 `json:"certainty"`
	Confidence            float64                `json:"confidence"`
	Evidence              []LearningEvidenceView `json:"evidence"`
	SourceEntryIDs        []int64                `json:"source_entry_ids"`
	SuggestedSupersedesID int64                  `json:"suggested_supersedes_id"`
	EvidenceAt            string                 `json:"evidence_at"`
	ValidTo               string                 `json:"valid_to"`
	TimeConfidence        string                 `json:"time_confidence"`
	CreatedAt             string                 `json:"created_at"`
}

// LearningEvidenceView 是一条逐字证据：消息主键、所属 Run、角色、引文与消息时刻。
// 引文只经管理面读模型出站；日志与审计都不落它。
// One verbatim citation; it leaves the process only through this read model.
type LearningEvidenceView struct {
	MessageID int64  `json:"message_id"`
	RunID     string `json:"run_id"`
	Role      string `json:"role"`
	Quote     string `json:"quote"`
	MessageAt string `json:"message_at"`
}

// LearningExtractionDetailView 是详情响应体：账本行 + 候选视图 + 草案摘要。
// The detail response: the row plus candidates and draft summaries.
type LearningExtractionDetailView struct {
	Extraction LearningExtractionView  `json:"extraction"`
	Candidates []LearningCandidateView `json:"candidates"`
	Drafts     []LearningDraftMetaView `json:"drafts"`
}

// LearningDraftMetaView 是草案列表 / 摘要投影：**没有正文**。正文只在草案详情出站
// （`learning drafts show` 是 CLI 侧的唯一出口，HTTP 侧对应详情端点）。
// Draft metadata without the body; the body leaves only through the detail row.
type LearningDraftMetaView struct {
	DraftID       string `json:"draft_id"`
	ExtractionID  string `json:"extraction_id"`
	Agent         string `json:"agent"`
	Name          string `json:"name"`
	ContentSHA256 string `json:"content_sha256"`
	RelDir        string `json:"rel_dir"`
	CreatedAt     string `json:"created_at"`
}

// LearningDraftListView 是草案列表端点的响应体。
// The draft list response.
type LearningDraftListView struct {
	Drafts []LearningDraftMetaView `json:"drafts"`
}

// LearningDraftDetailView 是草案详情：元数据 + `DRAFT.md` 正文 + `evidence.json` 证据。
// The draft detail adds the body and the evidence file to the metadata.
type LearningDraftDetailView struct {
	LearningDraftMetaView
	Content  string                 `json:"content"`
	Evidence []LearningEvidenceView `json:"evidence"`
}

// handleAdminLearningStatus 处理 `GET /api/v1/learning/status`：只读状态与预算。
// handleAdminLearningStatus serves the read-only status and budget view.
func (s *Server) handleAdminLearningStatus(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.LearningAdmin == nil {
		s.writeAdminPortMissing(w, "LearningAdmin", r.Method+" "+r.URL.Path)
		return
	}
	status, err := s.opts.LearningAdmin.Status(r.Context())
	if err != nil {
		s.writeLearningAdminError(w, err, "读取学习状态")
		return
	}
	s.writeOK(w, learningStatusView(status))
}

// handleAdminListLearningExtractions 处理 `GET /api/v1/learning/extractions`。
// handleAdminListLearningExtractions serves the extraction list.
func (s *Server) handleAdminListLearningExtractions(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.LearningAdmin == nil {
		s.writeAdminPortMissing(w, "LearningAdmin", r.Method+" "+r.URL.Path)
		return
	}
	query := r.URL.Query()
	state := strings.TrimSpace(query.Get("state"))
	if state != "" && !slices.Contains(learningExtractionStates, state) {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageLearningStateInvalid+state)
		return
	}
	limit, ok := s.learningListLimit(w, r)
	if !ok {
		return
	}
	views, err := s.opts.LearningAdmin.ListExtractions(r.Context(), core.LearningExtractionFilter{
		Agent: strings.TrimSpace(query.Get("agent")),
		State: state,
		Limit: limit,
	})
	if err != nil {
		s.writeLearningAdminError(w, err, "列出学习抽取")
		return
	}
	items := make([]LearningExtractionView, 0, len(views))
	for _, view := range views {
		items = append(items, learningExtractionRowView(view.Extraction))
	}
	s.writeOK(w, LearningExtractionListView{Extractions: items})
}

// handleAdminLoadLearningExtraction 处理 `GET /api/v1/learning/extractions/{id}`：
// 账本行 + 候选视图 + 草案摘要；不存在 → 404（`core.ErrLearningNotFound`）。
// handleAdminLoadLearningExtraction serves one extraction with its candidates and drafts.
func (s *Server) handleAdminLoadLearningExtraction(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.LearningAdmin == nil {
		s.writeAdminPortMissing(w, "LearningAdmin", r.Method+" "+r.URL.Path)
		return
	}
	view, err := s.opts.LearningAdmin.LoadExtraction(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeLearningAdminError(w, err, "读取学习抽取")
		return
	}
	candidates := make([]LearningCandidateView, 0, len(view.Candidates))
	for _, candidate := range view.Candidates {
		candidates = append(candidates, learningCandidateRowView(candidate))
	}
	drafts := make([]LearningDraftMetaView, 0, len(view.Drafts))
	for _, draft := range view.Drafts {
		drafts = append(drafts, learningDraftMetaView(draft))
	}
	s.writeOK(w, LearningExtractionDetailView{
		Extraction: learningExtractionRowView(view.Extraction),
		Candidates: candidates,
		Drafts:     drafts,
	})
}

// handleAdminListLearningDrafts 处理 `GET /api/v1/learning/drafts`：只读草案元数据。
// handleAdminListLearningDrafts serves the read-only draft list.
func (s *Server) handleAdminListLearningDrafts(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.LearningAdmin == nil {
		s.writeAdminPortMissing(w, "LearningAdmin", r.Method+" "+r.URL.Path)
		return
	}
	limit, ok := s.learningListLimit(w, r)
	if !ok {
		return
	}
	views, err := s.opts.LearningAdmin.ListDrafts(r.Context(), core.LearningDraftFilter{
		Agent: strings.TrimSpace(r.URL.Query().Get("agent")),
		Limit: limit,
	})
	if err != nil {
		s.writeLearningAdminError(w, err, "列出学习方法草案")
		return
	}
	drafts := make([]LearningDraftMetaView, 0, len(views))
	for _, view := range views {
		drafts = append(drafts, learningDraftMetaView(view))
	}
	s.writeOK(w, LearningDraftListView{Drafts: drafts})
}

// handleAdminLoadLearningDraft 处理 `GET /api/v1/learning/drafts/{id}`：元数据 +
// `DRAFT.md` 正文 + `evidence.json`；不存在 → 404（`core.ErrLearningDraftNotFound`）。
// handleAdminLoadLearningDraft serves one draft with its body and evidence.
func (s *Server) handleAdminLoadLearningDraft(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.LearningAdmin == nil {
		s.writeAdminPortMissing(w, "LearningAdmin", r.Method+" "+r.URL.Path)
		return
	}
	view, err := s.opts.LearningAdmin.LoadDraft(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeLearningAdminError(w, err, "读取学习方法草案")
		return
	}
	s.writeOK(w, LearningDraftDetailView{
		LearningDraftMetaView: learningDraftMetaView(view),
		Content:               view.Content,
		Evidence:              learningEvidenceViews(view.Evidence),
	})
}

// learningListLimit 解析 `limit`：缺省 / 空串取 50，给了就必须是 1–200 的整数；
// 越界 400 且**不**触达端口（与压缩链、流程分页的既有口径一致）。
// learningListLimit applies the documented default and rejects out-of-range values
// before the port, matching the compaction and flow list rows.
func (s *Server) learningListLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxLearningListLimit {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageLearningLimitRange)
			return 0, false
		}
		return parsed, true
	}
	return defaultLearningListLimit, true
}

// writeLearningAdminError 是学习查询面的唯一错误出口：两种"查不到"分别 404；
// 其余（存储不可用、扫描失败）一律 500 并记 ERROR——**不得**降级成空列表或空状态。
// The one error exit of the learning surface: two not-found sentinels map to 404 and
// everything else fails closed with 500 instead of degrading to an empty page.
func (s *Server) writeLearningAdminError(w http.ResponseWriter, err error, action string) {
	switch {
	case errors.Is(err, core.ErrLearningNotFound):
		s.writeError(w, http.StatusNotFound, codeNotFound, messageLearningExtractionNotFound)
	case errors.Is(err, core.ErrLearningDraftNotFound):
		s.writeError(w, http.StatusNotFound, codeNotFound, messageLearningDraftNotFound)
	default:
		s.logger().Error("学习查询失败", "action", action, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	}
}

// learningStatusView 投影状态读模型（空名单保持 `[]`，不写成 null）。
// learningStatusView projects the status model; empty lists stay [].
func learningStatusView(status core.LearningStatus) LearningStatusView {
	return LearningStatusView{
		Enabled:             status.Enabled,
		Agents:              stringSlice(status.Agents),
		UnknownAgents:       stringSlice(status.UnknownAgents),
		RuleVersion:         status.RuleVersion,
		PromptVersion:       status.PromptVersion,
		AttemptsUsed:        status.AttemptsUsed,
		AttemptLimit:        status.AttemptLimit,
		Exhausted:           status.Exhausted,
		BudgetWindowSeconds: int64(status.BudgetWindow.Seconds()),
		PendingExtractions:  status.PendingExtractions,
		UnknownExtractions:  status.UnknownExtractions,
		OrphanDrafts:        status.OrphanDrafts,
		LastExtractionAt:    formatTime(status.LastExtractionAt),
	}
}

// learningExtractionRowView 投影一行抽取账本；时间统一 RFC3339 UTC，零值渲染空串
// （`finished_at` / `verified_at` 对未完成 / 未查证的行就是空串）。
// learningExtractionRowView projects one row; zero times render as empty strings.
func learningExtractionRowView(extraction core.LearningExtraction) LearningExtractionView {
	return LearningExtractionView{
		ExtractionID:          extraction.ExtractionID,
		Agent:                 extraction.Agent,
		SessionID:             extraction.SessionID,
		RunID:                 extraction.RunID,
		LastAttemptRunID:      extraction.LastAttemptRunID,
		OriginCallerID:        extraction.OriginCallerID,
		OriginKeyID:           extraction.OriginKeyID,
		Status:                string(extraction.Status),
		WindowFromMessageID:   extraction.WindowFromMessageID,
		WindowToMessageID:     extraction.WindowToMessageID,
		WindowMessages:        extraction.WindowMessages,
		DroppedWindowMessages: extraction.DroppedWindowMessages,
		PriorMessages:         extraction.PriorMessages,
		Attempts:              extraction.Attempts,
		ItemsTotal:            extraction.ItemsTotal,
		ItemsAccepted:         extraction.ItemsAccepted,
		ItemsDropped:          extraction.ItemsDropped,
		PromptVersion:         extraction.PromptVersion,
		RuleVersion:           extraction.RuleVersion,
		Provider:              extraction.Provider,
		Model:                 extraction.Model,
		FailureClass:          string(extraction.FailureClass),
		ErrorMessage:          extraction.ErrorMessage,
		CreatedAt:             formatTime(extraction.CreatedAt),
		LastAttemptAt:         formatTime(extraction.LastAttemptAt),
		FinishedAt:            formatTime(extraction.FinishedAt),
		VerifiedAt:            formatTime(extraction.VerifiedAt),
	}
}

// learningCandidateRowView 投影一条候选；`evidence` 与 `source_entry_ids` 恒为数组
// （空集合输出 `[]`），来源调用方 / Key 不投影。
// learningCandidateRowView keeps the arrays non-nil and never projects the origin.
func learningCandidateRowView(candidate core.LearningCandidateView) LearningCandidateView {
	sourceIDs := make([]int64, 0, len(candidate.SourceEntryIDs))
	sourceIDs = append(sourceIDs, candidate.SourceEntryIDs...)
	return LearningCandidateView{
		EntryID:               candidate.EntryID,
		State:                 candidate.State,
		Content:               candidate.Content,
		Kind:                  candidate.Kind,
		Certainty:             candidate.Certainty,
		Confidence:            candidate.Confidence,
		Evidence:              learningEvidenceViews(candidate.Evidence),
		SourceEntryIDs:        sourceIDs,
		SuggestedSupersedesID: candidate.SuggestedSupersedesID,
		EvidenceAt:            formatTime(candidate.EvidenceAt),
		ValidTo:               formatTime(candidate.ValidTo),
		TimeConfidence:        candidate.TimeConfidence,
		CreatedAt:             formatTime(candidate.CreatedAt),
	}
}

// learningDraftMetaView 投影草案元数据（不含正文）。
// learningDraftMetaView projects the metadata without the body.
func learningDraftMetaView(draft core.LearningDraftView) LearningDraftMetaView {
	return LearningDraftMetaView{
		DraftID:       draft.DraftID,
		ExtractionID:  draft.ExtractionID,
		Agent:         draft.Agent,
		Name:          draft.Name,
		ContentSHA256: draft.ContentSHA256,
		RelDir:        draft.RelDir,
		CreatedAt:     formatTime(draft.CreatedAt),
	}
}

// learningEvidenceViews 投影证据列表（空集合保持 `[]`，不写成 null）。
// learningEvidenceViews keeps an empty list as [].
func learningEvidenceViews(evidence []core.LearningEvidence) []LearningEvidenceView {
	views := make([]LearningEvidenceView, 0, len(evidence))
	for _, item := range evidence {
		views = append(views, LearningEvidenceView{
			MessageID: item.MessageID,
			RunID:     item.RunID,
			Role:      item.Role,
			Quote:     item.Quote,
			MessageAt: formatTime(item.MessageAt),
		})
	}
	return views
}
