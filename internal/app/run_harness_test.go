//go:build integration

package app

// 本文件是 R18（入口速率 / 并发名额 / 配额预留的生命周期）与 C-05 / C-29 服务级用例的
// 共用夹具（颗粒度文档 §4.4 图 6 [1]、[3]～[6]、C-35）。判据的主角是**生产路径**：
// `web.RunService` → `runAdmission.run` → `core.AgentService.Run` → 自实现 ReAct 循环，
// 名额与配额的取还全部由这些生产代码完成——**测试不代为 ReleaseRun**，否则
// "生产路径漏释放"这条缺陷再也测不出来（C-35 的原始理由）。
// This file is the shared fixture for the R18 lifecycle cases. The production path does
// every acquire and release; a test that released the slot itself would make the very
// defect these cases exist for invisible.
//
// 真实件（不替换）：
//   - PostgreSQL（隔离 schema）+ 真实 `postgres` 仓储：RunStore / QuotaStore /
//     SessionStore / SnapshotStore / LLMCallRepo / ToolInvocationRepo；
//   - 真实 `callers.Limiter`（入口桶 + 名额表 + 配额端口同一个实例，C-35 单实例裁决）、
//     真实 `runAdmission`、真实 `core.ReActLoop`（含 `WithCallQuota`）、真实
//     `provider.ProviderService`（路由 + 两阶段审计）；
//   - 真实 AGENT.md 落盘（快照每次运行重读，模型与工具集来自 frontmatter）。
//
// 替身（仅这三样，且都带明示理由）：
//   - `scriptedProvider`：模型协议客户端，按脚本产出文本 / 工具调用 / 超时 / 阻塞；
//   - `armedTool`：Agent 声明的唯一工具，默认成功、可武装成"执行期 panic"；
//   - `runHarnessSessions`：会话状态端口，用 `MarkBusy` 复现 [4] 会话串行拒绝
//     （真实实现的忙集在 `AgentService` 内部，测试无法从外部置位）。
// The three stand-ins are the protocol client, the single declared tool and the session
// status port; everything else is the production object graph.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
	providersvc "github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
)

const (
	// runHarnessAgentName 是夹具 Agent 的目录名（= frontmatter 的 name）。
	runHarnessAgentName = "admission-fixture"
	// runHarnessProvider / runHarnessModel 同时出现在 AGENT.md 与显式映射表里：
	// 路由只查表（宪法 III），两边不一致会让每次调用在 [6] 之前就被拒绝。
	runHarnessProvider = "prov-a"
	runHarnessModel    = "model-a"
	// runHarnessToolName 是 Agent 声明的唯一工具（`tools: [boom]`）。
	runHarnessToolName = "boom"
	// runHarnessCaller 是默认调用方；`runs.caller_id` 对 `callers` 有外键，
	// 限额外的一条 `callers` 行由 `SetLimit` 顺带补齐。
	runHarnessCaller = "c1"

	// runHarnessReserveTokens 是夹具固定的单次对话预留量：卡片的
	// `TestRunService_UnknownUsageReleasesRunSlot` 断言"用量未知的预留继续占额度 = 100"，
	// 因此这里走 `chatQuota.tokens` 注入位而不是生产公式（估算输入 + 4096）。
	// The fixture pins the per-call reservation so the card's reserved==100 assertion
	// holds; production keeps the estimated-input formula.
	runHarnessReserveTokens = 100

	// runHarnessDefaultTokens 是未显式 `setLimit` 时的周期额度：`Reserve` 对"限额缺失
	// 或为零"一律 `ErrQuotaExhausted`，没有这一档，取消 / panic / 入口速率三个用例
	// 会先在 [5] 被挡下，测不到它们真正要测的退出路径。
	// A generous default keeps the non-quota cases from being stopped at stage [5].
	runHarnessDefaultTokens int64 = 1_000_000

	// runHarnessBurst 是未显式 `setIngressQPS` 时的桶容量：足够覆盖单个用例的全部
	// HTTP 请求，只在用例显式收紧时才会拒绝。
	runHarnessBurst = 1000

	// runHarnessDefaultConcurrent 是未显式 `setMaxConcurrent` 时的名额上限。
	// `memoryConcurrency` 对"限额缺失或为零"**拒绝全部**（fail-closed），因此夹具必须
	// 给每个用到的调用方留一条正数行，否则用例在 [3] 就被挡下、测不到它要测的退出路径。
	// A positive default keeps non-slot cases from being stopped at stage [3].
	runHarnessDefaultConcurrent = 8

	// runHarnessDefaultTasks 是未显式 `setTaskLimit` 时的周期任务次数：`ClassRun` 的
	// `Reserve` 同样对"限额缺失或为零"一律拒绝，没有这一档，非任务用例会先在 [5]
	// 的任务次数上被挡下，测不到它们真正要测的退出路径。
	// A generous default keeps non-task cases from being stopped at the task count.
	runHarnessDefaultTasks int64 = 1000
)

// errRunHarnessInjected 是夹具自身的故障注入错误（不是产品错误，不得与产品哨兵混用）。
var errRunHarnessInjected = errors.New("run harness: injected storage failure")

// ctx 是准入 / 配额用例共用的背景上下文（颗粒度文档 §4.4「片段约定」：共享夹具提供，
// 每个用例不重复声明）；需要取消或截止的用例自行派生。
// ctx is the shared background context; cancellation cases derive their own.
var ctx = context.Background()

// runHarness 是一次 R18 用例的完整对象图。
// runHarness is the full object graph behind one R18 case.
type runHarness struct {
	observation *Observability
	diag        *diagnostics.Recorder
	agents      *agentRegistry
	registry    *tool.Registry
	t           *testing.T
	db          *postgres.DB
	service     web.RunService
	// invoker 是**调度器的运行入口**（`admittedRunInvoker`）：没有 HTTP 入口，
	// 因此不扣入口速率，但要占名额。`fire` 走的就是这条生产路径。
	// invoker is the scheduler-side run entry: a slot, never an ingress token.
	invoker  *admittedRunInvoker
	limiter  *callers.Limiter
	quotas   *togglableQuotaStore
	slots    *runHarnessSlots
	buckets  *runHarnessBuckets
	sessions *runHarnessSessions
	audit    *togglableCallStore
	provider *scriptedProvider
	tools    *armedTool
	agent    string
	// message 是本 Run 的用户消息（同时是运行级知识检索的查询词）；空 = 默认"巡检"。
	// message is the run's user message and the run-level retrieval query.
	message string
	// scopes 是追加到调用方身份上的额外 scope（知识读 scope 落在这里）。
	// scopes are extra caller scopes on top of sessions:write.
	scopes []string
	// serviceOptions 是主服务的冻结装配；`workerService` 用它派生第二个 Worker
	// （同一组端口、独立进程内实例计数）。
	serviceOptions core.ServiceOptions
	// limitClock 是限额投影缓存的可推进时钟：管理面改完上限调 `tickLimitCache`
	// 让新值立刻在预检名额表与准入解析器上生效（生产 TTL 是 10s，用例不真的等）。
	// limitClock advances the limit projection cache so mid-test limit changes apply.
	limitClock *runHarnessLimitClock

	limitMu    sync.Mutex
	maxLimit   map[string]int
	tokenLimit map[string]int64
	// taskLimit / taskSet 一起区分"没设置"与"显式设成 0"：后者是 N-01 的失败关闭
	// 判据（零不等于不限量），不能被缺省值悄悄补齐。
	// taskSet separates "unset" from an explicit zero, which must stay fail-closed.
	taskLimit map[string]int64
	taskSet   map[string]bool
}

// runHarnessOptions 是按用例定制的装配增量；零值 = 与既有准入 / 配额用例逐字节相同的夹具。
// `Wire` 在注册表与仓储就位之后调用，用于把**真实**知识链路接进
// `core.ServiceOptions`——知识 E2E 的判据是 Run 链路（V-6），不是单独调用检索器。
// runHarnessOptions carries per-case assembly deltas; the zero value keeps every existing
// admission case identical. Wire runs once the registry exists so a case can plug the real
// knowledge pipeline into the production AgentService.
type runHarnessOptions struct {
	// Observability只给OBS贯通场景启用同源生产诊断装饰器；既有夹具默认不变。
	// Observability enables production diagnostic wiring only for observation scenarios.
	Observability     bool
	ObservationWiring func(*diagnostics.Recorder)
	RequestedTool     string
	// Knowledge 是 Agent 的 `knowledge:` 声明（进 `Profile.Knowledge`，驱动范围解析）。
	Knowledge []core.KnowledgeBinding
	// ExtraScopes 追加到调用方身份（`knowledge:read` 落在这里）。
	ExtraScopes []string
	// Message 是本 Run 的用户消息，同时是运行级检索的查询词。
	Message string
	// Wire 装配 Run 侧知识端口；nil = 未接线（既有行为逐字节不变）。
	Wire func(t *testing.T, db *postgres.DB, agents postgres.KnowledgeAgentSource) core.KnowledgeRunSource
	// Assembly 按库句柄构造对话装配记录写入器（V-7）：nil = 未接线，既有用例的行为与
	// 读数逐字节不变（尤其知识 E2E 的"该 Run 恰一条 retrieval 行"——接线后会多出 chat
	// 行，故只有显式要求证据面的用例开启）。用构造回调而不是现成实例，是因为夹具库在
	// 选项求值之后才建。
	// Assembly builds the chat assembly recorder from the fixture database; a nil factory
	// keeps every existing case byte-for-byte identical, including knowledge E2E row counts.
	Assembly func(t *testing.T, db *postgres.DB) core.AssemblyRecorder
	// ArmLostAckBeforeStartRun 在准入事务（US-24 Q21 起：账本行 + 清单 + 引用 + 租约 +
	// 任务次数记账**同一笔提交**）之前一刻装上"确认丢失"钩子。钩子必须精确挂在
	// `StartRun` 前：C-34 判的就是这笔提交的未知结局不得被当成"未提交"去重放。
	// The lost-ack hook arms right before StartRun — the single admission commit on the
	// run path since US-24 Q21.
	ArmLostAckBeforeStartRun bool
	// Learning 是抽取钩子；nil = 未装配（既有用例的行为与读数逐字节不变）。
	// Learning is the extraction hook; nil keeps every existing case identical.
	Learning core.LearningHook
	// MaxConcurrent 覆盖实例并发上限（`TOO_MANY_RUNS` 的判据面）：0 = 沿用夹具惯例的
	// 64（见 `serviceOptions` 注释：放大以免替调用方名额挡下请求）。错误优先级用例必须
	// 把它压到 1，才能让"实例满"真的发生在名额与会话门禁之后而不是之前。
	// MaxConcurrent overrides the per-worker instance cap; 0 keeps the fixture's 64.
	MaxConcurrent int
}

// newRunHarness 组装默认夹具。
// newRunHarness assembles the default fixture.
func newRunHarness(t *testing.T) *runHarness {
	return newRunHarnessWith(t, runHarnessOptions{})
}

// newRunHarnessWith 组装夹具：真实库 + 真实限流 / 配额 / 循环 / 运行入口，三个替身见文件头。
// newRunHarnessWith assembles the fixture described in the file header.
func newRunHarnessWith(t *testing.T, opts runHarnessOptions) *runHarness {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := pgtest.NewTestDB(t)
	root := t.TempDir()

	h := &runHarness{
		t:          t,
		db:         db,
		agent:      runHarnessAgentName,
		message:    opts.Message,
		scopes:     opts.ExtraScopes,
		sessions:   &runHarnessSessions{busy: make(map[string]bool)},
		provider:   newScriptedProvider(),
		tools:      &armedTool{},
		maxLimit:   make(map[string]int),
		tokenLimit: make(map[string]int64),
		taskLimit:  make(map[string]int64),
		taskSet:    make(map[string]bool),
		limitClock: &runHarnessLimitClock{},
	}
	writeRunHarnessAgent(t, root, h.agent, runHarnessProvider, runHarnessModel,
		[]string{runHarnessToolName}, core.KnowledgeBindingNames(opts.Knowledge))

	limits := concurrencyLimitReader(postgres.NewQuotaStore(db).MaxConcurrent)
	slots, limitCache := newRunHarnessConcurrency(t, limits, h.limitClock.now)
	h.slots = &runHarnessSlots{inner: slots, inUse: make(map[string]int)}
	h.buckets = newRunHarnessBuckets(t)
	quotas := &togglableQuotaStore{QuotaStore: postgres.NewQuotaStore(db)}
	h.quotas = quotas
	h.limiter = callers.NewLimiter(callers.LimiterDeps{
		Buckets:     h.buckets,
		Concurrency: h.slots,
		Quotas:      quotas,
	})
	h.audit = &togglableCallStore{inner: postgres.NewLLMCallRepo(db)}

	// 路由表逐条构造（不遍历推断），模型表与审计仓储同源：与 wire.go 的装配形状一致。
	var client core.ProviderClient = h.provider
	if opts.Observability {
		metrics := observability.NewMetricsCollector(postgres.NewObservabilityStore(db), time.Now)
		var err error
		h.diag, _, err = newDiagnosticsPortsWithMetrics(cli.DiagnosticsConfig{}, db, core.WorkerIdentity{}, time.Now, logger, metrics)
		require.NoError(t, err)
		cfg := cli.DefaultObservabilityConfig()
		h.observation, err = NewObservability(t.Context(), cfg, Deps{Store: postgres.NewObservabilityStore(db), Metrics: metrics, Diagnostics: h.diag, Clock: time.Now, Logger: logger})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, h.observation.Close()) })
		client = newDiagnosticProviderClient(client, h.diag)
		db.ObserveAuditWriteFailures(func() { metrics.IncCounter("obs_audit_write_failures_total", nil) })
		if opts.ObservationWiring != nil {
			opts.ObservationWiring(h.diag)
		}
	}
	h.provider.toolName = opts.RequestedTool
	routing := providersvc.NewProviderService(
		map[string]core.ProviderClient{runHarnessProvider: client},
		map[string]string{runHarnessProvider: runHarnessModel},
		h.audit,
		logger,
	)

	registry := tool.NewRegistry(logger)
	h.registry = registry
	require.NoError(t, registry.Register(h.tools), "夹具工具必须能注册")
	invocations := postgres.NewToolInvocationRepo(db)

	// 对话装配记录（V-7）只由显式开启的用例构造；nil = 未接线，与接线前逐字节一致。
	var assembly core.AssemblyRecorder
	if opts.Assembly != nil {
		assembly = opts.Assembly(t, db)
	}
	loop := core.NewReActLoop(
		core.NewPromptBuilder(registry),
		core.NewToolExecutor(registry, invocations, logger).WithDiagnostics(newToolDiagAdapter(h.diag, logger)),
		logger,
	).WithCallQuota(&chatQuota{
		limiter: h.limiter,
		// 注入位只改"预留多少"，不改变维度（`ClassChat` 恒记 Token）。
		tokens: func(core.CallQuotaRequest) int64 { return runHarnessReserveTokens },
		logger: logger,
		diag:   h.diag,
	}).WithAssemblyRecorder(assembly)

	entries := []core.AgentEntry{{
		Name:       h.agent,
		Registered: true,
		Profile: &core.Profile{
			Name:      h.agent,
			Provider:  core.ProviderRef{Name: runHarnessProvider, Model: runHarnessModel},
			Tools:     []string{runHarnessToolName},
			Knowledge: opts.Knowledge,
		},
	}}
	agents := newAgentRegistry(root, entries, logger)
	h.agents = agents
	// 账本读写用同一份仓储实例（生产装配的口径）：读面不得看到另一条连接池的影子。
	runs := postgres.NewRunStore(db)
	var runPort core.RunStore = runs
	if opts.ArmLostAckBeforeStartRun {
		// 只包运行入口这一个端口：读取面（`web.RunService` 的归档路径）仍拿真实仓储。
		runPort = &armingRunStore{inner: runs, before: func() {
			postgres.ArmLoseNextCommitAck(t, db)
		}}
	}
	// 实例级并发上限与线上缺省不同值是刻意的：用例判的是**调用方**名额（[3]），
	// 实例级上限放大到不会被误触的水平，避免它替 [3] 挡下请求。错误优先级用例
	// 经 `runHarnessOptions.MaxConcurrent` 显式调小。
	// The fixture inflates the instance cap so it never steals [3]'s denials.
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 64
	}
	serviceOptions := core.ServiceOptions{
		Agents:        agents,
		Client:        routing,
		Runs:          runPort,
		Snapshots:     postgres.NewSnapshotStore(db),
		Audit:         invocations,
		Sessions:      h.sessions,
		Loop:          loop,
		Logger:        logger,
		MaxConcurrent: maxConcurrent,
		// Run 级任务次数（N-01）：与 [3] / [5] 共用同一个限制器实例（C-35）。
		// US-24 P-12 起准入记账在 `StartRun` 事务内完成，本端口只作 Flow 准入前置。
		RunQuota: newRunQuota(h.limiter, h.diag, logger),
		Learning: opts.Learning,
		// 调用方并发判定输入（US-24 T013）：与上面的名额表共用同一份限额投影。
		// localAdminUnlimited 与限制器的同名开关同源——夹具里未开启，与生产缺省不同
		// 是有意的（夹具调用方都是 `PrincipalCaller`，走不到该分支）。
		Admission: &callerAdmissionResolver{limits: limitCache},
	}
	if opts.Wire != nil {
		// Run 侧知识端口按需接线：未接线时 `Knowledge` 保持 nil，服务路径逐字节不变。
		serviceOptions.Knowledge = opts.Wire(t, db, agents)
	}
	h.serviceOptions = serviceOptions
	svc := core.NewAgentService(serviceOptions)
	// 夹具按生产口径接上 [4] 预检的两个依赖（US-24 FR-24-34）：入口在取在途名额之前
	// 先判调用方并发，判据与 `core.AgentService` 的 [4] / `StartRun` 复查同源。
	admission := &runAdmission{
		limiter:   h.limiter,
		logger:    logger,
		diag:      h.diag,
		runs:      serviceOptions.Runs,
		admission: serviceOptions.Admission,
	}
	h.invoker = &admittedRunInvoker{inner: svc, admission: admission}
	h.service = NewRunService(svc, runs, withRunAdmission(admission))

	// 默认调用方先落一条限额行：它同时满足 `runs.caller_id` 外键与 [5] 的"限额缺失即拒绝"。
	h.applyLimit(t, runHarnessCaller)
	return h
}

// workerService 构造**另一个 Worker** 的服务实例：端口与主实例相同（同一 PostgreSQL、
// 同一批仓储），但进程内实例计数独立——`TOO_MANY_RUNS` 按每个 Worker 计就是靠这一点。
// 跨 Worker 的调用方并发仍由 `StartRun` 事务在共享库里复查，所以第二个 Worker 放行
// 不变量并不依赖任何进程内状态。
// workerService builds a second worker: same ports and database, its own in-process
// instance counter, which is exactly what the per-worker TOO_MANY_RUNS limit means.
func (h *runHarness) workerService(maxConcurrent int) *core.AgentService {
	opts := h.serviceOptions
	opts.MaxConcurrent = maxConcurrent
	return core.NewAgentService(opts)
}

// armingRunStore 在 `StartRun`（业务事务入口）之前装上"提交确认丢失"钩子，其余方法
// 原样转发。它**不参与判定**：只是把故障注入钉在正确的提交上。
// armingRunStore arms the lost-ack hook right before the business transaction.
type armingRunStore struct {
	inner  core.RunStore
	before func()
}

// StartRun 先装钩子再转调真实仓储（US-24 签名：owner / admission / runID 原样透传）。
func (s *armingRunStore) StartRun(ctx context.Context, rec core.RunRecord, manifest core.RunResourceManifest,
	owner core.WorkerIdentity, admission core.CallerAdmission, runID string,
) (core.LeaseToken, error) {
	if s.before != nil {
		s.before()
	}
	return s.inner.StartRun(ctx, rec, manifest, owner, admission, runID)
}

// FinishRun 原样转发。
func (s *armingRunStore) FinishRun(ctx context.Context, runID string, token core.LeaseToken, status core.RunStatus, reason string, iterations, toolCalls int, at time.Time) (bool, error) {
	return s.inner.FinishRun(ctx, runID, token, status, reason, iterations, toolCalls, at)
}

// CompleteRunCloseout 原样转发。
func (s *armingRunStore) CompleteRunCloseout(ctx context.Context, runID string, token core.LeaseToken, at time.Time) error {
	return s.inner.CompleteRunCloseout(ctx, runID, token, at)
}

// ReleaseRunRefs 原样转发。
func (s *armingRunStore) ReleaseRunRefs(ctx context.Context, runID string, at time.Time) error {
	return s.inner.ReleaseRunRefs(ctx, runID, at)
}

// CheckAdmission 原样转发。
func (s *armingRunStore) CheckAdmission(ctx context.Context, sessionID string, p core.Principal, admission core.CallerAdmission) error {
	return s.inner.CheckAdmission(ctx, sessionID, p, admission)
}

// LookupAdmittedRun 原样转发。
func (s *armingRunStore) LookupAdmittedRun(ctx context.Context, runID string) (core.LeaseToken, error) {
	return s.inner.LookupAdmittedRun(ctx, runID)
}

// RequestCancel 原样转发。
func (s *armingRunStore) RequestCancel(ctx context.Context, runID string) (core.RunDetail, error) {
	return s.inner.RequestCancel(ctx, runID)
}

// GetRun 原样转发。
func (s *armingRunStore) GetRun(ctx context.Context, runID string) (core.RunDetail, error) {
	return s.inner.GetRun(ctx, runID)
}

// ListRuns 原样转发。
func (s *armingRunStore) ListRuns(ctx context.Context, filter core.RunFilter) ([]core.RunDetail, error) {
	return s.inner.ListRuns(ctx, filter)
}

// newRunHarnessConcurrency 建真实名额表（限额读 `caller_limits.max_concurrent`，读不到
// 时回退实例级上限——与 `concurrency_limits.go` 的三档语义同一份实现）。
// newRunHarnessConcurrency builds the production slot table over the real limit reader.
func newRunHarnessConcurrency(
	t *testing.T, reader concurrencyLimitReader, now func() time.Time,
) (callers.Concurrency, *concurrencyLimitCache) {
	t.Helper()

	cache := newConcurrencyLimitCache(reader, 64, now)
	concurrency, err := callers.NewMemoryConcurrency(cache.limit)
	require.NoError(t, err)
	return concurrency, cache
}

// runHarnessLimitClock 是限额投影缓存的可推进时钟：只前进、不回拨，读数与真实时钟
// 相加，因此缓存的过期判定语义与生产逐字相同，用例只是把 TTL 提前走完。
// runHarnessLimitClock only moves forward; expiry semantics stay production-identical.
type runHarnessLimitClock struct {
	mu  sync.Mutex
	off time.Duration
}

// now 返回真实时钟加偏移。
func (c *runHarnessLimitClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

// advance 前进偏移。
func (c *runHarnessLimitClock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

// tickLimitCache 推进限额投影缓存，让刚写的调用方上限立刻在预检名额表与准入解析器
// 上生效（`concurrencyLimitTTL` 是生产行为，用例不真的等 10 秒）。
// tickLimitCache expires the limit projection so a just-written limit applies at once.
func (h *runHarness) tickLimitCache() {
	h.limitClock.advance(concurrencyLimitTTL + time.Second)
}

// runHarnessSlots 是名额表的**观察装饰器**：`Acquire` / `Release` 原样转给真实名额表，
// 另在内存里记一份计数供 `concurrencyInUse` 读数。它不参与判定，只提供"名额有没有
// 归零"这一个可观测量——因此不会掩盖生产路径漏释放。
// runHarnessSlots observes the real slot table; it never decides anything.
type runHarnessSlots struct {
	inner callers.Concurrency

	mu    sync.Mutex
	inUse map[string]int
}

// Acquire 转调真实名额表，成功后才计数。
func (s *runHarnessSlots) Acquire(ctx context.Context, callerID string) error {
	if err := s.inner.Acquire(ctx, callerID); err != nil {
		return err
	}
	s.mu.Lock()
	s.inUse[callerID]++
	s.mu.Unlock()
	return nil
}

// Release 转调真实名额表，再减计数（计数不参与判定，负数一律钳到 0）。
func (s *runHarnessSlots) Release(callerID string) {
	s.inner.Release(callerID)
	s.mu.Lock()
	if s.inUse[callerID] > 0 {
		s.inUse[callerID]--
	}
	s.mu.Unlock()
}

// count 返回该调用方当前占用的名额数。
func (s *runHarnessSlots) count(callerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inUse[callerID]
}

// runHarnessBuckets 是可换桶的入口限流端口：`setIngressQPS` 换掉内层令牌桶（限流预算是
// 类别级配置，不是调用方级——调用方只体现在桶键上），并记下每个调用方被拒的次数。
// runHarnessBuckets swaps the inner token bucket and counts denials per caller.
type runHarnessBuckets struct {
	mu     sync.Mutex
	inner  callers.RateBucket
	denied map[string]int
}

// newRunHarnessBuckets 建一个全类别都很宽松的初始桶（用例显式收紧才可能被拒）。
func newRunHarnessBuckets(t *testing.T) *runHarnessBuckets {
	t.Helper()

	return &runHarnessBuckets{
		inner:  newRunHarnessBucket(t, callers.IngressLimits{}),
		denied: make(map[string]int),
	}
}

// newRunHarnessBucket 按给定类别预算建真实内存桶；未指定的类别取 `runHarnessBurst`。
func newRunHarnessBucket(t *testing.T, limits callers.IngressLimits) callers.RateBucket {
	t.Helper()

	generous := callers.IngressBudget{QPS: runHarnessBurst, Burst: runHarnessBurst}
	if limits.Run.QPS <= 0 && limits.Run.Burst <= 0 {
		limits.Run = generous
	}
	if limits.Ingest.QPS <= 0 && limits.Ingest.Burst <= 0 {
		limits.Ingest = generous
	}
	if limits.Read.QPS <= 0 && limits.Read.Burst <= 0 {
		limits.Read = generous
	}
	bucket, err := callers.NewRateBucket(limits, nil)
	require.NoError(t, err)
	return bucket
}

// Allow 转调当前桶，并记下拒绝次数（拒绝也是可观测量，不只是错误返回）。
func (b *runHarnessBuckets) Allow(callerID string, class callers.IngressClass) bool {
	return b.AllowWith(callerID, class, nil)
}

// AllowWith 转调当前桶的覆盖判定；拒绝计数与 Allow 共用一份。
func (b *runHarnessBuckets) AllowWith(
	callerID string, class callers.IngressClass, override *callers.IngressBudget,
) bool {
	b.mu.Lock()
	inner := b.inner
	b.mu.Unlock()

	if inner.AllowWith(callerID, class, override) {
		return true
	}
	b.mu.Lock()
	b.denied[callerID]++
	b.mu.Unlock()
	return false
}

// swap 换掉内层桶；计数不随桶重置。
func (b *runHarnessBuckets) swap(next callers.RateBucket) {
	b.mu.Lock()
	b.inner = next
	b.mu.Unlock()
}

// deniedCount 返回该调用方的入口拒绝次数。
func (b *runHarnessBuckets) deniedCount(callerID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.denied[callerID]
}

// runHarnessSessions 是会话状态端口的替身：`MarkBusy` 之后该会话的状态读取返回
// `*core.SessionBusyError`，`AgentService.admit` 的 `default` 分支把它原样上抛，
// 因此这条拒绝路径就是 [4] 会话串行（真实实现的忙集在服务内部，测试无法外部置位）。
// runHarnessSessions reproduces the [4] session-serial rejection from the outside.
type runHarnessSessions struct {
	mu   sync.Mutex
	busy map[string]bool
}

// MarkBusy 把会话标成"已有运行在跑"。
func (s *runHarnessSessions) MarkBusy(t *testing.T, sessionID string) {
	t.Helper()

	s.mu.Lock()
	s.busy[sessionID] = true
	s.mu.Unlock()
}

// GetSession 对标记过的会话返回 `*core.SessionBusyError`，其余按"不存在"处理
// （`RequireExistingSession` 为假的请求因此照常放行，与生产语义一致）。
func (s *runHarnessSessions) GetSession(_ context.Context, id string) (core.SessionRecord, error) {
	s.mu.Lock()
	busy := s.busy[id]
	s.mu.Unlock()
	if busy {
		return core.SessionRecord{}, &core.SessionBusyError{SessionID: id}
	}
	return core.SessionRecord{}, fs.ErrNotExist
}

// ArchiveSession 不参与本文件判据：一律按"会话不存在"处理。
func (s *runHarnessSessions) ArchiveSession(context.Context, string, time.Time) error {
	return fs.ErrNotExist
}

// togglableQuotaStore 是可注入故障的配额端口：嵌入式接口让其余方法仍走真实实现，
// 只有 `Reserve` 可以被切成失败（C-02 的"PG 错误后不得继续受控消耗"）。
// togglableQuotaStore fails Reserve on demand and delegates everything else.
type togglableQuotaStore struct {
	callers.QuotaStore

	mu      sync.Mutex
	failing bool
}

// failReserve 切换 Reserve 的故障状态。
func (s *togglableQuotaStore) failReserve(on bool) {
	s.mu.Lock()
	s.failing = on
	s.mu.Unlock()
}

// Reserve 在故障状态下直接失败（不写预留行）。
func (s *togglableQuotaStore) Reserve(
	ctx context.Context, callerID string, class callers.RequestClass, cost callers.Cost, idem string,
) (callers.Reservation, error) {
	s.mu.Lock()
	failing := s.failing
	s.mu.Unlock()
	if failing {
		return callers.Reservation{}, errRunHarnessInjected
	}
	return s.QuotaStore.Reserve(ctx, callerID, class, cost, idem)
}

// togglableCallStore 是可注入故障的审计写端口（两阶段审计的预写闸门）。终态回填恒走
// 真实实现：`TestQuota_AuditFailureReleasesReservation` 判的是"预写失败 → 拒绝发出"，
// 不是回填失败。
// togglableCallStore fails the audit pre-write on demand; terminal writes stay real.
type togglableCallStore struct {
	inner core.LLMCallStore

	mu   sync.Mutex
	next error
}

// FailNext 让**下一次**预写失败并返回该错误（用后即清，后续调用回到真实实现）。
// 颗粒度文档 §4.4 以 `h.audit.FailNext(core.ErrAuditWrite)` 调用它。
// FailNext arms exactly one failed pre-write with the given error.
func (s *togglableCallStore) FailNext(err error) {
	s.mu.Lock()
	s.next = err
	s.mu.Unlock()
}

// BeginCall 在武装状态下失败一次——这就是"审计不可用"的那一次写。
func (s *togglableCallStore) BeginCall(ctx context.Context, call core.LLMCall) (int64, error) {
	s.mu.Lock()
	armed := s.next
	s.next = nil
	s.mu.Unlock()
	if armed != nil {
		return 0, armed
	}
	return s.inner.BeginCall(ctx, call)
}

// FinishCall 恒走真实实现。
func (s *togglableCallStore) FinishCall(ctx context.Context, id int64, res core.LLMCallResult) error {
	return s.inner.FinishCall(ctx, id, res)
}

// FencedBeginCall 把**受保护**预写原样交给真实仓储（US-24 的跨 Worker 用例需要
// 非零执行权凭证；归属校验与 `ErrLeaseLost` 由仓储判，夹具不该在旁边另判一份）。
// 武装开关（`FailNext`）只管普通入口：它模拟的是"审计不可用"这一种失败，不是失权。
// FencedBeginCall delegates the fenced pre-write to the real repo; the toggle only
// guards the unfenced entry because it simulates audit unavailability, not lease loss.
func (s *togglableCallStore) FencedBeginCall(ctx context.Context, token core.LeaseToken,
	call core.LLMCall,
) (int64, error) {
	fenced, ok := s.inner.(core.FencedChatCallStore)
	if !ok {
		return 0, fmt.Errorf("%w（llm_calls 存储 %T）", core.ErrLeaseFenceUnavailable, s.inner)
	}
	return fenced.FencedBeginCall(ctx, token, call)
}

// 脚本化 provider 的三种"非文本"模式。
const (
	scriptModeText      = "text"
	scriptModeToolCalls = "toolcalls"
	scriptModeTimeout   = "timeout"
	scriptModeBlock     = "block"
)

// scriptedProvider 是按脚本产出响应的模型协议客户端（`core.ProviderClient`）。
// `Router` 之外的一切仍走生产代码：路由、审计两阶段写、配额预留与结算都不在这里。
// scriptedProvider only replaces the protocol call itself.
type scriptedProvider struct {
	toolName string
	mu       sync.Mutex
	mode     string
	text     string
	rounds   int
	toolMax  int
	callsN   int
	hook     func(core.ChatRequest)

	firstCall chan struct{}
	once      sync.Once
}

// newScriptedProvider 构造脚本化客户端，初始脚本为"直接给出最终回答"。
func newScriptedProvider() *scriptedProvider {
	return &scriptedProvider{
		mode:      scriptModeText,
		text:      "done",
		firstCall: make(chan struct{}),
	}
}

// respondWithText 让下一次起的调用直接返回最终文本（并结束该 Run）。
func (p *scriptedProvider) respondWithText(text string) {
	p.mu.Lock()
	p.mode = scriptModeText
	p.text = text
	p.rounds = 0
	p.toolMax = 0
	p.mu.Unlock()
}

// respondWithToolCalls 让前 `n` 轮返回一次工具调用，第 `n+1` 轮返回最终文本——
// 一次 Run 因此恰好产生 `n+1` 次模型调用（卡片"4 次模型调用"取 n=3）。
func (p *scriptedProvider) respondWithToolCalls(n int) {
	p.mu.Lock()
	p.mode = scriptModeToolCalls
	p.rounds = 0
	p.toolMax = n
	p.text = "tool-path-done"
	p.mu.Unlock()
}

// failAfterTimeout 模拟**供应商侧**超时：直接返回超时错误且**不**取消调用方上下文
// （取消是另一种结局，由 `blockUntilCanceled` 覆盖）。用量因此未知，配额预留必须保持
// pending。
func (p *scriptedProvider) failAfterTimeout() {
	p.mu.Lock()
	p.mode = scriptModeTimeout
	p.mu.Unlock()
}

// blockUntilCanceled 让调用一直阻塞到 ctx 结束，再如实返回 ctx 的错误。
func (p *scriptedProvider) blockUntilCanceled() {
	p.mu.Lock()
	p.mode = scriptModeBlock
	p.mu.Unlock()
}

// observe 挂一个"调用发生时"的观察钩子（用于断言预留先于模型调用存在）。
func (p *scriptedProvider) observe(fn func(core.ChatRequest)) {
	p.mu.Lock()
	p.hook = fn
	p.mu.Unlock()
}

// waitFirstCall 阻塞到第一次模型调用已经开始。
func (p *scriptedProvider) waitFirstCall() {
	<-p.firstCall
}

// calls 返回已经发出的模型调用次数。
func (p *scriptedProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callsN
}

// Chat 按当前脚本产出一次响应。
func (p *scriptedProvider) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	p.mu.Lock()
	p.callsN++
	mode, text, round, toolMax, hook, toolName := p.mode, p.text, p.rounds, p.toolMax, p.hook, p.toolName
	if mode == scriptModeToolCalls {
		p.rounds++
	}
	p.mu.Unlock()

	p.once.Do(func() { close(p.firstCall) })
	if hook != nil {
		hook(req)
	}

	switch mode {
	case scriptModeTimeout:
		// 错误的文本只为可读性；判定看的是"ctx 未取消 + 调用失败"这一分支。
		return core.ChatResponse{}, fmt.Errorf("upstream timeout: %w", context.DeadlineExceeded)
	case scriptModeBlock:
		<-ctx.Done()
		return core.ChatResponse{}, ctx.Err()
	case scriptModeToolCalls:
		if round < toolMax {
			if toolName == "" {
				toolName = runHarnessToolName
			}
			return core.ChatResponse{
				ToolCalls: []core.ToolCall{{
					ID:        fmt.Sprintf("call-%d", round+1),
					Name:      toolName,
					Arguments: "{}",
				}},
				Usage: runHarnessUsage(),
			}, nil
		}
		return core.ChatResponse{Content: text, Usage: runHarnessUsage()}, nil
	default:
		return core.ChatResponse{Content: text, Usage: runHarnessUsage()}, nil
	}
}

// runHarnessUsage 是脚本响应的用量：非零，好让 `CommitCall` 走"已知用量"分支。
func runHarnessUsage() core.Usage {
	return core.Usage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10}
}

// armedTool 是 Agent 声明的唯一工具：默认返回成功，`panicOnInvoke` 之后每次执行都
// panic——用来验证 panic 由运行入口收口为 `failed` 且名额仍归还。
// armedTool panics on invoke while armed and succeeds otherwise.
type armedTool struct {
	mu          sync.Mutex
	armed       bool
	invocations int
	// onExecute 在每次执行时（解锁之后）回调；用例用它制造"工具执行期间状态被改"的
	// 时序（知识 E2E 的运行中删文档）。
	// onExecute fires once per execution, outside the lock, to build mid-run state changes.
	onExecute func()
}

// Name 返回工具名（与 AGENT.md 的 `tools:` 声明逐字一致）。
func (t *armedTool) Name() string { return runHarnessToolName }

// Description 是工具描述（进提示词的工具清单）。
func (t *armedTool) Description() string {
	return "夹具工具：默认成功，武装后模拟执行期 panic"
}

// InputSchema 是空对象 schema（本工具不读入参）。
func (t *armedTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// Execute 在武装状态下 panic，否则返回一段固定内容。
func (t *armedTool) Execute(context.Context, core.ToolEnv, json.RawMessage) (core.Result, error) {
	t.mu.Lock()
	armed := t.armed
	hook := t.onExecute
	t.invocations++
	t.mu.Unlock()

	if hook != nil {
		hook()
	}
	if armed {
		panic("fixture tool armed: simulating a panic inside tool execution")
	}
	return core.Result{Content: "tool-ok"}, nil
}

// onInvoke 挂一个执行期回调（在工具真正"执行"的时刻调用）。
// onInvoke registers the execution-time hook.
func (t *armedTool) onInvoke(fn func()) {
	t.mu.Lock()
	t.onExecute = fn
	t.mu.Unlock()
}

// invocationCount 返回已执行次数（读也走锁，避免与运行中的执行竞争）。
// invocationCount reads the counter under the same lock.
func (t *armedTool) invocationCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.invocations
}

// panicOnInvoke 武装工具（此后每次执行都 panic）。
func (t *armedTool) panicOnInvoke() {
	t.mu.Lock()
	t.armed = true
	t.mu.Unlock()
}

// restore 解除武装并清零计数。
func (t *armedTool) restore() {
	t.mu.Lock()
	t.armed = false
	t.invocations = 0
	t.mu.Unlock()
}

// writeRunHarnessAgent 落一份可装载的 AGENT.md：provider 决定路由（宪法 III 的查表键），
// `tools` 决定本次运行的工具集（未声明的工具连提示词都进不去），`knowledge` 是
// `knowledge:` 声明（与注册表里的 `Profile.Knowledge` 同源，文件保持自洽可读）。
// writeRunHarnessAgent writes a loadable AGENT.md carrying provider, tools and knowledge.
func writeRunHarnessAgent(t *testing.T, root, name, provider, model string, tools, knowledge []string) {
	t.Helper()

	dir := filepath.Join(root, "agents", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	var b strings.Builder
	b.WriteString("---\nname: " + name + "\ndescription: 准入夹具\n")
	b.WriteString("provider:\n  name: " + provider + "\n  model: " + model + "\n")
	if len(tools) > 0 {
		b.WriteString("tools:\n")
		for _, item := range tools {
			b.WriteString("  - " + item + "\n")
		}
	}
	if len(knowledge) > 0 {
		b.WriteString("knowledge:\n")
		for _, item := range knowledge {
			b.WriteString("  - name: " + item + "\n    access: read\n")
		}
	}
	b.WriteString("---\n\n你是 " + name + "。\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(b.String()), 0o644))
}

// applyLimit 把该调用方当前的（并发, Token）两档限额一并写库：`SetLimit` 是整行替换，
// 分两次写会让后写的一次抹掉前一次——夹具有责任让 `setMaxConcurrent` 之后
// 再 `setLimit` 时并发上限仍然生效。
// applyLimit rewrites both limit columns together; SetLimit replaces the whole row.
func (h *runHarness) applyLimit(t *testing.T, callerID string) {
	t.Helper()

	h.limitMu.Lock()
	limit := callers.Limit{
		MaxConcurrent:   h.maxLimit[callerID],
		TokensPerPeriod: h.tokenLimit[callerID],
		Period:          callers.PeriodDay,
		Timezone:        "UTC",
	}
	if limit.MaxConcurrent == 0 {
		limit.MaxConcurrent = runHarnessDefaultConcurrent
	}
	if limit.TokensPerPeriod == 0 {
		limit.TokensPerPeriod = runHarnessDefaultTokens
	}
	// 任务次数按"设置过才算"取值：显式 0 是 N-01 的失败关闭判据，不得被缺省补齐。
	limit.TasksPerPeriod = runHarnessDefaultTasks
	if h.taskSet[callerID] {
		limit.TasksPerPeriod = h.taskLimit[callerID]
	}
	h.limitMu.Unlock()

	require.NoError(t, h.quotas.SetLimit(context.Background(), callerID, limit))
}

// setMaxConcurrent 写该调用方的并发名额上限（图 6 [3] 的限额来源）。
func (h *runHarness) setMaxConcurrent(t *testing.T, callerID string, n int) {
	t.Helper()

	h.limitMu.Lock()
	h.maxLimit[callerID] = n
	h.limitMu.Unlock()
	h.applyLimit(t, callerID)
}

// setLimit 写该调用方的周期 Token 额度（图 6 [5] 的预算来源）。
func (h *runHarness) setLimit(t *testing.T, callerID string, tokens int64) {
	t.Helper()

	h.limitMu.Lock()
	h.tokenLimit[callerID] = tokens
	h.limitMu.Unlock()
	h.applyLimit(t, callerID)
}

// setTaskLimit 写该调用方的周期任务次数（图 6 [5] 的 `ClassRun` 一支，N-01）。
// 显式 0 会**原样落库**（零不等于不限量），因此它同时是失败关闭用例的入口。
// setTaskLimit writes the per-period task count; an explicit zero stays zero.
func (h *runHarness) setTaskLimit(t *testing.T, callerID string, tasks int64) {
	t.Helper()

	h.limitMu.Lock()
	h.taskLimit[callerID] = tasks
	h.taskSet[callerID] = true
	h.limitMu.Unlock()
	h.applyLimit(t, callerID)
}

// setIngressQPS 把入口桶换成"指定类别按 `qps`、其余类别宽松"的真实令牌桶。
//
// 限流预算是**类别级**配置（`callers.IngressLimits`），调用方只体现在桶键上；`callerID`
// 参数因此只用于可读性——与卡片夹具签名保持一致，不参与装配。
// The bucket budget is per class; the caller only keys the bucket.
func (h *runHarness) setIngressQPS(t *testing.T, _ string, class callers.IngressClass, qps float64) {
	t.Helper()

	// 速率 1 时给 1 个突发额度：第一个请求通过，第二个立刻被拒（固定时钟会饿死回填，
	// 这里用真实时钟 + 极小窗口，判据只依赖"第二个"这一次，不依赖秒级对齐）。
	budget := callers.IngressBudget{QPS: qps, Burst: int(qps)}
	if budget.Burst < 1 {
		budget.Burst = 1
	}
	limits := callers.IngressLimits{}
	switch class {
	case callers.IngressRun:
		limits.Run = budget
	case callers.IngressIngest:
		limits.Ingest = budget
	case callers.IngressRead:
		limits.Read = budget
	default:
		t.Fatalf("未知入口类别 %q", class)
	}
	h.buckets.swap(newRunHarnessBucket(t, limits))
}

// ensureCaller 为该调用方落一条缺省限额行：`callers` 行是 `runs.caller_id` 外键与
// Token 账本的前提（`Reserve` 对"限额缺失"一律拒绝），而 `max_concurrent` 的缺省值由
// `applyLimit` 补齐。夹具装配面，不是判定面。
// ensureCaller writes the default limit row a caller needs before any run.
func (h *runHarness) ensureCaller(t *testing.T, callerID string) {
	t.Helper()

	h.applyLimit(t, callerID)
}

// fire 走**调度器的运行入口**（`admittedRunInvoker`）：没有 HTTP 入口，因此不扣入口
// 速率，但要占名额。颗粒度文档 §4.4 的 `TestIngress_SchedulerBypassesIngressBucket`
// 判的就是这条路径不碰 HTTP 桶。
// fire runs one background-triggered invocation through the scheduler entry.
func (h *runHarness) fire(t *testing.T, callerID string) {
	t.Helper()

	res, err := h.invoker.Run(context.Background(), h.requestFor(callerID))
	require.NoError(t, err, "后台触发的运行期失败仍走 RunResult，不经 error")
	require.Equal(t, core.StatusCompleted, res.Status, "后台触发必须真的执行完成")
}

// principal 构造一个合法的调用方身份（零值 Principal 在任何副作用之前就被拒绝，
// 夹具必须给出 `Valid()` 为真的那个形状）。额外 scope 由用例经 `ExtraScopes` 追加。
func (h *runHarness) principal(callerID string) core.Principal {
	scopes := make([]string, 0, 1+len(h.scopes))
	scopes = append(scopes, core.ScopeSessionsWrite)
	scopes = append(scopes, h.scopes...)
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		KeyID:    callerID + "-key",
		Scopes:   scopes,
		Actor:    core.ActorCaller,
	}
}

// harnessBudget 是夹具的执行预算：迭代与工具次数要容得下 4 次模型调用（3 轮工具 +
// 1 轮最终回答），上下文余量取够大以免触发压缩路径。
func harnessBudget() core.Budget {
	return core.Budget{MaxIterations: 8, MaxToolCalls: 8, ContextTokens: 100000}
}

// requestFor 构造一次无状态运行请求（空 session_id：同一调用方的多次运行互不串行）。
// 默认消息是"巡检"；知识用例用 `Message` 指定可检索的查询词。
func (h *runHarness) requestFor(callerID string) core.RunRequest {
	message := h.message
	if message == "" {
		message = "巡检"
	}
	return core.RunRequest{
		Principal: h.principal(callerID),
		Agent:     h.agent,
		Message:   message,
		Channel:   "web",
		Budget:    harnessBudget(),
	}
}

// requestForSession 构造一次绑定到指定会话的运行请求。
func (h *runHarness) requestForSession(callerID, sessionID string) core.RunRequest {
	req := h.requestFor(callerID)
	req.SessionID = sessionID
	return req
}

// postChat 复现 web 中间件的一次对话请求：先扣入口速率（图 6 [1]），被拒时**不**进入
// 运行入口；放到 [3] 之后判速率就等于让被拒的请求先占了名额。
//
// 中间件到 `AdmitIngress` 的映射由 `internal/web` 的 `TestIngress_HTTPIngestRateLimited`
// 钉住；本夹具判的是顺序与"被拒请求不占名额"这一性质。
// postChat mirrors the middleware: [2] then the run entry.
func (h *runHarness) postChat(ctx context.Context, callerID string) (core.RunResult, error) {
	if err := h.limiter.AdmitIngress(ctx, h.principal(callerID), callers.IngressRun); err != nil {
		return core.RunResult{}, err
	}
	return h.service.Run(ctx, h.requestFor(callerID))
}

// concurrencyInUse 返回该调用方当前占用的运行并发名额。
func (h *runHarness) concurrencyInUse(t *testing.T, callerID string) int {
	t.Helper()

	return h.slots.count(callerID)
}

// ingressRejections 返回该调用方被入口速率拒绝的次数。
func (h *runHarness) ingressRejections(t *testing.T, callerID string) int {
	t.Helper()

	return h.buckets.deniedCount(callerID)
}

// usageRow 读取该调用方 `tokens` 账本的 (已结算, 待结算) 合计。用例各自只落在一个
// 周期里，跨周期求和因此等价于颗粒度文档 §4.4 的按周期读数，且不把周期起算写进夹具。
// usageRow reads the settled and reserved token totals for one caller.
func (h *runHarness) usageRow(t *testing.T, callerID string) (used, reserved int64) {
	t.Helper()

	require.NoError(t, h.db.QueryRowContext(context.Background(), `
		SELECT COALESCE(SUM(used), 0), COALESCE(SUM(reserved), 0)
		FROM caller_usage_periods
		WHERE caller_id = $1 AND kind = 'tokens'`,
		callerID,
	).Scan(&used, &reserved))
	return used, reserved
}

// reservedTokens 是 `usageRow` 的窄读数版（只要待结算预留），供 `usageReserved` 用。
func (h *runHarness) reservedTokens(callerID string) (int64, error) {
	var reserved int64
	err := h.db.QueryRowContext(context.Background(), `
		SELECT COALESCE(SUM(reserved), 0)
		FROM caller_usage_periods
		WHERE caller_id = $1 AND kind = 'tokens'`,
		callerID,
	).Scan(&reserved)
	return reserved, err
}

// usageReserved 是 `reservedTokens` 的断言版（夹具读失败一律 Fatal，不当作 0）。
func (h *runHarness) usageReserved(t *testing.T, callerID string) int64 {
	t.Helper()

	reserved, err := h.reservedTokens(callerID)
	require.NoError(t, err)
	return reserved
}

// taskUsage 读取该调用方 `tasks` 账本的 (已结算, 待结算) 合计（与 `usageRow` 同形，
// 只是换了账本维度）：N-01 的判据是任务次数而不是 Token。
// taskUsage reads the task ledger totals for one caller.
func (h *runHarness) taskUsage(t *testing.T, callerID string) (used, reserved int64) {
	t.Helper()

	require.NoError(t, h.db.QueryRowContext(context.Background(), `
		SELECT COALESCE(SUM(used), 0), COALESCE(SUM(reserved), 0)
		FROM caller_usage_periods
		WHERE caller_id = $1 AND kind = 'tasks'`,
		callerID,
	).Scan(&used, &reserved))
	return used, reserved
}

// taskReservations 返回该调用方落在 `ClassRun` 上的预留行数与状态集合——"重放不产生
// 第二条预留"与"崩溃恢复把 pending 收口"都靠它判。
// taskReservations counts the run-class reservation rows for one caller.
func (h *runHarness) taskReservations(t *testing.T, callerID string) map[string]int {
	t.Helper()

	rows, err := h.db.QueryContext(context.Background(), `
		SELECT state, COUNT(*) FROM quota_reservations
		WHERE caller_id = $1 AND class = 'run'
		GROUP BY state`, callerID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	states := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		require.NoError(t, rows.Scan(&state, &n))
		states[state] = n
	}
	require.NoError(t, rows.Err())
	return states
}

// runReservationID 读账本行上的预留标识（N-01：Run 与 `quota_reservations` 的关联列）。
// runReservationID reads the reservation id recorded on the run ledger row.
func (h *runHarness) runReservationID(t *testing.T, runID string) string {
	t.Helper()

	var id string
	require.NoError(t, h.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(reservation_id, '') FROM runs WHERE run_id = $1`, runID).Scan(&id))
	return id
}

// countRuns 返回该调用方的运行账本行数（"不得盲目重放业务事务"的判据）。
func (h *runHarness) countRuns(t *testing.T, callerID string) int {
	t.Helper()

	var n int
	require.NoError(t, h.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runs WHERE caller_id = $1`, callerID).Scan(&n))
	return n
}

// 编译期断言：替身满足它们替代的端口（缺一个会在装配处失败，而不是运行期 panic）。
var (
	_ core.ProviderClient     = (*scriptedProvider)(nil)
	_ core.Tool               = (*armedTool)(nil)
	_ core.SessionStatusStore = (*runHarnessSessions)(nil)
	_ core.LLMCallStore       = (*togglableCallStore)(nil)
	_ callers.QuotaStore      = (*togglableQuotaStore)(nil)
	_ callers.Concurrency     = (*runHarnessSlots)(nil)
	_ callers.RateBucket      = (*runHarnessBuckets)(nil)
	_ web.RunService          = (*runService)(nil)
	_ core.RunInvoker         = (*core.AgentService)(nil)
	_ core.ProviderClient     = (*providersvc.ProviderService)(nil)
)
