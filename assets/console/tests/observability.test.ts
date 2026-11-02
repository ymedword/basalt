// 用户可见语义来自US-16A §2.10；反例是零/缺失混淆、跨锚点残留、未知当正常及写请求。
// User-visible contracts reject missing-as-zero, stale anchors, healthy unknowns, and mutations.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleApp } from '../src/app'
import { observabilityHref } from '../src/router'

interface Call { method: string; path: string; query: URLSearchParams }
let wrapper: ReturnType<typeof mount> | undefined
let calls: Call[] = []
const at = '2026-11-01T12:00:00Z'
const overview = {runs: 0, failed: 0, failed_ratio: null, timeouts: 0, tool_denials: 0, missing_assembly: 0, groups: [], truncated: false, merged_groups: 0, other_group: null}
const metrics = {samples: [{name: 'obs_runs_total', value: null, available: false, reason: 'source_unavailable', scope: 'database', window: 900000000000, unit: '1', type: 'gauge', labels: {}}], collected_at: at, process_started_at: at}
const timeline = {anchor: {kind: 'run', value: 'real-run'}, entries: [{source: 'runs', source_id: 'real-run', kind: 'run', display_step_id: 'S1', occurred_at: at, ended_at: null, end_source: 'unknown', status: 'failed', reason_code: 'timeout', evidence: 'direct', attrs: {}, redacted_fields: 0}], gaps: [], order_scope: 'anchor-local', truncated: false, projected_at: at}
const context = {run_id: 'real-run', retrieval: null, rounds: [], other: [], gaps: [], grade: 'insufficient'}
const exportStatus = {enabled: false, endpoint_configured: true, headers_configured: true, protocol: 'http/protobuf', pending: 3, inflight: 1, retry: 2, unconfirmed: 4, failed_permanent: 5, dropped: 6, stale: 7, unknown_attempts: 8, oldest_pending_age_seconds: 90, last_scan_at: null, last_batch_id: 'batch-real', last_partial_at: at, last_record_unit: 'data_point', last_record_count: 10, last_outcome: 'partial_unconfirmed', last_accepted: null, last_rejected: null, last_warning: '', last_error_kind: 'protocol_invalid', sources: [{source: 'runs', coverage_kind: 'terminal_time', covered_from: null, covered_through: null, last_attempt_at: at, last_success_at: null, last_reconcile_at: null}]}
const cleanup = {enabled: false, policies: [{table: 'diagnostic_events', enabled: false, retention_days: 7, batch_size: 1000, max_batches: 20, require_confirmed_export: false}], runs: [{cleanup_id: 'cleanup-real', table_kind: 'diagnostic_events', window_start: at, window_end: at, scan_covered_from: null, scan_covered_through: null, deleted: 0, skipped: 0, skip_reason: 'scan_not_covered', status: 'skipped', error_kind: '', started_at: at, finished_at: at}], truncated: false}

function response(data: unknown, status = 200): Response {
  return {ok: status < 400, status, headers: new Headers(), json: async () => ({code: status < 400 ? 'OK' : status === 403 ? 'FORBIDDEN' : 'INTERNAL', message: status === 403 ? '无权访问' : status >= 400 ? '观测数据读取失败' : 'success', data: status < 400 ? data : undefined, timestamp: at})} as Response
}
type Route = (call: Call) => unknown | Promise<unknown>
async function start(route: Route = () => undefined): Promise<void> {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const call = {method: init?.method ?? 'GET', path: url.pathname, query: url.searchParams}
    calls.push(call)
    const value = await route(call)
    if (value instanceof Response || typeof value === 'object' && value !== null && 'status' in value && 'json' in value) return value
    const defaults: Record<string, unknown> = {overview, timeline, context, metrics, alerts: {enabled: true, rules: [], transitions: [], truncated: false}, export: exportStatus, cleanup}
    return response(value === undefined ? (url.pathname.startsWith('/api/v1/observability/') ? defaults[url.pathname.split('/').at(-1) ?? ''] : {}) : value)
  }))
  window.history.replaceState(null, '', '/console/login?next=' + encodeURIComponent(observabilityHref('query')))
  wrapper = mount(createConsoleApp(), {attachTo: document.body})
  await flushPromises()
  await wrapper.find('#console-token').setValue('synthetic-console-admin')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
  expect(wrapper.find('#console-navigation').exists()).toBe(true)
}
async function visit(view: string, anchor?: readonly [string, string]): Promise<void> {
  window.history.pushState(null, '', observabilityHref(view, anchor))
  window.dispatchEvent(new PopStateEvent('popstate'))
  await flushPromises()
}
const obsCalls = () => calls.filter(c => c.path.startsWith('/api/v1/observability/'))
afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.unstubAllGlobals(); document.body.innerHTML = '' })

describe('US-16A observation management', () => {

  it('offers explicit one, seven and thirty day windows without forwarding UI-only parameters', async () => {
    await start(); await visit('overview')
    expect((wrapper!.find('#obs-range').element as HTMLSelectElement).value).toBe('1d')
    expect(wrapper!.find('#obs-from').exists()).toBe(false)
    for (const [preset, days] of [['7d', 7], ['30d', 30]] as const) {
      await wrapper!.find('#obs-range').setValue(preset)
      await wrapper!.find('form.filters').trigger('submit'); await flushPromises()
      const query = [...obsCalls()].reverse().find(call => call.path.endsWith('/overview'))!.query
      expect(Date.parse(query.get('to')!) - Date.parse(query.get('from')!)).toBe(days * 86400000)
      expect(query.has('range')).toBe(false)
    }
  })

  it('validates custom history bounds and does not hide old anchored runs behind a one-day filter', async () => {
    await start(); await visit('query', ['session_id', 'historical-session'])
    expect((wrapper!.find('#obs-range').element as HTMLSelectElement).value).toBe('all')
    expect(obsCalls().at(-1)?.query.has('from')).toBe(false)
    await wrapper!.find('#obs-range').setValue('custom')
    await wrapper!.find('#obs-from').setValue('2026-10-01T00:00:00')
    await wrapper!.find('#obs-to').setValue('2026-11-01T00:00:00')
    const before = obsCalls().length
    await wrapper!.find('#obs-query-form').trigger('submit'); await flushPromises()
    expect(wrapper!.text()).toContain('不超过30天')
    expect(obsCalls()).toHaveLength(before)
  })

  it('clears stale deep-link dates when the selected anchored range is all history', async () => {
    await start()
    window.history.pushState(null, '', '/console/observability?view=query&session_id=historical-session&range=all&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z')
    window.dispatchEvent(new PopStateEvent('popstate')); await flushPromises()
    expect((wrapper!.find('#obs-range').element as HTMLSelectElement).value).toBe('all')
    expect(obsCalls().at(-1)?.query.has('from')).toBe(false)
    expect(obsCalls().at(-1)?.query.has('to')).toBe(false)
  })

  it('shows local run counts, model, tokens, operation code and recovery provenance', async () => {
    const run = {...timeline.entries[0], attrs: {rounds: 2, tool_calls: 1}}
    const calls = [
      {...run, source: 'llm_calls', source_id: 'llm-recovered', kind: 'llm_call', status: 'unknown', ended_at: null, attrs: {model: 'synthetic-chat', prompt_tokens: 0, completion_tokens: 0, recovered_at: at}},
      {...run, source: 'tool_invocations', source_id: 'tool-legacy', kind: 'tool_call', status: 'unknown', ended_at: null, attrs: {}},
      {...run, source: 'embedding_calls', source_id: 'embedding', kind: 'embedding', attrs: {model: 'synthetic-embedding'}},
      {...run, source: 'management_operations', source_id: 'operation', kind: 'management_operation', attrs: {result_code: 'OK'}}
    ]
    await start(c => c.path.endsWith('/observability/timeline') ? {...timeline, entries: [run, ...calls]} : undefined)
    await visit('run', ['run_id', 'real-run'])
    const text = wrapper!.text()
    for (const expected of ['轮次', '工具调用', '耗时（毫秒）', 'synthetic-chat', 'synthetic-embedding', '操作结果码', '输入 token', '输出 token', '恢复时间仅作观测', '恢复时间未记录，导出不保证']) expect(text).toContain(expected)
    const recovered = wrapper!.findAll('tbody tr').find(row => row.text().includes('llm-recovered'))!
    expect(recovered.text()).toContain('输入 token0')
    expect(recovered.text()).toContain('输出 token0')
  })

  it('does not call a diagnostic-only retrieval fallback recorded empty assembly', async () => {
    await start(c => c.path.endsWith('/observability/context') ? {...context, retrieval: {candidates: [], hits: [], reason_codes: [], top_k: null, candidate_k: null, budget_tokens: null, filtered_count: 5, filtered_reason: 'not_distinguished', index_generation: null, evidence: {assembly: 'insufficient', filtered_count: 'direct'}, filtered_note: ''}, gaps: [{source: 'context_assembly_records', reason: 'not_recorded', detail: ''}]} : undefined)
    await visit('run', ['run_id', 'real-run'])
    expect(wrapper!.text()).not.toContain('知识库无结果')
    expect(wrapper!.text()).not.toContain('本次检索装配证据已记录')
    expect(wrapper!.text()).toContain('检索命中未记录')
    expect(wrapper!.text()).toContain('5')
  })

  it('shows nullable retry schedule and HTTP response in technical details', async () => {
    await start(c => c.path.endsWith('/observability/export') ? {...exportStatus, retry_attempts: 2, next_retry_at: at, last_http_status: 503} : undefined)
    await visit('export')
    expect(wrapper!.text()).toContain('当前待重试项再次认领次数（合计）')
    expect(wrapper!.text()).toContain('最早下次尝试时间')
    expect(wrapper!.text()).toContain('HTTP 响应码')
    expect(wrapper!.text()).toContain('503')
  })
  it('shows a recorded empty retrieval as knowledge with no results without inventing no_answer', async () => {
    await start(c => c.path.endsWith('/observability/context') ? {...context, retrieval: {candidates: [], hits: [], reason_codes: [], top_k: 5, candidate_k: 20, budget_tokens: 1000, filtered_count: 0, filtered_reason: 'not_distinguished', index_generation: 1, evidence: {assembly: 'direct', filtered_count: 'direct', filtered_reason: 'insufficient'}, filtered_note: ''}} : undefined)
    await visit('run', ['run_id', 'real-run'])
    expect(wrapper!.text()).toContain('知识库无结果')
    expect(wrapper!.text()).not.toContain('系统正常')
    expect(wrapper!.html()).not.toContain('no_answer')
  })
  it('does not turn an empty export ledger into a zero-record sending batch', async () => {
    await start(c => c.path.endsWith('/observability/export') ? {...exportStatus, last_batch_id: '', last_record_count: 0, last_record_unit: '', last_outcome: '', last_partial_at: null, unknown_attempts: 0} : undefined)
    await visit('export')
    expect(wrapper!.text()).toContain('尚无发送批次记录')
    expect(wrapper!.text()).not.toContain('批次记录数0')
  })
  it('ignores an old permission failure after a refresh has returned current evidence', async () => {
    let release: (value: unknown) => void = () => undefined
    let requests = 0
    await start(c => c.path.endsWith('/observability/timeline') && ++requests === 1 ? new Promise(resolve => {release = resolve}) : undefined)
    await visit('run', ['run_id', 'real-run'])
    await wrapper!.findAll('button').find(n => n.text() === '刷新')!.trigger('click')
    await flushPromises()
    expect(wrapper!.text()).toContain('S1')
    release(response(undefined, 403)); await flushPromises()
    expect(wrapper!.text()).toContain('S1')
    expect(wrapper!.text()).not.toContain('无权访问')
  })

  it('does not let an old permission failure erase an unsubmitted anchor draft', async () => {
    let release: (value: unknown) => void = () => undefined
    await start(c => c.path.endsWith('/observability/timeline') ? new Promise(resolve => {release = resolve}) : undefined)
    await visit('run', ['run_id', 'real-run'])
    await wrapper!.find('#obs-anchor-value').setValue('draft-run')
    release(response(undefined, 403)); await flushPromises()
    expect(wrapper!.find('#obs-anchor-value').exists()).toBe(true)
    expect((wrapper!.find('#obs-anchor-value').element as HTMLInputElement).value).toBe('draft-run')
    expect(wrapper!.text()).not.toContain('无权访问')
  })
  it('separates metric no-data, missing, unsupported, query failure and measured zero', async () => {
    await start(c => c.path.endsWith('/observability/metrics') ? {...metrics, samples: ['no_data', 'not_recorded', 'not_supported', 'query_failed', ''].map((reason, i) => ({...metrics.samples[0], name: 'sample-' + i, reason, available: i === 4, value: i === 4 ? 0 : null}))} : undefined)
    await visit('metrics')
    const rows = wrapper!.findAll('tbody tr')
    expect(rows[0]!.text()).toContain('无数据')
    expect(rows[1]!.text()).toContain('未记录')
    expect(rows[2]!.text()).toContain('尚不支持')
    expect(rows[3]!.text()).toContain('无法读取')
    expect(rows[4]!.text()).not.toContain('无法读取')
    expect(rows[4]!.findAll('td')[3]!.text()).toBe('0')
  })
  it('distinguishes retrieval visibility filtering from a tool permission refusal', async () => {
    const events = ['retrieval', 'permission_decision'].map((kind, i) => ({...timeline.entries[0], source: 'diagnostic_events', source_id: 'event-' + i, event_id: 'event-' + i, kind, display_step_id: 'S' + (i + 2), reason_code: 'permission_denied', attrs: {count: 5}, status: ''}))
    await start(c => c.path.endsWith('/observability/timeline') ? {...timeline, entries: [...timeline.entries, ...events]} : undefined)
    await visit('query', ['request_id', 'real-request'])
    const retrieval = wrapper!.findAll('tbody tr').find(n => n.text().includes('知识检索'))!
    const tool = wrapper!.findAll('tbody tr').find(n => n.text().includes('权限判定'))!
    expect(retrieval.text()).toContain('已按访问范围筛选')
    expect(retrieval.text()).not.toContain('权限拒绝')
    expect(retrieval.text()).toContain('混合过滤计数')
    expect(tool.text()).toContain('权限拒绝')
  })

  it('filters anchored times and sources without renumbering backend steps', async () => {
    await start(c => c.path.endsWith('/observability/timeline') ? {...timeline, entries: timeline.entries.map(e => ({...e, display_step_id: 'S7'}))} : undefined)
    await visit('query')
    await wrapper!.find('#obs-anchor-value').setValue('real-run')
    await wrapper!.find('#obs-range').setValue('custom')
    await wrapper!.find('#obs-from').setValue('2026-11-01T00:00:00')
    await wrapper!.find('#obs-to').setValue('2026-11-01T23:59:00')
    await wrapper!.find('#obs-limit').setValue('25')
    await wrapper!.find('#obs-sources').setValue('runs')
    await wrapper!.find('#obs-query-form').trigger('submit'); await flushPromises()
    const query = [...obsCalls()].reverse().find(c => c.path.endsWith('/timeline'))!.query
    expect(query.get('sources')).toBe('runs')
    expect(query.get('from')).toBe(new Date('2026-11-01T00:00:00').toISOString())
    expect(query.get('limit')).toBe('25')
    expect(wrapper!.text()).toContain('S7')
    expect(wrapper!.text()).not.toContain('S1')
  })
  it('does not claim assembly when the run has no assembly evidence', async () => {
    await start(); await visit('run', ['run_id', 'real-run'])
    const evidence = wrapper!.findAll('section').find(n => n.find('h2').text() === '上下文证据')
    expect(evidence).toBeDefined()
    expect(evidence!.text()).not.toContain('已装配，使用需核对输出')
    expect(evidence!.text()).toContain('没有对话装配记录')
  })

  it('clears old evidence while an anchor edit is unsubmitted and cannot mix it on refresh', async () => {
    await start(); await visit('run', ['run_id', 'real-run'])
    const count = obsCalls().length
    await wrapper!.find('#obs-anchor-value').setValue('draft-run')
    expect(wrapper!.text()).not.toContain('S1')
    const refresh = wrapper!.findAll('button').find(n => n.text() === '刷新')!
    expect(refresh.attributes('disabled')).toBeDefined()
    await refresh.trigger('click'); await flushPromises()
    expect(obsCalls()).toHaveLength(count)
  })

  it('ignores a late response from an earlier anchor after navigation', async () => {
    let release: (value: unknown) => void = () => undefined
    await start(c => c.path.endsWith('/observability/timeline') ? c.query.get('run_id') === 'old-run' ? new Promise(resolve => {release = resolve}) : {...timeline, anchor: {kind: 'run', value: 'new-run'}, entries: timeline.entries.map(e => ({...e, source_id: 'new-run'}))} : undefined)
    await visit('run', ['run_id', 'old-run'])
    await visit('run', ['run_id', 'new-run'])
    release({...timeline, anchor: {kind: 'run', value: 'old-run'}, entries: timeline.entries.map(e => ({...e, source_id: 'old-run'}))})
    await flushPromises()
    expect(wrapper!.html()).toContain('new-run')
    expect(wrapper!.html()).not.toContain('old-run')
  })
  it('keeps the overview concise and unavailable metrics distinct from healthy zero', async () => {
    await start(); await visit('overview')
    expect(wrapper!.text()).toContain('该时间窗没有 Run 记录')
    expect(wrapper!.text()).not.toContain('系统正常')
    expect(obsCalls().map(c => c.path)).not.toContain('/api/v1/observability/metrics')
    await visit('metrics')
    expect(wrapper!.text()).toContain('无法读取')
    expect(obsCalls().map(c => c.path)).toContain('/api/v1/observability/metrics')
    expect(obsCalls().every(c => c.method === 'GET')).toBe(true)
  })

  it('drills down every bounded run and exposes merged groups without a representative run', async () => {
    await start(c => c.path.endsWith('/observability/overview') ? {...overview, runs: 3, failed: 2, failed_ratio: 2 / 3, groups: [{agent: 'agent-a', total: 2, failed: 1, window_start: at, window_end: at, top_reasons: [{reason: 'timeout', count: 1}], run_ids: ['run-a', 'run-b']}], merged_groups: 2, other_group: {agent: '其他', total: 1, failed: 1, window_start: at, window_end: at, top_reasons: [], run_ids: ['run-c']}} : undefined)
    await visit('groups')
    const links = wrapper!.findAll('a').map(a => a.attributes('href'))
    for (const id of ['run-a', 'run-b', 'run-c']) expect(links).toContain(observabilityHref('run', ['run_id', id]))
    expect(wrapper!.text()).toContain('合并了 2 个组')
    expect(wrapper!.text()).not.toContain('代表 Run')
  })

  it('separates timeline, context evidence, mixed counts and collapsed technical identities', async () => {
    await start(c => c.path.endsWith('/observability/context') ? {...context, retrieval: {candidates: [{chunk_id: 'allowed', version_id: 'v1', content_hash: 'hash', token_estimate: 2, retrieval_score: .5, rerank_score: null, rank: 1, decision: 'kept', acl_decision: 'allow', content: 'CANARY_UNKNOWN_BODY'}], hits: ['allowed'], filtered_count: 5, filtered_reason: 'not_distinguished', top_k: null, candidate_k: 0, budget_tokens: null, index_generation: null, reason_codes: [], evidence: {model_use: 'needs_output_check'}, filtered_note: '混合过滤计数；不代表被过滤文档数或当前查询命中数'}, rounds: [{step_id: 'chat:1', candidates: [], hits: [], reason_codes: [], budget: null, final_segments: ['knowledge'], history_trim: null, compression: null, evidence: {model_use: 'needs_output_check'}, note: '已装配，使用需核对输出'}]} : c.path.endsWith('/observability/timeline') ? {...timeline, entries: [...timeline.entries, {source: 'diagnostic_events', source_id: 'event-real', event_id: 'event-real', schema_version: 0, kind: 'future_kind', display_step_id: 'S2', occurred_at: at, ended_at: null, end_source: 'unknown', status: '', reason_code: 'future_reason', evidence: 'direct', attrs: {content: 'CANARY_UNKNOWN_ATTR'}, redacted_fields: 2}]} : undefined)
    await visit('run', ['run_id', 'real-run'])
    await wrapper!.findAll('.obs-view-switch button')[2]!.trigger('click')
    expect(wrapper!.text()).toContain('已装配，使用需核对输出')
    expect(wrapper!.text()).toContain('混合过滤计数')
    expect(wrapper!.text()).toContain('未记录')
    expect(wrapper!.text()).toContain('未知事件')
    expect(wrapper!.text()).toContain('有 2 个未展示字段')
    expect(wrapper!.findAll('details').length).toBeGreaterThan(0)
    expect(wrapper!.findAll('details').every(n => n.attributes('open') === undefined)).toBe(true)
    expect(wrapper!.html()).not.toContain('CANARY')
  })

  it('requires one anchor before querying and never leaves stale rows on read errors', async () => {
    await start(c => c.path.endsWith('/observability/timeline') && c.query.get('operation_id') === 'missing' ? response(undefined, 500) : undefined)
    await visit('query')
    expect(obsCalls()).toHaveLength(0)
    await wrapper!.find('#obs-anchor-value').setValue('real-task')
    await wrapper!.find('#obs-anchor-kind').setValue('task_id')
    await wrapper!.find('#obs-query-form').trigger('submit'); await flushPromises()
    expect(obsCalls().at(-1)?.query.get('task_id')).toBe('real-task')
    expect(obsCalls().at(-1)?.query.has('run_id')).toBe(false)
    await visit('query', ['operation_id', 'missing'])
    expect(wrapper!.text()).toContain('观测数据读取失败')
    expect(wrapper!.text()).not.toContain('real-run')
    expect(wrapper!.text()).not.toContain('没有可展示记录')
  })

  it('shows source-specific gaps and does not claim a cleaned window proves a row deletion', async () => {
    await start(c => c.path.endsWith('/observability/timeline') ? {...timeline, entries: [], gaps: [{source: 'llm_calls', reason: 'query_failed', detail: '无法查询'}, {source: 'diagnostic_events', reason: 'sampled_out', detail: '采样可能省略'}, {source: 'context_assembly_records', reason: 'retention_window_cleaned', detail: '该时间窗发生过装配记录清理；当前记录是否因此缺失，无法确认'}]} : undefined)
    await visit('run', ['run_id', 'real-run'])
    expect(wrapper!.text()).toContain('读取失败')
    expect(wrapper!.text()).toContain('可能被采样省略')
    expect(wrapper!.text()).toContain('无法确认')
  })

  it('keeps unknown alerts and disabled candidates distinct from recovery', async () => {
    await start(c => c.path.endsWith('/observability/alerts') ? {enabled: true, rules: [{rule_id: 'audit_write_failed', enabled: true, window_seconds: 300, for_seconds: 0, states: [{rule_id: 'audit_write_failed', scope_key: 'process', state: 'unknown', since: null, last_eval_at: null, value: 0, reason: 'state_store_unavailable'}]}, {rule_id: 'run_failure_burst', enabled: false, window_seconds: 900, for_seconds: 900, states: [{rule_id: 'run_failure_burst', scope_key: 'global', state: 'unknown', since: null, last_eval_at: null, value: 0, reason: 'disabled'}]}], transitions: [], truncated: false} : undefined)
    await visit('alerts')
    expect(wrapper!.text()).toContain('状态存储不可用')
    expect(wrapper!.text()).toContain('未启用')
    expect(wrapper!.text()).toContain('不发送外部通知')
    expect(wrapper!.text()).toContain('重启前证据不可用')
    expect(wrapper!.text()).not.toContain('已恢复')
    expect(wrapper!.find('details').text()).toContain('本次读数未确认')
  })

  it('keeps unknown export counts, duplicate attempts, units and uncovered history explicit', async () => {
    await start(); await visit('export')
    for (const text of ['导出未启用', '此前结果未知，可能重复', '未确认', '永久失败', '丢弃', '过期', '数据点', '早于覆盖起点']) expect(wrapper!.text()).toContain(text)
    expect(wrapper!.find('details').text()).toContain('http/protobuf')
    expect(wrapper!.find('details').attributes('open')).toBeUndefined()
  })

  it('shows business retention groups and keeps row counts separate from group counts', async () => {
    await start(c => c.path.endsWith('/observability/cleanup') ? {...cleanup, history_enabled:true, history_days:30, history:[{id:'h1',group:'sessions',at,cutoff:at,deleted_groups:2,deleted_rows:25,protected_groups:3,status:'succeeded',reason:'active_unresolved_or_referenced'}]} : undefined)
    await visit('cleanup')
    for (const expected of ['超过 30 天','最后活动时间','长期记忆','凭据','删除组数','删除行数','保护组数','闲置会话及完整运行']) expect(wrapper!.text()).toContain(expected)
    expect(wrapper!.findAll('tr').find(row=>row.text().includes('闲置会话及完整运行'))?.text()).toContain('25')
    expect(obsCalls().every(call=>call.method==='GET')).toBe(true)
  })

  it('shows disabled cleanup and a skipped unenumerated window without inventing no candidates', async () => {
    await start(); await visit('cleanup')
    expect(wrapper!.text()).toContain('清理未启用，数据不会被自动删除')
    expect(wrapper!.text()).toContain('本轮未枚举候选')
    expect(wrapper!.text()).toContain('不能证明具体记录被删除')
    expect(obsCalls().every(c => c.method === 'GET')).toBe(true)
  })

  it('renders permission refusal without showing stale identifiers or success-empty copy', async () => {
    await start(c => c.path.endsWith('/observability/timeline') ? response(undefined, 403) : undefined)
    await visit('query', ['request_id', 'sensitive-request-id'])
    expect(wrapper!.text()).toContain('无权访问')
    expect(wrapper!.text()).not.toContain('sensitive-request-id')
    expect(wrapper!.text()).not.toContain('没有可展示记录')
  })
})
