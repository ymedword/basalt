// US-16A typed read results; kept in sync with the OpenAPI OBS schemas.
import type { ApiClient } from './api'

export type ObsEvidenceGap = {
  source: "runs" | "task_executions" | "management_operations" | "kb_ingest_jobs" | "embedding_calls" | "llm_calls" | "tool_invocations" | "context_assembly_records" | "diagnostic_events"
  reason: "not_recorded" | "sampled_out" | "query_failed" | "retention_window_cleaned" | "not_supported" | "unknown"
  detail: string
}

export type ObsAnchor = {
  kind: "run" | "request" | "task" | "flow_task" | "operation" | "ingest" | "session"
  value: string
}

export type ObsTimelineEntry = {
  flow?: { task_id: string, root_task_id: string, node_id: string, node_attempt_id: string, revision: number, execution_epoch: number, action_id?: string }
  schema_version?: number
  decision?: string
  redacted_fields: number
  display_step_id: string
  event_id?: string
  source: "runs" | "task_executions" | "management_operations" | "kb_ingest_jobs" | "embedding_calls" | "llm_calls" | "tool_invocations" | "context_assembly_records" | "diagnostic_events"
  source_id: string
  kind: string
  occurred_at: string
  ended_at: string | null
  end_source: "recorded" | "derived_recorded" | "unknown"
  status: string
  reason_code?: string
  step_id?: string
  attempt_id?: string
  parent_step_id?: string
  seq?: number
  audit_ref?: string
  evidence: "direct" | "needs_output_check" | "insufficient"
  attrs: Record<string, string | number | boolean>
}

export type ObsTimeline = {
  anchor: ObsAnchor
  entries: (ObsTimelineEntry)[]
  gaps: (ObsEvidenceGap)[]
  order_scope: "anchor-local"
  truncated: boolean
  projected_at: string
}

export type ObsReasonCount = {
  reason: string
  count: number
}

export type ObsTaskGroup = {
  agent: string
  window_start: string
  window_end: string
  failed: number
  total: number
  top_reasons: (ObsReasonCount)[]
  run_ids: (string)[]
}

export type ObsOverview = {
  runs: number
  failed: number
  failed_ratio: number | null
  timeouts: number
  tool_denials: number
  missing_assembly: number
  groups: (ObsTaskGroup)[]
  truncated: boolean
  merged_groups: number
  other_group: ObsTaskGroup | null
}

export type ObsCandidate = {
  chunk_id: string
  version_id: string
  content_hash: string
  token_estimate: number
  retrieval_score: number
  rerank_score: number | null
  rank: number
  decision: "kept" | "rank_cut" | "budget_drop" | "not_recorded"
  acl_decision: "allow"
}

export type ObsRetrieval = {
  candidates: (ObsCandidate)[]
  hits: (string)[]
  reason_codes: (string)[]
  top_k: number | null
  candidate_k: number | null
  budget_tokens: number | null
  filtered_count: number | null
  filtered_reason: string
  index_generation: number | null
  evidence: Record<string, "direct" | "needs_output_check" | "insufficient">
  filtered_note: string
}

export type ObsHistoryTrim = {
  dropped_groups: number
  first_dropped_message_id: string
  cut_message_id: string
  reason_code: string
  tokens_before: number
  tokens_after: number
  strategy_version: string
}

export type ObsCompression = {
  enabled: boolean
  decision: "none" | "compressed" | "degraded" | "rejected"
  compaction_id: string
  covered_from_message_id: string
  cut_message_id: string
  first_kept_message_id: string
  tokens_before: number
  tokens_after: number
  reason_code: string
}

export type ObsBudget = {
  context_tokens: number
  protocol_reserve: number
  output_reserve: number
  estimator_version: string
}

export type ObsRound = {
  step_id: string
  candidates: (ObsCandidate)[]
  hits: (string)[]
  history_trim: ObsHistoryTrim | null
  compression: ObsCompression | null
  reason_codes: (string)[]
  budget: ObsBudget | null
  final_segments: (string)[]
  evidence: Record<string, "direct" | "needs_output_check" | "insufficient">
  note: string
}

export type ObsAssembly = {
  record_id: string
  purpose: string
  step_id: string
  reason_codes: (string)[]
}

export type ObsContext = {
  run_id: string
  retrieval: ObsRetrieval | null
  rounds: (ObsRound)[]
  other: (ObsAssembly)[]
  gaps: (ObsEvidenceGap)[]
  grade: "direct" | "needs_output_check" | "insufficient"
}

export type ObsMetricSample = {
  name: string
  type: "counter" | "gauge" | "histogram"
  unit: string
  value: number | null
  window: number
  scope: "process" | "database"
  available: boolean
  reason: string
  labels: Record<string, string>
}

export type ObsMetrics = {
  samples: (ObsMetricSample)[]
  collected_at: string
  process_started_at: string
}

export type ObsAlertState = {
  rule_id: string
  scope_key: string
  state: "inactive" | "pending" | "firing" | "unknown"
  since: string | null
  last_eval_at: string | null
  value: number
  reason: string
}

export type ObsAlertRule = {
  rule_id: string
  enabled: boolean
  window_seconds: number
  for_seconds: number
  states: (ObsAlertState)[]
}

export type ObsAlertTransition = {
  id: number
  rule_id: string
  scope_key: string
  from_state: "inactive" | "pending" | "firing" | "unknown"
  to_state: "inactive" | "pending" | "firing" | "resolved" | "unknown"
  value: number
  occurred_at: string
}

export type ObsAlerts = {
  enabled: boolean
  rules: (ObsAlertRule)[]
  transitions: (ObsAlertTransition)[]
  truncated: boolean
}

export type ObsSourceCoverage = {
  source: "runs" | "task_executions" | "management_operations" | "kb_ingest_jobs" | "embedding_calls" | "llm_calls" | "tool_invocations" | "diagnostic_events"
  coverage_kind: "terminal_time" | "occurred_at"
  covered_from: string | null
  covered_through: string | null
  last_attempt_at: string | null
  last_success_at: string | null
  last_reconcile_at: string | null
}

export type ObsExport = {
  enabled: boolean
  endpoint_configured: boolean
  headers_configured: boolean
  protocol: "http/protobuf" | "http/json"
  pending: number
  inflight: number
  retry: number
  retry_attempts: number | null
  next_retry_at: string | null
  unconfirmed: number
  failed_permanent: number
  dropped: number
  stale: number
  unknown_attempts: number
  oldest_pending_age_seconds: number
  last_scan_at: string | null
  last_batch_id: string
  last_partial_at: string | null
  last_record_unit: "" | "span" | "log_record" | "data_point"
  last_record_count: number
  last_outcome: string
  last_http_status: number | null
  last_accepted: number | null
  last_rejected: number | null
  last_warning: string
  last_error_kind: string
  sources: (ObsSourceCoverage)[]
}

export type ObsRetentionPolicy = {
  table: "diagnostic_events" | "context_assembly_records" | "alert_history" | "export_history"
  enabled: boolean
  retention_days: number
  batch_size: number
  max_batches: number
  require_confirmed_export: boolean
}

export type ObsCleanupRun = {
  cleanup_id: string
  table_kind: "diagnostic_events" | "context_assembly_records" | "alert_history" | "export_history"
  window_start: string
  window_end: string
  scan_covered_from: string | null
  scan_covered_through: string | null
  deleted: number
  skipped: number
  skip_reason: string
  status: "succeeded" | "failed" | "skipped"
  error_kind: string
  started_at: string
  finished_at: string | null
}

export type ObsCleanup = {
 files?: {enabled:boolean;days:number;log_file_mib:number;log_total_mib:number;min_verified_backups:number;last_run_at:string|null;deleted_files:number;status:string}
 history_enabled?: boolean
 history_days?: number
 history?: {id:string;group:string;at:string;cutoff:string;deleted_groups:number;deleted_rows:number;protected_groups:number;status:string;reason:string}[]
  enabled: boolean
  policies: (ObsRetentionPolicy)[]
  runs: (ObsCleanupRun)[]
  truncated: boolean
}

export type ObsQuery = Readonly<Record<string, string | number | undefined>>
export type ObsWindow = '5m' | '15m' | '1h' | '24h'

// All seven operations are GET; this client cannot mutate observation or business state.
export class ObservabilityApi {
  constructor(readonly client: ApiClient) {}
  overview(query: ObsQuery = {}): Promise<ObsOverview> { return this.get('overview', query) }
  timeline(query: ObsQuery): Promise<ObsTimeline> { return this.get('timeline', query) }
  context(runId: string): Promise<ObsContext> { return this.get('context', {run_id: runId}) }
  metrics(window: ObsWindow = '15m'): Promise<ObsMetrics> { return this.get('metrics', {window}) }
  alerts(query: ObsQuery = {}): Promise<ObsAlerts> { return this.get('alerts', query) }
  exportStatus(): Promise<ObsExport> { return this.get('export') }
  cleanup(query: ObsQuery = {}): Promise<ObsCleanup> { return this.get('cleanup', query) }
  private get<T>(name: string, query: ObsQuery = {}): Promise<T> {
    return this.client.request<T>('GET', '/observability/' + name, {query})
  }
}
