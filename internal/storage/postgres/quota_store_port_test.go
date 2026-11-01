//go:build integration

package postgres

// 本文件是 `callers.QuotaStore` PostgreSQL 实现的**包级**判卷（US-16 新增实现：
// `internal/storage/postgres/quota_store.go` 无 SQLite 旧用例可移植，按实现契约新写）。
//
// 覆盖口径（图 6 [5] / C-29 / R28 系列）：
//
//   - Reserve / Commit / Release / Reconcile / ClearOverage / SetLimit /
//     MaxConcurrent / ListPending / Get 的往返与错误路径各有用例，断言到哨兵错误
//     或可读文案；
//   - 限额缺行、对应账本额度 <= 0 时 fail-closed（`ErrQuotaExhausted`），**不得**
//     退化成不限量（`quota_store.go:581-626`）；
//   - 同一幂等键重复 Reserve：参数一致返回既有预留，换 class / 成本返回
//     `ErrIdempotencyConflict`（依据 `quota_store.go:84-85 / 116-141` 的实现注释：
//     先写预留行、命中 `(caller_id, idempotency_key)` 唯一键后回读旧行比对参数）；
//   - Release 归还额度、Commit 的 usage 记账（overage 不截断）、Reconcile 对
//     pending / committed / released / reconciled 四态各自的结局；
//   - 周期边界：`caller_limits.period/timezone` 与该次调用的句柄时钟共同决定周期
//     起点，结算永远记在预留创建时的周期（`quota_store.go:586-626 / 210-235`）。
//
// 夹具：`portTestDB` 提供已迁移的隔离 schema；本文件把句柄时钟钉在 `opNow`（与
// SQLite 谱系同一固定时刻），BIGINT Unix 毫秒时间列与周期起止因此可复算。
// Package-level cases for the PostgreSQL quota store: every documented round trip and
// error path, under a fixed handle clock. Assertions are never relaxed.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
)

// quotaUTCDayStartMS 是固定时钟（opNow）在 UTC 日周期下的起点：2026-10-21T00:00:00Z。
// quotaUTCDayStartMS is the UTC day start containing opNow.
var quotaUTCDayStartMS = time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC).UnixMilli()

// quotaTestDB 返回已迁移的隔离句柄，并把句柄时钟固定在 opNow——周期边界与时间列
// 断言都要可复算（共享夹具只给句柄，不注入时钟）。
// quotaTestDB returns a migrated isolated handle with the clock pinned to opNow.
func quotaTestDB(t *testing.T) *DB {
	t.Helper()
	db := portTestDB(t)
	db.now = func() time.Time { return opNow }
	return db
}

// quotaSetLimit 写入限额并返回被测存储（SetLimit 顺带满足 callers 外键）。
// quotaSetLimit writes a limit and returns the store under test.
func quotaSetLimit(t *testing.T, db *DB, callerID string, limit callers.Limit) *quotaStore {
	t.Helper()
	store := NewQuotaStore(db)
	require.NoError(t, store.SetLimit(context.Background(), callerID, limit))
	return store
}

// quotaReserve 预留一次 Token 消耗（Chat 类别）并断言成功。
// quotaReserve reserves one chat token call and requires success.
func quotaReserve(t *testing.T, store *quotaStore, callerID, idem string, tokens int64) callers.Reservation {
	t.Helper()
	return quotaReserveCost(t, store, callerID, callers.ClassChat, callers.Cost{Tokens: tokens}, idem)
}

// quotaReserveCost 按给定类别与成本预留一次并断言成功。
// quotaReserveCost reserves one call with an explicit class and cost.
func quotaReserveCost(t *testing.T, store *quotaStore, callerID string, class callers.RequestClass, cost callers.Cost, idem string) callers.Reservation {
	t.Helper()
	reserved, err := store.Reserve(context.Background(), callerID, class, cost, idem)
	require.NoError(t, err)
	return reserved
}

// quotaLedgerRow 读某账本行的 used / reserved / overage；行不存在按全零返回
// （未记账 = 0，与 SQLite 谱系同口径）。
// quotaLedgerRow reads one usage row; a missing row reads as all zeros.
func quotaLedgerRow(t *testing.T, db *DB, callerID, kind string, periodStart int64) (used, reserved, overage int64) {
	t.Helper()
	err := db.pool.QueryRowContext(context.Background(), `
		SELECT used, reserved, overage
		FROM caller_usage_periods
		WHERE caller_id = $1 AND period_start = $2 AND kind = $3`,
		callerID, periodStart, kind,
	).Scan(&used, &reserved, &overage)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0
	}
	require.NoError(t, err)
	return used, reserved, overage
}

// quotaSeedCaller 直接种一行调用方（不建 caller_limits 行）：用于"缺限额行"与
// 绕过 SetLimit 的边界用例。
// quotaSeedCaller inserts a caller row without any limit row.
func quotaSeedCaller(t *testing.T, db *DB, callerID, kind string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, $2, $1, '', 'active', $3)
		ON CONFLICT DO NOTHING`, callerID, kind, opNow.UnixMilli())
	require.NoError(t, err)
}

// quotaReservationCount 数某调用方的预留行数。
// quotaReservationCount counts reservation rows for one caller.
func quotaReservationCount(t *testing.T, db *DB, callerID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM quota_reservations WHERE caller_id = $1`, callerID).Scan(&n))
	return n
}

// quotaUsageRowCount 数某调用方的用量行数（预留被拒时必须为零）。
// quotaUsageRowCount counts usage rows for one caller.
func quotaUsageRowCount(t *testing.T, db *DB, callerID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM caller_usage_periods WHERE caller_id = $1`, callerID).Scan(&n))
	return n
}

// quotaCallerRowCount 数调用方行（判 SetLimit 的补行是否幂等）。
// quotaCallerRowCount counts caller rows for one id.
func quotaCallerRowCount(t *testing.T, db *DB, callerID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM callers WHERE caller_id = $1`, callerID).Scan(&n))
	return n
}

// requireReservationLedger 断言预留标识自描述账本：前缀逐字、载荷可解析回同一维度。
// requireReservationLedger checks the id prefix and that it parses back to the kind.
func requireReservationLedger(t *testing.T, id, prefix, kind string) {
	t.Helper()
	require.True(t, strings.HasPrefix(id, prefix), "预留标识 %q 必须以 %q 开头", id, prefix)
	parsed, err := parseReservationKind(id)
	require.NoError(t, err)
	require.Equal(t, kind, parsed)
}

// TestQuotaStore_ReserveCommitRoundTrip 判一次完整的预留—结算往返：预留行先落库
// （pending）并占额度，Get 与 ListPending 读回同一行；Commit 把 reserved 转为 used、
// 状态变 committed、从 ListPending 消失；时间列是 Unix 毫秒。
// 逆改点：Commit 只改预留行不改周期行 → used 断言红；ListPending 不带 state 过滤 →
// 结算后的行仍在列表里。
// 中文验收点：预留—结算往返与状态迁移。
func TestQuotaStore_ReserveCommitRoundTrip(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_roundtrip", callers.Limit{TokensPerPeriod: 1000})

	reserved := quotaReserve(t, store, "c_roundtrip", "run:r1:chat:s1:a1", 200)
	requireReservationLedger(t, reserved.ID, reservationIDPrefix+usageKindTokens+"_", usageKindTokens)
	require.Equal(t, "c_roundtrip", reserved.CallerID)
	require.Equal(t, callers.ClassChat, reserved.Class)
	require.Equal(t, quotaUTCDayStartMS, reserved.PeriodStart, "日周期按 UTC 本地 00:00 起算")
	require.Equal(t, int64(200), reserved.Amount)
	require.Equal(t, callers.ReservationPending, reserved.Status)

	used, held, over := quotaLedgerRow(t, db, "c_roundtrip", usageKindTokens, reserved.PeriodStart)
	require.Zero(t, used)
	require.Equal(t, int64(200), held, "预留必须先占额度")
	require.Zero(t, over)

	var (
		createdAt int64
		state     string
	)
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT created_at, state FROM quota_reservations WHERE reservation_id = $1`, reserved.ID,
	).Scan(&createdAt, &state))
	require.Equal(t, opNow.UnixMilli(), createdAt, "时间列是 BIGINT Unix 毫秒")
	require.Equal(t, "pending", state)

	got, err := store.Get(ctx, "c_roundtrip", reserved.ID)
	require.NoError(t, err)
	require.Equal(t, reserved, got)

	pending, err := store.ListPending(ctx, "c_roundtrip")
	require.NoError(t, err)
	require.Equal(t, []callers.Reservation{reserved}, pending)

	settlement, err := store.Commit(ctx, "c_roundtrip", reserved.ID, callers.Usage{Tokens: 120})
	require.NoError(t, err)
	require.Equal(t, callers.Settlement{
		ReservationID: reserved.ID,
		CallerID:      "c_roundtrip",
		Class:         callers.ClassChat,
		State:         callers.ReservationCommitted,
		PeriodStart:   reserved.PeriodStart,
		Amount:        200,
		Actual:        120,
		Overage:       0,
	}, settlement)

	used, held, over = quotaLedgerRow(t, db, "c_roundtrip", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(120), used)
	require.Zero(t, held, "Commit 释放预留")
	require.Zero(t, over)

	var (
		actual   int64
		settled  sql.NullInt64
		overage2 int64
	)
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT actual, settled_at, overage FROM quota_reservations WHERE reservation_id = $1`, reserved.ID,
	).Scan(&actual, &settled, &overage2))
	require.Equal(t, int64(120), actual)
	require.True(t, settled.Valid)
	require.Equal(t, opNow.UnixMilli(), settled.Int64)
	require.Zero(t, overage2)

	got, err = store.Get(ctx, "c_roundtrip", reserved.ID)
	require.NoError(t, err)
	require.Equal(t, callers.ReservationCommitted, got.Status)

	pending, err = store.ListPending(ctx, "c_roundtrip")
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestQuotaStore_ReserveFailsClosedWithoutUsableLimit 判 fail-closed：caller_limits
// 缺行、或对应账本的周期额度 <= 0，一律 `ErrQuotaExhausted`，**不得**退化成不限量；
// 被拒的预留不留行、不建用量行（预留行先写、额度再占，回滚必须干净）。
// 逆改点：缺行时把 limit 当 0 跳过条件更新 → 预留成功、余额不受限。
// 中文验收点：缺限额或零额度必须失败关闭。
func TestQuotaStore_ReserveFailsClosedWithoutUsableLimit(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()

	t.Run("missing_limit_row", func(t *testing.T) {
		quotaSeedCaller(t, db, "c_nolimit", "service")
		store := NewQuotaStore(db)
		_, err := store.Reserve(ctx, "c_nolimit", callers.ClassChat, callers.Cost{Tokens: 1}, "k-missing")
		require.ErrorIs(t, err, callers.ErrQuotaExhausted, "没有限额行必须失败关闭")
		require.Zero(t, quotaReservationCount(t, db, "c_nolimit"), "被拒预留不得留行")
		require.Zero(t, quotaUsageRowCount(t, db, "c_nolimit"), "被拒预留不得建用量行")
	})

	t.Run("zero_ledger_limit", func(t *testing.T) {
		store := quotaSetLimit(t, db, "c_zero", callers.Limit{TokensPerPeriod: 0})
		_, err := store.Reserve(ctx, "c_zero", callers.ClassChat, callers.Cost{Tokens: 1}, "k-zero")
		require.ErrorIs(t, err, callers.ErrQuotaExhausted, "额度为 0 必须失败关闭")
		require.Zero(t, quotaReservationCount(t, db, "c_zero"))
	})

	t.Run("limit_is_per_ledger", func(t *testing.T) {
		store := quotaSetLimit(t, db, "c_perledger", callers.Limit{TokensPerPeriod: 50, TasksPerPeriod: 0})
		_, err := store.Reserve(ctx, "c_perledger", callers.ClassRun, callers.Cost{Tasks: 1}, "k-run")
		require.ErrorIs(t, err, callers.ErrQuotaExhausted, "任务账本额度为 0 时不得借用 Token 额度")
		// 同一调用方的 Token 账本仍可用：拒绝来自本账本，不是整行限额。
		reserved := quotaReserve(t, store, "c_perledger", "k-chat", 10)
		require.Equal(t, int64(10), reserved.Amount)
	})
}

// TestQuotaStore_ReserveIdempotencyKey 判同一幂等键的重复 Reserve：参数一致时返回
// 同一条既有预留（不新增行、不再占额度），换 class 或成本一律
// `ErrIdempotencyConflict`。依据实现注释（quota_store.go:84-85 / 116-141）：先写
// 预留行、命中 `(caller_id, idempotency_key)` 唯一键后回读旧行比对参数；结算幂等，
// 因此回放返回的是该行的**当前**状态（这里断言 committed 之后仍是同一条）。
// 逆改点：回放时直接新建第二行 → 行数与额度翻倍；只比 class 不比 amount → 冲突漏判。
// 中文验收点：幂等键重放收敛到同一条预留。
func TestQuotaStore_ReserveIdempotencyKey(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_idem", callers.Limit{TokensPerPeriod: 1000})

	first := quotaReserve(t, store, "c_idem", "run:r1:chat:s1:a1", 100)
	again, err := store.Reserve(ctx, "c_idem", callers.ClassChat, callers.Cost{Tokens: 100}, "run:r1:chat:s1:a1")
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID, "参数一致返回同一预留")
	require.Equal(t, callers.ReservationPending, again.Status)
	require.Equal(t, 1, quotaReservationCount(t, db, "c_idem"))
	used, held, _ := quotaLedgerRow(t, db, "c_idem", usageKindTokens, first.PeriodStart)
	require.Zero(t, used)
	require.Equal(t, int64(100), held, "重复预留不得重复占额度")

	_, err = store.Reserve(ctx, "c_idem", callers.ClassChat, callers.Cost{Tokens: 130}, "run:r1:chat:s1:a1")
	require.ErrorIs(t, err, callers.ErrIdempotencyConflict, "同键换成本不得静默复用")

	_, err = store.Reserve(ctx, "c_idem", callers.ClassCompaction, callers.Cost{Tokens: 100}, "run:r1:chat:s1:a1")
	require.ErrorIs(t, err, callers.ErrIdempotencyConflict, "同键换 class 同样冲突")

	require.Equal(t, 1, quotaReservationCount(t, db, "c_idem"), "冲突请求不得新增预留")
	used, held, _ = quotaLedgerRow(t, db, "c_idem", usageKindTokens, first.PeriodStart)
	require.Zero(t, used)
	require.Equal(t, int64(100), held, "冲突请求不得改动额度")

	// 提交后同键回放：返回同一预留的当前状态，不重复记账。
	_, err = store.Commit(ctx, "c_idem", first.ID, callers.Usage{Tokens: 60})
	require.NoError(t, err)
	replayed, err := store.Reserve(ctx, "c_idem", callers.ClassChat, callers.Cost{Tokens: 100}, "run:r1:chat:s1:a1")
	require.NoError(t, err)
	require.Equal(t, first.ID, replayed.ID)
	require.Equal(t, callers.ReservationCommitted, replayed.Status, "回放返回既有行的当前状态")
	require.Equal(t, 1, quotaReservationCount(t, db, "c_idem"))
	used, held, _ = quotaLedgerRow(t, db, "c_idem", usageKindTokens, first.PeriodStart)
	require.Equal(t, int64(60), used)
	require.Zero(t, held)

	// 幂等键的作用域是调用方：别的调用方用同一个键是另一条预留。
	other := quotaSetLimit(t, db, "c_idem_other", callers.Limit{TokensPerPeriod: 1000})
	require.NotEqual(t, first.ID, quotaReserve(t, other, "c_idem_other", "run:r1:chat:s1:a1", 100).ID)
}

// TestQuotaStore_ReserveIsAtomicUnderConcurrency 判条件累加的原子性：额度 100、
// 8 个并发各预留 30 → 恰好 3 个成功，账本 reserved=90，恰好 3 条预留行。
// 逆改点：把"先 SELECT 再 UPDATE"拆开 → 成功数 > 3。
// 中文验收点：并发预留不超发。
func TestQuotaStore_ReserveIsAtomicUnderConcurrency(t *testing.T) {
	db := quotaTestDB(t)
	store := quotaSetLimit(t, db, "c_conc", callers.Limit{TokensPerPeriod: 100})

	const attempts = 8
	var granted, denied atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			idem := fmt.Sprintf("run:r1:chat:s%d:a1", n)
			_, err := store.Reserve(context.Background(), "c_conc", callers.ClassChat, callers.Cost{Tokens: 30}, idem)
			switch {
			case err == nil:
				granted.Add(1)
			case errors.Is(err, callers.ErrQuotaExhausted):
				denied.Add(1)
			default:
				t.Errorf("并发预留出现非预期错误: %v", err)
			}
		}(i)
	}
	wg.Wait()

	require.Equal(t, int64(3), granted.Load(), "100 额度只装得下 3 份 30")
	require.Equal(t, int64(attempts-3), denied.Load())
	require.Equal(t, 3, quotaReservationCount(t, db, "c_conc"), "被拒请求不得留下预留行")
	_, held, _ := quotaLedgerRow(t, db, "c_conc", usageKindTokens, quotaUTCDayStartMS)
	require.Equal(t, int64(90), held)
}

// TestQuotaStore_ReserveRejectsInvalidArguments 判预留的参数校验：空调用方 / 空幂等
// 键 / 闭集外类别 / 非法成本（负值、零维度、多维、类别与维度不相容）一律
// `errQuotaInvalidArgument`，且不落任何行。
// 中文验收点：参数校验先于写库。
func TestQuotaStore_ReserveRejectsInvalidArguments(t *testing.T) {
	db := quotaTestDB(t)
	store := quotaSetLimit(t, db, "c_args", callers.Limit{
		TasksPerPeriod:       100,
		TokensPerPeriod:      1000,
		IngestBytesPerPeriod: 100,
	})

	cases := []struct {
		name     string
		callerID string
		class    callers.RequestClass
		cost     callers.Cost
		idem     string
	}{
		{"empty_caller", "", callers.ClassChat, callers.Cost{Tokens: 1}, "k"},
		{"blank_caller", "   ", callers.ClassChat, callers.Cost{Tokens: 1}, "k"},
		{"empty_key", "c_args", callers.ClassChat, callers.Cost{Tokens: 1}, ""},
		{"blank_key", "c_args", callers.ClassChat, callers.Cost{Tokens: 1}, "  "},
		{"unknown_class", "c_args", callers.RequestClass("bogus"), callers.Cost{Tokens: 1}, "k"},
		{"empty_cost", "c_args", callers.ClassChat, callers.Cost{}, "k"},
		{"negative_cost", "c_args", callers.ClassChat, callers.Cost{Tokens: -1}, "k"},
		{"ambiguous_cost", "c_args", callers.ClassIngest, callers.Cost{Tokens: 1, Bytes: 1}, "k"},
		{"tasks_on_chat", "c_args", callers.ClassChat, callers.Cost{Tasks: 1}, "k"},
		{"bytes_on_chat", "c_args", callers.ClassChat, callers.Cost{Bytes: 1}, "k"},
		{"tokens_on_run", "c_args", callers.ClassRun, callers.Cost{Tokens: 1}, "k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Reserve(context.Background(), tc.callerID, tc.class, tc.cost, tc.idem)
			require.ErrorIs(t, err, errQuotaInvalidArgument)
		})
	}

	require.Zero(t, quotaReservationCount(t, db, "c_args"), "参数被拒不得留预留行")
	require.Zero(t, quotaUsageRowCount(t, db, "c_args"), "参数被拒不得建用量行")
}

// TestQuotaStore_ReserveLedgerMapping 判「类别 + 成本 → 账本」的映射：任务次数记
// tasks、字节记 ingest_bytes、其余 Token（含摄取期 Embedding 与重排）记 tokens；
// 三本账各自一行、互不串账。
// 中文验收点：账本维度由成本维度唯一决定。
func TestQuotaStore_ReserveLedgerMapping(t *testing.T) {
	db := quotaTestDB(t)
	store := quotaSetLimit(t, db, "c_ledger", callers.Limit{
		TasksPerPeriod:       10,
		TokensPerPeriod:      100,
		IngestBytesPerPeriod: 1000,
	})

	run := quotaReserveCost(t, store, "c_ledger", callers.ClassRun, callers.Cost{Tasks: 2}, "run:r1:admit")
	requireReservationLedger(t, run.ID, reservationIDPrefix+usageKindTasks+"_", usageKindTasks)

	ingestBytes := quotaReserveCost(t, store, "c_ledger", callers.ClassIngest, callers.Cost{Bytes: 500}, "ingest:b1")
	requireReservationLedger(t, ingestBytes.ID, reservationIDPrefix+usageKindIngestBytes+"_", usageKindIngestBytes)

	embed := quotaReserveCost(t, store, "c_ledger", callers.ClassIngest, callers.Cost{Tokens: 40}, "ingest:e1")
	requireReservationLedger(t, embed.ID, reservationIDPrefix+usageKindTokens+"_", usageKindTokens)

	rerank := quotaReserveCost(t, store, "c_ledger", callers.ClassRerank, callers.Cost{Tokens: 7}, "rerank:1")
	requireReservationLedger(t, rerank.ID, reservationIDPrefix+usageKindTokens+"_", usageKindTokens)

	_, tasksHeld, _ := quotaLedgerRow(t, db, "c_ledger", usageKindTasks, quotaUTCDayStartMS)
	require.Equal(t, int64(2), tasksHeld)
	_, bytesHeld, _ := quotaLedgerRow(t, db, "c_ledger", usageKindIngestBytes, quotaUTCDayStartMS)
	require.Equal(t, int64(500), bytesHeld)
	_, tokensHeld, _ := quotaLedgerRow(t, db, "c_ledger", usageKindTokens, quotaUTCDayStartMS)
	require.Equal(t, int64(47), tokensHeld, "摄取期 Embedding 与重排共用 Token 账本")
}

// TestQuotaStore_PeriodBoundaryAndRollover 判周期边界与跨周期结算：周期起点按
// `caller_limits.period/timezone` 的本地 00:00 计算（存 UTC 瞬时），句柄时钟推进后
// 新预留落新周期行；结算**总是**记在预留创建时的周期，不按当前时间重算。
// 逆改点：结算取当前周期 → 上一周期行 reserved 不释放、当前周期冒出 used。
// 中文验收点：周期起止与时区、跨周期结算。
func TestQuotaStore_PeriodBoundaryAndRollover(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_shanghai", callers.Limit{
		TokensPerPeriod: 1000,
		Period:          callers.PeriodDay,
		Timezone:        "Asia/Shanghai",
	})

	// 2026-10-21 09:00 UTC = 2026-10-21 17:00 +08:00，日周期起点是 2026-10-20 16:00 UTC。
	firstStart := time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC).UnixMilli()
	first := quotaReserve(t, store, "c_shanghai", "run:r1:chat:s1:a1", 100)
	require.Equal(t, firstStart, first.PeriodStart, "周期按调用方时区本地 00:00 起算并存 UTC 瞬时")

	// 时钟推进到下一个本地日：新预留落新周期行，旧行原封不动。
	db.now = func() time.Time { return opNow.Add(24 * time.Hour) }
	secondStart := time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC).UnixMilli()
	second := quotaReserve(t, store, "c_shanghai", "run:r1:chat:s2:a1", 200)
	require.Equal(t, secondStart, second.PeriodStart)
	require.NotEqual(t, first.PeriodStart, second.PeriodStart)

	_, firstHeld, _ := quotaLedgerRow(t, db, "c_shanghai", usageKindTokens, firstStart)
	require.Equal(t, int64(100), firstHeld)
	_, secondHeld, _ := quotaLedgerRow(t, db, "c_shanghai", usageKindTokens, secondStart)
	require.Equal(t, int64(200), secondHeld)

	// 跨周期提交：结算必须记在预留所属周期。
	_, err := store.Commit(ctx, "c_shanghai", first.ID, callers.Usage{Tokens: 80})
	require.NoError(t, err)
	firstUsed, firstHeld, _ := quotaLedgerRow(t, db, "c_shanghai", usageKindTokens, firstStart)
	require.Equal(t, int64(80), firstUsed, "必须记在预留所属周期")
	require.Zero(t, firstHeld)
	secondUsed, secondHeld, _ := quotaLedgerRow(t, db, "c_shanghai", usageKindTokens, secondStart)
	require.Zero(t, secondUsed, "不得记到当前周期")
	require.Equal(t, int64(200), secondHeld)

	// 月周期：起点是调用方时区当月 1 日 00:00。
	monthStore := quotaSetLimit(t, db, "c_month", callers.Limit{
		TokensPerPeriod: 1000,
		Period:          callers.PeriodMonth,
		Timezone:        "UTC",
	})
	monthly := quotaReserve(t, monthStore, "c_month", "run:m1:chat:s1:a1", 10)
	require.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), monthly.PeriodStart)

	monthLocal := quotaSetLimit(t, db, "c_month_sh", callers.Limit{
		TokensPerPeriod: 1000,
		Period:          callers.PeriodMonth,
		Timezone:        "Asia/Shanghai",
	})
	monthlyLocal := quotaReserve(t, monthLocal, "c_month_sh", "run:m2:chat:s1:a1", 10)
	require.Equal(t, time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC).UnixMilli(), monthlyLocal.PeriodStart,
		"月起点同样按调用方时区本地 00:00")

	// 非法 IANA 时区是配置缺陷：按错误返回，**不得**静默退回 UTC。
	quotaSeedCaller(t, db, "c_badtz", "service")
	_, err = db.pool.ExecContext(ctx, `
		INSERT INTO caller_limits (caller_id, tokens_per_period, period, timezone, updated_at)
		VALUES ($1, 100, 'day', 'Nowhere/Invalid', $2)`, "c_badtz", opNow.UnixMilli())
	require.NoError(t, err)
	_, err = NewQuotaStore(db).Reserve(ctx, "c_badtz", callers.ClassChat, callers.Cost{Tokens: 1}, "k-badtz")
	require.ErrorContains(t, err, "无效的 IANA 时区")
}

// TestQuotaStore_CommitOverageAndClearResumesReserve 判超预留与恢复：actual > amount
// 时如实记 overage（不截断、不回滚调用），overage 未清前新预留一律 429；管理员清零
// 后已发生的 used 保留，剩余额度按 `used + reserved + amount <= limit` 精确恢复。
// 逆改点：把 actual 截成 amount → overage 为 0，下一次预留成功；清零时连 used 一起
// 抹掉 → 剩余额度恢复到 1000。
// 中文验收点：超限不截断、清零不动已发生用量。
func TestQuotaStore_CommitOverageAndClearResumesReserve(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_over", callers.Limit{TokensPerPeriod: 1000})

	reserved := quotaReserve(t, store, "c_over", "run:r1:chat:s1:a1", 100)
	settlement, err := store.Commit(ctx, "c_over", reserved.ID, callers.Usage{Tokens: 130})
	require.NoError(t, err)
	require.Equal(t, int64(130), settlement.Actual)
	require.Equal(t, int64(30), settlement.Overage, "actual > amount 必须如实记 overage")

	used, held, over := quotaLedgerRow(t, db, "c_over", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(130), used)
	require.Zero(t, held)
	require.Equal(t, int64(30), over)

	_, err = store.Reserve(ctx, "c_over", callers.ClassChat, callers.Cost{Tokens: 1}, "run:r1:chat:s2:a1")
	require.ErrorIs(t, err, callers.ErrQuotaExhausted, "overage 未清前一律失败关闭")
	require.Equal(t, 1, quotaReservationCount(t, db, "c_over"))

	require.ErrorIs(t, store.ClearOverage(ctx, "c_over", 0, "  "), errQuotaInvalidArgument, "清零必须写明理由")
	require.NoError(t, store.ClearOverage(ctx, "c_over", 0, "管理员核对为供应商计价错误"))

	used, held, over = quotaLedgerRow(t, db, "c_over", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(130), used, "清零不回收已发生的用量")
	require.Zero(t, held)
	require.Zero(t, over)

	// 边界：剩余额度恰好 870 时成功，再多 1 就失败。
	require.NotEmpty(t, quotaReserve(t, store, "c_over", "run:r1:chat:s2:a1", 870).ID)
	_, err = store.Reserve(ctx, "c_over", callers.ClassChat, callers.Cost{Tokens: 1}, "run:r1:chat:s3:a1")
	require.ErrorIs(t, err, callers.ErrQuotaExhausted)
}

// TestQuotaStore_CommitIsIdempotentAndRejectsConflicts 判重复提交与终态：同一条预留
// 用相同用量重复 Commit 返回首次结果且只记一次账，用量不一致返回
// `ErrIdempotencyConflict`；对 `released` / `reconciled` 的预留发 Commit 返回
// `ErrReservationSettled`。
// 中文验收点：结算幂等与终态保护。
func TestQuotaStore_CommitIsIdempotentAndRejectsConflicts(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_commit_idem", callers.Limit{TokensPerPeriod: 1000})

	reserved := quotaReserve(t, store, "c_commit_idem", "run:r1:chat:s1:a1", 100)
	first, err := store.Commit(ctx, "c_commit_idem", reserved.ID, callers.Usage{Tokens: 80})
	require.NoError(t, err)
	require.Equal(t, callers.ReservationCommitted, first.State)

	second, err := store.Commit(ctx, "c_commit_idem", reserved.ID, callers.Usage{Tokens: 80})
	require.NoError(t, err)
	require.Equal(t, first, second, "重复提交返回首次结果")

	used, held, over := quotaLedgerRow(t, db, "c_commit_idem", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(80), used, "重复 Commit 不得重复计数")
	require.Zero(t, held)
	require.Zero(t, over)

	_, err = store.Commit(ctx, "c_commit_idem", reserved.ID, callers.Usage{Tokens: 70})
	require.ErrorIs(t, err, callers.ErrIdempotencyConflict, "同一条预留的用量不一致按冲突拒绝")
	used, _, _ = quotaLedgerRow(t, db, "c_commit_idem", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(80), used, "冲突请求不得改动账本")

	released := quotaReserve(t, store, "c_commit_idem", "run:r1:chat:s2:a1", 10)
	require.NoError(t, store.Release(ctx, "c_commit_idem", released.ID, callers.ReleaseNotSent))
	_, err = store.Commit(ctx, "c_commit_idem", released.ID, callers.Usage{Tokens: 1})
	require.ErrorIs(t, err, callers.ErrReservationSettled, "released 的预留不得再结算")

	reconciled := quotaReserve(t, store, "c_commit_idem", "run:r1:chat:s3:a1", 10)
	_, err = store.Reconcile(ctx, reconciled.ID, callers.ReconcileReleased, 0, "核对为未消耗")
	require.NoError(t, err)
	_, err = store.Commit(ctx, "c_commit_idem", reconciled.ID, callers.Usage{Tokens: 1})
	require.ErrorIs(t, err, callers.ErrReservationSettled, "reconciled 的预留不得再结算")
}

// TestQuotaStore_CommitRejectsBadInputAndMissingLedger 判 Commit 的参数 / 归属 /
// 账本一致性问题：非法参数与非法标识按参数错误拒绝，归属不匹配与不存在一律
// `ErrNotFound`，账本行缺失按 `errQuotaUsageRowMissing` 回滚且预留保持 pending。
// 中文验收点：结算的每一个错误路径都有断言。
func TestQuotaStore_CommitRejectsBadInputAndMissingLedger(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_commit_err", callers.Limit{TokensPerPeriod: 1000})

	reserved := quotaReserve(t, store, "c_commit_err", "run:r1:chat:s1:a1", 100)

	cases := []struct {
		name     string
		callerID string
		id       string
		usage    callers.Usage
		want     error
	}{
		{"empty_caller", "", reserved.ID, callers.Usage{Tokens: 1}, errQuotaInvalidArgument},
		{"empty_reservation", "c_commit_err", "", callers.Usage{Tokens: 1}, errQuotaInvalidArgument},
		{"malformed_id", "c_commit_err", "qres_tokens_short", callers.Usage{Tokens: 1}, errQuotaInvalidReservationID},
		{"unknown_kind", "c_commit_err", "qres_bogus_0123456789ABCDEFGHJKMNPQRS", callers.Usage{Tokens: 1}, errQuotaInvalidReservationID},
		{"lowercase_payload", "c_commit_err", "qres_tokens_0123456789abcdefghjkmnpqrs", callers.Usage{Tokens: 1}, errQuotaInvalidReservationID},
		{"negative_usage", "c_commit_err", reserved.ID, callers.Usage{Tokens: -1}, errQuotaInvalidArgument},
		{"cross_dimension_usage", "c_commit_err", reserved.ID, callers.Usage{Tasks: 1}, errQuotaInvalidArgument},
		{"unknown_reservation", "c_commit_err", "qres_tokens_0123456789ABCDEFGHJKMNPQRS", callers.Usage{Tokens: 1}, callers.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Commit(ctx, tc.callerID, tc.id, tc.usage)
			require.ErrorIs(t, err, tc.want)
		})
	}

	// 越权结算：归属不匹配与不存在同一条 ErrNotFound，账本不动。
	other := quotaSetLimit(t, db, "c_commit_other", callers.Limit{TokensPerPeriod: 1000})
	_, err := other.Commit(ctx, "c_commit_other", reserved.ID, callers.Usage{Tokens: 100})
	require.ErrorIs(t, err, callers.ErrNotFound, "归属不匹配不得结算")
	used, held, _ := quotaLedgerRow(t, db, "c_commit_err", usageKindTokens, reserved.PeriodStart)
	require.Zero(t, used, "越权结算不得改动账本")
	require.Equal(t, int64(100), held)

	// 账本行缺失（数据不一致）：回滚整个事务，预留仍是 pending，不得静默结清。
	orphan := quotaReserve(t, store, "c_commit_err", "run:r1:chat:s9:a1", 50)
	_, err = db.pool.ExecContext(ctx, `
		DELETE FROM caller_usage_periods
		WHERE caller_id = $1 AND period_start = $2 AND kind = $3`,
		"c_commit_err", orphan.PeriodStart, usageKindTokens)
	require.NoError(t, err)
	_, err = store.Commit(ctx, "c_commit_err", orphan.ID, callers.Usage{Tokens: 50})
	require.ErrorIs(t, err, errQuotaUsageRowMissing)
	got, err := store.Get(ctx, "c_commit_err", orphan.ID)
	require.NoError(t, err)
	require.Equal(t, callers.ReservationPending, got.Status, "账本不一致时不得把预留改成终态")
}

// TestQuotaStore_ReleaseReturnsQuotaAndIsNoopOnTerminal 判 Release：pending → released
// 并全额归还额度；重复释放与对 committed 的释放都是空操作（不重复归还）；参数 /
// 归属 / 标识错误各有断言；账本行缺失按数据不一致回滚。
// 逆改点：committed 之后释放再归还一次 → reserved 变负 / 额度凭空变多。
// 中文验收点：释放归还额度且只归还一次。
func TestQuotaStore_ReleaseReturnsQuotaAndIsNoopOnTerminal(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_release", callers.Limit{TokensPerPeriod: 1000})

	reserved := quotaReserve(t, store, "c_release", "run:r1:chat:s1:a1", 100)
	require.NoError(t, store.Release(ctx, "c_release", reserved.ID, callers.ReleaseNotSent))

	got, err := store.Get(ctx, "c_release", reserved.ID)
	require.NoError(t, err)
	require.Equal(t, callers.ReservationReleased, got.Status)

	used, held, over := quotaLedgerRow(t, db, "c_release", usageKindTokens, reserved.PeriodStart)
	require.Zero(t, used, "确认零消耗不得记 used")
	require.Zero(t, held, "释放必须归还预留")
	require.Zero(t, over)

	pending, err := store.ListPending(ctx, "c_release")
	require.NoError(t, err)
	require.Empty(t, pending)

	// 重复释放是空操作。
	require.NoError(t, store.Release(ctx, "c_release", reserved.ID, callers.ReleaseZeroUsage))
	_, held, _ = quotaLedgerRow(t, db, "c_release", usageKindTokens, reserved.PeriodStart)
	require.Zero(t, held)

	// 额度已归还：同额度可以再次预留，之后结算再释放也不重复归还。
	again := quotaReserve(t, store, "c_release", "run:r1:chat:s2:a1", 100)
	_, held, _ = quotaLedgerRow(t, db, "c_release", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(100), held, "释放后的额度必须可复用")
	_, err = store.Commit(ctx, "c_release", again.ID, callers.Usage{Tokens: 100})
	require.NoError(t, err)
	require.NoError(t, store.Release(ctx, "c_release", again.ID, callers.ReleaseZeroUsage),
		"committed 之后释放是空操作")
	used, held, over = quotaLedgerRow(t, db, "c_release", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(100), used)
	require.Zero(t, held)
	require.Zero(t, over)

	// 参数、标识与归属错误。
	require.ErrorIs(t, store.Release(ctx, "", reserved.ID, callers.ReleaseNotSent), errQuotaInvalidArgument)
	require.ErrorIs(t, store.Release(ctx, "c_release", "", callers.ReleaseNotSent), errQuotaInvalidArgument)
	require.ErrorIs(t, store.Release(ctx, "c_release", reserved.ID, callers.ReleaseReason("bogus")),
		errQuotaInvalidArgument, "释放理由必须在闭集内")
	require.ErrorIs(t, store.Release(ctx, "c_release", "qres_tokens_short", callers.ReleaseNotSent),
		errQuotaInvalidReservationID)
	require.ErrorIs(t, store.Release(ctx, "c_release", "qres_tokens_0123456789ABCDEFGHJKMNPQRS", callers.ReleaseNotSent),
		callers.ErrNotFound)

	pendingReservation := quotaReserve(t, store, "c_release", "run:r1:chat:s3:a1", 100)
	other := quotaSetLimit(t, db, "c_release_other", callers.Limit{TokensPerPeriod: 1000})
	require.ErrorIs(t, other.Release(ctx, "c_release_other", pendingReservation.ID, callers.ReleaseNotSent),
		callers.ErrNotFound, "只能按 (caller_id, reservation_id) 释放")
	_, held, _ = quotaLedgerRow(t, db, "c_release", usageKindTokens, pendingReservation.PeriodStart)
	require.Equal(t, int64(100), held, "越权释放不得归还额度")

	// 账本行缺失（数据不一致）：回滚，预留保持 pending。
	_, err = db.pool.ExecContext(ctx, `
		DELETE FROM caller_usage_periods
		WHERE caller_id = $1 AND period_start = $2 AND kind = $3`,
		"c_release", pendingReservation.PeriodStart, usageKindTokens)
	require.NoError(t, err)
	require.ErrorIs(t, store.Release(ctx, "c_release", pendingReservation.ID, callers.ReleaseNotSent),
		errQuotaUsageRowMissing)
	got, err = store.Get(ctx, "c_release", pendingReservation.ID)
	require.NoError(t, err)
	require.Equal(t, callers.ReservationPending, got.Status, "账本不一致时不得把预留改成终态")
}

// TestQuotaStore_ReconcileOutcomes 判管理面核对的两条结局：committed 按实际用量
// 记账（超预留记 overage，不截断），released 按零消耗归还且忽略 actual 参数；
// 核对是管理路径，不需要调用方参数。
// 中文验收点：Reconcile 的两条结局各自记账。
func TestQuotaStore_ReconcileOutcomes(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_rec", callers.Limit{TokensPerPeriod: 1000})

	// pending + committed（actual <= amount）。
	short := quotaReserve(t, store, "c_rec", "run:r1:chat:s1:a1", 100)
	settled, err := store.Reconcile(ctx, short.ID, callers.ReconcileCommitted, 60, "管理员确认供应商已计费")
	require.NoError(t, err)
	require.Equal(t, callers.Settlement{
		ReservationID: short.ID,
		CallerID:      "c_rec",
		Class:         callers.ClassChat,
		State:         callers.ReservationReconciled,
		PeriodStart:   short.PeriodStart,
		Amount:        100,
		Actual:        60,
		Overage:       0,
	}, settled)
	used, held, over := quotaLedgerRow(t, db, "c_rec", usageKindTokens, short.PeriodStart)
	require.Equal(t, int64(60), used)
	require.Zero(t, held)
	require.Zero(t, over)
	got, err := store.Get(ctx, "c_rec", short.ID)
	require.NoError(t, err)
	require.Equal(t, callers.ReservationReconciled, got.Status)

	// pending + committed（actual > amount）：如实记 overage。
	overReserved := quotaReserve(t, store, "c_rec", "run:r1:chat:s2:a1", 100)
	settled, err = store.Reconcile(ctx, overReserved.ID, callers.ReconcileCommitted, 140, "按供应商账单核对")
	require.NoError(t, err)
	require.Equal(t, int64(140), settled.Actual)
	require.Equal(t, int64(40), settled.Overage)
	used, held, over = quotaLedgerRow(t, db, "c_rec", usageKindTokens, short.PeriodStart)
	require.Equal(t, int64(200), used)
	require.Zero(t, held)
	require.Equal(t, int64(40), over)

	// pending + released：按零消耗归还，actual 参数被忽略。前一条用例留下的 overage
	// 会挡住该调用方的下一次 Reserve（既有断言见 overage 用例），因此换一个调用方，
	// 免得本用例在准备阶段就撞上失败关闭。
	releaseStore := quotaSetLimit(t, db, "c_rec_release", callers.Limit{TokensPerPeriod: 1000})
	returned := quotaReserve(t, releaseStore, "c_rec_release", "run:r1:chat:s1:a1", 100)
	settled, err = releaseStore.Reconcile(ctx, returned.ID, callers.ReconcileReleased, 50, "调用确认未发出")
	require.NoError(t, err)
	require.Equal(t, callers.Settlement{
		ReservationID: returned.ID,
		CallerID:      "c_rec_release",
		Class:         callers.ClassChat,
		State:         callers.ReservationReconciled,
		PeriodStart:   returned.PeriodStart,
		Amount:        100,
		Actual:        0,
		Overage:       0,
	}, settled, "released 结局不记实际用量")
	used, held, over = quotaLedgerRow(t, db, "c_rec_release", usageKindTokens, returned.PeriodStart)
	require.Zero(t, used, "released 结局不记 used")
	require.Zero(t, held, "released 结局全额归还预留")
	require.Zero(t, over)
}

// TestQuotaStore_ReconcileOnlyClaimsPending 判四态收口：只有 pending 能被核对；
// committed / released / reconciled 一律 `ErrReservationSettled`，且账本不变。
// 中文验收点：终态不得二次核对。
func TestQuotaStore_ReconcileOnlyClaimsPending(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_rec_state", callers.Limit{TokensPerPeriod: 1000})

	committed := quotaReserve(t, store, "c_rec_state", "run:r1:chat:s1:a1", 10)
	_, err := store.Commit(ctx, "c_rec_state", committed.ID, callers.Usage{Tokens: 5})
	require.NoError(t, err)

	released := quotaReserve(t, store, "c_rec_state", "run:r1:chat:s2:a1", 10)
	require.NoError(t, store.Release(ctx, "c_rec_state", released.ID, callers.ReleaseNotSent))

	reconciled := quotaReserve(t, store, "c_rec_state", "run:r1:chat:s3:a1", 10)
	_, err = store.Reconcile(ctx, reconciled.ID, callers.ReconcileReleased, 0, "核对为未消耗")
	require.NoError(t, err)

	used, held, over := quotaLedgerRow(t, db, "c_rec_state", usageKindTokens, committed.PeriodStart)

	for _, tc := range []struct {
		name   string
		id     string
		status callers.ReservationStatus
	}{
		{"committed", committed.ID, callers.ReservationCommitted},
		{"released", released.ID, callers.ReservationReleased},
		{"reconciled", reconciled.ID, callers.ReservationReconciled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Reconcile(ctx, tc.id, callers.ReconcileCommitted, 1, "重复核对")
			require.ErrorIs(t, err, callers.ErrReservationSettled)
			got, err := store.Get(ctx, "c_rec_state", tc.id)
			require.NoError(t, err)
			require.Equal(t, tc.status, got.Status, "终态不得被二次核对改写")
		})
	}

	afterUsed, afterHeld, afterOver := quotaLedgerRow(t, db, "c_rec_state", usageKindTokens, committed.PeriodStart)
	require.Equal(t, used, afterUsed, "被拒的核对不得改动账本")
	require.Equal(t, held, afterHeld)
	require.Equal(t, over, afterOver)
}

// TestQuotaStore_ReconcileRejectsBadInputAndMissingLedger 判 Reconcile 的参数 /
// 标识 / 账本一致性错误路径。
// 中文验收点：核对的错误路径逐条断言。
func TestQuotaStore_ReconcileRejectsBadInputAndMissingLedger(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_rec_err", callers.Limit{TokensPerPeriod: 1000})

	cases := []struct {
		name    string
		id      string
		outcome callers.ReconcileOutcome
		actual  int64
		reason  string
		want    error
	}{
		{"empty_id", "", callers.ReconcileReleased, 0, "理由", errQuotaInvalidArgument},
		{"bad_outcome", "qres_tokens_0123456789ABCDEFGHJKMNPQRS", callers.ReconcileOutcome("pending"), 0, "理由", errQuotaInvalidArgument},
		{"negative_actual", "qres_tokens_0123456789ABCDEFGHJKMNPQRS", callers.ReconcileCommitted, -1, "理由", errQuotaInvalidArgument},
		{"empty_reason", "qres_tokens_0123456789ABCDEFGHJKMNPQRS", callers.ReconcileReleased, 0, "", errQuotaInvalidArgument},
		{"blank_reason", "qres_tokens_0123456789ABCDEFGHJKMNPQRS", callers.ReconcileReleased, 0, "   ", errQuotaInvalidArgument},
		{"malformed_id", "qres_tokens_short", callers.ReconcileReleased, 0, "理由", errQuotaInvalidReservationID},
		{"unknown_reservation", "qres_tokens_0123456789ABCDEFGHJKMNPQRS", callers.ReconcileReleased, 0, "理由", callers.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Reconcile(ctx, tc.id, tc.outcome, tc.actual, tc.reason)
			require.ErrorIs(t, err, tc.want)
		})
	}

	// 账本行缺失：回滚事务，预留保持 pending。
	orphan := quotaReserve(t, store, "c_rec_err", "run:r1:chat:s1:a1", 50)
	_, err := db.pool.ExecContext(ctx, `
		DELETE FROM caller_usage_periods
		WHERE caller_id = $1 AND period_start = $2 AND kind = $3`,
		"c_rec_err", orphan.PeriodStart, usageKindTokens)
	require.NoError(t, err)
	_, err = store.Reconcile(ctx, orphan.ID, callers.ReconcileCommitted, 50, "核对")
	require.ErrorIs(t, err, errQuotaUsageRowMissing)
	got, err := store.Get(ctx, "c_rec_err", orphan.ID)
	require.NoError(t, err)
	require.Equal(t, callers.ReservationPending, got.Status)
}

// TestQuotaStore_ClearOverageResolvesPeriod 判 admin 清零的周期折算：periodStart=0
// 取"当前周期"，非 0 的瞬时被折算到所属周期的起点（管理面拿到的是"某个月里的某一
// 刻"，不该要求调用方自己算本地 00:00）；缺限额行按 DDL 缺省 day/UTC 解释，非法
// 时区按配置缺陷报错；只清 overage > 0 的行。
// 中文验收点：清零只动目标周期的 overage。
func TestQuotaStore_ClearOverageResolvesPeriod(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_clear", callers.Limit{TokensPerPeriod: 1000})

	// 造出当前周期的 overage。
	reserved := quotaReserve(t, store, "c_clear", "run:r1:chat:s1:a1", 100)
	_, err := store.Commit(ctx, "c_clear", reserved.ID, callers.Usage{Tokens: 150})
	require.NoError(t, err)
	_, _, over := quotaLedgerRow(t, db, "c_clear", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(50), over)

	// periodStart=0 → 当前周期（opNow 所在日，UTC）。
	require.NoError(t, store.ClearOverage(ctx, "c_clear", 0, "清零当前周期"))
	used, held, over := quotaLedgerRow(t, db, "c_clear", usageKindTokens, reserved.PeriodStart)
	require.Equal(t, int64(150), used, "清零不回收已发生的用量")
	require.Zero(t, held)
	require.Zero(t, over)

	// 非 0 瞬时折算到所属周期起点：月周期下，月中某一刻清的是整月那一行。
	monthStore := quotaSetLimit(t, db, "c_clear_month", callers.Limit{
		TokensPerPeriod: 1000,
		Period:          callers.PeriodMonth,
		Timezone:        "UTC",
	})
	monthStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	monthly := quotaReserve(t, monthStore, "c_clear_month", "run:m1:chat:s1:a1", 100)
	_, err = monthStore.Commit(ctx, "c_clear_month", monthly.ID, callers.Usage{Tokens: 130})
	require.NoError(t, err)

	// 上一月也留一条 overage，用于证明只清目标周期。
	previousStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	_, err = db.pool.ExecContext(ctx, `
		INSERT INTO caller_usage_periods (caller_id, period_start, kind, used, reserved, overage, updated_at)
		VALUES ($1, $2, $3, 10, 0, 7, $4)`,
		"c_clear_month", previousStart.UnixMilli(), usageKindTokens, opNow.UnixMilli())
	require.NoError(t, err)

	midMonth := time.Date(2026, 10, 15, 12, 30, 0, 0, time.UTC)
	require.NoError(t, monthStore.ClearOverage(ctx, "c_clear_month", midMonth.UnixMilli(), "按月中账单清零"))
	used, _, octOver := quotaLedgerRow(t, db, "c_clear_month", usageKindTokens, monthStart.UnixMilli())
	require.Equal(t, int64(130), used, "清零不动 used")
	require.Zero(t, octOver)
	_, _, sepOver := quotaLedgerRow(t, db, "c_clear_month", usageKindTokens, previousStart.UnixMilli())
	require.Equal(t, int64(7), sepOver, "只清目标周期，别的周期原封不动")

	// 缺限额行：按缺省 day/UTC 折算（管理面先清了限额也不该报错）。
	quotaSeedCaller(t, db, "c_clear_default", "service")
	defaultStart := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	_, err = db.pool.ExecContext(ctx, `
		INSERT INTO caller_usage_periods (caller_id, period_start, kind, used, reserved, overage, updated_at)
		VALUES ($1, $2, $3, 0, 0, 3, $4)`,
		"c_clear_default", defaultStart.UnixMilli(), usageKindTokens, opNow.UnixMilli())
	require.NoError(t, err)
	require.NoError(t, NewQuotaStore(db).ClearOverage(ctx, "c_clear_default", 0, "无限额行按缺省周期清零"))
	_, _, defaultOver := quotaLedgerRow(t, db, "c_clear_default", usageKindTokens, defaultStart.UnixMilli())
	require.Zero(t, defaultOver)

	// 非法 IANA 时区是配置缺陷（ClearOverage 走同一条周期解析）。
	quotaSeedCaller(t, db, "c_clear_badtz", "service")
	_, err = db.pool.ExecContext(ctx, `
		INSERT INTO caller_limits (caller_id, tokens_per_period, period, timezone, updated_at)
		VALUES ($1, 100, 'day', 'Nowhere/Invalid', $2)`, "c_clear_badtz", opNow.UnixMilli())
	require.NoError(t, err)
	err = NewQuotaStore(db).ClearOverage(ctx, "c_clear_badtz", 0, "配置缺陷")
	require.ErrorContains(t, err, "无效的 IANA 时区")

	// 参数与"没有超限"：空调用方 / 空白理由按非法参数拒绝，无匹配行是空操作。
	require.ErrorIs(t, store.ClearOverage(ctx, "", 0, "缺调用方"), errQuotaInvalidArgument)
	require.ErrorIs(t, store.ClearOverage(ctx, "c_clear", 0, "   "), errQuotaInvalidArgument)
	require.NoError(t, store.ClearOverage(ctx, "c_clear", 0, "没有超限也是空操作"))
}

// TestQuotaStore_SetLimitBackfillsCallerAndMaxConcurrent 判 SetLimit / MaxConcurrent：
// SetLimit 在同一事务内补调用方行（R28.5）、按 `(caller_id)` upsert 限额、缺省值
// 由 Normalize 补齐；MaxConcurrent 只回答"有没有这一行、值是多少"——缺行给
// ok=false，行存在但为 0 给 ok=true（回退到实例级上限由装配层裁决）。
// 中文验收点：限额写入的补行、upsert 与读回。
func TestQuotaStore_SetLimitBackfillsCallerAndMaxConcurrent(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := NewQuotaStore(db)

	// 缺行：ok=false，存储层不做缺省填充。
	limit, ok, err := store.MaxConcurrent(ctx, "c_limit")
	require.NoError(t, err)
	require.False(t, ok, "没有限额行时不得凭空给出上限")
	require.Zero(t, limit)

	// 管理面先配限额再建调用方：同一事务内补调用方行，满足外键。
	require.NoError(t, store.SetLimit(ctx, "c_limit", callers.Limit{
		QPS:                  5.5,
		Burst:                10,
		MaxConcurrent:        4,
		TasksPerPeriod:       11,
		TokensPerPeriod:      1000,
		IngestBytesPerPeriod: 2048,
		Period:               callers.PeriodMonth,
		Timezone:             "Asia/Shanghai",
	}))

	var (
		kind, name, project, status string
		createdAt                   int64
	)
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT kind, name, project, status, created_at FROM callers WHERE caller_id = $1`,
		"c_limit").Scan(&kind, &name, &project, &status, &createdAt))
	require.Equal(t, "service", kind, "补出的调用方按 service 建档")
	require.Equal(t, "c_limit", name)
	require.Equal(t, "", project)
	require.Equal(t, "active", status)
	require.Equal(t, opNow.UnixMilli(), createdAt)

	limit, ok, err = store.MaxConcurrent(ctx, "c_limit")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 4, limit)

	var (
		qps                  float64
		burst, maxConcurrent int
		tasks, tokens        int64
		ingest               int64
		period, timezone     string
	)
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT qps::float8, burst, max_concurrent, tasks_per_period, tokens_per_period,
		       ingest_bytes_per_period, period, timezone
		FROM caller_limits WHERE caller_id = $1`, "c_limit",
	).Scan(&qps, &burst, &maxConcurrent, &tasks, &tokens, &ingest, &period, &timezone))
	require.Equal(t, 5.5, qps)
	require.Equal(t, 10, burst)
	require.Equal(t, 4, maxConcurrent)
	require.Equal(t, int64(11), tasks)
	require.Equal(t, int64(1000), tokens)
	require.Equal(t, int64(2048), ingest)
	require.Equal(t, "month", period)
	require.Equal(t, "Asia/Shanghai", timezone)

	// 二次 SetLimit 是 upsert：补行幂等、只有一行、值被更新，缺省值被补齐。
	require.NoError(t, store.SetLimit(ctx, "c_limit", callers.Limit{MaxConcurrent: 2, TokensPerPeriod: 10}))
	require.Equal(t, 1, quotaCallerRowCount(t, db, "c_limit"))
	var limitRows int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM caller_limits WHERE caller_id = $1`, "c_limit").Scan(&limitRows))
	require.Equal(t, 1, limitRows, "同一调用方只有一行限额")
	limit, ok, err = store.MaxConcurrent(ctx, "c_limit")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, limit)
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT period, timezone FROM caller_limits WHERE caller_id = $1`, "c_limit",
	).Scan(&period, &timezone))
	require.Equal(t, "day", period, "空周期按 day 补齐")
	require.Equal(t, "UTC", timezone, "空时区按 UTC 补齐")

	// 行存在但值为 0：ok=true，与"没有行"区分开。
	require.NoError(t, store.SetLimit(ctx, "c_limit", callers.Limit{}))
	limit, ok, err = store.MaxConcurrent(ctx, "c_limit")
	require.NoError(t, err)
	require.True(t, ok)
	require.Zero(t, limit)

	// 已有调用方行不被覆盖（ON CONFLICT DO NOTHING）。
	_, err = db.pool.ExecContext(ctx, `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ('c_preset', 'builtin', 'preset', '', 'active', $1)`, opNow.UnixMilli())
	require.NoError(t, err)
	require.NoError(t, store.SetLimit(ctx, "c_preset", callers.Limit{TokensPerPeriod: 1}))
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT kind, name FROM callers WHERE caller_id = 'c_preset'`).Scan(&kind, &name))
	require.Equal(t, "builtin", kind, "补行不得改写既有调用方")
	require.Equal(t, "preset", name)
}

// TestQuotaStore_SetLimitRejectsBadInput 判 SetLimit 的参数校验先于写库：空调用方、
// 负限额、未知周期、非法时区都拒绝，且不留下调用方行或限额行。
// 中文验收点：非法限额不落地。
func TestQuotaStore_SetLimitRejectsBadInput(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := NewQuotaStore(db)

	require.ErrorIs(t, store.SetLimit(ctx, "", callers.Limit{}), errQuotaInvalidArgument)
	require.ErrorIs(t, store.SetLimit(ctx, "   ", callers.Limit{}), errQuotaInvalidArgument)
	require.ErrorContains(t, store.SetLimit(ctx, "c_bad", callers.Limit{QPS: -1}), "限额不得为负")
	require.ErrorContains(t, store.SetLimit(ctx, "c_bad", callers.Limit{Period: "week"}), "未知配额周期")
	require.ErrorContains(t, store.SetLimit(ctx, "c_bad", callers.Limit{Timezone: "Mars/Olympus"}), "无效的 IANA 时区")

	require.Zero(t, quotaCallerRowCount(t, db, "c_bad"), "非法限额不得补调用方行")
	var limitRows int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM caller_limits WHERE caller_id = $1`, "c_bad").Scan(&limitRows))
	require.Zero(t, limitRows)
}

// TestQuotaStore_ListPendingAndGet 判未结算清单与按归属取行：只列 pending、按
// (created_at, reservation_id) 稳定排序、跨调用方隔离；Get 对归属不匹配与不存在
// 一律 `ErrNotFound`，参数缺失是非法参数。
// 中文验收点：清单只含结局未知的预留。
func TestQuotaStore_ListPendingAndGet(t *testing.T) {
	db := quotaTestDB(t)
	ctx := context.Background()
	store := quotaSetLimit(t, db, "c_list", callers.Limit{TokensPerPeriod: 1000})
	other := quotaSetLimit(t, db, "c_list_other", callers.Limit{TokensPerPeriod: 1000})

	pending, err := store.ListPending(ctx, "c_list")
	require.NoError(t, err)
	require.Empty(t, pending)

	first := quotaReserve(t, store, "c_list", "run:r1:chat:s1:a1", 10)
	second := quotaReserve(t, store, "c_list", "run:r1:chat:s2:a1", 20)
	third := quotaReserve(t, store, "c_list", "run:r1:chat:s3:a1", 30)
	foreign := quotaReserve(t, other, "c_list_other", "run:r9:chat:s1:a1", 5)

	pending, err = store.ListPending(ctx, "c_list")
	require.NoError(t, err)
	require.Len(t, pending, 3)
	ids := []string{pending[0].ID, pending[1].ID, pending[2].ID}
	want := []string{first.ID, second.ID, third.ID}
	sort.Strings(want)
	require.Equal(t, want, ids, "同一 created_at 下按 reservation_id 稳定排序")
	require.NotContains(t, ids, foreign.ID, "不得串出别的调用方的预留")

	// 终态从清单消失：committed / released / reconciled 各一。
	_, err = store.Commit(ctx, "c_list", first.ID, callers.Usage{Tokens: 10})
	require.NoError(t, err)
	require.NoError(t, store.Release(ctx, "c_list", second.ID, callers.ReleaseZeroUsage))
	_, err = store.Reconcile(ctx, third.ID, callers.ReconcileReleased, 0, "核对为未消耗")
	require.NoError(t, err)
	pending, err = store.ListPending(ctx, "c_list")
	require.NoError(t, err)
	require.Empty(t, pending)

	// Get：状态回读、归属不匹配与不存在都是 ErrNotFound、参数缺失是非法参数。
	got, err := store.Get(ctx, "c_list", first.ID)
	require.NoError(t, err)
	require.Equal(t, callers.ReservationCommitted, got.Status, "Get 返回行的当前状态")

	_, err = store.Get(ctx, "c_list_other", first.ID)
	require.ErrorIs(t, err, callers.ErrNotFound, "归属不匹配按不存在处理")
	_, err = store.Get(ctx, "c_list", "qres_tokens_0123456789ABCDEFGHJKMNPQRS")
	require.ErrorIs(t, err, callers.ErrNotFound)
	_, err = store.Get(ctx, "", first.ID)
	require.ErrorIs(t, err, errQuotaInvalidArgument)
	_, err = store.Get(ctx, "c_list", "  ")
	require.ErrorIs(t, err, errQuotaInvalidArgument)
	_, err = store.ListPending(ctx, "  ")
	require.ErrorIs(t, err, errQuotaInvalidArgument)
}

// TestQuotaStore_UnconfiguredHandle 判装配缺失一律失败关闭：没有数据库句柄时九个
// 方法全部返回 `errQuotaStoreUnconfigured`，**不**放行、不 panic（含 nil 接收者）。
// 中文验收点：缺句柄不得退化成任何放行路径。
func TestQuotaStore_UnconfiguredHandle(t *testing.T) {
	ctx := context.Background()
	store := NewQuotaStore(nil)
	validID := "qres_tokens_0123456789ABCDEFGHJKMNPQRS"

	_, err := store.Reserve(ctx, "c", callers.ClassChat, callers.Cost{Tokens: 1}, "k")
	require.ErrorIs(t, err, errQuotaStoreUnconfigured)
	_, err = store.Commit(ctx, "c", validID, callers.Usage{Tokens: 1})
	require.ErrorIs(t, err, errQuotaStoreUnconfigured)
	require.ErrorIs(t, store.Release(ctx, "c", validID, callers.ReleaseNotSent), errQuotaStoreUnconfigured)
	_, err = store.Reconcile(ctx, validID, callers.ReconcileReleased, 0, "理由")
	require.ErrorIs(t, err, errQuotaStoreUnconfigured)
	require.ErrorIs(t, store.ClearOverage(ctx, "c", 0, "理由"), errQuotaStoreUnconfigured)
	require.ErrorIs(t, store.SetLimit(ctx, "c", callers.Limit{TokensPerPeriod: 1}), errQuotaStoreUnconfigured)
	_, _, err = store.MaxConcurrent(ctx, "c")
	require.ErrorIs(t, err, errQuotaStoreUnconfigured)
	_, err = store.ListPending(ctx, "c")
	require.ErrorIs(t, err, errQuotaStoreUnconfigured)
	_, err = store.Get(ctx, "c", validID)
	require.ErrorIs(t, err, errQuotaStoreUnconfigured)

	// nil 接收者同样失败关闭（方法不得先解引用 db）。
	var nilStore *quotaStore
	_, err = nilStore.Reserve(ctx, "c", callers.ClassChat, callers.Cost{Tokens: 1}, "k")
	require.ErrorIs(t, err, errQuotaStoreUnconfigured)
}

// TestQuotaStore_ClosedHandleFailsClosed 判存储读失败按错误上抛（C-02 口径）：句柄的
// 连接池关闭后，读路径给出可读错误、写路径在开事务处失败；**不得**把读不到限额
// 当成不限量或 ok=true。
// 中文验收点：读失败不降级。
func TestQuotaStore_ClosedHandleFailsClosed(t *testing.T) {
	ctx := context.Background()
	pool, err := openPool(ctx, Config{DSN: testDSN(t), ConnectTimeout: 5 * time.Second})
	require.NoError(t, err)
	require.NoError(t, pool.Close())
	store := NewQuotaStore(&DB{pool: pool})
	validID := "qres_tokens_0123456789ABCDEFGHJKMNPQRS"

	_, configured, readErr := store.MaxConcurrent(ctx, "c")
	require.False(t, configured, "读失败不得报成已配置")
	require.ErrorContains(t, readErr, "读取调用方并发上限")
	_, err = store.ListPending(ctx, "c")
	require.ErrorContains(t, err, "查询未结算预留")
	_, err = store.Get(ctx, "c", validID)
	require.ErrorContains(t, err, "查询预留")

	_, err = store.Reserve(ctx, "c", callers.ClassChat, callers.Cost{Tokens: 1}, "k")
	require.ErrorContains(t, err, "开启 PostgreSQL 事务")
	_, err = store.Commit(ctx, "c", validID, callers.Usage{Tokens: 1})
	require.ErrorContains(t, err, "开启 PostgreSQL 事务")
	require.ErrorContains(t, store.Release(ctx, "c", validID, callers.ReleaseNotSent), "开启 PostgreSQL 事务")
	_, err = store.Reconcile(ctx, validID, callers.ReconcileReleased, 0, "理由")
	require.ErrorContains(t, err, "开启 PostgreSQL 事务")
	require.ErrorContains(t, store.ClearOverage(ctx, "c", 0, "理由"), "开启 PostgreSQL 事务")
	require.ErrorContains(t, store.SetLimit(ctx, "c", callers.Limit{TokensPerPeriod: 1}), "开启 PostgreSQL 事务")
}

// TestQuotaStore_ReservationIDAndLedgerHelpers 判标识生成 / 解析与账本映射的闭集：
// 未知前缀、长度不符、小写或闭集外字符一律拒绝；成本必须恰好一个维度且与类别
// 相容；用量只能落在本账本维度。
// 中文验收点：标识自描述账本、维度映射是闭集。
func TestQuotaStore_ReservationIDAndLedgerHelpers(t *testing.T) {
	for _, kind := range []string{usageKindTasks, usageKindTokens, usageKindIngestBytes} {
		id, err := newReservationID(kind)
		require.NoError(t, err)
		prefix := reservationIDPrefix + kind + "_"
		require.True(t, strings.HasPrefix(id, prefix))
		payload := strings.TrimPrefix(id, prefix)
		require.Len(t, payload, reservationPayloadLength)
		for _, r := range payload {
			require.Contains(t, crockfordAlphabet, string(r), "载荷必须来自 Crockford 闭集")
		}
		parsed, err := parseReservationKind(id)
		require.NoError(t, err)
		require.Equal(t, kind, parsed, "标识自描述账本，结算路径从 ID 恢复维度")
	}

	first, err := newReservationID(usageKindTokens)
	require.NoError(t, err)
	second, err := newReservationID(usageKindTokens)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "随机载荷不得重复")

	badIDs := []string{
		"",
		"qres_tokens_short",
		"qres_" + usageKindTokens + "_" + strings.Repeat("0", 25),
		"qres_" + usageKindTokens + "_" + strings.Repeat("0", 27),
		"qres_" + usageKindTokens + "_" + strings.Repeat("i", 26),
		"qres_" + usageKindTokens + "_" + strings.Repeat("0", 25) + "-",
		"qres_bogus_" + strings.Repeat("0", 26),
	}
	for _, id := range badIDs {
		_, err := parseReservationKind(id)
		require.ErrorIs(t, err, errQuotaInvalidReservationID, "非法标识 %q 必须拒绝", id)
	}

	ledgerCases := []struct {
		name       string
		class      callers.RequestClass
		cost       callers.Cost
		wantKind   string
		wantAmount int64
	}{
		{"run_tasks", callers.ClassRun, callers.Cost{Tasks: 3}, usageKindTasks, 3},
		{"ingest_bytes", callers.ClassIngest, callers.Cost{Bytes: 9}, usageKindIngestBytes, 9},
		{"ingest_tokens", callers.ClassIngest, callers.Cost{Tokens: 7}, usageKindTokens, 7},
		{"chat_tokens", callers.ClassChat, callers.Cost{Tokens: 5}, usageKindTokens, 5},
		{"rerank_tokens", callers.ClassRerank, callers.Cost{Tokens: 2}, usageKindTokens, 2},
	}
	for _, tc := range ledgerCases {
		t.Run("ledger_"+tc.name, func(t *testing.T) {
			kind, amount, err := ledgerFor(tc.class, tc.cost)
			require.NoError(t, err)
			require.Equal(t, tc.wantKind, kind)
			require.Equal(t, tc.wantAmount, amount)
		})
	}

	badLedgers := []struct {
		name  string
		class callers.RequestClass
		cost  callers.Cost
	}{
		{"negative", callers.ClassChat, callers.Cost{Tokens: -1}},
		{"empty", callers.ClassChat, callers.Cost{}},
		{"ambiguous", callers.ClassIngest, callers.Cost{Tokens: 1, Bytes: 1}},
		{"tasks_on_chat", callers.ClassChat, callers.Cost{Tasks: 1}},
		{"bytes_on_chat", callers.ClassChat, callers.Cost{Bytes: 1}},
		{"tokens_on_run", callers.ClassRun, callers.Cost{Tokens: 1}},
	}
	for _, tc := range badLedgers {
		t.Run("bad_ledger_"+tc.name, func(t *testing.T) {
			_, _, err := ledgerFor(tc.class, tc.cost)
			require.ErrorIs(t, err, errQuotaInvalidArgument)
		})
	}

	actualCases := []struct {
		name  string
		kind  string
		usage callers.Usage
		want  int64
	}{
		{"tasks", usageKindTasks, callers.Usage{Tasks: 4}, 4},
		{"tokens", usageKindTokens, callers.Usage{Tokens: 6}, 6},
		{"ingest_bytes", usageKindIngestBytes, callers.Usage{Bytes: 8}, 8},
		{"zero", usageKindTokens, callers.Usage{}, 0},
	}
	for _, tc := range actualCases {
		t.Run("actual_"+tc.name, func(t *testing.T) {
			actual, err := actualFor(tc.kind, tc.usage)
			require.NoError(t, err)
			require.Equal(t, tc.want, actual)
		})
	}

	badActual := []struct {
		name  string
		kind  string
		usage callers.Usage
	}{
		{"negative", usageKindTokens, callers.Usage{Tokens: -1}},
		{"tasks_cross", usageKindTasks, callers.Usage{Tasks: 1, Tokens: 1}},
		{"tokens_cross", usageKindTokens, callers.Usage{Tasks: 1}},
		{"bytes_cross", usageKindIngestBytes, callers.Usage{Tokens: 1}},
		{"unknown_kind", "bogus", callers.Usage{Tokens: 1}},
	}
	for _, tc := range badActual {
		t.Run("bad_actual_"+tc.name, func(t *testing.T) {
			_, err := actualFor(tc.kind, tc.usage)
			require.ErrorIs(t, err, errQuotaInvalidArgument)
		})
	}

	require.Equal(t, int64(30), overageFor(130, 100))
	require.Zero(t, overageFor(100, 100), "恰好用完不记超限")
	require.Zero(t, overageFor(90, 100))
}
