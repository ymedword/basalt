// 视图共享层：装配上下文、统一的视图 props、异步加载组合式函数与状态文案映射。
// 视图只依赖这里暴露的上下文，不自行创建客户端或访问全局状态。
// Shared view layer: the console context, uniform view props, the loader composable
// and status-label maps. Views never construct their own clients.
import { h, onMounted, ref, shallowRef } from 'vue'
import type { PropType, Ref, ShallowRef, VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import type { AgentState, ApiClient, OperationStatus, RunStatus } from '../api'
import type { ConsoleAuth } from '../auth'
import { StatusBadge } from '../components/ui'
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
