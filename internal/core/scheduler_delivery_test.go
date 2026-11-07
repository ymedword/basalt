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
	recoverCalls int
	outcome      DeliveryOutcome
	onPrepare    func()
	// onDeliver 在发送进入锁之前执行：用例据此做"发送中"的事件记录或阻塞，
	// 不占住替身自己的互斥锁。
	onDeliver func()

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
	if d.onDeliver != nil {
		d.onDeliver()
	}
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

func (d *fakeSchedulerDelivery) Recover(_ context.Context, _ time.Time, _ string) (int, error) {
	d.mu.Lock()
	d.recoverCalls++
	d.mu.Unlock()
	return d.recoverCount, d.recoverErr
}

// recoverCallCount 读回 Recover 被调用的次数（US-24：启动路径必须为零）。
func (d *fakeSchedulerDelivery) recoverCallCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.recoverCalls
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

// TestScheduler_Start_DoesNotRecoverDeliveriesGlobally —— US-24 起**启动不做全局恢复**
// （FR-24-02 / FR-24-21）：投递 pending 的改判由回收器在**被领取的 Run 作用域**内完成，
// 启动路径一次都不碰投递表。
//
// 逆改点：把 `Recover` 的全局调用加回 `Start`，`recoverCallCount()` 立刻从 0 变成 1；
// 恢复失败也不再是致命的——因为启动根本不等待它。
// Startup must not scan pending deliveries globally; scope-based settlement belongs to
// the reclaimer inside a claimed run.
func TestScheduler_Start_DoesNotRecoverDeliveriesGlobally(t *testing.T) {
	deprecated := []struct {
		name    string
		deliver *fakeSchedulerDelivery
	}{
		{name: "recover error", deliver: &fakeSchedulerDelivery{recoverErr: errors.New("database is locked")}},
		{name: "recover would return rows", deliver: &fakeSchedulerDelivery{recoverCount: 3}},
	}
	for _, tc := range deprecated {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, tc.deliver, nil)
			fixture.start(t)

			require.Zero(t, tc.deliver.recoverCallCount(), "启动不得做全局投递恢复")
			require.False(t, fixture.logs.hasRecord(slog.LevelInfo, "投递恢复完成"),
				"US-24 起启动不再产出全局恢复计数日志")
		})
	}
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

// ===== 调度器侧收尾末笔（US-24 五步收尾的第 4、5 步）=====

// fakeRunCloseout 是 RunCloseout 的记录替身：Lookup 按 run_id 造出本进程持有的租约
// 凭证，Complete 记录调用；onClose 在返回前执行，用例据此把收尾放进共享事件序列。
type fakeRunCloseout struct {
	mu        sync.Mutex
	lookupErr error
	// lookupEpoch 覆盖回查租约的代次（0 = 准入代次 1）。用它构造"被回收器以
	// 更高代次领回"的既成事实。
	// lookupEpoch overrides the looked-up epoch (0 means the admission epoch 1).
	lookupEpoch int64
	lookupIDs   []string
	closeIDs    []string
	onClose     func()
}

func (c *fakeRunCloseout) LookupAdmittedRun(_ context.Context, runID string) (LeaseToken, error) {
	c.mu.Lock()
	c.lookupIDs = append(c.lookupIDs, runID)
	lookupErr := c.lookupErr
	c.mu.Unlock()
	if lookupErr != nil {
		return LeaseToken{}, lookupErr
	}
	epoch := c.lookupEpoch
	if epoch == 0 {
		epoch = 1
	}
	return LeaseToken{
		Kind: LeaseRun, ResourceID: runID,
		OwnerWorkerID: schedWorker.InstanceID, OwnershipEpoch: epoch,
	}, nil
}

func (c *fakeRunCloseout) CompleteRunCloseout(_ context.Context, runID string,
	_ LeaseToken, _ time.Time,
) error {
	c.mu.Lock()
	c.closeIDs = append(c.closeIDs, runID)
	c.mu.Unlock()
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

// lookupCount / closeCount 读回计数（并发安全）。
func (c *fakeRunCloseout) lookupCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lookupIDs)
}

func (c *fakeRunCloseout) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.closeIDs)
}

// renewRecordingLeases 接受任意 token 并记录续约：用来证明结算期间 run 租约仍在续。
type renewRecordingLeases struct {
	mu     sync.Mutex
	renews []LeaseToken
}

func (l *renewRecordingLeases) Acquire(_ context.Context, kind LeaseKind, resourceID string,
	owner WorkerIdentity, ttl time.Duration,
) (Lease, error) {
	token := LeaseToken{
		Kind: kind, ResourceID: resourceID,
		OwnerWorkerID: owner.InstanceID, OwnershipEpoch: 1,
	}
	now := time.Now()
	return Lease{LeaseToken: token, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(ttl)}, nil
}

func (l *renewRecordingLeases) Renew(_ context.Context, token LeaseToken, ttl time.Duration) (Lease, error) {
	l.mu.Lock()
	l.renews = append(l.renews, token)
	l.mu.Unlock()
	now := time.Now()
	return Lease{LeaseToken: token, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(ttl)}, nil
}

func (l *renewRecordingLeases) Release(_ context.Context, _ LeaseToken) error { return nil }

func (l *renewRecordingLeases) FencedTx(context.Context, LeaseToken,
	func(context.Context, Tx) error,
) error {
	return ErrLeaseFenceUnavailable
}

func (l *renewRecordingLeases) renewCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.renews)
}

// TestScheduler_DeliveredRunClosesOutAfterDelivery —— 系统代投的 Run 由调度器补齐
// 五步收尾的第 5 步，且次序固定：投递结算 → CompleteRunCloseout。投递结果落库前
// 交还租约，B1 就会把**在途**发送改判成 unknown。
//
// 逆改点：把 settleAndClose 换回 settleDelivery（不补末笔），closeIDs 立刻为空。
// The scheduler must settle the delivery first and complete the caller-side closeout
// afterwards.
func TestScheduler_DeliveredRunClosesOutAfterDelivery(t *testing.T) {
	var eventsMu sync.Mutex
	var events []string
	record := func(event string) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, event)
	}
	delivery := &fakeSchedulerDelivery{onDeliver: func() { record("deliver") }}
	closeout := &fakeRunCloseout{onClose: func() { record("closeout") }}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery,
		func(o *SchedulerOptions) {
			o.Closeout = closeout
			o.Leases = &renewRecordingLeases{}
		})
	fixture.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
		return RunResult{RunID: req.RunID, Status: StatusCompleted, FinalText: "巡检完成"}, nil
	}
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	_, delivers, skips := delivery.snapshot()
	require.Len(t, delivers, 1)
	require.Empty(t, skips)
	require.Equal(t, []string{delivers[0].RunID}, closeout.lookupIDs,
		"收尾末笔必须按本次 Run 的 run_id 取回租约凭证")
	require.Equal(t, []string{delivers[0].RunID}, closeout.closeIDs)
	require.Equal(t, []string{"deliver", "closeout"}, events,
		"次序固定：投递结果先落库，收尾末笔在其后")
}

// TestScheduler_DeliveredRunRenewsRunLeaseDuringSend —— Run 返回时服务侧 keeper 已经
// 只停不续；结算期间必须由调度器续约。否则一次超过 TTL 的发送会让租约自然过期，
// 回收器把在途投递改判成 unknown，与「不重发」的结论混在一起。
//
// 逆改点：去掉 settleAndClose 里的 KeepLease，续约次数为 0。
// The scheduler keeps the run lease alive while the send is in flight.
func TestScheduler_DeliveredRunRenewsRunLeaseDuringSend(t *testing.T) {
	delivery := &fakeSchedulerDelivery{onDeliver: func() { time.Sleep(120 * time.Millisecond) }}
	closeout := &fakeRunCloseout{}
	leases := &renewRecordingLeases{}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery,
		func(o *SchedulerOptions) {
			o.Closeout = closeout
			o.Leases = leases
			o.LeaseTTL = 200 * time.Millisecond
			o.RenewInterval = 20 * time.Millisecond
		})
	fixture.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
		return RunResult{RunID: req.RunID, Status: StatusCompleted, FinalText: "巡检完成"}, nil
	}
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	require.Equal(t, 1, closeout.closeCount())
	require.GreaterOrEqual(t, leases.renewCount(), 2,
		"结算期间必须持续续约 run 租约，租约不得在投递落库前过期")
}

// TestScheduler_DeliveredRunLookupFailureStillSettlesDelivery —— Run 没有建成（准入
// 被拒）时回查返回 ErrRunNotFound：投递仍要如实结算（本轮标 skipped），收尾不补；
// 残余 pending 留给 B2 按「无 Run 的前置投递」收成 skipped。
// A missing run row still settles the delivery; the closeout is not attempted.
func TestScheduler_DeliveredRunLookupFailureStillSettlesDelivery(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	closeout := &fakeRunCloseout{lookupErr: ErrRunNotFound}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery,
		func(o *SchedulerOptions) {
			o.Closeout = closeout
			o.Leases = &renewRecordingLeases{}
		})
	fixture.invoker.run = func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, errors.New("准入被拒")
	}
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	_, delivers, skips := delivery.snapshot()
	require.Empty(t, delivers)
	require.Equal(t, []string{"运行未成功，未投递"}, skips)
	require.Equal(t, 1, closeout.lookupCount())
	require.Zero(t, closeout.closeCount(), "没有 Run 行就没有收尾末笔")
}

// TestScheduler_DeliveredRunReclaimedAtHigherEpochSkipsSettlement —— 回查到的 run
// 租约虽然仍属**本 Worker 实例**，但代次已不是准入时的 1：本进程的回收器在投递结算
// 之前领回过这条租约（回收器与调度器共用同一 Worker 身份，领取语句不排除自身），
// Run 的终态与投递已由回收作用域裁决。此时派发投递或补末笔都会顶替该裁决，两项一律
// 不做。逆改点：把 `token.OwnershipEpoch != 1` 从守卫里去掉，本用例立刻红——
// 投递被发出、末笔被补写。
// A lease re-claimed at a higher epoch (possibly by this process's own reclaimer) is
// no longer ours to settle, even though the owner id still matches.
func TestScheduler_DeliveredRunReclaimedAtHigherEpochSkipsSettlement(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	closeout := &fakeRunCloseout{lookupEpoch: 2}
	fixture := newDeliverySchedFixture(t, []Schedule{deliverySchedTask()}, delivery,
		func(o *SchedulerOptions) {
			o.Closeout = closeout
			o.Leases = &renewRecordingLeases{}
		})
	fixture.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
		return RunResult{RunID: req.RunID, Status: StatusCompleted, FinalText: "巡检完成"}, nil
	}
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	_, delivers, skips := delivery.snapshot()
	require.Empty(t, delivers, "代次已前进的 Run 不得再发投递")
	require.Empty(t, skips, "也不得再改写投递行（回收作用域已裁决）")
	require.Equal(t, 1, closeout.lookupCount(), "守卫必须以回查到的租约为判据")
	require.Zero(t, closeout.closeCount(), "代次已前进的 Run 不补收尾末笔")
}

// TestScheduler_NoDeliveryTargetSkipsCallerCloseout —— 未配置代投的 Run 不设
// SuppressNotify，五步收尾在服务侧已经走完；调度器不得再补一次收尾。
// A run without a delivery target is closed out by the service, not the scheduler.
func TestScheduler_NoDeliveryTargetSkipsCallerCloseout(t *testing.T) {
	delivery := &fakeSchedulerDelivery{}
	closeout := &fakeRunCloseout{}
	fixture := newDeliverySchedFixture(t, []Schedule{demoTask(schedTaskID)}, delivery,
		func(o *SchedulerOptions) {
			o.Closeout = closeout
			o.Leases = &renewRecordingLeases{}
		})
	fixture.invoker.run = func(_ context.Context, req RunRequest) (RunResult, error) {
		require.False(t, req.SuppressNotify, "未代投的 Run 不抑制 notify")
		return RunResult{RunID: req.RunID, Status: StatusCompleted, FinalText: "完成"}, nil
	}
	fixture.start(t)
	fixture.fire(t, schedTaskID)

	require.Zero(t, closeout.lookupCount(), "服务侧收尾的 Run 不得被调度器重复收尾")
	require.Zero(t, closeout.closeCount())
}
