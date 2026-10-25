import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, readFile, writeFile, cp, rm, unlink } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { checkLinks } from '../scripts/check-links.mjs'
import { checkDiagrams } from '../scripts/check-diagrams.mjs'
import { checkScope, ALLOWLIST } from '../scripts/check-scope.mjs'
import { checkPublicContent, loadContentFiles } from './content.test.mjs'
import { DIAGRAMS } from '../.vitepress/data/capabilities.mjs'

const root = fileURLToPath(new URL('../../', import.meta.url))
const website = path.join(root, 'website')
async function fixture(t) {
  // Keep fixtures inside this checkout so real Git ancestry checks remain active.
  // 夹具留在仓库内，真实提交祖先校验仍然生效。
  const parent = path.join(root, '.loop', 'us13-test-fixtures')
  await mkdir(parent, { recursive: true })
  const directory = await mkdtemp(path.join(parent, 'case-'))
  t.after(async () => {
    assert.equal(path.dirname(directory), parent)
    assert.ok(path.basename(directory).startsWith('case-'))
    await rm(directory, { recursive: true, force: true })
  })
  return directory
}
const reasons = findings => findings.map(item => item.reason)

test('link fixtures accept valid base and anchors and reject broken or private targets', async t => {
  const distDir = await fixture(t)
  await mkdir(path.join(distDir, 'quickstart'))
  await writeFile(path.join(distDir, 'quickstart', 'index.html'), '<h1 id="deploy">Deploy</h1>')
  const file = path.join(distDir, 'index.html')
  await writeFile(file, '<a href="/basalt/quickstart/#deploy">Deploy</a>')
  assert.deepEqual(checkLinks({ distDir }), [])
  // 模板表把 examples/ 作为公开目标链接到仓库；该路径允许，私有与工程路径仍拒绝。
  // The template tables link examples/ back to the repository; that path is allowed, private
  // and engineering paths are not.
  await writeFile(file, '<a href="https://github.com/ymedword/basalt/tree/main/examples/daily/github-radar">Example</a>')
  assert.deepEqual(checkLinks({ distDir }), [])
  for (const [ref, reason] of [
    ['/basalt/missing/', 'missing-target'], ['/basalt/quickstart/#absent', 'missing-anchor'],
    ['/quickstart/', 'missing-base'], ['/basalt/career/private.md', 'private-path'],
    ['https://github.com/ymedword/basalt/blob/main/docs/Requirements.md', 'private-path'],
    ['https://github.com/ymedword/basalt/tree/main/docs/us/us14-templates-demos-release.md', 'private-path'],
    ['https://github.com/ymedword/basalt/tree/main/examples/../career/private.md', 'private-path'],
    ['https://unapproved.invalid/', 'external-not-allowed'], ['', 'empty-link'],
    ['#', 'empty-link'], ['javascript:alert(1)', 'javascript-url']
  ]) {
    await writeFile(file, `<a href="${ref}">Link</a>`)
    assert.ok(reasons(checkLinks({ distDir })).includes(reason), `${ref}: ${reason}`)
  }
  await writeFile(file, '<img src="https://cdn.invalid/art.svg">')
  assert.ok(reasons(checkLinks({ distDir })).includes('external-resource'))
  await writeFile(file, '<p>Local only</p>')
  await writeFile(path.join(distDir, 'bad.css'), '@import url(https://cdn.invalid/font.css);')
  assert.ok(reasons(checkLinks({ distDir })).includes('external-css-resource'))
})

test('diagram fixtures reject missing pairs, unsafe SVG and invalid provenance', async t => {
  const directory = await fixture(t)
  await cp(path.join(website, 'public'), path.join(directory, 'public'), { recursive: true })
  await cp(path.join(website, 'diagrams'), path.join(directory, 'diagrams'), { recursive: true })
  assert.deepEqual(checkDiagrams({ root: directory }), [])
  const file = path.join(directory, 'public', 'diagrams', 'zh', 'execution-flow.svg')
  const original = await readFile(file, 'utf8')
  await unlink(file)
  assert.ok(reasons(checkDiagrams({ root: directory })).includes('missing-file'))
  for (const [text, reason] of [
    [original.replace(/data-source-commit="[^"]+"/, ''), 'missing-provenance'],
    [original.replace(/data-source-commit="[^"]+"/, 'data-source-commit="not-a-commit"'), 'invalid-provenance'],
    [original.replace('</svg>', '<image href="https://cdn.invalid/a.svg"/></svg>'), 'external-or-script'],
    [original.replace('</svg>', '<foreignObject><p>Unsafe</p></foreignObject></svg>'), 'external-or-script'],
    [original.replace(/viewBox="[^"]+"/, 'viewBox="0 0 1 1"'), 'viewbox-mismatch'],
    [original.replace(/<title[^>]*>[\s\S]*?<\/title>/, ''), 'missing-metadata'],
    [original.replace(/limit_reached/g, 'removed'), 'desc-missing-limit_reached'],
    [original.replace(/<desc[^>]*>[\s\S]*?<\/desc>/, '<desc>completed limit_reached cancelled failed</desc>'), 'desc-missing-recovery']
  ]) {
    await writeFile(file, text)
    assert.ok(reasons(checkDiagrams({ root: directory })).includes(reason), reason)
  }
  await writeFile(file, original)
  assert.ok(reasons(checkDiagrams({ root: directory, diagrams: DIAGRAMS.slice(1) })).includes('missing-diagram'))
  assert.ok(reasons(checkDiagrams({ root: directory, diagrams: [...DIAGRAMS, DIAGRAMS[0]] })).includes('duplicate-diagram-id'))
  const source = path.join(directory, 'diagrams', 'basalt-diagrams.drawio')
  const sourceText = await readFile(source, 'utf8')
  await writeFile(source, sourceText.replace(/data-source-commit="[^"]+"/, ''))
  assert.ok(reasons(checkDiagrams({ root: directory })).includes('missing-source-provenance'))
  await writeFile(source, sourceText.replace(/data-source-commit="[^"]+"/, 'data-source-commit="not-a-commit"'))
  assert.ok(reasons(checkDiagrams({ root: directory })).includes('invalid-source-provenance'))
  await writeFile(source, '<mxfile><diagram name="system-architecture-en"/></mxfile>')
  assert.ok(reasons(checkDiagrams({ root: directory })).includes('page-count-1'))
})

test('content negative fixtures exercise the same production rules', async () => {
  const original = await loadContentFiles()
  assert.deepEqual(checkPublicContent(original), [])
  const missing = { ...original }
  delete missing['website/index.md']
  assert.ok(reasons(checkPublicContent(missing)).includes('missing-file'))
  for (const [file, change, reason] of [
    ['README.md', value => value.replace('basalt init', 'basalt status'), 'quickstart command sequence drifted'],
    ['README.md', value => value + '\n![Detailed flow](website/public/diagrams/en/execution-flow.svg)\n', 'readme-diagram-scope'],
    ['website/quickstart/index.md', value => value.replace('basalt chat --agent', 'basalt chat --profile'), 'invalid-cli'],
    ['website/index.md', value => value + '\n[Private](https://github.com/ymedword/basalt/blob/main/docs/Requirements.md)\n', 'private-path'],
    ['website/index.md', value => value + '\nReady in 5 minutes.\n', 'unmeasured-duration'],
    ['website/index.md', value => value + '\n已验证\n', 'unverified-claim'],
    ['website/architecture/index.md', value => value.replace('## ', 'Supports distributed execution.\n\n## '), 'future-capability-claim'],
    ['website/demo/transcript.json', value => JSON.stringify({ ...JSON.parse(value), lines: ['Invented reply'] }), 'reply-drift']
  ]) {
    const findings = checkPublicContent({ ...original, [file]: change(original[file]) })
    assert.ok(reasons(findings).some(value => value.toLowerCase().includes(reason.toLowerCase())), `${file}: ${JSON.stringify(findings)}`)
  }
})

function onSection(text) {
  const match = text.match(/^on:[ \t]*(.*)$/m)
  assert.ok(match, 'workflow must declare on:')
  const inline = match[1].replace(/#.*$/, '').trim()
  if (inline !== '') return inline
  const rest = text.slice(match.index + match[0].length)
  const end = rest.search(/^[A-Za-z0-9_-]+:/m)
  return (end < 0 ? rest : rest.slice(0, end)).trim()
}
function jobBlock(text, name) {
  const start = text.search(new RegExp(`^  ${name}:[ \\t]*(?:#.*)?$`, 'm'))
  assert.ok(start >= 0, `job ${name} must exist`)
  const rest = text.slice(start + 1)
  const end = rest.search(/^  [A-Za-z0-9_-]+:[ \t]*(?:#.*)?$/m)
  return end < 0 ? rest : rest.slice(0, end)
}
function pagesTriggers(text) {
  const on = onSection(text).replace(/#.*$/gm, '')
  assert.ok(!on.startsWith('['), 'array triggers are forbidden')
  return [...on.matchAll(/^(?: {2})?([a-z_]+):/gm)].map(match => match[1])
}
function assertPagesWorkflow(text) {
  const on = onSection(text).replace(/#.*$/gm, '')
  assert.deepEqual(pagesTriggers(text), ['workflow_run', 'workflow_dispatch'], 'workflow_run and workflow_dispatch are the only triggers')
  assert.match(on, /^ {0,2}workflow_run:\n {4}workflows: \["CI"\]\n {4}types: \[completed\]\n {4}branches: \[main\]$/m)
  assert.match(on, /^ {2}workflow_dispatch:$/m)
  assert.ok(!/secrets\./.test(text), 'no secrets references')
  assert.match(text, /actions\/deploy-pages@/)
  const deploy = jobBlock(text, 'deploy').replace(/#.*$/gm, '').replace(/\s+/g, ' ')
  assert.match(deploy, /github\.event_name == 'workflow_dispatch'/)
  assert.match(deploy, /github\.event\.workflow_run\.conclusion == 'success'/)
  assert.match(deploy, /github\.event\.workflow_run\.head_sha == github\.sha/)
  assert.match(deploy, /ref: \$\{\{ github\.event_name == 'workflow_run' && github\.event\.workflow_run\.head_sha \|\| github\.sha \}\}/)
}
test('Pages workflow publishes only after successful CI on main and keeps manual dispatch', async () => {
  const text = await readFile(path.join(root, '.github', 'workflows', 'pages.yml'), 'utf8')
  assertPagesWorkflow(text)
  for (const trigger of ['push', 'pull_request', 'schedule', 'release']) assert.throws(() => assertPagesWorkflow(text.replace('  workflow_dispatch:', `  ${trigger}:\n  workflow_dispatch:`)))
  assert.throws(() => assertPagesWorkflow(text.replace('workflows: ["CI"]', 'workflows: ["Website"]')))
  assert.throws(() => assertPagesWorkflow(text.replace('types: [completed]', 'types: [requested]')))
  assert.throws(() => assertPagesWorkflow(text.replace('branches: [main]', 'branches: [develop]')))
  assert.throws(() => assertPagesWorkflow(text.replace("github.event.workflow_run.conclusion == 'success' &&", '')))
  assert.throws(() => assertPagesWorkflow(text.replace('github.event.workflow_run.head_sha == github.sha', 'true')))
  assert.throws(() => assertPagesWorkflow(text.replace("github.event_name == 'workflow_dispatch' ||", '')))
  assert.throws(() => assertPagesWorkflow(text.replace(/on:\s*\n  workflow_run:/, 'on: [push]\n  workflow_run:')))
  assert.throws(() => assertPagesWorkflow(text + '\n# ${{ secrets.TOKEN }}\n'))
  for (const pattern of [/contents:\s*read/, /pages:\s*write/, /id-token:\s*write/, /name:\s*github-pages/, /actions\/configure-pages@/, /actions\/upload-pages-artifact@/, /path:\s*website\/\.vitepress\/dist/]) assert.match(text, pattern)
})
test('CI isolates the website build and keeps complete history for provenance', async () => {
  const text = await readFile(path.join(root, '.github', 'workflows', 'ci.yml'), 'utf8')
  const on = onSection(text).replace(/#.*$/gm, '')
  assert.match(text, /^name: CI$/m)
  assert.match(on, /^(?: {2})?push:/m)
  assert.match(on, /^(?: {2})?pull_request:/m)
  const job = jobBlock(text, 'website').replace(/#.*$/gm, '')
  assert.ok(!/deploy-pages/.test(text))
  for (const command of ['npm ci', 'npm test', 'npm run build', 'npm run check']) assert.ok(job.includes(command), command)
  for (const pattern of [/actions\/checkout@/, /fetch-depth:\s*0/, /actions\/setup-node@/, /node-version-file:\s*website\/\.nvmrc/, /actions\/upload-artifact@/, /name:\s*website-dist/]) assert.match(job, pattern)
  for (const name of ['test', 'build']) assert.ok(!/\bnpm\b|setup-node/.test(jobBlock(text, name).replace(/#.*$/gm, '')), `Go job ${name} does not call Node`)
})
test('scope checker accepts only approved changes and checks both sides of renames', () => {
  assert.deepEqual(checkScope('?? website/index.md\n M docs/Operations.md\n?? specs/013-website-readme/tasks.md\n?? tmp/fixture.txt\n'), [])
  assert.deepEqual(checkScope(' M internal/core/loop.go\n?? website-evil/index.md\n'), ['internal/core/loop.go', 'website-evil/index.md'])
  assert.deepEqual(checkScope(' R internal/core/loop.go -> website/loop.go\n'), ['internal/core/loop.go'])
  assert.deepEqual(ALLOWLIST, ['README.md', 'README.zh-CN.md', 'website/', '.gitignore', '.github/workflows/ci.yml', '.github/workflows/pages.yml', 'internal/cli/public_docs_test.go', 'docs/Operations.md', 'specs/013-', 'harness/stages/us13-website-readme.json', 'tmp/'])
})
