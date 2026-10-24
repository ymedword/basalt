// 操作审计视图：管理面高权限变更的固定窗口列表与详情（FR6 / FR7）。
// 只读；不提供导出或企业级检索（扩展阶段）。
// Operations audit view: the fixed-window list and detail of management-side
// changes. Read-only; export and enterprise search stay out of this phase.
import { defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import type { OperationDetail, OperationStatus, OperationTargetKind } from '../api'
import { CodeBlock, DataTable, Notice, formatTimestamp } from '../components/ui'
import {
  OPERATION_STATUS_LABELS,
  badge,
  definitions,
  identifier,
  pageHeading,
  operationStatusTone,
  section,
  useLoader,
  viewProps,
} from './shared'

const TARGET_KINDS: readonly OperationTargetKind[] = [
  'agent',
  'schedule',
  'model_connection',
  'mcp_server',
  'skill',
  'notify_channel',
  'network_allowlist',
  'system',
]

const TARGET_KIND_LABELS: Record<OperationTargetKind, string> = {
  agent: 'Agent',
  schedule: '定时任务',
  model_connection: '模型连接',
  mcp_server: 'MCP 服务',
  skill: '技能',
  notify_channel: '通知渠道',
  network_allowlist: '网络白名单',
  system: '系统',
}

const STATUSES: readonly OperationStatus[] = ['pending', 'success', 'failed', 'unknown']

const VERIFICATION_LABELS: Record<OperationDetail['verification'], string> = {
  not_required: '无需查证',
  applied: '已应用',
  not_applied: '未应用',
  indeterminate: '无法确定',
}

function normalizeTargetKind(raw: string): OperationTargetKind | '' {
  return (TARGET_KINDS as readonly string[]).includes(raw) ? (raw as OperationTargetKind) : ''
}

function normalizeStatus(raw: string): OperationStatus | '' {
  return (STATUSES as readonly string[]).includes(raw) ? (raw as OperationStatus) : ''
}

function normalizeLimit(raw: string): number {
  const parsed = Number.parseInt(raw, 10)
  if (Number.isNaN(parsed) || parsed < 1) {
    return 50
  }
  return Math.min(parsed, 200)
}

export const OperationsView = defineComponent({
  name: 'OperationsView',
  props: viewProps,
  setup(props) {
    const api = props.ctx.api
    const action = ref('')
    const targetKind = ref<OperationTargetKind | ''>('')
    const status = ref<OperationStatus | ''>('')
    const limit = ref(50)

    const operations = useLoader(() =>
      api.listOperations({
        action: action.value.trim() === '' ? undefined : action.value.trim(),
        target_kind: targetKind.value === '' ? undefined : targetKind.value,
        status: status.value === '' ? undefined : status.value,
        limit: limit.value,
      }),
    )

    const selected = ref<OperationDetail | null>(null)
    const detailError = ref('')
    const detailLoading = ref(false)

    async function openDetail(operationId: string): Promise<void> {
      detailLoading.value = true
      detailError.value = ''
      try {
        selected.value = await api.getOperation(operationId)
      } catch (caught) {
        detailError.value = errorMessage(caught)
        selected.value = null
      } finally {
        detailLoading.value = false
      }
    }

    function apply(event: Event): void {
      event.preventDefault()
      void operations.reload()
    }

    function renderDetail(): VNodeChild {
      const detail = selected.value
      if (detail === null) {
        return null
      }
      const content: VNodeChild[] = [
        h('p', { class: 'detail-identity' }, [h('code', { class: 'detail-id', title: detail.operation_id }, detail.operation_id)]),
        definitions([
          ['动作', h('code', detail.action)],
          ['目标', `${TARGET_KIND_LABELS[detail.target_kind]} · ${detail.target_id}`],
          ['主体', detail.actor_subject],
          ['认证方式', detail.auth_method],
          ['来源', detail.source === '' ? '—' : detail.source],
          ['状态', badge(OPERATION_STATUS_LABELS[detail.status], operationStatusTone(detail.status))],
          ['结果码', detail.result_code === '' ? '—' : detail.result_code],
          ['错误信息', detail.error_message === '' ? '—' : detail.error_message],
          ['创建时间', formatTimestamp(detail.created_at)],
          ['结束时间', formatTimestamp(detail.finished_at)],
          ['请求哈希', h('code', detail.request_hash)],
          ['期望版本', String(detail.requested_version_id)],
          ['期望 ETag', detail.expected_etag === '' ? '—' : h('code', detail.expected_etag)],
          ['查证结论', VERIFICATION_LABELS[detail.verification]],
          ['恢复时间', formatTimestamp(detail.recovered_at)],
        ]),
        h('h3', '请求详情'),
        h(CodeBlock, { text: JSON.stringify(detail.detail, null, 2) }),
      ]
      return section('操作详情', content)
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        h(
          'form',
          { class: 'filters', onSubmit: apply },
          [
            h('label', ['动作', h('input', {
              type: 'text',
              value: action.value,
              placeholder: '如 agent.disable',
              onInput: (event: Event) => {
                action.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['目标类型', h(
              'select',
              {
                value: targetKind.value,
                onChange: (event: Event) => {
                  targetKind.value = normalizeTargetKind((event.target as HTMLSelectElement).value)
                },
              },
              [
                h('option', { value: '' }, '全部'),
                ...TARGET_KINDS.map((kind) =>
                  h('option', { key: kind, value: kind }, TARGET_KIND_LABELS[kind]),
                ),
              ],
            )]),
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
                ...STATUSES.map((item) =>
                  h('option', { key: item, value: item }, OPERATION_STATUS_LABELS[item]),
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
      ]

      if (operations.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: operations.error.value }))
      }
      if (operations.loading.value && operations.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      const data = operations.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            tableClass: 'operations-table',
            columnClasses: ['col-id', 'col-action-name', 'col-target', 'col-name', 'col-short', 'col-state', 'col-time', 'col-action'],
            headers: ['操作', '动作', '目标', '主体', '方式', '状态', '创建时间', ''],
            rows: data.map((item) => ({
              key: item.operation_id,
              cells: [
                identifier(item.operation_id),
                identifier(item.action),
                identifier(`${TARGET_KIND_LABELS[item.target_kind]} · ${item.target_id}`),
                identifier(item.actor_subject),
                item.auth_method,
                badge(OPERATION_STATUS_LABELS[item.status], operationStatusTone(item.status)),
                formatTimestamp(item.created_at),
                h(
                  'button',
                  {
                    type: 'button',
                    onClick: () => {
                      void openDetail(item.operation_id)
                    },
                  },
                  '查看',
                ),
              ],
            })),
            empty: '没有符合条件的操作记录',
          }),
        )
      }

      if (detailError.value !== '') {
        content.push(h(Notice, { tone: 'error', text: detailError.value }))
      }
      if (detailLoading.value) {
        content.push(h('p', { class: 'muted' }, '详情加载中…'))
      }
      content.push(renderDetail())

      return h('section', { class: 'view view-operations' }, [pageHeading('SYSTEM / 09', '管理操作', '追溯管理面的配置变更、操作者与执行结果。'), ...content])
    }
  },
})
