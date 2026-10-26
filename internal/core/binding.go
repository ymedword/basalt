package core

// 本文件声明资源绑定的领域模型（颗粒度文档 R4.6 / T105）。权威是 AGENT.md
// frontmatter 的 `resource_bindings`：每个 `provider.name` / `notify_channels[]`
// 必须恰有一条同 kind 同名绑定，反之亦然；数据库投影可重建，不是写入口。
// This file declares the resource-binding model: AGENT.md is authoritative and every
// reference must pair with exactly one same-kind, same-name binding.

import "fmt"

// BindingKind 是绑定条目的资源类别；本轮只有模型连接与通知渠道两类。
// BindingKind is the resource category of a binding; only model connections and
// notification channels are bound in this revision.
type BindingKind string

const (
	// BindingModelConnection 对应 `providers[].name`。
	BindingModelConnection BindingKind = "model_connection"
	// BindingNotifyChannel 对应 `notify_channels[]`。
	BindingNotifyChannel BindingKind = "notify_channel"
	// BindingMCPServer 是 **MCP server 占位符凭据的属主类别**，不是可绑定资源：
	// `credentials.owner_kind` 认它（v6 起），AGENT.md 的 `resource_bindings` 不认它。
	// 因此 `ParseBindingKind` 仍然拒绝它，绑定分组也不因它扩第三类。
	// BindingMCPServer is a credential owner kind only; it is not a bindable resource
	// kind and ParseBindingKind still refuses it.
	BindingMCPServer BindingKind = "mcp_server"
)

// IsCredentialOwnerKind 报告该类别能否作为凭据属主（`credentials.owner_kind` 的三值
// 闭集，与 schema.sql 的 CHECK 逐字对齐）。
// IsCredentialOwnerKind reports whether the kind may own a credential.
func IsCredentialOwnerKind(kind BindingKind) bool {
	switch kind {
	case BindingModelConnection, BindingNotifyChannel, BindingMCPServer:
		return true
	default:
		return false
	}
}

// ParseCredentialOwnerKind 逐字解析**凭据属主**类别：比 `ParseBindingKind` 宽一档，
// 认 `mcp_server`（它能持有凭据，但不是可绑定资源）。凭据写面与审计 detail 回放用它——
// 用 `ParseBindingKind` 会把 MCP 撤销记录判成不可解码。
// ParseCredentialOwnerKind accepts the three credential owner kinds, including
// mcp_server, which is not a bindable resource kind.
func ParseCredentialOwnerKind(raw string) (BindingKind, error) {
	kind := BindingKind(raw)
	if !IsCredentialOwnerKind(kind) {
		return "", fmt.Errorf("%w: 未知的凭据属主类别 %q", ErrAgentResourceBindingInvalid, raw)
	}
	return kind, nil
}

// ParseBindingKind 逐字解析绑定类别；技能 / MCP / 未知值一律拒绝。
// ParseBindingKind accepts exactly the two bound kinds; skills, MCP and unknown
// values are refused.
func ParseBindingKind(raw string) (BindingKind, error) {
	switch BindingKind(raw) {
	case BindingModelConnection:
		return BindingModelConnection, nil
	case BindingNotifyChannel:
		return BindingNotifyChannel, nil
	default:
		return "", fmt.Errorf("%w: 未知的绑定类别 %q", ErrAgentResourceBindingInvalid, raw)
	}
}

// ResourceBinding 是一条 `{name, uid}` 绑定：name 是 AGENT.md 中的标签，
// uid 是不可变的资源身份。
// ResourceBinding is one `{name, uid}` entry: the name is the AGENT.md label and
// the uid is the immutable resource identity.
type ResourceBinding struct {
	Name string      `json:"name"`
	UID  ResourceUID `json:"uid"`
}

// BindingChange 是结构化补丁 `changes.bindings` 的一个元素（契约 agent-md §2.3）：
// 服务端按 `resource_kind` 分组后整块写入 frontmatter 的 `resource_bindings`，
// **不**按名称重新解析 UID、不按名补绑。
// BindingChange is one element of the `changes.bindings` patch; the server groups
// entries by kind and never re-resolves a uid from the name.
type BindingChange struct {
	ResourceKind BindingKind `json:"resource_kind"`
	RefName      string      `json:"ref_name"`
	ResourceUID  ResourceUID `json:"resource_uid"`
}

// ResourceMapping 是复制 Agent 时对一条**源专属**绑定给出的显式替代（契约
// `AgentCopyRequest.resource_mapping` 的逐项语义，FR-070）：源 UID → 目标 UID。
//
// 映射只换指向，**不**带走源资源的任何凭据；目标必须已存在、已发布、对副本有资格，
// 且**不得**沿用源 UID（同名重建不自动接管，复制也不自动接管）。缺失映射的源专属绑定
// 由调用方按 `ErrAgentCopyBlocked` 拒绝整次复制——不产生副本、不留操作行。
// ResourceMapping replaces one source-exclusive binding by an explicit target; it
// re-points a binding and never carries credentials across.
type ResourceMapping struct {
	Kind      BindingKind
	SourceUID ResourceUID
	TargetUID ResourceUID
}

// AgentResourceBinding 是 `agent_resource_bindings` 的行视图：AGENT.md 的**投影**，
// 不是写入口——权威永远是定义文件，整表可重建（删除重算也一样）。
// `ContentHash` 记产生该绑定的 AGENT.md 内容哈希，回答"这份投影基于哪一版定义"。
// AgentResourceBinding is the row view of the binding projection; the definition file
// stays authoritative and the table can always be rebuilt.
type AgentResourceBinding struct {
	AgentUID    AgentUID
	AgentName   string
	Kind        BindingKind
	RefName     string
	ResourceUID ResourceUID
	ContentHash string
	ProjectedAt int64
}

// ResourceBindings 是 AGENT.md 中两组绑定的领域形态。
// ResourceBindings is the domain shape of the two binding groups.
type ResourceBindings struct {
	ModelConnections []ResourceBinding
	NotifyChannels   []ResourceBinding
}

// Entries 返回指定类别的绑定条目；未知类别返回 nil。
// Entries returns the bindings of one kind, or nil for an unknown kind.
func (b ResourceBindings) Entries(kind BindingKind) []ResourceBinding {
	switch kind {
	case BindingModelConnection:
		return b.ModelConnections
	case BindingNotifyChannel:
		return b.NotifyChannels
	default:
		return nil
	}
}

// Validate 校验绑定本身的结构：名称非空、UID 合法、同一类别内名称与 UID 都不重复。
// Validate checks the bindings themselves: non-empty names, valid UIDs and no
// duplicate name or uid within one kind.
func (b ResourceBindings) Validate() error {
	if err := validateBindingGroup(BindingModelConnection, b.ModelConnections); err != nil {
		return err
	}
	return validateBindingGroup(BindingNotifyChannel, b.NotifyChannels)
}

// ValidateCorrespondence 校验绑定与定义中的引用严格一一对应：
// 每个 `providers[].name` / `notify_channels[]` 恰有一条同名绑定，且不存在孤儿绑定。
// 缺失 / 多余都按 `AGENT_RESOURCE_BINDING_INVALID` 拒绝，`不得`按名补绑。
// ValidateCorrespondence enforces an exact one-to-one match between references and
// bindings; missing and orphan entries are both invalid and never auto-filled.
func (b ResourceBindings) ValidateCorrespondence(providerNames, notifyChannelNames []string) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := matchBindingNames(BindingModelConnection, b.ModelConnections, providerNames); err != nil {
		return err
	}
	return matchBindingNames(BindingNotifyChannel, b.NotifyChannels, notifyChannelNames)
}

// validateBindingGroup 校验一组绑定的结构与唯一性。
// validateBindingGroup checks structure and uniqueness inside one group.
func validateBindingGroup(kind BindingKind, entries []ResourceBinding) error {
	names := make(map[string]struct{}, len(entries))
	uids := make(map[ResourceUID]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Name == "" {
			return fmt.Errorf("%w: %s 绑定缺少名称", ErrAgentResourceBindingInvalid, kind)
		}
		if _, err := ParseResourceUID(string(entry.UID)); err != nil {
			return fmt.Errorf("%w: %s 绑定 %q 的 UID 非法", ErrAgentResourceBindingInvalid, kind, entry.Name)
		}
		if _, ok := names[entry.Name]; ok {
			return fmt.Errorf("%w: %s 绑定重复名称 %q", ErrAgentResourceBindingInvalid, kind, entry.Name)
		}
		names[entry.Name] = struct{}{}
		if _, ok := uids[entry.UID]; ok {
			return fmt.Errorf("%w: %s 绑定重复 UID %q", ErrAgentResourceBindingInvalid, kind, entry.UID)
		}
		uids[entry.UID] = struct{}{}
	}
	return nil
}

// matchBindingNames 比较绑定名称集合与定义引用集合（双向精确相等）。
// matchBindingNames compares the binding-name set with the declared-reference set
// in both directions.
func matchBindingNames(kind BindingKind, entries []ResourceBinding, declared []string) error {
	bound := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		bound[entry.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		if _, ok := seen[name]; ok {
			return fmt.Errorf("%w: %s 引用重复名称 %q", ErrAgentResourceBindingInvalid, kind, name)
		}
		seen[name] = struct{}{}
		if _, ok := bound[name]; !ok {
			return fmt.Errorf("%w: %s 引用 %q 缺少绑定", ErrAgentResourceBindingMissing, kind, name)
		}
	}
	for _, entry := range entries {
		if _, ok := seen[entry.Name]; !ok {
			return fmt.Errorf("%w: %s 绑定 %q 没有对应引用", ErrAgentResourceBindingInvalid, kind, entry.Name)
		}
	}
	return nil
}
