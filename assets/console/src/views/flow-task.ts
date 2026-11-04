import { defineComponent, h, onUnmounted, ref } from 'vue'
import { ApiError } from '../api'
import { FlowApi, FLOW_STATUS, flowError, flowHref, flowIdentity, flowTerminal, flowWriteDefinitelyRejected } from '../api-flow'
import type { FlowArtifact, FlowControl, FlowTask, FlowVerification } from '../api-flow'
import { CodeBlock, DataTable, Notice } from '../components/ui'
import { observabilityHref, runHref } from '../router'
import { badge, definitions, identifier, pageHeading, refreshStamp, section, useLiveLoader, viewProps } from './shared'
import { flowConfirmation, flowTabs, flowTime, focusFlowConfirmation } from './flows'

const REASONS: Record<string, string> = {
  node_output_invalid: '节点输出不符合固定格式，未接受为任务结果。', task_output_invalid: '最终输出不符合任务契约，验收未通过。',
  required_verification_missing: '必需的证据或验证尚未满足，请查看下方缺口。', required_artifact_missing: '缺少必需产物，任务未验收。',
  artifact_verification_missing: '产物尚未通过全部必需验证。', artifact_format_invalid: '产物格式不符合任务要求。',
  action_outcome_unknown: '存在结果未查证的动作，已停止推进；不会自动重放。',
  leaf_execution_unconfirmed: '节点运行结果尚未确认，请打开关联运行核对。', leaf_execution_failed: '节点运行失败，请查看关联运行和诊断。',
  run_limit_reached: '节点已达到运行限制，任务停止。', startup_reconciliation_required: '服务曾重启，任务已暂停，需先核对原运行与动作结果。',
  bounded_readonly_retry: '正在按作者规则进行有界只读重试。',
}
const slotLabel: Record<string, string> = { missing: '缺少证据', found: '已找到，未验证', conflict: '来源冲突', unverifiable: '无法验证', verified: '已验证' }
const verificationReasons: Record<string, string> = { independent_sources_missing: '独立来源数量不足，尚未达到验收要求。', schema_valid: '结果符合固定格式要求。', schema_invalid: '结果不符合固定格式要求。', source_support_unavailable: '当前无法核对来源是否支持该结果。', source_unavailable: '当前无法读取或核对所需来源。', human_required: '此规则需要人工核对。' }
const outcomeLabel: Record<string, string> = { pass: '通过', fail: '未通过', inconclusive: '无法确定' }
const attemptLabel: Record<string, string> = { prepared: '已准备', running: '执行中', succeeded: '节点完成', failed: '节点失败', interrupted: '节点未被接受', unknown: '结果未知', yielded: '等待名额' }

export const FlowTaskView = defineComponent({
  name: 'FlowTaskView', props: viewProps,
  setup(props) {
    const api = new FlowApi(props.ctx.api)
    const id = props.params.id ?? ''
    const admin = props.query.get('admin') === '1'
    const active = ref(true)
    const attemptCursor = ref('')
    const artifactCursor = ref('')
    const verificationCursor = ref('')
    const error = ref('')
    const message = ref('')
    const reason = ref('')
    const busy = ref(false)
    const unresolved = ref<{ action: string; body: FlowControl } | null>(null)
    const confirmation = ref<{ action: 'cancel' | 'resume' | 'abandon'; reason: string; etag: string } | null>(null)
    let alive = true
    let clearRead = (): void => {}
    const live = useLiveLoader(async () => {
      try {
        const task = await api.task(id, admin, attemptCursor.value)
        if (!alive || !props.ctx.auth.authenticated) throw new Error('view closed')
        const artifacts = admin ? { items: [] as FlowArtifact[] } : await api.artifacts(id, artifactCursor.value)
        if (!alive || !props.ctx.auth.authenticated) throw new Error('view closed')
        const verifications = admin ? { items: [] as FlowVerification[] } : await api.verifications(id, verificationCursor.value)
        active.value = !flowTerminal(task.status)
        return { task, artifacts, verifications }
      } catch (caught) {
        if (alive && caught instanceof ApiError && [401, 403, 404].includes(caught.status)) {
          // Transient failures retain the last view; explicit loss of authority
          // removes it, so an old private artifact cannot remain on screen.
          clearRead(); active.value = false; error.value = flowError(caught)
        }
        throw caught
      }
    }, { intervalMs: 2000, active })
    clearRead = live.reset
    props.ctx.router.setLeaveGuard(() => !unresolved.value || window.confirm('操作结果尚未核对，请保留原操作标识。离开不会撤销已持久化的关闭或恢复请求。仍要离开？'))
    onUnmounted(() => { alive = false; props.ctx.router.setLeaveGuard(null) })
    async function refresh(): Promise<void> {
      if (!alive) return
      if (!active.value) { active.value = true; return }
      await live.reload()
    }
    function prepareControl(action: 'cancel' | 'resume' | 'abandon'): void {
      const task = live.data.value?.task
      if (!task || busy.value || unresolved.value || live.error.value || flowTerminal(task.status) || !reason.value.trim()) return
      confirmation.value = { action, reason: reason.value.trim(), etag: task.etag }
      focusFlowConfirmation()
    }
    async function control(): Promise<void> {
      const task = live.data.value?.task
      const candidate = confirmation.value
      if (!task || !candidate || busy.value || unresolved.value || live.error.value || flowTerminal(task.status)) return
      if (candidate.etag !== task.etag) { error.value = '任务状态已更新，请返回后重新核对操作。'; return }
      const action = candidate.action
      if (action === 'resume' && (task.unknown_actions || task.cancel_requested || task.close_intent !== 'none' || task.deadline <= Date.now())) return
      confirmation.value = null
      busy.value = true; error.value = ''; message.value = ''
      const body = { operation_id: flowIdentity(), reason: candidate.reason, etag: candidate.etag }
      unresolved.value = { action, body }
      try {
        const result = await api.control(id, action, body)
        if (!alive) return
        unresolved.value = null
        message.value = flowTerminal(result.status) ? `任务状态：${FLOW_STATUS[result.status]}。未知动作与账本仍保留。` : result.close_intent !== 'none' ? '关闭意图已保存，正在等待真实执行退出；当前尚不能认为名额已释放。' : '恢复核对已完成，请以刷新后的任务状态为准。'
        await refresh()
      } catch (caught) {
        if (!alive) return
        error.value = flowError(caught)
        // Unknown commits can be HTTP 409; retain the original operation unless
        // the protocol explicitly proves refusal before the mutation.
        if (flowWriteDefinitelyRejected(caught)) unresolved.value = null
        await refresh()
      } finally { busy.value = false }
    }
    function taskPanels(task: FlowTask) {
      const terminal = flowTerminal(task.status)
      const canResume = !admin && task.status === 'blocked' && !task.unknown_actions && !task.cancel_requested && task.close_intent === 'none' && task.deadline > Date.now()
      const blockedWrite = busy.value || !!unresolved.value || !!live.error.value
      return [
        section('本次任务', [h('div', { class: 'flow-summary' }, [badge(FLOW_STATUS[task.status], task.status === 'succeeded' ? 'ok' : task.status === 'failed' ? 'bad' : 'warn'), identifier(task.task_id)]),
          h('p', { class: 'flow-workflow' }, ['工作流：', h('strong', `${task.definition_name || task.definition_uid} · v${task.definition_version}`)]),
          admin ? h(Notice, { text: '运维视图仅显示执行元数据。管理身份不获得原调用方的目标、来源或产物正文，也不能代替其恢复运行。' }) : h('p', { class: 'flow-goal' }, task.goal),
          task.unknown_actions ? task.status === 'running' && task.close_intent === 'none' && !task.cancel_requested ? h(Notice, { text: '调用结果或用量尚待确认，状态将在返回核对后更新。' }) : h(Notice, { tone: 'warn', text: '存在结果未查证的动作。任务关闭也不代表动作未发生；请人工对账，不会自动重放。' }) : null,
          task.close_intent !== 'none' && !terminal ? h(Notice, { tone: 'warn', text: '正在停止：关闭意图已保存，但在途执行尚未确认全部退出，不能认为名额已释放。' }) : null,
          task.reason ? h(Notice, { tone: task.status === 'failed' ? 'error' : 'info', text: REASONS[task.reason] ?? '任务已记录停止或核对原因，请查看关联节点与验证结果。' }) : null,
          definitions([['当前节点', task.current_node || '路线已结束'], ['定义版本', `${task.definition_uid} · v${task.definition_version}`], ['任务版本', `修订 ${task.version.revision} · 状态 ${task.version.state_version} · 执行代次 ${task.version.execution_epoch}`], ['创建时间', flowTime(task.created_at)], ['固定截止', flowTime(task.deadline)], ['最近状态更新', flowTime(task.updated_at)]]),
          h('p', [h('a', { href: observabilityHref('query', ['flow_task_id', task.task_id]) }, '排查该流程的原运行与诊断')]),
          task.parent_task_id ? h('p', ['父任务：', identifier(task.parent_task_id, flowHref(task.parent_task_id, admin))]) : null,
          !admin ? h('ul', { class: 'flow-constraints' }, task.constraints.map(text => h('li', text))) : null,
          h('details', [h('summary', '核对版本与原因代码'), definitions([['完整条件版本', h('code', task.etag)], ['原因代码', task.reason || '无'], ['关闭意图', task.close_intent === 'none' ? '无' : task.close_intent]])]),
        ]),
        section('预算与证据缺口', [h('div', { class: 'flow-stats' }, [h('div', [h('span', '剩余 token'), h('strong', task.remaining_tokens === undefined ? '未提供' : String(task.remaining_tokens))]), h('div', [h('span', '剩余工具次数'), h('strong', task.remaining_tool_calls === undefined ? '未提供' : String(task.remaining_tool_calls))]), h('div', [h('span', '验收状态'), h('strong', FLOW_STATUS[task.status])])]),
          task.budget_exhausted ? h(Notice, { tone: 'warn', text: '预算已耗尽；未知用量仍保留预留，不会按零用量返还。' }) : null,
          h(DataTable, { headers: ['必需槽位', '当前状态', '依据', '验证'], empty: admin ? '运维元数据视图不展示任务证据。' : '该任务未声明必需证据槽位；验收仍以契约规则为准。', rows: task.slots.map(slot => ({ key: slot.id, cells: [slot.id, slotLabel[slot.state] ?? '状态无法识别', `${slot.evidence_ids.length} 条来源引用`, `${slot.verification_ids.length} 条验证`] })) }),
          h(DataTable, { headers: ['结论', '版本', '状态', '内容'], empty: '当前没有可展示的结论。', rows: task.claims.map(claim => ({ key: claim.claim_id + ':' + claim.claim_revision, cells: [claim.claim_id, String(claim.claim_revision), claim.state === 'invalid' ? '已失效' : claim.state, claim.readable ? claim.text ?? '未记录' : '当前权限不可读取正文'] })) }),
        ]),
        section('顺序节点与原运行', [h('p', '节点跨度包含执行和暂停核对时间，不等于模型调用耗时；恢复核对可能延长跨度。实际执行耗时请打开原运行详情。每个节点复用原运行记录；未准入的运行标识不代表已执行。'),
          h(DataTable, { headers: ['节点 / 尝试', '结果', '开始 / 结束', '节点跨度', '关联'], empty: '尚无节点尝试记录。', rows: task.attempts.map(attempt => ({ key: attempt.node_attempt_id, cells: [h('div', [h('strong', attempt.node_id), h('small', `第 ${attempt.attempt} 次 · 修订 ${attempt.task_revision}`)]), attemptLabel[attempt.state] ?? attempt.state, h('div', [h('p', flowTime(attempt.started_at)), h('p', flowTime(attempt.finished_at))]), attempt.finished_at ? `${Math.max(0, attempt.finished_at - attempt.started_at) / 1000} 秒` : '尚未结束', attempt.run_id && attempt.run_admitted ? h('div', [h('a', { href: runHref(attempt.run_id) }, '运行详情'), h('a', { href: observabilityHref('run', ['run_id', attempt.run_id]) }, '运行排障')]) : attempt.run_id ? '尚未准入，仅有计划标识' : '控制节点，无独立运行'] })) }),
          task.next_attempt_cursor ? h('button', { disabled: live.loading.value, onClick: () => { attemptCursor.value = task.next_attempt_cursor!; void refresh() } }, '下一页节点') : null,
        ]),
        !admin ? section('产物与验收依据', [h('p', '产物记录不代表整个任务已通过验收，请结合任务状态和全部必需规则核对。'), h(DataTable, { headers: ['产物 / 版本', '状态', '内容'], empty: '当前没有产物；尚未产生产物不代表验收成功。', rows: live.data.value!.artifacts.items.map(artifact => ({ key: artifact.artifact_id + ':' + artifact.version, cells: [`${artifact.artifact_id} v${artifact.version} · 修订 ${artifact.task_revision}`, artifact.state === 'invalid' ? '已失效' : artifact.state === 'accepted' ? '已记录' : artifact.state, artifact.readable ? h('details', [h('summary', '查看当前有权读取的内容'), h(CodeBlock, { text: artifact.content ?? '' })]) : artifact.reason === 'artifact_basis_unconfirmed' ? '节点结果的核验依据尚未确认，正文暂不展示。' : '当前来源权限或版本无法核对，正文已隐藏'] })) }),
          live.data.value?.artifacts.next_cursor ? h('button', { disabled: live.loading.value, onClick: () => { artifactCursor.value = live.data.value!.artifacts.next_cursor!; void refresh() } }, '下一页产物') : null,
          h(DataTable, { headers: ['规则 / 版本', '目标版本', '结论', '依据'], empty: '尚无验证记录；来源可读不等于结论受支持。', rows: live.data.value!.verifications.items.map(record => ({ key: record.verification_id, cells: [`${record.rule_id} v${record.rule_version}`, `${record.target_id} v${record.target_version}`, outcomeLabel[record.outcome] ?? '无法确定', h('div', [h('p', verificationReasons[record.reason] ?? '验证结果已记录，请展开原因代码核对。'), h('details', [h('summary', '原因代码与版本摘要'), h(CodeBlock, { text: JSON.stringify({reason: record.reason, basis_hashes: record.basis_hashes}, null, 2) })])])] })) }),
          live.data.value?.verifications.next_cursor ? h('button', { disabled: live.loading.value, onClick: () => { verificationCursor.value = live.data.value!.verifications.next_cursor!; void refresh() } }, '下一页验证') : null,
        ]) : null,
        !terminal ? section('任务操作', [h('label', ['操作理由', h('textarea', { id: 'flow-control-reason', rows: 2, value: reason.value, disabled: blockedWrite || !!confirmation.value, onInput: (event: Event) => { reason.value = (event.target as HTMLTextAreaElement).value } })]),
          h('div', { class: 'flow-actions' }, [admin ? h('button', { disabled: blockedWrite || !!confirmation.value || !reason.value.trim(), onClick: () => { prepareControl('abandon') } }, '放弃任务') : h('button', { disabled: blockedWrite || !!confirmation.value || task.cancel_requested || !reason.value.trim(), onClick: () => { prepareControl('cancel') } }, '取消并关闭'), canResume ? h('button', { disabled: blockedWrite || !!confirmation.value || !reason.value.trim(), onClick: () => { prepareControl('resume') } }, '核对并恢复') : null]),
          confirmation.value ? flowConfirmation('核对任务操作', [task.task_id, `操作理由：${confirmation.value.reason}`, confirmation.value.action === 'resume' ? '先核对原授权、来源和动作结果，再恢复。未知动作不会重放。' : '持久禁止继续推进，随后等待在途执行真正退出。已发送的副作用不会被撤回，未知预留不会自动释放。', ...(confirmation.value.etag !== task.etag ? ['任务状态已更新，请返回后重新核对操作。'] : [])], confirmation.value.action === 'resume' ? '确认恢复' : confirmation.value.action === 'cancel' ? '确认取消并关闭' : '确认放弃', blockedWrite || confirmation.value.etag !== task.etag, () => { void control() }, () => { confirmation.value = null }) : null,
          unresolved.value ? h(Notice, { tone: 'warn', text: '上次操作结果未确认，已锁定新的控制请求。刷新只读取当前状态，不会重发或换键。' }) : null,
          unresolved.value ? h('p', ['原操作标识：', h('code', unresolved.value.body.operation_id)]) : null,
        ]) : null,
      ]
    }
    return () => h('div', { class: 'view-flows' }, [pageHeading('WORKSPACE / WORKFLOW TASK', '工作流任务详情', '验收依据和原运行可追溯；刷新只读取状态，不会继续执行。', h('button', { disabled: live.loading.value, onClick: () => { void refresh() } }, '刷新核对')), flowTabs('tasks'),
      h('p', { class: 'muted' }, refreshStamp(live.lastRefreshedAt.value)),
      live.error.value ? h(Notice, { tone: 'error', text: live.error.value + '；保留最后一次成功读取的数据，当前状态可能已变化。' }) : null,
      message.value ? h(Notice, { text: message.value }) : null,
      error.value ? h(Notice, { tone: 'error', text: error.value }) : null,
      live.data.value ? taskPanels(live.data.value.task) : h('p', live.loading.value ? '正在读取任务…' : '未获得任务数据。'),
    ])
  },
})
