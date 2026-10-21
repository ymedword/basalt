// History API 自实现路由（约束 41，UI-R2 扩展批准）：十二条路径 + 404 视图，不引入第三方路由库。
// popstate 恢复视图，未知路径渲染 404；离开守卫承载「未保存编辑离开前确认」。
// A hand-written History API router: eight fixed paths plus the 404 view, with
// popstate restoration and a leave guard for unsaved edits.

/** 管理台路径前缀；服务端对 /console/* 非静态资源一律回退 index.html。 */
export const CONSOLE_BASE = '/console'

export type RouteName =
  | 'overview'
  | 'login'
  | 'agents'
  | 'agent-detail'
  | 'runs'
  | 'run-detail'
  | 'resources'
  | 'operations'
  | 'tools'
  | 'memory'
  | 'notifications'
  | 'sandbox'
  | 'not-found'

/** `/console/resources/:kind` 的固定三值。 */
export const RESOURCE_KINDS = ['skills', 'mcp-servers', 'model-connections'] as const
export type ResourceKind = (typeof RESOURCE_KINDS)[number]

export interface RouteMatch {
  readonly name: RouteName
  /** 规范化路径（不含查询串）；not-found 时保留请求路径原样。 */
  readonly path: string
  readonly params: Readonly<Record<string, string>>
  readonly query: URLSearchParams
}

/** 离开守卫：返回 false 表示取消本次跳转（典型实现是 window.confirm）。 */
export type LeaveGuard = () => boolean

export interface NavigateOptions {
  /** true = replaceState（不新增历史项）；缺省 pushState。 */
  replace?: boolean
  /** true = 跳过离开守卫（认证失效等强制跳转）。 */
  force?: boolean
}

interface RoutePattern {
  readonly name: Exclude<RouteName, 'not-found'>
  readonly segments: readonly string[]
}

const ROUTES: readonly RoutePattern[] = [
  { name: 'overview', segments: [] },
  { name: 'login', segments: ['login'] },
  { name: 'agents', segments: ['agents'] },
  { name: 'agent-detail', segments: ['agents', ':name'] },
  { name: 'runs', segments: ['runs'] },
  { name: 'run-detail', segments: ['runs', ':id'] },
  { name: 'resources', segments: ['resources', ':kind'] },
  { name: 'operations', segments: ['operations'] },
  { name: 'tools', segments: ['library', 'tools'] },
  { name: 'memory', segments: ['library', 'memory'] },
  { name: 'notifications', segments: ['settings', 'notifications'] },
  { name: 'sandbox', segments: ['settings', 'sandbox'] },
]

function isResourceKind(value: string): value is ResourceKind {
  return (RESOURCE_KINDS as readonly string[]).includes(value)
}

/** 去掉末尾斜杠（根路径除外），让 `/console/agents/` 与 `/console/agents` 等价。 */
function trimTrailingSlash(path: string): string {
  let end = path.length
  while (end > 1 && path[end - 1] === '/') {
    end -= 1
  }
  return path.slice(0, end)
}

/**
 * 匹配一条 `path[?query]`：只认 `/console` 前缀；段数不符或 `:kind` 非固定值
 * 一律落到 404。百分号编码非法时同样按 404 处理，不抛出。
 * Matches a path against the fixed table; malformed encodings fall through to 404.
 */
export function matchRoute(url: string): RouteMatch {
  const queryIndex = url.indexOf('?')
  const rawPath = queryIndex === -1 ? url : url.slice(0, queryIndex)
  const search = queryIndex === -1 ? '' : url.slice(queryIndex)
  const path = trimTrailingSlash(rawPath)
  const notFound: RouteMatch = {
    name: 'not-found',
    path: rawPath,
    params: {},
    query: new URLSearchParams(search),
  }

  if (path !== CONSOLE_BASE && !path.startsWith(`${CONSOLE_BASE}/`)) {
    return notFound
  }
  const rest = path === CONSOLE_BASE ? '' : path.slice(CONSOLE_BASE.length + 1)
  const segments = rest === '' ? [] : rest.split('/')

  for (const route of ROUTES) {
    if (route.segments.length !== segments.length) {
      continue
    }
    const params: Record<string, string> = {}
    let matched = true
    for (let i = 0; i < route.segments.length; i += 1) {
      const pattern = route.segments[i] ?? ''
      const actual = segments[i] ?? ''
      if (pattern.startsWith(':')) {
        let decoded: string
        try {
          decoded = decodeURIComponent(actual)
        } catch {
          matched = false
          break
        }
        if (decoded === '') {
          matched = false
          break
        }
        params[pattern.slice(1)] = decoded
        continue
      }
      if (pattern !== actual) {
        matched = false
        break
      }
    }
    if (!matched) {
      continue
    }
    if (route.name === 'resources') {
      const kind = params.kind ?? ''
      if (!isResourceKind(kind)) {
        return notFound
      }
    }
    return {
      name: route.name,
      path: route.name === 'overview' ? `${CONSOLE_BASE}/` : `${CONSOLE_BASE}/${rest}`,
      params,
      query: new URLSearchParams(search),
    }
  }
  return notFound
}

/** 形如凭据的查询参数：即使出现在 next 里也不带进登录页。 */
const SENSITIVE_QUERY_KEYS = new Set([
  'token',
  'access_token',
  'authorization',
  'auth',
  'api_key',
  'apikey',
  'key',
  'secret',
  'password',
])

/**
 * 校验并清洗登录后的返回路径：只接受站内 `/console` 路径，剔除可能的凭据查询
 * 参数，丢弃哈希；其余情况返回空串（登录后回默认页）。
 * Sanitizes the `next` path: same-origin console routes only, credential-shaped
 * query keys removed.
 */
export function safeNextPath(candidate: string | null | undefined): string {
  if (typeof candidate !== 'string') {
    return ''
  }
  const trimmed = candidate.trim()
  if (trimmed === '' || trimmed.includes('\\')) {
    return ''
  }
  // 哈希片段不参与路由，也不带进登录页（浏览器不会把它发给服务端）。
  const withoutHash = trimmed.split('#')[0] ?? ''
  if (withoutHash !== CONSOLE_BASE && !withoutHash.startsWith(`${CONSOLE_BASE}/`)) {
    return ''
  }
  const match = matchRoute(withoutHash)
  if (match.name === 'login' || match.name === 'not-found') {
    return ''
  }
  const params = new URLSearchParams(match.query)
  for (const key of [...params.keys()]) {
    if (SENSITIVE_QUERY_KEYS.has(key.toLowerCase())) {
      params.delete(key)
    }
  }
  const query = params.toString()
  return query === '' ? match.path : `${match.path}?${query}`
}

/** 组装登录页路径；next 经 safeNextPath 清洗，空则不附参数。 */
export function loginPath(next?: string | null): string {
  const safe = safeNextPath(next)
  return safe === '' ? `${CONSOLE_BASE}/login` : `${CONSOLE_BASE}/login?next=${encodeURIComponent(safe)}`
}

/** 站内链接构造器：参数一律百分号编码。 */
export function agentHref(name: string): string {
  return `${CONSOLE_BASE}/agents/${encodeURIComponent(name)}`
}

export function runHref(runId: string): string {
  return `${CONSOLE_BASE}/runs/${encodeURIComponent(runId)}`
}

export function resourceHref(kind: ResourceKind): string {
  return `${CONSOLE_BASE}/resources/${kind}`
}

type RouteListener = (match: RouteMatch) => void

/**
 * ConsoleRouter：start() 后接管 popstate 与站内链接点击；navigate() 先问离开守卫。
 * 认证失效等强制跳转用 `force` 绕过守卫，避免把人困在已失效的页面上。
 * After start() the router owns popstate and same-origin console link clicks.
 */
export class ConsoleRouter {
  #current: RouteMatch | null = null
  #lastURL: string = CONSOLE_BASE
  #leaveGuard: LeaveGuard | null = null
  #started = false
  readonly #listeners = new Set<RouteListener>()
  readonly #onPopState = (): void => {
    this.#handlePopState()
  }

  readonly #onClick = (event: MouseEvent): void => {
    this.#handleClick(event)
  }

  /** 最近一次派发的路由；start() 之前为 null。 */
  get current(): RouteMatch | null {
    return this.#current
  }

  /** 订阅路由变化；返回退订函数。 */
  subscribe(listener: RouteListener): () => void {
    this.#listeners.add(listener)
    return () => {
      this.#listeners.delete(listener)
    }
  }

  /** 安装 / 卸载离开守卫（null = 无未保存编辑）。 */
  setLeaveGuard(guard: LeaveGuard | null): void {
    this.#leaveGuard = guard
  }

  /** 接管浏览器事件并按当前地址派发首个路由；重复调用无副作用。 */
  start(): void {
    if (this.#started) {
      return
    }
    this.#started = true
    window.addEventListener('popstate', this.#onPopState)
    document.addEventListener('click', this.#onClick)
    this.#dispatch(`${window.location.pathname}${window.location.search}`)
  }

  /** 释放事件监听（页面卸载或测试收尾用）。 */
  stop(): void {
    if (!this.#started) {
      return
    }
    this.#started = false
    window.removeEventListener('popstate', this.#onPopState)
    document.removeEventListener('click', this.#onClick)
  }

  /**
   * 跳转到站内路径：非 `/console` 目标直接忽略；被离开守卫拦下时不改地址。
   * 返回是否发生了跳转。
   * Navigates inside the console only; a blocking guard leaves the URL untouched.
   */
  navigate(to: string, options: NavigateOptions = {}): boolean {
    if (!to.startsWith(CONSOLE_BASE)) {
      return false
    }
    if (to === this.#lastURL && !options.replace) {
      return true
    }
    if (!options.force && this.#leaveGuard !== null && !this.#leaveGuard()) {
      return false
    }
    if (options.replace) {
      window.history.replaceState(null, '', to)
    } else {
      window.history.pushState(null, '', to)
    }
    this.#dispatch(to)
    return true
  }

  #handlePopState(): void {
    const target = `${window.location.pathname}${window.location.search}`
    if (this.#leaveGuard !== null && !this.#leaveGuard()) {
      // 后退被拒：把地址栏恢复到当前视图对应的 URL（pushState 不触发 popstate）。
      window.history.pushState(null, '', this.#lastURL)
      return
    }
    this.#dispatch(target)
  }

  #handleClick(event: MouseEvent): void {
    if (event.defaultPrevented || event.button !== 0) {
      return
    }
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return
    }
    const target = event.target
    if (!(target instanceof Element)) {
      return
    }
    const anchor = target.closest('a')
    if (!(anchor instanceof HTMLAnchorElement)) {
      return
    }
    if (anchor.target !== '' && anchor.target !== '_self') {
      return
    }
    if (anchor.hasAttribute('download')) {
      return
    }
    let url: URL
    try {
      url = new URL(anchor.href, window.location.href)
    } catch {
      return
    }
    if (url.origin !== window.location.origin) {
      return
    }
    if (url.pathname !== CONSOLE_BASE && !url.pathname.startsWith(`${CONSOLE_BASE}/`)) {
      return
    }
    event.preventDefault()
    this.navigate(`${url.pathname}${url.search}`)
  }

  #dispatch(url: string): void {
    const match = matchRoute(url)
    this.#current = match
    this.#lastURL = `${window.location.pathname}${window.location.search}`
    for (const listener of this.#listeners) {
      listener(match)
    }
  }
}
