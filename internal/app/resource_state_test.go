package app

// 本文件是资源状态动作（撤销托管凭据 / 切换使用范围）的判卷（模块卡 R4.4 / R4.8 / T139）：
//   - 三件纯请求校验（`confirm` / `resource_uid` / `If-Match`）全部排在预写之前：任一件
//     失败都不留操作行，目标行逐字段不变；
//   - 撤销成功写 `model_connection.credential.revoke` 行、把 `If-Match` 原件落进
//     `expected_etag`、回读版本数与撤销后的条件 ETag；重放幂等且不新开操作行；
//   - 范围切换预写四证（revision 前后值、范围与属主前后值）→ 终态 success，revision
//     只推进一次；同目标重放不新开操作行；
//   - 被绑定占用 → 409，操作行终态 failed 并带 `operation_id`，指针零变化；
//   - 属主不存在 / 已归档 → 404 / 409，且零操作行（属主解析排在预写之前）；
//   - 预写失败零写入（FR-001）；
//   - 通知渠道走**同一份**渠道 ETag 配方，并给出 `all_eligible` 的影响提示。
//
// These tests pin the two state actions: every pure check precedes the pending row, the
// scope conversion carries four evidences, and blocked or failing writes leave the
// target untouched.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// resourceStateEnv 在解析器夹具之上装配两个状态动作：同一个工作区、同一份库句柄、
// 与生产同形的 `ResourceAdmin` / `ScopeService` / 渠道管理读面。
// resourceStateEnv assembles both state actions on the resolver fixture.
type resourceStateEnv struct {
	*resolverFixture
	credFixture *credentialOpsFixture
	admin       *ResourceAdmin
	scopes      *ScopeService
	channels    *notifyChannelReadPort
	service     *ResourceStateService
	// entries 是影响提示读到的注册表快照；用例直接赋值即可。
	// entries is the registry snapshot the impact hint reads.
	entries []core.AgentEntry
	logs    *strings.Builder
}

// newResourceStateEnv 建夹具；mutate 在构造服务之前改依赖（注入失败台账等）。
// newResourceStateEnv builds the fixture; mutate runs before the service is constructed.
func newResourceStateEnv(t *testing.T,
	mutate ...func(*resourceStateEnv, *ResourceStateDeps),
) *resourceStateEnv {
	t.Helper()
	base := newResolverFixture(t)
	logs := &strings.Builder{}
	env := &resourceStateEnv{resolverFixture: base, logs: logs}
	env.credFixture = &credentialOpsFixture{
		resolverFixture: base,
		credOps:         newTestCredentialOps(t, base, base.ops, logs),
		logs:            logs,
	}
	// 写面只用到 `store` / `credentials` / 按键互斥表：三者与生产同形，`LockResource`
	// 与 `CurrentETag` 走真实实现而不是替身。
	env.admin = &ResourceAdmin{
		store:         base.versions,
		credentials:   base.credentials,
		resourceLocks: &resourceLockTable{},
	}
	env.scopes = &ScopeService{
		Resources:     base.versions,
		Agents:        base.versions,
		WorkspaceRoot: base.root,
		Now:           baselineClock,
	}
	env.channels = newNotifyChannelReadPort(storage.NewNotifyChannelRepo(base.db),
		base.credentials, base.versions, notifyReadDeps{})

	deps := ResourceStateDeps{
		Resources:     base.versions,
		Credentials:   base.credentials,
		Operations:    base.ops,
		Admin:         env.admin,
		Scopes:        env.scopes,
		CredentialOps: env.credFixture.credOps,
		Channels:      env.channels,
		Agents:        func() ([]core.AgentEntry, error) { return env.entries, nil },
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           baselineClock,
	}
	for _, apply := range mutate {
		apply(env, &deps)
	}
	service, err := NewResourceStateService(deps)
	require.NoError(t, err)
	env.service = service
	return env
}

// seedAgentState 写一条 Agent 生命周期索引：属主解析只认这份身份镜像。
// seedAgentState records one lifecycle identity; owner resolution reads only this.
func (e *resourceStateEnv) seedAgentState(t *testing.T, name string,
	state core.AgentState,
) core.AgentUID {
	t.Helper()
	uid, err := core.NewAgentUID()
	require.NoError(t, err)
	require.NoError(t, e.versions.PutLifecycleIndex(context.Background(), core.LifecycleRecord{
		SchemaVersion: 2, Agent: name, AgentUID: uid, State: state,
		Generation: 1, UpdatedAt: baselineClock().UnixMilli(), OperationID: e.opID,
	}, true, ""))
	return uid
}

// seedChannel 建一条通知渠道（env 模式，地址是占位引用）。
// seedChannel creates one notification channel in env mode.
func (e *resourceStateEnv) seedChannel(t *testing.T, name string, scope core.ResourceScope,
	owner core.AgentUID,
) core.ResourceUID {
	t.Helper()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	require.NoError(t, storage.NewNotifyAdminStore(e.db).Create(context.Background(),
		core.NotifyChannel{
			ResourceUID:   uid,
			Name:          name,
			Type:          "webhook",
			URL:           "${BASALT_TEST_OPS_HOOK}",
			URLHost:       "https://hooks.example.com",
			Scope:         scope,
			OwnerAgentUID: owner,
			OperationID:   e.opID,
		}))
	return uid
}

// modelPointer 读模型连接当前的发布指针（判"零写入"与"已生效"）。
// modelPointer reads the current pointer of one model connection.
func (e *resourceStateEnv) modelPointer(t *testing.T, name string) core.ResourcePointer {
	t.Helper()
	pointer, err := e.versions.Pointer(context.Background(), core.ResourceModelConnection, name)
	require.NoError(t, err)
	return pointer
}

// staleETag 是一个形状合法但必然不匹配的条件值。
// staleETag is a well-formed ETag that never matches.
func staleETag() string {
	return `"sha256:` + strings.Repeat("0", 64) + `"`
}

// TestResourceState_CurrentETagIsReadOnly —— 条件 ETag 的对外读点与写路径同源：
// 非法类别 / 空名 / 未播种一律 false，且不写操作行。
// TestResourceState_CurrentETagIsReadOnly pins the read-only ETag read point.
func TestResourceState_CurrentETagIsReadOnly(t *testing.T) {
	ctx := context.Background()
	env := newResourceStateEnv(t)
	env.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")

	before := len(listConsoleOperations(t, env.ops))
	etag, ok := env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "alpha")
	require.True(t, ok)
	require.True(t, strings.HasPrefix(etag, `"sha256:`), "条件 ETag 必须是强 ETag 字面量")
	require.Len(t, etag, len(`"sha256:`)+64+len(`"`))

	_, ok = env.service.CurrentETag(ctx, string(core.ResourceSkill), "alpha")
	require.False(t, ok, "只接受两类带范围 / 凭据的资源")
	_, ok = env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "missing")
	require.False(t, ok)
	_, ok = env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "  ")
	require.False(t, ok)
	require.Equal(t, before, len(listConsoleOperations(t, env.ops)), "读取不得写操作行")
}

// TestResourceState_RevokeRejectsBeforePending —— 确认位、资源身份与 If-Match 三件纯
// 请求校验都排在预写之前：任一件失败都不留操作行，凭据保持 active。
// TestResourceState_RevokeRejectsBeforePending pins the pre-pending rejections.
func TestResourceState_RevokeRejectsBeforePending(t *testing.T) {
	ctx := context.Background()
	env := newResourceStateEnv(t)
	env.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	writeConnection(t, env.credFixture, "alpha", "sk-alpha-secret-1")

	uid := connectionUID(t, "alpha")
	etag, ok := env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "alpha")
	require.True(t, ok)
	before := env.modelPointer(t, "alpha")

	command := CredentialRevokeCommand{
		Kind:        string(core.ResourceModelConnection),
		Name:        "alpha",
		ResourceUID: string(uid),
		Confirm:     true,
		IfMatch:     etag,
	}
	unconfirmed := command
	unconfirmed.Confirm = false
	wrongIdentity := command
	wrongIdentity.ResourceUID = string(fixtureResourceUID(t, core.ResourceModelConnection, "other"))
	stale := command
	stale.IfMatch = staleETag()

	cases := []struct {
		name   string
		cmd    CredentialRevokeCommand
		verify func(t *testing.T, err error)
	}{
		{"未确认", unconfirmed, func(t *testing.T, err error) {
			require.True(t, core.IsInvalidInput(err))
			require.Contains(t, err.Error(), "confirm")
		}},
		{"身份不符", wrongIdentity, func(t *testing.T, err error) {
			require.ErrorIs(t, err, core.ErrResourceUIDMismatch)
			require.Equal(t, string(uid), core.AdminErrorData(err)["resource_uid"])
		}},
		{"前置条件过期", stale, func(t *testing.T, err error) {
			require.ErrorIs(t, err, core.ErrPreconditionFailed)
			require.Equal(t, etag, core.AdminErrorData(err)["current_etag"])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.service.RevokeCredential(ctx, resourceActor(), tc.cmd)
			require.Error(t, err)
			tc.verify(t, err)
		})
	}

	require.Equal(t, 0, countOperations(t, env.ops, actionModelConnectionCredentialRevoke),
		"纯请求校验失败不得留操作行")
	after := env.modelPointer(t, "alpha")
	require.Equal(t, before.Revision, after.Revision)
	identity, err := env.credentials.CredentialByOwner(ctx, core.BindingModelConnection, uid)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityActive, identity.State, "被拒的撤销不得改变凭据状态")
}

// TestResourceState_RevokeWritesOperationAndReadsBack —— 成功撤销：终态 success、
// `expected_etag` 落行、版本数回读、撤销后 ETag 变化；重放幂等且不新增操作行。
// TestResourceState_RevokeWritesOperationAndReadsBack pins the successful revoke.
func TestResourceState_RevokeWritesOperationAndReadsBack(t *testing.T) {
	ctx := context.Background()
	env := newResourceStateEnv(t)
	env.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	writeConnection(t, env.credFixture, "alpha", "sk-alpha-secret-1")

	uid := connectionUID(t, "alpha")
	etag, ok := env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "alpha")
	require.True(t, ok)

	out, err := env.service.RevokeCredential(ctx, resourceActor(), CredentialRevokeCommand{
		Kind:        string(core.ResourceModelConnection),
		Name:        "alpha",
		ResourceUID: string(uid),
		Confirm:     true,
		IfMatch:     etag,
	})
	require.NoError(t, err)
	require.Equal(t, uid, out.ResourceUID)
	require.NotEmpty(t, out.CredentialID)
	require.Equal(t, 1, out.RevocationEpoch)
	require.Equal(t, 1, out.RevokedVersions)
	require.NotEmpty(t, out.OperationID)
	require.NotEmpty(t, out.ETag)
	require.NotEqual(t, etag, out.ETag, "撤销推进 revision 并改变凭据段，ETag 必须变化")

	row := findConsoleOperation(t, env.ops, actionModelConnectionCredentialRevoke)
	require.Equal(t, out.OperationID, row.OperationID)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, lifecycleResultOK, row.ResultCode)
	require.Equal(t, string(core.ResourceModelConnection), row.TargetKind)
	require.Equal(t, string(uid), row.TargetID)
	require.Equal(t, etag, row.ExpectedETag, "条件写期望值必须落进操作行")
	require.Equal(t, resourceActor().Subject, row.ActorSubject)
	require.NotEmpty(t, row.RequestHash)

	identity, err := env.credentials.CredentialByOwner(ctx, core.BindingModelConnection, uid)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRevoked, identity.State)

	// 重放同一请求：幂等结论（Changed=false 时调用方不带 operation_id），不新开行。
	replay, err := env.service.RevokeCredential(ctx, resourceActor(), CredentialRevokeCommand{
		Kind:        string(core.ResourceModelConnection),
		Name:        "alpha",
		ResourceUID: string(uid),
		Confirm:     true,
		IfMatch:     out.ETag,
	})
	require.NoError(t, err)
	require.Empty(t, replay.OperationID, "已撤销的凭据重放不新开操作行")
	require.Equal(t, out.ETag, replay.ETag, "重放不改变条件 ETag")
	require.Equal(t, 1, countOperations(t, env.ops, actionModelConnectionCredentialRevoke))
}

// TestResourceState_ScopeWritesFourEvidences —— 共享转专属：四证 pending → success，
// revision 只推进一次，写后回读的 ETag 与条件写读点一致；同目标重放不新开行。
// TestResourceState_ScopeWritesFourEvidences pins the four-evidence conversion.
func TestResourceState_ScopeWritesFourEvidences(t *testing.T) {
	ctx := context.Background()
	env := newResourceStateEnv(t)
	env.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	ownerUID := env.seedAgentState(t, "ops-bot", core.AgentDisabled)

	uid := connectionUID(t, "alpha")
	before := env.modelPointer(t, "alpha")
	etag, ok := env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "alpha")
	require.True(t, ok)

	out, err := env.service.ConvertScope(ctx, resourceActor(), ResourceScopeCommand{
		Kind:        string(core.ResourceModelConnection),
		Name:        "alpha",
		ResourceUID: string(uid),
		Scope:       string(core.ScopeExclusive),
		OwnerAgent:  "ops-bot",
		IfMatch:     etag,
	})
	require.NoError(t, err)
	require.Equal(t, core.ScopeExclusive, out.Scope)
	require.Equal(t, "ops-bot", out.OwnerAgent)
	require.Equal(t, before.Revision+1, out.Revision)
	require.NotEmpty(t, out.ETag)
	require.Empty(t, out.AffectedEligibleAgents, "模型连接没有逐 Agent 通知模式，影响提示为空")

	after := env.modelPointer(t, "alpha")
	require.Equal(t, core.ScopeExclusive, after.Scope)
	require.Equal(t, ownerUID, after.OwnerAgentUID)
	require.Equal(t, before.Revision+1, after.Revision)

	current, ok := env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "alpha")
	require.True(t, ok)
	require.Equal(t, current, out.ETag, "写后回读的 ETag 必须等于条件写读点")

	row := findConsoleOperation(t, env.ops, actionModelConnectionScope)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, lifecycleResultOK, row.ResultCode)
	require.Equal(t, etag, row.ExpectedETag)
	require.Equal(t, string(core.ResourceModelConnection), row.TargetKind)
	require.Equal(t, string(uid), row.TargetID)
	require.NotEmpty(t, row.RequestHash)

	var detail credentialActionDetail
	require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
	require.Equal(t, string(core.ResourceModelConnection), detail.Kind)
	require.Equal(t, "alpha", detail.Name)
	require.Equal(t, string(uid), detail.ResourceUID)
	require.Equal(t, before.Revision+1, detail.Revision, "预写详情的 revision 是写后值")
	require.Equal(t, before.Revision, detail.PreviousRevision)
	require.Equal(t, string(core.ScopeExclusive), detail.Scope)
	require.Equal(t, string(core.ScopeShared), detail.PreviousScope)
	require.Equal(t, string(ownerUID), detail.OwnerAgentUID)
	require.Empty(t, detail.PreviousOwnerAgentUID)

	// 同目标重放：不新开操作行、不推进 revision、ETag 不变。
	replay, err := env.service.ConvertScope(ctx, resourceActor(), ResourceScopeCommand{
		Kind:        string(core.ResourceModelConnection),
		Name:        "alpha",
		ResourceUID: string(uid),
		Scope:       string(core.ScopeExclusive),
		OwnerAgent:  "ops-bot",
		IfMatch:     current,
	})
	require.NoError(t, err)
	require.Equal(t, out.Revision, replay.Revision)
	require.Equal(t, out.ETag, replay.ETag)
	require.Equal(t, 1, countOperations(t, env.ops, actionModelConnectionScope))
	require.Equal(t, after.Revision, env.modelPointer(t, "alpha").Revision)
}

// TestResourceState_ScopeBlockedByBindingWritesFailedRow —— 别的 Agent 还绑着这条资源时，
// 共享转专属必须 409 点名，操作行终态 failed 并带操作标识，指针零变化。
// TestResourceState_ScopeBlockedByBindingWritesFailedRow pins the blocked conversion.
func TestResourceState_ScopeBlockedByBindingWritesFailedRow(t *testing.T) {
	ctx := context.Background()
	env := newResourceStateEnv(t)
	env.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	env.seedAgentState(t, "ops-bot", core.AgentDisabled)

	uid := connectionUID(t, "alpha")
	otherUID, err := core.NewAgentUID()
	require.NoError(t, err)
	require.NoError(t, storage.NewBindingStore(env.db).Rebuild(ctx, otherUID, "writer",
		"sha256:"+strings.Repeat("0", 64),
		core.ResourceBindings{
			ModelConnections: []core.ResourceBinding{{Name: "alpha", UID: uid}},
		}, baselineClock()))

	before := env.modelPointer(t, "alpha")
	etag, ok := env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "alpha")
	require.True(t, ok)

	_, err = env.service.ConvertScope(ctx, resourceActor(), ResourceScopeCommand{
		Kind:        string(core.ResourceModelConnection),
		Name:        "alpha",
		ResourceUID: string(uid),
		Scope:       string(core.ScopeExclusive),
		OwnerAgent:  "ops-bot",
		IfMatch:     etag,
	})
	require.ErrorIs(t, err, core.ErrResourceScopeInUse)
	require.Contains(t, err.Error(), string(otherUID), "409 必须点名绑定者")

	row := findConsoleOperation(t, env.ops, actionModelConnectionScope)
	require.Equal(t, core.OperationFailed, row.Status)
	require.Equal(t, lifecycleResultInternal, row.ResultCode)
	require.Equal(t, row.OperationID, core.AdminErrorData(err)["operation_id"],
		"写后失败必须带上操作标识")

	after := env.modelPointer(t, "alpha")
	require.Equal(t, before.Scope, after.Scope, "被拒绝的转换必须零写入")
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, before.OwnerAgentUID, after.OwnerAgentUID)
}

// TestResourceState_ScopeOwnerResolutionPrecedesPending —— 属主不存在（404）与已归档
// （409）都是纯目标检查：排在预写之前，零操作行。
// TestResourceState_ScopeOwnerResolutionPrecedesPending pins owner checks before pending.
func TestResourceState_ScopeOwnerResolutionPrecedesPending(t *testing.T) {
	ctx := context.Background()
	env := newResourceStateEnv(t)
	env.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	env.seedAgentState(t, "retired", core.AgentArchived)

	uid := connectionUID(t, "alpha")
	etag, ok := env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "alpha")
	require.True(t, ok)
	before := env.modelPointer(t, "alpha")

	cases := []struct {
		name  string
		owner string
		is    error
	}{
		{"属主不存在", "ghost", core.ErrResourceNotFound},
		{"属主已归档", "retired", core.ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.service.ConvertScope(ctx, resourceActor(), ResourceScopeCommand{
				Kind:        string(core.ResourceModelConnection),
				Name:        "alpha",
				ResourceUID: string(uid),
				Scope:       string(core.ScopeExclusive),
				OwnerAgent:  tc.owner,
				IfMatch:     etag,
			})
			require.ErrorIs(t, err, tc.is)
		})
	}

	require.Equal(t, 0, countOperations(t, env.ops, actionModelConnectionScope),
		"属主解析失败不得留操作行")
	after := env.modelPointer(t, "alpha")
	require.Equal(t, before.Scope, after.Scope)
	require.Equal(t, before.Revision, after.Revision)
}

// TestResourceState_ScopePreWriteFailureWritesNothing —— 预写失败零写入（FR-001）：
// 台账写不进去就不动目标。
// TestResourceState_ScopePreWriteFailureWritesNothing pins the pre-write failure rule.
func TestResourceState_ScopePreWriteFailureWritesNothing(t *testing.T) {
	ctx := context.Background()
	env := newResourceStateEnv(t, func(_ *resourceStateEnv, deps *ResourceStateDeps) {
		deps.Operations = &failingOperationStore{}
	})
	env.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	env.seedAgentState(t, "ops-bot", core.AgentDisabled)

	uid := connectionUID(t, "alpha")
	before := env.modelPointer(t, "alpha")
	etag, ok := env.service.CurrentETag(ctx, string(core.ResourceModelConnection), "alpha")
	require.True(t, ok)

	_, err := env.service.ConvertScope(ctx, resourceActor(), ResourceScopeCommand{
		Kind:        string(core.ResourceModelConnection),
		Name:        "alpha",
		ResourceUID: string(uid),
		Scope:       string(core.ScopeExclusive),
		OwnerAgent:  "ops-bot",
		IfMatch:     etag,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "预写操作记录失败")

	require.Equal(t, 0, countOperations(t, env.ops, actionModelConnectionScope))
	after := env.modelPointer(t, "alpha")
	require.Equal(t, before.Scope, after.Scope)
	require.Equal(t, before.Revision, after.Revision)
}

// TestResourceState_NotifyChannelUsesChannelETagAndImpactHint —— 通知渠道走渠道 ETag
// 配方（与列表 / 条件写同一份）；共享转专属给出 `all_eligible` 的影响提示；没有托管
// 凭据的渠道撤销按入参错误拒绝且不写操作行。
// TestResourceState_NotifyChannelUsesChannelETagAndImpactHint pins the channel branch.
func TestResourceState_NotifyChannelUsesChannelETagAndImpactHint(t *testing.T) {
	ctx := context.Background()
	env := newResourceStateEnv(t)
	ownerUID := env.seedAgentState(t, "ops-bot", core.AgentDisabled)
	uid := env.seedChannel(t, "ops-hook", core.ScopeShared, "")

	etag, ok := env.service.CurrentETag(ctx, string(core.ResourceNotifyChannel), "ops-hook")
	require.True(t, ok)

	// 影响提示：共享转专属后，`all_eligible` 的已注册 Agent 失去资格；`allowlist`、
	// 未注册与新属主都不算受影响。
	env.entries = []core.AgentEntry{
		{Name: "reporter", Registered: true, Profile: &core.Profile{}},
		{Name: "digest", Registered: true, Profile: &core.Profile{NotifyChannels: []string{"ops-hook"}}},
		{Name: "ops-bot", Registered: true, Profile: &core.Profile{}},
		{Name: "pending", Registered: false, Profile: &core.Profile{}},
	}

	out, err := env.service.ConvertScope(ctx, resourceActor(), ResourceScopeCommand{
		Kind:        string(core.ResourceNotifyChannel),
		Name:        "ops-hook",
		ResourceUID: string(uid),
		Scope:       string(core.ScopeExclusive),
		OwnerAgent:  "ops-bot",
		IfMatch:     etag,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"reporter"}, out.AffectedEligibleAgents)
	require.NotEmpty(t, out.ETag)

	channel, err := env.channels.Channel(ctx, "ops-hook")
	require.NoError(t, err)
	require.Equal(t, core.ScopeExclusive, channel.Scope)
	require.Equal(t, ownerUID, channel.OwnerAgentUID)
	require.Equal(t, "ops-bot", channel.OwnerAgentName)
	require.Equal(t, out.ETag, channel.ETag, "写后回读与管理读面必须是同一份渠道 ETag")

	row := findConsoleOperation(t, env.ops, actionNotifyChannelScope)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, string(core.ResourceNotifyChannel), row.TargetKind)
	require.Equal(t, string(uid), row.TargetID)

	// M-3：影响名单必须落进操作记录，且与响应同源——审计事后能回答"当时告诉过谁"。
	var detail credentialActionDetail
	require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
	require.Equal(t, []string{"reporter"}, detail.AffectedEligibleAgents)
	require.Equal(t, channel.Revision, detail.Revision)

	// env 模式的渠道没有托管凭据：撤销按入参错误拒绝，不写操作行。
	_, err = env.service.RevokeCredential(ctx, resourceActor(), CredentialRevokeCommand{
		Kind:        string(core.ResourceNotifyChannel),
		Name:        "ops-hook",
		ResourceUID: string(uid),
		Confirm:     true,
		IfMatch:     channel.ETag,
	})
	require.True(t, core.IsInvalidInput(err))
	require.Equal(t, 0, countOperations(t, env.ops, actionNotifyChannelCredentialRevoke))
}
