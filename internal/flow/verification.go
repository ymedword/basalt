package flow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// VerifyRequest 只点名固定目标与规则版本，不接受外部自带通过事实。
// VerifyRequest names pinned targets and rules, never supplied verdicts.
type VerifyRequest struct {
	Principal       core.Principal
	TaskID          string
	TaskRevision    int64
	NodeAttemptID   string
	TargetKind      string
	TargetID        string
	TargetVersion   int64
	ContractVersion int64
	RuleID          string
	RuleVersion     int64
}

// Verification 是绑定目标、规则和验证者确切版本的追加事实。
// Verification is an append-only verdict bound to exact versions.
type Verification struct {
	ID              string   `json:"verification_id"`
	TaskID          string   `json:"task_id"`
	TaskRevision    int64    `json:"task_revision"`
	NodeAttemptID   string   `json:"node_attempt_id"`
	TargetKind      string   `json:"target_kind"`
	TargetID        string   `json:"target_id"`
	TargetVersion   int64    `json:"target_version"`
	ContractVersion int64    `json:"contract_version"`
	RuleID          string   `json:"rule_id"`
	RuleVersion     int64    `json:"rule_version"`
	ValidatorKind   string   `json:"validator_kind"`
	ValidatorID     string   `json:"validator_id"`
	CalibrationID   string   `json:"calibration_id,omitempty"`
	Outcome         string   `json:"outcome"`
	Reason          string   `json:"reason"`
	CreatedAt       int64    `json:"created_at"`
	BasisHashes     []string `json:"basis_hashes"`
}

// VerificationBasis 来自仓储，不允许模型覆盖契约和观察。
// VerificationBasis comes from authoritative storage rather than model controls.
type VerificationBasis struct {
	RequiredSources                              int
	TaskID, TargetKind, TargetID, ContentHash    string
	TaskRevision, TargetVersion, ContractVersion int64
	Rule                                         AcceptanceRule
	Output                                       json.RawMessage
	Evidence                                     []EvidenceRef
}

// VerificationRepository 读取及写结论均复核归属和目标版本。
// VerificationRepository checks ownership and target versions on both operations.
type VerificationRepository interface {
	LoadVerificationBasis(ctx context.Context, req VerifyRequest) (VerificationBasis, error)
	RecordVerification(ctx context.Context, req VerifyRequest, result Verification) error
}

// SourceValidity 每次按原身份即时复议来源权限，不缓存旧授权。
// SourceValidity rechecks current source authority for the original principal.
type SourceValidity interface {
	Check(ctx context.Context, principal core.Principal, ref EvidenceRef) (SourceVerdict, error)
}

// SourceVerdict 只回当前有效性和版本哈希，不回来源正文。
// SourceVerdict reports validity and hashes without source text.
type SourceVerdict struct {
	Valid                        bool
	Reason, Version, ContentHash string
}

// JudgmentValidator 由可信代码以校准身份注册，作者只引用它。
// JudgmentValidator is registered by trusted code with calibrated identity.
type JudgmentValidator interface {
	Identity() (validatorID, calibrationID string)
	Judge(ctx context.Context, principal core.Principal, basis VerificationBasis) (string, error)
}

// SourceSupport 独立校验目标支持／冲突，而不把可读性当成事实证明。
// SourceSupport checks target support independently of source readability.
type SourceSupport interface {
	Identity() (string, int64)
	CheckSupport(ctx context.Context, principal core.Principal, basis VerificationBasis) (SupportVerdict, error)
}

// SupportVerdict 绑定目标、规则和来源版本的基础哈希。
// SupportVerdict binds exact target, rule and source versions through its basis hash.
type SupportVerdict struct {
	Outcome   string
	BasisHash string
}

// VerificationService 分离程序、证据和判断验收，不把工具成功当通过。
// VerificationService separates validation from tool execution success.
type VerificationService struct {
	Support    SourceSupport
	Repository VerificationRepository
	Schemas    SchemaCompiler
	Sources    SourceValidity
	Judges     map[string]JudgmentValidator
	Now        func() time.Time
	Logger     *slog.Logger
}

// Verify 验证确切目标并持久追加结论，核对错误阻断接受。
// Verify appends an exact-target verdict; checking errors block acceptance.
func (v *VerificationService) Verify(ctx context.Context, req VerifyRequest) (Verification, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	if !req.Principal.Valid() || !req.Principal.HasScope("flows:write") {
		return Verification{}, core.ErrUnauthorized
	}
	if v.Repository == nil || v.Schemas == nil || req.TaskID == "" || req.TargetVersion < 1 || req.ContractVersion < 1 || req.RuleVersion < 1 || req.NodeAttemptID == "" {
		return Verification{}, errors.New("flow verification input or adapter unavailable")
	}
	basis, err := v.Repository.LoadVerificationBasis(ctx, req)
	if err != nil {
		return Verification{}, err
	}
	if basis.TaskID != req.TaskID || basis.TaskRevision != req.TaskRevision || basis.TargetID != req.TargetID || basis.TargetVersion != req.TargetVersion || basis.TargetKind != req.TargetKind || basis.ContractVersion != req.ContractVersion || basis.Rule.ID != req.RuleID || basis.Rule.Version != req.RuleVersion || basis.Rule.TargetID != req.TargetID || basis.Rule.TargetKind != req.TargetKind {
		return Verification{}, ErrStateConflict
	}
	id, err := newID("fv_")
	if err != nil {
		return Verification{}, err
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	bindingHash, err := VerificationBasisHash(basis)
	if err != nil {
		return Verification{}, err
	}
	result := Verification{ID: id, TaskID: req.TaskID, TaskRevision: req.TaskRevision, NodeAttemptID: req.NodeAttemptID, TargetKind: req.TargetKind, TargetID: req.TargetID, TargetVersion: req.TargetVersion, ContractVersion: req.ContractVersion, RuleID: req.RuleID, RuleVersion: req.RuleVersion, ValidatorKind: basis.Rule.Kind, ValidatorID: "basalt-deterministic-v1", Outcome: "inconclusive", CreatedAt: now.UnixMilli(), BasisHashes: []string{basis.ContentHash, bindingHash}}
	result.Outcome, result.Reason, err = v.check(ctx, req.Principal, basis)
	if basis.Rule.Kind == "judgment" {
		result.ValidatorID, result.CalibrationID = basis.Rule.ValidatorID, basis.Rule.CalibrationID
		if basis.Rule.HumanRequired {
			// This identifies the program that reports a missing capability. It
			// is not a human approval, and the outcome stays inconclusive.
			result.ValidatorID = "basalt-human-gate-v1"
		}
	}
	if basis.Rule.Kind == "evidence" && v.Support != nil {
		id, version := v.Support.Identity()
		result.ValidatorID = id + ":" + strconv.FormatInt(version, 10)
	}
	if err != nil {
		logger := v.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, "Flow verification unavailable", "task_id", req.TaskID, "rule_id", req.RuleID, "reason", result.Reason)
	}
	if recordErr := v.Repository.RecordVerification(ctx, req, result); recordErr != nil {
		return result, errors.Join(err, recordErr)
	}
	return result, err
}

func (v *VerificationService) check(ctx context.Context, principal core.Principal, basis VerificationBasis) (string, string, error) {
	if basis.RequiredSources > 0 && basis.Rule.Kind != "evidence" {
		outcome, reason, err := v.checkEvidence(ctx, principal, basis)
		if err != nil || outcome != "pass" {
			return outcome, reason, err
		}
	}
	switch basis.Rule.Kind {
	case "deterministic":
		hash, err := v.Schemas.Compile(ctx, basis.Rule.Schema)
		if err != nil {
			return "inconclusive", "schema_unavailable", err
		}
		if err := v.Schemas.Validate(ctx, hash, basis.Output); err != nil {
			var violation *core.SchemaViolationError
			if errors.As(err, &violation) {
				return "fail", "output_schema_invalid", nil
			}
			return "inconclusive", "schema_unavailable", err
		}
		return "pass", "schema_valid", nil
	case "evidence":
		return v.checkEvidence(ctx, principal, basis)
	case "judgment":
		if basis.Rule.HumanRequired {
			return "inconclusive", "human_verification_required", nil
		}
		judge := v.Judges[basis.Rule.ValidatorID]
		if judge == nil {
			return "inconclusive", "judge_unavailable", errors.New("flow judgment validator unavailable")
		}
		id, calibration := judge.Identity()
		if id != basis.Rule.ValidatorID || calibration == "" || calibration != basis.Rule.CalibrationID {
			return "inconclusive", "judge_calibration_invalid", errors.New("flow judgment calibration mismatch")
		}
		outcome, err := judge.Judge(ctx, principal, basis)
		if err != nil {
			return "inconclusive", "judge_failed", err
		}
		if outcome != "pass" && outcome != "fail" && outcome != "inconclusive" {
			return "inconclusive", "judge_result_invalid", errors.New("flow judgment result unsupported")
		}
		return outcome, "judgment_recorded", nil
	default:
		return "inconclusive", "rule_unsupported", errors.New("flow verification kind unsupported")
	}
}

func (v *VerificationService) checkEvidence(ctx context.Context, principal core.Principal, basis VerificationBasis) (string, string, error) {
	if v.Sources == nil {
		return "inconclusive", "source_check_unavailable", errors.New("flow source validator unavailable")
	}
	minimum := max(basis.Rule.MinSources, basis.RequiredSources)
	if minimum < 1 {
		return "inconclusive", "rule_invalid", errors.New("flow evidence rule invalid")
	}
	sources := make(map[string]bool)
	for _, ref := range basis.Evidence {
		if ref.TaskID != basis.TaskID || ref.ID == "" || ref.SourceKind == "" || ref.SourceUID == "" || ref.SourceVersion == "" || ref.ContentHash == "" {
			return "fail", "source_identity_invalid", nil
		}
		verdict, err := v.Sources.Check(ctx, principal, ref)
		if err != nil {
			return "inconclusive", "source_check_failed", err
		}
		if !verdict.Valid || verdict.Version != ref.SourceVersion || verdict.ContentHash != ref.ContentHash {
			return "fail", "source_stale_or_unreadable", nil
		}
		sources[ref.SourceKind+":"+ref.SourceUID] = true
	}
	if len(sources) < minimum {
		return "fail", "independent_sources_missing", nil
	}
	if v.Support == nil {
		return "inconclusive", "source_support_unavailable", errors.New("flow source support validator unavailable")
	}
	id, version := v.Support.Identity()
	if id == "" || version < 1 || basis.Rule.Kind == "evidence" && basis.Rule.ValidatorID != "" && basis.Rule.ValidatorID != id {
		return "inconclusive", "source_support_identity_invalid", errors.New("flow source support identity mismatch")
	}
	support, err := v.Support.CheckSupport(ctx, principal, basis)
	if err != nil {
		return "inconclusive", "source_support_failed", err
	}
	expected, err := VerificationBasisHash(basis)
	if err != nil {
		return "inconclusive", "source_support_basis_invalid", err
	}
	if support.BasisHash != expected {
		return "inconclusive", "source_support_stale", ErrStateConflict
	}
	switch support.Outcome {
	case "supported":
		return "pass", "source_supported", nil
	case "conflict":
		return "fail", "source_conflict", nil
	case "unverifiable":
		return "inconclusive", "source_support_unproven", nil
	default:
		return "inconclusive", "source_support_invalid", errors.New("flow source support outcome invalid")
	}
}

// VerificationBasisHash 封住目标、规则、输出与来源版本，拒绝旧支持结论挪用。
// VerificationBasisHash binds exact support inputs against stale reuse.
func VerificationBasisHash(basis VerificationBasis) (string, error) {
	refs := make([]string, 0, len(basis.Evidence))
	for _, ref := range basis.Evidence {
		encoded, err := json.Marshal([]string{ref.SourceKind, ref.SourceUID, ref.SourceVersion, ref.Location, ref.ContentHash})
		if err != nil {
			return "", err
		}
		refs = append(refs, string(encoded))
	}
	slices.Sort(refs)
	data, err := json.Marshal(struct {
		TaskID                         string
		Revision                       int64
		TargetKind, TargetID           string
		TargetVersion, ContractVersion int64
		Rule                           AcceptanceRule
		ContentHash                    string
		Output                         json.RawMessage
		References                     []string
		RequiredSources                int
	}{basis.TaskID, basis.TaskRevision, basis.TargetKind, basis.TargetID, basis.TargetVersion, basis.ContractVersion, basis.Rule, basis.ContentHash, basis.Output, refs, basis.RequiredSources})
	if err != nil {
		return "", errors.New("flow support basis encoding failed")
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func newID(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errors.New("flow identity entropy unavailable")
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}
