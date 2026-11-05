package core

// 本文件是准入契约的严格 JSON 解析与哈希 / 规范化函数（颗粒度文档 US-21 §3.1.2）。
// 解析器只做结构与枚举校验：文件元信息（普通文件、软链接、64 KiB）由
// internal/workspace 的两个读取器执行，凭证扫描在 internal/app。
// This file holds the strict JSON parsing and hashing helpers of the admission
// contract. Parsers validate structure and enums only; file metadata checks live in
// internal/workspace and secret scanning in internal/app.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// admissionJSONMaxBytes 是来源 / 决策 / 清单文件共用的体积上限。
	// admissionJSONMaxBytes bounds every admission JSON document.
	admissionJSONMaxBytes = 64 << 10
	// admissionListMaxItems 是反例 / 证据 / 局限三个列表的项数上限。
	admissionListMaxItems = 20
	// admissionListMaxRunes 是列表单条的字符上限。
	admissionListMaxRunes = 500
	// admissionTextMaxRunes 是理由 / 说明 / 审查证据的字符上限（与 DDL 的 char_length 同口径）。
	admissionTextMaxRunes = 2000
	// admissionRefMaxRunes 是来源引用文本的字符上限。
	admissionRefMaxRunes = 500
)

var admissionHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AdmissionDecisionInput 是决策文件的严格解析结果；字段与 JSON 键的对应关系见
// docs/contracts/skill-admission.md。批准者主体**不在**这里——只能来自 OS 身份。
// AdmissionDecisionInput is the parsed decision file; the approver subject is absent by
// design because it can only come from the local OS identity.
type AdmissionDecisionInput struct {
	CandidateID       string
	CandidateRevision int64
	CandidateHash     string
	EvaluationID      string
	EvaluationHash    string
	Decision          AdmissionDecisionKind
	Reason            string
	Acknowledged      []AdmissionDimensionName
	ApprovedScope     *AdmissionApprovedScope
	ScriptReview      []AdmissionScriptReview
	Baseline          *AdmissionBaseline
	ExpiresAt         time.Time
}

// admissionSourceKinds / admissionTaskOutcomes 等是来源声明的固定枚举面。
// The following slices are the frozen enum surfaces of the source declaration.
var (
	admissionSourceKinds    = []string{"manual", "run", "import", "model_proposal"}
	admissionTaskOutcomes   = []string{"completed", "failed", "unknown", "not_applicable"}
	admissionConfirmations  = []string{"direct", "user_confirmed", "model_inferred", "relayed"}
	admissionAuthorizations = []string{"declared", "not_required", "unknown"}
	admissionRedactions     = []string{"declared", "not_needed", "unknown"}
	admissionRiskTiers      = []string{string(AdmissionRiskNormal), string(AdmissionRiskHigh)}
	admissionSkillStates    = []string{AdmissionSkillPresent, AdmissionSkillDeleted, AdmissionSkillAbsent}
	admissionDecisionKinds  = []string{string(AdmissionApprove), string(AdmissionReject)}
)

// admissionDimensionNames 是七个维度的固定枚举（顺序即报告顺序）。
// admissionDimensionNames is the fixed dimension enumeration in report order.
var admissionDimensionNames = []AdmissionDimensionName{
	AdmissionDimensionContent,
	AdmissionDimensionSource,
	AdmissionDimensionPermissions,
	AdmissionDimensionSafety,
	AdmissionDimensionRegression,
	AdmissionDimensionEffect,
	AdmissionDimensionCost,
}

// admissionAcknowledgeableOrder 是**唯一**可确认维度集合（顺序固定，供稳定序列化）。
// admissionAcknowledgeableOrder is the only confirmable dimension set.
var admissionAcknowledgeableOrder = []AdmissionDimensionName{
	AdmissionDimensionEffect,
	AdmissionDimensionCost,
	AdmissionDimensionRegression,
}

// admissionSourceWire 用指针区分"缺省"与"显式零值"；枚举与上限校验在 ParseAdmissionSource。
// admissionSourceWire distinguishes absent fields from explicit zero values.
type admissionSourceWire struct {
	Kind            *string  `json:"kind"`
	Ref             *string  `json:"ref"`
	TaskOutcome     *string  `json:"task_outcome"`
	Confirmation    *string  `json:"confirmation"`
	Authorization   *string  `json:"authorization"`
	Redaction       *string  `json:"redaction"`
	RetentionUntil  *string  `json:"retention_until"`
	Applicable      *bool    `json:"applicable"`
	Counterexamples []string `json:"counterexamples"`
	Evidence        []string `json:"evidence"`
	Limitations     []string `json:"limitations"`
	Notes           *string  `json:"notes"`
}

// admissionBaselineWire 是决策文件里 `baseline` 的严格形态；`canonical_json` **不**是输入
// （派生值），由 AdmissionBaselineCanonical 复算，防止决策文件自带"证据"。
// admissionBaselineWire is the strict baseline shape; canonical_json is derived, not input.
type admissionBaselineWire struct {
	SkillState         string   `json:"skill_state"`
	ResourceUID        string   `json:"resource_uid"`
	PublishedVersionID int64    `json:"published_version_id"`
	VersionNo          int      `json:"version_no"`
	ContentHash        string   `json:"content_hash"`
	PointerRevision    int64    `json:"pointer_revision"`
	Referrers          []string `json:"referrers"`
	ReferrersHash      string   `json:"referrers_hash"`
}

// admissionDecisionWire 是决策文件的严格形态；全部字段用指针，才能拒绝 approve 缺项
// 与 reject 多写两件事，而不是把缺省值当默认接受。
// admissionDecisionWire makes every field a pointer so missing approve fields and extra
// reject fields are both rejected instead of defaulted.
type admissionDecisionWire struct {
	CandidateID            *string                 `json:"candidate_id"`
	CandidateRevision      *int64                  `json:"candidate_revision"`
	CandidateHash          *string                 `json:"candidate_hash"`
	EvaluationID           *string                 `json:"evaluation_id"`
	EvaluationHash         *string                 `json:"evaluation_hash"`
	Decision               *string                 `json:"decision"`
	Reason                 *string                 `json:"reason"`
	AcknowledgedDimensions []string                `json:"acknowledged_dimensions"`
	ApprovedScope          *AdmissionApprovedScope `json:"approved_scope"`
	ScriptReview           []AdmissionScriptReview `json:"script_review"`
	Baseline               *admissionBaselineWire  `json:"baseline"`
	ExpiresAt              *string                 `json:"expires_at"`
}

// admissionDecisionTemplateJSON 是 `show --decision-template` 的输出形态；字段序固定，
// 便于管理员按结构填写。空字符串字段是**故意**留白的必填项，未填时解析必然拒绝。
// admissionDecisionTemplateJSON is the template shape; blank required fields are
// deliberate so an unfilled template cannot be approved.
type admissionDecisionTemplateJSON struct {
	CandidateID            string                   `json:"candidate_id"`
	CandidateRevision      int64                    `json:"candidate_revision"`
	CandidateHash          string                   `json:"candidate_hash"`
	EvaluationID           string                   `json:"evaluation_id"`
	EvaluationHash         string                   `json:"evaluation_hash"`
	Decision               string                   `json:"decision"`
	Reason                 string                   `json:"reason"`
	AcknowledgedDimensions []AdmissionDimensionName `json:"acknowledged_dimensions"`
	ApprovedScope          *AdmissionApprovedScope  `json:"approved_scope"`
	ScriptReview           []AdmissionScriptReview  `json:"script_review"`
	Baseline               json.RawMessage          `json:"baseline"`
	ExpiresAt              string                   `json:"expires_at"`
}

// admissionScopeCanonical 是批准范围的固定序列化形态（文件按 path、引用者按字典序）。
// admissionScopeCanonical fixes the serialisation order of the approved scope.
type admissionScopeCanonical struct {
	SkillName string                `json:"skill"`
	RiskTier  AdmissionRiskTier     `json:"risk_tier"`
	Files     []AdmissionFileDigest `json:"files"`
	Referrers []string              `json:"referrers"`
}

// admissionBaselineCanonical 是基线的固定序列化形态（字段序即契约序）。
// admissionBaselineCanonical fixes the baseline field order.
type admissionBaselineCanonicalJSON struct {
	SkillState         string   `json:"skill_state"`
	ResourceUID        string   `json:"resource_uid"`
	PublishedVersionID int64    `json:"published_version_id"`
	VersionNo          int      `json:"version_no"`
	ContentHash        string   `json:"content_hash"`
	PointerRevision    int64    `json:"pointer_revision"`
	Referrers          []string `json:"referrers"`
	ReferrersHash      string   `json:"referrers_hash"`
}

// admissionEvaluationHashInput 是评估哈希的固定输入面；评估哈希本身**不**入哈希
// （自引用），时间统一折算成 Unix 毫秒，避免数据库毫秒精度把复算结果改掉。
// admissionEvaluationHashInput is the fixed hash input; the hash never feeds on itself
// and timestamps are normalised to Unix milliseconds.
type admissionEvaluationHashInput struct {
	EvaluationID      string               `json:"evaluation_id"`
	CandidateID       string               `json:"candidate_id"`
	CandidateRevision int64                `json:"candidate_revision"`
	CandidateHash     string               `json:"candidate_hash"`
	GraderVersion     string               `json:"grader_version"`
	PolicyHash        string               `json:"policy_hash"`
	Dimensions        []AdmissionDimension `json:"dimensions"`
	Scripts           []string             `json:"scripts"`
	Aggregate         AdmissionAggregate   `json:"aggregate"`
	RiskTier          AdmissionRiskTier    `json:"risk_tier"`
	Referrers         []string             `json:"referrers"`
	BaselineJSON      string               `json:"baseline_json"`
	EvaluatedAtMS     int64                `json:"evaluated_at_ms"`
	OperationID       string               `json:"operation_id"`
}

// decodeAdmissionJSONValue 是准入文件**唯一**的严格解码入口：体积、UTF-8、顶层形态、
// 未知键与尾随内容。`root` 是期望的 JSON 顶层定界符（`{` 或 `[`）。
// decodeAdmissionJSONValue is the single strict decoder for admission documents.
func decodeAdmissionJSONValue(raw []byte, root byte, out any) error {
	if len(raw) == 0 || len(raw) > admissionJSONMaxBytes {
		return fmt.Errorf("%w: JSON 体积 %d 字节超出上限 %d",
			ErrAdmissionInvalidInput, len(raw), admissionJSONMaxBytes)
	}
	if !utf8.Valid(raw) {
		return fmt.Errorf("%w: JSON 不是合法的 UTF-8", ErrAdmissionInvalidInput)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != root {
		return fmt.Errorf("%w: JSON 顶层必须是 %q", ErrAdmissionInvalidInput, string(root))
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		// Go 的 decoder 消息只含字段名或单个字符，不含字段值，故可随错误上抛。
		// Decoder messages carry a field name or a single character, never a value.
		return fmt.Errorf("%w: JSON 字段非法或含未知键: %w", ErrAdmissionInvalidInput, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: JSON 含尾随内容", ErrAdmissionInvalidInput)
	}
	return nil
}

// admissionRequiredString 校验必填字符串字段：存在、非空、不超字符上限。
// admissionRequiredString validates a required, bounded, non-empty string field.
func admissionRequiredString(name string, value *string, maxRunes int) (string, error) {
	if value == nil {
		return "", fmt.Errorf("%w: 缺少必填字段 %s", ErrAdmissionInvalidInput, name)
	}
	if *value == "" {
		return "", fmt.Errorf("%w: 字段 %s 不得为空", ErrAdmissionInvalidInput, name)
	}
	if utf8.RuneCountInString(*value) > maxRunes {
		return "", fmt.Errorf("%w: 字段 %s 超过 %d 字符", ErrAdmissionInvalidInput, name, maxRunes)
	}
	return *value, nil
}

// admissionRequiredEnum 校验必填枚举字段（不回声取值，避免把误填的敏感串带进日志）。
// admissionRequiredEnum validates a required enum without echoing the value.
func admissionRequiredEnum(name string, value *string, allowed []string) (string, error) {
	raw, err := admissionRequiredString(name, value, admissionRefMaxRunes)
	if err != nil {
		return "", err
	}
	if !slices.Contains(allowed, raw) {
		return "", fmt.Errorf("%w: 字段 %s 的取值不在允许集合内", ErrAdmissionInvalidInput, name)
	}
	return raw, nil
}

// admissionStringList 校验列表字段的项数与单条字符上限。
// admissionStringList bounds a list field by item count and per-item runes.
func admissionStringList(name string, values []string) ([]string, error) {
	if len(values) > admissionListMaxItems {
		return nil, fmt.Errorf("%w: 字段 %s 超过 %d 项", ErrAdmissionInvalidInput, name, admissionListMaxItems)
	}
	for _, value := range values {
		if utf8.RuneCountInString(value) > admissionListMaxRunes {
			return nil, fmt.Errorf("%w: 字段 %s 的单项超过 %d 字符",
				ErrAdmissionInvalidInput, name, admissionListMaxRunes)
		}
	}
	return values, nil
}

// admissionHashValid 报告字符串是否为 64 位小写十六进制 SHA-256。
// admissionHashValid reports whether the string is a lowercase 64-hex digest.
func admissionHashValid(value string) bool { return admissionHashPattern.MatchString(value) }

// ParseAdmissionSource 严格解析来源声明；只校验结构与枚举，语义判定留给评估。
// ParseAdmissionSource strictly parses a source declaration; semantic verdicts belong
// to the evaluator.
func ParseAdmissionSource(raw []byte) (AdmissionSource, error) {
	var wire admissionSourceWire
	if err := decodeAdmissionJSONValue(raw, '{', &wire); err != nil {
		return AdmissionSource{}, err
	}
	kind, err := admissionRequiredEnum("kind", wire.Kind, admissionSourceKinds)
	if err != nil {
		return AdmissionSource{}, err
	}
	ref, err := admissionRequiredString("ref", wire.Ref, admissionRefMaxRunes)
	if err != nil {
		return AdmissionSource{}, err
	}
	outcome, err := admissionRequiredEnum("task_outcome", wire.TaskOutcome, admissionTaskOutcomes)
	if err != nil {
		return AdmissionSource{}, err
	}
	confirmation, err := admissionRequiredEnum("confirmation", wire.Confirmation, admissionConfirmations)
	if err != nil {
		return AdmissionSource{}, err
	}
	authorization, err := admissionRequiredEnum("authorization", wire.Authorization, admissionAuthorizations)
	if err != nil {
		return AdmissionSource{}, err
	}
	redaction, err := admissionRequiredEnum("redaction", wire.Redaction, admissionRedactions)
	if err != nil {
		return AdmissionSource{}, err
	}
	if wire.Applicable == nil {
		return AdmissionSource{}, fmt.Errorf("%w: 缺少必填字段 applicable", ErrAdmissionInvalidInput)
	}
	retention := ""
	if wire.RetentionUntil != nil {
		retention = *wire.RetentionUntil
		if retention != "" {
			if _, err := time.Parse(time.RFC3339, retention); err != nil {
				return AdmissionSource{}, fmt.Errorf("%w: 字段 retention_until 不是 RFC3339",
					ErrAdmissionInvalidInput)
			}
		}
	}
	notes := ""
	if wire.Notes != nil {
		notes = *wire.Notes
		if utf8.RuneCountInString(notes) > admissionTextMaxRunes {
			return AdmissionSource{}, fmt.Errorf("%w: 字段 notes 超过 %d 字符",
				ErrAdmissionInvalidInput, admissionTextMaxRunes)
		}
	}
	counterexamples, err := admissionStringList("counterexamples", wire.Counterexamples)
	if err != nil {
		return AdmissionSource{}, err
	}
	evidence, err := admissionStringList("evidence", wire.Evidence)
	if err != nil {
		return AdmissionSource{}, err
	}
	limitations, err := admissionStringList("limitations", wire.Limitations)
	if err != nil {
		return AdmissionSource{}, err
	}
	return AdmissionSource{
		Kind:            kind,
		Ref:             ref,
		TaskOutcome:     outcome,
		Confirmation:    confirmation,
		Authorization:   authorization,
		Redaction:       redaction,
		RetentionUntil:  retention,
		Applicable:      *wire.Applicable,
		Counterexamples: counterexamples,
		Evidence:        evidence,
		Limitations:     limitations,
		Notes:           notes,
	}, nil
}

// ParseAdmissionDecision 严格解析决策文件；批准范围、基线、有效期三者只在 approve
// 允许出现，reject 多写即拒。有效期窗口（≤24h）由应用层按当前时钟判定。
// ParseAdmissionDecision strictly parses a decision file; the 24h window is enforced by
// the application layer against the current clock.
func ParseAdmissionDecision(raw []byte) (AdmissionDecisionInput, error) {
	var wire admissionDecisionWire
	if err := decodeAdmissionJSONValue(raw, '{', &wire); err != nil {
		return AdmissionDecisionInput{}, err
	}
	candidateID, err := admissionIDField("candidate_id", wire.CandidateID, "adm_")
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	if wire.CandidateRevision == nil || *wire.CandidateRevision <= 0 {
		return AdmissionDecisionInput{}, fmt.Errorf("%w: 字段 candidate_revision 必须是正整数",
			ErrAdmissionInvalidInput)
	}
	candidateHash, err := admissionHashField("candidate_hash", wire.CandidateHash)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	evaluationID, err := admissionIDField("evaluation_id", wire.EvaluationID, "eval_")
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	evaluationHash, err := admissionHashField("evaluation_hash", wire.EvaluationHash)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	kind, err := admissionRequiredEnum("decision", wire.Decision, admissionDecisionKinds)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	reason, err := admissionRequiredString("reason", wire.Reason, admissionTextMaxRunes)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	acknowledged, err := admissionAcknowledgedField(wire.AcknowledgedDimensions)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	reviews, err := admissionScriptReviewField(wire.ScriptReview)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	input := AdmissionDecisionInput{
		CandidateID:       candidateID,
		CandidateRevision: *wire.CandidateRevision,
		CandidateHash:     candidateHash,
		EvaluationID:      evaluationID,
		EvaluationHash:    evaluationHash,
		Decision:          AdmissionDecisionKind(kind),
		Reason:            reason,
		Acknowledged:      acknowledged,
		ScriptReview:      reviews,
	}
	if AdmissionDecisionKind(kind) == AdmissionReject {
		if wire.ApprovedScope != nil || wire.Baseline != nil || wire.ScriptReview != nil {
			return AdmissionDecisionInput{}, fmt.Errorf(
				"%w: reject 决策不得携带 approved_scope / baseline / script_review",
				ErrAdmissionInvalidInput)
		}
		return input, nil
	}
	if wire.ApprovedScope == nil {
		return AdmissionDecisionInput{}, fmt.Errorf("%w: approve 决策缺少 approved_scope",
			ErrAdmissionInvalidInput)
	}
	scope, err := admissionApprovedScopeField(wire.ApprovedScope)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	if wire.Baseline == nil {
		return AdmissionDecisionInput{}, fmt.Errorf("%w: approve 决策缺少 baseline",
			ErrAdmissionInvalidInput)
	}
	baseline, err := admissionBaselineField(wire.Baseline)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	expiresAt, err := admissionRequiredString("expires_at", wire.ExpiresAt, admissionRefMaxRunes)
	if err != nil {
		return AdmissionDecisionInput{}, err
	}
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return AdmissionDecisionInput{}, fmt.Errorf("%w: 字段 expires_at 不是 RFC3339",
			ErrAdmissionInvalidInput)
	}
	input.ApprovedScope = scope
	input.Baseline = baseline
	input.ExpiresAt = expiry
	return input, nil
}

// admissionIDField 校验带固定前缀的准入标识字段。
// admissionIDField validates a prefixed admission identifier field.
func admissionIDField(name string, value *string, prefix string) (string, error) {
	raw, err := admissionRequiredString(name, value, admissionRefMaxRunes)
	if err != nil {
		return "", err
	}
	parsed, err := ParseAdmissionID(prefix, raw)
	if err != nil {
		return "", fmt.Errorf("%w: 字段 %s 不是合法的 %s 标识",
			ErrAdmissionInvalidInput, name, prefix)
	}
	return parsed, nil
}

// admissionHashField 校验 64 位小写十六进制摘要字段。
// admissionHashField validates a lowercase 64-hex digest field.
func admissionHashField(name string, value *string) (string, error) {
	raw, err := admissionRequiredString(name, value, 64)
	if err != nil {
		return "", err
	}
	if !admissionHashValid(raw) {
		return "", fmt.Errorf("%w: 字段 %s 不是 64 位小写十六进制",
			ErrAdmissionInvalidInput, name)
	}
	return raw, nil
}

// admissionAcknowledgedField 校验确认集合：只允许七个已知维度名，且不得重复。
// 越界（如勾选 permissions）与集合不匹配由应用层判定，以保留专用哨兵。
// admissionAcknowledgedField keeps unknown names and duplicates out; the whitelist and
// exact-match rules belong to the application boundary check.
func admissionAcknowledgedField(values []string) ([]AdmissionDimensionName, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]AdmissionDimensionName, 0, len(values))
	seen := make(map[AdmissionDimensionName]struct{}, len(values))
	for _, value := range values {
		name := AdmissionDimensionName(value)
		if !slices.Contains(admissionDimensionNames, name) {
			return nil, fmt.Errorf("%w: acknowledged_dimensions 含未知维度", ErrAdmissionInvalidInput)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("%w: acknowledged_dimensions 含重复维度", ErrAdmissionInvalidInput)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// admissionScriptReviewField 校验脚本审查条目：路径非空、证据非空且有上限、路径不重复。
// admissionScriptReviewField validates each review entry and rejects duplicate paths.
func admissionScriptReviewField(reviews []AdmissionScriptReview) ([]AdmissionScriptReview, error) {
	if len(reviews) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(reviews))
	for _, review := range reviews {
		if review.Path == "" {
			return nil, fmt.Errorf("%w: script_review 含空路径", ErrAdmissionInvalidInput)
		}
		if review.Evidence == "" {
			return nil, fmt.Errorf("%w: script_review 的 evidence 不得为空", ErrAdmissionInvalidInput)
		}
		if utf8.RuneCountInString(review.Evidence) > admissionTextMaxRunes {
			return nil, fmt.Errorf("%w: script_review 的 evidence 超过 %d 字符",
				ErrAdmissionInvalidInput, admissionTextMaxRunes)
		}
		if _, dup := seen[review.Path]; dup {
			return nil, fmt.Errorf("%w: script_review 的 path 重复", ErrAdmissionInvalidInput)
		}
		seen[review.Path] = struct{}{}
	}
	return reviews, nil
}

// admissionApprovedScopeField 校验批准范围并返回规范化副本（文件与引用者排序）。
// admissionApprovedScopeField validates the scope and returns a normalised copy.
func admissionApprovedScopeField(scope *AdmissionApprovedScope) (*AdmissionApprovedScope, error) {
	if scope.SkillName == "" {
		return nil, fmt.Errorf("%w: approved_scope.skill 不得为空", ErrAdmissionInvalidInput)
	}
	if !slices.Contains(admissionRiskTiers, string(scope.RiskTier)) {
		return nil, fmt.Errorf("%w: approved_scope.risk_tier 必须是 normal 或 high",
			ErrAdmissionInvalidInput)
	}
	files := make([]AdmissionFileDigest, 0, len(scope.Files))
	seen := make(map[string]struct{}, len(scope.Files))
	for _, file := range scope.Files {
		if file.Path == "" {
			return nil, fmt.Errorf("%w: approved_scope.files 含空路径", ErrAdmissionInvalidInput)
		}
		if !admissionHashValid(file.SHA256) {
			return nil, fmt.Errorf("%w: approved_scope.files 的 sha256 非法", ErrAdmissionInvalidInput)
		}
		if file.Size < 0 {
			return nil, fmt.Errorf("%w: approved_scope.files 的 size 不得为负", ErrAdmissionInvalidInput)
		}
		if _, dup := seen[file.Path]; dup {
			return nil, fmt.Errorf("%w: approved_scope.files 的路径重复", ErrAdmissionInvalidInput)
		}
		seen[file.Path] = struct{}{}
		files = append(files, file)
	}
	slices.SortStableFunc(files, func(a, b AdmissionFileDigest) int {
		return strings.Compare(a.Path, b.Path)
	})
	return &AdmissionApprovedScope{
		SkillName: scope.SkillName,
		RiskTier:  scope.RiskTier,
		Files:     files,
		Referrers: normOrdinal(scope.Referrers),
	}, nil
}

// admissionBaselineField 校验决策携带的基线并复算规范 JSON（拒绝决策文件自带证据）。
// admissionBaselineField validates the baseline and recomputes its canonical JSON.
func admissionBaselineField(wire *admissionBaselineWire) (*AdmissionBaseline, error) {
	if !slices.Contains(admissionSkillStates, wire.SkillState) {
		return nil, fmt.Errorf("%w: baseline.skill_state 取值非法", ErrAdmissionInvalidInput)
	}
	uid := ""
	if wire.ResourceUID != "" {
		parsed, err := ParseAdmissionID("rs_", wire.ResourceUID)
		if err != nil {
			return nil, fmt.Errorf("%w: baseline.resource_uid 非法", ErrAdmissionInvalidInput)
		}
		uid = parsed
	}
	if wire.ContentHash != "" && !admissionHashValid(wire.ContentHash) {
		return nil, fmt.Errorf("%w: baseline.content_hash 非法", ErrAdmissionInvalidInput)
	}
	if wire.PublishedVersionID < 0 || wire.VersionNo < 0 || wire.PointerRevision < 0 {
		return nil, fmt.Errorf("%w: baseline 的版本 / 修订号不得为负", ErrAdmissionInvalidInput)
	}
	if wire.ReferrersHash != "" && !admissionHashValid(wire.ReferrersHash) {
		return nil, fmt.Errorf("%w: baseline.referrers_hash 非法", ErrAdmissionInvalidInput)
	}
	baseline := AdmissionBaseline{
		SkillState:         wire.SkillState,
		ResourceUID:        ResourceUID(uid),
		PublishedVersionID: wire.PublishedVersionID,
		VersionNo:          wire.VersionNo,
		ContentHash:        wire.ContentHash,
		PointerRevision:    wire.PointerRevision,
		Referrers:          normOrdinal(wire.Referrers),
		ReferrersHash:      wire.ReferrersHash,
	}
	baseline.CanonicalJSON = AdmissionBaselineCanonical(baseline)
	return &baseline, nil
}

// ParseAdmissionManifest 严格解析候选清单（`[{"path","sha256","size"}]`）；清单是
// 规范序列化产物，因此要求路径严格升序且不重复——顺序乱掉即视为被改动。
// ParseAdmissionManifest requires the canonical ascending, duplicate-free manifest.
func ParseAdmissionManifest(raw string) ([]AdmissionFileDigest, error) {
	var digests []AdmissionFileDigest
	if err := decodeAdmissionJSONValue([]byte(raw), '[', &digests); err != nil {
		return nil, err
	}
	previous := ""
	for i, digest := range digests {
		if digest.Path == "" {
			return nil, fmt.Errorf("%w: 清单含空路径", ErrAdmissionInvalidInput)
		}
		if !admissionHashValid(digest.SHA256) {
			return nil, fmt.Errorf("%w: 清单第 %d 条的 sha256 非法", ErrAdmissionInvalidInput, i+1)
		}
		if digest.Size < 0 {
			return nil, fmt.Errorf("%w: 清单第 %d 条的 size 不得为负", ErrAdmissionInvalidInput, i+1)
		}
		if i > 0 && strings.Compare(previous, digest.Path) >= 0 {
			return nil, fmt.Errorf("%w: 清单路径必须严格升序且不重复", ErrAdmissionInvalidInput)
		}
		previous = digest.Path
	}
	return digests, nil
}

// AdmissionDecisionTemplate 生成待填写的决策文件模板：候选 / 评估绑定值、批准范围、
// 基线、确认维度与脚本审查条目全部预填；`reason` 与 `expires_at` 故意留空，
// 脚本审查证据故意留空——未真正填写就无法通过解析。
// AdmissionDecisionTemplate prefills everything except the deliberate blanks (reason,
// expires_at, review evidence) so an unfilled template cannot pass parsing.
func AdmissionDecisionTemplate(detail AdmissionDetail) (string, error) {
	evaluation := detail.Evaluation
	if evaluation == nil {
		return "", fmt.Errorf("%w: 候选尚未评估，无法生成决策模板", ErrAdmissionInvalidState)
	}
	if len(evaluation.BaselineJSON) == 0 || !json.Valid([]byte(evaluation.BaselineJSON)) {
		return "", fmt.Errorf("%w: 评估缺少可用的基线 JSON", ErrAdmissionInvalidState)
	}
	files, err := ParseAdmissionManifest(detail.Candidate.ManifestJSON)
	if err != nil {
		return "", err
	}
	scripts := make([]string, len(evaluation.Scripts))
	copy(scripts, evaluation.Scripts)
	slices.Sort(scripts)
	reviews := make([]AdmissionScriptReview, 0, len(scripts))
	for _, path := range scripts {
		reviews = append(reviews, AdmissionScriptReview{Path: path})
	}
	accountable := admissionAcknowledgeableDimensions(*evaluation)
	if accountable == nil {
		accountable = []AdmissionDimensionName{}
	}
	template := admissionDecisionTemplateJSON{
		CandidateID:            detail.Candidate.CandidateID,
		CandidateRevision:      detail.Candidate.Revision,
		CandidateHash:          detail.Candidate.CandidateHash,
		EvaluationID:           evaluation.EvaluationID,
		EvaluationHash:         evaluation.EvaluationHash,
		Decision:               string(AdmissionApprove),
		AcknowledgedDimensions: accountable,
		ApprovedScope: &AdmissionApprovedScope{
			SkillName: detail.Candidate.SkillName,
			RiskTier:  evaluation.RiskTier,
			Files:     files,
			Referrers: normOrdinal(evaluation.Referrers),
		},
		ScriptReview: reviews,
		Baseline:     json.RawMessage(evaluation.BaselineJSON),
	}
	encoded, err := json.MarshalIndent(template, "", "  ")
	if err != nil {
		return "", fmt.Errorf("%w: 决策模板序列化失败", ErrAdmissionInvalidInput)
	}
	return string(encoded) + "\n", nil
}

// admissionAcknowledgeableDimensions 返回"不可评估且属于可确认白名单"的维度
// （顺序固定 effect / cost / regression）。这是**模板预填**口径；批准时的精确匹配
// 判据在 internal/app 的 checkAdmissionApprovalBoundary，两处必须同规则。
// admissionAcknowledgeableDimensions prefills the template; the authoritative match
// lives in internal/app's approval boundary check and must follow the same rule.
func admissionAcknowledgeableDimensions(evaluation AdmissionEvaluation) []AdmissionDimensionName {
	out := make([]AdmissionDimensionName, 0, len(admissionAcknowledgeableOrder))
	for _, name := range admissionAcknowledgeableOrder {
		for _, dimension := range evaluation.Dimensions {
			if dimension.Name == name && dimension.Status == AdmissionDimensionNotEvaluable {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// AdmissionBaselineCanonical 输出基线的规范 JSON：字段序固定、列表排序、紧凑无空白。
// CanonicalJSON 是派生值，**不**参与序列化，避免作者自证。
// AdmissionBaselineCanonical emits the canonical baseline JSON; the derived
// CanonicalJSON field is never part of its own input.
func AdmissionBaselineCanonical(b AdmissionBaseline) string {
	wire := admissionBaselineCanonicalJSON{
		SkillState:         b.SkillState,
		ResourceUID:        string(b.ResourceUID),
		PublishedVersionID: b.PublishedVersionID,
		VersionNo:          b.VersionNo,
		ContentHash:        b.ContentHash,
		PointerRevision:    b.PointerRevision,
		Referrers:          normOrdinal(b.Referrers),
		ReferrersHash:      b.ReferrersHash,
	}
	if wire.Referrers == nil {
		wire.Referrers = []string{}
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		// 结构只含字符串与整数，Marshal 不可能失败；返回空串使上层比对必然失败（fail-closed）。
		// The struct cannot fail to marshal; "" makes every comparison fail closed.
		return ""
	}
	return string(encoded)
}

// AdmissionEvaluationHash 计算评估哈希：绑定候选 / 评估 / 策略 / 基线 / 维度 / 脚本 /
// 风险档 / 引用者 / 时间，同一评估两次读数逐字相同。
// AdmissionEvaluationHash binds the candidate, evaluation, policy, baseline, dimensions,
// scripts, risk tier, referrers and timestamp of one evaluation.
func AdmissionEvaluationHash(e AdmissionEvaluation) string {
	dimensions := make([]AdmissionDimension, len(e.Dimensions))
	copy(dimensions, e.Dimensions)
	slices.SortStableFunc(dimensions, func(a, b AdmissionDimension) int {
		return admissionDimensionRank(a.Name) - admissionDimensionRank(b.Name)
	})
	scripts := make([]string, len(e.Scripts))
	copy(scripts, e.Scripts)
	slices.Sort(scripts)
	wire := admissionEvaluationHashInput{
		EvaluationID:      e.EvaluationID,
		CandidateID:       e.CandidateID,
		CandidateRevision: e.CandidateRevision,
		CandidateHash:     e.CandidateHash,
		GraderVersion:     e.GraderVersion,
		PolicyHash:        e.PolicyHash,
		Dimensions:        dimensions,
		Scripts:           scripts,
		Aggregate:         e.Aggregate,
		RiskTier:          e.RiskTier,
		Referrers:         normOrdinal(e.Referrers),
		BaselineJSON:      e.BaselineJSON,
		EvaluatedAtMS:     e.EvaluatedAt.UTC().UnixMilli(),
		OperationID:       e.OperationID,
	}
	if wire.Scripts == nil {
		wire.Scripts = []string{}
	}
	if wire.Referrers == nil {
		wire.Referrers = []string{}
	}
	if wire.Dimensions == nil {
		wire.Dimensions = []AdmissionDimension{}
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return ""
	}
	return AdmissionSHA256Hex(encoded)
}

// admissionDimensionRank 给七个维度固定排序位；未知维度排在最后，仍保持确定性。
// admissionDimensionRank fixes the order of the seven dimensions.
func admissionDimensionRank(name AdmissionDimensionName) int {
	for index, known := range admissionDimensionNames {
		if known == name {
			return index
		}
	}
	return len(admissionDimensionNames)
}

// AdmissionRequestHash 计算管理操作的 request_hash：对参数做 JSON 数组序列化后取
// SHA-256，长度前缀天然存在（`["ab","c"]` 与 `["a","bc"]` 不混淆）。
// AdmissionRequestHash hashes the parameters as a JSON array, so element boundaries
// cannot be confused.
func AdmissionRequestHash(parts ...string) string {
	if parts == nil {
		parts = []string{}
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	return AdmissionSHA256Hex(encoded)
}

// AdmissionSHA256Hex 返回小写十六进制 SHA-256（准入哈希的统一字节口径）。
// AdmissionSHA256Hex is the single lowercase SHA-256 helper for admission hashes.
func AdmissionSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// AdmissionApprovedScopeJSON 输出批准范围的固定序列化：文件按 path 升序、引用者按
// 字典序；nil 返回空串（调用方对应 reject 的 NULL 列）。
// AdmissionApprovedScopeJSON emits the fixed scope serialisation; nil yields "" for the
// NULL column of a reject decision.
func AdmissionApprovedScopeJSON(scope *AdmissionApprovedScope) string {
	if scope == nil {
		return ""
	}
	files := make([]AdmissionFileDigest, len(scope.Files))
	copy(files, scope.Files)
	slices.SortStableFunc(files, func(a, b AdmissionFileDigest) int {
		return strings.Compare(a.Path, b.Path)
	})
	referrers := normOrdinal(scope.Referrers)
	if referrers == nil {
		referrers = []string{}
	}
	// files 由 make 构造，空集也是非 nil 切片，序列化成 `[]`；不再重复判 nil。
	// `files` comes from make(), so an empty set already serialises as `[]`.
	encoded, err := json.Marshal(admissionScopeCanonical{
		SkillName: scope.SkillName,
		RiskTier:  scope.RiskTier,
		Files:     files,
		Referrers: referrers,
	})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// AdmissionScriptReviewJSON 输出脚本审查条目的固定序列化（按 path 升序）；
// nil 返回 `[]`（approve 无脚本时落库 `[]`，reject 由调用方写 NULL）。
// AdmissionScriptReviewJSON sorts by path and returns "[]" for nil so the approve row
// satisfies the NOT NULL check while a reject row stores NULL.
func AdmissionScriptReviewJSON(reviews []AdmissionScriptReview) string {
	sorted := make([]AdmissionScriptReview, len(reviews))
	copy(sorted, reviews)
	slices.SortStableFunc(sorted, func(a, b AdmissionScriptReview) int {
		return strings.Compare(a.Path, b.Path)
	})
	encoded, err := json.Marshal(sorted)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}
