// 系统总览：按服务状态、待处理问题、运行读数组织已有 API 的真实数据。
// System overview prioritizes readiness, actionable issues and measured process data.
import { computed, defineComponent, h } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type { SystemWorkerView } from '../api'
import { Notice, formatBytes, formatTimestamp, formatUptime } from '../components/ui'
import { brandMark } from '../components/brand'
import {
  WORKER_ACTIVE_NOTE,
  WORKER_STALE_HINT,
  WORKER_STALE_NOTE,
  WORKER_STATUS_LABELS,
  WORKER_STOPPED_NOTE,
  formatHeartbeatAge,
  groupWorkers,
  normalizeWorkerStatus,
  truncationText,
  workerAsOfText,
  workerCountText,
  workerDetailPairs,
  workerGroupCountText,
  workerStatusLabel,
  workerStatusTone,
  workerTotalLabel,
} from '../components/overview-worker-labels'
import {
  CREDENTIAL_SUMMARY_TITLE,
  INSTANCE_SECRETS_TITLE,
  credentialSummaryText,
  credentialSummaryTone,
  instanceSecretsDetail,
  instanceSecretsLabel,
  instanceSecretsTone,
  isInstanceSecretsView,
  recoveryReasonText,
} from '../instance-secrets'
import { agentHref, resourceHref, runHref } from '../router'
import { badge, definitions, RUN_STATUS_LABELS, useLoader, viewProps } from './shared'

/** shortInstance 把 16 进制实例标识压成可读短码；完整值留在 title 里。 */
function shortInstance(instance: string): string {
  return instance.length <= 18 ? instance : `${instance.slice(0, 8)}…${instance.slice(-6)}`
}

/** 心跳年龄排序键：只使用服务端字段，缺失值排到最后；不读浏览器时钟。 */
function heartbeatSortKey(node: SystemWorkerView): number {
  return Number.isFinite(node.heartbeat_age_seconds) ? node.heartbeat_age_seconds : Number.POSITIVE_INFINITY
}

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
    // 实例加密状态与凭据汇总：只读投影，失败时卡片显示「状态待确认」，
    // 不把读不到说成「未初始化」（两者对用户的含义完全不同）。
    // The instance-secrets projection; a read failure is "unknown", never "uninitialized".
    const secrets = useLoader(() => api.getInstanceSecrets())
    const loaders = [info, overview, sample, agents, mcps, recent, secrets]
    const busy = computed(() => loaders.some(loader => loader.loading.value))
    const errors = computed(() => loaders.map(loader => loader.error.value).filter(Boolean))
    function refresh(): void { void Promise.all(loaders.map(loader => loader.reload())) }
    /**
     * 实例加密与连接凭据两行（设计 §2.6.5）：状态徽标在右，技术明细默认折叠。
     * 读失败时两行都显示「待确认」——不把「读不到」写成「未初始化」。
     * Two rows for the instance key and its credential summary; a failed read is
     * reported as unknown, never as uninitialized.
     */
    function renderSecretsPanel(): VNode {
      const view =
        secrets.data.value !== null && isInstanceSecretsView(secrets.data.value)
          ? secrets.data.value
          : null
      const failed = secrets.error.value !== ''
      const stateBadge = failed
        ? badge('状态待确认', 'muted')
        : view === null
          ? badge('读取中', 'muted')
          : badge(instanceSecretsLabel(view), instanceSecretsTone(view))
      // 说明句与模型连接表单共用（instanceSecretsDetail），两处口径不会漂移。
      const stateDetail = failed
        ? `读取失败：${secrets.error.value}`
        : view === null
          ? '正在读取实例状态…'
          : instanceSecretsDetail(view)
      const summaryBadge =
        failed || view === null
          ? badge('待确认', 'muted')
          : badge(
              credentialSummaryText(view.credential_summary),
              credentialSummaryTone(view.credential_summary),
            )
      const rows: VNodeChild[] = [
        h('div', { class: 'dependency-row' }, [
          h('div', [h('strong', INSTANCE_SECRETS_TITLE), h('p', stateDetail)]),
          stateBadge,
        ]),
        h('div', { class: 'dependency-row' }, [
          h('div', [
            h('strong', CREDENTIAL_SUMMARY_TITLE),
            h('p', '托管与环境变量来源的凭据槽位；不包含调用方密钥。'),
          ]),
          summaryBadge,
        ]),
      ]
      if (view !== null) {
        rows.push(
          h('details', { class: 'technical-details' }, [
            h('summary', '查看详情'),
            definitions([
              ['状态', instanceSecretsLabel(view)],
              ['key_id', view.key_id === '' ? '—' : h('code', {}, view.key_id)],
              ['初始化时间', formatTimestamp(view.initialized_at)],
              ['密钥文件', view.key_file === '' ? '—' : h('code', {}, view.key_file)],
              [
                '恢复原因',
                view.recovery_reason === ''
                  ? '—'
                  : `${recoveryReasonText(view.recovery_reason)}（${view.recovery_reason}）`,
              ],
              [
                '凭据槽位',
                `托管 ${view.credential_summary.managed} / 已撤销 ${view.credential_summary.managed_revoked} / 环境变量 ${view.credential_summary.env} / 缺值 ${view.credential_summary.missing}`,
              ],
            ]),
          ]),
        )
      }
      return h('section', { class: 'overview-panel secrets-panel' }, [
        h('div', { class: 'panel-heading' }, [
          h('h2', '实例与凭据'),
          h('span', { class: 'eyebrow' }, 'INSTANCE'),
        ]),
        ...rows,
      ])
    }
    /**
     * 单个 Worker 实例行（US-25C §2.3.9）：默认只显示名称、状态与距上次心跳，
     * 宿主 / 版本 / 租约进「详情」；状态只读服务端字段，同名的不同实例各占一行。
     * One process instance per row: compact by default, host/version/leases in details.
     */
    function renderWorkerRow(node: SystemWorkerView): VNode {
      const label = workerStatusLabel(node.status)
      const title = node.name === '' ? shortInstance(node.instance) : node.name
      return h('div', { key: node.instance, class: ['worker-row', `worker-row-${normalizeWorkerStatus(node.status)}`] }, [
        h('div', { class: 'worker-row-main' }, [
          h('div', { class: 'worker-row-identity' }, [
            h('strong', { title: node.instance }, title),
            h('p', { class: 'worker-row-reading' }, `${label} · 距上次心跳 ${formatHeartbeatAge(node.heartbeat_age_seconds)}`),
          ]),
          badge(label, workerStatusTone(node.status)),
        ]),
        h('details', { class: 'technical-details worker-details' }, [
          h('summary', {}, '详情'),
          definitions(workerDetailPairs(node)),
        ]),
      ])
    }
    /** 分组标题：组计数取自服务端聚合，始终可见。 Group heading with the aggregate count. */
    function renderWorkerGroupHeading(title: string, count: number | undefined): VNode {
      return h('div', { class: 'worker-group-heading' }, [
        h('h3', {}, title),
        h('span', { class: 'dependency-count' }, workerGroupCountText(count)),
      ])
    }
    /**
     * 组内没有明细行时的说明：聚合计数仍大于 0 说明是明细截断，
     * **不得**写成「当前没有」，否则与组计数自相矛盾。
     * An absent page never claims the group is empty when its aggregate count is nonzero.
     */
    function emptyWorkerGroupNote(label: string, count: number | undefined, rows: number): VNode | null {
      if (rows > 0) {
        return null
      }
      const bounded = typeof count === 'number' && count > 0
      return h('p', { class: 'muted small' }, bounded
        ? `本页未包含${label}的登记行（明细有界）。`
        : `当前没有${label}的实例。`)
    }
    /**
     * 分布式执行面板（US-24 §3.1.13 / US-25C §2.3.9）：Worker 列表与全库活跃执行权都是
     * 只读诊断。数据库不可读时整组省略——这里显示「状态不可用」，**不得**显示成 0 个实例
     * 或全部已停止；反过来，能读到的 0 是真实读数（单 Worker 空闲）。
     * Refreshing after a successful read keeps the last reading but marks it expired.
     */
    function renderWorkersPanel(): VNode {
      const summary = overview.data.value
      const workers = summary?.workers ?? null
      const readFailed = overview.error.value !== ''
      const expired = readFailed && workers !== null
      const headingBadge =
        workers === null
          ? badge('待确认', 'muted')
          : expired
            ? badge('数据已过期', 'warn')
            : badge(workerTotalLabel(workers.total), (workers.counts?.stale ?? 0) > 0 ? 'warn' : 'ok')
      const body: VNodeChild[] = []
      if (workers === null) {
        body.push(
          h('p', { class: 'muted' }, readFailed || summary !== null
            ? '状态不可用：数据库不可读，Worker 状态待确认。'
            : '正在读取 Worker 状态…'),
          h('p', { class: 'muted small' }, '活跃租约数读不到时不显示为 0。'),
        )
      } else {
        body.push(
          h('p', { class: expired ? 'notice notice-warn' : 'muted small worker-asof' }, expired
            ? `状态不可用：数据已过期，以下为${workerAsOfText(workers.as_of)}的上次读数。`
            : `${workerAsOfText(workers.as_of)}（数据库时间）`),
          h('p', { class: 'muted small' }, `全库活跃执行权 ${workers.active_leases} 个；心跳只作诊断，执行权归属以租约到期为准。`),
          h('div', { class: 'overview-metrics worker-metrics' }, [
            metric('心跳正常', workerCountText(workers.counts?.active), WORKER_ACTIVE_NOTE),
            metric('心跳超时待确认', workerCountText(workers.counts?.stale), WORKER_STALE_NOTE),
            metric('已停止', workerCountText(workers.counts?.stopped), WORKER_STOPPED_NOTE),
          ]),
        )
        const groups = groupWorkers(workers.nodes)
        // 心跳超时组最近的在前：按服务端给出的心跳年龄升序，默认只展开 5 条。
        // Oldest heartbeat first would hide fresh stale rows; sort by server-provided age.
        const stale = [...groups.stale].sort(
          (left, right) => heartbeatSortKey(left) - heartbeatSortKey(right),
        )
        const staleVisible = stale.slice(0, 5)
        const staleRest = stale.slice(5)
        body.push(
          h('section', { class: 'workers-group workers-active' }, [
            renderWorkerGroupHeading(WORKER_STATUS_LABELS.active, workers.counts?.active),
            ...groups.active.map(renderWorkerRow),
            emptyWorkerGroupNote('心跳正常', workers.counts?.active, groups.active.length),
          ]),
          h('section', { class: 'workers-group workers-stale' }, [
            renderWorkerGroupHeading(WORKER_STATUS_LABELS.stale, workers.counts?.stale),
            h('p', { class: 'worker-group-hint' }, WORKER_STALE_HINT),
            ...staleVisible.map(renderWorkerRow),
            staleRest.length > 0
              ? h('details', { class: 'technical-details workers-overflow' }, [
                  h('summary', {}, `展开其余 ${staleRest.length} 条`),
                  ...staleRest.map(renderWorkerRow),
                ])
              : null,
            emptyWorkerGroupNote('心跳超时', workers.counts?.stale, stale.length),
          ]),
          h('details', { class: 'workers-group workers-stopped' }, [
            h('summary', {}, `已停止 · ${workerGroupCountText(workers.counts?.stopped)}`),
            ...groups.stopped.map(renderWorkerRow),
            emptyWorkerGroupNote('已停止', workers.counts?.stopped, groups.stopped.length),
          ]),
        )
        const shown = workers.nodes.length
        const total = Math.max(workers.total, shown)
        const truncation = truncationText(shown, total)
        if (truncation !== '') {
          body.push(h('p', { class: 'muted small worker-truncation' }, `${truncation}；完整登记可用本地命令 basalt workers list 查看。`))
        } else if (shown === 0) {
          body.push(h('p', { class: 'muted small' }, '还没有 Worker 登记：启动一个 serve 进程后会出现在这里。'))
        }
      }
      return h('section', { class: 'overview-panel workers-panel' }, [
        h('div', { class: 'panel-heading' }, [h('h2', '分布式执行'), headingBadge]),
        ...body,
      ])
    }
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
            h('div', { class: 'dependency-row' }, [h('div', [h('strong', '分布式执行'), h('p', '登记的 Worker 实例与全库活跃执行权')]), h('span', { class: 'dependency-count' }, summary?.workers ? `${summary.workers.total} 个实例 · ${summary.workers.active_leases} 个活跃执行权` : '待确认')]),
          ]),
        ]),
        renderSecretsPanel(),
        renderWorkersPanel(),
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
