import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { markRaw } from 'vue'
import { ApiClient } from '../src/api'
import { ConsoleAuth } from '../src/auth'
import { ConsoleRouter } from '../src/router'
import { FlowTaskView } from '../src/views/flow-task'
import { createConsoleApp } from '../src/app'
import { FlowApi } from '../src/api-flow'
import { FlowsView, FlowTasksView } from '../src/views/flows'
import type { FlowTask } from '../src/api-flow'
import { addWorkflowNode, newWorkflow, workflowSchema } from '../src/workflow-editor'

let wrapper: ReturnType<typeof mount> | undefined
const button = (name: string) => wrapper!.findAll('button').find(item => item.text() === name)!
const now = Date.now()
const task = (extra: Partial<FlowTask> = {}): FlowTask => ({task_id: 'ft_real', root_task_id: 'ft_real', definition_uid: 'fd_real', definition_version: 1, status: 'blocked', version: {revision: 0, state_version: 3, execution_epoch: 1}, etag: '"task-full-version"', goal: '核对固定资料', constraints: ['保留所有硬约束'], current_node: 'research', close_intent: 'none', cancel_requested: false, unknown_actions: false, deadline: now + 3600000, created_at: now, updated_at: now, remaining_tokens: 92, remaining_tool_calls: 8, budget_exhausted: false, slots: [{id: 'facts', state: 'missing', evidence_ids: [], verification_ids: []}], claims: [], attempts: [], ...extra})
const definition = {definition_uid: 'fd_real', version: 1, name: 'report', content_hash: 'hash', retired: false, goal: '原目标', input_schema: {type: 'object'}, output_schema: {type: 'object'}, nodes: [{id: 'research', kind: 'agent'}]}
function response(data: unknown, status = 200, code = 'OK'): Response { return {ok: status < 400, status, headers: new Headers(), json: async () => ({code, message: status >= 400 ? '读取或写入失败' : '成功', data})} as Response }
function context() { const auth = new ConsoleAuth(); auth.setToken('synthetic-admin'); return markRaw({api: new ApiClient(auth), auth, router: new ConsoleRouter()}) }
type Call = {method: string; path: string; body: Record<string, unknown> | undefined; headers: Record<string, string>}
let calls: Call[] = []
function network(route: (call: Call) => Response | Promise<Response> | undefined) {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const call = {method: init?.method ?? 'GET', path: new URL(String(input), 'http://localhost').pathname, body: typeof init?.body === 'string' ? JSON.parse(init.body) as Record<string, unknown> : undefined, headers: init?.headers as Record<string,string>}
    calls.push(call)
    return await route(call) ?? response({items: []})
  }))
}
afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); document.body.innerHTML = '' })

describe('US19 durable Flow views', () => {
  it('shows the pinned workflow name and version in task details', async () => {
    network(call => call.path.endsWith('/ft_real') ? response(task({definition_name: 'bid-readiness-matrix-v1', definition_version: 2})) : undefined)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}}); await flushPromises()
    expect(wrapper.find('.flow-workflow').text()).toBe('工作流：bid-readiness-matrix-v1 · v2')
    expect(wrapper.text()).toContain('核对固定资料')
  })
  it('filters tasks by workflow before paging and refreshes from the newest page', async () => {
    network(() => undefined)
    const read = vi.spyOn(FlowApi.prototype, 'tasks').mockResolvedValue({items: [task({definition_name: 'bid-readiness-matrix-v1'})], next_cursor: 'older-page'})
    wrapper = mount(FlowTasksView, {props: {ctx: context()}}); await flushPromises()
    expect(wrapper.text()).toContain('工作流编号')
    expect(wrapper.text()).toContain('bid-readiness-matrix-v1 · v1')
    await button('下一页任务').trigger('click'); await flushPromises()
    expect(read).toHaveBeenLastCalledWith(false, 'older-page', '')
    await wrapper.find('#flow-task-filter').setValue('bid-readiness-matrix-v1')
    await wrapper.find('#flow-task-filters').trigger('submit'); await flushPromises()
    expect(read).toHaveBeenLastCalledWith(false, '', 'bid-readiness-matrix-v1')
    await button('下一页任务').trigger('click'); await flushPromises()
    expect(read).toHaveBeenLastCalledWith(false, 'older-page', 'bid-readiness-matrix-v1')
    await button('刷新任务').trigger('click'); await flushPromises()
    expect(read).toHaveBeenLastCalledWith(false, '', 'bid-readiness-matrix-v1')
    await button('清除筛选').trigger('click'); await flushPromises()
    expect(read).toHaveBeenLastCalledWith(false, '', '')
  })
  it('loads the bundled template through the current paged catalog and preserves an existing draft', async () => {
    const names = ['bid-requirements', 'bid-technical', 'bid-delivery', 'bid-reviewer']
    const candidates = names.map(name => ({name, display_name: name, description: 'current role', eligible: true, reason: 'ready' as const, knowledge_collections: [], memory_scope: 'agent_only' as const, readonly_tools: ['read_file'], agent: {name, uid: 'current-' + name, version: 3, config_hash: 'a'.repeat(64), manifest_hash: 'b'.repeat(64)}}))
    network(() => undefined)
    const read = vi.spyOn(FlowApi.prototype, 'authoringAgents').mockImplementation(async cursor => cursor === 'next' ? {items: candidates.slice(2)} : {items: candidates.slice(0, 2), next_cursor: 'next'})
    wrapper = mount(FlowsView, {props: {ctx: context()}}); await flushPromises()
    await wrapper.find('#workflow-name').setValue('my-draft')
    await button('载入采购准备模板').trigger('click'); await flushPromises()
    await vi.waitFor(() => {
      expect(read).toHaveBeenCalledWith('next')
      expect(wrapper!.text()).toContain('替换当前草稿')
    })
    expect((wrapper.find('#flow-bundle').element as HTMLTextAreaElement).value).toContain('my-draft')
    await button('替换草稿').trigger('click'); await flushPromises()
    const draft = JSON.parse((wrapper.find('#flow-bundle').element as HTMLTextAreaElement).value)
    expect(draft.nodes).toHaveLength(17)
    expect(draft.nodes[0].agent.uid).toBe('current-bid-requirements')
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
    expect(button('核对发布').attributes('disabled')).toBeDefined()
  })
  it('names the missing default roles without publishing or running a task', async () => {
    network(() => undefined)
    wrapper = mount(FlowsView, {props: {ctx: context()}}); await flushPromises()
    await button('载入采购准备模板').trigger('click'); await flushPromises()
    await vi.waitFor(() => expect(wrapper!.text()).toContain('请先在 Agents 中配置模型并启用：bid-requirements、bid-technical、bid-delivery、bid-reviewer'))
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
  })
  it('clears authoring data and drafts when the catalog rejects the admin identity', async () => {
    let denied = false
    network(call => call.path === '/api/v1/admin/flows' ? response({items: [definition]}) : call.path === '/api/v1/admin/flow-agents' && denied ? response(null, 403, 'FORBIDDEN') : undefined)
    wrapper = mount(FlowsView, {props: {ctx: context()}}); await flushPromises()
    await wrapper.find('#workflow-name').setValue('private-draft')
    expect(wrapper.text()).toContain('report')
    denied = true
    await button('刷新 Agent 目录').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('管理身份已失效')
    expect(wrapper.text()).not.toContain('report')
    expect(wrapper.find('#flow-bundle').exists()).toBe(false)
    expect(wrapper.find('#workflow-name').exists()).toBe(false)
  })
  it('keeps an unknown 409 start frozen and reconciles using the same identity', async () => {
    vi.stubGlobal('crypto', {getRandomValues: crypto.getRandomValues.bind(crypto)})
    let count = 0
    const ctx = context(); vi.spyOn(ctx.router, 'navigate').mockReturnValue(true)
    network(call => call.path === '/api/v1/flows' ? response({items: [definition]}) : call.method === 'POST' ? (++count === 1 ? response(null, 409, 'FLOW_STATE_CONFLICT') : response(task(), 200)) : undefined)
    wrapper = mount(FlowTasksView, {props: {ctx}}); await flushPromises()
    await wrapper.find('#flow-definition').setValue('fd_real')
    await wrapper.find('#flow-input').setValue('{"answer":"frozen"}')
    await wrapper.find('#flow-start').trigger('submit'); await flushPromises()
    expect(wrapper.find('#flow-input').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('查证本次受理')
    await wrapper.find('#flow-start').trigger('submit'); await flushPromises()
    const writes = calls.filter(call => call.method === 'POST')
    expect(writes).toHaveLength(2)
    expect(writes[1]!.headers['Idempotency-Key']).toBe(writes[0]!.headers['Idempotency-Key'])
    expect(writes[1]!.body).toEqual(writes[0]!.body)
  })

  it.each(['cancel', 'resume', 'abandon'] as const)('keeps unknown 409 %s controls locked with the original operation', async action => {
    const admin = action === 'abandon'
    network(call => call.path.endsWith('/' + action) ? response(null, 409, 'FLOW_STATE_CONFLICT') : call.path.endsWith('/ft_real') ? response(task()) : undefined)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}, query: new URLSearchParams(admin ? 'admin=1' : '')}}); await flushPromises()
    await wrapper.find('#flow-control-reason').setValue('reviewed reason')
    const names = {cancel: ['取消并关闭', '确认取消并关闭'], resume: ['核对并恢复', '确认恢复'], abandon: ['放弃任务', '确认放弃']} as const
    await button(names[action][0]).trigger('click'); await flushPromises()
    await button(names[action][1]).trigger('click'); await flushPromises()
    const writes = calls.filter(call => call.path.endsWith('/' + action))
    expect(writes).toHaveLength(1)
    expect(wrapper.text()).toContain(String(writes[0]!.body!.operation_id))
    expect(button(names[action][0]).attributes('disabled')).toBeDefined()
    await button(names[action][0]).trigger('click'); await flushPromises()
    expect(calls.filter(call => call.path.endsWith('/' + action))).toHaveLength(1)
  })
  it.each(['publish', 'retire'])('retains the original %s operation after a 409 unknown commit', async action => {
    network(call => call.path === '/api/v1/admin/flows' ? response({items: [definition]}) : call.path.endsWith('/validate') ? response({valid: true}) : call.path.endsWith('/' + action) ? response(null, 409, 'FLOW_STATE_CONFLICT') : undefined)
    wrapper = mount(FlowsView, {props: {ctx: context()}}); await flushPromises()
    if (action === 'publish') {
      await wrapper.find('#flow-bundle').setValue(JSON.stringify({name: 'report', contract: {goal: 'new'}, nodes: []}))
      await button('只读在线校验').trigger('click'); await flushPromises()
      await button('核对发布').trigger('click'); await flushPromises()
      await button('确认发布').trigger('click')
    } else { await button('退役').trigger('click'); await flushPromises(); await button('确认退役').trigger('click') }
    await flushPromises()
    const writes = calls.filter(call => call.path.endsWith('/' + action))
    expect(writes).toHaveLength(1)
    expect(wrapper.text()).toContain(String(writes[0]!.body!.operation_id))
    expect(wrapper.text()).toContain('上次变更结果尚未核对')
    expect(wrapper.find('#flow-bundle').attributes('disabled')).toBeDefined()
    expect(button('退役').attributes('disabled')).toBeDefined()
    await button('核对发布').trigger('click'); await button('退役').trigger('click'); await flushPromises()
    expect(calls.filter(call => call.path.endsWith('/' + action))).toHaveLength(1)
  })
  it('loads an exact published author version, confirms draft replacement and preserves precise raw bytes', async () => {
    const raw = '{"name":"report","contract":{"goal":"author version","exact":9007199254740993},"nodes":[]}'
    network(call => call.path === '/api/v1/admin/flows' ? response({items: [definition]}) : call.path.endsWith('/versions/1') ? response({definition_uid: 'fd_real', version: 1, bundle_json: raw}) : undefined)
    wrapper = mount(FlowsView, {props: {ctx: context()}})
    await flushPromises()
    await wrapper.find('#workflow-name').setValue('my-draft')
    const draft = (wrapper.find('#flow-bundle').element as HTMLTextAreaElement).value
    await button('编辑副本').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('替换当前草稿')
    expect((wrapper.find('#flow-bundle').element as HTMLTextAreaElement).value).toBe(draft)
    await button('返回修改').trigger('click'); await flushPromises()
    expect((wrapper.find('#flow-bundle').element as HTMLTextAreaElement).value).toBe(draft)
    await button('编辑副本').trigger('click'); await flushPromises()
    await button('替换草稿').trigger('click'); await flushPromises()
    expect((wrapper.find('#flow-bundle').element as HTMLTextAreaElement).value).toBe(raw)
    expect(wrapper.text()).toContain('精确大数')
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
  })

  it('downloads the unchanged draft without storing or publishing it', async () => {
    const raw = '{ "name": "exact", "number": 9007199254740993 }'
    network(() => undefined)
    const blobs: Blob[] = []
    vi.stubGlobal('URL', class extends URL { static createObjectURL(blob: Blob) { blobs.push(blob); return 'blob:workflow-draft' }; static revokeObjectURL() {} })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const persist = vi.spyOn(Storage.prototype, 'setItem')
    wrapper = mount(FlowsView, {props: {ctx: context()}})
    await flushPromises(); persist.mockClear()
    await wrapper.find('#flow-bundle').setValue(raw)
    await button('下载草稿').trigger('click')
    expect(blobs).toHaveLength(1)
    const text = await new Promise<string>(resolve => { const reader = new FileReader(); reader.onload = () => resolve(String(reader.result)); reader.readAsText(blobs[0]!) })
    expect(text).toBe(raw)
    expect(click).toHaveBeenCalledOnce()
    expect(persist).not.toHaveBeenCalled()
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
  })

  it('starts a basic workflow using form inputs and keeps its submitted request frozen', async () => {
    const ctx = context(); vi.spyOn(ctx.router, 'navigate').mockReturnValue(true)
    const schema = workflowSchema([{name: 'request', type: 'string', required: true, description: '业务需求'}, {name: 'count', type: 'integer', required: true, description: ''}])
    network(call => call.path === '/api/v1/flows' ? response({items: [{...definition, input_schema: schema}]}) : call.method === 'POST' ? response(task(), 202) : undefined)
    wrapper = mount(FlowTasksView, {props: {ctx}}); await flushPromises()
    await wrapper.find('#flow-definition').setValue('fd_real')
    await wrapper.find('[aria-label="request"]').setValue('Review the public RFP')
    await wrapper.find('[aria-label="count"]').setValue('3')
    await wrapper.find('#flow-start').trigger('submit'); await flushPromises()
    expect(calls.find(call => call.method === 'POST')?.body?.input).toEqual({request: 'Review the public RFP', count: 3})
  })

  it('invalidates online approval after a visual edit and retains the original pin on catalog refresh', async () => {
    const pin = {uid: 'agent', name: 'researcher', version: 1, config_hash: 'config1', manifest_hash: 'manifest1'}
    const doc = addWorkflowNode(newWorkflow(), pin); doc.name = 'report'; doc.contract.goal = 'old goal'
    network(call => call.path.endsWith('/validate') ? response({valid: true}) : call.path.endsWith('/flow-agents') ? response({items: [{name: pin.name, display_name: '研究员', description: '', eligible: true, reason: 'ready', agent: {...pin, version: 2, config_hash: 'config2'}, knowledge_collections: [], readonly_tools: [], memory_scope: 'agent_only'}]}) : undefined)
    wrapper = mount(FlowsView, {props: {ctx: context()}}); await flushPromises()
    await wrapper.find('#flow-bundle').setValue(JSON.stringify(doc))
    await button('只读在线校验').trigger('click'); await flushPromises()
    expect(button('核对发布').attributes('disabled')).toBeUndefined()
    await wrapper.find('#workflow-goal').setValue('changed goal')
    expect(button('核对发布').attributes('disabled')).toBeDefined()
    await button('刷新 Agent 目录').trigger('click'); await flushPromises()
    expect(JSON.parse((wrapper.find('#flow-bundle').element as HTMLTextAreaElement).value).nodes[0].agent).toEqual(pin)
  })

  it('shows normal in-flight accounting without demanding manual reconciliation', async () => {
    network(call => call.path === '/api/v1/flow-tasks/ft_real' ? response(task({status: 'running', unknown_actions: true})) : undefined)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    expect(wrapper.text()).toContain('调用结果或用量尚待确认')
    expect(wrapper.text()).not.toContain('请人工对账')
    expect(wrapper.text()).toContain('节点跨度')
    expect(wrapper.text()).toContain('包含执行和暂停核对时间')
  })

  it('distinguishes ended routes and recorded artifacts from task acceptance', async () => {
    network(call => {
      if (call.path === '/api/v1/flow-tasks') return response({items: [task({status: 'succeeded', current_node: ''})]})
      if (call.path === '/api/v1/flows') return response({items: [definition]})
      if (call.path === '/api/v1/flow-tasks/ft_real') return response(task())
      if (call.path.endsWith('/artifacts')) return response({items: [{artifact_id: 'report', version: 1, task_revision: 0, state: 'accepted', format: 'json', content_hash: 'hash', readable: false, reason: 'artifact_basis_unconfirmed'}]})
      if (call.path.endsWith('/verifications')) return response({items: [{verification_id: 'v', target_kind: 'slot', target_id: 'facts', target_version: 1, task_revision: 0, rule_id: 'facts-check', rule_version: 1, validator_kind: 'evidence', outcome: 'fail', reason: 'independent_sources_missing', basis_hashes: ['contract', 'target'], created_at: now}]})
    })
    wrapper = mount(FlowTasksView, {props: {ctx: context()}})
    await flushPromises()
    expect(wrapper.text()).toContain('路线已结束')
    expect(wrapper.text()).not.toContain('尚未开始')
    wrapper.unmount()
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    expect(wrapper.text()).toContain('已记录')
    expect(wrapper.text()).toContain('节点结果的核验依据尚未确认')
    expect(wrapper.text()).toContain('独立来源数量不足')
    expect(wrapper.text()).not.toContain('当前来源权限或版本无法核对')
    expect(wrapper.text()).not.toContain('2 个版本化依据')
  })

  it('resumes a real none intent without a false closing warning', async () => {
    let current = task({close_intent: 'none'})
    network(call => {
      if (call.path === '/api/v1/flow-tasks/ft_real') return response(current)
      if (call.path.endsWith('/resume')) { current = task({status: 'queued', close_intent: 'none'}); return response(current) }
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    expect(wrapper.text()).not.toContain('正在停止：关闭意图已保存')
    const resume = wrapper.findAll('button').find(button => button.text() === '核对并恢复')
    expect(resume).toBeDefined()
    await wrapper.find('textarea').setValue('原来源已核对')
    await resume!.trigger('click'); await flushPromises()
    expect(calls.some(call => call.path.endsWith('/resume'))).toBe(false)
    await button('确认恢复').trigger('click'); await flushPromises()
    expect(calls.filter(call => call.path.endsWith('/resume'))).toHaveLength(1)
    expect(wrapper.text()).toContain('恢复核对已完成')
    expect(wrapper.text()).not.toContain('关闭意图已保存，正在等待')
  })

  it.each(['cancel', 'abandon', 'deadline'] as const)('keeps %s intent non-resumable', async close_intent => {
    network(call => call.path === '/api/v1/flow-tasks/ft_real' ? response(task({close_intent, cancel_requested: close_intent !== 'deadline'})) : undefined)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    expect(wrapper.text()).toContain('正在停止：关闭意图已保存')
    expect(wrapper.findAll('button').some(button => button.text() === '核对并恢复')).toBe(false)
  })

  it('retains duplicate keys and exact large integers until strict server parsing', async () => {
    const raw: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => { raw.push(String(init?.body)); return response(task()) }))
    const api = new FlowApi(context().api)
    const input = '{"exact":9007199254740993,"same":1,"same":2}'
    await api.start('fd_real', input, 'stable-key')
    await api.validate('{"format_version":1,"name":"x","name":"y"}')
    expect(raw[0]).toContain(input)
    expect(raw[1]).toContain('"name":"x","name":"y"')
  })

  it('registers both navigation entrances and renders real app routes without proxying private clients', async () => {
    network(call => call.path === '/api/v1/flows' ? response({items: [definition]}) : call.path === '/api/v1/flow-tasks/ft_real' ? response(task({status: 'succeeded'})) : undefined)
    window.history.replaceState(null, '', '/console/login?next=/console/flow-tasks/ft_real')
    wrapper = mount(createConsoleApp(), {attachTo: document.body})
    await flushPromises()
    await wrapper.find('#console-token').setValue('synthetic-admin')
    await wrapper.find('.login-form').trigger('submit'); await flushPromises()
    expect(wrapper.text()).toContain('已通过验收')
    expect(wrapper.find('a[href="/console/flows"]').exists()).toBe(true)
    expect(wrapper.find('a[href="/console/flow-tasks"]').exists()).toBe(true)
    expect(calls.some(call => call.path === '/api/v1/flow-tasks/ft_real')).toBe(true)
  })

  it('does not continue source reads when a late task response arrives after leaving', async () => {
    let resolve: ((value: Response) => void) | undefined
    network(call => call.path === '/api/v1/flow-tasks/ft_real' ? new Promise<Response>(done => { resolve = done }) : undefined)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    expect(calls).toHaveLength(1)
    wrapper.unmount(); wrapper = undefined
    resolve!(response(task()))
    await flushPromises()
    expect(calls).toHaveLength(1)
  })
  it('blocks replay for unknown, retains constraints and permits confirmed cancellation with full ETag', async () => {
    let current = task({unknown_actions: true})
    network(call => {
      if (call.path === '/api/v1/flow-tasks/ft_real') return response(current)
      if (call.path.endsWith('/cancel')) { current = task({status: 'cancelled', cancel_requested: true, close_intent: 'cancel', unknown_actions: true}); return response(current) }
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    expect(wrapper.text()).toContain('保留所有硬约束')
    expect(wrapper.text()).toContain('不会自动重放')
    expect(wrapper.text()).not.toContain('核对并恢复')
    expect(wrapper.text()).toContain('缺少证据')
    await wrapper.find('#flow-control-reason').setValue('人工停止并对账')
    const cancel = wrapper.findAll('button').find(button => button.text() === '取消并关闭')!
    await cancel.trigger('click'); await cancel.trigger('click'); await flushPromises()
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
    expect(wrapper.text()).toContain('已发送的副作用不会被撤回')
    const confirm = button('确认取消并关闭')
    await confirm.trigger('click'); await confirm.trigger('click'); await flushPromises()
    const writes = calls.filter(call => call.method === 'POST')
    expect(writes).toHaveLength(1)
    expect(writes[0]!.headers['If-Match']).toBe('"task-full-version"')
    expect(writes[0]!.body?.operation_id).toBeTruthy()
    expect(wrapper.text()).toContain('已取消')
    expect(wrapper.text()).toContain('人工对账')
    expect(wrapper.text()).not.toContain('核对并恢复')
  })

  it('keeps one original request after uncertain start and locks semantic inputs', async () => {
    let attempts = 0
    const ctx = context()
    const navigate = vi.spyOn(ctx.router, 'navigate').mockReturnValue(true)
    network(call => {
      if (call.path === '/api/v1/flows') return response({items: [definition]})
      if (call.path === '/api/v1/flow-tasks' && call.method === 'POST') { attempts++; if (attempts === 1) throw new Error('network interrupted'); return response(task({status: 'queued'}), 202) }
    })
    wrapper = mount(FlowTasksView, {props: {ctx}})
    await flushPromises()
    await wrapper.find('#flow-definition').setValue('fd_real')
    await wrapper.find('#flow-input').setValue('{"answer":"fixed"}')
    await wrapper.find('#flow-start').trigger('submit'); await flushPromises()
    expect(wrapper.text()).toContain('查证本次受理')
    expect((wrapper.find('#flow-input').element as HTMLTextAreaElement).disabled).toBe(true)
    await wrapper.find('#flow-start').trigger('submit'); await flushPromises()
    const writes = calls.filter(call => call.method === 'POST')
    expect(writes).toHaveLength(2)
    expect(writes[0]!.headers['Idempotency-Key']).toBe(writes[1]!.headers['Idempotency-Key'])
    expect(writes[0]!.body).toEqual(writes[1]!.body)
    expect(navigate).toHaveBeenCalledWith('/console/flow-tasks/ft_real')
  })

  it('shows admin metadata with no artifact queries and no borrowed resume capability', async () => {
    network(call => call.path === '/api/v1/admin/flow-tasks/ft_real' ? response(task({goal: '', constraints: [], remaining_tokens: undefined, remaining_tool_calls: undefined, unknown_actions: true})) : undefined)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}, query: new URLSearchParams('admin=1')}})
    await flushPromises()
    expect(wrapper.text()).toContain('运维视图仅显示执行元数据')
    expect(wrapper.text()).toContain('未提供')
    expect(wrapper.text()).toContain('放弃任务')
    expect(wrapper.text()).not.toContain('核对并恢复')
    expect(calls).toHaveLength(1)
    expect(calls[0]!.method).toBe('GET')
  })

  it('preserves last data on polling errors and stops terminal polling and unmount work', async () => {
    vi.useFakeTimers()
    let current = task({status: 'running'})
    let fail = false
    network(call => call.path === '/api/v1/flow-tasks/ft_real' ? fail ? response(null, 500, 'INTERNAL') : response(current) : undefined)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    fail = true
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect(wrapper.text()).toContain('保留最后一次成功读取的数据')
    expect(wrapper.text()).toContain('核对固定资料')
    fail = false; current = task({status: 'succeeded'})
    await vi.advanceTimersByTimeAsync(4000); await flushPromises()
    expect(wrapper.text()).toContain('已通过验收')
    const count = calls.length
    await vi.advanceTimersByTimeAsync(30000); await flushPromises()
    expect(calls).toHaveLength(count)
    wrapper.unmount(); wrapper = undefined
    await vi.advanceTimersByTimeAsync(30000)
    expect(calls).toHaveLength(count)
  })

  it('clears private artifacts after an explicit permission loss instead of keeping a stale body', async () => {
    vi.useFakeTimers()
    let denied = false
    network(call => {
      if (call.path === '/api/v1/flow-tasks/ft_real') return denied ? response(null, 403, 'FORBIDDEN') : response(task({status: 'running'}))
      if (call.path.endsWith('/artifacts')) return response({items: [{artifact_id: 'report', version: 1, task_revision: 0, state: 'accepted', format: 'text', content_hash: 'hash', readable: true, content: 'PRIVATE-CONTENT-SENTINEL'}]})
    })
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    expect(wrapper.text()).toContain('PRIVATE-CONTENT-SENTINEL')
    denied = true
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect(wrapper.text()).not.toContain('PRIVATE-CONTENT-SENTINEL')
    expect(wrapper.text()).toContain('当前身份无权')
    const count = calls.length
    await vi.advanceTimersByTimeAsync(30000)
    expect(calls).toHaveLength(count)
  })

  it('requires read-only validation, confirms publication and sends the exact expected version', async () => {
    network(call => call.path === '/api/v1/admin/flows' ? response({items: [definition]}) : call.path.endsWith('/validate') ? response({valid: true}) : call.path.endsWith('/publish') ? response({...definition, version: 2}) : undefined)
    const nativeConfirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    wrapper = mount(FlowsView, {props: {ctx: context()}})
    await flushPromises()
    await wrapper.find('#flow-bundle').setValue(JSON.stringify({name: 'report', contract: {goal: '新目标'}, nodes: [{id: 'research'}]}))
    const publish = () => button('核对发布')
    expect((publish().element as HTMLButtonElement).disabled).toBe(true)
    await wrapper.findAll('button').find(button => button.text() === '只读在线校验')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('v1 → 新版本')
    await publish().trigger('click'); await flushPromises()
    expect(calls.some(call => call.path.endsWith('/publish'))).toBe(false)
    expect(wrapper.text()).toContain('只影响新任务；在途任务继续固定旧版本')
    await button('返回修改').trigger('click'); await flushPromises()
    expect(calls.some(call => call.path.endsWith('/publish'))).toBe(false)
    await publish().trigger('click'); await flushPromises()
    const confirm = button('确认发布')
    await confirm.trigger('click'); await confirm.trigger('click'); await flushPromises()
    expect(calls.filter(call => call.path.endsWith('/publish'))).toHaveLength(1)
    expect(nativeConfirm).not.toHaveBeenCalled()
    const write = calls.find(call => call.path.endsWith('/publish'))!
    expect(write.body?.expected_version).toBe(1)
    expect(write.body?.definition_uid).toBe('fd_real')
    expect(write.body?.operation_id).toBeTruthy()
    expect(wrapper.text()).toContain('发布已提交')
  })

  it('allows backing out of retirement and preserves the reviewed definition version', async () => {
    const nativeConfirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    network(call => call.path === '/api/v1/admin/flows' ? response({items: [definition]}) : call.path.endsWith('/retire') ? response({...definition, retired: true}) : undefined)
    wrapper = mount(FlowsView, {props: {ctx: context()}})
    await flushPromises()
    await button('退役').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('历史版本和在途任务会保留')
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
    await button('返回修改').trigger('click'); await flushPromises()
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
    await button('退役').trigger('click'); await flushPromises()
    await button('确认退役').trigger('click'); await flushPromises()
    const writes = calls.filter(call => call.method === 'POST')
    expect(writes).toHaveLength(1)
    expect(writes[0]!.body?.expected_version).toBe(1)
    expect(nativeConfirm).not.toHaveBeenCalled()
  })

  it('requires a fresh confirmation if polling changes the reviewed task version', async () => {
    vi.useFakeTimers()
    let current = task()
    network(call => call.path === '/api/v1/flow-tasks/ft_real' ? response(current) : undefined)
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}}})
    await flushPromises()
    await wrapper.find('#flow-control-reason').setValue('核对后恢复')
    await button('核对并恢复').trigger('click'); await flushPromises()
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
    current = task({etag: '"new-task-full-version"'})
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect((button('确认恢复').element as HTMLButtonElement).disabled).toBe(true)
    await button('确认恢复').trigger('click'); await flushPromises()
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
    expect(wrapper.text()).toContain('任务状态已更新')
    await button('返回修改').trigger('click'); await flushPromises()
    expect(wrapper.find('[aria-label="核对任务操作"]').exists()).toBe(false)
  })

  it('confirms admin abandonment separately without reading private artifacts or borrowing resume', async () => {
    const nativeConfirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    let current = task({goal: '', constraints: [], unknown_actions: true})
    network(call => {
      if (call.path === '/api/v1/admin/flow-tasks/ft_real') return response(current)
      if (call.path.endsWith('/abandon')) { current = task({status: 'abandoned', close_intent: 'abandon', unknown_actions: true}); return response(current) }
    })
    wrapper = mount(FlowTaskView, {props: {ctx: context(), params: {id: 'ft_real'}, query: new URLSearchParams('admin=1')}})
    await flushPromises()
    await wrapper.find('#flow-control-reason').setValue('人工停止并保留未知账本')
    await button('放弃任务').trigger('click'); await flushPromises()
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
    await button('返回修改').trigger('click'); await flushPromises()
    expect(calls.filter(call => call.method === 'POST')).toHaveLength(0)
    await button('放弃任务').trigger('click'); await flushPromises()
    const confirm = button('确认放弃')
    await confirm.trigger('click'); await confirm.trigger('click'); await flushPromises()
    const writes = calls.filter(call => call.method === 'POST')
    expect(writes).toHaveLength(1)
    expect(writes[0]!.path).toBe('/api/v1/admin/flow-tasks/ft_real/abandon')
    expect(writes[0]!.headers['If-Match']).toBe('"task-full-version"')
    expect(writes[0]!.body?.reason).toBe('人工停止并保留未知账本')
    expect(calls.some(call => /artifacts|verifications|resume/.test(call.path))).toBe(false)
    expect(nativeConfirm).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('已放弃')
  })
})
