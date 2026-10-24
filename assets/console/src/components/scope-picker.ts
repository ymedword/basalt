// 资源使用范围选择：shared（全部 Agent 可用）/ exclusive（只给一个 Agent）。
// 选中 exclusive 时给出所有者下拉；共享转专属是否被引用阻断由服务端判定
// （409 `RESOURCE_SCOPE_IN_USE`），组件不预先禁用也不提供 force。
// Scope picker for resources: shared vs exclusive. The component never predicts the
// server's RESOURCE_SCOPE_IN_USE verdict and offers no force switch.
import { computed, defineComponent, h } from 'vue'
import type { PropType, VNode } from 'vue'
import type { ResourceScope } from '../api'

/** 范围的固定中文标签（契约 `scope` 两值）。 */
export const SCOPE_LABELS: Readonly<Record<ResourceScope, string>> = {
  shared: '共享（所有 Agent 可用）',
  exclusive: '专属（只给一个 Agent）',
}

export const ScopePicker = defineComponent({
  name: 'ScopePicker',
  props: {
    /** 控件前缀：radio 的 name 与元素 id 都由它派生。 */
    id: { type: String, required: true },
    scope: { type: String as PropType<ResourceScope>, required: true },
    /** exclusive 时的所有者 Agent 名；shared 时为空串。 */
    ownerAgent: { type: String, default: '' },
    /** 所有者候选（在世 Agent 名）；当前所有者不在列表里时仍会出现在下拉中。 */
    agents: { type: Array as PropType<readonly string[]>, default: () => [] },
    disabled: { type: Boolean, default: false },
  },
  emits: {
    'update:scope': (_scope: ResourceScope) => true,
    'update:ownerAgent': (_name: string) => true,
  },
  setup(props, { emit }) {
    const exclusive = computed(() => props.scope === 'exclusive')
    const ownerChoices = computed<readonly string[]>(() => {
      if (props.ownerAgent === '' || props.agents.includes(props.ownerAgent)) {
        return props.agents
      }
      // 当前所有者可能已归档：保留它，避免切换时静默改选别人。
      return [props.ownerAgent, ...props.agents]
    })

    function radio(scope: ResourceScope): VNode {
      return h('label', { class: 'scope-option', for: `${props.id}-${scope}` }, [
        h('input', {
          id: `${props.id}-${scope}`,
          type: 'radio',
          name: `${props.id}-scope`,
          value: scope,
          checked: props.scope === scope,
          disabled: props.disabled,
          onChange: () => {
            emit('update:scope', scope)
          },
        }),
        h('span', {}, SCOPE_LABELS[scope]),
      ])
    }

    function ownerSelect(): VNode {
      const choices: VNode[] = [
        h('option', { key: '', value: '' }, '请选择所有者 Agent'),
        ...ownerChoices.value.map((agent) =>
          h('option', { key: agent, value: agent }, agent),
        ),
      ]
      return h('label', { class: 'field', for: `${props.id}-owner` }, [
        h('span', '所有者 Agent'),
        h(
          'select',
          {
            id: `${props.id}-owner`,
            value: props.ownerAgent,
            disabled: props.disabled,
            onChange: (event: Event) => {
              emit('update:ownerAgent', (event.target as HTMLSelectElement).value)
            },
          },
          choices,
        ),
      ])
    }

    return (): VNode => {
      const children: VNode[] = [
        h('div', { class: 'scope-options', role: 'radiogroup' }, [
          radio('shared'),
          radio('exclusive'),
        ]),
      ]
      if (exclusive.value) {
        children.push(ownerSelect())
      }
      return h('div', { class: 'scope-picker' }, children)
    }
  },
})
