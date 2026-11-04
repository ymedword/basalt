import { computed, defineComponent, h, nextTick, onUnmounted, ref, watch } from 'vue'
import { ApiError } from '../api'
import { FlowApi, FLOW_STATUS, flowError, flowHref, flowIdentity, flowTerminal, flowWriteDefinitelyRejected } from '../api-flow'
import type { FlowAgentCandidate, FlowDefinition } from '../api-flow'
import { WorkflowEditor } from './workflow-editor'
import { parseWorkflowJSON, workflowFields } from '../workflow-editor'
import { CodeBlock, DataTable, Notice, formatTimestamp } from '../components/ui'
import { identifier, pageHeading, section, useLoader, viewProps } from './shared'

const textValue = (event: Event): string => (event.target as HTMLInputElement).value
export const flowTime = (at?: number): string => at ? formatTimestamp(new Date(at).toISOString()) : '尚未记录'
export const focusFlowConfirmation = () => { void nextTick(() => document.getElementById('flow-confirm-back')?.focus()) }
export const flowConfirmation = (title: string, details: string[], label: string, disabled: boolean, confirm: () => void, back: () => void) => h('section', { class: 'flow-confirmation', role: 'region', 'aria-label': title, onKeydown: (event: KeyboardEvent) => { if (event.key === 'Escape') { event.preventDefault(); back() } } }, [
  h('h3', title), ...details.map(text => h('p', text)), h('div', { class: 'flow-actions' }, [h('button', { id: 'flow-confirm-back', onClick: back }, '返回修改'), h('button', { class: 'primary', disabled, onClick: confirm }, label)]),
])
export const flowTabs = (active: 'definitions' | 'tasks') => h('nav', { class: 'tabs', 'aria-label': '工作流' }, [
  h('a', { class: ['tab', active === 'definitions' ? 'active' : ''], href: '/console/flows' }, '工作流定义'),
  h('a', { class: ['tab', active === 'tasks' ? 'active' : ''], href: '/console/flow-tasks' }, '执行任务'),
])

export const FlowsView = defineComponent({
  name: 'FlowsView', props: viewProps,
  setup(props) {
    const api = new FlowApi(props.ctx.api)
    const cursor = ref('')
    const definitions = useLoader(() => adminRead(() => api.definitions(true, cursor.value)))
    const agentCursor = ref('')
    const agents = useLoader(() => adminRead(() => api.authoringAgents(agentCursor.value)))
    const catalog = ref<FlowAgentCandidate[]>([])
    watch(agents.data, page => { if (page) catalog.value = agentCursor.value ? [...catalog.value.filter(item => !page.items.some(next => next.name === item.name)), ...page.items] : page.items })
    const bundle = ref('')
    const validated = ref('')
    const error = ref('')
    const message = ref('')
    const busy = ref(false)
    const accessRevoked = ref(false)
    const unresolved = ref('')
    const pendingDraft = ref<{ text: string; label: string } | null>(null)
    const confirmation = ref<{ kind: 'publish'; bundle: string; name: string; uid?: string; version: number } | { kind: 'retire'; item: FlowDefinition } | null>(null)
    let alive = true
    function handleError(caught: unknown) {
      if (caught instanceof ApiError && [401, 403].includes(caught.status)) {
        accessRevoked.value = true
        bundle.value = ''; validated.value = ''; pendingDraft.value = null; confirmation.value = null
        catalog.value = []; definitions.reset(); agents.reset()
      }
      return flowError(caught)
    }
    async function adminRead<T>(read: () => Promise<T>): Promise<T> {
      try { return await read() } catch (caught) { handleError(caught); throw caught }
    }
    const published = computed(() => definitions.data.value?.items ?? [])
    const proposal = computed((): { name?: string; contract?: { goal?: string }; nodes?: unknown[] } | null => {
      try { const parsed: unknown = JSON.parse(bundle.value); return typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed) ? parsed : null } catch { return null }
    })
    const previous = computed(() => published.value.find(item => item.name === proposal.value?.name))
    watch(bundle, () => { validated.value = ''; confirmation.value = null; if (message.value.startsWith('在线校验通过')) message.value = '' })
    props.ctx.router.setLeaveGuard(() => unresolved.value === '' || window.confirm('上次发布结果尚未核对。请保留操作标识；离开不会撤销已提交的发布。仍要离开？'))
    onUnmounted(() => { alive = false; props.ctx.router.setLeaveGuard(null) })
    function replaceDraft(text: string, label: string) {
      if (bundle.value) pendingDraft.value = { text, label }
      else { bundle.value = text; error.value = '' }
    }
    async function loadProcurementTemplate() {
      if (busy.value || unresolved.value || confirmation.value || pendingDraft.value) return
      busy.value = true; error.value = ''
      try {
        const { procurementTemplate } = await import('../workflow-template')
        const collected: FlowAgentCandidate[] = []
        const seen = new Set<string>()
        let after = ''
        for (;;) {
          const page = await adminRead(() => api.authoringAgents(after))
          if (!alive || accessRevoked.value) return
          collected.push(...page.items)
          if (!page.next_cursor) break
          if (seen.has(page.next_cursor) || seen.size >= 100) throw new Error('invalid catalog pagination')
          seen.add(page.next_cursor); after = page.next_cursor
        }
        const result = procurementTemplate(collected)
        if (result.missing.length) { error.value = `请先在 Agents 中配置模型并启用：${result.missing.join('、')}。新实例已准备四个角色；旧实例可先执行 basalt demo workflow install。`; return }
        catalog.value = collected
        replaceDraft(result.text, '采购响应准备模板（17 节点）')
        message.value = '模板已按本次读取的四个 Agent 身份生成。请核对实际角色配置、来源和预算，再校验发布；尚未创建或执行任务。'
      } catch (caught) { if (alive) error.value = handleError(caught) } finally { busy.value = false }
    }
    async function editDefinition(item: FlowDefinition) {
      if (busy.value || unresolved.value || confirmation.value || pendingDraft.value) return
      busy.value = true; error.value = ''
      try {
        const version = await adminRead(() => api.authoringVersion(item.definition_uid, item.version))
        if (!alive || accessRevoked.value) return
        if (version.definition_uid !== item.definition_uid || version.version !== item.version || typeof version.bundle_json !== 'string' || new TextEncoder().encode(version.bundle_json).length > 1_048_576) throw new Error('invalid author version')
        replaceDraft(version.bundle_json, `${item.name} v${item.version} 的编辑副本`)
      } catch (caught) { if (alive) error.value = handleError(caught) } finally { busy.value = false }
    }
    function downloadDraft() {
      if (!bundle.value || busy.value || unresolved.value) return
      const url = URL.createObjectURL(new Blob([bundle.value], { type: 'application/json;charset=utf-8' }))
      const link = document.createElement('a'); link.href = url; link.download = 'basalt-workflow-draft.json'; link.click()
      setTimeout(() => URL.revokeObjectURL(url), 0)
    }
    async function importFile(event: Event): Promise<void> {
      const file = (event.target as HTMLInputElement).files?.[0]
      if (!file || busy.value || unresolved.value || confirmation.value || pendingDraft.value) return
      if (file.size > 1_048_576) { error.value = '导入文件超过 1 MiB，请缩小定义。'; return }
      busy.value = true
      try { const text = await file.text(); if (alive) replaceDraft(text, '导入的草稿') } catch { if (alive) error.value = '无法读取文件，请重选本机 JSON 草稿。' } finally { busy.value = false }
    }
    async function validate(): Promise<void> {
      if (busy.value || pendingDraft.value || unresolved.value) return
      if (!proposal.value) { error.value = '请先填写工作流目标并添加节点，或导入完整定义。'; return }
      busy.value = true; error.value = ''
      const candidate = bundle.value
      try { await api.validate(candidate); if (alive && !accessRevoked.value && candidate === bundle.value) { validated.value = candidate; message.value = '在线校验通过。请核对下面的版本差异，发布只影响新任务。' } } catch (caught) { if (alive) error.value = handleError(caught) } finally { busy.value = false }
    }
    async function publish(): Promise<void> {
      if (busy.value || unresolved.value || !validated.value || validated.value !== bundle.value) return
      const candidate = confirmation.value
      if (!candidate || candidate.kind !== 'publish' || candidate.bundle !== validated.value) return
      confirmation.value = null
      busy.value = true; error.value = ''
      const operation = flowIdentity()
      unresolved.value = operation
      try {
        await api.publish(candidate.bundle, operation, candidate.version, candidate.uid)
        if (!alive || accessRevoked.value) return
        unresolved.value = ''; validated.value = ''; bundle.value = ''; message.value = '发布已提交。新任务将使用此版本。'
        await definitions.reload()
      } catch (caught) {
        error.value = handleError(caught)
        if (flowWriteDefinitelyRejected(caught)) unresolved.value = ''
      } finally { busy.value = false }
    }
    async function retire(item: FlowDefinition): Promise<void> {
      if (busy.value || unresolved.value || confirmation.value?.kind !== 'retire' || confirmation.value.item !== item) return
      confirmation.value = null
      busy.value = true; error.value = ''; unresolved.value = flowIdentity()
      try { await api.retire(item, unresolved.value, '管理台确认退役，禁止新任务'); unresolved.value = ''; await definitions.reload() } catch (caught) { error.value = handleError(caught); if (flowWriteDefinitelyRejected(caught)) unresolved.value = '' } finally { busy.value = false }
    }
    return () => accessRevoked.value ? h(Notice, { tone: 'error', text: '管理身份已失效，已清除页面中的定义和草稿。请重新登录后加载。' }) : h('div', { class: 'view-flows' }, [
      pageHeading('WORKSPACE / WORKFLOWS', '工作流', '组织已有 Agent，连接各阶段的输入与结果；任务可追溯，遇到缺口会暂停核对。'), flowTabs('definitions'),
      definitions.error.value ? h(Notice, { tone: 'error', text: definitions.error.value }) : null,
      section('已发布工作流', [h('button', { disabled: definitions.loading.value || busy.value, onClick: () => { void definitions.reload() } }, '刷新定义'),
        h(DataTable, { headers: ['工作流', '版本', '目标与步骤', '状态', '操作'], empty: definitions.loading.value ? '正在读取…' : '还没有工作流。在下方选择 Agent 并编排，校验后即可发布。', rows: published.value.map(item => ({ key: item.definition_uid + ':' + item.version, cells: [identifier(item.name), `v${item.version}`, h('div', [h('p', item.goal), h('p', item.nodes.map(node => node.id).join(' → '))]), item.retired ? '已退役' : '允许新任务', h('div', { class: 'flow-actions' }, [h('button', { disabled: busy.value || !!unresolved.value || !!confirmation.value || !!pendingDraft.value, onClick: () => { void editDefinition(item) } }, '编辑副本'), item.retired ? '保留历史版本' : h('button', { disabled: busy.value || !!unresolved.value || !!confirmation.value || !!pendingDraft.value, onClick: () => { confirmation.value = { kind: 'retire', item }; focusFlowConfirmation() } }, '退役')])] })) }),
        confirmation.value?.kind === 'retire' ? flowConfirmation('核对退役影响', [`${confirmation.value.item.name} v${confirmation.value.item.version}`, '退役后不能新建任务；历史版本和在途任务会保留。'], '确认退役', busy.value || !!unresolved.value, () => { if (confirmation.value?.kind === 'retire') void retire(confirmation.value.item) }, () => { confirmation.value = null }) : null,
        definitions.data.value?.next_cursor ? h('button', { disabled: definitions.loading.value, onClick: () => { cursor.value = definitions.data.value!.next_cursor!; void definitions.reload() } }, '下一页定义') : null,
      ]),
      section('编排与发布', [
        h('div', { class: 'flow-diff' }, [h('h3', '内置示例 · 企业采购响应准备'), h('p', '4 类 Agent · 17 个顺序节点 · 48 条合成需求。使用固定版本的公开产品资料与合成企业材料，输出内部准备包，不对外投标。'), h('p', '首次使用：配置四个 Agent 的模型，按随包 SETUP 准备知识库与确认记忆，再启用 Agent。模板会绑定当前实际配置；已有用户修改不会被覆盖。'), h('button', { disabled: busy.value || !!unresolved.value || !!confirmation.value || !!pendingDraft.value, onClick: () => { void loadProcurementTemplate() } }, '载入采购准备模板')]),
        h('div', { class: 'flow-actions' }, [h('button', { disabled: !bundle.value || busy.value || !!unresolved.value, onClick: downloadDraft }, '下载草稿'), h('button', { disabled: agents.loading.value || busy.value, onClick: () => { agentCursor.value = ''; agents.reset(); void agents.reload() } }, '刷新 Agent 目录')]),
        h('p', { class: 'muted' }, '草稿仅在当前页面；离开前可下载到本机。导入、编排和在线校验不会自动发布。'),
        agents.error.value ? h(Notice, { tone: 'error', text: agents.error.value }) : null,
        agents.loading.value ? h('p', '正在读取已配置的 Agent…') : null,
        h(WorkflowEditor, { raw: bundle.value, catalog: catalog.value, disabled: busy.value || !!unresolved.value || !!confirmation.value || !!pendingDraft.value, onChange: (raw: string) => { bundle.value = raw } }),
        agents.data.value?.next_cursor ? h('button', { disabled: agents.loading.value, onClick: () => { agentCursor.value = agents.data.value!.next_cursor!; void agents.reload() } }, '加载更多 Agent') : null,
        pendingDraft.value ? flowConfirmation('替换当前草稿', [`将载入${pendingDraft.value.label}。当前未发布内容会被替换；需要保留时请先下载草稿。`], '替换草稿', busy.value || !!unresolved.value, () => { bundle.value = pendingDraft.value!.text; pendingDraft.value = null; error.value = '' }, () => { pendingDraft.value = null }) : null,
        h('details', { class: 'workflow-settings' }, [h('summary', '高级定义原文与文件导入'), h('p', '高级分支、返工和精确数字保留原文；服务端是最终校验依据。'), h(CodeBlock, { text: 'basalt flow validate <工作流目录> --bundle > flow-bundle.json' }),
        h('label', ['导入文件', h('input', { type: 'file', accept: '.json,application/json', disabled: busy.value || !!unresolved.value || !!confirmation.value || !!pendingDraft.value, onChange: (event: Event) => { void importFile(event) } })]),
        h('label', ['完整定义（JSON）', h('textarea', { id: 'flow-bundle', rows: 12, value: bundle.value, disabled: busy.value || !!unresolved.value || !!confirmation.value || !!pendingDraft.value, spellcheck: false, onInput: (event: Event) => { bundle.value = textValue(event) } })]), ]),
        proposal.value ? h('div', { class: 'flow-diff' }, [h('h3', '本次发布差异'), h('p', `${proposal.value.name ?? '未命名'}：${previous.value ? 'v' + previous.value.version + ' → 新版本' : '首次发布'}`), h('p', `当前目标：${previous.value?.goal ?? '无'}`), h('p', `拟发布目标：${proposal.value.contract?.goal ?? '尚未填写'}`), h('p', `步骤数：${previous.value?.nodes.length ?? 0} → ${proposal.value.nodes?.length ?? 0}`)]) : null,
        h('div', { class: 'flow-actions' }, [h('button', { disabled: busy.value || !!unresolved.value || !!confirmation.value || !!pendingDraft.value || !bundle.value, onClick: () => { void validate() } }, '只读在线校验'), h('button', { class: 'primary', disabled: busy.value || !!unresolved.value || !!confirmation.value || !!pendingDraft.value || validated.value !== bundle.value || !validated.value, onClick: () => { confirmation.value = { kind: 'publish', bundle: validated.value, name: proposal.value?.name ?? '', uid: previous.value?.definition_uid, version: previous.value?.version ?? 0 }; focusFlowConfirmation() } }, '核对发布')]),
        confirmation.value?.kind === 'publish' ? flowConfirmation('核对发布影响', [`${confirmation.value.name}：${confirmation.value.version ? 'v' + confirmation.value.version + ' → 新版本' : '首次发布'}`, `拟发布目标：${proposal.value?.contract?.goal ?? ''}`, '只影响新任务；在途任务继续固定旧版本。'], '确认发布', busy.value || !!unresolved.value, () => { void publish() }, () => { confirmation.value = null }) : null,
        unresolved.value ? h(Notice, { tone: 'warn', text: '上次变更结果尚未核对，已禁止再次发布或退役。请保留操作标识，刷新定义并查看管理操作记录。' }) : null,
        unresolved.value ? h('p', ['原操作标识：', h('code', unresolved.value), ' ', h('a', { href: '/console/operations?target_kind=flow_definition' }, '查看管理操作')]) : null,
        message.value ? h(Notice, { text: message.value }) : null, error.value ? h(Notice, { tone: 'error', text: error.value }) : null,
      ]),
    ])
  },
})

export const FlowTasksView = defineComponent({
  name: 'FlowTasksView', props: viewProps,
  setup(props) {
    const api = new FlowApi(props.ctx.api)
    const admin = ref(props.query.get('admin') === '1')
    const cursor = ref('')
    const workflowFilter = ref('')
    const appliedFilter = ref('')
    const tasks = useLoader(() => api.tasks(admin.value, cursor.value, appliedFilter.value))
    const definitions = useLoader(() => api.definitions())
    const filterOptions = computed(() => [...new Set([...(definitions.data.value?.items ?? []).map(item => item.name), ...(tasks.data.value?.items ?? []).map(item => item.definition_name).filter((name): name is string => !!name)])].sort())
    function refreshTasks() { cursor.value = ''; tasks.reset(); void tasks.reload() }
    const selected = ref('')
    const input = ref('{}')
    const identity = ref('')
    const frozen = ref<{ uid: string; input: string } | null>(null)
    const busy = ref(false)
    const uncertain = ref(false)
    const error = ref('')
    let alive = true
    props.ctx.router.setLeaveGuard(() => !busy.value && !uncertain.value || window.confirm('本次受理尚未确认，请先保留幂等标识。离开不会撤销已保存的任务。仍要离开？'))
    onUnmounted(() => { alive = false; props.ctx.router.setLeaveGuard(null) })
    const current = computed(() => definitions.data.value?.items.find(item => item.definition_uid === selected.value))
    const inputFields = computed(() => current.value ? workflowFields(current.value.input_schema) : null)
    const formInput = computed((): Record<string, unknown> | null => {
      if (!inputFields.value) return null
      try {
        const parsed = parseWorkflowJSON(input.value)
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed) || Object.keys(parsed).some(key => !inputFields.value!.some(field => field.name === key))) return null
        return parsed as Record<string, unknown>
      } catch { return null }
    })
    function changeInput(name: string, type: string, event: Event) {
      if (!formInput.value || busy.value || uncertain.value) return
      const next = { ...formInput.value }; const value = textValue(event)
      if (type !== 'string' && value === '') delete next[name]
      else if (type === 'integer') {
        if (!/^-?(0|[1-9]\d*)$/.test(value) || !Number.isSafeInteger(Number(value))) { error.value = '请输入安全范围内的整数；精确大数请使用高级输入。'; return }
        next[name] = Number(value)
      } else next[name] = type === 'boolean' ? value === 'true' : value
      input.value = JSON.stringify(next); error.value = ''
    }
    watch([input, selected], () => { if (!uncertain.value) { identity.value = ''; frozen.value = null } })
    async function start(event: Event): Promise<void> {
      event.preventDefault()
      if (busy.value) return
      error.value = ''
      if (!frozen.value) {
        if (!selected.value) { error.value = '请先选择流程。'; return }
        try { JSON.parse(input.value); frozen.value = { uid: selected.value, input: input.value } } catch { error.value = '输入必须是合法 JSON，请按照该流程的输入要求填写。'; return }
      }
      if (!identity.value) identity.value = flowIdentity()
      busy.value = true
      try { const task = await api.start(frozen.value.uid, frozen.value.input, identity.value); if (!alive) return; uncertain.value = false; busy.value = false; props.ctx.router.navigate(flowHref(task.task_id)) } catch (caught) {
        if (!alive) return
        error.value = flowError(caught)
        uncertain.value = !flowWriteDefinitelyRejected(caught)
        if (!uncertain.value && caught instanceof ApiError && caught.status === 400) { frozen.value = null; identity.value = '' }
      } finally { busy.value = false }
    }
    return () => h('div', { class: 'view-flows' }, [
      pageHeading('WORKSPACE / WORKFLOW TASKS', '工作流执行任务', '选择已发布的工作流，填写本次输入；在这里查看各阶段结果与待核对事项。'), flowTabs('tasks'),
      section('任务列表', [h('div', { class: 'flow-actions' }, [h('button', { disabled: tasks.loading.value, onClick: () => { admin.value = !admin.value; refreshTasks() } }, admin.value ? '切换到我的任务' : '运维视图（安全元数据）'), h('button', { disabled: tasks.loading.value, onClick: refreshTasks }, '刷新任务')]),
        h('form', { id: 'flow-task-filters', class: 'flow-actions', onSubmit: (event: Event) => { event.preventDefault(); appliedFilter.value = workflowFilter.value.trim(); refreshTasks() } }, [
          h('label', ['工作流编号筛选', h('input', { id: 'flow-task-filter', list: 'flow-task-filter-options', value: workflowFilter.value, placeholder: '全部工作流；可选择或输入完整编号', disabled: tasks.loading.value, onInput: (event: Event) => { workflowFilter.value = textValue(event) } })]),
          h('datalist', { id: 'flow-task-filter-options' }, filterOptions.value.map(name => h('option', { value: name }))),
          h('button', { type: 'submit', disabled: tasks.loading.value }, '筛选'),
          appliedFilter.value ? h('button', { type: 'button', disabled: tasks.loading.value, onClick: () => { workflowFilter.value = ''; appliedFilter.value = ''; refreshTasks() } }, '清除筛选') : null,
          h('span', { class: 'muted' }, '按更新时间从新到旧排列'),
        ]),
        tasks.error.value ? h(Notice, { tone: 'error', text: tasks.error.value }) : null,
        admin.value ? h('p', '运维可跨调用方查看执行元数据，不获得原任务的来源、目标或产物阅读权限。') : null,
        h(DataTable, { headers: ['任务', '工作流编号', '状态', '当前节点', '版本', '更新时间'], empty: tasks.loading.value ? '正在读取…' : appliedFilter.value ? '该工作流暂无任务。' : '当前没有流程任务。', rows: (tasks.data.value?.items ?? []).map(task => ({ key: task.task_id, cells: [identifier(task.task_id, flowHref(task.task_id, admin.value)), `${task.definition_name || task.definition_uid} · v${task.definition_version}`, FLOW_STATUS[task.status], task.current_node || (flowTerminal(task.status) ? '路线已结束' : task.status === 'blocked' ? '等待核对' : '尚未开始'), `修订 ${task.version.revision} / 状态 ${task.version.state_version}`, flowTime(task.updated_at)] })) }),
        tasks.data.value?.next_cursor ? h('button', { disabled: tasks.loading.value, onClick: () => { cursor.value = tasks.data.value!.next_cursor!; void tasks.reload() } }, '下一页任务') : null,
      ]),
      section('新建任务', [h('form', { id: 'flow-start', onSubmit: (event: Event) => { void start(event) } }, [
        definitions.error.value ? h(Notice, { tone: 'error', text: definitions.error.value }) : null,
        h('label', ['工作流', h('select', { id: 'flow-definition', value: selected.value, disabled: busy.value || uncertain.value, onChange: (event: Event) => { selected.value = textValue(event) } }, [h('option', { value: '' }, '请选择当前已发布工作流'), ...(definitions.data.value?.items ?? []).map(item => h('option', { value: item.definition_uid }, `${item.name} · v${item.version}`))])]),
        current.value ? h('details', [h('summary', '查看输入要求'), h(CodeBlock, { text: JSON.stringify(current.value.input_schema, null, 2) })]) : null,
        formInput.value ? h('div', { class: 'workflow-task-inputs' }, inputFields.value!.map(field => h('label', [field.description || field.name, field.required ? '（必填）' : '（选填）', field.type === 'boolean' ? h('select', { 'aria-label': field.name, value: formInput.value![field.name] === undefined ? '' : String(formInput.value![field.name]), disabled: busy.value || uncertain.value, onChange: (event: Event) => changeInput(field.name, field.type, event) }, [h('option', { value: '' }, '请选择'), h('option', { value: 'true' }, '是'), h('option', { value: 'false' }, '否')]) : h('input', { 'aria-label': field.name, type: 'text', inputmode: field.type === 'integer' ? 'numeric' : undefined, value: formInput.value![field.name] ?? '', disabled: busy.value || uncertain.value, onInput: (event: Event) => changeInput(field.name, field.type, event) })]))) : null,
        h('details', { open: !formInput.value }, [h('summary', '高级输入（JSON）'), h('label', ['输入参数（JSON）', h('textarea', { id: 'flow-input', rows: 6, value: input.value, disabled: busy.value || uncertain.value, spellcheck: false, onInput: (event: Event) => { input.value = textValue(event) } })])]),
        h('p', '受理成功只表示任务已保存，完成后仍须通过固定验收规则。'),
        identity.value ? h('p', ['本次受理标识：', h('code', identity.value)]) : null,
        uncertain.value ? h(Notice, { tone: 'warn', text: '上次受理结果未确认。输入和标识已冻结；点击查证只会复用原请求，不会换键新建。' }) : null,
        h('button', { type: 'submit', class: 'primary', disabled: busy.value }, busy.value ? '正在确认…' : uncertain.value ? '查证本次受理' : '提交任务'),
        error.value ? h(Notice, { tone: 'error', text: error.value }) : null,
      ])]),
    ])
  },
})
