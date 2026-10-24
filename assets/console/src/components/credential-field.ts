// 凭据输入（write-only）：密码框只在本次输入里持有新值，保存后由父页面把绑定值
// 重置为空串，界面只保留模式、状态、版本与尾号。已存凭据没有回填路径——组件只
// 渲染 props 给的内容，不缓存、不写存储、不读 DOM。
// Write-only credential input. The parent resets the bound value to '' after a save;
// the page keeps only mode, state, version and last4. Stored secrets have no
// read-back path: the component renders props only and never stores the value.
import { computed, defineComponent, h } from 'vue'
import type { PropType, VNode, VNodeChild } from 'vue'
import type { CredentialMode, CredentialState } from '../api'
import { StatusBadge } from './ui'
import type { Tone } from './ui'

/** 凭据状态的标签与色调（契约 `credential_state` 四值）。 */
export const CREDENTIAL_STATE_LABELS: Readonly<
  Record<CredentialState, { readonly label: string; readonly tone: Tone }>
> = {
  '': { label: '未录入', tone: 'muted' },
  active: { label: '有效', tone: 'ok' },
  revoked: { label: '已撤销', tone: 'bad' },
  retired: { label: '已停用', tone: 'muted' },
}

/** 凭据来源的标签（契约 `credential_mode` 两值；页面只说来源，不暴露实现词）。 */
// Credential source labels; the wire values stay env / managed.
export const CREDENTIAL_MODE_LABELS: Readonly<Record<CredentialMode, string>> = {
  env: '环境变量',
  managed: '加密托管',
}

export const CredentialField = defineComponent({
  name: 'CredentialField',
  props: {
    /** 输入框 id：label 的 for 与测试定位都靠它。 */
    id: { type: String, required: true },
    label: { type: String, default: '凭据' },
    /** 资源当前生效的模式；`editableMode` 时只作为下拉初值。 */
    mode: { type: String as PropType<CredentialMode>, required: true },
    state: { type: String as PropType<CredentialState>, default: '' },
    /** 当前活动凭据版本；env 模式为 0。 */
    version: { type: Number, default: 0 },
    /** 托管密钥尾号；长度不足 8 位时服务端给空串。 */
    last4: { type: String, default: '' },
    /** 页面输入的新值（write-only 草稿）；保存成功后由父页面清空。 */
    value: { type: String, default: '' },
    /** 输入框占位；缺省按模式给出密钥文案（通知地址等场景可覆盖）。 */
    placeholder: { type: String, default: '' },
    /** 输入框下方的说明；缺省按模式给出密钥纪律文案。 */
    hint: { type: String, default: '' },
    /** 托管对象的名词（默认「密钥」；通知渠道传「地址」）。 */
    noun: { type: String, default: '密钥' },
    /**
     * env 模式的只读引用（例如 `${DEEPSEEK_API_KEY}`）：env 没有凭据状态，
     * 展示引用本身才是「当前配置」的事实。
     * Read-only env reference; env mode has no credential state to show.
     */
    reference: { type: String, default: '' },
    /** 是否允许在这里切换模式（资源表单）；详情卡片只读时为 false。 */
    editableMode: { type: Boolean, default: false },
    disabled: { type: Boolean, default: false },
  },
  emits: {
    'update:value': (_value: string) => true,
    'update:mode': (_mode: CredentialMode) => true,
  },
  setup(props, { emit }) {
    const managed = computed(() => props.mode === 'managed')
    const placeholder = computed(() => {
      if (props.placeholder !== '') {
        return props.placeholder
      }
      return managed.value ? `粘贴完整的${props.noun}（保存后不回显原值）` : '${API_KEY_ENV}'
    })
    const hint = computed(() => {
      if (props.hint !== '') {
        return props.hint
      }
      return managed.value
        ? '输入原值，由系统加密保存；保存后不回显原值。'
        : `这里只保存变量名，例如 \${API_KEY_ENV}；变量值由部署者在进程环境中配置，本页不读取也不保存。`
    })
    const stateInfo = computed(() => CREDENTIAL_STATE_LABELS[props.state])

    function valueInput(): VNode {
      return h('label', { class: 'field', for: props.id }, [
        h('span', {}, props.label),
        h('input', {
          id: props.id,
          // managed 模式是真正的秘密：一律密码框；env 模式输入的是变量引用，
          // 不是秘密，用文本框避免把 `${VAR}` 遮成圆点。
          type: managed.value ? 'password' : 'text',
          value: props.value,
          placeholder: placeholder.value,
          autocomplete: 'new-password',
          spellcheck: false,
          disabled: props.disabled,
          onInput: (event: Event) => {
            emit('update:value', (event.target as HTMLInputElement).value)
          },
        }),
      ])
    }

    function modeSelect(): VNode {
      const choices: VNode[] = (['env', 'managed'] as const).map((mode) =>
        h('option', { key: mode, value: mode }, CREDENTIAL_MODE_LABELS[mode]),
      )
      return h('label', { class: 'field', for: `${props.id}-mode` }, [
        h('span', '凭据来源'),
        h(
          'select',
          {
            id: `${props.id}-mode`,
            value: props.mode,
            disabled: props.disabled,
            onChange: (event: Event) => {
              emit('update:mode', (event.target as HTMLSelectElement).value as CredentialMode)
            },
          },
          choices,
        ),
      ])
    }

    function statusLine(): VNode {
      // env 模式在契约里没有凭据状态（credential_state 恒为空串）：显示「未录入」
      // 会把「有变量引用」说成「没配置」。env 分支只展示模式与引用。
      // env has no credential state; only managed shows the state badge.
      const parts: VNodeChild[] = []
      if (managed.value) {
        parts.push(h(StatusBadge, { label: stateInfo.value.label, tone: stateInfo.value.tone }))
      }
      parts.push(h('span', { class: 'credential-mode' }, CREDENTIAL_MODE_LABELS[props.mode]))
      if (managed.value) {
        if (props.version > 0) {
          parts.push(h('span', {}, `凭据版本 ${props.version}`))
        }
        if (props.last4 !== '') {
          parts.push(h('code', { class: 'credential-last4' }, `尾号 ····${props.last4}`))
        } else {
          parts.push(h('span', { class: 'muted' }, `尚无加密托管的${props.noun}`))
        }
      } else if (props.reference !== '') {
        parts.push(h('code', {}, props.reference))
      }
      return h('p', { class: 'credential-status' }, parts)
    }

    return (): VNode => {
      const children: VNodeChild[] = [valueInput()]
      if (props.editableMode) {
        children.push(modeSelect())
      }
      children.push(statusLine(), h('p', { class: 'field-hint muted' }, hint.value))
      return h('div', { class: 'credential-field' }, children)
    }
  },
})
