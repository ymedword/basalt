// 工作区只读浏览回归：`.basalt/agents/<name>/AGENT.md` 的预览提供结构化配置入口；
// 其它文件不提供跳转；页面依旧没有任何写入口。
// Workspace-browser regressions: previewing an agent's own AGENT.md links to the
// structured config page; other files do not; the page still has no write affordance.
import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
  readonly query: string
}

type Route = (call: Call) => unknown

const summary = {
  version: 'test-version', uptime_seconds: 3600, started_at: '2026-10-21T12:00:00Z',
  agents: { total: 1, active: 1, disabled: 0, archived: 0, invalid: 0 },
  runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
  resources: { model_connections: 0, mcp_servers: 0, skills: 0 },
  database: { status: 'ok', schema_version: 3 }, auth: { mode: 'token', loopback_only: true },
}

const ROOT_TREE = [{ path: 'writer', type: 'dir', size: 0 }]
const AGENT_FILES = [
  { path: 'writer/AGENT.md', type: 'file', size: 13 },
  { path: 'writer/SOUL.md', type: 'file', size: 128 },
]
const FILES: Record<string, { path: string; content: string; size: number; truncated: boolean }> = {
  'writer/AGENT.md': { path: 'writer/AGENT.md', content: 'name: writer\n', size: 13, truncated: false },
  'writer/SOUL.md': { path: 'writer/SOUL.md', content: 'calm\n', size: 5, truncated: false },
}

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
    return { version: 'test-version', uptime_seconds: 3600, agents: 1, tools: 0, mcp_servers: 0, providers: [] }
  }
  if (call.path.endsWith('/system/overview')) {
    return summary
  }
  if (call.path.endsWith('/system/sample')) {
    return { sampled_at: '2026-10-21T13:00:00Z', cpu_percent: null, memory_rss_bytes: 1024, goroutines: 12, unavailable: ['cpu_percent'] }
  }
  return []
}

/** 工作区两个只读端点：目录树按相对路径返回，文件预览由查询参数定位。 */
// The two read-only workspace endpoints; the page only ever sends relative paths.
function workspaceReply(call: Call): unknown {
  if (call.path === '/api/v1/workspace/tree') {
    const path = new URLSearchParams(call.query).get('path') ?? ''
    return path === '' ? ROOT_TREE : AGENT_FILES
  }
  if (call.path === '/api/v1/workspace/file') {
    const path = new URLSearchParams(call.query).get('path') ?? ''
    return FILES[path] ?? { path, content: '', size: 0, truncated: false }
  }
  return undefined
}

async function signIn(route: Route = () => undefined): Promise<void> {
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const call: Call = {
      method: (init?.method ?? 'GET').toUpperCase(),
      path: url.pathname,
      query: url.search,
    }
    const routed = route(call)
    const data = routed === undefined ? workspaceReply(call) : routed
    return toResponse(data === undefined ? defaultReply(call) : data)
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

function button(label: string) {
  const found = wrapper!.findAll('button').find(item => item.text().trim() === label)
  expect(found, '按钮「' + label + '」应可达').toBeTruthy()
  return found!
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

describe('workspace browser', () => {
  it('links an agent definition preview to its structured config page', async () => {
    await signIn()
    await visit('/console/workspace')
    await button('打开').trigger('click')
    await flushPromises()

    const agentMdRow = wrapper!.findAll('tbody tr').find(row => row.text().includes('AGENT.md'))
    expect(agentMdRow, '目录中应有 AGENT.md').toBeTruthy()
    const preview = agentMdRow!.findAll('button').find(item => item.text().trim() === '预览')
    expect(preview, 'AGENT.md 应有预览入口').toBeTruthy()
    await preview!.trigger('click')
    await flushPromises()

    // 定义文件 → 结构化配置页；路径前缀取自目录名，不经名称重新解析。
    const link = wrapper!.find('a.link-button')
    expect(link.text()).toBe('打开 Agent 配置')
    expect(link.attributes('href')).toBe('/console/agents/writer/config')
    expect(wrapper!.find('.view-workspace-browser').text()).toContain('只读预览')
    expect(wrapper!.findAll('button').map(item => item.text())).not.toContain('保存')
  })

  it('offers no config link for a plain file preview', async () => {
    await signIn((call) => {
      if (call.path === '/api/v1/workspace/tree') {
        return [{ path: 'writer/SOUL.md', type: 'file', size: 12 }]
      }
      if (call.path === '/api/v1/workspace/file') {
        return { path: 'writer/SOUL.md', content: 'calm\n', size: 5, truncated: false }
      }
      return undefined
    })
    await visit('/console/workspace')
    await button('预览').trigger('click')
    await flushPromises()

    expect(wrapper!.find('.code-block').text()).toContain('calm')
    expect(wrapper!.findAll('a.link-button')).toHaveLength(0)
  })
})
