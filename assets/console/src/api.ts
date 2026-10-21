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

export interface AgentLifecycleView {
  agent: string
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
  description: string
  provider: string
  model: string
  tools: string[]
  skills: string[]
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
  identity?: { agent_name?: string; prompt?: string }
  provider?: { name?: string; model?: string; temperature?: number }
  tools?: string[]
  skills?: string[]
  mcp_servers?: string[]
  bootstrap?: string[]
  settings?: { max_iterations?: number; max_history_turns?: number }
  schedules?: Record<string, unknown>[]
}

/** 整份替换或结构化补丁（二者互斥，契约 AgentContentUpdate）。 */
export type AgentContentUpdate = { content: string } | { changes: AgentContentChanges }

export interface AgentFileEntry {
  path: string
  size: number
}

export interface AgentFilesView {
  agent: string
  lifecycle: AgentLifecycleView
  etag: string
  files: AgentFileEntry[]
}

export interface AgentLifecycleTransition {
  expected_state: 'active' | 'disabled'
  expected_generation: number
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
  type: string
  description: string
  url_host: string
}

export interface ScheduleView {
  id: string
  agent: string
  cron: string
  zone: string
  message: string
  enabled: boolean
  last_run_at: string
  last_status: string
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

export interface McpServerView {
  name: string
  transport: 'stdio' | 'http'
  /** 仅可执行名；参数不展示。 */
  command: string
  /** 只回变量名，不返回值。 */
  env_keys: string[]
  connected: boolean
  tools: string[]
  published_version_id: number
  published_version_no: number
  applied_version_id: number
  applied_version_no: number
  pending_restart: boolean
}

export interface ModelConnectionView {
  name: string
  base_url: string
  /** 只回 `${ENV_VAR}` 引用名；未配置为空串。 */
  api_key_env: string
  default_model: string
  published_version_id: number
  published_version_no: number
  applied_version_id: number
  applied_version_no: number
  etag: string
  referenced_by: string[]
}

export interface SandboxView {
  file_allowed_paths: string[]
  shell_allowed_commands: string[]
  shell_interpreters: string[]
  http_allowed_domains: string[]
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
 * ApiError 保留 HTTP 状态、固定错误码与 Retry-After 秒数；message 直接用服务端
 * 固定文案（契约保证不含路径 / 凭证）。
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly retryAfterSeconds: number | null

  constructor(status: number, code: string, message: string, retryAfterSeconds: number | null = null) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.retryAfterSeconds = retryAfterSeconds
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

type QueryValues = Record<string, string | number | undefined>

interface RequestOptions {
  query?: QueryValues
  body?: unknown
  ifMatch?: string
  signal?: AbortSignal
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
      throw new ApiError(response.status, envelope.code, envelope.message, retryAfter)
    }
    if (!('data' in envelope) || envelope.data === undefined) {
      throw new ApiError(0, 'PROTOCOL', '响应缺少 data 载荷')
    }
    return { data: envelope.data as T, code: envelope.code, message: envelope.message }
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
