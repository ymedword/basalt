//go:build integration

package callers_test

// 本文件覆盖 T023 存储侧（`postgres.NewQuotaStore`）与 `Limiter.AdmitCall` 的接线
// （颗粒度文档 §2.3 图 6 [5] / C-29 / C-35 / §4.4 配额用例）：并发预留的原子性、
// 同 Run 多调用的独立键、重复提交不双计、跨周期按预留周期记账、超预留阻止后续、
// 归属校验、多 Key 共享额度、超时重发换键、未知用量保持 pending、内置管理员零预留。
// 服务级（RunService / 审计失败）三段留 T090 / T025。
// These tests pin the PostgreSQL quota store: atomic reservation under concurrency,
// per-call keys, idempotent settlement, cross-period booking, overage gating, caller
// ownership, shared per-caller limits, pending-on-unknown and the admin no-row path.

import (
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// quotaHarness 是一个隔离 schema 上的配额存储句柄。
// quotaHarness binds a quota store to one isolated schema.
type quotaHarness struct {
	db *postgres.DB
	q  callers.QuotaStore
}

// newQuotaHarness 建隔离库并返回配额存储（每个用例独立 schema，可并行）。
// newQuotaHarness builds an isolated database per test.
func newQuotaHarness(t *testing.T) *quotaHarness {
	t.Helper()
	db := pgtest.NewTestDB(t)
	return &quotaHarness{db: db, q: postgres.NewQuotaStore(db)}
}

// setLimit 写入该调用方的 Token 周期额度（其余维度保持 0）。
// setLimit writes a token period limit; other ledgers stay at zero.
func (h *quotaHarness) setLimit(t *testing.T, callerID string, tokens int64) {
	t.Helper()
	require.NoError(t, h.q.SetLimit(t.Context(), callerID, callers.Limit{
		TokensPerPeriod: tokens,
		Period:          callers.PeriodDay,
		Timezone:        "UTC",
	}))
}

// reserve 预留一次 Chat 调用并断言成功。
// reserve reserves one chat call and requires success.
func (h *quotaHarness) reserve(t *testing.T, callerID, idem string, tokens int64) callers.Reservation {
	t.Helper()
	reserved, err := h.q.Reserve(t.Context(), callerID, callers.ClassChat, callers.Cost{Tokens: tokens}, idem)
	require.NoError(t, err)
	return reserved
}

// usageRow 读取 tokens 账本的三列用量；行不存在按全零处理（未记账即 0）。
// usageRow reads the token ledger; a missing row means nothing was booked.
func usageRow(t *testing.T, db *postgres.DB, callerID string, periodStart int64) (used, reserved int64) {
	t.Helper()
	used, reserved, _ = usageRow3(t, db, callerID, periodStart)
	return used, reserved
}

// usageRow3 读取 tokens 账本的 used / reserved / overage。
// usageRow3 reads used, reserved and overage from the token ledger.
func usageRow3(t *testing.T, db *postgres.DB, callerID string, periodStart int64) (used, reserved, overage int64) {
	t.Helper()
	err := db.QueryRowContext(t.Context(), `
		SELECT used, reserved, overage
		FROM caller_usage_periods
		WHERE caller_id = $1 AND period_start = $2 AND kind = 'tokens'`,
		callerID, periodStart,
	).Scan(&used, &reserved, &overage)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0
	}
	require.NoError(t, err)
	return used, reserved, overage
}

// seedPendingReservation 直接按 SQL 种一条 pending 预留与对应用量行：时间相关的
// 用例不注入时钟（R28.5），而是先造出跨周期状态，再用真实时钟结算。
// seedPendingReservation plants a pending reservation in an arbitrary period.
func seedPendingReservation(t *testing.T, db *postgres.DB, id, callerID, idem string, periodStart, amount int64) {
	t.Helper()
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', 'active', $2)
		ON CONFLICT DO NOTHING`, callerID, time.Now().UTC().UnixMilli())
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO quota_reservations
			(reservation_id, caller_id, idempotency_key, class, period_start, amount, state, created_at)
		VALUES ($1, $2, $3, 'chat', $4, $5, 'pending', $6)`,
		id, callerID, idem, periodStart, amount, time.Now().UTC().UnixMilli())
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO caller_usage_periods
			(caller_id, period_start, kind, used, reserved, overage, updated_at)
		VALUES ($1, $2, 'tokens', 0, $3, 0, $4)
		ON CONFLICT (caller_id, period_start, kind) DO UPDATE
		SET reserved = caller_usage_periods.reserved + EXCLUDED.reserved`,
		callerID, periodStart, amount, time.Now().UTC().UnixMilli())
	require.NoError(t, err)
}

// TestQuota_ReserveIsAtomicUnderConcurrency 判并发预留不超发：额度 100、8 个并发
// 各预留 30 → 恰好 3 个成功，账本 reserved=90，恰好 3 条预留行。
// 逆改点：把条件累加拆成"先 SELECT 再 UPDATE" → 成功数 > 3，断言红。
func TestQuota_ReserveIsAtomicUnderConcurrency(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 100)

	const attempts = 8
	var granted, denied atomic.Int64
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.q.Reserve(t.Context(), "c1", callers.ClassChat, callers.Cost{Tokens: 30},
				"run:r1:chat:s"+string(rune('a'+i))+":a1")
			switch {
			case err == nil:
				granted.Add(1)
			case errors.Is(err, callers.ErrQuotaExhausted):
				denied.Add(1)
			default:
				t.Errorf("并发预留出现非预期错误: %v", err)
			}
		}()
	}
	wg.Wait()

	require.Equal(t, int64(3), granted.Load(), "100 额度只装得下 3 份 30")
	require.Equal(t, int64(attempts-3), denied.Load())

	var rows int
	require.NoError(t, h.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM quota_reservations WHERE caller_id = 'c1'`).Scan(&rows))
	require.Equal(t, 3, rows, "被拒请求不得留下预留行")

	_, reserved := usageRow(t, h.db, "c1", firstPeriodStart(t, h, "c1"))
	require.Equal(t, int64(90), reserved)
}

// firstPeriodStart 取该调用方账本里唯一的周期起点（用例只用一个周期）。
// firstPeriodStart returns the single period start present in the ledger.
func firstPeriodStart(t *testing.T, h *quotaHarness, callerID string) int64 {
	t.Helper()
	var start int64
	require.NoError(t, h.db.QueryRowContext(t.Context(),
		`SELECT DISTINCT period_start FROM caller_usage_periods WHERE caller_id = $1`, callerID).Scan(&start))
	return start
}

// TestQuota_MultipleCallsPerRunHaveDistinctKeys 判同一 Run 的多次调用各占一条预留；
// 同一幂等键重复预留返回同一条，不新增行。
// 逆改点：唯一键仍是 (caller_id, run_id, kind) → 第二次预留冲突。
func TestQuota_MultipleCallsPerRunHaveDistinctKeys(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)

	a := h.reserve(t, "c1", "run:r1:chat:s1:a1", 100)
	b := h.reserve(t, "c1", "run:r1:chat:s2:a1", 100)
	require.NotEqual(t, a.ID, b.ID, "同一 Run 的第二次调用必须能独立预留")

	again, err := h.q.Reserve(t.Context(), "c1", callers.ClassChat, callers.Cost{Tokens: 100}, "run:r1:chat:s1:a1")
	require.NoError(t, err)
	require.Equal(t, a.ID, again.ID, "同一幂等键返回同一预留")

	_, reserved := usageRow(t, h.db, "c1", a.PeriodStart)
	require.Equal(t, int64(200), reserved, "重复预留不得重复占额度")
}

// TestQuota_IdempotentRetryNotDoubleCharged 判重复提交不双计：两次相同 Commit 只记
// 一次 used，且余额自动释放。
// 逆改点：Commit 每次累加、或按当前时间重算周期 → 断言红。
func TestQuota_IdempotentRetryNotDoubleCharged(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)
	r := h.reserve(t, "c1", "run:r1:chat:s1:a1", 100)

	require.NoError(t, commitTokens(t, h, r, 80))
	require.NoError(t, commitTokens(t, h, r, 80)) // 网络重试

	used, reserved := usageRow(t, h.db, "c1", r.PeriodStart)
	require.Equal(t, int64(80), used, "重复 Commit 不得重复计数")
	require.Zero(t, reserved, "Commit 后余额自动释放")
}

// commitTokens 用 Token 用量结算一条预留。
// commitTokens settles one reservation with a token usage.
func commitTokens(t *testing.T, h *quotaHarness, r callers.Reservation, tokens int64) error {
	t.Helper()
	_, err := h.q.Commit(t.Context(), r.CallerID, r.ID, callers.Usage{Tokens: tokens})
	return err
}

// TestQuota_CommitAfterPeriodRolloverUsesReservationPeriod 判跨周期提交：预留记在
// 创建时的周期，Commit **总是**更新那一行，**不得**按当前时间重算周期。
// 逆改点：结算取当前周期 → 上一周期行 reserved 不释放、当前周期冒出 used。
func TestQuota_CommitAfterPeriodRolloverUsesReservationPeriod(t *testing.T) {
	h := newQuotaHarness(t)
	ctx := t.Context()

	// 上一周期（UTC 日）：用 SQL 造出 pending 预留与用量行，再用当前时刻提交。
	prev := time.Now().UTC().Add(-24 * time.Hour).Truncate(24 * time.Hour)
	next, err := callers.NextPeriodStart(callers.PeriodDay, "UTC", prev)
	require.NoError(t, err)
	require.True(t, !time.Now().UTC().Before(next), "构造前提：当前时刻已跨入下一周期")

	seedPendingReservation(t, h.db, "qres_tokens_0123456789ABCDEFGHJKMNPQRS", "c1", "run:r1:chat:s1:a1",
		prev.UnixMilli(), 100)

	_, err = h.q.Commit(ctx, "c1", "qres_tokens_0123456789ABCDEFGHJKMNPQRS", callers.Usage{Tokens: 60})
	require.NoError(t, err)

	usedP1, _ := usageRow(t, h.db, "c1", prev.UnixMilli())
	usedP2, _ := usageRow(t, h.db, "c1", next.UnixMilli())
	require.Equal(t, int64(60), usedP1, "必须记在预留所属周期")
	require.Zero(t, usedP2, "不得记到当前周期")
}

// TestQuota_OverageBlocksNextReserve 判超预留：actual > amount 如实记 overage，
// 不截断、不回滚调用；overage 未清前新预留一律 429。
// 逆改点：把 actual 截成 amount → overage 为 0，下一次预留成功。
func TestQuota_OverageBlocksNextReserve(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)
	r := h.reserve(t, "c1", "run:r1:chat:s1:a1", 100)
	require.NoError(t, commitTokens(t, h, r, 130))

	_, _, overage := usageRow3(t, h.db, "c1", r.PeriodStart)
	require.Equal(t, int64(30), overage, "超预留必须如实记 overage")

	_, err := h.q.Reserve(t.Context(), "c1", callers.ClassChat, callers.Cost{Tokens: 1}, "run:r1:chat:s2:a1")
	require.ErrorIs(t, err, callers.ErrQuotaExhausted, "overage 未清前不得再预留")
}

// TestQuota_ClearOverageResumesReserve 判超限清零后恢复：清零要写明理由，恢复后同一
// 调用方能再次预留（且不产生负额度）。
// 逆改点：清零时连 used 一起抹掉 → 恢复后剩余额度变成 900，后续断言红。
func TestQuota_ClearOverageResumesReserve(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)
	r := h.reserve(t, "c1", "run:r1:chat:s1:a1", 100)
	require.NoError(t, commitTokens(t, h, r, 130))

	require.Error(t, h.q.ClearOverage(t.Context(), "c1", r.PeriodStart, "  "), "清零必须写明理由")
	require.NoError(t, h.q.ClearOverage(t.Context(), "c1", r.PeriodStart, "管理员核对为供应商计价错误"))

	used, reserved, overage := usageRow3(t, h.db, "c1", r.PeriodStart)
	require.Equal(t, int64(130), used, "已发生的用量不因清零而回滚")
	require.Zero(t, reserved)
	require.Zero(t, overage)

	next := h.reserve(t, "c1", "run:r1:chat:s2:a1", 100)
	require.NotEmpty(t, next.ID, "清零后必须能再次预留")
}

// TestQuota_IdempotencyKeyConflictRejected 判同键换参数：class / 成本任何一个不同
// 都返回冲突，且不新增预留、不改动额度。
// 逆改点：静默复用旧预留 → 冲突断言失败；新建第二行 → 额度翻倍。
func TestQuota_IdempotencyKeyConflictRejected(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)
	r := h.reserve(t, "c1", "run:r1:chat:s1:a1", 100)

	_, err := h.q.Reserve(t.Context(), "c1", callers.ClassChat, callers.Cost{Tokens: 130}, "run:r1:chat:s1:a1")
	require.ErrorIs(t, err, callers.ErrIdempotencyConflict, "同键换成本不得静默复用")

	_, err = h.q.Reserve(t.Context(), "c1", callers.ClassCompaction, callers.Cost{Tokens: 100}, "run:r1:chat:s1:a1")
	require.ErrorIs(t, err, callers.ErrIdempotencyConflict, "同键换 class 同样冲突")

	used, reserved := usageRow(t, h.db, "c1", r.PeriodStart)
	require.Zero(t, used)
	require.Equal(t, int64(100), reserved, "冲突请求不得新增预留")
}

// TestQuota_EmbeddingRetryAfterTimeoutGetsNewKey 判「重发 = 新调用」：超时那次保持
// pending 继续占额度，只有确认未发出的重发才允许 Release。
// 逆改点：重发复用旧键 → 唯一键挡住第二次预留（断言失败）；超时也 Release →
// reserved 变 100 而不是 200。
func TestQuota_EmbeddingRetryAfterTimeoutGetsNewKey(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)

	first, err := h.q.Reserve(t.Context(), "c1", callers.ClassIngest, callers.Cost{Tokens: 100},
		"ingest:job-1:embed:attempt-1:batch-0")
	require.NoError(t, err)
	second, err := h.q.Reserve(t.Context(), "c1", callers.ClassIngest, callers.Cost{Tokens: 100},
		"ingest:job-1:embed:attempt-2:batch-0")
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)

	_, reserved := usageRow(t, h.db, "c1", first.PeriodStart)
	require.Equal(t, int64(200), reserved, "两次实际发出必须各占一份预留")

	require.NoError(t, h.q.Release(t.Context(), "c1", second.ID, callers.ReleaseNotSent))
	_, reserved = usageRow(t, h.db, "c1", first.PeriodStart)
	require.Equal(t, int64(100), reserved, "未发出的重发可释放，超时那次仍占额度")
}

// TestQuota_UnknownUsageStaysPending 判未知用量：不提交就一直是 pending、继续占额度，
// 换一个存储实例（模拟重启）也不会被回收，只能靠管理面核对。
// 逆改点：进程启动或读路径顺手释放 pending → ListPending 为空、reserved 归零。
func TestQuota_UnknownUsageStaysPending(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)
	r := h.reserve(t, "c1", "run:r1:chat:s1:a1", 100)

	restarted := postgres.NewQuotaStore(h.db)
	pending, err := restarted.ListPending(t.Context(), "c1")
	require.NoError(t, err)
	require.Len(t, pending, 1, "重启不得自动释放结局未知的预留")
	require.Equal(t, r.ID, pending[0].ID)

	_, reserved := usageRow(t, h.db, "c1", r.PeriodStart)
	require.Equal(t, int64(100), reserved, "pending 继续占额度")

	_, err = h.q.Commit(t.Context(), "c1", r.ID, callers.Usage{Tokens: 100})
	require.NoError(t, err, "管理员核对后按 commit 收口")
	pending, err = h.q.ListPending(t.Context(), "c1")
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestQuota_CrossCallerReservationRejected 判归属校验：只凭 reservation_id 结算
// 一律按未找到处理，不泄露预留是否存在，也不改动任何额度。
// 逆改点：去掉 `AND caller_id` → 他人可结算，断言红。
func TestQuota_CrossCallerReservationRejected(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)
	h.setLimit(t, "c2", 1000)
	r := h.reserve(t, "c1", "run:r1:chat:s1:a1", 100)

	_, err := h.q.Commit(t.Context(), "c2", r.ID, callers.Usage{Tokens: 100})
	require.ErrorIs(t, err, callers.ErrNotFound)

	require.ErrorIs(t, h.q.Release(t.Context(), "c2", r.ID, callers.ReleaseNotSent), callers.ErrNotFound)

	used, reserved := usageRow(t, h.db, "c1", r.PeriodStart)
	require.Zero(t, used, "越权结算不得改动账本")
	require.Equal(t, int64(100), reserved)
}

// TestQuota_MultiKeySharedAccountLimit 判额度按调用方计：同一调用方的两把 Key
// （同一 CallerID 的不同幂等键）共享同一本账，`caller_keys` 的轮换不重置周期。
// 逆改点：账本主键改成 (caller_id, key_id, …) → 第二笔预留不再互斥。
func TestQuota_MultiKeySharedAccountLimit(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 100)

	require.NotEmpty(t, h.reserve(t, "c1", "run:r1:chat:s1:a1", 60).ID)
	_, err := h.q.Reserve(t.Context(), "c1", callers.ClassChat, callers.Cost{Tokens: 60}, "run:r1:chat:s2:a1")
	require.ErrorIs(t, err, callers.ErrQuotaExhausted, "同一调用方的两把 Key 共享额度")
}

// TestQuota_LocalAdminUnlimitedNoReservation 判内置管理员不限量：真实存储下
// `AdmitCall` 不写任何预留 / 用量行，结算为空操作。
// 逆改点：把不限量也送去 Reserve → 预留行计数为 1，断言红。
func TestQuota_LocalAdminUnlimitedNoReservation(t *testing.T) {
	h := newQuotaHarness(t)
	limiter := callers.NewLimiter(callers.LimiterDeps{Quotas: h.q, LocalAdminUnlimited: true})
	admin := core.Principal{
		Kind:     core.PrincipalLocalAdmin,
		CallerID: core.LocalAdminCallerID,
		Builtin:  true,
		Actor:    core.ActorCLIAdmin,
	}

	ticket, err := limiter.AdmitCall(t.Context(), admin, callers.ClassChat, callers.Cost{Tokens: 10}, "run:r1:chat:s1:a1")
	require.NoError(t, err)
	require.True(t, ticket.NoReservation)
	require.NoError(t, limiter.Release(t.Context(), ticket, callers.ReleaseZeroUsage))

	var reservations, usageRows int
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM quota_reservations`).Scan(&reservations))
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM caller_usage_periods`).Scan(&usageRows))
	require.Zero(t, reservations, "不限量路径不得写预留行")
	require.Zero(t, usageRows, "不限量路径不得写用量行")
}

// TestLimiter_MultipleCallsShareOneRunSlot 判 C-35 的三段边界：同一 Run 的两次模型
// 调用共用**一个**并发名额——`AdmitCall` 不得再 Acquire 一次。
// 逆改点：在 AdmitCall 里调 concurrency.Acquire → 第二次调用撞满名额，断言红。
func TestLimiter_MultipleCallsShareOneRunSlot(t *testing.T) {
	h := newQuotaHarness(t)
	h.setLimit(t, "c1", 1000)

	concurrency, err := callers.NewMemoryConcurrency(func(string) (int, bool) { return 1, true })
	require.NoError(t, err)
	limiter := callers.NewLimiter(callers.LimiterDeps{Quotas: h.q, Concurrency: concurrency})

	principal := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "c1",
		KeyID:    "key-1",
		Scopes:   []string{core.ScopeSessionsRead},
		Actor:    core.ActorCaller,
	}

	runTicket, err := limiter.AdmitRun(t.Context(), principal)
	require.NoError(t, err)
	defer func() { require.NoError(t, limiter.ReleaseRun(t.Context(), runTicket)) }()

	for _, idem := range []string{"run:r1:chat:s1:a1", "run:r1:chat:s2:a1"} {
		ticket, err := limiter.AdmitCall(t.Context(), principal, callers.ClassChat, callers.Cost{Tokens: 10}, idem)
		require.NoError(t, err, "同一 Run 的多次调用不得再抢并发名额")
		_, err = limiter.Commit(t.Context(), ticket, callers.Usage{Tokens: 5})
		require.NoError(t, err)
	}
}
