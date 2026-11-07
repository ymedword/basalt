package core

// 本文件是调度器在 US-24 后的**主判卷面**：触发权从进程内定时器搬到数据库之后，
// 验收语义按模块卡 §4.8 / Q3 做**等价替换**，不删减任何一条：
//
//	旧（进程内锁 / generation）                 新（数据库领取 / CAS / 租约）
//	---------------------------------------------------------------
//	两次触发只有一次 actually fires      →  (task_id, planned_at) 重复领取静默跳过
//	Stop 后 handle 被 Stop                →  后台到期扫描先停领取，在途按宽限收尾
//	Start 恢复上次 started 执行行         →  Start **不做**全局恢复（交回收器）
//	generation 递增使旧触发失效           →  领取事务的 CAS 推进 next_fire_at
//	6 段 cron / CRON_TZ                   →  同（断言改为看送进 CronEngine 的 spec）
//	定义哈希 / 退休 / 错失记录 / 唯一执行 →  同（断言落在仓储读数上）
//
// 判据怎么读：每条用例只断言「调度器按正确顺序调用了仓储」与「终态落账」两件事；
// 真实的唯一索引、行锁与 CAS 由 internal/storage/postgres 的集成用例守（两层各守一段）。
// This file owns the core-side scheduling order after US-24: claim, CAS advance, missed
// grace, gate, lease-backed execution and terminal bookkeeping. Database-level
// invariants (unique indexes, row locks, CAS) live in the postgres integration suite.

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 固定字面量：任务 id 参与代投正文（`【demo】定时任务 demo-1 执行完成`），改它会
// 连带改投递断言，所以集中在这里。
// Fixed fixtures; the task id is embedded in the delivery body text.
const (
	schedTaskID  = "demo-1"
	schedTaskID2 = "demo-2"
	schedAgent   = "demo"
	schedMessage = "巡检"
	schedCron    = "0 */5 * * * *"
	schedZone    = "UTC"

	// schedSession 是 DeriveSessionID("scheduler", "scheduler", "demo") 的字面量。
	// 调度执行必须落在**派生**会话上，否则同一 Agent 的定时任务各自开会话。
	schedSession = "scheduler-26ab61a473f25490"
)

var (
	// schedNow 是夹具时钟：调度器与假仓储看到同一个"现在"，到期 / 宽限判定才可判。
	schedNow  = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	schedStep = 5 * time.Minute

	schedLeaseTTL      = 3 * time.Hour
	schedRenewInterval = time.Hour

	schedWorker = WorkerIdentity{InstanceID: "worker-under-test", Name: "test", SchemaVersion: 12}
)

// scanBypassKey 是"这一轮扫描由用例直接驱动"的 ctx 标记：后台扫描门据此放行，
// 不与用例争夺同一批到期行。
type scanBypassKey struct{}

// demoTask 造一条标准任务定义（6 段 cron + 显式时区）。
func demoTask(id string) Schedule {
	return Schedule{ID: id, Cron: schedCron, Zone: schedZone, Message: schedMessage}
}

// schedCronSpec 与 workspace.CronSpec 同形（core 不得 import workspace）：
// 唯一出口拼 `CRON_TZ=<zone> <expr>`，空 cron / 已带前缀一律拒绝。
func schedCronSpec(s Schedule) (string, error) {
	if s.Cron == "" {
		return "", errors.New("cron is required")
	}
	if strings.HasPrefix(s.Cron, "CRON_TZ=") {
		return "", errors.New("cron must not carry CRON_TZ=")
	}
	zone := s.Zone
	if zone == "" {
		zone = "UTC"
	}
	return "CRON_TZ=" + zone + " " + s.Cron, nil
}

// ---------------------------------------------------------------------------
// 日志替身
// ---------------------------------------------------------------------------

// captureState 把 slog 记录留在内存里供断言（不写 stdout，跑测试时不刷屏）。
type captureState struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureState) Enabled(context.Context, slog.Level) bool { return true }

func (c *captureState) Handle(_ context.Context, rec slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec.Clone())
	return nil
}

func (c *captureState) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureState) WithGroup(string) slog.Handler      { return c }

// newCaptureLogger 返回记录器与它的读数句柄。
func newCaptureLogger() (*slog.Logger, *captureState) {
	state := &captureState{}
	return slog.New(state), state
}

// recordsOf 返回满足级别与消息的记录副本。
func (c *captureState) recordsOf(level slog.Level, msg string) []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []slog.Record
	for _, rec := range c.records {
		if rec.Level == level && rec.Message == msg {
			out = append(out, rec)
		}
	}
	return out
}

// requireRecord 断言至少有一条指定级别与消息的记录，并返回它。
func (c *captureState) requireRecord(t *testing.T, level slog.Level, msg string) slog.Record {
	t.Helper()
	recs := c.recordsOf(level, msg)
	require.NotEmpty(t, recs, "缺少日志：level=%s msg=%q", level, msg)
	return recs[0]
}

// hasRecord 报告是否出现过指定级别与消息。
func (c *captureState) hasRecord(level slog.Level, msg string) bool {
	return len(c.recordsOf(level, msg)) > 0
}

// countLevel 数某级别的记录条数（用于"无声跳过不得记 ERROR"这类断言）。
func (c *captureState) countLevel(level slog.Level) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, rec := range c.records {
		if rec.Level == level {
			n++
		}
	}
	return n
}

// text 把全部消息与属性拼成一段文本，用于"日志里不得出现正文"这类断言。
func (c *captureState) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, rec := range c.records {
		b.WriteString(rec.Message)
		b.WriteString(" ")
		rec.Attrs(func(attr slog.Attr) bool {
			b.WriteString(attr.Key)
			b.WriteString("=")
			b.WriteString(attr.Value.String())
			b.WriteString(" ")
			return true
		})
		b.WriteString("\n")
	}
	return b.String()
}

// attrOf 取记录上的某个属性值。
func attrOf(rec slog.Record, key string) (slog.Value, bool) {
	var out slog.Value
	found := false
	rec.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			out, found = attr.Value, true
			return false
		}
		return true
	})
	return out, found
}

// ---------------------------------------------------------------------------
// 运行面与 cron 引擎替身
// ---------------------------------------------------------------------------

// countingInvoker 记录每次运行请求；run 未设置时返回 completed。
type countingInvoker struct {
	calls atomic.Int32

	mu   sync.Mutex
	reqs []RunRequest
	run  func(context.Context, RunRequest) (RunResult, error)
}

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

// requests 返回运行请求副本。
func (i *countingInvoker) requests() []RunRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]RunRequest(nil), i.reqs...)
}

// fakeEngine 只实现 US-24 保留的两个方法；Next 固定返回 from + schedStep，
// 并记录收到的 spec 与 from（6 段 cron / CRON_TZ 的判据就从这里读）。
type fakeEngine struct {
	mu       sync.Mutex
	specs    []string
	froms    []time.Time
	nextErr  error
	next     func(from time.Time) time.Time
	occurred []time.Time

	// 错过触发点的有界枚举判据：调用次数与 limit 入参。
	// 1000 上限由 schedulerMissedOccurrences 定义，测试从这里反查而不是复制常量。
	occurrenceCalls  int
	occurrenceLimits []int
}

func (e *fakeEngine) Next(spec string, from time.Time) (time.Time, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.specs = append(e.specs, spec)
	e.froms = append(e.froms, from)
	if e.nextErr != nil {
		return time.Time{}, e.nextErr
	}
	if e.next != nil {
		return e.next(from), nil
	}
	return from.Add(schedStep), nil
}

func (e *fakeEngine) Occurrences(_ string, _, _ time.Time, limit int) []time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.occurrenceCalls++
	e.occurrenceLimits = append(e.occurrenceLimits, limit)
	return append([]time.Time(nil), e.occurred...)
}

// occurrenceReadings 返回枚举调用次数与每次收到的 limit 副本。
func (e *fakeEngine) occurrenceReadings() (int, []int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.occurrenceCalls, append([]int(nil), e.occurrenceLimits...)
}

// readings 返回收到的 spec 与 from 副本。
func (e *fakeEngine) readings() ([]string, []time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.specs...), append([]time.Time(nil), e.froms...)
}

// ---------------------------------------------------------------------------
// 任务仓储替身（同时实现 TaskStore 与 LeaseStore）
// ---------------------------------------------------------------------------

type applyCall struct {
	Agent     string
	UpsertIDs []string
	RetireIDs []string
}

type claimCall struct {
	TaskID     string
	PlannedAt  time.Time
	NextFireAt time.Time
	Owner      WorkerIdentity
	TTL        time.Duration
}

type missedCall struct {
	TaskID    string
	PlannedAt time.Time
	Missed    int
	Reason    string
}

type busyCall struct {
	TaskID    string
	PlannedAt time.Time
}

type nextFireWrite struct {
	TaskID   string
	Expected *time.Time
	Next     time.Time
}

type markCall struct {
	TaskID string
	At     time.Time
	Status ExecutionStatus
}

// finishCall 是一次终态写的读数：CtxErr / Deadline 用来证明收尾用的是
// WithoutCancel + 独立短超时，而不是可能已超时的运行 ctx。
type finishCall struct {
	ID          int64
	Token       LeaseToken
	Status      ExecutionStatus
	ErrMsg      string
	At          time.Time
	CtxErr      error
	Deadline    time.Time
	HasDeadline bool
}

// fakeTaskStore 是 TaskStore + LeaseStore 的内存替身：maps 模拟行与部分唯一索引，
// 记录切片保留每次调用的入参供断言，故障开关模拟仓储失败。
type fakeTaskStore struct {
	mu sync.Mutex

	tasks    map[string]TaskRecord
	nextFire map[string]time.Time // 缺席 = NULL
	active   map[string]int64     // taskID → 在途执行 id（模拟 ux_task_executions_active）
	leased   map[int64]LeaseToken // 执行 id → 租约 token
	terminal map[int64]ExecutionStatus
	claimed  map[string]bool // task_id + planned_at → 已被领取
	runRows  map[int64]string
	execTask map[int64]string
	renewed  map[LeaseToken]Lease
	cancels  map[LeaseToken]bool

	executions map[string][]ExecutionRecord
	started    []ExecutionRecord
	finished   []ExecutionStatus
	finishLog  []finishCall
	marks      []markCall
	applied    []applyCall
	claims     []claimCall
	missed     []missedCall
	busy       []busyCall
	nextWrites []nextFireWrite
	skipped    []ExecutionRecord
	releases   []LeaseToken
	renewCalls int

	nextExecID int64

	nowFn func() time.Time

	// 后台扫描门：Start 的到期扫描第一轮立刻跑，会和用例自己的 dispatchOnce 抢同一批
	// 到期行，也会让 executionWG 的 Add/Wait 交错。夹具默认把后台扫描挡在门上，用例
	// 用 f.scan / f.fire 明确驱动，时序才可判。
	scanPaused    atomic.Bool
	scanEntered   chan struct{}
	scanRelease   chan struct{}
	scanEnterOnce sync.Once
	scanRelOnce   sync.Once

	// 故障注入与注入点。
	listErr           error
	dispatchErr       error
	applyErr          error
	applyErrOnCall    int
	applyCalls        int
	setEnabledErr     error
	claimErr          error
	setNextFireErr    error
	missedErr         error
	busyErr           error
	startExecutionErr error
	forced            []DueTask
	dispatchHook      func()
}

// newFakeTaskStore 构造空仓储。
func newFakeTaskStore() *fakeTaskStore {
	return &fakeTaskStore{
		tasks:       make(map[string]TaskRecord),
		nextFire:    make(map[string]time.Time),
		active:      make(map[string]int64),
		leased:      make(map[int64]LeaseToken),
		terminal:    make(map[int64]ExecutionStatus),
		claimed:     make(map[string]bool),
		runRows:     make(map[int64]string),
		execTask:    make(map[int64]string),
		renewed:     make(map[LeaseToken]Lease),
		cancels:     make(map[LeaseToken]bool),
		executions:  make(map[string][]ExecutionRecord),
		nowFn:       time.Now,
		scanEntered: make(chan struct{}),
		scanRelease: make(chan struct{}),
	}
}

// setClock 在构造后统一夹具时钟与调度器时钟。
func (s *fakeTaskStore) setClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.nowFn = now
	}
}

// armScanGate 挂上后台扫描门（必须在 Start 之前调用）。
func (s *fakeTaskStore) armScanGate() { s.scanPaused.Store(true) }

// resumeScan 放开门并解除暂停（幂等）。
func (s *fakeTaskStore) resumeScan() {
	s.scanPaused.Store(false)
	s.scanRelOnce.Do(func() { close(s.scanRelease) })
}

// awaitScanEntered 等后台扫描真正抵达门口；只有 Start 成功后才应调用。
func (s *fakeTaskStore) awaitScanEntered(t *testing.T) {
	t.Helper()
	select {
	case <-s.scanEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("后台到期扫描未在第一轮进入等待门（Start 没有启动 dispatchLoop？）")
	}
}

// gateScan 是扫描门的实现：被用例直接驱动（ctx 带标记）时放行；否则等待放行或 ctx 取消。
// 等 ctx 取消是必须的——Stop 依赖它把扫描放出来，否则停机死锁。
func (s *fakeTaskStore) gateScan(ctx context.Context) error {
	if !s.scanPaused.Load() || ctx.Value(scanBypassKey{}) != nil {
		return nil
	}
	s.scanEnterOnce.Do(func() { close(s.scanEntered) })
	select {
	case <-s.scanRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// seedTask 预置一行任务定义；空缺的归属按 SQLite 谱系映射补成管理员（与迁移常量一致）。
func (s *fakeTaskStore) seedTask(rec TaskRecord) {
	if rec.CallerID == "" {
		rec.CallerID = LocalAdminCallerID
	}
	if rec.AuthorizingKind == "" {
		rec.AuthorizingKind = AuthorizingKindAdminBuiltin
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[rec.TaskID] = rec
}

// task 读回一行任务定义。
func (s *fakeTaskStore) task(taskID string) (TaskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[taskID]
	return rec, ok
}

// mutateTask 就地改写一行（夹具专用，绕开生产路径的可见性判定）。
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

// nextFireOf 读回 next_fire_at。
func (s *fakeTaskStore) nextFireOf(taskID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.nextFire[taskID]
	return at, ok
}

// setNextFireDirect 直写 next_fire_at（夹具专用；生产写一律经 CAS）。
func (s *fakeTaskStore) setNextFireDirect(taskID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextFire[taskID] = at
}

func (s *fakeTaskStore) snapshotStarted() []ExecutionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ExecutionRecord(nil), s.started...)
}

func (s *fakeTaskStore) snapshotSkipped() []ExecutionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ExecutionRecord(nil), s.skipped...)
}

func (s *fakeTaskStore) snapshotFinished() []ExecutionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ExecutionStatus(nil), s.finished...)
}

func (s *fakeTaskStore) snapshotFinishCalls() []finishCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]finishCall(nil), s.finishLog...)
}

func (s *fakeTaskStore) snapshotMarks() []markCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]markCall(nil), s.marks...)
}

func (s *fakeTaskStore) snapshotApplied() []applyCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]applyCall(nil), s.applied...)
}

func (s *fakeTaskStore) snapshotClaims() []claimCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]claimCall(nil), s.claims...)
}

func (s *fakeTaskStore) snapshotMissed() []missedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]missedCall(nil), s.missed...)
}

func (s *fakeTaskStore) snapshotBusy() []busyCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]busyCall(nil), s.busy...)
}

func (s *fakeTaskStore) snapshotNextWrites() []nextFireWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]nextFireWrite(nil), s.nextWrites...)
}

// record 追加一条执行行。
func (s *fakeTaskStore) record(rec ExecutionRecord) {
	s.executions[rec.TaskID] = append(s.executions[rec.TaskID], rec)
	s.execTask[rec.ID] = rec.TaskID
	if rec.Status == ExecutionStarted && rec.Trigger == TriggerCron {
		s.started = append(s.started, rec)
	}
}

// claimKey 复刻 `(task_id, planned_at)` 唯一键。
func claimKey(taskID string, plannedAt time.Time) string {
	return taskID + "\x1f" + strconv.FormatInt(plannedAt.UnixMilli(), 10)
}

// nextID 分配执行行 id（调用方持锁）。
func (s *fakeTaskStore) nextID() int64 {
	s.nextExecID++
	return s.nextExecID
}

// leaseToken 生成与生产同形的执行租约凭证（调用方持锁）。
func leaseToken(id int64, owner WorkerIdentity) LeaseToken {
	return LeaseToken{
		Kind:           LeaseScheduleExecution,
		ResourceID:     strconv.FormatInt(id, 10),
		OwnerWorkerID:  owner.InstanceID,
		OwnershipEpoch: 1,
	}
}

// ===== TaskStore =====

func (s *fakeTaskStore) ListTasks(context.Context) ([]TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]TaskRecord, 0, len(s.tasks))
	for _, rec := range s.tasks {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out, nil
}

// ApplyAgentTasks 模拟单事务：先做归属预检，冲突即整体不生效；新行 enabled=true
// （schema 缺省），既有行只更新定义列，保留 enabled / last_* / created_at 与归属三列
// （生产 SQL 的 ON CONFLICT 逐列口径）。
func (s *fakeTaskStore) ApplyAgentTasks(_ context.Context, agent string, upserts []TaskRecord,
	retireIDs []string, now time.Time,
) error {
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
	s.applied = append(s.applied, applyCall{
		Agent:     agent,
		UpsertIDs: ids,
		RetireIDs: append([]string(nil), retireIDs...),
	})
	return nil
}

func (s *fakeTaskStore) SetTaskEnabled(_ context.Context, taskID string, enabled bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *fakeTaskStore) MarkTaskRun(_ context.Context, taskID string, at time.Time, status ExecutionStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks = append(s.marks, markCall{TaskID: taskID, At: at, Status: status})
	if rec, ok := s.tasks[taskID]; ok {
		rec.LastRunAt = at
		s.tasks[taskID] = rec
	}
	return nil
}

func (s *fakeTaskStore) ListDispatchable(ctx context.Context, limit int) ([]DueTask, error) {
	if err := s.gateScan(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.dispatchErr != nil {
		err := s.dispatchErr
		s.mu.Unlock()
		return nil, err
	}
	var out []DueTask
	if len(s.forced) > 0 {
		out = append(out, s.forced...)
	} else {
		now := s.nowFn()
		for _, rec := range s.tasks {
			if !rec.Enabled || !rec.RetiredAt.IsZero() {
				continue
			}
			at, has := s.nextFire[rec.TaskID]
			if has && at.After(now) {
				continue
			}
			out = append(out, DueTask{Task: rec, NextFireAt: at})
		}
	}
	hook := s.dispatchHook
	s.mu.Unlock()

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].NextFireAt, out[j].NextFireAt
		if a.IsZero() != b.IsZero() {
			return a.IsZero()
		}
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].Task.TaskID < out[j].Task.TaskID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	// 注入点位于"读到期集合"与"领取"之间：模拟另一 Worker 抢先推进 / 领取。
	if hook != nil {
		hook()
	}
	return out, nil
}

func (s *fakeTaskStore) SetNextFire(_ context.Context, taskID string, expected *time.Time, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[taskID]; !ok {
		return fs.ErrNotExist
	}
	if s.setNextFireErr != nil {
		return s.setNextFireErr
	}
	if expected != nil {
		cur, has := s.nextFire[taskID]
		if !has || !cur.Equal(*expected) {
			return ErrStaleNextFire
		}
	}
	s.nextFire[taskID] = next
	s.nextWrites = append(s.nextWrites, nextFireWrite{TaskID: taskID, Expected: expected, Next: next})
	return nil
}

// ClaimDueTrigger 模拟领取事务：预检归属 → 忙判定 → 唯一键去重 → CAS 推进 → 落行 + 租约。
func (s *fakeTaskStore) ClaimDueTrigger(_ context.Context, taskID string, plannedAt, nextFireAt time.Time,
	owner WorkerIdentity, ttl time.Duration,
) (DueExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return DueExecution{}, s.claimErr
	}
	row, ok := s.tasks[taskID]
	if !ok || !row.RetiredAt.IsZero() {
		return DueExecution{}, fs.ErrNotExist
	}
	if _, busy := s.active[taskID]; busy {
		return DueExecution{}, ErrTaskBusy
	}
	if s.claimed[claimKey(taskID, plannedAt)] {
		return DueExecution{}, ErrTriggerAlreadyClaimed
	}
	if cur, has := s.nextFire[taskID]; !has || !cur.Equal(plannedAt) {
		return DueExecution{}, ErrStaleNextFire
	}

	id := s.nextID()
	// run_id 由领取事务分配，形状必须与生产仓储同口径（契约 §5）。
	// The claim transaction owns the run_id; mirror the production shape.
	runID := NewRunID("scheduler", plannedAt.UTC(), RunIDSuffix())
	planned := plannedAt
	rec := ExecutionRecord{
		ID:        id,
		TaskID:    taskID,
		RunID:     runID,
		Status:    ExecutionStarted,
		Trigger:   TriggerCron,
		StartedAt: plannedAt,
		PlannedAt: &planned,
	}
	s.claimed[claimKey(taskID, plannedAt)] = true
	s.nextFire[taskID] = nextFireAt
	s.record(rec)
	s.active[taskID] = id
	s.runRows[id] = runID
	token := leaseToken(id, owner)
	s.leased[id] = token
	s.claims = append(s.claims, claimCall{
		TaskID: taskID, PlannedAt: plannedAt, NextFireAt: nextFireAt, Owner: owner, TTL: ttl,
	})
	return DueExecution{Task: row, ExecutionID: id, PlannedAt: plannedAt, Lease: token, RunID: runID}, nil
}

func (s *fakeTaskStore) RecordMissedTrigger(_ context.Context, taskID string, plannedAt, nextFireAt time.Time,
	missed int, reason string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[taskID]; !ok {
		return fs.ErrNotExist
	}
	if s.missedErr != nil {
		return s.missedErr
	}
	if cur, has := s.nextFire[taskID]; !has || !cur.Equal(plannedAt) {
		return ErrStaleNextFire
	}
	s.nextFire[taskID] = nextFireAt
	planned := plannedAt
	rec := ExecutionRecord{
		TaskID: taskID, Status: ExecutionSkipped, Trigger: TriggerCron,
		ErrorMessage: reason, PlannedAt: &planned, FinishedAt: plannedAt,
	}
	s.record(rec)
	s.skipped = append(s.skipped, rec)
	s.missed = append(s.missed, missedCall{TaskID: taskID, PlannedAt: plannedAt, Missed: missed, Reason: reason})
	return nil
}

func (s *fakeTaskStore) RecordBusyTrigger(_ context.Context, taskID string, plannedAt, nextFireAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[taskID]; !ok {
		return fs.ErrNotExist
	}
	if s.busyErr != nil {
		return s.busyErr
	}
	if cur, has := s.nextFire[taskID]; !has || !cur.Equal(plannedAt) {
		return ErrStaleNextFire
	}
	s.nextFire[taskID] = nextFireAt
	planned := plannedAt
	rec := ExecutionRecord{
		TaskID: taskID, Status: ExecutionSkipped, Trigger: TriggerCron,
		ErrorMessage: schedulerLockMissMessage, PlannedAt: &planned, FinishedAt: plannedAt,
	}
	s.record(rec)
	s.skipped = append(s.skipped, rec)
	s.busy = append(s.busy, busyCall{TaskID: taskID, PlannedAt: plannedAt})
	return nil
}

func (s *fakeTaskStore) StartExecution(_ context.Context, rec ExecutionRecord, owner WorkerIdentity,
	ttl time.Duration,
) (int64, LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startExecutionErr != nil {
		return 0, LeaseToken{}, s.startExecutionErr
	}
	if _, busy := s.active[rec.TaskID]; busy {
		return 0, LeaseToken{}, ErrTaskBusy
	}
	id := s.nextID()
	rec.ID = id
	if rec.StartedAt.IsZero() {
		rec.StartedAt = s.nowFn()
	}
	s.record(rec)
	s.active[rec.TaskID] = id
	s.runRows[id] = rec.RunID
	token := leaseToken(id, owner)
	s.leased[id] = token
	return id, token, nil
}

// FinishExecution 带 token 做 fencing：已终态幂等返回；token 不符即 ErrLeaseLost；
// 成功则释放租约并同步 last_status（生产 SQL 的同一口径）。
func (s *fakeTaskStore) FinishExecution(ctx context.Context, id int64, token LeaseToken,
	status ExecutionStatus, duration time.Duration, errMsg string, at time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, hasDeadline := ctx.Deadline()
	s.finishLog = append(s.finishLog, finishCall{
		ID: id, Token: token, Status: status, ErrMsg: errMsg, At: at,
		CtxErr: ctx.Err(), Deadline: deadline, HasDeadline: hasDeadline,
	})
	if _, done := s.terminal[id]; done {
		return nil
	}
	leased, ok := s.leased[id]
	if !ok || leased != token {
		return ErrLeaseLost
	}
	s.terminal[id] = status
	delete(s.leased, id)
	taskID := s.execTask[id]
	if execID, busy := s.active[taskID]; busy && execID == id {
		delete(s.active, taskID)
	}
	rows := s.executions[taskID]
	for i := range rows {
		if rows[i].ID == id {
			rows[i].Status = status
			rows[i].DurationMS = duration.Milliseconds()
			rows[i].ErrorMessage = errMsg
			rows[i].FinishedAt = at
		}
	}
	s.executions[taskID] = rows
	if rec, ok := s.tasks[taskID]; ok {
		rec.LastStatus = status
		rec.LastRunAt = at
		s.tasks[taskID] = rec
	}
	s.finished = append(s.finished, status)
	return nil
}

func (s *fakeTaskStore) RecordSkipped(_ context.Context, rec ExecutionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record(rec)
	s.skipped = append(s.skipped, rec)
	return nil
}

func (s *fakeTaskStore) ListExecutions(_ context.Context, taskID string, limit int) ([]ExecutionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := append([]ExecutionRecord(nil), s.executions[taskID]...)
	if limit > 0 && len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	return rows, nil
}

// ===== LeaseStore（调度器只用到 Renew / Release 的"不挡路"版本） =====

func (s *fakeTaskStore) Acquire(_ context.Context, kind LeaseKind, resourceID string,
	owner WorkerIdentity, ttl time.Duration,
) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowFn()
	token := LeaseToken{Kind: kind, ResourceID: resourceID, OwnerWorkerID: owner.InstanceID, OwnershipEpoch: 1}
	lease := Lease{LeaseToken: token, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(ttl)}
	s.renewed[token] = lease
	return lease, nil
}

func (s *fakeTaskStore) Renew(_ context.Context, token LeaseToken, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewCalls++
	lease, ok := s.renewed[token]
	if !ok {
		return Lease{}, ErrLeaseLost
	}
	lease.RenewedAt = s.nowFn()
	lease.ExpiresAt = lease.RenewedAt.Add(ttl)
	lease.CancelRequested = s.cancels[token]
	s.renewed[token] = lease
	return lease, nil
}

func (s *fakeTaskStore) Release(_ context.Context, token LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, token)
	delete(s.renewed, token)
	return nil
}

func (s *fakeTaskStore) FencedTx(context.Context, LeaseToken, func(context.Context, Tx) error) error {
	return ErrLeaseFenceUnavailable
}

// ---------------------------------------------------------------------------
// 调度夹具
// ---------------------------------------------------------------------------

// schedFixture 把仓储 / 运行面 / 引擎 / 日志与调度器绑在一起。
type schedFixture struct {
	store   *fakeTaskStore
	invoker *countingInvoker
	engine  *fakeEngine
	logs    *captureState
	sched   *Scheduler

	// fires 记录每个任务被驱动过几次，用来给每次触发不同的 planned_at
	// （`(task_id, planned_at)` 去重键要求同一任务的两次触发不能共用同一个时刻）。
	fires map[string]int
}

// newSchedulerWith 组装调度器：注册一个 Agent、接上执行权装配与固定的到期扫描节奏。
func newSchedulerWith(store TaskStore, invoker RunInvoker, engine CronEngine, logger *slog.Logger,
	schedules []Schedule, tune func(*SchedulerOptions),
) *Scheduler {
	opts := SchedulerOptions{
		Store:   store,
		Invoker: invoker,
		Engine:  engine,
		Entries: []AgentEntry{{
			Name:       schedAgent,
			Registered: true,
			Profile:    &Profile{Schedules: schedules},
		}},
		CronSpec: schedCronSpec,
		Logger:   logger,
		// 固定时钟：到期 / 宽限 / 收尾时刻都可判。
		Now:           func() time.Time { return schedNow },
		Worker:        schedWorker,
		LeaseTTL:      schedLeaseTTL,
		RenewInterval: schedRenewInterval,
		// 后备扫描不参与判卷：门会挡住它，且 1h 的轮询间隔保证它不会再触发第二轮。
		PollInterval: time.Hour,
		// MissedGrace 的零值是"零宽限"，必须显式给缺省 300s，否则每次触发都判错失。
		MissedGrace: defaultSchedulerMissedGrace,
		StopGrace:   100 * time.Millisecond,
	}
	if leases, ok := store.(LeaseStore); ok {
		opts.Leases = leases
	}
	if tune != nil {
		tune(&opts)
	}
	if fs, ok := store.(*fakeTaskStore); ok {
		fs.setClock(opts.Now)
	}
	return NewScheduler(opts)
}

// newSchedFixture 组装一套默认夹具。
func newSchedFixture(t *testing.T, schedules []Schedule, tune func(*SchedulerOptions)) *schedFixture {
	t.Helper()
	store := newFakeTaskStore()
	invoker := &countingInvoker{}
	engine := &fakeEngine{}
	logger, logs := newCaptureLogger()
	return &schedFixture{
		store:   store,
		invoker: invoker,
		engine:  engine,
		logs:    logs,
		sched:   newSchedulerWith(store, invoker, engine, logger, schedules, tune),
	}
}

// arm 挂上后台扫描门并登记清理：先 Stop（取消 dispatchCtx 把门后的扫描放走），
// 再放开门——顺序反了会让后台扫描在停机后继续跑。
func (f *schedFixture) arm(t *testing.T) {
	t.Helper()
	f.store.armScanGate()
	t.Cleanup(func() {
		f.sched.Stop()
		f.store.resumeScan()
	})
}

// start 启动调度器并等后台扫描抵达门口。
func (f *schedFixture) start(t *testing.T) {
	t.Helper()
	f.arm(t)
	require.NoError(t, f.sched.Start(context.Background()))
	f.store.awaitScanEntered(t)
}

// startErr 启动调度器并返回错误（预期失败的用例不走等待门）。
func (f *schedFixture) startErr(t *testing.T) error {
	t.Helper()
	f.arm(t)
	return f.sched.Start(context.Background())
}

// directCtx 标记"本轮扫描由用例驱动"，让后台扫描门放行。
func (f *schedFixture) directCtx() context.Context {
	return context.WithValue(context.Background(), scanBypassKey{}, true)
}

// seed 把任务的 next_fire_at 拨到"刚刚到期"，返回本次计划时刻（每次调用都不同）。
func (f *schedFixture) seed(t *testing.T, taskID string) time.Time {
	t.Helper()
	if f.fires == nil {
		f.fires = make(map[string]int)
	}
	f.fires[taskID]++
	planned := schedNow.Add(-time.Duration(f.fires[taskID]) * time.Second)
	f.seedAt(t, taskID, planned)
	return planned
}

// seedAt 用指定计划时刻制造到期状态。
func (f *schedFixture) seedAt(t *testing.T, taskID string, planned time.Time) {
	t.Helper()
	require.NotEmpty(t, taskID)
	f.store.setNextFireDirect(taskID, planned)
}

// scan 驱动一轮到期扫描（绕过后台扫描门）。
func (f *schedFixture) scan(t *testing.T) {
	t.Helper()
	require.NoError(t, f.sched.dispatchOnce(f.directCtx()))
}

// wait 等在途执行全部收尾；与 WaitGroup.Add 串行的前提是后台扫描已被门挡住。
func (f *schedFixture) wait(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		f.sched.executionWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("在途执行未在 5s 内结束")
	}
}

// claim 只领取、不等收尾（阻塞型运行面用例用它制造在途执行）。
func (f *schedFixture) claim(t *testing.T, taskID string) time.Time {
	t.Helper()
	planned := f.seed(t, taskID)
	f.scan(t)
	return planned
}

// fire 触发一次到点执行并等它收尾，返回计划时刻。
func (f *schedFixture) fire(t *testing.T, taskID string) time.Time {
	t.Helper()
	planned := f.claim(t, taskID)
	f.wait(t)
	return planned
}

// ===========================================================================
// 到期扫描与领取
// ===========================================================================

// TestScheduler_Dispatch_DerivesSessionAndRunID —— 定时执行落在**派生**会话上，
// run_id 来自领取事务，两次触发各有一条执行行。
func TestScheduler_Dispatch_DerivesSessionAndRunID(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)}, nil)
	f.start(t)
	f.fire(t, schedTaskID)
	f.fire(t, schedTaskID2)

	reqs := f.invoker.requests()
	require.Len(t, reqs, 2)
	require.Equal(t, schedSession, reqs[0].SessionID, "定时执行必须落在派生会话上")
	require.Equal(t, "scheduler", reqs[0].Channel)
	require.Equal(t, "scheduler", reqs[0].UserID)
	require.Equal(t, TriggerCron, reqs[0].Trigger)
	require.Equal(t, schedMessage, reqs[0].Message)
	require.Equal(t, schedAgent, reqs[0].Agent)
	// RunID 形状是公共契约（§5）：<渠道>-<毫秒>-<6 位 base36 小写>。
	// 只断言 NotEmpty 会让退化格式（缺渠道 / 缺随机段）悄悄过关。
	runIDShape := regexp.MustCompile(`^scheduler-\d{13}-[a-z0-9]{6}$`)
	require.Regexp(t, runIDShape, reqs[0].RunID, "RunID 形状必须是 scheduler-<13 位毫秒>-<6 位 base36>")
	require.Regexp(t, runIDShape, reqs[1].RunID)
	require.NotEqual(t, reqs[0].RunID, reqs[1].RunID, "两次触发必须是两个 Run")

	require.Equal(t, LocalAdminCallerID, reqs[0].Principal.CallerID)
	require.Equal(t, ActorScheduler, reqs[0].Principal.Actor)

	started := f.store.snapshotStarted()
	require.Len(t, started, 2)
	require.Equal(t, reqs[0].RunID, started[0].RunID, "执行行必须带领取事务分配的 run_id")
	require.Regexp(t, runIDShape, started[0].RunID, "执行行落库的 run_id 与运行请求同一形状")
	require.Equal(t, TriggerCron, started[0].Trigger)
	require.NotNil(t, started[0].PlannedAt)
}

// TestScheduler_Dispatch_ExecutesAndRecords —— 领取 → 运行 → fencing 收尾 → 推进
// next_fire_at 的全链路读数。
func TestScheduler_Dispatch_ExecutesAndRecords(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	planned := f.fire(t, schedTaskID)

	require.Equal(t, int32(1), f.invoker.calls.Load())
	claims := f.store.snapshotClaims()
	require.Len(t, claims, 1)
	require.Equal(t, schedTaskID, claims[0].TaskID)
	require.True(t, planned.Equal(claims[0].PlannedAt), "领取的计划时刻必须是到期扫描读到的时刻")
	require.Equal(t, schedWorker, claims[0].Owner, "领取必须写入本进程的 Worker 身份")
	require.Equal(t, schedLeaseTTL, claims[0].TTL)

	marks := f.store.snapshotMarks()
	require.Len(t, marks, 1)
	require.Equal(t, ExecutionStarted, marks[0].Status)

	require.Equal(t, []ExecutionStatus{ExecutionCompleted}, f.store.snapshotFinished())
	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Empty(t, finishes[0].ErrMsg)
	require.NotZero(t, finishes[0].ID)

	// 收尾必须释放执行租约，否则回收器会误判"仍有在途"。
	f.store.mu.Lock()
	require.Empty(t, f.store.leased, "终态写必须释放执行租约")
	f.store.mu.Unlock()

	next, ok := f.store.nextFireOf(schedTaskID)
	require.True(t, ok)
	require.True(t, schedNow.Add(schedStep).Equal(next), "领取事务必须把 next_fire_at 推进到未来")
}

// TestScheduler_Dispatch_ReadsTaskFromStore —— 任务状态一律从库里读：停用行不领取、
// 读失败整体 fail-closed、定义变化下一轮即生效（没有进程内句柄需要换）。
func TestScheduler_Dispatch_ReadsTaskFromStore(t *testing.T) {
	t.Run("disabled rows are not dispatched", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		f.store.mutateTask(schedTaskID, func(rec *TaskRecord) { rec.Enabled = false })
		f.seed(t, schedTaskID)
		f.scan(t)
		f.wait(t)

		require.Empty(t, f.store.snapshotClaims(), "停用行不得被领取")
		require.Zero(t, f.invoker.calls.Load())
	})

	t.Run("list failure fails closed", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		f.seed(t, schedTaskID)
		f.store.dispatchErr = errors.New("connection reset")
		err := f.sched.dispatchOnce(f.directCtx())
		require.ErrorContains(t, err, "list dispatchable")
		require.Zero(t, f.invoker.calls.Load())
	})

	t.Run("definition change applies to the next dispatch", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		f.fire(t, schedTaskID)

		updated := demoTask(schedTaskID)
		updated.Message = "复核"
		require.NoError(t, f.sched.ReconcileAgent(context.Background(), schedAgent,
			[]Schedule{updated}))
		f.fire(t, schedTaskID)

		reqs := f.invoker.requests()
		require.Len(t, reqs, 2)
		require.Equal(t, "复核", reqs[1].Message, "新定义必须在下一轮扫描即生效")
	})
}

// TestScheduler_Dispatch_ScheduleGateSkipsInactiveAgent —— 生命周期门在任何执行落库
// **之前**求值：本轮不排期只推进 next_fire_at，不写执行行、不发起运行。
func TestScheduler_Dispatch_ScheduleGateSkipsInactiveAgent(t *testing.T) {
	var enabled atomic.Bool
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.ScheduleGate = func(context.Context, string) bool { return enabled.Load() }
	})
	f.start(t)
	planned := f.seed(t, schedTaskID)
	f.scan(t)
	f.wait(t)

	require.Empty(t, f.store.snapshotClaims(), "未启用时不得领取")
	require.Zero(t, f.invoker.calls.Load())
	require.Empty(t, f.store.snapshotStarted(), "未启用时不得写执行行")
	writes := f.store.snapshotNextWrites()
	require.Len(t, writes, 1)
	require.NotNil(t, writes[0].Expected, "门跳过必须走 CAS 推进")
	require.True(t, planned.Equal(*writes[0].Expected))
	require.True(t, schedNow.Add(schedStep).Equal(writes[0].Next))
	f.logs.requireRecord(t, slog.LevelDebug, "Agent 未启用，本轮不排期")

	enabled.Store(true)
	f.fire(t, schedTaskID)
	require.Equal(t, int32(1), f.invoker.calls.Load(), "启用后下一次触发必须能跑")
}

// TestScheduler_Dispatch_FailureMapping —— Run 结果到执行行终态的映射（约束 10）。
func TestScheduler_Dispatch_FailureMapping(t *testing.T) {
	cases := []struct {
		name   string
		run    func(context.Context, RunRequest) (RunResult, error)
		status ExecutionStatus
		errMsg string
	}{
		{
			name: "run error",
			run: func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("模型调用失败")
			},
			status: ExecutionFailed,
			errMsg: "模型调用失败",
		},
		{
			name: "result failed keeps reason",
			run: func(_ context.Context, req RunRequest) (RunResult, error) {
				return RunResult{RunID: req.RunID, Status: StatusFailed, Reason: "上下文准入拒绝"}, nil
			},
			status: ExecutionFailed,
			errMsg: "上下文准入拒绝",
		},
		{
			name: "cancelled maps to failed",
			run: func(_ context.Context, req RunRequest) (RunResult, error) {
				return RunResult{RunID: req.RunID, Status: StatusCancelled}, nil
			},
			status: ExecutionFailed,
		},
		{
			name: "limit reached maps to completed",
			run: func(_ context.Context, req RunRequest) (RunResult, error) {
				return RunResult{RunID: req.RunID, Status: StatusLimitReached}, nil
			},
			status: ExecutionCompleted,
		},
		{
			name: "running at return is a failure",
			run: func(_ context.Context, req RunRequest) (RunResult, error) {
				return RunResult{RunID: req.RunID, Status: StatusRunning}, nil
			},
			status: ExecutionFailed,
			errMsg: "调度运行未在时限内结束",
		},
		{
			name: "unknown status fails",
			run: func(_ context.Context, req RunRequest) (RunResult, error) {
				return RunResult{RunID: req.RunID, Status: RunStatus("weird")}, nil
			},
			status: ExecutionFailed,
			errMsg: `未知运行状态 "weird"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
			f.invoker.run = tc.run
			f.start(t)
			f.fire(t, schedTaskID)

			finishes := f.store.snapshotFinishCalls()
			require.Len(t, finishes, 1)
			require.Equal(t, tc.status, finishes[0].Status)
			require.Equal(t, tc.errMsg, finishes[0].ErrMsg)
		})
	}
}

// TestScheduler_Dispatch_AdmissionErrorRecordsFailed —— 准入类错误（实例满 / 授权失效）
// 同样落 failed 执行行，不得静默丢弃。
func TestScheduler_Dispatch_AdmissionErrorRecordsFailed(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	admission := &InstanceFullError{Limit: 4}
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, admission
	}
	f.start(t)
	f.fire(t, schedTaskID)

	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionFailed, finishes[0].Status)
	require.Equal(t, admission.Error(), finishes[0].ErrMsg)
}

// TestScheduler_Dispatch_SessionBusyRecordsSkipped —— 会话忙是**跳过**（不是失败），
// 原因用固定文案。
func TestScheduler_Dispatch_SessionBusyRecordsSkipped(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, &SessionBusyError{SessionID: schedSession}
	}
	f.start(t)
	f.fire(t, schedTaskID)

	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionSkipped, finishes[0].Status)
	require.Equal(t, schedulerBusyMessage, finishes[0].ErrMsg)
	f.logs.requireRecord(t, slog.LevelWarn, schedulerBusyMessage)
}

// TestScheduler_Dispatch_ArchivedSessionSkipped —— 会话已归档同样记跳过并保留原因。
func TestScheduler_Dispatch_ArchivedSessionSkipped(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, &SessionArchivedError{SessionID: schedSession}
	}
	f.start(t)
	f.fire(t, schedTaskID)

	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionSkipped, finishes[0].Status)
	require.Equal(t, schedulerArchivedMessage, finishes[0].ErrMsg)
}

// TestScheduler_Dispatch_PanicRecovered —— 运行 panic 必须被吸收，执行行收尾 failed。
func TestScheduler_Dispatch_PanicRecovered(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		panic("boom")
	}
	f.start(t)
	f.fire(t, schedTaskID)

	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionFailed, finishes[0].Status)
	require.Contains(t, finishes[0].ErrMsg, "运行 panic")
	require.Contains(t, finishes[0].ErrMsg, "boom")
	f.logs.requireRecord(t, slog.LevelError, schedulerPanicLog)
}

// TestScheduler_Dispatch_FinishUsesCleanupCtx —— 终态写用 WithoutCancel + 独立短超时：
// 运行 ctx 已被取消时也要能落账（约束 11）。
func TestScheduler_Dispatch_FinishUsesCleanupCtx(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	ctx, cancel := context.WithCancel(context.Background())
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		cancel()
		return RunResult{}, context.Canceled
	}
	_, _, err := f.sched.RunNow(ctx, schedTaskID)
	require.Error(t, err)

	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.NoError(t, finishes[0].CtxErr, "终态写不得继承已取消的运行 ctx")
	require.True(t, finishes[0].HasDeadline, "终态写必须带独立超时")
	require.WithinDuration(t, time.Now().Add(schedulerFinishTimeout), finishes[0].Deadline, 2*time.Second)
}

// TestScheduler_Dispatch_ClaimFailureLogsAndSkips —— 领取返回未知错误：记 ERROR、
// 本轮不发起运行（不是静默吞掉）。
func TestScheduler_Dispatch_ClaimFailureLogsAndSkips(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.seed(t, schedTaskID)
	f.store.claimErr = errors.New("connection reset")
	f.scan(t)
	f.wait(t)

	require.Zero(t, f.invoker.calls.Load())
	f.logs.requireRecord(t, slog.LevelError, "claim due trigger")
}

// TestScheduler_Dispatch_BusyClaimRecordsSkipped —— 同任务已有在途执行：记一条
// task_busy 跳过行并推进（不排队、不叠加）。
func TestScheduler_Dispatch_BusyClaimRecordsSkipped(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.store.mu.Lock()
	f.store.active[schedTaskID] = 99
	f.store.mu.Unlock()

	planned := f.seed(t, schedTaskID)
	f.scan(t)
	f.wait(t)

	require.Zero(t, f.invoker.calls.Load(), "忙路径不得发起运行")
	busy := f.store.snapshotBusy()
	require.Len(t, busy, 1)
	require.True(t, planned.Equal(busy[0].PlannedAt))
	skipped := f.store.snapshotSkipped()
	require.Len(t, skipped, 1)
	require.Equal(t, ExecutionSkipped, skipped[0].Status)
	require.Equal(t, TriggerCron, skipped[0].Trigger, "忙冲突落的是定时触发行，不能记成 manual")
	require.Equal(t, schedulerLockMissMessage, skipped[0].ErrorMessage)
	f.logs.requireRecord(t, slog.LevelWarn, schedulerLockMissMessage)
}

// TestScheduler_Dispatch_ClaimConflictIsSilent —— 另一 Worker 已领取 / 已推进时本轮
// **无声跳过**：既不重试也不记 ERROR（跨 Worker 的正常竞争不是故障）。
func TestScheduler_Dispatch_ClaimConflictIsSilent(t *testing.T) {
	t.Run("already claimed", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		planned := f.seed(t, schedTaskID)
		f.store.mu.Lock()
		f.store.claimed[claimKey(schedTaskID, planned)] = true
		f.store.mu.Unlock()

		f.scan(t)
		f.wait(t)
		require.Zero(t, f.invoker.calls.Load())
		require.Zero(t, f.logs.countLevel(slog.LevelError), "重复领取是正常竞争，不得记 ERROR")
	})

	t.Run("stale next fire", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.start(t)
		planned := f.seed(t, schedTaskID)
		f.store.dispatchHook = func() {
			// 读到期集合与领取之间，另一 Worker 推进了 next_fire_at。
			f.store.setNextFireDirect(schedTaskID, planned.Add(time.Minute))
		}

		f.scan(t)
		f.wait(t)
		require.Zero(t, f.invoker.calls.Load())
		require.Zero(t, f.logs.countLevel(slog.LevelError), "CAS 落败是正常竞争，不得记 ERROR")
	})
}

// TestScheduler_Dispatch_MissedGraceRecordsOneSkip —— 错过宽限只记**一条** skipped
// 并推进到未来，不逐点补跑（FR-24-12）。
func TestScheduler_Dispatch_MissedGraceRecordsOneSkip(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	planned := schedNow.Add(-10 * time.Minute)
	f.seedAt(t, schedTaskID, planned)

	f.scan(t)
	f.wait(t)

	require.Zero(t, f.invoker.calls.Load(), "错失分支不得发起执行")
	require.Empty(t, f.store.snapshotClaims())
	missed := f.store.snapshotMissed()
	require.Len(t, missed, 1, "一个错失窗口只落一条跳过行")
	require.True(t, planned.Equal(missed[0].PlannedAt))
	require.Equal(t, 1, missed[0].Missed)
	require.Equal(t, schedulerMissedReason, missed[0].Reason)
	// 错失分支经 `RecordMissedTrigger` 落的是 skipped 行，**不**经 `FinishExecution`
	// 收尾（那一轮根本没有领取，也就没有 started 行要收）。
	skipped := f.store.snapshotSkipped()
	require.Len(t, skipped, 1)
	require.Equal(t, ExecutionSkipped, skipped[0].Status)
	require.Equal(t, schedulerMissedReason, skipped[0].ErrorMessage)

	next, ok := f.store.nextFireOf(schedTaskID)
	require.True(t, ok)
	require.True(t, schedNow.Add(schedStep).Equal(next), "错过分支同样必须推进到未来")
}

// TestScheduler_Dispatch_InitializesNullNextFireWithoutRun —— 迁移后的遗留行 / 新建行
// 第一次只初始化 next_fire_at，不执行（图 4 第一个分支）。
func TestScheduler_Dispatch_InitializesNullNextFireWithoutRun(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	_, has := f.store.nextFireOf(schedTaskID)
	require.False(t, has, "对账后新行的 next_fire_at 必须是 NULL")

	f.scan(t)
	f.wait(t)

	require.Empty(t, f.store.snapshotClaims(), "初始化分支不得领取")
	require.Zero(t, f.invoker.calls.Load())
	writes := f.store.snapshotNextWrites()
	require.Len(t, writes, 1)
	require.Nil(t, writes[0].Expected, "初始化是无条件写")
	next, ok := f.store.nextFireOf(schedTaskID)
	require.True(t, ok)
	require.True(t, schedNow.Add(schedStep).Equal(next))
}

// TestScheduler_Dispatch_UsesSixFieldCronWithZone —— 送进 CronEngine 的必须是
// 6 段表达式 + `CRON_TZ=` 时区前缀，起算点是 max(next_fire_at, db_now)。
func TestScheduler_Dispatch_UsesSixFieldCronWithZone(t *testing.T) {
	shanghai := demoTask(schedTaskID2)
	shanghai.Zone = "Asia/Shanghai"

	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID), shanghai}, nil)
	f.start(t)
	f.fire(t, schedTaskID)
	f.fire(t, schedTaskID2)

	specs, froms := f.engine.readings()
	// 同批扫描还会为 NULL 行算一次 next_fire（初始化分支），所以按"包含 + 末次"判：
	// 每任务时区必须经 `CRON_TZ=` 前缀出现在 spec 里，不得依赖进程时区。
	require.Contains(t, specs, "CRON_TZ=UTC "+schedCron)
	require.Equal(t, "CRON_TZ=Asia/Shanghai "+schedCron, specs[len(specs)-1])
	for _, from := range froms {
		require.True(t, schedNow.Equal(from), "起算点必须是 max(next_fire_at, db_now)")
	}
}

// TestScheduler_Dispatch_SkipsRetiredAndUnregistered —— 仓储已过滤，扫描仍要防御性
// 复查；未注册 Agent 的任务留给持有方推进 next_fire_at。
func TestScheduler_Dispatch_SkipsRetiredAndUnregistered(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	planned := f.seed(t, schedTaskID)
	f.store.mu.Lock()
	f.store.forced = []DueTask{
		{Task: TaskRecord{
			TaskID: "retired", Agent: schedAgent, Enabled: true, Cron: schedCron,
			Zone: schedZone, Message: "x", RetiredAt: schedNow,
		}, NextFireAt: planned},
		{Task: TaskRecord{
			TaskID: "disabled", Agent: schedAgent, Enabled: false, Cron: schedCron,
			Zone: schedZone, Message: "x",
		}, NextFireAt: planned},
		{Task: TaskRecord{
			TaskID: "ghost", Agent: "ghost", Enabled: true, Cron: schedCron,
			Zone: schedZone, Message: "x",
		}, NextFireAt: planned},
		{
			Task: TaskRecord{
				TaskID: schedTaskID, Agent: schedAgent, Enabled: true, Cron: schedCron,
				Zone: schedZone, Message: schedMessage,
				CallerID: LocalAdminCallerID, AuthorizingKind: AuthorizingKindAdminBuiltin,
			},
			NextFireAt: planned,
		},
	}
	f.store.mu.Unlock()

	f.scan(t)
	f.wait(t)

	claims := f.store.snapshotClaims()
	require.Len(t, claims, 1, "退休 / 停用 / 未注册三类都不得领取")
	require.Equal(t, schedTaskID, claims[0].TaskID)
}

// TestScheduler_Dispatch_ConcurrentDistinctTasks —— 两条不同任务可以同时在途，
// 各有自己的 run_id（实例内并发不是全局串行）。
func TestScheduler_Dispatch_ConcurrentDistinctTasks(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)}, nil)
	f.start(t)

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	f.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
		arrived <- struct{}{}
		<-release
		return RunResult{RunID: req.RunID, Status: StatusCompleted}, nil
	}

	f.claim(t, schedTaskID)
	f.claim(t, schedTaskID2)
	<-arrived
	<-arrived
	require.Equal(t, int32(2), f.invoker.calls.Load())
	close(release)
	f.wait(t)

	require.ElementsMatch(t, []ExecutionStatus{ExecutionCompleted, ExecutionCompleted},
		f.store.snapshotFinished())
	reqs := f.invoker.requests()
	require.Len(t, reqs, 2)
	require.NotEqual(t, reqs[0].RunID, reqs[1].RunID)
}

// TestScheduler_LogFieldsAndNoBody —— 运行日志只带固定四字段 + 正文长度，**不得**
// 把任务正文（可能是业务数据）写进日志。
func TestScheduler_LogFieldsAndNoBody(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.fire(t, schedTaskID)

	rec := f.logs.requireRecord(t, slog.LevelInfo, schedulerTriggerLog)
	for _, key := range []string{"task_id", "agent", "run_id", "trigger"} {
		_, ok := attrOf(rec, key)
		require.True(t, ok, "缺少固定日志字段 %s", key)
	}
	size, ok := attrOf(rec, "message_bytes")
	require.True(t, ok)
	require.Equal(t, int64(len(schedMessage)), size.Int64())
	require.NotContains(t, f.logs.text(), schedMessage, "日志不得携带任务正文")
}

// ===========================================================================
// 启动 / 停止
// ===========================================================================

// TestScheduler_Start_DoesNotRecoverGlobally 见 scheduler_delivery_test.go（T018 口径）。

// TestScheduler_Start_ReconcilesDefinitions —— 启动对账只写定义列：新行 enabled=true，
// 既有行的 enabled 保持库值；定义变化记 INFO 提示。
func TestScheduler_Start_ReconcilesDefinitions(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	applied := f.store.snapshotApplied()
	require.Len(t, applied, 1)
	require.Equal(t, schedAgent, applied[0].Agent)
	require.Equal(t, []string{schedTaskID}, applied[0].UpsertIDs)
	row, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	require.True(t, row.Enabled, "新行按 schema 缺省 enabled=true")

	require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, false))
	updated := demoTask(schedTaskID)
	updated.Message = "复核"
	require.NoError(t, f.sched.ReconcileAgent(context.Background(), schedAgent, []Schedule{updated}))
	row, _ = f.store.task(schedTaskID)
	require.False(t, row.Enabled, "定义更新不得翻转库里的 enabled")
	require.Equal(t, "复核", row.Message)
	f.logs.requireRecord(t, slog.LevelInfo, "任务定义已更新，enabled 保持库值")
}

// TestScheduler_StartTwice —— 成功启动后不得重入。
func TestScheduler_StartTwice(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	require.ErrorIs(t, f.sched.Start(context.Background()), errSchedulerStarted)
}

// TestScheduler_Start_MissedTriggerWarnsWithoutRun —— 启动时只对已知的错过触发点
// WARN，不补跑（真正的错失落库在到期扫描里）。
// 逆改点：对 missed 发起运行 → invoker 计数非零；对零 last_run_at 也调 Occurrences
// → 枚举计数非零；漏传固定上限 → limit 断言红。
func TestScheduler_Start_MissedTriggerWarnsWithoutRun(t *testing.T) {
	t.Run("missed triggers warn without catch-up", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: schedZone,
			Message: schedMessage, Enabled: true, LastRunAt: schedNow.Add(-time.Hour),
		})
		f.engine.occurred = []time.Time{schedNow.Add(-30 * time.Minute), schedNow.Add(-10 * time.Minute)}
		f.start(t)

		rec := f.logs.requireRecord(t, slog.LevelWarn, "错过调度触发点")
		missed, ok := attrOf(rec, "missed_count")
		require.True(t, ok)
		require.Equal(t, int64(2), missed.Int64())
		atLeast, ok := attrOf(rec, "missed_at_least")
		require.True(t, ok)
		require.False(t, atLeast.Bool(), "未达上限时必须明确为 false")
		calls, limits := f.engine.occurrenceReadings()
		require.Equal(t, 1, calls)
		require.Equal(t, []int{schedulerMissedOccurrences}, limits,
			"错过触发点枚举必须用固定上限（约束 38）")
		require.Zero(t, f.invoker.calls.Load(), "启动告警不得补跑")
		require.Empty(t, f.store.snapshotClaims())
	})

	t.Run("capped enumeration reports at least", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: schedZone,
			Message: schedMessage, Enabled: true, LastRunAt: schedNow.Add(-48 * time.Hour),
		})
		capped := make([]time.Time, schedulerMissedOccurrences)
		for i := range capped {
			capped[i] = schedNow.Add(-time.Duration(schedulerMissedOccurrences-i) * time.Minute)
		}
		f.engine.occurred = capped
		f.start(t)

		rec := f.logs.requireRecord(t, slog.LevelWarn, "错过调度触发点")
		missed, ok := attrOf(rec, "missed_count")
		require.True(t, ok)
		require.EqualValues(t, schedulerMissedOccurrences, missed.Int64())
		atLeast, ok := attrOf(rec, "missed_at_least")
		require.True(t, ok)
		require.True(t, atLeast.Bool(), "达到枚举上限时必须明示「>=1000」（约束 38）")
		require.Zero(t, f.invoker.calls.Load(), "启动告警不得补跑")
	})

	t.Run("task without last run does not warn", func(t *testing.T) {
		f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
		f.store.seedTask(TaskRecord{
			TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: schedZone,
			Message: schedMessage, Enabled: true,
		})
		f.engine.occurred = []time.Time{schedNow.Add(-time.Minute)}
		f.start(t)

		require.False(t, f.logs.hasRecord(slog.LevelWarn, "错过调度触发点"),
			"零 last_run_at 的任务是新任务，不得告警")
		calls, _ := f.engine.occurrenceReadings()
		require.Zero(t, calls, "无 last_run_at 时不得枚举触发点")
	})
}

// TestScheduler_Start_StoreFailureFatal —— 读库失败即启动失败，且不置启动位。
func TestScheduler_Start_StoreFailureFatal(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.listErr = errors.New("database is locked")
	err := f.startErr(t)
	require.ErrorContains(t, err, "list tasks")
	require.False(t, f.sched.started, "启动失败不得置启动位")

	f.store.listErr = nil
	require.NoError(t, f.sched.Start(context.Background()), "失败不粘滞，可重试")
}

// TestScheduler_Start_ApplyFailureIsNotLatched —— 对账写失败同样不粘滞。
func TestScheduler_Start_ApplyFailureIsNotLatched(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		_ = opts
	})
	f.store.applyErr = errors.New("deadlock detected")
	f.store.applyErrOnCall = 1
	err := f.startErr(t)
	require.ErrorContains(t, err, "reconcile agent")
	require.False(t, f.sched.started)

	f.store.applyErr = nil
	f.store.armScanGate()
	require.NoError(t, f.sched.Start(context.Background()))
	f.store.awaitScanEntered(t)
}

// TestScheduler_Start_OwnerConflictFatal —— 任务 id 归属其他 Agent：对账失败即启动失败。
func TestScheduler_Start_OwnerConflictFatal(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.seedTask(TaskRecord{TaskID: schedTaskID, Agent: "intruder", Cron: schedCron, Zone: schedZone})
	err := f.startErr(t)
	require.ErrorContains(t, err, "reconcile agent")
	require.ErrorIs(t, err, ErrTaskOwnerConflict)
}

// TestScheduler_Start_SecondAgentApplyFails_NoScanStart —— 第二个 Agent 对账失败时
// 整体启动失败，到期扫描不得启动。
func TestScheduler_Start_SecondAgentApplyFails_NoScanStart(t *testing.T) {
	store := newFakeTaskStore()
	store.applyErr = errors.New("deadlock detected")
	store.applyErrOnCall = 2
	logger, _ := newCaptureLogger()
	sched := NewScheduler(SchedulerOptions{
		Store:   store,
		Invoker: &countingInvoker{},
		Engine:  &fakeEngine{},
		Entries: []AgentEntry{
			{Name: "alpha", Registered: true, Profile: &Profile{}},
			{Name: "beta", Registered: true, Profile: &Profile{}},
		},
		CronSpec:      schedCronSpec,
		Logger:        logger,
		Now:           func() time.Time { return schedNow },
		Leases:        store,
		Worker:        schedWorker,
		LeaseTTL:      schedLeaseTTL,
		RenewInterval: schedRenewInterval,
		PollInterval:  time.Hour,
	})
	store.armScanGate()
	t.Cleanup(func() { sched.Stop(); store.resumeScan() })

	err := sched.Start(context.Background())
	require.ErrorContains(t, err, "reconcile agent")
	require.Equal(t, 2, func() int {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.applyCalls
	}())
	select {
	case <-store.scanEntered:
		t.Fatal("对账失败后不得启动到期扫描")
	default:
	}
}

// TestScheduler_StopConcurrent —— Stop 幂等且并发安全。
func TestScheduler_StopConcurrent(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(f.sched.Stop)
	}
	wg.Wait()

	select {
	case <-f.sched.done:
	default:
		t.Fatal("Stop 返回后 done 必须已关闭")
	}
}

// TestScheduler_StopOnContextCancel —— 启动 ctx 取消触发停机（watcher 恰一次）。
func TestScheduler_StopOnContextCancel(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.armScanGate()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		f.store.resumeScan()
	})
	require.NoError(t, f.sched.Start(ctx))
	f.store.awaitScanEntered(t)

	cancel()
	select {
	case <-f.sched.done:
	case <-time.After(5 * time.Second):
		t.Fatal("启动 ctx 取消后调度器未停机")
	}
}

// TestScheduler_StopBeforeStart —— 未启动就 Stop：立即返回，不阻塞。
func TestScheduler_StopBeforeStart(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.sched.Stop()
	select {
	case <-f.sched.done:
	default:
		t.Fatal("未启动的 Stop 必须立即完成")
	}
}

// TestScheduler_CancelExecutionsEndsInflightBeforeGrace —— 外部取消入口判图 11 [4]：
// 编排层在排空窗口到点时调用 CancelExecutions，在途 Run 必须**立即**收尾，而不是
// 等调度器自己的兜底宽限。用例把 StopGrace 设成 1h：若取消入口不存在或未生效，
// Stop 会一直等下去，本用例红。
// It asserts the drain-time external cancel ends in-flight work without waiting for the
// scheduler's own fallback grace.
func TestScheduler_CancelExecutionsEndsInflightBeforeGrace(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.StopGrace = time.Hour // 兜底宽限不可达：唯一有效的取消来源是外部入口
	})
	f.start(t)

	blocked := make(chan struct{})
	aborted := make(chan struct{})
	f.invoker.run = func(ctx context.Context, req RunRequest) (RunResult, error) {
		close(blocked)
		<-ctx.Done()
		close(aborted)
		return RunResult{RunID: req.RunID, Status: StatusCancelled}, ctx.Err()
	}
	f.claim(t, schedTaskID)
	<-blocked

	// 停止领取不得打断在途：这是 [2] 与 [3]/[4] 的分界。
	f.sched.StopClaims()
	select {
	case <-aborted:
		t.Fatal("停止领取不得取消在途运行：排空期间它必须继续跑")
	case <-time.After(50 * time.Millisecond):
	}

	// 排空到点：外部取消让在途 Run 退出，Stop 随即在宽限（1h）之前 join 完成。
	f.sched.CancelExecutions()
	done := make(chan struct{})
	go func() {
		f.sched.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("外部取消之后 Stop 必须立即完成，不得等满 StopGrace")
	}
	require.Equal(t, []ExecutionStatus{ExecutionFailed}, f.store.snapshotFinished(),
		"被停机取消的在途运行必须收尾为 failed")
}

// TestScheduler_StopTimeoutCancelsInflight —— 宽限耗尽后取消在途运行；终态仍要落账
// （用 WithoutCancel 派生的清理 ctx），停机不得留下永远 started 的执行行。
func TestScheduler_StopTimeoutCancelsInflight(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.StopGrace = 20 * time.Millisecond
	})
	f.start(t)

	blocked := make(chan struct{})
	f.invoker.run = func(ctx context.Context, req RunRequest) (RunResult, error) {
		close(blocked)
		<-ctx.Done()
		return RunResult{RunID: req.RunID, Status: StatusCancelled}, ctx.Err()
	}
	f.claim(t, schedTaskID)
	<-blocked

	f.sched.Stop()
	require.Equal(t, []ExecutionStatus{ExecutionFailed}, f.store.snapshotFinished(),
		"被停机取消的在途运行必须收尾为 failed")
}

// ===========================================================================
// 对账与注册集合
// ===========================================================================

// TestScheduler_ReconcileAgent —— 运行期对账把新增定义落库（US-24 起没有句柄要换）。
func TestScheduler_ReconcileAgent(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	require.NoError(t, f.sched.ReconcileAgent(context.Background(), schedAgent,
		[]Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)}))
	require.Len(t, f.store.snapshotApplied(), 2)
	_, ok := f.store.task(schedTaskID2)
	require.True(t, ok, "新增定义必须落库")
}

// TestScheduler_SetRegistered_RetiresRemovedAgent —— 移出注册集合：先撤注册，再退休
// 该 Agent 的全部未退休任务（只置 retired_at、不删行）。
func TestScheduler_SetRegistered_RetiresRemovedAgent(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	require.NoError(t, f.sched.SetRegistered(schedAgent, false))
	require.False(t, f.sched.registry[schedAgent])
	row, ok := f.store.task(schedTaskID)
	require.True(t, ok, "退休不得物理删除行")
	require.False(t, row.RetiredAt.IsZero())
}

// TestScheduler_SetRegistered_NotStartedOnlyTouchesRegistry —— 未启动时只动进程内注册表，
// 不写库（文件已落盘，下次启动对账生效）。
func TestScheduler_SetRegistered_NotStartedOnlyTouchesRegistry(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.seedTask(TaskRecord{TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: schedZone})
	require.NoError(t, f.sched.SetRegistered(schedAgent, false))
	require.False(t, f.sched.registry[schedAgent])
	require.Empty(t, f.store.snapshotApplied())
}

// TestScheduler_SetRegistered_ErrorKeepsRemoval —— 退休写失败如实上抛，注册表撤销保留
// （装配层据此记 WARN，不静默退回）。
func TestScheduler_SetRegistered_ErrorKeepsRemoval(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.store.applyErr = errors.New("connection reset")

	err := f.sched.SetRegistered(schedAgent, false)
	require.ErrorContains(t, err, "retire agent")
	require.False(t, f.sched.registry[schedAgent], "库写失败不得恢复注册（否则新 Run 又能进来）")
}

// TestScheduler_ReconcileAgent_StoreFailureKeepsRegistration —— 对账读库失败不得动摇注册表。
func TestScheduler_ReconcileAgent_StoreFailureKeepsRegistration(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.store.listErr = errors.New("database is locked")

	err := f.sched.ReconcileAgent(context.Background(), schedAgent, []Schedule{demoTask(schedTaskID)})
	require.ErrorContains(t, err, "list tasks")
	require.True(t, f.sched.registry[schedAgent])
}

// TestScheduler_Reconcile_SecondWriteFails_KeepsOldDefinition —— 对账失败保留旧定义
// （单事务语义：不出现半套定义）。
func TestScheduler_Reconcile_SecondWriteFails_KeepsOldDefinition(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.store.applyErr = errors.New("deadlock detected")
	f.store.applyErrOnCall = 2

	updated := demoTask(schedTaskID)
	updated.Message = "复核"
	err := f.sched.ReconcileAgent(context.Background(), schedAgent, []Schedule{updated})
	require.ErrorContains(t, err, "reconcile agent")

	row, _ := f.store.task(schedTaskID)
	require.Equal(t, schedMessage, row.Message, "对账失败必须保留旧定义")
}

// TestScheduler_AfterReconcile_NextDispatchUsesLatestDefinition —— 对账提交后到期扫描
// 立即使用最新定义（每轮读库，没有缓存要失效）。
func TestScheduler_AfterReconcile_NextDispatchUsesLatestDefinition(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.fire(t, schedTaskID)

	updated := demoTask(schedTaskID)
	updated.Message = "复核"
	require.NoError(t, f.sched.ReconcileAgent(context.Background(), schedAgent,
		[]Schedule{updated}))
	reqs := f.invoker.requests()
	require.Len(t, reqs, 1)
}

// TestScheduler_ReconcileAgent_ApplyIsAllOrNothing —— 一批 upsert 里任一归属冲突，
// 整批不生效（不出现半套定义）。
func TestScheduler_ReconcileAgent_ApplyIsAllOrNothing(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.store.seedTask(TaskRecord{TaskID: schedTaskID2, Agent: "intruder", Cron: schedCron, Zone: schedZone})

	err := f.sched.ReconcileAgent(context.Background(), schedAgent,
		[]Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)})
	require.ErrorIs(t, err, ErrTaskOwnerConflict)

	row, _ := f.store.task(schedTaskID)
	require.Equal(t, schedMessage, row.Message, "冲突批次不得产生半套定义")
	other, _ := f.store.task(schedTaskID2)
	require.Equal(t, "intruder", other.Agent)
}

// ===========================================================================
// 手动补跑
// ===========================================================================

// TestScheduler_RunNow_SameChain —— 手动补跑复用同一条执行链，只是 trigger=manual、
// 没有 planned_at。
func TestScheduler_RunNow_SameChain(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	req, res, err := f.sched.RunNow(context.Background(), schedTaskID)
	require.NoError(t, err)
	require.Equal(t, TriggerManual, req.Trigger)
	require.Equal(t, schedSession, req.SessionID)
	require.Equal(t, req.RunID, res.RunID)
	require.Empty(t, f.store.snapshotClaims(), "手动补跑不走 cron 领取")
	require.Equal(t, []ExecutionStatus{ExecutionCompleted}, f.store.snapshotFinished())
	started := f.store.snapshotStarted()
	require.Empty(t, started, "手动补跑的执行行不是 cron started 行")
}

// TestScheduler_RunNow_BusyReturnsErrTaskBusy —— 同任务已有在途执行：立即拒绝（409），
// 不排队、不叠加。
func TestScheduler_RunNow_BusyReturnsErrTaskBusy(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.store.mu.Lock()
	f.store.active[schedTaskID] = 99
	f.store.mu.Unlock()

	_, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	require.ErrorIs(t, err, ErrTaskBusy)
	require.Zero(t, f.invoker.calls.Load())
}

// TestScheduler_RunNow_HiddenTaskNotFound —— 未知 / 退休 / 未注册 Agent 的任务一律 404。
func TestScheduler_RunNow_HiddenTaskNotFound(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	_, _, err := f.sched.RunNow(context.Background(), "ghost")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestScheduler_RunNow_DisabledTaskStillRuns —— 手动补跑是显式请求，不受 enabled 影响。
func TestScheduler_RunNow_DisabledTaskStillRuns(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, false))

	_, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.invoker.calls.Load(), "显式补跑必须无视 enabled")
}

// TestScheduler_RunNow_SessionBusyRecordsSkipped —— 会话忙：补跑返回错误，执行行记跳过。
func TestScheduler_RunNow_SessionBusyRecordsSkipped(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	busyErr := &SessionBusyError{SessionID: schedSession}
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, busyErr
	}

	_, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	require.ErrorIs(t, err, busyErr)
	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionSkipped, finishes[0].Status)
	require.Equal(t, schedulerBusyMessage, finishes[0].ErrMsg)
}

// TestScheduler_RunNow_ExecuteRowFailure —— 执行行写失败：本轮不发起运行，错误如实上抛。
func TestScheduler_RunNow_ExecuteRowFailure(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.store.startExecutionErr = errors.New("connection reset")

	_, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	require.ErrorContains(t, err, "connection reset")
	require.Zero(t, f.invoker.calls.Load(), "执行行写失败不得发起运行")
	f.logs.requireRecord(t, slog.LevelError, "执行行写入失败，本轮不发起运行")
}

// TestScheduler_RunNow_ArchivedSessionPropagates —— 会话归档：错误原样上抛，执行行记跳过。
func TestScheduler_RunNow_ArchivedSessionPropagates(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	archived := &SessionArchivedError{SessionID: schedSession}
	f.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, archived
	}

	_, _, err := f.sched.RunNow(context.Background(), schedTaskID)
	require.ErrorIs(t, err, archived)
	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionSkipped, finishes[0].Status)
	require.Equal(t, schedulerArchivedMessage, finishes[0].ErrMsg)
}

// ===========================================================================
// 管理读面与启停
// ===========================================================================

// TestScheduler_SetEnabled_ControlsDispatch —— 启停只看库值：停用后不领取，启用后恢复。
func TestScheduler_SetEnabled_ControlsDispatch(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, false))
	f.seed(t, schedTaskID)
	f.scan(t)
	f.wait(t)
	require.Empty(t, f.store.snapshotClaims(), "停用后不得领取")

	require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, true))
	f.fire(t, schedTaskID)
	require.Len(t, f.store.snapshotClaims(), 1)
}

// TestScheduler_SetEnabled_HiddenTaskNotFound —— 不可见任务（未知 / 退休 / 未注册）→ 404。
func TestScheduler_SetEnabled_HiddenTaskNotFound(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	require.ErrorIs(t, f.sched.SetEnabled(context.Background(), "ghost", false), fs.ErrNotExist)
}

// TestScheduler_List_FiltersAndOrders —— 退休行与未注册 Agent 的任务一律隐藏，输出按
// task_id 升序。
func TestScheduler_List_FiltersAndOrders(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	require.NoError(t, f.sched.ReconcileAgent(context.Background(), schedAgent,
		[]Schedule{demoTask(schedTaskID), demoTask(schedTaskID2)}))
	f.store.seedTask(TaskRecord{TaskID: "hidden-1", Agent: "ghost", Cron: schedCron, Zone: schedZone})
	f.store.seedTask(TaskRecord{
		TaskID: "retired-1", Agent: schedAgent, Cron: schedCron,
		Zone: schedZone, RetiredAt: schedNow,
	})

	rows, err := f.sched.List(context.Background())
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.TaskID)
	}
	require.Equal(t, []string{schedTaskID, schedTaskID2}, ids)
}

// TestScheduler_List_ReflectsRunState —— 读面每次都读库：运行痕迹立刻可见。
func TestScheduler_List_ReflectsRunState(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	require.NoError(t, f.sched.SetEnabled(context.Background(), schedTaskID, true))
	f.fire(t, schedTaskID)

	rows, err := f.sched.List(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, schedNow.Equal(rows[0].LastRunAt), "运行后 last_run_at 必须可见")
	require.Equal(t, ExecutionCompleted, rows[0].LastStatus)
}

// TestScheduler_Executions_UsesLimit —— limit 原样透传给仓储。
func TestScheduler_Executions_UsesLimit(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	for i := 0; i < 3; i++ {
		f.fire(t, schedTaskID)
	}

	rows, err := f.sched.Executions(context.Background(), schedTaskID, 2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

// TestScheduler_Executions_HiddenTaskNotFound —— 不可见任务不暴露执行历史。
func TestScheduler_Executions_HiddenTaskNotFound(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	_, err := f.sched.Executions(context.Background(), "ghost", 10)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestScheduler_SetEnabled_WritesDBFirst —— 写库失败时库里保持原值，错误如实上抛。
func TestScheduler_SetEnabled_WritesDBFirst(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)
	f.store.setEnabledErr = errors.New("connection reset")

	err := f.sched.SetEnabled(context.Background(), schedTaskID, false)
	require.ErrorContains(t, err, "enabled=false")
	row, _ := f.store.task(schedTaskID)
	require.True(t, row.Enabled, "写库失败不得改动库值")
}

// TestScheduler_SetEnabled_ConcurrentToggles —— 并发启停不崩、最终值以库为准，
// 且下一轮扫描严格跟随时最终库值（数据库是唯一事实源）。
func TestScheduler_SetEnabled_ConcurrentToggles(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.start(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		target := i%2 == 0
		wg.Go(func() {
			_ = f.sched.SetEnabled(context.Background(), schedTaskID, target)
		})
	}
	wg.Wait()

	row, ok := f.store.task(schedTaskID)
	require.True(t, ok)
	f.seed(t, schedTaskID)
	f.scan(t)
	f.wait(t)
	if row.Enabled {
		require.Len(t, f.store.snapshotClaims(), 1, "库值为启用 → 下一轮必须领取")
	} else {
		require.Empty(t, f.store.snapshotClaims(), "库值为停用 → 下一轮不得领取")
	}
}

// TestScheduler_CheckOwnership —— 归属预检只拦"属于其他 Agent"的 id；未知 id 放行。
func TestScheduler_CheckOwnership(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	f.store.seedTask(TaskRecord{TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: schedZone})
	f.store.seedTask(TaskRecord{TaskID: "other-1", Agent: "other", Cron: schedCron, Zone: schedZone})

	require.NoError(t, f.sched.CheckOwnership(context.Background(), schedAgent, nil))
	require.NoError(t, f.sched.CheckOwnership(context.Background(), schedAgent, []string{schedTaskID}))
	require.NoError(t, f.sched.CheckOwnership(context.Background(), schedAgent, []string{"ghost"}))
	require.ErrorIs(t,
		f.sched.CheckOwnership(context.Background(), schedAgent, []string{"other-1"}),
		ErrTaskOwnerConflict)
}

// TestScheduler_LeaselessAssemblyRefusesToRun —— 没有执行权装配就既不启动也不补跑
// （fail-closed，不降级成无租约执行）。
func TestScheduler_LeaselessAssemblyRefusesToRun(t *testing.T) {
	logger, logs := newCaptureLogger()
	store := newFakeTaskStore()
	store.seedTask(TaskRecord{TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: schedZone})
	sched := NewScheduler(SchedulerOptions{
		Store:    store,
		Invoker:  &countingInvoker{},
		Engine:   &fakeEngine{},
		Entries:  []AgentEntry{{Name: schedAgent, Registered: true, Profile: &Profile{}}},
		CronSpec: schedCronSpec,
		Logger:   logger,
		Now:      func() time.Time { return schedNow },
	})

	require.ErrorIs(t, sched.Start(context.Background()), errSchedulerNoLeases)
	require.False(t, sched.started)
	_, _, err := sched.RunNow(context.Background(), schedTaskID)
	require.ErrorIs(t, err, errSchedulerNoLeases)
	logs.requireRecord(t, slog.LevelError, "调度器缺少执行权装配，拒绝启动")
}
