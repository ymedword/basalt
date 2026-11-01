// 知识库集合与文档页（US-16 §3.1.9 / T043）：集合列表 / 建集合 / 软删集合，集合内
// 文档列表 / 摄取 / 软删文档。三条契约事实决定页面形态：
//
//  1. 契约**没有**单集合读取端点：集合详情页从 `GET /collections`（含软删）里定位
//     该行；找不到就如实说"不存在或不可见"，不伪造一个读取接口。
//  2. 摄取返回的 `status` 是终态事实：`failed` 只代表**本次处理的版本**失败，文档
//     如有更早的 active 版本则继续服务；页面必须把这两种情况分开写，否则操作者会
//     以为文档已不可用，或把空知识库当成仍然可用。
//  3. 权限由服务端判定：页面不禁用"越权"按钮，被拒时照实显示 403 / 404。
//
// Collection and document pages. The contract has no single-collection read, a failed
// ingest never replaces the active version, and permissions are judged by the server,
// never inferred from which button the page chose to show.
import { computed, defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import { KnowledgeApi } from '../api-knowledge'
import type {
  CollectionSummary,
  DocumentSummary,
  DocumentVersionStatus,
  IngestResult,
  KnowledgeSearchResult,
} from '../api-knowledge'
import {
  ConfirmDialog,
  DataTable,
  Notice,
  formatTimestamp,
  shortHash,
} from '../components/ui'
import type { Tone } from '../components/ui'
import { collectionHref, documentHref, knowledgeHref } from '../router'
import type { NoticeTone } from './shared'
import { badge, definitions, identifier, pageHeading, useLoader, viewProps } from './shared'

/** 版本状态六值文案；只有 `active` 参与检索。 Unknown codes fall back to the raw value. */
export const DOCUMENT_STATUS_LABELS: Readonly<
  Record<DocumentVersionStatus, { readonly label: string; readonly tone: Tone }>
> = {
  pending: { label: '待索引', tone: 'warn' },
  indexed: { label: '已索引', tone: 'warn' },
  active: { label: '生效中', tone: 'ok' },
  superseded: { label: '已取代', tone: 'muted' },
  retired: { label: '已退役', tone: 'muted' },
  failed: { label: '失败', tone: 'bad' },
}

/** 摄取失败类别文案；`error_kind` 只描述失败的**本次版本**，不是整个文档。 */
export const INGEST_ERROR_LABELS: Readonly<Record<string, string>> = {
  path_rejected: '路径被拒绝（越界或扩展名不符）',
  decode_failed: '解码失败（非 UTF-8 文本）',
  parser_failed: '解析失败',
  chunker_failed: '分块失败',
  embed_failed: '向量化失败',
  verify_failed: '校验失败',
}

/** 状态徽标；缺省（尚无版本）显示"无版本"，未知值原样展示并标灰。 */
export function documentStatusBadge(status: string | undefined): VNode {
  if (status === undefined || status === '') {
    return badge('无版本', 'muted')
  }
  const known = DOCUMENT_STATUS_LABELS[status as DocumentVersionStatus]
  return known === undefined ? badge(status, 'muted') : badge(known.label, known.tone)
}

/** 失败类别文案；未知类别原样返回，不假装认识它。 */
export function ingestErrorLabel(kind: string | undefined): string {
  if (kind === undefined || kind === '') {
    return '未给出失败类别'
  }
  return INGEST_ERROR_LABELS[kind] ?? kind
}

/**
 * 摄取 / 重建索引结果的页面文案：`deduplicated` 是"没新建版本"而不是失败；
 * `failed` 必须同时说明旧 active 版本继续服务。
 * The ingest outcome copy; a deduplicated result is not a failure.
 */
export function ingestNotice(result: IngestResult): { tone: NoticeTone; text: string } {
  if (result.deduplicated === true) {
    return {
      tone: 'info',
      text: `内容未变：复用文档 ${result.document_id} 的版本 ${result.version_id}，本次未新建版本。`,
    }
  }
  switch (result.status) {
    case 'active':
      return {
        tone: 'info',
        text: `已生效：版本 ${result.version_id}，${result.chunk_count ?? 0} 个切片，索引代际 ${result.generation ?? 0}。`,
      }
    case 'failed':
      // 失败只在“该文档此前已有生效版本”时才谈得上“旧版本继续服务”：
      // 首次摄取失败时根本不存在可服务的版本，写死那句会把空知识库说成可用。
      // The "old version keeps serving" clause only holds when an active version exists.
      return {
        tone: 'error',
        text:
          `摄取失败（${ingestErrorLabel(result.error_kind)}）：版本 ${result.version_id} 不参与检索。` +
          '若该文档此前已有生效版本，它继续服务；首次摄取失败时该文档暂时没有可检索内容，' +
          '修正来源文件后可重试。',
      }
    case 'pending':
    case 'indexed':
      return {
        tone: 'warn',
        text:
          `版本 ${result.version_id} 已写入但尚未生效` +
          `（${result.status === 'indexed' ? '已索引' : '待索引'}）；激活前不参与检索。`,
      }
    default:
      return { tone: 'info', text: `版本 ${result.version_id} 状态：${result.status}。` }
  }
}

/** 待确认的软删目标；集合与文档共用同一个对话框。 */
interface PendingDelete {
  readonly kind: 'collection' | 'document'
  readonly id: string
  readonly label: string
}

function deleteButton(id: string, label: string, disabled: boolean, onClick: () => void): VNode {
  return h(
    'button',
    { id, type: 'button', class: 'link-button', disabled, onClick },
    label,
  )
}

export const KnowledgeView = defineComponent({
  name: 'KnowledgeView',
  props: viewProps,
  setup(props) {
    const api = new KnowledgeApi(props.ctx.api)
    const collectionId = props.params.collectionId ?? ''
    const listMode = collectionId === ''

    // --- 集合列表态 -------------------------------------------------------
    const includeDeleted = ref(false)
    const collections = useLoader(
      () => api.listCollections({ includeDeleted: includeDeleted.value }),
      listMode,
    )
    const name = ref('')
    const project = ref('')
    const creating = ref(false)
    const createError = ref('')
    const created = ref<CollectionSummary | null>(null)

    // --- 集合详情态 -------------------------------------------------------
    // 契约没有单集合读取：用含软删的列表定位当前行，找不到时如实说明。
    const docsIncludeDeleted = ref(false)
    const catalog = useLoader(() => api.listCollections({ includeDeleted: true }), !listMode)
    const documents = useLoader(
      () => api.listDocuments(collectionId, docsIncludeDeleted.value),
      !listMode,
    )
    const collection = computed<CollectionSummary | null>(() => {
      const rows = catalog.data.value
      if (rows === null) {
        return null
      }
      return rows.find((row) => row.collection_id === collectionId) ?? null
    })
    const ingestPath = ref('')
    const uploadFile = ref<File | null>(null)
    const uploading = ref(false)
    const uploadNotice = ref('')
    const uploadError = ref('')
    const ingesting = ref(false)
    const ingestError = ref('')
    const ingested = ref<IngestResult | null>(null)
    const searchQuery = ref('')
    const searchSubmitted = ref('')
    const searchBusy = ref(false)
    const searchError = ref('')
    const searchResult = ref<KnowledgeSearchResult | null>(null)
    const descriptionEditing = ref(false)
    const descriptionDraft = ref('')
    const descriptionRevision = ref<number | null>(null)
    const descriptionBusy = ref(false)
    const descriptionError = ref('')
    const descriptionNotice = ref('')
    let searchGeneration = 0

    function editDescription(row: CollectionSummary): void {
      if (row.description_revision === undefined) {
        descriptionError.value = '服务端未提供简介版本，暂不能编辑；请刷新后重试。'
        return
      }
      descriptionDraft.value = row.description ?? ''
      descriptionRevision.value = row.description_revision
      descriptionError.value = ''
      descriptionNotice.value = ''
      descriptionEditing.value = true
    }

    async function saveDescription(): Promise<void> {
      if (descriptionRevision.value === null || descriptionBusy.value) return
      descriptionBusy.value = true
      descriptionError.value = ''
      try {
        await api.updateDescription(collectionId, descriptionDraft.value, descriptionRevision.value)
        descriptionEditing.value = false
        descriptionNotice.value = '简介已保存。'
        await catalog.reload()
      } catch (caught) {
        descriptionError.value = `${errorMessage(caught)}。输入内容已保留；如有冲突，请复制内容后刷新。`
      } finally {
        descriptionBusy.value = false
      }
    }

    async function searchCollection(): Promise<void> {
      const query = searchQuery.value.trim()
      const ticket = ++searchGeneration
      searchResult.value = null
      searchError.value = ''
      if (query === '') {
        searchError.value = '请输入要检索的问题或关键词。'
        return
      }
      searchBusy.value = true
      searchSubmitted.value = query
      try {
        const result = await api.searchCollection(collectionId, query)
        if (ticket === searchGeneration) searchResult.value = result
      } catch (caught) {
        if (ticket === searchGeneration) searchError.value = errorMessage(caught)
      } finally {
        if (ticket === searchGeneration) searchBusy.value = false
      }
    }

    // --- 软删（集合 / 文档共用一个受控确认框）-----------------------------
    const pendingDelete = ref<PendingDelete | null>(null)
    const deleting = ref(false)
    const deleteError = ref('')
    const deleteNotice = ref('')

    function askDeleteCollection(row: CollectionSummary): void {
      pendingDelete.value = { kind: 'collection', id: row.collection_id, label: row.name }
    }

    function askDeleteDocument(row: DocumentSummary): void {
      pendingDelete.value = {
        kind: 'document',
        id: row.document_id,
        label: row.source_path,
      }
    }

    async function confirmDelete(): Promise<void> {
      const target = pendingDelete.value
      if (target === null) {
        return
      }
      deleting.value = true
      deleteError.value = ''
      try {
        if (target.kind === 'collection') {
          await api.deleteCollection(target.id)
          searchGeneration++
          searchResult.value = null
          searchBusy.value = false
          deleteNotice.value =
            `已软删集合「${target.label}」：集合内文档立即退出检索与来源视图；` +
            '历史消息与引用不被改写，行也不被物理删除。' +
            '恢复方式：在同一项目下再次创建同名集合会复活原集合（沿用原集合 ID）；' +
            '集合内文档记录保留，但生效版本已退役，需重新摄取或重建索引后才重新参与检索。'
          await collections.reload()
        } else {
          await api.deleteDocument(target.id)
          searchGeneration++
          searchResult.value = null
          searchBusy.value = false
          deleteNotice.value =
            `已软删文档 ${target.label}：全部版本退出检索；` +
            '历史消息与引用不被改写，来源视图会按当前状态标注失效。' +
            '恢复方式：重新摄取同一路径会复活该文档（沿用原 document_id）；' +
	            '恢复可能需要重新解析与向量化，并消耗摄取配额。'
          // 文档数与本页集合行的统计来自 catalog，两个数据源都要刷新，
          // 否则删完文档后集合卡片上的文档数还停在旧值。
          await Promise.all([documents.reload(), catalog.reload()])
        }
        pendingDelete.value = null
      } catch (caught) {
        deleteError.value = errorMessage(caught)
      } finally {
        deleting.value = false
      }
    }

    // --- 写操作 -----------------------------------------------------------
    async function createCollection(event: Event): Promise<void> {
      event.preventDefault()
      createError.value = ''
      created.value = null
      const trimmedName = name.value.trim()
      if (trimmedName === '') {
        createError.value = '集合名称不得为空。'
        return
      }
      creating.value = true
      try {
        created.value = await api.createCollection({
          name: trimmedName,
          ...(project.value.trim() === '' ? {} : { project: project.value.trim() }),
        })
        name.value = ''
        project.value = ''
        await collections.reload()
      } catch (caught) {
        createError.value = errorMessage(caught)
      } finally {
        creating.value = false
      }
    }

    async function ingest(event: Event): Promise<void> {
      event.preventDefault()
      ingestError.value = ''
      ingested.value = null
      const path = ingestPath.value.trim()
      if (path === '') {
        ingestError.value = '文档路径必填：填写工作区允许根内的相对路径（仅 .md / .txt）。'
        return
      }
      ingesting.value = true
      try {
        ingested.value = await api.ingest(collectionId, path)
        ingestPath.value = ''
        // 新文档会让集合的文档数变化，catalog 与文档列表一起刷新。
        await Promise.all([documents.reload(), catalog.reload()])
      } catch (caught) {
        ingestError.value = errorMessage(caught)
      } finally {
        ingesting.value = false
      }
    }

    async function upload(): Promise<void> {
      const file = uploadFile.value
      if (!file || uploading.value) return
      uploading.value = true
      uploadNotice.value = ''
      uploadError.value = ''
      try {
        if (file.size > 20 * 1024 * 1024) throw new Error('文件超过 20 MiB；实际限额还可能受实例配置限制。')
        const content = new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer())
        const result = await api.upload(file.name, content)
        ingestPath.value = result.path
        uploadNotice.value = `已保存 ${result.path}。文件尚未索引，点击“摄取并索引”继续。`
        uploadFile.value = null
      } catch (caught) {
        uploadError.value = `${errorMessage(caught)}。未确认上传成功时，请先核对文件是否已保存。`
      } finally { uploading.value = false }
    }

    // --- 渲染片段 ---------------------------------------------------------
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

    function includeDeletedToggle(id: string, value: boolean, onChange: (next: boolean) => void): VNode {
      return h('label', { class: 'check-row', for: id }, [
        h('input', {
          id,
          type: 'checkbox',
          checked: value,
          onChange: (event: Event) => {
            onChange((event.target as HTMLInputElement).checked)
          },
        }),
        h('span', {}, '显示已软删的行（软删只标记，不物理删除）'),
      ])
    }

    function feedback(): VNodeChild[] {
      const nodes: VNodeChild[] = []
      if (deleteError.value !== '') {
        nodes.push(h(Notice, { tone: 'error', text: deleteError.value }))
      }
      if (deleteNotice.value !== '') {
        nodes.push(h(Notice, { tone: 'info', text: deleteNotice.value }))
      }
      return nodes
    }

    function createForm(): VNode {
      const children: VNodeChild[] = [
        h('h2', {}, '建集合'),
        h(
          'form',
          { class: 'collection-create-form', onSubmit: (event: Event) => { void createCollection(event) } },
          [
            textField('collection-name', '名称（必填）', name.value, (next) => {
              name.value = next
            }, '例如 运维手册'),
            textField('collection-project', '所属项目（留空 = 全局）', project.value, (next) => {
              project.value = next
            }, '例如 ops'),
            h('p', { class: 'field-hint muted' }, '同一项目下活动集合不得重名（409）；同名软删集合会被本操作复活并沿用原集合 ID；非法名 400，零写入。'),
            h('div', { class: 'form-actions' }, [
              h(
                'button',
                { id: 'collection-create', type: 'submit', class: 'primary', disabled: creating.value },
                creating.value ? '创建中…' : '创建集合',
              ),
            ]),
          ],
        ),
      ]
      if (createError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: createError.value }))
      }
      if (created.value !== null) {
        children.push(
          h(Notice, {
            tone: 'info',
            text:
              `已创建集合：${created.value.name}（${created.value.collection_id}）。` +
              '同项目同名软删集合会被本次操作复活并沿用原 ID（接口不回传是新建还是恢复）；' +
              '下一步：进入集合摄取文档。',
          }),
          h('p', {}, ['详情：', identifier(created.value.collection_id, collectionHref(created.value.collection_id))]),
        )
      }
      return h('section', { class: 'card' }, children)
    }

    function collectionsSection(): VNodeChild {
      const children: VNodeChild[] = [
        h('h2', {}, '集合'),
        h(
          'p',
          { class: 'muted small' },
	          '选择一个知识库查看其中的文档。',
        ),
        includeDeletedToggle('collections-include-deleted', includeDeleted.value, (next) => {
          includeDeleted.value = next
          void collections.reload()
        }),
      ]
      if (collections.error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: collections.error.value }))
      }
      const rows = collections.data.value
      if (rows === null) {
        if (collections.loading.value) {
          children.push(h('p', { class: 'muted' }, '加载中…'))
        }
      } else {
        children.push(
          h(DataTable, {
            tableClass: 'collections-table',
            columnClasses: ['col-name', '', 'col-short', 'col-number', 'col-time', 'col-time', 'col-short'],
            headers: ['名称', '简介', '项目', '文档数', '创建时间', '删除时间', '操作'],
            rows: rows.map((row) => {
              const deleted = row.deleted_at !== ''
              return {
                key: row.collection_id,
                cells: [
                  h('span', [h('a', { href: collectionHref(row.collection_id) }, row.name), deleted ? [' ', badge('已删除', 'muted')] : '']),
                  row.description || '暂无简介',
                  row.project === '' ? '—' : row.project,
                  row.document_count === undefined ? '—' : String(row.document_count),
                  formatTimestamp(row.created_at),
                  deleted ? formatTimestamp(row.deleted_at) : '—',
                  deleteButton(
                    `delete-collection-${row.collection_id}`,
                    '删除',
                    deleted,
                    () => { askDeleteCollection(row) },
                  ),
                ],
              }
            }),
            empty: '还没有集合；先建一个集合再摄取文档。',
          }),
        )
      }
      return h('section', { class: 'card' }, children)
    }

    function ingestForm(): VNode {
      const children: VNodeChild[] = [
        h('h2', {}, '摄取文档'),
        h('p', { class: 'field-hint muted' }, '可先上传 UTF-8 的 .md 或 .txt 文件；同名文件不会覆盖。上传成功后再摄取并索引。'),
        h('input', { id: 'knowledge-upload', type: 'file', accept: '.md,.txt,text/plain,text/markdown',
          onChange: (event: Event) => { uploadFile.value = (event.target as HTMLInputElement).files?.[0] ?? null } }),
        h('button', { type: 'button', disabled: uploading.value || uploadFile.value === null,
          onClick: () => { void upload() } }, uploading.value ? '上传中…' : '上传文件'),
        ...(uploadNotice.value ? [h(Notice, { tone: 'info', text: uploadNotice.value })] : []),
        ...(uploadError.value ? [h(Notice, { tone: 'error', text: uploadError.value })] : []),
        h(
          'form',
          { class: 'ingest-form', onSubmit: (event: Event) => { void ingest(event) } },
          [
            textField('ingest-path', '工作区相对路径（必填）', ingestPath.value, (next) => {
              ingestPath.value = next
            }, '例如 docs/runbook.md'),
            h(
              'p',
              { class: 'field-hint muted' },
              '只读取工作区允许根内的 .md / .txt：越界、扩展名不符或超上限 → 400 且零写入。' +
                '同 collection 内已有同哈希的 active 版本时不新建版本（deduplicated）。',
            ),
            h('div', { class: 'form-actions' }, [
              h(
                'button',
                { id: 'ingest-submit', type: 'submit', class: 'primary', disabled: ingesting.value },
                ingesting.value ? '摄取中…' : '摄取并索引',
              ),
            ]),
          ],
        ),
      ]
      if (ingestError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: ingestError.value }))
      }
      const result = ingested.value
      if (result !== null) {
        const notice = ingestNotice(result)
        children.push(h(Notice, { tone: notice.tone, text: notice.text }))
        children.push(
          h('p', {}, [
            '文档详情：',
            identifier(result.document_id, documentHref(result.document_id, collectionId)),
          ]),
        )
      }
      return h('section', { class: 'card' }, children)
    }

    function documentsSection(): VNodeChild {
      const children: VNodeChild[] = [
        h('h2', {}, '文档'),
        includeDeletedToggle('documents-include-deleted', docsIncludeDeleted.value, (next) => {
          docsIncludeDeleted.value = next
          void documents.reload()
        }),
      ]
      if (documents.error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: documents.error.value }))
      }
      const rows = documents.data.value
      if (rows === null) {
        if (documents.loading.value) {
          children.push(h('p', { class: 'muted' }, '加载中…'))
        }
      } else {
        children.push(
          h(DataTable, {
            tableClass: 'documents-table',
            columnClasses: ['col-command', 'col-state', 'col-hash', 'col-time', 'col-time', 'col-short'],
            headers: ['文档', '最新版本状态', '内容哈希', '更新时间', '删除时间', '操作'],
            rows: rows.map((row) => {
              const deleted = (row.deleted_at ?? '') !== ''
              return {
                key: row.document_id,
                cells: [
                  // 已软删的文档不给详情链接：版本接口按删除返回 404，点进去只能看到一个错误。
                  // Deleted documents get no detail link: the versions endpoint is 404 by design.
                  identifier(row.source_path, deleted ? undefined : documentHref(row.document_id, collectionId)),
                  documentStatusBadge(row.latest_status),
                  h('code', { class: 'cell-id', title: row.content_hash }, shortHash(row.content_hash)),
                  formatTimestamp(row.updated_at),
                  deleted ? formatTimestamp(row.deleted_at) : '—',
                  deleteButton(
                    `delete-document-${row.document_id}`,
                    '删除',
                    deleted,
                    () => { askDeleteDocument(row) },
                  ),
                ],
              }
            }),
            empty: '这个集合还没有文档；用上面的表单摄取。',
          }),
        )
        // 「最新版本状态」是该文档最近一次摄取 / 重建的结果，不等于正在服务的版本：
        // 本次版本失败时，若还有更早的 active 版本则它继续生效，没有就没有可检索内容。
        // 这一行把两种结局都写明，不把空知识库说成仍然可用。
        if (rows.some((row) => (row.deleted_at ?? '') === '')) {
          children.push(
            h(
              'p',
              { class: 'muted small' },
              '「最新版本状态」是最近一次摄取 / 重建的结果，不一定是正在服务的版本——' +
                '本次版本失败时，若该文档还有更早的 active 版本则它继续生效；没有时该文档暂时没有' +
                '可检索内容。具体以文档详情里的版本表为准。已软删的文档不再提供详情。',
            ),
          )
        }
      }
      return h('section', { class: 'card' }, children)
    }

    function collectionHeader(row: CollectionSummary): VNode {
      return h('section', { class: 'card' }, [
        h('h2', {}, '知识库简介'),
        descriptionEditing.value
          ? h('div', [
              h('label', { for: 'collection-description' }, '这个知识库包含什么、适用于哪些任务？'),
              h('textarea', { id: 'collection-description', value: descriptionDraft.value, maxlength: 2000,
                onInput: (event: Event) => { descriptionDraft.value = (event.target as HTMLTextAreaElement).value } }),
              h('button', { type: 'button', disabled: descriptionBusy.value, onClick: () => { void saveDescription() } }, '保存简介'),
              h('button', { type: 'button', disabled: descriptionBusy.value, onClick: () => { descriptionEditing.value = false } }, '取消'),
            ])
          : h('div', [h('p', {}, row.description || '暂无简介。'),
              ...(row.deleted_at === '' ? [h('button', { type: 'button', onClick: () => { editDescription(row) } }, '编辑简介')] : [])]),
        ...(descriptionError.value ? [h(Notice, { tone: 'error', text: descriptionError.value })] : []),
        ...(descriptionNotice.value ? [h(Notice, { tone: 'info', text: descriptionNotice.value })] : []),
        h('p', { class: 'muted small' }, '声明关联的 Agent：' + (row.declared_agents === undefined ? '当前服务未提供' : row.declared_agents.length ? row.declared_agents.join('、') : '暂无') + '。这是配置投影，不代表正在运行或当前调用方具备访问权限。'),
        definitions([
          ['集合', identifier(row.collection_id)],
          ['名称', row.name],
          ['项目', row.project === '' ? '—' : row.project],
          ['文档数', row.document_count === undefined ? '—（未统计）' : String(row.document_count)],
          ['创建时间', formatTimestamp(row.created_at)],
          ['删除时间', row.deleted_at === '' ? '—' : formatTimestamp(row.deleted_at)],
        ]),
      ])
    }

    function searchSection(): VNode {
      const children: VNodeChild[] = [
        h('h2', {}, '检索试算'),
        h('p', { class: 'muted small' }, '以当前管理台身份检索本集合。管理员能看到的内容不代表其他调用方或 Agent 一定有权访问。检索可能调用向量模型并产生费用。'),
        h('label', { class: 'field', for: 'knowledge-search-query' }, [
          h('span', {}, '问题或关键词'),
          h('input', {
            id: 'knowledge-search-query',
            value: searchQuery.value,
            onInput: (event: Event) => { searchQuery.value = (event.target as HTMLInputElement).value },
          }),
        ]),
        h('button', { id: 'knowledge-search', type: 'button', disabled: searchBusy.value, onClick: () => { void searchCollection() } },
          searchBusy.value ? '检索中…' : '检索'),
      ]
      if (searchError.value !== '') children.push(h(Notice, { tone: 'error', text: searchError.value }))
      const result = searchResult.value
      if (result !== null) {
        children.push(h('p', { class: 'muted small' }, `“${searchSubmitted.value}”的结果：${result.hits.length} 条；检索方式：${result.strategy}。`))
        if (result.generation !== undefined) children.push(h('p', { class: 'muted small' }, `索引代际：${result.generation}`))
        if (result.degraded_reason) children.push(h(Notice, { tone: 'warn', text: `检索已降级：${result.degraded_reason}` }))
        if (result.hits.length === 0) children.push(h('p', { class: 'muted' }, '当前身份未检索到结果。'))
        for (const hit of result.hits) {
          children.push(h('article', { class: 'card', key: hit.chunk_id }, [
            h('p', {}, `${hit.rank}. ${hit.source_path || hit.document_id}（分数 ${hit.score}）`),
            h('p', {}, hit.snippet),
            h('p', { class: 'muted small' }, `命中版本 ${hit.version_id}；片段仅供查看，不能直接作为引用。`),
            h('a', { href: documentHref(hit.document_id, collectionId) }, '打开文档（显示当前生效版本）'),
          ]))
        }
      }
      return h('section', { class: 'card', id: 'knowledge-search-panel' }, children)
    }

    function listPage(): VNode {
      return h('section', { class: 'view view-knowledge' }, [
        pageHeading(
          'KNOWLEDGE / COLLECTIONS',
          '知识库',
          '集合、文档与索引状态；检索与来源读取每次都按当前身份重查权限。',
        ),
        h(
          'p',
          { class: 'muted small' },
          '创建知识库后可以添加资料；Agent 需要在配置中声明引用才能使用。',
        ),
        ...feedback(),
        collectionsSection(),
        createForm(),
        deleteDialog(),
      ])
    }

    function detailPage(): VNode {
      const row = collection.value
      const heading = row === null ? '集合' : row.name
      const content: VNodeChild[] = [
        h('p', {}, [h('a', { href: knowledgeHref(), class: 'back-link' }, '← 返回集合列表')]),
        h(
          'p',
          { class: 'muted small' },
          '这里展示集合中的资料与处理状态；可以检索试算，检查相关内容是否能被找到。',
        ),
        ...feedback(),
      ]
      if (catalog.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: catalog.error.value }))
      }
      if (row !== null) {
        content.push(collectionHeader(row))
        if (row.deleted_at !== '') {
          content.push(
            h(Notice, {
              tone: 'warn',
              text: '集合已软删：集合内文档已退出检索与来源视图；行未被物理删除，历史消息与引用不被改写。',
            }),
          )
        }
        content.push(documentsSection())
        if (row.deleted_at === '') {
          content.push(searchSection())
          content.push(ingestForm())
        }
      } else if (catalog.loading.value && catalog.error.value === '') {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      } else if (catalog.error.value === '') {
        content.push(
          h(Notice, {
            tone: 'warn',
	            text: '没有这个集合，或当前账号没有查看权限。',
          }),
        )
      }
      content.push(deleteDialog())
      return h('section', { class: 'view view-knowledge' }, [
        pageHeading(
          'KNOWLEDGE / COLLECTION',
          heading,
          '文档与版本状态；摄取失败不影响已生效版本，但没有任何生效版本时该文档不会出现在检索里。',
        ),
        ...content,
      ])
    }

    function deleteDialog(): VNode {
      const target = pendingDelete.value
      const message = target === null
        ? ''
        : target.kind === 'collection'
          ? `软删集合「${target.label}」后，集合内全部文档立即退出检索与来源视图；历史消息与引用不被改写。恢复方式：在同一项目下再次创建同名集合（复活原集合 ID），再逐文档重新摄取或重建索引。`
	          : `软删文档 ${target.label} 后，它的全部版本退出检索；历史消息与引用不被改写，来源视图会标注失效。恢复方式：重新摄取同一路径（沿用原 document_id）；恢复可能需要重新解析与向量化，并消耗摄取配额。`
      return h(ConfirmDialog, {
        open: target !== null,
        title: '确认软删',
        message,
        confirmText: '软删',
        busy: deleting.value,
        onConfirm: () => { void confirmDelete() },
        onCancel: () => {
          if (!deleting.value) {
            pendingDelete.value = null
          }
        },
      })
    }

    return (): VNode => (listMode ? listPage() : detailPage())
  },
})
