package callers

// 本文件覆盖 T019 认证路径的验收点（颗粒度文档 §3.1.5 / 图 6 [0] / C-28）：
// admin token 常量时间比对映射内置管理员；caller key 只按指纹查表；状态、过期与
// 作用域默认拒绝，且所有凭据失败的对外表现完全一致。
// The tests pin the T019 acceptance points: constant-time admin matching, fingerprint
// only key lookup, default deny on status, expiry and scopes, and indistinguishable
// credential failures.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// stubAdminAuth 是按 token 指纹注入的管理凭据来源；生产实现由装配层提供。
type stubAdminAuth struct {
	fingerprint [sha256.Size]byte
	configured  bool
}

func (s stubAdminAuth) AdminTokenFingerprint() ([sha256.Size]byte, bool) {
	return s.fingerprint, s.configured
}

// newStubAdminAuth 用明文 token 构造指纹来源（指纹算法与认证器一致）。
func newStubAdminAuth(token string) stubAdminAuth {
	return stubAdminAuth{fingerprint: sha256.Sum256([]byte(token)), configured: true}
}

// keyLifecycleUnused 给只测认证的桩补上 `KeyStore` 的生命周期方法：认证路径
// **不得**触碰它们，所以这里只提供「一律未找到 / 一律报错」的零值实现。真实行为
// 由 key_test.go 的 `lifecycleKeyStore` 覆盖——认证测试借此在结构上也无法"顺手"写库。
// keyLifecycleUnused completes the KeyStore port for authentication-only stubs; the
// lifecycle methods refuse every call, so the auth tests cannot write behind their back.
type keyLifecycleUnused struct{}

func (keyLifecycleUnused) LookupByID(context.Context, string, string) (KeyRecord, error) {
	return KeyRecord{}, ErrNotFound
}

func (keyLifecycleUnused) Create(context.Context, KeyRecord) error {
	return ErrForbidden
}

func (keyLifecycleUnused) SetStatus(context.Context, string, string, KeyStatus) error {
	return ErrForbidden
}

func (keyLifecycleUnused) Rotate(context.Context, string, string, KeyRecord, RotateOptions) (RotateResult, error) {
	return RotateResult{}, ErrForbidden
}

// 管理面四方法同样一律拒绝：认证测试不碰它们，管理面行为由 admin_test.go 覆盖。
// The admin methods refuse as well; admin_test.go covers their real behavior.
func (keyLifecycleUnused) List(context.Context) ([]CallerRecord, error) {
	return nil, ErrForbidden
}

func (keyLifecycleUnused) LookupCaller(context.Context, string) (CallerRecord, error) {
	return CallerRecord{}, ErrNotFound
}

func (keyLifecycleUnused) CreateCaller(context.Context, CallerRecord, *Limit) error {
	return ErrForbidden
}

func (keyLifecycleUnused) Usage(context.Context, string, int64, time.Time) (UsageAggregate, error) {
	return UsageAggregate{}, ErrForbidden
}

// stubKeyStore 记录每次查表的指纹，便于断言"只按指纹"。
type stubKeyStore struct {
	keyLifecycleUnused

	record KeyRecord
	err    error
	calls  []string
}

func (s *stubKeyStore) Lookup(_ context.Context, fingerprint string) (KeyRecord, error) {
	s.calls = append(s.calls, fingerprint)
	if s.err != nil {
		return KeyRecord{}, s.err
	}
	return s.record, nil
}

// activeRecord 返回一条仅缺指纹的可用记录；测试各自补 KeyHash。
func activeRecord() KeyRecord {
	return KeyRecord{
		KeyID:     "key_01J0000000000000000000000",
		CallerID:  "cal_01J0000000000000000000000",
		KeyPrefix: "bk_live",
		Scopes:    []string{core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "daily"},
		Status:    KeyStatusActive,
	}
}

func fingerprintOf(presented string) string {
	sum := sha256.Sum256([]byte(presented))
	return hex.EncodeToString(sum[:])
}

// TestAuthenticator_AdminTokenReturnsLocalAdminPrincipal 验证管理 token 命中后
// 直接构造内置管理员，且完全不触碰调用方 Key 存储。
func TestAuthenticator_AdminTokenReturnsLocalAdminPrincipal(t *testing.T) {
	auth := NewAuthenticator(nil, newStubAdminAuth("admin-token-value"))

	p, err := auth.Authenticate(context.Background(), "admin-token-value")

	require.NoError(t, err)
	require.Equal(t, core.PrincipalLocalAdmin, p.Kind)
	require.Equal(t, core.LocalAdminCallerID, p.CallerID)
	require.Empty(t, p.KeyID)
	require.True(t, p.Builtin)
	require.Equal(t, core.ActorCaller, p.Actor)
	require.True(t, p.Valid())
}

// TestAuthenticator_AdminTokenNotConfiguredNeverGrantsAdmin 验证 local_trust
// 姿态下（token 形态未启用）任何字符串都不得被提升为管理员。
func TestAuthenticator_AdminTokenNotConfiguredNeverGrantsAdmin(t *testing.T) {
	record := activeRecord()
	record.KeyHash = fingerprintOf("bk_live")
	keys := &stubKeyStore{record: record}
	admin := stubAdminAuth{fingerprint: sha256.Sum256([]byte("bk_live")), configured: false}
	auth := NewAuthenticator(keys, admin)

	p, err := auth.Authenticate(context.Background(), "bk_live")

	require.NoError(t, err)
	require.Equal(t, core.PrincipalCaller, p.Kind, "未启用 token 形态时不得构造管理员")
	require.False(t, p.Builtin)
	require.Equal(t, record.CallerID, p.CallerID)
}

// TestAuthenticator_NilAdminSourceNeverGrantsAdmin 验证未装配管理凭据来源时
// 认证器只走调用方 Key 路径；nil 不构成放行理由。
func TestAuthenticator_NilAdminSourceNeverGrantsAdmin(t *testing.T) {
	auth := NewAuthenticator(nil, nil)

	_, err := auth.Authenticate(context.Background(), "anything")

	require.Equal(t, ErrUnauthorized, err)
}

// TestAuthenticator_CallerKeyResolvesPrincipal 验证调用方 Key 解析出的身份字段
// 与 scope 副本：归属、Key 标识、执行者类别与不可变拷贝。
func TestAuthenticator_CallerKeyResolvesPrincipal(t *testing.T) {
	record := activeRecord()
	record.KeyHash = fingerprintOf("bk_live_secret")
	keys := &stubKeyStore{record: record}
	auth := NewAuthenticator(keys, nil)

	p, err := auth.Authenticate(context.Background(), "bk_live_secret")

	require.NoError(t, err)
	require.Equal(t, core.PrincipalCaller, p.Kind)
	require.Equal(t, record.CallerID, p.CallerID)
	require.Equal(t, record.KeyID, p.KeyID)
	require.False(t, p.Builtin)
	require.Equal(t, core.ActorCaller, p.Actor)
	require.True(t, p.Valid())
	require.True(t, p.HasScope(core.ScopeSessionsRead))
	require.True(t, p.HasAgentScope("daily"))

	// 构造后改写入参切片不得影响已发出的身份。
	record.Scopes[0] = core.ScopeKnowledgeWrite
	require.True(t, p.HasScope(core.ScopeSessionsRead), "Principal 必须持有 scope 副本")
	require.False(t, p.HasScope(core.ScopeKnowledgeWrite))
}

// TestAuthenticator_UsesFingerprintOnly 验证查表键是 SHA-256 十六进制指纹：
// 明文既不入查询参数，也不出现在任何对外错误里。
func TestAuthenticator_UsesFingerprintOnly(t *testing.T) {
	record := activeRecord()
	record.KeyHash = fingerprintOf("bk_live_secret")
	keys := &stubKeyStore{record: record}
	auth := NewAuthenticator(keys, nil)

	_, err := auth.Authenticate(context.Background(), "bk_live_secret")
	require.NoError(t, err)

	require.Len(t, keys.calls, 1)
	require.Equal(t, fingerprintOf("bk_live_secret"), keys.calls[0])
	require.NotEqual(t, "bk_live_secret", keys.calls[0], "明文不得作为查表键")

	// 失败路径同样不得回显明文。
	failing := &stubKeyStore{err: fmt.Errorf("lookup failed")}
	auth = NewAuthenticator(failing, nil)
	_, err = auth.Authenticate(context.Background(), "bk_live_secret")
	require.Equal(t, ErrUnauthorized, err)
	require.NotContains(t, err.Error(), "bk_live_secret")
}

// TestAuthenticator_ExpiryBoundary 验证过期判定用注入时钟：到期时刻本身即失效，
// 未过期与不过期仍可认证。
func TestAuthenticator_ExpiryBoundary(t *testing.T) {
	now := time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC)
	record := activeRecord()
	record.KeyHash = fingerprintOf("bk_live_secret")

	expired := now
	alive := now.Add(time.Millisecond)

	tests := []struct {
		name      string
		expiresAt *time.Time
		wantErr   bool
	}{
		{name: "不过期", expiresAt: nil, wantErr: false},
		{name: "到期时刻即失效", expiresAt: &expired, wantErr: true},
		{name: "尚未到期", expiresAt: &alive, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := record
			current.ExpiresAt = tt.expiresAt
			auth := NewAuthenticator(&stubKeyStore{record: current}, nil)
			auth.now = func() time.Time { return now }

			p, err := auth.Authenticate(context.Background(), "bk_live_secret")

			if tt.wantErr {
				require.Equal(t, ErrUnauthorized, err)
				require.Equal(t, core.Principal{}, p)
				return
			}
			require.NoError(t, err)
			require.Equal(t, record.CallerID, p.CallerID)
		})
	}
}

// TestAuthenticator_RejectsIndistinguishably 验证所有凭据失败都返回同一个
// ErrUnauthorized、同一个文案与同一个零值身份：不存在 / 吊销 / 过期 / 作用域
// 非法在响应面上不可区分。
func TestAuthenticator_RejectsIndistinguishably(t *testing.T) {
	base := activeRecord()
	base.KeyHash = fingerprintOf("bk_live_secret")

	revoked := base
	revoked.Status = KeyStatusRevoked
	expired := base
	expired.Status = KeyStatusExpired
	unknownStatus := base
	unknownStatus.Status = KeyStatus("pending")
	missingKeyID := base
	missingKeyID.KeyID = ""
	missingCallerID := base
	missingCallerID.CallerID = ""
	hashMismatch := base
	hashMismatch.KeyHash = fingerprintOf("another-key")
	emptyScopes := base
	emptyScopes.Scopes = nil
	unknownScope := base
	unknownScope.Scopes = []string{"unknown:scope"}
	adminScope := base
	adminScope.Scopes = []string{core.ScopeDiagnosticsRead}
	callersAdminScope := base
	callersAdminScope.Scopes = []string{core.ScopeCallersAdmin}
	wildcardScope := base
	wildcardScope.Scopes = []string{core.AgentInvokeWildcard}
	reservedOwner := base
	reservedOwner.CallerID = core.LocalAdminCallerID

	tests := []struct {
		name      string
		presented string
		store     *stubKeyStore
	}{
		{name: "空凭据", presented: "", store: &stubKeyStore{record: base}},
		{name: "存储报错", presented: "bk_live_secret", store: &stubKeyStore{err: fmt.Errorf("not found")}},
		{name: "已吊销", presented: "bk_live_secret", store: &stubKeyStore{record: revoked}},
		{name: "已过期状态", presented: "bk_live_secret", store: &stubKeyStore{record: expired}},
		{name: "未知状态", presented: "bk_live_secret", store: &stubKeyStore{record: unknownStatus}},
		{name: "缺 Key 标识", presented: "bk_live_secret", store: &stubKeyStore{record: missingKeyID}},
		{name: "缺调用方归属", presented: "bk_live_secret", store: &stubKeyStore{record: missingCallerID}},
		{name: "指纹不一致", presented: "bk_live_secret", store: &stubKeyStore{record: hashMismatch}},
		{name: "空作用域", presented: "bk_live_secret", store: &stubKeyStore{record: emptyScopes}},
		{name: "未知作用域", presented: "bk_live_secret", store: &stubKeyStore{record: unknownScope}},
		{name: "管理面作用域", presented: "bk_live_secret", store: &stubKeyStore{record: adminScope}},
		{name: "调用方管理作用域", presented: "bk_live_secret", store: &stubKeyStore{record: callersAdminScope}},
		{name: "通配调用作用域", presented: "bk_live_secret", store: &stubKeyStore{record: wildcardScope}},
		{name: "保留归属标识", presented: "bk_live_secret", store: &stubKeyStore{record: reservedOwner}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := NewAuthenticator(tt.store, nil)

			p, err := auth.Authenticate(context.Background(), tt.presented)

			require.Equal(t, ErrUnauthorized, err)
			require.Equal(t, ErrUnauthorized.Error(), err.Error(), "失败文案必须完全一致")
			require.Equal(t, core.Principal{}, p, "失败不得返回半成品身份")
			if tt.presented == "" {
				require.Empty(t, tt.store.calls, "空凭据不得查表")
			} else {
				require.Len(t, tt.store.calls, 1)
			}
		})
	}
}

// TestAuthenticator_NoAdminEnabledAccessor 守住 C-28 的结构约束：认证器不得
// 提供"从凭据是否存在推断管理能力"的入口——那会把 local_trust 误判成关闭。
func TestAuthenticator_NoAdminEnabledAccessor(t *testing.T) {
	typ := reflect.TypeOf(&Authenticator{})

	_, found := typ.MethodByName("AdminEnabled")
	require.False(t, found, "Authenticator 不得提供 AdminEnabled()——管理能力只由 core.AdminPosture 表达")
}

// ExampleNewAuthenticator 演示调用方 Key 的完整认证序列：按指纹查表、构造身份。
func ExampleNewAuthenticator() {
	keys := &stubKeyStore{record: KeyRecord{
		KeyID:    "key_example",
		CallerID: "cal_example",
		KeyHash:  fingerprintOf("bk_example"),
		Scopes:   []string{core.ScopeSessionsRead},
		Status:   KeyStatusActive,
	}}
	auth := NewAuthenticator(keys, nil)

	p, err := auth.Authenticate(context.Background(), "bk_example")
	fmt.Println(p.Kind, p.CallerID, p.KeyID, err)

	// Output:
	// caller cal_example key_example <nil>
}
