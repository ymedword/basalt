// 资源绑定列表：每个引用一行，显示解析状态、声明 UID 与该 UID 的**当前**名称，
// 并允许从候选资源里显式选择一个 UID（选择结果只经事件交给表单，由表单放进
// `changes.bindings` 提交）。组件不按名称猜 UID、不自动补绑定。
// 孤儿绑定（正文已无该引用）只展示、不改写——移除它要走整份正文保存。
// Binding list: one row per reference, with status, declared uid, the uid's current
// name and an explicit candidate pick. The component never resolves names to uids
// and never auto-binds; orphan bindings are shown but not rewritten here.
import { defineComponent, h } from 'vue'
import type { PropType, VNode, VNodeChild } from 'vue'
import type { AgentConfigReference, BindingStatus, ResourceScope } from '../api'
import { resourceDetailHref } from '../router'
import { DataTable, StatusBadge } from './ui'
import type { TableRow, Tone } from './ui'

/** 一个可绑定的候选资源（共享或属于当前 Agent）。 */
export interface BindingCandidate {
  readonly uid: string
  readonly name: string
  readonly scope: ResourceScope
  /** exclusive 时的所有者 Agent 名；shared 为空串。 */
  readonly ownerAgent: string
}

/** 一行绑定：一条引用 + 该种类下页面可选的候选资源。 */
export interface BindingRow {
  readonly key: string
  readonly reference: AgentConfigReference
  readonly candidates: readonly BindingCandidate[]
}

/** 选择事件的载荷：与契约 `AgentContentChanges.bindings` 的一项同形。 */
export interface BindingSelection {
  readonly resource_kind: 'model_connection' | 'notify_channel'
  readonly ref_name: string
  readonly resource_uid: string
}

/** 绑定解析状态的中文标签与色调（契约 `binding_status` 五值）。 */
export const BINDING_STATUS_LABELS: Readonly<
  Record<BindingStatus, { readonly label: string; readonly tone: Tone }>
> = {
  bound: { label: '已关联', tone: 'ok' },
  missing: { label: '未关联', tone: 'warn' },
  stale_uid: { label: '指向旧资源', tone: 'bad' },
  orphan: { label: '孤儿关联', tone: 'warn' },
  duplicate: { label: '重复关联', tone: 'bad' },
}

const KIND_LABELS: Readonly<Record<AgentConfigReference['resource_kind'], string>> = {
  model_connection: '模型连接',
  notify_channel: '通知渠道',
}

export const BindingList = defineComponent({
  name: 'BindingList',
  props: {
    /** 控件前缀（select 的 id）。 */
    id: { type: String, required: true },
    rows: { type: Array as PropType<readonly BindingRow[]>, required: true },
    /** 当前 Agent 名：候选的专属归属标成本 Agent。 */
    agent: { type: String, default: '' },
    disabled: { type: Boolean, default: false },
    /**
     * 只读形态（Agent 结构化配置页）：资源绑定不是第三个编辑入口，切换连接在
     * 模型 / 通知分区完成；这里只展示服务端事实并给出资源管理入口。
     * Read-only form: bindings are edited through the model / notify sections, so this
     * list only shows the server's current resolution plus a management link.
     */
    readonly: { type: Boolean, default: false },
  },
  emits: {
    select: (_selection: BindingSelection) => true,
  },
  setup(props, { emit }) {
    function candidateLabel(candidate: BindingCandidate): string {
      if (candidate.scope === 'shared') {
        return `${candidate.name}（共享）`
      }
      if (candidate.ownerAgent === props.agent) {
        return `${candidate.name}（专属 · 本 Agent）`
      }
      return `${candidate.name}（专属 · ${candidate.ownerAgent}）`
    }

    /**
     * 当前资源列：主界面只显示资源名称；UID 是精确关联的内部标识，放悬停提示。
     * 名称变化（同名重建）不改变关联——关联始终按 UID 判定。
     * The main cell shows the resource name; the UID stays in the tooltip.
     */
    function resourceCell(reference: AgentConfigReference): VNodeChild {
      if (reference.resource_uid === '') {
        return h('span', { class: 'muted' }, '—')
      }
      const name = reference.resource_name === '' ? reference.ref_name : reference.resource_name
      return h('span', { title: `资源 UID：${reference.resource_uid}` }, name)
    }

    /** 状态列：固定标签 + 页面必须知道的阻断原因。 */
    function statusCell(reference: AgentConfigReference): VNode {
      const info = BINDING_STATUS_LABELS[reference.binding_status]
      const parts: VNodeChild[] = [h(StatusBadge, { label: info.label, tone: info.tone })]
      if (reference.resource_uid !== '' && reference.deleted) {
        parts.push(h('span', { class: 'binding-reason' }, '资源已删除'))
      } else if (reference.resource_uid !== '' && !reference.published) {
        parts.push(h('span', { class: 'binding-reason' }, '尚未发布'))
      }
      if (!reference.eligible && reference.resource_uid !== '') {
        parts.push(h('span', { class: 'binding-reason' }, '无使用资格'))
      }
      if (reference.credential_state === 'revoked') {
        parts.push(h('span', { class: 'binding-reason' }, '凭据已撤销'))
      }
      return h('div', { class: 'binding-status' }, parts)
    }

    /** 只读形态的管理入口：模型连接进资源详情，通知渠道进渠道管理。 */
    // Read-only management entry: model connections open the detail page; notify
    // channels open the channel manager (there is no per-channel detail page).
    function manageCell(reference: AgentConfigReference): VNodeChild {
      if (reference.binding_status === 'orphan') {
        return h('span', { class: 'muted' }, '—')
      }
      if (reference.resource_kind === 'notify_channel') {
        return h('a', { href: '/console/settings/notifications' }, '渠道管理')
      }
      if (reference.deleted) {
        return h('span', { class: 'muted' }, '资源已删除')
      }
      const target = reference.resource_name === '' ? reference.ref_name : reference.resource_name
      return h('a', { href: resourceDetailHref('model-connections', target) }, '资源详情')
    }

    function bindingCell(row: BindingRow): VNodeChild {
      const reference = row.reference
      if (reference.binding_status === 'orphan') {
        return h('span', { class: 'muted' }, '正文已无该引用；保存完整正文可移除')
      }
      if (props.readonly) {
        return manageCell(reference)
      }
      const options: VNode[] = [
        h('option', { key: '', value: '' }, '请选择资源'),
      ]
      for (const candidate of row.candidates) {
        options.push(h('option', { key: candidate.uid, value: candidate.uid }, candidateLabel(candidate)))
      }
      return h(
        'select',
        {
          id: `${props.id}-${reference.resource_kind}-${reference.ref_name}`,
          class: 'binding-select',
          'data-ref': `${reference.resource_kind}:${reference.ref_name}`,
          value: reference.resource_uid,
          disabled: props.disabled,
          onChange: (event: Event) => {
            emit('select', {
              resource_kind: reference.resource_kind,
              ref_name: reference.ref_name,
              resource_uid: (event.target as HTMLSelectElement).value,
            })
          },
        },
        options,
      )
    }

    return (): VNode => {
      const rows: TableRow[] = props.rows.map((row) => ({
        key: row.key,
        cells: [
          h('code', {}, row.reference.ref_name),
          KIND_LABELS[row.reference.resource_kind],
          statusCell(row.reference),
          resourceCell(row.reference),
          bindingCell(row),
        ],
      }))
      return h('div', { class: 'binding-section' }, [
        h(DataTable, {
          headers: ['引用', '类型', '状态', '当前资源', props.readonly ? '管理入口' : '关联'],
          rows,
          empty: '没有需要关联的引用',
        }),
        h('details', { class: 'technical-details' }, [
          h('summary', '技术细节'),
          h(
            'p',
            { class: 'muted small' },
            '每一行按资源 UID 精确关联（悬停「当前资源」可看 UID），不是按名称扫描；资源改名不改变关联。',
          ),
        ]),
      ])
    }
  },
})
