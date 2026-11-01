//go:build integration

package app

// 本文件是「主密钥交接的状态联立」判卷（模块卡 R25 / C-38；T077 批 C 的 10 个具名用例）。
//
// 判据口径：记录 / 密钥 / 密文三者**必须同时成立**才算 ready；三者任一不成立都进
// recovery_required 并给出细分原因，且恢复态**不得**铸新钥、**不得**回落 env、
// **不得**产生新密文。夹具 `newHandoffHarness` 在临时目录建真实工作区 + 一把真实主密钥
// 文件，直接写 `instance_secrets` 记录（不经生产路径的自动初始化），密文用生产同一条
// AAD 构造（`CredentialTargetBinding` + `SealCredential`）封装、用生产的 `openManaged`
// 解开——夹具不自造 AAD，避免"测试自己变绿"。
//
// AAD 维度的**可观测边界**（2026-10-29 逆改探针实测）：模型连接在 `checkManagedState`
// 里已把 `version.TargetBinding` 钉成等于当前发布定义，因此"open 侧改用 `expectedTarget`
// 组 AAD"这类逆改在这组用例中**不可观测**（探针 p7 实测 PASS，属等价改动，不是用例
// 缺陷）；能区分两侧绑定来源的是非模型凭据（notify / MCP 槽位：`expectedTarget` 为空而
// 版本记录非空），该维度由校验类用例承担，不由本文件承担。
//
// 覆盖的**逆改点**（每条对应一种真实事故）：
//   - 只看 `instance_secrets` 行不校验密钥文件 → `MissingKeyRequiresRecovery` 照样启动；
//   - 密钥不匹配时静默生成新钥 → `WrongKeyDoesNotStart` 的记录被另一把密钥接管；
//   - 把「无记录」当成「新实例」直接初始化 → `CiphertextWithoutRecordRequiresRecovery`
//     把仍有密文的实例洗成新实例；
//   - 校验时打印明文 → `NoPlaintextInOutput` 输出含明文片段；
//   - 没有托管密文就跳过密钥校验 → `KeyHistoryOnlyStaysProtected` 在密钥被删后仍判 ready；
//   - 把「无主密钥」一律判 recovery_required → `EnvOnlyInstanceStaysUninitialized` 让纯 env
//     实例无法完成迁移；
//   - 托管可用就跳过 env 就绪检查 → `MixedSourcesVerifiedSeparately` 在缺变量时判通过；
//   - 恢复流程写入新密钥即宣布成功 → `RestoreOriginalKeyResumes` 旧密文解不开却进入 ready；
//   - 校验失败仍允许目标实例写库 → `TargetWriteBlockedUntilVerified` 产生新密文。
//
// 与模块卡代码草稿的三处字面差异（语义与断言不变，逐条留痕）：
//   1. 卡片写 `app.ErrRecoveryRequired`；本仓没有该符号——恢复态由
//      `core.ErrCredentialUnavailable` 包装 + 原因码表达（与 `secretsService.Status` 同一条），
//      因此断言两者都查：错误分类 + `recovery_required` 状态 + 原因码字面量。
//   2. 卡片用 `"key-1"` / `"key-2"` 作 `key_id`；主密钥文件要求 `k_` 前缀（`validMasterKeyID`），
//      改用 `k_key1` / `k_key2`，身份比较的判别力不变。
//   3. 卡片调 `h.verify(ctx)`；本夹具持有 ctx，签名收敛为 `h.verify(t)`。卡片里的
//      `sealRepresentativeWithOutRecord` 未单独建方法：`sealRepresentative` 只封装密文，
//      「有没有初始化记录」由用例是否先调 `seedRecordAndKey` 决定，两种写法判别力相同。
//
// These tests pin the master-key handoff triple (record + key file + ciphertext) on a real
// PostgreSQL instance: any one of the three missing or mismatched is recovery_required, and
// recovery never mints a replacement key, never falls back to env and never writes new
// ciphertext.

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

const (
	// handoffKeyIDPrimary / handoffKeyIDOther 是两把**合法但不同**的主密钥身份：`k_` 前缀
	// 是主密钥文件的解析前提，换别的写法在加载层就会被拒，测不到判定表。
	handoffKeyIDPrimary = "k_key1"
	handoffKeyIDOther   = "k_key2"
	// handoffRepresentative 是代表性托管模型连接名；handoffPlaintext 是它的明文材料。
	handoffRepresentative = "model-default"
	handoffPlaintext      = "sk-test-representative"
	// handoffEnvVar 是 env 模式连接的变量名（取值只在用例内设置，不落盘）。
	handoffEnvVar = "BASALT_HANDOFF_ENV_TOKEN"
)

// handoffReport 是一次交接校验的读数：三态、密钥身份与逐条校验计数。
// handoffReport is the outcome of one handoff verification.
type handoffReport struct {
	Status InstanceSecretsStatus
	Verify SecretsVerifyReport
	KeyID  string
	// DecryptedCount = Checked − Failed：真的解开了几条。计数不能替代解密，
	// 卡片要求「代表性凭据必须真的解开」，因此单独给出。
	DecryptedCount int
}

// handoffHarness 是主密钥交接夹具：临时工作区 + 临时 PG + 一把真实主密钥文件。
// 主密钥写在**工作区外**（`LoadMasterKey` 的边界要求），记录直接落库、不经自动初始化。
// handoffHarness wires the handoff fixture: workspace, database and an out-of-workspace
// master key file; the instance record is inserted directly, not through auto-init.
type handoffHarness struct {
	*resolverFixture

	ctx      context.Context
	workDir  string
	keyDir   string
	keyFile  string
	locator  *MasterKeyLocator
	store    *postgres.InstanceSecretsStore
	instance *InstanceSecretsService
	secrets  SecretsService
	writer   *CredentialOps
	log      *slog.Logger
	logs     *strings.Builder

	primaryKey MasterKey
	otherKey   MasterKey

	// representative 记录最近一次 sealRepresentative 的连接名：`fellBackToEnv` 用它
	// 证明恢复态下解析器没有偷偷改走环境变量。
	representative string
}

// newHandoffHarness 建夹具：一把 `k_key1` 主密钥文件已落盘，路径与解析 / 写入 / 实例
// 服务共享同一个 locator（与生产装配同形）。
// newHandoffHarness builds the fixture with the primary key already published on disk.
func newHandoffHarness(t *testing.T) *handoffHarness {
	t.Helper()
	h := assembleHandoffHarness(t)
	primary := handoffKey(handoffKeyIDPrimary, 0x11)
	other := handoffKey(handoffKeyIDOther, 0x22)
	t.Cleanup(func() {
		primary.Zero()
		other.Zero()
	})
	h.primaryKey = primary
	h.otherKey = other
	h.keyFile = writeMasterKeyFile(t, h.keyDir, primary.KeyID, primary.Key)
	h.wire(t, h.keyFile)
	return h
}

// newEnvOnlyHandoffHarness 建**纯 env** 实例夹具：没有主密钥路径、没有初始化记录、
// 没有托管密文——三态必须判 `uninitialized`，而不是把「无主密钥」当成故障。
// newEnvOnlyHandoffHarness models an env-only instance that must stay uninitialized.
func newEnvOnlyHandoffHarness(t *testing.T) *handoffHarness {
	t.Helper()
	h := assembleHandoffHarness(t)
	h.wire(t, "")
	return h
}

// assembleHandoffHarness 建公共部分：工作区 / PG / 日志 / 密钥目录。装配动作在
// `wire` 里做——纯 env 变体与常规变体的唯一差别是主密钥路径。
// assembleHandoffHarness builds everything that does not depend on the key path.
func assembleHandoffHarness(t *testing.T) *handoffHarness {
	t.Helper()
	env := newResolverFixture(t)
	logs := &strings.Builder{}
	return &handoffHarness{
		resolverFixture: env,
		ctx:             context.Background(),
		workDir:         t.TempDir(),
		keyDir:          t.TempDir(),
		store:           postgres.NewInstanceSecretsStore(env.db),
		logs:            logs,
		log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}
}

// wire 按生产依赖形状装配四个组件；`keyPath` 为空串即纯 env 部署（不建立任何路径）。
// wire assembles the four credential components over one shared locator.
func (h *handoffHarness) wire(t *testing.T, keyPath string) {
	t.Helper()
	h.locator = NewMasterKeyLocator(keyPath)
	instance, err := NewInstanceSecretsService(InstanceSecretsDeps{
		Store:      h.store,
		Operations: h.ops,
		// MasterKeyFile 留空：locator 是权威路径（生产过程也是这一条）。
		KeyPath:       h.locator,
		WorkDir:       h.workDir,
		WorkspaceRoot: h.root,
		AutoInit:      true,
		// 交接目标是**存量实例**：不允许在缺名册时就地建立身份（H1）。
		FreshInstance: false,
		Logger:        h.log,
		Now:           baselineClock,
	})
	require.NoError(t, err)
	resolver, err := newCredentialResolver(CredentialResolverDeps{
		Credentials:   h.credentials,
		Resources:     h.versions,
		Notify:        h.admission.NotifyAdmin,
		Bindings:      postgres.NewBindingStore(h.db),
		KeyPath:       h.locator,
		WorkspaceRoot: h.root,
		// Logger 与生产装配同形：不给它，「校验过程不得输出明文」就只覆盖
		// secretsService 一层，解析器自己的日志完全不在判卷面内（2026-10-29
		// 探针 p10 首次跑绿即由此暴露）。
		Logger: h.log,
	})
	require.NoError(t, err)
	writer, err := NewCredentialOps(CredentialOpsDeps{
		Operations:    h.ops,
		Credentials:   h.credentials,
		Resources:     h.versions,
		Notify:        h.admission.NotifyAdmin,
		Instance:      instance,
		KeyPath:       h.locator,
		WorkspaceRoot: h.root,
		Logger:        h.log,
		Now:           baselineClock,
	})
	require.NoError(t, err)
	secretService, err := NewSecretsService(SecretsServiceDeps{
		Credentials:   h.credentials,
		Channels:      postgres.NewNotifyChannelRepo(h.db),
		Ops:           writer,
		Resolver:      resolver,
		Instance:      instance,
		KeyPath:       h.locator,
		WorkspaceRoot: h.root,
		Resources:     h.versions,
		Logger:        h.log,
	})
	require.NoError(t, err)
	h.instance = instance
	h.resolver = resolver
	h.writer = writer
	h.secrets = secretService
}

// handoffKey 返回一把固定内容的测试密钥（32 字节，内容可复现；非生产用法）。
// handoffKey returns a fixed-content 32-byte test key; never production material.
func handoffKey(keyID string, seed byte) MasterKey {
	raw := make([]byte, masterKeyBytes)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return MasterKey{KeyID: keyID, Key: raw}
}

// keyFor 把 `key_id` 映射回夹具持有的密钥材料；未知身份直接失败。
// keyFor resolves a test key id back to its material.
func (h *handoffHarness) keyFor(t *testing.T, keyID string) MasterKey {
	t.Helper()
	switch keyID {
	case h.primaryKey.KeyID:
		return h.primaryKey
	case h.otherKey.KeyID:
		return h.otherKey
	default:
		t.Fatalf("未知测试主密钥身份 %q", keyID)
		return MasterKey{}
	}
}

// seedKeyFile 在固定路径写一份主密钥（重复调用即替换）；路径不变，重启语义一致。
// seedKeyFile writes (or replaces) the master key at the fixture's one stable path.
func (h *handoffHarness) seedKeyFile(t *testing.T, keyID string) {
	t.Helper()
	key := h.keyFor(t, keyID)
	h.keyFile = writeMasterKeyFile(t, h.keyDir, key.KeyID, key.Key)
}

// replaceKeyFile 是「换了另一把合法密钥」的显式说法：路径相同、内容与 key_id 不同。
// replaceKeyFile swaps in another valid key at the same path.
func (h *handoffHarness) replaceKeyFile(t *testing.T, keyID string) {
	t.Helper()
	h.seedKeyFile(t, keyID)
}

// removeKeyFile 模拟「路径已解析、文件被删」：判定必须是 `missing`，不是 `key_path_missing`。
// removeKeyFile simulates a resolved path whose file is gone.
func (h *handoffHarness) removeKeyFile(t *testing.T) {
	t.Helper()
	require.NoError(t, os.Remove(h.keyFile))
}

// seedRecordAndKey 直接写入初始化记录与匹配的密钥文件——**不经**生产路径的自动初始化，
// 因此判定表看到的是「历史实例」而不是「本次新实例」。
// seedRecordAndKey writes the record and the matching key file directly.
func (h *handoffHarness) seedRecordAndKey(t *testing.T, keyID string) {
	t.Helper()
	key := h.keyFor(t, keyID)
	h.seedKeyFile(t, keyID)
	fingerprint, err := MasterKeyFingerprint(key)
	require.NoError(t, err)
	require.NoError(t, h.store.Insert(h.ctx, postgres.InstanceSecretRecord{
		Scope:         core.InstanceSecretScopeMasterKey,
		KeyID:         key.KeyID,
		Fingerprint:   fingerprint,
		InitializedAt: baselineClock(),
		OperationID:   h.opID,
	}))
}

// sealRepresentative 发布一条托管模型连接并用**主密钥**封装它的凭据。
// sealRepresentative publishes one managed connection and seals its credential.
func (h *handoffHarness) sealRepresentative(t *testing.T, name, plaintext string) string {
	t.Helper()
	return h.sealRepresentativeUnder(t, h.primaryKey, h.primaryKey.KeyID, name, plaintext)
}

// sealRepresentativeUnder 允许「用另一把密钥封装、却按给定 key_id 记账」：这正是
// 「记录与密文对不上」的最小构造（密钥身份一致但材料不同 → 只有真解密才能发现）。
// sealRepresentativeUnder seals with one key while recording another key id.
func (h *handoffHarness) sealRepresentativeUnder(t *testing.T,
	sealKey MasterKey, recordedKeyID, name, plaintext string,
) string {
	t.Helper()
	baseURL := "https://" + name + ".example.com/v1"
	version := h.seedVersion(t, core.ResourceModelConnection, name,
		managedProviderDefinition(name, baseURL, "alpha-model"), "", "")
	h.publish(t, core.ResourceModelConnection, name, version.ID, true)

	binding, ok := credentialBindingKind(core.ResourceModelConnection)
	require.True(t, ok)
	uid := fixtureResourceUID(t, core.ResourceModelConnection, name)
	credID := fixtureCredentialID(t, string(core.ResourceModelConnection)+"/"+name)
	// 与 verifyIdentity / openManaged 同一条 AAD（目标绑定由当前发布定义重算）：
	// 生产侧改口径，这里就会解不开。
	target, err := CredentialTargetBinding(binding, baseURL, "")
	require.NoError(t, err)
	aad := CredentialAAD{
		OwnerKind:     binding,
		OwnerUID:      uid,
		CredentialID:  credID,
		VersionNo:     1,
		TargetBinding: target,
	}
	nonce, ciphertext, err := SealCredential(sealKey, aad, []byte(plaintext))
	require.NoError(t, err)
	require.NoError(t, h.credentials.Create(h.ctx, core.CredentialRecord{
		ID:          credID,
		OwnerKind:   binding,
		OwnerUID:    uid,
		OwnerName:   name,
		KeyID:       recordedKeyID,
		OperationID: h.opID,
		CreatedAt:   baselineClock().UnixMilli(),
	}, core.CredentialVersionRecord{
		KeyID:         recordedKeyID,
		TargetBinding: target,
		Last4:         last4Of(plaintext),
		Nonce:         nonce,
		Ciphertext:    ciphertext,
		OperationID:   h.opID,
	}))
	h.representative = name
	return credID
}

// verify 走启动路径（`Ensure`）→ 三态 → 逐条托管校验，返回交接报告。
// 只有**恢复态**返回 `core.ErrCredentialUnavailable` 包装错误（见文件头差异 1）：
// `uninitialized` 是合法状态（纯 env 实例），不是故障。恢复态**不**铸钥、**不**回落
// env，调用方据原因码分辨细分原因。
// verify runs the startup path, then the per-credential check, and reports both. Only
// recovery_required is an error; uninitialized is a legal state.
func (h *handoffHarness) verify(t *testing.T) (handoffReport, error) {
	t.Helper()
	status, err := h.instance.Ensure(h.ctx)
	if err != nil {
		return handoffReport{}, err
	}
	report := handoffReport{Status: status, KeyID: status.KeyID}
	if status.Decision.State == core.SecretStateRecoveryRequired {
		return report, fmt.Errorf("%w: 主密钥处于恢复态（原因 %q）——不生成替代密钥、不回落 env",
			core.ErrCredentialUnavailable, status.Decision.Reason)
	}
	verified, err := h.secrets.Verify(h.ctx)
	if err != nil {
		return report, err
	}
	report.Verify = verified
	report.DecryptedCount = verified.Checked - verified.Failed
	return report, nil
}

// reasonCode 重新读一次三态（只读），返回细分原因；判定表是纯函数，重读稳定。
// reasonCode re-reads the read-only verdict and returns the recovery reason.
func (h *handoffHarness) reasonCode(t *testing.T) core.SecretRecoveryReason {
	t.Helper()
	status, err := h.instance.Status(h.ctx)
	require.NoError(t, err)
	return status.Decision.Reason
}

// generatedNewKey 报告校验之后主密钥文件是否存在。缺失场景里文件是被删的，
// 「出现」就只能来自生成；纯 env 实例没有路径，另由 `locator.Path()` 断言。
// generatedNewKey reports whether a key file exists after verification.
func (h *handoffHarness) generatedNewKey(t *testing.T) bool {
	t.Helper()
	if h.keyFile == "" {
		return false
	}
	_, err := os.Stat(h.keyFile)
	return err == nil
}

// fellBackToEnv 报告解析器是否「静默改走环境变量」把代表性连接解开了：恢复态下
// 正常实现只会返回不可用错误，成功即等于发生回落。
// fellBackToEnv reports whether resolving silently succeeded instead of failing closed.
func (h *handoffHarness) fellBackToEnv(t *testing.T) bool {
	t.Helper()
	if h.representative == "" {
		return false
	}
	_, err := h.resolver.ResolveModelCredential(h.ctx, h.refFor(t, h.representative))
	return err == nil
}

// refFor 按当前发布指针构造连接引用（与生产准入同一条取数路径）。
// refFor builds the version ref from the current published pointer.
func (h *handoffHarness) refFor(t *testing.T, name string) core.ResourceVersionRef {
	t.Helper()
	pointer, err := h.versions.Pointer(h.ctx, core.ResourceModelConnection, name)
	require.NoError(t, err)
	version, err := h.versions.VersionByID(h.ctx, pointer.PublishedVersionID)
	require.NoError(t, err)
	return core.ResourceVersionRef{
		Name:        pointer.ResourceID,
		ResourceUID: pointer.ResourceUID,
		VersionID:   version.ID,
		VersionNo:   version.VersionNo,
		ContentHash: version.ContentHash,
	}
}

// recordCount / recordKeyID 读初始化记录（0 / 1 与身份）；恢复态必须保持原样。
// recordCount and recordKeyID read the initialization record.
func (h *handoffHarness) recordCount(t *testing.T) int {
	t.Helper()
	_, exists, err := h.store.Load(h.ctx, core.InstanceSecretScopeMasterKey)
	require.NoError(t, err)
	if !exists {
		return 0
	}
	return 1
}

func (h *handoffHarness) recordKeyID(t *testing.T) string {
	t.Helper()
	record, exists, err := h.store.Load(h.ctx, core.InstanceSecretScopeMasterKey)
	require.NoError(t, err)
	require.True(t, exists, "初始化记录应当存在")
	return record.KeyID
}

// sealedCount 统计现存托管密文版本数（撤销后密文为 NULL，不计入）。
// sealedCount counts every version row that still carries ciphertext.
func (h *handoffHarness) sealedCount(t *testing.T) int {
	t.Helper()
	count, err := h.store.CountManagedCiphertext(h.ctx)
	require.NoError(t, err)
	return count
}

// output 是本次用例捕获到的全部日志与审计面文案——「校验过程不得输出明文」的判卷面。
// output is everything the components logged during the test.
func (h *handoffHarness) output() string { return h.logs.String() }

// TestSecretsHandoff_RecordAndKeyMatch —— 记录、密钥、密文三者同时成立才算 ready。
// 逆改点：把「记录存在」当成就绪（不比对 key_id、不试解密文）→ DecryptedCount 断言红。
func TestSecretsHandoff_RecordAndKeyMatch(t *testing.T) {
	h := newHandoffHarness(t)
	h.seedRecordAndKey(t, handoffKeyIDPrimary)
	h.sealRepresentative(t, handoffRepresentative, handoffPlaintext)

	rep, err := h.verify(t)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateReady, rep.Status.Decision.State)
	require.Equal(t, handoffKeyIDPrimary, rep.KeyID)
	require.Equal(t, 1, rep.Verify.Checked)
	require.Equal(t, 0, rep.Verify.Failed)
	require.Equal(t, 1, rep.DecryptedCount, "代表性凭据必须真的解开")
	require.NotContains(t, h.output(), handoffPlaintext, "校验过程不得输出明文")
}

// TestSecretsHandoff_MissingKeyRequiresRecovery —— 路径已解析、文件被删 → `missing`。
// 逆改点：只看 instance_secrets 行不校验密钥文件 → 本用例照样启动（NoError）。
func TestSecretsHandoff_MissingKeyRequiresRecovery(t *testing.T) {
	h := newHandoffHarness(t)
	h.seedRecordAndKey(t, handoffKeyIDPrimary)
	h.sealRepresentative(t, handoffRepresentative, handoffPlaintext)
	h.removeKeyFile(t)

	rep, err := h.verify(t)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Equal(t, core.SecretStateRecoveryRequired, rep.Status.Decision.State)
	require.Equal(t, "missing", string(h.reasonCode(t)),
		"路径已解析但文件被删 → missing；key_path_missing 只用于有记录且路径未解析")
	require.False(t, h.generatedNewKey(t), "缺失时不得自动生成新主密钥")
	require.False(t, h.fellBackToEnv(t), "缺失时不得静默切到环境变量")
	require.Equal(t, 1, h.recordCount(t), "恢复态不得新增或改写初始化记录")
	require.Equal(t, handoffKeyIDPrimary, h.recordKeyID(t))
	require.Equal(t, 1, h.sealedCount(t))
}

// TestSecretsHandoff_WrongKeyDoesNotStart —— 另一把合法密钥顶替原钥 → `key_id_mismatch`。
// 逆改点：密钥不匹配时静默生成新钥 → 记录 key_id 变了、旧密文全废（两条断言同时红）。
func TestSecretsHandoff_WrongKeyDoesNotStart(t *testing.T) {
	h := newHandoffHarness(t)
	h.seedRecordAndKey(t, handoffKeyIDPrimary)
	h.sealRepresentative(t, handoffRepresentative, handoffPlaintext)
	h.replaceKeyFile(t, handoffKeyIDOther)

	rep, err := h.verify(t)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Equal(t, core.SecretStateRecoveryRequired, rep.Status.Decision.State)
	require.Equal(t, "key_id_mismatch", string(h.reasonCode(t)))
	require.Equal(t, handoffKeyIDPrimary, h.recordKeyID(t), "记录不得被另一把密钥接管")
	require.Equal(t, 1, h.sealedCount(t), "密文不得被删除或重封装")
}

// TestSecretsHandoff_CiphertextWithoutRecordRequiresRecovery —— 有密文、无记录
// → `unprovable_provenance`，**即便当前密钥能解开**（D-40）。
// 逆改点：把「无记录」当成「新实例」直接初始化 → 记录被凭空补上、实例被洗成新实例。
func TestSecretsHandoff_CiphertextWithoutRecordRequiresRecovery(t *testing.T) {
	h := newHandoffHarness(t)
	h.sealRepresentative(t, handoffRepresentative, handoffPlaintext)
	h.seedKeyFile(t, handoffKeyIDPrimary)

	rep, err := h.verify(t)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Equal(t, core.SecretStateRecoveryRequired, rep.Status.Decision.State)
	require.Equal(t, "unprovable_provenance", string(h.reasonCode(t)),
		"有密文却说不出密钥来历，不得当新实例")
	require.Equal(t, 0, h.recordCount(t), "能解开也不得补写初始化记录（来源不可证）")
	require.Equal(t, 1, h.sealedCount(t))
}

// TestSecretsHandoff_NoPlaintextInOutput —— 校验失败路径的日志 / 错误串不得带材料。
// 逆改点：校验时打印明文或密钥片段 → 输出断言红；失败不留 WARN → 可观测性断言红。
func TestSecretsHandoff_NoPlaintextInOutput(t *testing.T) {
	h := newHandoffHarness(t)
	h.seedRecordAndKey(t, handoffKeyIDPrimary)
	h.sealRepresentative(t, "model-good", handoffPlaintext)
	// 用另一把密钥封装、却按 `k_key1` 记账：身份校验过得去，只有真解密才发现问题。
	h.sealRepresentativeUnder(t, h.otherKey, h.primaryKey.KeyID, "model-tampered", "sk-test-second")

	rep, err := h.verify(t)
	require.NoError(t, err, "记录与密钥一致，实例状态本身仍就绪；失败只体现在逐条校验")
	require.Equal(t, 2, rep.Verify.Checked)
	require.Equal(t, 1, rep.Verify.Failed)
	require.Equal(t, 1, rep.DecryptedCount)

	out := h.output()
	require.Contains(t, out, "secrets verify", "失败路径必须留下 WARN 日志（可观测性双轨）")
	require.NotContains(t, out, handoffPlaintext)
	require.NotContains(t, out, "sk-test-second")
	// 密钥片段：base64 前 16 位足够定位泄露面，又不至于把整把钥匙写进断言消息。
	keyFragment := base64.StdEncoding.EncodeToString(h.primaryKey.Key)[:16]
	require.NotContains(t, out, keyFragment, "密钥片段不得出现在任何日志里")

	_, resolveErr := h.resolver.ResolveModelCredential(h.ctx, h.refFor(t, "model-tampered"))
	require.Error(t, resolveErr)
	require.NotContains(t, resolveErr.Error(), "sk-test-second")
	require.NotContains(t, resolveErr.Error(), keyFragment)
}

// TestSecretsHandoff_KeyHistoryOnlyStaysProtected —— 没有托管密文不等于可以跳过密钥校验。
// 逆改点：`ManagedCiphertexts == 0` 就认为「无需校验」→ 密钥被删仍判 ready。
func TestSecretsHandoff_KeyHistoryOnlyStaysProtected(t *testing.T) {
	h := newHandoffHarness(t)
	h.seedRecordAndKey(t, handoffKeyIDPrimary)

	rep, err := h.verify(t)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateReady, rep.Status.Decision.State)
	require.Equal(t, 0, rep.Verify.Checked, "没有托管密文时逐条校验没有对象")

	h.removeKeyFile(t)
	rep, err = h.verify(t)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Equal(t, core.SecretStateRecoveryRequired, rep.Status.Decision.State)
	require.Equal(t, "missing", string(h.reasonCode(t)), "有密钥历史的实例密钥被删必须报恢复态")
	require.False(t, h.generatedNewKey(t))
}

// TestSecretsHandoff_EnvOnlyInstanceStaysUninitialized —— 纯 env 实例三态是
// `uninitialized`（动作 `await_init`），不是恢复态——否则迁移无法完成。
// 逆改点：把「无主密钥」一律判 recovery_required → 状态与动作断言同时红。
func TestSecretsHandoff_EnvOnlyInstanceStaysUninitialized(t *testing.T) {
	h := newEnvOnlyHandoffHarness(t)

	rep, err := h.verify(t)
	require.NoError(t, err, "纯 env 实例不是故障：校验不得报恢复态")
	require.False(t, rep.Status.Configured)
	require.Equal(t, core.SecretStateUninitialized, rep.Status.Decision.State)
	require.Equal(t, SecretsActionAwaitInit, rep.Status.Decision.Action)
	require.Equal(t, 0, rep.Verify.Checked)
	require.Equal(t, 0, h.recordCount(t))
	require.Empty(t, h.locator.Path(), "纯 env 实例不得因为一次校验就被赋予密钥路径")
}

// TestSecretsHandoff_MixedSourcesVerifiedSeparately —— 托管可用不得替代 env 就绪检查
// （L3 口径：托管可解密 + env 变量名就绪**分开**验证，缺项逐条列出）。
// 逆改点：解析出托管 Key 就跳过 env 分支 → 第一次构目录不报错，缺变量的目标机被判通过。
func TestSecretsHandoff_MixedSourcesVerifiedSeparately(t *testing.T) {
	h := newHandoffHarness(t)
	h.seedRecordAndKey(t, handoffKeyIDPrimary)
	h.sealRepresentative(t, "model-managed", handoffPlaintext)

	envBaseURL := "https://model-env.example.com/v1"
	envVersion := h.seedVersion(t, core.ResourceModelConnection, "model-env",
		providerDefinition("model-env", envBaseURL, "${"+handoffEnvVar+"}", "alpha-model"), "", "")
	h.publish(t, core.ResourceModelConnection, "model-env", envVersion.ID, true)

	// 变量缺失（空值与未设置同档拒绝）：托管侧明明可用，构建仍必须失败并点名缺项。
	t.Setenv(handoffEnvVar, "")
	_, err := newProviderCatalogFromPublished(h.ctx, h.versions, nil, h.log, h.resolver)
	require.Error(t, err, "托管可用不得替代 env 就绪检查")
	require.Contains(t, err.Error(), handoffEnvVar, "缺项必须点名变量名")
	require.Contains(t, err.Error(), "model-env")
	require.NotContains(t, err.Error(), "model-managed", "托管连接不应被误报为缺项")

	// 变量就绪后两者各走各的路径，目录才建成。
	t.Setenv(handoffEnvVar, "sk-env-handoff-value")
	catalog, err := newProviderCatalogFromPublished(h.ctx, h.versions, nil, h.log, h.resolver)
	require.NoError(t, err)
	require.Len(t, catalog.clients, 2)

	rep, err := h.verify(t)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Verify.Checked, "逐条校验只覆盖托管凭据")
	require.Equal(t, 0, rep.Verify.Failed)
}

// TestSecretsHandoff_RestoreOriginalKeyResumes —— 把原钥匙放回去即恢复；「换一把新钥
// 就当恢复成功」不成立。
// 逆改点：恢复流程写入新密钥即宣布成功 → 第一次 verify 就会 NoError（旧密文解不开却 ready）。
func TestSecretsHandoff_RestoreOriginalKeyResumes(t *testing.T) {
	h := newHandoffHarness(t)
	h.seedRecordAndKey(t, handoffKeyIDPrimary)
	h.sealRepresentative(t, handoffRepresentative, handoffPlaintext)

	h.replaceKeyFile(t, handoffKeyIDOther)
	_, err := h.verify(t)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Equal(t, "key_id_mismatch", string(h.reasonCode(t)))
	require.Equal(t, 1, h.sealedCount(t), "恢复流程不得删改既有密文")

	h.seedKeyFile(t, handoffKeyIDPrimary)
	rep, err := h.verify(t)
	require.NoError(t, err)
	require.Equal(t, handoffKeyIDPrimary, rep.KeyID)
	require.Equal(t, 1, rep.DecryptedCount, "原钥匙回位后旧密文必须真的解得开")
}

// TestSecretsHandoff_TargetWriteBlockedUntilVerified —— 校验失败的目标实例不得写入：
// 首次受控保存的前置闸门直接拒绝，库内密文与记录都保持原样。
// 逆改点：恢复态仍放行 `PrepareManagedWrite` → 半可用实例产生新密文（计数断言红）。
func TestSecretsHandoff_TargetWriteBlockedUntilVerified(t *testing.T) {
	h := newHandoffHarness(t)
	h.seedRecordAndKey(t, handoffKeyIDPrimary)
	h.sealRepresentative(t, handoffRepresentative, handoffPlaintext)
	before := h.sealedCount(t)

	h.replaceKeyFile(t, handoffKeyIDOther)
	writeErr := h.writer.PrepareManagedWrite(h.ctx, core.Actor{})
	require.ErrorIs(t, writeErr, core.ErrCredentialUnavailable)
	require.Equal(t, before, h.sealedCount(t), "校验失败不得产生新密文")
	require.Equal(t, 1, h.recordCount(t))
	require.Equal(t, handoffKeyIDPrimary, h.recordKeyID(t))
	require.NotContains(t, writeErr.Error(), handoffPlaintext)

	h.seedKeyFile(t, handoffKeyIDPrimary)
	require.NoError(t, h.writer.PrepareManagedWrite(h.ctx, core.Actor{}),
		"原钥匙回位后受控写入必须放行")
}
