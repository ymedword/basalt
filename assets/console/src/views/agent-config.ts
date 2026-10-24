// 统一的 Agent 配置页（/console/agents/:name/config，同一组件内嵌在 Agent 详情
// 的「配置」页签）。七个分区：基本信息 / 执行规则 / 模型 / 工具与技能 / MCP /
// 通知 / 高级设置。模型连接、工具、技能、MCP 与通知渠道都从服务端目录取候选项
// （不硬编码、不伪造选项）；目录读取失败时降级为手动填写并说明原因。保存只提交
// 真正改动的字段（`changes`），与原文编辑共用同一个 `content_etag`；412 只刷新
// ETag 并保留输入，成功后重新读取服务端真值。原文编辑器放在「高级设置」里，表单
// 与原文各自有未保存修改时互相阻止保存，避免一方静默覆盖另一方。
// The unified agent config view (standalone route and embedded in the detail page).
// Seven sections; every option list comes from a server catalog and falls back to
// explicit manual entry when a catalog cannot be read. Saves send only changed fields
// through `changes`, share one `content_etag` with the raw editor, refresh on 412, and
// reread the server after every successful write. Raw and form drafts block each other
// so neither silently overwrites the other.
import { computed, defineComponent, h, onBeforeUnmount, onMounted, onUnmounted, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage, resourceErrorCopy, resourceErrorCopyOf } from '../api'
import type {
  AgentConfigReference,
  AgentConfigView,
  AgentContentBinding,
  AgentContentChanges,
  AgentToolChangeView,
  CredentialMode,
  McpServerView,
  ModelConnectionView,
  NotifyChannelView,
  NotifyMode,
  ResourceScope,
  SkillView,
  ToolView,
} from '../api'
import { adminApi, problemsOf } from '../api-admin'
import type { ModelConnectionUpsert, NotifyChannelUpsert, WorkspaceEntry } from '../api-admin'
import { splitAgentBody } from '../agent-body'
import { BindingList } from '../components/binding-list'
import type { BindingRow } from '../components/binding-list'
import { CredentialField } from '../components/credential-field'
import { ScopePicker } from '../components/scope-picker'
import { Notice } from '../components/ui'
import { agentHref, resourceDetailHref } from '../router'
import { AVAILABILITY_LABELS, pageHeading, section, useLoader, viewProps } from './shared'

/** 通知意图三态的逐字标签（R4.2「通知」分区）。 */
const NOTIFY_MODE_LABELS: Readonly<Record<NotifyMode, string>> = {
  none: '不使用通知',
  all_eligible: '使用所有可用渠道',
  allowlist: '仅使用所选渠道',
}

/** 环境变量引用只接受精确 `${VAR}`；页面负责把裸变量名补成该形态。 */
const ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]*$/

/** temperature 的两种编辑语义：省略字段（模型默认）或写入显式数值。 */
type TemperatureMode = 'default' | 'custom'

/** 目录候选项的统一形态：名称、说明、来源 / 状态与可用性。 */
interface Choice {
  readonly name: string
  readonly description: string
  readonly meta: string
  readonly available: boolean
}

/** 结构化列表字段的文本形态：一行一项，忽略空行与首尾空白。 */
function splitList(value: string): string[] {
  return value
    .split('\n')
    .map((item) => item.trim())
    .filter((item) => item !== '')
}

function sameList(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((item, index) => item === right[index])
}

/** 勾选 / 取消勾选：保留其它项的相对顺序，新项追加到末尾。 */
function toggleName(current: readonly string[], name: string, checked: boolean): string[] {
  const rest = current.filter((item) => item !== name)
  return checked ? [...rest, name] : rest
}

/** 把裸环境变量名或完整占位归一成 `${VAR}`；其它输入（真实密钥、URL）返回 null。 */
function envReference(raw: string): string | null {
  const trimmed = raw.trim()
  const inner =
    trimmed.startsWith('${') && trimmed.endsWith('}')
      ? trimmed.slice(2, -1)
      : trimmed
  return ENV_KEY.test(inner) ? '${' + inner + '}' : null
}

/** 文件大小的紧凑显示。 */
function formatBytes(size: number): string {
  if (size < 1024) {
    return `${size} B`
  }
  return `${(size / 1024).toFixed(1)} KiB`
}

/** 保存失败时附带「资源已创建」上下文：创建与绑定是两步，不能把前者说成失败。 */
interface SaveContext {
  readonly createdResource?: { readonly kind: '模型连接' | '通知渠道'; readonly name: string }
}

interface BlockingIssue {
  readonly label: string
  readonly code: string
}

export const AgentConfigPage = defineComponent({
  name: 'AgentConfigPage',
  props: {
    ...viewProps,
    /** true = 内嵌在 Agent 详情页签里（不渲染自己的页眉与离开守卫）。 */
    embedded: { type: Boolean, default: false },
  },
  emits: {
    'dirty-change': (dirty: boolean) => typeof dirty === 'boolean',
  },
  setup(props, { emit }) {
    const name = props.params.name ?? ''
    const api = props.ctx.api
    const admin = adminApi(api)
    const config = useLoader(() => admin.getAgentConfig(name))
    const content = useLoader(() => api.getAgentContent(name))
    const candidates = useLoader(async () => {
      const [models, channels] = await Promise.all([
        api.listModelConnections(),
        api.listNotifyChannels(),
      ])
      return { models, channels }
    })
    // 工具 / 技能 / MCP 目录：选项来自服务端；读取失败时降级为手动填写。
    const catalogs = useLoader(async () => {
      const [tools, skills, mcpServers] = await Promise.all([
        api.listTools(),
        api.listSkills(),
        api.listMcpServers(),
      ])
      return { tools, skills, mcpServers }
    })
    const notifyTypes = useLoader(() => admin.listNotifyTypes())

    // --- 表单草稿（applyConfig / applyBody 会整体覆盖；保存后由重新读取覆盖）----
    const etag = ref('')
    const description = ref('')
    /** 展示名（display_name）：空串 = 未声明，展示处回退技术名；不改身份。 */
    const displayName = ref('')
    /** `identity.agent_name` 是写进系统提示的人设自称，不是列表显示名。 */
    const persona = ref('')
    const prompt = ref('')
    const body = ref('')
    const bodyOriginal = ref('')
    /** 原文围栏可解析时才允许编辑正文；不可解析时只提示改用原文编辑器。 */
    const bodyEditable = ref(false)
    const bodyParseFailed = ref(false)
    const emptyBodyArmed = ref(false)
    const providerName = ref('')
    /** 当前选择的连接 UID：选择与提交都以 UID 为准，名称只是标签。 */
    const providerUID = ref('')
    const providerModel = ref('')
    const temperatureMode = ref<TemperatureMode>('default')
    const temperatureText = ref('')
    const tools = ref<string[]>([])
    const skills = ref<string[]>([])
    const mcpServers = ref<string[]>([])
    const bootstrap = ref<string[]>([])
    // 目录读取失败时的降级草稿；目录可用时以数组为准。
    const toolsText = ref('')
    const skillsText = ref('')
    const mcpText = ref('')
    const notifyMode = ref<NotifyMode>('none')
    const notifyChannelNames = ref<string[]>([])
    /** 已选渠道名 → UID（来自声明或本次显式选择）。 */
    const notifyPicks = ref<Record<string, string>>({})
    const maxIterationsText = ref('')
    const maxHistoryTurnsText = ref('')

    // --- 选择器状态 ----------------------------------------------------------
    const providerQuery = ref('')
    const toolAdd = ref('')
    const skillAdd = ref('')
    const mcpAdd = ref('')
    const bootstrapPath = ref('')
    const bootstrapOpen = ref(false)
    const bootstrapDir = ref('')
    const bootstrapEntries = ref<readonly WorkspaceEntry[]>([])
    const bootstrapLoaded = ref(false)
    const bootstrapLoading = ref(false)
    const bootstrapError = ref('')

    // --- 配置原文 ------------------------------------------------------------
    const rawOpen = ref(false)
    const rawLoaded = ref(false)
    const rawOriginal = ref('')
    const rawEditor = ref('')
    const rawSaving = ref(false)
    const rawError = ref('')
    const rawNotice = ref('')

    // --- 就地新建连接 --------------------------------------------------------
    const showNewConnection = ref(false)
    const newConnectionName = ref('')
    const newConnectionBaseURL = ref('')
    const newConnectionModel = ref('')
    const newConnectionKey = ref('')
    const newConnectionMode = ref<CredentialMode>('managed')
    const newConnectionScope = ref<ResourceScope>('exclusive')
    const newConnectionPrefillUID = ref('')

    // --- 就地新建通知渠道 ----------------------------------------------------
    const showNewChannel = ref(false)
    const newChannelName = ref('')
    const newChannelType = ref('')
    const newChannelURL = ref('')
    const newChannelMode = ref<CredentialMode>('managed')
    const newChannelScope = ref<ResourceScope>('exclusive')
    const newChannelDescription = ref('')

    // --- 反馈 ---------------------------------------------------------------
    const busy = ref(false)
    const error = ref('')
    const problems = ref<string[]>([])
    const notice = ref('')

    function resetFeedback(): void {
      error.value = ''
      problems.value = []
      notice.value = ''
    }

    function resetRawFeedback(): void {
      rawError.value = ''
      rawNotice.value = ''
    }

    function bindingUID(view: AgentConfigView, kind: AgentConfigReference['resource_kind'], refName: string): string {
      const found = view.references.find(
        (reference) => reference.resource_kind === kind && reference.ref_name === refName,
      )
      return found?.resource_uid ?? ''
    }

    /** 当前声明块的扁平形态：模型在前、通知在后，与补丁里的顺序一致。 */
    function currentBindings(view: AgentConfigView): AgentContentBinding[] {
      const out: AgentContentBinding[] = []
      for (const item of view.fields.resource_bindings.model_connections) {
        out.push({ resource_kind: 'model_connection', ref_name: item.name, resource_uid: item.uid })
      }
      for (const item of view.fields.resource_bindings.notify_channels) {
        out.push({ resource_kind: 'notify_channel', ref_name: item.name, resource_uid: item.uid })
      }
      return out
    }

    /** 绑定块是否需要提交：整块替换，集合或顺序变化都算变化。 */
    function bindingsDiffer(current: readonly AgentContentBinding[], desired: readonly AgentContentBinding[]): boolean {
      if (current.length !== desired.length) {
        return true
      }
      return current.some(
        (item, index) =>
          item.resource_kind !== desired[index]!.resource_kind ||
          item.ref_name !== desired[index]!.ref_name ||
          item.resource_uid !== desired[index]!.resource_uid,
      )
    }

    function applyConfig(view: AgentConfigView, contentETag: string): void {
      etag.value = contentETag === '' ? view.content_etag : contentETag
      const fields = view.fields
      description.value = fields.description
      displayName.value = fields.display_name
      persona.value = fields.identity.agent_name
      prompt.value = fields.identity.prompt
      providerName.value = fields.provider.name
      providerUID.value = bindingUID(view, 'model_connection', fields.provider.name)
      providerModel.value = fields.provider.model
      const temperature = fields.provider.temperature
      temperatureMode.value = temperature === null ? 'default' : 'custom'
      temperatureText.value = temperature === null ? '' : String(temperature)
      tools.value = [...fields.tools]
      skills.value = [...fields.skills]
      mcpServers.value = [...fields.mcp_servers]
      bootstrap.value = [...fields.bootstrap]
      toolsText.value = fields.tools.join('\n')
      skillsText.value = fields.skills.join('\n')
      mcpText.value = fields.mcp_servers.join('\n')
      notifyMode.value = view.notify_mode
      notifyChannelNames.value = [...fields.notify_channels]
      const picks: Record<string, string> = {}
      for (const reference of view.references) {
        if (reference.resource_kind === 'notify_channel' && reference.resource_uid !== '') {
          picks[reference.ref_name] = reference.resource_uid
        }
      }
      notifyPicks.value = picks
      maxIterationsText.value = String(fields.settings.max_iterations)
      maxHistoryTurnsText.value = String(fields.settings.max_history_turns)
      // 服务端是唯一真值：任何一次重新读取都丢弃本地待保存内容，避免页面“记住”旧状态。
      emptyBodyArmed.value = false
    }

    function applyBody(raw: string): void {
      const split = splitAgentBody(raw)
      bodyEditable.value = split.ok
      bodyParseFailed.value = !split.ok
      body.value = split.body
      bodyOriginal.value = split.body
      emptyBodyArmed.value = false
    }

    watch(config.data, (next) => {
      if (next !== null) {
        applyConfig(next.data, next.etag)
      }
    })
    watch(content.data, (next) => {
      if (next === null) {
        return
      }
      applyBody(next.content)
      rawLoaded.value = true
      // 原文没有被本地改过时跟随服务端；有未保存修改时保留编辑器内容。
      if (!rawDirty.value) {
        rawOriginal.value = next.content
        rawEditor.value = next.content
      }
    })

    const references = computed<readonly AgentConfigReference[]>(
      () => config.data.value?.data.references ?? [],
    )
    // 资源候选的派生列表用普通函数而不是 computed：本页在 Vitest + jsdom + Vue 3.5.44
    // 下实测到 computed→computed→shallowRef 链的失效——候选重新读取后，同一 tick 里
    // 新建的 computed 已能读到新列表，长期存活的 computed 却仍返回旧值（2026-10-23，
    // console 测试复现）。列表只有几十条，按需过滤比缓存更可靠。
    function allModels(): readonly ModelConnectionView[] {
      return candidates.data.value?.models ?? []
    }
    function allChannels(): readonly NotifyChannelView[] {
      return candidates.data.value?.channels ?? []
    }
    /** 只列已发布且有使用资格（共享或本人专属）的连接。 */
    function modelPool(): readonly ModelConnectionView[] {
      return allModels().filter(
        (connection) =>
          connection.published_version_id > 0 &&
          (connection.scope === 'shared' || connection.owner_agent === name),
      )
    }
    function channelPool(): readonly NotifyChannelView[] {
      return allChannels().filter(
        (channel) => channel.scope === 'shared' || channel.owner_agent === name,
      )
    }
    const bodyDirty = computed(() => bodyEditable.value && body.value !== bodyOriginal.value)

    function selectedModel(): ModelConnectionView | null {
      return modelPool().find((connection) => connection.resource_uid === providerUID.value) ?? null
    }

    function selectedChannelNamed(channelName: string): NotifyChannelView | null {
      return channelPool().find((channel) => channel.name === channelName) ?? null
    }

    function notifyUID(channelName: string): string {
      return notifyPicks.value[channelName] ?? ''
    }

    /** 保存前先本地拦下的绑定问题：服务端同样会拒绝，但页面要指出改哪一处。 */
    const blockingIssues = computed<readonly BlockingIssue[]>(() => {
      const issues: BlockingIssue[] = []
      if (providerName.value !== '') {
        if (providerUID.value === '') {
          issues.push({ label: `模型连接 ${providerName.value}`, code: 'AGENT_RESOURCE_BINDING_MISSING' })
        } else {
          const connection = selectedModel()
          if (connection === null) {
            issues.push({ label: `模型连接 ${providerName.value}`, code: 'AGENT_RESOURCE_UNAVAILABLE' })
          } else if (connection.credential_state === 'revoked') {
            issues.push({ label: `模型连接 ${providerName.value}`, code: 'CREDENTIAL_REVOKED' })
          }
        }
      }
      if (notifyMode.value === 'allowlist') {
        for (const channelName of notifyChannelNames.value) {
          if (notifyUID(channelName) === '') {
            issues.push({ label: `通知渠道 ${channelName}`, code: 'AGENT_RESOURCE_BINDING_MISSING' })
            continue
          }
          const channel = selectedChannelNamed(channelName)
          if (channel === null) {
            issues.push({ label: `通知渠道 ${channelName}`, code: 'AGENT_RESOURCE_UNAVAILABLE' })
          } else if (channel.credential_state === 'revoked') {
            issues.push({ label: `通知渠道 ${channelName}`, code: 'CREDENTIAL_REVOKED' })
          }
        }
      }
      return issues
    })

    // --- 目录候选项 ----------------------------------------------------------

    const catalogReady = computed(() => catalogs.data.value !== null)

    function effectiveList(items: readonly string[], text: string): readonly string[] {
      return catalogReady.value ? items : splitList(text)
    }

    const effectiveTools = computed(() => effectiveList(tools.value, toolsText.value))
    const effectiveSkills = computed(() => effectiveList(skills.value, skillsText.value))
    const effectiveMcp = computed(() => effectiveList(mcpServers.value, mcpText.value))

    function toolChoices(): readonly Choice[] {
      const known: Choice[] = (catalogs.data.value?.tools ?? []).map((tool: ToolView) => ({
        name: tool.name,
        description: tool.description,
        meta:
          tool.name === 'notify'
            ? '内置工具 · 由通知意图自动增删（系统受控，仅此一项）'
            : tool.source === 'builtin'
              ? '内置工具'
              : tool.source === 'mcp'
                ? 'MCP 工具'
                : '代码工具',
        available: true,
      }))
      const knownNames = new Set(known.map((choice) => choice.name))
      const extras: Choice[] = tools.value
        .filter((toolName) => !knownNames.has(toolName))
        .map((toolName) => ({
          name: toolName,
          description: '',
          meta: '不在当前工具目录中：运行时可能找不到这个工具',
          available: false,
        }))
      return [...known, ...extras]
    }

    function skillChoices(): readonly Choice[] {
      const known: Choice[] = (catalogs.data.value?.skills ?? []).map((skill: SkillView) => ({
        name: skill.name,
        description: skill.description,
        meta: `已发布 v${skill.version_no} · ${skill.file_count} 个文件`,
        available: true,
      }))
      const knownNames = new Set(known.map((choice) => choice.name))
      const extras: Choice[] = skills.value
        .filter((skillName) => !knownNames.has(skillName))
        .map((skillName) => ({
          name: skillName,
          description: '',
          meta: '不在当前技能目录中：运行时可能找不到这个技能',
          available: false,
        }))
      return [...known, ...extras]
    }

    function mcpChoices(): readonly Choice[] {
      const known: Choice[] = (catalogs.data.value?.mcpServers ?? []).map((server: McpServerView) => ({
        name: server.name,
        description: server.tools.length === 0 ? '' : `工具：${server.tools.join('、')}`,
        meta: [
          server.transport === 'stdio' ? 'stdio' : 'HTTP',
          server.connected ? '已连接' : server.pending_restart ? '待重启' : '未连接',
        ].join(' · '),
        available: server.connected,
      }))
      const knownNames = new Set(known.map((choice) => choice.name))
      const extras: Choice[] = mcpServers.value
        .filter((serverName) => !knownNames.has(serverName))
        .map((serverName) => ({
          name: serverName,
          description: '',
          meta: '不在当前 MCP 目录中：运行时可能无法连接',
          available: false,
        }))
      return [...known, ...extras]
    }

    function addUnlisted(
      raw: string,
      current: readonly string[],
      update: (next: string[]) => void,
    ): void {
      const trimmed = raw.trim()
      if (trimmed === '' || current.includes(trimmed)) {
        return
      }
      update([...current, trimmed])
    }

    // --- 模型连接选择 -------------------------------------------------------

    /**
     * 选择行只回答四件事：名称、默认模型、共享 / 专属、本地配置就绪。
     * 地址、凭据、版本、所有者与引用者在资源详情页（R7）。
     * The picker row shows name, default model, scope and local readiness only.
     */
    function modelMeta(connection: ModelConnectionView): string {
      const scope = connection.scope === 'shared' ? '共享' : '专属 · 本 Agent'
      return `${scope} · ${AVAILABILITY_LABELS[connection.availability]}`
    }

    function modelMatches(connection: ModelConnectionView, query: string): boolean {
      if (query === '') {
        return true
      }
      return (
        connection.name.toLowerCase().includes(query) ||
        connection.default_model.toLowerCase().includes(query) ||
        connection.base_url.toLowerCase().includes(query)
      )
    }

    function filteredModels(): readonly ModelConnectionView[] {
      const query = providerQuery.value.trim().toLowerCase()
      return modelPool().filter(
        (connection) =>
          connection.resource_uid !== providerUID.value && modelMatches(connection, query),
      )
    }

    function onModelSelect(connection: ModelConnectionView): void {
      if (busy.value) {
        return
      }
      providerUID.value = connection.resource_uid
      providerName.value = connection.name
      // 选择连接时预填它的默认模型；之后仍可改模型与温度（R4.2 流程 A）。
      providerModel.value = connection.default_model
    }

    function modelRow(connection: ModelConnectionView, selected: boolean): VNode {
      return h(
        'button',
        {
          type: 'button',
          class: [
            'picker-row',
            selected ? 'selected' : '',
            connection.credential_state === 'revoked' ? 'unavailable' : '',
          ],
          'data-uid': connection.resource_uid,
          'aria-pressed': selected,
          disabled: busy.value,
          onClick: () => {
            onModelSelect(connection)
          },
        },
        [
          h('span', { class: 'picker-title' }, [
            h('code', {}, connection.name),
            selected ? h('span', { class: 'tag tag-ok' }, '当前') : null,
          ]),
          h('span', { class: 'picker-meta muted' }, modelMeta(connection)),
          h(
            'span',
            { class: 'picker-desc muted' },
            `默认模型 ${connection.default_model}`,
          ),
        ],
      )
    }

    function modelPicker(): VNode {
      const selected = selectedModel()
      const rows: VNode[] = []
      if (selected !== null) {
        rows.push(modelRow(selected, true))
      } else if (providerUID.value !== '') {
        rows.push(
          h('p', { class: 'notice notice-warn' }, [
            `当前选择的模型连接 ${providerName.value === '' ? '（未命名）' : providerName.value} 不可用（连接已删除、未发布或不在本 Agent 的可用范围）：`,
            '没有按名称自动关联；请从下面重新选择一个连接。',
          ]),
        )
      }
      const matches = filteredModels()
      for (const connection of matches) {
        rows.push(modelRow(connection, false))
      }
      if (selected === null && matches.length === 0) {
        rows.push(
          h(
            'p',
            { class: 'muted' },
            modelPool().length === 0
              ? '还没有可用（已发布且对本 Agent 有资格）的模型连接；可以在下面为当前 Agent 新建一个专属连接。'
              : '没有匹配的连接；清除搜索词或换个关键字。',
          ),
        )
      }
      return h('div', { class: 'field', id: 'config-provider' }, [
        h('span', {}, '模型连接'),
        h('input', {
          id: 'config-provider-search',
          type: 'search',
          value: providerQuery.value,
          placeholder: '搜索连接名称、默认模型或 Base URL',
          autocomplete: 'off',
          disabled: busy.value,
          onInput: (event: Event) => {
            providerQuery.value = (event.target as HTMLInputElement).value
          },
        }),
        h('div', { class: 'picker-list', 'aria-label': '模型连接候选项' }, rows),
        h(
          'span',
          { class: 'field-hint muted' },
          '只列已发布且有使用资格的连接；选择后会把连接名称与 UID 一并写入 resource_bindings，服务端重新校验。',
        ),
      ])
    }

    /** 候选列表读取失败时两个分区都不可靠：放在模型分区顶部统一提示。 */
    function candidatesWarning(): VNodeChild {
      if (candidates.error.value === '') {
        return null
      }
      return h(Notice, {
        tone: 'warn',
        text: `读取资源候选失败：${candidates.error.value}；模型与通知列表可能不完整，请点「重新读取」。`,
      })
    }

    // --- 输出随机性（temperature）-------------------------------------------

    const originalTemperature = computed(() => config.data.value?.data.fields.provider.temperature ?? null)

    function temperatureDirty(current: number | null): boolean {
      if (temperatureMode.value === 'default') {
        return current !== null
      }
      const raw = temperatureText.value.trim()
      if (raw === '') {
        return true
      }
      const parsed = Number(raw)
      return !Number.isFinite(parsed) || parsed !== current
    }

    function temperatureField(): VNode {
      const current = originalTemperature.value
      const custom = temperatureMode.value === 'custom'
      return h('div', { class: 'field' }, [
        h('span', {}, '输出随机性（temperature）'),
        h('div', { class: 'radio-list', role: 'radiogroup', 'aria-label': '输出随机性设置' }, [
          h('label', { class: 'checkbox-item', for: 'config-temperature-default' }, [
            h('input', {
              id: 'config-temperature-default',
              type: 'radio',
              name: 'config-temperature-mode',
              checked: !custom,
              disabled: busy.value,
              onChange: () => {
                temperatureMode.value = 'default'
              },
            }),
            h('span', {}, '使用模型默认值'),
          ]),
          h('label', { class: 'checkbox-item', for: 'config-temperature-custom' }, [
            h('input', {
              id: 'config-temperature-custom',
              type: 'radio',
              name: 'config-temperature-mode',
              checked: custom,
              disabled: busy.value,
              onChange: () => {
                temperatureMode.value = 'custom'
                if (temperatureText.value === '' && current !== null) {
                  temperatureText.value = String(current)
                }
              },
            }),
            h('span', {}, '自定义'),
          ]),
        ]),
        custom
          ? h('div', { class: 'inline-controls' }, [
              h('input', {
                id: 'config-temperature',
                type: 'number',
                min: '0',
                max: '2',
                step: '0.1',
                value: temperatureText.value,
                disabled: busy.value,
                onInput: (event: Event) => {
                  temperatureText.value = (event.target as HTMLInputElement).value
                },
              }),
              current !== null
                ? h(
                    'button',
                    {
                      type: 'button',
                      id: 'config-temperature-restore',
                      disabled: busy.value,
                      onClick: () => {
                        temperatureMode.value = 'default'
                        temperatureText.value = ''
                      },
                    },
                    '恢复默认',
                  )
                : null,
            ])
          : null,
        h(
          'span',
          { class: 'field-hint muted' },
          custom
            ? '0 到 2 的数值；0 是有效取值，但不等于「确定性采样」——相同输入也可能得到不同结果。保存时写入 AGENT.md 的 provider.temperature。'
            : current === null
              ? '当前未声明 temperature，由模型连接决定（不是默认 0）；保存不会写入该字段。'
              : `当前声明为 ${current}；保存后将移除该字段（提交 null），改由模型连接决定。`,
        ),
      ])
    }

    // --- 校验清单通用渲染 ----------------------------------------------------

    function catalogChecklist(
      prefix: string,
      choices: readonly Choice[],
      selected: readonly string[],
      onToggle: (name: string, checked: boolean) => void,
    ): VNode {
      return h(
        'div',
        { class: 'check-list', id: `${prefix}-choices` },
        choices.map((choice, index) =>
          h(
            'label',
            {
              key: choice.name,
              class: choice.available ? 'check-row' : 'check-row check-row-unavailable',
              for: `${prefix}-${index}`,
            },
            [
              h('input', {
                id: `${prefix}-${index}`,
                'data-name': choice.name,
                type: 'checkbox',
                checked: selected.includes(choice.name),
                disabled: busy.value,
                onChange: (event: Event) => {
                  onToggle(choice.name, (event.target as HTMLInputElement).checked)
                },
              }),
              h('span', { class: 'check-body' }, [
                h('span', { class: 'check-title' }, [
                  h('code', {}, choice.name),
                  choice.available ? null : h('span', { class: 'tag tag-warn' }, '不可用'),
                ]),
                choice.description === ''
                  ? null
                  : h('span', { class: 'check-desc' }, choice.description),
                h('span', { class: 'check-meta muted' }, choice.meta),
              ]),
            ],
          ),
        ),
      )
    }

    function addUnlistedRow(
      id: string,
      value: () => string,
      update: (next: string) => void,
      current: readonly string[],
      apply: (next: string[]) => void,
      placeholder: string,
    ): VNode {
      return h('div', { class: 'inline-controls' }, [
        h('input', {
          id,
          type: 'text',
          value: value(),
          placeholder,
          autocomplete: 'off',
          spellcheck: false,
          disabled: busy.value,
          onInput: (event: Event) => {
            update((event.target as HTMLInputElement).value)
          },
        }),
        h(
          'button',
          {
            type: 'button',
            disabled: busy.value,
            onClick: () => {
              addUnlisted(value(), current, (next) => {
                apply(next)
                update('')
              })
            },
          },
          '添加未列出的名称',
        ),
      ])
    }

    function listFallback(
      id: string,
      value: () => string,
      update: (next: string) => void,
      hint: string,
    ): VNode {
      return h('label', { class: 'field', for: id }, [
        h('span', {}, '手动填写（一行一项）'),
        h('textarea', {
          id,
          rows: 4,
          value: value(),
          spellcheck: false,
          disabled: busy.value,
          onInput: (event: Event) => {
            update((event.target as HTMLTextAreaElement).value)
          },
        }),
        h('span', { class: 'field-hint muted' }, hint),
      ])
    }

    function catalogNotice(): VNodeChild {
      if (catalogs.error.value === '') {
        return null
      }
      return h(Notice, {
        tone: 'warn',
        text:
          catalogs.data.value === null
            ? `读取工具、技能与 MCP 目录失败：${catalogs.error.value}；已降级为手动填写。`
            : `目录刷新失败：${catalogs.error.value}；下面显示的是上次读取的结果。`,
      })
    }

    // --- 工具 / 技能 / MCP 分区 ---------------------------------------------

    function toolsSubsection(): VNodeChild[] {
      const nodes: VNodeChild[] = [h('h3', { class: 'subheading' }, '工具（tools）')]
      if (!catalogReady.value) {
        if (catalogs.loading.value) {
          nodes.push(h('p', { class: 'muted' }, '正在读取工具目录…'))
        } else {
          nodes.push(
            listFallback(
              'config-tools',
              () => toolsText.value,
              (next) => (toolsText.value = next),
              '一行一个工具名。保存后新运行生效；只有这里声明的工具才会出现在运行中。',
            ),
          )
        }
        return nodes
      }
      const choices = toolChoices()
      nodes.push(
        choices.length === 0
          ? h('p', { class: 'muted' }, '当前没有可用工具。')
          : catalogChecklist('config-tool', choices, tools.value, (toolName, checked) => {
              tools.value = toggleName(tools.value, toolName, checked)
            }),
      )
      nodes.push(
        addUnlistedRow(
          'config-tool-add',
          () => toolAdd.value,
          (next) => (toolAdd.value = next),
          tools.value,
          (next) => (tools.value = next),
          '添加目录中未列出的工具名',
        ),
      )
      return nodes
    }

    function skillsSubsection(): VNodeChild[] {
      const nodes: VNodeChild[] = [h('h3', { class: 'subheading' }, '技能（skills）')]
      if (!catalogReady.value) {
        if (catalogs.loading.value) {
          nodes.push(h('p', { class: 'muted' }, '正在读取技能目录…'))
        } else {
          nodes.push(
            listFallback(
              'config-skills',
              () => skillsText.value,
              (next) => (skillsText.value = next),
              '一行一个技能名。声明名必须已有实体；没有实体的声明会让该 Agent 整只不注册。',
            ),
          )
        }
        return nodes
      }
      const choices = skillChoices()
      nodes.push(
        choices.length === 0
          ? h('p', { class: 'muted' }, '当前没有可用技能；可在「技能」资源页导入并发布后再回来选择。')
          : catalogChecklist('config-skill', choices, skills.value, (skillName, checked) => {
              skills.value = toggleName(skills.value, skillName, checked)
            }),
      )
      nodes.push(
        addUnlistedRow(
          'config-skill-add',
          () => skillAdd.value,
          (next) => (skillAdd.value = next),
          skills.value,
          (next) => (skills.value = next),
          '添加目录中未列出的技能名',
        ),
      )
      return nodes
    }

    function mcpSubsection(): VNodeChild[] {
      const nodes: VNodeChild[] = [h('h3', { class: 'subheading' }, 'MCP 服务（mcp_servers）')]
      if (!catalogReady.value) {
        if (catalogs.loading.value) {
          nodes.push(h('p', { class: 'muted' }, '正在读取 MCP 目录…'))
        } else {
          nodes.push(
            listFallback(
              'config-mcp',
              () => mcpText.value,
              (next) => (mcpText.value = next),
              '一行一个 MCP 服务名。服务定义在「MCP 服务」资源页维护。',
            ),
          )
        }
        return nodes
      }
      const choices = mcpChoices()
      nodes.push(
        choices.length === 0
          ? h('p', { class: 'muted' }, '当前没有 MCP 服务；可在「MCP 服务」资源页新建并发布后再回来选择。')
          : catalogChecklist('config-mcp', choices, mcpServers.value, (serverName, checked) => {
              mcpServers.value = toggleName(mcpServers.value, serverName, checked)
            }),
      )
      nodes.push(
        addUnlistedRow(
          'config-mcp-add',
          () => mcpAdd.value,
          (next) => (mcpAdd.value = next),
          mcpServers.value,
          (next) => (mcpServers.value = next),
          '添加目录中未列出的 MCP 服务名',
        ),
      )
      nodes.push(
        h(
          'p',
          { class: 'field-hint muted' },
          '未连接或「待重启」的 MCP 在连接成功前会让运行被拒绝；服务定义发布后如需重启加载，按资源页提示重启进程。',
        ),
      )
      return nodes
    }

    // --- 上下文文件（bootstrap）---------------------------------------------

    function addBootstrapPath(raw: string): void {
      const trimmed = raw.trim()
      if (trimmed === '' || bootstrap.value.includes(trimmed)) {
        return
      }
      bootstrap.value = [...bootstrap.value, trimmed]
    }

    function addBootstrapManual(): void {
      const raw = bootstrapPath.value
        .trim()
        .replace(/\\/g, '/')
        .replace(/^\.\/+/, '')
        .replace(/^\/+/, '')
      const normalized = raw.startsWith('.basalt/') ? raw.slice('.basalt/'.length) : raw
      if (normalized !== '') {
        addBootstrapPath(normalized)
        bootstrapPath.value = ''
      }
    }

    async function loadBootstrapDir(path: string): Promise<void> {
      bootstrapLoading.value = true
      bootstrapError.value = ''
      try {
        const entries = await admin.getWorkspaceTree(path)
        bootstrapDir.value = path
        bootstrapEntries.value = entries
        bootstrapLoaded.value = true
      } catch (caught) {
        bootstrapError.value = errorMessage(caught)
      } finally {
        bootstrapLoading.value = false
      }
    }

    function toggleBootstrapPicker(): void {
      bootstrapOpen.value = !bootstrapOpen.value
      if (bootstrapOpen.value && !bootstrapLoaded.value && !bootstrapLoading.value) {
        void loadBootstrapDir('')
      }
    }

    function bootstrapParent(): string {
      const parts = bootstrapDir.value.split('/').filter((part) => part !== '')
      parts.pop()
      return parts.join('/')
    }

    function sortedBootstrapEntries(): readonly WorkspaceEntry[] {
      return [...bootstrapEntries.value].sort((left, right) => {
        if (left.type !== right.type) {
          return left.type === 'dir' ? -1 : 1
        }
        return left.path.localeCompare(right.path)
      })
    }

    function bootstrapPicker(): VNodeChild {
      if (!bootstrapOpen.value) {
        return null
      }
      const rows: VNode[] = []
      if (bootstrapDir.value !== '') {
        rows.push(
          h(
            'button',
            {
              type: 'button',
              class: 'tree-row',
              disabled: bootstrapLoading.value,
              onClick: () => {
                void loadBootstrapDir(bootstrapParent())
              },
            },
            '↰ 上一级',
          ),
        )
      }
      for (const entry of sortedBootstrapEntries()) {
        if (entry.type === 'dir') {
          rows.push(
            h(
              'button',
              {
                key: entry.path,
                type: 'button',
                class: 'tree-row',
                disabled: bootstrapLoading.value,
                onClick: () => {
                  void loadBootstrapDir(entry.path)
                },
              },
              [h('span', { class: 'tree-icon' }, '▸'), ` ${entry.path}/`],
            ),
          )
        } else if (entry.type === 'file') {
          const workspacePath = `agents/${entry.path}`
          rows.push(
            h(
              'div',
              { key: entry.path, class: 'tree-row tree-file' },
              [
                h('span', { class: 'tree-path' }, [
                  h('code', {}, workspacePath),
                  h('span', { class: 'muted' }, ` · ${formatBytes(entry.size)}`),
                ]),
                h(
                  'button',
                  {
                    type: 'button',
                    disabled: busy.value || bootstrap.value.includes(workspacePath),
                    onClick: () => {
                      addBootstrapPath(workspacePath)
                    },
                  },
                  bootstrap.value.includes(workspacePath) ? '已加入' : '加入上下文文件',
                ),
              ],
            ),
          )
        } else {
          rows.push(
            h(
              'div',
              { key: entry.path, class: 'tree-row tree-file' },
              [
                h('span', { class: 'tree-path' }, [h('code', {}, entry.path)]),
                h('span', { class: 'muted' }, '软链接（不跟随，不可选）'),
              ],
            ),
          )
        }
      }
      return h('div', { class: 'tree-picker card' }, [
        h('div', { class: 'tree-head' }, [
          h('strong', {}, bootstrapDir.value === '' ? '工作区根（.basalt/agents）' : `agents/${bootstrapDir.value}`),
          h(
            'button',
            {
              type: 'button',
              disabled: bootstrapLoading.value,
              onClick: () => {
                void loadBootstrapDir(bootstrapDir.value)
              },
            },
            '刷新',
          ),
        ]),
        bootstrapLoading.value ? h('p', { class: 'muted' }, '正在读取目录…') : null,
        bootstrapError.value === ''
          ? null
          : h(Notice, { tone: 'warn', text: `读取目录失败：${bootstrapError.value}` }),
        rows.length === 0 && !bootstrapLoading.value && bootstrapError.value === ''
          ? h('p', { class: 'muted' }, '这个目录为空。')
          : null,
        h(
          'p',
          { class: 'field-hint muted' },
          '文件树只能浏览 .basalt/agents；上下文文件也可以指向 .basalt 下的其它文件，请用上面的路径输入手动添加。',
        ),
      ])
    }

    function bootstrapSection(): VNodeChild[] {
      const nodes: VNodeChild[] = [h('h3', { class: 'subheading' }, '上下文文件（bootstrap）')]
      if (bootstrap.value.length === 0) {
        nodes.push(h('p', { class: 'muted' }, '未配置上下文文件；首轮直接使用系统提示。'))
      } else {
        nodes.push(
          h(
            'ul',
            { class: 'plain-list' },
            bootstrap.value.map((path) =>
              h('li', { key: path }, [
                h('code', {}, path),
                ' ',
                h(
                  'button',
                  {
                    type: 'button',
                    class: 'link-button',
                    disabled: busy.value,
                    onClick: () => {
                      bootstrap.value = bootstrap.value.filter((item) => item !== path)
                    },
                  },
                  '移除',
                ),
              ]),
            ),
          ),
        )
      }
      nodes.push(
        h('div', { class: 'field' }, [
          h('label', { for: 'config-bootstrap-path' }, '工作区相对路径'),
          h('div', { class: 'inline-controls' }, [
            h('input', {
              id: 'config-bootstrap-path',
              type: 'text',
              value: bootstrapPath.value,
              placeholder: '例如 agents/writer/AGENT.md',
              autocomplete: 'off',
              spellcheck: false,
              disabled: busy.value,
              onInput: (event: Event) => {
                bootstrapPath.value = (event.target as HTMLInputElement).value
              },
            }),
            h(
              'button',
              {
                type: 'button',
                disabled: busy.value,
                onClick: addBootstrapManual,
              },
              '添加文件',
            ),
            h(
              'button',
              {
                type: 'button',
                disabled: busy.value,
                onClick: toggleBootstrapPicker,
              },
              bootstrapOpen.value ? '收起文件树' : '从工作区选择…',
            ),
          ]),
        ]),
      )
      nodes.push(bootstrapPicker())
      nodes.push(
        h(
          'p',
          { class: 'field-hint muted' },
          '路径以工作区根 .basalt 为基准。这里列出的文件在每轮按列表顺序全文加载，追加到系统提示末尾；' +
            '缺失或不是普通文件会让 Agent 注册被拒绝。',
        ),
      )
      return nodes
    }

    // --- 通知 ---------------------------------------------------------------

    interface NotifyChoice {
      readonly name: string
      readonly uid: string
      readonly description: string
      readonly meta: string
      readonly available: boolean
    }

    function channelMeta(channel: NotifyChannelView): string {
      const scope = channel.scope === 'shared' ? '共享' : '专属 · 本 Agent'
      const revoked = channel.credential_state === 'revoked' ? ' · 凭据已撤销' : ''
      const address =
        channel.address_status === 'empty'
          ? ' · 地址未配置'
          : channel.address_status === 'managed_unavailable'
            ? ' · 加密托管地址不可用'
            : channel.address_status === 'env'
              ? ' · 环境变量（发送时解析）'
              : ' · 地址已配置'
      return `${channel.type || '未知类型'} · ${scope}${revoked}${address}`
    }

    function notifyChoices(): readonly NotifyChoice[] {
      const choices: NotifyChoice[] = channelPool().map((channel) => ({
        name: channel.name,
        uid: channel.resource_uid,
        description: channel.description,
        meta: channelMeta(channel),
        available: true,
      }))
      for (const channelName of notifyChannelNames.value) {
        if (choices.some((choice) => choice.name === channelName)) {
          continue
        }
        const reference = references.value.find(
          (item) => item.resource_kind === 'notify_channel' && item.ref_name === channelName,
        )
        choices.push({
          name: channelName,
          uid: reference?.resource_uid ?? '',
          description: '',
          meta: '当前声明不可用：资源缺失或无资格',
          available: false,
        })
      }
      return choices
    }

    function toggleNotifyChoice(choice: NotifyChoice, checked: boolean): void {
      const rest = notifyChannelNames.value.filter((channelName) => channelName !== choice.name)
      notifyChannelNames.value = checked ? [...rest, choice.name] : rest
      const picks = { ...notifyPicks.value }
      if (checked) {
        picks[choice.name] = choice.uid
      } else {
        delete picks[choice.name]
      }
      notifyPicks.value = picks
    }

    function notifyModeChoices(): VNode {
      const options = (['none', 'all_eligible', 'allowlist'] as const).map((mode) =>
        h('label', { key: mode, class: 'checkbox-item', for: `config-notify-mode-${mode}` }, [
          h('input', {
            id: `config-notify-mode-${mode}`,
            type: 'radio',
            name: 'config-notify-mode',
            value: mode,
            checked: notifyMode.value === mode,
            disabled: busy.value,
            onChange: () => {
              notifyMode.value = mode
            },
          }),
          h('span', {}, NOTIFY_MODE_LABELS[mode]),
        ]),
      )
      return h('div', { id: 'config-notify-mode', class: 'checkbox-list', role: 'radiogroup', 'aria-label': '通知模式' }, options)
    }

    function notifyChannelChoices(): VNodeChild {
      if (notifyMode.value !== 'allowlist') {
        return h('p', { class: 'muted' }, '当前模式不使用声明列表；切换到「仅使用所选渠道」后可勾选。')
      }
      const choices = notifyChoices()
      if (choices.length === 0) {
        return h('p', { class: 'muted' }, '工作区还没有可用渠道；可以在下面就地新建。')
      }
      return h(
        'div',
        { class: 'check-list' },
        choices.map((choice, index) =>
          h(
            'label',
            {
              key: choice.name,
              class: choice.available ? 'check-row' : 'check-row check-row-unavailable',
              for: `notify-${index}`,
            },
            [
              h('input', {
                id: `notify-${index}`,
                'data-name': choice.name,
                type: 'checkbox',
                checked: notifyChannelNames.value.includes(choice.name),
                disabled: busy.value,
                onChange: (event: Event) => {
                  toggleNotifyChoice(choice, (event.target as HTMLInputElement).checked)
                },
              }),
              h('span', { class: 'check-body' }, [
                h('span', { class: 'check-title' }, [
                  h('code', {}, choice.name),
                  choice.available ? null : h('span', { class: 'tag tag-warn' }, '不可用'),
                ]),
                choice.description === ''
                  ? null
                  : h('span', { class: 'check-desc' }, choice.description),
                h('span', { class: 'check-meta muted' }, choice.meta),
              ]),
            ],
          ),
        ),
      )
    }

    /**
     * 通知意图与 tools 的关系（R7 / 宪法约束 11）：保存时系统按意图自动增删，且只
     * 增删 notify 一个工具。这里只预告系统会做什么，不预判渠道资格——资格不足时
     * 系统不改 tools，由保存期绑定校验点名具体问题。
     * Preview what the save-time assembly will do; eligibility itself stays server-side.
     */
    function notifyToolRelation(): string {
      const hasNotify = effectiveTools.value.includes('notify')
      if (notifyMode.value === 'none') {
        return hasNotify
          ? '通知意图为 none：保存时系统会从 tools 移除 notify，运行中不向模型提供该工具。'
          : ''
      }
      if (hasNotify) {
        return 'notify 已在 tools 中：模型可在运行中主动调用；定时任务配置了结果投递渠道时，该轮运行由系统发送，不向模型提供 notify。'
      }
      return '保存时系统会按通知意图自动把 notify 加入 tools（渠道存在且可用时）；系统只会自动增删 notify 这一个工具。'
    }

    /** 保存结果里的系统工具变更（只可能是 notify）；空数组 = 本次没有自动调整。 */
    // The system-applied tool delta of one save; an empty delta renders nothing.
    function systemToolChange(change: AgentToolChangeView): string {
      const parts: string[] = []
      if (change.tools_added.length > 0) {
        parts.push(`加入 ${change.tools_added.join('、')}`)
      }
      if (change.tools_removed.length > 0) {
        parts.push(`移除 ${change.tools_removed.join('、')}`)
      }
      return parts.length === 0 ? '' : `系统已按通知意图自动调整 tools：${parts.join('；')}。`
    }

    function newChannelBody(): NotifyChannelUpsert | null {
      const channelName = newChannelName.value.trim()
      const channelType = newChannelType.value.trim()
      if (channelName === '') {
        error.value = '请填写渠道名称'
        return null
      }
      if (channelType === '') {
        error.value = '请选择渠道类型'
        return null
      }
      const raw = newChannelURL.value.trim()
      if (raw === '') {
        error.value = newChannelMode.value === 'managed' ? '请填写 Webhook 地址' : '请填写环境变量名'
        return null
      }
      const body: NotifyChannelUpsert = {
        name: channelName,
        type: channelType,
        description: newChannelDescription.value.trim(),
        credential_mode: newChannelMode.value,
        scope: newChannelScope.value,
      }
      if (newChannelScope.value === 'exclusive') {
        body.owner_agent = name
      }
      if (newChannelMode.value === 'managed') {
        body.url = raw
      } else {
        const reference = envReference(raw)
        if (reference === null) {
          error.value = '地址只接受 ${ENV_VAR} 占位，例如 ${ALERT_WEBHOOK}；明文地址不会被提交'
          return null
        }
        body.url = reference
      }
      return body
    }

    function resetNewChannel(): void {
      showNewChannel.value = false
      newChannelName.value = ''
      newChannelType.value = ''
      newChannelURL.value = ''
      newChannelMode.value = 'managed'
      newChannelScope.value = 'exclusive'
      newChannelDescription.value = ''
    }

    /**
     * 创建渠道 → 重新读取候选 → 加入「仅使用所选渠道」名单 → 保存 Agent 配置写入绑定。
     * 与连接一样：创建成功而保存失败时，渠道已存在，页面只说“尚未写入配置”。
     */
    async function createChannelAndBind(): Promise<void> {
      if (busy.value) {
        return
      }
      resetFeedback()
      if (rawDirty.value) {
        error.value = '配置原文有未保存修改：请先保存或放弃原文修改，再新建渠道并关联'
        return
      }
      const body = newChannelBody()
      if (body === null) {
        return
      }
      busy.value = true
      let created: NotifyChannelView
      try {
        created = await admin.createNotifyChannel(body)
      } catch (caught) {
        problems.value = problemsOf(caught)
        error.value = errorMessage(caught)
        busy.value = false
        return
      }
      await candidates.reload()
      notifyMode.value = 'allowlist'
      notifyChannelNames.value = [...notifyChannelNames.value.filter((item) => item !== created.name), created.name]
      notifyPicks.value = { ...notifyPicks.value, [created.name]: created.resource_uid }
      resetNewChannel()
      busy.value = false
      await save({ createdResource: { kind: '通知渠道', name: created.name } })
    }

    function channelTypeField(): VNode {
      const types = notifyTypes.data.value ?? []
      if (types.length === 0) {
        return textField(
          'config-new-channel-type',
          '渠道类型',
          () => newChannelType.value,
          (next) => (newChannelType.value = next),
          {
            placeholder: '例如 webhook',
            hint: notifyTypes.loading.value
              ? '正在读取类型表…'
              : notifyTypes.error.value !== ''
                ? `读取渠道类型失败，暂以文本输入：${notifyTypes.error.value}`
                : '类型表为空，请直接输入类型名',
          },
        )
      }
      const options: VNode[] = [h('option', { key: '', value: '' }, '请选择类型')]
      for (const item of types) {
        options.push(h('option', { key: item, value: item }, item))
      }
      return h('label', { class: 'field', for: 'config-new-channel-type' }, [
        h('span', '渠道类型'),
        h(
          'select',
          {
            id: 'config-new-channel-type',
            value: newChannelType.value,
            disabled: busy.value,
            onChange: (event: Event) => {
              newChannelType.value = (event.target as HTMLSelectElement).value
            },
          },
          options,
        ),
      ])
    }

    function channelCreatePanel(): VNodeChild {
      if (!showNewChannel.value) {
        return null
      }
      return h('div', { class: 'inline-create card' }, [
        h('h3', '为当前 Agent 新建通知渠道'),
        textField(
          'config-new-channel-name',
          '渠道名称',
          () => newChannelName.value,
          (next) => (newChannelName.value = next),
          { placeholder: '例如 writer-alerts' },
        ),
        channelTypeField(),
        h(CredentialField, {
          id: 'config-new-channel-url',
          label: 'Webhook 地址',
          noun: '地址',
          mode: newChannelMode.value,
          value: newChannelURL.value,
          editableMode: true,
          disabled: busy.value,
          placeholder:
            newChannelMode.value === 'managed' ? 'https://hooks.example.com/…' : 'ALERT_WEBHOOK 或 ${ALERT_WEBHOOK}',
          'onUpdate:value': (next: string) => {
            newChannelURL.value = next
          },
          'onUpdate:mode': (next: CredentialMode) => {
            newChannelMode.value = next
          },
        }),
        h(ScopePicker, {
          id: 'config-new-channel-scope',
          scope: newChannelScope.value,
          ownerAgent: name,
          agents: [name],
          disabled: busy.value,
          'onUpdate:scope': (next: ResourceScope) => {
            newChannelScope.value = next
          },
        }),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: busy.value,
              onClick: () => {
                void createChannelAndBind()
              },
            },
            busy.value ? '创建中…' : '创建渠道并关联',
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value,
              onClick: () => {
                resetNewChannel()
                resetFeedback()
              },
            },
            '取消',
          ),
        ]),
      ])
    }

    // --- 就地新建连接 -------------------------------------------------------

    function prefillConnection(uid: string): void {
      const source = allModels().find((connection) => connection.resource_uid === uid)
      if (source === undefined) {
        return
      }
      // 只预填非密文字段：Base URL 与默认模型；密钥永远不跨资源读取。
      newConnectionBaseURL.value = source.base_url
      newConnectionModel.value = source.default_model
    }

    function newConnectionBody(): ModelConnectionUpsert | null {
      const connectionName = newConnectionName.value.trim()
      const baseURL = newConnectionBaseURL.value.trim()
      const defaultModel = newConnectionModel.value.trim()
      if (connectionName === '' || baseURL === '' || defaultModel === '') {
        error.value = '请填写连接名称、Base URL 与默认模型'
        return null
      }
      const raw = newConnectionKey.value.trim()
      if (raw === '') {
        error.value =
          newConnectionMode.value === 'managed'
            ? '请填写完整 API 密钥（加密托管）；保存后不回显原值'
            : '请填写环境变量名（例如 DEEPSEEK_API_KEY）；真实密钥由部署者在进程环境中配置'
        return null
      }
      const body: ModelConnectionUpsert = {
        name: connectionName,
        base_url: baseURL,
        default_model: defaultModel,
        credential_mode: newConnectionMode.value,
        scope: newConnectionScope.value,
      }
      if (newConnectionScope.value === 'exclusive') {
        body.owner_agent = name
      }
      if (newConnectionMode.value === 'managed') {
        body.api_key = raw
      } else {
        const reference = envReference(raw)
        if (reference === null) {
          error.value =
            '这里只填写环境变量名（例如 DEEPSEEK_API_KEY），不填写真实密钥；' +
            '环境变量名只能包含字母、数字与下划线，且不能以数字开头'
          return null
        }
        body.api_key = reference
      }
      return body
    }

    function resetNewConnection(): void {
      showNewConnection.value = false
      newConnectionName.value = ''
      newConnectionBaseURL.value = ''
      newConnectionModel.value = ''
      newConnectionKey.value = ''
      newConnectionMode.value = 'managed'
      newConnectionScope.value = 'exclusive'
      newConnectionPrefillUID.value = ''
    }

    /**
     * 创建连接（创建即发布）→ 重新读取候选 → 回填选择 → 保存 Agent 配置写入绑定。
     * 连接与绑定是两步：第二步失败时连接已经存在，文案必须说明可重试而不是创建失败。
     */
    async function createConnectionAndBind(): Promise<void> {
      if (busy.value) {
        return
      }
      resetFeedback()
      if (rawDirty.value) {
        error.value = '配置原文有未保存修改：请先保存或放弃原文修改，再新建连接并关联'
        return
      }
      const body = newConnectionBody()
      if (body === null) {
        return
      }
      busy.value = true
      let created: ModelConnectionView
      try {
        created = await admin.createModelConnection(body)
      } catch (caught) {
        problems.value = problemsOf(caught)
        error.value = errorMessage(caught)
        busy.value = false
        return
      }
      await candidates.reload()
      providerUID.value = created.resource_uid
      providerName.value = created.name
      providerModel.value = created.default_model
      resetNewConnection()
      busy.value = false
      await save({ createdResource: { kind: '模型连接', name: created.name } })
    }

    function connectionCreatePanel(): VNodeChild {
      if (!showNewConnection.value) {
        return null
      }
      const prefillChoices: VNode[] = [h('option', { key: '', value: '' }, '不从已有连接预填')]
      for (const connection of allModels()) {
        prefillChoices.push(
          h('option', { key: connection.resource_uid, value: connection.resource_uid }, connection.name),
        )
      }
      return h('div', { class: 'inline-create card' }, [
        h('h3', '为当前 Agent 新建模型连接'),
        h(
          'p',
          { class: 'muted' },
          '要使用自己的 API 密钥，请为当前 Agent 新建一个专属连接。修改共享连接会影响所有使用它的 Agent。',
        ),
        h('label', { class: 'field', for: 'config-new-connection-prefill' }, [
          h('span', '从已有连接预填（只预填 Base URL 与默认模型）'),
          h(
            'select',
            {
              id: 'config-new-connection-prefill',
              value: newConnectionPrefillUID.value,
              disabled: busy.value,
              onChange: (event: Event) => {
                const uid = (event.target as HTMLSelectElement).value
                newConnectionPrefillUID.value = uid
                prefillConnection(uid)
              },
            },
            prefillChoices,
          ),
        ]),
        textField(
          'config-new-connection-name',
          '连接名称',
          () => newConnectionName.value,
          (next) => (newConnectionName.value = next),
          { placeholder: '例如 writer-private' },
        ),
        textField(
          'config-new-connection-base-url',
          'Base URL',
          () => newConnectionBaseURL.value,
          (next) => (newConnectionBaseURL.value = next),
          { placeholder: 'https://api.example.com/v1' },
        ),
        textField(
          'config-new-connection-model',
          '默认模型',
          () => newConnectionModel.value,
          (next) => (newConnectionModel.value = next),
          { placeholder: '例如 deepseek-chat' },
        ),
        h(CredentialField, {
          id: 'config-new-connection-key',
          label: 'API 密钥',
          mode: newConnectionMode.value,
          value: newConnectionKey.value,
          editableMode: true,
          disabled: busy.value,
          placeholder:
            newConnectionMode.value === 'managed'
              ? '粘贴完整的 API 密钥（保存后不回显原值）'
              : 'DEEPSEEK_API_KEY 或 ${DEEPSEEK_API_KEY}',
          'onUpdate:value': (next: string) => {
            newConnectionKey.value = next
          },
          'onUpdate:mode': (next: CredentialMode) => {
            newConnectionMode.value = next
          },
        }),
        h(ScopePicker, {
          id: 'config-new-connection-scope',
          scope: newConnectionScope.value,
          ownerAgent: name,
          agents: [name],
          disabled: busy.value,
          'onUpdate:scope': (next: ResourceScope) => {
            newConnectionScope.value = next
          },
        }),
        h(
          'p',
          { class: 'muted small' },
          '保存即受控发布：新连接对工作区（共享）或当前 Agent（专属）立即可用。创建成功后会把当前表单的改动一并保存。',
        ),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: busy.value,
              onClick: () => {
                void createConnectionAndBind()
              },
            },
            busy.value ? '创建中…' : '创建连接并关联',
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value,
              onClick: () => {
                resetNewConnection()
                resetFeedback()
              },
            },
            '取消',
          ),
        ]),
      ])
    }

    // --- 保存 ---------------------------------------------------------------

    function parseInteger(value: string, label: string): number | null | undefined {
      const trimmed = value.trim()
      if (trimmed === '') {
        return undefined
      }
      const parsed = Number(trimmed)
      if (!Number.isInteger(parsed) || parsed < 1) {
        error.value = `${label}必须是正整数`
        return null
      }
      return parsed
    }

    function integerDraftDiffers(text: string, current: number): boolean {
      const trimmed = text.trim()
      if (trimmed === '') {
        return true
      }
      const parsed = Number(trimmed)
      return !Number.isInteger(parsed) || parsed < 1 || parsed !== current
    }

    /** 绑定整块按“保存后的引用集合”组装：模型 1 条 + 声明渠道各 1 条。 */
    function desiredBindings(): AgentContentBinding[] | null {
      const bindings: AgentContentBinding[] = []
      if (providerName.value !== '') {
        if (providerUID.value === '') {
          return null
        }
        bindings.push({
          resource_kind: 'model_connection',
          ref_name: providerName.value,
          resource_uid: providerUID.value,
        })
      }
      if (notifyMode.value === 'allowlist') {
        for (const channelName of notifyChannelNames.value) {
          const uid = notifyUID(channelName)
          if (uid === '') {
            return null
          }
          bindings.push({ resource_kind: 'notify_channel', ref_name: channelName, resource_uid: uid })
        }
      }
      return bindings
    }

    /** 组装补丁：未改动的字段一律不出现，绑定显式列出。返回 null 表示本地校验失败。 */
    function buildChanges(view: AgentConfigView): AgentContentChanges | null {
      const fields = view.fields
      const changes: AgentContentChanges = {}

      if (description.value.trim() !== fields.description) {
        changes.description = description.value.trim()
      }
      // 显示名三态：未改不出现；改为空串 = 移除该行；非空 = 写入。
      // An empty draft removes the key; an untouched draft omits it entirely.
      if (displayName.value.trim() !== fields.display_name) {
        changes.display_name = displayName.value.trim()
      }
      if (persona.value.trim() !== fields.identity.agent_name) {
        changes.identity = { ...(changes.identity ?? {}), agent_name: persona.value.trim() }
      }
      if (prompt.value !== fields.identity.prompt) {
        changes.identity = { ...(changes.identity ?? {}), prompt: prompt.value }
      }
      if (bodyDirty.value) {
        changes.body = body.value
      }
      const provider: { name?: string; model?: string; temperature?: number | null } = {}
      if (providerName.value !== fields.provider.name) {
        provider.name = providerName.value
      }
      if (providerModel.value.trim() !== fields.provider.model) {
        provider.model = providerModel.value.trim()
      }
      if (temperatureMode.value === 'default') {
        // 省略 = 不修改；null = 移除字段回到供应商缺省（不是“清空字符串”）。
        if (fields.provider.temperature !== null) {
          provider.temperature = null
        }
      } else {
        const raw = temperatureText.value.trim()
        if (raw === '') {
          error.value = '自定义输出随机性需要填写 0 到 2 之间的数字；要使用模型默认值请选择「使用模型默认值」'
          return null
        }
        const parsed = Number(raw)
        if (!Number.isFinite(parsed) || parsed < 0 || parsed > 2) {
          error.value = 'temperature 必须是 0 到 2 之间的数字；要使用模型默认值请选择「使用模型默认值」'
          return null
        }
        if (parsed !== fields.provider.temperature) {
          provider.temperature = parsed
        }
      }
      if (Object.keys(provider).length > 0) {
        changes.provider = provider
      }
      const nextTools = effectiveTools.value
      if (!sameList(nextTools, fields.tools)) {
        changes.tools = [...nextTools]
      }
      const nextSkills = effectiveSkills.value
      if (!sameList(nextSkills, fields.skills)) {
        changes.skills = [...nextSkills]
      }
      const nextMcp = effectiveMcp.value
      if (!sameList(nextMcp, fields.mcp_servers)) {
        changes.mcp_servers = [...nextMcp]
      }
      if (!sameList(bootstrap.value, fields.bootstrap)) {
        changes.bootstrap = [...bootstrap.value]
      }
      const modeChanged = notifyMode.value !== view.notify_mode
      const declaredChannels = fields.notify_channels
      if (modeChanged) {
        changes.notify_mode = notifyMode.value
      }
      if (notifyMode.value === 'allowlist') {
        if (notifyChannelNames.value.length === 0) {
          error.value = '仅使用所选渠道时要至少选择一个通知渠道'
          return null
        }
        if (modeChanged || !sameList(notifyChannelNames.value, declaredChannels)) {
          changes.notify_channels = [...notifyChannelNames.value]
        }
      } else if (modeChanged) {
        // none / all_eligible 时声明列表必须为空（契约 AgentContentChanges）。
        changes.notify_channels = []
      }
      const maxIterations = parseInteger(maxIterationsText.value, '最大迭代次数')
      if (maxIterations === null) {
        return null
      }
      if (maxIterations !== undefined && maxIterations !== fields.settings.max_iterations) {
        changes.settings = { ...(changes.settings ?? {}), max_iterations: maxIterations }
      }
      const maxHistoryTurns = parseInteger(maxHistoryTurnsText.value, '最大历史轮数')
      if (maxHistoryTurns === null) {
        return null
      }
      if (
        maxHistoryTurns !== undefined &&
        maxHistoryTurns !== fields.settings.max_history_turns
      ) {
        changes.settings = { ...(changes.settings ?? {}), max_history_turns: maxHistoryTurns }
      }
      // 绑定显式提交（服务端不补绑、不按名猜 UID）；整块替换，孤儿绑定不参与。
      // 无变化时不发 bindings，避免每次保存都重排声明块。
      const bindings = desiredBindings()
      if (bindings === null) {
        return null
      }
      if (bindingsDiffer(currentBindings(view), bindings)) {
        changes.bindings = bindings
      }
      return changes
    }

    /** 表单是否有未保存修改（不含原文编辑器）。 */
    const formDirty = computed(() => {
      const view = config.data.value?.data
      if (view === null || view === undefined) {
        return false
      }
      const fields = view.fields
      if (description.value.trim() !== fields.description) {
        return true
      }
      if (displayName.value.trim() !== fields.display_name) {
        return true
      }
      if (persona.value.trim() !== fields.identity.agent_name) {
        return true
      }
      if (prompt.value !== fields.identity.prompt) {
        return true
      }
      if (bodyDirty.value) {
        return true
      }
      if (providerName.value !== fields.provider.name) {
        return true
      }
      if (providerModel.value.trim() !== fields.provider.model) {
        return true
      }
      if (temperatureDirty(fields.provider.temperature)) {
        return true
      }
      if (!sameList(effectiveTools.value, fields.tools)) {
        return true
      }
      if (!sameList(effectiveSkills.value, fields.skills)) {
        return true
      }
      if (!sameList(effectiveMcp.value, fields.mcp_servers)) {
        return true
      }
      if (!sameList(bootstrap.value, fields.bootstrap)) {
        return true
      }
      if (notifyMode.value !== view.notify_mode) {
        return true
      }
      if (
        notifyMode.value === 'allowlist' &&
        !sameList(notifyChannelNames.value, fields.notify_channels)
      ) {
        return true
      }
      if (integerDraftDiffers(maxIterationsText.value, fields.settings.max_iterations)) {
        return true
      }
      if (integerDraftDiffers(maxHistoryTurnsText.value, fields.settings.max_history_turns)) {
        return true
      }
      const bindings = desiredBindings()
      return bindings !== null && bindingsDiffer(currentBindings(view), bindings)
    })

    const rawDirty = computed(
      () => rawLoaded.value && rawEditor.value !== rawOriginal.value,
    )
    const dirty = computed(() => formDirty.value || rawDirty.value)

    watch(
      dirty,
      (value) => {
        emit('dirty-change', value)
      },
      { immediate: true },
    )

    async function save(context: SaveContext = {}): Promise<void> {
      const view = config.data.value
      if (busy.value || view === null) {
        return
      }
      const prefix =
        context.createdResource === undefined
          ? ''
          : `${context.createdResource.kind}「${context.createdResource.name}」已创建，但尚未写入 Agent 配置：`
      resetFeedback()
      if (rawDirty.value) {
        error.value = `${prefix}配置原文有未保存的修改：请先在「高级设置 → 配置原文」保存或放弃，再保存表单`
        return
      }
      if (candidates.data.value === null) {
        // 没有候选列表就无法判断绑定是否可用：宁可拦住，也不发出一个凭猜测的绑定。
        error.value = `${prefix}资源候选尚未读取成功：请点「重新读取」后再保存`
        return
      }
      if (blockingIssues.value.length > 0) {
        const first = blockingIssues.value[0]!
        const labels = blockingIssues.value.map((issue) => issue.label).join('、')
        error.value = `${prefix}${resourceErrorCopyOf(first.code)}（涉及：${labels}）`
        return
      }
      // 正文清空是不可逆的“看起来保存成功但指令没了”场景：第二次点击才提交。
      if (bodyDirty.value && body.value.trim() === '' && !emptyBodyArmed.value) {
        emptyBodyArmed.value = true
        notice.value = '正文将被清空（执行规则会消失）：再次点击「保存配置」确认。'
        return
      }
      const changes = buildChanges(view.data)
      if (changes === null) {
        const localReason = error.value
        error.value = `${prefix}${localReason}`
        return
      }
      if (Object.keys(changes).length === 0) {
        notice.value = '没有改动需要保存'
        return
      }
      busy.value = true
      try {
        // 表单与原文共用同一个 content_etag：条件写基准来自配置 GET 的 ETag。
        const updated = await api.updateAgentContent(name, { changes }, etag.value)
        // 重新读取（不是乐观更新）：表单回到服务端真值，绑定与正文一并刷新。
        await Promise.all([config.reload(), candidates.reload(), content.reload()])
        emptyBodyArmed.value = false
        // 关系变化随保存结果回显（系统自动增删只可能是 notify）；不在这里重算推断。
        const applied = systemToolChange(updated.changes)
        notice.value =
          (context.createdResource === undefined
            ? '已保存；配置与资源关联已从服务端重新读取。新运行使用这份配置，已开始的运行不受影响。'
            : `${context.createdResource.kind}「${context.createdResource.name}」已创建并写入 Agent 配置的资源关联。`) +
          applied
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 412) {
          // 412：文件已前移。只换 ETag，保留用户输入（服务端按字段合并）。
          error.value = `${prefix}AGENT.md 已被他人修改，本次保存没有写入；已重新读取 ETag，请确认后再提交`
          try {
            const fresh = await api.getAgentContent(name)
            etag.value = fresh.etag
          } catch {
            // ETag 刷新失败不覆盖主错误；再次保存会重新得到 412。
          }
          return
        }
        const copy = resourceErrorCopy(caught)
        error.value = `${prefix}${copy === '' ? errorMessage(caught) : copy}`
        problems.value = problemsOf(caught)
      } finally {
        busy.value = false
      }
    }

    // --- 配置原文 -----------------------------------------------------------

    async function saveRaw(): Promise<void> {
      if (rawSaving.value || !rawLoaded.value) {
        return
      }
      resetRawFeedback()
      if (formDirty.value) {
        rawError.value = '表单有未保存的修改：请先保存表单或点「重新读取」，再保存配置原文'
        return
      }
      if (!rawDirty.value) {
        rawNotice.value = '原文没有改动'
        return
      }
      if (
        rawEditor.value.trim() === '' &&
        !window.confirm('配置原文为空：保存会清空 AGENT.md 的全部内容（包括 frontmatter），确定继续？')
      ) {
        return
      }
      rawSaving.value = true
      try {
        const updated = await api.updateAgentContent(name, { content: rawEditor.value }, etag.value)
        rawOriginal.value = rawEditor.value
        etag.value = updated.etag
        rawNotice.value = '已保存原文；表单已从服务端重新读取。' + systemToolChange(updated.changes)
        await Promise.all([config.reload(), candidates.reload(), content.reload()])
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 412) {
          rawError.value = 'AGENT.md 已被他人修改，本次保存没有写入；请先重新读取，再基于新内容修改'
          try {
            const fresh = await api.getAgentContent(name)
            etag.value = fresh.etag
            rawOriginal.value = fresh.content
          } catch {
            // ETag 刷新失败不覆盖主错误。
          }
          return
        }
        rawError.value = errorMessage(caught)
      } finally {
        rawSaving.value = false
      }
    }

    async function reloadRaw(): Promise<void> {
      resetRawFeedback()
      rawSaving.value = false
      await content.reload()
      const next = content.data.value
      if (next === null) {
        rawError.value = `读取配置原文失败：${content.error.value}`
        return
      }
      rawOriginal.value = next.content
      rawEditor.value = next.content
      rawLoaded.value = true
      etag.value = next.etag
      rawNotice.value = '已重新读取配置原文。'
    }

    function discardRaw(): void {
      rawEditor.value = rawOriginal.value
      resetRawFeedback()
    }

    // --- 通用字段 -----------------------------------------------------------

    function textField(
      id: string,
      label: string,
      value: () => string,
      update: (next: string) => void,
      options: { placeholder?: string; hint?: string } = {},
    ): VNode {
      const children: VNodeChild[] = [
        h('span', {}, label),
        h('input', {
          id,
          type: 'text',
          value: value(),
          placeholder: options.placeholder ?? '',
          autocomplete: 'off',
          spellcheck: false,
          onInput: (event: Event) => {
            update((event.target as HTMLInputElement).value)
          },
        }),
      ]
      if (options.hint !== undefined) {
        children.push(h('span', { class: 'field-hint muted' }, options.hint))
      }
      return h('label', { class: 'field', for: id }, children)
    }

    /** 正文编辑区：围栏不可解析时禁用，并指向原文编辑器。 */
    function bodyField(): VNodeChild[] {
      if (content.loading.value && !bodyEditable.value && !bodyParseFailed.value) {
        return [h('p', { class: 'muted' }, '正在读取正文…')]
      }
      if (content.error.value !== '') {
        return [h(Notice, { tone: 'warn', text: `读取正文失败：${content.error.value}；可在「高级设置 → 配置原文」修改。` })]
      }
      if (bodyParseFailed.value) {
        return [
          h('p', { class: 'notice notice-warn' }, [
            '无法解析 AGENT.md 的 frontmatter 围栏；请先修正原文结构，再回到这里编辑正文。',
            h(
              'button',
              {
                type: 'button',
                onClick: () => {
                  rawOpen.value = true
                },
              },
              '打开配置原文',
            ),
          ]),
        ]
      }
      return [
        h('label', { class: 'field', for: 'config-body' }, [
          h('span', '执行规则（围栏外正文）'),
          h('textarea', {
            id: 'config-body',
            class: 'editor config-body-editor',
            rows: 12,
            spellcheck: false,
            value: body.value,
            onInput: (event: Event) => {
              body.value = (event.target as HTMLTextAreaElement).value
              emptyBodyArmed.value = false
            },
          }),
          h(
            'span',
            { class: 'field-hint muted' },
            '保存时整段替换围栏外正文；留空会清空执行规则，需要再次确认。与「角色定义」是两段独立内容。',
          ),
        ]),
        emptyBodyArmed.value
          ? h(Notice, {
              tone: 'warn',
              text: '正文当前为空：再次点击「保存配置」以确认清空执行规则。',
            })
          : null,
      ]
    }

    function bindingIssueNotices(): VNodeChild[] {
      const nodes: VNodeChild[] = []
      if (blockingIssues.value.length > 0) {
        const first = blockingIssues.value[0]!
        nodes.push(
          h(Notice, {
            tone: 'warn',
            text: `${resourceErrorCopyOf(first.code)}（涉及：${blockingIssues.value.map((issue) => issue.label).join('、')}）`,
          }),
        )
      }
      const revoked = references.value.filter(
        (reference) =>
          reference.credential_state === 'revoked' && reference.binding_status !== 'orphan',
      )
      for (const reference of revoked) {
        nodes.push(
          h('p', { class: 'notice notice-warn' }, [
            `已关联的 ${reference.ref_name} 凭据已撤销：`,
            h(
              'a',
              {
                href:
                  reference.resource_kind === 'model_connection'
                    ? resourceDetailHref(
                        'model-connections',
                        reference.resource_name === '' ? reference.ref_name : reference.resource_name,
                      )
                    : '/console/settings/notifications',
              },
              '到资源页重新录入凭据',
            ),
          ]),
        )
      }
      return nodes
    }

    const bindingRows = computed<readonly BindingRow[]>(() =>
      references.value.map((reference) => ({
        key: `${reference.resource_kind}:${reference.ref_name}`,
        reference,
        candidates: [],
      })),
    )

    // --- 离开保护 -----------------------------------------------------------

    const beforeUnloadHandler = (event: BeforeUnloadEvent): void => {
      if (!dirty.value) {
        return
      }
      event.preventDefault()
      event.returnValue = ''
    }

    onMounted(() => {
      if (props.embedded) {
        return
      }
      window.addEventListener('beforeunload', beforeUnloadHandler)
      props.ctx.router.setLeaveGuard(
        () => !dirty.value || window.confirm('配置有未保存的修改，离开将丢失这些修改。继续？'),
      )
    })

    onUnmounted(() => {
      if (props.embedded) {
        return
      }
      window.removeEventListener('beforeunload', beforeUnloadHandler)
      props.ctx.router.setLeaveGuard(null)
    })

    onBeforeUnmount(() => {
      // 页签切走后父组件不应继续认为配置有未保存修改（草稿已随组件销毁）。
      emit('dirty-change', false)
    })

    // --- 渲染 ---------------------------------------------------------------

    function advancedSection(): VNode {
      return section('高级设置', [
        ...bootstrapSection(),
        h('h3', { class: 'subheading' }, '执行设置'),
        textField(
          'config-max-iterations',
          '最大迭代次数',
          () => maxIterationsText.value,
          (next) => (maxIterationsText.value = next),
          { hint: '正整数；保存后下一次新运行生效。' },
        ),
        textField(
          'config-max-history',
          '最大历史轮数',
          () => maxHistoryTurnsText.value,
          (next) => (maxHistoryTurnsText.value = next),
          { hint: '正整数；保存后下一次新运行生效。' },
        ),
        // 输出随机性属于高级调参：默认值由模型连接决定，不放在常规模型分区。
        // Output randomness lives here; the normal model section stays simple.
        temperatureField(),
        h('h3', { class: 'subheading' }, '资源关联（只读）'),
        h(
          'p',
          { class: 'muted' },
          '每条引用必须显式关联一个资源 UID；同名重建后旧 UID 不会自动指向新资源。切换模型或通知渠道在上面各自的分区完成，删除整个连接请打开资源详情。',
        ),
        ...bindingIssueNotices(),
        h(BindingList, {
          id: 'config-bindings',
          rows: bindingRows.value,
          agent: name,
          readonly: true,
        }),
        h('h3', { class: 'subheading' }, '配置原文'),
        h(
          'p',
          { class: 'muted' },
          '需要改注释、未知字段或无法结构化表达的块时，在这里直接编辑 AGENT.md。原文保存整份内容；表单保存只提交改动字段。两者共用同一个 ETag，任一保存成功都会让另一方需要重新读取。',
        ),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              id: 'config-raw-toggle',
              disabled: rawSaving.value,
              onClick: () => {
                rawOpen.value = !rawOpen.value
                resetRawFeedback()
              },
            },
            rawOpen.value
              ? '收起配置原文'
              : rawDirty.value
                ? '配置原文（有未保存修改）'
                : '编辑配置原文',
          ),
        ]),
        rawOpen.value ? rawEditorPanel() : null,
        h('h3', { class: 'subheading' }, '诊断信息'),
        h('details', { class: 'diagnostics' }, [
          h('summary', {}, '显示 UID、ETag 与字段名'),
          definitionsBlock(),
        ]),
        h(
          'p',
          { class: 'field-hint muted' },
          '定时任务在 Agent 的「定时」页签维护；本页不重复编辑。',
        ),
      ])
    }

    function rawEditorPanel(): VNode {
      return h('div', { class: 'raw-editor' }, [
        rawError.value === '' ? null : h(Notice, { tone: 'error', text: rawError.value }),
        rawNotice.value === '' ? null : h(Notice, { tone: 'info', text: rawNotice.value }),
        h('label', { class: 'field', for: 'config-raw' }, [
          h('span', 'AGENT.md 原文'),
          h('textarea', {
            id: 'config-raw',
            class: 'editor',
            rows: 24,
            spellcheck: false,
            value: rawEditor.value,
            disabled: rawSaving.value,
            onInput: (event: Event) => {
              rawEditor.value = (event.target as HTMLTextAreaElement).value
            },
          }),
          h(
            'span',
            { class: 'field-hint muted' },
            '保存使用整份内容与当前 ETag 做条件写；412 时不会写入，需要重新读取后基于新内容修改。',
          ),
        ]),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              id: 'config-raw-save',
              disabled: rawSaving.value || !rawDirty.value,
              onClick: () => {
                void saveRaw()
              },
            },
            rawSaving.value ? '保存中…' : '保存原文',
          ),
          h(
            'button',
            {
              type: 'button',
              id: 'config-raw-discard',
              disabled: rawSaving.value || !rawDirty.value,
              onClick: discardRaw,
            },
            '放弃修改',
          ),
          h(
            'button',
            {
              type: 'button',
              id: 'config-raw-reload',
              disabled: rawSaving.value,
              onClick: () => {
                void reloadRaw()
              },
            },
            '重新读取',
          ),
        ]),
      ])
    }

    function definitionsBlock(): VNode {
      const state = config.data.value?.data
      return h('dl', { class: 'definitions' }, [
        h('dt', {}, 'Agent UID'),
        h('dd', {}, [h('code', {}, state === undefined || state.agent_uid === '' ? '（无状态文件）' : state.agent_uid)]),
        h('dt', {}, '配置校验标识（ETag，字段 content_etag）'),
        h('dd', {}, [h('code', {}, etag.value === '' ? '（尚未读取）' : etag.value)]),
        h('dt', {}, '通知模式来源'),
        h(
          'dd',
          {},
          state?.notify_mode_source === 'explicit'
            ? '定义显式声明'
            : '兼容模式：按旧配置推断（保存后写为显式值）',
        ),
        h('dt', {}, '资源关联完整'),
        h('dd', {}, state?.binding_complete === false ? '否：保存与准入会被拒绝' : '是'),
        h('dt', {}, 'AGENT.md 字段名'),
        h('dd', {}, [
          h(
            'code',
            { class: 'field-names' },
            'description · display_name · identity.agent_name · identity.prompt · provider.name / model / temperature · tools · skills · mcp_servers · bootstrap · notify_mode · notify_channels · resource_bindings · settings.max_iterations / max_history_turns',
          ),
        ]),
      ])
    }

    function effectNote(): VNode {
      return h(
        'p',
        { class: 'field-hint muted' },
        '生效方式：保存后下一次新运行使用新配置；已开始的运行保持准入时固定的版本。MCP 服务定义在资源页发布；显示「待重启」时，重启进程后才按新版本连接。',
      )
    }

    return (): VNode => {
      const state = config.data.value
      const children: VNodeChild[] = []
      if (!props.embedded) {
        children.push(
          pageHeading(
            'Agents / 配置',
            name,
            '结构化字段、资源选择与执行规则；高级改动在「高级设置 → 配置原文」完成。',
            h('div', { class: 'form-actions' }, [
              h('a', { class: 'button', href: agentHref(name) }, '返回 Agent 工作区'),
            ]),
          ),
        )
      }
      if (config.error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: config.error.value }))
      }
      if (state === null) {
        children.push(h('p', { class: 'muted' }, config.loading.value ? '正在读取配置…' : '配置尚未读取。'))
        if (!config.loading.value && config.error.value !== '') {
          children.push(
            h('div', { class: 'form-actions' }, [
              h(
                'button',
                {
                  type: 'button',
                  onClick: () => {
                    void Promise.all([config.reload(), content.reload(), candidates.reload()])
                  },
                },
                '重新读取',
              ),
            ]),
          )
        }
        return h('div', { class: ['view', 'agent-config', props.embedded ? 'embedded' : ''] }, children)
      }
      if (error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: error.value }))
      }
      if (notice.value !== '') {
        children.push(h(Notice, { tone: 'info', text: notice.value }))
      }
      if (problems.value.length > 0) {
        children.push(
          h(
            'ul',
            { class: 'problem-list' },
            problems.value.map((item, index) => h('li', { key: index }, item)),
          ),
        )
      }
      children.push(
        section('基本信息', [
          textField('config-description', '描述', () => description.value, (next) => (description.value = next)),
          textField('config-display-name', '显示名（display_name）', () => displayName.value, (next) => (displayName.value = next), {
            hint: '列表、详情标题优先显示它，旁边仍保留技术名；留空则回退技术名。它只影响展示，不改目录、API 与运行身份。',
          }),
          h('div', { class: 'field' }, [
            h('span', {}, '技术名（目录 / 运行 / API）'),
            h('p', { class: 'read-value' }, [h('code', {}, name)]),
            h(
              'span',
              { class: 'field-hint muted' },
              '技术名与目录名属于受控生命周期能力，本页不提供重命名；复制并归档旧 Agent 不是无损重命名。',
            ),
          ]),
          textField(
            'config-agent-name',
            'Agent 自称（identity.agent_name）',
            () => persona.value,
            (next) => (persona.value = next),
            {
              hint: '写进系统提示的人设自称（例如“你的名字是「小助手」”），不是列表或标题使用的显示名；留空表示不声明。',
            },
          ),
          h('label', { class: 'field', for: 'config-prompt' }, [
            h('span', {}, '角色定义（identity.prompt）'),
            h('textarea', {
              id: 'config-prompt',
              rows: 4,
              value: prompt.value,
              spellcheck: false,
              onInput: (event: Event) => {
                prompt.value = (event.target as HTMLTextAreaElement).value
              },
            }),
            h(
              'span',
              { class: 'field-hint muted' },
              '角色定义说明职责、行为边界与风格，写入系统提示；下方「执行规则」说明流程、约束与输出格式，是另一段独立内容，两段都会生效。',
            ),
          ]),
        ]),
        section('执行规则', bodyField()),
        section('模型', [
          candidatesWarning(),
          modelPicker(),
          textField('config-provider-model', '模型', () => providerModel.value, (next) => (providerModel.value = next), {
            hint: '选择连接时会预填该连接的默认模型；这里自由填写该连接支持的模型 ID，不提供伪造的模型目录。',
          }),
          h('p', { class: 'muted small' }, '要使用自己的 API 密钥，请为当前 Agent 新建一个专属连接。修改共享连接会影响所有使用它的 Agent。'),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                disabled: busy.value,
                onClick: () => {
                  showNewConnection.value = !showNewConnection.value
                  if (showNewConnection.value) {
                    showNewChannel.value = false
                  }
                },
              },
              showNewConnection.value ? '收起新建连接' : '为当前 Agent 新建连接',
            ),
          ]),
          connectionCreatePanel(),
        ]),
        section('工具与技能', [
          h(
            'p',
            { class: 'muted' },
            '勾选即生效：只有勾选的工具与技能会出现在运行中；保存后下一次新运行生效。目录不可读时降级为手动填写，不伪造选项。',
          ),
          catalogNotice(),
          ...toolsSubsection(),
          ...skillsSubsection(),
        ]),
        section('MCP', [
          h(
            'p',
            { class: 'muted' },
            '这里只声明当前 Agent 使用哪些 MCP 服务；服务定义（command / URL / env 占位）在「MCP 服务」资源页维护与发布。',
          ),
          ...mcpSubsection(),
        ]),
        section('通知', [
          notifyModeChoices(),
          h(
            'p',
            { class: 'muted' },
            `当前生效来源：${state.data.notify_mode_source === 'explicit' ? '定义显式声明' : '兼容模式：按旧配置推断（保存后写为显式值）'}`,
          ),
          notifyChannelChoices(),
          notifyToolRelation() === ''
            ? null
            : h(Notice, { tone: 'info', text: notifyToolRelation() }),
          h(
            'p',
            { class: 'muted small' },
            '这里声明允许使用的渠道边界；保存时系统按通知意图自动装配 tools 里的 notify（只增删这一个工具，不改其它工具）。同名重建或缺失 UID 的渠道不会按名称自动关联，取消勾选后重新勾选即按当前 UID 显式关联。定时任务配置了结果投递渠道后，那一轮由系统在运行成功后发送，不向模型提供 notify，避免双方各发一次。',
          ),
          h('p', { class: 'muted small' }, '渠道地址与凭据在「通知渠道」资源页维护。'),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                disabled: busy.value,
                onClick: () => {
                  showNewChannel.value = !showNewChannel.value
                  if (showNewChannel.value) {
                    showNewConnection.value = false
                    notifyMode.value = 'allowlist'
                  }
                },
              },
              showNewChannel.value ? '收起新建渠道' : '为当前 Agent 新建渠道',
            ),
          ]),
          channelCreatePanel(),
        ]),
        advancedSection(),
        effectNote(),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: busy.value || rawSaving.value,
              onClick: () => void save(),
            },
            busy.value ? '保存中…' : '保存配置',
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value || rawSaving.value,
              onClick: () => {
                resetFeedback()
                void Promise.all([
                  config.reload(),
                  content.reload(),
                  candidates.reload(),
                  catalogs.reload(),
                  notifyTypes.reload(),
                ])
              },
            },
            '重新读取',
          ),
          dirty.value ? h('span', { class: 'muted' }, '有未保存修改') : null,
        ]),
      )
      return h('div', { class: ['view', 'agent-config', props.embedded ? 'embedded' : ''] }, children)
    }
  },
})
