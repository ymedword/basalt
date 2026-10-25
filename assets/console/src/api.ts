// API 客户端：认证头注入、统一信封解包、错误码映射与 401 统一处理（约束 24 / 41）。
// 凭据只随请求头发送，**不得**进入查询串或日志；401（以及 local_trust 下的 403）
// 会清除内存凭据并交给调用方跳登录，返回路径经 router.safeNextPath 清洗。
// The API client injects credentials, unwraps the envelope and maps failures. A 401
// (or a 403 while in local_trust mode) clears the in-memory credentials and hands
// the caller the current location for a sanitized login redirect.
import type { ConsoleAuth } from './auth'

const API_BASE = '/api/v1'

// ---------------------------------------------------------------------------
// 契约视图类型（docs/contracts/openapi.yaml 的 components.schemas 摘要）
// Contract view types; field names stay snake_case on purpose.
// ---------------------------------------------------------------------------

export interface ProviderInfo {
  name: string
  models: string[]
  configured: boolean
}

export interface InfoView {
  version: string
  uptime_seconds: number
  agents: number
  tools: number
  mcp_servers: number
  providers: ProviderInfo[]
}

export type AgentState = 'active' | 'disabled' | 'archived'

/** 使用范围：共享（所有 Agent 可用）或专属（仅属主）。 */
export type ResourceScope = 'shared' | 'exclusive'

/** 凭据模式：env 只存 `${ENV_VAR}` 引用；managed 存托管密文（只回尾号与状态）。 */
export type CredentialMode = 'env' | 'managed'

/** 凭据状态；空串 = env 模式或尚未写入（契约 credential_state）。 */
export type CredentialState = '' | 'active' | 'revoked' | 'retired'

/** 通知意图三态；`none` 不得带渠道，`all_eligible` 冻结全部合格渠道。 */
export type NotifyMode = 'none' | 'all_eligible' | 'allowlist'

/** 通知模式的取数来源：显式声明，或按旧文件兼容推断。 */
export type NotifyModeSource = 'explicit' | 'legacy_inferred'

/** 生命周期身份来源；空串 = 没有有效状态文件。 */
export type LifecycleUIDSource = '' | 'state_file' | 'archive' | 'legacy_unrecorded'

/** 绑定解析状态（契约 AgentConfigReference.binding_status）。 */
export type BindingStatus = 'bound' | 'missing' | 'stale_uid' | 'orphan' | 'duplicate'

export interface AgentLifecycleView {
  agent: string
  /** 状态文件 schema v2 的稳定身份；缺失或损坏时为空串。 */
  agent_uid: string
  /** 身份来源；空串 = 无有效状态文件。 */
  uid_source: LifecycleUIDSource
  /** 状态文件缺失或损坏时为 null，配合 valid=false 与 lifecycle_error。 */
  state: AgentState | null
  generation: number
  updated_at: string
  operation_id: string
  valid: boolean
  lifecycle_error: string
}

export interface ProfileView {
  name: string
  /**
   * 展示名（AGENT.md 的 display_name）：只影响列表、详情标题与面包屑；
   * 技术名（`name`）同时可见，目录 / API / 运行身份都不改。空串表示未声明。
   */
  display_name: string
  description: string
  provider: string
  model: string
  tools: string[]
  skills: string[]
  /** 通知渠道白名单；空 = 不限制（AGENT.md 的 notify_channels）。 */
  notify_channels: string[]
  schedules: number
  lifecycle: AgentLifecycleView
  registered: boolean
  runnable: boolean
  problems: string[]
}

export interface AgentContent {
  content: string
  etag: string
}

export interface AgentContentChanges {
  description?: string
  /**
   * 显示名三态：缺省 = 不改；空串 = 移除该行（不存在时为空操作）；非空 = 写入。
   * 它只影响展示，不参与提示词，也不改 `name` / 目录 / UID。
   */
  display_name?: string
  identity?: { agent_name?: string; prompt?: string }
  /** 正文：缺省 = 不改；字符串 = 整段替换；空串 = 清空（页面需二次确认）。 */
  body?: string
  /**
   * provider.name 必须已在 resource_bindings 中绑定且 UID 是当前指针。
   * temperature 三态：缺省 = 不修改；数字（含 0）= 写入；null = 移除并回到供应商缺省。
   */
  provider?: { name?: string; model?: string; temperature?: number | null }
  tools?: string[]
  skills?: string[]
  mcp_servers?: string[]
  bootstrap?: string[]
  /** 通知意图三态；none / all_eligible 时 notify_channels 必须为空。 */
  notify_mode?: NotifyMode
  /** nil = 不改；[] = 清空声明（不表示「禁用通知」，那由 notify_mode 表达）。 */
  notify_channels?: string[]
  /** 表单显式选择的绑定：服务端在同一写边界合并，不按名重解析 UID。 */
  bindings?: AgentContentBinding[]
  settings?: { max_iterations?: number; max_history_turns?: number }
  schedules?: Record<string, unknown>[]
}

/** 结构化补丁中的一项绑定（契约 AgentContentChanges.bindings 项）。 */
export interface AgentContentBinding {
  resource_kind: 'model_connection' | 'notify_channel'
  ref_name: string
  resource_uid: string
}

/** 整份替换或结构化补丁（二者互斥，契约 AgentContentUpdate）。 */
export type AgentContentUpdate = { content: string } | { changes: AgentContentChanges }

export interface AgentFileEntry {
  path: string
  size: number
}

/**
 * 系统在一次保存中自动应用的受控工具增删（R7 / 宪法约束 11）：`notify` 是唯一
 * 可能的名字，两个数组恒为数组（非 null）。
 * The system-applied tool delta of one save; notify is the only possible name.
 */
export interface AgentToolChangeView {
  tools_added: string[]
  tools_removed: string[]
}

export interface AgentFilesView {
  agent: string
  lifecycle: AgentLifecycleView
  etag: string
  files: AgentFileEntry[]
  /** 本次保存中被系统自动加入 / 移除的工具；空数组 = 本次没有自动调整。 */
  changes: AgentToolChangeView
}

export interface AgentLifecycleTransition {
  expected_state: 'active' | 'disabled'
  expected_generation: number
}

// --- 结构化 Agent 配置（GET /agents/{name}/config，契约 AgentConfigView）-------

/** 一条 `resource_bindings` 声明：名称是标签，UID 才是身份。 */
export interface AgentConfigBinding {
  name: string
  uid: string
}

/** `resource_bindings` 的两组（与 frontmatter 同形）。 */
export interface AgentConfigBindings {
  model_connections: AgentConfigBinding[]
  notify_channels: AgentConfigBinding[]
}

export interface AgentConfigSchedule {
  id: string
  cron: string
  zone: string
  message: string
  /** 系统代投渠道的资源 UID；空串 = 不代投（R7）。 */
  deliver_to: string
}

/** `fields` 的结构化字段值：键名与 AGENT.md frontmatter 逐字对应。 */
export interface AgentConfigFields {
  name: string
  description: string
  /** 定义文件里的原文值（可为空串）；列表与标题的回退在展示处做。 */
  display_name: string
  identity: { agent_name: string; prompt: string }
  /** `temperature` 为 null = 未声明（与显式 0 不同）。 */
  provider: { name: string; model: string; temperature: number | null }
  tools: string[]
  skills: string[]
  mcp_servers: string[]
  bootstrap: string[]
  notify_mode: NotifyMode
  notify_channels: string[]
  resource_bindings: AgentConfigBindings
  settings: { max_iterations: number; max_history_turns: number }
  schedules: AgentConfigSchedule[]
}

/**
 * 一条引用（或孤儿绑定）的解析状态。`resource_uid` 为空串 = 未绑定；
 * `resource_name` 是该 UID 的**当前**名称——同名重建后与 `ref_name` 不同。
 */
export interface AgentConfigReference {
  resource_kind: 'model_connection' | 'notify_channel'
  ref_name: string
  resource_uid: string
  resource_name: string
  binding_status: BindingStatus
  published: boolean
  /** shared 或属于当前 Agent；false 时保存与准入会被拒绝。 */
  eligible: boolean
  /** 绑定 UID 已删除（墓碑存在）；不自动改绑同名新资源。 */
  deleted: boolean
  credential_state: CredentialState
}

/** 表单视图；与原文共用同一个 `content_etag`（不引入 config_etag）。 */
export interface AgentConfigView {
  agent: string
  agent_uid: string
  /** 与 GET /agents/{name}/content 完全一致；表单与原文保存共用。 */
  content_etag: string
  fields: AgentConfigFields
  notify_mode: NotifyMode
  notify_mode_source: NotifyModeSource
  references: AgentConfigReference[]
  /** 所有引用都恰有一条有效绑定；false 时保存与准入会被拒绝并点名。 */
  binding_complete: boolean
}

export interface SessionSummary {
  session_id: string
  profile_name: string
  channel: string
  user_id: string
  status: 'active' | 'archived'
  message_count: number
  created_at: string
  last_active_at: string
}

export interface ToolCallView {
  id: string
  name: string
  arguments: string
}

export interface MessageView {
  role: 'system' | 'user' | 'assistant' | 'tool'
  content: string
  tool_calls?: ToolCallView[]
  tool_call_id?: string
  created_at?: string
}

export interface SessionDetail extends SessionSummary {
  messages: MessageView[]
}

export type RunStatus = 'running' | 'completed' | 'limit_reached' | 'cancelled' | 'failed'
export type RunTrigger = 'user' | 'cron' | 'manual' | 'api'

export interface RunView {
  run_id: string
  session_id: string
  agent: string
  channel: string
  trigger: RunTrigger
  status: RunStatus
  reason: string
  final_text: string
  iterations: number
  tool_calls: number
  config_hash: string
  manifest_hash: string
  started_at: string
  finished_at: string
}

export interface ResourceVersionRef {
  kind: 'model_connection' | 'mcp_server'
  name: string
  version_id: number
  version_no: number
  content_hash: string
}

export interface SkillVersionRef {
  name: string
  version_id: number
  version_no: number
  content_hash: string
  skill_md_hash: string
  l1_hash: string
  l2_hash: string
  l3_hash: string
}

export interface FileHash {
  path: string
  sha256: string
  size: number
}

export interface RunResourceManifestView {
  format_version: number
  agent_lifecycle_state: AgentState
  /** US-11 之前的 Run 无清单，不回填。 */
  legacy_unrecorded: boolean
  providers: ResourceVersionRef[]
  mcp_servers: ResourceVersionRef[]
  skills: SkillVersionRef[]
  files: FileHash[]
}

export interface RunDetail extends RunView {
  resource_manifest: RunResourceManifestView | null
}

/** 取消结果：message 说明「已是终态、无需取消」等提示（契约 /runs/{id}/cancel）。 */
export interface RunCancelResult {
  run: RunView
  message: string
}

export interface MemoryView {
  scope: 'global' | 'agent'
  agent: string
  backend: 'markdown' | 'sqlite'
  core: string
  archive: string
}

export interface NotifyChannelView {
  name: string
  /** 不可变资源身份；同名重建会生成新 UID。 */
  resource_uid: string
  type: string
  description: string
  /** scheme + host；`${VAR}` 引用与不可解析地址为空串。 */
  url_host: string
  /** 存储里是否有非空地址引用；与 url_host 不同义。 */
  url_configured: boolean
  /**
   * 地址可用性四值：managed_configured / env / empty / managed_unavailable
   * （主密钥不可用或已撤销）。空 host **不等于**未配置。
   */
  address_status: 'managed_configured' | 'env' | 'empty' | 'managed_unavailable'
  credential_mode: CredentialMode
  /** 只读凭据引用；仅 managed 模式非空。 */
  credential_ref: string
  credential_state: CredentialState
  credential_version: number
  /** 托管密钥尾号；长度不足 8 位为空串。 */
  last4: string
  scope: ResourceScope
  /** exclusive 时的所有者 Agent 名；shared 为空串。 */
  owner_agent: string
  /** 行级修订号；资源条件 ETag 的组成之一。 */
  revision: number
  /** 资源条件 ETag（定义 + 范围 + 凭据版本 / 状态 + revision）。 */
  etag: string
  /** 文档引用扫描（排序去重，归档项带 archive: 前缀）；不是精确绑定。 */
  referenced_by: string[]
  /**
   * 本地配置就绪三值；**不等于**实测可用——实测只来自 `last_test`，
   * 页面不得把它显示成「已验证」。
   */
  availability: 'ready' | 'unconfigured' | 'unavailable'
  /** 最后一次连通性测试结果；null = 从未测过。 */
  last_test: ConnectionTestView | null
}

export interface ScheduleView {
  id: string
  agent: string
  cron: string
  zone: string
  message: string
  /** 系统代投渠道的资源 UID；空串 = 不代投（R7）。 */
  deliver_to: string
  enabled: boolean
  last_run_at: string
  last_status: string
}

/** 一次执行记录；投递结果与执行结果**分开**（R7）。 */
export interface ExecutionView {
  task_id: string
  run_id: string
  status: string
  trigger: string
  duration_ms: number
  error_message: string
  started_at: string
  finished_at: string
  /** 空串 = 本行没有投递记录（任务未绑定投递目标）。 */
  delivery_status: string
  /** 渠道资源 UID。 */
  delivery_channel: string
  /** 脱敏短语。 */
  delivery_detail: string
}

/** 最后一次连接测试的脱敏结果；null = 从未测过（**不得**用 availability 冒充）。 */
export interface ConnectionTestView {
  status: 'success' | 'failed' | 'unknown'
  /** RFC3339 UTC，测试完成时刻。 */
  tested_at: string
  detail: string
}

export interface SkillView {
  name: string
  description: string
  path: string
  version_id: number
  version_no: number
  content_hash: string
  etag: string
  file_count: number
  referenced_by: string[]
}

/**
 * 工具来源里的一跳：赢家来源是 `builtin` 或某个 MCP server 名（无法确定时为
 * `unknown`）；跳过原因只有两个 code，不返回自由文本（契约 McpToolSkip）。
 * One skipped tool: the winning source plus one fixed reason code.
 */
export interface McpToolSkip {
  name: string
  winner_source: string
  reason: 'duplicate_name' | 'invalid_tool'
}

/**
 * 一个 server 的注册投影（契约 McpToolDisposition）：offered 是它对注册池提出的
 * 工具数，accepted 是注册成功的数；恒有 `offered === accepted + skipped.length`。
 * 它**不**改变 `McpServerView.tools` 的语义——后者仍是 offered 名单。
 * The registration outcome; it never redefines a server's offered tool list.
 */
export interface McpToolDisposition {
  offered: number
  accepted: number
  skipped: McpToolSkip[]
}

export interface McpServerView {
  name: string
  transport: 'stdio' | 'http'
  /** 仅可执行名；参数不展示。 */
  command: string
  /** 只回变量名，不返回值。 */
  env_keys: string[]
  connected: boolean
  /** 该 server 提供的工具名（offered）；被谁抢走看 `tool_disposition`。 */
  tools: string[]
  published_version_id: number
  published_version_no: number
  applied_version_id: number
  applied_version_no: number
  pending_restart: boolean
  /** 未连接或未知时为 null——不拿"没有结果"冒充"全部注册成功"。 */
  tool_disposition: McpToolDisposition | null
}

/** 目录 warnings 固定集合（契约 McpCatalogEntry.warnings，七个 code，与诊断集合各自独立）。 */
export type MCPCatalogWarning =
  | 'read_only_tool_face_required'
  | 'server_write_capability'
  | 'write_tool_name_collision'
  | 'browser_required'
  | 'sqlite_sample_only'
  | 'package_entry_unverified'
  | 'stdio_child_egress_unconstrained'

/**
 * 一条 env 占位声明（契约 McpCatalogEnv）：只有键名与 `${KEY}` 占位符，**没有值**。
 * 它是"该填哪个变量"的提示，不是凭据；保存时由服务端 env 闸门核对真实进程环境。
 * One env placeholder declaration; it never carries a value.
 */
export interface MCPCatalogEnv {
  key: string
  placeholder: string
  required: boolean
  description: string
}

/**
 * 一条可复制的 server 配方（契约 McpCatalogEntry）：`command` 与 `url` 按
 * `transport` 二选一，另一侧为空串；`args` 里的 `<...>` 只是编辑标记，不是运行时
 * 变量；`verified=false` 时 `verified_at` 与 `verified_platforms` 为空，且
 * `verification_note` 写清待实测项。
 * One copyable recipe; the verified quartet keeps measured apart from not yet measured.
 */
export interface MCPCatalogEntry {
  id: string
  display_name: string
  summary: string
  category: string
  transport: 'stdio' | 'http'
  command: string
  args: string[]
  url: string
  env: MCPCatalogEnv[]
  requires: string[]
  source_url: string
  verified: boolean
  verification_note: string
  /** RFC 3339；`verified=false` 时为空串。 */
  verified_at: string
  verified_platforms: ('windows' | 'linux' | 'darwin')[]
  warnings: MCPCatalogWarning[]
}

/** 整份只读目录（契约 McpCatalog）：`items` 由服务端按 `id` 升序给出，页面不重排。 */
export interface MCPCatalog {
  version: number
  items: MCPCatalogEntry[]
}

/**
 * 命令定位三态（契约 McpCommandDiagnosis.state）：`unconfirmed` **不是**失败，只是
 * 按现行启动构造无法确定；测试仍会真实尝试，分类由真实错误给出。
 * The three-state lookup verdict; unconfirmed never blocks a test attempt.
 */
export type McpCommandState = 'located' | 'not_found' | 'unconfirmed'

/** 定位原因的固定集合（契约 McpCommandDiagnosis.reason）。 */
export type McpCommandReason =
  | 'lookpath'
  | 'absolute'
  | 'not_found'
  | 'relative_path'
  | 'script'
  | 'permission'
  | 'unknown'

export interface McpCommandDiagnosis {
  state: McpCommandState
  /** 仅 `located` 时非空。 */
  path: string
  reason: McpCommandReason
}

/** 单个环境变量是否就绪；只回布尔，不回值（契约 McpEnvDiagnosis）。 */
export interface McpEnvDiagnosis {
  key: string
  set: boolean
}

/** 诊断 warnings 固定集合（契约 McpServerDiagnosis.warnings，六个 code）。 */
export type McpDiagnosticWarning =
  | 'stdio_child_egress_unconstrained'
  | 'remote_mcp_downstream_unconstrained'
  | 'args_placeholders_not_resolved'
  | 'package_entry_unverified'
  | 'http_auth_header_unsupported'
  | 'env_gate_process_local'

/**
 * MCP server 只读诊断（契约 McpServerDiagnosis）：命令三态、env 就绪布尔、固定
 * warnings 与检查时间。**不含** `connected`——诊断不预测连接结果，页面据此分轴
 * 展示，不把诊断结论当成测试结论。
 * The read-only diagnosis; it deliberately carries no connected flag.
 */
export interface McpServerDiagnostics {
  name: string
  transport: 'stdio' | 'http'
  published_version_id: number
  applied_version_id: number
  /** 只由 published != applied 且 published != 0 决定，与 connected 无关。 */
  pending_restart: boolean
  /** stdio 为对象；http 为 null。 */
  command: McpCommandDiagnosis | null
  /** 按 key 升序；只回布尔。 */
  env: McpEnvDiagnosis[]
  warnings: McpDiagnosticWarning[]
  /** RFC 3339 UTC。 */
  checked_at: string
}

export interface ModelConnectionView {
  name: string
  /** 不可变资源身份；同名重建会生成新 UID。 */
  resource_uid: string
  base_url: string
  /** env 模式只回 `${ENV_VAR}` 引用名；managed 模式为空串。 */
  api_key_env: string
  default_model: string
  credential_mode: CredentialMode
  /** 只读凭据引用；仅 managed 模式非空。 */
  credential_ref: string
  credential_state: CredentialState
  credential_version: number
  /** 托管密钥尾号；长度不足 8 位为空串。 */
  last4: string
  scope: ResourceScope
  /** exclusive 时的所有者 Agent 名；shared 为空串。 */
  owner_agent: string
  /** 行级修订号；资源条件 ETag 的组成之一。 */
  revision: number
  published_version_id: number
  published_version_no: number
  applied_version_id: number
  applied_version_no: number
  /** 资源条件 ETag（定义 + 范围 + 凭据版本 / 状态 + revision）。 */
  etag: string
  referenced_by: string[]
  /** 本地配置就绪三值；**不等于**实测可用（见 `last_test`）。 */
  availability: 'ready' | 'unconfigured' | 'unavailable'
  /** 最后一次连通性测试结果；null = 从未测过。 */
  last_test: ConnectionTestView | null
}

/** 网络域名白名单的实际生效状态（契约 NetworkAllowlistView）。 */
export interface NetworkAllowlistView {
  /** 条件写的 If-Match 唯一取值；尚未装载（revision 0）时为空串。 */
  etag: string
  /** 从 1 起，每次成功写入 +1；尚未装载为 0。 */
  revision: number
  /** config_seed = 首次启动由配置写入；database = 来自管理台保存。 */
  source: 'config_seed' | 'database'
  /** 最近一次写入时间；零值渲染为空串。 */
  updated_at: string
  /** 脱敏写入者（token:<指纹> / local_trust）；种子行为空串。 */
  updated_by: string
}

export interface SandboxView {
  file_allowed_paths: string[]
  shell_allowed_commands: string[]
  shell_interpreters: string[]
  /** 当前实际生效的域名；权威值在 `network_allowlist`，此字段只是同一份数据的列表投影。 */
  http_allowed_domains: string[]
  network_allowlist: NetworkAllowlistView
}

export interface ToolView {
  name: string
  description: string
  source: 'builtin' | 'mcp' | 'code'
}

export interface SystemOverview {
  version: string
  uptime_seconds: number
  started_at: string
  agents: {
    total: number
    active: number
    disabled: number
    archived: number
    invalid: number
  }
  runs: {
    running: number
    last_24h: number
    failed_last_24h: number
  }
  resources: {
    model_connections: number
    mcp_servers: number
    skills: number
  }
  database: {
    status: 'ok' | 'unavailable'
    schema_version: number
  }
  auth: {
    mode: 'token' | 'local_trust'
    loopback_only: boolean
  }
}

export interface SystemSample {
  sampled_at: string
  /** 平台不支持或首次采样无差值基线时为 null，并列入 unavailable；不得伪造 0。 */
  cpu_percent: number | null
  memory_rss_bytes: number | null
  goroutines: number | null
  unavailable: string[]
}

export type OperationStatus = 'pending' | 'success' | 'failed' | 'unknown'
export type OperationTargetKind =
  | 'agent'
  | 'schedule'
  | 'model_connection'
  | 'mcp_server'
  | 'skill'
  | 'notify_channel'
  | 'network_allowlist'
  | 'system'

export interface OperationView {
  operation_id: string
  action: string
  target_kind: OperationTargetKind
  target_id: string
  actor_subject: string
  auth_method: 'bearer' | 'local_trust' | 'cli' | 'migration'
  source: string
  status: OperationStatus
  result_code: string
  error_message: string
  created_at: string
  finished_at: string
}

export interface OperationDetail extends OperationView {
  request_hash: string
  requested_version_id: number
  expected_etag: string
  detail: Record<string, unknown>
  recovered_at: string
  verification: 'not_required' | 'applied' | 'not_applied' | 'indeterminate'
}

// ---------------------------------------------------------------------------
// 错误与请求
// ---------------------------------------------------------------------------

/** 认证失效回调：参数是发生失效时的站内路径（交给 router.safeNextPath 清洗）。 */
export type AuthFailureHandler = (next: string) => void

/** ApiClient 构造选项；未提供 onAuthFailure 时只清凭据、不跳转。 */
export interface ApiClientOptions {
  onAuthFailure?: AuthFailureHandler
}

/**
 * 失败信封的 data：契约只允许四个固定字符串数组（agents / active_runs /
 * run_ids / problems）与三个非空字符串（current_etag / operation_id /
 * resource_uid）。没有 data 为 null；空数组表示"点名了零个"，与未携带不同，
 * 页面不得把两者混为一谈。
 * The whitelisted failure payload; absent data is null while [] stays [].
 */
export interface ApiErrorData {
  agents?: string[]
  active_runs?: string[]
  run_ids?: string[]
  problems?: string[]
  /** 条件写失败（412）时服务端当前 ETag；页面据此重新取 ETag 后再写。 */
  current_etag?: string
  /** `pending` 之后失败的管理操作标识；页面据此可经 GET /operations/{id} 追查。 */
  operation_id?: string
  /** `RESOURCE_UID_MISMATCH` 时服务端当前的资源身份。 */
  resource_uid?: string
}

/**
 * ApiError 保留 HTTP 状态、固定错误码、Retry-After 秒数与白名单化的失败数据；
 * message 直接用服务端固定文案（契约保证不含路径 / 凭证）。
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly retryAfterSeconds: number | null
  readonly data: ApiErrorData | null

  constructor(
    status: number,
    code: string,
    message: string,
    retryAfterSeconds: number | null = null,
    data: ApiErrorData | null = null,
  ) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.retryAfterSeconds = retryAfterSeconds
    this.data = data
  }
}

/**
 * 统一错误文案：ApiError 直接使用服务端固定文案；429 追加 Retry-After 秒数，
 * 便于页面提示"稍后重试"。其他异常回退到 message 或通用文案。
 * Maps any thrown value to a user-facing sentence; 429 appends the retry delay.
 */
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.retryAfterSeconds !== null) {
      return `${error.message}（${error.retryAfterSeconds} 秒后重试）`
    }
    return error.message
  }
  if (error instanceof Error && error.message !== '') {
    return error.message
  }
  return '请求失败'
}

/**
 * 12 个资源 / 凭据错误的页面文案（字面量以契约 run-and-errors.md 为准）。
 * 服务端 message 是固定短语，但只说"哪一类被拒"；这里补"下一步怎么做"。
 * 码表外返回空串，由调用方回退到 errorMessage。
 * Page-level copy for the twelve resource/credential codes; unknown codes → ''.
 */
export const RESOURCE_ERROR_COPY: Readonly<Record<string, string>> = {
  AGENT_RESOURCE_BINDING_MISSING:
    '配置引用缺少对应的资源关联：请显式选择资源后重新保存。',
  AGENT_RESOURCE_BINDING_INVALID:
    '资源关联形状非法、重复、是孤儿或指向旧资源：请重新选择当前资源。',
  AGENT_RESOURCE_UNAVAILABLE: '关联的资源不存在、已删除或尚未发布：请先发布资源或重新关联。',
  RESOURCE_NOT_ELIGIBLE: '资源不属于该 Agent（共享或本人专属之外），或渠道不在声明内。',
  CREDENTIAL_REVOKED: '关联资源的凭据已撤销：请在该资源上重新录入凭据（旧版本不会复活）。',
  CREDENTIAL_UNAVAILABLE:
    '主密钥缺失、不可读或无法解密：请检查 secrets.master_key_file 与文件权限。',
  CREDENTIAL_MODE_REQUIRED:
    '无法按值的形态安全判断凭据来源：请显式选择「环境变量」或「加密托管」。',
  CREDENTIAL_REF_READ_ONLY: 'credential_ref 是服务端只读字段，请求体里不能出现。',
  CREDENTIAL_OWNER_MISMATCH: '凭据属于其他资源：不得跨资源复用。',
  RESOURCE_SCOPE_IN_USE: '资源仍被其他 Agent 配置引用或运行占用：先解除，再切换为专属。',
  RESOURCE_UID_MISMATCH: '页面持有的资源身份已过期（可能已同名重建）：请重新读取后再操作。',
  AGENT_COPY_BLOCKED: '复制遇到专属资源引用：请为目标 Agent 选择映射资源。',
}

/**
 * 按错误码取页面文案；码表外返回空串。页面能在本地就判定的拒绝（例如保存前
 * 发现引用未绑定）也取这一份文案，避免与失败响应的提示分叉。
 * Returns the page copy for a known resource error code, or ''.
 */
export function resourceErrorCopyOf(code: string): string {
  return RESOURCE_ERROR_COPY[code] ?? ''
}

/**
 * 取某个错误的资源文案；非 ApiError 或码表外返回空串（调用方回退 errorMessage）。
 * Returns the resource copy for an ApiError code, or '' when there is none.
 */
export function resourceErrorCopy(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return ''
  }
  return resourceErrorCopyOf(error.code)
}

type QueryValues = Record<string, string | number | undefined>

/**
 * 请求选项：US-12 的写面函数（api-admin.ts）与本文件共用。`allowEmptyData`
 * 只给无载荷成功端点（DELETE 一类）用——正常端点缺 data 仍按协议错误处理。
 * Shared request options; `allowEmptyData` is only for payload-less successes.
 */
export interface RequestOptions {
  query?: QueryValues
  body?: unknown
  ifMatch?: string
  signal?: AbortSignal
  allowEmptyData?: boolean
}

interface RawEnvelope {
  code: string
  message: string
  data?: unknown
}

interface CallResult<T> {
  data: T
  code: string
  message: string
  /** 响应头 ETag（无该头时为空串）；资源条件写的取值来源。 */
  etag: string
}

export interface SessionFilter {
  agent?: string
  status?: 'active' | 'archived'
}

export interface RunFilter {
  session_id?: string
  agent?: string
  status?: RunStatus
  limit?: number
}

export interface OperationFilter {
  action?: string
  target_kind?: OperationTargetKind
  target_id?: string
  status?: OperationStatus
  limit?: number
}

/**
 * ApiClient 是管理台唯一的 HTTP 出口：视图不直接调用 fetch，凭据与 401 策略只在此实现。
 * The single HTTP exit for the console; views never call fetch directly.
 */
export class ApiClient {
  readonly #auth: ConsoleAuth
  readonly #onAuthFailure: AuthFailureHandler

  constructor(auth: ConsoleAuth, options: ApiClientOptions = {}) {
    this.#auth = auth
    this.#onAuthFailure = options.onAuthFailure ?? (() => {})
  }

  // --- 写面扩展出口（US-12）-----------------------------------------------
  //
  // 写面端点定义在 api-admin.ts，但 HTTP 调用必须经这三个方法返回本文件：
  // 凭据注入、信封解包、401 策略与 Retry-After **只在这里实现**（约束 45 的
  // "只有一个 HTTP 出口"）。新增端点不得自己调用 fetch。
  // Write-surface endpoints live in api-admin.ts but must call through these
  // methods, so credential injection and the 401 policy stay in this file only.

  /** 发一次请求并返回 data 载荷。 */
  async request<T>(method: string, path: string, options: RequestOptions = {}): Promise<T> {
    const result = await this.#call<T>(method, path, options)
    return result.data
  }

  /** 同 request，并把响应头 ETag 一并返回（资源写面 If-Match 的取值来源）。 */
  async requestWithEtag<T>(
    method: string,
    path: string,
    options: RequestOptions = {},
  ): Promise<{ data: T; etag: string }> {
    const result = await this.#call<T>(method, path, options)
    return { data: result.data, etag: result.etag }
  }

  /** 无载荷成功（DELETE 等）：200 信封可以不带 data，其余错误照常抛出。 */
  async requestOK(method: string, path: string, options: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>(method, path, { ...options, allowEmptyData: true })
  }

  // --- 元数据与总览 -------------------------------------------------------

  /** 登录凭据校验端点（D-04：GET /info）。 */
  getInfo(): Promise<InfoView> {
    return this.#request<InfoView>('GET', '/info')
  }

  getSystemOverview(): Promise<SystemOverview> {
    return this.#request<SystemOverview>('GET', '/system/overview')
  }

  getSystemSample(): Promise<SystemSample> {
    return this.#request<SystemSample>('GET', '/system/sample')
  }

  listProfiles(): Promise<ProfileView[]> {
    return this.#request<ProfileView[]>('GET', '/profiles')
  }

  listTools(): Promise<ToolView[]> {
    return this.#request<ToolView[]>('GET', '/tools')
  }

  getSandbox(): Promise<SandboxView> {
    return this.#request<SandboxView>('GET', '/sandbox')
  }

  getGlobalMemory(): Promise<MemoryView> {
    return this.#request<MemoryView>('GET', '/memory')
  }

  listNotifyChannels(): Promise<NotifyChannelView[]> {
    return this.#request<NotifyChannelView[]>('GET', '/notify-channels')
  }

  // --- Agent 配置与生命周期 ----------------------------------------------

  getAgentContent(name: string): Promise<AgentContent> {
    return this.#request<AgentContent>('GET', `/agents/${encodeURIComponent(name)}/content`)
  }

  /** 条件写：If-Match 必须是强 ETag，不匹配由服务端返回 412。 */
  updateAgentContent(
    name: string,
    update: AgentContentUpdate,
    ifMatch: string,
  ): Promise<AgentFilesView> {
    return this.#request<AgentFilesView>('PUT', `/agents/${encodeURIComponent(name)}/content`, {
      body: update,
      ifMatch,
    })
  }

  disableAgent(name: string, transition: AgentLifecycleTransition): Promise<AgentLifecycleView> {
    return this.#request<AgentLifecycleView>(
      'POST',
      `/agents/${encodeURIComponent(name)}/disable`,
      { body: transition },
    )
  }

  enableAgent(name: string, transition: AgentLifecycleTransition): Promise<AgentLifecycleView> {
    return this.#request<AgentLifecycleView>(
      'POST',
      `/agents/${encodeURIComponent(name)}/enable`,
      { body: transition },
    )
  }

  getAgentMemory(name: string): Promise<MemoryView> {
    return this.#request<MemoryView>('GET', `/agents/${encodeURIComponent(name)}/memory`)
  }

  // --- 会话与运行 ---------------------------------------------------------

  listSessions(filter: SessionFilter = {}): Promise<SessionSummary[]> {
    return this.#request<SessionSummary[]>('GET', '/sessions', {
      query: { agent: filter.agent, status: filter.status },
    })
  }

  getSession(sessionId: string): Promise<SessionDetail> {
    return this.#request<SessionDetail>('GET', `/sessions/${encodeURIComponent(sessionId)}`)
  }

  /**
   * 创建会话：同一 Agent / 渠道 / 用户组合幂等返回既有会话（契约 /sessions POST）。
   * Creating a session is idempotent for the same agent/channel/user combination.
   */
  createSession(
    agent: string,
    options: { channel?: string; user_id?: string } = {},
  ): Promise<SessionSummary> {
    const body: Record<string, string> = { agent }
    if (options.channel !== undefined) {
      body.channel = options.channel
    }
    if (options.user_id !== undefined) {
      body.user_id = options.user_id
    }
    return this.#request<SessionSummary>('POST', '/sessions', { body })
  }

  /** 发消息并同步等待 Run 完成；limit_reached 仍是 200 的正常结果。 */
  sendSessionMessage(sessionId: string, message: string): Promise<RunView> {
    return this.#request<RunView>('POST', `/sessions/${encodeURIComponent(sessionId)}/messages`, {
      body: { message },
    })
  }

  /** 归档会话（不删数据）；有在途 Run 时服务端 409 SESSION_BUSY。 */
  archiveSession(sessionId: string): Promise<void> {
    return this.requestOK('DELETE', `/sessions/${encodeURIComponent(sessionId)}`)
  }

  listRuns(filter: RunFilter = {}): Promise<RunView[]> {
    return this.#request<RunView[]>('GET', '/runs', {
      query: {
        session_id: filter.session_id,
        agent: filter.agent,
        status: filter.status,
        limit: filter.limit,
      },
    })
  }

  getRun(runId: string): Promise<RunDetail> {
    return this.#request<RunDetail>('GET', `/runs/${encodeURIComponent(runId)}`)
  }

  /** 取消运行；已是终态时 200 + 提示文案（不产生第二次副作用）。 */
  async cancelRun(runId: string): Promise<RunCancelResult> {
    const result = await this.#call<RunView>(
      'POST',
      `/runs/${encodeURIComponent(runId)}/cancel`,
    )
    return { run: result.data, message: result.message }
  }

  // --- 只读资源 -----------------------------------------------------------

  listSkills(): Promise<SkillView[]> {
    return this.#request<SkillView[]>('GET', '/skills')
  }

  listMcpServers(): Promise<McpServerView[]> {
    return this.#request<McpServerView[]>('GET', '/mcp-servers')
  }

  listModelConnections(): Promise<ModelConnectionView[]> {
    return this.#request<ModelConnectionView[]>('GET', '/model-connections')
  }

  // --- 定时与操作审计 -----------------------------------------------------

  /** /schedules 无 agent 过滤参数；Agent 详情页在客户端按 agent 过滤。 */
  listSchedules(): Promise<ScheduleView[]> {
    return this.#request<ScheduleView[]>('GET', '/schedules')
  }

  /** 启用 / 停用只写数据库状态；任务定义仍以 AGENT.md 为准。 */
  setScheduleEnabled(scheduleId: string, enabled: boolean): Promise<void> {
    return this.requestOK('PUT', `/schedules/${encodeURIComponent(scheduleId)}`, {
      body: { enabled },
    })
  }

  /** 手动补跑：与到点触发同一条链路，同步返回 RunView。 */
  runSchedule(scheduleId: string): Promise<RunView> {
    return this.#request<RunView>('POST', `/schedules/${encodeURIComponent(scheduleId)}/run`)
  }

  /**
   * 某任务的执行历史（新→旧）。执行结果与投递结果分列返回：`status` 是本轮运行
   * 是否成功，`delivery_status` 是系统代投是否送达——两者不得互相代替（R7）。
   * Execution history, newest first; run status and delivery status are separate.
   */
  listExecutions(taskId: string): Promise<ExecutionView[]> {
    return this.#request<ExecutionView[]>(
      'GET',
      `/schedules/${encodeURIComponent(taskId)}/executions`,
    )
  }

  listOperations(filter: OperationFilter = {}): Promise<OperationView[]> {
    return this.#request<OperationView[]>('GET', '/operations', {
      query: {
        action: filter.action,
        target_kind: filter.target_kind,
        target_id: filter.target_id,
        status: filter.status,
        limit: filter.limit,
      },
    })
  }

  getOperation(operationId: string): Promise<OperationDetail> {
    return this.#request<OperationDetail>(
      'GET',
      `/operations/${encodeURIComponent(operationId)}`,
    )
  }

  // --- 内部实现 -----------------------------------------------------------

  async #request<T>(method: string, path: string, options: RequestOptions = {}): Promise<T> {
    const result = await this.#call<T>(method, path, options)
    return result.data
  }

  async #call<T>(method: string, path: string, options: RequestOptions = {}): Promise<CallResult<T>> {
    const headers: Record<string, string> = { Accept: 'application/json', ...this.#auth.headers() }
    let body: string | undefined
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(options.body)
    }
    if (options.ifMatch !== undefined) {
      headers['If-Match'] = options.ifMatch
    }

    let response: Response
    try {
      response = await fetch(buildURL(path, options.query), {
        method,
        headers,
        body,
        signal: options.signal,
      })
    } catch (error) {
      if (isAbortError(error)) {
        throw error
      }
      throw new ApiError(0, 'NETWORK', '无法连接到服务器，请检查网络或服务状态')
    }

    const envelope = await readEnvelope(response)
    const retryAfter = parseRetryAfter(response)
    if (!response.ok) {
      this.#handleAuthFailure(response.status)
      throw new ApiError(
        response.status,
        envelope.code,
        envelope.message,
        retryAfter,
        errorData(envelope.data),
      )
    }
    if (!options.allowEmptyData && (!('data' in envelope) || envelope.data === undefined)) {
      throw new ApiError(0, 'PROTOCOL', '响应缺少 data 载荷')
    }
    return {
      data: envelope.data as T,
      code: envelope.code,
      message: envelope.message,
      etag: response.headers.get('ETag') ?? '',
    }
  }

  /** 401 与 local_trust 下的 403 视为认证失效：清内存凭据后交给调用方跳登录。 */
  #handleAuthFailure(status: number): void {
    const expired = status === 401 || (status === 403 && this.#auth.mode === 'local_trust')
    if (!expired) {
      return
    }
    this.#auth.clear()
    this.#onAuthFailure(currentLocation())
  }
}

function buildURL(path: string, query?: QueryValues): string {
  const base = `${API_BASE}${path}`
  if (query === undefined) {
    return base
  }
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === '') {
      continue
    }
    params.set(key, String(value))
  }
  const serialized = params.toString()
  return serialized === '' ? base : `${base}?${serialized}`
}

async function readEnvelope(response: Response): Promise<RawEnvelope> {
  let payload: unknown
  try {
    payload = await response.json()
  } catch {
    payload = null
  }
  if (isEnvelope(payload)) {
    return payload
  }
  return {
    code: response.ok ? 'OK' : 'INTERNAL',
    message: response.ok ? '' : '服务器返回了无法识别的响应',
  }
}

function isEnvelope(value: unknown): value is RawEnvelope {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const record = value as Record<string, unknown>
  return typeof record.code === 'string' && typeof record.message === 'string'
}

/**
 * 从失败信封的 data 里只取契约固定的白名单字段：四个字符串数组与三个非空
 * 字符串（空串按"未携带"处理，与服务端投影同口径）；形状不符的键直接丢弃，
 * 未带任何白名单字段时返回 null。凭证、定义正文没有出站载体。
 * Keeps only the whitelisted fields; anything else is dropped.
 */
function errorData(value: unknown): ApiErrorData | null {
  if (typeof value !== 'object' || value === null) {
    return null
  }
  const record = value as Record<string, unknown>
  const data: ApiErrorData = {}
  let present = false
  const keys = ['agents', 'active_runs', 'run_ids', 'problems'] as const
  for (const key of keys) {
    const entry = record[key]
    if (Array.isArray(entry) && entry.every((item) => typeof item === 'string')) {
      data[key] = entry as string[]
      present = true
    }
  }
  const scalarKeys = ['current_etag', 'operation_id', 'resource_uid'] as const
  for (const key of scalarKeys) {
    const entry = record[key]
    if (typeof entry === 'string' && entry !== '') {
      data[key] = entry
      present = true
    }
  }
  return present ? data : null
}

function parseRetryAfter(response: Response): number | null {
  const raw = response.headers.get('Retry-After')
  if (raw === null || !/^\d+$/.test(raw.trim())) {
    return null
  }
  return Number.parseInt(raw, 10)
}

function isAbortError(error: unknown): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    'name' in error &&
    (error as { name?: unknown }).name === 'AbortError'
  )
}

/** 当前站内路径（401 保留 next 用）；无 window（非浏览器环境）时返回空串。 */
function currentLocation(): string {
  if (typeof window === 'undefined') {
    return ''
  }
  return `${window.location.pathname}${window.location.search}`
}
