// 执行环境页的「网络域名白名单」回归（US-12 R8 裁决 2）：条件写、保存即生效、
// 失败保留输入、412 后重新读取且绝不自动重发。
// The live allowlist editor: conditional write, re-read on 412, and never resend.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { markRaw } from 'vue'
import { ApiClient } from '../src/api'
import type { SandboxView as SandboxViewData } from '../src/api'
import { ConsoleAuth } from '../src/auth'
import { ConsoleRouter } from '../src/router'
import { SandboxView } from '../src/views/workspace'

interface Call {
  readonly url: string
  readonly init: RequestInit
}

const ETAG = `"sha256:${'a'.repeat(64)}"`
const NEW_ETAG = `"sha256:${'b'.repeat(64)}"`

function sandboxView(domains: readonly string[],
  overrides: Partial<SandboxViewData['network_allowlist']> = {}): SandboxViewData {
  return {
    file_allowed_paths: [],
    shell_allowed_commands: [],
    shell_interpreters: [],
    http_allowed_domains: [...domains],
    network_allowlist: {
      etag: ETAG, revision: 3, source: 'database',
      updated_at: '2026-10-24T08:30:00Z', updated_by: 'token:abc123',
      ...overrides,
    },
    // 本文件只测白名单编辑器；档位固定严格档，让空态文案与"[] 拒绝全部"一致。
    execution_profile: 'strict',
    execution_policy: {
      etag: ETAG, revision: 2, source: 'instance_init', updated_at: '', updated_by: '',
    },
    denied_domains: [],
    network_denylist: {
      etag: ETAG, revision: 1, source: 'database', updated_at: '', updated_by: '',
    },
  }
}

const EMPTY_EXCEPTIONS = { etag: ETAG, exceptions: [] }

function envelope(data: unknown, status = 200, message = 'success'): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => ({ code: status === 200 ? 'OK' : 'FAILED', message, data, timestamp: '2026-10-24T12:00:00Z' }),
  } as unknown as Response
}

/** 执行档位视图由同一个 sandbox fixture 派生，保持两处档位一致。 */
function policyOf(view: SandboxViewData): unknown {
  return {
    ...view.execution_policy,
    profile: view.execution_profile,
    exceptions_count: 0,
    protected_addresses: [],
    shell_names: [],
    pending_confirmations: [],
  }
}

async function mountEditor(
  view: SandboxViewData,
  handler?: (call: Call) => Response | null,
): Promise<{
  wrapper: ReturnType<typeof mount>
  calls: Call[]
}> {
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
      return envelope(policyOf(view))
    }
    if (path === '/api/v1/network-exceptions') {
      return envelope(EMPTY_EXCEPTIONS)
    }
    // 运行环境区块读取已发布 MCP 服务：没有服务就是空列表，不能落回 sandbox 视图。
    if (path === '/api/v1/mcp-servers') {
      return envelope([])
    }
    return envelope(view)
  }))
  const auth = new ConsoleAuth()
  auth.setToken('console-token')
  const api = new ApiClient(auth)
  // markRaw：避免 VTU 深代理 props 后破坏 ApiClient 的私有字段品牌检查。
  // markRaw keeps the client out of VTU's deep-reactive props wrapper.
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

function pathOf(call: Call): string {
  return new URL(call.url, 'http://console').pathname
}

function bodyOf(call: Call): unknown {
  return JSON.parse(String(call.init.body)) as unknown
}

afterEach(() => {
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('network allowlist editor', () => {
  it('renders the live metadata and the fixed empty-state copy', async () => {
    const { wrapper } = await mountEditor(sandboxView([]))
    const text = wrapper.text()
    expect(text).toContain('未配置允许域名，受本页策略约束的出站请求将被拒绝。模型连接与 stdio MCP 子进程不受本页约束。')
    expect(text).toContain('管理台保存')
    expect(text).toContain('修订 3')
    expect(text).toContain('token:abc123')
    expect((wrapper.find('#network-allowlist-editor').element as HTMLTextAreaElement).value).toBe('')
    // ETag 只放在技术细节里，不占主界面。
    expect(wrapper.find('.allowlist-details').text()).toContain(ETAG)
    expect(wrapper.find('.page-description').text()).toContain('查看并管理')
  })

  it('saves with the live ETag and re-reads the server response', async () => {
    const { wrapper, calls } = await mountEditor(sandboxView(['api.example.com']), call =>
      methodOf(call) === 'PUT'
        ? envelope(sandboxView(['a.example.com', 'b.example.com'], { revision: 4 }))
        : null)
    await wrapper.find('#network-allowlist-editor').setValue('a.example.com\nb.example.com\n')
    await wrapper.find('#network-allowlist-save').trigger('click')
    await flushPromises()

    const put = calls.find(call => methodOf(call) === 'PUT')
    expect(put?.url).toBe('/api/v1/sandbox/network-domains')
    expect((put!.init.headers as Record<string, string>)['If-Match']).toBe(ETAG)
    expect(bodyOf(put!)).toEqual({ domains: ['a.example.com', 'b.example.com'] })
    expect((wrapper.find('#network-allowlist-editor').element as HTMLTextAreaElement).value)
      .toBe('a.example.com\nb.example.com')
    expect(wrapper.text()).toContain('已保存并立即生效：修订 4')
  })

  it('re-reads on 412, keeps the draft and never resends the write', async () => {
    let view = sandboxView(['api.example.com'])
    const { wrapper, calls } = await mountEditor(view, (call) => {
      if (methodOf(call) === 'PUT') {
        view = sandboxView(['changed.example.com'], { etag: NEW_ETAG, revision: 5 })
        return envelope(
          { current_etag: NEW_ETAG },
          412,
          '网络域名白名单已被其他修改更新，请重新读取后再保存。',
        )
      }
      // 412 之后的重新读取要看到新值：/sandbox 由这里接管，其余 GET 走默认。
      return call.url.endsWith('/sandbox') ? envelope(view) : null
    })
    await wrapper.find('#network-allowlist-editor').setValue('mine.example.com')
    await wrapper.find('#network-allowlist-save').trigger('click')
    await flushPromises()

    // 顺序判据（不只是次数）：挂载读一次 → 条件写一次 → 412 后重新读一次。
    // 页面上另有 `/api/v1/execution-policy` 等 GET，与本节判定无关，按路径过滤掉；
    // 过滤后剩下的必须是**恰好这三条、且按此次序**，多写 / 少读 / 顺序颠倒都会红。
    // Ordering, not just counts: mount read → conditional write → re-read after 412.
    const timeline = calls
      .filter(call => ['/api/v1/sandbox', '/api/v1/sandbox/network-domains'].includes(pathOf(call)))
      .map(call => `${methodOf(call)} ${pathOf(call)}`)
    expect(timeline).toEqual([
      'GET /api/v1/sandbox',
      'PUT /api/v1/sandbox/network-domains',
      'GET /api/v1/sandbox',
    ])
    expect((wrapper.find('#network-allowlist-editor').element as HTMLTextAreaElement).value)
      .toBe('mine.example.com')
    expect(wrapper.text()).toContain('你的输入已保留')
    // 重新读取后的状态替换了实际生效信息，但草稿没有被覆盖。
    expect(wrapper.text()).toContain('修订 5')
    expect(wrapper.find('.allowlist-details').text()).toContain(NEW_ETAG)
  })

  it('keeps the draft when the server rejects the domain list', async () => {
    const { wrapper, calls } = await mountEditor(sandboxView(['api.example.com']), call =>
      methodOf(call) === 'PUT'
        ? envelope({}, 400, '域名格式非法：第 2 项 bad domain')
        : null)
    await wrapper.find('#network-allowlist-editor').setValue('api.example.com\nbad domain')
    await wrapper.find('#network-allowlist-save').trigger('click')
    await flushPromises()

    // 与 412 用例同一口径的有序判据：400 既不重发写，也不触发重新读取。
    const timeline = calls
      .filter(call => ['/api/v1/sandbox', '/api/v1/sandbox/network-domains'].includes(pathOf(call)))
      .map(call => `${methodOf(call)} ${pathOf(call)}`)
    expect(timeline).toEqual([
      'GET /api/v1/sandbox',
      'PUT /api/v1/sandbox/network-domains',
    ])
    expect((wrapper.find('#network-allowlist-editor').element as HTMLTextAreaElement).value)
      .toBe('api.example.com\nbad domain')
    expect(wrapper.find('.notice-error').text()).toContain('第 2 项')
  })
})
