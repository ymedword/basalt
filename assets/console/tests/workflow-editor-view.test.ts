import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, describe, expect, it } from 'vitest'
import { WorkflowEditor } from '../src/views/workflow-editor'
import { addWorkflowNode, newWorkflow } from '../src/workflow-editor'
import type { FlowAgentCandidate } from '../src/api-flow'

const agent: FlowAgentCandidate = { name: 'researcher', display_name: '需求研究员', description: '逐项整理来源与要求', eligible: true, reason: 'ready', agent: { uid: 'agent-1', name: 'researcher', version: 1, config_hash: 'config', manifest_hash: 'manifest' }, knowledge_collections: ['rfp'], memory_scope: 'agent_only', readonly_tools: ['read_file'] }
let wrapper: ReturnType<typeof mount> | undefined
afterEach(() => { wrapper?.unmount(); wrapper = undefined })
const button = (text: string) => wrapper!.findAll('button').find(item => item.text() === text)!

describe('visual workflow editor', () => {
  it('authors a sequential definition from enabled Agents without writing JSON', async () => {
    wrapper = mount(WorkflowEditor, { props: { raw: '', catalog: [agent, { ...agent, name: 'disabled', eligible: false, reason: 'agent_disabled', agent: undefined }] } })
    expect(wrapper.emitted('change')).toBeUndefined()
    await wrapper.find('#workflow-name').setValue('bid-review')
    await wrapper.find('#workflow-goal').setValue('Prepare a sourced response draft')
    expect(wrapper.find('option[value="disabled"]').attributes('disabled')).toBeDefined()
    await wrapper.find('#workflow-add-agent').setValue('researcher')
    await button('添加节点').trigger('click'); await flushPromises()
    const firstID = JSON.parse(wrapper.emitted('change')!.at(-1)![0] as string).nodes[0].id as string
    expect(wrapper.findAll('.workflow-node')).toHaveLength(1)
    expect(wrapper.text()).toContain('知识库：rfp')
    expect(wrapper.text()).toContain('仅 Agent 核心记忆')
    await button('添加节点').trigger('click')
    const secondID = JSON.parse(wrapper.emitted('change')!.at(-1)![0] as string).nodes[1].id as string
    await wrapper.find('[aria-label="传入字段名"]').setValue('facts')
    await wrapper.find('[aria-label="来源字段"]').setValue(`outputs.${firstID}.result`)
    await button('连接输入').trigger('click')
    const raw = wrapper.emitted('change')!.at(-1)![0] as string
    const definition = JSON.parse(raw)
    expect(definition.name).toBe('bid-review')
    expect(definition.nodes[0].agent).toEqual(agent.agent)
    expect(definition.nodes[0].next).toBe(secondID)
    expect(definition.nodes[1].inputs).toContainEqual({ target: 'facts', source: `outputs.${firstID}.result` })
    expect(definition.contract.acceptance[0].kind).toBe('deterministic')
    expect(wrapper.findAll('svg')).toHaveLength(3)
    await wrapper.find(`[aria-label="上移 ${secondID}"]`).trigger('click')
    expect(wrapper.text()).toContain('不在上游')
    expect(JSON.parse(wrapper.emitted('change')!.at(-1)![0] as string).nodes[0].inputs).toContainEqual({ target: 'facts', source: `outputs.${firstID}.result` })
  })
  it('does not reserialize imported raw bytes or discard advanced precise numbers', async () => {
    const document = addWorkflowNode(newWorkflow(), agent.agent!)
    const raw = JSON.stringify(document).replace('50000', '9007199254740993')
    wrapper = mount(WorkflowEditor, { props: { raw, catalog: [agent] } })
    expect(wrapper.text()).toContain('精确大数')
    expect(wrapper.emitted('change')).toBeUndefined()
    await wrapper.setProps({ raw: JSON.stringify(document, null, 4) })
    expect(wrapper.findAll('.workflow-node')).toHaveLength(1)
    expect(wrapper.emitted('change')).toBeUndefined()
  })
  it('locks edits during pending writes and supports keyboard order changes otherwise', async () => {
    const document = addWorkflowNode(addWorkflowNode(newWorkflow(), agent.agent!), agent.agent!)
    const secondID = document.nodes[1]!.id
    wrapper = mount(WorkflowEditor, { props: { raw: JSON.stringify(document), catalog: [agent], disabled: true } })
    await wrapper.findAll('.workflow-node-select')[1]!.trigger('keydown', { key: 'ArrowUp', altKey: true })
    expect(wrapper.emitted('change')).toBeUndefined()
    expect(wrapper.find(`[aria-label="上移 ${secondID}"]`).attributes('disabled')).toBeDefined()
    await wrapper.setProps({ disabled: false })
    await wrapper.findAll('.workflow-node-select')[1]!.trigger('keydown', { key: 'ArrowUp', altKey: true })
    expect(JSON.parse(wrapper.emitted('change')!.at(-1)![0] as string).entry).toBe(secondID)
  })
})
