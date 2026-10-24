package core

// 本文件覆盖定时任务**系统代投**在调度链里的位置（US-12 R7）：运行前预写并复核渠道、
// 预写失败不发起运行、运行成功才投递、未成功标跳过、panic 收尾、启动恢复，以及投递
// 使用独立时限的 ctx。发送实现本身由 app / tool 包用例覆盖，这里只验调度器的接线。
// This file covers the scheduler's delivery wiring: prepare before the run, abort on
// prepare failure, deliver only on success, skip otherwise, panic settling, startup
// recovery, and the independent delivery deadline.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 合法的资源 UID 固定字面量（与 identity 用例同一形态：rs_ + 26 位 ULID 载荷）。
const (
	deliverySchedChannel       = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	deliverySchedChannelSecond = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"
)

// deliverySchedTask 造一条带代投目标的任务定义。
func deliverySchedTask() Schedule {
	task := demoTask(schedTaskID)
	task.DeliverTo = deliverySchedChannel
	return task
}

// fakeSchedulerDelivery 是 ScheduleDelivery 的记录替身：成功路径默认返回 sent。
type fakeSchedulerDelivery struct {
	mu           sync.Mutex
	prepareErr   error
	recoverErr   error
	recoverCount int
	outcome      DeliveryOutcome
	onPrepare    func()

	prepares  []DeliveryIntent
	delivers  []DeliveryIntent
	contents  []string
	skips     []string
	skipCalls []DeliveryIntent

	deliverCtxErr      error
	deliverDeadline    time.Time
	deliverHasDeadline bool
}

func (d *fakeSchedulerDelivery) Prepare(_ context.Context, intent DeliveryIntent) error {
	if d.onPrepare != nil {
		d.onPrepare()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prepares = append(d.prepares, intent)
	return d.prepareErr
}

func (d *fakeSchedulerDelivery) Deliver(ctx context.Context, intent DeliveryIntent,
	content string,
) DeliveryOutcome {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivers = append(d.delivers, intent)
	d.contents = append(d.contents, content)
	d.deliverCtxErr = ctx.Err()
	d.deliverDeadline, d.deliverHasDeadline = ctx.Deadline()
	if d.outcome.Status == "" {
		return DeliveryOutcome{Status: DeliverySent, Detail: "测试发送成功"}
	}
	return d.outcome
}

func (d *fakeSchedulerDelivery) Skip(_ context.Context, intent DeliveryIntent, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.skipCalls = append(d.skipCalls, intent)
	d.skips = append(d.skips, reason)
	return nil
}

func (d *fakeSchedulerDelivery) Recover(_ context.Context, _ time.Time) (int, error) {
	return d.recoverCount, d.recoverErr
}

func (d *fakeSchedulerDelivery) snapshot() (prepares, delivers []DeliveryIntent, skips []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prepares = append([]DeliveryIntent(nil), d.prepares...)
	delivers = append([]DeliveryIntent(nil), d.delivers...)
	skips = append([]string(nil), d.skips...)
	return prepares, delivers, skips
}

// newDeliverySchedFixture 组装带投递端口的调度夹具；delivery 为 nil 时模拟装配缺陷。
func newDeliverySchedFixture(t *testing.T, schedules []Schedule,
	delivery *fakeSchedulerDelivery, tune func(*SchedulerOptions),
) *schedFixture {
	t.Helper()
	store := newFakeTaskStore()
	invoker := &countingInvoker{}
	engine := &fakeEngine{}
	logger, logs := newCaptureLogger()
	sched := newSchedulerWith(store, invoker, engine, logger, schedules, func(o *SchedulerOptions) {
		// 只在非 nil 时赋值：把 typed nil 装进接口会得到"非 nil 接口"，装配缺陷用例失效。
		if delivery != nil {
			o.Delivery = delivery
		}
		if tune != nil {
			tune(o)
		}
	})
	return &schedFixture{store: store, invoker: invoker, engine: engine, logs: logs, sched: sched}
}

// TestScheduler_DeliveryPreparesBeforeRun —— 运行前先预写（此时模型还没被调用），
// 预写与运行共用同一个 run_id，并把 SuppressNotify 置真。
// Prepare runs before the model call, shares the run id, and suppresses notify.
func TestScheduler_DeliveryPreparesBeforeRun(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
	delivery.onPrepare = func() {
		require.Zero(t, fixture.invoker.calls.Load(), "预写必须先于模型运行")
	}
	fixture.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
		return RunResult{RunID: req.RunID, Status: StatusCompleted, FinalText: "巡检完成"}, nil
	}
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	prepares, delivers, skips := delivery.snapshot()
	require.Len(t, prepares, 1)
	require.Equal(t, schedTaskID, prepares[0].TaskID)
	require.Equal(t, schedAgent, prepares[0].Agent)
	require.Equal(t, ResourceUID(deliverySchedChannel), prepares[0].ChannelUID)

	requests := fixture.invoker.requests()
	require.Len(t, requests, 1)
	require.Equal(t, prepares[0].RunID, requests[0].RunID, "预写行与运行必须是同一个 run_id")
	require.True(t, requests[0].SuppressNotify, "系统代投接管后不得再向模型提供 notify")

	require.Len(t, delivers, 1)
	require.Empty(t, skips)
	require.Equal(t, "【demo】定时任务 demo-1 执行完成\n\n巡检完成", delivery.contents[0])
	require.Equal(t, []ExecutionStatus{ExecutionCompleted}, fixture.store.snapshotFinished())
}

// TestScheduler_DeliveryPrepareFailureAbortsBeforeRun —— 预写 / 资格失败：零模型调用、
// 执行行收尾为 failed、不发送也不跳过（根本没有预写行）。
// A prepare failure aborts the run before the model is called.
func TestScheduler_DeliveryPrepareFailureAbortsBeforeRun(t *testing.T) {
	delivery := &fakeSchedulerDelivery{prepareErr: errors.New("渠道已撤销")}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	require.Zero(t, fixture.invoker.calls.Load(), "预写失败不得发起运行")
	require.Equal(t, []ExecutionStatus{ExecutionFailed}, fixture.store.snapshotFinished())
	_, delivers, skips := delivery.snapshot()
	require.Empty(t, delivers)
	require.Empty(t, skips)
	fixture.logs.requireRecord(t, slog.LevelError, "代投前置检查失败，本轮不发起运行")
}

// TestScheduler_DeliveryInvalidTargetUIDAborts —— 目标不是合法资源 UID（例如被写成了
// 渠道名）时同样在运行前失败关闭。
// A non-UID target fails closed before the run.
func TestScheduler_DeliveryInvalidTargetUIDAborts(t *testing.T) {
	task := demoTask(schedTaskID)
	task.DeliverTo = "feishu-ops" // 渠道名不是 UID：同名重建会让旧任务送往新机器人
	delivery := &fakeSchedulerDelivery{}
	fixture := newDeliverySchedFixture(t, []Schedule{task}, delivery, nil)
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	require.Zero(t, fixture.invoker.calls.Load())
	require.Equal(t, []ExecutionStatus{ExecutionFailed}, fixture.store.snapshotFinished())
	prepares, delivers, _ := delivery.snapshot()
	require.Empty(t, prepares, "UID 不合法时不进入预写")
	require.Empty(t, delivers)
}

// TestScheduler_DeliveryServiceMissingFailsClosed —— 任务配了代投但实例没装配投递端口：
// 装配错误按失败关闭处理，不静默漏投。
// A task with a delivery target and no delivery port fails closed.
func TestScheduler_DeliveryServiceMissingFailsClosed(t *testing.T) {
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, nil, nil)
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	require.Zero(t, fixture.invoker.calls.Load())
	require.Equal(t, []ExecutionStatus{ExecutionFailed}, fixture.store.snapshotFinished())
}

// TestScheduler_DeliverySkipsFailedRun —— 运行失败只标跳过，不发送；跳过带着同一 intent。
// A failed run is marked skipped, never delivered.
func TestScheduler_DeliverySkipsFailedRun(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
	fixture.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, errors.New("模型不可用")
	}
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	prepares, delivers, skips := delivery.snapshot()
	require.Len(t, prepares, 1)
	require.Empty(t, delivers, "运行失败不得发送")
	require.Equal(t, []string{"运行未成功，未投递"}, skips)
	require.Len(t, delivery.skipCalls, 1)
	require.Equal(t, prepares[0].RunID, delivery.skipCalls[0].RunID)
	require.Equal(t, []ExecutionStatus{ExecutionFailed}, fixture.store.snapshotFinished())
}

// TestScheduler_DeliveryPanicSettlesAsSkip —— 模型运行 panic：执行行 failed、投递行跳过，
// panic 不外溢。
// A panicking run is absorbed, and the prepared delivery row is marked skipped.
func TestScheduler_DeliveryPanicSettlesAsSkip(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
	fixture.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		panic("boom")
	}
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	_, delivers, skips := delivery.snapshot()
	require.Empty(t, delivers)
	require.Equal(t, []string{"运行未成功，未投递"}, skips)
	require.Equal(t, []ExecutionStatus{ExecutionFailed}, fixture.store.snapshotFinished())
	fixture.logs.requireRecord(t, slog.LevelError, "调度运行 panic")
}

// TestScheduler_DeliveryNoTargetKeepsNotify —— 未配置目标的任务保持原行为：不预写、
// 不发送、不抑制 notify。
// A task without a delivery target leaves notify untouched.
func TestScheduler_DeliveryNoTargetKeepsNotify(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	fixture := newDeliverySchedFixture(t, []Schedule{demoTask(schedTaskID)}, delivery, nil)
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	prepares, delivers, skips := delivery.snapshot()
	require.Empty(t, prepares)
	require.Empty(t, delivers)
	require.Empty(t, skips)
	requests := fixture.invoker.requests()
	require.Len(t, requests, 1)
	require.False(t, requests[0].SuppressNotify)
}

// TestScheduler_DeliveryUnknownIsLoggedNotResent —— 结果未知照实记录并记 ERROR，
// 调度器**不**自动重发。
// An unknown outcome is logged and never resent.
func TestScheduler_DeliveryUnknownIsLoggedNotResent(t *testing.T) {
	delivery := &fakeSchedulerDelivery{outcome: DeliveryOutcome{
		Status: DeliveryUnknown, Detail: "投递请求超时，结果未知：https://hooks.example.test",
	}}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	_, delivers, skips := delivery.snapshot()
	require.Len(t, delivers, 1, "未知结果只发一次")
	require.Empty(t, skips)
	rec := fixture.logs.requireRecord(t, slog.LevelError, "系统代投结果未知（不自动重发）")
	status, ok := attrOf(rec, "delivery_status")
	require.True(t, ok)
	require.Equal(t, string(DeliveryUnknown), status.String())
}

// TestScheduler_DeliveryUsesIndependentDeadline —— 运行 ctx 被取消后，发送仍用
// context.WithoutCancel 派生的约 15s ctx：运行截止不拖垮已经完成的投递。
// The delivery send keeps its own ~15s deadline after the run context is canceled.
func TestScheduler_DeliveryUsesIndependentDeadline(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.invoker.run = func(ctx context.Context, req RunRequest) (RunResult, error) {
		cancel() // 模拟调用方在运行结束时取消
		return RunResult{RunID: req.RunID, Status: StatusCompleted, FinalText: "ok"}, nil
	}
	fixture.start(t)

	_, _, err := fixture.sched.RunNow(ctx, schedTaskID)
	require.NoError(t, err)

	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	require.Len(t, delivery.delivers, 1)
	require.NoError(t, delivery.deliverCtxErr, "投递 ctx 不得已取消")
	require.True(t, delivery.deliverHasDeadline, "投递 ctx 必须带截止时间")
	require.WithinDuration(t, time.Now().Add(schedulerDeliveryTimeout),
		delivery.deliverDeadline, 2*time.Second)
}

// TestScheduler_DeliveryRecoverAtStart —— 启动时恢复遗留 pending 行；恢复失败是致命的，
// 调度器不得带着未恢复的投递行开始接单。
// Startup recovery runs before serving, and its failure is fatal.
func TestScheduler_DeliveryRecoverAtStart(t *testing.T) {
	t.Run("failure is fatal", func(t *testing.T) {
		delivery := &fakeSchedulerDelivery{recoverErr: errors.New("database is locked")}
		fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
		err := fixture.sched.Start(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "recover schedule deliveries")
	})

	t.Run("count is logged", func(t *testing.T) {
		delivery := &fakeSchedulerDelivery{recoverCount: 3}
		fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
		fixture.start(t)
		rec := fixture.logs.requireRecord(t, slog.LevelInfo, "投递恢复完成")
		count, ok := attrOf(rec, "recovered")
		require.True(t, ok)
		require.Equal(t, "3", count.String())
	})
}

// TestScheduler_ReconcileKeepsDeliverTo —— 对账把定义里的 deliver_to 写入库行：它是
// 任务定义的一部分，漏写会让每次启动 / 热加载后目标被清空，任务照跑却不再投递。
// Reconcile persists deliver_to; dropping it would silently disable delivery on every
// startup or file reload.
func TestScheduler_ReconcileKeepsDeliverTo(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery, nil)
	fixture.start(t)

	task, ok := fixture.store.tasks[schedTaskID]
	require.True(t, ok)
	require.Equal(t, deliverySchedChannel, task.DeliverTo)

	// 热加载改成另一个目标后，库行随之更新（不是只更新 cron / message）。
	updated := deliverySchedTask()
	updated.DeliverTo = deliverySchedChannelSecond
	require.NoError(t, fixture.sched.ReconcileAgent(context.Background(), schedAgent,
		[]Schedule{updated}))
	task, ok = fixture.store.tasks[schedTaskID]
	require.True(t, ok)
	require.Equal(t, deliverySchedChannelSecond, task.DeliverTo)
}
