// 通用展示组件：状态徽标、提示条、表格、代码块、确认对话框与读数格式化。
// 所有文本都走 Vue 的文本节点转义；本文件不解析 HTML、不拼字符串模板。
// Shared presentational components: badges, notices, tables, code blocks and a
// confirm dialog. Every value is rendered as a Vue text node.
import { defineComponent, h } from 'vue'
import type { PropType, VNode, VNodeChild } from 'vue'

/** 徽标色调：正常 / 警示 / 失败 / 中性。 */
export type Tone = 'ok' | 'warn' | 'bad' | 'muted'

/** 状态徽标：纯文本 + 色调类名。 */
export const StatusBadge = defineComponent({
  name: 'StatusBadge',
  props: {
    label: { type: String, required: true },
    tone: { type: String as PropType<Tone>, default: 'muted' },
  },
  setup(props) {
    return (): VNode => h('span', { class: ['badge', `badge-${props.tone}`] }, props.label)
  },
})

/** 提示条：info / warn / error 三档；error 带 role=alert 便于辅助技术感知。 */
export const Notice = defineComponent({
  name: 'Notice',
  props: {
    tone: { type: String as PropType<'info' | 'warn' | 'error'>, default: 'info' },
    text: { type: String, required: true },
  },
  setup(props) {
    return (): VNode =>
      h(
        'p',
        {
          class: ['notice', `notice-${props.tone}`],
          role: props.tone === 'error' ? 'alert' : 'status',
        },
        props.text,
      )
  },
})

/** 表格行：key 用于 diff，cells 与表头一一对应。 */
export interface TableRow {
  readonly key: string
  readonly cells: readonly VNodeChild[]
}

/**
 * 只读数据表：表头 + 行；空集合渲染 empty 文案而不是空表。
 * A read-only table that renders an explicit empty state instead of a bare header.
 */
export const DataTable = defineComponent({
  name: 'DataTable',
  props: {
    headers: { type: Array as PropType<readonly string[]>, required: true },
    rows: { type: Array as PropType<readonly TableRow[]>, required: true },
    empty: { type: String, default: '暂无数据' },
    columnClasses: { type: Array as PropType<readonly string[]>, default: () => [] },
    tableClass: { type: String, default: '' },
  },
  setup(props) {
    return (): VNode => {
      if (props.rows.length === 0) {
        return h('p', { class: 'empty' }, props.empty)
      }
      // 表格外层加滚动容器：窄屏横向浏览，页面本身不产生横向滚动。
      // The wrapper owns horizontal overflow so narrow screens scroll the table only.
      return h('div', { class: 'table-wrap' }, [
        h('table', { class: ['data-table', props.tableClass] }, [
          h(
            'thead',
            [h('tr', props.headers.map((title, index) => h('th', { key: index, scope: 'col', class: props.columnClasses[index] }, title)))],
          ),
          h(
            'tbody',
            props.rows.map((row) =>
              h(
                'tr',
                { key: row.key },
                row.cells.map((cell, index) => h('td', { key: index, class: props.columnClasses[index] }, [cell])),
              ),
            ),
          ),
        ]),
      ])
    }
  },
})

/** 代码块：原文逐字展示（编辑器回显、哈希、运行最终文本的兜底形态）。 */
export const CodeBlock = defineComponent({
  name: 'CodeBlock',
  props: {
    text: { type: String, required: true },
  },
  setup(props) {
    return (): VNode => h('pre', { class: 'code-block' }, [h('code', props.text)])
  },
})

/** 确认对话框：确认 / 取消两个出口，busy 时禁用按钮防重复提交。 */
export const ConfirmDialog = defineComponent({
  name: 'ConfirmDialog',
  props: {
    open: { type: Boolean, default: false },
    title: { type: String, required: true },
    message: { type: String, required: true },
    confirmText: { type: String, default: '确认' },
    busy: { type: Boolean, default: false },
  },
  emits: {
    confirm: () => true,
    cancel: () => true,
  },
  setup(props, { emit }) {
    return (): VNode | null => {
      if (!props.open) {
        return null
      }
      return h(
        'div',
        {
          class: 'dialog-backdrop',
          role: 'dialog',
          'aria-modal': 'true',
          'aria-label': props.title,
        },
        [
          h('div', { class: 'dialog' }, [
            h('h2', props.title),
            h('p', props.message),
            h('div', { class: 'dialog-actions' }, [
              h(
                'button',
                { type: 'button', disabled: props.busy, onClick: () => emit('cancel') },
                '取消',
              ),
              h(
                'button',
                {
                  type: 'button',
                  class: 'primary',
                  disabled: props.busy,
                  onClick: () => emit('confirm'),
                },
                props.confirmText,
              ),
            ]),
          ]),
        ],
      )
    }
  },
})

/** 页码式表格的普通链接：站内 href 由 router 拦截，不需要编程式跳转。 */
export function link(href: string, label: string): VNode {
  return h('a', { href }, label)
}

/** 时间戳格式化：空串显示破折号；无法解析时原样返回（不伪造时间）。 */
export function formatTimestamp(value: string | undefined | null): string {
  if (value === undefined || value === null || value === '') {
    return '—'
  }
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) {
    return value
  }
  const pad = (part: number): string => String(part).padStart(2, '0')
  return (
    `${parsed.getFullYear()}-${pad(parsed.getMonth() + 1)}-${pad(parsed.getDate())} ` +
    `${pad(parsed.getHours())}:${pad(parsed.getMinutes())}:${pad(parsed.getSeconds())}`
  )
}

/** 运行时长：秒 → 天 / 小时 / 分钟；为负数按 0 处理。 */
export function formatUptime(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds))
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  if (days > 0) {
    return `${days} 天 ${hours} 小时 ${minutes} 分`
  }
  if (hours > 0) {
    return `${hours} 小时 ${minutes} 分`
  }
  return `${minutes} 分`
}

/** 字节数 → MB（保留一位小数）；null 表示平台不可采，显示"不可用"而非 0。 */
export function formatBytes(value: number | null): string {
  if (value === null) {
    return '不可用'
  }
  return `${(value / (1024 * 1024)).toFixed(1)} MB`
}

/** 哈希缩略：只展示前 12 位，完整值放 title 由浏览器提示。 */
export function shortHash(value: string): string {
  if (value.length <= 12) {
    return value
  }
  return `${value.slice(0, 12)}…`
}
