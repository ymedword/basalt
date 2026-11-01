// US-16 压缩记录面板回归（T089）：会话详情与运行详情两个范围的只读链、状态与降级
// 原因、两个引用集合分列、摘要的派生视图声明，以及"面板里没有任何触发入口"。
// 四条契约事实在页面上如实呈现：只有 GET、scope 恰给一个、失败带 reason_code、
// 摘要与引用不是一回事。
// Compaction panel regressions: both scopes, statuses with reasons, separate reference
// sets, the derived-summary label, and the absence of any trigger control.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { CompactionView } from '../src/api-compaction'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
  readonly query: URLSearchParams
  readonly body: unknown
}

type Route = (call: Call) => unknown

const SESSION_ID = 's-1'
const RUN_ID = 'run-1'

const committedEntry: CompactionView = {
  compaction_id: 'cp-1',
  scope_session_id: SESSION_ID,
  scope_run_id: '',
  chain_epoch: 2,
  parent_compaction_id: '',
  trigger_run_id: RUN_ID,
  attempt_id: 'att-1',
  covered_from_message_id: '1',
  cut_message_id: '40',
  first_kept_message_id: '41',
  strategy_version: 'tail-v1',
  prompt_version: 'summary-v1',
  model: 'mock-model',
  tokens_before: 12000,
  tokens_after: 3000,
  status: 'committed',
  reason_code: '',
  source_refs: [{ id: 'doc-1', version_id: 'v1', content_hash: 'a'.repeat(64) }],
  included_refs: [
    { id: 'doc-1', version_id: 'v1', content_hash: 'a'.repeat(64) },
    { id: 'doc-2', version_id: 'v3' },
  ],
  created_at: '2026-10-28T09:30:00Z',
  stale_at: '',
  summary_view: { compaction_id: 'cp-1', summary: '巡检结论摘要', truncated: false },
}

const failedEntry: CompactionView = {
  ...committedEntry,
  compaction_id: 'cp-2',
  parent_compaction_id: 'cp-1',
  covered_from_message_id: '41',
  cut_message_id: '60',
  first_kept_message_id: '61',
  status: 'failed',
  reason_code: 'compaction_timeout',
  source_refs: [],
  included_refs: [],
  created_at: '2026-10-28T09:31:00Z',
  summary_view: { compaction_id: 'cp-2', summary: '', truncated: false },
}

const staleEntry: CompactionView = {
  ...committedEntry,
  compaction_id: 'cp-3',
  parent_compaction_id: 'cp-2',
  covered_from_message_id: '61',
  cut_message_id: '80',
  first_kept_message_id: '81',
  status: 'stale',
  reason_code: 'revoked',
  created_at: '2026-10-28T09:32:00Z',
  stale_at: '2026-10-28T10:00:00Z',
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
      timestamp: '2026-10-28T12:00:00Z',
    }),
  } as unknown as Response
}

/** 会话详情夹具：页面会核对 profile_name 与深链里的 Agent 名。 */
// Session detail fixture; the view cross-checks profile_name against the deep link.
const sessionDetail = {
  session_id: SESSION_ID,
  profile_name: 'writer',
  channel: 'web',
  user_id: 'u1',
  status: 'active',
  message_count: 0,
  created_at: '2026-10-28T08:00:00Z',
  last_active_at: '2026-10-28T09:30:00Z',
  messages: [],
}

const runDetail = {
  run_id: RUN_ID,
  session_id: SESSION_ID,
  agent: 'writer',
  channel: 'web',
  trigger: 'user',
  status: 'completed',
  reason: '',
  final_text: 'done',
  iterations: 3,
  tool_calls: 1,
  config_hash: 'f'.repeat(64),
  manifest_hash: 'e'.repeat(64),
  started_at: '2026-10-28T09:30:00Z',
  finished_at: '2026-10-28T09:31:00Z',
  resource_manifest: null,
}

/** 默认路由：控制台登录后的总览、会话详情与运行详情；压缩链由用例覆盖。 */
// Default replies: the console's own overview plus the two detail pages.
function defaultReply(call: Call): unknown {
  if (call.path === '/api/v1/info') {
    return { version: 'test-version', uptime_seconds: 3600, agents: 0, tools: 0, mcp_servers: 0, providers: [] }
  }
  if (call.path === '/api/v1/system/overview') {
    return {
      version: 'test-version',
      uptime_seconds: 3600,
      started_at: '2026-10-28T00:00:00Z',
      agents: { total: 0, active: 0, disabled: 0, archived: 0, invalid: 0 },
      runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
      resources: { model_connections: 0, mcp_servers: 0, skills: 0 },
      database: { status: 'ok', schema_version: 3 },
      auth: { mode: 'token', loopback_only: true },
    }
  }
  if (call.path === '/api/v1/system/sample') {
    return {
      sampled_at: '2026-10-28T00:00:00Z',
      cpu_percent: null,
      memory_rss_bytes: 1024,
      goroutines: 12,
      unavailable: [],
    }
  }
  if (call.path === `/api/v1/sessions/${SESSION_ID}`) {
    return sessionDetail
  }
  if (call.path === `/api/v1/runs/${RUN_ID}`) {
    return runDetail
  }
  return undefined
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
    const routed = route(call)
    if (routed === undefined) {
      return envelope(defaultReply(call))
    }
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

function chainCalls(): Call[] {
  return calls.filter((call) => call.path === '/api/v1/compactions')
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('compaction panel', () => {
  it('renders the session-scope chain with statuses, reasons and separate reference sets', async () => {
    await signIn((call) => {
      if (call.path === '/api/v1/compactions') {
        return [committedEntry, failedEntry, staleEntry]
      }
      return undefined
    })
    await visit(`/console/agents/writer/sessions/${SESSION_ID}`)

    const requests = chainCalls()
    expect(requests).toHaveLength(1)
    expect(requests[0]?.query.get('session_id')).toBe(SESSION_ID)
    expect(requests[0]?.query.get('run_id')).toBeNull()

    const rows = wrapper!.findAll('.compaction-table tbody tr')
    expect(rows).toHaveLength(3)
    expect(wrapper!.text()).toContain('覆盖 1–40 → 保留自 41')
    expect(wrapper!.text()).toContain('mock-model')
    expect(wrapper!.text()).toContain('12000 → 3000 tokens')
    expect(wrapper!.text()).toContain('策略 tail-v1')
    expect(wrapper!.text()).toContain('提示 summary-v1')

    // 三种状态各自可见；失败与失效条目必须显示原因码，不得只显示徽标。
    expect(rows[0]!.text()).toContain('已提交')
    expect(rows[1]!.text()).toContain('失败')
    expect(rows[1]!.text()).toContain('compaction_timeout')
    expect(rows[2]!.text()).toContain('已失效')
    expect(rows[2]!.text()).toContain('revoked')
    expect(rows[2]!.text()).toContain('失效')

    // 两个引用集合分列：审计集 1 条、当前依赖集 2 条（不得合并成一个数字）。
    expect(rows[0]!.text()).toContain('审计 1 条')
    expect(rows[0]!.text()).toContain('当前依赖 2 条')
    expect(rows[1]!.text()).toContain('审计 0 条')

    // 失败不产出摘要时给明确文案，而不是一片空白。
    expect(rows[1]!.text()).toContain('未产出摘要（失败）')

    // 只读：面板所在卡片没有任何按钮（触发 / 重试 / 删除都不该存在）。
    const cards = wrapper!.findAll('section.card')
    const panel = cards.find((card) => card.text().includes('压缩记录'))
    expect(panel).toBeDefined()
    expect(panel!.findAll('button')).toHaveLength(0)
    expect(panel!.text()).toContain('不提供手动触发')
  })

  it('labels the summary as a derived view rather than a citation', async () => {
    await signIn((call) =>
      call.path === '/api/v1/compactions' ? [committedEntry] : undefined,
    )
    await visit(`/console/agents/writer/sessions/${SESSION_ID}`)

    expect(wrapper!.text()).toContain('查看摘要')
    expect(wrapper!.text()).toContain('巡检结论摘要')
    expect(wrapper!.text()).toContain('派生视图：不是消息原文，也不是引用依据')
  })

  it('requests the run scope from the run page and never both scopes at once', async () => {
    await signIn((call) => (call.path === '/api/v1/compactions' ? [] : undefined))
    await visit(`/console/runs/${RUN_ID}`)

    const requests = chainCalls()
    expect(requests).toHaveLength(1)
    expect(requests[0]?.query.get('run_id')).toBe(RUN_ID)
    expect(requests[0]?.query.get('session_id')).toBeNull()
    expect(wrapper!.text()).toContain('当前范围没有压缩记录')
  })

  it('surfaces a failing endpoint instead of showing an empty chain', async () => {
    await signIn((call) =>
      call.path === '/api/v1/compactions' ? failure(500, '服务器内部错误') : undefined,
    )
    await visit(`/console/agents/writer/sessions/${SESSION_ID}`)

    expect(wrapper!.text()).toContain('服务器内部错误')
    expect(wrapper!.text()).not.toContain('当前范围没有压缩记录')
  })
})
