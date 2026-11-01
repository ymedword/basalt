package app

// 本文件判 `secrets recover-record`（D-40 记录恢复，颗粒度文档 §2.12.0a）的端到端行为：
// 从可信备份把 `instance_secrets` 记录写回**离线 SQLite** 工作区，并在任一校验不过时
// 保持零写入。工作区是按当前 schema 建的真库，备份是原生备份集（`basalt.db` +
// `instance.json`）或导出包（`source-snapshot.db` + `manifest.json`），名册与密文都是
// 真字节——没有任何一道校验靠"看起来像"通过。
//
// 覆盖的**逆改点**（每条对应一种真实事故）：
//   - 把备份里的记录当成可信来源直接插入 → 身份 / 记录 / 密钥 / 密文任一不符仍然写记录；
//   - 恢复时把 `initialized_at` 写成恢复时刻 → 凭据历史时间线被抹平（记录来自备份，时间也是）；
//   - 记录的 `operation_id` 不回溯到备份库的成功操作行 → 来源不可证明的记录被当成可信；
//   - 用"解不开"统一当 AAD 漂移（`strings.Contains(first, "AAD")`）→ 真损坏的密文被报成
//     `aad_mismatch`，`ciphertext_undecryptable` 永远不可达；
//   - 名册缺失时凭清单重建身份 → 来源证明被"重建"冒名顶替；
//   - 审计预写失败仍继续写名册 / 记录 → fail-closed 变成 fail-open。
//
// These tests pin the offline record recovery end to end: every refusal happens before
// the first write, the restored record keeps the backup's own `initialized_at`, and the
// roster is either copied verbatim from the backup or not written at all.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

const (
	// recoverSeedOperationID 是夹具里那条完成态 `system.secrets_init` 操作行：备份记录的
	// `operation_id` 指向它，恢复侧的"来源可回溯"校验认的就是这类成功行。
	recoverSeedOperationID = "op-recover-e2e-seed-1"
	// recoverInstanceID 是夹具名册里的实例标识（合法形态：`inst_` + 16 位十六进制）。
	recoverInstanceID = "inst_0123456789abcdef"
	// recoverBaseURL 是托管模型连接的目标地址：`target_binding` 由它重算。
	recoverBaseURL = "https://api.example.com/v1"
	// recoverPlaintext 是被封进密文的合成明文（任何日志 / 报告 / 审计里都不许出现）。
	recoverPlaintext = "sk-recover-e2e-plaintext"
	// pendingRecoverOperationID 是"上次崩溃留下的"恢复操作行标识。
	pendingRecoverOperationID = "op-recover-e2e-pending-1"
)

// recoverRecordInitializedAt 是备份记录的初始化时刻：**早于**恢复时刻 72 小时。
// 两者必须分得开——记录来自备份，写回的是它的时间，不是"现在"。
// recoverRecordInitializedAt is 72h before the recovery clock, so the two cannot be
// confused: the restored record keeps the backup's own timestamp.
func recoverRecordInitializedAt() time.Time {
	return baselineClock().Add(-72 * time.Hour)
}

// recoverFixture 是一座**记录丢失**的 v6 离线工作区 + 一份可信原生备份。
// 工作区里名册、密钥、密文都在，唯独 `instance_secrets` 记录不在——正是案例 3。
// recoverFixture models the case-3 workspace: roster, key and ciphertext present,
// record lost, plus a trusted native backup of the same database.
type recoverFixture struct {
	t          *testing.T
	root       string
	ws         string
	dbPath     string
	configPath string
	key        MasterKey
	keyPath    string
	roster     []byte
	// pristine 是"记录还在"时的库字节：备份的唯一来源。工作区库随后被删掉记录，
	// 因此不能拿工作区现况当备份——否则每个用例的备份都缺记录，校验全落在 ② 上。
	// pristine is the database bytes with the record still in place; backups are cut
	// from it, never from the live (record-less) workspace.
	pristine []byte
	backup   string
	backupDB string
}

// newRecoverFixture 建工作区 → 造库（记录 + 1 条密文）→ 复制成备份 → 删掉工作区记录。
// newRecoverFixture builds the workspace, copies it into a native backup, then deletes
// the workspace record so only the backup still has it.
func newRecoverFixture(t *testing.T) *recoverFixture {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, workspaceDirName)
	require.NoError(t, os.MkdirAll(ws, 0o700))

	key := testMasterKey()
	keyPath := writeMasterKeyFile(t, filepath.Join(root, "private"), key.KeyID, key.Key)
	configPath := filepath.Join(root, configFileName)
	require.NoError(t, os.WriteFile(configPath,
		[]byte(adoptConfigYAML("local_trust", "", adoptKeyLine(keyPath))), 0o600))

	roster, err := marshalInstanceMarker(instanceMarker{
		Version: instanceMarkerVersion, InstanceID: recoverInstanceID,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(ws, instanceMarkerFileName), roster, 0o600))

	f := &recoverFixture{
		t: t, root: root, ws: ws,
		dbPath:     filepath.Join(ws, databaseFileName),
		configPath: configPath,
		key:        key,
		keyPath:    keyPath,
		roster:     roster,
	}
	f.buildWorkspaceDatabase()
	f.rebuildBackup(nil)
	f.dropWorkspaceRecord()
	return f
}

// buildWorkspaceDatabase 建 v6 库：一条成功的 `system.secrets_init` 操作行（凭据行与
// 记录的外键目标）+ 一条 `master_key` 记录 + 一条真封的托管密文。
// buildWorkspaceDatabase writes one init operation, one record and one sealed credential.
func (f *recoverFixture) buildWorkspaceDatabase() {
	t := f.t
	ctx := context.Background()
	created, err := storage.Bootstrap(ctx, f.dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	require.True(t, created, "夹具必须是新建库")

	db, err := storage.OpenWritable(ctx, f.dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	store := storage.NewManagementStore(db)
	require.NoError(t, store.BeginOperation(ctx, core.OperationRecord{
		OperationID:  recoverSeedOperationID,
		Action:       core.ActionSystemSecretsInit,
		TargetKind:   core.TargetKindSystem,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: "migration:startup",
		AuthMethod:   core.AuthMethodMigration,
		Source:       core.AuthMethodMigration,
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}))
	require.NoError(t, store.FinishOperation(ctx, recoverSeedOperationID,
		core.OperationSuccess, "", "", nil, baselineClock()))
	require.NoError(t, db.Close())

	fingerprint, err := MasterKeyFingerprint(f.key)
	require.NoError(t, err)

	raw := openRawSQLite(t, f.dbPath)
	_, err = raw.Exec(
		`INSERT INTO instance_secrets (scope, key_id, fingerprint, initialized_at, operation_id)
		 VALUES ('master_key', ?, ?, ?, ?)`,
		f.key.KeyID, fingerprint, recoverRecordInitializedAt().UnixMilli(), recoverSeedOperationID)
	require.NoError(t, err)
	f.insertSealedCredential(raw, 0)
	_, err = raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	body, err := os.ReadFile(f.dbPath)
	require.NoError(t, err)
	f.pristine = body
}

// credentialIdentity 给出第 i 条凭据的 AAD 骨架与它此刻的目标绑定。
// credentialIdentity returns the AAD plus the target binding it is built from.
func (f *recoverFixture) credentialIdentity(index int) (CredentialAAD, string) {
	ownerUID, credentialID := legacyCredentialIDs(index)
	target, err := CredentialTargetBinding(core.BindingModelConnection, recoverBaseURL, "")
	require.NoError(f.t, err)
	return CredentialAAD{
		OwnerKind:     core.BindingModelConnection,
		OwnerUID:      ownerUID,
		CredentialID:  credentialID,
		VersionNo:     1,
		TargetBinding: target,
	}, target
}

// insertSealedCredential 用给定密钥真封一条托管模型连接凭据（凭据行 + 版本行）。
// 裸 SQL 是因为夹具要精确控制 `key_id` / `target_binding` / 密文字节这三处可篡改面。
// insertSealedCredential writes one sealed credential through raw SQL so the case can
// tamper with key_id / target_binding / ciphertext independently.
func (f *recoverFixture) insertSealedCredential(raw *sql.DB, index int) {
	t := f.t
	aad, target := f.credentialIdentity(index)
	nonce, ciphertext, err := SealCredential(f.key, aad, []byte(recoverPlaintext))
	require.NoError(t, err)
	now := baselineClock().UnixMilli()
	_, err = raw.Exec(
		`INSERT INTO credentials (credential_id, owner_kind, owner_uid, owner_name, state,
		                          current_version, key_id, created_at, updated_at, operation_id)
		 VALUES (?, ?, ?, ?, 'active', 1, ?, ?, ?, ?)`,
		aad.CredentialID, string(core.BindingModelConnection), string(aad.OwnerUID),
		"recover-openai", f.key.KeyID, now, now, recoverSeedOperationID)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO credential_versions (credential_id, version_no, state, key_id,
		                                  target_binding, nonce, ciphertext, created_at,
		                                  updated_at, operation_id)
		 VALUES (?, 1, 'active', ?, ?, ?, ?, ?, ?, ?)`,
		aad.CredentialID, f.key.KeyID, target, nonce, ciphertext, now, now, recoverSeedOperationID)
	require.NoError(t, err)
}

// rebuildBackup 从 pristine 字节重新生成一份原生备份集（库 + 名册）。
// mutate 在落名册之前改备份库——"备份本身不可信"的用例都从这里造。
// rebuildBackup cuts a fresh native backup from the pristine bytes; mutate runs
// against the copy so untrusted-backup cases never touch the live workspace.
func (f *recoverFixture) rebuildBackup(mutate func(*sql.DB)) {
	t := f.t
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "basalt.db")
	require.NoError(t, os.WriteFile(dbPath, f.pristine, 0o600))
	if mutate != nil {
		db := openRawSQLite(t, dbPath)
		mutate(db)
		_, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		require.NoError(t, err)
		require.NoError(t, db.Close())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, instanceMarkerFileName), f.roster, 0o600))
	f.backup = dir
	f.backupDB = dbPath
}

// dropWorkspaceRecord 删掉工作区的 `instance_secrets` 行：名册、密钥、密文原样留下。
// dropWorkspaceRecord removes only the record, leaving roster, key and ciphertext intact.
func (f *recoverFixture) dropWorkspaceRecord() {
	raw := openRawSQLite(f.t, f.dbPath)
	defer func() { require.NoError(f.t, raw.Close()) }()
	_, err := raw.Exec(`DELETE FROM instance_secrets`)
	require.NoError(f.t, err)
	_, err = raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(f.t, err)
}

// recover 走端口执行一次恢复（确认恒为 true：命令面已拒绝缺省形态）。
// recover runs one recovery through the port with the operator's confirmation.
func (f *recoverFixture) recover(kind cli.BackupKind) (cli.RecoverRecordResult, error) {
	return secretsRecoverPort{Now: baselineClock}.RecoverSecretsRecord(context.Background(),
		cli.RecoverRecordRequest{
			Workspace: f.ws,
			Backup:    f.backup,
			Kind:      kind,
			Confirm:   true,
		})
}

// sealed 只读打开工作区，列出全部现存托管密文（断言"不得删改旧密文"用）。
// sealed lists the workspace's sealed credentials through the production read path.
func (f *recoverFixture) sealed() []storage.SealedCredential {
	t := f.t
	ctx := context.Background()
	db, err := storage.OpenReadOnly(ctx, f.dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	sealed, err := storage.NewInstanceSecretsStore(db).SealedCredentials(ctx)
	require.NoError(t, err)
	return sealed
}

// recordCount 数 `instance_secrets` 行。
// recordCount counts the instance_secrets rows.
func (f *recoverFixture) recordCount() int {
	return rawCount(f.t, f.dbPath, "instance_secrets")
}

// recoverOps 数 `instance.secrets_recover` 操作行（任何状态）。
// recoverOps counts the recovery operation rows in any status.
func (f *recoverFixture) recoverOps() int {
	return rawQueryInt(f.t, f.dbPath,
		`SELECT count(*) FROM management_operations WHERE action = ?`, core.ActionInstanceSecretsRecover)
}

// markerPresent 报告工作区名册是否存在。
// markerPresent reports whether the workspace roster exists.
func (f *recoverFixture) markerPresent() bool {
	_, err := os.Stat(filepath.Join(f.ws, instanceMarkerFileName))
	return err == nil
}

// rawString 读一列字符串。
// rawString scans one text column.
func rawString(t *testing.T, path, query string, args ...any) string {
	t.Helper()
	db := openRawSQLite(t, path)
	defer func() { require.NoError(t, db.Close()) }()
	var value string
	require.NoError(t, db.QueryRow(query, args...).Scan(&value))
	return value
}

// TestInstanceSecrets_RecoverRecordFromTrustedBackup 判主链路：可信原生备份 → 记录写回
// → 审计 success → 密文一条不差 → 重跑幂等且不重复写审计。
// TestInstanceSecrets_RecoverRecordFromTrustedBackup pins the happy path and idempotency.
func TestInstanceSecrets_RecoverRecordFromTrustedBackup(t *testing.T) {
	f := newRecoverFixture(t)

	// 恢复前：案例 3 的真实形状——名册在、密钥在、密文在，记录不在。
	require.True(t, f.markerPresent())
	require.Equal(t, 0, f.recordCount(), "夹具必须是记录丢失的恢复态")
	sealedBefore := f.sealed()
	require.Len(t, sealedBefore, 1)
	ciphertextsBefore := ciphertextDigest(sealedBefore)
	require.Zero(t, f.recoverOps())

	result, err := f.recover(cli.BackupKindNativeSet)
	require.NoError(t, err)
	require.Equal(t, "completed", result.FinalState)
	require.True(t, result.RosterPresentBefore)
	require.False(t, result.RosterRestored, "名册本来就在：恢复不得重写它")
	require.Equal(t, hexSHA256(f.roster), result.RosterSHA256)
	require.Equal(t, 1, result.Ciphertexts)
	require.Zero(t, result.CiphertextsUnreadable)
	require.Equal(t, f.key.KeyID, result.RecordKeyID)
	require.Equal(t, recoverInstanceID, result.InstanceID)
	require.NotEmpty(t, result.OperationID)

	// 记录：`key_id` / `fingerprint` / `initialized_at` 来自备份，`operation_id` 是本次恢复。
	require.Equal(t, 1, f.recordCount())
	require.Equal(t, f.key.KeyID, rawString(t, f.dbPath,
		`SELECT key_id FROM instance_secrets WHERE scope = 'master_key'`))
	require.Equal(t, int(recoverRecordInitializedAt().UnixMilli()), rawQueryInt(t, f.dbPath,
		`SELECT initialized_at FROM instance_secrets WHERE scope = 'master_key'`),
		"恢复必须保留备份记录的 initialized_at，不是恢复时刻")
	require.Equal(t, result.OperationID, rawString(t, f.dbPath,
		`SELECT operation_id FROM instance_secrets WHERE scope = 'master_key'`),
		"记录的 operation_id 指向本次恢复操作")

	// 审计：恰好一条成功行，detail_json 记来源与名册事实，且不含明文。
	require.Equal(t, 1, f.recoverOps())
	require.Equal(t, string(core.OperationSuccess), rawString(t, f.dbPath,
		`SELECT status FROM management_operations WHERE operation_id = ?`, result.OperationID))
	detail := rawString(t, f.dbPath,
		`SELECT detail_json FROM management_operations WHERE operation_id = ?`, result.OperationID)
	require.Contains(t, detail, `"record_origin":"trusted_backup"`)
	require.Contains(t, detail, `"roster_restored":false`)
	require.NotContains(t, detail, recoverPlaintext)

	// 密文一条不差，且这把密钥现在能解开全部密文（独立复算，不复用被测路径的结论）。
	require.Equal(t, ciphertextsBefore, ciphertextDigest(f.sealed()))
	unreadable, first, verifyErr := verifySealedCredentials(f.key, f.sealed())
	require.NoError(t, verifyErr)
	require.Zero(t, unreadable, "恢复后全部密文必须可解：%s", first)

	// 重跑幂等：记录已存在直接拒绝，且不新增审计行。
	_, err = f.recover(cli.BackupKindNativeSet)
	requireOfflineRefusal(t, err, recoverCodeRecordExists, "不覆盖既有记录")
	require.Equal(t, 1, f.recoverOps(), "幂等拒绝不得写审计")
}

// TestInstanceSecrets_RecoverRecordRestoresRoster 判名册门与名册恢复：
//   - 工作区无名册时，只有带名册的备份能提供身份；缺名册 / 坏名册一律拒绝且零写入；
//   - 放行时名册逐字复制（不新增字段），中断态重跑不会重写已有名册。
//
// TestInstanceSecrets_RecoverRecordRestoresRoster pins the roster gate and verbatim restore.
func TestInstanceSecrets_RecoverRecordRestoresRoster(t *testing.T) {
	// removeWorkspaceRoster 把工作区变成"库在、名册不在"的存量形状。
	removeWorkspaceRoster := func(t *testing.T, f *recoverFixture) {
		t.Helper()
		require.NoError(t, os.Remove(filepath.Join(f.ws, instanceMarkerFileName)))
		require.False(t, f.markerPresent())
	}

	t.Run("native_backup_without_roster_is_refused", func(t *testing.T) {
		f := newRecoverFixture(t)
		removeWorkspaceRoster(t, f)
		require.NoError(t, os.Remove(filepath.Join(f.backup, instanceMarkerFileName)))

		_, err := f.recover(cli.BackupKindNativeSet)
		requireOfflineRefusal(t, err, recoverCodeBackupRosterMissing, "必须携带名册")
		require.False(t, f.markerPresent(), "拒绝路径不得建立名册")
		require.Equal(t, 0, f.recordCount())
		require.Zero(t, f.recoverOps())
	})

	t.Run("export_package_without_identity_is_refused", func(t *testing.T) {
		f := newRecoverFixture(t)
		removeWorkspaceRoster(t, f)
		// 路线 A：只有快照库与清单；清单的 source_instance_id 为空 = 备份不带身份。
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "source-snapshot.db"), f.pristine, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"),
			[]byte(`{"source_instance_id": ""}`), 0o600))
		f.backup = dir

		_, err := f.recover(cli.BackupKindExportPackage)
		requireOfflineRefusal(t, err, recoverCodeBackupRosterMissing, "恢复无法建立身份")
		require.False(t, f.markerPresent())
		require.Equal(t, 0, f.recordCount())
	})

	t.Run("export_package_manifest_identity_cannot_replace_roster", func(t *testing.T) {
		// D-40：路线 A 的 manifest 标识**不足以**恢复身份——没有可逐字复制的名册字节
		// 就必须拒绝，而不是按标识拼一份名册出来充数。
		// 逆改点：把本用例的拒绝改成"按 manifest 重建名册" → 名册存在性断言红。
		f := newRecoverFixture(t)
		removeWorkspaceRoster(t, f)
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "source-snapshot.db"), f.pristine, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"),
			[]byte(`{"source_instance_id": "inst_manifest_only"}`), 0o600))
		f.backup = dir

		_, err := f.recover(cli.BackupKindExportPackage)
		requireOfflineRefusal(t, err, recoverCodeBackupRosterMissing, "manifest 不足以建立身份")
		require.False(t, f.markerPresent(), "拒绝路径不得凭清单标识拼装名册")
		require.Equal(t, 0, f.recordCount())
		require.Zero(t, f.recoverOps())
	})

	t.Run("corrupt_backup_roster_is_refused", func(t *testing.T) {
		f := newRecoverFixture(t)
		removeWorkspaceRoster(t, f)
		require.NoError(t, os.WriteFile(filepath.Join(f.backup, instanceMarkerFileName),
			[]byte("{not json"), 0o600))

		_, err := f.recover(cli.BackupKindNativeSet)
		requireOfflineRefusal(t, err, recoverCodeBackupRosterInvalid, "名册损坏或标识非法")
		require.False(t, f.markerPresent())
		require.Equal(t, 0, f.recordCount())
	})

	t.Run("missing_roster_is_restored_verbatim", func(t *testing.T) {
		f := newRecoverFixture(t)
		removeWorkspaceRoster(t, f)

		result, err := f.recover(cli.BackupKindNativeSet)
		require.NoError(t, err)
		require.Equal(t, "completed", result.FinalState)
		require.False(t, result.RosterPresentBefore)
		require.True(t, result.RosterRestored)
		require.Equal(t, hexSHA256(f.roster), result.RosterSHA256)

		restored, err := os.ReadFile(filepath.Join(f.ws, instanceMarkerFileName))
		require.NoError(t, err)
		require.Equal(t, f.roster, restored, "名册必须逐字复制，不新增字段、不重排")
		require.Equal(t, 1, f.recordCount())
	})

	t.Run("interrupted_restore_rerun_keeps_roster_bytes", func(t *testing.T) {
		f := newRecoverFixture(t)
		removeWorkspaceRoster(t, f)
		// 中断态：名册已按备份写入、记录未插入，且上次的审计行停在 pending。
		require.NoError(t, os.WriteFile(filepath.Join(f.ws, instanceMarkerFileName), f.roster, 0o600))
		seedPendingRecover(t, f.dbPath)
		require.Equal(t, 0, f.recordCount())

		result, err := f.recover(cli.BackupKindNativeSet)
		require.NoError(t, err)
		require.False(t, result.RosterRestored, "名册已存在：不得重写")
		restored, err := os.ReadFile(filepath.Join(f.ws, instanceMarkerFileName))
		require.NoError(t, err)
		require.Equal(t, f.roster, restored)
		require.Equal(t, 1, f.recordCount())
		// 上次的 pending 行必须拿到终态：记录当时不存在 → 未生效。
		require.Equal(t, string(core.OperationFailed), rawString(t, f.dbPath,
			`SELECT status FROM management_operations WHERE operation_id = ?`, pendingRecoverOperationID))
		require.Equal(t, core.VerificationNotApplied, rawString(t, f.dbPath,
			`SELECT verification FROM management_operations WHERE operation_id = ?`, pendingRecoverOperationID))
		require.Equal(t, "completed", result.FinalState)
	})
}

// seedPendingRecover 在工作区库里留一条 pending 的 `instance.secrets_recover` 操作行。
// seedPendingRecover leaves a pending recovery row behind, as a crash would.
func seedPendingRecover(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	require.NoError(t, storage.NewManagementStore(db).BeginOperation(ctx, core.OperationRecord{
		OperationID:  pendingRecoverOperationID,
		Action:       core.ActionInstanceSecretsRecover,
		TargetKind:   core.TargetKindInstance,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: "cli:operator",
		AuthMethod:   core.AuthMethodCLI,
		Source:       core.AuthMethodCLI,
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}))
}

// TestInstanceSecrets_RecoverRecordRefusesUnprovable 判"来源不可证明"的八类形状：
// 身份不符、记录缺失、记录不可追溯、记录时间缺失、密钥不符、密文损坏、AAD 漂移、
// 备份与当前库不一致。每一类都必须零写入、旧密文不动、密钥文件不被改写。
//
// TestInstanceSecrets_RecoverRecordRefusesUnprovable pins every unprovable-provenance refusal.
func TestInstanceSecrets_RecoverRecordRefusesUnprovable(t *testing.T) {
	cases := []struct {
		name   string
		want   string
		detail string
		mutate func(*testing.T, *recoverFixture)
	}{
		{
			"identity_mismatch", recoverCodeBackupIdentity, "拒绝把别的实例的记录写进来",
			func(t *testing.T, f *recoverFixture) {
				other, err := marshalInstanceMarker(instanceMarker{
					Version: instanceMarkerVersion, InstanceID: "inst_fedcba9876543210",
				})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(
					filepath.Join(f.backup, instanceMarkerFileName), other, 0o600))
			},
		},
		{
			"record_missing", recoverCodeBackupRecordMissing, "备份产于首次成功写记录之前",
			func(t *testing.T, f *recoverFixture) {
				f.rebuildBackup(func(db *sql.DB) {
					_, err := db.Exec(`DELETE FROM instance_secrets`)
					require.NoError(t, err)
				})
			},
		},
		{
			"record_not_traceable", recoverCodeBackupRecordInvalid, "operation_id",
			func(t *testing.T, f *recoverFixture) {
				f.rebuildBackup(func(db *sql.DB) {
					_, err := db.Exec(
						`UPDATE instance_secrets SET operation_id = 'op-never-existed'`)
					require.NoError(t, err)
				})
			},
		},
		{
			"record_time_missing", recoverCodeBackupRecordInvalid, "initialized_at",
			func(t *testing.T, f *recoverFixture) {
				f.rebuildBackup(func(db *sql.DB) {
					_, err := db.Exec(`UPDATE instance_secrets SET initialized_at = 0`)
					require.NoError(t, err)
				})
			},
		},
		{
			"key_record_mismatch", recoverCodeKeyRecordMismatch, "不得生成替代密钥",
			func(t *testing.T, f *recoverFixture) {
				// 部署者换了一把密钥（不同材料）：指纹对不上，恢复不得自造替代密钥。
				other := MasterKey{
					KeyID: "k_otherinstance00000000001",
					Key:   make([]byte, masterKeyBytes),
				}
				path := writeMasterKeyFile(t, filepath.Join(f.root, "replaced"), other.KeyID, other.Key)
				require.NoError(t, os.WriteFile(f.configPath,
					[]byte(adoptConfigYAML("local_trust", "", adoptKeyLine(path))), 0o600))
			},
		},
		{
			"ciphertext_undecryptable", recoverCodeCiphertextUndecryptable, "无法用当前主密钥解开",
			func(t *testing.T, f *recoverFixture) {
				tamperWorkspaceCiphertext(t, f.dbPath)
			},
		},
		{
			"aad_mismatch", recoverCodeAADMismatch, "AAD",
			func(t *testing.T, f *recoverFixture) {
				retargetWorkspaceBinding(t, f.dbPath)
			},
		},
		{
			"ciphertext_mismatch", recoverCodeCiphertextMismatch, "密文行与备份不一致",
			func(t *testing.T, f *recoverFixture) {
				f.rebuildBackup(func(db *sql.DB) {
					_, err := db.Exec(
						`UPDATE credential_versions SET ciphertext = zeroblob(length(ciphertext))`)
					require.NoError(t, err)
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoverFixture(t)
			tc.mutate(t, f)
			// 基线在**篡改之后**取：用例自己改的那一处不算"实现动了密文"，
			// 之后任何再变化都只能是实现写入的。
			ciphertextsBefore := ciphertextDigest(f.sealed())
			keyBefore, err := os.ReadFile(f.keyPath)
			require.NoError(t, err)

			_, err = f.recover(cli.BackupKindNativeSet)
			requireOfflineRefusal(t, err, tc.want, tc.detail)
			require.Equal(t, 0, f.recordCount(), "失败必须零写入")
			require.Zero(t, f.recoverOps(), "校验阶段拒绝不得写审计")
			require.Equal(t, ciphertextsBefore, ciphertextDigest(f.sealed()), "不得删改旧密文")
			keyAfter, err := os.ReadFile(f.keyPath)
			require.NoError(t, err)
			require.Equal(t, keyBefore, keyAfter, "不得生成或改写主密钥文件")
		})
	}
}

// tamperWorkspaceCiphertext 翻转工作区里唯一一条密文的首字节：解密必然失败，但 AAD
// 字段完全没动——这只能报 `ciphertext_undecryptable`，不能报成 AAD 漂移。
// tamperWorkspaceCiphertext flips one ciphertext byte in place, leaving AAD untouched.
func tamperWorkspaceCiphertext(t *testing.T, dbPath string) {
	t.Helper()
	db := openRawSQLite(t, dbPath)
	defer func() { require.NoError(t, db.Close()) }()
	var raw []byte
	require.NoError(t, db.QueryRow(`SELECT ciphertext FROM credential_versions`).Scan(&raw))
	require.NotEmpty(t, raw)
	raw[0] ^= 0xff
	_, err := db.Exec(`UPDATE credential_versions SET ciphertext = ?`, raw)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
}

// retargetWorkspaceBinding 把工作区的 `target_binding` 换成一个**形态合法**的别的绑定：
// AAD 仍能重建，但六段与备份不一致——这是"记录被篡改 / 跨实例拼接"的可判定形状。
// retargetWorkspaceBinding swaps in another well-formed binding so the AAD stays
// rebuildable while its segments no longer match the backup.
func retargetWorkspaceBinding(t *testing.T, dbPath string) {
	t.Helper()
	other, err := CredentialTargetBinding(core.BindingModelConnection,
		"https://api.example.com/v2", "")
	require.NoError(t, err)
	db := openRawSQLite(t, dbPath)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(`UPDATE credential_versions SET target_binding = ?`, other)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
}

// TestInstanceSecrets_RecoverRecordAuditFailClosed 判审计预写门：预写失败即整体放弃——
// 名册不写、记录不写、报告停在 `not_applied`。
// TestInstanceSecrets_RecoverRecordAuditFailClosed proves the pre-write gate is fail-closed.
func TestInstanceSecrets_RecoverRecordAuditFailClosed(t *testing.T) {
	f := newRecoverFixture(t)
	// 名册也一起删掉：如果实现先写名册再预写审计，这条会红在"名册被写了"。
	require.NoError(t, os.Remove(filepath.Join(f.ws, instanceMarkerFileName)))
	blockRecoverAuditPrewrite(t, f.dbPath)

	result, err := f.recover(cli.BackupKindNativeSet)
	requireOfflineRefusal(t, err, adoptCodeAuditPrewrite, "按 fail-closed 不写记录")
	require.Equal(t, "not_applied", result.FinalState)
	require.False(t, f.markerPresent(), "预写失败不得写名册")
	require.Equal(t, 0, f.recordCount(), "预写失败不得插入记录")
}

// blockRecoverAuditPrewrite 让 `instance.secrets_recover` 的审计插入必然失败：用触发器
// 精确挡住那一个 action，其他审计行（夹具的 seed 行）不受影响。
// blockRecoverAuditPrewrite installs a trigger that aborts only the recovery audit insert.
func blockRecoverAuditPrewrite(t *testing.T, dbPath string) {
	t.Helper()
	db := openRawSQLite(t, dbPath)
	defer func() { require.NoError(t, db.Close()) }()
	_, err := db.Exec(
		`CREATE TRIGGER fail_recover_audit BEFORE INSERT ON management_operations
		 WHEN NEW.action = '` + core.ActionInstanceSecretsRecover + `'
		 BEGIN SELECT RAISE(ABORT, 'audit prewrite blocked by test'); END`)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
}
