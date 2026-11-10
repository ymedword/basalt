// US-25C §2.3.9 概览「分布式执行」：服务端三态读数、有界明细、截断、读取失败与
// 禁用措辞。状态一律来自服务端字段——浏览器时钟偏差不得改变任何一行；同名实例
// 不得合并。读取失败显示「状态不可用」，不补 0、不渲染成全部已停止。
// Overview worker readouts: server-side statuses, bounded detail, truncation and
// read failures. Statuses are rendered from the server field only.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { SystemWorkerView } from '../src/api'
import { createConsoleApp } from '../src/app'
import {
  formatHeartbeatAge,
  groupWorkers,
  truncationText,
  workerDetailPairs,
  workerStatusLabel,
} from '../src/components/overview-worker-labels'

interface WorkerCounts {
  active: number
  stale: number
  stopped: number
}

const baseSummary = {
  version: 'test-version',
  uptime_seconds: 3600,
  started_at: '2026-11-09T10:00:00Z',
  agents: { total: 0, active: 0, disabled: 0, archived: 0, invalid: 0 },
  runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
  resources: { model_connections: 0, mcp_servers: 0, skills: 0 },
  database: { status: 'ok', schema_version: 13 },
  auth: { mode: 'token', loopback_only: true },
}

/** 完整登记行：默认值代表一个近期有心跳的实例。 */
function workerNode(overrides: Partial<SystemWorkerView> = {}): SystemWorkerView {
  return {
    instance: 'instance-0001',
    name: 'worker-a',
    started_at: '2026-11-09T10:00:00Z',
    heartbeat_at: '2026-11-09T12:00:00Z',
    active_leases: 0,
    hostname: 'host-a',
    build_version: '0.1.0',
    schema_version: 13,
    heartbeat_age_seconds: 5,
    heartbeat_interval_seconds: 10,
    stale_after_seconds: 30,
    status: 'active',
    ...overrides,
  }
}

function workersView(
  nodes: SystemWorkerView[],
  counts: WorkerCounts,
  extra: { activeLeases?: number; total?: number; truncated?: boolean; asOf?: string } = {},
): Record<string, unknown> {
  return {
    active_leases: extra.activeLeases ?? 0,
    as_of: extra.asOf ?? '2026-11-09T12:00:00Z',
    counts,
    total: extra.total ?? counts.active + counts.stale + counts.stopped,
    truncated: extra.truncated ?? false,
    nodes,
  }
}

function overviewWith(workers: Record<string, unknown>): Record<string, unknown> {
  return { ...baseSummary, workers }
}

interface StubState {
  overview: unknown
  failOverview: boolean
}

let wrapper: ReturnType<typeof mount> | undefined

function stubFetch(state: StubState): void {
  vi.stubGlobal('fetch', vi.fn(async (input: string) => {
    const path = new URL(String(input), 'http://localhost').pathname
    if (path.endsWith('/system/overview') && state.failOverview) {
      throw new Error('overview unavailable')
    }
    let data: unknown = []
    if (path.endsWith('/info')) {
      data = { version: 'test-version', uptime_seconds: 3600, agents: 0, tools: 0, mcp_servers: 0, providers: [] }
    }
    if (path.endsWith('/system/overview')) data = state.overview
    if (path.endsWith('/system/sample')) {
      data = { sampled_at: '2026-11-09T12:00:00Z', cpu_percent: 1, memory_rss_bytes: 1024, goroutines: 12, unavailable: [] }
    }
    if (path.endsWith('/profiles')) data = []
    if (path.endsWith('/mcp-servers')) data = []
    if (path.endsWith('/runs')) data = []
    if (path.endsWith('/instance/secrets')) data = {}
    return {
      ok: true,
      status: 200,
      headers: new Headers(),
      json: async () => ({ code: 'OK', message: '', data, timestamp: '2026-11-09T12:00:00Z' }),
    } as Response
  }))
}

async function signIn(state: StubState): Promise<void> {
  stubFetch(state)
  window.history.replaceState(null, '', '/console/login')
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

function panel(): ReturnType<NonNullable<typeof wrapper>['find']> {
  return wrapper!.find('.workers-panel')
}

function countBlocks(): { label: string; value: string }[] {
  return wrapper!.findAll('.worker-metrics .overview-metric').map(block => ({
    label: block.find('.metric-label').text(),
    value: block.find('.metric-value').text(),
  }))
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('overview worker labels', () => {
  it('formats the server-provided heartbeat age without reading the clock', () => {
    expect(formatHeartbeatAge(0)).toBe('刚刚')
    expect(formatHeartbeatAge(12)).toBe('12 秒前')
    expect(formatHeartbeatAge(90)).toBe('1 分钟前')
    expect(formatHeartbeatAge(7200)).toBe('2 小时前')
    expect(formatHeartbeatAge(172800)).toBe('2 天前')
    expect(formatHeartbeatAge(undefined)).toBe('未知')
  })

  it('builds the truncation copy only when the detail is bounded', () => {
    expect(truncationText(200, 250)).toBe('当前展示 200 条，共 250 条')
    expect(truncationText(2, 2)).toBe('')
  })

  it('keeps same-name instances in separate rows when grouping', () => {
    const groups = groupWorkers([
      workerNode({ instance: 'instance-a', name: 'same-name' }),
      workerNode({ instance: 'instance-b', name: 'same-name' }),
    ])
    expect(groups.active).toHaveLength(2)
  })

  it('labels legacy rows with the deployment default threshold', () => {
    const legacy = workerNode({ heartbeat_interval_seconds: undefined })
    const text = workerDetailPairs(legacy).map(([label, value]) => `${label}:${value}`).join('\n')
    expect(text).toContain('阈值取部署默认')
    expect(workerStatusLabel('unknown')).toBe('待确认')
  })
})

describe('overview distributed-execution panel', () => {
  it('renders the three server counts and the cluster-wide lease count', async () => {
    await signIn({
      overview: overviewWith(workersView(
        [
          workerNode({ instance: 'instance-1', name: 'worker-a' }),
          workerNode({ instance: 'instance-2', name: 'worker-b', status: 'stale', heartbeat_age_seconds: 120 }),
          workerNode({ instance: 'instance-3', name: 'worker-c', status: 'stopped', stopped_at: '2026-11-09T11:00:00Z' }),
        ],
        { active: 2, stale: 1, stopped: 3 },
        { activeLeases: 7 },
      )),
      failOverview: false,
    })

    expect(countBlocks()).toEqual([
      { label: '心跳正常', value: '2' },
      { label: '心跳超时待确认', value: '1' },
      { label: '已停止', value: '3' },
    ])
    expect(panel().text()).toContain('全库活跃执行权 7 个')
    expect(panel().text()).toContain('最近更新于')
    // 已停止组默认折叠，组计数在 summary 上始终可见。
    expect(panel().find('.workers-stopped').attributes('open')).toBeUndefined()
    expect(panel().find('.workers-stopped').text()).toContain('已停止 · 3 个实例')
  })

  it('never turns 37 process instances into 37 machines', async () => {
    const nodes = Array.from({ length: 37 }, (_, index) =>
      workerNode({ instance: `instance-${index}`, name: `worker-${index}` }),
    )
    await signIn({
      overview: overviewWith(workersView(nodes, { active: 37, stale: 0, stopped: 0 })),
      failOverview: false,
    })

    const text = panel().text()
    expect(text).toContain('37 个 Worker 实例')
    expect(text).not.toContain('37 台')
    expect(text).not.toContain('台机器')
    expect(panel().findAll('.worker-row')).toHaveLength(37)
  })

  it('expands only the first five stale rows and keeps the rest collapsed', async () => {
    const nodes = Array.from({ length: 7 }, (_, index) =>
      workerNode({
        instance: `instance-stale-${index}`,
        name: `stale-${index}`,
        status: 'stale',
        heartbeat_age_seconds: 100 + index,
      }),
    )
    await signIn({
      overview: overviewWith(workersView(nodes, { active: 0, stale: 7, stopped: 0 })),
      failOverview: false,
    })

    const group = wrapper!.find('.workers-stale')
    expect(group.text()).toContain('7 个实例')
    const overflow = group.find('.workers-overflow')
    expect(overflow.exists()).toBe(true)
    expect(overflow.attributes('open')).toBeUndefined()
    expect(overflow.text()).toContain('展开其余 2 条')
    expect(overflow.findAll('.worker-row')).toHaveLength(2)
    // 最近的在前：stale-0 的心跳年龄最小，排在第一行。
    expect(group.findAll('.worker-row')[0]?.text()).toContain('stale-0')
  })

  it('renders a stale instance as pending confirmation, never as stopped', async () => {
    await signIn({
      overview: overviewWith(workersView(
        [workerNode({ instance: 'instance-stale', name: 'worker-stale', status: 'stale', heartbeat_age_seconds: 600 })],
        { active: 0, stale: 1, stopped: 0 },
      )),
      failOverview: false,
    })

    const staleGroup = wrapper!.find('.workers-stale')
    expect(staleGroup.exists()).toBe(true)
    expect(staleGroup.text()).toContain('待确认')
    expect(staleGroup.text()).not.toContain('已停止')
    expect(staleGroup.text()).not.toContain('进程已死亡')
    expect(staleGroup.findAll('.worker-row-stale')).toHaveLength(1)
  })

  it('shows bounded detail with an honest truncation line', async () => {
    const nodes = Array.from({ length: 200 }, (_, index) =>
      workerNode({ instance: `instance-${index}`, name: `worker-${index}`, heartbeat_age_seconds: index }),
    )
    await signIn({
      overview: overviewWith(workersView(
        nodes,
        { active: 200, stale: 30, stopped: 20 },
        { total: 250, truncated: true, activeLeases: 120 },
      )),
      failOverview: false,
    })

    expect(panel().text()).toContain('当前展示 200 条，共 250 条')
    expect(panel().text()).toContain('basalt workers list')
    // 计数来自服务端聚合：明细里没有 stale 行，但计数仍是 30。
    expect(countBlocks()).toEqual([
      { label: '心跳正常', value: '200' },
      { label: '心跳超时待确认', value: '30' },
      { label: '已停止', value: '20' },
    ])
    expect(panel().findAll('.worker-row')).toHaveLength(200)
    // 明细截断时不得把「本页没有行」写成「当前没有实例」，否则与组计数矛盾。
    expect(panel().find('.workers-stale').text()).toContain('本页未包含心跳超时')
    expect(panel().find('.workers-stale').text()).not.toContain('当前没有')
    expect(panel().find('.workers-stopped').text()).toContain('本页未包含已停止')
  })

  it('does not let a skewed browser clock change the server status', async () => {
    // 浏览器时钟拨到 2035 年；服务端给出的年龄与状态互相“矛盾”，页面必须只认 status。
    vi.spyOn(Date, 'now').mockReturnValue(new Date('2035-01-01T00:00:00Z').getTime())
    await signIn({
      overview: overviewWith(workersView(
        [
          workerNode({ instance: 'instance-active', name: 'server-says-active', status: 'active', heartbeat_age_seconds: 999999 }),
          workerNode({ instance: 'instance-stale', name: 'server-says-stale', status: 'stale', heartbeat_age_seconds: 1 }),
        ],
        { active: 1, stale: 1, stopped: 0 },
      )),
      failOverview: false,
    })

    const rows = panel().findAll('.worker-row')
    const activeRow = rows.find(row => row.text().includes('server-says-active'))
    const staleRow = rows.find(row => row.text().includes('server-says-stale'))
    expect(activeRow?.text()).toContain('心跳正常')
    expect(staleRow?.text()).toContain('心跳超时待确认')
  })

  it('shows the reading as unavailable instead of zero when the group is omitted', async () => {
    await signIn({ overview: baseSummary, failOverview: false })

    const text = panel().text()
    expect(text).toContain('状态不可用')
    expect(text).toContain('数据库不可读')
    expect(text).toContain('待确认')
    expect(text).not.toContain('还没有 Worker 登记')
    expect(panel().find('.worker-metrics').exists()).toBe(false)
  })

  it('shows the reading as unavailable instead of zero when the request fails', async () => {
    await signIn({
      overview: overviewWith(workersView([], { active: 0, stale: 0, stopped: 0 })),
      failOverview: true,
    })

    const text = panel().text()
    expect(text).toContain('状态不可用')
    expect(text).not.toContain('0 个实例')
    expect(panel().find('.worker-metrics').exists()).toBe(false)
  })

  it('keeps the last reading but marks it expired when a refresh fails', async () => {
    const state: StubState = {
      overview: overviewWith(workersView(
        [workerNode({ instance: 'instance-1', name: 'worker-kept' })],
        { active: 1, stale: 0, stopped: 0 },
      )),
      failOverview: false,
    }
    await signIn(state)
    expect(panel().text()).toContain('worker-kept')

    state.failOverview = true
    await wrapper!.find('.refresh-button').trigger('click')
    await flushPromises()

    const text = panel().text()
    expect(text).toContain('数据已过期')
    expect(text).toContain('状态不可用')
    expect(text).toContain('最近更新于')
    // 保留上次读数，但明确标注过期。
    expect(text).toContain('worker-kept')
  })

  it('renders two same-name instances as two rows', async () => {
    await signIn({
      overview: overviewWith(workersView(
        [
          workerNode({ instance: 'instance-a', name: 'worker-a', hostname: 'host-a' }),
          workerNode({ instance: 'instance-b', name: 'worker-a', hostname: 'host-b' }),
        ],
        { active: 2, stale: 0, stopped: 0 },
      )),
      failOverview: false,
    })

    expect(panel().findAll('.worker-row')).toHaveLength(2)
    expect(panel().text()).toContain('2 个 Worker 实例')
  })

  it('notes the deployment default for legacy rows in the details', async () => {
    await signIn({
      overview: overviewWith(workersView(
        [
          workerNode({ instance: 'instance-legacy', name: 'legacy-row', heartbeat_interval_seconds: undefined }),
          workerNode({ instance: 'instance-fresh', name: 'fresh-row', heartbeat_interval_seconds: 60 }),
        ],
        { active: 2, stale: 0, stopped: 0 },
      )),
      failOverview: false,
    })

    const texts = panel().findAll('.worker-row').map(row => row.text())
    expect(texts[0]).toContain('阈值取部署默认')
    expect(texts[1]).toContain('60 秒')
    expect(texts[1]).not.toContain('阈值取部署默认')
  })

  it('avoids forbidden wording across all three groups', async () => {
    await signIn({
      overview: overviewWith(workersView(
        [
          workerNode({ instance: 'instance-active', name: 'worker-a' }),
          workerNode({ instance: 'instance-stale', name: 'worker-b', status: 'stale', heartbeat_age_seconds: 600 }),
          workerNode({ instance: 'instance-stopped', name: 'worker-c', status: 'stopped', stopped_at: '2026-11-09T11:00:00Z' }),
        ],
        { active: 1, stale: 1, stopped: 1 },
        { activeLeases: 2 },
      )),
      failOverview: false,
    })

    const text = panel().text()
    for (const phrase of ['正在运行任务', '健康', '整个宿主已下线', '正在执行的任务数', '台机器']) {
      expect(text).not.toContain(phrase)
    }
  })

  it('renders a readable zero instead of unknown when the reading is real', async () => {
    await signIn({
      overview: overviewWith(workersView([], { active: 0, stale: 0, stopped: 0 })),
      failOverview: false,
    })

    expect(countBlocks()).toEqual([
      { label: '心跳正常', value: '0' },
      { label: '心跳超时待确认', value: '0' },
      { label: '已停止', value: '0' },
    ])
    expect(panel().text()).toContain('还没有 Worker 登记')
    expect(panel().text()).not.toContain('状态不可用')
  })
})
