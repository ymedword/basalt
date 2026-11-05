package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是技能学习准入四张表的**唯一 SQL 入口**（颗粒度文档 US-21 §3.1.4）：
// 候选 / 评估 / 决策 / 发布记录。每个写方法在**同一事务**内写领域行、做候选状态 CAS
// 并调用 finishOperationTx 回填操作终态；任一步失败整体回滚。读方法不做状态派生——
// `expired` 只作为列表过滤的 SQL 翻译，不回写数据库。
// This file is the only SQL entry point for the four admission tables. Every write runs
// its domain row, the candidate state CAS and finishOperationTx in one transaction; reads
// never derive or rewrite stored state (`expired` is a list-filter translation only).

// 准入列表窗口与既有管理操作列表同口径：缺省 50、上限 200（颗粒度文档 §2.6）。
// The admission list window matches the management list: default 50, hard cap 200.
const (
	defaultAdmissionListLimit = 50
	maxAdmissionListLimit     = 200
)

// 四张表的读取列序；scan* 按同一顺序扫描，列清单只在此处出现一次。
// The single read-column order for each table; each scan* mirrors it exactly.
const (
	admissionCandidateColumns = `candidate_id, skill_name, state, revision, candidate_hash,
		source_hash, manifest_json, source_json, baseline_json, proposer_subject,
		proposer_auth_method, proposer_source, risk_tier, terminal_reason,
		terminal_operation_id, operation_id, created_at, updated_at`

	admissionEvaluationColumns = `evaluation_id, candidate_id, candidate_revision, candidate_hash,
		evaluation_hash, grader_version, policy_hash, dimensions_json, scripts_json, aggregate,
		risk_tier, referrers_json, baseline_json, evaluated_at, operation_id`

	admissionDecisionColumns = `decision_id, candidate_id, candidate_revision, candidate_hash,
		evaluation_id, evaluation_hash, decision, reason, acknowledged_json, approved_scope_json,
		script_review_json, baseline_json, risk_tier, approver_subject, approver_auth_method,
		approver_source, expires_at, superseded_at, superseded_by, created_at, operation_id`

	admissionPublicationColumns = `candidate_id, decision_id, skill_name, skill_uid, version_id,
		version_no, content_hash, baseline_json, state, published_at, withdrawn_at,
		withdraw_operation_id, withdraw_reason, withdraw_version_id, operation_id`

	// skill_admission_candidates_active_name 是 DDL 里同名部分唯一索引的约束名；
	// 冲突按名字判定，不靠错误文案（pgx 文案不稳定）。
	admissionActiveNameIndex = "skill_admission_candidates_active_name"
)

// AdmissionStore 是准入四表的仓储。
// AdmissionStore is the repository of the four admission tables.
type AdmissionStore struct{ db *DB }

// NewAdmissionStore 绑定一个已打开的写模式库句柄。
// NewAdmissionStore binds the store to an opened writable database handle.
func NewAdmissionStore(db *DB) *AdmissionStore { return &AdmissionStore{db: db} }

// CreateCandidate 写入不可变候选行并把操作置 success。同名活跃候选（部分唯一索引
// 覆盖 proposed / evaluated / approved）冲突 → core.ErrAdmissionActiveCandidateExists；
// **不**重试、**不**覆盖。候选目录已在磁盘冻结，数据库失败时由编排层做补偿删除。
// CreateCandidate freezes the candidate row and closes the operation in one transaction;
// an active same-name conflict maps to ErrAdmissionActiveCandidateExists without retry.
func (s *AdmissionStore) CreateCandidate(ctx context.Context, c core.AdmissionCandidate, operationID string, now time.Time) error {
	if _, err := core.ParseAdmissionID("adm_", c.CandidateID); err != nil {
		return fmt.Errorf("%w: %v", core.ErrAdmissionInvalidInput, err)
	}
	if c.SkillName == "" || c.CandidateHash == "" || c.SourceHash == "" ||
		c.ManifestJSON == "" || c.SourceJSON == "" || c.BaselineJSON == "" {
		return fmt.Errorf("%w: 候选字段不完整", core.ErrAdmissionInvalidInput)
	}
	if operationID == "" {
		return fmt.Errorf("%w: operation_id 不能为空", core.ErrAdmissionInvalidInput)
	}
	if c.State != "" && c.State != core.AdmissionProposed {
		return fmt.Errorf("%w: 新候选只能以 proposed 写入", core.ErrAdmissionInvalidInput)
	}
	if now.IsZero() {
		now = time.Now()
	}

	tx, err := s.db.pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入准入候选 %s: 开启事务: %w", c.CandidateID, err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO skill_admission_candidates
		   (candidate_id, skill_name, state, revision, candidate_hash, source_hash,
		    manifest_json, source_json, baseline_json, proposer_subject, proposer_auth_method,
		    proposer_source, risk_tier, terminal_reason, terminal_operation_id, operation_id,
		    created_at, updated_at)
		 VALUES ($1, $2, 'proposed', 1, $3, $4, $5, $6, $7, $8, $9, $10, 'unknown',
		         NULL, NULL, $11, $12, $12)`,
		c.CandidateID, c.SkillName, c.CandidateHash, c.SourceHash, c.ManifestJSON,
		c.SourceJSON, c.BaselineJSON, c.ProposerSubject, c.ProposerAuthMethod,
		c.ProposerSource, operationID, now.UnixMilli())
	if err != nil {
		if isAdmissionActiveNameConflict(err) {
			return fmt.Errorf("%w: 技能 %s 已有活跃候选", core.ErrAdmissionActiveCandidateExists, c.SkillName)
		}
		return fmt.Errorf("写入准入候选 %s: %w", c.CandidateID, err)
	}
	if err := finishOperationTx(ctx, s.db, tx, operationID, core.OperationSuccess, "", "", nil, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		// 与发布 / 撤回同一口径：提交错误不等于未生效，调用方不得删除冻结目录。
		// Same rule as publish/withdraw: an unknown commit forbids deleting the
		// frozen candidate directory, which may be the only evidence.
		return fmt.Errorf("写入准入候选 %s: 提交结果未知: %w: %w",
			c.CandidateID, core.ErrAdmissionCommitUnknown, err)
	}
	return nil
}

// isAdmissionActiveNameConflict 按 SQLSTATE 23505 + 约束名判定同名活跃候选冲突。
// isAdmissionActiveNameConflict classifies the partial-unique-index conflict by name.
func isAdmissionActiveNameConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolationSQLState {
		return false
	}
	return pgErr.ConstraintName == admissionActiveNameIndex
}

// GetCandidate 按标识取候选；不存在返回 core.ErrAdmissionNotFound。
// GetCandidate fetches one candidate; a miss maps to core.ErrAdmissionNotFound.
func (s *AdmissionStore) GetCandidate(ctx context.Context, candidateID string) (core.AdmissionCandidate, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionCandidateColumns+` FROM skill_admission_candidates WHERE candidate_id = $1`,
		candidateID)
	c, err := scanAdmissionCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionCandidate{}, fmt.Errorf("%w: %s", core.ErrAdmissionNotFound, candidateID)
	}
	if err != nil {
		return core.AdmissionCandidate{}, fmt.Errorf("读取准入候选 %s: %w", candidateID, err)
	}
	return c, nil
}

// ListCandidates 按过滤条件取候选：存储态直接比较；`expired` 翻译为
// `state='approved' AND 活跃批准 expires_at <= now` 的 EXISTS 子查询（§3.1.4 末条）。
// SQL 返回的仍是存储态行，过期只影响筛选，不回写状态。
// ListCandidates filters stored rows; `expired` is translated into an EXISTS predicate
// over the active approval and never rewrites the stored state.
func (s *AdmissionStore) ListCandidates(ctx context.Context, f core.AdmissionFilter) ([]core.AdmissionCandidate, error) {
	switch f.State {
	case "", core.AdmissionProposed, core.AdmissionEvaluated, core.AdmissionApproved,
		core.AdmissionRejected, core.AdmissionCanceled, core.AdmissionPublished,
		core.AdmissionWithdrawn, core.AdmissionExpired:
	default:
		return nil, fmt.Errorf("%w: 未知候选状态 %q", core.ErrAdmissionInvalidInput, f.State)
	}

	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT `+admissionCandidateColumns+`
		   FROM skill_admission_candidates c
		  WHERE (CASE
		           WHEN $1::text = '' THEN true
		           WHEN $1::text = 'expired' THEN
		             c.state = 'approved' AND EXISTS (
		               SELECT 1 FROM skill_admission_decisions d
		                WHERE d.candidate_id = c.candidate_id
		                  AND d.decision = 'approve'
		                  AND d.superseded_at IS NULL
		                  AND d.expires_at <= $2::bigint)
		           ELSE c.state = $1::text
		         END)
		    AND ($3::text = '' OR c.skill_name = $3::text)
		  ORDER BY c.updated_at DESC, c.candidate_id DESC
		  LIMIT $4::bigint`,
		string(f.State), time.Now().UnixMilli(), f.Skill, int64(normalizeAdmissionLimit(f.Limit)))
	if err != nil {
		return nil, fmt.Errorf("读取准入候选列表: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.AdmissionCandidate
	for rows.Next() {
		c, err := scanAdmissionCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("读取准入候选列表: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取准入候选列表: %w", err)
	}
	return out, nil
}

// normalizeAdmissionLimit 把 limit 收进 [1, 200]，缺省 50。
// normalizeAdmissionLimit clamps the limit to [1, 200], defaulting to 50.
func normalizeAdmissionLimit(limit int) int {
	if limit <= 0 {
		return defaultAdmissionListLimit
	}
	if limit > maxAdmissionListLimit {
		return maxAdmissionListLimit
	}
	return limit
}

// InsertEvaluation 在同一事务内：候选 CAS（proposed / evaluated / approved 且修订号
// 匹配）→ 旧活跃批准置 superseded → 插入评估行 → 操作终态。CAS 未命中返回
// core.ErrAdmissionInvalidState。
// InsertEvaluation runs the candidate CAS, supersedes the previous active approval and
// appends the evaluation row in one transaction.
func (s *AdmissionStore) InsertEvaluation(ctx context.Context, e core.AdmissionEvaluation, expectedRevision int64, now time.Time) error {
	if _, err := core.ParseAdmissionID("eval_", e.EvaluationID); err != nil {
		return fmt.Errorf("%w: %v", core.ErrAdmissionInvalidInput, err)
	}
	if e.CandidateID == "" || e.CandidateHash == "" || e.EvaluationHash == "" ||
		e.GraderVersion == "" || e.PolicyHash == "" || e.BaselineJSON == "" {
		return fmt.Errorf("%w: 评估字段不完整", core.ErrAdmissionInvalidInput)
	}
	if e.RiskTier != core.AdmissionRiskNormal && e.RiskTier != core.AdmissionRiskHigh {
		return fmt.Errorf("%w: 评估风险档只能是 normal / high", core.ErrAdmissionInvalidInput)
	}
	if e.OperationID == "" {
		return fmt.Errorf("%w: operation_id 不能为空", core.ErrAdmissionInvalidInput)
	}
	// 证据行绑定的修订号必须就是 CAS 用的期望修订：两者不一致会写出绑定到不存在
	// 修订的证据行，宁可拒绝。
	// The evidence row must bind the same revision the CAS expects.
	if e.CandidateRevision != expectedRevision {
		return fmt.Errorf("%w: 评估的候选修订 %d 与 CAS 期望 %d 不一致",
			core.ErrAdmissionInvalidInput, e.CandidateRevision, expectedRevision)
	}
	if now.IsZero() {
		now = time.Now()
	}
	dimensions, err := marshalAdmissionJSONList(e.Dimensions)
	if err != nil {
		return fmt.Errorf("%w: dimensions: %v", core.ErrAdmissionInvalidInput, err)
	}
	scripts, err := marshalAdmissionJSONList(e.Scripts)
	if err != nil {
		return fmt.Errorf("%w: scripts: %v", core.ErrAdmissionInvalidInput, err)
	}
	referrers, err := marshalAdmissionJSONList(e.Referrers)
	if err != nil {
		return fmt.Errorf("%w: referrers: %v", core.ErrAdmissionInvalidInput, err)
	}
	evaluatedAt := e.EvaluatedAt
	if evaluatedAt.IsZero() {
		evaluatedAt = now
	}

	tx, err := s.db.pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入准入评估 %s: 开启事务: %w", e.EvaluationID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE skill_admission_candidates
		    SET state = 'evaluated', revision = revision + 1, risk_tier = $1, updated_at = $2
		  WHERE candidate_id = $3 AND revision = $4
		    AND state IN ('proposed','evaluated','approved')`,
		string(e.RiskTier), now.UnixMilli(), e.CandidateID, expectedRevision)
	if err != nil {
		return fmt.Errorf("写入准入评估 %s: 更新候选状态: %w", e.EvaluationID, err)
	}
	if err := requireSingleRow(ctx, tx, res, "skill_admission_candidates", e.CandidateID); err != nil {
		return err
	}
	if err := supersedeAdmissionApprovalTx(ctx, tx, e.CandidateID, e.OperationID, now); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO skill_admission_evaluations
		   (evaluation_id, candidate_id, candidate_revision, candidate_hash, evaluation_hash,
		    grader_version, policy_hash, dimensions_json, scripts_json, aggregate, risk_tier,
		    referrers_json, baseline_json, evaluated_at, operation_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		e.EvaluationID, e.CandidateID, e.CandidateRevision, e.CandidateHash,
		e.EvaluationHash, e.GraderVersion, e.PolicyHash, dimensions, scripts,
		string(e.Aggregate), string(e.RiskTier), referrers, e.BaselineJSON,
		evaluatedAt.UnixMilli(), e.OperationID)
	if err != nil {
		return fmt.Errorf("写入准入评估 %s: %w", e.EvaluationID, err)
	}
	if err := finishOperationTx(ctx, s.db, tx, e.OperationID, core.OperationSuccess, "", "", nil, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("写入准入评估 %s: 提交: %w", e.EvaluationID, err)
	}
	return nil
}

// LatestEvaluation 按 seq（插入序）取最新评估——同一毫秒的两次评估也有确定顺序。
// LatestEvaluation orders by identity seq, not by the millisecond timestamp.
func (s *AdmissionStore) LatestEvaluation(ctx context.Context, candidateID string) (core.AdmissionEvaluation, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionEvaluationColumns+`
		   FROM skill_admission_evaluations
		  WHERE candidate_id = $1
		  ORDER BY seq DESC LIMIT 1`, candidateID)
	e, err := scanAdmissionEvaluation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionEvaluation{}, fmt.Errorf("%w: 候选 %s 没有评估", core.ErrAdmissionNotFound, candidateID)
	}
	if err != nil {
		return core.AdmissionEvaluation{}, fmt.Errorf("读取最新准入评估 %s: %w", candidateID, err)
	}
	return e, nil
}

// GetEvaluation 按评估标识取行；不存在返回 core.ErrAdmissionNotFound。
// GetEvaluation fetches one evaluation; a miss maps to core.ErrAdmissionNotFound.
func (s *AdmissionStore) GetEvaluation(ctx context.Context, evaluationID string) (core.AdmissionEvaluation, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionEvaluationColumns+` FROM skill_admission_evaluations WHERE evaluation_id = $1`,
		evaluationID)
	e, err := scanAdmissionEvaluation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionEvaluation{}, fmt.Errorf("%w: %s", core.ErrAdmissionNotFound, evaluationID)
	}
	if err != nil {
		return core.AdmissionEvaluation{}, fmt.Errorf("读取准入评估 %s: %w", evaluationID, err)
	}
	return e, nil
}

// InsertDecision 在同一事务内：旧活跃批准置 superseded → 插入决策行 → 候选状态 CAS
// （approve 要求 evaluated；reject 允许 evaluated / approved）→ 操作终态。reject 把
// 决策理由写入候选 terminal_reason；approve 保持 NULL（DDL 的联动 CHECK）。
// InsertDecision supersedes the previous active approval, appends the decision and runs
// the candidate CAS in one transaction; reject also stamps the terminal reason.
func (s *AdmissionStore) InsertDecision(ctx context.Context, d core.AdmissionDecision, expectedRevision int64, now time.Time) error {
	if _, err := core.ParseAdmissionID("dec_", d.DecisionID); err != nil {
		return fmt.Errorf("%w: %v", core.ErrAdmissionInvalidInput, err)
	}
	if d.CandidateID == "" || d.CandidateHash == "" || d.EvaluationID == "" ||
		d.EvaluationHash == "" || d.OperationID == "" {
		return fmt.Errorf("%w: 决策字段不完整", core.ErrAdmissionInvalidInput)
	}
	if d.RiskTier != core.AdmissionRiskNormal && d.RiskTier != core.AdmissionRiskHigh {
		return fmt.Errorf("%w: 决策风险档只能是 normal / high", core.ErrAdmissionInvalidInput)
	}
	if d.Decision != core.AdmissionApprove && d.Decision != core.AdmissionReject {
		return fmt.Errorf("%w: 决策只能是 approve / reject", core.ErrAdmissionInvalidInput)
	}
	// 同 InsertEvaluation：决策行绑定的修订号必须等于 CAS 的期望修订。
	// Same as InsertEvaluation: the decision must bind the CAS expected revision.
	if d.CandidateRevision != expectedRevision {
		return fmt.Errorf("%w: 决策的候选修订 %d 与 CAS 期望 %d 不一致",
			core.ErrAdmissionInvalidInput, d.CandidateRevision, expectedRevision)
	}
	if now.IsZero() {
		now = time.Now()
	}
	acknowledged, err := marshalAdmissionJSONList(d.Acknowledged)
	if err != nil {
		return fmt.Errorf("%w: acknowledged_dimensions: %v", core.ErrAdmissionInvalidInput, err)
	}

	tx, err := s.db.pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入准入决策 %s: 开启事务: %w", d.DecisionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := supersedeAdmissionApprovalTx(ctx, tx, d.CandidateID, d.OperationID, now); err != nil {
		return err
	}

	// approve / reject 分别写入可空列：DDL 的联动 CHECK 要求 approve 四列非空、
	// reject 四列为 NULL，两套字面量在 SQL 里固定，不由调用方拼。
	var (
		scope, scriptReview, baseline, expires any
	)
	if d.Decision == core.AdmissionApprove {
		if d.ApprovedScopeJSON == "" || d.ScriptReviewJSON == "" || d.BaselineJSON == "" {
			return fmt.Errorf("%w: approve 必须带批准范围、脚本审查与基线", core.ErrAdmissionInvalidInput)
		}
		scope = d.ApprovedScopeJSON
		scriptReview = d.ScriptReviewJSON
		baseline = d.BaselineJSON
		if d.ExpiresAt.IsZero() {
			return fmt.Errorf("%w: approve 必须带 expires_at", core.ErrAdmissionInvalidInput)
		}
		expires = d.ExpiresAt.UnixMilli()
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO skill_admission_decisions
		   (decision_id, candidate_id, candidate_revision, candidate_hash, evaluation_id,
		    evaluation_hash, decision, reason, acknowledged_json, approved_scope_json,
		    script_review_json, baseline_json, risk_tier, approver_subject,
		    approver_auth_method, approver_source, expires_at, superseded_at,
		    superseded_by, created_at, operation_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
		         NULL, NULL, $18, $19)`,
		d.DecisionID, d.CandidateID, d.CandidateRevision, d.CandidateHash, d.EvaluationID,
		d.EvaluationHash, string(d.Decision), d.Reason, acknowledged, scope, scriptReview,
		baseline, string(d.RiskTier), d.ApproverSubject, d.ApproverAuthMethod,
		d.ApproverSource, expires, now.UnixMilli(), d.OperationID)
	if err != nil {
		return fmt.Errorf("写入准入决策 %s: %w", d.DecisionID, err)
	}

	var execRes sql.Result
	if d.Decision == core.AdmissionApprove {
		execRes, err = tx.ExecContext(ctx,
			`UPDATE skill_admission_candidates
			    SET state = 'approved', revision = revision + 1, risk_tier = $1, updated_at = $2
			  WHERE candidate_id = $3 AND revision = $4 AND state = 'evaluated'`,
			string(d.RiskTier), now.UnixMilli(), d.CandidateID, expectedRevision)
	} else {
		execRes, err = tx.ExecContext(ctx,
			`UPDATE skill_admission_candidates
			    SET state = 'rejected', revision = revision + 1, updated_at = $1,
			        terminal_reason = $2, terminal_operation_id = $3
			  WHERE candidate_id = $4 AND revision = $5
			    AND state IN ('evaluated','approved')`,
			now.UnixMilli(), d.Reason, d.OperationID, d.CandidateID, expectedRevision)
	}
	if err != nil {
		return fmt.Errorf("写入准入决策 %s: 更新候选状态: %w", d.DecisionID, err)
	}
	if err := requireSingleRow(ctx, tx, execRes, "skill_admission_candidates", d.CandidateID); err != nil {
		return err
	}
	if err := finishOperationTx(ctx, s.db, tx, d.OperationID, core.OperationSuccess, "", "", nil, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("写入准入决策 %s: 提交: %w", d.DecisionID, err)
	}
	return nil
}

// LatestDecision 按 seq 取最新决策（approve / reject 都算）。
// LatestDecision returns the newest decision by identity seq.
func (s *AdmissionStore) LatestDecision(ctx context.Context, candidateID string) (core.AdmissionDecision, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionDecisionColumns+`
		   FROM skill_admission_decisions
		  WHERE candidate_id = $1
		  ORDER BY seq DESC LIMIT 1`, candidateID)
	d, err := scanAdmissionDecision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 候选 %s 没有决策", core.ErrAdmissionNotFound, candidateID)
	}
	if err != nil {
		return core.AdmissionDecision{}, fmt.Errorf("读取最新准入决策 %s: %w", candidateID, err)
	}
	return d, nil
}

// ActiveApproval 取仍然有效的批准（`superseded_at IS NULL`）；过期与否由编排层按
// `ExpiresAt` 判定，存储层不派生。
// ActiveApproval returns the non-superseded approval; expiry stays an orchestration check.
func (s *AdmissionStore) ActiveApproval(ctx context.Context, candidateID string) (core.AdmissionDecision, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionDecisionColumns+`
		   FROM skill_admission_decisions
		  WHERE candidate_id = $1 AND decision = 'approve' AND superseded_at IS NULL
		  ORDER BY seq DESC LIMIT 1`, candidateID)
	d, err := scanAdmissionDecision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 候选 %s 没有活跃批准", core.ErrAdmissionNotFound, candidateID)
	}
	if err != nil {
		return core.AdmissionDecision{}, fmt.Errorf("读取活跃准入批准 %s: %w", candidateID, err)
	}
	return d, nil
}

// CancelCandidate 在同一事务内把候选置 canceled（只允许 proposed / evaluated /
// approved）、把活跃批准置 superseded、回填操作终态；返回更新后的候选行。CAS 未命中
// 时区分"不存在"与"状态不允许"。
// CancelCandidate moves the candidate to canceled, supersedes the active approval and
// closes the operation in one transaction, returning the updated row.
func (s *AdmissionStore) CancelCandidate(ctx context.Context, candidateID, reason, operationID string, expectedRevision int64, now time.Time) (core.AdmissionCandidate, error) {
	if candidateID == "" || operationID == "" {
		return core.AdmissionCandidate{}, fmt.Errorf("%w: 候选与操作标识不能为空", core.ErrAdmissionInvalidInput)
	}
	// 按字符（而非字节）计数：DDL 用 char_length，编排层也用 RuneCount；三处同一口径。
	// Count characters, not bytes — the DDL uses char_length and the orchestrator counts runes.
	if reason == "" || utf8.RuneCountInString(reason) > 2000 {
		return core.AdmissionCandidate{}, fmt.Errorf("%w: 取消理由必须为 1~2000 字符", core.ErrAdmissionInvalidInput)
	}
	if now.IsZero() {
		now = time.Now()
	}

	tx, err := s.db.pool.BeginTx(ctx, nil)
	if err != nil {
		return core.AdmissionCandidate{}, fmt.Errorf("取消准入候选 %s: 开启事务: %w", candidateID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE skill_admission_candidates
		    SET state = 'canceled', revision = revision + 1, updated_at = $1,
		        terminal_reason = $2, terminal_operation_id = $3
		  WHERE candidate_id = $4 AND revision = $5
		    AND state IN ('proposed','evaluated','approved')`,
		now.UnixMilli(), reason, operationID, candidateID, expectedRevision)
	if err != nil {
		return core.AdmissionCandidate{}, fmt.Errorf("取消准入候选 %s: 更新状态: %w", candidateID, err)
	}
	if err := requireSingleRow(ctx, tx, res, "skill_admission_candidates", candidateID); err != nil {
		return core.AdmissionCandidate{}, err
	}
	if err := supersedeAdmissionApprovalTx(ctx, tx, candidateID, operationID, now); err != nil {
		return core.AdmissionCandidate{}, err
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+admissionCandidateColumns+` FROM skill_admission_candidates WHERE candidate_id = $1`,
		candidateID)
	canceled, err := scanAdmissionCandidate(row)
	if err != nil {
		return core.AdmissionCandidate{}, fmt.Errorf("取消准入候选 %s: 读取结果: %w", candidateID, err)
	}
	if err := finishOperationTx(ctx, s.db, tx, operationID, core.OperationSuccess, "", "", nil, now); err != nil {
		return core.AdmissionCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.AdmissionCandidate{}, fmt.Errorf("取消准入候选 %s: 提交: %w", candidateID, err)
	}
	return canceled, nil
}

// PublishAdmission 是受控发布的**单事务**（§3.1.4 / §2.3.5 第 11 步）：插入版本行 →
// 带基线期望的 publishTx → 插入发布记录 → 候选置 published（CAS）→ 操作终态。任一
// 失败整体回滚，候选保持 approved。
// PublishAdmission is the single publish transaction: version row, expectation-guarded
// pointer UPSERT, publication row, candidate CAS and the operation outcome.
func (s *AdmissionStore) PublishAdmission(ctx context.Context, req core.AdmissionPublishRequest, now time.Time) (core.AdmissionPublication, error) {
	if req.CandidateID == "" || req.DecisionID == "" || req.OperationID == "" {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 发布请求字段不完整", core.ErrAdmissionInvalidInput)
	}
	if req.Baseline.CanonicalJSON == "" {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 发布必须绑定基线", core.ErrAdmissionInvalidInput)
	}
	version, err := normalizeResourceVersion(req.Version)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if version.Kind != core.ResourceSkill {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 准入发布只接受技能版本", core.ErrAdmissionInvalidInput)
	}
	// 版本行的 operation_id 必须指向本事务的预写操作（外键），不依赖调用方是否填了。
	version.OperationID = req.OperationID
	expect, err := admissionBaselineExpectation(req.Baseline)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}

	tx, err := s.db.pool.BeginTx(ctx, nil)
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("发布准入候选 %s: 开启事务: %w", req.CandidateID, err)
	}
	defer func() { _ = tx.Rollback() }()

	skillName, err := admissionCandidateSkillName(ctx, tx, req.CandidateID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if skillName != version.ResourceID {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 版本技能名与候选不一致", core.ErrAdmissionInvalidInput)
	}
	if err := checkDecisionBelongsToCandidate(ctx, tx, req.DecisionID, req.CandidateID); err != nil {
		return core.AdmissionPublication{}, err
	}
	// 一个候选最多一次成功发布：先读发布记录给出领域错误；主键仍在并发下兜底。
	// At most one successful publication per candidate: the pre-read yields the domain
	// sentinel, the primary key still backstops the concurrent case.
	if err := checkCandidateNotPublishedTx(ctx, tx, req.CandidateID); err != nil {
		return core.AdmissionPublication{}, err
	}

	inserted, err := insertVersionTx(ctx, tx, version)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if version.VersionNo > 0 && inserted.VersionNo != version.VersionNo {
		return core.AdmissionPublication{}, fmt.Errorf(
			"%w: 版本号分配为 %d，与存储路径约定的 %d 不一致",
			core.ErrAdmissionInvalidInput, inserted.VersionNo, version.VersionNo)
	}
	if err := publishTx(ctx, tx, core.ResourceSkill, skillName, inserted.ResourceUID,
		inserted.ID, true, req.OperationID, now, expect); err != nil {
		return core.AdmissionPublication{}, err
	}

	publication := core.AdmissionPublication{
		CandidateID:  req.CandidateID,
		DecisionID:   req.DecisionID,
		SkillName:    skillName,
		SkillUID:     inserted.ResourceUID,
		VersionID:    inserted.ID,
		VersionNo:    int64(inserted.VersionNo),
		ContentHash:  version.ContentHash,
		BaselineJSON: req.Baseline.CanonicalJSON,
		State:        core.AdmissionPublicationActive,
		PublishedAt:  now,
		OperationID:  req.OperationID,
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO skill_admission_publications
		   (candidate_id, decision_id, skill_name, skill_uid, version_id, version_no,
		    content_hash, baseline_json, state, published_at, withdrawn_at,
		    withdraw_operation_id, withdraw_reason, withdraw_version_id, operation_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', $9, NULL, NULL, NULL, NULL, $10)`,
		publication.CandidateID, publication.DecisionID, publication.SkillName,
		string(publication.SkillUID), publication.VersionID, publication.VersionNo,
		publication.ContentHash, publication.BaselineJSON, now.UnixMilli(),
		publication.OperationID)
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("发布准入候选 %s: 写入发布记录: %w", req.CandidateID, err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE skill_admission_candidates
		    SET state = 'published', revision = revision + 1, updated_at = $1
		  WHERE candidate_id = $2 AND revision = $3 AND state = 'approved'`,
		now.UnixMilli(), req.CandidateID, req.CandidateRevision)
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("发布准入候选 %s: 更新候选状态: %w", req.CandidateID, err)
	}
	if err := requireSingleRow(ctx, tx, res, "skill_admission_candidates", req.CandidateID); err != nil {
		return core.AdmissionPublication{}, err
	}
	if err := finishOperationTx(ctx, s.db, tx, req.OperationID, core.OperationSuccess, "", "", nil, now); err != nil {
		return core.AdmissionPublication{}, err
	}
	if err := tx.Commit(); err != nil {
		// 提交错误不等于未生效：连接中断时事务可能已经落库。带哨兵上抛，调用方
		// 不得回滚磁盘，只能把操作记 unknown 交给恢复路径查证。
		// A commit error does not prove the transaction rolled back; mark it unknown
		// so no caller compensates on disk.
		return core.AdmissionPublication{}, fmt.Errorf("发布准入候选 %s: 提交结果未知: %w: %w",
			req.CandidateID, core.ErrAdmissionCommitUnknown, err)
	}
	return publication, nil
}

// admissionBaselineExpectation 把决策基线翻译成 publishTx 的条件断言：present /
// deleted 比对 uid、版本、修订号与删除标记；absent 断言没有指针行。
// admissionBaselineExpectation maps the decision baseline onto the pointer expectation.
func admissionBaselineExpectation(b core.AdmissionBaseline) (*pointerExpectation, error) {
	switch b.SkillState {
	case core.AdmissionSkillPresent:
		return &pointerExpectation{
			UID: b.ResourceUID, PublishedVersionID: b.PublishedVersionID,
			Revision: b.PointerRevision, Deleted: false,
			Mismatch: core.ErrAdmissionStaleBaseline,
		}, nil
	case core.AdmissionSkillDeleted:
		return &pointerExpectation{
			UID: b.ResourceUID, PublishedVersionID: b.PublishedVersionID,
			Revision: b.PointerRevision, Deleted: true,
			Mismatch: core.ErrAdmissionStaleBaseline,
		}, nil
	case core.AdmissionSkillAbsent:
		return &pointerExpectation{Absent: true, Mismatch: core.ErrAdmissionStaleBaseline}, nil
	default:
		return nil, fmt.Errorf("%w: 未知基线技能状态 %q", core.ErrAdmissionInvalidInput, b.SkillState)
	}
}

// GetPublication 按候选取发布记录；不存在返回 core.ErrAdmissionNotFound。
// GetPublication fetches the publication of one candidate.
func (s *AdmissionStore) GetPublication(ctx context.Context, candidateID string) (core.AdmissionPublication, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionPublicationColumns+` FROM skill_admission_publications WHERE candidate_id = $1`,
		candidateID)
	p, err := scanAdmissionPublication(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 候选 %s 没有发布记录", core.ErrAdmissionNotFound, candidateID)
	}
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("读取准入发布记录 %s: %w", candidateID, err)
	}
	return p, nil
}

// WithdrawAdmission 在同一事务内：按当前指针重新分类 → 需要时插入新版本并带期望写指针
// → 发布记录置 withdrawn → 候选置 withdrawn（CAS）→ 操作终态。`superseded` / `deleted`
// 两种情况只写撤回记录，不移动指针。
// WithdrawAdmission re-classifies against the live pointer inside the transaction and
// only moves the pointer for restored_previous; superseded/deleted just record it.
func (s *AdmissionStore) WithdrawAdmission(ctx context.Context, req core.AdmissionWithdrawRequest, now time.Time) (core.AdmissionPublication, error) {
	if req.CandidateID == "" || req.OperationID == "" {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 撤回请求字段不完整", core.ErrAdmissionInvalidInput)
	}
	if req.Reason == "" || utf8.RuneCountInString(req.Reason) > 2000 {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 撤回理由必须为 1~2000 字符", core.ErrAdmissionInvalidInput)
	}
	var withdrawVersion *core.ResourceVersion
	if req.WithdrawVersion != nil {
		v, err := normalizeResourceVersion(*req.WithdrawVersion)
		if err != nil {
			return core.AdmissionPublication{}, err
		}
		if v.Kind != core.ResourceSkill {
			return core.AdmissionPublication{}, fmt.Errorf("%w: 撤回版本必须是技能版本", core.ErrAdmissionInvalidInput)
		}
		v.OperationID = req.OperationID
		withdrawVersion = &v
	}
	if now.IsZero() {
		now = time.Now()
	}

	tx, err := s.db.pool.BeginTx(ctx, nil)
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("撤回准入发布 %s: 开启事务: %w", req.CandidateID, err)
	}
	defer func() { _ = tx.Rollback() }()

	skillName, err := admissionCandidateSkillName(ctx, tx, req.CandidateID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	publication, err := admissionPublicationTx(ctx, tx, req.CandidateID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if publication.State != core.AdmissionPublicationActive {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 发布记录已撤回", core.ErrAdmissionInvalidState)
	}
	if req.Publication.VersionID != 0 && req.Publication.VersionID != publication.VersionID {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 发布记录与读取时不一致", core.ErrAdmissionInvalidState)
	}
	if publication.SkillName != skillName {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 发布记录与候选技能名不一致", core.ErrAdmissionInvalidInput)
	}

	expect, withdrawReason, err := classifyAdmissionWithdrawalTx(ctx, tx, publication, withdrawVersion != nil)
	if err != nil {
		return core.AdmissionPublication{}, err
	}

	var withdrawVersionID any
	if withdrawReason == core.AdmissionWithdrawRestoredPrevious {
		inserted, err := insertVersionTx(ctx, tx, *withdrawVersion)
		if err != nil {
			return core.AdmissionPublication{}, err
		}
		if withdrawVersion.VersionNo > 0 && inserted.VersionNo != withdrawVersion.VersionNo {
			return core.AdmissionPublication{}, fmt.Errorf(
				"%w: 版本号分配为 %d，与存储路径约定的 %d 不一致",
				core.ErrAdmissionInvalidInput, inserted.VersionNo, withdrawVersion.VersionNo)
		}
		if err := publishTx(ctx, tx, core.ResourceSkill, publication.SkillName, inserted.ResourceUID,
			inserted.ID, true, req.OperationID, now, expect); err != nil {
			return core.AdmissionPublication{}, err
		}
		withdrawVersionID = inserted.ID
		publication.WithdrawVersionID = inserted.ID
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE skill_admission_publications
		    SET state = 'withdrawn', withdrawn_at = $1, withdraw_operation_id = $2,
		        withdraw_reason = $3, withdraw_version_id = $4
		  WHERE candidate_id = $5 AND state = 'active'`,
		now.UnixMilli(), req.OperationID, withdrawReason, withdrawVersionID, req.CandidateID)
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("撤回准入发布 %s: 更新发布记录: %w", req.CandidateID, err)
	}
	if err := requireSingleRow(ctx, tx, res, "skill_admission_publications", req.CandidateID); err != nil {
		return core.AdmissionPublication{}, err
	}

	res, err = tx.ExecContext(ctx,
		`UPDATE skill_admission_candidates
		    SET state = 'withdrawn', revision = revision + 1, updated_at = $1,
		        terminal_reason = $2, terminal_operation_id = $3
		  WHERE candidate_id = $4 AND revision = $5 AND state = 'published'`,
		now.UnixMilli(), req.Reason, req.OperationID, req.CandidateID, req.CandidateRevision)
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("撤回准入发布 %s: 更新候选状态: %w", req.CandidateID, err)
	}
	if err := requireSingleRow(ctx, tx, res, "skill_admission_candidates", req.CandidateID); err != nil {
		return core.AdmissionPublication{}, err
	}
	if err := finishOperationTx(ctx, s.db, tx, req.OperationID, core.OperationSuccess, "", "", nil, now); err != nil {
		return core.AdmissionPublication{}, err
	}
	if err := tx.Commit(); err != nil {
		// 与发布同一口径：提交结果未知时禁止回滚磁盘。
		// Same rule as publish: an unknown commit forbids on-disk compensation.
		return core.AdmissionPublication{}, fmt.Errorf("撤回准入发布 %s: 提交结果未知: %w: %w",
			req.CandidateID, core.ErrAdmissionCommitUnknown, err)
	}

	publication.State = core.AdmissionPublicationWithdrawn
	publication.WithdrawnAt = now
	publication.WithdrawOperationID = req.OperationID
	publication.WithdrawReason = withdrawReason
	return publication, nil
}

// classifyAdmissionWithdrawalTx 用事务内读到的指针行重新分类（覆盖编排层读后变更）：
// 指针已删 → deleted；仍指向本次发布版本 → restored_previous（必须有已准备的撤回版本）；
// 其余 → superseded。分类与准备的版本不一致时返回 core.ErrAdmissionInvalidState，
// 让调用方重试而不是把错误版本写进指针。
// classifyAdmissionWithdrawalTx re-classifies against the live pointer inside the
// transaction so a stale orchestration read can never move the pointer wrongly.
func classifyAdmissionWithdrawalTx(ctx context.Context, tx *sql.Tx, publication core.AdmissionPublication, hasWithdrawVersion bool) (*pointerExpectation, string, error) {
	var (
		uid              sql.NullString
		publishedVersion sql.NullInt64
		deletedAt        sql.NullInt64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT resource_uid, published_version_id, deleted_at
		   FROM resource_pointers
		  WHERE resource_kind = $1 AND resource_id = $2
		  FOR UPDATE`, string(core.ResourceSkill), publication.SkillName).
		Scan(&uid, &publishedVersion, &deletedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 指针行消失（技能从未发布 / 被清理）：只记录撤回，不移动指针。
		if hasWithdrawVersion {
			return nil, "", fmt.Errorf("%w: 指针行已不存在，不能恢复上一版", core.ErrAdmissionInvalidState)
		}
		return nil, core.AdmissionWithdrawDeleted, nil
	case err != nil:
		return nil, "", fmt.Errorf("撤回准入发布 %s: 读取技能指针: %w", publication.SkillName, err)
	case deletedAt.Valid:
		if hasWithdrawVersion {
			return nil, "", fmt.Errorf("%w: 技能已删除，不能恢复上一版", core.ErrAdmissionInvalidState)
		}
		return nil, core.AdmissionWithdrawDeleted, nil
	case uid.Valid && core.ResourceUID(uid.String) == publication.SkillUID &&
		publishedVersion.Valid && publishedVersion.Int64 == publication.VersionID:
		if !hasWithdrawVersion {
			return nil, "", fmt.Errorf("%w: 指针仍指向本次发布，撤回必须准备上一版", core.ErrAdmissionInvalidState)
		}
		return &pointerExpectation{
			UID: publication.SkillUID, PublishedVersionID: publication.VersionID,
			Deleted: false, Mismatch: core.ErrAdmissionInvalidState,
		}, core.AdmissionWithdrawRestoredPrevious, nil
	default:
		if hasWithdrawVersion {
			return nil, "", fmt.Errorf("%w: 指针已被后续发布超越，不能恢复上一版", core.ErrAdmissionInvalidState)
		}
		return nil, core.AdmissionWithdrawSuperseded, nil
	}
}

// FindEvaluationByOperation 按操作号找评估行（启动恢复查证用）；没有返回
// core.ErrAdmissionNotFound——与"读取失败"区分，恢复据此判 not_applied。
// FindEvaluationByOperation supports crash recovery: a miss is ErrAdmissionNotFound.
func (s *AdmissionStore) FindEvaluationByOperation(ctx context.Context, operationID string) (core.AdmissionEvaluation, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionEvaluationColumns+`
		   FROM skill_admission_evaluations
		  WHERE operation_id = $1
		  ORDER BY seq DESC LIMIT 1`, operationID)
	e, err := scanAdmissionEvaluation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionEvaluation{}, fmt.Errorf("%w: 操作 %s 没有评估", core.ErrAdmissionNotFound, operationID)
	}
	if err != nil {
		return core.AdmissionEvaluation{}, fmt.Errorf("按操作读取准入评估 %s: %w", operationID, err)
	}
	return e, nil
}

// FindDecisionByOperation 按操作号找决策行（启动恢复查证用）。
// FindDecisionByOperation supports crash recovery: a miss is ErrAdmissionNotFound.
func (s *AdmissionStore) FindDecisionByOperation(ctx context.Context, operationID string) (core.AdmissionDecision, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionDecisionColumns+`
		   FROM skill_admission_decisions
		  WHERE operation_id = $1
		  ORDER BY seq DESC LIMIT 1`, operationID)
	d, err := scanAdmissionDecision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 操作 %s 没有决策", core.ErrAdmissionNotFound, operationID)
	}
	if err != nil {
		return core.AdmissionDecision{}, fmt.Errorf("按操作读取准入决策 %s: %w", operationID, err)
	}
	return d, nil
}

// FindPublicationByOperation 按发布操作号找发布记录（启动恢复查证用）。
// FindPublicationByOperation supports publish recovery: a miss is ErrAdmissionNotFound.
func (s *AdmissionStore) FindPublicationByOperation(ctx context.Context, operationID string) (core.AdmissionPublication, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionPublicationColumns+`
		   FROM skill_admission_publications WHERE operation_id = $1`, operationID)
	p, err := scanAdmissionPublication(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 操作 %s 没有发布记录", core.ErrAdmissionNotFound, operationID)
	}
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("按操作读取准入发布 %s: %w", operationID, err)
	}
	return p, nil
}

// FindPublicationByWithdrawOperation 按撤回操作号找发布记录（启动恢复查证用）。
// FindPublicationByWithdrawOperation supports withdraw recovery.
func (s *AdmissionStore) FindPublicationByWithdrawOperation(ctx context.Context, operationID string) (core.AdmissionPublication, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT `+admissionPublicationColumns+`
		   FROM skill_admission_publications WHERE withdraw_operation_id = $1`, operationID)
	p, err := scanAdmissionPublication(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 操作 %s 没有撤回记录", core.ErrAdmissionNotFound, operationID)
	}
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("按操作读取准入撤回 %s: %w", operationID, err)
	}
	return p, nil
}

// supersedeAdmissionApprovalTx 把候选的活跃批准置 superseded：时间取事务时刻，
// `superseded_by` 统一写**取代它的操作号**（重新评估 / 重新批准 / 拒绝 / 取消），
// 不混填决策号（§3.1.4）。
// supersedeAdmissionApprovalTx marks the live approval superseded by the given operation.
func supersedeAdmissionApprovalTx(ctx context.Context, tx *sql.Tx, candidateID, operationID string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE skill_admission_decisions
		    SET superseded_at = $1, superseded_by = $2
		  WHERE candidate_id = $3 AND decision = 'approve' AND superseded_at IS NULL`,
		now.UnixMilli(), operationID, candidateID)
	if err != nil {
		return fmt.Errorf("取代准入批准 %s: %w", candidateID, err)
	}
	return nil
}

// requireSingleRow 要求 CAS 更新恰好命中一行：0 行时读候选当前状态区分"不存在"与
// "状态 / 修订号已变化"；多于一行属于不可能状态，按错误处理。
// requireSingleRow distinguishes a missing candidate from a CAS miss on zero rows.
func requireSingleRow(ctx context.Context, tx *sql.Tx, res sql.Result, table, candidateID string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取 %s 影响行数: %w", table, err)
	}
	if affected == 1 {
		return nil
	}
	if affected > 1 {
		return fmt.Errorf("更新 %s 命中 %d 行，拒绝提交", table, affected)
	}
	if table != "skill_admission_candidates" {
		return fmt.Errorf("%w: %s 没有可更新的行", core.ErrAdmissionInvalidState, candidateID)
	}
	var state string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM skill_admission_candidates WHERE candidate_id = $1`, candidateID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", core.ErrAdmissionNotFound, candidateID)
	}
	if err != nil {
		return fmt.Errorf("读取准入候选 %s 状态: %w", candidateID, err)
	}
	return fmt.Errorf("%w: 候选 %s 当前为 %s，操作未命中 CAS", core.ErrAdmissionInvalidState, candidateID, state)
}

// admissionCandidateSkillName 读取候选技能名；候选不存在返回 core.ErrAdmissionNotFound。
// admissionCandidateSkillName returns the candidate's skill name or ErrAdmissionNotFound.
func admissionCandidateSkillName(ctx context.Context, tx *sql.Tx, candidateID string) (string, error) {
	var skillName string
	err := tx.QueryRowContext(ctx,
		`SELECT skill_name FROM skill_admission_candidates WHERE candidate_id = $1`, candidateID).
		Scan(&skillName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", core.ErrAdmissionNotFound, candidateID)
	}
	if err != nil {
		return "", fmt.Errorf("读取准入候选 %s: %w", candidateID, err)
	}
	return skillName, nil
}

// checkCandidateNotPublishedTx 拒绝同一候选的第二次发布：发布记录主键已经存在即说明
// 有一次成功提交（失败事务不会留下记录）。
// checkCandidateNotPublishedTx refuses a second successful publication of one candidate.
func checkCandidateNotPublishedTx(ctx context.Context, tx *sql.Tx, candidateID string) error {
	var existing string
	err := tx.QueryRowContext(ctx,
		`SELECT candidate_id FROM skill_admission_publications WHERE candidate_id = $1`, candidateID).
		Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取准入发布记录 %s: %w", candidateID, err)
	}
	return fmt.Errorf("%w: 候选 %s 已成功发布", core.ErrAdmissionInvalidState, candidateID)
}

// checkDecisionBelongsToCandidate 确认发布绑定的决策属于同一候选——跨候选的决策号
// 虽然满足外键，但会让发布记录绑定别人的批准。
// checkDecisionBelongsToCandidate rejects cross-candidate decision bindings.
func checkDecisionBelongsToCandidate(ctx context.Context, tx *sql.Tx, decisionID, candidateID string) error {
	var owner string
	err := tx.QueryRowContext(ctx,
		`SELECT candidate_id FROM skill_admission_decisions WHERE decision_id = $1`, decisionID).
		Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 决策 %s", core.ErrAdmissionNotFound, decisionID)
	}
	if err != nil {
		return fmt.Errorf("读取准入决策 %s: %w", decisionID, err)
	}
	if owner != candidateID {
		return fmt.Errorf("%w: 决策 %s 不属于候选 %s", core.ErrAdmissionInvalidInput, decisionID, candidateID)
	}
	return nil
}

// admissionPublicationTx 在事务内读取候选的发布记录。
// admissionPublicationTx reads the publication row inside the transaction.
func admissionPublicationTx(ctx context.Context, tx *sql.Tx, candidateID string) (core.AdmissionPublication, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+admissionPublicationColumns+` FROM skill_admission_publications WHERE candidate_id = $1`,
		candidateID)
	p, err := scanAdmissionPublication(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 候选 %s 没有发布记录", core.ErrAdmissionNotFound, candidateID)
	}
	if err != nil {
		return core.AdmissionPublication{}, fmt.Errorf("读取准入发布记录 %s: %w", candidateID, err)
	}
	return p, nil
}

// marshalAdmissionJSONList 把列表序列化成 JSON 文本；nil 映射为 `[]` 而不是 `null`，
// 让 `scripts_json` / `referrers_json` 的空集在库里有唯一形态。
// marshalAdmissionJSONList serializes a list with nil mapped to [] rather than null.
func marshalAdmissionJSONList[T any](values []T) (string, error) {
	if values == nil {
		values = []T{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// decodeAdmissionJSONList 解析 JSON 文本列；`null` 视为空列表。
// decodeAdmissionJSONList decodes a JSON text column; null becomes an empty list.
func decodeAdmissionJSONList[T any](raw string) ([]T, error) {
	if raw == "" || raw == "null" {
		return []T{}, nil
	}
	var values []T
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	if values == nil {
		values = []T{}
	}
	return values, nil
}

// scanAdmissionCandidate 按 admissionCandidateColumns 的顺序扫描候选行。
// scanAdmissionCandidate scans one candidate row in the shared column order.
func scanAdmissionCandidate(row interface{ Scan(...any) error }) (core.AdmissionCandidate, error) {
	var (
		c                   core.AdmissionCandidate
		state, riskTier     string
		terminalReason      sql.NullString
		terminalOperationID sql.NullString
		createdAt           int64
		updatedAt           int64
	)
	err := row.Scan(&c.CandidateID, &c.SkillName, &state, &c.Revision, &c.CandidateHash,
		&c.SourceHash, &c.ManifestJSON, &c.SourceJSON, &c.BaselineJSON, &c.ProposerSubject,
		&c.ProposerAuthMethod, &c.ProposerSource, &riskTier, &terminalReason,
		&terminalOperationID, &c.OperationID, &createdAt, &updatedAt)
	if err != nil {
		return core.AdmissionCandidate{}, err
	}
	c.State = core.AdmissionState(state)
	c.RiskTier = core.AdmissionRiskTier(riskTier)
	c.TerminalReason = terminalReason.String
	c.TerminalOperationID = terminalOperationID.String
	c.CreatedAt = time.UnixMilli(createdAt).UTC()
	c.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return c, nil
}

// scanAdmissionEvaluation 按 admissionEvaluationColumns 的顺序扫描评估行。
// scanAdmissionEvaluation scans one evaluation row in the shared column order.
func scanAdmissionEvaluation(row interface{ Scan(...any) error }) (core.AdmissionEvaluation, error) {
	var (
		e                                    core.AdmissionEvaluation
		dimensionsJSON, scriptsJSON          string
		referrersJSON, aggregate, riskTierSz string
		evaluatedAt                          int64
	)
	err := row.Scan(&e.EvaluationID, &e.CandidateID, &e.CandidateRevision, &e.CandidateHash,
		&e.EvaluationHash, &e.GraderVersion, &e.PolicyHash, &dimensionsJSON, &scriptsJSON,
		&aggregate, &riskTierSz, &referrersJSON, &e.BaselineJSON, &evaluatedAt, &e.OperationID)
	if err != nil {
		return core.AdmissionEvaluation{}, err
	}
	if e.Dimensions, err = decodeAdmissionJSONList[core.AdmissionDimension](dimensionsJSON); err != nil {
		return core.AdmissionEvaluation{}, fmt.Errorf("解析评估维度: %w", err)
	}
	if e.Scripts, err = decodeAdmissionJSONList[string](scriptsJSON); err != nil {
		return core.AdmissionEvaluation{}, fmt.Errorf("解析评估脚本清单: %w", err)
	}
	if e.Referrers, err = decodeAdmissionJSONList[string](referrersJSON); err != nil {
		return core.AdmissionEvaluation{}, fmt.Errorf("解析评估引用名单: %w", err)
	}
	e.Aggregate = core.AdmissionAggregate(aggregate)
	e.RiskTier = core.AdmissionRiskTier(riskTierSz)
	e.EvaluatedAt = time.UnixMilli(evaluatedAt).UTC()
	return e, nil
}

// scanAdmissionDecision 按 admissionDecisionColumns 的顺序扫描决策行。
// scanAdmissionDecision scans one decision row in the shared column order.
func scanAdmissionDecision(row interface{ Scan(...any) error }) (core.AdmissionDecision, error) {
	var (
		d                 core.AdmissionDecision
		kind, riskTier    string
		acknowledgedJSON  string
		approvedScopeJSON sql.NullString
		scriptReviewJSON  sql.NullString
		baselineJSON      sql.NullString
		expiresAt         sql.NullInt64
		supersededAt      sql.NullInt64
		supersededBy      sql.NullString
		createdAt         int64
	)
	err := row.Scan(&d.DecisionID, &d.CandidateID, &d.CandidateRevision, &d.CandidateHash,
		&d.EvaluationID, &d.EvaluationHash, &kind, &d.Reason, &acknowledgedJSON,
		&approvedScopeJSON, &scriptReviewJSON, &baselineJSON, &riskTier, &d.ApproverSubject,
		&d.ApproverAuthMethod, &d.ApproverSource, &expiresAt, &supersededAt, &supersededBy,
		&createdAt, &d.OperationID)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	if d.Acknowledged, err = decodeAdmissionJSONList[core.AdmissionDimensionName](acknowledgedJSON); err != nil {
		return core.AdmissionDecision{}, fmt.Errorf("解析确认维度: %w", err)
	}
	d.Decision = core.AdmissionDecisionKind(kind)
	d.ApprovedScopeJSON = approvedScopeJSON.String
	d.ScriptReviewJSON = scriptReviewJSON.String
	d.BaselineJSON = baselineJSON.String
	d.RiskTier = core.AdmissionRiskTier(riskTier)
	if expiresAt.Valid {
		d.ExpiresAt = time.UnixMilli(expiresAt.Int64).UTC()
	}
	if supersededAt.Valid {
		d.SupersededAt = time.UnixMilli(supersededAt.Int64).UTC()
	}
	d.SupersededBy = supersededBy.String
	d.CreatedAt = time.UnixMilli(createdAt).UTC()
	return d, nil
}

// scanAdmissionPublication 按 admissionPublicationColumns 的顺序扫描发布记录。
// scanAdmissionPublication scans one publication row in the shared column order.
func scanAdmissionPublication(row interface{ Scan(...any) error }) (core.AdmissionPublication, error) {
	var (
		p                   core.AdmissionPublication
		skillUID, state     string
		withdrawnAt         sql.NullInt64
		withdrawOperationID sql.NullString
		withdrawReason      sql.NullString
		withdrawVersionID   sql.NullInt64
		publishedAt         int64
	)
	err := row.Scan(&p.CandidateID, &p.DecisionID, &p.SkillName, &skillUID, &p.VersionID,
		&p.VersionNo, &p.ContentHash, &p.BaselineJSON, &state, &publishedAt, &withdrawnAt,
		&withdrawOperationID, &withdrawReason, &withdrawVersionID, &p.OperationID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	p.SkillUID = core.ResourceUID(skillUID)
	p.State = core.AdmissionPublicationState(state)
	p.PublishedAt = time.UnixMilli(publishedAt).UTC()
	if withdrawnAt.Valid {
		p.WithdrawnAt = time.UnixMilli(withdrawnAt.Int64).UTC()
	}
	p.WithdrawOperationID = withdrawOperationID.String
	p.WithdrawReason = withdrawReason.String
	if withdrawVersionID.Valid {
		p.WithdrawVersionID = withdrawVersionID.Int64
	}
	return p, nil
}
