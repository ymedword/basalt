// Agent 三段面板回归：阻断项只来自就绪视图的 pending、通知提示不进阻断段、
// 「系统已准备」只陈述读得到的事实（服务版本 / MCP 版本 / 脚本目录），
// 读取失败显式写「不可读」而不是伪装就绪。
// Agent three-panel regressions: blockers come only from `pending`, notify advisories
// never enter the blocker panel, prepared facts are only what is readable, and read
// failures say "unreadable" instead of pretending readiness.
import { mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import type {
  AgentConfigView,
  AgentReadinessView,
  McpServerView,
} from '../src/api'
import type { WorkspaceEntry } from '../src/api-admin'
import { AgentReadinessPanels } from '../src/components/agent-readiness'

function readiness(
  pending: AgentReadinessView['pending'] = [],
  notices: AgentReadinessView['notices'] = [],
): AgentReadinessView {
  return { agent: 'demo', ready: pending.length === 0, pending, notices }
}

function config(overrides: Partial<AgentConfigView['fields']> = {}): AgentConfigView {
  return {
    agent: 'demo',
    agent_uid: 'ag_test',
    content_etag: '"sha256:test"',
    fields: {
      name: 'demo',
      description: '',
      display_name: '',
      identity: { agent_name: 'demo', prompt: '' },
      provider: { name: '', model: '', temperature: null },
      tools: [],
      skills: [],
      mcp_servers: [],
      bootstrap: [],
      notify_mode: 'none',
      notify_channels: [],
      resource_bindings: {} as AgentConfigView['fields']['resource_bindings'],
      settings: { max_iterations: 8, max_history_turns: 6 },
      schedules: [],
      ...overrides,
    },
    notify_mode: 'none',
    notify_mode_source: 'explicit',
    references: [],
    binding_complete: true,
  }
}

function mcp(overrides: Partial<McpServerView> = {}): McpServerView {
  return {
    name: 'gh',
    transport: 'stdio',
    command: 'npx',
    env_keys: ['GITHUB_TOKEN'],
    header_keys: [],
    credential_keys: [{ key: 'GITHUB_TOKEN', mode: 'env', state: 'env', last4: '' }],
    connected: true,
    tools: [],
    published_version_id: 1,
    published_version_no: 1,
    applied_version_id: 1,
    applied_version_no: 1,
    pending_restart: false,
    tool_disposition: null,
    ...overrides,
  }
}

function mountPanels(props: Record<string, unknown>): VueWrapper {
  return mount(AgentReadinessPanels, { props })
}

/** 三段面板按设计顺序渲染：需要你提供 / 系统已准备 / 未启用原因。 */
function panelTexts(wrapper: VueWrapper): string[] {
  const panels = wrapper.findAll('.ready-panel')
  expect(panels).toHaveLength(3)
  return panels.map((panel) => panel.text())
}

describe('AgentReadinessPanels', () => {
  it('lists blockers first and keeps notify advisories out of the blocker count', () => {
    const wrapper = mountPanels({
      readiness: readiness(
        [
          {
            kind: 'model_connection',
            item: 'model_connection/default',
            detail: '未绑定模型连接。',
            fix: '/console/resources/model-connections',
          },
          {
            kind: 'credential',
            item: 'mcp_server/gh#GITHUB_TOKEN',
            detail: '未配置环境变量 GITHUB_TOKEN。',
            fix: '/console/resources/mcp-servers/gh',
          },
        ],
        [
          {
            kind: 'notify_channel',
            item: '',
            detail: '未配置通知渠道，结果只留在运行记录中。',
            fix: '/console/settings/notifications',
          },
          { kind: 'budget', item: 'budget/daily', detail: '已接近预算上限。', fix: '' },
        ],
      ),
      config: config({ mcp_servers: ['gh'], notify_mode: 'all_eligible' }),
      mcps: [],
      info: { version: '0.1.0', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] },
      entries: [{ path: 'scripts', type: 'dir', size: 0 }],
      lifecycle: 'disabled',
    })

    const [provide, prepared, reason] = panelTexts(wrapper)
    expect(provide).toContain('模型连接')
    expect(provide).toContain('未绑定模型连接。')
    // 凭据行按机器名拆成 server 与 key，不让运维去猜 `mcp_server/gh#KEY` 的含义。
    expect(provide).toContain('gh 凭据（GITHUB_TOKEN）')
    expect(provide).toContain('模型与连接 →')
    expect(provide).toContain('MCP 详情 →')
    // 通知提示出现在阻断段，但状态词是「提示」，且不参与「还差 N 项」。
    expect(provide).toContain('通知渠道（可选）')
    expect(provide).toContain('提示')
    expect(reason).toContain('还差 2 项')
    expect(reason).not.toContain('还差 3 项')
    expect(reason).toContain('已接近预算上限。')

    // 系统已准备：只写读得到的事实。
    expect(prepared).toContain('0.1.0（运行时组件随安装包提供）')
    expect(prepared).toContain('MCP gh')
    expect(prepared).toContain('未安装或未发布。')
    expect(prepared).toContain('已就位。')
    expect(prepared).toContain('目录不存在（自定义 Agent 不要求）。')
  })

  it('reports satisfied categories once nothing blocks them', () => {
    const wrapper = mountPanels({
      readiness: readiness(),
      config: config({
        provider: { name: 'local', model: 'm1', temperature: null },
        mcp_servers: ['gh'],
      }),
      mcps: [
        mcp({
          connected: true,
          published_version_no: 2,
          applied_version_no: 2,
          tools: ['search', 'read'],
        }),
      ],
      info: { version: '0.1.0', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 1, providers: [] },
      entries: [],
      lifecycle: 'active',
    })

    const [provide, prepared, reason] = panelTexts(wrapper)
    expect(provide).toContain('local / m1')
    expect(provide).toContain('1 个槽位已配置（加密托管或环境变量）')
    // notify_mode=none 是「不需要」，不是「缺通知渠道」。
    expect(provide).toContain('此 Agent 不发送通知，无需配置。')
    expect(provide).toContain('不需要')
    expect(prepared).toContain('已连接 · 已应用 v2 · 提供 2 个工具')
    expect(reason).toContain('已启用')
    expect(reason).toContain('启用前置已满足，按计划运行。')
  })

  it('names both versions while an MCP publish awaits restart', () => {
    const wrapper = mountPanels({
      readiness: readiness(),
      config: config({ mcp_servers: ['gh', 'zeta'] }),
      mcps: [mcp({ pending_restart: true, published_version_no: 3, applied_version_no: 2 })],
      lifecycle: 'active',
    })

    const prepared = panelTexts(wrapper)[1]
    expect(prepared).toContain('已发布 v3，运行中仍是 v2，需重启服务后生效。')
    expect(prepared).toContain('待重启')
    expect(prepared).toContain('MCP zeta')
    expect(prepared).toContain('未安装或未发布。')
  })

  it('marks unreadable sources explicitly instead of showing them as ready', () => {
    const wrapper = mountPanels({
      readiness: null,
      readinessError: 'permission denied',
      config: null,
      configError: 'agent.md 不可读',
      mcps: null,
      info: null,
      infoError: 'offline',
      entries: null,
      entriesError: '',
      lifecycle: null,
    })

    const [provide, prepared, reason] = panelTexts(wrapper)
    expect(provide).toContain('读取失败：permission denied')
    expect(provide).toContain('不可读')
    expect(provide).toContain('读取失败：agent.md 不可读')
    expect(reason).toContain('无法确认启用前置')
    expect(prepared).toContain('读取失败：offline')
    expect(prepared).toContain('正在读取 Agent 目录…')
  })

  it('treats array-shaped payloads as loading rather than crashing or guessing', () => {
    const wrapper = mountPanels({
      readiness: [] as unknown,
      config: [] as unknown,
      mcps: null,
      info: [] as unknown,
      entries: [] as WorkspaceEntry[],
      lifecycle: 'active',
    })

    const [provide, prepared] = panelTexts(wrapper)
    expect(provide).toContain('正在计算离能跑还差什么…')
    expect(prepared).toContain('Agent 定义读取中或不可读，暂不能核对组件。')
    // info 非对象 → 按读取中；不把 `undefined` 版本渲染进页面。
    expect(prepared).toContain('正在读取服务版本…')
  })
})
