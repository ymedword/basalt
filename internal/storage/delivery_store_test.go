package storage

// 本文件判卷 `schedule_deliveries`（US-12 R7）的仓储面：预写幂等且不覆盖终态、终态只
// 从 pending 回填一次、未知 run_id 报 `fs.ErrNotExist`、回执 code 的 NULL 与 0 可区分、
// 崩溃恢复按关联 Run 的实际状态改判（已完成 → unknown，其余 → skipped）。
// 恢复**不重发**是本表的立身之本：unknown 是"无法证明"，不是"失败"。
// These tests pin the authoritative delivery table: one terminal write, a NULL receipt
// distinct from zero, and a recovery that never resends.

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// deliveryTaskID 是投递用例共用的任务标识（`schedule_deliveries.task_id` 有外键）。
const deliveryTaskID = "task-deliver"

// newDeliveryStore 给每个用例一个独立的真库，并建一行被投递的任务。
func newDeliveryStore(t *testing.T) (*DeliveryStore, *DB) {
	t.Helper()
	db := openTempDB(t)
	seedTask(t, db, deliveryTaskID, "assistant", "0 8 * * * *", "每日汇总")
	return NewDeliveryStore(db), db
}

// prepareDelivery 预写一条 pending 投递行。
func prepareDelivery(t *testing.T, store *DeliveryStore, runID string, createdAt time.Time) {
	t.Helper()
	require.NoError(t, store.Prepare(context.Background(), core.ScheduleDeliveryRecord{
		RunID:       runID,
		TaskID:      deliveryTaskID,
		Agent:       "assistant",
		AgentUID:    core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"),
		ChannelUID:  core.ResourceUID("rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"),
		ChannelName: "alerts",
		CreatedAt:   createdAt,
	}))
}

// TestDeliveryStore_PrepareIsIdempotentAndStaysPending —— 同一 run_id 重复预写是空操作：
// 不产生第二行，也不覆盖既有行的名称快照；已回填终态后再预写同样不改终态。
// 逆改点：Prepare 改成 upsert → 重试把 sent 打回 pending，恢复会再投一次，本用例红。
func TestDeliveryStore_PrepareIsIdempotentAndStaysPending(t *testing.T) {
	store, _ := newDeliveryStore(t)
	ctx := context.Background()
	prepareDelivery(t, store, "run-1", policyNow)

	// 第二次预写换一份名称快照：既有行必须原样保留。
	require.NoError(t, store.Prepare(ctx, core.ScheduleDeliveryRecord{
		RunID:       "run-1",
		TaskID:      deliveryTaskID,
		Agent:       "另一个名字",
		ChannelName: "另一个渠道",
		CreatedAt:   policyNow.Add(time.Minute),
	}))

	rec, err := store.Get(ctx, "run-1")
	require.NoError(t, err)
	require.Equal(t, core.DeliveryPending, rec.Status)
	require.Equal(t, "assistant", rec.Agent, "既有名称快照不得被覆盖")
	require.Equal(t, "alerts", rec.ChannelName)
	require.True(t, rec.CreatedAt.Equal(policyNow))
	require.True(t, rec.FinishedAt.IsZero())

	require.NoError(t, store.Finish(ctx, "run-1", core.DeliverySent, 200, nil, "发送成功", policyNow, policyNow))
	prepareDelivery(t, store, "run-1", policyNow.Add(2*time.Minute))
	after, err := store.Get(ctx, "run-1")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySent, after.Status, "已回填的终态不得被打回 pending")
}

// TestDeliveryStore_FinishWritesTerminalOnce —— 终态只从 pending 回填一次：状态、HTTP
// 状态、回执 code（0 是飞书成功）、说明与两个时刻都落库；重复回填不改既有终态，未知
// run_id 报 `fs.ErrNotExist`。
// 逆改点：去掉 `AND status = 'pending'` → 迟到回执覆盖既有结论，本用例红。
func TestDeliveryStore_FinishWritesTerminalOnce(t *testing.T) {
	store, _ := newDeliveryStore(t)
	ctx := context.Background()
	prepareDelivery(t, store, "run-2", policyNow)

	zero := 0
	attempted := policyNow.Add(30 * time.Second)
	finished := policyNow.Add(45 * time.Second)
	require.NoError(t, store.Finish(ctx, "run-2", core.DeliverySent, 200, &zero, "发送成功",
		attempted, finished))

	rec, err := store.Get(ctx, "run-2")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySent, rec.Status)
	require.Equal(t, 200, rec.HTTPStatus)
	require.NotNil(t, rec.ReceiptCode)
	require.Equal(t, 0, *rec.ReceiptCode, "回执 0 是成功，不得被当成未取得")
	require.Equal(t, "发送成功", rec.Detail)
	require.True(t, rec.AttemptedAt.Equal(attempted))
	require.True(t, rec.FinishedAt.Equal(finished))

	// 重复回填：幂等空操作，既有终态一字不动。
	require.NoError(t, store.Finish(ctx, "run-2", core.DeliveryFailed, 500, nil, "迟到的失败",
		finished, finished))
	again, err := store.Get(ctx, "run-2")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySent, again.Status)
	require.Equal(t, 200, again.HTTPStatus)
	require.Equal(t, "发送成功", again.Detail)

	// 未知 run_id：0 行影响且行不存在 → 如实报不存在（不是静默成功）。
	err = store.Finish(ctx, "run-unknown", core.DeliverySent, 200, nil, "", policyNow, policyNow)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestDeliveryStore_NullReceiptStaysDistinctFromZero —— 未取得回执写 NULL，读回是
// nil 指针；未取得 HTTP 状态写 NULL，读回 0；两个时刻为零值时读回零值时间。
// 逆改点：NULL 与 0 用同一列类型混存 → 页面把"未取得"显示成"0"，本用例红。
func TestDeliveryStore_NullReceiptStaysDistinctFromZero(t *testing.T) {
	store, _ := newDeliveryStore(t)
	ctx := context.Background()
	prepareDelivery(t, store, "run-3", policyNow)
	require.NoError(t, store.Finish(ctx, "run-3", core.DeliveryUnknown, 0, nil,
		"投递请求超时，结果未知：https://hooks.example.test", time.Time{}, time.Time{}))

	rec, err := store.Get(ctx, "run-3")
	require.NoError(t, err)
	require.Equal(t, core.DeliveryUnknown, rec.Status)
	require.Equal(t, 0, rec.HTTPStatus)
	require.Nil(t, rec.ReceiptCode, "未取得回执必须与 0 区分")
	require.True(t, rec.AttemptedAt.IsZero())
	require.True(t, rec.FinishedAt.IsZero())
	require.Contains(t, rec.Detail, "https://hooks.example.test")

	var nullReceipt sql.NullInt64
	require.NoError(t, store.db.read.QueryRowContext(ctx,
		`SELECT receipt_code FROM schedule_deliveries WHERE run_id = ?`, "run-3").Scan(&nullReceipt))
	require.False(t, nullReceipt.Valid, "库里必须是 NULL，不是 0")
}

// TestDeliveryStore_ListByTaskNewestFirstWithLimit —— 按创建时间新→旧、最多 limit 条；
// 没有记录的 task 返回空（不是错误）。
func TestDeliveryStore_ListByTaskNewestFirstWithLimit(t *testing.T) {
	store, _ := newDeliveryStore(t)
	ctx := context.Background()
	prepareDelivery(t, store, "run-old", policyNow.Add(-time.Hour))
	prepareDelivery(t, store, "run-mid", policyNow.Add(-time.Minute))
	prepareDelivery(t, store, "run-new", policyNow)

	listed, err := store.ListByTask(ctx, deliveryTaskID, 2)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, []string{"run-new", "run-mid"}, []string{listed[0].RunID, listed[1].RunID})

	all, err := store.ListByTask(ctx, deliveryTaskID, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "run-old", all[2].RunID)

	none, err := store.ListByTask(ctx, "task-without-deliveries", 5)
	require.NoError(t, err)
	require.Empty(t, none)
}

// TestDeliveryStore_GetMissingReportsNotExist —— 读取不存在的投递行报 `fs.ErrNotExist`
// （而不是零值记录），页面据此区分"没有这条"与"还没回填"。
func TestDeliveryStore_GetMissingReportsNotExist(t *testing.T) {
	store, _ := newDeliveryStore(t)
	_, err := store.Get(context.Background(), "run-missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestDeliveryStore_RecoverResolvesFromRunState —— 崩溃遗留的 pending 行按关联 Run 的
// 实际终态改判：已完成 / 达上限 → unknown（模型已产出正文，投递是否发生无法证明），
// 失败 / 仍运行 / 无 Run → skipped；已回填的行不动；再跑一次恢复 0 条。
// 逆改点：unknown 被写成 failed 并重发 → 重复投递，本用例红。
func TestDeliveryStore_RecoverResolvesFromRunState(t *testing.T) {
	store, db := newDeliveryStore(t)
	ctx := context.Background()

	for _, runID := range []string{
		"run-completed", "run-limit", "run-failed",
		"run-running", "run-orphan", "run-sent",
	} {
		prepareDelivery(t, store, runID, policyNow)
	}
	seedRunRow(t, db.write, "run-completed", string(core.StatusCompleted), sql.NullString{}, nil, nil)
	seedRunRow(t, db.write, "run-limit", string(core.StatusLimitReached), sql.NullString{}, nil, nil)
	seedRunRow(t, db.write, "run-failed", string(core.StatusFailed), sql.NullString{}, nil, nil)
	seedRunRow(t, db.write, "run-running", string(core.StatusRunning), sql.NullString{}, nil, nil)
	// run-orphan 故意没有 runs 行：pending 先于 Run 准入写出，这是合法状态。
	require.NoError(t, store.Finish(ctx, "run-sent", core.DeliverySent, 200, nil, "发送成功",
		policyNow, policyNow))

	now := policyNow.Add(time.Hour)
	recovered, err := store.Recover(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 5, recovered, "只改判当次仍为 pending 的行")

	for runID, want := range map[string]core.DeliveryStatus{
		"run-completed": core.DeliveryUnknown,
		"run-limit":     core.DeliveryUnknown,
		"run-failed":    core.DeliverySkipped,
		"run-running":   core.DeliverySkipped,
		"run-orphan":    core.DeliverySkipped,
	} {
		rec, getErr := store.Get(ctx, runID)
		require.NoError(t, getErr)
		require.Equal(t, want, rec.Status, "%s 的恢复结论", runID)
		require.True(t, rec.FinishedAt.Equal(now), "%s 的收尾时间取恢复时刻", runID)
	}
	unknown, err := store.Get(ctx, "run-completed")
	require.NoError(t, err)
	require.Equal(t, "进程在投递完成前退出，投递结果未知", unknown.Detail)
	skipped, err := store.Get(ctx, "run-failed")
	require.NoError(t, err)
	require.Equal(t, "运行未完成，未投递", skipped.Detail)

	sent, err := store.Get(ctx, "run-sent")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySent, sent.Status, "已回填的行不得被恢复改动")
	require.Equal(t, "发送成功", sent.Detail)

	again, err := store.Recover(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 0, again, "恢复是幂等的")
}

// TestDeliveryStore_RecoverWithoutPendingIsNoop —— 没有 pending 行时恢复返回 0 且不改
// 任何行。
func TestDeliveryStore_RecoverWithoutPendingIsNoop(t *testing.T) {
	store, _ := newDeliveryStore(t)
	recovered, err := store.Recover(context.Background(), policyNow)
	require.NoError(t, err)
	require.Equal(t, 0, recovered)
}

// TestDeliveryStore_ErrorPathsReportContext —— 池故障带上下文前缀返回；读取故障不得被
// 误判成"不存在"（否则页面会显示没有记录）。
func TestDeliveryStore_ErrorPathsReportContext(t *testing.T) {
	ctx := context.Background()

	t.Run("read", func(t *testing.T) {
		store, db := newDeliveryStore(t)
		closeReadPool(t, db)
		_, err := store.Get(ctx, "run-1")
		require.Error(t, err)
		require.False(t, errors.Is(err, fs.ErrNotExist), "池故障不得伪装成记录不存在")
		require.Contains(t, err.Error(), "读取定时投递记录")
		_, err = store.ListByTask(ctx, deliveryTaskID, 5)
		require.Error(t, err)
		require.Contains(t, err.Error(), "列出定时投递记录")
	})

	t.Run("write", func(t *testing.T) {
		store, db := newDeliveryStore(t)
		closeWritePool(t, db)
		err := store.Prepare(ctx, core.ScheduleDeliveryRecord{
			RunID:     "run-x",
			TaskID:    deliveryTaskID,
			Agent:     "assistant",
			CreatedAt: policyNow,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "预写定时投递记录")

		err = store.Finish(ctx, "run-x", core.DeliverySent, 200, nil, "", policyNow, policyNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "回填定时投递记录")

		_, err = store.Recover(ctx, policyNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "获取写连接")
	})
}
