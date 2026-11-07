package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/tool"
)

// flowNodeRunner is the one production leaf adapter: it only enters the shared
// runAdmission and AgentService, never appending messages or invoking a tool.
type flowNodeRunner struct {
	service   *core.AgentService
	admission *runAdmission
	store     *postgres.FlowStore
	authority flow.AuthorityResolver
	sources   flow.SourceValidity
	// gate 是根子树的三个持久判据（US-24 §3.1.8 / core.SubtreeGate）。Joined 与
	// Isolated 各答各的问题，**不得**互换：前者问"还有没有子节点可能提交"，
	// 后者问"还有没有人能提交"。nil（未装配）一律 fail-closed。
	// gate carries the three durable subtree criteria; Joined and Isolated never
	// substitute for each other, and a nil gate fails closed.
	gate            core.SubtreeGate
	collections     knowledge.Authz
	schemas         *tool.SchemaCompiler
	writeReady      bool
	mu              sync.Mutex
	startupComplete bool
	active          map[string]int
}

var _ flow.NodeRunner = (*flowNodeRunner)(nil)

func (r *flowNodeRunner) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	if r == nil || r.service == nil || r.admission == nil || r.store == nil || r.authority == nil || r.schemas == nil || req.Flow == nil || req.Node.Agent == nil {
		return flow.NodeRunResult{}, core.ErrFlowDispatch
	}
	r.mu.Lock()
	if !r.startupComplete {
		r.mu.Unlock()
		return flow.NodeRunResult{}, core.ErrFlowDispatch
	}
	if r.active == nil {
		r.active = make(map[string]int)
	}
	r.active[req.Task.RootTaskID]++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active[req.Task.RootTaskID]--
		if r.active[req.Task.RootTaskID] == 0 {
			delete(r.active, req.Task.RootTaskID)
		}
		r.mu.Unlock()
	}()
	p, err := r.authority.Current(ctx, req.Task)
	if err != nil {
		return flow.NodeRunResult{}, err
	}
	if p.CallerID != req.Principal.CallerID || p.KeyID != req.Principal.KeyID || req.Attempt.RunID == "" || req.Attempt.SessionID == "" || req.Flow.TaskID != req.Task.ID || req.Flow.NodeAttemptID != req.Attempt.ID {
		return flow.NodeRunResult{}, core.ErrFlowDispatch
	}
	input, err := flow.CanonicalJSON(ctx, req.Attempt.Input)
	if err != nil {
		return flow.NodeRunResult{}, err
	}
	inputSum := sha256.Sum256(input)
	if hex.EncodeToString(inputSum[:]) != req.Attempt.InputHash {
		return flow.NodeRunResult{}, core.ErrFlowDispatch
	}
	graph, err := r.store.ReadGraph(ctx, p, req.Task.ID)
	if err != nil {
		return flow.NodeRunResult{}, err
	}
	var refs []flow.EvidenceRef
	for _, mapping := range req.Node.Inputs {
		parts := strings.Split(mapping.Source, ".")
		if len(parts) > 2 && parts[0] == "outputs" {
			output, found := req.Task.Revision.Outputs[parts[1]]
			if !found {
				return flow.NodeRunResult{}, flow.ErrStateConflict
			}
			selected, err := r.store.OutputEvidence(ctx, p, req.Task.ID, output)
			if err != nil {
				return flow.NodeRunResult{}, err
			}
			refs = append(refs, selected...)
		}
	}
	refs = append(refs, req.Task.Contract.InputRefs...)
	if err := checkFlowSources(ctx, p, req.Task, refs, r.store, r.sources); err != nil {
		return flow.NodeRunResult{}, err
	}
	summary, err := flow.BuildSummary(req.Task, graph)
	if err != nil {
		return flow.NodeRunResult{}, err
	}
	f := req.Flow
	f.Authority = req.Task.Contract.Scope
	f.Authority.Scopes = slices.Clone(f.Authority.Scopes)
	f.Authority.Tools = slices.Clone(f.Authority.Tools)
	f.State = &core.StructuredState{Goal: req.Task.Contract.Goal}
	for _, constraint := range req.Task.Contract.Constraints {
		f.State.Constraints = append(f.State.Constraints, core.ConstraintRef{Text: constraint})
	}
	for _, id := range summary.Unresolved {
		f.State.OpenItems = append(f.State.OpenItems, core.OpenItem{Text: id})
	}
	for _, ref := range refs {
		f.InputEvidenceRefs = append(f.InputEvidenceRefs, core.EvidenceRef{Kind: ref.SourceKind, ID: ref.SourceUID, VersionID: ref.SourceVersion, ContentHash: ref.ContentHash})
	}
	recheck := func(ctx context.Context) error {
		current, err := r.authority.Current(ctx, req.Task)
		if err != nil {
			return err
		}
		if current.CallerID != p.CallerID || current.KeyID != p.KeyID {
			return core.ErrUnauthorized
		}
		if err := r.store.CheckActiveBoundary(ctx, current, req.Task.ID, req.Attempt.ID, req.Attempt.RunID, req.Task.Version); err != nil {
			return err
		}
		return checkFlowSources(ctx, current, req.Task, refs, r.store, r.sources)
	}
	f.Actions = &postgres.FlowActions{Store: r.store, Principal: p, TaskID: req.Task.ID, NodeAttemptID: req.Attempt.ID, Recheck: recheck}
	f.Budget = &postgres.FlowBudget{Store: r.store, Principal: p, TaskID: req.Task.ID}
	f.Retrieval = &flowRetrievalGuard{check: recheck, current: func(ctx context.Context) (core.Principal, error) { return r.authority.Current(ctx, req.Task) }, authz: r.collections, principal: p, agent: req.Node.Agent.Name, collections: slices.Clone(f.Authority.CollectionIDs)}
	f.Retrieval.(*flowRetrievalGuard).permit = func(ctx context.Context, auditID string, budget core.BudgetTicket) error {
		return r.store.AuthorizeQueryEmbedding(ctx, p, req.Task.ID, req.Attempt.ID, req.Attempt.RunID, auditID, req.Task.Version, budget)
	}
	binder := &flowToolBinder{flow: f, task: req.Task, schemas: r.schemas, fixed: req.Node.Kind == "tool", writeReady: r.writeReady}
	f.Tools = binder
	f.Progress = &postgres.FlowProgress{Store: r.store, Principal: p, TaskID: req.Task.ID, NodeAttemptID: req.Attempt.ID, Proof: func(ctx context.Context, call core.ToolCall, result core.Result, out core.ActionOutcome) (string, error) {
		if err := recheck(ctx); err != nil {
			return "", err
		}
		return binder.progressBasis(ctx, call, result, out)
	}}
	f.Models = &postgres.FlowModels{Store: r.store, Principal: p, TaskID: req.Task.ID, NodeAttemptID: req.Attempt.ID, Estimate: func(input int) int64 { return chatReserveTokens(core.CallQuotaRequest{EstimatedInput: input}) }, Recheck: func(ctx context.Context, model core.ChatRequest) error {
		if err := recheck(ctx); err != nil {
			return err
		}
		var modelRefs []flow.EvidenceRef
		for _, ref := range model.FlowEvidenceRefs {
			modelRefs = append(modelRefs, flow.EvidenceRef{TaskID: req.Task.ID, SourceKind: ref.Kind, SourceUID: ref.ID, SourceVersion: ref.VersionID, ContentHash: ref.ContentHash})
		}
		current, err := r.authority.Current(ctx, req.Task)
		if err != nil {
			return err
		}
		return checkFlowSources(ctx, current, req.Task, modelRefs, r.store, r.sources)
	}}
	message, err := json.Marshal(struct {
		Input        json.RawMessage  `json:"input"`
		OutputSchema json.RawMessage  `json:"output_schema"`
		Summary      flow.TaskSummary `json:"task_summary"`
	}{input, req.Node.OutputSchema, summary})
	if err != nil || len(message) > 1<<20 {
		return flow.NodeRunResult{}, flow.ErrBudget
	}
	run := core.RunRequest{Principal: p, RunID: req.Attempt.RunID, Agent: req.Node.Agent.Name, SessionID: req.Attempt.SessionID, Message: string(message), Channel: "cli", UserID: "flow:" + p.CallerID, Flow: f, Budget: core.Budget{ToolTimeout: min(30*time.Second, time.Until(time.UnixMilli(req.Task.Deadline)))}}
	if req.Node.Kind == "tool" {
		run.FixedTool = &core.ToolCall{ID: "fixed-" + req.Attempt.ID, Name: req.Node.Tool, Arguments: string(input)}
	}
	result, runErr := r.admission.run(ctx, run, r.service.Run)
	var full *core.InstanceFullError
	if errors.Is(runErr, callers.ErrConcurrencyLimited) || errors.As(runErr, &full) {
		return flow.NodeRunResult{}, flow.ErrCapacity
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	observed, readErr := r.store.ObserveNode(finish, p, req.Task.ID, req.Attempt.ID)
	if readErr != nil {
		return flow.NodeRunResult{Run: result, Unknown: !errors.Is(readErr, core.ErrRunNotFound)}, errors.Join(runErr, readErr)
	}
	if observed.Run.Status != result.Status {
		observed.Unknown = true
	}
	observed.Run = result
	if req.Node.Kind == "agent" {
		observed.Output = json.RawMessage(result.FinalText)
		observed.Retryable = true
	}
	return observed, runErr
}

// Joined = 根子树下不存在 prepared / running attempt（US-24 §3.1.8 / FR-24-38）。
//
// 本地活跃表只作**快路径**（命中即 false，本进程还有节点在跑，不查库）；未命中时
// 必须查库，**不再**用租约有效性代替——租约有效性属于另一个判据 Isolated。
// interrupted / unknown 不计入 Joined：它们只能由 Recover 清理，计入就永远等不到
// 放行；租约是否过期也不参与本判据（TTL 不能推断子 Run 已退出）。
func (r *flowNodeRunner) Joined(ctx context.Context, task flow.Task) (bool, error) {
	if r == nil || r.gate == nil {
		return false, core.ErrFlowDispatch
	}
	r.mu.Lock()
	startupComplete, active := r.startupComplete, r.active[task.RootTaskID] > 0
	r.mu.Unlock()
	// Startup is set only after original recovery under the process instance lock.
	// Every current-process leaf is registered before admission and removed after
	// the synchronous original Run has actually returned, including audit cleanup.
	if !startupComplete || active {
		return false, nil
	}
	inflight, err := r.inFlightFor(ctx, task)
	if err != nil {
		return false, err
	}
	return !inflight, nil
}

// inFlightFor 按**推进目标**的作用域判"子树是否在途"：
//   - 根任务：整棵根子树（`core.SubtreeGate`，FR-24-20 / FR-24-38 的既有口径，
//     与 Worker 的门 ② 同一份查询）；
//   - 子任务：目标任务自身子树（`HasInFlightNodeAttemptUnder`）。祖先链上的 attempt
//     是**调用方自己**在同一轮 `advanceSubflow` 里刚 Claim 的父 / 祖父 subflow 节点
//     凭证——subflow 节点没有 Run，attempt 就是它的执行凭证——把它计入等于让子复用
//     自我阻塞：Joined=false → ErrCapacity → Worker 当竞争错误吞掉 → 每轮重复。
//     后代的 prepared / running attempt 仍然计入，interrupted / unknown 仍不计入。
//
// 两处都只认持久事实（TTL 不参与推断）；本进程活跃表只是上面的快路径。
// inFlightFor scopes the Joined criterion to the task being advanced: the whole root
// subtree for a root, the task's own subtree for a child, so the parent subflow attempt
// claimed by this very advance cannot block reuse of a completed child Run.
func (r *flowNodeRunner) inFlightFor(ctx context.Context, task flow.Task) (bool, error) {
	if task.ParentTaskID == "" {
		return r.gate.HasInFlightNodeAttempt(ctx, task.RootTaskID)
	}
	if r.store == nil {
		return false, core.ErrFlowDispatch
	}
	return r.store.HasInFlightNodeAttemptUnder(ctx, task.ID)
}

// Isolated = 根子树下没有仍有效的 run 租约；**只**用于驱动 Recover /
// 关闭路径把 prepared|running attempt 收敛为 interrupted，不参与推进门禁。
// 判据是租约有效性而非进程存活性：受保护写入全走 FencedTx，租约失效后旧属主的
// 提交被拒——这是 fencing 的语义，不是猜测。
// Isolated gates attempt convergence only; it never gates advancement.
func (r *flowNodeRunner) Isolated(ctx context.Context, task flow.Task) (bool, error) {
	if r == nil || r.gate == nil {
		return false, core.ErrFlowDispatch
	}
	live, err := r.gate.HasLiveRunLease(ctx, task.RootTaskID)
	if err != nil {
		return false, err
	}
	return !live, nil
}

func (r *flowNodeRunner) ObserveCompleted(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	p, err := r.authority.Current(ctx, req.Task)
	if err != nil {
		return flow.NodeRunResult{}, err
	}
	if p.CallerID != req.Principal.CallerID || p.KeyID != req.Principal.KeyID || req.Node.Agent == nil {
		return flow.NodeRunResult{}, core.ErrUnauthorized
	}
	result, err := r.store.ObserveNode(ctx, p, req.Task.ID, req.Attempt.ID)
	if err != nil {
		return result, err
	}
	if result.Unknown || result.Run.Status != core.StatusCompleted || result.Run.ConfigHash != req.Node.Agent.ConfigHash || result.Run.ManifestHash != req.Node.Agent.ManifestHash {
		return result, flow.ErrStateConflict
	}
	if err := checkFlowSources(ctx, p, req.Task, result.Evidence, r.store, r.sources); err != nil {
		return result, err
	}
	if req.Node.Kind == "agent" {
		result.Output = json.RawMessage(result.Run.FinalText)
	}
	return result, nil
}
