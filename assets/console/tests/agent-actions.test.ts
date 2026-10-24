// US-12 页面操作闭环回归：新建入口可达、复制提交与跳转、取消确认零请求、
// 归档成功跳转与重新读取、409 AGENT_BUSY 点名 Run、重复点击保护、恢复后落到停用详情。
// 只经真实视图与路由触发，不直接调用 api-admin.ts 的方法。
// Page-operation regressions for US-12: reachable entries, copy submit/navigate,
// cancel sends nothing, archive navigate + re-read, AGENT_BUSY run ids, repeat-click
// guards and the restore landing page. Everything goes through real views and the
// router; api-admin.ts methods are never called directly.
import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
  readonly body: unknown
}

interface Reply {
  readonly status?: number
  readonly code?: string
  readonly message?: string
  readonly data?: unknown
}

type Route = (call: Call) => Reply | Promise<Reply> | undefined

const summary = {
  version: 'test-version', uptime_seconds: 3600, started_at: '2026-10-21T12:00:00Z',
  agents: { total: 1, active: 1, disabled: 0, archived: 0, invalid: 0 },
  runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
  resources: { model_connections: 0, mcp_servers: 0, skills: 0 },
  database: { status: 'ok', schema_version: 2 }, auth: { mode: 'token', loopback_only: true },
}

let wrapper: ReturnType<typeof mount> | undefined
let calls: Call[] = []
let profilesData: unknown[] = []

/** Agent 列表 / 详情的生命周期夹具；state 同时决定 runnable。 */
// Profile fixture used by list and detail views; state decides runnable.
function profileOf(name: string, state: 'active' | 'disabled' = 'disabled'): Record<string, unknown> {
  return {
    name,
    display_name: '',
    description: name + ' description',
    provider: 'demo',
    model: 'demo-model',
    tools: [],
    skills: [],
    notify_channels: [],
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

const EXCLUSIVE_MODEL_UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'
const SHARED_MODEL_UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B'
const SHARED_CHANNEL_UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D'

/** 模型连接列表夹具：UID 是复制映射的来源身份，范围决定是否有资格当替代资源。 */
// Model connection fixtures; scope decides whether a resource can be a mapping target.
function modelConnectionOf(
  name: string,
  uid: string,
  overrides: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    name,
    resource_uid: uid,
    base_url: 'https://api.example.com',
    api_key_env: 'DEMO_API_KEY',
    default_model: 'demo-model',
    credential_mode: 'env',
    credential_ref: '',
    credential_state: '',
    credential_version: 0,
    last4: '',
    scope: 'shared',
    owner_agent: '',
    revision: 1,
    published_version_id: 3,
    published_version_no: 3,
    applied_version_id: 3,
    applied_version_no: 3,
    etag: '"sha256:' + 'c'.repeat(64) + '"',
    referenced_by: [],
    ...overrides,
  }
}

/** 配置视图夹具：阻断响应只点名，「来源 UID」只能从这里取（不按名重解析）。 */
// The config view is the only source for the blocked reference's resource_uid.
function configViewOf(): Record<string, unknown> {
  return {
    agent: 'writer',
    agent_uid: 'ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3E',
    content_etag: '"sha256:' + 'b'.repeat(64) + '"',
    fields: {
      name: 'writer',
      description: 'writer description',
      display_name: '',
      identity: { agent_name: 'writer', prompt: 'you write' },
      provider: { name: 'exclusive-model', model: 'demo-model', temperature: null },
      tools: [],
      skills: [],
      mcp_servers: [],
      bootstrap: [],
      notify_mode: 'all_eligible',
      notify_channels: [],
      resource_bindings: { model_connections: [], notify_channels: [] },
      settings: { max_iterations: 8, max_history_turns: 12 },
      schedules: [],
    },
    notify_mode: 'all_eligible',
    notify_mode_source: 'config',
    references: [
      {
        resource_kind: 'model_connection',
        ref_name: 'exclusive-model',
        resource_uid: EXCLUSIVE_MODEL_UID,
        resource_name: 'exclusive-model',
        binding_status: 'bound',
        published: true,
        eligible: false,
        deleted: false,
        credential_state: 'active',
      },
    ],
    binding_complete: true,
  }
}

/** 写端点成功后返回的 AgentFilesView。 */
function filesViewOf(name: string, state: 'active' | 'disabled' = 'disabled'): Record<string, unknown> {
  const profile = profileOf(name, state)
  return {
    agent: name,
    lifecycle: profile.lifecycle,
    etag: '"sha256:' + 'a'.repeat(64) + '"',
    files: [],
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
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
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

function callsTo(method: string, path: string): Call[] {
  return calls.filter(call => call.method === method && call.path === path)
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolveFn: (value: T) => void = () => {}
  const promise = new Promise<T>(resolve => { resolveFn = resolve })
  return { promise, resolve: resolveFn }
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

describe('agent page operations', () => {
  it('exposes create and archived entries from the agents list header', async () => {
    profilesData = [profileOf('writer', 'active')]
    await signIn()
    await visit('/console/agents')
    expect(wrapper!.find('.page-actions').text()).toContain('查看已归档')

    await button('新建 Agent').trigger('click')
    await flushPromises()
    expect(window.location.pathname).toBe('/console/agents/new')
    expect(wrapper!.find('.page-heading h1').text()).toBe('新建 Agent')

    await visit('/console/agents')
    await button('查看已归档').trigger('click')
    await flushPromises()
    expect(window.location.pathname).toBe('/console/archived')
    expect(wrapper!.find('.page-heading h1').text()).toBe('已归档 Agents')
  })

  it('flags a declared notify channel without the notify tool on the card', async () => {
    profilesData = [
      { ...profileOf('silent', 'active'), notify_channels: ['feishu-ops'], tools: ['save_memory'] },
      { ...profileOf('sender', 'active'), notify_channels: ['feishu-ops'], tools: ['notify'] },
    ]
    await signIn()
    await visit('/console/agents')

    const cards = wrapper!.findAll('.agent-card')
    const silent = cards.find(card => card.text().includes('silent'))
    const sender = cards.find(card => card.text().includes('sender'))
    expect(silent, '两张卡片都应渲染').toBeTruthy()
    expect(sender, '两张卡片都应渲染').toBeTruthy()
    // R7：该提示只描述"下次保存会自动装配"，不再声称"不会发送"——系统代投不依赖它。
    expect(silent!.text()).toContain('tools 里还没有 notify')
    expect(silent!.text()).toContain('系统自动装配')
    expect(sender!.text()).not.toContain('tools 里还没有 notify')
  })

  it('shows the display name on the card while the technical name stays visible', async () => {
    profilesData = [
      { ...profileOf('ops-agent', 'active'), display_name: '值班小助手' },
      profileOf('writer', 'active'),
    ]
    await signIn()
    await visit('/console/agents')

    const card = wrapper!.findAll('.agent-card').find(item => item.text().includes('值班小助手'))
    expect(card, '显示名应出现在卡片上').toBeTruthy()
    const heading = card!.find('h2')
    expect(heading.find('a').text()).toBe('值班小助手')
    // 技术名仍在旁边：显示名不是身份，跳转与 API 继续用技术名。
    expect(heading.find('code').text()).toBe('ops-agent')
    expect(heading.find('a').attributes('href')).toBe('/console/agents/ops-agent')

    // 搜索同时匹配显示名与技术名，两处都能定位同一条目。
    await wrapper!.find('input[type="search"]').setValue('值班')
    expect(wrapper!.findAll('.agent-card')).toHaveLength(1)
    await wrapper!.find('input[type="search"]').setValue('ops-agent')
    expect(wrapper!.findAll('.agent-card')).toHaveLength(1)
  })

  it('offers the same create entry from the empty state', async () => {
    profilesData = []
    await signIn()
    await visit('/console/agents')
    expect(wrapper!.find('.empty').text()).toContain('工作区中还没有 Agent')
    await wrapper!.find('.empty button').trigger('click')
    await flushPromises()
    expect(window.location.pathname).toBe('/console/agents/new')
  })

  it('links the list card to the config route and keeps one config tab in the detail', async () => {
    profilesData = [profileOf('writer', 'active')]
    await signIn()
    await visit('/console/agents')

    const cardLink = wrapper!.find('.agent-card-bottom a[aria-label="配置 Agent writer"]')
    expect(cardLink.text()).toBe('配置')
    expect(cardLink.attributes('href')).toBe('/console/agents/writer/config')

    await visit('/console/agents/writer')
    // 统一入口：页头不再有第二个“结构化配置”链接，配置在页签内完成。
    const headerLinks = wrapper!.findAll('a').filter(item => item.text().trim() === '结构化配置')
    expect(headerLinks).toHaveLength(0)
    const tabs = wrapper!.findAll('.tabs button').map(item => item.text())
    expect(tabs).toEqual(['会话', '运行', '配置', '记忆', '定时'])
  })

  it('copies the configuration and lands on the disabled copy', async () => {
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path === '/api/v1/agents/writer/copy') {
        return { data: filesViewOf('writer-copy') }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active'), profileOf('writer-copy', 'disabled')]
    await signIn(route)
    await visit('/console/agents/writer')

    await button('复制配置').trigger('click')
    const dialog = wrapper!.find('.dialog')
    expect(dialog.text()).toContain('默认停用')
    expect(dialog.text()).toContain('定时任务清空')
    await dialog.find('input').setValue('writer-copy')
    await dialogButton('确认复制').trigger('click')
    await flushPromises()

    const copyCalls = callsTo('POST', '/api/v1/agents/writer/copy')
    expect(copyCalls).toHaveLength(1)
    expect(copyCalls[0]?.body).toEqual({ new_name: 'writer-copy' })
    expect(window.location.pathname).toBe('/console/agents/writer-copy')
    expect(window.location.search).toBe('?copied=1')
    expect(wrapper!.find('.page-heading h1').text()).toBe('writer-copy')
    expect(wrapper!.find('.notice-info').text()).toContain('默认停用')
    expect(wrapper!.find('.agent-title').text()).toContain('停用')
  })

  it('asks for an explicit shared replacement when the copy is blocked by exclusive references', async () => {
    let copies = 0
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path === '/api/v1/agents/writer/copy') {
        copies += 1
        if (copies === 1) {
          return {
            status: 409,
            code: 'AGENT_COPY_BLOCKED',
            message: '此 Agent 使用了 1 个专属模型连接和 0 个专属通知渠道。',
            // 契约只点名，不带 UID。
            data: { problems: ['model_connection exclusive-model'] },
          }
        }
        return { data: filesViewOf('writer-copy') }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/writer/config') {
        return { data: configViewOf() }
      }
      if (call.method === 'GET' && call.path === '/api/v1/model-connections') {
        return {
          data: [
            modelConnectionOf('exclusive-model', EXCLUSIVE_MODEL_UID, { scope: 'exclusive', owner_agent: 'writer' }),
            modelConnectionOf('shared-model', SHARED_MODEL_UID),
            // 未发布的共享连接不能当替代：副本准入时会被拒。
            modelConnectionOf('draft-model', 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3F', {
              published_version_id: 0,
              published_version_no: 0,
            }),
          ],
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/notify-channels') {
        return {
          data: [{
            name: 'shared-alerts',
            resource_uid: SHARED_CHANNEL_UID,
            type: 'webhook',
            description: '',
            url_host: 'hooks.example.com',
            url_configured: true,
            address_status: 'env',
            credential_mode: 'env',
            credential_ref: '',
            credential_state: '',
            credential_version: 0,
            last4: '',
            scope: 'shared',
            owner_agent: '',
            revision: 1,
            etag: '"sha256:' + 'd'.repeat(64) + '"',
            referenced_by: [],
          }],
        }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active'), profileOf('writer-copy', 'disabled')]
    await signIn(route)
    await visit('/console/agents/writer')

    await button('复制配置').trigger('click')
    await wrapper!.find('.dialog').find('input').setValue('writer-copy')
    await dialogButton('确认复制').trigger('click')
    await flushPromises()

    // 409 → 映射向导：点名来源引用，说明计数，候选只列共享且已发布的资源。
    let dialog = wrapper!.find('.dialog')
    expect(dialog.text()).toContain('专属引用需要显式映射')
    expect(dialog.text()).toContain('1 个专属模型连接和 0 个专属通知渠道')
    expect(dialog.text()).toContain('模型连接「exclusive-model」')
    expect(dialog.text()).toContain('只列出工作区共享的资源')
    let select = dialog.find('select[data-blocked="model_connection:exclusive-model"]')
    expect(select.findAll('option').map(option => option.text())).toEqual([
      '请选择共享资源',
      'shared-model（rs_01J8Z…）',
    ])
    expect(callsTo('POST', '/api/v1/agents/writer/copy')).toHaveLength(1)

    // 未选替代资源 → 本地拦下，不发第二次请求。
    await dialogButton('确认复制').trigger('click')
    await flushPromises()
    expect(callsTo('POST', '/api/v1/agents/writer/copy')).toHaveLength(1)
    dialog = wrapper!.find('.dialog')
    expect(dialog.find('.notice-error').text()).toContain('请为每个专属引用选择替代的共享资源')

    // 选定后重试：映射用来源 UID（配置视图）与目标 UID（共享资源），不经名称重解析。
    select = dialog.find('select[data-blocked="model_connection:exclusive-model"]')
    await select.setValue(SHARED_MODEL_UID)
    await dialogButton('确认复制').trigger('click')
    await flushPromises()

    const postCalls = callsTo('POST', '/api/v1/agents/writer/copy')
    expect(postCalls).toHaveLength(2)
    expect(postCalls[1]?.body).toEqual({
      new_name: 'writer-copy',
      resource_mapping: [{
        resource_kind: 'model_connection',
        source_resource_uid: EXCLUSIVE_MODEL_UID,
        target_resource_uid: SHARED_MODEL_UID,
      }],
    })
    expect(window.location.pathname).toBe('/console/agents/writer-copy')
    expect(window.location.search).toBe('?copied=1')
  })

  it('retries the profile read while a copied agent is still registering', async () => {
    let profileReads = 0
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path === '/api/v1/agents/writer/copy') {
        return { data: filesViewOf('writer-copy') }
      }
      if (call.method === 'GET' && call.path === '/api/v1/profiles') {
        profileReads += 1
        // 详情页首次读取（第 2 次）还看不到新 Agent；之后才出现，模拟注册表延迟。
        const copyVisible = profileReads >= 3
        return {
          data: copyVisible
            ? [profileOf('writer', 'active'), profileOf('writer-copy', 'disabled')]
            : [profileOf('writer', 'active')],
        }
      }
      return undefined
    }
    await signIn(route)
    await visit('/console/agents/writer')

    await button('复制配置').trigger('click')
    await wrapper!.find('.dialog').find('input').setValue('writer-copy')
    await dialogButton('确认复制').trigger('click')
    await flushPromises()
    expect(window.location.pathname).toBe('/console/agents/writer-copy')
    // 注册表尚未刷新时到达页不得把刚复制的 Agent 报成「未找到」。
    expect(wrapper!.text()).not.toContain('未找到 Agent')

    await vi.waitFor(() => {
      expect(wrapper!.find('.page-heading h1').text()).toBe('writer-copy')
    }, { timeout: 4000, interval: 50 })
    expect(wrapper!.find('.notice-info').text()).toContain('默认停用')
    expect(wrapper!.find('.agent-title').text()).toContain('停用')
  })

  it('sends no request when the copy or archive dialog is cancelled', async () => {
    profilesData = [profileOf('writer', 'active')]
    await signIn()
    await visit('/console/agents/writer')

    await button('复制配置').trigger('click')
    await dialogButton('取消').trigger('click')
    expect(wrapper!.find('.dialog').exists()).toBe(false)

    await button('移出工作区（可恢复）').trigger('click')
    expect(wrapper!.find('.dialog').text()).toContain('新运行与消息会被拒绝')
    await dialogButton('取消').trigger('click')
    expect(wrapper!.find('.dialog').exists()).toBe(false)
    expect(calls.filter(call => call.method !== 'GET')).toEqual([])
  })

  it('archives, returns to the list and re-reads the profiles', async () => {
    let archived = false
    const route: Route = (call) => {
      if (call.method === 'DELETE' && call.path === '/api/v1/agents/writer') {
        archived = true
        return {
          data: {
            archive_id: 'writer-20261022-120000',
            agent: 'writer',
            archived_at: '2026-10-22T12:00:00Z',
            content_hash: 'abc123',
            restored_at: '',
            restored_agent_name: '',
          },
        }
      }
      if (call.method === 'GET' && call.path === '/api/v1/profiles') {
        return { data: archived ? [] : [profileOf('writer', 'active')] }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')
    const readsBefore = callsTo('GET', '/api/v1/profiles').length

    await button('移出工作区（可恢复）').trigger('click')
    const dialog = wrapper!.find('.dialog')
    expect(dialog.text()).toContain(
      '移出后：新运行与消息会被拒绝，定时任务注销；正在运行的任务会被请求取消',
    )
    expect(dialog.text()).toContain('会话、消息、记忆、审计与定时执行历史都保留')
    expect(dialog.text()).toContain('恢复后它默认处于停用状态、定时任务已清空')
    await dialogButton('移出工作区').trigger('click')
    await flushPromises()

    expect(callsTo('DELETE', '/api/v1/agents/writer')).toHaveLength(1)
    expect(window.location.pathname).toBe('/console/agents')
    expect(window.location.search).toBe('?archived=1')
    expect(wrapper!.find('.notice-info').text()).toContain('已归档该 Agent')
    expect(callsTo('GET', '/api/v1/profiles').length).toBeGreaterThan(readsBefore)
    expect(wrapper!.find('.empty').text()).toContain('工作区中还没有 Agent')
  })

  it('keeps the page and names the busy runs on AGENT_BUSY', async () => {
    const route: Route = (call) => {
      if (call.method === 'DELETE' && call.path === '/api/v1/agents/writer') {
        return {
          status: 409,
          code: 'AGENT_BUSY',
          message: 'Agent 仍有在途 Run，等待上限内未退出',
          data: { run_ids: ['run-alpha', 'run-beta'] },
        }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active')]
    await signIn(route)
    await visit('/console/agents/writer')
    const readsBefore = callsTo('GET', '/api/v1/profiles').length

    await button('移出工作区（可恢复）').trigger('click')
    await dialogButton('移出工作区').trigger('click')
    await flushPromises()

    expect(window.location.pathname).toBe('/console/agents/writer')
    expect(wrapper!.find('.page-heading h1').text()).toBe('writer')
    expect(wrapper!.find('.dialog').exists()).toBe(false)
    const alert = wrapper!.find('.notice-error').text()
    expect(alert).toContain('run-alpha')
    expect(alert).toContain('run-beta')
    expect(alert).toContain('稍后重试')
    expect(callsTo('GET', '/api/v1/profiles').length).toBeGreaterThan(readsBefore)
  })

  it('does not resubmit a pending copy or archive request', async () => {
    const pendingCopy = deferred<Reply>()
    const copyRoute: Route = (call) => {
      if (call.method === 'POST' && call.path === '/api/v1/agents/writer/copy') {
        return pendingCopy.promise
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'active'), profileOf('writer-copy', 'disabled')]
    await signIn(copyRoute)
    await visit('/console/agents/writer')
    await button('复制配置').trigger('click')
    await wrapper!.find('.dialog').find('input').setValue('writer-copy')

    const copyConfirm = dialogButton('确认复制')
    void copyConfirm.trigger('click')
    void copyConfirm.trigger('click')
    await flushPromises()
    expect(callsTo('POST', '/api/v1/agents/writer/copy')).toHaveLength(1)
    expect(copyConfirm.attributes('disabled')).toBeDefined()

    pendingCopy.resolve({ data: filesViewOf('writer-copy') })
    await flushPromises()
    expect(window.location.pathname).toBe('/console/agents/writer-copy')

    const pendingArchive = deferred<Reply>()
    const archiveRoute: Route = (call) => {
      if (call.method === 'DELETE' && call.path === '/api/v1/agents/writer-copy') {
        return pendingArchive.promise
      }
      return undefined
    }
    installFetch(archiveRoute)
    await visit('/console/agents/writer-copy')
    await button('移出工作区（可恢复）').trigger('click')

    const archiveConfirm = dialogButton('移出工作区')
    void archiveConfirm.trigger('click')
    void archiveConfirm.trigger('click')
    await flushPromises()
    expect(callsTo('DELETE', '/api/v1/agents/writer-copy')).toHaveLength(1)
    expect(archiveConfirm.attributes('disabled')).toBeDefined()

    pendingArchive.resolve({
      data: {
        archive_id: 'writer-copy-20261022-120000',
        agent: 'writer-copy',
        archived_at: '2026-10-22T12:00:00Z',
        content_hash: 'abc123',
        restored_at: '',
        restored_agent_name: '',
      },
    })
    await flushPromises()
    expect(window.location.pathname).toBe('/console/agents')
  })

  it('restores an archived agent and lands on its disabled detail', async () => {
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/archived-agents') {
        return {
          data: [{
            archive_id: 'writer-20261022-120000',
            agent: 'writer',
            archived_at: '2026-10-22T12:00:00Z',
            content_hash: 'abc123',
            restored_at: '',
            restored_agent_name: '',
          }],
        }
      }
      if (call.method === 'POST' && call.path === '/api/v1/archived-agents/writer-20261022-120000/restore') {
        return { data: filesViewOf('writer', 'disabled') }
      }
      return undefined
    }
    profilesData = [profileOf('writer', 'disabled')]
    await signIn(route)
    await visit('/console/archived')

    await button('恢复').trigger('click')
    const panel = wrapper!.findAll('section.card').find(item => item.text().includes('恢复归档'))
    expect(panel, '恢复面板应出现').toBeTruthy()
    expect(panel!.text()).toContain('原名恢复')
    const confirm = panel!.findAll('button').find(item => item.text().startsWith('确认恢复为'))
    expect(confirm, '确认恢复按钮应可达').toBeTruthy()
    await confirm!.trigger('click')
    await flushPromises()

    const restoreCalls = callsTo('POST', '/api/v1/archived-agents/writer-20261022-120000/restore')
    expect(restoreCalls).toHaveLength(1)
    expect(restoreCalls[0]?.body).toEqual({})
    expect(window.location.pathname).toBe('/console/agents/writer')
    expect(window.location.search).toBe('?restored=1')
    expect(wrapper!.find('.page-heading h1').text()).toBe('writer')
    expect(wrapper!.find('.notice-info').text()).toContain('默认停用')
    expect(wrapper!.find('.agent-title').text()).toContain('停用')
  })
})

/** 作者草稿夹具：只有路径与正文，草稿不落盘。 Author draft: paths and bodies only. */
function draftOf(files: Array<{ path: string; content: string }>): Reply {
  return { data: { agent: 'draft', files } }
}

describe('one-sentence agent creation', () => {
  it('hands the one-liner to the create page without creating anything', async () => {
    profilesData = [profileOf('writer', 'active')]
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path.endsWith('/generate-files')) {
        return draftOf([{ path: 'AGENT.md', content: '# agent\n' }])
      }
      return undefined
    }
    await signIn(route)
    await visit('/console/agents')

    expect(wrapper!.find('#one-liner-create').text()).toContain('一句话创建 Agent')
    expect(wrapper!.find('#agent-one-liner-start').attributes('disabled')).toBeDefined()

    const description = '每天早上汇总 GitHub Trending 并发送到飞书'
    await wrapper!.find('#agent-one-liner').setValue(description)
    await button('生成草稿并预览').trigger('click')
    await flushPromises()

    expect(window.location.pathname).toBe('/console/agents/new')
    const params = new URLSearchParams(window.location.search)
    expect(params.get('description')).toBe(description)
    expect(params.get('autodraft')).toBe('1')
    // 入口只导航：创建端点一次都没有请求，创建仍要用户在新建页确认。
    expect(callsTo('POST', '/api/v1/agents')).toHaveLength(0)
  })

  it('prefills the description, authors one draft and previews it', async () => {
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path.endsWith('/generate-files')) {
        return draftOf([
          { path: 'AGENT.md', content: '# 日报 Agent\n' },
          { path: 'skills/daily.md', content: '汇总步骤\n' },
        ])
      }
      return undefined
    }
    profilesData = []
    await signIn(route)
    const description = '每天早上汇总 GitHub Trending'
    await visit('/console/agents/new?description=' + encodeURIComponent(description) + '&autodraft=1')

    const nameInput = wrapper!.find('#agent-name').element as HTMLInputElement
    expect(nameInput.value).toMatch(/^agent-[0-9a-z]+$/)
    expect((wrapper!.find('#agent-description').element as HTMLTextAreaElement).value).toBe(description)

    // 自动起草只发生一次，名字建议是确定性的：请求路径里的名字就是输入框里的值。
    const generates = calls.filter(call => call.method === 'POST' && call.path.endsWith('/generate-files'))
    expect(generates).toHaveLength(1)
    expect(generates[0]?.path).toBe(`/api/v1/agents/${nameInput.value}/generate-files`)
    expect(generates[0]?.body).toEqual({ description })

    const preview = wrapper!.findAll('section.card').find(item => item.text().includes('结构化预览'))
    expect(preview, '结构化预览应出现').toBeTruthy()
    expect(preview!.text()).toContain(nameInput.value)
    expect(preview!.text()).toContain('AGENT.md')
    expect(preview!.text()).toContain('skills/daily.md')
    expect(preview!.text()).toContain('停用')
  })

  it('reports a failed creation without claiming the agent exists', async () => {
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path === '/api/v1/agents') {
        return { status: 409, code: 'AGENT_EXISTS', message: '同名 Agent 已存在' }
      }
      return undefined
    }
    profilesData = []
    await signIn(route)
    await visit('/console/agents/new')

    await wrapper!.find('#agent-name').setValue('writer')
    await wrapper!.find('#agent-description').setValue('写日报')
    await button('创建 Agent').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.notice-error').text()).toContain('同名 Agent 已存在')
    expect(wrapper!.find('.view-agents-new').text()).toContain('创建没有发生')
    expect(wrapper!.find('.view-agents-new').text()).not.toContain('已创建（默认停用）')
  })

  it('binds the selected model connection when submitting the drafted files', async () => {
    const body = '你是日报 Agent。汇总 GitHub Trending 并输出三行。'
    const draftContent = `---\nname: ops-new\nprovider:\n  name: deepseek\n  model: demo-model\n---\n\n${body}\n`
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/model-connections') {
        return {
          data: [
            modelConnectionOf('deepseek', SHARED_MODEL_UID, { default_model: 'demo-model' }),
            modelConnectionOf('private-x', EXCLUSIVE_MODEL_UID, {
              scope: 'exclusive', owner_agent: 'writer', default_model: 'private-model',
            }),
            modelConnectionOf('draft-only', 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C', {
              published_version_id: 0, published_version_no: 0,
            }),
          ],
        }
      }
      if (call.method === 'POST' && call.path.endsWith('/generate-files')) {
        return draftOf([{ path: 'AGENT.md', content: draftContent }])
      }
      if (call.method === 'POST' && call.path === '/api/v1/agents') {
        return { data: filesViewOf('ops-new', 'disabled') }
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/ops-new/content') {
        return { data: { content: draftContent, etag: '"sha256:' + 'd'.repeat(64) + '"' } }
      }
      if (call.method === 'PUT' && call.path === '/api/v1/agents/ops-new/content') {
        return { data: filesViewOf('ops-new', 'disabled') }
      }
      return undefined
    }
    profilesData = [profileOf('ops-new', 'disabled')]
    await signIn(route)
    await visit('/console/agents/new?description=' + encodeURIComponent('汇总 GitHub Trending') + '&autodraft=1')

    // 只列已发布且有资格的共享连接；专属与未发布连接可见但不可选，并说明原因。
    const select = wrapper!.find('#agent-model-connection')
    expect(select.exists()).toBe(true)
    const options = select.findAll('option')
    expect(options.map(option => option.text())).toEqual([
      '（请选择模型连接）',
      'deepseek（默认模型 demo-model）',
    ])
    expect((select.element as HTMLSelectElement).value).toBe(SHARED_MODEL_UID)
    const text = wrapper!.find('.view-agents-new').text()
    expect(text).toContain('private-x（专属 · writer）')
    expect(text).toContain('draft-only（未发布）')
    expect((wrapper!.find('#agent-model').element as HTMLInputElement).value).toBe('demo-model')

    await button('创建 Agent').trigger('click')
    await flushPromises()
    await button('提交文件').trigger('click')
    await flushPromises()

    const updates = callsTo('PUT', '/api/v1/agents/ops-new/content')
    expect(updates).toHaveLength(1)
    expect(updates[0]?.body).toEqual({
      changes: {
        body,
        provider: { name: 'deepseek', model: 'demo-model' },
        bindings: [{
          resource_kind: 'model_connection',
          ref_name: 'deepseek',
          resource_uid: SHARED_MODEL_UID,
        }],
      },
    })
    // 草稿只有 AGENT.md：不再发多文件提交（否则会用没有绑定的原文覆盖结果）。
    expect(callsTo('POST', '/api/v1/agents/ops-new/files')).toHaveLength(0)
    expect(wrapper!.find('.view-agents-new').text()).toContain('草稿文件已提交')
  })

  it('blocks draft submission without a published shared connection', async () => {
    const route: Route = (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/model-connections') {
        return {
          data: [
            modelConnectionOf('private-x', EXCLUSIVE_MODEL_UID, {
              scope: 'exclusive', owner_agent: 'writer', default_model: 'private-model',
            }),
          ],
        }
      }
      if (call.method === 'POST' && call.path.endsWith('/generate-files')) {
        return draftOf([{ path: 'AGENT.md', content: '---\nname: ops-new\n---\n\n正文\n' }])
      }
      if (call.method === 'POST' && call.path === '/api/v1/agents') {
        return { data: filesViewOf('ops-new', 'disabled') }
      }
      return undefined
    }
    profilesData = []
    await signIn(route)
    await visit('/console/agents/new?description=' + encodeURIComponent('写日报') + '&autodraft=1')
    await button('创建 Agent').trigger('click')
    await flushPromises()

    expect(wrapper!.find('#agent-model-connection').attributes('disabled')).toBeDefined()
    expect(wrapper!.find('.view-agents-new').text()).toContain('没有可用于新 Agent 的模型连接')
    const submit = wrapper!.findAll('button').find(item => item.text().trim() === '提交文件')
    expect(submit?.attributes('disabled')).toBeDefined()
    expect(callsTo('PUT', '/api/v1/agents/ops-new/content')).toHaveLength(0)
  })

  it('separates created from submitted so a partial state is repaired, not recreated', async () => {
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path === '/api/v1/agents') {
        return { data: filesViewOf('ops-new', 'disabled') }
      }
      return undefined
    }
    profilesData = [profileOf('ops-new', 'disabled')]
    await signIn(route)
    await visit('/console/agents/new')

    await wrapper!.find('#agent-name').setValue('ops-new')
    await wrapper!.find('#agent-description').setValue('写日报')
    await button('创建 Agent').trigger('click')
    await flushPromises()

    const text = wrapper!.find('.view-agents-new').text()
    expect(text).toContain('草稿尚未提交')
    expect(text).toContain('不要重复创建')
    const creates = callsTo('POST', '/api/v1/agents')
    expect(creates).toHaveLength(1)
    expect(creates[0]?.body).toEqual({ name: 'ops-new', description: '写日报' })
  })
})

/** 追问卡按可见标题定位；返回 undefined 表示当前没有追问。 */
function questionsCard() {
  return wrapper!.findAll('section.card').find(item => item.text().includes('作者需要补充信息'))
}

describe('author questions', () => {
  it('asks for missing details once, then drafts from the accumulated answers', async () => {
    let authorCalls = 0
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path.endsWith('/generate-files')) {
        authorCalls += 1
        if (authorCalls === 1) {
          return {
            data: {
              agent: 'draft',
              files: [],
              questions: [{ id: 'frequency', question: '多久运行一次？' }],
            },
          }
        }
        return draftOf([{ path: 'AGENT.md', content: '# 日报 Agent\n' }])
      }
      return undefined
    }
    profilesData = []
    await signIn(route)
    const description = '早上汇总 GitHub Trending'
    await visit('/console/agents/new?description=' + encodeURIComponent(description) + '&autodraft=1')

    const card = questionsCard()
    expect(card, '追问卡应出现').toBeTruthy()
    expect(card!.text()).toContain('多久运行一次？')
    // 未回答前不能继续；第一轮只有问题，没有生成任何草稿。
    expect(card!.find('#draft-answer-continue').attributes('disabled')).toBeDefined()
    expect(wrapper!.find('.view-agents-new').text()).not.toContain('结构化预览')

    await card!.find('#draft-answer-frequency').setValue('每天早上 8:30')
    expect(card!.find('#draft-answer-continue').attributes('disabled')).toBeUndefined()
    await card!.find('#draft-answer-continue').trigger('click')
    await flushPromises()

    const generates = calls.filter(call => call.method === 'POST' && call.path.endsWith('/generate-files'))
    expect(generates).toHaveLength(2)
    // 第二次请求带上全部答案；问题原文来自服务端，页面不改写。
    expect(generates[1]?.body).toEqual({
      description,
      answers: [{ id: 'frequency', question: '多久运行一次？', answer: '每天早上 8:30' }],
    })
    expect(questionsCard()).toBeUndefined()
    expect(wrapper!.find('.view-agents-new').text()).toContain('AGENT.md')
    // 问答本身不创建任何东西：创建仍要用户单独确认。
    expect(callsTo('POST', '/api/v1/agents')).toHaveLength(0)
  })

  it('stops after two question rounds instead of looping on the author model', async () => {
    let authorCalls = 0
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path.endsWith('/generate-files')) {
        authorCalls += 1
        return {
          data: {
            agent: 'draft',
            files: [],
            questions: [{ id: 'round' + authorCalls, question: '第 ' + authorCalls + ' 轮还需要什么信息？' }],
          },
        }
      }
      return undefined
    }
    profilesData = []
    await signIn(route)
    await visit('/console/agents/new?description=' + encodeURIComponent('写日报') + '&autodraft=1')

    for (const round of [1, 2]) {
      const card = questionsCard()
      expect(card, '第 ' + round + ' 轮追问应出现').toBeTruthy()
      await card!.find('#draft-answer-round' + round).setValue('回答 ' + round)
      await card!.find('#draft-answer-continue').trigger('click')
      await flushPromises()
    }

    // 第三轮仍是问题 → 停止，不无限追问；错误给出可执行的下一步。
    expect(authorCalls).toBe(3)
    expect(questionsCard()).toBeUndefined()
    expect(wrapper!.find('.notice-error').text()).toContain('作者模型连续 2 轮仍在追问')
    expect(wrapper!.find('.view-agents-new').text()).not.toContain('结构化预览')
  })

  it('discards the answers without another author call and re-enables drafting', async () => {
    let authorCalls = 0
    const route: Route = (call) => {
      if (call.method === 'POST' && call.path.endsWith('/generate-files')) {
        authorCalls += 1
        return {
          data: {
            agent: 'draft',
            files: [],
            questions: [{ id: 'frequency', question: '多久运行一次？' }],
          },
        }
      }
      return undefined
    }
    profilesData = []
    await signIn(route)
    await visit('/console/agents/new?description=' + encodeURIComponent('写日报') + '&autodraft=1')

    const card = questionsCard()
    expect(card, '追问卡应出现').toBeTruthy()
    await card!.find('#draft-answer-frequency').setValue('随便写')
    await card!.find('#draft-answer-discard').trigger('click')
    await flushPromises()

    // 放弃只在内存里清空：不发请求、不产生草稿，主按钮恢复可用。
    expect(authorCalls).toBe(1)
    expect(questionsCard()).toBeUndefined()
    const generate = wrapper!.findAll('button').find(item => item.text().trim() === '生成草稿')
    expect(generate?.attributes('disabled')).toBeUndefined()
    expect(wrapper!.find('.view-agents-new').text()).not.toContain('结构化预览')
  })
})
