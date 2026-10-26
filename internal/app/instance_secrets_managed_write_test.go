package app

// 本文件是 R2-1「首次保存托管凭据即完成实例初始化」的判卷：
//
//	存量实例（库在、名册不在、无初始化记录、无托管密文）→ 管理员在网页填写 API Key
//	→ 点击保存 → 同一进程内完成必要初始化并加密写入 → 解析器随即解得开。
//
// 覆盖的**逆改点**（每条对应一种真实事故）：
//   - 把"没有 secrets 段"当成纯环境变量部署 → 存量实例永远无法启用托管（A 分支红）；
//   - 有初始化记录 / 有密文时仍然生成新密钥 → B / C 分支红（旧凭据永久不可解）；
//   - 初始化后不更新共享 locator → 同一进程内的解析器仍指向空路径（D 分支红）；
//   - 审计归因写死成启动流程 → 操作行 actor 断言红（高权限变更无法归因）。
//
// These tests pin the first-managed-save flow: a provably never-managed instance is
// initialized inside the same audited admin action, the shared locator takes effect
// immediately, and every state with a record, ciphertext or explicit pure-env setting
// refuses without minting anything.

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// managedWriteFixture 是一个"存量实例"：库与既发布资源都在，但实例名册、初始化记录、
// 主密钥路径三者全空。数据根（名册推导出的私有目录所在处）指向用例自己的临时目录，
// 不碰本机真实的 LOCALAPPDATA / XDG_DATA_HOME。
//
// 夹具里的 `resolverFixture` 自带一把钥匙与一个解析器，本用例**不引用**它们：被测的
// 实例加密服务从空路径开始，写面与后建的解析器共用同一个 `MasterKeyLocator`。
// managedWriteFixture models a legacy instance: database and resources exist, while the
// roster, the record and the key path are all absent.
type managedWriteFixture struct {
	*resolverFixture
	workDir string
	dataDir string
	locator *MasterKeyLocator
	secrets *InstanceSecretsService
	store   *storage.InstanceSecretsStore
	writer  *CredentialOps
	logs    *strings.Builder
}

// newManagedWriteFixture 建夹具；mutate 可在构造前改服务入参（auto_init / 工作目录）。
// newManagedWriteFixture wires the service and the write surface on a fresh legacy
// instance; mutate adjusts the service inputs before construction.
func newManagedWriteFixture(t *testing.T, mutate func(*InstanceSecretsDeps)) *managedWriteFixture {
	t.Helper()
	env := newResolverFixture(t)
	// Windows 取 LOCALAPPDATA、Linux 取 XDG_DATA_HOME：两处都指向同一个临时根，用例在
	// 两种平台上都推导到自己的目录。
	dataDir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	workDir := t.TempDir()
	locator := NewMasterKeyLocator("")
	logs := &strings.Builder{}
	store := storage.NewInstanceSecretsStore(env.db)
	deps := InstanceSecretsDeps{
		Store:         store,
		Operations:    env.ops,
		MasterKeyFile: "",
		KeyPath:       locator,
		WorkDir:       workDir,
		WorkspaceRoot: env.root,
		AutoInit:      true,
		FreshInstance: false,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           baselineClock,
	}
	if mutate != nil {
		mutate(&deps)
	}
	secrets, err := NewInstanceSecretsService(deps)
	require.NoError(t, err)
	writer, err := NewCredentialOps(CredentialOpsDeps{
		Operations:    env.ops,
		Credentials:   env.credentials,
		Resources:     env.versions,
		Notify:        env.admission.NotifyAdmin,
		Instance:      secrets,
		MasterKeyFile: "",
		KeyPath:       locator,
		WorkspaceRoot: env.root,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           baselineClock,
	})
	require.NoError(t, err)
	return &managedWriteFixture{
		resolverFixture: env,
		workDir:         workDir,
		dataDir:         dataDir,
		locator:         locator,
		secrets:         secrets,
		store:           store,
		writer:          writer,
		logs:            logs,
	}
}

// resolverOnLocator 按生产形状构造解析器，只依赖共享 locator——初始化之后它必须
// 立刻解得开新写入的密文，不需要重启或重建。
// resolverOnLocator builds the production-shaped resolver over the shared locator.
func (f *managedWriteFixture) resolverOnLocator(t *testing.T) *credentialResolver {
	t.Helper()
	resolver, err := newCredentialResolver(CredentialResolverDeps{
		Credentials:   f.credentials,
		Resources:     f.versions,
		Notify:        f.admission.NotifyAdmin,
		Bindings:      storage.NewBindingStore(f.db),
		KeyPath:       f.locator,
		WorkspaceRoot: f.root,
	})
	require.NoError(t, err)
	return resolver
}

// markerPresent 报告实例名册是否存在。
// markerPresent reports whether the roster exists.
func (f *managedWriteFixture) markerPresent(t *testing.T) bool {
	t.Helper()
	_, present, err := readInstanceMarker(f.workDir)
	require.NoError(t, err)
	return present
}

// keyFilesUnder 列出数据根下所有主密钥文件（拒绝路径必须一个都没有）。
// keyFilesUnder lists every key file under the data root.
func (f *managedWriteFixture) keyFilesUnder(t *testing.T) []string {
	t.Helper()
	var found []string
	walkErr := filepath.WalkDir(f.dataDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == masterKeyFileName {
			found = append(found, path)
		}
		return nil
	})
	require.NoError(t, walkErr)
	return found
}

// secretsInitRows 列出初始化操作行。
// secretsInitRows lists the secrets-init operation rows.
func (f *managedWriteFixture) secretsInitRows(t *testing.T) []core.OperationRecord {
	t.Helper()
	rows, err := f.ops.ListOperations(context.Background(), core.OperationFilter{
		Action: core.ActionSystemSecretsInit,
		Limit:  20,
	})
	require.NoError(t, err)
	return rows
}

// managedWriteActor 是发起保存的管理员身份（bearer 管理 token）。
// managedWriteActor is the admin identity that performs the save.
func managedWriteActor() core.Actor {
	return core.Actor{
		Subject: "token:0123456789abcdef", Method: core.AuthMethodBearer,
		Source: "127.0.0.1:5000",
	}
}

// TestInstanceSecrets_FirstManagedSaveInitializesLegacyInstance 是 R2-1 的主链路：
// 存量实例 → 保存托管凭据 → 名册 / 密钥 / 记录在同一次受控动作里落地 → 同一进程的
// 解析器立刻解得开 → 再保存一次是幂等的（不生成第二把密钥）。
// TestInstanceSecrets_FirstManagedSaveInitializesLegacyInstance pins the whole chain.
func TestInstanceSecrets_FirstManagedSaveInitializesLegacyInstance(t *testing.T) {
	ctx := context.Background()
	f := newManagedWriteFixture(t, nil)
	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	uid := connectionUID(t, "alpha")
	actor := managedWriteActor()

	// 保存之前：名册不在、记录不在、路径为空——存量实例的真实形状。
	require.False(t, f.markerPresent(t), "存量实例保存前没有名册")
	_, exists, err := f.store.Load(ctx, core.InstanceSecretScopeMasterKey)
	require.NoError(t, err)
	require.False(t, exists, "存量实例保存前没有初始化记录")
	require.Empty(t, f.locator.Path(), "存量实例启动时解析不出主密钥路径")
	require.Empty(t, f.keyFilesUnder(t), "存量实例保存前没有主密钥文件")

	// 网页保存的前置动作：检查 + 必要初始化（与模型连接保存路径调用同一入口）。
	require.NoError(t, f.writer.PrepareManagedWrite(ctx, actor))

	marker, present, err := readInstanceMarker(f.workDir)
	require.NoError(t, err)
	require.True(t, present, "保存动作必须为存量实例建立名册")
	require.Regexp(t, `^inst_[0-9a-f]{16}$`, marker.InstanceID)

	require.NotEmpty(t, f.locator.Path(), "初始化成功后共享 locator 必须立即生效")
	keyPath := f.locator.Path()
	require.FileExists(t, keyPath)
	require.True(t, strings.HasPrefix(keyPath, filepath.Join(f.dataDir, "basalt", "instances")),
		"缺省主密钥必须落在实例私有目录（数据根下），实际 %s", keyPath)

	record, exists, err := f.store.Load(ctx, core.InstanceSecretScopeMasterKey)
	require.NoError(t, err)
	require.True(t, exists, "初始化必须写入 instance_secrets 记录")

	key, err := LoadMasterKey(keyPath, f.root)
	require.NoError(t, err)
	require.Equal(t, record.KeyID, key.KeyID, "记录与密钥必须一致")
	key.Zero()

	rows := f.secretsInitRows(t)
	require.Len(t, rows, 1, "一次保存只产生一条初始化操作行")
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, actor.Subject, rows[0].ActorSubject, "初始化审计必须归因到发起保存的管理员")
	require.Equal(t, core.AuthMethodBearer, rows[0].AuthMethod)

	// 加密保存：与模型连接保存路径同一入口。
	result, err := f.writer.Write(ctx, CredentialWriteRequest{
		Actor: actor,
		Kind:  core.BindingModelConnection,
		Name:  "alpha",
		UID:   uid,
		Mode:  core.CredentialModeManaged,
		Value: []byte("sk-legacy-secret-1"),
	})
	require.NoError(t, err)
	require.True(t, result.Changed)
	require.Equal(t, core.CredentialIdentityActive, result.State)

	// 同一进程内立即可用：解析器只依赖共享 locator，没有重启、没有重建。
	plaintext, err := f.resolverOnLocator(t).ResolveModelCredential(ctx, core.ResourceVersionRef{
		Name: "alpha", ResourceUID: uid,
	})
	require.NoError(t, err)
	require.Equal(t, "sk-legacy-secret-1", plaintext)

	// 第二次保存是幂等的：已有记录 + 密钥 → Prepare 只读，不再生成、不覆盖密钥。
	before, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	require.NoError(t, f.writer.PrepareManagedWrite(ctx, actor))
	require.Len(t, f.secretsInitRows(t), 1, "已就绪实例不得重复初始化")
	after, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "密钥文件不得被覆盖")
	require.NotContains(t, f.logs.String(), "sk-legacy-secret-1", "日志不得出现明文")
}

// TestInstanceSecrets_ManagedWriteRefusesNonProvableStates：三条拒绝路径——有记录但
// 解析不出路径（key_path_missing）、有密文却无记录无密钥（unprovable_provenance）、
// 显式纯环境变量部署（auto_init: false）——都必须失败关闭且**不生成任何密钥**。
// TestInstanceSecrets_ManagedWriteRefusesNonProvableStates pins the three refusals.
func TestInstanceSecrets_ManagedWriteRefusesNonProvableStates(t *testing.T) {
	ctx := context.Background()

	t.Run("record without resolvable key path", func(t *testing.T) {
		f := newManagedWriteFixture(t, nil)
		require.NoError(t, f.store.Insert(ctx, storage.InstanceSecretRecord{
			Scope:         core.InstanceSecretScopeMasterKey,
			KeyID:         "mk-historic",
			Fingerprint:   "fp-historic",
			InitializedAt: baselineClock(),
			OperationID:   "op-historic",
		}))

		err := f.writer.PrepareManagedWrite(ctx, managedWriteActor())
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "恢复态", "有记录却无路径必须按恢复态拒绝，不是当成新实例")
		require.False(t, f.markerPresent(t), "拒绝路径不得建立名册")
		require.Empty(t, f.locator.Path())
		require.Empty(t, f.keyFilesUnder(t), "拒绝路径不得生成任何主密钥")
		require.Empty(t, f.secretsInitRows(t), "预写之前拒绝：零操作行")
	})

	t.Run("ciphertext without record or key", func(t *testing.T) {
		f := newManagedWriteFixture(t, nil)
		f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		target, targetErr := CredentialTargetBinding(core.BindingModelConnection,
			"https://api.example.com/v1", "")
		require.NoError(t, targetErr)
		f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target,
			"sk-historic-secret")

		err := f.writer.PrepareManagedWrite(ctx, managedWriteActor())
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "恢复态")
		require.False(t, f.markerPresent(t), "有密文时不得建立新身份")
		require.Empty(t, f.keyFilesUnder(t), "有密文时不得生成替代密钥")
		require.Empty(t, f.secretsInitRows(t))
	})

	t.Run("explicit pure env deployment", func(t *testing.T) {
		f := newManagedWriteFixture(t, func(deps *InstanceSecretsDeps) {
			deps.AutoInit = false
		})

		err := f.writer.PrepareManagedWrite(ctx, managedWriteActor())
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "纯环境变量部署",
			"只有显式 auto_init: false 才是纯 env；缺少 secrets 段不是")
		require.False(t, f.markerPresent(t))
		require.Empty(t, f.keyFilesUnder(t))
		require.Empty(t, f.secretsInitRows(t))
	})
}

// TestInstanceSecrets_ManagedWriteWithoutInstancePortFallsBack：半截装配（没有实例
// 加密服务）时保存路径仍只校验主密钥可装载，且失败是 503——不伪造一个能初始化的实例。
// TestInstanceSecrets_ManagedWriteWithoutInstancePortFallsBack pins the wiring-gap path.
func TestInstanceSecrets_ManagedWriteWithoutInstancePortFallsBack(t *testing.T) {
	ctx := context.Background()
	env := newResolverFixture(t)
	f := &credentialOpsFixture{resolverFixture: env, logs: &strings.Builder{}}
	f.credOps = newTestCredentialOps(t, env, env.opStore(), f.logs)

	err := f.credOps.PrepareManagedWrite(ctx, managedWriteActor())
	require.NoError(t, err, "夹具主密钥可装载：无实例端口时退回只读校验")

	missing := filepath.Join(t.TempDir(), "missing", "master.key")
	ops, err := NewCredentialOps(CredentialOpsDeps{
		Operations:    env.ops,
		Credentials:   env.credentials,
		Resources:     env.versions,
		Notify:        env.admission.NotifyAdmin,
		MasterKeyFile: missing,
		WorkspaceRoot: env.root,
		Now:           baselineClock,
	})
	require.NoError(t, err)
	require.ErrorIs(t, ops.PrepareManagedWrite(ctx, managedWriteActor()), core.ErrCredentialUnavailable,
		"主密钥不可装载必须是 503，不得静默通过")
}
