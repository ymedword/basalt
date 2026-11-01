// 管理面记忆条目组件（US-16 裁决 A）：结构化读模型 + 四个显式写动作。
//
// 三条边界在界面上必须如实表达，不能靠用户猜：
//  1. **管理视图 ≠ 召回面**——这里显示某一层的全部状态（含候选与已删）；能在这里
//     看到，不代表任何业务调用方或该 Agent 在运行时也能检索到（召回只返回生效且
//     未到期的条目）。
//  2. **候选不自动生效**——新提议恒落 candidate，确认是唯一晋升路径；不会从对话或
//     运行结束自动抽取（自动记忆属于后续设计，不在此处）。
//  3. **删除是软删**——行与审计保留、立即不可召回；重复删除幂等成功。
//
// 按钮按状态开放，不提供"点了注定 409"的入口：候选可确认 / 删除；生效可纠正 / 删除；
// 冲突待裁决可删除（放弃这条冲突记录）；其余历史行（已被替代 / 已被纠正 / 已删除）
// 只读——它们的作用是留证据，不是可编辑对象。
// The management-side memory component: structured read model plus the four explicit
// writes. Buttons follow the six states so the page never offers an action the server
// would reject; history rows stay read-only because their job is evidence.
import { defineComponent, h, onMounted, ref, watch } from 'vue'
import type { PropType, VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import type { ApiClient, MemoryEntryState, MemoryEntryView } from '../api'
import { AdminApi } from '../api-admin'
import type { MemoryCorrectionWrite, MemoryProposalWrite } from '../api-admin'
import { ConfirmDialog, DataTable, Notice, StatusBadge, formatTimestamp } from './ui'
import type { Tone } from './ui'

/** 行内动作：按状态开放，规则见 `memoryEntryActions`。 */
export type MemoryEntryAction = 'confirm' | 'correct' | 'delete'

/**
 * 某状态下的可用动作。这是**界面**的开放规则，服务端仍按自己的状态机复核：
 * 两边一致才是可预期，界面放宽只会制造 409，界面收紧会把本来允许的动作藏起来。
 */
export function memoryEntryActions(state: MemoryEntryState): readonly MemoryEntryAction[] {
  switch (state) {
    case 'candidate':
      return ['confirm', 'delete']
    case 'active':
      return ['correct', 'delete']
    case 'conflicted':
      return ['delete']
    default:
      return []
  }
}

/** 六态中文标签（与契约字面量一一对应；没有 expired——到期由 valid_to 表达）。 */
const STATE_LABELS: Record<MemoryEntryState, string> = {
  candidate: '候选',
  active: '生效',
  superseded: '已被替代',
  corrected: '已被纠正',
  deleted: '已删除',
  conflicted: '冲突待裁决',
}

/** 徽标色调：生效是正常态；候选与冲突需要人看；历史行退到中性。 */
const STATE_TONES: Record<MemoryEntryState, Tone> = {
  candidate: 'warn',
  active: 'ok',
  superseded: 'muted',
  corrected: 'muted',
  deleted: 'muted',
  conflicted: 'bad',
}

const SECTION_LABELS: Record<MemoryEntryView['section'], string> = {
  core: '核心（常驻注入）',
  archive: '归档（可检索）',
}

const SOURCE_LABELS: Record<MemoryEntryView['source'], string> = {
  save_memory: '运行中保存',
  explicit_propose: '显式提议',
  legacy_import: '旧记忆导入',
  migration: '迁移写入',
}

/** 状态过滤的选项（六态 + 全部）；取值与契约枚举逐字一致。 */
const STATE_FILTERS: ReadonlyArray<{ value: MemoryEntryState | ''; label: string }> = [
  { value: '', label: '全部状态' },
  { value: 'candidate', label: STATE_LABELS.candidate },
  { value: 'active', label: STATE_LABELS.active },
  { value: 'conflicted', label: STATE_LABELS.conflicted },
  { value: 'corrected', label: STATE_LABELS.corrected },
  { value: 'superseded', label: STATE_LABELS.superseded },
  { value: 'deleted', label: STATE_LABELS.deleted },
]

interface Feedback {
  text: string
  tone: 'info' | 'warn'
}

/** 正文预览：确认框里只放摘要，避免长正文把对话框撑开。 */
function snippet(content: string): string {
  const text = content.trim()
  return text.length > 60 ? `${text.slice(0, 60)}…` : text
}

/**
 * MemoryEntries 是全局层与 Agent 层共用的管理面板：`agent` 空串 = 全局层。
 * 写入一律"服务端说了算"——成功后整表重读，不做乐观更新（回执只用来写提示）。
 */
export const MemoryEntries = defineComponent({
  name: 'MemoryEntries',
  props: {
    api: { type: Object as PropType<ApiClient>, required: true },
    /** 空串 = 全局层；非空 = 该 Agent 那一层。 */
    agent: { type: String, default: '' },
  },
  setup(props) {
    const admin = new AdminApi(props.api)
    const entries = ref<MemoryEntryView[]>([])
    const filter = ref<MemoryEntryState | ''>('')
    const loading = ref(false)
    const busy = ref(false)
    const error = ref('')
    const feedback = ref<Feedback | null>(null)
    const draftSection = ref<'archive' | 'core'>('archive')
    const draftContent = ref('')
    const editingID = ref(0)
    const editContent = ref('')
    const pendingDeleteID = ref(0)

    async function load(): Promise<void> {
      loading.value = true
      try {
        const view = await admin.listMemoryEntries(props.agent, filter.value)
        entries.value = view.entries
        error.value = ''
      } catch (err) {
        error.value = errorMessage(err)
      } finally {
        loading.value = false
      }
    }

    /** 所有写动作共用：互斥、先清提示、成功后整表重读（不做乐观更新）。 */
    async function submit(action: () => Promise<Feedback>): Promise<void> {
      if (busy.value) {
        return
      }
      busy.value = true
      error.value = ''
      feedback.value = null
      try {
        const result = await action()
        feedback.value = result
        await load()
      } catch (err) {
        error.value = errorMessage(err)
      } finally {
        busy.value = false
      }
    }

    function propose(): void {
      const body: MemoryProposalWrite = { content: draftContent.value }
      if (draftSection.value === 'core') {
        body.section = 'core'
      }
      void submit(async () => {
        const receipt = await admin.proposeMemory(props.agent, body)
        draftContent.value = ''
        if (receipt.merged) {
          return {
            tone: 'info',
            text: `已合并到既有条目 #${receipt.entry_id}（状态：${STATE_LABELS[receipt.state]}）——本次没有新增行。`,
          }
        }
        return {
          tone: 'info',
          text: `已落候选 #${receipt.entry_id}：确认后才参与召回，在此之前不会进入提示词。`,
        }
      })
    }

    function confirmEntry(entry: MemoryEntryView): void {
      void submit(async () => {
        const receipt = await admin.confirmMemory(entry.entry_id)
        return { tone: 'info', text: `条目 #${receipt.entry_id} 已生效，后续召回可用。` }
      })
    }

    function startEdit(entry: MemoryEntryView): void {
      editingID.value = entry.entry_id
      editContent.value = entry.content
      error.value = ''
      feedback.value = null
    }

    function cancelEdit(): void {
      editingID.value = 0
      editContent.value = ''
    }

    function saveEdit(entry: MemoryEntryView): void {
      const body: MemoryCorrectionWrite = { content: editContent.value }
      void submit(async () => {
        const receipt = await admin.correctMemory(entry.entry_id, body)
        editingID.value = 0
        editContent.value = ''
        if (receipt.state === 'conflicted') {
          return {
            tone: 'warn',
            text:
              `目标 #${entry.entry_id} 已不在有效态：已记录冲突条目 #${receipt.entry_id} 待人工裁决，` +
              '旧条目未改动。',
          }
        }
        return {
          tone: 'info',
          text: `#${entry.entry_id} 已退役（已被纠正），#${receipt.entry_id} 接替为生效条目。`,
        }
      })
    }

    function askDelete(entry: MemoryEntryView): void {
      pendingDeleteID.value = entry.entry_id
    }

    function cancelDelete(): void {
      pendingDeleteID.value = 0
    }

    function confirmDelete(): void {
      const target = entries.value.find(entry => entry.entry_id === pendingDeleteID.value)
      if (target === undefined) {
        pendingDeleteID.value = 0
        return
      }
      void submit(async () => {
        const receipt = await admin.deleteMemory(target.entry_id)
        pendingDeleteID.value = 0
        return {
          tone: 'info',
          text: `条目 #${receipt.entry_id} 已软删：行与审计保留，之后不会再被召回。`,
        }
      })
    }

    onMounted(load)
    watch(() => props.agent, () => {
      cancelEdit()
      cancelDelete()
      void load()
    })
    watch(filter, () => {
      void load()
    })

    // --- 渲染 ---------------------------------------------------------------

    function renderContent(entry: MemoryEntryView): VNodeChild {
      if (editingID.value === entry.entry_id) {
        return h('div', { class: 'memory-entry-editor' }, [
          h('textarea', {
            class: 'editor',
            rows: 3,
            value: editContent.value,
            'aria-label': `纠正条目 #${entry.entry_id}`,
            onInput: (event: Event) => {
              editContent.value = (event.target as HTMLTextAreaElement).value
            },
          }),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                class: 'primary',
                disabled: busy.value || editContent.value.trim() === '',
                onClick: () => saveEdit(entry),
              },
              '保存纠正',
            ),
            h('button', { type: 'button', disabled: busy.value, onClick: cancelEdit }, '取消'),
          ]),
        ])
      }
      const children: VNodeChild[] = [
        h('span', { class: 'cell-id', title: `条目 #${entry.entry_id}` }, `#${entry.entry_id}`),
        h('span', { class: 'memory-entry-content', title: entry.content }, entry.content),
      ]
      if (entry.supersedes_id > 0) {
        children.push(
          h('span', { class: 'muted small' }, `替代 #${entry.supersedes_id}`),
        )
      }
      return h('div', { class: 'memory-entry-body' }, children)
    }

    function renderActions(entry: MemoryEntryView): VNodeChild {
      const actions = memoryEntryActions(entry.state)
      if (actions.length === 0) {
        return h('span', { class: 'muted' }, '历史行（只读）')
      }
      const buttons: VNode[] = []
      for (const action of actions) {
        switch (action) {
          case 'confirm':
            buttons.push(
              h(
                'button',
                {
                  type: 'button',
                  disabled: busy.value,
                  title: '确认后这条候选才进召回面',
                  onClick: () => confirmEntry(entry),
                },
                '确认',
              ),
            )
            break
          case 'correct':
            buttons.push(
              h(
                'button',
                { type: 'button', disabled: busy.value, onClick: () => startEdit(entry) },
                '纠正',
              ),
            )
            break
          case 'delete':
            buttons.push(
              h(
                'button',
                { type: 'button', disabled: busy.value, onClick: () => askDelete(entry) },
                '删除',
              ),
            )
            break
        }
      }
      return h('div', { class: 'form-actions' }, buttons)
    }

    function renderProposalForm(): VNode {
      return h('div', { class: 'card memory-propose' }, [
        h('h2', '显式提议一条候选'),
        h('label', { class: 'field', for: 'memory-propose-content' }, [
          h('span', '正文'),
          h('textarea', {
            id: 'memory-propose-content',
            rows: 3,
            value: draftContent.value,
            placeholder: '要记住的事实、偏好或口径；确认后才会进入召回面',
            onInput: (event: Event) => {
              draftContent.value = (event.target as HTMLTextAreaElement).value
            },
          }),
        ]),
        h('label', { class: 'field', for: 'memory-propose-section' }, [
          h('span', '分区'),
          h(
            'select',
            {
              id: 'memory-propose-section',
              value: draftSection.value,
              onChange: (event: Event) => {
                draftSection.value = (event.target as HTMLSelectElement).value as 'archive' | 'core'
              },
            },
            [
              h('option', { value: 'archive' }, SECTION_LABELS.archive),
              h('option', { value: 'core' }, SECTION_LABELS.core),
            ],
          ),
        ]),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: busy.value || draftContent.value.trim() === '',
              onClick: propose,
            },
            '提交候选',
          ),
        ]),
        h(
          'p',
          { class: 'muted small' },
          '新行恒为候选，确认前不参与召回；正文只进条目行与读模型，审计与日志里只有摘要。' +
            '提议不会从对话或运行结束自动产生——自动抽取属于后续设计。',
        ),
      ])
    }

    function renderFilters(): VNode {
      return h('div', { class: 'memory-filters' }, [
        h('label', { class: 'field', for: 'memory-filter' }, [
          h('span', '状态'),
          h(
            'select',
            {
              id: 'memory-filter',
              value: filter.value,
              onChange: (event: Event) => {
                filter.value = (event.target as HTMLSelectElement).value as MemoryEntryState | ''
              },
            },
            STATE_FILTERS.map(item => h('option', { key: item.value, value: item.value }, item.label)),
          ),
        ]),
        h(
          'p',
          { class: 'muted small' },
          loading.value ? '正在读取条目…' : `当前显示 ${entries.value.length} 条。`,
        ),
      ])
    }

    function renderTable(): VNode {
      const rows = entries.value.map(entry => ({
        key: String(entry.entry_id),
        cells: [
          h(StatusBadge, { label: STATE_LABELS[entry.state], tone: STATE_TONES[entry.state] }),
          renderContent(entry),
          SECTION_LABELS[entry.section],
          SOURCE_LABELS[entry.source],
          formatTimestamp(entry.created_at),
          renderActions(entry),
        ],
      }))
      return h(DataTable, {
        tableClass: 'memory-entries-table',
        columnClasses: [
          'col-memory-state',
          'col-memory-content',
          'col-memory-section',
          'col-memory-source',
          'col-memory-time',
          'col-memory-actions',
        ],
        headers: ['状态', '正文', '分区', '来源', '写入时间', '操作'],
        rows,
        empty:
          filter.value === ''
            ? '这一层还没有记忆条目。运行中 save_memory、显式提议或迁移导入才会产生条目。'
            : `没有处于「${STATE_LABELS[filter.value]}」状态的条目。`,
      })
    }

    return (): VNode => {
      const panels: VNodeChild[] = [
        h(
          'p',
          { class: 'muted small' },
          '本页是管理视图：显示这一层全部状态的条目（含候选与已删）。能在这里看到，不代表任何业务调用方' +
            '或该 Agent 在运行时也能检索到同样的内容——召回只返回生效且未到期的条目。',
        ),
        error.value !== '' ? h(Notice, { tone: 'error', text: error.value }) : null,
        feedback.value === null
          ? null
          : h(Notice, {
              tone: feedback.value.tone === 'warn' ? 'warn' : 'info',
              text: feedback.value.text,
            }),
        renderProposalForm(),
        renderFilters(),
        renderTable(),
        h(ConfirmDialog, {
          open: pendingDeleteID.value !== 0,
          title: '删除记忆条目',
          message: (() => {
            const target = entries.value.find(entry => entry.entry_id === pendingDeleteID.value)
            if (target === undefined) {
              return '目标条目已不在当前列表。'
            }
            return (
              `将软删条目 #${target.entry_id}（${STATE_LABELS[target.state]}）：${snippet(target.content)}。` +
              '行与审计保留，删除后立即不可召回；重复删除幂等。'
            )
          })(),
          confirmText: '确认删除',
          busy: busy.value,
          onConfirm: confirmDelete,
          onCancel: cancelDelete,
        }),
      ]
      return h('div', { class: 'memory-management' }, panels)
    }
  },
})
