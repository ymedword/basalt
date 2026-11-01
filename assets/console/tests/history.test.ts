// US-16 历史回查页回归（T057）：命中只是指针、精确读取分开走、当时版本与当前结论
// 并排、truncated 必须显式标注、空关键词与 from > to 不发请求。
// 每条用例对应设计 §3.1.9「历史页面」的一行口径；逆改点写在每个用例注释里。
// History page regressions: pointers only, a separate exact read, original version
// next to today's verdict, an explicit truncated flag, and no knowably-bad requests.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { HistorySearchResult, MessageDetail } from '../src/api-history'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
  readonly query: URLSearchParams
  readonly body: unknown
}

type Route = (call: Call) => unknown

const hit = {
  message_id: 'm-1',
  session_id: 'sess-1',
  run_id: 'run-1',
  role: 'assistant' as const,
  snippet: '…按运行手册第 3 节…',
  score: 0.87,
  created_at: '2026-10-29T09:31:00Z',
}

function searchResult(partial: Partial<HistorySearchResult> = {}): HistorySearchResult {
  return { hits: [hit], truncated: false, ...partial }
}

const messageDetail: MessageDetail = {
  message_id: 'm-1',
  session_id: 'sess-1',
  run_id: 'run-1',
  seq: 3,
  role: 'assistant',
  content: '按运行手册第 3 节处理：先停机，再核对。',
  truncated: false,
  created_at: '2026-10-29T09:31:00Z',
  evidence_refs: [
    {
      ref: { id: 'doc-1', version_id: 'ver-7', content_hash: 'abcdef0123456789' },
      validity: { valid: false, reason: 'revoked' },
    },
    {
      ref: { id: 'doc-2', version_id: 'ver-9' },
      validity: { valid: true, reason: '' },
    },
  ],
}

let wrapper: ReturnType<typeof mount> | undefined
let calls: Call[] = []

function envelope(data: unknown, status = 200, message = ''): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => ({
      code: status >= 200 && status < 300 ? 'OK' : 'FAILED',
      message,
      data,
      timestamp: '2026-10-30T12:00:00Z',
    }),
  } as unknown as Response
}

/** 失败响应哨兵：路由返回它时 harness 造一个非 2xx 信封。 */
// A failure sentinel the harness turns into a non-2xx response.
interface Failure {
  readonly status: number
  readonly message: string
}

function failure(status: number, message: string): Failure {
  return { status, message }
}

function isFailure(value: unknown): value is Failure {
  return typeof value === 'object' && value !== null && 'status' in value && 'message' in value
}

/** 登录校验端点必须回一份真的 `InfoView`，否则登录态不会成立、侧栏也不渲染。 */
// /info must answer a real InfoView, or the shell never reaches the authenticated state.
const INFO = {
  version: 'test',
  uptime_seconds: 1,
  agents: 0,
  tools: 0,
  mcp_servers: 0,
  providers: [],
}

async function signIn(route: Route = () => undefined): Promise<void> {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const call: Call = {
      method: (init?.method ?? 'GET').toUpperCase(),
      path: url.pathname,
      query: url.searchParams,
      body: init?.body === undefined ? null : JSON.parse(String(init.body)) as unknown,
    }
    calls.push(call)
    if (call.path === '/api/v1/info') {
      return envelope(INFO)
    }
    const routed = route(call)
    if (isFailure(routed)) {
      return envelope(undefined, routed.status, routed.message)
    }
    return envelope(routed)
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

function searchCalls(): Call[] {
  return calls.filter((call) => call.path === '/api/v1/history/search')
}

function detailCalls(): Call[] {
  return calls.filter(
    (call) => call.path === '/api/v1/sessions/sess-1/messages/m-1' && call.method === 'GET',
  )
}

/** 按标签文字取筛选输入框（标签文案是页面与用户之间的唯一约定）。 */
function inputByLabel(text: string) {
  const label = wrapper!.findAll('.filters label').find((node) => node.text().startsWith(text))
  if (label === undefined) {
    throw new Error(`找不到标签：${text}`)
  }
  return label.find('input')
}

function openFirstHitButton() {
  const button = wrapper!.findAll('button').find((node) => node.text() === '查看原文')
  if (button === undefined) {
    throw new Error('找不到「查看原文」按钮')
  }
  return button
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('history page', () => {
  it('seeds the session from the deep link and sends nothing without a keyword', async () => {
    await signIn((call) => (call.path === '/api/v1/history/search' ? searchResult() : undefined))
    await visit('/console/history?session_id=sess-1')

    // 会话详情页深链只带 session_id：没有关键词就不发注定被 400 的请求。
    // The session deep link carries no keyword, so nothing is sent yet.
    expect(searchCalls()).toHaveLength(0)
    expect((inputByLabel('会话 ID').element as HTMLInputElement).value).toBe('sess-1')

    await wrapper!.find('.filters').trigger('submit')
    await flushPromises()

    expect(searchCalls()).toHaveLength(0)
    expect(notices().some((text) => text.includes('必须给出关键词'))).toBe(true)
  })

  it('sends the keyword with the seeded filters and the contract default window', async () => {
    await signIn((call) => (call.path === '/api/v1/history/search' ? searchResult() : undefined))
    await visit('/console/history?session_id=sess-1')

    await inputByLabel('关键词（必填）').setValue('停机')
    await wrapper!.find('.filters').trigger('submit')
    await flushPromises()

    const requests = searchCalls()
    expect(requests).toHaveLength(1)
    expect(requests[0]?.method).toBe('GET')
    expect(requests[0]?.query.get('q')).toBe('停机')
    expect(requests[0]?.query.get('session_id')).toBe('sess-1')
    expect(requests[0]?.query.get('limit')).toBe('50')
    // 空的可选过滤不得被补成空串参数发出去。
    expect(requests[0]?.query.has('agent')).toBe(false)
    expect(requests[0]?.query.has('caller_id')).toBe(false)
    expect(requests[0]?.query.has('from')).toBe(false)
    expect(requests[0]?.query.has('to')).toBe(false)
  })

  it('renders hits as pointers and never presents the snippet as a citation', async () => {
    await signIn((call) => (call.path === '/api/v1/history/search' ? searchResult() : undefined))
    await visit('/console/history?q=停机')

    const rows = wrapper!.findAll('.history-table tbody tr')
    expect(rows).toHaveLength(1)
    expect(rows[0]!.text()).toContain('…按运行手册第 3 节…')
    expect(rows[0]!.text()).toContain('m-1')
    expect(rows[0]!.text()).toContain('sess-1')
    expect(rows[0]!.text()).toContain('run-1')
    expect(rows[0]!.text()).toContain('0.870')
    // 列表阶段只有指针：正文在没有点开之前不得出现。
    expect(wrapper!.find('.history-detail').exists()).toBe(false)
    expect(wrapper!.text()).toContain('命中片段只用于定位，不是引用依据')
  })

  it('reads the exact message and shows the original version beside the current verdict', async () => {
    await signIn((call) => {
      if (call.path === '/api/v1/history/search') {
        return searchResult()
      }
      if (call.path === '/api/v1/sessions/sess-1/messages/m-1') {
        return messageDetail
      }
      return undefined
    })
    await visit('/console/history?q=停机')

    await openFirstHitButton().trigger('click')
    await flushPromises()

    expect(detailCalls()).toHaveLength(1)
    const detailText = wrapper!.find('.history-detail').text()
    expect(detailText).toContain('按运行手册第 3 节处理：先停机，再核对。')
    expect(detailText).toContain('3 · 助手')
    // 当时版本（version_id）与当前结论两列各自独立：撤权不改写引用的版本。
    expect(detailText).toContain('当时版本（version_id）')
    expect(detailText).toContain('ver-7')
    expect(detailText).toContain('权限已撤销')
    expect(detailText).toContain('ver-9')
    // 「当前可读」而不是「有效」：可定位不等于回答被原文证明。
    expect(detailText).toContain('当前可读')
    expect(detailText).toContain('不证明当时的回答正确')
    // 资料 ID 深链到文档详情，核对版本与当前生效状态。
    expect(wrapper!.find('.history-refs a[href*="/console/knowledge/documents/"]').exists()).toBe(true)
    // 片段不得被当作引用依据混进引用表：引用表只列 evidence_refs 的两条。
    expect(wrapper!.findAll('.history-refs tbody tr')).toHaveLength(2)
  })

  it('marks a truncated page instead of silently presenting it as complete', async () => {
    await signIn((call) =>
      call.path === '/api/v1/history/search' ? searchResult({ truncated: true }) : undefined,
    )
    await visit('/console/history?q=停机')

    expect(notices().some((text) => text.includes('本页不是全部'))).toBe(true)
  })

  it('keeps the untruncated page free of the truncation notice', async () => {
    await signIn((call) =>
      call.path === '/api/v1/history/search' ? searchResult({ truncated: false }) : undefined,
    )
    await visit('/console/history?q=停机')

    expect(notices().some((text) => text.includes('本页不是全部'))).toBe(false)
  })

  it('refuses a start time later than the end time without sending it', async () => {
    await signIn((call) => (call.path === '/api/v1/history/search' ? searchResult() : undefined))
    await visit('/console/history?q=停机')

    // 深链带关键词会先查一次；清空账本后只判本次提交是否外发。
    // The deep link already searched once; only the submit below is under test.
    const seededCalls = searchCalls().length
    expect(seededCalls).toBe(1)

    await inputByLabel('起始时间（本地）').setValue('2026-10-30T12:00')
    await inputByLabel('结束时间（本地）').setValue('2026-10-30T09:00')
    await wrapper!.find('.filters').trigger('submit')
    await flushPromises()

    expect(searchCalls()).toHaveLength(seededCalls)
    expect(notices().some((text) => text.includes('起始时间不得晚于结束时间'))).toBe(true)
  })

  it('wires the history route into the sidebar and the session page deep link', async () => {
    await signIn((call) => {
      if (call.path === '/api/v1/sessions/sess-1') {
        return {
          session_id: 'sess-1',
          profile_name: 'demo',
          channel: 'console',
          user_id: 'u1',
          status: 'active',
          message_count: 0,
          created_at: '2026-10-29T09:00:00Z',
          last_active_at: '2026-10-29T09:00:00Z',
          messages: [],
        }
      }
      if (call.path === '/api/v1/profiles') {
        return []
      }
      return undefined
    })

    // 侧栏入口：路由注册后仍需可点达，不是"页面存在但没有入口"。
    await visit('/console/history')
    const navLink = wrapper!
      .findAll('a')
      .find((node) => node.attributes('href') === '/console/history')
    expect(navLink).toBeDefined()
    expect(navLink!.text()).toContain('历史回查')

    await visit('/console/agents/demo/sessions/sess-1')

    // 会话详情页带本会话作过滤条件深链进历史页。
    const deep = wrapper!
      .findAll('a')
      .find((node) => node.attributes('href') === '/console/history?session_id=sess-1')
    expect(deep).toBeDefined()
    expect(deep!.text()).toBe('历史回查')
  })

  it('surfaces an exact-read failure instead of showing stale or invented content', async () => {
    await signIn((call) => {
      if (call.path === '/api/v1/history/search') {
        return searchResult()
      }
      if (call.path === '/api/v1/sessions/sess-1/messages/m-1') {
        return failure(404, '消息不存在或不可见')
      }
      return undefined
    })
    await visit('/console/history?q=停机')

    await openFirstHitButton().trigger('click')
    await flushPromises()

    expect(wrapper!.text()).toContain('消息不存在或不可见')
    expect(wrapper!.find('.history-detail').exists()).toBe(false)
    expect(wrapper!.text()).not.toContain('按运行手册第 3 节处理')
  })
})

/** 提示条原文集合：形状错误与截断提示按条判定，避免被页眉描述里的同名词干扰。 */
function notices(): string[] {
  return wrapper!.findAll('p.notice').map((node) => node.text())
}
