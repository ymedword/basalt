package app

// 本文件实现 `db verify --handoff <dir>`：迁移准备副本的**离线**交接核对（§3.1.2 / §3.1.8）。
//
// 三个稳定码只在本报告的**报告面**成立（`docs/contracts/` 没有同名条目，它们不是契约
// 字面量，而是给脚本判卷的稳定 fail-closed 码）：
//
//	instance_identity_missing   缺身份材料——名册不在、库文件不在、目录不在
//	instance_identity_mismatch  材料**对不上**——名册损坏、记录与密钥 / 密文 / 目标配置
//	                            不一致、有密文却无记录（来源不可证明）
//	recovery_required           材料**无法产出或比对**——密钥加载失败、密钥路径缺失或
//	                            非绝对、schema 不可打开、缺 `target_config_sha256` 证据、
//	                            记录来源操作查不到、统一核验 fatal
//
// `<dir>` 的唯一形态是**迁移准备副本的工作区根**（`<copy-root>/.basalt`），同级
// `<copy-root>/basalt.yaml` 是配置——与 `instance adopt` 的 `report.ConfigPath` 同一
// 路径口径（`instance_adopt.go`）。导出包（`manifest.json` + JSONL + `package/`）不是本
// 命令的输入：包内既无名册也无密钥与配置，§3.1.2 要求的逐项核对在包上无法执行；包本身
// 由 `db verify --dump` 复核。命令**不做**父目录下降——猜"哪一层才是工作区"会把两种
// 形态的失败糊成一条"找不到名册"。
// The offline handoff gate. <dir> is the migration-preparation copy's workspace root; an
// export package carries none of the identity material and is checked by db verify --dump.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// 报告面三个稳定码（见文件头）；`Ready` 为 false 时**必有一个**。
// The three stable report codes; a not-ready report always carries exactly one.
const (
	handoffCodeIdentityMissing  = "instance_identity_missing"
	handoffCodeIdentityMismatch = "instance_identity_mismatch"
	handoffCodeRecoveryRequired = "recovery_required"
)

// 五条恢复指引与拒绝一一对应；报告与错误文案都带上它，免得操作者只看得到"不行"。
// Every refusal carries a recovery hint: "it failed" alone does not tell the operator
// which procedure to run next.
const (
	handoffRecoveryWorkspace = "交接核对指向迁移准备副本的**工作区根**（<copy-root>/.basalt），" +
		"不是副本根目录；导出包（manifest.json）用 db verify --dump <pkg> 复核"
	handoffRecoveryRoster = "名册只经 basalt instance adopt 在**同一副本**上建立" +
		"（缺名册不构成可启动），或经 secrets recover-record 从可信备份恢复"
	handoffRecoveryCopy = "从备份**重新复制新副本**再走一遍准备流程——部分迁移的副本不得复用"
	handoffRecoveryKey  = "恢复原主密钥并在副本同级 basalt.yaml 里持久化**绝对路径**后再核对；" +
		"不得自动生成新主密钥、不得静默回落 env"
	handoffRecoveryRecord = "记录只经 secrets recover-record 从可信备份恢复；schema 3～5 存量" +
		"经 secrets adopt-legacy 在隔离副本上补记录（D-40）"
)

// handoffVerifyPort 实现 cli.HandoffVerification。无状态：库句柄每次调用自建自关。
// handoffVerifyPort is stateless; every call opens and releases its own handles.
type handoffVerifyPort struct {
	Logger *slog.Logger

	// Now 是可注入的时钟（报告耗时）；nil 时取 time.Now。
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
}

// logger / clock 保证零值适配器可用（与 secretsRecoverPort 同形）。
// logger / clock keep a zero-valued adapter usable.
func (p handoffVerifyPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

func (p handoffVerifyPort) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// VerifyHandoff 核对交接就绪。
//
// `Ready` 只由本函数在**整条链走完**时置真——任何一处拒绝都提前返回错误，报告因此
// 不会出现"没有错误却是 ready"的中间态。拒绝另记 WARN：审计不落库（本命令零写入），
// 日志是唯一的现场。
// Ready is set only after the whole chain succeeds; every refusal returns early with a
// status-bearing error and leaves a WARN behind, because this command writes nothing.
func (p handoffVerifyPort) VerifyHandoff(ctx context.Context, req cli.HandoffVerifyRequest) (cli.HandoffVerifyReport, error) {
	start := p.clock()
	report := cli.HandoffVerifyReport{Dir: req.Dir}

	err := p.verifyHandoffInto(ctx, req, &report)
	report.Duration = p.clock().Sub(start)
	if err != nil {
		p.logger().Warn("db verify --handoff：未通过",
			"dir", report.Dir, "workspace", report.Workspace, "code", report.Code, "error", err)
		return report, err
	}
	report.Ready = true
	return report, nil
}

// verifyHandoffInto 是核对主体：只负责拒绝，成功就是走到底。
// verifyHandoffInto refuses at the first unmet criterion; falling through is success.
func (p handoffVerifyPort) verifyHandoffInto(ctx context.Context, req cli.HandoffVerifyRequest, report *cli.HandoffVerifyReport) error {
	dirArg := strings.TrimSpace(req.Dir)
	if dirArg == "" {
		// 命令面的模式判定已经挡过一次（空值不算选中 --handoff）；这里只防端口被
		// 别的调用方直连，按"前置不满足"给退出码 2。
		return cli.NewExitError(2, "db verify --handoff：--handoff 为空——请给出迁移准备副本的工作区根（<copy-root>/.basalt）")
	}
	ws, err := filepath.Abs(dirArg)
	if err != nil {
		return refuseHandoff(report, handoffCodeIdentityMissing,
			fmt.Sprintf("工作区 %q 无法规范化为绝对路径", dirArg), handoffRecoveryWorkspace)
	}
	report.Workspace = ws
	report.RosterPath = filepath.Join(ws, instanceMarkerFileName)
	// `basalt.yaml` 是工作区的**同级**部署配置，不是工作区资产（§3.1.8c 路径口径）。
	report.ConfigPath = filepath.Join(filepath.Dir(ws), configFileName)

	roster, present, rosterErr := readInstanceMarkerAt(ws)
	switch {
	case rosterErr != nil:
		return refuseHandoff(report, handoffCodeIdentityMismatch,
			fmt.Sprintf("实例名册不可用（损坏 / 版本过高 / 标识非法）: %v", rosterErr),
			handoffRecoveryRoster)
	case !present:
		return refuseHandoff(report, handoffCodeIdentityMissing,
			fmt.Sprintf("工作区 %s 里没有实例名册（%s）——交接核对要求身份已采纳", ws, instanceMarkerFileName),
			handoffRecoveryWorkspace+"；"+handoffRecoveryRoster)
	}
	report.RosterPresent = true
	report.InstanceID = roster.InstanceID

	db, err := p.openCopy(ctx, report, ws)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			p.logger().Warn("db verify --handoff：关闭只读库句柄失败", "error", closeErr)
		}
	}()

	record, hasRecord, err := p.readFacts(ctx, report, db)
	if err != nil {
		return err
	}

	// 迁移准备模式的**唯一**判据是"记录指向的操作行是 legacy-adopt"（与
	// readAdoptFacts 同口径）：普通存量实例没有配置基线，不得假装核对过。
	migration := false
	sourceAction := ""
	if hasRecord {
		op, opErr := storage.NewManagementStore(db).GetOperation(ctx, record.OperationID)
		switch {
		case opErr == nil:
			sourceAction = op.Action
			migration = op.Action == core.ActionInstanceSecretsLegacyAdopt
		case errors.Is(opErr, core.ErrOperationNotFound):
			return refuseHandoff(report, handoffCodeRecoveryRequired,
				fmt.Sprintf("实例记录指向的管理操作 %s 查不到——记录来源不可证明，不得判成交接就绪", record.OperationID),
				handoffRecoveryRecord)
		default:
			return refuseHandoff(report, handoffCodeRecoveryRequired,
				fmt.Sprintf("读取实例记录的来源操作 %s 失败: %v", record.OperationID, opErr),
				handoffRecoveryRecord)
		}
	}

	// 配置只在有密钥 / 密文需要核对的实例上读取：纯 env 实例（无记录、无密文）本就
	// 没有密钥路径可核，读配置只会把"还没初始化"误报成"配置不对"。
	// The config is read only when key material exists; a pure-env instance has nothing
	// to check and must not be failed for it.
	var rawConfig []byte
	if hasRecord {
		rawConfig, err = os.ReadFile(report.ConfigPath)
		if err != nil {
			return refuseHandoff(report, handoffCodeRecoveryRequired,
				fmt.Sprintf("读取副本配置 %s 失败: %v", report.ConfigPath, err),
				"迁移准备副本必须镜像原布局：`<copy-root>/.basalt` 与同级的 basalt.yaml 缺一不可")
		}
	}

	var key *MasterKey
	if hasRecord {
		key, err = p.loadKey(report, rawConfig, record, ws)
		if err != nil {
			return err
		}
		defer key.Zero()
	}

	switch {
	case migration:
		if err := p.checkTargetConfig(ctx, report, db, roster, rawConfig, sourceAction); err != nil {
			return err
		}
	case hasRecord:
		// 普通存量实例：没有配置基线可对。显式写出"未核对"，缺省读法才不会被误当成核过。
		report.ConfigNote = fmt.Sprintf(
			"记录来源为 %s（非 %s）——普通存量实例没有 target_config_sha256 基线，当前 basalt.yaml **未核对**",
			sourceAction, core.ActionInstanceSecretsLegacyAdopt)
	default:
		report.ConfigNote = "尚未初始化主密钥（无记录、无密文）——本次不核对密钥与目标配置"
	}

	return p.checkCredentials(ctx, report, db, key)
}

// refuseHandoff 填好报告的三项拒绝事实后返回带码错误（退出码 1 = 核验不就绪）。
// refuseHandoff fills the three refusal facts and returns the status-bearing error.
func refuseHandoff(report *cli.HandoffVerifyReport, code, detail, recovery string) error {
	report.Ready = false
	report.Code = code
	report.Detail = detail
	report.Recovery = recovery
	return refuseOffline(code, 1, "%s", detail)
}

// openCopy 只读打开副本库，并把状态矩阵的四类结论翻成本报告的语言。
// openCopy maps the read-only state matrix onto the handoff report codes.
func (p handoffVerifyPort) openCopy(ctx context.Context, report *cli.HandoffVerifyReport, ws string) (*storage.DB, error) {
	dbPath := filepath.Join(ws, databaseFileName)
	db, err := storage.OpenReadOnly(ctx, dbPath, storage.SupportedSchemaVersion)
	if err == nil {
		return db, nil
	}
	switch {
	case errors.Is(err, core.ErrDatabaseMissing):
		return nil, refuseHandoff(report, handoffCodeIdentityMissing,
			fmt.Sprintf("工作区 %s 里没有 SQLite 库（%s）——交接核对不建库", ws, databaseFileName),
			handoffRecoveryCopy)
	case errors.Is(err, core.ErrSchemaOutdated):
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("库 %s 的 schema 低于 v%d——先按 §2.12.0b 在隔离副本上跑 secrets adopt-legacy，再核对",
				dbPath, storage.SupportedSchemaVersion),
			"从备份重新复制新副本后先跑 secrets adopt-legacy；部分迁移的副本不得复用")
	case errors.Is(err, core.ErrSchemaTooNew):
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("库 %s 的 schema 高于本二进制支持的 v%d——请用与部署一致的二进制核对",
				dbPath, storage.SupportedSchemaVersion),
			"用与部署一致的二进制重跑核对；版本不符不得判成交接就绪")
	case errors.Is(err, core.ErrSchemaCorrupt):
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("库 %s 的 schema_version 表存在但零行——损坏态，拒绝继续", dbPath),
			handoffRecoveryCopy)
	default:
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("只读打开库 %s 失败: %v", dbPath, err),
			handoffRecoveryCopy)
	}
}

// readFacts 读记录与托管密文两条身份事实，并守住"有密文却无记录"这条不可证明来源线。
// readFacts loads the record and the sealed versions, and refuses provable-provenance
// gaps before anything else runs.
func (p handoffVerifyPort) readFacts(ctx context.Context, report *cli.HandoffVerifyReport, db *storage.DB) (storage.InstanceSecretRecord, bool, error) {
	record, hasRecord, err := storage.NewInstanceSecretsStore(db).Load(ctx, core.InstanceSecretScopeMasterKey)
	if err != nil {
		return record, false, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("读取实例加密记录失败: %v", err), handoffRecoveryRecord)
	}
	report.RecordPresent = hasRecord
	if hasRecord {
		report.RecordKeyID = record.KeyID
		report.RecordFingerprint = record.Fingerprint
	}

	versions, err := storage.NewCredentialStore(db).ListAllVersions(ctx)
	if err != nil {
		return record, false, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("列出现存托管密文失败: %v", err), handoffRecoveryCopy)
	}
	sealed := sealedCredentialVersions(groupCredentialVersions(versions))
	if !hasRecord && sealed > 0 {
		// 案例 3：有密文、无记录。即使密钥能解开也不得判成就绪——来源不可证明。
		return record, false, refuseHandoff(report, handoffCodeIdentityMismatch,
			fmt.Sprintf("库里有 %d 个托管密文版本但没有主密钥初始化记录——来源不可证明，不得判成交接就绪", sealed),
			handoffRecoveryRecord)
	}
	return record, hasRecord, nil
}

// loadKey 按 §3.1.2 核对密钥路径与权限，并比对 `key_id` / `fingerprint`。
//
// 路径规则与 `instance adopt` 同口径：必须持久化在配置里、必须是**绝对路径**。
// `LoadMasterKey` 相对路径按进程 cwd 解析，离线核对因此不可复现——宁可拒绝也不猜。
// The key path must be persisted, absolute and loadable; LoadMasterKey itself enforces
// the file's type, location, link count and permission bits.
func (p handoffVerifyPort) loadKey(report *cli.HandoffVerifyReport, rawConfig []byte, record storage.InstanceSecretRecord, ws string) (*MasterKey, error) {
	value, err := masterKeyPathValue(rawConfig)
	switch {
	case errors.Is(err, errConfigKeyPathMissing):
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("副本配置里没有 %s——交接要求密钥路径已持久化", adoptKeyPathConfigKey),
			handoffRecoveryKey)
	case err != nil:
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("副本配置的 %s 不可判定: %v", adoptKeyPathConfigKey, err), handoffRecoveryKey)
	}

	// `${VAR}` 用装载层**同一实现**展开（与在线入口同口径）；展开失败即前置不满足，
	// 不静默当成空串——那会让"变量没设"看起来像"没配密钥路径"。
	expanded, err := cli.ResolveEnvPlaceholder(value, adoptKeyPathConfigKey, os.LookupEnv)
	if err != nil {
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("展开 %s 的占位符失败（按本进程环境核对）: %v", adoptKeyPathConfigKey, err),
			handoffRecoveryKey)
	}
	keyPath := strings.TrimSpace(expanded)
	report.KeyFile = keyPath
	if keyPath == "" {
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("%s 为空——交接要求密钥路径已持久化", adoptKeyPathConfigKey), handoffRecoveryKey)
	}
	// 已持久化与可用性是两件事：相对路径同样"已写进配置"，只是离线核对不可复现。
	// 报告分开表达，运维才不会把"没配"与"配错了"混成一件事。
	// Persisted and usable are distinct facts; the report keeps them apart.
	report.KeyPathPersisted = true
	if !filepath.IsAbs(keyPath) {
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("%s 是相对路径 %q——运行期按进程 cwd 解析，离线核对不可复现；请改为绝对路径",
				adoptKeyPathConfigKey, keyPath), handoffRecoveryKey)
	}

	loaded, err := LoadMasterKey(keyPath, ws)
	if err != nil {
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("主密钥文件不可用（路径 / 类型 / 权限 / 内容任一项不满足）: %v", err),
			handoffRecoveryKey)
	}
	report.KeyPresent = true
	report.KeyID = loaded.KeyID
	fingerprint, err := MasterKeyFingerprint(loaded)
	if err != nil {
		loaded.Zero()
		return nil, refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("计算主密钥指纹失败: %v", err), handoffRecoveryKey)
	}
	report.Fingerprint = fingerprint
	if loaded.KeyID != record.KeyID || fingerprint != record.Fingerprint {
		loaded.Zero()
		return nil, refuseHandoff(report, handoffCodeIdentityMismatch,
			fmt.Sprintf("主密钥与记录不符（记录 key_id=%s 指纹=%s；密钥 key_id=%s 指纹=%s）",
				record.KeyID, record.Fingerprint, loaded.KeyID, fingerprint),
			handoffRecoveryKey)
	}
	return &loaded, nil
}

// checkTargetConfig 核对迁移准备副本的目标配置：当前整文件哈希必须等于 `instance.adopt`
// 审计里的 `target_config_sha256`——采纳之后任何配置改动都会让它失效（§2.12.0b）。
// checkTargetConfig compares the raw config bytes against the adopt audit's target hash:
// any edit after adoption invalidates the attestation.
func (p handoffVerifyPort) checkTargetConfig(ctx context.Context, report *cli.HandoffVerifyReport, db *storage.DB, roster instanceMarker, rawConfig []byte, sourceAction string) error {
	report.ConfigRequired = true
	target, err := handoffAdoptTarget(ctx, storage.NewManagementStore(db), roster)
	if err != nil {
		return refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("取 %s 的 target_config_sha256 失败: %v", core.ActionInstanceAdopt, err),
			handoffRecoveryRecord)
	}
	report.TargetConfigSHA256 = target
	report.CurrentConfigSHA256 = hexSHA256(rawConfig)
	if report.CurrentConfigSHA256 != target {
		return refuseHandoff(report, handoffCodeIdentityMismatch,
			fmt.Sprintf("当前 basalt.yaml 整文件哈希 %s 与 %s 审计的 target_config_sha256 %s 不符——"+
				"采纳之后配置又被改过", report.CurrentConfigSHA256, core.ActionInstanceAdopt, target),
			"只能从备份重新复制副本、带新的变更记录重做（受控调整仅限 adopt-legacy 之后、"+
				"instance adopt 之前，且只改 "+adoptKeyPathConfigKey+"）；不得静默接受")
	}
	report.ConfigChecked = true
	report.ConfigNote = fmt.Sprintf("当前 basalt.yaml 与 %s 审计的 target_config_sha256 一致",
		core.ActionInstanceAdopt)
	if sourceAction != core.ActionInstanceSecretsLegacyAdopt {
		// 迁移标志来自来源操作，走不到这里；留一行防御，免得判据被改坏后静默错档。
		report.ConfigNote += "（注意：记录来源操作是 " + sourceAction + "）"
	}
	return nil
}

// handoffAdoptTarget 在 `instance.adopt` 审计里找属于该名册的那一条，取目标配置哈希。
//
// 列表按 `created_at DESC` 倒序，首次命中即最新一次采纳；`candidate_instance_id` 与
// 摘要两路匹配与 `adjudicateAdoptRecovery` 同口径。
// The list is newest-first; the first row matching the roster by id or digest wins.
func handoffAdoptTarget(ctx context.Context, mgmt *storage.ManagementStore, roster instanceMarker) (string, error) {
	ops, err := mgmt.ListOperations(ctx, core.OperationFilter{
		Action: core.ActionInstanceAdopt,
		Limit:  200,
	})
	if err != nil {
		return "", fmt.Errorf("列出 %s 审计失败: %w", core.ActionInstanceAdopt, err)
	}
	digest := adoptInstanceIDSHA256(roster.InstanceID)
	for _, op := range ops {
		var detail instanceAdoptDetail
		if err := json.Unmarshal(op.DetailJSON, &detail); err != nil {
			// 审计详情不可解析：无法判定它属于谁，按证据缺失拒绝，不猜。
			return "", fmt.Errorf("操作 %s 的审计详情不可解析: %w", op.OperationID, err)
		}
		if detail.CandidateInstanceID != roster.InstanceID && detail.CandidateInstanceIDSHA256 != digest {
			continue
		}
		if op.Status != core.OperationSuccess {
			return "", fmt.Errorf("操作 %s 不是 success 终态（%s）——目标配置哈希未定稿",
				op.OperationID, op.Status)
		}
		target := strings.TrimSpace(detail.TargetConfigSHA256)
		if target == "" {
			return "", fmt.Errorf("操作 %s 的审计缺 target_config_sha256——迁移准备副本无配置基线",
				op.OperationID)
		}
		return target, nil
	}
	return "", fmt.Errorf("最近 200 条管理操作里没有属于实例 %s 的 %s 审计——"+
		"该副本的身份不是经 instance adopt 建立的", roster.InstanceID, core.ActionInstanceAdopt)
}

// checkCredentials 走**同一个** VerifyCredentials（在线与离线共用，§2.12.1）并落报告。
// 两组计数都为零才是就绪：`Failed` 涵盖历史密文，`CurrentFailed` 涵盖当前可用性。
// checkCredentials runs the shared verifier; both groups must be clean.
func (p handoffVerifyPort) checkCredentials(ctx context.Context, report *cli.HandoffVerifyReport, db *storage.DB, key *MasterKey) error {
	identities, err := storage.NewCredentialStore(db).ListAll(ctx)
	if err != nil {
		return refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("列出托管凭据身份失败: %v", err), handoffRecoveryCopy)
	}
	versions, err := storage.NewCredentialStore(db).ListAllVersions(ctx)
	if err != nil {
		return refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("列出托管凭据版本失败: %v", err), handoffRecoveryCopy)
	}
	channels, err := storage.NewNotifyChannelRepo(db).ListNotifyChannels(ctx)
	if err != nil {
		return refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("列出通知渠道失败: %v", err), handoffRecoveryCopy)
	}

	credentials, err := VerifyCredentials(ctx, CredentialVerifyInput{
		MasterKey:  key,
		Identities: identities,
		Versions:   versions,
		Published:  storage.NewResourceStore(db),
		Channels:   channels,
		// 与在线入口同口径：按**本进程**环境判占位符存在性，报告因此标明这一点。
		Env:    os.LookupEnv,
		Logger: p.logger(),
	})
	report.Credentials = cliSecretsVerifyReport(credentials)
	if err != nil {
		return refuseHandoff(report, handoffCodeRecoveryRequired,
			fmt.Sprintf("统一凭据核验无法完成: %v", err), handoffRecoveryKey)
	}
	if credentials.Failed > 0 || credentials.CurrentFailed > 0 {
		return refuseHandoff(report, handoffCodeIdentityMismatch,
			fmt.Sprintf("托管凭据核验未通过：历史密文 %d 条失败、当前可用性 %d 条失败"+
				"（按本进程环境核对，明细见 WARN 日志）",
				credentials.Failed, credentials.CurrentFailed),
			"逐条按 WARN 日志的分类处置；密文整体不可解时按 C-38 / K1～K2 恢复**原**主密钥，"+
				"不得自动生成新密钥")
	}
	return nil
}
