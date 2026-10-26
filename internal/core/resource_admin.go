package core

// 本文件是公共资源写面的领域类型与固定字面量（颗粒度文档 §3.1.2）：
// ResourceKind / ResourceVersion / ResourcePointer 沿用 US-11 `core/resources.go` 的
// 唯一领域类型（**不得**另建同名类型），这里只追加发布请求 / 结果、引用视图、
// 测试结果、版本详情与动作 / 目标类型字面量。字面量逐字取自
// docs/contracts/run-and-errors.md §9.3 与 schema.sql 的 CHECK 子句。
// This file declares the public-resource write surface. Resource kinds, versions and
// pointers reuse the US-11 domain types; this file adds publish/reference/test shapes,
// the version-detail projection and the action / target-kind literals copied verbatim
// from the contract.

import (
	"fmt"
)

// —— 动作字面量（run-and-errors.md §9.3 逐字；本 US 产生的集合）——
// Action literals copied verbatim from run-and-errors.md §9.3, limited to what this
// US produces. `agent.generate_files` is reserved by the contract but never written
// as a management operation row: its only audit is the llm_calls record.
const (
	// ActionAgentCreate 是创建 Agent。
	ActionAgentCreate = "agent.create"
	// ActionAgentFilesUpdate 是 Agent 多文件提交。
	ActionAgentFilesUpdate = "agent.files.update"
	// ActionAgentGenerateFiles 是作者模型起草；**不写管理操作行**（契约注 3）。
	ActionAgentGenerateFiles = "agent.generate_files"
	// ActionAgentArchive 是归档 Agent。
	ActionAgentArchive = "agent.archive"
	// ActionAgentCopy 是复制 Agent。
	ActionAgentCopy = "agent.copy"
	// ActionAgentRestore 是从归档恢复 Agent。
	ActionAgentRestore = "agent.restore"

	// ActionModelConnectionCreate 是新增模型连接。
	ActionModelConnectionCreate = "model_connection.create"
	// ActionModelConnectionUpdate 是修改模型连接。
	ActionModelConnectionUpdate = "model_connection.update"
	// ActionModelConnectionDelete 是删除模型连接。
	ActionModelConnectionDelete = "model_connection.delete"
	// ActionModelConnectionTest 是测试模型连接（可能计费的模型调用，必须留痕）。
	ActionModelConnectionTest = "model_connection.test"
	// ActionModelConnectionPublish 是发布模型连接版本。
	ActionModelConnectionPublish = "model_connection.publish"

	// ActionMCPServerCreate 是新增 MCP server。
	ActionMCPServerCreate = "mcp_server.create"
	// ActionMCPServerUpdate 是修改 MCP server。
	ActionMCPServerUpdate = "mcp_server.update"
	// ActionMCPServerDelete 是删除 MCP server。
	ActionMCPServerDelete = "mcp_server.delete"
	// ActionMCPServerTest 是测试 MCP server。
	ActionMCPServerTest = "mcp_server.test"
	// ActionMCPServerPublish 是发布 MCP server 版本。
	ActionMCPServerPublish = "mcp_server.publish"
	// ActionMCPServerApply 是受控重建单个 MCP server（14D；契约 §9.1 字面量）。
	// 保存即发布，apply 是**另一件事**：它才改变运行期连接。
	ActionMCPServerApply = "mcp_server.apply"
	// ActionMCPServerCredentialRevoke 是撤销某个 MCP 占位符的托管凭据（14D）。
	// target_id 是服务器资源 UID，detail 带 binding_key；撤销即失效，不等下一次 apply。
	// #nosec G101 -- 操作契约名（动作标识），不是凭据（变量名命中）。
	ActionMCPServerCredentialRevoke = "mcp_server.credential.revoke"

	// ActionSkillCreate 是创建技能。
	ActionSkillCreate = "skill.create"
	// ActionSkillImport 是导入技能。
	ActionSkillImport = "skill.import"
	// ActionSkillUpdate 是更新技能。
	ActionSkillUpdate = "skill.update"
	// ActionSkillDelete 是删除技能。
	ActionSkillDelete = "skill.delete"
	// ActionSkillPublish 是发布技能版本。
	ActionSkillPublish = "skill.publish"

	// ActionNotifyChannelCreate 是新增通知渠道。
	ActionNotifyChannelCreate = "notify_channel.create"
	// ActionNotifyChannelUpdate 是修改通知渠道。
	ActionNotifyChannelUpdate = "notify_channel.update"
	// ActionNotifyChannelDelete 是删除通知渠道。
	ActionNotifyChannelDelete = "notify_channel.delete"
	// ActionNotifyChannelTest 是测试通知渠道连通性（会真实发出一条固定测试消息，
	// 必须留痕）。R7 范围修订新增（契约 §9.3 同步）。
	ActionNotifyChannelTest = "notify_channel.test"
)

// —— 目标类型字面量（契约 §9.3 逐字）——
// Target-kind literals copied verbatim from run-and-errors.md §9.3.
const (
	// TargetKindAgent 是 Agent 目标。
	TargetKindAgent = "agent"
	// TargetKindModelConnection 是模型连接目标。
	TargetKindModelConnection = "model_connection"
	// TargetKindMCPServer 是 MCP server 目标。
	TargetKindMCPServer = "mcp_server"
	// TargetKindSkill 是技能目标。
	TargetKindSkill = "skill"
	// TargetKindNotifyChannel 是通知渠道目标。
	TargetKindNotifyChannel = "notify_channel"
)

// ParseResourceKind 把外部字符串解析为三个冻结的资源种类之一；其余一律拒绝
// （含大小写变体与空白包裹）——资源种类是 schema CHECK 的固定字面量，不做模糊匹配。
// ParseResourceKind accepts exactly the three frozen resource kinds and rejects every
// other spelling, including case variants and padded whitespace.
func ParseResourceKind(s string) (ResourceKind, error) {
	switch kind := ResourceKind(s); kind {
	case ResourceModelConnection, ResourceMCPServer, ResourceSkill:
		return kind, nil
	}
	return "", fmt.Errorf("未知资源类型：%q（只接受 model_connection / mcp_server / skill）", s)
}

// ResourcePublishRequest 是一次发布请求：目标资源、可选版本与期望 ETag。
// VersionID 为 nil 表示"发布最新"（已发布则幂等）；ExpectedETag 是当前发布版本的
// 强 ETag，由 web 层在写操作行之前校验。
// ResourcePublishRequest carries the resource, an optional version (nil publishes the
// latest) and the expected strong ETag.
type ResourcePublishRequest struct {
	Kind         ResourceKind
	ID           string
	VersionID    *int64 // nil = 发布最新
	ExpectedETag string
}

// ResourcePublishResult 是发布结果：新发布的版本与 MCP 是否待重启。
// ResourcePublishResult carries the published version and whether an MCP restart is
// pending (published version != applied version).
type ResourcePublishResult struct {
	Version        ResourceVersion
	PendingRestart bool
}

// ResourceReferences 是资源的引用联合视图：资源身份、定义名引用、绑定投影引用与
// 未释放的运行引用。不得用"当前没有运行"推断无引用——ActiveRuns 只来自
// run_resource_refs 的未释放行。
// ResourceReferences joins the resource identity, definition-name references, the
// binding projection and unreleased run references.
type ResourceReferences struct {
	Kind ResourceKind
	ID   string
	// ResourceUID 是当前指针指向的稳定身份（同名重建后与旧引用的区分依据）。
	ResourceUID        ResourceUID
	PublishedVersionID int64
	// Agents 是定义名引用（活动注册表 + 归档目录扫描；解析失败按可能引用点名）。
	Agents []string
	// BoundAgents 是 `agent_resource_bindings` 投影里指向 ResourceUID 的 Agent——
	// 绑定权威的精确引用（含归档 Agent），与 Agents 取并集才是完整证据。
	BoundAgents []ResourceBoundAgent
	ActiveRuns  []string
}

// ResourceBoundAgent 是绑定投影里的一条反向引用。Archived 由归档索引给出：
// 归档 Agent 也参与投影重建，仍可能是资源的合法引用者。
// ResourceBoundAgent is one reverse reference from the binding projection; archived
// agents take part in the rebuild and still count as referrers.
type ResourceBoundAgent struct {
	Agent    string
	AgentUID AgentUID
	Archived bool
}

// ResourceTestResult 是模型连接 / MCP 连接测试的脱敏结果。ErrorCode 只放固定短语，
// ErrorMessage 不得含凭证与 URL 原文。
// US-14 起 MCP 测试追加四字段（契约 D-03）：OperationID 指向预写的那条管理操作行，
// TestedVersionID 是实际被测版本（0 = 无法确定），Stage / FailureClass 是结构化进度与
// 分类——成功时 FailureClass 为空串，空工具清单仍是成功（tools_list 阶段走完）。
// 模型连接测试不使用后三个字段，保持零值。
// ResourceTestResult is the redacted result of a connection test. Since US-14 the MCP
// test also reports its operation id, tested version, stage and structured failure
// class; model-connection tests leave those zero, and an empty tool list is success.
type ResourceTestResult struct {
	OK           bool
	LatencyMS    int64
	Model        string
	Tools        []string
	ErrorCode    string
	ErrorMessage string
	// —— US-14 追加（契约 D-03）；空串 / 0 是合法值，零值语义与既有字段一致 ——
	// Added by US-14 (contract D-03); zero values are meaningful, not "absent".
	OperationID     string
	TestedVersionID int64
	Stage           string
	FailureClass    string
}

// ResourceVersionDetail 是 GET /resource-versions/{version_id} 的服务层返回：
// 版本行 + 脱敏后的 definition_yaml + 解析后的清单 + 指针状态。
// DefinitionYAML 只对模型连接 / MCP 有值，技能恒为空；脱敏 fail-closed。
// ResourceVersionDetail is the version-detail projection; skill definitions keep an
// empty DefinitionYAML, and redaction fails closed.
type ResourceVersionDetail struct {
	Version               ResourceVersion
	DefinitionYAML        string // 模型连接 / MCP 为块原文；技能恒为空
	Manifest              []ResourceManifestFile
	Published             bool
	Applied               bool
	RevertedFromVersionID *int64
}

// ResourceManifestFile 是技能版本清单中的一个文件（路径 / 哈希 / 字节数）。
// ResourceManifestFile is one file in a skill version manifest.
type ResourceManifestFile struct {
	Path   string
	SHA256 string
	Size   int64
}

// SkillValidateResult 是 POST /skills/{name}/validate 的只读返回：是否合法与问题清单。
// Problems 为空当且仅当 Valid 为 true。
// SkillValidateResult is the read-only result of skill validation.
type SkillValidateResult struct {
	Valid    bool
	Problems []string
}

// FormatStrongETag 把发布版本的 content_hash 渲染为强 ETag：
// `"sha256:<64 位小写十六进制>"`（含双引号；契约 §9.5）。哈希按存储值原样拼接，
// **不**重新序列化定义、**不**重新求哈希——读接口与写接口因此得到同一个值。
// FormatStrongETag renders a stored content hash as a strong ETag. The hash is used
// verbatim: nothing is re-serialized or re-hashed, so every entry point agrees.
func FormatStrongETag(contentHash string) string {
	return `"sha256:` + contentHash + `"`
}
