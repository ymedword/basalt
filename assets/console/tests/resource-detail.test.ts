// 资源详情页（R4 追加部分）回归：范围与托管凭据卡片、显式 UID + If-Match 的
// 范围切换与撤销、共享转专属被引用时的点名与专属文案、撤销必须经过确认、
// 引用卡片展示 UID 绑定与“引用未记录”证据缺口。
// Resource detail regressions for the R4 cards: scope and managed credential,
// UID + If-Match on both writes, the pointed denial for a blocked scope switch,
// a confirmation step before revoking, and UID-based reference evidence gaps.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ModelConnectionView, ProfileView } from '../src/api'
import type { ResourceReferencesView, ResourceVersionView } from '../src/api-admin'
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
