//go:build integration

package app

// 本文件是「托管凭据首次写入」这条链的判卷，判据面在 2026-10-29 按 H1 改过向：
//
//	**新实例**（库与名册都是本次新建）→ 管理员在网页填写 API Key → 点击保存 →
//	完成初始化并加密写入 → 解析器随即解得开；
//	**存量实例**（库在、名册不在）→ 同样的保存动作 → `roster_missing_requires_adopt`，
//	零写入、零铸钥、零外呼——就地补一份名册会把"来源可证"换成"来源被重建"。
//
// 覆盖的**逆改点**（每条对应一种真实事故）：
//   - 存量实例缺名册也照旧建名册 → `ExistingInstanceWithoutRosterRefusesManagedInit` 红
//     （身份被就地重建，此后无法区分原装与补造）；
//   - 为了"堵住"存量实例把新实例的自动初始化一起关掉 → `FreshInstanceStillAutoInitializes` 红
//     （全新部署永远起不来，或管理员每次都要手工点一次）；
//   - 有初始化记录 / 有密文时仍然生成新密钥 → 拒绝路径用例红（旧凭据永久不可解）；
//   - 初始化后不更新共享 locator → 同一进程内的解析器仍指向空路径（解析用例红）；
//   - 审计归因写死成启动流程 → 操作行 actor 断言红（高权限变更无法归因）。
//
// These tests pin the first-managed-write chain after the 2026-10-29 H1 change: only a
// provably-new instance may establish a roster in place; an existing instance without
// one is refused with a stable code and nothing is written.

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
	"github.com/ymedword/basalt/internal/storage/postgres"
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
	store   *postgres.InstanceSecretsStore
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
	store := postgres.NewInstanceSecretsStore(env.db)
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
		Bindings:      postgres.NewBindingStore(f.db),
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

// TestInstanceSecrets_ExistingInstanceWithoutRosterRefusesManagedInit 判 H1：存量实例
// （库在、名册不在）既不能通过网页保存、也不能通过显式初始化就地建立身份。
// 两条入口都必须给出稳定码 `roster_missing_requires_adopt`，并且零写入：名册不建、
// 密钥不铸、记录不写、审计不落——拒绝发生在任何副作用之前。
//
// 本用例替换原 `TestInstanceSecrets_FirstManagedSaveInitializesLegacyInstance`：
// 那条用例把"存量实例首次保存即就地初始化"当成正确行为，H1 判定它正是要被堵住的路径。
// TestInstanceSecrets_ExistingInstanceWithoutRosterRefusesManagedInit pins the H1 refusal
// on both entry points.
func TestInstanceSecrets_ExistingInstanceWithoutRosterRefusesManagedInit(t *testing.T) {
	ctx := context.Background()

	// requireRefused 断言拒绝三件套 + 零写入。
	requireRefused := func(t *testing.T, f *managedWriteFixture, err error) {
		t.Helper()
		require.Error(t, err)
		var refusal *secretsRefusal
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, SecretsCodeRosterMissingRequiresAdopt, refusal.Code)
		require.True(t, strings.HasPrefix(err.Error(), SecretsCodeRosterMissingRequiresAdopt+":"),
			"错误文案必须以稳定码开头：%s", err)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable,
			"管理面仍按 409 处理：码给运维，哨兵错误给调用链")
		require.False(t, f.markerPresent(t), "拒绝路径不得建立名册")
		require.Empty(t, f.locator.Path(), "拒绝路径不得让 locator 生效")
		require.Empty(t, f.keyFilesUnder(t), "拒绝路径不得生成任何主密钥")
		_, exists, loadErr := f.store.Load(ctx, core.InstanceSecretScopeMasterKey)
		require.NoError(t, loadErr)
		require.False(t, exists, "拒绝路径不得写初始化记录")
		require.Empty(t, f.secretsInitRows(t), "拒绝路径不得写初始化审计")
	}

	t.Run("managed_write_is_refused", func(t *testing.T) {
		f := newManagedWriteFixture(t, nil)
		f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		require.False(t, f.markerPresent(t), "存量实例保存前没有名册")

		requireRefused(t, f, f.writer.PrepareManagedWrite(ctx, managedWriteActor()))
	})

	t.Run("explicit_init_is_refused", func(t *testing.T) {
		f := newManagedWriteFixture(t, nil)
		require.False(t, f.markerPresent(t))

		_, err := f.secrets.Init(ctx, managedWriteActor())
		requireRefused(t, f, err)
	})

	t.Run("existing_roster_still_allowed", func(t *testing.T) {
		// 反向保护：名册在的存量实例不能被这条门误伤——它走的是正常初始化路径。
		f := newManagedWriteFixture(t, nil)
		_, err := ensureInstanceMarker(f.workDir)
		require.NoError(t, err)
		require.True(t, f.markerPresent(t))

		require.NoError(t, f.writer.PrepareManagedWrite(ctx, managedWriteActor()))
		require.NotEmpty(t, f.locator.Path(), "名册在时初始化必须照常生效")
		require.NotEmpty(t, f.keyFilesUnder(t))
	})
}

// TestInstanceSecrets_FreshInstanceStillAutoInitializes 是新实例的反向保护：H1 的门只看
// 存量实例，`FreshInstance=true`（库与名册都是本次新建）必须照旧在同一次受控动作里
// 建名册、铸密钥、写记录、让 locator 立即生效——否则全新部署永远起不来。
// TestInstanceSecrets_FreshInstanceStillAutoInitializes keeps new-instance auto-init intact.
func TestInstanceSecrets_FreshInstanceStillAutoInitializes(t *testing.T) {
	ctx := context.Background()
	f := newManagedWriteFixture(t, func(deps *InstanceSecretsDeps) {
		deps.FreshInstance = true
	})
	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	uid := connectionUID(t, "alpha")
	actor := managedWriteActor()

	require.False(t, f.markerPresent(t), "新实例在建名册之前同样没有名册")
	require.Empty(t, f.keyFilesUnder(t))

	require.NoError(t, f.writer.PrepareManagedWrite(ctx, actor))

	marker, present, err := readInstanceMarker(f.workDir)
	require.NoError(t, err)
	require.True(t, present, "新实例必须自动建立名册")
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
		Value: []byte("sk-fresh-secret-1"),
	})
	require.NoError(t, err)
	require.True(t, result.Changed)
	require.Equal(t, core.CredentialIdentityActive, result.State)

	// 同一进程内立即可用：解析器只依赖共享 locator，没有重启、没有重建。
	plaintext, err := f.resolverOnLocator(t).ResolveModelCredential(ctx, core.ResourceVersionRef{
		Name: "alpha", ResourceUID: uid,
	})
	require.NoError(t, err)
	require.Equal(t, "sk-fresh-secret-1", plaintext)

	// 第二次保存是幂等的：已有记录 + 密钥 → Prepare 只读，不再生成、不覆盖密钥。
	before, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	require.NoError(t, f.writer.PrepareManagedWrite(ctx, actor))
	require.Len(t, f.secretsInitRows(t), 1, "已就绪实例不得重复初始化")
	after, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "密钥文件不得被覆盖")
	require.NotContains(t, f.logs.String(), "sk-fresh-secret-1", "日志不得出现明文")
}

// TestInstanceSecrets_ManagedWriteRefusesNonProvableStates：三条拒绝路径——有记录但
// 解析不出路径（key_path_missing）、有密文却无记录无密钥（unprovable_provenance）、
// 显式纯环境变量部署（auto_init: false）——都必须失败关闭且**不生成任何密钥**。
// TestInstanceSecrets_ManagedWriteRefusesNonProvableStates pins the three refusals.
func TestInstanceSecrets_ManagedWriteRefusesNonProvableStates(t *testing.T) {
	ctx := context.Background()

	t.Run("record without resolvable key path", func(t *testing.T) {
		f := newManagedWriteFixture(t, nil)
		require.NoError(t, f.store.Insert(ctx, postgres.InstanceSecretRecord{
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
