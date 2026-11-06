package app

// 本文件是 US-21 学习准入的编排面（颗粒度文档 §3.1.5、§2.3.2~§2.3.4、T011 / T013）：
//
//	Propose  —— 校验身份与来源、冻结候选目录、单事务写候选行与操作终态；
//	Evaluate —— 从磁盘重读候选并复算哈希，七维打分后单事务写评估行与候选状态；
//	Approve  —— 在本机可信身份下重读磁盘、校验绑定 / 基线 / 策略 / 风险档 / 保留期 /
//	            有效期 / 确认边界 / 高风险分离，再单事务写决策行与候选状态；
//	Reject   —— 同一身份与绑定校验，写拒绝决策并把理由落到候选终态；
//	Cancel   —— 预发布阶段关掉候选，取代活跃批准；
//	Publish  —— 重读磁盘与基线后复用既有工作副本 / 快照 / 发布事务原语，失败补偿
//	            工作副本与快照，候选保持 approved 供重新复核后重试；
//	Withdraw —— 按当前指针分三类撤回：仍指向本次发布版本时把上一版内容作为新版本
//	            重新发布，被超越 / 已删除只记撤回，首版拒绝撤回到空；
//	List/Get —— 只读查询，管理台与 CLI show 共用同一详情形态。
//
// 三条纪律：
//   - **审计预写先于变更**：七个写动作都先写 `pending` 管理操作（宪法第 5 条），
//     预写失败即拒绝执行；磁盘成功而数据库失败时记 `unknown` 并**保留**孤儿目录
//     供人工检查，不在失败路径上删除证据（C-16 / C-19）；
//   - **候选不可变**：评估从 `.basalt/admissions/candidates/<id>/` 重读并复算内容
//     哈希与来源哈希，与候选行不一致即拒绝，不把行里的旧字节当新证据；
//   - **身份在状态读取之前**：写方法第一步调用 `checkTrustedActor`，先于任何
//     `GetCandidate` / 写入；身份只来自 CLI 端口的 OS 用户，不读请求字段。
//
// This file orchestrates propose / evaluate / list / get. Every write pre-writes a
// pending management operation before mutating anything; a database failure after the
// candidate directory is frozen finishes the operation as unknown and keeps the orphan.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// admissionDiffMaxBytes 是 `show --diff` 的输出上限（颗粒度文档 §2.4：1 MiB，
// 超出截断并标注）。截断只影响展示，不影响任何判定。
// admissionDiffMaxBytes caps the show diff output; truncation never affects verdicts.
const admissionDiffMaxBytes = 1 << 20

// admissionDiffLCSCells 是行级 LCS 的动态规划上限（单元格数）。超过时退化为
// "整段替换"的保守输出——仍然如实反映两份内容，只是不做最小编辑距离。
// admissionDiffLCSCells bounds the line-LCS table; larger inputs fall back to a whole
// block replacement, which stays honest but is not minimal.
const admissionDiffLCSCells = 4_000_000

// AdmissionAdminDeps 是准入编排面的装配输入。
// LockResource 必须来自与手工技能写相同的 *ResourceAdmin 实例，否则准入与手工会
// 各拿一张锁表（颗粒度文档 §3.1.5）。
// AdmissionAdminDeps wires the admission orchestration surface; LockResource must come
// from the same ResourceAdmin instance used by manual skill writes.
type AdmissionAdminDeps struct {
	Root         string
	Store        *postgres.AdmissionStore
	Resources    *postgres.ResourceStore
	Operations   core.OperationStore
	Gate         *core.ResourceGate
	LockResource func(kind core.ResourceKind, id string) func()
	References   func(skillName string) ([]string, error)
	Logger       *slog.Logger
	Now          func() time.Time
}

// AdmissionAdmin 是七个写动作与只读查询的编排器；写方法要求调用方已在实例锁内
// （CLI 端口层负责），进程内再按技能取互斥锁串行。
// AdmissionAdmin orchestrates the seven write actions and read-only queries.
type AdmissionAdmin struct {
	root         string
	store        *postgres.AdmissionStore
	resources    *postgres.ResourceStore
	operations   core.OperationStore
	gate         *core.ResourceGate
	lockResource func(kind core.ResourceKind, id string) func()
	references   func(skillName string) ([]string, error)
	logger       *slog.Logger
	now          func() time.Time

	// 测试注入点：默认绑定真实仓储方法，用例只替换失败注入的那一个，不改变
	// 生产行为。The hooks default to the real store methods; tests replace one to
	// inject a failure without changing production behaviour.
	createCandidate   func(ctx context.Context, c core.AdmissionCandidate, operationID string, now time.Time) error
	insertEvaluation  func(ctx context.Context, e core.AdmissionEvaluation, expectedRevision int64, now time.Time) error
	publishAdmission  func(ctx context.Context, req core.AdmissionPublishRequest, now time.Time) (core.AdmissionPublication, error)
	withdrawAdmission func(ctx context.Context, req core.AdmissionWithdrawRequest, now time.Time) (core.AdmissionPublication, error)
	removeSnapshot    func(root, name string, uid core.ResourceUID, versionNo int) error
	writeCandidate    func(root string, c core.AdmissionCandidate, files []core.AuthorFile, sourceJSON []byte) error
	writeWorkCopy     func(root string, cand workspace.SkillCandidate) error
}

// NewAdmissionAdmin 组装准入编排面；Gate / Logger / Now 为 nil 时回落安全缺省值。
// LockResource 缺失时写方法在取锁前失败关闭——静默退化为自有锁会破坏与手工技能
// 发布之间的互斥（见 lockCandidate）。
// NewAdmissionAdmin assembles the orchestrator; a missing LockResource makes write
// actions fail closed instead of silently degrading to a private lock table.
func NewAdmissionAdmin(deps AdmissionAdminDeps) *AdmissionAdmin {
	gate := deps.Gate
	if gate == nil {
		gate = &core.ResourceGate{}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	admin := &AdmissionAdmin{
		root:           deps.Root,
		store:          deps.Store,
		resources:      deps.Resources,
		operations:     deps.Operations,
		gate:           gate,
		lockResource:   deps.LockResource,
		references:     deps.References,
		logger:         logger,
		now:            now,
		removeSnapshot: removeSkillSnapshot,
		writeCandidate: workspace.WriteAdmissionCandidate,
		writeWorkCopy:  workspace.WriteSkillWorkCopy,
	}
	if deps.Store != nil {
		admin.createCandidate = deps.Store.CreateCandidate
		admin.insertEvaluation = deps.Store.InsertEvaluation
		admin.publishAdmission = deps.Store.PublishAdmission
		admin.withdrawAdmission = deps.Store.WithdrawAdmission
	}
	return admin
}

// checkTrustedActor 校验完整可信身份：主体必须是 `cli:<非空>` 且不是 `cli:unknown`，
// Method 与 Source 都必须是 `cli`。七个写方法都先调用它（纵深防御；数据库 CHECK 是
// 另一道）。Approve 把失败映射为 ErrAdmissionUnknownApprover，其余写方法返回
// ErrAdmissionUntrustedActor。
// checkTrustedActor validates the full trusted local identity before any state read.
func checkTrustedActor(actor core.Actor) error {
	if actor.Method != core.AuthMethodCLI || actor.Source != core.AuthMethodCLI {
		return core.ErrAdmissionUntrustedActor
	}
	suffix, ok := strings.CutPrefix(actor.Subject, "cli:")
	if !ok || suffix == "" || suffix == "unknown" {
		return core.ErrAdmissionUntrustedActor
	}
	return nil
}

// baseline 读取技能当前指针与引用名单并冻结为评估 / 批准 / 发布共用的基线。
// baseline freezes the skill pointer plus referrers shared by evaluate/approve/publish.
func (a *AdmissionAdmin) baseline(ctx context.Context, skillName string) (core.AdmissionBaseline, error) {
	return BuildAdmissionBaseline(ctx, a.resources, a.references, skillName)
}

// lockCandidate 先读候选拿技能名，再按技能取与手工写共用的进程内互斥锁，取锁后
// 重读候选——调用期间候选可能已被别的写动作推进，旧行不能继续用。
// lockCandidate serialises by skill name on the lock table shared with manual writes,
// then re-reads the row so the caller never acts on a stale revision.
func (a *AdmissionAdmin) lockCandidate(ctx context.Context, candidateID string) (core.AdmissionCandidate, func(), error) {
	if a.lockResource == nil {
		return core.AdmissionCandidate{}, nil, fmt.Errorf("%w: 未装配 LockResource，拒绝准入写动作",
			core.ErrAdmissionInvalidInput)
	}
	first, err := a.store.GetCandidate(ctx, candidateID)
	if err != nil {
		return core.AdmissionCandidate{}, nil, err
	}
	unlock := a.lockResource(core.ResourceSkill, first.SkillName)
	current, err := a.store.GetCandidate(ctx, candidateID)
	if err != nil {
		unlock()
		return core.AdmissionCandidate{}, nil, err
	}
	return current, unlock, nil
}

// finishFailed 把操作回填为 failed 并原样返回失败原因；终态写入失败只记日志，
// 留待启动恢复查证（与既有资源写面同一口径）。
// finishFailed closes the operation as failed and returns the original cause.
func (a *AdmissionAdmin) finishFailed(ctx context.Context, operationID string, cause error) error {
	a.logger.Warn("准入动作失败，回填 failed", "operation_id", operationID, "error", cause)
	if err := a.operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		core.OperationFailed, lifecycleResultInternal, admissionResultMessage(cause), nil, a.now()); err != nil {
		a.logger.Error("准入操作终态回填失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(core.OperationFailed), "error", err)
	}
	return cause
}

// finishUnknown 用于"外部结果不确定"的失败：磁盘已写、数据库未确认且补偿也失败，
// 操作记 unknown 且不自动清理孤儿目录。恢复只查证、不重放（C-16）。
// finishUnknown records an indeterminate outcome and never cleans up the orphan.
func (a *AdmissionAdmin) finishUnknown(ctx context.Context, operationID string, cause error) error {
	a.logger.Error("准入动作结果不确定，回填 unknown", "operation_id", operationID, "error", cause)
	if err := a.operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		core.OperationUnknown, lifecycleResultInternal, admissionResultMessage(cause), nil, a.now()); err != nil {
		a.logger.Error("准入 unknown 终态回填失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(core.OperationUnknown), "error", err)
	}
	return cause
}

// admissionResultMessage 把失败原因收成有界文本；数据库列本身另有 4 KB 截断，
// 这里只防止把任意长的嵌套错误带进审计面。
// admissionResultMessage bounds the message written into the audit row.
func admissionResultMessage(cause error) string {
	if cause == nil {
		return ""
	}
	const maxRunes = 2000
	message := cause.Error()
	runes := []rune(message)
	if len(runes) <= maxRunes {
		return message
	}
	return string(runes[:maxRunes])
}

// Propose 校验身份、候选文件集与来源声明，冻结候选目录，再单事务写候选行与操作
// 终态。重复活跃候选由仓储的唯一索引拒绝；此时磁盘上**不**保留本次候选目录。
// Propose validates identity, files and source, freezes the candidate directory, then
// writes the row and the operation terminal in one transaction.
func (a *AdmissionAdmin) Propose(ctx context.Context, actor core.Actor, req core.AdmissionProposeRequest) (core.AdmissionCandidate, error) {
	if err := checkTrustedActor(actor); err != nil {
		return core.AdmissionCandidate{}, core.ErrAdmissionUntrustedActor
	}
	cand, err := workspace.ValidateSkillDraft(req.SkillName, req.Files)
	if err != nil {
		return core.AdmissionCandidate{}, err
	}
	source, err := core.ParseAdmissionSource(req.SourceJSON)
	if err != nil {
		return core.AdmissionCandidate{}, err
	}
	if core.SourceHasSecret(source) || len(core.ScanFilesForSecrets(req.Files)) > 0 {
		return core.AdmissionCandidate{}, core.ErrAdmissionSecretDetected
	}
	baseline, err := a.baseline(ctx, req.SkillName)
	if err != nil {
		return core.AdmissionCandidate{}, err
	}
	candidateID, err := core.NewAdmissionCandidateID()
	if err != nil {
		return core.AdmissionCandidate{}, err
	}
	now := a.now()
	operationID := core.NewOperationID(now)
	candidate := core.AdmissionCandidate{
		CandidateID:        candidateID,
		SkillName:          req.SkillName,
		State:              core.AdmissionProposed,
		Revision:           1,
		CandidateHash:      cand.ContentHash,
		SourceHash:         core.AdmissionSHA256Hex(req.SourceJSON),
		ManifestJSON:       cand.ManifestJSON,
		SourceJSON:         string(req.SourceJSON),
		BaselineJSON:       baseline.CanonicalJSON,
		ProposerSubject:    actor.Subject,
		ProposerAuthMethod: actor.Method,
		ProposerSource:     actor.Source,
		OperationID:        operationID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionSkillAdmissionPropose,
		TargetKind:   core.TargetKindSkillAdmission,
		TargetID:     candidateID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  core.AdmissionRequestHash(req.SkillName, cand.ContentHash),
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := a.operations.BeginOperation(ctx, record); err != nil {
		a.logger.Error("准入提议预写操作失败，拒绝执行", "skill", req.SkillName, "error", err)
		return core.AdmissionCandidate{}, fmt.Errorf("预写管理操作失败: %w", err)
	}
	if err := a.writeCandidate(a.root, candidate, req.Files, req.SourceJSON); err != nil {
		// 冻结目录可能已经就位（置只读或关根目录失败）：删目录会毁证据、建行会伪造
		// 事实，按「行不存在但目录存在」记 unknown 交恢复查证；目录确实不存在才是
		// 确定失败（US-21 review ② F-02，契约「恢复」表 propose 行的第三态）。
		// A frozen directory that already exists is half a write: unknown, not failed.
		if workspace.AdmissionCandidateDirExists(a.root, candidateID) {
			return core.AdmissionCandidate{}, a.finishUnknown(ctx, operationID, err)
		}
		return core.AdmissionCandidate{}, a.finishFailed(ctx, operationID, err)
	}
	if err := a.createCandidate(ctx, candidate, operationID, now); err != nil {
		// 提交结果未知：候选行可能已经落库，冻结目录是唯一证据，禁止删除。
		// 只把操作记 unknown 交恢复查证（与发布 / 撤回同一口径）。
		// An unknown commit may have landed, so the frozen directory is kept and
		// only the operation is recorded as unknown for recovery to verify.
		if errors.Is(err, core.ErrAdmissionCommitUnknown) {
			return core.AdmissionCandidate{}, a.finishUnknown(ctx, operationID, err)
		}
		if removeErr := workspace.RemoveAdmissionCandidate(a.root, candidateID); removeErr != nil {
			// 目录已成孤儿：保留它供人工检查，操作记 unknown，不在这里"修复"证据。
			return core.AdmissionCandidate{}, a.finishUnknown(ctx, operationID, errors.Join(err, removeErr))
		}
		return core.AdmissionCandidate{}, a.finishFailed(ctx, operationID, err)
	}
	return candidate, nil
}

// Evaluate 从磁盘重读候选、复算内容与来源哈希，再跑七维评估；评估行、候选状态与
// 操作终态由仓储在同一事务写入，失败时候选状态不变且没有评估行。
// Evaluate re-reads the frozen candidate, recomputes both hashes and runs the fixed
// grader; the store writes the evaluation, candidate state and terminal atomically.
func (a *AdmissionAdmin) Evaluate(ctx context.Context, actor core.Actor, candidateID string, policy core.AdmissionPolicy) (core.AdmissionEvaluation, error) {
	if err := checkTrustedActor(actor); err != nil {
		return core.AdmissionEvaluation{}, core.ErrAdmissionUntrustedActor
	}
	candidate, unlock, err := a.lockCandidate(ctx, candidateID)
	if err != nil {
		return core.AdmissionEvaluation{}, err
	}
	defer unlock()
	if !candidate.State.CanEvaluate() {
		return core.AdmissionEvaluation{}, fmt.Errorf("%w: 候选状态 %s 不允许评估",
			core.ErrAdmissionInvalidState, candidate.State)
	}
	stored, files, sourceJSON, err := workspace.ReadAdmissionCandidate(a.root, candidateID)
	if err != nil {
		return core.AdmissionEvaluation{}, err
	}
	if stored.CandidateHash != candidate.CandidateHash {
		return core.AdmissionEvaluation{}, fmt.Errorf("%w: 磁盘内容哈希与候选行不一致",
			core.ErrAdmissionCandidateMismatch)
	}
	if core.AdmissionSHA256Hex(sourceJSON) != candidate.SourceHash {
		return core.AdmissionEvaluation{}, fmt.Errorf("%w: 磁盘来源哈希与候选行不一致",
			core.ErrAdmissionSourceMismatch)
	}
	source, err := core.ParseAdmissionSource(sourceJSON)
	if err != nil {
		return core.AdmissionEvaluation{}, err
	}
	baseline, err := a.baseline(ctx, candidate.SkillName)
	if err != nil {
		return core.AdmissionEvaluation{}, err
	}
	evaluationID, err := core.NewAdmissionEvaluationID()
	if err != nil {
		return core.AdmissionEvaluation{}, err
	}
	now := a.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionSkillAdmissionEvaluate,
		TargetKind:   core.TargetKindSkillAdmission,
		TargetID:     candidateID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  core.AdmissionRequestHash(candidateID, candidate.CandidateHash),
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := a.operations.BeginOperation(ctx, record); err != nil {
		a.logger.Error("准入评估预写操作失败，拒绝执行", "candidate", candidateID, "error", err)
		return core.AdmissionEvaluation{}, fmt.Errorf("预写管理操作失败: %w", err)
	}
	evaluation, err := EvaluateAdmission(candidate, files, source, policy, baseline, now)
	if err != nil {
		return core.AdmissionEvaluation{}, a.finishFailed(ctx, operationID, err)
	}
	// 身份、时间与哈希在预写之后补齐：前面的判定面对同一输入可复算，后面的绑定
	// 值才把这次评估固定到具体候选修订与操作号上。
	evaluation.EvaluationID = evaluationID
	evaluation.CandidateID = candidate.CandidateID
	evaluation.CandidateRevision = candidate.Revision
	evaluation.CandidateHash = candidate.CandidateHash
	evaluation.OperationID = operationID
	evaluation.EvaluatedAt = now
	evaluation.EvaluationHash = core.AdmissionEvaluationHash(evaluation)
	if err := a.insertEvaluation(ctx, evaluation, candidate.Revision, now); err != nil {
		return core.AdmissionEvaluation{}, a.finishFailed(ctx, operationID, err)
	}
	return evaluation, nil
}

// admissionConfirmableOrder 是确认集合的固定白名单与顺序：只有效果、成本与既有技能
// 行为回归三类未知可由批准人逐项接受。其余维度无论 passed / failed / not_evaluable
// 都不得进入确认集合（C-04）。
// admissionConfirmableOrder is the fixed acknowledgement whitelist and order; every
// other dimension is either blocked or outside the acknowledgement surface.
var admissionConfirmableOrder = []core.AdmissionDimensionName{
	core.AdmissionDimensionEffect,
	core.AdmissionDimensionCost,
	core.AdmissionDimensionRegression,
}

// admissionScriptEvidenceMaxRunes 与决策解析器的 evidence 上限一致（2000 字符）；
// 这里再判一次，覆盖直接构造 AdmissionDecisionInput 的调用方。
// admissionScriptEvidenceMaxRunes mirrors the parser's evidence bound for direct callers.
const admissionScriptEvidenceMaxRunes = 2000

// admissionReasonMaxRunes 是取消理由的上限（模块卡 §3.3：理由 2000 字符，与决策理由
// 同一口径）；理由会写进候选终态与审计摘要，超限即拒绝而不是截断。
// admissionReasonMaxRunes bounds the cancel reason, matching the decision reason limit.
const admissionReasonMaxRunes = 2000

// admissionAcknowledgeableSet 返回可确认集合：状态为 not_evaluable 且属于固定白名单
// 的维度。批准人必须精确勾选这个集合，多认、少认与越界都拒绝。
// admissionAcknowledgeableSet returns the exact set the approver must acknowledge.
func admissionAcknowledgeableSet(e core.AdmissionEvaluation) []core.AdmissionDimensionName {
	statuses := make(map[core.AdmissionDimensionName]core.AdmissionDimensionStatus, len(e.Dimensions))
	for _, dimension := range e.Dimensions {
		statuses[dimension.Name] = dimension.Status
	}
	out := make([]core.AdmissionDimensionName, 0, len(admissionConfirmableOrder))
	for _, name := range admissionConfirmableOrder {
		if statuses[name] == core.AdmissionDimensionNotEvaluable {
			out = append(out, name)
		}
	}
	return out
}

// admissionBlockedDimensions 返回阻止批准的维度：所有 failed 维度，以及不在白名单
// 内的 not_evaluable 维度。唯一例外是 permissions 不可评估且评估列出了脚本——那一
// 维的未知由 checkScriptReview 逐条静态审查设闸，不进入阻止集合，否则脚本审查永远
// 不可达；permissions=failed 仍然是阻止维度。
// admissionBlockedDimensions lists every dimension that forbids approval; the single
// exception is permissions=not_evaluable with scripts, which the script review gates.
func admissionBlockedDimensions(e core.AdmissionEvaluation) []core.AdmissionDimensionName {
	blocked := make([]core.AdmissionDimensionName, 0, len(e.Dimensions))
	for _, dimension := range e.Dimensions {
		switch dimension.Status {
		case core.AdmissionDimensionFailed:
			blocked = append(blocked, dimension.Name)
		case core.AdmissionDimensionNotEvaluable:
			if dimension.Name == core.AdmissionDimensionPermissions && len(e.Scripts) > 0 {
				continue
			}
			if slices.Contains(admissionConfirmableOrder, dimension.Name) {
				continue
			}
			blocked = append(blocked, dimension.Name)
		}
	}
	return blocked
}

// checkScriptReview 要求人工静态审查逐条覆盖评估列出的全部脚本：路径集合完全相等、
// 每条证据非空且不超限、路径都在批准范围的文件集内。评估无脚本时 script_review 必须
// 是空集（对应 DDL 的 approve 非 NULL 约束）。仅勾选 permissions 不能代替本审查。
// checkScriptReview requires one non-empty evidence entry per evaluated script, all
// inside the approved file set; an empty set is only valid when no script was listed.
func checkScriptReview(e core.AdmissionEvaluation, in core.AdmissionDecisionInput) error {
	scripts := make([]string, len(e.Scripts))
	copy(scripts, e.Scripts)
	slices.Sort(scripts)

	if len(in.ScriptReview) == 0 {
		if len(scripts) == 0 {
			return nil
		}
		return fmt.Errorf("%w: 评估列出 %d 个脚本，但没有静态审查证据",
			core.ErrAdmissionScriptReviewRequired, len(scripts))
	}
	// 先判空再取 len：批准范围缺失时审查路径无处可查，必须走拒绝而不是解引用 panic。
	// Guard the nil scope before len so a malformed decision is refused, never panics.
	allowed := make(map[string]struct{})
	if in.ApprovedScope != nil {
		allowed = make(map[string]struct{}, len(in.ApprovedScope.Files))
		for _, file := range in.ApprovedScope.Files {
			allowed[file.Path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(in.ScriptReview))
	for _, review := range in.ScriptReview {
		if review.Path == "" || review.Evidence == "" {
			return fmt.Errorf("%w: 脚本审查条目缺少路径或证据", core.ErrAdmissionScriptReviewRequired)
		}
		if utf8.RuneCountInString(review.Evidence) > admissionScriptEvidenceMaxRunes {
			return fmt.Errorf("%w: 脚本审查证据超过 %d 字符",
				core.ErrAdmissionScriptReviewRequired, admissionScriptEvidenceMaxRunes)
		}
		if _, ok := allowed[review.Path]; !ok {
			return fmt.Errorf("%w: 脚本审查路径 %s 不在批准范围内",
				core.ErrAdmissionScriptReviewRequired, review.Path)
		}
		paths = append(paths, review.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(scripts, paths) {
		return fmt.Errorf("%w: 脚本审查未逐条覆盖评估列出的脚本",
			core.ErrAdmissionScriptReviewRequired)
	}
	return nil
}

// checkAdmissionApprovalBoundary 按固定顺序执行三项批准边界：① 确认集合与可接受集合
// 精确相等；② 阻止维度为空；③ 每次都执行脚本静态审查。前一步失败即返回，不再执行
// 后一步——越界勾选不会因为"后面还有更深的检查"被静默放过。
// checkAdmissionApprovalBoundary runs the three boundary checks in their fixed order and
// stops at the first failure.
func checkAdmissionApprovalBoundary(e core.AdmissionEvaluation, in core.AdmissionDecisionInput) error {
	expected := admissionAcknowledgeableSet(e)
	actual := make([]core.AdmissionDimensionName, len(in.Acknowledged))
	copy(actual, in.Acknowledged)
	slices.Sort(expected)
	slices.Sort(actual)
	if !slices.Equal(expected, actual) {
		return fmt.Errorf("%w: acknowledged_dimensions 必须精确等于可接受集合 %v",
			core.ErrAdmissionAcknowledgmentMismatch, expected)
	}
	if blocked := admissionBlockedDimensions(e); len(blocked) > 0 {
		return fmt.Errorf("%w: 阻止维度不可确认: %v", core.ErrAdmissionDimensionBlocked, blocked)
	}
	return checkScriptReview(e, in)
}

// checkApprovalSeparation 只对 high 风险要求批准人的 Subject 不同于提出人；Method 与
// Source 已由 checkTrustedActor 固定为 cli，不参与比较。normal 风险不额外限制，
// 发布也不要求发布人 ≠ 批准人。
// checkApprovalSeparation enforces proposer ≠ approver for high-risk candidates only.
func checkApprovalSeparation(c core.AdmissionCandidate, e core.AdmissionEvaluation, actor core.Actor) error {
	if e.RiskTier != core.AdmissionRiskHigh {
		return nil
	}
	if actor.Subject == c.ProposerSubject {
		return fmt.Errorf("%w: 高风险候选必须由不同的本机身份批准",
			core.ErrAdmissionSeparationRequired)
	}
	return nil
}

// bindAdmissionDecision 逐字校验决策文件与候选 / 评估行的绑定关系：候选标识、修订与
// 内容哈希，评估标识与哈希，批准范围的文件集与引用名单，以及决策携带的基线。任何
// 一项不符返回对应哨兵，不静默修正。reject 决策不带批准范围，绑定止于评估。
// bindAdmissionDecision verifies every binding field; a mismatch maps to its sentinel.
func bindAdmissionDecision(c core.AdmissionCandidate, e core.AdmissionEvaluation, in core.AdmissionDecisionInput) error {
	if in.CandidateID != c.CandidateID {
		return fmt.Errorf("%w: 决策文件指向别的候选", core.ErrAdmissionInvalidInput)
	}
	if in.CandidateRevision != c.Revision {
		return fmt.Errorf("%w: 决策绑定修订 %d，候选当前修订 %d",
			core.ErrAdmissionInvalidState, in.CandidateRevision, c.Revision)
	}
	if in.CandidateHash != c.CandidateHash {
		return fmt.Errorf("%w: 决策绑定的候选内容哈希与候选行不一致",
			core.ErrAdmissionCandidateMismatch)
	}
	if in.EvaluationID != e.EvaluationID || in.EvaluationHash != e.EvaluationHash {
		return fmt.Errorf("%w: 决策绑定的评估与最新评估不一致",
			core.ErrAdmissionEvaluationMismatch)
	}
	if in.ApprovedScope == nil {
		return nil
	}
	if in.ApprovedScope.SkillName != c.SkillName {
		return fmt.Errorf("%w: 批准范围的技能与候选不一致", core.ErrAdmissionInvalidInput)
	}
	if in.ApprovedScope.RiskTier != e.RiskTier {
		return fmt.Errorf("%w: 批准范围的风险档与评估不一致",
			core.ErrAdmissionEvaluationMismatch)
	}
	manifest, err := core.ParseAdmissionManifest(c.ManifestJSON)
	if err != nil {
		return err
	}
	if !slices.Equal(manifest, in.ApprovedScope.Files) {
		return fmt.Errorf("%w: 批准范围的文件集与候选清单不一致",
			core.ErrAdmissionCandidateMismatch)
	}
	referrers := append([]string(nil), in.ApprovedScope.Referrers...)
	expectedReferrers := append([]string(nil), e.Referrers...)
	slices.Sort(referrers)
	slices.Sort(expectedReferrers)
	if !slices.Equal(referrers, expectedReferrers) {
		return fmt.Errorf("%w: 批准范围的引用名单与评估不一致",
			core.ErrAdmissionEvaluationMismatch)
	}
	if in.Baseline != nil && in.Baseline.CanonicalJSON != e.BaselineJSON {
		return fmt.Errorf("%w: 决策携带的基线不是评估绑定的基线", core.ErrAdmissionStaleBaseline)
	}
	return nil
}

// Approve 是受控批准的唯一入口：身份先于状态读取，随后按固定顺序校验状态、评估聚合、
// 磁盘内容与来源、保留期、决策解析、绑定、当前基线、策略哈希、风险档、批准边界、
// 有效期与高风险分离；全部通过才预写 pending 并单事务写决策行。任何失败都发生在决策
// 写入之前，候选保持 evaluated 且没有决策行。
// Approve re-reads the frozen candidate, re-verifies every binding, and writes the
// decision only after all preconditions pass; failures leave the candidate evaluated.
func (a *AdmissionAdmin) Approve(ctx context.Context, actor core.Actor, candidateID string, decisionJSON []byte, policy core.AdmissionPolicy) (core.AdmissionDecision, error) {
	if err := checkTrustedActor(actor); err != nil {
		return core.AdmissionDecision{}, core.ErrAdmissionUnknownApprover
	}
	candidate, unlock, err := a.lockCandidate(ctx, candidateID)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	defer unlock()
	if candidate.State != core.AdmissionEvaluated {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 候选状态 %s 不允许批准",
			core.ErrAdmissionInvalidState, candidate.State)
	}
	evaluation, err := a.store.LatestEvaluation(ctx, candidateID)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	if evaluation.Aggregate == core.AdmissionAggregateFailed {
		return core.AdmissionDecision{}, core.ErrAdmissionEvaluationFailed
	}
	// C-03：批准也必须从磁盘重读并复算哈希，评估后的篡改在决策写入前拒绝。
	// C-03: approval re-reads and re-hashes the frozen candidate before writing.
	stored, _, sourceJSON, err := workspace.ReadAdmissionCandidate(a.root, candidateID)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	if stored.CandidateHash != candidate.CandidateHash {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 磁盘内容哈希与候选行不一致",
			core.ErrAdmissionCandidateMismatch)
	}
	if core.AdmissionSHA256Hex(sourceJSON) != candidate.SourceHash {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 磁盘来源哈希与候选行不一致",
			core.ErrAdmissionSourceMismatch)
	}
	source, err := core.ParseAdmissionSource(sourceJSON)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	if err := admissionRetentionValid(source, a.now()); err != nil {
		return core.AdmissionDecision{}, err
	}
	input, err := core.ParseAdmissionDecision(decisionJSON)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	if input.Decision != core.AdmissionApprove {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 批准入口只接受 approve 决策",
			core.ErrAdmissionInvalidInput)
	}
	if core.TextHasSecret(input.Reason) {
		return core.AdmissionDecision{}, core.ErrAdmissionSecretDetected
	}
	if err := bindAdmissionDecision(candidate, evaluation, input); err != nil {
		return core.AdmissionDecision{}, err
	}
	current, err := a.baseline(ctx, candidate.SkillName)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	if current.CanonicalJSON != evaluation.BaselineJSON {
		return core.AdmissionDecision{}, core.ErrAdmissionStaleBaseline
	}
	if policy.Hash != evaluation.PolicyHash {
		return core.AdmissionDecision{}, core.ErrAdmissionStalePolicy
	}
	if risk := AdmissionRiskTier(candidate, evaluation); risk != evaluation.RiskTier {
		return core.AdmissionDecision{}, core.ErrAdmissionRiskEscalated
	}
	if err := checkAdmissionApprovalBoundary(evaluation, input); err != nil {
		return core.AdmissionDecision{}, err
	}
	if !admissionExpiryWithin24h(input.ExpiresAt, a.now()) {
		return core.AdmissionDecision{}, core.ErrAdmissionExpiryInvalid
	}
	if err := checkApprovalSeparation(candidate, evaluation, actor); err != nil {
		return core.AdmissionDecision{}, err
	}
	decisionID, err := core.NewAdmissionDecisionID()
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	now := a.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionSkillAdmissionApprove,
		TargetKind:   core.TargetKindSkillAdmission,
		TargetID:     candidateID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  core.AdmissionSHA256Hex(decisionJSON),
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := a.operations.BeginOperation(ctx, record); err != nil {
		a.logger.Error("准入批准预写操作失败，拒绝执行", "candidate", candidateID, "error", err)
		return core.AdmissionDecision{}, fmt.Errorf("预写管理操作失败: %w", err)
	}
	decision := core.AdmissionDecision{
		DecisionID:         decisionID,
		CandidateID:        candidateID,
		CandidateRevision:  candidate.Revision,
		CandidateHash:      candidate.CandidateHash,
		EvaluationID:       evaluation.EvaluationID,
		EvaluationHash:     evaluation.EvaluationHash,
		Decision:           core.AdmissionApprove,
		Reason:             input.Reason,
		Acknowledged:       input.Acknowledged,
		ApprovedScopeJSON:  core.AdmissionApprovedScopeJSON(input.ApprovedScope),
		ScriptReviewJSON:   core.AdmissionScriptReviewJSON(input.ScriptReview),
		BaselineJSON:       evaluation.BaselineJSON,
		RiskTier:           evaluation.RiskTier,
		ApproverSubject:    actor.Subject,
		ApproverAuthMethod: actor.Method,
		ApproverSource:     actor.Source,
		ExpiresAt:          input.ExpiresAt,
		CreatedAt:          now,
		OperationID:        operationID,
	}
	if err := a.store.InsertDecision(ctx, decision, candidate.Revision, now); err != nil {
		return core.AdmissionDecision{}, a.finishFailed(ctx, operationID, err)
	}
	return decision, nil
}

// Reject 走与批准相同的身份、评估与绑定校验，但不需要批准范围 / 基线 / 有效期，也不
// 重读磁盘：拒绝不会让候选生效，过期的来源材料不阻止把它关掉。单事务把旧活跃批准置
// superseded、写拒绝决策并把理由落到候选终态。
// Reject shares the identity and binding checks with Approve but needs no scope, baseline
// or expiry; the store supersedes the active approval and stamps the terminal reason.
func (a *AdmissionAdmin) Reject(ctx context.Context, actor core.Actor, candidateID string, decisionJSON []byte) (core.AdmissionDecision, error) {
	if err := checkTrustedActor(actor); err != nil {
		return core.AdmissionDecision{}, core.ErrAdmissionUntrustedActor
	}
	candidate, unlock, err := a.lockCandidate(ctx, candidateID)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	defer unlock()
	if candidate.State != core.AdmissionEvaluated && candidate.State != core.AdmissionApproved {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 候选状态 %s 不允许拒绝",
			core.ErrAdmissionInvalidState, candidate.State)
	}
	// 决策行必须绑定评估行（evaluation_id NOT NULL）；没有评估的 proposed 只能 cancel。
	// The decision row requires an evaluation; a proposed candidate can only be canceled.
	evaluation, err := a.store.LatestEvaluation(ctx, candidateID)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	input, err := core.ParseAdmissionDecision(decisionJSON)
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	if input.Decision != core.AdmissionReject {
		return core.AdmissionDecision{}, fmt.Errorf("%w: 拒绝入口只接受 reject 决策",
			core.ErrAdmissionInvalidInput)
	}
	if core.TextHasSecret(input.Reason) {
		return core.AdmissionDecision{}, core.ErrAdmissionSecretDetected
	}
	if err := bindAdmissionDecision(candidate, evaluation, input); err != nil {
		return core.AdmissionDecision{}, err
	}
	decisionID, err := core.NewAdmissionDecisionID()
	if err != nil {
		return core.AdmissionDecision{}, err
	}
	now := a.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionSkillAdmissionReject,
		TargetKind:   core.TargetKindSkillAdmission,
		TargetID:     candidateID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  core.AdmissionSHA256Hex(decisionJSON),
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := a.operations.BeginOperation(ctx, record); err != nil {
		a.logger.Error("准入拒绝预写操作失败，拒绝执行", "candidate", candidateID, "error", err)
		return core.AdmissionDecision{}, fmt.Errorf("预写管理操作失败: %w", err)
	}
	decision := core.AdmissionDecision{
		DecisionID:         decisionID,
		CandidateID:        candidateID,
		CandidateRevision:  candidate.Revision,
		CandidateHash:      candidate.CandidateHash,
		EvaluationID:       evaluation.EvaluationID,
		EvaluationHash:     evaluation.EvaluationHash,
		Decision:           core.AdmissionReject,
		Reason:             input.Reason,
		Acknowledged:       input.Acknowledged,
		RiskTier:           evaluation.RiskTier,
		ApproverSubject:    actor.Subject,
		ApproverAuthMethod: actor.Method,
		ApproverSource:     actor.Source,
		CreatedAt:          now,
		OperationID:        operationID,
	}
	if err := a.store.InsertDecision(ctx, decision, candidate.Revision, now); err != nil {
		return core.AdmissionDecision{}, a.finishFailed(ctx, operationID, err)
	}
	return decision, nil
}

// Cancel 把候选关掉：只允许 proposed / evaluated / approved，先校验身份、状态与理由
// （含凭据扫描），再预写 pending 操作，最后由仓储在同一事务里置 canceled、把活跃批准
// 置 superseded 并回填终态。它不读磁盘、不读策略——取消不需要重新复核证据。
// Cancel closes a candidate before publication; it never re-reads the frozen tree or the
// policy, because cancellation does not make the candidate effective.
func (a *AdmissionAdmin) Cancel(ctx context.Context, actor core.Actor, candidateID, reason string) (core.AdmissionCandidate, error) {
	if err := checkTrustedActor(actor); err != nil {
		return core.AdmissionCandidate{}, core.ErrAdmissionUntrustedActor
	}
	candidate, unlock, err := a.lockCandidate(ctx, candidateID)
	if err != nil {
		return core.AdmissionCandidate{}, err
	}
	defer unlock()
	switch candidate.State {
	case core.AdmissionProposed, core.AdmissionEvaluated, core.AdmissionApproved:
	default:
		return core.AdmissionCandidate{}, fmt.Errorf("%w: 候选状态 %s 不允许取消",
			core.ErrAdmissionInvalidState, candidate.State)
	}
	// 理由进候选终态与审计摘要，先扫描再落盘，错误文案不回显命中内容。
	// The reason lands in the terminal columns, so scan it before anything is written.
	if reason == "" || utf8.RuneCountInString(reason) > admissionReasonMaxRunes {
		return core.AdmissionCandidate{}, fmt.Errorf("%w: 取消理由必须为 1~%d 字符",
			core.ErrAdmissionInvalidInput, admissionReasonMaxRunes)
	}
	if core.TextHasSecret(reason) {
		return core.AdmissionCandidate{}, core.ErrAdmissionSecretDetected
	}
	now := a.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionSkillAdmissionCancel,
		TargetKind:   core.TargetKindSkillAdmission,
		TargetID:     candidateID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  core.AdmissionRequestHash(candidateID, strconv.FormatInt(candidate.Revision, 10), reason),
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := a.operations.BeginOperation(ctx, record); err != nil {
		a.logger.Error("准入取消预写操作失败，拒绝执行", "candidate", candidateID, "error", err)
		return core.AdmissionCandidate{}, fmt.Errorf("预写管理操作失败: %w", err)
	}
	canceled, err := a.store.CancelCandidate(ctx, candidateID, reason, operationID, candidate.Revision, now)
	if err != nil {
		a.logger.Warn("准入取消失败", "candidate", candidateID, "operation", operationID, "error", err)
		return core.AdmissionCandidate{}, a.finishFailed(ctx, operationID, err)
	}
	return canceled, nil
}

// Publish 是受控发布的唯一入口（§2.3.5、C-01 / C-09 / C-10 / C-18 / C-20）：身份先于
// 状态读取，随后按固定顺序校验状态、决策绑定、批准有效期、评估哈希、磁盘内容与来源、
// 保留期、当前基线、策略哈希与风险档；全部通过后才取回滚材料、分配版本号并预写
// pending。文件写入在技能锁内、gate 外完成，gate 只包住指针事务；任一失败经
// abortSkillCommit 补偿，候选保持 approved（C-18），重试必须从第 1 步重新复核。
// Publish is the only controlled-publication entry: it re-verifies every binding,
// reuses the shared work-copy / snapshot / publish-transaction primitives, and leaves
// the candidate approved on failure so a retry must re-check everything.
func (a *AdmissionAdmin) Publish(ctx context.Context, actor core.Actor, candidateID string, policy core.AdmissionPolicy) (core.AdmissionPublication, error) {
	if err := checkTrustedActor(actor); err != nil {
		return core.AdmissionPublication{}, core.ErrAdmissionUntrustedActor
	}
	candidate, unlock, err := a.lockCandidate(ctx, candidateID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	defer unlock()
	if candidate.State != core.AdmissionApproved {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 候选状态 %s 不允许发布",
			core.ErrAdmissionInvalidState, candidate.State)
	}
	decision, err := a.store.LatestDecision(ctx, candidateID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if decision.Decision != core.AdmissionApprove {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 最新决策不是批准",
			core.ErrAdmissionInvalidState)
	}
	if decision.CandidateHash != candidate.CandidateHash {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 决策绑定的候选哈希与候选行不一致",
			core.ErrAdmissionCandidateMismatch)
	}
	// C-08：批准在 `now >= expires_at` 即失效，发布必须拒绝；状态只按决策派生，不回写。
	// C-08: publication is refused from the first instant the approval is expired.
	if !a.now().Before(decision.ExpiresAt) {
		return core.AdmissionPublication{}, core.ErrAdmissionApprovalExpired
	}
	evaluation, err := a.store.GetEvaluation(ctx, decision.EvaluationID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if decision.EvaluationHash != evaluation.EvaluationHash {
		return core.AdmissionPublication{}, core.ErrAdmissionEvaluationMismatch
	}
	// 决策行的 CandidateRevision 记录批准前的修订号；批准事务把候选修订号 +1。
	// 发布以 candidate_hash 绑定内容身份，不要求修订号与候选当前值相等。
	// The decision pins the pre-approval revision; content identity is the hash below.
	stored, files, sourceJSON, err := workspace.ReadAdmissionCandidate(a.root, candidateID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if stored.CandidateHash != candidate.CandidateHash {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 磁盘内容哈希与候选行不一致",
			core.ErrAdmissionCandidateMismatch)
	}
	if core.AdmissionSHA256Hex(sourceJSON) != candidate.SourceHash {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 磁盘来源哈希与候选行不一致",
			core.ErrAdmissionSourceMismatch)
	}
	source, err := core.ParseAdmissionSource(sourceJSON)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if err := admissionRetentionValid(source, a.now()); err != nil {
		return core.AdmissionPublication{}, err
	}
	current, err := a.baseline(ctx, candidate.SkillName)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if current.CanonicalJSON != decision.BaselineJSON {
		return core.AdmissionPublication{}, core.ErrAdmissionStaleBaseline
	}
	if policy.Hash != evaluation.PolicyHash {
		return core.AdmissionPublication{}, core.ErrAdmissionStalePolicy
	}
	if risk := AdmissionRiskTier(candidate, evaluation); risk != evaluation.RiskTier {
		return core.AdmissionPublication{}, core.ErrAdmissionRiskEscalated
	}
	cand, err := workspace.ValidateSkillDraft(candidate.SkillName, files)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	// 回滚材料在预写之前取：读失败不该留下 pending / failed 操作行。
	// The rollback material is read before the pre-write, so a read failure leaves no row.
	restore, err := a.restoreWorkCopyFn(candidate.SkillName)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	// 基线 present 沿用既有身份；absent / deleted 建新 UID，墓碑身份不复用。
	// A present baseline keeps its identity; absent/deleted start a new uid.
	uid := current.ResourceUID
	if current.SkillState != core.AdmissionSkillPresent {
		uid, err = core.NewResourceUID()
		if err != nil {
			return core.AdmissionPublication{}, err
		}
	}
	versions, err := a.resources.VersionsByUID(ctx, core.ResourceSkill, uid)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	versionNo := 1
	if len(versions) > 0 {
		versionNo = versions[0].VersionNo + 1
	}
	now := a.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionSkillAdmissionPublish,
		TargetKind:   core.TargetKindSkillAdmission,
		TargetID:     candidateID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  core.AdmissionRequestHash(candidateID, decision.DecisionID),
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := a.operations.BeginOperation(ctx, record); err != nil {
		a.logger.Error("准入发布预写操作失败，拒绝执行", "candidate", candidateID, "error", err)
		return core.AdmissionPublication{}, fmt.Errorf("预写管理操作失败: %w", err)
	}
	// gate 只包住数据库提交，与既有技能发布同一锁序：资源锁 → 工作副本 / 快照 →
	// gate → 指针事务 → 失败补偿在 gate 外。
	// The gate covers the commit only; file writes and compensation stay outside it.
	if err := a.writeWorkCopy(a.root, cand); err != nil {
		// 工作副本交换失败**且回滚也失败**时磁盘状态不确定（正式目录缺失、旧树在
		// `.backup`、新树在 `.staging`）：记 unknown 留现场给恢复查证 / 人工裁决，
		// 不得记 failed，也不尝试自动复原（US-21 review ② F-01，契约 line 122）。
		// An uncertain work copy must be recorded as unknown, never failed.
		if errors.Is(err, workspace.ErrWorkCopyUncertain) {
			return core.AdmissionPublication{}, a.finishUnknown(ctx, operationID, err)
		}
		// 其余失败由 WriteSkillWorkCopy 保证旧副本原样保留，这一阶段没有文件要回滚。
		return core.AdmissionPublication{}, a.finishFailed(ctx, operationID, err)
	}
	storagePath, err := workspace.SnapshotSkill(a.root, cand, uid, versionNo)
	if err != nil {
		return core.AdmissionPublication{}, a.abortSkillCommit(ctx, operationID, candidate.SkillName,
			"快照失败", err, uid, versionNo, restore)
	}
	request := core.AdmissionPublishRequest{
		CandidateID:       candidateID,
		CandidateRevision: candidate.Revision,
		DecisionID:        decision.DecisionID,
		Version: core.ResourceVersion{
			Kind:         core.ResourceSkill,
			ResourceID:   candidate.SkillName,
			ResourceUID:  uid,
			VersionNo:    versionNo,
			ContentHash:  cand.ContentHash,
			ManifestJSON: cand.ManifestJSON,
			StoragePath:  storagePath,
			OperationID:  operationID,
		},
		Baseline:    current,
		OperationID: operationID,
	}
	a.gate.Lock()
	publication, err := a.publishAdmission(ctx, request, a.now())
	a.gate.Unlock()
	if err != nil {
		return core.AdmissionPublication{}, a.abortSkillCommit(ctx, operationID, candidate.SkillName,
			"发布失败", err, uid, versionNo, restore)
	}
	return publication, nil
}

// restoreWorkCopyFn 在预写前读取技能当前工作副本，返回把工作副本恢复成读取时状态的
// 闭包：副本存在 → 复用 readSkillFiles + restoreSkillWorkCopy 全量重写；副本不存在
// （从未发布 / 已删除）→ 回滚目标就是"没有这个技能"，删除本次写入。读取失败直接返回
// 错误——不在读不出旧内容时开始一次无法补偿的写入。
// restoreWorkCopyFn snapshots the current work copy and returns the compensating closure;
// a missing copy rolls back to "no skill", and a read failure aborts before the pre-write.
func (a *AdmissionAdmin) restoreWorkCopyFn(skillName string) (func() error, error) {
	work, err := workspace.NewSkillLibrary(a.root).HashWorkCopy(skillName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return func() error { return removeSkillWorkCopy(a.root, skillName) }, nil
		}
		return nil, fmt.Errorf("读取技能 %s 的工作副本: %w", skillName, err)
	}
	relDir, err := workspace.SkillWorkCopyRelDir(skillName)
	if err != nil {
		return nil, err
	}
	previous, err := readSkillFiles(a.root, relDir, work.Files)
	if err != nil {
		return nil, fmt.Errorf("读取技能 %s 的工作副本文件: %w", skillName, err)
	}
	return func() error { return restoreSkillWorkCopy(a.root, skillName, previous) }, nil
}

// Withdraw 撤回一次已生效的发布（颗粒度文档 §2.3.6、C-09）：身份先于状态读取；
// 发布记录必须处于 active（已撤回则幂等返回）；随后按**当前指针**分三类——
//
//	restored_previous —— 指针仍指向本次发布的版本：取同 UID 的更早版本内容，
//	                     经共享草稿校验后作为**新版本**快照，事务内带期望写指针；
//	superseded        —— 指针已指向后续发布 / 新的资源实例：只记撤回，不移动指针；
//	deleted           —— 技能指针已删除：只记撤回，不移动指针。
//
// 首版没有更早版本，拒绝撤回到空（引导技能删除或新候选）。事务失败经补偿恢复工作
// 副本并删除新快照，候选保持 published，重试从分类重新开始；分类在事务内会按活指针
// 重算，编排层读到的旧指针不会把错误版本写进指针。
// Withdraw reverts a live publication. Only the restored_previous case moves the
// pointer (through a new version of the earlier content); superseded / deleted only
// record the withdrawal. The transaction re-classifies against the live pointer.
func (a *AdmissionAdmin) Withdraw(ctx context.Context, actor core.Actor, candidateID, reason string) (core.AdmissionPublication, error) {
	if err := checkTrustedActor(actor); err != nil {
		return core.AdmissionPublication{}, core.ErrAdmissionUntrustedActor
	}
	candidate, unlock, err := a.lockCandidate(ctx, candidateID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	defer unlock()
	if candidate.State != core.AdmissionPublished && candidate.State != core.AdmissionWithdrawn {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 候选状态 %s 不允许撤回",
			core.ErrAdmissionInvalidState, candidate.State)
	}
	// 理由进候选终态与发布记录，先按与取消同一上限校验，再扫描凭据。
	// The reason lands in the terminal columns; validate and scan before any write.
	if reason == "" || utf8.RuneCountInString(reason) > admissionReasonMaxRunes {
		return core.AdmissionPublication{}, fmt.Errorf("%w: 撤回理由必须为 1~%d 字符",
			core.ErrAdmissionInvalidInput, admissionReasonMaxRunes)
	}
	publication, err := a.store.GetPublication(ctx, candidateID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	// 幂等：候选已撤回且发布记录同态时原样返回，不新增操作行、不改任何行。
	// Idempotent retry: already-withdrawn pairs return the recorded publication.
	if candidate.State == core.AdmissionWithdrawn {
		if publication.State != core.AdmissionPublicationWithdrawn {
			return core.AdmissionPublication{}, fmt.Errorf("%w: 候选已撤回但发布记录仍是 %s",
				core.ErrAdmissionInvalidState, publication.State)
		}
		return publication, nil
	}
	if publication.State == core.AdmissionPublicationWithdrawn {
		return publication, nil
	}
	if core.TextHasSecret(reason) {
		return core.AdmissionPublication{}, core.ErrAdmissionSecretDetected
	}
	pointer, err := a.resources.Pointer(ctx, core.ResourceSkill, candidate.SkillName)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	// 上一版只能从版本表按 UID 取：指针只带 PublishedVersionID，没有版本号与
	// storage_path，无法单独推导撤回目标。
	// The earlier version is resolved from the version table, never from the pointer.
	versions, err := a.resources.VersionsByUID(ctx, core.ResourceSkill, publication.SkillUID)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	withdrawReason, previous, err := classifyAdmissionWithdrawal(&pointer, publication, versions)
	if err != nil {
		return core.AdmissionPublication{}, err
	}
	if withdrawReason == core.AdmissionWithdrawRestoredPrevious && previous == nil {
		return core.AdmissionPublication{}, core.ErrAdmissionFirstVersionWithdraw
	}
	now := a.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionSkillAdmissionWithdraw,
		TargetKind:   core.TargetKindSkillAdmission,
		TargetID:     candidateID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  core.AdmissionRequestHash(candidateID, strconv.FormatInt(publication.VersionID, 10), reason),
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := a.operations.BeginOperation(ctx, record); err != nil {
		a.logger.Error("准入撤回预写操作失败，拒绝执行", "candidate", candidateID, "error", err)
		return core.AdmissionPublication{}, fmt.Errorf("预写管理操作失败: %w", err)
	}
	var (
		withdrawVersion *core.ResourceVersion
		withdrawRestore func() error
	)
	if withdrawReason == core.AdmissionWithdrawRestoredPrevious {
		withdrawVersion, withdrawRestore, err = a.prepareWithdrawVersion(ctx, candidate.SkillName, *previous)
		if err != nil {
			// 补偿本身失败时磁盘状态不确定：记 unknown，留现场给恢复路径查证。
			// A failed compensation is an indeterminate outcome, not a clean failure.
			if errors.Is(err, core.ErrAdmissionCompensationUncertain) {
				return core.AdmissionPublication{}, a.finishUnknown(ctx, operationID, err)
			}
			return core.AdmissionPublication{}, a.finishFailed(ctx, operationID, err)
		}
	}
	request := core.AdmissionWithdrawRequest{
		CandidateID:       candidateID,
		CandidateRevision: candidate.Revision,
		Publication:       publication,
		WithdrawVersion:   withdrawVersion,
		Reason:            reason,
		OperationID:       operationID,
	}
	// 撤回可能在指针仍指向本次发布版本时写新版本 + 指针，因此与既有发布同持 gate；
	// gate 只包住事务，失败补偿在释放后执行（与 Publish 同一锁序）。
	// The gate covers only the transaction; compensation runs after the unlock.
	a.gate.Lock()
	withdrawn, err := a.withdrawAdmission(ctx, request, a.now())
	a.gate.Unlock()
	if err != nil {
		if withdrawRestore != nil {
			return core.AdmissionPublication{}, a.abortSkillCommit(ctx, operationID,
				candidate.SkillName, "撤回失败", err, publication.SkillUID, withdrawVersion.VersionNo, withdrawRestore)
		}
		// 被超越 / 技能已删分支不碰磁盘，但提交结果未知同样只证明"可能已生效"：
		// 操作记 unknown 交恢复查证，不按确定失败回执（契约「提交结果未知」段）。
		// The no-disk branches still cannot prove an unknown commit rolled back,
		// so they record unknown and leave verification to recovery.
		if errors.Is(err, core.ErrAdmissionCommitUnknown) {
			return core.AdmissionPublication{}, a.finishUnknown(ctx, operationID, err)
		}
		return core.AdmissionPublication{}, a.finishFailed(ctx, operationID, err)
	}
	return withdrawn, nil
}

// classifyAdmissionWithdrawal 只分类、不改指针：已删除 → deleted；指针 UID 与发布记录
// 一致且仍指向本次发布版本 → restored_previous，并从版本列表取 < 本次版本号的最大者
// 作为上一版；其余 → superseded。没有更早版本时 previous 为 nil，由 Withdraw 映射为
// 首版撤回拒绝；只有"指针为 nil"这种装配错误才返回 error。
// classifyAdmissionWithdrawal classifies against the read pointer without touching it;
// it never derives a version number or storage path from the pointer.
func classifyAdmissionWithdrawal(pointer *core.ResourcePointer, publication core.AdmissionPublication,
	versions []core.ResourceVersion,
) (string, *core.ResourceVersion, error) {
	if pointer == nil {
		return "", nil, fmt.Errorf("%w: 撤回缺少技能指针", core.ErrAdmissionInvalidInput)
	}
	if pointer.DeletedAt != nil {
		return core.AdmissionWithdrawDeleted, nil, nil
	}
	if pointer.ResourceUID != publication.SkillUID || pointer.PublishedVersionID != publication.VersionID {
		return core.AdmissionWithdrawSuperseded, nil, nil
	}
	var previous *core.ResourceVersion
	for i := range versions {
		if int64(versions[i].VersionNo) >= publication.VersionNo {
			continue
		}
		if previous == nil || versions[i].VersionNo > previous.VersionNo {
			previous = &versions[i]
		}
	}
	return core.AdmissionWithdrawRestoredPrevious, previous, nil
}

// prepareWithdrawVersion 把上一版发布内容读回、校验并作为同 UID 的**新版本**落盘：
// `LoadPublished` 全量闸门（目录归属 / 版本号 / 全量重扫）+ 与登记清单逐项比对 →
// 共享草稿校验 → 预读工作副本 → 全量替换工作副本 → 快照。版本号按既有口径取
// `versions[0].VersionNo + 1`；事务里分配的号与之不符会
// 回滚，不会把错误版本号写进 storage_path。内部中途失败的补偿：快照失败先恢复工作
// 副本、删除本次快照再返回；补偿失败或工作副本交换回滚失败返回
// `ErrAdmissionCompensationUncertain`，调用方据此记 unknown，其余按 failed 回执。
// prepareWithdrawVersion materializes the earlier content as a new version of the same
// UID; on an internal failure it restores the work copy and removes the snapshot. A
// failed compensation or an uncertain work copy is returned as compensation-uncertain
// so the caller records unknown.
func (a *AdmissionAdmin) prepareWithdrawVersion(ctx context.Context, skillName string,
	previous core.ResourceVersion,
) (*core.ResourceVersion, func() error, error) {
	// 先走 LoadPublished 的完整闸门：目录归属（storage_path 必须属于该技能与版本）
	// 与整个发布目录的全量重扫，再把重算结果与登记的 manifest_json / content_hash
	// 逐项比对——清单外多文件、少文件、正文被改写都拒绝，不把可疑内容当成"上一版"
	// 重新发布（与手工技能详情读取同一口径）。
	// LoadPublished validates dir ownership and rescans the whole published dir;
	// the recomputed manifest and content hash must equal the recorded row.
	snapshot, err := workspace.NewSkillLibrary(a.root).LoadPublished(
		ctx, skillName, previous.VersionNo, previous.StoragePath)
	if err != nil {
		// 目录归属 / 路径形状 / 全量重扫的任一失败都是「上一版不可信」，统一包成
		// admission-mismatch 类错误；原始错误经第二个 %w 保留在链上。
		// Any LoadPublished failure means the earlier version is untrustworthy; wrap it
		// as a mismatch while keeping the original error in the chain.
		return nil, nil, fmt.Errorf("%w: 技能 %s 的上一版发布目录不可信: %w",
			core.ErrAdmissionCandidateMismatch, skillName, err)
	}
	if snapshot.ContentHash != previous.ContentHash {
		return nil, nil, fmt.Errorf("%w: 技能 %s 的上一版内容重算哈希与登记不一致",
			core.ErrAdmissionCandidateMismatch, skillName)
	}
	hashes, err := parseSkillManifest(skillName, previous.ManifestJSON)
	if err != nil {
		return nil, nil, err
	}
	if !slices.Equal(snapshot.Files, hashes) {
		return nil, nil, fmt.Errorf("%w: 技能 %s 的上一版文件清单与登记不一致",
			core.ErrAdmissionCandidateMismatch, skillName)
	}
	relDir, err := workspace.PublishedVersionRelDir(previous.StoragePath)
	if err != nil {
		return nil, nil, err
	}
	files, err := readSkillFiles(a.root, relDir, hashes)
	if err != nil {
		return nil, nil, fmt.Errorf("读取技能 %s 的上一版内容: %w", skillName, err)
	}
	drafts := make([]core.AuthorFile, len(files))
	for i, file := range files {
		drafts[i] = core.AuthorFile{Path: file.Path, Content: string(file.Content)}
	}
	cand, err := workspace.ValidateSkillDraft(skillName, drafts)
	if err != nil {
		return nil, nil, err
	}
	versions, err := a.resources.VersionsByUID(ctx, core.ResourceSkill, previous.ResourceUID)
	if err != nil {
		return nil, nil, err
	}
	versionNo := 1
	if len(versions) > 0 {
		versionNo = versions[0].VersionNo + 1
	}
	restore, err := a.restoreWorkCopyFn(skillName)
	if err != nil {
		return nil, nil, err
	}
	if err := a.writeWorkCopy(a.root, cand); err != nil {
		// 工作副本交换失败且回滚也失败时磁盘状态不确定：包成补偿不确定交给调用方
		// 记 unknown；其余失败旧副本原样保留，按确定失败返回（US-21 review ② F-01）。
		// An uncertain work copy is escalated so the caller records unknown.
		if errors.Is(err, workspace.ErrWorkCopyUncertain) {
			return nil, nil, fmt.Errorf("%w: %w", core.ErrAdmissionCompensationUncertain, err)
		}
		return nil, nil, err
	}
	storagePath, err := workspace.SnapshotSkill(a.root, cand, previous.ResourceUID, versionNo)
	if err != nil {
		// 补偿失败时磁盘状态不确定（工作副本可能没恢复、快照可能残留），必须让
		// 调用方把操作记 unknown 而不是 failed（US-21 review ② 发现 4）。两个失败
		// 都保留在错误链上，便于排障。
		// A failed compensation leaves the disk in an uncertain state; the caller must
		// record unknown rather than failed.
		var compensationErr error
		if restoreErr := restore(); restoreErr != nil {
			a.logger.Error("撤回准备失败后恢复工作副本失败", "skill", skillName, "error", restoreErr)
			compensationErr = restoreErr
		}
		if snapshotErr := a.removeSnapshot(a.root, skillName, previous.ResourceUID, versionNo); snapshotErr != nil {
			a.logger.Error("撤回准备失败后清理快照失败", "skill", skillName,
				"version_no", versionNo, "error", snapshotErr)
			compensationErr = errors.Join(compensationErr, snapshotErr)
		}
		if compensationErr != nil {
			return nil, nil, fmt.Errorf("%w: 撤回准备失败且补偿未完成: %w: %w",
				core.ErrAdmissionCompensationUncertain, err, compensationErr)
		}
		return nil, nil, err
	}
	return &core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceID:   skillName,
		ResourceUID:  previous.ResourceUID,
		VersionNo:    versionNo,
		ContentHash:  cand.ContentHash,
		ManifestJSON: cand.ManifestJSON,
		StoragePath:  storagePath,
	}, restore, nil
}

// abortSkillCommit 是 AdmissionAdmin 对包级补偿的薄适配：终态回填走准入面自己的
// `lifecycleResultInternal` 文案口径，补偿逻辑与 ResourceAdmin 共用同一份实现（P-03）。
// abortSkillCommit is the thin adapter over the package-level compensation shared with
// ResourceAdmin; only the terminal writer differs.
func (a *AdmissionAdmin) abortSkillCommit(ctx context.Context, operationID, skillName, reason string,
	cause error, uid core.ResourceUID, versionNo int, restore func() error,
) error {
	return abortSkillCommit(ctx, a.root, a.logger,
		func(ctx context.Context, operationID string, status core.OperationStatus, message string) {
			if err := a.operations.FinishOperation(context.WithoutCancel(ctx), operationID,
				status, lifecycleResultInternal, message, nil, a.now()); err != nil {
				a.logger.Error("准入补偿终态回填失败，留待启动恢复查证",
					"operation_id", operationID, "status", string(status), "error", err)
			}
		},
		operationID, skillName, reason, cause, uid, versionNo, restore)
}

// VerifyOperation 是启动恢复对 `skill.admission.*` 待决操作的只读查证（颗粒度文档
// §2.3.7）：返回 core 的查证字面量，由 `verifyOutcome` 统一映射终态。**只查证、
// 不重放**——读失败一律折叠为 indeterminate（unknown），不猜成功、不中止启动。
//
// 各动作的证据来源都是同一个操作在写路径上留下的"最后一次写入"事实：
// propose 看候选行 / 候选目录（目录在而行为空 = 写到一半，只能 unknown）；其余
// 五个动作看带 `operation_id` 的评估行 / 决策行 / 发布行。cancel 不看"状态是不是
// 终态"而看**终态是不是本操作写的**——别的操作留下的 rejected / withdrawn 不能
// 认成这次取消的成果。
// VerifyOperation is the recovery-time read-only verdict for skill.admission.*
// operations; it never replays and folds every read failure into indeterminate.
func (a *AdmissionAdmin) VerifyOperation(ctx context.Context, op core.OperationRecord) string {
	switch op.Action {
	case core.ActionSkillAdmissionPropose:
		_, err := a.store.GetCandidate(ctx, op.TargetID)
		if err == nil {
			return core.VerificationApplied
		}
		if !errors.Is(err, core.ErrAdmissionNotFound) {
			return core.VerificationIndeterminate
		}
		if workspace.AdmissionCandidateDirExists(a.root, op.TargetID) {
			// 目录在而候选行不在：写到一半，删目录会毁证据、建行会伪造事实——只能 unknown。
			// A frozen directory without its row is half a write: neither outcome is provable.
			return core.VerificationIndeterminate
		}
		return core.VerificationNotApplied
	case core.ActionSkillAdmissionEvaluate:
		_, err := a.store.FindEvaluationByOperation(ctx, op.OperationID)
		return admissionRecoveryVerdict(err)
	case core.ActionSkillAdmissionApprove, core.ActionSkillAdmissionReject:
		_, err := a.store.FindDecisionByOperation(ctx, op.OperationID)
		return admissionRecoveryVerdict(err)
	case core.ActionSkillAdmissionCancel:
		candidate, err := a.store.GetCandidate(ctx, op.TargetID)
		if err != nil {
			return core.VerificationIndeterminate
		}
		if candidate.State == core.AdmissionCanceled && candidate.TerminalOperationID == op.OperationID {
			return core.VerificationApplied
		}
		switch candidate.State {
		case core.AdmissionProposed, core.AdmissionEvaluated, core.AdmissionApproved:
			return core.VerificationNotApplied
		default:
			// 已是别的终态（rejected / withdrawn / 别的操作取消）——不能把别人的
			// 终态认成本操作的成果。
			// Another operation's terminal state is not evidence for this one.
			return core.VerificationIndeterminate
		}
	case core.ActionSkillAdmissionPublish:
		_, err := a.store.FindPublicationByOperation(ctx, op.OperationID)
		return admissionRecoveryVerdict(err)
	case core.ActionSkillAdmissionWithdraw:
		_, err := a.store.FindPublicationByWithdrawOperation(ctx, op.OperationID)
		return admissionRecoveryVerdict(err)
	default:
		return core.VerificationIndeterminate
	}
}

// admissionRecoveryVerdict 把"按 operation_id 查行"的结果折成三态：行在 → applied；
// 明确不存在 → not_applied；其余读失败 → indeterminate（读不懂不是没生效）。
// admissionRecoveryVerdict maps an operation-keyed row lookup to the tri-state verdict.
func admissionRecoveryVerdict(err error) string {
	switch {
	case err == nil:
		return core.VerificationApplied
	case errors.Is(err, core.ErrAdmissionNotFound):
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// admissionEffectiveState 按决策行派生展示态：`approved` 且活跃批准有效期已过时显示
// `expired`，其余逐字返回存储态。它只改返回值，不写库、不触发后台扫描（C-08）。
// 管理台侧的 `admissionDisplayState` 是同口径的**另一份**实现（契约要求两侧各持一份、
// 不共享代码），改动时两侧必须同步并各自跑派生态用例。
// admissionEffectiveState derives the display-only `expired` state from the active
// approval; it never writes. The web package keeps its own predicate with identical
// semantics (the contract requires one on each side, no shared code) — change both.
func admissionEffectiveState(candidate core.AdmissionCandidate, decision *core.AdmissionDecision, now time.Time) core.AdmissionState {
	if candidate.State != core.AdmissionApproved || decision == nil ||
		decision.Decision != core.AdmissionApprove || !decision.SupersededAt.IsZero() ||
		decision.ExpiresAt.IsZero() {
		return candidate.State
	}
	// 文档口径：`now >= expires_at` 即过期（含相等）。
	// Doc semantics: expiry begins at `now == expires_at`, not after it.
	if !now.Before(decision.ExpiresAt) {
		return core.AdmissionExpired
	}
	return candidate.State
}

// List 按过滤条件返回候选；`expired` 过滤由仓储翻译成"活跃批准已过期"的查询，返回行
// 再按 `admissionEffectiveState` 派生展示态——数据库里的状态始终不变。
// List returns candidates with the derived display state applied; stored rows never change.
func (a *AdmissionAdmin) List(ctx context.Context, filter core.AdmissionFilter) ([]core.AdmissionCandidate, error) {
	candidates, err := a.store.ListCandidates(ctx, filter)
	if err != nil {
		return nil, err
	}
	// 只有 approved 行需要活跃批准来判断展示态；已终态的行逐字返回。
	// Only approved rows need the active approval row to derive the display state.
	now := a.now()
	for i, candidate := range candidates {
		if candidate.State != core.AdmissionApproved {
			continue
		}
		decision, err := a.store.ActiveApproval(ctx, candidate.CandidateID)
		switch {
		case err == nil:
			candidates[i].State = admissionEffectiveState(candidate, &decision, now)
		case errors.Is(err, core.ErrAdmissionNotFound):
		default:
			return nil, err
		}
	}
	// 按 `approved` 过滤时排除派生为 expired 的行：用 `--state approved` 的人要的是
	// 还能发布的批准，过期行要在 `--state expired` 里看。这只是读取侧投影，库不变。
	// `--state approved` excludes rows that derive to expired; the stored rows stay put.
	if filter.State == core.AdmissionApproved {
		kept := candidates[:0]
		for _, candidate := range candidates {
			if candidate.State == core.AdmissionApproved {
				kept = append(kept, candidate)
			}
		}
		candidates = kept
	}
	return candidates, nil
}

// Get 组装只读详情：候选、最新评估 / 决策 / 发布、解析后的来源；`IncludeFiles`
// 附带候选文件，`IncludeDiff` 附带与当前已发布版本的有界 unified diff。缺评估 /
// 决策 / 发布的候选是正常状态，对应字段为 nil。
// Get assembles the read-only detail; missing evaluation/decision/publication are
// normal states, and files/diff are opt-in.
func (a *AdmissionAdmin) Get(ctx context.Context, candidateID string, options core.AdmissionShowOptions) (core.AdmissionDetail, error) {
	candidate, err := a.store.GetCandidate(ctx, candidateID)
	if err != nil {
		return core.AdmissionDetail{}, err
	}
	detail := core.AdmissionDetail{Candidate: candidate}

	evaluation, err := a.store.LatestEvaluation(ctx, candidateID)
	switch {
	case err == nil:
		detail.Evaluation = &evaluation
	case errors.Is(err, core.ErrAdmissionNotFound):
	default:
		return core.AdmissionDetail{}, err
	}
	decision, err := a.store.LatestDecision(ctx, candidateID)
	switch {
	case err == nil:
		detail.Decision = &decision
	case errors.Is(err, core.ErrAdmissionNotFound):
	default:
		return core.AdmissionDetail{}, err
	}
	// 详情与列表用同一个派生判据：过期只改展示态，库里的 approved 不变。
	// Detail shares the list's single expiry predicate; the stored state is untouched.
	detail.Candidate.State = admissionEffectiveState(candidate, detail.Decision, a.now())
	publication, err := a.store.GetPublication(ctx, candidateID)
	switch {
	case err == nil:
		detail.Publication = &publication
	case errors.Is(err, core.ErrAdmissionNotFound):
	default:
		return core.AdmissionDetail{}, err
	}
	source, err := core.ParseAdmissionSource([]byte(candidate.SourceJSON))
	if err != nil {
		return core.AdmissionDetail{}, err
	}
	detail.Source = source

	if !options.IncludeFiles && !options.IncludeDiff {
		return detail, nil
	}
	stored, files, sourceJSON, err := workspace.ReadAdmissionCandidate(a.root, candidateID)
	if err != nil {
		return core.AdmissionDetail{}, err
	}
	if stored.CandidateHash != candidate.CandidateHash {
		return core.AdmissionDetail{}, fmt.Errorf("%w: 磁盘内容哈希与候选行不一致",
			core.ErrAdmissionCandidateMismatch)
	}
	if core.AdmissionSHA256Hex(sourceJSON) != candidate.SourceHash {
		return core.AdmissionDetail{}, fmt.Errorf("%w: 磁盘来源哈希与候选行不一致",
			core.ErrAdmissionSourceMismatch)
	}
	if options.IncludeFiles {
		detail.Files = files
	}
	if options.IncludeDiff {
		previous, err := a.publishedSkillFiles(ctx, candidate.SkillName)
		if err != nil {
			return core.AdmissionDetail{}, err
		}
		detail.Diff = admissionUnifiedDiff(previous, files)
	}
	return detail, nil
}

// publishedSkillFiles 读取技能当前发布版本的文件内容；技能未发布 / 已删除返回空
// 集合（此时 diff 呈现为"全部新增"）。
// publishedSkillFiles returns the currently published files, or nil when the skill has
// no live publication.
func (a *AdmissionAdmin) publishedSkillFiles(ctx context.Context, skillName string) ([]core.AuthorFile, error) {
	pointer, err := a.resources.Pointer(ctx, core.ResourceSkill, skillName)
	if errors.Is(err, core.ErrResourceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pointer.DeletedAt != nil {
		return nil, nil
	}
	version, err := a.resources.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return nil, err
	}
	var hashes []core.FileHash
	if err := json.Unmarshal([]byte(version.ManifestJSON), &hashes); err != nil {
		return nil, fmt.Errorf("解析技能 %s 的发布清单: %w", skillName, err)
	}
	// storage_path 是带 `.basalt/` 前缀的契约字面量，而 a.root 已经是工作区根
	// （`.basalt/`）——读取必须换成相对工作区根的形态，否则会在根下再拼一层。
	// The recorded storage_path is the contract literal; work-root-relative reads
	// must strip the `.basalt/` prefix first.
	relDir, err := workspace.PublishedVersionRelDir(version.StoragePath)
	if err != nil {
		return nil, err
	}
	files, err := readSkillFiles(a.root, relDir, hashes)
	if err != nil {
		return nil, err
	}
	out := make([]core.AuthorFile, 0, len(files))
	for _, file := range files {
		out = append(out, core.AuthorFile{Path: file.Path, Content: string(file.Content)})
	}
	return out, nil
}

// admissionUnifiedDiff 生成有界 unified diff：按路径并集排序，跳过两侧相同的文件，
// 每个变更文件一个单 hunk。超过 1 MiB 即停止并在末尾标注截断。
// admissionUnifiedDiff renders a bounded unified diff in path order.
func admissionUnifiedDiff(previous, candidate []core.AuthorFile) string {
	before := make(map[string]string, len(previous))
	for _, file := range previous {
		before[file.Path] = file.Content
	}
	after := make(map[string]string, len(candidate))
	for _, file := range candidate {
		after[file.Path] = file.Content
	}
	paths := make([]string, 0, len(before)+len(after))
	for path := range before {
		paths = append(paths, path)
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)

	var out strings.Builder
	truncated := false
	write := func(line string) bool {
		if out.Len()+len(line)+1 > admissionDiffMaxBytes {
			truncated = true
			return false
		}
		out.WriteString(line)
		out.WriteByte('\n')
		return true
	}
	for _, path := range paths {
		oldContent, existed := before[path]
		newContent, exists := after[path]
		if existed && exists && oldContent == newContent {
			continue
		}
		oldLines := admissionSplitLines(oldContent)
		newLines := admissionSplitLines(newContent)
		oldHeader := "/dev/null"
		if existed {
			oldHeader = "a/" + path
		}
		newHeader := "/dev/null"
		if exists {
			newHeader = "b/" + path
		}
		header := []string{
			"diff --git a/" + path + " b/" + path,
			"--- " + oldHeader,
			"+++ " + newHeader,
			fmt.Sprintf("@@ -%d,%d +%d,%d @@", admissionDiffStart(oldLines), len(oldLines),
				admissionDiffStart(newLines), len(newLines)),
		}
		ok := true
		for _, line := range header {
			if !write(line) {
				ok = false
				break
			}
		}
		if !ok {
			break
		}
		for _, line := range admissionDiffLines(oldLines, newLines) {
			if !write(line) {
				ok = false
				break
			}
		}
		if !ok {
			break
		}
	}
	if truncated {
		out.WriteString("...（diff 超过 1 MiB，已截断）\n")
	}
	return out.String()
}

// admissionDiffStart 给出 hunk 起始行号：空文件按 unified diff 惯例记 0。
// admissionDiffStart reports the hunk start line, using 0 for an empty side.
func admissionDiffStart(lines []string) int {
	if len(lines) == 0 {
		return 0
	}
	return 1
}

// admissionSplitLines 按行切分并丢掉尾部换行产生的空元素；不保留"文件末尾无换行"
// 的标记——它只影响展示细节，不影响差异内容。
// admissionSplitLines splits content into lines, dropping the artifact of a trailing
// newline.
func admissionSplitLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// admissionDiffLines 输出带前缀的行：` ` 保留、`-` 删除、`+` 新增。先剥掉公共
// 前后缀再算 LCS；单元格数超限时退化整段替换。
// admissionDiffLines prefixes kept/removed/added lines; large inputs degrade to a
// whole-block replacement after trimming the common prefix and suffix.
func admissionDiffLines(before, after []string) []string {
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix &&
		before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	middleBefore := before[prefix : len(before)-suffix]
	middleAfter := after[prefix : len(after)-suffix]

	out := make([]string, 0, len(before)+len(after))
	for _, line := range before[:prefix] {
		out = append(out, " "+line)
	}
	if len(middleBefore) > 0 && len(middleAfter) > 0 &&
		len(middleBefore) <= admissionDiffLCSCells/len(middleAfter) {
		out = append(out, admissionDiffLCS(middleBefore, middleAfter)...)
	} else {
		for _, line := range middleBefore {
			out = append(out, "-"+line)
		}
		for _, line := range middleAfter {
			out = append(out, "+"+line)
		}
	}
	for _, line := range before[len(before)-suffix:] {
		out = append(out, " "+line)
	}
	return out
}

// admissionDiffLCS 是行级最长公共子序列的经典动态规划实现；调用方已保证
// `len(before)*len(after) <= admissionDiffLCSCells`。
// admissionDiffLCS is the classic dynamic-programming line LCS; the caller has already
// bounded the cell count.
func admissionDiffLCS(before, after []string) []string {
	rows, cols := len(before), len(after)
	table := make([][]int, rows+1)
	for i := range table {
		table[i] = make([]int, cols+1)
	}
	for i := rows - 1; i >= 0; i-- {
		for j := cols - 1; j >= 0; j-- {
			if before[i] == after[j] {
				table[i][j] = table[i+1][j+1] + 1
				continue
			}
			if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}
	out := make([]string, 0, rows+cols)
	i, j := 0, 0
	for i < rows && j < cols {
		switch {
		case before[i] == after[j]:
			out = append(out, " "+before[i])
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			out = append(out, "-"+before[i])
			i++
		default:
			out = append(out, "+"+after[j])
			j++
		}
	}
	for ; i < rows; i++ {
		out = append(out, "-"+before[i])
	}
	for ; j < cols; j++ {
		out = append(out, "+"+after[j])
	}
	return out
}
