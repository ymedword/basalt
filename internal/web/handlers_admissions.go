package web

// 本文件是学习准入的**只读**管理面（US-21 颗粒度文档 §3.1.11）：
// `GET /api/v1/admissions` 与 `GET /api/v1/admissions/{candidate_id}`。
//
// 三条固定口径：
//   - 数据只来自 PostgreSQL 行（`Options.Admissions`）：清单用
//     `core.ParseAdmissionManifest`、来源用 `core.ParseAdmissionSource` 投影，
//     **不**读 `.basalt/admissions` 磁盘、**不**返回候选文件正文；
//   - 本文件**没有**写端点——批准 / 发布 / 撤回只在本机 CLI 执行（C-14）；
//   - `state` 按 `admissionDisplayState` 派生展示态：`approved` 且活跃批准已过
//     有效期时显示 `expired`，与 CLI 的 `AdmissionAdmin.admissionEffectiveState`
//     同一判据；数据库状态始终不变。
//
// Read-only admission routes. Rows come from PostgreSQL only (never the
// `.basalt/admissions` directory), no candidate file bodies leave the process, and no
// write endpoint lives here: approve / publish / withdraw stay on the local CLI.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 学习准入列表的固定窗口（与仓储 `normalizeAdmissionLimit` 同一对常量口径）。
// Fixed admission-list window, mirroring the store's own bounds.
const (
	defaultAdmissionListLimit = 50
	maxAdmissionListLimit     = 200
)

// 参数非法时的固定 400 文案（不回显请求值）与详情 404 前缀（与既有
// `"未找到运行："` 同一句式）。
// Fixed 400 texts (request values are never echoed) and the detail 404 prefix.
const (
	badAdmissionStateMessage = "state 取值非法（可选：proposed / evaluated / approved / rejected / canceled / published / withdrawn / expired）"
	badAdmissionLimitMessage = "limit 必须是 1–200 之间的整数"
	admissionNotFoundPrefix  = "未找到准入候选："
)

// AdmissionReader 是学习准入只读面的消费方端口（§3.1.11 的固定签名）：五个方法
// 直接由 `*postgres.AdmissionStore` 结构化满足，测试用 fake。候选缺失以
// `core.ErrAdmissionNotFound` 报告；评估 / 决策 / 发布记录缺失同样返回该哨兵，
// 详情按"尚未发生"投影为 null，而不是整份详情 500。
// AdmissionReader is the frozen consumer-side port; a missing sub-record is projected
// as null rather than failing the whole detail.
type AdmissionReader interface {
	ListCandidates(ctx context.Context, f core.AdmissionFilter) ([]core.AdmissionCandidate, error)
	GetCandidate(ctx context.Context, candidateID string) (core.AdmissionCandidate, error)
	LatestEvaluation(ctx context.Context, candidateID string) (core.AdmissionEvaluation, error)
	LatestDecision(ctx context.Context, candidateID string) (core.AdmissionDecision, error)
	GetPublication(ctx context.Context, candidateID string) (core.AdmissionPublication, error)
}

// admissionsBound 校验装配端口：Options.Admissions 为 nil 是装配错误，按 500
// INTERNAL 返回（不是 404、也不是空列表 200——空列表会被读成"确实没有候选"）。
// admissionsBound rejects a missing port as an assembly error, never an empty list.
func (s *Server) admissionsBound(w http.ResponseWriter) bool {
	if s.opts.Admissions != nil {
		return true
	}
	s.logger().Error("装配缺少 Admissions 端口")
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	return false
}

// parseAdmissionFilter 解析 `GET /api/v1/admissions` 的过滤参数：state / skill /
// limit；未知参数忽略。state 只认八个字面量（含派生展示态 expired），limit 必须
// 是 1–200 的整数；非法参数在触达仓储前返回错误（调用方映射 400，零查询）。
// parseAdmissionFilter accepts exactly the three documented parameters; illegal values
// are rejected before the store is touched.
func parseAdmissionFilter(r *http.Request) (core.AdmissionFilter, error) {
	q := r.URL.Query()
	filter := core.AdmissionFilter{
		Skill: q.Get("skill"),
		Limit: defaultAdmissionListLimit,
	}
	if q.Has("state") {
		state := q.Get("state")
		if !validAdmissionState(state) {
			return core.AdmissionFilter{}, errors.New(badAdmissionStateMessage)
		}
		filter.State = core.AdmissionState(state)
	}
	if q.Has("limit") {
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > maxAdmissionListLimit {
			return core.AdmissionFilter{}, errors.New(badAdmissionLimitMessage)
		}
		filter.Limit = limit
	}
	return filter, nil
}

// validAdmissionState 只接受契约状态机的八个字面量（expired 是派生展示态）。
// validAdmissionState accepts exactly the eight state machine literals.
func validAdmissionState(state string) bool {
	switch core.AdmissionState(state) {
	case core.AdmissionProposed, core.AdmissionEvaluated, core.AdmissionApproved,
		core.AdmissionRejected, core.AdmissionCanceled, core.AdmissionPublished,
		core.AdmissionWithdrawn, core.AdmissionExpired:
		return true
	default:
		return false
	}
}

// handleListAdmissions 返回固定窗口的候选列表；`state` 派生展示态与 CLI 同判据，
// `state=approved` 与 CLI 一样剔除已派生的 expired 行（读取侧投影，库不变）。
// handleListAdmissions projects the list with the same derived-display rule the CLI
// uses; the `approved` filter drops rows that derive to expired.
func (s *Server) handleListAdmissions(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.admissionsBound(w) {
		return
	}
	filter, err := parseAdmissionFilter(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	candidates, err := s.opts.Admissions.ListCandidates(r.Context(), filter)
	if err != nil {
		s.logger().Error("查询学习准入候选失败", "error", err, "actor", actor.Subject)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	now := s.now()
	// 只有 approved 行需要活跃批准判断展示态；已终态的行逐字返回（与 CLI 同一
	// 判定范围，避免为每个终态行多一次查询）。
	for i := range candidates {
		if candidates[i].State != core.AdmissionApproved {
			continue
		}
		decision, err := s.opts.Admissions.LatestDecision(r.Context(), candidates[i].CandidateID)
		switch {
		case err == nil:
			candidates[i].State = admissionDisplayState(candidates[i], &decision, now)
		case errors.Is(err, core.ErrAdmissionNotFound):
			// 行存在而决策行缺失属不一致数据：不派生、不改写，按存储态展示。
		default:
			s.logger().Error("读取学习准入决策失败", "candidate_id", candidates[i].CandidateID,
				"error", err, "actor", actor.Subject)
			s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
	}
	views := make([]AdmissionListView, 0, len(candidates))
	for _, candidate := range candidates {
		if filter.State == core.AdmissionApproved && candidate.State == core.AdmissionExpired {
			continue
		}
		views = append(views, admissionListView(candidate))
	}
	s.writeOK(w, views)
}

// handleGetAdmission 返回候选详情：清单摘要、结构化来源声明、最新评估 / 决策 /
// 发布记录。子记录缺失投影为 null（尚未发生），候选本身缺失才是 404。
// handleGetAdmission projects the detail; missing sub-records are null, only a missing
// candidate is a 404.
func (s *Server) handleGetAdmission(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.admissionsBound(w) {
		return
	}
	id := r.PathValue("candidate_id")
	candidate, err := s.opts.Admissions.GetCandidate(r.Context(), id)
	if err != nil {
		if errors.Is(err, core.ErrAdmissionNotFound) {
			s.writeError(w, http.StatusNotFound, codeNotFound, admissionNotFoundPrefix+id)
			return
		}
		s.logger().Error("读取学习准入候选失败", "candidate_id", id, "error", err, "actor", actor.Subject)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	view, err := s.admissionDetailView(r.Context(), candidate)
	if err != nil {
		s.logger().Error("读取学习准入详情失败", "candidate_id", id, "error", err, "actor", actor.Subject)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	s.writeOK(w, view)
}

// admissionDetailView 组装详情投影：清单与来源由 core 的严格解析器还原，任何
// 解析失败都按内部读取错误处理（数据库行由本模块写入，解析失败说明数据被外部
// 改写——不能把损坏行降级成"没有文件"）。
// admissionDetailView parses the frozen manifest and source declaration; a parse
// failure is an internal read error, never silently an empty projection.
func (s *Server) admissionDetailView(ctx context.Context, candidate core.AdmissionCandidate,
) (AdmissionDetailView, error) {
	files, err := core.ParseAdmissionManifest(candidate.ManifestJSON)
	if err != nil {
		return AdmissionDetailView{}, fmt.Errorf("解析候选清单: %w", err)
	}
	source, err := core.ParseAdmissionSource([]byte(candidate.SourceJSON))
	if err != nil {
		return AdmissionDetailView{}, fmt.Errorf("解析来源声明: %w", err)
	}
	// 来源 notes 是自由文本，不整段出站（§3.1.11 的脱敏口径）。
	// The free-form source notes never leaves the process in full.
	source.Notes = ""

	view := AdmissionDetailView{
		AdmissionListView: admissionListView(candidate),
		Source:            source,
		Files:             admissionFileViews(files),
	}
	var decision *core.AdmissionDecision
	evaluation, err := s.opts.Admissions.LatestEvaluation(ctx, candidate.CandidateID)
	switch {
	case err == nil:
		projected := admissionEvaluationView(evaluation)
		view.Evaluation = &projected
	case errors.Is(err, core.ErrAdmissionNotFound):
	default:
		return AdmissionDetailView{}, fmt.Errorf("读取最新评估: %w", err)
	}
	decisionRecord, err := s.opts.Admissions.LatestDecision(ctx, candidate.CandidateID)
	switch {
	case err == nil:
		decision = &decisionRecord
		projected := admissionDecisionView(decisionRecord)
		view.Decision = &projected
	case errors.Is(err, core.ErrAdmissionNotFound):
	default:
		return AdmissionDetailView{}, fmt.Errorf("读取最新决策: %w", err)
	}
	publication, err := s.opts.Admissions.GetPublication(ctx, candidate.CandidateID)
	switch {
	case err == nil:
		projected := admissionPublicationView(publication)
		view.Publication = &projected
	case errors.Is(err, core.ErrAdmissionNotFound):
	default:
		return AdmissionDetailView{}, fmt.Errorf("读取发布记录: %w", err)
	}
	view.State = string(admissionDisplayState(candidate, decision, s.now()))
	return view, nil
}

// admissionDisplayState 与 `AdmissionAdmin.admissionEffectiveState` 同一判据：
// `approved` 且活跃批准有效期已过（`now >= expires_at`，含相等）时显示
// `expired`；其余逐字返回存储态。只改返回值，不写库、不触发后台扫描（C-08）。
// admissionDisplayState applies the single expiry predicate shared with the CLI: the
// display-only expired state begins at now >= expires_at and never touches storage.
func admissionDisplayState(candidate core.AdmissionCandidate, decision *core.AdmissionDecision,
	now time.Time,
) core.AdmissionState {
	if candidate.State != core.AdmissionApproved || decision == nil ||
		decision.Decision != core.AdmissionApprove || !decision.SupersededAt.IsZero() ||
		decision.ExpiresAt.IsZero() {
		return candidate.State
	}
	if !now.Before(decision.ExpiresAt) {
		return core.AdmissionExpired
	}
	return candidate.State
}

// admissionListView 把候选行投影为列表行。
// admissionListView projects one candidate row.
func admissionListView(candidate core.AdmissionCandidate) AdmissionListView {
	return AdmissionListView{
		CandidateID:     candidate.CandidateID,
		SkillName:       candidate.SkillName,
		State:           string(candidate.State),
		RiskTier:        string(candidate.RiskTier),
		ProposerSubject: candidate.ProposerSubject,
		CandidateHash:   candidate.CandidateHash,
		SourceHash:      candidate.SourceHash,
		CreatedAt:       formatTime(candidate.CreatedAt),
		UpdatedAt:       formatTime(candidate.UpdatedAt),
	}
}

// admissionFileViews 投影冻结清单的文件摘要（**没有**正文）。
// admissionFileViews projects the frozen digests; file bodies never appear.
func admissionFileViews(digests []core.AdmissionFileDigest) []AdmissionFileView {
	views := make([]AdmissionFileView, 0, len(digests))
	for _, digest := range digests {
		views = append(views, AdmissionFileView{
			Path:   digest.Path,
			SHA256: digest.SHA256,
			Size:   digest.Size,
		})
	}
	return views
}

// admissionEvaluationView 投影最新评估：七个维度的判定与理由、脚本清单与绑定
// 证据；`baseline_json` / `approved_scope_json` 是持久内部结构，不在此出站。
// admissionEvaluationView projects the latest evaluation without internal blobs.
func admissionEvaluationView(evaluation core.AdmissionEvaluation) AdmissionEvaluationView {
	dimensions := make([]core.AdmissionDimension, len(evaluation.Dimensions))
	copy(dimensions, evaluation.Dimensions)
	return AdmissionEvaluationView{
		EvaluationID:      evaluation.EvaluationID,
		CandidateRevision: evaluation.CandidateRevision,
		CandidateHash:     evaluation.CandidateHash,
		EvaluationHash:    evaluation.EvaluationHash,
		GraderVersion:     evaluation.GraderVersion,
		PolicyHash:        evaluation.PolicyHash,
		Dimensions:        dimensions,
		Scripts:           stringSlice(evaluation.Scripts),
		Aggregate:         string(evaluation.Aggregate),
		RiskTier:          string(evaluation.RiskTier),
		Referrers:         stringSlice(evaluation.Referrers),
		EvaluatedAt:       formatTime(evaluation.EvaluatedAt),
	}
}

// admissionDecisionView 投影最新决策：批准者主体、有效期与理由（写入前已按
// 2000 字符上限与凭证扫描校验，可整段展示）。
// admissionDecisionView projects the latest decision; its reason is bounded and
// credential-scanned before it is ever written.
func admissionDecisionView(decision core.AdmissionDecision) AdmissionDecisionView {
	acknowledged := make([]string, 0, len(decision.Acknowledged))
	for _, name := range decision.Acknowledged {
		acknowledged = append(acknowledged, string(name))
	}
	return AdmissionDecisionView{
		DecisionID:      decision.DecisionID,
		Decision:        string(decision.Decision),
		Reason:          decision.Reason,
		Acknowledged:    acknowledged,
		RiskTier:        string(decision.RiskTier),
		ApproverSubject: decision.ApproverSubject,
		ExpiresAt:       formatTime(decision.ExpiresAt),
		SupersededAt:    formatTime(decision.SupersededAt),
		CreatedAt:       formatTime(decision.CreatedAt),
	}
}

// admissionPublicationView 投影发布记录：版本号与内容哈希、发布 / 撤回状态与
// 撤回分类（`withdraw_version_id` 为 0 表示未移动指针）。
// admissionPublicationView projects the publication record, including the withdrawal
// classification; a zero withdraw_version_id means the pointer never moved.
func admissionPublicationView(publication core.AdmissionPublication) AdmissionPublicationView {
	return AdmissionPublicationView{
		SkillName:         publication.SkillName,
		VersionID:         publication.VersionID,
		VersionNo:         publication.VersionNo,
		ContentHash:       publication.ContentHash,
		State:             string(publication.State),
		PublishedAt:       formatTime(publication.PublishedAt),
		WithdrawnAt:       formatTime(publication.WithdrawnAt),
		WithdrawReason:    publication.WithdrawReason,
		WithdrawVersionID: publication.WithdrawVersionID,
	}
}
