package app

// 本文件是 `basalt instance adopt` 的生产适配器（`cli.InstanceAdoption` 的实现，端口定义
// 在 internal/cli/ports.go）。它只做一件有副作用的事：给一个**已有 SQLite 库、但没有
// 名册**的存量工作区补写 `instance.json`。
//
// 四条纪律（颗粒度文档 §3.1.8a / C-47）：
//   - **离线**：只碰工作区锁、`serve` 同款配置、旧 SQLite 库与名册；不装配运行期，
//     零外呼；
//   - **只写名册**：不写 `instance_secrets` 记录、不调 `Init` / `Ensure`、不生成 /
//     搬移 / 重加密主密钥，也不创建私密目录；
//   - **拒绝零写入**：库不存在、名册已存在、锁不可得、schema ∈ {3,4,5}、四判定不通过、
//     迁移准备副本的配置与 adopt-legacy 基线不符（缺引用 / 越界修改 / 缺证据）、
//     密钥路径未持久化 / 相对 / 与显式路径不符、token 模式无显式 token——全部在写名册
//     之前返回；
//   - **先审计后落盘**（宪法第 5 条）：`instance.adopt` 的 `pending` 行先写，预写失败
//     不写名册；崩溃恢复只按"名册上的 instance_id 是否等于预写候选"查证，**不重做**。
//
// The adapter behind `basalt instance adopt`: offline, roster-only, refusals write
// nothing, and the pending audit row lands before the roster.
//
// 只读阶段先行：`storage.OpenWritable` 会切 WAL（写库头），而"拒绝即零写入"要求拒绝
// 路径连库头都不动。所以事实采集与全部拒绝都走 `storage.OpenReadOnly`（本身零写、
// 且只放行当前版本），只有确实要写（回填 pending 或写名册）时才开可写句柄。
// The read-only pass comes first because OpenWritable touches the WAL header; only an
// actual write reopens the database writable.

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
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// 采纳命令的稳定拒绝码（报告与错误文本以码开头，测试按码断言）。字面量与颗粒度文档
// §3.1.8a「拒绝路径」一一对应；文档未给字面量的三处（`instance_workspace_missing` /
// `instance_roster_invalid` / `instance_id_failed` 等实现侧失败码）在 run-state 披露。
// The stable refusal codes; each mirrors one row of the module document's refusal list.
const (
	// adoptCodeWorkspaceRequired：没有给出工作区。
	adoptCodeWorkspaceRequired = "workspace_required"
	// adoptCodeConfigMissing：同级 `basalt.yaml` 不存在。
	adoptCodeConfigMissing = "config_missing"
	// adoptCodeConfigInvalid：配置解析 / 校验失败。
	adoptCodeConfigInvalid = "config_invalid"
	// adoptCodeWorkspaceMissing：工作区里没有 SQLite 库（不是存量实例）。
	adoptCodeWorkspaceMissing = "instance_workspace_missing"
	// adoptCodeStorageUnavailable：库或审计表读写失败（无法判定）。
	adoptCodeStorageUnavailable = "storage_unavailable"
	// adoptCodeWorkspaceBusy：实例锁不可得。
	adoptCodeWorkspaceBusy = "workspace_busy"
	// adoptCodeLegacySchema：schema ∈ {3,4,5}（或更旧）——先走 adopt-legacy。
	adoptCodeLegacySchema = "legacy_schema_requires_adopt_legacy"
	// adoptCodeSchemaTooNew：库 schema 高于本二进制支持的版本。
	adoptCodeSchemaTooNew = "schema_too_new"
	// adoptCodeSchemaCorrupt：schema_version 表存在但零行。
	adoptCodeSchemaCorrupt = "schema_corrupt"
	// adoptCodeRosterExists：名册已存在——采纳不覆盖既有身份。
	adoptCodeRosterExists = "instance_roster_exists"
	// adoptCodeRosterInvalid：名册损坏 / 版本过高 / 标识非法。
	adoptCodeRosterInvalid = "instance_roster_invalid"
	// adoptCodeUnprovableProvenance：案例 3（无记录有密文），与判定表共用字面量语义。
	adoptCodeUnprovableProvenance = "unprovable_provenance"
	// adoptCodeRecoveryRequired：案例 1 / 2 的原密钥缺失、不符，或密文解不开。
	adoptCodeRecoveryRequired = "recovery_required"
	// adoptCodeKeyPathUnpersisted：需要原密钥但 `secrets.master_key_file` 未持久化。
	adoptCodeKeyPathUnpersisted = "master_key_path_unpersisted"
	// adoptCodeKeyPathRelative：需要原密钥但持久化路径是相对路径。
	adoptCodeKeyPathRelative = "master_key_path_relative"
	// adoptCodeKeyPathMismatch：配置路径与 `--master-key-file` 不一致。
	adoptCodeKeyPathMismatch = "master_key_path_mismatch"
	// adoptCodeConfigChangedUnattested：迁移准备副本的配置与 adopt-legacy 基线不符，
	// 且没带 `--config-change-ref`（§2.12.0b 受控调整）。
	adoptCodeConfigChangedUnattested = "config_changed_unattested"
	// adoptCodeConfigChangeOutOfScope：有变更引用，但密钥路径以外的字节也变了。
	adoptCodeConfigChangeOutOfScope = "config_change_out_of_scope"
	// adoptCodeConfigAmbiguous：配置重复定义 / 别名 / 合并锚点 / 多文档，不能猜生效值。
	adoptCodeConfigAmbiguous = "config_ambiguous"
	// adoptCodeLegacyEvidenceMissing：记录指向 `instance.secrets_legacy_adopt`，但拿不到
	// 它的成功终态审计（或审计缺配置基线字段）——迁移准备模式不得跳过配置核对。
	adoptCodeLegacyEvidenceMissing = "legacy_adopt_evidence_missing"
	// adoptCodeAdminTokenMissing：token 模式没有显式 token 来源。
	adoptCodeAdminTokenMissing = "admin_token_source_missing"
	// adoptCodeAuditPrewrite：预写 `pending` 失败——不写名册。
	adoptCodeAuditPrewrite = "audit_prewrite_failed"
	// adoptCodeRosterWrite：名册写入或读回失败（审计据此回填 failed）。
	adoptCodeRosterWrite = "roster_write_failed"
	// adoptCodeAuditFinish：名册已写、终态回填失败——报告必须说清"名册已落盘"。
	adoptCodeAuditFinish = "audit_finish_failed"
	// adoptCodeRecoveryFailed：回填上次 pending 行失败。
	adoptCodeRecoveryFailed = "audit_recovery_failed"
	// adoptCodeDataRoot：解析不出当前用户的数据根目录（私密目录无法定位）。
	adoptCodeDataRoot = "instance_data_root_unresolved"
	// adoptCodePrivateDirExists：为新标识推导的私密目录已存在（标识碰撞，拒绝）。
	adoptCodePrivateDirExists = "instance_private_dir_exists"
	// adoptCodeInstanceID：生成候选实例标识失败。
	adoptCodeInstanceID = "instance_id_failed"
)

// adoptKeyPathConfigKey 是主密钥路径所在的配置键——审计只记**键位置**，不记值。
// adoptKeyPathConfigKey names the config key; the audit never records the value.
const adoptKeyPathConfigKey = "secrets.master_key_file"

// adoptRefusal 是带稳定码的拒绝：`Error()` 以码开头（命令面与测试都能逐字断言），
// `Unwrap()` 保留底层错误链，`errors.Is` / `errors.As` 照常可用。
// adoptRefusal keeps the stable code in the error text and the cause in the chain.
type adoptRefusal struct {
	Code string
	Err  error
}

// Error 以码开头返回拒绝文案。
// Error renders the code first so callers and tests can match it verbatim.
func (r *adoptRefusal) Error() string { return r.Code + ": " + r.Err.Error() }

// Unwrap 保留底层错误链。
// Unwrap keeps the underlying cause reachable.
func (r *adoptRefusal) Unwrap() error { return r.Err }

// refuseAdopt 构造一条拒绝。
// refuseAdopt builds one refusal with its stable code.
func refuseAdopt(code, format string, args ...any) error {
	return &adoptRefusal{Code: code, Err: fmt.Errorf(format, args...)}
}

// instanceAdoptDetail 是 `instance.adopt` 的 `detail_json` **预写**投影：候选标识（完整
// 值 + 摘要）、命中判定编号、密钥路径已持久化的事实与密文计数。**不含**任何密钥材料、
// 密文或明文。
//
// 迁移准备模式（记录指向 `instance.secrets_legacy_adopt`）在**终态**再补三字段：
// `target_config_sha256` / `config_change_ref`（仅受控调整过）/ `config_adjusted`。
// `FinishOperation` 带 detail 是**整份替换**而不是合并，所以终态回填会把预写字段原样
// 重新带上。
// instanceAdoptDetail is the pre-written projection: the candidate id plus digest, the
// case number, whether the key path is persisted, and the ciphertext count. Migration
// preparation mode adds the three config fields on the terminal write.
type instanceAdoptDetail struct {
	CandidateInstanceID       string `json:"candidate_instance_id"`
	CandidateInstanceIDSHA256 string `json:"candidate_instance_id_sha256"`
	Case                      int    `json:"case"`
	KeyPathPersisted          bool   `json:"key_path_persisted"`
	KeyPathConfigKey          string `json:"key_path_config_key,omitempty"`
	ManagedCiphertexts        int    `json:"managed_ciphertexts"`
	TargetConfigSHA256        string `json:"target_config_sha256,omitempty"`
	ConfigChangeRef           string `json:"config_change_ref,omitempty"`
	ConfigAdjusted            *bool  `json:"config_adjusted,omitempty"`
}

// adoptLegacyDetail 只解析 adopt-legacy 审计里参与配置核对的字段；其余字段（备份路径、
// 清单哈希、密钥摘要……）不属于本命令的判据。
// adoptLegacyDetail picks just the config baselines out of the adopt-legacy detail.
type adoptLegacyDetail struct {
	BackupConfigSHA256         string `json:"backup_config_sha256"`
	ConfigWithoutKeyPathSHA256 string `json:"config_without_key_path_sha256"`
}

// adoptMigrationConfig 是迁移准备模式的配置核对结论：`Checked` 为 false 表示这是一次
// 普通存量实例采纳，审计终态不写配置字段。
// adoptMigrationConfig is the migration-mode verdict; Checked=false means the adoption
// is an ordinary one and carries no config fields.
type adoptMigrationConfig struct {
	Checked  bool
	Target   string
	Adjusted bool
	Ref      string
}

// adoptInstanceIDSHA256 对候选标识求摘要：写入与崩溃恢复核对共用同一个函数，避免
// 两处各算一份导致永远对不上。
// adoptInstanceIDSHA256 is shared by the pre-write and the crash-recovery comparison.
func adoptInstanceIDSHA256(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// instanceAdoptPort 实现 cli.InstanceAdoption。无状态：库句柄每次调用自建自关。
// instanceAdoptPort is stateless; every call opens and releases its own handles.
type instanceAdoptPort struct {
	Logger *slog.Logger

	// Now 是可注入的时钟（报告时刻、审计时间与候选标识前缀）；nil 时取 time.Now。
	// Now is the injectable clock behind the report instant and the audit timestamps.
	Now func() time.Time
}

// logger / clock 保证零值 instanceAdoptPort 也能工作（与其它适配器同法）。
// logger / clock keep a zero-valued adapter usable, as every other adapter does.
func (p instanceAdoptPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

func (p instanceAdoptPort) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// adoptFacts 是只读阶段采集的**全部**事实：实例加密记录、现存密文、待恢复操作与名册。
// 采集完即关闭只读句柄，后续写阶段重新开可写句柄——两个阶段不复用同一个句柄。
// adoptFacts is everything the read-only pass collects before it closes the handle.
type adoptFacts struct {
	record    storage.InstanceSecretRecord
	hasRecord bool
	// legacyAdopt 是记录所指向的 `instance.secrets_legacy_adopt` 操作行：非 nil 即
	// 迁移准备模式，必须核对副本配置（§2.12.0b）。记录来自其他动作时为 nil。
	// legacyAdopt marks migration preparation mode; a record from any other action
	// leaves it nil and the config check does not apply.
	legacyAdopt   *core.OperationRecord
	sealed        []storage.SealedCredential
	pending       []core.OperationRecord
	roster        instanceMarker
	rosterPresent bool
}

// Adopt 为存量工作区建立实例名册。
//
// 顺序是判据的一部分：配置 → 库存在 → 锁 → 只读事实采集 → 崩溃恢复 → 名册已存在 →
// 四判定 → 密钥路径与 token → 只在全部通过后才开可写句柄写审计与名册。
// The order is itself a criterion: every refusal happens before the first write.
func (p instanceAdoptPort) Adopt(ctx context.Context, req cli.InstanceAdoptRequest) (cli.InstanceAdoptReport, error) {
	report := cli.InstanceAdoptReport{AdoptedAt: p.clock()}

	workspaceArg := strings.TrimSpace(req.Workspace)
	if workspaceArg == "" {
		return report, p.refuse(report, refuseAdopt(adoptCodeWorkspaceRequired,
			"instance adopt：--workspace 为空——采纳只服务已有 SQLite 库的存量实例"))
	}
	ws, err := filepath.Abs(workspaceArg)
	if err != nil {
		return report, p.refuse(report, refuseAdopt(adoptCodeWorkspaceMissing,
			"instance adopt：工作区 %q 无法规范化为绝对路径", workspaceArg))
	}
	report.Workspace = ws
	// `--workspace` 已经是工作区根（`<WorkDir>/.basalt`），名册就在它下面——
	// 不能再过 `instanceMarkerPath`（那个函数要的是 WorkDir，会多拼一层 `.basalt`）。
	report.RosterPath = filepath.Join(ws, instanceMarkerFileName)

	configPath := strings.TrimSpace(req.ConfigPath)
	if configPath == "" {
		// §3.1.8c 路径口径：`basalt.yaml` 是工作区的**同级**部署配置，不是工作区资产。
		configPath = filepath.Join(filepath.Dir(ws), configFileName)
	}
	if abs, absErr := filepath.Abs(configPath); absErr == nil {
		configPath = abs
	}
	report.ConfigPath = configPath

	cfg, err := cli.LoadConfig(configPath, os.LookupEnv)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return report, p.refuse(report, refuseAdopt(adoptCodeConfigMissing,
			"instance adopt：找不到配置文件 %s——采纳需要 serve 同款配置（secrets / http.admin_auth）", configPath))
	default:
		return report, p.refuse(report, refuseAdopt(adoptCodeConfigInvalid,
			"instance adopt：装载配置 %s 失败: %v", configPath, err))
	}

	dbPath := filepath.Join(ws, databaseFileName)
	info, statErr := os.Stat(dbPath)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return report, p.refuse(report, refuseAdopt(adoptCodeWorkspaceMissing,
			"instance adopt：工作区 %s 里没有 SQLite 库（%s）——采纳只服务存量实例，本命令不建库", ws, databaseFileName))
	case statErr != nil:
		return report, p.refuse(report, refuseAdopt(adoptCodeStorageUnavailable,
			"instance adopt：检查工作区库 %s: %v", dbPath, statErr))
	case info.IsDir():
		return report, p.refuse(report, refuseAdopt(adoptCodeWorkspaceMissing,
			"instance adopt：%s 是目录，不是 SQLite 库文件", dbPath))
	}

	// 锁文件是 `O_CREATE`（不截断）——这一条固有写入已在 run-state 披露；除它之外，
	// 任何拒绝路径都不落一个字节。
	report, err = withInstanceLock(ws, func() (cli.InstanceAdoptReport, error) {
		return p.adoptLocked(ctx, cfg, req, report)
	})
	if err != nil {
		var busy *workspace.LockBusyError
		if errors.As(err, &busy) {
			return report, p.refuse(report, refuseAdopt(adoptCodeWorkspaceBusy,
				"instance adopt：工作区 %s 的实例锁被持有（%s）——请先停止 serve 再重跑", ws, busy.Error()))
		}
		return report, err
	}
	return report, nil
}

// adoptLocked 是持锁区间的主体：全部拒绝先于写，写阶段只做 pending 回填与名册落盘。
// adoptLocked is the whole body under the instance lock.
func (p instanceAdoptPort) adoptLocked(ctx context.Context, cfg *cli.Config, req cli.InstanceAdoptRequest, report cli.InstanceAdoptReport) (cli.InstanceAdoptReport, error) {
	ws := report.Workspace
	dbPath := filepath.Join(ws, databaseFileName)

	facts, err := p.readAdoptFacts(ctx, dbPath, ws)
	if err != nil {
		return report, p.refuse(report, err)
	}

	// 崩溃恢复先于"名册已存在"判定：上次写到一半的操作行必须拿到终态，重跑才不会
	// 留下永远 pending 的孤儿行；随后仍按"不覆盖既有名册"拒绝本次采纳。
	// Recovery runs before the roster-exists refusal so a crashed attempt gets a
	// terminal state; the current run still refuses to overwrite the roster.
	recovered, err := p.recoverPendingAdopts(ctx, dbPath, facts)
	if err != nil {
		return report, p.refuse(report, err)
	}
	report.RecoveredPending = recovered

	if facts.rosterPresent {
		return report, p.refuse(report, refuseAdopt(adoptCodeRosterExists,
			"instance adopt：工作区 %s 已有实例名册（instance_id=%s）——采纳只服务无名册的存量实例，"+
				"绝不覆盖既有身份", ws, facts.roster.InstanceID))
	}

	report.ManagedCiphertexts = len(facts.sealed)
	report.RecordKeyID = facts.record.KeyID
	report.RecordFingerprint = facts.record.Fingerprint
	switch {
	case facts.hasRecord && len(facts.sealed) > 0:
		report.Case = 1
	case facts.hasRecord:
		report.Case = 2
	case len(facts.sealed) > 0:
		report.Case = 3
	default:
		report.Case = 4
	}
	if report.Case == 3 {
		// 案例 3：有密文、无记录。**任何**情况都拒绝写名册——即使密钥能解开密文，
		// 来源仍不可证明，身份与记录只经 `secrets recover-record`（§3.1.8b）。
		return report, p.refuse(report, refuseAdopt(adoptCodeUnprovableProvenance,
			"instance adopt：库里有 %d 条托管密文但没有主密钥初始化记录——来源不可证明，"+
				"拒绝写名册（身份与记录只经 secrets recover-record 处置）", len(facts.sealed)))
	}

	// 配置核对先于密钥加载：迁移准备副本的完整性是**前提**，不是密钥可用性的附带结论。
	// 缺基线 / 缺引用 / 越界修改都在这里拒绝，且此刻尚未打开可写句柄。
	// The config attestation runs before any key work: copy integrity is the premise.
	migration, err := p.resolveMigrationConfig(facts, report.ConfigPath, req.ConfigChangeRef)
	if err != nil {
		return report, p.refuse(report, err)
	}
	report.ConfigChecked = migration.Checked
	report.TargetConfigSHA256 = migration.Target
	report.ConfigChangeRef = migration.Ref
	report.ConfigAdjusted = migration.Adjusted

	explicitPath := strings.TrimSpace(cfg.Secrets.MasterKeyFile)
	paramPath := strings.TrimSpace(req.MasterKeyFile)
	needKey := report.Case == 1 || report.Case == 2
	if report.Case == 4 {
		// 案例 4：盘上存在密钥文件才需要原密钥；只探测、不读取。相对路径按**配置文件
		// 所在目录**换算——装载层就是这么读的；按进程 cwd 探会漏判，进而放过一条本该
		// 由 `master_key_path_relative` 拒绝的部署（T071 报告披露）。
		// The case-4 probe anchors relative paths at the config file's directory, the
		// same way the loader reads them.
		needKey = regularFileExists(adoptKeyProbePath(report.ConfigPath, explicitPath)) ||
			regularFileExists(paramPath)
	}
	var key *MasterKey
	if needKey {
		if explicitPath == "" {
			return report, p.refuse(report, refuseAdopt(adoptCodeKeyPathUnpersisted,
				"instance adopt：需要原密钥，但 %s 未持久化（案例 %d）——采纳只读配置、不代写；"+
					"请先把原密钥路径写入 %s 再重跑", adoptKeyPathConfigKey, report.Case, adoptKeyPathConfigKey))
		}
		if !filepath.IsAbs(explicitPath) {
			return report, p.refuse(report, refuseAdopt(adoptCodeKeyPathRelative,
				"instance adopt：%s 是相对路径——运行期按进程 cwd 解析，不能保证重启一致；"+
					"请改为绝对路径再重跑", adoptKeyPathConfigKey))
		}
		if paramPath != "" && !sameFilePath(paramPath, explicitPath) {
			return report, p.refuse(report, refuseAdopt(adoptCodeKeyPathMismatch,
				"instance adopt：--master-key-file 与 %s 不一致——两者必须指同一个文件（只做互证，不覆盖配置）",
				adoptKeyPathConfigKey))
		}
		loaded, loadErr := LoadMasterKey(explicitPath, ws)
		if loadErr != nil {
			return report, p.refuse(report, refuseAdopt(adoptCodeRecoveryRequired,
				"instance adopt：案例 %d 需要原密钥，但主密钥文件不可用: %v", report.Case, loadErr))
		}
		defer loaded.Zero()
		key = &loaded
		report.KeyFile = explicitPath
		report.KeyPresent = true
		report.KeyID = loaded.KeyID
		fingerprint, fpErr := MasterKeyFingerprint(loaded)
		if fpErr != nil {
			return report, p.refuse(report, refuseAdopt(adoptCodeRecoveryRequired,
				"instance adopt：计算主密钥指纹失败: %v", fpErr))
		}
		report.Fingerprint = fingerprint
	}

	if key != nil && facts.hasRecord {
		if key.KeyID != facts.record.KeyID {
			return report, p.refuse(report, refuseAdopt(adoptCodeRecoveryRequired,
				"instance adopt：主密钥 key_id 与实例记录不符（记录 %s，文件 %s）——"+
					"不得生成替代密钥，请按恢复流程处置", facts.record.KeyID, key.KeyID))
		}
		if report.Fingerprint != facts.record.Fingerprint {
			return report, p.refuse(report, refuseAdopt(adoptCodeRecoveryRequired,
				"instance adopt：主密钥指纹与实例记录不符（记录 %s，文件 %s）——"+
					"不得生成替代密钥，请按恢复流程处置", facts.record.Fingerprint, report.Fingerprint))
		}
	}

	if key != nil && report.Case == 1 {
		// 四判定要求**逐条**试解：记录与 key_id 相符不代表每一条密文都能打开——
		// 只比 key_id 正是要防的假成功。
		unreadable, first, verifyErr := verifySealedCredentials(*key, facts.sealed)
		if verifyErr != nil {
			return report, p.refuse(report, refuseAdopt(adoptCodeStorageUnavailable,
				"instance adopt：核验托管密文失败: %v", verifyErr))
		}
		report.CiphertextsUnreadable = unreadable
		report.FirstFailure = first
		if unreadable > 0 {
			return report, p.refuse(report, refuseAdopt(adoptCodeRecoveryRequired,
				"instance adopt：%d / %d 条托管密文无法用当前主密钥解开（首个失败：%s）——"+
					"四判定要求逐条可解，拒绝写名册", unreadable, len(facts.sealed), first))
		}
		report.CiphertextsVerified = true
	}

	if cfg.HTTP.AdminAuth.Mode == adminAuthModeToken && strings.TrimSpace(cfg.HTTP.AdminAuth.Token) == "" {
		// 采纳会给实例铸新标识，新的私密目录里还没有 admin-token；三处都没有凭据时
		// 下一次 serve 必然拒绝启动——先在写名册之前把这条路堵死。
		return report, p.refuse(report, refuseAdopt(adoptCodeAdminTokenMissing,
			"instance adopt：http.admin_auth.mode=token 但配置没有提供 token——"+
				"采纳后的实例在新私密目录里没有 admin-token 文件，serve 会因三处都无凭据而拒绝启动；"+
				"请先在配置里写 http.admin_auth.token = ${VAR} 再重跑"))
	}

	decision := EvaluateSecrets(SecretsFacts{
		Configured:          explicitPath != "",
		FreshInstance:       false,
		AutoInit:            cfg.Secrets.AutoInitEnabled(),
		Record:              adoptRecordForDecision(facts),
		KeyPresent:          key != nil,
		KeyID:               report.KeyID,
		ManagedCiphertexts:  len(facts.sealed),
		CiphertextsVerified: report.CiphertextsVerified,
	})
	report.State = string(decision.State)
	report.Action = string(decision.Action)
	report.Reason = string(decision.Reason)

	candidate, err := newInstanceID()
	if err != nil {
		return report, p.refuse(report, refuseAdopt(adoptCodeInstanceID,
			"instance adopt：生成候选实例标识失败: %v", err))
	}
	report.InstanceID = candidate
	if privateDir, dirErr := adoptPrivateDir(candidate); dirErr != nil {
		return report, p.refuse(report, refuseAdopt(adoptCodeDataRoot,
			"instance adopt：定位实例私密目录失败: %v", dirErr))
	} else if present, _ := pathExists(privateDir); present {
		return report, p.refuse(report, refuseAdopt(adoptCodePrivateDirExists,
			"instance adopt：为新标识推导的私密目录 %s 已存在——拒绝复用别人的目录，请重跑", privateDir))
	} else {
		report.NewPrivateDir = privateDir
	}

	return p.writeRoster(ctx, dbPath, ws, report, candidate)
}

// resolveMigrationConfig 核对迁移准备副本的配置（§2.12.0b「受控调整」/ C-51）。
//
//   - 普通存量实例（记录不来自 adopt-legacy）→ 不核对，返回 Checked=false；
//   - 记录指向 legacy-adopt 但操作非 success 终态、或审计缺配置基线 → 证据缺失，拒绝；
//   - 当前整文件哈希 == 审计 `backup_config_sha256` → 未调整，目标哈希即当前值；
//   - 不等：缺 `--config-change-ref` → `config_changed_unattested`；
//     带引用：重算 `config_without_key_path_sha256`，与审计值不符 → 越界；
//     相符 → 受控调整，记目标哈希 + 引用 + `config_adjusted=true`。
//
// 四值里本函数只 **读** 审计里的 `backup_config_sha256`（备份自证）与
// `config_without_key_path_sha256`，产出 `target_config_sha256`；`source_config_sha256`
// 与 `copy_config_sha256` 归备份工具与 adopt-legacy，本命令不得回写或替代。
// resolveMigrationConfig attests the copy's config against the adopt-legacy baselines;
// it only reads them and produces the target hash.
func (p instanceAdoptPort) resolveMigrationConfig(facts adoptFacts, configPath, changeRef string) (adoptMigrationConfig, error) {
	if facts.legacyAdopt == nil {
		return adoptMigrationConfig{}, nil
	}
	op := *facts.legacyAdopt
	if op.Status != core.OperationSuccess {
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeLegacyEvidenceMissing,
			"instance adopt：实例记录指向 %s 操作 %s，但该操作不是 success 终态（%s）——"+
				"迁移准备副本缺配置基线，不得跳过核对", core.ActionInstanceSecretsLegacyAdopt, op.OperationID, op.Status)
	}
	var baseline adoptLegacyDetail
	if err := json.Unmarshal(op.DetailJSON, &baseline); err != nil {
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeLegacyEvidenceMissing,
			"instance adopt：%s 操作 %s 的审计详情不可解析: %v",
			core.ActionInstanceSecretsLegacyAdopt, op.OperationID, err)
	}
	if baseline.BackupConfigSHA256 == "" || baseline.ConfigWithoutKeyPathSHA256 == "" {
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeLegacyEvidenceMissing,
			"instance adopt：%s 操作 %s 的审计缺 backup_config_sha256 / "+
				"config_without_key_path_sha256——迁移准备模式无法核对配置",
			core.ActionInstanceSecretsLegacyAdopt, op.OperationID)
	}

	// 同一先例：路径由命令参数给出，读取经所在目录句柄限定（`os.Root`）。
	// Scoped the same way as every other caller-supplied path read.
	raw, readErr := readFileUnder(filepath.Dir(configPath), filepath.Base(configPath))
	if readErr != nil {
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeConfigInvalid,
			"instance adopt：读取副本配置 %s 失败: %v", configPath, readErr)
	}
	current := hexSHA256(raw)
	if current == baseline.BackupConfigSHA256 {
		// 未调整：当前值就是目标值，且它与备份自证值相等。
		return adoptMigrationConfig{Checked: true, Target: current}, nil
	}

	ref := strings.TrimSpace(changeRef)
	if ref == "" {
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeConfigChangedUnattested,
			"instance adopt：副本配置整文件哈希 %s 与 %s 审计的 backup_config_sha256 %s 不符，"+
				"且没有 --config-change-ref——受控调整必须先在变更单登记原 / 目标哈希与审批编号，"+
				"再用该引用重跑", current, core.ActionInstanceSecretsLegacyAdopt, baseline.BackupConfigSHA256)
	}

	masked, maskErr := configMaskedSHA256(configPath)
	switch {
	case errors.Is(maskErr, errConfigKeyPathMissing):
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeKeyPathUnpersisted,
			"instance adopt：副本配置里没有 %s——受控调整的产物必须保留该键", adoptKeyPathConfigKey)
	case errors.Is(maskErr, errConfigAmbiguous):
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeConfigAmbiguous,
			"instance adopt：副本配置存在歧义定义，无法确定生效值: %v", maskErr)
	case maskErr != nil:
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeConfigInvalid,
			"instance adopt：计算副本配置规范化摘要失败: %v", maskErr)
	}
	if masked != baseline.ConfigWithoutKeyPathSHA256 {
		return adoptMigrationConfig{}, refuseAdopt(adoptCodeConfigChangeOutOfScope,
			"instance adopt：副本配置除 %s 以外的字节也变了（规范化摘要不符）——"+
				"受控调整只允许改密钥路径，越界修改即使有变更引用也不得采纳", adoptKeyPathConfigKey)
	}
	// 密钥仍是同一把（同一 key_id / 指纹）由后面的记录比对保证：迁移准备模式下记录必在，
	// key_id 与指纹不符会在那里以 `recovery_required` 拒绝。
	return adoptMigrationConfig{Checked: true, Target: current, Adjusted: true, Ref: ref}, nil
}

// readAdoptFacts 只读打开库并采集全部事实；句柄在本函数内关闭，**不**带出。
// readAdoptFacts opens the database read-only, collects every fact and closes it.
func (p instanceAdoptPort) readAdoptFacts(ctx context.Context, dbPath, ws string) (adoptFacts, error) {
	var facts adoptFacts

	db, err := storage.OpenReadOnly(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return facts, classifyAdoptOpen(dbPath, err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			p.logger().Warn("instance adopt：关闭只读库句柄失败", "error", closeErr)
		}
	}()

	secretsStore := storage.NewInstanceSecretsStore(db)
	facts.record, facts.hasRecord, err = secretsStore.Load(ctx, core.InstanceSecretScopeMasterKey)
	if err != nil {
		return facts, refuseAdopt(adoptCodeStorageUnavailable,
			"instance adopt：读取实例加密记录失败: %v", err)
	}
	mgmt := storage.NewManagementStore(db)
	if facts.hasRecord {
		// 迁移准备模式的**唯一**判据是"记录指向的操作行是 legacy-adopt"：记录写入层强制
		// 带 operation_id，所以查不到行只可能是副本被裁剪 / 篡改——来源不可证明，拒绝
		// （§3.1.8a 拒绝路径 `legacy_adopt_evidence_missing`，fail-closed）。
		// Migration preparation mode is keyed by the record's originating operation. A
		// missing row means the copy is inconsistent, so the source is unprovable.
		op, opErr := mgmt.GetOperation(ctx, facts.record.OperationID)
		switch {
		case opErr == nil && op.Action == core.ActionInstanceSecretsLegacyAdopt:
			facts.legacyAdopt = &op
		case opErr == nil:
			// 记录来自 init / recover-record 等动作：普通存量实例，不做配置核对。
		case errors.Is(opErr, core.ErrOperationNotFound):
			return facts, refuseAdopt(adoptCodeLegacyEvidenceMissing,
				"instance adopt：实例记录指向的管理操作 %s 查不到——记录来源不可证明，"+
					"不得跳过配置核对直接采纳", facts.record.OperationID)
		default:
			return facts, refuseAdopt(adoptCodeStorageUnavailable,
				"instance adopt：读取实例记录的来源操作 %s 失败: %v", facts.record.OperationID, opErr)
		}
	}
	facts.sealed, err = secretsStore.SealedCredentials(ctx)
	if err != nil {
		return facts, refuseAdopt(adoptCodeStorageUnavailable,
			"instance adopt：列出现存托管密文失败: %v", err)
	}
	facts.pending, err = mgmt.PendingOperations(ctx)
	if err != nil {
		return facts, refuseAdopt(adoptCodeStorageUnavailable,
			"instance adopt：列出待恢复管理操作失败: %v", err)
	}
	facts.roster, facts.rosterPresent, err = readInstanceMarkerAt(ws)
	if err != nil {
		return facts, refuseAdopt(adoptCodeRosterInvalid,
			"instance adopt：工作区名册不可用（损坏 / 版本过高 / 标识非法）——不得在坏名册上补写: %v", err)
	}
	return facts, nil
}

// classifyAdoptOpen 把只读打开的状态机结论翻译成采纳的拒绝码。
// classifyAdoptOpen maps the read-only state matrix onto the adopt refusal codes.
func classifyAdoptOpen(dbPath string, err error) error {
	switch {
	case errors.Is(err, core.ErrSchemaOutdated):
		return refuseAdopt(adoptCodeLegacySchema,
			"instance adopt：库 %s 的 schema 低于 v%d——先按 §2.12.0b 用 secrets adopt-legacy "+
				"在隔离副本上升级，再对副本跑迁移准备模式的 instance adopt", dbPath, storage.SupportedSchemaVersion)
	case errors.Is(err, core.ErrSchemaTooNew):
		return refuseAdopt(adoptCodeSchemaTooNew,
			"instance adopt：库 %s 的 schema 高于本二进制支持的 v%d——请升级程序", dbPath, storage.SupportedSchemaVersion)
	case errors.Is(err, core.ErrSchemaCorrupt):
		return refuseAdopt(adoptCodeSchemaCorrupt,
			"instance adopt：库 %s 的 schema_version 表存在但零行——损坏态，拒绝继续", dbPath)
	case errors.Is(err, core.ErrDatabaseMissing):
		return refuseAdopt(adoptCodeWorkspaceMissing,
			"instance adopt：工作区库 %s 不存在——采纳只服务存量实例，本命令不建库", dbPath)
	default:
		return refuseAdopt(adoptCodeStorageUnavailable,
			"instance adopt：只读打开库 %s 失败: %v", dbPath, err)
	}
}

// recoverPendingAdopts 回填上次崩溃留下的 `instance.adopt` pending 行；查证口径见
// 颗粒度文档 §3.1.8a「崩溃恢复」——**只看名册**，不重做、不假定操作行里一定有原值。
// recoverPendingAdopts settles leftover pending adopt rows by looking at the roster only.
func (p instanceAdoptPort) recoverPendingAdopts(ctx context.Context, dbPath string, facts adoptFacts) (bool, error) {
	var targets []core.OperationRecord
	for _, op := range facts.pending {
		if op.Action == core.ActionInstanceAdopt {
			targets = append(targets, op)
		}
	}
	if len(targets) == 0 {
		return false, nil
	}

	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return false, refuseAdopt(adoptCodeStorageUnavailable,
			"instance adopt：打开可写库回填未完成的操作失败: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			p.logger().Warn("instance adopt：关闭可写库句柄失败", "error", closeErr)
		}
	}()

	store := storage.NewManagementStore(db)
	now := p.clock()
	for _, op := range targets {
		verification := adjudicateAdoptRecovery(op.DetailJSON, facts.roster, facts.rosterPresent)
		status := verifyOutcome(verification)
		if err := store.RecoverOperation(ctx, op.OperationID, status, verification, nil, now); err != nil {
			return false, refuseAdopt(adoptCodeRecoveryFailed,
				"instance adopt：回填上次未完成的采纳操作 %s 失败: %v", op.OperationID, err)
		}
		// 恢复是管理面高权限变更的收尾，WARN 留痕（可观测性双轨）。
		p.logger().Warn("instance adopt：回填上次未完成的采纳操作",
			"operation_id", op.OperationID, "verification", verification, "status", status)
	}
	return true, nil
}

// adjudicateAdoptRecovery 按名册给出查证结论：名册不存在 → 未生效；名册存在且标识等于
// 预写候选（完整值优先，只有摘要时按摘要比）→ 已生效；解析失败 / 两键皆缺 / 不符 →
// 无法判定（**不**自动重做）。
// adjudicateAdoptRecovery decides the outcome from the roster alone.
func adjudicateAdoptRecovery(detailJSON []byte, roster instanceMarker, rosterPresent bool) string {
	if !rosterPresent {
		return core.VerificationNotApplied
	}
	var detail instanceAdoptDetail
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		return core.VerificationIndeterminate
	}
	switch {
	case detail.CandidateInstanceID != "":
		if roster.InstanceID == detail.CandidateInstanceID {
			return core.VerificationApplied
		}
		return core.VerificationIndeterminate
	case detail.CandidateInstanceIDSHA256 != "":
		if adoptInstanceIDSHA256(roster.InstanceID) == detail.CandidateInstanceIDSHA256 {
			return core.VerificationApplied
		}
		return core.VerificationIndeterminate
	default:
		return core.VerificationIndeterminate
	}
}

// adoptRecordForDecision 把 SQLite 侧记录投影成判定表使用的 PostgreSQL 侧记录：两侧
// 字段集相同（同一张表、同一契约），转换只为满足类型，不新增语义。
// adoptRecordForDecision converts the SQLite record into the decision table's type;
// the field set is identical, so the conversion adds no semantics.
func adoptRecordForDecision(facts adoptFacts) *postgres.InstanceSecretRecord {
	if !facts.hasRecord {
		return nil
	}
	return &postgres.InstanceSecretRecord{
		Scope:         facts.record.Scope,
		KeyID:         facts.record.KeyID,
		Fingerprint:   facts.record.Fingerprint,
		InitializedAt: facts.record.InitializedAt,
		OperationID:   facts.record.OperationID,
	}
}

// verifySealedCredentials 用给定主密钥逐条试解全部现存密文（顺序固定，失败点位可复现），
// 返回不可解条数与第一条失败描述；明文解出即清零，**不**外带。
// verifySealedCredentials decrypts every sealed version in a stable order, zeroing each
// plaintext and reporting only the count and the first failure.
func verifySealedCredentials(key MasterKey, sealed []storage.SealedCredential) (int, string, error) {
	unreadable := 0
	first := ""
	for _, item := range sealed {
		if item.KeyID != key.KeyID {
			unreadable++
			if first == "" {
				first = fmt.Sprintf("凭据 %s v%d 的 key_id=%s 与主密钥 %s 不符",
					item.CredentialID, item.VersionNo, item.KeyID, key.KeyID)
			}
			continue
		}
		aad := CredentialAAD{
			OwnerKind:     core.BindingKind(item.OwnerKind),
			OwnerUID:      core.ResourceUID(item.OwnerUID),
			CredentialID:  item.CredentialID,
			VersionNo:     item.VersionNo,
			TargetBinding: item.TargetBinding,
		}
		plaintext, err := OpenCredential(key, aad, item.Nonce, item.Ciphertext)
		if err != nil {
			unreadable++
			if first == "" {
				first = fmt.Sprintf("凭据 %s v%d 无法用当前主密钥解密（AAD / key_id 或密文不匹配）",
					item.CredentialID, item.VersionNo)
			}
			continue
		}
		zeroCredentialValue(plaintext)
	}
	return unreadable, first, nil
}

// adoptKeyProbePath 把配置里的主密钥路径换算成**盘上探测用**的路径：相对路径按配置
// 文件所在目录解析（装载层 `secrets.master_key_file` 的同一口径），绝对路径原样返回。
// 它只服务案例 4 的"盘上有没有密钥文件"探测，**不**参与任何拒绝判定——拒绝仍只看
// 配置里的原值（空 / 相对 / 与显式参数不符）。
// adoptKeyProbePath anchors a relative key path at the config file's directory for the
// case-4 on-disk probe only; refusals still read the configured value verbatim.
func adoptKeyProbePath(configPath, keyPath string) string {
	if strings.TrimSpace(keyPath) == "" {
		return ""
	}
	if filepath.IsAbs(keyPath) {
		return keyPath
	}
	return filepath.Join(filepath.Dir(configPath), keyPath)
}

// regularFileExists 只探测普通文件是否存在（跟随链接、不读取内容）；任何错误都按
// "不存在"处理——它只用来判断案例 4 是否需要原密钥。
// regularFileExists probes for a regular file without reading it; errors mean "absent".
func regularFileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// adoptPrivateDir 按**候选标识**直算私密目录：名册尚未落盘时不能走
// `defaultInstancePrivateDir`（它要读盘上名册）。
// adoptPrivateDir derives the private directory from the candidate id, not the roster.
func adoptPrivateDir(instanceID string) (string, error) {
	root, err := instanceDataRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, instanceDataDirName, instancesDirName, instanceID), nil
}

// writeRoster 是可写阶段：预写 `pending` → 写名册 → 读回核对 → 回填终态。
//
// 本阶段**不做任何拒绝**——所有拒绝都在只读阶段返回，进入这里的调用已经通过全部判定。
// The write phase pre-writes the pending row, writes the roster, reads it back, and
// finishes the audit; every refusal has already happened earlier.
func (p instanceAdoptPort) writeRoster(ctx context.Context, dbPath, ws string, report cli.InstanceAdoptReport, candidate string) (cli.InstanceAdoptReport, error) {
	now := p.clock()
	entry := instanceAdoptDetail{
		CandidateInstanceID:       candidate,
		CandidateInstanceIDSHA256: adoptInstanceIDSHA256(candidate),
		Case:                      report.Case,
		KeyPathPersisted:          strings.TrimSpace(report.KeyFile) != "",
		KeyPathConfigKey:          adoptKeyPathConfigKey,
		ManagedCiphertexts:        report.ManagedCiphertexts,
	}
	detail, err := json.Marshal(entry)
	if err != nil {
		return report, p.refuse(report, refuseAdopt(adoptCodeAuditPrewrite,
			"instance adopt：序列化审计详情失败: %v", err))
	}

	// 终态 detail：迁移准备模式把配置核对三字段并回**同一份**投影。FinishOperation 带
	// detail 是整份替换而不是合并，所以预写字段必须原样重新带上；普通采纳传 nil
	// （COALESCE 保留预写字节）。
	// The terminal detail re-carries the pre-written fields because the finish write
	// replaces the whole JSON; ordinary adoptions pass nil and keep the pre-write.
	var terminalDetail []byte
	if report.ConfigChecked {
		adjusted := report.ConfigAdjusted
		entry.TargetConfigSHA256 = report.TargetConfigSHA256
		entry.ConfigChangeRef = report.ConfigChangeRef
		entry.ConfigAdjusted = &adjusted
		terminalDetail, err = json.Marshal(entry)
		if err != nil {
			return report, p.refuse(report, refuseAdopt(adoptCodeAuditPrewrite,
				"instance adopt：序列化迁移准备模式审计详情失败: %v", err))
		}
	}

	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return report, p.refuse(report, refuseAdopt(adoptCodeStorageUnavailable,
			"instance adopt：打开可写库失败: %v", err))
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			p.logger().Warn("instance adopt：关闭可写库句柄失败", "error", closeErr)
		}
	}()

	store := storage.NewManagementStore(db)
	actor := cliWriteActor(p.logger())
	opID := core.NewOperationID(now)
	if err := store.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       core.ActionInstanceAdopt,
		TargetKind:   core.TargetKindInstance,
		TargetID:     core.InstanceTargetIdentity,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	}); err != nil {
		return report, p.refuse(report, refuseAdopt(adoptCodeAuditPrewrite,
			"instance adopt：预写 %s 审计失败——按 fail-closed 不写名册: %v", core.ActionInstanceAdopt, err))
	}
	report.AuditOperationID = opID

	if err := writeInstanceRoster(ws, candidate); err != nil {
		// 名册没写成功：审计回填 failed（动作未生效），错误按 roster_write_failed 上抛。
		p.finishAdopt(ctx, store, opID, core.OperationFailed, adoptCodeRosterWrite, err)
		return report, p.refuse(report, refuseAdopt(adoptCodeRosterWrite,
			"instance adopt：写名册失败（操作 %s 已回填 failed）: %v", opID, err))
	}

	// 读回核对：`O_EXCL` 成功不等于内容正确——名册是身份的唯一事实源，必须读回逐项比对。
	marker, present, readErr := readInstanceMarkerAt(ws)
	if readErr != nil || !present || marker.InstanceID != candidate {
		reason := fmt.Sprintf("名册读回不符（present=%t, instance_id=%q）", present, marker.InstanceID)
		if readErr != nil {
			reason = fmt.Sprintf("名册读回失败: %v", readErr)
		}
		cause := errors.New(reason)
		p.finishAdopt(ctx, store, opID, core.OperationFailed, adoptCodeRosterWrite, cause)
		return report, p.refuse(report, refuseAdopt(adoptCodeRosterWrite,
			"instance adopt：%s（操作 %s 已回填 failed）", reason, opID))
	}

	if err := store.FinishOperation(ctx, opID, core.OperationSuccess, "", "", terminalDetail, now); err != nil {
		// 名册已落盘、审计未回填：报告必须说清这一点，操作行留待下次 adopt 按查证回填。
		p.logger().Error("instance adopt：名册已写入，但回填审计终态失败",
			"operation_id", opID, "error", err)
		return report, p.refuse(report, refuseAdopt(adoptCodeAuditFinish,
			"instance adopt：名册已写入（instance_id=%s），但回填审计终态失败（操作 %s）——"+
				"下次运行 instance adopt 会按名册查证并回填: %v", candidate, opID, err))
	}
	return report, nil
}

// writeInstanceRoster 以 `O_CREAT|O_EXCL` 写名册：已存在即失败（**不覆盖**）——即使
// 另一个写入者抢先，也只报错不夺权（标识不可变）。
// writeInstanceRoster writes the roster exclusively and never overwrites one.
func writeInstanceRoster(ws, candidate string) error {
	base, err := os.OpenRoot(ws)
	if err != nil {
		return fmt.Errorf("打开工作区 %s: %w", ws, err)
	}
	defer func() { _ = base.Close() }()

	raw, err := marshalInstanceMarker(instanceMarker{Version: instanceMarkerVersion, InstanceID: candidate})
	if err != nil {
		return err
	}
	created, err := createExclusiveFile(base, instanceMarkerFileName, raw)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("名册 %s 已存在——不覆盖既有身份", filepath.Join(ws, instanceMarkerFileName))
	}
	return nil
}

// finishAdopt 回填失败终态；用 `context.WithoutCancel` 让取消不再改变审计结论——动作
// 已经发生（或已明确失败），审计必须落地。回填本身失败只记 ERROR，不覆盖主错误。
// finishAdopt writes the terminal state even when the caller's context is gone.
func (p instanceAdoptPort) finishAdopt(ctx context.Context, store *storage.ManagementStore, opID string, status core.OperationStatus, resultCode string, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if err := store.FinishOperation(context.WithoutCancel(ctx), opID, status, resultCode, message, nil, p.clock()); err != nil {
		p.logger().Error("instance adopt：回填审计终态失败", "operation_id", opID, "error", err)
	}
}

// refuse 在返回前留一条 WARN：拒绝是命令的正常结局，但它是"人需要知道"的事件，且
// 失败路径的日志与审计必须双轨（只落审计不记日志 = 线上排障黑洞）。审计行只在真正
// 走到写阶段时才有，因此拒绝路径的日志是唯一痕迹。
// refuse logs the refusal as a WARN before returning it.
func (p instanceAdoptPort) refuse(report cli.InstanceAdoptReport, err error) error {
	p.logger().Warn("instance adopt：拒绝",
		"workspace", report.Workspace, "case", report.Case, "error", err)
	return err
}
