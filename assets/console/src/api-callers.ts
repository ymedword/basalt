// 调用方管理 API（US-16 §3.1.9 / T026a）：七个契约端点各一个方法，字段名与
// `docs/contracts/openapi.yaml` 逐字一致。契约**没有** Key 列表端点，也**没有**
// 待核对预留列表端点——这里不发明：页面只按签发响应里的一次性 `key_id` 与手填的
// 预留标识工作，缺口在页面上如实说明。
// Caller-admin client: one method per contract endpoint. The contract exposes no key
// list and no pending-reservation list, so nothing here pretends otherwise; the
// console works from the issued key's id and manually entered reservation ids.
import type { ApiClient } from './api'

/** 调用方种类（契约 `CallerSummary.kind`）。 */
export type CallerKind = 'builtin' | 'external'

/** 调用方状态；`disabled` 后该调用方的全部 Key 认证失败。 */
export type CallerStatus = 'active' | 'disabled'

/** 配额周期（契约 `CallerLimitView.period`）。 */
export type CallerLimitPeriod = 'day' | 'month'

/** Key 状态（契约 `CallerKeySummary.status`）。 */
export type CallerKeyStatus = 'active' | 'revoked' | 'expired'

/** 预留核对结局（契约 `ReservationReconcileRequest.outcome`）。 */
export type CallerReconcileOutcome = 'committed' | 'released'

/** `CallerSummary`：列表行；`key_count` 只数未吊销的 Key。 */
export interface CallerSummary {
  caller_id: string
  kind: CallerKind
  name: string
  project: string
  status: CallerStatus
  created_at: string
  key_count: number
}

/**
 * `CallerLimitView`：零值照实输出——零表示「该维度拒绝」，**不是**不限量。
 * Zero means denied, never unlimited.
 */
export interface CallerLimitView {
  qps: number
  burst: number
  max_concurrent: number
  tasks_per_period: number
  tokens_per_period: number
  ingest_bytes_per_period: number
  period: CallerLimitPeriod
  timezone: string
}

/** `CallerKeySummary`：不含明文，只有展示前缀；空串表示无该时刻。 */
export interface CallerKeySummary {
  key_id: string
  caller_id: string
  key_prefix: string
  scopes: string[]
  status: CallerKeyStatus
  created_at: string
  expires_at: string
  revoked_at: string
}

/**
 * `IssuedKey`：Key 明文**只在这里出现一次**。视图状态持有它直到操作者确认抄录；
 * 它不进 URL、不进存储、不进日志。
 * The one-time plaintext; it lives only in view state until acknowledged.
 */
export interface IssuedKey extends CallerKeySummary {
  key: string
}

/** `UsageLine`：预留继续占额度，超限不可回滚。 */
export interface CallerUsageLine {
  kind: string
  used: number
  reserved: number
  overage: number
}

/** `CallerUsageView`：周期边界按 `timezone` 本地 00:00 折算后以 UTC 出站。 */
export interface CallerUsageView {
  caller_id: string
  period_start: string
  period_end: string
  timezone: string
  limits: CallerLimitView
  usage: CallerUsageLine[]
  pending_reservations: number
}

/** `ReservationView`：结算结果只带结算行能给出的事实，不编造可选字段。 */
export interface ReservationView {
  reservation_id: string
  caller_id: string
  class: string
  period_start: string
  amount: number
  actual?: number
  overage: number
  status: string
}

/** `CallerCreateRequest`：`name` 必填；`limits` 缺省时**不写**限额行。 */
export interface CallerCreateRequest {
  caller_id?: string
  name: string
  project?: string
  limits?: CallerLimitView
}

/** `CallerKeyIssueRequest`：`scopes` 至少一项；`expires_at` 缺省不过期。 */
export interface CallerKeyIssueRequest {
  scopes: string[]
  expires_at?: string
}

/** `ReservationReconcileRequest`：`committed` 必须带 `actual`，`released` 不得带。 */
export interface ReservationReconcileRequest {
  outcome: CallerReconcileOutcome
  actual?: number
  reason: string
}

/** `ClearOverageRequest`：`reason` 必填并写入管理审计；空 `period_start` = 当前周期。 */
export interface ClearOverageRequest {
  period_start?: string
  reason: string
}

/**
 * CallersApi 是调用方管理面的唯一入口：方法名与契约路径同构，路径参数一律
 * `encodeURIComponent`；不在这里补默认值或重试。
 * One method per contract endpoint; path segments are always encoded.
 */
export class CallersApi {
  readonly #client: ApiClient

  constructor(client: ApiClient) {
    this.#client = client
  }

  /** 列调用方（服务端固定按 `caller_id` 升序，`local-admin` 恒在列表内）。 */
  list(): Promise<CallerSummary[]> {
    return this.#client.request<CallerSummary[]>('GET', '/callers')
  }

  /** 建调用方；响应是回读后的行，不含限额字段。 */
  create(body: CallerCreateRequest): Promise<CallerSummary> {
    return this.#client.request<CallerSummary>('POST', '/callers', { body })
  }

  /** 签发 Key：明文只出现在本次响应里，服务端只存指纹。 */
  issueKey(callerId: string, body: CallerKeyIssueRequest): Promise<IssuedKey> {
    return this.#client.request<IssuedKey>(
      'POST',
      `/callers/${encodeURIComponent(callerId)}/keys`,
      { body },
    )
  }

  /** 吊销 Key（重复吊销幂等）；`reason` 只进管理审计，空串时不发请求体。 */
  revokeKey(callerId: string, keyId: string, reason = ''): Promise<CallerKeySummary> {
    const trimmed = reason.trim()
    const path = `/callers/${encodeURIComponent(callerId)}/keys/${encodeURIComponent(keyId)}/revoke`
    return trimmed === ''
      ? this.#client.request<CallerKeySummary>('POST', path)
      : this.#client.request<CallerKeySummary>('POST', path, { body: { reason: trimmed } })
  }

  /** 查周期用量；`periodStart` 空串 = 当前周期（周期起点由服务端折算）。 */
  usage(callerId: string, periodStart = ''): Promise<CallerUsageView> {
    return this.#client.request<CallerUsageView>(
      'GET',
      `/callers/${encodeURIComponent(callerId)}/usage`,
      { query: { period_start: periodStart } },
    )
  }

  /** 核对未知结局的预留：只收 `pending`，二次结算由服务端按 409 拒绝。 */
  reconcile(
    callerId: string,
    reservationId: string,
    body: ReservationReconcileRequest,
  ): Promise<ReservationView> {
    return this.#client.request<ReservationView>(
      'POST',
      `/callers/${encodeURIComponent(callerId)}/reservations/${encodeURIComponent(reservationId)}/reconcile`,
      { body },
    )
  }

  /** 清零某周期的 `overage`：不动 `used` / `reserved`，不删预留行。 */
  clearOverage(callerId: string, body: ClearOverageRequest): Promise<CallerUsageView> {
    return this.#client.request<CallerUsageView>(
      'POST',
      `/callers/${encodeURIComponent(callerId)}/usage/clear-overage`,
      { body },
    )
  }
}
