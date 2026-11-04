//go:build integration

package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func TestFlow_ProductionWorkerFollowsOnlyBoundedPublishedRework(t *testing.T) {
	for _, mode := range []string{"correction", "rework-limit", "human-gate"} {
		t.Run(mode, func(t *testing.T) {
			f := newFlowHTTPFixture(t)
			bad := core.ChatResponse{Content: `{"answer":7}`, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}
			good := core.ChatResponse{Content: `{"answer":"corrected"}`, Usage: bad.Usage}
			f.model.mu.Lock()
			f.model.replies = []core.ChatResponse{bad, good}
			if mode == "rework-limit" {
				f.model.replies = []core.ChatResponse{bad}
			}
			if mode == "human-gate" {
				f.model.replies = []core.ChatResponse{good}
			}
			f.model.mu.Unlock()
			definition := liveFlowDefinition(t, f.rt)
			definition.Nodes[0].Failure = flow.FailurePolicy{MaxAttempts: 3, MaxReworks: 1, ReworkTarget: "draft"}
			if mode == "human-gate" {
				definition.Contract.Acceptance[0] = flow.AcceptanceRule{ID: "shape", Version: 1, Kind: "judgment", TargetKind: "artifact", TargetID: "report", Required: true, HumanRequired: true}
			}
			published, err := f.rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-rework", Definition: definition})
			require.NoError(t, err)
			task, err := f.rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: published.DefinitionUID, IdempotencyKey: "start-rework", Input: json.RawMessage(`{"answer":"input"}`)})
			require.NoError(t, err)
			var finished flow.Task
			require.Eventually(t, func() bool {
				finished, err = f.rt.flows.store.ReadForWorker(t.Context(), task.ID)
				return err == nil && (finished.Status.Terminal() || finished.Status == flow.Blocked && finished.Reason != "bounded_rework_required")
			}, 8*time.Second, 20*time.Millisecond)
			require.NoError(t, f.rt.flows.Close())
			if mode == "human-gate" {
				require.Equal(t, flow.Blocked, finished.Status)
				require.EqualValues(t, 0, finished.Version.Revision)
				require.Len(t, f.model.requests(), 1)
				return
			}
			require.EqualValues(t, 1, finished.Version.Revision)
			require.Equal(t, 1, finished.Revision.Reworks["draft"])
			require.Equal(t, 2, finished.Revision.Attempts["draft"])
			require.Len(t, f.model.requests(), 2)
			require.Equal(t, task.Deadline, finished.Deadline)
			require.Equal(t, task.Contract, finished.Contract)
			if mode == "correction" {
				require.Equal(t, flow.Succeeded, finished.Status)
			} else {
				require.Equal(t, flow.Failed, finished.Status)
			}
			var rows int
			var tokens int64
			require.NoError(t, f.rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs WHERE flow_task_id=$1`, task.ID).Scan(&rows))
			require.Equal(t, 2, rows)
			require.NoError(t, f.rt.db.QueryRowContext(t.Context(), `SELECT COALESCE(sum(actual_tokens),0) FROM flow_budget_allocations WHERE task_id=$1 AND state='settled'`, task.ID).Scan(&tokens))
			require.EqualValues(t, 16, tokens, "revision must not reset or refund prior model usage")
		})
	}
}
