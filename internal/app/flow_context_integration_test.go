//go:build integration

package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// 仅在消费边界注入损坏的摘要版本；实际下游仍是生产 NodeRunner。
// Inject corrupt summary input at the consumer boundary, retaining the production runner.
type flowSummaryFaultRunner struct{ inner flow.NodeRunner }

func (r flowSummaryFaultRunner) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	req.Task.Revision.Revision++
	return r.inner.Run(ctx, req)
}

func TestFlow_ContextKeepsHardConstraintsAndFailsClosed(t *testing.T) {
	for _, mode := range []string{"revision", "summary-fault", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			fx := newWireFixture(t)
			fx.writeAgent(t, "reader")
			fx.cfg.Flows = flow.DefaultConfig()
			fx.cfg.Flows.Enabled = true
			model := &scriptedProviderClient{replies: []core.ChatResponse{
				{Content: `{"answer":7}`, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}},
				{Content: `{"answer":"valid shape but missing facts"}`, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}},
			}}
			fx.override = model
			rt := fx.assemble(t)
			rt.adminPosture.Set(core.AdminPostureToken)
			d := liveFlowDefinition(t, rt)
			d.Contract.Constraints = []string{"HARD-ONE: preserve all original authority", "HARD-TWO: never infer missing facts"}
			d.Contract.RequiredSlots = []flow.RequiredSlot{{ID: "facts", Validators: []string{"facts-check"}, MinSources: 1}}
			d.Contract.Acceptance = append(d.Contract.Acceptance, flow.AcceptanceRule{ID: "facts-check", Version: 1, Kind: "evidence", TargetKind: "slot", TargetID: "facts", Required: true, MinSources: 1})
			if mode == "revision" {
				d.Nodes[0].Failure = flow.FailurePolicy{MaxAttempts: 3, MaxReworks: 1, ReworkTarget: "draft"}
			}
			if mode == "overflow" {
				// Below the 1MiB author bound, above the original 65,536-token context.
				d.Contract.Constraints = append(d.Contract.Constraints, strings.Repeat("NONTRIMMABLE ", 30000))
			}
			pub, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-context", Definition: d})
			require.NoError(t, err)
			if mode == "summary-fault" {
				rt.flows.service.Runner = flowSummaryFaultRunner{inner: rt.flows.service.Runner}
			}
			task, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: pub.DefinitionUID, IdempotencyKey: "context", Input: json.RawMessage(`{"answer":"input"}`)})
			require.NoError(t, err)
			rt.flows.authenticationReady()
			require.NoError(t, rt.flows.Start(t.Context()))
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("flow context mode=%s: last read status=%s reason=%s err=%v", mode, task.Status, task.Reason, err)
				}
			})
			// 溢出样例携带约 390KB 硬约束：每次 ReadForWorker 都要重解定义 / 契约 / 检查点
			// （各约 390KB）。20ms 轮询在 -race 下把读侧和 Worker 互相挤慢，8s 边界实测只剩
			// 不到一秒余量（绿跑 8.42–9.41s，红跑 9.81s 超时）。轮询放宽到 100ms、等待放宽
			// 到 30s；判据不变：仍必须到达终态或非 rework 的 Blocked，且零模型调用。
			// The overflow case carries ~390KB hard state and every ReadForWorker re-parses
			// the definition, contract and checkpoint. A 20ms poll starves both sides under
			// -race, leaving under a second of margin on an 8s bound; 100ms/30s keeps the
			// same predicates with room. The cleanup line records the last read on failure.
			require.Eventually(t, func() bool {
				task, err = rt.flows.store.ReadForWorker(t.Context(), task.ID)
				return err == nil && (task.Status.Terminal() || task.Status == flow.Blocked && task.Reason != "bounded_rework_required")
			}, 30*time.Second, 100*time.Millisecond)
			require.NoError(t, rt.flows.Close())
			require.NotEqual(t, flow.Succeeded, task.Status)
			require.NotEqual(t, "verified", task.Revision.Slots["facts"].State)
			requests := model.requests()
			if mode != "revision" {
				require.Empty(t, requests, "failed summary/oversized hard state must issue zero model calls")
				var calls int
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM llm_calls`).Scan(&calls))
				require.Zero(t, calls)
				return
			}
			require.Len(t, requests, 2)
			artifacts, err := (&flowHTTP{runtime: rt.flows}).Artifacts(t.Context(), core.LocalAdminCLIPrincipal(), task.ID, "", 50)
			require.NoError(t, err)
			require.Len(t, artifacts.Items, 1)
			require.False(t, artifacts.Items[0].Readable)
			require.Empty(t, artifacts.Items[0].Content)
			require.Equal(t, "artifact_basis_unconfirmed", artifacts.Items[0].Reason, "missing verification must not be mislabeled as missing authority")
			for i, request := range requests {
				var payload struct {
					Summary flow.TaskSummary `json:"task_summary"`
				}
				var system string
				for _, message := range request.Messages {
					if message.Role == "system" {
						system += message.Content
					}
					if message.Role == core.RoleUser {
						require.NoError(t, json.Unmarshal([]byte(message.Content), &payload))
					}
				}
				require.EqualValues(t, i, payload.Summary.TaskRevision)
				require.Equal(t, d.Contract.Constraints, payload.Summary.HardConstraints)
				require.Equal(t, []string{"facts"}, payload.Summary.Unresolved)
				for _, constraint := range d.Contract.Constraints {
					require.Contains(t, system, constraint)
				}
				require.Contains(t, system, "facts")
			}
			require.Less(t, time.Now().UnixMilli(), task.Deadline)
		})
	}
}
