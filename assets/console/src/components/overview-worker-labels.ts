// 概览「分布式执行」的措辞集中处（US-25C §2.3.9）：状态标签、分组、心跳间隔的
// 人读格式、截断文案与详情字段。这里只有纯函数，不读浏览器时钟——状态一律来自
// 服务端字段（active / stale / stopped），页面不得自行推算。
// Wording for the distributed-execution overview panel. Pure functions only; status
// always comes from the server field, never from the browser clock.
import { formatTimestamp } from './ui'
import type { SystemWorkerView, WorkerStatus } from '../api'

/** 三种状态的服务端标签；「心跳正常」只说明近期有心跳。 */
export const WORKER_STATUS_LABELS: Record<WorkerStatus, string> = {
  active: '心跳正常',
  stale: '心跳超时待确认',
  stopped: '已停止',
}

/** 三种状态对应的徽标色调。 */
export const WORKER_STATUS_TONES: Record<WorkerStatus, 'ok' | 'warn' | 'muted'> = {
  active: 'ok',
  stale: 'warn',
  stopped: 'muted',
}

/** 心跳正常的边界说明（N4）：不得写成「任务运行正常」或「健康」。 */
export const WORKER_ACTIVE_NOTE = '仅表示近期有心跳；不等于任务运行正常。'

/** 心跳超时的边界说明（N7）：只提出待确认，不宣称实例已经不在。 */
export const WORKER_STALE_NOTE = '心跳超时可能只是阻塞或断网；是否存活需进一步确认。'

/** 心跳超时分组的醒目提示（同样避免「已停止 / 进程已死亡」类断言）。 */
export const WORKER_STALE_HINT =
  '心跳超时只表示近期没有收到心跳，可能只是阻塞或断网；是否存活需进一步确认，请结合宿主与网络排查。'

/** 已停止的边界说明：只有 stopped_at 非空才属于这一类。 */
export const WORKER_STOPPED_NOTE = '已走过停机流程的进程实例。'

/** 旧登记行缺心跳间隔时的详情说明（阈值按部署默认计算）。 */
export const WORKER_LEGACY_THRESHOLD_NOTE = '阈值取部署默认'

/** 卡片顶部的实例计数：统计对象是进程实例，不是服务器。 */
export function workerTotalLabel(total: number): string {
  return Number.isFinite(total) ? `${total} 个 Worker 实例` : 'Worker 实例数待确认'
}

/**
 * 服务端状态归一：只认三个枚举值；缺失或未知值按「待确认」（stale 组）处理，
 * **不得**回落到 active，也不得按浏览器时间推断。
 * Unknown statuses fall back to the cautious "pending confirmation" bucket.
 */
export function normalizeWorkerStatus(status: unknown): WorkerStatus {
  if (status === 'active' || status === 'stopped') {
    return status
  }
  return 'stale'
}

/** 状态标签；未知状态显示「待确认」，不冒充正常或已停止。 */
export function workerStatusLabel(status: unknown): string {
  if (status === 'active') return WORKER_STATUS_LABELS.active
  if (status === 'stale') return WORKER_STATUS_LABELS.stale
  if (status === 'stopped') return WORKER_STATUS_LABELS.stopped
  return '待确认'
}

/** 状态徽标色调；未知状态用中性色。 */
export function workerStatusTone(status: unknown): 'ok' | 'warn' | 'muted' {
  if (status === 'active') return WORKER_STATUS_TONES.active
  if (status === 'stale') return WORKER_STATUS_TONES.stale
  return 'muted'
}

/** 分组结果；组内保持服务端返回顺序（组计数另取服务端聚合，不由明细推算）。 */
export interface WorkerGroups {
  readonly active: readonly SystemWorkerView[]
  readonly stale: readonly SystemWorkerView[]
  readonly stopped: readonly SystemWorkerView[]
}

/** 按服务端状态分组；同一名称的多个实例各自成行，不合并。 */
export function groupWorkers(nodes: readonly SystemWorkerView[]): WorkerGroups {
  const groups: Record<WorkerStatus, SystemWorkerView[]> = {
    active: [],
    stale: [],
    stopped: [],
  }
  for (const node of nodes) {
    groups[normalizeWorkerStatus(node.status)].push(node)
  }
  return groups
}

/**
 * 距上次心跳的人读格式：只使用服务端给出的 heartbeat_age_seconds，
 * 不读当前时间。null / 非有限值显示「未知」。
 * Humanizes the server-provided heartbeat age without consulting the clock.
 */
export function formatHeartbeatAge(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) {
    return '未知'
  }
  const whole = Math.max(0, Math.floor(seconds))
  if (whole < 5) {
    return '刚刚'
  }
  if (whole < 60) {
    return `${whole} 秒前`
  }
  const minutes = Math.floor(whole / 60)
  if (minutes < 60) {
    return `${minutes} 分钟前`
  }
  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return `${hours} 小时前`
  }
  return `${Math.floor(hours / 24)} 天前`
}

/** 截断文案：「当前展示 X 条，共 Y 条」；未截断返回空串。 */
export function truncationText(shown: number, total: number): string {
  return total > shown ? `当前展示 ${shown} 条，共 ${total} 条` : ''
}

/** 计数文案：无法取得聚合计数时显示「待确认」，**不得**补 0。 */
export function workerCountText(count: number | undefined): string {
  return typeof count === 'number' && Number.isFinite(count) ? String(count) : '待确认'
}

/** 分组标题右侧的实例计数。 */
export function workerGroupCountText(count: number | undefined): string {
  return typeof count === 'number' && Number.isFinite(count) ? `${count} 个实例` : '计数待确认'
}

/** 数据时间文案：数据库时间，不换算成浏览器当前时间。 */
export function workerAsOfText(asOf: string | undefined): string {
  return `最近更新于 ${formatTimestamp(asOf)}`
}

/**
 * 单行详情字段：默认折叠的宿主、版本、时间与租约信息。
 * 旧行缺少 heartbeat_interval_seconds 时明确标注「阈值取部署默认」（N9）。
 */
export function workerDetailPairs(node: SystemWorkerView): readonly (readonly [string, string])[] {
  const interval = node.heartbeat_interval_seconds
  const pairs: (readonly [string, string])[] = [
    ['实例', node.instance],
    ['Worker 名称', node.name === '' ? '—' : node.name],
    ['宿主', node.hostname === '' ? '—' : node.hostname],
    ['构建版本', node.build_version === '' ? '—' : node.build_version],
    ['结构版本', Number.isFinite(node.schema_version) ? String(node.schema_version) : '—'],
    ['启动时间', formatTimestamp(node.started_at)],
    ['上次心跳', formatTimestamp(node.heartbeat_at)],
    [
      '心跳间隔',
      interval === undefined || interval <= 0
        ? `未登记，${WORKER_LEGACY_THRESHOLD_NOTE}（${node.stale_after_seconds} 秒）`
        : `${interval} 秒`,
    ],
    ['诊断阈值', `${node.stale_after_seconds} 秒（超过即待确认）`],
    ['活跃执行权', `${node.active_leases} 个活跃租约`],
  ]
  if (node.stopped_at !== undefined && node.stopped_at !== '') {
    pairs.push(['停止时间', formatTimestamp(node.stopped_at)])
  }
  return pairs
}
