//go:build integration

package postgres

// 本文件是投递记录（`schedule_deliveries`，US-12 R7）与通知冻结 / 投递记录（R4.8 /
// T116）两处仓储在 PostgreSQL 谱系上的包内判卷：预写幂等且不覆盖终态、终态只从 pending
// 回填一次、未知 run_id 报 `fs.ErrNotExist`、回执 code 的 NULL 与 0 可区分、崩溃恢复按
// 关联 Run 的实际状态改判；通知侧一次调用至多一条投递记录、渠道集合准入冻结、终态释放。
//
// 由 SQLite 谱系 `internal/storage/delivery_store_test.go` 与
// `internal/storage/notify_delivery_store_test.go` 逐条移植：测试函数名、断言、中文
// 验收点注释与逆改点注释原样保留；只替换夹具（`openTempDB` → `portTestDB`）、读写句柄
// 合一（`db.read` / `db.write` → `db.pool`）与方言（`?` → `$n`、时间列 BIGINT Unix 毫秒）。
// 直插 `runs` / `scheduled_tasks` 行另补 PG 新增的归属 NOT NULL 列（caller_id /
// authorizing_kind / actor）。
// These are the PostgreSQL ports of the schedule-delivery and notify-delivery store cases;
// function names and assertions stay verbatim, only the fixture and dialect differ.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// deliveryTaskID 是投递用例共用的任务标识（`schedule_deliveries.task_id` 有外键）。
const deliveryTaskID = "task-deliver"

// deliveryPortPolicyNow 是投递用例的固定写库时刻（与 SQLite 侧 `policyNow` 同一时刻，
// 断言可复算）。
// deliveryPortPolicyNow is the frozen instant of the delivery cases.
var deliveryPortPolicyNow = time.Date(2026, 10, 24, 8, 30, 0, 0, time.UTC)

// notifyPortFixedNow 是通知用例的"当前时刻"（与 SQLite 侧 `fixedNow` 同一时刻）。
// notifyPortFixedNow is the frozen instant of the notify cases.
var notifyPortFixedNow = time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)

// notifyPortFixedStart 是通知准入用例的起始时刻（与 SQLite 侧 `fixedStart` 同一时刻）。
// notifyPortFixedStart is the frozen start instant of the notify admission case.
var notifyPortFixedStart = time.Date(2026, 10, 16, 8, 30, 0, 123*int(time.Millisecond), time.UTC)

// notifyPortSessionID 是通知用例的会话标识（与 SQLite 侧 `recoverySession` 同值）。
const notifyPortSessionID = "s1"

// ---- 投递记录夹具 ------------------------------------------------------------

// deliveryPortNewStore 给每个用例一个已迁移的隔离库，并建一行被投递的任务。
// PG 的 `scheduled_tasks` 带归属 NOT NULL 列，直插行与调度器定义对齐路径同值。
// deliveryPortNewStore returns one isolated migrated handle with the delivered task seeded.
func deliveryPortNewStore(t *testing.T) (*DeliveryStore, *DB) {
	t.Helper()
	db := portTestDB(t)
	deliveryPortSeedTask(t, db, deliveryTaskID, "assistant", "0 8 * * * *", "每日汇总")
	return NewDeliveryStore(db), db
}

// deliveryPortSeedTask 直插一条 active 任务行（时间取 opNow——判据不依赖实现自取时钟）。
// deliveryPortSeedTask inserts one active task row at the shared frozen clock.
func deliveryPortSeedTask(t *testing.T, db *DB, id, agent, cron, message string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`INSERT INTO scheduled_tasks
		   (task_id, agent_name, cron, zone, message, enabled, definition_hash, created_at, updated_at,
		    caller_id, authorizing_kind)
		 VALUES ($1, $2, $3, 'UTC', $4, true, 'h1', $5, $5, $6, $7)`,
		id, agent, cron, message, opNow.UnixMilli(),
		core.LocalAdminCallerID, string(core.AuthorizingKindAdminBuiltin))
	require.NoError(t, err)
}

// deliveryPortPrepare 预写一条 pending 投递行。
// deliveryPortPrepare pre-writes one pending delivery row.
func deliveryPortPrepare(t *testing.T, store *DeliveryStore, runID string, createdAt time.Time) {
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

// deliveryPortSeedRunRow 建一行账本。终态行的 `reason` / `finished_at` 由用例显式给
// （nil → NULL）——只有它们非空，"恢复有没有覆盖已结束的行"才判得出来。PG 的 `runs`
// 带归属 NOT NULL 列，直插行补 local-admin + admin-builtin + actor=scheduler。
// deliveryPortSeedRunRow inserts one runs row with the scheduler ownership columns.
func deliveryPortSeedRunRow(t *testing.T, db *DB, runID, status string, session sql.NullString, reason any, finishedAt any) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`INSERT INTO runs
		   (run_id, session_id, agent, channel, user_id, trigger, status,
		    reason, iterations, tool_calls, config_hash, started_at, finished_at,
		    caller_id, authorizing_kind, actor)
		 VALUES ($1, $2, 'assistant', 'cli', 'u1', 'user', $3, $4, 0, 0, NULL, $5, $6, $7, $8, 'scheduler')`,
		runID, session, status, reason, opNow.Add(-time.Hour).UnixMilli(), finishedAt,
		core.LocalAdminCallerID, string(core.AuthorizingKindAdminBuiltin))
	require.NoError(t, err)
}

// deliveryPortClosePool 关掉连接池，造出真实的查询 / 写入失败。PG 只有一个连接池，
// 读写故障共用本助手（SQLite 侧分开关 db.read / db.write）。
// deliveryPortClosePool closes the single PostgreSQL pool to force real failures.
func deliveryPortClosePool(t *testing.T, db *DB) {
	t.Helper()
	require.NoError(t, db.pool.Close())
}

// ---- 通知侧夹具 --------------------------------------------------------------

// notifyPortSeedSession 建会话行。重复调用是 no-op（主键冲突忽略）。
// notifyPortSeedSession inserts one session row, ignoring duplicates.
func notifyPortSeedSession(t *testing.T, db *DB, sessionID string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`INSERT INTO sessions
		   (session_id, profile_name, channel, user_id, created_at, last_active_at)
		 VALUES ($1, 'assistant', 'cli', 'u1', $2, $2)
		 ON CONFLICT (session_id) DO NOTHING`,
		sessionID, notifyPortFixedNow.UnixMilli())
	require.NoError(t, err)
}

// notifyPortSeedRun 建一行挂在 notifyPortSessionID 上的账本。
// notifyPortSeedRun inserts one runs row bound to the notify session.
func notifyPortSeedRun(t *testing.T, db *DB, runID, status string) {
	t.Helper()
	notifyPortSeedSession(t, db, notifyPortSessionID)
	deliveryPortSeedRunRow(t, db, runID, status,
		sql.NullString{String: notifyPortSessionID, Valid: true}, nil, nil)
}

// notifyPortSeedToolInvocation 预置一行工具审计（`resultContent` 非空即写进
// `result_json.content`）。返回行 id 供逐列回读——PG 的自增 id 只能经 RETURNING 取回。
// notifyPortSeedToolInvocation returns the generated invocation id via RETURNING.
func notifyPortSeedToolInvocation(t *testing.T, db *DB, runID, callID, status, resultContent string) int64 {
	t.Helper()
	var result any
	if resultContent != "" {
		raw, err := json.Marshal(map[string]string{"content": resultContent})
		require.NoError(t, err)
		result = string(raw)
	}
	var id int64
	err := db.pool.QueryRowContext(context.Background(),
		`INSERT INTO tool_invocations
		   (run_id, session_id, tool_call_id, tool_name, input_json, result_json,
		    status, error_message, error_kind, duration_ms, started_at, finished_at)
		 VALUES ($1, $2, $3, 'read_file', '{}', $4, $5, NULL, NULL, NULL, $6, NULL)
		 RETURNING id`,
		runID, notifyPortSessionID, callID, result, status, notifyPortFixedNow.UnixMilli()).Scan(&id)
	require.NoError(t, err)
	return id
}

// notifyPortResourceUIDFor 按 `(kind, name)` 派生一个稳定的测试 UID。
// notifyPortResourceUIDFor derives a stable test uid from kind and name.
func notifyPortResourceUIDFor(kind core.ResourceKind, name string) core.ResourceUID {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + name))
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	payload := make([]byte, 26)
	for i := range payload {
		payload[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return core.ResourceUID(core.ResourceUIDPrefix + string(payload))
}

// notifyPortModelVersion 造一条模型连接版本；版本号 0 表示让仓储自动取号。
// notifyPortModelVersion builds a model_connection version; zero means auto-numbering.
func notifyPortModelVersion(resourceID, contentHash, operationID string, versionNo int) core.ResourceVersion {
	return core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    notifyPortResourceUIDFor(core.ResourceModelConnection, resourceID),
		ResourceID:     resourceID,
		VersionNo:      versionNo,
		ContentHash:    contentHash,
		DefinitionYAML: "name: " + resourceID + "\nmodel: gpt-test\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    operationID,
	}
}

// notifyPortSeedRunManifest 落库一条 Provider 版本并返回可准入的解析结果（生命周期
// active）。版本行的外键要求管理操作先存在——真实路径由基线 / US-12 的 BeginOperation
// 提供。原始夹具还落 MCP 与技能版本，通知用例只依赖 Provider 与通知集合，故按最小等价
// 夹具收敛（断言面不变）。
// notifyPortSeedRunManifest seeds the provider version the admission path requires.
func notifyPortSeedRunManifest(t *testing.T, db *DB) core.ResolvedResources {
	t.Helper()
	ctx := context.Background()
	seedOperation(t, db, "op-run-manifest")
	store := NewResourceStore(db)

	provider, _, err := store.EnsureVersion(ctx,
		notifyPortModelVersion("openai", "sha256:provider", "op-run-manifest", 0))
	require.NoError(t, err)

	return core.ResolvedResources{
		Provider: core.ResourceVersionRef{
			Name:        provider.ResourceID,
			ResourceUID: provider.ResourceUID,
			VersionID:   provider.ID,
			VersionNo:   provider.VersionNo,
			ContentHash: provider.ContentHash,
		},
		ProviderModel: "gpt-test",
		ProviderCredential: core.CredentialPin{
			Mode:          core.CredentialModeEnv,
			TargetBinding: testTargetBinding,
		},
		Notify: core.NotifyManifest{Mode: core.NotifyModeAllEligible, Channels: []core.NotifyChannelRef{}},
	}
}

// notifyPortStartRow 造一份起始行夹具（渠道参数化）。PG 的 runs 归属四列 NOT NULL 且带
// CHECK，这里补内置管理员 Principal。
// notifyPortStartRow builds a starting-row fixture carrying the built-in admin principal.
func notifyPortStartRow(channel string) core.RunRecord {
	return core.RunRecord{
		RunID:      channel + "-1776000000123-abc123",
		SessionID:  "cli-3f9a1c07b2d84e55",
		Agent:      "demo",
		Channel:    channel,
		UserID:     "alice",
		Principal:  core.LocalAdminPrincipal(),
		ConfigHash: "3f9a1c07b2d84e55",
		StartedAt:  notifyPortFixedStart,
	}
}

// TestDeliveryStore_PrepareIsIdempotentAndStaysPending —— 同一 run_id 重复预写是空操作：
// 不产生第二行，也不覆盖既有行的名称快照；已回填终态后再预写同样不改终态。
// 逆改点：Prepare 改成 upsert → 重试把 sent 打回 pending，恢复会再投一次，本用例红。
func TestDeliveryStore_PrepareIsIdempotentAndStaysPending(t *testing.T) {
	store, _ := deliveryPortNewStore(t)
	ctx := context.Background()
	deliveryPortPrepare(t, store, "run-1", deliveryPortPolicyNow)

	// 第二次预写换一份名称快照：既有行必须原样保留。
	require.NoError(t, store.Prepare(ctx, core.ScheduleDeliveryRecord{
		RunID:       "run-1",
		TaskID:      deliveryTaskID,
		Agent:       "另一个名字",
		ChannelName: "另一个渠道",
		CreatedAt:   deliveryPortPolicyNow.Add(time.Minute),
	}))

	rec, err := store.Get(ctx, "run-1")
	require.NoError(t, err)
	require.Equal(t, core.DeliveryPending, rec.Status)
	require.Equal(t, "assistant", rec.Agent, "既有名称快照不得被覆盖")
	require.Equal(t, "alerts", rec.ChannelName)
	require.True(t, rec.CreatedAt.Equal(deliveryPortPolicyNow))
	require.True(t, rec.FinishedAt.IsZero())

	require.NoError(t, store.Finish(ctx, "run-1", core.DeliverySent, 200, nil, "发送成功",
		deliveryPortPolicyNow, deliveryPortPolicyNow))
	deliveryPortPrepare(t, store, "run-1", deliveryPortPolicyNow.Add(2*time.Minute))
	after, err := store.Get(ctx, "run-1")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySent, after.Status, "已回填的终态不得被打回 pending")
}

// TestDeliveryStore_FinishWritesTerminalOnce —— 终态只从 pending 回填一次：状态、HTTP
// 状态、回执 code（0 是飞书成功）、说明与两个时刻都落库；重复回填不改既有终态，未知
// run_id 报 `fs.ErrNotExist`。
// 逆改点：去掉 `AND status = 'pending'` → 迟到回执覆盖既有结论，本用例红。
func TestDeliveryStore_FinishWritesTerminalOnce(t *testing.T) {
	store, _ := deliveryPortNewStore(t)
	ctx := context.Background()
	deliveryPortPrepare(t, store, "run-2", deliveryPortPolicyNow)

	zero := 0
	attempted := deliveryPortPolicyNow.Add(30 * time.Second)
	finished := deliveryPortPolicyNow.Add(45 * time.Second)
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
	err = store.Finish(ctx, "run-unknown", core.DeliverySent, 200, nil, "",
		deliveryPortPolicyNow, deliveryPortPolicyNow)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestDeliveryStore_NullReceiptStaysDistinctFromZero —— 未取得回执写 NULL，读回是
// nil 指针；未取得 HTTP 状态写 NULL，读回 0；两个时刻为零值时读回零值时间。
// 逆改点：NULL 与 0 用同一列类型混存 → 页面把"未取得"显示成"0"，本用例红。
func TestDeliveryStore_NullReceiptStaysDistinctFromZero(t *testing.T) {
	store, _ := deliveryPortNewStore(t)
	ctx := context.Background()
	deliveryPortPrepare(t, store, "run-3", deliveryPortPolicyNow)
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
	require.NoError(t, store.db.pool.QueryRowContext(ctx,
		`SELECT receipt_code FROM schedule_deliveries WHERE run_id = $1`, "run-3").Scan(&nullReceipt))
	require.False(t, nullReceipt.Valid, "库里必须是 NULL，不是 0")
}

// TestDeliveryStore_ListByTaskNewestFirstWithLimit —— 按创建时间新→旧、最多 limit 条；
// 没有记录的 task 返回空（不是错误）。
func TestDeliveryStore_ListByTaskNewestFirstWithLimit(t *testing.T) {
	store, _ := deliveryPortNewStore(t)
	ctx := context.Background()
	deliveryPortPrepare(t, store, "run-old", deliveryPortPolicyNow.Add(-time.Hour))
	deliveryPortPrepare(t, store, "run-mid", deliveryPortPolicyNow.Add(-time.Minute))
	deliveryPortPrepare(t, store, "run-new", deliveryPortPolicyNow)

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
	store, _ := deliveryPortNewStore(t)
	_, err := store.Get(context.Background(), "run-missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestDeliveryStore_RecoverResolvesFromRunState —— 崩溃遗留的 pending 行按关联 Run 的
// 实际终态改判：已完成 / 达上限 → unknown（模型已产出正文，投递是否发生无法证明），
// 失败 / 仍运行 / 无 Run → skipped；已回填的行不动；再跑一次恢复 0 条。
// 逆改点：unknown 被写成 failed 并重发 → 重复投递，本用例红。
func TestDeliveryStore_RecoverResolvesFromRunState(t *testing.T) {
	store, db := deliveryPortNewStore(t)
	ctx := context.Background()

	for _, runID := range []string{
		"run-completed", "run-limit", "run-failed",
		"run-running", "run-orphan", "run-sent",
	} {
		deliveryPortPrepare(t, store, runID, deliveryPortPolicyNow)
	}
	deliveryPortSeedRunRow(t, db, "run-completed", string(core.StatusCompleted), sql.NullString{}, nil, nil)
	deliveryPortSeedRunRow(t, db, "run-limit", string(core.StatusLimitReached), sql.NullString{}, nil, nil)
	deliveryPortSeedRunRow(t, db, "run-failed", string(core.StatusFailed), sql.NullString{}, nil, nil)
	deliveryPortSeedRunRow(t, db, "run-running", string(core.StatusRunning), sql.NullString{}, nil, nil)
	// run-orphan 故意没有 runs 行：pending 先于 Run 准入写出，这是合法状态。
	require.NoError(t, store.Finish(ctx, "run-sent", core.DeliverySent, 200, nil, "发送成功",
		deliveryPortPolicyNow, deliveryPortPolicyNow))

	now := deliveryPortPolicyNow.Add(time.Hour)
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
	store, _ := deliveryPortNewStore(t)
	recovered, err := store.Recover(context.Background(), deliveryPortPolicyNow)
	require.NoError(t, err)
	require.Equal(t, 0, recovered)
}

// TestDeliveryStore_ErrorPathsReportContext —— 池故障带上下文前缀返回；读取故障不得被
// 误判成"不存在"（否则页面会显示没有记录）。
func TestDeliveryStore_ErrorPathsReportContext(t *testing.T) {
	ctx := context.Background()

	t.Run("read", func(t *testing.T) {
		store, db := deliveryPortNewStore(t)
		deliveryPortClosePool(t, db)
		_, err := store.Get(ctx, "run-1")
		require.Error(t, err)
		require.False(t, errors.Is(err, fs.ErrNotExist), "池故障不得伪装成记录不存在")
		require.Contains(t, err.Error(), "读取定时投递记录")
		_, err = store.ListByTask(ctx, deliveryTaskID, 5)
		require.Error(t, err)
		require.Contains(t, err.Error(), "列出定时投递记录")
	})

	t.Run("write", func(t *testing.T) {
		store, db := deliveryPortNewStore(t)
		deliveryPortClosePool(t, db)
		err := store.Prepare(ctx, core.ScheduleDeliveryRecord{
			RunID:     "run-x",
			TaskID:    deliveryTaskID,
			Agent:     "assistant",
			CreatedAt: deliveryPortPolicyNow,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "预写定时投递记录")

		err = store.Finish(ctx, "run-x", core.DeliverySent, 200, nil, "",
			deliveryPortPolicyNow, deliveryPortPolicyNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "回填定时投递记录")

		// PG 只有一个连接池：关闭后 Recover 的失败点在事务开启（SQLite 侧是「获取写连接」）。
		_, err = store.Recover(ctx, deliveryPortPolicyNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "开启 PostgreSQL 事务")
	})
}

// TestNotifyDeliveryStore_OnePerInvocation：投递记录的主键是工具调用 id——
// 第二次记录同一调用是冲突而不是静默覆盖；首条记录保留，审计读到的永远是第一次。
// 中文验收点：一次调用一条账，重复记录不覆盖。
func TestNotifyDeliveryStore_OnePerInvocation(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	store := NewNotifyDeliveryStore(db)

	notifyPortSeedSession(t, db, notifyPortSessionID)
	notifyPortSeedRun(t, db, "run-notify-1", "running")
	invocation := notifyPortSeedToolInvocation(t, db, "run-notify-1", "call-1", "succeeded", "ok")

	record := core.NotifyDeliveryRecord{
		ToolInvocationID:  invocation,
		RunID:             "run-notify-1",
		AgentUID:          testAgentUID,
		ChannelUID:        testResourceUID('A'),
		ChannelName:       "ops",
		CredentialID:      testCredentialID('A'),
		CredentialVersion: 2,
		TargetHost:        "open.feishu.cn",
		ResolvedAt:        opNow.UnixMilli(),
	}
	require.NoError(t, store.Record(ctx, record))

	stored, ok, err := store.Delivery(ctx, invocation)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, record.RunID, stored.RunID)
	require.Equal(t, record.AgentUID, stored.AgentUID)
	require.Equal(t, record.ChannelUID, stored.ChannelUID)
	require.Equal(t, record.ChannelName, stored.ChannelName)
	require.Equal(t, record.CredentialID, stored.CredentialID)
	require.Equal(t, record.CredentialVersion, stored.CredentialVersion)
	require.Equal(t, record.TargetHost, stored.TargetHost)
	require.Equal(t, record.ResolvedAt, stored.ResolvedAt)

	// 重复投递：冲突，且首条记录不被改写。
	duplicate := record
	duplicate.ChannelName = "other"
	duplicate.CredentialVersion = 3
	require.ErrorIs(t, store.Record(ctx, duplicate), core.ErrConflict)

	stored, ok, err = store.Delivery(ctx, invocation)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ops", stored.ChannelName)
	require.Equal(t, 2, stored.CredentialVersion)

	deliveries, err := store.Deliveries(ctx, "run-notify-1")
	require.NoError(t, err)
	require.Len(t, deliveries, 1)

	// 没记录过的调用：ok=false（"还没发"与"发过没记"要能分辨）。
	_, ok, err = store.Delivery(ctx, invocation+1)
	require.NoError(t, err)
	require.False(t, ok)

	// env 模式（无凭据身份）：凭据列留空，不得带版本；带版本即拒绝。
	envInvocation := notifyPortSeedToolInvocation(t, db, "run-notify-1", "call-2", "succeeded", "ok")
	envRecord := core.NotifyDeliveryRecord{
		ToolInvocationID: envInvocation,
		RunID:            "run-notify-1",
		AgentUID:         testAgentUID,
		ChannelUID:       testResourceUID('B'),
		ChannelName:      "mail",
		TargetHost:       "hooks.example.com",
	}
	require.NoError(t, store.Record(ctx, envRecord))
	stored, ok, err = store.Delivery(ctx, envInvocation)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, stored.CredentialID)
	require.Zero(t, stored.CredentialVersion)

	envRecord.ToolInvocationID = notifyPortSeedToolInvocation(t, db, "run-notify-1", "call-3", "succeeded", "ok")
	envRecord.CredentialVersion = 1
	require.Error(t, store.Record(ctx, envRecord), "env 模式不得带凭据版本")
}

// TestNotifyDeliveryStore_AdmissionFreezesChannels：准入把清单里的渠道集合冻进
// `run_notify_refs`，终态释放；通知**不**固定凭据版本（run_credential_refs 只记模型连接）。
// 中文验收点：渠道集合准入冻结、随终态释放、不产生凭据固定行。
func TestNotifyDeliveryStore_AdmissionFreezesChannels(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	runs := NewRunStore(db)
	store := NewNotifyDeliveryStore(db)
	pins := NewRunCredentialStore(db)

	resolved := notifyPortSeedRunManifest(t, db)
	resolved.Notify = core.NotifyManifest{
		Mode: core.NotifyModeAllEligible,
		Channels: []core.NotifyChannelRef{
			{Name: "ops", ResourceUID: testResourceUID('A')},
			{Name: "mail", ResourceUID: testResourceUID('B')},
		},
	}
	manifest, err := core.BuildRunResourceManifest(testAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)

	rec := notifyPortStartRow("scheduler")
	rec.RunID = "scheduler-1776000000123-notify1"
	require.NoError(t, runs.StartRun(ctx, rec, manifest))

	refs, err := store.Refs(ctx, rec.RunID)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	require.Equal(t, "mail", refs[0].ChannelName, "按名称升序")
	require.Equal(t, testResourceUID('B'), refs[0].ChannelUID)
	require.Equal(t, testAgentUID, refs[0].AgentUID)
	require.Nil(t, refs[0].ReleasedAt)

	pinned, err := pins.Pins(ctx, rec.RunID)
	require.NoError(t, err)
	require.Empty(t, pinned, "通知不固定凭据版本")

	require.True(t, requireFinishRun(t, runs, ctx, rec.RunID, core.StatusCompleted, "", 1, 1, notifyPortFixedNow))
	require.NoError(t, runs.ReleaseRunRefs(ctx, rec.RunID, notifyPortFixedNow))
	refs, err = store.Refs(ctx, rec.RunID)
	require.NoError(t, err)
	require.Len(t, refs, 2, "释放不删行")
	for _, ref := range refs {
		require.NotNil(t, ref.ReleasedAt)
	}

	// 独立释放：只动未释放的行，重复释放是 no-op。
	released, err := store.ReleaseRefs(ctx, rec.RunID, notifyPortFixedNow)
	require.NoError(t, err)
	require.Zero(t, released)
}
