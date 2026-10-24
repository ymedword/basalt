// US-12 资源列表回归（R7 收敛）：模型连接列表只留选择连接需要的四列——名称 /
// 默认模型 / 范围 / 本地就绪；UID、地址、凭据、版本、所有者与引用者都在详情页。
// 列表不含任何密钥、地址或 UID 回显载体；本地就绪与实测结果是两件事。
// R7: the list keeps only name, default model, scope and local readiness. Address,
// credentials, versions, owner and referencers live on the detail page; readiness is
// never presented as a recorded test.
import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
}

type Route = (call: Call) => unknown

const summary = {
  version: 'test-version', uptime_seconds: 3600, started_at: '2026-10-21T12:00:00Z',
  agents: { total: 0, active: 0, disabled: 0, archived: 0, invalid: 0 },
  runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
  resources: { model_connections: 2, mcp_servers: 0, skills: 0 },
  database: { status: 'ok', schema_version: 3 }, auth: { mode: 'token', loopback_only: true },
}

const MANAGED_UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'
const ENV_UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B'

/** 列表夹具：一条专属托管凭据连接 + 一条共享环境变量连接。 */
// One exclusive managed connection and one shared env connection.
const connections = [
  {
    name: 'deepseek',
    resource_uid: MANAGED_UID,
    base_url: 'https://api.deepseek.com',
    api_key_env: '',
    default_model: 'deepseek-chat',
    credential_mode: 'managed',
    credential_ref: 'cred_01J8Z6YQ',
    credential_state: 'active',
    credential_version: 2,
    last4: '9f2c',
    scope: 'exclusive',
    owner_agent: 'writer',
    revision: 4,
    published_version_id: 3,
    published_version_no: 3,
    applied_version_id: 3,
    applied_version_no: 3,
    etag: '"sha256:' + 'a'.repeat(64) + '"',
    referenced_by: ['writer'],
    availability: 'ready',
    last_test: null,
  },
  {
    name: 'local-model',
    resource_uid: ENV_UID,
    base_url: 'http://127.0.0.1:11434/v1',
    api_key_env: 'LOCAL_API_KEY',
    default_model: 'qwen',
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
    etag: '"sha256:' + 'b'.repeat(64) + '"',
    referenced_by: [],
    availability: 'unconfigured',
    last_test: null,
  },
]

let wrapper: ReturnType<typeof mount> | undefined

function toResponse(data: unknown): Response {
  return {
    ok: true,
    status: 200,
    headers: new Headers(),
    json: async () => ({ code: 'OK', message: '', data, timestamp: '2026-10-22T12:00:00Z' }),
  } as Response
}

function defaultReply(call: Call): unknown {
  if (call.path.endsWith('/info')) {
    return { version: 'test-version', uptime_seconds: 3600, agents: 0, tools: 0, mcp_servers: 0, providers: [] }
  }
  if (call.path.endsWith('/system/overview')) {
    return summary
  }
  if (call.path.endsWith('/system/sample')) {
    return { sampled_at: '2026-10-21T13:00:00Z', cpu_percent: null, memory_rss_bytes: 1024, goroutines: 12, unavailable: ['cpu_percent'] }
  }
  return []
}

async function signIn(route: Route = () => undefined): Promise<void> {
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const call: Call = {
      method: (init?.method ?? 'GET').toUpperCase(),
      path: new URL(String(input), 'http://localhost').pathname,
    }
    const routed = route(call)
    return toResponse(routed === undefined ? defaultReply(call) : routed)
  }))
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

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

describe('model connection list', () => {
  it('lists only name, default model, scope and local readiness', async () => {
    await signIn((call) => (call.path === '/api/v1/model-connections' ? connections : undefined))
    await visit('/console/resources/model-connections')

    const headers = wrapper!.findAll('thead th').map(cell => cell.text())
    expect(headers).toEqual(['名称', '默认模型', '范围', '可用性'])

    // 名称链接到详情页：身份、地址、凭据与引用者不再铺在列表上。
    const links = wrapper!.findAll('tbody a.cell-id')
    expect(links.map(cell => cell.text())).toEqual(['deepseek', 'local-model'])
    expect(links.map(cell => cell.attributes('href'))).toEqual([
      '/console/resources/model-connections/deepseek',
      '/console/resources/model-connections/local-model',
    ])
    expect(wrapper!.findAll('tbody .col-short').map(cell => cell.text())).toEqual(['专属', '共享'])

    // 可用性 = 本地配置就绪三值；不是"已验证"。
    expect(wrapper!.findAll('tbody .col-state').map(cell => cell.text())).toEqual(['配置就绪', '未配置'])

    // 列表没有任何地址 / 密钥 / UID / 尾号回显载体。
    const text = wrapper!.text()
    expect(text).not.toContain(MANAGED_UID)
    expect(text).not.toContain(ENV_UID)
    expect(text).not.toContain('api.deepseek.com')
    expect(text).not.toContain('9f2c')
    expect(text).not.toContain('LOCAL_API_KEY')
  })

  it('separates local readiness from a recorded test in the list note', async () => {
    await signIn((call) => (call.path === '/api/v1/model-connections' ? connections : undefined))
    await visit('/console/resources/model-connections')
    const note = wrapper!.find('.page-description').text()
    expect(note).toContain('本地配置就绪不代表已实测连通')
    expect(note).toContain('实测结果与时间单独记录')
  })
})
