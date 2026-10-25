// MCP 目录页回归：只读配方表、筛选、配方详情、预填不写、配方状态与配置状态
// 分开；页面不出现笼统的“已验证”徽章，也只是预填表单而从不自动保存。
// Catalog regressions: read-only recipes, filters, detail, prefill without writing,
// recipe vs configured status kept apart, and no blanket "verified" badge.
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { MCPCatalogEntry } from '../src/api'
import { createConsoleApp } from '../src/app'

interface RecordedCall {
  readonly method: string
  readonly path: string
  readonly body: unknown
}

let calls: RecordedCall[] = []
let wrapper: ReturnType<typeof mount> | undefined

function entry(overrides: Partial<MCPCatalogEntry> = {}): MCPCatalogEntry {
  return {
    id: 'github',
    display_name: 'GitHub',
    summary: '仓库、Issue 与 PR 检索',
    category: '代码托管',
    transport: 'stdio',
    command: 'npx',
    args: ['-y', '@modelcontextprotocol/server-github'],
    url: '',
    env: [
      {
        key: 'GITHUB_PERSONAL_ACCESS_TOKEN',
        placeholder: '${GITHUB_PAT}',
        required: true,
        description: '个人访问令牌',
      },
    ],
    requires: ['Node 20+'],
    source_url: 'https://github.com/modelcontextprotocol/servers',
    verified: true,
    verification_note: '2026-10-22 在 Windows 上完成连接与代表调用',
    verified_at: '2026-10-22T10:00:00Z',
    verified_platforms: ['windows'],
    warnings: ['stdio_child_egress_unconstrained'],
    ...overrides,
  }
}

const BRAVE = entry({
  id: 'brave-search',
  display_name: 'Brave Search',
  summary: '网页与新闻检索',
  category: '检索',
  args: ['-y', '@modelcontextprotocol/server-brave-search'],
  env: [
    {
      key: 'BRAVE_API_KEY',
      placeholder: '${BRAVE_API_KEY}',
      required: true,
      description: '搜索 API 密钥',
    },
  ],
  requires: ['Node 20+', 'Brave API key'],
  verified: false,
  verification_note: '配方来自官方仓库，尚未在本仓实测',
  verified_at: '',
  verified_platforms: [],
  warnings: ['package_entry_unverified'],
})

/** 目录按 id 升序（契约要求）；两套 warnings / 状态一字不差地展示给管理员。 */
const CATALOG = { version: 1, items: [BRAVE, entry()] }

function respond(status: number, message: string, data: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => ({
      code: status < 300 ? 'OK' : 'INTERNAL',
      message,
      data,
      timestamp: '2026-10-25T00:00:00Z',
    }),
  } as Response
}

interface Stubs {
  /** 按路径返回的成功响应；未命中的 GET 回 `[]`。 */
  readonly ok?: Record<string, unknown>
  /** 按路径返回的失败响应。 */
  readonly fail?: Record<string, { status: number; message: string }>
}

function setupFetch(stubs: Stubs = {}): void {
  calls = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      calls.push({
        method: (init?.method ?? 'GET').toUpperCase(),
        path: url.pathname,
        body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
      })
      const failed = stubs.fail?.[url.pathname]
      if (failed !== undefined) {
        return respond(failed.status, failed.message, null)
      }
      if (stubs.ok !== undefined && Object.hasOwn(stubs.ok, url.pathname)) {
        return respond(200, '', stubs.ok[url.pathname])
      }
      if (url.pathname.endsWith('/info')) {
        return respond(200, '', {
          version: 'test',
          uptime_seconds: 1,
          agents: 0,
          tools: 0,
          mcp_servers: 0,
          providers: [],
        })
      }
      return respond(200, '', [])
    }),
  )
}

/** 登录后直达目录页，避开总览页的旁路请求。 */
async function openCatalog(): Promise<void> {
  window.history.replaceState(
    null,
    '',
    `/console/login?next=${encodeURIComponent('/console/catalog/mcp')}`,
  )
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

function catalogRows(): DOMWrapper<Element>[] {
  return wrapper!.findAll('.catalog-table tbody tr')
}

function rowOf(name: string): DOMWrapper<Element> {
  const row = catalogRows().find((item) => item.text().includes(name))
  expect(row, name).toBeDefined()
  return row!
}

interface ButtonScope {
  findAll: (selector: string) => DOMWrapper<Element>[]
}

async function clickButton(scope: ButtonScope, label: string): Promise<void> {
  const button = scope.findAll('button').find((item) => item.text() === label)
  expect(button, label).toBeDefined()
  await button!.trigger('click')
}

function writeCalls(): RecordedCall[] {
  return calls.filter((call) => call.method !== 'GET')
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

describe('MCP catalog view', () => {
  it('renders recipe status and configured status as separate facts', async () => {
    setupFetch({
      ok: {
        '/api/v1/catalog/mcp': CATALOG,
        '/api/v1/mcp-servers': [{ name: 'github' }],
      },
    })
    await openCatalog()

    const text = wrapper!.text()
    expect(text).toContain('配方已实测')
    expect(text).toContain('配方未实测')
    // 笼统的“已验证”是禁用词：目录只承诺配方状态，不承诺业务可用。
    // 禁用词只约束徽章——“不等于已验证”这类解释句是页面刻意写的。
    expect(wrapper!.findAll('.badge').map((item) => item.text())).not.toContain('已验证')
    // 配置状态来自现有 server 名单：github 已配置，brave-search 未配置。
    expect(rowOf('GitHub').text()).toContain('已配置')
    expect(rowOf('Brave Search').text()).toContain('未配置')
    // 只读页：打开即不产生任何写请求。
    expect(writeCalls()).toHaveLength(0)
  })

  it('filters by keyword and recipe status without mixing the two', async () => {
    setupFetch({
      ok: {
        '/api/v1/catalog/mcp': CATALOG,
        '/api/v1/mcp-servers': [{ name: 'github' }],
      },
    })
    await openCatalog()

    await wrapper!.find('input[type="search"]').setValue('brave')
    expect(catalogRows()).toHaveLength(1)
    expect(catalogRows()[0]!.text()).toContain('Brave Search')

    await wrapper!.find('input[type="search"]').setValue('')
    const selects = wrapper!.findAll('form.filters select')
    expect(selects).toHaveLength(3)
    await selects[2]!.setValue('unverified')
    expect(catalogRows()).toHaveLength(1)
    expect(catalogRows()[0]!.text()).toContain('配方未实测')

    await selects[2]!.setValue('verified')
    expect(catalogRows()).toHaveLength(1)
    expect(catalogRows()[0]!.text()).toContain('GitHub')
  })

  it('shows the recipe detail with placeholders and boundaries, never values', async () => {
    setupFetch({
      ok: {
        '/api/v1/catalog/mcp': CATALOG,
        '/api/v1/mcp-servers': [],
      },
    })
    await openCatalog()

    await clickButton(rowOf('Brave Search'), '详情')

    const text = wrapper!.text()
    expect(text).toContain('配方详情：Brave Search')
    expect(text).toContain('${BRAVE_API_KEY}')
    expect(text).toContain('配方来自官方仓库，尚未在本仓实测')
    expect(text).toContain('包入口未实测，命令能否定位以诊断结果为准')
    // 来源 URL 只用 https 链接；页面不写配方之外的任何值。
    const link = wrapper!.find('a[href="https://github.com/modelcontextprotocol/servers"]')
    expect(link.exists()).toBe(true)
    expect(text).toContain('占位符只是“该填哪个变量”的提示，不是值')
  })

  it('shows the risk boundary of a recipe verbatim in its detail', async () => {
    setupFetch({
      ok: {
        '/api/v1/catalog/mcp': CATALOG,
        '/api/v1/mcp-servers': [],
      },
    })
    await openCatalog()

    await clickButton(rowOf('GitHub'), '详情')

    // 边界只说“什么没被约束”，不替管理员判断可用性。
    expect(wrapper!.text()).toContain('stdio 子进程的出网不受本仓网络策略约束')
  })

  it('prefills the create form without writing anything', async () => {
    setupFetch({
      ok: {
        '/api/v1/catalog/mcp': CATALOG,
        '/api/v1/mcp-servers': [],
      },
    })
    await openCatalog()

    await clickButton(rowOf('GitHub'), '添加到配置')
    await flushPromises()

    expect(wrapper!.find('#resource-name').element).toHaveProperty('value', 'github')
    expect(wrapper!.find('#resource-command').element).toHaveProperty('value', 'npx')
    expect(wrapper!.find('#resource-args').element).toHaveProperty(
      'value',
      '-y\n@modelcontextprotocol/server-github',
    )
    expect(wrapper!.find('#resource-env').element).toHaveProperty(
      'value',
      'GITHUB_PERSONAL_ACCESS_TOKEN=${GITHUB_PAT}',
    )
    // 预填不是写入：表单只是摆好，提交仍由用户点按钮。
    expect(writeCalls()).toHaveLength(0)
    expect(wrapper!.text()).toContain('页面不会自动保存')

    await clickButton(wrapper!, '创建并发布')
    await flushPromises()

    const posts = calls.filter((call) => call.method === 'POST' && call.path === '/api/v1/mcp-servers')
    expect(posts).toHaveLength(1)
    expect(posts[0]!.body).toEqual({
      name: 'github',
      transport: 'stdio',
      command: 'npx',
      args: ['-y', '@modelcontextprotocol/server-github'],
      env: { GITHUB_PERSONAL_ACCESS_TOKEN: '${GITHUB_PAT}' },
    })
    expect(wrapper!.text()).toContain('配方“GitHub”本身没有被修改')
  })

  it('does not claim "not configured" when the server list read fails', async () => {
    setupFetch({
      ok: { '/api/v1/catalog/mcp': CATALOG },
      fail: { '/api/v1/mcp-servers': { status: 500, message: '数据库不可用' } },
    })
    await openCatalog()

    const text = wrapper!.text()
    expect(text).toContain('读取现有 MCP 服务失败，配置状态列暂不可用')
    // 名单没读到就写“—”，不把读取失败说成“未配置”。
    expect(rowOf('GitHub').text()).toContain('—')
    expect(rowOf('GitHub').text()).not.toContain('未配置')
  })
})
