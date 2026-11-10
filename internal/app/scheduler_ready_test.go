package app

// 本文件是 US-25C 的**到期就绪门与扫描公平**判据（T041 / T043）与**钉住 Run 准入**
// 判据（T043）的落地。全部用例跑在 `internal/app`（package app）内、**不带构建标签**：
// 两个面都经装配端口驱动，既不碰真实数据库，也不发起任何模型调用。
//
// 调度面用一台**逐拍握手**的夹具：每拍到取页之前停住，由用例改状态后再放行。因为
// 派发循环是顺序的、领取与游标提交都在同一拍内同步完成，所以"收到第 n+1 拍请求"
// 就等价于"第 n 拍已全部跑完"——断言因此不靠 sleep，也不靠日志。取页之前的
// `ReadSweepBounds` / `DBNow` 已经读过，所以时钟改动只影响**下一拍**，这正好是
// "圈边界冻结"要考察的那条性质。
//
// This file pins the readiness gate and scan fairness of the due scan, plus the three
// admission judges of a pinned run. Everything runs without build tags: the scheduler and
// the run surface are driven through their assembly ports, with no database and no model
// call. The harness handshakes every dispatch pass, so "the next pass asked to run"
// proves the previous one finished — assertions never sleep.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

// ──────────────────────────────── 单拍握手 ────────────────────────────────

// readyPassGate 在每一拍的取页处停住，由用例决定何时放行。
// readyPassGate blocks each dispatch pass just before the page read.
type readyPassGate struct {
	requested chan int
	permit    chan struct{}

	mu     sync.Mutex
	passes int
}

func newReadyPassGate() *readyPassGate {
	return &readyPassGate{requested: make(chan int), permit: make(chan struct{}, 1)}
}

// enter 报告本拍序号并等待放行；ctx 结束（停机）时返回 ctx 的错误。
func (g *readyPassGate) enter(ctx context.Context) error {
	g.mu.Lock()
	g.passes++
	pass := g.passes
	g.mu.Unlock()
	select {
	case g.requested <- pass:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.permit:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release 放行当前这一拍。
func (g *readyPassGate) release() { g.permit <- struct{}{} }

// waitPass 等第 want 拍来到闸口；序号不符即为夹具违约。
func (g *readyPassGate) waitPass(t *testing.T, want int) {
	t.Helper()
	select {
	case got := <-g.requested:
		require.Equal(t, want, got, "派发拍序必须逐拍前进（第 %d 拍没到就等到了第 %d 拍）", want, got)
	case <-time.After(10 * time.Second):
		t.Fatalf("等待第 %d 拍超时：派发循环没有前进", want)
	}
}

// ──────────────────────────────── 任务行 ────────────────────────────────

// readyRow 是 `scheduled_tasks` 在夹具里的一行：到期时刻、以及两条可注入的拒绝。
type readyRow struct {
	task       core.TaskRecord
	nextFireAt time.Time
	// claimErr 让钉住领取在该行上零写入地拒绝（ErrAssetMoved / ErrAssetNotReady）。
	claimErr error
	// setNextErr 让 NULL 段的初始化写失败，用来造"整页 NULL 都初始化不了"。
	setNextErr error
}

// readyTaskRecord 造一条 admin-builtin 任务行：调度执行只能按归属重建 Principal，
// 因此 CallerID 必须是固定字面量 `local-admin`（core.OwnedPrincipal 的硬条件）。
func readyTaskRecord(id, agent string) core.TaskRecord {
	return core.TaskRecord{
		TaskID:          id,
		Agent:           agent,
		Cron:            "0 */5 * * * *",
		Zone:            "UTC",
		Message:         "message-" + id,
		CallerID:        core.LocalAdminCallerID,
		AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		Enabled:         true,
	}
}

// ──────────────────────────────── 调度仓储替身 ────────────────────────────────

// readyClaim 是一次领取尝试的读数。
type readyClaim struct {
	taskID     string
	plannedAt  time.Time
	nextFireAt time.Time
	expectHash string
	owner      core.WorkerIdentity
	ttl        time.Duration
}

// readyAdvance 是一次 `next_fire_at` 写的读数；expected 为 nil 即初始化分支。
type readyAdvance struct {
	taskID   string
	expected *time.Time
	next     time.Time
}

// readyMissed 是一次"错过宽限"落库的读数。
type readyMissed struct {
	taskID     string
	plannedAt  time.Time
	nextFireAt time.Time
	missed     int
	reason     string
}

// readyCounts 是各类写面的调用计数快照。
type readyCounts struct {
	sweepCalls     int
	pageCalls      int
	pinnedAttempts int
	pinnedClaims   int
	unpinnedClaims int
	setNextCalls   int
	missedCalls    int
	busyCalls      int
	finishCalls    int
	markRunCalls   int
}

// readyStore 是到期扫描与领取的 TaskStore 替身：**只**实现本文件观测到的方法，其余
// 经内嵌 nil 接口保持未实现——误调用即 panic，避免"顺手实现"把未判定的写路径放行。
//
// `ListDispatchableForAgents` 的分页语义**逐条照抄** `internal/storage/postgres` 的
// `schedule_scan.go`：NULL 段按 `task_id` 前进、非 NULL 段按 `(next_fire_at, task_id)`
// 前进、两段共享一页容量、短页即段尾。判据只有在与生产同形的分页上才有意义。
// readyStore mirrors the postgres pager exactly; unobserved methods panic via the
// embedded nil interfaces.
type readyStore struct {
	core.TaskStore
	core.LeaseStore

	gate *readyPassGate

	mu    sync.Mutex
	rows  map[string]*readyRow
	order []string
	now   time.Time

	counts readyCounts

	sweepReads []core.SweepBounds
	pages      [][]string
	pinnedLog  []readyClaim
	claimed    []readyClaim
	advances   []readyAdvance
	missedLog  []readyMissed
	finished   []core.ExecutionStatus
}

func newReadyStore(gate *readyPassGate, now time.Time) *readyStore {
	return &readyStore{gate: gate, rows: map[string]*readyRow{}, now: now}
}

// addRow 预置一行（构造期调用，之后的行变更一律走 setRow）。
func (s *readyStore) addRow(row readyRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[row.task.TaskID] = &row
	s.order = append(s.order, row.task.TaskID)
}

// setRow 在用例拍间改一行（丢弃指针后重新写入，避免与并发读者共享地址）。
func (s *readyStore) setRow(row readyRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := row
	s.rows[row.task.TaskID] = &clone
}

// setClock 推进夹具的数据库时钟：`DBNow` 与开圈边界都读它。
func (s *readyStore) setClock(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// rowNextFire 读回一行的 `next_fire_at`（零值 = NULL 段）。
func (s *readyStore) rowNextFire(taskID string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[taskID]
	if !ok {
		return time.Time{}
	}
	return row.nextFireAt
}

func (s *readyStore) calls() readyCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts
}

func (s *readyStore) pageAt(i int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.pages) {
		return nil
	}
	return append([]string(nil), s.pages[i]...)
}

func (s *readyStore) sweepCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts.sweepCalls
}

func (s *readyStore) claimLog() []readyClaim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]readyClaim(nil), s.pinnedLog...)
}

func (s *readyStore) claimedLog() []readyClaim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]readyClaim(nil), s.claimed...)
}

func (s *readyStore) advanceLog() []readyAdvance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]readyAdvance(nil), s.advances...)
}

func (s *readyStore) missedLogs() []readyMissed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]readyMissed(nil), s.missedLog...)
}

func (s *readyStore) finishedLog() []core.ExecutionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.ExecutionStatus(nil), s.finished...)
}

// ListTasks 返回启动对账用的行快照（Worker 角色下只读）。
func (s *readyStore) ListTasks(context.Context) ([]core.TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.TaskRecord, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.rows[id].task)
	}
	return out, nil
}

// DBNow 返回夹具时钟（除开圈那一拍外，每拍读一次）。
func (s *readyStore) DBNow(context.Context) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now, nil
}

// ReadSweepBounds 一次只读给出冻结边界：数据库时间 + NULL 段最大 task_id。
// 空 Agent 集合在 postgres 是错误，这里同样不返回"看起来合法"的空边界。
func (s *readyStore) ReadSweepBounds(_ context.Context, agents []string) (core.SweepBounds, error) {
	if len(agents) == 0 {
		return core.SweepBounds{}, errors.New("readyStore: 到期扫描没有 Agent 集合")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed := readyAgentSet(agents)
	upper := ""
	for _, id := range s.order {
		row := s.rows[id]
		if !readySelectable(row, allowed) || !row.nextFireAt.IsZero() {
			continue
		}
		if row.task.TaskID > upper {
			upper = row.task.TaskID
		}
	}
	bounds := core.SweepBounds{Cutoff: s.now, NullUpperTaskID: upper}
	s.counts.sweepCalls++
	s.sweepReads = append(s.sweepReads, bounds)
	return bounds, nil
}

// ListDispatchableForAgents 取一拍的一页：闸口在这里，取页在放行之后。
func (s *readyStore) ListDispatchableForAgents(ctx context.Context, agents []string,
	after *core.DueScanCursor, limit int,
) ([]core.DueTask, error) {
	if err := s.gate.enter(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	allowed := readyAgentSet(agents)
	var page []core.DueTask
	if after == nil || after.Sweep == nil {
		// 等价于旧全局批次的首屏：NULL 与已到期一起按 (next_fire, task_id) 排序。
		page = s.firstPageLocked(allowed, limit)
	} else {
		if after.InNullSegment && after.Sweep.NullUpperTaskID != "" {
			page = s.nullPageLocked(allowed, after.NullAfterTaskID, after.Sweep.NullUpperTaskID, limit)
		}
		if len(page) < limit {
			page = append(page, s.sweepPageLocked(allowed, after, after.Sweep.Cutoff, limit-len(page))...)
		}
	}
	s.counts.pageCalls++
	s.pages = append(s.pages, readyTaskIDs(page))
	return page, nil
}

// SetNextFire 只初始化 / 推进；expected 非 nil 时做 CAS。
func (s *readyStore) SetNextFire(_ context.Context, taskID string, expected *time.Time, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts.setNextCalls++
	s.advances = append(s.advances, readyAdvance{taskID: taskID, expected: expected, next: next})
	row, ok := s.rows[taskID]
	if !ok {
		return fmt.Errorf("readyStore: 未知任务 %s", taskID)
	}
	if expected != nil && !row.nextFireAt.Equal(*expected) {
		return core.ErrStaleNextFire
	}
	if row.setNextErr != nil {
		return row.setNextErr
	}
	row.nextFireAt = next
	return nil
}

// ClaimDueTrigger 是未钉住的领取：本文件的用例一律要求走钉住路径，触达即是缺陷。
func (s *readyStore) ClaimDueTrigger(_ context.Context, taskID string, plannedAt, nextFireAt time.Time,
	owner core.WorkerIdentity, ttl time.Duration,
) (core.DueExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts.unpinnedClaims++
	s.pinnedLog = append(s.pinnedLog, readyClaim{
		taskID: taskID, plannedAt: plannedAt, nextFireAt: nextFireAt, owner: owner, ttl: ttl,
	})
	return core.DueExecution{}, errors.New("readyStore: 就绪门给出了哈希，不得走未钉住的领取")
}

// ClaimDueTriggerPinned 是领取事务：拒绝即零写入，成功才推进 `next_fire_at`。
func (s *readyStore) ClaimDueTriggerPinned(_ context.Context, taskID string,
	plannedAt, nextFireAt time.Time, owner core.WorkerIdentity, ttl time.Duration, expectHash string,
) (core.DueExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts.pinnedAttempts++
	attempt := readyClaim{
		taskID: taskID, plannedAt: plannedAt, nextFireAt: nextFireAt,
		expectHash: expectHash, owner: owner, ttl: ttl,
	}
	s.pinnedLog = append(s.pinnedLog, attempt)
	row, ok := s.rows[taskID]
	if !ok {
		return core.DueExecution{}, fmt.Errorf("readyStore: 未知任务 %s", taskID)
	}
	if row.claimErr != nil {
		return core.DueExecution{}, row.claimErr
	}
	if !row.nextFireAt.Equal(plannedAt) {
		return core.DueExecution{}, core.ErrStaleNextFire
	}
	row.nextFireAt = nextFireAt
	s.counts.pinnedClaims++
	s.claimed = append(s.claimed, attempt)
	return core.DueExecution{
		Task:        row.task,
		ExecutionID: int64(len(s.claimed)),
		PlannedAt:   plannedAt,
		Lease: core.LeaseToken{
			Kind:           core.LeaseScheduleExecution,
			ResourceID:     fmt.Sprintf("execution-%d", len(s.claimed)),
			OwnerWorkerID:  owner.InstanceID,
			OwnershipEpoch: 1,
		},
		RunID:            fmt.Sprintf("cron-%d-%06d", plannedAt.UnixMilli(), len(s.claimed)),
		PinnedBundleHash: expectHash,
	}, nil
}

// RecordMissedTrigger 落一条 skipped 并在同一事务里推进。
func (s *readyStore) RecordMissedTrigger(_ context.Context, taskID string,
	plannedAt, nextFireAt time.Time, missed int, reason string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts.missedCalls++
	s.missedLog = append(s.missedLog, readyMissed{
		taskID: taskID, plannedAt: plannedAt, nextFireAt: nextFireAt, missed: missed, reason: reason,
	})
	if row, ok := s.rows[taskID]; ok {
		row.nextFireAt = nextFireAt
	}
	return nil
}

// RecordBusyTrigger 记撞忙路径的推进写。
func (s *readyStore) RecordBusyTrigger(context.Context, string, time.Time, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts.busyCalls++
	return nil
}

// MarkTaskRun 记运行痕迹（执行链的第一笔写）。
func (s *readyStore) MarkTaskRun(context.Context, string, time.Time, core.ExecutionStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts.markRunCalls++
	return nil
}

// FinishExecution 带 token 收尾：本文件的执行链一律成功，收尾必须发生。
func (s *readyStore) FinishExecution(_ context.Context, _ int64, _ core.LeaseToken,
	status core.ExecutionStatus, _ time.Duration, _ string, _ time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts.finishCalls++
	s.finished = append(s.finished, status)
	return nil
}

// firstPageLocked 是旧全局批次首屏的等价语义（NULL 优先、再按到期时刻）。
func (s *readyStore) firstPageLocked(allowed map[string]bool, limit int) []core.DueTask {
	due := make([]*readyRow, 0, len(s.order))
	for _, id := range s.order {
		row := s.rows[id]
		if !readySelectable(row, allowed) {
			continue
		}
		if !row.nextFireAt.IsZero() && row.nextFireAt.After(s.now) {
			continue
		}
		due = append(due, row)
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].nextFireAt.IsZero() != due[j].nextFireAt.IsZero() {
			return due[i].nextFireAt.IsZero()
		}
		if !due[i].nextFireAt.Equal(due[j].nextFireAt) {
			return due[i].nextFireAt.Before(due[j].nextFireAt)
		}
		return due[i].task.TaskID < due[j].task.TaskID
	})
	return readyPage(due, limit)
}

// nullPageLocked 取 NULL 段的一页：按 `task_id` 前进，上界是本圈冻结的 NULL 上界。
func (s *readyStore) nullPageLocked(allowed map[string]bool, afterID, upper string, limit int) []core.DueTask {
	null := make([]*readyRow, 0, len(s.order))
	for _, id := range s.order {
		row := s.rows[id]
		if !readySelectable(row, allowed) || !row.nextFireAt.IsZero() {
			continue
		}
		if row.task.TaskID <= afterID || row.task.TaskID > upper {
			continue
		}
		null = append(null, row)
	}
	sort.SliceStable(null, func(i, j int) bool { return null[i].task.TaskID < null[j].task.TaskID })
	return readyPage(null, limit)
}

// sweepPageLocked 取非 NULL 段的一页：只考察 `next_fire_at <= cutoff`，元组下界只在
// 游标带 `AfterNextFireAt` 时才拼——**不得**把 NULL 当元组下界。
func (s *readyStore) sweepPageLocked(allowed map[string]bool, after *core.DueScanCursor,
	cutoff time.Time, limit int,
) []core.DueTask {
	due := make([]*readyRow, 0, len(s.order))
	for _, id := range s.order {
		row := s.rows[id]
		if !readySelectable(row, allowed) || row.nextFireAt.IsZero() {
			continue
		}
		if row.nextFireAt.After(cutoff) {
			continue
		}
		if after.AfterNextFireAt != nil {
			bound := *after.AfterNextFireAt
			if row.nextFireAt.Before(bound) {
				continue
			}
			if row.nextFireAt.Equal(bound) && row.task.TaskID <= after.AfterTaskID {
				continue
			}
		}
		due = append(due, row)
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].nextFireAt.Equal(due[j].nextFireAt) {
			return due[i].nextFireAt.Before(due[j].nextFireAt)
		}
		return due[i].task.TaskID < due[j].task.TaskID
	})
	return readyPage(due, limit)
}

func readyAgentSet(agents []string) map[string]bool {
	allowed := make(map[string]bool, len(agents))
	for _, agent := range agents {
		allowed[agent] = true
	}
	return allowed
}

func readySelectable(row *readyRow, allowed map[string]bool) bool {
	return allowed[row.task.Agent] && row.task.Enabled && row.task.RetiredAt.IsZero()
}

func readyPage(rows []*readyRow, limit int) []core.DueTask {
	if limit <= 0 {
		return nil
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]core.DueTask, 0, len(rows))
	for _, row := range rows {
		out = append(out, core.DueTask{Task: row.task, NextFireAt: row.nextFireAt})
	}
	return out
}

func readyTaskIDs(page []core.DueTask) []string {
	ids := make([]string, 0, len(page))
	for _, due := range page {
		ids = append(ids, due.Task.TaskID)
	}
	return ids
}

// ──────────────────────────────── 租约 / 就绪 / 引擎 / 运行面替身 ────────────────────────────────

// readyLeases 是执行租约端口：续约与释放都记录，回收面不在本文件判据内。
type readyLeases struct {
	core.LeaseStore

	mu       sync.Mutex
	renewed  []core.LeaseToken
	released []core.LeaseToken
}

func (l *readyLeases) Renew(_ context.Context, token core.LeaseToken, ttl time.Duration) (core.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renewed = append(l.renewed, token)
	now := time.Now().UTC()
	return core.Lease{LeaseToken: token, RenewedAt: now, ExpiresAt: now.Add(ttl)}, nil
}

func (l *readyLeases) Release(_ context.Context, token core.LeaseToken) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = append(l.released, token)
	return nil
}

// readyBook 是就绪门替身：按 Agent 给出已安装哈希或未就绪原因，并记录查询次数。
type readyBook struct {
	mu    sync.Mutex
	ready map[string]string
	fail  map[string]error
	calls map[string]int
}

func newReadyBook() *readyBook {
	return &readyBook{
		ready: map[string]string{},
		fail:  map[string]error{},
		calls: map[string]int{},
	}
}

func (b *readyBook) set(agent, hash string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ready[agent] = hash
	delete(b.fail, agent)
}

func (b *readyBook) unset(agent string, reason error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail[agent] = reason
	delete(b.ready, agent)
}

func (b *readyBook) hash(_ context.Context, agent string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls[agent]++
	if err, ok := b.fail[agent]; ok {
		return "", err
	}
	hash, ok := b.ready[agent]
	if !ok {
		// 未配置的 Agent 必须**显式**未就绪：默认就绪会让漏配的用例静默通过。
		return "", fmt.Errorf("readyBook: agent %s 没有配置就绪状态", agent)
	}
	return hash, nil
}

func (b *readyBook) callsFor(agent string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[agent]
}

// readyEngine 让下一次触发固定落在两小时后：一拍之内不会再到期，判据因此稳定。
type readyEngine struct{}

func (readyEngine) Next(_ string, from time.Time) (time.Time, error) {
	return from.Add(2 * time.Hour), nil
}

func (readyEngine) Occurrences(string, time.Time, time.Time, int) []time.Time { return nil }

// readyCronSpec 拼出与生产同形的 6 段表达式（带 CRON_TZ= 前缀）。
func readyCronSpec(schedule core.Schedule) (string, error) {
	zone := schedule.Zone
	if zone == "" {
		zone = "UTC"
	}
	return "CRON_TZ=" + zone + " " + schedule.Cron, nil
}

// readyInvoker 记录成功领取后的每一次 Run 请求。
type readyInvoker struct {
	mu       sync.Mutex
	requests []core.RunRequest
}

func (i *readyInvoker) Run(_ context.Context, req core.RunRequest) (core.RunResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.requests = append(i.requests, req)
	return core.RunResult{RunID: req.RunID, Status: core.StatusCompleted}, nil
}

func (i *readyInvoker) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.requests)
}

func readyLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// readyWorker 是领取归属身份（非零才允许 Start）。
var readyWorker = core.WorkerIdentity{
	InstanceID: "worker-b-1",
	Name:       "worker-b",
	Hostname:   "host-b",
	Version:    "test",
}

// ──────────────────────────────── 夹具 ────────────────────────────────

// readySpec 是一台调度夹具的输入：哪些 Agent 注册、单轮上限、宽限与预置行。
type readySpec struct {
	agents      []string
	rows        []readyRow
	batch       int
	missedGrace time.Duration
}

// readyHarness 把一台按拍握手的调度器连同它的替身装在一起。
type readyHarness struct {
	base    time.Time
	gate    *readyPassGate
	store   *readyStore
	leases  *readyLeases
	book    *readyBook
	invoker *readyInvoker
	sched   *core.Scheduler
}

func newReadyHarness(t *testing.T, spec readySpec) *readyHarness {
	t.Helper()
	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	gate := newReadyPassGate()
	store := newReadyStore(gate, base)
	for _, row := range spec.rows {
		store.addRow(row)
	}
	leases := &readyLeases{}
	book := newReadyBook()
	invoker := &readyInvoker{}
	for _, agent := range spec.agents {
		book.set(agent, "sha256:"+agent)
	}

	entries := make([]core.AgentEntry, 0, len(spec.agents))
	for _, agent := range spec.agents {
		entries = append(entries, core.AgentEntry{Name: agent, Registered: true})
	}
	batch := spec.batch
	if batch <= 0 {
		batch = 8
	}
	// 缺省宽限放到 24h：默认 0 会把每一条"已到期"都判成错过宽限，判据就测不到领取。
	grace := spec.missedGrace
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	sched := core.NewScheduler(core.SchedulerOptions{
		Store:    store,
		Invoker:  invoker,
		Engine:   readyEngine{},
		Entries:  entries,
		CronSpec: readyCronSpec,
		Logger:   readyLogger(),
		Now:      func() time.Time { return base },
		Leases:   leases,
		Worker:   readyWorker,

		LeaseTTL:      time.Minute,
		RenewInterval: 10 * time.Second,
		PollInterval:  time.Millisecond,
		DispatchBatch: batch,
		MissedGrace:   grace,
		StopGrace:     2 * time.Second,

		AssetReadyHash: book.hash,
		// Worker 角色：启动对账只读，不写共享定义列。
		DefinitionAuthority: false,
	})
	require.NoError(t, sched.Start(context.Background()))
	t.Cleanup(sched.Stop)

	return &readyHarness{
		base: base, gate: gate, store: store, leases: leases,
		book: book, invoker: invoker, sched: sched,
	}
}

// waitPass 等第 n 拍来到闸口。
func (h *readyHarness) waitPass(t *testing.T, pass int) {
	t.Helper()
	h.gate.waitPass(t, pass)
}

// step 等第 n 拍、执行变更、再放行。
func (h *readyHarness) step(t *testing.T, pass int, mutate func()) {
	t.Helper()
	h.gate.waitPass(t, pass)
	if mutate != nil {
		mutate()
	}
	h.gate.release()
}

// release 放行当前**已 hold** 的那一拍：`waitPass` 之后做断言或改变更，再调它。
// release lets the held pass run; pair it with a preceding waitPass.
func (h *readyHarness) release() { h.gate.release() }

// readyDueRow 造一行"已到期 d 时长"的任务。
func readyDueRow(id, agent string, base time.Time, d time.Duration) readyRow {
	return readyRow{task: readyTaskRecord(id, agent), nextFireAt: base.Add(-d)}
}

// ──────────────────────────────── 调度判据 ────────────────────────────────

// TestScheduler_NotReadyLeavesTrigger 守 FR-25C-11 的"未就绪不领取"：门返回原因时，
// 该触发必须**原样留在原地**——不领取（钉住与未钉住都不）、不推进 `next_fire_at`、
// 不写执行行、不记 missed、不记撞忙、不发起运行。连跑三拍验证"每一拍都重新求值"。
// An unready agent must leave the trigger untouched: no claim, no advance, no execution.
func TestScheduler_NotReadyLeavesTrigger(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha"},
		rows:   []readyRow{readyDueRow("t1", "alpha", base, time.Minute)},
		batch:  2,
	})
	harness.book.unset("alpha", errors.New("asset sync: 尚未安装"))

	for pass := 1; pass <= 3; pass++ {
		harness.step(t, pass, nil)
	}
	harness.waitPass(t, 4)

	counts := harness.store.calls()
	require.Zero(t, counts.pinnedAttempts, "未就绪不得发起钉住领取")
	require.Zero(t, counts.unpinnedClaims, "未就绪不得退回未钉住的领取")
	require.Zero(t, counts.setNextCalls, "未就绪不得推进 / 初始化 next_fire_at")
	require.Zero(t, counts.missedCalls, "未越过宽限的行不得记 missed")
	require.Zero(t, counts.busyCalls)
	require.Zero(t, counts.finishCalls, "没有领取就没有执行行可收尾")
	require.Zero(t, counts.markRunCalls)
	require.Zero(t, harness.invoker.count(), "未就绪不得发起运行")
	require.True(t, harness.store.rowNextFire("t1").Equal(base.Add(-time.Minute)),
		"next_fire_at 必须原样留在原地")
	require.GreaterOrEqual(t, harness.book.callsFor("alpha"), 3, "每一拍都必须重新求值就绪门")
}

// TestScheduler_PublishBetweenGateAndClaimLeavesTrigger 守"扫描后、领取前被发布抢先"：
// 领取事务以 `ErrAssetMoved` 零写入拒绝，触发留在原地；下一拍就绪快照换成新哈希后
// 同一个 plannedAt 才被领取。
// A publish that lands between the gate and the claim leaves the trigger in place.
func TestScheduler_PublishBetweenGateAndClaimLeavesTrigger(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	row := readyDueRow("t1", "alpha", base, time.Minute)
	row.claimErr = core.ErrAssetMoved
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha"},
		rows:   []readyRow{row},
		batch:  2,
	})

	harness.step(t, 1, func() {
		harness.store.setRow(row) // claimErr 保持：本拍领取必然被拒
	})
	harness.waitPass(t, 2)

	require.Empty(t, harness.store.claimedLog(), "被抢先的领取不得留下任何成功记录")
	require.True(t, harness.store.rowNextFire("t1").Equal(base.Add(-time.Minute)),
		"零写入拒绝不得推进 next_fire_at")
	require.Zero(t, harness.invoker.count())
	require.Zero(t, harness.store.calls().finishCalls)

	// 新版本已就绪：同一拍的下一次考察用新哈希领取成功。
	harness.store.setRow(readyDueRow("t1", "alpha", base, time.Minute))
	harness.book.set("alpha", "sha256:h2")
	harness.release()
	harness.waitPass(t, 3)

	claims := harness.store.claimedLog()
	require.Len(t, claims, 1, "发布落地后必须能领取")
	require.Equal(t, "sha256:h2", claims[0].expectHash, "领取必须钉住**新**快照的哈希")
	require.True(t, claims[0].plannedAt.Equal(base.Add(-time.Minute)), "planned_at 保持原值")
	require.Eventually(t, func() bool {
		return harness.invoker.count() == 1 && len(harness.store.finishedLog()) == 1
	}, 5*time.Second, 5*time.Millisecond, "领取成功后必须发起一次运行并带 token 收尾")
	require.Equal(t, []core.ExecutionStatus{core.ExecutionCompleted}, harness.store.finishedLog())
}

// TestScheduler_UnreadyPrefixDoesNotStarveHealthyTask 守 F6 饿死：单轮上限被未就绪行
// 占满时，健康任务必须在**下一拍**的下一页里被领取，而不是永远进不了批次。
// An unready prefix must not starve a healthy task: the next page still reaches it.
func TestScheduler_UnreadyPrefixDoesNotStarveHealthyTask(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha", "beta"},
		rows: []readyRow{
			readyDueRow("a1", "alpha", base, 4*time.Minute),
			readyDueRow("a2", "alpha", base, 3*time.Minute),
			readyDueRow("b1", "beta", base, 30*time.Second),
		},
		batch: 2,
	})
	harness.book.unset("alpha", errors.New("asset sync: alpha 尚未安装"))

	harness.step(t, 1, nil)
	harness.waitPass(t, 2)
	require.Equal(t, []string{"a1", "a2"}, harness.store.pageAt(0), "首屏被未就绪行占满")
	require.Empty(t, harness.store.claimedLog(), "未就绪行不得领取")

	harness.release()
	harness.waitPass(t, 3)
	require.Equal(t, []string{"b1"}, harness.store.pageAt(1), "健康任务必须出现在下一页")

	claims := harness.store.claimedLog()
	require.Len(t, claims, 1)
	require.Equal(t, "b1", claims[0].taskID)
	require.Equal(t, "sha256:beta", claims[0].expectHash)
	require.Eventually(t, func() bool { return harness.invoker.count() == 1 },
		5*time.Second, 5*time.Millisecond, "领取成功后必须在执行链里发起一次运行")
}

// TestScheduler_ErrAssetMovedPrefixDoesNotStarveHealthyTask 与上一条同形，但前缀行的
// 拒绝来自**领取事务**（`ErrAssetMoved`）而不是就绪门：两条零写入拒绝都不得占住单轮
// 名额，健康任务仍须在下一页被领取。
// A claim-time refusal prefix is just as harmless as a gate refusal.
func TestScheduler_ErrAssetMovedPrefixDoesNotStarveHealthyTask(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	movedA1 := readyDueRow("a1", "alpha", base, 4*time.Minute)
	movedA1.claimErr = core.ErrAssetMoved
	movedA2 := readyDueRow("a2", "alpha", base, 3*time.Minute)
	movedA2.claimErr = core.ErrAssetNotReady
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha", "beta"},
		rows: []readyRow{
			movedA1, movedA2,
			readyDueRow("b1", "beta", base, 30*time.Second),
		},
		batch: 2,
	})

	harness.step(t, 1, nil)
	harness.waitPass(t, 2)
	require.Equal(t, []string{"a1", "a2"}, harness.store.pageAt(0))
	require.Empty(t, harness.store.claimedLog())
	require.True(t, harness.store.rowNextFire("a1").Equal(base.Add(-4*time.Minute)))
	require.True(t, harness.store.rowNextFire("a2").Equal(base.Add(-3*time.Minute)))

	harness.release()
	harness.waitPass(t, 3)
	require.Equal(t, []string{"b1"}, harness.store.pageAt(1))

	claims := harness.store.claimedLog()
	require.Len(t, claims, 1)
	require.Equal(t, "b1", claims[0].taskID)
}

// TestScheduler_NullFullPageDoesNotStarveHealthyTask 守"整页 NULL 初始化失败"：NULL 段
// 占满一页且每一行的初始化写都失败时，游标必须按 `task_id` 前进，让同一圈的下一页
// 取到非 NULL 段的健康任务——不得复位、不得把 NULL 当元组下界。
// A full NULL page whose initialization fails still advances by task_id.
func TestScheduler_NullFullPageDoesNotStarveHealthyTask(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	initErr := errors.New("readyStore: 初始化写失败")
	a1 := readyRow{task: readyTaskRecord("a1", "alpha"), setNextErr: initErr}
	a2 := readyRow{task: readyTaskRecord("a2", "alpha"), setNextErr: initErr}
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha", "beta"},
		rows: []readyRow{
			a1, a2,
			readyDueRow("b1", "beta", base, 30*time.Second),
		},
		batch: 2,
	})

	harness.step(t, 1, nil)
	harness.waitPass(t, 2)
	require.Equal(t, []string{"a1", "a2"}, harness.store.pageAt(0), "首屏只取 NULL 段")
	require.Empty(t, harness.store.claimedLog(), "NULL 行只初始化，不执行")

	advances := harness.store.advanceLog()
	require.Len(t, advances, 2, "每一行都必须尝试一次初始化")
	for _, advance := range advances {
		require.Nil(t, advance.expected, "NULL 行走高基线的初始化分支，不做 CAS")
	}

	harness.release()
	harness.waitPass(t, 3)
	require.Equal(t, []string{"b1"}, harness.store.pageAt(1),
		"整页 NULL 之后必须取到非 NULL 段的健康任务")
	require.Equal(t, []string{"b1"}, readyClaimIDs(harness.store.claimedLog()))
	require.Eventually(t, func() bool { return harness.invoker.count() == 1 },
		5*time.Second, 5*time.Millisecond, "领取成功后必须在执行链里发起一次运行")
}

func readyClaimIDs(claims []readyClaim) []string {
	ids := make([]string, 0, len(claims))
	for _, claim := range claims {
		ids = append(ids, claim.taskID)
	}
	return ids
}

// TestScheduler_SyncRefreshKeepsScanProgress 守"运行期集合替换不得复位扫描进度"：第一页
// 的唯一一行被零写入拒绝后，`ReplaceEntries` 把新 Agent 纳入本进程，第二拍必须接着
// 上一页的元组下界取**下一页**（t2），而不是退回首屏重读 t1；同样不得重新开圈。
// ReplaceEntries must never reset the scan cursor or reopen the sweep.
func TestScheduler_SyncRefreshKeepsScanProgress(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	stuck := readyDueRow("t1", "alpha", base, time.Minute)
	stuck.claimErr = core.ErrAssetMoved
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha"},
		rows: []readyRow{
			stuck,
			readyDueRow("t2", "beta", base, 30*time.Second),
		},
		batch: 1,
	})

	// 集合替换发生在第一拍的闸口：本拍的 Agent 集合快照已取走，替换从下一拍生效。
	harness.step(t, 1, func() {
		require.NoError(t, harness.sched.ReplaceEntries([]core.AgentEntry{
			{Name: "alpha", Registered: true},
			{Name: "beta", Registered: true},
		}))
		harness.book.set("beta", "sha256:beta")
	})
	harness.waitPass(t, 2)
	require.Equal(t, []string{"t1"}, harness.store.pageAt(0))
	require.Equal(t, 1, harness.store.sweepCount())

	harness.release()
	harness.waitPass(t, 3)

	require.Equal(t, []string{"t2"}, harness.store.pageAt(1),
		"集合替换后必须接着上一页前进，不得退回首屏重读 t1")
	require.Equal(t, 1, harness.store.sweepCount(),
		"集合替换不得重新开圈（开圈次数保持 1）")
	require.Equal(t, []string{"t2"}, readyClaimIDs(harness.store.claimedLog()))
	require.True(t, harness.store.rowNextFire("t1").Equal(base.Add(-time.Minute)),
		"被拒绝的行必须原样留在原地")
}

// TestScheduler_MissedBehindUnreadyPrefixStillRecorded 守"错失分支在就绪门之前"：越过
// 宽限的未就绪行必须**如实记 missed 并推进**，而不是被门悄悄挡住——否则该条永远不消失。
// The missed branch precedes the readiness gate: overdue unready rows are still recorded.
func TestScheduler_MissedBehindUnreadyPrefixStillRecorded(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha"},
		rows: []readyRow{
			readyDueRow("m1", "alpha", base, 2*time.Hour),
		},
		batch:       2,
		missedGrace: time.Hour,
	})
	harness.book.unset("alpha", errors.New("asset sync: 尚未安装"))

	harness.step(t, 1, nil)
	harness.waitPass(t, 2)

	missed := harness.store.missedLogs()
	require.Len(t, missed, 1, "越过宽限的未就绪行必须记一条 missed")
	require.Equal(t, "m1", missed[0].taskID)
	require.Equal(t, "missed_grace_exceeded", missed[0].reason)
	require.Equal(t, 1, missed[0].missed)
	require.True(t, missed[0].plannedAt.Equal(base.Add(-2*time.Hour)))
	require.True(t, missed[0].nextFireAt.After(base), "推进到未来，不逐点补跑")

	require.Empty(t, harness.store.claimedLog(), "错失的行不得领取")
	require.Zero(t, harness.book.callsFor("alpha"),
		"错失分支在就绪门之前：越过宽限的行不得再求值就绪门")
	require.Zero(t, harness.invoker.count())
	require.True(t, harness.store.rowNextFire("m1").After(base), "next_fire_at 已在同一事务推进")
}

// TestScheduler_DueScanCursorWrapsAfterSweep 守"两段键集轮转 + 短页即圈尾"：三行到期、
// 单轮上限 2，一圈必须恰好走完三页（满页 / 满页 / 短页），最后一页之后边界被清空，
// 随后重新开圈。
// A sweep must walk to a short page and then reopen with a fresh boundary.
func TestScheduler_DueScanCursorWrapsAfterSweep(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha"},
		rows: []readyRow{
			readyDueRow("t1", "alpha", base, 3*time.Minute),
			readyDueRow("t2", "alpha", base, 2*time.Minute),
			readyDueRow("t3", "alpha", base, time.Minute),
		},
		batch: 2,
	})

	harness.step(t, 1, nil)
	harness.step(t, 2, nil)
	harness.step(t, 3, nil)
	harness.waitPass(t, 4)

	require.Equal(t, []string{"t1", "t2"}, harness.store.pageAt(0))
	require.Equal(t, []string{"t3"}, harness.store.pageAt(1), "短页结束本圈")
	require.Empty(t, harness.store.pageAt(2), "新一圈从空页开始（三行都已推进到未来）")
	require.Equal(t, 3, harness.store.sweepCount(), "第 4 拍开第三圈")
	require.Equal(t, []string{"t1", "t2", "t3"}, readyClaimIDs(harness.store.claimedLog()))
	require.Eventually(t, func() bool { return harness.invoker.count() == 3 },
		5*time.Second, 5*time.Millisecond, "三次领取都必须在执行链里发起运行")
}

// TestScheduler_SweepCompletesUnderContinuousDue 守"持续高频到期仍能结束一圈"：alpha 的
// 四行始终到期且始终未就绪，beta 的任务在第二拍才就绪、同时新增一行 NULL 任务。
// 一圈走完之后必须重新开圈，把新增的 NULL 行纳入（否则它永远不被初始化），并且
// 中途就绪的 beta 任务必须在本圈内被领取。
// Continuous due rows still terminate the sweep, which then rediscovers new NULL rows.
func TestScheduler_SweepCompletesUnderContinuousDue(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha", "beta"},
		rows: []readyRow{
			readyDueRow("a1", "alpha", base, 4*time.Minute),
			readyDueRow("a2", "alpha", base, 3*time.Minute),
			readyDueRow("a3", "alpha", base, 2*time.Minute),
			readyDueRow("a4", "alpha", base, time.Minute),
			readyDueRow("b1", "beta", base, 30*time.Second),
		},
		batch: 2,
	})
	harness.book.unset("alpha", errors.New("asset sync: alpha 尚未安装"))
	harness.book.unset("beta", errors.New("asset sync: beta 尚未安装"))

	harness.step(t, 1, nil)
	harness.step(t, 2, func() {
		// 第二拍取页之前：beta 就绪，并新增一行从未初始化过的 NULL 任务。
		harness.book.set("beta", "sha256:beta")
		harness.store.addRow(readyRow{task: readyTaskRecord("a0", "alpha")})
	})
	harness.step(t, 3, nil)
	harness.step(t, 4, nil)
	harness.step(t, 5, nil)
	harness.waitPass(t, 6)

	require.Equal(t, []string{"a1", "a2"}, harness.store.pageAt(0))
	require.Equal(t, []string{"a3", "a4"}, harness.store.pageAt(1))
	require.Equal(t, []string{"b1"}, harness.store.pageAt(2), "本圈内到达就绪的 beta 任务")
	require.Equal(t, []string{"a0", "a1"}, harness.store.pageAt(3),
		"新一圈先把 NULL 段的 a0 走完，再用剩余容量前进到非 NULL 段")
	require.Equal(t, 2, harness.store.sweepCount(), "第 4 拍开第二圈")

	require.Equal(t, []string{"b1"}, readyClaimIDs(harness.store.claimedLog()),
		"只有 beta 任务可领取")
	require.Eventually(t, func() bool { return harness.invoker.count() == 1 },
		5*time.Second, 5*time.Millisecond, "唯一可领取的任务必须真的跑起来")

	advances := harness.store.advanceLog()
	require.NotEmpty(t, advances)
	require.Equal(t, "a0", advances[0].taskID, "新增的 NULL 行必须被初始化，而不仅仅是被读到")
	require.Nil(t, advances[0].expected)
}

// TestScheduler_ScanRespectsFrozenSweepCutoff 守"圈界冻结"：开圈时冻结的 cutoff 是唯一
// 的到期判据。扫描中途把数据库时钟推到 t2 的到期时刻之后，同一圈**仍不得**取到 t2
// （它属于下一圈）；新一圈才把 t2 取出来。
// The frozen cutoff is the only due criterion inside one sweep; a mid-sweep clock jump
// must not pull a later row into the current sweep.
func TestScheduler_ScanRespectsFrozenSweepCutoff(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha"},
		rows: []readyRow{
			readyDueRow("t3", "alpha", base, 2*time.Minute),
			readyDueRow("t1", "alpha", base, time.Minute),
			readyDueRow("t2", "alpha", base.Add(10*time.Minute), 0),
		},
		batch: 2,
	})

	harness.step(t, 1, func() {
		// 开圈边界已经在本拍读走（cutoff = base）；这里推进的时钟只影响后续拍。
		harness.store.setClock(base.Add(15 * time.Minute))
	})
	harness.waitPass(t, 2)
	require.Equal(t, []string{"t3", "t1"}, harness.store.pageAt(0),
		"首屏只考察开圈时已到期的两行")

	harness.release()
	harness.waitPass(t, 3)
	require.Empty(t, harness.store.pageAt(1),
		"时钟已越过 t2 的到期时刻，但本圈冻结的 cutoff 不允许把它拉进来")

	harness.release()
	harness.waitPass(t, 4)
	require.Equal(t, []string{"t2"}, harness.store.pageAt(2), "新一圈才取到 t2")

	claims := harness.store.claimedLog()
	require.Equal(t, []string{"t3", "t1", "t2"}, readyClaimIDs(claims))
	require.Eventually(t, func() bool { return harness.invoker.count() == 3 },
		5*time.Second, 5*time.Millisecond, "三次领取都必须在执行链里发起运行")
	require.True(t, harness.store.rowNextFire("t2").Equal(base.Add(15*time.Minute).Add(2*time.Hour)),
		"下一次触发从 max(next_fire_at, db_now) 起算（新一圈的 db_now 是推进后的时钟）")
}

// TestClaim_ErrAssetMovedWritesNothing 守 FR-25C-11 的零写入结局：领取事务以
// `ErrAssetMoved` 拒绝时必须**一个写都不留**（不推进 next_fire_at、不写执行行、不记
// missed / 撞忙、不发起运行、不收尾），并且该触发在下一拍被重新考察。
// A refused pinned claim writes nothing at all and is re-examined next pass.
func TestClaim_ErrAssetMovedWritesNothing(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	row := readyDueRow("t1", "alpha", base, time.Minute)
	row.claimErr = core.ErrAssetMoved
	harness := newReadyHarness(t, readySpec{
		agents: []string{"alpha"},
		rows:   []readyRow{row},
		batch:  2,
	})

	harness.step(t, 1, nil)
	harness.step(t, 2, nil)
	harness.waitPass(t, 3)

	counts := harness.store.calls()
	require.Equal(t, 2, counts.pinnedAttempts, "被拒绝的触发必须在下一拍重新考察")
	require.Zero(t, counts.pinnedClaims)
	require.Zero(t, counts.unpinnedClaims, "有期望哈希时不得退回未钉住的领取")
	require.Zero(t, counts.setNextCalls, "零写入结局：不得推进 next_fire_at")
	require.Zero(t, counts.missedCalls)
	require.Zero(t, counts.busyCalls)
	require.Zero(t, counts.finishCalls)
	require.Zero(t, counts.markRunCalls)
	require.Zero(t, harness.invoker.count(), "被拒绝的触发不得发起运行")
	require.True(t, harness.store.rowNextFire("t1").Equal(base.Add(-time.Minute)),
		"planned_at 与 next_fire_at 都必须原样不动")
	require.Equal(t, "sha256:alpha", harness.store.claimLog()[0].expectHash,
		"领取必须带上就绪快照的哈希")
}

// ──────────────────────────────── 钉住 Run 的准入判据 ────────────────────────────────

// errPinSnapshotStop 是快照构建的哨兵：钉住路径走到这里就说明 Entry、资源解析与清单
// 复核全部通过，用例因此不必真的发起模型调用（真实模型测试一律禁用）。
var errPinSnapshotStop = errors.New("pin fixture: 快照构建到此为止")

// pinEntryLoader 把"Entry 来自哪一份 bundle"写进 Definition：判据据此断言同源。
type pinEntryLoader struct {
	mu    sync.Mutex
	calls int
}

func (l *pinEntryLoader) EntryFromBundle(_ string, installed assets.Installed,
	_ PreparedResources, _ map[string]string,
) (core.AgentEntry, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	return core.AgentEntry{
		Name:       installed.Manifest.Agent,
		Registered: true,
		Profile:    &core.Profile{Name: installed.Manifest.Agent},
		Definition: []byte("hash=" + installed.Manifest.BundleHash),
	}, nil
}

// pinAgentSource 把注册表当前快照与钉住装载面分开：普通 Run 取 current，钉住 Run 走
// 真实的 `pinnedEntries`。`Lookup` 的调用次数是"钉住 Run 没有回落当前快照"的证据。
type pinAgentSource struct {
	current core.AgentEntry

	mu            sync.Mutex
	currentCalls  int
	pinnedCalls   int
	pinnedQueries []string
	pinned        core.PinnedAgentSource
}

func (s *pinAgentSource) Lookup(string) (core.AgentEntry, bool) {
	s.mu.Lock()
	s.currentCalls++
	s.mu.Unlock()
	return s.current, true
}

func (s *pinAgentSource) Dir(string) string { return "" }

func (s *pinAgentSource) Snapshot(context.Context, core.AgentEntry, time.Time) (*core.ConfigSnapshot, error) {
	return nil, errors.New("pin fixture: 钉住路径不得走工作副本快照")
}

func (s *pinAgentSource) LookupPinned(ctx context.Context, agent, hash string) (core.AgentEntry, error) {
	s.mu.Lock()
	s.pinnedCalls++
	s.pinnedQueries = append(s.pinnedQueries, hash)
	s.mu.Unlock()
	return s.pinned.LookupPinned(ctx, agent, hash)
}

func (s *pinAgentSource) currentLookups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentCalls
}

func (s *pinAgentSource) pinnedHashes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.pinnedQueries...)
}

// pinRunRuntime 是准入固定的运行时：`Provider` 必须发放条目（模型调用入口从这里取），
// `Release` 由 Run 的 defer 调用一次。
type pinRunRuntime struct {
	mu       sync.Mutex
	releases int
}

func (r *pinRunRuntime) Provider(string) (core.ProviderClient, bool) { return nil, true }

func (r *pinRunRuntime) MCP(string) (core.MCPHandle, bool) { return nil, false }

func (r *pinRunRuntime) ReleaseAdmission() {}

func (r *pinRunRuntime) Release() {
	r.mu.Lock()
	r.releases++
	r.mu.Unlock()
}

func (r *pinRunRuntime) releaseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releases
}

// pinRunResources 记录两条解析路径的调用：钉住 Run 只准走 `ResolvePinned`。
// `bundleHash` 是交给清单的资产哈希——把它设成与领取钉住值不同的哈希即可复现
// "装配把两条路径接错"的情况。
type pinRunResources struct {
	resourceUID core.ResourceUID
	bundleHash  string

	mu          sync.Mutex
	resolveCall int
	pinnedCall  int
	pinnedSeen  []string
	runtime     *pinRunRuntime
}

func newPinRunResources(resourceUID core.ResourceUID, bundleHash string) *pinRunResources {
	return &pinRunResources{resourceUID: resourceUID, bundleHash: bundleHash, runtime: &pinRunRuntime{}}
}

func (r *pinRunResources) Resolve(context.Context, core.AgentEntry) (core.ResolvedResources, error) {
	r.mu.Lock()
	r.resolveCall++
	r.mu.Unlock()
	return r.resolved(), nil
}

func (r *pinRunResources) ResolvePinned(_ context.Context, _ core.AgentEntry, hash string) (core.ResolvedResources, error) {
	r.mu.Lock()
	r.pinnedCall++
	r.pinnedSeen = append(r.pinnedSeen, hash)
	r.mu.Unlock()
	return r.resolved(), nil
}

func (r *pinRunResources) resolved() core.ResolvedResources {
	r.mu.Lock()
	hash := r.bundleHash
	r.mu.Unlock()
	return core.ResolvedResources{
		Provider: core.ResourceVersionRef{
			Name:        "prov",
			ResourceUID: r.resourceUID,
			VersionID:   1,
			VersionNo:   1,
			ContentHash: "sha256:prov",
		},
		ProviderModel: "model-x",
		ProviderCredential: core.CredentialPin{
			Mode:          core.CredentialModeEnv,
			TargetBinding: strings.Repeat("a", 64),
		},
		Notify:      core.NotifyManifest{Mode: core.NotifyModeNone},
		AssetBundle: core.AssetBundlePin{Hash: hash},
		Runtime:     r.runtime,
	}
}

func (r *pinRunResources) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resolveCall, r.pinnedCall
}

func (r *pinRunResources) pinnedHashes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pinnedSeen...)
}

// pinSnapshotBuilder 在快照构建处停住：Entry、资源与清单都已经过，且**没有**发起模型调用。
type pinSnapshotBuilder struct {
	mu        sync.Mutex
	calls     int
	lastEntry core.AgentEntry
}

func (b *pinSnapshotBuilder) Build(_ context.Context, entry core.AgentEntry, _ core.ResolvedResources) (core.ConfigSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.lastEntry = entry
	return core.ConfigSnapshot{}, errPinSnapshotStop
}

func (b *pinSnapshotBuilder) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *pinSnapshotBuilder) definition() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.lastEntry.Definition)
}

// pinClaimReader 回读领取记录；`ok=false` 复现"领取行不存在"。
type pinClaimReader struct {
	record core.ClaimedPinRecord
	ok     bool
	err    error
}

func (r *pinClaimReader) ReadClaimedPin(context.Context, int64) (core.ClaimedPinRecord, bool, error) {
	return r.record, r.ok, r.err
}

// pinLifecycle 是生命周期端口：只实现准入读，其余经内嵌 nil 接口保持未实现。
type pinLifecycle struct {
	core.LifecyclePort

	record core.LifecycleRecord
	exists bool
	err    error
}

func (l *pinLifecycle) Read(context.Context, string) (core.LifecycleRecord, bool, error) {
	return l.record, l.exists, l.err
}

// pinScene 是一台钉住 Run 的装配现场：真实资产根 + 两个已安装版本 + 全部替身。
type pinScene struct {
	fixture   *assetSyncFixture
	agent     string
	uid       core.AgentUID
	oldHash   string
	newHash   string
	pinned    *pinnedEntries
	source    *pinAgentSource
	resources *pinRunResources
	snapshots *pinSnapshotBuilder
	claims    *pinClaimReader
	lifecycle *pinLifecycle
	service   *core.AgentService
}

// pinSceneOptions 允许用例改掉场景里的某一处装配（伪造领取记录 / 生命周期 / 解析哈希）。
type pinSceneOptions struct {
	mutateClaim  func(*core.ClaimedPinRecord)
	claimOK      bool
	lifecycleRec core.LifecycleRecord
	resolvedHash func(oldHash string) string
}

// newPinScene 砌一份"同步已经把 H2 装好、领取钉住的仍是 H1"的现场。
func newPinScene(t *testing.T, opts pinSceneOptions) *pinScene {
	t.Helper()
	fixture := newAssetSyncFixture(t)
	agent := "alpha"
	uid := syncUID(1)
	oldHash := fixture.addInstalledBundle(agent, uid, 1)
	newHash := fixture.addInstalledBundle(agent, uid, 2)
	require.NotEqual(t, oldHash, newHash, "两次发布必须是两个不同哈希")

	loader := &pinEntryLoader{}
	pinned := newPinnedEntries(fixture.assetRoot, fixture.workspaceRoot, fixture.dist, loader, nil)
	newEntry, err := pinned.LookupPinned(context.Background(), agent, newHash)
	require.NoError(t, err, "H2 必须已安装且可装载")

	scene := &pinScene{
		fixture: fixture, agent: agent, uid: uid,
		oldHash: oldHash, newHash: newHash, pinned: pinned,
		source: &pinAgentSource{current: newEntry, pinned: pinned},
	}
	resolvedHash := oldHash
	if opts.resolvedHash != nil {
		resolvedHash = opts.resolvedHash(oldHash)
	}
	scene.resources = newPinRunResources(
		fixtureResourceUID(t, core.ResourceModelConnection, "prov"), resolvedHash)
	scene.snapshots = &pinSnapshotBuilder{}
	scene.lifecycle = &pinLifecycle{
		record: core.LifecycleRecord{
			SchemaVersion: 2,
			Agent:         agent,
			AgentUID:      uid,
			State:         core.AgentActive,
			Generation:    1,
			Checksum:      "sha256:" + strings.Repeat("b", 64),
		},
		exists: true,
	}
	if opts.lifecycleRec.State != "" {
		scene.lifecycle.record = opts.lifecycleRec
	}

	record := core.ClaimedPinRecord{
		ExecutionID:    7,
		RunID:          "run-7",
		Agent:          agent,
		Status:         string(core.ExecutionStarted),
		BundleHash:     oldHash,
		OwnerWorkerID:  readyWorker.InstanceID,
		OwnershipEpoch: 1,
	}
	if opts.mutateClaim != nil {
		opts.mutateClaim(&record)
	}
	scene.claims = &pinClaimReader{record: record, ok: opts.claimOK}

	scene.service = core.NewAgentService(core.ServiceOptions{
		Agents:       scene.source,
		Loop:         core.NewReActLoop(nil, nil, readyLogger()),
		Logger:       readyLogger(),
		Now:          func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) },
		Lifecycle:    scene.lifecycle,
		Resources:    scene.resources,
		Claims:       scene.claims,
		RunSnapshots: scene.snapshots,
	})
	return scene
}

// request 组装一次钉住 Run 请求（调度通道才会填 `Pinned`）。
func (s *pinScene) request() core.RunRequest {
	return core.RunRequest{
		Principal: core.LocalAdminPrincipal(),
		RunID:     "run-7",
		Agent:     s.agent,
		SessionID: "scheduler-session",
		Message:   "定时任务的正文",
		Channel:   "scheduler",
		UserID:    "scheduler",
		Trigger:   core.TriggerCron,
		Pinned: &core.ClaimedBundlePin{
			ExecutionID: 7,
			BundleHash:  s.oldHash,
			Lease: core.LeaseToken{
				Kind:           core.LeaseScheduleExecution,
				ResourceID:     "run-7",
				OwnerWorkerID:  readyWorker.InstanceID,
				OwnershipEpoch: 1,
			},
		},
	}
}

// TestRun_UsesPinnedBundleWhileSyncAdvances 守 FR-25C-08 的固定版本：同步已经把 H2
// 装好并推进了指针，领取钉住 H1 的 Run 仍必须用 H1 的 Entry 与 H1 的资源解析——
// 不读注册表当前快照、不读工作副本、不按当前指针重解析。
// A pinned run keeps serving the claimed bundle after sync advanced to a newer one.
func TestRun_UsesPinnedBundleWhileSyncAdvances(t *testing.T) {
	t.Parallel()

	scene := newPinScene(t, pinSceneOptions{claimOK: true})
	res, err := scene.service.Run(context.Background(), scene.request())
	require.NoError(t, err)
	require.Equal(t, core.StatusFailed, res.Status, "哨兵在快照构建处停下")
	require.Equal(t, errPinSnapshotStop.Error(), res.Reason)

	resolveCalls, pinnedCalls := scene.resources.counts()
	require.Zero(t, resolveCalls, "钉住 Run 不得走按当前指针解析的 Resolve")
	require.Equal(t, 1, pinnedCalls, "钉住 Run 必须按固定哈希解析一次")
	require.Equal(t, []string{scene.oldHash}, scene.resources.pinnedHashes())

	require.Equal(t, 1, scene.snapshots.callCount())
	require.Equal(t, "hash="+scene.oldHash, scene.snapshots.definition(),
		"Entry 必须来自被钉住的那一份 bundle，而不是同步已推进的新版本")
	require.Zero(t, scene.source.currentLookups(), "钉住 Run 不得回落到注册表的当前快照")
	require.Equal(t, []string{scene.oldHash}, scene.source.pinnedHashes())
	require.Equal(t, 1, scene.resources.runtime.releaseCount(), "准入固定的运行时必须释放一次")
}

// TestRun_ForgedPinWithoutClaimRecordRejected 守"伪造领取记录一律拒绝"：领取行缺失、
// 状态不是 started、Run / Agent / 哈希 / 租约副本任一不符，都必须以 `ErrClaimPinInvalid`
// 在**任何装载与解析之前**失败关闭（web 面据此映射 503 ASSET_UNAVAILABLE）。
// 不得退回当前指针，也不得当成普通 Run 继续。
// A forged claim is rejected before any load or resolution; it never falls back.
func TestRun_ForgedPinWithoutClaimRecordRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		options pinSceneOptions
	}{
		{
			name:    "领取行不存在",
			options: pinSceneOptions{claimOK: false},
		},
		{
			name: "状态不是 started",
			options: pinSceneOptions{
				claimOK: true,
				mutateClaim: func(record *core.ClaimedPinRecord) {
					record.Status = string(core.ExecutionCompleted)
				},
			},
		},
		{
			name: "Run 不符",
			options: pinSceneOptions{
				claimOK: true,
				mutateClaim: func(record *core.ClaimedPinRecord) {
					record.RunID = "run-other"
				},
			},
		},
		{
			name: "Agent 不符",
			options: pinSceneOptions{
				claimOK: true,
				mutateClaim: func(record *core.ClaimedPinRecord) {
					record.Agent = "beta"
				},
			},
		},
		{
			name: "哈希不符",
			options: pinSceneOptions{
				claimOK: true,
				mutateClaim: func(record *core.ClaimedPinRecord) {
					record.BundleHash = strings.Repeat("0", 64)
				},
			},
		},
		{
			name: "租约归属不符",
			options: pinSceneOptions{
				claimOK: true,
				mutateClaim: func(record *core.ClaimedPinRecord) {
					record.OwnerWorkerID = "worker-z-9"
				},
			},
		},
		{
			name: "租约代次不符",
			options: pinSceneOptions{
				claimOK: true,
				mutateClaim: func(record *core.ClaimedPinRecord) {
					record.OwnershipEpoch = 2
				},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			scene := newPinScene(t, testCase.options)
			_, err := scene.service.Run(context.Background(), scene.request())
			require.ErrorIs(t, err, core.ErrClaimPinInvalid)

			resolveCalls, pinnedCalls := scene.resources.counts()
			require.Zero(t, resolveCalls, "伪造的领取记录不得触发按当前指针解析")
			require.Zero(t, pinnedCalls, "伪造的领取记录不得触发钉住装载")
			require.Zero(t, scene.snapshots.callCount(), "伪造的领取记录不得推进到快照")
			require.Zero(t, scene.source.currentLookups(), "不得退回注册表当前快照")
			require.Empty(t, scene.source.pinnedHashes())
			require.Zero(t, scene.resources.runtime.releaseCount(), "没有解析就没有运行时需要释放")
		})
	}

	t.Run("没有领取通道", func(t *testing.T) {
		t.Parallel()
		scene := newPinScene(t, pinSceneOptions{claimOK: true})
		scene.service = core.NewAgentService(core.ServiceOptions{
			Agents:       scene.source,
			Loop:         core.NewReActLoop(nil, nil, readyLogger()),
			Logger:       readyLogger(),
			Lifecycle:    scene.lifecycle,
			Resources:    scene.resources,
			RunSnapshots: scene.snapshots,
		})
		_, err := scene.service.Run(context.Background(), scene.request())
		require.ErrorIs(t, err, core.ErrClaimPinInvalid,
			"没有领取通道时钉住请求必须按无效领取拒绝，不得当成普通 Run")
	})
}

// TestStartRun_PinnedClaimSubstitutesOnlyPointerEquality 守"钉住只替换指针相等那一环"：
// 钉住路径省掉的**只有**"bundle 哈希等于当前指针"这一条判据，其余准入（生命周期、
// 资源解析、清单、设置）逐项照旧；而清单与领取钉住值不符时仍以 `ErrClaimPinInvalid`
// 失败关闭——两条路径接错不得带错版本继续跑。
// The pin substitutes only the pointer-equality check; every other admission step stays.
func TestStartRun_PinnedClaimSubstitutesOnlyPointerEquality(t *testing.T) {
	t.Parallel()

	t.Run("钉住路径走通", func(t *testing.T) {
		t.Parallel()
		scene := newPinScene(t, pinSceneOptions{claimOK: true})
		res, err := scene.service.Run(context.Background(), scene.request())
		require.NoError(t, err)
		require.Equal(t, core.StatusFailed, res.Status)
		require.Equal(t, errPinSnapshotStop.Error(), res.Reason)
		resolveCalls, pinnedCalls := scene.resources.counts()
		require.Zero(t, resolveCalls)
		require.Equal(t, 1, pinnedCalls)
		require.Equal(t, 1, scene.snapshots.callCount())
	})

	t.Run("生命周期门照旧", func(t *testing.T) {
		t.Parallel()
		scene := newPinScene(t, pinSceneOptions{
			claimOK: true,
			lifecycleRec: core.LifecycleRecord{
				SchemaVersion: 2,
				Agent:         "alpha",
				AgentUID:      syncUID(1),
				State:         core.AgentDisabled,
				Generation:    2,
				Checksum:      "sha256:" + strings.Repeat("c", 64),
			},
		})
		_, err := scene.service.Run(context.Background(), scene.request())
		var unavailable *core.AgentUnavailableError
		require.ErrorAs(t, err, &unavailable, "钉住只替换指针判据，生命周期门必须照旧拒绝")

		resolveCalls, pinnedCalls := scene.resources.counts()
		require.Zero(t, resolveCalls)
		require.Zero(t, pinnedCalls, "生命周期拒绝发生在解析之前")
		require.Zero(t, scene.snapshots.callCount())
	})

	t.Run("清单与钉住哈希不符", func(t *testing.T) {
		t.Parallel()
		scene := newPinScene(t, pinSceneOptions{
			claimOK:      true,
			resolvedHash: func(string) string { return "sha256:" + strings.Repeat("d", 64) },
		})
		_, err := scene.service.Run(context.Background(), scene.request())
		require.ErrorIs(t, err, core.ErrClaimPinInvalid,
			"清单记录的 bundle 必须就是领取钉住的那一份")

		_, pinnedCalls := scene.resources.counts()
		require.Equal(t, 1, pinnedCalls)
		require.Zero(t, scene.snapshots.callCount(), "对账失败不得推进到快照构建")
		require.Equal(t, 1, scene.resources.runtime.releaseCount(), "解析出的运行时必须释放")
	})
}
