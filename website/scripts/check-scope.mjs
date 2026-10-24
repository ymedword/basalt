import { execFileSync } from 'node:child_process'
import { fileURLToPath, pathToFileURL } from 'node:url'
export const ALLOWLIST = ['README.md', 'README.zh-CN.md', 'website/', '.gitignore', '.github/workflows/ci.yml', '.github/workflows/pages.yml', 'internal/cli/public_docs_test.go', 'docs/Operations.md', 'specs/013-', 'harness/stages/us13-website-readme.json', 'tmp/']
const isAllowed = (name, allowlist) => allowlist.some(item => item.endsWith('/') || item.endsWith('-') ? name.startsWith(item) : name === item)
export function checkScope(input = {}) {
  const options = typeof input === 'string' ? { statusText: input } : input
  const root = options.root ?? fileURLToPath(new URL('../../', import.meta.url))
  const allowlist = options.allowlist ?? ALLOWLIST
  if (options.statusText !== undefined) {
    return options.statusText.split('\n').filter(Boolean).flatMap(line => line.slice(3).replace(/^"|"$/g, '').split(' -> ')).filter(name => !isAllowed(name, allowlist))
  }
  const status = execFileSync('git', ['status', '--porcelain=v1', '-z', '--untracked-files=all'], { cwd: root, encoding: 'utf8' }).split('\0')
  const changed = []
  for (let i = 0; i < status.length; i++) {
    if (!status[i]) continue
    const entry = status[i]
    changed.push(entry.slice(3))
    if (/[RC]/.test(entry.slice(0, 2))) changed.push(status[++i])
  }
  return changed.filter(name => !isAllowed(name, allowlist))
}
export function main() {
  const findings = checkScope()
  if (findings.length) { console.error(JSON.stringify({ reason: 'out-of-scope', paths: findings })); process.exitCode = 1 }
  else console.log('Scope: no findings')
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main()
