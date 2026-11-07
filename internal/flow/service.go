package flow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// Service owns durable task coordination. Construction starts no goroutines;
// Worker is the only background driver and every leaf still goes through Run.
type Service struct {
	Repository   Repository
	Definitions  DefinitionRepository
	Schemas      SchemaCompiler
	Authority    AuthorityResolver
	Runner       NodeRunner
	Config       Config
	Now          func() time.Time
	Logger       *slog.Logger
	Outputs      OutputRevalidator
	Verification *VerificationService
	Operations   core.OperationStore
	// Identity / Leases / LeaseTTL 是根租约的装配面（US-24 §3.1.6）：
	// 管理面 resume / revise 的请求里没有租约，写推进类状态时由 `withRootLease`
	// 用这三项自取；零值 = 未装配，此时写 Queued 只能拒绝（不得退化成无租约写入）。
	// Identity / Leases / LeaseTTL let management resume/revise acquire the root lease
	// themselves; a zero assembly refuses advance-class writes instead of degrading.
	Identity core.WorkerIdentity
	Leases   core.LeaseStore
	LeaseTTL time.Duration
	workMu   sync.Mutex
	work     map[string]*localWork
}

// Start durably accepts one immutable definition/contract/input identity. Existing
// idempotency keys are reconciled before consulting a possibly changed pointer.
func (s *Service) Start(ctx context.Context, req StartRequest) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if !req.Principal.Valid() || !req.Principal.HasScope(core.ScopeFlowsWrite) {
		return Task{}, core.ErrUnauthorized
	}
	if req.Principal.Kind == core.PrincipalCaller && req.Principal.KeyID == "" {
		return Task{}, core.ErrUnauthorized
	}
	if s == nil || s.Repository == nil || s.Definitions == nil || s.Schemas == nil || s.Authority == nil || !s.Config.Enabled {
		return Task{}, ErrStateConflict
	}
	if req.DefinitionUID == "" || strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 256 || req.DefinitionVersion < 0 {
		return Task{}, ErrDefinitionInvalid
	}
	previous, err := s.Repository.FindAccepted(ctx, req.Principal, req.IdempotencyKey)
	if err == nil {
		return s.reconcileAccepted(ctx, req, previous)
	}
	if !errors.Is(err, ErrNotFound) {
		return Task{}, err
	}
	definition, err := s.Definitions.ResolveCurrent(ctx, req.DefinitionUID, req.DefinitionVersion)
	if err != nil {
		return Task{}, err
	}
	if err := s.Authority.CheckStart(ctx, req.Principal, definition); err != nil {
		return Task{}, err
	}
	if capacity, ok := s.Authority.(interface {
		CheckNewTask(context.Context, core.Principal) error
	}); ok {
		if err := capacity.CheckNewTask(ctx, req.Principal); err != nil {
			return Task{}, err
		}
	}
	freezer, ok := s.Authority.(AuthorityFreezer)
	if !ok {
		return Task{}, core.ErrUnauthorized
	}
	ceiling, err := freezer.Freeze(ctx, req.Principal, definition)
	if err != nil {
		return Task{}, err
	}
	if ceiling.CallerID != req.Principal.CallerID || ceiling.KeyID != req.Principal.KeyID || !slices.Contains(ceiling.Scopes, core.ScopeFlowsWrite) {
		return Task{}, core.ErrUnauthorized
	}
	expectedKind := core.AuthorizingKindCallerKey
	if req.Principal.Kind == core.PrincipalLocalAdmin {
		expectedKind = core.AuthorizingKindAdminBuiltin
	}
	if ceiling.AuthorizingKind != expectedKind {
		return Task{}, core.ErrUnauthorized
	}
	contract := definition.Definition.Contract
	base := minimumBudget(contract.Budget, s.Config.Budget())
	budget, err := TightenBudget(base, req.BudgetCap)
	if err != nil {
		return Task{}, err
	}
	contract.Budget, contract.Scope = budget, ceiling
	input, inputHash, err := s.validateInput(ctx, definition, req.Input)
	if err != nil {
		return Task{}, err
	}
	requestHash, err := startHash(definition.DefinitionUID, definition.Version, contract.ContractVersion, input, budget)
	if err != nil {
		return Task{}, err
	}
	id, err := newID("ft_")
	if err != nil {
		return Task{}, err
	}
	now := s.clock().UnixMilli()
	created, err := s.Repository.Create(ctx, CreateTask{Request: req, TaskID: id, RootTaskID: id, RequestHash: requestHash, InputHash: inputHash, Definition: definition, Contract: contract, Input: input, CreatedAt: now, Deadline: now + budget.MaxDurationSeconds*1000, Config: s.Config})
	if errors.Is(err, ErrIdempotencyConflict) || errors.Is(err, ErrDefinitionNotCurrent) || errors.Is(err, ErrDefinitionRetired) {
		// A concurrent winner may have accepted the old version. Recompute against
		// that winner's frozen template instead of treating the new pointer as input.
		winner, readErr := s.Repository.FindAccepted(ctx, req.Principal, req.IdempotencyKey)
		if readErr == nil {
			return s.reconcileAccepted(ctx, req, winner)
		}
		if !errors.Is(readErr, ErrNotFound) {
			return Task{}, errors.Join(err, readErr)
		}
	}
	created.NewlyAccepted = err == nil && created.ID == id
	return created, err
}

func (s *Service) reconcileAccepted(ctx context.Context, req StartRequest, previous Task) (Task, error) {
	if req.DefinitionUID != previous.DefinitionUID || req.DefinitionVersion > 0 && req.DefinitionVersion != previous.DefinitionVersion {
		return Task{}, ErrIdempotencyConflict
	}
	budget := previous.Contract.Budget
	if req.BudgetCap != nil {
		var err error
		budget, err = TightenBudget(previous.AcceptedVersion.Definition.Contract.Budget, req.BudgetCap)
		if err != nil {
			return Task{}, ErrIdempotencyConflict
		}
	}
	input, _, err := s.validateInput(ctx, previous.AcceptedVersion, req.Input)
	if err != nil {
		return Task{}, ErrIdempotencyConflict
	}
	hash, err := startHash(previous.DefinitionUID, previous.DefinitionVersion, previous.ContractVersion, input, budget)
	if err != nil || hash != previous.RequestHash {
		return Task{}, ErrIdempotencyConflict
	}
	return previous, nil
}

func (s *Service) validateInput(ctx context.Context, definition DefinitionVersion, input json.RawMessage) (json.RawMessage, string, error) {
	canonical, err := CanonicalJSON(ctx, input)
	if err != nil {
		return nil, "", errors.Join(ErrDefinitionInvalid, err)
	}
	hash, err := s.Schemas.Compile(ctx, definition.Definition.Contract.InputSchema)
	if err != nil {
		return nil, "", err
	}
	if err := s.Schemas.Validate(ctx, hash, canonical); err != nil {
		return nil, "", errors.Join(ErrDefinitionInvalid, err)
	}
	return canonical, flowInputHash(canonical), nil
}

func startHash(uid string, version, contractVersion int64, input json.RawMessage, budget BudgetCap) (string, error) {
	canonical, err := CanonicalJSON(context.Background(), mustJSON(struct {
		UID                      string
		Version, ContractVersion int64
		Input                    json.RawMessage
		Budget                   BudgetCap
	}{uid, version, contractVersion, input, budget}))
	if err != nil {
		return "", err
	}
	return flowInputHash(canonical), nil
}

func minimumBudget(a, b BudgetCap) BudgetCap {
	return BudgetCap{MaxTotalTokens: min(a.MaxTotalTokens, b.MaxTotalTokens), MaxToolCalls: min(a.MaxToolCalls, b.MaxToolCalls), MaxDurationSeconds: min(a.MaxDurationSeconds, b.MaxDurationSeconds), MaxDepth: min(a.MaxDepth, b.MaxDepth), MaxChildren: min(a.MaxChildren, b.MaxChildren), MaxAttemptsPerNode: min(a.MaxAttemptsPerNode, b.MaxAttemptsPerNode), MaxRevisions: min(a.MaxRevisions, b.MaxRevisions)}
}

func (s *Service) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
