// US-22 受控自我进化：「记忆」页的「待确认」与「学习记录」两个页签
// （颗粒度文档 §3.1.10 / FR-22-11）。
//
// 五条边界在界面上必须如实表达：
//  1. **候选不自动生效**——自动抽取只产 candidate；确认之前不参与任何召回或注入。
//  2. **不新增写端点**——确认与删除复用既有记忆管理端点，启用只改 `basalt.yaml`。
//  3. **学习记录只读**——没有启用开关、手动抽取、重试、发布或批准按钮。
//  4. **替代前必须对照**——新旧正文同屏展示；没有 `suggested_supersedes_id` 时
//     「替代」不可点，也不提交空目标。
//  5. **上下文不丢**——页签与双向深链都走查询串，切换时保留 `agent`；锚点指向的
//     记录已不存在时显示「已不存在」，不回退成 404 或静默落到别的记录。
// The two memory tabs of US-22: the pending-candidate review and the read-only
// learning records. Every tab and anchor lives in the query string, and the agent
// context survives every switch; a missing anchor is reported, never silently swapped.
import { computed, defineComponent, h, onMounted, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type {
  LearningDraftDetailView,
  LearningDraftMetaView,
  LearningExtractionDetailView,
  LearningExtractionState,
  LearningExtractionView,
  LearningStatusView,
  MemoryEntryState,
  MemoryEntryView,
  ProfileView,
} from '../api'
import { AdminApi } from '../api-admin'
import { ConfirmDialog, DataTable, Notice, StatusBadge, formatTimestamp, shortHash } from '../components/ui'
import type { Tone } from '../components/ui'
import { memoryHref } from '../router'
import type { MemoryTab } from '../router'
import { definitions, viewProps } from './shared'

/** 查询串里的页签；未知取值按缺省「记忆库」处理，与前端的宽容口径一致。 */
export function memoryTabOf(query: URLSearchParams): MemoryTab {
  const tab = query.get('tab')
  return tab === 'pending' || tab === 'records' ? tab : 'library'
}

const TAB_LABELS: ReadonlyArray<{ id: MemoryTab; label: string }> = [
  { id: 'library', label: '记忆库' },
  { id: 'pending', label: '待确认' },
  { id: 'records', label: '学习记录' },
]

/**
 * 三个页签的导航：只切查询串，不新增路由与左侧入口（信息架构裁决 2026-11-05）。
 * `agent` 随页签保留；`candidate` / `extraction` / `draft` 锚点不跨页签携带。
 */
export const MemoryTabNav = defineComponent({
  name: 'MemoryTabNav',
  props: {
    active: { type: String, required: true },
    agent: { type: String, default: '' },
  },
  setup(props) {
    return (): VNode =>
      h(
        'nav',
        { class: 'tabs memory-tabs', 'aria-label': '记忆页签' },
        TAB_LABELS.map(tab =>
          h(
            'a',
            {
              key: tab.id,
              href: memoryHref(tab.id, { agent: props.agent }),
              class: ['tab', props.active === tab.id ? 'active' : ''],
              'aria-current': props.active === tab.id ? 'page' : undefined,
            },
            tab.label,
          ),
        ),
      )
  },
})

interface Feedback {
  text: string
  tone: 'info' | 'warn'
}

const ROLE_LABELS: Record<string, string> = { user: '用户', assistant: '助手' }

/** 候选类型闭集（`method` 只进草案，不写记忆条目）。 */
const KIND_LABELS: Record<string, string> = {
  fact: '事实',
  preference: '偏好',
  constraint: '约束',
  inference: '推断',
  temporary: '临时',
  method: '方法',
}

const CERTAINTY_LABELS: Record<string, string> = {
  asserted: '用户陈述',
  uncertain: '不确定',
}

const TIME_CONFIDENCE_LABELS: Record<string, string> = {
  exact: '精确',
  relative_unknown: '相对时间未定',
  unknown: '未识别',
}

/** 六类失败分类（与领域闭集逐字同源）；空串不查表。 */
const FAILURE_LABELS: Record<string, string> = {
  model_error: '模型错误',
  invalid_output: '非法输出',
  timeout: '超时',
  audit_unavailable: '审计不可用',
  extractor_unavailable: '装配缺陷',
  window_gone: '冻结窗口缺失',
}

/** 记忆条目六态（与领域闭集逐字同源）：学习记录里的候选状态同样只读展示。 */
const ENTRY_STATE_LABELS: Record<MemoryEntryState, string> = {
  candidate: '候选',
  active: '生效',
  conflicted: '冲突待裁决',
  superseded: '已被替代',
  corrected: '已被纠正',
  deleted: '已删除',
}

const EXTRACTION_STATE_LABELS: Record<LearningExtractionState, string> = {
  pending: '进行中',
  success: '成功',
  failed: '失败',
  skipped: '跳过',
  unknown: '结果未知',
}

const EXTRACTION_STATE_TONES: Record<LearningExtractionState, Tone> = {
  pending: 'warn',
  success: 'ok',
  failed: 'bad',
  skipped: 'muted',
  unknown: 'warn',
}

function labelOf(labels: Record<string, string>, value: string): string {
  if (value === '') {
    return '—'
  }
  return labels[value] ?? value
}

function isNotFound(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404
}

/** 逐字证据块：消息主键、角色、引文与消息时刻；引文原样展示，不截断。 */
function renderEvidence(entry: MemoryEntryView): VNode {
  const evidence = entry.candidate?.evidence ?? []
  if (evidence.length === 0) {
    return h('p', { class: 'empty' }, '没有可用证据')
  }
  return h(
    'ul',
    { class: 'memory-evidence' },
    evidence.map(item =>
      h('li', { key: String(item.message_id) }, [
        h('span', { class: 'cell-id', title: `消息 #${item.message_id}` }, `#${item.message_id}`),
        h(
          'span',
          { class: 'muted small' },
          ` ${ROLE_LABELS[item.role] ?? item.role} · ${formatTimestamp(item.message_at)}`,
        ),
        h('blockquote', { class: 'evidence-quote' }, item.quote),
      ]),
    ),
  )
}

/** Agent 选择器：选中态写进 `agent` 查询参数，不清空其它上下文。 */
function agentPicker(
  id: string,
  agents: readonly ProfileView[],
  current: string,
  onSelect: (value: string) => void,
): VNode {
  return h('label', { class: 'field', for: id }, [
    h('span', 'Agent'),
    h(
      'select',
      {
        id,
        value: current,
        onChange: (event: Event) => {
          onSelect((event.target as HTMLSelectElement).value)
        },
      },
      [
        h('option', { value: '' }, '选择 Agent'),
        ...agents.map(agent => h('option', { key: agent.name, value: agent.name }, agent.name)),
      ],
    ),
  ])
}

/**
 * 「待确认」页签：只列 `state='candidate'` 且 `source='auto_extract'` 的条目。
 * 显式提议候选不在这里——全局层留在记忆库，Agent 层留在对应 Agent 详情页。
 * The pending tab lists auto-extracted candidates only; explicit proposals stay where
 * they already lived, so the same candidate is never reviewable in two places.
 */
export const PendingCandidates = defineComponent({
  name: 'PendingCandidates',
  props: viewProps,
  setup(props) {
    const admin = new AdminApi(props.ctx.api)
    const profiles = ref<ProfileView[]>([])
    const entries = ref<MemoryEntryView[]>([])
    const loaded = ref(false)
    const loading = ref(false)
    const busy = ref(false)
    const error = ref('')
    const feedback = ref<Feedback | null>(null)
    const compare = ref<{ candidate: MemoryEntryView; target: MemoryEntryView } | null>(null)
    const pendingDeleteID = ref(0)

    const agent = computed(() => props.query.get('agent') ?? '')
    const anchor = computed(() => props.query.get('candidate') ?? '')
    /** 深链锚点指向的行已不在当前列表：显示「已不存在」，不回退成 404。 */
    const anchorMissing = computed(
      () =>
        loaded.value &&
        anchor.value !== '' &&
        entries.value.every(entry => String(entry.entry_id) !== anchor.value),
    )

    async function loadProfiles(): Promise<void> {
      try {
        profiles.value = await props.ctx.api.listProfiles()
      } catch {
        // Agent 名单只影响选择器；读取失败不挂在数据区，候选读取有自己的错误位。
        profiles.value = []
      }
    }

    async function load(): Promise<void> {
      if (agent.value === '') {
        entries.value = []
        loaded.value = false
        return
      }
      loading.value = true
      try {
        const view = await admin.listMemoryEntries(agent.value, 'candidate')
        entries.value = view.entries.filter(entry => entry.source === 'auto_extract')
        error.value = ''
        loaded.value = true
      } catch (err) {
        error.value = errorMessage(err)
      } finally {
        loading.value = false
      }
    }

    /** 写动作共用：互斥、先清提示、成功后整表重读（不做乐观更新）。 */
    async function submit(action: () => Promise<Feedback>): Promise<void> {
      if (busy.value) {
        return
      }
      busy.value = true
      error.value = ''
      feedback.value = null
      try {
        feedback.value = await action()
        await load()
      } catch (err) {
        error.value = errorMessage(err)
      } finally {
        busy.value = false
      }
    }

    function selectAgent(value: string): void {
      props.ctx.router.navigate(memoryHref('pending', { agent: value }))
    }

    function confirmKeepBoth(entry: MemoryEntryView): void {
      void submit(async () => {
        const receipt = await admin.confirmMemory(entry.entry_id, { resolution: 'keep_both' })
        return {
          tone: 'info',
          text: `候选 #${receipt.entry_id} 已确认生效（保留双方），对该 Agent 的所有调用方可见。`,
        }
      })
    }

    /**
     * 替代前先读生效条目做新旧正文对照；建议目标已不在生效态时不进入对照，也不提交。
     * The supersede path reads the live target first; a stale suggestion is reported
     * instead of submitting a request the server would reject.
     */
    function askSupersede(entry: MemoryEntryView): void {
      const suggested = entry.candidate?.suggested_supersedes_id ?? 0
      if (suggested === 0) {
        return
      }
      void (async () => {
        if (busy.value) {
          return
        }
        busy.value = true
        error.value = ''
        feedback.value = null
        try {
          const view = await admin.listMemoryEntries(agent.value, 'active')
          const target = view.entries.find(item => item.entry_id === suggested)
          if (target === undefined) {
            feedback.value = {
              tone: 'warn',
              text:
                `建议替代目标 #${suggested} 已不在生效态（可能已被替代、纠正、删除或到期），` +
                '本次不能替代；请改为保留双方或删除该候选。',
            }
            return
          }
          compare.value = { candidate: entry, target }
        } catch (err) {
          error.value = errorMessage(err)
        } finally {
          busy.value = false
        }
      })()
    }

    function cancelSupersede(): void {
      compare.value = null
    }

    function confirmSupersede(): void {
      const pair = compare.value
      if (pair === null) {
        return
      }
      void submit(async () => {
        const receipt = await admin.confirmMemory(pair.candidate.entry_id, {
          resolution: 'supersede',
          supersedes_entry_id: pair.target.entry_id,
        })
        compare.value = null
        return {
          tone: 'info',
          text:
            `候选 #${receipt.entry_id} 已生效，旧条目 #${receipt.superseded_id} 已替代；` +
            '对该 Agent 的所有调用方可见。',
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
          text: `候选 #${receipt.entry_id} 已删除，后续运行不会召回这条内容。`,
        }
      })
    }

    onMounted(() => {
      void loadProfiles()
      void load()
    })
    watch(agent, () => {
      compare.value = null
      pendingDeleteID.value = 0
      feedback.value = null
      void load()
    })

    function renderCandidateCell(entry: MemoryEntryView): VNode {
      const candidate = entry.candidate
      const meta: VNodeChild[] = [
        h('span', { class: 'cell-id', title: `条目 #${entry.entry_id}` }, `#${entry.entry_id}`),
        h('span', { class: 'memory-entry-content', title: entry.content }, entry.content),
      ]
      if (candidate !== null) {
        meta.push(
          h(
            'span',
            { class: 'muted small' },
            `类型 ${labelOf(KIND_LABELS, candidate.kind)} · 确定性 ${labelOf(CERTAINTY_LABELS, candidate.certainty)} · ` +
              `事实时间 ${labelOf(TIME_CONFIDENCE_LABELS, candidate.time_confidence)}`,
          ),
        )
      }
      if (String(entry.entry_id) === anchor.value) {
        meta.push(h(StatusBadge, { label: '当前定位', tone: 'muted' }))
      }
      return h('div', { class: 'memory-entry-body' }, meta)
    }

    function renderActions(entry: MemoryEntryView): VNode {
      const suggested = entry.candidate?.suggested_supersedes_id ?? 0
      return h('div', { class: 'memory-pending-actions' }, [
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: busy.value,
              onClick: () => confirmKeepBoth(entry),
            },
            '保留双方',
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value || suggested === 0,
              title:
                suggested === 0
                  ? '没有建议替代目标，不能替代'
                  : '替代前会先展示新旧正文对照',
              onClick: () => askSupersede(entry),
            },
            '替代',
          ),
          h(
            'button',
            { type: 'button', disabled: busy.value, onClick: () => askDelete(entry) },
            '删除',
          ),
        ]),
        // 固定提示（卡片 §3.1.10）：确认的影响面必须写在按钮旁，不靠用户猜。
        h('p', { class: 'muted small memory-scope-hint' }, '确认后对该 Agent 的所有调用方可见。'),
      ])
    }

    function renderTable(): VNode {
      if (agent.value === '') {
        return h('p', { class: 'empty' }, '先在上方选择一个 Agent；待确认候选按 Agent 分层。')
      }
      const rows = entries.value.map(entry => {
        const candidate = entry.candidate
        return {
          key: String(entry.entry_id),
          cells: [
            renderCandidateCell(entry),
            renderEvidence(entry),
            candidate === null
              ? '—'
              : h(
                  'a',
                  {
                    href: memoryHref('records', {
                      agent: agent.value,
                      extraction: candidate.extraction_id,
                    }),
                    title: candidate.extraction_id,
                  },
                  candidate.extraction_id,
                ),
            formatTimestamp(candidate?.valid_to),
            (candidate?.suggested_supersedes_id ?? 0) === 0
              ? '—'
              : `#${candidate?.suggested_supersedes_id ?? 0}`,
            renderActions(entry),
          ],
        }
      })
      return h(DataTable, {
        tableClass: 'memory-pending-table',
        columnClasses: [
          'col-memory-pending-content',
          'col-memory-pending-evidence',
          'col-memory-pending-extraction',
          'col-memory-pending-validity',
          'col-memory-pending-suggestion',
          'col-memory-pending-actions',
        ],
        headers: ['候选', '逐字证据', '来源抽取', '有效期', '建议替代', '操作'],
        rows,
        empty: '这个 Agent 还没有待确认的自动抽取候选。',
      })
    }

    function renderCompare(): VNodeChild {
      const pair = compare.value
      if (pair === null) {
        return null
      }
      return h(
        'section',
        { class: 'card memory-supersede-compare', role: 'dialog', 'aria-label': '替代前对照' },
        [
          h('h2', '替代前对照'),
          h(
            'p',
            { class: 'muted small' },
            '确认后新候选生效、旧条目退役；来源失效、目标变化或与建议不一致时服务端会拒绝（409），' +
              '页面不做乐观更新。',
          ),
          h('div', { class: 'memory-compare' }, [
            h('div', { class: 'memory-compare-side' }, [
              h('h3', `旧条目 #${pair.target.entry_id}（生效中）`),
              h('p', { class: 'memory-entry-content' }, pair.target.content),
            ]),
            h('div', { class: 'memory-compare-side' }, [
              h('h3', `新候选 #${pair.candidate.entry_id}`),
              h('p', { class: 'memory-entry-content' }, pair.candidate.content),
            ]),
          ]),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              { type: 'button', class: 'primary', disabled: busy.value, onClick: confirmSupersede },
              '确认替代',
            ),
            h('button', { type: 'button', disabled: busy.value, onClick: cancelSupersede }, '取消'),
          ]),
        ],
      )
    }

    return (): VNode => {
      const panels: VNodeChild[] = [
        h(
          'p',
          { class: 'muted small' },
          '这里是模型自动抽取的记忆候选：确认之前不会参与任何召回或注入。显式提议的候选仍在' +
            '「记忆库」（全局层）或对应 Agent 详情页处理，同一候选不会在两处重复出现。',
        ),
        agentPicker('memory-pending-agent', profiles.value, agent.value, selectAgent),
        error.value !== '' ? h(Notice, { tone: 'error', text: error.value }) : null,
        feedback.value === null
          ? null
          : h(Notice, {
              tone: feedback.value.tone === 'warn' ? 'warn' : 'info',
              text: feedback.value.text,
            }),
        anchorMissing.value
          ? h(Notice, {
              tone: 'info',
              text: `候选 #${anchor.value} 已不存在（可能已确认、删除或过期），当前显示该 Agent 的全部待确认候选。`,
            })
          : null,
        loading.value ? h('p', { class: 'loading' }, '正在读取候选…') : null,
        renderCompare(),
        renderTable(),
        h(ConfirmDialog, {
          open: pendingDeleteID.value !== 0,
          title: '删除记忆候选',
          message: (() => {
            const target = entries.value.find(entry => entry.entry_id === pendingDeleteID.value)
            if (target === undefined) {
              return '目标候选已不在当前列表。'
            }
            const text = target.content.trim()
            const preview = text.length > 60 ? `${text.slice(0, 60)}…` : text
            return (
              `将软删候选 #${target.entry_id}：${preview}。行与审计保留，删除后立即不可召回；重复删除幂等。`
            )
          })(),
          confirmText: '确认删除',
          busy: busy.value,
          onConfirm: confirmDelete,
          onCancel: cancelDelete,
        }),
      ]
      return h('div', { class: 'memory-pending' }, panels)
    }
  },
})

/**
 * 「学习记录」页签：只读展示启用状态、预算、抽取列表、失败分类与关联方法草案。
 * 抽取详情 → 候选（待确认页签）与草案；草案详情 → 来源抽取。双向链接都保留 `agent`。
 * The read-only records tab; every link keeps the agent context and no write control
 * exists on this surface.
 */
export const LearningRecords = defineComponent({
  name: 'LearningRecords',
  props: viewProps,
  setup(props) {
    const admin = new AdminApi(props.ctx.api)
    const profiles = ref<ProfileView[]>([])
    const status = ref<LearningStatusView | null>(null)
    const extractions = ref<LearningExtractionView[]>([])
    const drafts = ref<LearningDraftMetaView[]>([])
    const extractionDetail = ref<LearningExtractionDetailView | null>(null)
    const draftDetail = ref<LearningDraftDetailView | null>(null)
    const loading = ref(false)
    const error = ref('')
    const missing = ref('')

    const agent = computed(() => props.query.get('agent') ?? '')
    const extractionAnchor = computed(() => props.query.get('extraction') ?? '')
    const draftAnchor = computed(() => props.query.get('draft') ?? '')
    let generation = 0

    async function loadProfiles(): Promise<void> {
      try {
        profiles.value = await props.ctx.api.listProfiles()
      } catch {
        profiles.value = []
      }
    }

    async function loadExtractionDetail(ticket: number): Promise<void> {
      extractionDetail.value = null
      const id = extractionAnchor.value
      if (id === '') {
        return
      }
      try {
        const detail = await admin.getLearningExtraction(id)
        if (ticket === generation) {
          extractionDetail.value = detail
        }
      } catch (err) {
        if (ticket !== generation) {
          return
        }
        if (isNotFound(err)) {
          missing.value = `抽取记录 ${id} 已不存在，以下为当前列表。`
          return
        }
        error.value = errorMessage(err)
      }
    }

    async function loadDraftDetail(ticket: number): Promise<void> {
      draftDetail.value = null
      const id = draftAnchor.value
      if (id === '') {
        return
      }
      try {
        const detail = await admin.getLearningDraft(id)
        if (ticket === generation) {
          draftDetail.value = detail
        }
      } catch (err) {
        if (ticket !== generation) {
          return
        }
        if (isNotFound(err)) {
          missing.value = `方法草案 ${id} 已不存在，以下为当前列表。`
          return
        }
        error.value = errorMessage(err)
      }
    }

    async function load(): Promise<void> {
      const ticket = ++generation
      loading.value = true
      error.value = ''
      missing.value = ''
      try {
        const [statusView, extractionList, draftList] = await Promise.all([
          admin.getLearningStatus(),
          admin.listLearningExtractions({ agent: agent.value }),
          admin.listLearningDrafts({ agent: agent.value }),
        ])
        if (ticket !== generation) {
          return
        }
        status.value = statusView
        extractions.value = extractionList.extractions
        drafts.value = draftList.drafts
      } catch (err) {
        if (ticket === generation) {
          error.value = errorMessage(err)
        }
      } finally {
        if (ticket === generation) {
          loading.value = false
        }
      }
      if (ticket !== generation) {
        return
      }
      await Promise.all([loadExtractionDetail(ticket), loadDraftDetail(ticket)])
    }

    onMounted(() => {
      void loadProfiles()
      void load()
    })
    watch([agent, extractionAnchor, draftAnchor], () => {
      void load()
    })

    function selectAgent(value: string): void {
      props.ctx.router.navigate(memoryHref('records', { agent: value }))
    }

    function renderStatus(): VNode {
      const view = status.value
      if (view === null) {
        return h('p', { class: 'muted' }, loading.value ? '正在读取启用状态…' : '启用状态不可用。')
      }
      return h('div', {}, [
        view.enabled
          ? null
          : h(Notice, {
              tone: 'info',
              text:
                '自动抽取当前关闭（learning.auto_extract=false，出厂默认）；启用只改 basalt.yaml ' +
                '并重启，管理台不提供开关。',
            }),
        definitions([
          ['启用状态', h(StatusBadge, { label: view.enabled ? '已启用' : '未启用', tone: view.enabled ? 'ok' : 'muted' })],
          ['Agent 名单', view.agents.length === 0 ? '—' : view.agents.join('、')],
          [
            '名单内缺失的 Agent',
            view.unknown_agents.length === 0
              ? '—'
              : `${view.unknown_agents.join('、')}（记 WARN，不抽取，不阻止启动）`,
          ],
          ['规则 / 提示词版本', `${view.rule_version} / ${view.prompt_version}`],
          [
            '滚动 24 小时尝试',
            `${view.attempts_used} / ${view.attempt_limit}${view.exhausted ? '（已耗尽，不建行不调用）' : ''}`,
          ],
          ['预算窗口', `${view.budget_window_seconds} 秒`],
          [
            '待决计数',
            `进行中 ${view.pending_extractions} · 结果未知 ${view.unknown_extractions} · 无来源草案 ${view.orphan_drafts}`,
          ],
          ['最近抽取', formatTimestamp(view.last_extraction_at)],
        ]),
        h(
          'p',
          { class: 'muted small' },
          '本页只读：没有启用开关、手动抽取、重试、发布或批准按钮；学习查询接口保持只读。',
        ),
      ])
    }

    function extractionHrefOf(row: LearningExtractionView): string {
      return memoryHref('records', {
        agent: agent.value === '' ? row.agent : agent.value,
        extraction: row.extraction_id,
      })
    }

    function renderExtractionTable(): VNode {
      const rows = extractions.value.map(row => ({
        key: row.extraction_id,
        cells: [
          h('a', { href: extractionHrefOf(row), class: 'cell-id', title: row.extraction_id }, row.extraction_id),
          row.agent,
          h(StatusBadge, {
            label: EXTRACTION_STATE_LABELS[row.status],
            tone: EXTRACTION_STATE_TONES[row.status],
          }),
          String(row.attempts),
          `${row.window_messages} 条${row.dropped_window_messages > 0 ? `（丢弃 ${row.dropped_window_messages}）` : ''}`,
          `接受 ${row.items_accepted} / 共 ${row.items_total}${row.items_dropped > 0 ? `（丢弃 ${row.items_dropped}）` : ''}`,
          row.failure_class === ''
            ? '—'
            : `${labelOf(FAILURE_LABELS, row.failure_class)}${row.error_message === '' ? '' : `：${row.error_message}`}`,
          h('a', { href: extractionHrefOf(row) }, '查看详情'),
        ],
      }))
      return h(DataTable, {
        tableClass: 'learning-extractions-table',
        headers: ['抽取', 'Agent', '状态', '尝试', '窗口', '结果', '失败分类', '操作'],
        rows,
        empty: '没有符合当前过滤条件的抽取记录。',
      })
    }

    function renderExtractionDetail(): VNodeChild {
      const detail = extractionDetail.value
      if (detail === null) {
        return null
      }
      const row = detail.extraction
      return h('section', { class: 'card learning-extraction-detail' }, [
        h('h2', `抽取详情 ${row.extraction_id}`),
        definitions([
          ['状态', EXTRACTION_STATE_LABELS[row.status]],
          ['Agent', row.agent],
          ['会话', h('span', { class: 'cell-id', title: row.session_id }, row.session_id)],
          ['触发 Run', h('span', { class: 'cell-id', title: row.run_id }, row.run_id)],
          ['最近尝试 Run', row.last_attempt_run_id === '' ? '—' : row.last_attempt_run_id],
          ['来源', `${row.origin_caller_id || '—'}${row.origin_key_id === '' ? '' : ` / Key ${row.origin_key_id}`}`],
          [
            '冻结窗口',
            `消息 #${row.window_from_message_id}–#${row.window_to_message_id}（${row.window_messages} 条` +
              `${row.dropped_window_messages > 0 ? `，丢弃 ${row.dropped_window_messages}` : ''}；前文 ${row.prior_messages} 条）`,
          ],
          ['尝试次数', String(row.attempts)],
          [
            '结果',
            `接受 ${row.items_accepted} / 共 ${row.items_total}${row.items_dropped > 0 ? `（丢弃 ${row.items_dropped}）` : ''}`,
          ],
          ['模型', `${row.provider} / ${row.model}`],
          ['版本', `${row.rule_version} / ${row.prompt_version}`],
          ['失败分类', labelOf(FAILURE_LABELS, row.failure_class)],
          ['错误摘要', row.error_message === '' ? '—' : row.error_message],
          ['创建', formatTimestamp(row.created_at)],
          ['最近尝试', formatTimestamp(row.last_attempt_at)],
          ['完成', formatTimestamp(row.finished_at)],
          ['查证时间', formatTimestamp(row.verified_at)],
        ]),
        h('h3', '候选'),
        h(DataTable, {
          tableClass: 'learning-candidates-table',
          headers: ['候选', '状态', '类型', '正文', '证据', '有效期', '建议替代'],
          rows: detail.candidates.map(candidate => ({
            key: String(candidate.entry_id),
            cells: [
              h(
                'a',
                {
                  href: memoryHref('pending', {
                    agent: agent.value === '' ? row.agent : agent.value,
                    candidate: String(candidate.entry_id),
                  }),
                  class: 'cell-id',
                },
                `#${candidate.entry_id}`,
              ),
              labelOf(ENTRY_STATE_LABELS, candidate.state),
              labelOf(KIND_LABELS, candidate.kind),
              candidate.content,
              String(candidate.evidence.length),
              formatTimestamp(candidate.valid_to),
              candidate.suggested_supersedes_id === 0 ? '—' : `#${candidate.suggested_supersedes_id}`,
            ],
          })),
          empty: '这次抽取没有留下候选（模型输出被丢弃或整次失败）。',
        }),
        h('h3', '关联方法草案'),
        detail.drafts.length === 0
          ? h('p', { class: 'empty' }, '这次抽取没有产生方法草案。')
          : h(
              'ul',
              { class: 'learning-draft-links' },
              detail.drafts.map(draft =>
                h('li', { key: draft.draft_id }, [
                  h(
                    'a',
                    {
                      href: memoryHref('records', {
                        agent: agent.value === '' ? row.agent : agent.value,
                        draft: draft.draft_id,
                      }),
                    },
                    `${draft.name}（${draft.draft_id}）`,
                  ),
                  h('span', { class: 'muted small' }, ` ${draft.rel_dir}`),
                ]),
              ),
            ),
        h('p', {}, [h('a', { href: memoryHref('records', { agent: agent.value }) }, '收起详情')]),
      ])
    }

    function renderDraftsTable(): VNode {
      const rows = drafts.value.map(draft => ({
        key: draft.draft_id,
        cells: [
          h(
            'a',
            {
              href: memoryHref('records', {
                agent: agent.value === '' ? draft.agent : agent.value,
                draft: draft.draft_id,
              }),
              class: 'cell-id',
              title: draft.draft_id,
            },
            draft.draft_id,
          ),
          draft.agent,
          draft.name,
          h('a', { href: memoryHref('records', { agent: draft.agent, extraction: draft.extraction_id }) }, draft.extraction_id),
          h('code', { class: 'cell-id', title: draft.content_sha256 }, shortHash(draft.content_sha256)),
          formatTimestamp(draft.created_at),
        ],
      }))
      return h(DataTable, {
        tableClass: 'learning-drafts-table',
        headers: ['草案', 'Agent', '名称', '来源抽取', '内容哈希', '写入时间'],
        rows,
        empty: '还没有方法草案。草案只由方法类候选写入固定目录，只读且不执行。',
      })
    }

    function renderDraftDetail(): VNodeChild {
      const detail = draftDetail.value
      if (detail === null) {
        return null
      }
      return h('section', { class: 'card learning-draft-detail' }, [
        h('h2', `方法草案 ${detail.draft_id}`),
        definitions([
          ['Agent', detail.agent],
          [
            '来源抽取',
            h(
              'a',
              {
                href: memoryHref('records', {
                  agent: agent.value === '' ? detail.agent : agent.value,
                  extraction: detail.extraction_id,
                }),
              },
              detail.extraction_id,
            ),
          ],
          ['名称', detail.name],
          ['目录', h('span', { class: 'cell-id' }, detail.rel_dir)],
          ['内容哈希', h('span', { class: 'cell-id', title: detail.content_sha256 }, detail.content_sha256)],
          ['写入时间', formatTimestamp(detail.created_at)],
        ]),
        h('p', { class: 'muted small' }, '草案是只读文件（DRAFT.md + evidence.json），不进 Agent 读写白名单、不执行脚本；评估、批准与发布仍归技能准入。'),
        h('h3', 'DRAFT.md'),
        h('pre', { class: 'code-block' }, [h('code', detail.content)]),
        h('h3', '证据'),
        h(DataTable, {
          tableClass: 'learning-draft-evidence',
          headers: ['消息', 'Run', '角色', '引文', '消息时刻'],
          rows: detail.evidence.map(item => ({
            key: String(item.message_id),
            cells: [
              h('span', { class: 'cell-id' }, `#${item.message_id}`),
              h('span', { class: 'cell-id', title: item.run_id }, item.run_id),
              ROLE_LABELS[item.role] ?? item.role,
              h('blockquote', { class: 'evidence-quote' }, item.quote),
              formatTimestamp(item.message_at),
            ],
          })),
          empty: '没有可用证据。',
        }),
        h('p', {}, [h('a', { href: memoryHref('records', { agent: agent.value }) }, '收起草案')]),
      ])
    }

    return (): VNode => {
      const panels: VNodeChild[] = [
        h(
          'p',
          { class: 'muted small' },
          '自动抽取的只读记录：启用状态、滚动预算、抽取账本与关联方法草案。启用只改 basalt.yaml；' +
            '候选的确认与删除在「待确认」页签，方法草案的评估与批准在「学习准入」。',
        ),
        agentPicker('memory-records-agent', profiles.value, agent.value, selectAgent),
        error.value !== '' ? h(Notice, { tone: 'error', text: error.value }) : null,
        missing.value !== '' ? h(Notice, { tone: 'info', text: missing.value }) : null,
        loading.value ? h('p', { class: 'loading' }, '正在读取学习记录…') : null,
        h('section', { class: 'card learning-status' }, [h('h2', '启用状态'), renderStatus()]),
        h('section', { class: 'card' }, [h('h2', '抽取记录'), renderExtractionTable()]),
        renderExtractionDetail(),
        h('section', { class: 'card' }, [h('h2', '方法草案'), renderDraftsTable()]),
        renderDraftDetail(),
      ]
      return h('div', { class: 'learning-records' }, panels)
    }
  },
})
