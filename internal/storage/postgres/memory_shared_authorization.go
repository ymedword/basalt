package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// AuthorizeMemory checks the live key and caller again for every explicit
// shared-memory read; a frozen principal alone cannot prove a key remains valid.
func (s *MemoryStore) AuthorizeMemory(ctx context.Context, p core.Principal, agent string) error {
	if !p.Valid() {
		return core.ErrUnauthorized
	}
	if agent == "" || !p.HasAgentScope(agent) {
		return core.ErrForbidden
	}
	if p.Kind == core.PrincipalLocalAdmin {
		return nil
	}
	if s == nil || s.db == nil {
		return core.ErrForbidden
	}
	record, err := NewKeyStore(s.db).LookupByID(ctx, p.CallerID, p.KeyID)
	if errors.Is(err, callers.ErrNotFound) {
		return core.ErrForbidden
	}
	if err != nil {
		return fmt.Errorf("检查记忆调用方状态: %w", err)
	}
	if record.Status != callers.KeyStatusActive || !slices.Contains(record.Scopes, core.AgentInvokeScopePrefix+agent) {
		return core.ErrForbidden
	}
	return nil
}
