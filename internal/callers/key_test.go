package callers

// 本文件覆盖 T020 Key 生命周期的验收点（颗粒度文档 §3.1.5 / C-07 / C-32）：
// scope 在写任何行之前校验；明文只在返回值出现一次，库里只有指纹与前缀；
// 预写失败即拒绝执行；吊销不碰任务绑定；轮换显式声明再绑定并逐条报告未能
// 再绑定的任务。
// The tests pin the T020 lifecycle points: scope validation precedes every durable
// write, the plaintext exists only in the returned value, a failed pre-write refuses
// the change, revocation leaves bindings alone, and rotation reports left-bound tasks.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// testCallerID 是测试用的调用方归属标识。
	testCallerID = "cal_01J0000000000000000000000"
	// testSecret 是注入的确定性明文：`bk_` + 40 位小写十六进制，与生产格式一致。
	testSecret = "bk_0123456789abcdef0123456789abcdef01234567"
	// testActorSource 是测试用的来源归因。
	testActorSource = "127.0.0.1:50000"
)

// testActor 返回一条管理请求主体；三个字段必须原样进审计。
func testActor() core.Actor {
	return core.Actor{Subject: "admin", Method: "admin-token", Source: testActorSource}
}

// statusCall 记录一次 SetStatus 调用。
type statusCall struct {
	CallerID string
	KeyID    string
	Status   KeyStatus
}

// rotateCall 记录一次 Rotate 调用。
type rotateCall struct {
	CallerID  string
	OldKeyID  string
	Next      KeyRecord
	Opts      RotateOptions
	RebindOpt bool
}

// lifecycleKeyStore 是 KeyStore 的内存实现：按 `(caller_id, key_id)` 存行、
// 记录每次调用的方法名与入参，并可注入错误。调用名清单同时用来断言
// "吊销不触碰任何任务绑定"——没有第三个方法可被调用。
// lifecycleKeyStore is the in-memory KeyStore fake: it records every call name and
// argument so tests can assert the exact call sequence and injected failures.
type lifecycleKeyStore struct {
	records map[string]KeyRecord

	lookupErr     error
	lookupByIDErr error
	createErr     error
	setStatusErr  error
	rotateErr     error
	rotateResult  RotateResult

	calls       []string
	created     []KeyRecord
	statusCalls []statusCall
	rotateCalls []rotateCall
}

func newLifecycleKeyStore(records ...KeyRecord) *lifecycleKeyStore {
	store := &lifecycleKeyStore{records: map[string]KeyRecord{}}
	for _, record := range records {
		store.records[record.CallerID+"\x00"+record.KeyID] = record
	}
	return store
}

func (s *lifecycleKeyStore) Lookup(_ context.Context, _ string) (KeyRecord, error) {
	s.calls = append(s.calls, "Lookup")
	if s.lookupErr != nil {
		return KeyRecord{}, s.lookupErr
	}
	return KeyRecord{}, ErrNotFound
}

func (s *lifecycleKeyStore) LookupByID(_ context.Context, callerID, keyID string) (KeyRecord, error) {
	s.calls = append(s.calls, "LookupByID")
	if s.lookupByIDErr != nil {
		return KeyRecord{}, s.lookupByIDErr
	}
	record, ok := s.records[callerID+"\x00"+keyID]
	if !ok {
		return KeyRecord{}, ErrNotFound
	}
	return record, nil
}

func (s *lifecycleKeyStore) Create(_ context.Context, record KeyRecord) error {
	s.calls = append(s.calls, "Create")
	if s.createErr != nil {
		return s.createErr
	}
	s.created = append(s.created, record)
	s.records[record.CallerID+"\x00"+record.KeyID] = record
	return nil
}

func (s *lifecycleKeyStore) SetStatus(_ context.Context, callerID, keyID string, status KeyStatus) error {
	s.calls = append(s.calls, "SetStatus")
	if s.setStatusErr != nil {
		return s.setStatusErr
	}
	s.statusCalls = append(s.statusCalls, statusCall{CallerID: callerID, KeyID: keyID, Status: status})
	record := s.records[callerID+"\x00"+keyID]
	record.Status = status
	s.records[callerID+"\x00"+keyID] = record
	return nil
}

func (s *lifecycleKeyStore) Rotate(_ context.Context, callerID, oldKeyID string, next KeyRecord, opts RotateOptions) (RotateResult, error) {
	s.calls = append(s.calls, "Rotate")
	s.rotateCalls = append(s.rotateCalls, rotateCall{
		CallerID: callerID,
		OldKeyID: oldKeyID,
		Next:     next,
		Opts:     opts,
	})
	if s.rotateErr != nil {
		return RotateResult{}, s.rotateErr
	}
	return s.rotateResult, nil
}

// 管理面四方法不在签发路径上：一律拒绝，且**不**进 `calls` 清单——否则"吊销
// 只调 SetStatus"这类序列断言会被无关方法名污染。真实行为由 admin 测试的内存
// 假实现覆盖。
// The admin-facing methods refuse and stay out of the call log; admin tests own
// their real behavior.
func (s *lifecycleKeyStore) List(context.Context) ([]CallerRecord, error) {
	return nil, ErrForbidden
}

func (s *lifecycleKeyStore) LookupCaller(context.Context, string) (CallerRecord, error) {
	return CallerRecord{}, ErrNotFound
}

func (s *lifecycleKeyStore) CreateCaller(context.Context, CallerRecord, *Limit) error {
	return ErrForbidden
}

func (s *lifecycleKeyStore) Usage(context.Context, string, int64, time.Time) (UsageAggregate, error) {
	return UsageAggregate{}, ErrForbidden
}

// finishedOperation 记录一次 FinishOperation 的回填。
type finishedOperation struct {
	OperationID string
	Status      core.OperationStatus
	Code        string
	Message     string
	Detail      []byte
}

// stubOperations 是 OperationStore 的内存实现：只记录预写与回填，其余方法
// 不参与本包逻辑（启动恢复由 app 层驱动）。
type stubOperations struct {
	begun     []core.OperationRecord
	finished  []finishedOperation
	beginErr  error
	finishErr error
}

func (s *stubOperations) BeginOperation(_ context.Context, rec core.OperationRecord) error {
	if s.beginErr != nil {
		return s.beginErr
	}
	s.begun = append(s.begun, rec)
	return nil
}

func (s *stubOperations) FinishOperation(_ context.Context, operationID string, status core.OperationStatus,
	resultCode, errorMessage string, detailJSON []byte, _ time.Time,
) error {
	s.finished = append(s.finished, finishedOperation{
		OperationID: operationID,
		Status:      status,
		Code:        resultCode,
		Message:     errorMessage,
		Detail:      detailJSON,
	})
	return s.finishErr
}

func (s *stubOperations) GetOperation(context.Context, string) (core.OperationRecord, error) {
	return core.OperationRecord{}, errors.New("GetOperation 不在本包路径上")
}

func (s *stubOperations) ListOperations(context.Context, core.OperationFilter) ([]core.OperationRecord, error) {
	return nil, nil
}

func (s *stubOperations) PendingOperations(context.Context) ([]core.OperationRecord, error) {
	return nil, nil
}

func (s *stubOperations) RecoverOperation(context.Context, string, core.OperationStatus, string, []byte, time.Time) error {
	return nil
}

// newTestIssuer 构造注入固定时钟与确定性明文的签发器。
func newTestIssuer(t *testing.T, keys *lifecycleKeyStore) (*KeyIssuer, *stubOperations) {
	t.Helper()
	ops := &stubOperations{}
	issuer := NewKeyIssuer(keys, ops)
	issuer.now = func() time.Time { return time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC) }
	issuer.newSecret = func() (string, error) { return testSecret, nil }
	return issuer, ops
}

// TestParseScopeSet_ValidatesAndDeduplicates 验证发放期 scope 校验的拒绝面
// （空 / 未知 / 通配 / 管理面）与保序去重；逆改点：删掉 AdminOnlyScope 判定后
// 管理面 scope 会被接受。
func TestParseScopeSet_ValidatesAndDeduplicates(t *testing.T) {
	rejected := [][]string{
		nil,
		{},
		{""},
		{"unknown:scope"},
		{core.AgentInvokeWildcard},
		{"*"},
		{core.ScopeDiagnosticsRead},
		{core.ScopeCallersAdmin},
		{core.ScopeSessionsRead, core.ScopeCallersAdmin},
		{core.AgentInvokeScopePrefix},
	}
	for _, raw := range rejected {
		scopes, err := ParseScopeSet(raw)
		require.ErrorIs(t, err, ErrInvalidScope, "scope 集合 %v 必须被拒绝", raw)
		require.Empty(t, scopes.List())
	}

	accepted, err := ParseScopeSet([]string{
		core.ScopeSessionsRead, core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "daily",
	})
	require.NoError(t, err)
	require.Equal(t, []string{core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "daily"},
		accepted.List(), "重复项按首次出现去重且保序")

	listed := accepted.List()
	listed[0] = core.ScopeKnowledgeWrite
	require.Equal(t, core.ScopeSessionsRead, accepted.List()[0], "List 必须返回副本")
}

// TestKeyIssuerIssue_PersistsFingerprintAndPrefixOnly 验证签发只落指纹与前缀：
// 库行、审计 detail 与请求摘要里都不出现明文，且审计字段逐列对齐。
func TestKeyIssuerIssue_PersistsFingerprintAndPrefixOnly(t *testing.T) {
	keys := newLifecycleKeyStore()
	issuer, ops := newTestIssuer(t, keys)

	issued, err := issuer.Issue(context.Background(), testActor(), IssueRequest{
		CallerID: testCallerID,
		Scopes:   []string{core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "daily"},
	})
	require.NoError(t, err)

	require.Equal(t, testSecret, issued.Secret, "明文只在返回值出现一次")
	require.True(t, regexp.MustCompile(`^key_[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(issued.KeyID),
		"Key 标识必须是 key_ + 26 位 Crockford Base32，实际 %q", issued.KeyID)
	require.Equal(t, KeyStatusActive, issued.Status)
	require.Equal(t, testCallerID, issued.CallerID)
	require.Equal(t, []string{core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "daily"}, issued.Scopes)

	require.Len(t, keys.created, 1)
	stored := keys.created[0]
	require.Equal(t, fingerprintOf(testSecret), stored.KeyHash, "库里只有 SHA-256 指纹")
	require.Len(t, stored.KeyHash, 64)
	require.Equal(t, testSecret[:8], stored.KeyPrefix, "前缀是明文的展示用前 8 字符")
	require.NotEqual(t, testSecret, stored.KeyHash)
	require.NotEqual(t, testSecret, stored.KeyPrefix)

	require.Len(t, ops.begun, 1)
	begun := ops.begun[0]
	require.Equal(t, core.OperationPending, begun.Status, "变更前必须先写 pending")
	require.Equal(t, "caller.key.issue", begun.Action)
	require.Equal(t, "caller", begun.TargetKind, "target_kind 冻结为 caller")
	require.Equal(t, testCallerID, begun.TargetID, "target_id = caller_id")
	require.Equal(t, "admin", begun.ActorSubject)
	require.Equal(t, "admin-token", begun.AuthMethod)
	require.Equal(t, testActorSource, begun.Source)
	require.True(t, regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(begun.RequestHash))
	require.NotContains(t, begun.RequestHash, testSecret)
	require.NotContains(t, string(begun.DetailJSON), testSecret)

	require.Len(t, ops.finished, 1)
	require.Equal(t, begun.OperationID, ops.finished[0].OperationID)
	require.Equal(t, core.OperationSuccess, ops.finished[0].Status)
	require.Equal(t, resultOK, ops.finished[0].Code)
	require.NotContains(t, string(ops.finished[0].Detail), testSecret)
	var detail map[string]any
	require.NoError(t, json.Unmarshal(ops.finished[0].Detail, &detail))
	require.Equal(t, issued.KeyID, detail["key_id"])
	require.Equal(t, issued.KeyPrefix, detail["key_prefix"])
	require.Equal(t, float64(0), detail["expires_at_unix_ms"], "未设置过期时刻记 0")
}

// TestKeyIssuerIssue_RecordsExpiryInMilliseconds 验证过期时刻按 Unix 毫秒 UTC
// 落进 Key 行与审计 detail（与库里的时间列同一表示，C-13 / C-14）。
func TestKeyIssuerIssue_RecordsExpiryInMilliseconds(t *testing.T) {
	keys := newLifecycleKeyStore()
	issuer, ops := newTestIssuer(t, keys)
	expiresAt := time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC)

	issued, err := issuer.Issue(context.Background(), testActor(), IssueRequest{
		CallerID:  testCallerID,
		Scopes:    []string{core.ScopeSessionsRead},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	require.NotNil(t, issued.ExpiresAt)
	require.True(t, expiresAt.Equal(*issued.ExpiresAt))
	require.True(t, expiresAt.Equal(*keys.created[0].ExpiresAt))

	var detail map[string]any
	require.NoError(t, json.Unmarshal(ops.finished[0].Detail, &detail))
	require.Equal(t, float64(expiresAt.UnixMilli()), detail["expires_at_unix_ms"])
}

// TestKeyIssuerIssue_RefusesWhenPreWriteFails 验证预写失败即拒绝执行：目标行
// 一个都不写、也没有可回填的终态（宪法 V fail-closed）。
// 逆改点：把 Create 挪到 begin 之前，created 变成 1 行。
func TestKeyIssuerIssue_RefusesWhenPreWriteFails(t *testing.T) {
	keys := newLifecycleKeyStore()
	issuer, ops := newTestIssuer(t, keys)
	ops.beginErr = errors.New("operations store down")

	_, err := issuer.Issue(context.Background(), testActor(), IssueRequest{
		CallerID: testCallerID,
		Scopes:   []string{core.ScopeSessionsRead},
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "预写操作记录失败")
	require.Empty(t, keys.calls, "预写失败不得触碰 Key 存储")
	require.Empty(t, keys.created)
	require.Empty(t, ops.finished)
}

// TestKeyIssuerIssue_AbortsOnEntropyFailure 验证随机源失败时不回落到可预测值，
// 也不留下任何持久痕迹。
func TestKeyIssuerIssue_AbortsOnEntropyFailure(t *testing.T) {
	keys := newLifecycleKeyStore()
	issuer, ops := newTestIssuer(t, keys)
	issuer.newSecret = func() (string, error) { return "", errors.New("no entropy") }

	_, err := issuer.Issue(context.Background(), testActor(), IssueRequest{
		CallerID: testCallerID,
		Scopes:   []string{core.ScopeSessionsRead},
	})

	require.ErrorContains(t, err, "no entropy")
	require.Empty(t, ops.begun, "生成失败不得留 pending")
	require.Empty(t, keys.calls)
}

// TestKeyIssuerIssue_RefusesWithoutPorts 验证装配缺端口时拒绝变更，而不是"先做了再说"。
func TestKeyIssuerIssue_RefusesWithoutPorts(t *testing.T) {
	keys := newLifecycleKeyStore()
	ops := &stubOperations{}

	// 必须传字面量 nil：传 (*lifecycleKeyStore)(nil) 会变成非空接口，绕开装配检查。
	noKeys := NewKeyIssuer(nil, &stubOperations{})
	noKeys.newSecret = func() (string, error) { return testSecret, nil }
	_, err := noKeys.Issue(context.Background(), testActor(), IssueRequest{
		CallerID: testCallerID, Scopes: []string{core.ScopeSessionsRead},
	})
	require.ErrorContains(t, err, "装配错误")

	noOps := NewKeyIssuer(keys, nil)
	noOps.newSecret = func() (string, error) { return testSecret, nil }
	_, err = noOps.Issue(context.Background(), testActor(), IssueRequest{
		CallerID: testCallerID, Scopes: []string{core.ScopeSessionsRead},
	})
	require.ErrorContains(t, err, "装配错误")
	require.Empty(t, keys.calls, "缺操作存储时不得写 Key 行")
	require.Empty(t, ops.begun)
}

// TestKeyIssuerIssue_RejectsEmptyCallerWithoutPending 验证空归属按未找到拒绝，
// 且不产生操作行。
func TestKeyIssuerIssue_RejectsEmptyCallerWithoutPending(t *testing.T) {
	keys := newLifecycleKeyStore()
	issuer, ops := newTestIssuer(t, keys)

	_, err := issuer.Issue(context.Background(), testActor(), IssueRequest{
		Scopes: []string{core.ScopeSessionsRead},
	})

	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, ops.begun)
	require.Empty(t, keys.calls)
}

// TestKeyIssuerIssue_FinishFailureKeepsIssuedKey 验证回填失败不改业务结果：
// 变更已生效，向调用方报失败会诱导重试并签发第二把 Key，因此只记日志。
func TestKeyIssuerIssue_FinishFailureKeepsIssuedKey(t *testing.T) {
	keys := newLifecycleKeyStore()
	issuer, ops := newTestIssuer(t, keys)
	ops.finishErr = errors.New("finish failed")

	issued, err := issuer.Issue(context.Background(), testActor(), IssueRequest{
		CallerID: testCallerID,
		Scopes:   []string{core.ScopeSessionsRead},
	})

	require.NoError(t, err, "回填失败不得伪装成业务失败")
	require.Equal(t, testSecret, issued.Secret)
	require.Len(t, ops.finished, 1, "回填失败仍必须尝试过回填")
	require.Len(t, keys.created, 1)
}

// TestKeyIssuerRevoke_LooksUpBeforePreWrite 验证找不到 Key 时按未找到拒绝，
// 且不产生操作行——避免为一次失败的定位写审计。
// 逆改点：把 begin 挪到 LookupByID 之前，begun 变成 1 行。
func TestKeyIssuerRevoke_LooksUpBeforePreWrite(t *testing.T) {
	keys := newLifecycleKeyStore()
	issuer, ops := newTestIssuer(t, keys)

	err := issuer.Revoke(context.Background(), testActor(), RevokeRequest{
		CallerID: testCallerID,
		KeyID:    "key_01J0000000000000000000000",
		Reason:   "泄露",
	})

	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, ops.begun)
	require.Equal(t, []string{"LookupByID"}, keys.calls)
}

// TestKeyIssuerRevoke_IsIdempotentAndLeavesBindings 验证吊销：重复吊销幂等、
// 只调用 `LookupByID` 与 `SetStatus`（**不**触碰任何任务绑定）、理由进审计。
func TestKeyIssuerRevoke_IsIdempotentAndLeavesBindings(t *testing.T) {
	record := KeyRecord{
		KeyID:     "key_01J0000000000000000000000",
		CallerID:  testCallerID,
		KeyHash:   fingerprintOf("bk_gone_secret_value"),
		KeyPrefix: "bk_gone1",
		Scopes:    []string{core.ScopeSessionsRead},
		Status:    KeyStatusActive,
	}
	keys := newLifecycleKeyStore(record)
	issuer, ops := newTestIssuer(t, keys)
	req := RevokeRequest{CallerID: testCallerID, KeyID: record.KeyID, Reason: "疑似泄露"}

	require.NoError(t, issuer.Revoke(context.Background(), testActor(), req))
	require.NoError(t, issuer.Revoke(context.Background(), testActor(), req), "重复吊销必须幂等")

	require.Equal(t, []string{"LookupByID", "SetStatus", "LookupByID", "SetStatus"}, keys.calls,
		"吊销只允许定位 + 置状态，不得改绑任何任务")
	require.Len(t, keys.statusCalls, 2)
	require.Equal(t, KeyStatusRevoked, keys.statusCalls[0].Status)
	require.Equal(t, record.KeyID, keys.statusCalls[0].KeyID)

	require.Len(t, ops.finished, 2)
	require.Equal(t, core.OperationSuccess, ops.finished[0].Status)
	require.Equal(t, "caller.key.revoke", ops.begun[0].Action)
	require.Equal(t, testCallerID, ops.begun[0].TargetID)
	var detail map[string]any
	require.NoError(t, json.Unmarshal(ops.finished[1].Detail, &detail))
	require.Equal(t, "疑似泄露", detail["reason"])
	require.Equal(t, record.KeyPrefix, detail["key_prefix"])
	require.NotContains(t, string(ops.finished[0].Detail), "bk_gone_secret_value")
}

// TestKeyIssuerRevoke_StoreFailureFinishesAsFailed 验证置状态失败时回填 failed 并如实报错。
func TestKeyIssuerRevoke_StoreFailureFinishesAsFailed(t *testing.T) {
	record := KeyRecord{
		KeyID:     "key_01J0000000000000000000000",
		CallerID:  testCallerID,
		KeyPrefix: "bk_gone1",
		Status:    KeyStatusActive,
	}
	keys := newLifecycleKeyStore(record)
	issuer, ops := newTestIssuer(t, keys)
	keys.setStatusErr = errors.New("row locked")

	err := issuer.Revoke(context.Background(), testActor(), RevokeRequest{
		CallerID: testCallerID, KeyID: record.KeyID,
	})

	require.ErrorContains(t, err, "row locked")
	require.Len(t, ops.finished, 1)
	require.Equal(t, core.OperationFailed, ops.finished[0].Status)
	require.Equal(t, resultInternal, ops.finished[0].Code)
}

// TestKeyIssuerRotate_RejectsCallerMismatch 验证归属不一致按禁止拒绝，
// 且不查询、不留操作行（跨调用方轮换不是本动作）。
func TestKeyIssuerRotate_RejectsCallerMismatch(t *testing.T) {
	keys := newLifecycleKeyStore()
	issuer, ops := newTestIssuer(t, keys)

	_, _, err := issuer.Rotate(context.Background(), testActor(), RotateRequest{
		CallerID: testCallerID,
		OldKeyID: "key_01J0000000000000000000000",
		Next: KeyRecord{
			CallerID: "cal_01J0000000000000000000001",
			Scopes:   []string{core.ScopeSessionsRead},
		},
	})

	require.ErrorIs(t, err, ErrForbidden)
	require.Empty(t, ops.begun)
	require.Empty(t, keys.calls)
}

// TestKeyIssuerRotate_RequiresActiveOldKey 验证非 active 的旧 Key 不可被"轮换救活"，
// 且拒绝发生在任何写入之前。
func TestKeyIssuerRotate_RequiresActiveOldKey(t *testing.T) {
	record := KeyRecord{
		KeyID:    "key_01J0000000000000000000000",
		CallerID: testCallerID,
		Status:   KeyStatusRevoked,
	}
	keys := newLifecycleKeyStore(record)
	issuer, ops := newTestIssuer(t, keys)

	_, _, err := issuer.Rotate(context.Background(), testActor(), RotateRequest{
		CallerID: testCallerID,
		OldKeyID: record.KeyID,
		Next:     KeyRecord{Scopes: []string{core.ScopeSessionsRead}},
	})

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound, "旧 Key 存在但状态不可轮换，不是未找到")
	require.Empty(t, ops.begun)
	require.Equal(t, []string{"LookupByID"}, keys.calls)
}

// TestKeyIssuerRotate_PassesRebindOptionAndReportsLeftBound 验证轮换把再绑定声明
// 原样下传存储层，并把已再绑定 / 未能再绑定的任务逐条写进审计。
func TestKeyIssuerRotate_PassesRebindOptionAndReportsLeftBound(t *testing.T) {
	old := KeyRecord{
		KeyID:    "key_01J0000000000000000000000",
		CallerID: testCallerID,
		Scopes:   []string{core.ScopeSessionsRead},
		Status:   KeyStatusActive,
	}
	keys := newLifecycleKeyStore(old)
	issuer, ops := newTestIssuer(t, keys)
	keys.rotateResult = RotateResult{
		Key: KeyRecord{
			KeyID:     "key_01J0000000000000000000001",
			CallerID:  testCallerID,
			KeyHash:   fingerprintOf(testSecret),
			KeyPrefix: testSecret[:8],
			Scopes:    []string{core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "daily"},
			Status:    KeyStatusActive,
		},
		ReboundTaskIDs: []string{"task_a"},
		LeftBound:      []BoundTask{{TaskID: "task_b", Reason: BoundTaskScopeMissing}},
	}

	issued, result, err := issuer.Rotate(context.Background(), testActor(), RotateRequest{
		CallerID:             testCallerID,
		OldKeyID:             old.KeyID,
		Next:                 KeyRecord{Scopes: []string{core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "daily"}},
		RebindScheduledTasks: true,
	})
	require.NoError(t, err)

	require.Equal(t, testSecret, issued.Secret)
	require.Equal(t, keys.rotateResult.Key.KeyID, issued.KeyID)
	require.Equal(t, []string{"task_a"}, result.ReboundTaskIDs)
	require.Equal(t, []BoundTask{{TaskID: "task_b", Reason: BoundTaskScopeMissing}}, result.LeftBound)

	require.Len(t, keys.rotateCalls, 1)
	require.True(t, keys.rotateCalls[0].Opts.RebindScheduledTasks, "再绑定必须由请求显式声明")
	require.Equal(t, old.KeyID, keys.rotateCalls[0].OldKeyID)
	require.Equal(t, fingerprintOf(testSecret), keys.rotateCalls[0].Next.KeyHash,
		"新 Key 行由签发方生成，只把指纹交给存储层")
	require.Regexp(t, `^key_[0-9A-HJKMNP-TV-Z]{26}$`, keys.rotateCalls[0].Next.KeyID,
		"Key 标识只能由签发方生成，不接受调用方指定")

	require.Len(t, ops.finished, 1)
	require.Equal(t, core.OperationSuccess, ops.finished[0].Status)
	require.Equal(t, "caller.key.rotate", ops.begun[0].Action)
	require.NotContains(t, string(ops.finished[0].Detail), testSecret)
	var detail struct {
		OldKeyID       string              `json:"old_key_id"`
		ReboundTaskIDs []string            `json:"rebound_task_ids"`
		LeftBound      []map[string]string `json:"left_bound"`
	}
	require.NoError(t, json.Unmarshal(ops.finished[0].Detail, &detail))
	require.Equal(t, old.KeyID, detail.OldKeyID)
	require.Equal(t, []string{"task_a"}, detail.ReboundTaskIDs)
	require.Equal(t, []map[string]string{{"task_id": "task_b", "reason": BoundTaskScopeMissing}}, detail.LeftBound)
}

// TestKeyIssuerRotate_FailureFinishesAsFailed 验证存储事务失败时回填 failed，
// 且明文不再返回（该次签发未生效）。
func TestKeyIssuerRotate_FailureFinishesAsFailed(t *testing.T) {
	old := KeyRecord{
		KeyID:    "key_01J0000000000000000000000",
		CallerID: testCallerID,
		Scopes:   []string{core.ScopeSessionsRead},
		Status:   KeyStatusActive,
	}
	keys := newLifecycleKeyStore(old)
	issuer, ops := newTestIssuer(t, keys)
	keys.rotateErr = errors.New("transaction rolled back")

	issued, result, err := issuer.Rotate(context.Background(), testActor(), RotateRequest{
		CallerID: testCallerID,
		OldKeyID: old.KeyID,
		Next:     KeyRecord{Scopes: []string{core.ScopeSessionsRead}},
	})

	require.ErrorContains(t, err, "transaction rolled back")
	require.Empty(t, issued.Secret, "事务失败时不得返回明文")
	require.Empty(t, result)
	require.Len(t, ops.finished, 1)
	require.Equal(t, core.OperationFailed, ops.finished[0].Status)
	require.Equal(t, resultInternal, ops.finished[0].Code)
	require.NotContains(t, string(ops.finished[0].Detail), testSecret)
}

// TestKeySecret_FormatShape 验证明文格式与指纹 / 前缀派生的口径：每次生成不同，
// 指纹与小写十六进制一致。
func TestKeySecret_FormatShape(t *testing.T) {
	shape := regexp.MustCompile(`^bk_[0-9a-f]{40}$`)

	first, err := newKeySecret()
	require.NoError(t, err)
	second, err := newKeySecret()
	require.NoError(t, err)

	require.True(t, shape.MatchString(first), "明文格式不符：%q", first)
	require.True(t, shape.MatchString(second))
	require.NotEqual(t, first, second, "两把 Key 不得相同")
	require.Len(t, keyPrefix(first), 8)
	require.Equal(t, fingerprintOf(first), keyFingerprint(first))
	require.True(t, regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(keyFingerprint(first)))
}
