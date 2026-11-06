package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"
)

// 本文件是**唯一运行入口**（颗粒度文档 §2.3 流程 A）：准入 → 冻结快照并落库 → 取会话 →
// 组装运行环境 → 预写账本 → 写用户消息 → 循环 → 终态落库 → 返回。
// This file is the single run entry point (flow A): admission, frozen snapshot, session,
// run environment, ledger pre-write, user message, loop, terminal write, return.
//
// 两条纪律。① **准入不排队**：同会话忙、实例满都是直接拒绝——排队会把"立即失败"变成
// "过一会儿失败"，调用方拿到的是错误的调度语义（契约 §1.2）。② **证据先行**：快照落库、
// 账本预写、用户消息写入三步都在模型调用之前，任一步失败即拒绝启动——审计链要求的
// "先留痕再发出"在运行级同样成立。
// Two rules: admission never queues, and evidence is written before the model is called.

// DefaultMaxConcurrentRuns 是实例级并发运行数的缺省上限（流程 A ①）。
// 导出是因为管理台的就绪视图（14D / E8）要把"生效上限"与 Agent 的计划数对照，
// 而缺省值必须与本包取同一份——两处各写一个 32 迟早对不上。
// DefaultMaxConcurrentRuns is the default instance-level concurrency cap. It is exported
// so the console's readiness view compares against the same default this package applies.
const DefaultMaxConcurrentRuns = 32

// 运行预算的缺省值（契约 §6）：调用方未填的项在这里补齐。
// Default budget values (contract §6), applied when the caller leaves a field unset.
const (
	// DefaultMaxIterations 是实例预算未配置时生效的迭代上限；导出理由同
	// DefaultMaxConcurrentRuns（就绪视图的预算兼容检查）。
	// DefaultMaxIterations is the effective iteration cap when the instance budget is
	// unset; exported for the same readiness comparison.
	DefaultMaxIterations = 10
	defaultMaxToolCalls  = 50
)

// 取消等待的缺省上限（约束 20）：取消只发信号，**等循环自己收场**；超过这个上限就
// 如实返回当前状态，不代写终态。
// defaultCancelWait bounds how long Cancel waits for the loop to finish on its own; past
// it Cancel reports the current state instead of writing one.
const defaultCancelWait = 30 * time.Second

// cancelReadTimeout 是取消路径读库的独立上限：等待可能已耗尽调用方的期限，读当前状态
// 仍须完成（读失败才如实上抛）。
// cancelReadTimeout bounds the detail read on the cancel path; the caller's deadline may
// already be gone, yet reporting the current state still has to complete.
const cancelReadTimeout = 5 * time.Second

// AgentSource 是运行入口需要的 Agent 装载面——**端口定义在消费方**（core），适配器在 app
// （宪法·架构约束：接口在消费方、实现在别处、装配在 app）。
// AgentSource is the agent-loading face the run entry point needs; the port is declared by
// its consumer and adapted in app.
type AgentSource interface {
	// Lookup 取已注册的 Agent 条目；未注册 = 调用方问题，不落任何痕迹（图 F ①）。
	// 注册不变量由注册表自身维持（未注册条目不得出现在注册表里），准入因此不需要
	// 再看 `Registered` 字段。
	// Lookup returns the registered agent entry; a miss is a caller problem and leaves no
	// trace. The registry itself upholds the registered-only invariant.
	Lookup(name string) (AgentEntry, bool)

	// Dir 返回 Agent 目录的绝对路径（进 `ToolEnv.AgentDir`，工具据它做沙箱判定）。
	// Dir returns the agent directory, which tools use for sandbox decisions.
	Dir(name string) string

	// Snapshot 从**准入条目携带的已校验字节**冻结一次运行的配置快照（同一份字节 → Profile +
	// 段一正文 + 哈希）：不第二次查注册表、不回读磁盘——准入与快照因此是同一份配置
	//（人工 review ② 缺陷 1）。
	// Snapshot freezes the run snapshot from the validated bytes carried by the admitted
	// entry; it neither re-looks-up the registry nor re-reads the disk.
	Snapshot(ctx context.Context, entry AgentEntry, now time.Time) (*ConfigSnapshot, error)
}

// SessionStatusStore 是会话状态端口（消费方 = AgentService，实现 = storage.SessionStore）：
// 运行准入据它拒绝已归档会话，归档动作经它与在途运行互斥。未命中一律 fs.ErrNotExist。
// SessionStatusStore is the session-status port the entry point consumes; a miss is
// always fs.ErrNotExist.
type SessionStatusStore interface {
	GetSession(ctx context.Context, id string) (SessionRecord, error)  // 未命中返回 fs.ErrNotExist
	ArchiveSession(ctx context.Context, id string, at time.Time) error // 未命中返回 fs.ErrNotExist
}

// ServiceOptions 是运行入口的装配面。字段即依赖：没有任何一项从包级状态或 context 里取
// （宪法 VII）——`Now` 也显式注入，业务时间戳因此在测试里完全可控。
// ServiceOptions is the assembly surface of the run entry point; every dependency is an
// explicit field, including the clock.
type ServiceOptions struct {
	Phases    PhaseRecorder        // Optional droppable stage timing; never controls admission.
	Agents    AgentSource          // Agent 装载面（是否存在 / 目录 / 运行级快照）
	Client    ProviderClient       // 唯一的模型调用入口（实现为 ProviderService：路由 + 两阶段审计在其内）
	Messages  MessageStore         // 会话历史写透（无状态调用也可为 nil）
	Runs      RunStore             // 运行账本
	Snapshots SnapshotStore        // 配置快照落库
	Audit     ToolInvocationReader // 半截组补齐的分类依据（只读；可为 nil → 一律走"无行"分支）
	Memory    MemoryReader         // 记忆读取端口（可空：nil = 段 2 恒空，行为与无记忆逐字节相同）
	// Knowledge 是 Run 侧知识链路端口（V-6：检索 / 轮内复核 / 引用复核）。
	// nil = 未接线知识库：不检索、注入面恒空、行为与接线前逐字节相同。
	// Knowledge is the run-side knowledge port. Nil keeps the pre-wiring behavior byte for
	// byte: no retrieval, empty injection.
	Knowledge     KnowledgeRunSource
	Loop          *ReActLoop       // 自实现的循环本体
	Logger        *slog.Logger     // 为空 → 进程缺省日志器
	Now           func() time.Time // 时钟注入；为空 → time.Now
	MaxConcurrent int              // 实例级并发运行上限；<=0 → 32
	// Nil reserves one interactive instance slot; explicit zero opts into no reserve.
	FlowInteractiveReservedRuns *int
	// FlowRunsReady is app-owned assembly control; never a configuration/DTO flag.
	// It remains false in production until durable wiring and behavior gates pass.
	FlowRunsReady bool
	// FlowWritesReady is separately app-owned and stays false until G-FT behavior
	// evidence proves the controlled fixed-write branch. Agent tools stay readonly.
	FlowWritesReady bool
	// Sessions 是会话状态端口（归档 / 缺失判定）；生产装配必须由 wire.go 单点传入。
	// nil = 不做会话门禁（仅既有零值单测），此时 Archive 返回 *SessionGateUnavailableError，
	// 不得 nil 解引用。
	// Sessions is the session-status port. Nil disables the gate (zero-value tests only);
	// Archive then returns *SessionGateUnavailableError instead of dereferencing nil.
	Sessions SessionStatusStore
	// Lifecycle 是生命周期状态文件端口（图 F ③）：每次准入在 Agent 级读锁内**现场重读**，
	// 只有 active 放行；缺失 / 损坏 / 停用 / 归档一律 *AgentUnavailableError（失败关闭）。
	// nil = 不做生命周期门禁（仅既有零值单测）；生产装配必须由 wire.go 单点传入。
	// Lifecycle is the lifecycle state-file port. Admission re-reads it under the agent's
	// read lock on every run; nil disables the gate for zero-value tests only.
	Lifecycle LifecyclePort
	// Gate 是 Agent 级读写锁（准入读锁 / 启停写锁 / 内容写锁共用同一实例）。准入只在
	// 状态读取这一小段持读锁，**不跨越**快照读取、模型调用与工具执行。
	// Gate is the per-agent read/write lock shared by admission, lifecycle transitions and
	// content edits; admission holds it only across the state-file read.
	Gate *AgentGate
	// Resources 是准入资源解析端口（图 F ③）：在 ResourceGate 读锁内查发布指针并固定
	// 运行时。非 nil 时本服务同时要求 Lifecycle 与 RunSnapshots 非 nil——资源清单要有
	// 权威生命周期状态，快照要按发布版本构建；缺一即装配错误、启动前失败关闭。
	// nil = 尚未接线（资源解析与清单写入整体关闭，运行读作"未记录"）。
	// Resources resolves the published resources at admission. When non-nil it requires
	// Lifecycle and RunSnapshots as well; a partial wiring fails closed before any run.
	Resources RunResources
	// RunSnapshots 把解析结果与配置快照合成一次运行的冻结输入（越早固定越不会漂移）。
	// nil = 尚未接线；与 Resources 必须同进同出。
	// RunSnapshots folds resolved resources into the run's frozen snapshot; it must be
	// wired together with Resources.
	RunSnapshots RunSnapshotBuilder
	// ToolPins 是准入时钉住工具实例的解析面（14D / C4）：注册池可被受控重建整组换掉，
	// 本次运行手里的实例由此固定。nil = 不钉（既有零值单测），执行器按名查注册池。
	// ToolPins freezes this run's tool instances against a later controlled rebuild; a
	// nil pinner keeps the pre-14D name lookup.
	ToolPins ToolPinner
	// RunQuota 是 Run 级任务次数端口（图 6 [5] 的 `ClassRun` 一支，N-01）：在全部准入
	// 通过之后、业务执行之前预留并结算 1 次。nil = 不记账（既有零值单测与轻装配），
	// 行为与接线前逐字节相同；生产装配由 wire.go 单点接上。
	// RunQuota charges one task per admitted run before any business write; nil keeps the
	// pre-wiring behaviour byte for byte.
	RunQuota RunQuota
	// Learning 是 Run 终态之后的**唯一**同步回调（US-22 记忆抽取）；nil = 关闭，
	// 行为与接线前逐字节相同。它**没有返回值**：从类型上就不可能改变 Run 的结果、
	// 状态或错误；触发点只在本文件的 `finish`（C-05），**不得**在 web / CLI / 调度器
	// 另接第二个。
	// Learning is the one synchronous callback after a run's terminal state. Nil keeps the
	// pre-wiring behaviour byte for byte, and the hook cannot change the run's result by
	// construction; finish is its only trigger point.
	Learning LearningHook
}

// AgentService 是唯一运行入口的实现：准入计数 + 一次运行的完整编排。
// **唯一入口**不是修辞：定时触发、评测基座、REST 端点都经它跑，渠道差异只体现在
// `RunRequest.Channel` 上（触发源的映射由仓储按 Channel 完成）。
// AgentService is *the* run entry point: schedulers, evals and future REST endpoints all
// run through it, and channels differ only by RunRequest.Channel.
type AgentService struct {
	opts ServiceOptions

	mu          sync.Mutex
	busy        map[string]struct{}   // 会话忙集：键 = session_id（空串不入集——无状态调用之间互不相干）
	active      map[string]*activeRun // 在途运行登记：键 = run_id（取消信号的唯一入口）
	running     int                   // 实例级在跑数
	flowRunning int

	// cancelWait 是取消信号发出后等待循环收场的上限（约束 20）。生产恒为
	// defaultCancelWait；测试经 export_test.go 改小，免得超时用例真等 30 秒。
	// cancelWait is the bound Cancel waits for the loop; tests shorten it through
	// export_test.go so the timeout case does not really wait 30 seconds.
	cancelWait time.Duration
}

// activeRun 是一次在途运行的取消登记：`cancel` 只取消**循环自己的**上下文（准备阶段与
// 终态写入仍用调用方 ctx，取消不会让终态写不进去）；`done` 在 Run 返回、终态落库之后
// 关闭——取消路径据此判断"等到了没有"，**从不**自行写终态。
// activeRun is the cancel registration of one in-flight run. cancel only cancels the
// loop's context, so preparing the run and writing its terminal row stay unaffected; done
// closes after Run returns, i.e. after the terminal state is persisted.
type activeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// waitOutcome 是 Cancel 等待循环收场的三态结果。
// waitOutcome is the three-way result of Cancel's bounded wait.
type waitOutcome int

const (
	// waitFinished：循环已收场，终态已落库。
	waitFinished waitOutcome = iota
	// waitTimedOut：到达等待上限仍未结束——如实返回当前状态并记 WARN。
	waitTimedOut
	// waitAborted：调用方取消了自己的上下文——停止等待，不算降级。
	waitAborted
)

// NewAgentService 组装运行入口。缺省值在此补齐（缺省日志器 / 时钟 / 并发上限），
// 免得每个消费点各判一次 nil。
// NewAgentService assembles the entry point and fills the three documented defaults.
func NewAgentService(opts ServiceOptions) *AgentService {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultMaxConcurrentRuns
	}
	return &AgentService{
		opts:       opts,
		busy:       map[string]struct{}{},
		active:     map[string]*activeRun{},
		cancelWait: defaultCancelWait,
	}
}

// Run 执行一次运行。返回的 `RunResult` 无论成败都非零（Status 表达结局）；**函数级 error
// 只表达调用方问题**：Agent 不存在 / 同会话忙 / 实例满（契约 §1.2）——这三类**不排队**、
// 不落任何痕迹。其余失败一律体现在 `RunResult.Status`（含 failed 的 Reason）。
// Run runs one run. A function-level error means a caller-side problem only; everything
// else shows up in the result status.
func (s *AgentService) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	// ⓪ 身份准入（设计 §3.1.0 / P-04）：零值或非法 Principal 在**任何副作用之前**以
	// `ErrUnauthorized` 拒绝——不排队、不落痕迹，也**不得**补成内置管理员再继续
	// （"零值即管理员"正是 C-28 要关掉的那条提权路径）。
	// The identity gate precedes every side effect: a missing or invalid principal is
	// rejected with ErrUnauthorized and never defaulted to the built-in admin.
	if !req.Principal.Valid() {
		return RunResult{}, fmt.Errorf("%w: run request carries no valid principal", ErrUnauthorized)
	}
	// The durable adapter is not yet wired. Fail closed until strict Flow binding
	// and the G-FT implementation gates replace this guard; never run it as an
	// ordinary unrestricted Agent by merely setting a Flow pointer.
	if req.FixedTool != nil && req.Flow == nil {
		return RunResult{}, ErrFlowDispatch
	}
	if req.Flow != nil && (!s.opts.FlowRunsReady || !validFixedFlowRequest(req) || s.opts.Resources == nil || s.opts.Lifecycle == nil || s.opts.RunSnapshots == nil || s.opts.ToolPins == nil || s.opts.RunQuota == nil) {
		return RunResult{}, ErrFlowDispatch
	}

	// ① 准入：未知 Agent → 会话锁 → 实例计数（顺序即错误优先级），三道都**不排队**。
	entry, ok := s.lookupAgent(req.Agent)
	if !ok {
		return RunResult{}, &AgentNotFoundError{Name: req.Agent}
	}
	release, err := s.admit(ctx, req)
	if err != nil {
		return RunResult{}, err
	}
	defer release()

	if s.opts.Loop == nil {
		// 装配错误（引擎没接上）：照实报，不落痕迹——半截账本比不跑更糟。
		return RunResult{}, errors.New("agent service: run loop is not configured")
	}
	// 资源接线的三件必须同进同出：只有解析端口而没有快照构建 / 生命周期端口时，清单的
	// 状态与技能冻结都没有权威来源——装配错误，启动前失败关闭，不落半套账。
	// The three resource ports must be wired together; a partial wiring is an assembly
	// error and fails closed before any trace is written.
	if s.opts.Resources != nil && (s.opts.RunSnapshots == nil || s.opts.Lifecycle == nil) {
		return RunResult{}, errors.New("agent service: run resources are configured without a snapshot builder or lifecycle port")
	}

	// ③ 生命周期门禁（图 F ③）：在 Agent 级读锁内**现场重读**状态文件——不得缓存为
	// "目录存在即 active"，也不得只信 SQLite 索引；只有 active 放行，缺失 / 损坏 /
	// 停用 / 归档一律失败关闭（409 AGENT_UNAVAILABLE）。读锁只覆盖这一小段，不跨越
	// 快照读取、模型调用与工具执行；停用因此不会取消在途运行。
	// The lifecycle gate re-reads the state file under the agent's read lock; the lock
	// never spans the model call or tool execution, so disabling leaves in-flight runs
	// untouched.
	lifecycle, err := s.admitLifecycle(ctx, req.Agent)
	if err != nil {
		return RunResult{}, err
	}
	if req.Flow != nil && (string(lifecycle.AgentUID) != req.Flow.AgentUID || !slices.Contains(req.Flow.Authority.AgentUIDs, req.Flow.AgentUID)) {
		return RunResult{}, ErrFlowDispatch
	}

	// ③ 资源固定（图 F ③，紧随门禁）：解析在 ResourceGate 读锁内完成；清单的生命周期
	// 状态取**本次准入读到的**记录（同一份读数），技能 / MCP / Provider 版本与哈希自此
	// 冻结。读锁租约随解析结果延续到 **StartRun 成功之后**（人工 review ② US-12 交接
	// 裁决）：成功路径在此显式 ReleaseAdmission，运行时本身活到运行结束经 defer Release
	// （约束 46）；解析之后、StartRun 之前的任何失败分支都只经 defer Release 一并归还
	// 读锁与运行时——不得留下未归还的准入租约。
	// The admission read lock is leased until StartRun succeeds; every failure path
	// returns it through the deferred Release.
	var (
		resolved  ResolvedResources
		manifest  RunResourceManifest
		llmClient = s.opts.Client
	)
	if s.opts.Resources != nil {
		resolved, err = s.opts.Resources.Resolve(ctx, entry)
		if err != nil {
			return RunResult{}, s.resourceUnavailable(req.Agent, err)
		}
		if resolved.Runtime != nil {
			defer resolved.Runtime.Release()
		}
		// 模型调用入口从准入固定的运行时取（人工 review ② 缺陷 2）：Run 内路由只查这
		// 一张固定表（宪法 III），且发放的视图仍带两阶段审计（宪法 V）。缺条目 = 准入
		// 与映射表分叉，失败关闭，不得退回全局客户端"照样能跑"。
		// The model client comes from the pinned runtime; a missing entry fails closed
		// instead of falling back to the global client.
		llmClient, err = pinnedClientWhenNeeded(resolved, req.FixedTool == nil)
		if err != nil {
			return RunResult{}, s.resourceUnavailable(req.Agent, err)
		}
		manifest, err = BuildRunResourceManifest(lifecycle.AgentUID, lifecycle.State, resolved)
		if err != nil {
			return RunResult{}, s.resourceUnavailable(req.Agent, err)
		}
		if req.Flow != nil && manifest.Hash != req.Flow.ResourceManifestHash {
			return RunResult{}, ErrFlowDispatch
		}
	}

	now := s.opts.Now()
	// 调用方给了 RunID 就原样用（调度器预生成，用于把执行行与 Run 关联）；否则按既有
	// 公式生成——既有 CLI / REST / 评测调用方零改动（约束 13 / 14）。
	runID := req.RunID
	if runID == "" {
		runID = NewRunID(req.Channel, now, runSuffix())
	}

	// ⑤ 在途登记（约束 20）：登记发生在快照 / 会话 / 账本预写**之前**，取消因此覆盖整个
	// 在途窗口；登记后的每个返回路径都由 defer 摘除并关 `done`——循环收场、终态落库之后
	// 取消方才会看到"已结束"。准备阶段仍用调用方 `ctx`，只有循环拿到可取消的 `runCtx`：
	// 取消不会让终态写不进去（写终态用的仍是调用方 ctx）。
	// Registration precedes snapshot/session/ledger writes so cancellation covers the whole
	// in-flight window; every later return path unregisters via defer. Only the loop gets
	// the cancelable context, so cancellation can never prevent the terminal write.
	runCtx, cancelRun := context.WithCancel(ctx)
	run := s.registerActive(runID, cancelRun)
	defer s.unregisterActive(runID, run)
	defer cancelRun()

	// ② 冻结运行级快照并**先落库**：读盘失败或落库失败都拒绝启动（零 LLM 调用、零工具执行）。
	// 这一份快照在 Run 内不变——中途改盘不换配置（读即所记）。
	snap, err := s.buildRunSnapshot(ctx, entry, resolved, now)
	if err != nil {
		return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error()}, nil
	}
	if req.Flow != nil && snap.Hash != req.Flow.AgentConfigHash {
		return RunResult{}, ErrFlowDispatch
	}
	if s.opts.Snapshots != nil {
		if err := s.opts.Snapshots.SaveSnapshot(ctx, snap); err != nil {
			return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error()}, nil
		}
	}
	req.ConfigHash = snap.Hash // 运行期填充（调用方不填）——与审计列同源

	// ③ 取会话：有 session_id → 建行并载入历史；空 → 仅内存（无状态调用仍落 Run 与审计）。
	sess := NewSession(req.SessionID, s.opts.Messages, s.opts.Audit)
	if req.SessionID != "" && s.opts.Messages != nil {
		if err := s.opts.Messages.UpsertSession(ctx, SessionRecord{
			ID:           req.SessionID,
			ProfileName:  req.Agent,
			Channel:      req.Channel,
			UserID:       req.UserID,
			CallerID:     req.Principal.CallerID,
			CreatedAt:    now,
			LastActiveAt: now,
		}); err != nil {
			return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error(), ConfigHash: snap.Hash}, nil
		}
	}
	if err := sess.Load(ctx); err != nil {
		return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error(), ConfigHash: snap.Hash}, nil
	}

	// ④ 组装运行环境：ToolSet 取**快照冻结的** Profile（中途改盘不换配置），
	// 预算按"请求值与 Profile 收紧取小"合并；模型客户端经显式映射表的查表结果注入；
	// 记忆**在运行开始时读一次**、写进运行环境后冻结（运行内不重读——中途改盘不换记忆）。
	// 资源清单经 RunEnv 显式携带（约束 46：不进 context）；未接线时为 nil。
	var runResources *RunResourceManifest
	if s.opts.Resources != nil {
		runResources = &manifest
	}
	// 通知授权随准入落定（R4.7）：接了资源解析端口时，模式与**具体渠道 UID** 来自准入
	// 枚举结果；没有端口（轻装配 / 直调）时退回 Profile 兼容读取（空 = 不限，工具按名判定）。
	// The notify grant is frozen at admission: concrete channel uids when resources are
	// wired, the profile compatibility read otherwise.
	notifyMode, notifyChannels := s.notifyGrant(resolved, snap)
	toolSet := snapshotTools(snap)
	if req.Flow != nil {
		toolSet = slices.DeleteFunc(slices.Clone(toolSet), func(name string) bool { return !slices.Contains(req.Flow.Authority.Tools, name) })
		if req.FixedTool != nil && !slices.Contains(toolSet, req.FixedTool.Name) {
			return RunResult{}, ErrFlowDispatch
		}
	}
	// 系统代投的 Run 不向模型提供 notify（R7）：工具名集里删掉它，模式固定 none。
	// 授权层仍按 none 兜底——即使工具因装配错误仍在池里，调用也会被拒。
	// A system-delivery run never sees the notify tool, and the grant is forced to none as
	// the authorization-layer backstop.
	if req.SuppressNotify {
		notifyMode, notifyChannels = NotifyModeNone, nil
		toolSet = withoutToolName(toolSet, "notify")
	}
	env := &RunEnv{
		phases:          s.opts.Phases,
		Req:             req,
		Flow:            req.Flow,
		flowWritesReady: s.opts.FlowWritesReady,
		AgentUID:        lifecycle.AgentUID,
		Snapshot:        snap,
		Resources:       runResources,
		Session:         sess,
		Budget:          mergeBudget(req.Budget, snap.Profile),
		ToolSet:         toolSet,
		llmClient:       llmClient,
		// 摘要客户端与对话客户端来自准入的**同一张固定映射表**（原则 III）：压缩接线
		// 只拿这一份，不按名字重新解析；未接资源时为零值，压缩装配据此拒绝。
		// The summary client comes from the same frozen map as the chat client.
		summaryClient: resolved.SummaryProvider,
		agentDir:      s.opts.Agents.Dir(req.Agent),
		skillDirs:     boundSkillDirs(resolved),
		// 模式与渠道集合随 Run 准入冻结，工具调用只读这一份副本（改文件不影响在途运行）。
		// The mode and channel set are frozen at admission; tool calls never re-read the file.
		notifyMode:     notifyMode,
		notifyChannels: notifyChannels,
	}
	// 工具实例在准入时钉住（14D / C4）：此后对注册池的受控重建换不到本次运行手里这一组；
	// 旧连接进入排空态，切换之后的新调用按"连接已重建"明确失败，不静默挂起、也不偷转。
	// Tool instances are pinned at admission: a later rebuild cannot retarget this run,
	// and calls on a draining link fail explicitly instead of being rerouted.
	if s.opts.ToolPins != nil {
		env.pinnedTools, env.pinnedSpecs = s.opts.ToolPins.Pin(toolSet)
	}
	if req.Flow != nil && req.FixedTool == nil {
		filter := req.Flow.Tools.(FlowReadOnlyToolFilter) // Checked before any side effect.
		allowed, err := filter.ReadOnlyTools(ctx, env.pinnedTools, toolSet)
		if err != nil {
			return RunResult{}, err
		}
		for _, name := range allowed {
			if !slices.Contains(toolSet, name) || env.pinnedTools[name] == nil {
				return RunResult{}, ErrFlowDispatch
			}
		}
		env.ToolSet = allowed
		env.pinnedSpecs = slices.DeleteFunc(env.pinnedSpecs, func(spec ToolSpec) bool { return !slices.Contains(allowed, spec.Name) })
	}
	if req.FixedTool == nil && s.opts.Memory == nil && snap.Profile.Settings.IncludeGlobalMemory != nil {
		return RunResult{RunID: runID, Status: StatusFailed, Reason: "记忆分层读取未装配", ConfigHash: snap.Hash}, nil
	}
	if req.FixedTool == nil && s.opts.Memory != nil {
		var text string
		var err error
		if snap.Profile.Settings.IncludeGlobalMemory != nil {
			layered, ok := s.opts.Memory.(LayeredMemoryReader)
			if !ok {
				return RunResult{RunID: runID, Status: StatusFailed, Reason: "记忆分层读取未装配", ConfigHash: snap.Hash}, nil
			}
			text, err = layered.LoadLayers(ctx, req.Agent, req.Principal, *snap.Profile.Settings.IncludeGlobalMemory)
		} else {
			text, err = s.opts.Memory.LoadEffective(ctx, req.Principal, req.Agent)
		}
		if err != nil {
			// 读取失败 = 拒绝启动（与段一读取失败同口径：零 LLM 调用、零工具执行），
			// 形状沿用既有 StatusFailed——不另立一套失败形态。
			return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error(), ConfigHash: snap.Hash}, nil
		}
		env.memory = text
	}

	// ⑤″ Run 级任务次数（N-01）：预留与结算一起做，且都在**业务执行之前**——账本行、
	// 用户消息与循环都是可审计的执行痕迹，被任务配额拒绝的 Run 一条都不该留下。
	// 拒绝经函数级 error 上抛（与 [3] 并发名额 / [4] 会话门禁同一形态），web 入口据
	// 哨兵映射 429 `QUOTA_EXHAUSTED`。
	// 计时点选在会话 / 快照落库之后：那些步骤失败时 Run 本就没执行，不该先扣任务；
	// 选在 StartRun 之前：任务次数是准入成本，不是运行结果——先跑后扣会给并发窗口
	// 留出超额放行的口子（与"零值即不限量"同一个失败方向）。
	// 记下的预留标识写进账本行，与 `quota_reservations` 对得上（契约列
	// `runs.reservation_id`）；适配层没预留时（内置管理员不限量）为空。
	// The task charge is admission cost, not an execution result: it is reserved and
	// settled before the ledger row, and a refusal leaves no trace at all.
	if s.opts.RunQuota != nil {
		charged, qerr := s.opts.RunQuota.ChargeRun(ctx, RunQuotaRequest{
			Principal: req.Principal,
			RunID:     runID,
		})
		if qerr != nil {
			return RunResult{}, qerr
		}
		req.ReservationID = charged.ReservationID
	}

	// ⑤ 预写账本行（status=running）+ 资源清单 + 引用，同一事务（约束 18）：**写失败 →
	// 直接 error**——账本不落下就不开跑。未接线时清单为零值，仓储只写运行行（读作"未记录"）。
	if s.opts.Runs != nil {
		if err := s.opts.Runs.StartRun(ctx, RunRecord{
			RunID:         runID,
			RequestID:     req.RequestID,
			SessionID:     req.SessionID,
			Agent:         req.Agent,
			Channel:       req.Channel,
			UserID:        req.UserID,
			Principal:     req.Principal,
			ReservationID: req.ReservationID,
			Trigger:       req.Trigger,
			ConfigHash:    snap.Hash,
			StartedAt:     now,
			Flow:          req.Flow,
		}, manifest); err != nil {
			return RunResult{}, fmt.Errorf("start run ledger row: %w", err)
		}
	}

	// 准入读锁租约到此为止：账本行、资源清单与引用已在同一事务落库，发布不再能插进
	// "版本已固定、引用未落库"的窗口。运行本身继续持有固定的运行时视图直到终态。
	// The admission lease ends here: the ledger row and resource refs are committed, so a
	// publish can no longer interleave; the pinned runtime lives until the run finishes.
	if resolved.Runtime != nil {
		resolved.Runtime.ReleaseAdmission()
	}

	// ⑥ 用户输入写进消息仓储：**宪法 II 规定的唯一写入点**。写失败 = fail-closed →
	// Run failed（先把账本收口，再如实返回失败）。
	user := Message{Role: RoleUser, Content: req.Message}
	if req.Flow != nil {
		user.EvidenceRefs = slices.Clone(req.Flow.InputEvidenceRefs)
	}
	if err := sess.AppendMessage(ctx, runID, user); err != nil {
		return s.finish(ctx, runID, env, RunResult{
			Status: StatusFailed, Reason: err.Error(), ConfigHash: snap.Hash,
		}), nil
	}

	// ⑥½ 知识注入（V-6）：检索发生在账本行与用户消息之后——装配记录与诊断事件都拿
	// `run_id` 作关联主锚点；查询词就是本 Run 的用户消息。注入面**运行级冻结**（每轮
	// 组装不重读载体），但权限不冻结：循环在每轮组装前复核片段（见 `KnowledgeRunSource`）。
	// 错误分流与记忆读取同口径（上文 ④）：**存储违约 → 拒绝启动**（零模型调用、零工具
	// 执行）；无权限 / 无声明集合是合法状态，由适配器收成"空注入 + nil error"。
	// The injection is retrieved after the ledger row and user message so records can anchor
	// on the run id. Storage failures refuse the run before any model call; "no permission /
	// nothing declared" is a legal empty injection, not an error.
	// Flow 的根范围是所有节点的上限；空声明叶子不应把兄弟节点的集合当检索请求。
	// The root ceiling is not a retrieval request for a leaf with no declared bindings.
	flowHasKnowledge := req.Flow == nil || len(snap.Profile.Knowledge) > 0
	if req.FixedTool == nil && s.opts.Knowledge != nil && flowHasKnowledge {
		env.knowledgeSource = s.opts.Knowledge
		finishKnowledge := BeginPhase(runCtx, env.phases, PhaseKnowledge, "", runID, "", 0)
		injection, kerr := s.opts.Knowledge.RetrieveForRun(runCtx, KnowledgeRunRequest{
			Flow:      req.Flow,
			Principal: req.Principal,
			RunID:     runID,
			Agent:     req.Agent,
			Query:     req.Message,
			Budget:    KnowledgeBudget(env.Budget.ContextTokens),
		})
		finishKnowledge(kerr)
		if kerr != nil {
			// 取消优先：检索横跨一次外部调用（查询向量化）与两次 SQL，期间的取消 /
			// 截止不是"知识库故障"——按循环的同一口径记 cancelled，不写成 failed。
			// Cancellation wins over failure: an aborted retrieval is a cancelled run, not
			// a knowledge failure.
			if ctxErr := runCtx.Err(); ctxErr != nil {
				return s.finish(ctx, runID, env, env.cancelled(ctxErr, 0, 0)), nil
			}
			return s.finish(ctx, runID, env, RunResult{
				Status: StatusFailed, Reason: kerr.Error(), ConfigHash: snap.Hash,
			}), nil
		}
		env.knowledge = injection
	}

	// ⑦ 循环。`error` **不全是**函数级错误：运行期失败与取消仍归 `Status`（契约 §2），
	// 但**准入期拒绝**——配额预留被拒（图 6 [5]）——由循环原样上抛，这里先收口账本再走
	// 错误通道返回零值 `RunResult`（`run-and-errors.md` §2 / §4 对两类拒绝的形态）。
	// 循环拿 `runCtx`：取消信号从这里进入传播链（轮首 / 调用后 / 每次工具前 / 组返回后）。
	// The loop's error is normally nil; an admission-class refusal (stage [5]) is the
	// exception and travels up after the ledger is closed as failed.
	var res RunResult
	var loopErr error
	if req.FixedTool != nil {
		res, loopErr = s.opts.Loop.runFixedTool(runCtx, runID, env, *req.FixedTool)
	} else {
		res, loopErr = s.opts.Loop.run(runCtx, runID, env)
	}
	if loopErr != nil {
		// 账本已经写了 running 行（⑤）：**不得**留成 running 等恢复扫描误判，
		// 先落 failed 终态，再按错误通道返回。
		s.finish(ctx, runID, env, RunResult{Status: StatusFailed, Reason: loopErr.Error()})
		return RunResult{}, loopErr
	}
	res.RunID = runID // RunEnv 的字段字不含 RunID（契约 §1.1），由运行入口填

	// ⑦½ 引用复核（V-6）：答案产出后拿**本次实际进入上下文的片段**（循环收窄后的最终
	// 注入面，不是重新检索的结果）与答案里的引用标记对账。计数只进诊断事件与日志（不含
	// 正文）；复核本身失败是诊断面故障，**不得**把一次已经成功的运行改判失败。
	// Citation review runs against the chunks that actually entered the final context; a
	// review failure never turns a successful run into a failed one.
	if res.Status == StatusCompleted && s.opts.Knowledge != nil && !env.knowledge.Empty() {
		if _, aerr := s.opts.Knowledge.ResolveAnswer(ctx, KnowledgeAnswerRequest{
			Flow:      req.Flow,
			Principal: req.Principal,
			RunID:     runID,
			Answer:    res.FinalText,
			Chunks:    env.knowledge.Chunks,
		}); aerr != nil {
			s.opts.Logger.Warn("引用复核未完成——答案照常返回",
				"run_id", runID, "op", "citation_resolve", "error", aerr)
		}
	}

	// ⑧⑨ 终态落库后返回——落库失败**不覆盖**已得结果（记 ERROR，行留 running 待恢复扫描收尾）。
	return s.finish(ctx, runID, env, res), nil
}

// finish 写终态账本行、按条件触发唯一的同步学习钩子，并释放运行引用；返回结果。
// 三段顺序固定（C-05）：**终态落库 → 抽取钩子 → 引用释放**。写失败只记 ERROR：
// 结果照常返回——修复历史不能以扣下已得结果为代价。释放**无条件**执行（钩子 panic、
// 超时、跳过都不影响），失败只记 ERROR 并留给启动恢复兜底。
// finish writes the terminal ledger state, triggers the one synchronous learning hook
// under its conditions, and releases the run's references — in that fixed order. The
// release is unconditional; neither the hook nor the release can change the result.
func (s *AgentService) finish(ctx context.Context, runID string, env *RunEnv, res RunResult) RunResult {
	res.RunID = runID
	if res.ConfigHash == "" {
		res.ConfigHash = env.snapshotHash()
	}
	// 清单哈希与账本同源：入参未填时取准入时冻结的清单（契约 §1.1 的 `ManifestHash`）。
	// 未接线的装配路径 env.Resources 为空指针——保持空串，读作"未记录"，**不补造**。
	// The manifest hash mirrors the ledger: fall back to the admission manifest, never
	// fabricate one for unwired paths.
	if res.ManifestHash == "" && env != nil && env.Resources != nil {
		res.ManifestHash = env.Resources.Hash
	}
	if s.opts.Runs == nil {
		return res
	}

	// 终态写回用独立 5 秒 + WithoutCancel：调用方取消不得让终态落不进去（既有
	// Flow 取消路径的同一纪律，现在统一适用）。`applied` 是本次是否真正完成转换。
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	applied, err := s.opts.Runs.FinishRun(finishCtx, runID, res.Status, res.Reason,
		res.Iterations, res.ToolCalls, s.opts.Now())
	if err != nil {
		s.opts.Logger.Error("运行终态落库失败",
			"run_id", runID, "store", "runs", "op", "finish")
	}

	// 释放无条件执行：钩子 panic、超时、跳过都不影响；失败由启动恢复补齐。
	// 注册在钩子之前为的是让它在钩子返回之后才跑——顺序是终态 → 抽取 → 释放。
	// The release is registered before the hook so it runs after the hook returns.
	defer func() {
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelRelease()
		if rerr := s.opts.Runs.ReleaseRunRefs(releaseCtx, runID, s.opts.Now()); rerr != nil {
			s.opts.Logger.Error("运行引用释放失败——留待启动恢复",
				"run_id", runID, "store", "runs", "op", "release")
		}
	}()

	// 抽取只在**本次调用确实完成终态转换**时发生，且只对正常完成的普通交互运行。
	// `FinishRun` 写失败或命中 0 行幂等分支都跳过（C-05）；Flow / 固定工具走各自的
	// 受控执行路径，调度器渠道没有"用户陈述"可抽取。
	// Extraction happens only after this call really applied the terminal state, and only
	// for ordinary completed web/cli runs; every other path returns untouched.
	if !applied || res.Status != StatusCompleted || s.opts.Learning == nil ||
		env == nil || env.Req.Flow != nil || env.Req.FixedTool != nil {
		return res
	}
	if env.Req.Channel != "web" && env.Req.Channel != "cli" {
		return res
	}
	// 类型断言取本次 Run 准入时冻结的客户端；断言失败 → nil，抽取服务据此放弃，
	// 不退回装配期全局客户端（原则 III）。生产链路的 guarded / diagnostic 装饰器
	// 都必须实现 ChatMemoryExtract，否则此处永远失败。
	// The extractor is the client frozen at admission; a failed assertion yields nil and
	// the service gives up rather than reaching for a global client.
	extractor, _ := env.ProviderClient().(MemoryExtractor)
	s.callLearningHook(ctx, LearningRunInput{
		RunID:       res.RunID,
		Agent:       env.agentName(),
		SessionID:   env.Req.SessionID,
		Channel:     env.Req.Channel,
		Provider:    env.providerRef().Name,
		Model:       env.ProviderModel(),
		ConfigHash:  res.ConfigHash,
		CompletedAt: s.opts.Now(),
		Extractor:   extractor,
	}, res)
	return res
}

// callLearningHook 调用唯一的同步抽取钩子，并在最外层兜底 recover：生产实现
// （memory.LearningService）内部已有一层 recover，这一层保证「钩子自身有缺陷」也不可能把
// panic 穿过收尾路径拖垮请求的返回；已登记的释放 defer 在恢复后照常执行（C-27 / C-32）。
// callLearningHook invokes the one synchronous extraction hook behind an outer recover:
// the production hook already recovers internally; this layer guarantees a defective hook
// still cannot take down the request, and the release defer still runs afterwards.
func (s *AgentService) callLearningHook(ctx context.Context, in LearningRunInput, res RunResult) {
	defer func() {
		if r := recover(); r != nil {
			// 只记 panic 类型：panic 值可能携带消息正文或凭证，日志不得落内容（C-33）。
			// Log the type only: a panic value may carry content or credentials.
			s.opts.Logger.Error("学习抽取钩子 panic——已恢复，Run 结果不变",
				"run_id", in.RunID, "panic_type", fmt.Sprintf("%T", r))
		}
	}()
	s.opts.Learning.AfterRun(ctx, in, res)
}

// Cancel 取消一次在途运行——**只发信号，不写终态**（约束 20 / 图 G）。
// Cancel cancels an in-flight run: it only signals, never writes a terminal state.
//
// 三步。① 在途登记表查 `run_id`：命中即取消循环的 ctx，并至多等 `cancelWait`（30 秒）
// 让循环自己收场。② 无论等到与否都读库取**当前**状态返回——超时可能仍是 running，由
// 调用方轮询；调用方自己取消上下文时同样停止等待（不算降级）。③ 未命中登记表时读库：
// 无行 → `ErrRunNotFound`（HTTP 404）；已是终态 → 原样返回（重复取消因此幂等）；仍写作
// running → 单进程下只可能是恢复未完或缺陷，如实返回当前状态并记 ERROR，**不假装取消成功**
// （注 15）。等待上限授权的是"等"，不是"写"：本方法没有任何 FinishRun 调用。
func (s *AgentService) Cancel(ctx context.Context, runID string) (RunDetail, error) {
	s.mu.Lock()
	run := s.active[runID]
	s.mu.Unlock()

	if run != nil {
		run.cancel()
		switch s.waitActive(ctx, run) {
		case waitTimedOut:
			s.opts.Logger.Warn("取消信号已发出，运行在等待上限内尚未结束",
				"run_id", runID, "wait", s.cancelWait, "op", "cancel")
		case waitAborted:
			s.opts.Logger.Debug("调用方中止取消等待，运行尚未结束", "run_id", runID, "op", "cancel")
		}
	}

	detail, err := s.readRunDetail(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	// 库说 running、内存没有登记：取消信号无处可送。如实返回当前状态并留 ERROR 供排障，
	// 不把"没送到"伪装成"已取消"（注 15）。
	if run == nil && detail.Status == StatusRunning {
		s.opts.Logger.Error("运行不在本进程，取消信号未送达", "run_id", runID, "op", "cancel")
	}
	return detail, nil
}

// waitActive 等待循环收场，至多 `cancelWait`；调用方中止与超时分开表达（只有后者记 WARN）。
// waitActive waits up to cancelWait for the loop to finish; a caller abort is reported
// separately from a timeout (only the latter logs WARN).
func (s *AgentService) waitActive(ctx context.Context, run *activeRun) waitOutcome {
	timer := time.NewTimer(s.cancelWait)
	defer timer.Stop()
	select {
	case <-run.done:
		return waitFinished
	case <-ctx.Done():
		return waitAborted
	case <-timer.C:
		return waitTimedOut
	}
}

// readRunDetail 读运行当前状态。等待可能已吃掉调用方的期限，故读用**不随调用方取消**的
// 派生上下文与独立上限——取消路径必须报得出"现在是什么状态"；读失败才如实上抛。
// readRunDetail reads the current state under a context detached from the caller's
// cancellation: the bounded wait may have consumed its deadline, yet Cancel still has to
// report what state the run is in.
func (s *AgentService) readRunDetail(ctx context.Context, runID string) (RunDetail, error) {
	if s.opts.Runs == nil {
		// 装配缺陷（账本没接上）：如实报错，不猜成"不存在"（那会把缺陷伪装成 404）。
		return RunDetail{}, errors.New("agent service: run store is not configured")
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelReadTimeout)
	defer cancel()
	return s.opts.Runs.GetRun(readCtx, runID)
}

// registerActive 登记一次在途运行，返回登记项（摘除时用它判断"还是不是这一项"）。
// registerActive registers one in-flight run and returns the entry.
func (s *AgentService) registerActive(runID string, cancel context.CancelFunc) *activeRun {
	run := &activeRun{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.active[runID] = run
	s.mu.Unlock()
	return run
}

// unregisterActive 摘除登记并关 `done`。**只摘自己那一项**：同一 run_id 被并发复用时
// （调用方问题），先返回的那次不得摘掉后来者的登记；`done` 各关各的，不重复关。
// unregisterActive removes only its own entry and closes its own done channel, so a
// reused run id can never have a later registration removed by an earlier return.
func (s *AgentService) unregisterActive(runID string, run *activeRun) {
	s.mu.Lock()
	if s.active[runID] == run {
		delete(s.active, runID)
	}
	s.mu.Unlock()
	close(run.done)
}

// 生命周期拒绝的原因分类（自产、面向用户）：区分四类状态，并给修复入口；**不得**拼接
// 底层错误原文（可能含绝对路径）。
// Self-authored lifecycle refusal reasons: four categories with a repair hint, never
// echoing the underlying error text (which may carry absolute paths).
const (
	agentUnavailableMissing  = "lifecycle state file missing; restore it before running"
	agentUnavailableCorrupt  = "lifecycle state file corrupt; restore it from a trusted copy"
	agentUnavailableDisabled = "lifecycle state disabled; enable the agent first"
	agentUnavailableArchived = "lifecycle state archived"
)

// admitLifecycle 在图 F ③ 的读锁段内重读生命周期状态文件：只有 active 放行；缺失 / 损坏
// 同时记 ERROR（审计与日志双轨——卡片约束 15）。`Lifecycle == nil` 表示尚未接线（仅
// 既有零值单测），生产装配缺它等于没有门禁，故 wire.go 必须单点传入。
// admitLifecycle re-reads the lifecycle state file inside the read-lock segment of flow F.
// Missing and corrupt states also log at ERROR level; a nil port means "not wired yet"
// and is reserved for zero-value tests.
//
// 返回值是**本次准入读到的**状态记录：资源清单的 `agent_lifecycle_state` 必须取同一份
// 读数（不得从注册表或库里另取一份，那会让门禁与清单指向两个时刻）。
// The record returned is the very one the gate read, so the manifest's lifecycle state
// can never come from a different snapshot than the one that admitted the run.
func (s *AgentService) admitLifecycle(ctx context.Context, agent string) (LifecycleRecord, error) {
	if s.opts.Lifecycle == nil {
		return LifecycleRecord{}, nil
	}
	if s.opts.Gate != nil {
		unlock := s.opts.Gate.RLock(agent)
		defer unlock()
	}

	rec, exists, err := s.opts.Lifecycle.Read(ctx, agent)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		s.opts.Logger.Error("生命周期状态文件缺失，拒绝新运行", "agent", agent, "op", "admit")
		return LifecycleRecord{}, &AgentUnavailableError{Name: agent, Kind: AgentUnavailableMissing, Reason: agentUnavailableMissing, Cause: err}
	case err != nil:
		s.opts.Logger.Error("生命周期状态文件损坏，拒绝新运行", "agent", agent, "op", "admit")
		return LifecycleRecord{}, &AgentUnavailableError{Name: agent, Kind: AgentUnavailableCorrupt, Reason: agentUnavailableCorrupt, Cause: err}
	case !exists:
		s.opts.Logger.Error("生命周期状态文件缺失，拒绝新运行", "agent", agent, "op", "admit")
		return LifecycleRecord{}, &AgentUnavailableError{Name: agent, Kind: AgentUnavailableMissing, Reason: agentUnavailableMissing}
	case rec.State != AgentActive:
		return LifecycleRecord{}, &AgentUnavailableError{Name: agent, State: rec.State, Kind: lifecycleKind(rec.State), Reason: lifecycleReason(rec.State)}
	}
	return rec, nil
}

// lookupAgent 在 AgentSource 上按名取条目；端口为 nil 时按"不存在"处理（装配缺失不得
// 演变成 panic，也不得猜成可用）。
// lookupAgent returns the registered entry, treating a nil port as "not found".
func (s *AgentService) lookupAgent(name string) (AgentEntry, bool) {
	if s.opts.Agents == nil {
		return AgentEntry{}, false
	}
	return s.opts.Agents.Lookup(name)
}

// buildRunSnapshot 按装配选择快照来源：资源接线后走发布版本构建（技能卡片与正文从准入
// 固定的不可变版本目录读）；未接线时沿用工作副本快照。两条来源共用 core 的同一段
// 排序 / 序列化 / 哈希代码。
// buildRunSnapshot picks the snapshot source: the published-version builder once resources
// are wired, the workspace copy otherwise.
func (s *AgentService) buildRunSnapshot(ctx context.Context, entry AgentEntry, resolved ResolvedResources, now time.Time) (*ConfigSnapshot, error) {
	if s.opts.RunSnapshots != nil {
		snap, err := s.opts.RunSnapshots.Build(ctx, entry, resolved)
		if err != nil {
			return nil, err
		}
		return &snap, nil
	}
	return s.opts.Agents.Snapshot(ctx, entry, now)
}

// pinnedClient 取本次运行固定的模型调用入口（人工 review ② 缺陷 2）：资源接线后只认准入
// 时固定的映射表（宪法 III），返回的视图仍带两阶段审计（宪法 V）——**不得**退回装配期的
// 全局客户端，也**不得**绕过审计直接用协议客户端。缺条目 / 缺运行时一律失败关闭。
// pinnedClient returns the client fixed at admission; it still audits every call and never
// falls back to the assembly-time global client or a raw protocol client.
func pinnedClient(resolved ResolvedResources) (ProviderClient, error) {
	if resolved.Runtime == nil {
		return nil, errors.New("准入未返回运行时视图")
	}
	client, ok := resolved.Runtime.Provider(resolved.Provider.Name)
	if !ok {
		return nil, fmt.Errorf("准入固定的映射表中没有 provider %s", resolved.Provider.Name)
	}
	return client, nil
}

// pinnedClientWhenNeeded selects the client before any model-dependent work.
// The FixedTool design probe uses false: resource/Agent pins still belong to Run
// admission, while absence of a model mapping must not invent a Chat dependency.
func pinnedClientWhenNeeded(resolved ResolvedResources, modelRequired bool) (ProviderClient, error) {
	if !modelRequired {
		return nil, nil
	}
	return pinnedClient(resolved)
}

// boundSkillDirs 把准入固定的技能目录映射拍成**有序**切片（人工 review ② F1）：文件工具
// 只放行这些发布版本目录。Go 的 map 遍历顺序随机，不排序就会让同一次运行的工具环境在
// 不同进程里读数不同；排序后 ToolEnv.SkillDirs 是可复核的稳定输入。
// boundSkillDirs flattens the admitted skill directories into a sorted slice so the tool
// environment is deterministic across processes.
func boundSkillDirs(resolved ResolvedResources) []string {
	if len(resolved.SkillDirs) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(resolved.SkillDirs))
	for _, dir := range resolved.SkillDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// resourceUnavailable 把资源解析 / 清单构造的失败统一为 500 INTERNAL（失败关闭）：已是
// *ResourceUnavailableError 的原样上抛（不二次包装），其余补上自产分类与修复入口。失败
// 原因只进日志（审计与日志双轨），不进响应文案——底层错误可能带绝对路径。
// resourceUnavailable maps resource failures to 500 INTERNAL, keeping an existing
// ResourceUnavailableError as-is and logging the cause for operators.
func (s *AgentService) resourceUnavailable(agent string, cause error) error {
	s.opts.Logger.Error("运行资源不可用，拒绝新运行", "agent", agent, "op", "admit", "error", cause)
	var unavailable *ResourceUnavailableError
	if errors.As(cause, &unavailable) {
		return unavailable
	}
	return &ResourceUnavailableError{
		Name:   agent,
		Reason: "published resource version unavailable; re-run the baseline or republish",
		Cause:  cause,
	}
}

// lifecycleKind 把非 active 状态映射为机器可判分类（界面文案由 web 层按它出中文）。
// lifecycleKind maps a non-active state to the machine-judgeable category.
func lifecycleKind(state AgentState) AgentUnavailableKind {
	switch state {
	case AgentDisabled:
		return AgentUnavailableDisabled
	case AgentArchived:
		return AgentUnavailableArchived
	default:
		return AgentUnavailableOther
	}
}

// lifecycleReason 把非 active 状态映射为带修复入口的固定文案。
// lifecycleReason maps a non-active state to its fixed refusal text.
func lifecycleReason(state AgentState) string {
	switch state {
	case AgentDisabled:
		return agentUnavailableDisabled
	case AgentArchived:
		return agentUnavailableArchived
	default:
		return "lifecycle state " + string(state)
	}
}

// admit 完成准入的四步记账（**全部在同一把 `s.mu` 临界区内**）：忙集 → 实例满 →
// 会话状态（归档 / 缺失）→ 登记忙集。返回的释放函数**幂等**（defer 与错误路径都可能调）。
// 反了顺序就会漏记：先占会话再判实例，实例满时那次占用再也释放不掉。
// 无会话请求（SessionID == ""，无状态 invoke）只走实例上限一步——不得入忙集，否则
// 两条并发 invoke 会因空会话互相 409。
// admit performs the four admission steps inside one critical section: busy set, instance
// cap, session status, then registration. Stateless requests only face the instance cap.
func (s *AgentService) admit(ctx context.Context, req RunRequest) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionID := req.SessionID
	if sessionID != "" {
		if _, busy := s.busy[sessionID]; busy {
			return nil, &SessionBusyError{SessionID: sessionID}
		}
	}
	if s.running >= s.opts.MaxConcurrent {
		return nil, &InstanceFullError{Limit: s.opts.MaxConcurrent}
	}
	if req.Flow != nil {
		reserved := 1
		if s.opts.FlowInteractiveReservedRuns != nil {
			reserved = *s.opts.FlowInteractiveReservedRuns
		}
		if reserved < 0 || reserved >= s.opts.MaxConcurrent || s.flowRunning >= s.opts.MaxConcurrent-reserved {
			return nil, &InstanceFullError{Limit: s.opts.MaxConcurrent}
		}
	}
	// 会话状态门禁：只有"要复用某个会话"的请求才查；DB 读在锁内（单次主键查询，
	// 这把锁只保护准入与归档，不覆盖整个运行——已知取舍）。
	// 未命中：要求既有 → 调用方错误；否则放行，后续 UpsertSession 自动创建。
	// 命中且已归档 → 一律拒绝（不论 RequireExistingSession）——cron / 手动触发也挡在这里。
	// 其它读错误原样上抛（fail-closed，不把基础设施故障当成"会话不存在"）。
	if sessionID != "" && s.opts.Sessions != nil {
		rec, err := s.opts.Sessions.GetSession(ctx, sessionID)
		switch {
		case err == nil:
			if rec.Status == "archived" {
				return nil, &SessionArchivedError{SessionID: sessionID}
			}
		case errors.Is(err, fs.ErrNotExist):
			if req.RequireExistingSession {
				return nil, &SessionNotFoundError{SessionID: sessionID}
			}
		default:
			return nil, err
		}
	}
	if sessionID != "" {
		s.busy[sessionID] = struct{}{}
	}
	s.running++
	if req.Flow != nil {
		s.flowRunning++
	}

	released := false
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if released {
			return
		}
		released = true
		if sessionID != "" {
			delete(s.busy, sessionID)
		}
		s.running--
		if req.Flow != nil {
			s.flowRunning--
		}
	}, nil
}

// Archive 归档一个会话：与在途运行互斥（同一把锁保护准入与归档），重复归档幂等。
// 空会话 → *SessionNotFoundError（无可归档）；nil 门禁 → *SessionGateUnavailableError
// （装配缺陷，映射 500——不 nil 转发、不静默成功）；仓储未命中 → *SessionNotFoundError，
// 其余错误原样上抛。
// Archive archives a session, mutually exclusive with in-flight runs; archiving twice is
// idempotent. A nil gate is an assembly defect and never a silent success.
func (s *AgentService) Archive(ctx context.Context, sessionID string) error {
	if s.opts.Sessions == nil {
		return &SessionGateUnavailableError{}
	}
	if sessionID == "" {
		return &SessionNotFoundError{SessionID: sessionID}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.busy[sessionID]; busy {
		return &SessionBusyError{SessionID: sessionID}
	}
	if err := s.opts.Sessions.ArchiveSession(ctx, sessionID, s.opts.Now()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &SessionNotFoundError{SessionID: sessionID}
		}
		return err
	}
	return nil
}

// TrySessionMaintenance excludes admission and archiving for one idle session.
// Maintenance does not consume a run concurrency slot. Release is idempotent.
func (s *AgentService) TrySessionMaintenance(sessionID string) (func(), bool) {
	if sessionID == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.busy[sessionID]; busy {
		return nil, false
	}
	s.busy[sessionID] = struct{}{}
	released := false
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !released {
			delete(s.busy, sessionID)
			released = true
		}
	}, true
}

// mergeBudget 合并实际预算：请求值未填的项补契约 §6 缺省，再让 Profile **只能收紧**迭代上限。
//
// 装载层保留了 `settings.max_iterations` 的**显式零值**（US-3 注 9），故这里**不得**把 0
// 当成"未声明"——它是一句"该 Agent 不许调用模型"的明确配置，收紧到 0 正是它的语义。
// mergeBudget fills the documented defaults and lets the profile tighten the iteration cap
// only. An explicit zero survives the loader on purpose and is honoured here.
func mergeBudget(req Budget, profile *Profile) Budget {
	if req.MaxIterations <= 0 {
		req.MaxIterations = DefaultMaxIterations
	}
	if req.MaxToolCalls <= 0 {
		req.MaxToolCalls = defaultMaxToolCalls
	}
	if req.ToolTimeout <= 0 {
		req.ToolTimeout = defaultToolTimeout
	}
	if req.MaxOutputBytes <= 0 {
		req.MaxOutputBytes = defaultMaxOutputBytes
	}
	if req.ContextTokens <= 0 {
		req.ContextTokens = defaultContextTokens
	}
	if profile != nil && profile.Settings.MaxIterations < req.MaxIterations {
		req.MaxIterations = profile.Settings.MaxIterations
	}
	return req
}

// notifyGrant 摊平本次 Run 的通知授权（R4.7）：接了资源解析端口时取准入枚举的**具体
// 渠道 UID**（`all_eligible` 已枚举共享 ∪ 本人专属、`allowlist` 只取声明 UID，`none`
// 为空），顺序沿用清单的 UID 升序；没有端口时退回 Profile 兼容读取（US-8 口径）。
// 冻结之后工具面只认这份副本——空集合在 Run 内不等于「不限制」。
// notifyGrant flattens the admitted notification grant into concrete channel uids; without
// the resources port it falls back to the profile compatibility read.
func (s *AgentService) notifyGrant(resolved ResolvedResources, snap *ConfigSnapshot) (NotifyMode, []string) {
	if s.opts.Resources != nil {
		uids := make([]string, 0, len(resolved.Notify.Channels))
		for _, ref := range resolved.Notify.Channels {
			uids = append(uids, string(ref.ResourceUID))
		}
		return resolved.Notify.Mode, uids
	}
	return EffectiveNotifyMode(snap.Profile.NotifyMode, snap.Profile.NotifyChannels),
		slices.Clone(snap.Profile.NotifyChannels)
}

// snapshotTools 取快照冻结的工具名集；Profile 缺失（未注册的 Agent 不会有快照）时为空池。
// **空池 ≠ 全部可用**：不声明就是没有工具（陷阱 #9）。
// snapshotTools returns the frozen tool-name set; an absent profile means an empty pool,
// which never means "everything is allowed".
func snapshotTools(snap *ConfigSnapshot) []string {
	if snap == nil || snap.Profile == nil {
		return nil
	}
	return snap.Profile.Tools
}

// withoutToolName 返回去掉指定工具名的副本（不修改快照切片——快照被多个 Run 共享）。
// withoutToolName returns a copy without the named tool; the snapshot slice is shared and
// must never be mutated.
func withoutToolName(tools []string, name string) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool == name {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// runSuffix 生成 RunID 的随机段：**6 位 base36 小写**（§6 固定口径）。
// 随机源取不到时不回落时间 / 计数器（那会退化成可预测的 id）：以全零段收场，唯一性仍由
// 时间戳段保证，而这次故障会由启动期的完整性检查暴露。
// runSuffix returns the six-character base36 suffix of a run id; a missing entropy source
// never degrades into a predictable counter.
func runSuffix() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	// 252 = 36×7：把 ≥252 的字节剔除，取模偏差即归零（256 = 36×7 + 4）。
	const cutoff = 252
	var (
		out [6]byte
		buf [1]byte
	)
	for i := 0; i < len(out); {
		if _, err := rand.Read(buf[:]); err != nil {
			return "000000"
		}
		if int(buf[0]) >= cutoff {
			continue
		}
		out[i] = alphabet[int(buf[0])%36]
		i++
	}
	return string(out[:])
}
