// 会话详情深链视图（R7）：`/console/agents/:name/sessions/:id` 是可刷新、可收藏、
// 可回退的独立页面；消息区自动滚到最新，输入区继续对话，归档需要显式确认。
// 会话 ID 由服务端全局解析，页面再核对 profile_name，避免挂到错误 Agent 的深链下。
// Session detail as a real route: refresh, bookmark, history back, transcript scrolled
// to the latest message, continue-chat composer and a confirmed archive action. The
// API resolves a session id globally, so the page cross-checks `profile_name`.
import { computed, defineComponent, h, nextTick, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type { MessageView, ProfileView, SessionDetail } from '../api'
import { ConfirmDialog, Notice, formatTimestamp } from '../components/ui'
import { renderSafeText } from '../render'
import { agentConfigHref, agentHref, runHref, sessionHref } from '../router'
import {
  badge,
  definitions,
  identifier,
  pageHeading,
  section,
  sendResultNotice,
  useLoader,
  viewProps,
} from './shared'

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

/** 单条消息：角色徽标 + 时间、正文（纯文本安全渲染）与工具调用参数。 */
// One transcript message: role badge, timestamp, safe text body and tool calls.
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

export const SessionDetailView = defineComponent({
  name: 'SessionDetailView',
  props: viewProps,
  setup(props) {
    const agentName = props.params.name ?? ''
    const sessionId = props.params.id ?? ''
    const api = props.ctx.api

    const session = useLoader(() => api.getSession(sessionId))
    const profiles = useLoader(() => api.listProfiles(), false)
    const profile = computed<ProfileView | null>(
      () => profiles.data.value?.find((item) => item.name === agentName) ?? null,
    )

    // 深链属于别的 Agent 时先提示，不渲染输入区，避免在错误上下文里发消息。
    // A session owned by another agent is reported instead of shown as writable.
    const wrongAgent = computed(() => {
      const detail = session.data.value
      return detail !== null && detail.profile_name !== agentName
    })

    // 首次进入直接读一次 Agent 信息；发送前再确保可用（不可运行时禁用发送）。
    // Read the agent once on entry; the composer gate re-checks it before sending.
    void profiles.reload()

    const chatInput = ref('')
    const chatBusy = ref(false)
    const chatNotice = ref('')
    const chatNoticeTone = ref<'info' | 'warn' | 'error'>('info')
    const lastRunId = ref('')
    const archiveOpen = ref(false)
    const archiveBusy = ref(false)
    const actionNotice = ref('')
    const actionError = ref('')

    const transcript = ref<HTMLElement | null>(null)

    /** 新消息写入后滚到底部；列表自身可滚动，不带动整页跳到底。 */
    // Scrolls the transcript container (not the page) to the newest message.
    function scrollToBottom(): void {
      void nextTick(() => {
        const element = transcript.value
        if (element !== null) {
          element.scrollTop = element.scrollHeight
        }
      })
    }
    watch(
      () => session.data.value,
      () => {
        scrollToBottom()
      },
    )

    /** 聊天前置条件：归档会话、Agent 未注册或不可运行时给出原因并禁用发送。 */
    // The composer's blocked reason, or '' when sending is allowed.
    function chatBlockedReason(): string {
      const detail = session.data.value
      if (detail === null) {
        return ''
      }
      if (detail.status !== 'active') {
        return '会话已归档，不可追加消息。'
      }
      if (wrongAgent.value) {
        return '该会话不属于当前 Agent，不能在这里继续对话。'
      }
      const current = profile.value
      if (current === null) {
        return profiles.error.value === ''
          ? 'Agent 信息尚未加载。'
          : 'Agent 信息读取失败，暂时无法确认可否发送；请先刷新。'
      }
      if (!current.registered) {
        return 'Agent 未注册：请先修正 AGENT.md。'
      }
      if (!current.runnable) {
        return 'Agent 当前不可运行（停用或配置有误）；请先启用并修正问题。'
      }
      return ''
    }

    async function refresh(): Promise<void> {
      await Promise.all([session.reload(), profiles.reload()])
    }

    async function send(): Promise<void> {
      const detail = session.data.value
      if (detail === null || chatBusy.value) {
        return
      }
      const blocked = chatBlockedReason()
      if (blocked !== '') {
        chatNoticeTone.value = 'warn'
        chatNotice.value = blocked
        return
      }
      const text = chatInput.value.trim()
      if (text === '') {
        chatNoticeTone.value = 'error'
        chatNotice.value = '请输入消息内容'
        return
      }
      chatBusy.value = true
      chatNotice.value = ''
      lastRunId.value = ''
      try {
        const run = await api.sendSessionMessage(detail.session_id, text)
        const result = sendResultNotice(run)
        chatNoticeTone.value = result.tone
        chatNotice.value = result.text
        lastRunId.value = run.run_id
        chatInput.value = ''
        await refresh()
      } catch (caught) {
        if (caught instanceof ApiError && caught.code === 'SESSION_BUSY') {
          chatNoticeTone.value = 'warn'
          chatNotice.value = `该会话已有运行在执行，请稍后重试。${errorMessage(caught)}`
        } else if (caught instanceof ApiError && caught.status === 0) {
          chatNoticeTone.value = 'error'
          chatNotice.value = `${errorMessage(caught)} 无法确认这次消息是否已写入；请先刷新会话，不要直接重复发送。`
        } else {
          chatNoticeTone.value = 'error'
          chatNotice.value = errorMessage(caught)
        }
      } finally {
        chatBusy.value = false
      }
    }

    /** 归档只置 archived；409 SESSION_BUSY 时不产生任何写入。 */
    async function archive(): Promise<void> {
      const detail = session.data.value
      if (detail === null || archiveBusy.value) {
        return
      }
      archiveBusy.value = true
      actionError.value = ''
      try {
        await api.archiveSession(detail.session_id)
        archiveOpen.value = false
        actionNotice.value = `已归档会话 ${detail.session_id}；历史消息保留。`
        await refresh()
      } catch (caught) {
        archiveOpen.value = false
        if (caught instanceof ApiError && caught.code === 'SESSION_BUSY') {
          actionError.value = `会话仍有在途运行，未归档：${errorMessage(caught)} 请稍后重试。`
        } else if (caught instanceof ApiError && caught.status === 0) {
          actionError.value = `${errorMessage(caught)} 无法确认归档结果；请先刷新会话，不要直接重复提交。`
        } else {
          actionError.value = errorMessage(caught)
        }
        await refresh()
      } finally {
        archiveBusy.value = false
      }
    }

    function renderComposer(detail: SessionDetail): VNodeChild {
      if (detail.status !== 'active') {
        return h('div', { class: 'chat-composer' }, [
          h(Notice, {
            tone: 'info',
            text: '该会话已归档：只能查看历史消息，不能追加消息。需要继续对话请回 Agent 页点“新建会话”。',
          }),
          h('p', { class: 'muted small' }, [
            h('a', { href: agentHref(agentName) }, '← 返回 Agent 查看会话列表'),
          ]),
        ])
      }
      const blocked = chatBlockedReason()
      const children: VNodeChild[] = []
      if (blocked !== '') {
        children.push(h(Notice, { tone: 'warn', text: blocked }))
        children.push(
          h('p', { class: 'muted small' }, [
            h('a', { href: agentConfigHref(agentName) }, '打开 Agent 配置检查状态与模型连接'),
          ]),
        )
      }
      children.push(
        h('label', { class: 'field', for: 'session-chat-input' }, [
          h('span', '发送消息（同步执行一轮 ReAct 循环）'),
          h('textarea', {
            id: 'session-chat-input',
            class: 'editor',
            rows: 4,
            spellcheck: false,
            value: chatInput.value,
            placeholder: '输入要发送给这个 Agent 的消息',
            disabled: chatBusy.value || blocked !== '',
            onInput: (event: Event) => {
              chatInput.value = (event.target as HTMLTextAreaElement).value
            },
          }),
        ]),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: chatBusy.value || blocked !== '' || chatInput.value.trim() === '',
              onClick: () => {
                void send()
              },
            },
            chatBusy.value ? '运行中…' : '发送并运行',
          ),
        ]),
        h(
          'p',
          { class: 'muted small' },
          '消息会立即执行并返回本轮结果；limit_reached 是达到迭代上限的正常结果，不代表服务故障。',
        ),
      )
      return h('div', { class: 'chat-composer' }, children)
    }

    function renderTranscript(detail: SessionDetail): VNode {
      return h(
        'div',
        { class: 'transcript', ref: transcript, 'aria-label': '会话消息' },
        detail.messages.length === 0
          ? [h('p', { class: 'muted' }, '该会话还没有消息。')]
          : detail.messages.map((message, index) =>
              h('div', { key: `${detail.session_id}-${index}` }, [messageView(message)]),
            ),
      )
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        pageHeading(
          'WORKSPACE / AGENTS / SESSION',
          '会话详情',
          '查看完整对话、刷新消息、继续对话或归档会话；本页可直接收藏或分享链接。',
        ),
        h('p', { class: 'back-link' }, [
          h('a', { href: agentHref(agentName) }, `← 返回 ${agentName}`),
        ]),
      ]

      if (props.query.get('reopened') === '1') {
        content.push(
          h(Notice, {
            tone: 'info',
            text: '原 console 会话已归档；已用新的用户标识新建会话，归档记录仍保留在列表中。',
          }),
        )
      }

      if (session.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: session.error.value }))
      }
      if (session.loading.value && session.data.value === null) {
        content.push(h('p', { class: 'muted' }, '会话加载中…'))
      }

      const detail = session.data.value
      if (detail !== null) {
        if (wrongAgent.value) {
          content.push(
            h(Notice, {
              tone: 'error',
              text: `该会话属于 Agent“${detail.profile_name}”，不属于“${agentName}”；请从对应 Agent 的会话列表打开。`,
            }),
            h('p', { class: 'back-link' }, [
              h(
                'a',
                { href: sessionHref(detail.profile_name, detail.session_id) },
                `→ 打开 Agent“${detail.profile_name}”下的同一会话`,
              ),
            ]),
          )
        }
        content.push(
          h('div', { class: 'detail-identity' }, [
            h('code', { class: 'detail-id', title: detail.session_id }, detail.session_id),
            badge(detail.status === 'active' ? '活跃' : '已归档', detail.status === 'active' ? 'ok' : 'muted'),
            // 调度器创建的会话在详情页同样标出来源，不把 "scheduler" 当普通渠道名。
            // Scheduler-created sessions are marked here as well.
            h('span', { class: 'muted', title: detail.channel }, ` ${detail.channel === 'scheduler' ? '定时任务' : detail.channel} · ${detail.user_id}`),
          ]),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                disabled: session.loading.value || chatBusy.value,
                onClick: () => {
                  void refresh()
                },
              },
              session.loading.value ? '刷新中…' : '刷新消息',
            ),
            detail.status === 'active'
              ? h(
                  'button',
                  {
                    type: 'button',
                    class: 'danger',
                    disabled: archiveBusy.value,
                    onClick: () => {
                      archiveOpen.value = true
                    },
                  },
                  '归档会话',
                )
              : null,
          ]),
          section('会话信息', [
            definitions([
              ['状态', detail.status === 'active' ? '活跃（可继续对话）' : '已归档（只读）'],
              ['渠道', detail.channel === 'scheduler' ? '定时任务（系统调度）' : detail.channel],
              ['用户', detail.user_id],
              ['Agent', identifier(detail.profile_name, agentHref(detail.profile_name))],
              ['消息数', String(detail.message_count)],
              // 「最近使用」= 最近一次运行开始时间；未运行过时建行时间与它相同。
              // 消息数为 0 不代表没有运行过（准入后早失败 / 取消同样会留下零消息）。
              ['最近使用', h('span', {
                title: `创建于 ${formatTimestamp(detail.created_at)} · 最近一次运行开始 ${formatTimestamp(detail.last_active_at)}`,
              }, formatTimestamp(detail.last_active_at))],
              ['深链', h('code', { class: 'cell-id', title: window.location.origin + sessionHref(agentName, sessionId) }, window.location.origin + sessionHref(agentName, sessionId))],
            ]),
          ]),
          section('对话记录', [
            renderTranscript(detail),
            chatNotice.value === ''
              ? null
              : h(Notice, { tone: chatNoticeTone.value, text: chatNotice.value }),
            lastRunId.value === ''
              ? null
              : h('p', { class: 'muted small' }, [
                  h('a', { href: runHref(lastRunId.value) }, '查看本次运行详情'),
                ]),
            wrongAgent.value ? null : renderComposer(detail),
          ]),
        )
        if (actionNotice.value !== '') {
          content.push(h(Notice, { tone: 'info', text: actionNotice.value }))
        }
        if (actionError.value !== '') {
          content.push(h(Notice, { tone: 'error', text: actionError.value }))
        }
      }

      content.push(
        h(ConfirmDialog, {
          open: archiveOpen.value,
          title: '归档会话',
          message: '归档后只能查看历史消息，不能再追加消息；会话、消息与审计记录都会保留。继续？',
          confirmText: '归档会话',
          busy: archiveBusy.value,
          onConfirm: () => {
            void archive()
          },
          onCancel: () => {
            archiveOpen.value = false
          },
        }),
      )

      return h('section', { class: 'view view-session-detail' }, content)
    }
  },
})
