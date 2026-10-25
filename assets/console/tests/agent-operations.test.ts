// US-12 R3.5 页面操作回归：新建会话 / 发消息 / 归档、能力绑定保存、定时任务
// 增删改启停、技能文件修改、MCP 环境变量整条替换闸门。全部只经真实视图与路由
// 触发（按用户可见文案找入口），不直接调用 api-admin.ts 的方法。
// Page-operation regressions for US-12 R3.5: sessions (create/send/archive),
// capability bindings, schedule CRUD, skill file edits and the MCP env gate.
// Everything is driven through real views and the router by visible labels.
import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleApp } from '../src/app'
import { formatTimestamp } from '../src/components/ui'

interface Call {
  readonly method: string
  readonly path: string
  readonly body: unknown
  readonly headers: Record<string, string>
}

interface Reply {
  readonly status?: number
  readonly code?: string
  readonly message?: string
  readonly data?: unknown
  /** 详情类 GET 的强 ETag 只出现在响应头。 */
  readonly etag?: string
}

type Route = (call: Call) => Reply | Promise<Reply> | undefined

const summary = {
  version: 'test-version', uptime_seconds: 3600, started_at: '2026-10-21T12:00:00Z',
  agents: { total: 1, active: 1, disabled: 0, archived: 0, invalid: 0 },
  runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
  resources: { model_connections: 0, mcp_servers: 0, skills: 0 },
  database: { status: 'ok', schema_version: 2 }, auth: { mode: 'token', loopback_only: true },
}

const ETAG_A = '"sha256:' + 'a'.repeat(64) + '"'

let wrapper: ReturnType<typeof mount> | undefined
let calls: Call[] = []
let profilesData: unknown[] = []

/** Agent Profile 夹具；tools / skills / notify_channels 同时驱动能力页签的初始选择。 */
// Profile fixture; tools/skills/notify_channels also seed the capabilities tab.
function profileOf(
  name: string,
  state: 'active' | 'disabled' = 'disabled',
  tools: string[] = [],
  skills: string[] = [],
  notifyChannels: string[] = [],
): Record<string, unknown> {
  return {
    name,
    display_name: '',
    description: name + ' description',
    provider: 'demo',
    model: 'demo-model',
    tools,
    skills,
    notify_channels: notifyChannels,
    schedules: 0,
    registered: true,
    runnable: state === 'active',
    problems: [],
    lifecycle: {
      agent: name,
      state,
      valid: true,
      generation: 1,
      updated_at: '2026-10-21T12:00:00Z',
      operation_id: 'op-test',
      lifecycle_error: '',
    },
  }
}

function filesViewOf(name: string): Record<string, unknown> {
  const profile = profileOf(name, 'disabled')
  // 契约 AgentFilesView 的 changes 恒存在：空数组 = 本次保存没有系统自动增删。
  return {
    agent: name,
    lifecycle: profile.lifecycle,
    etag: ETAG_A,
    files: [],
    changes: { tools_added: [], tools_removed: [] },
  }
}

const UID_DEMO = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z9A'
const UID_CHANNEL = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z9B'

/** 统一配置页的 AgentConfigView 夹具；测试只覆写自己关心的字段。 */
// AgentConfigView fixture for the unified config tab.
function agentConfigOf(
  fields: Record<string, unknown> = {},
  view: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    agent: 'writer',
    agent_uid: 'ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z9C',
    content_etag: ETAG_A,
    fields: {
      name: 'writer',
      description: 'writer description',
      display_name: '',
      identity: { agent_name: 'Writer', prompt: '你是作者。' },
      provider: { name: 'demo', model: 'demo-model', temperature: null },
      tools: ['save_memory', 'legacy_tool'],
      skills: ['legacy_skill'],
      mcp_servers: [],
      bootstrap: [],
      notify_mode: 'none',
      notify_channels: [],
      resource_bindings: {
        model_connections: [{ name: 'demo', uid: UID_DEMO }],
        notify_channels: [],
      },
      settings: { max_iterations: 8, max_history_turns: 20 },
      schedules: [],
      ...fields,
    },
    notify_mode: 'none',
    notify_mode_source: 'explicit',
    references: [{
      resource_kind: 'model_connection', ref_name: 'demo', resource_uid: UID_DEMO,
      resource_name: 'demo', binding_status: 'bound', published: true, eligible: true,
      deleted: false, credential_state: 'active',
    }],
    binding_complete: true,
    ...view,
  }
}

function sessionOf(
  sessionId: string,
  status: 'active' | 'archived' = 'active',
  messageCount = 0,
): Record<string, unknown> {
  return {
    session_id: sessionId,
    profile_name: 'writer',
    channel: 'web',
    user_id: 'console',
    status,
    message_count: messageCount,
    created_at: '2026-10-22T10:00:00Z',
    last_active_at: '2026-10-22T10:00:00Z',
  }
}

function runOf(): Record<string, unknown> {
  return {
    run_id: 'run-1',
    session_id: 's-1',
    agent: 'writer',
    channel: 'web',
    trigger: 'user',
    status: 'completed',
    reason: '',
    final_text: 'done',
    iterations: 2,
    tool_calls: 1,
    config_hash: 'cfg',
    manifest_hash: 'man',
    started_at: '2026-10-22T10:00:00Z',
    finished_at: '2026-10-22T10:00:01Z',
  }
}

function scheduleOf(
  id: string,
  enabled = true,
  message = '早报',
): Record<string, unknown> {
  return {
    id,
    agent: 'writer',
    cron: '0 0 9 * * *',
    zone: 'Asia/Shanghai',
    message,
    deliver_to: '',
    enabled,
    last_run_at: '',
    last_status: '',
  }
}

function skillDetailOf(content: string): Record<string, unknown> {
  return {
    name: 'alpha',
    description: 'alpha skill',
    path: 'alpha',
    version_id: 1,
    version_no: 1,
    content_hash: 'hash',
    etag: ETAG_A,
    file_count: 1,
    referenced_by: [],
    files: [{ path: 'SKILL.md', size: content.length, sha256: 'sha', content }],
  }
}

function mcpDetailOf(envKeys: string[]): Record<string, unknown> {
  return {
    name: 'files',
    transport: 'stdio',
    command: 'npx',
    args: [],
    url: '',
    env_keys: envKeys,
    connected: true,
    tools: ['read_file'],
    published_version_id: 1,
    published_version_no: 1,
    applied_version_id: 1,
    applied_version_no: 1,
    pending_restart: false,
    tool_disposition: { offered: 1, accepted: 1, skipped: [] },
  }
}

function defaultReply(call: Call): Reply {
  if (call.method !== 'GET') {
    return { status: 500, message: '未预期的写请求：' + call.method + ' ' + call.path }
  }
  if (call.path.endsWith('/info')) {
    return { data: { version: 'test-version', uptime_seconds: 3600, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
  }
  if (call.path.endsWith('/system/overview')) {
    return { data: summary }
  }
  if (call.path.endsWith('/system/sample')) {
    return {
      data: {
        sampled_at: '2026-10-21T13:00:00Z',
        cpu_percent: null,
        memory_rss_bytes: 1024,
        goroutines: 12,
        unavailable: ['cpu_percent'],
      },
    }
  }
  if (call.path.endsWith('/profiles')) {
    return { data: profilesData }
  }
  if (call.path.endsWith('/memory')) {
    return { data: { scope: 'global', agent: '', backend: 'markdown', core: '', archive: '' } }
  }
  return { data: [] }
}

function toResponse(reply: Reply): Response {
  const status = reply.status ?? 200
  const code = reply.code ?? (status < 300 ? 'OK' : 'INTERNAL')
  const envelope = {
    code,
    message: reply.message ?? '',
    data: reply.data ?? null,
    timestamp: '2026-10-22T12:00:00Z',
  }
  const headers = new Headers()
  if (reply.etag !== undefined) {
    headers.set('ETag', reply.etag)
  }
  return {
    ok: status >= 200 && status < 300,
    status,
    headers,
    json: async () => envelope,
  } as Response
}

function installFetch(route: Route = () => undefined): void {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const call: Call = {
      method: (init?.method ?? 'GET').toUpperCase(),
      path: url.pathname,
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
      headers: (init?.headers as Record<string, string> | undefined) ?? {},
    }
    calls.push(call)
    const routed = await route(call)
    return toResponse(routed ?? defaultReply(call))
  }))
}

async function signIn(route: Route = () => undefined): Promise<void> {
  installFetch(route)
  window.history.replaceState(null, '', '/console/login')
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

async function visit(path: string): Promise<void> {
  window.history.pushState(null, '', path)
  window.dispatchEvent(new PopStateEvent('popstate'))
  await flushPromises()
}

/** 按可见文案找按钮：测试只认用户能点到的入口，不认内部实现。 */
// Find a button by its visible label, the way a user would.
function button(label: string) {
  const found = wrapper!.findAll('button').find(item => item.text().trim() === label)
  expect(found, '按钮「' + label + '」应可达').toBeTruthy()
  return found!
}

function dialogButton(label: string) {
  const dialog = wrapper!.find('.dialog')
  const found = dialog.findAll('button').find(item => item.text().trim() === label)
  expect(found, '对话框按钮「' + label + '」应可达').toBeTruthy()
  return found!
}

/** 按复选框旁的 code 文案定位一行绑定。 */
// Locate a binding row by the code label next to its checkbox.
function checkRow(label: string) {
  const row = wrapper!.findAll('.check-row').find(item =>
    item.findAll('code').some(code => code.text() === label),
  )
  expect(row, '绑定行「' + label + '」应可达').toBeTruthy()
  return row!
}

function checkRowInput(label: string) {
  return checkRow(label).find('input')
}

/** 行内按钮：按表格行的可见文案定位，避免依赖列表顺序。 */
// Row buttons are located by the row's visible text, not by list order.
function rowButton(rowLabel: string, label: string) {
  const row = wrapper!.findAll('tr').find(item => item.text().includes(rowLabel))
  expect(row, '表格行「' + rowLabel + '」应可达').toBeTruthy()
  const found = row!.findAll('button').find(item => item.text().trim() === label)
  expect(found, '行「' + rowLabel + '」的按钮「' + label + '」应可达').toBeTruthy()
  return found!
}

function callsTo(method: string, path: string): Call[] {
  return calls.filter(call => call.method === method && call.path === path)
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

describe('agent session operations', () => {
  it('creates a session, sends a message and archives it after confirmation', async () => {
    let sessionList: Record<string, unknown>[] = []
    let messages: Record<string, unknown>[] = []
    let status: 'active' | 'archived' = 'active'
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/sessions') {
        return { data: sessionList }
      }
      if (call.method === 'POST' && call.path === '/api/v1/sessions') {
        sessionList = [sessionOf('s-1')]
        return { data: sessionOf('s-1') }
      }
      if (call.method === 'GET' && call.path === '/api/v1/sessions/s-1') {
        return { data: { ...sessionOf('s-1', status, messages.length), messages } }
      }
      if (call.method === 'POST' && call.path === '/api/v1/sessions/s-1/messages') {
        messages = [
          { role: 'user', content: 'hello', created_at: '2026-10-22T10:00:00Z' },
          { role: 'assistant', content: 'done', created_at: '2026-10-22T10:00:01Z' },
        ]
        sessionList = [sessionOf('s-1', status, messages.length)]
        return { data: runOf() }
      }
      if (call.method === 'DELETE' && call.path === '/api/v1/sessions/s-1') {
        status = 'archived'
        sessionList = [sessionOf('s-1', status, messages.length)]
        return { data: null, message: '会话已归档' }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')

    expect(wrapper!.text()).toContain('该 Agent 还没有会话')
    await button('新建会话').trigger('click')
    await flushPromises()

    const createCalls = callsTo('POST', '/api/v1/sessions')
    expect(createCalls).toHaveLength(1)
    expect(createCalls[0]?.body).toEqual({ agent: 'writer', channel: 'web', user_id: 'console' })
    // 新建后直接进入独立会话深链，而不是在长列表底部展开面板。
    expect(window.location.pathname).toBe('/console/agents/writer/sessions/s-1')
    expect(wrapper!.find('.view-session-detail').text()).toContain('会话详情')

    await wrapper!.find('#session-chat-input').setValue('hello')
    await button('发送并运行').trigger('click')
    await flushPromises()

    const sendCalls = callsTo('POST', '/api/v1/sessions/s-1/messages')
    expect(sendCalls).toHaveLength(1)
    expect(sendCalls[0]?.body).toEqual({ message: 'hello' })
    expect(wrapper!.find('.view-session-detail').text()).toContain('运行完成（2 轮，1 次工具调用）')
    expect(wrapper!.find('.transcript').text()).toContain('hello')
    expect(wrapper!.find('.transcript').text()).toContain('done')

    // 取消确认框不发任何写请求；再次确认才归档。
    await button('归档会话').trigger('click')
    expect(wrapper!.find('.dialog').text()).toContain('只能查看历史消息')
    await dialogButton('取消').trigger('click')
    expect(callsTo('DELETE', '/api/v1/sessions/s-1')).toHaveLength(0)

    await button('归档会话').trigger('click')
    await dialogButton('归档会话').trigger('click')
    await flushPromises()

    expect(callsTo('DELETE', '/api/v1/sessions/s-1')).toHaveLength(1)
    // 页面里同时有上一次发送的结果提示，归档成功提示在整页文本里核对。
    expect(wrapper!.text()).toContain('已归档会话 s-1；历史消息保留。')
    // 归档后同一页面转为只读：历史消息仍在，输入框消失。
    expect(wrapper!.find('.transcript').text()).toContain('hello')
    expect(wrapper!.find('#session-chat-input').exists()).toBe(false)

    // 返回 Agent 页：列表仍是独立入口，状态显示已归档。
    await wrapper!.find('.back-link a').trigger('click')
    await flushPromises()
    expect(window.location.pathname).toBe('/console/agents/writer')
    expect(wrapper!.find('.agent-sessions-table').text()).toContain('已归档')
  })

  it('labels the time column 最近使用 and marks scheduler-created sessions', async () => {
    const sessionList = [
      {
        ...sessionOf('s-scheduled'),
        channel: 'scheduler',
        user_id: 'scheduler',
        message_count: 0,
        created_at: '2026-10-22T08:00:00Z',
        last_active_at: '2026-10-22T09:00:00Z',
      },
      {
        ...sessionOf('s-used', 'active', 3),
        created_at: '2026-10-21T08:00:00Z',
        last_active_at: '2026-10-22T08:30:00Z',
      },
    ]
    profilesData = [profileOf('writer', 'active')]
    await signIn(call => call.method === 'GET' && call.path === '/api/v1/sessions'
      ? { data: sessionList }
      : undefined)
    await visit('/console/agents/writer')

    const table = wrapper!.find('.agent-sessions-table')
    expect(table.text()).toContain('最近使用')
    const scheduled = table.findAll('tr').find(row => row.text().includes('s-scheduled'))!
    const used = table.findAll('tr').find(row => row.text().includes('s-used'))!
    expect(scheduled.find('.badge').text()).toBe('定时任务')
    // 「最近使用」取运行准入时间：零消息但已运行过的会话不能退回创建时间。
    expect(scheduled.find('.col-time').text()).toBe(formatTimestamp('2026-10-22T09:00:00Z'))
    expect(used.find('.col-time').text()).toBe(formatTimestamp('2026-10-22T08:30:00Z'))
    expect(used.find('.col-time span').attributes('title')).toContain('最近一次运行开始')
    // 列表只读刷新留痕：页头说明最近一次成功读取的时间。
    expect(wrapper!.find('.refresh-stamp').text()).toContain('最近刷新')
  })

  it('reopens an archived console session under a fresh user id', async () => {
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/sessions') {
        return { data: [sessionOf('s-old', 'archived', 2)] }
      }
      if (call.method === 'POST' && call.path === '/api/v1/sessions') {
        const body = call.body as { user_id?: string }
        if (body.user_id === 'console') {
          // 服务端语义：已归档行不复活（session_store 的 GetOrCreateSession）。
          return { data: sessionOf('s-old', 'archived', 2) }
        }
        return { data: sessionOf('s-new') }
      }
      if (call.method === 'GET' && call.path === '/api/v1/sessions/s-new') {
        return { data: { ...sessionOf('s-new'), messages: [] } }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')

    await button('新建会话').trigger('click')
    await flushPromises()

    const createCalls = callsTo('POST', '/api/v1/sessions')
    expect(createCalls).toHaveLength(2)
    expect(createCalls[0]?.body).toEqual({ agent: 'writer', channel: 'web', user_id: 'console' })
    const retry = createCalls[1]?.body as { agent: string; channel: string; user_id: string }
    expect(retry.agent).toBe('writer')
    expect(retry.channel).toBe('web')
    expect(retry.user_id).toMatch(/^console-/)
    // 换新用户标识后落在新会话深链，并解释原会话已归档。
    expect(window.location.pathname).toBe('/console/agents/writer/sessions/s-new')
    expect(window.location.search).toBe('?reopened=1')
    expect(wrapper!.find('.view-session-detail').text()).toContain('原 console 会话已归档')
  })

  it('opens the deep link directly and reports a busy session without retrying', async () => {
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/sessions') {
        return { data: [sessionOf('s-1')] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/sessions/s-1') {
        return { data: { ...sessionOf('s-1'), messages: [] } }
      }
      if (call.method === 'POST' && call.path === '/api/v1/sessions/s-1/messages') {
        return { status: 409, code: 'SESSION_BUSY', message: '会话正在执行另一轮运行' }
      }
      if (call.method === 'DELETE' && call.path === '/api/v1/sessions/s-1') {
        return { status: 409, code: 'SESSION_BUSY', message: '会话正在执行另一轮运行' }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')

    // 列表里的会话 ID 与“查看”都指向同一深链。
    const viewLink = wrapper!.findAll('a').find((item) => item.text().trim() === '查看')
    expect(viewLink?.attributes('href')).toBe('/console/agents/writer/sessions/s-1')
    expect(
      wrapper!.findAll('a').some((item) => item.attributes('href') === '/console/agents/writer/sessions/s-1'),
    ).toBe(true)

    // 直接打开深链等价于刷新 / 收藏 / 从别处分享进来，页面独立读取会话。
    await visit('/console/agents/writer/sessions/s-1')
    await wrapper!.find('#session-chat-input').setValue('hello')
    await button('发送并运行').trigger('click')
    await flushPromises()

    expect(callsTo('POST', '/api/v1/sessions/s-1/messages')).toHaveLength(1)
    expect(wrapper!.find('.view-session-detail').text()).toContain('该会话已有运行在执行')

    await button('归档会话').trigger('click')
    await dialogButton('归档会话').trigger('click')
    await flushPromises()

    expect(callsTo('DELETE', '/api/v1/sessions/s-1')).toHaveLength(1)
    expect(wrapper!.find('.notice-error').text()).toContain('会话仍有在途运行，未归档')
    expect(wrapper!.text()).not.toContain('已归档会话')
  })

  it('states that session history is not long-term memory', async () => {
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/memory') {
        return { data: { scope: 'agent', agent: 'writer', backend: 'markdown', core: '', archive: '' } }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')

    const memoryTab = wrapper!.findAll('.tabs button').find((item) => item.text() === '记忆')
    expect(memoryTab, '记忆页签应可达').toBeTruthy()
    await memoryTab!.trigger('click')
    await flushPromises()

    const text = wrapper!.text()
    // 会话消息自动保存，但保存的是对话历史，不是记忆条目；两层分开说清楚。
    expect(text).toContain('会话消息随每次运行自动保存为会话历史')
    expect(text).toContain('不会自动进入长期记忆')
    expect(text).toContain('只有 Agent 在运行中调用 save_memory 才会写入')
    // 可检索层是关键词召回，不是语义检索，也没有对话后的自动抽取。
    expect(text).toContain('不会自动从对话里抽取内容')
    expect(text).not.toContain('自动形成长期记忆')
  })
})

describe('unified agent config tab', () => {
  it('saves tool, skill and channel selections from one page and keeps catalog-unknown bindings', async () => {
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/config') {
        return { data: agentConfigOf(), etag: ETAG_A }
      }
      if (call.method === 'GET' && call.path === '/api/v1/tools') {
        return {
          data: [
            { name: 'save_memory', description: '保存记忆', source: 'builtin' },
            { name: 'recall_memory', description: '检索记忆', source: 'builtin' },
          ],
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/skills') {
        return {
          data: [{
            name: 'summarize', description: '摘要', path: 'summarize',
            version_id: 1, version_no: 1, content_hash: 'h', etag: ETAG_A,
            file_count: 1, referenced_by: [],
          }],
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/mcp-servers') {
        return { data: [] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/model-connections') {
        return {
          data: [{
            name: 'demo', resource_uid: UID_DEMO, base_url: 'https://api.example.com',
            default_model: 'demo-model', credential_mode: 'env', credential_state: 'active',
            scope: 'shared', owner_agent: '', published_version_id: 1, published_version_no: 1,
          }],
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/notify-channels') {
        return {
          data: [{
            name: 'ops-alerts', resource_uid: UID_CHANNEL, type: 'webhook',
            description: '值班告警', url_host: 'hooks.example.com', url_configured: true,
            address_status: 'env', credential_mode: 'env', credential_state: 'active',
            scope: 'shared', owner_agent: '', published_version_id: 1,
            published_version_no: 1, referenced_by: [],
          }],
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/notify-channels/types') {
        return { data: ['webhook'] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/content') {
        return { data: { content: '---\nname: writer\n---\n你是作者。', etag: ETAG_A } }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/agents/writer/content') {
        return { data: filesViewOf('writer') }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active', ['save_memory', 'legacy_tool'], ['legacy_skill'])]
    await signIn(route)
    await visit('/console/agents/writer')

    await button('配置').trigger('click')
    await flushPromises()

    // 目录里没有的既有绑定必须保留勾选并标注，否则保存会静默删除声明。
    expect(checkRow('legacy_tool').text()).toContain('不在当前工具目录中')
    expect((checkRowInput('legacy_tool').element as HTMLInputElement).checked).toBe(true)
    expect((checkRowInput('legacy_skill').element as HTMLInputElement).checked).toBe(true)

    // 通知渠道与工具/技能同一套目录：切到 allowlist 后显示服务端状态（含类型与地址来源）。
    await wrapper!.find('#config-notify-mode-allowlist').setValue()
    expect(checkRow('ops-alerts').text()).toContain('环境变量（发送时解析）')

    await checkRowInput('recall_memory').setValue(true)
    await checkRowInput('summarize').setValue(true)
    await checkRowInput('ops-alerts').setValue(true)
    await button('保存配置').trigger('click')
    await flushPromises()

    // 首次读取 + 保存成功后的重新读取（不做乐观更新）。
    const contentReads = callsTo('GET', '/api/v1/agents/writer/content')
    expect(contentReads).toHaveLength(2)
    const writes = callsTo('PUT', '/api/v1/agents/writer/content')
    expect(writes).toHaveLength(1)
    const changes = (writes[0]!.body as { changes: Record<string, unknown> }).changes
    expect([...(changes.tools as string[])].sort()).toEqual([
      'legacy_tool', 'recall_memory', 'save_memory',
    ])
    expect([...(changes.skills as string[])].sort()).toEqual(['legacy_skill', 'summarize'])
    expect(changes.notify_mode).toBe('allowlist')
    expect(changes.notify_channels).toEqual(['ops-alerts'])
    // 新渠道必须显式绑定 UID：勾选不会按名称自动补绑。
    expect(changes.bindings).toEqual([
      { resource_kind: 'model_connection', ref_name: 'demo', resource_uid: UID_DEMO },
      { resource_kind: 'notify_channel', ref_name: 'ops-alerts', resource_uid: UID_CHANNEL },
    ])
    expect(writes[0]!.headers['If-Match']).toBe(ETAG_A)
    expect(wrapper!.text()).toContain('已保存')
  })

  it('does not rewrite notify_channels when only tools changed', async () => {
    // 未改动通知渠道时不得发送 notify_channels: []——否则每次保存工具/技能都会
    // 把“未声明 = 不限制”悄悄改成空声明（对服务端等价，但会制造无谓变更）。
    // A tools-only save must not emit notify_channels: legacy (absent) means unrestricted.
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/tools') {
        return { data: [{ name: 'save_memory', description: '', source: 'builtin' }] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/config') {
        return {
          data: agentConfigOf({
            tools: ['save_memory'], skills: [], mcp_servers: [],
          }),
          etag: ETAG_A,
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/model-connections') {
        return {
          data: [{
            name: 'demo', resource_uid: UID_DEMO, base_url: 'https://api.example.com',
            default_model: 'demo-model', credential_mode: 'env', credential_state: 'active',
            scope: 'shared', owner_agent: '', published_version_id: 1, published_version_no: 1,
          }],
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/content') {
        return { data: { content: '---\nname: writer\n---\n你是作者。', etag: ETAG_A } }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/agents/writer/content') {
        return { data: filesViewOf('writer') }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active', ['save_memory'])]
    await signIn(route)
    await visit('/console/agents/writer')

    await button('配置').trigger('click')
    await flushPromises()
    // 工具目录只有一项且已勾选；取消勾选制造一次仅工具变化的保存。
    await checkRowInput('save_memory').setValue(false)
    await button('保存配置').trigger('click')
    await flushPromises()

    const writes = callsTo('PUT', '/api/v1/agents/writer/content')
    expect(writes).toHaveLength(1)
    const changes = (writes[0]!.body as { changes: Record<string, unknown> }).changes
    expect(changes.tools).toEqual([])
    expect('notify_channels' in changes).toBe(false)
    expect('notify_mode' in changes).toBe(false)
  })

  // 配置草稿随组件销毁：页签切换与路由离开都必须先确认，取消后地址与草稿保持不变。
  // The config draft dies with the component; both exits must ask first.
  function configGuardRoute(): Route {
    return (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/config') {
        return { data: agentConfigOf(), etag: ETAG_A }
      }
      if (call.method === 'GET' && call.path === '/api/v1/tools') {
        return { data: [{ name: 'save_memory', description: '', source: 'builtin' }] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/model-connections') {
        return {
          data: [{
            name: 'demo', resource_uid: UID_DEMO, base_url: 'https://api.example.com',
            default_model: 'demo-model', credential_mode: 'env', credential_state: 'active',
            scope: 'shared', owner_agent: '', published_version_id: 1, published_version_no: 1,
          }],
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/content') {
        return { data: { content: '---\nname: writer\n---\n你是作者。', etag: ETAG_A } }
      }
      return undefined
    }
  }

  it('asks before switching tabs away from an unsaved config draft', async () => {
    profilesData = [profileOf('writer', 'active', ['save_memory'])]
    await signIn(configGuardRoute())
    await visit('/console/agents/writer')

    await button('配置').trigger('click')
    await flushPromises()
    await wrapper!.find('#config-description').setValue('新描述')

    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    try {
      await button('运行').trigger('click')
      await flushPromises()
      expect(confirm).toHaveBeenCalledWith('配置有未保存的修改，切换页签将丢失这些修改。继续？')
      expect(wrapper!.find('#config-description').exists()).toBe(true)

      confirm.mockReturnValue(true)
      await button('运行').trigger('click')
      await flushPromises()
      expect(wrapper!.find('#config-description').exists()).toBe(false)
    } finally {
      confirm.mockRestore()
    }
  })

  it('asks before leaving the route with an unsaved config draft', async () => {
    profilesData = [profileOf('writer', 'active', ['save_memory'])]
    await signIn(configGuardRoute())
    await visit('/console/agents/writer')

    await button('配置').trigger('click')
    await flushPromises()
    await wrapper!.find('#config-description').setValue('新描述')

    const brand = wrapper!.findAll('a').find((item) => item.attributes('href') === '/console/')
    expect(brand, '侧栏品牌链接应可达').toBeTruthy()
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    try {
      await brand!.trigger('click')
      await flushPromises()
      expect(confirm).toHaveBeenCalledWith('配置有未保存的修改，确定离开？')
      expect(window.location.pathname).toBe('/console/agents/writer')
      expect(wrapper!.find('#config-description').exists()).toBe(true)

      confirm.mockReturnValue(true)
      await brand!.trigger('click')
      await flushPromises()
      expect(window.location.pathname).toBe('/console/')
    } finally {
      confirm.mockRestore()
    }
  })
})

describe('agent detail placeholder model', () => {
  it('warns that the template placeholder passes validation but breaks every run', async () => {
    // 模板占位符是普通文本：注册成功、可启用，第一次运行才失败。页头必须提前点名。
    // The placeholder registers and can be enabled; only a run reveals it — warn up front.
    profilesData = [{ ...profileOf('writer', 'active'), model: '请在此填写模型名' }]
    await signIn()
    await visit('/console/agents/writer')

    const warning = wrapper!.find('.notice-warn')
    expect(warning.text()).toContain('请在此填写模型名')
    expect(warning.text()).toContain('「配置」')
  })
})

describe('agent schedules', () => {
  it('creates, edits, toggles, runs and deletes a schedule', async () => {
    let scheduleList: Record<string, unknown>[] = [scheduleOf('daily')]
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/schedules') {
        return { data: scheduleList }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/content') {
        return { data: { content: '# writer', etag: ETAG_A } }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/agents/writer/content') {
        const rows = (call.body as {
          changes: { schedules: Array<{ id: string; cron: string; zone: string; message: string }> }
        }).changes.schedules
        scheduleList = rows.map((row) => {
          const previous = scheduleList.find(item => item.id === row.id)
          return {
            ...scheduleOf(row.id, previous === undefined ? true : (previous.enabled as boolean), row.message),
            cron: row.cron,
            zone: row.zone,
          }
        })
        return { data: filesViewOf('writer') }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/schedules/daily') {
        const body = call.body as { enabled: boolean }
        scheduleList = scheduleList.map(item =>
          item.id === 'daily' ? { ...item, enabled: body.enabled } : item,
        )
        return { data: null }
      }
      if (call.method === 'POST' && call.path === '/api/v1/schedules/daily/run') {
        return { data: { ...runOf(), trigger: 'manual' } }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')

    await button('定时').trigger('click')
    await flushPromises()
    expect(wrapper!.find('.agent-schedules-table').text()).toContain('daily')

    // 新建：整段替换，已有任务保留。
    await button('新建定时任务').trigger('click')
    await wrapper!.find('#schedule-id').setValue('weekly')
    await wrapper!.find('#schedule-cron').setValue('0 0 10 * * 1')
    await wrapper!.find('#schedule-zone').setValue('Asia/Shanghai')
    await wrapper!.find('#schedule-message').setValue('周报')
    await button('创建任务').trigger('click')
    await flushPromises()

    let writes = callsTo('PUT', '/api/v1/agents/writer/content')
    expect(writes).toHaveLength(1)
    let rows = (writes[0]!.body as {
      changes: { schedules: Array<{ id: string; cron: string; zone: string; message: string }> }
    }).changes.schedules
    expect(rows.map(row => row.id).sort()).toEqual(['daily', 'weekly'])
    expect(rows.find(row => row.id === 'weekly')).toEqual({
      id: 'weekly', cron: '0 0 10 * * 1', zone: 'Asia/Shanghai', message: '周报',
      deliver_to: '',
    })
    expect(writes[0]!.headers['If-Match']).toBe(ETAG_A)
    expect(wrapper!.text()).toContain('已创建定时任务 weekly')

    // 编辑：ID 是任务身份，编辑态不可改；保存后仍整段替换。
    await rowButton('daily', '编辑').trigger('click')
    expect((wrapper!.find('#schedule-id').element as HTMLInputElement).disabled).toBe(true)
    await wrapper!.find('#schedule-message').setValue('早报 v2')
    await button('保存修改').trigger('click')
    await flushPromises()

    writes = callsTo('PUT', '/api/v1/agents/writer/content')
    expect(writes).toHaveLength(2)
    rows = (writes[1]!.body as {
      changes: { schedules: Array<{ id: string; cron: string; zone: string; message: string }> }
    }).changes.schedules
    expect(rows.find(row => row.id === 'daily')?.message).toBe('早报 v2')
    expect(rows.find(row => row.id === 'weekly')?.message).toBe('周报')

    // 停用只写数据库状态，不动定义文件。
    await rowButton('daily', '停用').trigger('click')
    await flushPromises()
    const toggles = callsTo('PUT', '/api/v1/schedules/daily')
    expect(toggles).toHaveLength(1)
    expect(toggles[0]?.body).toEqual({ enabled: false })
    expect(wrapper!.text()).toContain('已停用定时任务 daily')

    // 立即运行走专用端点，结果与聊天同口径。
    await rowButton('daily', '立即运行').trigger('click')
    await flushPromises()
    expect(callsTo('POST', '/api/v1/schedules/daily/run')).toHaveLength(1)
    expect(wrapper!.text()).toContain('daily：运行完成')

    // 删除需要确认，成功后定义里不再有该任务。
    await rowButton('daily', '删除').trigger('click')
    expect(wrapper!.find('.dialog').text()).toContain('删除定时任务 daily')
    await dialogButton('删除').trigger('click')
    await flushPromises()

    writes = callsTo('PUT', '/api/v1/agents/writer/content')
    expect(writes).toHaveLength(3)
    rows = (writes[2]!.body as {
      changes: { schedules: Array<{ id: string; cron: string; zone: string; message: string }> }
    }).changes.schedules
    expect(rows.map(row => row.id)).toEqual(['weekly'])
    expect(wrapper!.text()).toContain('已删除定时任务 daily')
  })

  // 一句话 → 服务端候选（含三次触发）→ 确认走同一条件写；未确认前不碰文件。
  // One sentence becomes a server-computed candidate; saving uses the same write.
  it('turns one sentence into a server-computed candidate and saves it through the conditional write', async () => {
    let scheduleList: Record<string, unknown>[] = [scheduleOf('daily')]
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/schedules') {
        return { data: scheduleList }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/content') {
        return { data: { content: '# writer', etag: ETAG_A } }
      }
      if (call.method === 'POST' && call.path === '/api/v1/agents/writer/schedule-draft') {
        expect(call.body).toEqual({ instruction: '每天上午九点生成一份简报' })
        return {
          data: {
            agent: 'writer',
            id: 'morning-report',
            cron: '0 0 9 * * *',
            zone: 'Asia/Shanghai',
            message: '生成一份简报',
            next_runs: ['2026-10-25T01:00:00Z', '2026-10-26T01:00:00Z', '2026-10-27T01:00:00Z'],
          },
        }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/agents/writer/content') {
        const rows = (call.body as {
          changes: { schedules: Array<{ id: string; cron: string; zone: string; message: string }> }
        }).changes.schedules
        scheduleList = rows.map(row => ({
          ...scheduleOf(row.id, true, row.message),
          cron: row.cron,
          zone: row.zone,
        }))
        return { data: filesViewOf('writer') }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')
    await button('定时').trigger('click')
    await flushPromises()

    await wrapper!.find('#schedule-prompt').setValue('每天上午九点生成一份简报')
    await button('生成候选').trigger('click')
    await flushPromises()

    const candidate = wrapper!.find('#schedule-candidate')
    expect(candidate.exists()).toBe(true)
    expect(candidate.text()).toContain('morning-report')
    expect(candidate.text()).toContain('0 0 9 * * *')
    expect(candidate.text()).toContain('Asia/Shanghai')
    expect(candidate.text()).toContain('生成一份简报')
    // 未来三次触发来自服务端；页面只附本地换算，不自行推导。
    expect(candidate.findAll('.schedule-run')).toHaveLength(3)
    expect(candidate.text()).toContain('2026-10-25T01:00:00Z')
    expect(candidate.text()).toContain('2026-10-27T01:00:00Z')
    // 确认前不写文件。
    expect(callsTo('PUT', '/api/v1/agents/writer/content')).toHaveLength(0)

    await wrapper!.find('#schedule-candidate-save').trigger('click')
    await flushPromises()

    const writes = callsTo('PUT', '/api/v1/agents/writer/content')
    expect(writes).toHaveLength(1)
    expect(writes[0]!.headers['If-Match']).toBe(ETAG_A)
    const rows = (writes[0]!.body as {
      changes: { schedules: Array<{ id: string; cron: string; zone: string; message: string }> }
    }).changes.schedules
    expect(rows.map(row => row.id)).toEqual(['daily', 'morning-report'])
    expect(rows.find(row => row.id === 'morning-report')).toEqual({
      id: 'morning-report', cron: '0 0 9 * * *', zone: 'Asia/Shanghai', message: '生成一份简报',
      deliver_to: '',
    })
    expect(wrapper!.text()).toContain('已保存定时任务 morning-report')
    // 保存成功后候选卡与输入一起清空，避免重复提交同一句描述。
    expect(wrapper!.find('#schedule-candidate').exists()).toBe(false)
    expect((wrapper!.find('#schedule-prompt').element as HTMLInputElement).value).toBe('')
  })

  it('refuses a candidate whose id already exists instead of overwriting the task', async () => {
    let scheduleList: Record<string, unknown>[] = [scheduleOf('daily')]
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/schedules') {
        return { data: scheduleList }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/content') {
        return { data: { content: '# writer', etag: ETAG_A } }
      }
      if (call.method === 'POST' && call.path === '/api/v1/agents/writer/schedule-draft') {
        return {
          data: {
            agent: 'writer',
            id: 'daily',
            cron: '0 0 10 * * *',
            zone: 'Asia/Shanghai',
            message: '别的意思',
            // 空数组显示为「暂无触发时间」，而不是留一片空白。
            next_runs: [],
          },
        }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')
    await button('定时').trigger('click')
    await flushPromises()

    await wrapper!.find('#schedule-prompt').setValue('每天上午十点生成日报')
    await button('生成候选').trigger('click')
    await flushPromises()
    expect(wrapper!.find('#schedule-candidate').text()).toContain('（暂无触发时间）')

    await wrapper!.find('#schedule-candidate-save').trigger('click')
    await flushPromises()

    // 同名任务已存在 → 阻止保存并指向手工编辑，不静默覆盖。
    expect(callsTo('PUT', '/api/v1/agents/writer/content')).toHaveLength(0)
    expect(wrapper!.find('#schedule-candidate').text()).toContain('任务 ID 已存在：daily')
    expect(wrapper!.text()).toContain('直接在下方编辑该任务')
  })

  it('warns on the schedules tab when a task has no system delivery target', async () => {
    // R7：声明的渠道只是边界；发送由任务上的系统代投目标决定。任务没配目标时，
    // 结果不会由系统发送——提示直接指向"为任务选择投递渠道"，不再要求手动勾选 notify
    // （保存配置时系统会按通知意图自动装配它）。
    // R7: delivery is decided by the task's target; the hint points at that field.
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/schedules') {
        return { data: [scheduleOf('daily')] }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active', ['save_memory'], [], ['feishu-ops'])]
    await signIn(route)
    await visit('/console/agents/writer')
    await button('定时').trigger('click')
    await flushPromises()

    const warnings = wrapper!.findAll('.notice-warn').map(item => item.text()).join('\n')
    expect(warnings).toContain('没有配置结果投递渠道')
    expect(warnings).toContain('由系统在运行成功后发送')
  })

  // 系统代投（R7）：目标来自该 Agent 的渠道声明并预填；执行结果与投递结果分开显示。
  // Delivery target prefills from declared channels; run and delivery stay separate.
  it('prefills a declared delivery channel and shows run and delivery outcomes separately', async () => {
    let scheduleList: Record<string, unknown>[] = [
      { ...scheduleOf('daily'), deliver_to: UID_CHANNEL },
    ]
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/schedules') {
        return { data: scheduleList }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/content') {
        return { data: { content: '# writer', etag: ETAG_A } }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/config') {
        return {
          data: agentConfigOf({ notify_channels: ['feishu-ops'] }, {
            references: [
              ...(agentConfigOf().references as unknown[]),
              {
                resource_kind: 'notify_channel',
                ref_name: 'feishu-ops',
                resource_uid: UID_CHANNEL,
                resource_name: 'feishu-ops',
                binding_status: 'bound',
                published: true,
                eligible: true,
                deleted: false,
                credential_state: 'active',
              },
            ],
          }),
          etag: ETAG_A,
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/notify-channels') {
        return { data: [{ resource_uid: UID_CHANNEL, availability: 'ready' }] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/schedules/daily/executions') {
        return {
          data: [{
            task_id: 'daily',
            run_id: 'run-1',
            status: 'completed',
            trigger: 'cron',
            duration_ms: 1200,
            error_message: '',
            started_at: '2026-10-22T09:00:00Z',
            finished_at: '2026-10-22T09:00:01Z',
            delivery_status: 'sent',
            delivery_channel: UID_CHANNEL,
            delivery_detail: '已发送（HTTP 200）',
          }],
        }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/agents/writer/content') {
        const rows = (call.body as {
          changes: { schedules: Array<{ id: string; deliver_to: string }> }
        }).changes.schedules
        scheduleList = rows.map(row => ({
          ...scheduleOf(row.id, true),
          deliver_to: row.deliver_to,
        }))
        return { data: filesViewOf('writer') }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active', ['notify'], [], ['feishu-ops'])]
    await signIn(route)
    await visit('/console/agents/writer')
    await button('定时').trigger('click')
    await flushPromises()

    // 列表：渠道名与投递结果各自成列；投递详情单独一行，不与执行状态混写。
    const table = wrapper!.find('.agent-schedules-table').text()
    expect(table).toContain('feishu-ops')
    expect(table).toContain('已发送')
    expect(table).toContain('已发送（HTTP 200）')

    // 新建任务：默认选中声明渠道；保存时把 UID 写进定义行。
    await button('新建定时任务').trigger('click')
    const select = wrapper!.find('#schedule-deliver-to')
    expect((select.element as HTMLSelectElement).value).toBe(UID_CHANNEL)
    await wrapper!.find('#schedule-id').setValue('weekly')
    await wrapper!.find('#schedule-cron').setValue('0 0 10 * * 1')
    await wrapper!.find('#schedule-zone').setValue('Asia/Shanghai')
    await wrapper!.find('#schedule-message').setValue('周报')
    await button('创建任务').trigger('click')
    await flushPromises()

    const writes = callsTo('PUT', '/api/v1/agents/writer/content')
    expect(writes).toHaveLength(1)
    const rows = (writes[0]!.body as {
      changes: { schedules: Array<{ id: string; deliver_to: string }> }
    }).changes.schedules
    expect(rows.find(row => row.id === 'weekly')?.deliver_to).toBe(UID_CHANNEL)
    expect(rows.find(row => row.id === 'daily')?.deliver_to).toBe(UID_CHANNEL)
    expect(wrapper!.text()).toContain('由系统投递到 feishu-ops')
  })

  it('points at scheduled tasks that have no delivery target', async () => {
    // 旧任务没有 deliver_to：如实点名，并说明结果不会由系统发送。
    // Legacy tasks have no target; the page names them instead of staying silent.
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/schedules') {
        return { data: [scheduleOf('daily')] }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active', ['notify'], [], ['feishu-ops'])]
    await signIn(route)
    await visit('/console/agents/writer')
    await button('定时').trigger('click')
    await flushPromises()

    const warnings = wrapper!.findAll('.notice-warn').map(item => item.text()).join('\n')
    expect(warnings).toContain('以下定时任务没有配置结果投递渠道：daily')
    expect(warnings).toContain('不会由系统发送')
  })

  it('surfaces a failed delivery read instead of claiming nothing was sent', async () => {
    // 读失败是证据缺口，不是“没有投递”；页面必须分开说明。
    // A failed read is missing evidence, never "no delivery".
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/schedules') {
        return { data: [{ ...scheduleOf('daily'), deliver_to: UID_CHANNEL }] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/schedules/daily/executions') {
        return { status: 503, message: 'database is locked' }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active', ['notify'], [], ['feishu-ops'])]
    await signIn(route)
    await visit('/console/agents/writer')
    await button('定时').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.agent-schedules-table').text()).toContain('读取失败')
    expect(wrapper!.text()).toContain('这不代表没有投递')
  })
})

describe('resource edits', () => {
  it('submits skill files with the detail ETag and re-reads the listing', async () => {
    let skillContent = '# 旧版'
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/skills/alpha') {
        return { data: skillDetailOf(skillContent), etag: ETAG_A }
      }
      if (call.method === 'GET' && call.path === '/api/v1/resources/skill/alpha/versions') {
        return { data: [] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/resources/skill/alpha/references') {
        return {
          data: {
            kind: 'skill',
            id: 'alpha',
            resource_uid: 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A',
            published_version_id: 1,
            agents: [],
            bound_agents: [],
            active_runs: [],
          },
        }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/skills/alpha') {
        const files = (call.body as { files: Array<{ path: string; content: string }> }).files
        skillContent = files.find(file => file.path === 'SKILL.md')?.content ?? ''
        return { data: skillDetailOf(skillContent), etag: ETAG_A }
      }
      return undefined
    }
    await signIn(route)
    await visit('/console/resources/skills/alpha')

    expect(wrapper!.find('.view-resource-detail').text()).toContain('修改技能')
    const editor = wrapper!.find('textarea[aria-label="文件正文 SKILL.md"]')
    expect(editor.exists()).toBe(true)
    await editor.setValue('# 新版')
    await button('保存新版本').trigger('click')
    await flushPromises()

    const writes = callsTo('PUT', '/api/v1/skills/alpha')
    expect(writes).toHaveLength(1)
    expect(writes[0]?.body).toEqual({ files: [{ path: 'SKILL.md', content: '# 新版' }] })
    expect(writes[0]!.headers['If-Match']).toBe(ETAG_A)
    expect(wrapper!.text()).toContain('已保存并发布新版本')
  })

  it('requires an explicit env decision before replacing an MCP entry', async () => {
    let envKeys = ['TOKEN']
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/mcp-servers/files') {
        return { data: mcpDetailOf(envKeys), etag: ETAG_A }
      }
      if (call.method === 'GET' && call.path === '/api/v1/resources/mcp_server/files/versions') {
        return { data: [] }
      }
      // 详情页新增的只读诊断轴（US-14）：不启动进程，只回三态与 env 布尔。
      if (call.method === 'GET' && call.path === '/api/v1/mcp-servers/files/diagnostics') {
        return {
          data: {
            name: 'files',
            transport: 'stdio',
            published_version_id: 1,
            applied_version_id: 1,
            pending_restart: false,
            command: { state: 'located', path: 'npx', reason: 'lookpath' },
            env: envKeys.map((key) => ({ key, set: true })),
            warnings: [],
            checked_at: '2026-10-22T12:00:00Z',
          },
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/resources/mcp_server/files/references') {
        return {
          data: {
            kind: 'mcp_server',
            id: 'files',
            resource_uid: 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B',
            published_version_id: 1,
            agents: [],
            bound_agents: [],
            active_runs: [],
          },
        }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/mcp-servers/files') {
        envKeys = []
        return { data: mcpDetailOf([]), etag: ETAG_A }
      }
      return undefined
    }
    await signIn(route)
    await visit('/console/resources/mcp-servers/files')

    expect(wrapper!.find('.view-resource-detail').text()).toContain('修改MCP 服务')
    expect(wrapper!.find('.view-resource-detail').text()).toContain('该服务已有环境变量映射（TOKEN）')

    // 详情只回键名；不重填也不勾选清空时必须拒绝，且一个写请求都不发。
    await button('保存修改').trigger('click')
    await flushPromises()
    expect(callsTo('PUT', '/api/v1/mcp-servers/files')).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('请重新填写 KEY=${VAR}')

    await wrapper!.find('#resource-clear-env').setValue(true)
    await button('保存修改').trigger('click')
    await flushPromises()

    const writes = callsTo('PUT', '/api/v1/mcp-servers/files')
    expect(writes).toHaveLength(1)
    const body = writes[0]!.body as Record<string, unknown>
    expect(body.name).toBe('files')
    expect(body.transport).toBe('stdio')
    expect('env' in body).toBe(false)
    expect(writes[0]!.headers['If-Match']).toBe(ETAG_A)
    expect(wrapper!.find('.notice-info').text()).toContain('已保存并发布')
  })
})
