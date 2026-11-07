//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
)

type flowCommitAuthority struct {
	flowStartAuthorityProbe
	p core.Principal
}

func (a *flowCommitAuthority) Current(context.Context, flow.Task) (core.Principal, error) {
	return a.p, nil
}

type flowCallbackSchemas struct {
	flow.SchemaCompiler
	calls    int
	callback func()
}

func (s *flowCallbackSchemas) Validate(ctx context.Context, hash string, raw json.RawMessage) error {
	s.calls++
	if s.calls == 3 {
		s.callback()
	}
	return s.SchemaCompiler.Validate(ctx, hash, raw)
}

func TestFlow_CommitRechecksOriginalKeyAfterValidation(t *testing.T) {
	for _, mode := range []string{"valid", "revoked", "scope-removed"} {
		t.Run(mode, func(t *testing.T) {
			s, def := newFlowStoreTest(t)
			keys := NewKeyStore(s.db)
			require.NoError(t, keys.CreateCaller(t.Context(), callers.CallerRecord{CallerID: "flow-owner", Name: "flow owner", Kind: callers.CallerKindExternal, Status: callers.CallerStatusActive}, nil))
			scopes := []string{core.ScopeFlowsWrite, core.ScopeFlowsRead, core.AgentInvokeScopePrefix + "writer"}
			require.NoError(t, keys.Create(t.Context(), keyStoreRecord("flow-key", "flow-owner", "bk_"+strings.Repeat("a", 40), scopes...)))
			p, err := core.OwnedPrincipal("flow-owner", core.AuthorizingKindCallerKey, "flow-key", scopes, core.ActorRecovery)
			require.NoError(t, err)
			create := flowCreateRequest(def, "commit-authority", "key")
			create.Request.Principal = p
			create.Contract.Scope = core.FlowAuthority{CallerID: p.CallerID, KeyID: p.KeyID, AuthorizingKind: core.AuthorizingKindCallerKey, Scopes: scopes, AgentUIDs: []string{"ag_one"}}
			task, err := s.Create(t.Context(), create)
			require.NoError(t, err)
			// 凭证必须原样下传：子 Run 创建（createRunForNode）以它证明父根归属。
			root := flowRootLeaseFor(t, s.db, task.RootTaskID)
			fired := false
			schemas := &flowCallbackSchemas{SchemaCompiler: tool.NewSchemaCompiler(), callback: func() {
				fired = true
				switch mode {
				case "revoked":
					_, err = s.db.pool.ExecContext(t.Context(), `UPDATE caller_keys SET revoked_at=1 WHERE key_id='flow-key'`)
				case "scope-removed":
					_, err = s.db.pool.ExecContext(t.Context(), `UPDATE caller_keys SET scopes=ARRAY['flows:read','agents:invoke:writer']::text[] WHERE key_id='flow-key'`)
				}
				require.NoError(t, err)
			}}
			config := flow.DefaultConfig()
			config.Enabled = true
			service := &flow.Service{Repository: s, Definitions: s, Schemas: schemas, Authority: &flowCommitAuthority{p: p}, Runner: &flowAdvanceRunner{store: s, output: json.RawMessage(`{"answer":"known output"}`)}, Config: config}
			finished, err := service.Advance(t.Context(), flow.AdvanceRequest{Principal: p, TaskID: task.ID, Expected: task.Version, Lease: root})
			require.True(t, fired, "the mutation must occur in the last validator")
			var artifacts int
			require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_artifacts WHERE task_id=$1`, task.ID).Scan(&artifacts))
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, flow.Succeeded, finished.Status)
				require.Equal(t, 1, artifacts)
			} else {
				require.ErrorIs(t, err, core.ErrUnauthorized)
				require.Zero(t, artifacts)
				current, err := s.ReadForWorker(t.Context(), task.ID)
				require.NoError(t, err)
				require.Equal(t, flow.Running, current.Status)
				require.Empty(t, current.Revision.Outputs)
			}
		})
	}
}
