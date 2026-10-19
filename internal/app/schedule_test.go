package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// 本文件是 US-10 调度装配面的验收（颗粒度文档 §3.2 的 app/schedule_test.go；首批 7 个
// 函数 / 14 例 + T023 追加的 2 个释放 / 信号接缝用例）：
//
//	TestNeedsSchedules                     —— 只有 serve / gateway 启动调度器（约束 20）
//	TestNewScheduler_Options               —— entries 快照 / budget / 300s·70s 常量
//	TestRun_ScheduleStartModes             —— 启动接缝的模式判定（N-03）
//	TestRun_ScheduleStartFailureExits      —— Start 失败 fail closed（注 16）
//	TestScheduleStartExitCode_SignalCanceledReturnsZero —— 信号取消返回 0（注 15）
//	TestScheduleStartExitCode_FailureReturnsOne         —— 其余失败返回 1
//	TestNewRuntime_ServiceHasSessionGate   —— 生产 newRuntime 真装配 Sessions（约束 40 / R10-H1）
//	TestHeavyRuntime_CloseStopsSchedulerFirst           —— 释放顺序 scheduler → MCP → DB → 锁（约束 22）
//	TestApp_SignalContextUsesInjectedNotifier           —— 信号上下文只经 App.notifyContext 派生（约束 24 / R10-M10）
//	TestScheduleEditorAdapter_MapsRejectionToInvalidInput —— 拒绝 → invalid_input；IO 错误原样透传
//	TestScheduleEditorAdapter_ReconcilesAfterWrite      —— 写成功后 ReconcileAgent 落库
//	TestScheduleEditorAdapter_RefreshesClaimBaseline    —— 成功写入刷新私有 claim 基线（约束 30）
//	TestScheduleEditorAdapter_ConcurrentCrossAgentSameID —— 并发同名 id 恰一个成功（-race）
//	TestScheduleEditorAdapter_ConcurrentWithProfilesRead —— 与 /profiles 读面并发不回写 entries（P-05）
//	TestScheduleEditorAdapter_CrossOwnerConflict_NoWrite —— 库内归属冲突零写入（约束 42）
//
// 全部离线：装配面的其余部分（模型 / MCP）不参与本组用例。
// Every case stays offline; no real model or network call is involved.

// TestNeedsSchedules 覆盖约束 20：调度器只在 serve / gateway 启动。
// 逆改点：needsSchedules 恒真 → chat / tool 子例红；漏判 gateway → 对应子例红。
func TestNeedsSchedules(t *testing.T) {
	t.Run("serve", func(t *testing.T) {
		require.True(t, needsSchedules([]string{"serve"}))
	})
	t.Run("gateway", func(t *testing.T) {
		require.True(t, needsSchedules([]string{"gateway"}))
	})
	t.Run("chat", func(t *testing.T) {
		require.False(t, needsSchedules([]string{"chat", "demo"}))
	})
	t.Run("tool", func(t *testing.T) {
		require.False(t, needsSchedules([]string{"tool", "read_file"}))
	})
}

// TestNewScheduler_Options 校验 newScheduler 的三项装配契约：
// ① Entries 在构造时被拷成私有快照（此后调用方改 slice 不影响调度定义，P-05）；
// ② 运行预算按配置段映射（与 web / CLI 同一口径，约束 35）；
// ③ 运行超时 300s、停机宽限 70s（约束 23 / 24）。
//
// 逆改点：构造时保留 entries slice 引用 → ① 在构造后篡改 entries，Start 不再对账
// demo-1（断言红）；budget 不接 cfg → ② 收到全零预算（断言红）；超时常量改错 → ③ 红。
func TestNewScheduler_Options(t *testing.T) {
	t.Run("entries are snapshotted at construction", func(t *testing.T) {
		entries := []core.AgentEntry{{
			Name:       "demo",
			Registered: true,
			Profile: &core.Profile{Schedules: []core.Schedule{{
				ID: "demo-1", Cron: "*/5 * * * * *", Zone: "UTC", Message: "巡检",
			}}},
		}}
		sched, _, store := newScheduleTestScheduler(t, &cli.Config{}, entries)

		// 构造之后篡改调用方持有的 slice：快照必须不受影响（P-05）。
		entries[0].Registered = false
		entries[0].Profile.Schedules[0].Message = "被篡改"

		require.NoError(t, sched.Start(context.Background()))
		t.Cleanup(sched.Stop)

		// 行内容来自构造时的快照：若实现保留 entries 引用、Start 时才读定义，
		// 这里会读到"被篡改"或整只不注册（断言红）。
		rows, err := store.ListTasks(context.Background())
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "巡检", rows[0].Message, "定义取自构造时的深拷贝，不是调用方后续改写")
		require.Equal(t, "demo", rows[0].Agent)
	})

	t.Run("budget follows configuration", func(t *testing.T) {
		cfg := &cli.Config{Budget: cli.BudgetConfig{
			MaxIterations: 7, MaxToolCalls: 9, ContextTokens: 12345, MaxOutputBytes: 4096,
		}}
		entries := []core.AgentEntry{demoEntry("demo-1")}
		sched, invoker, _ := newScheduleTestScheduler(t, cfg, entries)

		require.NoError(t, sched.Start(context.Background()))
		t.Cleanup(sched.Stop)

		// 真实 cron 引擎按 `*/1` 每秒触发；等第一次运行落地即为装配值读数。
		req := invoker.awaitFirstRequest(t)
		require.Equal(t, core.Budget{
			MaxIterations:  7,
			MaxToolCalls:   9,
			ContextTokens:  12345,
			MaxOutputBytes: 4096,
		}, req.Budget, "定时运行与手动补跑共用配置预算（约束 35）")
		require.Equal(t, "scheduler", req.Channel)
		require.Equal(t, "scheduler", req.UserID)
	})

	t.Run("run timeout and stop grace literals", func(t *testing.T) {
		require.Equal(t, 300*time.Second, scheduleRunTimeout, "单次定时运行上限 300s")
		require.Equal(t, 70*time.Second, scheduleStopGrace, "停机宽限 70s")

		sched, invoker, _ := newScheduleTestScheduler(t, &cli.Config{}, []core.AgentEntry{demoEntry("demo-1")})
		require.NoError(t, sched.Start(context.Background()))
		t.Cleanup(sched.Stop)

		deadline := invoker.awaitFirstDeadline(t)
		require.False(t, deadline.IsZero(), "运行 ctx 必须带截止时间")
		require.WithinDuration(t, time.Now().Add(scheduleRunTimeout), deadline, 2*time.Second)
	})
}

// TestRun_ScheduleStartModes 覆盖注 15 / N-03 的启动接缝：
// serve / gateway 调用 Start，chat 完全不触碰调度器。
// 逆改点：needsSchedules 恒真 → chat 子例红；startSchedules 不调 Start → serve / gateway 红。
func TestRun_ScheduleStartModes(t *testing.T) {
	t.Run("serve starts scheduler", func(t *testing.T) {
		fake := &fakeSchedulerRunner{}
		require.NoError(t, startSchedules(context.Background(), []string{"serve"}, fake))
		require.Equal(t, 1, fake.starts)
	})
	t.Run("gateway starts scheduler", func(t *testing.T) {
		fake := &fakeSchedulerRunner{}
		require.NoError(t, startSchedules(context.Background(), []string{"gateway"}, fake))
		require.Equal(t, 1, fake.starts)
	})
	t.Run("chat does not start scheduler", func(t *testing.T) {
		fake := &fakeSchedulerRunner{}
		require.NoError(t, startSchedules(context.Background(), []string{"chat", "demo"}, fake))
		require.Zero(t, fake.starts, "chat 进程不得后台触发定时任务（约束 20）")
	})
}

// TestRun_ScheduleStartFailureExits 覆盖注 16：Start 失败必须上抛，由 Run 返回退出码 1。
// 逆改点：startSchedules 吞掉错误 → 断言红。
func TestRun_ScheduleStartFailureExits(t *testing.T) {
	fake := &fakeSchedulerRunner{err: errors.New("recover failed")}
	require.Error(t, startSchedules(context.Background(), []string{"serve"}, fake),
		"Start 失败必须上抛（fail closed），不得降级为可服务")
	require.Equal(t, 1, fake.starts)
}

// TestScheduleStartExitCode_SignalCanceledReturnsZero 覆盖注 15：runCtx 已被信号取消时
// 启动错误不算失败，进程返回 0（WARN 由 run.go 记录）。
// 逆改点：任何错误一律返回 1 → 断言红。
func TestScheduleStartExitCode_SignalCanceledReturnsZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.Zero(t, scheduleStartExitCode(ctx, context.Canceled), "信号取消不是启动失败")
}

// TestScheduleStartExitCode_FailureReturnsOne 与上例构成两分支对照：非取消类错误 → 1。
// 逆改点：忽略 runCtx 状态、恒返回 0 → 断言红。
func TestScheduleStartExitCode_FailureReturnsOne(t *testing.T) {
	require.Equal(t, 1, scheduleStartExitCode(context.Background(), errors.New("recover failed")))
}

// TestNewRuntime_ServiceHasSessionGate 覆盖约束 40 / R10-H1：生产 newRuntime 必须把
// Sessions 端口接进 AgentService——已归档会话的 Run 在准入处被拒，而不是退化为无门禁。
// 逆改点：wire.go 漏传 Sessions → 准入不查会话状态，Run 返回 nil 错误或其余错误，断言红。
func TestNewRuntime_ServiceHasSessionGate(t *testing.T) {
	f := newWireFixture(t)
	f.writeAgent(t, "reader", "read_file")
	rt := f.assemble(t)

	const sessionID = "scheduler:scheduler:reader"
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	require.NoError(t, rt.sessions.UpsertSession(context.Background(), core.SessionRecord{
		ID: sessionID, ProfileName: "reader", Channel: "scheduler", UserID: "scheduler",
		CreatedAt: now, LastActiveAt: now,
	}))
	require.NoError(t, rt.sessions.ArchiveSession(context.Background(), sessionID, now))

	_, err := rt.service.Run(context.Background(), core.RunRequest{
		Agent:     "reader",
		SessionID: sessionID,
		Message:   "归档后不应再运行",
		Channel:   "cli",
		Budget:    core.Budget{ContextTokens: 1},
	})
	var archived *core.SessionArchivedError
	require.ErrorAs(t, err, &archived, "生产装配必须真接 Sessions；归档会话在准入处被拒")
	require.Equal(t, sessionID, archived.SessionID)
}

// TestHeavyRuntime_CloseStopsSchedulerFirst 覆盖约束 22 / 图 F：重路径释放顺序必须是
// 调度器 → MCP → 库 → 实例锁。三个判定面都是**可观测副作用**：
//   - 调度器先停：MCP.Close 执行时，注入引擎的 Stop 已经返回（在途运行先于库关闭收尾）；
//   - MCP 先于库：MCP.Close 执行时库仍可读（ListTasks 成功）；
//   - 实例锁最后：MCP.Close 执行时锁仍被持有，Close 返回后才可再次取得。
//
// 逆改点：`scheduler.Stop()` 挪到 mcp.Close 之后 → ① 红；`db.Close()` 提前到 mcp.Close
// 之前 → ② 红；`lock.Release()` 提前到 MCP 关闭之前 → ③ 红。
func TestHeavyRuntime_CloseStopsSchedulerFirst(t *testing.T) {
	f := newWireFixture(t)
	db, err := storage.OpenWritable(context.Background(), f.dbPath(), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	require.NoError(t, db.Migrate(context.Background(), storage.SupportedSchemaVersion))
	t.Cleanup(func() { require.NoError(t, db.Close(), "重复关闭是幂等的") })

	lock, err := workspace.AcquireInstanceLock(f.root)
	require.NoError(t, err)

	store := storage.NewTaskStore(db)
	engine := &orderProbeEngine{}
	sched := core.NewScheduler(core.SchedulerOptions{
		Store:      store,
		Invoker:    &scheduleTestInvoker{},
		Engine:     engine,
		CronSpec:   workspace.CronSpec,
		RunTimeout: scheduleRunTimeout,
		StopGrace:  scheduleStopGrace,
		Logger:     slog.Default(),
		Now:        func() time.Time { return time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC) },
	})
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	require.NoError(t, sched.Start(runCtx))

	probe := &releaseOrderProbe{engine: engine, store: store, root: f.root}
	rt := &heavyRuntime{scheduler: sched, mcp: probe, db: db, lock: lock}
	require.NoError(t, rt.Close())

	require.Equal(t, 1, probe.closeCalls, "MCP 连接池必须恰好释放一次")
	require.True(t, probe.schedulerStoppedAtMCPClose,
		"调度器必须先于 MCP 停止：在途运行要在库关闭前收尾（约束 22）")
	require.True(t, probe.dbOpenAtMCPClose,
		"MCP 必须先于库释放：MCP.Close 执行时库仍必须可读")
	require.True(t, probe.lockHeldAtMCPClose,
		"实例锁不得先于 MCP 释放：有活句柄时另一个实例不得进得来")

	// Close 返回之后：库已关闭、锁已可再次取得——锁是最后一个释放的资源。
	_, err = store.ListTasks(context.Background())
	require.Error(t, err, "Close 返回后库必须已关闭")
	reacquired, err := workspace.AcquireInstanceLock(f.root)
	require.NoError(t, err, "Close 返回后实例锁必须已释放")
	require.NoError(t, reacquired.Release())
}

// TestApp_SignalContextUsesInjectedNotifier 覆盖约束 24（R10-M10）：serve / gateway 的
// **唯一信号上下文**必须经 `App.notifyContext` 这条接缝派生——注入替身后标准库不再参与。
// 断言：注入器恰调用一次且收到调用方的父 ctx 与生产同一组信号；返回的 runCtx 与注入器
// 返回的 ctx 同源；取消传播到 runCtx；`stopSignals` 就是注入器返回的 cancel。
// 逆改点：生产分支绕过字段直接调 `signal.NotifyContext` → 注入器零调用，断言红。
func TestApp_SignalContextUsesInjectedNotifier(t *testing.T) {
	base, cancelBase := context.WithCancel(context.Background())
	t.Cleanup(cancelBase)

	injectedCtx, cancelInjected := context.WithCancel(base)
	t.Cleanup(cancelInjected)

	var (
		notifyCalls int
		gotParent   context.Context
		gotSignals  []os.Signal
		stopCalls   int
	)
	a := &App{notifyContext: func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
		notifyCalls++
		gotParent = parent
		gotSignals = append([]os.Signal(nil), signals...)
		return injectedCtx, func() {
			stopCalls++
			cancelInjected()
		}
	}}

	runCtx, stopSignals := a.signalContext(base)
	require.Equal(t, 1, notifyCalls,
		"信号上下文必须经 App.notifyContext 接缝派生（绕过字段直接调 signal.NotifyContext → 注入器零调用）")
	require.True(t, gotParent == base, "注入器必须收到调用方传入的父 ctx")
	require.Equal(t, []os.Signal{os.Interrupt, syscall.SIGTERM}, gotSignals,
		"信号集与生产一致：Interrupt + SIGTERM")
	require.True(t, runCtx == injectedCtx, "signalContext 必须原样返回注入器的 ctx（同源）")

	// 取消传播：注入器取消（模拟收到停机信号）→ runCtx 随之取消。
	cancelInjected()
	require.ErrorIs(t, runCtx.Err(), context.Canceled, "取消必须传播到 runCtx")

	// stopSignals 必须就是注入器返回的 cancel：调用它 → 注入器侧计数 +1。
	stopSignals()
	require.Equal(t, 1, stopCalls, "stopSignals 必须是注入器返回的 cancel（调用方要能停掉信号监听）")
}

// orderProbeEngine 是 CronEngine 的探针实现：只记录 Stop 是否已被调用，不真正调度。
// Stop 返回已完成的 ctx——没有在途作业，调度器停机立即收尾。
// orderProbeEngine records whether Stop has run; it never schedules anything.
type orderProbeEngine struct {
	mu      sync.Mutex
	stopped bool
}

// Add 接受注册但不保存作业（本用例不触发运行）。
// Add accepts registrations without keeping the job.
func (e *orderProbeEngine) Add(string, func()) (core.CronHandle, error) {
	return noopCronHandle{}, nil
}

// Start 是空实现：探针引擎没有调度循环。
// Start is a no-op: the probe engine has no scheduling loop.
func (e *orderProbeEngine) Start() {}

// Stop 记录停机事实并返回一个已完成 ctx（无在途作业）。
// Stop records the shutdown and returns an already-done context.
func (e *orderProbeEngine) Stop() context.Context {
	e.mu.Lock()
	e.stopped = true
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// Occurrences 返回空集：本用例没有错过触发点的任务。
// Occurrences returns nothing: there is no task with a missed trigger here.
func (e *orderProbeEngine) Occurrences(string, time.Time, time.Time, int) []time.Time { return nil }

// wasStopped 报告 Stop 是否已执行（调度器先停的判据）。
// wasStopped reports whether Stop has run — the "scheduler stops first" reading.
func (e *orderProbeEngine) wasStopped() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopped
}

// noopCronHandle 是探针引擎的注册句柄。
// noopCronHandle is the probe engine's registration handle.
type noopCronHandle struct{}

// Stop 是空实现：探针作业不会被触发。
// Stop is a no-op because the probe job never runs.
func (noopCronHandle) Stop() {}

// releaseOrderProbe 是释放顺序探针（mcpConnector 实现）：在 MCP.Close 的调用点观察其余
// 资源的可观测状态——调度器是否已停、库是否仍可读、实例锁是否仍被持有。
// releaseOrderProbe observes the other resources at the exact MCP.Close call site.
type releaseOrderProbe struct {
	engine *orderProbeEngine
	store  core.TaskStore
	root   string

	closeCalls                 int
	schedulerStoppedAtMCPClose bool
	dbOpenAtMCPClose           bool
	lockHeldAtMCPClose         bool
}

// Connect 是空实现：本用例不连接任何 MCP server。
// Connect is a no-op because no server is involved here.
func (p *releaseOrderProbe) Connect(context.Context, []core.MCPServerConfig) ([]tool.MCPConnection, error) {
	return nil, nil
}

// Close 在释放点上读取三种资源的状态（只读观察，不改变释放顺序）。
// Close reads the state of the three resources without changing the release order.
func (p *releaseOrderProbe) Close() error {
	p.closeCalls++
	p.schedulerStoppedAtMCPClose = p.engine.wasStopped()
	if _, err := p.store.ListTasks(context.Background()); err == nil {
		p.dbOpenAtMCPClose = true
	}
	if other, err := workspace.AcquireInstanceLock(p.root); err != nil {
		var busy *workspace.LockBusyError
		p.lockHeldAtMCPClose = errors.As(err, &busy)
	} else {
		// 锁已被提前释放：记录为"未持有"并立刻归还，避免污染后续判定。
		_ = other.Release()
	}
	return nil
}

// demoEntry 构造一条已注册、带单一定时任务的 Agent 条目。
// demoEntry builds one registered agent entry with a single schedule.
func demoEntry(taskID string) core.AgentEntry {
	return core.AgentEntry{
		Name:       "demo",
		Registered: true,
		Profile: &core.Profile{Schedules: []core.Schedule{{
			ID: taskID, Cron: "*/1 * * * * *", Zone: "UTC", Message: "巡检",
		}}},
	}
}

// openScheduleDB 打开一份临时库并显式迁移（与重路径同一开库口径，但不取实例锁）。
// openScheduleDB opens a temporary database and migrates it explicitly.
func openScheduleDB(t *testing.T) *storage.DB {
	t.Helper()

	db, err := storage.OpenWritable(context.Background(), filepath.Join(t.TempDir(), "basalt.db"), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	require.NoError(t, db.Migrate(context.Background(), storage.SupportedSchemaVersion))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// newScheduleTestScheduler 用真实仓储 + 真实 cron 引擎 + fake invoker 构造被测调度器。
// newScheduler 的签名是冻结面（不接收引擎），因此引擎不走注入：`*/1` 的定时任务在
// 下一秒真实触发，用 fake invoker 捕获运行请求。
// newScheduleTestScheduler builds the scheduler under test with a real task store, the
// real cron engine (the frozen constructor takes no engine), and a fake invoker.
func newScheduleTestScheduler(t *testing.T, cfg *cli.Config, entries []core.AgentEntry) (*core.Scheduler, *scheduleTestInvoker, core.TaskStore) {
	t.Helper()

	store := storage.NewTaskStore(openScheduleDB(t))
	invoker := &scheduleTestInvoker{}
	a := &App{
		logger: slog.Default(),
		now:    func() time.Time { return time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC) },
	}
	sched := a.newScheduler(store, invoker, entries, cfg, slog.Default())
	return sched, invoker, store
}

// fakeSchedulerRunner 记录 Start 调用次数；err 非 nil 时模拟启动失败。
// fakeSchedulerRunner counts Start calls and can fail on demand.
type fakeSchedulerRunner struct {
	starts int
	err    error
}

func (f *fakeSchedulerRunner) Start(context.Context) error {
	f.starts++
	return f.err
}

// scheduleTestInvoker 记录每次 Run 的请求与 ctx 截止时间。
// scheduleTestInvoker records every Run request and its context deadline.
type scheduleTestInvoker struct {
	mu        sync.Mutex
	reqs      []core.RunRequest
	deadlines []time.Time
}

func (i *scheduleTestInvoker) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	deadline, _ := ctx.Deadline()
	i.mu.Lock()
	i.reqs = append(i.reqs, req)
	i.deadlines = append(i.deadlines, deadline)
	i.mu.Unlock()
	return core.RunResult{RunID: req.RunID, Status: core.StatusCompleted}, nil
}

// awaitFirstRequest 等待 cron 的首次真实触发（`*/1` 最多约 1s），返回第一条运行请求。
// 等待上限 5s 只为覆盖调度抖动；超时即"到点根本没有发起运行"，断言红。
// awaitFirstRequest waits for the first real cron tick and returns its run request.
func (i *scheduleTestInvoker) awaitFirstRequest(t *testing.T) core.RunRequest {
	t.Helper()

	require.Eventually(t, func() bool {
		i.mu.Lock()
		defer i.mu.Unlock()
		return len(i.reqs) > 0
	}, 5*time.Second, 10*time.Millisecond, "定时任务到点未发起运行")

	i.mu.Lock()
	defer i.mu.Unlock()
	return i.reqs[0]
}

// awaitFirstDeadline 等待首次触发并返回其运行 ctx 的截止时间（零值 = 无截止）。
// awaitFirstDeadline waits for the first tick and returns its context deadline.
func (i *scheduleTestInvoker) awaitFirstDeadline(t *testing.T) time.Time {
	t.Helper()

	require.Eventually(t, func() bool {
		i.mu.Lock()
		defer i.mu.Unlock()
		return len(i.deadlines) > 0
	}, 5*time.Second, 10*time.Millisecond, "定时任务到点未发起运行")

	i.mu.Lock()
	defer i.mu.Unlock()
	return i.deadlines[0]
}

// TestScheduleEditorAdapter_MapsRejectionToInvalidInput 覆盖约束 31 的失败分类：
// 候选校验拒绝（workspace.ErrScheduleRejected）→ *core.InvalidInputError（模型可见
// invalid_input），且文件零写入；未知 Agent 的 IO 错误（fs.ErrNotExist）原样透传
// （工具层据此归 not_found，不得改写成 invalid_input）。
// 逆改点：拒绝原样上抛 → IsInvalidInput 断言红；IO 错误也包成 InvalidInputError →
// not_found 分类断言红；拒绝路径先落盘 → 文件字节断言红。
func TestScheduleEditorAdapter_MapsRejectionToInvalidInput(t *testing.T) {
	t.Run("rejection maps to invalid input", func(t *testing.T) {
		root := t.TempDir()
		before := writeAgentFixture(t, root, "demo")
		adapter := newAdapterFixture(t, root, []core.AgentEntry{registeredEntry("demo")}, openAdapterStore(t), false)

		_, err := adapter.ApplySchedule(context.Background(), "demo", core.ScheduleSet,
			core.Schedule{ID: "bad", Cron: "0 8 * * *", Message: "5 段 cron"})
		require.True(t, core.IsInvalidInput(err), "候选校验失败必须归 invalid_input：%v", err)
		require.Equal(t, before, readAgentFixture(t, root, "demo"), "拒绝路径必须零写入")
	})

	t.Run("io error passes through", func(t *testing.T) {
		root := t.TempDir()
		adapter := newAdapterFixture(t, root, []core.AgentEntry{registeredEntry("demo")}, openAdapterStore(t), false)

		_, err := adapter.ApplySchedule(context.Background(), "ghost", core.ScheduleSet,
			core.Schedule{ID: "ghost-1", Cron: "*/5 * * * * *", Message: "不存在"})
		require.ErrorIs(t, err, fs.ErrNotExist, "未知 Agent 的 IO 错误必须保留 fs.ErrNotExist 链（not_found）")
		require.False(t, core.IsInvalidInput(err), "IO 错误不得改写成 invalid_input")
	})
}

// TestScheduleEditorAdapter_ReconcilesAfterWrite 覆盖图 E / 约束 16、17、41：已 Start
// （serve / gateway）时写成功后必须调 ReconcileAgent——以 ApplyAgentTasks 单事务落库，
// 注册表立即生效；未 Start 的 no-op 由 core 的 TestScheduler_ReconcileAgent 覆盖。
// 逆改点：写成功不调 ReconcileAgent → 库行仍是启动时的旧 message，断言红。
func TestScheduleEditorAdapter_ReconcilesAfterWrite(t *testing.T) {
	root := t.TempDir()
	writeAgentFixture(t, root, "demo")
	store := storage.NewTaskStore(openScheduleDB(t))
	adapter := newAdapterFixture(t, root, []core.AgentEntry{demoEntry("demo-1")}, store, true)

	after, err := adapter.ApplySchedule(context.Background(), "demo", core.ScheduleSet,
		core.Schedule{ID: "demo-1", Cron: "*/10 * * * * *", Message: "改后的任务"})
	require.NoError(t, err)
	require.Len(t, after, 1)

	rows, err := store.ListTasks(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "改后的任务", rows[0].Message, "写成功必须经 ReconcileAgent 落库（已 Start）")
	require.Equal(t, "*/10 * * * * *", rows[0].Cron)
}

// TestScheduleEditorAdapter_RefreshesClaimBaseline 覆盖约束 30 / R-05：私有 claim 基线
// 在每次成功写入后于同一临界区刷新——A 先写入的 id，B 在本进程内再写必须被拒，
// 不等到重启才由装载期 validateSchedules 让其中一只整只不注册。
// 逆改点：成功写入后不替换 claims[agent] → 第二次写入成功，IsInvalidInput 断言红。
func TestScheduleEditorAdapter_RefreshesClaimBaseline(t *testing.T) {
	root := t.TempDir()
	writeAgentFixture(t, root, "demo")
	otherBefore := writeAgentFixture(t, root, "other")
	entries := []core.AgentEntry{registeredEntry("demo"), registeredEntry("other")}
	adapter := newAdapterFixture(t, root, entries, openAdapterStore(t), false)

	_, err := adapter.ApplySchedule(context.Background(), "demo", core.ScheduleSet,
		core.Schedule{ID: "shared", Cron: "*/5 * * * * *", Message: "demo 的任务"})
	require.NoError(t, err)

	_, err = adapter.ApplySchedule(context.Background(), "other", core.ScheduleSet,
		core.Schedule{ID: "shared", Cron: "*/6 * * * * *", Message: "other 的任务"})
	require.True(t, core.IsInvalidInput(err), "第二次写入必须按刷新后的 claims 拒绝：%v", err)
	require.Contains(t, err.Error(), "demo", "拒绝 detail 必须点名占用者")
	require.Equal(t, otherBefore, readAgentFixture(t, root, "other"), "拒绝路径零写入")
}

// TestScheduleEditorAdapter_ConcurrentCrossAgentSameID 覆盖约束 30 的并发面：两个 Agent
// 以同名 id 并发 set，由 adapter.mu 串行化——恰一个成功、另一个 ErrScheduleRejected
// （invalid_input），落盘文件里只有胜者的定义（不出现双写同名 id）。在 -race 下运行。
// 逆改点：读快照后释放锁再写 → 两者都成功，恰好一个断言红。
func TestScheduleEditorAdapter_ConcurrentCrossAgentSameID(t *testing.T) {
	root := t.TempDir()
	writeAgentFixture(t, root, "demo")
	writeAgentFixture(t, root, "other")
	entries := []core.AgentEntry{registeredEntry("demo"), registeredEntry("other")}
	adapter := newAdapterFixture(t, root, entries, openAdapterStore(t), false)

	start := make(chan struct{})
	var errs [2]error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = adapter.ApplySchedule(context.Background(), "demo", core.ScheduleSet,
			core.Schedule{ID: "shared", Cron: "*/5 * * * * *", Message: "demo"})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errs[1] = adapter.ApplySchedule(context.Background(), "other", core.ScheduleSet,
			core.Schedule{ID: "shared", Cron: "*/6 * * * * *", Message: "other"})
	}()
	close(start)
	wg.Wait()

	okCount := 0
	var loser error
	for _, err := range errs {
		if err == nil {
			okCount++
		} else {
			loser = err
		}
	}
	require.Equal(t, 1, okCount, "并发同名 id 必须恰一个成功")
	require.True(t, core.IsInvalidInput(loser), "败者必须是 ErrScheduleRejected → invalid_input：%v", loser)
	require.Contains(t, loser.Error(), "shared", "拒绝 detail 点名 id")

	owners := 0
	for _, name := range []string{"demo", "other"} {
		// 写入门一律以 YAML 双引号标量渲染 id（用户 review ② 的注入修复）。
		if strings.Contains(readAgentFixture(t, root, name), `id: "shared"`) {
			owners++
		}
	}
	require.Equal(t, 1, owners, "落盘文件里只能出现胜者的定义")
}

// TestScheduleEditorAdapter_ConcurrentWithProfilesRead 覆盖 P-05 / F5 / R26：适配器只改
// 私有 claims、**不回写**共享 entries；与 `/profiles` 的真实读路径并发时 -race 干净，
// 且写入结束后 entries 内容仍是装载时的原值。
// 逆改点：落盘后原地改写共享 entries（如把新 Schedule 写回 entries[i].Profile.Schedules）
// → -race 报 DATA RACE；最后一条"entries 未被回写"断言同时红。
func TestScheduleEditorAdapter_ConcurrentWithProfilesRead(t *testing.T) {
	root := t.TempDir()
	writeAgentFixture(t, root, "demo")
	entries := []core.AgentEntry{{
		Name:       "demo",
		Registered: true,
		Profile: &core.Profile{Schedules: []core.Schedule{{
			ID: "demo-1", Cron: "0 8 * * * *", Zone: "UTC", Message: "巡检",
		}}},
	}}
	// 未 Start：ReconcileAgent 是 no-op，本用例只压并发写面。
	adapter := newAdapterFixture(t, root, entries, openAdapterStore(t), false)

	errCh := make(chan error, 40)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // 写面：反复 upsert 同一 id
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := adapter.ApplySchedule(context.Background(), "demo", core.ScheduleSet,
				core.Schedule{ID: "demo-1", Cron: "0 9 * * * *", Message: "巡检"}); err != nil {
				errCh <- err
			}
		}
	}()
	go func() { // 读面：/profiles 的真实读路径（agentProfilesPort 直接读同一 entries 切片）
		defer wg.Done()
		port := agentProfilesPort{entries: entries}
		for i := 0; i < 20; i++ {
			for _, e := range port.Profiles() {
				if e.Profile == nil {
					continue
				}
				for _, s := range e.Profile.Schedules { // 读元素内容：竞争落在 Schedule 字段上才暴露
					_ = s.Cron
				}
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	require.Equal(t, "0 8 * * * *", entries[0].Profile.Schedules[0].Cron,
		"适配器不得回写共享 entries（P-05）")
}

// TestScheduleEditorAdapter_CrossOwnerConflict_NoWrite 覆盖约束 30 / 42（HIGH-3）：
// 库内 task_id 已归属其他 Agent（含 retired 行）→ CheckOwnership 返回
// ErrTaskOwnerConflict → 适配器包装 ErrScheduleRejected / invalid_input，**文件零写入**、
// 不触发 ReconcileAgent（库行原样）。
// 逆改点：先落盘再判归属 → 文件字节已变（且 ReconcileAgent 才会撞事务守卫），断言红。
func TestScheduleEditorAdapter_CrossOwnerConflict_NoWrite(t *testing.T) {
	root := t.TempDir()
	before := writeAgentFixture(t, root, "demo")
	store := storage.NewTaskStore(openScheduleDB(t))
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	require.NoError(t, store.ApplyAgentTasks(context.Background(), "other", []core.TaskRecord{{
		TaskID: "shared", Cron: "*/5 * * * * *", Zone: "UTC", Message: "他家的任务",
	}}, nil, now))
	adapter := newAdapterFixture(t, root, []core.AgentEntry{registeredEntry("demo")}, store, false)

	_, err := adapter.ApplySchedule(context.Background(), "demo", core.ScheduleSet,
		core.Schedule{ID: "shared", Cron: "*/7 * * * * *", Message: "抢占"})
	require.True(t, core.IsInvalidInput(err), "跨 owner 冲突必须归 invalid_input：%v", err)
	require.Contains(t, err.Error(), "other", "detail 必须点名原 owner")
	require.Equal(t, before, readAgentFixture(t, root, "demo"), "归属预检失败必须零写入（约束 42）")

	rows, err := store.ListTasks(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "他家的任务", rows[0].Message, "不得走到 ReconcileAgent（库行原样）")
}

// writeAgentFixture 落一份最小可写的 AGENT.md（合法 frontmatter、无 schedules 块），
// 返回原始字节供"零写入"对照。Applying a schedule only works on an existing file.
func writeAgentFixture(t *testing.T, root, name string) string {
	t.Helper()

	dir := filepath.Join(root, "agents", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := "---\nname: " + name + "\ndescription: 适配器夹具\n---\n你是 " + name + "。\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(content), 0o644))
	return content
}

// readAgentFixture 读回 AGENT.md 原始字节（零写入断言逐字节对照）。
// readAgentFixture returns the raw bytes for byte-identical assertions.
func readAgentFixture(t *testing.T, root, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, "agents", name, "AGENT.md"))
	require.NoError(t, err)
	return string(raw)
}

// registeredEntry 构造一条已注册、无任务的 Agent 条目（claims 基线为空）。
// registeredEntry builds a registered agent entry with no schedules.
func registeredEntry(name string) core.AgentEntry {
	return core.AgentEntry{Name: name, Registered: true, Profile: &core.Profile{}}
}

// newAdapterFixture 构造适配器测试夹具：真实写入门 + 指定仓储 + 探针引擎的调度器；
// start 为真时 Start（ReconcileAgent 生效），否则保持未启动（no-op）。
// newAdapterFixture builds the adapter under test with the real write gate and a probe
// engine; start controls whether ReconcileAgent is live.
func newAdapterFixture(t *testing.T, root string, entries []core.AgentEntry, store core.TaskStore, start bool) *scheduleEditorAdapter {
	t.Helper()

	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{Logger: slog.Default()})
	sched := core.NewScheduler(core.SchedulerOptions{
		Store:      store,
		Invoker:    &scheduleTestInvoker{},
		Engine:     &orderProbeEngine{},
		Entries:    entries,
		CronSpec:   workspace.CronSpec,
		RunTimeout: scheduleRunTimeout,
		StopGrace:  scheduleStopGrace,
		Logger:     slog.Default(),
		Now:        func() time.Time { return time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC) },
	})
	if start {
		require.NoError(t, sched.Start(context.Background()))
		t.Cleanup(sched.Stop)
	}
	return newScheduleEditorAdapter(loader, root, entries, sched, slog.Default())
}

// openAdapterStore 打开独立临时库并返回任务仓储（未 Start 的场景仍需 CheckOwnership 读面）。
// openAdapterStore opens a fresh task store for adapter-only cases.
func openAdapterStore(t *testing.T) core.TaskStore {
	t.Helper()

	return storage.NewTaskStore(openScheduleDB(t))
}
