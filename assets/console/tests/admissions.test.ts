// US-21 学习准入页回归（T022）：只读列表与详情。三条契约事实在页面上如实呈现：
// `api.ts` 的准入调用只有 GET（管理面没有写端点）；渲染后的页面没有 `<button>`
// （只读页没有可触发动作）；构建产物里没有 token 落盘（localStorage / sessionStorage /
// document.cookie 一处都不写）。另断言派生展示态 expired、七维判定与「不可评估不等于
// 通过」、批准者 / 有效期、发布版本与撤回分类。
// Admission console regressions: GET-only API surface, no button in the rendered
// pages, no persisted token, derived expired state, and the decision / publication
// projections.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { AdmissionDetailView, AdmissionListView } from '../src/api'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
  readonly query: URLSearchParams
  readonly body: unknown
}

type Route = (call: Call) => unknown

const APPROVED_ID = 'adm_01J9APPROVED'
const EXPIRED_ID = 'adm_01J9EXPIRED'
const PUBLISHED_ID = 'adm_01J9PUBLISHED'
const CANDIDATE_HASH = 'a'.repeat(64)
const SOURCE_HASH = 'b'.repeat(64)

const approvedRow: AdmissionListView = {
  candidate_id: APPROVED_ID,
  skill_name: 'weekly-report',
  state: 'approved',
  risk_tier: 'normal',
  proposer_subject: 'cli:administrator',
  candidate_hash: CANDIDATE_HASH,
  source_hash: SOURCE_HASH,
  created_at: '2026-11-04T01:00:00Z',
  updated_at: '2026-11-04T01:30:00Z',
}

const expiredRow: AdmissionListView = {
  ...approvedRow,
  candidate_id: EXPIRED_ID,
  skill_name: 'ops-runbook',
  state: 'expired',
}

const publishedRow: AdmissionListView = {
  ...approvedRow,
  candidate_id: PUBLISHED_ID,
  skill_name: 'deploy-checklist',
  state: 'published',
  risk_tier: 'high',
}

const listRows: AdmissionListView[] = [approvedRow, expiredRow, publishedRow]

// 需求：详情展示来源的结构化字段、评估维度、批准者、有效期、发布版本与撤回状态。
// 来源自由备注置为哨兵：页面任何位置都不得出现它（服务端已置空，页面也不渲染）。
const detail: AdmissionDetailView = {
  ...publishedRow,
  source: {
    kind: 'manual',
    ref: 'refs/us21-weekly-report',
    task_outcome: 'completed',
    confirmation: 'direct',
    authorization: 'declared',
    redaction: 'declared',
    retention_until: '2026-12-01',
    applicable: true,
    counterexamples: [],
    evidence: ['2026-11-04 管理员整理'],
    limitations: ['仅在日报场景验证'],
    notes: '内部自由备注：不得整段出站',
  },
  files: [
    { path: 'SKILL.md', sha256: 'c'.repeat(64), size: 128 },
    { path: 'references/checklist.md', sha256: 'd'.repeat(64), size: 64 },
  ],
  evaluation: {
    evaluation_id: 'eval_01J9',
    candidate_revision: 2,
    candidate_hash: CANDIDATE_HASH,
    evaluation_hash: 'e'.repeat(64),
    grader_version: 'admission-grader-v1',
    policy_hash: 'f'.repeat(64),
    dimensions: [
      { name: 'content', status: 'passed', reason: '' },
      { name: 'source', status: 'passed', reason: '' },
      { name: 'permissions', status: 'passed', reason: 'script_review_required' },
      { name: 'safety', status: 'passed', reason: '' },
      { name: 'regression', status: 'passed', reason: '' },
      { name: 'effect', status: 'not_evaluable', reason: 'real_model_not_authorized' },
      { name: 'cost', status: 'not_evaluable', reason: 'real_model_not_authorized' },
    ],
    scripts: ['scripts/collect.sh'],
    aggregate: 'incomplete',
    risk_tier: 'high',
    referrers: ['walkthrough-guide'],
    evaluated_at: '2026-11-04T01:10:00Z',
  },
  decision: {
    decision_id: 'dec_01J9',
    decision: 'approve',
    reason: '静态审查通过，脚本只读工作目录',
    acknowledged: ['effect', 'cost'],
    risk_tier: 'high',
    approver_subject: 'cli:reviewer',
    expires_at: '2026-11-04T13:10:00Z',
    superseded_at: '',
    created_at: '2026-11-04T01:20:00Z',
  },
  publication: {
    skill_name: 'deploy-checklist',
    version_id: 12,
    version_no: 3,
    content_hash: '9'.repeat(64),
    state: 'withdrawn',
    published_at: '2026-11-04T02:00:00Z',
    withdrawn_at: '2026-11-04T03:00:00Z',
    withdraw_reason: 'restored_previous',
    withdraw_version_id: 11,
  },
}

/** 尚无子记录的详情：评估 / 决策 / 发布三块都要显示空态，而不是伪造字段。 */
const emptyDetail: AdmissionDetailView = {
  ...approvedRow,
  source: { ...detail.source, notes: '' },
  files: [],
  evaluation: null,
  decision: null,
  publication: null,
}

function envelope(data: unknown, status = 200, message = ''): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => ({
      code: status >= 200 && status < 300 ? 'OK' : 'FAILED',
      message,
      data,
      timestamp: '2026-11-04T12:00:00Z',
    }),
  } as unknown as Response
}

function defaultReply(call: Call): unknown {
  if (call.path === '/api/v1/info') {
    return { version: 'test-version', uptime_seconds: 3600, agents: 0, tools: 0, mcp_servers: 0, providers: [] }
  }
  // 登录后的默认路由是总览页；不提供它的读数会留下未处理的拒绝。
  if (call.path === '/api/v1/system/overview') {
    return {
      version: 'test-version',
      uptime_seconds: 3600,
      started_at: '2026-11-04T00:00:00Z',
      agents: { total: 0, active: 0, disabled: 0, archived: 0, invalid: 0 },
      runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
      resources: { model_connections: 0, mcp_servers: 0, skills: 0 },
      database: { status: 'ok', schema_version: 10 },
      auth: { mode: 'token', loopback_only: true },
    }
  }
  if (call.path === '/api/v1/system/sample') {
    return { sampled_at: '2026-11-04T00:00:00Z', cpu_percent: null, memory_rss_bytes: 1024, goroutines: 12, unavailable: [] }
  }
  return []
}

let wrapper: ReturnType<typeof mount> | undefined
let calls: Call[] = []

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
    return envelope(routed === undefined ? defaultReply(call) : routed)
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
  document.body.innerHTML = ''
})

describe('admission api surface', () => {
  it('only calls GET for admissions — the admin surface has no write endpoint', () => {
    // 源码与构建产物都以原文读入（Vite 的 ?raw 机制，不新增 Node 类型依赖）。
    const source = import.meta.glob<string>('../src/api.ts', {
      query: '?raw',
      import: 'default',
      eager: true,
    })['../src/api.ts'] ?? ''
    expect(source.length).toBeGreaterThan(0)
    // 抓取所有“方法 + /admissions 路径”的同段出现：任何 POST / PUT / PATCH / DELETE
    // 都会被收集到。一处也抓不到 = 断言失效，同样判失败。
    const verbs = [...source.matchAll(/['"`](GET|POST|PUT|PATCH|DELETE)['"`][^\n]*\/admissions/g)]
      .map((match) => match[1])
    expect(verbs.length).toBeGreaterThan(0)
    expect(verbs.every((verb) => verb === 'GET')).toBe(true)
  })

  it('ships no token write in the built bundle', () => {
    const bundles = import.meta.glob<string>('../dist/assets/*.js', {
      query: '?raw',
      import: 'default',
      eager: true,
    })
    const entries = Object.entries(bundles)
    expect(entries.length).toBeGreaterThan(0)
    for (const [name, text] of entries) {
      expect(text, name).not.toContain('localStorage.setItem')
      expect(text, name).not.toContain('sessionStorage.setItem')
      expect(text, name).not.toContain('document.cookie')
    }
  })
})

describe('admission list', () => {
  it('renders states, the derived expired marker and no write control', async () => {
    await signIn((call) => (call.path === '/api/v1/admissions' ? listRows : undefined))
    await visit('/console/admissions')

    const view = wrapper!.find('.view-admissions')
    expect(view.exists()).toBe(true)
    expect(view.findAll('button')).toHaveLength(0)
    expect(view.text()).toContain('批准 / 发布 / 撤回只在本机 CLI 执行')
    expect(view.text()).toContain('「批准已过期」是派生展示态')

    const rows = view.findAll('.admissions-table tbody tr')
    expect(rows).toHaveLength(3)
    expect(rows[0]!.text()).toContain('已批准')
    // expired 是派生展示态：标签写明派生，不能让读者以为库内状态变了。
    expect(rows[1]!.text()).toContain('批准已过期（派生）')
    expect(rows[2]!.text()).toContain('已发布')
    expect(rows[2]!.text()).toContain('高风险')
    expect(rows[1]!.find('a').attributes('href')).toBe(`/console/admissions/${EXPIRED_ID}`)

    // 导航项高亮在同一页。
    expect(wrapper!.find('.nav-item.active').text()).toContain('学习准入')
  })

  it('passes state and skill filters to the GET query without a submit button', async () => {
    await signIn((call) => (call.path === '/api/v1/admissions' ? listRows : undefined))
    await visit('/console/admissions')

    const before = calls.filter((call) => call.method === 'GET' && call.path === '/api/v1/admissions')
    expect(before.at(-1)?.query.get('state')).toBeNull()

    await wrapper!.find('#admissions-state').setValue('expired')
    await flushPromises()
    const afterState = calls.filter((call) => call.method === 'GET' && call.path === '/api/v1/admissions').at(-1)
    expect(afterState?.query.get('state')).toBe('expired')

    const skillInput = wrapper!.find('#admissions-skill')
    await skillInput.setValue('  weekly-report  ')
    await skillInput.trigger('change')
    await flushPromises()
    const afterSkill = calls.filter((call) => call.method === 'GET' && call.path === '/api/v1/admissions').at(-1)
    expect(afterSkill?.query.get('skill')).toBe('weekly-report')
  })
})

describe('admission detail', () => {
  it('projects the source, dimensions, decision and publication without write controls', async () => {
    await signIn((call) => (call.path === `/api/v1/admissions/${PUBLISHED_ID}` ? detail : undefined))
    await visit(`/console/admissions/${PUBLISHED_ID}`)

    const view = wrapper!.find('.view-admission-detail')
    expect(view.exists()).toBe(true)
    expect(view.findAll('button')).toHaveLength(0)
    expect(view.text()).toContain('批准 / 发布 / 撤回只在本机 CLI 执行')
    expect(view.text()).toContain('来源声明')
    expect(view.text()).toContain('2026-12-01')
    // 自由备注不整段出站：即使夹具里带着哨兵，页面也不得渲染它。
    expect(view.text()).not.toContain('内部自由备注')
    // 七维判定：不可评估要写明「不等于通过」。
    expect(view.text()).toContain('不可评估（不等于通过）')
    expect(view.text()).toContain('不完整：有维度不可评估，批准需显式确认')
    expect(view.text()).toContain('scripts/collect.sh')
    // 决策：批准者、有效期与确认维度。
    expect(view.text()).toContain('cli:reviewer')
    expect(view.text()).toContain('效果评测')
    expect(view.text()).toContain('成本测量')
    // 发布：版本与撤回分类（restored_previous → 已恢复上一版）。
    expect(view.text()).toContain('v3（版本 ID 12）')
    expect(view.text()).toContain('已把上一版内容作为新版本重新发布')
    expect(view.text()).toContain('已切换到版本 ID 11')
  })

  it('shows explicit empty states when evaluation, decision and publication are absent', async () => {
    await signIn((call) => (call.path === `/api/v1/admissions/${APPROVED_ID}` ? emptyDetail : undefined))
    await visit(`/console/admissions/${APPROVED_ID}`)

    const view = wrapper!.find('.view-admission-detail')
    expect(view.text()).toContain('尚未评估')
    expect(view.text()).toContain('尚未决策')
    expect(view.text()).toContain('尚未发布')
    expect(view.findAll('button')).toHaveLength(0)
  })
})
