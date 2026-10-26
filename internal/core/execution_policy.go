package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 本文件是执行环境两档的领域字面量（14D；契约 openapi `ExecutionPolicyView` 与
// run-and-errors.md §7.3 的四类细分原因）。两档共享同一套基础防护，差别只在授权
// 范围；细分原因只用于解释"为什么被拒"，不改 `error_kind` 的 `sandbox_denied`
// 分类。零可执行语句、零内部包依赖。
// This file declares the execution-profile literals and the four policy-denial
// reasons frozen by contract. The profile only widens or narrows authorization;
// the denial reason refines an explanation and never replaces the error kind.

// ExecutionProfile 是执行档位。`strict` 只放行显式白名单，`standard` 额外放行未命中
// 禁止域名的公网域名与安装清单内的程序目录可执行件。
// ExecutionProfile is the execution profile; strict admits only explicit allowlists.
type ExecutionProfile string

const (
	// ProfileStandard 是标准档：公网域名与安装清单内的程序目录可用，仍拦私有段、
	// 元数据地址、非 80/443 与交互式 shell 名称。
	ProfileStandard ExecutionProfile = "standard"
	// ProfileStrict 是严格档：只放行显式白名单；存量实例升级后一律落这一档。
	ProfileStrict ExecutionProfile = "strict"
)

// Valid 判定档位是否是契约闭集内的取值；空串与大小写变体一律拒绝。
// Valid accepts exactly the two contract literals.
func (p ExecutionProfile) Valid() bool {
	return p == ProfileStandard || p == ProfileStrict
}

// PolicyDenialReason 是工具被拒的细分原因（契约 `policy_denial_reason`）。四类互斥：
// 档位拒绝、命中禁止域名、解析落在受保护地址段、本可由例外放行但缺少例外。
// PolicyDenialReason refines a policy denial; the four values are mutually exclusive.
type PolicyDenialReason string

const (
	// DenialProfileDenied 表示档位授权范围拒绝（如严格档下目标不在白名单），
	// 与具体地址无关。
	DenialProfileDenied PolicyDenialReason = "profile_denied"
	// DenialDeniedDomain 表示命中 `http.denied_domains`；两档都先查禁止域名。
	DenialDeniedDomain PolicyDenialReason = "denied_domain"
	// DenialProtectedAddress 表示解析结果落在受保护地址段（私有 / 回环 / 链路本地 /
	// 云元数据）；云元数据地址任何档位与例外都不放行。
	DenialProtectedAddress PolicyDenialReason = "protected_address"
	// DenialExceptionMissing 表示该地址本可由访问例外放行，但当前没有生效例外。
	DenialExceptionMissing PolicyDenialReason = "exception_missing"
)

// Valid 判定细分原因是否是契约闭集内的取值；空串表示"没有被拒"，同样不是合法原因。
// Valid accepts exactly the four contract literals; an empty reason means no denial.
func (r PolicyDenialReason) Valid() bool {
	switch r {
	case DenialProfileDenied, DenialDeniedDomain, DenialProtectedAddress, DenialExceptionMissing:
		return true
	default:
		return false
	}
}

// PendingConfirmationAxis 是升级迁移待确认项所在的轴（契约 `PendingPolicyConfirmation`）。
// 两轴各自独立判定，**不合并**成一个"策略问题"列表——出网条目与命令条目要分开处置。
// PendingConfirmationAxis names the axis a pending confirmation belongs to.
type PendingConfirmationAxis string

const (
	// PendingAxisNetworkAllowlist 是出网白名单条目轴：解析到受保护地址段的条目。
	PendingAxisNetworkAllowlist PendingConfirmationAxis = "network_allowlist"
	// PendingAxisShellCommand 是命令白名单条目轴：命中新增 shell 名称的条目。
	PendingAxisShellCommand PendingConfirmationAxis = "shell_command"
)

// Valid 判定轴取值是否是契约闭集内的取值。
// Valid accepts exactly the two contract literals.
func (a PendingConfirmationAxis) Valid() bool {
	switch a {
	case PendingAxisNetworkAllowlist, PendingAxisShellCommand:
		return true
	default:
		return false
	}
}

// PendingConfirmationReason 是待确认的原因：为什么这一条需要人来裁决。
// PendingConfirmationReason explains why an item needs a human decision.
type PendingConfirmationReason string

const (
	// PendingReasonProtectedAddress 表示该域名解析落在受保护地址段。
	PendingReasonProtectedAddress PendingConfirmationReason = "protected_address"
	// PendingReasonShellName 表示该命令名命中新增的交互式 shell 名称集合。
	PendingReasonShellName PendingConfirmationReason = "shell_name"
)

// Valid 判定原因取值是否是契约闭集内的取值。
// Valid accepts exactly the two contract literals.
func (r PendingConfirmationReason) Valid() bool {
	switch r {
	case PendingReasonProtectedAddress, PendingReasonShellName:
		return true
	default:
		return false
	}
}

// 受保护地址段（设计 §2.2.2 的冻结清单）：两档相同，来自代码常量而非配置。展示与
// 判定同源——页面列出这些段，判定读同一份字面量，二者不可能漂移。
// The protected ranges are code constants shared by display and judgement alike.
var protectedAddressRanges = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"169.254.0.0/16", "100.64.0.0/10", "0.0.0.0/8",
	"::1/128", "fe80::/10", "fc00::/7",
}

// 云元数据地址：**任何档位与例外都不放行**，因此与"可加例外的受保护段"分开列。
// Cloud metadata addresses can never be excepted, so they are listed apart.
var cloudMetadataAddresses = []string{"169.254.169.254", "fd00:ec2::254"}

// protectedBlocks 是启动期解析好的段（字面量写死，解析失败只可能是代码写错）。
// protectedBlocks are parsed once from the literals.
var protectedBlocks = func() []*net.IPNet {
	blocks := make([]*net.IPNet, 0, len(protectedAddressRanges))
	for _, cidr := range protectedAddressRanges {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("core: 受保护地址段字面量非法: " + cidr)
		}
		blocks = append(blocks, block)
	}
	return blocks
}()

// cloudMetadataIPs 是解析好的云元数据地址。
// cloudMetadataIPs are the parsed metadata addresses.
var cloudMetadataIPs = func() []net.IP {
	ips := make([]net.IP, 0, len(cloudMetadataAddresses))
	for _, raw := range cloudMetadataAddresses {
		ip := net.ParseIP(raw)
		if ip == nil {
			panic("core: 云元数据地址字面量非法: " + raw)
		}
		ips = append(ips, ip)
	}
	return ips
}()

// ProtectedAddresses 返回受保护地址段与云元数据地址的展示清单（顺序固定：先 IPv4
// 段、再 IPv6 段、最后元数据地址）。`GET /execution-policy` 的
// `protected_addresses` 与 tool 层的判定读同一份定义。
// ProtectedAddresses is the display list shared with the judgement.
func ProtectedAddresses() []string {
	out := make([]string, 0, len(protectedAddressRanges)+len(cloudMetadataAddresses))
	out = append(out, protectedAddressRanges...)
	return append(out, cloudMetadataAddresses...)
}

// ClassifyAddress 判定一个解析结果落在哪一档：`protected` 表示命中受保护段，
// `metadata` 表示云元数据地址（protected 恒真且**不可**经例外放行）。
// ClassifyAddress reports whether an address is protected and whether it is metadata.
func ClassifyAddress(ip net.IP) (protected, metadata bool) {
	if ip == nil {
		return false, false
	}
	for _, meta := range cloudMetadataIPs {
		if meta.Equal(ip) {
			return true, true
		}
	}
	for _, block := range protectedBlocks {
		if block.Contains(ip) {
			return true, false
		}
	}
	return false, false
}

// 交互式 shell 名称集合（设计 §2.2.5）：去扩展名、大小写折叠后比对；命中的命令名
// 一律拒。这是**能力上限声明**，不是行为识别——改名副本与硬链接不在覆盖范围内。
// The interactive shell name set is a declared capability ceiling, not behaviour detection.
var shellNames = map[string]struct{}{
	"bash": {}, "sh": {}, "zsh": {}, "dash": {}, "ksh": {}, "fish": {},
	"csh": {}, "tcsh": {}, "busybox": {}, "powershell": {}, "pwsh": {}, "cmd": {},
}

// ShellNames 返回交互式 shell 名称集合的展示清单（升序固定）。
// ShellNames is the ordered display list of the shell name set.
func ShellNames() []string {
	out := make([]string, 0, len(shellNames))
	for name := range shellNames {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// IsShellName 判定一个可执行名（可带完整路径与可执行后缀）是否命中 shell 名称集合：
// 取 basename、小写、去扩展名后逐字比对。
// IsShellName judges the basename with its executable suffix stripped.
func IsShellName(target string) bool {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(target, `\`, "/")))
	if _, ok := shellNames[base]; ok {
		return true
	}
	if ext := strings.LastIndexByte(base, '.'); ext > 0 {
		_, ok := shellNames[base[:ext]]
		return ok
	}
	return false
}

// ExecutionPolicy 是 `execution_policy` 单行表的领域形态。`OperationID` 记录产生该
// 修订的管理操作或系统流程（`system.migrate_v6` / `system.instance_init`）。
// ExecutionPolicy is the domain shape of the single execution_policy row.
type ExecutionPolicy struct {
	Profile     ExecutionProfile
	Revision    int64
	UpdatedAt   time.Time
	UpdatedBy   string
	OperationID string
}

// 系统流程写入 `execution_policy` 时使用的操作标识（不是管理台写）。
// The system-flow operation ids written into the execution policy row.
const (
	// SystemOperationInstanceInit 是新建实例首次初始化写入 `standard` 的标识。
	SystemOperationInstanceInit = "system.instance_init"
	// SystemOperationMigrateV6 是 v5 → v6 迁移写入 `strict` 的标识。
	SystemOperationMigrateV6 = "system.migrate_v6"
)

// Source 返回该行由谁写入：迁移 / 实例初始化 / 管理台。空操作标识（尚未装载）回空串。
// Source reports who wrote the row: migration, instance init, or the console.
func (p ExecutionPolicy) Source() string {
	switch {
	case p.OperationID == "":
		return ""
	case strings.HasPrefix(p.OperationID, "system.migrate"):
		return NetworkAllowlistSourceMigration
	case strings.HasPrefix(p.OperationID, "system.instance_init"):
		return NetworkAllowlistSourceInstanceInit
	default:
		return NetworkAllowlistSourceDatabase
	}
}

// ExecutionPolicyETag 给出档位行的强 ETag（材料 = 档位 + 修订号；**不**含待确认项，
// 待确认项是只读派生信息，不参与条件写）。
// ExecutionPolicyETag renders the strong ETag over profile plus revision.
func ExecutionPolicyETag(p ExecutionPolicy) string {
	sum := sha256.Sum256([]byte(string(p.Profile) + "\n" + strconv.FormatInt(p.Revision, 10)))
	return `"sha256:` + hex.EncodeToString(sum[:]) + `"`
}

// PendingPolicyConfirmation 是一条升级迁移待确认项（只读扫描结果，不落库）。
// PendingPolicyConfirmation is one read-only pending item from the upgrade scan.
type PendingPolicyConfirmation struct {
	ID                string
	Axis              PendingConfirmationAxis
	Value             string
	Reason            PendingConfirmationReason
	ResolvedProtected []string
}

// 访问例外的固定字面量（openapi `NetworkExceptionView`）。
// Network exception literals frozen by the contract.
const (
	// NetworkExceptionKindHost 是主机名例外（与出网白名单同一套通配语法）。
	NetworkExceptionKindHost = "host"
	// NetworkExceptionKindIPPort 是 `IP:端口` 字面量例外。
	NetworkExceptionKindIPPort = "ip_port"
)

// NetworkExceptionIDPrefix 是访问例外标识的前缀（契约路径参数
// `^exc_[0-9A-HJKMNP-TV-Z]{26}$`）。
// NetworkExceptionIDPrefix is the exception id prefix frozen by the contract.
const NetworkExceptionIDPrefix = "exc_"

// NetworkExceptionNoteMaxLength 是例外理由的最大字符数（契约 `NetworkExceptionCreate.note`
// 的 `maxLength: 500`；按 rune 计，不按字节）。
// The note bound is measured in characters, matching the contract's maxLength.
const NetworkExceptionNoteMaxLength = 500

// NewNetworkExceptionID 生成 `exc_` + 26 位 Crockford Base32；随机源失败只报错，
// **不**回落到可预测值（例外标识是审计目标，猜得到的标识不可接受）。
// NewNetworkExceptionID returns a fresh id; randomness failure is reported, never
// replaced by a predictable fallback.
func NewNetworkExceptionID() (string, error) { return newUID(NetworkExceptionIDPrefix) }

// NetworkException 是一条网络访问例外：`note` 必填（放行必须带理由，理由进审计）。
// NetworkException is one network access exception; the note is mandatory.
type NetworkException struct {
	ExceptionID string
	Kind        string
	Value       string
	Note        string
	Revision    int64
	CreatedAt   time.Time
	CreatedBy   string
	OperationID string
}

// NetworkExceptionETag 给出整份例外集合的强 ETag：按 `kind` + `value` 升序后逐行摘要；
// 空集合同样有稳定值（页面据此做删除的 If-Match）。
// NetworkExceptionETag digests the sorted collection; the empty set has a stable value.
func NetworkExceptionETag(list []NetworkException) string {
	sorted := make([]NetworkException, len(list))
	copy(sorted, list)
	slices.SortFunc(sorted, func(a, b NetworkException) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(a.Value, b.Value)
	})
	var builder strings.Builder
	for _, item := range sorted {
		builder.WriteString(item.Kind)
		builder.WriteString("\n")
		builder.WriteString(item.Value)
		builder.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return `"sha256:` + hex.EncodeToString(sum[:]) + `"`
}

// ValidateNetworkExceptionValue 规整并校验一条例外值：`host` 走白名单同一套域名语法；
// `ip_port` 必须是 `IP:端口`。云元数据地址**不得**登记为例外（任何档位都不放行）。
// ValidateNetworkExceptionValue normalizes the value; metadata addresses are refused.
func ValidateNetworkExceptionValue(kind, value string) (string, error) {
	switch kind {
	case NetworkExceptionKindHost:
		normalized, err := NormalizeDomainList([]string{value})
		if err != nil {
			return "", err
		}
		return normalized[0], nil
	case NetworkExceptionKindIPPort:
		host, port, err := net.SplitHostPort(strings.TrimSpace(value))
		if err != nil {
			return "", &InvalidInputError{
				Tool:   "network_exception",
				Reason: "ip_port 必须是 IP:端口 形态（IPv6 用方括号）",
			}
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return "", &InvalidInputError{Tool: "network_exception", Reason: "ip_port 的主机部分必须是 IP 字面量"}
		}
		portNum, convErr := strconv.Atoi(port)
		if convErr != nil || portNum < 1 || portNum > 65535 {
			return "", &InvalidInputError{Tool: "network_exception", Reason: "端口必须是 1-65535 的整数"}
		}
		if _, metadata := ClassifyAddress(ip); metadata {
			return "", &InvalidInputError{
				Tool:   "network_exception",
				Reason: "云元数据地址任何档位与例外都不放行，不能登记为例外",
			}
		}
		return net.JoinHostPort(ip.String(), strconv.Itoa(portNum)), nil
	default:
		return "", &InvalidInputError{Tool: "network_exception", Reason: "kind 只能是 host 或 ip_port"}
	}
}

// ExecutionPolicyStore 是 `execution_policy` 单行表的读写端口（消费方声明，storage 实现）。
// `Seed` 只在表内无行时写入；`Update` 以期望修订号做 CAS，冲突返回 ErrPolicyRevisionConflict。
// ExecutionPolicyStore is the read/write port of the single execution_policy row.
type ExecutionPolicyStore interface {
	// Load 读取当前档位行；无行时 exists=false。
	Load(ctx context.Context) (policy ExecutionPolicy, exists bool, err error)
	// Seed 在表内无行时写入种子（系统流程的 operationID）并返回落库行。
	Seed(ctx context.Context, profile ExecutionProfile, operationID string, now time.Time) (ExecutionPolicy, error)
	// Update 以 expectedRevision 做 CAS 更新；冲突返回 ErrPolicyRevisionConflict。
	Update(ctx context.Context, profile ExecutionProfile, expectedRevision int64,
		operationID, actor string, now time.Time) (ExecutionPolicy, error)
}

// NetworkExceptionStore 是访问例外的读写端口。
// NetworkExceptionStore is the read/write port of the exception rows.
type NetworkExceptionStore interface {
	List(ctx context.Context) ([]NetworkException, error)
	Create(ctx context.Context, exception NetworkException) error
	Delete(ctx context.Context, exceptionID string) error
	// Lookup 按标识取一行；未命中返回 ok=false。
	Lookup(ctx context.Context, exceptionID string) (NetworkException, bool, error)
}

// ExecutionPolicyAdmin 是 web 管理面依赖的端口（消费方声明，app 实现）。
// ExecutionPolicyAdmin is the web-facing execution-policy port.
type ExecutionPolicyAdmin interface {
	Current() ExecutionPolicy
	// ExceptionsCount 是当前**生效**的内网访问例外条数（运行期策略实例里的那份集合，
	// 与判定同源；不是另读一次库——库里的行若尚未装载，判定也还没用上它们）。
	// The count comes from the live policy snapshot, the same set the judgement reads.
	ExceptionsCount() int
	// Update 切换档位：`expectedProfile` 与库内不一致 → ErrExecutionProfileMismatch（零写入）。
	Update(ctx context.Context, profile, expectedProfile ExecutionProfile, ifMatch string,
		actor Actor) (ExecutionPolicy, string, error)
	PendingConfirmations(ctx context.Context) ([]PendingPolicyConfirmation, error)
	// Confirm 记录一条「保持拒绝」；already=true 表示此前已记录（不新增审计行）。
	Confirm(ctx context.Context, id string, actor Actor) (operationID string, already bool, err error)
}

// NetworkExceptionAdmin 是 web 管理面依赖的访问例外端口。
// NetworkExceptionAdmin is the web-facing exception port.
type NetworkExceptionAdmin interface {
	List() []NetworkException
	ETag() string
	Create(ctx context.Context, kind, value, note string, actor Actor) (NetworkException, string, error)
	Delete(ctx context.Context, exceptionID, ifMatch string, actor Actor) (string, error)
}
