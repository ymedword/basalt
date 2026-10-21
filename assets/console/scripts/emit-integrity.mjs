// 生成 dist/integrity.json：dist 内每个文件（清单自身除外）→ sha256。
// 路径一律 POSIX 相对 dist，按字典序排列；不写时间戳，保证同一输入产出逐字节相同。
// Emits dist/integrity.json mapping every bundled file (except the manifest itself)
// to its sha256; paths are POSIX-relative to dist and sorted, with no timestamps.
import { createHash } from 'node:crypto'
import { readdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const consoleRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const distDir = join(consoleRoot, 'dist')
const manifestName = 'integrity.json'

async function walk(dir) {
  const entries = await readdir(dir, { withFileTypes: true })
  const files = []
  for (const entry of entries) {
    const full = join(dir, entry.name)
    if (entry.isDirectory()) {
      files.push(...(await walk(full)))
    } else {
      files.push(full)
    }
  }
  return files
}

const paths = (await walk(distDir))
  .map((full) => relative(distDir, full).split(sep).join('/'))
  .filter((rel) => rel !== manifestName)
  .sort()

const manifest = {}
for (const rel of paths) {
  const digest = createHash('sha256').update(await readFile(join(distDir, rel))).digest('hex')
  manifest[rel] = digest
}

await writeFile(join(distDir, manifestName), `${JSON.stringify(manifest, null, 2)}\n`, 'utf8')
console.log(`integrity.json: ${paths.length} 个文件`)
