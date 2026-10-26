package core

// 本文件是实例级加密状态与实例私密动作的领域字面量（14D；契约 openapi
// `InstanceSecretsView` 与 schema v6 的 `instance_secrets`）。三态由「初始化记录 /
// 密钥文件 / 现存托管密文」三项联立判定，**不是**"文件在不在"；恢复态下禁止生成
// 替代密钥，也不回落到 env。零可执行语句、零内部包依赖。
// This file declares the instance-secret state literals. The three states are decided
// by record + key file + existing ciphertext together, never by file existence alone.

// SecretState 是实例加密服务的三态。`uninitialized` 与 `recovery_required` 的区别
// 在于前者"还没有密文"，后者"有密文但当前读不出来"——后者**必须**由人按恢复流程处置。
// SecretState is the three-state instance-encryption verdict.
type SecretState string

const (
	// SecretStateUninitialized 表示无初始化记录、无密钥文件、无托管密文：加密托管从未
	// 启用（纯 env 实例，或尚未启用托管的存量实例）。页面给一键初始化（人类动作）。
	SecretStateUninitialized SecretState = "uninitialized"
	// SecretStateReady 表示记录、密钥文件与 `key_id` 一致且完整读回通过。
	SecretStateReady SecretState = "ready"
	// SecretStateRecoveryRequired 表示密钥文件缺失 / 损坏 / `key_id` 不符 / 发布残留
	// 来源不可证明：拒绝一切以密文为前提的操作，且**不得**生成替代密钥。
	SecretStateRecoveryRequired SecretState = "recovery_required"
)

// Valid 判定状态是否是契约闭集内的取值。
// Valid accepts exactly the three contract literals.
func (s SecretState) Valid() bool {
	switch s {
	case SecretStateUninitialized, SecretStateReady, SecretStateRecoveryRequired:
		return true
	default:
		return false
	}
}

// SecretRecoveryReason 是 `recovery_required` 的细分原因。空串表示**不需要恢复**
// （`uninitialized` / `ready`），不是"原因未知"；`unavailable` 只报"无法判定"，
// 不猜具体原因。
// SecretRecoveryReason explains why recovery is required; the empty string means
// no recovery is needed.
type SecretRecoveryReason string

const (
	// SecretRecoveryNone 表示不需要恢复。
	SecretRecoveryNone SecretRecoveryReason = ""
	// SecretRecoveryMissing 表示密钥文件不存在。
	SecretRecoveryMissing SecretRecoveryReason = "missing"
	// SecretRecoveryCorrupt 表示密钥文件通不过格式 / 权限 / 链接数校验。
	SecretRecoveryCorrupt SecretRecoveryReason = "corrupt"
	// SecretRecoveryKeyIDMismatch 表示密钥文件的 key_id 与实例记录不符。
	// #nosec G101 -- 原因码字面量，不是凭据（变量名命中）。
	SecretRecoveryKeyIDMismatch SecretRecoveryReason = "key_id_mismatch"
	// SecretRecoveryUnprovableProvenance 表示存在无法用 `os.SameFile` 证明来源的密钥文件。
	// #nosec G101 -- 原因码字面量，不是凭据（变量名命中）。
	SecretRecoveryUnprovableProvenance SecretRecoveryReason = "unprovable_provenance"
	// SecretRecoveryUnavailable 表示库或文件系统读取失败：只报"无法判定"。
	SecretRecoveryUnavailable SecretRecoveryReason = "unavailable"
	// SecretRecoveryKeyPathMissing 表示实例有初始化记录（或托管密文）但当前解析不出
	// 主密钥路径：实例身份与密钥位置之间存在缺口（名册丢失、secrets 段被移除等）。
	// 它与 `missing`（路径已知、文件不在）分开报——恢复动作不同：先找回名册 / 配置，
	// 而不是去某个已知目录找文件。**不**因为"这次没解析出路径"就当成新实例重新生成。
	// The instance remembers a key that this process cannot locate; it is never a cue
	// to mint a replacement.
	// #nosec G101 -- 原因码字面量，不是凭据（变量名命中）。
	SecretRecoveryKeyPathMissing SecretRecoveryReason = "key_path_missing"
)

// Valid 判定恢复原因是否是契约闭集内的取值。
// Valid accepts the empty string and the six contract literals.
func (r SecretRecoveryReason) Valid() bool {
	switch r {
	case SecretRecoveryNone, SecretRecoveryMissing, SecretRecoveryCorrupt,
		SecretRecoveryKeyIDMismatch, SecretRecoveryUnprovableProvenance, SecretRecoveryUnavailable,
		SecretRecoveryKeyPathMissing:
		return true
	default:
		return false
	}
}

// 实例私密动作与记录字面量（契约 run-and-errors.md §9.3 与 schema v6）。
// Instance-secret action and record literals frozen by contract.
const (
	// ActionSystemSecretsInit 是主密钥初始化：唯一允许由用户启动的初始化流程自动执行的
	// 凭据类动作，且只限可证明的新实例（宪法第 14 条）。
	ActionSystemSecretsInit = "system.secrets_init"
	// TargetKindSystem 是实例级动作的目标类型；与资源类目标分开，读侧据此区分。
	TargetKindSystem = "system"
	// InstanceSecretScopeMasterKey 是 `instance_secrets.scope` 的唯一取值。
	InstanceSecretScopeMasterKey = "master_key"
	// InstanceTargetMasterKey 是 `system.secrets_init` 的 `target_id` 固定字面量。
	InstanceTargetMasterKey = "master_key"
)
