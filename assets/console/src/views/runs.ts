// 运行列表视图：三个契约过滤条件 + 窗口上限 200，排序完全由服务端固定（FR12）。
// Run list with the three contract filters; ordering stays server-side and fixed.
import { defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type { RunStatus } from '../api'
import { DataTable, Notice, formatTimestamp } from '../components/ui'
import { runHref } from '../router'
import {
  RUN_STATUS_LABELS,
  badge,
  identifier,
  pageHeading,
  runStatusTone,
  useLoader,
  viewProps,
} from './shared'

const STATUS_OPTIONS: readonly RunStatus[] = [
  'running',
  'completed',
  'limit_reached',
  'cancelled',
  'failed',
]

/** 窗口归一：缺省 50、上限 200，非法输入按缺省处理。 */
function normalizeLimit(raw: string | null): number {
  if (raw === null || raw.trim() === '') {
    return 50
  }
  const parsed = Number.parseInt(raw, 10)
  if (Number.isNaN(parsed) || parsed < 1) {
    return 50
  }
  return Math.min(parsed, 200)
}

function normalizeStatus(raw: string | null): RunStatus | '' {
  if (raw === null) {
    return ''
  }
  return (STATUS_OPTIONS as readonly string[]).includes(raw) ? (raw as RunStatus) : ''
}

export const RunsView = defineComponent({
  name: 'RunsView',
  props: viewProps,
  setup(props) {
    const api = props.ctx.api
    const agent = ref(props.query.get('agent') ?? '')
    const sessionId = ref(props.query.get('session_id') ?? '')
    const status = ref<RunStatus | ''>(normalizeStatus(props.query.get('status')))
    const limit = ref(normalizeLimit(props.query.get('limit')))

    const runs = useLoader(() =>
      api.listRuns({
        agent: agent.value.trim() === '' ? undefined : agent.value.trim(),
        session_id: sessionId.value.trim() === '' ? undefined : sessionId.value.trim(),
        status: status.value === '' ? undefined : status.value,
        limit: limit.value,
      }),
    )

    function apply(event: Event): void {
      event.preventDefault()
      limit.value = normalizeLimit(String(limit.value))
      void runs.reload()
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        h(
          'form',
          { class: 'filters', onSubmit: apply },
          [
            h('label', ['Agent', h('input', {
              type: 'text',
              value: agent.value,
              onInput: (event: Event) => {
                agent.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['会话', h('input', {
              type: 'text',
              value: sessionId.value,
              onInput: (event: Event) => {
                sessionId.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['状态', h(
              'select',
              {
                value: status.value,
                onChange: (event: Event) => {
                  status.value = normalizeStatus((event.target as HTMLSelectElement).value)
                },
              },
              [
                h('option', { value: '' }, '全部'),
                ...STATUS_OPTIONS.map((option) =>
                  h('option', { key: option, value: option }, RUN_STATUS_LABELS[option]),
                ),
              ],
            )]),
            h('label', ['窗口', h('input', {
              type: 'number',
              min: 1,
              max: 200,
              value: String(limit.value),
              onInput: (event: Event) => {
                limit.value = normalizeLimit((event.target as HTMLInputElement).value)
              },
            })]),
            h('button', { type: 'submit' }, '查询'),
          ],
        ),
        h('p', { class: 'muted small' }, '排序由服务端固定为最近开始优先；窗口缺省 50、上限 200。'),
      ]

      if (runs.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: runs.error.value }))
      }
      if (runs.loading.value && runs.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      const data = runs.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            tableClass: 'runs-table',
            columnClasses: ['col-id', 'col-name', 'col-state', 'col-short', 'col-number', 'col-number', 'col-time', 'col-time'],
            headers: ['运行', 'Agent', '状态', '触发', '迭代', '工具调用', '开始时间', '结束时间'],
            rows: data.map((run) => ({
              key: run.run_id,
              cells: [
                identifier(run.run_id, runHref(run.run_id)),
                identifier(run.agent),
                badge(RUN_STATUS_LABELS[run.status], runStatusTone(run.status)),
                run.trigger,
                String(run.iterations),
                String(run.tool_calls),
                formatTimestamp(run.started_at),
                formatTimestamp(run.finished_at),
              ],
            })),
            empty: '没有符合条件的运行',
          }),
        )
      }

      return h('section', { class: 'view view-runs' }, [pageHeading('WORKSPACE / RUNS', '运行列表', '跨 Agent 查看执行结果，按会话与状态定位一次运行。'), ...content])
    }
  },
})
