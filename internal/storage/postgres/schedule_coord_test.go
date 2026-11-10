//go:build integration

package postgres

// 本文件是 US-24 调度协同面的专属判卷：触发权从进程内存搬到数据库之后，
// "一次计划触发只有一条执行"、"错失只记一条 skipped 不补跑"、"同任务同时最多一条
// started"这三条不变量必须在真实 PostgreSQL 上成立——它们是 C-10 / C-11 的守点，
// 单元格下无法证明（需要真实的唯一索引、行锁与 CAS）。
//
// 判卷对象是 `internal/storage/postgres/schedule_coord.go` 暴露的五个仓储方法，
// 不是 `core.Scheduler` 的循环（后者的等价断言在 `internal/core/scheduler_test.go`）。
// 这样两层各守一段：本文件证明"数据库这一层挡得住"，core 侧证明"调度器按这张表
// 的正确顺序调用"。
// These cases own the database half of C-10 / C-11: unique indexes, row locks and CAS
// against real PostgreSQL; the core-side scheduling order is asserted in core.

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// workTTL 是调度用例使用的执行租约时长：远长于用例耗时，判据不靠到期制造接管。
// workTTL is far longer than these cases run, so expiry never muddies an assertion.
const workTTL = 30 * time.Second

// scheduleSetNextFire 直写 `next_fire_at`（夹具专用）：仓储的 `SetNextFire` 走 CAS，
// 造"已到期"的初态需要绕开它；生产路径不会这样写。
// scheduleSetNextFire seeds next_fire_at directly; production writes go through the CAS.
func scheduleSetNextFire(t *testing.T, db *DB, taskID string, at time.Time) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`UPDATE scheduled_tasks SET next_fire_at = $2 WHERE task_id = $1`,
		taskID, at.UnixMilli())
	require.NoError(t, err)
}

// scheduleNextFireOf 读回 `next_fire_at`（NULL → Valid=false）。
// scheduleNextFireOf reads next_fire_at back, keeping NULL distinguishable.
func scheduleNextFireOf(t *testing.T, db *DB, taskID string) (time.Time, bool) {
	t.Helper()
	var ms *int64
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT next_fire_at FROM scheduled_tasks WHERE task_id = $1`, taskID).Scan(&ms))
	if ms == nil {
		return time.Time{}, false
	}
	return time.UnixMilli(*ms).UTC(), true
}

// scheduleStartedRows 数某个任务的在途（started）执行行——C-11 的直接读数。
// scheduleStartedRows counts the task's in-flight executions, the direct C-11 reading.
func scheduleStartedRows(t *testing.T, db *DB, taskID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM task_executions WHERE task_id = $1 AND status = $2`,
		taskID, string(core.ExecutionStarted)).Scan(&n))
	return n
}

// TestScheduler_TriggerDedup_OneExecutionPerPlannedTime：同一 `(task_id, planned_at)`
// 只能有一个领取者，库里只能有一行 cron 执行（模块卡 §4.4.3 原文用例，夹具改为本包
// 的隔离库句柄）。
//
// 逆改点：删除 `ux_task_executions_cron_planned` 或把领取事务的"预检已存在"去掉，
// `okCount` 与 `executions` 都会变成 2。判据保护：两个 goroutine 真并发，`-race` 下
// 同时覆盖行锁顺序——串行调用证明不了并发下的互斥。
// Two goroutines race for the same planned instant: exactly one wins, one row exists.
func TestScheduler_TriggerDedup_OneExecutionPerPlannedTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)
	taskPortSeedTask(t, db, "task-1", "demo", "*/1 * * * * *", "巡检")

	planned := opNow.Truncate(time.Second)
	next := planned.Add(time.Second)
	scheduleSetNextFire(t, db, "task-1", planned)

	workers := []core.WorkerIdentity{
		{InstanceID: "worker-a", Name: "worker"},
		{InstanceID: "worker-b", Name: "worker"}, // 同名不同实例：Name 不参与身份（FR-24-30）
	}
	results := make([]error, len(workers))
	var wg sync.WaitGroup
	for i, owner := range workers {
		wg.Go(func() {
			_, results[i] = store.ClaimDueTrigger(ctx, "task-1", planned, next, owner, workTTL)
		})
	}
	wg.Wait()

	var okCount, claimedCount int
	for _, err := range results {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, core.ErrTriggerAlreadyClaimed):
			claimedCount++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, okCount, "同一 planned_at 只能有一个领取者")
	require.Equal(t, 1, claimedCount, "落败的一方必须得到 ErrTriggerAlreadyClaimed")

	rows := taskPortListExecutions(t, db.pool, "task-1")
	require.Len(t, rows, 1, "唯一索引必须挡住第二行")
	require.Equal(t, core.TriggerCron, rows[0].Trigger)
	require.Equal(t, planned.UnixMilli(), rows[0].PlannedAt.UnixMilli())
	require.Equal(t, core.ExecutionStarted, rows[0].Status)

	after, ok := scheduleNextFireOf(t, db, "task-1")
	require.True(t, ok)
	require.Equal(t, next.UnixMilli(), after.UnixMilli(), "领取必须在同一事务里推进 next_fire_at（CAS）")
}

// TestScheduler_MissedTriggerRecordsSkippedWithoutReplay：错过宽限只记**一条**
// skipped 并推进到未来，不逐点补跑（FR-24-12）。
//
// 逆改点：把 CAS 推进挪到插入之后或省掉，第二次调用会因 `(task_id, planned_at)`
// 唯一约束报错而不是 `ErrStaleNextFire`；改成逐点补跑则执行行数 > 1。
// One missed window yields exactly one skipped row and one forward advance — never a
// point-by-point replay.
func TestScheduler_MissedTriggerRecordsSkippedWithoutReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)
	taskPortSeedTask(t, db, "task-1", "demo", "0 */5 * * * *", "巡检")

	planned := opNow.Add(-time.Hour)
	next := opNow.Add(5 * time.Minute)
	scheduleSetNextFire(t, db, "task-1", planned)

	require.NoError(t, store.RecordMissedTrigger(ctx, "task-1", planned, next, 12,
		"missed_grace_exceeded"))

	rows := taskPortListExecutions(t, db.pool, "task-1")
	require.Len(t, rows, 1, "12 个错失点只落一行，不是 12 行")
	require.Equal(t, core.ExecutionSkipped, rows[0].Status)
	require.Equal(t, core.TriggerCron, rows[0].Trigger)
	require.Equal(t, "missed_grace_exceeded", rows[0].ErrorMessage)
	require.Empty(t, rows[0].RunID, "跳过行没有产生 Run（不自动重放）")
	require.Equal(t, planned.UnixMilli(), rows[0].PlannedAt.UnixMilli())

	after, ok := scheduleNextFireOf(t, db, "task-1")
	require.True(t, ok)
	require.Equal(t, next.UnixMilli(), after.UnixMilli(), "必须推进到未来，否则该任务永远到期")
	require.Zero(t, scheduleStartedRows(t, db, "task-1"), "错失分支不得发起任何执行")

	// 第二个 Worker 拿着同一 planned_at 再来：CAS 已失效，什么都不写。
	err := store.RecordMissedTrigger(ctx, "task-1", planned, next.Add(5*time.Minute), 1,
		"missed_grace_exceeded")
	require.ErrorIs(t, err, core.ErrStaleNextFire)
	require.Len(t, taskPortListExecutions(t, db.pool, "task-1"), 1, "重复记录不得产生第二行")
}

// TestScheduler_OverlapSkipsAndRunNowBusy：同一任务同时最多一条 started 执行
// （FR-24-13）；定时触发撞上在途执行时记一条 `task_busy` 跳过行并推进，
// 手动补跑（RunNow 走的正是 `StartExecution`）拿到既有 `ErrTaskBusy`。
//
// 逆改点：去掉 `ux_task_executions_active` → 两条 started；忙路径不推进
// next_fire_at → 该任务此后每次领取都撞同一 planned_at，永远卡死。
// At most one started execution per task; the busy cron tick is recorded and advanced,
// and the manual path gets ErrTaskBusy without writing.
func TestScheduler_OverlapSkipsAndRunNowBusy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)
	taskPortSeedTask(t, db, "task-1", "demo", "0 */5 * * * *", "巡检")
	workerA := core.WorkerIdentity{InstanceID: "worker-a"}
	workerB := core.WorkerIdentity{InstanceID: "worker-b"}

	// 定时执行在 A 上跑着（这条也是"已有一条 started"的初态）。
	planned := opNow.Truncate(time.Second)
	next := planned.Add(5 * time.Minute)
	scheduleSetNextFire(t, db, "task-1", planned)
	claimed, err := store.ClaimDueTrigger(ctx, "task-1", planned, next, workerA, workTTL)
	require.NoError(t, err)
	// run_id 由领取事务分配，形状是公共契约（§5）：scheduler-<毫秒>-<6 位 base36>。
	require.Regexp(t, `^scheduler-\d{13}-[a-z0-9]{6}$`, claimed.RunID,
		"领取事务分配的 run_id 必须符合契约形状")

	// 下一轮到期时上一条仍在跑：领取必须报忙。
	planned2 := next
	next2 := next.Add(5 * time.Minute)
	scheduleSetNextFire(t, db, "task-1", planned2)
	_, err = store.ClaimDueTrigger(ctx, "task-1", planned2, next2, workerB, workTTL)
	require.ErrorIs(t, err, core.ErrTaskBusy, "同任务已有在途执行 → 不排队、不叠加")

	// 忙路径同样必须在**同一事务**里推进并落跳过行。
	require.NoError(t, store.RecordBusyTrigger(ctx, "task-1", planned2, next2))

	rows := taskPortListExecutions(t, db.pool, "task-1")
	require.Len(t, rows, 2, "在途行 + 一条跳过行")
	require.Equal(t, core.ExecutionStarted, rows[0].Status)
	require.Equal(t, core.TriggerCron, rows[0].Trigger)
	require.Equal(t, claimed.RunID, rows[0].RunID, "执行行必须落领取事务分配的同一个 run_id")
	require.Equal(t, core.ExecutionSkipped, rows[1].Status)
	require.Equal(t, core.TriggerCron, rows[1].Trigger, "忙冲突落的是定时触发行，不能记成 manual")
	require.Equal(t, scheduleSkipBusyReason, rows[1].ErrorMessage)
	require.Empty(t, rows[1].RunID, "跳过行没有 Run")
	require.Equal(t, 1, scheduleStartedRows(t, db, "task-1"), "C-11：同时只能有一条 started")

	after, ok := scheduleNextFireOf(t, db, "task-1")
	require.True(t, ok)
	require.Equal(t, next2.UnixMilli(), after.UnixMilli(), "忙分支不推进 → 该任务永远卡在同一 planned_at")

	// 手动补跑（RunNow 的仓储半边）：撞上同一条在途执行 → ErrTaskBusy，零写入。
	_, _, err = store.StartExecution(ctx, core.ExecutionRecord{
		TaskID: "task-1", RunID: "manual-run", Status: core.ExecutionStarted,
		Trigger: core.TriggerManual, StartedAt: opNow,
	}, workerB, workTTL)
	require.ErrorIs(t, err, core.ErrTaskBusy)
	require.Len(t, taskPortListExecutions(t, db.pool, "task-1"), 2, "被拒的手动补跑零写入")

	// 收尾之后手动补跑才放行——索引只约束"同时"。
	require.NoError(t, store.FinishExecution(ctx, claimed.ExecutionID, claimed.Lease,
		core.ExecutionCompleted, time.Second, "", opNow))
	_, _, err = store.StartExecution(ctx, core.ExecutionRecord{
		TaskID: "task-1", RunID: "manual-run", Status: core.ExecutionStarted,
		Trigger: core.TriggerManual, StartedAt: opNow,
	}, workerB, workTTL)
	require.NoError(t, err)
	require.Equal(t, 1, scheduleStartedRows(t, db, "task-1"))
}

// TestScheduleCoord_ListDispatchableAndCASAdvance：到期扫描只返回"启用、未退休、
// next_fire_at 已到或为空"的任务，空值排在最前；推进必须是行条件更新（CAS），
// 预期值不符时不写（FR-24-10 的 CAS 半边）。
//
// 逆改点：把 CAS 写成无条件 UPDATE → 两个 Worker 各自按自己算出的 next 覆盖，
// 触发点丢失；扫描漏掉 `next_fire_at IS NULL` → 迁移后的遗留任务永远不排期。
// The due scan must see NULL next_fire_at rows first; the advance must be conditional.
func TestScheduleCoord_ListDispatchableAndCASAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)

	taskPortSeedTask(t, db, "due", "demo", "0 */5 * * * *", "到期")
	taskPortSeedTask(t, db, "future", "demo", "0 */5 * * * *", "未来")
	taskPortSeedTask(t, db, "never", "demo", "0 */5 * * * *", "未排期")
	taskPortSeedTask(t, db, "off", "demo", "0 */5 * * * *", "已停用")
	taskPortSeedRetiredTask(t, db.pool, "gone", "demo")

	scheduleSetNextFire(t, db, "due", opNow.Add(-time.Minute))
	scheduleSetNextFire(t, db, "future", time.Now().Add(time.Hour))
	_, err := db.pool.ExecContext(ctx,
		`UPDATE scheduled_tasks SET enabled = false WHERE task_id = 'off'`)
	require.NoError(t, err)

	due, err := store.ListDispatchable(ctx, 32)
	require.NoError(t, err)
	ids := make([]string, 0, len(due))
	for _, d := range due {
		ids = append(ids, d.Task.TaskID)
	}
	require.Equal(t, []string{"never", "due"}, ids,
		"NULL 排最前（先初始化）、再按 next_fire_at 升序；停用 / 退休 / 未到期都不在集合里")

	// 初始化分支：无预期值 → 条件写（行仍为 NULL 时成功）。
	next := opNow.Add(5 * time.Minute)
	require.NoError(t, store.SetNextFire(ctx, "never", nil, next))
	got, ok := scheduleNextFireOf(t, db, "never")
	require.True(t, ok)
	require.Equal(t, next.UnixMilli(), got.UnixMilli())

	// CAS 分支：预期值不符 → ErrStaleNextFire 且一字不改。
	wrong := next.Add(time.Minute)
	err = store.SetNextFire(ctx, "never", &wrong, next.Add(10*time.Minute))
	require.ErrorIs(t, err, core.ErrStaleNextFire)
	got, _ = scheduleNextFireOf(t, db, "never")
	require.Equal(t, next.UnixMilli(), got.UnixMilli(), "CAS 失败不得改动 next_fire_at")

	// CAS 分支：预期值相符 → 推进。
	advance := next.Add(10 * time.Minute)
	require.NoError(t, store.SetNextFire(ctx, "never", &next, advance))
	got, _ = scheduleNextFireOf(t, db, "never")
	require.Equal(t, advance.UnixMilli(), got.UnixMilli())

	// 未知任务：初始化写必须如实报"不存在"，不得静默成功。
	require.ErrorIs(t, store.SetNextFire(ctx, "ghost", nil, next), fs.ErrNotExist)
}

// TestScheduleCoord_InitDoesNotClobberConcurrentArm：初始化（next_fire_at 从 NULL 写成
// 非空）必须是**行条件写**——扫描到 NULL 只是快照，不是写权。若在读取与写入之间外部
// （管理面重写 / 夹具武装）已把行改成"已到期"，迟到的初始化必须放弃（ErrStaleNextFire）
// 并一字不改；无条件 UPDATE 会把外部刚写下的触发覆盖成 cron 算出的未来值，触发被静默
// 吞掉（US-25C 收尾实测：一整圈 60s 无执行行、无任何 WARN/ERROR）。
//
// 逆改点：把初始化写回无条件 UPDATE → 本用例红（armed 被覆盖成 future）。
// The NULL-initialization is a conditional write: a late init loses to an external
// writer with ErrStaleNextFire instead of clobbering the armed trigger.
func TestScheduleCoord_InitDoesNotClobberConcurrentArm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)

	taskPortSeedTask(t, db, "race", "demo", "0 0 0 1 1 *", "并发武装")
	due, err := store.ListDispatchable(ctx, 32)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.True(t, due[0].NextFireAt.IsZero(), "初态必须是扫描可见的 NULL 行")

	// 扫描之后、初始化之前：外部把该行改成"已到期"（等价于夹具武装 / 管理面重写）。
	armed := opNow.Add(-time.Minute)
	scheduleSetNextFire(t, db, "race", armed)

	// 迟到的初始化：必须放弃且一字不改。
	future := opNow.Add(5 * time.Minute)
	err = store.SetNextFire(ctx, "race", nil, future)
	require.ErrorIs(t, err, core.ErrStaleNextFire)
	got, ok := scheduleNextFireOf(t, db, "race")
	require.True(t, ok)
	require.Equal(t, armed.UnixMilli(), got.UnixMilli(), "迟到的初始化不得覆盖外部刚写下的触发")
}

// TestScheduler_ClaimRejectsTaskDisabledAfterScan 判领取对**最新已提交启停状态**的复查
// （review ② 复现项 3 / FR-24-10）：到期扫描（`ListDispatchable`）先读到 enabled 行，
// 随后管理员把任务停用并提交；领取事务必须在自己锁住任务行之后重读 `enabled`，停用即
// 拒绝——**不得**落下 started 执行行与租约。review 现场读数：停用提交后领取仍成功，
// `startedRows=1`。
//
// 手动补跑（`RunNow` 走的正是 `StartExecution`）语义**不变**：它是管理员的显式动作，
// 停用不回滚已发起的补跑，本用例把它一并钉住，防止修复顺带改变手动路径。
//
// 逆改点：删掉 `ClaimDueTrigger` 在 `lockTaskRow` 之后的 `Enabled` 复查 → 领取成功，
// 执行行与租约被写入，断言红。
func TestScheduler_ClaimRejectsTaskDisabledAfterScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)
	taskPortSeedTask(t, db, "task-off", "demo", "0 */5 * * * *", "巡检")
	worker := core.WorkerIdentity{InstanceID: "worker-a"}

	planned := opNow.Truncate(time.Second)
	next := planned.Add(5 * time.Minute)
	scheduleSetNextFire(t, db, "task-off", planned)

	due, err := store.ListDispatchable(ctx, 32)
	require.NoError(t, err)
	require.Len(t, due, 1, "扫描必须先把这条启用行选出来（复现的起点）")

	// 扫描之后、领取之前：停用提交。
	_, err = db.pool.ExecContext(ctx,
		`UPDATE scheduled_tasks SET enabled = false WHERE task_id = 'task-off'`)
	require.NoError(t, err)

	_, err = store.ClaimDueTrigger(ctx, "task-off", planned, next, worker, workTTL)
	require.ErrorIs(t, err, core.ErrTaskDisabled, "停用后领取必须被拒绝")

	require.Zero(t, scheduleStartedRows(t, db, "task-off"), "被拒的领取不得留下 started 执行行")
	require.Empty(t, taskPortListExecutions(t, db.pool, "task-off"), "被拒的领取必须零写入")
	after, ok := scheduleNextFireOf(t, db, "task-off")
	require.True(t, ok)
	require.Equal(t, planned.UnixMilli(), after.UnixMilli(), "被拒的领取不得推进 next_fire_at")

	// 手动补跑的既有语义：停用只影响定时触发，管理员显式补跑照常放行。
	_, _, err = store.StartExecution(ctx, core.ExecutionRecord{
		TaskID: "task-off", RunID: "manual-run-after-disable", Status: core.ExecutionStarted,
		Trigger: core.TriggerManual, StartedAt: opNow,
	}, worker, workTTL)
	require.NoError(t, err, "停用不得改变手动 RunNow 的既有语义")
	require.Equal(t, 1, scheduleStartedRows(t, db, "task-off"))
}
