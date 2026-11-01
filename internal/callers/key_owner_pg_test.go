//go:build integration

package callers_test

// 本文件守 C-32 的**轮换再绑定**闭环：`KeyStore.Rotate` 在显式声明
// `RebindScheduledTasks` 时把符合条件任务的 `authorizing_key_id` 换成新 Key，换完之后
// 调度执行读到的就是新 Key——同一条链路上"任务绑定"与"执行复核"必须是同一把。
// 未声明再绑定（或 scope 不覆盖）时任务保持旧绑定，下一次触发按 `owner_unauthorized`
// fail-closed，**不得**借用同一调用方的其他 Key。
// This file pins the rotation rebind loop end to end: after an explicit rebind the
// scheduler executes on the new key, and an unrebound task fails closed.

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// keyOwnerHarness 把真 Key 存储、真任务存储与真调度器装在同一个隔离 schema 上。
// keyOwnerHarness wires the real key store, task store and scheduler on one schema.
type keyOwnerHarness struct {
	t       *testing.T
	db      *postgres.DB
	keys    callers.KeyStore
	issuer  *callers.KeyIssuer
	tasks   *postgres.TaskStore
	sched   *core.Scheduler
	invoker *recordingInvoker
	now     time.Time
}

// recordingInvoker 记录调度器发起的运行请求；缺省返回 completed。
// recordingInvoker records the run requests the scheduler issues.
type recordingInvoker struct {
	mu   sync.Mutex
	reqs []core.RunRequest
}

func (r *recordingInvoker) Run(_ context.Context, req core.RunRequest) (core.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return core.RunResult{RunID: req.RunID, Status: core.StatusCompleted}, nil
}

// count 返回已发起的运行次数（失败关闭用例要断言"一次都没出去"）。
// count reports how many runs were issued; fail-closed cases assert zero.
func (r *recordingInvoker) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// newKeyOwnerHarness 建隔离库、装配签发器与调度器，并种一个可用调用方。
// newKeyOwnerHarness builds the isolated schema and the wired scheduler.
func newKeyOwnerHarness(t *testing.T) *keyOwnerHarness {
	t.Helper()
	db := pgtest.NewTestDB(t)
	keys := postgres.NewKeyStore(db)
	h := &keyOwnerHarness{
		t:       t,
		db:      db,
		keys:    keys,
		issuer:  callers.NewKeyIssuer(keys, postgres.NewManagementStore(db)),
		tasks:   postgres.NewTaskStore(db),
		invoker: &recordingInvoker{},
		now:     time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC),
	}
	h.sched = core.NewScheduler(core.SchedulerOptions{
		Store:      h.tasks,
		Invoker:    h.invoker,
		Entries:    []core.AgentEntry{{Name: schedOwnerAgent, Registered: true}},
		OwnerKeys:  callers.NewKeyVerifier(keys),
		Budget:     core.Budget{MaxIterations: 10},
		RunTimeout: time.Minute,
		StopGrace:  time.Minute,
		Logger:     slog.New(slog.DiscardHandler),
		Now:        func() time.Time { return h.now },
	})
	h.seedCaller("caller-1")
	return h
}

// schedOwnerAgent 是调度用例的固定 Agent 名（与任务行、scope 三处必须一致）。
const schedOwnerAgent = "daily"

// seedCaller 种一个可用调用方。
func (h *keyOwnerHarness) seedCaller(callerID string) {
	h.t.Helper()
	_, err := h.db.ExecContext(h.t.Context(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', 'active', $2)`, callerID, h.now.UnixMilli())
	require.NoError(h.t, err)
}

// seedKey 种一把有效 Key（scope 含该 Agent 的调用权）。
func (h *keyOwnerHarness) seedKey(callerID, keyID string) {
	h.t.Helper()
	_, err := h.db.ExecContext(h.t.Context(), `
		INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		keyID, callerID, "hash-"+keyID, "bk_"+keyID,
		[]string{"sessions:read", core.AgentInvokeScopePrefix + schedOwnerAgent},
		h.now.UnixMilli())
	require.NoError(h.t, err)
}

// seedScheduledTaskWithKey 种一条绑定到指定 Key 的定时任务。
func (h *keyOwnerHarness) seedScheduledTaskWithKey(callerID, keyID string) string {
	h.t.Helper()
	const taskID = "daily-1"
	_, err := h.db.ExecContext(h.t.Context(), `
		INSERT INTO scheduled_tasks
			(task_id, agent_name, cron, zone, message, deliver_to, enabled, definition_hash,
			 created_at, updated_at, caller_id, authorizing_kind, authorizing_key_id)
		VALUES ($1, $2, '0 */5 * * * *', 'UTC', '巡检', '', true, 'hash', $3, $3, $4, 'caller-key', $5)`,
		taskID, schedOwnerAgent, h.now.UnixMilli(), callerID, keyID)
	require.NoError(h.t, err)
	return taskID
}

// taskAuthorizingKey 读任务当前绑定的 Key。
func (h *keyOwnerHarness) taskAuthorizingKey(taskID string) string {
	h.t.Helper()
	var keyID string
	require.NoError(h.t, h.db.QueryRowContext(h.t.Context(),
		`SELECT authorizing_key_id FROM scheduled_tasks WHERE task_id = $1`, taskID).Scan(&keyID))
	return keyID
}

// lastExecutionReason 读最近一条执行行的 error_message。
func (h *keyOwnerHarness) lastExecutionReason(taskID string) string {
	h.t.Helper()
	rows, err := h.tasks.ListExecutions(h.t.Context(), taskID, 1)
	require.NoError(h.t, err)
	require.NotEmpty(h.t, rows, "到点触发必须落一条执行行")
	return rows[0].ErrorMessage
}

// TestKeyRotation_RebindsEligibleScheduledTasks 守 §3.1.5：显式声明再绑定时，任务在同一
// 事务里换到新 Key，下一次触发用**新 Key**执行——绑定与执行复核不能各持一把。
// 逆改点：`Rotate` 忽略 `RebindScheduledTasks` → 任务仍绑 key-old（已吊销），触发时
// fail-closed；调度器复核时改用"调用方任意有效 Key" → 执行身份里的 key_id 不是新 Key。
func TestKeyRotation_RebindsEligibleScheduledTasks(t *testing.T) {
	h := newKeyOwnerHarness(t)
	h.seedKey("caller-1", "key-old")
	taskID := h.seedScheduledTaskWithKey("caller-1", "key-old")

	actor := core.Actor{Subject: core.ActorSubjectLocalTrust, Method: core.AuthMethodLocalTrust, Source: "127.0.0.1:1"}
	issued, result, err := h.issuer.Rotate(h.t.Context(), actor, callers.RotateRequest{
		CallerID: "caller-1", OldKeyID: "key-old",
		Next:                 callers.KeyRecord{Scopes: []string{"sessions:read", core.AgentInvokeScopePrefix + schedOwnerAgent}},
		RebindScheduledTasks: true, // 显式声明：同一事务内再绑定
	})
	require.NoError(t, err)
	require.Equal(t, issued.KeyID, h.taskAuthorizingKey(taskID), "符合条件的任务再绑定到新 Key")
	require.Equal(t, []string{taskID}, result.ReboundTaskIDs)
	require.Empty(t, result.LeftBound)

	req, res, err := h.sched.RunNow(h.t.Context(), taskID)
	require.NoError(t, err, "再绑定后任务必须可执行")
	require.NotContains(t, h.lastExecutionReason(taskID), "owner_unauthorized")
	require.Equal(t, issued.KeyID, req.Principal.KeyID, "执行身份绑定的是新 Key")
	require.Equal(t, "caller-1", req.Principal.CallerID, "再绑定不改归属")
	require.Equal(t, core.ActorScheduler, req.Principal.Actor)
	require.Equal(t, core.StatusCompleted, res.Status)
}

// TestKeyRotation_LeavesIneligibleTasksBoundToRevokedKey 守 C-32 / §3.1.5：轮换**没有**
// 显式再绑定，或新 Key 的 scope 不覆盖任务时，任务保持旧绑定（进 `LeftBound` 并带原因
// 码），下一次触发按 `owner_unauthorized` fail-closed——同一调用方此刻还有一把有效
// 新 Key，但任务**不得**借用它，也不得被静默改绑。
// 逆改点：把缺省行为改成"顺手全绑上新 Key" → 第一条绑定断言红；scope 不足时照样改绑
// → `LeftBound` 断言红；执行前跳过 Key 复核 → 运行计数 1，红。
func TestKeyRotation_LeavesIneligibleTasksBoundToRevokedKey(t *testing.T) {
	actor := core.Actor{Subject: core.ActorSubjectLocalTrust, Method: core.AuthMethodLocalTrust, Source: "127.0.0.1:1"}
	rotate := func(h *keyOwnerHarness, scopes []string, rebind bool) (callers.IssuedKey, callers.RotateResult) {
		h.t.Helper()
		issued, result, err := h.issuer.Rotate(h.t.Context(), actor, callers.RotateRequest{
			CallerID: "caller-1", OldKeyID: "key-old",
			Next:                 callers.KeyRecord{Scopes: scopes},
			RebindScheduledTasks: rebind,
		})
		require.NoError(h.t, err)
		return issued, result
	}
	covered := []string{"sessions:read", core.AgentInvokeScopePrefix + schedOwnerAgent}

	t.Run("rebind_not_requested", func(t *testing.T) {
		h := newKeyOwnerHarness(t)
		h.seedKey("caller-1", "key-old")
		taskID := h.seedScheduledTaskWithKey("caller-1", "key-old")

		issued, result := rotate(h, covered, false) // 缺省：不声明即不绑
		require.NotEmpty(t, issued.KeyID, "轮换本身必须成功：同调用方此刻确实有一把有效新 Key")
		require.Empty(t, result.ReboundTaskIDs)
		require.Empty(t, result.LeftBound, "未声明再绑定时连候选都不读，不是逐条判失败")
		require.Equal(t, "key-old", h.taskAuthorizingKey(taskID), "未声明的任务保持旧绑定")

		// 同调用方此刻有另一把有效 Key（issued.KeyID）——任务仍必须失败关闭。
		_, _, err := h.sched.RunNow(h.t.Context(), taskID)
		require.Error(t, err, "旧 Key 已吊销，任务不得继续执行")
		require.Contains(t, h.lastExecutionReason(taskID), "owner_unauthorized")
		require.Zero(t, h.invoker.count(), "不得借用同调用方的新 Key 出网")
	})

	t.Run("scope_missing", func(t *testing.T) {
		h := newKeyOwnerHarness(t)
		h.seedKey("caller-1", "key-old")
		taskID := h.seedScheduledTaskWithKey("caller-1", "key-old")

		issued, result := rotate(h, []string{"sessions:read"}, true) // 声明了，但新 Key 不覆盖该 Agent
		require.Empty(t, result.ReboundTaskIDs, "scope 不覆盖的任务不得改绑")
		require.Equal(t,
			[]callers.BoundTask{{TaskID: taskID, Reason: callers.BoundTaskScopeMissing}},
			result.LeftBound, "判不通过的任务必须显式报告，不得静默改绑")
		require.Equal(t, "key-old", h.taskAuthorizingKey(taskID))

		_, _, err := h.sched.RunNow(h.t.Context(), taskID)
		require.Error(t, err)
		require.Contains(t, h.lastExecutionReason(taskID), "owner_unauthorized")
		require.Zero(t, h.invoker.count(), "scope 不覆盖的任务不得出网")
		require.NotEmpty(t, issued.KeyID, "新 Key 已签发，只是 scope 不覆盖该任务")
	})
}
