// 绑定列表组件回归：每条引用一行、状态与阻断原因可见、候选资源按共享 / 专属
// 标注、选择后发出与契约 changes.bindings 同形的载荷、孤儿绑定只展示不改写。
// Binding list regressions: one row per reference with status and blocking reasons,
// candidate labels for shared/exclusive scope, a contract-shaped selection payload,
// and orphan bindings that are displayed but never rewritten here.
import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import type { AgentConfigReference } from '../src/api'
import { BindingList } from '../src/components/binding-list'
import type { BindingCandidate, BindingRow } from '../src/components/binding-list'

const UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'
const OLD_UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B'

function reference(overrides: Partial<AgentConfigReference> = {}): AgentConfigReference {
  return {
    resource_kind: 'model_connection',
    ref_name: 'deepseek',
    resource_uid: UID,
    resource_name: 'deepseek',
    binding_status: 'bound',
    published: true,
    eligible: true,
    deleted: false,
    credential_state: 'active',
    ...overrides,
  }
}

const CANDIDATES: readonly BindingCandidate[] = [
  { uid: UID, name: 'deepseek', scope: 'shared', ownerAgent: '' },
  { uid: OLD_UID, name: 'writer-only', scope: 'exclusive', ownerAgent: 'writer' },
]

function mountList(rows: readonly BindingRow[], agent = 'writer') {
  return mount(BindingList, { props: { id: 'bindings', rows, agent } })
}

describe('BindingList', () => {
  it('renders status, the declared uid and the uid current name', () => {
    const wrapper = mountList([
      {
        key: 'model_connection:deepseek',
        reference: reference({ binding_status: 'stale_uid', resource_name: 'deepseek-v2' }),
        candidates: CANDIDATES,
      },
    ])
    expect(wrapper.text()).toContain('deepseek')
    expect(wrapper.text()).toContain('指向旧资源')
    // UID 是精确关联的内部标识：主文本不显示，只放在悬停提示与技术细节里。
    expect(wrapper.text()).not.toContain(UID)
    const titled = wrapper.findAll('span[title]')
    expect(titled.some((node) => (node.attributes('title') ?? '').includes(UID))).toBe(true)
    expect(wrapper.text()).toContain('deepseek-v2')
  })

  it('labels candidates by scope and emits a contract-shaped selection', async () => {
    // 以另一个 Agent 的身份查看：候选的专属归属显示所有者名而不是“本 Agent”。
    const wrapper = mountList(
      [
        {
          key: 'model_connection:deepseek',
          reference: reference({ binding_status: 'missing', resource_uid: '', resource_name: '' }),
          candidates: CANDIDATES,
        },
      ],
      'reader',
    )
    expect(wrapper.text()).toContain('未关联')
    expect(wrapper.text()).toContain('deepseek（共享）')
    expect(wrapper.text()).toContain('writer-only（专属 · writer）')

    const select = wrapper.find('select.binding-select')
    expect(select.exists()).toBe(true)
    await select.setValue(UID)
    expect(wrapper.emitted('select')).toEqual([
      [{ resource_kind: 'model_connection', ref_name: 'deepseek', resource_uid: UID }],
    ])
  })

  it('marks the exclusive candidate owned by this agent on its own', () => {
    const wrapper = mountList(
      [
        {
          key: 'model_connection:deepseek',
          reference: reference(),
          candidates: [CANDIDATES[1]!],
        },
      ],
      'writer',
    )
    expect(wrapper.text()).toContain('writer-only（专属 · 本 Agent）')
  })

  it('shows blocking reasons for deleted, unpublished, ineligible and revoked resources', () => {
    const wrapper = mountList([
      {
        key: 'model_connection:deepseek',
        reference: reference({
          published: false,
          eligible: false,
          deleted: true,
          credential_state: 'revoked',
        }),
        candidates: CANDIDATES,
      },
    ])
    expect(wrapper.text()).toContain('资源已删除')
    expect(wrapper.text()).toContain('无使用资格')
    expect(wrapper.text()).toContain('凭据已撤销')
    // 已删除的行不会再报“尚未发布”：两者是同一件事的两个阶段。
    expect(wrapper.text()).not.toContain('尚未发布')
  })

  it('shows orphan bindings without a select and explains the full-content path', () => {
    const wrapper = mountList([
      {
        key: 'notify_channel:ops',
        reference: reference({
          resource_kind: 'notify_channel',
          ref_name: 'ops',
          binding_status: 'orphan',
        }),
        candidates: [],
      },
    ])
    expect(wrapper.text()).toContain('孤儿关联')
    expect(wrapper.text()).toContain('保存完整正文可移除')
    expect(wrapper.find('select.binding-select').exists()).toBe(false)
  })

  it('renders an explicit empty state when there is nothing to bind', () => {
    const wrapper = mountList([])
    expect(wrapper.text()).toContain('没有需要关联的引用')
  })

  it('read-only: links model connections to their resource detail instead of a select', () => {
    const wrapper = mount(BindingList, {
      props: {
        id: 'bindings',
        readonly: true,
        rows: [{ key: 'model_connection:deepseek', reference: reference(), candidates: CANDIDATES }],
      },
    })
    // 表头变成管理入口；绑定编辑只发生在模型 / 通知分区，这里不是第三个入口。
    expect(wrapper.findAll('th').map((cell) => cell.text())).toEqual([
      '引用',
      '类型',
      '状态',
      '当前资源',
      '管理入口',
    ])
    expect(wrapper.find('select.binding-select').exists()).toBe(false)
    const link = wrapper.find('a')
    expect(link.text()).toBe('资源详情')
    expect(link.attributes('href')).toBe('/console/resources/model-connections/deepseek')
  })

  it('read-only: follows the current name of a rebuilt resource', () => {
    const wrapper = mount(BindingList, {
      props: {
        id: 'bindings',
        readonly: true,
        rows: [
          {
            key: 'model_connection:deepseek',
            reference: reference({ binding_status: 'stale_uid', resource_name: 'deepseek-v2' }),
            candidates: CANDIDATES,
          },
        ],
      },
    })
    expect(wrapper.text()).toContain('指向旧资源')
    const link = wrapper.find('a')
    expect(link.attributes('href')).toBe('/console/resources/model-connections/deepseek-v2')
  })

  it('read-only: notify channels open the channel manager', () => {
    const wrapper = mount(BindingList, {
      props: {
        id: 'bindings',
        readonly: true,
        rows: [
          {
            key: 'notify_channel:ops',
            reference: reference({
              resource_kind: 'notify_channel',
              ref_name: 'ops',
              resource_name: 'ops',
            }),
            candidates: [],
          },
        ],
      },
    })
    const link = wrapper.find('a')
    expect(link.text()).toBe('渠道管理')
    expect(link.attributes('href')).toBe('/console/settings/notifications')
  })

  it('read-only: a deleted resource has no detail entry', () => {
    const wrapper = mount(BindingList, {
      props: {
        id: 'bindings',
        readonly: true,
        rows: [
          {
            key: 'model_connection:deepseek',
            reference: reference({ deleted: true, binding_status: 'missing' }),
            candidates: [],
          },
        ],
      },
    })
    expect(wrapper.text()).toContain('资源已删除')
    expect(wrapper.find('a').exists()).toBe(false)
  })

  it('read-only: orphan bindings show a dash and cannot be edited here', () => {
    const wrapper = mount(BindingList, {
      props: {
        id: 'bindings',
        readonly: true,
        rows: [
          {
            key: 'notify_channel:ops',
            reference: reference({
              resource_kind: 'notify_channel',
              ref_name: 'ops',
              binding_status: 'orphan',
            }),
            candidates: [],
          },
        ],
      },
    })
    expect(wrapper.text()).toContain('孤儿关联')
    expect(wrapper.find('a').exists()).toBe(false)
    expect(wrapper.find('select.binding-select').exists()).toBe(false)
  })
})
