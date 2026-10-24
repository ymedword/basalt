// 通知渠道写面组件（页内挂 /console/settings/notifications）：新建 / 修改 / 删除
// 与类型下拉。地址只接受精确 `${ENV_VAR}` 占位——明文在本地就拒绝、零请求发出；
// 列表只渲染服务端回传的 `url_host` / `url_configured` / `referenced_by`，原始地址
// 在组件渲染里没有任何载体（约束 38）。删除被 Agent 引用时由服务端 400 点名；
// `force=true` 只在页面二次确认后发出。写成功后只发 `changed` 事件——列表由父页面
// 重新 GET（不做乐观更新）。
// In-page notification-channel CRUD. The URL field accepts only exact ${ENV_VAR}
// placeholders (plaintext is rejected locally with zero requests); the list renders
// only url_host / url_configured / referenced_by from the server. Deleting a
// referenced channel needs a second confirmation before the literal force=true is
// sent; writes emit `changed` so the parent re-reads instead of patching its list.
//
// US-12 修订追加：列表补范围 / 所有者 / 凭据状态 / 地址状态列；范围切换与托管凭据
// 撤销走资源端点（`/resources/notify_channel/{name}/...`），条件写带行内 etag 与
// resource_uid——不做乐观更新，成功后由父页面重新读取。
// Scope switching and managed-credential revocation use the resource endpoints with
// the row's etag and resource_uid; the component never patches its own list.
import { computed, defineComponent, h, nextTick, onMounted, ref } from 'vue'
import type { PropType, VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type { ApiClient, CredentialMode, NotifyChannelView, ResourceScope } from '../api'
import { adminApi, problemsOf } from '../api-admin'
import type { NotifyChannelTestResult, NotifyChannelUpsert } from '../api-admin'
import { agentHref } from '../router'
import { CREDENTIAL_STATE_LABELS, CredentialField } from './credential-field'
import { ScopePicker } from './scope-picker'
import { ConfirmDialog, DataTable, Notice, StatusBadge, formatTimestamp } from './ui'
import type { TableRow, Tone } from './ui'

/** 与写侧同一口径的精确占位：`${VAR}` 独占整个值，前后不得有其他字符。 */
const ENV_PLACEHOLDER = /^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/

/** 环境变量名：页面把裸名补成 `${VAR}`，与模型连接的输入口径一致。 */
const ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]*$/

/** 裸环境变量名或完整占位 → `${VAR}`；其它输入返回 null（真实地址不在此模式提交）。 */
function envReference(raw: string): string | null {
  const trimmed = raw.trim()
  const inner = trimmed.startsWith('${') && trimmed.endsWith('}') ? trimmed.slice(2, -1) : trimmed
  return ENV_KEY.test(inner) ? '${' + inner + '}' : null
}

/** 列表用的范围短标签（完整说明在资源详情页的范围选择器下方）。 */
// Short scope labels for the list column.
const SCOPE_SHORT: Readonly<Record<ResourceScope, string>> = {
  shared: '共享',
  exclusive: '专属',
}

/**
 * 地址状态四值（契约 `address_status`）：`url_host` 为空**不等于**未配置——
 * env 引用同样投影为空。托管地址在主密钥不可用时不可发送。
 * Four-value address status: an empty url_host never means "unconfigured" on its own.
 */
const ADDRESS_STATUS_LABELS: Readonly<
  Record<NotifyChannelView['address_status'], { readonly label: string; readonly tone: Tone }>
> = {
  env: { label: '环境变量', tone: 'muted' },
  empty: { label: '未配置地址', tone: 'warn' },
  managed_configured: { label: '加密托管地址已配置', tone: 'ok' },
  managed_unavailable: { label: '加密托管地址不可用', tone: 'bad' },
}

/**
 * 实测结论三值：`unknown`（超时 / 回执不可解析）是"可能已送达"，既不是成功也
 * 不是失败——列表照契约显示「结果未知」，不折叠成任一档。
 * Test verdicts; unknown may have been delivered and is never folded into pass/fail.
 */
/**
 * 目标服务的文本（列表与编辑提示共用一份）。托管地址不回显，只说明"已由服务端
 * 托管"；env 引用在发送时才解析；`empty` 才是真正未配置。
 */
function targetText(channel: NotifyChannelView): string {
  if (channel.url_host !== '') {
    return channel.url_host
  }
  switch (channel.address_status) {
    case 'managed_configured':
    case 'managed_unavailable':
      return '加密托管地址（不回显）'
    case 'env':
      return '环境变量（发送时解析）'
    default:
      return '未配置地址，暂不可发送'
  }
}

const TEST_STATUS_LABELS: Readonly<
  Record<NotifyChannelTestResult['status'], { readonly label: string; readonly tone: Tone }>
> = {
  success: { label: '成功', tone: 'ok' },
  failed: { label: '失败', tone: 'bad' },
  unknown: { label: '结果未知', tone: 'muted' },
}

export const NotifyChannels = defineComponent({
  name: 'NotifyChannels',
  props: {
    api: { type: Object as PropType<ApiClient>, required: true },
    /** 父页面 useLoader 读到的渠道列表；组件只读它，写后由父页面重新读取。 */
    channels: { type: Array as PropType<readonly NotifyChannelView[]>, required: true },
  },
  emits: {
    changed: () => true,
  },
  setup(props, { emit }) {
    const client = adminApi(props.api)

    // --- 类型表（GET /notify-channels/types，服务端有序切片：约束 39）--------
    const types = ref<string[]>([])
    const typesError = ref('')

    // --- 表单 ---------------------------------------------------------------
    const mode = ref<'create' | 'edit'>('create')
    const editingName = ref<string | null>(null)
    const name = ref('')
    const type = ref('')
    // 地址输入是 write-only 草稿：managed 提交完整 Webhook 地址，env 提交 `${VAR}`。
    // 编辑态留空 = 保留当前地址；新建时留空（env 模式）表示暂不可发送（FR37）。
    // The address draft is write-only; a blank edit keeps the current address.
    const url = ref('')
    const credentialMode = ref<CredentialMode>('managed')
    // 新建时选择使用范围；所有者候选只在真正选专属时读取。
    // Create-time scope; the owner list is fetched only when exclusive is chosen.
    const createScope = ref<ResourceScope>('shared')
    const createOwner = ref('')
    const ownerAgents = ref<readonly string[]>([])
    const ownerAgentsError = ref('')
    const ownerAgentsLoading = ref(false)
    const description = ref('')

    // --- 反馈 ---------------------------------------------------------------
    const busy = ref(false)
    const error = ref('')
    const problems = ref<string[]>([])
    const notice = ref('')
    // 测试结论按成功 / 失败 / 未知切换色调：失败与未知都不能显示成"已验证"。
    const noticeTone = ref<'info' | 'warn' | 'error'>('info')
    /** 正在测试的渠道名：只禁用该行按钮，不把整张表锁死。 */
    const testingName = ref<string | null>(null)
    // 表单在列表上方：点行内「编辑」后把卡片滚进视口，否则用户看不到表单切换。
    // The form sits above the list; scroll it into view when editing starts.
    const formRef = ref<HTMLElement | null>(null)
    const editingChannel = computed<NotifyChannelView | null>(
      () => props.channels.find((channel) => channel.name === editingName.value) ?? null,
    )

    // --- 删除确认链：一级确认 → （被引用时）强删二级确认 ---------------------
    const pendingDelete = ref<string | null>(null)
    const forceTarget = ref<string | null>(null)
    const forceArmed = ref(false)
    const forceAgents = ref<string[]>([])

    // --- 范围切换与凭据撤销（资源端点；条件写带行内 etag 与 resource_uid）-----
    // Scope switching and revocation go through /resources/notify_channel/{name}/...
    // with the row's own etag + resource_uid; the list itself is never patched.
    const scopeTarget = ref<string | null>(null)
    const draftScope = ref<ResourceScope>('shared')
    const draftOwner = ref('')
    const scopeAgents = ref<readonly string[]>([])
    const scopeAgentsError = ref('')
    const revokeTarget = ref<string | null>(null)

    function channelNamed(name: string): NotifyChannelView | null {
      return props.channels.find((channel) => channel.name === name) ?? null
    }

    function cancelScope(): void {
      scopeTarget.value = null
      draftScope.value = 'shared'
      draftOwner.value = ''
      scopeAgentsError.value = ''
    }

    /** 读取所有者候选（新建专属渠道时）；失败留在表单里提示，不静默留空。 */
    async function ensureOwnerAgents(): Promise<void> {
      if (ownerAgents.value.length > 0 || ownerAgentsLoading.value) {
        return
      }
      ownerAgentsLoading.value = true
      try {
        const profiles = await props.api.listProfiles()
        ownerAgents.value = profiles.map((profile) => profile.name)
        ownerAgentsError.value = ''
      } catch (caught) {
        ownerAgentsError.value = `读取 Agent 列表失败，所有者候选暂不可用：${errorMessage(caught)}`
      } finally {
        ownerAgentsLoading.value = false
      }
    }

    /** 打开范围编辑器；所有者候选只在真正要切换时才读取，读取失败留在面板里报错。 */
    function openScope(channel: NotifyChannelView): void {
      resetFeedback()
      cancelDelete()
      scopeTarget.value = channel.name
      draftScope.value = channel.scope
      draftOwner.value = channel.owner_agent
      if (scopeAgents.value.length > 0) {
        return
      }
      void props.api
        .listProfiles()
        .then((profiles) => {
          scopeAgents.value = profiles.map((profile) => profile.name)
          scopeAgentsError.value = ''
        })
        .catch((caught: unknown) => {
          scopeAgentsError.value = `读取 Agent 列表失败，所有者候选暂不可用：${errorMessage(caught)}`
        })
    }

    /**
     * 条件写：`resource_uid` 必须等于当前指针 UID，`If-Match` 用行内 etag。
     * 412 = 别人先改了行，页面只提示并让父页面重新读取，不自动重试。
     * Conditional write with the row's etag; 412 re-reads instead of retrying.
     */
    async function changeScope(): Promise<void> {
      const target = scopeTarget.value === null ? null : channelNamed(scopeTarget.value)
      if (busy.value || target === null) {
        return
      }
      resetFeedback()
      const owner = draftOwner.value.trim()
      if (draftScope.value === 'exclusive' && owner === '') {
        error.value = '转专属时必须选择所有者 Agent'
        return
      }
      if (draftScope.value === target.scope && owner === target.owner_agent) {
        notice.value = '使用范围没有变化'
        return
      }
      busy.value = true
      try {
        const result = await client.setResourceScope(
          'notify_channel',
          target.name,
          {
            resource_uid: target.resource_uid,
            scope: draftScope.value,
            ...(draftScope.value === 'exclusive' ? { owner_agent: owner } : {}),
          },
          target.etag,
        )
        const affected = result.affected_eligible_agents
        cancelScope()
        notice.value =
          result.scope === 'shared'
            ? affected.length === 0
              ? `已将 ${target.name} 切换为共享（所有 Agent 可关联）。`
              : `已将 ${target.name} 切换为共享；以下 Agent 可用但不自动改动：${affected.join('、')}。`
            : affected.length === 0
              ? `已将 ${target.name} 切换为专属（${result.owner_agent}）。`
              : `已将 ${target.name} 切换为专属（${result.owner_agent}）；原本可用的 ${affected.join('、')} 需要另行关联。`
        emit('changed')
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 412) {
          error.value = '渠道已被他人修改，本次切换没有写入；已重新读取渠道列表，请确认后再操作'
          emit('changed')
        } else {
          problems.value = problemsOf(caught)
          error.value = errorMessage(caught)
        }
      } finally {
        busy.value = false
      }
    }

    function askRevoke(channel: NotifyChannelView): void {
      resetFeedback()
      cancelDelete()
      cancelScope()
      revokeTarget.value = channel.name
    }

    function cancelRevoke(): void {
      revokeTarget.value = null
    }

    /** 撤销托管凭据：整条凭据身份失效，旧版本永不复活；重新录入会生成新版本。 */
    // Revocation retires the credential identity; a later re-entry adds new versions.
    async function confirmRevoke(): Promise<void> {
      const target = revokeTarget.value === null ? null : channelNamed(revokeTarget.value)
      revokeTarget.value = null
      if (busy.value || target === null) {
        return
      }
      resetFeedback()
      busy.value = true
      try {
        const result = await client.revokeCredential(
          'notify_channel',
          target.name,
          { resource_uid: target.resource_uid, confirm: true },
          target.etag,
        )
        notice.value = `已撤销 ${target.name} 的 ${result.revoked_versions} 个凭据版本（撤销序号 ${result.revocation_epoch}）；旧版本不会复活，重新录入会生成新版本。`
        emit('changed')
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 412) {
          error.value = '渠道已被他人修改，本次撤销没有写入；已重新读取渠道列表，请确认后再操作'
          emit('changed')
        } else {
          problems.value = problemsOf(caught)
          error.value = errorMessage(caught)
        }
      } finally {
        busy.value = false
      }
    }

    /**
     * 渠道连通性测试（R7）：经与系统代投**同一个**发送面发出一条固定文案
     * 「Basalt 连通性测试」。结论三值分档——失败不是"已验证"，未知可能已送达且
     * 系统不会自动重发。每次测试都由服务端覆盖 `last_test` 投影；页面只重新读取，
     * 不自行拼一条"已测试"的记录。
     * The connectivity test sends the fixed text; verdicts stay three-valued and the
     * page only re-reads the server-recorded result.
     */
    async function testChannel(channel: NotifyChannelView): Promise<void> {
      if (busy.value) {
        return
      }
      resetFeedback()
      busy.value = true
      testingName.value = channel.name
      try {
        const result = await client.testNotifyChannel(channel.name)
        noticeTone.value =
          result.status === 'success' ? 'info' : result.status === 'failed' ? 'error' : 'warn'
        notice.value = testResultText(channel.name, result)
      } catch (caught) {
        problems.value = problemsOf(caught)
        error.value = errorMessage(caught)
      } finally {
        busy.value = false
        testingName.value = null
        // 无论结论如何都重新读取列表：last_test 是服务端记录，不在页面里自行拼。
        emit('changed')
      }
    }

    /** 测试结论文案：成功说"对端确认受理"，失败给出脱敏原因，未知明确"可能已送达"。 */
    function testResultText(name: string, result: NotifyChannelTestResult): string {
      const latency = `${result.latency_ms} ms`
      const detail = result.detail === '' ? '服务端未给出原因' : result.detail
      switch (result.status) {
        case 'success':
          return `渠道「${name}」连通性测试成功（${latency}）：已发送固定文案「Basalt 连通性测试」，对端确认受理。`
        case 'failed':
          return `渠道「${name}」连通性测试失败（${latency}）：${detail}。这是一次已记录的失败实测，不代表已验证。`
        default:
          return `渠道「${name}」连通性测试结果未知（${latency}）：${detail}。可能已送达，系统不会自动重发；请先在接收端确认，再决定是否重试。`
      }
    }

    onMounted(() => {
      void loadTypes()
    })

    async function loadTypes(): Promise<void> {
      try {
        types.value = await client.listNotifyTypes()
      } catch (caught) {
        typesError.value = `读取渠道类型失败，暂以文本输入类型：${errorMessage(caught)}`
      }
    }

    function resetFeedback(): void {
      error.value = ''
      problems.value = []
      notice.value = ''
      noticeTone.value = 'info'
    }

    /** 回到新建态并清空删除确认链与范围面板；反馈文案由调用方决定是否保留。 */
    function resetForm(): void {
      mode.value = 'create'
      editingName.value = null
      name.value = ''
      type.value = ''
      url.value = ''
      credentialMode.value = 'managed'
      createScope.value = 'shared'
      createOwner.value = ''
      description.value = ''
      cancelDelete()
      cancelScope()
    }

    function cancelDelete(): void {
      pendingDelete.value = null
      forceTarget.value = null
      forceArmed.value = false
      forceAgents.value = []
    }

    /** 修改入口：地址栏留空 = 保留已登记值（契约 NotifyChannelUpsert）。 */
    function startEdit(channel: NotifyChannelView): void {
      resetFeedback()
      cancelDelete()
      cancelScope()
      mode.value = 'edit'
      editingName.value = channel.name
      name.value = channel.name
      type.value = channel.type
      url.value = ''
      credentialMode.value = channel.credential_mode
      description.value = channel.description
      // 表单位于列表上方；不滚动时点击行内“编辑”看起来没有反应。
      void nextTick(() => {
        formRef.value?.scrollIntoView?.({ behavior: 'smooth', block: 'start' })
      })
    }

    /**
     * 本地校验：名称 / 类型必填；地址两种模式三种情形（新建 managed 必填 / 编辑
     * 留空保留 / 切换模式带值）都在这里先判——服务端同样会拒，但本地先挡可以保证
     * 「零请求发出」。错误文案不回显地址或密钥。
     * Local checks mirror the server: managed needs a full address on create, a blank
     * edit keeps the current one, and switching modes always needs a new value.
     */
    function requestBody(): NotifyChannelUpsert | null {
      const trimmedName = name.value.trim()
      const trimmedType = type.value.trim()
      const editingChannelNow = editingChannel.value
      const creating = editingName.value === null
      if (trimmedName === '') {
        error.value = '请填写渠道名称'
        return null
      }
      if (trimmedType === '') {
        error.value = '请选择渠道类型'
        return null
      }
      const body: NotifyChannelUpsert = {
        name: trimmedName,
        type: trimmedType,
        description: description.value.trim(),
      }
      const raw = url.value
      const trimmedURL = raw.trim()
      if (trimmedURL !== '') {
        if (credentialMode.value === 'managed') {
          if (ENV_PLACEHOLDER.test(trimmedURL)) {
            error.value = '「加密托管」来源不接受 ${VAR} 占位：请填写完整的 Webhook 地址'
            return null
          }
          if (trimmedURL !== raw) {
            error.value = '地址首尾不得含空白'
            return null
          }
          body.url = raw
        } else {
          const reference = envReference(raw)
          if (reference === null) {
            error.value = '「环境变量」来源只接受变量名或 ${VAR} 占位，例如 ${ALERT_WEBHOOK}；明文地址不会被提交'
            return null
          }
          body.url = reference
        }
        body.credential_mode = credentialMode.value
      } else if (creating) {
        if (credentialMode.value === 'managed') {
          error.value = '「加密托管」来源必须填写完整的 Webhook 地址；要创建暂不可发送的渠道，请切换到「环境变量」后留空'
          return null
        }
      } else if (editingChannelNow !== null && credentialMode.value !== editingChannelNow.credential_mode) {
        error.value = '切换凭据来源必须填写新值（加密托管：完整 Webhook 地址；环境变量：变量名）'
        return null
      }
      if (creating) {
        body.scope = createScope.value
        if (createScope.value === 'exclusive') {
          const owner = createOwner.value.trim()
          if (owner === '') {
            error.value = '专属渠道必须选择所有者 Agent'
            return null
          }
          body.owner_agent = owner
        }
      }
      return body
    }

    async function submit(): Promise<void> {
      if (busy.value) {
        return
      }
      resetFeedback()
      const body = requestBody()
      if (body === null) {
        return
      }
      busy.value = true
      try {
        const editing = editingName.value
        if (editing === null) {
          await client.createNotifyChannel(body)
          notice.value = '已创建；渠道列表将从服务端重新读取。'
        } else {
          await client.updateNotifyChannel(editing, body)
          notice.value = '已保存；渠道列表将从服务端重新读取。'
        }
        resetForm()
        emit('changed')
      } catch (caught) {
        problems.value = problemsOf(caught)
        error.value = errorMessage(caught)
      } finally {
        busy.value = false
      }
    }

    // --- 删除 ---------------------------------------------------------------

    function askDelete(channel: NotifyChannelView): void {
      resetFeedback()
      cancelDelete()
      pendingDelete.value = channel.name
    }

    /** 被引用（400 且点名 agents）时转入强删通道；其它失败只展示信封文案。 */
    function enterForceMode(target: string, caught: unknown): void {
      const agents = caught instanceof ApiError ? (caught.data?.agents ?? []) : []
      if (!(caught instanceof ApiError) || caught.status !== 400 || agents.length === 0) {
        return
      }
      pendingDelete.value = null
      forceTarget.value = target
      forceArmed.value = false
      forceAgents.value = agents
    }

    /**
     * 删除必须带上行里读到的 `resource_uid`（契约 DELETE /notify-channels 的
     * 必需查询参数）：同名重建后列表里的旧行会被 409 拒绝，而不是删掉新渠道。
     */
    async function deleteChannel(
      target: string,
      resourceUid: string,
      force: boolean,
    ): Promise<void> {
      if (busy.value) {
        return
      }
      resetFeedback()
      busy.value = true
      try {
        await client.deleteNotifyChannel(target, resourceUid, force)
        notice.value = force
          ? `已强制删除 ${target}；删除前仍引用它的 Agent 会在下次发送时失败。`
          : `已删除 ${target}；渠道列表将从服务端重新读取。`
        cancelDelete()
        if (editingName.value === target) {
          resetForm()
        }
        emit('changed')
      } catch (caught) {
        problems.value = problemsOf(caught)
        error.value = errorMessage(caught)
        if (!force) {
          enterForceMode(target, caught)
        }
      } finally {
        busy.value = false
      }
    }

    /** force=true 的二次确认：第一次点击只解锁按钮，第二次才真正发请求。 */
    function forceClick(target: string, resourceUid: string): void {
      if (!forceArmed.value) {
        forceArmed.value = true
        return
      }
      void deleteChannel(target, resourceUid, true)
    }

    // --- 渲染 ---------------------------------------------------------------

    function textField(
      id: string,
      label: string,
      value: () => string,
      update: (next: string) => void,
      options: { placeholder?: string; disabled?: boolean } = {},
    ): VNode {
      return h('label', { class: 'field', for: id }, [
        h('span', label),
        h('input', {
          id,
          type: 'text',
          value: value(),
          placeholder: options.placeholder ?? '',
          autocomplete: 'off',
          spellcheck: false,
          disabled: options.disabled ?? false,
          onInput: (event: Event) => {
            update((event.target as HTMLInputElement).value)
          },
        }),
      ])
    }

    /** 类型下拉来自服务端有序表；读取失败时退化为文本输入，仍由服务端校验。 */
    function typeField(): VNode {
      if (types.value.length === 0) {
        return textField(
          'notify-type',
          typesError.value === '' ? '类型（读取中）' : '类型',
          () => type.value,
          (next) => (type.value = next),
          { placeholder: '例如 webhook' },
        )
      }
      const options = [...types.value]
      if (type.value !== '' && !options.includes(type.value)) {
        options.unshift(type.value)
      }
      const choices: VNode[] = []
      if (type.value === '') {
        choices.push(h('option', { value: '' }, '请选择类型'))
      }
      for (const item of options) {
        choices.push(h('option', { key: item, value: item }, item))
      }
      return h('label', { class: 'field', for: 'notify-type' }, [
        h('span', '类型'),
        h(
          'select',
          {
            id: 'notify-type',
            value: type.value,
            onChange: (event: Event) => {
              type.value = (event.target as HTMLSelectElement).value
            },
          },
          choices,
        ),
      ])
    }

    function channelActions(channel: NotifyChannelView): VNode {
      if (pendingDelete.value === channel.name) {
        return h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: busy.value,
              onClick: () => {
                void deleteChannel(channel.name, channel.resource_uid, false)
              },
            },
            '确认删除',
          ),
          h('button', { type: 'button', disabled: busy.value, onClick: cancelDelete }, '取消'),
        ])
      }
      if (forceTarget.value === channel.name) {
        return h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value,
              onClick: () => forceClick(channel.name, channel.resource_uid),
            },
            forceArmed.value ? '再次确认强制删除' : '强制删除',
          ),
          h('button', { type: 'button', disabled: busy.value, onClick: cancelDelete }, '取消'),
        ])
      }
      const actions: VNode[] = [
        h('button', { type: 'button', disabled: busy.value, onClick: () => startEdit(channel) }, '编辑'),
        h(
          'button',
          {
            type: 'button',
            disabled: busy.value,
            title: '经系统代投发送面发出一条「Basalt 连通性测试」；结果记入最近实测',
            onClick: () => {
              void testChannel(channel)
            },
          },
          testingName.value === channel.name ? '测试中…' : '测试连通性',
        ),
        h('button', { type: 'button', disabled: busy.value, onClick: () => openScope(channel) }, '切换范围'),
        h('button', { type: 'button', disabled: busy.value, onClick: () => askDelete(channel) }, '删除'),
      ]
      // 撤销只对托管且仍有效的凭据开放：env 模式没有可撤销的密文。
      if (channel.credential_mode === 'managed' && channel.credential_state === 'active') {
        actions.push(
          h('button', { type: 'button', disabled: busy.value, onClick: () => askRevoke(channel) }, '撤销凭据'),
        )
      }
      return h('div', { class: 'form-actions' }, actions)
    }

    /** 凭据状态列：env 模式没有托管密文；managed 只给状态与尾号，不含密文。 */
    // Credential state: env has no managed ciphertext; managed shows state + last4.
    function credentialCell(channel: NotifyChannelView): VNodeChild {
      if (channel.credential_mode === 'env') {
        return h('span', { class: 'muted' }, '无加密凭据（环境变量）')
      }
      const state = CREDENTIAL_STATE_LABELS[channel.credential_state]
      return h('span', { class: 'credential-status' }, [
        h(StatusBadge, { label: state.label, tone: state.tone }),
        channel.last4 === '' ? null : h('code', {}, '尾号 ····' + channel.last4),
      ])
    }

    /** 地址状态列：四值逐字来自契约，不与「目标服务是否为空」互相推断。 */
    function addressCell(channel: NotifyChannelView): VNodeChild {
      const status = ADDRESS_STATUS_LABELS[channel.address_status]
      return h(StatusBadge, { label: status.label, tone: status.tone })
    }

    /**
     * 最近实测列：只来自服务端 `last_test` 记录——`null` 就是从未测过，绝不用
     * 「地址状态」这类本地配置读数冒充一次实测。
     * The last real test only; null means never tested and local readiness never fills in.
     */
    function lastTestCell(channel: NotifyChannelView): VNodeChild {
      const test = channel.last_test
      if (test === null) {
        return h('span', { class: 'muted' }, '从未测试')
      }
      const status = TEST_STATUS_LABELS[test.status]
      return h(
        'span',
        { class: 'credential-status', title: test.detail },
        [
          h(StatusBadge, { label: status.label, tone: status.tone }),
          h('span', { class: 'muted small' }, formatTimestamp(test.tested_at)),
        ],
      )
    }

    /**
     * 目标服务的文本投影：以服务端 `address_status` 为准。`url_host` 只有 env
     * 模式才可能解析出来；托管模式的地址按设计不回显，空 `url_host` 并不等于
     * "没配地址"。四值分别投影，不折叠——托管地址即使主密钥暂不可用，也仍是
     * "已配置但发不出"，不能说成未配置。
     * Target text projects `address_status`; an empty url_host is normal for managed
     * addresses and must never be rendered as "unconfigured".
     */
    function targetCell(channel: NotifyChannelView): VNodeChild {
      const text = targetText(channel)
      return channel.url_host !== '' ? text : h('span', { class: 'muted' }, text)
    }

    /**
     * 文档引用：服务端扫描活动与归档 AGENT.md 的整词提及，是**引用**不是精确
     * 绑定；归档项以 `archive:` 前缀标出，不可点击（没有活动详情页）。
     * Document references: a textual scan, not an exact binding; archived hits are
     * prefixed and not linked.
     */
    function referenceCell(channel: NotifyChannelView): VNodeChild {
      if (channel.referenced_by.length === 0) {
        return h('span', { class: 'muted' }, '无')
      }
      const nodes: VNodeChild[] = []
      channel.referenced_by.forEach((entry, index) => {
        if (index > 0) {
          nodes.push('、')
        }
        nodes.push(
          entry.startsWith('archive:')
            ? h('span', { class: 'muted' }, entry)
            : h('a', { href: agentHref(entry) }, entry),
        )
      })
      return h('span', {}, nodes)
    }

    function channelRows(): TableRow[] {
      return props.channels.map((channel) => ({
        key: channel.name,
        cells: [
          h('code', { class: 'notify-channel-name' }, channel.name),
          h('code', { title: channel.resource_uid }, channel.resource_uid.slice(0, 8) + '…'),
          channel.type,
          channel.description === '' ? '—' : channel.description,
          targetCell(channel),
          h(StatusBadge, {
            label: SCOPE_SHORT[channel.scope],
            tone: channel.scope === 'exclusive' ? 'warn' : 'muted',
          }),
          channel.owner_agent === '' ? h('span', { class: 'muted' }, '—') : channel.owner_agent,
          credentialCell(channel),
          addressCell(channel),
          lastTestCell(channel),
          referenceCell(channel),
          channelActions(channel),
        ],
      }))
    }

    /** 范围面板：与资源详情页同一套端点与规则，不预判服务端的引用判定、不提供 force。 */
    // The scope panel mirrors the resource detail page: no client-side prediction.
    function renderScopePanel(): VNodeChild {
      const target = scopeTarget.value === null ? null : channelNamed(scopeTarget.value)
      if (target === null) {
        return null
      }
      return h('div', { class: 'scope-editor card' }, [
        h('h3', `切换使用范围：${target.name}`),
        scopeAgentsError.value === '' ? null : h(Notice, { tone: 'warn', text: scopeAgentsError.value }),
        h(ScopePicker, {
          id: 'notify-scope',
          scope: draftScope.value,
          ownerAgent: draftOwner.value,
          agents: scopeAgents.value,
          disabled: busy.value,
          'onUpdate:scope': (next: ResourceScope) => {
            draftScope.value = next
          },
          'onUpdate:ownerAgent': (next: string) => {
            draftOwner.value = next
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
                void changeScope()
              },
            },
            '保存范围',
          ),
          h('button', { type: 'button', disabled: busy.value, onClick: cancelScope }, '取消'),
        ]),
        h(
          'p',
          { class: 'muted small' },
          '共享转专属时若仍被 Agent 配置引用，服务端会拒绝并点名对应的 Agent，本页不提供强制切换。',
        ),
      ])
    }

    /** 地址输入：托管模式是真正的秘密（密码框），env 模式是变量引用（文本框）。 */
    // The address field uses the shared write-only credential input.
    function addressField(): VNode {
      const target = editingChannel.value
      const editingNow = editingName.value !== null
      return h(CredentialField, {
        id: 'notify-url',
        noun: '地址',
        label: editingNow ? 'Webhook 地址（留空 = 保留当前地址）' : 'Webhook 地址',
        mode: credentialMode.value,
        state: editingNow && target !== null ? target.credential_state : '',
        version: editingNow && target !== null ? target.credential_version : 0,
        last4: editingNow && target !== null ? target.last4 : '',
        value: url.value,
        editableMode: true,
        disabled: busy.value,
        placeholder:
          credentialMode.value === 'managed'
            ? 'https://hooks.example.com/…'
            : 'ALERT_WEBHOOK 或 ${ALERT_WEBHOOK}',
        hint:
          credentialMode.value === 'managed'
            ? '输入原值，由系统加密保存；保存后不回显原值。更换地址会生成新的凭据版本，旧版本不会复活。'
            : '「环境变量」来源只保存变量名；部署者在该变量里配置完整的 Webhook 地址。',
        'onUpdate:value': (next: string) => {
          url.value = next
        },
        'onUpdate:mode': (next: CredentialMode) => {
          credentialMode.value = next
          error.value = ''
          problems.value = []
        },
      })
    }

    /** 撤销确认：二级确认之后才发请求；旧版本永不复活，重新录入生成新版本。 */
    // Revocation needs an explicit confirmation; entry after revocation creates new versions.
    function renderRevokeDialog(): VNodeChild {
      const target = revokeTarget.value
      return h(ConfirmDialog, {
        open: target !== null,
        title: '撤销加密凭据',
        message:
          target === null
            ? ''
            : `撤销「${target}」的加密凭据？撤销后所有旧版本立即不可用且永不复活；如需继续使用，必须重新录入（生成新的凭据版本）。`,
        confirmText: '确认撤销',
        busy: busy.value,
        onConfirm: () => {
          void confirmRevoke()
        },
        onCancel: cancelRevoke,
      })
    }

    return (): VNode => {
      const editing = editingName.value !== null
      const children: VNodeChild[] = []
      if (typesError.value !== '') {
        children.push(h(Notice, { tone: 'warn', text: typesError.value }))
      }
      children.push(
        textField('notify-name', '名称', () => name.value, (next) => (name.value = next), {
          placeholder: '例如 ops-alerts',
          disabled: editing,
        }),
        typeField(),
        addressField(),
        textField(
          'notify-description',
          '说明（可留空）',
          () => description.value,
          (next) => (description.value = next),
          { placeholder: '一句话说明这个渠道接收什么通知' },
        ),
      )
      if (!editing) {
        if (ownerAgentsError.value !== '') {
          children.push(h(Notice, { tone: 'warn', text: ownerAgentsError.value }))
        }
        children.push(
          h(ScopePicker, {
            id: 'notify-scope-create',
            scope: createScope.value,
            ownerAgent: createOwner.value,
            agents: ownerAgents.value,
            disabled: busy.value,
            'onUpdate:scope': (next: ResourceScope) => {
              createScope.value = next
              if (next === 'exclusive') {
                void ensureOwnerAgents()
              }
            },
            'onUpdate:ownerAgent': (next: string) => {
              createOwner.value = next
            },
          }),
          h(
            'p',
            { class: 'muted small' },
            '新建时即可选择范围：共享对所有 Agent 可用，专属只对所有者 Agent 可用；创建后仍可在「切换范围」里修改。',
          ),
        )
      }
      if (editing) {
        const target = editingChannel.value
        const host = target === null ? '（列表尚未重新读取）' : targetText(target)
        children.push(
          h(Notice, {
            tone: 'info',
            text: `正在修改「${editingName.value ?? ''}」；当前目标：${host}。地址留空 = 保留当前值。`,
          }),
        )
      }
      if (error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: error.value }))
      }
      if (problems.value.length > 0) {
        children.push(
          h(
            'ul',
            { class: 'problems' },
            problems.value.map((problem) => h('li', { key: problem }, problem)),
          ),
        )
      }
      if (forceTarget.value !== null) {
        children.push(
          h(Notice, {
            tone: 'warn',
            text: `渠道 ${forceTarget.value} 仍被以下 Agent 引用：${forceAgents.value.join('、')}。强制删除后这些 Agent 的下一次发送会失败。`,
          }),
        )
      }
      if (notice.value !== '') {
        children.push(h(Notice, { tone: noticeTone.value, text: notice.value }))
      }
      children.push(
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: busy.value,
              onClick: () => {
                void submit()
              },
            },
            busy.value ? '提交中…' : editing ? '保存修改' : '创建渠道',
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value,
              onClick: () => {
                resetForm()
              },
            },
            editing ? '取消修改' : '清空',
          ),
        ]),
      )
      children.push(
        h(
          'p',
          { class: 'muted small' },
          '「加密托管」来源的地址由系统加密保存、页面不回显；「环境变量」来源只接受 ${ENV_VAR} 占位。列表只显示目标服务（scheme + host）。',
        ),
      )
      return h('section', { ref: formRef, class: ['card', 'notify-channels'] }, [
        h('h2', editing ? '修改通知渠道' : '新建通知渠道'),
        ...children,
        renderScopePanel(),
        h('h2', '已配置渠道'),
        h(DataTable, {
          headers: [
            '渠道', 'UID', '类型', '说明', '目标服务', '范围', '所有者',
            '凭据状态', '地址状态', '最近实测', '文档引用的 Agent', '操作',
          ],
          rows: channelRows(),
          empty: '还没有配置通知渠道',
          columnClasses: [
            '', 'col-uid', '', '', 'col-host', 'col-short', 'col-name',
            'col-keys', 'col-address', 'col-state', 'col-refs', 'col-actions',
          ],
        }),
        h(
          'p',
          { class: 'muted small' },
          '「文档引用的 Agent」是对活动与归档 AGENT.md 的整词扫描（归档项以 archive: 标注），只是配置扫描出的引用线索，不能替代精确关联；每个 Agent 实际可用的渠道由它自己 notify_channels 声明中的关联决定。',
        ),
        h(
          'p',
          { class: 'muted small' },
          '「最近实测」只来自一次真实连通性测试的记录（固定文案「Basalt 连通性测试」，历史以管理操作台账为准）；凭据状态与地址状态只是本地配置读数，从不代表已验证。测试结果未知可能已送达，系统不会自动重发。',
        ),
        renderRevokeDialog(),
      ])
    }
  },
})
