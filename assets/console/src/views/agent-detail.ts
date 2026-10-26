// Agent 详情视图：会话 / 运行 / 配置 / 记忆 / 定时 五页签。
// 配置页签内嵌统一的 AgentConfigPage（与独立路由 /console/agents/:name/config
// 同一个组件）；有未保存修改时装卸离开守卫。页面操作闭环（US-12 增量）：复制配置
// （默认停用、清空定时）与「移出工作区（可恢复）」的归档入口。
// Agent detail with five tabs. The config tab embeds the same AgentConfigPage used
// by the standalone config route, so there is one config entry. The page also
// carries copy and the recoverable archive action; both navigate on success and
// never retry a write whose outcome is unknown.
import { computed, defineComponent, h, onMounted, onUnmounted, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type {
  ExecutionView,
  MemoryView,
  ProfileView,
  ScheduleView,
  SessionSummary,
} from '../api'
import { adminApi } from '../api-admin'
import type { CredentialResourceKind, ResourceMapping, ScheduleDraftView } from '../api-admin'
import { AgentReadinessPanels } from '../components/agent-readiness'
import {
  CodeBlock,
  ConfirmDialog,
  DataTable,
  Notice,
  formatTimestamp,
} from '../components/ui'
import { agentHref, runHref, sessionHref } from '../router'
import { AgentConfigPage } from './agent-config'
import {
  AGENT_STATE_LABELS,
  RUN_STATUS_LABELS,
  badge,
  identifier,
  lifecycleTone,
  pageHeading,
  refreshStamp,
  runStatusTone,
  section,
  sendResultNotice,
  useLiveLoader,
  useLoader,
  viewProps,
} from './shared'

type TabKey = 'sessions' | 'runs' | 'config' | 'memory' | 'schedules'

const TABS: readonly { readonly key: TabKey; readonly label: string }[] = [
  { key: 'sessions', label: '会话' },
  { key: 'runs', label: '运行' },
  { key: 'config', label: '配置' },
  { key: 'memory', label: '记忆' },
  { key: 'schedules', label: '定时' },
]

/**
 * 模板写入的可见占位模型名（与 internal/workspace/templates.go 同字面量）。
 * 它通过校验、可注册，但作为模型名发往 Provider 时必然失败。
 * The visible placeholder written by the agent template; valid but unusable.
 */
const PLACEHOLDER_MODEL_NAME = '请在此填写模型名'

/** 标题用的显示名：未声明或空白时回退技术名（只换展示，不改身份）。 */
// The heading falls back to the technical name; identity never changes.
function shownName(profile: ProfileView): string {
  const display = profile.display_name.trim()
  return display === '' ? profile.name : display
}

export const AgentDetailView = defineComponent({
  name: 'AgentDetailView',
  props: viewProps,
  setup(props) {
    const name = props.params.name ?? ''
    const api = props.ctx.api
    const router = props.ctx.router
    const admin = adminApi(api)

    // 复制 / 恢复成功后的到达说明：默认停用与清空定时是两条路径的共同结果。
    // Arrival notice: both copy and restore land here disabled with schedules cleared.
    const arrivalNotice =
      props.query.get('copied') === '1'
        ? '已复制配置到新 Agent：默认停用、定时任务已清空；确认无误后再启用。'
        : props.query.get('restored') === '1'
          ? '已从归档恢复：默认停用、定时任务已清空；确认无误后再启用。'
          : ''

    const profiles = useLoader(() => api.listProfiles())
    const profile = computed<ProfileView | null>(
      () => profiles.data.value?.find((item) => item.name === name) ?? null,
    )

    // 三段面板的事实来源（设计 §2.5.1）：就绪投影、MCP 版本、服务版本与 Agent 目录。
    // 都是只读；任一读取失败只让对应行显示「不可读」，不合并成一条假结论。
    // Read-only inputs for the three readiness panels; a failed read marks only its
    // own row as unavailable instead of collapsing into a single fake verdict.
    const readiness = useLoader(() => api.getAgentReadiness(name))
    const agentMcps = useLoader(() => api.listMcpServers())
    const agentInfo = useLoader(() => api.getInfo())
    const agentTree = useLoader(() => admin.getWorkspaceTree(name))

    // 复制 / 恢复的写响应先于注册表刷新返回：到达页在约 5 秒内有界重试。
    // 重试期间不显示「未找到」，否则刚创建或刚恢复的 Agent 会被瞬时误报；
    // 普通访问只读一次，不引入轮询。有限任其结束，仍无结果才按未找到处理。
    // The registry can lag the copy/restore write, so arrival pages retry briefly
    // and keep a pending message until the bounded window closes.
    const arrivalExpected =
      props.query.get('copied') === '1' || props.query.get('restored') === '1'
    const arrivalRetrying = ref(arrivalExpected)
    let arrivalRetryCancelled = false
    onUnmounted(() => {
      arrivalRetryCancelled = true
    })
    onMounted(() => {
      if (!arrivalExpected) {
        return
      }
      void (async () => {
        try {
          for (let attempt = 0; attempt < 20 && !arrivalRetryCancelled; attempt += 1) {
            await new Promise((resolve) => window.setTimeout(resolve, 250))
            if (arrivalRetryCancelled || profile.value !== null) {
              return
            }
            await profiles.reload()
          }
        } finally {
          arrivalRetrying.value = false
        }
      })()
    })

    const tab = ref<TabKey>('sessions')

    // --- 页签数据 ---------------------------------------------------------
    // 会话 / 运行 / 定时都在数据库里，且会被其他窗口、调度器或上一轮运行改变：
    // 页签可见时每 20 秒重读一次（R8 裁决 3）。只读 GET，绝不重发运行或写请求；
    // 后台刷新不清空列表、不动滚动位置，页脚显示最近成功刷新时间。
    // The three database-backed tabs poll while visible; reads only, never writes.
    const sessions = useLiveLoader(() => api.listSessions({ agent: name }), {
      active: computed(() => tab.value === 'sessions'),
    })
    const runs = useLiveLoader(() => api.listRuns({ agent: name }), {
      active: computed(() => tab.value === 'runs'),
    })
    const schedules = useLiveLoader(async () =>
      (await api.listSchedules()).filter((item) => item.agent === name), {
      active: computed(() => tab.value === 'schedules'),
    })
    const memory = useLoader(() => api.getAgentMemory(name), false)

    /** 页头只显示当前页签的刷新时间，避免三个页签各留一份时间戳。 */
    // One header stamp follows the active tab; three footers would duplicate it.
    function activeRefreshStamp(): string {
      const current = tab.value === 'sessions' ? sessions : tab.value === 'runs' ? runs : schedules
      return refreshStamp(current.lastRefreshedAt.value)
    }

    // 页面操作（R3.5 / R7）：新建会话、归档会话在列表页；查看与会话内发消息走
    // 独立深链 `/console/agents/:name/sessions/:id`（刷新、分享、回到同一条会话）。
    // Create and archive live on the list; viewing and chatting use the deep link.
    const sessionCreateBusy = ref(false)
    const sessionNotice = ref('')
    const sessionActionError = ref('')
    const archiveSessionOpen = ref(false)
    const archiveSessionBusy = ref(false)
    /** 待归档的会话 id（列表行内按钮与确认框共用）。 */
    const archiveSessionTarget = ref('')

    /**
     * 新建 = 幂等打开 web / console 用户的会话；已存在时服务端返回同一条。
     * 该 tuple 已归档时服务端不复活旧行（契约语义），因此换一个新的控制台用户
     * 标识再建一次；归档会话保留在列表里可查，不让“新建会话”指向已归档记录。
     * 成功后直接进入会话深链，不再在长列表底部展开面板。
     * Reopening is idempotent; an archived row is never revived, so the console
     * retries once under a fresh user id and lands on the session deep link.
     */
    async function createSession(): Promise<void> {
      if (sessionCreateBusy.value) {
        return
      }
      sessionCreateBusy.value = true
      sessionNotice.value = ''
      sessionActionError.value = ''
      try {
        let created = await api.createSession(name, { channel: 'web', user_id: 'console' })
        let reopened = false
        if (created.status === 'archived') {
          const suffix = new Date().toISOString().replace(/[-:T]/g, '').slice(0, 14)
          const userId = `console-${suffix}`
          created = await api.createSession(name, { channel: 'web', user_id: userId })
          reopened = true
        }
        await sessions.reload()
        const target = sessionHref(name, created.session_id)
        router.navigate(reopened ? `${target}?reopened=1` : target)
      } catch (caught) {
        sessionActionError.value = errorMessage(caught)
      } finally {
        sessionCreateBusy.value = false
      }
    }

    function askArchiveSession(sessionId: string): void {
      if (archiveSessionBusy.value) {
        return
      }
      archiveSessionTarget.value = sessionId
      archiveSessionOpen.value = true
    }

    /** 归档会话只置 archived；409 SESSION_BUSY 时不产生任何写入。 */
    async function archiveSelectedSession(): Promise<void> {
      const target = archiveSessionTarget.value
      if (target === '' || archiveSessionBusy.value) {
        return
      }
      archiveSessionBusy.value = true
      sessionActionError.value = ''
      try {
        await api.archiveSession(target)
        archiveSessionOpen.value = false
        sessionNotice.value = `已归档会话 ${target}；历史消息保留。`
        await sessions.reload()
      } catch (caught) {
        archiveSessionOpen.value = false
        if (caught instanceof ApiError && caught.code === 'SESSION_BUSY') {
          sessionActionError.value = `会话仍有在途运行，未归档：${errorMessage(caught)} 请稍后重试。`
        } else if (caught instanceof ApiError && caught.status === 0) {
          sessionActionError.value = `${errorMessage(caught)} 无法确认归档结果；请先刷新会话列表，不要直接重复提交。`
        } else {
          sessionActionError.value = errorMessage(caught)
        }
        await sessions.reload()
      } finally {
        archiveSessionBusy.value = false
      }
    }

    // --- 定时页签：列表整体替换 -------------------------------------------
    // 定义在 AGENT.md 的 schedules 段；保存 = 用最新 ETag 整段替换，随后对账同步调度。
    // The whole schedules block is replaced under a fresh ETag, then reconciled.
    const scheduleFormOpen = ref(false)
    /** 正在编辑的任务 id；null = 新建。id 是任务身份，编辑时不可改。 */
    const scheduleEditingId = ref<string | null>(null)
    const scheduleId = ref('')
    const scheduleCron = ref('')
    const scheduleZone = ref('UTC')
    const scheduleMessage = ref('')
    const scheduleSaving = ref(false)
    const scheduleError = ref('')
    const scheduleNotice = ref('')
    const scheduleDeleteId = ref<string | null>(null)
    const scheduleRunBusyId = ref('')
    const scheduleToggleBusyId = ref('')
    const scheduleNoticeTone = ref<'info' | 'warn' | 'error'>('info')

    // --- 一句话生成定时候选（R6.4）------------------------------------------
    // 页面只发一句话；候选四字段与未来三次触发都由服务端给出（同一 cron 解析器），
    // 浏览器只把 UTC 时间格式化成本地时间展示，不自行推 cron。
    // The page sends one sentence; the server returns the candidate and its triggers.
    const schedulePrompt = ref('')
    const scheduleDraftBusy = ref(false)
    const scheduleDraftError = ref('')
    const scheduleCandidate = ref<ScheduleDraftView | null>(null)

    // --- 系统代投目标（R7）--------------------------------------------------
    // 目标 = 该 Agent 已声明通知渠道的资源 UID；空 = 不自动发送。可选清单只来自
    // AGENT.md 的声明（配置视图 references）——声明是权限边界，页面不提供任意 UID。
    // 配置了目标的任务由系统在运行成功后投递，运行内不再向模型提供 notify，避免双方
    // 各发一次；运行结果与投递结果是两条独立记录。
    // The delivery target is a channel UID from the Agent's own declarations; setting
    // one hands sending to the system and suppresses notify inside that run.
    const scheduleDeliverTo = ref('')
    // 立即读取：页头的三段面板用 fields.mcp_servers 与 provider，定时页签继续复用
    // 同一份结果，不重复请求。ensureDeliveryChoices 在已读到时仍是空操作。
    // Loaded eagerly for the readiness panels; the schedules tab reuses the same read.
    const scheduleConfig = useLoader(() => admin.getAgentConfig(name))
    const scheduleChannels = useLoader(() => api.listNotifyChannels(), false)

    interface DeliveryChoice {
      readonly uid: string
      readonly label: string
    }

    /** 已声明、已绑定、当前对本人有资格且未删除的通知渠道；其余一律不可选。 */
    // Only declared, bound, eligible and non-deleted notify channels are offered.
    const deliveryChoices = computed<DeliveryChoice[]>(() => {
      // 响应可能来自测试桩或旧服务端：多一层可选链，读不到就按"没有声明"处理，
      // 保存仍由服务端按声明校验。
      // One extra optional hop so a malformed payload reads as "no declarations".
      const references = scheduleConfig.data.value?.data?.references ?? []
      const channels = new Map(
        (scheduleChannels.data.value ?? []).map((item) => [item.resource_uid, item]),
      )
      const choices: DeliveryChoice[] = []
      for (const reference of references) {
        if (
          reference.resource_kind !== 'notify_channel' ||
          reference.binding_status !== 'bound' ||
          !reference.eligible ||
          reference.deleted
        ) {
          continue
        }
        const name =
          reference.resource_name === '' ? reference.ref_name : reference.resource_name
        const channel = channels.get(reference.resource_uid)
        const suffix =
          channel === undefined || channel.availability === 'ready'
            ? ''
            : channel.availability === 'unconfigured'
              ? '（未配置地址，暂时无法发送）'
              : '（加密托管地址不可用）'
        choices.push({ uid: reference.resource_uid, label: `${name}${suffix}` })
      }
      return choices
    })

    /** 新任务的默认目标：声明渠道的第一项；没有声明则不发送。 */
    // New tasks default to the first declared channel, matching the workspace default.
    function defaultDeliverTo(): string {
      return deliveryChoices.value[0]?.uid ?? ''
    }

    function deliveryChoiceLabel(uid: string): string {
      const choice = deliveryChoices.value.find((item) => item.uid === uid)
      if (choice !== undefined) {
        return choice.label
      }
      return `${uid}（已失效或未声明）`
    }

    /** 打开表单前确保渠道声明已读：读不到就让服务端兜底，不猜可用渠道。 */
    // Reads declarations before the form opens; a read failure leaves the final say
    // to the server instead of guessing an eligible channel.
    function ensureDeliveryChoices(): void {
      if (scheduleConfig.data.value === null && !scheduleConfig.loading.value) {
        void scheduleConfig.reload()
      }
      if (scheduleChannels.data.value === null && !scheduleChannels.loading.value) {
        void scheduleChannels.reload()
      }
    }

    // --- 执行与投递记录（分开展示）----------------------------------------
    // 每个任务读最近一页执行记录（契约固定窗口，按 started_at 倒序），只取第一行。
    // 读失败逐任务记录：读不到证据 ≠ 没有投递，页面必须分开说明。
    // Each task's latest execution row carries both the run and delivery outcome;
    // a read failure is surfaced as missing evidence, never as "not delivered".
    const scheduleExecutions = ref<Record<string, ExecutionView | null>>({})
    const scheduleExecutionFailures = ref<Record<string, string>>({})
    const scheduleExecutionsError = ref('')

    async function loadScheduleExecutions(items: readonly ScheduleView[]): Promise<void> {
      if (items.length === 0) {
        scheduleExecutions.value = {}
        scheduleExecutionFailures.value = {}
        scheduleExecutionsError.value = ''
        return
      }
      const next: Record<string, ExecutionView | null> = {}
      const failures: Record<string, string> = {}
      let firstFailure = ''
      const settled = await Promise.allSettled(
        items.map((item) => api.listExecutions(item.id)),
      )
      settled.forEach((result, index) => {
        const id = items[index]?.id ?? ''
        if (result.status === 'fulfilled') {
          next[id] = result.value[0] ?? null
          return
        }
        next[id] = null
        failures[id] = errorMessage(result.reason)
        if (firstFailure === '') {
          firstFailure = `${id}：${failures[id]}`
        }
      })
      scheduleExecutions.value = next
      scheduleExecutionFailures.value = failures
      scheduleExecutionsError.value =
        firstFailure === ''
          ? ''
          : `读取执行 / 投递记录失败（${firstFailure}）。这不代表没有投递，请稍后刷新。`
    }

    function openScheduleCreate(): void {
      scheduleEditingId.value = null
      scheduleId.value = ''
      scheduleCron.value = ''
      scheduleZone.value = 'UTC'
      scheduleMessage.value = ''
      scheduleDeliverTo.value = defaultDeliverTo()
      scheduleError.value = ''
      scheduleNotice.value = ''
      ensureDeliveryChoices()
      scheduleFormOpen.value = true
    }

    function openScheduleEdit(item: ScheduleView): void {
      scheduleEditingId.value = item.id
      scheduleId.value = item.id
      scheduleCron.value = item.cron
      scheduleZone.value = item.zone
      scheduleMessage.value = item.message
      scheduleDeliverTo.value = item.deliver_to
      scheduleError.value = ''
      scheduleNotice.value = ''
      ensureDeliveryChoices()
      scheduleFormOpen.value = true
    }

    function closeScheduleForm(): void {
      if (scheduleSaving.value) {
        return
      }
      scheduleFormOpen.value = false
      scheduleEditingId.value = null
      scheduleError.value = ''
    }

    /** 定义行只含文件字段；enabled 是数据库状态，不写进文件。 */
    // Definition rows carry the file fields; enabled is database state.
    // 用具名类型别名（不是 interface）：结构化补丁的 schedules 项是
    // `Record<string, unknown>[]`，interface 不带隐式索引签名会被拒。
    type ScheduleRow = {
      readonly id: string
      readonly cron: string
      readonly zone: string
      readonly message: string
      readonly deliver_to: string
    }

    function scheduleRows(items: readonly ScheduleView[]): ScheduleRow[] {
      return items.map((item) => ({
        id: item.id,
        cron: item.cron,
        zone: item.zone,
        message: item.message,
        deliver_to: item.deliver_to,
      }))
    }

    /** 本地预检与服务端同一口径的常用项；其余错误由 400 problems 展示。 */
    function validateSchedule(): ScheduleRow | null {
      const id = scheduleId.value.trim()
      const cron = scheduleCron.value.trim()
      const zone = scheduleZone.value.trim()
      const message = scheduleMessage.value.trim()
      const deliverTo = scheduleDeliverTo.value.trim()
      const existing = schedules.data.value ?? []
      if (id === '') {
        scheduleError.value = '请填写任务 ID（用于调度注册与执行历史对位）'
        return null
      }
      if (scheduleEditingId.value === null && existing.some((item) => item.id === id)) {
        scheduleError.value = `任务 ID 已存在：${id}`
        return null
      }
      if (cron === '') {
        scheduleError.value = '请填写 Cron 表达式'
        return null
      }
      if (cron.includes('CRON_TZ=')) {
        scheduleError.value = '时区只写在“时区”字段里；Cron 表达式不可带 CRON_TZ= 前缀'
        return null
      }
      if (cron.split(/\s+/).filter((field) => field !== '').length !== 6) {
        scheduleError.value = 'Cron 必须是 6 段（秒 分 时 日 月 周）'
        return null
      }
      if (message === '') {
        scheduleError.value = '请填写触发时发送给 Agent 的消息'
        return null
      }
      // 只在声明读取成功后做本地拦截；读取失败交给服务端同一道闸，不误判。
      if (
        deliverTo !== '' &&
        scheduleConfig.data.value !== null &&
        !deliveryChoices.value.some((choice) => choice.uid === deliverTo)
      ) {
        scheduleError.value =
          '投递渠道必须是该 Agent 已声明、已关联且当前可用的通知渠道；请重新选择，或改为“不自动发送”。'
        return null
      }
      return { id, cron, zone: zone === '' ? 'UTC' : zone, message, deliver_to: deliverTo }
    }

    /** 用最新 ETag 保存整份定时任务列表；成功后再重读列表与 Profile。 */
    // Saves the whole list under a fresh ETag, then re-reads list and profile.
    function saveScheduleList(
      rows: ScheduleRow[],
      successNotice: string,
      onSuccess?: () => void,
    ): void {
      if (scheduleSaving.value) {
        return
      }
      scheduleSaving.value = true
      scheduleError.value = ''
      scheduleNotice.value = ''
      void (async () => {
        try {
          const view = await api.getAgentContent(name)
          await api.updateAgentContent(name, { changes: { schedules: rows } }, view.etag)
          scheduleFormOpen.value = false
          scheduleEditingId.value = null
          scheduleNoticeTone.value = 'info'
          scheduleNotice.value = successNotice
          onSuccess?.()
          await Promise.all([schedules.reload(), profiles.reload(), runs.reload()])
          await loadScheduleExecutions(schedules.data.value ?? [])
        } catch (caught) {
          if (caught instanceof ApiError && caught.status === 412) {
            scheduleError.value = 'AGENT.md 已被他人修改，本次保存没有写入；请重新加载后重试。'
          } else {
            scheduleError.value = errorMessage(caught)
          }
        } finally {
          scheduleSaving.value = false
        }
      })()
    }

    function submitSchedule(): void {
      const entry = validateSchedule()
      if (entry === null) {
        return
      }
      const existing = schedules.data.value ?? []
      const rows = scheduleRows(
        scheduleEditingId.value === null
          ? existing
          : existing.filter((item) => item.id !== scheduleEditingId.value),
      )
      const others = rows.filter((item) => item.id !== entry.id)
      const editing = scheduleEditingId.value === null ? '已创建' : '已保存'
      const delivery =
        entry.deliver_to === ''
          ? '未配置投递渠道，结果只留在运行记录里。'
          : `运行成功后由系统投递到 ${deliveryChoiceLabel(entry.deliver_to)}。`
      saveScheduleList(
        [...others, entry],
        `${editing}定时任务 ${entry.id}；调度已按新定义同步。${delivery}`,
      )
    }

    /** 一句话 → 服务端定时候选（未保存）；失败只展示错误，不清空已输入的描述。 */
    async function draftScheduleFromPrompt(): Promise<void> {
      if (scheduleDraftBusy.value || scheduleSaving.value) {
        return
      }
      const instruction = schedulePrompt.value.trim()
      scheduleDraftError.value = ''
      if (instruction === '') {
        scheduleDraftError.value = '请用一句话说明什么时候运行、运行什么'
        return
      }
      scheduleDraftBusy.value = true
      scheduleCandidate.value = null
      try {
        scheduleCandidate.value = await admin.draftSchedule(name, instruction)
        // 候选与手工表单同一默认：声明渠道第一项；没有声明则不发送。
        scheduleDeliverTo.value = defaultDeliverTo()
        ensureDeliveryChoices()
      } catch (caught) {
        scheduleDraftError.value = errorMessage(caught)
      } finally {
        scheduleDraftBusy.value = false
      }
    }

    /** 确认候选：与手工表单同一条条件写；id 冲突时阻止，不静默覆盖同名任务。 */
    // A candidate saves through the same conditional write; an id clash is refused.
    function confirmScheduleCandidate(): void {
      const candidate = scheduleCandidate.value
      if (candidate === null || scheduleSaving.value) {
        return
      }
      const existing = schedules.data.value ?? []
      if (existing.some((item) => item.id === candidate.id)) {
        scheduleDraftError.value = `任务 ID 已存在：${candidate.id}；请换一种描述，或直接在下方编辑该任务。`
        return
      }
      saveScheduleList(
        [
          ...scheduleRows(existing),
          {
            id: candidate.id,
            cron: candidate.cron,
            zone: candidate.zone,
            message: candidate.message,
            deliver_to: scheduleDeliverTo.value,
          },
        ],
        scheduleDeliverTo.value === ''
          ? `已保存定时任务 ${candidate.id}；未配置投递渠道，结果只留在运行记录里。`
          : `已保存定时任务 ${candidate.id}；运行成功后由系统投递到 ${deliveryChoiceLabel(scheduleDeliverTo.value)}。`,
        () => {
          scheduleCandidate.value = null
          schedulePrompt.value = ''
          scheduleDraftError.value = ''
        },
      )
    }

    function discardScheduleCandidate(): void {
      if (scheduleSaving.value) {
        return
      }
      scheduleCandidate.value = null
      scheduleDraftError.value = ''
    }

    function askDeleteSchedule(item: ScheduleView): void {
      if (scheduleSaving.value) {
        return
      }
      scheduleDeleteId.value = item.id
    }

    function confirmDeleteSchedule(): void {
      const target = scheduleDeleteId.value
      if (target === null) {
        return
      }
      scheduleDeleteId.value = null
      const existing = schedules.data.value ?? []
      saveScheduleList(
        scheduleRows(existing.filter((item) => item.id !== target)),
        `已删除定时任务 ${target}；历史执行记录保留。`,
      )
    }

    /** 启停只改数据库状态（PUT /schedules/{id}），不改定义文件。 */
    async function toggleSchedule(item: ScheduleView): Promise<void> {
      if (scheduleToggleBusyId.value !== '' || scheduleSaving.value) {
        return
      }
      scheduleToggleBusyId.value = item.id
      scheduleError.value = ''
      scheduleNotice.value = ''
      try {
        const next = !item.enabled
        await api.setScheduleEnabled(item.id, next)
        scheduleNoticeTone.value = 'info'
        scheduleNotice.value = next
          ? `已启用定时任务 ${item.id}。`
          : `已停用定时任务 ${item.id}；下次触发前会重新核对启用状态。`
        await schedules.reload()
      } catch (caught) {
        scheduleError.value = errorMessage(caught)
      } finally {
        scheduleToggleBusyId.value = ''
      }
    }

    /** 立即运行一轮：同步返回 RunView；结果文案与聊天发送同一口径。 */
    async function runScheduleNow(item: ScheduleView): Promise<void> {
      if (scheduleRunBusyId.value !== '' || scheduleSaving.value) {
        return
      }
      scheduleRunBusyId.value = item.id
      scheduleError.value = ''
      scheduleNotice.value = ''
      try {
        const run = await api.runSchedule(item.id)
        const result = sendResultNotice(run)
        scheduleNoticeTone.value = result.tone
        scheduleNotice.value = `${item.id}：${result.text}`
        await Promise.all([runs.reload(), schedules.reload()])
        await loadScheduleExecutions(schedules.data.value ?? [])
      } catch (caught) {
        if (caught instanceof ApiError && caught.code === 'SESSION_BUSY') {
          scheduleError.value = `上一轮仍在执行，本次没有触发：${errorMessage(caught)}`
        } else if (caught instanceof ApiError && caught.status === 0) {
          scheduleError.value = `${errorMessage(caught)} 无法确认本次触发结果；请先刷新运行列表，不要直接重复触发。`
        } else {
          scheduleError.value = errorMessage(caught)
        }
      } finally {
        scheduleRunBusyId.value = ''
      }
    }

    // --- 配置页签：内嵌统一配置组件 ---------------------------------------
    /** 配置组件上报的未保存状态；页签切换与路由离开都由它守护。 */
    const configDirty = ref(false)

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

    // --- 复制配置 ---------------------------------------------------------
    // 复制是服务端一次性动作：成功后直接进入新 Agent（带 ?copied=1 到达说明）。
    // Copy is a single server action; success navigates to the new Agent.
    const copyOpen = ref(false)
    const copyName = ref('')
    const copyBusy = ref(false)
    const copyError = ref('')

    // 源专属引用被 409 `AGENT_COPY_BLOCKED` 阻断后的显式映射（FR-070 / Demo M）：
    // 每一项都必须由用户选一个工作区共享的替代资源，页面不替用户挑、也不自动解绑。
    // After a blocked copy the page asks for one shared replacement per exclusive
    // reference; nothing is mapped implicitly and nothing is unbound automatically.
    interface BlockedReference {
      readonly kind: CredentialResourceKind
      readonly ref: string
      readonly sourceUID: string
    }
    const copyBlocked = ref<BlockedReference[]>([])
    const copyTargets = ref<Record<string, string>>({})
    const copyCandidates = ref<Record<CredentialResourceKind, { readonly uid: string; readonly name: string }[]>>({
      model_connection: [],
      notify_channel: [],
    })
    const copyCandidatesError = ref('')

    function blockedKey(entry: BlockedReference): string {
      return `${entry.kind}:${entry.ref}`
    }

    /** 解析 `problems` 的固定写法「<类别> <名称>」；无法识别的条目直接展示服务端文案。 */
    // The blocked list uses the literal "<kind> <name>" form from the server.
    function parseBlocked(problems: readonly string[]): BlockedReference[] {
      const parsed: BlockedReference[] = []
      for (const problem of problems) {
        const [rawKind, ...rest] = problem.split(' ')
        const ref = rest.join(' ').trim()
        if ((rawKind === 'model_connection' || rawKind === 'notify_channel') && ref !== '') {
          parsed.push({ kind: rawKind, ref, sourceUID: '' })
        }
      }
      return parsed
    }

    /**
     * 映射需要源 UID，而阻断响应只点名；源 UID 从配置视图的引用里取（同名资源不按名
     * 重解析）。候选只列工作区共享且已发布 / 未删除的资源——副本的 agent_uid 在复制
     * 完成后才生成，专属资源对它没有资格，服务端会按同一口径再拒一次。
     * The blocked response names resources only; source UIDs come from the config
     * view, and candidates are workspace-shared resources only.
     */
    async function enterCopyBlocked(caught: ApiError): Promise<void> {
      const problems: string[] = caught.data?.problems ?? []
      const parsed = parseBlocked(problems)
      if (parsed.length === 0) {
        copyError.value = errorMessage(caught)
        return
      }
      copyCandidatesError.value = ''
      copyTargets.value = {}
      try {
        const [config, models, channels] = await Promise.all([
          admin.getAgentConfig(name),
          api.listModelConnections(),
          api.listNotifyChannels(),
        ])
        const byReference = new Map<string, string>()
        for (const reference of config.data.references) {
          byReference.set(`${reference.resource_kind}:${reference.ref_name}`, reference.resource_uid)
        }
        copyBlocked.value = parsed.map((entry) => ({
          ...entry,
          sourceUID: byReference.get(`${entry.kind}:${entry.ref}`) ?? '',
        }))
        copyCandidates.value = {
          model_connection: models
            .filter((item) => item.scope === 'shared' && item.published_version_id > 0)
            .map((item) => ({ uid: item.resource_uid, name: item.name })),
          notify_channel: channels
            .filter((item) => item.scope === 'shared')
            .map((item) => ({ uid: item.resource_uid, name: item.name })),
        }
      } catch (mappingCaught) {
        copyCandidatesError.value = `读取替代资源失败：${errorMessage(mappingCaught)}`
      }
    }

    /** 阻断清单未全部选择替代资源时，本地拦下并给出下一步，不发请求。 */
    // Every blocked reference needs an explicit target before the retry.
    function copyMapping(): ResourceMapping[] | null {
      const mapping: ResourceMapping[] = []
      for (const entry of copyBlocked.value) {
        const target = copyTargets.value[blockedKey(entry)] ?? ''
        if (entry.sourceUID === '') {
          copyError.value = `无法证明「${entry.ref}」当前关联的资源身份，请重新读取配置后再试。`
          return null
        }
        if (target === '') {
          copyError.value = '请为每个专属引用选择替代的共享资源后重试'
          return null
        }
        mapping.push({
          resource_kind: entry.kind,
          source_resource_uid: entry.sourceUID,
          target_resource_uid: target,
        })
      }
      return mapping
    }

    function openCopy(): void {
      if (copyBusy.value || archiveBusy.value) {
        return
      }
      copyName.value = ''
      copyError.value = ''
      copyBlocked.value = []
      copyTargets.value = {}
      copyCandidatesError.value = ''
      copyOpen.value = true
    }

    function closeCopy(): void {
      if (copyBusy.value) {
        return
      }
      copyOpen.value = false
      copyName.value = ''
      copyBlocked.value = []
      copyTargets.value = {}
      copyCandidatesError.value = ''
    }

    /** 409 = 目标名已存在；400 = 名称不合法；其余交给服务端固定文案。 */
    // 409 means the target name exists; 400 means an invalid name.
    function copyErrorText(caught: unknown): string {
      const text = errorMessage(caught)
      if (caught instanceof ApiError && caught.status === 409) {
        return `复制被拒绝：目标名称已存在。${text}`
      }
      if (caught instanceof ApiError && caught.status === 400) {
        return `复制被拒绝：目标名称不合法。${text}`
      }
      return text
    }

    async function confirmCopy(): Promise<void> {
      const target = copyName.value.trim()
      if (target === '' || copyBusy.value) {
        return
      }
      let mapping: ResourceMapping[] | undefined
      if (copyBlocked.value.length > 0) {
        const explicit = copyMapping()
        if (explicit === null) {
          return
        }
        mapping = explicit
      }
      copyBusy.value = true
      copyError.value = ''
      try {
        const created = await admin.copyAgent(name, target, mapping)
        copyOpen.value = false
        router.navigate(agentHref(created.agent) + '?copied=1')
      } catch (caught) {
        // 409 有两种：目标名冲突与源专属引用未映射；后者进入映射向导，不冒充重名。
        if (caught instanceof ApiError && caught.code === 'AGENT_COPY_BLOCKED') {
          copyError.value = ''
          await enterCopyBlocked(caught)
        } else {
          copyError.value = copyErrorText(caught)
        }
      } finally {
        copyBusy.value = false
      }
    }

    // --- 移出工作区（可恢复的归档）----------------------------------------
    // 409 AGENT_BUSY 点名仍在运行的 Run 并刷新当前状态；网络错误的写结果未知，
    // 只提示先查证，不自动重试（取消信号已发出，无法承诺"运行毫无变化"）。
    // A 409 AGENT_BUSY lists the surviving runs and refreshes state; a network
    // failure is left unknown on purpose (cancellation signals cannot be undone).
    const archiveOpen = ref(false)
    const archiveBusy = ref(false)
    const archiveError = ref('')

    function openArchive(): void {
      if (archiveBusy.value || copyBusy.value) {
        return
      }
      archiveError.value = ''
      archiveOpen.value = true
    }

    function closeArchive(): void {
      if (archiveBusy.value) {
        return
      }
      archiveOpen.value = false
    }

    function archiveErrorText(caught: unknown): string {
      const text = errorMessage(caught)
      if (caught instanceof ApiError && caught.code === 'AGENT_BUSY') {
        const runIDs = caught.data?.run_ids ?? []
        const runs = runIDs.length === 0 ? '' : `仍在运行：${runIDs.join('、')}。`
        return `移出工作区被拒绝：等待在途 Run 退出的时间已超时，${runs}请稍后重试。${text}`
      }
      if (caught instanceof ApiError && caught.status === 0) {
        return `${text} 无法确认移出结果；请先查询当前状态或到「管理操作」查证，不要直接重复提交。`
      }
      return text
    }

    async function confirmArchive(): Promise<void> {
      if (archiveBusy.value) {
        return
      }
      archiveBusy.value = true
      archiveError.value = ''
      try {
        await admin.archiveAgent(name)
        archiveOpen.value = false
        // 列表页重新挂载并重新读取，不做乐观更新。
        // The list view remounts and re-reads; no optimistic update.
        router.navigate('/console/agents?archived=1')
      } catch (caught) {
        // 失败时收起对话框，把点名与提示留在页头提示区；当前页面不跳转。
        // Close the dialog on failure; the named runs and guidance stay on the page.
        archiveOpen.value = false
        archiveError.value = archiveErrorText(caught)
        if (caught instanceof ApiError && caught.code === 'AGENT_BUSY') {
          // 保留当前页并刷新生命周期 / 运行状态，让用户看到占用是否已释放。
          await profiles.reload()
        }
      } finally {
        archiveBusy.value = false
      }
    }

    // --- 页签切换与未保存守卫 --------------------------------------------
    function selectTab(key: TabKey): void {
      if (key === tab.value) {
        return
      }
      // 配置页签的草稿随组件销毁；切换前确认，避免静默丢失。
      if (
        tab.value === 'config' &&
        configDirty.value &&
        !window.confirm('配置有未保存的修改，切换页签将丢失这些修改。继续？')
      ) {
        return
      }
      tab.value = key
      if (key === 'memory' && memory.data.value === null && !memory.loading.value) {
        void memory.reload()
      }
      // 会话与运行页签的进入刷新由各自的 useLiveLoader 在 active 变为 true 时触发
      // （见 shared.ts），不在这里再开第二条刷新路径。
      // Sessions/runs refresh on activation inside useLiveLoader; no second path here.
      // 定时页签每次进入都重读：启停状态、上次运行与投递结果都在数据库里，
      // 可能已被他人或上一轮调度改变。
      // Enabled flags, last runs and delivery rows live in the database; re-read.
      if (key === 'schedules') {
        void (async () => {
          await Promise.all([
            schedules.refreshNow(),
            scheduleConfig.reload(),
            scheduleChannels.reload(),
          ])
          await loadScheduleExecutions(schedules.data.value ?? [])
        })()
      }
    }

    watch(
      configDirty,
      (value) => {
        router.setLeaveGuard(
          value ? () => window.confirm('配置有未保存的修改，确定离开？') : null,
        )
      },
      { immediate: true },
    )

    const beforeUnload = (event: BeforeUnloadEvent): void => {
      if (!configDirty.value) {
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
    /** 刷新会话列表；打开中的会话由独立详情页自己读取与刷新。 */
    // Re-reads the list only; the open session refreshes on its own route.
    async function refreshSessions(): Promise<void> {
      await sessions.refreshNow()
    }

    /** 会话行操作：查看走深链，活跃会话可归档。 */
    // Row actions: view via the deep link, archive while active.
    function sessionRowActions(item: SessionSummary): VNode {
      const actions: VNodeChild[] = [
        h('a', { href: sessionHref(name, item.session_id) }, '查看'),
      ]
      if (item.status === 'active') {
        actions.push(
          h(
            'button',
            {
              type: 'button',
              class: 'danger',
              disabled: archiveSessionBusy.value,
              onClick: () => askArchiveSession(item.session_id),
            },
            '归档',
          ),
        )
      }
      return h('div', { class: 'form-actions' }, actions)
    }

    function renderSessions(): VNodeChild {
      const content: VNodeChild[] = []
      if (sessions.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: sessions.error.value }))
      }
      content.push(
        h('div', { class: 'form-actions tab-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: sessionCreateBusy.value,
              onClick: () => {
                void createSession()
              },
            },
            sessionCreateBusy.value ? '打开中…' : '新建会话',
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: sessions.loading.value,
              onClick: () => {
                void refreshSessions()
              },
            },
            '刷新',
          ),
        ]),
        h(
          'p',
          { class: 'muted small' },
          '“新建会话”会打开（或复用）当前控制台用户在该 Agent 下的 web 会话；同一 Agent、渠道与用户组合始终对应同一条会话。' +
            '每条会话有自己的详情页（可刷新、可收藏链接、可继续对话），不再在列表下方展开。' +
            '“最近使用”显示最近一次运行开始时间；还没有运行过的新会话显示创建时间。列表在本页签可见时每 20 秒自动刷新，只重新读取、不重发任何运行请求。',
        ),
      )
      if (sessionNotice.value !== '') {
        content.push(h(Notice, { tone: 'info', text: sessionNotice.value }))
      }
      if (sessionActionError.value !== '') {
        content.push(h(Notice, { tone: 'error', text: sessionActionError.value }))
      }
      const data = sessions.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            headers: ['会话', '渠道', '用户', '状态', '消息数', '最近使用', '操作'],
            tableClass: 'agent-sessions-table',
            columnClasses: ['col-id', 'col-short', 'col-name', 'col-state', 'col-number', 'col-time', 'col-actions'],
            rows: data.map((item) => ({
              key: item.session_id,
              cells: [
                identifier(item.session_id, sessionHref(name, item.session_id)),
                // 调度器创建的会话明确标来源；其余渠道保持原文。
                item.channel === 'scheduler'
                  ? h('span', { title: item.channel }, [badge('定时任务', 'muted')])
                  : item.channel,
                item.user_id,
                item.status === 'active' ? '活跃' : '已归档',
                String(item.message_count),
                // 「最近使用」= 最近一次运行开始时间；未运行过时建行时间与它相同。
                // 不能用消息数判断是否运行过：准入后早失败 / 取消会留下零消息但已运行的会话。
                // message_count is not a run marker; last_active_at is the admission timestamp.
                h('span', {
                  title: `创建于 ${formatTimestamp(item.created_at)} · 最近一次运行开始 ${formatTimestamp(item.last_active_at)}`,
                }, formatTimestamp(item.last_active_at)),
                sessionRowActions(item),
              ],
            })),
            empty: '该 Agent 还没有会话；点上方“新建会话”开始。',
          }),
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
      // 单入口：独立路由与页签内嵌使用同一个组件，共用条件写、ETag 与写入门。
      return h('div', { class: 'tab-panel config-embedded' }, [
        h(AgentConfigPage, {
          key: 'agent-config',
          ctx: props.ctx,
          params: props.params,
          query: props.query,
          embedded: true,
          onDirtyChange: (value: boolean) => {
            configDirty.value = value
          },
          // 配置保存成功后重读本页的所有配置派生读数：就绪三段面板、MCP 列表与计划
          // 配置都来自保存前的那次读取。此前不重读，用户必须退出工作区再进来才看到
          // 「模型连接：就绪」（用户实测 2026-10-27）。
          // Re-read every config-derived panel after a successful save.
          onSaved: () => {
            void Promise.all([
              profiles.reload(),
              readiness.reload(),
              agentMcps.reload(),
              scheduleConfig.reload(),
            ])
          },
        }),
      ])
    }

    // 记忆展示按实际行为命名：常驻区每轮完整注入且不裁剪；可检索区只在
    // recall_memory 关键词命中时进入上下文，不是语义检索、也不是自动抽取。
    // 文件分区标题仍是契约固定的「核心记忆 / 归档记忆」，这里给出的是行为名。
    // The memory view names the two partitions by behaviour; the file headings stay
    // the contract literals 核心记忆 / 归档记忆.
    function renderMemoryView(view: MemoryView): VNodeChild {
      const parts: VNodeChild[] = []
      parts.push(
        h(
          'p',
          { class: 'muted small' },
          '本页只读：记忆的写入只经记忆工具 save_memory（运行中由 Agent 调用），页面不提供新增、修改或删除。',
        ),
      )
      parts.push(
        section('常驻记忆', [
          h(
            'p',
            { class: 'muted small' },
            '每次运行都会把这一整段完整注入系统提示词，不参与历史裁剪（文件分区标题为「核心记忆」）。' +
              '写入来自 Agent 在运行中调用 save_memory 且 scope=CORE；控制台不提供写入，手工维护需编辑服务端记忆文件。',
          ),
          view.core === ''
            ? h(
                'p',
                { class: 'muted' },
                '这一层还没有条目。Agent 在运行中调用 save_memory 并显式选择核心区后才会出现内容。',
              )
            : h(CodeBlock, { text: view.core }),
        ]),
      )
      parts.push(
        section('可检索记忆', [
          h(
            'p',
            { class: 'muted small' },
            '不会自动注入上下文。Agent 调用 recall_memory 时按关键词匹配这一区，命中的条目才进入当次运行' +
              '（文件分区标题为「归档记忆」）。不是语义检索，也不会自动从对话里抽取内容。',
          ),
          view.archive === ''
            ? h(
                'p',
                { class: 'muted' },
                '这一层还没有条目。save_memory 省略 scope 时写入这里，需要 Agent 在运行中主动调用。',
              )
            : h(CodeBlock, { text: view.archive }),
        ]),
      )
      // 会话历史与长期记忆是两层：消息由服务端随运行保存，但不会自己变成记忆条目。
      // Session history and long-term memory are separate layers; messages never become
      // memory entries on their own.
      parts.push(
        h(Notice, {
          tone: 'info',
          text:
            '会话消息随每次运行自动保存为会话历史（在「会话」页签查看），但那只代表对话记录本身：' +
            '它不会自动进入长期记忆，也不会在对话结束后被抽取成记忆条目。长期记忆只有 Agent 在运行中调用 save_memory 才会写入。',
        }),
      )
      parts.push(
        h(Notice, {
          tone: 'info',
          text:
            view.scope === 'agent'
              ? '此页只显示这个 Agent 自己的记忆层，不会回退到全局记忆。运行时若该层为空，提示词会使用工作区的全局记忆；两层不会叠加，因此页面为空不代表运行中没有可用记忆。'
              : '全局记忆是工作区独立维护的一层，不是各 Agent 记忆的汇总。正常 Agent 运行只写自己的记忆层；只有不带 Agent 身份的运行才会写入这里。',
        }),
      )
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

    /** 定时任务行操作：编辑定义、启停、立即运行、删除。 */
    // Row actions: edit the definition, toggle, run now, delete.
    function scheduleRowActions(item: ScheduleView): VNode {
      const busy =
        scheduleSaving.value ||
        scheduleRunBusyId.value !== '' ||
        scheduleToggleBusyId.value !== ''
      return h('div', { class: 'form-actions' }, [
        h(
          'button',
          {
            type: 'button',
            disabled: busy,
            onClick: () => openScheduleEdit(item),
          },
          '编辑',
        ),
        h(
          'button',
          {
            type: 'button',
            disabled: busy,
            onClick: () => {
              void toggleSchedule(item)
            },
          },
          scheduleToggleBusyId.value === item.id ? '提交中…' : item.enabled ? '停用' : '启用',
        ),
        h(
          'button',
          {
            type: 'button',
            disabled: busy,
            onClick: () => {
              void runScheduleNow(item)
            },
          },
          scheduleRunBusyId.value === item.id ? '运行中…' : '立即运行',
        ),
        h(
          'button',
          {
            type: 'button',
            class: 'danger',
            disabled: busy,
            onClick: () => askDeleteSchedule(item),
          },
          '删除',
        ),
      ])
    }

    /**
     * 代投渠道下拉：选项只来自该 Agent 的声明。空选项 = 不自动发送；若当前目标已
     * 失效或不在声明里，保留为一个显式选项，让用户看见再改，而不是被页面静默替换。
     * The select offers declared channels only; a stale current target stays visible.
     */
    function renderDeliverySelect(id: string, disabled: boolean): VNode {
      const current = scheduleDeliverTo.value
      const stale =
        current !== '' && !deliveryChoices.value.some((choice) => choice.uid === current)
      return h(
        'select',
        {
          id,
          disabled,
          onChange: (event: Event) => {
            scheduleDeliverTo.value = (event.target as HTMLSelectElement).value
          },
        },
        [
          h(
            'option',
            { value: '', selected: current === '' },
            '不自动发送（结果只留在运行记录里）',
          ),
          ...deliveryChoices.value.map((choice) =>
            h('option', { value: choice.uid, selected: current === choice.uid }, choice.label),
          ),
          stale
            ? h('option', { value: current, selected: true }, `当前目标已失效（${current}）`)
            : null,
        ],
      )
    }

    function renderScheduleForm(): VNodeChild {
      if (!scheduleFormOpen.value) {
        return null
      }
      const editing = scheduleEditingId.value !== null
      const disabled = scheduleSaving.value
      return h('section', { class: 'card schedule-form' }, [
        h('h2', editing ? `修改定时任务 ${scheduleEditingId.value ?? ''}` : '新建定时任务'),
        h('label', { class: 'field', for: 'schedule-id' }, [
          h('span', editing ? 'ID（任务身份，不可修改）' : 'ID（全局唯一）'),
          h('input', {
            id: 'schedule-id',
            type: 'text',
            value: scheduleId.value,
            disabled: editing || disabled,
            autocomplete: 'off',
            spellcheck: false,
            placeholder: '例如 daily-report',
            onInput: (event: Event) => {
              scheduleId.value = (event.target as HTMLInputElement).value
            },
          }),
        ]),
        h('label', { class: 'field', for: 'schedule-cron' }, [
          h('span', 'Cron（6 段：秒 分 时 日 月 周）'),
          h('input', {
            id: 'schedule-cron',
            type: 'text',
            value: scheduleCron.value,
            disabled,
            autocomplete: 'off',
            spellcheck: false,
            placeholder: '0 0 9 * * *',
            onInput: (event: Event) => {
              scheduleCron.value = (event.target as HTMLInputElement).value
            },
          }),
        ]),
        h('label', { class: 'field', for: 'schedule-zone' }, [
          h('span', '时区（IANA 名称；留空 = UTC）'),
          h('input', {
            id: 'schedule-zone',
            type: 'text',
            value: scheduleZone.value,
            disabled,
            autocomplete: 'off',
            spellcheck: false,
            placeholder: 'Asia/Shanghai',
            onInput: (event: Event) => {
              scheduleZone.value = (event.target as HTMLInputElement).value
            },
          }),
        ]),
        h('label', { class: 'field', for: 'schedule-message' }, [
          h('span', '触发消息（到点后同步执行一轮）'),
          h('textarea', {
            id: 'schedule-message',
            class: 'editor',
            rows: 3,
            spellcheck: false,
            disabled,
            value: scheduleMessage.value,
            placeholder: '例如：汇总昨天的新闻并给出三条要点',
            onInput: (event: Event) => {
              scheduleMessage.value = (event.target as HTMLTextAreaElement).value
            },
          }),
        ]),
        h('label', { class: 'field', for: 'schedule-deliver-to' }, [
          h('span', '结果投递渠道（运行成功后由系统发送）'),
          renderDeliverySelect('schedule-deliver-to', disabled),
        ]),
        scheduleConfig.error.value === ''
          ? null
          : h(Notice, {
              tone: 'warn',
              text: `读取该 Agent 的渠道声明失败：${scheduleConfig.error.value}。渠道选项可能不完整，保存仍由服务端按声明校验。`,
            }),
        scheduleError.value === '' ? null : h(Notice, { tone: 'error', text: scheduleError.value }),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled,
              onClick: submitSchedule,
            },
            disabled ? '保存中…' : editing ? '保存修改' : '创建任务',
          ),
          h('button', { type: 'button', disabled, onClick: closeScheduleForm }, '取消'),
        ]),
        h(
          'p',
          { class: 'muted small' },
          '保存会整体替换 AGENT.md 的 schedules 段；创建 / 修改 / 删除都走同一条条件写。配置投递渠道后，这一轮运行结束后由系统发送，运行内不再向模型提供 notify（避免模型与系统各发一次）；执行结果与投递结果分开记录，发送失败或结果未知时不会自动重发。',
        ),
      ])
    }

    /** 一句话生成区：只发指令；候选卡在确认前不写文件。 */
    function renderScheduleComposer(): VNode {
      return h('section', { class: 'card schedule-composer', id: 'schedule-composer' }, [
        h('h2', '一句话生成定时任务'),
        h(
          'p',
          { class: 'muted' },
          '例如「每天上午九点汇总昨天的新闻并发到飞书」。作者模型给出候选；未来三次触发由服务端计算；确认前不写入文件。',
        ),
        h('div', { class: 'form-actions' }, [
          h('input', {
            id: 'schedule-prompt',
            type: 'text',
            autocomplete: 'off',
            placeholder: '每天上午九点生成一份简报',
            value: schedulePrompt.value,
            disabled: scheduleDraftBusy.value || scheduleSaving.value,
            onInput: (event: Event) => {
              schedulePrompt.value = (event.target as HTMLInputElement).value
            },
            onKeydown: (event: KeyboardEvent) => {
              if (event.key === 'Enter') {
                event.preventDefault()
                void draftScheduleFromPrompt()
              }
            },
          }),
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              id: 'schedule-prompt-draft',
              disabled:
                scheduleDraftBusy.value || scheduleSaving.value || schedulePrompt.value.trim() === '',
              onClick: () => {
                void draftScheduleFromPrompt()
              },
            },
            scheduleDraftBusy.value ? '生成中…' : '生成候选',
          ),
        ]),
        scheduleDraftError.value !== '' && scheduleCandidate.value === null
          ? h(Notice, { tone: 'error', text: scheduleDraftError.value })
          : null,
      ])
    }

    /** 候选卡：四字段 + 服务端三次触发；确认走既有条件写，id 冲突阻止保存。 */
    function renderScheduleCandidate(): VNodeChild {
      const candidate = scheduleCandidate.value
      if (candidate === null) {
        return null
      }
      const runs = candidate.next_runs
      return h('section', { class: 'card schedule-candidate', id: 'schedule-candidate' }, [
        h('h2', '定时候选（尚未保存）'),
        h('dl', { class: 'definitions' }, [
          h('dt', {}, '任务 ID'),
          h('dd', {}, [h('code', {}, candidate.id)]),
          h('dt', {}, 'Cron'),
          h('dd', {}, [h('code', {}, candidate.cron)]),
          h('dt', {}, '时区'),
          h('dd', {}, candidate.zone),
          h('dt', {}, '消息'),
          h('dd', {}, candidate.message),
          h('dt', {}, '结果投递渠道'),
          h('dd', {}, [
            renderDeliverySelect('schedule-candidate-deliver-to', scheduleSaving.value),
            h(
              'p',
              { class: 'muted small' },
              '默认选中该 Agent 声明的第一个通知渠道；可改为“不自动发送”。',
            ),
          ]),
          h('dt', {}, '未来三次触发'),
          h(
            'dd',
            {},
            runs.length === 0
              ? '（暂无触发时间）'
              : runs.map((at) =>
                  h('div', { class: 'schedule-run', key: at }, [
                    h('code', {}, at),
                    h('span', { class: 'muted small' }, ` 本地 ${formatTimestamp(at)}`),
                  ]),
                ),
          ),
        ]),
        h(
          'p',
          { class: 'muted small' },
          '时间由服务端用与调度器相同的 6 段 cron 解析器计算（返回 UTC）；页面只换算成本地时间显示，不自行推导。确认后走与手工表单相同的条件写。',
        ),
        scheduleDraftError.value === '' ? null : h(Notice, { tone: 'error', text: scheduleDraftError.value }),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              id: 'schedule-candidate-save',
              disabled: scheduleSaving.value,
              onClick: confirmScheduleCandidate,
            },
            scheduleSaving.value ? '保存中…' : '确认并保存',
          ),
          h(
            'button',
            {
              type: 'button',
              id: 'schedule-candidate-discard',
              disabled: scheduleSaving.value,
              onClick: discardScheduleCandidate,
            },
            '放弃候选',
          ),
        ]),
      ])
    }

    /** 投递状态固定标签（契约字面量 pending / sent / failed / unknown / skipped）。 */
    const DELIVERY_STATUS_LABELS: Record<string, string> = {
      pending: '待发送',
      sent: '已发送',
      failed: '发送失败',
      unknown: '结果未知',
      skipped: '未投递',
    }

    /**
     * 上次投递单元格：执行结果与投递结果分开显示。读失败单独标出——读不到证据
     * 不等于没有投递；没有执行记录显示「—」，有执行但没有投递行显示「未记录」。
     */
    function deliveryCell(item: ScheduleView): VNodeChild {
      const failure = scheduleExecutionFailures.value[item.id]
      if (failure !== undefined) {
        return h('span', { class: 'delivery-failed', title: failure }, '读取失败')
      }
      const execution = scheduleExecutions.value[item.id] ?? null
      if (execution === null) {
        return h('span', { class: 'muted' }, '—')
      }
      if (execution.delivery_status === '') {
        return h('span', { class: 'muted' }, '未记录')
      }
      const label = DELIVERY_STATUS_LABELS[execution.delivery_status] ?? execution.delivery_status
      return h('div', { class: 'delivery-cell' }, [
        h('span', {}, label),
        execution.delivery_detail === ''
          ? null
          : h('span', { class: 'muted small' }, execution.delivery_detail),
      ])
    }

    function renderSchedules(): VNodeChild {
      const content: VNodeChild[] = []
      if (schedules.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: schedules.error.value }))
      }
      if (scheduleExecutionsError.value !== '') {
        content.push(h(Notice, { tone: 'warn', text: scheduleExecutionsError.value }))
      }
      // 结果投递的提示按事实分三档：未声明渠道 → 没有可投目标；部分任务没配目标 →
      // 这些任务不归系统发（旧任务提示）；全部配了 → 不再提示 notify 工具。
      // Delivery hints follow the facts: no declared channel, some tasks untargeted,
      // or every task targeted (in which case notify no longer matters).
      const current = profile.value
      const tasks = schedules.data.value ?? []
      const missingTarget = tasks.filter((item) => item.deliver_to === '')
      if (current !== null && current.notify_channels.length === 0) {
        content.push(
          h(Notice, {
            tone: 'info',
            text: '该 Agent 还没有声明通知渠道：定时任务的结果只留在运行记录里，不会发送。先在「配置 → 通知」声明渠道，再回来为任务选择投递渠道。',
          }),
        )
      } else if (missingTarget.length > 0) {
        content.push(
          h(Notice, {
            tone: 'warn',
            text: `以下定时任务没有配置结果投递渠道：${missingTarget
              .map((item) => item.id)
              .join('、')}。它们到点照常执行，但结果不会由系统发送（模型是否主动调用 notify 不由系统保证）；建议逐条“修改”并为任务选择投递渠道，由系统在运行成功后发送。`,
          }),
        )
      }
      content.push(
        h('div', { class: 'form-actions tab-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: scheduleSaving.value || scheduleFormOpen.value,
              onClick: openScheduleCreate,
            },
            '新建定时任务',
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: schedules.loading.value || scheduleSaving.value,
              onClick: () => {
                void schedules.refreshNow()
              },
            },
            '刷新',
          ),
        ]),
      )
      content.push(renderScheduleComposer())
      content.push(renderScheduleCandidate())
      content.push(renderScheduleForm())
      if (!scheduleFormOpen.value && scheduleError.value !== '') {
        content.push(h(Notice, { tone: 'error', text: scheduleError.value }))
      }
      if (scheduleNotice.value !== '') {
        content.push(h(Notice, { tone: scheduleNoticeTone.value, text: scheduleNotice.value }))
      }
      const data = schedules.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            headers: [
              'ID',
              'Cron（6 段）',
              '时区',
              '启用',
              '上次运行',
              '上次执行状态',
              '投递渠道',
              '上次投递',
              '消息',
              '操作',
            ],
            tableClass: 'agent-schedules-table',
            columnClasses: [
              'col-name',
              'col-short',
              'col-short',
              'col-state',
              'col-time',
              'col-state',
              '',
              '',
              '',
              'col-actions',
            ],
            rows: data.map((item) => ({
              key: item.id,
              cells: [
                h('code', item.id),
                h('code', item.cron),
                item.zone,
                item.enabled ? '是' : '否',
                formatTimestamp(item.last_run_at),
                item.last_status === '' ? '—' : item.last_status,
                item.deliver_to === ''
                  ? h('span', { class: 'muted' }, '不自动发送')
                  : deliveryChoiceLabel(item.deliver_to),
                deliveryCell(item),
                item.message,
                scheduleRowActions(item),
              ],
            })),
            empty: '该 Agent 没有定时任务；点上方“新建定时任务”添加。',
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

    /** 阻断后的映射区：每个源专属引用一行下拉，候选只来自工作区共享资源。 */
    // One select per blocked exclusive reference; candidates are shared resources.
    function renderCopyMapping(): VNodeChild {
      if (copyBlocked.value.length === 0) {
        return null
      }
      const models = copyBlocked.value.filter((entry) => entry.kind === 'model_connection').length
      const channels = copyBlocked.value.filter((entry) => entry.kind === 'notify_channel').length
      const rows = copyBlocked.value.map((entry) => {
        const key = blockedKey(entry)
        const options = copyCandidates.value[entry.kind]
        const choices = [
          h(
            'option',
            { value: '' },
            options.length === 0 ? '没有可选的共享资源' : '请选择共享资源',
          ),
          ...options.map((option) =>
            h('option', { key: option.uid, value: option.uid }, `${option.name}（${option.uid.slice(0, 8)}…）`),
          ),
        ]
        return h('label', { key, class: 'field' }, [
          h(
            'span',
            `${entry.kind === 'model_connection' ? '模型连接' : '通知渠道'}「${entry.ref}」→`,
          ),
          h(
            'select',
            {
              'data-blocked': key,
              value: copyTargets.value[key] ?? '',
              onChange: (event: Event) => {
                copyTargets.value = {
                  ...copyTargets.value,
                  [key]: (event.target as HTMLSelectElement).value,
                }
              },
            },
            choices,
          ),
        ])
      })
      return h('div', { class: 'copy-mapping' }, [
        h('h3', '专属引用需要显式映射'),
        h(
          'p',
          `此 Agent 使用了 ${models} 个专属模型连接和 ${channels} 个专属通知渠道。` +
            '复制不会带走这些资源或它们的凭据。请为副本明确选择替代资源，或为副本新建资源。',
        ),
        ...rows,
        copyCandidatesError.value === ''
          ? null
          : h(Notice, { tone: 'error', text: copyCandidatesError.value }),
        h(
          'p',
          { class: 'muted small' },
          '只列出工作区共享的资源：副本的身份在复制完成后才生成，专属资源对它没有资格。',
        ),
        h(
          'p',
          { class: 'muted small' },
          '复制完成后，可在副本的「配置」页为它就地新建专属连接（填写 API 密钥、选择范围）并保存关联。',
        ),
      ])
    }

    /** 复制对话框：输入新名称 + 明确说明默认停用与清空定时；执行期间禁用重复提交。 */
    // Copy dialog with an explicit new name and the disabled/schedules-cleared note.
    function renderCopyDialog(): VNode | null {
      if (!copyOpen.value) {
        return null
      }
      return h('div', { class: 'dialog-backdrop' }, [
        h(
          'div',
          { class: 'dialog', role: 'dialog', 'aria-modal': 'true', 'aria-label': '复制 Agent 配置' },
          [
            h('h2', '复制 Agent 配置'),
            h('p', '从当前 Agent 完整复制工作区文件。新 Agent 默认停用，定时任务清空。'),
            h('label', { class: 'field' }, [
              h('span', '新名称'),
              h('input', {
                type: 'text',
                value: copyName.value,
                placeholder: '新 Agent 的名称',
                autocomplete: 'off',
                onInput: (event: Event) => {
                  copyName.value = (event.target as HTMLInputElement).value
                },
              }),
            ]),
            renderCopyMapping(),
            copyError.value === '' ? null : h(Notice, { tone: 'error', text: copyError.value }),
            h('div', { class: 'dialog-actions' }, [
              h(
                'button',
                { type: 'button', disabled: copyBusy.value, onClick: closeCopy },
                '取消',
              ),
              h(
                'button',
                {
                  type: 'button',
                  class: 'primary',
                  disabled: copyBusy.value || copyName.value.trim() === '',
                  onClick: () => {
                    void confirmCopy()
                  },
                },
                copyBusy.value ? '复制中…' : '确认复制',
              ),
            ]),
          ],
        ),
      ])
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
          disabled: transitionBusy.value || archiveBusy.value,
          onClick: askTransition,
        },
        current.state === 'active' ? '停用 Agent' : '启用 Agent',
      )
    }

    /** 页头动作：复制配置、生命周期迁移，以及「更多操作」里的移出工作区。 */
    // Header actions: copy, lifecycle transition, and the recoverable archive in a
    // "more actions" menu rather than a danger zone.
    function renderHeadingActions(): VNode {
      return h('div', { class: 'page-actions' }, [
        h(
          'button',
          {
            type: 'button',
            disabled: copyOpen.value || copyBusy.value || archiveBusy.value,
            onClick: openCopy,
          },
          '复制配置',
        ),
        renderTransitionButton(),
        h('details', { class: 'more-actions' }, [
          h('summary', { role: 'button', 'aria-label': '更多操作' }, '更多操作'),
          h('div', { class: 'more-actions-menu' }, [
            h(
              'button',
              {
                type: 'button',
                class: 'danger',
                disabled: archiveBusy.value || copyBusy.value,
                onClick: openArchive,
              },
              archiveBusy.value ? '移出工作区中…' : '移出工作区（可恢复）',
            ),
          ]),
        ]),
      ])
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
        if (arrivalRetrying.value) {
          content.push(h('p', { class: 'muted' }, '正在确认新 Agent 的注册状态…'))
        } else {
          content.push(
            h(Notice, { tone: 'warn', text: `未找到 Agent「${name}」，它可能尚未注册或已被移除。` }),
            h('p', {}, [h('a', { href: '/console/agents' }, '← 返回 Agents 列表')]),
          )
        }
      }

      const current = profile.value
      if (current !== null) {
        content.push(
          h('div', { class: 'agent-head' }, [
            h('p', { class: 'breadcrumb' }, [
              h('a', { href: '/console/agents' }, '← 返回 Agents 列表'),
            ]),
            pageHeading('WORKSPACE / AGENT', shownName(current), current.description || '查看这个 Agent 的会话、运行、配置、记忆与定时任务。', renderHeadingActions()),
            h('div', { class: 'agent-title' }, [renderLifecycleBadge()]),
            h('p', { class: 'agent-meta' }, [
              // 标题用显示名时，技术名必须在同一屏可见：目录、API 与运行记录只认技术名。
              // The technical name stays visible whenever the title shows a display name.
              shownName(current) === current.name
                ? null
                : h('code', { class: 'agent-technical-name', title: '技术名（目录 / API）' }, current.name),
              h('code', `${current.provider} / ${current.model}`),
              ...renderLifecycleMeta(),
              h('span', { class: 'muted small refresh-stamp' }, activeRefreshStamp()),
            ]),
            current.problems.length === 0
              ? null
              : h(
                  'ul',
                  { class: 'problems' },
                  current.problems.map((problem) => h('li', { key: problem }, problem)),
                ),
            // 三段面板与 problems 并存（设计 §2.5.1）：problems 继续显示通用问题，
            // 这三段只回答「离能跑还差什么」，阻断与提示分开。
            // The three panels coexist with the generic problems list; blockers and
            // advisories stay on separate rows.
            h(AgentReadinessPanels, {
              readiness: readiness.data.value,
              readinessError: readiness.error.value,
              config: scheduleConfig.data.value?.data ?? null,
              configError: scheduleConfig.error.value,
              mcps: agentMcps.data.value,
              info: agentInfo.data.value,
              infoError: agentInfo.error.value,
              entries: agentTree.data.value,
              entriesError: agentTree.error.value,
              lifecycle: current.lifecycle.state,
            }),
            // 模板占位符是普通文本：能通过校验、能注册，但每次运行都会在模型
            // 调用处失败。这里提前点名，而不是等用户发起会话后才看到失败。
            // The template placeholder passes validation and registers, but every run
            // fails at the model call — surface it before the first run.
            current.model === PLACEHOLDER_MODEL_NAME
              ? h(Notice, {
                  tone: 'warn',
                  text: 'provider.model 仍是模板占位符「请在此填写模型名」：Agent 可以启用，但运行会在模型调用处失败。请到「配置」页把 model 改成真实模型名，或改用复制配置得到一个已配置的 Agent。',
                })
              : null,
            transitionError.value === ''
              ? null
              : h(Notice, { tone: 'error', text: transitionError.value }),
            arrivalNotice === '' ? null : h(Notice, { tone: 'info', text: arrivalNotice }),
            archiveError.value === ''
              ? null
              : h(Notice, { tone: 'error', text: archiveError.value }),
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
          renderCopyDialog(),
          h(ConfirmDialog, {
            open: archiveSessionOpen.value,
            title: '归档会话',
            message:
              `归档会话 ${archiveSessionTarget.value} 后将只保留历史消息，不能再追加新消息；会话记录不会删除。` +
              '若仍有在途运行，归档会被拒绝且不产生任何写入。',
            confirmText: archiveSessionBusy.value ? '归档中…' : '归档会话',
            busy: archiveSessionBusy.value,
            onConfirm: () => {
              void archiveSelectedSession()
            },
            onCancel: () => {
              if (!archiveSessionBusy.value) {
                archiveSessionOpen.value = false
              }
            },
          }),
          h(ConfirmDialog, {
            open: scheduleDeleteId.value !== null,
            title: '删除定时任务',
            message:
              `删除定时任务 ${scheduleDeleteId.value ?? ''} 后将不再触发；历史执行记录保留，` +
              '定义会从 AGENT.md 的 schedules 段移除。',
            confirmText: scheduleSaving.value ? '删除中…' : '删除',
            busy: scheduleSaving.value,
            onConfirm: confirmDeleteSchedule,
            onCancel: () => {
              if (!scheduleSaving.value) {
                scheduleDeleteId.value = null
              }
            },
          }),
          h(ConfirmDialog, {
            open: archiveOpen.value,
            title: '移出工作区（可恢复）',
            message:
              '移出后：新运行与消息会被拒绝，定时任务注销；正在运行的任务会被请求取消，' +
              '超出等待时限时本次操作返回 409 并完整回滚，不会留下半移出的状态。' +
              '会话、消息、记忆、审计与定时执行历史都保留，Agent 目录移入归档区并登记一条归档记录。' +
              '恢复后它默认处于停用状态、定时任务已清空，需要显式启用；' +
              '模型连接与通知渠道的关联按定义重建，若资源在此期间发布了新版本，请在配置页复核。',
            confirmText: archiveBusy.value ? '移出中…' : '移出工作区',
            busy: archiveBusy.value,
            onConfirm: () => {
              void confirmArchive()
            },
            onCancel: closeArchive,
          }),
        )
      }

      return h('section', { class: 'view view-agent-detail' }, content)
    }
  },
})
