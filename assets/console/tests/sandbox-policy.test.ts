// 执行环境页的信息分层与写入回归（14D / T119）：档位切换（方向随确认框冻结）、
// 禁止域名、内网访问例外与两组待确认项。所有条件写都必须带页面**当前**的 ETag，
// 失败一律重新读取、绝不自动重发。
// The execution-environment page: profile switch with a frozen direction, denylist,
// exceptions and the two pending-confirmation axes. Writes are conditional and are
// never resent automatically.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { markRaw } from 'vue'
import { ApiClient } from '../src/api'
import type {
  ExecutionPolicyView,
  ExecutionProfile,
  McpServerDiagnostics,
  McpServerView,
  NetworkAllowlistView,
  NetworkExceptionListView,
  NetworkExceptionView,
  PendingPolicyConfirmation,
  SandboxView as SandboxViewData,
} from '../src/api'
import { ConsoleAuth } from '../src/auth'
import { ConsoleRouter } from '../src/router'
import { SandboxView } from '../src/views/workspace'

interface Call {
  readonly url: string
  readonly init: RequestInit
}

interface State {
  profile: ExecutionProfile
  allow: string[]
  denied: string[]
  pending: PendingPolicyConfirmation[]
  exceptions: NetworkExceptionView[]
}

const POLICY_ETAG = `"sha256:${'c'.repeat(64)}"`
const DENY_ETAG = `"sha256:${'d'.repeat(64)}"`
const LIST_ETAG = `"sha256:${'e'.repeat(64)}"`

function meta(etag: string, revision: number, source: NetworkAllowlistView['source']) {
  return { etag, revision, source, updated_at: '2026-10-26T08:00:00Z', updated_by: 'token:abc123' }
}

function sandboxOf(state: State, denylistRevision = 4): SandboxViewData {
  return {
    file_allowed_paths: ['.basalt/agents/<agent>/'],
    shell_allowed_commands: ['git'],
    shell_interpreters: ['python3'],
    http_allowed_domains: [...state.allow],
    network_allowlist: meta(LIST_ETAG, 3, 'database'),
    execution_profile: state.profile,
    execution_policy: meta(POLICY_ETAG, 2, 'instance_init'),
    denied_domains: [...state.denied],
    network_denylist: meta(DENY_ETAG, denylistRevision, 'database'),
  }
}

function policyOf(state: State): ExecutionPolicyView {
  return {
    ...meta(POLICY_ETAG, 2, 'instance_init'),
    profile: state.profile,
    exceptions_count: state.exceptions.length,
    protected_addresses: ['10.0.0.0/8', '169.254.169.254'],
    shell_names: ['bash', 'cmd.exe'],
    pending_confirmations: [...state.pending],
  }
}

function exceptionsOf(state: State): NetworkExceptionListView {
  return { etag: LIST_ETAG, exceptions: [...state.exceptions] }
}

function exceptionView(
  id: string,
  kind: NetworkExceptionView['kind'],
  value: string,
  note: string,
): NetworkExceptionView {
  return {
    exception_id: id, kind, value, note, revision: 1,
    created_at: '2026-10-26T08:00:00Z', created_by: 'token:abc123', operation_id: 'op-' + id,
  }
}

function mcpServerOf(overrides: Partial<McpServerView> = {}): McpServerView {
  return {
    name: 'dbhub',
    transport: 'stdio',
    command: 'node',
    env_keys: [],
    header_keys: [],
    credential_keys: [],
    connected: false,
    tools: [],
    published_version_id: 3,
    published_version_no: 2,
    applied_version_id: 3,
    applied_version_no: 2,
    pending_restart: false,
    tool_disposition: null,
    ...overrides,
  }
}

function diagnosticsOf(overrides: Partial<McpServerDiagnostics> = {}): McpServerDiagnostics {
  return {
    name: 'dbhub',
    transport: 'stdio',
    published_version_id: 3,
    applied_version_id: 3,
    pending_restart: false,
    command: { state: 'located', path: '/opt/basalt/runtime/node/node', reason: 'absolute' },
    env: [],
    dependency: { component: 'dbhub', state: 'ready', expected_version: '1.5.0', found_version: '1.5.0' },
    references: { count: 0, agents: [] },
    warnings: ['stdio_child_egress_unconstrained'],
    checked_at: '2026-10-27T08:00:00Z',
    ...overrides,
  }
}

function envelope(data: unknown, status = 200, message = 'success'): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => ({ code: status === 200 ? 'OK' : 'FAILED', message, data, timestamp: '2026-10-26T12:00:00Z' }),
  } as unknown as Response
}

async function mountPage(
  state: State,
  handler?: (call: Call) => Response | null,
  sandboxOverrides?: (view: SandboxViewData) => SandboxViewData,
): Promise<{ wrapper: ReturnType<typeof mount>; calls: Call[] }> {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
    const call: Call = { url: String(url), init }
    calls.push(call)
    const custom = handler?.(call)
    if (custom !== undefined && custom !== null) {
      return custom
    }
    const path = new URL(call.url, 'http://console').pathname
    if (path === '/api/v1/execution-policy') {
      return envelope(policyOf(state))
    }
    if (path === '/api/v1/network-exceptions') {
      return envelope(exceptionsOf(state))
    }
    // 运行环境区块在挂载时读取已发布 MCP 服务；没有服务就是空列表，
    // 不能落回 sandbox 视图（那不是列表）。
    if (path === '/api/v1/mcp-servers') {
      return envelope([])
    }
    const view = sandboxOf(state)
    return envelope(sandboxOverrides ? sandboxOverrides(view) : view)
  }))
  const auth = new ConsoleAuth()
  auth.setToken('console-token')
  const api = new ApiClient(auth)
  const wrapper = mount(SandboxView, {
    props: { ctx: { api: markRaw(api), auth, router: new ConsoleRouter() } },
    attachTo: document.body,
  })
  await flushPromises()
  return { wrapper, calls }
}

function methodOf(call: Call | undefined): string {
  return call?.init.method ?? 'GET'
}

function bodyOf(call: Call): unknown {
  return JSON.parse(String(call.init.body)) as unknown
}

function writable(calls: Call[], path: string): Call[] {
  return calls.filter(call => call.url === path && methodOf(call) !== 'GET')
}

function reads(calls: Call[], path: string): Call[] {
  return calls.filter(call => call.url === path && methodOf(call) === 'GET')
}

function baseState(overrides: Partial<State> = {}): State {
  return { profile: 'standard', allow: ['api.example.com'], denied: [], pending: [], exceptions: [], ...overrides }
}

afterEach(() => {
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('execution policy summary', () => {
  it('states what each layer does without inventing runtime versions', async () => {
    const { wrapper } = await mountPage(baseState())
    const text = wrapper.text()
    expect(text).toContain('本机进程（无容器隔离）')
    expect(text).toContain('标准')
    expect(text).toContain('允许访问未被禁止的公网域名')
    expect(text).toContain('DNS 解析校验')
    expect(text).toContain('运行环境')
    // 没有已配置 MCP 服务时如实说明无法确认，而不是推断"组件未安装"。
    expect(text).toContain('当前没有已发布的 MCP 服务')
    // 没有运行时清点端点：页面不得编造 Node / Python 版本号。
    expect(text).not.toContain('Node 24')
    expect(text).not.toContain('Python 3.12')
    // 「未配置受控解释器」不等于「未安装 Python」：不再用配置数组推断安装状态。
    expect(text).not.toContain('未配置受控解释器')
  })

  it('explains that an empty allowlist does not deny egress in the standard profile', async () => {
    const { wrapper } = await mountPage(baseState({ allow: [] }))
    expect(wrapper.text()).toContain('标准模式仍允许未命中禁止域名的公网访问')
  })

  it('keeps the allowlist inside advanced settings and shows no strict warning in the standard profile', async () => {
    const { wrapper } = await mountPage(baseState({ allow: [] }))
    const advanced = wrapper.find('details.advanced-settings')
    expect(advanced.exists()).toBe(true)
    expect(advanced.find('#network-allowlist-editor').exists()).toBe(true)
    expect(wrapper.find('.strict-empty-warning').exists()).toBe(false)
    // 标准模式不把白名单当主入口：编辑器只在高级设置里，没有顶层白名单卡片。
    const allowlistHeading = wrapper.findAll('h2').find(heading => heading.text() === '网络域名白名单')
    expect(allowlistHeading).toBeDefined()
    expect(allowlistHeading!.element.closest('details.advanced-settings')).not.toBeNull()
  })

  it('warns that this page blocks its own requests when strict has no allowlist and no exception', async () => {
    const { wrapper } = await mountPage(baseState({ profile: 'strict', allow: [], exceptions: [] }))
    const warning = wrapper.find('.strict-empty-warning')
    expect(warning.exists()).toBe(true)
    expect(warning.text()).toContain('受本页策略约束的请求')
    expect(warning.find('a[href="#network-allowlist-editor"]').text()).toBe('配置允许域名')
    expect(warning.find('a[href="#exception-value"]').text()).toBe('添加访问例外')
    // 严格模式下白名单是主入口：独立卡片，不在高级设置里面。
    const allowlistHeading = wrapper.findAll('h2').find(heading => heading.text() === '网络域名白名单')
    expect(allowlistHeading).toBeDefined()
    expect(allowlistHeading!.element.closest('details.advanced-settings')).toBeNull()
  })
})

describe('runtime environment', () => {
  it('shows existence, connectivity and command registration as separate facts', async () => {
    const { wrapper } = await mountPage(baseState(), (call) => {
      const path = new URL(call.url, 'http://console').pathname
      if (path === '/api/v1/mcp-servers') {
        return envelope([mcpServerOf({ name: 'fetch-server', command: 'python3', connected: true })])
      }
      if (path === '/api/v1/mcp-servers/fetch-server/diagnostics') {
        return envelope(diagnosticsOf({
          name: 'fetch-server',
          command: { state: 'located', path: '/opt/basalt/runtime/python/python3', reason: 'absolute' },
          dependency: { component: 'fetch', state: 'ready', expected_version: '2026.10.24', found_version: '2026.10.24' },
          references: { count: 1, agents: ['industry-brief'] },
        }))
      }
      return null
    })
    const text = wrapper.text()
    // 行按已配置的 server 列出；组件名与版本在详情列，不合并成一个真假难辨的结论。
    expect(text).toContain('fetch-server')
    expect(text).toContain('组件 fetch · 期望 2026.10.24')
    expect(text).toContain('Python')
    expect(text).toContain('已就位')
    expect(text).toContain('已连接')
    expect(text).toContain('已登记')
    expect(text).toContain('industry-brief')
    expect(text).not.toContain('会阻断内置 Agent 的问题')
    const link = wrapper.findAll('a').find(anchor => anchor.text() === '查看依赖')
    expect(link).toBeDefined()
    expect(link!.attributes('href')).toBe('/console/resources/mcp-servers/fetch-server')
  })

  it('names the blocked agents and the real fix when a component is missing and the command is unregistered', async () => {
    const { wrapper } = await mountPage(baseState(), (call) => {
      const path = new URL(call.url, 'http://console').pathname
      if (path === '/api/v1/mcp-servers') {
        return envelope([mcpServerOf({ name: 'dbhub', command: 'node', connected: false })])
      }
      if (path === '/api/v1/mcp-servers/dbhub/diagnostics') {
        return envelope(diagnosticsOf({
          name: 'dbhub',
          command: { state: 'not_found', path: '', reason: 'not_found' },
          dependency: { component: 'dbhub', state: 'missing', expected_version: '1.5.0', found_version: '' },
          references: { count: 2, agents: ['ticket-insights', 'biz-daily'] },
        }))
      }
      return null
    })
    const blockers = wrapper.find('.runtime-blockers')
    expect(blockers.exists()).toBe(true)
    const text = blockers.text()
    // 缺件的影响必须点名到 Agent，并说明系统如何补齐（随包提供、运行时不下装）。
    expect(text).toContain('ticket-insights、biz-daily')
    expect(text).toContain('整只不注册')
    expect(text).toContain('不会在运行时下载或安装')
    // 未登记命令的修复入口是实例级配置，而不是本页开关。
    expect(text).toContain('shell.allowed_commands')
    expect(wrapper.text()).toContain('缺失')
    expect(wrapper.text()).toContain('未登记')
  })
})

describe('execution profile switch', () => {
  it('confirms the direction and writes expected_profile with the live ETag', async () => {
    const state = baseState()
    const { wrapper, calls } = await mountPage(state, (call) => {
      if (methodOf(call) === 'PUT' && call.url === '/api/v1/execution-policy') {
        state.profile = 'strict'
        return envelope(policyOf(state))
      }
      return null
    })
    await wrapper.find('#profile-switch').trigger('click')
    const dialog = wrapper.find('.dialog')
    expect(dialog.text()).toContain('切换到严格模式？')
    // 反向切换必须说清收紧的是哪些授权，不能只换标题。
    expect(dialog.text()).toContain('会收紧授权范围')
    expect(dialog.text()).toContain('已发出的请求无法撤回')
    expect(dialog.find('button.primary').text()).toBe('切换为严格模式')

    await dialog.find('button.primary').trigger('click')
    await flushPromises()

    const puts = writable(calls, '/api/v1/execution-policy')
    expect(puts).toHaveLength(1)
    expect((puts[0]!.init.headers as Record<string, string>)['If-Match']).toBe(POLICY_ETAG)
    expect(bodyOf(puts[0]!)).toEqual({ profile: 'strict', expected_profile: 'standard' })
    expect(wrapper.text()).toContain('已切换到严格模式，后续操作按新策略执行')
    // 档位行与 /sandbox 的投影是同一份状态：切换后重读，页面两处不打架。
    expect(reads(calls, '/api/v1/sandbox').length).toBe(2)
    expect(wrapper.find('.dialog').exists()).toBe(false)
  })

  it('spells out what the standard mode allows without implying arbitrary commands', async () => {
    const state = baseState({ profile: 'strict', allow: ['api.example.com'] })
    const { wrapper } = await mountPage(state)
    await wrapper.find('#profile-switch').trigger('click')
    const dialog = wrapper.find('.dialog')
    expect(dialog.text()).toContain('切换到标准模式？')
    expect(dialog.text()).toContain('将允许访问未被禁止的公网域名，并自动授权满足校验条件的随包程序。')
    expect(dialog.text()).toContain('内网访问仍需单独授权，文件访问范围和禁用命令规则保持不变。')
    expect(dialog.text()).toContain('已发出的请求无法撤回。')
    expect(dialog.find('button.primary').text()).toBe('切换为标准模式')
    // 不得暗示标准模式允许执行任意命令。
    expect(dialog.text()).not.toContain('任意命令')
  })

  it('re-reads on a stale direction and never resends the switch', async () => {
    const state = baseState()
    const { wrapper, calls } = await mountPage(state, (call) =>
      methodOf(call) === 'PUT' && call.url === '/api/v1/execution-policy'
        ? envelope({}, 409, '库内档位与页面确认的方向不一致，请刷新后重新确认')
        : null)
    await wrapper.find('#profile-switch').trigger('click')
    await wrapper.find('.dialog button.primary').trigger('click')
    await flushPromises()

    expect(writable(calls, '/api/v1/execution-policy')).toHaveLength(1)
    expect(reads(calls, '/api/v1/execution-policy').length).toBe(2)
    expect(wrapper.text()).toContain('请核对方向后重新确认')
    expect(wrapper.find('.dialog').exists()).toBe(false)
    // 档位没有被页面乐观改写。
    expect(wrapper.find('#profile-switch').text()).toBe('切换到严格…')
  })
})

describe('denied domains', () => {
  it('saves the full list with the denylist ETag', async () => {
    const state = baseState()
    const { wrapper, calls } = await mountPage(state, (call) => {
      if (methodOf(call) === 'PUT' && call.url === '/api/v1/sandbox/denied-domains') {
        state.denied = ['a.example.com', '*.b.example.com']
        return envelope(sandboxOf(state, 5))
      }
      return null
    })
    await wrapper.find('#denied-domains-editor').setValue('a.example.com\n*.b.example.com\n')
    await wrapper.find('#denied-domains-save').trigger('click')
    await flushPromises()

    const puts = writable(calls, '/api/v1/sandbox/denied-domains')
    expect(puts).toHaveLength(1)
    expect((puts[0]!.init.headers as Record<string, string>)['If-Match']).toBe(DENY_ETAG)
    expect(bodyOf(puts[0]!)).toEqual({ domains: ['a.example.com', '*.b.example.com'] })
    expect(wrapper.text()).toContain('当前禁止 2 个域名')
    expect((wrapper.find('#denied-domains-editor').element as HTMLTextAreaElement).value)
      .toBe('a.example.com\n*.b.example.com')
  })

  it('keeps the draft when the denylist write is rejected', async () => {
    const state = baseState()
    const { wrapper, calls } = await mountPage(state, (call) =>
      methodOf(call) === 'PUT' && call.url === '/api/v1/sandbox/denied-domains'
        ? envelope({ current_etag: DENY_ETAG }, 412, '网络禁止域名表已被其他操作修改，请刷新后重试')
        : null)
    await wrapper.find('#denied-domains-editor').setValue('mine.example.com')
    await wrapper.find('#denied-domains-save').trigger('click')
    await flushPromises()

    expect(writable(calls, '/api/v1/sandbox/denied-domains')).toHaveLength(1)
    expect((wrapper.find('#denied-domains-editor').element as HTMLTextAreaElement).value)
      .toBe('mine.example.com')
    expect(wrapper.text()).toContain('你的输入已保留')
  })
})

describe('network exceptions', () => {
  it('adds an exception only with a reason and refreshes every face', async () => {
    const state = baseState()
    const { wrapper, calls } = await mountPage(state, (call) => {
      if (methodOf(call) === 'POST' && call.url === '/api/v1/network-exceptions') {
        const exception = exceptionView('exc_01ABC', 'ip_port', '10.20.0.8:5432', '内部报表库')
        state.exceptions = [exception]
        return envelope({ etag: LIST_ETAG, exception })
      }
      return null
    })
    await wrapper.find('#exception-value').setValue('10.20.0.8:5432')
    await wrapper.find('#exception-note').setValue('内部报表库')
    await wrapper.find('#exception-add').trigger('click')
    await flushPromises()

    const posts = writable(calls, '/api/v1/network-exceptions')
    expect(posts).toHaveLength(1)
    expect(bodyOf(posts[0]!)).toEqual({ kind: 'ip_port', value: '10.20.0.8:5432', note: '内部报表库' })
    expect(wrapper.text()).toContain('已添加例外：10.20.0.8:5432')
    expect(wrapper.text()).toContain('内部报表库')
    // 例外条数在档位视图里：写后必须重读，不能只改本地表格。
    expect(reads(calls, '/api/v1/execution-policy').length).toBe(2)
    expect(reads(calls, '/api/v1/sandbox').length).toBe(2)
  })

  it('deletes with the collection ETag and asks first', async () => {
    const state = baseState({ exceptions: [exceptionView('exc_01ABC', 'host', 'report.internal', '报表')] })
    const { wrapper, calls } = await mountPage(state, (call) => {
      if (methodOf(call) === 'DELETE') {
        state.exceptions = []
        return envelope({ exception_id: 'exc_01ABC', etag: LIST_ETAG, operation_id: 'op-del' })
      }
      return null
    })
    const deleteButton = wrapper.findAll('button').find(button => button.text() === '删除')
    expect(deleteButton).toBeDefined()
    await deleteButton!.trigger('click')
    expect(wrapper.find('.dialog').text()).toContain('删除后下一次授权判定不再放行 report.internal')
    await wrapper.find('.dialog button.primary').trigger('click')
    await flushPromises()

    const deletes = calls.filter(call => methodOf(call) === 'DELETE')
    expect(deletes).toHaveLength(1)
    expect(deletes[0]!.url).toBe('/api/v1/network-exceptions/exc_01ABC')
    expect((deletes[0]!.init.headers as Record<string, string>)['If-Match']).toBe(LIST_ETAG)
    expect(wrapper.text()).toContain('已删除例外：report.internal')
  })
})

describe('pending confirmations', () => {
  it('keeps a shell item denied and removes it from the list', async () => {
    const state = baseState({
      pending: [{ id: 'shell_command:bash', axis: 'shell_command', value: 'bash', reason: 'shell_name', resolved_protected: [] }],
    })
    const { wrapper, calls } = await mountPage(state, (call) => {
      if (methodOf(call) === 'POST' && call.url === '/api/v1/execution-policy/confirmations') {
        state.pending = []
        return envelope({ id: 'shell_command:bash', decision: 'keep_denied', operation_id: 'op-confirm', already_recorded: false })
      }
      return null
    })
    const text = wrapper.text()
    expect(text).toContain('命中交互式 shell 名称')
    expect(text).not.toContain('加例外（放行）')

    const keep = wrapper.findAll('button').find(button => button.text() === '保持拒绝')
    expect(keep).toBeDefined()
    await keep!.trigger('click')
    await flushPromises()

    const posts = writable(calls, '/api/v1/execution-policy/confirmations')
    expect(posts).toHaveLength(1)
    expect(bodyOf(posts[0]!)).toEqual({ id: 'shell_command:bash', decision: 'keep_denied' })
    expect(wrapper.text()).toContain('已记录「保持拒绝」：bash')
    expect(wrapper.text()).not.toContain('待确认项')
  })

  it('turns a protected-address item into a host exception with a reason', async () => {
    const state = baseState({
      pending: [{
        id: 'network_allowlist:internal.example', axis: 'network_allowlist', value: 'internal.example',
        reason: 'protected_address', resolved_protected: ['10.20.0.8'],
      }],
    })
    const { wrapper, calls } = await mountPage(state, (call) => {
      if (methodOf(call) === 'POST' && call.url === '/api/v1/network-exceptions') {
        state.pending = []
        state.exceptions = [exceptionView('exc_02DEF', 'host', 'internal.example', '内网报表')]
        return envelope({ etag: LIST_ETAG, exception: state.exceptions[0] })
      }
      return null
    })
    expect(wrapper.text()).toContain('解析到受保护地址：10.20.0.8')
    const allow = wrapper.findAll('button').find(button => button.text() === '加例外（放行）')
    expect(allow).toBeDefined()
    await allow!.trigger('click')
    await wrapper.find('input[id^="pending-note-"]').setValue('内网报表')
    await wrapper.find('#pending-exception-confirm').trigger('click')
    await flushPromises()

    const posts = writable(calls, '/api/v1/network-exceptions')
    expect(posts).toHaveLength(1)
    expect(bodyOf(posts[0]!)).toEqual({ kind: 'host', value: 'internal.example', note: '内网报表' })
    expect(wrapper.text()).toContain('已为 internal.example 添加主机名例外')
    expect(wrapper.text()).not.toContain('待确认项')
  })
})
