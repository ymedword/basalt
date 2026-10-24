// 路由单元用例：US-11 的十二条固定路径 + US-12 的五条新路径 + R7 会话深链、
// 静态段先于动态段、未知路径 404、next 清洗与 History API 导航（约束 41 /
// 卡片 §3.1.10；配置页 `agents/:name/config` 与深链
// `agents/:name/sessions/:id` 分属两次受控新增）。
// Router unit tests: twelve US-11 paths, five US-12 paths and the R7 session deep
// link, static-before-dynamic matching, unknown-path 404, `next` sanitizing.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  CONSOLE_BASE,
  ConsoleRouter,
  RESOURCE_KINDS,
  agentConfigHref,
  agentHref,
  loginPath,
  matchRoute,
  resourceHref,
  runHref,
  safeNextPath,
  sessionHref,
} from '../src/router'
import type { RouteMatch } from '../src/router'

function resetLocation(path: string): void {
  window.history.replaceState(null, '', path)
}

describe('matchRoute', () => {
  it('matches the eighteen approved console paths', () => {
    // 十二条 US-11 固定路径 + 五条 US-12 新路径 + R7 会话深链逐条命中。
    const cases: Array<[string, string]> = [
      ['/console/', 'overview'],
      ['/console/login', 'login'],
      ['/console/agents', 'agents'],
      ['/console/agents/new', 'agents-new'],
      ['/console/agents/reporter', 'agent-detail'],
      ['/console/agents/reporter/config', 'agent-config'],
      ['/console/agents/reporter/sessions/s-1', 'agent-session'],
      ['/console/runs', 'runs'],
      ['/console/runs/run-1', 'run-detail'],
      ['/console/resources/skills', 'resources'],
      ['/console/resources/skills/skill-1', 'resource-detail'],
      ['/console/archived', 'archived'],
      ['/console/workspace', 'workspace'],
      ['/console/operations', 'operations'],
      ['/console/library/tools', 'tools'],
      ['/console/library/memory', 'memory'],
      ['/console/settings/notifications', 'notifications'],
      ['/console/settings/sandbox', 'sandbox'],
    ]
    for (const [path, name] of cases) {
      expect(matchRoute(path).name, path).toBe(name)
    }
  })

  it('matches agents/new before the agent-detail dynamic segment', () => {
    // 静态段先于动态段：`/console/agents/new` 不得被当成名为 "new" 的 Agent 页面。
    const create = matchRoute('/console/agents/new')
    expect(create.name).toBe('agents-new')
    expect(create.params).toEqual({})
    expect(matchRoute('/console/agents/new/extra').name).toBe('not-found')
  })

  it('matches the config page without shadowing the agent detail route', () => {
    // 配置页三段、详情页两段：段数不同互不抢占；名字仍按段解码。
    const config = matchRoute('/console/agents/a%2Fb/config')
    expect(config.name).toBe('agent-config')
    expect(config.params.name).toBe('a/b')
    expect(matchRoute('/console/agents/a%2Fb').name).toBe('agent-detail')
    // 尾段不是 config 时仍按 404：不为配置页开前缀匹配。
    expect(matchRoute('/console/agents/reporter/config/extra').name).toBe('not-found')
  })

  it('matches the session deep link without shadowing agent detail or config', () => {
    // 深链四段：与详情页（两段）、配置页（三段）互不抢占；两个参数都按段解码。
    const session = matchRoute('/console/agents/a%2Fb/sessions/s%2F1')
    expect(session.name).toBe('agent-session')
    expect(session.params.name).toBe('a/b')
    expect(session.params.id).toBe('s/1')
    expect(matchRoute('/console/agents/a%2Fb/sessions').name).toBe('not-found')
    expect(matchRoute('/console/agents/a%2Fb/sessions/s-1/extra').name).toBe('not-found')
    // 会话深链的空 id 与非法编码都落到 404。
    expect(matchRoute('/console/agents/reporter/sessions/').name).toBe('not-found')
    expect(matchRoute('/console/agents/reporter/sessions/%E0%A4%A').name).toBe('not-found')
  })

  it('validates the kind on resource detail and decodes the id', () => {
    // 详情页复用固定三值：未知 kind 落到 404；id 按段解码。
    expect(matchRoute('/console/resources/widgets/1').name).toBe('not-found')
    for (const kind of RESOURCE_KINDS) {
      const match = matchRoute(`/console/resources/${kind}/a%2Fb`)
      expect(match.name).toBe('resource-detail')
      expect(match.params.kind).toBe(kind)
      expect(match.params.id).toBe('a/b')
    }
  })

  it('keeps the canonical overview path and decodes route params', () => {
    expect(matchRoute('/console').path).toBe('/console/')
    const detail = matchRoute('/console/agents/a%2Fb')
    expect(detail.name).toBe('agent-detail')
    expect(detail.params.name).toBe('a/b')
  })

  it('treats trailing slashes as equivalent', () => {
    expect(matchRoute('/console/agents/').name).toBe('agents')
    expect(matchRoute('/console/agents/').path).toBe('/console/agents')
  })

  it('falls back to not-found for unknown paths, kinds and bad encodings', () => {
    // 未知路径 / 未知资源类型 / 非法百分号编码都落到 404，不抛错。
    expect(matchRoute('/console/unknown').name).toBe('not-found')
    expect(matchRoute('/console/resources/widgets').name).toBe('not-found')
    expect(matchRoute('/console/agents/%E0%A4%A').name).toBe('not-found')
    expect(matchRoute('/other/path').name).toBe('not-found')
  })

  it('accepts exactly the three resource kinds', () => {
    for (const kind of RESOURCE_KINDS) {
      const match = matchRoute(`/console/resources/${kind}`)
      expect(match.name).toBe('resources')
      expect(match.params.kind).toBe(kind)
    }
  })

  it('preserves the query string for the view layer', () => {
    const match = matchRoute('/console/runs?status=failed&limit=20')
    expect(match.query.get('status')).toBe('failed')
    expect(match.query.get('limit')).toBe('20')
  })
})

describe('safeNextPath', () => {
  it('accepts in-console paths including their query string', () => {
    expect(safeNextPath('/console/agents/reporter')).toBe('/console/agents/reporter')
    expect(safeNextPath('/console/runs?status=running')).toBe('/console/runs?status=running')
  })

  it('rejects external, protocol-relative and non-console targets', () => {
    // 登录后返回路径只允许站内 /console；外部与协议相对路径一律清空。
    expect(safeNextPath('https://evil.example/console/')).toBe('')
    expect(safeNextPath('//evil.example/console/')).toBe('')
    expect(safeNextPath('/api/v1/info')).toBe('')
    expect(safeNextPath('console/agents')).toBe('')
    expect(safeNextPath('')).toBe('')
    expect(safeNextPath(null)).toBe('')
  })

  it('collapses login and not-found targets to the default page', () => {
    expect(safeNextPath('/console/login')).toBe('')
    expect(safeNextPath('/console/login?next=/console/runs')).toBe('')
    expect(safeNextPath('/console/unknown')).toBe('')
  })

  it('rejects backslashes the browser would normalize', () => {
    expect(safeNextPath('/console\\agents')).toBe('')
  })

  it('strips credential-shaped query keys and hashes', () => {
    // next 里携带的凭据形参数与哈希片段都不带进登录页。
    expect(safeNextPath('/console/agents/x?token=abc&tab=sessions')).toBe(
      '/console/agents/x?tab=sessions',
    )
    expect(safeNextPath('/console/agents/x#section')).toBe('/console/agents/x')
  })
})

describe('loginPath and hrefs', () => {
  it('builds the login path with an encoded next parameter', () => {
    expect(loginPath()).toBe('/console/login')
    expect(loginPath('/console/runs?status=failed')).toBe(
      `/console/login?next=${encodeURIComponent('/console/runs?status=failed')}`,
    )
    expect(loginPath('https://evil.example')).toBe('/console/login')
  })

  it('encodes agent, run and session identifiers', () => {
    expect(agentHref('a b')).toBe('/console/agents/a%20b')
    expect(agentConfigHref('a b')).toBe('/console/agents/a%20b/config')
    expect(runHref('run/1')).toBe('/console/runs/run%2F1')
    expect(sessionHref('a b', 's/1')).toBe('/console/agents/a%20b/sessions/s%2F1')
    expect(resourceHref('mcp-servers')).toBe('/console/resources/mcp-servers')
  })
})

describe('ConsoleRouter', () => {
  let router: ConsoleRouter | null = null

  beforeEach(() => {
    resetLocation('/console/agents')
  })

  afterEach(() => {
    router?.stop()
    router = null
  })

  it('dispatches the current location on start and on navigate', () => {
    router = new ConsoleRouter()
    const seen: RouteMatch[] = []
    router.subscribe((match) => seen.push(match))
    router.start()
    expect(router.current?.name).toBe('agents')
    expect(router.navigate('/console/runs')).toBe(true)
    expect(window.location.pathname).toBe('/console/runs')
    expect(router.current?.name).toBe('runs')
    expect(seen.map((match) => match.name)).toEqual(['agents', 'runs'])
  })

  it('ignores targets outside the console base', () => {
    router = new ConsoleRouter()
    router.start()
    expect(router.navigate('https://evil.example/console/runs')).toBe(false)
    expect(window.location.pathname).toBe('/console/agents')
  })

  it('blocks navigation when the leave guard returns false', () => {
    // 未保存编辑的离开守卫：拒绝时 URL 保持不动。
    router = new ConsoleRouter()
    router.start()
    const guard = vi.fn(() => false)
    router.setLeaveGuard(guard)
    expect(router.navigate('/console/runs')).toBe(false)
    expect(guard).toHaveBeenCalledTimes(1)
    expect(window.location.pathname).toBe('/console/agents')
    expect(router.current?.name).toBe('agents')
  })

  it('lets forced navigation bypass the leave guard', () => {
    router = new ConsoleRouter()
    router.start()
    router.setLeaveGuard(() => false)
    expect(router.navigate('/console/runs', { force: true })).toBe(true)
    expect(window.location.pathname).toBe('/console/runs')
  })

  it('restores the view from popstate', () => {
    router = new ConsoleRouter()
    router.start()
    window.history.replaceState(null, '', '/console/runs')
    window.dispatchEvent(new PopStateEvent('popstate'))
    expect(router.current?.name).toBe('runs')
  })

  it('pushes the previous URL back when popstate is blocked', () => {
    // 后退被守卫拦下时把地址栏恢复到当前视图，避免地址与视图不一致。
    router = new ConsoleRouter()
    router.start()
    router.setLeaveGuard(() => false)
    window.history.replaceState(null, '', '/console/runs')
    window.dispatchEvent(new PopStateEvent('popstate'))
    expect(window.location.pathname).toBe('/console/agents')
    expect(router.current?.name).toBe('agents')
  })

  it('exposes the console base constant the server fallback relies on', () => {
    expect(CONSOLE_BASE).toBe('/console')
  })
})
