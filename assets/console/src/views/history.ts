// 历史回查页面（US-16 §3.1.9 / T057）：按关键词、Agent、调用方、会话与时间窗回查
// 历史消息。四条口径直接来自契约与设计：
//
//  1. **列表只是指针**：命中只给 `message_id` / `session_id` / `run_id` / 片段与分数，
//     页面**不得**把片段当引用依据；点开后走精确读取（`GET /sessions/{id}/messages/{id}`）
//     才显示原文。
//  2. **当时版本要标出来**：精确读取的 `evidence_refs` 带 `version_id`——那是消息发出时
//     引用的版本，当前结论（valid / reason）由服务端按**今天**的 ACL / 删除 / 版本重查；
//     页面把两者分开呈现，不用新版本冒充旧引用。
//  3. **`truncated` 是数据**：检索命中超过本页上限时必须明说"本页不是全部"，不得静默
//     当成完整结果。
//  4. **本地先判形状**：空关键词与 from > to 都是注定被 400 的请求，页面不发出。
//
// The history page treats hits as pointers only, shows each evidence ref's original
// version alongside today's validity verdict, and never sends a request it knows the
// contract will reject.
import { defineComponent, h, onMounted, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import { HistoryApi } from '../api-history'
import type {
  HistoryHit,
  HistoryQuery,
  HistoryRole,
  MessageDetail,
  MessageEvidenceRefView,
} from '../api-history'
import { CodeBlock, DataTable, Notice, formatTimestamp, shortHash } from '../components/ui'
import type { TableRow } from '../components/ui'
import { documentHref, runHref } from '../router'
import { badge, definitions, identifier, pageHeading, useLoader, viewProps } from './shared'

/** 角色四值的中文文案；键名逐字等于契约取值。 */
// The four contract roles, labelled; keys are the contract values verbatim.
const ROLE_LABELS: Record<HistoryRole, string> = {
  user: '用户',
  assistant: '助手',
  tool: '工具',
  system: '系统',
}

/** 窗口缺省 50、上限 200（契约 `/history/search`）；非法输入按缺省处理。 */
const DEFAULT_LIMIT = 50
const MAX_LIMIT = 200

function normalizeLimit(raw: string): number {
  if (raw.trim() === '') {
    return DEFAULT_LIMIT
  }
  const parsed = Number.parseInt(raw, 10)
  if (Number.isNaN(parsed) || parsed < 1) {
    return DEFAULT_LIMIT
  }
  return Math.min(parsed, MAX_LIMIT)
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

/** 失效原因文案（契约 `ValidityView.reason`）；`valid=true` 时 reason 为空串。 */
// Mirrors the contract enum; unknown reasons fall back to the raw value.
const VALIDITY_REASON_LABELS: Readonly<Record<string, string>> = {
  revoked: '权限已撤销',
  deleted: '资料已删除',
  version_changed: '版本已变更',
  not_found: '未找到',
}

function validityLabel(valid: boolean, reason: string): string {
  if (valid) {
    // 「当前可读」而不是「有效」：它只说这个版本现在仍能按引用精确读取，
    // 既不证明当时的回答正确，也不代表引用的仍是当前最新版本。
    return '当前可读'
  }
  return VALIDITY_REASON_LABELS[reason] ?? reason
}

/** 来源引用表：资料 / 当时版本 / 内容哈希 / 当前结论——四列各自独立。 */
// The original version stays next to today's verdict; neither replaces the other.
function refTable(refs: readonly MessageEvidenceRefView[]): VNode {
  const rows: TableRow[] = refs.map((entry, index) => ({
    key: `${entry.ref.id}:${entry.ref.version_id}:${index}`,
    cells: [
      // 资料 ID 直接进文档详情：那一页能看到版本列表与当前生效状态。
      // The document page shows the version list and which version is active now.
      identifier(entry.ref.id, documentHref(entry.ref.id)),
      identifier(entry.ref.version_id),
      entry.ref.content_hash === undefined || entry.ref.content_hash === ''
        ? '—'
        : h('code', { title: entry.ref.content_hash }, shortHash(entry.ref.content_hash)),
      badge(
        validityLabel(entry.validity.valid, entry.validity.reason),
        entry.validity.valid ? 'ok' : 'warn',
      ),
    ],
  }))
  return h(DataTable, {
    headers: ['资料', '当时版本（version_id）', '内容哈希', '当前结论'],
    rows,
    empty: '没有来源引用',
    tableClass: 'history-refs',
  })
}

export const HistoryView = defineComponent({
  name: 'HistoryView',
  props: viewProps,
  setup(props) {
    const api = new HistoryApi(props.ctx.api)
    // 深链回填：会话详情页的「历史回查」只带 session_id；关键词仍由用户给出。
    // The session page deep-links with session_id only; the keyword stays user input.
    const q = ref(props.query.get('q') ?? '')
    const agent = ref(props.query.get('agent') ?? '')
    const callerId = ref(props.query.get('caller_id') ?? '')
    const sessionId = ref(props.query.get('session_id') ?? '')
    const from = ref('')
    const to = ref('')
    const limit = ref(DEFAULT_LIMIT)
    // 本地形状提示：空串表示形状合法。**不**把必然 400 的请求发出去。
    // A client-side shape check; an invalid request is never sent.
    const shapeError = ref('')

    function buildQuery(): HistoryQuery | null {
      const keyword = q.value.trim()
      if (keyword === '') {
        shapeError.value = '必须给出关键词：历史检索不接受空关键词'
        return null
      }
      const fromRaw = toRFC3339(from.value)
      const toRaw = toRFC3339(to.value)
      if (fromRaw !== undefined && toRaw !== undefined && fromRaw > toRaw) {
        shapeError.value = '起始时间不得晚于结束时间'
        return null
      }
      shapeError.value = ''
      return {
        q: keyword,
        agent: agent.value.trim() === '' ? undefined : agent.value.trim(),
        sessionId: sessionId.value.trim() === '' ? undefined : sessionId.value.trim(),
        callerId: callerId.value.trim() === '' ? undefined : callerId.value.trim(),
        from: fromRaw,
        to: toRaw,
        limit: normalizeLimit(String(limit.value)),
      }
    }

    const search = useLoader(() => {
      const query = buildQuery()
      if (query === null) {
        return Promise.reject(new Error(shapeError.value))
      }
      return api.search(query)
    }, false)

    // --- 精确读取（点开一条命中之后）---------------------------------------
    // 代际号丢弃过期响应：连续点两条时，先发的响应晚到也不覆盖后选中的原文。
    // A generation ticket drops a stale exact-read response.
    const detail = ref<MessageDetail | null>(null)
    const detailError = ref('')
    const detailLoading = ref(false)
    let detailTicket = 0

    async function openMessage(hit: HistoryHit): Promise<void> {
      const ticket = ++detailTicket
      detailLoading.value = true
      detailError.value = ''
      try {
        const view = await api.readMessage(hit.session_id, hit.message_id)
        if (ticket === detailTicket) {
          detail.value = view
        }
      } catch (caught) {
        if (ticket === detailTicket) {
          detail.value = null
          detailError.value = errorMessage(caught)
        }
      } finally {
        if (ticket === detailTicket) {
          detailLoading.value = false
        }
      }
    }

    function closeDetail(): void {
      detailTicket += 1
      detail.value = null
      detailError.value = ''
      detailLoading.value = false
    }

    function apply(event: Event): void {
      event.preventDefault()
      closeDetail()
      // 换条件先丢旧结果：否则上一次命中会和新的错误提示并排显示，
      // 用户无从判断这批命中属于哪次查询。
      search.reset()
      void search.reload()
    }

    onMounted(() => {
      // 深链带了关键词就直接查一次；只有会话过滤时不发请求，停在表单上等关键词。
      if (q.value.trim() !== '') {
        void search.reload()
      }
    })

    /** 命中表：时间 / 角色 / 片段 / 消息 / 会话 / 运行 / 分数 / 操作。 */
    function hitTable(hits: readonly HistoryHit[]): VNode {
      const rows: TableRow[] = hits.map((hit) => ({
        key: hit.message_id,
        cells: [
          formatTimestamp(hit.created_at),
          h('div', [
            badge(ROLE_LABELS[hit.role], 'muted'),
            h('div', { class: 'muted small' }, h('code', { title: hit.role }, hit.role)),
          ]),
          h('span', { class: 'history-snippet' }, hit.snippet),
          identifier(hit.message_id),
          identifier(hit.session_id),
          identifier(hit.run_id, runHref(hit.run_id)),
          hit.score === undefined ? '—' : hit.score.toFixed(3),
          h(
            'button',
            { type: 'button', class: 'link-button', onClick: () => void openMessage(hit) },
            '查看原文',
          ),
        ],
      }))
      return h(DataTable, {
        headers: ['时间', '角色', '片段', '消息', '会话', '运行', '分数', '操作'],
        rows,
        empty: '当前条件没有命中；换个关键词或放宽时间窗',
        tableClass: 'history-table',
      })
    }

    /** 精确读取面板：元数据、原文，以及每条引用「当时版本」与「当前结论」。 */
    function detailSection(view: MessageDetail): VNode {
      const pairs: (readonly [string, VNodeChild])[] = [
        ['消息', identifier(view.message_id)],
        ['会话', identifier(view.session_id)],
        ['运行', identifier(view.run_id, runHref(view.run_id))],
        ['序号 / 角色', `${view.seq} · ${ROLE_LABELS[view.role]}`],
        ['创建时间', formatTimestamp(view.created_at)],
      ]
      if (view.tool_call_id !== undefined) {
        pairs.push(['工具调用', identifier(view.tool_call_id)])
      }
      const children: VNodeChild[] = [
        definitions(pairs),
        view.truncated
          ? h(Notice, {
              tone: 'warn',
              text: '原文已按保留策略截断：这里显示的不是完整内容。',
            })
          : null,
        h(CodeBlock, { text: view.content }),
        h('h3', '来源引用'),
        h(
          'p',
          { class: 'muted small' },
          '「当时版本」是消息发出时引用的版本；「当前结论」由服务端按今天的权限 / 删除 / 版本重查，两者不互相替换。' +
            // 「当前可读」不是「当时的回答被证明」：这是历史回查最容易读错的一处。
            '「当前可读」只说明该版本现在仍能按引用精确读取——它不证明当时的回答正确，' +
            '也不说明引用的是资料的最新版本；点资料 ID 可到文档详情核对版本与生效状态。',
        ),
        refTable(view.evidence_refs ?? []),
      ]
      return h('section', { class: 'card history-detail' }, [
        h('div', { class: 'card-head' }, [
          h('h2', '消息原文'),
          h('button', { type: 'button', class: 'link-button', onClick: closeDetail }, '关闭'),
        ]),
        ...children,
      ])
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        h(
          'form',
          { class: 'filters', onSubmit: apply },
          [
            h('label', ['关键词（必填）', h('input', {
              type: 'text',
              value: q.value,
              onInput: (event: Event) => {
                q.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['Agent', h('input', {
              type: 'text',
              value: agent.value,
              onInput: (event: Event) => {
                agent.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['调用方 ID', h('input', {
              type: 'text',
              value: callerId.value,
              onInput: (event: Event) => {
                callerId.value = (event.target as HTMLInputElement).value
              },
            })]),
            h('label', ['会话 ID', h('input', {
              type: 'text',
              value: sessionId.value,
              onInput: (event: Event) => {
                sessionId.value = (event.target as HTMLInputElement).value
              },
            })]),
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
              max: MAX_LIMIT,
              value: String(limit.value),
              onInput: (event: Event) => {
                limit.value = normalizeLimit((event.target as HTMLInputElement).value)
              },
            })]),
            h('button', { type: 'submit' }, '查询'),
          ],
        ),
        h(
          'p',
          { class: 'muted small' },
          '关键词必填；调用方过滤只对 admin token 生效（caller key 传他人 ID 会被拒绝）。窗口缺省 50、上限 200。',
        ),
      ]

      if (shapeError.value !== '') {
        content.push(h(Notice, { tone: 'warn', text: shapeError.value }))
      }
      if (search.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: search.error.value }))
        // 失败后给出下一步：否则用户只知道"出错了"，不知道能做什么。
        content.push(
          h(
            'p',
            { class: 'muted small' },
            '检索失败不会返回半份结果：先确认服务可用（登录页右上状态），再缩小时间窗或换关键词重试；' +
              '持续 500 时用相关运行 ID 到「诊断事件」页查服务端记录。',
          ),
        )
      }
      if (search.loading.value && search.data.value === null) {
        content.push(h('p', { class: 'muted' }, '检索中…'))
      }
      const result = search.data.value
      if (result !== null) {
        if (result.truncated === true) {
          content.push(
            h(Notice, {
              tone: 'warn',
              text: '命中数超过本页上限（truncated）：本页不是全部，请收窄条件或改小时间窗。',
            }),
          )
        }
        content.push(
          h(
            'p',
            { class: 'muted small' },
            '命中片段只用于定位，不是引用依据。引用以「查看原文」里消息自带的来源引用为准；' +
              '要核对资料本身（当前正文、版本状态），点来源引用里的资料 ID 进文档详情做精确读取。' +
              '本页只检索会话消息，不检索知识库文档正文。',
          ),
        )
        content.push(hitTable(result.hits))
      }

      if (detailLoading.value) {
        content.push(h('p', { class: 'muted' }, '原文加载中…'))
      }
      if (detailError.value !== '') {
        content.push(h(Notice, { tone: 'error', text: detailError.value }))
      }
      const view = detail.value
      if (view !== null) {
        content.push(detailSection(view))
      }

      return h('section', { class: 'view view-history' }, [
        pageHeading(
          'SYSTEM / HISTORY',
          '历史回查',
          '按关键词、Agent、调用方与时间窗回查历史消息；命中只是指针，点开后精确读取原文。',
        ),
        ...content,
      ])
    }
  },
})
