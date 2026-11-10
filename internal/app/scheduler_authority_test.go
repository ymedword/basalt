package app

// 本文件是 US-25C FR-25C-13 的角色门判据：`DefinitionAuthority=false`（Worker）时
// 调度器**不得**写共享任务定义列，但领取、`next_fire_at` 推进、执行行、租约等执行
// 协调写必须照常。三组用例分别守三件事：
//
//   - `TestWorkerRestart_KeepsNewChannel`：旧 Worker 持旧种子重启，Start 的只读对账
//     不得把共享定义列（这里以 `deliver_to` 渠道绑定为代表）写回旧值；
//   - `TestWorker_AdvancesNextFire`：同一角色下到期领取与 `next_fire_at` 推进照旧；
//   - `TestScheduler_DefinitionAuthorityFailClosed`：运行期 `ReconcileAgent` /
//     `SetRegistered(false)` 失败关闭且零写入（Start 的启动对账按模块卡 §2.3.1
//     「只读对账」继续，不算失败关闭面）。
//
// This file pins the worker side of the scheduler role gate: no shared definition
// writes, but every execution-coordination write stays live.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// authorityAgent / authorityTaskID 是三个用例共用的 Agent 与任务标识。
	authorityAgent  = "writer"
	authorityTaskID = "writer-1"
)

// authorityApply 是一次 ApplyAgentTasks 调用的读数（共享定义列写面）。
type authorityApply struct {
	agent   string
	upserts []core.TaskRecord
	retire  []string
}

// authorityClaim 是一次领取 / 撞忙推进的读数：plannedAt 是本轮计划时刻，next 是
// 交给仓储的下一触发点。`next` 严格大于 `plannedAt` 即证明 `next_fire_at` 在推进。
type authorityClaim struct {
	taskID    string
	plannedAt time.Time
	next      time.Time
}

// authorityStore 是角色门用例的最小 TaskStore + LeaseStore 替身：**只**实现用例
// 观测到的方法，其余经内嵌 nil 接口保持未实现——误调用即 panic，避免"顺手实现"
// 把未观测的写路径悄悄放行。
// authorityStore implements only the observed methods; the embedded nil interfaces
// make every other call panic instead of quietly standing in for a write.
type authorityStore struct {
	core.TaskStore
	core.LeaseStore

	mu       sync.Mutex
	rows     []core.TaskRecord
	due      []core.DueTask
	applyLog []authorityApply
	claimLog []authorityClaim
	busyLog  []authorityClaim
	claimErr error
}

// ListTasks 返回当前共享行快照。
func (s *authorityStore) ListTasks(context.Context) ([]core.TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.TaskRecord, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

// ApplyAgentTasks 记录并**真的**改写共享行：若角色门失效，用例读到的 `deliver_to`
// 就是被 Worker 写回旧值的那一列，红态因此显示真实倒退而不是只差一个计数。
func (s *authorityStore) ApplyAgentTasks(_ context.Context, agent string,
	upserts []core.TaskRecord, retireIDs []string, now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLog = append(s.applyLog, authorityApply{
		agent:   agent,
		upserts: append([]core.TaskRecord(nil), upserts...),
		retire:  append([]string(nil), retireIDs...),
	})
	for _, upsert := range upserts {
		replaced := false
		for i := range s.rows {
			if s.rows[i].TaskID != upsert.TaskID {
				continue
			}
			// 保留库侧 enabled（定义变化不翻开关，注 14），其余列按定义覆盖。
			upsert.Enabled = s.rows[i].Enabled
			s.rows[i] = upsert
			replaced = true
			break
		}
		if !replaced {
			s.rows = append(s.rows, upsert)
		}
	}
	for _, id := range retireIDs {
		for i := range s.rows {
			if s.rows[i].TaskID == id {
				s.rows[i].RetiredAt = now
			}
		}
	}
	return nil
}

// ListDispatchable 返回预置的到期行。
func (s *authorityStore) ListDispatchable(context.Context, int) ([]core.DueTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.DueTask, len(s.due))
	copy(out, s.due)
	return out, nil
}

// ClaimDueTrigger 只记录不执行：用例判的是"领取与推进照旧发生"，执行链不在本文件
// 的判据面内（返回撞忙后由 RecordBusyTrigger 承接推进写）。
func (s *authorityStore) ClaimDueTrigger(_ context.Context, taskID string,
	plannedAt, nextFireAt time.Time, _ core.WorkerIdentity, _ time.Duration,
) (core.DueExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimLog = append(s.claimLog, authorityClaim{taskID: taskID, plannedAt: plannedAt, next: nextFireAt})
	return core.DueExecution{}, s.claimErr
}

// RecordBusyTrigger 记录撞忙路径的推进写。
func (s *authorityStore) RecordBusyTrigger(_ context.Context, taskID string,
	plannedAt, nextFireAt time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busyLog = append(s.busyLog, authorityClaim{taskID: taskID, plannedAt: plannedAt, next: nextFireAt})
	return nil
}

// row 读回一条共享行。
func (s *authorityStore) row(taskID string) (core.TaskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.rows {
		if rec.TaskID == taskID {
			return rec, true
		}
	}
	return core.TaskRecord{}, false
}

// applyCalls 返回共享定义列写的次数。
func (s *authorityStore) applyCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.applyLog)
}

// claims 返回领取读数快照。
func (s *authorityStore) claims() []authorityClaim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]authorityClaim(nil), s.claimLog...)
}

// busyAdvances 返回撞忙推进读数快照。
func (s *authorityStore) busyAdvances() []authorityClaim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]authorityClaim(nil), s.busyLog...)
}

// authorityEngine 是 cron 引擎替身：下一次触发固定为一小时后，逐次前进即可。
type authorityEngine struct{}

// Next 返回 from 之后一小时。
func (authorityEngine) Next(_ string, from time.Time) (time.Time, error) {
	return from.Add(time.Hour), nil
}

// Occurrences 只服务启动错失告警；本文件不考察。
func (authorityEngine) Occurrences(string, time.Time, time.Time, int) []time.Time { return nil }

// authorityCronSpec 拼出带时区的 6 段表达式（与生产同形）。
func authorityCronSpec(s core.Schedule) (string, error) {
	zone := s.Zone
	if zone == "" {
		zone = "UTC"
	}
	return "CRON_TZ=" + zone + " " + s.Cron, nil
}

// authorityLogger 静默掉调度器日志：本文件判状态，不判日志。
func authorityLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// authorityWorker 是领取归属身份（非零才允许 Start）。
var authorityWorker = core.WorkerIdentity{
	InstanceID: "worker-b-1",
	Name:       "worker-b",
	Hostname:   "host-b",
	Version:    "test",
}

// newAuthorityScheduler 按给定定义权装配一台调度器；Entries 只含共享行里那个
// 已注册 Agent，Profile 里放的是 **Worker 本地的旧定义**。
// newAuthorityScheduler assembles a scheduler whose in-memory definitions are the
// worker's stale local seed while the store holds the authoritative rows.
func newAuthorityScheduler(t *testing.T, store *authorityStore, definitionAuthority bool,
	entries []core.AgentEntry,
) *core.Scheduler {
	t.Helper()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	sched := core.NewScheduler(core.SchedulerOptions{
		Store:    store,
		Invoker:  authorityInvoker{},
		Engine:   authorityEngine{},
		Entries:  entries,
		CronSpec: authorityCronSpec,
		Logger:   authorityLogger(),
		Now:      func() time.Time { return now },
		Leases:   store,
		Worker:   authorityWorker,

		LeaseTTL:      time.Minute,
		RenewInterval: 10 * time.Second,
		PollInterval:  time.Hour,
		MissedGrace:   time.Hour,
		// Worker 的定义权为 false；control 为 true。此字段就是 T033 的行为开关。
		DefinitionAuthority: definitionAuthority,
	})
	t.Cleanup(sched.Stop)
	return sched
}

// authorityInvoker 在领取成功后才会被调用；本文件的领取一律撞忙，故不会被触达。
type authorityInvoker struct{}

// Run 报告未预期调用。
func (authorityInvoker) Run(context.Context, core.RunRequest) (core.RunResult, error) {
	panic("authorityInvoker: 本用例不应发起执行")
}

// TestWorkerRestart_KeepsNewChannel 守 FR-25C-13 的"定义不倒退"：共享行里的
// `deliver_to` 已由控制面改成新渠道，Worker 带着旧种子的定义重启，Start 的启动
// 对账**不得**把旧渠道写回共享列。
// The worker's stale seed must never roll the shared deliver_to back on restart.
func TestWorkerRestart_KeepsNewChannel(t *testing.T) {
	t.Parallel()

	staleDefinitions := []core.Schedule{{
		ID:        authorityTaskID,
		Cron:      "0 */5 * * * *",
		Zone:      "UTC",
		Message:   "日报",
		DeliverTo: "notify-old-uid",
	}}
	store := &authorityStore{rows: []core.TaskRecord{{
		TaskID:          authorityTaskID,
		Agent:           authorityAgent,
		Cron:            "0 */5 * * * *",
		Zone:            "UTC",
		Message:         "日报",
		CallerID:        core.LocalAdminCallerID,
		AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		DeliverTo:       "notify-new-uid",
		Enabled:         true,
	}}}
	sched := newAuthorityScheduler(t, store, false, []core.AgentEntry{{
		Name:       authorityAgent,
		Registered: true,
		Profile:    &core.Profile{Name: authorityAgent, Schedules: staleDefinitions},
	}})

	require.NoError(t, sched.Start(context.Background()),
		"Worker 的启动对账是只读的（§2.3.1），不得因定义权缺失而拒绝启动")

	require.Zero(t, store.applyCalls(),
		"Worker 重启不得写共享任务定义列（ApplyAgentTasks 必须零调用）")
	row, ok := store.row(authorityTaskID)
	require.True(t, ok)
	require.Equal(t, "notify-new-uid", row.DeliverTo,
		"旧 Worker 重启不得让共享定义列倒退到本地旧渠道")
}

// TestWorker_AdvancesNextFire 守 FR-25C-13 的另一半：定义权缺失只挡定义列，
// 到期领取与 `next_fire_at` 推进必须照常——把协调写一起禁掉会让 Worker 彻底不干活。
// The gate must not spill over into claim/advance: those coordination writes stay live.
func TestWorker_AdvancesNextFire(t *testing.T) {
	t.Parallel()

	planned := time.Date(2026, 11, 9, 11, 55, 0, 0, time.UTC)
	store := &authorityStore{
		rows: []core.TaskRecord{{
			TaskID:          authorityTaskID,
			Agent:           authorityAgent,
			Cron:            "0 */5 * * * *",
			Zone:            "UTC",
			Message:         "巡检",
			CallerID:        core.LocalAdminCallerID,
			AuthorizingKind: core.AuthorizingKindAdminBuiltin,
			Enabled:         true,
		}},
		due: []core.DueTask{{Task: core.TaskRecord{
			TaskID:          authorityTaskID,
			Agent:           authorityAgent,
			Cron:            "0 */5 * * * *",
			Zone:            "UTC",
			Message:         "巡检",
			CallerID:        core.LocalAdminCallerID,
			AuthorizingKind: core.AuthorizingKindAdminBuiltin,
			Enabled:         true,
		}, NextFireAt: planned}},
		claimErr: core.ErrTaskBusy,
	}
	sched := newAuthorityScheduler(t, store, false, []core.AgentEntry{{
		Name:       authorityAgent,
		Registered: true,
		Profile:    &core.Profile{Name: authorityAgent},
	}})

	require.NoError(t, sched.Start(context.Background()))

	require.Eventually(t, func() bool { return len(store.busyAdvances()) > 0 },
		5*time.Second, 5*time.Millisecond, "Worker 到期扫描必须照常领取（撞忙后推进 next_fire_at）")

	claims := store.claims()
	require.Len(t, claims, 1)
	require.Equal(t, planned, claims[0].plannedAt, "领取的计划时刻必须是库里的到期点")
	require.True(t, claims[0].next.After(claims[0].plannedAt),
		"领取事务必须把 next_fire_at 推进到计划时刻之后")

	advances := store.busyAdvances()
	require.Len(t, advances, 1)
	require.Equal(t, planned, advances[0].plannedAt)
	require.True(t, advances[0].next.After(advances[0].plannedAt),
		"撞忙路径同样必须推进 next_fire_at，否则该 planned_at 被永久占住")

	require.Zero(t, store.applyCalls(),
		"协调写照常不意味着定义写放行：ApplyAgentTasks 仍须零调用")
}

// TestScheduler_DefinitionAuthorityFailClosed 守运行期失败关闭：Worker 上出现
// 定义协调（ReconcileAgent）或移出注册集合（SetRegistered(false)，会退休共享行）
// 时必须**报错且零写入**，让装配缺陷可见，而不是悄悄改共享定义。
// Runtime definition writes fail closed on a worker instead of silently landing.
func TestScheduler_DefinitionAuthorityFailClosed(t *testing.T) {
	t.Parallel()

	store := &authorityStore{rows: []core.TaskRecord{{
		TaskID:          authorityTaskID,
		Agent:           authorityAgent,
		Cron:            "0 */5 * * * *",
		Zone:            "UTC",
		Message:         "日报",
		CallerID:        core.LocalAdminCallerID,
		AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		Enabled:         true,
	}}}
	sched := newAuthorityScheduler(t, store, false, []core.AgentEntry{{
		Name:       authorityAgent,
		Registered: true,
		Profile:    &core.Profile{Name: authorityAgent},
	}})
	require.NoError(t, sched.Start(context.Background()))
	require.Zero(t, store.applyCalls(), "Worker 启动对账不得写定义列")

	err := sched.ReconcileAgent(context.Background(), authorityAgent, []core.Schedule{{
		ID:      authorityTaskID,
		Cron:    "0 */9 * * * *",
		Zone:    "UTC",
		Message: "被改写",
	}})
	require.ErrorContains(t, err, "定义写权",
		"Worker 的运行期定义协调必须失败关闭")
	require.Zero(t, store.applyCalls(), "失败关闭必须零写入")

	err = sched.SetRegistered(authorityAgent, false)
	require.ErrorContains(t, err, "定义写权",
		"移出注册集合会退休共享行，Worker 上必须失败关闭")
	require.Zero(t, store.applyCalls(), "失败关闭必须零写入")

	row, ok := store.row(authorityTaskID)
	require.True(t, ok)
	require.True(t, row.RetiredAt.IsZero(), "共享行不得被 Worker 退休")
	require.Equal(t, "0 */5 * * * *", row.Cron, "共享定义列不得被 Worker 改写")
}
