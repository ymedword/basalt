import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { DIAGRAMS } from '../.vitepress/data/capabilities.mjs'
const IDS = ['project-overview', 'system-architecture', 'execution-flow', 'module-map', 'package-dependencies', 'deployment']
function isAncestor(root, commit) {
  if (!/^[0-9a-f]{7,40}$/.test(commit)) return false
  try { execFileSync('git', ['merge-base', '--is-ancestor', commit, 'HEAD'], { cwd: root, stdio: 'ignore' }); return true } catch { return false }
}
export function checkDiagrams({ root, diagrams = DIAGRAMS }) {
  const findings = []
  const ids = diagrams.map(item => item.id)
  for (const id of IDS) if (!ids.includes(id)) findings.push({ diagram: id, reason: 'missing-diagram' })
  for (const id of ids) if (!IDS.includes(id)) findings.push({ diagram: id, reason: 'unexpected-diagram' })
  if (new Set(ids).size !== ids.length) findings.push({ reason: 'duplicate-diagram-id' })
  for (const locale of ['en', 'zh']) {
    const directory = path.join(root, 'public', 'diagrams', locale)
    if (existsSync(directory)) for (const name of readdirSync(directory)) if (name.endsWith('.svg') && !IDS.includes(name.slice(0, -4))) findings.push({ locale, diagram: name, reason: 'unexpected-file' })
  }
  for (const diagram of diagrams) {
    const boxes = {}
    for (const locale of ['en', 'zh']) {
      const file = path.join(root, 'public', 'diagrams', locale, `${diagram.id}.svg`)
      const fail = reason => findings.push({ diagram: diagram.id, locale, reason })
      if (!existsSync(file)) { fail('missing-file'); continue }
      const svg = readFileSync(file, 'utf8')
      const commit = svg.match(/\bdata-source-commit="([^"]+)"/)?.[1]
      if (!svg.includes(`data-diagram-id="${diagram.id}"`) || !commit) fail('missing-provenance')
      else if (!isAncestor(root, commit)) fail('invalid-provenance')
      if (!/<title[^>]*>[^<]+<\/title>/.test(svg) || !/<desc[^>]*>[\s\S]+?<\/desc>/.test(svg) || !/\bviewBox=/.test(svg)) fail('missing-metadata')
      if (/(?:href|xlink:href)\s*=\s*["'](?!#)|url\(\s*["']?(?:https?:|\/\/)|@import\b|<script\b|<foreignObject\b|\bon\w+\s*=/i.test(svg)) fail('external-or-script')
      const desc = svg.match(/<desc[^>]*>([\s\S]*?)<\/desc>/)?.[1] ?? ''
      if (diagram.id === 'execution-flow') {
        for (const state of ['completed', 'limit_reached', 'cancelled', 'failed']) if (!desc.includes(state)) fail(`desc-missing-${state}`)
        for (const [label, expression] of [['failure', /failure|失败/i], ['denial', /denial|拒绝/i], ['cancel', /cancel|取消/i], ['recovery', /recovery|恢复/i]]) if (!expression.test(desc)) fail(`desc-missing-${label}`)
      }
      boxes[locale] = svg.match(/\bviewBox="([^"]+)"/)?.[1]
    }
    if (boxes.en && boxes.zh && boxes.en !== boxes.zh) findings.push({ diagram: diagram.id, reason: 'viewbox-mismatch' })
  }
  const file = path.join(root, 'diagrams', 'basalt-diagrams.drawio')
  if (!existsSync(file)) findings.push({ source: 'drawio', reason: 'missing-source' })
  else {
    const source = readFileSync(file, 'utf8')
    const commit = source.match(/<mxfile\b[^>]*\bdata-source-commit="([^"]+)"/)?.[1]
    if (!commit) findings.push({ source: 'drawio', reason: 'missing-source-provenance' })
    else if (!isAncestor(root, commit)) findings.push({ source: 'drawio', reason: 'invalid-source-provenance' })
    const pages = [...source.matchAll(/<diagram\b[^>]*\bname="([^"]+)"/g)].map(match => match[1])
    if (pages.length !== IDS.length * 2) findings.push({ source: 'drawio', reason: `page-count-${pages.length}` })
    for (const id of IDS) if (pages.filter(name => name.startsWith(id)).length !== 2) findings.push({ source: 'drawio', reason: `missing-page-${id}` })
  }
  return findings
}
export function main() {
  const findings = checkDiagrams({ root: fileURLToPath(new URL('../', import.meta.url)) })
  for (const finding of findings) console.error(JSON.stringify(finding))
  if (findings.length) process.exitCode = 1
  else console.log('Diagrams: no findings')
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main()
