// 调用方列表（US-16 §3.1.9 / T026a）：列出调用方并提供「建调用方」表单。
// 两条契约事实决定页面形态：`POST /callers` 是**唯一**写入限额的入口（没有改
// 限额的端点），`CallerSummary` 不回传限额——因此限额只在创建时填写，创建后
// 只能在详情页的周期用量里看到实际生效值。
// The list page and the create form. Limits are written only at creation: the
// contract has no update endpoint and CallerSummary never carries them.
import { defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import { CallersApi } from '../api-callers'
import type {
  CallerCreateRequest,
  CallerKind,
  CallerLimitPeriod,
  CallerLimitView,
  CallerSummary,
  CallerStatus,
} from '../api-callers'
import { DataTable, Notice, formatTimestamp } from '../components/ui'
import type { Tone } from '../components/ui'
import { callerHref } from '../router'
import { badge, identifier, pageHeading, useLoader, viewProps } from './shared'

/** 调用方种类文案。 */
export const CALLER_KIND_LABELS: Readonly<Record<CallerKind, string>> = {
  builtin: '内置',
  external: '外部',
}

/** 调用方状态文案与色调；停用不改历史行，只让后续认证失败。 */
export const CALLER_STATUS_LABELS: Readonly<Record<CallerStatus, { readonly label: string; readonly tone: Tone }>> = {
  active: { label: '启用', tone: 'ok' },
  disabled: { label: '停用', tone: 'muted' },
}

/** 限额草稿：表单里一律按字符串持有，提交时再判数值——输入框的原始值不丢。 */
interface LimitDraft {
  qps: string
  burst: string
  maxConcurrent: string
  tasksPerPeriod: string
  tokensPerPeriod: string
  ingestBytesPerPeriod: string
  period: CallerLimitPeriod
  timezone: string
}

/** 空限额：零值不是"不限量"，而是"该维度拒绝"——表单必须显式写出来。 */
function emptyLimitDraft(): LimitDraft {
  return {
    qps: '0',
    burst: '0',
    maxConcurrent: '0',
    tasksPerPeriod: '0',
    tokensPerPeriod: '0',
    ingestBytesPerPeriod: '0',
    period: 'day',
    timezone: 'UTC',
  }
}

/** 非负数字解析：空串按 0（与零值语义一致），非法或负数返回 null。 */
function parseCount(raw: string, integer: boolean): number | null {
  const trimmed = raw.trim()
  if (trimmed === '') {
    return 0
  }
  const parsed = Number(trimmed)
  if (!Number.isFinite(parsed) || parsed < 0) {
    return null
  }
  if (integer && !Number.isInteger(parsed)) {
    return null
  }
  return parsed
}

/** 把草稿转成线上限额；任何一项非法返回 null（调用方负责给出固定文案）。 */
function limitDraftBody(draft: LimitDraft): CallerLimitView | null {
  const qps = parseCount(draft.qps, false)
  const burst = parseCount(draft.burst, true)
  const maxConcurrent = parseCount(draft.maxConcurrent, true)
  const tasks = parseCount(draft.tasksPerPeriod, true)
  const tokens = parseCount(draft.tokensPerPeriod, true)
  const ingest = parseCount(draft.ingestBytesPerPeriod, true)
  const timezone = draft.timezone.trim()
  if (
    qps === null ||
    burst === null ||
    maxConcurrent === null ||
    tasks === null ||
    tokens === null ||
    ingest === null ||
    timezone === ''
  ) {
    return null
  }
  return {
    qps,
    burst,
    max_concurrent: maxConcurrent,
    tasks_per_period: tasks,
    tokens_per_period: tokens,
    ingest_bytes_per_period: ingest,
    period: draft.period,
    timezone,
  }
}

/** 数字输入：字符串值原样回填，不在输入过程中改写操作者的草稿。 */
function numberField(
  id: string,
  label: string,
  value: string,
  onInput: (next: string) => void,
  step = '1',
): VNode {
  return h('label', { class: 'field', for: id }, [
    h('span', {}, label),
    h('input', {
      id,
      type: 'number',
      min: 0,
      step,
      value,
      onInput: (event: Event) => {
        onInput((event.target as HTMLInputElement).value)
      },
    }),
  ])
}

/** 文本输入。 */
function textField(
  id: string,
  label: string,
  value: string,
  onInput: (next: string) => void,
  placeholder = '',
): VNode {
  return h('label', { class: 'field', for: id }, [
    h('span', {}, label),
    h('input', {
      id,
      type: 'text',
      value,
      placeholder,
      spellcheck: false,
      onInput: (event: Event) => {
        onInput((event.target as HTMLInputElement).value)
      },
    }),
  ])
}

export const CallersView = defineComponent({
  name: 'CallersView',
  props: viewProps,
  setup(props) {
    const api = new CallersApi(props.ctx.api)
    const callers = useLoader(() => api.list())

    const callerId = ref('')
    const name = ref('')
    const project = ref('')
    const withLimits = ref(true)
    const draft = ref<LimitDraft>(emptyLimitDraft())
    const creating = ref(false)
    const formError = ref('')
    const created = ref<CallerSummary | null>(null)

    function patchDraft(patch: Partial<LimitDraft>): void {
      draft.value = { ...draft.value, ...patch }
    }

    async function create(event: Event): Promise<void> {
      event.preventDefault()
      formError.value = ''
      created.value = null
      const trimmedName = name.value.trim()
      if (trimmedName === '') {
        formError.value = '调用方名称不得为空。'
        return
      }
      const body: CallerCreateRequest = { name: trimmedName }
      const explicitId = callerId.value.trim()
      if (explicitId !== '') {
        body.caller_id = explicitId
      }
      const trimmedProject = project.value.trim()
      if (trimmedProject !== '') {
        body.project = trimmedProject
      }
      if (withLimits.value) {
        const limits = limitDraftBody(draft.value)
        if (limits === null) {
          formError.value = '限额必须是非负数；整数项不得带小数，时区不得为空。'
          return
        }
        body.limits = limits
      }
      creating.value = true
      try {
        created.value = await api.create(body)
        name.value = ''
        callerId.value = ''
        project.value = ''
        await callers.reload()
      } catch (caught) {
        formError.value = errorMessage(caught)
      } finally {
        creating.value = false
      }
    }

    function limitFields(): VNode {
      const current = draft.value
      return h('fieldset', { class: 'form-section' }, [
        h('legend', {}, '限额（只在创建时写入）'),
        numberField('caller-limit-qps', '每秒请求（QPS，可小数）', current.qps, (next) => {
          patchDraft({ qps: next })
        }, '0.5'),
        numberField('caller-limit-burst', '突发请求数', current.burst, (next) => {
          patchDraft({ burst: next })
        }),
        // 三条零值语义各不相同，必须逐列写清：写反了要么白配、要么把调用方整个拒掉。
        h(
          'p',
          { class: 'field-hint muted' },
          'QPS / 突发：0 = 不覆盖，沿用实例入口预算（callers.ingress.*）；填了只会收紧' +
            '（生效值取类别预算与这里较小者），不会放宽实例上限。',
        ),
        numberField('caller-limit-max-concurrent', '并发 Run 名额', current.maxConcurrent, (next) => {
          patchDraft({ maxConcurrent: next })
        }),
        h(
          'p',
          { class: 'field-hint muted' },
          '并发 Run 名额：0 = 用实例上限（server.max_concurrent_runs），不是"禁止"；填了就按这个数限制在跑 Run。',
        ),
        numberField('caller-limit-tasks', '每周期任务数', current.tasksPerPeriod, (next) => {
          patchDraft({ tasksPerPeriod: next })
        }),
        numberField('caller-limit-tokens', '每周期 tokens', current.tokensPerPeriod, (next) => {
          patchDraft({ tokensPerPeriod: next })
        }),
        numberField('caller-limit-ingest', '每周期摄取字节', current.ingestBytesPerPeriod, (next) => {
          patchDraft({ ingestBytesPerPeriod: next })
        }),
        h(
          'p',
          { class: 'field-hint muted' },
          '周期额度（任务数 / tokens / 摄取字节）：0 = 该维度拒绝，不是不限量；账本缺失同样按拒绝处理。',
        ),
        h('label', { class: 'field', for: 'caller-limit-period' }, [
          h('span', {}, '周期'),
          h(
            'select',
            {
              id: 'caller-limit-period',
              value: current.period,
              onChange: (event: Event) => {
                patchDraft({
                  period: (event.target as HTMLSelectElement).value as CallerLimitPeriod,
                })
              },
            },
            [
              h('option', { value: 'day' }, '按日'),
              h('option', { value: 'month' }, '按月'),
            ],
          ),
        ]),
        textField('caller-limit-timezone', '周期时区（IANA 名称）', current.timezone, (next) => {
          patchDraft({ timezone: next })
        }, 'UTC'),
        h('p', { class: 'field-hint muted' }, '周期按该时区本地 00:00 起算；时区写错会让配额在错误的时刻重置。'),
      ])
    }

    function createForm(): VNode {
      const fields: VNodeChild[] = [
        textField('caller-name', '名称（必填）', name.value, (next) => {
          name.value = next
        }, '例如 日报接入'),
        textField('caller-id', '调用方标识（留空由服务端生成）', callerId.value, (next) => {
          callerId.value = next
        }, 'cal_ + 26 位 Crockford Base32'),
        textField('caller-project', '所属项目（可选）', project.value, (next) => {
          project.value = next
        }, '例如 ops'),
        h('label', { class: 'check-row', for: 'caller-write-limits' }, [
          h('input', {
            id: 'caller-write-limits',
            type: 'checkbox',
            checked: withLimits.value,
            onChange: (event: Event) => {
              withLimits.value = (event.target as HTMLInputElement).checked
            },
          }),
          h('span', {}, '创建时写入限额行'),
        ]),
      ]
      if (withLimits.value) {
        fields.push(limitFields())
        fields.push(h('p', { class: 'muted small' }, '契约没有修改限额的端点：限额只在这里写入一次。'))
      } else {
        fields.push(
          h(Notice, {
            tone: 'warn',
            text:
              '不写入限额行时：周期额度（任务数 / tokens / 摄取字节）按 0 处理，相关请求会被拒绝；' +
              'QPS 仍按实例入口预算放行，并发名额仍用实例上限。' +
              '契约没有修改限额的端点，之后只能重建调用方。',
          }),
        )
      }
      fields.push(
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            { id: 'caller-create', type: 'submit', class: 'primary', disabled: creating.value },
            creating.value ? '创建中…' : '创建调用方',
          ),
        ]),
      )
      const children: VNodeChild[] = [
        h('h2', {}, '建调用方'),
        h('form', { class: 'caller-create-form', onSubmit: (event: Event) => { void create(event) } }, fields),
      ]
      if (formError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: formError.value }))
      }
      if (created.value !== null) {
        children.push(
          h(Notice, {
            tone: 'info',
            text: `已创建调用方：${created.value.caller_id}（${created.value.name}）。下一步：签发 Key 并把明文交给接入方。`,
          }),
          h('p', {}, ['详情：', identifier(created.value.caller_id, callerHref(created.value.caller_id))]),
        )
      }
      return h('section', { class: 'card' }, children)
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        h(
          'p',
          { class: 'muted small' },
          '调用方是业务接入身份：每个调用方有自己的 Key、scope、速率与周期配额。Key 明文只在签发响应出现一次。',
        ),
      ]
      if (callers.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: callers.error.value }))
      }
      if (callers.loading.value && callers.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      const data = callers.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            tableClass: 'callers-table',
            columnClasses: ['col-id', 'col-name', 'col-short', 'col-short', 'col-state', 'col-number', 'col-time'],
            headers: ['调用方', '名称', '项目', '种类', '状态', '未吊销 Key', '创建时间'],
            rows: data.map((item) => {
              const status = CALLER_STATUS_LABELS[item.status]
              return {
                key: item.caller_id,
                cells: [
                  identifier(item.caller_id, callerHref(item.caller_id)),
                  item.name,
                  item.project === '' ? '—' : item.project,
                  CALLER_KIND_LABELS[item.kind],
                  badge(status.label, status.tone),
                  String(item.key_count),
                  formatTimestamp(item.created_at),
                ],
              }
            }),
            empty: '还没有调用方；内置的 local-admin 由服务端登记。',
          }),
        )
      }
      content.push(createForm())
      return h('section', { class: 'view view-callers' }, [
        pageHeading(
          'SYSTEM / CALLERS',
          '调用方',
          '管理业务接入身份、Key 与周期配额；用量与拒绝原因在详情页按周期查看。',
        ),
        // 调用方是给外部系统用的：先讲清它是什么、拒绝时长什么样，再看表格。
        h(
          'p',
          { class: 'muted small' },
          '调用方是外部系统接入本实例时使用的身份：一个调用方可以签发多把 Key，同一调用方的所有 Key ' +
            '共享并发名额与周期额度，吊销单把 Key 不重置已占用的额度。',
        ),
        h(
          'p',
          { class: 'muted small' },
          '接入方被拒时看到的业务码：429 CALLER_RATE_LIMITED（入口限流，按 Retry-After 稍后重试）、' +
            '429 CALLER_CONCURRENCY_LIMITED（在跑 Run 已满，等一个运行结束）、' +
            '429 QUOTA_EXHAUSTED（周期额度用尽，等下一周期或管理员调额）。被拒请求不占额度。',
        ),
        ...content,
      ])
    }
  },
})
