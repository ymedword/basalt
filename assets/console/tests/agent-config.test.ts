// Agent 结构化配置页回归：模型连接按 UID 选择并把「名称 + UID」显式放进
// changes.bindings（页面不编辑 resource_bindings 原文，也不按名自动补绑）、
// 就地为当前 Agent 新建连接 / 渠道并回填绑定（创建与绑定是两步，第二步失败
// 明确说「尚未写入配置」）、正文走 changes.body（清空需二次确认）、资源绑定
// 分区只读并给出管理入口、候选读取失败时拒绝保存、412 只换 ETag 保留输入。
// Agent config regressions: connections are selected by uid and submitted as
// explicit bindings, inline creation binds the new resource (a failed second step
// says "not yet written"), the body is saved through changes.body with a second
// confirmation for clearing it, the bindings card is read-only with management
// links, an unloaded candidate list blocks the save, and 412 keeps user input.
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type {
  AgentConfigFields,
  AgentConfigReference,
  AgentConfigView,
  ModelConnectionView,
  NotifyChannelView,
} from '../src/api'
import { createConsoleApp } from '../src/app'

const CONFIG_ETAG = `"sha256:${'a'.repeat(64)}"`
const NEW_ETAG = `"sha256:${'b'.repeat(64)}"`
const UID_MODEL = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'
const UID_BACKUP = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B'
const UID_CHANNEL = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C'
const UID_NEW = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D'
const SECRET = 'sk-live-0123456789abcdef'
const DEFAULT_CONTENT = '---\nname: writer\n---\n你是报告作者。\n'

/** 一条已绑定的模型连接引用；备份引用默认未绑定。 */
function reference(overrides: Partial<AgentConfigReference> = {}): AgentConfigReference {
  return {
    resource_kind: 'model_connection',
    ref_name: 'deepseek',
    resource_uid: UID_MODEL,
    resource_name: 'deepseek',
    binding_status: 'bound',
    published: true,
    eligible: true,
    deleted: false,
    credential_state: 'active',
    ...overrides,
  }
}

function backupReference(): AgentConfigReference {
  return reference({
    ref_name: 'backup',
    resource_uid: '',
    resource_name: '',
    binding_status: 'missing',
    credential_state: '',
  })
}

function channelReference(): AgentConfigReference {
  return reference({
    resource_kind: 'notify_channel',
    ref_name: 'ops',
    resource_uid: UID_CHANNEL,
    resource_name: 'ops',
    credential_state: '',
  })
}

function fields(overrides: Partial<AgentConfigFields> = {}): AgentConfigFields {
  return {
    name: 'writer',
    description: '旧描述',
    display_name: '',
    identity: { agent_name: 'Writer', prompt: '你是报告作者。' },
    provider: { name: 'deepseek', model: 'deepseek-chat', temperature: null },
    tools: ['read_file'],
    skills: [],
    mcp_servers: [],
    bootstrap: [],
    notify_mode: 'none',
    notify_channels: [],
    resource_bindings: {
      model_connections: [{ name: 'deepseek', uid: UID_MODEL }],
      notify_channels: [],
    },
    settings: { max_iterations: 8, max_history_turns: 20 },
    schedules: [
      { id: 'daily', cron: '0 0 9 * * *', zone: 'Asia/Shanghai', message: '生成日报', deliver_to: '' },
    ],
    ...overrides,
  }
}

function configView(overrides: Partial<AgentConfigView> = {}): AgentConfigView {
  return {
    agent: 'writer',
    agent_uid: 'ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A',
    content_etag: CONFIG_ETAG,
    fields: fields(),
    notify_mode: 'none',
    notify_mode_source: 'explicit',
    references: [reference(), backupReference()],
    binding_complete: false,
    ...overrides,
  }
}

function modelView(overrides: Partial<ModelConnectionView> = {}): ModelConnectionView {
  return {
    name: 'deepseek',
    resource_uid: UID_MODEL,
    base_url: 'https://api.deepseek.com',
    api_key_env: '${DEEPSEEK_API_KEY}',
    default_model: 'deepseek-chat',
    credential_mode: 'env',
    credential_ref: '',
    credential_state: '',
    credential_version: 0,
    last4: '',
    scope: 'shared',
    owner_agent: '',
    revision: 1,
    published_version_id: 1,
    published_version_no: 1,
    applied_version_id: 1,
    applied_version_no: 1,
    etag: `"sha256:${'c'.repeat(64)}"`,
    referenced_by: ['writer'],
    availability: 'ready',
    last_test: null,
    ...overrides,
  }
}

function channelView(overrides: Partial<NotifyChannelView> = {}): NotifyChannelView {
  return {
    name: 'ops',
    resource_uid: UID_CHANNEL,
    type: 'webhook',
    description: '运维群',
    url_host: 'open.feishu.cn',
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
    etag: `"sha256:${'d'.repeat(64)}"`,
    referenced_by: [],
    availability: 'ready',
    last_test: null,
    ...overrides,
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

/** 服务端真值：每个测试只覆写自己关心的部分，未匹配请求记入 unexpected。 */
interface Fixture {
  config?: AgentConfigView
  content?: string
  models?: readonly ModelConnectionView[]
  channels?: readonly NotifyChannelView[]
  tools?: readonly unknown[]
  skills?: readonly unknown[]
  mcpServers?: readonly unknown[]
  types?: readonly string[]
}

const infoView = {
  version: 'test',
  uptime_seconds: 1,
  agents: 1,
  tools: 0,
  mcp_servers: 0,
  providers: [],
}

/** 实例加密就绪视图：配置页内嵌的模型连接表单读它决定「加密托管」是否可选。 */
// Instance-encryption ready view; it only gates the managed credential option.
const instanceSecretsView = {
  state: 'ready',
  key_id: 'k_test000001',
  initialized_at: '2026-10-20T08:00:00Z',
  key_file: '/var/lib/basalt/secrets/instance.key',
  recovery_reason: '',
  can_initialize: false,
  credential_summary: { managed: 0, managed_revoked: 0, env: 0, missing: 0 },
}

function baseReply(fixture: Fixture, call: RecordedCall): Reply | undefined {
  if (call.path.endsWith('/info')) {
    return { data: infoView }
  }
  if (call.path === '/api/v1/instance/secrets' && call.method === 'GET') {
    return { data: instanceSecretsView }
  }
  if (call.path === '/api/v1/agents/writer/config' && call.method === 'GET') {
    return { data: fixture.config ?? configView(), etag: CONFIG_ETAG }
  }
  if (call.path === '/api/v1/agents/writer/content' && call.method === 'GET') {
    return { data: { content: fixture.content ?? DEFAULT_CONTENT, etag: CONFIG_ETAG } }
  }
  if (call.path === '/api/v1/model-connections' && call.method === 'GET') {
    return {
      data: fixture.models ?? [
        modelView(),
        modelView({ name: 'backup', resource_uid: UID_BACKUP }),
      ],
    }
  }
  if (call.path === '/api/v1/notify-channels' && call.method === 'GET') {
    return { data: fixture.channels ?? [] }
  }
  if (call.path === '/api/v1/notify-channels/types') {
    return { data: fixture.types ?? ['webhook'] }
  }
  // 工具 / 技能 / MCP 目录：默认空目录，页面降级为手动填写而不是发错请求。
  if (call.path === '/api/v1/tools' && call.method === 'GET') {
    return { data: fixture.tools ?? [] }
  }
  if (call.path === '/api/v1/skills' && call.method === 'GET') {
    return { data: fixture.skills ?? [] }
  }
  if (call.path === '/api/v1/mcp-servers' && call.method === 'GET') {
    return { data: fixture.mcpServers ?? [] }
  }
  return undefined
}

let calls: RecordedCall[] = []
let unexpected: string[] = []
let wrapper: ReturnType<typeof mount> | undefined

/** 记录并分派；未预期的请求既记入 unexpected，也返回 500 让页面显式报错。 */
function setupFetch(
  fixture: Fixture = {},
  extra?: (call: RecordedCall) => Reply | undefined,
): void {
  calls = []
  unexpected = []
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
      const result = extra?.(call) ?? baseReply(fixture, call)
      if (result === undefined) {
        unexpected.push(`${call.method} ${call.path}`)
      }
      const status = result?.status ?? (result === undefined ? 500 : 200)
      const responseHeaders = new Headers()
      if (result?.etag !== undefined) {
        responseHeaders.set('ETag', result.etag)
      }
      return {
        ok: status >= 200 && status < 300,
        status,
        headers: responseHeaders,
        json: async () => ({
          code: result?.code ?? (result === undefined ? 'INTERNAL' : 'OK'),
          message: result?.message ?? (result === undefined ? '未预期请求' : ''),
          data: result?.data ?? null,
          timestamp: '2026-10-23T00:00:00Z',
        }),
      } as Response
    }),
  )
}

/** 登录并把 `next` 直接指向配置页，避免先渲染总览。 */
async function openConfig(): Promise<void> {
  window.history.replaceState(null, '', '/console/login?next=%2Fconsole%2Fagents%2Fwriter%2Fconfig')
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

function contentCalls(): RecordedCall[] {
  return calls.filter(
    (call) => call.method === 'PUT' && call.path === '/api/v1/agents/writer/content',
  )
}

/** 内容写成功响应（契约 AgentFilesView）：changes 恒存在，默认空 = 无系统增删。 */
function savedAgent(agent = 'writer'): Reply {
  return {
    data: {
      agent,
      lifecycle: {},
      etag: CONFIG_ETAG,
      files: [],
      changes: { tools_added: [], tools_removed: [] },
    },
  }
}

function found(selector: string): DOMWrapper<Element> {
  return wrapper!.find(selector)
}

function buttonOf(label: string): DOMWrapper<Element> {
  const foundButton = wrapper!.findAll('button').find((item) => item.text() === label)
  if (foundButton === undefined) {
    throw new Error(`找不到按钮「${label}」`)
  }
  return foundButton
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
  expect(unexpected).toEqual([])
})

describe('agent config page', () => {
  it('lists every published eligible connection and shows bindings read-only with management links', async () => {
    setupFetch({
      models: [
        modelView(),
        modelView({ name: 'backup', resource_uid: UID_BACKUP }),
        // 未发布与属于其他 Agent 的连接都不可选。
        modelView({ name: 'draft', resource_uid: UID_NEW, published_version_id: 0, published_version_no: 0 }),
        modelView({ name: 'reviewer-only', resource_uid: 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3E', scope: 'exclusive', owner_agent: 'reviewer' }),
      ],
    })
    await openConfig()

    expect(wrapper!.find('h1').text()).toBe('writer')
    expect((found('#config-description').element as HTMLInputElement).value).toBe('旧描述')
    // 模型连接是可搜索的选择器：当前绑定永远可见，其余只列已发布且有资格的。
    const selectedRow = wrapper!.find(`#config-provider button[data-uid="${UID_MODEL}"]`)
    expect(selectedRow.exists()).toBe(true)
    expect(selectedRow.classes()).toContain('selected')
    expect(selectedRow.text()).toContain('当前')
    const rows = wrapper!.findAll('#config-provider button[data-uid]')
    const uids = rows.map((row) => row.attributes('data-uid'))
    expect(uids).toContain(UID_BACKUP)
    expect(uids).not.toContain(UID_NEW)
    expect(uids).not.toContain('rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3E')
    expect(wrapper!.text()).toContain('deepseek-chat')

    // 搜索只收窄可选行，不动当前绑定。
    await found('#config-provider-search').setValue('不存在')
    expect(
      wrapper!.findAll('#config-provider button[data-uid]').map((row) => row.attributes('data-uid')),
    ).toEqual([UID_MODEL])
    await found('#config-provider-search').setValue('')

    // 资源绑定分区只读：没有选择框，只有状态与「资源详情 / 渠道管理」入口。
    expect(wrapper!.find('select.binding-select').exists()).toBe(false)
    expect(wrapper!.text()).toContain('管理入口')
    expect(wrapper!.text()).toContain('已关联')
    expect(wrapper!.text()).toContain('未关联')
    const detail = wrapper!.find('a[href="/console/resources/model-connections/deepseek"]')
    expect(detail.exists()).toBe(true)
    expect(detail.text()).toBe('资源详情')
  })

  // 显示名只负责展示：写入 / 清空 frontmatter 的 display_name，不改目录与 API 身份。
  // display_name is presentation only; saving or clearing it never renames the agent.
  it('saves and clears the display name without touching the technical name', async () => {
    const config = configView()
    setupFetch({ config }, (call) => {
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        const changes = (call.body as { changes: { display_name?: string } }).changes
        config.fields.display_name = changes.display_name ?? config.fields.display_name
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    expect((found('#config-display-name').element as HTMLInputElement).value).toBe('')
    await found('#config-display-name').setValue('值班小助手')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    expect(contentCalls()[0]!.body).toEqual({ changes: { display_name: '值班小助手' } })
    // 页头仍是技术名：显示名不改目录、API 路径与运行身份。
    expect(wrapper!.find('h1').text()).toBe('writer')

    // 清空 = 显式移除该行；省略字段才表示「不改」。
    await found('#config-display-name').setValue('')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()
    expect(contentCalls()[1]!.body).toEqual({ changes: { display_name: '' } })
  })

  it('submits the chosen connection name and uid together, never the raw binding block', async () => {
    setupFetch({
      models: [
        modelView(),
        modelView({ name: 'backup', resource_uid: UID_BACKUP, default_model: 'backup-chat' }),
      ],
    }, (call) => {
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    await found(`#config-provider button[data-uid="${UID_BACKUP}"]`).trigger('click')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    const puts = contentCalls()
    expect(puts).toHaveLength(1)
    expect(puts[0]!.ifMatch).toBe(CONFIG_ETAG)
    expect(puts[0]!.body).toEqual({
      changes: {
        provider: { name: 'backup', model: 'backup-chat' },
        bindings: [{ resource_kind: 'model_connection', ref_name: 'backup', resource_uid: UID_BACKUP }],
      },
    })
    expect(wrapper!.text()).toContain('已保存')
  })

  it('creates an exclusive connection inline and binds it after publishing', async () => {
    const models: ModelConnectionView[] = [modelView()]
    setupFetch({ models }, (call) => {
      if (call.path === '/api/v1/model-connections' && call.method === 'POST') {
        const created = modelView({
          name: 'writer-private',
          resource_uid: UID_NEW,
          base_url: 'https://api.example.com/v1',
          default_model: 'private-model',
          credential_mode: 'managed',
          credential_state: 'active',
          credential_version: 1,
          last4: 'cdef',
          scope: 'exclusive',
          owner_agent: 'writer',
          published_version_no: 1,
        })
        models.push(created)
        return { data: created }
      }
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    await buttonOf('为当前 Agent 新建连接').trigger('click')
    await found('#config-new-connection-name').setValue('writer-private')
    await found('#config-new-connection-base-url').setValue('https://api.example.com/v1')
    await found('#config-new-connection-model').setValue('private-model')
    // 默认托管模式：密码框录入完整 Key，提交后页面不留存。
    const keyInput = found('#config-new-connection-key')
    expect(keyInput.attributes('type')).toBe('password')
    await keyInput.setValue(SECRET)
    await buttonOf('创建连接并关联').trigger('click')
    await flushPromises()

    const created = calls.find((call) => call.method === 'POST')
    expect(created?.body).toEqual({
      name: 'writer-private',
      base_url: 'https://api.example.com/v1',
      default_model: 'private-model',
      api_key: SECRET,
      credential_mode: 'managed',
      scope: 'exclusive',
      owner_agent: 'writer',
    })
    const puts = contentCalls()
    expect(puts).toHaveLength(1)
    expect(puts[0]!.body).toEqual({
      changes: {
        provider: { name: 'writer-private', model: 'private-model' },
        bindings: [
          { resource_kind: 'model_connection', ref_name: 'writer-private', resource_uid: UID_NEW },
        ],
      },
    })
    expect(wrapper!.text()).toContain('模型连接「writer-private」已创建并写入 Agent 配置的资源关联')
    // 密钥只出现在请求体里：页面正文没有任何回显载体。
    expect(wrapper!.text()).not.toContain(SECRET)
  })

  it('keeps the inline-created connection and says it is not yet written when the save fails', async () => {
    const models: ModelConnectionView[] = [modelView()]
    setupFetch({ models }, (call) => {
      if (call.path === '/api/v1/model-connections' && call.method === 'POST') {
        const created = modelView({
          name: 'writer-private',
          resource_uid: UID_NEW,
          scope: 'exclusive',
          owner_agent: 'writer',
        })
        models.push(created)
        return { data: created }
      }
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return {
          status: 400,
          code: 'AGENT_RESOURCE_BINDING_INVALID',
          message: '绑定形状非法',
        }
      }
      return undefined
    })
    await openConfig()

    await buttonOf('为当前 Agent 新建连接').trigger('click')
    await found('#config-new-connection-name').setValue('writer-private')
    await found('#config-new-connection-base-url').setValue('https://api.example.com/v1')
    await found('#config-new-connection-model').setValue('private-model')
    await found('#config-new-connection-key').setValue(SECRET)
    await buttonOf('创建连接并关联').trigger('click')
    await flushPromises()

    const error = wrapper!.find('.notice-error').text()
    expect(error).toContain('模型连接「writer-private」已创建，但尚未写入 Agent 配置')
    expect(error).toContain('资源关联形状非法、重复、是孤儿或指向旧资源')
  })

  it('creates a notify channel inline, adds it to the allowlist and binds it', async () => {
    const channels: NotifyChannelView[] = []
    setupFetch({ channels, types: ['webhook'] }, (call) => {
      if (call.path === '/api/v1/notify-channels' && call.method === 'POST') {
        const created = channelView({
          name: 'writer-alerts',
          resource_uid: UID_NEW,
          credential_mode: 'managed',
          credential_state: 'active',
          credential_version: 1,
          last4: '9f2c',
          address_status: 'managed_configured',
          scope: 'exclusive',
          owner_agent: 'writer',
        })
        channels.push(created)
        return { data: created }
      }
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    await buttonOf('为当前 Agent 新建渠道').trigger('click')
    await found('#config-new-channel-name').setValue('writer-alerts')
    await found('#config-new-channel-type').setValue('webhook')
    await found('#config-new-channel-url').setValue(
      'https://hooks.example.com/services/x',
    )
    await buttonOf('创建渠道并关联').trigger('click')
    await flushPromises()

    const created = calls.find((call) => call.method === 'POST')
    expect(created?.body).toEqual({
      name: 'writer-alerts',
      type: 'webhook',
      description: '',
      credential_mode: 'managed',
      url: 'https://hooks.example.com/services/x',
      scope: 'exclusive',
      owner_agent: 'writer',
    })
    const puts = contentCalls()
    expect(puts).toHaveLength(1)
    expect(puts[0]!.body).toEqual({
      changes: {
        notify_mode: 'allowlist',
        notify_channels: ['writer-alerts'],
        bindings: [
          { resource_kind: 'model_connection', ref_name: 'deepseek', resource_uid: UID_MODEL },
          { resource_kind: 'notify_channel', ref_name: 'writer-alerts', resource_uid: UID_NEW },
        ],
      },
    })
    expect(wrapper!.text()).toContain('通知渠道「writer-alerts」已创建并写入 Agent 配置的资源关联')
    expect(wrapper!.text()).not.toContain('hooks.example.com/services')
  })

  it('saves the edited body and requires a second click to clear it', async () => {
    setupFetch({}, (call) => {
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    const bodyField = found('#config-body')
    expect((bodyField.element as HTMLTextAreaElement).value).toBe('你是报告作者。')
    await bodyField.setValue('新的执行规则')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()
    expect(contentCalls()[0]!.body).toEqual({ changes: { body: '新的执行规则' } })

    // 清空正文：第一次点击只提示，第二次才提交空串（不可逆，页面必须二次确认）。
    await found('#config-body').setValue('')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()
    expect(wrapper!.text()).toContain('正文将被清空')
    expect(contentCalls()).toHaveLength(1)

    await buttonOf('保存配置').trigger('click')
    await flushPromises()
    expect(contentCalls()).toHaveLength(2)
    expect(contentCalls()[1]!.body).toEqual({ changes: { body: '' } })
  })

  it('disables body editing when the frontmatter fences cannot be parsed', async () => {
    setupFetch({ content: 'name: writer\n---\n正文' })
    await openConfig()

    expect(wrapper!.find('#config-body').exists()).toBe(false)
    expect(wrapper!.text()).toContain('无法解析 AGENT.md 的 frontmatter 围栏')
  })

  it('blocks the save locally when a declared connection has no binding', async () => {
    setupFetch({
      config: configView({
        references: [reference({ resource_uid: '', resource_name: '', binding_status: 'missing' })],
        fields: fields({
          resource_bindings: { model_connections: [], notify_channels: [] },
        }),
      }),
    })
    await openConfig()

    await found('#config-description').setValue('新描述')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    // 服务端同样会拒；页面先在本地拦下，零写入请求。
    expect(contentCalls()).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('配置引用缺少对应的资源关联')
  })

  it('refuses to save when the candidate lists never loaded', async () => {
    setupFetch({}, (call) => {
      if (call.path === '/api/v1/model-connections' && call.method === 'GET') {
        return { status: 500, code: 'INTERNAL', message: '读取失败' }
      }
      return undefined
    })
    await openConfig()

    expect(wrapper!.text()).toContain('读取资源候选失败')
    await found('#config-description').setValue('新描述')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    expect(contentCalls()).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('资源候选尚未读取成功')
  })

  it('keeps the input after 412 and refreshes the ETag for the next save', async () => {
    let contentReads = 0
    let patched = false
    setupFetch({}, (call) => {
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'GET') {
        contentReads += 1
        // 412 之后的重读给出前移的 ETag；正文本身仍是服务端真值。
        return {
          data: { content: DEFAULT_CONTENT, etag: contentReads > 1 ? NEW_ETAG : CONFIG_ETAG },
        }
      }
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT' && !patched) {
        patched = true
        return { status: 412, code: 'PRECONDITION_FAILED', message: 'ETag 不匹配' }
      }
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    await found('#config-description').setValue('本地草稿')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    expect(wrapper!.text()).toContain('AGENT.md 已被他人修改')
    expect((found('#config-description').element as HTMLInputElement).value).toBe(
      '本地草稿',
    )

    await buttonOf('保存配置').trigger('click')
    await flushPromises()
    const puts = contentCalls()
    expect(puts).toHaveLength(2)
    expect(puts[0]!.ifMatch).toBe(CONFIG_ETAG)
    // 第二次用的是重新读到的 ETag，而不是页面启动时的旧值。
    expect(puts[1]!.ifMatch).toBe(NEW_ETAG)
  })

  it('turns the allowlist off with an empty channel list and drops the notify binding', async () => {
    setupFetch({
      config: configView({
        fields: fields({
          notify_mode: 'allowlist',
          notify_channels: ['ops'],
          resource_bindings: {
            model_connections: [{ name: 'deepseek', uid: UID_MODEL }],
            notify_channels: [{ name: 'ops', uid: UID_CHANNEL }],
          },
        }),
        notify_mode: 'allowlist',
        references: [reference(), channelReference()],
        binding_complete: true,
      }),
      channels: [channelView()],
    }, (call) => {
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    expect((found('#config-notify-mode-allowlist').element as HTMLInputElement).checked).toBe(true)
    const ops = wrapper!.find('input[data-name="ops"]')
    expect(ops.exists()).toBe(true)
    expect((ops.element as HTMLInputElement).checked).toBe(true)

    await found('#config-notify-mode-none').setValue()
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    const put = contentCalls()
    expect(put).toHaveLength(1)
    expect(put[0]!.body).toEqual({
      changes: {
        notify_mode: 'none',
        notify_channels: [],
        // 退出 allowlist 后声明渠道不再是引用：绑定整块随之收窄。
        bindings: [
          { resource_kind: 'model_connection', ref_name: 'deepseek', resource_uid: UID_MODEL },
        ],
      },
    })
  })

  it('requires at least one channel when switching to allowlist', async () => {
    setupFetch({ channels: [channelView()] })
    await openConfig()

    await found('#config-notify-mode-allowlist').setValue()
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    expect(contentCalls()).toHaveLength(0)
    expect(wrapper!.text()).toContain('仅使用所选渠道时要至少选择一个通知渠道')
  })

  it('links a revoked credential to the resource page', async () => {
    setupFetch({
      config: configView({ references: [reference({ credential_state: 'revoked' })] }),
      models: [modelView({ credential_state: 'revoked' })],
    })
    await openConfig()

    expect(wrapper!.text()).toContain('凭据已撤销')
    expect(wrapper!.text()).toContain('到资源页重新录入凭据')
    const links = wrapper!.findAll('a[href="/console/resources/model-connections/deepseek"]')
    expect(links.length).toBeGreaterThan(0)
  })

  it('restores the provider default temperature by submitting null', async () => {
    setupFetch({
      config: configView({
        fields: fields({
          provider: { name: 'deepseek', model: 'deepseek-chat', temperature: 0.7 },
        }),
      }),
    }, (call) => {
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    expect((found('#config-temperature-custom').element as HTMLInputElement).checked).toBe(true)
    expect((found('#config-temperature').element as HTMLInputElement).value).toBe('0.7')
    await found('#config-temperature-restore').trigger('click')
    expect((found('#config-temperature-default').element as HTMLInputElement).checked).toBe(true)
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    // 「使用模型默认值」= 移除字段（null），不是写入 0、也不是省略后保留旧值。
    expect(contentCalls()[0]!.body).toEqual({ changes: { provider: { temperature: null } } })
  })

  it('writes an explicit custom temperature including zero', async () => {
    setupFetch({}, (call) => {
      if (call.path === '/api/v1/agents/writer/content' && call.method === 'PUT') {
        return savedAgent()
      }
      return undefined
    })
    await openConfig()

    await found('#config-temperature-custom').setValue()
    await found('#config-temperature').setValue('0')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()
    expect(contentCalls()[0]!.body).toEqual({ changes: { provider: { temperature: 0 } } })
  })

  it('blocks an empty custom temperature instead of silently keeping the old value', async () => {
    setupFetch({
      config: configView({
        fields: fields({
          provider: { name: 'deepseek', model: 'deepseek-chat', temperature: 0.7 },
        }),
      }),
    })
    await openConfig()

    await found('#config-temperature').setValue('')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    expect(contentCalls()).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('自定义输出随机性需要填写 0 到 2 之间的数字')
  })

  it('sends nothing when there is no change', async () => {
    setupFetch()
    await openConfig()

    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    expect(contentCalls()).toHaveLength(0)
    expect(wrapper!.text()).toContain('没有改动需要保存')
  })

  // 表单与原文共用同一份 ETag：任一方有未保存修改时，另一方保存都会整份覆盖，
  // 所以两条保存路径都要先点名「另一边还没保存」，而不是静默丢弃草稿。
  // Form and raw share one ETag; either save would discard the other draft.
  it('blocks the form save while the raw editor holds unsaved changes', async () => {
    setupFetch()
    await openConfig()

    await found('#config-raw-toggle').trigger('click')
    await found('#config-raw').setValue(DEFAULT_CONTENT + '\n补充一行。\n')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    expect(contentCalls()).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('配置原文有未保存的修改')
  })

  it('blocks the raw save while the form holds unsaved changes and keeps both drafts', async () => {
    setupFetch()
    await openConfig()

    await found('#config-description').setValue('新描述')
    await found('#config-raw-toggle').trigger('click')
    // 原文也改过（否则「保存原文」按钮本就禁用，测不到跨区守卫）。
    // The raw draft must differ too, or the save button is disabled for lack of changes.
    await found('#config-raw').setValue(DEFAULT_CONTENT + '\n补充一行。\n')
    await found('#config-raw-save').trigger('click')
    await flushPromises()

    expect(contentCalls()).toHaveLength(0)
    expect(wrapper!.text()).toContain('表单有未保存的修改')
    expect((found('#config-raw').element as HTMLTextAreaElement).value).toBe(DEFAULT_CONTENT + '\n补充一行。\n')
    expect((found('#config-description').element as HTMLInputElement).value).toBe('新描述')
  })

  // R7：系统按通知意图自动增删的受控工具增量随保存结果回显，两条保存路径都要
  // 显示这次关系变化；空增量不显示任何提示。
  // R7: the system-applied tool delta is surfaced after either save path.
  it('shows the system-applied tool delta after both save paths', async () => {
    let saved = 0
    setupFetch({}, (call) => {
      if (call.method === 'PUT' && call.path === '/api/v1/agents/writer/content') {
        saved += 1
        return {
          data: {
            agent: 'writer',
            lifecycle: {},
            etag: CONFIG_ETAG,
            files: [],
            changes:
              saved === 1
                ? { tools_added: ['notify'], tools_removed: [] }
                : { tools_added: [], tools_removed: ['notify'] },
          },
        }
      }
      return undefined
    })
    await openConfig()

    await found('#config-description').setValue('新描述')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()
    expect(wrapper!.find('.notice-info').text()).toContain(
      '系统已按通知意图自动调整 tools：加入 notify。',
    )

    await found('#config-raw-toggle').trigger('click')
    await found('#config-raw').setValue(DEFAULT_CONTENT + '\n补充一行。\n')
    await found('#config-raw-save').trigger('click')
    await flushPromises()
    expect(wrapper!.text()).toContain(
      '已保存原文；表单已从服务端重新读取。系统已按通知意图自动调整 tools：移除 notify。',
    )
  })

  it('does not claim a tool delta when the save changed no tools', async () => {
    setupFetch({}, (call) =>
      call.method === 'PUT' && call.path === '/api/v1/agents/writer/content'
        ? savedAgent()
        : undefined,
    )
    await openConfig()

    await found('#config-description').setValue('新描述')
    await buttonOf('保存配置').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.notice-info').text()).toContain('已保存；配置与资源关联已从服务端重新读取。')
    expect(wrapper!.text()).not.toContain('系统已按通知意图自动调整 tools')
  })

  it('previews the notify relationship without predicting channel eligibility', async () => {
    setupFetch({
      config: configView({
        notify_mode: 'allowlist',
        fields: fields({ notify_mode: 'allowlist', notify_channels: ['ops'] }),
      }),
      channels: [channelView()],
    })
    await openConfig()

    expect(wrapper!.text()).toContain(
      '保存时系统会按通知意图自动把 notify 加入 tools（渠道存在且可用时）；系统只会自动增删 notify 这一个工具。',
    )
    // 保存页脚的口径：装配只增删 notify，不改其它工具。
    expect(wrapper!.text()).toContain('保存时系统按通知意图自动装配 tools 里的 notify')
  })

  it('states when notify is already in tools and that none removes it', async () => {
    setupFetch({
      config: configView({
        notify_mode: 'allowlist',
        fields: fields({ notify_mode: 'allowlist', notify_channels: ['ops'], tools: ['read_file', 'notify'] }),
      }),
      channels: [channelView()],
    })
    await openConfig()
    expect(wrapper!.text()).toContain(
      'notify 已在 tools 中：模型可在运行中主动调用；定时任务配置了结果投递渠道时，该轮运行由系统发送，不向模型提供 notify。',
    )
    wrapper!.unmount()

    setupFetch({
      config: configView({
        notify_mode: 'none',
        fields: fields({ tools: ['read_file', 'notify'] }),
      }),
    })
    await openConfig()
    expect(wrapper!.text()).toContain(
      '通知意图为 none：保存时系统会从 tools 移除 notify，运行中不向模型提供该工具。',
    )
  })
})
