// US-16 调用方管理页回归（T026a）：列表 / 建调用方（限额只在创建时写入）/
// 周期用量与超限 / 预留核对 / 签发与吊销 Key。三条契约缺口在页面上如实呈现：
// 没有单个调用方读取端点（概要从列表取）、没有 Key 列表端点（吊销按 key_id）、
// 没有待核对预留列表（核对按 reservation_id）。
// Caller admin pages: list, creation with write-once limits, period usage and
// overage, reservation reconciliation, key issuance and revocation.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type {
  CallerKeySummary,
  CallerSummary,
  CallerUsageView,
  IssuedKey,
  ReservationView,
} from '../src/api-callers'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
  readonly body: unknown
}

type Route = (call: Call) => unknown

const CALLER_ID = 'cal_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'
const KEY_ID = 'key_01J8Z6YQ4M2S3T7V9W0X1Y2Z4B'
const RESERVATION_ID = 'resv_01J8Z6YQ4M2S3T7V9W0X1Y2Z5C'
const ISSUED_SECRET = 'bsk_live_0123456789abcdef'

const caller: CallerSummary = {
  caller_id: CALLER_ID,
  kind: 'external',
  name: '日报接入',
  project: 'ops',
  status: 'active',
  created_at: '2026-10-20T08:00:00Z',
  key_count: 2,
}

const localAdmin: CallerSummary = {
  caller_id: 'local-admin',
  kind: 'builtin',
  name: '本地管理员',
  project: '',
  status: 'active',
  created_at: '2026-10-01T00:00:00Z',
  key_count: 0,
}

function usageView(overage: number, pending = 1): CallerUsageView {
  return {
    caller_id: CALLER_ID,
    period_start: '2026-10-28T00:00:00Z',
    period_end: '2026-10-29T00:00:00Z',
    timezone: 'UTC',
    limits: {
      qps: 2,
      burst: 4,
      max_concurrent: 2,
      tasks_per_period: 100,
      tokens_per_period: 1000,
      ingest_bytes_per_period: 1048576,
      period: 'day',
      timezone: 'UTC',
    },
    usage: [
      { kind: 'run', used: 4, reserved: 1, overage: 0 },
      { kind: 'chat', used: 120, reserved: 0, overage },
      { kind: 'compaction', used: 2, reserved: 0, overage: 0 },
      { kind: 'rerank', used: 0, reserved: 0, overage: 0 },
      { kind: 'embedding_tokens', used: 500, reserved: 0, overage: 0 },
      { kind: 'ingest_bytes', used: 0, reserved: 0, overage: 0 },
    ],
    pending_reservations: pending,
  }
}

function issuedKey(): IssuedKey {
  return {
    key_id: KEY_ID,
    caller_id: CALLER_ID,
    key_prefix: 'bsk_live_01',
    scopes: ['sessions:read', 'agents:invoke:daily-report'],
    status: 'active',
    created_at: '2026-10-28T02:00:00Z',
    expires_at: '',
    revoked_at: '',
    key: ISSUED_SECRET,
  }
}

function revokedKey(): CallerKeySummary {
  return {
    key_id: KEY_ID,
    caller_id: CALLER_ID,
    key_prefix: 'bsk_live_01',
    scopes: ['sessions:read'],
    status: 'revoked',
    created_at: '2026-10-28T02:00:00Z',
    expires_at: '',
    revoked_at: '2026-10-28T03:00:00Z',
  }
}

function reservation(): ReservationView {
  return {
    reservation_id: RESERVATION_ID,
    caller_id: CALLER_ID,
    class: 'run',
    period_start: '2026-10-28T00:00:00Z',
    amount: 3,
    actual: 5,
    overage: 2,
    status: 'reconciled',
  }
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
      timestamp: '2026-10-28T12:00:00Z',
    }),
  } as unknown as Response
}

function defaultReply(call: Call): unknown {
  if (call.path === '/api/v1/info') {
    return { version: 'test-version', uptime_seconds: 3600, agents: 0, tools: 0, mcp_servers: 0, providers: [] }
  }
  // 登录后默认路由是总览页：不提供它的读数会在 flushPromises 后留下未处理的拒绝。
  // The default route after sign-in is the overview view; it needs its own fixtures.
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
  if (call.path === '/api/v1/callers') {
    return call.method === 'POST' ? caller : [caller, localAdmin]
  }
  if (call.path === `/api/v1/callers/${CALLER_ID}/usage`) {
    return usageView(30)
  }
  return []
}

let wrapper: ReturnType<typeof mount> | undefined
let calls: Call[] = []

async function signIn(route: Route = () => undefined): Promise<void> {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const call: Call = {
      method: (init?.method ?? 'GET').toUpperCase(),
      path: new URL(String(input), 'http://localhost').pathname,
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

function writes(method: string, path: string): Call[] {
  return calls.filter((call) => call.method === method && call.path === path)
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('caller list', () => {
  it('links each caller to its detail page and shows the key count', async () => {
    await signIn()
    await visit('/console/callers')

    const headers = wrapper!.findAll('.callers-table thead th').map((cell) => cell.text())
    expect(headers).toEqual(['调用方', '名称', '项目', '种类', '状态', '未吊销 Key', '创建时间'])

    const links = wrapper!.findAll('tbody a.cell-id')
    expect(links.map((cell) => cell.attributes('href'))).toEqual([
      `/console/callers/${CALLER_ID}`,
      '/console/callers/local-admin',
    ])
    const rows = wrapper!.findAll('.callers-table tbody tr')
    expect(rows.map((row) => row.findAll('td')[1]!.text())).toEqual(['日报接入', '本地管理员'])
    expect(rows[0]!.findAll('td')[5]!.text()).toBe('2')
    expect(rows[1]!.findAll('td')[2]!.text()).toBe('—')
    expect(wrapper!.text()).toContain('Key 明文只在签发响应出现一次')
  })

  it('sends limits verbatim at creation time', async () => {
    await signIn()
    await visit('/console/callers')
    await wrapper!.find('#caller-name').setValue('日报接入')
    await wrapper!.find('#caller-project').setValue('ops')
    await wrapper!.find('#caller-limit-qps').setValue('2')
    await wrapper!.find('#caller-limit-burst').setValue('4')
    await wrapper!.find('#caller-limit-max-concurrent').setValue('2')
    await wrapper!.find('#caller-limit-tasks').setValue('100')
    await wrapper!.find('#caller-limit-tokens').setValue('1000')
    await wrapper!.find('#caller-limit-ingest').setValue('1048576')
    await wrapper!.find('.caller-create-form').trigger('submit')
    await flushPromises()

    const [created] = writes('POST', '/api/v1/callers')
    expect(created?.body).toEqual({
      name: '日报接入',
      project: 'ops',
      limits: {
        qps: 2,
        burst: 4,
        max_concurrent: 2,
        tasks_per_period: 100,
        tokens_per_period: 1000,
        ingest_bytes_per_period: 1048576,
        period: 'day',
        timezone: 'UTC',
      },
    })
    expect(wrapper!.text()).toContain(`已创建调用方：${CALLER_ID}`)
  })

  it('omits the limits key entirely when the operator opts out', async () => {
    await signIn()
    await visit('/console/callers')
    await wrapper!.find('#caller-name').setValue('无额度调用方')
    await wrapper!.find('#caller-write-limits').setValue(false)
    await wrapper!.find('.caller-create-form').trigger('submit')
    await flushPromises()

    const [created] = writes('POST', '/api/v1/callers')
    const body = created?.body as Record<string, unknown>
    expect(body).toEqual({ name: '无额度调用方' })
    expect('limits' in body).toBe(false)
    // 页面对后果说清楚：缺限额行 = 周期额度按 0 处理（拒绝），不是「不限量」；
    // QPS / 并发仍走实例预算。文案与 §5.9 的零值语义口径一致（2026-10-31 修正）。
    expect(wrapper!.text()).toContain('周期额度（任务数 / tokens / 摄取字节）按 0 处理，相关请求会被拒绝')
    expect(wrapper!.text()).toContain('QPS 仍按实例入口预算放行')
  })
})

describe('caller detail usage', () => {
  it('shows the six usage lines, the pending count and the overage warning', async () => {
    await signIn()
    await visit(`/console/callers/${CALLER_ID}`)

    const rows = wrapper!.findAll('.usage-table tbody tr')
    expect(rows.map((row) => row.findAll('td')[0]!.text())).toEqual([
      '运行次数',
      '对话 tokens',
      '压缩 tokens',
      '重排',
      '向量化 tokens',
      '摄取字节',
    ])
    expect(rows[1]!.findAll('td').map((cell) => cell.text())).toEqual(['对话 tokens', '120', '0', '30'])
    expect(wrapper!.text()).toContain('新预留一律 429 QUOTA_EXHAUSTED')
    expect(wrapper!.text()).toContain('有 1 条结局未知的预留仍占用额度')
  })

  it('refuses to clear overage without a reason, then clears with the reason', async () => {
    let current = usageView(30)
    await signIn((call) => {
      if (call.method === 'POST' && call.path.endsWith('/usage/clear-overage')) {
        current = usageView(0, 0)
        return current
      }
      if (call.path === `/api/v1/callers/${CALLER_ID}/usage`) {
        return current
      }
      return undefined
    })
    await visit(`/console/callers/${CALLER_ID}`)
    expect(wrapper!.find('#clear-overage').exists()).toBe(true)

    await wrapper!.find('.clear-overage-form').trigger('submit')
    await flushPromises()
    expect(writes('POST', `/api/v1/callers/${CALLER_ID}/usage/clear-overage`)).toHaveLength(0)
    expect(wrapper!.text()).toContain('清零理由必填')

    await wrapper!.find('#clear-overage-reason').setValue('已与调用方确认并补录账单')
    await wrapper!.find('.clear-overage-form').trigger('submit')
    await flushPromises()

    const [cleared] = writes('POST', `/api/v1/callers/${CALLER_ID}/usage/clear-overage`)
    expect(cleared?.body).toEqual({ reason: '已与调用方确认并补录账单' })
    // 清零后超限区块消失：按钮是 overage > 0 的派生视图，不是权限开关。
    expect(wrapper!.find('#clear-overage').exists()).toBe(false)
  })
})

describe('caller keys', () => {
  it('shows the plaintext exactly once and drops it on acknowledgement', async () => {
    await signIn((call) =>
      call.method === 'POST' && call.path.endsWith('/keys') ? issuedKey() : undefined)
    await visit(`/console/callers/${CALLER_ID}`)

    await wrapper!.find('.issue-key-form').trigger('submit')
    await flushPromises()
    expect(writes('POST', `/api/v1/callers/${CALLER_ID}/keys`)).toHaveLength(0)
    expect(wrapper!.text()).toContain('至少选择一个 scope')

    await wrapper!.find('#scope-sessions-read').setValue(true)
    await wrapper!.find('#agent-scopes').setValue('daily-report')
    await wrapper!.find('.issue-key-form').trigger('submit')
    await flushPromises()

    const [issued] = writes('POST', `/api/v1/callers/${CALLER_ID}/keys`)
    expect(issued?.body).toEqual({ scopes: ['sessions:read', 'agents:invoke:daily-report'] })
    expect(wrapper!.find('.issued-key code').text()).toBe(ISSUED_SECRET)
    expect(wrapper!.text()).toContain('只显示这一次')

    // 明文可一键填进吊销表单（key_id），但不进入 URL 或任何持久位置。
    await wrapper!.find('#use-issued-key-id').trigger('click')
    expect((wrapper!.find('#revoke-key-id').element as HTMLInputElement).value).toBe(KEY_ID)
    await wrapper!.find('#dismiss-issued-key').trigger('click')
    await flushPromises()
    expect(wrapper!.text()).not.toContain(ISSUED_SECRET)
  })

  it('revokes through the confirm dialog with the typed key id', async () => {
    await signIn((call) =>
      call.method === 'POST' && call.path.endsWith('/revoke') ? revokedKey() : undefined)
    await visit(`/console/callers/${CALLER_ID}`)

    await wrapper!.find('#revoke-key-id').setValue(KEY_ID)
    await wrapper!.find('#revoke-reason').setValue('接入方轮换')
    await wrapper!.find('.revoke-key-form').trigger('submit')
    await flushPromises()
    expect(writes('POST', `/api/v1/callers/${CALLER_ID}/keys/${KEY_ID}/revoke`)).toHaveLength(0)
    expect(wrapper!.text()).toContain(`吊销 Key ${KEY_ID} 后该 Key 立即失效`)

    await wrapper!.find('.dialog button.primary').trigger('click')
    await flushPromises()

    const [revoked] = writes('POST', `/api/v1/callers/${CALLER_ID}/keys/${KEY_ID}/revoke`)
    expect(revoked?.body).toEqual({ reason: '接入方轮换' })
    expect(wrapper!.text()).toContain('已吊销')
  })
})

describe('reservation reconciliation', () => {
  async function openDetail(route: Route = () => undefined): Promise<void> {
    await signIn(route)
    await visit(`/console/callers/${CALLER_ID}`)
  }

  it('requires an id and a reason before any request', async () => {
    await openDetail()
    await wrapper!.find('.reconcile-form').trigger('submit')
    await flushPromises()
    expect(calls.filter((call) => call.path.includes('/reservations/'))).toHaveLength(0)
    expect(wrapper!.text()).toContain('预留标识必填')
  })

  it('commits with the actual usage and shows the settled row', async () => {
    await openDetail((call) =>
      call.path.includes('/reservations/') ? reservation() : undefined)
    await wrapper!.find('#reservation-id').setValue(RESERVATION_ID)
    await wrapper!.find('#reservation-actual').setValue('5')
    await wrapper!.find('#reconcile-reason').setValue('按账单补录')
    await wrapper!.find('.reconcile-form').trigger('submit')
    await flushPromises()

    const [settled] = calls.filter((call) => call.path.includes('/reservations/'))
    expect(settled?.path).toBe(
      `/api/v1/callers/${CALLER_ID}/reservations/${RESERVATION_ID}/reconcile`,
    )
    expect(settled?.body).toEqual({ outcome: 'committed', actual: 5, reason: '按账单补录' })
    const result = wrapper!.findAll('.caller-reservations dd').map((cell) => cell.text())
    expect(result.slice(3)).toEqual(['3', '5', '2', '已核对'])
  })

  it('releases without an actual value even when one was typed', async () => {
    await openDetail((call) =>
      call.path.includes('/reservations/')
        ? { ...reservation(), actual: undefined, status: 'reconciled' }
        : undefined)
    await wrapper!.find('#reservation-id').setValue(RESERVATION_ID)
    await wrapper!.find('#reservation-outcome').setValue('released')
    await wrapper!.find('#reservation-actual').setValue('5')
    await wrapper!.find('#reconcile-reason').setValue('确认零消耗')
    await wrapper!.find('.reconcile-form').trigger('submit')
    await flushPromises()

    const [settled] = calls.filter((call) => call.path.includes('/reservations/'))
    expect(settled?.body).toEqual({ outcome: 'released', reason: '确认零消耗' })
  })

  it('keeps the committed outcome honest when actual is missing', async () => {
    await openDetail()
    await wrapper!.find('#reservation-id').setValue(RESERVATION_ID)
    await wrapper!.find('#reconcile-reason').setValue('按账单补录')
    await wrapper!.find('.reconcile-form').trigger('submit')
    await flushPromises()
    expect(calls.filter((call) => call.path.includes('/reservations/'))).toHaveLength(0)
    expect(wrapper!.text()).toContain('committed 必须给出实际用量')
  })
})
