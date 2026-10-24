// 范围选择组件回归：两个固定选项、共享态不出现所有者下拉、切到专属后给出
// 候选 Agent 且保留已归档的当前所有者、禁用态不可操作。
// Scope picker regressions: the two fixed scopes, no owner select while shared, an
// owner select with candidates after switching to exclusive (keeping an archived
// current owner in the list), and a disabled state that cannot be operated.
import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { ScopePicker } from '../src/components/scope-picker'

describe('ScopePicker', () => {
  it('renders both scopes and checks the current one', () => {
    const wrapper = mount(ScopePicker, {
      props: { id: 'scope', scope: 'shared' },
    })
    const shared = wrapper.find('#scope-shared')
    const exclusive = wrapper.find('#scope-exclusive')
    expect((shared.element as HTMLInputElement).checked).toBe(true)
    expect((exclusive.element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.text()).toContain('共享（所有 Agent 可用）')
    expect(wrapper.text()).toContain('专属（只给一个 Agent）')
  })

  it('emits the chosen scope and shows the owner select only for exclusive', async () => {
    const wrapper = mount(ScopePicker, {
      props: { id: 'scope', scope: 'shared', agents: ['writer', 'reader'] },
    })
    expect(wrapper.find('#scope-owner').exists()).toBe(false)

    await wrapper.find('#scope-exclusive').setValue()
    expect(wrapper.emitted('update:scope')).toEqual([['exclusive']])

    // 受控组件：父页面把 scope 换成 exclusive 后才出现所有者下拉。
    await wrapper.setProps({ scope: 'exclusive' })
    const owner = wrapper.find('#scope-owner')
    expect(owner.exists()).toBe(true)
    expect(wrapper.text()).toContain('writer')
    expect(wrapper.text()).toContain('reader')
  })

  it('emits the selected owner and keeps an archived current owner listed', async () => {
    const wrapper = mount(ScopePicker, {
      props: {
        id: 'scope',
        scope: 'exclusive',
        ownerAgent: 'archived-agent',
        agents: ['writer'],
      },
    })
    const owner = wrapper.find('#scope-owner')
    expect(wrapper.text()).toContain('archived-agent')
    await owner.setValue('writer')
    expect(wrapper.emitted('update:ownerAgent')).toEqual([['writer']])
  })

  it('disables every control when disabled', () => {
    const wrapper = mount(ScopePicker, {
      props: { id: 'scope', scope: 'exclusive', ownerAgent: 'writer', agents: ['writer'], disabled: true },
    })
    expect(wrapper.find('#scope-shared').attributes('disabled')).toBeDefined()
    expect(wrapper.find('#scope-exclusive').attributes('disabled')).toBeDefined()
    expect(wrapper.find('#scope-owner').attributes('disabled')).toBeDefined()
  })
})
