// 管理写面 API：US-12 的写端点在**这里**定义请求形状与 If-Match，但真正的
// HTTP 出口仍是 api.ts 的 ApiClient——凭据注入、信封解包、401 与 Retry-After
// 只有那一处实现（约束 45：页面不直接调用 fetch，也不新增第二套客户端）。
// 视图只通过 `new AdminApi(ctx.api)` 使用本文件；本文件不发请求、不读 DOM。
// Write-surface endpoints for US-12: request shapes and If-Match live here, while
// every call goes through ApiClient so the single HTTP/auth exit stays in api.ts.
import { ApiError } from './api'
import type { ApiClient } from './api'
import type {
  AgentConfigView,
  AgentFilesView,
  CredentialMode,
  ExecutionPolicyView,
  ExecutionProfile,
  InstanceSecretsView,
  McpCredentialKeyMode,
  McpServerView,
  ModelConnectionView,
  NetworkExceptionCreateResult,
  NetworkExceptionDeleteResult,
  NotifyChannelView,
  PolicyConfirmationResult,
  ResourceScope,
  SandboxView,
  SkillView,
} from './api'

// ---------------------------------------------------------------------------
// 契约视图类型（docs/contracts/openapi.yaml 的 components.schemas 摘要）
// Contract view types; field names stay snake_case on purpose.
// ---------------------------------------------------------------------------

/** `POST /agents` 的请求体（description / model 可省略）。 */
export interface AgentCreateRequest {
  name: string
  description?: string
  model?: string
}

/** 一份待提交的文件（相对 Agent 目录的路径 + UTF-8 正文）。 */
export interface FileDraft {
  path: string
  content: string
}

/** 作者模型的一轮澄清问题（contract AgentDraftView.questions）：逐项必填后才继续。 */
export interface AgentDraftQuestion {
  id: string
  question: string
}

/** 一条问答答案：只随本次请求进入提示词，不落盘、不写日志。 */
export interface AgentDraftAnswer {
  id: string
  question: string
  answer: string
}

/** 作者草稿：未落盘、无 ETag、不是磁盘真值（契约 AgentDraftView）；files 与 questions 互斥。 */
export interface AgentDraftView {
  agent: string
  files: FileDraft[]
  questions: AgentDraftQuestion[]
}

/** 定时候选（契约 ScheduleDraftView）：尚未保存；触发时间由服务端计算。 */
export interface ScheduleDraftView {
  agent: string
  id: string
  cron: string
  zone: string
  message: string
  next_runs: string[]
}

/** 源专属资源到目标资源的显式映射（复制携带专属引用时必填）。 */
export interface ResourceMapping {
  resource_kind: CredentialResourceKind
  source_resource_uid: string
  target_resource_uid: string
}

export interface AgentCopyRequest {
  new_name: string
  /** 源专属资源的映射；缺映射的专属引用 → 409 `AGENT_COPY_BLOCKED`。 */
  resource_mapping?: ResourceMapping[]
}

/** 恢复请求：new_name 留空 = 恢复原目录名。 */
export interface AgentRestoreRequest {
  new_name?: string
}

export interface AgentArchiveView {
  archive_id: string
  agent: string
  archived_at: string
  content_hash: string
  restored_at: string
  restored_agent_name: string
}

export type ArchivedAgentView = AgentArchiveView

export interface ModelConnectionUpsert {
  name: string
  base_url: string
  default_model: string
  /**
   * env 模式：精确 `${ENV_VAR}`；managed 模式：write-only 完整新值。
   * 省略 = 保留；空串 = 400（页面不回填已存凭据）。
   */
  api_key?: string
  /** 省略：创建按 env 规则、更新保持当前模式；显式切换必须给目标模式所需的值。 */
  credential_mode?: CredentialMode
  /** 仅创建时可给；缺省 shared。后续切换走 `/scope` 端点。 */
  scope?: ResourceScope
  /** scope=exclusive 时必填，必须是在世 Agent 名。 */
  owner_agent?: string
}

/** 带使用范围的资源种类（范围与凭据动作只覆盖这两类）。 */
export type CredentialResourceKind = 'model_connection' | 'notify_channel'

/**
 * 凭据属主种类（契约 `CredentialOwnerKind`）：比 `CredentialResourceKind` 多
 * `mcp_server`——MCP server 有逐占位符的托管凭据，但没有 shared / exclusive 范围，
 * 因此只有撤销走这条 kind。
 * Credential-owner kinds: an MCP server has per-slot credentials but no scope.
 */
export type CredentialOwnerKind = CredentialResourceKind | 'mcp_server'

/** 撤销托管凭据：撤销整个凭据身份，旧版本永不复活。 */
export interface CredentialRevokeRequest {
  /** 必须等于当前指针 UID；不一致 → 409 `RESOURCE_UID_MISMATCH`。 */
  resource_uid: string
  /** 必须显式为 true（契约 enum [true]）。 */
  confirm: true
  /**
   * 仅 `kind=mcp_server` 必填（占位符名，env 键或 header 名；缺失 → 400）；其它 kind
   * 出现 → 400。撤销粒度是单个占位符，同一 server 的其它占位符不受影响。
   * Required for MCP (one slot at a time); rejected for other kinds.
   */
  binding_key?: string
}

export interface CredentialRevokeResult {
  resource_uid: string
  /** 仅 `kind=mcp_server` 时非空（被撤销的占位符名）；其它 kind 为空串。 */
  binding_key: string
  credential_ref: string
  credential_state: 'revoked'
  revoked_versions: number
  revocation_epoch: number
}

/** 切换资源使用范围；共享转专属被引用时 409 且不提供 force。 */
export interface ResourceScopeRequest {
  resource_uid: string
  scope: ResourceScope
  /** scope=exclusive 时必填。 */
  owner_agent?: string
}

export interface ResourceScopeResult {
  resource_uid: string
  scope: ResourceScope
  owner_agent: string
  revision: number
  etag: string
  /** `all_eligible` 模式下会受影响但不会被自动修改的 Agent 名（影响提示）。 */
  affected_eligible_agents: string[]
}

/** 按 `resource_bindings` 的 UID 精确引用该资源的一个 Agent。 */
export interface ResourceBoundAgentRef {
  agent: string
  agent_uid: string
  archived: boolean
}

export interface ModelConnectionTestResult {
  ok: boolean
  latency_ms: number
  model: string
  error_code: string
  error_message: string
}

/**
 * 渠道连通性测试结果（契约 NotifyChannelTestResult）：测试会真实发出一条固定文案
 * `Basalt 连通性测试`。`status=unknown` 表示可能已送达（超时 / 回执不可解析），
 * 不得折叠成失败或成功；`ok=true` 当且仅当服务端确认受理。
 * The connectivity test sends one fixed text; an unknown verdict is not a failure.
 */
export interface NotifyChannelTestResult {
  ok: boolean
  status: 'success' | 'failed' | 'unknown'
  latency_ms: number
  detail: string
}

/** 新增内网访问例外的请求体（契约 NetworkExceptionCreate）：理由必填且不能全空白。 */
export interface NetworkExceptionCreate {
  kind: 'host' | 'ip_port'
  value: string
  note: string
}

export interface McpServerDetail extends McpServerView {
  args: string[]
  /** 仅 http 传输；凭证不得出现在 URL。 */
  url: string
}

/**
 * 一个占位符的来源声明（契约 McpCredentialInput）：`mode` 必填——来源不靠"托管行
 * 是否存在"推断。`value` 只往上走、永不回读：`managed` + 值 = 新版本；`managed` +
 * 省略 = 保留已有托管值；没有旧值又省略 → 400；`env` 带值 → 400。
 * One explicit source choice; a value only ever travels upward.
 */
export interface McpCredentialInput {
  /** `env` 的键名或 `headers` 的头名；同名即同一占位符（跨两者重名 → 400）。 */
  key: string
  mode: McpCredentialKeyMode
  /** write-only；不进日志 / 审计 / 模型上下文。 */
  value?: string
}

export interface McpServerUpsert {
  name: string
  transport: 'stdio' | 'http'
  command?: string
  args?: string[]
  url?: string
  /** 值只能是 `${ENV_VAR}`；明文凭证由服务端 400 拒绝。 */
  env?: Record<string, string>
  /**
   * 仅 http。值只能是整值 `${VAR}`；`Authorization` 装配为 `Bearer <value>`，其它
   * 头名原样装配，不做前缀推导。只在内存里拼接，不回读。
   * HTTP headers; values are whole `${VAR}` placeholders only.
   */
  headers?: Record<string, string>
  /**
   * 逐占位符显式选择来源；只对本次 `env` / `headers` 出现的占位符生效。省略某键 =
   * 沿用既有声明；合并后仍有占位符没有声明 → 400 点名该键（不隐式按 env 推断）。
   * Per-slot source choices; undeclared slots are rejected, never inferred.
   */
  credentials?: McpCredentialInput[]
}

/**
 * 表单预填载荷：与写入体同形——目录“添加到配置”只是把配方摆进表单，提交仍由
 * 用户点按钮，走同一条创建 / env 闸门 / 发布路径，没有第二套写入口。
 * The prefill payload has the write shape; saving still goes through the one write path.
 */
export type McpServerWrite = McpServerUpsert

/** 测试所处阶段（契约 McpServerTestResult.stage）：`complete` 也是成功的一种落点。 */
export type McpTestStage = 'preflight' | 'spawn' | 'initialize' | 'tools_list' | 'complete'

/**
 * 失败分类（契约 McpServerTestResult.failure_class）：成功为空串。分类只来自结构化
 * 失败，**不**解析 stderr / 日志。503 / 504 不返回本对象，页面改从操作详情读
 * `detail.stage` / `detail.failure_class`。
 * The fixed failure classes; success is the empty string.
 */
export type McpTestFailureClass =
  | ''
  | 'command_not_found'
  | 'permission_denied'
  | 'policy_denied'
  | 'timeout'
  | 'http_unauthorized'
  | 'http_unreachable'
  | 'handshake_failed'
  | 'tools_list_failed'
  | 'env_missing'
  | 'dependency_unconfirmed'

/**
 * 一次 MCP 连接测试的记录（契约 McpServerTestResult）。`tools` 始终存在：空数组表示
 * `tools/list` 成功但没有工具，**不是**失败；`tested_version_id=0` 表示无法确定。
 * The test record; an empty tool list is success, and version id 0 means unknown.
 */
export interface McpServerTestResult {
  ok: boolean
  latency_ms: number
  /** `tools/list` 的实名单；空数组 = 服务端未暴露工具，仍是 ok。 */
  tools: string[]
  error_code: string
  error_message: string
  /** 对应 `management_operations.operation_id`，可跳到操作详情。 */
  operation_id: string
  /** 被测试的版本；0 表示无法确定（历史 / 旧记录）。 */
  tested_version_id: number
  stage: McpTestStage
  failure_class: McpTestFailureClass
}

/**
 * 应用失败分类（契约 `McpServerApplyResult.failure_class`）：复用测试的全部取值并
 * 追加 `drain_timeout`（旧连接没在窗口内自然排空）。成功且排空正常时为空串。
 * Apply reuses the test classes and adds `drain_timeout`.
 */
export type McpApplyFailureClass = McpTestFailureClass | 'drain_timeout'

/** 应用请求体（契约 McpServerApplyRequest）：`0` / 省略 = 当前已发布版本。 */
export interface McpServerApplyRequest {
  version_id?: number
}

/**
 * 一次受控应用的结果（契约 McpServerApplyResult）。200 只会是 `applied`；
 * `superseded` / `failed` 以 409 / 500 返回，但同一形状（去掉 `server`）会写进操作行
 * `detail`，页面按 `operation_id` 查操作详情——所以失败分类可能只在详情里。
 * One apply outcome; failure details live in the operation row as well.
 */
export interface McpServerApplyResult {
  outcome: 'applied' | 'superseded' | 'failed'
  /** 切换后运行生效的版本；`superseded` / `failed` 时保持不变。 */
  applied_version_id: number
  /** 切换前的 applied 版本；未曾应用为 0。 */
  previous_version_id: number
  /** 本次冻结的目标版本。 */
  target_version_id: number
  /** 旧连接排空截止（RFC 3339 UTC）；`superseded` / `failed` 为空串。 */
  drain_deadline: string
  /** 排空窗口长度（默认 30 秒）。 */
  drain_timeout_seconds: number
  failure_class: McpApplyFailureClass
  /** 仅 `superseded` 时非空：被哪类并发写取代。 */
  superseded_by: '' | 'publish' | 'revoke' | 'apply'
  operation_id: string
  /** 仅 200 响应带；409 / 500 的操作行 detail 没有这一项。 */
  server?: McpServerView
}

export interface SkillFileView {
  path: string
  size: number
  sha256: string
  content: string
}

export interface SkillDetail extends SkillView {
  files: SkillFileView[]
}

export interface SkillCreate {
  name: string
  description: string
}

export interface SkillImport {
  name: string
  files: FileDraft[]
}

export interface SkillUpdate {
  files: FileDraft[]
}

export interface SkillValidateResult {
  valid: boolean
  problems: string[]
}

/** 资源种类的契约字面量（与 URL 路径一致；排序与路由表无关）。 */
export type AdminResourceKind = 'model_connection' | 'mcp_server' | 'skill'

export interface ResourceVersionView {
  version_id: number
  resource_kind: AdminResourceKind
  resource_id: string
  /** 该版本所属资源身份；同名重建后的新旧版本靠它区分。 */
  resource_uid: string
  version_no: number
  content_hash: string
  created_at: string
  operation_id: string
  published: boolean
  applied: boolean
  /** 非撤回版本为 null。 */
  reverted_from_version_id: number | null
}

export interface ResourceVersionDetail extends ResourceVersionView {
  /** 脱敏后的定义；凭证只能是 `${ENV_VAR}`。 */
  definition_yaml: string
  manifest: Record<string, unknown>
}

export interface PublishRequest {
  /** 留空 = 发布当前最新；给旧版本则先创建同内容新版本再发布。 */
  version_id?: number
}

export interface ResourcePublishResult {
  version: ResourceVersionView
  pending_restart: boolean
}

export interface ResourceReferencesView {
  kind: AdminResourceKind
  id: string
  resource_uid: string
  published_version_id: number
  /** 定义名引用（解析失败按可能引用处理）。 */
  agents: string[]
  /** 按 `resource_bindings` 的 UID 精确引用（含归档 Agent）。 */
  bound_agents: ResourceBoundAgentRef[]
  active_runs: string[]
}

export interface NotifyChannelUpsert {
  name: string
  type: string
  /**
   * env 模式：精确 `${ENV_VAR}`；managed 模式：write-only 完整新地址。
   * 更新时省略 = 保留原值；空串 = 400（页面不回填已存地址）。
   */
  url?: string
  description?: string
  /** 省略：创建按 env 规则、更新保持当前模式；显式切换必须给目标模式所需的值。 */
  credential_mode?: CredentialMode
  /** 缺省 shared；exclusive 必须给 owner_agent。 */
  scope?: ResourceScope
  /** scope=exclusive 时必填，必须是在世 Agent 名。 */
  owner_agent?: string
}

export interface WorkspaceEntry {
  /** 相对 `.basalt/agents/` 的路径。 */
  path: string
  type: 'file' | 'dir' | 'symlink'
  size: number
}

export interface WorkspaceFile {
  path: string
  content: string
  size: number
  /** 契约冻结为 false：超限直接 400，不截断。 */
  truncated: boolean
}

/** 带响应头 ETag 的详情结果：写面的 If-Match 用 `etag`，页面回显用 `data`。 */
export interface DetailWithEtag<T> {
  data: T
  etag: string
}

// ---------------------------------------------------------------------------
// 端点
// ---------------------------------------------------------------------------

/**
 * AdminApi 是 US-12 写面的唯一入口：每个方法对应契约里的一个端点，名字与
 * 契约路径同构。条件写方法**必须**显式接收 `ifMatch`——不给默认值，避免页面
 * 忘记传 ETag 时静默发出无条件写。
 * AdminApi maps one method per contract endpoint. Conditional writes take an
 * explicit ifMatch with no default, so an unconditional write cannot slip through.
 */
export class AdminApi {
  readonly #client: ApiClient

  constructor(client: ApiClient) {
    this.#client = client
  }

  // --- Agent 写面 ---------------------------------------------------------

  /** 模板创建（先验后写；失败零残余），返回落盘后的真值。 */
  createAgent(body: AgentCreateRequest): Promise<AgentFilesView> {
    return this.#client.request<AgentFilesView>('POST', '/agents', { body })
  }

  /**
   * 作者模型起草：草稿不落盘、不注册；无 ETag。信息不足时返回 `questions`
   * （文件为空），页面逐项回答后带 `answers` 再调一次；答案不持久化。
   */
  generateAgentFiles(
    name: string,
    description: string,
    answers: readonly AgentDraftAnswer[] = [],
  ): Promise<AgentDraftView> {
    const body: { description: string; answers?: readonly AgentDraftAnswer[] } = { description }
    if (answers.length > 0) {
      body.answers = answers
    }
    return this.#client.request<AgentDraftView>(
      'POST',
      `/agents/${encodeURIComponent(name)}/generate-files`,
      { body },
    )
  }

  /**
   * 一句话生成定时候选（未保存）：候选字段与未来三次触发都由服务端给出，
   * 页面不自行推导 cron 时间；确认后仍走既有的 schedules 条件写。
   */
  draftSchedule(name: string, instruction: string): Promise<ScheduleDraftView> {
    return this.#client.request<ScheduleDraftView>(
      'POST',
      `/agents/${encodeURIComponent(name)}/schedule-draft`,
      { body: { instruction } },
    )
  }

  /** 读受管文件集合（清单 + 集合 ETag）；响应头与视图的 etag 同值。 */
  getAgentFiles(name: string): Promise<AgentFilesView> {
    return this.#client.request<AgentFilesView>(
      'GET',
      `/agents/${encodeURIComponent(name)}/files`,
    )
  }

  /** 提交多文件：If-Match 必须是集合 ETag；返回重新读取的落盘真值。 */
  submitAgentFiles(
    name: string,
    files: FileDraft[],
    ifMatch: string,
  ): Promise<AgentFilesView> {
    return this.#client.request<AgentFilesView>(
      'POST',
      `/agents/${encodeURIComponent(name)}/files`,
      { body: { files }, ifMatch },
    )
  }

  /**
   * 读结构化配置（表单视图）。响应头 ETag 与视图的 `content_etag` 同值——
   * 表单保存与原文保存共用这一个条件写基准。
   */
  getAgentConfig(name: string): Promise<DetailWithEtag<AgentConfigView>> {
    return this.#readDetail<AgentConfigView>(`/agents/${encodeURIComponent(name)}/config`)
  }

  /**
   * 复制 Agent。`mapping` 只填源专属资源的显式映射；缺映射的专属引用由服务端
   * 409 `AGENT_COPY_BLOCKED` 拒绝，页面不自行补齐。
   */
  copyAgent(
    name: string,
    newName: string,
    mapping?: ResourceMapping[],
  ): Promise<AgentFilesView> {
    const body: AgentCopyRequest =
      mapping === undefined || mapping.length === 0
        ? { new_name: newName }
        : { new_name: newName, resource_mapping: mapping }
    return this.#client.request<AgentFilesView>(
      'POST',
      `/agents/${encodeURIComponent(name)}/copy`,
      { body },
    )
  }

  /** 归档删除（会话 / 记忆 / 审计不动）；无 If-Match。 */
  archiveAgent(name: string): Promise<AgentArchiveView> {
    return this.#client.request<AgentArchiveView>(
      'DELETE',
      `/agents/${encodeURIComponent(name)}`,
    )
  }

  listArchivedAgents(): Promise<ArchivedAgentView[]> {
    return this.#client.request<ArchivedAgentView[]>('GET', '/archived-agents')
  }

  /** 恢复：new_name 留空 = 原名恢复（条件迁移 archived → disabled，generation 递增）。 */
  restoreArchivedAgent(archiveId: string, newName?: string): Promise<AgentFilesView> {
    const body: AgentRestoreRequest = newName === undefined ? {} : { new_name: newName }
    return this.#client.request<AgentFilesView>(
      'POST',
      `/archived-agents/${encodeURIComponent(archiveId)}/restore`,
      { body },
    )
  }

  // --- 模型连接 -----------------------------------------------------------

  createModelConnection(body: ModelConnectionUpsert): Promise<ModelConnectionView> {
    return this.#client.request<ModelConnectionView>('POST', '/model-connections', { body })
  }

  /** 详情 GET 的 ETag 只在响应头：条件写前必须先取它。 */
  getModelConnection(name: string): Promise<DetailWithEtag<ModelConnectionView>> {
    return this.#readDetail<ModelConnectionView>(
      `/model-connections/${encodeURIComponent(name)}`,
    )
  }

  updateModelConnection(
    name: string,
    body: ModelConnectionUpsert,
    ifMatch: string,
  ): Promise<ModelConnectionView> {
    return this.#client.request<ModelConnectionView>(
      'PUT',
      `/model-connections/${encodeURIComponent(name)}`,
      { body, ifMatch },
    )
  }

  /**
   * 删除：`If-Match` 之外**必须**给出与当前指针一致的 `resource_uid`（契约
   * ResourceUID 参数）——同名重建后页面持有的旧身份会被 409 拒绝，而不是删错目标。
   */
  deleteModelConnection(name: string, resourceUid: string, ifMatch: string): Promise<void> {
    return this.#client.requestOK('DELETE', `/model-connections/${encodeURIComponent(name)}`, {
      query: { resource_uid: resourceUid },
      ifMatch,
    })
  }

  /**
   * 撤销托管凭据（全部版本）；重新录入是新版本，旧版本永不复活。
   * `kind=mcp_server` 时必须带 `binding_key`：只撤销那一个占位符，并按失效态回收该
   * server 的连接——不等下一次 apply。
   * MCP revocation is per slot and reclaims the live connection immediately.
   */
  revokeCredential(
    kind: CredentialOwnerKind,
    name: string,
    body: CredentialRevokeRequest,
    ifMatch: string,
  ): Promise<CredentialRevokeResult> {
    return this.#client.request<CredentialRevokeResult>(
      'POST',
      `/resources/${kind}/${encodeURIComponent(name)}/credential/revoke`,
      { body, ifMatch },
    )
  }

  /** 切换使用范围：shared ⇄ exclusive；被引用时 409 点名，不提供 force。 */
  setResourceScope(
    kind: CredentialResourceKind,
    name: string,
    body: ResourceScopeRequest,
    ifMatch: string,
  ): Promise<ResourceScopeResult> {
    return this.#client.request<ResourceScopeResult>(
      'POST',
      `/resources/${kind}/${encodeURIComponent(name)}/scope`,
      { body, ifMatch },
    )
  }

  testModelConnection(name: string): Promise<ModelConnectionTestResult> {
    return this.#client.request<ModelConnectionTestResult>(
      'POST',
      `/model-connections/${encodeURIComponent(name)}/test`,
    )
  }

  // --- MCP server ---------------------------------------------------------

  createMcpServer(body: McpServerUpsert): Promise<McpServerView> {
    return this.#client.request<McpServerView>('POST', '/mcp-servers', { body })
  }

  getMcpServer(name: string): Promise<DetailWithEtag<McpServerDetail>> {
    return this.#readDetail<McpServerDetail>(`/mcp-servers/${encodeURIComponent(name)}`)
  }

  updateMcpServer(
    name: string,
    body: McpServerUpsert,
    ifMatch: string,
  ): Promise<McpServerView> {
    return this.#client.request<McpServerView>(
      'PUT',
      `/mcp-servers/${encodeURIComponent(name)}`,
      { body, ifMatch },
    )
  }

  deleteMcpServer(name: string, resourceUid: string, ifMatch: string): Promise<void> {
    return this.#client.requestOK('DELETE', `/mcp-servers/${encodeURIComponent(name)}`, {
      query: { resource_uid: resourceUid },
      ifMatch,
    })
  }

  testMcpServer(name: string): Promise<McpServerTestResult> {
    return this.#client.request<McpServerTestResult>(
      'POST',
      `/mcp-servers/${encodeURIComponent(name)}/test`,
    )
  }

  /**
   * 受控应用：把已发布版本切成运行生效版本，只重建这一个 server。
   * `If-Match` 必填（缺失 / 弱 ETag / `*` → 412 零写入零操作行）；`versionId`
   * 省略 = 当前 `published_version_id`。保存、测试、应用是三件不同的事，互不代言。
   * Conditional apply of the published version to the running connection.
   */
  applyMcpServer(
    name: string,
    ifMatch: string,
    versionId = 0,
  ): Promise<McpServerApplyResult> {
    const body: McpServerApplyRequest = { version_id: versionId }
    return this.#client.request<McpServerApplyResult>(
      'POST',
      `/mcp-servers/${encodeURIComponent(name)}/apply`,
      { body, ifMatch },
    )
  }

  // --- 技能 ---------------------------------------------------------------

  createSkill(body: SkillCreate): Promise<SkillView> {
    return this.#client.request<SkillView>('POST', '/skills', { body })
  }

  importSkill(body: SkillImport): Promise<SkillView> {
    return this.#client.request<SkillView>('POST', '/skills/import', { body })
  }

  getSkill(name: string): Promise<DetailWithEtag<SkillDetail>> {
    return this.#readDetail<SkillDetail>(`/skills/${encodeURIComponent(name)}`)
  }

  updateSkill(name: string, files: FileDraft[], ifMatch: string): Promise<SkillDetail> {
    const body: SkillUpdate = { files }
    return this.#client.request<SkillDetail>('PUT', `/skills/${encodeURIComponent(name)}`, {
      body,
      ifMatch,
    })
  }

  deleteSkill(name: string, resourceUid: string, ifMatch: string): Promise<void> {
    return this.#client.requestOK('DELETE', `/skills/${encodeURIComponent(name)}`, {
      query: { resource_uid: resourceUid },
      ifMatch,
    })
  }

  /** 校验草稿：不落盘、不写操作行、不执行脚本。 */
  validateSkill(name: string, files: FileDraft[]): Promise<SkillValidateResult> {
    const body: SkillUpdate = { files }
    return this.#client.request<SkillValidateResult>(
      'POST',
      `/skills/${encodeURIComponent(name)}/validate`,
      { body },
    )
  }

  // --- 版本 / 发布 / 引用 -------------------------------------------------

  listResourceVersions(
    kind: AdminResourceKind,
    id: string,
  ): Promise<ResourceVersionView[]> {
    return this.#client.request<ResourceVersionView[]>(
      'GET',
      `/resources/${kind}/${encodeURIComponent(id)}/versions`,
    )
  }

  getResourceVersion(versionId: number): Promise<ResourceVersionDetail> {
    return this.#client.request<ResourceVersionDetail>('GET', `/resource-versions/${versionId}`)
  }

  /** 发布：version_id 缺省 = 最新版本；给旧版本则新建版本再发布。 */
  publishResource(
    kind: AdminResourceKind,
    id: string,
    ifMatch: string,
    versionId?: number,
  ): Promise<ResourcePublishResult> {
    const body: PublishRequest = versionId === undefined ? {} : { version_id: versionId }
    return this.#client.request<ResourcePublishResult>(
      'POST',
      `/resources/${kind}/${encodeURIComponent(id)}/publish`,
      { body, ifMatch },
    )
  }

  getResourceReferences(
    kind: AdminResourceKind,
    id: string,
  ): Promise<ResourceReferencesView> {
    return this.#client.request<ResourceReferencesView>(
      'GET',
      `/resources/${kind}/${encodeURIComponent(id)}/references`,
    )
  }

  // --- 通知渠道 -----------------------------------------------------------

  /**
   * 类型下拉的数据源：写侧校验与 `GET /notify-channels/types` 共用装配层的
   * 同一有序切片（约束 39）；页面不自行列举类型。
   * Ordered type list shared with the write-side validation; the page never
   * enumerates types on its own.
   */
  listNotifyTypes(): Promise<string[]> {
    return this.#client.request<string[]>('GET', '/notify-channels/types')
  }

  createNotifyChannel(body: NotifyChannelUpsert): Promise<NotifyChannelView> {
    return this.#client.request<NotifyChannelView>('POST', '/notify-channels', { body })
  }

  updateNotifyChannel(name: string, body: NotifyChannelUpsert): Promise<NotifyChannelView> {
    return this.#client.request<NotifyChannelView>(
      'PUT',
      `/notify-channels/${encodeURIComponent(name)}`,
      { body },
    )
  }

  /**
   * 连通性测试：真实发出一条固定文案 `Basalt 连通性测试`（接收方一眼可区分测试与
   * 真实通知）。三值结论都回 200，`ok=false` 是测试失败而不是接口错误；地址完全
   * 未配置是 503 `NOT_CONFIGURED`，**不得**当作「已验证」。
   */
  testNotifyChannel(name: string): Promise<NotifyChannelTestResult> {
    return this.#client.request<NotifyChannelTestResult>(
      'POST',
      `/notify-channels/${encodeURIComponent(name)}/test`,
    )
  }

  /**
   * 删除：契约只声明 `resource_uid`（必需）与 `force`，**没有** If-Match——渠道
   * 行没有资源条件 ETag。`resource_uid` 仍必须等于当前指针，否则 409。
   * `force` 只在页面二次确认后传 true（契约只认字面 true）。
   */
  deleteNotifyChannel(name: string, resourceUid: string, force = false): Promise<void> {
    const query: Record<string, string> = { resource_uid: resourceUid }
    if (force) {
      query.force = 'true'
    }
    return this.#client.requestOK('DELETE', `/notify-channels/${encodeURIComponent(name)}`, {
      query,
    })
  }

  // --- 工作区（只读）------------------------------------------------------

  getWorkspaceTree(path?: string): Promise<WorkspaceEntry[]> {
    return this.#client.request<WorkspaceEntry[]>('GET', '/workspace/tree', {
      query: path === undefined || path === '' ? undefined : { path },
    })
  }

  getWorkspaceFile(path: string): Promise<WorkspaceFile> {
    return this.#client.request<WorkspaceFile>('GET', '/workspace/file', {
      query: { path },
    })
  }

  // --- 执行环境：网络域名白名单 -------------------------------------------

  /**
   * 保存网络域名白名单（全量替换，保存即生效、不需要重启）。条件写必须带**当前
   * 生效**的 ETag；成功后服务端返回重新读取的 SandboxView（不是请求体回显），
   * 页面按返回值回显，不做乐观更新。`domains: []` 是合法的"全部拒绝"。
   */
  updateNetworkDomains(domains: readonly string[], ifMatch: string): Promise<SandboxView> {
    return this.#client.request<SandboxView>('PUT', '/sandbox/network-domains', {
      body: { domains },
      ifMatch,
    })
  }

  /**
   * 保存禁止域名表（全量替换，两档都先生效）。与白名单同构：条件写必须带禁止表
   * 当前生效的 ETag；成功返回重新读取的 SandboxView；`domains: []` 表示没有禁止条目。
   */
  updateDeniedDomains(domains: readonly string[], ifMatch: string): Promise<SandboxView> {
    return this.#client.request<SandboxView>('PUT', '/sandbox/denied-domains', {
      body: { domains },
      ifMatch,
    })
  }

  /**
   * 切换执行档位：`If-Match` 之外必须给出渲染确认框时看到的档位（`expectedProfile`）。
   * 库内档位与之不一致 → 409 `EXECUTION_PROFILE_MISMATCH`，零写入；页面不自动重试。
   * The confirmed direction is part of the write, so a stale dialog cannot be replayed.
   */
  updateExecutionPolicy(
    profile: ExecutionProfile,
    expectedProfile: ExecutionProfile,
    ifMatch: string,
  ): Promise<ExecutionPolicyView> {
    return this.#client.request<ExecutionPolicyView>('PUT', '/execution-policy', {
      body: { profile, expected_profile: expectedProfile },
      ifMatch,
    })
  }

  /**
   * 记录一条待确认项的「保持拒绝」。它不改授权结论、不需要 If-Match；放行动作走
   * `createNetworkException`，一次写入只表达一个目的。
   */
  confirmPolicyDenial(id: string): Promise<PolicyConfirmationResult> {
    return this.#client.request<PolicyConfirmationResult>(
      'POST',
      '/execution-policy/confirmations',
      { body: { id, decision: 'keep_denied' } },
    )
  }

  /** 新增内网访问例外：理由必填；重复登记 → 409（不覆盖既有理由）。 */
  createNetworkException(body: NetworkExceptionCreate): Promise<NetworkExceptionCreateResult> {
    return this.#client.request<NetworkExceptionCreateResult>('POST', '/network-exceptions', {
      body,
    })
  }

  /**
   * 删除例外：`If-Match` 必须是 `GET /network-exceptions` 的集合 ETag——保证删的是
   * 页面看过的那一份集合；陈旧 ETag → 412 零写入。
   */
  deleteNetworkException(
    exceptionId: string,
    ifMatch: string,
  ): Promise<NetworkExceptionDeleteResult> {
    return this.#client.request<NetworkExceptionDeleteResult>(
      'DELETE',
      `/network-exceptions/${encodeURIComponent(exceptionId)}`,
      { ifMatch },
    )
  }

  /**
   * 显式初始化实例主密钥（14D）：只在 `uninitialized` 且可初始化时成功，`confirm`
   * 必须字面为 true。已就绪 → 409 `CONFLICT`（**不覆盖**）；恢复态 → 409
   * `SECRETS_RECOVERY_REQUIRED`（**不生成替代密钥**）；两种情况都零写入。
   * Explicit instance-key init; ready and recovery states are refused with zero writes.
   */
  initInstanceSecrets(): Promise<InstanceSecretsView> {
    return this.#client.request<InstanceSecretsView>('POST', '/instance/secrets/init', {
      body: { confirm: true },
    })
  }

  // --- 内部实现 -----------------------------------------------------------

  /** 详情 GET：视图放 data，响应头 ETag 单独交给条件写。 */
  async #readDetail<T>(path: string): Promise<DetailWithEtag<T>> {
    const { data, etag } = await this.#client.requestWithEtag<T>('GET', path)
    return { data, etag }
  }
}

/** 视图侧便捷构造：一个视图共享一个 ApiClient，不另建连接。 */
export function adminApi(client: ApiClient): AdminApi {
  return new AdminApi(client)
}

/**
 * 失败信封里的点名列表（契约错误 data 白名单的 `problems`）。没有名单时返回空
 * 数组；调用方仍要展示 `errorMessage(error)`——problems 是补充，不替代固定文案。
 * Reads the contract's `problems` list from a failure envelope; the fixed message
 * stays the primary text.
 */
export function problemsOf(error: unknown): string[] {
  if (!(error instanceof ApiError) || error.data === null) {
    return []
  }
  return error.data.problems ?? []
}
