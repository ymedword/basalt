//go:build integration

package app

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// 观测只依赖真实只读账本；读取不得创建新 Run 或提升调用方权限。
// Observation reads original ledgers without advancing work or elevating callers.
func TestFlow_DiagnosticsDoNotAdvanceTask(t *testing.T) {
	f := newFlowHTTPFixture(t)
	published, err := f.rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-observation", Definition: liveFlowDefinition(t, f.rt)})
	require.NoError(t, err)
	task, err := f.rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: published.DefinitionUID, IdempotencyKey: "observe", Input: json.RawMessage(`{"answer":"PRIVATE_TASK_BODY"}`)})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		task, err = f.rt.flows.store.ReadForWorker(t.Context(), task.ID)
		return err == nil && task.Status == flow.Succeeded
	}, 5*time.Second, 20*time.Millisecond)
	store := postgres.NewObservabilityStore(f.rt.db)
	ids, err := store.RunsByFlowTaskID(t.Context(), task.ID)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	link, err := store.FlowAssociation(t.Context(), ids[0])
	require.NoError(t, err)
	require.Equal(t, task.ID, link.TaskID)
	require.NotEmpty(t, link.NodeAttemptID)
	projector := observability.NewProjector(observability.Ports{Runs: store, Exec: store}, observability.ProjectorConfig{SampleRate: 1})
	for range 3 {
		view, err := projector.Timeline(t.Context(), observability.TimelineQuery{Anchor: observability.Anchor{Kind: observability.AnchorFlowTask, Value: task.ID}, Limit: 200})
		require.NoError(t, err)
		require.NotEmpty(t, view.Entries)
		for _, entry := range view.Entries {
			require.NotNil(t, entry.Flow)
			require.Equal(t, task.ID, entry.Flow.TaskID)
		}
		raw, err := json.Marshal(view)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "PRIVATE_TASK_BODY")
	}
	after, err := f.rt.flows.store.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, task.Version, after.Version)
	require.Len(t, f.model.requests(), 1)
	owner := f.issue("cal_observer", true, core.ScopeFlowsRead)
	status, _, _, _ := f.request(http.MethodGet, "/api/v1/observability/timeline?flow_task_id="+task.ID, owner, "", "", nil)
	require.Equal(t, http.StatusForbidden, status)
}
