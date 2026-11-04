//go:build integration

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

type flowBlockingProtocol struct{ entered, cancelled, release chan struct{} }

func (p *flowBlockingProtocol) Chat(ctx context.Context, _ core.ChatRequest) (core.ChatResponse, error) {
	close(p.entered)
	<-ctx.Done()
	close(p.cancelled)
	<-p.release
	return core.ChatResponse{}, ctx.Err()
}

// SC-004：使用真实30/35秒预算，超时仍持有原执行与PG，释放后才返回。
// Exercise real default clocks without replacing a timeout with join evidence.
func TestFlow_DefaultCancelAndShutdownClocksKeepResourcesUntilJoin(t *testing.T) {
	fx := newWireFixture(t)
	fx.expectedCloseError = "flow drain exceeded 35s"
	fx.writeAgent(t, "reader")
	fx.cfg.Flows = flow.DefaultConfig()
	fx.cfg.Flows.Enabled = true
	protocol := &flowBlockingProtocol{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	fx.override = protocol
	rt := fx.assemble(t)
	overrun := make(chan struct{})
	rt.flows.worker.Logger = slog.New(flowDrainClockHandler{Handler: fx.logger.Handler(), fired: overrun})
	rt.adminPosture.Set(core.AdminPostureToken)
	release := sync.OnceFunc(func() { close(protocol.release) })
	t.Cleanup(release)
	definition, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-default-clocks", Definition: liveFlowDefinition(t, rt)})
	require.NoError(t, err)
	task, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: definition.DefinitionUID, IdempotencyKey: "default-clocks", Input: json.RawMessage(`{"answer":"input"}`)})
	require.NoError(t, err)
	require.NoError(t, rt.flows.Start(t.Context()))
	rt.flows.authenticationReady()
	select {
	case <-protocol.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("original Run did not start")
	}
	task, err = rt.flows.store.Read(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
	require.NoError(t, err)
	type result struct {
		task    flow.Task
		err     error
		elapsed time.Duration
	}
	cancelled := make(chan result, 1)
	started := time.Now()
	go func() {
		closed, err := rt.flows.service.Cancel(t.Context(), flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "cancel-default-clocks", Reason: "measure bounded wait"})
		cancelled <- result{closed, err, time.Since(started)}
	}()
	select {
	case <-protocol.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("original context was not cancelled")
	}
	joined := make(chan error, 1)
	drainStarted := time.Now()
	go func() { joined <- rt.flows.Close() }()
	var closed result
	select {
	case closed = <-cancelled:
	case <-time.After(32 * time.Second):
		t.Fatal("cancel exceeded its 30-second wait allowance")
	}
	require.NoError(t, closed.err)
	require.Equal(t, flow.Cancelling, closed.task.Status)
	require.GreaterOrEqual(t, closed.elapsed, 30*time.Second)
	require.Less(t, closed.elapsed, 32*time.Second)
	select {
	case <-overrun:
	case <-time.After(7 * time.Second):
		t.Fatal("worker did not report the 35-second drain overrun")
	}
	require.GreaterOrEqual(t, time.Since(drainStarted), 35*time.Second)
	select {
	case err := <-joined:
		t.Fatalf("timeout manufactured a join: %v", err)
	default:
	}
	actualJoin, err := rt.flows.runner.Joined(t.Context(), closed.task)
	require.NoError(t, err)
	require.False(t, actualJoin)
	var alive int
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT 1`).Scan(&alive))
	require.Equal(t, 1, alive)
	release()
	select {
	case err := <-joined:
		require.ErrorContains(t, err, "flow drain exceeded 35s")
	case <-time.After(6 * time.Second):
		t.Fatal("original leaf did not join after release")
	}
	actualJoin, err = rt.flows.runner.Joined(t.Context(), closed.task)
	require.NoError(t, err)
	require.True(t, actualJoin)
	t.Logf("US19_LIFECYCLE cancel_wait_ms=%.3f drain_overrun_ms=%.3f audit_cleanup_limit_ms=5000 actual_join=true pg_alive_before_join=true", float64(closed.elapsed.Microseconds())/1000, float64(time.Since(drainStarted).Microseconds())/1000)
}

type flowDrainClockHandler struct {
	slog.Handler
	fired chan struct{}
}

func (h flowDrainClockHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if record.Message == "Flow shutdown exceeded grace; waiting for real join" {
		close(h.fired)
	}
	return err
}

func liveFlowDefinition(t *testing.T, rt *heavyRuntime) flow.Definition {
	t.Helper()
	authority := rt.flows.service.Authority.(*flowAuthority)
	entry, ok := rt.agents.Lookup("reader")
	require.True(t, ok)
	life, exists, err := rt.lifecycle.Read(t.Context(), "reader")
	require.NoError(t, err)
	require.True(t, exists)
	resolved, err := authority.resources.Resolve(t.Context(), entry)
	require.NoError(t, err)
	defer resolved.Runtime.Release()
	manifest, err := core.BuildRunResourceManifest(life.AgentUID, life.State, resolved)
	require.NoError(t, err)
	snapshot, err := authority.snapshots.Build(t.Context(), entry, resolved)
	require.NoError(t, err)
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	cap := flow.BudgetCap{MaxTotalTokens: 100000, MaxToolCalls: 10, MaxDurationSeconds: 60, MaxDepth: 3, MaxChildren: 16, MaxAttemptsPerNode: 3, MaxRevisions: 2}
	return flow.Definition{FormatVersion: 1, Name: "runtime-probe", Entry: "draft", TaskContract: "TASK.json", Limits: cap, Nodes: []flow.Node{{ID: "draft", Kind: "agent", Agent: &flow.AgentRef{UID: string(life.AgentUID), Name: "reader", Version: life.Generation, ConfigHash: snapshot.Hash, ManifestHash: manifest.Hash}, OutputSchema: schema, Failure: flow.FailurePolicy{MaxAttempts: 1}}}, Contract: flow.TaskContract{ContractVersion: 1, Goal: "produce a structured answer", Constraints: []string{"preserve hard constraint"}, InputSchema: schema, OutputSchema: schema, Budget: cap, StopConditions: []string{"budget", "deadline", "unknown"}, Escalation: "show unresolved", ArtifactSpec: []flow.ArtifactSpec{{ID: "report", Format: "json", Required: true}}, Acceptance: []flow.AcceptanceRule{{ID: "shape", Version: 1, Kind: "deterministic", TargetKind: "artifact", TargetID: "report", Required: true, Schema: schema}}}}
}

func TestFlow_RuntimeConstructionAndAuthenticatedWorkerLifecycle(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader")
	fx.cfg.Flows = flow.DefaultConfig()
	fx.cfg.Flows.Enabled = true
	response := core.ChatResponse{Content: `{"answer":"runtime complete"}`, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}
	model := &scriptedProviderClient{replies: []core.ChatResponse{response, response}}
	fx.override = model
	rt := fx.assemble(t)
	require.NotNil(t, rt.flows)
	require.Nil(t, rt.flows.done)
	require.False(t, rt.flows.runner.startupComplete)
	require.Empty(t, model.requests(), "construction must not start a Worker or model")
	// The real HTTP constructor supplies this posture and calls authenticationReady.
	// This lifecycle probe drives that narrow seam without exposing a listener.
	rt.adminPosture.Set(core.AdminPostureToken)
	definition, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-runtime-probe", Definition: liveFlowDefinition(t, rt)})
	require.NoError(t, err)
	task, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: definition.DefinitionUID, IdempotencyKey: "runtime-start", Input: json.RawMessage(`{"answer":"input"}`)})
	require.NoError(t, err)
	orphan, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: definition.DefinitionUID, IdempotencyKey: "startup-orphan", Input: json.RawMessage(`{"answer":"input"}`)})
	require.NoError(t, err)
	sum := sha256.Sum256(orphan.Revision.Input)
	_, err = rt.flows.store.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: orphan.ID, Expected: orphan.Version, Now: time.Now().UnixMilli(), Attempt: flow.NodeAttempt{ID: "orphan-attempt", TaskID: orphan.ID, NodeID: "draft", NodeInvocationID: orphan.ID + ":draft", Attempt: 1, ExecutionEpoch: orphan.Version.ExecutionEpoch, RunID: "orphan-planned-run", SessionID: "orphan-planned-session", Input: orphan.Revision.Input, InputHash: hex.EncodeToString(sum[:])}})
	require.NoError(t, err)
	rt.flows.service.Config.MaxOpenTasks = 1 // Simulate a lowered admission ceiling after two roots existed.
	require.NoError(t, rt.flows.Start(t.Context()))
	orphan, err = rt.flows.store.ReadForWorker(t.Context(), orphan.ID)
	require.NoError(t, err)
	require.Equal(t, flow.Blocked, orphan.Status, "startup must park an interrupted root before any dispatch")
	require.Empty(t, model.requests(), "Worker must await the validated listener authentication posture")
	rt.flows.authenticationReady()
	require.Eventually(t, func() bool {
		current, err := rt.flows.store.ReadForWorker(t.Context(), task.ID)
		return err == nil && current.Status == flow.Succeeded
	}, 5*time.Second, 10*time.Millisecond)
	require.Len(t, model.requests(), 1)
	_, err = rt.flows.service.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: orphan.ID, Expected: orphan.Version, OperationID: "resume-startup-orphan", Reason: "original process gone; no external action issued"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		current, err := rt.flows.store.ReadForWorker(t.Context(), orphan.ID)
		return err == nil && current.Status == flow.Succeeded
	}, 5*time.Second, 10*time.Millisecond)
	require.Len(t, model.requests(), 2)
	require.NoError(t, rt.flows.Close())
	var count int
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&count))
	require.Equal(t, 2, count, "DB remains open through the actual Worker join; planned orphan was never a Run")
	joined, err := rt.flows.runner.Joined(t.Context(), task)
	require.NoError(t, err)
	require.True(t, joined)
}

func TestFlow_ConcurrentCloseWaitsForSameRealJoin(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader")
	fx.cfg.Flows = flow.DefaultConfig()
	fx.cfg.Flows.Enabled = true
	protocol := &flowBlockingProtocol{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	fx.override = protocol
	rt := fx.assemble(t)
	rt.adminPosture.Set(core.AdminPostureToken)
	releaseLeaf := sync.OnceFunc(func() { close(protocol.release) })
	t.Cleanup(releaseLeaf)
	definition, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-close-probe", Definition: liveFlowDefinition(t, rt)})
	require.NoError(t, err)
	_, err = rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: definition.DefinitionUID, IdempotencyKey: "start-close-probe", Input: json.RawMessage(`{"answer":"input"}`)})
	require.NoError(t, err)
	require.NoError(t, rt.flows.Start(t.Context()))
	rt.flows.authenticationReady()
	select {
	case <-protocol.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("real Run did not enter model protocol")
	}
	closed := make(chan error, 2)
	started := make(chan struct{}, 2)
	for range 2 {
		go func() { started <- struct{}{}; closed <- rt.flows.Close() }()
	}
	<-started
	<-started
	select {
	case <-protocol.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("close did not signal original Run")
	}
	require.ErrorIs(t, rt.flows.Start(t.Context()), flow.ErrStateConflict, "a stopping runtime must not start a second pool")
	select {
	case err := <-closed:
		t.Fatalf("Close returned without actual leaf join: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	var alive int
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT 1`).Scan(&alive))
	require.Equal(t, 1, alive)
	releaseLeaf()
	for range 2 {
		select {
		case err := <-closed:
			require.NoError(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("a concurrent Close did not receive the shared join result")
		}
	}
	require.ErrorIs(t, rt.flows.Start(t.Context()), flow.ErrStateConflict, "closed runtime cannot restart")
}
