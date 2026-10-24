import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { BASE, ROUTES, localePath, switchLocalePath, resolveNav, resolveFooter, REPO_URL } from '../.vitepress/data/navigation.mjs'

test('four routes are paired, and every declared anchor exists in both source pages', async () => {
  assert.deepEqual(ROUTES.map(item => item.id), ['home', 'quickstart', 'architecture', 'docs'])
  for (const route of ROUTES) for (const locale of ['en', 'zh']) {
    const page = await readFile(new URL(`..${route[locale]}index.md`, import.meta.url), 'utf8')
    for (const anchor of route.anchors) assert.ok(page.includes(`{#${anchor}}`), `${route[locale]} missing explicit anchor ${anchor}`)
  }
})
test('locale switching preserves route, query, hash and exactly one base', () => {
  for (const base of [BASE, '/', '/nested/project/']) for (const route of ROUTES) for (const target of ['en', 'zh']) {
    const prefix = base === '/' ? '' : base.slice(0, -1)
    for (const source of ['en', 'zh']) {
      const input = `${prefix}${route[source]}?from=readme&x=a%20b#config`
      assert.equal(switchLocalePath(input, target, base), `${prefix}${route[target]}?from=readme&x=a%20b#config`)
    }
  }
  assert.equal(switchLocalePath('/basalt/unknown/?q=1#x', 'zh'), '/basalt/zh/?q=1#x')
  assert.equal(switchLocalePath('/basalt/zh/404.html', 'en'), '/basalt/')
  assert.equal(switchLocalePath('/basalt/quickstart/index.html', 'zh'), '/basalt/zh/quickstart/')
})
test('navigation and footer resolve to the approved real targets', () => {
  for (const locale of ['en', 'zh']) {
    const nav = resolveNav(locale)
    assert.deepEqual(nav.map(item => item.link), [localePath(locale, 'home'), localePath(locale, 'docs'), REPO_URL, `${localePath(locale, 'home')}#demo`])
    const links = resolveFooter(locale).flatMap(group => group.links).map(item => item.link)
    for (const target of [`${localePath(locale, 'quickstart')}#deploy`, `${localePath(locale, 'quickstart')}#commands`, `${localePath(locale, 'architecture')}#tools-and-mcp`]) assert.ok(links.includes(target), target)
    assert.ok(!links.some(link => /contribut|license/.test(link)))
  }
})
test('404 strings are localized without adding a ninth content page', async () => {
  const config = await readFile(new URL('../.vitepress/config.mts', import.meta.url), 'utf8')
  assert.match(config, /Page not found/)
  assert.match(config, /页面未找到/)
  assert.match(config, /Go home/)
  assert.match(config, /返回首页/)
  assert.equal(ROUTES.length * 2, 8)
})
