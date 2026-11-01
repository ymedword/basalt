// 知识库 API（US-16 §3.1.9 / T043）：知识页用到的九个契约端点，字段名与
// `docs/contracts/openapi.yaml` 的知识组逐字一致。三条契约事实决定页面形态：
//
//  1. 契约**没有**单集合 / 单文档读取端点——集合行从列表里找，文档摘要从集合的
//     文档列表取；页面不发明"读取一个"的路径。
//  2. 检索命中（`SearchHit`）**不带** `content_hash`，因此命中在签名层面就不能当
//     引用依据；引用必须走 `POST /sources/view` 精确读取。管理台知识页按设计
//     §3.1.9 只覆盖「集合 / 文档 / 版本 / 失败处理 / 来源与引用」，**不**提供检索
//     面板只展示试算结果；查看原文须另走来源精确读取。
//  3. `SourceViewResult` 在 `validity.valid=false` 时正文、哈希、事实与摘要一律
//     为空；页面不得用新版本的正文冒充旧引用。
//
// Knowledge client: one method per endpoint the knowledge page uses. The contract has
// no single-collection / single-document read, hits never carry a content hash (so a
// hit can never act as a citation), and an invalid source view serves no content.
import type { ApiClient } from './api'

/** 版本状态六值（契约 `DocumentVersionStatus`）；只有 `active` 参与检索。 */
export type DocumentVersionStatus =
  | 'pending'
  | 'indexed'
  | 'active'
  | 'superseded'
  | 'retired'
  | 'failed'

/** 来源视图失效原因（契约 `ValidityView.reason`）；`valid=true` 时为空串。 */
export type SourceValidityReason = '' | 'revoked' | 'deleted' | 'version_changed' | 'not_found'

/** 摄取失败类别（契约 `IngestResult.error_kind`）；仅 `failed` 时非空。 */
export type IngestErrorKind =
  | 'path_rejected'
  | 'decode_failed'
  | 'parser_failed'
  | 'chunker_failed'
  | 'embed_failed'
  | 'verify_failed'

/**
 * `CollectionSummary`：`document_count` 缺省表示适配器没统计——0 是"确实为空"，
 * 两者不得混同；`deleted_at` 空串表示未删除。
 * A missing document_count means "not counted"; 0 is a real answer.
 */
export interface CollectionSummary {
  collection_id: string
  name: string
  project: string
  description?: string
  description_revision?: number
  declared_agents?: string[]
  document_count?: number
  created_at: string
  deleted_at: string
}

/** `DocumentSummary`：`latest_status` 缺省表示尚无版本（空串不在六值枚举内）。 */
export interface DocumentSummary {
  document_id: string
  collection_id: string
  source_path: string
  content_hash: string
  latest_version_id?: string
  latest_status?: DocumentVersionStatus
  created_at: string
  updated_at: string
  deleted_at?: string
}

/** `IngestResult`：`deduplicated=true` 表示同哈希内容已存在，本次不新建版本。 */
export interface IngestResult {
  document_id: string
  version_id: string
  ingest_job_id: string
  status: DocumentVersionStatus
  deduplicated?: boolean
  chunk_count?: number
  generation?: number
  error_kind?: IngestErrorKind
}

/**
 * `DocumentVersionSummary`：普通摄取可按内容哈希去重；显式重建会创建独立版本，
 * 即使内容未变化。旧版本保留；版本行存在本身不代表已有回滚能力。
 */
export interface DocumentVersionSummary {
  version_id: string
  document_id: string
  revision?: number
  content_hash: string
  status: DocumentVersionStatus
  parser_version?: string
  chunker_version?: string
  generation?: number
  error_kind?: IngestErrorKind
  created_at: string
  activated_at: string
}

/** `CollectionCreateRequest`：`name` 必填，`project` 缺省为空串（全局）。 */
export interface CollectionCreateRequest {
  name: string
  project?: string
}

/** `DocumentIngestRequest`：工作区允许根内的相对路径，仅 `.md` / `.txt`。 */
export interface DocumentIngestRequest {
  path: string
}

export interface KnowledgeSearchHit {
  chunk_id: string
  document_id: string
  version_id: string
  collection_id: string
  rank: number
  score: number
  snippet: string
  source_path?: string
  heading_path?: string
}

export interface KnowledgeSearchResult {
  hits: KnowledgeSearchHit[]
  denied_count: number
  strategy: string
  degraded_reason?: string
  generation?: number
}

/** `EvidenceRef`：不含正文；`content_hash` 可缺省，给出来时用于"可定位"判定。 */
export interface EvidenceRef {
  id: string
  version_id: string
  content_hash?: string
}

/** `EvidenceRange`：字符偏移，左闭右开；越界由服务端 400，不静默截断。 */
export interface EvidenceRange {
  start: number
  end: number
}

/** `SourceViewRequest`：身份只在请求上下文里，**不放** Principal。 */
export interface SourceViewRequest {
  ref: EvidenceRef
  range?: EvidenceRange
  budget_tokens?: number
  cursor?: string
  include_facts?: boolean
  include_summary?: boolean
}

/** `ValidityView`：按当前 ACL / 删除 / 版本重查的结论。 */
export interface ValidityView {
  valid: boolean
  reason: SourceValidityReason
}

/** `SourceFact`：值由端口保证已脱敏。 */
export interface SourceFact {
  key: string
  value: string
}

/** `SourceSummary`：只描述来源本身，不含任务进度。 */
export interface SourceSummary {
  document_title?: string
  heading_path?: string
  section_summary?: string
}

/**
 * `SourceViewResult`：必填字段恒在；`validity.valid=false` 时 `text` 为空串，
 * `content_hash` / `range` / `next_cursor` / `conversion` / `summary` 缺省。
 * An invalid view keeps the identity and validity but serves no content.
 */
export interface SourceViewResult {
  ref: EvidenceRef
  validity: ValidityView
  version_id?: string
  content_hash?: string
  text: string
  range?: EvidenceRange
  truncated: boolean
  next_cursor?: string
  conversion?: string
  parent_refs: EvidenceRef[]
  facts: SourceFact[]
  summary?: SourceSummary
}

export interface HistoricalSourceResult {
  version_id: string
  content_hash: string
  text: string
  truncated: boolean
  next_cursor?: string
  historical: true
}

export interface IngestAttempt {
  job_id: string
  version_id: string
  attempt: number
  stage: string
  status: string
  error_kind?: string
  started_at: string
  finished_at: string
}

/**
 * KnowledgeApi 是知识页的唯一入口：方法名与契约路径同构，路径参数一律
 * `encodeURIComponent`；不补默认值、不重试、不自行排序。
 * One method per contract endpoint the page uses; path segments are always encoded.
 */
export class KnowledgeApi {
  readonly #client: ApiClient

  constructor(client: ApiClient) {
    this.#client = client
  }

  upload(name: string, content: string): Promise<{ path: string }> {
    return this.#client.request('POST', '/admin/knowledge/uploads', { body: { name, content } })
  }

  updateDescription(collectionId: string, description: string, expectedRevision: number): Promise<{ description_revision: number }> {
    return this.#client.request('PUT', `/admin/knowledge/collections/${encodeURIComponent(collectionId)}/description`, {
      body: { description, expected_revision: expectedRevision },
    })
  }

  historicalSource(ref: EvidenceRef, cursor?: string): Promise<HistoricalSourceResult> {
    return this.#client.request('POST', '/admin/knowledge/historical-source', {
      body: { ref, ...(cursor ? { cursor } : {}) },
    })
  }

  ingestAttempts(documentId: string): Promise<{ items: IngestAttempt[]; truncated: boolean }> {
    return this.#client.request('GET', `/admin/knowledge/documents/${encodeURIComponent(documentId)}/ingest-attempts`)
  }

  searchCollection(collectionId: string, query: string): Promise<KnowledgeSearchResult> {
    return this.#client.request<KnowledgeSearchResult>('POST', '/search', {
      body: { query, collections: [collectionId] },
    })
  }

  /** 列集合（服务端固定按 `name` 升序；软删集合默认不返回）。 */
  listCollections(
    options: { project?: string; includeDeleted?: boolean } = {},
  ): Promise<CollectionSummary[]> {
    return this.#client.request<CollectionSummary[]>('GET', '/collections', {
      query: {
        project: options.project,
        include_deleted: options.includeDeleted === true ? 'true' : undefined,
      },
    })
  }

  /**
   * 建集合；同项目下活动集合同名 → 409，非法名 → 400。
   * 同项目同名软删行会被本操作复活并返回原 collection_id（响应不区分新建与恢复）。
   */
  createCollection(body: CollectionCreateRequest): Promise<CollectionSummary> {
    return this.#client.request<CollectionSummary>('POST', '/collections', { body })
  }

  /**
   * 软删集合（幂等）：集合内文档立即退出检索与来源视图，历史消息不被改写。
   * 恢复走同名创建（沿用原 collection_id）；集合内文档记录保留但版本已退役，
   * 需重新摄取或重建索引。
   */
  deleteCollection(collectionId: string): Promise<void> {
    return this.#client.requestOK('DELETE', `/collections/${encodeURIComponent(collectionId)}`)
  }

  /** 列集合内文档（固定按 `source_path` 升序）；软删文档默认不返回。 */
  listDocuments(collectionId: string, includeDeleted = false): Promise<DocumentSummary[]> {
    return this.#client.request<DocumentSummary[]>(
      'GET',
      `/collections/${encodeURIComponent(collectionId)}/documents`,
      { query: { include_deleted: includeDeleted ? 'true' : undefined } },
    )
  }

  /** 摄取按工作区相对路径读取的文档；同哈希内容返回既有版本并置 `deduplicated`。 */
  ingest(collectionId: string, path: string): Promise<IngestResult> {
    return this.#client.request<IngestResult>(
      'POST',
      `/collections/${encodeURIComponent(collectionId)}/documents`,
      { body: { path } satisfies DocumentIngestRequest },
    )
  }

  /**
   * 软删文档（幂等）：全部版本退出检索，历史引用按当前状态标注失效。
   * 恢复走同一路径重新摄取（沿用原 document_id）；历史版本保留。
   */
  deleteDocument(documentId: string): Promise<void> {
    return this.#client.requestOK('DELETE', `/documents/${encodeURIComponent(documentId)}`)
  }

  /** 列文档版本（固定按 `created_at` 倒序）。 */
  listVersions(documentId: string): Promise<DocumentVersionSummary[]> {
    return this.#client.request<DocumentVersionSummary[]>(
      'GET',
      `/documents/${encodeURIComponent(documentId)}/versions`,
    )
  }

  /**
   * 重建索引：按存储的来源路径重新处理并创建独立版本，成功前旧版本继续服务。
   */
  reindex(documentId: string): Promise<IngestResult> {
    return this.#client.request<IngestResult>(
      'POST',
      `/documents/${encodeURIComponent(documentId)}/reindex`,
    )
  }

  /** 来源视图：按 `EvidenceRef` 精确读取，不经过相似度；每次按当前身份重查。 */
  sourceView(body: SourceViewRequest): Promise<SourceViewResult> {
    return this.#client.request<SourceViewResult>('POST', '/sources/view', { body })
  }
}
