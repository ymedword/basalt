import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import type { OperationDetail } from '../src/api'
import type { ConsoleContext } from '../src/views/shared'
import { OperationsView } from '../src/views/operations'

const operation: OperationDetail = {
  operation_id: 'op-fixture', action: 'agent.enable', target_kind: 'agent', target_id: 'fixture-agent',
  actor_subject: 'local-admin', auth_method: 'bearer', source: 'web', status: 'success', result_code: 'OK', error_message: '',
  created_at: '2026-11-02T02:00:00Z', finished_at: '2026-11-02T02:00:01Z', request_hash: 'fixture-hash',
  requested_version_id: 1, expected_etag: '', detail: {synthetic: true}, recovered_at: '', verification: 'not_required'
}
let wrapper: VueWrapper | undefined
afterEach(() => { wrapper?.unmount(); wrapper = undefined; document.body.innerHTML = '' })
async function start(getOperation: () => Promise<OperationDetail>): Promise<void> {
  const ctx = {api: {listOperations: vi.fn().mockResolvedValue([operation]), getOperation}} as unknown as ConsoleContext
  wrapper = mount(OperationsView, {props: {ctx}, attachTo: document.body})
  await flushPromises()
}
const viewButton = () => wrapper!.findAll('button').find(button => button.text() === '查看')!

describe('operation detail is visible at the point of action', () => {
  it('opens a focused dialog immediately and shows detail there rather than below the long list', async () => {
    let complete!: (detail: OperationDetail) => void
    await start(() => new Promise(resolve => {complete = resolve}))
    const trigger = viewButton()
    await trigger.trigger('click')
    expect(wrapper!.find('[role="dialog"]').text()).toContain('详情加载中')
    expect(document.activeElement).toBe(wrapper!.find('[role="dialog"]').element)
    complete(operation); await flushPromises()
    expect(wrapper!.find('[role="dialog"]').text()).toContain('fixture-agent')
    expect(wrapper!.find('[role="dialog"] a').attributes('href')).toContain('operation_id=op-fixture')
    await wrapper!.find('[aria-label="关闭操作详情"]').trigger('click')
    expect(wrapper!.find('[role="dialog"]').exists()).toBe(false)
    expect(document.activeElement).toBe(trigger.element)
  })
  it('does not reopen a closed detail when an old request completes', async () => {
    let complete!: (detail: OperationDetail) => void
    await start(() => new Promise(resolve => {complete = resolve}))
    await viewButton().trigger('click')
    await wrapper!.find('[role="dialog"]').trigger('keydown', {key: 'Escape'})
    complete(operation); await flushPromises()
    expect(wrapper!.find('[role="dialog"]').exists()).toBe(false)
  })
  it('shows an error inside the open dialog with an available close action', async () => {
    await start(async () => {throw new Error('读取操作详情失败')})
    await viewButton().trigger('click'); await flushPromises()
    expect(wrapper!.find('[role="dialog"] [role="alert"]').text()).toContain('读取操作详情失败')
    expect(wrapper!.find('[aria-label="关闭操作详情"]').exists()).toBe(true)
  })
})
