package app

// 本文件覆盖托管凭据的按需加载与启动依赖（模块卡 R4.8 / T130）：
//   - 四个按需场景：全 env + 托管渠道启动不读密钥、空实例可首次建托管连接、缺密钥时
//     只有托管操作失败、必需凭据不可解密时拒绝启动并点名资源；
//   - 启动依赖范围：只有「当前发布 + 被活动区已注册 Agent 绑定」的托管模型连接参与
//     启动校验，未绑定 / 仅归档引用 / 托管渠道 / env 连接都不读主密钥；
//   - 准入只读检查：撤销 → 固定错误，目标地址漂移 → 不可用，env 模式恒通过。
// These tests cover on-demand master-key loading, the minimal startup dependency scope
// and the read-only admission check.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// resolverFixture 在 runResourcesEnv 之上补主密钥与解析器。
// resolverFixture adds the master key and the resolver to the resource fixture.
type resolverFixture struct {
	*runResourcesEnv
	key      MasterKey
	keyPath  string
	resolver *credentialResolver
}

// newResolverFixture 建夹具：主密钥写在**工作区外**的独立临时目录，解析器指向它。
// newResolverFixture stores the master key outside the workspace and points the resolver
// at it.
func newResolverFixture(t *testing.T) *resolverFixture {
	t.Helper()
	env := newRunResourcesEnv(t)
	key := testMasterKey()
	t.Cleanup(func() { key.Zero() })
	keyPath := writeMasterKeyFile(t, t.TempDir(), key.KeyID, key.Key)
	f := &resolverFixture{runResourcesEnv: env, key: key, keyPath: keyPath}
	f.resolver = newTestResolver(t, env, keyPath)
	return f
}

// newTestResolver 按生产依赖形状构造解析器，并保留具体类型以调用启动校验 / 准入检查。
// newTestResolver keeps the concrete type so tests can call VerifyStartup/CheckBinding.
func newTestResolver(t *testing.T, env *runResourcesEnv, masterKeyPath string) *credentialResolver {
	t.Helper()
	port, err := NewCredentialResolver(CredentialResolverDeps{
		Credentials:   env.credentials,
		Resources:     env.versions,
		Notify:        env.admission.NotifyAdmin,
		Bindings:      storage.NewBindingStore(env.db),
		MasterKeyFile: masterKeyPath,
		WorkspaceRoot: env.root,
	})
	require.NoError(t, err)
	impl, ok := port.(*credentialResolver)
	require.True(t, ok, "解析器端口必须由 credentialResolver 实现")
	return impl
}

// useKeyPath 只换解析器指向的路径（模拟"密钥缺失 / 指错"的部署状态）；
// `f.keyPath` 始终保留真实密钥位置，测试可随时切回。
// useKeyPath swaps only the resolver's path; the fixture keeps the real key location.
func (f *resolverFixture) useKeyPath(t *testing.T, path string) {
	t.Helper()
	f.resolver = newTestResolver(t, f.runResourcesEnv, path)
}

// managedProviderDefinition 写一份 v3 托管模式的模型连接定义：没有 `api_key`，
// 凭据引用在库里（`credential_mode: managed`）。
// managedProviderDefinition renders a managed-mode connection without any api_key.
func managedProviderDefinition(name, baseURL, model string) string {
	return "name: " + name + "\n" +
		"base_url: " + baseURL + "\n" +
		"credential_mode: managed\n" +
		"default_model: " + model + "\n"
}

// seedManagedModelConnection 播种并发布一条托管模型连接，返回发布版本。
// seedManagedModelConnection publishes one managed-mode model connection.
func (f *resolverFixture) seedManagedModelConnection(t *testing.T, name, baseURL string) core.ResourceVersion {
	t.Helper()
	version := f.seedVersion(t, core.ResourceModelConnection, name,
		managedProviderDefinition(name, baseURL, "alpha-model"), "", "")
	f.publish(t, core.ResourceModelConnection, name, version.ID, true)
	return version
}

// last4Of 按 schema 口径取尾号：原值不足 8 位时留空（短值等于泄露）。
// last4Of keeps the last four characters only for values of eight or more.
func last4Of(value string) string {
	if len(value) < 8 {
		return ""
	}
	return value[len(value)-4:]
}

// seedSealedCredential 用夹具主密钥真正加密一份材料，返回凭据 ID。
// seedSealedCredential seals real ciphertext under the fixture master key.
func (f *resolverFixture) seedSealedCredential(t *testing.T, kind core.ResourceKind,
	name, target, plaintext string,
) string {
	t.Helper()
	binding, ok := credentialBindingKind(kind)
	require.True(t, ok)
	uid := fixtureResourceUID(t, kind, name)
	credID := fixtureCredentialID(t, string(kind)+"/"+name)
	aad := CredentialAAD{
		OwnerKind:     binding,
		OwnerUID:      uid,
		CredentialID:  credID,
		VersionNo:     1,
		TargetBinding: target,
	}
	nonce, ciphertext, err := SealCredential(f.key, aad, []byte(plaintext))
	require.NoError(t, err)
	require.NoError(t, f.credentials.Create(context.Background(), core.CredentialRecord{
		ID:          credID,
		OwnerKind:   binding,
		OwnerUID:    uid,
		OwnerName:   name,
		KeyID:       f.key.KeyID,
		OperationID: f.opID,
		CreatedAt:   baselineClock().UnixMilli(),
	}, core.CredentialVersionRecord{
		KeyID:         f.key.KeyID,
		TargetBinding: target,
		Last4:         last4Of(plaintext),
		Nonce:         nonce,
		Ciphertext:    ciphertext,
		OperationID:   f.opID,
	}))
	return credID
}

// seedManagedChannel 建一条托管通知渠道：行里只有 host 与凭据引用，完整地址在密文里。
// seedManagedChannel creates a managed channel whose full address lives in ciphertext.
func (f *resolverFixture) seedManagedChannel(t *testing.T, name, fullURL string) core.NotifyChannel {
	t.Helper()
	uid := fixtureResourceUID(t, core.ResourceNotifyChannel, name)
	target, err := CredentialTargetBinding(core.BindingNotifyChannel, "", fullURL)
	require.NoError(t, err)
	credID := f.seedSealedCredential(t, core.ResourceNotifyChannel, name, target, fullURL)
	channel := core.NotifyChannel{
		ResourceUID:    uid,
		Name:           name,
		Type:           "webhook",
		URLHost:        "https://hooks.example.com",
		CredentialMode: core.CredentialModeManaged,
		CredentialID:   credID,
		OperationID:    f.opID,
	}
	require.NoError(t, f.admission.NotifyAdmin.Create(context.Background(), channel))
	return channel
}

// project 写入某 Agent 的绑定投影行（同名 Agent 只建一次身份，重复调用整份替换）。
// project replaces the binding projection of one agent.
func (f *resolverFixture) project(t *testing.T, agentUID core.AgentUID, agentName string, bindings core.ResourceBindings) {
	t.Helper()
	require.NoError(t, storage.NewBindingStore(f.db).Rebuild(context.Background(), agentUID, agentName,
		strings.Repeat("c", 64), bindings, baselineClock()))
}

// modelBindings / channelBindings 按夹具的 UID 派生规则构造一条绑定。
// modelBindings and channelBindings build one binding each with the fixture uid rule.
func modelBindings(t *testing.T, refName string) core.ResourceBindings {
	t.Helper()
	return core.ResourceBindings{ModelConnections: []core.ResourceBinding{{
		Name: refName, UID: fixtureResourceUID(t, core.ResourceModelConnection, refName),
	}}}
}

func channelBindings(t *testing.T, refName string) core.ResourceBindings {
	t.Helper()
	return core.ResourceBindings{NotifyChannels: []core.ResourceBinding{{
		Name: refName, UID: fixtureResourceUID(t, core.ResourceNotifyChannel, refName),
	}}}
}

// TestCredentialResolver_OnDemandScenarios 覆盖颗粒度文档 R4.8 的四个按需加载场景。
// 逆改点：启动时无条件读主密钥 → 场景 1 / 2 的"缺密钥仍能启动"断言红；托管失败回落
// env → 场景 3 的显式失败断言红；必需凭据不可解密却放行启动 → 场景 4 红。
// TestCredentialResolver_OnDemandScenarios covers the four on-demand loading scenarios.
func TestCredentialResolver_OnDemandScenarios(t *testing.T) {
	ctx := context.Background()

	t.Run("all_env_model_with_managed_channel", func(t *testing.T) {
		f := newResolverFixture(t)
		// 主密钥文件不存在：只要启动读了它，这条用例就会红。
		f.useKeyPath(t, filepath.Join(t.TempDir(), "absent", "master.key"))

		envVersion := f.seedVersion(t, core.ResourceModelConnection, "alpha",
			providerDefinition("alpha", "https://api.example.com/v1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
		f.publish(t, core.ResourceModelConnection, "alpha", envVersion.ID, true)
		channel := f.seedManagedChannel(t, "ops", "https://hooks.example.com/t/v1/abc")

		agentUID, err := core.NewAgentUID()
		require.NoError(t, err)
		bindings := modelBindings(t, "alpha")
		bindings.NotifyChannels = channelBindings(t, "ops").NotifyChannels
		f.project(t, agentUID, "writer", bindings)

		require.NoError(t, f.resolver.VerifyStartup(ctx, []string{"writer"}),
			"全 env 模型连接 + 托管渠道：主密钥缺失也不得阻断启动")

		_, err = f.resolver.ResolveNotifyCredential(ctx, channel)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable,
			"缺主密钥时托管发送必须显式失败，不得伪装成功")

		// 补齐主密钥后同一次发送按需解析成功：密钥只在真正发送时读取。
		f.useKeyPath(t, f.keyPath)
		got, err := f.resolver.ResolveNotifyCredential(ctx, channel)
		require.NoError(t, err)
		require.Equal(t, "https://hooks.example.com/t/v1/abc", got.Target)
		require.NotEmpty(t, got.CredentialID, "解析结果必须带上实际使用的凭据身份")
		require.Equal(t, 1, got.CredentialVersion, "解析结果必须带上实际使用的版本号")
	})

	t.Run("empty_instance_can_create_first_managed_connection", func(t *testing.T) {
		f := newResolverFixture(t)

		require.NoError(t, f.resolver.VerifyStartup(ctx, []string{"writer"}),
			"没有任何资源时启动通过，不要求先有密钥使用方")

		version := f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
		require.NoError(t, err)
		credID := f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")

		got, err := f.resolver.ResolveModelCredential(ctx, core.ResourceVersionRef{
			Name:        "alpha",
			ResourceUID: fixtureResourceUID(t, core.ResourceModelConnection, "alpha"),
			VersionID:   version.ID,
			VersionNo:   version.VersionNo,
			ContentHash: version.ContentHash,
		})
		require.NoError(t, err, "首个托管连接建好后必须能按需解析")
		require.Equal(t, "sk-alpha-plaintext", got)
		require.NotEmpty(t, credID)
	})

	t.Run("missing_master_key_fails_managed_operations_only", func(t *testing.T) {
		f := newResolverFixture(t)
		f.useKeyPath(t, "")

		envVersion := f.seedVersion(t, core.ResourceModelConnection, "alpha",
			providerDefinition("alpha", "https://api.example.com/v1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
		f.publish(t, core.ResourceModelConnection, "alpha", envVersion.ID, true)
		managed := f.seedManagedModelConnection(t, "beta", "https://api.example.com/v2")
		target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v2", "")
		require.NoError(t, err)
		f.seedSealedCredential(t, core.ResourceModelConnection, "beta", target, "sk-beta-plaintext")
		channel := f.seedManagedChannel(t, "ops", "https://hooks.example.com/t/v1/abc")

		agentUID, err := core.NewAgentUID()
		require.NoError(t, err)
		f.project(t, agentUID, "writer", modelBindings(t, "alpha"))

		require.NoError(t, f.resolver.VerifyStartup(ctx, []string{"writer"}),
			"没有必需托管模型连接时，未配置主密钥也不得阻断启动")

		_, err = f.resolver.ResolveModelCredential(ctx, core.ResourceVersionRef{
			Name:        "beta",
			ResourceUID: fixtureResourceUID(t, core.ResourceModelConnection, "beta"),
			VersionID:   managed.ID,
		})
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "secrets.master_key_file",
			"错误必须点名部署配置项，而不是笼统的'凭据不可用'")

		_, err = f.resolver.ResolveNotifyCredential(ctx, channel)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	})

	t.Run("undecryptable_required_credential_refuses_startup", func(t *testing.T) {
		f := newResolverFixture(t)
		f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
		require.NoError(t, err)
		f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")

		agentUID, err := core.NewAgentUID()
		require.NoError(t, err)
		f.project(t, agentUID, "writer", modelBindings(t, "alpha"))

		// 换一把主密钥：文件存在、格式合法，但解不开库里已有的密文。
		wrong := testMasterKey()
		wrong.KeyID = "k_rotated"
		wrong.Key[0] ^= 0xFF
		t.Cleanup(func() { wrong.Zero() })
		f.useKeyPath(t, writeMasterKeyFile(t, t.TempDir(), wrong.KeyID, wrong.Key))

		err = f.resolver.VerifyStartup(ctx, []string{"writer"})
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "alpha", "拒绝启动必须点名资源")

		// 密钥文件整体缺失同样拒绝启动，且不点名任何密钥材料。
		f.useKeyPath(t, filepath.Join(t.TempDir(), "absent", "master.key"))
		err = f.resolver.VerifyStartup(ctx, []string{"writer"})
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "alpha")
		require.NotContains(t, err.Error(), "sk-alpha-plaintext")
	})
}

// TestCredentialResolver_StartupDependencyScope 覆盖启动依赖的判定边界：只有当前发布
// 且被**活动区已注册** Agent 绑定的托管模型连接才算依赖；未绑定、仅归档引用、托管
// 渠道与 env 连接一律不读主密钥。
// TestCredentialResolver_StartupDependencyScope pins the minimal startup dependency.
func TestCredentialResolver_StartupDependencyScope(t *testing.T) {
	ctx := context.Background()
	missingKey := filepath.Join(t.TempDir(), "absent", "master.key")

	t.Run("unbound_managed_connection", func(t *testing.T) {
		f := newResolverFixture(t)
		f.useKeyPath(t, missingKey)
		f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		require.NoError(t, f.resolver.VerifyStartup(ctx, []string{"writer"}),
			"没有 Agent 绑定的托管连接不是启动依赖")
	})

	t.Run("archived_only_binding", func(t *testing.T) {
		f := newResolverFixture(t)
		f.useKeyPath(t, missingKey)
		f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		agentUID, err := core.NewAgentUID()
		require.NoError(t, err)
		// 投影里保留归档副本的绑定行；活动区名单里没有它。
		f.project(t, agentUID, "old-writer", modelBindings(t, "alpha"))
		require.NoError(t, f.resolver.VerifyStartup(ctx, []string{"writer"}),
			"仅归档副本引用不算启动依赖")
	})

	t.Run("managed_channel_never_blocks_startup", func(t *testing.T) {
		f := newResolverFixture(t)
		f.useKeyPath(t, missingKey)
		channel := f.seedManagedChannel(t, "ops", "https://hooks.example.com/t/v1/abc")
		agentUID, err := core.NewAgentUID()
		require.NoError(t, err)
		f.project(t, agentUID, "writer", channelBindings(t, "ops"))
		require.NoError(t, f.resolver.VerifyStartup(ctx, []string{"writer"}),
			"托管通知渠道不是启动依赖："+channel.Name)
	})

	t.Run("env_connection_never_reads_key", func(t *testing.T) {
		f := newResolverFixture(t)
		f.useKeyPath(t, missingKey)
		version := f.seedVersion(t, core.ResourceModelConnection, "alpha",
			providerDefinition("alpha", "https://api.example.com/v1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
		f.publish(t, core.ResourceModelConnection, "alpha", version.ID, true)
		agentUID, err := core.NewAgentUID()
		require.NoError(t, err)
		f.project(t, agentUID, "writer", modelBindings(t, "alpha"))
		require.NoError(t, f.resolver.VerifyStartup(ctx, []string{"writer"}),
			"env 模式连接不读主密钥")
	})

	t.Run("bound_managed_connection_is_required", func(t *testing.T) {
		f := newResolverFixture(t)
		f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
		require.NoError(t, err)
		f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")
		agentUID, err := core.NewAgentUID()
		require.NoError(t, err)
		f.project(t, agentUID, "writer", modelBindings(t, "alpha"))

		f.useKeyPath(t, missingKey)
		err = f.resolver.VerifyStartup(ctx, []string{"writer"})
		require.ErrorIs(t, err, core.ErrCredentialUnavailable, "必需的托管凭据缺失时不得放行启动")
		require.Contains(t, err.Error(), "alpha")

		f.useKeyPath(t, f.keyPath)
		require.NoError(t, f.resolver.VerifyStartup(ctx, []string{"writer"}))
	})
}

// TestCredentialResolver_CheckBinding 覆盖准入期只读检查：managed 且 active 通过、
// env 模式恒通过、撤销 → `ErrCredentialRevoked`（409）、`base_url` 漂移 → 不可用
// （必须重新录入 Key），以及不支持的资源类别被拒绝。检查本身不解密。
// TestCredentialResolver_CheckBinding covers the read-only admission check.
func TestCredentialResolver_CheckBinding(t *testing.T) {
	ctx := context.Background()
	f := newResolverFixture(t)

	baseURL := "https://api.example.com/v1"
	f.seedManagedModelConnection(t, "alpha", baseURL)
	target, err := CredentialTargetBinding(core.BindingModelConnection, baseURL, "")
	require.NoError(t, err)
	credID := f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")
	alphaUID := fixtureResourceUID(t, core.ResourceModelConnection, "alpha")

	require.NoError(t, f.resolver.CheckBinding(ctx, core.ResourceModelConnection, alphaUID))

	envVersion := f.seedVersion(t, core.ResourceModelConnection, "beta",
		providerDefinition("beta", "https://api.example.com/beta", "${BASALT_BETA_KEY}", "beta-model"), "", "")
	f.publish(t, core.ResourceModelConnection, "beta", envVersion.ID, true)
	require.NoError(t, f.resolver.CheckBinding(ctx,
		core.ResourceModelConnection, fixtureResourceUID(t, core.ResourceModelConnection, "beta")),
		"env 模式没有凭据身份，准入必须通过")

	// 目标地址漂移：发布新版本改了 base_url，旧密文与当前定义不再对应。
	drifted := f.seedVersion(t, core.ResourceModelConnection, "alpha",
		managedProviderDefinition("alpha", "https://api.example.com/v2", "alpha-model"), "", "")
	f.publish(t, core.ResourceModelConnection, "alpha", drifted.ID, true)
	err = f.resolver.CheckBinding(ctx, core.ResourceModelConnection, alphaUID)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Contains(t, err.Error(), "目标地址")

	require.NoError(t, f.credentials.Revoke(ctx, credID, f.opID, baselineClock()))
	err = f.resolver.CheckBinding(ctx, core.ResourceModelConnection, alphaUID)
	require.ErrorIs(t, err, core.ErrCredentialRevoked, "撤销后准入必须给出可映射 409 的固定类别")

	channel := f.seedManagedChannel(t, "ops", "https://hooks.example.com/t/v1/abc")
	require.NoError(t, f.resolver.CheckBinding(ctx, core.ResourceNotifyChannel, channel.ResourceUID))
	require.NoError(t, f.credentials.Revoke(ctx, channel.CredentialID, f.opID, baselineClock()))
	err = f.resolver.CheckBinding(ctx, core.ResourceNotifyChannel, channel.ResourceUID)
	require.ErrorIs(t, err, core.ErrCredentialRevoked)

	err = f.resolver.CheckBinding(ctx, core.ResourceSkill, alphaUID)
	require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
}
