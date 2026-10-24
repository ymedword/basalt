package core

// 本文件声明 Agent 与公共资源的稳定身份类型（颗粒度文档 R4.5 / T103）：
// `ag_` / `rs_` 前缀 + 26 位 Crockford Base32（ULID 形态），大小写不敏感解码，
// 生成使用 crypto/rand。名称只是标签，身份不可变；同名删除后重建必然是新身份。
// This file declares the stable identity types for agents and shared resources:
// an `ag_`/`rs_` prefix plus a 26-character Crockford Base32 payload, decoded
// case-insensitively and generated from crypto/rand. Names are labels only.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

const (
	// AgentUIDPrefix 是 Agent 稳定身份的前缀。
	AgentUIDPrefix = "ag_"
	// ResourceUIDPrefix 是公共资源稳定身份的前缀。
	ResourceUIDPrefix = "rs_"
	// uidPayloadLength 是 Crockford Base32 载荷长度（26 字符 = 130 位）。
	uidPayloadLength = 26
	// crockfordAlphabet 是 Crockford Base32 字符集：排除 I / L / O / U 四个易混字符。
	crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// AgentUID 是 Agent 的稳定身份（生命周期状态文件 schema v2 的权威字段）。
// AgentUID is an agent's stable identity, authoritative in the lifecycle state file.
type AgentUID string

// ResourceUID 是模型连接 / 通知渠道等公共资源的稳定身份。
// ResourceUID is the stable identity of a shared resource.
type ResourceUID string

// IdentityState 是 `resource_identities.state` 的两值字面量（schema CHECK 逐字）。
// 删除只把身份置为 deleted 并写 deleted_at，行保留供历史检索——身份从不物理消失。
// IdentityState is the two-value literal set of an identity row; a deletion only flips
// the state and stamps deleted_at, because identities never disappear physically.
type IdentityState string

const (
	// IdentityActive 表示该资源实例当前活着（名称可能已指向同一实例）。
	IdentityActive IdentityState = "active"
	// IdentityDeleted 表示该资源实例已删除；UID 永不复用。
	IdentityDeleted IdentityState = "deleted"
)

// ResourceIdentity 是 `resource_identities` 的行视图：身份一次写入、此后不可变，
// 只有 state / deleted_at 随删除动作翻转。
// ResourceIdentity is the row view of resource_identities; everything but state and
// deleted_at is written once and never rewritten.
type ResourceIdentity struct {
	UID         ResourceUID
	Kind        ResourceKind
	Name        string
	State       IdentityState
	CreatedAt   int64
	DeletedAt   *int64
	OperationID string
}

// ParseIdentityKind 接受身份表允许的**四**个种类：版本表的三值之外还有
// `notify_channel`（通知渠道没有版本行，但有身份与删除历史）。分开解析是为了让
// 版本写入面继续只认三值——把通知渠道当作可版本化资源是另一类错误。
// ParseIdentityKind accepts the four kinds of the identity table; the version table
// still accepts only three, so the two parsers stay separate.
func ParseIdentityKind(raw string) (ResourceKind, error) {
	switch kind := ResourceKind(raw); kind {
	case ResourceModelConnection, ResourceMCPServer, ResourceSkill, ResourceNotifyChannel:
		return kind, nil
	default:
		return "", fmt.Errorf("非法的资源身份种类 %q", raw)
	}
}

// ErrInvalidAgentUID 表示 Agent 身份格式不合法。
// ErrInvalidAgentUID reports a malformed agent UID.
var ErrInvalidAgentUID = errors.New("invalid agent uid")

// ErrInvalidResourceUID 表示资源身份格式不合法。
// ErrInvalidResourceUID reports a malformed resource UID.
var ErrInvalidResourceUID = errors.New("invalid resource uid")

// NewAgentUID 生成一个新的 Agent 身份；随机源失败时返回错误，不回落到可预测值。
// NewAgentUID returns a fresh agent UID; randomness failure is reported, never
// replaced by a predictable fallback.
func NewAgentUID() (AgentUID, error) {
	uid, err := newUID(AgentUIDPrefix)
	if err != nil {
		return "", err
	}
	return AgentUID(uid), nil
}

// NewResourceUID 生成一个新的资源身份。
// NewResourceUID returns a fresh resource UID.
func NewResourceUID() (ResourceUID, error) {
	uid, err := newUID(ResourceUIDPrefix)
	if err != nil {
		return "", err
	}
	return ResourceUID(uid), nil
}

// ParseAgentUID 校验并规范化一个 Agent 身份：接受小写输入，输出大写载荷的同值。
// ParseAgentUID validates and canonicalizes an agent UID; lowercase input is accepted
// and the returned value carries the canonical uppercase payload.
func ParseAgentUID(raw string) (AgentUID, error) {
	payload, err := splitUID(raw, AgentUIDPrefix)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidAgentUID, err)
	}
	return AgentUID(AgentUIDPrefix + payload), nil
}

// ParseResourceUID 校验并规范化一个资源身份。
// ParseResourceUID validates and canonicalizes a resource UID.
func ParseResourceUID(raw string) (ResourceUID, error) {
	payload, err := splitUID(raw, ResourceUIDPrefix)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidResourceUID, err)
	}
	return ResourceUID(ResourceUIDPrefix + payload), nil
}

// String 返回身份的字符串形态。
// String returns the textual form.
func (u AgentUID) String() string { return string(u) }

// String 返回身份的字符串形态。
// String returns the textual form.
func (u ResourceUID) String() string { return string(u) }

// Valid 报告身份是否满足前缀、长度与字符集约束。
// Valid reports whether the UID satisfies the prefix, length and alphabet rules.
func (u AgentUID) Valid() bool {
	_, err := ParseAgentUID(string(u))
	return err == nil
}

// Valid 报告身份是否满足前缀、长度与字符集约束。
// Valid reports whether the UID satisfies the prefix, length and alphabet rules.
func (u ResourceUID) Valid() bool {
	_, err := ParseResourceUID(string(u))
	return err == nil
}

// newUID 生成前缀 + 26 位随机 Crockford Base32；256 % 32 == 0，逐字节取模无偏。
// newUID emits the prefix plus 26 random Crockford Base32 characters. Since 32 divides
// 256, the byte-wise modulo is unbiased.
func newUID(prefix string) (string, error) {
	var buf [uidPayloadLength]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成身份随机源失败: %w", err)
	}
	out := make([]byte, 0, len(prefix)+uidPayloadLength)
	out = append(out, prefix...)
	for _, b := range buf {
		out = append(out, crockfordAlphabet[int(b)%len(crockfordAlphabet)])
	}
	return string(out), nil
}

// splitUID 拆分前缀与载荷，并按 Crockford 规则校验：I / L / O / U 一律拒绝
// （**不做** 1 / 0 的可逆映射——身份必须逐字可复现，不能被"猜一个同音字"通过）。
// splitUID validates the prefix and payload. I/L/O/U are refused outright: identities
// must round-trip byte for byte rather than accept a confusable spelling.
func splitUID(raw, prefix string) (string, error) {
	if !strings.HasPrefix(raw, prefix) {
		return "", fmt.Errorf("前缀必须为 %q", prefix)
	}
	payload := raw[len(prefix):]
	if len(payload) != uidPayloadLength {
		return "", fmt.Errorf("载荷长度必须为 %d，实际 %d", uidPayloadLength, len(payload))
	}
	upper := strings.ToUpper(payload)
	for i := 0; i < len(upper); i++ {
		c := upper[i]
		switch c {
		case 'I', 'L', 'O', 'U':
			return "", fmt.Errorf("载荷不得包含易混字符 %q", c)
		}
		if !strings.ContainsRune(crockfordAlphabet, rune(c)) {
			return "", fmt.Errorf("载荷包含非法字符 %q", c)
		}
	}
	return upper, nil
}
