//go:build integration

package postgres_test

// 本文件守 C-32 / §3.4.2 的**恢复归属**：崩溃恢复只做标记、不重放，收尾时执行者记
// `recovery`，而 `caller_id` / `authorizing_kind` / `key_id` **一律不动**；caller-key
// 的运行在收尾前还要复核**那一把**原授权 Key——失效时 reason 写 `owner_unauthorized`。
// 恢复路径没有模型调用，因此"不得继续出网"在这里表现为"没有任何执行面参与"，
// 由恢复契约本身（只改行、不触发运行）保证。
// These cases pin recovery ownership: the executor becomes recovery, the owner columns
// never change, and a run whose bound key no longer holds fails as owner_unauthorized.

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// slogDiscard 是恢复扫描的静默日志目标：用例只判行，不判日志文案。
// slogDiscard silences the recovery summary log inside these cases.
func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// recoveryOwnerHarness 是一个隔离 schema 上的恢复夹具（真库、真 SQL、固定时钟）。
// recoveryOwnerHarness runs recovery against one isolated schema.
type recoveryOwnerHarness struct {
	t   *testing.T
	db  *postgres.DB
	now time.Time
}

// newRecoveryOwnerHarness 建隔离库并固定注入时钟。
// newRecoveryOwnerHarness builds an isolated database with a fixed clock.
func newRecoveryOwnerHarness(t *testing.T) *recoveryOwnerHarness {
	t.Helper()
	return &recoveryOwnerHarness{
		t:   t,
		db:  pgtest.NewTestDB(t),
		now: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC),
	}
}

// seedCaller 种一个可用调用方。
// seedCaller plants an active caller.
func (h *recoveryOwnerHarness) seedCaller(callerID string) {
	h.t.Helper()
	_, err := h.db.ExecContext(h.t.Context(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', 'active', $2)`, callerID, h.now.UnixMilli())
	require.NoError(h.t, err)
}

// seedKey 种一把 Key；`revoked` 为真时同时写吊销时刻（吊销是唯一的显式失效动作）。
// seedKey plants one key, optionally already revoked.
func (h *recoveryOwnerHarness) seedKey(callerID, keyID string, revoked bool, scopes ...string) {
	h.t.Helper()
	var revokedAt any
	if revoked {
		revokedAt = h.now.UnixMilli()
	}
	_, err := h.db.ExecContext(h.t.Context(), `
		INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		keyID, callerID, "hash-"+keyID, "bk_"+keyID, scopes, revokedAt, h.now.UnixMilli())
	require.NoError(h.t, err)
}

// seedInterruptedRun 种一条崩溃前仍为 `running` 的 caller-key 运行（绑定 key-a）。
// seedInterruptedRun plants one running caller-key run bound to key-a.
func (h *recoveryOwnerHarness) seedInterruptedRun(callerID, agent string) string {
	h.t.Helper()
	const runID = "run-crashed-1"
	_, err := h.db.ExecContext(h.t.Context(), `
		INSERT INTO runs (run_id, agent, channel, user_id, trigger, status, started_at,
		                  caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, $2, 'scheduler', 'scheduler', 'cron', 'running', $3,
		        $4, 'caller-key', $5, 'scheduler')`,
		runID, agent, h.now.Add(-time.Minute).UnixMilli(), callerID, "key-a")
	require.NoError(h.t, err)
	return runID
}

// seedCallerWithKey 是多数用例的公共前置：调用方 + 有效 key-a + 崩溃运行。
// seedCallerWithKey plants the caller, its live key and the interrupted run.
func (h *recoveryOwnerHarness) seedCallerWithKey(callerID, agent string) string {
	h.t.Helper()
	h.seedCaller(callerID)
	h.seedKey(callerID, "key-a", false, "sessions:read", core.AgentInvokeScopePrefix+agent)
	return h.seedInterruptedRun(callerID, agent)
}

// revokeAllKeys 吊销该调用方的全部 Key。
// revokeAllKeys revokes every key of the caller.
func (h *recoveryOwnerHarness) revokeAllKeys(callerID string) {
	h.t.Helper()
	_, err := h.db.ExecContext(h.t.Context(),
		`UPDATE caller_keys SET revoked_at = $2 WHERE caller_id = $1 AND revoked_at IS NULL`,
		callerID, h.now.UnixMilli())
	require.NoError(h.t, err)
}

// seedExpiredLeases 为每条 `running` 行补一条**已过期**的 run 租约。
//
// US-24 起恢复只作用于「合法领取的过期租约」（FR-24-02 / FR-24-15）：这些夹具直接
// 种库，崩溃现场没有真实租约行，所以这里显式造出"属主已失联"的既成事实。反过来说，
// 按心跳或进程启动时间推断死亡正是本 US 明确拒绝的路径——本夹具若改成那种写法，
// 下面所有用例都会红。
// seedExpiredLeases plants one already-expired run lease per running row.
func (h *recoveryOwnerHarness) seedExpiredLeases() {
	h.t.Helper()
	_, err := h.db.ExecContext(h.t.Context(), `
		INSERT INTO execution_leases
			(kind, resource_id, owner_worker_id, ownership_epoch,
			 acquired_at, renewed_at, expires_at, released_at)
		SELECT 'run', r.run_id, 'dead-worker', 1, $1, $1, $1, NULL
		  FROM runs r
		 WHERE r.status = 'running'
		ON CONFLICT (kind, resource_id) DO NOTHING`,
		h.now.Add(-time.Minute).UnixMilli())
	require.NoError(h.t, err)
}

// recover 走一次**作用域限定**的接管收尾：先按过期租约领取，再逐条按其作用域恢复。
// 没有全局扫描（FR-24-02）：没被领取的行一条都不会动。
// recover claims expired leases then recovers each claimed run in its own scope.
func (h *recoveryOwnerHarness) recover() {
	h.t.Helper()
	h.seedExpiredLeases()
	worker := core.WorkerIdentity{
		InstanceID: "recovery-owner-test",
		Name:       "recovery-owner-test",
	}
	leases, err := postgres.NewReclaimerStore(h.db).ClaimExpired(h.t.Context(),
		core.LeaseRun, worker, 64, time.Minute)
	require.NoError(h.t, err)
	for _, lease := range leases {
		_, err := postgres.RecoverRun(h.t.Context(), h.db, slogDiscard(),
			lease.LeaseToken, h.now)
		require.NoError(h.t, err)
	}
}

// lastRunPrincipal 从运行行重建执行身份：归属取 `caller_id`，执行者取 `actor`。
// lastRunPrincipal rebuilds the principal from the run row.
func (h *recoveryOwnerHarness) lastRunPrincipal(runID string) core.Principal {
	h.t.Helper()
	var callerID, kind, keyID, actor string
	require.NoError(h.t, h.db.QueryRowContext(h.t.Context(), `
		SELECT caller_id, authorizing_kind, COALESCE(key_id, ''), actor
		  FROM runs WHERE run_id = $1`, runID).Scan(&callerID, &kind, &keyID, &actor))
	p := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		KeyID:    keyID,
		Actor:    core.ActorKind(actor),
	}
	if kind == string(core.AuthorizingKindAdminBuiltin) {
		p.Kind = core.PrincipalLocalAdmin
		p.Builtin = true
	}
	return p
}

// usageCaller 直接读归属列：用量与配额一律按 `runs.caller_id` 归账，恢复不得把它
// 改写成执行者——这条读数与身份重建分开，避免"重建逻辑自己也错了"时两边一起变绿。
// usageCaller reads the attribution column raw; recovery must not rewrite it.
func (h *recoveryOwnerHarness) usageCaller(runID string) string {
	h.t.Helper()
	var callerID string
	require.NoError(h.t, h.db.QueryRowContext(h.t.Context(),
		`SELECT caller_id FROM runs WHERE run_id = $1`, runID).Scan(&callerID))
	return callerID
}

// runStatus / runReason 读取收尾后的状态与原因。
// runStatus and runReason read the terminal status and reason.
func (h *recoveryOwnerHarness) runStatus(runID string) core.RunStatus {
	h.t.Helper()
	var status string
	require.NoError(h.t, h.db.QueryRowContext(h.t.Context(),
		`SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&status))
	return core.RunStatus(status)
}

func (h *recoveryOwnerHarness) runReason(runID string) string {
	h.t.Helper()
	var reason string
	require.NoError(h.t, h.db.QueryRowContext(h.t.Context(),
		`SELECT COALESCE(reason, '') FROM runs WHERE run_id = $1`, runID).Scan(&reason))
	return reason
}

// TestRecovery_PreservesRunCaller 守 §3.4.2：恢复只更新 `actor`，归属列一个字都不改。
// 逆改点：收尾语句写 `actor='caller'`（或沿用原 actor）→ 执行者断言红；
// 用 `LocalAdminPrincipal()` 代表恢复 → CallerID 变 local-admin、Builtin=true，
// 归属与配额两条断言同时红。
func TestRecovery_PreservesRunCaller(t *testing.T) {
	h := newRecoveryOwnerHarness(t)
	runID := h.seedCallerWithKey("caller-1", "demo")

	h.recover()

	got := h.lastRunPrincipal(runID)
	require.Equal(t, "caller-1", got.CallerID, "恢复必须保留原归属")
	require.Equal(t, core.ActorRecovery, got.Actor, "执行者单独记录")
	require.False(t, got.Builtin, "不得变成内置管理员")
	require.Equal(t, "key-a", got.KeyID, "原授权 Key 不得被换掉")
	require.Equal(t, "caller-1", h.usageCaller(runID), "用量记在归属调用方，不记执行者")
	require.Equal(t, core.StatusFailed, h.runStatus(runID), "崩溃运行收尾为 failed")
}

// TestRecovery_RevokedOwnerFailsClosed 守 C-32：原授权 Key 已失效的运行，收尾原因
// 必须是 `owner_unauthorized`，不能只写通用中断文案——否则"授权没了"和"进程崩了"
// 在账上分不开。
// 逆改点：收尾语句不查 caller_keys → reason 恒为进程中断文案，断言红。
func TestRecovery_RevokedOwnerFailsClosed(t *testing.T) {
	h := newRecoveryOwnerHarness(t)
	runID := h.seedCallerWithKey("caller-1", "demo")
	h.revokeAllKeys("caller-1")

	h.recover()

	require.Equal(t, core.StatusFailed, h.runStatus(runID))
	require.Contains(t, h.runReason(runID), "owner_unauthorized",
		"归属失效必须以 owner_unauthorized 收尾")
	require.Equal(t, core.ActorRecovery, h.lastRunPrincipal(runID).Actor,
		"授权失败不改变执行者标注")
}

// revokeKey 只吊销指定的一把 Key：区分"同一调用方还有别的有效 Key"与"整户失效"。
// revokeKey revokes exactly one key, leaving the caller's other keys live.
func (h *recoveryOwnerHarness) revokeKey(callerID, keyID string) {
	h.t.Helper()
	_, err := h.db.ExecContext(h.t.Context(),
		`UPDATE caller_keys SET revoked_at = $3 WHERE caller_id = $1 AND key_id = $2`,
		callerID, keyID, h.now.UnixMilli())
	require.NoError(h.t, err)
}

// requireAdminBuiltinCaller 断言内置管理员调用方已由建库 / 迁移 upsert（`schema.pg.sql`
// 第 1192 行的固定项）：`runs` 的 CHECK 要求 admin-builtin 的归属逐字是 `local-admin`，
// 且 `caller_id` 有外键——这条前置不存在时应红在建库，不是在本用例里补种。
// requireAdminBuiltinCaller asserts the migration-provided built-in caller exists.
func (h *recoveryOwnerHarness) requireAdminBuiltinCaller() {
	h.t.Helper()
	var kind string
	require.NoError(h.t, h.db.QueryRowContext(h.t.Context(),
		`SELECT kind FROM callers WHERE caller_id = 'local-admin'`).Scan(&kind))
	require.Equal(h.t, "builtin", kind, "local-admin 必须由建库 / 迁移预置为内置调用方")
}

// seedInterruptedAdminBuiltinRun 种一条 admin-builtin 的崩溃运行：没有授权 Key，
// 授权来源是部署内置管理员能力。
// seedInterruptedAdminBuiltinRun plants a running admin-builtin run without a key.
func (h *recoveryOwnerHarness) seedInterruptedAdminBuiltinRun(agent string) string {
	h.t.Helper()
	const runID = "run-crashed-admin-1"
	_, err := h.db.ExecContext(h.t.Context(), `
		INSERT INTO runs (run_id, agent, channel, user_id, trigger, status, started_at,
		                  caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, $2, 'scheduler', 'scheduler', 'cron', 'running', $3,
		        'local-admin', 'admin-builtin', NULL, 'scheduler')`,
		runID, agent, h.now.Add(-time.Minute).UnixMilli())
	require.NoError(h.t, err)
	return runID
}

// TestRecovery_AdminBuiltinRunUsesAdminPrincipal 守 C-32 的第二类授权来源：
// `authorizing_kind='admin-builtin'` 的运行没有、也不需要授权 Key，收尾必须保留
// 内置管理员身份并写通用中断原因——**不得**因为"查不到 key"被判成归属失效。
// 逆改点：把 caller-key 的 Key 复核条件扩大到所有行（去掉 `authorizing_kind` 判定）
// → reason 变成 `owner_unauthorized`，Builtin / KeyID 断言与"原因不含归属失效"同时红。
func TestRecovery_AdminBuiltinRunUsesAdminPrincipal(t *testing.T) {
	h := newRecoveryOwnerHarness(t)
	h.requireAdminBuiltinCaller()
	runID := h.seedInterruptedAdminBuiltinRun("demo")

	h.recover()

	got := h.lastRunPrincipal(runID)
	require.Equal(t, core.PrincipalLocalAdmin, got.Kind, "admin-builtin 的身份是内置管理员")
	require.True(t, got.Builtin, "不得降级成普通调用方")
	require.Equal(t, "local-admin", got.CallerID, "归属逐字保留")
	require.Empty(t, got.KeyID, "内置管理员没有授权 Key，收尾也不得补一把")
	require.Equal(t, core.ActorRecovery, got.Actor, "执行者与归属分开记")
	require.Equal(t, core.StatusFailed, h.runStatus(runID), "崩溃运行收尾为 failed")
	require.NotContains(t, h.runReason(runID), "owner_unauthorized",
		"内置管理员没有 Key 属正常形态，不是归属失效")
	require.Equal(t, "local-admin", h.usageCaller(runID), "用量按归属记，不记执行者")
}

// TestRecovery_RevokedAuthorizingKeyNotSubstituted 守 C-32 的另一半：归属正确不等于
// 授权仍在，且**不得**改用同一调用方的另一把 Key。运行绑定 key-a，key-b 同样有效且
// 权限更全——恢复只认 `runs.key_id` 指向的那一把。
// 逆改点：复核按 `(caller_id, 任意 key)` 放宽到"只要有一把有效就放行" →
// reason 不再含 owner_unauthorized、key_id 被改写，两条断言同时红。
func TestRecovery_RevokedAuthorizingKeyNotSubstituted(t *testing.T) {
	h := newRecoveryOwnerHarness(t)
	runID := h.seedCallerWithKey("caller-1", "demo")
	h.seedKey("caller-1", "key-b", false, "sessions:read", core.AgentInvokeScopePrefix+"demo")
	h.revokeKey("caller-1", "key-a")

	h.recover()

	require.Equal(t, core.StatusFailed, h.runStatus(runID))
	require.Contains(t, h.runReason(runID), "owner_unauthorized",
		"原授权 Key 撤销即归属失效，哪怕同调用方还有别的有效 Key")
	got := h.lastRunPrincipal(runID)
	require.Equal(t, "key-a", got.KeyID, "审计留存的是原授权 Key，不得换成 key-b")
	require.Equal(t, "caller-1", got.CallerID)
	require.Equal(t, core.ActorRecovery, got.Actor, "授权失败不改变执行者标注")
	require.Equal(t, "caller-1", h.usageCaller(runID), "用量仍记归属调用方")
}
