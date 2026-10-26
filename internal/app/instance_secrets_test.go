package app

// 本文件是实例首次初始化与加密三态的判卷（14D T092；模块卡验收 A1–A12）。
//
// 覆盖的**逆改点**（每一条都对应一种真实事故）：
//   - 以"文件是否存在"代替三项联立 → A2 / A3 / A7 红（格式正确但内容错误的密钥被判就绪）；
//   - 有密文时不真解一遍就 adopt → A7 的反例分支红（全部托管凭据永久不可用的假成功）；
//   - 存量库自动生成密钥 → A6 红（升级时静默换钥匙）；
//   - 生成路径覆盖既有文件 → A8 红（第二把密钥接管第一把）；
//   - 发布残留直接放宽 `nlink > 1` 或删掉来源不明的文件 → A10 / A11 红；
//   - 权限校验之前就建文件 → A12 红。
//
// These tests pin the three-state decision, the exclusive publish and the residue
// recovery; each assertion names the regression it guards.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// instanceSecretsFixture 把服务面接在既有资源夹具上：同一个库、同一个工作区，
// 主密钥写在**工作区外**的独立私有目录（由被测代码自己建）。
// instanceSecretsFixture wires the service onto the resource fixture with a private
// directory outside the workspace.
type instanceSecretsFixture struct {
	*resolverFixture
	service *InstanceSecretsService
	store   *storage.InstanceSecretsStore
	keyPath string
	keyDir  string
}

// newInstanceSecretsFixture 建夹具；私有目录本身**不预建**——建目录与校验权限是
// 被测行为的一部分（T088）。
// newInstanceSecretsFixture leaves the private directory absent on purpose.
func newInstanceSecretsFixture(t *testing.T) *instanceSecretsFixture {
	t.Helper()
	env := newResolverFixture(t)
	instanceRoot := filepath.Join(t.TempDir(), "instance")
	keyPath := filepath.Join(instanceRoot, "master.key")
	store := storage.NewInstanceSecretsStore(env.db)
	service, err := NewInstanceSecretsService(InstanceSecretsDeps{
		Store:         store,
		Operations:    env.ops,
		MasterKeyFile: keyPath,
		WorkspaceRoot: env.root,
		AutoInit:      true,
		FreshInstance: true,
		Logger:        slog.New(slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           baselineClock,
	})
	require.NoError(t, err)
	return &instanceSecretsFixture{
		resolverFixture: env,
		service:         service,
		store:           store,
		keyPath:         keyPath,
		keyDir:          instanceRoot,
	}
}

// record 读当前初始化记录（未命中即失败——多数用例需要它存在）。
// record loads the instance record; a miss fails the test.
func (f *instanceSecretsFixture) record(t *testing.T) storage.InstanceSecretRecord {
	t.Helper()
	record, exists, err := f.store.Load(context.Background(), core.InstanceSecretScopeMasterKey)
	require.NoError(t, err)
	require.True(t, exists, "instance_secrets 记录必须存在")
	return record
}

// secretsInitOps 列出全部初始化操作行。
// secretsInitOps lists the secrets-init operation rows.
func (f *instanceSecretsFixture) secretsInitOps(t *testing.T) []core.OperationRecord {
	t.Helper()
	rows, err := f.ops.ListOperations(context.Background(), core.OperationFilter{
		Action: core.ActionSystemSecretsInit,
		Limit:  20,
	})
	require.NoError(t, err)
	return rows
}

// TestInstanceSecrets_FirstStartGenerates 覆盖 A1：可证明的新实例自动生成密钥 →
// 写记录 → 审计 success，且私有目录里只有最终文件（没有残留临时链接）。
// 逆改点：不写记录、审计写成 failed、或把临时链接留在盘上都会红。
func TestInstanceSecrets_FirstStartGenerates(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateReady, status.Decision.State)
	require.Equal(t, core.SecretRecoveryNone, status.Decision.Reason)
	require.NotEmpty(t, status.KeyID)
	require.Len(t, status.Fingerprint, masterKeyFingerprintLen)

	key, err := LoadMasterKey(f.keyPath, f.root)
	require.NoError(t, err)
	defer key.Zero()
	require.Equal(t, status.KeyID, key.KeyID, "记录与文件必须是同一把密钥")

	record := f.record(t)
	require.Equal(t, status.KeyID, record.KeyID)
	require.Equal(t, status.Fingerprint, record.Fingerprint)
	require.Equal(t, baselineClock().UnixMilli(), record.InitializedAt.UnixMilli())

	ops := f.secretsInitOps(t)
	require.Len(t, ops, 1)
	require.Equal(t, core.OperationSuccess, ops[0].Status)
	require.Equal(t, core.TargetKindSystem, ops[0].TargetKind)
	require.Equal(t, core.InstanceTargetMasterKey, ops[0].TargetID)
	require.Equal(t, record.OperationID, ops[0].OperationID)
	require.JSONEq(t, `{"key_id":"`+status.KeyID+`","fingerprint":"`+status.Fingerprint+`","recovered":false}`,
		string(ops[0].DetailJSON))
	require.Equal(t, core.ActorSubjectMigration, ops[0].ActorSubject,
		"自动初始化由启动流程发起，不冒充管理台身份")

	entries, err := os.ReadDir(f.keyDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "私有目录只应有最终密钥文件")
	require.Equal(t, "master.key", entries[0].Name())

	info, err := os.Lstat(f.keyPath)
	require.NoError(t, err)
	links, known := hardLinkCount(f.keyPath, info)
	require.True(t, known)
	require.Equal(t, uint64(1), links, "发布完成后链接数必须回到 1")
}

// TestInstanceSecrets_MissingKeyRecoveryRequired 覆盖 A2：记录存在而密钥文件缺失
// 一律进恢复态，**不**生成替代密钥。
// 逆改点：把"文件不存在"判成未初始化并自动生成 → 全部托管密文永久不可用。
func TestInstanceSecrets_MissingKeyRecoveryRequired(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)

	_, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	record := f.record(t)
	require.NoError(t, os.Remove(f.keyPath))

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateRecoveryRequired, status.Decision.State)
	require.Equal(t, core.SecretRecoveryMissing, status.Decision.Reason)
	require.Equal(t, SecretsActionRecover, status.Decision.Action)

	_, statErr := os.Lstat(f.keyPath)
	require.ErrorIs(t, statErr, os.ErrNotExist, "恢复态不得生成替代密钥")
	require.Equal(t, record.KeyID, f.record(t).KeyID, "记录不得被改写")

	_, err = f.service.Init(ctx, core.Actor{})
	require.Error(t, err, "恢复态下显式初始化同样拒绝")
	require.Contains(t, err.Error(), "recovery_required")
}

// TestInstanceSecrets_KeyIDMismatch 覆盖 A3：文件可解析但 `key_id` 与记录不符 →
// 恢复态，且文件一个字节都不动。
// 逆改点：按"文件能读"就判就绪 → 用错的密钥加密下一份凭据。
func TestInstanceSecrets_KeyIDMismatch(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)
	_, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.NoError(t, os.Remove(f.keyPath))

	other := testMasterKey()
	other.KeyID = "k_otherinstance000000000001"
	writeMasterKeyFile(t, f.keyDir, other.KeyID, other.Key)
	before, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateRecoveryRequired, status.Decision.State)
	require.Equal(t, core.SecretRecoveryKeyIDMismatch, status.Decision.Reason)

	after, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "恢复态不得改写来源不明的密钥文件")
}

// TestInstanceSecrets_ConcurrentSingleWinner 覆盖 A4，分两段：
//
//	① 排他发布是硬保证：第二个发布者对既有文件必须失败，内容一个字节不变；
//	② 不加实例锁地并发跑两个 `Ensure`，只断言**最终不变量**——盘上恰好一把密钥、
//	   记录与文件同一 `key_id`、`nlink == 1`，且任何声称就绪的一方只能指向这把密钥。
//	   生产路径由 `workspace.AcquireInstanceLock` 串行化；本段故意去掉那把锁，验证
//	   即使没有它，发布层也不会让两把密钥同时有效。
//
// 逆改点：用 `os.Rename` 发布 / 先 stat 再写 → 覆盖窗口出现，两个 key_id 都"有效"。
func TestInstanceSecrets_ConcurrentSingleWinner(t *testing.T) {
	ctx := context.Background()

	// ① 排他发布。
	existing := newInstanceSecretsFixture(t)
	_, err := existing.service.Ensure(ctx)
	require.NoError(t, err)
	published, err := os.ReadFile(existing.keyPath)
	require.NoError(t, err)
	replacement := testMasterKey()
	replacement.KeyID = "k_replacement0000000000001"
	require.ErrorIs(t, publishMasterKey(existing.keyPath, existing.root, replacement),
		errMasterKeyAlreadyExists, "第二个发布者必须被排他语义挡住")
	afterPublish, err := os.ReadFile(existing.keyPath)
	require.NoError(t, err)
	require.Equal(t, published, afterPublish, "既有密钥文件一个字节都不得被覆盖")

	// ② 并发 Ensure（无实例锁）。
	f := newInstanceSecretsFixture(t)
	second := *f.service
	second.deps.Logger = slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	var wait sync.WaitGroup
	results := make([]InstanceSecretsStatus, 2)
	errs := make([]error, 2)
	for index, service := range []*InstanceSecretsService{f.service, &second} {
		wait.Add(1)
		go func(index int, service *InstanceSecretsService) {
			defer wait.Done()
			results[index], errs[index] = service.Ensure(context.Background())
		}(index, service)
	}
	wait.Wait()

	key, err := LoadMasterKey(f.keyPath, f.root)
	require.NoError(t, err)
	defer key.Zero()
	record := f.record(t)
	require.Equal(t, key.KeyID, record.KeyID, "记录必须指向盘上那把密钥")

	ready := 0
	for index := range results {
		if errs[index] != nil || results[index].Decision.State != core.SecretStateReady {
			// 输家允许失败：排他发布把它挡在门外，这是 fail-closed 的预期形态。
			continue
		}
		require.Equal(t, key.KeyID, results[index].KeyID, "就绪的初始化者只能是同一把密钥")
		ready++
	}
	require.GreaterOrEqual(t, ready, 1, "至少要有一个初始化者成功")

	entries, err := os.ReadDir(f.keyDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "竞争结束后不得留下第二把密钥或残留临时链接")
}

// TestInstanceSecrets_AdoptAfterRecordFailure 覆盖 A5：密钥已发布而记录没写成功
// （崩溃窗口）时，下次启动走 adopt——沿用同一把密钥，**不**重新生成。
// 夹具直接用生产方式发布密钥（`publishMasterKey`）而不写记录，落在同一个状态上。
// 逆改点：把"有文件无记录"当未初始化 → A5 红，且重试必然换钥匙。
func TestInstanceSecrets_AdoptAfterRecordFailure(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)
	require.NoError(t, prepareSecretDirectory(f.keyDir))
	key := testMasterKey()
	require.NoError(t, publishMasterKey(f.keyPath, f.root, key))
	before, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateReady, status.Decision.State)
	require.Equal(t, key.KeyID, status.KeyID, "adopt 必须沿用盘上那把密钥")

	after, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "adopt 不得重写密钥文件")

	record := f.record(t)
	require.Equal(t, key.KeyID, record.KeyID)
	ops := f.secretsInitOps(t)
	require.Len(t, ops, 1)
	require.Equal(t, core.OperationSuccess, ops[0].Status)
}

// TestInstanceSecrets_ExistingInstanceNotAutoInit 覆盖 A6：存量库（非本次新建）
// 不自动生成，页面拿到「未初始化」等人类动作，且盘上不多出任何文件。
// 逆改点：只看"没有记录"就自动生成 → 升级时静默换钥匙。
func TestInstanceSecrets_ExistingInstanceNotAutoInit(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)
	f.service.deps.FreshInstance = false

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateUninitialized, status.Decision.State)
	require.Equal(t, SecretsActionAwaitInit, status.Decision.Action)
	require.Equal(t, core.SecretRecoveryNone, status.Decision.Reason)

	_, statErr := os.Lstat(f.keyPath)
	require.ErrorIs(t, statErr, os.ErrNotExist, "存量实例不得自动生成密钥")
	_, exists, err := f.store.Load(ctx, core.InstanceSecretScopeMasterKey)
	require.NoError(t, err)
	require.False(t, exists)
	require.Empty(t, f.secretsInitOps(t))
}

// TestInstanceSecrets_AdoptMatchesCiphertext 覆盖 A7：无记录但既有密钥又有密文时
// 必须真解一遍——解得开才 adopt，解不开就是恢复态，且**不**写记录。
// 逆改点：只校验路径与权限就 adopt → 全部托管凭据永久不可用而页面显示就绪。
func TestInstanceSecrets_AdoptMatchesCiphertext(t *testing.T) {
	target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
	require.NoError(t, err)

	t.Run("密钥解得开现有密文则 adopt", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")
		require.NoError(t, prepareSecretDirectory(f.keyDir))
		writeMasterKeyFile(t, f.keyDir, f.key.KeyID, f.key.Key)

		status, err := f.service.Ensure(context.Background())
		require.NoError(t, err)
		require.Equal(t, core.SecretStateReady, status.Decision.State)
		require.Equal(t, f.key.KeyID, f.record(t).KeyID)
	})

	t.Run("密钥解不开现有密文则进恢复态", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")
		require.NoError(t, prepareSecretDirectory(f.keyDir))
		stranger := testMasterKey()
		stranger.KeyID = "k_stranger00000000000000001"
		// 只改 key_id 不够：`testMasterKey` 的字节是固定的，那样"别人的钥匙"仍能解开密文。
		// 这里连密钥材料一起换掉，才真正落在"解不开"分支上。
		// Different bytes as well as a different id, so decryption really fails.
		for index := range stranger.Key {
			stranger.Key[index] = byte(255 - index)
		}
		writeMasterKeyFile(t, f.keyDir, stranger.KeyID, stranger.Key)

		status, err := f.service.Ensure(context.Background())
		require.NoError(t, err)
		require.Equal(t, core.SecretStateRecoveryRequired, status.Decision.State)
		require.Equal(t, core.SecretRecoveryUnprovableProvenance, status.Decision.Reason)
		_, exists, loadErr := f.store.Load(context.Background(), core.InstanceSecretScopeMasterKey)
		require.NoError(t, loadErr)
		require.False(t, exists, "试解失败不得留下初始化记录")
	})
}

// TestInstanceSecrets_RefusesOverwrite 覆盖 A8：已就绪的实例拒绝再生成，发布路径
// 对既有文件同样是排他语义。
// 逆改点：发布改用 `os.Rename`（Windows 走 MOVEFILE_REPLACE_EXISTING）→ 覆盖分支红。
func TestInstanceSecrets_RefusesOverwrite(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)
	first, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	before, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)

	_, err = f.service.Init(ctx, core.Actor{})
	require.Error(t, err, "已就绪实例必须拒绝显式初始化")

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateReady, status.Decision.State)
	require.Equal(t, first.KeyID, status.KeyID)

	replacement := testMasterKey()
	replacement.KeyID = "k_replacement0000000000001"
	err = publishMasterKey(f.keyPath, f.root, replacement)
	require.ErrorIs(t, err, errMasterKeyAlreadyExists, "发布必须对既有文件失败")

	after, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "既有密钥文件一个字节都不得被覆盖")
}

// TestInstanceSecrets_DistinctPerInstance 覆盖 A9：两个实例各自生成，密钥材料与
// `key_id` 都不同——同机多实例不得互相覆盖或共用密钥。
// 逆改点：把密钥路径写死成全局位置 → 第二个实例覆盖第一个。
func TestInstanceSecrets_DistinctPerInstance(t *testing.T) {
	ctx := context.Background()
	first := newInstanceSecretsFixture(t)
	second := newInstanceSecretsFixture(t)
	require.NotEqual(t, first.keyPath, second.keyPath)

	firstStatus, err := first.service.Ensure(ctx)
	require.NoError(t, err)
	secondStatus, err := second.service.Ensure(ctx)
	require.NoError(t, err)

	require.NotEqual(t, firstStatus.KeyID, secondStatus.KeyID)
	require.NotEqual(t, firstStatus.Fingerprint, secondStatus.Fingerprint)
	require.Equal(t, firstStatus.KeyID, first.record(t).KeyID)
	require.Equal(t, secondStatus.KeyID, second.record(t).KeyID)
}

// TestInstanceSecrets_RecoversPublicationResidue 覆盖 A10：`os.Link` 成功而
// `unlink tmp` 之前中断留下 `nlink == 2`，下次启动只删能被 `os.SameFile` 证明的
// 那条临时链接，链接数回到 1 后正常 adopt。
// 逆改点：用"全局取消 nlink>1 检查"绕过 → 真正的多路径泄露被放行；不清理 → A10 红。
func TestInstanceSecrets_RecoversPublicationResidue(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)
	require.NoError(t, prepareSecretDirectory(f.keyDir))
	key := testMasterKey()
	require.NoError(t, publishMasterKey(f.keyPath, f.root, key))

	tmpPath := f.keyPath + masterKeyTempSuffix
	require.NoError(t, os.Link(f.keyPath, tmpPath))
	info, err := os.Lstat(f.keyPath)
	require.NoError(t, err)
	links, known := hardLinkCount(f.keyPath, info)
	require.True(t, known)
	require.Equal(t, uint64(2), links, "夹具必须复现发布残留窗口")

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateReady, status.Decision.State)
	require.Equal(t, key.KeyID, status.KeyID)

	_, statErr := os.Lstat(tmpPath)
	require.ErrorIs(t, statErr, os.ErrNotExist, "可证明来源的临时链接必须被清理")
	after, err := os.Lstat(f.keyPath)
	require.NoError(t, err)
	links, known = hardLinkCount(f.keyPath, after)
	require.True(t, known)
	require.Equal(t, uint64(1), links, "链接数必须回到 1")
}

// TestInstanceSecrets_UnprovableProvenanceRecovery 覆盖 A11：`nlink > 1` 而第二条
// 链接不在固定临时路径上（或临时链接指向别的 inode）——来源不可证明，进恢复态，
// **不**删任何文件。
// 逆改点：见到 `nlink > 1` 就删一条 → A11 红（删掉的可能是真正的泄露副本）。
func TestInstanceSecrets_UnprovableProvenanceRecovery(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)
	require.NoError(t, prepareSecretDirectory(f.keyDir))
	key := testMasterKey()
	require.NoError(t, publishMasterKey(f.keyPath, f.root, key))

	strayLink := filepath.Join(f.keyDir, "copy-of-master.key")
	require.NoError(t, os.Link(f.keyPath, strayLink))

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateRecoveryRequired, status.Decision.State)
	require.Equal(t, core.SecretRecoveryUnprovableProvenance, status.Decision.Reason)

	require.FileExists(t, f.keyPath, "恢复态不得删除主密钥文件")
	require.FileExists(t, strayLink, "恢复态不得删除来源不明的链接")
}

// TestInstanceSecrets_WindowsDACLBeforeWrite 覆盖 A12：权限校验先于任何文件写入。
//   - 目录建不出来（父路径是文件）时，一个文件都不产生；
//   - 成功路径上私有目录权限受限（Windows 判 DACL 只允许当前用户 / SYSTEM /
//     Administrators；Unix 判 0700 + 属主），且校验发生在写密钥之前——`Ensure`
//     结束后目录里只有最终文件。
func TestInstanceSecrets_WindowsDACLBeforeWrite(t *testing.T) {
	blocking := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(blocking, []byte("x"), 0o600))
	require.Error(t, prepareSecretDirectory(filepath.Join(blocking, "instance")),
		"父路径不是目录时必须拒绝")
	entries, err := os.ReadDir(filepath.Dir(blocking))
	require.NoError(t, err)
	require.Len(t, entries, 1, "权限校验失败时不得创建任何文件")

	f := newInstanceSecretsFixture(t)
	_, err = f.service.Ensure(context.Background())
	require.NoError(t, err)
	requireDirectoryRestricted(t, f.keyDir)

	entries, err = os.ReadDir(f.keyDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "私有目录在初始化后只应有最终密钥文件")
}

// TestInstanceSecretsPort_StatusProjection：web 端口的三个投影点逐条钉住——
//   - 可证明的新实例（无记录、无密钥文件）在端口上**是**可初始化的；
//   - `Ensure` 之后同一端口报 `ready`，`key_file` 是配置路径、`initialized_at` 是
//     记录时间、`key_id` 非空且 `CanInitialize` 转 false；
//   - 纯 env 实例（没配主密钥路径）在端口上**不**可初始化：判定表的
//     `uninitialized / await_init` 单看会得出"可初始化"，没有路径却必然失败，
//     处理器必须先在预写之前回「未配置」的 409。
//
// 逆改点：把判定直接投影（漏掉 `Configured` 合取）/ 漏投影 key_file 或时间 → 红。
func TestInstanceSecretsPort_StatusProjection(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh instance is initializable", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		port := instanceSecretsPortOf(f.service)
		require.NotNil(t, port)

		status, err := port.Status(ctx)
		require.NoError(t, err)
		require.Equal(t, string(core.SecretStateUninitialized), status.State)
		require.Empty(t, status.Reason)
		require.Empty(t, status.KeyID)
		require.Equal(t, f.keyPath, status.KeyFile)
		require.True(t, status.InitializedAt.IsZero(), "未初始化没有初始化时间")
		require.True(t, status.CanInitialize)
	})

	t.Run("after ensure the port reads ready", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		_, err := f.service.Ensure(ctx)
		require.NoError(t, err)

		status, err := instanceSecretsPortOf(f.service).Status(ctx)
		require.NoError(t, err)
		require.Equal(t, string(core.SecretStateReady), status.State)
		require.NotEmpty(t, status.KeyID)
		require.Equal(t, f.keyPath, status.KeyFile)
		require.Equal(t, baselineClock().UTC(), status.InitializedAt.UTC())
		require.False(t, status.CanInitialize)
	})

	t.Run("pure env instance is not initializable", func(t *testing.T) {
		env := newResolverFixture(t)
		service, err := NewInstanceSecretsService(InstanceSecretsDeps{
			Store:         storage.NewInstanceSecretsStore(env.db),
			Operations:    env.ops,
			WorkspaceRoot: env.root,
			AutoInit:      true,
			FreshInstance: true,
			Logger:        slog.New(slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Now:           baselineClock,
		})
		require.NoError(t, err)

		status, err := instanceSecretsPortOf(service).Status(ctx)
		require.NoError(t, err)
		require.Equal(t, string(core.SecretStateUninitialized), status.State)
		require.Empty(t, status.KeyFile)
		require.False(t, status.CanInitialize, "没有主密钥路径的实例不可初始化")
	})

	t.Run("nil service projects to a nil port", func(t *testing.T) {
		require.Nil(t, instanceSecretsPortOf(nil), "缺席必须是 nil 接口，不是带 nil 指针的非 nil 接口")
	})
}

// TestInstanceSecretsPort_InitSurfacesOperationID：预写之后的失败必须把
// `SecretsInitError.OperationID` 交还 web 层——处理器据此在 500 里带
// `data.operation_id`；预写之前的拒绝保持空串，不会出现"有标识却没有操作行"。
// 逆改点：吃掉标识 / 把包装错误拆掉只剩文本 → 红。
func TestInstanceSecretsPort_InitSurfacesOperationID(t *testing.T) {
	blocking := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(blocking, []byte("x"), 0o600))

	env := newResolverFixture(t)
	service, err := NewInstanceSecretsService(InstanceSecretsDeps{
		Store:         storage.NewInstanceSecretsStore(env.db),
		Operations:    env.ops,
		MasterKeyFile: filepath.Join(blocking, "instance", "master.key"),
		WorkspaceRoot: env.root,
		AutoInit:      true,
		FreshInstance: true,
		Logger:        slog.New(slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           baselineClock,
	})
	require.NoError(t, err)

	operationID, err := instanceSecretsPortOf(service).Init(context.Background(), core.Actor{})
	require.Error(t, err)
	require.NotEmpty(t, operationID, "预写之后的失败必须带操作标识")

	var initErr *SecretsInitError
	require.ErrorAs(t, err, &initErr)
	require.Equal(t, operationID, initErr.OperationID)

	rows, listErr := env.ops.ListOperations(context.Background(), core.OperationFilter{
		Action: core.ActionSystemSecretsInit,
		Limit:  10,
	})
	require.NoError(t, listErr)
	require.NotEmpty(t, rows, "带标识的失败必须真的有一行可查证的操作记录")
}

// TestWebOptions_InstanceSecretsProjection：装配层只投影运行期里的同一个服务实例；
// 半截运行期（服务缺席）保持 nil，处理器按固定 500 失败关闭。
// 逆改点：在 webOptions 里临时构造服务 / 缺席时换成空实现 → 红。
func TestWebOptions_InstanceSecretsProjection(t *testing.T) {
	opts := (&App{}).webOptions(&heavyRuntime{cfg: &cli.Config{}})
	require.Nil(t, opts.InstanceSecrets, "服务缺席时端口必须是 nil")

	f := newInstanceSecretsFixture(t)
	rt := &heavyRuntime{cfg: &cli.Config{}, instanceSecrets: f.service}
	opts = (&App{}).webOptions(rt)
	require.NotNil(t, opts.InstanceSecrets)
	require.IsType(t, instanceSecretsPort{}, opts.InstanceSecrets)
}
