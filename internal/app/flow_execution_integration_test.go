//go:build integration

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// holdFlowRootLease 替本用例在真实 PostgreSQL 上领取根执行权。US-24 起所有推进类
// 写入（Claim / Commit / ParkTask / Recover 写 Queued）都在**同一事务**里校验目标行
// 自己的 flow_root 租约（§3.1.6 / P-04），直接调 coordinator.Advance 不再免费通过；
// 生产由 flow.Worker 按真实身份领取，这里作为夹具显式持有，让用例走与生产同形的路径
// 而不是绕过执行权。
// The test holds the root lease the way the production Worker does.
func holdFlowRootLease(t *testing.T, db *postgres.DB, rootTaskID string) core.LeaseToken {
	t.Helper()
	lease, err := postgres.NewLeaseStore(db).Acquire(t.Context(), core.LeaseFlowRoot, rootTaskID,
		core.WorkerIdentity{InstanceID: "flow-execution-fixture", Name: "fixture"}, 5*time.Minute)
	require.NoError(t, err)
	return lease.LeaseToken
}

// This probe uses the production resource resolver, snapshot builder, original
// AgentService/loop/executor/provider/session/audit stores and shared admission.
// Only the model protocol client is scripted; there is no external model call.
func TestFlow_ProductionNodeUsesOriginalRunAndAudit(t *testing.T) {
	for _, scenario := range []string{"tool", "agent", "tool-checkpoint", "agent-checkpoint", "write", "write-checkpoint", "write-unknown"} {
		t.Run(scenario, func(t *testing.T) {
			kind := strings.Split(scenario, "-")[0]
			write := kind == "write"
			if write {
				kind = "tool"
			}
			fx := newWireFixture(t)
			fx.writeAgent(t, "reader", "read_file", "write_file")
			model := &scriptedProviderClient{replies: []core.ChatResponse{{Content: `{"answer":"verified structure"}`, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}}}
			fx.override = model
			rt := fx.assemble(t)
			require.NoError(t, os.WriteFile(filepath.Join(fx.root, "agents", "reader", "input.txt"), []byte("source input"), 0o600))
			catalog := rt.catalogStore.Load()
			client := modelClient(catalog, rt.llmCalls, fx.logger)
			// 轻装配显式声明"本清单不记 bundle 哈希"；资产校验没有旁路。
			// The light assembly declares "unrecorded" explicitly; assets have no bypass.
			resources := NewRunResources(rt.versions, catalog, client, rt.mcpRT, rt.skills, rt.resourceGate, AdmissionDeps{Identities: rt.versions, NotifyAdmin: postgres.NewNotifyAdminStore(rt.db), Channels: postgres.NewNotifyChannelRepo(rt.db), Bindings: postgres.NewBindingStore(rt.db), Assets: undeclaredAssets{}})
			snapshots := newRunSnapshotBuilder(workspace.NewContextLoader(fx.logger), fx.root, time.Now)
			store := postgres.NewFlowStore(rt.db)
			schemas := tool.NewSchemaCompiler()
			authority := &flowAuthority{agents: rt.agents, lifecycle: rt.lifecycle, gate: rt.agentGate, resources: resources, snapshots: snapshots, definitions: store, store: store, tools: rt.registry, schemas: schemas, posture: func() core.AdminPosture { return core.AdminPostureToken }}
			authority.newTaskCapacity = rt.flows.service.Authority.(*flowAuthority).newTaskCapacity
			authority.writeReady = write // Isolated approved G-FT probe; production remains disabled.
			entry, ok := rt.agents.Lookup("reader")
			require.True(t, ok)
			life, exists, err := rt.lifecycle.Read(t.Context(), "reader")
			require.NoError(t, err)
			require.True(t, exists)
			resolved, err := resources.Resolve(t.Context(), entry)
			require.NoError(t, err)
			manifest, err := core.BuildRunResourceManifest(life.AgentUID, life.State, resolved)
			require.NoError(t, err)
			snapshot, err := snapshots.Build(t.Context(), entry, resolved)
			require.NoError(t, err)
			resolved.Runtime.Release()
			loop := core.NewReActLoop(core.NewPromptBuilder(rt.registry), core.NewToolExecutor(rt.registry, rt.invocations, fx.logger), fx.logger).WithCallQuota(&chatQuota{limiter: rt.admission.limiter, logger: fx.logger})
			service := core.NewAgentService(core.ServiceOptions{Agents: rt.agents, Client: client, Messages: rt.sessions, Runs: rt.runs, Snapshots: postgres.NewSnapshotStore(rt.db), Audit: rt.invocations, Sessions: rt.sessions, Lifecycle: rt.lifecycle, Gate: rt.agentGate, Resources: resources, RunSnapshots: snapshots, ToolPins: rt.registry, Loop: loop, Logger: fx.logger, MaxConcurrent: 4, RunQuota: newRunQuota(rt.admission.limiter, nil, fx.logger), FlowRunsReady: true, FlowWritesReady: write})
			runner := &flowNodeRunner{service: service, admission: rt.admission, store: store, authority: authority, gate: postgres.NewSubtreeGate(rt.db), schemas: schemas, startupComplete: true, writeReady: write}
			cap := flow.BudgetCap{MaxTotalTokens: 100000, MaxToolCalls: 10, MaxDurationSeconds: 60, MaxDepth: 3, MaxChildren: 16, MaxAttemptsPerNode: 3, MaxRevisions: 2}
			inputSchema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)
			input := json.RawMessage(`{"path":"input.txt"}`)
			if write {
				inputSchema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`)
				input = json.RawMessage(`{"path":"output.txt","content":"controlled write bytes"}`)
			}
			outputSchema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
			if kind == "tool" {
				// Original tool audit omits the two false flags (synthesis.go).
				outputSchema = json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"},"retryable":{"type":"boolean"},"truncated":{"type":"boolean"}},"required":["content"],"additionalProperties":false}`)
			}
			node := flow.Node{ID: "read", Kind: kind, Agent: &flow.AgentRef{UID: string(life.AgentUID), Name: "reader", Version: life.Generation, ConfigHash: snapshot.Hash, ManifestHash: manifest.Hash}, Inputs: []flow.InputMapping{{Target: "path", Source: "input.path"}}, OutputSchema: outputSchema, Failure: flow.FailurePolicy{MaxAttempts: 1}}
			if kind == "tool" {
				node.Tool = "read_file"
				if write {
					node.Tool = "write_file"
					node.Inputs = append(node.Inputs, flow.InputMapping{Target: "content", Source: "input.content"})
				}
			}
			definition := flow.Definition{FormatVersion: 1, Name: "production-probe", Entry: "read", TaskContract: "TASK.json", Limits: cap, Nodes: []flow.Node{node}, Contract: flow.TaskContract{ContractVersion: 1, Goal: "read the pinned input", Constraints: []string{"never modify source"}, InputSchema: inputSchema, OutputSchema: outputSchema, Budget: cap, StopConditions: []string{"budget", "deadline", "unknown"}, Escalation: "show unresolved", ArtifactSpec: []flow.ArtifactSpec{{ID: "report", Format: "json", Required: true}}, Acceptance: []flow.AcceptanceRule{{ID: "shape", Version: 1, Kind: "deterministic", TargetKind: "artifact", TargetID: "report", Required: true, Schema: outputSchema}}}}
			// Isolated G-FT read-only probe; production's write gate stays closed.
			publisher := &flow.Publisher{Definitions: store, Operations: rt.ops, Validator: &flow.ContractValidator{Schemas: schemas, Resolver: authority, WriteGate: true}}
			published, err := publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-flow-probe", Definition: definition})
			require.NoError(t, err)
			cfg := flow.DefaultConfig()
			cfg.Enabled = true
			// 管理面 Recover 写 Queued 属推进类，需要根租约（US-24 §3.1.6）：
			// 夹具按真实装配注入身份与租约端口，缺这三项会被拒为 ErrFlowRootBusy。
			coordinator := &flow.Service{
				Repository: store, Definitions: store, Schemas: schemas, Authority: authority, Runner: runner, Config: cfg,
				Identity: core.WorkerIdentity{InstanceID: "flow-execution-fixture", Name: "fixture"}, Leases: postgres.NewLeaseStore(rt.db), LeaseTTL: 30 * time.Second,
			}
			task, err := coordinator.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: published.DefinitionUID, IdempotencyKey: "start", Input: input})
			require.NoError(t, err)
			root := holdFlowRootLease(t, rt.db, task.ID)
			writtenPath := filepath.Join(fx.root, "agents", "reader", "output.txt")
			if scenario == "write-unknown" {
				_, err = rt.db.ExecContext(t.Context(), `CREATE SEQUENCE flow_write_fault_seq; CREATE FUNCTION flow_write_audit_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('flow_write_fault_seq'); RAISE EXCEPTION 'flow write audit fault fired'; END $$; CREATE TRIGGER flow_write_audit_fault BEFORE UPDATE ON tool_invocations FOR EACH ROW WHEN (NEW.status='success') EXECUTE FUNCTION flow_write_audit_fault()`)
				require.NoError(t, err)
				blocked, err := coordinator.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
				require.NoError(t, err)
				require.Equal(t, flow.Blocked, blocked.Status)
				body, err := os.ReadFile(writtenPath)
				require.NoError(t, err)
				require.Equal(t, "controlled write bytes", string(body))
				var fired bool
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT is_called FROM flow_write_fault_seq`).Scan(&fired))
				require.True(t, fired)
				unknown, err := store.HasUnknownActions(t.Context(), task.ID)
				require.NoError(t, err)
				require.True(t, unknown)
				_, err = coordinator.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: blocked.Version, OperationID: "no-blind-write-replay", Reason: "query original outcome", Lease: root})
				require.ErrorIs(t, err, flow.ErrActionUnknown)
				after, err := store.ReadForWorker(t.Context(), task.ID)
				require.NoError(t, err)
				require.Equal(t, blocked.Version, after.Version)
				var calls, held int
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM tool_invocations`).Scan(&calls))
				require.Equal(t, 1, calls)
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_budget_allocations WHERE state='unknown' AND reserved_tool_calls=1`).Scan(&held))
				require.Equal(t, 1, held)
				require.Empty(t, model.requests())
				return
			}
			var writtenAt time.Time
			if strings.HasSuffix(scenario, "-checkpoint") {
				_, err = rt.db.ExecContext(t.Context(), `CREATE SEQUENCE flow_accept_fault_seq; CREATE FUNCTION flow_accept_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('flow_accept_fault_seq'); RAISE EXCEPTION 'flow acceptance fault fired'; END $$; CREATE TRIGGER flow_accept_fault BEFORE INSERT ON flow_artifacts FOR EACH ROW EXECUTE FUNCTION flow_accept_fault()`)
				require.NoError(t, err)
				_, err = coordinator.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
				require.Error(t, err)
				var fired bool
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT is_called FROM flow_accept_fault_seq`).Scan(&fired))
				require.True(t, fired)
				if write {
					info, err := os.Stat(writtenPath)
					require.NoError(t, err)
					writtenAt = info.ModTime()
				}
				_, err = rt.db.ExecContext(t.Context(), `DROP TRIGGER flow_accept_fault ON flow_artifacts`)
				require.NoError(t, err)
				task, err = store.ReadForWorker(t.Context(), task.ID)
				require.NoError(t, err)
				require.NoError(t, store.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), task.ID, task.Version, "checkpoint_unconfirmed"))
				task, err = store.ReadForWorker(t.Context(), task.ID)
				require.NoError(t, err)
				// Recreate all Flow service/adapter/store memory after the original
				// synchronous Run returned. This is not a binary-kill test.
				store = postgres.NewFlowStore(rt.db)
				runner = &flowNodeRunner{service: service, admission: rt.admission, store: store, authority: authority, gate: postgres.NewSubtreeGate(rt.db), schemas: schemas, startupComplete: true, writeReady: write}
				coordinator = &flow.Service{
					Repository: store, Definitions: store, Schemas: schemas, Authority: authority, Runner: runner, Config: cfg,
					Identity: core.WorkerIdentity{InstanceID: "flow-execution-fixture", Name: "fixture"}, Leases: postgres.NewLeaseStore(rt.db), LeaseTTL: 30 * time.Second,
				}
				// 接管恢复是**两轮收敛**（US-24 §2.3.6 / FR-24-38）：第 1 轮 Isolated=true 把
				// 子树里 prepared / running 的 attempt 收敛成 interrupted，此时 Joined 仍为
				// false，只能写 Blocked；第 2 轮 Joined=true 才放行 Queued。旧用例断言一轮
				// 即 Queued，那是进程内 Joined 时代的语义，已随本次已审设计改写。
				// Recovery converges in two rounds: isolation first, join second.
				blocked, err := coordinator.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "resume-known-run", Reason: "reconcile after checkpoint failure", Lease: root})
				require.NoError(t, err)
				require.Equal(t, flow.Blocked, blocked.Status, "第 1 轮只做隔离收敛，Joined 仍为 false")
				task, err = coordinator.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: blocked.ID, Expected: blocked.Version, OperationID: "resume-joined-round", Reason: "joined after attempt convergence", Lease: root})
				require.NoError(t, err)
				require.Equal(t, flow.Queued, task.Status)
			}
			finished, err := coordinator.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
			require.NoError(t, err)
			require.Equal(t, flow.Succeeded, finished.Status, "checkpoint=%+v logs=%s", finished.Revision, fx.logs.String())
			var runs, messages, toolCalls, modelCalls int
			require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&runs))
			require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&messages))
			require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM tool_invocations`).Scan(&toolCalls))
			require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM llm_calls`).Scan(&modelCalls))
			require.Equal(t, 1, runs)
			var attempts int
			require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_node_attempts WHERE task_id=$1`, task.ID).Scan(&attempts))
			require.Equal(t, 1, attempts, "reusing completion must not create another attempt")
			if kind == "tool" {
				require.Equal(t, 3, messages)
				if write {
					body, err := os.ReadFile(writtenPath)
					require.NoError(t, err)
					require.Equal(t, "controlled write bytes", string(body))
					if !writtenAt.IsZero() {
						info, err := os.Stat(writtenPath)
						require.NoError(t, err)
						require.Equal(t, writtenAt, info.ModTime(), "known completed write must not be repeated during checkpoint reuse")
					}
				} else {
					require.JSONEq(t, `{"content":"source input"}`, string(finished.Revision.Outputs["read"].Value))
				}
				require.Equal(t, 1, toolCalls)
				require.Zero(t, modelCalls)
				require.Empty(t, model.requests())
			} else {
				require.Equal(t, 2, messages)
				require.Zero(t, toolCalls)
				require.Equal(t, 1, modelCalls)
				require.Len(t, model.requests(), 1)
				for _, spec := range model.requests()[0].Tools {
					require.NotEqual(t, "write_file", spec.Name)
				}
				var actual int64
				require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT sum(actual_tokens) FROM flow_budget_allocations WHERE task_id=$1`, task.ID).Scan(&actual))
				require.EqualValues(t, 8, actual)
			}
			joined, err := runner.Joined(t.Context(), finished)
			require.NoError(t, err)
			require.True(t, joined)
		})
	}
}
