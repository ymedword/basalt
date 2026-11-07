//go:build integration

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

type flowEvalCase struct {
	ID, Scenario, Expected string
	Nodes                  []string
	Sources                []string `json:"source_ids"`
	Runs                   int      `json:"expected_leaf_runs"`
	Tools                  int      `json:"expected_tool_dispatches"`
}

type flowEvalScript struct {
	Reply       json.RawMessage `json:"reply"`
	Tool        string          `json:"tool"`
	Arguments   json.RawMessage `json:"arguments"`
	Hold        bool            `json:"hold_until_cancellation"`
	Repeat      bool            `json:"repeat_until_guard"`
	Hook, Fault string
	MaxTokens   int64 `json:"max_total_tokens"`
}

type flowEvalProtocol struct {
	mu      sync.Mutex
	script  flowEvalScript
	answer  string
	usage   core.Usage
	calls   int
	after   func()
	entered chan struct{}
	once    sync.Once
}

func (m *flowEvalProtocol) Chat(ctx context.Context, _ core.ChatRequest) (core.ChatResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	if m.script.Hold {
		m.once.Do(func() { close(m.entered) })
		<-ctx.Done()
		return core.ChatResponse{}, ctx.Err()
	}
	if m.after != nil {
		m.after()
	}
	if m.script.Tool != "" && (n == 1 || m.script.Repeat) {
		return core.ChatResponse{ToolCalls: []core.ToolCall{{ID: fmt.Sprintf("eval-%d", n), Name: m.script.Tool, Arguments: string(m.script.Arguments)}}, Usage: m.usage}, nil
	}
	answer := m.answer
	if len(m.script.Reply) > 0 {
		answer = string(m.script.Reply)
	}
	return core.ChatResponse{Content: answer, Usage: m.usage}, nil
}

type flowEvalReadBasis struct {
	basis  flow.VerificationBasis
	record flow.Verification
}

func (r *flowEvalReadBasis) LoadVerificationBasis(context.Context, flow.VerifyRequest) (flow.VerificationBasis, error) {
	return r.basis, nil
}

func (r *flowEvalReadBasis) RecordVerification(_ context.Context, _ flow.VerifyRequest, v flow.Verification) error {
	r.record = v
	return nil
}

// 独立评测仅适配原SourceViewer，不伪造Flow任务或持久化第二套运行/审计。
// Evaluation-only source projection reads the original source authority for both arms.
type flowEvalSources struct{ viewer *knowledge.SourceViewer }

func (s flowEvalSources) Check(ctx context.Context, p core.Principal, ref flow.EvidenceRef) (flow.SourceVerdict, error) {
	v, err := s.viewer.Check(ctx, p, core.EvidenceRef{Kind: ref.SourceKind, ID: ref.SourceUID, VersionID: ref.SourceVersion, ContentHash: ref.ContentHash})
	return flow.SourceVerdict{Valid: v.Valid, Reason: v.Reason, Version: v.VersionID, ContentHash: v.ContentHash}, err
}

type flowEvalResult struct {
	Case, Arm, Status   string
	Accepted            bool
	Runs, Models, Tools int
	KnownTokens         int `json:"known_tokens"`
	UnknownUsage        int `json:"unknown_usage_calls"`
	IllegalCalls        int `json:"illegal_calls"`
	ElapsedMS           float64
}

// F19：12组真实装配/原Run/原审计/PG，只有外部模型协议为冻结脚本。
// This compares control correctness and recorded cost, never real-model quality.
func TestFlow_DemoContractAndArtifacts(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(path string) []byte {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		require.NoError(t, err)
		return raw
	}
	var catalog struct {
		Version    string `json:"corpus_version"`
		Input      json.RawMessage
		Constraint string `json:"hard_constraint"`
		Cases      []flowEvalCase
	}
	require.NoError(t, json.Unmarshal(read("testdata/flow/workloads.json"), &catalog))
	require.Equal(t, "us19-local-control-v1", catalog.Version)
	var scripts struct {
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage_per_chat"`
		Scripts map[string]flowEvalScript
	}
	require.NoError(t, json.Unmarshal(read("evals/flow/model-scripts.json"), &scripts))
	answer := string(read("testdata/flow/corpus/policy-v1.json"))
	var baseline struct {
		MaxTools   int   `json:"max_tool_calls"`
		MaxTokens  int64 `json:"max_total_tokens"`
		MaxSeconds int64 `json:"max_duration_seconds"`
	}
	require.NoError(t, json.Unmarshal(read("evals/flow/baseline.json"), &baseline))
	require.Equal(t, int64(200000), baseline.MaxTokens)
	require.Equal(t, 200, baseline.MaxTools)
	require.Equal(t, int64(1800), baseline.MaxSeconds)
	require.Len(t, catalog.Cases, 12)
	var report []flowEvalResult
	for _, item := range catalog.Cases {
		for _, arm := range []string{"flow", "single-agent"} {
			t.Run(item.ID+"/"+arm, func(t *testing.T) {
				script, ok := scripts.Scripts[item.Scenario]
				require.True(t, ok)
				fx := newWireFixture(t)
				fx.cfg.Flows = flow.DefaultConfig()
				fx.cfg.Flows.Enabled = true
				tools := []string{}
				if script.Tool != "" {
					tools = append(tools, script.Tool)
				}
				fx.writeAgent(t, "reader", tools...)
				if item.Scenario == "two-agent-handoff" {
					fx.writeAgent(t, "reviewer")
				}
				refs := flowEvalSeedSources(t, fx, item.Sources, read)
				model := &flowEvalProtocol{script: script, answer: answer, usage: core.Usage{PromptTokens: scripts.Usage.Prompt, CompletionTokens: scripts.Usage.Completion, TotalTokens: scripts.Usage.Total}, entered: make(chan struct{})}
				fx.override = model
				rt := fx.assemble(t)
				rt.adminPosture.Set(core.AdminPostureToken)
				require.NoError(t, rt.flows.prepare(t.Context()))
				require.NoError(t, os.WriteFile(filepath.Join(fx.root, "agents", "reader", "policy.json"), []byte(answer), 0o600))
				if script.Hook == "revoke-source-after-chat" {
					model.after = func() {
						_, err := rt.db.ExecContext(t.Context(), `UPDATE kb_documents SET deleted_at=2 WHERE document_id='policy-current'`)
						require.NoError(t, err)
					}
				}
				definition := flowEvalDefinition(t, rt, item, script, catalog.Constraint, refs)
				if script.Fault == "original-tool-terminal-after-effect" {
					_, err := rt.db.ExecContext(t.Context(), `CREATE SEQUENCE eval_fault_seq; CREATE FUNCTION eval_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('eval_fault_seq'); RAISE EXCEPTION 'frozen eval fault'; END $$; CREATE TRIGGER eval_fault BEFORE UPDATE ON tool_invocations FOR EACH ROW WHEN (NEW.status='success') EXECUTE FUNCTION eval_fault()`)
					require.NoError(t, err)
				}
				started := time.Now()
				result := flowEvalResult{Case: item.ID, Arm: arm}
				var output json.RawMessage
				if arm == "flow" {
					published, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-eval", Definition: definition})
					require.NoError(t, err)
					task, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: published.DefinitionUID, IdempotencyKey: "eval", Input: catalog.Input})
					require.NoError(t, err)
					if script.Hold {
						// 调度在**测试 goroutine** 里领租约：require 不得从子 goroutine
						// FailNow。取消走关闭类写入，不需要根租约，所以持有期间 Cancel
						// 照常生效（US-24 §2.3.6 写入分档）。
						root := acquireFlowRootForTest(t, rt.db, task.RootTaskID)
						done := make(chan error, 1)
						go func() {
							_, err := rt.flows.service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
							done <- err
						}()
						select {
						case <-model.entered:
						// 3s → 15s（2026-11-06 用户裁决：并行判卷下余量不足会让整轮门禁重跑）。
						// 只放宽等待上限，断言不变。
						case <-time.After(15 * time.Second):
							t.Fatal("cancel script did not start")
						}
						task, err = rt.flows.store.ReadForWorker(t.Context(), task.ID)
						require.NoError(t, err)
						_, err = rt.flows.service.Cancel(t.Context(), flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "cancel-eval", Reason: "frozen cancellation"})
						require.NoError(t, err)
						require.NoError(t, <-done)
						releaseFlowRootForTest(t, rt.db, root)
						task, err = rt.flows.store.ReadForWorker(t.Context(), task.ID)
						require.NoError(t, err)
					} else {
						if script.Fault == "artifact-insert-before-commit" {
							_, err = rt.db.ExecContext(t.Context(), `CREATE SEQUENCE eval_fault_seq; CREATE FUNCTION eval_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('eval_fault_seq'); RAISE EXCEPTION 'frozen eval fault'; END $$; CREATE TRIGGER eval_fault BEFORE INSERT ON flow_artifacts FOR EACH ROW EXECUTE FUNCTION eval_fault()`)
							require.NoError(t, err)
							_, err = advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
							require.Error(t, err)
							_, err = rt.db.ExecContext(t.Context(), `DROP TRIGGER eval_fault ON flow_artifacts`)
							require.NoError(t, err)
							task, err = rt.flows.store.ReadForWorker(t.Context(), task.ID)
							require.NoError(t, err)
							require.NoError(t, rt.flows.store.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), task.ID, task.Version, "eval_checkpoint"))
							task, err = rt.flows.store.ReadForWorker(t.Context(), task.ID)
							require.NoError(t, err)
							// 两轮收敛（US-24 §2.3.6 / FR-24-38）：第一轮以 Isolated=true 把
							// 子树里残留的 running attempt 收敛成 interrupted，结论仍是 Blocked；
							// 第二轮没有 in-flight attempt 才允许写 Queued。单个 Recover 停在
							// Blocked 是设计行为，不是失败——旧用例断言一轮即 Queued，那是进程内
							// Joined 时代的语义。
							task, err = rt.flows.service.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "recover-eval", Reason: "original completed result"})
							require.NoError(t, err)
							require.Equal(t, flow.Blocked, task.Status, "第一轮只收敛子树，不放行推进")
							task, err = rt.flows.service.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "recover-eval-joined", Reason: "original completed result"})
							require.NoError(t, err)
							require.Equal(t, flow.Queued, task.Status, "第二轮 joined 成立才写 Queued")
						}
						for n := 0; n < 8 && !task.Status.Terminal() && task.Status != flow.Blocked; n++ {
							task, err = advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
							require.NoError(t, err, "reason=%s", task.Reason)
						}
						graph, err := rt.flows.store.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
						require.NoError(t, err)
						if len(graph.Artifacts) > 0 {
							output = graph.Artifacts[len(graph.Artifacts)-1].Content
						}
					}
					result.Status = string(task.Status)
					require.Equal(t, item.Expected, result.Status, "reason=%s", task.Reason)
				} else {
					ctx, cancel := context.WithTimeout(t.Context(), time.Duration(baseline.MaxSeconds)*time.Second)
					defer cancel()
					if script.Hold {
						go func() {
							select {
							case <-model.entered:
								cancel()
							case <-ctx.Done():
							}
						}()
					}
					run, err := rt.admission.run(ctx, core.RunRequest{Principal: core.LocalAdminCLIPrincipal(), Agent: "reader", SessionID: "eval-baseline", Channel: "cli", Message: catalog.Constraint + "\n" + string(catalog.Input), Budget: core.Budget{MaxToolCalls: baseline.MaxTools, MaxIterations: 200}}, rt.service.Run)
					require.NoError(t, err)
					result.Status = string(run.Status)
					output = json.RawMessage(run.FinalText)
				}
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&result.Runs))
				usage, err := rt.db.QueryContext(t.Context(), `SELECT COALESCE(prompt_tokens,0),COALESCE(completion_tokens,0),COALESCE(total_tokens,0) FROM llm_calls`)
				require.NoError(t, err)
				for usage.Next() {
					var u core.Usage
					require.NoError(t, usage.Scan(&u.PromptTokens, &u.CompletionTokens, &u.TotalTokens))
					result.Models++
					total, known, err := core.FlowUsageTotal(u)
					if err != nil || !known {
						result.UnknownUsage++
					} else {
						result.KnownTokens += int(total)
					}
				}
				require.NoError(t, usage.Err())
				require.NoError(t, usage.Close())
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM tool_invocations`).Scan(&result.Tools))
				flowEvalCallSet(t, rt, item, script, arm, &result)
				result.ElapsedMS = float64(time.Since(started).Microseconds()) / 1000
				result.Accepted = flowEvalAccepted(t, rt, definition, refs, output, result)
				require.Equal(t, item.Expected == "succeeded", result.Accepted, "same frozen verdict must reject unsafe completion")
				if arm == "flow" {
					require.Equal(t, item.Runs, result.Runs)
					require.Equal(t, item.Tools, result.Tools)
				} else {
					require.Equal(t, 1, result.Runs)
					if item.Scenario == "unknown-write" {
						require.Equal(t, 1, result.Tools, "unknown terminal audit cannot cause another write in the frozen script")
					}
				}
				if script.Fault == "original-tool-terminal-after-effect" || arm == "flow" && script.Fault == "artifact-insert-before-commit" {
					var fired bool
					require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT is_called FROM eval_fault_seq`).Scan(&fired))
					require.True(t, fired)
				}
				report = append(report, result)
			})
		}
	}
	require.Len(t, report, 24)
	matched, falseSuccess, illegal := 0, 0, 0
	for _, r := range report {
		illegal += r.IllegalCalls
		if r.Arm == "flow" {
			matched++
			if r.Status == "succeeded" && !r.Accepted {
				falseSuccess++
			}
		}
	}
	require.Equal(t, 12, matched)
	require.Zero(t, falseSuccess)
	require.Zero(t, illegal)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	t.Logf("US19_FROZEN_COMPARISON corpus=us19-local-control-v1 control_expected=%d/12 false_success=%d illegal_calls=%d real_model_quality=not_tested results=%s", matched, falseSuccess, illegal, raw)
}

func flowEvalCallSet(t *testing.T, rt *heavyRuntime, item flowEvalCase, script flowEvalScript, arm string, result *flowEvalResult) {
	t.Helper()
	rows, err := rt.db.QueryContext(t.Context(), `SELECT tool_name,COALESCE(input_json,'') FROM tool_invocations ORDER BY id`)
	require.NoError(t, err)
	for rows.Next() {
		var name, input string
		require.NoError(t, rows.Scan(&name, &input))
		actual, actualErr := flow.CanonicalJSON(t.Context(), []byte(input))
		expected, expectedErr := flow.CanonicalJSON(t.Context(), script.Arguments)
		if name != script.Tool || actualErr != nil || expectedErr != nil || string(actual) != string(expected) {
			result.IllegalCalls++
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	if result.Tools > 200 {
		result.IllegalCalls += result.Tools - 200
	}
	if item.Scenario == "unknown-write" && result.Tools != 1 {
		result.IllegalCalls++
	}
	if script.Tool == "" && result.Tools != 0 {
		result.IllegalCalls += result.Tools
	}
	if arm == "flow" {
		rows, err = rt.db.QueryContext(t.Context(), `SELECT n.node_id,r.agent,COALESCE(r.session_id,'') FROM flow_node_attempts n JOIN runs r ON r.run_id=n.run_id AND r.flow_task_id=n.task_id AND r.flow_node_attempt_id=n.node_attempt_id ORDER BY n.started_at,n.node_attempt_id`)
		require.NoError(t, err)
		var nodes, agents, sessions []string
		for rows.Next() {
			var node, agent, session string
			require.NoError(t, rows.Scan(&node, &agent, &session))
			nodes = append(nodes, node)
			agents = append(agents, agent)
			sessions = append(sessions, session)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		want := item.Nodes
		if item.Scenario == "exclusive-branch" {
			want = []string{"decide", "chosen"}
		}
		require.Equal(t, want, nodes, "compare actual original Run node identities, not merely counts")
		if item.Scenario == "two-agent-handoff" {
			require.Equal(t, []string{"reader", "reviewer"}, agents)
			require.NotEqual(t, sessions[0], sessions[1])
			require.NotEmpty(t, sessions[0])
			require.NotEmpty(t, sessions[1])
		}
	}
	require.Zero(t, result.IllegalCalls, "actual original audit must match frozen tool name, parameters and bounded dispatch set")
}

func flowEvalSeedSources(t *testing.T, fx *wireFixture, ids []string, read func(string) []byte) []flow.EvidenceRef {
	t.Helper()
	if len(ids) == 0 {
		return nil
	}
	db, err := postgres.Open(t.Context(), postgres.Config{DSN: fx.dsn})
	require.NoError(t, err)
	collection, err := postgres.NewKnowledgeCatalogStore(db).CreateCollection(t.Context(), core.LocalAdminCLIPrincipal(), "eval-data", "")
	require.NoError(t, err)
	var refs []flow.EvidenceRef
	for _, id := range ids {
		name := "policy-v1.json"
		if id == "policy-conflicting" {
			name = "policy-conflict-v1.json"
		}
		text := strings.TrimSpace(string(read("testdata/flow/corpus/" + name)))
		sum := sha256.Sum256([]byte(text))
		hash := hex.EncodeToString(sum[:])
		version := id + "-v1"
		_, err = db.ExecContext(t.Context(), `INSERT INTO kb_documents(document_id,collection_id,source_path,content_hash,created_at,updated_at) VALUES($1,$2,$3,$4,1,1)`, id, collection.CollectionID, name, hash)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO kb_document_versions(version_id,document_id,content_hash,status,parser_version,chunker_version,source_text,created_at,updated_at) VALUES($1,$2,$3,'active','plain-v1','v1',$4,1,1)`, version, id, hash, text)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `UPDATE kb_documents SET latest_version_id=$2 WHERE document_id=$1`, id, version)
		require.NoError(t, err)
		refs = append(refs, flow.EvidenceRef{SourceKind: core.EvidenceKindKnowledge, SourceUID: id, SourceVersion: version, ContentHash: hash, Location: "source:" + id})
	}
	require.NoError(t, db.Close())
	for _, agent := range fx.agents {
		path := filepath.Join(fx.root, "agents", agent, "AGENT.md")
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "\nprovider:", "\nknowledge:\n  - name: eval-data\n    access: read\nprovider:", 1)), 0o600))
	}
	return refs
}

func flowEvalDefinition(t *testing.T, rt *heavyRuntime, item flowEvalCase, script flowEvalScript, constraint string, refs []flow.EvidenceRef) flow.Definition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "flow", "definitions.json"))
	require.NoError(t, err)
	var templates map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &templates))
	raw, found := templates[item.Scenario]
	require.True(t, found)
	var bundle map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &bundle))
	contract, found := bundle["contract"]
	require.True(t, found)
	delete(bundle, "contract")
	definitionBytes, err := json.Marshal(bundle)
	require.NoError(t, err)
	d, err := flow.DecodeDefinitionJSON(t.Context(), definitionBytes)
	require.NoError(t, err)
	d.Contract, err = flow.DecodeTaskContractJSON(t.Context(), contract)
	require.NoError(t, err)
	require.Equal(t, []string{constraint}, d.Contract.Constraints)
	require.Equal(t, item.Nodes[0], d.Entry)
	if script.MaxTokens > 0 {
		require.Equal(t, script.MaxTokens, d.Limits.MaxTotalTokens)
	}
	d.Contract.InputRefs = refs
	authority := rt.flows.service.Authority.(*flowAuthority)
	pins := map[string]*flow.AgentRef{}
	for i := range d.Nodes {
		node := &d.Nodes[i]
		if node.Agent == nil {
			continue
		}
		name := node.Agent.Name
		pin, found := pins[name]
		if !found {
			entry, ok := rt.agents.Lookup(name)
			require.True(t, ok)
			life, exists, err := rt.lifecycle.Read(t.Context(), name)
			require.NoError(t, err)
			require.True(t, exists)
			resources, err := authority.resources.Resolve(t.Context(), entry)
			require.NoError(t, err)
			manifest, manifestErr := core.BuildRunResourceManifest(life.AgentUID, life.State, resources)
			snapshot, snapshotErr := authority.snapshots.Build(t.Context(), entry, resources)
			resources.Runtime.Release()
			require.NoError(t, manifestErr)
			require.NoError(t, snapshotErr)
			pin = &flow.AgentRef{UID: string(life.AgentUID), Name: name, Version: life.Generation, ConfigHash: snapshot.Hash, ManifestHash: manifest.Hash}
			pins[name] = pin
		}
		node.Agent = pin
	}
	return d
}

func flowEvalAccepted(t *testing.T, rt *heavyRuntime, d flow.Definition, refs []flow.EvidenceRef, output json.RawMessage, result flowEvalResult) bool {
	t.Helper()
	if result.Status != "succeeded" && result.Status != "completed" {
		return false
	}
	var unknown int
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM tool_invocations WHERE status IN ('pending','unknown'))+(SELECT count(*) FROM llm_calls WHERE status IN ('pending','unknown'))`).Scan(&unknown))
	if unknown > 0 || result.UnknownUsage > 0 || int64(result.KnownTokens) > d.Contract.Budget.MaxTotalTokens {
		return false
	}
	if len(output) == 0 {
		return false
	}
	// Same production VerificationService and exact source-value implementation for both arms.
	viewer := rt.flows.service.Verification.Sources.(flowSourceValidity).viewer
	sources := flowEvalSources{viewer: viewer}
	for _, rule := range d.Contract.Acceptance {
		basis := flow.VerificationBasis{TaskID: "eval-original-run", TaskRevision: 0, TargetKind: rule.TargetKind, TargetID: rule.TargetID, TargetVersion: 1, ContractVersion: 1, Rule: rule, Output: output, ContentHash: ""}
		sum := sha256.Sum256(output)
		basis.ContentHash = hex.EncodeToString(sum[:])
		for i, ref := range refs {
			ref.ID = fmt.Sprint("eval-ref-", i)
			ref.TaskID = basis.TaskID
			basis.Evidence = append(basis.Evidence, ref)
		}
		repo := &flowEvalReadBasis{basis: basis}
		validator := &flow.VerificationService{Repository: repo, Schemas: rt.flows.service.Schemas, Sources: sources, Support: flowSourceValueSupport{viewer: viewer, sources: sources, check: func(ctx context.Context, _ core.Principal, _ flow.VerificationBasis, _ flow.EvidenceRef) error {
			return ctx.Err()
		}}}
		v, err := validator.Verify(t.Context(), flow.VerifyRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: basis.TaskID, NodeAttemptID: "eval-read-only-original-result", TargetKind: rule.TargetKind, TargetID: rule.TargetID, TargetVersion: 1, ContractVersion: 1, RuleID: rule.ID, RuleVersion: rule.Version})
		if err != nil || v.Outcome != "pass" {
			return false
		}
	}
	return true
}
