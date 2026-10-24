export const BASE = '/basalt/'
export const REPO_URL = 'https://github.com/ymedword/basalt'
export const ISSUES_URL = `${REPO_URL}/issues`
export const OPERATIONS_URL = `${REPO_URL}/blob/main/docs/Operations.md`
export const ROUTES = [
  { id: 'home', en: '/', zh: '/zh/', anchors: ['demo'] },
  { id: 'quickstart', en: '/quickstart/', zh: '/zh/quickstart/', anchors: ['deploy', 'config', 'console', 'commands', 'first-chat', 'verify', 'troubleshoot'] },
  { id: 'architecture', en: '/architecture/', zh: '/zh/architecture/', anchors: ['overview', 'capabilities', 'execution', 'tools-and-mcp', 'diagrams', 'current-scope'] },
  { id: 'docs', en: '/docs/', zh: '/zh/docs/', anchors: ['what-is-basalt', 'agent-os', 'current-capabilities', 'concepts', 'roadmap'] }
]

export function localePath(locale, routeId) {
  const route = ROUTES.find(item => item.id === routeId)
  if (!route) throw new Error(`Unknown route: ${routeId}`)
  return route[locale === 'zh' ? 'zh' : 'en']
}

// 先剥离部署前缀，再映射页面；查询与片段保持原样。
// Strip the deployment prefix before matching; preserve the query and fragment.
export function switchLocalePath(currentPath, targetLocale, base = BASE) {
  const url = new URL(currentPath, 'https://local.invalid')
  const prefix = `/${base.split('/').filter(Boolean).join('/')}`
  let pathname = url.pathname
  if (prefix !== '/' && (pathname === prefix || pathname.startsWith(`${prefix}/`))) pathname = pathname.slice(prefix.length) || '/'
  pathname = pathname.replace(/(?:index\.html|\.html)$/, '')
  if (!pathname.endsWith('/')) pathname += '/'
  const route = ROUTES.find(item => item.en === pathname || item.zh === pathname)
  const target = localePath(targetLocale, route?.id ?? 'home')
  return `${prefix === '/' ? '' : prefix}${target}${url.search}${url.hash}`
}

export function resolveNav(locale) {
  const zh = locale === 'zh'
  return [
    { text: zh ? '首页' : 'Home', link: localePath(locale, 'home'), activeMatch: zh ? '^/zh/$' : '^/$' },
    { text: zh ? '文档' : 'Docs', link: localePath(locale, 'docs'), activeMatch: zh ? '^/zh/(docs|quickstart|architecture)/' : '^/(docs|quickstart|architecture)/' },
    { text: 'GitHub', link: REPO_URL },
    { text: zh ? '演示' : 'Demo', link: `${localePath(locale, 'home')}#demo` }
  ]
}

export function resolveFooter(locale) {
  const zh = locale === 'zh'
  const local = (text, route, anchor = '') => ({ text, link: `${localePath(locale, route)}${anchor ? `#${anchor}` : ''}` })
  return [
    { title: zh ? '开始' : 'START', links: [local(zh ? '部署指南' : 'Deployment guide', 'quickstart', 'deploy'), local(zh ? 'CLI 演示' : 'CLI demo', 'home', 'demo')] },
    { title: zh ? '文档' : 'DOCS', links: [local(zh ? '架构' : 'Architecture', 'architecture'), local(zh ? '配置' : 'Configuration', 'quickstart', 'config'), local(zh ? 'CLI 命令' : 'CLI commands', 'quickstart', 'commands'), local(zh ? '工具与 MCP' : 'Tools & MCP', 'architecture', 'tools-and-mcp'), { text: zh ? '运维手册（GitHub）' : 'Operations (GitHub)', link: OPERATIONS_URL }] },
    { title: zh ? '项目' : 'PROJECT', links: [{ text: 'GitHub', link: REPO_URL }, { text: zh ? '问题反馈' : 'Issues', link: ISSUES_URL }] }
  ]
}
