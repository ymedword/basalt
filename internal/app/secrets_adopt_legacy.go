package app

// 本文件实现 `secrets adopt-legacy` 的离线适配器（颗粒度文档 §3.1.8c / §2.12.0b，
// C-51）：为 schema 3～5 的合法存量在**隔离副本**上准备 `instance_secrets` 记录。
//
// 信任模型是「外部锚点 + 全量重算 + 源端未漂移」，缺一不可：
//
//	锚点（--expect-backup-sha256）来自变更单 / 交接单 / 离线台账，不来自备份自身
//	清单（backup.json）逐项重算——备份内的哈希只是自证，不构成来源证明
//	源端（--source）重算配置与库哈希；源端不可读时才用带外见证编号
//	原密钥与全部密文连续——只比 key_id 是假成功，必须逐条试解
//
// 唯一写入（许可清单见 §3.1.8c「不做」）：副本的 3～5 → 6 迁移、1 条
// `record_origin=attested_legacy` 记录、1 条 `instance.secrets_legacy_adopt` 审计。
// This adapter performs the controlled legacy upgrade on an isolated copy only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// legacyCodeStateNotLegacy：副本不是 schema 3～5（已是 v6 或更高）。
	legacyCodeStateNotLegacy = "state_not_legacy"
	// legacyCodeSchemaUnsupported：副本 schema 低于 3 或高于本二进制。
	legacyCodeSchemaUnsupported = "backup_schema_unsupported"
	// legacyCodeBackupInvalid：备份目录 / 清单 / 副本与备份不一致。
	legacyCodeBackupInvalid = "backup_invalid"
	// legacyCodeRecordAlreadyExists：副本已有 master_key 记录（幂等，先于状态门）。
	legacyCodeRecordAlreadyExists = "record_already_exists"
	// legacyCodeBackupRecordPresent：备份已经带记录——该走 `recover-record`。
	legacyCodeBackupRecordPresent = "backup_record_present"
	// legacyCodeKeyUnavailable：原密钥读不到。
	legacyCodeKeyUnavailable = "key_unavailable"
	// legacyCodeKeyUnverified：取钥来源不可信（覆盖副本无锚点 / 锚点不符 / 指纹不符）。
	legacyCodeKeyUnverified = "key_unverified"
	// legacyCodeAttestationMissing：既没给 --source 也没给带外见证编号。
	legacyCodeAttestationMissing = "attestation_missing"
	// legacyCodeAttestationMismatch：密钥文件与备份清单的 attestation 不符。
	legacyCodeAttestationMismatch = "attestation_mismatch"
	// legacyCodeCiphertextUndecryptable：有密文用原密钥解不开。
	legacyCodeCiphertextUndecryptable = "ciphertext_undecryptable"
	// legacyCodeAADMismatch：AAD 与记录不一致。
	legacyCodeAADMismatch = "aad_mismatch"
	// legacyCodeSourceChanged：源端在备份之后发生漂移。
	legacyCodeSourceChanged = "source_changed"
	// legacyCodeIdentityMismatch：副本名册与备份身份不符（含"副本已建名册"）。
	legacyCodeIdentityMismatch = "backup_identity_mismatch"
	// legacyCodeMigrationFailed：迁移失败（预写后）。
	legacyCodeMigrationFailed = "migration_failed"
	// legacyCodeCiphertextMismatch：副本密文与备份不一致，或迁移改变了密文。
	legacyCodeCiphertextMismatch = "ciphertext_mismatch"
	// legacyCodeRecordInsert：记录插入失败（预写后）。
	legacyCodeRecordInsert = "record_insert_failed"
)

// legacyBackupManifest 是 `backup.json` 的结构（§2.12.0b 字段表，format_version=1）。
// 它只是**待重算的声明**：每个哈希都由本适配器在盘上重算，声明本身不构成证明。
// legacyBackupManifest is only a claim; every hash in it is recomputed on disk.
type legacyBackupManifest struct {
	FormatVersion      int    `json:"format_version"`
	SourceHost         string `json:"source_host"`
	SourceWorkspace    string `json:"source_workspace"`
	SourceStoppedAt    string `json:"source_stopped_at"`
	BackupCreatedAt    string `json:"backup_created_at"`
	Operator           string `json:"operator"`
	SchemaVersion      int    `json:"schema_version"`
	DBSHA256           string `json:"db_sha256"`
	WALSHA256          string `json:"wal_sha256"`
	SHMSHA256          string `json:"shm_sha256"`
	SourceConfigSHA256 string `json:"source_config_sha256"`
	BackupConfigSHA256 string `json:"backup_config_sha256"`
	RosterPresent      bool   `json:"roster_present"`
	RosterSHA256       string `json:"roster_sha256"`
	InstanceID         string `json:"instance_id"`
	KeyFingerprint     string `json:"key_fingerprint"`
	KeyID              string `json:"key_id"`
	KeyFileSHA256      string `json:"key_file_sha256"`
	KeyFileName        string `json:"key_file"`
	CredentialVersions int    `json:"credential_version_count"`
	CiphertextCount    int    `json:"ciphertext_count"`
	RecordsAbsent      bool   `json:"records_absent"`
	ProcedureRef       string `json:"procedure_ref"`
	AnchorRef          string `json:"anchor_ref"`
}

// legacyAdoptPort 实现 cli.LegacySecretsAdopt。无状态：句柄每次调用自建自关。
// legacyAdoptPort is stateless; every call opens and releases its own handles.
type legacyAdoptPort struct {
	Logger *slog.Logger

	// Now 是可注入的时钟；nil 时取 time.Now。
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
}

// logger / clock 保证零值适配器可用。
// logger / clock keep a zero-valued adapter usable.
func (p legacyAdoptPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

func (p legacyAdoptPort) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// refuse 在返回前留一条 WARN（失败路径日志与审计双轨）。
// refuse logs the refusal as a WARN before returning it.
func (p legacyAdoptPort) refuse(result cli.AdoptLegacyResult, err error) error {
	p.logger().Warn("secrets adopt-legacy：拒绝",
		"workspace", result.Workspace, "error", err)
	return err
}

// AdoptLegacySecrets 执行受控升级（§2.12.0b 校验顺序 0～11）。
// AdoptLegacySecrets runs the documented check order 0–11.
func (p legacyAdoptPort) AdoptLegacySecrets(ctx context.Context, req cli.AdoptLegacyRequest) (cli.AdoptLegacyResult, error) {
	result := cli.AdoptLegacyResult{
		Workspace:      req.Workspace,
		BackupPath:     req.Backup,
		ComputedSHA256: req.ManifestSHA256,
		ExpectedSHA256: req.ManifestSHA256,
		FinalState:     "not_applied",
	}
	ws, err := filepath.Abs(strings.TrimSpace(req.Workspace))
	if err != nil || strings.TrimSpace(req.Workspace) == "" {
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"state_not_legacy: --workspace 无法规范化为绝对路径"))
	}
	result.Workspace = ws
	backupDir, absErr := filepath.Abs(strings.TrimSpace(req.Backup))
	if absErr != nil || strings.TrimSpace(req.Backup) == "" {
		return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: --backup 无法规范化为绝对路径"))
	}
	result.BackupPath = backupDir

	// 0：显式确认、源端见证、目标不能是同一个工作区。命令面已判过一遍，这里是
	// 纵深防御——端口也可能被别的入口调用。
	if !req.Confirm {
		return result, p.refuse(result, refuseOffline(legacyCodeAttestationMissing, 2,
			"attestation_missing: 缺少 --confirm——受控升级会迁移副本，必须显式确认"))
	}
	if strings.TrimSpace(req.Source) == "" && strings.TrimSpace(req.AttestSourceUnchanged) == "" {
		return result, p.refuse(result, refuseOffline(legacyCodeAttestationMissing, 2,
			"attestation_missing: 本地可读时用 --source <original-ws>；源端不可读时必须给 --attest-source-unchanged <ticket-ref>"))
	}
	if req.Source != "" {
		src, srcErr := filepath.Abs(strings.TrimSpace(req.Source))
		if srcErr != nil {
			return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
				"--source 无法规范化为绝对路径"))
		}
		if sameFilePath(src, ws) {
			return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
				"source_is_target: --source 与 --workspace 是同一个工作区——受控升级只对隔离副本操作"))
		}
	}

	configPath := filepath.Join(filepath.Dir(ws), configFileName)
	cfg, cfgErr := cli.LoadConfig(configPath, os.LookupEnv)
	switch {
	case cfgErr == nil:
	case errors.Is(cfgErr, fs.ErrNotExist):
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"config_missing: 副本的上级目录里没有 %s——副本必须镜像原 <WorkDir> 的布局", configFileName))
	default:
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"config_invalid: 装载副本配置 %s 失败: %v", configPath, cfgErr))
	}

	dbPath := filepath.Join(ws, databaseFileName)
	info, statErr := os.Stat(dbPath)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"副本 %s 里没有 SQLite 库（%s）", ws, databaseFileName))
	case statErr != nil:
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"检查副本库 %s: %v", dbPath, statErr))
	case info.IsDir():
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"%s 是目录，不是 SQLite 库文件", dbPath))
	}

	result, err = withInstanceLock(ws, func() (cli.AdoptLegacyResult, error) {
		return p.adoptLegacyLocked(ctx, cfg, configPath, dbPath, ws, backupDir, req, result)
	})
	if err != nil {
		var busy *workspace.LockBusyError
		if errors.As(err, &busy) {
			return result, p.refuse(result, refuseOffline(adoptCodeWorkspaceBusy, 2,
				"副本 %s 的实例锁被持有——请先停止该副本上的进程再重跑", ws))
		}
		return result, err
	}
	return result, nil
}

// adoptLegacyLocked 是持锁区间的主体：校验全部通过后才迁移并写记录。
// adoptLegacyLocked validates everything before the migration and the record.
func (p legacyAdoptPort) adoptLegacyLocked(ctx context.Context, cfg *cli.Config, configPath, dbPath, ws, backupDir string, req cli.AdoptLegacyRequest, result cli.AdoptLegacyResult) (cli.AdoptLegacyResult, error) {
	manifest, err := p.verifyBackup(backupDir, req, result)
	if err != nil {
		return result, p.refuse(result, err)
	}
	result.SourceConfigSHA256 = manifest.SourceConfigSHA256
	result.BackupConfigSHA256 = manifest.BackupConfigSHA256
	result.InstanceID = manifest.InstanceID

	// 副本与备份逐字节一致（配置与库两侧都算；只看库哈希会放过配置漂移）。
	copyConfigSHA, err := hashFileString(configPath)
	if err != nil {
		return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 读取副本配置失败: %v", err))
	}
	result.CopyConfigSHA256 = copyConfigSHA
	if copyConfigSHA != manifest.BackupConfigSHA256 {
		return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 副本 basalt.yaml 与备份不一致（copy=%s backup=%s）——"+
				"配置漂移必须在受控流程之外显式处理", copyConfigSHA, manifest.BackupConfigSHA256))
	}
	// `config_without_key_path_sha256`（§2.12.0b 冻结算法）：原始字节、仅掩码密钥路径
	// 标量。它证明"受控调整只改了这一个键"，写进审计供 `instance adopt` 比对；
	// 算不出来就别迁——没有它的采纳记录无法证明后续调整是否越界。
	maskedSHA, maskedErr := configMaskedSHA256(configPath)
	if maskedErr != nil {
		code := configMaskRefusalCode(maskedErr)
		return result, p.refuse(result, refuseOffline(code, 2, "%s: %v", code, maskedErr))
	}
	result.ConfigWithoutKeyPathSHA256 = maskedSHA
	if sha, shaErr := hashFileString(dbPath); shaErr != nil {
		return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 读取副本库失败: %v", shaErr))
	} else if sha != manifest.DBSHA256 {
		return result, p.refuse(result, refuseOffline(legacyCodeCiphertextMismatch, 1,
			"ciphertext_mismatch: 副本库与备份不一致（copy=%s backup=%s）——"+
				"副本必须在备份之后原样复制，不得复用被改过的副本", sha, manifest.DBSHA256))
	}

	// 名册（§3.1.8c 校验 8）：清单声明了就按哈希 + 实例标识核对副本，没声明则副本必须
	// 没有名册——先建名册再采纳会把不在清单里的身份带进副本。本命令**不**写名册。
	// Roster (check 8): a declared roster is verified by hash and id against the copy;
	// an undeclared one must be absent, or the copy carries an unbacked identity.
	marker, copyRosterPresent, rosterErr := readInstanceMarkerAt(ws)
	switch {
	case rosterErr != nil:
		return result, p.refuse(result, refuseOffline(recoverCodeBackupRosterInvalid, 1,
			"backup_roster_invalid: 副本名册不可用: %v", rosterErr))
	case manifest.RosterPresent && !copyRosterPresent:
		return result, p.refuse(result, refuseOffline(legacyCodeIdentityMismatch, 1,
			"backup_identity_mismatch: 清单声明有名册（%s），副本里没有——副本复制不完整",
			manifest.InstanceID))
	case manifest.RosterPresent:
		copyRosterSHA, shaErr := hashFileString(filepath.Join(ws, instanceMarkerFileName))
		if shaErr != nil {
			return result, p.refuse(result, refuseOffline(recoverCodeBackupRosterInvalid, 1,
				"backup_roster_invalid: 读取副本名册失败: %v", shaErr))
		}
		if copyRosterSHA != manifest.RosterSHA256 {
			return result, p.refuse(result, refuseOffline(legacyCodeIdentityMismatch, 1,
				"backup_identity_mismatch: 副本名册与清单不符（copy=%s backup=%s）——"+
					"副本必须原样复制自备份", copyRosterSHA, manifest.RosterSHA256))
		}
		if marker.InstanceID != manifest.InstanceID {
			return result, p.refuse(result, refuseOffline(legacyCodeIdentityMismatch, 1,
				"backup_identity_mismatch: 副本名册的实例标识与清单不符（copy=%s backup=%s）",
				marker.InstanceID, manifest.InstanceID))
		}
		result.RosterPresent = true
	case copyRosterPresent:
		return result, p.refuse(result, refuseOffline(legacyCodeIdentityMismatch, 1,
			"backup_identity_mismatch: 副本已有名册（%s），但备份清单没有——受控升级必须在 "+
				"instance adopt 之前，不得在已采纳的副本上补写记录", marker.InstanceID))
	}
	result.IdentityEvidence = legacyIdentityEvidence(result.RosterPresent)

	// schema 门与记录门（在旧谱系上读：v3～5 没有 instance_secrets 表）。
	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"打开副本库失败: %v", err))
	}
	schemaVersion, schemaErr := db.SchemaVersion(ctx)
	if schemaErr != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"读取副本 schema 版本失败: %v", schemaErr))
	}
	result.SchemaBefore = schemaVersion
	if schemaVersion < 3 || schemaVersion > storage.SupportedSchemaVersion {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeSchemaUnsupported, 2,
			"backup_schema_unsupported: 副本 schema v%d 不在 3～%d 的受控升级范围",
			schemaVersion, storage.SupportedSchemaVersion-1))
	}
	hasSecretsTable, tableErr := db.HasTable(ctx, "instance_secrets")
	if tableErr != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 检查副本记录表失败: %v", tableErr))
	}
	secretsStore := storage.NewInstanceSecretsStore(db)
	if hasSecretsTable {
		// 表在 = 副本已在 v6 谱系上跑过：有记录 → 幂等拒绝（退出 1，先于状态门）；
		// 没记录 → 副本是"部分迁移"的残骸，按状态门拒绝（退出 2，丢弃重来）。
		_, recorded, loadErr := secretsStore.Load(ctx, core.InstanceSecretScopeMasterKey)
		if loadErr != nil {
			_ = db.Close()
			return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
				"backup_invalid: 读取副本初始化记录失败: %v", loadErr))
		}
		_ = db.Close()
		if recorded {
			return result, p.refuse(result, refuseOffline(legacyCodeRecordAlreadyExists, 1,
				"record_already_exists: 副本已有 master_key 记录——本命令不覆盖；"+
					"要重做请从备份重新复制一份新副本"))
		}
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"state_not_legacy: 副本已是 v%d 且没有记录——丢弃副本，从备份重新复制再跑；"+
				"不得在部分迁移的副本上重试", schemaVersion))
	}
	if schemaVersion == storage.SupportedSchemaVersion {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeStateNotLegacy, 2,
			"state_not_legacy: 副本已是 v%d 且没有记录——丢弃副本，从备份重新复制再跑；"+
				"不得在部分迁移的副本上重试", schemaVersion))
	}
	if manifest.SchemaVersion != schemaVersion {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 清单声明 schema v%d，副本实际 v%d", manifest.SchemaVersion, schemaVersion))
	}
	sealedStore := secretsStore
	sealed, sealedErr := sealedStore.SealedCredentials(ctx)
	if sealedErr != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 列出副本密文失败: %v", sealedErr))
	}
	if len(sealed) != manifest.CiphertextCount {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeCiphertextMismatch, 1,
			"ciphertext_mismatch: 副本有 %d 条密文，清单声明 %d 条", len(sealed), manifest.CiphertextCount))
	}
	result.Ciphertexts = len(sealed)

	// 取钥（K1 备份内副本 / K2 外部副本 / 配置路径）与连续校验。
	key, keySource, keyFileSHA, keyErr := p.loadLegacyKey(cfg, backupDir, ws, manifest, req)
	if keyErr != nil {
		_ = db.Close()
		return result, p.refuse(result, keyErr)
	}
	defer key.Zero()
	result.KeySource = keySource
	result.KeyFileSHA256 = keyFileSHA
	result.KeyID = key.KeyID
	fingerprint, fpErr := MasterKeyFingerprint(key)
	if fpErr != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeKeyUnverified, 1,
			"key_unverified: 计算主密钥指纹失败: %v", fpErr))
	}
	result.KeyFingerprint = fingerprint
	if manifest.KeyID != "" && manifest.KeyID != key.KeyID {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeKeyUnverified, 1,
			"key_unverified: 主密钥 key_id 与清单不符（清单 %s，文件 %s）", manifest.KeyID, key.KeyID))
	}
	if manifest.KeyFingerprint != "" && manifest.KeyFingerprint != fingerprint {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeKeyUnverified, 1,
			"key_unverified: 主密钥指纹与清单不符（清单 %s，文件 %s）",
			manifest.KeyFingerprint, fingerprint))
	}

	// AAD 与密文分两步判。表的行里没有 AAD 摘要列，GCM 层分不清"AAD 漂移"与"密文
	// 损坏"，所以先用记录字段重建 AAD（`Validate`）：重建不出 → `aad_mismatch`；
	// 重建得出但解不开 → `ciphertext_undecryptable`。这条切分由本实现定义（§2.12.0b
	// 未固定两者边界），已在 run-state 披露。
	if badAAD, firstAAD := unusableAAD(sealed); badAAD > 0 {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeAADMismatch, 1,
			"aad_mismatch: %d / %d 条密文记录的 AAD 字段无法重建（首个：%s）——拒绝迁移",
			badAAD, len(sealed), firstAAD))
	}
	unreadable, first, verifyErr := verifySealedCredentials(key, sealed)
	if verifyErr != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 核验副本密文失败: %v", verifyErr))
	}
	if unreadable > 0 {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeCiphertextUndecryptable, 1,
			"ciphertext_undecryptable: %d / %d 条密文无法用原密钥解开（首个失败：%s）——拒绝迁移",
			unreadable, len(sealed), first))
	}

	// 源端未漂移：本地可读时重算（必须），否则接受带外见证（报告标 attested，不是已验）。
	result.SourceWitness = "attestation"
	if req.Source != "" {
		result.SourceWitness = "source"
		if err := p.verifySourceUnchanged(req.Source, manifest); err != nil {
			_ = db.Close()
			return result, p.refuse(result, err)
		}
	}

	// 迁移与写记录（唯一写入许可）。
	migrateErr := db.Migrate(ctx, storage.SupportedSchemaVersion)
	if migrateErr != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeMigrationFailed, 3,
			"migration_failed: 副本迁移失败（副本已不可用，请从备份重新复制）: %v", migrateErr))
	}
	result.SchemaAfter = storage.SupportedSchemaVersion

	// 迁移后复核：密文行必须逐字节不变（只改表结构，不改密文）。
	postSealed, postErr := sealedStore.SealedCredentials(ctx)
	if postErr != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeCiphertextMismatch, 3,
			"ciphertext_mismatch: 迁移后读回密文失败: %v", postErr))
	}
	if ciphertextDigest(postSealed) != ciphertextDigest(sealed) || len(postSealed) != len(sealed) {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(legacyCodeCiphertextMismatch, 3,
			"ciphertext_mismatch: 迁移改变了密文行——副本不可信，请从备份重新复制"))
	}

	now := p.clock()
	detail, marshalErr := json.Marshal(legacyAdoptAuditDetail(manifest, result, req.ManifestSHA256))
	if marshalErr != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(adoptCodeAuditPrewrite, 3,
			"序列化审计详情失败: %v", marshalErr))
	}

	store := storage.NewManagementStore(db)
	actor := cliWriteActor(p.logger())
	opID := core.NewOperationID(now)
	if err := store.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       core.ActionInstanceSecretsLegacyAdopt,
		TargetKind:   core.TargetKindInstance,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	}); err != nil {
		_ = db.Close()
		return result, p.refuse(result, refuseOffline(adoptCodeAuditPrewrite, 3,
			"审计预写失败——按 fail-closed 不写记录: %v", err))
	}
	result.OperationID = opID

	record := storage.InstanceSecretRecord{
		Scope:         core.InstanceSecretScopeMasterKey,
		KeyID:         key.KeyID,
		Fingerprint:   fingerprint,
		InitializedAt: now,
		OperationID:   opID,
	}
	if err := sealedStore.Insert(ctx, record); err != nil {
		p.finishOffline(ctx, store, opID, core.OperationFailed, legacyCodeRecordInsert, err)
		_ = db.Close()
		result.FinalState = "failed"
		return result, p.refuse(result, refuseOffline(legacyCodeRecordInsert, 3,
			"record_insert_failed: 写记录失败（操作 %s 已回填 failed）: %v", opID, err))
	}
	if err := store.FinishOperation(ctx, opID, core.OperationSuccess, "", "", nil, now); err != nil {
		p.logger().Error("secrets adopt-legacy：记录已写入，但回填审计终态失败",
			"operation_id", opID, "error", err)
		_ = db.Close()
		result.FinalState = "failed"
		return result, p.refuse(result, refuseOffline(adoptCodeAuditFinish, 3,
			"记录已写入但审计终态回填失败（操作 %s）——重跑会被 record_already_exists 挡住，请按操作号查证: %v", opID, err))
	}
	if closeErr := db.Close(); closeErr != nil {
		p.logger().Warn("secrets adopt-legacy：关闭副本库句柄失败", "error", closeErr)
	}
	result.FinalState = "completed"
	return result, nil
}

// legacyIdentityEvidence 把名册事实映射成 §2.12.0b 的两档身份证据：清单带着名册且
// 哈希 / 实例标识已核对为 `roster`；无名册存量为 `attested_no_roster`——身份来自部署者
// 见证，**不是**密码学证明，报告与审计都不许写成"已证明".
// legacyIdentityEvidence maps the roster fact to the two documented evidence tiers.
func legacyIdentityEvidence(rosterPresent bool) string {
	if rosterPresent {
		return "roster"
	}
	return "attested_no_roster"
}

// legacyAdoptAuditDetail 组装 §3.1.8c「审计」行要求的 `detail_json`：备份路径 / 清单
// 哈希 / 外部锚点编号与备份程序编号 / 源端见证 / schema 前后 / 密钥公开摘要 / 取钥来源
// 与文件哈希 / 密文计数 / 名册状态 / 配置哈希四值 / `record_origin`。
//
// 只含公开摘要与路径：`key_id` / 指纹 / `key_file_sha256` 都是**摘要**，不是密钥材料。
// 这里**不**记密钥字节、密文或明文，也不记 `backup.json` 的原始内容。
// The detail carries public digests only: never key material, ciphertext or plaintext.
func legacyAdoptAuditDetail(manifest legacyBackupManifest, result cli.AdoptLegacyResult, manifestSHA string) map[string]any {
	detail := map[string]any{
		"backup_path":                    result.BackupPath,
		"backup_manifest_sha256":         manifestSHA,
		"anchor_ref":                     manifest.AnchorRef,
		"procedure_ref":                  manifest.ProcedureRef,
		"source_config_sha256":           manifest.SourceConfigSHA256,
		"backup_config_sha256":           manifest.BackupConfigSHA256,
		"copy_config_sha256":             result.CopyConfigSHA256,
		"config_without_key_path_sha256": result.ConfigWithoutKeyPathSHA256,
		"schema_before":                  result.SchemaBefore,
		"schema_after":                   result.SchemaAfter,
		"key_id":                         result.KeyID,
		"record_fingerprint":             result.KeyFingerprint,
		"key_source":                     result.KeySource,
		"key_file_sha256":                result.KeyFileSHA256,
		"ciphertext_count":               result.Ciphertexts,
		"roster_present":                 result.RosterPresent,
		"identity_evidence":              result.IdentityEvidence,
		"source_witness":                 result.SourceWitness,
		"record_origin":                  "attested_legacy",
	}
	// 名册字段成组出现：无册时整组缺席，读审计的人一眼能看出"没有"，而不是去猜空串。
	if result.RosterPresent {
		detail["roster_sha256"] = manifest.RosterSHA256
		detail["instance_id"] = manifest.InstanceID
	}
	return detail
}

// verifyBackup 重算 `backup.json` 声明并核对备份自己的文件：清单与文件不符即
// `backup_invalid`（退出 1）。外部锚点已在命令面核对过，这里再做**纵深防御**的
// 字节比对——两份判据都成立才继续。
// verifyBackup recomputes every declared hash against the backup's own files.
func (p legacyAdoptPort) verifyBackup(backupDir string, req cli.AdoptLegacyRequest, result cli.AdoptLegacyResult) (legacyBackupManifest, error) {
	var manifest legacyBackupManifest
	// 备份目录由命令参数给出：读取经该目录的句柄（`os.Root`），文件名不再拼接。
	// The backup manifest is read through a root handle on the backup directory.
	raw, err := readFileUnder(backupDir, "backup.json")
	if err != nil {
		return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 读取 backup.json 失败: %v", err)
	}
	if computed := hexSHA256(raw); computed != req.ManifestSHA256 {
		return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: backup.json 在命令面核对之后被改动（now=%s checked=%s）",
			computed, req.ManifestSHA256)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: backup.json 不是合法 JSON: %v", err)
	}
	if manifest.FormatVersion != 1 {
		return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: backup.json 的 format_version=%d，本二进制只理解 1", manifest.FormatVersion)
	}
	if !manifest.RecordsAbsent {
		return manifest, refuseOffline(legacyCodeBackupRecordPresent, 1,
			"backup_record_present: 清单声明备份里已有记录——那不是 schema 3～5 的无记录存量，"+
				"记录恢复请走 `secrets recover-record`")
	}
	// 备份 schema 决定路线：低于 3 / 高于 6 没有受控升级语义；6 已在新谱系上（有记录 /
	// 无记录分别走 recover-record / 保持恢复态），本命令都不接。
	switch {
	case manifest.SchemaVersion < 3 || manifest.SchemaVersion > storage.SupportedSchemaVersion:
		return manifest, refuseOffline(legacyCodeSchemaUnsupported, 2,
			"backup_schema_unsupported: 备份清单声明 schema v%d，受控升级只接 v3～v%d",
			manifest.SchemaVersion, storage.SupportedSchemaVersion-1)
	case manifest.SchemaVersion == storage.SupportedSchemaVersion:
		return manifest, refuseOffline(legacyCodeStateNotLegacy, 2,
			"state_not_legacy: 备份清单声明 schema v%d——已是新谱系，本命令不接；"+
				"记录丢失用 `secrets recover-record`，无记录则保持恢复态",
			manifest.SchemaVersion)
	}
	if manifest.SourceConfigSHA256 == "" || manifest.BackupConfigSHA256 == "" ||
		manifest.SourceConfigSHA256 != manifest.BackupConfigSHA256 {
		return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 清单的 source_config_sha256 与 backup_config_sha256 必须相等且非空")
	}
	// 备份程序与外部锚点编号必须写明（§2.12.0b 字段表：非空）。它们进审计详情，也是
	// "这次备份对应哪张变更单 / 交接单"的唯一线索——缺了就无法人工复核来源。
	if strings.TrimSpace(manifest.ProcedureRef) == "" || strings.TrimSpace(manifest.AnchorRef) == "" {
		return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 清单缺少 procedure_ref / anchor_ref——外部锚点编号必须来自"+
				"变更单或交接单，不能为空")
	}
	// 名册字段要么整组给（present + 哈希 + 实例标识），要么整组空（无名册存量）：
	// 半组声明说明清单自相矛盾，重算哈希也判断不了该核对什么。
	switch {
	case manifest.RosterPresent && (manifest.RosterSHA256 == "" || manifest.InstanceID == ""):
		return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 清单声明 roster_present=true，但 roster_sha256 / instance_id 为空")
	case !manifest.RosterPresent && (manifest.RosterSHA256 != "" || manifest.InstanceID != ""):
		return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
			"backup_invalid: 清单声明 roster_present=false，却带了 roster_sha256 / instance_id")
	}

	checks := []struct {
		name string
		want string
	}{
		{"basalt.db", manifest.DBSHA256},
		{"basalt.yaml", manifest.BackupConfigSHA256},
	}
	if manifest.WALSHA256 != "" {
		checks = append(checks, struct {
			name string
			want string
		}{"basalt.db-wal", manifest.WALSHA256})
	}
	if manifest.SHMSHA256 != "" {
		checks = append(checks, struct {
			name string
			want string
		}{"basalt.db-shm", manifest.SHMSHA256})
	}
	if manifest.RosterPresent {
		checks = append(checks, struct {
			name string
			want string
		}{"instance.json", manifest.RosterSHA256})
	}
	for _, check := range checks {
		got, hashErr := hashFileString(filepath.Join(backupDir, check.name))
		if hashErr != nil {
			return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
				"backup_invalid: 备份文件 %s 不可读: %v", check.name, hashErr)
		}
		if check.want == "" || got != check.want {
			return manifest, refuseOffline(legacyCodeBackupInvalid, 1,
				"backup_invalid: 备份文件 %s 与清单声明不符（got=%s want=%s）",
				check.name, got, check.want)
		}
	}
	return manifest, nil
}

// verifySourceUnchanged 重算源端配置与库哈希：两者任一漂移都判 `source_changed`。
// 只看配置不看库会放过"备份后又写了几条凭据"，只看库不看配置会放过密钥路径变化。
// verifySourceUnchanged recomputes the source config and database hashes.
func (p legacyAdoptPort) verifySourceUnchanged(source string, manifest legacyBackupManifest) error {
	src, err := filepath.Abs(source)
	if err != nil {
		return refuseOffline(legacyCodeSourceChanged, 1, "source_changed: --source 无法规范化为绝对路径")
	}
	configPath := filepath.Join(filepath.Dir(src), configFileName)
	configSHA, err := hashFileString(configPath)
	if err != nil {
		return refuseOffline(legacyCodeSourceChanged, 1,
			"source_changed: 读取源端配置失败（源端不可读时必须改用 --attest-source-unchanged）: %v", err)
	}
	if configSHA != manifest.SourceConfigSHA256 {
		return refuseOffline(legacyCodeSourceChanged, 1,
			"source_changed: 源端 basalt.yaml 在备份之后发生了变化（%s → %s）",
			manifest.SourceConfigSHA256, configSHA)
	}
	dbSHA, err := hashFileString(filepath.Join(src, databaseFileName))
	if err != nil {
		return refuseOffline(legacyCodeSourceChanged, 1,
			"source_changed: 读取源端库失败: %v", err)
	}
	if dbSHA != manifest.DBSHA256 {
		return refuseOffline(legacyCodeSourceChanged, 1,
			"source_changed: 源端库在备份之后发生了变化（%s → %s）——"+
				"备份不是源端最后状态，不得据此采纳", manifest.DBSHA256, dbSHA)
	}
	// WAL 只有清单记录过才参与漂移判定；`-shm` 只比存在性（§2.11.6 文件身份口径），
	// 它是共享内存的易变映像，内容变化不代表源端写了库。
	if manifest.WALSHA256 != "" {
		walSHA, walErr := hashFileString(filepath.Join(src, databaseFileName+"-wal"))
		if walErr != nil {
			return refuseOffline(legacyCodeSourceChanged, 1,
				"source_changed: 读取源端 WAL 失败（清单记录过 WAL）: %v", walErr)
		}
		if walSHA != manifest.WALSHA256 {
			return refuseOffline(legacyCodeSourceChanged, 1,
				"source_changed: 源端 WAL 在备份之后发生了变化（%s → %s）",
				manifest.WALSHA256, walSHA)
		}
	}
	return nil
}

// loadLegacyKey 按 K1 / K2 / 配置路径取钥，并核对文件哈希与来源可信度。
// K1（备份内副本）可省略 `--expect-key-sha256`；K2（备份外副本）**必须**带锚点。
// loadLegacyKey resolves the key by provenance; a K2 path without an anchor is refused.
func (p legacyAdoptPort) loadLegacyKey(cfg *cli.Config, backupDir, ws string, manifest legacyBackupManifest, req cli.AdoptLegacyRequest) (MasterKey, string, string, error) {
	var keyPath string
	keySource := "config_path"
	switch {
	case strings.TrimSpace(req.MasterKeyFile) != "":
		keyPath = req.MasterKeyFile
		keySource = "external_anchor"
	case strings.TrimSpace(cfg.Secrets.MasterKeyFile) != "":
		keyPath = cfg.Secrets.MasterKeyFile
	default:
		// 配置没有路径时只允许**备份内**的密钥副本（K1）。
		name := manifest.KeyFileName
		if name == "" {
			name = masterKeyFileName
		}
		keyPath = filepath.Join(backupDir, name)
		keySource = "backup_copy"
	}

	keyFileSHA, err := hashFileString(keyPath)
	if err != nil {
		return MasterKey{}, keySource, "", refuseOffline(legacyCodeKeyUnavailable, 1,
			"key_unavailable: 读取原密钥 %s 失败（副本主机不可达时用 --master-key-file 受控读取）: %v", keyPath, err)
	}
	// 清单哈希不符的**类别**取决于取钥来源：配置路径 / 备份内副本是"备份自证对不上"
	// （`attestation_mismatch`），覆盖路径是"覆盖来源不可信"（`key_unverified`）。
	if manifest.KeyFileSHA256 != "" && keyFileSHA != manifest.KeyFileSHA256 {
		code := legacyCodeAttestationMismatch
		if keySource == "external_anchor" {
			code = legacyCodeKeyUnverified
		}
		return MasterKey{}, keySource, "", refuseOffline(code, 1,
			"%s: 密钥文件哈希与清单不符（got=%s want=%s）——密钥不是备份时那一把",
			code, keyFileSHA, manifest.KeyFileSHA256)
	}
	switch keySource {
	case "external_anchor":
		expect, ok := normalizeKeySHA256(req.ExpectKeySHA256)
		if !ok {
			return MasterKey{}, keySource, "", refuseOffline(legacyCodeKeyUnverified, 1,
				"key_unverified: 备份外取钥必须带 --expect-key-sha256 外部锚点")
		}
		if expect != keyFileSHA {
			return MasterKey{}, keySource, "", refuseOffline(legacyCodeKeyUnverified, 1,
				"key_unverified: 密钥文件与外部锚点不符（expected=%s computed=%s）",
				expect, keyFileSHA)
		}
	case "backup_copy":
		// K1：备份内副本自带清单哈希，外部锚点可省；带了就必须相符。
		if req.ExpectKeySHA256 != "" {
			expect, ok := normalizeKeySHA256(req.ExpectKeySHA256)
			if !ok || expect != keyFileSHA {
				return MasterKey{}, keySource, "", refuseOffline(legacyCodeKeyUnverified, 1,
					"key_unverified: 密钥文件与外部锚点不符")
			}
		}
	}
	key, loadErr := LoadMasterKey(keyPath, ws)
	if loadErr != nil {
		return MasterKey{}, keySource, "", refuseOffline(legacyCodeKeyUnavailable, 1,
			"key_unavailable: 主密钥文件不可用: %v", loadErr)
	}
	return key, keySource, keyFileSHA, nil
}

// finishOffline 回填失败终态；取消不再改变审计结论。
// finishOffline writes the terminal state even when the caller's context is gone.
func (p legacyAdoptPort) finishOffline(ctx context.Context, store *storage.ManagementStore, opID string, status core.OperationStatus, resultCode string, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if err := store.FinishOperation(context.WithoutCancel(ctx), opID, status, resultCode, message, nil, p.clock()); err != nil {
		p.logger().Error("secrets 离线流程：回填审计终态失败", "operation_id", opID, "error", err)
	}
}

// hashFileString 返回文件字节的小写十六进制 SHA-256。
// hashFileString hashes a file's bytes into lowercase hex.
//
// 路径由命令参数 / 工作区推出，读取经所在目录句柄（`os.Root`）——与其他同类读取
// 同一先例，既消除 G304，也不跟随越出目录的链接。
// The read is scoped through a root handle on the file's own directory.
func hashFileString(path string) (string, error) {
	raw, err := readFileUnder(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return "", err
	}
	return hexSHA256(raw), nil
}

// configMaskRefusalCode 把摘要算法的三种可判定拒绝映射成 §2.12.0b 的字面量。
// configMaskRefusalCode maps the digest algorithm's three refusals to their literals.
func configMaskRefusalCode(err error) string {
	switch {
	case errors.Is(err, errConfigKeyPathMissing):
		return "master_key_path_unpersisted"
	case errors.Is(err, errConfigAmbiguous):
		return "config_ambiguous"
	default:
		return "config_invalid"
	}
}

// unusableAAD 统计"用记录字段重建不出 AAD"的密文条数，返回首个失败描述。
// 判据是 `CredentialAAD.Validate()`——属主类别 / 资源 UID / 凭据 ID / 版本号 /
// 目标绑定任一不合形态即视为该行 AAD 已不可重建。
// unusableAAD counts rows whose AAD cannot be rebuilt from the record's own fields.
func unusableAAD(sealed []storage.SealedCredential) (int, string) {
	bad := 0
	first := ""
	for _, item := range sealed {
		aad := CredentialAAD{
			OwnerKind:     core.BindingKind(item.OwnerKind),
			OwnerUID:      core.ResourceUID(item.OwnerUID),
			CredentialID:  item.CredentialID,
			VersionNo:     item.VersionNo,
			TargetBinding: item.TargetBinding,
		}
		err := aad.Validate()
		if err == nil {
			continue
		}
		bad++
		if first == "" {
			first = fmt.Sprintf("凭据 %s v%d: %v", item.CredentialID, item.VersionNo, err)
		}
	}
	return bad, first
}

// normalizeKeySHA256 规范化外部给来的 SHA-256 十六进制串：去空白、转小写、校验形态。
// 形态不合就当"没给"——**不得**宽松接受，否则锚点比对会被空串或前缀蒙混过关。
// normalizeKeySHA256 normalises an operator-supplied SHA-256 hex digest, or reports
// false so the caller refuses rather than comparing against a malformed value.
func normalizeKeySHA256(value string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if len(normalized) != 64 {
		return "", false
	}
	for _, char := range normalized {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", false
		}
	}
	return normalized, true
}
