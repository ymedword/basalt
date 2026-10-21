// 404 视图：未知客户端路径渲染明确的未找到页，不白屏（约束 41）。
// The client-side 404 view for unknown console paths.
import { defineComponent, h } from 'vue'
import type { VNode } from 'vue'
import { pageHeading, viewProps } from './shared'

export const NotFoundView = defineComponent({
  name: 'NotFoundView',
  props: viewProps,
  setup() {
    return (): VNode =>
      h('section', { class: 'view view-not-found' }, [
        pageHeading('WORKSPACE / 404', '页面不存在', '这个地址没有对应的管理台页面。请检查链接，或从系统总览重新进入。'),
        h('div', { class: 'not-found-panel' }, [
          h('span', { class: 'not-found-number', 'aria-hidden': 'true' }, '404'),
          h('div', [h('p', '工作区仍在原处。'), h('a', { class: 'text-action', href: '/console/' }, '返回系统总览 →')]),
        ]),
      ])
  },
})
