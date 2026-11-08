//go:build integration

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
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// instanceSecretsFixture 把服务面接在既有资源夹具上：同一个库、同一个工作区，
// 主密钥写在**工作区外**的独立私有目录（由被测代码自己建）。
// instanceSecretsFixture wires the service onto the resource fixture with a private
// directory outside the workspace.
type instanceSecretsFixture struct {
	*resolverFixture
	service *InstanceSecretsService
	store   *postgres.InstanceSecretsStore
	keyPath string
	keyDir  string
}

// clearBusinessHistory 把四张业务历史表清空，让夹具回到"可证明的新实例"（空库）。
// 资源夹具为准入预置了一条 `agent_lifecycle` 身份行；US-25 起自动铸钥还要求零业务
// 历史（§2.3.3），那行属于准入场景，留着会让本文件所有"新实例自动铸钥"用例误判成
// 存量库。四表的逐表判据在 internal/storage/postgres/instance_secrets_store_test.go，
// 这里只是把夹具恢复到"从未跑过业务"的状态。
// clearBusinessHistory resets the four business-history tables so the fixture starts
// as a provably-new instance; the per-table judgement lives in the postgres package.
func clearBusinessHistory(t *testing.T, db *postgres.DB) {
	t.Helper()
	for _, statement := range []string{
		`DELETE FROM task_executions`,
		`DELETE FROM runs`,
		`DELETE FROM agent_lifecycle`,
		`DELETE FROM kb_documents`,
	} {
		_, err := db.ExecContext(context.Background(), statement)
		require.NoError(t, err)
	}
}

// newInstanceSecretsFixture 建夹具；私有目录本身**不预建**——建目录与校验权限是
// 被测行为的一部分（T088）。
// newInstanceSecretsFixture leaves the private directory absent on purpose.
func newInstanceSecretsFixture(t *testing.T) *instanceSecretsFixture {
	t.Helper()
	env := newResolverFixture(t)
	clearBusinessHistory(t, env.db)
	instanceRoot := filepath.Join(t.TempDir(), "instance")
	keyPath := filepath.Join(instanceRoot, "master.key")
	store := postgres.NewInstanceSecretsStore(env.db)
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
func (f *instanceSecretsFixture) record(t *testing.T) postgres.InstanceSecretRecord {
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

// TestInstanceSecrets_AdoptMatchesCiphertext 覆盖 A7 与 D-40：无记录但既有密钥
// 又有密文时**一律**恢复态——解不开当然不 adopt（A7 原判据），解得开也不再 adopt
// （D-40，2026-10-27 裁决：能解不等于来源可信），两种情况都**不**写记录。
// 逆改点：把"密钥能解开现存密文"当来源证明去 adopt → 无主来源被采纳成原生身份，
// 事后与真正的丢记录无法区分（第一子例红）。
func TestInstanceSecrets_AdoptMatchesCiphertext(t *testing.T) {
	target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
	require.NoError(t, err)

	t.Run("密钥解得开现有密文也不 adopt", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")
		require.NoError(t, prepareSecretDirectory(f.keyDir))
		writeMasterKeyFile(t, f.keyDir, f.key.KeyID, f.key.Key)

		status, err := f.service.Ensure(context.Background())
		require.NoError(t, err)
		require.Equal(t, core.SecretStateRecoveryRequired, status.Decision.State,
			"D-40：即使这把钥匙能解开全部密文，也不得自动 adopt")
		require.Equal(t, core.SecretRecoveryUnprovableProvenance, status.Decision.Reason)
		_, exists, loadErr := f.store.Load(context.Background(), core.InstanceSecretScopeMasterKey)
		require.NoError(t, loadErr)
		require.False(t, exists, "D-40：无记录 + 有密文时启动不得补写记录")
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

// secretsRecordCount 报告实例主密钥初始化记录是否存在：本模块只有 `master_key`
// 一个 scope，因此"有记录"即 1、无记录即 0。
// secretsRecordCount reports whether the master-key record exists (1 or 0).
func (f *instanceSecretsFixture) secretsRecordCount(t *testing.T) int {
	t.Helper()
	_, exists, err := f.store.Load(context.Background(), core.InstanceSecretScopeMasterKey)
	require.NoError(t, err)
	if !exists {
		return 0
	}
	return 1
}

// auditHas 报告该 action 是否已经写下操作行（预写 `pending` 也算写过）。
// auditHas reports whether any operation row carries that action.
func (f *instanceSecretsFixture) auditHas(t *testing.T, action string) bool {
	t.Helper()
	rows, err := f.ops.ListOperations(context.Background(), core.OperationFilter{Action: action, Limit: 20})
	require.NoError(t, err)
	return len(rows) > 0
}

// TestInstanceSecrets_NoRecordWithCiphertextStaysRecovery 覆盖 D-40（2026-10-27 裁决）：
// 无初始化记录 + 有托管密文 ⇒ `recovery_required` / `unprovable_provenance`，
// **密钥能解开也不 adopt**；`Ensure` / `Init` / `EnsureForManagedWrite` / `Adopt`
// 四个入口一律零写入（无记录、无操作行、无新密钥、密钥文件字节不变）。
//
// 覆盖的逆改点（每条对应一种真实事故）：
//   - `EvaluateSecrets` 在"密钥可解"时返回 adopt → decryptable 子例的状态 / 原因 /
//     动作 / `CanInitialize` 四条断言同时红（无主来源被采纳成原生身份）；
//   - `Ensure` 在守卫之后仍补写 → 记录数 0 → 1 且出现 `system.secrets_init`；
//   - `Init` / `EnsureForManagedWrite` / `Adopt` 未被恢复态拦住 → 三个拒绝断言红；
//   - 恢复态顺手生成替代密钥 → 密钥文件出现（missing_key 子例红）；
//   - 恢复态重写 / 替换盘上的密钥 → 字节比对红（present 子例红）。
//
// 文档原文（颗粒度文档 §4.2 R30）：三子例
// `{"decryptable_key", true, true}` / `{"undecryptable_key", true, false}` /
// `{"missing_key", false, false}`，注释「D-40 的核心：能解开也不再 adopt」。
func TestInstanceSecrets_NoRecordWithCiphertextStaysRecovery(t *testing.T) {
	target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
	require.NoError(t, err)

	cases := []struct {
		name     string
		present  bool
		verified bool
	}{
		{"decryptable_key", true, true}, // D-40 的核心：能解开也不再 adopt
		{"undecryptable_key", true, false},
		{"missing_key", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 判定表本体是纯函数：状态 / 原因 / 动作 / 能否初始化只由事实决定。
			got := EvaluateSecrets(SecretsFacts{
				Configured:          true,
				AutoInit:            true,
				KeyPresent:          tc.present,
				KeyID:               "key-1",
				ManagedCiphertexts:  3,
				CiphertextsVerified: tc.verified,
			})
			require.Equal(t, core.SecretStateRecoveryRequired, got.State)
			require.Equal(t, core.SecretRecoveryUnprovableProvenance, got.Reason)
			require.Equal(t, SecretsActionRecover, got.Action)
			require.False(t, got.CanInitialize())

			// 四条入口接在同一份事实上：有密文、无记录。
			ctx := context.Background()
			f := newInstanceSecretsFixture(t)
			f.seedSealedCredential(t, core.ResourceModelConnection, "model-default", target,
				"sk-test-representative")
			var keyBefore []byte
			if tc.present {
				require.NoError(t, prepareSecretDirectory(f.keyDir))
				keyID, keyBytes := f.key.KeyID, f.key.Key
				if !tc.verified {
					// 只换 key_id 不够：材料一起换，"解不开"才是真的。
					stranger := testMasterKey()
					stranger.KeyID = "k_stranger00000000000000001"
					for index := range stranger.Key {
						stranger.Key[index] = byte(255 - index)
					}
					keyID, keyBytes = stranger.KeyID, stranger.Key
				}
				writeMasterKeyFile(t, f.keyDir, keyID, keyBytes)
				before, readErr := os.ReadFile(f.keyPath)
				require.NoError(t, readErr)
				keyBefore = before
			}

			status, err := f.service.Ensure(ctx)
			require.NoError(t, err, "恢复态是状态返回，不是启动错误")
			require.Equal(t, core.SecretStateRecoveryRequired, status.Decision.State)
			require.Equal(t, 0, f.secretsRecordCount(t), "启动不得补写记录")
			require.False(t, f.auditHas(t, core.ActionSystemSecretsInit), "启动不得写初始化审计")
			if tc.present {
				after, readErr := os.ReadFile(f.keyPath)
				require.NoError(t, readErr)
				require.Equal(t, keyBefore, after, "恢复态不得重写或替换密钥文件")
			} else {
				_, statErr := os.Lstat(f.keyPath)
				require.ErrorIs(t, statErr, os.ErrNotExist, "恢复态不得生成新密钥")
			}

			require.Error(t, f.service.EnsureForManagedWrite(ctx, managedWriteActor()))
			_, err = f.service.Init(ctx, managedWriteActor())
			require.ErrorIs(t, err, core.ErrCredentialUnavailable)
			_, err = f.service.Adopt(ctx)
			require.Error(t, err)
			require.Equal(t, 0, f.secretsRecordCount(t), "拒绝路径一律零写入")
			require.Empty(t, f.secretsInitOps(t), "拒绝路径不得留下任何操作行")
		})
	}
}

// TestInstanceSecrets_TrustedRecordAndKeyReady 是 D-40 的误伤守卫：初始化记录在、
// 密钥与之一致（key_id / 指纹）、现存密文全可解时，判定必须是 `ready`——不得因为
// "实例里还有托管密文"就被推回恢复态（那会让合法实例升级后起不来）。启动既不补写
// 记录、不刷新记录时间、不新增初始化操作行，也不改写密钥文件。
//
// 覆盖的逆改点（每条对应一种真实事故）：
//   - D-40 的守卫漏掉 `facts.Record == nil` 前置（写成"有密文就恢复态"）→ 状态 /
//     原因 / 动作三条断言同时红（就绪实例被判恢复态，线上表现为升级后无法启动）；
//   - 就绪分支仍补写记录 / 把 `initialized_at` 刷新成启动时刻 → 记录数与时间断言红；
//   - 记录里的指纹与密钥不一致（或干脆不写）→ 指纹断言红（管理台的比对值不可信）；
//   - 就绪路径重写密钥文件 → 字节比对红。
//
// 文档原文（颗粒度文档 §4.2 R30 守卫收紧误伤案例 1）："有记录 + 匹配密钥 + 密文全可解
// 仍被拒 → 合法实例无法启动"，断言 `ready`、记录不改、`secrets verify` 全通过。
// `secrets verify` 的 CLI 层（VerifyCredentials）由 TestSecretsVerify_* 覆盖；本用例用
// 它的同一原语 verifySealedCredentials 判"全可解"，避免在批次间重复实现验证器。
// TestInstanceSecrets_TrustedRecordAndKeyReady keeps a trusted, ready instance ready:
// D-40 only applies to the record-less branch.
func TestInstanceSecrets_TrustedRecordAndKeyReady(t *testing.T) {
	ctx := context.Background()
	f := newInstanceSecretsFixture(t)

	// 可信三件套：可回溯的初始化操作行 + 与之对应的记录 + 盘上那把密钥。
	fingerprint, err := MasterKeyFingerprint(f.key)
	require.NoError(t, err)
	initializedAt := baselineClock().Add(-72 * time.Hour)
	opID := core.NewOperationID(initializedAt)
	require.NoError(t, f.ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       core.ActionSystemSecretsInit,
		TargetKind:   core.TargetKindSystem,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       core.AuthMethodMigration,
		Status:       core.OperationPending,
		CreatedAt:    initializedAt,
	}))
	require.NoError(t, f.ops.FinishOperation(ctx, opID,
		core.OperationSuccess, "", "", nil, initializedAt))
	writeMasterKeyFile(t, f.keyDir, f.key.KeyID, f.key.Key)
	require.NoError(t, f.store.Insert(ctx, postgres.InstanceSecretRecord{
		Scope:         core.InstanceSecretScopeMasterKey,
		KeyID:         f.key.KeyID,
		Fingerprint:   fingerprint,
		InitializedAt: initializedAt,
		OperationID:   opID,
	}))

	target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
	require.NoError(t, err)
	f.seedSealedCredential(t, core.ResourceModelConnection, "model-default", target,
		"sk-test-representative")
	keyBefore, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)

	status, err := f.service.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateReady, status.Decision.State, "D-40 只作用于无记录分支")
	require.Equal(t, core.SecretRecoveryNone, status.Decision.Reason)
	require.Equal(t, SecretsActionReady, status.Decision.Action)
	require.False(t, status.CanInitialize, "就绪实例不得允许再初始化")
	require.Equal(t, f.key.KeyID, status.KeyID)
	require.Equal(t, fingerprint, status.Fingerprint)
	require.Equal(t, 1, status.ManagedCiphertexts)
	require.Equal(t, 1, f.secretsRecordCount(t), "有记录分支不得补写")

	record := f.record(t)
	require.Equal(t, f.key.KeyID, record.KeyID)
	require.Equal(t, fingerprint, record.Fingerprint)
	require.Equal(t, initializedAt.UnixMilli(), record.InitializedAt.UnixMilli(),
		"记录时间不得被启动刷新")
	require.Equal(t, opID, record.OperationID, "记录来源不得被改写")

	rows := f.secretsInitOps(t)
	require.Len(t, rows, 1, "启动不得新增初始化操作行")
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, opID, rows[0].OperationID)

	after, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)
	require.Equal(t, keyBefore, after, "就绪路径不得重写密钥文件")

	// "密文全可解"用与 secrets verify 同一个原语判：解不开的条数必须为 0。
	sealed, err := f.store.SealedCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, sealed, 1)
	// 同一原语判"全可解"：adopt-legacy / 启动校验用的都是 verifySealedCredentials。
	sealedRows := make([]storage.SealedCredential, len(sealed))
	for index, item := range sealed {
		sealedRows[index] = storage.SealedCredential{
			CredentialID:  item.CredentialID,
			OwnerKind:     item.OwnerKind,
			OwnerUID:      item.OwnerUID,
			VersionNo:     item.VersionNo,
			KeyID:         item.KeyID,
			TargetBinding: item.TargetBinding,
			Nonce:         item.Nonce,
			Ciphertext:    item.Ciphertext,
		}
	}
	key, err := LoadMasterKey(f.keyPath, f.root)
	require.NoError(t, err)
	defer key.Zero()
	unreadable, first, err := verifySealedCredentials(key, sealedRows)
	require.NoError(t, err)
	require.Zero(t, unreadable, "全部密文必须可解，第一条失败：%s", first)
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
			Store:         postgres.NewInstanceSecretsStore(env.db),
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
		Store:         postgres.NewInstanceSecretsStore(env.db),
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

// TestFirstInstall_SecretsEligibility 判自动铸钥判定表的每一格（US-25 §2.3.3 /
// FR-25-03 / FR-25-04）：只有"无记录、无密钥、零密文、零历史、auto_init、
// （fresh 或 pending）"同时成立才 create；有历史 / 读历史失败 / 关闭 auto_init /
// 既非新实例也无 pending 一律 await_init。恢复态与就绪态不因新增字段改变。
// 逆改点：退回只看 FreshInstance → "有业务历史"行从 await_init 变 create；把
// RuntimeHistoryErr 当 false → "读历史失败"行变 create。
func TestFirstInstall_SecretsEligibility(t *testing.T) {
	historyErr := errors.New("业务历史读取失败")
	base := SecretsFacts{Configured: true, AutoInit: true, FreshInstance: true}
	with := func(mutate func(*SecretsFacts)) SecretsFacts {
		facts := base
		mutate(&facts)
		return facts
	}

	cases := []struct {
		name  string
		facts SecretsFacts
		want  SecretsAction
	}{
		{"全新实例且零历史", base, SecretsActionCreate},
		{"中断续做 pending 且零历史", with(func(f *SecretsFacts) {
			f.FreshInstance = false
			f.ProvisioningPending = true
		}), SecretsActionCreate},
		{"历史优先于 fresh 与 pending", with(func(f *SecretsFacts) {
			f.ProvisioningPending = true
			f.RuntimeHistory = true
		}), SecretsActionAwaitInit},
		{"读历史失败不得当成没有", with(func(f *SecretsFacts) {
			f.RuntimeHistoryErr = historyErr
		}), SecretsActionAwaitInit},
		{"auto_init 关闭", with(func(f *SecretsFacts) {
			f.AutoInit = false
		}), SecretsActionAwaitInit},
		{"既非新实例也无 pending", with(func(f *SecretsFacts) {
			f.FreshInstance = false
		}), SecretsActionAwaitInit},
		{"有托管密文仍是恢复态", with(func(f *SecretsFacts) {
			f.ManagedCiphertexts = 1
		}), SecretsActionRecover},
		{"记录与密钥齐全仍是就绪", with(func(f *SecretsFacts) {
			f.Record = &postgres.InstanceSecretRecord{KeyID: "mk_0123456789abcdef"}
			f.KeyPresent = true
			f.KeyID = "mk_0123456789abcdef"
		}), SecretsActionReady},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := EvaluateSecrets(tc.facts)
			require.Equal(t, tc.want, decision.Action)
			if tc.want == SecretsActionRecover {
				require.Equal(t, core.SecretStateRecoveryRequired, decision.State)
				require.Equal(t, core.SecretRecoveryUnprovableProvenance, decision.Reason)
			}
		})
	}
}

// TestFirstInstall_ExistingInstanceNeverMints 判"看起来全新的目录指向已有业务库"
// 与中断续做两条真实路径：
//   - 四张业务历史表任一有行时，即使 FreshInstance=true 也不得自动铸钥（否则会
//     在共享业务库上静默换钥匙）；删掉该行后同一路径才回到 create（正向对照）；
//   - `init` 已落名册与 pending、首次 serve 因缺入口限流退出后，重试以
//     FreshInstance=false + pending 续做并补发主密钥（FR-25-02）。
//
// 逆改点：判定表只看 FreshInstance → 第一个子例把密钥写出来；把 pending 从 create
// 条件里删掉 → 续做子例停在 uninitialized。
func TestFirstInstall_ExistingInstanceNeverMints(t *testing.T) {
	ctx := context.Background()
	seedLifecycleHistory := func(t *testing.T, f *instanceSecretsFixture) {
		t.Helper()
		_, err := f.db.ExecContext(ctx,
			`INSERT INTO agent_lifecycle (agent_name, updated_at) VALUES ('hist-agent', 1)`)
		require.NoError(t, err)
	}

	t.Run("已有业务历史的新目录不铸钥", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		seedLifecycleHistory(t, f)

		status, err := f.service.Ensure(ctx)
		require.NoError(t, err)
		require.Equal(t, core.SecretStateUninitialized, status.Decision.State)
		require.Equal(t, SecretsActionAwaitInit, status.Decision.Action)
		_, statErr := os.Lstat(f.keyPath)
		require.ErrorIs(t, statErr, os.ErrNotExist, "有业务历史时不得生成密钥")
		_, exists, err := f.store.Load(ctx, core.InstanceSecretScopeMasterKey)
		require.NoError(t, err)
		require.False(t, exists, "有业务历史时不得写初始化记录")
		require.Empty(t, f.secretsInitOps(t), "有业务历史时不得预写初始化审计")
	})

	t.Run("删除历史后同一路径恢复 create", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		seedLifecycleHistory(t, f)
		_, err := f.db.ExecContext(ctx, `DELETE FROM agent_lifecycle`)
		require.NoError(t, err)

		status, err := f.service.Ensure(ctx)
		require.NoError(t, err)
		require.Equal(t, core.SecretStateReady, status.Decision.State)
		require.FileExists(t, f.keyPath)
	})

	t.Run("init 的 pending 记录续做", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		workDir := t.TempDir()
		_, err := firstInstallAdapter{}.Prepare(workDir)
		require.NoError(t, err)

		service, err := NewInstanceSecretsService(InstanceSecretsDeps{
			Store:         f.store,
			Operations:    f.ops,
			MasterKeyFile: f.keyPath,
			WorkspaceRoot: f.root,
			WorkDir:       workDir,
			AutoInit:      true,
			FreshInstance: false, // 首次 serve 已退出：名册在、库未建
			Logger:        f.service.deps.Logger,
			Now:           baselineClock,
		})
		require.NoError(t, err)

		status, err := service.Ensure(ctx)
		require.NoError(t, err)
		require.Equal(t, core.SecretStateReady, status.Decision.State, "pending + 零历史必须续做")
		require.FileExists(t, f.keyPath)
	})
}

// TestFirstInstall_RecoveryNeverCreates 判恢复态不会因新增资格字段改变：有托管密文
// 但无记录无密钥、或有记录但密钥缺失时，Ensure 都不得生成替代密钥，也不得写记录。
// 逆改点：把零历史加进恢复分支的放行条件，或让 create 先于恢复判定 → 密钥文件出现。
func TestFirstInstall_RecoveryNeverCreates(t *testing.T) {
	ctx := context.Background()

	t.Run("有密文无记录无密钥仍是恢复态", func(t *testing.T) {
		target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
		require.NoError(t, err)
		f := newInstanceSecretsFixture(t)
		f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")

		status, err := f.service.Ensure(ctx)
		require.NoError(t, err)
		require.Equal(t, core.SecretStateRecoveryRequired, status.Decision.State)
		require.Equal(t, core.SecretRecoveryUnprovableProvenance, status.Decision.Reason)
		_, statErr := os.Lstat(f.keyPath)
		require.ErrorIs(t, statErr, os.ErrNotExist, "恢复态不得生成替代密钥")
		require.Empty(t, f.secretsInitOps(t))
	})

	t.Run("有记录缺密钥不生成第二把", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		fingerprint, err := MasterKeyFingerprint(f.key)
		require.NoError(t, err)
		existing := postgres.InstanceSecretRecord{
			Scope:         core.InstanceSecretScopeMasterKey,
			KeyID:         f.key.KeyID,
			Fingerprint:   fingerprint,
			InitializedAt: baselineClock(),
			OperationID:   "op-recovery-missing-key",
		}
		require.NoError(t, f.store.Insert(ctx, existing))

		status, err := f.service.Ensure(ctx)
		require.NoError(t, err)
		require.Equal(t, core.SecretStateRecoveryRequired, status.Decision.State)
		require.Equal(t, core.SecretRecoveryMissing, status.Decision.Reason)
		_, statErr := os.Lstat(f.keyPath)
		require.ErrorIs(t, statErr, os.ErrNotExist, "记录在而密钥缺失时不得生成第二把")
		require.Equal(t, existing.KeyID, f.record(t).KeyID, "既有记录不得被改写")
		require.Empty(t, f.secretsInitOps(t))
	})
}

// TestFirstInstall_KeyCreationAudited 判 create 的审计顺序（宪法第 5 条 / C-27）：
// 审计预写失败即拒绝生成（盘上零密钥、库里零记录）；成功路径恰好一条 success 行，
// 且记录的 operation_id 与审计行一致。
// 逆改点：把 beginSecretsInit 移到 generateAndPublish 之后 → 第一个子例留下密钥文件。
func TestFirstInstall_KeyCreationAudited(t *testing.T) {
	ctx := context.Background()

	t.Run("预写失败即拒绝铸钥", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		service, err := NewInstanceSecretsService(InstanceSecretsDeps{
			Store:         f.store,
			Operations:    us21FailingOperationStore{OperationStore: f.ops, err: errors.New("audit down")},
			MasterKeyFile: f.keyPath,
			WorkspaceRoot: f.root,
			AutoInit:      true,
			FreshInstance: true,
			Logger:        f.service.deps.Logger,
			Now:           baselineClock,
		})
		require.NoError(t, err)

		_, err = service.Ensure(ctx)
		require.Error(t, err, "审计预写失败必须拒绝生成密钥")
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		_, statErr := os.Lstat(f.keyPath)
		require.ErrorIs(t, statErr, os.ErrNotExist, "预写失败时不得留下密钥文件")
		_, exists, loadErr := f.store.Load(ctx, core.InstanceSecretScopeMasterKey)
		require.NoError(t, loadErr)
		require.False(t, exists, "预写失败时不得写初始化记录")
	})

	t.Run("成功路径留一条 success 且 operation_id 一致", func(t *testing.T) {
		f := newInstanceSecretsFixture(t)
		status, err := f.service.Ensure(ctx)
		require.NoError(t, err)
		require.Equal(t, core.SecretStateReady, status.Decision.State)

		ops := f.secretsInitOps(t)
		require.Len(t, ops, 1, "一次初始化只产生一条操作行")
		require.Equal(t, core.OperationSuccess, ops[0].Status)
		record := f.record(t)
		require.Equal(t, record.OperationID, ops[0].OperationID)
		require.Equal(t, record.KeyID, status.KeyID)
		require.JSONEq(t, `{"key_id":"`+status.KeyID+`","fingerprint":"`+status.Fingerprint+`","recovered":false}`,
			string(ops[0].DetailJSON))
	})
}
