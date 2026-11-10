// 分布式执行的只读投影（US-24 §3.1.13 / §3.1.14）：系统概览的 Worker 列表与活跃
// 租约数、运行详情的 Worker / 代次 / 资产包哈希。三条纪律各有用例：能读到的 0 是
// 真实读数、读不到显示“待确认”（不写成空集群）、历史 Run 显示“未记录”（不补当前值）。
// Read-only distributed-execution projections: a readable zero is real, an unreadable
// database is "unknown", and a legacy run says "unrecorded" instead of a guess.
import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleApp } from '../src/app'

const baseSummary = {
  version: 'test-version',
  uptime_seconds: 3600,
  started_at: '2026-10-21T12:00:00Z',
  agents: { total: 1, active: 1, disabled: 0, archived: 0, invalid: 0 },
  runs: { running: 0, last_24h: 1, failed_last_24h: 0 },
  resources: { model_connections: 1, mcp_servers: 0, skills: 1 },
  database: { status: 'ok', schema_version: 12 },
  auth: { mode: 'token', loopback_only: true },
}

const profiles = [{
  name: 'writer',
  display_name: '',
  description: 'writer description',
  provider: 'local-model',
  model: 'model-v1',
  tools: [],
  skills: [],
  schedules: 0,
  registered: true,
  runnable: true,
  problems: [],
  notify_channels: [],
  lifecycle: {
    agent: 'writer',
    state: 'active',
    valid: true,
    generation: 1,
    updated_at: '2026-10-21T12:00:00Z',
    operation_id: 'op-test',
    lifecycle_error: '',
  },
}]

function runDetail(extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    run_id: 'run-24',
    request_id: 'req-24',
    session_id: 'sess-1',
    agent: 'writer',
    channel: 'web',
    trigger: 'api',
    status: 'failed',
    reason: '',
    final_text: '收尾完成',
    iterations: 2,
    tool_calls: 1,
    config_hash: 'config-hash',
    manifest_hash: 'manifest-hash',
    worker_instance_id: '',
    ownership_epoch: 0,
    asset_bundle_hash: '',
    started_at: '2026-10-21T12:00:00Z',
    finished_at: '2026-10-21T12:00:03Z',
    resource_manifest: null,
    isolation_executions: [],
    ...extra,
  }
}

let wrapper: ReturnType<typeof mount> | undefined

function stubFetch(overview: unknown, run: unknown, observation: unknown = null): void {
  vi.stubGlobal('fetch', vi.fn(async (input: string) => {
    const path = new URL(String(input), 'http://localhost').pathname
    let data: unknown = []
    if (path.endsWith('/info')) {
      data = { version: 'test-version', uptime_seconds: 3600, agents: 1, tools: 0, mcp_servers: 0, providers: [] }
    }
    if (path.endsWith('/system/overview')) data = overview
    if (path.endsWith('/system/sample')) {
      data = { sampled_at: '2026-10-21T13:00:00Z', cpu_percent: 1, memory_rss_bytes: 1024, goroutines: 12, unavailable: [] }
    }
    if (path.endsWith('/profiles')) data = profiles
    if (path.endsWith('/mcp-servers')) data = []
    if (path.endsWith('/runs')) data = []
    if (path.endsWith('/instance/secrets')) data = {}
    if (/\/runs\/[^/]+$/.test(path)) data = run
    if (path.endsWith('/observation/timeline')) data = observation
    return {
      ok: true,
      status: 200,
      headers: new Headers(),
      json: async () => ({ code: 'OK', message: '', data, timestamp: '2026-10-21T13:00:00Z' }),
    } as Response
  }))
}

async function signIn(overview: unknown, run: unknown = runDetail()): Promise<void> {
  stubFetch(overview, run)
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

describe('distributed execution projections', () => {
  it('lists registered workers with the cluster-wide active lease count', async () => {
    await signIn({
      ...baseSummary,
      workers: {
        active_leases: 3,
        as_of: '2026-10-21T13:00:00Z',
        counts: { active: 2, stale: 0, stopped: 0 },
        total: 2,
        truncated: false,
        nodes: [
          {
            instance: 'abcdef0123456789abcdef0123456789',
            name: 'worker-a',
            started_at: '2026-10-21T10:00:00Z',
            heartbeat_at: '2026-10-21T12:59:30Z',
            active_leases: 2,
            hostname: 'host-a',
            build_version: 'test-version',
            schema_version: 12,
            heartbeat_age_seconds: 30,
            heartbeat_interval_seconds: 10,
            stale_after_seconds: 30,
            status: 'active',
          },
          {
            instance: '0123456789abcdef0123456789abcdef',
            name: 'worker-b',
            started_at: '2026-10-21T11:00:00Z',
            heartbeat_at: '2026-10-21T12:59:31Z',
            active_leases: 0,
            hostname: 'host-b',
            build_version: 'test-version',
            schema_version: 12,
            heartbeat_age_seconds: 29,
            heartbeat_interval_seconds: 10,
            stale_after_seconds: 30,
            status: 'active',
          },
        ],
      },
    })

    const panel = wrapper!.find('.workers-panel')
    expect(panel.exists()).toBe(true)
    expect(panel.text()).toContain('2 个 Worker')
    expect(panel.text()).toContain('全库活跃执行权 3 个')
    expect(panel.text()).toContain('worker-a')
    expect(panel.text()).toContain('2 个活跃租约')
    // 能读到的 0 是真实读数：worker-b 有行、计数为 0，面板仍然列出它。
    expect(panel.text()).toContain('worker-b')
    expect(panel.text()).toContain('0 个活跃租约')
  })

  it('shows workers as unknown when the database reading is omitted', async () => {
    // 数据库不可读时后端整组省略：页面**不得**把它渲染成“没有 Worker”的空集群。
    await signIn(baseSummary)

    const panel = wrapper!.find('.workers-panel')
    expect(panel.exists()).toBe(true)
    expect(panel.text()).toContain('待确认')
    expect(panel.text()).toContain('数据库不可读')
    expect(panel.text()).not.toContain('还没有 Worker 登记')
  })

  it('renders lease ownership and bundle hash on the run detail page', async () => {
    await signIn(
      { ...baseSummary, workers: { active_leases: 0, as_of: '2026-10-21T13:00:00Z', counts: { active: 0, stale: 0, stopped: 0 }, total: 0, truncated: false, nodes: [] } },
      runDetail({
        worker_instance_id: 'abcdef0123456789',
        ownership_epoch: 7,
        asset_bundle_hash: 'ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff',
      }),
    )
    await visit('/console/runs/run-24')

    const text = wrapper!.find('.view-run-detail').text()
    expect(text).toContain('执行 Worker')
    expect(text).toContain('归属代次')
    expect(text).toContain('7')
    expect(text).toContain('资产包哈希')
  })

  it('marks unrecorded lease fields instead of fabricating values', async () => {
    // 历史 Run 没有租约行、旧清单没有资产包哈希：三项都显示“未记录”。
    await signIn({ ...baseSummary, workers: { active_leases: 0, as_of: '2026-10-21T13:00:00Z', counts: { active: 0, stale: 0, stopped: 0 }, total: 0, truncated: false, nodes: [] } }, runDetail())
    await visit('/console/runs/run-24')

    const text = wrapper!.find('.view-run-detail').text()
    const unrecorded = text.match(/未记录/g) ?? []
    expect(unrecorded.length).toBeGreaterThanOrEqual(3)
  })
})
