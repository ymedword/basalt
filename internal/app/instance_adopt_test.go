package app

// 本文件是 `instance adopt` 的判卷（颗粒度文档 §3.1.8a / C-47，check `us16-instance-adopt`）。
//
// 命令是**离线**的：只碰工作区锁、同级部署配置、旧 SQLite 库与名册，零外呼、不装配运行期。
// 因此全部断言都在真 SQLite 夹具上完成，不需要 PostgreSQL。
//
// 零写入用四条独立判据核验：名册不存在、`instance_secrets` 行数不变、
// `management_operations` 行数不变、库文件 SHA-256 前后一致——只断言"返回了错误"不足以
// 证明零写：拒绝路径必须真的一个字节都没落。
//
// 重启后的判定由 `startupDecision` 镜像 `gatherFacts` 复核：`InstanceSecretsDeps.Store` 是
// 具体类型 `*postgres.InstanceSecretsStore`，单元侧造不出带 `Ensure` 的服务实例；「`Ensure`
// 恰好补写 1 条 + `system.secrets_init`」由 PostgreSQL 集成用例
// `TestInstanceSecrets_AdoptAfterRecordFailure` 承担（run-state 已披露）。
// These tests pin the offline adoption contract on a real SQLite fixture: every refusal
// writes nothing, and the roster is the only artifact the successful path creates.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// 判卷用固定标识：凭据 ID 走 Crockford 载荷校验，资源 UID 走领域解析，两者都不能是
// 自由字符串。两条凭据的属主 UID 必须不同（`UNIQUE (owner_kind, owner_uid)`）。
// Fixed identifiers: credential ids and resource uids are domain-validated literals.
var (
	adoptCredID1  = "cred_00000000000000000000000011"
	adoptCredID2  = "cred_00000000000000000000000012"
	adoptOwner1   = core.ResourceUID("rs_00000000000000000000000011")
	adoptOwner2   = core.ResourceUID("rs_00000000000000000000000012")
	adoptTargetID = strings.Repeat("a", 64)
)

// adoptFixture 是采纳判卷的夹具：真旧 SQLite 工作区 + 同级部署配置 + 工作区外主密钥文件。
// `ws` 就是 `--workspace`（`<root>/.basalt`）：工作区**根**，名册落在它下面。
// The fixture is a genuine legacy SQLite workspace with a sibling config and an
// out-of-workspace master key.
type adoptFixture struct {
	t          *testing.T
	root       string // 部署根（WorkDir）：配置与工作区同级
	ws         string // 工作区根 = <root>/.basalt
	configPath string
	dbPath     string
	dataRoot   string // 当前用户数据根；钉住私密目录推导，避免写到真机上
	keyPath    string
	key        MasterKey
}

// newAdoptFixture 建夹具：库按当前版本建好（存量实例形态），主密钥落在工作区之外。
func newAdoptFixture(t *testing.T) *adoptFixture {
	t.Helper()
	ctx := context.Background()

	root := t.TempDir()
	ws := filepath.Join(root, workspaceDirName)
	require.NoError(t, os.MkdirAll(ws, 0o700))
	dbPath := filepath.Join(ws, databaseFileName)
	created, err := storage.Bootstrap(ctx, dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	require.True(t, created, "夹具必须是新建库：存量实例才有库")

	dataRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", dataRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)

	key := testMasterKey()
	keyDir := filepath.Join(root, "private")
	keyPath := writeMasterKeyFile(t, keyDir, key.KeyID, key.Key)

	f := &adoptFixture{
		t:          t,
		root:       root,
		ws:         ws,
		configPath: filepath.Join(ws, "..", configFileName),
		dbPath:     dbPath,
		dataRoot:   dataRoot,
		keyPath:    keyPath,
		key:        key,
	}
	f.configPath = filepath.Clean(f.configPath)
	return f
}

// adoptConfigYAML 拼同级部署配置。`secretsLines` 为空即整段省略（纯 env 形态）。
// adoptConfigYAML renders the sibling deployment config; no secrets lines omits the
// whole section.
func adoptConfigYAML(mode, token string, secretsLines ...string) string {
	var b strings.Builder
	b.WriteString("http:\n  admin_auth:\n    mode: " + mode + "\n")
	if token != "" {
		b.WriteString("    token: '" + token + "'\n")
	}
	if len(secretsLines) > 0 {
		b.WriteString("secrets:\n")
		for _, line := range secretsLines {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

// adoptKeyLine 渲染 `secrets.master_key_file` 行；单引号让 Windows 反斜杠保持原样。
func adoptKeyLine(path string) string {
	return "master_key_file: '" + path + "'"
}

// writeConfig 覆盖同级部署配置（本机信任 + 给定 secrets 段）。
func (f *adoptFixture) writeConfig(secretsLines ...string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(f.configPath,
		[]byte(adoptConfigYAML("local_trust", "", secretsLines...)), 0o600))
}

// writeRawConfig 覆盖同级部署配置（整份内容由用例给出）。
func (f *adoptFixture) writeRawConfig(body string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(f.configPath, []byte(body), 0o600))
}

// adopt 发起一次采纳；`--master-key-file` 原样透传（空串即未给）。
func (f *adoptFixture) adopt(masterKeyFile string) (cli.InstanceAdoptReport, error) {
	f.t.Helper()
	port := instanceAdoptPort{Now: baselineClock}
	return port.Adopt(context.Background(), cli.InstanceAdoptRequest{
		Workspace:     f.ws,
		ConfigPath:    f.configPath,
		MasterKeyFile: masterKeyFile,
	})
}

// roster 读回名册（不存在返回 present=false）。
func (f *adoptFixture) roster() (instanceMarker, bool) {
	f.t.Helper()
	marker, present, err := readInstanceMarkerAt(f.ws)
	require.NoError(f.t, err)
	return marker, present
}

// writeRoster 直接落一份名册（既有身份 / 预写候选两种用例共用）。
func (f *adoptFixture) writeRoster(instanceID string) {
	f.t.Helper()
	raw, err := marshalInstanceMarker(instanceMarker{
		Version:    instanceMarkerVersion,
		InstanceID: instanceID,
	})
	require.NoError(f.t, err)
	require.NoError(f.t, os.WriteFile(filepath.Join(f.ws, instanceMarkerFileName), raw, 0o600))
}

// withWritableDB 打开可写句柄执行夹具写入，返回前关闭（关闭即触发 WAL 检查点）。
func (f *adoptFixture) withWritableDB(fn func(*storage.DB)) {
	f.t.Helper()
	db, err := storage.OpenWritable(context.Background(), f.dbPath, storage.SupportedSchemaVersion)
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, db.Close()) }()
	fn(db)
}

// seedOperation 造一条**已完成**的操作行：凭据表的 `operation_id` 外键指向它。
func (f *adoptFixture) seedOperation(opID string) {
	f.t.Helper()
	ctx := context.Background()
	f.withWritableDB(func(db *storage.DB) {
		store := storage.NewManagementStore(db)
		require.NoError(f.t, store.BeginOperation(ctx, core.OperationRecord{
			OperationID:  opID,
			Action:       "system.test_seed",
			TargetKind:   core.TargetKindInstance,
			TargetID:     core.InstanceTargetIdentity,
			ActorSubject: "cli:test",
			// auth_method 有 CHECK 约束（bearer / local_trust / cli / migration），
			// 造数只能用契约里的值。
			AuthMethod: "cli",
			Source:     "cli",
			CreatedAt:  baselineClock(),
		}))
		require.NoError(f.t, store.FinishOperation(ctx, opID, core.OperationSuccess, "", "", nil, baselineClock()))
	})
}

// seedRecord 写 `instance_secrets` 记录（`key_id` / 指纹可覆盖，用来造"不符"的恢复态）。
func (f *adoptFixture) seedRecord(keyID, fingerprint, opID string) {
	f.t.Helper()
	f.withWritableDB(func(db *storage.DB) {
		require.NoError(f.t, storage.NewInstanceSecretsStore(db).Insert(context.Background(),
			storage.InstanceSecretRecord{
				Scope:         core.InstanceSecretScopeMasterKey,
				KeyID:         keyID,
				Fingerprint:   fingerprint,
				InitializedAt: baselineClock(),
				OperationID:   opID,
			}))
	})
}

// seedCredential 写一条托管凭据：`sealKey` 决定密文能不能被"原密钥"解开，`keyID` 是落库的
// 密钥标识（两者可以故意不一致——"key_id 相符但解不开"正是案例 1 要拦的形状）。
func (f *adoptFixture) seedCredential(opID, credID string, ownerUID core.ResourceUID, keyID string, sealKey MasterKey) {
	f.t.Helper()
	nonce, ciphertext, err := SealCredential(sealKey, CredentialAAD{
		OwnerKind:     core.BindingModelConnection,
		OwnerUID:      ownerUID,
		CredentialID:  credID,
		VersionNo:     1,
		TargetBinding: adoptTargetID,
	}, []byte("sk-adopt-plaintext"))
	require.NoError(f.t, err)

	now := baselineClock().UnixMilli()
	f.withWritableDB(func(db *storage.DB) {
		require.NoError(f.t, storage.NewCredentialStore(db).Create(context.Background(),
			core.CredentialRecord{
				ID:             credID,
				OwnerKind:      core.BindingModelConnection,
				OwnerUID:       ownerUID,
				OwnerName:      "openai",
				State:          core.CredentialIdentityActive,
				CurrentVersion: 1,
				KeyID:          keyID,
				CreatedAt:      now,
				UpdatedAt:      now,
				OperationID:    opID,
			},
			core.CredentialVersionRecord{
				CredentialID:  credID,
				VersionNo:     1,
				State:         core.CredentialVersionActive,
				KeyID:         keyID,
				TargetBinding: adoptTargetID,
				Last4:         "text",
				Nonce:         nonce,
				Ciphertext:    ciphertext,
				CreatedAt:     now,
				UpdatedAt:     now,
				OperationID:   opID,
			}))
	})
}

// fingerprint 是原密钥的公开摘要（记录侧比对用）。
func (f *adoptFixture) fingerprint() string {
	f.t.Helper()
	fp, err := MasterKeyFingerprint(f.key)
	require.NoError(f.t, err)
	return fp
}

// seedPendingAdopt 造一条崩溃留下的 `instance.adopt` pending 行。
func (f *adoptFixture) seedPendingAdopt(opID string, detail []byte) {
	f.t.Helper()
	f.withWritableDB(func(db *storage.DB) {
		require.NoError(f.t, storage.NewManagementStore(db).BeginOperation(context.Background(),
			core.OperationRecord{
				OperationID:  opID,
				Action:       core.ActionInstanceAdopt,
				TargetKind:   core.TargetKindInstance,
				TargetID:     core.InstanceTargetIdentity,
				ActorSubject: "cli",
				AuthMethod:   "cli",
				Source:       "cli",
				DetailJSON:   detail,
				CreatedAt:    baselineClock(),
			}))
	})
}

// operation 读回一条操作行（终态与查证结论）。
func (f *adoptFixture) operation(opID string) core.OperationRecord {
	f.t.Helper()
	db, err := storage.OpenReadOnly(context.Background(), f.dbPath, storage.SupportedSchemaVersion)
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, db.Close()) }()
	rec, err := storage.NewManagementStore(db).GetOperation(context.Background(), opID)
	require.NoError(f.t, err)
	return rec
}

// rowCount 用一个独立的只读连接数表：`storage.DB` 不对外暴露查询面，而零写入判据必须真读库。
// rowCount reads counts through an independent read-only connection.
func (f *adoptFixture) rowCount(query string, args ...any) int {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.dbPath+"?mode=ro")
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, db.Close()) }()
	var count int
	require.NoError(f.t, db.QueryRow(query, args...).Scan(&count))
	return count
}

// adoptSnapshot 是零写入判据的读数快照：四项彼此独立，任一项变化都说明拒绝路径写了东西。
type adoptSnapshot struct {
	rosterPresent bool
	secretsRows   int
	opsRows       int
	dbHash        string
}

// snapshot 采一次读数。名册只看**存在性**（Stat）而不解析——坏名册本身也是被测场景，
// 解析会把 fixture 自己搞成失败。
// The snapshot stats the roster instead of parsing it: a corrupt marker is itself
// under test.
func (f *adoptFixture) snapshot() adoptSnapshot {
	f.t.Helper()
	_, statErr := os.Stat(filepath.Join(f.ws, instanceMarkerFileName))
	raw, err := os.ReadFile(f.dbPath)
	require.NoError(f.t, err)
	return adoptSnapshot{
		rosterPresent: statErr == nil,
		secretsRows:   f.rowCount(`SELECT count(*) FROM instance_secrets`),
		opsRows:       f.rowCount(`SELECT count(*) FROM management_operations`),
		dbHash:        sha256Hex(f.t, raw),
	}
}

// requireUnchanged 断言拒绝路径零写入：名册存在性、两张表的行数、库字节四项都不变。
func (f *adoptFixture) requireUnchanged(before adoptSnapshot) {
	f.t.Helper()
	after := f.snapshot()
	require.Equal(f.t, before.rosterPresent, after.rosterPresent, "拒绝路径不得写名册")
	require.Equal(f.t, before.secretsRows, after.secretsRows, "拒绝路径不得写 instance_secrets")
	require.Equal(f.t, before.opsRows, after.opsRows, "拒绝路径不得写管理操作行")
	require.Equal(f.t, before.dbHash, after.dbHash, "拒绝路径不得改动库字节")
}

// requireNoNewRows 用于**有 pending 待回填**的场景：回填本身是合法写入（终态与查证结论
// 落在原行上），所以只断言没有新增名册与新增行——库字节必然变。
// Recovery legitimately updates the pending row, so those cases assert "no new rows".
func (f *adoptFixture) requireNoNewRows(before adoptSnapshot) {
	f.t.Helper()
	after := f.snapshot()
	require.Equal(f.t, before.rosterPresent, after.rosterPresent, "拒绝路径不得写名册")
	require.Equal(f.t, before.secretsRows, after.secretsRows, "拒绝路径不得写 instance_secrets")
	require.Equal(f.t, before.opsRows, after.opsRows, "拒绝路径不得新增管理操作行")
}

// startupDecision 镜像 `InstanceSecretsService.gatherFacts` 的事实采集，再交给同一张判定表；
// 返回判定结论与现存密文条数。重启后的行为只能这样复核——离线命令本身不装配运行期。
// startupDecision mirrors gatherFacts so the post-restart verdict is checked with the
// same decision table.
func (f *adoptFixture) startupDecision() (SecretsDecision, int) {
	f.t.Helper()
	ctx := context.Background()
	cfg, err := cli.LoadConfig(f.configPath, os.LookupEnv)
	require.NoError(f.t, err)
	keyPath, err := resolveMasterKeyFile(cfg, f.root)
	require.NoError(f.t, err)

	facts := SecretsFacts{
		Configured:    strings.TrimSpace(keyPath) != "",
		FreshInstance: false, // 库在，实例就不是本次新建
		AutoInit:      cfg.Secrets.AutoInitEnabled(),
	}
	db, err := storage.OpenReadOnly(ctx, f.dbPath, storage.SupportedSchemaVersion)
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, db.Close()) }()

	store := storage.NewInstanceSecretsStore(db)
	record, exists, err := store.Load(ctx, core.InstanceSecretScopeMasterKey)
	require.NoError(f.t, err)
	facts.Record = adoptRecordForDecision(adoptFacts{record: record, hasRecord: exists})

	count, err := store.CountManagedCiphertext(ctx)
	require.NoError(f.t, err)
	facts.ManagedCiphertexts = count
	if facts.Configured {
		present, probeErr := masterKeyFileExists(keyPath)
		require.NoError(f.t, probeErr)
		facts.KeyPresent = present
		if present {
			key, loadErr := LoadMasterKey(keyPath, f.ws)
			require.NoError(f.t, loadErr)
			facts.KeyID = key.KeyID
			if count > 0 {
				sealed, listErr := store.SealedCredentials(ctx)
				require.NoError(f.t, listErr)
				unreadable, _, verifyErr := verifySealedCredentials(key, sealed)
				require.NoError(f.t, verifyErr)
				facts.CiphertextsVerified = unreadable == 0
			}
		}
	}
	return EvaluateSecrets(facts), count
}

// requireAdoptRefusal 断言拒绝码与零写入。
func (f *adoptFixture) requireAdoptRefusal(err error, code string, before adoptSnapshot) {
	f.t.Helper()
	require.Error(f.t, err)
	var refusal *adoptRefusal
	require.ErrorAs(f.t, err, &refusal)
	require.Equal(f.t, code, refusal.Code)
	require.True(f.t, strings.HasPrefix(err.Error(), code+":"), "错误文案必须以稳定码开头")
	f.requireUnchanged(before)
}

// requireAdoptRefusalRecovered 是"拒绝 + 已回填 pending"的变体判据。
func (f *adoptFixture) requireAdoptRefusalRecovered(err error, code string, before adoptSnapshot) {
	f.t.Helper()
	require.Error(f.t, err)
	var refusal *adoptRefusal
	require.ErrorAs(f.t, err, &refusal)
	require.Equal(f.t, code, refusal.Code)
	f.requireNoNewRows(before)
}

// TestInstanceAdopt_EstablishesIdentityWithoutMintingKey：采纳建立身份，但**不**铸密钥。
// 中文验收点：案例 4 无密钥文件时命令结束为 `uninitialized` / `await_init`，不生成主密钥、
// 不写 `instance_secrets`；写名册是唯一副作用。
func TestInstanceAdopt_EstablishesIdentityWithoutMintingKey(t *testing.T) {
	f := newAdoptFixture(t)
	f.writeConfig() // 整段省略 secrets：没有密钥路径，也没有密钥文件

	before := f.snapshot()
	report, err := f.adopt("")
	require.NoError(t, err)

	require.Equal(t, 4, report.Case)
	require.Equal(t, string(core.SecretStateUninitialized), report.State)
	require.Equal(t, string(SecretsActionAwaitInit), report.Action)
	require.False(t, report.WroteSecretsRecord)
	require.False(t, report.RestartVerified)
	require.False(t, report.CiphertextsVerified)

	marker, present := f.roster()
	require.True(t, present, "采纳必须写名册")
	require.Equal(t, report.InstanceID, marker.InstanceID)
	require.Regexp(t, instanceIDPattern, marker.InstanceID)

	// 不铸密钥：候选私密目录不会被创建，其中也不存在默认密钥文件。
	// （夹具里的 `f.keyDir` 是**测试自己**写的主密钥，不能用来判"有没有铸钥"。）
	require.NotEmpty(t, report.NewPrivateDir)
	_, statErr := os.Stat(report.NewPrivateDir)
	require.ErrorIs(t, statErr, os.ErrNotExist, "采纳不得创建私密目录")
	defaultKey := filepath.Join(f.dataRoot, instanceDataDirName, instancesDirName,
		marker.InstanceID, masterKeyFileName)
	_, keyErr := os.Stat(defaultKey)
	require.ErrorIs(t, keyErr, os.ErrNotExist, "采纳不得生成主密钥")

	// 记录仍是 0 条；审计恰好 1 条 success 的 instance.adopt。
	require.Equal(t, 0, f.rowCount(`SELECT count(*) FROM instance_secrets`))
	require.Equal(t, before.opsRows+1, f.rowCount(`SELECT count(*) FROM management_operations`))
	require.Equal(t, 1, f.rowCount(
		`SELECT count(*) FROM management_operations WHERE action = ? AND status = 'success'`,
		core.ActionInstanceAdopt))
	require.False(t, report.RecoveredPending)
}

// TestInstanceAdopt_WithoutRecordWithCiphertextRefuses：案例 3 一律拒绝——"能解开"不等于
// "来源可信"。中文验收点：无记录 + 有密文 → `unprovable_provenance`，零写入。
func TestInstanceAdopt_WithoutRecordWithCiphertextRefuses(t *testing.T) {
	f := newAdoptFixture(t)
	f.seedOperation("op-adopt-seed-1")
	f.seedCredential("op-adopt-seed-1", adoptCredID1, adoptOwner1, f.key.KeyID, f.key)
	f.writeConfig(adoptKeyLine(f.keyPath))

	before := f.snapshot()
	report, err := f.adopt("")
	f.requireAdoptRefusal(err, adoptCodeUnprovableProvenance, before)

	require.Equal(t, 3, report.Case)
	require.Equal(t, 1, report.ManagedCiphertexts)
	require.Empty(t, report.InstanceID, "拒绝路径不得先铸标识再写名册")
	// 拒绝时 WARN 留痕（可观测性双轨：拒绝路径没有审计行，日志是唯一痕迹）。
	require.False(t, report.RecoveredPending)
}

// TestInstanceAdopt_ExistingRosterUnchanged：既有名册（含损坏 / 版本过高）一律不覆盖。
// 中文验收点：`instance_roster_exists` / `instance_roster_invalid` 两种拒绝且名册逐字节不变。
func TestInstanceAdopt_ExistingRosterUnchanged(t *testing.T) {
	t.Run("valid_roster", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		f.writeRoster("inst_0123456789abcdef")
		original, err := os.ReadFile(filepath.Join(f.ws, instanceMarkerFileName))
		require.NoError(t, err)

		before := f.snapshot()
		report, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeRosterExists, before)

		after, err := os.ReadFile(filepath.Join(f.ws, instanceMarkerFileName))
		require.NoError(t, err)
		require.Equal(t, original, after, "既有身份必须逐字节保留")
		require.Empty(t, report.AuditOperationID, "拒绝发生在写阶段之前，不得有审计行")
	})

	t.Run("corrupt_roster", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		raw := []byte(`{"version":1,"instance_id":"not-an-instance"}`)
		require.NoError(t, os.WriteFile(filepath.Join(f.ws, instanceMarkerFileName), raw, 0o600))

		before := f.snapshot()
		_, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeRosterInvalid, before)

		after, err := os.ReadFile(filepath.Join(f.ws, instanceMarkerFileName))
		require.NoError(t, err)
		require.Equal(t, raw, after, "坏名册不得被补写或替换")
	})

	t.Run("version_too_new", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		raw := []byte(`{"version":2,"instance_id":"inst_0123456789abcdef"}`)
		require.NoError(t, os.WriteFile(filepath.Join(f.ws, instanceMarkerFileName), raw, 0o600))

		before := f.snapshot()
		_, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeRosterInvalid, before)
	})
}

// TestInstanceAdopt_RecordWithCiphertextVerifiesEveryCiphertext：案例 1 必须**逐条**试解。
// 中文验收点：两条都能解 → 成功且 `CiphertextsVerified`；第二条解不开 → `recovery_required`
// 且零写入（只验一条的实现会把这条放过去）。
func TestInstanceAdopt_RecordWithCiphertextVerifiesEveryCiphertext(t *testing.T) {
	t.Run("all_decryptable", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.seedOperation("op-adopt-seed-1")
		f.seedCredential("op-adopt-seed-1", adoptCredID1, adoptOwner1, f.key.KeyID, f.key)
		f.seedCredential("op-adopt-seed-1", adoptCredID2, adoptOwner2, f.key.KeyID, f.key)
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-adopt-seed-1")
		f.writeConfig(adoptKeyLine(f.keyPath))

		report, err := f.adopt("")
		require.NoError(t, err)
		require.Equal(t, 1, report.Case)
		require.Equal(t, 2, report.ManagedCiphertexts)
		require.True(t, report.CiphertextsVerified)
		require.Zero(t, report.CiphertextsUnreadable)
		require.Empty(t, report.FirstFailure)
		require.Equal(t, string(core.SecretStateReady), report.State)
		require.Equal(t, string(SecretsActionReady), report.Action)
		_, present := f.roster()
		require.True(t, present)
	})

	t.Run("one_ciphertext_unreadable", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.seedOperation("op-adopt-seed-1")
		// 第二条的 key_id 与主密钥相符，但密文由**另一把** 32 字节密钥封装：
		// 只比 key_id 的实现会判它可解，逐条真解才拦得住。
		otherKey := MasterKey{KeyID: f.key.KeyID, Key: bytes.Repeat([]byte{0x9}, masterKeyBytes)}
		f.seedCredential("op-adopt-seed-1", adoptCredID1, adoptOwner1, f.key.KeyID, f.key)
		f.seedCredential("op-adopt-seed-1", adoptCredID2, adoptOwner2, f.key.KeyID, otherKey)
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-adopt-seed-1")
		f.writeConfig(adoptKeyLine(f.keyPath))

		before := f.snapshot()
		report, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeRecoveryRequired, before)

		require.Equal(t, 1, report.Case)
		require.Equal(t, 1, report.CiphertextsUnreadable, "不可解条数必须如实计数")
		require.NotEmpty(t, report.FirstFailure)
		require.False(t, report.CiphertextsVerified)
		require.NotContains(t, report.FirstFailure, "sk-adopt-plaintext", "失败描述不得带出明文")
	})
}

// TestInstanceAdopt_RecordWithoutCiphertextChecksKeyOnly：案例 2 只核密钥与记录。
// 中文验收点：密钥与记录相符 → 成功；`key_id` 不符 → `recovery_required` 且零写入。
func TestInstanceAdopt_RecordWithoutCiphertextChecksKeyOnly(t *testing.T) {
	t.Run("matching_key", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.seedOperation("op-adopt-seed-1")
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-adopt-seed-1")
		f.writeConfig(adoptKeyLine(f.keyPath))

		report, err := f.adopt("")
		require.NoError(t, err)
		require.Equal(t, 2, report.Case)
		require.Zero(t, report.ManagedCiphertexts)
		require.True(t, report.KeyPresent)
		require.Equal(t, f.key.KeyID, report.KeyID)
		require.Equal(t, string(core.SecretStateReady), report.State)
		_, present := f.roster()
		require.True(t, present)
	})

	t.Run("key_id_mismatch", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.seedOperation("op-adopt-seed-1")
		f.seedRecord("k_someone_else", f.fingerprint(), "op-adopt-seed-1")
		f.writeConfig(adoptKeyLine(f.keyPath))

		before := f.snapshot()
		report, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeRecoveryRequired, before)
		require.Equal(t, 2, report.Case)
		require.Equal(t, "k_someone_else", report.RecordKeyID)
	})
}

// TestInstanceAdopt_WithoutRecordInventoriesKeyFiles：案例 4 的两种形态。
// 中文验收点：盘上有原密钥 → 命令结束仍 `uninitialized` / `adopt`、记录 0，且密钥文件
// **不消费、不搬移**；盘上没有密钥 → `await_init`、记录 0。
func TestInstanceAdopt_WithoutRecordInventoriesKeyFiles(t *testing.T) {
	t.Run("key_file_present", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig(adoptKeyLine(f.keyPath))
		original, err := os.ReadFile(f.keyPath)
		require.NoError(t, err)

		report, err := f.adopt("")
		require.NoError(t, err)
		require.Equal(t, 4, report.Case)
		require.Equal(t, string(core.SecretStateUninitialized), report.State)
		require.Equal(t, string(SecretsActionAdopt), report.Action)
		require.True(t, report.KeyPresent)
		require.Equal(t, f.keyPath, report.KeyFile)
		require.Zero(t, report.ManagedCiphertexts)
		require.False(t, report.WroteSecretsRecord)

		// 命令边界：只写名册。密钥文件原地未动、记录仍为 0。
		after, err := os.ReadFile(f.keyPath)
		require.NoError(t, err)
		require.Equal(t, original, after, "采纳不得改写 / 搬移原密钥")
		require.Equal(t, 0, f.rowCount(`SELECT count(*) FROM instance_secrets`))
		_, present := f.roster()
		require.True(t, present)
	})

	t.Run("key_file_absent", func(t *testing.T) {
		f := newAdoptFixture(t)
		require.NoError(t, os.Remove(f.keyPath))
		f.writeConfig(adoptKeyLine(f.keyPath))

		report, err := f.adopt("")
		require.NoError(t, err)
		require.Equal(t, 4, report.Case)
		require.Equal(t, string(SecretsActionAwaitInit), report.Action)
		require.False(t, report.KeyPresent)
		require.Zero(t, f.rowCount(`SELECT count(*) FROM instance_secrets`))
	})
}

// TestInstanceAdopt_RequiresWorkspaceLock：采纳必须取 `basalt.lock`，取不到即拒绝且零写入。
func TestInstanceAdopt_RequiresWorkspaceLock(t *testing.T) {
	f := newAdoptFixture(t)
	f.writeConfig()

	held, err := workspace.AcquireInstanceLock(f.ws)
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Release()) }()

	before := f.snapshot()
	report, err := f.adopt("")
	f.requireAdoptRefusal(err, adoptCodeWorkspaceBusy, before)
	require.Empty(t, report.InstanceID)

	// 释放后可正常采纳：拒绝只因"锁被持有"，不是配置或库的问题。
	require.NoError(t, held.Release())
	_, err = f.adopt("")
	require.NoError(t, err)
	_, present := f.roster()
	require.True(t, present)
}

// TestInstanceAdopt_TokenModeRefusesWithoutExplicitToken：token 模式且新私密目录里没有
// `admin-token` 时不得写名册——否则"采纳成功但起不来"。
// 中文验收点：无 token → `admin_token_source_missing`；给出 `${VAR}` 且可解析 → 成功。
func TestInstanceAdopt_TokenModeRefusesWithoutExplicitToken(t *testing.T) {
	t.Run("token_missing", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeRawConfig(adoptConfigYAML("token", ""))

		before := f.snapshot()
		_, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeAdminTokenMissing, before)
	})

	t.Run("token_resolved", func(t *testing.T) {
		f := newAdoptFixture(t)
		t.Setenv("BASALT_TEST_ADMIN_TOKEN", "test-token-value")
		f.writeRawConfig(adoptConfigYAML("token", "${BASALT_TEST_ADMIN_TOKEN}"))

		_, err := f.adopt("")
		require.NoError(t, err)
		_, present := f.roster()
		require.True(t, present)
	})
}

// TestInstanceAdopt_PendingRecoveryVerifiesOutcome：崩溃恢复只按**名册**查证，不重做。
// 中文验收点：名册不存在 → `not_applied` / failed；完整候选 ID 或摘要相等 → `applied` /
// success；解析失败 / 两键皆缺 → `unknown`，不得假定操作行里一定有原值。
func TestInstanceAdopt_PendingRecoveryVerifiesOutcome(t *testing.T) {
	const crashedOp = "op-adopt-crashed"
	candidate := "inst_0123456789abcdef"

	fullDetail := func() []byte {
		raw, err := json.Marshal(instanceAdoptDetail{
			CandidateInstanceID:       candidate,
			CandidateInstanceIDSHA256: adoptInstanceIDSHA256(candidate),
			Case:                      4,
		})
		require.NoError(t, err)
		return raw
	}

	t.Run("roster_absent_means_not_applied", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		f.seedPendingAdopt(crashedOp, fullDetail())

		report, err := f.adopt("")
		require.NoError(t, err)
		require.True(t, report.RecoveredPending, "本次必须报告回填了 pending 行")

		recovered := f.operation(crashedOp)
		require.Equal(t, core.OperationFailed, recovered.Status)
		require.Equal(t, core.VerificationNotApplied, recovered.Verification)
		require.False(t, recovered.RecoveredAt.IsZero(), "回填必须记录 recovered_at")

		// 归还不是重做：本次采纳另起一条新的 operation_id，旧行终态不变。
		require.NotEqual(t, crashedOp, report.AuditOperationID)
		require.Equal(t, 1, f.rowCount(
			`SELECT count(*) FROM management_operations WHERE action = ? AND status = 'success'`,
			core.ActionInstanceAdopt))
	})

	t.Run("full_candidate_id_means_applied", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		f.seedPendingAdopt(crashedOp, fullDetail())
		f.writeRoster(candidate)

		before := f.snapshot()
		report, err := f.adopt("")
		f.requireAdoptRefusalRecovered(err, adoptCodeRosterExists, before)
		require.True(t, report.RecoveredPending)

		recovered := f.operation(crashedOp)
		require.Equal(t, core.OperationSuccess, recovered.Status)
		require.Equal(t, core.VerificationApplied, recovered.Verification)
	})

	t.Run("digest_only_compares_by_digest", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		raw, err := json.Marshal(instanceAdoptDetail{
			CandidateInstanceIDSHA256: adoptInstanceIDSHA256(candidate),
			Case:                      4,
		})
		require.NoError(t, err)
		f.seedPendingAdopt(crashedOp, raw)
		f.writeRoster(candidate)

		before := f.snapshot()
		_, err = f.adopt("")
		f.requireAdoptRefusalRecovered(err, adoptCodeRosterExists, before)

		recovered := f.operation(crashedOp)
		require.Equal(t, core.OperationSuccess, recovered.Status)
		require.Equal(t, core.VerificationApplied, recovered.Verification)
	})

	t.Run("unparsable_detail_is_indeterminate", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		// 合法 JSON、但不是对象：`BeginOperation` 只校验 JSON 形态，形状错误留到
		// 查证时按"解析失败"处理。
		f.seedPendingAdopt(crashedOp, []byte(`"not-an-object"`))
		f.writeRoster("inst_0123456789abcdef")

		before := f.snapshot()
		_, err := f.adopt("")
		f.requireAdoptRefusalRecovered(err, adoptCodeRosterExists, before)

		recovered := f.operation(crashedOp)
		require.Equal(t, core.OperationUnknown, recovered.Status)
		require.Equal(t, core.VerificationIndeterminate, recovered.Verification)
	})

	t.Run("missing_candidate_keys_is_indeterminate", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		f.seedPendingAdopt(crashedOp, []byte(`{}`))
		f.writeRoster("inst_0123456789abcdef")

		before := f.snapshot()
		_, err := f.adopt("")
		f.requireAdoptRefusalRecovered(err, adoptCodeRosterExists, before)

		recovered := f.operation(crashedOp)
		require.Equal(t, core.OperationUnknown, recovered.Status)
		require.Equal(t, core.VerificationIndeterminate, recovered.Verification)
	})
}

// TestInstanceAdopt_LeavesSecretsRecordUntouched：采纳不写 `instance_secrets`，也不冒充
// `system.secrets_init`。中文验收点：命令结束记录数 0；启动判定仍为 `adopt`（即启动侧
// 才会补写记录），且命令自己一条都没写。
func TestInstanceAdopt_LeavesSecretsRecordUntouched(t *testing.T) {
	f := newAdoptFixture(t)
	f.writeConfig(adoptKeyLine(f.keyPath))

	_, err := f.adopt("")
	require.NoError(t, err)

	require.Zero(t, f.rowCount(`SELECT count(*) FROM instance_secrets`), "采纳只写名册")
	require.Zero(t, f.rowCount(
		`SELECT count(*) FROM management_operations WHERE action = ?`, "system.secrets_init"),
		"采纳不得冒充 system.secrets_init")
	require.Equal(t, 1, f.rowCount(
		`SELECT count(*) FROM management_operations WHERE action = ?`, core.ActionInstanceAdopt))

	// 启动侧重判：案例 4 + 原密钥 → `adopt`（补写记录是启动侧的动作，不是命令的）。
	decision, count := f.startupDecision()
	require.Equal(t, 0, count)
	require.Equal(t, core.SecretStateUninitialized, decision.State)
	require.Equal(t, SecretsActionAdopt, decision.Action)
}

// TestInstanceAdopt_PersistsKeyPathAcrossRestart：采纳只读配置、不代写；重启后仍按同一路径
// 判定。中文验收点：案例 4 → `adopt`、记录 0、配置与密钥逐字节不变；案例 1 / 2 启动 `ready`。
func TestInstanceAdopt_PersistsKeyPathAcrossRestart(t *testing.T) {
	t.Run("case4_restart_adopts", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig(adoptKeyLine(f.keyPath))
		configBefore, err := os.ReadFile(f.configPath)
		require.NoError(t, err)
		keyBefore, err := os.ReadFile(f.keyPath)
		require.NoError(t, err)

		_, err = f.adopt("")
		require.NoError(t, err)

		configAfter, err := os.ReadFile(f.configPath)
		require.NoError(t, err)
		require.Equal(t, configBefore, configAfter, "采纳不得改写配置")
		keyAfter, err := os.ReadFile(f.keyPath)
		require.NoError(t, err)
		require.Equal(t, keyBefore, keyAfter, "采纳不得复制 / 搬移密钥")

		decision, count := f.startupDecision()
		require.Zero(t, count)
		require.Equal(t, SecretsActionAdopt, decision.Action)

		// 新私密目录里没有 master.key：密钥只有配置里那一个来源。
		marker, present := f.roster()
		require.True(t, present)
		privateDir := filepath.Join(f.dataRoot, instanceDataDirName, instancesDirName, marker.InstanceID)
		_, statErr := os.Stat(filepath.Join(privateDir, masterKeyFileName))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("case1_restart_ready", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.seedOperation("op-adopt-seed-1")
		f.seedCredential("op-adopt-seed-1", adoptCredID1, adoptOwner1, f.key.KeyID, f.key)
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-adopt-seed-1")
		f.writeConfig(adoptKeyLine(f.keyPath))

		_, err := f.adopt("")
		require.NoError(t, err)

		decision, count := f.startupDecision()
		require.Equal(t, 1, count)
		require.Equal(t, core.SecretStateReady, decision.State)
		require.Equal(t, SecretsActionReady, decision.Action)
	})

	t.Run("case2_restart_ready", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.seedOperation("op-adopt-seed-1")
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-adopt-seed-1")
		f.writeConfig(adoptKeyLine(f.keyPath))

		_, err := f.adopt("")
		require.NoError(t, err)

		decision, count := f.startupDecision()
		require.Zero(t, count)
		require.Equal(t, core.SecretStateReady, decision.State)
	})
}

// TestInstanceAdopt_RefusesTransientKeyPath：三条"路径不可持久"的部署一律拒绝，同一测试内
// 逐例断言零写入。中文验收点：只给参数 → `master_key_path_unpersisted`；配置相对路径 →
// `master_key_path_relative`；配置与参数不一致 → `master_key_path_mismatch`。
func TestInstanceAdopt_RefusesTransientKeyPath(t *testing.T) {
	t.Run("parameter_only", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig() // 配置里没有 secrets 段

		before := f.snapshot()
		_, err := f.adopt(f.keyPath)
		f.requireAdoptRefusal(err, adoptCodeKeyPathUnpersisted, before)
	})

	t.Run("relative_config_path", func(t *testing.T) {
		f := newAdoptFixture(t)
		// 相对路径按**配置文件所在目录**换算：盘上确实有这把钥，但重启换个 cwd 就指向别处。
		f.writeConfig(adoptKeyLine(filepath.Join("private", masterKeyFileName)))

		before := f.snapshot()
		_, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeKeyPathRelative, before)
	})

	t.Run("parameter_mismatch", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig(adoptKeyLine(f.keyPath))
		otherDir := filepath.Join(f.root, "other-private")
		otherKeyPath := writeMasterKeyFile(t, otherDir, f.key.KeyID, f.key.Key)

		before := f.snapshot()
		_, err := f.adopt(otherKeyPath)
		f.requireAdoptRefusal(err, adoptCodeKeyPathMismatch, before)
	})
}

// TestInstanceAdopt_RestartWithoutPersistedPathFailsClosed：持久化路径被移除后启动必须
// 失败关闭，不得静默铸新钥、不得把存量实例当新实例补写。
// 中文验收点：案例 1 / 2 → `recovery_required`；案例 4 → `await_init` 且记录数保持 0、
// 没有新 `system.secrets_init`；`auto_init:true` 也不得铸钥。
func TestInstanceAdopt_RestartWithoutPersistedPathFailsClosed(t *testing.T) {
	// 去掉持久化路径的配置：`auto_init: false` 是"纯 env"的显式声明。
	withoutPath := adoptConfigYAML("local_trust", "", "auto_init: false")

	t.Run("case1_records_require_recovery", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.seedOperation("op-adopt-seed-1")
		f.seedCredential("op-adopt-seed-1", adoptCredID1, adoptOwner1, f.key.KeyID, f.key)
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-adopt-seed-1")
		f.writeConfig(adoptKeyLine(f.keyPath))
		_, err := f.adopt("")
		require.NoError(t, err)

		f.writeRawConfig(withoutPath)
		decision, _ := f.startupDecision()
		require.Equal(t, core.SecretStateRecoveryRequired, decision.State)
		require.Equal(t, core.SecretRecoveryKeyPathMissing, decision.Reason)
		require.Equal(t, SecretsActionRecover, decision.Action)
	})

	t.Run("case2_records_require_recovery", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.seedOperation("op-adopt-seed-1")
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-adopt-seed-1")
		f.writeConfig(adoptKeyLine(f.keyPath))
		_, err := f.adopt("")
		require.NoError(t, err)

		f.writeRawConfig(withoutPath)
		decision, _ := f.startupDecision()
		require.Equal(t, core.SecretStateRecoveryRequired, decision.State)
		require.Equal(t, SecretsActionRecover, decision.Action)
	})

	t.Run("case4_stays_await_init_and_mints_nothing", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig()
		_, err := f.adopt("")
		require.NoError(t, err)
		marker, present := f.roster()

		// 名册已在：`auto_init` 缺省为 true，仍不得把存量库当新实例补写 / 铸钥。
		decision, count := f.startupDecision()
		require.Zero(t, count)
		require.Equal(t, core.SecretStateUninitialized, decision.State)
		require.Equal(t, SecretsActionAwaitInit, decision.Action)
		require.Zero(t, f.rowCount(`SELECT count(*) FROM instance_secrets`))
		require.Zero(t, f.rowCount(
			`SELECT count(*) FROM management_operations WHERE action = ?`, "system.secrets_init"))

		// 缺省路径指向的私密目录里没有 master.key：没有铸钥。
		if present {
			privateDir := filepath.Join(f.dataRoot, instanceDataDirName, instancesDirName, marker.InstanceID)
			_, statErr := os.Stat(filepath.Join(privateDir, masterKeyFileName))
			require.ErrorIs(t, statErr, os.ErrNotExist)
		}
	})
}

// ─────────────── 迁移准备模式：配置哈希四值（T074 / §2.12.0b / C-51） ───────────────
//
// 迁移准备副本的唯一漂移基线是 `adopt-legacy` 审计里的 `backup_config_sha256`（它必须等于
// 源端 `source_config_sha256`）；`instance adopt` 只**读**该基线与
// `config_without_key_path_sha256`，产出 `target_config_sha256`，不得回写前三个。
// 缺引用 / 越界修改 / 缺证据一律拒绝且零写入——受控调整是流程凭据，不是口头声明。
//
// The migration copy's only drift baseline is the adopt-legacy backup_config_sha256;
// this command reads it and produces target_config_sha256, never the other way round.

// legacyAdoptSeedOpID 是迁移准备副本里 `instance.secrets_legacy_adopt` 审计行的固定标识。
const legacyAdoptSeedOpID = "op-legacy-adopt-seed-1"

// seedLegacyAdoptEvidence 造出迁移准备副本的证据链：legacy-adopt 审计（含配置基线）+
// 指向它的主密钥记录 + 一条可解的托管密文（案例 1）。返回造数时的整文件哈希
// （= `backup_config_sha256` = `copy_config_sha256` = `source_config_sha256`）。
// seedLegacyAdoptEvidence builds the migration-copy evidence chain and returns the
// whole-file config hash recorded as the backup baseline.
func (f *adoptFixture) seedLegacyAdoptEvidence(status core.OperationStatus) string {
	f.t.Helper()
	raw, err := os.ReadFile(f.configPath)
	require.NoError(f.t, err)
	backupHash := hexSHA256(raw)
	masked, err := configMaskedSHA256(f.configPath)
	require.NoError(f.t, err)
	detail, err := json.Marshal(map[string]any{
		"source_config_sha256":           backupHash,
		"backup_config_sha256":           backupHash,
		"copy_config_sha256":             backupHash,
		"config_without_key_path_sha256": masked,
		"record_origin":                  "attested_legacy",
	})
	require.NoError(f.t, err)
	f.withWritableDB(func(db *storage.DB) {
		ctx := context.Background()
		store := storage.NewManagementStore(db)
		require.NoError(f.t, store.BeginOperation(ctx, core.OperationRecord{
			OperationID:  legacyAdoptSeedOpID,
			Action:       core.ActionInstanceSecretsLegacyAdopt,
			TargetKind:   core.TargetKindInstance,
			TargetID:     core.InstanceTargetMasterKey,
			ActorSubject: "cli:test",
			AuthMethod:   "cli",
			Source:       "cli",
			Status:       core.OperationPending,
			DetailJSON:   detail,
			CreatedAt:    baselineClock(),
		}))
		if status != core.OperationPending {
			require.NoError(f.t, store.FinishOperation(ctx, legacyAdoptSeedOpID, status, "", "", nil, baselineClock()))
		}
	})
	return backupHash
}

// seedMigrationCopy 是迁移准备副本夹具：配置 + 证据链 + 记录 + 密文（案例 1）。
func seedMigrationCopy(t *testing.T) (*adoptFixture, string) {
	t.Helper()
	f := newAdoptFixture(t)
	f.writeConfig(adoptKeyLine(f.keyPath))
	backupHash := f.seedLegacyAdoptEvidence(core.OperationSuccess)
	f.seedCredential(legacyAdoptSeedOpID, adoptCredID1, adoptOwner1, f.key.KeyID, f.key)
	f.seedRecord(f.key.KeyID, f.fingerprint(), legacyAdoptSeedOpID)
	return f, backupHash
}

// relocateKey 是**受控调整**允许的唯一动作：把原密钥放到新的绝对路径，只改
// `secrets.master_key_file` 一个键（`adoptConfigYAML` 确定性渲染，其余字节不变）。
// relocateKey is the one permitted controlled adjustment: the key moves, only the
// key-path scalar changes.
func (f *adoptFixture) relocateKey() string {
	f.t.Helper()
	newPath := writeMasterKeyFile(f.t, filepath.Join(f.root, "private-relocated"), f.key.KeyID, f.key.Key)
	f.writeConfig(adoptKeyLine(newPath))
	return newPath
}

// configHash 是同级部署配置的整文件 SHA-256（与生产侧 `hexSHA256` 同一口径）。
func (f *adoptFixture) configHash() string {
	f.t.Helper()
	raw, err := os.ReadFile(f.configPath)
	require.NoError(f.t, err)
	return hexSHA256(raw)
}

// adoptWithRef 发起一次带 `--config-change-ref` 的采纳。
func (f *adoptFixture) adoptWithRef(ref string) (cli.InstanceAdoptReport, error) {
	f.t.Helper()
	port := instanceAdoptPort{Now: baselineClock}
	return port.Adopt(context.Background(), cli.InstanceAdoptRequest{
		Workspace:       f.ws,
		ConfigPath:      f.configPath,
		ConfigChangeRef: ref,
	})
}

// adoptAuditDetail 读回本次采纳审计的 `detail_json`。
func (f *adoptFixture) adoptAuditDetail(operationID string) map[string]any {
	f.t.Helper()
	op := f.operation(operationID)
	var detail map[string]any
	require.NoError(f.t, json.Unmarshal(op.DetailJSON, &detail))
	return detail
}

// TestInstanceAdopt_MigrationConfigHashes 钉住迁移准备模式的配置核对（T074）。
//
// 中文验收点：未调整 → `target_config_sha256` = 基线、`config_adjusted=false`；
// 调整过但没带变更引用 → `config_changed_unattested`（exit 1、零写入）；
// 带引用但越界改了别的键 → `config_change_out_of_scope`（exit 1、零写入）；
// 只改密钥路径 + 带引用 → 通过并回填 `target_config_sha256` / `config_change_ref` /
// `config_adjusted=true`，且 adopt-legacy 审计的 `backup_config_sha256` 不被覆盖；
// legacy-adopt 审计非终态 → `legacy_adopt_evidence_missing`（exit 1、零写入）。
func TestInstanceAdopt_MigrationConfigHashes(t *testing.T) {
	t.Run("unchanged_records_target_hash", func(t *testing.T) {
		f, backupHash := seedMigrationCopy(t)
		require.Equal(t, backupHash, f.configHash())

		report, err := f.adopt("")
		require.NoError(t, err)
		require.True(t, report.ConfigChecked, "迁移准备副本必须核对配置")
		require.Equal(t, backupHash, report.TargetConfigSHA256)
		require.False(t, report.ConfigAdjusted)
		require.Empty(t, report.ConfigChangeRef)

		detail := f.adoptAuditDetail(report.AuditOperationID)
		require.Equal(t, backupHash, detail["target_config_sha256"])
		require.Equal(t, false, detail["config_adjusted"], "未调整也必须显式记 false")
		_, hasRef := detail["config_change_ref"]
		require.False(t, hasRef, "未调整时不得写变更引用")
		require.Equal(t, float64(1), detail["case"], "预写字段必须原样带上（终态是整份替换）")
	})

	t.Run("unattested_refused", func(t *testing.T) {
		f, _ := seedMigrationCopy(t)
		f.relocateKey()

		before := f.snapshot()
		_, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeConfigChangedUnattested, before)
	})

	t.Run("out_of_scope_refused", func(t *testing.T) {
		f, _ := seedMigrationCopy(t)
		newPath := f.relocateKey()
		// 越界：密钥路径以外的键也被改了（受控调整只允许改密钥路径）。
		f.writeRawConfig(adoptConfigYAML("local_trust", "", adoptKeyLine(newPath)) + "log:\n  level: debug\n")

		before := f.snapshot()
		_, err := f.adoptWithRef("CHG-2026-043")
		f.requireAdoptRefusal(err, adoptCodeConfigChangeOutOfScope, before)
	})

	t.Run("adjusted_accepted", func(t *testing.T) {
		f, backupHash := seedMigrationCopy(t)
		f.relocateKey()
		targetHash := f.configHash()
		require.NotEqual(t, backupHash, targetHash, "整文件哈希必须真的变了")

		report, err := f.adoptWithRef("CHG-2026-042")
		require.NoError(t, err)
		require.True(t, report.ConfigChecked)
		require.True(t, report.ConfigAdjusted)
		require.Equal(t, "CHG-2026-042", report.ConfigChangeRef)
		require.Equal(t, targetHash, report.TargetConfigSHA256)

		detail := f.adoptAuditDetail(report.AuditOperationID)
		require.Equal(t, targetHash, detail["target_config_sha256"])
		require.Equal(t, "CHG-2026-042", detail["config_change_ref"])
		require.Equal(t, true, detail["config_adjusted"])

		// 源端 / 备份 / 副本三值不被目标哈希覆盖：adopt-legacy 审计原样保留。
		legacy := f.operation(legacyAdoptSeedOpID)
		var legacyDetail map[string]any
		require.NoError(t, json.Unmarshal(legacy.DetailJSON, &legacyDetail))
		require.Equal(t, backupHash, legacyDetail["backup_config_sha256"])
		require.Equal(t, backupHash, legacyDetail["copy_config_sha256"])
		require.Equal(t, backupHash, legacyDetail["source_config_sha256"])
	})

	for _, status := range []core.OperationStatus{core.OperationPending, core.OperationFailed} {
		t.Run("evidence_missing_"+string(status), func(t *testing.T) {
			f := newAdoptFixture(t)
			f.writeConfig(adoptKeyLine(f.keyPath))
			f.seedLegacyAdoptEvidence(status)
			f.seedCredential(legacyAdoptSeedOpID, adoptCredID1, adoptOwner1, f.key.KeyID, f.key)
			f.seedRecord(f.key.KeyID, f.fingerprint(), legacyAdoptSeedOpID)

			before := f.snapshot()
			_, err := f.adopt("")
			f.requireAdoptRefusal(err, adoptCodeLegacyEvidenceMissing, before)
		})
	}

	t.Run("record_without_origin_operation_refused", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig(adoptKeyLine(f.keyPath))
		// 记录指向一条不存在的操作行：来源不可证明，不得跳过核对。
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-missing-origin-1")

		before := f.snapshot()
		_, err := f.adopt("")
		f.requireAdoptRefusal(err, adoptCodeLegacyEvidenceMissing, before)
	})

	t.Run("plain_record_skips_config_check", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.writeConfig(adoptKeyLine(f.keyPath))
		f.seedOperation("op-adopt-seed-1")
		f.seedCredential("op-adopt-seed-1", adoptCredID1, adoptOwner1, f.key.KeyID, f.key)
		f.seedRecord(f.key.KeyID, f.fingerprint(), "op-adopt-seed-1")
		// 普通存量实例：记录来自 init / 其他动作，改造密钥路径不属于迁移准备模式的判据。
		f.relocateKey()

		report, err := f.adopt("")
		require.NoError(t, err)
		require.False(t, report.ConfigChecked, "非迁移准备副本不做配置核对")
		detail := f.adoptAuditDetail(report.AuditOperationID)
		_, hasTarget := detail["target_config_sha256"]
		require.False(t, hasTarget, "普通采纳不写迁移配置字段")
	})
}
