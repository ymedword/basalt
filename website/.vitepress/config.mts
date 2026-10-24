import { defineConfig } from 'vitepress'
import { BASE, resolveNav } from './data/navigation.mjs'

export default defineConfig({
  title: 'Basalt',
  description: 'A self-hosted Agent OS for enterprise teams.',
  base: BASE,
  lang: 'en',
  appearance: false,
  srcExclude: ['README.md'],
  head: [['link', { rel: 'icon', type: 'image/svg+xml', href: `${BASE}brand/favicon.svg` }]],
  locales: {
    root: { label: 'English', lang: 'en', themeConfig: { nav: resolveNav('en'), outline: { label: 'On this page', level: [2, 3] }, notFound: { title: 'Page not found', quote: 'This page is not here. Start again from Basalt.', linkLabel: 'Go home', linkText: 'Go home' }, skipToContentLabel: 'Skip to content' } },
    zh: { label: '简体中文', lang: 'zh-CN', description: '面向企业团队的自托管 Agent OS。', themeConfig: { nav: resolveNav('zh'), outline: { label: '本页内容', level: [2, 3] }, notFound: { title: '页面未找到', quote: '此页面不存在，请从 Basalt 首页继续。', linkLabel: '返回首页', linkText: '返回首页' }, skipToContentLabel: '跳到正文', returnToTopLabel: '回到顶部', sidebarMenuLabel: '目录', langMenuLabel: '语言', darkModeSwitchLabel: '显示主题', docFooter: { prev: '上一页', next: '下一页' } } }
  },
  themeConfig: {
    logo: '/brand/basalt-logo.svg', siteTitle: false,
    outline: [2, 3], externalLinkIcon: true,
    docFooter: { prev: 'Previous', next: 'Next' }
  },
  vite: { server: { host: '127.0.0.1' } }
})
