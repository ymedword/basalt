//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
)

type flowStartAuthorityProbe struct {
	checks  atomic.Int64
	failure error
}

func (p *flowStartAuthorityProbe) Current(context.Context, flow.Task) (core.Principal, error) {
	return core.LocalAdminCLIPrincipal(), nil
}

func (p *flowStartAuthorityProbe) CheckStart(context.Context, core.Principal, flow.DefinitionVersion) error {
	p.checks.Add(1)
	return p.failure
}

func (p *flowStartAuthorityProbe) NoAvailableCallerKey(context.Context, string) (bool, error) {
	return false, nil
}

func (p *flowStartAuthorityProbe) Freeze(_ context.Context, principal core.Principal, _ flow.DefinitionVersion) (core.FlowAuthority, error) {
	return core.FlowAuthority{CallerID: principal.CallerID, KeyID: principal.KeyID, AuthorizingKind: core.AuthorizingKindAdminBuiltin, Scopes: []string{core.ScopeFlowsWrite}, AgentUIDs: []string{"ag_one"}, Tools: []string{"flow_probe"}}, nil
}

func flowServiceForStore(s *FlowStore) *flow.Service {
	config := flow.DefaultConfig()
	config.Enabled = true
	// 管理面 Revise / Recover 写 Queued 属推进类，需要根租约（US-24 §3.1.6）：
	// 夹具按真实装配注入身份与租约端口，缺这三项会被拒为 ErrFlowRootBusy。
	return &flow.Service{
		Repository: s, Definitions: s, Schemas: tool.NewSchemaCompiler(), Authority: &flowStartAuthorityProbe{}, Config: config,
		Identity: core.WorkerIdentity{InstanceID: "flow-store-fixture", Name: "fixture"}, Leases: NewLeaseStore(s.db), LeaseTTL: 30 * time.Second,
	}
}

func startFlowRequest(def flow.DefinitionVersion, key string) flow.StartRequest {
	return flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: def.DefinitionUID, IdempotencyKey: key, Input: json.RawMessage(`{"answer":"input"}`)}
}

func TestFlow_StartAckRequiresCommit(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	installFlowFault(t, s, "flow_task_revisions", "INSERT", "")
	_, err := service.Start(t.Context(), startFlowRequest(def, "start-commit"))
	require.Error(t, err)
	var count int
	var fired bool
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT is_called FROM flow_probe_fault_seq`).Scan(&fired))
	require.True(t, fired)
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_tasks`).Scan(&count))
	require.Zero(t, count, "acceptance must not survive a failed checkpoint transaction")
	_, err = s.db.pool.ExecContext(t.Context(), `DROP TRIGGER flow_probe_fault_trigger ON flow_task_revisions`)
	require.NoError(t, err)
	accepted, err := service.Start(t.Context(), startFlowRequest(def, "start-commit"))
	require.NoError(t, err)
	require.NotEmpty(t, accepted.ID)
	require.Equal(t, flow.Queued, accepted.Status)
	require.Equal(t, accepted.ID, accepted.RootTaskID)
	require.Equal(t, core.LocalAdminCallerID, accepted.Contract.Scope.CallerID)
	stored, err := s.FindAccepted(t.Context(), core.LocalAdminCLIPrincipal(), "start-commit")
	require.NoError(t, err)
	require.Equal(t, accepted.ID, stored.ID)
}

func TestFlow_StartUsesFrozenContractBeforeCurrentPointer(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	req := startFlowRequest(def, "frozen-start")
	first, err := service.Start(t.Context(), req)
	require.NoError(t, err)
	newDefinition := flowStoreDefinition()
	newDefinition.Contract.InputSchema = json.RawMessage(`{"type":"object","properties":{"new_field":{"type":"string"}},"required":["new_field"],"additionalProperties":false}`)
	publishFlowStoreDefinition(t, s, newDefinition, 1)
	authority := service.Authority.(*flowStartAuthorityProbe)
	count := authority.checks.Load()
	authority.failure = errors.New("execution authority now unavailable")
	req.Input = json.RawMessage("{ \n \"answer\" : \"input\" }")
	again, err := service.Start(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	require.EqualValues(t, 1, again.DefinitionVersion)
	require.Equal(t, count, authority.checks.Load(), "idempotent read must not re-admit execution or consult the new schema")
	req.Input = json.RawMessage(`{"answer":"changed"}`)
	_, err = service.Start(t.Context(), req)
	require.ErrorIs(t, err, flow.ErrIdempotencyConflict)
	req = startFlowRequest(def, "new-start-key")
	req.DefinitionVersion = 1
	_, err = service.Start(t.Context(), req)
	require.ErrorIs(t, err, flow.ErrDefinitionNotCurrent)
}

type flowSwitchingDefinitions struct {
	flow.DefinitionRepository
	resolve func(context.Context, string, int64) (flow.DefinitionVersion, error)
}

func (d flowSwitchingDefinitions) ResolveCurrent(ctx context.Context, uid string, version int64) (flow.DefinitionVersion, error) {
	return d.resolve(ctx, uid, version)
}

func TestFlow_StartRaceReconcilesWinnerAgainstAcceptedTemplate(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	req := startFlowRequest(def, "switching-key")
	var winner flow.Task
	service.Definitions = flowSwitchingDefinitions{DefinitionRepository: s, resolve: func(ctx context.Context, uid string, version int64) (flow.DefinitionVersion, error) {
		// The first read really saw no accepted key. Before this request resolves
		// the current pointer, another request commits v1 and publication switches.
		var err error
		winner, err = flowServiceForStore(s).Start(ctx, req)
		if err != nil {
			return flow.DefinitionVersion{}, err
		}
		updated := flowStoreDefinition()
		updated.Contract.Goal = "new version"
		publishFlowStoreDefinition(t, s, updated, 1)
		return s.ResolveCurrent(ctx, uid, version)
	}}
	result, err := service.Start(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, winner.ID, result.ID)
	require.EqualValues(t, 1, result.DefinitionVersion)
	var count int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_tasks`).Scan(&count))
	require.Equal(t, 1, count)
}
