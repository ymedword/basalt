package flow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// Publisher 先预写既有管理审计，再发布不可变内容与条件指针。
// Publisher prewrites management audit before immutable publication and pointer CAS.
type Publisher struct {
	Definitions DefinitionRepository
	Operations  core.OperationStore
	Validator   *ContractValidator
	Now         func() time.Time
}

// Publish 只影响新任务；同操作回读事实，不重新解释旧接受版本。
// Publish affects new tasks only and reconciles idempotent operation facts.
func (p *Publisher) Publish(ctx context.Context, req PublishRequest) (DefinitionVersion, error) {
	if err := validateManager(req.Principal, req.Actor); err != nil {
		return DefinitionVersion{}, err
	}
	if p.Definitions == nil || p.Operations == nil || p.Validator == nil || req.OperationID == "" || req.ExpectedVersion < 0 {
		return DefinitionVersion{}, ErrDefinitionInvalid
	}
	if err := p.Validator.ValidateDefinition(ctx, req.Definition); err != nil {
		return DefinitionVersion{}, errors.Join(ErrDefinitionInvalid, err)
	}
	hash, err := DefinitionHash(req.Definition)
	if err != nil {
		return DefinitionVersion{}, err
	}
	requestHash, err := publicationRequestHash("publish", req.DefinitionUID, req.ExpectedVersion, hash, "")
	if err != nil {
		return DefinitionVersion{}, err
	}
	previous, err := p.Operations.GetOperation(ctx, req.OperationID)
	if err == nil {
		if previous.Action != "flow.publish" || previous.RequestHash != requestHash {
			return DefinitionVersion{}, ErrIdempotencyConflict
		}
		if previous.Status != core.OperationSuccess {
			return DefinitionVersion{}, ErrCommitUnknown
		}
		return p.Definitions.ReadDefinition(ctx, DefinitionRef{UID: previous.TargetID, Version: previous.RequestedVersionID})
	}
	if !errors.Is(err, core.ErrOperationNotFound) {
		return DefinitionVersion{}, err
	}
	var current DefinitionVersion
	if req.DefinitionUID != "" {
		current, err = p.Definitions.CurrentPointer(ctx, req.DefinitionUID)
	} else {
		current, err = p.Definitions.FindDefinition(ctx, req.Definition.Name)
	}
	if err != nil && !errors.Is(err, ErrDefinitionNotFound) {
		return DefinitionVersion{}, err
	}
	if errors.Is(err, ErrDefinitionNotFound) {
		if req.DefinitionUID != "" || req.ExpectedVersion != 0 {
			return DefinitionVersion{}, core.ErrPreconditionFailed
		}
		req.DefinitionUID, err = newID("fd_")
		if err != nil {
			return DefinitionVersion{}, err
		}
	} else {
		if current.Version != req.ExpectedVersion {
			return DefinitionVersion{}, core.ErrPreconditionFailed
		}
		req.DefinitionUID = current.DefinitionUID
	}
	version := current.Version + 1
	if current.Version > 0 && !current.Retired && current.ContentHash == hash {
		version = current.Version
	}
	now := p.clock()
	detail, err := json.Marshal(map[string]any{"format_version": 1, "content_hash": hash, "name": req.Definition.Name})
	if err != nil {
		return DefinitionVersion{}, err
	}
	if err := p.Operations.BeginOperation(ctx, core.OperationRecord{OperationID: req.OperationID, Action: "flow.publish", TargetKind: "flow_definition", TargetID: req.DefinitionUID, ActorSubject: req.Actor.Subject, AuthMethod: req.Actor.Method, Source: req.Actor.Source, RequestHash: requestHash, RequestedVersionID: version, Status: core.OperationPending, DetailJSON: detail, CreatedAt: now}); err != nil {
		return DefinitionVersion{}, err
	}
	result, err := p.Definitions.Publish(ctx, req)
	if err != nil && !errors.Is(err, ErrCommitUnknown) {
		return result, p.finishFailure(ctx, req.OperationID, err)
	}
	return result, err
}

// Retire 禁止新受理，但保留全部历史定义／契约／恢复依据。
// Retire forbids new acceptance while retaining all historical recovery content.
func (p *Publisher) Retire(ctx context.Context, req RetireRequest) (DefinitionVersion, error) {
	if err := validateManager(req.Principal, req.Actor); err != nil {
		return DefinitionVersion{}, err
	}
	if p.Definitions == nil || p.Operations == nil || req.OperationID == "" || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 4096 || req.Version < 1 || req.Version != req.ExpectedVersion {
		return DefinitionVersion{}, ErrDefinitionInvalid
	}
	hash, err := publicationRequestHash("retire", req.DefinitionUID, req.ExpectedVersion, "", req.Reason)
	if err != nil {
		return DefinitionVersion{}, err
	}
	previous, err := p.Operations.GetOperation(ctx, req.OperationID)
	if err == nil {
		if previous.Action != "flow.retire" || previous.RequestHash != hash {
			return DefinitionVersion{}, ErrIdempotencyConflict
		}
		if previous.Status != core.OperationSuccess {
			return DefinitionVersion{}, ErrCommitUnknown
		}
		version, err := p.Definitions.ReadDefinition(ctx, DefinitionRef{UID: req.DefinitionUID, Version: req.Version})
		version.Retired = true
		return version, err
	}
	if !errors.Is(err, core.ErrOperationNotFound) {
		return DefinitionVersion{}, err
	}
	current, err := p.Definitions.CurrentPointer(ctx, req.DefinitionUID)
	if err != nil {
		return DefinitionVersion{}, err
	}
	if current.Version != req.ExpectedVersion {
		return DefinitionVersion{}, core.ErrPreconditionFailed
	}
	detail, err := json.Marshal(map[string]any{"format_version": 1, "content_hash": current.ContentHash, "reason": req.Reason})
	if err != nil {
		return DefinitionVersion{}, err
	}
	if err := p.Operations.BeginOperation(ctx, core.OperationRecord{OperationID: req.OperationID, Action: "flow.retire", TargetKind: "flow_definition", TargetID: req.DefinitionUID, ActorSubject: req.Actor.Subject, AuthMethod: req.Actor.Method, Source: req.Actor.Source, RequestHash: hash, RequestedVersionID: req.Version, Status: core.OperationPending, DetailJSON: detail, CreatedAt: p.clock()}); err != nil {
		return DefinitionVersion{}, err
	}
	result, err := p.Definitions.Retire(ctx, req)
	if err != nil && !errors.Is(err, ErrCommitUnknown) {
		return result, p.finishFailure(ctx, req.OperationID, err)
	}
	return result, err
}

func (p *Publisher) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Publisher) finishFailure(ctx context.Context, id string, cause error) error {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := p.Operations.FinishOperation(finish, id, core.OperationFailed, "FLOW_OPERATION_FAILED", "流程变更未提交", nil, p.clock())
	return errors.Join(cause, err)
}

func validateManager(principal core.Principal, actor core.Actor) error {
	if !principal.Valid() || principal.Kind != core.PrincipalLocalAdmin || actor.Subject == "" || actor.Source == "" || (actor.Method != core.AuthMethodBearer && actor.Method != core.AuthMethodLocalTrust && actor.Method != core.AuthMethodCLI) {
		return core.ErrUnauthorized
	}
	return nil
}

func publicationRequestHash(action, uid string, expected int64, hash, reason string) (string, error) {
	data, err := json.Marshal([]any{action, uid, expected, hash, reason})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
