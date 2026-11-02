import { afterEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, h, ref } from 'vue'
import type { VueWrapper } from '@vue/test-utils'
import type { ObsTimeline, ObsTimelineEntry } from '../src/api-observability'
import { diagnosticTimeline, durationText, metricDashboard, runSummary } from '../src/observation-presentation'
import type { TimelineMode } from '../src/observation-presentation'

const at = '2026-11-02T00:37:16.373Z'
const entry: ObsTimelineEntry = {source: 'runs', source_id: 'fixture-run', kind: 'run', display_step_id: 'S1', occurred_at: at, ended_at: '2026-11-02T00:37:17.465Z', end_source: 'recorded', status: 'completed', evidence: 'direct', attrs: {rounds: 1, tool_calls: 0}, redacted_fields: 0}
const timeline = (entries: ObsTimelineEntry[]): ObsTimeline => ({anchor: {kind: 'run', value: 'fixture-run'}, entries, gaps: [], order_scope: 'anchor-local', truncated: false, projected_at: at})
let wrapper: VueWrapper | undefined
afterEach(() => { wrapper?.unmount(); wrapper = undefined })

describe('observation default readable diagnosis', () => {
  it('shows measured parent and child stages with summaries without inventing first-token timing', () => {
    const phase = (id: string, name: string, parent: string, duration: number, kind = 'retrieval'): ObsTimelineEntry => ({...entry, source: 'diagnostic_events', source_id: id, display_step_id: id, kind, attrs: {phase: name, phase_parent: parent, duration_ms: duration}, ended_at: new Date(Date.parse(at) + duration).toISOString()})
    const events = [phase('P1', 'knowledge_retrieval', '', 10), phase('P2', 'lexical_search', 'knowledge_retrieval', 2), phase('P3', 'lexical_search', 'knowledge_retrieval', 4), phase('P4', 'context_prepare', '', 5, 'assembly'), phase('P5', 'model_first_response', 'model_call', 20, 'llm_call'), phase('P6', 'model_response_read', 'model_call', 3, 'llm_call')]
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([entry, ...events]), 'execution', () => {})}))
    const breakdown = wrapper.find('.obs-phase-breakdown')
    const keyword = breakdown.findAll('tbody tr').find(row => row.text().includes('关键词检索'))!
    expect(keyword.findAll('td').map(cell => cell.text()).slice(0, 5)).toEqual(['↳ 关键词检索', '2 次', '6 毫秒', '3 毫秒', '4 毫秒'])
    expect(keyword.element.closest('details')?.hasAttribute('open')).toBe(false)
    expect(breakdown.text()).toContain('不能再加到父阶段或总时长上')
    expect(breakdown.text()).toContain('非首个 token')
    const measured = wrapper.find('#obs-step-P5')
    expect(measured.findAll('td')[2]!.text()).toBe('20 毫秒')
    expect(measured.findAll('td')[1]!.text()).not.toContain('瞬时事件')
    expect(measured.text()).toContain('不等于首个 token 等待')
  })

  it('keeps old retrieval events instantaneous and separates model purposes from duplicate diagnostics', () => {
    const old: ObsTimelineEntry = {...entry, source: 'diagnostic_events', kind: 'retrieval', display_step_id: 'old', ended_at: null, attrs: {}}
    const model: ObsTimelineEntry = {...entry, source: 'llm_calls', kind: 'llm_call', attrs: {duration_ms: 0, purpose: 'compaction'}}
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([entry, old, model]), 'all', () => {})}))
    expect(wrapper.find('.obs-phase-breakdown').text()).toContain('历史运行不会补算')
    expect(wrapper.find('#obs-step-old').text()).toContain('瞬时事件')
    const summary = wrapper.find('.obs-phase-breakdown details')
    expect(summary.text()).toContain('会话压缩模型')
    expect(summary.findAll('tbody tr')).toHaveLength(1)
    expect(summary.findAll('td')[2]!.text()).toBe('0 毫秒')
  })

  it('keeps measured duration visible when recorded wall-clock endpoints disagree', () => {
    const phase: ObsTimelineEntry = {...entry, source: 'diagnostic_events', kind: 'assembly', display_step_id: 'clock', ended_at: new Date(Date.parse(at) - 1000).toISOString(), attrs: {phase: 'context_prepare', duration_ms: 1.25}}
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([phase]), 'execution', () => {})}))
    expect(wrapper.find('#obs-step-clock').text()).toContain('时间边界异常')
    expect(wrapper.find('#obs-step-clock').findAll('td')[2]!.text()).toBe('1.25 毫秒')
  })

  it('treats retrieval filtering as a scoped statistic, not a failed execution', () => {
    const filter: ObsTimelineEntry = {...entry, source: 'diagnostic_events', kind: 'retrieval', status: '', ended_at: null, decision: 'deny', reason_code: 'permission_denied', attrs: {count: 4}}
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([filter]), 'all', () => {})}))
    expect(wrapper.find('.obs-issue-list').exists()).toBe(false)
    expect(wrapper.find('.obs-row-issue').exists()).toBe(false)
    const explanation = wrapper.find('.obs-timeline tbody tr').findAll('td')[3]!
    expect(explanation.text()).toContain('已按访问范围筛选')
    expect(explanation.text()).not.toContain('混合过滤计数')
    expect(wrapper.find('.obs-timeline details').text()).toContain('混合过滤计数 4')
  })

  it('explains the scheduler as an outer interval and shows recorded model costs', () => {
    const start = Date.parse(at), end = start + 17227
    const run = {...entry, ended_at: new Date(end).toISOString()}
    const task: ObsTimelineEntry = {...entry, source: 'task_executions', display_step_id: 'S0', kind: 'task_execution', occurred_at: new Date(start - 19).toISOString(), ended_at: new Date(end + 6).toISOString(), attrs: {run_id: entry.source_id}}
    const durations = [947, 3332, 2567, 2515, 1800, 1200, 2100, 1464]
    const models = durations.map((duration, i): ObsTimelineEntry => ({...entry, source: 'llm_calls', source_id: 'model-' + i, display_step_id: 'S' + (i + 2), kind: 'llm_call', status: 'success', ended_at: null, attrs: {duration_ms: duration}}))
    wrapper = mount(defineComponent({setup: () => () => h('div', [runSummary(timeline([run, task, ...models]))])}))
    expect(wrapper.text()).toContain('两者重叠，不能相加')
    expect(wrapper.text()).toContain('25 毫秒')
    expect(wrapper.text()).toContain('15.93 秒')
    const cards = wrapper.findAll('.obs-stat-grid-run .obs-stat')
    expect(cards[0]!.find('strong').text()).toBe('17.25 秒')
    expect(cards[0]!.text()).toContain('调度开始至调度结束')
    expect(cards[1]!.find('strong').text()).toBe('15.93 秒')
    expect(cards[1]!.text()).toContain('8 / 8 次调用有耗时记录')
    expect(cards[2]!.find('strong').text()).toBe('8 次')
    expect(wrapper.text()).toContain('包含 Agent 运行 17.23 秒')
    expect(wrapper.text()).toContain('3.33 秒')
    expect(wrapper.text()).toContain('不能把差额直接解释为排队')
  })

  it('uses the Agent interval when no scheduler record is returned and explains model timing', () => {
    const model: ObsTimelineEntry = {...entry, source: 'llm_calls', kind: 'llm_call', source_id: 'model-1', ended_at: null, attrs: {duration_ms: 697}}
    wrapper = mount(defineComponent({setup: () => () => h('div', [runSummary(timeline([entry, model]))])}))
    const cards = wrapper.findAll('.obs-stat-grid-run .obs-stat')
    expect(cards.map(c => c.find('span').text())).toEqual(['本次总时长', '模型调用累计耗时', '模型调用次数', '工具调用次数'])
    expect(cards.map(c => c.find('strong').text())).toEqual(['1.09 秒', '697 毫秒', '1 次', '0'])
    expect(cards[0]!.text()).toContain('按 Agent 运行起止计时')
    expect(wrapper.text()).toContain('包含网络传输，不等于模型服务端的纯处理时间')
    expect(wrapper.find('.obs-timing-notice').exists()).toBe(false)
  })

  it('counts failed and pending model audits and compaction once, without counting diagnostic duplicates', () => {
    const models: ObsTimelineEntry[] = [
      {...entry, source: 'llm_calls', source_id: 'model-1', kind: 'llm_call', status: 'success', ended_at: null, attrs: {duration_ms: 0}},
      {...entry, source: 'llm_calls', source_id: 'model-2', kind: 'llm_call', status: 'failed', ended_at: null, attrs: {duration_ms: 1000, purpose: 'compaction'}},
      {...entry, source: 'llm_calls', source_id: 'model-3', kind: 'llm_call', status: 'pending', ended_at: null, attrs: {}},
      {...entry, source: 'diagnostic_events', source_id: 'event-1', kind: 'llm_call', attrs: {duration_ms: 1000}}
    ]
    wrapper = mount(defineComponent({setup: () => () => h('div', [runSummary({...timeline([entry, ...models]), truncated: true})])}))
    const cards = wrapper.findAll('.obs-stat-grid-run .obs-stat')
    expect(cards[1]!.find('strong').text()).toBe('1 秒')
    expect(cards[1]!.text()).toContain('2 / 3 次调用有耗时记录；统计不完整')
    expect(cards[2]!.find('strong').text()).toBe('3 次')
    expect(cards[2]!.text()).toContain('含失败及会话压缩')
  })

  it('preserves missing model data and an unknown scheduler end instead of substituting a smaller total', () => {
    const task: ObsTimelineEntry = {...entry, source: 'task_executions', kind: 'task_execution', ended_at: null, attrs: {run_id: entry.source_id}}
    wrapper = mount(defineComponent({setup: () => () => h('div', [runSummary(timeline([entry, task]))])}))
    const cards = wrapper.findAll('.obs-stat-grid-run .obs-stat')
    expect(cards[0]!.find('strong').text()).toBe('未记录')
    expect(cards[1]!.find('strong').text()).toBe('未记录')
    expect(cards[2]!.find('strong').text()).toBe('未记录')
    expect(wrapper.text()).toContain('包含 Agent 运行 1.09 秒')
  })

  it('does not take the total from another run or treat a recorded zero model duration as missing', () => {
    const task: ObsTimelineEntry = {...entry, source: 'task_executions', kind: 'task_execution', attrs: {run_id: 'other-run', duration_ms: 99999}}
    const model: ObsTimelineEntry = {...entry, source: 'llm_calls', kind: 'llm_call', attrs: {duration_ms: 0}}
    wrapper = mount(defineComponent({setup: () => () => h('div', [runSummary(timeline([entry, task, model]))])}))
    const cards = wrapper.findAll('.obs-stat-grid-run .obs-stat')
    expect(cards[0]!.find('strong').text()).toBe('1.09 秒')
    expect(cards[1]!.find('strong').text()).toBe('0 毫秒')
    expect(cards[2]!.find('strong').text()).toBe('1 次')
  })
  it('puts the supported terminal cause, budget evidence and action outside collapsed details', () => {
    wrapper = mount(defineComponent({setup: () => () => h('div', [runSummary(timeline([{...entry, status: 'failed', reason_code: 'context_budget_exceeded', attrs: {rounds: 9, tool_calls: 19, context_required_tokens: 66171, context_limit_tokens: 65536, context_reserve_tokens: 1024}}]))])}))
    const outcome = wrapper.find('.obs-outcome')
    expect(outcome.text()).toContain('上下文预算超限')
    expect(outcome.text()).toContain('66,171')
    expect(outcome.text()).toContain('65,536')
    expect(outcome.text()).toContain('历史裁剪无法消除不可裁部分')
    expect(outcome.element.closest('details')).toBeNull()
    expect(wrapper.find('.obs-stat-grid').text()).toContain('1.09 秒')
  })

  it('shows policy decisions and point-event semantics instead of missing outcomes', () => {
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([{...entry, source: 'diagnostic_events', kind: 'quota_decision', status: '', ended_at: null, decision: 'allow'}]), 'all', () => {})}))
    const cells = wrapper.find('tbody tr').findAll('td')
    expect(cells[3]!.text()).toContain('放行')
    expect(cells[3]!.text()).not.toContain('未记录')
    expect(cells[1]!.text()).toContain('瞬时事件')
    expect(cells[2]!.text()).toBe('不适用')
  })

  it('presents zero tool duration, recorded model duration and unknown LLM end independently', () => {
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([
      {...entry, source: 'tool_invocations', kind: 'tool_call', status: 'failed', reason_code: 'invalid_input', end_source: 'derived_recorded', attrs: {tool: 'write_file', duration_ms: 0}},
      {...entry, display_step_id: 'S2', source: 'llm_calls', kind: 'llm_call', status: 'success', ended_at: null, attrs: {duration_ms: 697, model: 'test-chat'}}
    ]), 'execution', () => {})}))
    const rows = wrapper.findAll('.obs-timeline tbody tr')
    expect(rows[0]!.findAll('td')[2]!.text()).toBe('0 毫秒')
    expect(rows[0]!.findAll('td')[3]!.text()).toContain('参数无效')
    expect(rows[0]!.findAll('td')[1]!.text()).toContain('记录内派生')
    expect(rows[1]!.findAll('td')[2]!.text()).toBe('697 毫秒')
    expect(rows[1]!.findAll('td')[1]!.text()).toContain('结束时间未单独记录')
    expect(durationText(-1)).toBe('时间异常')
    expect(durationText(null)).toBe('未记录')
  })

  it('warns on conflicts only when the diagnostic references the exact authoritative audit', () => {
    const audit = {...entry, source: 'llm_calls' as const, kind: 'llm_call', status: 'success', audit_ref: 'llm_calls:7'}
    const diagnostic = {...entry, source: 'diagnostic_events' as const, source_id: 'event', kind: 'llm_call', status: '', reason_code: 'llm_call_failed', audit_ref: 'llm_calls:7'}
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([audit, diagnostic]), 'all', () => {})}))
    expect(wrapper.find('[role="alert"]').text()).toContain('诊断记录与权威记录不一致')
    expect(wrapper.find('[role="alert"]').text()).toContain('以审计为准')
    wrapper.unmount()
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([audit, {...diagnostic, audit_ref: 'llm_calls:8'}]), 'all', () => {})}))
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it('switches to all records without renumbering or recounting diagnostic events as calls', async () => {
    const mode = ref<TimelineMode>('execution')
    wrapper = mount(defineComponent({setup: () => () => diagnosticTimeline(timeline([entry, {...entry, source: 'diagnostic_events', kind: 'llm_call', status: '', display_step_id: 'S8'}]), mode.value, value => {mode.value = value})}))
    expect(wrapper.findAll('.obs-timeline tbody tr')).toHaveLength(1)
    await wrapper.findAll('.obs-view-switch button')[2]!.trigger('click')
    expect(wrapper.findAll('.obs-timeline tbody tr')).toHaveLength(2)
    expect(wrapper.find('#obs-step-S8').text()).toContain('事件记录')
    expect(wrapper.find('#obs-step-S8 .badge').text()).toBe('已记录')
  })

  it('exposes metric dimensions, percentile and units with bounded precision; buckets remain separately inspectable', () => {
    const sample = {name: 'obs_llm_call_duration_ms', type: 'gauge' as const, unit: 'ms', value: 5891.899999999999, window: 900e9, scope: 'database' as const, available: true, reason: '', labels: {status: 'success', purpose: 'chat', quantile: '0.95'}}
    wrapper = mount(defineComponent({setup: () => () => metricDashboard({collected_at: at, process_started_at: at, samples: [sample, {...sample, name: 'obs_llm_call_duration_ms_bucket', unit: '1', value: 0, labels: {le: '1000'}}, {...sample, available: false, value: null, reason: 'no_data'}]})}))
    const primary = wrapper.find('.table-wrap')
    expect(primary.text()).toContain('P95')
    expect(primary.text()).toContain('用途：对话')
    expect(primary.text()).toContain('5.89 秒')
    expect(primary.text()).toContain('无样本')
    expect(primary.text()).not.toContain('999999')
    expect(primary.element.closest('details')).toBeNull()
    expect(wrapper.find('details').attributes('open')).toBeUndefined()
    expect(wrapper.text()).toContain('累计桶相互重叠')
  })
})
