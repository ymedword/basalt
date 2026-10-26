// 通知渠道写面回归：目标服务三态（host / 环境变量引用 / 未配置）与文档引用列、
// 创建提交精确占位并让父页面重读、明文地址在本地拒绝（零写请求）、
// 被引用删除需二次确认才发 force=true、修改时地址留空不提交 url（保留已登记值）。
// US-12 修订追加：UID / 范围 / 所有者 / 凭据状态 / 地址状态列，范围切换与托管凭据
// 撤销走资源端点（行内 etag + resource_uid），412 只提示并让父页面重读。
// Regressions for the notify-channel component: three-state target rendering plus
// document references, placeholder create with a parent re-read, local plaintext
// rejection with zero write requests, two-step force confirmation for referenced
// deletes, and blank URL on update. The US-12 revision adds the scope / credential /
// address-status columns and the resource-endpoint writes with the row's etag.
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { markRaw } from 'vue'
import { ApiClient } from '../src/api'
import type { NotifyChannelView } from '../src/api'
import { ConsoleAuth } from '../src/auth'
import { NotifyChannels } from '../src/components/notify-channels'

const TYPES_PATH = '/api/v1/notify-channels/types'
const CHANNELS_PATH = '/api/v1/notify-channels'
const RESOURCES_PATH = '/api/v1/resources/notify_channel'
/** 删除与范围 / 撤销写面必须携带的资源身份（契约 `resource_uid`）。 */
const CHANNEL_UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'
/** 资源条件写的 If-Match 来源（行内 etag）。 */
const CHANNEL_ETAG = '"sha256:' + 'a'.repeat(64) + '"'

interface Call {
  method: string
  path: string
  query: string
  body: unknown
  ifMatch: string
}

type Handler = (call: Call) => Response | undefined

let wrapper: ReturnType<typeof mount> | undefined

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

/** 让数组下标在 noUncheckedIndexedAccess 下有明确失败信息。 */
function required<T>(value: T | undefined, what: string): T {
  if (value === undefined) {
    throw new Error(`找不到${what}`)
  }
  return value
}

function envelope(status: number, code: string, message: string, data?: unknown): Response {
  const body: Record<string, unknown> = {
    code,
    message,
    timestamp: '2026-10-22T08:00:00Z',
  }
  if (data !== undefined) {
    body.data = data
  }
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => body,
  } as Response
}

/** 服务端视图只带 host / 配置态 / 文档引用；原始地址不在任何字段里（约束 38）。 */
function channelView(overrides: Partial<NotifyChannelView> = {}): NotifyChannelView {
  return {
    name: 'ops-alerts',
    resource_uid: CHANNEL_UID,
    type: 'webhook',
    description: '值班告警',
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
    etag: CHANNEL_ETAG,
    referenced_by: [],
    availability: 'ready',
    last_test: null,
    ...overrides,
  }
}

/** 记录每次请求并按 handler 返回；缺分支直接抛错，避免测试静默走空。 */
function mountChannels(handler: Handler, channels: readonly NotifyChannelView[] = []): Call[] {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const rawBody = init?.body
      const headers = (init?.headers ?? {}) as Record<string, string>
      const call: Call = {
        method: init?.method ?? 'GET',
        path: url.pathname,
        query: url.search,
        body: typeof rawBody === 'string' ? JSON.parse(rawBody) : undefined,
        ifMatch: headers['If-Match'] ?? '',
      }
      calls.push(call)
      const response = handler(call)
      if (response === undefined) {
        throw new Error(`未预期的请求：${call.method} ${call.path}`)
      }
      return response
    }),
  )
  // markRaw：避免 VTU 深代理 props 后破坏 ApiClient 的私有字段品牌检查。
  // markRaw keeps the client out of VTU's deep-reactive props wrapper.
  wrapper = mount(NotifyChannels, {
    props: { api: markRaw(new ApiClient(new ConsoleAuth())), channels },
  })
  return calls
}

function button(label: string): DOMWrapper<Element> {
  const found = wrapper!.findAll('button').find((item) => item.text() === label)
  return required(found, `按钮「${label}」`)
}

function inputValue(selector: string): string {
  return (wrapper!.find(selector).element as HTMLInputElement).value
}

function writes(calls: readonly Call[]): Call[] {
  return calls.filter((call) => call.method !== 'GET')
}

describe('notify channels', () => {
  it('distinguishes host, env reference and unconfigured targets, and lists document references', async () => {
    mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['feishu', 'webhook'])
      }
      return undefined
    }, [
      channelView(),
      // `${VAR}` 引用：url_host 为空但 url_configured=true，不能说成"地址为空"。
      channelView({ name: 'env-channel', description: '', url_host: '', referenced_by: ['writer', 'archive:arc-1'] }),
      // 托管地址按设计不回显：url_host 与 url_configured 都为空，但地址确实已配置。
      channelView({
        name: 'managed-channel',
        description: '',
        url_host: '',
        url_configured: false,
        address_status: 'managed_configured',
        credential_mode: 'managed',
        credential_ref: 'cred_01J8Z6YQ',
        credential_version: 1,
        last4: '9f2c',
      }),
      channelView({ name: 'draft-channel', description: '', url_host: '', url_configured: false, address_status: 'empty' }),
    ])
    await flushPromises()

    // 定宽表：渠道名与 UID 是有明确宽度的标识列（用户实测曾被压成竖排）。
    // The table is fixed-layout so the identifier columns keep their width.
    expect(wrapper!.find('.notify-table').exists()).toBe(true)
    const firstRow = wrapper!.findAll('tbody tr')[0]!
    expect(firstRow.findAll('td')[0]!.classes()).toContain('col-name')
    expect(firstRow.findAll('td')[1]!.classes()).toContain('col-uid')
    expect(firstRow.findAll('td')[1]!.text()).toBe(CHANNEL_UID.slice(0, 8) + '…')
    // 表头的 th 也带列类名（DataTable 的既有行为），只取数据行。
    // DataTable tags header cells with the same column class, so scope to tbody.
    const hosts = wrapper!.findAll('tbody .col-host').map((cell) => cell.text())
    expect(hosts).toEqual([
      'hooks.example.com',
      '环境变量（发送时解析）',
      '加密托管地址（不回显）',
      '未配置地址，暂不可发送',
    ])
    // 文档引用：活动 Agent 可点进详情，归档项只展示 `archive:` 标注。
    const references = wrapper!.findAll('tbody .col-refs')
    expect(references.map((cell) => cell.text())).toEqual(['无', 'writer、archive:arc-1', '无', '无'])
    const link = required(references[1], '引用列').find('a')
    expect(link.attributes('href')).toBe('/console/agents/writer')
    expect(required(references[1], '引用列').findAll('a')).toHaveLength(1)
    // 原始地址没有渲染载体：页面文本里不出现 scheme 或路径片段。
    expect(wrapper!.text()).not.toContain('https://')
    expect(wrapper!.text()).not.toContain('/services/')
  })

  it('offers only real channel types and keeps an out-of-list current value selectable', async () => {
    // 服务端只列飞书（webhook 是地址形态，不是渠道类型——用户裁决 2026-10-27）；
    // 既有 webhook 渠道仍要能打开编辑：当前值不在表里时补一项，不得把它改坏。
    mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['feishu'])
      }
      return undefined
    }, [channelView({ name: 'legacy-hook', description: '' })])
    await flushPromises()

    expect(wrapper!.findAll('#notify-type option').map((option) => option.text())).toEqual([
      '请选择类型',
      '飞书',
    ])

    await button('编辑').trigger('click')
    await flushPromises()
    // 当前值不在可选表里时补在表头（下拉仍显示原类型），其余选项按服务端顺序。
    expect(wrapper!.findAll('#notify-type option').map((option) => option.text())).toEqual([
      'webhook',
      '飞书',
    ])
    expect(inputValue('#notify-type')).toBe('webhook')
  })

  it('defaults to a managed address and submits it with the shared scope', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['feishu', 'webhook'])
      }
      if (call.method === 'POST' && call.path === CHANNELS_PATH) {
        return envelope(200, 'OK', 'success', channelView({ credential_mode: 'managed' }))
      }
      return undefined
    })
    await flushPromises()

    // 类型下拉按服务端返回的顺序渲染，组件不自行排序或枚举；已知类型显示中文标签。
    expect(wrapper!.findAll('#notify-type option').map((option) => option.text())).toEqual([
      '请选择类型',
      '飞书',
      'webhook',
    ])
    // 默认托管：地址是密码框，直接录完整值；env 只是高级方式。
    const address = wrapper!.find('#notify-url')
    expect(address.attributes('type')).toBe('password')
    expect(wrapper!.find('.notice').exists()).toBe(false)
    expect(wrapper!.text()).toContain('「加密托管」来源的地址由系统加密保存、页面不回显')

    await wrapper!.find('#notify-name').setValue('ops-alerts')
    await wrapper!.find('#notify-type').setValue('webhook')
    await address.setValue('https://hooks.example.com/services/abc')
    await wrapper!.find('#notify-description').setValue('值班告警')
    await button('创建渠道').trigger('click')
    await flushPromises()

    const created = required(
      calls.find((call) => call.method === 'POST'),
      '创建请求',
    )
    expect(created.body).toEqual({
      name: 'ops-alerts',
      type: 'webhook',
      description: '值班告警',
      url: 'https://hooks.example.com/services/abc',
      credential_mode: 'managed',
      scope: 'shared',
    })
    // 组件不自行追加列表项：写后只发事件，列表由父页面重新 GET。
    expect(wrapper!.emitted('changed')).toHaveLength(1)
    expect(wrapper!.find('.notice-info').text()).toContain('已创建')
    // 地址只出现在请求体里：写后表单清空，页面任何位置都不回显它。
    expect(wrapper!.text()).not.toContain('https://')
  })

  it('creates in env mode with a normalised placeholder', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['feishu', 'webhook'])
      }
      if (call.method === 'POST' && call.path === CHANNELS_PATH) {
        return envelope(200, 'OK', 'success', channelView())
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#notify-name').setValue('ops-alerts')
    await wrapper!.find('#notify-type').setValue('webhook')
    // env 是高级方式：切换后输入裸变量名，组件归一成契约要求的 `${VAR}`。
    await wrapper!.find('#notify-url-mode').setValue('env')
    expect(wrapper!.find('#notify-url').attributes('type')).toBe('text')
    await wrapper!.find('#notify-url').setValue('ALERT_WEBHOOK')
    await wrapper!.find('#notify-description').setValue('值班告警')
    await button('创建渠道').trigger('click')
    await flushPromises()

    const created = required(
      calls.find((call) => call.method === 'POST'),
      '创建请求',
    )
    expect(created.body).toEqual({
      name: 'ops-alerts',
      type: 'webhook',
      description: '值班告警',
      url: '${ALERT_WEBHOOK}',
      credential_mode: 'env',
      scope: 'shared',
    })
  })

  it('creates an env channel without a URL (stored as unsendable, no url field)', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'POST' && call.path === CHANNELS_PATH) {
        return envelope(200, 'OK', 'success', channelView({ url_host: '', url_configured: false }))
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#notify-name').setValue('draft-channel')
    await wrapper!.find('#notify-type').setValue('webhook')
    // 留空只在 env 模式合法：空地址 = 暂不可发送（FR37）。
    await wrapper!.find('#notify-url-mode').setValue('env')
    await button('创建渠道').trigger('click')
    await flushPromises()

    const created = required(
      calls.find((call) => call.method === 'POST'),
      '创建请求',
    )
    expect(created.body).toEqual({
      name: 'draft-channel',
      type: 'webhook',
      description: '',
      scope: 'shared',
    })
    expect((created.body as Record<string, unknown>).url).toBeUndefined()
    expect((created.body as Record<string, unknown>).credential_mode).toBeUndefined()
    expect(wrapper!.emitted('changed')).toHaveLength(1)
  })

  it('rejects a blank managed address locally with zero write requests', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#notify-name').setValue('ops-alerts')
    await wrapper!.find('#notify-type').setValue('webhook')
    await button('创建渠道').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.notice-error').text()).toContain('「加密托管」来源必须填写完整的 Webhook 地址')
    expect(wrapper!.find('.notice-error').text()).toContain('请切换到「环境变量」后留空')
    expect(writes(calls)).toHaveLength(0)
    expect(wrapper!.emitted('changed')).toBeUndefined()
  })

  it('rejects a plaintext URL in env mode locally without sending it', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#notify-name').setValue('ops-alerts')
    await wrapper!.find('#notify-type').setValue('webhook')
    await wrapper!.find('#notify-url-mode').setValue('env')
    const plaintext = 'https://hooks.example.com/services/x?token=secret'
    await wrapper!.find('#notify-url').setValue(plaintext)
    await button('创建渠道').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.notice-error').text()).toContain('「环境变量」来源只接受变量名或 ${VAR} 占位')
    // 错误不回显用户输入（可能贴了真实地址）。
    expect(wrapper!.find('.notice-error').text()).not.toContain(plaintext)
    expect(inputValue('#notify-url')).toBe(plaintext)
    expect(writes(calls)).toHaveLength(0)
    expect(wrapper!.emitted('changed')).toBeUndefined()
  })

  it('rejects a ${VAR} placeholder in managed mode locally', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#notify-name').setValue('ops-alerts')
    await wrapper!.find('#notify-type').setValue('webhook')
    await wrapper!.find('#notify-url').setValue('${ALERT_WEBHOOK}')
    await button('创建渠道').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.notice-error').text()).toContain('「加密托管」来源不接受 ${VAR} 占位')
    expect(writes(calls)).toHaveLength(0)
  })

  it('creates an exclusive channel only after the owner is chosen', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'GET' && call.path === '/api/v1/profiles') {
        return envelope(200, 'OK', 'success', [{ name: 'writer' }, { name: 'reviewer' }])
      }
      if (call.method === 'POST' && call.path === CHANNELS_PATH) {
        return envelope(200, 'OK', 'success', channelView({ scope: 'exclusive', owner_agent: 'writer' }))
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#notify-name').setValue('writer-alerts')
    await wrapper!.find('#notify-type').setValue('webhook')
    await wrapper!.find('#notify-url').setValue('https://hooks.example.com/services/abc')
    await wrapper!.find('#notify-scope-create-exclusive').setValue()
    await flushPromises()

    // 选专属才读取所有者候选；未选所有者时本地拒绝、零写请求。
    expect(
      calls.filter((call) => call.method === 'GET' && call.path === '/api/v1/profiles'),
    ).toHaveLength(1)
    await button('创建渠道').trigger('click')
    await flushPromises()
    expect(wrapper!.find('.notice-error').text()).toContain('专属渠道必须选择所有者 Agent')
    expect(writes(calls)).toHaveLength(0)

    await wrapper!.find('#notify-scope-create-owner').setValue('writer')
    await button('创建渠道').trigger('click')
    await flushPromises()
    expect(required(calls.find((call) => call.method === 'POST'), '创建请求').body).toMatchObject({
      scope: 'exclusive',
      owner_agent: 'writer',
    })
  })

  it('requires a new address when an edit switches the credential mode', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      return undefined
    }, [channelView()])
    await flushPromises()

    await button('编辑').trigger('click')
    await wrapper!.find('#notify-url-mode').setValue('managed')
    await button('保存修改').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.notice-error').text()).toContain('切换凭据来源必须填写新值')
    expect(writes(calls)).toHaveLength(0)
  })

  it('requires a second confirmation before force-deleting a referenced channel', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'DELETE' && call.path === `${CHANNELS_PATH}/ops-alerts`) {
        if (!call.query.includes('force=')) {
          return envelope(400, 'BAD_REQUEST', '通知渠道 ops-alerts 仍被引用：writer', {
            agents: ['writer', 'archive:arc-1'],
          })
        }
        return envelope(200, 'OK', 'success')
      }
      return undefined
    }, [channelView()])
    await flushPromises()

    await button('删除').trigger('click')
    await button('确认删除').trigger('click')
    await flushPromises()

    // 第一次删除带 resource_uid、不带 force；400 的点名来自服务端，组件逐条展示。
    let deletes = calls.filter((call) => call.method === 'DELETE')
    expect(deletes).toHaveLength(1)
    expect(required(deletes[0], '第一次删除请求').query).toBe(`?resource_uid=${CHANNEL_UID}`)
    expect(wrapper!.find('.notice-error').text()).toContain('仍被引用')
    expect(wrapper!.text()).toContain('writer')
    expect(wrapper!.text()).toContain('archive:arc-1')

    // 第一次点「强制删除」只解锁按钮，不发请求。
    await button('强制删除').trigger('click')
    await flushPromises()
    expect(calls.filter((call) => call.method === 'DELETE')).toHaveLength(1)

    // 第二次点击才发出字面 force=true，并让父页面重读。
    await button('再次确认强制删除').trigger('click')
    await flushPromises()
    deletes = calls.filter((call) => call.method === 'DELETE')
    expect(deletes).toHaveLength(2)
    expect(required(deletes[1], '强制删除请求').query).toBe(
      `?resource_uid=${CHANNEL_UID}&force=true`,
    )
    expect(wrapper!.emitted('changed')).toHaveLength(1)
    expect(wrapper!.find('.notice-info').text()).toContain('已强制删除')
  })

  it('keeps the stored address when an edit leaves the URL blank', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'PUT' && call.path === `${CHANNELS_PATH}/ops-alerts`) {
        return envelope(200, 'OK', 'success', channelView())
      }
      return undefined
    }, [channelView()])
    await flushPromises()

    await button('编辑').trigger('click')
    expect(inputValue('#notify-url')).toBe('')
    await button('保存修改').trigger('click')
    await flushPromises()

    const saved = required(
      calls.find((call) => call.method === 'PUT'),
      '保存请求',
    )
    expect(saved.body).toEqual({ name: 'ops-alerts', type: 'webhook', description: '值班告警' })
    expect((saved.body as Record<string, unknown>).url).toBeUndefined()
    expect(wrapper!.find('.notice-info').text()).toContain('已保存')
  })

  it('shows scope, owner, credential state and the four-value address status', async () => {
    mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      return undefined
    }, [
      channelView({
        name: 'managed-alerts',
        credential_mode: 'managed',
        credential_ref: 'cred_01J8Z6YQ',
        credential_state: 'active',
        credential_version: 2,
        last4: '9f2c',
        scope: 'exclusive',
        owner_agent: 'writer',
        address_status: 'managed_configured',
      }),
      channelView({ name: 'env-alerts' }),
      channelView({
        name: 'down-alerts',
        url_host: '',
        url_configured: true,
        address_status: 'managed_unavailable',
        credential_mode: 'managed',
        credential_state: 'revoked',
        credential_version: 3,
      }),
    ])
    await flushPromises()

    // UID 短码带全值 title：同名重建后由 UID 区分身份。
    const uids = wrapper!.findAll('tbody .col-uid code')
    expect(uids.map((cell) => cell.text())).toEqual(['rs_01J8Z…', 'rs_01J8Z…', 'rs_01J8Z…'])
    expect(uids[0]?.attributes('title')).toBe(CHANNEL_UID)

    expect(wrapper!.findAll('tbody .col-scope').map((cell) => cell.text())).toEqual([
      '专属', '共享', '共享',
    ])
    // 名称 1 / UID 2 / 类型 3 / 说明 4 / 目标服务 5 / 范围 6 / 所有者 7。
    expect(wrapper!.findAll('tbody tr').map((row) => row.find('td:nth-child(7)').text())).toEqual([
      'writer', '—', '—',
    ])

    // 凭据列：托管只给状态与尾号，env 模式没有托管密文。
    const credentials = wrapper!.findAll('tbody .col-keys').map((cell) => cell.text())
    expect(credentials[0]).toContain('有效')
    expect(credentials[0]).toContain('尾号 ····9f2c')
    expect(credentials[1]).toContain('无加密凭据')
    expect(credentials[2]).toContain('已撤销')

    // 地址状态四值：`url_host` 为空不等于未配置。
    expect(wrapper!.findAll('tbody .col-address').map((cell) => cell.text())).toEqual([
      '加密托管地址已配置', '环境变量', '加密托管地址不可用',
    ])
  })

  it('switches scope through the resource endpoint with the row etag', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'GET' && call.path === '/api/v1/profiles') {
        return envelope(200, 'OK', 'success', [{ name: 'writer' }, { name: 'reviewer' }])
      }
      if (call.method === 'POST' && call.path === `${RESOURCES_PATH}/ops-alerts/scope`) {
        return envelope(200, 'OK', 'success', {
          resource_uid: CHANNEL_UID,
          scope: 'exclusive',
          owner_agent: 'writer',
          revision: 2,
          etag: CHANNEL_ETAG,
          affected_eligible_agents: ['reviewer'],
        })
      }
      return undefined
    }, [channelView()])
    await flushPromises()

    await button('切换范围').trigger('click')
    await flushPromises()
    expect(wrapper!.text()).toContain('切换使用范围：ops-alerts')
    expect(wrapper!.text()).toContain('本页不提供强制切换')

    // 转专属必须点名所有者：未选时就地拦下，零请求。
    await wrapper!.find('#notify-scope-exclusive').setValue()
    await flushPromises()
    await button('保存范围').trigger('click')
    await flushPromises()
    expect(wrapper!.find('.notice-error').text()).toContain('转专属时必须选择所有者 Agent')
    expect(writes(calls)).toHaveLength(0)

    await wrapper!.find('#notify-scope-owner').setValue('writer')
    await button('保存范围').trigger('click')
    await flushPromises()

    const scoped = required(
      calls.find((call) => call.method === 'POST' && call.path === `${RESOURCES_PATH}/ops-alerts/scope`),
      '范围切换请求',
    )
    expect(scoped.body).toEqual({
      resource_uid: CHANNEL_UID,
      scope: 'exclusive',
      owner_agent: 'writer',
    })
    expect(scoped.ifMatch).toBe(CHANNEL_ETAG)
    // 写后不自行改列表：发事件让父页面重新读取。
    expect(wrapper!.emitted('changed')).toHaveLength(1)
    expect(wrapper!.find('.notice-info').text()).toContain('已将 ops-alerts 切换为专属')
    expect(wrapper!.find('.notice-info').text()).toContain('reviewer')
    expect(wrapper!.text()).not.toContain('切换使用范围：ops-alerts')
  })

  it('turns a 412 scope switch into a re-read prompt without retrying', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'GET' && call.path === '/api/v1/profiles') {
        return envelope(200, 'OK', 'success', [{ name: 'writer' }])
      }
      if (call.method === 'POST' && call.path === `${RESOURCES_PATH}/ops-alerts/scope`) {
        return envelope(412, 'PRECONDITION_FAILED', 'If-Match 与当前发布版本不一致')
      }
      return undefined
    }, [channelView()])
    await flushPromises()

    await button('切换范围').trigger('click')
    await flushPromises()
    await wrapper!.find('#notify-scope-exclusive').setValue()
    await flushPromises()
    await wrapper!.find('#notify-scope-owner').setValue('writer')
    await button('保存范围').trigger('click')
    await flushPromises()

    expect(writes(calls)).toHaveLength(1)
    expect(wrapper!.find('.notice-error').text()).toContain('渠道已被他人修改')
    expect(wrapper!.find('.notice-error').text()).toContain('请确认后再操作')
    expect(wrapper!.emitted('changed')).toHaveLength(1)
  })

  it('revokes a managed credential only after explicit confirmation', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'POST' && call.path === `${RESOURCES_PATH}/ops-alerts/credential/revoke`) {
        return envelope(200, 'OK', 'success', {
          resource_uid: CHANNEL_UID,
          credential_ref: 'cred_01J8Z6YQ',
          credential_state: 'revoked',
          revoked_versions: 2,
          revocation_epoch: 3,
        })
      }
      return undefined
    }, [channelView({
      credential_mode: 'managed',
      credential_ref: 'cred_01J8Z6YQ',
      credential_state: 'active',
      credential_version: 2,
      last4: '9f2c',
      address_status: 'managed_configured',
    })])
    await flushPromises()

    // 打开确认框本身不发请求；文案说明旧版本永不复活。
    await button('撤销凭据').trigger('click')
    await flushPromises()
    expect(writes(calls)).toHaveLength(0)
    const dialog = wrapper!.find('.dialog')
    expect(dialog.text()).toContain('永不复活')
    expect(dialog.text()).toContain('重新录入')

    await dialog.findAll('button').find((item) => item.text() === '确认撤销')!.trigger('click')
    await flushPromises()

    const revoked = required(
      calls.find(
        (call) => call.method === 'POST' && call.path === `${RESOURCES_PATH}/ops-alerts/credential/revoke`,
      ),
      '撤销请求',
    )
    expect(revoked.body).toEqual({ resource_uid: CHANNEL_UID, confirm: true })
    expect(revoked.ifMatch).toBe(CHANNEL_ETAG)
    expect(wrapper!.emitted('changed')).toHaveLength(1)
    expect(wrapper!.find('.notice-info').text()).toContain('已撤销 ops-alerts 的 2 个凭据版本')
  })

  // R7 连通性测试：经系统代投发送面发出固定文案；三值结论分档，结论只由服务端
  // 的 `last_test` 记录承载——组件不自行拼一条"已测试"，失败不得显示成已验证。
  // R7: the test sends the fixed text through the system sender; the verdict stays
  // three-valued and only the server-recorded last_test is rendered.
  it('runs a real connectivity test and leaves the record to the server', async () => {
    const calls = mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'POST' && call.path === `${CHANNELS_PATH}/ops-alerts/test`) {
        return envelope(200, 'OK', 'success', {
          ok: true,
          status: 'success',
          latency_ms: 42,
          detail: '',
        })
      }
      return undefined
    }, [channelView()])
    await flushPromises()

    await button('测试连通性').trigger('click')
    await flushPromises()

    const tested = required(calls.find((call) => call.method === 'POST'), '测试请求')
    expect(tested.path).toBe(`${CHANNELS_PATH}/ops-alerts/test`)
    expect(tested.body).toBeUndefined()
    expect(writes(calls)).toHaveLength(1)
    const notice = wrapper!.find('.notice-info')
    expect(notice.text()).toContain('渠道「ops-alerts」连通性测试成功（42 ms）')
    expect(notice.text()).toContain('Basalt 连通性测试')
    expect(notice.text()).toContain('对端确认受理')
    // 结论是服务端投影：组件只让父页面重读，props 未变时列表仍显示「从未测试」。
    expect(wrapper!.emitted('changed')).toHaveLength(1)
    expect(wrapper!.findAll('tbody .col-state').map((cell) => cell.text())).toEqual(['从未测试'])
  })

  it('renders a failed connectivity test with the reason and never as verified', async () => {
    mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'POST' && call.path === `${CHANNELS_PATH}/ops-alerts/test`) {
        return envelope(200, 'OK', 'success', {
          ok: false,
          status: 'failed',
          latency_ms: 30,
          detail: 'HTTP 404 Not Found',
        })
      }
      return undefined
    }, [channelView()])
    await flushPromises()

    await button('测试连通性').trigger('click')
    await flushPromises()

    const notice = wrapper!.find('.notice-error')
    expect(notice.text()).toContain('连通性测试失败（30 ms）')
    expect(notice.text()).toContain('HTTP 404 Not Found')
    expect(notice.text()).toContain('不代表已验证')
    // 失败不会以成功色调出现，也不会被写成一条"已测试通过"。
    expect(wrapper!.find('.notice-info').exists()).toBe(false)
    expect(wrapper!.emitted('changed')).toHaveLength(1)
  })

  it('reports an unknown connectivity test without claiming delivery', async () => {
    mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'POST' && call.path === `${CHANNELS_PATH}/ops-alerts/test`) {
        return envelope(200, 'OK', 'success', {
          ok: false,
          status: 'unknown',
          latency_ms: 5000,
          detail: '等待回执超时',
        })
      }
      return undefined
    }, [channelView()])
    await flushPromises()

    await button('测试连通性').trigger('click')
    await flushPromises()

    const notice = wrapper!.find('.notice-warn')
    expect(notice.text()).toContain('连通性测试结果未知（5000 ms）')
    expect(notice.text()).toContain('等待回执超时')
    expect(notice.text()).toContain('可能已送达')
    expect(notice.text()).toContain('系统不会自动重发')
  })

  it('turns a not-configured test into an error and still re-reads the list', async () => {
    mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      if (call.method === 'POST' && call.path === `${CHANNELS_PATH}/ops-alerts/test`) {
        return envelope(
          503,
          'NOT_CONFIGURED',
          '通知渠道 ops-alerts 未配置可用地址，无法发送测试消息',
        )
      }
      return undefined
    }, [channelView({ url_configured: false, address_status: 'empty' })])
    await flushPromises()

    await button('测试连通性').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.notice-error').text()).toContain('未配置可用地址')
    expect(wrapper!.text()).not.toContain('连通性测试成功')
    // 失败同样让父页面重读：服务端已记录一次失败实测。
    expect(wrapper!.emitted('changed')).toHaveLength(1)
  })

  it('shows the last recorded test separately from local readiness', async () => {
    mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      return undefined
    }, [
      channelView({ name: 'fresh' }),
      channelView({
        name: 'ok-alerts',
        last_test: { status: 'success', tested_at: '2026-10-22T08:00:00Z', detail: '已受理' },
      }),
      channelView({
        name: 'bad-alerts',
        last_test: { status: 'failed', tested_at: '2026-10-22T09:30:00Z', detail: 'HTTP 401' },
      }),
    ])
    await flushPromises()

    expect(wrapper!.findAll('thead th').map((cell) => cell.text())).toContain('最近实测')
    const cells = wrapper!.findAll('tbody .col-state')
    expect(cells).toHaveLength(3)
    // null 就是从未测过：地址状态之类的本地配置读数不填这一列。
    expect(required(cells[0], '未测试行').text()).toBe('从未测试')
    expect(required(cells[1], '成功行').text()).toContain('成功')
    expect(required(cells[1], '成功行').find('.credential-status').attributes('title')).toBe(
      '已受理',
    )
    expect(required(cells[1], '成功行').find('.muted.small').text()).toMatch(
      /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/,
    )
    expect(required(cells[2], '失败行').text()).toContain('失败')
    expect(required(cells[2], '失败行').text()).not.toContain('成功')
    // 页脚口径：本地配置读数从不冒充实测。
    expect(wrapper!.text()).toContain('「最近实测」只来自一次真实连通性测试的记录')
    expect(wrapper!.text()).toContain('从不代表已验证')
  })

  it('offers no revocation for env channels or already-revoked credentials', async () => {
    mountChannels((call) => {
      if (call.method === 'GET' && call.path === TYPES_PATH) {
        return envelope(200, 'OK', 'success', ['webhook'])
      }
      return undefined
    }, [
      channelView({ name: 'env-alerts' }),
      channelView({
        name: 'down-alerts',
        credential_mode: 'managed',
        credential_state: 'revoked',
        credential_version: 3,
        address_status: 'managed_unavailable',
      }),
    ])
    await flushPromises()

    expect(wrapper!.findAll('button').some((item) => item.text() === '撤销凭据')).toBe(false)
    // 范围切换对两种模式都可用。
    expect(wrapper!.findAll('button').filter((item) => item.text() === '切换范围')).toHaveLength(2)
  })
})
