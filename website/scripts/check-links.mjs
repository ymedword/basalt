import { readFileSync, readdirSync, existsSync, statSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { BASE, REPO_URL } from '../.vitepress/data/navigation.mjs'

export function isForbiddenPath(value, internal = false) {
  let name
  try { name = decodeURIComponent(value).replace(/\\/g, '/').replace(/^\/+/, '').split(/[?#]/)[0] } catch { return true }
  name = path.posix.normalize(name).replace(/^\.\//, '')
  if (internal && ['docs/', 'docs/index.html', 'docs'].includes(name)) return false
  if (/(^|\/)(career|specs|harness|\.specify|\.claude)(\/|$)/i.test(name)) return true
  if (/(^|\/)(CLAUDE(?:\.local)?\.md|\.gitmessage)$/i.test(name)) return true
  if (/^docs\//i.test(name)) return !/^docs\/(?:Operations\.md$|contracts\/|assets\/)/.test(name)
  return name.startsWith('../')
}
function walk(directory) {
  return readdirSync(directory, { withFileTypes: true }).flatMap(entry => entry.isDirectory() ? walk(path.join(directory, entry.name)) : [path.join(directory, entry.name)])
}
function decode(value) {
  return value.replace(/&amp;/g, '&').replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&#(\d+);/g, (_, code) => String.fromCharCode(Number(code)))
}
function allowedExternal(url) {
  const repo = new URL(REPO_URL)
  return (url.protocol === 'https:' && url.hostname === repo.hostname && (url.pathname === repo.pathname || url.pathname.startsWith(repo.pathname + '/'))) ||
    (url.protocol === 'http:' && url.hostname === '127.0.0.1' && url.port === '8080' && url.pathname === '/console/' && !url.search)
}
export function checkLinks({ distDir, base = BASE }) {
  const findings = []
  if (!existsSync(distDir)) return [{ page: distDir, reason: 'missing-dist' }]
  const root = path.resolve(distDir)
  const report = (page, ref, reason) => findings.push({ page: path.relative(root, page), ref, reason })
  const files = walk(root)
  const pages = files.filter(file => file.endsWith('.html'))
  if (!pages.length) return [{ page: root, reason: 'missing-html' }]
  for (const page of pages) {
    const html = readFileSync(page, 'utf8')
    const pageUrl = new URL(base + path.relative(root, page).replaceAll(path.sep, '/'), 'https://local.invalid')
    for (const match of html.matchAll(/<([a-z][\w:-]*)\b[^>]*?\s(?:href|src)\s*=\s*(?:"([^"]*)"|'([^']*)')/gi)) {
      const tag = match[1].toLowerCase()
      const ref = decode(match[2] ?? match[3])
      if (!ref.trim() || ref === '#') { report(page, ref, 'empty-link'); continue }
      if (/^javascript:/i.test(ref)) { report(page, ref, 'javascript-url'); continue }
      if (/^data:/i.test(ref)) { if (tag !== 'img' || !/^data:image\/(?:svg\+xml|png|webp);/i.test(ref)) report(page, ref, 'unsafe-data-url'); continue }
      let url
      try { url = new URL(ref, pageUrl) } catch { report(page, ref, 'invalid-url'); continue }
      if (url.origin !== pageUrl.origin) {
        if (tag !== 'a') { report(page, ref, 'external-resource'); continue }
        const repoPath = url.pathname.replace(/^\/ymedword\/basalt\/(?:blob|tree)\/[^/]+\//, '')
        if (isForbiddenPath(repoPath)) report(page, ref, 'private-path')
        else if (!allowedExternal(url)) report(page, ref, 'external-not-allowed')
        continue
      }
      if (!url.pathname.startsWith(base)) { report(page, ref, 'missing-base'); continue }
      let relative
      try { relative = decodeURIComponent(url.pathname.slice(base.length)) } catch { report(page, ref, 'invalid-url'); continue }
      if (isForbiddenPath(relative, true)) { report(page, ref, 'private-path'); continue }
      let target = path.resolve(root, relative)
      if (!(target === root || target.startsWith(root + path.sep))) { report(page, ref, 'outside-dist'); continue }
      if (existsSync(target) && statSync(target).isDirectory()) target = path.join(target, 'index.html')
      else if (!existsSync(target) && !path.extname(target)) target += '.html'
      if (!existsSync(target) || !statSync(target).isFile()) { report(page, ref, 'missing-target'); continue }
      if (url.hash && target.endsWith('.html')) {
        let anchor
        try { anchor = decodeURIComponent(url.hash.slice(1)) } catch { report(page, ref, 'invalid-anchor'); continue }
        const ids = [...readFileSync(target, 'utf8').matchAll(/\bid=["']([^"']+)["']/g)].map(item => decode(item[1]))
        if (!ids.includes(anchor)) report(page, ref, 'missing-anchor')
      }
    }
  }
  for (const css of files.filter(file => file.endsWith('.css'))) {
    const text = readFileSync(css, 'utf8')
    if (/@import\b|url\(\s*["']?(?:https?:|\/\/)/i.test(text)) report(css, '', 'external-css-resource')
  }
  return findings
}
export function main() {
  const findings = checkLinks({ distDir: fileURLToPath(new URL('../.vitepress/dist/', import.meta.url)), base: BASE })
  for (const finding of findings) console.error(JSON.stringify(finding))
  if (findings.length) process.exitCode = 1
  else console.log('Links: no findings')
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main()
