// Flow transport always uses the shared credential/envelope client. Queries are
// read-only; unknown writes are never automatically retried with a new identity.
import { ApiError, type ApiClient } from './api'
import { randomFlowID } from './flow-identity'

export type FlowStatus = 'queued' | 'running' | 'blocked' | 'succeeded' | 'failed' | 'stopped' | 'cancelling' | 'cancelled' | 'abandoned'
export interface FlowPage<T> { items: T[]; next_cursor?: string }
export interface FlowAgentPin { uid: string; version: number; name: string; config_hash: string; manifest_hash: string }
export interface FlowAgentCandidate {
  name: string; display_name: string; description: string; eligible: boolean
  reason: 'ready' | 'agent_unregistered' | 'agent_disabled' | 'resources_unavailable'
  agent?: FlowAgentPin; knowledge_collections: string[]
  memory_scope: 'agent_only' | 'global_and_agent' | 'legacy_default'; readonly_tools: string[]
}
export interface FlowAuthoringVersion { definition_uid: string; version: number; content_hash: string; retired: boolean; published_at: number; bundle_json: string }
export interface FlowDefinition {
  definition_uid: string; version: number; name: string; content_hash: string
  retired: boolean; goal: string; input_schema: Record<string, unknown>; output_schema: Record<string, unknown>
  nodes: { id: string; kind: string; agent?: string; tool?: string; next?: string }[]
}
export interface FlowAttempt {
  node_attempt_id: string; node_id: string; node_invocation_id: string; attempt: number
  task_revision: number; execution_epoch: number; run_id?: string; session_id?: string
  state: string; started_at: number; finished_at?: number; input_hash: string; output_hash?: string
  run_admitted: boolean
}
export interface FlowTask {
  task_id: string; root_task_id: string; parent_task_id?: string; definition_uid: string; definition_name?: string; definition_version: number
  status: FlowStatus; version: { revision: number; state_version: number; execution_epoch: number }; etag: string
  goal: string; constraints: string[]; current_node: string; close_intent: 'none' | 'cancel' | 'abandon' | 'deadline'; cancel_requested: boolean
  unknown_actions: boolean; reason?: string; deadline: number; created_at: number; updated_at: number
  budget?: { max_total_tokens: number; max_tool_calls: number; max_duration_seconds: number }
  remaining_tokens?: number; remaining_tool_calls?: number; budget_exhausted: boolean
  slots: { id: string; state: string; evidence_ids: string[]; verification_ids: string[] }[]
  claims: { claim_id: string; claim_revision: number; state: string; text?: string; readable: boolean }[]
  attempts: FlowAttempt[]; next_attempt_cursor?: string
}
export interface FlowArtifact { artifact_id: string; version: number; task_revision: number; state: string; format: string; content_hash: string; readable: boolean; content?: string; reason?: string }
export interface FlowVerification { verification_id: string; target_kind: string; target_id: string; target_version: number; task_revision: number; rule_id: string; rule_version: number; validator_kind: string; outcome: string; reason: string; basis_hashes: string[]; created_at: number }
export type FlowControl = { operation_id: string; reason: string; etag: string }

export class FlowApi {
  constructor(readonly client: ApiClient) {}
  authoringAgents(cursor = ''): Promise<FlowPage<FlowAgentCandidate>> { return this.client.request('GET', '/admin/flow-agents', { query: { cursor, limit: 50 } }) }
  authoringVersion(uid: string, version: number): Promise<FlowAuthoringVersion> { return this.client.request('GET', `/admin/flows/${encodeURIComponent(uid)}/versions/${version}`) }
  definitions(admin = false, cursor = ''): Promise<FlowPage<FlowDefinition>> { return this.client.request('GET', admin ? '/admin/flows' : '/flows', { query: { cursor, limit: 50 } }) }
  tasks(admin = false, cursor = '', definitionName = ''): Promise<FlowPage<FlowTask>> { return this.client.request('GET', admin ? '/admin/flow-tasks' : '/flow-tasks', { query: { cursor, limit: 50, definition_name: definitionName } }) }
  task(id: string, admin = false, cursor = ''): Promise<FlowTask> { return this.client.request('GET', `${admin ? '/admin' : ''}/flow-tasks/${encodeURIComponent(id)}`, { query: { cursor, limit: 50 } }) }
  artifacts(id: string, cursor = ''): Promise<FlowPage<FlowArtifact>> { return this.client.request('GET', `/flow-tasks/${encodeURIComponent(id)}/artifacts`, { query: { cursor, limit: 50 } }) }
  verifications(id: string, cursor = ''): Promise<FlowPage<FlowVerification>> { return this.client.request('GET', `/flow-tasks/${encodeURIComponent(id)}/verifications`, { query: { cursor, limit: 50 } }) }
  validate(definition: string): Promise<{ valid: boolean }> { return this.client.request('POST', '/admin/flows/validate', { rawBody: `{"definition":${definition}}` }) }
  publish(definition: string, operation_id: string, expected_version: number, definition_uid?: string): Promise<FlowDefinition> { const metadata = JSON.stringify({operation_id, expected_version, definition_uid}); return this.client.request('POST', '/admin/flows/publish', { rawBody: metadata.slice(0, -1) + `,"definition":${definition}}` }) }
  retire(definition: FlowDefinition, operation_id: string, reason: string): Promise<FlowDefinition> { return this.client.request('POST', `/admin/flows/${encodeURIComponent(definition.definition_uid)}/retire`, { body: { expected_version: definition.version, operation_id, reason } }) }
  start(definition_uid: string, input: string, idempotencyKey: string): Promise<FlowTask> { return this.client.request('POST', '/flow-tasks', { idempotencyKey, rawBody: `{"definition_uid":${JSON.stringify(definition_uid)},"input":${input}}` }) }
  control(id: string, action: 'cancel' | 'resume' | 'abandon', body: FlowControl): Promise<FlowTask> { return this.client.request('POST', `${action === 'abandon' ? '/admin' : ''}/flow-tasks/${encodeURIComponent(id)}/${action}`, { ifMatch: body.etag, body: { operation_id: body.operation_id, reason: body.reason } }) }
}

export const FLOW_STATUS: Record<FlowStatus, string> = { queued: '等待执行', running: '执行中', blocked: '等待核对', succeeded: '已通过验收', failed: '验收失败', stopped: '已停止', cancelling: '正在停止', cancelled: '已取消', abandoned: '已放弃' }
export const flowTerminal = (status: FlowStatus): boolean => ['succeeded', 'failed', 'stopped', 'cancelled', 'abandoned'].includes(status)
export function flowIdentity(): string { return 'console-flow-' + randomFlowID() }
// HTTP 409 also represents an unknown commit. Only explicit pre-dispatch
// refusal codes release a write lock; status class alone is not evidence.
export function flowWriteDefinitelyRejected(error: unknown): boolean {
  if (!(error instanceof ApiError) || error.status < 400 || error.status >= 500) return false
  return ['BAD_REQUEST', 'UNAUTHORIZED', 'FORBIDDEN', 'PRECONDITION_FAILED', 'PRECONDITION_REQUIRED', 'FLOW_DEFINITION_INVALID', 'FLOW_SCHEMA_UNSUPPORTED', 'FLOW_DEFINITION_NOT_FOUND', 'FLOW_DEFINITION_VERSION_NOT_FOUND', 'FLOW_NOT_FOUND', 'FLOW_DEFINITION_RETIRED', 'FLOW_DEFINITION_NOT_CURRENT', 'FLOW_ACTION_UNKNOWN', 'FLOW_CHECKPOINT_UNSUPPORTED', 'FLOW_CLOSED', 'FLOW_DEADLINE_EXPIRED', 'FLOW_CAPACITY_EXHAUSTED', 'FLOW_INTERACTIVE_RESERVE', 'RATE_LIMITED'].includes(error.code)
}
export function flowHref(id: string, admin = false): string { return '/console/flow-tasks/' + encodeURIComponent(id) + (admin ? '?admin=1' : '') }
export function flowError(error: unknown): string {
  if (!(error instanceof ApiError)) return '无法读取或提交流程数据，请保留原操作标识并刷新核对。'
  const copy: Record<string, string> = {
    FORBIDDEN: '当前身份无权执行此操作；流程权限不代替 Agent 或来源权限。',
    FLOW_NOT_FOUND: '任务不存在或不属于当前调用方。',
    PRECONDITION_FAILED: '任务或发布版本已变化；请刷新核对后再决定是否执行新操作。',
    FLOW_IDEMPOTENCY_CONFLICT: '原操作标识对应另一份请求；请核对原任务，不要换标识重复执行。',
    FLOW_ACTION_UNKNOWN: '动作结果尚未查证，已阻止恢复。需要人工对账，不能强制成功或重放。',
    FLOW_INTERACTIVE_RESERVE: '当前调用方没有可用的流程并发额度；请核对交互预留与调用方限额。',
    FLOW_CAPACITY_EXHAUSTED: '持久任务容量已满，未接受新任务。请先处理现有任务。',
    FLOW_CLOSED: '任务已关闭或正在关闭，不能继续执行。',
    FLOW_DEADLINE_EXPIRED: '任务已到固定截止时间，不能延长后重试。',
  }
  return copy[error.code] ?? error.message
}
