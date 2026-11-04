import { h, nextTick } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type { ObsMetricSample, ObsMetrics, ObsOverview, ObsTimeline, ObsTimelineEntry } from './api-observability'
import { DataTable, Notice, StatusBadge, formatTimestamp } from './components/ui'
import { diagnosticsHref, observabilityHref, runHref } from './router'
import { isVisibilityFilter, visibilityFilterNote } from './observation-copy'

export const OBS_REASONS: Record<string, string> = {
  timeout: '模型超时', run_timeout: '运行超时', llm_call_failed: '模型调用失败', permission_denied: '权限拒绝',
  sandbox_denied: '执行被沙箱拒绝', budget_trimmed: '按预算裁剪', history_trimmed: '历史被裁剪', group_dropped: '移除较早的完整消息组',
  no_answer: '知识库无结果', vector_unavailable: '向量通道不可用', keyword_unavailable: '关键词通道不可用',
  quota_exhausted: '配额耗尽', concurrency_limited: '并发名额已满', rate_limited: '速率受限', audit_write_failed: '审计写入失败',
  chunk_invalidated: '资料可见性已收窄', verify_failed: '摄取验证失败', parser_failed: '解析失败', chunker_failed: '切分失败',
  embed_failed: '向量化失败', index_failed: '索引失败', cancelled: '已取消', unclassified: '已记录失败，但尚无安全的原因分类',
  external_error: '外部调用失败', invalid_input: '参数无效', runtime_error: '执行时发生错误', not_found: '所需资源不存在',
  notify_disabled: '通知通道未启用', iteration_limit: '达到迭代次数上限', tool_call_limit: '达到工具调用次数上限',
  unknown: '结果仍未知', context_budget_exceeded: '上下文预算超限', none: '未发生', not_needed: '无需处理', not_configured: '尚未配置',
  compaction_timeout: '会话压缩超时', compaction_failed: '会话压缩失败', compaction_over_limit: '压缩后仍超过预算',
  stale: '记录已过时', append_pending: '等待消息写入', no_injectable_chain: '无可注入的摘要链', no_committed_head: '尚无已提交摘要',
  stale_ancestor: '摘要祖先已过时', chain_broken: '摘要链不完整', epoch_raced: '会话代次已变化', append_lock_timeout: '等待会话写入锁超时'
}
export const OBS_RULES: Record<string, string> = {
  audit_write_failed: '审计写入失败', diagnostic_write_failed: '诊断写入失败', export_permanent_failure: '导出永久失败',
  export_backlog_stuck: '导出积压', cleanup_failed: '清理失败', run_failure_burst: '任务失败集中出现',
  llm_timeout_burst: '模型超时集中出现', tool_denial_burst: '工具权限拒绝集中出现'
}
const STATES: Record<string, string> = {
  running: '执行中', pending: '待处理', started: '执行中', completed: '已完成', success: '已完成', succeeded: '已完成',
  failed: '失败', cancelled: '已取消', skipped: '已跳过', unknown: '未知', inactive: '未触发', firing: '告警中', resolved: '已恢复',
  limit_reached: '达到执行限制', recorded: '已记录', sampled_out: '采样省略', write_failed: '写入失败', sent: '已确认发送', retryable: '待重试'
}
const KINDS: Record<string, string> = {
  run: '运行', task_execution: '调度执行', management_operation: '管理操作', ingest_job: '知识摄取', embedding: '向量调用',
  llm_call: '模型调用', tool_call: '工具调用', assembly: '上下文装配', retrieval: '知识检索', permission_decision: '权限判定',
  quota_decision: '配额判定', ingest_stage: '摄取阶段', index_switch: '索引切换', migration: '迁移'
}
const METRICS: Record<string, string> = {
  obs_audit_write_failures_total: '审计写入失败', obs_diagnostic_write_failures_total: '诊断写入失败', obs_export_items_total: '导出条目',
  obs_export_batches_total: '导出批次', obs_export_batch_duration_ms: '导出批次耗时', obs_alert_transitions_total: '告警状态变化',
  obs_cleanup_deleted_rows_total: '已清理记录', obs_cleanup_skipped_rows_total: '清理跳过记录', obs_runs_total: '运行次数',
  obs_run_failures_total: '运行失败次数', obs_llm_calls_total: '模型调用次数', obs_llm_call_duration_ms: '模型调用耗时',
  obs_tool_calls_total: '工具调用次数', obs_diagnostic_events_total: '诊断事件', obs_export_queue_depth: '导出队列',
  obs_export_unconfirmed_items: '未确认导出条目', obs_export_oldest_pending_age_ms: '最老待处理年龄', obs_run_duration_ms: '运行耗时',
  obs_llm_timeout_total: '模型超时', obs_tool_invocations_total: '工具调用', obs_tool_denials_total: '工具权限拒绝',
  obs_retrieval_empty_total: '空检索', obs_assembly_missing_total: '缺装配记录'
}
const PHASES: Record<string, string> = {
  knowledge_retrieval: '知识检索全过程', query_embedding: '查询向量化', retrieval_scope: '访问范围解析', lexical_search: '关键词检索',
  vector_search: '向量检索', retrieval_fusion: '结果融合与预算筛选', knowledge_source_read: '命中内容读取与复核',
  context_prepare: '每轮上下文准备', knowledge_revalidate: '知识权限复核', context_compaction: '会话压缩检查与处理', context_assembly: '上下文装配（含预算与裁剪）',
  model_first_response: '模型首响应等待', model_response_read: '模型响应接收'
}
function measuredPhase(e: ObsTimelineEntry): boolean {
  return e.source === 'diagnostic_events' && typeof e.attrs.phase === 'string' && Boolean(PHASES[e.attrs.phase]) && typeof e.attrs.duration_ms === 'number' && Number.isFinite(e.attrs.duration_ms) && e.attrs.duration_ms >= 0 && Boolean(e.ended_at)
}
export function observationReason(code: string): string { return code ? OBS_REASONS[code] ?? '原因未细分' : '未记录' }
export function readableNumber(value: number): string { return new Intl.NumberFormat('zh-CN', {maximumFractionDigits: 2}).format(value) }
export function durationText(ms: number | null): string {
  if (ms === null || !Number.isFinite(ms)) return '未记录'
  if (ms < 0) return '时间异常'
  if (ms < 1000) return `${readableNumber(ms)} 毫秒`
  if (ms < 60000) return `${readableNumber(ms / 1000)} 秒`
  return `${Math.floor(ms / 60000)} 分 ${readableNumber(ms % 60000 / 1000)} 秒`
}
const time = (value: string | null) => value ? formatTimestamp(value) : '未记录'
const section = (title: string, children: VNodeChild[]) => h('section', {class: 'obs-panel'}, [h('h2', title), ...children])
const note = (text: string, tone: 'info' | 'warn' | 'error' = 'info') => h(Notice, {text, tone})
const badge = (label: string, tone: 'ok' | 'muted' | 'warn' | 'bad' = 'muted') => h(StatusBadge, {label, tone})
const details = (title: string, children: VNodeChild[]) => h('details', {class: 'obs-details'}, [h('summary', title), ...children])
const fields = (values: (readonly [string, VNodeChild])[]) => h('dl', {class: 'definitions'}, values.flatMap(([key, value]) => [h('dt', key), h('dd', [value])]))
const card = (label: string, value: VNodeChild, hint?: string) => h('div', {class: 'obs-stat'}, [h('span', label), h('strong', [value]), hint ? h('small', hint) : null])
const grade = (value: string) => badge(({direct: '直接记录', needs_output_check: '需核对输出', insufficient: '证据不足'} as Record<string, string>)[value] ?? '证据不足')

export function overviewCards(o: ObsOverview): VNode {
  return h('div', {class: 'obs-stat-grid', 'aria-label': '运行概况'}, [
    card('运行次数', readableNumber(o.runs)), card('任务失败', readableNumber(o.failed)),
    card('失败率', o.failed_ratio == null ? '无样本' : `${readableNumber(o.failed_ratio * 100)}%`),
    card('模型超时', readableNumber(o.timeouts)), card('工具权限拒绝', readableNumber(o.tool_denials)),
    card('缺装配证据的 Run', readableNumber(o.missing_assembly))
  ])
}

function entryDuration(e: ObsTimelineEntry): number | null {
  if (typeof e.attrs.duration_ms === 'number') return e.attrs.duration_ms
  if (e.ended_at && e.occurred_at) return Date.parse(e.ended_at) - Date.parse(e.occurred_at)
  return null
}
function entryTitle(e: ObsTimelineEntry): string {
  if (measuredPhase(e)) return `${PHASES[String(e.attrs.phase)]} · 阶段计时`
  if (e.source === 'diagnostic_events') return isVisibilityFilter(e.kind, e.reason_code ?? '') ? '知识检索 · 访问范围筛选' : `${KINDS[e.kind] ?? '未知事件'} · 事件记录`
  if (e.kind === 'llm_call') return e.attrs.purpose === 'compaction' ? '摘要模型（会话压缩）' : '对话模型'
  if (e.kind === 'assembly') return e.attrs.purpose === 'retrieval' ? '检索结果装配' : '对话上下文装配'
  if (e.kind === 'tool_call' && typeof e.attrs.tool === 'string') return `工具 · ${e.attrs.tool}`
  return KINDS[e.kind] ?? '未知事件'
}
function result(e: ObsTimelineEntry): VNode {
  const verdicts: Record<string, string> = {allow: '放行', deny: '拒绝', trim: '裁剪', degrade: '降级'}
  if (isVisibilityFilter(e.kind, e.reason_code ?? '')) return badge('已按访问范围筛选')
  if (e.decision && verdicts[e.decision]) return badge(verdicts[e.decision]!, e.decision === 'allow' ? 'ok' : 'warn')
  if (!e.status) return badge('已记录')
  return badge(STATES[e.status] ?? '未知状态', e.status === 'failed' ? 'bad' : e.status === 'unknown' || e.status === 'pending' ? 'warn' : e.status === 'success' || e.status === 'completed' ? 'ok' : 'muted')
}
function explanation(e: ObsTimelineEntry): string {
  if (measuredPhase(e)) {
    if (e.ended_at && Date.parse(e.ended_at) < Date.parse(e.occurred_at)) return '时间边界异常：结束墙钟早于开始；耗时保留独立的实际计时值。'
    if (e.status === 'skipped') return '条件不满足，后续操作已跳过；耗时为本阶段检查时间，不表示运行失败。'
    if (e.status === 'failed') return '本阶段失败；终止原因以关联运行和调用审计为准。'
    if (e.status === 'cancelled') return '本阶段已取消；耗时为取消前实际经历的时间。'
    if (e.attrs.phase === 'model_first_response') return '从发出请求到收到 HTTP 响应首字节；非流式接口，不等于首个 token 等待。'
    if (e.attrs.phase === 'model_response_read') return '读取并关闭响应体的耗时；不单独表示模型调用最终成功。'
    return e.attrs.phase_parent ? '实际测量的子阶段；已包含在对应父阶段内。' : '实际测量的父阶段；包含其下已执行的子阶段。'
  }
  if (isVisibilityFilter(e.kind, e.reason_code ?? '')) return '检索按当前授权范围筛选内容。这是筛选统计，不表示运行失败；具体统计口径可展开技术详情查看。'
  if (e.reason_code) return (e.attrs.reason_source === 'associated_run' ? '关联运行原因：' : '') + observationReason(e.reason_code)
  if (e.decision === 'allow') return '本次配额或权限检查已放行'
  if (e.kind === 'assembly') return '已记录装配快照；是否被模型使用需核对输出'
  if (e.status === 'success' || e.status === 'completed') return '已完成，未记录错误原因'
  if (e.status === 'failed') return '失败原因未记录'
  return '事件已记录，不单独表示执行成功或失败'
}
function isIssue(e: ObsTimelineEntry): boolean { return !isVisibilityFilter(e.kind, e.reason_code ?? '') && (e.status === 'failed' || e.status === 'unknown' || e.decision === 'deny' || e.decision === 'degrade') }
function point(e: ObsTimelineEntry): boolean { return !measuredPhase(e) && (e.source === 'diagnostic_events' || e.source === 'context_assembly_records') }

function timingRows(groups: [string, ObsTimelineEntry[]][], child = false): {key: string; cells: VNodeChild[]}[] {
  return groups.map(([name, entries]) => {
    const timed = entries.map(entryDuration).filter((n): n is number => n !== null && Number.isFinite(n) && n >= 0)
    const total = timed.reduce((a, b) => a + b, 0)
    return {key: name, cells: [h('span', `${child || entries[0]?.attrs.phase_parent ? '↳ ' : ''}${name}`), `${entries.length} 次`, timed.length ? durationText(total) : '未记录', timed.length ? durationText(total / timed.length) : '未记录', timed.length ? durationText(Math.max(...timed)) : '未记录', `${timed.length} / ${entries.length} 次有耗时；失败及取消 ${entries.filter(e => ['failed', 'cancelled'].includes(e.status)).length} 次`]}
  })
}

function phaseBreakdown(v: ObsTimeline): VNodeChild {
  const phases = v.entries.filter(measuredPhase)
  const models = v.entries.filter(e => e.source === 'llm_calls')
  const tools = v.entries.filter(e => e.source === 'tool_invocations')
  const groupBy = (entries: ObsTimelineEntry[], label: (e: ObsTimelineEntry) => string): [string, ObsTimelineEntry[]][] => {
    const result = new Map<string, ObsTimelineEntry[]>()
    for (const e of entries) { const key = label(e); const group = result.get(key) ?? []; group.push(e); result.set(key, group) }
    return [...result]
  }
  const headers = ['阶段 / 用途', '记录次数', '累计耗时', '平均耗时', '最长一次', '记录覆盖']
  const phaseRows = (entries: ObsTimelineEntry[]) => timingRows(Object.keys(PHASES).map(key => [PHASES[key]!, entries.filter(e => e.attrs.phase === key)] as [string, ObsTimelineEntry[]]).filter(([, rows]) => rows.length))
  const main = phases.filter(e => !e.attrs.phase_parent || e.attrs.phase_parent === 'model_call')
  const children = phases.filter(e => e.attrs.phase_parent && e.attrs.phase_parent !== 'model_call')
  return h('div', {class: 'obs-phase-breakdown'}, [h('h3', '阶段耗时'),
    main.length ? h(DataTable, {headers, rows: phaseRows(main), empty: ''}) : !phases.length ? h('p', {class: 'muted small'}, '当前结果没有阶段计时。历史运行不会补算；新运行按实际执行阶段采集。') : h('p', {class: 'muted small'}, '当前结果仅返回子阶段，未返回父阶段计时。'),
    children.length ? details('展开检索与上下文子阶段', [h(DataTable, {headers, rows: phaseRows(children), empty: ''})]) : null,
    phases.length ? h('p', {class: 'muted small'}, '↳ 为父阶段内的子阶段，不能再加到父阶段或总时长上。首响应等待是 HTTP 首字节等待，非首个 token；响应接收不等于模型生成时间。仅统计当前返回的记录，未执行、采样或截断时可能缺项。') : null,
    models.length || tools.length ? details('模型用途与工具耗时细分', [h(DataTable, {headers, rows: [
      ...timingRows(groupBy(models, e => ({chat: '对话模型', compaction: '会话压缩模型', summary: '摘要模型', rerank: '重排模型'} as Record<string, string>)[String(e.attrs.purpose)] ?? '模型（用途未细分）')),
      ...timingRows(groupBy(tools, e => `工具 · ${typeof e.attrs.tool === 'string' ? e.attrs.tool : '名称未记录'}`))
    ], empty: ''}), h('p', {class: 'muted small'}, '平均耗时只除以有耗时记录的调用数；调用次数包含失败与未完成记录。模型和工具耗时均已发生在本次总时长内。')]) : null
  ])
}

function runEntries(v: ObsTimeline, run: ObsTimelineEntry): ObsTimelineEntry[] {
  return v.anchor.kind === 'run' ? v.entries : v.entries.filter(e => e.attrs.run_id === run.source_id || e.source === 'runs' && e.source_id === run.source_id)
}
function timingSummary(v: ObsTimeline, run: ObsTimelineEntry): VNodeChild {
  const own = runEntries(v, run)
  const task = own.find(e => e.source === 'task_executions' && e.attrs.run_id === run.source_id)
  const total = task ?? run
  const modelCalls = own.filter(e => e.source === 'llm_calls')
  const hasDuration = (e: ObsTimelineEntry) => typeof e.attrs.duration_ms === 'number' && Number.isFinite(e.attrs.duration_ms) && e.attrs.duration_ms >= 0
  const model = modelCalls.filter(hasDuration)
  const tools = own.filter(e => e.source === 'tool_invocations' && hasDuration(e))
  const sum = (values: ObsTimelineEntry[]) => values.reduce((total, e) => total + Number(e.attrs.duration_ms), 0)
  const slow = own.filter(e => ['llm_calls', 'tool_invocations', 'embedding_calls'].includes(e.source) && entryDuration(e) !== null).sort((a,b) => (entryDuration(b) ?? 0) - (entryDuration(a) ?? 0)).slice(0, 3)
  const partialModel = v.truncated || v.gaps.some(g => g.source === 'llm_calls') || model.length !== modelCalls.length
  const nested = task?.ended_at && run.ended_at && Date.parse(task.occurred_at) <= Date.parse(run.occurred_at) && Date.parse(task.ended_at) >= Date.parse(run.ended_at)
  return h('div', [
    h('div', {class: 'obs-stat-grid obs-stat-grid-run', 'aria-label': '本次运行耗时与调用'}, [
      card('本次总时长', durationText(entryDuration(total)), task ? '调度开始至调度结束，包含 Agent 运行' : '按 Agent 运行起止计时；当前未返回调度记录'),
      card('模型调用累计耗时', model.length ? durationText(sum(model)) : '未记录', `${model.length} / ${modelCalls.length} 次调用有耗时记录${partialModel ? '；统计不完整' : ''}`),
      card('模型调用次数', modelCalls.length ? `${readableNumber(modelCalls.length)} 次` : '未记录', '按已加载调用审计计数，含失败及会话压缩'),
      card('工具调用次数', String(run.attrs.tool_calls ?? '未记录'), '按本次 Agent 运行记录计数')
    ]),
    h('p', {class: 'obs-time-range'}, `本次起止：${time(total.occurred_at)} → ${time(total.ended_at)}`),
    task ? h('p', {class: 'obs-timing-notice'}, `本次总时长采用外层调度计时，包含 Agent 运行 ${durationText(entryDuration(run))}；两者重叠，不能相加。${nested ? `记录起止的外围差约 ${durationText((entryDuration(task) ?? 0) - (entryDuration(run) ?? 0))}。` : '时间边界不完全一致，不能直接拆分外围时间。'}`) : null,
    h('p', {class: 'muted small'}, `对话轮次：${String(run.attrs.rounds ?? '未记录')}；工具调用累计耗时：${tools.length ? durationText(sum(tools)) : '未记录'}。`),
    h('p', {class: 'muted small'}, '模型调用耗时是请求发出至返回的等待与响应时间，包含网络传输，不等于模型服务端的纯处理时间。'),
    h('p', {class: 'muted small'}, `${partialModel ? '模型统计不完整；' : ''}模型统计仅基于当前已加载的调用，来源或时间筛选可能省略记录。调用合计可能包含嵌套或重叠，不能与总时长相加；其余阶段未分项计时，不能把差额直接解释为排队或网络等待。`),
    slow.length ? h('div', [h('h3', '耗时较长的调用'), h('ul', {class: 'obs-issue-list'}, slow.map(e => h('li', [h('span', `${e.display_step_id} · ${entryTitle(e)}`), h('span', typeof e.attrs.model === 'string' ? e.attrs.model : typeof e.attrs.tool === 'string' ? e.attrs.tool : '调用记录'), h('strong', durationText(entryDuration(e)))])))]) : null
  ])
}

function sessionSummary(v: ObsTimeline): VNodeChild {
  if (v.anchor.kind !== 'session') return null
  const runs = v.entries.filter(e => e.source === 'runs')
  return section('会话关联的运行', [h('p', {class: 'muted small'}, '会话可包含多次运行。先选具体运行查看终止原因与耗时；以下只统计本次已加载的记录。'),
    h(DataTable, {headers: ['运行', '开始时间', '状态', '终止原因', '总耗时', '定位'], rows: runs.map(run => ({key: run.source_id, cells: [
      run.display_step_id.split('-')[0] ?? run.source_id, time(run.occurred_at), result(run), run.reason_code ? observationReason(run.reason_code) : run.status === 'completed' ? '已完成' : '未记录', durationText(entryDuration(run)),
      h('a', {href: observabilityHref('run', ['run_id', run.source_id]), title: run.source_id}, '打开此次运行')
    ]})), empty: '当前加载范围没有运行记录'})
  ])
}

export function runSummary(v: ObsTimeline): VNodeChild {
  const run = v.anchor.kind === 'run' ? v.entries.find(e => e.source === 'runs') : undefined
  if (!run) return null
  const budget = run.reason_code === 'context_budget_exceeded'
  const advice: Record<string, string> = {
    context_budget_exceeded: '检查当前消息、固定提示及本轮工具输出的大小；历史裁剪无法消除不可裁部分。预算调整前先确认模型容量。',
    timeout: '查看模型调用的耗时与超时记录，再检查模型连接和超时设置。', run_timeout: '查看最后完成的模型或工具步骤，确认任务时限是否足够。',
    external_error: '查看对应外部调用的记录，区分连接问题和服务端失败。', invalid_input: '检查失败步骤的参数格式与工具约束。',
    unclassified: '现有安全分类尚不能解释该错误。可使用运行 ID 在本机服务日志中进一步定位。'
  }
  return section('执行摘要', [
    h('div', {class: ['obs-outcome', run.status === 'failed' ? 'obs-outcome-failed' : '']}, [
      result(run), h('div', [h('h3', run.reason_code ? observationReason(run.reason_code) : run.status === 'completed' ? '本次运行已完成' : STATES[run.status] ?? '运行状态未知'),
        budget && typeof run.attrs.context_required_tokens === 'number' && typeof run.attrs.context_limit_tokens === 'number'
          ? h('p', `正文估算 ${readableNumber(run.attrs.context_required_tokens)} + 预留 ${readableNumber(Number(run.attrs.context_reserve_tokens ?? 0))} = ${readableNumber(run.attrs.context_required_tokens + Number(run.attrs.context_reserve_tokens ?? 0))} tokens，超过总预算 ${readableNumber(run.attrs.context_limit_tokens)} tokens。`) : null,
        run.reason_code && advice[run.reason_code] ? h('p', advice[run.reason_code]) : null])
    ]),
    timingSummary(v, run), phaseBreakdown(v)
  ])
}

export type TimelineMode = 'execution' | 'issues' | 'all'
export function diagnosticTimeline(v: ObsTimeline, mode: TimelineMode, change: (mode: TimelineMode) => void): VNode {
  const issues = v.entries.filter(isIssue)
  const calls = new Map(v.entries.filter(e => e.source !== 'diagnostic_events' && e.audit_ref).map(e => [e.audit_ref, e]))
  const conflicts = v.entries.filter(e => {
    if (e.source !== 'diagnostic_events' || !e.audit_ref || measuredPhase(e)) return false
    const audit = calls.get(e.audit_ref)
    if (!audit) return false
    return (audit.status === 'success' && (e.reason_code === 'llm_call_failed' || e.decision === 'deny' || e.status === 'failed')) ||
      (e.status === 'success' && audit.status === 'failed')
  })
  const selected = v.entries.filter(e => mode === 'all' || (mode === 'issues' ? isIssue(e) : !point(e)))
  return h('div', {class: 'obs-diagnosis'}, [
    runSummary(v), sessionSummary(v),
    v.truncated ? note('当前结果已截断，不能代表全部关联记录。可提高最多条数（上限1000），或从会话运行列表打开单个运行。', 'warn') : null,
    issues.length ? section('异常定位', [
      h('p', {class: 'muted small'}, '先看运行的终止原因，再核对途中失败；单次工具失败不一定导致整次运行失败。'),
      h('ul', {class: 'obs-issue-list'}, issues.map(e => h('li', [h('a', {href: `#obs-step-${e.display_step_id}`, onClick: (event: MouseEvent) => {event.preventDefault(); change('all'); void nextTick(() => document.getElementById(`obs-step-${e.display_step_id}`)?.scrollIntoView?.({block: 'center'}))}}, `${e.display_step_id} · ${entryTitle(e)}`), h('strong', explanation(e)), h('span', durationText(entryDuration(e)))])))
    ]) : section('异常定位', [h('p', '当前查询返回的记录中没有失败或未知结果；是否完整以证据缺口和截断提示为准。')]),
    conflicts.length ? note(`诊断记录与权威记录不一致（${conflicts.length} 条）。两种记录均保留，以审计为准；诊断可能来自旧版本或并发写入。`, 'error') : null,
    ...v.gaps.map(g => note(`${g.source}：${({query_failed: '读取失败', sampled_out: '可能被采样省略', not_recorded: '未记录', not_supported: '该来源尚不支持', retention_window_cleaned: `该时间窗发生过清理，具体缺失原因无法确认。${g.detail}`, unknown: '证据状态未知'} as Record<string, string>)[g.reason] ?? '证据状态未知'}`, 'warn')),
    section('执行时间线', [
      h('div', {class: 'obs-view-switch', role: 'group', 'aria-label': '时间线范围'}, ([['execution', '关键执行'], ['issues', '仅异常'], ['all', '全部记录']] as const).map(([key, label]) => h('button', {type: 'button', 'aria-pressed': mode === key, class: mode === key ? 'primary' : '', onClick: () => change(key)}, label))),
      h('p', {class: 'muted small'}, `显示 ${selected.length} / ${v.entries.length} 条。点事件无持续时间；展开“全部记录”查看配额判定、诊断事件与装配快照。`),
      h('div', {class: 'obs-timeline-scroll'}, [h('table', {class: 'data-table obs-timeline'}, [
        h('thead', h('tr', ['步骤 / 事件', '开始 / 结束（本地时间）', '耗时', '结果 / 原因', '证据与详情'].map(label => h('th', {scope: 'col'}, label)))),
        h('tbody', selected.map(e => h('tr', {id: `obs-step-${e.display_step_id}`, class: isIssue(e) ? 'obs-row-issue' : ''}, [
          h('td', [h('span', {class: 'obs-step'}, e.display_step_id), h('strong', entryTitle(e)), e.step_id ? h('small', `逻辑步骤 ${e.step_id}${e.attempt_id ? ` · 尝试 ${e.attempt_id}` : ''}`) : null]),
          h('td', [h('span', time(e.occurred_at)), h('small', e.ended_at ? `${time(e.ended_at)}${e.end_source === 'derived_recorded' ? ' · 记录内派生' : ''}` : point(e) ? '瞬时事件，不适用结束时间' : '结束时间未单独记录')]),
          h('td', point(e) ? '不适用' : durationText(entryDuration(e))),
          h('td', [result(e), h('p', explanation(e)), e.source === 'task_executions' && e.attrs.run_id ? h('small', '外层调度范围包含关联运行；两者时长重叠，不相加。') : null]),
          h('td', [grade(e.evidence), e.redacted_fields ? h('small', `有 ${e.redacted_fields} 个未展示字段`) : null,
            e.status === 'unknown' && (e.source === 'llm_calls' || e.source === 'tool_invocations') ? h('p', typeof e.attrs.recovered_at === 'string' ? `恢复时间仅作观测：${time(e.attrs.recovered_at)}；不作为调用结束时间。` : '恢复时间未记录，导出不保证。') : null,
            details('技术详情', [fields([
              ['来源', e.source], ['真实来源 ID', e.source_id], ['事件 ID', e.event_id ?? '未记录'], ['事件版本', e.schema_version ?? '未记录'], ['原因码', e.reason_code ?? '未记录'],
              ['判定', e.decision ?? '不适用'], ['步骤 ID', e.step_id ?? '不按轮次 / 未记录'], ['尝试 ID', e.attempt_id ?? '尝试未记录'], ['父步骤 ID', e.parent_step_id ?? '父关系未记录'],
              ...(isVisibilityFilter(e.kind, e.reason_code ?? '') ? [['筛选统计', visibilityFilterNote(e.attrs.count)] as const] : []),
              ['局部序号', e.seq ?? '未记录'], ['审计回指', e.audit_ref ?? '未记录'], ['结束时间来源', e.end_source],
              ...(e.flow ? [['流程任务', h('a', {href: '/console/flow-tasks/' + encodeURIComponent(e.flow.task_id) + '?admin=1'}, e.flow.task_id)], ['流程节点', e.flow.node_id], ['节点尝试', e.flow.node_attempt_id], ['逻辑动作', e.flow.action_id ?? '非工具动作'], ['任务修订 / 执行代次', `${e.flow.revision} / ${e.flow.execution_epoch}`]] as const : []),
              ['记录耗时（毫秒）', e.attrs.duration_ms ?? '未记录'], ['模型', e.attrs.model ?? '不适用'], ['输入 token', e.attrs.prompt_tokens ?? '未记录'], ['输出 token', e.attrs.completion_tokens ?? '未记录'],
              ['操作结果码', e.attrs.result_code ?? '不适用'], ['真实运行关联', typeof e.attrs.run_id === 'string' ? h('a', {href: observabilityHref('run', ['run_id', e.attrs.run_id])}, e.attrs.run_id) : '当前查询锚点']
            ])])])
        ])))
      ])]),
      selected.length ? null : h('p', {class: 'empty'}, '该锚点没有可展示记录'),
      h('p', {class: 'muted small'}, '步骤编号仅用于当前关联范围内的展示；时间顺序不表示因果关系。编号随数据变化可能重排，不是持久标识。时间来自本机墙钟，不能作为跨节点时序保证。'),
      v.truncated ? note('只展示有界前缀，后续记录可能被截断。', 'warn') : null
    ]),
    section('关联', [h('div', {class: 'form-actions'}, [v.anchor.kind === 'task' ? h('p', '请先选择关联运行，再查看诊断明细。') : h('a', {href: diagnosticsHref([v.anchor.kind === 'ingest' ? 'ingest_job_id' : v.anchor.kind + '_id', v.anchor.value])}, '查看诊断明细'), v.anchor.kind === 'run' ? h('a', {href: runHref(v.anchor.value)}, '查看权威运行记录') : null])])
  ])
}

const LABEL_KEYS: Record<string, string> = {status: '状态', trigger: '触发', purpose: '用途', result: '结果', kind: '事件', source_kind: '来源', rule_id: '规则', to_state: '转为', table_kind: '对象', state: '队列状态', reason: '原因', reason_code: '原因'}
function metricLabels(s: ObsMetricSample): string {
  return Object.entries(s.labels).filter(([key]) => key !== 'le' && key !== 'quantile').map(([key, value]) => {
    const text = OBS_RULES[value] ?? STATES[value] ?? KINDS[value] ?? OBS_REASONS[value] ?? ({chat: '对话', compaction: '会话压缩', cron: '定时任务', web: '网页', none: '无'} as Record<string, string>)[value] ?? value
    return `${LABEL_KEYS[key] ?? key}：${text}`
  }).join(' · ') || '全部'
}
function metricBase(name: string): string { return name.replace(/_(bucket|count|sum)$/, '') }
function metricStat(s: ObsMetricSample): string {
  if (s.name.endsWith('_bucket')) return `≤ ${s.labels.le === '+Inf' ? '不限' : durationText(Number(s.labels.le))} 累计桶`
  if (s.name.endsWith('_count')) return '样本数'
  if (s.name.endsWith('_sum')) return '累计耗时'
  if (s.labels.quantile) return `P${Number(s.labels.quantile) * 100}`
  return s.type === 'counter' ? '累计次数' : '窗口读数'
}
function metricValue(s: ObsMetricSample): string {
  if (!s.available || s.value == null) return s.reason === 'no_data' ? '无数据（该窗口无样本）' : s.reason === 'not_recorded' ? '未记录' : s.reason === 'not_supported' ? '该来源尚不支持' : '无法读取'
  return s.unit === 'ms' ? durationText(s.value) : s.unit === 's' ? durationText(s.value * 1000) : readableNumber(s.value)
}
function metricTable(samples: ObsMetricSample[], v: ObsMetrics): VNode {
  return h(DataTable, {headers: ['指标', '对象 / 状态', '统计口径', '读数', '单位', '范围', '详情'], rows: samples.map((s, i) => ({key: `${s.name}-${i}`, cells: [
    METRICS[metricBase(s.name)] ?? s.name, metricLabels(s), metricStat(s), metricValue(s), s.unit === 'ms' || s.unit === 's' ? '时间' : '次 / 条',
    s.scope === 'process' ? '本进程累计' : `${s.window / 1e9 / 60} 分钟窗口`, details('技术详情', [fields([['指标名', s.name], ['类型', s.type], ['单位', s.unit], ['标签', Object.entries(s.labels).map(([k, x]) => `${k}=${x}`).join('；')], ['采集时间', time(v.collected_at)], ['进程起点', time(v.process_started_at)], ['不可用原因', s.reason || '无']])])
  ]})), empty: '该窗口没有指标样本'})
}
export function metricDashboard(v: ObsMetrics): VNode {
  const important = v.samples.filter(s => !s.name.endsWith('_bucket') && (!s.available || s.value !== 0 || Object.keys(s.labels).length === 0)).slice(0, 24)
  const shown = new Set(important)
  const remaining = v.samples.filter(s => !shown.has(s))
  return section('指标读数', [
    h('p', {class: 'muted small'}, '本进程读数会在重启后归零；重启前证据不可用。数据库窗口相互重叠，不能相加。P50 / P95 / P99 分别表示50% / 95% / 99%的样本耗时不超过该值。'),
    h('p', {class: 'muted small'}, `默认展示 ${important.length} 项读数；其余读数完整保留在下方明细，不将不同标签或窗口相加。`),
    metricTable(important, v), remaining.length ? details(`其余细分读数与分布桶（${remaining.length} 项）`, [h('p', '累计桶相互重叠，不能相加；零值与无样本分别展示。'), metricTable(remaining, v)]) : null
  ])
}
