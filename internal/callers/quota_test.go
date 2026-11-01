package callers

// 本文件覆盖 T023 领域侧（`quota.go`）的准入与结算契约（颗粒度文档 §2.3 图 6 [5] /
// C-29 / C-35）：三段准入互不越界、内置管理员不限量走 `NoReservation` 空票据、
// 幂等键缺失与非法成本在触达存储之前就拒绝、结算方法对空票据与非法参数失败关闭。
// 存储侧的原子性与跨周期语义在 `quota_pg_test.go`（`-tags=integration`）。
// These tests pin the domain side of quota admission: the three stages stay apart,
// the unlimited admin skips reservations, and invalid input never reaches the store.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeQuotaStore 记录调用并回放预设结果，用来断言"拒绝发生在触达存储之前"。
// fakeQuotaStore records calls so rejections can be proven to happen before the store.
type fakeQuotaStore struct {
	reserveCalls int
	commitCalls  int
	releaseCalls int
	reconCalls   int
	getCalls     int

	reserveErr error
	reserved   Reservation
	settlement Settlement
	getErr     error
	got        Reservation
}

func (f *fakeQuotaStore) Reserve(context.Context, string, RequestClass, Cost, string) (Reservation, error) {
	f.reserveCalls++
	if f.reserveErr != nil {
		return Reservation{}, f.reserveErr
	}
	return f.reserved, nil
}

func (f *fakeQuotaStore) Commit(context.Context, string, string, Usage) (Settlement, error) {
	f.commitCalls++
	return f.settlement, nil
}

func (f *fakeQuotaStore) Release(context.Context, string, string, ReleaseReason) error {
	f.releaseCalls++
	return nil
}

func (f *fakeQuotaStore) Reconcile(context.Context, string, ReconcileOutcome, int64, string) (Settlement, error) {
	f.reconCalls++
	return f.settlement, nil
}

func (f *fakeQuotaStore) ClearOverage(context.Context, string, int64, string) error { return nil }

func (f *fakeQuotaStore) SetLimit(context.Context, string, Limit) error { return nil }

func (f *fakeQuotaStore) ListPending(context.Context, string) ([]Reservation, error) { return nil, nil }

func (f *fakeQuotaStore) Get(context.Context, string, string) (Reservation, error) {
	f.getCalls++
	if f.getErr != nil {
		return Reservation{}, f.getErr
	}
	return f.got, nil
}

// localAdminPrincipal 构造内置管理员身份（归属固定、无授权 Key）。
// localAdminPrincipal builds the built-in admin identity.
func localAdminPrincipal() core.Principal {
	return core.Principal{
		Kind:     core.PrincipalLocalAdmin,
		CallerID: core.LocalAdminCallerID,
		Builtin:  true,
		Actor:    core.ActorCLIAdmin,
	}
}

// TestQuota_AdmitCallReservesOncePerIssuedCall 判 [5] 只做配额、不做别的：一次
// 调用一张票据，票据携带归属与周期，且不产生任何 Run 名额动作（没有名额端口也能跑）。
// 逆改点：在 AdmitCall 里再拿一次并发名额 → 本用例因缺少端口失败关闭而红。
func TestQuota_AdmitCallReservesOncePerIssuedCall(t *testing.T) {
	store := &fakeQuotaStore{reserved: Reservation{
		ID: "qres_tokens_01", CallerID: "c1", Class: ClassChat, PeriodStart: 1_700_000_000_000, Amount: 100, Status: ReservationPending,
	}}
	limiter := NewLimiter(LimiterDeps{Quotas: store}) // 无 Buckets / Concurrency：配额路径不得依赖它们

	ticket, err := limiter.AdmitCall(context.Background(), testCallerPrincipal("c1"), ClassChat, Cost{Tokens: 100}, "run:r1:chat:s1:a1")
	require.NoError(t, err)
	require.Equal(t, "qres_tokens_01", ticket.ReservationID)
	require.Equal(t, "c1", ticket.CallerID)
	require.Equal(t, int64(1_700_000_000_000), ticket.PeriodStart)
	require.False(t, ticket.NoReservation)
	require.Equal(t, 1, store.reserveCalls)
}

// TestQuota_AdmitCallRejectsInvalidInputBeforeStore 判失败关闭：零值身份、未知类别、
// 多维度成本、空幂等键都在触达配额端口之前拒绝，**不得**先预留再报错。
// 逆改点：把校验挪到 Reserve 之后 → reserveCalls 非 0，断言红。
func TestQuota_AdmitCallRejectsInvalidInputBeforeStore(t *testing.T) {
	store := &fakeQuotaStore{}
	limiter := NewLimiter(LimiterDeps{Quotas: store})
	ctx := context.Background()

	_, err := limiter.AdmitCall(ctx, core.Principal{}, ClassChat, Cost{Tokens: 1}, "k")
	require.ErrorIs(t, err, core.ErrUnauthorized, "零值身份不得默认成管理员")

	_, err = limiter.AdmitCall(ctx, testCallerPrincipal("c1"), RequestClass("image"), Cost{Tokens: 1}, "k")
	require.ErrorIs(t, err, errInvalidRequestClass)

	_, err = limiter.AdmitCall(ctx, testCallerPrincipal("c1"), ClassChat, Cost{Tokens: 1, Bytes: 1}, "k")
	require.ErrorIs(t, err, errInvalidCost, "两个维度不得静默取其一")

	_, err = limiter.AdmitCall(ctx, testCallerPrincipal("c1"), ClassRun, Cost{Tokens: 1}, "k")
	require.ErrorIs(t, err, errInvalidCost, "ClassRun 只按任务次数记账")

	_, err = limiter.AdmitCall(ctx, testCallerPrincipal("c1"), ClassChat, Cost{Tokens: 1}, "   ")
	require.ErrorIs(t, err, errMissingIdempotencyKey, "幂等键必须由调用方生成")

	require.Zero(t, store.reserveCalls, "被拒请求不得触达配额端口")
}

// TestQuota_LocalAdminUnlimitedSkipsReservation 判内置管理员不限量：只拿空票据，
// 不写预留行；结算一律空操作。`unlimited=false` 时同一身份必须照常预留。
// 逆改点：把不限量分支删掉或挪到端口判空之后 → 第一种情形去 Reserve（store 计数非 0）。
func TestQuota_LocalAdminUnlimitedSkipsReservation(t *testing.T) {
	store := &fakeQuotaStore{reserved: Reservation{ID: "qres_tokens_01", CallerID: core.LocalAdminCallerID}}
	limiter := NewLimiter(LimiterDeps{Quotas: store, LocalAdminUnlimited: true})
	ctx := context.Background()

	ticket, err := limiter.AdmitCall(ctx, localAdminPrincipal(), ClassChat, Cost{Tokens: 10}, "run:r1:chat:s1:a1")
	require.NoError(t, err)
	require.True(t, ticket.NoReservation)
	require.Zero(t, store.reserveCalls, "不限量路径不得写预留行")

	settlement, err := limiter.Commit(ctx, ticket, Usage{Tokens: 10})
	require.NoError(t, err)
	require.Equal(t, Settlement{}, settlement)
	require.NoError(t, limiter.Release(ctx, ticket, ReleaseZeroUsage))
	require.Zero(t, store.commitCalls, "空票据结算不得触达存储")
	require.Zero(t, store.releaseCalls)

	enforced := NewLimiter(LimiterDeps{Quotas: store, LocalAdminUnlimited: false})
	_, err = enforced.AdmitCall(ctx, localAdminPrincipal(), ClassChat, Cost{Tokens: 10}, "run:r1:chat:s2:a1")
	require.NoError(t, err)
	require.Equal(t, 1, store.reserveCalls, "关闭不限量后必须走普通预留")
}

// TestQuota_SettlementRejectsInvalidTickets 判结算入口的失败关闭：空票据、未知
// 释放理由、非法核对参数都在触达存储之前拒绝。
// 逆改点：去掉票据校验 → 空 ID 也会打到 `WHERE reservation_id = ”`。
func TestQuota_SettlementRejectsInvalidTickets(t *testing.T) {
	store := &fakeQuotaStore{}
	limiter := NewLimiter(LimiterDeps{Quotas: store})
	ctx := context.Background()

	_, err := limiter.Commit(ctx, Ticket{}, Usage{Tokens: 1})
	require.ErrorIs(t, err, errInvalidTicket)
	require.ErrorIs(t, limiter.Release(ctx, Ticket{}, ReleaseNotSent), errInvalidTicket)

	_, err = limiter.Commit(ctx, Ticket{ReservationID: "qres_tokens_01", CallerID: "c1"}, Usage{Tokens: -1})
	require.ErrorIs(t, err, errInvalidUsage)

	require.ErrorIs(t,
		limiter.Release(ctx, Ticket{ReservationID: "qres_tokens_01", CallerID: "c1"}, ReleaseReason("whatever")),
		errInvalidReleaseReason)

	_, err = limiter.Reconcile(ctx, "", ReconcileCommitted, 0, "理由")
	require.ErrorIs(t, err, errInvalidReconcile)
	_, err = limiter.Reconcile(ctx, "qres_tokens_01", ReconcileOutcome("dropped"), 0, "理由")
	require.ErrorIs(t, err, errInvalidReconcile)
	_, err = limiter.Reconcile(ctx, "qres_tokens_01", ReconcileCommitted, -1, "理由")
	require.ErrorIs(t, err, errInvalidReconcile)
	_, err = limiter.Reconcile(ctx, "qres_tokens_01", ReconcileCommitted, 1, "  ")
	require.ErrorIs(t, err, errInvalidReconcile, "核对必须写明理由")

	require.Zero(t, store.commitCalls+store.releaseCalls+store.reconCalls)
}

// TestQuota_MissingStoreFailsClosed 判装配缺失：没有配额端口时 `AdmitCall` 与结算
// 一律失败关闭，**不得**放行也不得静默成功。
// 逆改点：把 nil 端口当"不限量"处理 → 断言红。
func TestQuota_MissingStoreFailsClosed(t *testing.T) {
	limiter := NewLimiter(LimiterDeps{})
	ctx := context.Background()

	_, err := limiter.AdmitCall(ctx, testCallerPrincipal("c1"), ClassChat, Cost{Tokens: 1}, "k")
	require.ErrorIs(t, err, errQuotaUnconfigured)
	_, err = limiter.Commit(ctx, Ticket{ReservationID: "qres_tokens_01", CallerID: "c1"}, Usage{Tokens: 1})
	require.ErrorIs(t, err, errQuotaUnconfigured)
	require.ErrorIs(t,
		limiter.Release(ctx, Ticket{ReservationID: "qres_tokens_01", CallerID: "c1"}, ReleaseNotSent),
		errQuotaUnconfigured)
	_, err = limiter.Reconcile(ctx, "qres_tokens_01", ReconcileReleased, 0, "理由")
	require.ErrorIs(t, err, errQuotaUnconfigured)
}

// TestQuota_ExhaustedPropagates 判预留失败的传播：存储返回 `ErrQuotaExhausted`
// 时票据必须是零值——调用方**不得**拿到一张"看起来能结算"的空票据。
// 逆改点：失败时仍返回 Ticket{CallerID, ReservationID:""} → 断言红。
func TestQuota_ExhaustedPropagates(t *testing.T) {
	store := &fakeQuotaStore{reserveErr: ErrQuotaExhausted}
	limiter := NewLimiter(LimiterDeps{Quotas: store})

	ticket, err := limiter.AdmitCall(context.Background(), testCallerPrincipal("c1"), ClassChat, Cost{Tokens: 1}, "k")
	require.ErrorIs(t, err, ErrQuotaExhausted)
	require.Equal(t, Ticket{}, ticket, "被拒请求不得留下可用票据")
}

// TestQuota_LimitNormalize 判限额缺省与校验：空周期 / 空时区补缺省，负值、未知周期
// 与非法 IANA 名拒绝——存储层与装配层共用同一份口径。
// 逆改点：空周期按 `month` 或直接放行负值 → 断言红。
func TestQuota_LimitNormalize(t *testing.T) {
	got, err := Limit{TokensPerPeriod: 10}.Normalize()
	require.NoError(t, err)
	require.Equal(t, PeriodDay, got.Period)
	require.Equal(t, DefaultTimezone, got.Timezone)

	_, err = Limit{TokensPerPeriod: -1}.Normalize()
	require.Error(t, err, "负限额必须拒绝")
	_, err = Limit{Period: Period("week")}.Normalize()
	require.Error(t, err)
	_, err = Limit{Timezone: "Mars/Olympus"}.Normalize()
	require.Error(t, err)
}

// TestQuota_ReleaseReasonClosedSet 判释放理由闭集：三个常量合法，其他值拒绝。
// 逆改点：把 Valid 写成恒真 → 断言红。
func TestQuota_ReleaseReasonClosedSet(t *testing.T) {
	for _, reason := range []ReleaseReason{ReleaseNotSent, ReleaseAuditFailed, ReleaseZeroUsage} {
		require.True(t, reason.Valid(), "%q 必须合法", reason)
	}
	require.False(t, ReleaseReason("").Valid())
	require.False(t, ReleaseReason("not-sent").Valid(), "字面量逐字冻结，不接受变体")
}

// TestQuota_ReservationStatusClosedSet 判预留状态闭集：四个冻结字面量合法，
// 空串与变体一律拒绝——状态列与 openapi `ReservationView.status` 逐字对齐，
// 多一个值就是契约漂移。
// 逆改点：把 Valid 写成恒真 → 空串 / 变体断言红；漏掉一个常量 → 对应合法断言红。
func TestQuota_ReservationStatusClosedSet(t *testing.T) {
	for _, status := range []ReservationStatus{
		ReservationPending, ReservationCommitted, ReservationReleased, ReservationReconciled,
	} {
		require.True(t, status.Valid(), "%q 必须合法", status)
	}
	require.False(t, ReservationStatus("").Valid())
	require.False(t, ReservationStatus("committed ").Valid(), "字面量逐字冻结，不接受变体")
	require.False(t, ReservationStatus("unknown").Valid())
}

// 编译期断言：本文件的假实现覆盖整个接口，新增方法时立即暴露。
// The fake must implement the whole port; a new method fails compilation.
var _ QuotaStore = (*fakeQuotaStore)(nil)
