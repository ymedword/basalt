// 原创柱状节理图形：仅使用本地矢量与 Vue 节点，不加载外部图像或字体。
// Original columnar-basalt artwork rendered as local SVG nodes.
import { h } from 'vue'
import type { VNode } from 'vue'

export function brandMark(): VNode {
  return h('svg', { viewBox: '0 0 32 34', class: 'brand-mark', 'aria-hidden': 'true' }, [
    h('path', { d: 'M2 12 8 8 14 12v19H2Z', fill: 'currentColor' }),
    h('path', { d: 'm12 5 7-4 7 4v26H12Z', fill: 'currentColor' }),
    h('path', { d: 'm23 14 5-3 4 3v17h-9Z', fill: 'currentColor', opacity: '.6' }),
    h('path', { d: 'm12 5 7 4 7-4M19 9v22M2 12l6 4 6-4M8 16v15', fill: 'none', stroke: 'var(--surface)', 'stroke-width': '1' }),
  ])
}

export function basaltIllustration(): VNode {
  const columns = [
    'M34 168 72 146 111 168v109l-39 22-38-22Z',
    'M93 116 133 93 173 116v157l-40 23-40-23Z',
    'M155 65 197 41 239 65v209l-42 24-42-24Z',
    'M220 127 256 106 292 127v148l-36 21-36-21Z',
    'M277 178 309 160 341 178v97l-32 19-32-19Z',
  ]
  const tops = ['M34 168 72 190 111 168M72 190v109', 'M93 116 133 139 173 116M133 139v157', 'M155 65 197 89 239 65M197 89v209', 'M220 127 256 148 292 127M256 148v148', 'M277 178 309 197 341 178M309 197v97']
  return h('svg', { viewBox: '0 0 390 350', class: 'basalt-illustration', 'aria-hidden': 'true' }, [
    h('path', { d: 'M20 305h338M197 18v13M197 314v18M8 168h14M354 168h14', stroke: 'currentColor', 'stroke-width': '.8', opacity: '.35' }),
    ...columns.map((d) => h('path', { d, fill: 'var(--paper)', stroke: 'currentColor', 'stroke-width': '1.3' })),
    ...tops.map((d) => h('path', { d, fill: 'none', stroke: 'currentColor', 'stroke-width': '1' })),
    ...Array.from({ length: 11 }, (_, i) => h('path', { d: 'M' + (158 + i * 3.2) + ' ' + (80 + i * 1.8) + 'v183', stroke: 'currentColor', 'stroke-width': '.5', opacity: '.5' })),
    h('path', { d: 'm155 65 42-24 42 24-42 24Z', fill: 'var(--accent)', stroke: 'var(--accent)' }),
    h('path', { d: 'm155 65 42 24 42-24M197 89v209', fill: 'none', stroke: 'var(--surface)', 'stroke-width': '1' }),
    h('text', { x: '25', y: '333', fill: 'currentColor', 'font-size': '9', 'letter-spacing': '2', 'font-family': 'monospace' }, 'BASALT / COLUMNAR JOINTING'),
    h('text', { x: '273', y: '62', fill: 'var(--accent)', 'font-size': '10', 'font-family': 'monospace' }, 'FIG. 01'),
    h('path', { d: 'M269 69h-20l-13 18', fill: 'none', stroke: 'var(--accent)', 'stroke-width': '.8' }),
  ])
}
