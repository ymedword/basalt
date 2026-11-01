// US-16 知识库页面回归（T043）：集合列表 / 建集合 / 软删集合，集合内文档列表 /
// 摄取终态（deduplicated 与 failed）/ 软删文档，文档版本历史 / 重建索引，以及
// 「来源与引用」的精确读取、失效引用与游标续读。四条契约事实在页面上如实呈现：
// 没有单集合 / 单文档读取端点、失败版本不替换旧 active、失效引用不吐正文、
// 摘要与事实只在显式请求时出现。
// Knowledge pages: collections, documents, ingest outcomes, version history, and the
// exact source view. Invalid references never serve text; a failed version never
// replaces the active one.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type {
  CollectionSummary,
  DocumentSummary,
  DocumentVersionSummary,
  IngestResult,
  SourceViewRequest,
  SourceViewResult,
} from '../src/api-knowledge'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
  readonly query: URLSearchParams
  readonly body: unknown
}

type Route = (call: Call) => unknown

const COLLECTION_ID = 'col_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'
const ARCHIVED_COLLECTION_ID = 'col_01J8Z6YQ4M2S3T7V9W0X1Y2Z4B'
const DOC_ID = 'doc_01J8Z6YQ4M2S3T7V9W0X1Y2Z5C'
const DELETED_DOC_ID = 'doc_01J8Z6YQ4M2S3T7V9W0X1Y2Z6D'
const ACTIVE_VERSION = 'ver_01J8Z6YQ4M2S3T7V9W0X1Y2Z7E'
const FAILED_VERSION = 'ver_01J8Z6YQ4M2S3T7V9W0X1Y2Z8F'
const ACTIVE_HASH = 'a'.repeat(64)
const FAILED_HASH = 'b'.repeat(64)
const DOCUMENTS_PATH = `/api/v1/collections/${COLLECTION_ID}/documents`

const collection: CollectionSummary = {
  collection_id: COLLECTION_ID,
  name: '运维手册',
  project: 'ops',
  description: '故障排查和恢复流程',
  description_revision: 3,
  declared_agents: ['walkthrough-guide'],
  document_count: 2,
  created_at: '2026-10-20T08:00:00Z',
  deleted_at: '',
}

// document_count 缺席 = 适配器没统计；页面必须显示破折号而不是 0。
const archivedCollection: CollectionSummary = {
  collection_id: ARCHIVED_COLLECTION_ID,
  name: '旧档案',
  project: '',
  created_at: '2026-10-19T08:00:00Z',
  deleted_at: '2026-10-27T09:00:00Z',
}

const documentRow: DocumentSummary = {
  document_id: DOC_ID,
  collection_id: COLLECTION_ID,
  source_path: 'docs/runbook.md',
  content_hash: ACTIVE_HASH,
  latest_version_id: ACTIVE_VERSION,
  latest_status: 'active',
  created_at: '2026-10-26T08:00:00Z',
  updated_at: '2026-10-27T03:00:05Z',
  deleted_at: '',
}

const deletedDocument: DocumentSummary = {
  document_id: DELETED_DOC_ID,
  collection_id: COLLECTION_ID,
  source_path: 'docs/old.md',
  content_hash: FAILED_HASH,
  latest_version_id: FAILED_VERSION,
  latest_status: 'failed',
  created_at: '2026-10-25T08:00:00Z',
  updated_at: '2026-10-27T04:00:00Z',
  deleted_at: '2026-10-27T04:00:00Z',
}

// 新版本失败：列表按 created_at 倒序，失败行在前、active 行在后。
const failedVersion: DocumentVersionSummary = {
  version_id: FAILED_VERSION,
  document_id: DOC_ID,
  content_hash: FAILED_HASH,
  status: 'failed',
  revision: 18,
  generation: 0,
  error_kind: 'embed_failed',
  created_at: '2026-10-28T03:00:00Z',
  activated_at: '',
}

const activeVersion: DocumentVersionSummary = {
  version_id: ACTIVE_VERSION,
  document_id: DOC_ID,
  content_hash: ACTIVE_HASH,
  status: 'active',
  revision: 17,
  parser_version: 'md-1',
  chunker_version: 'chunk-1',
  generation: 3,
  created_at: '2026-10-27T03:00:00Z',
  activated_at: '2026-10-27T03:00:05Z',
}

const deduplicatedIngest: IngestResult = {
  document_id: DOC_ID,
  version_id: ACTIVE_VERSION,
  ingest_job_id: 'job_01J8Z6YQ4M2S3T7V9W0X1Y2Z9G',
  status: 'active',
  deduplicated: true,
  chunk_count: 12,
  generation: 3,
}

const failedIngest: IngestResult = {
  document_id: DOC_ID,
  version_id: FAILED_VERSION,
  ingest_job_id: 'job_01J8Z6YQ4M2S3T7V9W0X1Y2ZAG',
  status: 'failed',
  chunk_count: 0,
  generation: 0,
  error_kind: 'embed_failed',
}

const reindexed: IngestResult = {
  document_id: DOC_ID,
  version_id: 'ver_01J8Z6YQ4M2S3T7V9W0X1Y2ZBG',
  ingest_job_id: 'job_01J8Z6YQ4M2S3T7V9W0X1Y2ZCG',
  status: 'active',
  chunk_count: 9,
  generation: 4,
}

function validSource(): SourceViewResult {
  return {
    ref: { id: DOC_ID, version_id: ACTIVE_VERSION, content_hash: ACTIVE_HASH },
    validity: { valid: true, reason: '' },
    version_id: ACTIVE_VERSION,
    content_hash: ACTIVE_HASH,
    text: '巡检步骤：先看磁盘，再看证书。',
    range: { start: 0, end: 16 },
    truncated: false,
    parent_refs: [],
    facts: [],
  }
}

function invalidSource(): SourceViewResult {
  return {
    ref: { id: DOC_ID, version_id: FAILED_VERSION, content_hash: FAILED_HASH },
    validity: { valid: false, reason: 'version_changed' },
    version_id: FAILED_VERSION,
    text: '',
    truncated: false,
    parent_refs: [],
    facts: [],
  }
}

function envelope(data: unknown, status = 200, message = ''): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => ({
      code: status >= 200 && status < 300 ? 'OK' : 'FAILED',
      message,
      data: await Promise.resolve(data),
      timestamp: '2026-10-28T12:00:00Z',
    }),
  } as unknown as Response
}

function defaultReply(call: Call): unknown {
  if (call.path === '/api/v1/info') {
    return { version: 'test-version', uptime_seconds: 3600, agents: 0, tools: 0, mcp_servers: 0, providers: [] }
  }
  // 登录后的默认路由是总览页；不提供它的读数会留下未处理的拒绝。
  if (call.path === '/api/v1/system/overview') {
    return {
      version: 'test-version',
      uptime_seconds: 3600,
      started_at: '2026-10-28T00:00:00Z',
      agents: { total: 0, active: 0, disabled: 0, archived: 0, invalid: 0 },
      runs: { running: 0, last_24h: 0, failed_last_24h: 0 },
      resources: { model_connections: 0, mcp_servers: 0, skills: 0 },
      database: { status: 'ok', schema_version: 3 },
      auth: { mode: 'token', loopback_only: true },
    }
  }
  if (call.path === '/api/v1/system/sample') {
    return {
      sampled_at: '2026-10-28T00:00:00Z',
      cpu_percent: null,
      memory_rss_bytes: 1024,
      goroutines: 12,
      unavailable: [],
    }
  }
  if (call.path === '/api/v1/collections') {
    if (call.method === 'POST') {
      const body = call.body as { name: string; project?: string }
      return {
        collection_id: 'col_01J8Z6YQ4M2S3T7V9W0X1Y2ZD1',
        name: body.name,
        project: body.project ?? '',
        created_at: '2026-10-28T12:00:00Z',
        deleted_at: '',
      }
    }
    return [collection, archivedCollection]
  }
  if (call.path === DOCUMENTS_PATH) {
    if (call.method === 'POST') {
      return deduplicatedIngest
    }
    return [documentRow, deletedDocument]
  }
  if (call.path === '/api/v1/admin/knowledge/uploads') {
    return { path: 'uploads/a.md' }
  }
  if (call.path === `/api/v1/documents/${DOC_ID}/versions`) {
    return [failedVersion, activeVersion]
  }
  if (call.path === `/api/v1/documents/${DOC_ID}/reindex`) {
    return reindexed
  }
  if (call.path === `/api/v1/admin/knowledge/documents/${DOC_ID}/ingest-attempts`) {
    return { items: [{ job_id: 'job-a', version_id: FAILED_VERSION, attempt: 2, stage: 'embed', status: 'failed',
      error_kind: 'embed_failed', started_at: '2026-10-28T03:00:00Z', finished_at: '2026-10-28T03:00:05Z' }], truncated: true }
  }
  if (call.path === '/api/v1/sources/view') {
    const body = call.body as SourceViewRequest
    return body.ref.version_id === FAILED_VERSION ? invalidSource() : validSource()
  }
  return []
}

let wrapper: ReturnType<typeof mount> | undefined
let calls: Call[] = []

async function signIn(route: Route = () => undefined): Promise<void> {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const call: Call = {
      method: (init?.method ?? 'GET').toUpperCase(),
      path: url.pathname,
      query: url.searchParams,
      body: init?.body === undefined ? null : JSON.parse(String(init.body)) as unknown,
    }
    calls.push(call)
    const routed = route(call)
    return envelope(routed === undefined ? defaultReply(call) : routed)
  }))
  window.history.replaceState(null, '', '/console/login')
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

async function visit(path: string): Promise<void> {
  window.history.pushState(null, '', path)
  window.dispatchEvent(new PopStateEvent('popstate'))
  await flushPromises()
}

function writes(method: string, path: string): Call[] {
  return calls.filter((call) => call.method === method && call.path === path)
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('knowledge collections', () => {
  it('lists collections with counts, omitted statistics and the deleted marker', async () => {
    await signIn()
    await visit('/console/knowledge')

    const headers = wrapper!.findAll('.collections-table thead th').map((cell) => cell.text())
    expect(headers).toEqual(['名称', '简介', '项目', '文档数', '创建时间', '删除时间', '操作'])
    expect(wrapper!.findAll('.collections-table tbody a').map((cell) => cell.attributes('href'))).toEqual([
      `/console/knowledge/${COLLECTION_ID}`,
      `/console/knowledge/${ARCHIVED_COLLECTION_ID}`,
    ])

    const rows = wrapper!.findAll('.collections-table tbody tr')
    expect(rows[0]!.findAll('td')[0]!.text()).toBe('运维手册')
    expect(rows[0]!.findAll('td')[1]!.text()).toBe('故障排查和恢复流程')
    expect(rows[0]!.findAll('td')[3]!.text()).toBe('2')
    // 项目空串与"没统计"各显示破折号；不能把统计缺席显示成 0。
    expect(rows[1]!.findAll('td')[2]!.text()).toBe('—')
    expect(rows[1]!.findAll('td')[3]!.text()).toBe('—')
    expect(rows[1]!.text()).toContain('已删除')
    expect(wrapper!.text()).toContain('创建知识库后可以添加资料')
  })

  it('sends include_deleted only when the operator asks for it', async () => {
    await signIn()
    await visit('/console/knowledge')

    const before = calls.filter((call) => call.method === 'GET' && call.path === '/api/v1/collections')
    expect(before.at(-1)?.query.get('include_deleted')).toBeNull()

    await wrapper!.find('#collections-include-deleted').setValue(true)
    await flushPromises()

    const after = calls.filter((call) => call.method === 'GET' && call.path === '/api/v1/collections')
    expect(after.at(-1)?.query.get('include_deleted')).toBe('true')
  })

  it('refuses an empty name, then creates with the trimmed name and optional project', async () => {
    await signIn()
    await visit('/console/knowledge')

    await wrapper!.find('.collection-create-form').trigger('submit')
    await flushPromises()
    expect(writes('POST', '/api/v1/collections')).toHaveLength(0)
    expect(wrapper!.text()).toContain('集合名称不得为空')

    await wrapper!.find('#collection-name').setValue('  发布规范  ')
    await wrapper!.find('.collection-create-form').trigger('submit')
    await flushPromises()
    const [first] = writes('POST', '/api/v1/collections')
    expect(first?.body).toEqual({ name: '发布规范' })
    expect(wrapper!.text()).toContain('已创建集合')

    await wrapper!.find('#collection-name').setValue('发布规范')
    await wrapper!.find('#collection-project').setValue('ops')
    await wrapper!.find('.collection-create-form').trigger('submit')
    await flushPromises()
    const sent = writes('POST', '/api/v1/collections')
    expect(sent.at(-1)?.body).toEqual({ name: '发布规范', project: 'ops' })
  })

  it('soft-deletes a collection only after explicit confirmation', async () => {
    await signIn()
    await visit('/console/knowledge')

    await wrapper!.find(`#delete-collection-${COLLECTION_ID}`).trigger('click')
    await flushPromises()
    expect(writes('DELETE', `/api/v1/collections/${COLLECTION_ID}`)).toHaveLength(0)
    expect(wrapper!.text()).toContain('软删集合「运维手册」')

    await wrapper!.find('.dialog button.primary').trigger('click')
    await flushPromises()
    expect(writes('DELETE', `/api/v1/collections/${COLLECTION_ID}`)).toHaveLength(1)
    expect(wrapper!.text()).toContain('已软删集合')
  })
})

describe('knowledge documents', () => {
  it('uploads a source without silently starting indexing', async () => {
    await signIn()
    await visit(`/console/knowledge/${COLLECTION_ID}`)
    const file = { name: 'a.md', size: 3, arrayBuffer: async () => new TextEncoder().encode('abc').buffer }
    const input = wrapper!.find('#knowledge-upload')
    Object.defineProperty(input.element, 'files', { configurable: true, value: [file] })
    await input.trigger('change')
    const button = wrapper!.findAll('button').find((entry) => entry.text() === '上传文件')!
    await button.trigger('click')
    await flushPromises()
    expect(writes('POST', '/api/v1/admin/knowledge/uploads')[0]?.body).toEqual({ name: 'a.md', content: 'abc' })
    expect(writes('POST', DOCUMENTS_PATH)).toHaveLength(0)
    expect((wrapper!.find('#ingest-path').element as HTMLInputElement).value).toBe('uploads/a.md')
    expect(wrapper!.text()).toContain('文件尚未索引')
  })
  it('shows and edits the collection description using its observed revision', async () => {
    await signIn()
    await visit(`/console/knowledge/${COLLECTION_ID}`)
    expect(wrapper!.text()).toContain('故障排查和恢复流程')
    expect(wrapper!.text()).toContain('声明关联的 Agent：walkthrough-guide')
    expect(wrapper!.text()).toContain('不代表正在运行')
    const edit = wrapper!.findAll('button').find((button) => button.text() === '编辑简介')
    expect(edit).toBeDefined()
    await edit!.trigger('click')
    await wrapper!.find('#collection-description').setValue('部署与恢复说明')
    const save = wrapper!.findAll('button').find((button) => button.text() === '保存简介')
    await save!.trigger('click')
    await flushPromises()
    expect(writes('PUT', `/api/v1/admin/knowledge/collections/${COLLECTION_ID}/description`)[0]?.body).toEqual({
      description: '部署与恢复说明', expected_revision: 3,
    })
    expect(wrapper!.text()).toContain('简介已保存')
  })
  it('tests retrieval only in the selected collection and labels identity and degraded results', async () => {
    await signIn((call) => call.path === '/api/v1/search'
      ? {
          hits: [{ chunk_id: 'chunk-1', document_id: DOC_ID, version_id: ACTIVE_VERSION,
            collection_id: COLLECTION_ID, source_path: 'docs/runbook.md', rank: 1, score: 0.8, snippet: '命中片段' }],
          denied_count: 2, strategy: 'keyword', degraded_reason: 'vector_unavailable',
        }
      : undefined)
    await visit(`/console/knowledge/${COLLECTION_ID}`)
    const panel = wrapper!.find('#knowledge-search-panel')
    expect(panel.text()).toContain('管理员能看到的内容不代表其他调用方')
    await panel.find('#knowledge-search').trigger('click')
    expect(writes('POST', '/api/v1/search')).toHaveLength(0)
    await panel.find('#knowledge-search-query').setValue('  巡检  ')
    await panel.find('#knowledge-search').trigger('click')
    await flushPromises()
    expect(writes('POST', '/api/v1/search')[0]?.body).toEqual({ query: '巡检', collections: [COLLECTION_ID] })
    expect(panel.text()).toContain('命中片段')
    expect(panel.text()).toContain('vector_unavailable')
    expect(panel.text()).toContain('片段仅供查看，不能直接作为引用')
    expect(panel.find('a').attributes('href')).toContain(`/documents/${DOC_ID}`)
    expect(panel.text()).not.toContain('2 条因权限')
  })

  it('lists documents and reports both the deduplicated and the failed ingest outcomes', async () => {
    let reply: IngestResult = deduplicatedIngest
    await signIn((call) => (call.path === DOCUMENTS_PATH && call.method === 'POST' ? reply : undefined))
    await visit(`/console/knowledge/${COLLECTION_ID}`)

    // 表头写「最新版本状态」而不是笼统的「最新状态」：该列是最近一次摄取 / 重建的
    // 结果，不代表正在服务的版本；下面的小字说明两者关系（2026-10-31 文案修正）。
    const headers = wrapper!.findAll('.documents-table thead th').map((cell) => cell.text())
    expect(headers).toEqual(['文档', '最新版本状态', '内容哈希', '更新时间', '删除时间', '操作'])
    expect(wrapper!.text()).toContain('不一定是正在服务的版本')
    const rows = wrapper!.findAll('.documents-table tbody tr')
    expect(rows[0]!.findAll('td')[1]!.text()).toBe('生效中')
    expect(rows[1]!.findAll('td')[1]!.text()).toBe('失败')
    expect(rows[1]!.findAll('td')[4]!.text()).not.toBe('—')

    await wrapper!.find('.ingest-form').trigger('submit')
    await flushPromises()
    expect(writes('POST', DOCUMENTS_PATH)).toHaveLength(0)
    expect(wrapper!.text()).toContain('文档路径必填')

    await wrapper!.find('#ingest-path').setValue('docs/runbook.md')
    await wrapper!.find('.ingest-form').trigger('submit')
    await flushPromises()
    const [posted] = writes('POST', DOCUMENTS_PATH)
    expect(posted?.body).toEqual({ path: 'docs/runbook.md' })
    expect(wrapper!.text()).toContain('未新建版本')

    reply = failedIngest
    await wrapper!.find('#ingest-path').setValue('docs/runbook.md')
    await wrapper!.find('.ingest-form').trigger('submit')
    await flushPromises()
    expect(wrapper!.text()).toContain('向量化失败')
    // 失败提示必须区分两种情况：此前有生效版本 → 旧版本继续服务；首次摄取失败
    // → 该文档暂时没有可检索内容（不是「旧版本继续」）。
    expect(wrapper!.text()).toContain('若该文档此前已有生效版本，它继续服务')
    expect(wrapper!.text()).toContain('首次摄取失败时该文档暂时没有可检索内容')

    await wrapper!.find('#documents-include-deleted').setValue(true)
    await flushPromises()
    const reads = calls.filter((call) => call.method === 'GET' && call.path === DOCUMENTS_PATH)
    expect(reads.at(-1)?.query.get('include_deleted')).toBe('true')
  })

  it('says a missing collection is not readable instead of inventing one', async () => {
    await signIn((call) =>
      call.path === '/api/v1/collections' ? [] : undefined)
    await visit('/console/knowledge/col_01J8Z6YQ4M2S3T7V9W0X1Y2ZEX')

    expect(wrapper!.text()).toContain('没有这个集合')
    expect(wrapper!.text()).not.toContain('摄取并索引')
  })
})

describe('knowledge document detail', () => {
  async function openDocument(route: Route = () => undefined): Promise<void> {
    await signIn(route)
    await visit(`/console/knowledge/documents/${DOC_ID}?collection=${COLLECTION_ID}`)
  }

  it('reads an explicitly selected superseded version as historical, not a valid citation', async () => {
    const previous = { ...failedVersion, status: 'superseded', error_kind: undefined } satisfies DocumentVersionSummary
    await openDocument((call) => {
      if (call.path === `/api/v1/documents/${DOC_ID}/versions`) return [activeVersion, previous]
      if (call.path === '/api/v1/admin/knowledge/historical-source') {
        return { version_id: FAILED_VERSION, content_hash: FAILED_HASH, text: '当时的说明', truncated: false, historical: true }
      }
      return undefined
    })
    await wrapper!.find('#source-version').setValue(FAILED_VERSION)
    await wrapper!.find('#historical-source-read').trigger('click')
    await flushPromises()
    expect(writes('POST', '/api/v1/admin/knowledge/historical-source')[0]?.body).toEqual({
      ref: { id: DOC_ID, version_id: FAILED_VERSION, content_hash: FAILED_HASH },
    })
    expect(wrapper!.text()).toContain('当时的说明')
    expect(wrapper!.text()).toContain('不能作为当前有效引用')
    await wrapper!.find('#source-version').setValue(ACTIVE_VERSION)
    expect(wrapper!.text()).not.toContain('当时的说明')
  })

  it('shows newest-first version history with the failed version and its reason', async () => {
    await openDocument()

    const headers = wrapper!.findAll('.versions-table thead th').map((cell) => cell.text())
    expect(headers).toEqual(['版本', '修订序号', '内容哈希', '状态', '索引代际', '解析器 / 分块器', '创建时间', '激活时间', '失败原因'])
    const rows = wrapper!.findAll('.versions-table tbody tr')
    expect(rows[0]!.findAll('td')[1]!.text()).toBe('18')
    expect(rows[0]!.findAll('td')[2]!.text()).toBe(FAILED_HASH)
    expect(rows[0]!.findAll('td')[3]!.text()).toBe('失败')
    expect(rows[0]!.findAll('td')[8]!.text()).toContain('向量化失败')
    expect(rows[1]!.findAll('td')[3]!.text()).toBe('生效中')
    expect(rows[1]!.findAll('td')[4]!.text()).toBe('3')
    expect(rows[1]!.findAll('td')[5]!.text()).toBe('md-1 / chunk-1')
    expect(rows[1]!.findAll('td')[8]!.text()).toBe('—')

    const back = wrapper!.find('a.back-link')
    expect(back.attributes('href')).toBe(`/console/knowledge/${COLLECTION_ID}`)
    expect(wrapper!.text()).toContain('正文按当前身份读取')
    expect(wrapper!.find('h1').text()).toContain('docs/runbook.md')
    expect(wrapper!.find('#ingest-attempts').text()).toContain('向量化失败')
    expect(wrapper!.find('#ingest-attempts').text()).toContain('仅显示最近 100 条')
  })

  it('reads the active version exactly and reports an invalid reference without any body', async () => {
    await openDocument()

    // 默认读 active，不是最新创建的失败版本。
    const select = wrapper!.find('#source-version').element as HTMLSelectElement
    expect(select.value).toBe(ACTIVE_VERSION)

    await wrapper!.find('#source-view-submit').trigger('click')
    await flushPromises()
    const requests = writes('POST', '/api/v1/sources/view')
    expect(requests.at(-1)?.body).toEqual({
      ref: { id: DOC_ID, version_id: ACTIVE_VERSION, content_hash: ACTIVE_HASH },
    })
    expect(wrapper!.text()).toContain('巡检步骤')
    expect(wrapper!.text()).toContain('按当前权限、删除状态与版本核对通过')

    await wrapper!.find('#source-version').setValue(FAILED_VERSION)
    await wrapper!.find('#source-view-submit').trigger('click')
    await flushPromises()
    const sent = writes('POST', '/api/v1/sources/view')
    expect((sent.at(-1)?.body as SourceViewRequest).ref.version_id).toBe(FAILED_VERSION)
    expect(wrapper!.text()).toContain('引用失效')
    expect(wrapper!.text()).toContain('版本已变更')
    expect(wrapper!.text()).toContain('不得用新版本的内容冒充旧引用')
    // 失效视图不渲染正文：旧页面的正文已被清掉，不能用它冒充这一版。
    expect(wrapper!.text()).not.toContain('巡检步骤')
  })

  it('previews the active version in bounded pages and rechecks validity for each page', async () => {
    await openDocument((call) => {
      if (call.path !== '/api/v1/sources/view') return undefined
      const body = call.body as SourceViewRequest
      return body.cursor === undefined
        ? { ...validSource(), text: '第一页正文', truncated: true, next_cursor: 'preview_2' }
        : { ...invalidSource(), text: '不得显示的旧正文' }
    })
    const preview = wrapper!.find('#document-preview')
    expect(preview.text()).toContain('第一页正文')
    expect(writes('POST', '/api/v1/sources/view')[0]?.body).toEqual({
      ref: { id: DOC_ID, version_id: ACTIVE_VERSION, content_hash: ACTIVE_HASH },
      budget_tokens: 2048,
    })
    await preview.find('#document-preview-next').trigger('click')
    await flushPromises()
    expect((writes('POST', '/api/v1/sources/view').at(-1)?.body as SourceViewRequest).cursor).toBe('preview_2')
    expect(preview.text()).toContain('正文不可用')
    expect(preview.text()).not.toContain('第一页正文')
    expect(preview.text()).not.toContain('不得显示的旧正文')
  })

  it('discards a delayed source response when the selected version changes', async () => {
    let finish!: (value: SourceViewResult) => void
    const delayed = new Promise<SourceViewResult>((resolve) => { finish = resolve })
    await openDocument((call) => {
      if (call.path !== '/api/v1/sources/view') return undefined
      const body = call.body as SourceViewRequest
      return body.budget_tokens === undefined ? delayed : validSource()
    })
    await wrapper!.find('#source-view-submit').trigger('click')
    await wrapper!.find('#source-version').setValue(FAILED_VERSION)
    finish({ ...validSource(), text: '旧请求返回的内容' })
    await flushPromises()
    expect(wrapper!.text()).not.toContain('旧请求返回的内容')
    expect((wrapper!.find('#source-view-submit').element as HTMLButtonElement).disabled).toBe(false)
  })

  it('continues a truncated read with the returned cursor and drops the cursor when done', async () => {
    const firstPage: SourceViewResult = {
      ...validSource(),
      text: '前半段正文',
      truncated: true,
      next_cursor: 'cur_2',
    }
    const secondPage: SourceViewResult = {
      ...validSource(),
      text: '后半段正文',
      range: undefined,
      truncated: false,
    }
    await openDocument((call) => {
      if (call.path !== '/api/v1/sources/view') {
        return undefined
      }
      const body = call.body as SourceViewRequest
      return body.cursor === undefined ? firstPage : secondPage
    })

    await wrapper!.find('#source-view-submit').trigger('click')
    await flushPromises()
    expect(wrapper!.text()).toContain('前半段正文')
    expect(wrapper!.find('#source-view-more').exists()).toBe(true)

    await wrapper!.find('#source-view-more').trigger('click')
    await flushPromises()
    const sent = writes('POST', '/api/v1/sources/view')
    const continuation = sent.at(-1)?.body as SourceViewRequest
    expect(continuation.cursor).toBe('cur_2')
    expect('range' in continuation).toBe(false)
    expect(wrapper!.text()).toContain('后半段正文')
    expect(wrapper!.find('#source-view-more').exists()).toBe(false)
  })

  it('reindexes into a new version and soft-deletes behind confirmations', async () => {
    let versionReads = 0
    await openDocument((call) => {
      if (call.path === `/api/v1/documents/${DOC_ID}/versions`) {
        versionReads += 1
        return [failedVersion, activeVersion]
      }
      return undefined
    })
    const readsBefore = versionReads

    await wrapper!.find('#document-reindex').trigger('click')
    await flushPromises()
    expect(writes('POST', `/api/v1/documents/${DOC_ID}/reindex`)).toHaveLength(1)
    expect(versionReads).toBe(readsBefore + 1)
    expect(wrapper!.text()).toContain('已生效：版本 ver_01J8Z6YQ4M2S3T7V9W0X1Y2ZBG')

    await wrapper!.find('#document-delete').trigger('click')
    await flushPromises()
    expect(writes('DELETE', `/api/v1/documents/${DOC_ID}`)).toHaveLength(0)
    expect(wrapper!.text()).toContain('确认软删文档')

    await wrapper!.find('.dialog button.primary').trigger('click')
    await flushPromises()
    expect(writes('DELETE', `/api/v1/documents/${DOC_ID}`)).toHaveLength(1)
    expect(wrapper!.text()).toContain('已软删文档')
    expect((wrapper!.find('#document-delete').element as HTMLButtonElement).disabled).toBe(true)
  })
})
