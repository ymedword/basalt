package core

// 本文件声明技能学习准入的纯类型与固定字面量（颗粒度文档 US-21 §3.1.1）：
// 候选 / 评估 / 决策 / 发布记录的状态机、七维评估枚举、固定动作与目标类型。
// 只有类型与常量——编排在 internal/app，存储在 internal/workspace 与
// internal/storage/postgres；core 因此仍然只依赖标准库。
// This file declares the pure types and frozen literals of skill-learning admission:
// the candidate/evaluation/decision/publication state machine, the seven evaluation
// dimensions, and the fixed action and target-kind literals. Orchestration lives in
// internal/app; core only depends on the standard library.

import (
	"errors"
	"fmt"
	"time"
)

// AdmissionState 是候选的存储态；`expired` 是派生的展示态，**不落库**。
// AdmissionState is the stored candidate state; `expired` is derived for display only.
type AdmissionState string

const (
	// AdmissionProposed 表示候选已冻结但尚未评估。
	AdmissionProposed AdmissionState = "proposed"
	// AdmissionEvaluated 表示候选已有确定性评估。
	AdmissionEvaluated AdmissionState = "evaluated"
	// AdmissionApproved 表示本机管理员已批准（不一定仍未过期）。
	AdmissionApproved AdmissionState = "approved"
	// AdmissionRejected 表示候选被拒绝，终态。
	AdmissionRejected AdmissionState = "rejected"
	// AdmissionCanceled 表示候选被取消，终态。
	AdmissionCanceled AdmissionState = "canceled"
	// AdmissionPublished 表示候选版本已进入发布指针。
	AdmissionPublished AdmissionState = "published"
	// AdmissionWithdrawn 表示本次发布已被撤回（记录仍保留）。
	AdmissionWithdrawn AdmissionState = "withdrawn"
	// AdmissionExpired 是派生展示态：approved 且活跃批准已过期，不落库。
	AdmissionExpired AdmissionState = "expired"
)

// AdmissionRiskTier 是候选 / 评估 / 决策的风险档；评估之后只允许 normal / high。
// AdmissionRiskTier is the risk tier; evaluations and decisions only use normal/high.
type AdmissionRiskTier string

const (
	// AdmissionRiskUnknown 是提议时的初始档位（尚未评估）。
	AdmissionRiskUnknown AdmissionRiskTier = "unknown"
	// AdmissionRiskNormal 表示更新未绑定技能且不含脚本。
	AdmissionRiskNormal AdmissionRiskTier = "normal"
	// AdmissionRiskHigh 表示更新已绑定技能或候选含脚本。
	AdmissionRiskHigh AdmissionRiskTier = "high"
)

// 基线里的技能状态；absent = 指针行不存在，deleted = 指针行有删除标记。
// Baseline skill states: absent means no pointer row, deleted means a tombstone.
const (
	// AdmissionSkillPresent 表示技能已发布且指针未删除。
	AdmissionSkillPresent = "present"
	// AdmissionSkillDeleted 表示指针行存在但已标记删除。
	AdmissionSkillDeleted = "deleted"
	// AdmissionSkillAbsent 表示从没有过该技能的指针行。
	AdmissionSkillAbsent = "absent"
)

// AdmissionDimensionName 是七个固定评估维度。
// AdmissionDimensionName is one of the seven fixed evaluation dimensions.
type AdmissionDimensionName string

const (
	// AdmissionDimensionContent 判定候选内容与清单自洽。
	AdmissionDimensionContent AdmissionDimensionName = "content"
	// AdmissionDimensionSource 判定来源声明可用且未被保留期拒绝。
	AdmissionDimensionSource AdmissionDimensionName = "source"
	// AdmissionDimensionPermissions 判定候选声明的权限与脚本审查面。
	AdmissionDimensionPermissions AdmissionDimensionName = "permissions"
	// AdmissionDimensionSafety 判定凭据扫描与危险模式。
	AdmissionDimensionSafety AdmissionDimensionName = "safety"
	// AdmissionDimensionRegression 判定既有引用者与基线影响。
	AdmissionDimensionRegression AdmissionDimensionName = "regression"
	// AdmissionDimensionEffect 表示效果评测（本 US 固定不可评估）。
	AdmissionDimensionEffect AdmissionDimensionName = "effect"
	// AdmissionDimensionCost 表示成本测量（本 US 固定不可评估）。
	AdmissionDimensionCost AdmissionDimensionName = "cost"
)

// AdmissionDimensionStatus 是单个维度的判定结果；`not_evaluable` **不**等于通过。
// AdmissionDimensionStatus is one dimension's verdict; not_evaluable never means pass.
type AdmissionDimensionStatus string

const (
	// AdmissionDimensionPassed 表示该项已按确定性判据通过。
	AdmissionDimensionPassed AdmissionDimensionStatus = "passed"
	// AdmissionDimensionFailed 表示该项失败，阻止批准。
	AdmissionDimensionFailed AdmissionDimensionStatus = "failed"
	// AdmissionDimensionNotEvaluable 表示本 US 无该维度的证据，不能算通过。
	AdmissionDimensionNotEvaluable AdmissionDimensionStatus = "not_evaluable"
)

// AdmissionAggregate 是评估聚合结论：全部 passed 才通过；有 failed 即失败；其余不完整。
// AdmissionAggregate is the evaluation rollup: all passed, any failed, or incomplete.
type AdmissionAggregate string

const (
	// AdmissionAggregatePassed 表示七维全部通过。
	AdmissionAggregatePassed AdmissionAggregate = "passed"
	// AdmissionAggregateFailed 表示至少一维失败。
	AdmissionAggregateFailed AdmissionAggregate = "failed"
	// AdmissionAggregateIncomplete 表示无失败但有维度不可评估。
	AdmissionAggregateIncomplete AdmissionAggregate = "incomplete"
)

// AdmissionDecisionKind 是本机决策的两种取值。
// AdmissionDecisionKind is the two-valued local decision literal.
type AdmissionDecisionKind string

const (
	// AdmissionApprove 表示批准。
	AdmissionApprove AdmissionDecisionKind = "approve"
	// AdmissionReject 表示拒绝。
	AdmissionReject AdmissionDecisionKind = "reject"
)

// AdmissionPublicationState 是发布记录的状态。
// AdmissionPublicationState is the publication record's state.
type AdmissionPublicationState string

const (
	// AdmissionPublicationActive 表示本次发布仍是该候选的有效发布。
	AdmissionPublicationActive AdmissionPublicationState = "active"
	// AdmissionPublicationWithdrawn 表示本次发布已撤回（版本历史保留）。
	AdmissionPublicationWithdrawn AdmissionPublicationState = "withdrawn"
)

// 撤回原因分类（契约固定字面量）。
// Withdrawal reason literals frozen by the contract.
const (
	// AdmissionWithdrawRestoredPrevious 表示上一版内容已作为新版本重新发布。
	AdmissionWithdrawRestoredPrevious = "restored_previous"
	// AdmissionWithdrawSuperseded 表示指针已被后续发布超越，只记录撤回。
	AdmissionWithdrawSuperseded = "superseded"
	// AdmissionWithdrawDeleted 表示技能已删除，只记录撤回。
	AdmissionWithdrawDeleted = "deleted"
)

// 准入的七个写动作与目标类型（`run-and-errors.md` §9.3 追加）。
// The seven admission write actions and the target kind (appended to §9.3).
const (
	// ActionSkillAdmissionPropose 是提议候选。
	ActionSkillAdmissionPropose = "skill.admission.propose"
	// ActionSkillAdmissionEvaluate 是确定性评估。
	ActionSkillAdmissionEvaluate = "skill.admission.evaluate"
	// ActionSkillAdmissionApprove 是本机批准。
	ActionSkillAdmissionApprove = "skill.admission.approve"
	// ActionSkillAdmissionReject 是本机拒绝。
	ActionSkillAdmissionReject = "skill.admission.reject"
	// ActionSkillAdmissionCancel 是取消候选。
	ActionSkillAdmissionCancel = "skill.admission.cancel"
	// ActionSkillAdmissionPublish 是受控发布。
	ActionSkillAdmissionPublish = "skill.admission.publish"
	// ActionSkillAdmissionWithdraw 是撤回发布。
	ActionSkillAdmissionWithdraw = "skill.admission.withdraw"
	// TargetKindSkillAdmission 是准入操作的目标类型。
	TargetKindSkillAdmission = "skill_admission"
)

// AdmissionCandidate 是一次准入提议的冻结快照（数据库行视图）。
// AdmissionCandidate is the frozen snapshot of one admission proposal.
type AdmissionCandidate struct {
	CandidateID         string
	SkillName           string
	State               AdmissionState
	Revision            int64
	CandidateHash       string
	SourceHash          string
	ManifestJSON        string
	SourceJSON          string
	BaselineJSON        string
	ProposerSubject     string
	ProposerAuthMethod  string
	ProposerSource      string
	RiskTier            AdmissionRiskTier
	TerminalReason      string
	TerminalOperationID string
	OperationID         string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// AdmissionDimension 是单个维度的判定与理由。
// AdmissionDimension is one dimension's verdict and reason.
type AdmissionDimension struct {
	Name   AdmissionDimensionName   `json:"name"`
	Status AdmissionDimensionStatus `json:"status"`
	Reason string                   `json:"reason,omitempty"`
}

// AdmissionScriptReview 是一条人工静态审查证据：Path 必须是候选内脚本的相对路径，
// Evidence 记录审查了什么（权限面、外部命令、文件 / 网络访问），不得为空。
// AdmissionScriptReview is one human static-review record; the evidence is mandatory.
type AdmissionScriptReview struct {
	Path     string `json:"path"`
	Evidence string `json:"evidence"`
}

// AdmissionEvaluation 是固定评分器对候选的七维判定与基线证据。
// AdmissionEvaluation is the fixed grader's seven-dimension verdict and baseline.
type AdmissionEvaluation struct {
	EvaluationID      string
	CandidateID       string
	CandidateRevision int64
	CandidateHash     string
	EvaluationHash    string
	GraderVersion     string
	PolicyHash        string
	Dimensions        []AdmissionDimension
	// Scripts 是评估列出的候选内脚本 / 可执行文件相对路径（含即 permissions=not_evaluable）；
	// 批准时的 script_review 必须逐条覆盖，不能只勾选 permissions。
	Scripts      []string
	Aggregate    AdmissionAggregate
	RiskTier     AdmissionRiskTier
	Referrers    []string
	BaselineJSON string
	EvaluatedAt  time.Time
	OperationID  string
}

// AdmissionDecision 是本机管理员的一次批准或拒绝。
// AdmissionDecision is one local approve/reject decision.
type AdmissionDecision struct {
	DecisionID         string
	CandidateID        string
	CandidateRevision  int64
	CandidateHash      string
	EvaluationID       string
	EvaluationHash     string
	Decision           AdmissionDecisionKind
	Reason             string
	Acknowledged       []AdmissionDimensionName
	ApprovedScopeJSON  string
	ScriptReviewJSON   string
	BaselineJSON       string
	RiskTier           AdmissionRiskTier
	ApproverSubject    string
	ApproverAuthMethod string
	ApproverSource     string
	ExpiresAt          time.Time
	SupersededAt       time.Time
	SupersededBy       string
	CreatedAt          time.Time
	OperationID        string
}

// AdmissionPublication 记录候选版本进入既有技能版本与发布指针的那次事务。
// AdmissionPublication records the transaction that moved the skill pointers.
type AdmissionPublication struct {
	CandidateID         string
	DecisionID          string
	SkillName           string
	SkillUID            ResourceUID
	VersionID           int64
	VersionNo           int64
	ContentHash         string
	BaselineJSON        string
	State               AdmissionPublicationState
	PublishedAt         time.Time
	WithdrawnAt         time.Time
	WithdrawOperationID string
	WithdrawReason      string
	WithdrawVersionID   int64
	OperationID         string
}

// AdmissionBaseline 是评估 / 批准 / 发布共同绑定的技能指针与引用基线。
// AdmissionBaseline freezes the skill pointer state and referrers for binding.
type AdmissionBaseline struct {
	SkillState         string
	ResourceUID        ResourceUID
	PublishedVersionID int64
	VersionNo          int
	ContentHash        string
	PointerRevision    int64
	Referrers          []string
	ReferrersHash      string
	CanonicalJSON      string
}

// AdmissionSource 是管理员提交的来源声明（严格 JSON 解析后的形态）。
// AdmissionSource is the administrator's source declaration after strict parsing.
type AdmissionSource struct {
	Kind            string   `json:"kind"`
	Ref             string   `json:"ref"`
	TaskOutcome     string   `json:"task_outcome"`
	Confirmation    string   `json:"confirmation"`
	Authorization   string   `json:"authorization"`
	Redaction       string   `json:"redaction"`
	RetentionUntil  string   `json:"retention_until"`
	Applicable      bool     `json:"applicable"`
	Counterexamples []string `json:"counterexamples"`
	Evidence        []string `json:"evidence"`
	Limitations     []string `json:"limitations"`
	Notes           string   `json:"notes"`
}

// AdmissionFileDigest 是候选清单中的一条文件摘要。
// AdmissionFileDigest is one file digest in the candidate manifest.
type AdmissionFileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// AdmissionApprovedScope 是批准范围：技能名、风险档、文件与引用者。
// AdmissionApprovedScope is the approved scope bound into the decision.
type AdmissionApprovedScope struct {
	SkillName string                `json:"skill"`
	RiskTier  AdmissionRiskTier     `json:"risk_tier"`
	Files     []AdmissionFileDigest `json:"files"`
	Referrers []string              `json:"referrers"`
}

// AdmissionPolicyConfig 是装配层收集的运行期安全面（不含数据库两表）。
// AdmissionPolicyConfig is the runtime safety surface collected by the assembly layer.
type AdmissionPolicyConfig struct {
	CrossAgentRead  []string
	AllowedCommands []string
	Interpreters    []string
	// ProgramDirNames 是安装清单的可执行名，与 tool.PolicyOptions 同源。
	ProgramDirNames  []string
	AllowRunCode     bool
	IsolationBackend string
	IsolationTools   map[string]string
	// IsolationProfiles 是 isolation.profiles.* 的有效档位（名字 + 执行边界）。
	// IsolationProfiles carries every effective isolation.profiles.* entry.
	IsolationProfiles []AdmissionIsolationProfile
	ExecutionProfile  string
}

// AdmissionIsolationProfile 是一个隔离档位的策略哈希输入：名字与全部执行边界。
// 评估 / 批准后修改同名档位的镜像、内存、PID 或超时，策略哈希随之改变，旧批准
// 在发布时被拒绝（ErrAdmissionStalePolicy）。
// AdmissionIsolationProfile is one isolation profile's hash input: its name and every
// execution boundary; editing a same-named profile changes the policy hash.
type AdmissionIsolationProfile struct {
	Name           string  `json:"name"`
	Runtime        string  `json:"runtime"`
	Image          string  `json:"image"`
	CPUs           float64 `json:"cpus"`
	MemoryMB       int     `json:"memory_mb"`
	PIDs           int     `json:"pids"`
	TmpfsMB        int     `json:"tmpfs_mb"`
	OutputTmpfsMB  int     `json:"output_tmpfs_mb"`
	MaxOutputBytes int     `json:"max_output_bytes"`
	TimeoutSeconds int     `json:"timeout_seconds"`
}

// AdmissionDomainPolicy 是一行权威域名策略的哈希输入（scope + 修订 + 排序域名）。
// AdmissionDomainPolicy is one authoritative domain-policy row's hash input.
type AdmissionDomainPolicy struct {
	Scope    string   `json:"scope"`
	Revision int64    `json:"revision"`
	Domains  []string `json:"domains"`
}

// AdmissionNetworkException 是一条生效中的网络访问例外。
// AdmissionNetworkException is one live network exception.
type AdmissionNetworkException struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// AdmissionPolicy 是评估 / 批准 / 发布共同绑定的策略快照。
// AdmissionPolicy is the policy snapshot bound by evaluate, approve and publish.
type AdmissionPolicy struct {
	Hash              string
	CrossAgentRead    []string
	AllowedCommands   []string
	Interpreters      []string
	DomainPolicies    []AdmissionDomainPolicy
	AllowRunCode      bool
	IsolationBackend  string
	IsolationTools    map[string]string
	IsolationProfiles []AdmissionIsolationProfile
	ExecutionProfile  string
	ExecutionRevision int64
	NetworkExceptions []AdmissionNetworkException
}

// AdmissionFilter 是候选列表的过滤条件；State 只接受存储态或派生态 `expired`。
// AdmissionFilter filters candidate listings; State accepts stored states or expired.
type AdmissionFilter struct {
	State AdmissionState
	Skill string
	Limit int
}

// AdmissionShowOptions 控制详情里是否附带文件与 diff。
// AdmissionShowOptions controls whether the detail view includes files and a diff.
type AdmissionShowOptions struct {
	IncludeFiles bool
	IncludeDiff  bool
}

// AdmissionDetail 是只读详情视图（管理台与 CLI show 共用）。
// AdmissionDetail is the read-only detail view shared by the console and CLI show.
type AdmissionDetail struct {
	Candidate        AdmissionCandidate
	Evaluation       *AdmissionEvaluation
	Decision         *AdmissionDecision
	Publication      *AdmissionPublication
	Source           AdmissionSource
	Files            []AuthorFile
	DecisionTemplate string
	Diff             string
}

// AdmissionProposeRequest 是一次提议请求（文件来自目录读取）。
// AdmissionProposeRequest is one propose request whose files came from a directory.
type AdmissionProposeRequest struct {
	SkillName  string
	Files      []AuthorFile
	SourceJSON []byte
}

// AdmissionPublishRequest 是发布事务的输入；Version 由编排层在补偿可回滚后填充。
// AdmissionPublishRequest feeds the publish transaction; Version is filled by the
// orchestration layer once compensation is armed.
type AdmissionPublishRequest struct {
	CandidateID       string
	CandidateRevision int64
	DecisionID        string
	Version           ResourceVersion
	Baseline          AdmissionBaseline
	OperationID       string
}

// AdmissionWithdrawRequest 是撤回事务的输入；WithdrawVersion 只在需要移动指针时非空。
// AdmissionWithdrawRequest feeds the withdraw transaction; WithdrawVersion is set only
// when the pointer must move.
type AdmissionWithdrawRequest struct {
	CandidateID       string
	CandidateRevision int64
	Publication       AdmissionPublication
	WithdrawVersion   *ResourceVersion
	Reason            string
	OperationID       string
}

// 错误哨兵：全部可 errors.Is；消息面只给固定短语，不回显凭证或正文。
// Error sentinels are all errors.Is-able; messages never echo secrets or bodies.
var (
	// ErrAdmissionNotFound 表示候选 / 评估 / 决策 / 发布记录不存在。
	ErrAdmissionNotFound = errors.New("admission not found")
	// ErrAdmissionInvalidState 表示当前状态不允许该动作（CAS 未命中）。
	ErrAdmissionInvalidState = errors.New("admission invalid state")
	// ErrAdmissionEvaluationFailed 表示评估聚合失败，不能批准。
	ErrAdmissionEvaluationFailed = errors.New("admission evaluation failed")
	// ErrAdmissionStaleBaseline 表示技能指针基线已变化。
	ErrAdmissionStaleBaseline = errors.New("admission stale baseline")
	// ErrAdmissionStalePolicy 表示策略哈希已变化。
	ErrAdmissionStalePolicy = errors.New("admission stale policy")
	// ErrAdmissionRiskEscalated 表示风险档在评估后升高。
	ErrAdmissionRiskEscalated = errors.New("admission risk escalated")
	// ErrAdmissionApprovalExpired 表示批准已过期。
	ErrAdmissionApprovalExpired = errors.New("admission approval expired")
	// ErrAdmissionExpiryInvalid 表示有效期不在 (now, now+24h]。
	ErrAdmissionExpiryInvalid = errors.New("admission expiry invalid")
	// ErrAdmissionSeparationRequired 表示高风险候选的提出者与批准者相同。
	ErrAdmissionSeparationRequired = errors.New("admission separation required")
	// ErrAdmissionUnknownApprover 表示无法确定审批人身份。
	ErrAdmissionUnknownApprover = errors.New("admission unknown approver")
	// ErrAdmissionUntrustedActor 表示主体不是可信本机 CLI。
	ErrAdmissionUntrustedActor = errors.New("admission untrusted actor")
	// ErrAdmissionInvalidInput 表示请求字段非法（结构或取值）。
	ErrAdmissionInvalidInput = errors.New("admission invalid input")
	// ErrAdmissionCandidateMismatch 表示候选哈希与冻结内容不一致。
	ErrAdmissionCandidateMismatch = errors.New("admission candidate mismatch")
	// ErrAdmissionCandidateMissing 表示候选目录 / 清单 / 来源文件缺失。
	ErrAdmissionCandidateMissing = errors.New("admission candidate missing")
	// ErrAdmissionEvaluationMismatch 表示决策绑定的评估与当前记录不一致。
	ErrAdmissionEvaluationMismatch = errors.New("admission evaluation mismatch")
	// ErrAdmissionSourceMismatch 表示来源文件在评估后被改动。
	ErrAdmissionSourceMismatch = errors.New("admission source mismatch")
	// ErrAdmissionSecretDetected 表示文本命中凭证模式。
	ErrAdmissionSecretDetected = errors.New("admission secret detected")
	// ErrAdmissionAcknowledgmentMismatch 表示确认集合不是允许集合的精确值。
	ErrAdmissionAcknowledgmentMismatch = errors.New("admission acknowledgment mismatch")
	// ErrAdmissionDimensionBlocked 表示存在不可确认的阻止维度。
	ErrAdmissionDimensionBlocked = errors.New("admission dimension blocked")
	// ErrAdmissionScriptReviewRequired 表示含脚本但没有逐条静态审查证据。
	ErrAdmissionScriptReviewRequired = errors.New("admission script review required")
	// ErrAdmissionRetentionExpired 表示来源材料可用期限已过。
	ErrAdmissionRetentionExpired = errors.New("admission retention expired")
	// ErrAdmissionFirstVersionWithdraw 表示首版撤回到空被拒绝。
	ErrAdmissionFirstVersionWithdraw = errors.New("admission first version withdrawal")
	// ErrAdmissionActiveCandidateExists 表示同名活跃候选已存在。
	ErrAdmissionActiveCandidateExists = errors.New("admission active candidate exists")
	// ErrAdmissionCommitUnknown 表示发布 / 撤回事务的提交返回错误：提交是否生效
	// 未知。调用方不得回滚磁盘（恢复工作副本、删除快照），只能把操作记 unknown
	// 并交给恢复路径查证；盲目补偿可能破坏已经生效的版本。
	// ErrAdmissionCommitUnknown marks a commit whose outcome is unknown; callers must
	// not compensate on disk and must record the operation as unknown instead.
	ErrAdmissionCommitUnknown = errors.New("admission commit outcome unknown")
	// ErrAdmissionCompensationUncertain 表示撤回准备失败的补偿本身也失败（工作副本
	// 恢复或快照清理），磁盘状态不确定；操作必须记 unknown 而非 failed。
	// ErrAdmissionCompensationUncertain marks a failed compensation whose on-disk
	// state is uncertain; the operation must be recorded as unknown, not failed.
	ErrAdmissionCompensationUncertain = errors.New("admission compensation uncertain")
)

// NewAdmissionCandidateID 生成 `adm_` + 26 位 Crockford Base32。
// NewAdmissionCandidateID mints an admission candidate id.
func NewAdmissionCandidateID() (string, error) { return newUID("adm_") }

// NewAdmissionEvaluationID 生成 `eval_` + 26 位 Crockford Base32。
// NewAdmissionEvaluationID mints an evaluation id.
func NewAdmissionEvaluationID() (string, error) { return newUID("eval_") }

// NewAdmissionDecisionID 生成 `dec_` + 26 位 Crockford Base32。
// NewAdmissionDecisionID mints a decision id.
func NewAdmissionDecisionID() (string, error) { return newUID("dec_") }

// ParseAdmissionID 校验前缀与载荷（与资源 UID 同一套 Crockford 规则）。
// ParseAdmissionID validates a prefixed admission id with the same Crockford rules.
func ParseAdmissionID(prefix, raw string) (string, error) {
	payload, err := splitUID(raw, prefix)
	if err != nil {
		return "", fmt.Errorf("准入标识 %q 非法: %w", raw, err)
	}
	return prefix + payload, nil
}

// CanEvaluate 报告该状态是否允许发起评估。
// CanEvaluate reports whether the state accepts a new evaluation.
func (s AdmissionState) CanEvaluate() bool {
	return s == AdmissionProposed || s == AdmissionEvaluated || s == AdmissionApproved
}

// Terminal 报告该状态是否终态（不再接受状态推进）。
// Terminal reports whether the state is terminal.
func (s AdmissionState) Terminal() bool {
	return s == AdmissionRejected || s == AdmissionCanceled || s == AdmissionWithdrawn
}

// normOrdinal 复制一份去重并排序的字符串切片（调用方不得依赖输入顺序）。
// 复用包内已有的 sortStrings，避免第二套排序实现。
// normOrdinal returns a sorted, de-duplicated copy without mutating the input.
func normOrdinal(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sortStrings(out)
	return out
}
