//go:build integration

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/tool"
)

// 本文件是 US-24 跨 Worker 用例的共享夹具（模块卡 §4.4.5 的统一助手契约）。
// 它只做三件事：发布一个可执行的单节点 Flow 定义、给出一个**真实**的两判据 Runner
// （Joined / Isolated 走 postgres.SubtreeGate）、以及把 Run / 租约 / 未决动作这些
// 持久事实按生产同形写库。用例不重新实现判据，也不绕过执行权：需要推进的地方一律
// 经 `advanceUnderFlowRoot` 或 `flow.Worker`。
//
// This file is the shared fixture for the US-24 cross-worker cases. It publishes one
// executable single-node definition, supplies a runner whose Joined/Isolated are the real
// SubtreeGate predicates, and seeds runs / leases / unresolved actions in the same shape
// production writes them. No case re-implements a predicate or bypasses execution rights.

const (
	// distributedAgentUID 与定义里的节点一致；`distributedAuthority.Freeze` 也只放行它。
	distributedAgentUID = "ag_one"
	// distributedNodeID 是唯一种子节点；两个判据都按 root_task_id 作用在整棵子树上。
	distributedNodeID = "node-1"
	// distributedConfigHash / distributedManifestHash 是定义冻结的资产身份：
	// `validateFlowRecoveredOutput` 要求 Run 行与实际清单与之逐字相等。
	distributedConfigHash   = "us24-config-hash"
	distributedManifestHash = "us24-manifest-hash"
	// distributedOutput 是节点输出与 Run 终稿共用的唯一文本：复用路径要求
	// assistant 最后一条消息的规范 JSON 与提交的 OutputHash 完全一致。
	distributedOutput = `{"answer":"reused"}`
)

// distributedDefinition 构造一份真实可发布的单节点 Flow 定义。
// 发布本身不跑定义校验（FlowStore.Publish 只做内容哈希），但 Service.Start 会编译
// InputSchema、TightenBudget 会与 Config.Budget 取最小值，因此各字段必须就位。
func distributedDefinition() flow.Definition {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	cap := flow.BudgetCap{MaxTotalTokens: 100000, MaxToolCalls: 10, MaxDurationSeconds: 60, MaxDepth: 3, MaxChildren: 16, MaxAttemptsPerNode: 3, MaxRevisions: 2}
	return flow.Definition{
		FormatVersion: 1,
		Name:          "distributed-probe",
		Entry:         distributedNodeID,
		TaskContract:  "TASK.json",
		Limits:        cap,
		Nodes: []flow.Node{{
			ID:           distributedNodeID,
			Kind:         "agent",
			Agent:        &flow.AgentRef{UID: distributedAgentUID, Name: "writer", Version: 1, ConfigHash: distributedConfigHash, ManifestHash: distributedManifestHash},
			OutputSchema: schema,
			Failure:      flow.FailurePolicy{MaxAttempts: 1},
		}},
		Contract: flow.TaskContract{
			ContractVersion: 1,
			Goal:            "produce a structured answer",
			Constraints:     []string{"keep evidence"},
			InputSchema:     schema,
			OutputSchema:    schema,
			Budget:          cap,
			StopConditions:  []string{"budget", "deadline", "unknown"},
			Escalation:      "show unresolved",
			ArtifactSpec:    []flow.ArtifactSpec{{ID: "report", Format: "json", Required: true}},
			Acceptance:      []flow.AcceptanceRule{{ID: "shape", Version: 1, Kind: "deterministic", TargetKind: "artifact", TargetID: "report", Required: true, Schema: schema}},
		},
	}
}

// distributedAuthority 固定为"内置管理员 + flows:write"的冻结上限。
// 与生产的差别只在数据源：这里没有 Agent 索引，改由本结构直接给出可接受的 ceiling。
type distributedAuthority struct{}

func (distributedAuthority) Current(context.Context, flow.Task) (core.Principal, error) {
	return core.LocalAdminCLIPrincipal(), nil
}

func (distributedAuthority) CheckStart(context.Context, core.Principal, flow.DefinitionVersion) error {
	return nil
}

func (distributedAuthority) NoAvailableCallerKey(context.Context, string) (bool, error) {
	return false, nil
}

func (distributedAuthority) Freeze(_ context.Context, p core.Principal, _ flow.DefinitionVersion) (core.FlowAuthority, error) {
	return core.FlowAuthority{
		CallerID:        p.CallerID,
		KeyID:           p.KeyID,
		AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		Scopes:          []string{core.ScopeFlowsWrite},
		AgentUIDs:       []string{distributedAgentUID},
		Tools:           []string{"flow_probe"},
	}, nil
}

// distributedNodeRunner 只替换叶子执行本身（`Run` / `ObserveCompleted`），
// `Joined` / `Isolated` 仍由内嵌的真实 `flowNodeRunner` 走三个持久判据。
// 这样"两判据各自独立取值"的断言考的是生产代码，不是夹具。
type distributedNodeRunner struct {
	*flowNodeRunner
	calls  atomic.Int64
	output json.RawMessage
}

var _ flow.NodeRunner = (*distributedNodeRunner)(nil)

func newDistributedRunner(db *postgres.DB) *distributedNodeRunner {
	return &distributedNodeRunner{
		flowNodeRunner: &flowNodeRunner{
			store:           postgres.NewFlowStore(db),
			gate:            postgres.NewSubtreeGate(db),
			startupComplete: true,
			active:          map[string]int{},
		},
		output: json.RawMessage(distributedOutput),
	}
}

func (r *distributedNodeRunner) Run(context.Context, flow.NodeRunRequest) (flow.NodeRunResult, error) {
	r.calls.Add(1)
	return r.result(), nil
}

// ObserveCompleted 是复用路径的观察口：它不派发新 Run，只回报那条已完成运行的结论。
func (r *distributedNodeRunner) ObserveCompleted(context.Context, flow.NodeRunRequest) (flow.NodeRunResult, error) {
	return r.result(), nil
}

func (r *distributedNodeRunner) result() flow.NodeRunResult {
	return flow.NodeRunResult{
		Run:    core.RunResult{Status: core.StatusCompleted, FinalText: string(r.output), Iterations: 1},
		Output: r.output,
	}
}

// publishDistributedDefinition 以一次真实的受控发布写入定义（含管理操作行）。
func publishDistributedDefinition(t *testing.T, db *postgres.DB, uid string) flow.DefinitionVersion {
	t.Helper()
	return publishDistributedDefinitionWith(t, db, uid, distributedDefinition())
}

// publishDistributedDefinitionWith 允许用例改一处策略（例如把节点失败上限抬到 2）
// 后再发布：重跑路径要走到第二次 attempt，而生产 Claim 用
// `min(Failure.MaxAttempts, Contract.Budget.MaxAttemptsPerNode)` 判上限。
func publishDistributedDefinitionWith(t *testing.T, db *postgres.DB, uid string, definition flow.Definition) flow.DefinitionVersion {
	t.Helper()
	store := postgres.NewFlowStore(db)
	// `idx_flow_pointer_name` 对 `lower(name)` 唯一：一个库内发布两份定义必须用不同的
	// 展示名，否则第二次发布会撞指针表而不是定义本身。用 uid 作名，天然一一对应。
	definition.Name = uid
	const version int64 = 1
	op := "publish-" + uid
	require.NoError(t, postgres.NewManagementStore(db).BeginOperation(t.Context(), core.OperationRecord{
		OperationID:        op,
		Action:             "flow.publish",
		TargetKind:         "flow_definition",
		TargetID:           uid,
		ActorSubject:       "admin",
		AuthMethod:         core.AuthMethodCLI,
		Source:             "cli",
		RequestedVersionID: version,
		CreatedAt:          time.Now(),
	}))
	published, err := store.Publish(t.Context(), flow.PublishRequest{
		Principal:       core.LocalAdminCLIPrincipal(),
		Actor:           core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"},
		DefinitionUID:   uid,
		ExpectedVersion: 0,
		OperationID:     op,
		Definition:      definition,
	})
	require.NoError(t, err)
	require.EqualValues(t, version, published.Version)
	return published
}

// distributeLogger 让夹具日志不进测试输出：断言只看持久事实与调用记录。
func distributeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newDistributedService 装配真实的 Service：仓储、定义与租约都走 PostgreSQL，
// 只有叶子执行与 authority 是夹具。Config.Enabled 必须显式打开，否则各入口一律
// 返回 ErrStateConflict。
func newDistributedService(db *postgres.DB, runner flow.NodeRunner) *flow.Service {
	store := postgres.NewFlowStore(db)
	config := flow.DefaultConfig()
	config.Enabled = true
	config.PollIntervalMS = 100
	return &flow.Service{
		Repository:  store,
		Definitions: store,
		Schemas:     tool.NewSchemaCompiler(),
		Authority:   distributedAuthority{},
		Runner:      runner,
		Config:      config,
		Logger:      distributeLogger(),
		Identity:    core.WorkerIdentity{InstanceID: "flow-service-fixture", Name: "flow-service-fixture"},
		Leases:      postgres.NewLeaseStore(db),
		LeaseTTL:    30 * time.Second,
	}
}

// startDistributedTask 走生产 Start 路径（冻结 Scope / 预算 / 截止时间），
// 返回 Queued 根任务。直接调 FlowStore.Create 会漏掉 Contract.Scope 的冻结。
func startDistributedTask(t *testing.T, service *flow.Service, definition flow.DefinitionVersion, key string) flow.Task {
	t.Helper()
	task, err := service.Start(t.Context(), flow.StartRequest{
		Principal:      core.LocalAdminCLIPrincipal(),
		DefinitionUID:  definition.DefinitionUID,
		IdempotencyKey: key,
		Input:          json.RawMessage(`{"answer":"input"}`),
	})
	require.NoError(t, err)
	require.Equal(t, flow.Queued, task.Status)
	return task
}

// claimDistributedAttempt 以**真实执行者身份**领取一次节点（推进类写入要求活根租约）：
// 领租约 → Claim → 重读任务 → 放租约。返回的 attempt 已带 run_id / session_id，
// 但 Run 行由调用方按需 seed——标准执行器会在同一事务创建它，夹具显式写。
//
// TaskRevision 必须显式带上：`Claim` 校验 `a.TaskRevision == task.Version.Revision`
// 并用它做 `flow_task_revisions` 外键。省掉这一项只会在 revision 0 时碰巧成立，
// 一旦发生修订就会变成外键错而不是断言失败。
func claimDistributedAttempt(t *testing.T, db *postgres.DB, store *postgres.FlowStore,
	task flow.Task, attemptID, runID, sessionID string, number int,
) (flow.NodeAttempt, flow.Task) {
	t.Helper()
	token := acquireFlowRootForTest(t, db, task.RootTaskID)
	attempt, err := store.Claim(t.Context(), flow.ClaimNode{
		Principal: core.LocalAdminCLIPrincipal(),
		TaskID:    task.ID,
		Expected:  task.Version,
		Lease:     token,
		Now:       time.Now().UnixMilli(),
		Attempt: flow.NodeAttempt{
			ID:               attemptID,
			TaskID:           task.ID,
			NodeID:           distributedNodeID,
			NodeInvocationID: task.ID + ":" + distributedNodeID,
			Attempt:          number,
			TaskRevision:     task.Version.Revision,
			ExecutionEpoch:   task.Version.ExecutionEpoch,
			RunID:            runID,
			SessionID:        sessionID,
			Input:            task.Revision.Input,
			// InputHash 直接取任务头记录的值：`ResolveNodeInput` 在无映射时要求
			// `flowInputHash(CanonicalJSON(revision.Input)) == revision.InputHash`，
			// 复算原始字节只会在输入本来非规整时才碰巧不等，反而把夹具与生产判据分叉。
			InputHash: task.Revision.InputHash,
		},
	})
	require.NoError(t, err)
	require.Equal(t, attemptID, attempt.ID)
	releaseFlowRootForTest(t, db, token)
	current, err := store.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, flow.Running, current.Status)
	return attempt, current
}

// distributedHashOf 复算 `flow_node_attempts.input_hash` 的取值：
// 存储层用 `flowDataHash`（sha256 over 原始字节 + hex），包内未导出，
// 断言侧按同一算法取值，不用硬编码。
func distributedHashOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// seedDistributedRun 写一条与 flow 节点关联的 Run 行及配套事实。
//
// 为什么手写 SQL 而不是 RunStore.StartRun：本用例集的叶子执行由夹具接管，不走
// 真实 AgentService；这里要固定的是**读侧**证据（runs + manifest + assistant 消息），
// 让 `validateFlowRecoveredOutput` 与 `CompletedAttempt` 的联表条件逐条成立。
func seedDistributedRun(t *testing.T, db *postgres.DB, task flow.Task, attempt flow.NodeAttempt, status string) {
	t.Helper()
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(t.Context(), `INSERT INTO sessions(session_id,profile_name,channel,user_id,status,message_count,created_at,last_active_at,caller_id)
VALUES($1,'writer','cli',$2,'active',1,$3,$3,$4)`,
		attempt.SessionID, "flow:"+task.CallerID, now, task.CallerID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO runs(run_id,session_id,agent,channel,user_id,trigger,status,reason,iterations,tool_calls,config_hash,started_at,finished_at,caller_id,authorizing_kind,key_id,actor,flow_task_id,flow_node_attempt_id)
VALUES($1,$2,'writer','cli',$3,'user',$4,'',$5,0,$6,$7,$7,$8,'admin-builtin',NULL,'caller',$9,$10)`,
		attempt.RunID, attempt.SessionID, "flow:"+task.CallerID, status, 1, distributedConfigHash, now, task.CallerID, task.ID, attempt.ID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO run_resource_manifests(run_id,format_version,manifest_hash,agent_lifecycle_state,snapshot_json,created_at)
VALUES($1,1,$2,'enabled','{}',$3)`, attempt.RunID, distributedManifestHash, now)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO messages(session_id,run_id,seq,role,content,created_at)
VALUES($1,$2,1,'assistant',$3,$4)`, attempt.SessionID, attempt.RunID, distributedOutput, now)
	require.NoError(t, err)
}

// seedUnresolvedAction 写一条未决动作（门 ③）：outcome ∈ dispatching / unknown 即
// 表示"这条外部动作可能已经发出"，推进方不得在其之上再派发一次。
func seedUnresolvedAction(t *testing.T, db *postgres.DB, task flow.Task, attempt flow.NodeAttempt, actionID, outcome string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `INSERT INTO flow_actions(task_id,action_id,node_invocation_id,logical_slot,tool_name,adapter_version,schema_hash,input,input_hash,recovery_class,outcome,created_at)
VALUES($1,$2,$3,'slot-1','flow_probe','v1','schema-hash','{}',$4,'unverifiable_write',$5,$6)`,
		task.ID, actionID, attempt.NodeInvocationID, distributedHashOf([]byte(`{}`)), outcome, time.Now().UnixMilli())
	require.NoError(t, err)
}

// setDistributedActionOutcome 改一条动作的 outcome。`flow_actions` 上有
// `UNIQUE(task_id, node_invocation_id, logical_slot)`，同一槽位不能插入第二条，
// 因此"同一动作从 prepared 走到未决"只能原地改值。
func setDistributedActionOutcome(t *testing.T, db *postgres.DB, actionID, outcome string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `UPDATE flow_actions SET outcome=$2 WHERE action_id=$1`, actionID, outcome)
	require.NoError(t, err)
}

// setDistributedAttemptState 把一条 attempt 改成指定状态（含 finished_at）。
// 夹具用它制造"上一轮已收敛 / 已失败"的持久事实，不经过任何业务写入路径。
func setDistributedAttemptState(t *testing.T, db *postgres.DB, attemptID, state string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `UPDATE flow_node_attempts SET state=$2,finished_at=$3 WHERE node_attempt_id=$1`,
		attemptID, state, time.Now().UnixMilli())
	require.NoError(t, err)
}

// acquireDistributedLease 以给定身份主动领取任意类型的租约（夹具造"别人持有"的现场）。
func acquireDistributedLease(t *testing.T, db *postgres.DB, kind core.LeaseKind, resourceID string,
	owner core.WorkerIdentity, ttl time.Duration,
) core.LeaseToken {
	t.Helper()
	lease, err := postgres.NewLeaseStore(db).Acquire(t.Context(), kind, resourceID, owner, ttl)
	require.NoError(t, err)
	return lease.LeaseToken
}

// expireDistributedLease 把任意类型的租约改成已过期：模拟属主崩溃后 TTL 到期。
// 它只动租约行、不改业务行——判据必须只认租约（FR-24-20 / FR-24-38）。
func expireDistributedLease(t *testing.T, db *postgres.DB, kind core.LeaseKind, resourceID string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `UPDATE execution_leases
   SET expires_at = (extract(epoch from clock_timestamp()) * 1000)::bigint - 1000
 WHERE kind = $1 AND resource_id = $2 AND released_at IS NULL`, string(kind), resourceID)
	require.NoError(t, err)
}

// countDistributedAttempts 数"带 Run 引用的节点尝试"：复用路径不新增 attempt，
// 重跑路径新增一条——1 与 2 的差别正是"复用 vs 重跑"。
func countDistributedAttempts(t *testing.T, db *postgres.DB, taskID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_node_attempts WHERE task_id=$1 AND run_id IS NOT NULL`, taskID).Scan(&count))
	return count
}

// countDistributedNodeRuns 数根子树下真实的 Run 行：复用路径**不得**多出一行。
func countDistributedNodeRuns(t *testing.T, db *postgres.DB, rootTaskID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs r JOIN flow_tasks t ON t.task_id=r.flow_task_id WHERE t.root_task_id=$1`, rootTaskID).Scan(&count))
	return count
}

// distributedRowCounts 是"零新行"断言要用的五行快照：门 ③ 命中时普通 Worker 既不得
// 派发新 Run、也不得写任何审计 / 动作 / 调用行。
// 每张表一条固定 SQL，不按表名拼串——拼串既绕开静态检查也容易被误用成注入面。
type distributedRowCounts struct {
	Runs            int
	Attempts        int
	Actions         int
	LLMCalls        int
	ToolInvocations int
}

func countDistributedRows(t *testing.T, db *postgres.DB, rootTaskID string) distributedRowCounts {
	t.Helper()
	var counts distributedRowCounts
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs r JOIN flow_tasks t ON t.task_id=r.flow_task_id WHERE t.root_task_id=$1`, rootTaskID).Scan(&counts.Runs))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_node_attempts a JOIN flow_tasks t ON t.task_id=a.task_id WHERE t.root_task_id=$1`, rootTaskID).Scan(&counts.Attempts))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_actions a JOIN flow_tasks t ON t.task_id=a.task_id WHERE t.root_task_id=$1`, rootTaskID).Scan(&counts.Actions))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM llm_calls c JOIN runs r ON r.run_id=c.run_id JOIN flow_tasks t ON t.task_id=r.flow_task_id WHERE t.root_task_id=$1`, rootTaskID).Scan(&counts.LLMCalls))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM tool_invocations i JOIN runs r ON r.run_id=i.run_id JOIN flow_tasks t ON t.task_id=r.flow_task_id WHERE t.root_task_id=$1`, rootTaskID).Scan(&counts.ToolInvocations))
	return counts
}

// distributedLeaseRow 是 execution_leases 的只读投影：断言只看这些持久事实。
type distributedLeaseRow struct {
	Owner    string
	Epoch    int64
	Released bool
}

func readDistributedLease(t *testing.T, db *postgres.DB, kind core.LeaseKind, resourceID string) distributedLeaseRow {
	t.Helper()
	var row distributedLeaseRow
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT owner_worker_id, ownership_epoch, released_at IS NOT NULL FROM execution_leases WHERE kind=$1 AND resource_id=$2`, string(kind), resourceID).
		Scan(&row.Owner, &row.Epoch, &row.Released))
	return row
}

// distributedRepository 固定 Worker 的领取面：ReadyRoots 委托真实 SQL 就绪判定后
// 只保留本用例的根，让 `flow.Worker` 在测试里可确定地被驱动；同时把 Recover / Claim /
// HasUnknownActions 的先后与次数记下来（FR-24-37：接管必须先 Recover 再 Advance）。
//
// ReadyRoots **不得**自写一份同义谓词：就绪条件（blocked 只有
// reason='bounded_rework_required' 才就绪）正是本 US 的判据之一，夹具重写它就会把
// 生产谓词的回归挡在用例之外。委托 + 按 rootID 过滤才是唯一安全的收窄方式。
type distributedRepository struct {
	*postgres.FlowStore
	rootID    string
	mu        sync.Mutex
	order     []string
	recoverNo int
	unknownNo int
	failFirst error
}

func newDistributedRepository(store *postgres.FlowStore, rootID string, failFirst error) *distributedRepository {
	return &distributedRepository{FlowStore: store, rootID: rootID, failFirst: failFirst}
}

func (r *distributedRepository) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func (r *distributedRepository) recoverCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recoverNo
}

// unknownCalls 数 `HasUnknownActions` 的调用次数：`Service.Recover` 在调用
// `Repository.Recover` **之前**就可能以 ErrActionUnknown 提前返回，因此"Recover 被
// 调用过"并不能证明查证做过，必须单独观察这一端口。
func (r *distributedRepository) unknownCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unknownNo
}

func (r *distributedRepository) ReadyRoots(ctx context.Context, now int64, limit int) ([]flow.Task, error) {
	roots, err := r.FlowStore.ReadyRoots(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	var result []flow.Task
	for _, task := range roots {
		if task.RootTaskID == r.rootID {
			result = append(result, task)
		}
	}
	return result, nil
}

func (r *distributedRepository) HasUnknownActions(ctx context.Context, taskID string) (bool, error) {
	r.mu.Lock()
	r.unknownNo++
	r.mu.Unlock()
	return r.FlowStore.HasUnknownActions(ctx, taskID)
}

func (r *distributedRepository) Recover(ctx context.Context, req flow.ReconcileTask) (flow.Task, error) {
	r.mu.Lock()
	r.order = append(r.order, "recover")
	r.recoverNo++
	fail := r.recoverNo == 1 && r.failFirst != nil
	r.mu.Unlock()
	if fail {
		return flow.Task{}, r.failFirst
	}
	return r.FlowStore.Recover(ctx, req)
}

func (r *distributedRepository) Claim(ctx context.Context, req flow.ClaimNode) (flow.NodeAttempt, error) {
	r.mu.Lock()
	r.order = append(r.order, "claim")
	r.mu.Unlock()
	return r.FlowStore.Claim(ctx, req)
}

// startDistributedWorker 以给定身份在后台驱动 `flow.Worker.Run`；返回停止函数。
// 停止时等待 Run 真正返回，避免下一个断言与在途推进交错。
func startDistributedWorker(t *testing.T, service *flow.Service, repo flow.WorkerRepository,
	db *postgres.DB, identity core.WorkerIdentity, ttl, renew time.Duration,
) func() {
	t.Helper()
	worker := &flow.Worker{
		Service:       service,
		Repository:    repo,
		Identity:      identity,
		Leases:        postgres.NewLeaseStore(db),
		Gate:          postgres.NewSubtreeGate(db),
		LeaseTTL:      ttl,
		RenewInterval: renew,
		Startup:       func(context.Context) error { return nil },
		Logger:        distributeLogger(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("flow worker did not stop within the test budget")
		}
	}
}
