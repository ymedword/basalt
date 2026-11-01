package app

// 本文件实现 `secrets recover-record` 的离线适配器（颗粒度文档 §3.1.8b / §2.12.0a，
// D-40 记录恢复）。它**不经过**重路径装配：不 `newRuntime`、不开 HTTP、不启 MCP、
// 不装调度与 notifier、不调用模型。全部动作只有三件——
//
//	① 只读采集：当前工作区（记录 / 密文 / 待恢复操作 / 名册）与备份（同四项）
//	② 逐项校验：身份 → 记录 → 密钥 → 密文可解 → 绑定 → 密文一致性 → token 来源
//	③ 通过后写：预写 `pending` 审计 → 名册（仅缺失时）→ 记录 → 回填终态
//
// 顺序是判据的一部分：任何拒绝都发生在第一次写之前，除锁文件的 `O_CREATE` 外零写入。
// This adapter implements the offline recover-record procedure; every refusal happens
// before the first write and no runtime is ever assembled.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// recoverCodeBackupIdentity：备份里的实例标识与当前工作区名册不符。
	recoverCodeBackupIdentity = "backup_identity_mismatch"
	// recoverCodeBackupRosterMissing：当前无名册，备份也没带身份。
	recoverCodeBackupRosterMissing = "backup_roster_missing"
	// recoverCodeBackupRosterInvalid：备份里的名册损坏 / 标识非法。
	recoverCodeBackupRosterInvalid = "backup_roster_invalid"
	// recoverCodeRecordExists：本库已有 `master_key` 记录（幂等先于状态门）。
	recoverCodeRecordExists = "record_already_exists"
	// recoverCodeBackupRecordMissing：备份里没有 `master_key` 记录。
	recoverCodeBackupRecordMissing = "backup_record_missing"
	// recoverCodeBackupRecordInvalid：备份里的记录字段残缺。
	recoverCodeBackupRecordInvalid = "backup_record_invalid"
	// recoverCodeKeyRecordMismatch：当前密钥与备份记录不符。
	recoverCodeKeyRecordMismatch = "key_record_mismatch"
	// recoverCodeKeyUnavailable：配置所指主密钥文件不可读。
	recoverCodeKeyUnavailable = "key_unavailable"
	// recoverCodeCiphertextUndecryptable：有密文用当前密钥解不开。
	recoverCodeCiphertextUndecryptable = "ciphertext_undecryptable"
	// recoverCodeAADMismatch：AAD 与记录不一致（解密失败的细分归因）。
	recoverCodeAADMismatch = "aad_mismatch"
	// recoverCodeBindingMismatch：绑定信息与备份不一致。
	recoverCodeBindingMismatch = "binding_mismatch"
	// recoverCodeCiphertextMismatch：密文行与备份不一致（库在备份之后被改过）。
	recoverCodeCiphertextMismatch = "ciphertext_mismatch"
	// recoverCodeStateNotRecovery：当前不是案例 3 的恢复态。
	recoverCodeStateNotRecovery = "state_not_recovery"
	// recoverCodeRecordWrite：写记录失败（审计回填 failed）。
	recoverCodeRecordWrite = "record_write_failed"
	// recoverCodeRosterWrite：写名册失败（审计回填 failed）。
	recoverCodeRosterWrite = "roster_write_failed"
)

// offlineRefusal 是离线流程的带码**且带退出码**的拒绝：`Error()` 以稳定码开头，
// `ExitCode()` 供装配骨架取四档退出码（§3.1.8b / §3.1.8c）。
// offlineRefusal carries both the stable code and the documented exit status.
type offlineRefusal struct {
	Code string
	Exit int
	Err  error
}

// Error 以码开头返回拒绝文案。
// Error renders the code first so callers and tests can match it verbatim.
func (r *offlineRefusal) Error() string { return r.Code + ": " + r.Err.Error() }

// Unwrap 保留底层错误链。
// Unwrap keeps the underlying cause reachable.
func (r *offlineRefusal) Unwrap() error { return r.Err }

// ExitCode 实现 cli.ExitCode：`1` 校验失败 / `2` 前置不满足 / `3` 预写后失败。
// ExitCode satisfies cli.ExitCode.
func (r *offlineRefusal) ExitCode() int { return r.Exit }

// refuseOffline 构造一条带退出码的拒绝。
// refuseOffline builds one status-bearing refusal.
func refuseOffline(code string, exit int, format string, args ...any) error {
	return &offlineRefusal{Code: code, Exit: exit, Err: fmt.Errorf(format, args...)}
}

// secretsRecoverPort 实现 cli.SecretsRecovery。无状态：句柄每次调用自建自关。
// secretsRecoverPort is stateless; every call opens and releases its own handles.
type secretsRecoverPort struct {
	Logger *slog.Logger

	// Now 是可注入的时钟（报告时刻与审计时间）；nil 时取 time.Now。
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
}

// logger / clock 保证零值适配器可用。
// logger / clock keep a zero-valued adapter usable.
func (p secretsRecoverPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

func (p secretsRecoverPort) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// recoverBackupFacts 是备份侧只读事实：数据库摘要、记录与名册。
// recoverBackupFacts is the read-only backup-side fact set.
type recoverBackupFacts struct {
	dbPath         string
	record         storage.InstanceSecretRecord
	hasRecord      bool
	sealed         []storage.SealedCredential
	bindingHash    string
	ciphertextHash string
	instanceID     string
	rosterBytes    []byte
	rosterSHA256   string
	rosterValid    bool
	// recordTraceable：备份记录的 `operation_id` 指向备份库里一条成功的
	// `system.secrets_init` / `instance.secrets_recover` 操作行（来源可回溯）。
	// recordTraceable marks a record whose operation_id resolves to a successful
	// init / recover row in the backup database.
	recordTraceable bool
}

// RecoverSecretsRecord 执行记录恢复（§2.12.0a 校验顺序 1～9）。
// RecoverSecretsRecord runs the documented check order 1–9.
func (p secretsRecoverPort) RecoverSecretsRecord(ctx context.Context, req cli.RecoverRecordRequest) (cli.RecoverRecordResult, error) {
	result := cli.RecoverRecordResult{
		Kind:       req.Kind,
		BackupPath: req.Backup,
		FinalState: "not_applied",
	}
	if !req.Confirm {
		// 命令面已经拒绝缺省形态；端口这一层再判一次是纵深防御（不得靠调用方守规矩）。
		return result, p.refuse(result, refuseOffline(recoverCodeStateNotRecovery, 2,
			"缺少显式确认——恢复记录必须由操作者确认"))
	}
	ws, err := filepath.Abs(strings.TrimSpace(req.Workspace))
	if err != nil || strings.TrimSpace(req.Workspace) == "" {
		return result, p.refuse(result, refuseOffline(recoverCodeStateNotRecovery, 2,
			"--workspace 无法规范化为绝对路径"))
	}
	result.ExpectedSHA256 = req.ManifestSHA256
	result.ComputedSHA256 = req.ManifestSHA256

	configPath := filepath.Join(filepath.Dir(ws), configFileName)
	cfg, err := cli.LoadConfig(configPath, os.LookupEnv)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return result, p.refuse(result, refuseOffline(recoverCodeStateNotRecovery, 2,
			"config_missing: 找不到配置文件 %s——恢复需要 serve 同款配置", configPath))
	default:
		return result, p.refuse(result, refuseOffline(recoverCodeStateNotRecovery, 2,
			"config_invalid: 装载配置 %s 失败: %v", configPath, err))
	}

	dbPath := filepath.Join(ws, databaseFileName)
	info, statErr := os.Stat(dbPath)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return result, p.refuse(result, refuseOffline(adoptCodeWorkspaceMissing, 2,
			"工作区 %s 里没有 SQLite 库（%s）", ws, databaseFileName))
	case statErr != nil:
		return result, p.refuse(result, refuseOffline(adoptCodeStorageUnavailable, 2,
			"检查工作区库 %s: %v", dbPath, statErr))
	case info.IsDir():
		return result, p.refuse(result, refuseOffline(adoptCodeWorkspaceMissing, 2,
			"%s 是目录，不是 SQLite 库文件", dbPath))
	}

	result, err = withInstanceLock(ws, func() (cli.RecoverRecordResult, error) {
		return p.recoverLocked(ctx, cfg, configPath, dbPath, ws, req, result)
	})
	if err != nil {
		var busy *workspace.LockBusyError
		if errors.As(err, &busy) {
			return result, p.refuse(result, refuseOffline(adoptCodeWorkspaceBusy, 2,
				"工作区 %s 的实例锁被持有——请先停止 serve 再重跑", ws))
		}
		return result, err
	}
	return result, nil
}

// recoverLocked 是持锁区间的主体：只读采集 → 逐项校验 → 写。
// recoverLocked is the whole body under the instance lock.
func (p secretsRecoverPort) recoverLocked(ctx context.Context, cfg *cli.Config, configPath, dbPath, ws string, req cli.RecoverRecordRequest, result cli.RecoverRecordResult) (cli.RecoverRecordResult, error) {
	facts, err := p.readRecoverFacts(ctx, dbPath, ws)
	if err != nil {
		return result, p.refuse(result, err)
	}
	result.RosterPresentBefore = facts.rosterPresent
	result.Ciphertexts = len(facts.sealed)

	// 崩溃恢复先于幂等判定：上次写到一半的 pending 行必须拿到终态。
	if err := p.recoverPendingRecovers(ctx, dbPath, facts); err != nil {
		return result, p.refuse(result, err)
	}
	// 幂等先于状态门：已恢复的实例重跑不会被报成 state_not_recovery。
	if facts.hasRecord {
		return result, p.refuse(result, refuseOffline(recoverCodeRecordExists, 1,
			"本库已有主密钥初始化记录（key_id=%s）——恢复不覆盖既有记录", facts.record.KeyID))
	}
	if len(facts.sealed) == 0 {
		return result, p.refuse(result, refuseOffline(recoverCodeStateNotRecovery, 2,
			"当前工作区没有托管密文（无记录、无密文）——不是案例 3 的恢复态，恢复无从证明"))
	}

	backup, err := p.readRecoverBackup(ctx, req)
	if err != nil {
		return result, p.refuse(result, err)
	}
	// ① 身份：有名册必须与备份一致；无名册必须能从备份恢复。
	switch {
	case facts.rosterPresent && backup.instanceID != "" && backup.instanceID != facts.roster.InstanceID:
		return result, p.refuse(result, refuseOffline(recoverCodeBackupIdentity, 1,
			"备份属于实例 %s，当前工作区名册是 %s——拒绝把别的实例的记录写进来",
			backup.instanceID, facts.roster.InstanceID))
	case !facts.rosterPresent && backup.instanceID == "":
		return result, p.refuse(result, refuseOffline(recoverCodeBackupRosterMissing, 1,
			"当前工作区没有名册，备份也没有携带实例身份——恢复无法建立身份"))
	case !facts.rosterPresent && len(backup.rosterBytes) == 0:
		// 路线 A（导出包）只有清单里的实例标识，没有可**逐字复制**的名册字节。D-40 明写
		// 「路线 A 的 manifest 不足以恢复身份」：用它拼一份名册出来，等于把"备份声称
		// 是谁"当成"工作区原先是谁"，而后者才是恢复要保住的事实。因此拒绝，且零写入。
		// Route A carries only the manifest identity, never roster bytes; rebuilding a
		// roster from it would present a claimed identity as the recovered one.
		return result, p.refuse(result, refuseOffline(recoverCodeBackupRosterMissing, 1,
			"当前工作区没有名册，备份只有清单里的实例标识（路线 A）——manifest 不足以建立身份，"+
				"恢复只接受路线 B 携带的备份名册"))
	case !facts.rosterPresent && !backup.rosterValid:
		return result, p.refuse(result, refuseOffline(recoverCodeBackupRosterInvalid, 1,
			"备份里的名册损坏或标识非法——不得据坏名册建立身份"))
	}
	// ② 记录：备份必须携带，字段必须完整，且来源可回溯。
	switch {
	case !backup.hasRecord:
		return result, p.refuse(result, refuseOffline(recoverCodeBackupRecordMissing, 1,
			"备份里没有 instance_secrets 的 master_key 记录——备份产于首次成功写记录之前，"+
				"按 schema 3～5 存量处理，走 secrets adopt-legacy"))
	case strings.TrimSpace(backup.record.KeyID) == "" || strings.TrimSpace(backup.record.Fingerprint) == "":
		return result, p.refuse(result, refuseOffline(recoverCodeBackupRecordInvalid, 1,
			"备份里的记录字段残缺（key_id / fingerprint 为空）——拒绝用坏记录恢复"))
	case backup.record.InitializedAt.UnixMilli() <= 0:
		// 记录连同它的时间一起来自备份：时间缺失说明备份库本身残缺，不得用"现在"补齐。
		return result, p.refuse(result, refuseOffline(recoverCodeBackupRecordInvalid, 1,
			"备份里的记录 initialized_at 不是正值——拒绝用时间线残缺的记录恢复"))
	case !backup.recordTraceable:
		return result, p.refuse(result, refuseOffline(recoverCodeBackupRecordInvalid, 1,
			"备份记录的 operation_id（%q）在备份库里找不到对应的成功初始化 / 恢复操作行——"+
				"来源不可回溯，拒绝恢复", backup.record.OperationID))
	}
	result.InstanceID = backup.instanceID
	result.RecordKeyID = backup.record.KeyID
	result.RecordFingerprint = backup.record.Fingerprint

	// ③ 密钥：必须能在配置所指路径上读到，且与备份记录相符。
	keyPath := strings.TrimSpace(cfg.Secrets.MasterKeyFile)
	if keyPath == "" {
		return result, p.refuse(result, refuseOffline(adoptCodeKeyPathUnpersisted, 1,
			"config_invalid: %s 未持久化——恢复要求原密钥路径已写死在配置里", adoptKeyPathConfigKey))
	}
	if !filepath.IsAbs(keyPath) {
		return result, p.refuse(result, refuseOffline(adoptCodeKeyPathRelative, 1,
			"config_invalid: %s 是相对路径——运行期按进程 cwd 解析，不能保证重启一致；请改为绝对路径", adoptKeyPathConfigKey))
	}
	key, err := LoadMasterKey(keyPath, ws)
	if err != nil {
		return result, p.refuse(result, refuseOffline(recoverCodeKeyUnavailable, 1,
			"主密钥文件不可用: %v", err))
	}
	defer key.Zero()
	if key.KeyID != backup.record.KeyID {
		return result, p.refuse(result, refuseOffline(recoverCodeKeyRecordMismatch, 1,
			"主密钥 key_id 与备份记录不符（记录 %s，文件 %s）——不得生成替代密钥",
			backup.record.KeyID, key.KeyID))
	}
	fingerprint, fpErr := MasterKeyFingerprint(key)
	if fpErr != nil {
		return result, p.refuse(result, refuseOffline(recoverCodeKeyRecordMismatch, 1,
			"计算主密钥指纹失败: %v", fpErr))
	}
	if fingerprint != backup.record.Fingerprint {
		return result, p.refuse(result, refuseOffline(recoverCodeKeyRecordMismatch, 1,
			"主密钥指纹与备份记录不符（记录 %s，文件 %s）", backup.record.Fingerprint, fingerprint))
	}

	// ④ 密文可解：逐条试解当前工作区的全部密文（顺序固定，失败点位可复现）。
	unreadable, first, verifyErr := verifySealedCredentials(key, facts.sealed)
	if verifyErr != nil {
		return result, p.refuse(result, refuseOffline(adoptCodeStorageUnavailable, 1,
			"核验托管密文失败: %v", verifyErr))
	}
	result.CiphertextsUnreadable = unreadable
	if unreadable > 0 {
		// 归因不能看解密失败文案（GCM 层分不出"AAD 变了"与"字节坏了"，两者都是
		// `cipher: message authentication failed`）。判据是与备份逐行比对 AAD 字段：
		// 字段漂移 → aad_mismatch；字段一致却解不开 → 密文本身坏了。
		code := recoverCodeCiphertextUndecryptable
		reason := "当前库的 AAD 字段与备份一致，说明密文字节已损坏或密钥不符"
		if aadDrift(facts.sealed, backup.sealed) {
			code = recoverCodeAADMismatch
			reason = "当前库的 AAD 字段已与备份不一致（所有者 / 版本 / 目标绑定被改动过）"
		}
		return result, p.refuse(result, refuseOffline(code, 1,
			"%d / %d 条托管密文无法用当前主密钥解开（首个失败：%s）——%s，拒绝写入记录",
			unreadable, len(facts.sealed), first, reason))
	}

	// ⑤ 绑定与密文一致性：库在备份之后被改过，恢复出的记录就不对应这堆密文。
	if facts.bindingHash != backup.bindingHash {
		return result, p.refuse(result, refuseOffline(recoverCodeBindingMismatch, 1,
			"当前工作区的凭据绑定与备份不一致——库在备份之后被改动过，拒绝恢复"))
	}
	if facts.ciphertextHash != backup.ciphertextHash {
		return result, p.refuse(result, refuseOffline(recoverCodeCiphertextMismatch, 1,
			"当前工作区的密文行与备份不一致——库在备份之后被改动过，拒绝恢复"))
	}

	// ⑥ token 来源：token 模式三处都无凭据时，下一次 serve 必然拒绝启动。
	if cfg.HTTP.AdminAuth.Mode == adminAuthModeToken && strings.TrimSpace(cfg.HTTP.AdminAuth.Token) == "" {
		return result, p.refuse(result, refuseOffline(adoptCodeAdminTokenMissing, 2,
			"http.admin_auth.mode=token 但配置没有提供 token——恢复后的实例将无法启动；"+
				"请先在配置里写 http.admin_auth.token = ${VAR} 再重跑"))
	}

	return p.writeRecovered(ctx, dbPath, ws, backup, facts, result)
}

// writeRecovered 是可写阶段：预写 pending → 名册（仅缺失时）→ 记录 → 回填终态。
// 进入这里已经通过全部校验；本阶段**不再做拒绝**。
// writeRecovered performs the write phase after every check has passed.
func (p secretsRecoverPort) writeRecovered(ctx context.Context, dbPath, ws string, backup recoverBackupFacts, facts recoverWorkspaceFacts, result cli.RecoverRecordResult) (cli.RecoverRecordResult, error) {
	now := p.clock()
	detail, marshalErr := json.Marshal(map[string]any{
		"backup_kind":            string(result.Kind),
		"backup_path":            result.BackupPath,
		"backup_manifest_sha256": result.ComputedSHA256,
		"copy_config_sha256":     "",
		"roster_sha256":          backup.rosterSHA256,
		"roster_restored":        !facts.rosterPresent,
		"record_key_id":          backup.record.KeyID,
		"record_fingerprint":     backup.record.Fingerprint,
		"ciphertexts":            result.Ciphertexts,
		"ciphertexts_unreadable": result.CiphertextsUnreadable,
		"record_origin":          "trusted_backup",
	})
	if marshalErr != nil {
		return result, p.refuse(result, refuseOffline(adoptCodeAuditPrewrite, 3,
			"序列化审计详情失败: %v", marshalErr))
	}

	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return result, p.refuse(result, refuseOffline(adoptCodeStorageUnavailable, 2,
			"打开可写库失败: %v", err))
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			p.logger().Warn("secrets recover-record：关闭可写库句柄失败", "error", closeErr)
		}
	}()

	store := storage.NewManagementStore(db)
	actor := cliWriteActor(p.logger())
	opID := core.NewOperationID(now)
	if err := store.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       core.ActionInstanceSecretsRecover,
		TargetKind:   core.TargetKindInstance,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	}); err != nil {
		// 预写失败：fail-closed，一个字节都不写（§2.12.0a 校验 9）。
		return result, p.refuse(result, refuseOffline(adoptCodeAuditPrewrite, 3,
			"预写 %s 审计失败——按 fail-closed 不写记录: %v", core.ActionInstanceSecretsRecover, err))
	}
	result.OperationID = opID

	// 写入顺序（§2.12.0a）：先名册后记录——名册落盘但记录失败可以重跑，反过来的
	// 半截状态会让"有名册无记录"的实例被当成别人的工作区。
	if !facts.rosterPresent {
		if err := writeRecoveredRoster(ws, backup.rosterBytes); err != nil {
			p.finishOffline(ctx, store, opID, core.OperationFailed, recoverCodeRosterWrite, err)
			result.FinalState = "failed"
			return result, p.refuse(result, refuseOffline(recoverCodeRosterWrite, 3,
				"写名册失败（操作 %s 已回填 failed）: %v", opID, err))
		}
		result.RosterRestored = true
		result.RosterSHA256 = backup.rosterSHA256
	} else if backup.rosterSHA256 != "" {
		result.RosterSHA256 = backup.rosterSHA256
	}

	secretsStore := storage.NewInstanceSecretsStore(db)
	record := storage.InstanceSecretRecord{
		Scope:       core.InstanceSecretScopeMasterKey,
		KeyID:       backup.record.KeyID,
		Fingerprint: backup.record.Fingerprint,
		// `initialized_at` 来自备份记录：恢复的是"当初那次初始化"，不是"这次恢复"。
		// `operation_id` 才是本次恢复——两者各记各的，时间线不被抹平。
		InitializedAt: backup.record.InitializedAt,
		OperationID:   opID,
	}
	if err := secretsStore.Insert(ctx, record); err != nil {
		if errors.Is(err, storage.ErrInstanceSecretExists) {
			// 幂等：另一个进程在我们持锁期间写了记录是不可能的（锁是互斥的），
			// 走到这里说明库在本次调用期间被外部改动——如实回填 failed。
			cause := fmt.Errorf("记录已存在（%w）", err)
			p.finishOffline(ctx, store, opID, core.OperationFailed, recoverCodeRecordWrite, cause)
			result.FinalState = "failed"
			return result, p.refuse(result, refuseOffline(recoverCodeRecordWrite, 3,
				"写记录失败（操作 %s 已回填 failed）: %v", opID, err))
		}
		p.finishOffline(ctx, store, opID, core.OperationFailed, recoverCodeRecordWrite, err)
		result.FinalState = "failed"
		return result, p.refuse(result, refuseOffline(recoverCodeRecordWrite, 3,
			"写记录失败（操作 %s 已回填 failed）: %v", opID, err))
	}

	if err := store.FinishOperation(ctx, opID, core.OperationSuccess, "", "", nil, now); err != nil {
		p.logger().Error("secrets recover-record：记录已写入，但回填审计终态失败",
			"operation_id", opID, "error", err)
		result.FinalState = "failed"
		return result, p.refuse(result, refuseOffline(adoptCodeAuditFinish, 3,
			"记录已写入但审计终态回填失败（操作 %s）——重跑会被 record_already_exists 挡住，请按操作号查证: %v", opID, err))
	}
	result.FinalState = "completed"
	return result, nil
}

// writeRecoveredRoster 把备份名册**逐字**写进工作区（`O_CREAT|O_EXCL`，不覆盖）。
// 空字节是调用方的编程错误：身份门已经拒绝"没有名册字节"的备份（含路线 A 的 manifest
// 标识），这里再拒一次而不是退回"按标识重建"——拼装名册正是 D-40 禁止的做法。
// writeRecoveredRoster copies the backup roster verbatim; empty bytes are refused here
// rather than rebuilt from an identity string (D-40 forbids assembling a roster).
func writeRecoveredRoster(ws string, raw []byte) error {
	if len(raw) == 0 {
		return fmt.Errorf("备份名册为空——不得按实例标识拼装名册（D-40）")
	}
	base, err := os.OpenRoot(ws)
	if err != nil {
		return fmt.Errorf("打开工作区 %s: %w", ws, err)
	}
	defer func() { _ = base.Close() }()

	created, err := createExclusiveFile(base, instanceMarkerFileName, raw)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("名册 %s 已存在——不覆盖既有身份", filepath.Join(ws, instanceMarkerFileName))
	}
	return nil
}

// finishOffline 回填失败终态；取消不再改变审计结论（动作已经发生或已明确失败）。
// finishOffline writes the terminal state even when the caller's context is gone.
func (p secretsRecoverPort) finishOffline(ctx context.Context, store *storage.ManagementStore, opID string, status core.OperationStatus, resultCode string, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if err := store.FinishOperation(context.WithoutCancel(ctx), opID, status, resultCode, message, nil, p.clock()); err != nil {
		p.logger().Error("secrets 离线流程：回填审计终态失败", "operation_id", opID, "error", err)
	}
}

// refuse 在返回前留一条 WARN（失败路径日志与审计双轨）。
// refuse logs the refusal as a WARN before returning it.
func (p secretsRecoverPort) refuse(result cli.RecoverRecordResult, err error) error {
	p.logger().Warn("secrets recover-record：拒绝",
		"workspace", result.Workspace, "kind", result.Kind, "error", err)
	return err
}

// recoverWorkspaceFacts 是当前工作区的事实集合：记录 / 密文 / 待恢复操作 / 名册。
// recoverWorkspaceFacts is the current workspace's fact set.
type recoverWorkspaceFacts struct {
	record         storage.InstanceSecretRecord
	hasRecord      bool
	sealed         []storage.SealedCredential
	pending        []core.OperationRecord
	roster         instanceMarker
	rosterPresent  bool
	bindingHash    string
	ciphertextHash string
}

// readRecoverFacts 只读打开工作区库并采集全部事实；句柄在本函数内关闭。
// readRecoverFacts opens the workspace read-only and closes the handle before returning.
func (p secretsRecoverPort) readRecoverFacts(ctx context.Context, dbPath, ws string) (recoverWorkspaceFacts, error) {
	var facts recoverWorkspaceFacts
	db, err := storage.OpenReadOnly(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return facts, classifyAdoptOpen(dbPath, err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			p.logger().Warn("secrets recover-record：关闭只读库句柄失败", "error", closeErr)
		}
	}()

	secretsStore := storage.NewInstanceSecretsStore(db)
	facts.record, facts.hasRecord, err = secretsStore.Load(ctx, core.InstanceSecretScopeMasterKey)
	if err != nil {
		return facts, refuseOffline(adoptCodeStorageUnavailable, 2,
			"读取实例加密记录失败: %v", err)
	}
	facts.sealed, err = secretsStore.SealedCredentials(ctx)
	if err != nil {
		return facts, refuseOffline(adoptCodeStorageUnavailable, 2,
			"列出现存托管密文失败: %v", err)
	}
	facts.pending, err = storage.NewManagementStore(db).PendingOperations(ctx)
	if err != nil {
		return facts, refuseOffline(adoptCodeStorageUnavailable, 2,
			"列出待恢复管理操作失败: %v", err)
	}
	facts.roster, facts.rosterPresent, err = readInstanceMarkerAt(ws)
	if err != nil {
		return facts, refuseOffline(adoptCodeRosterInvalid, 2,
			"工作区名册不可用（损坏 / 版本过高 / 标识非法）——不得在坏名册上补写: %v", err)
	}
	facts.bindingHash = bindingDigest(facts.sealed)
	facts.ciphertextHash = ciphertextDigest(facts.sealed)
	return facts, nil
}

// readRecoverBackup 按路线打开备份：A 读快照库 + 清单身份；B 读 `basalt.db` + 名册字节。
// readRecoverBackup opens the backup by route: A reads the snapshot and manifest identity,
// B reads basalt.db plus the roster bytes.
func (p secretsRecoverPort) readRecoverBackup(ctx context.Context, req cli.RecoverRecordRequest) (recoverBackupFacts, error) {
	var facts recoverBackupFacts
	switch req.Kind {
	case cli.BackupKindExportPackage:
		facts.dbPath = filepath.Join(req.Backup, "source-snapshot.db")
		manifestRaw, readErr := os.ReadFile(filepath.Join(req.Backup, "manifest.json"))
		if readErr != nil {
			return facts, refuseOffline("backup_invalid", 1, "读取备份清单失败: %v", readErr)
		}
		var manifest struct {
			SourceInstanceID string `json:"source_instance_id"`
		}
		if jsonErr := json.Unmarshal(manifestRaw, &manifest); jsonErr != nil {
			return facts, refuseOffline("backup_invalid", 1, "备份清单不是合法 JSON: %v", jsonErr)
		}
		facts.instanceID = strings.TrimSpace(manifest.SourceInstanceID)
		facts.rosterValid = facts.instanceID != ""
	case cli.BackupKindNativeSet:
		facts.dbPath = filepath.Join(req.Backup, "basalt.db")
		raw, readErr := os.ReadFile(filepath.Join(req.Backup, "instance.json"))
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				return facts, refuseOffline(recoverCodeBackupRosterMissing, 1,
					"备份目录里没有 instance.json——原生备份集必须携带名册")
			}
			return facts, refuseOffline(recoverCodeBackupRosterInvalid, 1,
				"读取备份名册失败: %v", readErr)
		}
		marker, parseErr := parseInstanceMarker(raw)
		if parseErr != nil {
			return facts, refuseOffline(recoverCodeBackupRosterInvalid, 1,
				"备份名册损坏或标识非法: %v", parseErr)
		}
		facts.instanceID = marker.InstanceID
		facts.rosterBytes = raw
		facts.rosterSHA256 = hexSHA256(raw)
		facts.rosterValid = true
	default:
		return facts, refuseOffline("backup_invalid", 1,
			"未知的备份形态 %q——只在命令面判定两种路线", string(req.Kind))
	}

	db, err := storage.OpenReadOnly(ctx, facts.dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		if errors.Is(err, core.ErrSchemaOutdated) {
			return facts, refuseOffline(recoverCodeBackupRecordMissing, 1,
				"备份库 schema 低于 v%d——该备份产于首次成功写记录之前，按 schema 3～5 存量走 secrets adopt-legacy",
				storage.SupportedSchemaVersion)
		}
		return facts, refuseOffline("backup_invalid", 1, "只读打开备份库失败: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			p.logger().Warn("secrets recover-record：关闭备份库句柄失败", "error", closeErr)
		}
	}()
	secretsStore := storage.NewInstanceSecretsStore(db)
	facts.record, facts.hasRecord, err = secretsStore.Load(ctx, core.InstanceSecretScopeMasterKey)
	if err != nil {
		return facts, refuseOffline("backup_invalid", 1, "读取备份记录失败: %v", err)
	}
	facts.sealed, err = secretsStore.SealedCredentials(ctx)
	if err != nil {
		return facts, refuseOffline("backup_invalid", 1, "列出备份密文失败: %v", err)
	}
	// 来源可回溯：`operation_id` 必须指向备份库里一条**成功**的初始化 / 恢复操作行。
	// 在这里解析（而不是到校验 ② 再查）是因为备份库句柄随本函数返回即关闭。
	if facts.hasRecord {
		op, opErr := storage.NewManagementStore(db).GetOperation(ctx, facts.record.OperationID)
		switch {
		case errors.Is(opErr, core.ErrOperationNotFound):
			facts.recordTraceable = false
		case opErr != nil:
			return facts, refuseOffline("backup_invalid", 1, "读取备份操作记录失败: %v", opErr)
		default:
			facts.recordTraceable = op.Status == core.OperationSuccess &&
				(op.Action == core.ActionSystemSecretsInit || op.Action == core.ActionInstanceSecretsRecover)
		}
	}
	facts.bindingHash = bindingDigest(facts.sealed)
	facts.ciphertextHash = ciphertextDigest(facts.sealed)
	if facts.instanceID == "" && len(facts.sealed) > 0 {
		// 后备身份来源：备份没有名册 / 清单标识时，不得凭"有密文"猜身份。
		facts.rosterValid = false
	}
	return facts, nil
}

// recoverPendingRecovers 回填上次崩溃留下的 `instance.secrets_recover` pending 行：
// 记录存在即已生效，否则未生效（**不**自动重做——重做由操作者重跑命令触发）。
// recoverPendingRecovers settles leftover pending recovery rows without redoing them.
func (p secretsRecoverPort) recoverPendingRecovers(ctx context.Context, dbPath string, facts recoverWorkspaceFacts) error {
	var targets []core.OperationRecord
	for _, op := range facts.pending {
		if op.Action == core.ActionInstanceSecretsRecover {
			targets = append(targets, op)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return refuseOffline(adoptCodeStorageUnavailable, 2,
			"打开可写库回填未完成的操作失败: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			p.logger().Warn("secrets recover-record：关闭可写库句柄失败", "error", closeErr)
		}
	}()
	store := storage.NewManagementStore(db)
	now := p.clock()
	for _, op := range targets {
		verification := core.VerificationNotApplied
		if facts.hasRecord {
			verification = core.VerificationApplied
		}
		status := verifyOutcome(verification)
		if err := store.RecoverOperation(ctx, op.OperationID, status, verification, nil, now); err != nil {
			return refuseOffline(adoptCodeRecoveryFailed, 2,
				"回填上次未完成的恢复操作 %s 失败: %v", op.OperationID, err)
		}
		p.logger().Warn("secrets recover-record：回填上次未完成的恢复操作",
			"operation_id", op.OperationID, "verification", verification, "status", status)
	}
	return nil
}

// bindingDigest 对凭据**绑定**求摘要：所有者种类 / 标识、凭据与版本号、目标绑定。
// 顺序固定（按凭据 ID、版本号排序），两侧可比。
// bindingDigest hashes the binding columns in a stable order.
func bindingDigest(sealed []storage.SealedCredential) string {
	rows := make([]string, 0, len(sealed))
	for _, item := range sealed {
		rows = append(rows, fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s",
			item.OwnerKind, item.OwnerUID, item.CredentialID, item.VersionNo, item.TargetBinding))
	}
	return digestRows(rows)
}

// ciphertextDigest 对密文**材料**求摘要：key_id、nonce 与密文逐字节。
// ciphertextDigest hashes the key id, nonce and ciphertext bytes in a stable order.
func ciphertextDigest(sealed []storage.SealedCredential) string {
	rows := make([]string, 0, len(sealed))
	for _, item := range sealed {
		rows = append(rows, fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s",
			item.CredentialID, item.KeyID, item.VersionNo,
			hex.EncodeToString(item.Nonce), hex.EncodeToString(item.Ciphertext)))
	}
	return digestRows(rows)
}

// aadDrift 判定"解不开"能否归因于 AAD 漂移。密文的 AAD 由该行自己的列重建
// （owner_kind / owner_uid / credential_id / version_no / target_binding），而 GCM 层
// 分不出"这六段变了"与"密文字节变了"——两者都是同一条认证失败。可判定的判据是
// **与备份逐行比对**：备份是这堆密文当时的唯一可信参照。
//   - 备份有同键行（凭据 ID + 版本号）：可比的 AAD 字段任一不同即漂移；
//   - 备份没有同键行（备份之后新增）：退回结构校验——AAD 能按文档形态重建就不算
//     漂移（新增凭据的密文损坏是密文问题，不是 AAD 问题）。
//
// aadDrift distinguishes AAD drift from ciphertext corruption by comparing each
// current row against the backup row with the same (credential, version) key.
func aadDrift(current, backup []storage.SealedCredential) bool {
	type rowKey struct {
		credentialID string
		versionNo    int
	}
	reference := make(map[rowKey]storage.SealedCredential, len(backup))
	for _, item := range backup {
		reference[rowKey{item.CredentialID, item.VersionNo}] = item
	}
	for _, item := range current {
		ref, ok := reference[rowKey{item.CredentialID, item.VersionNo}]
		if !ok {
			aad := CredentialAAD{
				OwnerKind:     core.BindingKind(item.OwnerKind),
				OwnerUID:      core.ResourceUID(item.OwnerUID),
				CredentialID:  item.CredentialID,
				VersionNo:     item.VersionNo,
				TargetBinding: item.TargetBinding,
			}
			if aad.Validate() != nil {
				return true
			}
			continue
		}
		if item.OwnerKind != ref.OwnerKind || item.OwnerUID != ref.OwnerUID ||
			item.TargetBinding != ref.TargetBinding {
			return true
		}
	}
	return false
}

// digestRows 排序后逐行喂进 SHA-256：排序让摘要与行序无关，逐行分隔避免拼接歧义。
// digestRows sorts then hashes; the sort makes the digest order-independent.
func digestRows(rows []string) string {
	sort.Strings(rows)
	h := sha256.New()
	for _, row := range rows {
		_, _ = h.Write([]byte(row))
		_, _ = h.Write([]byte{0x1e})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hexSHA256 返回字节的小写十六进制 SHA-256。
// hexSHA256 hashes bytes into lowercase hex.
func hexSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
