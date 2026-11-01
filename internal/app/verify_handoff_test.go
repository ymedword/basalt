package app

// 本文件判 `db verify --handoff <dir>`（手写覆盖率见颗粒度文档 §3.1.2 / §3.1.8，
// check `us16-handoff`）：`<dir>` 是迁移准备副本的**工作区根**，同级 `basalt.yaml`
// 是配置，工作区外是主密钥文件。夹具按副本应有的事实造：名册、记录（来源
// `instance.secrets_legacy_adopt`）、真封密文、`instance.adopt` 审计里的
// `target_config_sha256`、当前发布定义——没有一项靠"看起来像"通过。
//
// 覆盖的**逆改点**（每条对应一种真实事故）：
//   - 撤销的版本（密文已清空）被算进失败数 → 正常撤销的噪声淹没有效告警；
//   - 仍有密文的撤销版本被当成正常撤销 → C-38 的真异常被静默放过；
//   - `active` 却缺密文的版本被跳过不计数 → 凭据不可用却报交接就绪；
//   - 统一实现只解密密文、不核对 `expectedTarget` / `target_binding` → (a) 全绿而
//     (b) 指向旧地址；
//   - 普通存量实例被套用迁移准备口径（或反过来）→ 要么错拒，要么谎称核对过配置；
//   - 报告 / WARN 日志带出明文或密钥材料 → 交接记录本身变成泄密面。
//
// These tests pin the offline handoff gate end to end on a real v6 SQLite copy:
// every refusal carries a stable code and a recovery hint, and no readable surface
// ever carries credential material.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

const (
	// handoffInstanceID 是夹具名册里的实例标识（`inst_` + 16 位十六进制）。
	handoffInstanceID = "inst_fedcba9876543210"
	// handoffBaseURL 是托管模型连接的目标地址：`target_binding` 由它重算。
	handoffBaseURL = "https://api.example.com/v1"
	// handoffCopyPlaintext 是被封进密文的合成明文（任何报告 / 日志里都不许出现）。
	handoffCopyPlaintext = "sk-handoff-verify-plaintext-9z8y"
	// handoffConnectionID 是发布定义的名称快照（仅展示与检索）。
	handoffConnectionID = "handoff-openai"
	// 三条操作行：种子（init，凭据外键锚点）、记录来源（legacy adopt）、采纳审计。
	handoffSeedOperationID        = "op-handoff-e2e-seed-1"
	handoffLegacyAdoptOperationID = "op-handoff-e2e-legacy-adopt-1"
	handoffAdoptOperationID       = "op-handoff-e2e-adopt-1"
)

// handoffCredentialID / handoffConnectionUID 是领域校验过的固定字面量形态
// （`cred_` / `rs_` + 26 位 Crockford Base32）。
var (
	handoffCredentialID  = fmt.Sprintf("cred_%026d", 700)
	handoffConnectionUID = core.ResourceUID(fmt.Sprintf("rs_%026d", 700))
)

// handoffParams 是夹具变形旋钮；零值即"健康的迁移准备副本"。
// handoffParams dials the fixture; the zero value is the healthy migration copy.
type handoffParams struct {
	// NormalInstance 为真时记录指向 `system.secrets_init`（普通存量实例），且不写
	// `instance.adopt` 审计——这类实例没有 `target_config_sha256` 基线。
	NormalInstance bool
	// SkipAdoptAudit 仍按迁移准备模式建记录，但不写 `instance.adopt` 审计行。
	SkipAdoptAudit bool
	// PublishBaseURL 覆盖发布定义的 `base_url`（默认 handoffBaseURL）。
	PublishBaseURL string
	// KeyPathInConfig 覆盖写进配置的密钥路径（默认真实密钥路径）。
	KeyPathInConfig string
	// ConfigBody 覆盖整份配置原文（默认按 KeyPathInConfig 渲染）。
	ConfigBody string
}

// handoffFixture 是一座按 §3.1.2 造出来的迁移准备副本。
// handoffFixture is one migration-preparation copy laid out as §3.1.2 requires.
type handoffFixture struct {
	t          *testing.T
	root       string
	ws         string
	dbPath     string
	configPath string
	key        MasterKey
	keyPath    string
	configRaw  []byte
}

// newHandoffFixture 建 `<copy-root>/.basalt` + 同级 `basalt.yaml` + 工作区外密钥 +
// 真 v6 库（记录 / 密文 / 发布定义 / 三条审计行）。
// newHandoffFixture builds the workspace, the sibling config, the external key file
// and a genuine v6 database carrying record, ciphertext, published definition and
// the two audits the gate reads.
func newHandoffFixture(t *testing.T, mutate ...func(*handoffParams)) *handoffFixture {
	t.Helper()
	params := handoffParams{PublishBaseURL: handoffBaseURL}
	if len(mutate) > 0 && mutate[0] != nil {
		mutate[0](&params)
	}

	root := t.TempDir()
	ws := filepath.Join(root, workspaceDirName)
	require.NoError(t, os.MkdirAll(ws, 0o700))

	key := testMasterKey()
	keyPath := writeMasterKeyFile(t, filepath.Join(root, "private"), key.KeyID, key.Key)

	keyPathInConfig := params.KeyPathInConfig
	if keyPathInConfig == "" {
		keyPathInConfig = keyPath
	}
	configRaw := []byte(params.ConfigBody)
	if params.ConfigBody == "" {
		configRaw = []byte(adoptConfigYAML("local_trust", "", adoptKeyLine(keyPathInConfig)))
	}
	configPath := filepath.Join(root, configFileName)
	require.NoError(t, os.WriteFile(configPath, configRaw, 0o600))

	roster, err := marshalInstanceMarker(instanceMarker{
		Version: instanceMarkerVersion, InstanceID: handoffInstanceID,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(ws, instanceMarkerFileName), roster, 0o600))

	f := &handoffFixture{
		t: t, root: root, ws: ws,
		dbPath:     filepath.Join(ws, databaseFileName),
		configPath: configPath,
		key:        key,
		keyPath:    keyPath,
		configRaw:  configRaw,
	}
	f.buildDatabase(params)
	return f
}

// buildDatabase 写三条操作行（init / legacy-adopt / adopt）与全部身份材料。
// buildDatabase writes the three operation rows and the whole identity material set.
func (f *handoffFixture) buildDatabase(params handoffParams) {
	t := f.t
	ctx := context.Background()
	require.NoError(t, core.ValidateCredentialID(handoffCredentialID))
	_, err := core.ParseResourceUID(string(handoffConnectionUID))
	require.NoError(t, err)

	created, err := storage.Bootstrap(ctx, f.dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	require.True(t, created, "夹具必须是新建库")

	db, err := storage.OpenWritable(ctx, f.dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	store := storage.NewManagementStore(db)
	inherit := core.OperationRecord{
		TargetKind:   core.TargetKindInstance,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: "migration:startup",
		AuthMethod:   core.AuthMethodMigration,
		Source:       core.AuthMethodMigration,
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}
	seed := inherit
	seed.OperationID = handoffSeedOperationID
	seed.Action = core.ActionSystemSecretsInit
	seed.TargetKind = core.TargetKindSystem
	require.NoError(t, store.BeginOperation(ctx, seed))
	require.NoError(t, store.FinishOperation(ctx, handoffSeedOperationID,
		core.OperationSuccess, "", "", nil, baselineClock()))

	sourceOpID := handoffSeedOperationID
	if !params.NormalInstance {
		legacy := inherit
		legacy.OperationID = handoffLegacyAdoptOperationID
		legacy.Action = core.ActionInstanceSecretsLegacyAdopt
		require.NoError(t, store.BeginOperation(ctx, legacy))
		require.NoError(t, store.FinishOperation(ctx, handoffLegacyAdoptOperationID,
			core.OperationSuccess, "", "", nil, baselineClock()))
		sourceOpID = handoffLegacyAdoptOperationID

		if !params.SkipAdoptAudit {
			detail, marshalErr := json.Marshal(instanceAdoptDetail{
				CandidateInstanceID:       handoffInstanceID,
				CandidateInstanceIDSHA256: adoptInstanceIDSHA256(handoffInstanceID),
				Case:                      2,
				KeyPathPersisted:          true,
				KeyPathConfigKey:          adoptKeyPathConfigKey,
				ManagedCiphertexts:        1,
				TargetConfigSHA256:        hexSHA256(f.configRaw),
			})
			require.NoError(t, marshalErr)
			adopt := inherit
			adopt.OperationID = handoffAdoptOperationID
			adopt.Action = core.ActionInstanceAdopt
			require.NoError(t, store.BeginOperation(ctx, adopt))
			require.NoError(t, store.FinishOperation(ctx, handoffAdoptOperationID,
				core.OperationSuccess, "", "", detail, baselineClock()))
		}
	}
	require.NoError(t, db.Close())

	fingerprint, err := MasterKeyFingerprint(f.key)
	require.NoError(t, err)
	definition := fmt.Sprintf("name: %s\nbase_url: %s\napi_key: ${HANDOFF_API_KEY}\ndefault_model: handoff-model\ncredential_mode: managed\n",
		handoffConnectionID, params.PublishBaseURL)

	raw := openRawSQLite(t, f.dbPath)
	now := baselineClock().UnixMilli()
	_, err = raw.Exec(
		`INSERT INTO instance_secrets (scope, key_id, fingerprint, initialized_at, operation_id)
		 VALUES ('master_key', ?, ?, ?, ?)`,
		f.key.KeyID, fingerprint, baselineClock().UnixMilli(), sourceOpID)
	require.NoError(t, err)
	// 密文按**原**地址封：发布定义随后可以换地址，那正是 (b) 要发现的事。
	f.insertCredentialRow(raw, handoffBaseURL, 1, handoffCopyPlaintext, now)
	inserted, err := raw.Exec(
		`INSERT INTO resource_versions (resource_kind, resource_uid, resource_id, version_no,
		                                content_hash, definition_yaml, created_at, operation_id)
		 VALUES ('model_connection', ?, ?, 1, ?, ?, ?, ?)`,
		string(handoffConnectionUID), handoffConnectionID,
		hexSHA256([]byte(definition)), definition, now, handoffSeedOperationID)
	require.NoError(t, err)
	versionID, err := inserted.LastInsertId()
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO resource_pointers (resource_kind, resource_id, resource_uid, published_version_id,
		                                applied_version_id, scope, owner_agent_uid, revision, deleted_at,
		                                updated_at, operation_id)
		 VALUES ('model_connection', ?, ?, ?, ?, 'shared', NULL, 1, NULL, ?, ?)`,
		handoffConnectionID, string(handoffConnectionUID), versionID, versionID, now, handoffSeedOperationID)
	require.NoError(t, err)
	_, err = raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
}

// insertCredentialRow 用给定密钥真封一条托管模型连接凭据（身份行 + 版本行）。
// 裸 SQL 是刻意的：用例要精确控制 `state` / `ciphertext` 的可篡改面。
// insertCredentialRow seals one credential through raw SQL so a case can tamper with
// the state and ciphertext independently.
func (f *handoffFixture) insertCredentialRow(raw *sql.DB, baseURL string, versionNo int, plaintext string, now int64) {
	t := f.t
	target, err := CredentialTargetBinding(core.BindingModelConnection, baseURL, "")
	require.NoError(t, err)
	aad := CredentialAAD{
		OwnerKind:     core.BindingModelConnection,
		OwnerUID:      handoffConnectionUID,
		CredentialID:  handoffCredentialID,
		VersionNo:     versionNo,
		TargetBinding: target,
	}
	nonce, ciphertext, err := SealCredential(f.key, aad, []byte(plaintext))
	require.NoError(t, err)
	if versionNo == 1 {
		_, err = raw.Exec(
			`INSERT INTO credentials (credential_id, owner_kind, owner_uid, owner_name, state,
			                          current_version, key_id, created_at, updated_at, operation_id)
			 VALUES (?, 'model_connection', ?, ?, 'active', 1, ?, ?, ?, ?)`,
			handoffCredentialID, string(handoffConnectionUID), handoffConnectionID,
			f.key.KeyID, now, now, handoffSeedOperationID)
		require.NoError(t, err)
	}
	_, err = raw.Exec(
		`INSERT INTO credential_versions (credential_id, version_no, state, key_id, target_binding,
		                                  nonce, ciphertext, created_at, updated_at, operation_id)
		 VALUES (?, ?, 'active', ?, ?, ?, ?, ?, ?, ?)`,
		handoffCredentialID, versionNo, f.key.KeyID, target, nonce, ciphertext, now, now,
		handoffSeedOperationID)
	require.NoError(t, err)
	_, err = raw.Exec(`UPDATE credentials SET current_version = ?, updated_at = ? WHERE credential_id = ?`,
		versionNo, now, handoffCredentialID)
	require.NoError(t, err)
}

// port 给出一个把 WARN 收进 logs 的适配器；logs 为 nil 时日志丢弃。
// port builds an adapter whose WARN lines land in logs; nil discards them.
func (f *handoffFixture) port(logs *strings.Builder) handoffVerifyPort {
	writer := io.Discard
	if logs != nil {
		writer = logs
	}
	return handoffVerifyPort{
		Logger: slog.New(slog.NewTextHandler(writer, nil)),
		Now:    baselineClock,
	}
}

// verify 以工作区根为 `--handoff` 跑一次核对。
// verify runs one gate pass against the workspace root.
func (f *handoffFixture) verify(port handoffVerifyPort) (cli.HandoffVerifyReport, error) {
	return port.VerifyHandoff(context.Background(), cli.HandoffVerifyRequest{Dir: f.ws})
}

// execRaw 在夹具库上执行一条写语句并 checkpoint。
// execRaw runs one write against the fixture and checkpoints the WAL.
func (f *handoffFixture) execRaw(query string, args ...any) {
	t := f.t
	raw := openRawSQLite(t, f.dbPath)
	defer func() { require.NoError(t, raw.Close()) }()
	_, err := raw.Exec(query, args...)
	require.NoError(t, err)
	_, err = raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
}

// execIgnoringCheckConstraints 在**同一条连接**上关掉 CHECK 约束后写一行：
// 夹具要造"写路径不可能产生、但旧缺陷二进制或手改库会留下"的形状（撤销仍留密文）。
// 校验必须对这种行给出正确分类——造不出这种行的用例证明不了分类有判别力。
// execIgnoringCheckConstraints writes a row the schema forbids, so the verifier's
// classification of corrupt legacy data is proven rather than assumed.
func execIgnoringCheckConstraints(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db := openRawSQLite(t, path)
	defer func() { require.NoError(t, db.Close()) }()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, query, args...)
	require.NoError(t, err)
}

// requireHandoffRefusal 判拒绝四件套：稳定码、以码开头的文案、恢复指引、退出码 1。
// requireHandoffRefusal asserts the stable code, code-first rendering, a recovery
// hint and the documented exit status.
func requireHandoffRefusal(t *testing.T, report cli.HandoffVerifyReport, err error, code, want string) {
	t.Helper()
	require.Error(t, err)
	require.False(t, report.Ready, "拒绝路径不得留下 ready 报告")
	require.Equal(t, code, report.Code)
	require.NotEmpty(t, report.Detail)
	require.NotEmpty(t, report.Recovery, "每个拒绝都要给恢复指引")
	require.Contains(t, err.Error(), want)
	require.Equal(t, 1, cli.ExitStatusOf(err), "核验不就绪的退出码是 1")
	var refusal *offlineRefusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, code, refusal.Code)
}

// TestVerifyHandoff_ReportsRevokedAsRevoked：撤销语义两个方向都要分得开。
//   - 密文已清空的撤销版本计入 `Revoked`、**不**参与解密、不计失败；
//   - 仍有密文的撤销版本必须计入 `RevokedWithCiphertext`（C-38 真异常），不得被当成正常撤销。
//
// 逆改点：把"无密文"判定排在"撤销"之前 → 子例一 `MissingCiphertext` 非零；把
// "撤销"判定排在"有密文"之前 → 子例二 `RevokedWithCiphertext` 为零且移交就绪。
// TestVerifyHandoff_ReportsRevokedAsRevoked keeps both revocation directions decidable.
func TestVerifyHandoff_ReportsRevokedAsRevoked(t *testing.T) {
	t.Run("cleared_ciphertext_counts_as_revoked", func(t *testing.T) {
		f := newHandoffFixture(t)
		raw := openRawSQLite(t, f.dbPath)
		now := baselineClock().UnixMilli()
		f.insertCredentialRow(raw, handoffBaseURL, 2, "sk-handoff-second-version", now)
		require.NoError(t, raw.Close())
		// 正常撤销：材料清空（schema 允许的形状）。
		f.execRaw(
			`UPDATE credential_versions SET state = 'revoked', ciphertext = NULL
			  WHERE credential_id = ? AND version_no = 1`, handoffCredentialID)

		report, err := f.verify(f.port(nil))
		require.NoError(t, err)
		require.True(t, report.Ready, "正常撤销的版本不得阻塞交接")
		require.Equal(t, 1, report.Credentials.Revoked)
		require.Zero(t, report.Credentials.MissingCiphertext, "撤销的版本不得被当成密文缺失")
		require.Zero(t, report.Credentials.RevokedWithCiphertext)
		require.Zero(t, report.Credentials.Failed)
		require.Zero(t, report.Credentials.CurrentFailed)
	})

	t.Run("revoked_version_keeping_ciphertext_is_an_anomaly", func(t *testing.T) {
		f := newHandoffFixture(t)
		raw := openRawSQLite(t, f.dbPath)
		now := baselineClock().UnixMilli()
		f.insertCredentialRow(raw, handoffBaseURL, 2, "sk-handoff-second-version", now)
		require.NoError(t, raw.Close())
		// 撤销必须清空密文；这条行只有旧缺陷 / 手改库才会留下——用 ignore_check_constraints
		// 在同一条连接上写出来，校验必须把它算成异常。
		execIgnoringCheckConstraints(t, f.dbPath,
			`UPDATE credential_versions SET state = 'revoked'
			  WHERE credential_id = ? AND version_no = 2`, handoffCredentialID)
		f.execRaw(`PRAGMA wal_checkpoint(TRUNCATE)`)

		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeIdentityMismatch, "历史密文 1 条失败")
		require.Equal(t, 1, report.Credentials.RevokedWithCiphertext)
		require.Equal(t, 1, report.Credentials.Failed)
		require.Equal(t, 1, report.Credentials.HistoricalOK, "非当前版本且可解的第 1 版仍应计入")
		require.Equal(t, 1, report.Credentials.CurrentChecked)
		require.Equal(t, 1, report.Credentials.CurrentFailed, "当前版本已撤销：当前可用性必须跟着报红")
		require.Equal(t, 1, report.Credentials.KeyRevoked)
	})
}

// TestVerifyHandoff_ReportsMissingCiphertext：`active` 但密文为空的版本必须计数，
// 不得因为"没有可解的密文"就跳过——凭据不可用却报交接就绪，只会在业务调用时才暴露。
//
// 逆改点：`!sealed` 分支不计数 → `MissingCiphertext` 为零且报告 ready。
// TestVerifyHandoff_ReportsMissingCiphertext counts an empty ciphertext instead of
// skipping it.
func TestVerifyHandoff_ReportsMissingCiphertext(t *testing.T) {
	f := newHandoffFixture(t)
	// 零长度 BLOB 满足 schema 的 `ciphertext IS NOT NULL`，校验侧按长度判缺失。
	f.execRaw(`UPDATE credential_versions SET ciphertext = X'' WHERE credential_id = ?`,
		handoffCredentialID)

	report, err := f.verify(f.port(nil))
	requireHandoffRefusal(t, report, err, handoffCodeIdentityMismatch, "历史密文 1 条失败")
	require.Equal(t, 1, report.Credentials.Checked)
	require.Equal(t, 1, report.Credentials.MissingCiphertext)
	require.Equal(t, 1, report.Credentials.Failed)
	require.Zero(t, report.Credentials.AADMismatch, "缺密文不是解密失败，分类不得混")
	require.Equal(t, 1, report.Credentials.CurrentChecked)
	require.Equal(t, 1, report.Credentials.CurrentFailed)
	require.Equal(t, 1, report.Credentials.KeyRevoked)
	require.Contains(t, report.Detail, "当前可用性 1 条失败")
}

// TestVerifyHandoff_CurrentBindingRequired：统一实现不得只解密密文就放行——发布定义
// 换了地址，密文仍按旧地址封，`(b)` 必须报 `target_mismatch`。
//
// 逆改点：`managedCredentialProblem` 丢掉 `expectedTarget` 检查 → `(a)` 全绿、
// 报告 ready，而运维会在切换后第一次调用时才失败。
// TestVerifyHandoff_CurrentBindingRequired keeps the current-binding check in the
// shared verifier.
func TestVerifyHandoff_CurrentBindingRequired(t *testing.T) {
	f := newHandoffFixture(t, func(p *handoffParams) {
		p.PublishBaseURL = "https://api.example.com/v2"
	})

	report, err := f.verify(f.port(nil))
	requireHandoffRefusal(t, report, err, handoffCodeIdentityMismatch, "当前可用性 1 条失败")
	require.Equal(t, 1, report.Credentials.Checked)
	require.Zero(t, report.Credentials.Failed, "(a) 密文本身完好，不得跟着报红")
	require.Equal(t, 1, report.Credentials.CurrentChecked)
	require.Equal(t, 1, report.Credentials.CurrentFailed)
	require.Equal(t, 1, report.Credentials.TargetMismatch)
	require.Zero(t, report.Credentials.BindingMissing)
}

// TestVerifyHandoff_NoPlaintext：报告、错误串与 WARN 日志都不得带出明文或密钥材料——
// 交接记录本身是会被复制 / 上传的工件。
//
// 逆改点：报告或 WARN 里拼进明文 / nonce / 密钥片段 → 本用例红。
// TestVerifyHandoff_NoPlaintext keeps every readable surface free of credential
// material, on both the ready and the refused path.
func TestVerifyHandoff_NoPlaintext(t *testing.T) {
	f := newHandoffFixture(t)
	logs := &strings.Builder{}

	ready, err := f.verify(f.port(logs))
	require.NoError(t, err)
	require.True(t, ready.Ready)

	// 再走一条拒绝路径：改配置让目标配置哈希失效，WARN 同样是可读面。
	edited := append(append([]byte{}, f.configRaw...), '\n')
	require.NoError(t, os.WriteFile(f.configPath, edited, 0o600))
	refused, err := f.verify(f.port(logs))
	requireHandoffRefusal(t, refused, err, handoffCodeIdentityMismatch, "采纳之后配置又被改过")

	rendered := fmt.Sprintf("%+v", ready) + fmt.Sprintf("%+v", refused)
	secrets := []string{
		handoffCopyPlaintext,
		string(f.key.Key),
		hex.EncodeToString(f.key.Key),
		base64.StdEncoding.EncodeToString(f.key.Key),
	}
	for _, secret := range secrets {
		require.NotContains(t, rendered, secret, "报告不得带出明文或密钥材料")
		require.NotContains(t, logs.String(), secret, "WARN 日志不得带出明文或密钥材料")
		require.NotContains(t, err.Error(), secret, "错误串不得带出明文或密钥材料")
	}
}

// TestVerifyHandoff_MigrationCopyReadyAndConfigGate 判迁移准备副本的主链路与配置门：
// 就绪副本逐项事实齐全；采纳之后配置再被改动即 `instance_identity_mismatch`；
// 缺 `instance.adopt` 审计（没有配置基线）即 `recovery_required`。
//
// 逆改点：跳过 `target_config_sha256` 比对 → 子例二 ready；缺基线时按"未核对"放行
// → 子例三 ready，两种情况都让"交接就绪"失去意义。
// TestVerifyHandoff_MigrationCopyReadyAndConfigGate pins the ready facts and both
// configuration-gate refusals.
func TestVerifyHandoff_MigrationCopyReadyAndConfigGate(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		f := newHandoffFixture(t)
		report, err := f.verify(f.port(nil))
		require.NoError(t, err)
		require.True(t, report.Ready)
		require.Empty(t, report.Code)
		require.Empty(t, report.Detail)
		require.Empty(t, report.Recovery)

		require.Equal(t, f.ws, report.Workspace)
		require.Equal(t, filepath.Join(f.ws, instanceMarkerFileName), report.RosterPath)
		require.Equal(t, f.configPath, report.ConfigPath)
		require.True(t, report.RosterPresent)
		require.Equal(t, handoffInstanceID, report.InstanceID)

		require.True(t, report.RecordPresent)
		require.Equal(t, f.key.KeyID, report.RecordKeyID)
		require.NotEmpty(t, report.RecordFingerprint)

		require.True(t, report.KeyPresent)
		require.True(t, report.KeyPathPersisted)
		require.Equal(t, f.keyPath, report.KeyFile)
		require.Equal(t, f.key.KeyID, report.KeyID)
		require.Equal(t, report.RecordFingerprint, report.Fingerprint)

		require.True(t, report.ConfigRequired)
		require.True(t, report.ConfigChecked)
		require.Equal(t, hexSHA256(f.configRaw), report.TargetConfigSHA256)
		require.Equal(t, report.TargetConfigSHA256, report.CurrentConfigSHA256)

		require.Equal(t, 1, report.Credentials.Checked)
		require.Zero(t, report.Credentials.Failed)
		require.Equal(t, 1, report.Credentials.CurrentChecked)
		require.Zero(t, report.Credentials.CurrentFailed)
	})

	t.Run("config_edited_after_adoption", func(t *testing.T) {
		f := newHandoffFixture(t)
		edited := []byte(adoptConfigYAML("local_trust", "",
			adoptKeyLine(f.keyPath), "handoff_note: edited"))
		require.NoError(t, os.WriteFile(f.configPath, edited, 0o600))

		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeIdentityMismatch, "采纳之后配置又被改过")
		require.True(t, report.ConfigRequired)
		require.False(t, report.ConfigChecked)
		require.Equal(t, hexSHA256(f.configRaw), report.TargetConfigSHA256)
		require.Equal(t, hexSHA256(edited), report.CurrentConfigSHA256)
		require.NotEqual(t, report.TargetConfigSHA256, report.CurrentConfigSHA256,
			"两侧哈希都必须落报告，运维才能看出是哪一方变了")
	})

	t.Run("adopt_audit_missing", func(t *testing.T) {
		f := newHandoffFixture(t, func(p *handoffParams) { p.SkipAdoptAudit = true })
		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeRecoveryRequired, "没有属于实例")
		require.True(t, report.ConfigRequired)
		require.False(t, report.ConfigChecked)
		require.Empty(t, report.TargetConfigSHA256)
	})
}

// TestVerifyHandoff_MissingRosterRefuses：身份材料缺失的三种形状都要落在
// `instance_identity_missing`，并且恢复指引必须指出导出包的正确复核命令——
// `<dir>` 只接受工作区根，导出包不在本命令的输入面。
//
// 逆改点：缺名册时凭库里的记录重建身份 → 来源证明被"重建"冒名顶替。
// TestVerifyHandoff_MissingRosterRefuses pins the missing-material code and the
// export-package boundary.
func TestVerifyHandoff_MissingRosterRefuses(t *testing.T) {
	t.Run("missing_roster", func(t *testing.T) {
		f := newHandoffFixture(t)
		require.NoError(t, os.Remove(filepath.Join(f.ws, instanceMarkerFileName)))

		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeIdentityMissing, "没有实例名册")
		require.False(t, report.RosterPresent)
		require.Empty(t, report.InstanceID)
		require.Contains(t, report.Recovery, "db verify --dump",
			"导出包用 --dump 复核；恢复指引必须给对命令")
	})

	t.Run("missing_database", func(t *testing.T) {
		f := newHandoffFixture(t)
		require.NoError(t, os.Remove(f.dbPath))

		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeIdentityMissing, "没有 SQLite 库")
		require.True(t, report.RosterPresent, "名册已核对过，缺库是后一步的事实")
	})

	t.Run("export_package_is_not_a_workspace", func(t *testing.T) {
		pkg := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(pkg, "manifest.json"), []byte("{}"), 0o600))

		report, err := handoffVerifyPort{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}.
			VerifyHandoff(context.Background(), cli.HandoffVerifyRequest{Dir: pkg})
		requireHandoffRefusal(t, report, err, handoffCodeIdentityMissing, "没有实例名册")
		require.Contains(t, report.Recovery, "<copy-root>/.basalt", "必须说明 <dir> 是工作区根")
		require.Contains(t, report.Recovery, "db verify --dump")
	})

	t.Run("empty_dir_is_a_usage_error", func(t *testing.T) {
		_, err := handoffVerifyPort{}.VerifyHandoff(context.Background(),
			cli.HandoffVerifyRequest{Dir: "   "})
		require.Error(t, err)
		require.Equal(t, 2, cli.ExitStatusOf(err), "前置不满足按退出码 2")
		require.Contains(t, err.Error(), "--handoff 为空")
		require.Contains(t, err.Error(), "<copy-root>/.basalt")
	})
}

// TestVerifyHandoff_NormalInstanceDoesNotClaimConfigChecked：普通存量实例（记录来源
// 不是 legacy-adopt）没有 `target_config_sha256` 基线，不得假装核对过配置；
// 报告必须显式写"未核对"，而不是静默通过。
//
// 逆改点：对所有实例都去取 adopt 审计 → 本用例 recovery_required；或把
// `ConfigChecked` 置真 → 断言红，交接记录就成了一句无法追溯的"已核对"。
// TestVerifyHandoff_NormalInstanceDoesNotClaimConfigChecked keeps the report honest
// about what it did not check.
func TestVerifyHandoff_NormalInstanceDoesNotClaimConfigChecked(t *testing.T) {
	f := newHandoffFixture(t, func(p *handoffParams) { p.NormalInstance = true })

	report, err := f.verify(f.port(nil))
	require.NoError(t, err)
	require.True(t, report.Ready)
	require.False(t, report.ConfigRequired)
	require.False(t, report.ConfigChecked)
	require.Empty(t, report.TargetConfigSHA256)
	require.Contains(t, report.ConfigNote, "未核对")
	require.Equal(t, 1, report.Credentials.Checked)
	require.Zero(t, report.Credentials.Failed)
}

// TestVerifyHandoff_KeyFactsGuard：密钥四道门（已持久化 / 绝对路径 / 可加载 / 与记录
// 相符）与"有密文却无记录"的来源线各判一例。
//
// 逆改点：跳过 `fingerprint` 比对 → 子例三 ready；把"无记录"当 env-only → 子例四 ready。
// TestVerifyHandoff_KeyFactsGuard pins the key gates and the provable-provenance line.
func TestVerifyHandoff_KeyFactsGuard(t *testing.T) {
	t.Run("key_path_not_persisted", func(t *testing.T) {
		f := newHandoffFixture(t, func(p *handoffParams) {
			p.ConfigBody = adoptConfigYAML("local_trust", "")
		})
		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeRecoveryRequired,
			"没有 secrets.master_key_file")
		require.False(t, report.KeyPresent)
	})

	t.Run("relative_key_path", func(t *testing.T) {
		f := newHandoffFixture(t, func(p *handoffParams) {
			p.KeyPathInConfig = filepath.Join("keys", "master.key")
		})
		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeRecoveryRequired, "相对路径")
		require.True(t, report.KeyPathPersisted)
		require.False(t, report.KeyPresent, "相对路径不得被按 cwd 猜出密钥")
	})

	t.Run("key_does_not_match_record", func(t *testing.T) {
		f := newHandoffFixture(t)
		other := testMasterKey()
		other.KeyID = "k_other"
		other.Key = make([]byte, masterKeyBytes)
		for i := range other.Key {
			other.Key[i] = byte(0xA0 + i)
		}
		otherPath := writeMasterKeyFile(t, filepath.Join(f.root, "other"), other.KeyID, other.Key)
		require.NoError(t, os.WriteFile(f.configPath,
			[]byte(adoptConfigYAML("local_trust", "", adoptKeyLine(otherPath))), 0o600))

		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeIdentityMismatch, "主密钥与记录不符")
		require.Equal(t, other.KeyID, report.KeyID, "两侧标识都要落报告，差异才可追溯")
		require.Equal(t, f.key.KeyID, report.RecordKeyID)
		require.NotEqual(t, report.RecordFingerprint, report.Fingerprint)
	})

	t.Run("ciphertext_without_record", func(t *testing.T) {
		f := newHandoffFixture(t)
		f.execRaw(`DELETE FROM instance_secrets`)

		report, err := f.verify(f.port(nil))
		requireHandoffRefusal(t, report, err, handoffCodeIdentityMismatch, "来源不可证明")
		require.False(t, report.RecordPresent)
		require.Empty(t, report.RecordKeyID)
	})
}
