// 诊断事件只读客户端（US-16 §3.1.9 / T098）：一个端点、一个方向。字段名与
// `docs/contracts/openapi.yaml` 的 `DiagnosticEventView` / `DiagnosticsResult`
// 逐字一致。三条契约事实决定本客户端的形状：
//
//  1. **只有 GET**：导出与查询都归 US-16A（拟）OBS，本 US 不提供写面——客户端因此
//     没有可调用的写方法，"页面点不出导出"由接口面保证。
//  2. **至少一个主锚点**：`run_id` / `request_id` / `operation_id` / `ingest_job_id` /
//     `session_id` 至少给一个，`task_id` 只作限定符。本客户端不做"查全部"，页面也不提供。
//  3. **证据缺口是数据的一部分**：`missing` 与 `sampled_out` 互斥，页面必须把它们
//     如实呈现——空集不等于"一切正常"。
//
// Diagnostics client: read-only by construction, one anchor is mandatory, and the
// evidence-gap flags are part of the payload rather than a UI invention.
import type { ApiClient } from './api'

/** 契约 `DiagnosticKind` 十值（新增值须同时改契约，由后端契约测试兜住）。 */
// The ten contract kinds; the backend contract test catches drift.
export type DiagnosticKind =
  | 'ingest_stage'
  | 'index_switch'
  | 'retrieval'
  | 'assembly'
  | 'embedding'
  | 'llm_call'
  | 'tool_call'
  | 'permission_decision'
  | 'quota_decision'
  | 'migration'

/** 关联标识：审计与诊断共用的定位键；全部可空。 */
// The shared correlation keys; every one of them may be empty.
export interface DiagnosticCorrelationView {
  request_id: string
  run_id: string
  task_id: string
  operation_id: string
  ingest_job_id: string
}

/** 审计回指：诊断事件指向的权威记录（表名 + 主键）。 */
// A pointer at the authoritative record; diagnostics never replace it.
export interface DiagnosticAuditRefView {
  table: string
  id: string
}

/**
 * `DiagnosticEventView`：一条结构化诊断事件。`attrs` 已在服务端脱敏——页面**不得**
 * 据此重建被权限拒绝的资料标题，也不得以 `attrs` 冒充审计记录。
 * `attrs` arrives already redacted; the UI never rebuilds a denied title from it.
 */
export interface DiagnosticEventView {
  event_id: string
  schema_version: number
  kind: DiagnosticKind
  occurred_at: string
  correlation: DiagnosticCorrelationView
  actor: string
  step_id: string
  attempt_id: string
  parent_step_id: string
  seq: number
  reason_code: string
  decision: string
  attrs: Record<string, unknown>
  audit_ref?: DiagnosticAuditRefView
}

/**
 * `DiagnosticsResult`：一页事件 + 证据缺口标记。
 * `missing` = 全量采样下确实没有记录；`sampled_out` = 采样率 < 1 时无行。
 * 二者互斥，页面必须分开表述，不得合写成"没有数据"。
 */
export interface DiagnosticsResult {
  events: DiagnosticEventView[]
  missing: boolean
  sampled_out?: boolean
  /** 本实例的**配置值**，不是保留承诺（契约逐字）。 */
  retention_days: number
}

/** 查询参数：五个主锚点至少给一个；`task_id` 只作限定符。 */
// At least one anchor; task_id is a qualifier only.
export interface DiagnosticQuery {
  runId?: string
  requestId?: string
  operationId?: string
  ingestJobId?: string
  sessionId?: string
  taskId?: string
  kind?: DiagnosticKind
  /** RFC3339；空串表示该侧不限。 */
  from?: string
  to?: string
  limit?: number
}

/** 诊断只读客户端：一个方法对应 `GET /diagnostics`。 */
// One method per endpoint; there is no export call to leak into the UI.
export class DiagnosticsApi {
  readonly #client: ApiClient

  constructor(client: ApiClient) {
    this.#client = client
  }

  /** 按关联标识读一页事件（服务端按 `occurred_at` 升序、有界返回）。 */
  list(query: DiagnosticQuery): Promise<DiagnosticsResult> {
    return this.#client.request<DiagnosticsResult>('GET', '/diagnostics', {
      query: {
        run_id: query.runId,
        request_id: query.requestId,
        operation_id: query.operationId,
        ingest_job_id: query.ingestJobId,
        session_id: query.sessionId,
        task_id: query.taskId,
        kind: query.kind,
        from: query.from,
        to: query.to,
        limit: query.limit,
      },
    })
  }
}
