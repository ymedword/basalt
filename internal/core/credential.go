package core

// 本文件声明托管凭据的领域字面量与目标绑定（颗粒度文档 R4.8 / T106）。
// 凭据模式只有 `env` 与 `managed` 两种，必须显式声明或按契约规则判定，
// 不得按值形态静默推断；状态机的合法性在此收口，存储层只执行判定结果。
// This file declares credential modes, state machines and target binding. Modes are
// explicit; value shapes never imply a mode silently.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// CredentialMode 是凭据的两值模式。
// CredentialMode is the two-value credential mode.
type CredentialMode string

const (
	// CredentialModeEnv 表示值只存精确 `${VAR}` 引用，发送 / 调用时从环境解析。
	CredentialModeEnv CredentialMode = "env"
	// CredentialModeManaged 表示值由服务端写入并以 AES-256-GCM 密文落库。
	CredentialModeManaged CredentialMode = "managed"
)

// ParseCredentialMode 逐字解析凭据模式；`auto` 等推断值一律拒绝。
// ParseCredentialMode accepts exactly the two literals; inference values such as
// `auto` are refused.
func ParseCredentialMode(raw string) (CredentialMode, error) {
	switch CredentialMode(raw) {
	case CredentialModeEnv:
		return CredentialModeEnv, nil
	case CredentialModeManaged:
		return CredentialModeManaged, nil
	default:
		return "", fmt.Errorf("%w: 非法的凭据模式 %q", ErrCredentialModeRequired, raw)
	}
}

// CredentialIdentityState 是凭据身份状态。
// CredentialIdentityState is the credential identity state.
type CredentialIdentityState string

const (
	// CredentialIdentityActive 表示身份可被新使用，也可支撑在途使用。
	CredentialIdentityActive CredentialIdentityState = "active"
	// CredentialIdentityRevoked 表示身份与全部版本不可用。重新录入新 Key 只把**身份**
	// 迁回 `active` 并追加新版本；已撤销的版本永不复活，`revocation_epoch` 只增不减。
	CredentialIdentityRevoked CredentialIdentityState = "revoked"
	// CredentialIdentityRetired 表示资源删除 / 模式切换后的退役态。
	CredentialIdentityRetired CredentialIdentityState = "retired"
)

// CredentialVersionState 是凭据版本状态。
// CredentialVersionState is the credential version state.
type CredentialVersionState string

const (
	// CredentialVersionActive 表示当前版本，可被新解析与在途使用。
	CredentialVersionActive CredentialVersionState = "active"
	// CredentialVersionSuperseded 表示被新版本替换：新解析不用，在途仍可用。
	CredentialVersionSuperseded CredentialVersionState = "superseded"
	// CredentialVersionRevoked 表示显式撤销：新解析与在途都不可用；终态。
	CredentialVersionRevoked CredentialVersionState = "revoked"
	// CredentialVersionRetired 表示资源删除 / 模式切换：新解析不用，在途按类别处理。
	CredentialVersionRetired CredentialVersionState = "retired"
)

// CanTransitionTo 判定身份状态的合法迁移。`revoked` / `retired` 可经**重新录入**
// 回到 `active`：这是一条新版本路径，不修改任何既有版本行；版本状态的终态见
// `CredentialVersionState.CanTransitionTo`。
// CanTransitionTo reports whether an identity transition is legal. Re-entry moves a
// revoked or retired identity back to active as a new-version path; no version row is
// rewritten, and version-level revocation stays terminal.
func (s CredentialIdentityState) CanTransitionTo(next CredentialIdentityState) bool {
	switch s {
	case CredentialIdentityActive:
		return next == CredentialIdentityRevoked || next == CredentialIdentityRetired
	case CredentialIdentityRetired:
		return next == CredentialIdentityActive || next == CredentialIdentityRevoked
	case CredentialIdentityRevoked:
		return next == CredentialIdentityActive
	default:
		return false
	}
}

// CanTransitionTo 判定版本状态的合法迁移；`revoked` 是终态。
// CanTransitionTo reports whether a version transition is legal; revoked is terminal.
func (s CredentialVersionState) CanTransitionTo(next CredentialVersionState) bool {
	switch s {
	case CredentialVersionActive:
		return next == CredentialVersionSuperseded || next == CredentialVersionRevoked || next == CredentialVersionRetired
	case CredentialVersionSuperseded:
		return next == CredentialVersionRevoked || next == CredentialVersionRetired
	case CredentialVersionRetired:
		return next == CredentialVersionRevoked
	default:
		return false
	}
}

// CredentialRecord 是 `credentials` 的行视图：凭据身份（一个资源实例至多一条）、
// 当前活动版本号与撤销纪元。`RevocationEpoch` 每次撤销 +1 并参与资源 ETag——
// 已固定旧纪元的 Run 据此判出"凭据在途中被撤销"。
//
// 14D 起唯一键是 `(owner_kind, owner_uid, binding_key)`：`mcp_server` 一个 server 可以有
// 多个占位符，各自独立版本与撤销，`BindingKey` 就是占位符名；其余三种 kind 固定空串，
// 键空间不混用（schema CHECK 同口径）。模型连接 / 通知渠道"一个实例至多一条"的语义
// 因此不变——它们的 `binding_key` 恒为 `”`。
// CredentialRecord is the row view of the credential identity. Since 14D the uniqueness key
// is (owner_kind, owner_uid, binding_key): one MCP server may hold one credential per
// placeholder, while every other kind keeps the empty key and therefore one identity each.
type CredentialRecord struct {
	ID              string
	OwnerKind       BindingKind
	OwnerUID        ResourceUID
	BindingKey      string // mcp_server：占位符名；其它 kind 固定空串
	OwnerName       string
	State           CredentialIdentityState
	CurrentVersion  int
	RevocationEpoch int
	KeyID           string
	CreatedAt       int64
	UpdatedAt       int64
	OperationID     string
}

// CredentialVersionRecord 是 `credential_versions` 的行视图：不可变版本，撤销后
// `Ciphertext` 为 nil（不可再解密），其余状态保留密文供在途使用。
// CredentialVersionRecord is the immutable version row; a revoke nils the ciphertext.
type CredentialVersionRecord struct {
	CredentialID  string
	VersionNo     int
	State         CredentialVersionState
	KeyID         string
	TargetBinding string
	Last4         string
	Nonce         []byte
	Ciphertext    []byte
	CreatedAt     int64
	UpdatedAt     int64
	OperationID   string
}

// RunCredentialRef 是 `run_credential_refs` 的行视图：Run 准入时固定的凭据版本。
// Run 内每次模型调用前按它检查版本状态——版本被撤销即终止该 Run，**不**回退到
// env 或最新版本；`ReleasedAt` 非空表示该 Run 已不再占用这个版本。
// RunCredentialRef is the credential version a run pinned at admission.
type RunCredentialRef struct {
	RunID             string
	CredentialID      string
	CredentialVersion int
	ResourceKind      BindingKind
	ResourceUID       ResourceUID
	ResourceName      string
	ReleasedAt        *int64
}

// RunNotifyRef 是 `run_notify_refs` 的行视图：Run 准入时冻结的渠道集合。
// 通知**不**固定凭据版本——凭据按每次发送解析当前 active 版本。
// RunNotifyRef is the channel set a run froze at admission; notify never pins a
// credential version.
type RunNotifyRef struct {
	RunID       string
	AgentUID    AgentUID
	ChannelUID  ResourceUID
	ChannelName string
	ReleasedAt  *int64
}

// NotifyCredentialResolution 是一次发送解析到的托管凭据（R4.8）：`Target` 只用于本次
// 投递，**不得**进入任何对外文案；身份与版本号写进 `notify_deliveries` 供审计追溯。
// env 模式没有凭据身份与版本，两者保持零值。
// NotifyCredentialResolution carries the per-send target plus the credential identity and
// version recorded in notify_deliveries; env mode leaves both zero.
type NotifyCredentialResolution struct {
	Target            string // 本次投递用的真实地址；只在本次发送内可见
	CredentialID      string // managed 模式的凭据身份；env 模式为空
	CredentialVersion int    // managed 模式实际使用的版本号；env 模式为 0
}

// NotifyDeliveryRecord 是 `notify_deliveries` 的行视图：一次工具调用至多一条投递记录
// （主键就是工具调用 id），记发送当时解析到的凭据版本与目标 host，供审计追溯。
// NotifyDeliveryRecord is the one-per-tool-invocation delivery record.
type NotifyDeliveryRecord struct {
	ToolInvocationID  int64
	RunID             string
	AgentUID          AgentUID
	ChannelUID        ResourceUID
	ChannelName       string
	CredentialID      string
	CredentialVersion int
	TargetHost        string
	ResolvedAt        int64
}

// CredentialRef 是响应中的只读凭据引用：`cred_` + 26 位 Crockford Base32，
// 只带尾号与状态，**不含**任何明文或可逆材料。
// CredentialRef is the read-only credential reference returned to clients.
type CredentialRef struct {
	ID           string                  `json:"credential_ref"`
	Last4        string                  `json:"last4"`
	Mode         CredentialMode          `json:"credential_mode"`
	State        CredentialIdentityState `json:"credential_state"`
	VersionNo    int                     `json:"credential_version"`
	VersionState CredentialVersionState  `json:"credential_version_state"`
}

// ValidateCredentialID 校验 `cred_` 前缀与 Crockford 载荷。
// ValidateCredentialID checks the cred_ prefix and Crockford payload.
func ValidateCredentialID(raw string) error {
	const prefix = "cred_"
	if !strings.HasPrefix(raw, prefix) {
		return fmt.Errorf("%w: 凭据 ID 前缀必须为 %q", ErrCredentialOwnerMismatch, prefix)
	}
	payload := raw[len(prefix):]
	if len(payload) != uidPayloadLength {
		return fmt.Errorf("%w: 凭据 ID 载荷长度必须为 %d", ErrCredentialOwnerMismatch, uidPayloadLength)
	}
	upper := strings.ToUpper(payload)
	for i := 0; i < len(upper); i++ {
		c := upper[i]
		switch c {
		case 'I', 'L', 'O', 'U':
			return fmt.Errorf("%w: 凭据 ID 不得包含易混字符 %q", ErrCredentialOwnerMismatch, c)
		}
		if !strings.ContainsRune(crockfordAlphabet, rune(c)) {
			return fmt.Errorf("%w: 凭据 ID 包含非法字符 %q", ErrCredentialOwnerMismatch, c)
		}
	}
	return nil
}

// TargetBinding 是密文 AAD 的目标绑定输入：模型连接取 `base_url`，
// 通知渠道取完整地址。完整地址按凭据处理，**不得**进入日志或响应。
// TargetBinding feeds the AAD target-binding hash: base_url for model connections,
// the full URL for channels. The full URL is credential material.
type TargetBinding struct {
	Kind    BindingKind
	Owner   ResourceUID
	BaseURL string
	FullURL string
}

// Canonical 返回目标地址的规范形态：去首尾空白；可解析时 scheme / host 小写并
// 去掉默认端口；路径与查询串**原样保留**（它们指向具体目标，不能被改写）。
// 不可解析时退回去空白的原串——调用方仍会因 URL 校验失败而拒绝保存。
// Canonical lowercases scheme/host and drops default ports while preserving the
// path and query verbatim; unparseable input falls back to the trimmed string.
func (t TargetBinding) Canonical() string {
	raw := t.BaseURL
	if t.Kind == BindingNotifyChannel {
		raw = t.FullURL
	}
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return raw
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = host + ":" + port
	} else {
		parsed.Host = host
	}
	return parsed.String()
}

// Hash 返回目标绑定的 sha256 小写十六进制；AAD 只携带该哈希，不携带明文地址。
// Hash returns the lowercase hex sha256 of the canonical target; the AAD carries this
// hash and never the plaintext address.
func (t TargetBinding) Hash() string {
	sum := sha256.Sum256([]byte(t.Canonical()))
	return hex.EncodeToString(sum[:])
}
