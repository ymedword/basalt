//go:build integration

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

type flowProofModel struct {
	inner *scriptedProviderClient
	after func()
}

type flowProofReadHook struct {
	viewer flowProofViewer
	after  func()
}

func (h flowProofReadHook) SourceView(ctx context.Context, p core.Principal, req knowledge.SourceViewRequest) (knowledge.SourceViewResult, error) {
	result, err := h.viewer.SourceView(ctx, p, req)
	if err == nil {
		h.after()
	}
	return result, err
}

func (m flowProofModel) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	value, err := m.inner.Chat(ctx, req)
	if m.after != nil {
		m.after()
	}
	return value, err
}

// Real app composition, source reader/ACL, original Run/audit and claim/artifact
// transaction; only Chat's external protocol is scripted, with no paid calls.
func TestFlow_ProductionSourceVerificationAndClaimProjection(t *testing.T) {
	for _, mode := range []string{"equal", "conflict", "source-revoked-in-chat", "source-revoked-in-verification", "outside-frozen-scope", "support-temporarily-unavailable", "unaffected-upstream-revision", "used-optional-proposal", "unused-optional-proposal", "accepted-through-unclaimed-middle", "claim-pass-artifact-human"} {
		t.Run(mode, func(t *testing.T) {
			fx := newWireFixture(t)
			seed, err := postgres.Open(t.Context(), postgres.Config{DSN: fx.dsn})
			require.NoError(t, err)
			collection, err := postgres.NewKnowledgeCatalogStore(seed).CreateCollection(t.Context(), core.LocalAdminCLIPrincipal(), "proof-data", "")
			require.NoError(t, err)
			text := `{"answer":"exact source value"}`
			digest := sha256.Sum256([]byte(text))
			hash := hex.EncodeToString(digest[:])
			_, err = seed.ExecContext(t.Context(), `INSERT INTO kb_documents(document_id,collection_id,source_path,content_hash,created_at,updated_at) VALUES('doc-proof',$1,'proof.txt',$2,1,1)`, collection.CollectionID, hash)
			require.NoError(t, err)
			_, err = seed.ExecContext(t.Context(), `INSERT INTO kb_document_versions(version_id,document_id,content_hash,status,parser_version,chunker_version,source_text,created_at,updated_at) VALUES('ver-proof','doc-proof',$1,'active','plain-v1','v1',$2,1,1)`, hash, text)
			require.NoError(t, err)
			_, err = seed.ExecContext(t.Context(), `UPDATE kb_documents SET latest_version_id='ver-proof' WHERE document_id='doc-proof'`)
			require.NoError(t, err)
			require.NoError(t, seed.Close())
			fx.writeAgent(t, "reader")
			if mode != "outside-frozen-scope" {
				path := filepath.Join(fx.root, "agents", "reader", "AGENT.md")
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				raw = []byte(strings.Replace(string(raw), "\nprovider:", "\nknowledge:\n  - name: proof-data\n    access: read\nprovider:", 1))
				require.NoError(t, os.WriteFile(path, raw, 0o600))
			}
			fx.cfg.Flows = flow.DefaultConfig()
			fx.cfg.Flows.Enabled = true
			output := text
			if mode == "conflict" || strings.HasSuffix(mode, "optional-proposal") {
				output = `{"answer":"unproven inferred answer"}`
			}
			model := &scriptedProviderClient{replies: []core.ChatResponse{{Content: output, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}}}
			if mode == "unaffected-upstream-revision" {
				model.replies = append(model.replies, core.ChatResponse{Content: `{"answer":"needs correction"}`, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}, model.replies[0])
			}
			if strings.HasSuffix(mode, "optional-proposal") {
				model.replies = append(model.replies, core.ChatResponse{Content: text, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}})
			}
			protocol := &flowProofModel{inner: model}
			fx.override = protocol
			rt := fx.assemble(t)
			rt.adminPosture.Set(core.AdminPostureToken)
			require.NoError(t, rt.flows.prepare(t.Context()))
			if mode == "source-revoked-in-chat" {
				protocol.after = func() {
					_, err := rt.db.ExecContext(t.Context(), `UPDATE kb_documents SET deleted_at=2 WHERE document_id='doc-proof'`)
					require.NoError(t, err)
				}
			}
			if mode == "source-revoked-in-verification" {
				checker := rt.flows.service.Verification.Support.(flowSourceValueSupport)
				checker.viewer = flowProofReadHook{viewer: checker.viewer, after: func() {
					_, err := rt.db.ExecContext(t.Context(), `UPDATE kb_documents SET deleted_at=2 WHERE document_id='doc-proof'`)
					require.NoError(t, err)
				}}
				rt.flows.service.Verification.Support = checker
			}
			definition := liveFlowDefinition(t, rt)
			definition.Contract.InputRefs = []flow.EvidenceRef{{SourceKind: core.EvidenceKindKnowledge, SourceUID: "doc-proof", SourceVersion: "ver-proof", ContentHash: hash, Location: "source:doc-proof"}}
			definition.Contract.Acceptance = append(definition.Contract.Acceptance, flow.AcceptanceRule{ID: "source-proof", Version: 1, Kind: "evidence", TargetKind: "claim", TargetID: "draft", Required: true, MinSources: 1, ValidatorID: flowSourceValueValidator})
			if mode == "claim-pass-artifact-human" {
				definition.Contract.Acceptance[0] = flow.AcceptanceRule{ID: "shape", Version: 1, Kind: "judgment", TargetKind: "artifact", TargetID: "report", Required: true, HumanRequired: true}
			}
			if mode == "unaffected-upstream-revision" {
				final := definition.Nodes[0]
				final.ID = "final"
				final.Inputs = []flow.InputMapping{{Target: "answer", Source: "outputs.draft.answer"}}
				final.Failure.MaxAttempts = 3
				definition.Nodes[0].Next = "final"
				definition.Nodes = append(definition.Nodes, final)
				definition.Contract.Acceptance[0].Schema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string","enum":["exact source value"]}},"required":["answer"],"additionalProperties":false}`)
			}
			if strings.HasSuffix(mode, "optional-proposal") || mode == "accepted-through-unclaimed-middle" {
				final := definition.Nodes[0]
				final.ID = "final"
				if mode != "unused-optional-proposal" {
					final.Inputs = []flow.InputMapping{{Target: "answer", Source: "outputs.draft.answer"}}
				}
				definition.Nodes[0].Next = "final"
				definition.Nodes = append(definition.Nodes, final)
				if strings.HasSuffix(mode, "optional-proposal") {
					definition.Contract.Acceptance[1].Required = false
				}
				if mode == "accepted-through-unclaimed-middle" {
					definition.Nodes[1].ID = "middle"
					definition.Nodes[1].Next = "final"
					definition.Nodes[0].Next = "middle"
					final.Inputs = []flow.InputMapping{{Target: "answer", Source: "outputs.middle.answer"}}
					definition.Nodes = append(definition.Nodes, final)
				}
			}
			published, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-source-proof", Definition: definition})
			require.NoError(t, err)
			task, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: published.DefinitionUID, IdempotencyKey: "start-source-proof", Input: json.RawMessage(`{"answer":"input"}`)})
			require.NoError(t, err)
			support := rt.flows.service.Verification.Support
			if mode == "support-temporarily-unavailable" {
				rt.flows.service.Verification.Support = nil
			}
			finished, err := advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
			if mode == "outside-frozen-scope" {
				require.Error(t, err)
				require.Empty(t, model.requests(), "out-of-scope contract source must stop before model dispatch")
				return
			}
			require.NoError(t, err)
			graph, err := rt.flows.store.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
			require.NoError(t, err)
			require.Len(t, graph.Claims, 1, "status=%s reason=%s models=%d logs=%s", finished.Status, finished.Reason, len(model.requests()), fx.logs.String())
			if mode == "claim-pass-artifact-human" {
				require.Equal(t, flow.Blocked, finished.Status)
				require.Equal(t, "accepted", graph.Claims[0].State)
				require.Len(t, graph.Artifacts, 1)
				require.Equal(t, "proposed", graph.Artifacts[0].State)
				resumed, err := rt.flows.service.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: finished.Version, OperationID: "resume-artifact-human", Reason: "recheck original result without approving human gate"})
				require.NoError(t, err)
				finished, err = advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: resumed.Version})
				require.NoError(t, err)
				require.Equal(t, flow.Blocked, finished.Status)
				require.Len(t, model.requests(), 1, "missing human verification cannot cause a second model call or a false pass")
				graph, err = rt.flows.store.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
				require.NoError(t, err)
				require.Len(t, graph.Claims, 2)
				require.Len(t, graph.Artifacts, 2)
				require.Equal(t, "accepted", graph.Claims[1].State)
				require.Equal(t, "proposed", graph.Artifacts[1].State)
				return
			}
			if strings.HasSuffix(mode, "optional-proposal") || mode == "accepted-through-unclaimed-middle" {
				require.Equal(t, flow.Running, finished.Status)
				if strings.HasSuffix(mode, "optional-proposal") {
					require.False(t, finished.Revision.Outputs["draft"].Verified)
				}
				finished, err = advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: finished.Version})
				if mode == "used-optional-proposal" {
					require.Error(t, err)
					require.Len(t, model.requests(), 1)
					require.Empty(t, graph.Artifacts)
					return
				}
				require.NoError(t, err)
				if mode == "accepted-through-unclaimed-middle" {
					require.Equal(t, flow.Running, finished.Status)
					finished, err = advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: finished.Version})
					require.NoError(t, err)
				}
				require.Equal(t, flow.Succeeded, finished.Status)
				graph, err = rt.flows.store.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
				require.NoError(t, err)
				require.Len(t, graph.Artifacts, 1)
				if mode == "unused-optional-proposal" {
					require.Empty(t, graph.Artifacts[0].Claims)
					require.Len(t, model.requests(), 2)
				} else {
					require.Equal(t, []flow.ClaimRef{{ID: "draft", Revision: 1}}, graph.Artifacts[0].Claims)
					require.Len(t, model.requests(), 3)
				}
				return
			}
			if mode == "unaffected-upstream-revision" {
				require.Equal(t, flow.Running, finished.Status)
				finished, err = advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: finished.Version})
				require.NoError(t, err)
				require.Equal(t, flow.Blocked, finished.Status)
				revised, err := rt.flows.service.Revise(t.Context(), flow.RevisionRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: finished.Version, OperationID: "revise-final-only", Proposal: json.RawMessage(`{"reason":"correct final artifact","node_id":"final"}`)})
				require.NoError(t, err)
				require.EqualValues(t, 1, revised.Version.Revision)
				require.Equal(t, finished.Revision.Outputs["draft"], revised.Revision.Outputs["draft"])
				finished, err = advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: revised.Version})
				require.NoError(t, err)
				require.Equal(t, flow.Succeeded, finished.Status, "reason=%s node=%s attempts=%v calls=%d", finished.Reason, finished.Revision.NodeID, finished.Revision.Attempts, len(model.requests()))
				graph, err = rt.flows.store.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
				require.NoError(t, err)
				require.Len(t, graph.Claims, 1)
				require.Len(t, graph.Artifacts, 2)
				require.Equal(t, "accepted", graph.Artifacts[1].State)
				require.Equal(t, []flow.ClaimRef{{ID: "draft", Revision: 1}}, graph.Artifacts[1].Claims)
				require.Len(t, model.requests(), 3, "only final producer may rerun; the accepted upstream Run must be retained")
				return
			}
			if mode == "support-temporarily-unavailable" {
				require.Equal(t, flow.Blocked, finished.Status)
				require.Equal(t, "proposed", graph.Claims[0].State)
				rt.flows.service.Verification.Support = support
				resumed, err := rt.flows.service.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: finished.Version, OperationID: "resume-source-proof", Reason: "validator available; recheck original output"})
				require.NoError(t, err)
				finished, err = advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: resumed.Version})
				require.NoError(t, err)
				require.Equal(t, flow.Succeeded, finished.Status)
				graph, err = rt.flows.store.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
				require.NoError(t, err)
				require.Len(t, graph.Claims, 2)
				require.Equal(t, "accepted", graph.Claims[1].State)
				var attempts int
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_node_attempts WHERE task_id=$1`, task.ID).Scan(&attempts))
				require.Equal(t, 1, attempts, "verification recovery must reuse the original completed leaf")
			}
			if mode == "equal" {
				require.Equal(t, flow.Succeeded, finished.Status)
				require.Equal(t, "accepted", graph.Claims[0].State)
				require.Len(t, graph.Artifacts, 1)
				require.Equal(t, []flow.ClaimRef{{ID: "draft", Revision: 1}}, graph.Artifacts[0].Claims)
			} else if mode != "support-temporarily-unavailable" {
				require.Equal(t, flow.Blocked, finished.Status)
				require.NotEqual(t, "accepted", graph.Claims[0].State)
				for _, artifact := range graph.Artifacts {
					require.NotEqual(t, "accepted", artifact.State)
				}
			}
			require.Len(t, model.requests(), 1)
			var calls, runs int
			require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&runs))
			require.Equal(t, 1, runs)
			require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM llm_calls`).Scan(&calls))
			require.Equal(t, 1, calls, "verification must not dispatch a second model or create its own call ledger")
		})
	}
}
