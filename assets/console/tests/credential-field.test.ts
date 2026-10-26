// 凭据输入组件回归：managed 模式是密码框且正文不回显、保存后父页面把绑定值清空
// 时 DOM 值随之清空、状态 / 版本 / 尾号可见、env 模式用文本框并给出占位提示、
// 模式选择只在可编辑时出现。
// Credential field regressions: managed hides the value behind a password input and
// never renders it as text, clearing the bound value clears the DOM value, the
// state/version/last4 line is visible, env uses a text input, and the mode selector
// only appears when editable.
import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { CredentialField } from '../src/components/credential-field'

const SECRET = 'sk-live-0123456789abcdef'

describe('CredentialField', () => {
  it('masks managed input and never echoes the value as text', () => {
    const wrapper = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'managed', value: SECRET },
    })
    const input = wrapper.find('#conn-key')
    expect(input.attributes('type')).toBe('password')
    // 输入框里放的是用户刚输入的值（提交需要），但正文与其他字段没有它的载体。
    expect((input.element as HTMLInputElement).value).toBe(SECRET)
    expect(wrapper.text()).not.toContain(SECRET)
  })

  it('shows the state, version and last four digits instead of the stored secret', () => {
    const wrapper = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'managed', state: 'active', version: 3, last4: '9f2c' },
    })
    expect(wrapper.text()).toContain('有效')
    expect(wrapper.text()).toContain('版本 3')
    expect(wrapper.text()).toContain('尾号 ····9f2c')
    expect(wrapper.find('#conn-key').attributes('type')).toBe('password')
  })

  it('emits the typed value and clears the DOM value when the draft is reset', async () => {
    const wrapper = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'managed', value: '' },
    })
    const input = wrapper.find('#conn-key')
    await input.setValue(SECRET)
    expect(wrapper.emitted('update:value')).toEqual([[SECRET]])

    // 父页面在保存成功后把草稿置空：DOM 输入框随之为空，只剩状态与尾号。
    await wrapper.setProps({ value: SECRET })
    expect((input.element as HTMLInputElement).value).toBe(SECRET)
    await wrapper.setProps({ value: '', state: 'active', version: 4, last4: '9f2c' })
    expect((wrapper.find('#conn-key').element as HTMLInputElement).value).toBe('')
    expect(wrapper.text()).toContain('尾号 ····9f2c')
  })

  it('uses a text input and a placeholder hint for env references', () => {
    const wrapper = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'env', value: '' },
    })
    const input = wrapper.find('#conn-key')
    expect(input.attributes('type')).toBe('text')
    expect(input.attributes('placeholder')).toContain('${')
    expect(wrapper.text()).toContain('这里只保存变量名')
  })

  it('shows the env reference in the status line instead of an unset badge', () => {
    const wrapper = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'env', reference: '${MAIN_KEY}' },
    })
    expect(wrapper.text()).toContain('环境变量')
    expect(wrapper.text()).toContain('${MAIN_KEY}')
    // env 在契约里没有凭据状态：显示「未录入」会把「有变量引用」说成「没配置」。
    expect(wrapper.text()).not.toContain('未录入')
    expect(wrapper.find('#conn-key').attributes('type')).toBe('text')
  })

  it('only renders the mode selector when editable and reports the choice', async () => {
    const readOnly = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'env' },
    })
    expect(readOnly.find('#conn-key-mode').exists()).toBe(false)

    const editable = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'env', editableMode: true },
    })
    await editable.find('#conn-key-mode').setValue('managed')
    expect(editable.emitted('update:mode')).toEqual([['managed']])
  })

  it('separates the saved/not-saved line under an explicit status title', () => {
    const empty = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'managed', statusTitle: '当前 API 密钥' },
    })
    expect(empty.text()).toContain('当前 API 密钥：')
    expect(empty.text()).toContain('尚未保存')
    // 状态行不重复模式名：普通表单默认就是加密托管。
    expect(empty.text()).not.toContain('尚无加密托管')

    const saved = mount(CredentialField, {
      props: {
        id: 'conn-key',
        mode: 'managed',
        statusTitle: '当前 API 密钥',
        state: 'active',
        version: 2,
        last4: '9f2c',
      },
    })
    expect(saved.text()).toContain('已保存（版本 2，尾号 ····9f2c）')
    expect(saved.text()).toContain('有效')
  })

  it('nests the source selector behind an advanced disclosure for ordinary forms', () => {
    const managed = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'managed', editableMode: true, advancedSource: true },
    })
    const details = managed.find('details.credential-advanced')
    expect(details.exists()).toBe(true)
    // 默认收起：普通录入不需要先选择凭据来源。
    expect(details.attributes('open')).toBeUndefined()
    expect(details.find('summary').text()).toBe('高级：凭据来源（加密托管）')
    // 选择器仍在 DOM 里，可直接切换成环境变量。
    expect(details.find('#conn-key-mode').exists()).toBe(true)

    const env = mount(CredentialField, {
      props: { id: 'conn-key', mode: 'env', editableMode: true, advancedSource: true },
    })
    // 既有 env 配置默认展开，引用一眼可见。
    expect(env.find('details.credential-advanced').attributes('open')).toBeDefined()
    expect(env.find('details.credential-advanced summary').text()).toBe(
      '高级：凭据来源（环境变量）',
    )
  })
})
