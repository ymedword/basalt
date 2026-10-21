// 系统总览：按服务状态、待处理问题、运行读数组织已有 API 的真实数据。
// System overview prioritizes readiness, actionable issues and measured process data.
import { computed, defineComponent, h } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { Notice, formatBytes, formatTimestamp, formatUptime } from '../components/ui'
import { brandMark } from '../components/brand'
import { agentHref, resourceHref, runHref } from '../router'
import { badge, RUN_STATUS_LABELS, useLoader, viewProps } from './shared'

function metric(label: string, value: string, note: VNodeChild, accent = false): VNode {
  return h('div', { class: ['overview-metric', accent ? 'metric-accent' : ''] }, [
    h('span', { class: 'metric-label' }, label), h('strong', { class: 'metric-value' }, value), h('div', { class: 'metric-note' }, [note]),
  ])
}
export const OverviewView = defineComponent({
  name: 'OverviewView', props: viewProps,
  setup(props) {
    const api = props.ctx.api
    const info = useLoader(() => api.getInfo())
    const overview = useLoader(() => api.getSystemOverview())
    const sample = useLoader(() => api.getSystemSample())
    const agents = useLoader(() => api.listProfiles())
    const mcps = useLoader(() => api.listMcpServers())
    const recent = useLoader(() => api.listRuns({ limit: 5 }))
    const loaders = [info, overview, sample, agents, mcps, recent]
    const busy = computed(() => loaders.some(loader => loader.loading.value))
    const errors = computed(() => loaders.map(loader => loader.error.value).filter(Boolean))
    function refresh(): void { void Promise.all(loaders.map(loader => loader.reload())) }
    return (): VNode => {
      const summary = overview.data.value
      const system = info.data.value
      const readings = sample.data.value
      const alerts: { title: string; detail: string; href: string; kind: 'bad' | 'warn' }[] = []
      for (const agent of agents.data.value ?? []) {
        if (!agent.lifecycle.valid || !agent.registered || agent.problems.length > 0) {
          alerts.push({ title: agent.name, detail: agent.problems.join('；') || agent.lifecycle.lifecycle_error || 'Agent 尚未注册，请检查配置。', href: agentHref(agent.name), kind: 'warn' })
        }
      }
      for (const mcp of mcps.data.value ?? []) {
        if (!mcp.connected || mcp.pending_restart) alerts.push({ title: mcp.name + ' · MCP', detail: mcp.pending_restart ? '发布版本尚未应用，需要重启服务。' : '当前没有建立连接，请检查服务定义。', href: resourceHref('mcp-servers'), kind: 'warn' })
      }
      if (summary !== null && summary.runs.failed_last_24h > 0) {
        alerts.unshift({ title: '近 24 小时有 ' + summary.runs.failed_last_24h + ' 次运行失败', detail: '查看执行记录，定位失败原因与相关 Agent。', href: '/console/runs?status=failed', kind: 'bad' })
      }
      const complete = agents.data.value !== null && mcps.data.value !== null && summary !== null && errors.value.length === 0
      const status = errors.value.length > 0 ? '状态待确认' : summary === null ? '正在连接' : summary.database.status === 'ok' ? '服务已连接' : '存储不可用'
      const statusTone = errors.value.length > 0 || summary?.database.status === 'unavailable' ? 'warn' : summary === null ? 'muted' : 'ok'
      return h('section', { class: 'view view-overview' }, [
        h('div', { class: 'page-heading' }, [
          h('div', [h('p', { class: 'eyebrow' }, 'WORKSPACE / 01'), h('h1', '系统总览'), h('p', { class: 'page-description' }, '服务、运行与 Agent 的当前状态。')]),
          h('button', { class: 'refresh-button', type: 'button', disabled: busy.value, onClick: refresh }, busy.value ? '正在更新…' : '↻  刷新状态'),
        ]),
        ...errors.value.map((text, index) => h(Notice, { key: index, tone: 'error', text })),
        h('div', { class: 'system-strip' }, [
          h('div', { class: 'system-identity' }, [h('span', { class: 'system-seal' }, brandMark()), h('div', [h('strong', 'Basalt 服务'), h('p', [h('code', system?.version ?? '—'), h('span', ' · 已运行 ' + (system ? formatUptime(system.uptime_seconds) : '—'))])])]),
          h('div', { class: 'system-strip-status' }, [badge(status, statusTone), h('span', { class: 'muted small' }, '以本次接口读取结果为准')]),
        ]),
        h('div', { class: 'overview-metrics' }, [
          metric('正在运行', summary ? String(summary.runs.running) : '—', '当前执行中的 Run', true),
          metric('近 24 小时执行', summary ? String(summary.runs.last_24h) : '—', h('a', { href: '/console/runs' }, '查看运行列表 →')),
          metric('已启用 Agents', summary ? summary.agents.active + ' / ' + summary.agents.total : '—', h('a', { href: '/console/agents' }, '进入 Agent 工作区 ↗')),
          metric('已发布资源', summary ? String(summary.resources.model_connections + summary.resources.mcp_servers + summary.resources.skills) : '—', summary ? summary.resources.model_connections + ' 模型 · ' + summary.resources.mcp_servers + ' MCP · ' + summary.resources.skills + ' 技能' : '正在读取资源目录'),
        ]),
        h('div', { class: 'overview-columns' }, [
          h('section', { class: 'overview-panel attention-panel' }, [
            h('div', { class: 'panel-heading' }, [h('h2', '需要关注'), badge(complete && alerts.length === 0 ? '无待处理项' : String(alerts.length) + ' 项', alerts.length > 0 ? 'warn' : complete ? 'ok' : 'muted')]),
            alerts.length > 0 ? h('div', { class: 'attention-list' }, alerts.slice(0, 5).map(item => h('a', { key: item.title, href: item.href, class: 'attention-item' }, [
              h('span', { class: ['attention-marker', item.kind], 'aria-hidden': 'true' }, '!'), h('div', [h('strong', item.title), h('p', item.detail)]), h('span', { 'aria-hidden': 'true' }, '↗'),
            ]))) : h('div', { class: 'clear-state' }, [h('span', { class: 'clear-state-symbol', 'aria-hidden': 'true' }, complete ? '✓' : '·'), h('strong', complete ? '当前没有待处理的配置问题' : '正在确认工作区状态'), h('p', complete ? '进入 Agent 工作区，查看会话、执行与配置。' : '读取完成后，这里会列出需要处理的问题。'), h('a', { href: '/console/agents' }, '查看 Agents →')]),
            alerts.length > 5 ? h('p', { class: 'muted small' }, '另有 ' + (alerts.length - 5) + ' 项，请进入对应目录查看。') : null,
          ]),
          h('section', { class: 'overview-panel dependencies-panel' }, [
            h('div', { class: 'panel-heading' }, [h('h2', '连接与环境'), h('span', { class: 'eyebrow' }, 'DEPENDENCIES')]),
            h('div', { class: 'dependency-row' }, [h('div', [h('strong', '数据库'), h('p', '结构版本 ' + (summary ? summary.database.schema_version : '—'))]), badge(summary ? summary.database.status === 'ok' ? '就绪' : '不可用' : '待确认', summary ? summary.database.status === 'ok' ? 'ok' : 'bad' : 'muted')]),
            h('a', { class: 'dependency-row', href: resourceHref('model-connections') }, [h('div', [h('strong', '模型连接'), h('p', '配置已加载；未执行模型探测')]), h('span', { class: 'dependency-count' }, summary ? summary.resources.model_connections + ' 项 ↗' : '—')]),
            h('a', { class: 'dependency-row', href: resourceHref('mcp-servers') }, [h('div', [h('strong', 'MCP 服务'), h('p', '当前实际建立的服务连接')]), h('span', { class: 'dependency-count' }, mcps.data.value ? mcps.data.value.filter(item => item.connected).length + ' / ' + mcps.data.value.length + ' ↗' : '—')]),
            h('div', { class: 'dependency-row' }, [h('div', [h('strong', '管理面认证'), h('p', summary ? summary.auth.loopback_only ? '仅回环访问' : '允许远程访问' : '正在读取')]), h('span', { class: 'dependency-count' }, summary ? summary.auth.mode === 'token' ? 'Token' : '本机信任' : '—')]),
          ]),
        ]),
        h('section', { class: 'overview-panel process-panel' }, [
          h('div', { class: 'panel-heading' }, [h('h2', '进程读数'), h('span', { class: 'muted small' }, readings ? '采样于 ' + formatTimestamp(readings.sampled_at) : '尚未取得采样')]),
          h('div', { class: 'process-readings' }, [
            metric('CPU 使用率', readings?.cpu_percent == null ? '暂不可用' : readings.cpu_percent.toFixed(1) + '%', '当前服务进程'),
            metric('常驻内存', readings ? formatBytes(readings.memory_rss_bytes) : '—', 'RSS / 物理内存驻留'),
            metric('Goroutine', readings?.goroutines == null ? '—' : String(readings.goroutines), '当前 Go 协程数'),
          ]),
          readings?.unavailable.length ? h('p', { class: 'muted small sample-note' }, '本次不可用：' + readings.unavailable.join('、') + '。首次 CPU 采样需要建立比较基线。') : null,
        ]),
        h('section', { class: 'overview-panel recent-panel' }, [
          h('div', { class: 'panel-heading' }, [h('h2', '最近执行'), h('a', { href: '/console/runs', class: 'small' }, '查看运行列表 →')]),
          recent.data.value === null ? h('p', { class: 'loading' }, recent.error.value ? '执行记录暂不可用' : '正在读取执行记录…') : recent.data.value.length === 0 ? h('p', { class: 'empty' }, '还没有执行记录。Agent 运行后，结果会显示在这里。') : h('div', { class: 'recent-list' }, [
            h('div', { class: 'recent-labels', 'aria-hidden': 'true' }, [h('span', 'Agent'), h('span', '结果'), h('span', '迭代 / 工具调用'), h('span', '开始时间'), h('span')]),
            ...recent.data.value.map(run => h('a', { key: run.run_id, class: 'recent-run', href: runHref(run.run_id), 'aria-label': `查看 ${run.agent} 的运行详情 ${run.run_id}` }, [
              h('strong', { class: 'recent-agent', title: run.agent }, run.agent),
              h('span', { class: 'recent-status' }, badge(RUN_STATUS_LABELS[run.status], run.status === 'failed' ? 'bad' : run.status === 'running' ? 'warn' : 'muted')),
              h('span', { class: 'recent-counts' }, `${run.iterations} / ${run.tool_calls}`),
              h('time', { class: 'recent-time', datetime: run.started_at }, formatTimestamp(run.started_at)),
              h('span', { class: 'recent-action' }, '查看 →'),
            ])),
          ]),
        ]),
      ])
    }
  },
})
