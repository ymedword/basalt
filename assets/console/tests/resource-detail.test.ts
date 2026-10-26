// 资源详情页（R4 追加部分）回归：范围与托管凭据卡片、显式 UID + If-Match 的
// 范围切换与撤销、共享转专属被引用时的点名与专属文案、撤销必须经过确认、
// 引用卡片展示 UID 绑定与“引用未记录”证据缺口。
// Resource detail regressions for the R4 cards: scope and managed credential,
// UID + If-Match on both writes, the pointed denial for a blocked scope switch,
// a confirmation step before revoking, and UID-based reference evidence gaps.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type {
  ModelConnectionView,
  OperationDetail,
  OperationView,
  ProfileView,
} from '../src/api'
import type {
  McpServerDetail,
  ResourceReferencesView,
  ResourceVersionView,
} from '../src/api-admin'
import { createConsoleApp } from '../src/app'

const CONFIG_ETAG = `"sha256:${'a'.repeat(64)}"`
const NEXT_ETAG = `"sha256:${'b'.repeat(64)}"`
const UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'
const AGENT_UID = 'ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'

function connectionView(overrides: Partial<ModelConnectionView> = {}): ModelConnectionView {
  return {
    name: 'deepseek',
    resource_uid: UID,
    base_url: 'https://api.deepseek.com',
    api_key_env: '',
    default_model: 'deepseek-chat',
    credential_mode: 'managed',
    credential_ref: 'cred-01J8Z6YQ4M2S3T7V9W0X1Y2Z3A',
    credential_state: 'active',
    credential_version: 2,
    last4: '9f2c',
    scope: 'exclusive',
    owner_agent: 'writer',
    revision: 3,
    published_version_id: 2,
    published_version_no: 2,
    applied_version_id: 2,
    applied_version_no: 2,
    etag: CONFIG_ETAG,
    referenced_by: ['writer'],
    availability: 'ready',
    last_test: null,
    ...overrides,
  }
}

function referencesView(overrides: Partial<ResourceReferencesView> = {}): ResourceReferencesView {
  return {
    kind: 'model_connection',
    id: 'deepseek',
    resource_uid: UID,
    published_version_id: 2,
    agents: ['writer', 'legacy'],
    bound_agents: [
      { agent: 'writer', agent_uid: AGENT_UID, archived: false },
      { agent: 'legacy', agent_uid: '', archived: true },
    ],
    active_runs: [],
    ...overrides,
  }
}

function versionView(): ResourceVersionView {
  return {
    version_id: 2,
    resource_kind: 'model_connection',
    resource_id: 'deepseek',
    resource_uid: UID,
    version_no: 2,
    content_hash: `sha256:${'e'.repeat(64)}`,
    created_at: '2026-10-23T08:00:00Z',
    operation_id: 'op-1',
    published: true,
    applied: true,
    reverted_from_version_id: null,
  }
}

function profile(name: string): ProfileView {
  return {
    name,
    display_name: '',
    description: '',
    provider: 'deepseek',
    model: 'deepseek-chat',
    tools: [],
    skills: [],
    notify_channels: [],
    schedules: 0,
    lifecycle: {
      agent: name,
      agent_uid: AGENT_UID,
      uid_source: 'state_file',
      state: 'active',
      generation: 1,
      updated_at: '2026-10-23T08:00:00Z',
      operation_id: 'op-1',
      valid: true,
      lifecycle_error: '',
    },
    registered: true,
    runnable: true,
    problems: [],
  }
}

interface RecordedCall {
  readonly method: string
  readonly path: string
  readonly body: unknown
  readonly ifMatch: string | null
}

interface Reply {
  status?: number
  code?: string
  message?: string
  data?: unknown
  etag?: string
}

let calls: RecordedCall[] = []
let wrapper: ReturnType<typeof mount> | undefined

function setupFetch(reply: (call: RecordedCall) => Reply = () => ({})): void {
  calls = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const headers = new Headers(init?.headers ?? {})
      const call: RecordedCall = {
        method: init?.method ?? 'GET',
        path: url.pathname + url.search,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
        ifMatch: headers.get('If-Match'),
      }
      calls.push(call)
      const result = reply(call)
      const status = result.status ?? 200
      const responseHeaders = new Headers()
      if (result.etag !== undefined) {
        responseHeaders.set('ETag', result.etag)
      }
      return {
        ok: status >= 200 && status < 300,
        status,
        headers: responseHeaders,
        json: async () => ({
          code: result.code ?? 'OK',
          message: result.message ?? '',
          data: result.data ?? null,
          timestamp: '2026-10-23T00:00:00Z',
        }),
      } as Response
    }),
  )
}

/** 登录后直达模型连接详情，避开总览页的旁路请求。 */
async function openDetail(): Promise<void> {
  window.history.replaceState(
    null,
    '',
    `/console/login?next=${encodeURIComponent('/console/resources/model-connections/deepseek')}`,
  )
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

function clickButton(label: string): Promise<void> {
  const button = wrapper!.findAll('button').find((item) => item.text() === label)
  expect(button, label).toBeDefined()
  return button!.trigger('click')
}

function writeCalls(fragment: string): RecordedCall[] {
  return calls.filter((call) => call.method === 'POST' && call.path.includes(fragment))
}

/** 页面上所有结论徽章的文本；禁用词只判徽章，不判解释性句子。 */
function badgeTexts(): string[] {
  return wrapper!.findAll('.badge').map((item) => item.text())
}

/** 定义列表里某个术语对应的值（dt / dd 成对且按文档顺序对齐）。 */
function definitionValue(term: string): string {
  const terms = wrapper!.findAll('dt')
  const values = wrapper!.findAll('dd')
  const index = terms.findIndex((item) => item.text() === term)
  expect(index, `找不到定义项「${term}」`).toBeGreaterThanOrEqual(0)
  return values[index]!.text()
}

/** 写入之后是否重新读取了详情；资源表单自身也读详情，所以不按调用次数判定。 */
function rereadAfterLastWrite(): boolean {
  let lastWrite = -1
  calls.forEach((call, index) => {
    if (call.method === 'POST' || call.method === 'PUT') {
      lastWrite = index
    }
  })
  return (
    lastWrite >= 0 &&
    calls
      .slice(lastWrite + 1)
      .some((call) => call.method === 'GET' && call.path === '/api/v1/model-connections/deepseek')
  )
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

describe('resource detail scope and credential cards', () => {
  it('renders scope, managed credential and UID-based reference evidence', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        return { data: connectionView(), etag: CONFIG_ETAG }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [versionView()] }
      }
      return {}
    })
    await openDetail()

    expect(wrapper!.text()).toContain('专属（只给一个 Agent）')
    expect(wrapper!.text()).toContain('writer')
    expect(wrapper!.text()).toContain('加密托管的凭据')
    expect(wrapper!.text()).toContain('有效')
    expect(wrapper!.text()).toContain('版本v2')
    expect(wrapper!.text()).toContain('尾号····9f2c')
    // 迁移前没有 UID 证据的旧状态文件如实标注，不补造身份。
    expect(wrapper!.text()).toContain('引用未记录（迁移前无法证明身份）')
    expect(wrapper!.text()).toContain('已归档')
    expect(wrapper!.text()).toContain(UID)
  })

  it('switches scope with the resource UID and If-Match, then re-reads', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        return { data: connectionView(), etag: CONFIG_ETAG }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [versionView()] }
      }
      if (call.path === '/api/v1/profiles') {
        return { data: [profile('writer'), profile('reader')] }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/scope') {
        return {
          data: {
            resource_uid: UID,
            scope: 'shared',
            owner_agent: '',
            revision: 4,
            etag: NEXT_ETAG,
            affected_eligible_agents: ['reader'],
          },
        }
      }
      return {}
    })
    await openDetail()

    await clickButton('切换范围')
    await wrapper!.find('#resource-scope-shared').setValue()
    await clickButton('确认切换')
    await flushPromises()

    const posts = writeCalls('/scope')
    expect(posts).toHaveLength(1)
    expect(posts[0]!.ifMatch).toBe(CONFIG_ETAG)
    expect(posts[0]!.body).toEqual({ resource_uid: UID, scope: 'shared' })
    expect(wrapper!.text()).toContain('已切换为共享')
    expect(wrapper!.text()).toContain('reader')
    // 写后重新读取：页面不以范围切换响应做乐观更新。
    expect(rereadAfterLastWrite()).toBe(true)
  })

  it('refuses an exclusive scope without an owner before sending anything', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        return { data: connectionView({ scope: 'shared', owner_agent: '' }), etag: CONFIG_ETAG }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [] }
      }
      if (call.path === '/api/v1/profiles') {
        return { data: [profile('writer')] }
      }
      return {}
    })
    await openDetail()

    await clickButton('切换范围')
    await wrapper!.find('#resource-scope-exclusive').setValue()
    await clickButton('确认切换')
    await flushPromises()

    expect(writeCalls('/scope')).toHaveLength(0)
    expect(wrapper!.text()).toContain('转专属时必须选择所有者 Agent')
  })

  it('reports a blocked scope switch with the pointed agents and scope wording', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        return { data: connectionView({ scope: 'shared', owner_agent: '' }), etag: CONFIG_ETAG }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [] }
      }
      if (call.path === '/api/v1/profiles') {
        return { data: [profile('writer')] }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/scope') {
        return {
          status: 409,
          code: 'RESOURCE_SCOPE_IN_USE',
          message: '资源仍被其他 Agent 或运行引用',
          data: { agents: ['reader'], active_runs: ['run-1'] },
        }
      }
      return {}
    })
    await openDetail()

    await clickButton('切换范围')
    await wrapper!.find('#resource-scope-exclusive').setValue()
    await wrapper!.find('#resource-scope-owner').setValue('writer')
    await clickButton('确认切换')
    await flushPromises()

    expect(wrapper!.text()).toContain('切换范围被拒绝：仍有配置引用或运行占用')
    expect(wrapper!.text()).toContain('Agent 配置引用：reader')
    expect(wrapper!.text()).toContain('运行占用：run-1')
    expect(wrapper!.text()).toContain('不提供强制选项')
    // 错误文案来自共享码表：给出下一步，而不是只重复服务端短语。
    expect(wrapper!.text()).toContain('先解除，再切换为专属')
  })

  it('requires an explicit confirmation before revoking the managed credential', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        return { data: connectionView(), etag: CONFIG_ETAG }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [] }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/credential/revoke') {
        return {
          data: {
            resource_uid: UID,
            credential_ref: 'cred-01J8Z6YQ4M2S3T7V9W0X1Y2Z3A',
            credential_state: 'revoked',
            revoked_versions: 2,
            revocation_epoch: 3,
          },
        }
      }
      return {}
    })
    await openDetail()

    await clickButton('撤销凭据')
    await flushPromises()
    // 打开确认框本身不发请求。
    expect(writeCalls('/credential/revoke')).toHaveLength(0)
    expect(wrapper!.text()).toContain('旧版本永不复活')

    const confirm = wrapper!.findAll('.dialog-actions button').find((item) => item.text() === '撤销凭据')
    expect(confirm).toBeDefined()
    await confirm!.trigger('click')
    await flushPromises()

    const posts = writeCalls('/credential/revoke')
    expect(posts).toHaveLength(1)
    expect(posts[0]!.ifMatch).toBe(CONFIG_ETAG)
    expect(posts[0]!.body).toEqual({ resource_uid: UID, confirm: true })
    expect(wrapper!.text()).toContain('已撤销 2 个凭据版本')
    expect(rereadAfterLastWrite()).toBe(true)
  })

  it('offers no revoke action for env-mode connections', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        return {
          data: connectionView({
            api_key_env: '${DEEPSEEK_API_KEY}',
            credential_mode: 'env',
            credential_ref: '',
            credential_state: '',
            credential_version: 0,
            last4: '',
          }),
          etag: CONFIG_ETAG,
        }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [] }
      }
      return {}
    })
    await openDetail()

    expect(wrapper!.text()).toContain('没有可撤销的加密凭据')
    expect(wrapper!.findAll('button').some((item) => item.text() === '撤销凭据')).toBe(false)
  })

  // R7：本地配置就绪与真实实测分两行显示——配置就绪不等于已实测，失败也不会
  // 反过来把可用性改成别的值，两者互不冒充。
  // R7: local readiness and the recorded test are separate rows and never stand in
  // for each other.
  it('keeps local readiness separate from a never-tested record', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        return { data: connectionView(), etag: CONFIG_ETAG }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [] }
      }
      return {}
    })
    await openDetail()

    expect(definitionValue('本地配置就绪')).toBe('配置就绪')
    expect(definitionValue('最近实测')).toBe('从未测试（配置就绪不代表已实测）')
    expect(wrapper!.text()).toContain('「本地配置就绪」只看本机配置与凭据状态')
    expect(wrapper!.text()).toContain('配置就绪不等于已实测可用')
  })

  it('shows a recorded failed test without turning it into a success', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        return {
          data: connectionView({
            last_test: {
              status: 'failed',
              tested_at: '2026-10-23T08:00:00Z',
              detail: 'HTTP 401 Unauthorized',
            },
          }),
          etag: CONFIG_ETAG,
        }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [] }
      }
      return {}
    })
    await openDetail()

    const lastTest = definitionValue('最近实测')
    expect(lastTest).toContain('失败')
    expect(lastTest).toContain('HTTP 401 Unauthorized')
    expect(lastTest).not.toContain('成功')
    // 实测失败不改变「本地配置就绪」这个独立读数。
    expect(definitionValue('本地配置就绪')).toBe('配置就绪')
  })

  it('re-reads the summary after a test instead of patching a local record', async () => {
    let detailReads = 0
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.path === '/api/v1/model-connections/deepseek') {
        detailReads += 1
        return {
          data:
            detailReads === 1
              ? connectionView()
              : connectionView({
                  last_test: {
                    status: 'success',
                    tested_at: '2026-10-23T08:05:00Z',
                    detail: '',
                  },
                }),
          etag: CONFIG_ETAG,
        }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/references') {
        return { data: referencesView() }
      }
      if (call.path === '/api/v1/resources/model_connection/deepseek/versions') {
        return { data: [] }
      }
      if (call.path === '/api/v1/model-connections/deepseek/test') {
        return {
          data: { ok: true, latency_ms: 12, model: 'deepseek-chat', error_code: '', error_message: '' },
        }
      }
      return {}
    })
    await openDetail()
    expect(definitionValue('最近实测')).toBe('从未测试（配置就绪不代表已实测）')

    await clickButton('测试连接')
    await flushPromises()

    expect(writeCalls('/model-connections/deepseek/test')).toHaveLength(1)
    // 明细由服务端重新读取：本地不拼一条测试结果。
    expect(rereadAfterLastWrite()).toBe(true)
    expect(definitionValue('最近实测')).toContain('成功')
    expect(definitionValue('最近实测')).not.toContain('从未测试')
  })
})

// --- US-14：MCP 详情四轴 ---------------------------------------------------
// 配置 / 连接 / 上次测试 / 依赖诊断四条轴各看各的证据；测试记录以管理操作台账
// 为准，历史记录缺字段或没有被测版本时不得推断通过。
// US-14 MCP axes: config / connection / last test / diagnosis stay separate; the
// operation ledger is authoritative and legacy records never imply a pass.
const OP_ID = 'op_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C'
const TESTED_AT = '2026-10-24T09:30:00Z'

function mcpServerView(overrides: Partial<McpServerDetail> = {}): McpServerDetail {
  return {
    name: 'files',
    transport: 'stdio',
    command: 'npx',
    args: [],
    url: '',
    env_keys: ['TOKEN'],
    header_keys: [],
    credential_keys: [{ key: 'TOKEN', mode: 'env', state: 'env', last4: '' }],
    connected: true,
    tools: ['read_file', 'write_file'],
    published_version_id: 2,
    published_version_no: 2,
    applied_version_id: 2,
    applied_version_no: 2,
    pending_restart: false,
    tool_disposition: {
      offered: 2,
      accepted: 1,
      skipped: [{ name: 'read_file', winner_source: 'builtin', reason: 'duplicate_name' }],
    },
    ...overrides,
  }
}

function mcpVersion(versionId: number, versionNo: number): ResourceVersionView {
  return {
    ...versionView(),
    version_id: versionId,
    version_no: versionNo,
    resource_kind: 'mcp_server',
    resource_id: 'files',
  }
}

function operationView(): OperationView {
  return {
    operation_id: OP_ID,
    action: 'mcp_server.test',
    target_kind: 'mcp_server',
    target_id: 'files',
    actor_subject: 'admin',
    auth_method: 'bearer',
    source: '127.0.0.1',
    status: 'success',
    result_code: 'OK',
    error_message: '',
    created_at: TESTED_AT,
    finished_at: TESTED_AT,
  }
}

const PASSING_DETAIL: Record<string, unknown> = {
  ok: true,
  latency_ms: 42,
  tools: ['read_file'],
  error_code: '',
  error_message: '',
  stage: 'complete',
  failure_class: '',
}

/** 操作详情缺省为一次针对 v2 的成功测试；`detail` 整体替换（不做字段合并）。 */
function operationDetailOf(overrides: Partial<OperationDetail> = {}): OperationDetail {
  return {
    ...operationView(),
    request_hash: `sha256:${'c'.repeat(64)}`,
    requested_version_id: 2,
    expected_etag: CONFIG_ETAG,
    detail: { ...PASSING_DETAIL },
    recovered_at: '',
    verification: 'not_required',
    ...overrides,
  }
}

function mcpDiagnosis(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    name: 'files',
    transport: 'stdio',
    published_version_id: 2,
    applied_version_id: 2,
    pending_restart: false,
    command: { state: 'located', path: 'npx', reason: 'lookpath' },
    env: [{ key: 'TOKEN', set: true }],
    // 默认命令不指向已登记运行时组件：依赖轴显示「不适用」，不是「缺失」。
    dependency: null,
    references: { count: 0, agents: [] },
    warnings: [],
    checked_at: TESTED_AT,
    ...overrides,
  }
}

interface McpRouteOverrides {
  readonly view?: Partial<McpServerDetail>
  readonly diagnosis?: Record<string, unknown>
  readonly operation?: Partial<OperationDetail>
  readonly versions?: ResourceVersionView[]
}

/** MCP 详情页的全部只读路由；未列出的路径按空对象返回。 */
function mcpRoutes(overrides: McpRouteOverrides = {}): (call: RecordedCall) => Reply {
  return (call) => {
    if (call.path.endsWith('/info')) {
      return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 1, providers: [] } }
    }
    if (call.path === '/api/v1/mcp-servers/files') {
      return { data: mcpServerView(overrides.view), etag: CONFIG_ETAG }
    }
    if (call.path === '/api/v1/resources/mcp_server/files/versions') {
      return { data: overrides.versions ?? [mcpVersion(2, 2), mcpVersion(3, 3)] }
    }
    if (call.path === '/api/v1/resources/mcp_server/files/references') {
      return {
        data: {
          kind: 'mcp_server',
          id: 'files',
          resource_uid: UID,
          published_version_id: 2,
          agents: [],
          bound_agents: [],
          active_runs: [],
        },
      }
    }
    if (call.path === '/api/v1/mcp-servers/files/diagnostics') {
      return { data: mcpDiagnosis(overrides.diagnosis) }
    }
    if (call.path.startsWith('/api/v1/operations?')) {
      return { data: [operationView()] }
    }
    if (call.path === `/api/v1/operations/${OP_ID}`) {
      return { data: operationDetailOf(overrides.operation) }
    }
    return {}
  }
}

/** 登录后直达 MCP 服务详情。 */
async function openMcpDetail(): Promise<void> {
  window.history.replaceState(
    null,
    '',
    `/console/login?next=${encodeURIComponent('/console/resources/mcp-servers/files')}`,
  )
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

describe('MCP detail axes', () => {
  it('renders config, connection, last test and diagnosis as separate lines', async () => {
    setupFetch(
      mcpRoutes({
        view: {
          connected: true,
          pending_restart: true,
          published_version_id: 3,
          published_version_no: 3,
          applied_version_id: 2,
          applied_version_no: 2,
        },
        diagnosis: { command: { state: 'unconfirmed', path: '', reason: 'script' } },
        operation: { requested_version_id: 3 },
      }),
    )
    await openMcpDetail()

    // 配置轴只看占位符来源与就绪：TOKEN 由环境变量提供且诊断读到已设置，判「完整」。
    // 发布已前移、运行仍是旧版本，是连接轴上的「待应用」，不是配置失败。
    expect(definitionValue('配置')).toContain('完整')
    expect(definitionValue('连接')).toContain('待应用')
    expect(definitionValue('连接')).toContain('运行 v2，发布 v3')
    expect(definitionValue('待重启')).toContain('需重启')
    // 测试轴：诊断 unconfirmed 不改变测试结论；通过针对的是发布版本 v3。
    expect(definitionValue('上次测试')).toContain('连接测试通过')
    expect(definitionValue('上次测试')).toContain('v3')
    // 依赖轴：null 只是「不适用」，命令三态另列；不能写成「依赖缺失」。
    expect(definitionValue('依赖')).toContain('不适用')
    expect(definitionValue('依赖')).not.toContain('依赖缺失')
    expect(definitionValue('依赖')).toContain('命令无法确认')
    // 工具来源与 tools 语义分开：tools 仍是 offered，跳过项点名赢家。
    expect(definitionValue('工具')).toContain('已提供 2，已注册 1')
    expect(definitionValue('工具')).toContain('read_file：被 builtin 抢先（重名）')
    // 引用轴与「引用与占用」同源：没有绑定 Agent 就不阻断删除。
    expect(definitionValue('引用')).toContain('无配置引用')
    // 操作详情可展开，且明确 success 不是测试结论。
    expect(wrapper!.text()).toContain(`管理操作详情（${OP_ID}）`)
    expect(wrapper!.text()).toContain('操作状态 success 只表示管理动作执行完毕')
    // 禁用词只约束结论徽章；解释性句子不在其列。
    expect(badgeTexts()).not.toContain('已验证')
  })

  it('keeps an unconfirmed diagnosis from predicting a failing test', async () => {
    setupFetch(
      mcpRoutes({
        diagnosis: { command: { state: 'unconfirmed', path: '', reason: 'script' } },
        operation: {
          detail: {
            ok: false,
            latency_ms: 5,
            tools: [],
            error_code: 'START_FAILED',
            error_message: '启动失败',
            stage: 'spawn',
            failure_class: 'command_not_found',
          },
        },
      }),
    )
    await openMcpDetail()

    const axis = definitionValue('上次测试')
    expect(axis).toContain('连接测试失败')
    expect(axis).toContain('启动 / 找不到启动程序')
    expect(axis).toContain('针对 v2')
    expect(definitionValue('依赖')).toContain('命令无法确认')
    expect(wrapper!.text()).toContain('失败时先看「依赖」轴')
    expect(wrapper!.text()).toContain('诊断结论不预判测试')
    expect(wrapper!.text()).toContain('找不到启动程序')
  })

  it('never infers a pass from a record without a tested version', async () => {
    setupFetch(mcpRoutes({ operation: { requested_version_id: 0 } }))
    await openMcpDetail()

    expect(definitionValue('上次测试')).toContain('历史记录无被测版本')
    expect(definitionValue('上次测试')).toContain('需重新测试')
    // 汇总行与徽章都不得出现“通过”；操作记录里的“不代表连接测试通过”是解释句。
    expect(definitionValue('上次测试')).not.toContain('连接测试通过')
    expect(badgeTexts()).not.toContain('连接测试通过')
    expect(wrapper!.text()).toContain('按未通过处理，请重新测试')
  })

  it('never infers a pass from a legacy record missing D-03 fields', async () => {
    setupFetch(
      mcpRoutes({
        operation: { detail: { ok: true, latency_ms: 9, tools: ['read_file'] } },
      }),
    )
    await openMcpDetail()

    expect(definitionValue('上次测试')).toContain('历史记录字段不全')
    expect(definitionValue('上次测试')).toContain('不推断通过')
    expect(wrapper!.text()).toContain('这条历史记录早于测试记录补强')
    expect(definitionValue('上次测试')).not.toContain('连接测试通过')
    expect(badgeTexts()).not.toContain('连接测试通过')
  })

  it('marks a passing test of an older version as stale, not as a current pass', async () => {
    setupFetch(
      mcpRoutes({
        view: { published_version_id: 3, published_version_no: 3 },
        operation: { requested_version_id: 2 },
      }),
    )
    await openMcpDetail()

    const axis = definitionValue('上次测试')
    expect(axis).toContain('通过（版本已过期）')
    expect(axis).toContain('测试针对 v2')
    expect(axis).toContain('当前发布 v3')
    expect(axis).toContain('需重测')
  })

  it('shows the diagnosis details without values and marks pending restart in its own row', async () => {
    setupFetch(
      mcpRoutes({
        diagnosis: {
          command: { state: 'not_found', path: '', reason: 'not_found' },
          env: [
            { key: 'TOKEN', set: false },
            { key: 'REGION', set: true },
          ],
          warnings: ['env_gate_process_local'],
        },
      }),
    )
    await openMcpDetail()

    const text = wrapper!.text()
    expect(text).toContain('命令未找到')
    expect(text).toContain('明确不存在')
    expect(text).toContain('TOKEN：未设置')
    expect(text).toContain('REGION：已设置')
    // 诊断只回布尔，不回值：未设置的键不得被渲染成“已设置”。
    expect(text).not.toContain('TOKEN：已设置')
    expect(text).toContain('env 闸门只核对本进程环境，不核对子进程实际环境')
    expect(text).toContain('诊断不启动进程、不连接、不写操作行')
  })

  it('does not render an unconfigured server as disconnected', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 1, providers: [] } }
      }
      if (call.path === '/api/v1/mcp-servers') {
        return {
          data: [
            mcpServerView({
              connected: false,
              published_version_id: 0,
              published_version_no: 0,
              applied_version_id: 0,
              applied_version_no: 0,
              tools: [],
              tool_disposition: null,
            }),
          ],
        }
      }
      return {}
    })
    window.history.replaceState(
      null,
      '',
      `/console/login?next=${encodeURIComponent('/console/resources/mcp-servers')}`,
    )
    wrapper = mount(createConsoleApp(), { attachTo: document.body })
    await flushPromises()
    await wrapper.find('#console-token').setValue('test-console-token')
    await wrapper.find('.login-form').trigger('submit')
    await flushPromises()

    const table = wrapper!.find('.mcp-table').text()
    expect(table).not.toContain('未连接')
    expect(table).toContain('—')
  })
})

describe('MCP credential slots and apply', () => {
  it('names every blocked slot on the config axis and never claims completeness', async () => {
    setupFetch(
      mcpRoutes({
        view: {
          env_keys: ['TOKEN', 'REGION'],
          header_keys: ['X-API-Key'],
          credential_keys: [
            { key: 'REGION', mode: 'managed', state: 'managed_revoked', last4: '12ab' },
            { key: 'TOKEN', mode: 'env', state: 'env', last4: '' },
            { key: 'X-API-Key', mode: 'env', state: 'env', last4: '' },
          ],
        },
        diagnosis: {
          env: [
            { key: 'TOKEN', set: false },
            { key: 'REGION', set: true },
          ],
        },
      }),
    )
    await openMcpDetail()

    const axis = definitionValue('配置')
    expect(axis).toContain('缺凭据')
    expect(axis).toContain('TOKEN（环境变量未设置）')
    expect(axis).toContain('REGION（托管已撤销）')
    // 请求头占位符不在 env 诊断里：不假装「完整」，指明以应用预检为准。
    expect(axis).toContain('请求头占位符的就绪以应用预检为准')
    expect(axis).not.toContain('完整')
  })

  it('treats a declared placeholder without a source choice as a validation failure', async () => {
    setupFetch(
      mcpRoutes({
        view: {
          env_keys: ['TOKEN', 'OLD'],
          credential_keys: [{ key: 'TOKEN', mode: 'env', state: 'env', last4: '' }],
        },
      }),
    )
    await openMcpDetail()

    // 未声明来源是配置错误，不是“没有凭据”：它比就绪判定更靠前，单独点名。
    const axis = definitionValue('配置')
    expect(axis).toContain('校验失败')
    expect(axis).toContain('未声明来源')
    expect(axis).toContain('OLD')
    expect(axis).not.toContain('完整')
    // 凭据卡片把未声明的槽位单列，而不是当作已声明但缺值。
    const table = wrapper!.find('.mcp-credentials-table').text()
    expect(table).toContain('OLD')
    expect(table).toContain('未声明来源')
  })

  it('lists each slot with source, state and last4, and revokes one placeholder at a time', async () => {
    setupFetch((call) => {
      if (
        call.method === 'POST' &&
        call.path === '/api/v1/resources/mcp_server/files/credential/revoke'
      ) {
        return {
          data: {
            resource_uid: UID,
            binding_key: 'TOKEN',
            credential_ref: 'cred-1',
            credential_state: 'revoked',
            revoked_versions: 2,
            revocation_epoch: 5,
          },
        }
      }
      return mcpRoutes({
        view: {
          env_keys: ['TOKEN', 'REGION'],
          credential_keys: [
            { key: 'TOKEN', mode: 'managed', state: 'managed', last4: '9f2c' },
            { key: 'REGION', mode: 'env', state: 'env', last4: '' },
          ],
        },
      })(call)
    })
    await openMcpDetail()

    const table = wrapper!.find('.mcp-credentials-table').text()
    expect(table).toContain('TOKEN')
    expect(table).toContain('····9f2c')
    expect(table).toContain('托管有效')
    expect(table).toContain('环境变量')
    // env 来源没有“撤销”入口：撤销的是托管值，不是环境变量。
    const revokeButtons = wrapper!
      .findAll('.mcp-credentials-table button')
      .filter((button) => button.text() === '撤销')
    expect(revokeButtons).toHaveLength(1)

    await revokeButtons[0]!.trigger('click')
    const dialog = wrapper!.find('.dialog')
    expect(dialog.text()).toContain('TOKEN')
    expect(dialog.text()).toContain('立即关闭并回收连接')
    await dialog.find('button.primary').trigger('click')
    await flushPromises()

    const posts = calls.filter(
      (call) => call.method === 'POST' && call.path.endsWith('/credential/revoke'),
    )
    expect(posts).toHaveLength(1)
    expect(posts[0]!.ifMatch).toBe(CONFIG_ETAG)
    expect(posts[0]!.body).toEqual({ resource_uid: UID, confirm: true, binding_key: 'TOKEN' })
    expect(wrapper!.text()).toContain('已撤销占位符 TOKEN')
  })

  it('applies the published version with If-Match and reports the switch', async () => {
    setupFetch((call) => {
      if (call.method === 'POST' && call.path === '/api/v1/mcp-servers/files/apply') {
        return {
          data: {
            outcome: 'applied',
            applied_version_id: 3,
            previous_version_id: 2,
            target_version_id: 3,
            drain_deadline: '2026-10-24T09:30:30Z',
            drain_timeout_seconds: 30,
            failure_class: '',
            superseded_by: '',
            operation_id: OP_ID,
          },
        }
      }
      return mcpRoutes({ view: { published_version_id: 3, published_version_no: 3 } })(call)
    })
    await openMcpDetail()

    await clickButton('应用发布版本…')
    expect(wrapper!.find('.dialog').text()).toContain('切换到 v3')
    await wrapper!.find('.dialog button.primary').trigger('click')
    await flushPromises()

    const posts = calls.filter(
      (call) => call.method === 'POST' && call.path === '/api/v1/mcp-servers/files/apply',
    )
    expect(posts).toHaveLength(1)
    expect(posts[0]!.ifMatch).toBe(CONFIG_ETAG)
    expect(posts[0]!.body).toEqual({ version_id: 0 })
    const section = wrapper!.find('.view-resource-detail').text()
    expect(section).toContain('受控应用')
    expect(section).toContain('已应用')
    expect(section).toContain('排空窗口')
  })

  it('reads the operation row on a superseded apply and never resends', async () => {
    setupFetch((call) => {
      if (call.method === 'POST' && call.path === '/api/v1/mcp-servers/files/apply') {
        return {
          status: 409,
          code: 'MCP_APPLY_SUPERSEDED',
          message: '另一次发布推进了发布指针',
          data: {
            operation_id: OP_ID,
            outcome: 'superseded',
            superseded_by: 'publish',
            applied_version_id: 2,
            previous_version_id: 2,
            target_version_id: 3,
            drain_deadline: '',
            drain_timeout_seconds: 30,
            failure_class: '',
          },
        }
      }
      return mcpRoutes({
        view: { published_version_id: 3, published_version_no: 3 },
        operation: {
          action: 'mcp_server.apply',
          detail: {
            outcome: 'superseded',
            applied_version_id: 2,
            previous_version_id: 2,
            target_version_id: 3,
            drain_deadline: '',
            drain_timeout_seconds: 30,
            failure_class: '',
            superseded_by: 'publish',
            operation_id: OP_ID,
          },
        },
      })(call)
    })
    await openMcpDetail()

    await clickButton('应用发布版本…')
    await wrapper!.find('.dialog button.primary').trigger('click')
    await flushPromises()

    // 只有一次写：412 / 409 都不自动重发。
    expect(
      calls.filter((call) => call.method === 'POST' && call.path.endsWith('/apply')),
    ).toHaveLength(1)
    // 失败读数只认操作台账：被 publish 取代，运行版本不变。
    const text = wrapper!.find('.view-resource-detail').text()
    expect(text).toContain('被取代（未切换）')
    expect(text).toContain('另一次发布推进了发布指针')
    expect(text).toContain('操作状态')
  })
})
