// 历史回查只读客户端（US-16 §3.1.9 / T057）：两个端点、同一个方向。字段名与
// `docs/contracts/openapi.yaml` 的 `HistoryHit` / `HistorySearchResult` /
// `MessageDetail` 逐字一致。三条契约事实决定本客户端的形状：
//
//  1. **检索与精确读取是两条路**：`GET /history/search` 只回指针 + 片段，
//     `GET /sessions/{session_id}/messages/{message_id}` 回原文；片段**不是**引用依据，
//     页面拿不到"用片段冒充引用"的方法。
//  2. **`q` 必填**：契约写死非空关键词（空串 → 400）；本客户端不提供"列出全部"的
//     参数组合，页面也不提供。
//  3. **`truncated` 是数据**：命中超过本页上限时为 true，页面必须如实标注"本页不是
//     全部"，不得静默当成完整结果。
//
// History client: search returns pointers only, an empty keyword is never sent, and
// a truncated page is reported rather than silently treated as complete.
import type { ApiClient } from './api'
import type { EvidenceRef, ValidityView } from './api-knowledge'

/** 消息角色四值（契约 `role` 枚举）；与检索命中和原文读取共用。 */
export type HistoryRole = 'user' | 'assistant' | 'tool' | 'system'

/**
 * `HistoryHit`：命中只回指针与片段。`snippet` **不得**当作引用依据——引用走
 * 来源精确读取（`POST /sources/view`）。
 * A pointer plus a snippet; the snippet is never a citation.
 */
export interface HistoryHit {
  message_id: string
  session_id: string
  run_id: string
  role: HistoryRole
  snippet: string
  score?: number
  created_at: string
}

/** `HistorySearchResult`：`truncated` 缺省按"未截断"解释（旧响应不带该字段）。 */
export interface HistorySearchResult {
  hits: HistoryHit[]
  truncated?: boolean
}

/** 一条来源引用与它的**当前**结论（契约 `MessageEvidenceRefView`）。 */
export interface MessageEvidenceRefView {
  ref: EvidenceRef
  validity: ValidityView
}

/**
 * `MessageDetail`：原文与元数据。`evidence_refs` 由服务端按资料**当前** ACL /
 * 删除 / 版本逐个标注；页面只展示结论，不从正文反推引用。
 * The exact read; evidence refs arrive re-checked against today's ACL.
 */
export interface MessageDetail {
  message_id: string
  session_id: string
  run_id: string
  seq: number
  role: HistoryRole
  content: string
  /** 原文被保留策略截断时为 true（服务端重查后判定）。 */
  truncated: boolean
  tool_call_id?: string
  created_at: string
  evidence_refs?: MessageEvidenceRefView[]
}

/**
 * 检索参数：`q` 必填；`callerId` 只对 admin token 生效（caller key 传他人 ID 由
 * 服务端 403，页面不预判身份）。
 * Search parameters; only the keyword is mandatory.
 */
export interface HistoryQuery {
  q: string
  agent?: string
  sessionId?: string
  callerId?: string
  /** RFC3339；空串表示该侧不限。 */
  from?: string
  to?: string
  limit?: number
}

/** 历史只读客户端：两个方法对应两个端点。 */
// One method per endpoint; there is no write surface to leak into the UI.
export class HistoryApi {
  readonly #client: ApiClient

  constructor(client: ApiClient) {
    this.#client = client
  }

  /** 关键词检索（服务端固定按时间倒序、有界返回）。 */
  search(query: HistoryQuery): Promise<HistorySearchResult> {
    return this.#client.request<HistorySearchResult>('GET', '/history/search', {
      query: {
        q: query.q,
        agent: query.agent,
        session_id: query.sessionId,
        caller_id: query.callerId,
        from: query.from,
        to: query.to,
        limit: query.limit,
      },
    })
  }

  /** 精确读取一条消息原文；跨调用方 → 404，被保留策略删除 → 410（服务端语义）。 */
  readMessage(sessionId: string, messageId: string): Promise<MessageDetail> {
    return this.#client.request<MessageDetail>(
      'GET',
      `/sessions/${encodeURIComponent(sessionId)}/messages/${encodeURIComponent(messageId)}`,
    )
  }
}
