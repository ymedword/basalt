package core

// 本文件是调度面到点触发链路（US-10 / 流程 C）的**白盒**验收测试：`fire` / `lockFor`
// 与代次表都是非导出件（§3.1.2 签名表），只有同包测试能直接驱动它们；`prompt_test.go`
// 是包内测试的先例。夹具全部自带（fake TaskStore / RunInvoker / CronEngine + 日志捕获 +
// 注入时钟），**不引用** `core_test` 外部包夹具，避免两个测试包互相依赖（模块卡 P-07）。
// This file is the white-box acceptance test of the scheduled-trigger path: fire, lockFor
// and the generation table are unexported, so only an in-package test can drive them.
// All fixtures are local (fake store / invoker / engine, log capture, injected clock).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 调度面夹具的固定字面量：Agent / 任务 / 6 段 cron / 消息。
// Fixed literals of the scheduler fixtures.
const (
	schedAgent    = "demo"
	schedTaskID   = "demo-1"
	schedTaskID2  = "demo-2"
	schedCron     = "*/5 * * * * *"
	schedMessage  = "巡检"
	schedSession  = "scheduler-26ab61a473f25490" // DeriveSessionID("scheduler","scheduler","demo") 固定向量
	schedAgentTwo = "demo"
)

// schedNow 是注入时钟的固定读数（2026-10-19T08:00:00Z）。
// schedNow is the fixed reading of the injected clock.
var schedNow = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

// demoTask 造一条默认任务定义。
// demoTask builds one default schedule definition.
func demoTask(id string) Schedule {
	return Schedule{ID: id, Cron: schedCron, Zone: "UTC", Message: schedMessage}
}

// demoEntries 把任务定义包成已注册的 Agent 条目。
// demoEntries wraps the schedules into one registered agent entry.
func demoEntries(schedules ...Schedule) []AgentEntry {
	return []AgentEntry{{
		Name:       schedAgent,
		Registered: true,
		Profile:    &Profile{Schedules: schedules},
	}}
}

// fireCurrent 按当前注册代次直接触发一次（fire 未导出；仅用于不经 fake engine 捕获
// job 的用例）。逐字取自模块卡 §4.3。
// fireCurrent triggers one run at the current registration generation.
func fireCurrent(t *testing.T, s *Scheduler, taskID string) {
	t.Helper()
	s.stateMu.RLock()
	gen, ok := s.gens[taskID]
	s.stateMu.RUnlock()
	require.True(t, ok, "任务 %s 未注册，无法取当前代次", taskID)
	s.fire(taskID, gen)
}

// currentGen 读取任务的当前注册代次（并发用例在主 goroutine 先取好再交给协程）。
// currentGen reads the current registration generation before spawning goroutines.
func currentGen(t *testing.T, s *Scheduler, taskID string) uint64 {
	t.Helper()
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	gen, ok := s.gens[taskID]
	require.True(t, ok, "任务 %s 未注册，无法取当前代次", taskID)
	return gen
}

// startSched 启动调度器并登记清理。
// startSched starts the scheduler and registers the cleanup hook.
func startSched(t *testing.T, s *Scheduler) {
	t.Helper()
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(s.Stop)
}

// newSchedulerWith 组装一个测试调度器：固定时钟、假引擎、可选参数覆盖。
// newSchedulerWith assembles a test scheduler with a fixed clock and a fake engine.
func newSchedulerWith(
	store TaskStore, invoker RunInvoker, engine CronEngine,
	logger *slog.Logger, schedules []Schedule, tune func(*SchedulerOptions),
) *Scheduler {
	return newSchedulerWithEntries(store, invoker, engine, logger, demoEntries(schedules...), tune)
}

// newSchedulerWithEntries 以显式 entries 组装调度器（多 Agent / 未注册项用例）。
// newSchedulerWithEntries assembles a scheduler over explicit entries.
func newSchedulerWithEntries(
	store TaskStore, invoker RunInvoker, engine CronEngine,
	logger *slog.Logger, entries []AgentEntry, tune func(*SchedulerOptions),
) *Scheduler {
	opts := SchedulerOptions{
		Store:      store,
		Invoker:    invoker,
		Engine:     engine,
		Entries:    entries,
		CronSpec:   func(s Schedule) (string, error) { return "CRON_TZ=" + s.Zone + " " + s.Cron, nil },
		Budget:     Budget{MaxIterations: 10},
		RunTimeout: time.Minute,
		StopGrace:  time.Minute,
		Logger:     logger,
		Now:        func() time.Time { return schedNow },
	}
	if tune != nil {
		tune(&opts)
	}
	return NewScheduler(opts)
}

// agentEntry 把任务定义包成指定注册态的 Agent 条目。
// agentEntry wraps schedules into one agent entry with the given registration state.
func agentEntry(name string, registered bool, schedules ...Schedule) AgentEntry {
	return AgentEntry{Name: name, Registered: registered, Profile: &Profile{Schedules: schedules}}
}

// appliedFor 过滤某个 Agent 的对账调用读数。
// appliedFor filters the reconcile readings of one agent.
func appliedFor(records []fakeAppliedRecord, agent string) []fakeAppliedRecord {
	out := make([]fakeAppliedRecord, 0, 1)
	for _, rec := range records {
		if rec.Agent == agent {
			out = append(out, rec)
		}
	}
	return out
}

// schedFixture 是一组默认夹具（假仓储 / 调用器 / 引擎 / 日志捕获 / 注入时钟）。
// schedFixture bundles the default fakes used by the fire-path tests.
type schedFixture struct {
	store   *fakeTaskStore
	invoker *countingInvoker
	engine  *fakeEngine
	logs    *captureState
	sched   *Scheduler
}

// newSchedFixture 构造默认夹具；tune 只允许改标量选项，不得替换三大夹具。
// newSchedFixture builds the default fixture; tune may only adjust scalar options.
func newSchedFixture(t *testing.T, schedules []Schedule, tune func(*SchedulerOptions)) *schedFixture {
	t.Helper()
	store := newFakeTaskStore()
	invoker := &countingInvoker{}
	engine := &fakeEngine{}
	logger, logs := newCaptureLogger()
	sched := newSchedulerWith(store, invoker, engine, logger, schedules, tune)
	return &schedFixture{store: store, invoker: invoker, engine: engine, logs: logs, sched: sched}
}

// start 启动夹具并登记清理。
func (f *schedFixture) start(t *testing.T) { t.Helper(); startSched(t, f.sched) }

// fire 以当前代次触发一次。
func (f *schedFixture) fire(t *testing.T, taskID string) { t.Helper(); fireCurrent(t, f.sched, taskID) }

// fakeTaskStore 是 TaskStore 的内存实现：模拟事务语义、终态行 run_id 置空规则与
// last_status 同步，并提供故障注入字段。
// fakeTaskStore is the in-memory TaskStore: it models the transaction, the run_id
// nulling rule and last_status syncing, and exposes failure-injection knobs.
type fakeTaskStore struct {
	mu           sync.Mutex
	tasks        map[string]TaskRecord
	skipped      []ExecutionRecord
	started      []ExecutionRecord
	finished     []ExecutionStatus
	finishCalls  []fakeFinishRecord
	marked       []fakeMarkRecord
	applied      []fakeAppliedRecord
	executions   map[string][]ExecutionRecord
	existingRuns map[string]bool

	listErr           error
	startExecutionErr error
	finishErr         error
	markErr           error
	recoverCount      int
	recoverErr        error
	applyCalls        int
	applyErr          error
	applyErrOnCall    int
	setEnabledErr     error
	setEnabledCalls   int
	listCalls         int
	nextExecID        int64

	finishCtxErr      error
	finishHasDeadline bool
	finishDeadlineAt  time.Time

	// applyHook / listHook 是对账切换与快照临界区用例的**受控阻塞点**：分别在一次
	// ApplyAgentTasks / ListTasks 内触发且只触发一次（sync.Once）。阻塞点位于 fake 自身
	// 互斥锁之前——否则对账会被仓储锁而不是调度器 stateMu 挡住，用例失去判别力。
	// applyHook / listHook block once inside the fake before taking its own lock, so the
	// barrier under test is the scheduler state lock rather than the store mutex.
	applyHook func()
	applyOnce sync.Once
	listHook  func()
	listOnce  sync.Once

	// recoveredRuns 是启动恢复用例预置的 Run 终态；恢复时按文档映射写成终态行。
	// recoveredRuns seeds run terminal statuses for the startup-recovery cases.
	recoveredRuns []RunStatus
	recoverCalls  int
	recoverNow    time.Time
}

// fakeFinishRecord 是一次 FinishExecution 的读数（run_id 按 runs 存在性规则解析）。
// fakeFinishRecord is one observed FinishExecution call.
type fakeFinishRecord struct {
	ID       int64
	Status   ExecutionStatus
	Duration time.Duration
	ErrMsg   string
	At       time.Time
	RunID    string
}

// fakeMarkRecord 是一次 MarkTaskRun 的读数。
type fakeMarkRecord struct {
	TaskID string
	At     time.Time
	Status ExecutionStatus
}

// fakeAppliedRecord 是一次 ApplyAgentTasks 的读数。
type fakeAppliedRecord struct {
	Agent     string
	UpsertIDs []string
	RetireIDs []string
}

// newFakeTaskStore 构造空仓储。
func newFakeTaskStore() *fakeTaskStore {
	return &fakeTaskStore{
		tasks:        make(map[string]TaskRecord),
		executions:   make(map[string][]ExecutionRecord),
		existingRuns: make(map[string]bool),
	}
}

// ListTasks 按 task_id 升序返回全部行（含 retired，由 core 过滤）。
func (s *fakeTaskStore) ListTasks(context.Context) ([]TaskRecord, error) {
	if s.listHook != nil {
		s.listOnce.Do(s.listHook)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]TaskRecord, 0, len(s.tasks))
	for _, rec := range s.tasks {
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b TaskRecord) int { return strings.Compare(a.TaskID, b.TaskID) })
	return out, nil
}

// ApplyAgentTasks 模拟单事务：先做归属预检，冲突即整体不生效；新行 enabled=true
// （schema 缺省 1），既有行保留 enabled / last_* / created_at。
func (s *fakeTaskStore) ApplyAgentTasks(_ context.Context, agent string, upserts []TaskRecord, retireIDs []string, now time.Time) error {
	if s.applyHook != nil {
		s.applyOnce.Do(s.applyHook)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyCalls++
	if s.applyErr != nil && (s.applyErrOnCall == 0 || s.applyCalls == s.applyErrOnCall) {
		return s.applyErr
	}
	for _, up := range upserts {
		if old, ok := s.tasks[up.TaskID]; ok && old.Agent != agent {
			return ErrTaskOwnerConflict
		}
	}
	for _, id := range retireIDs {
		if old, ok := s.tasks[id]; ok && old.Agent != agent {
			return ErrTaskOwnerConflict
		}
	}

	ids := make([]string, 0, len(upserts))
	for _, up := range upserts {
		ids = append(ids, up.TaskID)
		if old, ok := s.tasks[up.TaskID]; ok {
			old.Cron, old.Zone, old.Message = up.Cron, up.Zone, up.Message
			old.DeliverTo = up.DeliverTo
			old.DefinitionHash = up.DefinitionHash
			old.RetiredAt = time.Time{}
			old.UpdatedAt = now
			s.tasks[up.TaskID] = old
			continue
		}
		rec := up
		rec.Agent = agent
		rec.Enabled = true
		rec.CreatedAt, rec.UpdatedAt = now, now
		s.tasks[up.TaskID] = rec
	}
	for _, id := range retireIDs {
		if rec, ok := s.tasks[id]; ok && rec.RetiredAt.IsZero() {
			rec.RetiredAt = now
			rec.UpdatedAt = now
			s.tasks[id] = rec
		}
	}
	s.applied = append(s.applied, fakeAppliedRecord{
		Agent:     agent,
		UpsertIDs: ids,
		RetireIDs: append([]string(nil), retireIDs...),
	})
	return nil
}

// SetTaskEnabled 更新 enabled；行不存在或已退休返回 fs.ErrNotExist。
func (s *fakeTaskStore) SetTaskEnabled(_ context.Context, taskID string, enabled bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setEnabledCalls++
	if s.setEnabledErr != nil {
		return s.setEnabledErr
	}
	rec, ok := s.tasks[taskID]
	if !ok || !rec.RetiredAt.IsZero() {
		return fs.ErrNotExist
	}
	rec.Enabled = enabled
	rec.UpdatedAt = now
	s.tasks[taskID] = rec
	return nil
}

// MarkTaskRun 记录运行痕迹；失败仅返回错误（调用方只记 WARN）。
func (s *fakeTaskStore) MarkTaskRun(_ context.Context, taskID string, at time.Time, status ExecutionStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked = append(s.marked, fakeMarkRecord{TaskID: taskID, At: at, Status: status})
	if s.markErr != nil {
		return s.markErr
	}
	if rec, ok := s.tasks[taskID]; ok {
		rec.LastRunAt = at
		rec.LastStatus = status
		s.tasks[taskID] = rec
	}
	return nil
}

// StartExecution 预写 started 行并分配行号；故障注入时零写入。
func (s *fakeTaskStore) StartExecution(_ context.Context, rec ExecutionRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startExecutionErr != nil {
		return 0, s.startExecutionErr
	}
	s.nextExecID++
	rec.ID = s.nextExecID
	s.started = append(s.started, rec)
	return rec.ID, nil
}

// FinishExecution 收尾终态：按 runs 存在性置空 run_id，并同事务同步 last_status。
// FinishExecution closes one execution row and mirrors last_status in the same step.
func (s *fakeTaskStore) FinishExecution(ctx context.Context, id int64, status ExecutionStatus, duration time.Duration, errMsg string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishCtxErr = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		s.finishHasDeadline = true
		s.finishDeadlineAt = deadline
	}
	if s.finishErr != nil {
		return s.finishErr
	}
	s.finished = append(s.finished, status)

	var taskID, runID string
	for i := range s.started {
		if s.started[i].ID != id {
			continue
		}
		taskID = s.started[i].TaskID
		if s.existingRuns[s.started[i].RunID] {
			runID = s.started[i].RunID
		}
		break
	}
	s.finishCalls = append(s.finishCalls, fakeFinishRecord{
		ID: id, Status: status, Duration: duration, ErrMsg: errMsg, At: at, RunID: runID,
	})
	if rec, ok := s.tasks[taskID]; ok {
		rec.LastStatus = status
		rec.UpdatedAt = at
		s.tasks[taskID] = rec
	}
	return nil
}

// RecordSkipped 直接插入终态行；契约要求不触碰 last_*（本 fake 同样不碰）。
func (s *fakeTaskStore) RecordSkipped(_ context.Context, rec ExecutionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipped = append(s.skipped, rec)
	return nil
}

// RecoverStartedExecutions 记录恢复读数并按文档映射收尾预置的遗留行。
func (s *fakeTaskStore) RecoverStartedExecutions(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoverCalls++
	s.recoverNow = now
	if s.recoverErr != nil {
		return 0, s.recoverErr
	}
	count := s.recoverCount
	for _, status := range s.recoveredRuns {
		s.finished = append(s.finished, recoveredStatus(status))
		count++
	}
	return count, nil
}

// recoveredStatus 复刻文档中的 Run 终态 → 执行行终态映射；真实 SQL join 的权威用例是
// internal/storage 的 TestTaskStore_RecoverStartedExecutions（本文件只判调度器接线）。
func recoveredStatus(status RunStatus) ExecutionStatus {
	switch status {
	case StatusCompleted, StatusLimitReached:
		return ExecutionCompleted
	default:
		return ExecutionFailed
	}
}

// seedTask 预置一行任务定义（模拟库里既有定义）。
func (s *fakeTaskStore) seedTask(rec TaskRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[rec.TaskID] = rec
}

// recoveryReadings 返回恢复调用次数与收到的 now。
func (s *fakeTaskStore) recoveryReadings() (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverCalls, s.recoverNow
}

// ListExecutions 返回预置的执行历史（限条数）。
func (s *fakeTaskStore) ListExecutions(_ context.Context, taskID string, limit int) ([]ExecutionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.executions[taskID]
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return append([]ExecutionRecord(nil), rows...), nil
}

// task 返回任务行副本。
func (s *fakeTaskStore) task(taskID string) (TaskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[taskID]
	return rec, ok
}

// mutateTask 在锁内修改一行（模拟另一进程直接改库）。
func (s *fakeTaskStore) mutateTask(taskID string, fn func(*TaskRecord)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[taskID]
	if !ok {
		return
	}
	fn(&rec)
	s.tasks[taskID] = rec
}

// snapshotStarted 返回预写行的副本。
func (s *fakeTaskStore) snapshotStarted() []ExecutionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ExecutionRecord(nil), s.started...)
}

// snapshotSkipped 返回跳过的副本。
func (s *fakeTaskStore) snapshotSkipped() []ExecutionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ExecutionRecord(nil), s.skipped...)
}

// snapshotFinished 返回终态状态序列的副本。
func (s *fakeTaskStore) snapshotFinished() []ExecutionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ExecutionStatus(nil), s.finished...)
}

// snapshotFinishCalls 返回终态写读数。
func (s *fakeTaskStore) snapshotFinishCalls() []fakeFinishRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeFinishRecord(nil), s.finishCalls...)
}

// snapshotMarks 返回运行痕迹读数。
func (s *fakeTaskStore) snapshotMarks() []fakeMarkRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeMarkRecord(nil), s.marked...)
}

// snapshotApplied 返回对账调用读数。
func (s *fakeTaskStore) snapshotApplied() []fakeAppliedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeAppliedRecord(nil), s.applied...)
}

// listReadings 返回 ListTasks 的调用次数（可见性判定必须先读库，N-02）。
// listReadings returns the ListTasks call count; visibility checks must read the store.
func (s *fakeTaskStore) listReadings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls
}

// setEnabledReadings 返回 SetTaskEnabled 的调用次数（隐藏任务必须零写 SQL）。
// setEnabledReadings returns the SetTaskEnabled call count; hidden tasks must write nothing.
func (s *fakeTaskStore) setEnabledReadings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setEnabledCalls
}

// finishCtxError 返回最近一次终态写拿到的 ctx 错误。
func (s *fakeTaskStore) finishCtxError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishCtxErr
}

// finishDeadline 返回最近一次终态写 ctx 的截止时间。
func (s *fakeTaskStore) finishDeadline() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishDeadlineAt, s.finishHasDeadline
}

// ctxAwareTaskStore 在运行 ctx 已被取消时拒绝终态写（逆改点探针）：用于证明实现
// 必须用 context.WithoutCancel 派生清理 ctx。
// ctxAwareTaskStore rejects a terminal write when the run context is already cancelled.
type ctxAwareTaskStore struct {
	*fakeTaskStore
	rejectedFinishes int
}

// FinishExecution 先检查 ctx，再委托给内存实现。
func (s *ctxAwareTaskStore) FinishExecution(ctx context.Context, id int64, status ExecutionStatus, duration time.Duration, errMsg string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		s.mu.Lock()
		s.finishCtxErr = err
		s.rejectedFinishes++
		s.mu.Unlock()
		return err
	}
	return s.fakeTaskStore.FinishExecution(ctx, id, status, duration, errMsg, at)
}

// countingInvoker 记录调用次数与请求，并可注入运行函数。
// countingInvoker records calls and requests; run may be injected.
type countingInvoker struct {
	calls atomic.Int32
	mu    sync.Mutex
	reqs  []RunRequest
	run   func(context.Context, RunRequest) (RunResult, error)
}

// Run 计数后委托注入函数；缺省返回 completed。
func (i *countingInvoker) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	i.calls.Add(1)
	i.mu.Lock()
	i.reqs = append(i.reqs, req)
	run := i.run
	i.mu.Unlock()
	if run != nil {
		return run(ctx, req)
	}
	return RunResult{RunID: req.RunID, Status: StatusCompleted}, nil
}

// requests 返回请求副本。
func (i *countingInvoker) requests() []RunRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]RunRequest(nil), i.reqs...)
}

// fakeEngine 是 CronEngine 的内存实现：捕获 spec 与作业、记录启停次数。
// fakeEngine captures specs/jobs and counts Start/Stop calls.
type fakeEngine struct {
	mu              sync.Mutex
	specs           []string
	jobs            []func()
	handles         []*fakeHandle
	started         int
	stopped         int
	addCalls        int
	addErr          error
	addErrOnCall    int
	addErrFor       string
	occurrences     []time.Time
	occurrenceLimit int
	occurrenceCalls int
	stopCtx         context.Context
	stopCancel      context.CancelFunc
}

// Add 捕获注册；故障注入时返回错误且不记句柄。
func (e *fakeEngine) Add(spec string, job func()) (CronHandle, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addCalls++
	e.specs = append(e.specs, spec)
	if e.addErrFor != "" && spec == e.addErrFor {
		return nil, fmt.Errorf("fake engine: add %q: injected failure", spec)
	}
	if e.addErr != nil && (e.addErrOnCall == 0 || e.addCalls == e.addErrOnCall) {
		return nil, e.addErr
	}
	handle := &fakeHandle{engine: e, spec: spec}
	e.jobs = append(e.jobs, job)
	e.handles = append(e.handles, handle)
	return handle, nil
}

// Start 记录启动次数。
func (e *fakeEngine) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started++
}

// Stop 返回宽限 ctx；缺省已关闭，阻塞变体由 newBlockingFakeEngine 提供。
func (e *fakeEngine) Stop() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped++
	if e.stopCtx == nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.stopCtx = ctx
	}
	return e.stopCtx
}

// Occurrences 返回预置触发点并记录调用次数与上限。
func (e *fakeEngine) Occurrences(_ string, _, _ time.Time, limit int) []time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.occurrenceCalls++
	e.occurrenceLimit = limit
	return append([]time.Time(nil), e.occurrences...)
}

// releaseStop 释放阻塞变体的宽限 ctx（幂等）。
func (e *fakeEngine) releaseStop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopCancel != nil {
		e.stopCancel()
	}
}

// engineCounts 返回启停与注册次数。
func (e *fakeEngine) engineCounts() (started, stopped, addCalls int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started, e.stopped, e.addCalls
}

// liveHandleSpecs 返回未被摘除的句柄对应 spec（按注册顺序）。
func (e *fakeEngine) liveHandleSpecs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	specs := make([]string, 0, len(e.handles))
	for _, handle := range e.handles {
		if !handle.isStopped() {
			specs = append(specs, handle.spec)
		}
	}
	return specs
}

// liveHandles 返回未被摘除的句柄数。
func (e *fakeEngine) liveHandles() int { return len(e.liveHandleSpecs()) }

// lastJob 返回最近一次成功注册的 job（无则 nil）。
func (e *fakeEngine) lastJob() func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.jobs) == 0 {
		return nil
	}
	return e.jobs[len(e.jobs)-1]
}

// occurrenceReadings 返回 Occurrences 的调用次数与最近一次上限。
func (e *fakeEngine) occurrenceReadings() (calls, limit int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.occurrenceCalls, e.occurrenceLimit
}

// handleStops 返回被摘除的句柄数。
func (e *fakeEngine) handleStops() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, handle := range e.handles {
		if handle.isStopped() {
			n++
		}
	}
	return n
}

// newBlockingFakeEngine 构造宽限 ctx 未关闭的引擎（Stop 会等到 releaseStop）。
func newBlockingFakeEngine() *fakeEngine {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeEngine{stopCtx: ctx, stopCancel: cancel}
}

// fakeHandle 是一次注册的句柄；Stop 幂等。
type fakeHandle struct {
	engine *fakeEngine
	spec   string
	once   sync.Once
	off    bool
}

// Stop 摘除本句柄并记数。
func (h *fakeHandle) Stop() {
	h.once.Do(func() {
		h.engine.mu.Lock()
		h.off = true
		h.engine.mu.Unlock()
	})
}

// isStopped 读取摘除态（调用方持有 engine.mu）。
func (h *fakeHandle) isStopped() bool { return h.off }

// captureState 是日志捕获的共享存储。
type captureState struct {
	mu      sync.Mutex
	records []captureRecord
}

// captureRecord 是一条捕获的日志。
type captureRecord struct {
	Level slog.Level
	Msg   string
	Attrs []slog.Attr
}

// captureHandler 把 slog 记录收进内存（支持 WithAttrs 前缀）。
type captureHandler struct {
	state *captureState
	base  []slog.Attr
}

// newCaptureLogger 构造捕获 logger 与共享状态。
func newCaptureLogger() (*slog.Logger, *captureState) {
	state := &captureState{}
	return slog.New(&captureHandler{state: state}), state
}

// Enabled 放行全部级别。
func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle 收集记录（含 WithAttrs 前缀）。
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make([]slog.Attr, 0, len(h.base)+r.NumAttrs())
	attrs = append(attrs, h.base...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	h.state.mu.Lock()
	h.state.records = append(h.state.records, captureRecord{Level: r.Level, Msg: r.Message, Attrs: attrs})
	h.state.mu.Unlock()
	return nil
}

// WithAttrs 返回带前缀的处理器。
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	base := make([]slog.Attr, 0, len(h.base)+len(attrs))
	base = append(base, h.base...)
	base = append(base, attrs...)
	return &captureHandler{state: h.state, base: base}
}

// WithGroup 保持扁平结构。
func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// snapshot 返回记录副本。
func (s *captureState) snapshot() []captureRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]captureRecord(nil), s.records...)
}

// requireRecord 断言存在指定级别与逐字消息的记录。
func (s *captureState) requireRecord(t *testing.T, level slog.Level, msg string) captureRecord {
	t.Helper()
	records := s.snapshot()
	for _, rec := range records {
		if rec.Level == level && rec.Msg == msg {
			return rec
		}
	}
	require.FailNow(t, "未找到日志", "level=%s msg=%q（实际 %d 条）", level, msg, len(records))
	return captureRecord{}
}

// attrOf 取记录中的某个字段。
func attrOf(rec captureRecord, key string) (slog.Value, bool) {
	for _, a := range rec.Attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return slog.Value{}, false
}

// requireLogFields 断言日志含约束 36 的四个固定字段。
func requireLogFields(t *testing.T, rec captureRecord, wantRunID string) {
	t.Helper()
	want := map[string]string{
		"task_id": schedTaskID,
		"agent":   schedAgent,
		"run_id":  wantRunID,
		"trigger": string(TriggerCron),
	}
	for key, value := range want {
		got, ok := attrOf(rec, key)
		require.True(t, ok, "日志缺少字段 %s：%+v", key, rec.Attrs)
		require.Equal(t, value, got.String(), "字段 %s 的值不符", key)
	}
}

// TestScheduler_Fire_DerivesSessionAndRunID 覆盖 R5：同 Agent 的两个任务共用一个
// 固定会话标识；RunID 带 scheduler- 前缀且与执行行一致。
func TestScheduler_Fire_DerivesSessionAndRunID(t *testing.T) {
	t.Run("session id is the fixed scheduler vector", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)}, nil)
		f.start(t)
		f.fire(t, schedTaskID)
		f.fire(t, schedTaskID2)

		reqs := f.invoker.requests()
		require.Len(t, reqs, 2, "两轮触发都要发起运行")
		require.Equal(t, reqs[0].SessionID, reqs[1].SessionID, "同 Agent 的任务必须共用调度会话")
		require.Equal(t, schedSession, reqs[0].SessionID)
		require.Equal(t, "scheduler", reqs[0].Channel)
		require.Equal(t, "scheduler", reqs[0].UserID)
		require.Equal(t, TriggerCron, reqs[0].Trigger)
	})

	t.Run("run id carries the scheduler prefix", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		f.fire(t, schedTaskID)

		reqs := f.invoker.requests()
		require.Len(t, reqs, 1)
		require.True(t, strings.HasPrefix(reqs[0].RunID, "scheduler-"), "RunID 前缀必须是 scheduler-：%s", reqs[0].RunID)
		started := f.store.snapshotStarted()
		require.Len(t, started, 1)
		require.Equal(t, reqs[0].RunID, started[0].RunID, "执行行与运行请求必须是同一个 RunID")
	})
}

// TestScheduler_Fire_ExecutesAndRecords 覆盖 R8：completed / limit_reached 都落
// completed，且先写 started 执行行、再记 last_run_at。
func TestScheduler_Fire_ExecutesAndRecords(t *testing.T) {
	cases := []struct {
		name   string
		status RunStatus
		reason string
	}{
		{name: "completed", status: StatusCompleted},
		{name: "limit_reached maps to completed", status: StatusLimitReached, reason: "达到迭代上限"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
			f.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
				return RunResult{RunID: req.RunID, Status: tc.status, Reason: tc.reason}, nil
			}
			f.start(t)
			f.fire(t, schedTaskID)

			require.EqualValues(t, 1, f.invoker.calls.Load())
			started := f.store.snapshotStarted()
			require.Len(t, started, 1)
			require.Equal(t, ExecutionStarted, started[0].Status)
			require.Equal(t, TriggerCron, started[0].Trigger)
			require.Equal(t, schedNow, started[0].StartedAt)

			marks := f.store.snapshotMarks()
			require.Len(t, marks, 1)
			require.Equal(t, ExecutionStarted, marks[0].Status, "开始执行也要记 last_run_at")

			require.Equal(t, []ExecutionStatus{ExecutionCompleted}, f.store.snapshotFinished())
			rec, ok := f.store.task(schedTaskID)
			require.True(t, ok)
			require.Equal(t, ExecutionCompleted, rec.LastStatus)
			require.False(t, rec.LastRunAt.IsZero())
		})
	}
}

// TestScheduler_Fire_ReadsTaskFromStore 覆盖 R25：每轮读库；停用 / 读不到库 / 改定义
// 都按库行生效。
func TestScheduler_Fire_ReadsTaskFromStore(t *testing.T) {
	t.Run("disabled task does not run", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		f.store.mutateTask(schedTaskID, func(rec *TaskRecord) { rec.Enabled = false })

		f.fire(t, schedTaskID)

		require.Zero(t, f.invoker.calls.Load(), "停用任务不得发起运行")
		require.Empty(t, f.store.snapshotStarted())
		require.Empty(t, f.store.snapshotSkipped())
	})

	t.Run("list failure fails closed", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		f.store.mu.Lock()
		f.store.listErr = errors.New("database is locked")
		f.store.mu.Unlock()

		f.fire(t, schedTaskID)

		require.Zero(t, f.invoker.calls.Load(), "读不到库不得发起运行")
		require.Empty(t, f.store.snapshotStarted())
		require.Empty(t, f.store.snapshotSkipped())
		f.logs.requireRecord(t, slog.LevelError, "读取任务定义失败")
	})

	t.Run("definition change applies next trigger", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		f.fire(t, schedTaskID)
		const updated = "第二版巡检"
		f.store.mutateTask(schedTaskID, func(rec *TaskRecord) { rec.Message = updated })
		f.fire(t, schedTaskID)

		reqs := f.invoker.requests()
		require.Len(t, reqs, 2)
		require.Equal(t, schedMessage, reqs[0].Message)
		require.Equal(t, updated, reqs[1].Message, "下一轮必须使用库里的新定义")
	})
}

// TestScheduler_Fire_FailureMapping 覆盖 R8：failed / cancelled 都收尾为 failed。
func TestScheduler_Fire_FailureMapping(t *testing.T) {
	cases := []struct {
		name   string
		status RunStatus
		reason string
	}{
		{name: "failed", status: StatusFailed, reason: "模型调用失败"},
		{name: "cancelled maps to failed", status: StatusCancelled, reason: "运行被取消"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
			f.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
				return RunResult{RunID: req.RunID, Status: tc.status, Reason: tc.reason}, nil
			}
			f.start(t)
			f.fire(t, schedTaskID)

			require.Equal(t, []ExecutionStatus{ExecutionFailed}, f.store.snapshotFinished())
			calls := f.store.snapshotFinishCalls()
			require.Len(t, calls, 1)
			require.Equal(t, tc.reason, calls[0].ErrMsg)
		})
	}
}

// TestScheduler_Fire_AdmissionErrorRecordsFailed 覆盖 D-18 的反面：非 SessionBusy
// 准入错误记 failed，且进程继续处理下一次触发。
func TestScheduler_Fire_AdmissionErrorRecordsFailed(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	admission := &AgentNotFoundError{Name: schedAgent}
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, admission
	}
	f.start(t)
	f.fire(t, schedTaskID)

	require.Equal(t, []ExecutionStatus{ExecutionFailed}, f.store.snapshotFinished())
	calls := f.store.snapshotFinishCalls()
	require.Len(t, calls, 1)
	require.Equal(t, admission.Error(), calls[0].ErrMsg)

	f.invoker.mu.Lock()
	f.invoker.run = nil
	f.invoker.mu.Unlock()
	f.fire(t, schedTaskID)

	require.EqualValues(t, 2, f.invoker.calls.Load(), "准入失败不得带崩调度器")
	require.Equal(t, []ExecutionStatus{ExecutionFailed, ExecutionCompleted}, f.store.snapshotFinished())
}

// TestScheduler_Fire_SessionBusyRecordsSkipped 覆盖 R23 / D-18：兄弟任务占会话时落
// skipped + 固定文案 + run_id 置空，并同步 last_status。
func TestScheduler_Fire_SessionBusyRecordsSkipped(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, &SessionBusyError{SessionID: schedSession}
	}
	f.start(t)
	f.fire(t, schedTaskID)

	require.Equal(t, []ExecutionStatus{ExecutionSkipped}, f.store.snapshotFinished())
	calls := f.store.snapshotFinishCalls()
	require.Len(t, calls, 1)
	require.Equal(t, "同一 Agent 的另一任务仍在执行", calls[0].ErrMsg)
	require.Empty(t, calls[0].RunID, "无 Run 行的终态行 run_id 必须置空")
	f.logs.requireRecord(t, slog.LevelWarn, "同一 Agent 的另一任务仍在执行")

	rec, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	require.Equal(t, ExecutionSkipped, rec.LastStatus, "FinishExecution 必须同步 skipped")
	require.Equal(t, schedNow, rec.LastRunAt)
}

// TestScheduler_Fire_StartExecutionFailure 覆盖 R7：预写执行行失败即不发 Run（fail closed）。
func TestScheduler_Fire_StartExecutionFailure(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.mu.Lock()
	f.store.startExecutionErr = errors.New("insert failed")
	f.store.mu.Unlock()
	f.start(t)
	f.fire(t, schedTaskID)

	require.Zero(t, f.invoker.calls.Load(), "执行行预写失败不得发起运行")
	require.Empty(t, f.store.snapshotStarted())
	require.Empty(t, f.store.snapshotFinished())
	f.logs.requireRecord(t, slog.LevelError, "执行行写入失败，本轮不发起运行")
}

// TestScheduler_Fire_LockMissRecordsSkipped 覆盖 R6：cron 撞上在途执行记 skipped，
// 不排队、不产生 Run。逆改点：TryLock 改 Lock → 测试超时且零 skipped；RecordSkipped
// 换成 FinishExecution → skipped 断言红；撞锁时先发 Run → 调用计数断言红。
func TestScheduler_Fire_LockMissRecordsSkipped(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	f.sched.lockFor(schedTaskID).Lock() // 模拟在途
	f.fire(t, schedTaskID)              // cron 撞锁（按当前代次直接触发）
	f.sched.lockFor(schedTaskID).Unlock()

	require.Zero(t, f.invoker.calls.Load(), "撞锁不得发 Run")
	skipped := f.store.snapshotSkipped()
	require.Len(t, skipped, 1)
	require.Equal(t, ExecutionSkipped, skipped[0].Status)
	require.Equal(t, TriggerCron, skipped[0].Trigger)
	f.logs.requireRecord(t, slog.LevelWarn, "上一轮未结束，本轮跳过")
}

// TestScheduler_Fire_PanicRecovered 覆盖 R8：运行 panic 被吸收，执行行收尾 failed，
// 后续触发仍正常执行。
func TestScheduler_Fire_PanicRecovered(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	var n int32
	f.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			panic("boom")
		}
		return RunResult{RunID: req.RunID, Status: StatusCompleted}, nil
	}
	f.start(t)
	f.fire(t, schedTaskID)

	require.Equal(t, []ExecutionStatus{ExecutionFailed}, f.store.snapshotFinished())
	calls := f.store.snapshotFinishCalls()
	require.Len(t, calls, 1)
	require.Contains(t, calls[0].ErrMsg, "panic")
	f.logs.requireRecord(t, slog.LevelError, "调度运行 panic")

	f.fire(t, schedTaskID)
	require.EqualValues(t, 2, f.invoker.calls.Load(), "panic 不得带走调度器")
	require.Equal(t, []ExecutionStatus{ExecutionFailed, ExecutionCompleted}, f.store.snapshotFinished())
}

// TestScheduler_Fire_FinishUsesCleanupCtx 覆盖 R8 / 约束 11：运行 ctx 已超时或被
// Stop 取消时，终态写仍用 context.WithoutCancel 派生的约 5s 清理 ctx 成功。
// 逆改点：沿用运行 ctx → ctx 敏感的 fake store 拒绝写入，"终态已落 failed"断言红；
// 清理 ctx 不设 5s 超时在 Stop 场景会无限等待 → 用例超时红。
func TestScheduler_Fire_FinishUsesCleanupCtx(t *testing.T) {
	t.Run("run context timed out", func(t *testing.T) {
		store := &ctxAwareTaskStore{fakeTaskStore: newFakeTaskStore()}
		blocking := &countingInvoker{run: func(ctx context.Context, _ RunRequest) (RunResult, error) {
			<-ctx.Done()
			return RunResult{Status: StatusCancelled, Reason: "运行超时"}, nil
		}}
		logger, _ := newCaptureLogger()
		sched := newSchedulerWith(store, blocking, &fakeEngine{}, logger, []Schedule{demoTask(schedTaskID)}, func(o *SchedulerOptions) {
			o.RunTimeout = 10 * time.Millisecond
		})
		startSched(t, sched)

		// fireCurrent 同步执行到 finishRun 返回；逆改清理 ctx 时该断言直接红（不靠超时兜底）。
		fireCurrent(t, sched, schedTaskID)

		require.Equal(t, []ExecutionStatus{ExecutionFailed}, store.snapshotFinished(), "超时取消仍须把执行行收尾为 failed")
		require.NoError(t, store.finishCtxError(), "清理 ctx 不得已取消")
		deadline, ok := store.finishDeadline()
		require.True(t, ok, "清理 ctx 必须带截止时间")
		require.WithinDuration(t, time.Now().Add(5*time.Second), deadline, time.Second)
		require.Zero(t, store.rejectedFinishes)
	})

	t.Run("run context cancelled by stop", func(t *testing.T) {
		store := &ctxAwareTaskStore{fakeTaskStore: newFakeTaskStore()}
		entered := make(chan struct{}, 1)
		blocking := &countingInvoker{run: func(ctx context.Context, _ RunRequest) (RunResult, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return RunResult{Status: StatusCancelled, Reason: "停止取消"}, nil
		}}
		engine := newBlockingFakeEngine()
		defer engine.releaseStop()
		logger, _ := newCaptureLogger()
		sched := newSchedulerWith(store, blocking, engine, logger, []Schedule{demoTask(schedTaskID)}, func(o *SchedulerOptions) {
			o.RunTimeout = time.Minute
			o.StopGrace = 20 * time.Millisecond
		})
		require.NoError(t, sched.Start(context.Background()))

		gen := currentGen(t, sched, schedTaskID)
		fireDone := make(chan struct{})
		go func() {
			defer close(fireDone)
			sched.fire(schedTaskID, gen)
		}()
		<-entered

		stopDone := make(chan struct{})
		go func() {
			defer close(stopDone)
			sched.Stop()
		}()
		require.Eventually(t, func() bool { return len(store.snapshotFinished()) == 1 }, 2*time.Second, 10*time.Millisecond)
		engine.releaseStop()
		select {
		case <-stopDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Stop 未在宽限 + 有界清理内返回")
		}
		select {
		case <-fireDone:
		case <-time.After(2 * time.Second):
			t.Fatal("fire 未在 Stop 取消后返回")
		}

		require.Equal(t, []ExecutionStatus{ExecutionFailed}, store.snapshotFinished())
		require.NoError(t, store.finishCtxError(), "Stop 取消后清理 ctx 仍须可用")
		deadline, ok := store.finishDeadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(5*time.Second), deadline, time.Second)
		require.Zero(t, store.rejectedFinishes)
	})
}

// TestScheduler_Fire_ArchivedSessionSkipped 覆盖 R27 / 约束 43：cron 遇归档会话
// 记 skipped + 固定文案、不重建会话、同步 last_status。
func TestScheduler_Fire_ArchivedSessionSkipped(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, &SessionArchivedError{SessionID: schedSession}
	}
	f.start(t)
	f.fire(t, schedTaskID)

	require.Equal(t, []ExecutionStatus{ExecutionSkipped}, f.store.snapshotFinished())
	calls := f.store.snapshotFinishCalls()
	require.Len(t, calls, 1)
	require.Equal(t, "会话已归档，本轮跳过", calls[0].ErrMsg)
	require.Empty(t, calls[0].RunID, "无 Run 行的终态行 run_id 必须置空")
	f.logs.requireRecord(t, slog.LevelWarn, "会话已归档，本轮跳过")

	rec, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	require.Equal(t, ExecutionSkipped, rec.LastStatus)
	require.EqualValues(t, 1, f.invoker.calls.Load(), "归档会话只尝试一次，不得重建后再跑")
}

// TestScheduler_Fire_ConcurrentDistinctTasks 覆盖 R26：不同任务的并发触发互不串行，
// 且 -race 干净。逆改点：每任务锁改成普通 map/全局锁 → DATA RACE 或第二个触发被阻塞。
func TestScheduler_Fire_ConcurrentDistinctTasks(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)}, nil)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	f.invoker.run = func(ctx context.Context, req RunRequest) (RunResult, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return RunResult{RunID: req.RunID, Status: StatusCompleted}, nil
		case <-ctx.Done():
			return RunResult{RunID: req.RunID, Status: StatusCancelled}, ctx.Err()
		}
	}
	f.start(t)

	gen1 := currentGen(t, f.sched, schedTaskID)
	gen2 := currentGen(t, f.sched, schedTaskID2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); f.sched.fire(schedTaskID, gen1) }()
	go func() { defer wg.Done(); f.sched.fire(schedTaskID2, gen2) }()

	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("不同任务的并发触发被串行化")
		}
	}
	close(release)
	wg.Wait()

	require.EqualValues(t, 2, f.invoker.calls.Load())
	require.Equal(t, []ExecutionStatus{ExecutionCompleted, ExecutionCompleted}, f.store.snapshotFinished())
}

// TestScheduler_LogFieldsAndNoBody 覆盖约束 36：失败 / 跳过日志带固定四字段；
// 消息正文不得整段落日志，只记字节长度。逆改点：把正文整段打日志 → 断言红。
func TestScheduler_LogFieldsAndNoBody(t *testing.T) {
	const marker = "巡检正文-秘密-marker-42"
	f := newSchedFixture(t, []Schedule{{ID: schedTaskID, Cron: schedCron, Zone: "UTC", Message: marker}}, nil)
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, &AgentNotFoundError{Name: schedAgent}
	}
	f.start(t)
	f.fire(t, schedTaskID)

	failure := f.logs.requireRecord(t, slog.LevelError, "调度运行失败")
	started := f.store.snapshotStarted()
	require.Len(t, started, 1)
	require.NotEmpty(t, started[0].RunID, "预写执行行必须携带 RunID")
	runIDValue, ok := attrOf(failure, "run_id")
	require.True(t, ok, "失败日志必须带 run_id 字段")
	require.NotEmpty(t, runIDValue.String(), "失败路径必须已在日志里携带 RunID")
	requireLogFields(t, failure, runIDValue.String())

	// 撞锁跳过路径同样带四字段（run_id 为空）。
	f.sched.lockFor(schedTaskID).Lock()
	f.fire(t, schedTaskID)
	f.sched.lockFor(schedTaskID).Unlock()
	skip := f.logs.requireRecord(t, slog.LevelWarn, "上一轮未结束，本轮跳过")
	requireLogFields(t, skip, "")

	// 正文只允许以字节长度出现。
	trigger := f.logs.requireRecord(t, slog.LevelInfo, "调度触发")
	bytes, ok := attrOf(trigger, "message_bytes")
	require.True(t, ok, "触发日志必须只记消息字节长度")
	require.EqualValues(t, len(marker), bytes.Int64())

	for _, rec := range f.logs.snapshot() {
		require.NotContains(t, rec.Msg, marker, "日志消息不得包含正文")
		for _, a := range rec.Attrs {
			require.NotContains(t, a.Value.String(), marker, "日志字段不得包含正文")
		}
	}
}

// TestScheduler_StartRecoversExecutions 覆盖 R4 的调度器接线面：Start 在任何对账之前
// 调用**一次** RecoverStartedExecutions，使用注入时钟，并按 Run 终态收尾遗留行
// （completed / limit_reached → completed；failed / cancelled → failed）。
// 真实 SQL join 与 NULL 扫描由 internal/storage 的 TestTaskStore_RecoverStartedExecutions
// 判卷；本用例只判调度器的次序、时钟与四类终态的委托口径。
// 逆改点：Start 跳过恢复 → recoverCalls 为 0、终态行缺失，断言红。
func TestScheduler_StartRecoversExecutions(t *testing.T) {
	cases := []struct {
		name   string
		status RunStatus
		want   ExecutionStatus
	}{
		{name: "completed run recovers as completed", status: StatusCompleted, want: ExecutionCompleted},
		{name: "limit_reached run recovers as completed", status: StatusLimitReached, want: ExecutionCompleted},
		{name: "failed run recovers as failed", status: StatusFailed, want: ExecutionFailed},
		{name: "cancelled run recovers as failed", status: StatusCancelled, want: ExecutionFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t, nil, nil)
			f.store.recoveredRuns = []RunStatus{tc.status}
			f.start(t)

			calls, at := f.store.recoveryReadings()
			require.Equal(t, 1, calls, "启动必须执行且只执行一次遗留执行恢复")
			require.Equal(t, schedNow, at, "恢复必须使用注入时钟")
			require.Equal(t, []ExecutionStatus{tc.want}, f.store.snapshotFinished(),
				"遗留执行行必须按 Run 终态映射收尾")
			require.Zero(t, f.invoker.calls.Load(), "恢复遗留行不得发起运行")
		})
	}
}

// TestScheduler_StartReconcilesDefinitions 覆盖 R19 / R26 的启动对账口径：
// 新定义入库、定义更新不翻 enabled、退休行复活保持库值、未注册 Agent 不落库、
// 文件移除与目录移除都只退休不物理删除。
// 逆改点：upsert 时把 enabled 写回 1（注 14 违反）→ 更新子例红；对未注册 Agent 也
// 执行 ApplyAgentTasks → 未注册子例红；按"派生集合"而非 task_id 判定退休 → 退休子例红。
func TestScheduler_StartReconcilesDefinitions(t *testing.T) {
	t.Run("new definitions are upserted", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)

		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok, "文件中的新任务必须入库")
		require.Equal(t, schedAgent, rec.Agent)
		require.Equal(t, schedMessage, rec.Message)
		require.True(t, rec.Enabled, "新行按缺省 enabled=1")
	})

	t.Run("definition update keeps db enabled value", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{{ID: schedTaskID, Cron: schedCron, Zone: "UTC", Message: "新巡检"}}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: "旧巡检", Enabled: false, DefinitionHash: "old",
		})
		f.start(t)

		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.Equal(t, "新巡检", rec.Message, "定义列按文件更新")
		require.False(t, rec.Enabled, "定义更新不得翻动库中 enabled（注 14）")
	})

	t.Run("retired task revives keeping enabled", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: "旧巡检", Enabled: false, RetiredAt: schedNow.Add(-time.Hour),
		})
		f.start(t)

		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.True(t, rec.RetiredAt.IsZero(), "同 Agent 同 id 再现必须复活")
		require.False(t, rec.Enabled, "复活保持库值 enabled=false")
	})

	t.Run("unregistered agent definitions are not written", func(t *testing.T) {
		store := newFakeTaskStore()
		logger, _ := newCaptureLogger()
		entries := []AgentEntry{
			agentEntry(schedAgent, true, demoTask(schedTaskID)),
			agentEntry("broken", false, demoTask("broken-1")),
		}
		sched := newSchedulerWithEntries(store, &countingInvoker{}, &fakeEngine{}, logger, entries, nil)
		require.NoError(t, sched.Start(context.Background()))
		t.Cleanup(sched.Stop)

		_, ok := store.task("broken-1")
		require.False(t, ok, "未注册 Agent 的定义不得入库（注 10）")
		require.Empty(t, appliedFor(store.snapshotApplied(), "broken"), "未注册 Agent 不遍历、不对账")
	})

	t.Run("tasks missing from file are retired", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID2, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: "第二版", Enabled: true,
		})
		f.start(t)

		kept, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.True(t, kept.RetiredAt.IsZero(), "文件仍声明的任务不得退休")
		gone, ok := f.store.task(schedTaskID2)
		require.True(t, ok)
		require.Equal(t, schedNow, gone.RetiredAt, "keep 是 task_id 集合：文件删掉的任务必须退休（P-02）")
	})

	t.Run("agents absent from entries are retired", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: "ghost-1", Agent: "ghost", Cron: schedCron, Zone: "UTC",
			Message: "残留", Enabled: true,
		})
		f.start(t)

		ghost, ok := f.store.task("ghost-1")
		require.True(t, ok)
		require.Equal(t, schedNow, ghost.RetiredAt, "目录被整个删除的 Agent，其库行必须退休（F9）")
		applied := appliedFor(f.store.snapshotApplied(), "ghost")
		require.Len(t, applied, 1)
		require.Equal(t, []string{"ghost-1"}, applied[0].RetireIDs, "退场 Agent 走同一条退休路径")
	})
}

// TestScheduler_StartRegistersEnabledOnly 覆盖约束 16：库 enabled 是注册的唯一开关，
// 文件声明与 enabled=true 同时成立才 Add。
// 逆改点：忽略 enabled 列 / 按文件定义注册 → 停用子例的 addCalls 非零，断言红。
func TestScheduler_StartRegistersEnabledOnly(t *testing.T) {
	const spec = "CRON_TZ=UTC " + schedCron

	t.Run("enabled task is registered", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.start(t)

		require.Equal(t, []string{spec}, f.engine.liveHandleSpecs(),
			"enabled 任务按 CRON_TZ 前缀注册")
	})

	t.Run("disabled task is not registered", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: false,
		})
		f.start(t)

		_, _, addCalls := f.engine.engineCounts()
		require.Zero(t, addCalls, "DB disabled 任务不得注册（约束 16）")
		require.Zero(t, f.engine.liveHandles())
	})
}

// TestScheduler_StartTwice 覆盖约束 21：成功启动后再次 Start 返回 errSchedulerStarted，
// 且不重复注册（没有第二套句柄）。
// 逆改点：去掉启动位判定 → 错误不存在 / addCalls 翻倍，断言红。
func TestScheduler_StartTwice(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	_, _, before := f.engine.engineCounts()

	err := f.sched.Start(context.Background())
	require.ErrorIs(t, err, errSchedulerStarted, "成功启动后再 Start 必须报错（约束 21）")
	_, _, after := f.engine.engineCounts()
	require.Equal(t, before, after, "第二次 Start 不得重复注册")
}

// TestScheduler_Fire_MissedTriggerWarnsWithoutRun 覆盖约束 18：停机窗口内错过的触发点
// 只记 WARN（带 missed_count），绝不补跑；零 last_run_at 的新任务不告警也不枚举。
// 逆改点：对 missed 发起运行 → invoker 计数非零；对零 last_run_at 也调 Occurrences
// → occurrenceCalls 非零，断言红。
func TestScheduler_Fire_MissedTriggerWarnsWithoutRun(t *testing.T) {
	t.Run("missed triggers warn without catch-up", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true, LastRunAt: schedNow.Add(-2 * time.Hour),
		})
		f.engine.occurrences = []time.Time{schedNow.Add(-90 * time.Minute), schedNow.Add(-30 * time.Minute)}
		f.start(t)

		rec := f.logs.requireRecord(t, slog.LevelWarn, "错过调度触发点")
		count, ok := attrOf(rec, "missed_count")
		require.True(t, ok, "告警必须带 missed_count")
		require.EqualValues(t, 2, count.Int64())
		atLeast, ok := attrOf(rec, "missed_at_least")
		require.True(t, ok, "告警必须带 missed_at_least")
		require.False(t, atLeast.Bool(), "未达上限时必须明确为 false")
		calls, limit := f.engine.occurrenceReadings()
		require.Equal(t, 1, calls)
		require.Equal(t, schedulerMissedOccurrences, limit, "枚举上限固定 1000（约束 38）")
		require.Zero(t, f.invoker.calls.Load(), "错过触发点只 WARN，不补跑（约束 18）")
	})

	t.Run("capped enumeration reports at least", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true, LastRunAt: schedNow.Add(-48 * time.Hour),
		})
		capped := make([]time.Time, schedulerMissedOccurrences)
		for i := range capped {
			capped[i] = schedNow.Add(-time.Duration(schedulerMissedOccurrences-i) * time.Minute)
		}
		f.engine.occurrences = capped
		f.start(t)

		rec := f.logs.requireRecord(t, slog.LevelWarn, "错过调度触发点")
		count, ok := attrOf(rec, "missed_count")
		require.True(t, ok, "告警必须带 missed_count")
		require.EqualValues(t, schedulerMissedOccurrences, count.Int64())
		atLeast, ok := attrOf(rec, "missed_at_least")
		require.True(t, ok, "达到枚举上限时必须明示「≥1000」（约束 38）")
		require.True(t, atLeast.Bool())
		require.Zero(t, f.invoker.calls.Load(), "错过触发点只 WARN，不补跑（约束 18）")
	})

	t.Run("task without last run does not warn", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.engine.occurrences = []time.Time{schedNow.Add(-time.Minute)}
		f.start(t)

		for _, rec := range f.logs.snapshot() {
			require.NotEqual(t, "错过调度触发点", rec.Msg, "零 last_run_at 的任务是新任务，不得告警")
		}
		calls, _ := f.engine.occurrenceReadings()
		require.Zero(t, calls, "无 last_run_at 时不得枚举触发点")
	})
}

// TestScheduler_StopConcurrent 覆盖约束 21：并发 Stop 只执行一次停机实体，
// 完成信号恰好关闭一次，引擎只停一次。
// 逆改点：去掉 sync.Once → 引擎多次 Stop / done 重复关闭 panic，断言红。
func TestScheduler_StopConcurrent(t *testing.T) {
	f := newSchedFixture(t, nil, nil)
	require.NoError(t, f.sched.Start(context.Background()))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.sched.Stop()
		}()
	}
	wg.Wait()

	select {
	case <-f.sched.done:
	default:
		t.Fatal("并发 Stop 后完成信号必须关闭")
	}
	_, stopped, _ := f.engine.engineCounts()
	require.Equal(t, 1, stopped, "引擎只能被停一次（约束 21）")
}

// TestScheduler_StopOnContextCancel 覆盖约束 24：启动 ctx 取消 → watcher 调 Stop
// 恰一次并完成（无 goroutine 泄漏）。
// 逆改点：不派生 watcher → done 不关闭，用例超时红。
func TestScheduler_StopOnContextCancel(t *testing.T) {
	f := newSchedFixture(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, f.sched.Start(ctx))
	cancel()

	select {
	case <-f.sched.done:
	case <-time.After(2 * time.Second):
		t.Fatal("启动 ctx 取消后 watcher 必须调用 Stop 并完成")
	}
	_, stopped, _ := f.engine.engineCounts()
	require.Equal(t, 1, stopped, "watcher 调用 Stop 恰一次（约束 24）")
}

// TestScheduler_StopBeforeStart 覆盖约束 21 的无条件 Close 口径：未启动即 Stop
// 立即完成、不触碰引擎；重复 Stop 也不 panic。
// 逆改点：未启动路径漏 close(done) → done 不关闭；误调 engine.Stop → stopped 非零。
func TestScheduler_StopBeforeStart(t *testing.T) {
	f := newSchedFixture(t, nil, nil)
	f.sched.Stop()

	select {
	case <-f.sched.done:
	default:
		t.Fatal("未启动即 Stop 必须立即完成（chat / tool 无条件 Close，约束 21）")
	}
	_, stopped, _ := f.engine.engineCounts()
	require.Zero(t, stopped, "未 Start 时 Stop 不得触碰引擎")
	f.sched.Stop() // 幂等：再次调用不 panic
}

// TestScheduler_StopTimeoutCancelsInflight 覆盖约束 23：停机先等宽限，宽限耗尽才取消
// 在途运行；被取消的运行按 failed 收尾，Stop 在有界等待内返回。
// 逆改点：跳过宽限直接取消 → 取消时刻早于宽限，断言红；取消后不等待 → 终态行缺失。
func TestScheduler_StopTimeoutCancelsInflight(t *testing.T) {
	store := newFakeTaskStore()
	entered := make(chan struct{}, 1)
	var sawCancel atomic.Bool
	var cancelAt atomic.Int64
	invoker := &countingInvoker{run: func(ctx context.Context, _ RunRequest) (RunResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		// 先记时间戳再置位标志：Eventually 轮询到 sawCancel 时必须读到已写入的 cancelAt，
		// 反过来写会让轮询落在两次存储之间、读到 0（负载下间歇性红）。
		cancelAt.Store(time.Now().UnixNano())
		sawCancel.Store(true)
		return RunResult{Status: StatusCancelled, Reason: "停机取消"}, nil
	}}
	engine := newBlockingFakeEngine()
	logger, _ := newCaptureLogger()
	sched := newSchedulerWith(store, invoker, engine, logger, []Schedule{demoTask(schedTaskID)}, func(o *SchedulerOptions) {
		o.RunTimeout = time.Minute
		o.StopGrace = 20 * time.Millisecond
	})
	require.NoError(t, sched.Start(context.Background()))

	gen := currentGen(t, sched, schedTaskID)
	fireDone := make(chan struct{})
	go func() { defer close(fireDone); sched.fire(schedTaskID, gen) }()
	<-entered

	stopStart := time.Now()
	stopDone := make(chan struct{})
	go func() { defer close(stopDone); sched.Stop() }()

	require.Eventually(t, sawCancel.Load, 2*time.Second, 5*time.Millisecond, "宽限耗尽必须取消在途运行（约束 23）")
	require.GreaterOrEqual(t, cancelAt.Load()-stopStart.UnixNano(), int64(10*time.Millisecond),
		"必须先等满宽限再取消")
	engine.releaseStop()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 未在宽限 + 有界清理内返回")
	}
	select {
	case <-fireDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fire 未在取消后返回")
	}
	require.Equal(t, []ExecutionStatus{ExecutionFailed}, store.snapshotFinished(),
		"cancelled 收尾为 failed")
}

// TestScheduler_ReconcileAgent 覆盖 R28 / R29 的写后立即生效口径：chat / tool 落盘后
// 调一次 ReconcileAgent——单事务应用、提交后才切换句柄、未启动时零副作用。
// 逆改点：先注册后提交 → 库写失败时句柄已换（兄弟用例红）；逐任务事务 → UpsertIDs
// 拆成多条，断言红；未启动也落库 → not-started 子例红。
func TestScheduler_ReconcileAgent(t *testing.T) {
	ctx := context.Background()

	t.Run("writes take effect immediately", func(t *testing.T) {
		store := newFakeTaskStore()
		engine := &fakeEngine{}
		logger, _ := newCaptureLogger()
		sched := newSchedulerWithEntries(store, &countingInvoker{}, engine, logger,
			[]AgentEntry{agentEntry(schedAgent, true)}, nil)
		startSched(t, sched)
		base := len(store.snapshotApplied())

		require.NoError(t, sched.ReconcileAgent(ctx, schedAgent,
			[]Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)}))

		applied := store.snapshotApplied()
		require.Len(t, applied, base+1, "同一 Agent 的定义必须一次事务应用")
		require.ElementsMatch(t, []string{schedTaskID, schedTaskID2}, applied[base].UpsertIDs)
		require.Empty(t, applied[base].RetireIDs)
		require.Equal(t, 2, engine.liveHandles(), "写后必须立即生效（注 13）")
	})

	t.Run("not started reconcile is a no-op", func(t *testing.T) {
		store := newFakeTaskStore()
		engine := &fakeEngine{}
		logger, _ := newCaptureLogger()
		idle := newSchedulerWithEntries(store, &countingInvoker{}, engine, logger,
			[]AgentEntry{agentEntry(schedAgent, true)}, nil)

		require.NoError(t, idle.ReconcileAgent(ctx, schedAgent, []Schedule{demoTask(schedTaskID)}))

		require.Empty(t, store.snapshotApplied(), "未启动不得落库（chat / tool 只落文件）")
		require.Zero(t, engine.liveHandles(), "未启动不得注册")
	})

	t.Run("definitions keep db enabled values", func(t *testing.T) {
		store := newFakeTaskStore()
		store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: "旧巡检", Enabled: false, DefinitionHash: "old",
		})
		engine := &fakeEngine{}
		logger, _ := newCaptureLogger()
		sched := newSchedulerWithEntries(store, &countingInvoker{}, engine, logger,
			[]AgentEntry{agentEntry(schedAgent, true)}, nil)
		startSched(t, sched)
		base := len(store.snapshotApplied())

		require.NoError(t, sched.ReconcileAgent(ctx, schedAgent,
			[]Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)}))

		applied := store.snapshotApplied()
		require.Len(t, applied, base+1)
		require.ElementsMatch(t, []string{schedTaskID, schedTaskID2}, applied[base].UpsertIDs,
			"新任务经一次 ApplyAgentTasks 入库")
		kept, ok := store.task(schedTaskID)
		require.True(t, ok)
		require.False(t, kept.Enabled, "既有行保持库值 enabled=false（注 14）")
		fresh, ok := store.task(schedTaskID2)
		require.True(t, ok)
		require.True(t, fresh.Enabled, "新行按缺省 enabled=1")
		require.Equal(t, 1, engine.liveHandles(), "停用任务不注册")
	})

	t.Run("removals retire in the same transaction", func(t *testing.T) {
		store := newFakeTaskStore()
		engine := &fakeEngine{}
		logger, _ := newCaptureLogger()
		sched := newSchedulerWithEntries(store, &countingInvoker{}, engine, logger,
			[]AgentEntry{agentEntry(schedAgent, true, demoTask(schedTaskID), demoTask(schedTaskID2))}, nil)
		startSched(t, sched)
		base := len(store.snapshotApplied())

		require.NoError(t, sched.ReconcileAgent(ctx, schedAgent, []Schedule{demoTask(schedTaskID)}))

		applied := store.snapshotApplied()
		require.Len(t, applied, base+1)
		require.Equal(t, []string{schedTaskID2}, applied[base].RetireIDs, "移除项在同一事务内退休")
		gone, ok := store.task(schedTaskID2)
		require.True(t, ok)
		require.False(t, gone.RetiredAt.IsZero())
		require.Equal(t, 1, engine.liveHandles(), "退休项句柄必须摘除")
	})
}

// TestScheduler_SetRegistered_RetiresRemovedAgent 覆盖约束 39：Agent 移出注册集合时，
// 只退休它自己的未退休任务（一次事务）、摘除句柄，其他 Agent 不受影响。
// 逆改点：删掉 ApplyAgentTasks + invalidateLocked 段 → 退休读数与句柄断言红。
func TestScheduler_SetRegistered_RetiresRemovedAgent(t *testing.T) {
	store := newFakeTaskStore()
	engine := &fakeEngine{}
	logger, logs := newCaptureLogger()
	sched := newSchedulerWithEntries(store, &countingInvoker{}, engine, logger,
		[]AgentEntry{
			agentEntry(schedAgent, true, demoTask(schedTaskID), demoTask(schedTaskID2)),
			agentEntry("other", true, demoTask("other-task")),
		}, nil)
	startSched(t, sched)
	require.Equal(t, 3, engine.liveHandles())
	base := len(store.snapshotApplied())

	require.NoError(t, sched.SetRegistered(schedAgent, false))

	applied := store.snapshotApplied()
	require.Len(t, applied, base+1, "一个 Agent 的退休必须一次事务完成")
	require.ElementsMatch(t, []string{schedTaskID, schedTaskID2}, applied[base].RetireIDs)
	require.Empty(t, applied[base].UpsertIDs, "退休事务不得顺带上写")
	require.Equal(t, 1, engine.liveHandles(), "只摘除该 Agent 的句柄")
	for _, id := range []string{schedTaskID, schedTaskID2} {
		rec, ok := store.task(id)
		require.True(t, ok)
		require.False(t, rec.RetiredAt.IsZero(), "任务 %s 必须标退休", id)
	}
	other, ok := store.task("other-task")
	require.True(t, ok)
	require.True(t, other.RetiredAt.IsZero(), "其他 Agent 的任务不得被波及")
	rec := logs.requireRecord(t, slog.LevelInfo, "Agent 已移出调度注册集合，任务退休")
	tasks, ok := attrOf(rec, "tasks")
	require.True(t, ok)
	require.Equal(t, "2", tasks.String())
}

// TestScheduler_SetRegistered_NotStartedOnlyTouchesRegistry 覆盖未启动时的注册集合
// 增减只做内存操作：不读库、不落库、不动句柄（定义由 Start 对账）。
func TestScheduler_SetRegistered_NotStartedOnlyTouchesRegistry(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)

	require.NoError(t, f.sched.SetRegistered(schedAgent, false))
	require.NoError(t, f.sched.SetRegistered("never-registered", false))
	require.NoError(t, f.sched.SetRegistered(schedAgent, true))

	require.Zero(t, f.store.listReadings(), "未启动不得读库")
	require.Empty(t, f.store.snapshotApplied(), "未启动不得落库")
	require.Zero(t, f.engine.liveHandles())
	f.sched.stateMu.RLock()
	_, stillIn := f.sched.registry[schedAgent]
	f.sched.stateMu.RUnlock()
	require.True(t, stillIn, "registered=true 必须重新纳入调度集合")
}

// TestScheduler_SetRegistered_ErrorKeepsRemovalAndHandles 覆盖失败处置：注册集合先
// 收回（准入失败关闭），库读 / 库写失败如实上抛，旧句柄与旧任务保持原样。
func TestScheduler_SetRegistered_ErrorKeepsRemovalAndHandles(t *testing.T) {
	t.Run("list failure", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		before := f.engine.liveHandleSpecs()
		listErr := errors.New("disk unavailable")
		f.store.listErr = listErr

		require.ErrorIs(t, f.sched.SetRegistered(schedAgent, false), listErr)

		require.Equal(t, before, f.engine.liveHandleSpecs(), "读库失败不得摘句柄")
		f.sched.stateMu.RLock()
		_, stillIn := f.sched.registry[schedAgent]
		f.sched.stateMu.RUnlock()
		require.False(t, stillIn, "移出注册集合先于库操作（准入立即失败关闭）")
		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.True(t, rec.RetiredAt.IsZero(), "读库失败不得退休")
	})

	t.Run("apply failure", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		before := f.engine.liveHandleSpecs()
		applyErr := errors.New("db down")
		f.store.applyErr = applyErr

		require.ErrorIs(t, f.sched.SetRegistered(schedAgent, false), applyErr)

		require.Equal(t, before, f.engine.liveHandleSpecs(), "写库失败不得摘句柄")
		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.True(t, rec.RetiredAt.IsZero(), "写库失败任务保持未退休")
	})
}

// TestScheduler_ReconcileAgent_StoreFailureKeepsRegistration 覆盖约束 41：库写失败时
// 旧定义与旧句柄完整保留——不先停句柄、不重注册。
// 逆改点：提交前 invalidate → 旧句柄已停，liveHandleSpecs 变空，断言红。
func TestScheduler_ReconcileAgent_StoreFailureKeepsRegistration(t *testing.T) {
	f := newSchedFixture(t, []Schedule{{ID: schedTaskID, Cron: schedCron, Zone: "UTC", Message: "旧巡检"}}, nil)
	f.start(t)
	before := f.engine.liveHandleSpecs()
	_, _, addBefore := f.engine.engineCounts()

	writeErr := errors.New("disk full")
	f.store.applyErr = writeErr
	err := f.sched.ReconcileAgent(context.Background(), schedAgent,
		[]Schedule{{ID: schedTaskID, Cron: "*/10 * * * * *", Zone: "UTC", Message: "新巡检"}})
	require.ErrorIs(t, err, writeErr)
	require.Equal(t, before, f.engine.liveHandleSpecs(), "库写失败必须保留旧句柄")
	_, _, addAfter := f.engine.engineCounts()
	require.Equal(t, addBefore, addAfter, "失败不得重注册")
	rec, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	require.Equal(t, "旧巡检", rec.Message, "库保持旧定义")
}

// TestScheduler_Start_StoreFailureFatal 覆盖注 16：存储层失败是致命启动失败——
// 零注册、不置启动位、可修复后重试。
// 逆改点：吞掉恢复错误继续启动 → Start 返回 nil、引擎启动，断言红。
func TestScheduler_Start_StoreFailureFatal(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.recoverErr = errors.New("recover failed")

	err := f.sched.Start(context.Background())
	require.Error(t, err, "存储层失败是致命启动失败（注 16）")
	_, _, addCalls := f.engine.engineCounts()
	require.Zero(t, addCalls, "失败不得注册任何任务")
	require.False(t, f.sched.started, "失败不得置启动位（约束 21）")

	// 修复后同一实例可重试成功（失败不置位）。
	f.store.recoverErr = nil
	f.start(t)
	_, _, addCalls = f.engine.engineCounts()
	require.Equal(t, 1, addCalls)
}

// TestScheduler_Start_TaskAddFailureStopsEngine 覆盖约束 19 / 注 16：启动阶段单任务
// Add 失败是致命的——停引擎、清句柄、不置启动位。
// 逆改点：忽略 Add 错误继续启动 → Start 返回 nil、started 为真，断言红。
func TestScheduler_Start_TaskAddFailureStopsEngine(t *testing.T) {
	store := newFakeTaskStore()
	engine := &fakeEngine{addErr: errors.New("add failed")}
	logger, _ := newCaptureLogger()
	sched := newSchedulerWith(store, &countingInvoker{}, engine, logger, []Schedule{demoTask(schedTaskID)}, nil)

	err := sched.Start(context.Background())
	require.Error(t, err, "启动阶段单任务 Add 失败是致命的（约束 19 / 注 16）")
	started, stopped, addCalls := engine.engineCounts()
	require.Zero(t, started, "失败路径不得启动引擎")
	require.Equal(t, 1, stopped, "失败必须停引擎")
	require.Equal(t, 1, addCalls)
	require.Zero(t, engine.liveHandles())
	require.False(t, sched.started)
}

// TestScheduler_Start_OwnerConflictFatal 覆盖约束 42：同 task_id 归属另一 Agent 时
// 启动失败（活动行与退休行都不放宽），零注册、不置启动位。
// 逆改点：冲突行按本 Agent 覆盖 → Start 返回 nil,断言红。
func TestScheduler_Start_OwnerConflictFatal(t *testing.T) {
	cases := []struct {
		name    string
		retired bool
	}{
		{name: "active id owned by another agent", retired: false},
		{name: "retired id still owned by another agent", retired: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeTaskStore()
			row := TaskRecord{
				TaskID: "shared-1", Agent: "other", Cron: schedCron, Zone: "UTC",
				Message: "他人的任务", Enabled: true,
			}
			if tc.retired {
				row.RetiredAt = schedNow.Add(-time.Hour)
			}
			store.seedTask(row)
			engine := &fakeEngine{}
			logger, _ := newCaptureLogger()
			sched := newSchedulerWithEntries(store, &countingInvoker{}, engine, logger,
				[]AgentEntry{agentEntry(schedAgent, true, Schedule{
					ID: "shared-1", Cron: schedCron, Zone: "UTC", Message: "接管",
				})}, nil)

			err := sched.Start(context.Background())
			require.ErrorIs(t, err, ErrTaskOwnerConflict, "归属冲突是致命启动失败（约束 42）")
			_, _, addCalls := engine.engineCounts()
			require.Zero(t, addCalls, "冲突不得注册")
			require.False(t, sched.started)
		})
	}
}

// TestScheduler_Reconcile_SecondWriteFails_KeepsOldDefinition 覆盖约束 41 的
// "库写失败 → 旧定义继续服务"：失败后触发仍使用旧内容，不出现新旧混合。
// 逆改点：先清 definitions / 先停句柄再写库 → 运行消息变新或运行缺失，断言红。
func TestScheduler_Reconcile_SecondWriteFails_KeepsOldDefinition(t *testing.T) {
	f := newSchedFixture(t, []Schedule{{ID: schedTaskID, Cron: schedCron, Zone: "UTC", Message: "旧巡检"}}, nil)
	f.start(t)
	before := f.engine.liveHandleSpecs()
	_, _, addBefore := f.engine.engineCounts()

	f.store.applyErr = ErrTaskOwnerConflict
	err := f.sched.ReconcileAgent(context.Background(), schedAgent,
		[]Schedule{{ID: schedTaskID, Cron: "*/10 * * * * *", Zone: "UTC", Message: "新巡检"}})
	require.ErrorIs(t, err, ErrTaskOwnerConflict)
	require.Equal(t, before, f.engine.liveHandleSpecs(), "失败后保留完整旧句柄")
	_, _, addAfter := f.engine.engineCounts()
	require.Equal(t, addBefore, addAfter, "失败不得注册新句柄")
	rec, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	require.Equal(t, "旧巡检", rec.Message, "库保持旧定义")

	f.fire(t, schedTaskID)
	reqs := f.invoker.requests()
	require.Len(t, reqs, 1)
	require.Equal(t, "旧巡检", reqs[0].Message, "不得出现「旧触发时间 + 新任务内容」")
}

// TestScheduler_OldCallbackAfterReconcile_NoRun 覆盖约束 44 的代次防线与快照临界区：
// ① 对账切换后已派发的旧回调直接返回（零运行、零执行行）；
// ② 新注册的回调使用完整新定义；
// ③ 快照的 ListTasks 与代次判定在同一个读锁临界区内，对账写锁不得插队。
// 逆改点：不比对代次 → ① 出现运行；ListTasks 移到锁外 → ③ 对账在快照中途完成，断言红。
func TestScheduler_OldCallbackAfterReconcile_NoRun(t *testing.T) {
	f := newSchedFixture(t, []Schedule{{ID: schedTaskID, Cron: schedCron, Zone: "UTC", Message: "旧巡检"}}, nil)
	f.start(t)

	t.Run("stale callback after swap produces no run", func(t *testing.T) {
		oldJob := f.engine.lastJob()
		require.NotNil(t, oldJob, "启动注册必须捕获旧 job")

		entered := make(chan struct{})
		release := make(chan struct{})
		f.store.applyHook = func() { close(entered); <-release }
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = f.sched.ReconcileAgent(context.Background(), schedAgent,
				[]Schedule{{ID: schedTaskID, Cron: "*/10 * * * * *", Zone: "UTC", Message: "新巡检"}})
		}()
		<-entered

		oldDone := make(chan struct{})
		go func() { defer close(oldDone); oldJob() }()
		select {
		case <-oldDone:
			t.Fatal("旧 job 不得越过对账写锁")
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		<-done
		<-oldDone

		require.Zero(t, f.invoker.calls.Load(), "切换后旧 job 必须直接返回")
		require.Empty(t, f.store.snapshotStarted(), "旧 job 不得写执行行")

		oldJob()
		require.Zero(t, f.invoker.calls.Load(), "切换完成后再调旧 job 仍无效")
		require.Empty(t, f.store.snapshotStarted())
	})

	t.Run("new callback runs with new definition", func(t *testing.T) {
		newJob := f.engine.lastJob()
		require.NotNil(t, newJob, "切换后必须注册新 job")
		newJob()

		require.EqualValues(t, 1, f.invoker.calls.Load())
		reqs := f.invoker.requests()
		require.Len(t, reqs, 1)
		require.Equal(t, "新巡检", reqs[0].Message, "新 job 必须使用完整新定义")
	})

	t.Run("snapshot is read under the state lock", func(t *testing.T) {
		current := f.engine.lastJob()
		require.NotNil(t, current)

		entered := make(chan struct{})
		releaseList := make(chan struct{})
		f.store.listHook = func() { close(entered); <-releaseList }
		oldDone := make(chan struct{})
		go func() { defer close(oldDone); current() }()
		<-entered

		swapDone := make(chan struct{})
		go func() {
			defer close(swapDone)
			_ = f.sched.ReconcileAgent(context.Background(), schedAgent,
				[]Schedule{{ID: schedTaskID, Cron: "*/20 * * * * *", Zone: "UTC", Message: "第三版巡检"}})
		}()
		select {
		case <-swapDone:
			t.Fatal("对账写锁不得在快照临界区内完成：ListTasks 必须在读锁内")
		case <-time.After(50 * time.Millisecond):
		}
		close(releaseList)
		<-oldDone
		<-swapDone

		require.EqualValues(t, 2, f.invoker.calls.Load(), "旧 job 必须真实执行一次")
		reqs := f.invoker.requests()
		require.Equal(t, "新巡检", reqs[len(reqs)-1].Message, "旧回调只能使用切换前的快照")
	})
}

// TestScheduler_ReconcileAgent_AddFailureReturnsError 覆盖约束 41 的运行期注册失败口径：
// 事务已提交（定义落库不回滚），其余任务照常注册，失败任务无句柄，错误上抛并点名任务。
// 逆改点：首个 Add 失败即中止后续注册 → demo-1 不再注册（liveHandleSpecs 变空），断言红。
func TestScheduler_ReconcileAgent_AddFailureReturnsError(t *testing.T) {
	const failingSpec = "CRON_TZ=UTC */15 * * * * *"
	f := newSchedFixture(t, []Schedule{{ID: schedTaskID, Cron: schedCron, Zone: "UTC", Message: "旧巡检"}}, nil)
	f.start(t)
	f.engine.mu.Lock()
	f.engine.addErrFor = failingSpec
	f.engine.mu.Unlock()

	err := f.sched.ReconcileAgent(context.Background(), schedAgent, []Schedule{
		{ID: schedTaskID, Cron: "*/10 * * * * *", Zone: "UTC", Message: "新巡检"},
		{ID: schedTaskID2, Cron: "*/15 * * * * *", Zone: "UTC", Message: "周报"},
	})
	require.ErrorContains(t, err, schedTaskID2, "运行期注册失败必须上抛聚合错误并点名任务")
	rec, ok := f.store.task(schedTaskID2)
	require.True(t, ok, "事务已提交：定义已落库，不回滚")
	require.Equal(t, "周报", rec.Message)
	require.Equal(t, []string{"CRON_TZ=UTC */10 * * * * *"}, f.engine.liveHandleSpecs(),
		"其余任务照常注册，失败任务无句柄")
}

// TestScheduler_Start_SecondAgentApplyFails_NoHandles 覆盖图 B 的两阶段启动：
// 任一 Agent 的事务失败 → Start 失败，且此前已提交的 Agent 也**零句柄**（先全部提交、
// 后统一注册）。
// 逆改点：逐 Agent「提交即注册」→ 第一只 Agent 的句柄已挂，addCalls 非零，断言红。
func TestScheduler_Start_SecondAgentApplyFails_NoHandles(t *testing.T) {
	store := newFakeTaskStore()
	store.applyErr = errors.New("second write failed")
	store.applyErrOnCall = 2
	engine := &fakeEngine{}
	logger, _ := newCaptureLogger()
	entries := []AgentEntry{
		agentEntry("demo", true, demoTask(schedTaskID)),
		agentEntry("other", true, demoTask("other-1")),
	}
	sched := newSchedulerWithEntries(store, &countingInvoker{}, engine, logger, entries, nil)

	err := sched.Start(context.Background())
	require.Error(t, err, "任一 Agent 的事务失败必须让 Start 失败")
	_, _, addCalls := engine.engineCounts()
	require.Zero(t, addCalls, "两阶段：全部提交成功前不得注册任何句柄（R10-M8）")
	require.Zero(t, engine.liveHandles())
	require.Len(t, appliedFor(store.snapshotApplied(), "demo"), 1, "第一个 Agent 的提交已完成")
	require.False(t, sched.started)
}

// TestScheduler_RunNow_SameChain 覆盖 R5 / 约束 1 / 13：手动补跑与到点触发共用
// scheduleRun 同一条执行链——trigger=manual，执行行与返回结果都来自同一次运行。
// 逆改点：RunNow 另起执行链（不预写执行行 / 不走 scheduleRun）→ started / finished
// 断言红；trigger 写 cron → 断言红。
func TestScheduler_RunNow_SameChain(t *testing.T) {
	t.Run("uses the shared execution chain", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.start(t)

		req, _, err := f.sched.RunNow(context.Background(), schedTaskID)
		require.NoError(t, err)
		require.Equal(t, TriggerManual, req.Trigger)

		started := f.store.snapshotStarted()
		require.Len(t, started, 1, "手动补跑必须走 StartExecution 预写（约束 9 同链）")
		require.Equal(t, TriggerManual, started[0].Trigger)
		require.Equal(t, ExecutionStarted, started[0].Status)
		require.Equal(t, req.RunID, started[0].RunID, "执行行与请求使用同一个 RunID")
		require.Equal(t, schedNow, started[0].StartedAt)
		require.Equal(t, []ExecutionStatus{ExecutionCompleted}, f.store.snapshotFinished())
		require.Equal(t, []fakeMarkRecord{{TaskID: schedTaskID, At: schedNow, Status: ExecutionStarted}},
			f.store.snapshotMarks(), "MarkTaskRun 与 cron 路径同序")
	})

	t.Run("returns the run request and result", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.start(t)

		req, res, err := f.sched.RunNow(context.Background(), schedTaskID)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(req.RunID, "scheduler-"), "RunID 前缀固定 scheduler-（约束 12）")
		require.Equal(t, schedAgent, req.Agent)
		require.Equal(t, schedSession, req.SessionID, "调度会话固定向量（约束 12）")
		require.Equal(t, schedMessage, req.Message)
		require.Equal(t, "scheduler", req.Channel)
		require.Equal(t, "scheduler", req.UserID)
		require.Equal(t, TriggerManual, req.Trigger)
		require.Equal(t, Budget{MaxIterations: 10}, req.Budget)
		require.Equal(t, req.RunID, res.RunID)
		require.Equal(t, StatusCompleted, res.Status)
	})
}

// TestScheduler_RunNow_BusyReturnsErrTaskBusy 覆盖 R10 / 约束 8：手动撞上在途执行 →
// ErrTaskBusy、不排队、零执行行、零运行；请求未成为一次运行，不落 skipped 行。
// 逆改点：用 Lock 排队 / 把撞锁落成 skipped 行 → 用例超时或行数断言红。
func TestScheduler_RunNow_BusyReturnsErrTaskBusy(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
	})
	f.start(t)

	f.sched.lockFor(schedTaskID).Lock()
	_, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	f.sched.lockFor(schedTaskID).Unlock()

	require.ErrorIs(t, err, ErrTaskBusy, "手动撞锁必须返回 ErrTaskBusy（约束 8）")
	require.Zero(t, f.invoker.calls.Load(), "撞锁不得发起运行")
	require.Empty(t, f.store.snapshotStarted(), "撞锁不得预写执行行")
	require.Empty(t, f.store.snapshotSkipped(), "手动撞锁不落 skipped 行（请求未成为一次运行）")
	require.Empty(t, f.store.snapshotFinished())
}

// TestScheduler_RunNow_HiddenTaskNotFound 覆盖约束 17 / N-06：未知 / retired /
// Agent 未注册三种不可见条件各一例，均包装 fs.ErrNotExist（web 404），零运行、零执行行。
// 逆改点：只判任务是否存在（不看 retired / registry）→ retired 与未注册子例红。
func TestScheduler_RunNow_HiddenTaskNotFound(t *testing.T) {
	cases := []struct {
		name       string
		registered bool
		seed       *TaskRecord
	}{
		{name: "unknown task", registered: true},
		{
			name: "retired task", registered: true,
			seed: &TaskRecord{
				TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
				Message: schedMessage, Enabled: true, RetiredAt: schedNow.Add(-time.Hour),
			},
		},
		{
			name: "agent not registered", registered: false,
			seed: &TaskRecord{
				TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
				Message: schedMessage, Enabled: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeTaskStore()
			if tc.seed != nil {
				store.seedTask(*tc.seed)
			}
			invoker := &countingInvoker{}
			engine := &fakeEngine{}
			logger, _ := newCaptureLogger()
			sched := newSchedulerWithEntries(store, invoker, engine, logger,
				[]AgentEntry{agentEntry(schedAgent, tc.registered)}, nil)
			startSched(t, sched)

			_, _, err := sched.RunNow(context.Background(), schedTaskID)
			require.ErrorIs(t, err, fs.ErrNotExist, "不可见任务必须包装 fs.ErrNotExist（web 404）")
			require.Zero(t, invoker.calls.Load(), "不可见任务不得发起运行")
			require.Empty(t, store.snapshotStarted())
			require.Empty(t, store.snapshotSkipped())
		})
	}
}

// TestScheduler_RunNow_DisabledTaskStillRuns 覆盖注 7：enabled 只控制 cron 注册，
// 停用任务仍可手动补跑一次（人工运维动作）。
// 逆改点：RunNow 复用 fire 的 enabled 过滤 → 返回 ErrTaskBusy / not exist，断言红。
func TestScheduler_RunNow_DisabledTaskStillRuns(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: false,
	})
	f.start(t)
	require.Zero(t, f.engine.liveHandles(), "停用任务不注册 cron 句柄")

	req, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	require.NoError(t, err, "停用任务仍可手动补跑（注 7）")
	require.Equal(t, TriggerManual, req.Trigger)
	require.EqualValues(t, 1, f.invoker.calls.Load())
	require.Equal(t, []ExecutionStatus{ExecutionCompleted}, f.store.snapshotFinished())
}

// TestScheduler_RunNow_SessionBusyRecordsSkipped 覆盖 D-18 manual 面 / N-11：兄弟任务
// 占会话时错误原样上抛（不是 ErrTaskBusy），执行行收尾 skipped + 固定文案、run_id 置空。
// 逆改点：把 SessionBusyError 折成 ErrTaskBusy → 错误类型断言红；按 failed 收尾 → 状态红。
func TestScheduler_RunNow_SessionBusyRecordsSkipped(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, &SessionBusyError{SessionID: schedSession}
	}
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
	})
	f.start(t)

	_, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	var busy *SessionBusyError
	require.ErrorAs(t, err, &busy, "SessionBusyError 必须原样上抛（web 按会话忙映射 409）")
	require.NotErrorIs(t, err, ErrTaskBusy, "兄弟任务占会话不得折成 ErrTaskBusy")
	require.Equal(t, []ExecutionStatus{ExecutionSkipped}, f.store.snapshotFinished())
	calls := f.store.snapshotFinishCalls()
	require.Len(t, calls, 1)
	require.Equal(t, "同一 Agent 的另一任务仍在执行", calls[0].ErrMsg)
	require.Empty(t, calls[0].RunID, "无 Run 行的终态行 run_id 必须置空")
	f.logs.requireRecord(t, slog.LevelWarn, "同一 Agent 的另一任务仍在执行")

	rec, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	require.Equal(t, ExecutionSkipped, rec.LastStatus, "FinishExecution 同步 skipped")
}

// TestScheduler_SetEnabled_TogglesRegistration 覆盖约束 16 / 注 2：停用注销、启用注册、
// 重复启用幂等且能修复缺失句柄。逆改点：重复启用直接 Add → 句柄叠加，断言红；
// 只按库值短路 → 缺失句柄永不修复，第三子例红。
func TestScheduler_SetEnabled_TogglesRegistration(t *testing.T) {
	t.Run("disable unregisters the handle", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.start(t)
		require.Equal(t, 1, f.engine.liveHandles())

		require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, false))

		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.False(t, rec.Enabled, "库值必须落为停用")
		require.Zero(t, f.engine.liveHandles(), "停用必须摘除句柄（约束 16）")
		require.Equal(t, 1, f.engine.handleStops())
	})

	t.Run("enable registers the handle", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: false,
		})
		f.start(t)
		require.Zero(t, f.engine.liveHandles())
		_, _, addBefore := f.engine.engineCounts()

		require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, true))

		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.True(t, rec.Enabled, "库值必须落为启用")
		require.Equal(t, 1, f.engine.liveHandles())
		_, _, addAfter := f.engine.engineCounts()
		require.Equal(t, addBefore+1, addAfter, "启用侧注册恰好一次")

		job := f.engine.lastJob()
		require.NotNil(t, job)
		job()
		require.EqualValues(t, 1, f.invoker.calls.Load(), "启用后的句柄必须真实可用")
	})

	t.Run("repeated enable is idempotent and repairs a missing handle", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.start(t)
		_, _, addBaseline := f.engine.engineCounts()

		require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, true))
		_, _, addAfterRepeat := f.engine.engineCounts()
		require.Equal(t, addBaseline, addAfterRepeat, "重复启用不得叠加句柄（幂等短路）")
		require.Zero(t, f.store.setEnabledReadings(), "库值与句柄态一致时零写 SQL")
		require.Equal(t, 1, f.engine.liveHandles())

		// 模拟上次 Add 失败：库值仍为 true、句柄缺失 → 启用侧必须补注册一次。
		f.sched.stateMu.Lock()
		f.sched.handles[schedTaskID].Stop()
		delete(f.sched.handles, schedTaskID)
		f.sched.stateMu.Unlock()
		require.Zero(t, f.engine.liveHandles())

		require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, true))
		_, _, addAfterRepair := f.engine.engineCounts()
		require.Equal(t, addBaseline+1, addAfterRepair, "缺失句柄必须补注册一次")
		require.Equal(t, 1, f.engine.liveHandles())
		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.True(t, rec.Enabled)
	})
}

// TestScheduler_SetEnabled_HiddenTaskNotFound 覆盖约束 17 / N-02：未知 / retired /
// Agent 未注册三种不可见条件各一例——判定前先读一次 ListTasks，随后零写 SQL、零引擎动作。
// 逆改点：只判库行存在（不看 retired / registry）→ 不可见子例写库成功、断言红。
func TestScheduler_SetEnabled_HiddenTaskNotFound(t *testing.T) {
	cases := []struct {
		name       string
		registered bool
		seed       *TaskRecord
		enabled    bool
	}{
		{name: "unknown task", registered: true, enabled: true},
		{
			name: "retired task", registered: true, enabled: false,
			seed: &TaskRecord{
				TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
				Message: schedMessage, Enabled: true, RetiredAt: schedNow.Add(-time.Hour),
			},
		},
		{
			name: "agent not registered", registered: false, enabled: false,
			seed: &TaskRecord{
				TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
				Message: schedMessage, Enabled: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeTaskStore()
			if tc.seed != nil {
				store.seedTask(*tc.seed)
			}
			engine := &fakeEngine{}
			logger, _ := newCaptureLogger()
			sched := newSchedulerWithEntries(store, &countingInvoker{}, engine, logger,
				[]AgentEntry{agentEntry(schedAgent, tc.registered)}, nil)
			startSched(t, sched)

			listBefore := store.listReadings()
			err := sched.SetEnabled(context.Background(), schedTaskID, tc.enabled)
			require.ErrorIs(t, err, fs.ErrNotExist, "不可见任务必须包装 fs.ErrNotExist（web 404）")
			require.Greater(t, store.listReadings(), listBefore, "判定前必须先读一次 ListTasks（N-02）")
			require.Zero(t, store.setEnabledReadings(), "不可见任务必须零写 SQL")
			_, _, addCalls := engine.engineCounts()
			require.Zero(t, addCalls, "不可见任务必须零引擎动作")
			require.Zero(t, engine.handleStops())
			if tc.seed != nil {
				rec, ok := store.task(schedTaskID)
				require.True(t, ok)
				require.True(t, rec.Enabled, "库值不得被改写")
			}
		})
	}
}

// TestScheduler_List_FiltersAndOrders 覆盖约束 17 / §6.3 第 10 条：retired 与未注册
// Agent 的任务隐藏；停用任务仍在管理读面可见；输出按 task_id 升序。
// 逆改点：不判 retired / registry → 列表混入隐藏行，断言红。
func TestScheduler_List_FiltersAndOrders(t *testing.T) {
	store := newFakeTaskStore()
	store.seedTask(TaskRecord{
		TaskID: "demo-2", Agent: "demo", Cron: schedCron, Zone: "UTC",
		Message: "停用巡检", Enabled: false, LastStatus: ExecutionCompleted, LastRunAt: schedNow,
	})
	store.seedTask(TaskRecord{
		TaskID: "demo-1", Agent: "demo", Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
	})
	store.seedTask(TaskRecord{
		TaskID: "demo-3", Agent: "demo", Cron: schedCron, Zone: "UTC",
		Message: "已下线", Enabled: true, RetiredAt: schedNow.Add(-time.Hour),
	})
	store.seedTask(TaskRecord{
		TaskID: "ghost-1", Agent: "ghost", Cron: schedCron, Zone: "UTC",
		Message: "未注册 Agent 的任务", Enabled: true,
	})
	logger, _ := newCaptureLogger()
	sched := newSchedulerWithEntries(store, &countingInvoker{}, &fakeEngine{}, logger,
		[]AgentEntry{agentEntry("demo", true), agentEntry("ghost", false)}, nil)

	rows, err := sched.List(context.Background())
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.TaskID)
	}
	require.Equal(t, []string{"demo-1", "demo-2"}, ids, "只返回可见任务且按 task_id 升序")
	require.False(t, rows[1].Enabled, "停用任务仍在管理读面可见")
}

// TestScheduler_List_ReflectsRunState 覆盖约束 17 / §3.1.2：List 每次读库，不在进程内
// 另存任务状态——两次调用之间改库，第二次输出随之变化。
// 逆改点：构造期 / 首次调用缓存任务表 → 第二次仍读到旧值，断言红。
func TestScheduler_List_ReflectsRunState(t *testing.T) {
	store := newFakeTaskStore()
	store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
		LastStatus: ExecutionCompleted, LastRunAt: schedNow.Add(-time.Hour),
	})
	logger, _ := newCaptureLogger()
	sched := newSchedulerWithEntries(store, &countingInvoker{}, &fakeEngine{}, logger,
		[]AgentEntry{agentEntry(schedAgent, true)}, nil)

	rows, err := sched.List(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ExecutionCompleted, rows[0].LastStatus)
	require.Equal(t, schedNow.Add(-time.Hour), rows[0].LastRunAt)

	store.mutateTask(schedTaskID, func(rec *TaskRecord) {
		rec.LastStatus = ExecutionFailed
		rec.LastRunAt = schedNow
	})

	rows, err = sched.List(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ExecutionFailed, rows[0].LastStatus, "List 每次读库，不得缓存任务状态")
	require.Equal(t, schedNow, rows[0].LastRunAt)
}

// TestScheduler_Executions_UsesLimit 覆盖约束 38 / 注 11：limit 透传到仓储并生效，
// 结果保持仓储的 started_at 降序。
// 逆改点：忽略 limit（固定 100 / 全量返回）→ 行数断言红；重排为升序 → 顺序断言红。
func TestScheduler_Executions_UsesLimit(t *testing.T) {
	store := newFakeTaskStore()
	store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
	})
	base := schedNow.Add(-time.Hour)
	store.executions[schedTaskID] = []ExecutionRecord{
		{ID: 5, TaskID: schedTaskID, Status: ExecutionCompleted, Trigger: TriggerCron, StartedAt: base.Add(5 * time.Minute)},
		{ID: 4, TaskID: schedTaskID, Status: ExecutionCompleted, Trigger: TriggerCron, StartedAt: base.Add(4 * time.Minute)},
		{ID: 3, TaskID: schedTaskID, Status: ExecutionFailed, Trigger: TriggerCron, StartedAt: base.Add(3 * time.Minute)},
		{ID: 2, TaskID: schedTaskID, Status: ExecutionSkipped, Trigger: TriggerCron, StartedAt: base.Add(2 * time.Minute)},
		{ID: 1, TaskID: schedTaskID, Status: ExecutionCompleted, Trigger: TriggerCron, StartedAt: base.Add(time.Minute)},
	}
	logger, _ := newCaptureLogger()
	sched := newSchedulerWithEntries(store, &countingInvoker{}, &fakeEngine{}, logger,
		[]AgentEntry{agentEntry(schedAgent, true)}, nil)

	rows, err := sched.Executions(context.Background(), schedTaskID, 2)
	require.NoError(t, err)
	require.Len(t, rows, 2, "limit 必须透传并生效")
	require.Equal(t, int64(5), rows[0].ID)
	require.Equal(t, int64(4), rows[1].ID)
	require.True(t, rows[0].StartedAt.After(rows[1].StartedAt), "保持 started_at 降序")
}

// TestScheduler_Executions_HiddenTaskNotFound 覆盖约束 33 / N-06：未知 / retired /
// Agent 未注册的任务一律 fs.ErrNotExist（web 404），不返回任何历史行。
// 逆改点：跳过可见性直接查执行历史 → retired / 未注册子例返回行，断言红。
func TestScheduler_Executions_HiddenTaskNotFound(t *testing.T) {
	cases := []struct {
		name       string
		registered bool
		seed       *TaskRecord
	}{
		{name: "unknown task", registered: true},
		{
			name: "retired task", registered: true,
			seed: &TaskRecord{
				TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
				Message: schedMessage, Enabled: true, RetiredAt: schedNow.Add(-time.Hour),
			},
		},
		{
			name: "agent not registered", registered: false,
			seed: &TaskRecord{
				TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
				Message: schedMessage, Enabled: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeTaskStore()
			if tc.seed != nil {
				store.seedTask(*tc.seed)
				store.executions[schedTaskID] = []ExecutionRecord{{
					ID: 1, TaskID: schedTaskID, Status: ExecutionCompleted,
					Trigger: TriggerCron, StartedAt: schedNow.Add(-time.Hour),
				}}
			}
			logger, _ := newCaptureLogger()
			sched := newSchedulerWithEntries(store, &countingInvoker{}, &fakeEngine{}, logger,
				[]AgentEntry{agentEntry(schedAgent, tc.registered)}, nil)

			rows, err := sched.Executions(context.Background(), schedTaskID, 100)
			require.ErrorIs(t, err, fs.ErrNotExist, "不可见任务必须包装 fs.ErrNotExist（web 404）")
			require.Empty(t, rows, "不得泄漏不可见任务的历史")
		})
	}
}

// TestScheduler_SetEnabled_WritesDBFirst 覆盖注 2 / 约束 16：先写库、后动引擎——
// 库写失败时引擎保持原状并上抛错误；并发下线（RowsAffected()==0）映射 fs.ErrNotExist。
// 逆改点：先停句柄再写库 → 库失败时句柄已停，liveHandles 断言红。
func TestScheduler_SetEnabled_WritesDBFirst(t *testing.T) {
	t.Run("store failure leaves the engine untouched", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.start(t)
		require.Equal(t, 1, f.engine.liveHandles())

		writeErr := errors.New("disk full")
		f.store.mu.Lock()
		f.store.setEnabledErr = writeErr
		f.store.mu.Unlock()

		err := f.sched.SetEnabled(context.Background(), schedTaskID, false)
		require.ErrorIs(t, err, writeErr, "库写失败必须上抛")
		require.Equal(t, 1, f.engine.liveHandles(), "库写失败不得停句柄")
		require.Zero(t, f.engine.handleStops())
		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.True(t, rec.Enabled, "库值保持原值")
	})

	t.Run("concurrent retire maps to not exist", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
			Message: schedMessage, Enabled: true,
		})
		f.start(t)
		require.Equal(t, 1, f.engine.liveHandles())

		// 模拟另一进程在判定与写库之间把任务下线：SQL 的 RowsAffected()==0。
		f.store.mu.Lock()
		f.store.setEnabledErr = fs.ErrNotExist
		f.store.mu.Unlock()

		err := f.sched.SetEnabled(context.Background(), schedTaskID, false)
		require.ErrorIs(t, err, fs.ErrNotExist, "并发下线必须映射 fs.ErrNotExist（web 404）")
		require.Equal(t, 1, f.engine.liveHandles(), "写库失败不得停句柄")
		require.Zero(t, f.engine.handleStops())
		rec, ok := f.store.task(schedTaskID)
		require.True(t, ok)
		require.True(t, rec.Enabled, "库值保持原值")
	})
}

// TestScheduler_SetEnabled_ConcurrentToggles 覆盖 P-06：并发启停后，库值、句柄表与
// 引擎句柄数一致，不出现「库=1 而句柄已停」的分叉（-race 判定数据竞争）。
// 逆改点：判定 / 写库 / 动引擎不在同一把 stateMu 内 → -race 报竞争或终态分叉，断言红。
func TestScheduler_SetEnabled_ConcurrentToggles(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
	})
	f.start(t)

	const toggles = 32
	errs := make([]error, toggles)
	var wg sync.WaitGroup
	for i := range toggles {
		enabled := i%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.sched.SetEnabled(context.Background(), schedTaskID, enabled)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "并发启停第 %d 次调用失败", i)
	}

	rec, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	f.sched.stateMu.RLock()
	_, hasHandle := f.sched.handles[schedTaskID]
	f.sched.stateMu.RUnlock()
	require.Equal(t, rec.Enabled, hasHandle, "库值与句柄表必须一致（P-06）")
	wantHandles := 0
	if rec.Enabled {
		wantHandles = 1
	}
	require.Equal(t, wantHandles, f.engine.liveHandles(), "引擎句柄数必须与库值一致")
}

// TestScheduler_RunNow_ArchivedSessionPropagates 覆盖约束 43：手动补跑遇归档会话 →
// *SessionArchivedError 原样上抛（web 400），执行行收尾 skipped + 固定文案、run_id 置空。
// 逆改点：折成 ErrTaskBusy 或吞成成功 → 错误类型断言红；按 failed 收尾 → 状态断言红。
func TestScheduler_RunNow_ArchivedSessionPropagates(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, &SessionArchivedError{SessionID: schedSession}
	}
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
	})
	f.start(t)

	_, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	var archived *SessionArchivedError
	require.ErrorAs(t, err, &archived, "归档错误必须原样上抛（web 400）")
	require.NotErrorIs(t, err, ErrTaskBusy, "归档错误不得折成 ErrTaskBusy")
	// 归档互斥由 AgentService.Run 判定：运行端口被调用一次即被拒绝，不重建会话。
	require.EqualValues(t, 1, f.invoker.calls.Load())
	require.Equal(t, []ExecutionStatus{ExecutionSkipped}, f.store.snapshotFinished())
	calls := f.store.snapshotFinishCalls()
	require.Len(t, calls, 1)
	require.Equal(t, "会话已归档，本轮跳过", calls[0].ErrMsg)
	require.Empty(t, calls[0].RunID, "无 Run 行的终态行 run_id 必须置空")
	f.logs.requireRecord(t, slog.LevelWarn, "会话已归档，本轮跳过")

	rec, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	require.Equal(t, ExecutionSkipped, rec.LastStatus)
}

// TestScheduler_CheckOwnership 覆盖约束 30 / 42 的归属预检：候选 id 归属其他 Agent
// （含 retired 行）→ ErrTaskOwnerConflict；无行或同 owner（含 retired，复活）→ nil。
// 逆改点：只查 active 行 / 不查 agent_name → 冲突与放行两侧断言红的至少一侧。
func TestScheduler_CheckOwnership(t *testing.T) {
	t.Run("other owner conflicts including retired", func(t *testing.T) {
		store := newFakeTaskStore()
		store.seedTask(TaskRecord{TaskID: "shared", Agent: "other", Cron: schedCron, Zone: "UTC", Message: schedMessage})
		store.seedTask(TaskRecord{
			TaskID: "retired", Agent: "other", Cron: schedCron, Zone: "UTC", Message: schedMessage,
			RetiredAt: schedNow.Add(-time.Hour),
		})
		sched := newSchedulerWith(store, &countingInvoker{}, &fakeEngine{}, nil, nil, nil)

		require.ErrorIs(t, sched.CheckOwnership(context.Background(), schedAgent, []string{"shared"}),
			ErrTaskOwnerConflict)
		require.ErrorIs(t, sched.CheckOwnership(context.Background(), schedAgent, []string{"retired"}),
			ErrTaskOwnerConflict, "retired 行同样绑定原 owner（跨 Agent 一律拒绝）")
	})

	t.Run("no row or same owner allowed", func(t *testing.T) {
		store := newFakeTaskStore()
		store.seedTask(TaskRecord{TaskID: "own-active", Agent: schedAgent, Cron: schedCron, Zone: "UTC", Message: schedMessage})
		store.seedTask(TaskRecord{
			TaskID: "own-retired", Agent: schedAgent, Cron: schedCron, Zone: "UTC", Message: schedMessage,
			RetiredAt: schedNow.Add(-time.Hour),
		})
		sched := newSchedulerWith(store, &countingInvoker{}, &fakeEngine{}, nil, nil, nil)

		require.NoError(t, sched.CheckOwnership(context.Background(), schedAgent,
			[]string{"missing", "own-active", "own-retired"}), "无行或同 owner（含 retired）必须放行")
		require.NoError(t, sched.CheckOwnership(context.Background(), schedAgent, nil))
	})
}
