package core

// 本文件是调度器的核心业务实现（US-10 / 技术方案 §6.3）：启动恢复与对账、任务注册与
// 代次、到点触发的每任务锁、唯一执行链 `scheduleRun`、管理面（List / Executions /
// SetEnabled / RunNow）与停机收尾。签名逐字取自 docs/us/us10-scheduler.md §3.1.2。
// This file implements the scheduler core: startup recovery and reconciliation, task
// registration with generations, the per-task lock on the scheduled path, the single
// execution chain, the management surface, and bounded shutdown.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 调度器的固定文案与常量（契约字面量，不得改写）。
// Fixed messages and constants of the scheduler (contract literals).
const (
	schedulerBusyMessage     = "同一 Agent 的另一任务仍在执行"
	schedulerArchivedMessage = "会话已归档，本轮跳过"
	schedulerLockMissMessage = "上一轮未结束，本轮跳过"

	schedulerTriggerLog = "调度触发"
	schedulerFailLog    = "调度运行失败"
	schedulerPanicLog   = "调度运行 panic"

	// schedulerMissedOccurrences 是错过触发点的枚举上限（约束 38）。
	schedulerMissedOccurrences = 1000
	// schedulerFinishTimeout 是终态写的清理时限（约束 11 / 图 C）。
	schedulerFinishTimeout = 5 * time.Second
	// schedulerDeliveryTimeout 是系统代投单次发送的时限：运行已结束，发送用独立 ctx，
	// 不继承可能已超时的运行 ctx；超时按「结果未知」记录，不自动重发（R7）。
	schedulerDeliveryTimeout = 15 * time.Second
	// schedulerStopWait 是宽限耗尽、取消在途运行后的第二段有界等待（约束 23）。
	schedulerStopWait = 5 * time.Second
)

// errSchedulerStarted 表示调度器已经成功启动或已被停止，不得再次启动。
// errSchedulerStarted reports a scheduler that was already started or stopped.
var errSchedulerStarted = errors.New("scheduler: already started")

// SchedulerOptions 是调度器的全部装配输入（§3.1.2 逐字）。
// SchedulerOptions is the full assembly input of the scheduler.
type SchedulerOptions struct {
	Store    TaskStore
	Invoker  RunInvoker
	Engine   CronEngine
	Entries  []AgentEntry
	CronSpec func(Schedule) (string, error) // workspace.CronSpec；唯一 CRON_TZ= 前缀出口
	// Delivery 是系统代投端口（R7）；任务配置了 deliver_to 而这里为 nil 时，
	// 该任务不发起运行并如实记失败——**不得**降级为"模型自己发通知"。
	Delivery   ScheduleDelivery
	Budget     Budget
	RunTimeout time.Duration // 300s（§3.1.8）
	StopGrace  time.Duration // 70s（§3.1.8）
	Logger     *slog.Logger
	Now        func() time.Time
}

// Scheduler 是定时任务调度器。任务运行期状态一律读库，进程内只保留**注册事实**
// （registry / definitions 只读快照、句柄表、注册代次）与锁表。
// Scheduler keeps only registration facts in memory; runtime task state is always read
// from the store.
type Scheduler struct {
	store      TaskStore
	invoker    RunInvoker
	engine     CronEngine
	cronSpec   func(Schedule) (string, error)
	delivery   ScheduleDelivery
	budget     Budget
	runTimeout time.Duration
	stopGrace  time.Duration
	logger     *slog.Logger
	now        func() time.Time

	// 构造期固化的只读快照（P-05）：registry 只当 Agent 注册表，definitions 仅供
	// Start 对账；二者都不得回写，也不缓存运行期任务状态（约束 3）。
	registry    map[string]bool
	definitions map[string][]Schedule

	// stateMu 保护以下字段；锁序固定为 stateMu → 每任务锁（约束 39 / P-06）。
	stateMu    sync.RWMutex
	handles    map[string]CronHandle
	gens       map[string]uint64
	nextGen    uint64
	started    bool
	stopped    bool
	baseCtx    context.Context
	cancelBase context.CancelFunc

	locks sync.Map // taskID → *sync.Mutex

	stopOnce sync.Once
	done     chan struct{}
}

// NewScheduler 构造调度器：拷贝 Entries 为只读快照、建立空句柄表与代次表。
// NewScheduler copies Entries into read-only snapshots and initializes the handle and
// generation tables.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now // 唯一的时钟缺省点（约束 37）
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	registry := make(map[string]bool, len(opts.Entries))
	definitions := make(map[string][]Schedule, len(opts.Entries))
	for _, entry := range opts.Entries {
		if entry.Registered {
			registry[entry.Name] = true
		}
		var schedules []Schedule
		if entry.Profile != nil {
			schedules = append(schedules, entry.Profile.Schedules...)
		}
		definitions[entry.Name] = schedules
	}
	return &Scheduler{
		store:       opts.Store,
		invoker:     opts.Invoker,
		engine:      opts.Engine,
		cronSpec:    opts.CronSpec,
		delivery:    opts.Delivery,
		budget:      opts.Budget,
		runTimeout:  opts.RunTimeout,
		stopGrace:   opts.StopGrace,
		logger:      opts.Logger,
		now:         opts.Now,
		registry:    registry,
		definitions: definitions,
		handles:     make(map[string]CronHandle),
		gens:        make(map[string]uint64),
		done:        make(chan struct{}),
	}
}

// Start 执行图 B：恢复 → 对账 → 注册 → 错过告警 → 启动引擎与信号 watcher。
// 只有**成功**过一次的 Start 会拒绝重入；失败不置启动位，可重试（约束 21 / 图 B ①）。
// Start runs the startup pipeline; a failed attempt does not mark the scheduler started,
// so a retry is allowed.
func (s *Scheduler) Start(ctx context.Context) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if s.started || s.stopped {
		return errSchedulerStarted
	}
	now := s.now()

	// ② 恢复遗留 started 执行行；存储层失败是致命的（约束 19 / 注 16）。
	recovered, err := s.store.RecoverStartedExecutions(ctx, now)
	if err != nil {
		s.logger.Error("启动恢复遗留执行失败", "error", err)
		return fmt.Errorf("scheduler: recover started executions: %w", err)
	}
	s.logger.Info("启动恢复完成", "recovered", recovered)

	// ②b 恢复遗留的投递 pending 行：关联运行已完成 → unknown（**不自动重发**），
	// 其余（运行未完成 / 已失败）→ skipped。存储失败与执行行恢复同属致命。
	if s.delivery != nil {
		recoveredDeliveries, deliveryErr := s.delivery.Recover(ctx, now)
		if deliveryErr != nil {
			s.logger.Error("启动恢复遗留投递失败", "error", deliveryErr)
			return fmt.Errorf("scheduler: recover schedule deliveries: %w", deliveryErr)
		}
		s.logger.Info("投递恢复完成", "recovered", recoveredDeliveries)
	}

	// ③ 读库。
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("启动读取任务失败", "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}

	// ④ 对账第一阶段：逐已注册 Agent 单事务提交；未注册项不遍历（坏文件不写库）。
	// 本阶段**只提交**，不注册任何句柄——任一事务失败时已提交的 Agent 也不会留下
	// 半套句柄（图 B 的两阶段口径，R10-M8）。
	affected := make(map[string]bool)
	for _, agent := range s.registeredAgentNames() {
		fresh, touched, err := s.commitAgentLocked(ctx, agent, s.definitions[agent], rows)
		if err != nil {
			s.logger.Error("启动对账失败", "agent", agent, "error", err)
			return fmt.Errorf("scheduler: reconcile agent %q: %w", agent, err)
		}
		rows = fresh
		for id := range touched {
			affected[id] = true
		}
	}

	// ⑥ 目录已被整个删除的 Agent：库行只退休、不物理删除（同属提交阶段）。
	if err := s.retireRemovedAgentsLocked(ctx, rows, now, affected); err != nil {
		s.logger.Error("退场 Agent 收尾失败", "error", err)
		return err
	}
	rows, err = s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("启动读取任务失败", "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}

	// ⑤ 对账第二阶段：全部提交成功后才切换句柄；单任务失败即致命（约束 19 / 注 16）。
	if err := s.registerEnabledLocked(rows, affected); err != nil {
		s.logger.Error("启动注册失败", "error", err)
		s.abortStartLocked()
		return err
	}

	// ⑦ 错过触发点只 WARN，不补跑（约束 18）。
	s.warnMissedTriggersLocked(rows, now)

	// ⑧ 运行期基础 ctx：父 ctx 的取消交给 watcher，Stop 另持取消句柄（约束 24）。
	s.baseCtx, s.cancelBase = context.WithCancel(context.WithoutCancel(ctx))

	// ⑨ 启动引擎。
	s.engine.Start()
	s.started = true

	// ⑩ 信号 watcher：ctx 取消 → Stop 恰一次；Stop 完成后退出（无 goroutine 泄漏）。
	go s.watchStartContext(ctx)
	return nil
}

// Stop 执行图 F：停引擎 → 有界等待 → 取消在途 → 再等待；幂等且并发安全。
// Stop implements the bounded shutdown sequence; it is idempotent and safe to call
// concurrently.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(s.stop)
}

// stop 是 Stop 的一次性实体。
// stop is the once-only body of Stop.
func (s *Scheduler) stop() {
	s.stateMu.Lock()
	s.stopped = true
	wasStarted := s.started
	// 停摆位落下后，已派发的旧回调一律因代次不匹配而返回（约束 44）。
	for taskID := range s.handles {
		s.nextGen++
		s.gens[taskID] = s.nextGen
	}
	s.handles = make(map[string]CronHandle)
	s.stateMu.Unlock()

	defer close(s.done)
	if !wasStarted {
		return
	}

	grace := s.engine.Stop() // 不再触发新任务；返回"在途作业完成"信号
	graceTimer := time.NewTimer(s.stopGrace)
	defer graceTimer.Stop()
	select {
	case <-grace.Done():
		return
	case <-graceTimer.C:
	}

	if s.cancelBase != nil {
		s.cancelBase() // 宽限耗尽：取消全部在途运行
	}
	waitTimer := time.NewTimer(schedulerStopWait)
	defer waitTimer.Stop()
	select {
	case <-grace.Done():
		return
	case <-waitTimer.C:
		// 安全优先于退出速度：记 ERROR 后继续等待（约束 23）。
		s.logger.Error("调度器停机超时，继续等待在途运行")
		<-grace.Done()
	}
}

// watchStartContext 监听启动 ctx 的取消；Stop 完成后退出。
// watchStartContext stops the scheduler when the start context is cancelled and exits
// once Stop completes.
func (s *Scheduler) watchStartContext(ctx context.Context) {
	select {
	case <-ctx.Done():
		s.Stop()
	case <-s.done:
	}
}

// fire 是 cron 回调入口：读锁内校验代次并取库中任务 → 每任务锁 → 唯一执行链。
// gen 是注册时捕获的代次；旧句柄派发的回调因代次不匹配直接返回（约束 44 / 图 C）。
// fire is the cron callback entry: verify the generation and snapshot the task under the
// read lock, then take the per-task lock and run the shared execution chain.
func (s *Scheduler) fire(taskID string, gen uint64) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error(schedulerPanicLog,
				"task_id", taskID, "trigger", string(TriggerCron), "panic", fmt.Sprint(r))
		}
	}()

	task, baseCtx, ok := s.snapshotFireTask(taskID, gen)
	if !ok {
		return
	}

	lock := s.lockFor(taskID)
	if !lock.TryLock() {
		s.recordLockMiss(baseCtx, task)
		return
	}
	defer lock.Unlock()

	runCtx, cancel := context.WithTimeout(baseCtx, s.runTimeout)
	defer cancel()
	// 失败类别与终态已由 scheduleRun 内的 classifyRun / finishRun 落库并记 WARN/ERROR；
	// cron 回调没有返回通道，这里只补一条 Debug 痕迹（终态日志不重复）。
	// Failure classes and terminal rows are already persisted and logged inside
	// scheduleRun; the cron callback has no caller, so the error stops here with a
	// debug trace only.
	if _, _, err := s.scheduleRun(runCtx, task, TriggerCron); err != nil {
		s.logger.Debug("cron 运行返回错误（终态已落账）",
			"task_id", task.TaskID, "agent", task.Agent, "trigger", string(TriggerCron), "error", err)
	}
}

// snapshotFireTask 在读锁内完成「代次校验 + 读库」，两者不得分两次取锁（约束 44 / P-06）。
// 读锁在独立作用域内释放；运行阶段不持锁。
// snapshotFireTask verifies the generation and reads the task in one read-lock critical
// section; the lock is released before any run starts.
func (s *Scheduler) snapshotFireTask(taskID string, gen uint64) (TaskRecord, context.Context, bool) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	if s.gens[taskID] != gen {
		s.logger.Debug("触发代次已失效", "task_id", taskID)
		return TaskRecord{}, nil, false
	}
	baseCtx := s.baseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	rows, err := s.store.ListTasks(baseCtx)
	if err != nil {
		s.logger.Error("读取任务定义失败", "task_id", taskID, "trigger", string(TriggerCron), "error", err)
		return TaskRecord{}, nil, false
	}
	for _, row := range rows {
		// 可见性走共用判定，fire 额外要求 enabled 为真（管理读面不要求，注 7）。
		if row.TaskID != taskID || !s.visible(row) || !row.Enabled {
			continue
		}
		return row, baseCtx, true
	}
	s.logger.Debug("任务不可见或已停用，本轮跳过", "task_id", taskID)
	return TaskRecord{}, nil, false
}

// recordLockMiss 记录 cron 撞锁：独立插入 skipped 行（无 started 前驱、不触碰
// last_*），WARN 后返回；不排队、不产生 409（约束 7 / 注 6）。
// recordLockMiss inserts a skipped row for a cron trigger that hit an in-flight run.
func (s *Scheduler) recordLockMiss(ctx context.Context, task TaskRecord) {
	now := s.now()
	rec := ExecutionRecord{
		TaskID:     task.TaskID,
		Status:     ExecutionSkipped,
		Trigger:    TriggerCron,
		StartedAt:  now,
		FinishedAt: now,
	}
	if err := s.store.RecordSkipped(ctx, rec); err != nil {
		s.logger.Error("记录跳过执行失败", append(s.runAttrs(task, "", TriggerCron), "error", err)...)
	}
	s.logger.Warn(schedulerLockMissMessage, s.runAttrs(task, "", TriggerCron)...)
}

// scheduleRun 是到点与手动补跑**共用**的唯一执行函数：不取锁、不派生超时
// （运行 ctx 的时限由调用方准备），只做 StartExecution → MarkTaskRun → Run → FinishExecution。
// scheduleRun is the single execution chain shared by cron and manual triggers; it takes
// no lock and derives no timeout.
func (s *Scheduler) scheduleRun(ctx context.Context, task TaskRecord, trigger Trigger) (req RunRequest, res RunResult, err error) {
	now := s.now()
	runID := NewRunID("scheduler", now, runSuffix())
	s.logger.Info(schedulerTriggerLog, append(s.runAttrs(task, runID, trigger), "message_bytes", len(task.Message))...)

	execID, startErr := s.store.StartExecution(ctx, ExecutionRecord{
		TaskID:    task.TaskID,
		RunID:     runID,
		Status:    ExecutionStarted,
		Trigger:   trigger,
		StartedAt: now,
	})
	if startErr != nil {
		// fail closed：先留痕再运行；执行行写不进去就不发起运行（约束 9）。
		s.logger.Error("执行行写入失败，本轮不发起运行",
			append(s.runAttrs(task, runID, trigger), "error", startErr)...)
		return RunRequest{}, RunResult{}, startErr
	}

	// 代投的运行期固定目标：在运行前确定，panic 收尾也要据此把预写行改判。
	var deliveryIntent DeliveryIntent
	deliveryPrepared := false

	// panic 兜底：运行 panic 必须吸收，并把已预写的执行行收尾为 failed（约束 11）；
	// 已预写的投递行同样收尾（运行未成功 → 跳过）。
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error(schedulerPanicLog, append(s.runAttrs(task, runID, trigger), "panic", fmt.Sprint(r))...)
			s.finishRun(ctx, execID, task, runID, trigger, ExecutionFailed,
				fmt.Sprintf("运行 panic：%v", r), now)
			s.settleDelivery(ctx, task, deliveryIntent, deliveryPrepared, trigger,
				ExecutionFailed, RunResult{RunID: runID, Status: StatusFailed})
			res = RunResult{RunID: runID, Status: StatusFailed}
			err = fmt.Errorf("scheduler: run panic: %v", r)
		}
	}()

	// 尝试开始（准入失败也保留 last_run_at）；失败只记 WARN（图 C）。
	if markErr := s.store.MarkTaskRun(ctx, task.TaskID, now, ExecutionStarted); markErr != nil {
		s.logger.Warn("记录任务运行痕迹失败",
			append(s.runAttrs(task, runID, trigger), "error", markErr)...)
	}

	// R7：配置了系统代投的任务，运行前必须先落投递预写行并复核渠道资格；任一步失败
	// 本轮不发起运行——没有记录就不投递，也不允许"先跑完再补记录"。
	if task.DeliverTo != "" {
		intent, prepErr := s.prepareDelivery(ctx, task, runID)
		if prepErr != nil {
			s.logger.Error("代投前置检查失败，本轮不发起运行",
				append(s.runAttrs(task, runID, trigger), "error", prepErr)...)
			s.finishRun(ctx, execID, task, runID, trigger, ExecutionFailed, prepErr.Error(), now)
			return RunRequest{}, RunResult{}, prepErr
		}
		deliveryIntent = intent
		deliveryPrepared = true
	}

	req = RunRequest{
		RunID:     runID,
		Agent:     task.Agent,
		SessionID: DeriveSessionID("scheduler", "scheduler", task.Agent),
		Message:   task.Message,
		Channel:   "scheduler",
		UserID:    "scheduler",
		Trigger:   trigger,
		Budget:    s.budget,
		// 系统代投接管投递：本 Run 不向模型提供 `notify`，授权层同时固定为 none。
		SuppressNotify: deliveryPrepared,
	}
	res, err = s.invoker.Run(ctx, req)

	status, errMsg := s.classifyRun(task, runID, trigger, res, err)
	s.finishRun(ctx, execID, task, runID, trigger, status, errMsg, now)
	s.settleDelivery(ctx, task, deliveryIntent, deliveryPrepared, trigger, status, res)
	return req, res, err
}

// prepareDelivery 校验代投目标并写 pending 行：投递服务未装配、UID 非法、渠道资格
// 复核失败都返回错误，由调用方终止本轮运行。
// prepareDelivery validates the target and writes the pending row; any failure aborts
// the run before the model is called.
func (s *Scheduler) prepareDelivery(ctx context.Context, task TaskRecord, runID string) (DeliveryIntent, error) {
	if s.delivery == nil {
		return DeliveryIntent{}, errors.New("任务配置了系统代投，但本实例未装配投递服务（装配错误）")
	}
	channelUID, uidErr := ParseResourceUID(task.DeliverTo)
	if uidErr != nil {
		return DeliveryIntent{}, fmt.Errorf("任务 %s 的代投目标不是合法的渠道资源 UID: %w",
			task.TaskID, uidErr)
	}
	intent := DeliveryIntent{
		RunID:      runID,
		TaskID:     task.TaskID,
		Agent:      task.Agent,
		ChannelUID: channelUID,
	}
	if err := s.delivery.Prepare(ctx, intent); err != nil {
		return DeliveryIntent{}, fmt.Errorf("系统代投预写失败: %w", err)
	}
	return intent, nil
}

// settleDelivery 在运行收尾后处理已预写的投递行：运行成功才发送，其余一律标跳过。
// 发送用独立的 15s ctx（不继承可能已超时的运行 ctx），结果三分如实落库与记日志；
// 未知**不**触发自动重发。
// settleDelivery delivers only after a successful run and marks everything else skipped.
// The send uses its own 15s context, and an unknown outcome never triggers a resend.
func (s *Scheduler) settleDelivery(ctx context.Context, task TaskRecord, intent DeliveryIntent,
	prepared bool, trigger Trigger, status ExecutionStatus, res RunResult,
) {
	if !prepared || s.delivery == nil {
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schedulerDeliveryTimeout)
	defer cancel()

	attrs := s.runAttrs(task, intent.RunID, trigger)
	if status != ExecutionCompleted {
		if err := s.delivery.Skip(finishCtx, intent, "运行未成功，未投递"); err != nil {
			s.logger.Error("投递跳过写库失败（记录保持 pending，启动恢复改判）",
				append(attrs, "error", err)...)
		}
		return
	}

	outcome := s.delivery.Deliver(finishCtx, intent, deliveryContent(task, res))
	attrs = append(attrs, "delivery_status", string(outcome.Status), "detail", outcome.Detail)
	switch outcome.Status {
	case DeliverySent:
		s.logger.Info("系统代投完成", attrs...)
	case DeliveryFailed:
		s.logger.Warn("系统代投失败", attrs...)
	default:
		// 未知需要人工核对：记录里保留可查证的事实，**不**盲目重发（R7）。
		s.logger.Error("系统代投结果未知（不自动重发）", attrs...)
	}
}

// deliveryContent 组装推送正文：点名 Agent 与任务，正文是模型本轮的最终回答。
// 无文本（例如达迭代上限）时给固定说明，不把空消息发出去。
// deliveryContent names the agent and task and carries the run's final text verbatim.
func deliveryContent(task TaskRecord, res RunResult) string {
	text := strings.TrimSpace(res.FinalText)
	if text == "" {
		text = "（本轮未产生文本回答）"
	}
	return fmt.Sprintf("【%s】定时任务 %s 执行完成\n\n%s", task.Agent, task.TaskID, text)
}

// classifyRun 把运行结果映射为执行行终态（约束 10 / D-18 / 约束 43）。
// classifyRun maps one run outcome onto the execution-row terminal state.
func (s *Scheduler) classifyRun(task TaskRecord, runID string, trigger Trigger, res RunResult, runErr error) (ExecutionStatus, string) {
	if runErr != nil {
		var busy *SessionBusyError
		var archived *SessionArchivedError
		switch {
		case errors.As(runErr, &busy):
			s.logger.Warn(schedulerBusyMessage, append(s.runAttrs(task, runID, trigger), "error", runErr)...)
			return ExecutionSkipped, schedulerBusyMessage
		case errors.As(runErr, &archived):
			// 不重建会话：归档互斥由 AgentService 判定（约束 40 / 43）。
			s.logger.Warn(schedulerArchivedMessage, append(s.runAttrs(task, runID, trigger), "error", runErr)...)
			return ExecutionSkipped, schedulerArchivedMessage
		default:
			s.logger.Error(schedulerFailLog, append(s.runAttrs(task, runID, trigger), "error", runErr)...)
			return ExecutionFailed, runErr.Error()
		}
	}

	switch res.Status {
	case StatusCompleted, StatusLimitReached:
		return ExecutionCompleted, ""
	case StatusFailed, StatusCancelled:
		s.logger.Error(schedulerFailLog,
			append(s.runAttrs(task, runID, trigger), "status", string(res.Status), "reason", res.Reason)...)
		return ExecutionFailed, res.Reason
	case StatusRunning:
		// 运行面不应在返回时仍是 running；按失败收尾，不掩盖异常（约束 10）。
		s.logger.Error(schedulerFailLog,
			append(s.runAttrs(task, runID, trigger), "status", string(res.Status))...)
		return ExecutionFailed, "调度运行未在时限内结束"
	default:
		s.logger.Error(schedulerFailLog,
			append(s.runAttrs(task, runID, trigger), "status", string(res.Status))...)
		return ExecutionFailed, fmt.Sprintf("未知运行状态 %q", res.Status)
	}
}

// finishRun 用 context.WithoutCancel 派生的 5s 清理 ctx 写终态：运行 ctx 已超时或
// 被 Stop 取消时，终态写仍必须成功（约束 11 / 图 C）。失败只记 ERROR，行留 started
// 待下次启动恢复。
// finishRun writes the terminal row with a 5s cleanup context that survives a cancelled
// run context.
func (s *Scheduler) finishRun(ctx context.Context, execID int64, task TaskRecord, runID string, trigger Trigger, status ExecutionStatus, errMsg string, startedAt time.Time) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schedulerFinishTimeout)
	defer cancel()
	duration := s.now().Sub(startedAt)
	if err := s.store.FinishExecution(finishCtx, execID, status, duration, errMsg, s.now()); err != nil {
		s.logger.Error("执行行收尾失败", append(s.runAttrs(task, runID, trigger), "error", err)...)
	}
}

// lockFor 取任务锁；锁表由 sync.Map 承载，不同任务的并发触发互不串行（P-06）。
// lockFor returns the per-task mutex; sync.Map keeps concurrent distinct tasks safe.
func (s *Scheduler) lockFor(taskID string) *sync.Mutex {
	value, _ := s.locks.LoadOrStore(taskID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// List 返回管理读面的任务列表：每次读库 + 共用可见性过滤，不在进程内缓存任务状态
// （约束 3 / 17）；输出保持 SQL 的 task_id 升序。
// List reads the store on every call and returns the visible rows in task_id order.
func (s *Scheduler) List(ctx context.Context) ([]TaskRecord, error) {
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list tasks: %w", err)
	}
	visible := make([]TaskRecord, 0, len(rows))
	for _, row := range rows {
		if s.visible(row) {
			visible = append(visible, row)
		}
	}
	return visible, nil
}

// Executions 返回任务执行历史：先过共用可见性判定（未知 / retired / 未注册 Agent →
// fs.ErrNotExist），再把 limit 原样透传给仓储（约束 33 / 38）。
// Executions gates the history behind the shared visibility rule and forwards the limit.
func (s *Scheduler) Executions(ctx context.Context, taskID string, limit int) ([]ExecutionRecord, error) {
	if _, err := s.lookupVisible(ctx, taskID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListExecutions(ctx, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list executions of task %q: %w", taskID, err)
	}
	return rows, nil
}

// SetEnabled 在 stateMu 写锁内执行「判定 → 写库 → 动引擎」（P-06 / 注 2）：先按共用
// 可见性读一次库，再幂等短路，库写成功后停用侧先失效代次再停句柄、启用侧先摘旧句柄
// 再注册；库写失败一律不动引擎，库值不回滚。
// SetEnabled runs visibility, the DB write and the engine action in one write-lock
// critical section; a DB failure never touches the engine and is never rolled back.
func (s *Scheduler) SetEnabled(ctx context.Context, taskID string, enabled bool) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("启停判定读取任务失败", "task_id", taskID, "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}
	task, ok := s.visibleRow(rows, taskID)
	if !ok {
		s.logger.Debug("任务不可见，启停请求拒绝", "task_id", taskID)
		return fmt.Errorf("scheduler: task %q: %w", taskID, fs.ErrNotExist)
	}

	// 幂等短路：目标值与库值相同且句柄态一致时零写 SQL、零引擎动作（不增代次）。
	// Idempotent short-circuit: DB value and handle state already match the target.
	if task.Enabled == enabled && (s.handles[taskID] != nil) == enabled {
		return nil
	}

	if err := s.store.SetTaskEnabled(ctx, taskID, enabled, s.now()); err != nil {
		// 并发下线由 SQL 的 retired_at 谓词兜底（RowsAffected()==0 → fs.ErrNotExist）。
		s.logger.Error("任务启停写库失败", "task_id", taskID, "enabled", enabled, "error", err)
		return fmt.Errorf("scheduler: set task %q enabled=%t: %w", taskID, enabled, err)
	}

	if !enabled {
		// 停用侧：先自增代次使已派发旧回调失效，再停句柄（约束 44）。
		s.invalidateLocked(map[string]bool{taskID: true})
		s.logger.Info("定时任务已停用", "task_id", taskID, "agent", task.Agent)
		return nil
	}

	// 停摆位落下后到达的管理写只写库、不注册句柄；重启按库态重建。
	if !s.started || s.stopped {
		s.logger.Debug("调度器未运行，启用只写库、不注册", "task_id", taskID, "agent", task.Agent)
		return nil
	}

	// 启用侧：已有旧句柄先停并移除（不叠加），再写入新代次并注册一次。
	if handle, ok := s.handles[taskID]; ok {
		handle.Stop()
		delete(s.handles, taskID)
	}
	if err := s.registerTaskLocked(task); err != nil {
		s.logger.Error("任务注册失败，库值不回滚", "task_id", taskID, "agent", task.Agent, "error", err)
		return err
	}
	s.logger.Info("定时任务已启用", "task_id", taskID, "agent", task.Agent)
	return nil
}

// RunNow 执行手动补跑：先过共用可见性判定，再以 TryLock 抢任务锁（撞锁 → ErrTaskBusy，
// 不排队、不写执行行），最后交给与 cron 共用的 scheduleRun（约束 1 / 8 / 13）。
// 运行 ctx 的时限由调用方准备（web handler 传 60s），调度器不另行派生。
// RunNow runs the shared execution chain under a TryLock; the caller owns the deadline.
func (s *Scheduler) RunNow(ctx context.Context, taskID string) (RunRequest, RunResult, error) {
	task, err := s.lookupVisible(ctx, taskID)
	if err != nil {
		return RunRequest{}, RunResult{}, err
	}
	lock := s.lockFor(taskID)
	if !lock.TryLock() {
		return RunRequest{}, RunResult{}, fmt.Errorf("scheduler: task %q: %w", taskID, ErrTaskBusy)
	}
	defer lock.Unlock()
	return s.scheduleRun(ctx, task, TriggerManual)
}

// visible 是 List / Executions / SetEnabled / RunNow / fire 共用的可见性判定：
// retired 行与构造期未注册 Agent 的任务一律隐藏（§3.1.2）。
// visible is the shared visibility rule: retired rows and unregistered agents are hidden.
func (s *Scheduler) visible(row TaskRecord) bool {
	return row.RetiredAt.IsZero() && s.registry[row.Agent]
}

// visibleRow 在同一次读库结果里定位可见任务；不可见返回 false。
// visibleRow locates a visible task inside one ListTasks result.
func (s *Scheduler) visibleRow(rows []TaskRecord, taskID string) (TaskRecord, bool) {
	for _, row := range rows {
		if row.TaskID == taskID && s.visible(row) {
			return row, true
		}
	}
	return TaskRecord{}, false
}

// lookupVisible 读一次库并返回可见任务；不可见一律包装 fs.ErrNotExist（web 404）。
// lookupVisible reads the store once and wraps fs.ErrNotExist for hidden tasks.
func (s *Scheduler) lookupVisible(ctx context.Context, taskID string) (TaskRecord, error) {
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("读取任务失败", "task_id", taskID, "error", err)
		return TaskRecord{}, fmt.Errorf("scheduler: list tasks: %w", err)
	}
	if task, ok := s.visibleRow(rows, taskID); ok {
		return task, nil
	}
	s.logger.Debug("任务不可见，请求拒绝", "task_id", taskID)
	return TaskRecord{}, fmt.Errorf("scheduler: task %q: %w", taskID, fs.ErrNotExist)
}

// CheckOwnership 是写入门**落盘之前**的归属预检（约束 30 / 42）：读一次 ListTasks，
// 任一候选 id 在库中归属于**其他** agent_name（含 retired 行）→ ErrTaskOwnerConflict；
// 无行或同 owner 一律放行（同 Agent 的 retired 行由随后的 ReconcileAgent 复活）。
// 只读、不取 stateMu（锁序约束 39）、不写库、不动引擎——最终一致性由 ApplyAgentTasks
// 的归属守卫兜底。
// CheckOwnership runs the pre-write ownership check: any candidate id owned by another
// agent (retired or not) is a conflict. It only reads, takes no state lock, and leaves
// the transactional guard as the final backstop.
func (s *Scheduler) CheckOwnership(ctx context.Context, agent string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("归属预检读取任务失败", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}
	owners := make(map[string]string, len(rows))
	for _, row := range rows {
		owners[row.TaskID] = row.Agent
	}
	for _, id := range taskIDs {
		if owner, ok := owners[id]; ok && owner != agent {
			s.logger.Warn("任务 id 归属其他 Agent，拒绝写入", "task_id", id, "agent", agent, "owner", owner)
			return fmt.Errorf("scheduler: 任务 id %s 属于 Agent %s: %w", id, owner, ErrTaskOwnerConflict)
		}
	}
	return nil
}

// runAttrs 返回约束 36 的固定四字段。
// runAttrs renders the four fixed log fields of constraint 36.
func (s *Scheduler) runAttrs(task TaskRecord, runID string, trigger Trigger) []any {
	return []any{
		"task_id", task.TaskID,
		"agent", task.Agent,
		"run_id", runID,
		"trigger", string(trigger),
	}
}

// registeredAgentNames 返回已注册 Agent 名（排序保证对账顺序稳定）。
// registeredAgentNames returns the registered agent names in sorted order.
func (s *Scheduler) registeredAgentNames() []string {
	names := make([]string, 0, len(s.registry))
	for name, ok := range s.registry {
		if ok {
			names = append(names, name)
		}
	}
	sortStrings(names)
	return names
}

// sortStrings 是只依赖标准库的升序排序（避免为一行排序引入额外导入面）。
// sortStrings sorts in place with a minimal standard-library surface.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// commitAgentLocked 以单事务提交一个 Agent 的完整 upsert / retire 集合，返回提交后的
// 全量任务行与受影响任务；**不切换句柄**——提交成功后才由调用方注册（图 B 两阶段，
// 任一事务失败时已提交的 Agent 也不留半套句柄）。
// 失败时库与旧句柄完整保留（HIGH-2 / 约束 41）。
// commitAgentLocked applies one agent's full upsert/retire set in a single transaction
// without swapping any handle; the caller registers only after every commit succeeded.
func (s *Scheduler) commitAgentLocked(ctx context.Context, agent string, schedules []Schedule, rows []TaskRecord) ([]TaskRecord, map[string]bool, error) {
	now := s.now()
	derived := make(map[string]bool, len(schedules))
	upserts := make([]TaskRecord, 0, len(schedules))
	for _, schedule := range schedules {
		derived[schedule.ID] = true
		upserts = append(upserts, TaskRecord{
			TaskID:  schedule.ID,
			Agent:   agent,
			Cron:    schedule.Cron,
			Zone:    schedule.Zone,
			Message: schedule.Message,
			// 代投目标随定义落库：它是任务在 AGENT.md 里声明的一部分（R7），
			// 不随定义指纹参与变更提示，但**必须**写入库列——漏写会让每次对账
			// 把已登记的目标清空，任务照常运行却再也不投递。
			DeliverTo:      schedule.DeliverTo,
			DefinitionHash: definitionHash(agent, schedule),
		})
	}
	retireIDs := make([]string, 0)
	oldHash := make(map[string]string, len(rows))
	affected := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.Agent != agent {
			continue
		}
		affected[row.TaskID] = true
		oldHash[row.TaskID] = row.DefinitionHash
		if row.RetiredAt.IsZero() && !derived[row.TaskID] {
			retireIDs = append(retireIDs, row.TaskID)
		}
	}
	for _, upsert := range upserts {
		affected[upsert.TaskID] = true
	}

	if err := s.store.ApplyAgentTasks(ctx, agent, upserts, retireIDs, now); err != nil {
		return rows, nil, err
	}

	// 定义变化只记 INFO，不翻 enabled（注 14）。
	for _, upsert := range upserts {
		if old, ok := oldHash[upsert.TaskID]; ok && old != "" && old != upsert.DefinitionHash {
			s.logger.Info("任务定义已更新，enabled 保持库值",
				"task_id", upsert.TaskID, "agent", agent)
		}
	}

	fresh, err := s.store.ListTasks(ctx)
	if err != nil {
		return rows, nil, fmt.Errorf("scheduler: list tasks after reconcile: %w", err)
	}
	return fresh, affected, nil
}

// ReconcileAgent 在 chat / tool 落盘后把单个 Agent 的定义同步到库与引擎（图 E 尾段）。
// 未启动（chat / tool）→ DEBUG 后返回：文件已落盘，不落库、不注册，下次启动对账生效；
// 已启动 → 单事务提交，提交成功才切换句柄。运行期单任务 CronSpec / Add 失败不中断
// 其余任务，返回聚合错误（注 16 运行期口径）。
// ReconcileAgent syncs one agent's definitions after a successful file write; it is a
// no-op before Start and swaps handles only after the single transaction commits.
func (s *Scheduler) ReconcileAgent(ctx context.Context, agent string, tasks []Schedule) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if !s.started || s.stopped {
		s.logger.Debug("调度器未启动，定义不落库、不注册", "agent", agent)
		return nil
	}

	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("运行期对账读取任务失败", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}
	fresh, affected, err := s.commitAgentLocked(ctx, agent, tasks, rows)
	if err != nil {
		s.logger.Error("运行期对账失败，保留旧定义与旧句柄", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: reconcile agent %q: %w", agent, err)
	}

	// 提交成功：先为受影响任务自增代次（旧回调失效）并停旧句柄，再按库态重注册。
	s.invalidateLocked(affected)
	var errs []error
	for _, row := range fresh {
		if row.Agent != agent || !row.Enabled || !row.RetiredAt.IsZero() {
			continue
		}
		if err := s.registerTaskLocked(row); err != nil {
			s.logger.Error("任务注册失败，其余任务照常", "task_id", row.TaskID, "agent", agent, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SetRegistered 在运行期增减调度器的 Agent 注册集合（颗粒度文档 §3.1.24 第 5 行 /
// 约束 39）：注册集合不再构造期封死。加入只放开准入——任务定义经 ReconcileAgent 落库
// 与注册；移除则退休该 Agent 全部未退休任务（只置 retired_at、不删行，与启动对账
// 同口径）并停掉句柄，库读 / 写失败如实上抛，由装配层记 WARN 保留注册表撤销结果。
// SetRegistered adds or removes an agent at run time. Adding only opens admission—
// definitions still go through ReconcileAgent. Removing retires the agent's live tasks
// (rows kept, retired_at set) and stops their handles.
func (s *Scheduler) SetRegistered(agent string, registered bool) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if registered {
		s.registry[agent] = true
		return nil
	}
	if _, ok := s.registry[agent]; !ok {
		return nil
	}
	delete(s.registry, agent)
	if !s.started || s.stopped {
		return nil
	}

	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("移出注册集合时读取任务失败", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}
	var ids []string
	for _, row := range rows {
		if row.Agent == agent && row.RetiredAt.IsZero() {
			ids = append(ids, row.TaskID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if err := s.store.ApplyAgentTasks(ctx, agent, nil, ids, s.now()); err != nil {
		s.logger.Error("移出注册集合时退休任务失败", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: retire agent %q: %w", agent, err)
	}
	affected := make(map[string]bool, len(ids))
	for _, id := range ids {
		affected[id] = true
	}
	s.invalidateLocked(affected)
	s.logger.Info("Agent 已移出调度注册集合，任务退休", "agent", agent, "tasks", len(ids))
	return nil
}

// registerEnabledLocked 在启动路径为全部 enabled && !retired 的已注册任务注册句柄；
// 先失效受影响代次（旧回调一律返回），任一失败立即上抛，由调用方停引擎清句柄。
// registerEnabledLocked registers every enabled task of registered agents at startup;
// the first failure aborts, and the caller stops the engine.
func (s *Scheduler) registerEnabledLocked(rows []TaskRecord, affected map[string]bool) error {
	s.invalidateLocked(affected)
	for _, row := range rows {
		if !s.registry[row.Agent] || !row.Enabled || !row.RetiredAt.IsZero() {
			continue
		}
		if err := s.registerTaskLocked(row); err != nil {
			return err
		}
	}
	return nil
}

// retireRemovedAgentsLocked 为 definitions 中已无的 Agent 退休其全部未退休任务。
// retireRemovedAgentsLocked retires every active task of agents missing from definitions.
func (s *Scheduler) retireRemovedAgentsLocked(ctx context.Context, rows []TaskRecord, now time.Time, affected map[string]bool) error {
	byAgent := make(map[string][]string)
	for _, row := range rows {
		if _, known := s.definitions[row.Agent]; known || !row.RetiredAt.IsZero() {
			continue
		}
		byAgent[row.Agent] = append(byAgent[row.Agent], row.TaskID)
	}
	agents := make([]string, 0, len(byAgent))
	for agent := range byAgent {
		agents = append(agents, agent)
	}
	sortStrings(agents)
	for _, agent := range agents {
		ids := byAgent[agent]
		if err := s.store.ApplyAgentTasks(ctx, agent, nil, ids, now); err != nil {
			return fmt.Errorf("scheduler: retire agent %q: %w", agent, err)
		}
		for _, id := range ids {
			affected[id] = true
		}
	}
	return nil
}

// invalidateLocked 为受影响任务自增代次并停掉旧句柄（调用方持 stateMu 写锁）。
// invalidateLocked bumps generations and stops old handles; the caller holds the write lock.
func (s *Scheduler) invalidateLocked(affected map[string]bool) {
	for taskID := range affected {
		s.nextGen++
		s.gens[taskID] = s.nextGen
		if handle, ok := s.handles[taskID]; ok {
			handle.Stop()
			delete(s.handles, taskID)
		}
	}
}

// registerTaskLocked 先写入代次再 Add；失败只上抛，库值不回滚（调用方决定致命性）。
// registerTaskLocked writes the generation before Add; a failure is returned without
// rolling back the committed definitions.
func (s *Scheduler) registerTaskLocked(row TaskRecord) error {
	if s.cronSpec == nil {
		return errors.New("scheduler: CronSpec is required")
	}
	s.nextGen++
	gen := s.nextGen
	s.gens[row.TaskID] = gen

	schedule := Schedule{ID: row.TaskID, Cron: row.Cron, Zone: row.Zone, Message: row.Message}
	spec, err := s.cronSpec(schedule)
	if err != nil {
		return fmt.Errorf("scheduler: task %s: cron spec: %w", row.TaskID, err)
	}
	taskID := row.TaskID
	handle, err := s.engine.Add(spec, func() { s.fire(taskID, gen) })
	if err != nil {
		return fmt.Errorf("scheduler: task %s: register: %w", row.TaskID, err)
	}
	s.handles[taskID] = handle
	return nil
}

// abortStartLocked 处理启动路径的致命注册失败：停引擎、清句柄、失效代次；不置启动位。
// abortStartLocked stops the engine and clears handles after a fatal startup failure.
func (s *Scheduler) abortStartLocked() {
	s.engine.Stop()
	affected := make(map[string]bool, len(s.handles))
	for taskID := range s.handles {
		affected[taskID] = true
	}
	s.invalidateLocked(affected)
	s.handles = make(map[string]CronHandle)
}

// warnMissedTriggersLocked 枚举停机窗口内错过的触发点：只 WARN、不补跑（约束 18）。
// warnMissedTriggersLocked warns about trigger points missed while the process was down.
func (s *Scheduler) warnMissedTriggersLocked(rows []TaskRecord, now time.Time) {
	for _, row := range rows {
		if !row.Enabled || !row.RetiredAt.IsZero() || !s.registry[row.Agent] || row.LastRunAt.IsZero() {
			continue
		}
		schedule := Schedule{ID: row.TaskID, Cron: row.Cron, Zone: row.Zone, Message: row.Message}
		if s.cronSpec == nil {
			continue
		}
		spec, err := s.cronSpec(schedule)
		if err != nil {
			continue
		}
		missed := s.engine.Occurrences(spec, row.LastRunAt, now, schedulerMissedOccurrences)
		if len(missed) == 0 {
			continue
		}
		// 枚举达到上限即说明真实错过数至少为上限，日志必须明示「≥1000」（约束 38）。
		// Hitting the cap means the true count is at least the cap; say so explicitly.
		s.logger.Warn("错过调度触发点",
			"task_id", row.TaskID, "agent", row.Agent,
			"missed_count", len(missed),
			"missed_at_least", len(missed) >= schedulerMissedOccurrences,
			"last_missed_at", missed[len(missed)-1].UTC().Format(time.RFC3339))
	}
}

// definitionHash 按 agent、id、cron、zone、message 顺序以换行连接后取 sha256（注 14）。
// `deliver_to` **不在**这五字段指纹内：它是投放目标而不是任务定义，变更由库列与
// AGENT.md 承载，不触发"任务定义已更新"提示。
// definitionHash computes the documented five-field fingerprint; deliver_to stays out
// because it is a delivery target rather than part of the task definition.
func definitionHash(agent string, schedule Schedule) string {
	sum := sha256.Sum256([]byte(agent + "\n" + schedule.ID + "\n" + schedule.Cron + "\n" + schedule.Zone + "\n" + schedule.Message))
	return hex.EncodeToString(sum[:])
}
