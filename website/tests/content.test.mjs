import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile, readdir, access } from 'node:fs/promises'
import { pathToFileURL } from 'node:url'
import { CAPABILITIES, CORE_CAPABILITIES, DIAGRAMS } from '../.vitepress/data/capabilities.mjs'
import { CATALOG, DEMOS, TEMPLATES, TIERS } from '../.vitepress/data/templates.mjs'
import { ROUTES, resolveFooter } from '../.vitepress/data/navigation.mjs'
import { QUICKSTART_COMMANDS, DURATION_MEASURED, MINIMAL_CONFIG, TOKEN_GENERATION, PREREQUISITES, assertQuickstartParity, extractBasaltCommands } from '../.vitepress/data/quickstart.mjs'
import { isForbiddenPath } from '../scripts/check-links.mjs'
import { replyFor } from '../demo/local-model.mjs'

const root = new URL('../../', import.meta.url)
export const CONTENT_FILES = ['README.md', ...ROUTES.flatMap(route => ['en', 'zh'].map(locale => `website${route[locale]}index.md`)), 'website/.vitepress/data/quickstart.mjs', 'website/.vitepress/data/templates.mjs', 'website/demo/transcript.json', 'website/.vitepress/theme/components/CapabilityGrid.vue', 'website/.vitepress/theme/components/CliDemo.vue', 'website/.vitepress/theme/components/TemplateTable.vue']
export async function loadContentFiles() {
  return Object.fromEntries(await Promise.all(CONTENT_FILES.map(async file => [file, await readFile(new URL(file, root), 'utf8')])))
}

// 文件映射可注入，让负向夹具与真实站点使用相同规则。
// An injected file map lets negative fixtures exercise the actual content rules.
export function checkPublicContent(files) {
  const findings = []
  const report = (file, reason) => findings.push({ file, reason })
  for (const file of CONTENT_FILES) if (typeof files[file] !== 'string') report(file, 'missing-file')
  if (findings.length) return findings
  try { assertQuickstartParity(files['README.md']) } catch (error) { report('README', error.message) }
  const titles = new Set()
  const descriptions = new Set()
  const duration = /(?:\d+|[一二三四五六七八九十两]+|one|two|three|four|five|ten)\s*(?:hours?|hrs?|minutes?|mins?|seconds?|secs?|milliseconds?|ms)\b|(?:\d+|[一二三四五六七八九十两]+|几)\s*(?:小时|分钟|毫秒|秒(?:钟)?)/i
  for (const [file, text] of Object.entries(files)) {
    if (/--profile|basalt\s+profile\s+enable\b/.test(text)) report(file, 'invalid-cli')
    if (/已上线|已验证/.test(text)) report(file, 'unverified-claim')
    if (!DURATION_MEASURED && !file.includes('components/') && duration.test(text)) report(file, 'unmeasured-duration')
    for (const link of text.matchAll(/\]\(([^)]+)\)|\bhref=["']([^"']+)["']/g)) {
      let ref = link[1] ?? link[2]
      if (ref.startsWith('http')) {
        try { ref = new URL(ref).pathname.replace(/^\/ymedword\/basalt\/(?:blob|tree)\/[^/]+\//, '') } catch { report(file, 'invalid-link'); continue }
      }
      if (isForbiddenPath(ref, true)) report(file, 'private-path')
    }
    if (file.startsWith('website/') && file.endsWith('/index.md')) {
      const title = text.match(/^title:\s*(.+)$/m)?.[1]
      const description = text.match(/^description:\s*(.+)$/m)?.[1]
      if (!title || titles.has(title)) report(file, 'missing-or-duplicate-title')
      if (!description || descriptions.has(description)) report(file, 'missing-or-duplicate-description')
      titles.add(title); descriptions.add(description)
    }
  }
  for (const locale of ['en', 'zh']) {
    const quick = files[`website${locale === 'zh' ? '/zh' : ''}/quickstart/index.md`]
    const commands = extractBasaltCommands(quick)
    if (JSON.stringify(commands) !== JSON.stringify(QUICKSTART_COMMANDS)) report(locale, 'quickstart-command-drift')
    for (const marker of ['registered=true', '请在此填写模型名', 'disabled', 'Convert]::ToBase64String', 'RandomNumberGenerator', 'openssl rand', '.basalt/basalt.db']) if (!quick.includes(marker)) report(locale, `quickstart-missing-${marker}`)
    if (!/registered=true[^\n]*(不等于|≠|only means|does not mean)/i.test(quick)) report(locale, 'registered-is-not-runnable')
    if (!/session list[\s\S]{0,400}(?:共\s*\d+\s*个会话|未发现会话)/.test(quick)) report(locale, 'missing-session-output')
    const docs = files[`website${locale === 'zh' ? '/zh' : ''}/docs/index.md`]
    if (!/no site search|无站内搜索/.test(docs) || /<(?:input|button)[^>]*(?:search|搜索)/i.test(docs)) report(locale, 'search-contract')
    for (const section of ['demos', 'templates', 'catalog']) {
      if (!new RegExp(`<TemplateTable section="${section}"`).test(docs)) report(locale, `missing-template-table-${section}`)
    }
    const architecture = files[`website${locale === 'zh' ? '/zh' : ''}/architecture/index.md`]
    for (const { id } of DIAGRAMS) if (!architecture.includes(`<DiagramFigure id="${id}"`)) report(locale, `missing-diagram-${id}`)
    const statement = architecture.split('{#current-scope}')[0]
    if (/supports distributed|supports vector|支持分布式|支持向量检索|自动生成技能/i.test(statement)) report(locale, 'future-capability-claim')
    for (const name of ['daily-report', 'file-review']) {
      if (!architecture.includes(name)) report(locale, `missing-agent-example-${name}`)
    }
  }
  for (const name of ['daily-report', 'file-review']) if (!files['README.md'].includes(name)) report('README', `missing-readme-agent-${name}`)
  for (const [file, text] of Object.entries(files).filter(([file]) => file.startsWith('README'))) {
    const diagrams = [...text.matchAll(/!\[[^\]]*\]\([^)]*diagrams\/[^/]+\/([^.)]+)\.svg\)/g)].map(match => match[1])
    if (JSON.stringify(diagrams) !== JSON.stringify(['system-architecture'])) report(file, 'readme-diagram-scope')
  }
  if (!files['website/.vitepress/theme/components/CapabilityGrid.vue'].includes('Platform capabilities')) report('CapabilityGrid', 'platform-heading')
  if (!files['website/.vitepress/theme/components/CliDemo.vue'].includes('DEMO_LABEL')) report('CliDemo', 'missing-scripted-disclosure')
  try {
    const transcript = JSON.parse(files['website/demo/transcript.json'])
    for (const key of ['capturedAt', 'source', 'stub', 'command', 'lines', 'exitCode']) if (!(key in transcript)) report('transcript', `missing-${key}`)
    if (transcript.command !== QUICKSTART_COMMANDS[4]) report('transcript', 'command-drift')
    if (transcript.source !== 'local-openai-compatible' || transcript.stub !== 'website/demo/local-model.mjs' || transcript.exitCode !== 0 || Number.isNaN(Date.parse(transcript.capturedAt))) report('transcript', 'invalid-source')
    const expected = replyFor([{ role: 'user', content: 'Hello' }, { role: 'tool', tool_call_id: 'basalt-demo-readme', content: '# Basalt\nA self-hosted Agent OS.' }]).content.split('\n')
    if (!Array.isArray(transcript.lines) || JSON.stringify(transcript.lines.slice(-expected.length)) !== JSON.stringify(expected)) report('transcript', 'reply-drift')
  } catch { report('transcript', 'invalid-json') }
  return findings
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  test('public content passes all shared content and command checks', async () => assert.deepEqual(checkPublicContent(await loadContentFiles()), []))
  test('platform six and core five have present implementation evidence', async () => {
    assert.equal(CAPABILITIES.length, 6)
    assert.deepEqual(CORE_CAPABILITIES.map(item => item.id), ['provider', 'react', 'memory', 'tool', 'web'])
    for (const item of [...CAPABILITIES, ...CORE_CAPABILITIES]) { await access(new URL(item.evidence, root)); assert.ok(item.titleEn && item.titleZh && item.bodyEn && item.bodyZh) }
    for (const item of CAPABILITIES) { await access(new URL(`website/public${item.illustration}`, root)); assert.ok(item.altEn && item.altZh) }
  })
  // 模板表必须与仓库目录和内置目录数据逐条对应，不能靠手抄；档位与双语字段一起判。
  // The public tables must correspond entry-for-entry to the repository sources.
  test('template table matches examples directories and the bundled MCP catalog', async () => {
    const agentDirectories = async directory => {
      const names = []
      for (const item of await readdir(new URL(directory, root), { withFileTypes: true })) {
        if (!item.isDirectory()) continue
        try { await access(new URL(`${directory}${item.name}/AGENT.md`, root)) } catch { continue }
        names.push(item.name)
      }
      return names.sort()
    }
    assert.equal(DEMOS.length, 5)
    assert.equal(TEMPLATES.length, 8)
    assert.equal(CATALOG.length, 6)
    assert.deepEqual(DEMOS.map(item => item.id).sort(), await agentDirectories('examples/daily/'))
    assert.deepEqual(TEMPLATES.map(item => item.id).sort(), await agentDirectories('examples/templates/'))
    const catalog = await readFile(new URL('internal/catalog/data/mcp_catalog.yaml', root), 'utf8')
    assert.deepEqual(CATALOG.map(item => item.id).sort(), [...catalog.matchAll(/^\s*-\s+id:\s*(\S+)\s*$/gm)].map(match => match[1]).sort())
    const entries = [...DEMOS, ...TEMPLATES]
    assert.equal(new Set(DEMOS.map(item => item.id)).size, 5)
    assert.equal(new Set(TEMPLATES.map(item => item.id)).size, 8)
    assert.equal(new Set(CATALOG.map(item => item.id)).size, 6)
    for (const item of entries) {
      assert.equal(item.path, `examples/${item.path.split('/')[1]}/${item.id}/`, `${item.id}: path does not mirror its id`)
      assert.ok(!isForbiddenPath(item.path, true), `${item.id}: forbidden path ${item.path}`)
      await access(new URL(item.path, root))
      for (const field of ['titleEn', 'titleZh', 'needsEn', 'needsZh']) assert.ok(item[field], `${item.id}: missing ${field}`)
    }
    for (const item of DEMOS) for (const field of ['bodyEn', 'bodyZh', 'evidenceEn', 'evidenceZh']) assert.ok(item[field], `${item.id}: missing ${field}`)
    for (const item of TEMPLATES) for (const field of ['domainEn', 'domainZh']) assert.ok(item[field], `${item.id}: missing ${field}`)
    for (const item of CATALOG) for (const field of ['nameEn', 'nameZh', 'summaryEn', 'summaryZh', 'needsEn', 'needsZh', 'limitEn', 'limitZh']) assert.ok(item[field], `${item.id}: missing ${field}`)
    assert.deepEqual(TIERS.map(item => item.tier), ['A', 'A*', 'B'])
    const tiers = TEMPLATES.map(item => item.tier)
    for (const tier of tiers) assert.ok(TIERS.some(item => item.tier === tier), `undeclared tier ${tier}`)
    assert.deepEqual([tiers.filter(tier => tier === 'A').length, tiers.filter(tier => tier === 'A*').length, tiers.filter(tier => tier === 'B').length], [6, 1, 1])
  })
  test('configuration, token methods and dependency scenarios are complete', () => {
    for (const marker of ['${BASALT_MODEL_KEY}', '${BASALT_ADMIN_TOKEN}', '127.0.0.1:8080', 'credential_mode: env']) assert.ok(MINIMAL_CONFIG.includes(marker))
    assert.match(TOKEN_GENERATION.windows, /RandomNumberGenerator/)
    assert.match(TOKEN_GENERATION.posix, /openssl rand/)
    assert.equal(PREREQUISITES.length, 5)
    for (const locale of ['en', 'zh']) assert.ok(!/\b(?:Go|Node|PostgreSQL|MySQL|Redis)\b/.test(PREREQUISITES[0][locale]))
    assert.match(PREREQUISITES[1].en, /Go/)
    assert.match(PREREQUISITES[4].en, /Node/)
  })
  test('footer includes only the approved destination set', () => {
    assert.equal(resolveFooter('en').length, 3)
    assert.equal(resolveFooter('zh').length, 3)
    assert.equal(resolveFooter('en').flatMap(group => group.links).length, 9)
  })
  // 站点分发包必须携带与仓库根一致的许可与第三方声明。
  // The deployed site must carry the same license and notices as the repository root.
  test('website notice copies match the repository notices', async () => {
    for (const name of ['LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES']) {
      assert.equal(await readFile(new URL(`website/public/${name}`, root), 'utf8'), await readFile(new URL(name, root), 'utf8'))
    }
  })
}
