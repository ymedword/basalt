// 诊断事件页面（US-16 §3.1.9 / T098）：按关联标识与时间窗**只读**查看结构化诊断
// 事件。五条口径：
//
//  1. **五类主锚点**：`run_id` / `request_id` / `operation_id` / `ingest_job_id` /
//     `session_id` 至少给一个（契约 §8 的 400 口径）；`task_id` 只作限定符。页面在
//     发请求之前先判——明知形状非法还发出去，等于把契约拒绝当成页面错误。
//  2. **证据缺口优先于空表**：`missing`（全量采样下确实没有记录）与 `sampled_out`
//     （采样率 < 1 时无行）必须给出不同的文案；空集**不得**被渲染成"一切正常"。
//  3. **只读**：导出归 US-16A（拟）OBS，本页不渲染任何导出 / 重放 / 删除按钮。
//  4. **诊断不是权威记录**：审计以 `audit_ref` 回指为准；本页只显示指针，不复制
//     审计内容，也不从 `attrs` 重建被权限拒绝的资料标题。
//  5. **保留期是配置值**：`retention_days` 逐字标注为配置值，不是保留承诺。
//  6. **能力覆盖要如实标注**：契约有十个种类 / 五个锚点，但当前只有部分类型有生产端
//     （见 COVERED_KINDS / ANCHOR_COVERAGE）。没生产端的类型空结果不等于"没发生"，
//     页面直接标"暂无生产端"，不留给用户自己猜。
//
// The diagnostics page is read-only. One mandatory anchor, distinct copy for the two
// evidence-gap flags, pointers at the audit trail rather than copies of it.
import { defineComponent, h, onMounted, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { DiagnosticsApi } from '../api-diagnostics'
import type { DiagnosticEventView, DiagnosticKind, DiagnosticQuery } from '../api-diagnostics'
import { DataTable, Notice, formatTimestamp } from '../components/ui'
import type { TableRow } from '../components/ui'
import { runHref } from '../router'
import { isVisibilityFilter, observationReasonLabel, visibilityFilterNote } from '../observation-copy'
import { identifier, pageHeading, useLoader, viewProps } from './shared'

/**
 * 五类主锚点（契约 `oneOf` 的固定集合）；`task_id` 不在其中，只作限定符。
 * `produced=false` 表示当前缺少真实关联；Session虽不直接存事件列，仍可经runs解析。
 * 必须如实区分查询覆盖与事件直接写入，不能让用户以为"没记录就是没发生"。
 */
// The five contract anchors; produced=false marks anchors no producer currently sets.
const ANCHORS = [
  { value: 'run_id', label: '运行 ID', produced: true },
  { value: 'request_id', label: '请求 ID', produced: true },
  { value: 'operation_id', label: '管理操作 ID', produced: false },
  { value: 'ingest_job_id', label: '摄取任务 ID', produced: true },
  { value: 'session_id', label: '会话 ID', produced: true },
] as const

type AnchorValue = (typeof ANCHORS)[number]['value']

/**
 * 有生产端的种类（`internal/app/diagnostics_client.go`、`tool_diagnostics.go`、
 * `quota_diagnostics.go`、`knowledge_adapters.go`、`web/middleware_ingress_rate.go`）。
 * 其余契约种类当前没有写入方：查询它们只会得到空集，页面标注"暂无生产端"。
 */
// Kinds that actually have producers today; the rest remain contract-only.
const COVERED_KINDS: ReadonlySet<DiagnosticKind> = new Set<DiagnosticKind>([
  'retrieval',
  'llm_call',
  'tool_call',
  'permission_decision',
  'quota_decision',
  'ingest_stage',
])

/** 原因码 → 用户可读文案；键名逐字等于 `DiagnosticReasonCode`（契约注释把文案归 UI）。 */
// Contract reason codes mapped to operator-facing copy; keys are contract literals.
const REASON_LABELS: Record<string, string> = {
  vector_unavailable: '向量通道不可用（降级到关键词）',
  keyword_unavailable: '关键词通道不可用',
  rerank_timeout: '重排超时',
  permission_denied: '权限拒绝',
  budget_trimmed: '按预算裁剪',
  history_trimmed: '历史被裁剪',
  no_answer: '没有可用答案',
  citation_unresolved: '引用无法落位',
  chunker_failed: '切分失败',
  embed_failed: '向量化失败',
  index_failed: '建索引失败',
  quota_exhausted: '周期配额耗尽',
  rate_limited: '速率受限',
  key_revoked: '调用方 Key 已撤销',
  migration_verify_failed: '迁移校验失败',
  llm_call_failed: '模型调用没有拿到可用响应',
  concurrency_limited: '并发名额不足（随请求结束恢复）',
  chunk_invalidated: '资料可见性已收窄',
}

/** 判定 → 中文；四值与契约 `decision` 枚举逐字对应，空串保持空。 */
// The four contract decisions, labelled; the empty value stays empty.
const DECISION_LABELS: Record<string, string> = {
  allow: '放行',
  deny: '拒绝',
  trim: '裁剪',
  degrade: '降级',
}

/** 能力覆盖说明：把"契约有 / 生产端有"的差集摆在查询框下方。 */
// What the contract accepts versus what producers actually write.
function coverageText(): string {
  const kinds = KIND_OPTIONS.filter((kind) => COVERED_KINDS.has(kind)).map((kind) => KIND_LABELS[kind])
  const anchors = ANCHORS.filter((anchor) => anchor.produced).map((anchor) => anchor.label)
  return `当前有生产端的种类：${kinds.join(' / ')}；可关联查询的锚点：${anchors.join(' / ')}。` +
    '未列出的种类与「管理操作 ID」暂无生产端——查空不等于没发生。会话 ID 经既有运行关联解析。' +
    '请求 ID 由入口限流与准入拒绝写入，是 Run 账本建立之前定位拒绝现场的唯一锚点。'
}

/** 诊断与审计的分工：一句话说清"权威记录"和"排障数据"不是一回事。 */
// Diagnostics are sampled troubleshooting data; audit records are authoritative.
const AUDIT_NOTE =
  '诊断不等于审计：审计记录是权威凭据（调用前先写 pending、失败即拒绝发出），诊断事件可采样、可丢弃。' +
  '要证明"做过什么"看审计回指指向的记录；本页只回答"当时发生了什么"。'

/** 契约 `DiagnosticKind` 十值的中文文案；键名逐字等于契约取值。 */
// The ten contract kinds, labelled; keys are the contract values verbatim.
const KIND_LABELS: Record<DiagnosticKind, string> = {
  ingest_stage: '摄取阶段',
  index_switch: '索引切换',
  retrieval: '检索',
  assembly: '上下文装配',
  embedding: '向量化',
  llm_call: '模型调用',
  tool_call: '工具调用',
  permission_decision: '权限判定',
  quota_decision: '配额判定',
  migration: '迁移',
}

const KIND_OPTIONS = Object.keys(KIND_LABELS) as DiagnosticKind[]

/** 窗口缺省 200、上限 1000（契约）；非法输入按缺省处理，不把 400 发出去。 */
function normalizeLimit(raw: string): number {
  if (raw.trim() === '') {
    return 200
  }
  const parsed = Number.parseInt(raw, 10)
  if (Number.isNaN(parsed) || parsed < 1) {
    return 200
  }
  return Math.min(parsed, 1000)
}

/** `datetime-local` 的值按**本地时间**解释，转成契约要的 RFC3339；空串 = 该侧不限。 */
// datetime-local is local time; the contract takes RFC3339 or an open bound.
function toRFC3339(raw: string): string | undefined {
  if (raw === '') {
    return undefined
  }
  const parsed = new Date(raw)
  return Number.isNaN(parsed.getTime()) ? undefined : parsed.toISOString()
}

/** 深链回填：查询串里给了哪个锚点就用哪个（`?run_id=…` 直接落到运行范围）。 */
// A deep link seeds the form from whichever anchor it carries.
function seedAnchor(query: URLSearchParams): { kind: AnchorValue; value: string } {
  for (const anchor of ANCHORS) {
    const value = query.get(anchor.value)
    if (value !== null && value !== '') {
      return { kind: anchor.value, value }
    }
  }
  return { kind: 'run_id', value: '' }
}

/** 关联列：只列非空键；`run_id` 给深链，其余按原值展示。 */
// Only non-empty correlation keys render; run_id links to its run page.
function correlationCell(event: DiagnosticEventView): VNodeChild {
  const correlation = event.correlation
  const pairs: readonly (readonly [string, string])[] = [
    ['request', correlation.request_id],
    ['run', correlation.run_id],
    ['task', correlation.task_id],
    ['operation', correlation.operation_id],
    ['ingest', correlation.ingest_job_id],
  ]
  const present = pairs.filter(([, value]) => value !== '')
  if (present.length === 0) {
    return h('span', { class: 'muted small' }, '—')
  }
  return h(
    'div',
    { class: 'muted small' },
    present.flatMap(([label, value], index) => [
      index === 0 ? null : h('br'),
      `${label}=`,
      label === 'run' ? identifier(value, runHref(value)) : h('code', { title: value }, value),
    ]),
  )
}

/** 位置列：步骤 / 重试 / 父步骤 + `seq`（只做局部有序，不是全局序号）。 */
// The local ordering triple; seq is local, never a global sequence.
function positionCell(event: DiagnosticEventView): VNodeChild {
  const parts: VNodeChild[] = [`seq ${event.seq}`]
  if (event.step_id !== '') {
    parts.push(h('br'), `step ${event.step_id}`)
  }
  if (event.attempt_id !== '') {
    parts.push(h('br'), `attempt ${event.attempt_id}`)
  }
  if (event.parent_step_id !== '') {
    parts.push(h('br'), `parent ${event.parent_step_id}`)
  }
  return h('div', { class: 'muted small' }, parts)
}

/**
 * 决策与原因列：两个独立字段，空值显示破折号（不写 0、不臆造）。
 * 契约注释把面向人的文案交给 UI：原因码查表翻译，原始值留在 title 里便于对日志；
 * 表外的码原样显示而不是硬编成"其他"——契约新增取值时页面不会假装认识它。
 */
// Decision and reason keep their own columns' semantics; the raw code stays in title.
function verdictCell(event: DiagnosticEventView): VNodeChild {
  const decisionRaw = event.decision
  const decisionText =
    decisionRaw === ''
      ? '决策 —'
      : `决策 ${DECISION_LABELS[decisionRaw] ?? decisionRaw}`
  const reasonRaw = event.reason_code
  const reasonText = reasonRaw === '' ? '原因 —' : `原因 ${observationReasonLabel(event.kind, reasonRaw, REASON_LABELS[reasonRaw] ?? reasonRaw)}`
  return h('div', { class: 'muted small' }, [
    h('span', { title: decisionRaw }, decisionText),
    h('br'),
    h('span', { title: reasonRaw }, reasonText),
    isVisibilityFilter(event.kind, reasonRaw) ? h('p', visibilityFilterNote(event.attrs.count)) : null,
  ])
}

/** 审计回指列：只显示指针（表名 + 主键），不复制权威记录。 */
// A pointer at the authoritative record; the record itself is never copied here.
function auditRefCell(event: DiagnosticEventView): VNodeChild {
  const ref = event.audit_ref
  if (ref === undefined) {
    return h('span', { class: 'muted small' }, '—')
  }
  return h('div', { class: 'muted small' }, [
    h('code', { title: `${ref.table}/${ref.id}` }, ref.table),
    h('br'),
    h('code', { title: ref.id }, ref.id),
  ])
}

/** 属性列：默认折叠；服务端已脱敏，页面只原样展示。 */
// Attrs arrive redacted; the page shows them verbatim and collapsed.
function attrsCell(event: DiagnosticEventView): VNodeChild {
  const keys = Object.keys(event.attrs)
  if (keys.length === 0) {
    return h('span', { class: 'muted small' }, '—')
  }
  return h('details', { class: 'diagnostic-attrs' }, [
    h('summary', `属性（${keys.length}）`),
    h('pre', { class: 'attrs-json' }, JSON.stringify(event.attrs, null, 2)),
  ])
}

/** 表格：列为 时间 / 种类 / 事件 / 决策与原因 / 位置 / 关联 / 审计回指 / 属性。 */
// Every contract field the card asks for has a column; nothing is merged away.
function eventTable(events: readonly DiagnosticEventView[]): VNode {
  const rows: TableRow[] = events.map((event) => ({
    key: event.event_id,
    cells: [
      formatTimestamp(event.occurred_at),
      h('div', [
        h('span', KIND_LABELS[event.kind]),
        h('div', { class: 'muted small' }, h('code', { title: event.kind }, event.kind)),
      ]),
      identifier(event.event_id),
      verdictCell(event),
      positionCell(event),
      correlationCell(event),
      auditRefCell(event),
      attrsCell(event),
    ],
  }))
  return h(DataTable, {
    headers: ['时间', '种类', '事件', '决策与原因', '位置', '关联', '审计回指', '属性'],
    rows,
    empty: '当前查询范围没有诊断事件',
    tableClass: 'diagnostics-table',
  })
}

export const DiagnosticsView = defineComponent({
  name: 'DiagnosticsView',
  props: viewProps,
  setup(props) {
    const api = new DiagnosticsApi(props.ctx.api)
    const seeded = seedAnchor(props.query)
    const anchorKind = ref<AnchorValue>(seeded.kind)
    const anchorValue = ref(seeded.value)
    const taskId = ref(props.query.get('task_id') ?? '')
    const kind = ref<DiagnosticKind | ''>('')
    const from = ref('')
    const to = ref('')
    const limit = ref(200)
    // 本地形状提示：空串表示形状合法。**不**把必然 400 的请求发出去。
    // A client-side shape check; an invalid request is never sent.
    const shapeError = ref('')

    function buildQuery(): DiagnosticQuery | null {
      const value = anchorValue.value.trim()
      if (value === '') {
        shapeError.value = '必须给出至少一个关联标识：运行 / 请求 / 管理操作 / 摄取任务 / 会话'
        return null
      }
      const fromRaw = toRFC3339(from.value)
      const toRaw = toRFC3339(to.value)
      if (fromRaw !== undefined && toRaw !== undefined && fromRaw > toRaw) {
        shapeError.value = '起始时间不得晚于结束时间'
        return null
      }
      shapeError.value = ''
      const base: DiagnosticQuery = {
        taskId: taskId.value.trim() === '' ? undefined : taskId.value.trim(),
        kind: kind.value === '' ? undefined : kind.value,
        from: fromRaw,
        to: toRaw,
        limit: normalizeLimit(String(limit.value)),
      }
      switch (anchorKind.value) {
        case 'run_id':
          return { ...base, runId: value }
        case 'request_id':
          return { ...base, requestId: value }
        case 'operation_id':
          return { ...base, operationId: value }
        case 'ingest_job_id':
          return { ...base, ingestJobId: value }
        default:
          return { ...base, sessionId: value }
      }
    }

    const result = useLoader(() => {
      const query = buildQuery()
      if (query === null) {
        return Promise.reject(new Error(shapeError.value))
      }
      return api.list(query)
    }, false)

    function apply(event: Event): void {
      event.preventDefault()
      // 换条件先丢旧结果：否则上一次查询的表格会和新的错误提示并排显示，
      // 用户无法判断屏幕上这批事件到底属于哪次查询。
      result.reset()
      void result.reload()
    }

    onMounted(() => {
      // 深链带了锚点就直接查一次；没带锚点就停在表单上，不发注定被 400 的请求。
      if (anchorValue.value.trim() !== '') {
        void result.reload()
      }
    })

    return (): VNode => {
      const content: VNodeChild[] = [
        h(
          'form',
          { class: 'filters', onSubmit: apply },
          [
            h('label', ['关联类型', h(
              'select',
              {
                value: anchorKind.value,
                onChange: (event: Event) => {
                  anchorKind.value = (event.target as HTMLSelectElement).value as AnchorValue
                },
              },
              ANCHORS.map((anchor) =>
                h(
                  'option',
                  { key: anchor.value, value: anchor.value },
                  // 锚点同理：契约收这个参数，但当前没有生产端写它。
                  anchor.produced ? anchor.label : `${anchor.label}（暂无生产端）`,
                ),
              ),
            )]),
            h('label', ['关联 ID', h('input', {
              type: 'text',
              value: anchorValue.value,
              onInput: (event: Event) => {
                anchorValue.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['任务限定（可选）', h('input', {
              type: 'text',
              value: taskId.value,
              onInput: (event: Event) => {
                taskId.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['种类', h(
              'select',
              {
                value: kind.value,
                onChange: (event: Event) => {
                  const next = (event.target as HTMLSelectElement).value
                  kind.value = next === '' ? '' : (next as DiagnosticKind)
                },
              },
              [
                h('option', { value: '' }, '全部'),
                // 没有生产端的种类在选项上就标出来：选中它必然空集，
                // 不标注等于让用户把"没写"当成"没发生"。
                ...KIND_OPTIONS.map((option) =>
                  h(
                    'option',
                    { key: option, value: option },
                    COVERED_KINDS.has(option) ? KIND_LABELS[option] : `${KIND_LABELS[option]}（暂无生产端）`,
                  ),
                ),
              ],
            )]),
            h('label', ['起始时间（本地）', h('input', {
              type: 'datetime-local',
              value: from.value,
              onInput: (event: Event) => {
                from.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['结束时间（本地）', h('input', {
              type: 'datetime-local',
              value: to.value,
              onInput: (event: Event) => {
                to.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['窗口', h('input', {
              type: 'number',
              min: 1,
              max: 1000,
              value: String(limit.value),
              onInput: (event: Event) => {
                limit.value = normalizeLimit((event.target as HTMLInputElement).value)
              },
            })]),
            h('button', { type: 'submit' }, '查询'),
          ],
        ),
        h('p', { class: 'muted small' }, '五类关联标识至少给一个；任务 ID 只作限定符，单独给出会被拒绝。窗口缺省 200、上限 1000。'),
        // 能力覆盖与审计区别：两句话都要在查询前可见——空结果的含义取决于它们。
        h('p', { class: 'muted small' }, coverageText()),
        h('p', { class: 'muted small' }, AUDIT_NOTE),
      ]

      if (shapeError.value !== '') {
        content.push(h(Notice, { tone: 'warn', text: shapeError.value }))
      }
      if (result.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: result.error.value }))
      }
      if (result.loading.value && result.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      const data = result.data.value
      if (data !== null) {
        // 证据缺口先于表格：空集在这两种标记下**不是**"一切正常"。
        // The gap notice precedes the table; an empty page is not "all good".
        if (data.missing) {
          content.push(
            h(Notice, {
              tone: 'warn',
              text: '当前查询范围没有诊断记录（missing）：这是证据缺口，不等于没有发生过。全量采样下无记录才敢这么说。',
            }),
          )
        }
        if (data.sampled_out === true) {
          content.push(
            h(Notice, {
              tone: 'warn',
              text: '本实例采样率低于 1，被丢弃的事件不落库、不留痕：无记录不能证明没发生（sampled_out）。',
            }),
          )
        }
        content.push(
          h('p', { class: 'muted small' }, `本实例保留期 ${data.retention_days} 天（配置值，不是保留承诺）。`),
        )
        content.push(eventTable(data.events))
      }

      return h('section', { class: 'view view-diagnostics' }, [
        pageHeading(
          'SYSTEM / DIAGNOSTICS',
          '诊断明细',
          '逐条查看原始诊断事件和关联标识；通常先到「运行排障」查看结果、耗时与失败原因。',
        ),
        ...content,
      ])
    }
  },
})
