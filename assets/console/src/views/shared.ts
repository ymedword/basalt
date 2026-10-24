// 视图共享层：装配上下文、统一的视图 props、异步加载组合式函数与状态文案映射。
// 视图只依赖这里暴露的上下文，不自行创建客户端或访问全局状态。
// Shared view layer: the console context, uniform view props, the loader composable
// and status-label maps. Views never construct their own clients.
import { h, onMounted, onUnmounted, ref, shallowRef, watch } from 'vue'
import type { PropType, Ref, ShallowRef, VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import type { AgentState, ApiClient, OperationStatus, RunStatus, RunView } from '../api'
import type { ConsoleAuth } from '../auth'
import { StatusBadge, formatTimestamp } from '../components/ui'
import type { Tone } from '../components/ui'
import type { ConsoleRouter } from '../router'

/** 管理台装配上下文：由 app.ts 创建并显式传给每个视图（不写全局单例）。 */
// The console wiring context created once in app.ts and passed explicitly.
export interface ConsoleContext {
  readonly api: ApiClient
  readonly auth: ConsoleAuth
  readonly router: ConsoleRouter
}

/** 所有视图共用的 props：上下文 + 路由参数 + 查询串。 */
// Uniform props shared by every view: context, route params and query string.
export const viewProps = {
  ctx: { type: Object as PropType<ConsoleContext>, required: true },
  params: {
    type: Object as PropType<Readonly<Record<string, string>>>,
    default: () => ({}),
  },
  query: { type: Object as PropType<URLSearchParams>, default: () => new URLSearchParams() },
} as const

/** 异步加载状态：数据、错误文案、加载中标记与重载入口。 */
// Async loading state with single-flight accounting: stale responses are dropped.
export interface LoaderState<T> {
  readonly data: ShallowRef<T | null>
  readonly error: Ref<string>
  readonly loading: Ref<boolean>
  reload(): Promise<void>
}

/**
 * useLoader：订阅一个异步数据源。同一时刻只认最后一次请求的结果，
 * 旧请求晚到也不会覆盖新数据。immediate=false 时由调用方显式 reload()。
 */
export function useLoader<T>(load: () => Promise<T>, immediate = true): LoaderState<T> {
  const data = shallowRef<T | null>(null)
  const error = ref('')
  const loading = ref(false)
  let generation = 0

  async function reload(): Promise<void> {
    const ticket = ++generation
    loading.value = true
    error.value = ''
    try {
      const value = await load()
      if (ticket === generation) {
        data.value = value
      }
    } catch (caught) {
      if (ticket === generation) {
        error.value = errorMessage(caught)
      }
    } finally {
      if (ticket === generation) {
        loading.value = false
      }
    }
  }

  if (immediate) {
    onMounted(() => {
      void reload()
    })
  }

  return { data, error, loading, reload }
}

/** 轮询基线（20 秒）、退避上限（5 分钟）与可见性 / 聚焦去重窗口。 */
// Polling base, backoff cap and the visibility/focus dedupe window.
export const LIVE_REFRESH_BASE_MS = 20_000
export const LIVE_REFRESH_MAX_MS = 300_000
const LIVE_REFRESH_DEDUPE_MS = 3_000

/** useLiveLoader 选项：轮询间隔与该数据所在页签是否活跃。 */
export interface LiveLoaderOptions {
  /** 轮询间隔；省略取 20 秒。 */
  intervalMs?: number
  /** 页签活跃标记：false 时停止轮询（页面卸载与登出由卸载清理覆盖）。 */
  active?: Ref<boolean>
}

/** 带可见性轮询的加载状态：在 LoaderState 之上增加最近成功刷新时间与立即刷新。 */
export interface LiveLoaderState<T> extends LoaderState<T> {
  /** 最近一次成功刷新的本地时间；null = 本次进入后尚未成功刷新。 */
  readonly lastRefreshedAt: Ref<Date | null>
  /** 立即刷新（与轮询共用单飞闸：已有请求在途时跳过）。 */
  refreshNow(): Promise<void>
  /**
   * 页内写后的重新读取：同样受单飞闸约束；若轮询已在途，等它结束后补一次，
   * 保证读到的是写后状态而不是写前响应。
   * Write-triggered reloads share the single-flight gate and queue one trailing read.
   */
  reload(): Promise<void>
}

/**
 * useLiveLoader：在 useLoader 之上加"页签可见时定时刷新"（R8 裁决 3）。
 *
 * 约束：① 同一时刻只跑一个请求（过期响应由 useLoader 的代际号丢弃，数据与滚动
 * 位置保持不动）；② 页面隐藏、切走页签、卸载时停止定时器；③ 失败按 2 倍退避到
 * 5 分钟，成功后回到基线；④ visibilitychange / focus 触发的刷新在 3 秒内去重；
 * ⑤ 只重读 GET 列表——**绝不**重发写请求或运行请求。
 * Polls only while the tab is visible; failures back off; writes are never retried.
 */
export function useLiveLoader<T>(
  load: () => Promise<T>,
  options: LiveLoaderOptions = {},
): LiveLoaderState<T> {
  // 首次读取也走单飞刷新：失败计入退避，只有成功才写「最近刷新」。
  // The initial read shares the single-flight path so the stamp stays truthful.
  const state = useLoader(load, false)
  const lastRefreshedAt = ref<Date | null>(null)
  const interval = options.intervalMs ?? LIVE_REFRESH_BASE_MS
  const active = options.active ?? ref(true)
  let timer: ReturnType<typeof setTimeout> | null = null
  let delay = interval
  let inFlight = false
  let pendingWriteRefresh = false
  let lastAttemptAt = 0
  let stopped = false

  function clearTimer(): void {
    if (timer !== null) {
      clearTimeout(timer)
      timer = null
    }
  }

  function mayRefresh(): boolean {
    return !stopped && active.value && document.visibilityState !== 'hidden'
  }

  function schedule(): void {
    clearTimer()
    if (!mayRefresh() || interval <= 0) {
      return
    }
    timer = setTimeout(() => {
      timer = null
      void tick()
    }, delay)
  }

  /** 单飞刷新；返回 null = 本轮因在途请求跳过（不改变退避档位）。 */
  async function run(): Promise<boolean | null> {
    if (inFlight) {
      return null
    }
    inFlight = true
    lastAttemptAt = Date.now()
    try {
      await state.reload()
      const ok = state.error.value === ''
      if (ok) {
        lastRefreshedAt.value = new Date()
        delay = interval
      }
      return ok
    } finally {
      inFlight = false
      // 页内写后刷新若撞上在途轮询，等它结束再补一次（仍是同一时刻一个请求）。
      if (pendingWriteRefresh) {
        pendingWriteRefresh = false
        if (mayRefresh()) {
          const ok = await run()
          if (ok === false) {
            delay = Math.min(delay * 2, LIVE_REFRESH_MAX_MS)
          }
        }
      }
    }
  }

  async function tick(): Promise<void> {
    if (!mayRefresh()) {
      return
    }
    if ((await run()) === false) {
      delay = Math.min(delay * 2, LIVE_REFRESH_MAX_MS)
    }
    schedule()
  }

  async function refreshNow(): Promise<void> {
    if (!mayRefresh()) {
      return
    }
    if ((await run()) === false) {
      delay = Math.min(delay * 2, LIVE_REFRESH_MAX_MS)
    }
    schedule()
  }

  /**
   * 页内写后的重新读取（T215 / T216）：不再直接暴露 useLoader 的裸 reload，
   * 否则会和轮询并发打同一端点。在途时排队一次尾随刷新，成功后时间戳照常前进。
   * Write-triggered reads never overlap a poll; one trailing read is queued instead.
   */
  async function reloadAfterWrite(): Promise<void> {
    if (!mayRefresh()) {
      return
    }
    if (inFlight) {
      pendingWriteRefresh = true
      return
    }
    if ((await run()) === false) {
      delay = Math.min(delay * 2, LIVE_REFRESH_MAX_MS)
    }
  }

  /** 可见性 / 聚焦触发的刷新：3 秒内只认一次，避免两个事件叠成两次请求。 */
  function refreshFromEvent(): void {
    if (Date.now() - lastAttemptAt < LIVE_REFRESH_DEDUPE_MS) {
      return
    }
    void refreshNow()
  }

  function onVisibilityChange(): void {
    if (document.visibilityState === 'hidden') {
      clearTimer()
      return
    }
    refreshFromEvent()
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibilityChange)
    window.addEventListener('focus', refreshFromEvent)
    void refreshNow()
  })
  onUnmounted(() => {
    stopped = true
    clearTimer()
    document.removeEventListener('visibilitychange', onVisibilityChange)
    window.removeEventListener('focus', refreshFromEvent)
  })
  // 进入 / 切回页签时立即重读一次，而不是等下一个轮询周期（裁决 3）。
  // Entering the tab re-reads at once instead of waiting for the next tick.
  watch(active, (value) => {
    if (value) {
      void refreshNow()
    } else {
      clearTimer()
    }
  })

  // 显式覆盖 reload：页内操作与轮询共用同一道单飞闸（R8 裁决 3）。
  return { ...state, reload: reloadAfterWrite, lastRefreshedAt, refreshNow }
}

/** 刷新时间戳的页面文案；尚未成功刷新时明确说"尚未刷新"，不伪造时间。 */
export function refreshStamp(at: Date | null): string {
  return at === null ? '尚未刷新' : `最近刷新 ${formatTimestamp(at.toISOString())}`
}

/** Notice 的三档色调；与状态徽标的 Tone 是两张表，不混用。 */
// Notice tones are a separate three-value scale from the badge Tone.
export type NoticeTone = 'info' | 'warn' | 'error'

/** 资源本地配置就绪三值；与实测结果（last_test）分开呈现，绝不互相冒充。 */
// Local readiness; never shown as if it were a live test result.
export type Availability = 'ready' | 'unconfigured' | 'unavailable'

export const AVAILABILITY_LABELS: Record<Availability, string> = {
  ready: '配置就绪',
  unconfigured: '未配置',
  unavailable: '不可用',
}

export function availabilityTone(availability: Availability): Tone {
  switch (availability) {
    case 'ready':
      return 'ok'
    case 'unconfigured':
      return 'muted'
    default:
      return 'bad'
  }
}

/**
 * 同步 Run 的结果文案：limit_reached 是 200 的正常结果，不是服务故障。
 * 会话发送与「立即运行一轮」共用同一口径，避免两处文案漂移。
 * A synchronous run's outcome; limit_reached is a normal 200 result.
 */
export function sendResultNotice(run: RunView): { tone: NoticeTone; text: string } {
  switch (run.status) {
    case 'completed':
      return {
        tone: 'info',
        text: `运行完成（${run.iterations} 轮，${run.tool_calls} 次工具调用）。`,
      }
    case 'limit_reached':
      return {
        tone: 'warn',
        text: `达到迭代上限（${run.iterations} 轮），运行以 limit_reached 结束；可查看运行详情与部分结果。`,
      }
    case 'failed':
      return {
        tone: 'error',
        text: `运行失败：${run.reason === '' ? '服务端未给出原因，请查看运行详情。' : run.reason}`,
      }
    case 'cancelled':
      return { tone: 'warn', text: '运行已取消。' }
    default:
      return { tone: 'info', text: `运行状态：${run.status}` }
  }
}

/** Agent 生命周期状态的中文标签。 */
export const AGENT_STATE_LABELS: Record<AgentState, string> = {
  active: '启用',
  disabled: '停用',
  archived: '归档',
}

export const RUN_STATUS_LABELS: Record<RunStatus, string> = {
  running: '运行中',
  completed: '已完成',
  limit_reached: '达到迭代上限',
  cancelled: '已取消',
  failed: '失败',
}

export const OPERATION_STATUS_LABELS: Record<OperationStatus, string> = {
  pending: '进行中',
  success: '成功',
  failed: '失败',
  unknown: '结果未知',
}

/** 生命周期徽标色调：启用绿、停用黄、归档中灰、无效红。 */
export function lifecycleTone(state: AgentState | null): Tone {
  switch (state) {
    case 'active':
      return 'ok'
    case 'disabled':
      return 'warn'
    case 'archived':
      return 'muted'
    default:
      return 'bad'
  }
}

export function runStatusTone(status: RunStatus): Tone {
  switch (status) {
    case 'running':
      return 'warn'
    case 'completed':
      return 'ok'
    case 'cancelled':
      return 'muted'
    default:
      return 'bad'
  }
}

export function operationStatusTone(status: OperationStatus): Tone {
  switch (status) {
    case 'success':
      return 'ok'
    case 'pending':
      return 'warn'
    case 'unknown':
      return 'muted'
    default:
      return 'bad'
  }
}

/** 徽标快捷构造。 */
export function badge(label: string, tone: Tone): VNode {
  return h(StatusBadge, { label, tone })
}

/** 统一页眉：眉题、标题与说明；动作由页面显式传入。 Shared page heading. */
export function pageHeading(eyebrow: string, title: string, description: string, actions?: VNodeChild): VNode {
  return h('header', { class: 'page-heading' }, [
    h('div', [h('p', { class: 'eyebrow' }, eyebrow), h('h1', title), h('p', { class: 'page-description' }, description)]),
    actions ?? null,
  ])
}

/** 长标识单行展示，完整值仍可复制并通过原生提示查看。 Full ID stays in the text node. */
export function identifier(value: string, href?: string): VNode {
  return h(href ? 'a' : 'code', { class: 'cell-id', title: value, ...(href ? { href } : {}) }, value)
}

/** 工具目录与 MCP 定义共用一个导航，不复制资源目录。 One canonical capability entry. */
export function capabilityTabs(active: 'tools' | 'mcp'): VNode {
  return h('nav', { class: 'tabs capability-tabs', 'aria-label': '工具与 MCP' }, [
    h('a', { class: ['tab', active === 'tools' ? 'active' : ''], href: '/console/library/tools', 'aria-current': active === 'tools' ? 'page' : undefined }, '可用工具'),
    h('a', { class: ['tab', active === 'mcp' ? 'active' : ''], href: '/console/resources/mcp-servers', 'aria-current': active === 'mcp' ? 'page' : undefined }, 'MCP 服务'),
  ])
}

/** 带标题的区块（卡片）。 */
export function section(title: string, children: readonly VNodeChild[]): VNode {
  return h('section', { class: 'card' }, [h('h2', title), ...children])
}

/** 定义列表：成对的术语 / 值。 */
export function definitions(pairs: readonly (readonly [string, VNodeChild])[]): VNode {
  return h(
    'dl',
    { class: 'definitions' },
    pairs.flatMap(([term, value]) => [h('dt', term), h('dd', [value])]),
  )
}
