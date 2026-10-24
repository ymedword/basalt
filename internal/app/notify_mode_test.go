package app

// 本文件覆盖通知意图三态（颗粒度文档 R4.7 / T122）：显式声明逐字校验、未声明按兼容
// 规则读取、空数组等于 all_eligible 而不是禁用、准入冻结具体 UID（无 `*` 通配）、
// 在途 Run 的冻结集合不受运行中途新增渠道影响。
// These tests cover the three-state notification intent and its admission freeze.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
)

const (
	// notifyOtherAgentUID 是另一个 Agent 的身份（专属渠道的属主）。
	notifyOtherAgentUID = core.AgentUID("ag_00000000000000000000000007")
	// notifyTargetBinding 是 env 模式下目标地址哈希的合法形状（64 位小写十六进制）。
	notifyTargetBinding = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// seedChannel 建一条通知渠道；owner 仅对 exclusive 有意义。
// seedChannel creates one notification channel.
func (e *bindingsEnv) seedChannel(t *testing.T, name string, scope core.ResourceScope,
	owner core.AgentUID,
) core.ResourceUID {
	t.Helper()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	require.NoError(t, e.notify.Create(context.Background(), core.NotifyChannel{
		ResourceUID:   uid,
		Name:          name,
		Type:          "webhook",
		URL:           "https://example.com/hook",
		URLHost:       "example.com",
		Scope:         scope,
		OwnerAgentUID: owner,
		OperationID:   e.opID,
	}))
	return uid
}

// notifyDeps 返回冻结用的只读依赖（注册表 + 绑定投影）。
// notifyDeps wires the catalog and the binding projection.
func (e *bindingsEnv) notifyDeps() NotifyGrantDeps {
	return NotifyGrantDeps{
		Channels: storage.NewNotifyChannelRepo(e.db),
		Bindings: e.bindings,
	}
}

// TestNotifyMode_ParsesThreeStates —— 三个字面量逐字通过，其它取值（含拼写变体）
// 一律拒绝，不做"看起来像哪个"的猜测。
func TestNotifyMode_ParsesThreeStates(t *testing.T) {
	cases := []struct {
		name   string
		mode   core.NotifyMode
		chans  []string
		want   core.NotifyMode
		legacy bool
	}{
		{"none", core.NotifyModeNone, nil, core.NotifyModeNone, false},
		{"all_eligible", core.NotifyModeAllEligible, nil, core.NotifyModeAllEligible, false},
		{"allowlist", core.NotifyModeAllowlist, []string{"ops"}, core.NotifyModeAllowlist, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveNotifyMode(&core.Profile{NotifyMode: tc.mode, NotifyChannels: tc.chans})
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Mode)
			require.Equal(t, tc.legacy, got.Legacy)
		})
	}

	for _, raw := range []string{"all-eligible", "ALL_ELIGIBLE", "everyone", "shared"} {
		_, err := ResolveNotifyMode(&core.Profile{NotifyMode: core.NotifyMode(raw)})
		require.Error(t, err, "非字面量 %q 必须被拒绝", raw)
	}

	_, err := ResolveNotifyMode(nil)
	require.Error(t, err, "没有定义文件就没有通知模式")
}

// TestNotifyMode_EmptyArrayIsAllEligible —— 未声明时按兼容规则读取：空数组 / 缺省
// = all_eligible，非空 = allowlist；空数组**不得**被读成"禁用"。
func TestNotifyMode_EmptyArrayIsAllEligible(t *testing.T) {
	got, err := ResolveNotifyMode(&core.Profile{NotifyChannels: nil})
	require.NoError(t, err)
	require.Equal(t, core.NotifyModeAllEligible, got.Mode)
	require.True(t, got.Legacy, "未声明要标记为兼容读取，页面据此显示「兼容模式」")

	got, err = ResolveNotifyMode(&core.Profile{NotifyChannels: []string{}})
	require.NoError(t, err)
	require.Equal(t, core.NotifyModeAllEligible, got.Mode)
	require.True(t, got.Legacy)
	require.NotEqual(t, core.NotifyModeNone, got.Mode, "空数组不等于禁用")

	got, err = ResolveNotifyMode(&core.Profile{NotifyChannels: []string{"ops"}})
	require.NoError(t, err)
	require.Equal(t, core.NotifyModeAllowlist, got.Mode)
	require.True(t, got.Legacy)

	// 显式三态与渠道列表矛盾 → 拒绝（加载期只是 WARN，准入不得替用户猜）。
	for _, tc := range []struct {
		name string
		mode core.NotifyMode
		ch   []string
	}{
		{"none 配渠道", core.NotifyModeNone, []string{"ops"}},
		{"all_eligible 配渠道", core.NotifyModeAllEligible, []string{"ops"}},
		{"allowlist 空渠道", core.NotifyModeAllowlist, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveNotifyMode(&core.Profile{NotifyMode: tc.mode, NotifyChannels: tc.ch})
			require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
		})
	}
}

// TestNotifyMode_NoneDisablesNotify —— `none` 冻结空集合且不读注册表（"没开通知"与
// "查不到渠道"是两件事），工具面因此固定报 `notify_disabled`。
func TestNotifyMode_NoneDisablesNotify(t *testing.T) {
	env := newBindingsEnv(t)
	ctx := context.Background()
	env.seedChannel(t, "ops", core.ScopeShared, "")

	// 刻意不给依赖：none 不该碰注册表或绑定投影。
	manifest, err := FreezeNotifyGrant(ctx, NotifyGrantDeps{}, bindingAgentUID,
		&core.Profile{NotifyMode: core.NotifyModeNone})
	require.NoError(t, err)
	require.Equal(t, core.NotifyModeNone, manifest.Mode)
	require.Empty(t, manifest.Channels, "none 冻结零渠道")
	require.NotNil(t, manifest.Channels, "空集合与零值要能区分")

	uids, err := FrozenChannelUIDs(manifest)
	require.NoError(t, err)
	require.Empty(t, uids)
}

// TestNotifyMode_FreezesConcreteUIDs —— 冻结的是具体 UID 而不是 `*`：`all_eligible`
// 只收有资格渠道（共享 ∪ 本 Agent 专属），`allowlist` 只收声明且绑定一致的渠道。
func TestNotifyMode_FreezesConcreteUIDs(t *testing.T) {
	env := newBindingsEnv(t)
	ctx := context.Background()

	sharedA := env.seedChannel(t, "ops", core.ScopeShared, "")
	sharedB := env.seedChannel(t, "audit", core.ScopeShared, "")
	foreign := env.seedChannel(t, "secret", core.ScopeExclusive, notifyOtherAgentUID)
	mine := env.seedChannel(t, "mine", core.ScopeExclusive, bindingAgentUID)

	t.Run("all_eligible 枚举全部有资格渠道", func(t *testing.T) {
		manifest, err := FreezeNotifyGrant(ctx, env.notifyDeps(), bindingAgentUID,
			&core.Profile{NotifyMode: core.NotifyModeAllEligible})
		require.NoError(t, err)
		require.Equal(t, core.NotifyModeAllEligible, manifest.Mode)

		uids, err := FrozenChannelUIDs(manifest)
		require.NoError(t, err, "集合里出现非法 UID（如 `*`）必须报错")
		require.ElementsMatch(t, []core.ResourceUID{sharedA, sharedB, mine}, uids)
		require.NotContains(t, uids, foreign, "他人的专属渠道不进集合")
		for _, uid := range uids {
			require.NotEqual(t, "*", string(uid))
		}
	})

	t.Run("allowlist 只冻结声明且绑定一致的渠道", func(t *testing.T) {
		require.NoError(t, env.bindings.Rebuild(ctx, bindingAgentUID, "reporter", "hash",
			core.ResourceBindings{NotifyChannels: []core.ResourceBinding{
				{Name: "ops", UID: sharedA},
				{Name: "audit", UID: sharedB},
			}}, baselineClock()))

		manifest, err := FreezeNotifyGrant(ctx, env.notifyDeps(), bindingAgentUID,
			&core.Profile{
				NotifyMode:     core.NotifyModeAllowlist,
				NotifyChannels: []string{"audit", "ops"},
			})
		require.NoError(t, err)
		uids, err := FrozenChannelUIDs(manifest)
		require.NoError(t, err)
		require.ElementsMatch(t, []core.ResourceUID{sharedA, sharedB}, uids)
		require.NotContains(t, uids, mine, "未声明的渠道不进集合")
	})

	t.Run("allowlist 声明未绑定", func(t *testing.T) {
		_, err := FreezeNotifyGrant(ctx, env.notifyDeps(), bindingAgentUID,
			&core.Profile{
				NotifyMode:     core.NotifyModeAllowlist,
				NotifyChannels: []string{"mine"},
			})
		require.ErrorIs(t, err, core.ErrAgentResourceBindingMissing)
	})

	t.Run("allowlist 绑定与当前渠道不一致", func(t *testing.T) {
		require.NoError(t, env.bindings.Rebuild(ctx, bindingAgentUID, "reporter", "hash",
			core.ResourceBindings{NotifyChannels: []core.ResourceBinding{
				{Name: "ops", UID: sharedA},
			}}, baselineClock()))
		// 同名重建：新的 UID 占住 ops，旧绑定不再指向当前实例。
		require.NoError(t, env.notify.Delete(ctx, "ops", env.opID))
		env.seedChannel(t, "ops", core.ScopeShared, "")

		_, err := FreezeNotifyGrant(ctx, env.notifyDeps(), bindingAgentUID,
			&core.Profile{
				NotifyMode:     core.NotifyModeAllowlist,
				NotifyChannels: []string{"ops"},
			})
		require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
	})

	t.Run("allowlist 声明他人专属", func(t *testing.T) {
		require.NoError(t, env.bindings.Rebuild(ctx, bindingAgentUID, "reporter", "hash",
			core.ResourceBindings{NotifyChannels: []core.ResourceBinding{
				{Name: "secret", UID: foreign},
			}}, baselineClock()))
		_, err := FreezeNotifyGrant(ctx, env.notifyDeps(), bindingAgentUID,
			&core.Profile{
				NotifyMode:     core.NotifyModeAllowlist,
				NotifyChannels: []string{"secret"},
			})
		require.ErrorIs(t, err, core.ErrResourceNotEligible)
	})
}

// TestNotifyMode_MidRunChangeKeepsFrozen —— 冻结发生在准入：运行中途新增渠道只影响
// 之后的 Run；在途 Run 的 `run_notify_refs` 不随文件 / 注册表变化。
func TestNotifyMode_MidRunChangeKeepsFrozen(t *testing.T) {
	env := newBindingsEnv(t)
	ctx := context.Background()
	version := env.publishModelVersion(t, "alpha")

	first := env.seedChannel(t, "ops", core.ScopeShared, "")
	grant, err := FreezeNotifyGrant(ctx, env.notifyDeps(), bindingAgentUID,
		&core.Profile{NotifyMode: core.NotifyModeAllEligible})
	require.NoError(t, err)
	require.Len(t, grant.Channels, 1)

	manifest, err := core.BuildRunResourceManifest(bindingAgentUID, core.AgentActive,
		core.ResolvedResources{
			Provider: core.ResourceVersionRef{
				Name: "alpha", ResourceUID: version.ResourceUID, VersionID: version.ID,
				VersionNo: version.VersionNo, ContentHash: version.ContentHash,
			},
			ProviderModel: "alpha-model",
			ProviderCredential: core.CredentialPin{
				Mode:          core.CredentialModeEnv,
				TargetBinding: notifyTargetBinding,
			},
			Notify: grant,
		})
	require.NoError(t, err)

	runID := "run-notify-freeze"
	runs := storage.NewRunStore(env.db)
	require.NoError(t, runs.StartRun(ctx, core.RunRecord{
		RunID: runID, Agent: "reporter", Channel: "web", UserID: "u-1",
		StartedAt: baselineClock(),
	}, manifest))

	deliveries := storage.NewNotifyDeliveryStore(env.db)
	frozen, err := deliveries.Refs(ctx, runID)
	require.NoError(t, err)
	require.Len(t, frozen, 1)
	require.Equal(t, first, frozen[0].ChannelUID)

	// 运行中途新增渠道：重新冻结会多一项，但在途 Run 的账不变。
	second := env.seedChannel(t, "night", core.ScopeShared, "")
	later, err := FreezeNotifyGrant(ctx, env.notifyDeps(), bindingAgentUID,
		&core.Profile{NotifyMode: core.NotifyModeAllEligible})
	require.NoError(t, err)
	require.Len(t, later.Channels, 2)

	frozen, err = deliveries.Refs(ctx, runID)
	require.NoError(t, err)
	require.Len(t, frozen, 1, "在途 Run 的冻结集合不得被后续变更改写")
	require.Equal(t, first, frozen[0].ChannelUID)

	// 删除闸门读取：未释放的 Run 握着的渠道能被反查到。
	blocked, err := deliveries.UnreleasedRunsByChannel(ctx, first)
	require.NoError(t, err)
	require.Equal(t, []string{runID}, blocked)
	unblocked, err := deliveries.UnreleasedRunsByChannel(ctx, second)
	require.NoError(t, err)
	require.Empty(t, unblocked, "准入之后才出现的渠道没有被任何在途 Run 冻结")

	require.NoError(t, runs.FinishRun(ctx, runID, core.StatusCompleted, "", 1, 0, baselineClock()))
	blocked, err = deliveries.UnreleasedRunsByChannel(ctx, first)
	require.NoError(t, err)
	require.Empty(t, blocked, "Run 终态释放冻结")

	require.True(t, strings.HasPrefix(string(first), "rs_"), "渠道身份带资源前缀")
}

// allowAllNotifyPolicy 只服务于本文件的本地投递用例：生产白名单拒绝 IP 字面量与非常规
// 端口（httptest 两者都占），判定面本身由 tool 包用例覆盖，这里要验的是执行器的调用 id
// 与投递记录的接线。
// allowAllNotifyPolicy only serves the local-delivery test; the policy surface itself is
// covered in the tool package.
type allowAllNotifyPolicy struct{}

// Check 恒放行。
// Check always allows.
func (allowAllNotifyPolicy) Check(context.Context, core.Action) error { return nil }

// TestNotifyDelivery_ExecutorWritesDerivedRow —— R4.8 / B-1：一次真实的 Run 内发送
// （执行器预写审计 → 工具解析 → 投递 → 写 `notify_deliveries`）落下一行派生记录，
// 行 id 就是本次审计行；env 模式不带凭据身份与版本，target_host 只记 host。
// An executed send inside a real run writes the one-per-invocation delivery row through
// the real store; the row carries the audit id from the executor and no credential.
func TestNotifyDelivery_ExecutorWritesDerivedRow(t *testing.T) {
	env := newBindingsEnv(t)
	ctx := context.Background()
	version := env.publishModelVersion(t, "alpha")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	t.Setenv("BASALT_APP_NOTIFY_HOOK", server.URL+"/hook")

	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	require.NoError(t, env.notify.Create(ctx, core.NotifyChannel{
		ResourceUID: uid, Name: "ops", Type: "webhook", URL: "${BASALT_APP_NOTIFY_HOOK}",
		URLHost: "127.0.0.1", Scope: core.ScopeShared, OperationID: env.opID,
	}))

	grant, err := FreezeNotifyGrant(ctx, env.notifyDeps(), bindingAgentUID,
		&core.Profile{NotifyMode: core.NotifyModeAllEligible})
	require.NoError(t, err)
	require.Len(t, grant.Channels, 1)

	manifest, err := core.BuildRunResourceManifest(bindingAgentUID, core.AgentActive,
		core.ResolvedResources{
			Provider: core.ResourceVersionRef{
				Name: "alpha", ResourceUID: version.ResourceUID, VersionID: version.ID,
				VersionNo: version.VersionNo, ContentHash: version.ContentHash,
			},
			ProviderModel: "alpha-model",
			ProviderCredential: core.CredentialPin{
				Mode:          core.CredentialModeEnv,
				TargetBinding: notifyTargetBinding,
			},
			Notify: grant,
		})
	require.NoError(t, err)
	runID := "webhook-1700000000100-delivery"
	require.NoError(t, storage.NewRunStore(env.db).StartRun(ctx, core.RunRecord{
		RunID: runID, Agent: "reporter", Channel: "web", UserID: "u-1",
		StartedAt: baselineClock(),
	}, manifest))

	// 装配与生产一致：真实渠道仓储 + 真实投递记录器 + 真实执行器。
	deliveryStore := storage.NewNotifyDeliveryStore(env.db)
	notifyTool := newNotifyTool(storage.NewNotifyChannelRepo(env.db), allowAllNotifyPolicy{},
		nil, notifyDeliveryRecorder{store: deliveryStore})
	logger := slog.New(slog.DiscardHandler)
	registry := tool.NewRegistry(logger)
	require.NoError(t, registry.Register(notifyTool))
	exec := core.NewToolExecutor(registry, storage.NewToolInvocationRepo(env.db), logger)

	res := exec.Execute(ctx, core.ToolEnv{
		RunID:          runID,
		AgentUID:       bindingAgentUID,
		AgentName:      "reporter",
		RunBound:       true,
		NotifyMode:     core.NotifyModeAllEligible,
		NotifyChannels: []string{string(uid)},
		Timeout:        5 * time.Second,
	}, []string{"notify"},
		core.ToolCall{Name: "notify", Arguments: `{"channel":"ops","content":"日报"}`})
	require.Equal(t, "已推送到通知渠道：ops", res.Content)

	rows, err := deliveryStore.Deliveries(ctx, runID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "一次工具调用一条投递记录")
	require.Positive(t, rows[0].ToolInvocationID, "记录主键是执行器预写的审计行 id")
	require.Equal(t, bindingAgentUID, rows[0].AgentUID)
	require.Equal(t, uid, rows[0].ChannelUID)
	require.Equal(t, "ops", rows[0].ChannelName)
	require.Empty(t, rows[0].CredentialID, "env 模式没有凭据身份")
	require.Zero(t, rows[0].CredentialVersion)
	require.Equal(t, "127.0.0.1", rows[0].TargetHost, "只记 host，不记路径与端口之外的地址")
	require.NotContains(t, rows[0].TargetHost, "/hook")
}
