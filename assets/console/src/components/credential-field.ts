// 凭据输入（write-only）：密码框只在本次输入里持有新值，保存后由父页面把绑定值
// 重置为空串，界面只保留模式、状态、版本与尾号。已存凭据没有回填路径——组件只
// 渲染 props 给的内容，不缓存、不写存储、不读 DOM。
// Write-only credential input. The parent resets the bound value to '' after a save;
// the page keeps only mode, state, version and last4. Stored secrets have no
// read-back path: the component renders props only and never stores the value.
import { computed, defineComponent, h, ref } from 'vue'
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
    /**
     * 实例加密服务未就绪时禁用「加密托管」选项（设计 §2.4.4 / §2.6.5）：
     * 「环境变量」始终可用，降级路径不能一起被关掉。
     * Disables the managed option while the instance key is not ready; the env
     * source stays available so there is still a usable path.
     */
    managedDisabled: { type: Boolean, default: false },
    /** 禁用「加密托管」的原因与修复动作；只在 managedDisabled 时展示。 */
    // Why managed is disabled; shown only while managedDisabled is set.
    managedReason: { type: String, default: '' },
    /**
     * 状态行的标题（例如模型连接的「当前 API 密钥」）：普通表单把「实例加密服务
     * 是否就绪」与「这个凭据是否已保存」分成两行，标题负责区分两者。
     * Optional title for the status line, so a page can separate "service ready"
     * from "this credential saved".
     */
    statusTitle: { type: String, default: '' },
    /**
     * 是否把「凭据来源」收进「高级」折叠区：普通表单默认加密托管，环境变量是高级
     * 部署选项，不该是每次录入都要做的选择；已用 env 的连接默认展开显示引用。
     * Nests the source selector behind an advanced disclosure; env stays available.
     */
    advancedSource: { type: Boolean, default: false },
    /**
     * 是否显示值输入框。MCP 逐占位符卡片在「环境变量」来源下不在这里填写任何东西
     * （引用写在定义里）——隐藏输入框比留一个会被忽略的框更诚实。
     * Hides the value input where the source forbids a value (per-slot env mode).
     */
    valueVisible: { type: Boolean, default: true },
    disabled: { type: Boolean, default: false },
  },
  emits: {
    'update:value': (_value: string) => true,
    'update:mode': (_mode: CredentialMode) => true,
  },
  setup(props, { emit }) {
    const managed = computed(() => props.mode === 'managed')
    // 高级折叠区的展开状态：已有 env 连接默认展开，让既有配置一眼可见。
    const sourceOpen = ref(props.mode === 'env')
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
        h(
          'option',
          {
            key: mode,
            value: mode,
            // 只灰掉「加密托管」；环境变量来源永远可选。
            disabled: mode === 'managed' && props.managedDisabled,
          },
          CREDENTIAL_MODE_LABELS[mode],
        ),
      )
      const children: VNodeChild[] = [
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
      ]
      if (props.managedDisabled && props.managedReason !== '') {
        children.push(h('span', { class: 'field-hint muted' }, props.managedReason))
      }
      return h('label', { class: 'field', for: `${props.id}-mode` }, children)
    }

    /** 「凭据来源」选择：普通表单收进「高级」折叠区，MCP 逐槽位卡片保持直接选择。 */
    // The source selector; ordinary forms nest it behind an advanced disclosure.
    function sourcePicker(): VNode {
      if (!props.advancedSource) {
        return modeSelect()
      }
      return h(
        'details',
        {
          class: 'credential-advanced',
          open: sourceOpen.value,
          onToggle: (event: Event) => {
            sourceOpen.value = (event.target as HTMLDetailsElement).open
          },
        },
        [
          h('summary', {}, `高级：凭据来源（${CREDENTIAL_MODE_LABELS[props.mode]}）`),
          modeSelect(),
        ],
      )
    }

    function statusLine(): VNode {
      // 这一行只说「这个凭据是否已保存」；不重复模式名——普通表单默认就是加密
      // 托管，来源只在高级折叠区里出现。env 在契约里没有凭据状态（恒为空串），
      // 显示「未录入」会把「有变量引用」说成「没配置」，所以 env 分支只展示引用。
      // The line says whether this credential is saved; the mode is not repeated
      // because managed is the ordinary default. env has no credential state.
      const parts: VNodeChild[] = []
      if (props.statusTitle !== '') {
        parts.push(h('span', { class: 'credential-status-title' }, `${props.statusTitle}：`))
      }
      if (managed.value) {
        if (props.version === 0) {
          parts.push(
            h('span', { class: 'muted' }, props.statusTitle === '' ? `尚未保存${props.noun}` : '尚未保存'),
          )
        } else {
          parts.push(h(StatusBadge, { label: stateInfo.value.label, tone: stateInfo.value.tone }))
          const suffix =
            props.last4 === ''
              ? `版本 ${props.version}`
              : `版本 ${props.version}，尾号 ····${props.last4}`
          parts.push(h('span', {}, `已保存（${suffix}）`))
        }
      } else {
        parts.push(h('span', { class: 'credential-mode' }, CREDENTIAL_MODE_LABELS[props.mode]))
        if (props.reference !== '') {
          parts.push(h('code', {}, props.reference))
        }
      }
      return h('p', { class: 'credential-status' }, parts)
    }

    return (): VNode => {
      const children: VNodeChild[] = []
      if (props.valueVisible) {
        children.push(valueInput())
      }
      if (props.editableMode) {
        children.push(sourcePicker())
      }
      children.push(statusLine(), h('p', { class: 'field-hint muted' }, hint.value))
      return h('div', { class: 'credential-field' }, children)
    }
  },
})
