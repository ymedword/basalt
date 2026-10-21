// Agent 详情视图：会话 / 运行 / 配置 / 能力 / 记忆 / 定时 六页签（图 H）。
// 配置页签承载 AGENT.md 的 ETag 条件写；有未保存修改时装卸离开守卫。
// Agent detail with the six tabs from figure H. The config tab performs the
// conditional AGENT.md write and installs the unsaved-edits leave guard.
import { computed, defineComponent, h, onMounted, onUnmounted, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type { MemoryView, MessageView, ProfileView, SessionDetail } from '../api'
import {
  CodeBlock,
  ConfirmDialog,
  DataTable,
  Notice,
  formatTimestamp,
} from '../components/ui'
import { renderSafeText } from '../render'
import { runHref } from '../router'
import {
  AGENT_STATE_LABELS,
  RUN_STATUS_LABELS,
  badge,
  definitions,
  identifier,
  lifecycleTone,
  pageHeading,
  runStatusTone,
  section,
  useLoader,
  viewProps,
} from './shared'

type TabKey = 'sessions' | 'runs' | 'config' | 'capabilities' | 'memory' | 'schedules'

const TABS: readonly { readonly key: TabKey; readonly label: string }[] = [
  { key: 'sessions', label: '会话' },
  { key: 'runs', label: '运行' },
  { key: 'config', label: '配置' },
  { key: 'capabilities', label: '能力' },
  { key: 'memory', label: '记忆' },
  { key: 'schedules', label: '定时' },
]

const ROLE_LABELS = {
  system: '系统',
  user: '用户',
  assistant: '助手',
  tool: '工具',
} as const

function roleTone(role: MessageView['role']): 'ok' | 'warn' | 'muted' {
  if (role === 'assistant') {
    return 'ok'
  }
  if (role === 'tool') {
    return 'warn'
  }
  return 'muted'
}

function messageView(message: MessageView): VNode {
  const toolCalls = message.tool_calls ?? []
  return h('article', { class: 'message' }, [
    h('header', { class: 'message-head' }, [
      badge(ROLE_LABELS[message.role], roleTone(message.role)),
      message.created_at === undefined
        ? null
        : h('span', { class: 'muted small' }, formatTimestamp(message.created_at)),
    ]),
    message.content === ''
      ? null
      : h('div', { class: 'message-body' }, renderSafeText(message.content)),
    toolCalls.length === 0
      ? null
      : h(
          'ul',
          { class: 'tool-calls' },
          toolCalls.map((call) =>
            h('li', { key: call.id }, [h('strong', call.name), ' ', h('code', call.arguments)]),
          ),
        ),
  ])
}

export const AgentDetailView = defineComponent({
  name: 'AgentDetailView',
  props: viewProps,
  setup(props) {
    const name = props.params.name ?? ''
    const api = props.ctx.api
    const router = props.ctx.router

    const profiles = useLoader(() => api.listProfiles())
    const profile = computed<ProfileView | null>(
      () => profiles.data.value?.find((item) => item.name === name) ?? null,
    )

    const tab = ref<TabKey>('sessions')

    // --- 页签数据 ---------------------------------------------------------
    const sessions = useLoader(() => api.listSessions({ agent: name }))
    const runs = useLoader(() => api.listRuns({ agent: name }))
    const schedules = useLoader(async () =>
      (await api.listSchedules()).filter((item) => item.agent === name),
    )
    const memory = useLoader(() => api.getAgentMemory(name), false)

    const selectedSession = ref<SessionDetail | null>(null)
    const sessionLoading = ref(false)
    const sessionError = ref('')

    async function openSession(sessionId: string): Promise<void> {
      sessionLoading.value = true
      sessionError.value = ''
      try {
        selectedSession.value = await api.getSession(sessionId)
      } catch (caught) {
        sessionError.value = errorMessage(caught)
        selectedSession.value = null
      } finally {
        sessionLoading.value = false
      }
    }

    // --- 配置页签：ETag 条件写 -------------------------------------------
    const content = ref('')
    const editor = ref('')
    const etag = ref('')
    const contentLoaded = ref(false)
    const contentLoading = ref(false)
    const saving = ref(false)
    const saveError = ref('')
    const saveNotice = ref('')
    const dirty = computed(() => contentLoaded.value && editor.value !== content.value)

    async function loadContent(): Promise<void> {
      contentLoading.value = true
      saveError.value = ''
      try {
        const view = await api.getAgentContent(name)
        content.value = view.content
        editor.value = view.content
        etag.value = view.etag
        contentLoaded.value = true
      } catch (caught) {
        saveError.value = errorMessage(caught)
      } finally {
        contentLoading.value = false
      }
    }

    async function saveContent(): Promise<void> {
      if (!dirty.value || saving.value) {
        return
      }
      saving.value = true
      saveError.value = ''
      saveNotice.value = ''
      try {
        const updated = await api.updateAgentContent(name, { content: editor.value }, etag.value)
        content.value = editor.value
        etag.value = updated.etag
        saveNotice.value = '已保存，ETag 已更新'
        await profiles.reload()
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 412) {
          saveError.value = '文件已被他人修改，本次保存没有写入磁盘；请重新加载后再试'
        } else {
          saveError.value = errorMessage(caught)
        }
      } finally {
        saving.value = false
      }
    }

    async function reloadContent(): Promise<void> {
      if (dirty.value && !window.confirm('放弃未保存的修改并重新加载？')) {
        return
      }
      await loadContent()
    }

    function discardEdits(): void {
      editor.value = content.value
      saveError.value = ''
      saveNotice.value = ''
    }

    // --- 生命周期迁移 -----------------------------------------------------
    const transition = ref<'disable' | 'enable' | null>(null)
    const transitionBusy = ref(false)
    const transitionError = ref('')

    const lifecycle = computed(() => profile.value?.lifecycle ?? null)

    function askTransition(): void {
      const state = lifecycle.value?.state ?? null
      if (state === 'active') {
        transition.value = 'disable'
      } else if (state === 'disabled') {
        transition.value = 'enable'
      }
    }

    async function confirmTransition(): Promise<void> {
      const current = lifecycle.value
      const action = transition.value
      if (current === null || action === null) {
        return
      }
      if (current.state !== 'active' && current.state !== 'disabled') {
        transition.value = null
        return
      }
      transitionBusy.value = true
      transitionError.value = ''
      const body = {
        expected_state: current.state,
        expected_generation: current.generation,
      }
      try {
        if (action === 'disable') {
          await api.disableAgent(name, body)
        } else {
          await api.enableAgent(name, body)
        }
        transition.value = null
        await profiles.reload()
      } catch (caught) {
        transitionError.value = errorMessage(caught)
      } finally {
        transitionBusy.value = false
      }
    }

    // --- 页签切换与未保存守卫 --------------------------------------------
    function selectTab(key: TabKey): void {
      tab.value = key
      if (key === 'config' && !contentLoaded.value && !contentLoading.value) {
        void loadContent()
      }
      if (key === 'memory' && memory.data.value === null && !memory.loading.value) {
        void memory.reload()
      }
    }

    watch(
      dirty,
      (value) => {
        router.setLeaveGuard(
          value ? () => window.confirm('配置有未保存的修改，确定离开？') : null,
        )
      },
      { immediate: true },
    )

    const beforeUnload = (event: BeforeUnloadEvent): void => {
      if (!dirty.value) {
        return
      }
      event.preventDefault()
      event.returnValue = ''
    }

    onMounted(() => {
      window.addEventListener('beforeunload', beforeUnload)
    })
    onUnmounted(() => {
      window.removeEventListener('beforeunload', beforeUnload)
      router.setLeaveGuard(null)
    })

    // --- 渲染 -------------------------------------------------------------
    function renderSessions(): VNodeChild {
      const content: VNodeChild[] = []
      if (sessions.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: sessions.error.value }))
      }
      const data = sessions.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            headers: ['会话', '渠道', '用户', '状态', '消息数', '最近活跃', ''],
            tableClass: 'agent-sessions-table',
            columnClasses: ['col-id', 'col-short', 'col-name', 'col-state', 'col-number', 'col-time', 'col-action'],
            rows: data.map((item) => ({
              key: item.session_id,
              cells: [
                identifier(item.session_id),
                item.channel,
                item.user_id,
                item.status === 'active' ? '活跃' : '已归档',
                String(item.message_count),
                formatTimestamp(item.last_active_at),
                h(
                  'button',
                  {
                    type: 'button',
                    onClick: () => {
                      void openSession(item.session_id)
                    },
                  },
                  '查看',
                ),
              ],
            })),
            empty: '该 Agent 还没有会话',
          }),
        )
      }
      if (sessionError.value !== '') {
        content.push(h(Notice, { tone: 'error', text: sessionError.value }))
      }
      if (sessionLoading.value) {
        content.push(h('p', { class: 'muted' }, '会话加载中…'))
      }
      if (selectedSession.value !== null) {
        content.push(
          h('section', { class: 'card' }, [
            h('h2', '会话详情'),
            h('p', { class: 'detail-identity' }, [h('code', { class: 'detail-id', title: selectedSession.value.session_id }, selectedSession.value.session_id)]),
            definitions([
              ['渠道', selectedSession.value.channel],
              ['用户', selectedSession.value.user_id],
              ['消息数', String(selectedSession.value.message_count)],
              ['最近活跃', formatTimestamp(selectedSession.value.last_active_at)],
            ]),
            h(
              'div',
              { class: 'transcript' },
              selectedSession.value.messages.map((message, index) =>
                h('div', { key: `${selectedSession.value?.session_id ?? ''}-${index}` }, [
                  messageView(message),
                ]),
              ),
            ),
          ]),
        )
      }
      return h('div', { class: 'tab-panel' }, content)
    }

    function renderRuns(): VNodeChild {
      const content: VNodeChild[] = []
      if (runs.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: runs.error.value }))
      }
      const data = runs.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            headers: ['运行', '状态', '触发', '迭代', '工具调用', '开始时间'],
            tableClass: 'agent-runs-table',
            columnClasses: ['col-id', 'col-state', 'col-short', 'col-number', 'col-number', 'col-time'],
            rows: data.map((run) => ({
              key: run.run_id,
              cells: [
                identifier(run.run_id, runHref(run.run_id)),
                badge(RUN_STATUS_LABELS[run.status], runStatusTone(run.status)),
                run.trigger,
                String(run.iterations),
                String(run.tool_calls),
                formatTimestamp(run.started_at),
              ],
            })),
            empty: '该 Agent 还没有运行记录',
          }),
        )
      }
      return h('div', { class: 'tab-panel' }, content)
    }

    function renderConfig(): VNodeChild {
      const content: VNodeChild[] = []
      if (contentLoading.value) {
        content.push(h('p', { class: 'muted' }, '配置加载中…'))
      }
      if (saveError.value !== '') {
        content.push(h(Notice, { tone: 'error', text: saveError.value }))
      }
      if (saveNotice.value !== '') {
        content.push(h(Notice, { tone: 'info', text: saveNotice.value }))
      }
      if (contentLoaded.value) {
        content.push(
          h('p', { class: 'muted small' }, [
            'ETag：',
            h('code', etag.value),
            dirty.value ? '（有未保存的修改）' : '（与磁盘一致）',
          ]),
          h('textarea', {
            class: 'editor',
            spellcheck: false,
            rows: 24,
            value: editor.value,
            onInput: (event: Event) => {
              editor.value = (event.target as HTMLTextAreaElement).value
            },
          }),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                class: 'primary',
                disabled: !dirty.value || saving.value,
                onClick: () => {
                  void saveContent()
                },
              },
              saving.value ? '保存中…' : '保存',
            ),
            h(
              'button',
              {
                type: 'button',
                disabled: !dirty.value || saving.value,
                onClick: discardEdits,
              },
              '放弃修改',
            ),
            h(
              'button',
              {
                type: 'button',
                disabled: saving.value,
                onClick: () => {
                  void reloadContent()
                },
              },
              '重新加载',
            ),
          ]),
        )
      }
      return h('div', { class: 'tab-panel' }, content)
    }

    function renderCapabilities(): VNodeChild {
      const current = profile.value
      if (current === null) {
        return h('p', { class: 'muted' }, 'Agent 信息尚未加载')
      }
      return h('div', { class: 'tab-panel' }, [
        section('工具', [
          current.tools.length === 0
            ? h('p', { class: 'muted' }, '未绑定工具')
            : h(
                'ul',
                current.tools.map((tool) => h('li', { key: tool }, h('code', tool))),
              ),
        ]),
        section('技能', [
          current.skills.length === 0
            ? h('p', { class: 'muted' }, '未绑定技能')
            : h(
                'ul',
                current.skills.map((skill) => h('li', { key: skill }, h('code', skill))),
              ),
        ]),
        h(Notice, {
          tone: 'info',
          text: 'MCP 定义、沙箱白名单与 Provider 段的改动需要重启后生效；AGENT.md 与技能内容热加载只影响新运行。',
        }),
      ])
    }

    function renderMemoryView(view: MemoryView): VNodeChild {
      const parts: VNodeChild[] = []
      parts.push(section('核心记忆', [view.core === '' ? h('p', { class: 'muted' }, '（空）') : h(CodeBlock, { text: view.core })]))
      parts.push(section('归档记忆', [view.archive === '' ? h('p', { class: 'muted' }, '（空）') : h(CodeBlock, { text: view.archive })]))
      return h('div', { class: 'tab-panel' }, parts)
    }

    function renderMemory(): VNodeChild {
      const content: VNodeChild[] = []
      if (memory.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: memory.error.value }))
      }
      if (memory.loading.value) {
        content.push(h('p', { class: 'muted' }, '记忆加载中…'))
      }
      const view = memory.data.value
      if (view !== null) {
        content.push(renderMemoryView(view))
      }
      return h('div', { class: 'tab-panel' }, content)
    }

    function renderSchedules(): VNodeChild {
      const content: VNodeChild[] = []
      if (schedules.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: schedules.error.value }))
      }
      const data = schedules.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            headers: ['ID', 'Cron（6 段）', '时区', '启用', '上次运行', '上次状态', '消息'],
            rows: data.map((item) => ({
              key: item.id,
              cells: [
                h('code', item.id),
                h('code', item.cron),
                item.zone,
                item.enabled ? '是' : '否',
                formatTimestamp(item.last_run_at),
                item.last_status === '' ? '—' : item.last_status,
                item.message,
              ],
            })),
            empty: '该 Agent 没有定时任务',
          }),
        )
      }
      return h('div', { class: 'tab-panel' }, content)
    }

    function renderTab(): VNodeChild {
      switch (tab.value) {
        case 'sessions':
          return renderSessions()
        case 'runs':
          return renderRuns()
        case 'config':
          return renderConfig()
        case 'capabilities':
          return renderCapabilities()
        case 'memory':
          return renderMemory()
        case 'schedules':
          return renderSchedules()
      }
    }

    /** 标题行里的生命周期徽标与状态异常说明。 */
    function renderLifecycleBadge(): VNode | null {
      const current = lifecycle.value
      if (current === null) {
        return null
      }
      if (!current.valid || current.state === null) {
        return badge('状态无效', 'bad')
      }
      return badge(AGENT_STATE_LABELS[current.state], lifecycleTone(current.state))
    }

    /** 生命周期元信息片段：版本、更新时间与无效状态的错误原因（拼进元信息行）。 */
    function renderLifecycleMeta(): VNodeChild[] {
      const current = lifecycle.value
      if (current === null) {
        return []
      }
      if (!current.valid || current.state === null) {
        return [h('span', { class: 'sep' }, '·'), h('span', {}, current.lifecycle_error)]
      }
      return [
        h('span', { class: 'sep' }, '·'),
        h('span', {}, `版本 ${current.generation}`),
        h('span', { class: 'sep' }, '·'),
        h('span', {}, `更新于 ${formatTimestamp(current.updated_at)}`),
      ]
    }

    function renderTransitionButton(): VNode | null {
      const current = lifecycle.value
      if (current === null || !current.valid || current.state === null) {
        return null
      }
      if (current.state !== 'active' && current.state !== 'disabled') {
        return null
      }
      return h(
        'button',
        {
          type: 'button',
          disabled: transitionBusy.value,
          onClick: askTransition,
        },
        current.state === 'active' ? '停用 Agent' : '启用 Agent',
      )
    }

    return (): VNode => {
      const content: VNodeChild[] = []
      if (profiles.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: profiles.error.value }))
      }
      if (profiles.loading.value && profile.value === null) {
        content.push(h('p', { class: 'muted' }, 'Agent 信息加载中…'))
      }
      if (!profiles.loading.value && profile.value === null && profiles.error.value === '') {
        content.push(
          h(Notice, { tone: 'warn', text: `未找到 Agent「${name}」，它可能尚未注册或已被移除。` }),
          h('p', {}, [h('a', { href: '/console/agents' }, '← 返回 Agents 列表')]),
        )
      }

      const current = profile.value
      if (current !== null) {
        content.push(
          h('div', { class: 'agent-head' }, [
            h('p', { class: 'breadcrumb' }, [
              h('a', { href: '/console/agents' }, '← 返回 Agents 列表'),
            ]),
            pageHeading('WORKSPACE / AGENT', current.name, current.description || '查看这个 Agent 的会话、运行、配置与能力。', renderTransitionButton()),
            h('div', { class: 'agent-title' }, [renderLifecycleBadge()]),
            h('p', { class: 'agent-meta' }, [
              h('code', `${current.provider} / ${current.model}`),
              ...renderLifecycleMeta(),
            ]),
            current.problems.length === 0
              ? null
              : h(
                  'ul',
                  { class: 'problems' },
                  current.problems.map((problem) => h('li', { key: problem }, problem)),
                ),
            transitionError.value === ''
              ? null
              : h(Notice, { tone: 'error', text: transitionError.value }),
            h(
              'nav',
              { class: 'tabs' },
              TABS.map((item) =>
                h(
                  'button',
                  {
                    key: item.key,
                    type: 'button',
                    class: ['tab', tab.value === item.key ? 'active' : ''],
                    onClick: () => selectTab(item.key),
                  },
                  item.label,
                ),
              ),
            ),
            renderTab(),
          ]),
          h(ConfirmDialog, {
            open: transition.value !== null,
            title: transition.value === 'disable' ? '停用 Agent' : '启用 Agent',
            message:
              transition.value === 'disable'
                ? '停用后新运行会被拒绝，在途运行继续完成。继续？'
                : '启用后新运行恢复。继续？',
            confirmText: transition.value === 'disable' ? '停用' : '启用',
            busy: transitionBusy.value,
            onConfirm: () => {
              void confirmTransition()
            },
            onCancel: () => {
              transition.value = null
            },
          }),
        )
      }

      return h('section', { class: 'view view-agent-detail' }, content)
    }
  },
})
