// 文档详情（US-16 §3.1.9 / T043）：版本历史、失败处理（重建索引）与「来源与引用」
// 精确读取。三条契约事实决定页面形态：
//
//  1. 显式重建创建独立版本；旧版本在新版本成功生效前继续服务。
//  2. 检索命中只是指针（不带 `content_hash`），**不得**直接当引用；引用必须按
//     `EvidenceRef`（文档 + 版本 + 范围）走 `POST /sources/view` 精确读取。
//  3. `validity.valid=false` 时正文、事实与摘要一律为空——页面不得用新版本正文
//     冒充旧引用，也不得把空正文渲染成"空文档"。
//
// Version history, failure handling and the exact source view. Versions are
// append-only, a hit is never a citation, and an invalid reference serves no text.
import { computed, defineComponent, h, onBeforeUnmount, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import { KnowledgeApi } from '../api-knowledge'
import type {
  DocumentVersionSummary,
  EvidenceRange,
  IngestResult,
  SourceValidityReason,
  SourceViewRequest,
  SourceViewResult,
  HistoricalSourceResult,
} from '../api-knowledge'
import { CodeBlock, ConfirmDialog, DataTable, Notice, formatTimestamp } from '../components/ui'
import { collectionHref, knowledgeHref } from '../router'
import {
  DOCUMENT_STATUS_LABELS,
  documentStatusBadge,
  ingestErrorLabel,
  ingestNotice,
} from './knowledge'
import { badge, definitions, identifier, pageHeading, useLoader, viewProps } from './shared'

/** 失效原因文案；`valid=true` 时 reason 为空串。 Unknown reasons fall back to the raw value. */
const VALIDITY_REASON_LABELS: Readonly<Record<string, string>> = {
  revoked: '权限已撤销',
  deleted: '资料已删除',
  version_changed: '版本已变更',
  not_found: '未找到',
}

function validityReasonLabel(reason: SourceValidityReason): string {
  if (reason === '') {
    return '有效'
  }
  return VALIDITY_REASON_LABELS[reason] ?? reason
}

/** 构造请求的结果：校验不通过时给出固定文案，且不发出请求。 */
type BuiltRequest =
  | { readonly ok: true; readonly body: SourceViewRequest }
  | { readonly ok: false; readonly error: string }

/** 正整数解析；空串返回 undefined（不是 0）。 */
function parsePositiveInteger(raw: string): number | null | undefined {
  const trimmed = raw.trim()
  if (trimmed === '') {
    return undefined
  }
  const value = Number(trimmed)
  if (!Number.isInteger(value) || value < 1) {
    return null
  }
  return value
}

/** 非负整数解析；空串返回 undefined；非法返回 null。 */
function parseNonNegativeInteger(raw: string): number | null | undefined {
  const trimmed = raw.trim()
  if (trimmed === '') {
    return undefined
  }
  const value = Number(trimmed)
  if (!Number.isInteger(value) || value < 0) {
    return null
  }
  return value
}

export const DocumentDetailView = defineComponent({
  name: 'DocumentDetailView',
  props: viewProps,
  setup(props) {
    const api = new KnowledgeApi(props.ctx.api)
    const documentId = props.params.id ?? ''
    const collectionId = props.query.get('collection') ?? ''
    const versions = useLoader(() => api.listVersions(documentId))
    const documentList = useLoader(() => api.listDocuments(collectionId, false), collectionId !== '')
    const documentName = computed(() =>
      (documentList.data.value ?? []).find((row) => row.document_id === documentId)?.source_path ?? '',
    )

    // 默认读**当前生效**版本，而不是最新创建的那行（最新行可能是失败版本）。
    const selectedVersionId = ref('')
    const attempts = useLoader(() => api.ingestAttempts(documentId), true)
    const activeVersion = computed<DocumentVersionSummary | null>(() => {
      const rows = versions.data.value ?? []
      return rows.find((row) => row.status === 'active') ?? rows[0] ?? null
    })
    const selectedVersion = computed<DocumentVersionSummary | null>(() => {
      const rows = versions.data.value ?? []
      return rows.find((row) => row.version_id === selectedVersionId.value) ?? activeVersion.value
    })

    // --- 版本维护（重建索引）---------------------------------------------
    const reindexing = ref(false)
    const reindexError = ref('')
    const reindexed = ref<IngestResult | null>(null)

    async function reindex(): Promise<void> {
      reindexing.value = true
      reindexError.value = ''
      reindexed.value = null
      try {
        reindexed.value = await api.reindex(documentId)
        selectedVersionId.value = ''
        await Promise.all([versions.reload(), attempts.reload()])
      } catch (caught) {
        reindexError.value = errorMessage(caught)
      } finally {
        reindexing.value = false
      }
    }

    // --- 来源与引用（精确读取）-------------------------------------------
    const rangeStart = ref('')
    const rangeEnd = ref('')
    const budgetTokens = ref('')
    const includeFacts = ref(false)
    const includeSummary = ref(false)
    const sourceBusy = ref(false)
    const sourceError = ref('')
    const sourceResult = ref<SourceViewResult | null>(null)
    const historicalResult = ref<HistoricalSourceResult | null>(null)
    const historicalError = ref('')
    const historicalBusy = ref(false)
    let historicalGeneration = 0

    async function readHistorical(cursor = ''): Promise<void> {
      const version = selectedVersion.value
      if (version === null || version.status !== 'superseded') return
      const ticket = ++historicalGeneration
      historicalBusy.value = true
      historicalError.value = ''
      try {
        const result = await api.historicalSource({ id: documentId, version_id: version.version_id, content_hash: version.content_hash }, cursor)
        if (ticket === historicalGeneration) historicalResult.value = result
      } catch (caught) {
        if (ticket === historicalGeneration) { historicalResult.value = null; historicalError.value = errorMessage(caught) }
      } finally {
        if (ticket === historicalGeneration) historicalBusy.value = false
      }
    }
    const viewedCursor = ref('')
    let sourceGeneration = 0

    function buildRequest(cursor: string): BuiltRequest {
      const version = selectedVersion.value
      if (version === null) {
        return { ok: false, error: '尚无可读取的版本：先摄取文档或重建索引。' }
      }
      const start = parseNonNegativeInteger(rangeStart.value)
      const end = parseNonNegativeInteger(rangeEnd.value)
      if (start === null || end === null) {
        return { ok: false, error: '范围必须是非负整数（字符偏移，左闭右开）。' }
      }
      if ((start === undefined) !== (end === undefined)) {
        return { ok: false, error: '范围必须同时给起点与终点，或都留空。' }
      }
      if (start !== undefined && end !== undefined && end < start) {
        return { ok: false, error: '范围终点不得小于起点。' }
      }
      const budget = parsePositiveInteger(budgetTokens.value)
      if (budget === null) {
        return { ok: false, error: 'token 预算必须是正整数。' }
      }
      const body: SourceViewRequest = {
        // 引用自带版本与内容哈希：只认这一版原文，不用新版本冒充旧引用。
        ref: { id: documentId, version_id: version.version_id, content_hash: version.content_hash },
      }
      if (cursor !== '') {
        body.cursor = cursor
      } else if (start !== undefined && end !== undefined) {
        const range: EvidenceRange = { start, end }
        body.range = range
      }
      if (budget !== undefined) {
        body.budget_tokens = budget
      }
      if (includeFacts.value) {
        body.include_facts = true
      }
      if (includeSummary.value) {
        body.include_summary = true
      }
      return { ok: true, body }
    }

    async function readSource(cursor = ''): Promise<void> {
      const ticket = ++sourceGeneration
      sourceError.value = ''
      sourceResult.value = null
      const built = buildRequest(cursor)
      if (!built.ok) {
        sourceError.value = built.error
        return
      }
      sourceBusy.value = true
      try {
        const result = await api.sourceView(built.body)
        if (ticket === sourceGeneration) {
          sourceResult.value = result
          viewedCursor.value = cursor
        }
      } catch (caught) {
        if (ticket === sourceGeneration) sourceError.value = errorMessage(caught)
      } finally {
        if (ticket === sourceGeneration) sourceBusy.value = false
      }
    }

    // --- 软删文档 ---------------------------------------------------------
    const deleteOpen = ref(false)
    const deleting = ref(false)
    const deleteError = ref('')
    const deleteNotice = ref('')
    const documentDeleted = ref(false)

    // 普通阅读默认加载当前生效版本，分页仍逐次经过来源权限检查。
    const preview = ref<SourceViewResult | null>(null)
    const previewError = ref('')
    const previewBusy = ref(false)
    let previewGeneration = 0

    async function readPreview(cursor = ''): Promise<void> {
      const version = selectedVersion.value
      const ticket = ++previewGeneration
      preview.value = null
      previewError.value = ''
      if (documentDeleted.value || version === null || version.status !== 'active') {
        previewBusy.value = false
        return
      }
      previewBusy.value = true
      try {
        const result = await api.sourceView({
          ref: { id: documentId, version_id: version.version_id, content_hash: version.content_hash },
          budget_tokens: 2048,
          ...(cursor === '' ? {} : { cursor }),
        })
        if (ticket === previewGeneration) {
          preview.value = result
        }
      } catch (caught) {
        if (ticket === previewGeneration) {
          previewError.value = errorMessage(caught)
        }
      } finally {
        if (ticket === previewGeneration) {
          previewBusy.value = false
        }
      }
    }

    watch(
      () => [selectedVersion.value?.version_id, selectedVersion.value?.content_hash, selectedVersion.value?.status, documentDeleted.value],
      () => { void readPreview() },
      { immediate: true },
    )
    onBeforeUnmount(() => { previewGeneration++; sourceGeneration++ })

    async function confirmDelete(): Promise<void> {
      deleting.value = true
      deleteError.value = ''
      try {
        await api.deleteDocument(documentId)
        documentDeleted.value = true
        sourceGeneration++
        sourceBusy.value = false
        deleteOpen.value = false
        deleteNotice.value =
          '已软删文档：全部版本退出检索；历史消息与引用不被改写，' +
          '来源视图会按当前状态标注失效（不是删除历史）。' +
          '重新摄取同一路径会复活该文档（沿用原 document_id）；' +
	          '重新摄取会重新解析与向量化，并可能占用摄取配额。'
        sourceResult.value = null
      } catch (caught) {
        deleteError.value = errorMessage(caught)
      } finally {
        deleting.value = false
      }
    }

    // --- 渲染片段 ---------------------------------------------------------
    function numberField(
      id: string,
      label: string,
      value: string,
      onInput: (next: string) => void,
      min: string,
    ): VNode {
      return h('label', { class: 'field', for: id }, [
        h('span', {}, label),
        h('input', {
          id,
          type: 'number',
          min,
          step: '1',
          value,
          onInput: (event: Event) => {
            onInput((event.target as HTMLInputElement).value)
          },
        }),
      ])
    }

    function checkRow(id: string, label: string, value: boolean, onChange: (next: boolean) => void): VNode {
      return h('label', { class: 'check-row', for: id }, [
        h('input', {
          id,
          type: 'checkbox',
          checked: value,
          onChange: (event: Event) => {
            onChange((event.target as HTMLInputElement).checked)
          },
        }),
        h('span', {}, label),
      ])
    }

    function versionsSection(): VNode {
      const children: VNodeChild[] = [
        h('h2', {}, '版本'),
        h(
          'p',
          { class: 'muted small' },
	          '重建会重新处理文档并创建新版本。新版本成功前，已有生效版本继续用于检索；' +
	            '失败版本不会参与检索。历史版本保留以供查看和追溯。修订序号为实例内排序号，不代表当前文档的处理次数。',
        ),
      ]
      if (versions.error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: versions.error.value }))
      }
      const rows = versions.data.value
      if (rows === null) {
        if (versions.loading.value) {
          children.push(h('p', { class: 'muted' }, '加载中…'))
        }
      } else {
        children.push(
          h(DataTable, {
            tableClass: 'versions-table',
            columnClasses: ['col-id', 'col-number', 'col-id', 'col-state', 'col-number', 'col-short', 'col-time', 'col-time', 'col-short'],
            headers: ['版本', '修订序号', '内容哈希', '状态', '索引代际', '解析器 / 分块器', '创建时间', '激活时间', '失败原因'],
            rows: rows.map((row) => ({
              key: row.version_id,
              cells: [
                identifier(row.version_id),
                row.revision === undefined ? '—' : String(row.revision),
                row.content_hash,
                documentStatusBadge(row.status),
                row.generation === undefined ? '—' : String(row.generation),
                `${row.parser_version ?? '—'} / ${row.chunker_version ?? '—'}`,
                formatTimestamp(row.created_at),
                formatTimestamp(row.activated_at),
                row.status === 'failed' ? ingestErrorLabel(row.error_kind) : '—',
              ],
            })),
            empty: '还没有版本；摄取会创建第一个版本。',
          }),
        )
      }
      return h('section', { class: 'card' }, children)
    }

    function reindexSection(): VNode {
      const children: VNodeChild[] = [
        h('h2', {}, '失败处理'),
        h(
          'p',
          { class: 'muted small' },
	          '按当前来源文件重新解析并建立索引，即使内容未变也会创建新版本。' +
	            '此操作会消耗摄取和模型配额；成功前已有生效版本继续服务。',
        ),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              id: 'document-reindex',
              type: 'button',
              disabled: reindexing.value || documentDeleted.value,
              onClick: () => { void reindex() },
            },
            reindexing.value ? '重建中…' : '重建索引',
          ),
        ]),
      ]
      if (reindexError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: reindexError.value }))
      }
      if (reindexed.value !== null) {
        const notice = ingestNotice(reindexed.value)
        children.push(h(Notice, { tone: notice.tone, text: notice.text }))
      }
      return h('section', { class: 'card' }, children)
    }

    function attemptsSection(): VNode {
      const children: VNodeChild[] = [h('h2', {}, '处理记录')]
      if (attempts.error.value) children.push(h(Notice, { tone: 'error', text: `处理记录读取失败：${attempts.error.value}` }))
      const result = attempts.data.value
      if (result !== null) {
        children.push(h(DataTable, {
          headers: ['版本', '尝试次数', '阶段', '结果', '失败原因', '开始时间', '结束时间'],
          rows: (result.items ?? []).map((row) => ({ key: row.job_id, cells: [
            identifier(row.version_id), String(row.attempt), row.stage, row.status,
            row.error_kind ? ingestErrorLabel(row.error_kind) : '—', formatTimestamp(row.started_at), formatTimestamp(row.finished_at),
          ] })),
          empty: '尚无处理记录。',
        }))
        if (result.truncated) children.push(h(Notice, { tone: 'warn', text: '仅显示最近 100 条处理记录。' }))
      } else if (attempts.loading.value) children.push(h('p', { class: 'muted' }, '正在读取处理记录…'))
      return h('section', { class: 'card', id: 'ingest-attempts' }, children)
    }

    function sourceResultView(): VNodeChild[] {
      const result = sourceResult.value
      if (result === null) {
        return []
      }
      const valid = result.validity.valid
      const readVersion = result.version_id ?? ''
      const contentHash = result.content_hash ?? ''
      const nodes: VNodeChild[] = [
        definitions([
          ['请求引用', h('span', [identifier(result.ref.id), ' @ ', identifier(result.ref.version_id)])],
          [
            '有效性',
            h('span', [
              valid ? badge('有效', 'ok') : badge('失效', 'bad'),
              ' ',
              valid
                ? '按当前权限、删除状态与版本核对通过'
                : `原因：${validityReasonLabel(result.validity.reason)}`,
            ]),
          ],
          ['实际读取版本', readVersion === '' ? '—' : identifier(readVersion)],
          [
            '内容哈希',
            contentHash === ''
              ? '—'
              : h('code', { class: 'cell-id', title: contentHash }, contentHash),
          ],
          ['范围', result.range === undefined ? '—' : `${result.range.start} – ${result.range.end}（左闭右开）`],
          ['截断', result.truncated ? '是（服务端给了下一页游标）' : '否'],
          ['本次续读游标', viewedCursor.value === '' ? '—' : viewedCursor.value],
        ]),
      ]
      if (!valid) {
        nodes.push(
          h(Notice, {
            tone: 'error',
            text:
              '引用失效：服务端不返回正文、事实与摘要。' +
              '不得用新版本的内容冒充旧引用，也不得把空正文当成"空文档"。',
          }),
        )
        return nodes
      }
      if (result.conversion !== undefined && result.conversion !== '') {
        nodes.push(h('p', { class: 'muted small' }, `转换说明：${result.conversion}`))
      }
      nodes.push(h('p', { class: 'muted small' }, '正文（按引用精确读取，不经过相似度）'))
      // 正文为空是**数据**，不是"没读到"：空代码块会让人以为页面没渲染，
      // 必须说清"这个版本本身没有正文"，也不得顺手换成别的版本内容。
      // An empty body is data, not a rendering failure; say so instead of showing a blank block.
      nodes.push(
        result.text === ''
          ? h(Notice, {
              tone: 'warn',
              text: '读取有效，但这个版本 / 范围没有正文（服务端返回空串）。不是页面加载失败，也不代表文档不存在。',
            })
          : h(CodeBlock, { text: result.text }),
      )
      nodes.push(
        definitions([
          [
            '父引用',
            result.parent_refs.length === 0
              ? '无（顶层引用）'
              : result.parent_refs.map((ref) => `${ref.id} @ ${ref.version_id}`).join('；'),
          ],
        ]),
      )
      if (result.facts.length > 0) {
        nodes.push(h('h3', {}, '结构化事实'))
        nodes.push(definitions(result.facts.map((fact) => [fact.key, fact.value] as const)))
      } else if (includeFacts.value) {
        nodes.push(h('p', { class: 'muted small' }, '没有结构化事实（已请求 include_facts）。'))
      }
      if (result.summary !== undefined) {
        nodes.push(h('h3', {}, '来源摘要（不含任务进度）'))
        nodes.push(
          definitions([
            ['文档标题', result.summary.document_title ?? '—'],
            ['章节路径', result.summary.heading_path ?? '—'],
            ['章节摘要', result.summary.section_summary ?? '—'],
          ]),
        )
      }
      if (result.truncated && result.next_cursor !== undefined && result.next_cursor !== '') {
        nodes.push(
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                id: 'source-view-more',
                type: 'button',
                disabled: sourceBusy.value,
                onClick: () => { void readSource(result.next_cursor ?? '') },
              },
              '继续读取下一页',
            ),
          ]),
        )
      }
      return nodes
    }

    function sourceSection(): VNode {
      const version = selectedVersion.value
      const children: VNodeChild[] = [
        h('h2', {}, '来源与引用'),
        h(
          'p',
          { class: 'muted small' },
          '检索命中只是指针（不带内容哈希），**不得**直接当引用；引用必须按"文档 + 版本 + 范围"' +
            '精确读取。每次读取都按当前身份重查权限、删除状态与版本：同一引用对不同调用方可以有不同结果。',
        ),
      ]
      if (versions.data.value === null) {
        children.push(h('p', { class: 'muted' }, '版本尚未加载，暂不能读取来源。'))
        return h('section', { class: 'card' }, children)
      }
      children.push(
        h('label', { class: 'field', for: 'source-version' }, [
          h('span', {}, '读取哪个版本'),
          h(
            'select',
            {
              id: 'source-version',
              value: version?.version_id ?? '',
              disabled: version === null,
              onChange: (event: Event) => {
                selectedVersionId.value = (event.target as HTMLSelectElement).value
                sourceGeneration++
                sourceBusy.value = false
                sourceResult.value = null
                historicalGeneration++
                historicalResult.value = null
                historicalError.value = ''
                historicalBusy.value = false
                viewedCursor.value = ''
              },
            },
            (versions.data.value ?? []).map((row) =>
              h(
                'option',
                { key: row.version_id, value: row.version_id },
                `${row.version_id}（${DOCUMENT_STATUS_LABELS[row.status].label}）`,
              ),
            ),
          ),
        ]),
      )
      children.push(
        h('p', { class: 'field-hint muted' }, '默认读取当前生效版本；最新创建的那行可能是失败版本。'),
      )
      children.push(
        h('div', { class: 'source-range' }, [
          numberField('source-range-start', '范围起点（可留空）', rangeStart.value, (next) => {
            rangeStart.value = next
          }, '0'),
          numberField('source-range-end', '范围终点（不含，可留空）', rangeEnd.value, (next) => {
            rangeEnd.value = next
          }, '0'),
        ]),
      )
      children.push(
        numberField('source-budget', 'token 预算（可留空）', budgetTokens.value, (next) => {
          budgetTokens.value = next
        }, '1'),
      )
      children.push(
        checkRow('source-include-facts', '包含结构化事实', includeFacts.value, (next) => {
          includeFacts.value = next
        }),
        checkRow('source-include-summary', '包含来源摘要', includeSummary.value, (next) => {
          includeSummary.value = next
        }),
      )
      children.push(
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              id: 'source-view-submit',
              type: 'button',
              class: 'primary',
              disabled: sourceBusy.value || version === null || documentDeleted.value,
              onClick: () => { void readSource('') },
            },
            sourceBusy.value ? '读取中…' : '精确读取',
          ),
        ]),
      )
      if (sourceError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: sourceError.value }))
      }
      children.push(...sourceResultView())
      if (version?.status === 'superseded' && !documentDeleted.value) {
        children.push(h('p', { class: 'muted small' }, '历史版本用于查证当时内容，不代表当前引用仍然有效。'))
        children.push(h('button', { id: 'historical-source-read', type: 'button', disabled: historicalBusy.value,
          onClick: () => { void readHistorical() } }, '查看历史正文'))
        if (historicalError.value) children.push(h(Notice, { tone: 'error', text: historicalError.value }))
        const historical = historicalResult.value
        if (historical !== null) {
          children.push(h(Notice, { tone: 'info', text: '历史版本：以下为当时原文，不能作为当前有效引用。' }))
          children.push(h(CodeBlock, { text: historical.text }))
          if (historical.truncated && historical.next_cursor) {
            children.push(h('button', { type: 'button', disabled: historicalBusy.value,
              onClick: () => { void readHistorical(historical.next_cursor) } }, '读取历史正文下一页'))
          }
        }
      }
      return h('section', { class: 'card' }, children)
    }

    function dangerZone(): VNode {
      const children: VNodeChild[] = [
        h('h2', {}, '软删文档'),
        h(
          'p',
          { class: 'muted small' },
          '软删会让全部版本退出检索，但不物理删除行，也不改写历史消息与其引用；' +
            '重复删除幂等。历史引用在读取面按当前删除状态标注失效。' +
            '恢复方式：重新摄取同一路径会复活该文档（沿用原 document_id）；' +
	            '恢复可能需要重新解析与向量化，并消耗摄取配额。',
        ),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              id: 'document-delete',
              type: 'button',
              class: 'danger',
              disabled: deleting.value || documentDeleted.value,
              onClick: () => {
                deleteError.value = ''
                deleteOpen.value = true
              },
            },
            documentDeleted.value ? '已软删' : '软删文档',
          ),
        ]),
      ]
      if (deleteError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: deleteError.value }))
      }
      if (deleteNotice.value !== '') {
        children.push(h(Notice, { tone: 'info', text: deleteNotice.value }))
      }
      children.push(
        h(ConfirmDialog, {
          open: deleteOpen.value,
          title: '确认软删文档',
          message:
            `软删文档 ${documentId} 后，它的全部版本退出检索；历史消息与引用不被改写。` +
            '可用同一路径重新摄取恢复（沿用原 document_id）。继续？',
          confirmText: deleting.value ? '删除中…' : '软删',
          busy: deleting.value,
          onConfirm: () => { void confirmDelete() },
          onCancel: () => {
            if (!deleting.value) {
              deleteOpen.value = false
            }
          },
        }),
      )
      return h('section', { class: ['card', 'danger-zone'] }, children)
    }

    function previewSection(): VNode {
      const children: VNodeChild[] = [h('h2', {}, '文档内容')]
      const version = selectedVersion.value
      if (documentDeleted.value) {
        children.push(h('p', { class: 'muted' }, '文档已删除，无法预览。'))
      } else if (version === null || version.status !== 'active') {
        children.push(h('p', { class: 'muted' }, '当前没有可预览的生效版本。'))
      } else if (previewBusy.value) {
        children.push(h('p', { class: 'muted' }, '正在读取正文…'))
      } else if (previewError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: `正文读取失败：${previewError.value}` }))
      } else if (preview.value !== null) {
        const result = preview.value
        if (!result.validity.valid) {
          children.push(h(Notice, { tone: 'error', text: `正文不可用：${validityReasonLabel(result.validity.reason)}` }))
        } else {
          children.push(h('p', { class: 'muted small' }, '正在查看当前生效版本；每次读取都会重新检查权限。'))
          children.push(result.text === ''
            ? h('p', { class: 'muted' }, '这个版本没有正文。')
            : h(CodeBlock, { text: result.text }))
          if (result.truncated && result.next_cursor) {
            children.push(h('button', {
              id: 'document-preview-next',
              type: 'button',
              onClick: () => { void readPreview(result.next_cursor) },
            }, '查看下一页'))
          }
        }
      }
      return h('section', { class: 'card', id: 'document-preview' }, children)
    }

    return (): VNode => {
      const back = collectionId === ''
        ? knowledgeHref()
        : collectionHref(collectionId)
      return h('section', { class: 'view view-document-detail' }, [
        pageHeading(
          'KNOWLEDGE / DOCUMENT',
          documentName.value || '文档详情',
          '查看正文、处理状态和历史版本。',
        ),
        h('p', {}, [h('a', { href: back, class: 'back-link' }, '← 返回知识库')]),
        h(
          'p',
          { class: 'muted small' },
          documentList.error.value === '' ? '正文按当前身份读取，权限或删除状态变化时可能无法查看。' : '文档名称暂时无法读取，可从集合列表查看。',
        ),
        h('section', { class: 'card' }, [
          definitions([
            ['文档', identifier(documentId)],
            [
              '集合',
              collectionId === ''
                ? '—（链接未带 collection 参数）'
                : identifier(collectionId, collectionHref(collectionId)),
            ],
          ]),
        ]),
        previewSection(),
        versionsSection(),
        attemptsSection(),
        reindexSection(),
        sourceSection(),
        dangerZone(),
      ])
    }
  },
})
