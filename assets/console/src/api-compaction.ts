// 压缩链只读视图客户端（US-16 §3.1.9 / T089）：一个端点、一个方向。
// 字段名与 `docs/contracts/openapi.yaml` 的 `CompactionView` 逐字一致。三条契约事实
// 决定本客户端的形状：
//
//  1. **只有 GET**：压缩由引擎在运行中触发，契约**没有**手动触发端点——客户端也
//     就没有可调用的写方法，"页面点不出压缩"由接口面保证，而不是靠按钮禁用。
//  2. **`session_id` 与 `run_id` 恰给一个**：都给或都不给都是 400。本客户端不提供
//     "列出全部"的调用形态，避免页面写出契约不允许的请求。
//  3. **摘要只是派生视图**：`summary_view.summary` 不得当消息原文或引用依据；引用
//     必须回 `POST /sources/view` 按 `included_refs` 逐条校验——页面上以文案声明。
//
// Compaction client: read-only by construction. Exactly one scope id per call, and the
// summary is a derived view that must never act as a citation.
import type { ApiClient } from './api'
import type { EvidenceRef } from './api-knowledge'

/** 条目状态三值（契约 `CompactionView.status`）；只有 `committed` 参与链。 */
export type CompactionStatus = 'committed' | 'failed' | 'stale'

/**
 * `CompactionSummaryView`：摘要正文 + 截断标记。`summary` 是派生文本，
 * **不**等于被覆盖消息的原文。
 * The summary sub-view; derived text, never the covered messages.
 */
export interface CompactionSummaryView {
  compaction_id: string
  summary: string
  truncated: boolean
}

/**
 * `CompactionView`：一条压缩条目。`source_refs` 是**审计集**（生成时涉及的全部引用，
 * 含此后失效 / 被剔除的），`included_refs` 是**当前依赖集**（正文里真正含其原文的
 * 来源）——两个集合分开承载，页面不得把任一者当成另一个。
 * `source_refs` is the audit set, `included_refs` the current dependency set; they are
 * never interchangeable.
 */
export interface CompactionView {
  compaction_id: string
  scope_session_id: string
  scope_run_id: string
  chain_epoch: number
  parent_compaction_id: string
  trigger_run_id: string
  attempt_id: string
  covered_from_message_id: string
  cut_message_id: string
  first_kept_message_id: string
  strategy_version: string
  prompt_version: string
  model: string
  tokens_before: number
  tokens_after: number
  status: CompactionStatus
  reason_code: string
  source_refs: EvidenceRef[]
  included_refs: EvidenceRef[]
  created_at: string
  stale_at: string
  summary_view: CompactionSummaryView
}

/** 读链参数：`sessionId` 与 `runId` 恰给一个（契约 400 口径）。 */
// Exactly one scope id; the server answers 400 otherwise.
export interface CompactionQuery {
  sessionId?: string
  runId?: string
  limit?: number
}

/** 压缩链只读客户端：一个方法对应 `GET /compactions`。 */
// One method per endpoint; there is no trigger call to leak into the UI.
export class CompactionApi {
  readonly #client: ApiClient

  constructor(client: ApiClient) {
    this.#client = client
  }

  /** 读当前 `chain_epoch` 的链（含 failed / stale 与原因码）。 */
  listChain(query: CompactionQuery): Promise<CompactionView[]> {
    return this.#client.request<CompactionView[]>('GET', '/compactions', {
      query: {
        session_id: query.sessionId,
        run_id: query.runId,
        limit: query.limit,
      },
    })
  }
}
