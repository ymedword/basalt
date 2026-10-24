package app

// 本文件是调度装配面（颗粒度文档 §3.1.8）：把 core.Scheduler 需要的仓储、运行入口、
// 引擎与注册定义在装配层一次接好，并提供 run.go 的启动接缝。
// This file wires the scheduler at the assembly layer and exposes the startup seam used
// by run.go.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	cronadapter "github.com/ymedword/basalt/internal/adapters/cron"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// scheduleRunTimeout 是单次定时运行的上限（约束 23）：到点即取消在途运行。
	// scheduleRunTimeout caps one scheduled run.
	scheduleRunTimeout = 300 * time.Second

	// scheduleStopGrace 是停机的宽限窗口（约束 24）：在途运行最长再等这么久，
	// 超时才取消并进入有界清理。
	// scheduleStopGrace is the shutdown grace for in-flight runs.
	scheduleStopGrace = 70 * time.Second
)

// newScheduler 构造调度器；Entries 由 core.NewScheduler 在构造时拷成私有快照，
// 适配器与 web 只读面此后都不回写它（P-05 / 约束 3）。
// newScheduler builds the scheduler; core.NewScheduler snapshots Entries at construction,
// so later mutations of the caller's slice cannot change definitions.
func (a *App) newScheduler(store core.TaskStore, invoker core.RunInvoker, entries []core.AgentEntry,
	delivery core.ScheduleDelivery, cfg *cli.Config, logger *slog.Logger,
) *core.Scheduler {
	if logger == nil {
		logger = a.loggerOr()
	}
	return core.NewScheduler(core.SchedulerOptions{
		Store:      store,
		Invoker:    invoker,
		Engine:     cronadapter.New(logger),
		Entries:    entries,
		CronSpec:   workspace.CronSpec,
		Delivery:   delivery,
		Budget:     webBudget(cfg),
		RunTimeout: scheduleRunTimeout,
		StopGrace:  scheduleStopGrace,
		Logger:     logger,
		Now:        a.nowOr(),
	})
}

// needsSchedules 只对 serve / gateway 为真（约束 20）：其余重命令不构造运行期调度，
// 写入门落下的定义在下次 serve / gateway 启动时生效。
// needsSchedules is true only for serve / gateway.
func needsSchedules(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "serve", "gateway":
		return true
	default:
		return false
	}
}

// scheduleStartExitCode 判定启动失败 / 信号中断的进程退出码：runCtx 已被取消 → 0
// （启动被信号打断，不是故障）；否则 → 1（fail closed）。纯函数，唯一调用方在 run.go。
// scheduleStartExitCode decides the process exit code: a cancelled run context means the
// signal interrupted startup (0), anything else is a real failure (1).
func scheduleStartExitCode(runCtx context.Context, err error) int {
	if runCtx != nil && runCtx.Err() != nil {
		return 0
	}
	return 1
}

// schedulerRunner 是启动接缝的消费方形态：测试注入 fake，直接驱动模式判定与失败处置
// （N-03 / §4.3），无需装配真实 runtime 或发真实信号。
// schedulerRunner is the startup seam's consumer shape, letting tests inject a fake.
type schedulerRunner interface {
	Start(ctx context.Context) error
}

// startSchedules 仅在 needsSchedules 为真时调用 Start；其余模式直接返回 nil。
// startSchedules starts the scheduler only for serve / gateway.
func startSchedules(ctx context.Context, args []string, sched schedulerRunner) error {
	if !needsSchedules(args) {
		return nil
	}
	return sched.Start(ctx)
}

// scheduleEditorAdapter 是 manage_schedule 的生产编辑后端（颗粒度文档 §3.1.8 / 约束 30）：
// 落盘前做库内归属预检（约束 42），落盘走工作区唯一写入门，写成功后刷新私有 claim 基线
// 并让调度器即时对账；它**不持有、不回写**共享 entries（P-05）——`/profiles` 读面与
// 调度器构造期快照全程无竞争。
// scheduleEditorAdapter is the production backend for manage_schedule: pre-write ownership
// check, the workspace write gate, private claim baseline refresh, and live reconciliation.
type scheduleEditorAdapter struct {
	loader    *workspace.AgentLoader
	root      string
	scheduler *core.Scheduler
	logger    *slog.Logger

	// mu 覆盖"读 claims → CheckOwnership → 校验 → 落盘 → 刷新 claims → ReconcileAgent"
	// 整段临界区（约束 30）：两个 Agent 并发写同一 id 由此串行化。SetRegistered 与
	// ReconcileAgent 的调度器委托同样在这把锁内完成——归档注销、热加载对账不得与写面
	// 交错。锁序固定 adapter.mu → loader.writeMu、adapter.mu → scheduler.stateMu，
	// 不得反向。
	// mu serialises the whole read-check-write-reconcile cycle and the registry hooks'
	// scheduler delegation; the lock order adapter.mu → loader.writeMu /
	// scheduler.stateMu is fixed and never reversed.
	mu sync.Mutex

	// claims 是私有 claim 基线（只含已注册 Agent 的 schedules）：构造时从 entries 拷出，
	// 此后只由 ApplySchedule 在 mu 内替换，entries 保持只读。
	claims map[string][]core.Schedule
}

// 编译期断言：适配器就是工具消费的编辑后端，同时兼任注册表变更的调度器接缝
// （注册表热加载后 claim 基线与调度器集合一起推进，约束 39）。
// The adapter is both the tool's editor backend and the scheduler seam for registry
// changes, so the claim baseline follows hot reloads.
var (
	_ tool.ScheduleEditor = (*scheduleEditorAdapter)(nil)
	_ ScheduleRegistry    = (*scheduleEditorAdapter)(nil)
)

// newScheduleEditorAdapter 构造适配器并拷出私有 claim 基线。
// newScheduleEditorAdapter snapshots the private claim baseline from entries.
func newScheduleEditorAdapter(loader *workspace.AgentLoader, root string, entries []core.AgentEntry, sched *core.Scheduler, logger *slog.Logger) *scheduleEditorAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	claims := make(map[string][]core.Schedule, len(entries))
	for _, entry := range entries {
		if !entry.Registered || entry.Profile == nil {
			continue
		}
		claims[entry.Name] = append([]core.Schedule(nil), entry.Profile.Schedules...)
	}
	return &scheduleEditorAdapter{
		loader:    loader,
		root:      root,
		scheduler: sched,
		logger:    logger,
		claims:    claims,
	}
}

// ReadSchedules 读当前文件的任务集（不读 DB、不缓存，约束 30）。
// ReadSchedules reads the file's current task set; no DB, no cache.
func (e *scheduleEditorAdapter) ReadSchedules(_ context.Context, agent string) ([]core.Schedule, error) {
	return e.loader.ReadSchedules(e.root, agent)
}

// SetRegistered 让注册集合的增减同时反映到私有 claim 基线（约束 39 / 约束 30），并在
// adapter.mu 内完成调度器委托：归档注销不得与 manage_schedule 的落盘 / 对账交错
// （约束 42 的写锁顺序 adapter.mu → scheduler.stateMu）。移出的 Agent 不再参与归属
// 预检，否则归档后旧 claim 会让 manage_schedule 误判跨 Agent 冲突；加入不在这里写
// claim——紧随其后的 ReconcileAgent 会带上新定义。注销成功才清 claim：调度器失败时
// 调用方按回滚矩阵恢复注册，旧基线保留到下一轮对齐。
// SetRegistered delegates to the scheduler under the same lock and drops the claim
// baseline only after the removal succeeds; an added agent learns its claims from
// ReconcileAgent.
func (e *scheduleEditorAdapter) SetRegistered(agent string, registered bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.scheduler.SetRegistered(agent, registered); err != nil {
		return err
	}
	if !registered {
		delete(e.claims, agent)
	}
	return nil
}

// ReconcileAgent 在同一临界区内先刷新 claim 基线再委托调度器（与 ApplySchedule 同序：
// 文件事实先行，库对齐失败不回退 claim——它记的是文件真值）。空定义（归档注销）只
// 退休任务行；本方法**不**自动启用任何任务——归档恢复本身不触碰调度器（约束 32），
// 恢复后的 Agent 要重新产生定时任务，只能由显式定义（热加载或下一次 manage_schedule）
// 驱动。
// ReconcileAgent refreshes the claim baseline and delegates under one critical section.
// Empty definitions only retire rows; this method never auto-enables a restored agent.
func (e *scheduleEditorAdapter) ReconcileAgent(ctx context.Context, agent string, schedules []core.Schedule) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.claims[agent] = append([]core.Schedule(nil), schedules...)
	return e.scheduler.ReconcileAgent(ctx, agent, schedules)
}

// ApplySchedule 在私有临界区内完成一次读-改-验-写：先按当前文件解析出本次触碰的 id，
// 落盘前查库内归属（跨 Agent 含 retired 一律拒绝、零写入），再走唯一写入门；成功后在
// 同一临界区替换 claims[agent]，最后让调度器即时对账（未 Start 时 no-op）。
// ApplySchedule performs the whole read-check-write-reconcile cycle under the private lock.
func (e *scheduleEditorAdapter) ApplySchedule(ctx context.Context, agent string, action core.ScheduleAction, task core.Schedule) ([]core.Schedule, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	touched, err := e.touchedIDs(agent, action, task)
	if err != nil {
		return nil, e.classify(err)
	}
	if err := e.scheduler.CheckOwnership(ctx, agent, touched); err != nil {
		e.logger.Warn("归属预检拒绝写入", "agent", agent, "task_ids", touched, "error", err)
		// 归属冲突保持"候选被拒"的同一载体口径（约束 31）：invalid_input，detail 点名原 owner。
		return nil, &core.InvalidInputError{Tool: "manage_schedule", Reason: err.Error()}
	}

	after, err := e.loader.ApplySchedule(e.root, e.claims, agent, action, task)
	if err != nil {
		return nil, e.classify(err)
	}

	// 落盘成功后在同一个临界区刷新基线（约束 30 / R-05）：下一次写入即以新基线判定。
	e.claims[agent] = append([]core.Schedule(nil), after...)

	// 已 Start（serve / gateway）时单事务落库并重注册；未 Start 时 no-op（注 13、约束 21）。
	// 库写错误不包装成 invalid_input：文件已合法落盘，重发 set 幂等，按工具执行失败上抛。
	if err := e.scheduler.ReconcileAgent(ctx, agent, after); err != nil {
		e.logger.Error("写入后对账失败，文件已落盘", "agent", agent, "error", err)
		return nil, err
	}
	return after, nil
}

// touchedIDs 解析本次写操作触碰的任务 id（约束 42 的预检对象）：remove 取被删 id；
// set 取显式 id，省略 id 时定位当前唯一任务的**实际** id（D-10——不派生 <agent>-1），
// 当前 0 个或多个 → 候选被拒。list 不写文件，返回空集。
// touchedIDs resolves the ids this write touches before anything is persisted.
func (e *scheduleEditorAdapter) touchedIDs(agent string, action core.ScheduleAction, task core.Schedule) ([]string, error) {
	switch action {
	case core.ScheduleRemove:
		id := strings.TrimSpace(task.ID)
		if id == "" {
			return nil, fmt.Errorf("%w: remove 必须提供 id", workspace.ErrScheduleRejected)
		}
		return []string{id}, nil

	case core.ScheduleSet:
		if id := strings.TrimSpace(task.ID); id != "" {
			return []string{id}, nil
		}
		current, err := e.loader.ReadSchedules(e.root, agent)
		if err != nil {
			return nil, err
		}
		if len(current) != 1 {
			return nil, fmt.Errorf("%w: set 省略 id 仅限该 Agent 当前恰有一个任务", workspace.ErrScheduleRejected)
		}
		return []string{current[0].ID}, nil

	default:
		return nil, fmt.Errorf("%w: 未知动作 %q", workspace.ErrScheduleRejected, action)
	}
}

// classify 把候选校验拒绝统一转成 invalid_input（模型可见、可自纠）；IO / 库错误原样
// 透传，由工具层按错误链分类（not_found 等）。
// classify maps write-gate rejections to invalid_input and passes other errors through.
func (e *scheduleEditorAdapter) classify(err error) error {
	if errors.Is(err, workspace.ErrScheduleRejected) {
		return &core.InvalidInputError{Tool: "manage_schedule", Reason: err.Error()}
	}
	return err
}
