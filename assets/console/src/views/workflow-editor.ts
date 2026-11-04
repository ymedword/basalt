import { computed, defineComponent, h, ref, shallowRef, watch } from 'vue'
import type { PropType } from 'vue'
import type { FlowAgentCandidate } from '../api-flow'
import { addWorkflowNode, adoptWorkflowOutput, canAdoptWorkflowOutput, loadWorkflow, moveWorkflowNode, newWorkflow, removeWorkflowNode, workflowFields, workflowIssues, workflowSchema, workflowSources } from '../workflow-editor'
import type { WorkflowDocument, WorkflowField, WorkflowMapping, WorkflowSchema } from '../workflow-editor'
import { CodeBlock, Notice } from '../components/ui'

const valueOf = (event: Event) => (event.target as HTMLInputElement).value
const memoryLabels = { agent_only: '仅 Agent 核心记忆', global_and_agent: '全局与 Agent 核心记忆', legacy_default: '沿用原记忆配置' }
const unavailable = { ready: '可选择', agent_disabled: '已停用', agent_unregistered: '已移除', resources_unavailable: '资源配置未就绪' }

const WorkflowFields = defineComponent({
  name: 'WorkflowFields',
  props: { schema: { type: Object as PropType<WorkflowSchema>, required: true }, title: { type: String, required: true }, disabled: Boolean },
  emits: { change: (_schema: WorkflowSchema) => true },
  setup(props, { emit }) {
    const name = ref(''); const type = ref<WorkflowField['type']>('string'); const description = ref(''); const required = ref(true); const error = ref('')
    const fields = computed(() => workflowFields(props.schema))
    function change(next: WorkflowField[]) { if (props.disabled) return; try { const schema = workflowSchema(next); emit('change', schema); error.value = ''; name.value = ''; description.value = '' } catch (caught) { error.value = caught instanceof Error ? caught.message : '字段无效' } }
    return () => h('section', { class: 'workflow-fields', 'aria-label': props.title }, [
      h('h4', props.title),
      fields.value === null ? h('p', { class: 'muted' }, '此处使用复杂格式约束，已完整保留；请在高级原文中编辑。') : [
        h('ul', { class: 'workflow-field-list' }, fields.value.map(field => h('li', { key: field.name }, [h('code', field.name), h('span', `${field.type === 'string' ? '文本' : field.type === 'integer' ? '整数' : '是／否'} · ${field.required ? '必填' : '选填'}`), h('span', field.description), h('button', { type: 'button', disabled: props.disabled, 'aria-label': `删除字段 ${field.name}`, onClick: () => change(fields.value!.filter(item => item.name !== field.name)) }, '删除')]))),
        h('div', { class: 'workflow-field-form' }, [
          h('label', ['字段名', h('input', { value: name.value, placeholder: '如 request', disabled: props.disabled, 'aria-label': props.title + '字段名', onInput: (event: Event) => { name.value = valueOf(event) } })]),
          h('label', ['类型', h('select', { value: type.value, disabled: props.disabled, 'aria-label': props.title + '字段类型', onChange: (event: Event) => { type.value = valueOf(event) as WorkflowField['type'] } }, [h('option', { value: 'string' }, '文本'), h('option', { value: 'integer' }, '整数'), h('option', { value: 'boolean' }, '是／否')])]),
          h('label', ['说明', h('input', { value: description.value, disabled: props.disabled, 'aria-label': props.title + '字段说明', onInput: (event: Event) => { description.value = valueOf(event) } })]),
          h('label', { class: 'workflow-checkbox' }, [h('input', { type: 'checkbox', checked: required.value, disabled: props.disabled, onChange: (event: Event) => { required.value = (event.target as HTMLInputElement).checked } }), '必填']),
          h('button', { type: 'button', disabled: props.disabled || !name.value, onClick: () => change([...fields.value!, { name: name.value, type: type.value, required: required.value, description: description.value }]) }, '添加字段'),
        ]),
      ], error.value ? h(Notice, { tone: 'error', text: error.value }) : null,
    ])
  },
})

export const WorkflowEditor = defineComponent({
  name: 'WorkflowEditor',
  props: { raw: { type: String, required: true }, catalog: { type: Array as PropType<FlowAgentCandidate[]>, required: true }, disabled: Boolean },
  emits: { change: (_raw: string) => true },
  setup(props, { emit }) {
    const document = shallowRef<WorkflowDocument | undefined>(newWorkflow())
    const reason = ref(''); const selected = ref(''); const addAgent = ref(''); const dragged = ref(''); const error = ref('')
    const mappingTarget = ref(''); const mappingSource = ref(''); const mappingMode = ref('source'); const literal = ref(''); const literalType = ref('string')
    let emitted = ''
    watch(() => props.raw, raw => {
      if (raw && raw === emitted) return
      emitted = ''
      const loaded = raw.trim() ? loadWorkflow(raw) : { document: newWorkflow() }
      document.value = loaded.document; reason.value = loaded.reason ?? ''; selected.value = loaded.document?.nodes[0]?.id ?? ''; error.value = ''
    }, { immediate: true })
    const active = computed(() => document.value?.nodes.find(node => node.id === selected.value))
    const sources = computed(() => document.value && active.value ? workflowSources(document.value, active.value.id) : [])
    const issues = computed(() => document.value ? workflowIssues(document.value) : [])
    watch(selected, () => { mappingTarget.value = ''; mappingSource.value = ''; literal.value = ''; error.value = '' })
    function commit(next: WorkflowDocument) {
      if (props.disabled) return
      document.value = next; emitted = JSON.stringify(next, null, 2); emit('change', emitted)
    }
    function mutate(fn: (copy: WorkflowDocument) => void) {
      if (!document.value || props.disabled) return
      const copy = structuredClone(document.value); fn(copy); commit(copy)
    }
    function move(id: string, destination: number) { if (document.value && !props.disabled) commit(moveWorkflowNode(document.value, id, destination)) }
    function add() {
      const item = props.catalog.find(agent => agent.name === addAgent.value)
      if (!document.value || !item?.eligible || !item.agent || props.disabled) return
      try {
        const next = addWorkflowNode(document.value, item.agent)
        const node = next.nodes.at(-1)!
        node.inputs = workflowSources(next, node.id).filter(source => source.value.startsWith('input.')).map(source => ({ target: source.value.slice('input.'.length).replaceAll('.', '_'), source: source.value }))
        commit(next); selected.value = node.id; error.value = ''
      } catch (caught) { error.value = caught instanceof Error ? caught.message : '无法添加节点' }
    }
    function addMapping() {
      if (!active.value || props.disabled) return
      let mapping: WorkflowMapping
      if (mappingMode.value === 'source') mapping = { target: mappingTarget.value, source: mappingSource.value }
      else {
        let parsed: string | number | boolean = literal.value
        if (literalType.value === 'integer') {
          if (!/^-?(0|[1-9]\d*)$/.test(literal.value) || !Number.isSafeInteger(Number(literal.value))) { error.value = '请输入安全范围内的整数；精确大数请使用高级原文。'; return }
          parsed = Number(literal.value)
        } else if (literalType.value === 'boolean') parsed = literal.value === 'true'
        mapping = { target: mappingTarget.value, literal: parsed }
      }
      mutate(copy => copy.nodes.find(node => node.id === selected.value)!.inputs.push(mapping))
      mappingTarget.value = ''; mappingSource.value = ''; literal.value = ''; error.value = ''
    }
    function graph() {
      const nodes = document.value!.nodes
      return h('div', { class: 'workflow-canvas', 'aria-label': '顺序工作流画布' }, [
        h('div', { class: 'workflow-endpoint' }, '任务输入'),
        ...nodes.flatMap((node, index) => [
          h('svg', { class: 'workflow-connector', viewBox: '0 0 24 32', 'aria-hidden': 'true' }, [h('path', { d: 'M12 0v28m-5-5 5 5 5-5', fill: 'none', stroke: 'currentColor', 'stroke-width': 1.5 })]),
          h('div', { key: node.id, class: ['workflow-node', selected.value === node.id ? 'selected' : ''], draggable: !props.disabled, onDragstart: (event: DragEvent) => { if (props.disabled) return; dragged.value = node.id; event.dataTransfer?.setData('text/plain', node.id) }, onDragend: () => { dragged.value = '' }, onDragover: (event: DragEvent) => { if (dragged.value && !props.disabled) event.preventDefault() }, onDrop: (event: DragEvent) => { event.preventDefault(); if (dragged.value) move(dragged.value, index); dragged.value = '' } }, [
            h('button', { type: 'button', class: 'workflow-node-select', 'aria-pressed': selected.value === node.id, title: node.id, onClick: () => { selected.value = node.id }, onKeydown: (event: KeyboardEvent) => { if (event.altKey && ['ArrowUp', 'ArrowDown'].includes(event.key)) { event.preventDefault(); move(node.id, index + (event.key === 'ArrowUp' ? -1 : 1)) } } }, [h('span', { class: 'workflow-step' }, String(index + 1).padStart(2, '0')), h('span', [h('strong', props.catalog.find(agent => agent.name === node.agent.name)?.display_name || node.agent.name), h('small', `${node.inputs.length} 个输入字段`)])]),
            h('div', { class: 'workflow-node-tools' }, [h('button', { type: 'button', disabled: props.disabled || index === 0, 'aria-label': `上移 ${node.id}`, onClick: () => move(node.id, index - 1) }, '↑'), h('button', { type: 'button', disabled: props.disabled || index === nodes.length - 1, 'aria-label': `下移 ${node.id}`, onClick: () => move(node.id, index + 1) }, '↓')]),
          ]),
        ]),
        h('svg', { class: 'workflow-connector', viewBox: '0 0 24 32', 'aria-hidden': 'true' }, [h('path', { d: 'M12 0v28m-5-5 5 5 5-5', fill: 'none', stroke: 'currentColor', 'stroke-width': 1.5 })]),
        h('div', { class: 'workflow-endpoint' }, '最终产物与验收'),
        !nodes.length ? h('p', '选择一个已配置好的 Agent，开始编排。') : null,
        h('p', { class: 'muted' }, '按箭头顺序执行。拖动或用 ↑ ↓ 调整；键盘 Alt + ↑ / ↓ 同样可用。'),
      ])
    }
    function nodePanel() {
      const node = active.value!
      const catalog = props.catalog.find(item => item.name === node.agent.name)
      return h('div', { class: 'workflow-node-panel' }, [
        h('div', { class: 'workflow-panel-heading' }, [h('h3', { title: node.id }, `步骤 ${document.value!.nodes.indexOf(node) + 1} · ${catalog?.display_name || node.agent.name}`), h('button', { type: 'button', disabled: props.disabled, onClick: () => { const next = removeWorkflowNode(document.value!, node.id); commit(next); selected.value = next.nodes[0]?.id ?? '' } }, '删除节点')]),
        h('label', ['执行 Agent', h('select', { 'aria-label': '节点 Agent', value: node.agent.name, disabled: props.disabled, onChange: (event: Event) => { const item = props.catalog.find(item => item.name === valueOf(event)); if (item?.eligible && item.agent) mutate(copy => { copy.nodes.find(item => item.id === node.id)!.agent = { ...item.agent! } }) } }, [!catalog ? h('option', { value: node.agent.name, disabled: true }, node.agent.name + '（当前目录中不可用）') : null, ...props.catalog.map(item => h('option', { value: item.name, disabled: !item.eligible }, item.display_name + (item.eligible ? '' : ' · ' + unavailable[item.reason])))])]),
        catalog ? h('div', { class: 'workflow-agent-context' }, [h('p', catalog.description || '此 Agent 未填写说明。'), h('p', `知识库：${catalog.knowledge_collections.join('、') || '未绑定'} · ${memoryLabels[catalog.memory_scope]}`), h('small', '此处展示配置；实际检索和记忆注入以运行记录为准。'), h('p', `工作流可用只读工具：${catalog.readonly_tools.join('、') || '无'}`)]) : null,
        h('h4', '输入连接'), h('p', { class: 'muted' }, '把任务参数或前面节点的已验证输出传给此 Agent。'),
        h('ul', { class: 'workflow-mappings' }, node.inputs.map((mapping, index) => h('li', { key: index }, [h('code', mapping.target || '未命名'), h('span', '←'), h('span', { title: mapping.source }, sources.value.find(source => source.value === mapping.source)?.label || mapping.source || `常量：${JSON.stringify(mapping.literal)}`), h('button', { type: 'button', disabled: props.disabled, 'aria-label': `删除输入 ${mapping.target}`, onClick: () => mutate(copy => { copy.nodes.find(item => item.id === node.id)!.inputs.splice(index, 1) }) }, '删除')]))),
        h('div', { class: 'workflow-mapping-form' }, [
          h('label', ['传入字段名', h('input', { 'aria-label': '传入字段名', value: mappingTarget.value, disabled: props.disabled, placeholder: '如 requirements', onInput: (event: Event) => { mappingTarget.value = valueOf(event) } })]),
          h('label', ['来源类型', h('select', { 'aria-label': '来源类型', value: mappingMode.value, disabled: props.disabled, onChange: (event: Event) => { mappingMode.value = valueOf(event); literal.value = '' } }, [h('option', { value: 'source' }, '任务输入或上游输出'), h('option', { value: 'literal' }, '固定常量')])]),
          mappingMode.value === 'source' ? h('label', ['来源字段', h('select', { 'aria-label': '来源字段', value: mappingSource.value, disabled: props.disabled, onChange: (event: Event) => { mappingSource.value = valueOf(event) } }, [h('option', { value: '' }, '请选择来源'), ...sources.value.map(source => h('option', { value: source.value }, source.label))])]) : [
            h('label', ['常量类型', h('select', { 'aria-label': '常量类型', value: literalType.value, disabled: props.disabled, onChange: (event: Event) => { literalType.value = valueOf(event); literal.value = '' } }, [h('option', { value: 'string' }, '文本'), h('option', { value: 'integer' }, '整数'), h('option', { value: 'boolean' }, '是／否')])]),
            h('label', ['常量', literalType.value === 'boolean' ? h('select', { 'aria-label': '常量', value: literal.value, disabled: props.disabled, onChange: (event: Event) => { literal.value = valueOf(event) } }, [h('option', { value: '' }, '请选择'), h('option', { value: 'true' }, '是'), h('option', { value: 'false' }, '否')]) : h('input', { 'aria-label': '常量', value: literal.value, disabled: props.disabled, onInput: (event: Event) => { literal.value = valueOf(event) } })]),
          ],
          h('button', { type: 'button', disabled: props.disabled || !mappingTarget.value || (mappingMode.value === 'source' && !mappingSource.value) || (mappingMode.value === 'literal' && literalType.value !== 'string' && !literal.value), onClick: addMapping }, '连接输入'),
        ]),
        h(WorkflowFields, { key: node.id, title: '节点输出', schema: node.output_schema, disabled: props.disabled, onChange: (schema: WorkflowSchema) => mutate(copy => { copy.nodes.find(item => item.id === node.id)!.output_schema = schema }) }),
      ])
    }
    return () => document.value ? h('div', { class: 'workflow-editor' }, [
      h('div', { class: 'workflow-basics' }, [
        h('label', ['工作流标识', h('input', { id: 'workflow-name', value: document.value.name, disabled: props.disabled, placeholder: '如 bid-opportunity-review', onInput: (event: Event) => mutate(copy => { copy.name = valueOf(event) }) })]),
        h('label', ['要完成什么', h('input', { id: 'workflow-goal', value: document.value.contract.goal, disabled: props.disabled, placeholder: '明确交付物及使用场景', onInput: (event: Event) => mutate(copy => { copy.contract.goal = valueOf(event) }) })]),
      ]),
      h('details', { class: 'workflow-settings' }, [h('summary', '任务输入、硬约束与预算'),
        h(WorkflowFields, { title: '任务输入', schema: document.value.contract.input_schema, disabled: props.disabled, onChange: (schema: WorkflowSchema) => mutate(copy => { copy.contract.input_schema = schema }) }),
        h('label', ['硬约束（每行一条）', h('textarea', { id: 'workflow-constraints', rows: 3, value: document.value.contract.constraints.join('\n'), disabled: props.disabled, onInput: (event: Event) => mutate(copy => { copy.contract.constraints = valueOf(event).split('\n').filter(line => line.trim()) }) })]),
        h('div', { class: 'workflow-budget' }, ([['max_total_tokens', '总 token 上限', 200000], ['max_tool_calls', '工具调用上限', 200], ['max_duration_seconds', '总时长上限（秒）', 1800]] as const).map(([key, title, max]) => h('label', [title, h('input', { type: 'number', min: 1, max, value: document.value!.contract.budget[key], disabled: props.disabled, onInput: (event: Event) => mutate(copy => { const n = Number(valueOf(event)); copy.contract.budget[key] = n; copy.limits[key] = n }) })]))),
      ]),
      h('div', { class: 'workflow-add' }, [h('label', ['添加已配置 Agent', h('select', { id: 'workflow-add-agent', 'aria-label': '添加已配置 Agent', value: addAgent.value, disabled: props.disabled, onChange: (event: Event) => { addAgent.value = valueOf(event) } }, [h('option', { value: '' }, '选择 Agent'), ...props.catalog.map(item => h('option', { value: item.name, disabled: !item.eligible || !item.agent }, `${item.display_name} · ${item.name}${item.eligible ? '' : '（' + unavailable[item.reason] + '）'}`))])]), h('button', { type: 'button', disabled: props.disabled || !props.catalog.some(item => item.name === addAgent.value && item.eligible && item.agent), onClick: add }, '添加节点')]),
      h('div', { class: 'workflow-layout' }, [graph(), active.value ? nodePanel() : h('div', { class: 'workflow-node-panel workflow-empty' }, [h('h3', '各司其职，结果逐步交接'), h('p', '选择 Agent 后，在这里配置输入连接和输出字段。Agent 的指令、知识库与工具在 Agent 管理中维护。')])]),
      h('details', { class: 'workflow-settings' }, [h('summary', '最终产物与验收规则'), h('p', '最后一个节点生成最终产物。格式验收只检查结构，业务事实仍需独立核对。导入的验收规则完整保留。'),
        h('p', `最终字段：${workflowFields(document.value.contract.output_schema)?.map(field => `${field.name}（${field.type}）`).join('、') || '使用高级格式约束'}`),
        canAdoptWorkflowOutput(document.value) ? [h('p', '以下操作会明确更新最终产物格式及 report-shape 结构验收规则，其他约束保留。'), h('button', { type: 'button', disabled: props.disabled || !document.value.nodes.length, onClick: () => { if (!props.disabled) commit(adoptWorkflowOutput(document.value!)) } }, '采用末节点格式并更新结构验收')] : h('p', '此定义包含高级验收规则，请在高级原文中修改。'),
        h('details', [h('summary', '查看全部验收规则'), h(CodeBlock, { text: JSON.stringify(document.value.contract.acceptance, null, 2) })]),
      ]),
      issues.value.length ? h('div', { class: 'workflow-issues', role: 'status' }, [h('strong', '发布前需要补齐'), h('ul', issues.value.map(issue => h('li', issue)))]) : h('p', { class: 'muted' }, '本地结构检查通过。发布前仍须在线核对 Agent 与资源版本。'),
      error.value ? h(Notice, { tone: 'error', text: error.value }) : null,
    ]) : h(Notice, { tone: 'warn', text: reason.value + ' 请展开“高级定义原文”；这里不会覆盖或转换该内容。' })
  },
})
