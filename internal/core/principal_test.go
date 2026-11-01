package core_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 身份是默认拒绝的：零值、缺 Actor、冒充内置管理员、携带管理面 scope 都必须
// 判非法——任何一条放宽都等于放过一种提权。
// Identity is deny-by-default: the zero value, an empty actor, admin impersonation
// and admin-only scopes must all be rejected.

func TestPrincipal_ZeroValueIsInvalid(t *testing.T) {
	var zero core.Principal
	assert.False(t, zero.Valid(), "零值 Principal 必须非法——不得默认成 local-admin")
	assert.False(t, zero.HasScope(core.ScopeSessionsRead), "零值不得持有任何 scope")
	assert.False(t, zero.HasAgentScope("reporter"))

	// 设计卡 §4.3 的原样用例：结构体字面量漏设 Actor 时同样非法。
	// 逆改点：Valid 里去掉 Actor 判定 → 本用例变绿。
	noActor := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "c1",
		Scopes:   []string{core.ScopeSessionsWrite},
	}
	assert.False(t, noActor.Valid(), "Actor 为空同样非法——不得退化成 caller 默认值")
}

func TestPrincipal_LocalAdminIsBuiltin(t *testing.T) {
	admin := core.LocalAdminPrincipal()
	require.True(t, admin.Valid())
	assert.Equal(t, core.PrincipalLocalAdmin, admin.Kind)
	assert.Equal(t, core.LocalAdminCallerID, admin.CallerID)
	assert.True(t, admin.Builtin)
	assert.Empty(t, admin.KeyID)
	assert.Equal(t, core.ActorCaller, admin.Actor)
	assert.True(t, admin.HasScope(core.ScopeCallersAdmin), "内置管理员隐含全部 scope")
	assert.True(t, admin.HasAgentScope("reporter"))

	cli := core.LocalAdminCLIPrincipal()
	require.True(t, cli.Valid())
	assert.Equal(t, core.ActorCLIAdmin, cli.Actor, "CLI 身份与管理员只差审计标注")
	assert.Equal(t, admin.Kind, cli.Kind)

	// 逆改点：Valid 不再要求 Kind=local-admin 时 CallerID 固定且 Builtin 为真
	// → 下面三条断言里对应的一条变绿。
	fakeOwner := admin
	fakeOwner.CallerID = "c1"
	assert.False(t, fakeOwner.Valid(), "内置管理员的归属不可改写")
	notBuiltin := admin
	notBuiltin.Builtin = false
	assert.False(t, notBuiltin.Valid(), "内置管理员必须 Builtin=true")
	withKey := admin
	withKey.KeyID = "key-1"
	assert.False(t, withKey.Valid(), "内置管理员没有授权 Key")
}

func TestPrincipal_CallerShapeValidation(t *testing.T) {
	valid := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-1",
		KeyID:    "key-1",
		Scopes:   []string{core.ScopeSessionsRead, core.ScopeSessionsWrite},
		Actor:    core.ActorCaller,
	}
	require.True(t, valid.Valid())

	cases := map[string]struct {
		mutate func(*core.Principal)
		reason string
	}{
		"空归属": {
			mutate: func(p *core.Principal) { p.CallerID = "" },
			reason: "CallerID 必须非空",
		},
		"冒充本地管理员": {
			mutate: func(p *core.Principal) { p.CallerID = core.LocalAdminCallerID },
			reason: "保留标识不得作为调用方归属",
		},
		"自封 Builtin": {
			mutate: func(p *core.Principal) { p.Builtin = true },
			reason: "调用方不得声称内置身份",
		},
		"空 scope": {
			mutate: func(p *core.Principal) { p.Scopes = nil },
			reason: "空 scope 的调用方身份不成立",
		},
		"未知 scope": {
			mutate: func(p *core.Principal) { p.Scopes = []string{"sessions:delete"} },
			reason: "词表外的 scope 一律非法",
		},
		"管理面 scope": {
			mutate: func(p *core.Principal) { p.Scopes = []string{core.ScopeCallersAdmin} },
			reason: "管理面 scope 只随内置管理员生效",
		},
		"通配 Agent scope": {
			mutate: func(p *core.Principal) { p.Scopes = []string{core.AgentInvokeWildcard} },
			reason: "通配只允许管理员发放，调用方身份不得携带",
		},
		"空 Actor": {
			mutate: func(p *core.Principal) { p.Actor = "" },
			reason: "执行者类别必须显式",
		},
		"未知 Actor": {
			mutate: func(p *core.Principal) { p.Actor = "robot" },
			reason: "执行者类别是闭集",
		},
		"未知 Kind": {
			mutate: func(p *core.Principal) { p.Kind = "service" },
			reason: "来源类别是闭集",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := valid
			p.Scopes = append([]string(nil), valid.Scopes...)
			tc.mutate(&p)
			assert.False(t, p.Valid(), tc.reason)
		})
	}
}

func TestPrincipal_HasScopeAndAgentScope(t *testing.T) {
	p := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-1",
		Scopes:   []string{core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "reporter"},
		Actor:    core.ActorCaller,
	}
	require.True(t, p.Valid())

	assert.True(t, p.HasScope(core.ScopeSessionsRead))
	assert.False(t, p.HasScope(core.ScopeSessionsWrite), "scope 精确匹配，不做通配展开")
	assert.True(t, p.HasAgentScope("reporter"))
	assert.False(t, p.HasAgentScope("researcher"), "具名 scope 只覆盖对应 Agent")
	assert.False(t, p.HasAgentScope(""), "空 Agent 名不命中任何 scope")

	// 通配只随管理员生效：即使字面量出现在调用方 scope 里（Valid 已拒绝），
	// HasAgentScope 也不得因此放行。
	wildcard := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-1",
		Scopes:   []string{core.AgentInvokeWildcard},
		Actor:    core.ActorCaller,
	}
	assert.False(t, wildcard.Valid())
	assert.False(t, wildcard.HasAgentScope("reporter"))
}

func TestOwnedPrincipal_KeepsCallerKeyOwner(t *testing.T) {
	p, err := core.OwnedPrincipal(
		"caller-1",
		core.AuthorizingKindCallerKey,
		"key-1",
		[]string{core.AgentInvokeScopePrefix + "reporter"},
		core.ActorScheduler,
	)
	require.NoError(t, err)
	require.True(t, p.Valid())
	assert.Equal(t, core.PrincipalCaller, p.Kind)
	assert.Equal(t, "caller-1", p.CallerID, "归属恒为任务原始调用方")
	assert.Equal(t, "key-1", p.KeyID, "Key 是任务绑定的原授权 Key")
	assert.Equal(t, core.ActorScheduler, p.Actor)
	assert.False(t, p.Builtin)
	assert.True(t, p.HasAgentScope("reporter"))

	recovery, err := core.OwnedPrincipal(
		"caller-1",
		core.AuthorizingKindCallerKey,
		"key-1",
		[]string{core.ScopeSessionsWrite},
		core.ActorRecovery,
	)
	require.NoError(t, err)
	assert.Equal(t, core.ActorRecovery, recovery.Actor)
	assert.Equal(t, "caller-1", recovery.CallerID, "恢复不得把归属改成执行者")
}

func TestOwnedPrincipal_AdminBuiltinKeepsActor(t *testing.T) {
	p, err := core.OwnedPrincipal(
		core.LocalAdminCallerID,
		core.AuthorizingKindAdminBuiltin,
		"",
		nil,
		core.ActorScheduler,
	)
	require.NoError(t, err)
	require.True(t, p.Valid())
	assert.Equal(t, core.PrincipalLocalAdmin, p.Kind)
	assert.True(t, p.Builtin)
	assert.Empty(t, p.KeyID)
	assert.Equal(t, core.ActorScheduler, p.Actor,
		"后台管理员任务必须保留执行者标注——不得回落到 LocalAdminPrincipal 的 caller")
}

func TestOwnedPrincipal_RejectsWithReason(t *testing.T) {
	callerScopes := []string{core.ScopeSessionsRead}
	cases := map[string]struct {
		callerID string
		kind     core.AuthorizingKind
		keyID    string
		scopes   []string
		actor    core.ActorKind
		reason   core.OwnerReason
	}{
		"caller-key 缺 Key": {
			callerID: "caller-1", kind: core.AuthorizingKindCallerKey,
			scopes: callerScopes, actor: core.ActorScheduler,
			reason: core.OwnerReasonKeyMissing,
		},
		"caller-key 空归属": {
			kind: core.AuthorizingKindCallerKey, keyID: "key-1",
			scopes: callerScopes, actor: core.ActorScheduler,
			reason: core.OwnerReasonKindMismatch,
		},
		"caller-key 用管理员归属": {
			callerID: core.LocalAdminCallerID, kind: core.AuthorizingKindCallerKey,
			keyID: "key-1", scopes: callerScopes, actor: core.ActorScheduler,
			reason: core.OwnerReasonKindMismatch,
		},
		"caller-key 空 scope": {
			callerID: "caller-1", kind: core.AuthorizingKindCallerKey,
			keyID: "key-1", actor: core.ActorScheduler,
			reason: core.OwnerReasonKindMismatch,
		},
		"caller-key 管理面 scope": {
			callerID: "caller-1", kind: core.AuthorizingKindCallerKey,
			keyID: "key-1", scopes: []string{core.ScopeCallersAdmin},
			actor: core.ActorScheduler, reason: core.OwnerReasonKindMismatch,
		},
		"admin-builtin 普通归属": {
			callerID: "caller-1", kind: core.AuthorizingKindAdminBuiltin,
			actor: core.ActorScheduler, reason: core.OwnerReasonKindMismatch,
		},
		"admin-builtin 带 Key": {
			callerID: core.LocalAdminCallerID, kind: core.AuthorizingKindAdminBuiltin,
			keyID: "key-1", actor: core.ActorScheduler,
			reason: core.OwnerReasonKindMismatch,
		},
		"admin-builtin 带 scope": {
			callerID: core.LocalAdminCallerID, kind: core.AuthorizingKindAdminBuiltin,
			scopes: callerScopes, actor: core.ActorScheduler,
			reason: core.OwnerReasonKindMismatch,
		},
		"未知授权来源": {
			callerID: "caller-1", kind: "delegated", keyID: "key-1",
			scopes: callerScopes, actor: core.ActorScheduler,
			reason: core.OwnerReasonKindMismatch,
		},
		"空授权来源": {
			callerID: "caller-1", keyID: "key-1",
			scopes: callerScopes, actor: core.ActorScheduler,
			reason: core.OwnerReasonKindMismatch,
		},
		"外部请求借后台入口": {
			callerID: "caller-1", kind: core.AuthorizingKindCallerKey, keyID: "key-1",
			scopes: callerScopes, actor: core.ActorCaller,
			reason: core.OwnerReasonActorInvalid,
		},
		"CLI 借后台入口": {
			callerID: core.LocalAdminCallerID, kind: core.AuthorizingKindAdminBuiltin,
			actor: core.ActorCLIAdmin, reason: core.OwnerReasonActorInvalid,
		},
		"空执行者": {
			callerID: "caller-1", kind: core.AuthorizingKindCallerKey, keyID: "key-1",
			scopes: callerScopes, reason: core.OwnerReasonActorInvalid,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := core.OwnedPrincipal(tc.callerID, tc.kind, tc.keyID, tc.scopes, tc.actor)
			require.Error(t, err, "组合不成立时必须报错，不得静默补默认值")
			assert.Equal(t, core.Principal{}, p, "失败时不得返回半成品身份")

			var principalErr *core.PrincipalError
			require.True(t, errors.As(err, &principalErr), "错误必须携带结构化的原因码")
			assert.Equal(t, tc.reason, principalErr.Reason)
			assert.ErrorIs(t, err, core.ErrUnauthorized, "构造失败属于授权不可用")
		})
	}
}

func TestOwnedPrincipal_CopiesScopes(t *testing.T) {
	scopes := []string{core.ScopeSessionsRead}
	p, err := core.OwnedPrincipal(
		"caller-1", core.AuthorizingKindCallerKey, "key-1", scopes, core.ActorRecovery,
	)
	require.NoError(t, err)

	scopes[0] = core.ScopeSessionsWrite
	assert.True(t, p.HasScope(core.ScopeSessionsRead), "Scopes 必须是不受调用方后续改写影响的副本")
	assert.False(t, p.HasScope(core.ScopeSessionsWrite))
}

// TestPrincipal_ZeroValueRejected 守 C-28 的身份默认拒绝：未显式设置
// `RunRequest.Principal` 的请求在**任何副作用之前**以 `ErrUnauthorized` 拒绝——
// 不落账本、不触达模型；补了 Kind / CallerID 但漏掉 Actor 的身份同样拒绝，
// **不得**退化成 caller 默认值或补成内置管理员。
// 逆改点：删掉 Run 的 ⓪ 准入，或把零值补成 `LocalAdminPrincipal()` → 第一个 Run
// 触达模型（provider 计数非 0）且账本多出一行，两条断言同时红。
func TestPrincipal_ZeroValueRejected(t *testing.T) {
	h := newLoopHarness(t)

	// 设计卡 R11 的原样用例：只给 Message，身份整个缺失。
	_, err := h.service.Run(ctx, core.RunRequest{Message: "你好"})
	require.ErrorIs(t, err, core.ErrUnauthorized, "缺 Principal 必须拒绝，不得默认成管理员")
	require.Zero(t, h.provider.callCount(), "零值身份不得触达模型")

	h.store.mu.Lock()
	rows := len(h.store.runs)
	h.store.mu.Unlock()
	require.Zero(t, rows, "拒绝发生在任何副作用之前（账本零行）")

	// 结构体字面量漏设 Actor：Kind / CallerID / Scopes 齐全也仍然非法。
	noActor := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-1",
		Scopes:   []string{core.ScopeSessionsWrite},
	}
	_, err = h.service.Run(ctx, core.RunRequest{Message: "你好", Principal: noActor})
	require.ErrorIs(t, err, core.ErrUnauthorized, "Actor 为空不得退化成 caller 默认值")
	require.Zero(t, h.provider.callCount())
}

// TestRunRequest_CarriesPrincipalExplicitly 守 C-06：身份只经
// `RunRequest.Principal` 显式进入服务层——会话归属与账本行都取 `Principal.CallerID`，
// **不得**从 `UserID` / `Channel` 反推，也没有从 context 取身份的路径。
// 逆改点：把 UpsertSession 的 CallerID 改成 `req.UserID` → 归属断言红；账本行
// 不再携带 Principal → 账本断言红。
func TestRunRequest_CarriesPrincipalExplicitly(t *testing.T) {
	h := newLoopHarness(t)
	principal := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-1",
		KeyID:    "key-1",
		Scopes:   []string{core.ScopeSessionsWrite, core.AgentInvokeScopePrefix + loopAgentName},
		Actor:    core.ActorCaller,
	}
	require.True(t, principal.Valid())

	req := h.req("你好")
	req.Principal = principal
	req.UserID = "someone-else" // 归属来源的干扰项：身份不得从 UserID 反推
	h.provider.onCall(0, textResponse("好的"))

	res, err := h.service.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	h.store.mu.Lock()
	session, sessionOK := h.store.sessions[h.sessionID]
	runRec, runOK := h.store.runs[res.RunID]
	h.store.mu.Unlock()
	require.True(t, sessionOK, "有 session_id 的运行必须落会话行")
	require.True(t, runOK, "运行必须落账本行")

	require.Equal(t, "caller-1", session.CallerID, "会话归属取 Principal.CallerID")
	require.NotEqual(t, req.UserID, session.CallerID, "不得从 UserID 反推归属")
	require.Equal(t, principal, runRec.Principal, "账本行携带同一份显式身份")
}
