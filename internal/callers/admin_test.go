package callers

// 本文件覆盖调用方管理面编排的验收点（US-16 / T026，颗粒度文档 §3.1.5 / §3.1.7）：
// 校验先于留痕、留痕先于执行（宪法 V）、归属先于预写、失败按哨兵分类、用量投影
// 固定六行且 `overage` 只挂代表行。存储端口与配额端口是**注入替身**——它们只回答
// "有没有发生写入"，不代替 PG 行为（那由 `*_pg_test.go` 的集成用例覆盖）。
// These tests pin the admin orchestration: validation before the pending row, the
// pending row before the change, ownership before the audit write, sentinel-based
// classification, and the fixed six-row usage projection.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeAdminKeyStore 是管理面测试的 KeyStore 替身：管理面四方法可注入行为并记录
// 入参，生命周期方法沿用 `lifecycleKeyStore`（签发路径用它记录写库）。
// fakeAdminKeyStore injects the four admin methods and reuses lifecycleKeyStore for
// the issuance path, which records every durable write.
type fakeAdminKeyStore struct {
	*lifecycleKeyStore

	listErr    error
	byID       map[string]CallerRecord
	lookupErr  error
	createErr  error
	created    []CallerRecord
	createdLim []*Limit
	usage      UsageAggregate
	usageErr   error
	usageCalls []usageCall
}

// usageCall 记录一次 Usage 查询的入参。
// usageCall records one Usage argument set.
type usageCall struct {
	CallerID    string
	PeriodStart int64
	Now         time.Time
}

// newFakeAdminKeyStore 构造带一条已存在调用方的替身。
// newFakeAdminKeyStore builds the fake with one existing caller.
func newFakeAdminKeyStore(callers ...CallerRecord) *fakeAdminKeyStore {
	store := &fakeAdminKeyStore{
		lifecycleKeyStore: newLifecycleKeyStore(),
		byID:              map[string]CallerRecord{},
	}
	for _, record := range callers {
		store.byID[record.CallerID] = record
	}
	return store
}

func (f *fakeAdminKeyStore) List(context.Context) ([]CallerRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	records := make([]CallerRecord, 0, len(f.byID))
	for _, record := range f.byID {
		records = append(records, record)
	}
	return records, nil
}

func (f *fakeAdminKeyStore) LookupCaller(_ context.Context, callerID string) (CallerRecord, error) {
	if f.lookupErr != nil {
		return CallerRecord{}, f.lookupErr
	}
	record, ok := f.byID[callerID]
	if !ok {
		return CallerRecord{}, ErrNotFound
	}
	return record, nil
}

func (f *fakeAdminKeyStore) CreateCaller(_ context.Context, record CallerRecord, limit *Limit) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, record)
	f.createdLim = append(f.createdLim, limit)
	f.byID[record.CallerID] = record
	return nil
}

func (f *fakeAdminKeyStore) Usage(_ context.Context, callerID string, periodStart int64, now time.Time) (UsageAggregate, error) {
	f.usageCalls = append(f.usageCalls, usageCall{CallerID: callerID, PeriodStart: periodStart, Now: now})
	if f.usageErr != nil {
		return UsageAggregate{}, f.usageErr
	}
	return f.usage, nil
}

// adminQuotaStore 在 `fakeQuotaStore` 之上注入核对 / 清零行为并记录入参。
// adminQuotaStore injects reconcile/clear behavior on top of fakeQuotaStore.
type adminQuotaStore struct {
	*fakeQuotaStore

	getErr         error
	current        Reservation
	getCalls       int
	reconcile      Settlement
	reconcileE     error
	reconcileCalls int
	clearErr       error
	clearCalls     []struct {
		CallerID    string
		PeriodStart int64
		Reason      string
	}
}

func newAdminQuotaStore() *adminQuotaStore {
	return &adminQuotaStore{fakeQuotaStore: &fakeQuotaStore{}}
}

func (f *adminQuotaStore) Get(context.Context, string, string) (Reservation, error) {
	f.getCalls++
	if f.getErr != nil {
		return Reservation{}, f.getErr
	}
	return f.current, nil
}

func (f *adminQuotaStore) Reconcile(context.Context, string, ReconcileOutcome, int64, string) (Settlement, error) {
	f.reconcileCalls++
	if f.reconcileE != nil {
		return Settlement{}, f.reconcileE
	}
	return f.reconcile, nil
}

func (f *adminQuotaStore) ClearOverage(_ context.Context, callerID string, periodStart int64, reason string) error {
	f.clearCalls = append(f.clearCalls, struct {
		CallerID    string
		PeriodStart int64
		Reason      string
	}{CallerID: callerID, PeriodStart: periodStart, Reason: reason})
	return f.clearErr
}

// newAdminFixture 构造注入了固定时钟的管理服务与两个端口替身。
// newAdminFixture builds the admin service with a fixed clock and both fakes.
func newAdminFixture(t *testing.T, keys *fakeAdminKeyStore, quotas *adminQuotaStore) (*CallerAdmin, *stubOperations) {
	t.Helper()
	ops := &stubOperations{}
	admin := NewCallerAdmin(keys, quotas, ops)
	fixed := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	admin.now = func() time.Time { return fixed }
	return admin, ops
}

// activeCaller 返回一条活跃的外部调用方记录。
// activeCaller returns one active external caller row.
func activeCaller(callerID string) CallerRecord {
	return CallerRecord{
		CallerID:  callerID,
		Kind:      CallerKindExternal,
		Name:      "ops",
		Status:    CallerStatusActive,
		CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
}

// TestCallerAdmin_CreateWritesPendingBeforeStore 判创建的两阶段顺序与审计投影：
// `pending` 先落库，行随后写入，终态回填成功；限额给了就同事务交给存储。
// 逆改点：把 begin 挪到 CreateCaller 之后 → 预写失败用例里行已写入，断言红。
func TestCallerAdmin_CreateWritesPendingBeforeStore(t *testing.T) {
	keys := newFakeAdminKeyStore()
	quotas := newAdminQuotaStore()
	admin, ops := newAdminFixture(t, keys, quotas)

	created, err := admin.CreateCaller(context.Background(), testActor(), CreateCallerRequest{
		CallerID: "cal_01J00000000000000000000000",
		Name:     "  ops  ",
		Project:  " p1 ",
		Limit:    &Limit{QPS: 5, Burst: 10, MaxConcurrent: 2, TasksPerPeriod: 100},
	})
	require.NoError(t, err)
	require.Equal(t, "ops", created.Name, "名称去首尾空白")
	require.Equal(t, "p1", created.Project)
	require.Equal(t, CallerKindExternal, created.Kind)
	require.Equal(t, CallerStatusActive, created.Status)

	require.Len(t, ops.begun, 1)
	begin := ops.begun[0]
	require.Equal(t, actionCallerCreate, begin.Action)
	require.Equal(t, targetKindCaller, begin.TargetKind)
	require.Equal(t, "cal_01J00000000000000000000000", begin.TargetID)
	require.Equal(t, core.OperationPending, begin.Status)
	require.Equal(t, testActor().Subject, begin.ActorSubject)
	require.Equal(t, testActor().Method, begin.AuthMethod)
	require.Equal(t, testActor().Source, begin.Source)
	require.NotEmpty(t, begin.RequestHash)

	require.Len(t, keys.created, 1)
	require.NotNil(t, keys.createdLim[0], "给了 limits 必须写出限额行")
	require.Equal(t, 5.0, keys.createdLim[0].QPS)
	require.Equal(t, PeriodDay, keys.createdLim[0].Period, "限额先 Normalize 再落库")
	require.Equal(t, DefaultTimezone, keys.createdLim[0].Timezone)

	require.Len(t, ops.finished, 1)
	require.Equal(t, core.OperationSuccess, ops.finished[0].Status)
	require.Equal(t, resultOK, ops.finished[0].Code)
}

// TestCallerAdmin_CreateWithoutLimitsLeavesRowMissing 判"没给限额"与"限额为零"
// 的区别：nil 原样交给存储（不补一行全零）。
// 逆改点：nil 换成 &Limit{} → 存储收到非 nil，断言红。
func TestCallerAdmin_CreateWithoutLimitsLeavesRowMissing(t *testing.T) {
	keys := newFakeAdminKeyStore()
	admin, _ := newAdminFixture(t, keys, newAdminQuotaStore())

	_, err := admin.CreateCaller(context.Background(), testActor(), CreateCallerRequest{Name: "ops"})
	require.NoError(t, err)
	require.Len(t, keys.createdLim, 1)
	require.Nil(t, keys.createdLim[0], "未给 limits 时不得写限额行")
}

// TestCallerAdmin_CreateRejectsBeforeAnyWrite 判校验先于留痕：空名称、非法标识、
// 非法限额都在写 `pending` 与调用存储之前拒绝。
// 逆改点：把校验挪到 begin 之后 → ops.begun 非空，断言红。
func TestCallerAdmin_CreateRejectsBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name string
		req  CreateCallerRequest
	}{
		{"名称缺失", CreateCallerRequest{Name: "   "}},
		{"标识格式非法", CreateCallerRequest{CallerID: "not-a-caller-id", Name: "ops"}},
		{"限额为负", CreateCallerRequest{Name: "ops", Limit: &Limit{TokensPerPeriod: -1}}},
		{"时区非法", CreateCallerRequest{Name: "ops", Limit: &Limit{Timezone: "Mars/Olympus"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := newFakeAdminKeyStore()
			admin, ops := newAdminFixture(t, keys, newAdminQuotaStore())
			_, err := admin.CreateCaller(context.Background(), testActor(), tc.req)
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.Empty(t, ops.begun, "校验失败不得留 pending 行")
			require.Empty(t, keys.created, "校验失败不得写调用方行")
		})
	}
}

// TestCallerAdmin_CreatePreWriteFailureRefusesStore 判预写失败即拒绝执行（宪法 V）：
// 审计端口出错时存储一行都不写。
// 逆改点：忽略 begin 错误继续 CreateCaller → keys.created 非空，断言红。
func TestCallerAdmin_CreatePreWriteFailureRefusesStore(t *testing.T) {
	keys := newFakeAdminKeyStore()
	quotas := newAdminQuotaStore()
	admin, ops := newAdminFixture(t, keys, quotas)
	ops.beginErr = errors.New("audit store down")

	_, err := admin.CreateCaller(context.Background(), testActor(), CreateCallerRequest{Name: "ops"})
	require.Error(t, err)
	require.Empty(t, keys.created, "预写失败不得写入调用方行")
}

// TestCallerAdmin_CreateConflictFinishesAsConflict 判冲突分类：存储的
// `core.ErrConflict` 原样上抛（web 映射 409），审计终态记 `CONFLICT`。
// 逆改点：把冲突改写成 INTERNAL → 终态码变化，断言红。
func TestCallerAdmin_CreateConflictFinishesAsConflict(t *testing.T) {
	keys := newFakeAdminKeyStore()
	keys.createErr = core.ErrConflict
	admin, ops := newAdminFixture(t, keys, newAdminQuotaStore())

	_, err := admin.CreateCaller(context.Background(), testActor(), CreateCallerRequest{Name: "dup"})
	require.ErrorIs(t, err, core.ErrConflict)
	require.Len(t, ops.finished, 1)
	require.Equal(t, core.OperationFailed, ops.finished[0].Status)
	require.Equal(t, resultConflict, ops.finished[0].Code)
}

// TestCallerAdmin_IssueKeyRejectsUnknownCallerWithoutPending 判签发前置：调用方
// 不存在按 `ErrNotFound` 拒绝，且**不**留 `pending`、不碰 Key 表。
// 逆改点：先 begin 再查调用方 → ops.begun 非空，断言红。
func TestCallerAdmin_IssueKeyRejectsUnknownCallerWithoutPending(t *testing.T) {
	keys := newFakeAdminKeyStore()
	admin, ops := newAdminFixture(t, keys, newAdminQuotaStore())

	_, err := admin.IssueKey(context.Background(), testActor(), IssueRequest{
		CallerID: "cal_missing", Scopes: []string{core.ScopeSessionsRead},
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, ops.begun)
	require.Empty(t, keys.created)
}

// TestCallerAdmin_IssueKeyValidatesScopesBeforeAnyWrite 判 scope 校验先于写库：
// 管理面 scope 与未知字面量在 `KeyIssuer` 里被拒绝，不留 pending、不写 Key。
// 逆改点：把 ParseScopeSet 挪到 Create 之后 → keys.created 非空，断言红。
func TestCallerAdmin_IssueKeyValidatesScopesBeforeAnyWrite(t *testing.T) {
	keys := newFakeAdminKeyStore(activeCaller("cal_01J00000000000000000000000"))
	admin, ops := newAdminFixture(t, keys, newAdminQuotaStore())

	for _, scopes := range [][]string{{core.ScopeCallersAdmin}, {"unknown:scope"}, nil} {
		_, err := admin.IssueKey(context.Background(), testActor(), IssueRequest{
			CallerID: "cal_01J00000000000000000000000", Scopes: scopes,
		})
		require.ErrorIs(t, err, ErrInvalidScope)
	}
	require.Empty(t, ops.begun)
	require.Empty(t, keys.created)
}

// TestCallerAdmin_ReconcileValidatesBeforeReading 判核对参数校验先于归属查询：
// 未知结局、理由为空、committed 缺 actual、released 带 actual 都在 Get 之前拒绝。
// 逆改点：把 Get 提到校验之前 → getCalls 非 0，断言红。
func TestCallerAdmin_ReconcileValidatesBeforeReading(t *testing.T) {
	actual := int64(5)
	cases := []struct {
		name string
		req  ReconcileRequest
	}{
		{"未知结局", ReconcileRequest{CallerID: "c1", ReservationID: "r1", Outcome: "maybe", Reason: "x"}},
		{"理由为空", ReconcileRequest{CallerID: "c1", ReservationID: "r1", Outcome: ReconcileReleased, Reason: " "}},
		{"committed 缺 actual", ReconcileRequest{CallerID: "c1", ReservationID: "r1", Outcome: ReconcileCommitted, Reason: "x"}},
		{"committed 负 actual", ReconcileRequest{CallerID: "c1", ReservationID: "r1", Outcome: ReconcileCommitted, Actual: ptrInt64(-1), Reason: "x"}},
		{"released 带 actual", ReconcileRequest{CallerID: "c1", ReservationID: "r1", Outcome: ReconcileReleased, Actual: &actual, Reason: "x"}},
		{"标识缺失", ReconcileRequest{CallerID: "", ReservationID: "r1", Outcome: ReconcileReleased, Reason: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quotas := newAdminQuotaStore()
			admin, ops := newAdminFixture(t, newFakeAdminKeyStore(), quotas)
			_, err := admin.ReconcileReservation(context.Background(), testActor(), tc.req)
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.Equal(t, 0, quotas.getCalls, "参数校验失败不得查预留")
			require.Empty(t, ops.begun)
		})
	}
}

// ptrInt64 返回 int64 指针（测试夹具用）。
// ptrInt64 returns a pointer to v.
func ptrInt64(v int64) *int64 { return &v }

// TestCallerAdmin_ReconcileChecksOwnershipAndStateBeforePending 判归属与状态先于
// 预写：Get 未命中 / 已终态都在写 `pending` 之前拒绝（不给错误的调用方留审计行）。
// 逆改点：先 begin 再 Get → ops.begun 非空，断言红。
func TestCallerAdmin_ReconcileChecksOwnershipAndStateBeforePending(t *testing.T) {
	t.Run("归属不符", func(t *testing.T) {
		quotas := newAdminQuotaStore()
		quotas.getErr = ErrNotFound
		admin, ops := newAdminFixture(t, newFakeAdminKeyStore(), quotas)
		_, err := admin.ReconcileReservation(context.Background(), testActor(), ReconcileRequest{
			CallerID: "c1", ReservationID: "r1", Outcome: ReconcileReleased, Reason: "x",
		})
		require.ErrorIs(t, err, ErrNotFound)
		require.Empty(t, ops.begun)
	})
	t.Run("已终态", func(t *testing.T) {
		quotas := newAdminQuotaStore()
		quotas.current = Reservation{ID: "r1", CallerID: "c1", Status: ReservationReconciled}
		admin, ops := newAdminFixture(t, newFakeAdminKeyStore(), quotas)
		_, err := admin.ReconcileReservation(context.Background(), testActor(), ReconcileRequest{
			CallerID: "c1", ReservationID: "r1", Outcome: ReconcileReleased, Reason: "x",
		})
		require.ErrorIs(t, err, ErrReservationSettled)
		require.Empty(t, ops.begun)
	})
}

// TestCallerAdmin_ReconcilePreWriteFailureRefuses 判核对同样 fail-closed：预写失败
// 时配额端口一次都不被调用。
// 逆改点：忽略 begin 错误 → quotas 记录到 Reconcile 调用，断言红。
func TestCallerAdmin_ReconcilePreWriteFailureRefuses(t *testing.T) {
	quotas := newAdminQuotaStore()
	quotas.current = Reservation{ID: "r1", CallerID: "c1", Status: ReservationPending}
	admin, ops := newAdminFixture(t, newFakeAdminKeyStore(), quotas)
	ops.beginErr = errors.New("audit store down")

	_, err := admin.ReconcileReservation(context.Background(), testActor(), ReconcileRequest{
		CallerID: "c1", ReservationID: "r1", Outcome: ReconcileReleased, Reason: "x",
	})
	require.Error(t, err)
	require.Zero(t, quotas.reconcileCalls, "预写失败后不得执行核对")
	require.Empty(t, ops.finished, "没有 pending 行就没有终态回填")
}

// TestCallerAdmin_ReconcileWritesPendingThenSettles 判成功路径：预写 →
// Reconcile → 回填成功，响应携带结算结果。
// 逆改点：不调 Reconcile 直接回填成功 → 用例拿到零值 settlement，断言红。
func TestCallerAdmin_ReconcileWritesPendingThenSettles(t *testing.T) {
	quotas := newAdminQuotaStore()
	quotas.current = Reservation{ID: "qres_tokens_01", CallerID: "cal_01J00000000000000000000000", Status: ReservationPending}
	quotas.reconcile = Settlement{
		ReservationID: "qres_tokens_01",
		CallerID:      "cal_01J00000000000000000000000",
		Class:         ClassChat,
		PeriodStart:   time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).UnixMilli(),
		Amount:        100,
		Actual:        42,
		State:         ReservationReconciled,
	}
	admin, ops := newAdminFixture(t, newFakeAdminKeyStore(), quotas)

	settlement, err := admin.ReconcileReservation(context.Background(), testActor(), ReconcileRequest{
		CallerID:      "cal_01J00000000000000000000000",
		ReservationID: "qres_tokens_01",
		Outcome:       ReconcileCommitted,
		Actual:        ptrInt64(42),
		Reason:        "对账后确认",
	})
	require.NoError(t, err)
	require.Equal(t, int64(42), settlement.Actual)
	require.Equal(t, ClassChat, settlement.Class)
	require.Len(t, ops.begun, 1)
	require.Equal(t, actionCallerReservationReconcile, ops.begun[0].Action)
	require.Len(t, ops.finished, 1)
	require.Equal(t, resultOK, ops.finished[0].Code)

	var detail map[string]any
	require.NoError(t, json.Unmarshal(ops.begun[0].DetailJSON, &detail))
	require.Equal(t, "qres_tokens_01", detail["reservation_id"])
	require.Equal(t, "committed", detail["outcome"])
	require.Equal(t, "对账后确认", detail["reason"])
}

// TestCallerAdmin_ClearOverageRequiresReasonAndKnownCaller 判清零前置：理由为空
// 按请求非法拒绝；调用方不存在按 404 语义拒绝；两者都不留 `pending`。
// 逆改点：删掉 LookupCaller → 不存在的调用方也能留下审计行，断言红。
func TestCallerAdmin_ClearOverageRequiresReasonAndKnownCaller(t *testing.T) {
	t.Run("理由为空", func(t *testing.T) {
		keys := newFakeAdminKeyStore(activeCaller("cal_01J00000000000000000000000"))
		admin, ops := newAdminFixture(t, keys, newAdminQuotaStore())
		_, err := admin.ClearOverage(context.Background(), testActor(), ClearOverageRequest{
			CallerID: "cal_01J00000000000000000000000", Reason: " ",
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
		require.Empty(t, ops.begun)
	})
	t.Run("调用方不存在", func(t *testing.T) {
		keys := newFakeAdminKeyStore()
		admin, ops := newAdminFixture(t, keys, newAdminQuotaStore())
		_, err := admin.ClearOverage(context.Background(), testActor(), ClearOverageRequest{
			CallerID: "cal_missing", Reason: "x",
		})
		require.ErrorIs(t, err, ErrNotFound)
		require.Empty(t, ops.begun)
	})
}

// TestCallerAdmin_ClearOverageRecordsLedgerScopeDetail 判清零审计投影：动作复用
// `caller.reservation.reconcile`，`detail.scope=overage` 与预留级核对区分；清零后
// 回读同一周期的用量视图。
// 逆改点：detail 缺 `scope` → 与预留级核对无法区分，断言红。
func TestCallerAdmin_ClearOverageRecordsLedgerScopeDetail(t *testing.T) {
	callerID := "cal_01J00000000000000000000000"
	keys := newFakeAdminKeyStore(activeCaller(callerID))
	keys.usage = UsageAggregate{
		CallerID:    callerID,
		PeriodStart: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).UnixMilli(),
		Limit:       Limit{Period: PeriodDay, Timezone: DefaultTimezone},
	}
	quotas := newAdminQuotaStore()
	admin, ops := newAdminFixture(t, keys, quotas)

	usage, err := admin.ClearOverage(context.Background(), testActor(), ClearOverageRequest{
		CallerID: callerID, Reason: "人工核对后清零",
	})
	require.NoError(t, err)
	require.Equal(t, callerID, usage.CallerID)

	require.Len(t, quotas.clearCalls, 1)
	require.Equal(t, int64(0), quotas.clearCalls[0].PeriodStart, "零时刻按当前周期交给存储")
	require.Equal(t, "人工核对后清零", quotas.clearCalls[0].Reason)

	require.Len(t, ops.begun, 1)
	require.Equal(t, actionCallerReservationReconcile, ops.begun[0].Action)
	var detail map[string]any
	require.NoError(t, json.Unmarshal(ops.begun[0].DetailJSON, &detail))
	require.Equal(t, overageScope, detail["scope"])
	require.Equal(t, callerID, detail["caller_id"])
}

// TestCallerAdmin_UnconfiguredPortsFailClosed 判装配缺失：三个端口缺任何一个，
// 六条管理动作全部拒绝（错误可识别且可判等）。
// 逆改点：ready() 只查 keys → 缺配额端口时 Reconcile 走进 nil 调用，断言红。
func TestCallerAdmin_UnconfiguredPortsFailClosed(t *testing.T) {
	keys := newFakeAdminKeyStore(activeCaller("cal_01J00000000000000000000000"))
	combos := []*CallerAdmin{
		NewCallerAdmin(nil, newAdminQuotaStore(), &stubOperations{}),
		NewCallerAdmin(keys, nil, &stubOperations{}),
		NewCallerAdmin(keys, newAdminQuotaStore(), nil),
	}
	for _, admin := range combos {
		_, err := admin.CreateCaller(context.Background(), testActor(), CreateCallerRequest{Name: "ops"})
		require.ErrorIs(t, err, errCallerAdminUnconfigured)
		_, err = admin.IssueKey(context.Background(), testActor(), IssueRequest{CallerID: "cal_01J00000000000000000000000", Scopes: []string{core.ScopeSessionsRead}})
		require.ErrorIs(t, err, errCallerAdminUnconfigured)
		_, err = admin.ReconcileReservation(context.Background(), testActor(), ReconcileRequest{CallerID: "c", ReservationID: "r", Outcome: ReconcileReleased, Reason: "x"})
		require.ErrorIs(t, err, errCallerAdminUnconfigured)
		_, err = admin.ClearOverage(context.Background(), testActor(), ClearOverageRequest{CallerID: "c", Reason: "x"})
		require.ErrorIs(t, err, errCallerAdminUnconfigured)
	}
}

// TestCallerAdmin_UsageProjectsSixFixedRows 判用量投影：六行固定顺序、`overage`
// 只挂账本代表行（tasks→run、tokens→chat、ingest_bytes→ingest_bytes）、周期边界
// 按 timezone 折算，没有限额行时零限额照实报出。
// 逆改点：把 overage 写到每个类别行 → 各行相加不等于账本总值，断言红。
func TestCallerAdmin_UsageProjectsSixFixedRows(t *testing.T) {
	callerID := "cal_01J00000000000000000000000"
	periodStart := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	keys := newFakeAdminKeyStore(activeCaller(callerID))
	keys.usage = UsageAggregate{
		CallerID:    callerID,
		PeriodStart: periodStart.UnixMilli(),
		Limit:       Limit{Period: PeriodDay, Timezone: "UTC"},
		Ledger: []LedgerUsage{
			{Kind: LedgerTasks, Used: 3, Overage: 1},
			{Kind: LedgerTokens, Used: 700, Overage: 50},
			{Kind: LedgerIngestBytes, Used: 4096},
		},
		Classes: []ClassUsage{
			{Class: ClassRun, Ledger: LedgerTasks, Used: 3},
			{Class: ClassChat, Ledger: LedgerTokens, Used: 500, Reserved: 20},
			{Class: ClassCompaction, Ledger: LedgerTokens, Used: 200},
			{Class: ClassIngest, Ledger: LedgerTokens, Used: 100},
			{Class: ClassIngest, Ledger: LedgerIngestBytes, Used: 4096},
		},
		PendingReservations: 2,
	}
	admin, _ := newAdminFixture(t, keys, newAdminQuotaStore())

	usage, err := admin.Usage(context.Background(), callerID, 0)
	require.NoError(t, err)
	require.Equal(t, periodStart, usage.PeriodStart)
	require.Equal(t, periodStart.Add(24*time.Hour), usage.PeriodEnd)
	require.Equal(t, "UTC", usage.Timezone)
	require.Equal(t, 0.0, usage.Limit.QPS, "没有限额行时零限额照实报出")
	require.Equal(t, 2, usage.PendingReservations)

	kinds := make([]string, 0, len(usage.Lines))
	byKind := map[string]UsageLine{}
	for _, line := range usage.Lines {
		kinds = append(kinds, line.Kind)
		byKind[line.Kind] = line
	}
	require.Equal(t, []string{
		UsageKindRun, UsageKindChat, UsageKindCompaction,
		UsageKindRerank, UsageKindEmbeddingTokens, UsageKindIngestBytes,
	}, kinds, "六行顺序固定")

	require.Equal(t, int64(3), byKind[UsageKindRun].Used)
	require.Equal(t, int64(1), byKind[UsageKindRun].Overage, "tasks 的 overage 挂在 run 行")
	require.Equal(t, int64(500), byKind[UsageKindChat].Used)
	require.Equal(t, int64(20), byKind[UsageKindChat].Reserved)
	require.Equal(t, int64(50), byKind[UsageKindChat].Overage, "tokens 的 overage 挂在 chat 行")
	require.Zero(t, byKind[UsageKindCompaction].Overage)
	require.Zero(t, byKind[UsageKindRerank].Used)
	require.Equal(t, int64(100), byKind[UsageKindEmbeddingTokens].Used)
	require.Equal(t, int64(4096), byKind[UsageKindIngestBytes].Used)
	require.Zero(t, byKind[UsageKindIngestBytes].Overage)
}

// TestCallerAdmin_UsageRejectsIncompatibleClassLedger 判投影的漂移探测：
// 闭集之外的 (类别, 账本) 组合报错，而不是静默丢行。
// 逆改点：未知组合默认返回空行 → 断言红。
func TestCallerAdmin_UsageRejectsIncompatibleClassLedger(t *testing.T) {
	callerID := "cal_01J00000000000000000000000"
	keys := newFakeAdminKeyStore(activeCaller(callerID))
	keys.usage = UsageAggregate{
		CallerID:    callerID,
		PeriodStart: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).UnixMilli(),
		Classes:     []ClassUsage{{Class: ClassRun, Ledger: LedgerTokens}},
	}
	admin, _ := newAdminFixture(t, keys, newAdminQuotaStore())

	_, err := admin.Usage(context.Background(), callerID, 0)
	require.Error(t, err, "类别与账本不相容必须报错")
}

// TestCallerAdmin_UsageRequiresPeriodStart 判缺周期起点的聚合不被当作"当前周期"
// 静默放行——那是存储层缺陷的信号。
// 逆改点：缺 PeriodStart 时补 time.Now() → 断言红。
func TestCallerAdmin_UsageRequiresPeriodStart(t *testing.T) {
	callerID := "cal_01J00000000000000000000000"
	keys := newFakeAdminKeyStore(activeCaller(callerID))
	keys.usage = UsageAggregate{CallerID: callerID}
	admin, _ := newAdminFixture(t, keys, newAdminQuotaStore())

	_, err := admin.Usage(context.Background(), callerID, 0)
	require.Error(t, err)
}

// flakyLookupStore 让阈值之后的 `LookupByID` 失败：模拟"吊销已生效、回读挂了"。
// flakyLookupStore fails the read-back that follows a successful revoke.
type flakyLookupStore struct {
	*fakeAdminKeyStore
	lookups  int
	failFrom int
}

// LookupByID 在超过阈值后返回错误，之前委托给内嵌替身。
// LookupByID delegates until the configured call index, then fails.
func (f *flakyLookupStore) LookupByID(ctx context.Context, callerID, keyID string) (KeyRecord, error) {
	f.lookups++
	if f.lookups > f.failFrom {
		return KeyRecord{}, errors.New("read back down")
	}
	return f.fakeAdminKeyStore.LookupByID(ctx, callerID, keyID)
}

// TestCallerAdmin_ListCallersReadsWithoutAudit 判列表是纯读路径：不写任何管理
// 操作行；端口缺失与存储失败都拒绝（fail-closed）。
// 逆改点：读路径也 begin → `ops.begun` 非空，断言红。
func TestCallerAdmin_ListCallersReadsWithoutAudit(t *testing.T) {
	first := activeCaller("cal_01J00000000000000000000001")
	second := activeCaller("cal_01J00000000000000000000002")
	keys := newFakeAdminKeyStore(first, second)
	admin, ops := newAdminFixture(t, keys, newAdminQuotaStore())

	records, err := admin.ListCallers(context.Background())
	require.NoError(t, err)
	callerIDs := make([]string, 0, len(records))
	for _, record := range records {
		callerIDs = append(callerIDs, record.CallerID)
	}
	require.ElementsMatch(t, []string{first.CallerID, second.CallerID}, callerIDs)
	require.Empty(t, ops.begun, "读路径不得写管理操作行")

	keys.listErr = errors.New("store down")
	_, err = admin.ListCallers(context.Background())
	require.Error(t, err)

	var unconfigured *CallerAdmin
	_, err = unconfigured.ListCallers(context.Background())
	require.ErrorIs(t, err, errCallerAdminUnconfigured)
}

// TestCallerAdmin_RevokeKeyReturnsTerminalRowAndFallsBackOnReadFailure 判吊销编排：
// 成功时回读终态行；吊销已生效而回读失败时返回最小投影并记 ERROR——**不得**把
// 已生效的吊销报成失败，否则调用方重试会把幂等吊销误判成未知结局。
// 逆改点：回读失败时返回错误 → 断言红；只回最小投影但不记日志 → 日志断言红。
func TestCallerAdmin_RevokeKeyReturnsTerminalRowAndFallsBackOnReadFailure(t *testing.T) {
	keyID := "key_01J0000000000000000000000"
	seed := func() *fakeAdminKeyStore {
		keys := newFakeAdminKeyStore(activeCaller(testCallerID))
		keys.records[testCallerID+"\x00"+keyID] = KeyRecord{
			KeyID:     keyID,
			CallerID:  testCallerID,
			KeyPrefix: "bk_ops01",
			Scopes:    []string{core.ScopeSessionsRead},
			Status:    KeyStatusActive,
		}
		return keys
	}

	keys := seed()
	admin, _ := newAdminFixture(t, keys, newAdminQuotaStore())
	record, err := admin.RevokeKey(context.Background(), testActor(), RevokeRequest{
		CallerID: testCallerID, KeyID: keyID, Reason: "疑似泄露",
	})
	require.NoError(t, err)
	require.Equal(t, KeyStatusRevoked, record.Status, "响应必须带吊销后的终态")
	require.Equal(t, keyID, record.KeyID)

	flaky := &flakyLookupStore{fakeAdminKeyStore: seed(), failFrom: 1}
	fallback := NewCallerAdmin(flaky, newAdminQuotaStore(), &stubOperations{})
	buf := &bytes.Buffer{}
	fallback.logger = slog.New(slog.NewTextHandler(buf, nil))
	record, err = fallback.RevokeKey(context.Background(), testActor(), RevokeRequest{
		CallerID: testCallerID, KeyID: keyID, Reason: "疑似泄露",
	})
	require.NoError(t, err, "回读失败不得把已生效的吊销报成失败")
	require.Equal(t, KeyRecord{KeyID: keyID, CallerID: testCallerID, Status: KeyStatusRevoked}, record,
		"回读失败时返回最小投影")
	require.Contains(t, buf.String(), "已吊销但回读失败")
}

// TestCallerAdmin_KindAndStatusClosedSets 判两个管理面闭集：内置 / 外部调用方与
// 活跃 / 停用状态逐字冻结，空串与变体一律拒绝（与 schema CHECK、openapi 对齐）。
// 逆改点：Valid 恒真 → 空串断言红；漏字面量 → 合法断言红。
func TestCallerAdmin_KindAndStatusClosedSets(t *testing.T) {
	for _, kind := range []CallerKind{CallerKindBuiltin, CallerKindExternal} {
		require.True(t, kind.Valid(), "%q 必须合法", kind)
	}
	require.False(t, CallerKind("").Valid())
	require.False(t, CallerKind("Builtin").Valid(), "大小写变体不是合法字面量")

	for _, status := range []CallerStatus{CallerStatusActive, CallerStatusDisabled} {
		require.True(t, status.Valid(), "%q 必须合法", status)
	}
	require.False(t, CallerStatus("").Valid())
	require.False(t, CallerStatus("enabled").Valid())
}
