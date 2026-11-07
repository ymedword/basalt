package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

type fixedNoModelRuntime struct{ lookups, admissionReleases, releases int }

func (r *fixedNoModelRuntime) Provider(string) (core.ProviderClient, bool) {
	r.lookups++
	return nil, false
}
func (*fixedNoModelRuntime) MCP(string) (core.MCPHandle, bool) { return nil, false }
func (r *fixedNoModelRuntime) ReleaseAdmission()               { r.admissionReleases++ }
func (r *fixedNoModelRuntime) Release()                        { r.releases++ }

type fixedPins struct {
	tool  core.Tool
	calls int
}

func (p *fixedPins) Pin(names []string) (map[string]core.Tool, []core.ToolSpec) {
	p.calls++
	for _, name := range names {
		if name == p.tool.Name() {
			return map[string]core.Tool{name: p.tool}, []core.ToolSpec{{Name: name, Parameters: p.tool.InputSchema()}}
		}
	}
	return nil, nil
}

type fixedBinder struct {
	compiler                 *tool.SchemaCompiler
	schemaHash, originalHash string
	class                    string
}

func (b fixedBinder) Bind(ctx context.Context, env core.ToolEnv, call core.ToolCall) (core.FlowToolExecution, error) {
	input, err := core.CanonicalFlowJSON(ctx, json.RawMessage(call.Arguments))
	if err != nil {
		return core.FlowToolExecution{}, err
	}
	f := env.Flow
	return core.FlowToolExecution{Flow: f, Request: core.ActionRequest{TaskID: f.TaskID, RootTaskID: f.RootTaskID, NodeInvocationID: f.NodeInvocationID, LogicalSlot: "fixed-tool", ToolName: call.Name, AdapterVersion: "1", SchemaHash: b.schemaHash, Input: input, InputHash: probeHash(input), RecoveryClass: b.class, RunID: env.RunID, TaskRevision: f.TaskRevision, ExecutionEpoch: 1}, OriginalSchemaHash: b.originalHash, Validator: b.compiler}, nil
}

type fixedMemoryTrap struct{ calls int }

func (p *fixedMemoryTrap) LoadEffective(context.Context, core.Principal, string) (string, error) {
	p.calls++
	return "", errors.New("prompt memory must not run")
}

type fixedKnowledgeTrap struct{ calls int }

func (p *fixedKnowledgeTrap) RetrieveForRun(context.Context, core.KnowledgeRunRequest) (*core.KnowledgeInjection, error) {
	p.calls++
	return nil, errors.New("prompt retrieval must not run")
}

func (p *fixedKnowledgeTrap) RevalidateChunks(context.Context, core.KnowledgeRevalidateRequest) (*core.KnowledgeInjection, error) {
	p.calls++
	return nil, errors.New("prompt revalidation must not run")
}

func (p *fixedKnowledgeTrap) ResolveAnswer(context.Context, core.KnowledgeAnswerRequest) (core.KnowledgeAnswerReport, error) {
	p.calls++
	return core.KnowledgeAnswerReport{}, errors.New("prompt citation review must not run")
}

type fixedChatQuotaTrap struct{ calls int }

func (p *fixedChatQuotaTrap) AdmitCall(context.Context, core.CallQuotaRequest) (core.CallQuotaTicket, error) {
	p.calls++
	return nil, errors.New("Chat quota must not run")
}

func (*fixedChatQuotaTrap) CommitCall(context.Context, core.CallQuotaTicket, core.Usage) error {
	return errors.New("Chat settlement must not run")
}

func (*fixedChatQuotaTrap) ReleaseCall(context.Context, core.CallQuotaTicket, error) error {
	return errors.New("Chat release must not run")
}

func TestFlow_FixedReadOnlyRunHasNoModelPromptOrChatCharge(t *testing.T) {
	for _, scenario := range []string{"positive", "run-quota-denied", "writing-adapter"} {
		t.Run(scenario, func(t *testing.T) {
			h := newLoopHarness(t)
			probeTool := &execTool{name: "read_file", schema: json.RawMessage(execToolSchema), result: core.Result{Content: `{"answer":"observed"}`}}
			pins := &fixedPins{tool: probeTool}
			runtime := &fixedNoModelRuntime{}
			resolved := sampleResolvedRefs()
			resolved.Runtime = runtime
			resources := &fakeRunResources{resolved: resolved}
			snapshot := &fakeSnapshotBuilder{}
			quota := &recordingRunQuota{}
			// US-24 Q21：任务次数记账已并入 `RunStore.StartRun` 的事务，本用例的
			// 假仓储因此拿到同一端口来模拟事务内扣费（扣费失败 → 整笔不落、零占用）。
			h.store.runQuota = quota
			if scenario == "run-quota-denied" {
				quota.err = errRunQuotaDenied
			}
			memory := &fixedMemoryTrap{}
			knowledge := &fixedKnowledgeTrap{}
			chat := &fixedChatQuotaTrap{}
			compactions := 0
			loop := core.NewReActLoop(core.NewPromptBuilder(h.registry), core.NewToolExecutor(h.registry, h.audit, nil), nil).WithCallQuota(chat).WithCompaction(0, func(*core.RunEnv) core.ContextCompactor { compactions++; return nil })
			service := core.NewAgentService(core.ServiceOptions{Agents: h.agents, Client: h.provider, Messages: h.store, Runs: h.store, Snapshots: h.store, Audit: h.audit, Loop: loop, RunQuota: quota, ToolPins: pins, Lifecycle: newFakeLifecycle(core.AgentActive), Gate: core.NewAgentGate(), Resources: resources, RunSnapshots: snapshot, FlowRunsReady: true, Memory: memory, Knowledge: knowledge})
			uid := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A")
			manifest, err := core.BuildRunResourceManifest(uid, core.AgentActive, resolved)
			require.NoError(t, err)
			compiler := tool.NewSchemaCompiler()
			schemaHash, err := compiler.Compile(t.Context(), json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`))
			require.NoError(t, err)
			class := "readonly"
			if scenario == "writing-adapter" {
				class = "unverifiable_write"
			}
			ledger := &flowLedgerProbe{}
			budget := &flowBudgetProbe{}
			progress := &flowProgressProbe{}
			request := h.req("accepted fixed input")
			request.RunID = "fixed-read-probe"
			request.FixedTool = &core.ToolCall{ID: "fixed-call", Name: probeTool.Name(), Arguments: `{"path":"a.txt"}`}
			request.Flow = &core.FlowRunContext{TaskID: "task-fixed", RootTaskID: "task-fixed", NodeInvocationID: "fixed-node", NodeAttemptID: "fixed-attempt", AgentUID: string(uid), AgentConfigHash: "published-snapshot-hash", ResourceManifestHash: manifest.Hash, Authority: core.FlowAuthority{CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin, Scopes: []string{core.ScopeFlowsWrite}, AgentUIDs: []string{string(uid)}, Tools: []string{probeTool.Name()}}, Actions: ledger, Budget: budget, Progress: progress, Tools: fixedBinder{compiler: compiler, schemaHash: schemaHash, originalHash: probeHash(probeTool.InputSchema()), class: class}}
			result, err := service.Run(t.Context(), request)
			require.Zero(t, runtime.lookups, "the whole Run must skip missing model mappings")
			require.Zero(t, h.provider.callCount())
			require.Zero(t, memory.calls)
			require.Zero(t, knowledge.calls)
			require.Zero(t, chat.calls)
			require.Zero(t, compactions)
			require.Len(t, quota.calls, 1, "fixed Run must use the original task-charge port exactly once")
			require.Equal(t, 1, runtime.releases)
			if scenario == "run-quota-denied" {
				require.ErrorIs(t, err, errRunQuotaDenied)
				require.Zero(t, probeTool.executedCount())
				require.Zero(t, h.store.runsOf())
				require.Zero(t, h.store.countByRole(t, h.sessionID, core.RoleUser))
				return
			}
			require.NoError(t, err)
			require.Zero(t, result.Iterations)
			require.Equal(t, 1, h.store.runsOf())
			require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleUser))
			require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleAssistant))
			require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleTool))
			require.Empty(t, h.store.missingResults(t, h.sessionID))
			if scenario == "writing-adapter" {
				require.Equal(t, core.StatusFailed, result.Status)
				require.Zero(t, probeTool.executedCount())
				require.Zero(t, result.ToolCalls)
				return
			}
			require.Equal(t, core.StatusCompleted, result.Status)
			require.Equal(t, 1, result.ToolCalls)
			require.Equal(t, 1, probeTool.executedCount())
			require.Len(t, budget.settled, 1)
			require.Equal(t, core.UsageActual{Known: true, ToolCalls: 1}, budget.settled[0])
			require.Equal(t, 1, progress.after)
		})
	}
}
