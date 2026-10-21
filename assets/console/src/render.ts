// 安全子集渲染（约束 40）：只生成固定标签，模型文本一律作为文本节点交给 Vue 转义；
// **不得**做原始 HTML 注入、**不得**引入 Markdown 库。链接只允许 http / https，
// 其余按字面文本输出。渲染器不解析 HTML——`<script>`、`onerror=` 之类只会成为文字。
// The safe-subset renderer emits only the fixed tag set; every model-text character
// reaches the DOM as a Vue text node. Links are limited to http/https; everything
// else stays literal text. Raw HTML is never parsed.
import { h } from 'vue'
import type { VNode } from 'vue'

/** 固定标签子集（约束 40）——渲染器**不得**生成集合之外的标签。 */
// The fixed tag allowlist; the renderer must never emit anything else.
export const SAFE_TAGS = [
  'p',
  'h1',
  'h2',
  'h3',
  'h4',
  'ul',
  'ol',
  'li',
  'pre',
  'code',
  'strong',
  'em',
  'a',
] as const

export type SafeTag = (typeof SAFE_TAGS)[number]

/** 行内渲染结果：纯文本片段（Vue 转义）或白名单元素。 */
export type SafeInlineChild = VNode | string

const FENCE = /^\s{0,3}(`{3,}|~{3,})/
const HEADING = /^(#{1,4})\s+(.*)$/
const UNORDERED_ITEM = /^\s{0,3}[-*]\s+(.*)$/
const ORDERED_ITEM = /^\s{0,3}\d+[.)]\s+(.*)$/

/** 链接标签的嵌套上限：防止畸形输入把递归撑爆。 */
const MAX_INLINE_DEPTH = 8

/** 把模型文本渲染为块级 VNode 序列（段落 / 标题 / 列表 / 代码块）。 */
// Renders model text into block-level VNodes from the fixed subset.
export function renderSafeText(source: string): VNode[] {
  const lines = normalizeLines(source)
  const blocks: VNode[] = []
  let index = 0
  while (index < lines.length) {
    const line = lines[index] ?? ''
    if (line.trim() === '') {
      index += 1
      continue
    }

    if (FENCE.test(line)) {
      index = pushFence(blocks, lines, index)
      continue
    }

    const heading = HEADING.exec(line)
    if (heading) {
      const level = (heading[1] ?? '#').length
      blocks.push(h(`h${level}`, renderSafeInline(heading[2] ?? '')))
      index += 1
      continue
    }

    if (UNORDERED_ITEM.test(line)) {
      const taken = takeList(lines, index, UNORDERED_ITEM)
      blocks.push(h('ul', taken.items.map((children) => h('li', children))))
      index = taken.next
      continue
    }

    if (ORDERED_ITEM.test(line)) {
      const taken = takeList(lines, index, ORDERED_ITEM)
      blocks.push(h('ol', taken.items.map((children) => h('li', children))))
      index = taken.next
      continue
    }

    const paragraph: string[] = []
    while (index < lines.length) {
      const current = lines[index] ?? ''
      if (current.trim() === '') {
        break
      }
      if (paragraph.length > 0 && isBlockStart(current)) {
        break
      }
      paragraph.push(current.trim())
      index += 1
    }
    blocks.push(h('p', renderSafeInline(paragraph.join(' '))))
  }
  return blocks
}

/** 把一行文本渲染为行内 VNode 序列（表格单元格、摘要等复用）。 */
// Renders one line into inline nodes; table cells and summaries reuse it.
export function renderSafeInline(source: string): SafeInlineChild[] {
  return renderInline(source, 0)
}

/** 统一换行并切行：CRLF 与单独 CR 都按一个换行处理。 */
function normalizeLines(source: string): string[] {
  return source.replace(/\r\n?/g, '\n').split('\n')
}

/** 判断某行是否会开启一个新的块（用于结束当前段落）。 */
function isBlockStart(line: string): boolean {
  return (
    FENCE.test(line) ||
    HEADING.test(line) ||
    UNORDERED_ITEM.test(line) ||
    ORDERED_ITEM.test(line)
  )
}

/**
 * 消费一个围栏代码块：围栏行之后的原文逐字节保留（含空行），直到闭合围栏。
 * 未闭合时吞到文本结尾——不丢失内容，也不猜测结构。
 * A fence keeps its body byte-for-byte; an unclosed fence runs to the end.
 */
function pushFence(blocks: VNode[], lines: string[], start: number): number {
  const opening = FENCE.exec(lines[start] ?? '')
  const marker = opening?.[1] ?? '```'
  const closing = marker.slice(0, 3)
  const body: string[] = []
  let index = start + 1
  while (index < lines.length && !(lines[index] ?? '').trimStart().startsWith(closing)) {
    body.push(lines[index] ?? '')
    index += 1
  }
  if (index < lines.length) {
    index += 1
  }
  blocks.push(h('pre', [h('code', body.join('\n'))]))
  return index
}

/** 消费一段连续列表项；遇到非列表行即结束。 */
function takeList(
  lines: string[],
  start: number,
  pattern: RegExp,
): { items: SafeInlineChild[][]; next: number } {
  const items: SafeInlineChild[][] = []
  let index = start
  while (index < lines.length) {
    const item = pattern.exec(lines[index] ?? '')
    if (!item) {
      break
    }
    items.push(renderSafeInline(item[1] ?? ''))
    index += 1
  }
  return { items, next: index }
}

/**
 * 行内扫描：反引号代码 / 双星粗体 / 单星斜体 / 中括号链接。任何无法闭合或无法
 * 安全转换的标记都原样留在文本里，不抛错、不吞字符。
 * Inline scan for code, strong, emphasis and links; anything unparsable stays text.
 */
function renderInline(source: string, depth: number): SafeInlineChild[] {
  const nodes: SafeInlineChild[] = []
  let buffer = ''
  let index = 0
  const flush = (): void => {
    if (buffer !== '') {
      nodes.push(buffer)
      buffer = ''
    }
  }

  while (index < source.length) {
    const char = source[index] ?? ''

    if (char === '`') {
      const close = source.indexOf('`', index + 1)
      if (close > index + 1) {
        flush()
        nodes.push(h('code', source.slice(index + 1, close)))
        index = close + 1
        continue
      }
      buffer += char
      index += 1
      continue
    }

    if (char === '*') {
      const marker = source[index + 1] === '*' ? '**' : '*'
      const from = index + marker.length
      const close = source.indexOf(marker, from)
      if (close > from) {
        flush()
        const inner = source.slice(from, close)
        nodes.push(h(marker === '**' ? 'strong' : 'em', renderInline(inner, depth)))
        index = close + marker.length
        continue
      }
      buffer += marker
      index = from
      continue
    }

    if (char === '[' && depth < MAX_INLINE_DEPTH) {
      const link = parseLink(source, index)
      if (link) {
        flush()
        nodes.push(
          h(
            'a',
            { href: link.href, rel: 'noopener noreferrer', target: '_blank' },
            renderInline(link.label, depth + 1),
          ),
        )
        index = link.next
        continue
      }
    }

    buffer += char
    index += 1
  }

  flush()
  return nodes
}

interface ParsedLink {
  readonly href: string
  readonly label: string
  readonly next: number
}

/** 解析 `[label](url)`；URL 必须是 http/https，否则整段按文本保留。 */
function parseLink(source: string, start: number): ParsedLink | null {
  const labelEnd = source.indexOf(']', start + 1)
  if (labelEnd === -1 || source[labelEnd + 1] !== '(') {
    return null
  }
  const urlEnd = source.indexOf(')', labelEnd + 2)
  if (urlEnd === -1) {
    return null
  }
  const href = safeHref(source.slice(labelEnd + 2, urlEnd).trim())
  if (href === null) {
    return null
  }
  return { href, label: source.slice(start + 1, labelEnd), next: urlEnd + 1 }
}

/**
 * 只接受绝对 http / https URL；控制字符、相对路径与 `javascript:` 等方案一律拒绝。
 * Only absolute http/https URLs survive; control characters and every other scheme
 * are rejected.
 */
function safeHref(raw: string): string | null {
  if (raw === '' || /[\u0000-\u001f\u007f]/.test(raw)) {
    return null
  }
  let parsed: URL
  try {
    parsed = new URL(raw)
  } catch {
    return null
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return null
  }
  return parsed.href
}
