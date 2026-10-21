// 渲染用例：固定标签子集与三类注入载荷（script / onerror / javascript:），
// 以及链接方案限制（约束 40）。所有模型文本必须成为文本节点。
// Renderer unit tests: the fixed tag subset, the three injection payloads and the
// http/https-only link rule. Model text must always land in text nodes.
import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { describe, expect, it } from 'vitest'
import { SAFE_TAGS, renderSafeInline, renderSafeText } from '../src/render'

function mountBlocks(source: string) {
  return mount(
    defineComponent({
      setup: () => () => h('div', { class: 'blocks' }, renderSafeText(source)),
    }),
  )
}

function mountInline(source: string) {
  return mount(
    defineComponent({
      setup: () => () => h('div', { class: 'inline' }, renderSafeInline(source)),
    }),
  )
}

describe('renderSafeText', () => {
  it('renders script payloads as literal text', () => {
    // `<script>` 只能是文字，DOM 里不存在 script 元素。
    const wrapper = mountBlocks('<script>alert(1)</script>')
    expect(wrapper.element.querySelector('script')).toBeNull()
    expect(wrapper.text()).toContain('<script>alert(1)</script>')
  })

  it('renders event-handler payloads as literal text', () => {
    // `<img onerror>` 只能是文字，DOM 里不存在 img 元素。
    const wrapper = mountBlocks('<img src=x onerror="alert(1)">')
    expect(wrapper.element.querySelector('img')).toBeNull()
    expect(wrapper.text()).toContain('onerror="alert(1)"')
  })

  it('only emits tags from the fixed subset', () => {
    const wrapper = mountBlocks(
      [
        '# 标题',
        '',
        '段落 **粗体** 与 *斜体* 与 `code`。',
        '',
        '- 第一项',
        '- 第二项',
        '',
        '1. 甲',
        '2. 乙',
        '',
        '```text',
        '原样代码',
        '```',
        '',
        '<style>body{display:none}</style>',
      ].join('\n'),
    )
    const root = wrapper.element as Element
    const tags = Array.from(root.querySelectorAll('*')).map((element) =>
      element.tagName.toLowerCase(),
    )
    expect(tags.length).toBeGreaterThan(0)
    for (const tag of tags) {
      expect(SAFE_TAGS as readonly string[]).toContain(tag)
    }
    expect(wrapper.element.querySelector('h1')?.textContent).toBe('标题')
    expect(wrapper.element.querySelectorAll('ul li')).toHaveLength(2)
    expect(wrapper.element.querySelectorAll('ol li')).toHaveLength(2)
    expect(wrapper.element.querySelector('pre code')?.textContent).toBe('原样代码')
    expect(wrapper.element.querySelector('strong')?.textContent).toBe('粗体')
    expect(wrapper.element.querySelector('em')?.textContent).toBe('斜体')
  })

  it('keeps an unclosed fence running to the end of the text', () => {
    const wrapper = mountBlocks('```\n第一行\n第二行')
    expect(wrapper.element.querySelector('pre code')?.textContent).toBe('第一行\n第二行')
  })

  it('keeps unbalanced inline markers as literal text', () => {
    const wrapper = mountBlocks('**没有闭合的粗体')
    expect(wrapper.element.querySelector('strong')).toBeNull()
    expect(wrapper.text()).toContain('**没有闭合的粗体')
  })
})

describe('renderSafeInline links', () => {
  it('allows http and https links with hardened attributes', () => {
    const wrapper = mountInline('[文档](https://example.com/docs?q=1)')
    const anchor = wrapper.element.querySelector('a')
    expect(anchor).not.toBeNull()
    expect(anchor?.getAttribute('href')).toBe('https://example.com/docs?q=1')
    expect(anchor?.getAttribute('rel')).toBe('noopener noreferrer')
    expect(anchor?.getAttribute('target')).toBe('_blank')
    expect(anchor?.getAttributeNames().sort()).toEqual(['href', 'rel', 'target'])
  })

  it('rejects javascript: links and keeps the source text', () => {
    // `javascript:` 方案一律不成链接，整段保持字面文本。
    const wrapper = mountInline('[点我](javascript:alert(1))')
    expect(wrapper.element.querySelector('a')).toBeNull()
    expect(wrapper.text()).toContain('[点我](javascript:alert(1))')
  })

  it('rejects relative, data and mixed-case javascript targets', () => {
    for (const source of [
      '[rel](/etc/passwd)',
      '[data](data:text/html;base64,PHNjcmlwdD4=)',
      '[case](JaVaScRiPt:alert(1))',
    ]) {
      const wrapper = mountInline(source)
      expect(wrapper.element.querySelector('a'), source).toBeNull()
    }
  })

  it('does not turn quote payloads into extra attributes', () => {
    const wrapper = mountInline('[x](https://example.com/"onmouseover="alert(1))')
    const anchor = wrapper.element.querySelector('a')
    expect(anchor).not.toBeNull()
    expect(anchor?.getAttributeNames().sort()).toEqual(['href', 'rel', 'target'])
    expect(wrapper.element.querySelector('[onmouseover]')).toBeNull()
  })
})
