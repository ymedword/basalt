package core

// 本文件是网络域名白名单的领域类型与固定字面量（US-12 R8 范围修订）：白名单的**唯一
// 权威源**是 SQLite 的 `network_domain_policy` 单行表；`basalt.yaml` 的同名键只在
// 首次启动（表内无行）时作为种子。运行期由管理台条件写修改并原子热替换，不经重启。
// This file declares the network allowlist domain types. The SQLite row is the single
// source of truth; the YAML key seeds it once, and admin writes swap the live rules
// atomically without a restart.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 域名列表的边界（R8.2.2）：条目上限与单项长度上限。超出即拒绝，**不**截断。
// List bounds; an over-long list is refused, never truncated.
const (
	MaxDomainEntries = 200
	MaxDomainLength  = 253
)

// NormalizeDomainList 校验并规整一份出网域名列表：去首尾空白、转小写、去重、升序。
// 每项只接受主机名（字母 / 数字 / `-` / `.`）或 `*.` 前缀；scheme、端口、路径、
// userinfo、空白、IP 字面量与空串一律拒绝。失败返回 `*InvalidInputError` 并点名
// 第一处非法项——页面据此保留输入并提示。
// NormalizeDomainList validates and canonicalizes the outbound allowlist, naming the
// first offending entry on failure.
func NormalizeDomainList(domains []string) ([]string, error) {
	if len(domains) > MaxDomainEntries {
		return nil, invalidDomain(fmt.Sprintf("最多 %d 项，实际 %d 项", MaxDomainEntries, len(domains)))
	}
	seen := make(map[string]struct{}, len(domains))
	out := make([]string, 0, len(domains))
	for i, raw := range domains {
		entry := strings.ToLower(strings.TrimSpace(raw))
		if entry == "" {
			return nil, invalidDomain(fmt.Sprintf("第 %d 项为空", i+1))
		}
		if len(entry) > MaxDomainLength {
			return nil, invalidDomain(fmt.Sprintf("第 %d 项超过 %d 字符", i+1, MaxDomainLength))
		}
		if !validDomainEntry(entry) {
			return nil, invalidDomain(fmt.Sprintf(
				"第 %d 项 %q 不是合法域名（只接受主机名或 *. 前缀，不含协议、端口、路径与 IP 字面量）",
				i+1, raw))
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	slices.Sort(out)
	return out, nil
}

// invalidDomain 组装域名校验失败：工具名固定，原因给用户看（中文）。
// invalidDomain builds the user-facing validation error.
func invalidDomain(reason string) error {
	return &InvalidInputError{Tool: "network_allowlist", Reason: reason}
}

// validDomainEntry 判定一条**已小写**条目：可选的 `*.` 前缀加一个合法主机名。
// 字符白名单同时挡掉 scheme（`:` / `/`）、userinfo（`@`）、路径与空白。
// validDomainEntry judges one lower-cased entry: an optional `*.` plus a hostname.
func validDomainEntry(entry string) bool {
	host := entry
	if suffix, ok := strings.CutPrefix(entry, "*."); ok {
		host = suffix
	}
	if host == "" || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// NetworkDomainETag 给出策略行的强 ETag（`"sha256:<64 位小写十六进制>"`，含双引号）。
// 材料 = 修订号 + 换行 + 规整域名按行连接；**不**重新序列化对象再求值。
// NetworkDomainETag renders the strong ETag over revision plus line-joined domains.
func NetworkDomainETag(policy NetworkDomainPolicy) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(policy.Revision, 10) + "\n" + strings.Join(policy.Domains, "\n")))
	return `"sha256:` + hex.EncodeToString(sum[:]) + `"`
}

// PolicyScopes 是策略作用域的固定字面量：当前只有出网域名白名单一项。作用域是表主键
// 的一部分，将来增加别的策略（如命令白名单）时不需要改表形态。
// Policy scopes are fixed literals; only the outbound domain allowlist exists today.
const (
	// PolicyScopeHTTPDomains 是出网域名白名单的作用域（与配置键同名）。
	PolicyScopeHTTPDomains = "http.allowed_domains"
)

// 动作与目标类型字面量（run-and-errors.md §9.3 追加，R8 范围修订）。
// Action and target-kind literals appended to the closed contract set.
const (
	// ActionNetworkAllowlistUpdate 是修改网络域名白名单。
	ActionNetworkAllowlistUpdate = "network_allowlist.update"
	// TargetKindNetworkAllowlist 是网络域名白名单目标。
	TargetKindNetworkAllowlist = "network_allowlist"
)

// NetworkDomainPolicy 是一行权威策略：规整后的域名列表、单调递增的修订号与最近一次
// 写入的脱敏操作者。`OperationID` 为空表示该行来自首次种子（不是管理写）。
// Revision is the CAS fence used by conditional writes; an empty OperationID marks the
// first-run seed rather than an admin write.
type NetworkDomainPolicy struct {
	Scope       string    // 固定 PolicyScopeHTTPDomains
	Domains     []string  // 规整形式：小写、去重、升序
	Revision    int64     // 从 1 起，每次成功写入 +1
	UpdatedAt   time.Time // 最近一次写入时间（种子 = 首次启动时间）
	UpdatedBy   string    // 脱敏操作者（token:<指纹> / local_trust）；种子为空
	OperationID string    // 产生该修订的管理操作；种子为空
}

// Seeded 报告该行是否来自首次种子（而非管理写）。
// Seeded reports whether this revision came from the config seed.
func (p NetworkDomainPolicy) Seeded() bool { return p.OperationID == "" }

// 生效来源的固定字面量（`GET /sandbox` 的 `network_allowlist.source`）：页面据此
// 区分「当前值来自首次种子」与「来自管理台写入」，**不**用文案猜测。
// Source literals exposed on the sandbox view: config_seed vs database.
const (
	NetworkAllowlistSourceConfigSeed = "config_seed"
	NetworkAllowlistSourceDatabase   = "database"
)

// Source 返回当前生效列表的来源：种子行（含尚未装载的零值）记 `config_seed`，
// 管理写产生的行记 `database`。
// Source reports whether the live list came from the seed or an admin write.
func (p NetworkDomainPolicy) Source() string {
	if p.Seeded() {
		return NetworkAllowlistSourceConfigSeed
	}
	return NetworkAllowlistSourceDatabase
}

// NetworkAllowlistAdmin 是 web 管理面依赖的端口（消费方声明，app 实现）：读当前
// **生效**值、执行一次条件写。`Update` 返回落库后的策略与操作标识——预写之后的失败
// 也带回操作标识，页面据此展示「失败 + 可查证的操作号」而不是成功。
// NetworkAllowlistAdmin is the web-facing port: read the live value, perform one
// conditional write, and return the operation id even for post-pre-write failures.
type NetworkAllowlistAdmin interface {
	Current() NetworkDomainPolicy
	Update(ctx context.Context, domains []string, ifMatch string, actor Actor) (NetworkDomainPolicy, string, error)
}

// NetworkDomainPolicyStore 是权威策略行的读写端口（消费方声明，storage 实现）。
// `Seed` 只在表内无行时插入；`Update` 以期望修订号做 CAS，冲突时返回
// `ErrPolicyRevisionConflict` 且零写入。
// The store seeds once and updates under a revision CAS.
type NetworkDomainPolicyStore interface {
	// Load 读取当前行；无行时 exists=false。
	Load(ctx context.Context) (policy NetworkDomainPolicy, exists bool, err error)
	// Seed 在无行时写入种子并返回落库行；已有行时原样返回既有行（幂等）。
	Seed(ctx context.Context, domains []string, now time.Time) (NetworkDomainPolicy, error)
	// Update 以 expectedRevision 做 CAS 更新；冲突返回 ErrPolicyRevisionConflict。
	Update(ctx context.Context, domains []string, expectedRevision int64,
		operationID string, actor string, now time.Time) (NetworkDomainPolicy, error)
}
