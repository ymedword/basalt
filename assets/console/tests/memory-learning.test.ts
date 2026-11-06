// US-22「记忆」页三页签回归（颗粒度文档 §4.6 的七个 TestUS22_Console_*）：
// 导航与路由、学习记录只读、待确认复用既有记忆动作、替代前新旧对照、无建议目标
// 时禁用替代、双向深链保留 Agent、确认按钮旁的共享范围提示。
// 断言只落在「页面说了什么」「DOM 上有没有这个控件」「发出了什么请求」三处。
// The seven console regressions required by §4.6: navigation, the read-only records
// tab, candidate handling through the existing memory endpoints, the pre-supersede
// comparison, the disabled supersede without a suggestion, agent-preserving deep
// links, and the shared-scope warning.
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleApp } from '../src/app'
import { matchRoute } from '../src/router'

interface Call {
  method: string
  path: string
  query: string
  body: unknown
}

type Handler = (call: Call) => Response | undefined

let wrapper: ReturnType<typeof mount> | undefined

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

function envelope(status: number, code: string, message: string, data?: unknown): Response {
  const body: Record<string, unknown> = { code, message, timestamp: '2026-11-05T08:00:00Z' }
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

function ok(data: unknown): Response {
  return envelope(200, 'OK', 'success', data)
}

function failed(status: number, code: string, message: string): Response {
  return envelope(status, code, message)
}

const PROFILES = [
  {
    name: 'ops', display_name: '', description: 'ops', provider: 'p', model: 'm',
    tools: [], skills: [], schedules: 0, registered: true, runnable: true, problems: [], notify_channels: [],
    lifecycle: {
      agent: 'ops', state: 'active', valid: true, generation: 1,
      updated_at: '2026-11-05T07:00:00Z', operation_id: 'op-test', lifecycle_error: '',
    },
  },
]

const OVERVIEW = {
  version: 'test-version', uptime_seconds: 3600, started_at: '2026-11-05T07:00:00Z',
  agents: { total: 1, active: 1, disabled: 0, archived: 0, invalid: 0 },
  runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
  resources: { model_connections: 1, mcp_servers: 0, skills: 0 },
  database: { status: 'ok', schema_version: 11 },
  auth: { mode: 'token', loopback_only: true },
}

const SECRETS_READY = {
  state: 'ready', key_id: 'k_test', initialized_at: '2026-11-01T00:00:00Z',
  key_file: '/var/lib/basalt/secrets/instance.key', recovery_reason: '',
  can_initialize: false,
  credential_summary: { managed: 0, managed_revoked: 0, env: 0, missing: 0 },
}

const STATUS_OFF = {
  enabled: false, agents: [], unknown_agents: [], rule_version: '', prompt_version: '',
  attempts_used: 0, attempt_limit: 200, exhausted: false, budget_window_seconds: 86400,
  pending_extractions: 0, unknown_extractions: 0, orphan_drafts: 0, last_extraction_at: '',
}

/** 底座端点：登录、总览与记忆库的既有读取；学习面默认给「关闭且为空」。 */
function defaults(call: Call): Response {
  switch (`${call.method} ${call.path}`) {
    case 'GET /api/v1/info':
      return ok({ version: 'test-version', uptime_seconds: 3600, agents: 1, tools: 0, mcp_servers: 0, providers: [] })
    case 'GET /api/v1/system/overview':
      return ok(OVERVIEW)
    case 'GET /api/v1/system/sample':
      return ok({ sampled_at: '2026-11-05T08:00:00Z', cpu_percent: null, memory_rss_bytes: 1024, goroutines: 8, unavailable: ['cpu_percent'] })
    case 'GET /api/v1/profiles':
      return ok(PROFILES)
    case 'GET /api/v1/instance/secrets':
      return ok(SECRETS_READY)
    case 'GET /api/v1/runs':
      return ok([])
    case 'GET /api/v1/memory':
      return ok({ scope: 'global', agent: '', backend: 'postgres', core: '', archive: '' })
    case 'GET /api/v1/memory/entries':
      return ok({ scope: 'global', agent: '', entries: [] })
    case 'GET /api/v1/learning/status':
      return ok(STATUS_OFF)
    case 'GET /api/v1/learning/extractions':
      return ok({ extractions: [] })
    case 'GET /api/v1/learning/drafts':
      return ok({ drafts: [] })
    default:
      return ok([])
  }
}

/** 挂载并登录；返回每一次请求（方法 / 路径 / 排序后的查询串 / 解析后的请求体）。 */
async function signIn(handler: Handler = () => undefined): Promise<Call[]> {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const query = [...url.searchParams.entries()]
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([key, value]) => `${key}=${value}`)
        .join('&')
      const rawBody = init?.body
      const call: Call = {
        method: init?.method ?? 'GET',
        path: url.pathname,
        query,
        body: typeof rawBody === 'string' ? JSON.parse(rawBody) : undefined,
      }
      calls.push(call)
      return handler(call) ?? defaults(call)
    }),
  )
  window.history.replaceState(null, '', '/console/login')
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
  return calls
}

async function visit(path: string): Promise<void> {
  window.history.pushState(null, '', path)
  window.dispatchEvent(new PopStateEvent('popstate'))
  await flushPromises()
  await flushPromises()
}

function text(): string {
  return wrapper!.text()
}

function buttonIn(selector: string, label: string): DOMWrapper<Element> {
  const found = wrapper!.findAll(`${selector} button`).find(item => item.text() === label)
  if (found === undefined) {
    throw new Error(`找不到按钮「${label}」（范围 ${selector}）`)
  }
  return found
}

/** 自动抽取候选条目（Agent 层）；`candidate` 元数据带逐字证据。 */
function autoCandidate(entryId: number, overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    entry_id: entryId,
    scope: 'agent',
    agent: 'ops',
    section: 'archive',
    state: 'candidate',
    content: '默认加班车走 21:30 那班',
    source: 'auto_extract',
    supersedes_id: 0,
    created_at: '2026-11-05T07:00:00Z',
    candidate: {
      extraction_id: 'ext-1',
      kind: 'preference',
      certainty: 'asserted',
      confidence: 0.8,
      evidence: [
        { message_id: 77, run_id: 'run-1', role: 'user', quote: '以后默认帮我订 21:30 那班加班车', message_at: '2026-11-05T06:59:00Z' },
      ],
      source_entry_ids: [],
      suggested_supersedes_id: 0,
      evidence_at: '2026-11-05T06:59:00Z',
      valid_to: '2026-12-31T00:00:00Z',
      time_confidence: 'relative_unknown',
    },
    ...overrides,
  }
}

function explicitCandidate(entryId: number): Record<string, unknown> {
  return {
    entry_id: entryId,
    scope: 'agent',
    agent: 'ops',
    section: 'archive',
    state: 'candidate',
    content: '显式提议的候选留在记忆库',
    source: 'explicit_propose',
    supersedes_id: 0,
    created_at: '2026-11-05T07:00:00Z',
    candidate: null,
  }
}

function activeEntry(entryId: number, content: string): Record<string, unknown> {
  return {
    entry_id: entryId,
    scope: 'agent',
    agent: 'ops',
    section: 'archive',
    state: 'active',
    content,
    source: 'explicit_propose',
    supersedes_id: 0,
    created_at: '2026-11-01T07:00:00Z',
    candidate: null,
  }
}

function receipt(entryId: number, state: string, supersededId = 0): Record<string, unknown> {
  return { entry_id: entryId, state, replaced_id: 0, merged: false, superseded_id: supersededId }
}

/** 抽取详情里的候选读模型（LearningCandidateView）：证据在顶层，不在 candidate 元数据里。 */
function learningCandidate(entryId: number): Record<string, unknown> {
  return {
    entry_id: entryId,
    state: 'candidate',
    content: '默认加班车走 21:30 那班',
    kind: 'preference',
    certainty: 'asserted',
    confidence: 0.8,
    evidence: [
      { message_id: 77, run_id: 'run-1', role: 'user', quote: '以后默认帮我订 21:30 那班加班车', message_at: '2026-11-05T06:59:00Z' },
    ],
    source_entry_ids: [],
    suggested_supersedes_id: 0,
    evidence_at: '2026-11-05T06:59:00Z',
    valid_to: '2026-12-31T00:00:00Z',
    time_confidence: 'relative_unknown',
    created_at: '2026-11-05T07:00:00Z',
  }
}

const EXTRACTION_VIEW = {
  extraction_id: 'ext-1',
  agent: 'ops',
  session_id: 'sess-1',
  run_id: 'run-1',
  last_attempt_run_id: 'run-1',
  origin_caller_id: 'console',
  origin_key_id: '',
  status: 'success',
  window_from_message_id: 70,
  window_to_message_id: 77,
  window_messages: 8,
  dropped_window_messages: 2,
  prior_messages: 3,
  attempts: 1,
  items_total: 2,
  items_accepted: 1,
  items_dropped: 1,
  prompt_version: 'p1',
  rule_version: 'r1',
  provider: 'local',
  model: 'model-v1',
  failure_class: '',
  error_message: '',
  created_at: '2026-11-05T07:00:00Z',
  last_attempt_at: '2026-11-05T07:00:01Z',
  finished_at: '2026-11-05T07:00:02Z',
  verified_at: '',
}

const DRAFT_META = {
  draft_id: 'drf-1',
  extraction_id: 'ext-1',
  agent: 'ops',
  name: 'night-shift',
  content_sha256: 'abcdef0123456789abcdef',
  rel_dir: 'drafts/ops/night-shift',
  created_at: '2026-11-05T07:00:03Z',
}

describe('TestUS22_Console_MemoryTabs', () => {
  it('keeps one memory route with three tabs and no learning navigation entry', async () => {
    await signIn()
    await visit('/console/library/memory')

    const tabs = wrapper!.findAll('.memory-tabs a')
    expect(tabs.map(item => item.text())).toEqual(['记忆库', '待确认', '学习记录'])
    expect(tabs.map(item => item.attributes('href'))).toEqual([
      '/console/library/memory',
      '/console/library/memory?tab=pending',
      '/console/library/memory?tab=records',
    ])
    expect(wrapper!.find('.memory-tabs a[aria-current="page"]').text()).toBe('记忆库')
    expect(wrapper!.find('h1').text()).toBe('全局记忆')

    // 不新增左侧「学习」入口：学习记录只在这三个页签里。
    expect(wrapper!.findAll('.nav-item').filter(item => item.text() === '学习')).toHaveLength(0)
    expect(wrapper!.findAll('.nav-item').filter(item => item.text().includes('学习记录'))).toHaveLength(0)
    expect(wrapper!.findAll('.console-sidebar a[href^="/console/learning"]')).toHaveLength(0)
    // 也没有 /console/learning* 路由。
    expect(matchRoute('/console/learning').name).toBe('not-found')
    expect(matchRoute('/console/learning/records').name).toBe('not-found')

    await wrapper!.find('.memory-tabs a[href="/console/library/memory?tab=pending"]').trigger('click')
    await flushPromises()
    await flushPromises()
    expect(window.location.pathname + window.location.search).toBe('/console/library/memory?tab=pending')
    expect(wrapper!.find('h1').text()).toBe('待确认候选')
    expect(wrapper!.find('.memory-tabs a[aria-current="page"]').text()).toBe('待确认')

    await wrapper!.find('.memory-tabs a[href="/console/library/memory?tab=records"]').trigger('click')
    await flushPromises()
    await flushPromises()
    expect(wrapper!.find('h1').text()).toBe('学习记录')
    expect(wrapper!.find('.memory-tabs a[aria-current="page"]').text()).toBe('学习记录')

    // 未知路径仍落到 404，学习记录没有另开路由。
    await visit('/console/learning')
    expect(wrapper!.find('h1').text()).toBe('页面不存在')
  })
})

describe('TestUS22_Console_LearningRecordsReadOnly', () => {
  it('shows status, ledger and drafts without any write control', async () => {
    const calls = await signIn(call => {
      if (call.method === 'GET' && call.path === '/api/v1/learning/status') {
        return ok({
          ...STATUS_OFF,
          enabled: true,
          agents: ['ops'],
          unknown_agents: ['ghost'],
          rule_version: 'r1',
          prompt_version: 'p1',
          attempts_used: 3,
          pending_extractions: 1,
          last_extraction_at: '2026-11-05T07:00:02Z',
        })
      }
      if (call.method === 'GET' && call.path === '/api/v1/learning/extractions') {
        return ok({
          extractions: [
            { ...EXTRACTION_VIEW, extraction_id: 'ext-fail', status: 'failed', failure_class: 'model_error', error_message: '模型返回 500' },
            EXTRACTION_VIEW,
          ],
        })
      }
      if (call.method === 'GET' && call.path === '/api/v1/learning/drafts') {
        return ok({ drafts: [DRAFT_META] })
      }
      return undefined
    })
    await visit('/console/library/memory?tab=records')

    const page = text()
    expect(page).toContain('已启用')
    expect(page).toContain('3 / 200')
    expect(page).toContain('ghost')
    expect(page).toContain('记 WARN，不抽取，不阻止启动')
    // 账本行：成功 / 失败 + 失败分类中文化。
    expect(page).toContain('ext-fail')
    expect(page).toContain('模型错误')
    expect(page).toContain('模型返回 500')
    expect(page).toContain('成功')
    // 草案只列元数据，正文只在详情。
    expect(page).toContain('night-shift')
    expect(page).toContain('没有启用开关、手动抽取、重试、发布或批准按钮')

    // 只读面：页面上没有任何按钮，所有链接都还在记忆页内。
    expect(wrapper!.findAll('.learning-records button')).toHaveLength(0)
    for (const anchor of wrapper!.findAll('.learning-records a')) {
      expect(anchor.attributes('href') ?? '').toMatch(/^\/console\/library\/memory/)
    }
    // 除登录与总览的 GET 外，本次没有发出任何写请求。
    expect(calls.filter(call => call.method !== 'GET')).toHaveLength(0)
  })
})

describe('TestUS22_Console_PendingUsesMemoryActions', () => {
  it('lists only auto candidates and reuses the existing confirm and delete endpoints', async () => {
    let deleted = false
    const calls = await signIn(call => {
      if (call.method === 'GET' && call.path === '/api/v1/agents/ops/memory/entries') {
        if (call.query === 'state=candidate') {
          return ok({ scope: 'agent', agent: 'ops', entries: deleted ? [] : [autoCandidate(11), explicitCandidate(12)] })
        }
        if (call.query === 'state=active') {
          return ok({ scope: 'agent', agent: 'ops', entries: [] })
        }
      }
      // 既有的确认 / 删除端点按条目主键定位，不按层分路径（卡片 §3.1.10 原句）。
      if (call.method === 'POST' && call.path === '/api/v1/memory/entries/11/confirm') {
        return ok(receipt(11, 'active'))
      }
      if (call.method === 'DELETE' && call.path === '/api/v1/memory/entries/11') {
        deleted = true
        return ok(receipt(11, 'deleted'))
      }
      return undefined
    })
    await visit('/console/library/memory?tab=pending&agent=ops')

    // 自动候选带证据；显式提议候选不在本页签。
    const page = text()
    expect(page).toContain('默认加班车走 21:30 那班')
    expect(page).toContain('以后默认帮我订 21:30 那班加班车')
    expect(page).toContain('来源抽取')
    expect(page).not.toContain('显式提议的候选留在记忆库')
    // 确认按钮旁固定提示共享范围。
    expect(wrapper!.find('.memory-scope-hint').text()).toContain('确认后对该 Agent 的所有调用方可见。')

    await buttonIn('.memory-pending', '保留双方').trigger('click')
    await flushPromises()
    const confirmCall = calls.find(call => call.method === 'POST')
    expect(confirmCall?.path).toBe('/api/v1/memory/entries/11/confirm')
    expect(confirmCall?.body).toEqual({ resolution: 'keep_both' })
    expect(text()).toContain('候选 #11 已确认生效（保留双方）')

    await buttonIn('.memory-pending', '删除').trigger('click')
    await flushPromises()
    expect(wrapper!.find('[role="dialog"]').text()).toContain('行与审计保留')
    await buttonIn('.memory-pending', '确认删除').trigger('click')
    await flushPromises()
    const deleteCall = calls.find(call => call.method === 'DELETE')
    expect(deleteCall?.path).toBe('/api/v1/memory/entries/11')
    expect(text()).toContain('候选 #11 已删除')

    // 没有另开写端点：写动作只有这两条既有记忆管理路径。
    expect(calls.filter(call => call.method !== 'GET').map(call => `${call.method} ${call.path}`)).toEqual([
      'POST /api/v1/memory/entries/11/confirm',
      'DELETE /api/v1/memory/entries/11',
    ])
  })
})

describe('TestUS22_Console_SupersedeComparison', () => {
  it('shows both bodies before a supersede is submitted', async () => {
    const calls = await signIn(call => {
      if (call.method === 'GET' && call.path === '/api/v1/agents/ops/memory/entries') {
        if (call.query === 'state=candidate') {
          return ok({
            scope: 'agent', agent: 'ops',
            entries: [autoCandidate(21, {
              content: '默认加班车改走 22:00 那班',
              candidate: { ...(autoCandidate(21).candidate as Record<string, unknown>), suggested_supersedes_id: 5 },
            })],
          })
        }
        if (call.query === 'state=active') {
          return ok({ scope: 'agent', agent: 'ops', entries: [activeEntry(5, '旧口径：21:00 发车')] })
        }
      }
      if (call.method === 'POST' && call.path === '/api/v1/memory/entries/21/confirm') {
        return ok(receipt(21, 'active', 5))
      }
      return undefined
    })
    await visit('/console/library/memory?tab=pending&agent=ops')

    await buttonIn('.memory-pending', '替代').trigger('click')
    await flushPromises()

    const compare = wrapper!.find('.memory-supersede-compare')
    expect(compare.exists()).toBe(true)
    expect(compare.text()).toContain('旧条目 #5（生效中）')
    expect(compare.text()).toContain('旧口径：21:00 发车')
    expect(compare.text()).toContain('新候选 #21')
    expect(compare.text()).toContain('默认加班车改走 22:00 那班')
    // 对照没看过之前不发 POST。
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)

    await buttonIn('.memory-pending', '确认替代').trigger('click')
    await flushPromises()
    const post = calls.find(call => call.method === 'POST')
    expect(post?.body).toEqual({ resolution: 'supersede', supersedes_entry_id: 5 })
    expect(text()).toContain('旧条目 #5 已替代')
  })
})

describe('TestUS22_Console_SupersedeNeedsSuggestion', () => {
  it('disables supersede without a suggestion and refuses a stale target', async () => {
    const calls = await signIn(call => {
      if (call.method === 'GET' && call.path === '/api/v1/agents/ops/memory/entries') {
        if (call.query === 'state=candidate') {
          return ok({
            scope: 'agent', agent: 'ops',
            entries: [
              autoCandidate(31),
              autoCandidate(32, {
                candidate: { ...(autoCandidate(32).candidate as Record<string, unknown>), suggested_supersedes_id: 99 },
              }),
            ],
          })
        }
        if (call.query === 'state=active') {
          return ok({ scope: 'agent', agent: 'ops', entries: [] })
        }
      }
      return undefined
    })
    await visit('/console/library/memory?tab=pending&agent=ops')

    const supersedes = wrapper!.findAll('.memory-pending button').filter(item => item.text() === '替代')
    expect(supersedes).toHaveLength(2)
    // 无建议目标：禁用，点了也不发请求。
    expect(supersedes[0]!.attributes('disabled')).toBeDefined()
    expect(supersedes[0]!.attributes('title')).toBe('没有建议替代目标，不能替代')
    await supersedes[0]!.trigger('click')
    await flushPromises()
    expect(calls.some(call => call.query === 'state=active')).toBe(false)

    // 建议目标已不在生效态：给出警告，不进入对照、不提交。
    await supersedes[1]!.trigger('click')
    await flushPromises()
    expect(calls.some(call => call.query === 'state=active')).toBe(true)
    expect(text()).toContain('建议替代目标 #99 已不在生效态')
    expect(wrapper!.find('.memory-supersede-compare').exists()).toBe(false)
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
  })
})

describe('TestUS22_Console_DeepLinksPreserveAgent', () => {
  it('keeps the agent across both directions and reports missing anchors', async () => {
    await signIn(call => {
      if (call.method === 'GET' && call.path === '/api/v1/learning/extractions/ext-1') {
        return ok({
          extraction: EXTRACTION_VIEW,
          candidates: [learningCandidate(41)],
          drafts: [DRAFT_META],
        })
      }
      if (call.method === 'GET' && call.path === '/api/v1/learning/extractions/missing') {
        return failed(404, 'NOT_FOUND', '抽取记录不存在')
      }
      if (call.method === 'GET' && call.path === '/api/v1/agents/ops/memory/entries') {
        if (call.query === 'state=candidate') {
          return ok({ scope: 'agent', agent: 'ops', entries: [autoCandidate(41)] })
        }
      }
      return undefined
    })
    await visit('/console/library/memory?tab=records&agent=ops&extraction=ext-1')

    expect(wrapper!.find('h1').text()).toBe('学习记录')
    expect(text()).toContain('抽取详情 ext-1')
    // 切换页签保留 agent，不把上下文丢掉。
    expect(wrapper!.find('.memory-tabs a[href="/console/library/memory?agent=ops"]').exists()).toBe(true)
    expect(wrapper!.find('.memory-tabs a[href="/console/library/memory?tab=pending&agent=ops"]').exists()).toBe(true)
    // 抽取详情 → 候选（待确认页签）与草案（同页签）都保留 agent。
    const candidateLink = wrapper!.find('.learning-candidates-table a[href*="candidate=41"]')
    expect(candidateLink.attributes('href')).toBe('/console/library/memory?tab=pending&agent=ops&candidate=41')
    expect(wrapper!.find(`.learning-draft-links a[href="/console/library/memory?tab=records&agent=ops&draft=drf-1"]`).exists()).toBe(true)

    await candidateLink.trigger('click')
    await flushPromises()
    await flushPromises()
    expect(window.location.pathname + window.location.search).toBe('/console/library/memory?tab=pending&agent=ops&candidate=41')
    expect(wrapper!.find('h1').text()).toBe('待确认候选')
    expect(text()).toContain('当前定位')

    // 锚点已不存在：显示「已不存在」，不静默换成别的记录。
    await visit('/console/library/memory?tab=records&agent=ops&extraction=missing')
    expect(text()).toContain('抽取记录 missing 已不存在')
    await visit('/console/library/memory?tab=pending&agent=ops&candidate=999')
    expect(text()).toContain('候选 #999 已不存在')
  })
})

describe('TestUS22_Console_SharedScopeWarning', () => {
  it('states the shared agent-wide scope beside the confirm action', async () => {
    await signIn(call => {
      if (call.method === 'GET' && call.path === '/api/v1/agents/ops/memory/entries' && call.query === 'state=candidate') {
        return ok({ scope: 'agent', agent: 'ops', entries: [autoCandidate(51)] })
      }
      return undefined
    })
    await visit('/console/library/memory?tab=pending&agent=ops')

    // 提示与确认动作在同一行的操作区里：不能只写在页脚。
    const actions = wrapper!.findAll('.memory-pending-actions')
    expect(actions.length).toBeGreaterThan(0)
    for (const row of actions) {
      expect(row.text()).toContain('确认后对该 Agent 的所有调用方可见。')
    }
    expect(buttonIn('.memory-pending-actions', '保留双方').exists()).toBe(true)
    // 页面同时说明管理视图与召回面的差别。
    expect(text()).toContain('确认之前不参与任何召回或注入')
  })
})
