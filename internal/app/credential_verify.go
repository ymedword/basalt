package app

// 本文件是模块卡 §2.12.1（T141 / C-38 / D-32）「凭据校验的两个对象」的**唯一**实现：
// `secrets verify`（在线，读运行库）与 `db verify --handoff`（离线，读迁移副本）都调
// `VerifyCredentials`，**不得**各写一份。两个报告对象的边界：
//
//	(a) 历史密文完整性：逐 `credential_versions` 行——密文在的版本必须能按 AAD 字段
//	    （owner_kind / owner_uid / credential_id / version_no / target_binding）解密，
//	    且 `key_id` 与当前主密钥一致；口径与启动期 `verifySealedCredentials` 相同。
//	    正常撤销（撤销应清空密文）且确无密文的版本只计入 `Revoked`，不参与解密、
//	    不计失败。
//	(b) 当前可用性：逐 **active** 身份——当前版本可解且目标绑定仍与当前发布资源
//	    一致（模型连接按当前定义重算、MCP 槽位按 `binding_key` 归位、通知渠道按
//	    渠道引用核对），env 模式的模型连接另核对占位符在进程环境里就位。
//
// 输出纪律：返回值只含计数；日志只点名类别、资源名、槽位名与凭据 ID。明文、密文、
// nonce、主密钥片段一律不进返回值、日志与错误文案。
// This file is the single implementation of the two report objects of §2.12.1: history
// integrity over every sealed version and current usability of every active identity.
// Only counts and category names leave this file; no credential material ever does.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// (b) 的四个原因码与模块卡 §2.12.1 逐字一致：`key_revoked` 落契约映射到既有的
// `CREDENTIAL_REVOKED`（托管凭据不可用），`env_missing` 已在 openapi 内
// （mcp test record 的 `failure_class`）；`target_mismatch` / `binding_missing`
// 由 D-15 裁决以本文件为唯一枚举处。
// The four (b) reason codes are verbatim from §2.12.1; key_revoked maps onto the
// existing CREDENTIAL_REVOKED contract code.
const (
	credentialReasonTargetMismatch = "target_mismatch"
	credentialReasonBindingMissing = "binding_missing"
	credentialReasonKeyRevoked     = "key_revoked"
	credentialReasonEnvMissing     = "env_missing"
)

// credentialVersionOutcome 是 (a) 对单个版本行的判定结果；(b) 复用同一份结果，
// **不**二次解密——两个报告对象读的是同一批密文，结论必须同源。
// credentialVersionOutcome is the per-version verdict shared by both report objects.
type credentialVersionOutcome int

const (
	credentialVersionUsable credentialVersionOutcome = iota
	// credentialVersionRevoked 是正常撤销（无密文）：审计上保留行，材料已清空。
	credentialVersionRevoked
	// credentialVersionRevokedWithCiphertext 是异常：撤销必须清空密文。
	credentialVersionRevokedWithCiphertext
	credentialVersionMissingCiphertext
	credentialVersionKeyMismatch
	credentialVersionAADMismatch
)

// CredentialPublishedResources 是核验侧读"当前发布定义"的窄面：在线入口传
// `*postgres.ResourceStore`，离线入口传迁移副本的等价实现（`db verify --handoff`）。
// CredentialPublishedResources is the narrow read face over published resources; both
// entry points hand in their own store, the judging stays here.
type CredentialPublishedResources interface {
	PointerByUID(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) (core.ResourcePointer, error)
	VersionByID(ctx context.Context, id int64) (core.ResourceVersion, error)
}

// CredentialVerifyInput 是统一核验的全部输入：数据行由调用方按入口读好，本实现只做
// 判定，不再访问任何存储（离线入口因此不需要 DSN，也不需要实例已启动）。
// CredentialVerifyInput carries pre-loaded rows; the verifier only judges them, so the
// offline entry point needs neither a DSN nor a running instance.
type CredentialVerifyInput struct {
	// MasterKey 是当前实例的主密钥；存在密文版本时**必须**提供（nil 即报错，
	// 不静默跳过解密）。没有密文时允许为 nil——纯 env 实例的 `secrets verify`
	// 不应因为缺密钥文件而失败。
	// MasterKey is required whenever any version carries ciphertext.
	MasterKey  *MasterKey
	Identities []core.CredentialRecord
	Versions   []core.CredentialVersionRecord
	Published  CredentialPublishedResources
	Channels   []core.NotifyChannel
	// Env 是进程环境读取面（nil 时用 os.LookupEnv）：只读占位符存在性，
	// 读到的值不外传、不落日志。
	// Env reads the process environment; nil falls back to os.LookupEnv.
	Env    func(string) (string, bool)
	Logger *slog.Logger
}

// VerifyCredentials 执行 (a) + (b) 两段核验并返回分类计数。它是两个入口的公共实现：
// 除 fatal 级错误（读取输入本身不成立，如"有密文但没给主密钥"）外，一切异常都进
// 报告计数并逐条记 WARN——校验命令的价值在于"数出哪里坏了"，而不是首个错误即退出。
// VerifyCredentials runs both passes; only a fatal input error aborts, every anomaly is
// counted and logged so one bad row does not hide the rest.
func VerifyCredentials(ctx context.Context, in CredentialVerifyInput) (SecretsVerifyReport, error) {
	report := SecretsVerifyReport{}
	identities := append([]core.CredentialRecord(nil), in.Identities...)
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].ID != identities[j].ID {
			return identities[i].ID < identities[j].ID
		}
		return identities[i].BindingKey < identities[j].BindingKey
	})
	versionsByCredential := groupCredentialVersions(in.Versions)

	sealed := sealedCredentialVersions(versionsByCredential)
	if sealed > 0 && in.MasterKey == nil {
		return SecretsVerifyReport{}, fmt.Errorf(
			"%w: 存在 %d 个密文版本但未提供主密钥", core.ErrCredentialUnavailable, sealed)
	}
	logger := in.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// (a) 历史密文完整性：逐版本判定；结果表供 (b) 复用。
	outcomes := make(map[string]credentialVersionOutcome)
	for _, identity := range identities {
		versions := versionsByCredential[identity.ID]
		if len(versions) == 0 {
			// 身份行没有任何版本行：写路径保证不会出现，出现即是数据损坏。
			report.Failed++
			report.MissingCiphertext++
			logger.Warn("secrets verify：凭据校验失败",
				"category", "missing_ciphertext", "kind", string(identity.OwnerKind),
				"resource", identity.OwnerName, "slot", identity.BindingKey,
				"credential", identity.ID, "detail", "凭据身份没有任何版本行")
			continue
		}
		for _, version := range versions {
			classifyCredentialVersion(&report, outcomes, identity, version, in.MasterKey, logger)
		}
	}

	// (b) 当前可用性：只统计 active 身份（撤销 / 退役身份的版本归 (a)）。
	lookup := in.Env
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, identity := range identities {
		if identity.State != core.CredentialIdentityActive {
			continue
		}
		report.CurrentChecked++
		reason, detail := currentCredentialProblem(ctx, in, identity, versionsByCredential, outcomes, lookup)
		if reason == "" {
			continue
		}
		report.CurrentFailed++
		switch reason {
		case credentialReasonTargetMismatch:
			report.TargetMismatch++
		case credentialReasonBindingMissing:
			report.BindingMissing++
		case credentialReasonEnvMissing:
			report.EnvMissing++
		default:
			report.KeyRevoked++
		}
		logger.Warn("secrets verify：当前凭据不可用",
			"reason", reason, "kind", string(identity.OwnerKind),
			"resource", identity.OwnerName, "slot", identity.BindingKey,
			"credential", identity.ID, "detail", detail)
	}
	return report, nil
}

// groupCredentialVersions 按凭据 ID 分组并按版本号升序排列：顺序是报告稳定性的
// 一部分（同一次运行两次输出必须同序）。
// groupCredentialVersions groups rows per credential in ascending version order.
func groupCredentialVersions(versions []core.CredentialVersionRecord) map[string][]core.CredentialVersionRecord {
	grouped := make(map[string][]core.CredentialVersionRecord, len(versions))
	for _, version := range versions {
		grouped[version.CredentialID] = append(grouped[version.CredentialID], version)
	}
	for id := range grouped {
		sort.Slice(grouped[id], func(i, j int) bool {
			return grouped[id][i].VersionNo < grouped[id][j].VersionNo
		})
	}
	return grouped
}

// sealedCredentialVersions 数带密文的版本：>0 时就**必须**有主密钥。
// sealedCredentialVersions counts versions that carry ciphertext.
func sealedCredentialVersions(grouped map[string][]core.CredentialVersionRecord) int {
	sealed := 0
	for _, versions := range grouped {
		for _, version := range versions {
			if len(version.Ciphertext) > 0 {
				sealed++
			}
		}
	}
	return sealed
}

// credentialVersionKey 是判定结果表的键：`凭据 ID + 版本号`（版本号逐凭据从 1 起）。
// credentialVersionKey keys the outcome table by (credential id, version no).
func credentialVersionKey(credentialID string, versionNo int) string {
	return credentialID + "\x00" + strconv.Itoa(versionNo)
}

// classifyCredentialVersion 判定单个版本行并更新 (a) 的计数与结果表。
//
// 分类优先级（互斥，前者命中即返回）：正常撤销 → 密文缺失 → 撤销仍留密文 →
// key_id 不符 → AAD / 密文解不开 → 可用（非当前版本另计 `HistoricalOK`）。
// classifyCredentialVersion classifies one version row for the history pass; the
// categories are mutually exclusive and checked in priority order.
func classifyCredentialVersion(report *SecretsVerifyReport,
	outcomes map[string]credentialVersionOutcome, identity core.CredentialRecord,
	version core.CredentialVersionRecord, masterKey *MasterKey, logger *slog.Logger,
) {
	key := credentialVersionKey(identity.ID, version.VersionNo)
	sealed := len(version.Ciphertext) > 0
	switch {
	case !sealed && version.State == core.CredentialVersionRevoked:
		// 撤销的语义就是"材料已清空"：这是正常态，不参与解密、不计失败。
		report.Revoked++
		outcomes[key] = credentialVersionRevoked
		return
	case !sealed:
		// 非撤销版本却没有密文：写路径不会产生这种行，出现即是数据损坏。
		report.Checked++
		report.MissingCiphertext++
		report.Failed++
		outcomes[key] = credentialVersionMissingCiphertext
		warnCredentialVersion(logger, identity, version, "missing_ciphertext", "密文缺失但版本状态不是 revoked")
		return
	case version.State == core.CredentialVersionRevoked:
		report.Checked++
		report.RevokedWithCiphertext++
		report.Failed++
		outcomes[key] = credentialVersionRevokedWithCiphertext
		warnCredentialVersion(logger, identity, version, "revoked_with_ciphertext", "已撤销的版本仍保留密文（撤销应清空）")
		return
	}

	report.Checked++
	if masterKey.KeyID != version.KeyID {
		report.KeyIDMismatch++
		report.Failed++
		outcomes[key] = credentialVersionKeyMismatch
		warnCredentialVersion(logger, identity, version, "key_id_mismatch", "版本的 key_id 与当前主密钥不一致")
		return
	}
	aad := CredentialAAD{
		OwnerKind:     identity.OwnerKind,
		OwnerUID:      identity.OwnerUID,
		CredentialID:  identity.ID,
		VersionNo:     version.VersionNo,
		TargetBinding: version.TargetBinding,
	}
	plaintext, err := OpenCredential(*masterKey, aad, version.Nonce, version.Ciphertext)
	if err != nil {
		report.AADMismatch++
		report.Failed++
		outcomes[key] = credentialVersionAADMismatch
		warnCredentialVersion(logger, identity, version, "aad_mismatch", "版本无法解密（AAD 字段或密文不匹配）")
		return
	}
	zeroCredentialValue(plaintext)
	outcomes[key] = credentialVersionUsable
	if version.VersionNo != identity.CurrentVersion {
		report.HistoricalOK++
	}
}

// warnCredentialVersion 记一条 (a) 的 WARN：类别 + 资源 + 槽位 + 凭据 ID + 版本号，
// **不**带 nonce、密文、明文或密钥片段。
// warnCredentialVersion logs one history anomaly without any credential material.
func warnCredentialVersion(logger *slog.Logger, identity core.CredentialRecord,
	version core.CredentialVersionRecord, category, detail string,
) {
	logger.Warn("secrets verify：凭据校验失败",
		"category", category, "kind", string(identity.OwnerKind),
		"resource", identity.OwnerName, "slot", identity.BindingKey,
		"credential", identity.ID, "version", version.VersionNo, "detail", detail)
}

// credentialWiring 是 (b) 对一个 active 身份"应当绑定到哪儿"的判定结果。
// `EnvServed` 表示该资源当前由进程环境供值（managed 身份是模式切换后的孤儿），
// 此时不核对托管版本；`ExpectedTarget` 为空表示该类资源没有目标绑定可核。
// credentialWiring says where an active identity should be bound; EnvServed marks a
// resource currently served by the process environment.
type credentialWiring struct {
	EnvServed      bool
	EnvName        string
	ExpectedTarget string
	Reason         string
	Detail         string
}

// currentCredentialProblem 判定一条 active 身份当前是否可用；空原因码 = 可用。
// currentCredentialProblem returns (reason, detail); an empty reason means usable.
func currentCredentialProblem(ctx context.Context, in CredentialVerifyInput,
	identity core.CredentialRecord, versionsByCredential map[string][]core.CredentialVersionRecord,
	outcomes map[string]credentialVersionOutcome, lookup func(string) (string, bool),
) (string, string) {
	wiring := resolveCredentialWiring(ctx, in, identity)
	if wiring.Reason != "" {
		return wiring.Reason, wiring.Detail
	}
	if wiring.EnvServed {
		// `api_key` 不是 `${VAR}` 引用时没有可核对的进程环境（发布路径本应拒绝
		// 这种定义）；这里只回答"引用就位了吗"，不替发布期补判定。
		if wiring.EnvName == "" {
			return "", ""
		}
		if value, ok := lookup(wiring.EnvName); !ok || strings.TrimSpace(value) == "" {
			return credentialReasonEnvMissing, "占位符 " + wiring.EnvName + " 在当前进程环境里缺失或为空"
		}
		return "", ""
	}
	return managedCredentialProblem(identity, versionsByCredential, outcomes, wiring.ExpectedTarget)
}

// managedCredentialProblem 用 (a) 的判定结果回答"当前版本还能用吗"，并按当前发布
// 定义重算的目标绑定核对 `target_binding`：地址换了而密文还是按旧地址封的，必须
// 重新录入 Key（既有 `verifyIdentity` 的检查，统一实现不得丢掉）。
// managedCredentialProblem reuses the (a) verdicts and compares the recomputed target
// binding against the current version, keeping the pre-existing target check.
func managedCredentialProblem(identity core.CredentialRecord,
	versionsByCredential map[string][]core.CredentialVersionRecord,
	outcomes map[string]credentialVersionOutcome, expectedTarget string,
) (string, string) {
	var current *core.CredentialVersionRecord
	for i := range versionsByCredential[identity.ID] {
		if versionsByCredential[identity.ID][i].VersionNo == identity.CurrentVersion {
			current = &versionsByCredential[identity.ID][i]
			break
		}
	}
	if current == nil {
		return credentialReasonKeyRevoked, "当前版本行缺失"
	}
	switch outcomes[credentialVersionKey(identity.ID, current.VersionNo)] {
	case credentialVersionUsable:
	case credentialVersionMissingCiphertext:
		return credentialReasonKeyRevoked, "当前版本缺少密文"
	case credentialVersionKeyMismatch:
		return credentialReasonKeyRevoked, "当前版本的 key_id 与当前主密钥不一致"
	case credentialVersionAADMismatch:
		return credentialReasonKeyRevoked, "当前版本无法解密（AAD 字段或密文不匹配）"
	default:
		return credentialReasonKeyRevoked, "当前版本状态为 " + string(current.State)
	}
	if expectedTarget != "" && current.TargetBinding != expectedTarget {
		return credentialReasonTargetMismatch, "目标绑定与当前发布定义不一致，必须重新录入 Key"
	}
	return "", ""
}

// resolveCredentialWiring 按属主类别算出当前发布事实；未知类别按绑定缺失处理
// （核验面比写面更保守：不认识的数据一律不可用）。
// resolveCredentialWiring resolves the published facts per owner kind.
func resolveCredentialWiring(ctx context.Context, in CredentialVerifyInput,
	identity core.CredentialRecord,
) credentialWiring {
	switch identity.OwnerKind {
	case core.BindingModelConnection:
		return modelConnectionWiring(ctx, in, identity)
	case core.BindingMCPServer:
		return mcpServerWiring(ctx, in, identity)
	case core.BindingNotifyChannel:
		return notifyChannelWiring(in, identity)
	default:
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "未知的凭据属主类别"}
	}
}

// modelConnectionWiring 读模型连接当前发布定义：managed 模式算目标绑定，env 模式
// 报占位符名（值由进程环境供）。模式缺省 = env，与发布路径同一口径。
// modelConnectionWiring resolves the current published provider definition.
func modelConnectionWiring(ctx context.Context, in CredentialVerifyInput,
	identity core.CredentialRecord,
) credentialWiring {
	if in.Published == nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "核验输入缺少发布资源读取面"}
	}
	pointer, err := in.Published.PointerByUID(ctx, core.ResourceModelConnection, identity.OwnerUID)
	if err != nil || pointer.DeletedAt != nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "模型连接没有当前发布指针（已删除或不存在）"}
	}
	version, err := in.Published.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "模型连接的发布版本不可读"}
	}
	definition, err := parseProviderDefinition(pointer.ResourceID, version.DefinitionYAML)
	if err != nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "模型连接的发布定义无法解析"}
	}
	mode := core.CredentialModeEnv
	if raw := strings.TrimSpace(definition.CredentialMode); raw != "" {
		parsed, parseErr := core.ParseCredentialMode(raw)
		if parseErr != nil {
			return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "模型连接的凭据模式非法"}
		}
		mode = parsed
	}
	if mode == core.CredentialModeEnv {
		name, _ := core.EnvPlaceholderName(strings.TrimSpace(definition.APIKey))
		return credentialWiring{EnvServed: true, EnvName: name}
	}
	expectedTarget, err := CredentialTargetBinding(core.BindingModelConnection, definition.BaseURL, "")
	if err != nil {
		return credentialWiring{Reason: credentialReasonTargetMismatch, Detail: "模型连接的发布定义缺少可用的 base_url"}
	}
	return credentialWiring{ExpectedTarget: expectedTarget}
}

// mcpServerWiring 核对槽位仍属于当前发布定义，并按 `(server UID, 槽位名)` 重算目标
// 绑定：密文既不能搬到别的 server，也不能搬到同一 server 的另一个槽位。
// mcpServerWiring keeps the slot-to-server binding the write path sealed.
func mcpServerWiring(ctx context.Context, in CredentialVerifyInput,
	identity core.CredentialRecord,
) credentialWiring {
	if in.Published == nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "核验输入缺少发布资源读取面"}
	}
	pointer, err := in.Published.PointerByUID(ctx, core.ResourceMCPServer, identity.OwnerUID)
	if err != nil || pointer.DeletedAt != nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "MCP server 没有当前发布指针（已删除或不存在）"}
	}
	version, err := in.Published.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "MCP server 的发布版本不可读"}
	}
	definition, err := workspace.ParseMCPServerBlock([]byte(version.DefinitionYAML))
	if err != nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "MCP server 的发布定义无法解析"}
	}
	slots, err := core.MCPCredentialSlots(definition)
	if err != nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "MCP server 的槽位声明冲突"}
	}
	if !slices.Contains(slots, identity.BindingKey) {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "该槽位已不在当前发布定义里"}
	}
	expectedTarget, err := MCPCredentialTargetBinding(identity.OwnerUID, identity.BindingKey)
	if err != nil {
		return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "槽位名非法"}
	}
	return credentialWiring{ExpectedTarget: expectedTarget}
}

// notifyChannelWiring 核对渠道仍以 managed 模式引用这条身份：渠道不存在、模式已切回
// env、或凭据引用换了人，都说明这条身份已经不是当前生效凭据。
// notifyChannelWiring requires the channel to still reference this identity in
// managed mode.
func notifyChannelWiring(in CredentialVerifyInput, identity core.CredentialRecord) credentialWiring {
	for _, channel := range in.Channels {
		if channel.ResourceUID != identity.OwnerUID {
			continue
		}
		if channel.CredentialMode != core.CredentialModeManaged || channel.CredentialID != identity.ID {
			return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "渠道的凭据引用与当前身份不一致"}
		}
		return credentialWiring{}
	}
	return credentialWiring{Reason: credentialReasonBindingMissing, Detail: "通知渠道不存在或已删除"}
}
