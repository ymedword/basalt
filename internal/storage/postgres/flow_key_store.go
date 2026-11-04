package postgres

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
)

// NoAvailableClosingKey does not require Agent invoke scope: a current same-owner
// flows:write key may cancel even after execution authorization has been revoked.
func (s *FlowStore) NoAvailableClosingKey(ctx context.Context, callerID string) (bool, error) {
	if callerID == core.LocalAdminCallerID {
		return false, nil
	}
	var available bool
	err := s.db.pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM callers c JOIN caller_keys k ON k.caller_id=c.caller_id WHERE c.caller_id=$1 AND c.status='active' AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>$2) AND 'flows:write'=ANY(k.scopes))`, callerID, s.db.clock().UnixMilli()).Scan(&available)
	return !available, err
}
