// 隔离只读展示：执行档与授权档位分别呈现，不提供配置写入口。
// Read-only execution isolation, separate from authorization policy profiles.
import { h } from 'vue'
import type { VNode } from 'vue'
import type { IsolationExecutionView, IsolationView } from '../api'
import { DataTable, Notice, formatTimestamp } from '../components/ui'
import { definitions, section } from './shared'

export function isolationSummary(view: IsolationView | undefined): string {
  if (!view?.enabled) return '本机进程（无容器隔离）；run_code 未启用'
  return `shell：${view.tools.shell === 'oci' ? '加固 OCI' : '本机进程（无容器隔离）'}；run_code：${view.tools.run_code === 'gvisor' ? 'gVisor' : '未启用'}`
}

export function renderIsolation(view: IsolationView | undefined): VNode {
  return section('任务隔离', [
    h('p', isolationSummary(view)),
    h('p', { class: 'muted small' }, '此处仅展示。执行档、资源预算和镜像通过 basalt.yaml 配置，修改后需重启；授权仍由工具策略独立判定。'),
    !view?.enabled ? h(Notice, { tone: 'info', text: '任务隔离未启用；服务部署在容器中不等于任务已被隔离。' }) : [
      definitions([
        ['后端', view.backend],
        ['最大并发', String(view.max_concurrent)],
        ['实例 CPU / 内存预算', `${view.max_total_cpus} 核 / ${view.max_total_memory_mb} MiB`],
        ['临时空间 / 产物总量', `${view.max_total_temp_bytes} / ${view.max_total_artifact_bytes} 字节`],
        ['启动检查时间', formatTimestamp(view.checked_at)],
      ]),
      view.unavailable_reason ? h(Notice, { tone: 'warn', text: view.unavailable_reason }) : null,
      h(DataTable, {
        tableClass: 'isolation-table',
        headers: ['执行档', '运行时 / 可用性', '固定镜像', '资源与时限'],
        rows: view.profiles.map(profile => ({ key: profile.name, cells: [
          `${profile.name} · ${profile.bound ? '已绑定' : '未绑定'}`,
          h('span', [profile.runtime, ' · ', !profile.bound ? '未探测' : profile.available ? '可用' : '不可用', profile.reason ? `（${profile.reason}）` : '']),
          h('code', { class: 'isolation-digest' }, profile.image_digest),
          `${profile.cpus} 核 · ${profile.memory_mb} MiB · ${profile.pids} 进程 · /tmp ${profile.tmpfs_mb} MiB · ${profile.timeout_seconds} 秒`,
        ] })),
        empty: '没有配置执行档',
      }),
      h(Notice, { tone: 'warn', text: '隔离任务默认无网、无秘密注入。输出空间在本实例内共享，v1 不提供单任务输出硬配额，任务之间可能争用容量；运行时不可用时拒绝执行。' }),
    ],
  ])
}

export function renderIsolationExecutions(rows: IsolationExecutionView[] | undefined): VNode {
  return section('隔离执行', [h(DataTable, {
    tableClass: 'isolation-table',
    headers: ['工具调用', '执行档 / 运行时', '执行结果', '清理', '产物'],
    rows: (rows ?? []).map(row => ({ key: String(row.tool_invocation_id), cells: [
      h('span', [row.tool_name, ' · ', h('code', row.tool_call_id)]),
      `${row.profile} / ${row.runtime}`,
      `${row.state} · ${row.state === 'finished' ? `退出码 ${row.exit_code}` : '退出码未知'}${row.error_reason ? ` · ${row.error_reason}` : ''}`,
      row.cleanup_state === 'failed' ? h(Notice, { tone: 'warn', text: '清理失败：资源可能仍占用，需运维查证' }) : row.cleanup_state,
      `${row.artifact_count} 个 / ${row.artifact_bytes} 字节`,
    ] })),
    empty: '本 Run 没有隔离执行',
  })])
}
