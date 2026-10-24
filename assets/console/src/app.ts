// 管理台外壳：分组侧栏、当前实例与原有内存认证 / History 路由。
// Console shell with grouped navigation; authentication and route guards stay shared.
import { defineComponent, h, onMounted, onUnmounted, ref } from 'vue'
import type { Component, VNode } from 'vue'
import { ApiClient } from './api'
import { ConsoleAuth } from './auth'
import { brandMark } from './components/brand'
import { CONSOLE_BASE, ConsoleRouter, loginPath, resourceHref } from './router'
import type { RouteMatch, RouteName } from './router'
import { AgentConfigPage } from './views/agent-config'
import { AgentDetailView } from './views/agent-detail'
import { AgentsNewView } from './views/agents-new'
import { AgentsView } from './views/agents'
import { ArchivedAgentsView } from './views/archived-agents'
import { LoginView } from './views/login'
import { NotFoundView } from './views/not-found'
import { OperationsView } from './views/operations'
import { OverviewView } from './views/overview'
import { ResourceDetailView } from './views/resource-detail'
import { ResourcesView } from './views/resources'
import { RunDetailView } from './views/run-detail'
import { RunsView } from './views/runs'
import { SessionDetailView } from './views/session-detail'
import { ToolsView, GlobalMemoryView, NotificationsView, SandboxView } from './views/workspace'
import { WorkspaceBrowserView } from './views/workspace-browser'
import type { ConsoleContext } from './views/shared'

const VIEWS: Record<RouteName, Component> = {
  overview: OverviewView, login: LoginView, agents: AgentsView,
  'agents-new': AgentsNewView, 'agent-detail': AgentDetailView, 'agent-config': AgentConfigPage,
  'agent-session': SessionDetailView,
  runs: RunsView, 'run-detail': RunDetailView,
  resources: ResourcesView, 'resource-detail': ResourceDetailView,
  archived: ArchivedAgentsView, workspace: WorkspaceBrowserView,
  operations: OperationsView, 'not-found': NotFoundView,
  tools: ToolsView, memory: GlobalMemoryView, notifications: NotificationsView, sandbox: SandboxView,
}
interface NavItem { label: string; href: string; code: string; names?: readonly RouteName[] }
const GROUPS: readonly { label: string; items: readonly NavItem[] }[] = [
  { label: '工作区', items: [
    { label: '系统总览', href: CONSOLE_BASE + '/', code: '01', names: ['overview'] },
    { label: 'Agents', href: CONSOLE_BASE + '/agents', code: '02', names: ['agents', 'agents-new', 'agent-detail', 'agent-config', 'agent-session', 'archived'] },
  ] },
  { label: '资源库', items: [
    { label: '模型与连接', href: resourceHref('model-connections'), code: '03' },
    { label: '工具与 MCP', href: CONSOLE_BASE + '/library/tools', code: '04', names: ['tools'] },
    { label: '技能', href: resourceHref('skills'), code: '05' },
    { label: '全局记忆', href: CONSOLE_BASE + '/library/memory', code: '06', names: ['memory'] },
  ] },
  { label: '系统管理', items: [
    { label: '通知渠道', href: CONSOLE_BASE + '/settings/notifications', code: '07', names: ['notifications'] },
    { label: '执行环境', href: CONSOLE_BASE + '/settings/sandbox', code: '08', names: ['sandbox'] },
    { label: '管理操作', href: CONSOLE_BASE + '/operations', code: '09', names: ['operations'] },
  ] },
]
const TITLES: Record<RouteName, string> = {
  overview: '系统总览', login: '连接工作区', agents: 'Agents', 'agents-new': '新建 Agent',
  'agent-detail': 'Agent 工作区', 'agent-config': 'Agent 配置', 'agent-session': '会话详情',
  runs: '运行列表', 'run-detail': '运行详情',
  resources: '资源库', 'resource-detail': '资源详情', archived: '已归档 Agents', workspace: '工作区文件',
  operations: '管理操作', 'not-found': '页面不存在',
  tools: '工具与 MCP', memory: '全局记忆', notifications: '通知渠道', sandbox: '执行环境',
}
function currentLocation(): string { return window.location.pathname + window.location.search }
function navActive(item: NavItem, match: RouteMatch): boolean {
  if (item.label === '工具与 MCP' && match.name === 'resources' && match.params.kind === 'mcp-servers') return true
  return item.names?.includes(match.name) ?? item.href === match.path
}

export function createConsoleApp(): Component {
  const auth = new ConsoleAuth()
  const router = new ConsoleRouter()
  const api = new ApiClient(auth, {
    onAuthFailure: (next) => {
      if (router.current?.name !== 'login') router.navigate(loginPath(next), { replace: true, force: true })
    },
  })
  const context: ConsoleContext = { api, auth, router }
  return defineComponent({
    name: 'ConsoleApp',
    setup() {
      const current = ref<RouteMatch | null>(router.current)
      const authenticated = ref(auth.authenticated)
      const menuOpen = ref(false)
      const unsubscribeRoute = router.subscribe((match) => { current.value = match; menuOpen.value = false })
      const unsubscribeAuth = auth.subscribe((snapshot) => { authenticated.value = snapshot.authenticated })
      onUnmounted(() => { router.stop(); unsubscribeRoute(); unsubscribeAuth() })
      onMounted(() => {
        router.start()
        const match = current.value
        if (!authenticated.value && match !== null && match.name !== 'login' && match.name !== 'not-found') {
          router.navigate(loginPath(currentLocation()), { replace: true, force: true })
        }
      })
      function logout(): void {
        auth.clear()
        router.navigate(loginPath(), { replace: true, force: true })
      }
      function sidebar(match: RouteMatch): VNode {
        return h('aside', { id: 'console-navigation', class: ['console-sidebar', menuOpen.value ? 'is-open' : ''] }, [
          h('a', { href: CONSOLE_BASE + '/', class: 'console-brand', 'aria-label': 'Basalt 系统总览' }, [
            brandMark(), h('span', [h('strong', 'BASALT'), h('small', 'AGENT OS')]),
          ]),
          h('nav', { 'aria-label': '管理台主导航' }, GROUPS.map((group) => h('div', { class: 'nav-group', key: group.label }, [
            h('p', { class: 'nav-group-label' }, group.label),
            ...group.items.map((item) => h('a', {
              key: item.href, href: item.href, class: ['nav-item', navActive(item, match) ? 'active' : ''],
              'aria-current': navActive(item, match) ? 'page' : undefined,
            }, [h('span', { class: 'nav-index', 'aria-hidden': 'true' }, item.code), h('span', item.label)])),
          ]))),
          h('div', { class: 'sidebar-footer' }, [
            h('p', { class: 'sidebar-caption' }, 'AGENT OPERATING SYSTEM'),
            h('div', { class: 'instance-label' }, [h('span', { class: 'instance-dot', 'aria-hidden': 'true' }), h('span', '当前实例')]),
            h('code', window.location.host),
          ]),
        ])
      }
      return (): VNode => {
        const match = current.value
        if (match === null) return h('p', { class: 'loading' }, '加载中…')
        const showChrome = authenticated.value && match.name !== 'login'
        const resourceTitle = match.params.kind === 'skills' ? '技能' : match.params.kind === 'mcp-servers' ? '工具与 MCP' : '模型与连接'
        return h('div', { class: ['console-shell', showChrome ? '' : 'console-shell--public'], onKeydown: (event: KeyboardEvent) => { if (event.key === 'Escape') menuOpen.value = false } }, [
          h('a', { href: '#console-main', class: 'skip-link', onClick: (event: MouseEvent) => {
            // 页内焦点跳转不进入路由，也不触发配置离开守卫。 In-page focus, not navigation.
            event.preventDefault()
            menuOpen.value = false
            document.getElementById('console-main')?.focus()
          } }, '跳到主要内容'),
          showChrome ? sidebar(match) : null,
          showChrome && menuOpen.value ? h('button', { class: 'sidebar-backdrop', 'aria-label': '关闭导航', onClick: () => { menuOpen.value = false } }) : null,
          h('div', { class: 'console-body' }, [
            showChrome ? h('header', { class: 'console-topbar' }, [
              h('button', { class: 'menu-toggle', 'aria-label': '打开导航', 'aria-expanded': menuOpen.value, 'aria-controls': 'console-navigation', onClick: () => { menuOpen.value = !menuOpen.value } }, '☰'),
              h('div', { class: 'topbar-breadcrumb' }, [h('span', '管理台'), h('span', { 'aria-hidden': 'true' }, '/'), h('strong', match.name === 'resources' ? resourceTitle : TITLES[match.name])]),
              h('div', { class: 'topbar-actions' }, [h('span', { class: 'topbar-label' }, 'SELF-HOSTED'), h('button', { type: 'button', class: 'link-button', onClick: logout }, '退出登录')]),
            ]) : null,
            h('main', { class: 'console-main', id: 'console-main', tabindex: -1 }, [h(VIEWS[match.name], { key: match.name + ':' + match.path, ctx: context, params: match.params, query: match.query })]),
          ]),
        ])
      }
    },
  })
}
