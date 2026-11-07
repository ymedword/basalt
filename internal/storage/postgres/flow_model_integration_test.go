//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/provider"
)

type pgFlowModelProbe struct {
	store         *FlowStore
	request       core.ChatRequest
	client        core.ProviderClient
	received      atomic.Int64
	bound         atomic.Bool
	withUsage     bool
	usageOverride *core.Usage
}

func newPGFlowModelProbe(t *testing.T) *pgFlowModelProbe {
	t.Helper()
	store := NewFlowStore(newTxTestDB(t))
	resources := seedRunManifest(t, store.db)
	definition := flowStoreDefinition()
	definition.Nodes[0].Agent.UID = string(resources.manifest.AgentUID)
	definition.Nodes[0].Agent.ManifestHash = resources.manifest.Hash
	version := publishFlowStoreDefinition(t, store, definition, 0)
	create := flowCreateRequest(version, "model-task", "model-key")
	create.Contract.Scope = core.FlowAuthority{CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin, Scopes: []string{core.ScopeFlowsWrite}, AgentUIDs: []string{string(resources.manifest.AgentUID)}}
	task, err := store.Create(t.Context(), create)
	require.NoError(t, err)
	rootLease := flowRootLeaseFor(t, store.db, task.RootTaskID)
	attempt := flow.NodeAttempt{ID: "model-attempt", TaskID: task.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: 1, RunID: "model-run", SessionID: "model-session", Input: task.Revision.Input, InputHash: task.Revision.InputHash}
	_, err = store.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Attempt: attempt, Now: task.CreatedAt + 1})
	require.NoError(t, err)
	f := &core.FlowRunContext{TaskID: task.ID, RootTaskID: task.ID, NodeAttemptID: attempt.ID, NodeInvocationID: attempt.NodeInvocationID, TaskRevision: 0, Lease: rootLease, AgentUID: string(resources.manifest.AgentUID), AgentConfigHash: "config", ResourceManifestHash: resources.manifest.Hash, Authority: create.Contract.Scope}
	require.NoError(t, startRunTest(NewRunStore(store.db), t.Context(), core.RunRecord{RunID: attempt.RunID, SessionID: attempt.SessionID, Agent: "writer", Channel: "cli", UserID: "model-probe", Principal: core.LocalAdminCLIPrincipal(), StartedAt: time.Now(), ConfigHash: "config", Flow: f}, resources.manifest))
	p := &pgFlowModelProbe{store: store, withUsage: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.received.Add(1)
		var bound bool
		err := store.db.pool.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM flow_action_attempts a JOIN llm_calls l ON l.id=a.llm_call_id WHERE a.task_id=$1 AND a.outcome='dispatching' AND l.status='pending' AND a.run_id=l.run_id)`, task.ID).Scan(&bound)
		p.bound.Store(err == nil && bound)
		w.Header().Set("Content-Type", "application/json")
		usage := ""
		if p.withUsage {
			usage = `,"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`
		}
		if p.usageOverride != nil {
			raw, _ := json.Marshal(map[string]int{"prompt_tokens": p.usageOverride.PromptTokens, "completion_tokens": p.usageOverride.CompletionTokens, "total_tokens": p.usageOverride.TotalTokens})
			usage = `,"usage":` + string(raw)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]` + usage + `}`))
	}))
	t.Cleanup(server.Close)
	transport, err := provider.NewOpenAIClient(provider.Options{Name: resources.manifest.Provider.Name, BaseURL: server.URL, APIKey: "fixture-not-a-real-credential"})
	require.NoError(t, err)
	p.client = provider.NewProviderService(map[string]core.ProviderClient{resources.manifest.Provider.Name: transport}, map[string]string{resources.manifest.Provider.Name: resources.manifest.Provider.Model}, NewLLMCallRepo(store.db), nil)
	f.Models = &FlowModels{Store: store, Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, NodeAttemptID: attempt.ID, Recheck: func(context.Context, core.ChatRequest) error { return nil }, Estimate: func(input int) int64 { return int64(input) + 50 }}
	p.request = core.ChatRequest{Provider: resources.manifest.Provider.Name, RunID: attempt.RunID, SessionID: attempt.SessionID, Round: 1, ConfigHash: "config", Model: resources.manifest.Provider.Model, Messages: []core.Message{{Role: "user", Content: "private-body-not-for-action-ledger"}}, Flow: f, FlowEstimatedInput: 20}
	return p
}

func TestFlow_ModelAuditBudgetBoundBeforeRealDispatch(t *testing.T) {
	p := newPGFlowModelProbe(t)
	response, err := p.client.Chat(t.Context(), p.request)
	require.NoError(t, err)
	require.Equal(t, "done", response.Content)
	require.EqualValues(t, 1, p.received.Load())
	require.True(t, p.bound.Load())
	var state, actionInput string
	var tokens int64
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT state,actual_tokens FROM flow_budget_allocations WHERE task_id='model-task'`).Scan(&state, &tokens))
	require.Equal(t, "settled", state)
	require.EqualValues(t, 5, tokens)
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT input FROM flow_actions WHERE task_id='model-task'`).Scan(&actionInput))
	require.False(t, strings.Contains(actionInput, "private-body"), "model prompts must not be copied into a second ledger")
	_, err = p.client.Chat(t.Context(), p.request)
	require.Error(t, err)
	require.EqualValues(t, 1, p.received.Load())
}

func TestFlow_ModelPrewritesRefuseBeforeDispatch(t *testing.T) {
	for _, tc := range []struct{ name, table, event, condition string }{
		{"action", "flow_actions", "INSERT", ""}, {"budget", "flow_budget_allocations", "INSERT", ""}, {"pending", "llm_calls", "INSERT", ""}, {"permit", "flow_action_attempts", "UPDATE", `WHEN (NEW.outcome='dispatching')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPGFlowModelProbe(t)
			installFlowFault(t, p.store, tc.table, tc.event, tc.condition)
			_, err := p.client.Chat(t.Context(), p.request)
			require.Error(t, err)
			require.Zero(t, p.received.Load())
			var fired bool
			require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT is_called FROM flow_probe_fault_seq`).Scan(&fired))
			require.True(t, fired)
		})
	}
}

func TestFlow_ModelUnknownAndMissingUsageRetainReservation(t *testing.T) {
	for _, terminalFault := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-usage", true: "audit-unknown"}[terminalFault], func(t *testing.T) {
			p := newPGFlowModelProbe(t)
			if terminalFault {
				installFlowFault(t, p.store, "llm_calls", "UPDATE", `WHEN (NEW.status<>'pending')`)
			} else {
				p.withUsage = false
			}
			_, err := p.client.Chat(t.Context(), p.request)
			if terminalFault {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, p.received.Load())
			var state string
			var reserved int64
			require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT state,reserved_tokens FROM flow_budget_allocations WHERE task_id='model-task'`).Scan(&state, &reserved))
			require.Equal(t, "unknown", state)
			require.EqualValues(t, 70, reserved)
			if terminalFault {
				unknown, err := p.store.HasUnknownActions(t.Context(), "model-task")
				require.NoError(t, err)
				require.True(t, unknown)
			}
		})
	}
}

func TestFlow_ModelRequestSnapshotPreservesExactValues(t *testing.T) {
	req := core.ChatRequest{Provider: "p", Model: "m", ConfigHash: "config", Messages: []core.Message{{Role: "user", Content: "original", ProviderExtra: map[string]any{"integer": json.Number("9007199254740993")}}}}
	copy, hash, err := core.SnapshotFlowModelRequest(t.Context(), req)
	require.NoError(t, err)
	req.Messages[0].Content = "changed"
	req.Messages[0].ProviderExtra["integer"] = json.Number("1")
	_, again, err := core.SnapshotFlowModelRequest(t.Context(), copy)
	require.NoError(t, err)
	require.Equal(t, hash, again)
	require.Equal(t, "original", copy.Messages[0].Content)
	require.Equal(t, json.Number("9007199254740993"), copy.Messages[0].ProviderExtra["integer"])
}

func TestFlow_ModelContradictoryUsageDoesNotReleaseEitherBudget(t *testing.T) {
	p := newPGFlowModelProbe(t)
	p.usageOverride = &core.Usage{PromptTokens: 60, CompletionTokens: 40, TotalTokens: 1}
	response, err := p.client.Chat(t.Context(), p.request)
	require.ErrorIs(t, err, core.ErrFlowUsageInvalid)
	require.NotErrorIs(t, err, core.ErrFlowModelNotIssued, "caller quota must remain pending for an actual issued request")
	require.Equal(t, 1, response.Usage.TotalTokens, "original response is preserved; no fake zero usage")
	var state string
	var actual sql.NullInt64
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT state,actual_tokens FROM flow_budget_allocations WHERE task_id='model-task'`).Scan(&state, &actual))
	require.Equal(t, "unknown", state)
	require.False(t, actual.Valid, "untrusted usage remains NULL, never a fabricated zero settlement")
	var prompt, total int
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT prompt_tokens,total_tokens FROM llm_calls WHERE run_id='model-run'`).Scan(&prompt, &total))
	require.Equal(t, 60, prompt)
	require.Equal(t, 1, total, "audit keeps the actual contradictory provider report")
	unknown, err := p.store.HasUnknownActions(t.Context(), "model-task")
	require.NoError(t, err)
	require.True(t, unknown)
	_, err = p.client.Chat(t.Context(), p.request)
	require.Error(t, err)
	require.EqualValues(t, 1, p.received.Load())
}

func TestFlow_ModelCallbackCannotReplaceFrozenPayload(t *testing.T) {
	for _, target := range []string{"messages", "tools", "evidence"} {
		t.Run(target, func(t *testing.T) {
			p := newPGFlowModelProbe(t)
			p.request.Tools = []core.ToolSpec{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}
			p.request.FlowEvidenceRefs = []core.EvidenceRef{{Kind: "knowledge", ID: "source", VersionID: "v1", ContentHash: "hash"}}
			models := p.request.Flow.Models.(*FlowModels)
			var checks atomic.Int64
			models.Recheck = func(_ context.Context, req core.ChatRequest) error {
				if checks.Add(1) == 2 {
					switch target {
					case "messages":
						req.Messages[0].Content = "replacement"
					case "tools":
						req.Tools[0].Parameters = json.RawMessage(`{}`)
					case "evidence":
						req.FlowEvidenceRefs[0].VersionID = "v2"
					}
				}
				return nil
			}
			_, err := p.client.Chat(t.Context(), p.request)
			require.ErrorIs(t, err, core.ErrFlowModelNotIssued)
			require.EqualValues(t, 2, checks.Load())
			require.Zero(t, p.received.Load())
			require.Equal(t, "private-body-not-for-action-ledger", p.request.Messages[0].Content)
			require.Equal(t, "v1", p.request.FlowEvidenceRefs[0].VersionID)
			var count int
			require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM llm_calls WHERE run_id='model-run' AND status='failed'`).Scan(&count))
			require.Equal(t, 1, count, "the unique original pending audit is completed without sending")
		})
	}
}
