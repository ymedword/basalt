// 资源新建 / 修改表单（页内组件，挂 /console/resources/:kind）：模型连接、MCP
// 与技能的创建与条件写修改（GET 详情取 ETag → PUT 带 If-Match）。
// 412 时重新读取 ETag 并**保留**用户输入；400 时展示信封里的 problems；写成功后
// 只发出事件——列表 / 详情由父页面重新 GET（注 20：不做乐观更新）。
// MCP 详情只回环境变量键名：已有映射时必须重新输入映射或显式勾选“清空”，
// 否则不发请求，避免整条替换时静默丢掉环境变量。
// 不解析 HTML、不执行脚本；技能导入只读文本文件，二进制与非法 UTF-8 在本地就拒绝。
// In-page create/edit form for model connections, MCP servers and skills. 412
// re-reads the ETag while keeping the user's input; the parent re-reads every
// list/detail after a write. MCP edits require re-entering env mappings because the
// detail exposes key names only and PUT replaces the whole entry.
import { computed, defineComponent, h, onMounted, ref } from 'vue'
import type { PropType, VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type {
  ApiClient,
  CredentialMode,
  CredentialState,
  InstanceSecretsView,
  McpCredentialKeyView,
  McpServerView,
  ModelConnectionView,
  ResourceScope,
  SkillView,
} from '../api'
import { adminApi, problemsOf } from '../api-admin'
import type {
  FileDraft,
  McpCredentialInput,
  McpServerApplyResult,
  McpServerDetail,
  McpServerUpsert,
  McpServerWrite,
  ModelConnectionUpsert,
  SkillDetail,
} from '../api-admin'
import {
  INSTANCE_SECRETS_TITLE,
  instanceSecretsDetail,
  instanceSecretsLabel,
  instanceSecretsTone,
  isInstanceSecretsView,
  recoveryReasonText,
} from '../instance-secrets'
import type { ResourceKind } from '../router'
import { CredentialField } from './credential-field'
import { ScopePicker } from './scope-picker'
import { ConfirmDialog, Notice, StatusBadge, formatTimestamp } from './ui'

const KIND_NOUNS: Record<ResourceKind, string> = {
  skills: '技能',
  'mcp-servers': 'MCP 服务',
  'model-connections': '模型连接',
}

const ENV_PLACEHOLDER = /^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/
const ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]*$/
// 请求头名按 RFC 7230 token：服务端的严格渲染是唯一判据，这里只做即时提示。
const HEADER_NAME = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/

/**
 * 「保存并应用」的结果：保存永远先完成，应用失败只影响第二个动作。父页面据此
 * 在提示里说清“已保存 / 应用是否切换”，不把两件事混成一句。
 * Save-and-apply outcome: the save always lands first; the second argument only
 * describes whether the run-version switch happened.
 */
export interface McpApplyOutcome {
  readonly result: McpServerApplyResult | null
  readonly error: string
}

/** 父页面重述的结论：文案 + 提示色调（未切换运行版本按 warn，不按 error）。 */
export interface CreatedApplyNotice {
  readonly text: string
  readonly tone: 'info' | 'warn'
}

/**
 * 新建结果的一句话结论。表单 emit 之后就被父页面卸载，它自己的提示随之消失，
 * 所以结论必须由父页面重述。保存永远先成功——三种没切成功的话都以「已创建并
 * 发布」开头；只有 `applied` 才说运行版本已切换。
 * The save always lands; only `applied` moved the run version. Parents must restate
 * this because the form unmounts as soon as it emits.
 */
export function createdApplyNotice(outcome: McpApplyOutcome | null | undefined): CreatedApplyNotice {
  if (outcome === null || outcome === undefined) {
    return { text: '已创建并发布。', tone: 'info' }
  }
  const result = outcome.result
  if (result === null) {
    return {
      text: `已创建并发布，但「应用」未完成：${outcome.error}。保存的版本仍是新的，运行版本保持不变，可在资源详情页重试。`,
      tone: 'warn',
    }
  }
  if (result.outcome === 'applied') {
    // 只有版本 ID，没有版本序号：不写成 vN，避免与列表的版本序号混用。
    return {
      text: `已创建、发布并应用：运行版本已切换（已应用版本 ID ${result.applied_version_id}，排空窗口 ${result.drain_timeout_seconds} 秒）。`,
      tone: 'info',
    }
  }
  return {
    text: `已创建并发布，但应用结果为「${result.outcome}」：运行版本未切换，详情页可查操作记录。`,
    tone: 'warn',
  }
}

/** 一个占位符的来源草稿：来源显式选择，托管值 write-only。 */
interface SlotDraft {
  readonly mode: CredentialMode
  readonly value: string
}

export const ResourceForm = defineComponent({
  name: 'ResourceForm',
  props: {
    api: { type: Object as PropType<ApiClient>, required: true },
    kind: { type: String as PropType<ResourceKind>, required: true },
    /** 非空 = 修改该资源（三类都以条件写提交新版本）；null = 新建。 */
    edit: { type: String as PropType<string | null>, default: null },
    /**
     * 新建时的预填载荷（当前只有 MCP 目录用它）：只给初始值，**不**触发保存，
     * 也不改变提交流程——用户仍要自己点“创建并发布”。
     * Prefill for a new MCP entry: initial values only; nothing is submitted.
     */
    prefill: { type: Object as PropType<McpServerWrite | null>, default: null },
  },
  emits: {
    // 第二个参数只对 MCP 的「保存并应用」出现：结果或失败文案，供父页面分行说明。
    created: (_view: unknown, _apply?: unknown) => true,
    saved: (_view: unknown, _apply?: unknown) => true,
    cancel: () => true,
  },
  setup(props, { emit }) {
    const client = adminApi(props.api)

    // --- 模型连接 ---------------------------------------------------------
    const name = ref('')
    const baseUrl = ref('')
    const defaultModel = ref('')
    // 凭据输入是 write-only 草稿：managed 模式提交完整 Key（不回显、保存后清空），
    // env 模式提交归一后的 `${VAR}` 引用。编辑态留空 = 保留当前凭据。
    // The credential draft is write-only: managed submits the full key (never echoed),
    // env submits a normalised ${VAR} reference, and a blank edit keeps the current one.
    const credentialMode = ref<CredentialMode>('managed')
    /** 最近一次详情读取的模式：切换模式的判据，不随下拉选择变化。 */
    const editingMode = ref<CredentialMode>('env')
    const credentialValue = ref('')
    const currentCredential = ref<{
      state: CredentialState
      version: number
      last4: string
      envRef: string
    }>({ state: '', version: 0, last4: '', envRef: '' })

    // 实例加密服务（设计 §2.4.4 / §2.6.5）：只读一次状态。只有**必然写不进去**的
    // 状态才灰掉「加密托管」——恢复态与显式纯 env；未初始化的存量实例首次保存时由
    // 服务端自动完成初始化，不构成禁用理由。读取失败**不**禁用：服务端仍是最终闸门。
    // Only states that provably cannot accept a managed write disable the option; a
    // legacy uninitialized instance initializes itself on first save.
    const instanceSecrets = ref<InstanceSecretsView | null>(null)
    const secretsReadError = ref('')
    const secretsError = ref('')
    const secretsNotice = ref('')
    const secretsBusy = ref(false)
    const secretsConfirmOpen = ref(false)

    async function loadInstanceSecrets(): Promise<void> {
      try {
        const view = await props.api.getInstanceSecrets()
        if (!isInstanceSecretsView(view)) {
          instanceSecrets.value = null
          secretsReadError.value = '实例加密状态返回了无法识别的响应'
          return
        }
        instanceSecrets.value = view
        secretsReadError.value = ''
      } catch (caught) {
        instanceSecrets.value = null
        secretsReadError.value = errorMessage(caught)
      }
    }

    /**
     * 保存成功后复核实例加密状态：首次托管保存可能刚刚完成了实例初始化，徽标与
     * 全站横幅要立即反映，不要求用户刷新页面或重启。
     * The first managed save may have initialised the instance; refresh in place.
     */
    function refreshInstanceSecretsAfterSave(): void {
      void loadInstanceSecrets().then(() => {
        window.dispatchEvent(new Event('basalt:instance-secrets-changed'))
      })
    }

    /** 初始化只对「未初始化」开放：已就绪不覆盖、恢复态不重建，两类拒绝都由服务端判。 */
    // Initialisation is offered only while uninitialized; the server refuses the rest.
    async function confirmInitSecrets(): Promise<void> {
      if (secretsBusy.value) {
        return
      }
      secretsBusy.value = true
      secretsError.value = ''
      try {
        const view = await client.initInstanceSecrets()
        if (!isInstanceSecretsView(view)) {
          secretsError.value = '初始化已提交，但返回的实例状态无法识别；请刷新页面确认结果。'
          return
        }
        instanceSecrets.value = view
        secretsReadError.value = ''
        secretsConfirmOpen.value = false
        secretsNotice.value = `实例加密服务已初始化（key_id ${view.key_id}）；后续保存的托管凭据都用这把密钥加密。`
        window.dispatchEvent(new Event('basalt:instance-secrets-changed'))
      } catch (caught) {
        // 409 也走这里：已就绪不覆盖、恢复态不重建——两类拒绝都不产生写入。
        secretsError.value = errorMessage(caught)
      } finally {
        secretsBusy.value = false
      }
    }

    const managedUnavailable = computed(() => {
      const view = instanceSecrets.value
      if (view === null) {
        return false
      }
      // 未初始化只有在明确不能就地初始化（显式纯 env）时才禁用。
      return (
        view.state === 'recovery_required' ||
        (view.state === 'uninitialized' && !view.can_initialize)
      )
    })
    const managedReason = computed(() => {
      const view = instanceSecrets.value
      if (view === null) {
        return ''
      }
      if (view.state === 'recovery_required') {
        return `实例加密服务需要恢复（${recoveryReasonText(view.recovery_reason)}）：托管凭据无法解密，新的托管写入会被拒绝；请按 docs/Operations.md 恢复主密钥，或改用「环境变量」。`
      }
      if (view.state === 'uninitialized' && !view.can_initialize) {
        return '此实例以纯环境变量方式部署（secrets.auto_init: false），不接受加密托管：请改用「环境变量」来源，或由部署者启用 secrets.master_key_file 后重启。'
      }
      return ''
    })

    // 新建时的使用范围：共享是默认；选专属时才读取所有者候选（在世 Agent 名）。
    // Create-time scope; the owner list is fetched only when exclusive is chosen.
    const createScope = ref<ResourceScope>('shared')
    const createOwner = ref('')
    const scopeAgents = ref<readonly string[]>([])
    const scopeAgentsError = ref('')
    const scopeAgentsLoading = ref(false)

    // --- MCP --------------------------------------------------------------
    const transport = ref<'stdio' | 'http'>('stdio')
    const command = ref('')
    const argsText = ref('')
    const url = ref('')
    const envText = ref('')
    const headersText = ref('')
    // 详情只回键名：编辑已有映射时要求重输，或显式勾选清空。请求头同理——PUT 整条
    // 替换，不重输就等于静默删掉已有头。
    // The detail exposes key names only; the PUT replaces the whole entry, headers
    // included, so an edit must re-enter them or clear them explicitly.
    const currentEnvKeys = ref<string[]>([])
    const currentHeaderKeys = ref<string[]>([])
    const clearEnv = ref(false)
    const clearHeaders = ref(false)
    // 逐占位符来源（14D）：键是槽位名（env 键名或请求头名），值只在这里存在。
    // 来源不靠“托管行是否存在”推断——每个槽位都要一个显式选择。
    // Per-slot source drafts; the source is an explicit choice, never inferred.
    const slotDrafts = ref<Record<string, SlotDraft>>({})
    // 编辑态读到的槽位现状（来源 / 状态 / 尾号）；新建态为空。
    const currentSlots = ref<McpCredentialKeyView[]>([])

    // --- 技能 -------------------------------------------------------------
    const skillMode = ref<'create' | 'import'>('create')
    const description = ref('')
    const importFiles = ref<FileDraft[]>([])
    const skillFiles = ref<FileDraft[]>([])

    // --- 通用状态 ---------------------------------------------------------
    const etag = ref('')
    const loading = ref(false)
    const busy = ref(false)
    const error = ref('')
    const problems = ref<string[]>([])
    const notice = ref('')

    const editing = computed(() => props.edit !== null)
    const noun = computed(() => KIND_NOUNS[props.kind])
    const heading = computed(() => (editing.value ? `修改${noun.value}` : `新建${noun.value}`))

    /**
     * 用目录配方填表：args 每行一个，env 逐条写成 `KEY=${VAR}`。这里只搬值，
     * 不做校验也不提交——保存仍走原来的建版 + env 闸门。
     * Applies a catalog recipe into the fields; it never submits.
     */
    function applyMcpPrefill(write: McpServerWrite): void {
      name.value = write.name
      transport.value = write.transport
      command.value = write.command ?? ''
      argsText.value = (write.args ?? []).join('\n')
      url.value = write.url ?? ''
      envText.value = Object.entries(write.env ?? {})
        .map(([key, reference]) => `${key}=${reference}`)
        .join('\n')
      headersText.value = Object.entries(write.headers ?? {})
        .map(([key, reference]) => `${key}=${reference}`)
        .join('\n')
      currentEnvKeys.value = []
      currentHeaderKeys.value = []
      currentSlots.value = []
      // 配方不预设来源：新建时默认「加密托管」，由用户逐个确认或改成环境变量。
      slotDrafts.value = {}
      clearEnv.value = false
      clearHeaders.value = false
    }

    /** 修改入口依赖详情 ETag；三类资源都先 GET 详情再填表。 */
    // Edits always start from the detail GET, which is also the ETag source.
    onMounted(() => {
      // 只有模型连接表单需要实例加密状态；技能与 MCP 不为此多发一次请求。
      // Only the model-connection form reads the instance-key projection.
      if (props.kind === 'model-connections') {
        void loadInstanceSecrets()
      }
      if (props.edit !== null) {
        void loadEdit()
        return
      }
      // 预填只在新建态生效：修改态以服务端真值为准，避免配方覆盖已有配置。
      // Prefill applies to creation only; edits always start from server truth.
      if (props.kind === 'mcp-servers' && props.prefill !== null) {
        applyMcpPrefill(props.prefill)
      }
    })

    async function loadEdit(): Promise<void> {
      const current = props.edit
      if (current === null) {
        return
      }
      loading.value = true
      error.value = ''
      try {
        if (props.kind === 'model-connections') {
          const detail = await client.getModelConnection(current)
          applyModelConnection(detail.data, detail.etag)
          return
        }
        if (props.kind === 'mcp-servers') {
          const detail = await client.getMcpServer(current)
          applyMcpServer(detail.data, detail.etag)
          return
        }
        const detail = await client.getSkill(current)
        applySkill(detail.data, detail.etag)
      } catch (caught) {
        error.value = errorMessage(caught)
      } finally {
        loading.value = false
      }
    }

    /** 用 MCP 详情填表；args 每行一个，env / headers 只回键名、留空等待重输。 */
    // MCP detail: args one per line; env and header values are never echoed back.
    function applyMcpServer(view: McpServerDetail, etagValue: string): void {
      name.value = view.name
      transport.value = view.transport
      command.value = view.command
      argsText.value = view.args.join('\n')
      url.value = view.url
      envText.value = ''
      headersText.value = ''
      currentEnvKeys.value = [...view.env_keys]
      currentHeaderKeys.value = [...view.header_keys]
      // 已有声明沿用为草稿初值：保存时若默认成「加密托管」，会把 env 槽位改成托管。
      currentSlots.value = [...view.credential_keys]
      slotDrafts.value = {}
      clearEnv.value = false
      clearHeaders.value = false
      etag.value = etagValue
    }

    /** 用技能详情填表：完整文件清单（含正文）可逐份编辑。 */
    // Skill detail: every file is editable text; the list is replaced as a whole.
    function applySkill(view: SkillDetail, etagValue: string): void {
      name.value = view.name
      description.value = view.description
      skillFiles.value = view.files.map((file) => ({
        path: file.path,
        content: file.content,
      }))
      etag.value = etagValue
    }

    /**
     * 用服务端真值填表：凭据草稿恒重置为空（页面不留旧值），只保留模式、状态、
     * 版本与尾号；编辑时留空即保留当前凭据（契约 ModelConnectionUpsert）。
     * The credential draft is always cleared; only mode/state/version/last4 are kept.
     */
    function applyModelConnection(view: ModelConnectionView, etagValue: string): void {
      name.value = view.name
      baseUrl.value = view.base_url
      defaultModel.value = view.default_model
      credentialMode.value = view.credential_mode
      editingMode.value = view.credential_mode
      credentialValue.value = ''
      currentCredential.value = {
        state: view.credential_state,
        version: view.credential_version,
        last4: view.last4,
        envRef: view.api_key_env,
      }
      etag.value = etagValue
    }

    /** 只在需要选所有者时读取在世 Agent 名；失败留在面板里提示，不静默留空。 */
    async function ensureScopeAgents(): Promise<void> {
      if (scopeAgents.value.length > 0 || scopeAgentsLoading.value) {
        return
      }
      scopeAgentsLoading.value = true
      try {
        const profiles = await props.api.listProfiles()
        scopeAgents.value = profiles.map((profile) => profile.name)
        scopeAgentsError.value = ''
      } catch (caught) {
        scopeAgentsError.value = `读取 Agent 列表失败，所有者候选暂不可用：${errorMessage(caught)}`
      } finally {
        scopeAgentsLoading.value = false
      }
    }

    /** 412 后的最小刷新：只前移 ETag，不覆盖用户已改的字段。 */
    async function refreshEtag(current: string): Promise<void> {
      switch (props.kind) {
        case 'model-connections':
          etag.value = (await client.getModelConnection(current)).etag
          return
        case 'mcp-servers':
          etag.value = (await client.getMcpServer(current)).etag
          return
        case 'skills':
          etag.value = (await client.getSkill(current)).etag
      }
    }

    /** 写成功后重新读取权威详情（不做乐观更新）。 */
    // Re-read the authoritative detail after a write; never patch the form optimistically.
    async function rereadModelConnection(current: string): Promise<ModelConnectionView> {
      const detail = await client.getModelConnection(current)
      applyModelConnection(detail.data, detail.etag)
      return detail.data
    }

    async function rereadMcpServer(current: string): Promise<McpServerView> {
      const detail = await client.getMcpServer(current)
      applyMcpServer(detail.data, detail.etag)
      return detail.data
    }

    async function rereadSkill(current: string): Promise<SkillView> {
      const detail = await client.getSkill(current)
      applySkill(detail.data, detail.etag)
      return detail.data
    }

    function resetFeedback(): void {
      error.value = ''
      problems.value = []
      notice.value = ''
    }

    /**
     * 统一失败分支：412 只在修改路径出现——刷新 ETag 并保留输入；其它错误按
     * 契约信封展示固定文案与 problems（problems 是补充，不替代文案）。
     * 412 refreshes the ETag only; other failures show the envelope message.
     */
    async function handleFailure(caught: unknown, refresh: (() => Promise<void>) | null): Promise<void> {
      if (caught instanceof ApiError && caught.status === 412 && refresh !== null) {
        error.value = '资源已被他人修改，本次保存没有写入；已重新读取 ETag，请确认内容后再提交'
        try {
          await refresh()
        } catch (refreshError) {
          error.value = `${error.value}（重新读取 ETag 失败：${errorMessage(refreshError)}）`
        }
        return
      }
      problems.value = problemsOf(caught)
      error.value = errorMessage(caught)
    }

    // --- 提交 -------------------------------------------------------------

    async function submit(wantApply = false): Promise<void> {
      if (busy.value) {
        return
      }
      resetFeedback()
      if (props.kind === 'skills') {
        await submitSkill()
        return
      }
      if (props.kind === 'model-connections') {
        await submitModelConnection()
        return
      }
      await submitMcpServer(wantApply)
    }

    /**
     * 把“环境变量名”归一成契约要求的 `${VAR}`；同时接受粘贴完整占位。
     * 返回 null = 输入不合法。错误文案不回显输入——用户可能把真实密钥粘了进来。
     * Normalises an environment-variable name (or a pasted placeholder) into the
     * contract's ${VAR} form; the error never echoes the input.
     */
    function apiKeyReference(raw: string): string | null {
      const trimmed = raw.trim()
      const inner =
        trimmed.startsWith('${') && trimmed.endsWith('}')
          ? trimmed.slice(2, -1)
          : trimmed
      return ENV_KEY.test(inner) ? '${' + inner + '}' : null
    }

    /**
     * 组装模型连接请求：凭据两种模式与三种情形（新建必填 / 编辑留空保留 / 切换模式
     * 必须带新值）都在本地先判，错误文案不回显输入。
     * Builds the upsert: managed takes the full key, env takes a ${VAR} reference, an
     * empty edit keeps the current credential, and a mode switch always needs a value.
     */
    function modelConnectionBody(): ModelConnectionUpsert | null {
      const body: ModelConnectionUpsert = {
        name: name.value.trim(),
        base_url: baseUrl.value.trim(),
        default_model: defaultModel.value.trim(),
      }
      if (body.name === '' || body.base_url === '' || body.default_model === '') {
        error.value = '请填写名称、Base URL 与默认模型'
        return null
      }
      const creating = props.edit === null
      const raw = credentialValue.value
      const trimmed = raw.trim()
      if (trimmed !== '') {
        if (credentialMode.value === 'managed') {
          if (ENV_PLACEHOLDER.test(trimmed)) {
            error.value = '「加密托管」来源不接受 ${VAR} 占位：请粘贴完整的 API 密钥'
            return null
          }
          if (trimmed !== raw) {
            error.value = 'API 密钥首尾不得含空白'
            return null
          }
          body.api_key = raw
        } else {
          const reference = apiKeyReference(trimmed)
          if (reference === null) {
            error.value =
              '「环境变量」来源只填写变量名（例如 DEEPSEEK_API_KEY），不填写真实密钥；' +
              '变量名只能包含字母、数字与下划线，且不能以数字开头'
            return null
          }
          body.api_key = reference
        }
        body.credential_mode = credentialMode.value
      } else if (creating) {
        error.value =
          credentialMode.value === 'managed'
            ? '请填写完整的 API 密钥（加密托管）；保存后不回显原值'
            : '请填写变量名（例如 DEEPSEEK_API_KEY）；变量值由部署者在进程环境中配置'
        return null
      } else if (credentialMode.value !== editingMode.value) {
        error.value = '切换凭据来源必须填写新值（加密托管：完整 API 密钥；环境变量：变量名）'
        return null
      }
      if (creating) {
        body.scope = createScope.value
        if (createScope.value === 'exclusive') {
          const owner = createOwner.value.trim()
          if (owner === '') {
            error.value = '专属连接必须选择所有者 Agent'
            return null
          }
          body.owner_agent = owner
        }
      }
      return body
    }

    async function submitModelConnection(): Promise<void> {
      const body = modelConnectionBody()
      if (body === null) {
        return
      }
      busy.value = true
      try {
        const current = props.edit
        if (current === null) {
          const view = await client.createModelConnection(body)
          credentialValue.value = ''
          notice.value = '已创建并发布；列表将从服务端重新读取。'
          refreshInstanceSecretsAfterSave()
          emit('created', view)
          return
        }
        await client.updateModelConnection(current, body, etag.value)
        const view = await rereadModelConnection(current)
        notice.value = '已保存并发布；上面是重新读取的服务端真值。'
        refreshInstanceSecretsAfterSave()
        emit('saved', view)
      } catch (caught) {
        const current = props.edit
        await handleFailure(
          caught,
          current === null ? null : () => refreshEtag(current),
        )
      } finally {
        busy.value = false
      }
    }

    /** 按行拆分；空行忽略（args 与 env 的输入形态）。 */
    function lines(text: string): string[] {
      return text
        .split('\n')
        .map((line) => line.trim())
        .filter((line) => line !== '')
    }

    /**
     * 逐行解析 `KEY=${VAR}`（env 与 headers 同一形态）；不合法就在本地拒绝，不发请求。
     * 错误文案不回显“值”以外的东西——它只可能是 `${VAR}`，不是秘密。
     */
    // Parses KEY=${VAR} mappings for either env keys or header names.
    function collectMappings(
      text: string,
      label: string,
      pattern: RegExp,
      example: string,
    ): Record<string, string> | null {
      const out: Record<string, string> = {}
      for (const line of lines(text)) {
        const separator = line.indexOf('=')
        const key = separator === -1 ? '' : line.slice(0, separator).trim()
        const value = separator === -1 ? '' : line.slice(separator + 1).trim()
        if (!pattern.test(key) || !ENV_PLACEHOLDER.test(value)) {
          error.value = `${label}必须是 KEY=\${VAR} 的占位，例如 ${example}：${line}`
          return null
        }
        out[key] = value
      }
      return out
    }

    function collectEnv(): Record<string, string> | null {
      return collectMappings(envText.value, '环境变量', ENV_KEY, 'TOKEN=${MCP_TOKEN}')
    }

    function collectHeaders(): Record<string, string> | null {
      return collectMappings(headersText.value, '请求头', HEADER_NAME, 'Authorization=${MCP_TOKEN}')
    }

    /** 映射文本里的槽位名（宽松解析：只取能识别的行，错误留给提交时点名）。 */
    function slotKeysOf(text: string, pattern: RegExp): string[] {
      const keys: string[] = []
      for (const line of lines(text)) {
        const separator = line.indexOf('=')
        const key = separator === -1 ? '' : line.slice(0, separator).trim()
        if (pattern.test(key)) {
          keys.push(key)
        }
      }
      return keys
    }

    /**
     * 本次保存声明的槽位：env 键名 + （http 的）请求头名，按字典序。
     * 页面从映射文本推导槽位，不新增第二个声明入口；每个槽位下面单独选来源。
     */
    // The declared slots of this save, derived from the mapping text itself.
    const declaredSlots = computed<readonly string[]>(() => {
      const names = new Set(slotKeysOf(envText.value, ENV_KEY))
      if (transport.value === 'http') {
        for (const name of slotKeysOf(headersText.value, HEADER_NAME)) {
          names.add(name)
        }
      }
      return [...names].sort()
    })

    /** 一个槽位在定义文件里的 `${VAR}`（env 或 headers 谁有取谁）。 */
    function slotReference(key: string): string {
      for (const text of [envText.value, headersText.value]) {
        for (const line of lines(text)) {
          const separator = line.indexOf('=')
          if (separator !== -1 && line.slice(0, separator).trim() === key) {
            return line.slice(separator + 1).trim()
          }
        }
      }
      return ''
    }

    function currentSlot(key: string): McpCredentialKeyView | null {
      return currentSlots.value.find((slot) => slot.key === key) ?? null
    }

    /**
     * 草稿初值：已有声明以服务端为准（编辑时不把 env 槽位悄悄改成托管），
     * 新槽位按设计默认「加密托管」。
     * Existing declarations win in edit mode; new slots default to managed.
     */
    function slotDraft(key: string): SlotDraft {
      const drafted = slotDrafts.value[key]
      if (drafted !== undefined) {
        return drafted
      }
      return { mode: currentSlot(key)?.mode ?? 'managed', value: '' }
    }

    function setSlotDraft(key: string, patch: Partial<SlotDraft>): void {
      slotDrafts.value = { ...slotDrafts.value, [key]: { ...slotDraft(key), ...patch } }
    }

    /** 槽位现状 → CredentialField 的状态色调（env 不显示状态徽章）。 */
    function slotCredentialState(key: string): CredentialState {
      switch (currentSlot(key)?.state) {
        case 'managed':
          return 'active'
        case 'managed_revoked':
          return 'revoked'
        default:
          return ''
      }
    }

    /**
     * 逐槽位来源声明（契约 `credentials`）：managed 可带新值（省略 = 沿用），env
     * 从不带值；没有旧值又没填值、或把仍有活动托管值的槽位改成 env，都在本地拒绝。
     * 页面不猜来源，也不悄悄丢掉一个活动托管值。
     * Builds the explicit per-slot declarations; local guards mirror the contract.
     */
    function mcpCredentialInputs(slots: readonly string[]): McpCredentialInput[] | null {
      const inputs: McpCredentialInput[] = []
      for (const key of slots) {
        const draft = slotDraft(key)
        const existing = currentSlot(key)
        if (draft.mode === 'env') {
          if (existing !== null && existing.state === 'managed') {
            error.value =
              `占位符 ${key} 仍有活动托管值：要改成「环境变量」请先在资源详情页撤销该占位符` +
              '（撤销后不会回落到进程环境）'
            return null
          }
          inputs.push({ key, mode: 'env' })
          continue
        }
        const value = draft.value
        if (value === '') {
          if (existing === null || existing.state !== 'managed') {
            error.value =
              `占位符 ${key} 选择了「加密托管」但还没有值：请填写值，` +
              '或把来源改成「环境变量」（要求进程环境里已有该变量）'
            return null
          }
          inputs.push({ key, mode: 'managed' })
          continue
        }
        if (value.trim() !== value) {
          error.value = `占位符 ${key} 的凭据值首尾不得含空白`
          return null
        }
        if (ENV_PLACEHOLDER.test(value)) {
          error.value = `占位符 ${key} 选择「加密托管」时不接受 \${VAR} 占位：请粘贴完整的值`
          return null
        }
        inputs.push({ key, mode: 'managed', value })
      }
      return inputs
    }

    function mcpServerBody(): McpServerUpsert | null {
      const body: McpServerUpsert = { name: name.value.trim(), transport: transport.value }
      if (body.name === '') {
        error.value = '请填写 MCP 服务名称'
        return null
      }
      if (transport.value === 'stdio') {
        if (command.value.trim() === '') {
          error.value = 'stdio 传输必须填写可执行名'
          return null
        }
        body.command = command.value.trim()
        const args = lines(argsText.value)
        if (args.length > 0) {
          body.args = args
        }
      } else {
        if (url.value.trim() === '') {
          error.value = 'http 传输必须填写 URL；凭证不得出现在 URL 中'
          return null
        }
        body.url = url.value.trim()
      }
      const typedEnv = collectEnv()
      if (typedEnv === null) {
        return null
      }
      const typedCount = Object.keys(typedEnv).length
      if (props.edit !== null && currentEnvKeys.value.length > 0) {
        // PUT 是整条替换，详情又只回键名：不给值就等于把已有映射删掉。
        // The PUT replaces the entry and the detail only exposes key names.
        if (typedCount === 0 && !clearEnv.value) {
          error.value =
            `该服务已有环境变量映射（${currentEnvKeys.value.join('、')}），` +
            '但页面拿不到它们的值。请重新填写 KEY=${VAR} 后保存，或勾选“清空环境变量映射”。'
          return null
        }
        if (typedCount > 0 && clearEnv.value) {
          error.value = '“清空环境变量映射”与重新填写只能选一个'
          return null
        }
      }
      let typedHeaders: Record<string, string> = {}
      if (transport.value === 'http') {
        const collected = collectHeaders()
        if (collected === null) {
          return null
        }
        typedHeaders = collected
        const headerCount = Object.keys(typedHeaders).length
        if (props.edit !== null && currentHeaderKeys.value.length > 0) {
          if (headerCount === 0 && !clearHeaders.value) {
            error.value =
              `该服务已有请求头占位（${currentHeaderKeys.value.join('、')}），` +
              '但页面拿不到它们的值。请重新填写头名=${VAR} 后保存，或勾选“清空请求头占位”。'
            return null
          }
          if (headerCount > 0 && clearHeaders.value) {
            error.value = '“清空请求头占位”与重新填写只能选一个'
            return null
          }
        }
        for (const name of Object.keys(typedHeaders)) {
          if (name in typedEnv) {
            error.value =
              `占位符 ${name} 同时出现在环境变量与请求头里：同一个名字只能占一个槽位，请改名后重试`
            return null
          }
        }
      }
      if (typedCount > 0) {
        body.env = typedEnv
      }
      if (Object.keys(typedHeaders).length > 0) {
        body.headers = typedHeaders
      }
      const slots = [...new Set([...Object.keys(typedEnv), ...Object.keys(typedHeaders)])].sort()
      if (slots.length > 0) {
        const credentials = mcpCredentialInputs(slots)
        if (credentials === null) {
          return null
        }
        body.credentials = credentials
      }
      return body
    }

    /**
     * 保存（= 发布新版本），可选再立即「应用」一次。
     *
     * 保存与应用是两件事：保存成功后重新读取 ETag 再应用；应用失败**不改写**保存
     * 结论，父页面通过第二个 emit 参数拿到结果并在提示里分开说。这里绝不自动重发。
     * Save always lands first; apply is a second, conditional write on the fresh ETag.
     */
    async function submitMcpServer(wantApply: boolean): Promise<void> {
      const body = mcpServerBody()
      if (body === null) {
        return
      }
      busy.value = true
      try {
        const current = props.edit
        let view: McpServerView
        let applyOutcome: McpApplyOutcome | null
        if (current === null) {
          view = await client.createMcpServer(body)
          // 新建没有 ETag：重新读一次详情取当前指针，再作为应用的条件。
          applyOutcome = wantApply ? await applyAfterSave(view.name) : null
        } else {
          await client.updateMcpServer(current, body, etag.value)
          view = await rereadMcpServer(current)
          applyOutcome = wantApply ? await applyAfterSave(current) : null
        }
        notice.value =
          applyOutcome === null
            ? '已保存并发布；MCP 连接在重启后生效（详情会标“待应用”）。'
            : applyNotice(applyOutcome)
        if (current === null) {
          emit('created', view, applyOutcome)
        } else {
          emit('saved', view, applyOutcome)
        }
      } catch (caught) {
        const current = props.edit
        await handleFailure(caught, current === null ? null : () => refreshEtag(current))
      } finally {
        busy.value = false
      }
    }

    /** 保存后重新读取详情（取当前 ETag）再应用；失败只回报，不重试。 */
    async function applyAfterSave(name: string): Promise<McpApplyOutcome> {
      try {
        const detail = await client.getMcpServer(name)
        etag.value = detail.etag
        const result = await client.applyMcpServer(name, detail.etag)
        return { result, error: '' }
      } catch (caught) {
        return { result: null, error: errorMessage(caught) }
      }
    }

    /** 保存 + 应用两段的提示文案：成功说清切换了哪个版本，失败说清保存已生效。 */
    function applyNotice(outcome: McpApplyOutcome): string {
      const result = outcome.result
      if (result === null) {
        return `已保存并发布，但「应用」未完成：${outcome.error}。保存的版本仍是新的，运行版本保持不变；请到资源详情页重试。`
      }
      if (result.outcome === 'applied') {
        // 这里只有版本 ID，没有版本序号：不把它写成 vN，避免和详情页的序号混用。
        return `已保存、发布并应用：运行版本已切换（已应用版本 ID ${result.applied_version_id}），排空窗口 ${result.drain_timeout_seconds} 秒。`
      }
      return `已保存并发布，但应用结果为「${result.outcome}」：运行版本未切换，请到资源详情页查看操作记录。`
    }

    /** 修改态的文件清单：路径非空、不重复；正文原样提交（服务端做完整校验）。 */
    // Edit-mode files: non-empty unique paths; the server validates the whole list.
    function skillFileBody(): FileDraft[] | null {
      if (skillFiles.value.length === 0) {
        error.value = '技能至少要有一个文件（含 SKILL.md）'
        return null
      }
      const seen = new Set<string>()
      const drafts: FileDraft[] = []
      for (const file of skillFiles.value) {
        const path = file.path.trim()
        if (path === '') {
          error.value = '文件路径不能为空'
          return null
        }
        if (seen.has(path)) {
          error.value = `文件路径重复：${path}`
          return null
        }
        seen.add(path)
        drafts.push({ path, content: file.content })
      }
      return drafts
    }

    async function submitSkill(): Promise<void> {
      const trimmed = name.value.trim()
      if (trimmed === '') {
        error.value = '请填写技能名称'
        return
      }
      const editingFiles = props.edit === null ? null : skillFileBody()
      if (props.edit !== null && editingFiles === null) {
        return
      }
      busy.value = true
      try {
        if (props.edit !== null && editingFiles !== null) {
          await client.updateSkill(props.edit, editingFiles, etag.value)
          const view = await rereadSkill(props.edit)
          notice.value = '已保存并发布新版本；文件清单已从服务端重新读取。'
          emit('saved', view)
          return
        }
        let view: SkillView
        if (skillMode.value === 'import') {
          if (importFiles.value.length === 0) {
            error.value = '请选择技能目录：只导入 UTF-8 文本文件'
            return
          }
          view = await client.importSkill({ name: trimmed, files: importFiles.value })
        } else {
          view = await client.createSkill({ name: trimmed, description: description.value.trim() })
        }
        notice.value = '已创建；技能内容将作为不可变版本发布。'
        emit('created', view)
      } catch (caught) {
        const current = props.edit
        await handleFailure(caught, current === null ? null : () => refreshEtag(current))
      } finally {
        busy.value = false
      }
    }

    // --- 技能目录选择 -----------------------------------------------------

    /**
     * 目录选择只读取文本：非法 UTF-8 或含 NUL 的文件在本地拒绝，**不**上传；
     * 路径去掉浏览器附加的根目录名，与 `SkillImport` 的相对路径口径一致。
     * Reads text files only; the browser-added root segment is stripped.
     */
    async function pickDirectory(event: Event): Promise<void> {
      const input = event.target as HTMLInputElement
      const picked = input.files
      error.value = ''
      importFiles.value = []
      if (picked === null || picked.length === 0) {
        return
      }
      const drafts: FileDraft[] = []
      let root = ''
      for (const file of Array.from(picked)) {
        const raw = (file.webkitRelativePath !== '' ? file.webkitRelativePath : file.name).replace(
          /\\/g,
          '/',
        )
        const slash = raw.indexOf('/')
        if (slash !== -1) {
          root = raw.slice(0, slash)
        }
        const path = slash === -1 ? raw : raw.slice(slash + 1)
        let text: string
        try {
          text = new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer())
        } catch {
          error.value = `只能导入 UTF-8 文本文件：${path}`
          return
        }
        if (text.includes('\u0000')) {
          error.value = `只能导入文本文件：${path}`
          return
        }
        drafts.push({ path, content: text })
      }
      importFiles.value = drafts
      if (name.value.trim() === '' && root !== '') {
        name.value = root
      }
    }

    // --- 渲染 -------------------------------------------------------------

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

    /**
     * 普通表单的第一行：实例加密服务状态。托管默认可用——未初始化的存量实例在
     * 首次保存时自动完成初始化，所以普通流程里没有「请先初始化」这一步；主密钥
     * 路径、初始化细节与手动入口都收进「高级：实例加密诊断」折叠区。
     * First line of the ordinary form: the instance-key status. Managed is usable
     * even before initialisation because the first save initialises the instance.
     */
    function renderInstanceSecretsStatus(): VNode {
      const view = instanceSecrets.value
      const parts: VNodeChild[] = [h('span', {}, INSTANCE_SECRETS_TITLE)]
      if (view === null) {
        parts.push(
          h(StatusBadge, {
            label: secretsReadError.value === '' ? '读取中' : '状态未知',
            tone: 'muted',
          }),
        )
      } else {
        parts.push(
          h(StatusBadge, { label: instanceSecretsLabel(view), tone: instanceSecretsTone(view) }),
        )
      }
      const readFailed = view === null && secretsReadError.value !== ''
      return h('div', {}, [
        h('p', { class: 'credential-status' }, parts),
        h(
          'p',
          { class: 'field-hint muted' },
          readFailed
            ? `读取实例加密状态失败：${secretsReadError.value}。保存时不因读取失败而拦下；服务端仍是最终闸门，不能写入会拒绝并说明原因。`
            : view === null
              ? '正在读取实例加密状态…'
              : instanceSecretsDetail(view),
        ),
        view === null ? null : renderSecretsAdvanced(view),
        secretsError.value === '' ? null : h(Notice, { tone: 'error', text: secretsError.value }),
        secretsNotice.value === '' ? null : h(Notice, { tone: 'info', text: secretsNotice.value }),
      ])
    }

    /**
     * 「高级：实例加密诊断」：主密钥路径、key_id、初始化时间与手动初始化入口。
     * 这些是管理诊断信息，不占据普通连接创建流程；恢复仍按 docs/Operations.md 处置。
     * Advanced diagnostics: key path, key id, init time and the manual init action.
     * Recovery still follows docs/Operations.md instead of a button here.
     */
    function renderSecretsAdvanced(view: InstanceSecretsView): VNode {
      const rows: VNodeChild[] = [
        h('p', {}, ['key_id：', view.key_id === '' ? '—' : h('code', {}, view.key_id)]),
        h('p', {}, [
          '初始化时间：',
          view.initialized_at === '' ? '—' : formatTimestamp(view.initialized_at),
        ]),
        h('p', {}, [
          '主密钥文件：',
          view.key_file === '' ? '—' : h('code', {}, view.key_file),
        ]),
      ]
      if (view.state === 'uninitialized') {
        rows.push(
          h(
            'p',
            { class: 'muted small' },
            view.can_initialize
              ? '尚未初始化：首次保存加密托管凭据时会自动完成；也可以在这里手动初始化。'
              : '此实例不接受加密托管（secrets.auto_init: false 或未配置主密钥路径）；如需启用，由部署者完成配置后重启。',
          ),
        )
      }
      if (view.can_initialize) {
        rows.push(
          h(
            'button',
            {
              type: 'button',
              disabled: secretsBusy.value,
              onClick: () => {
                secretsConfirmOpen.value = true
              },
            },
            secretsBusy.value ? '初始化中…' : '初始化实例加密',
          ),
        )
      }
      return h('details', { class: 'technical-details' }, [
        h('summary', {}, '高级：实例加密诊断'),
        ...rows,
      ])
    }

    /** 初始化确认框：说清只做一次、失败不覆盖、备份要带密钥文件。 */
    // Confirmation for initialisation: one-shot, never overwrites, back up the key file.
    function renderSecretsDialog(): VNode {
      return h(ConfirmDialog, {
        open: secretsConfirmOpen.value,
        title: '初始化实例加密',
        message:
          '初始化会在实例私有目录生成主密钥文件并登记 key_id。这是只做一次的动作：已就绪的实例不会被覆盖，恢复态也不会重建密钥。' +
          '请确认主密钥文件会与工作区一起备份——只搬工作区会让托管凭据无法解密。',
        confirmText: secretsBusy.value ? '初始化中…' : '初始化',
        busy: secretsBusy.value,
        onConfirm: () => {
          void confirmInitSecrets()
        },
        onCancel: () => {
          if (!secretsBusy.value) {
            secretsConfirmOpen.value = false
          }
        },
      })
    }

    function modelCredentialField(): VNode {
      return h(CredentialField, {
        id: 'resource-api-key',
        label: editing.value ? 'API 密钥（留空 = 保留当前凭据）' : 'API 密钥',
        // 两行分开：「实例加密服务」在上，「当前 API 密钥是否已保存」在这一行；
        // 来源选择收进「高级」，默认就是加密托管。
        statusTitle: '当前 API 密钥',
        advancedSource: true,
        mode: credentialMode.value,
        state: editing.value ? currentCredential.value.state : '',
        version: editing.value ? currentCredential.value.version : 0,
        last4: editing.value ? currentCredential.value.last4 : '',
        reference: editing.value ? currentCredential.value.envRef : '',
        value: credentialValue.value,
        editableMode: true,
        disabled: busy.value,
        managedDisabled: managedUnavailable.value,
        managedReason: managedReason.value,
        placeholder:
          credentialMode.value === 'managed'
            ? '粘贴完整的 API 密钥（保存后不回显原值）'
            : 'DEEPSEEK_API_KEY 或 ${DEEPSEEK_API_KEY}',
        hint:
          credentialMode.value === 'managed'
            ? '输入原值，由系统加密保存；保存后不回显原值。更换密钥会生成新的凭据版本，旧版本不会复活。'
            : '「环境变量」来源只保存变量名；变量值由部署者在进程环境中配置，修改后需重启。',
        'onUpdate:value': (next: string) => {
          credentialValue.value = next
        },
        'onUpdate:mode': (next: CredentialMode) => {
          credentialMode.value = next
          error.value = ''
          problems.value = []
        },
      })
    }

    function createScopePicker(): VNodeChild[] {
      if (editing.value) {
        return [
          h(
            'p',
            { class: 'muted small' },
            '修改使用范围请到资源详情页的范围卡片；仍被 Agent 配置引用的共享连接转专属会被服务端拒绝并点名对应的 Agent。',
          ),
        ]
      }
      return [
        h(
          'p',
          { class: 'muted small' },
          '保存即受控发布：共享连接对所有 Agent 可用；专属连接只对所有者 Agent 可用。',
        ),
        scopeAgentsError.value === '' ? null : h(Notice, { tone: 'warn', text: scopeAgentsError.value }),
        h(ScopePicker, {
          id: 'resource-scope-create',
          scope: createScope.value,
          ownerAgent: createOwner.value,
          agents: scopeAgents.value,
          disabled: busy.value,
          'onUpdate:scope': (next: ResourceScope) => {
            createScope.value = next
            if (next === 'exclusive') {
              void ensureScopeAgents()
            }
          },
          'onUpdate:ownerAgent': (next: string) => {
            createOwner.value = next
          },
        }),
      ]
    }

    function renderModelConnectionFields(): VNodeChild[] {
      return [
        textField('resource-name', '名称', () => name.value, (next) => (name.value = next), {
          placeholder: '例如 main',
          disabled: editing.value,
        }),
        textField(
          'resource-base-url',
          'Base URL',
          () => baseUrl.value,
          (next) => (baseUrl.value = next),
          { placeholder: 'https://api.example.com/v1' },
        ),
        textField(
          'resource-default-model',
          '默认模型',
          () => defaultModel.value,
          (next) => (defaultModel.value = next),
          { placeholder: '例如 gpt-4o-mini' },
        ),
        renderInstanceSecretsStatus(),
        modelCredentialField(),
        ...createScopePicker(),
      ]
    }

    function renderMcpFields(): VNodeChild[] {
      const fields: VNodeChild[] = [
        textField(
          'resource-name',
          '名称',
          () => name.value,
          (next) => (name.value = next),
          { placeholder: '例如 filesystem', disabled: editing.value },
        ),
        h('label', { class: 'field', for: 'resource-transport' }, [
          h('span', '传输'),
          h(
            'select',
            {
              id: 'resource-transport',
              value: transport.value,
              disabled: editing.value,
              onChange: (event: Event) => {
                transport.value = (event.target as HTMLSelectElement).value === 'http' ? 'http' : 'stdio'
              },
            },
            [
              h('option', { value: 'stdio' }, 'stdio（本地进程）'),
              h('option', { value: 'http' }, 'http（远程服务）'),
            ],
          ),
        ]),
      ]
      if (transport.value === 'stdio') {
        fields.push(
          textField(
            'resource-command',
            '可执行名',
            () => command.value,
            (next) => (command.value = next),
            { placeholder: '例如 npx；参数写到下一栏' },
          ),
          h('label', { class: 'field', for: 'resource-args' }, [
            h('span', '参数（每行一个）'),
            h('textarea', {
              id: 'resource-args',
              class: 'editor',
              rows: 4,
              spellcheck: false,
              value: argsText.value,
              placeholder: '-y\n@modelcontextprotocol/server-filesystem\n/data',
              onInput: (event: Event) => {
                argsText.value = (event.target as HTMLTextAreaElement).value
              },
            }),
          ]),
        )
      } else {
        fields.push(
          textField('resource-url', 'URL', () => url.value, (next) => (url.value = next), {
            placeholder: 'https://mcp.example.com/mcp',
          }),
        )
      }
      fields.push(
        h('label', { class: 'field', for: 'resource-env' }, [
          h('span', '环境变量（每行 KEY=${VAR}）'),
          h('textarea', {
            id: 'resource-env',
            class: 'editor',
            rows: 4,
            spellcheck: false,
            disabled: clearEnv.value,
            value: envText.value,
            placeholder: 'TOKEN=${MCP_TOKEN}',
            onInput: (event: Event) => {
              envText.value = (event.target as HTMLTextAreaElement).value
            },
          }),
        ]),
      )
      if (editing.value && currentEnvKeys.value.length > 0) {
        // 详情只回键名，PUT 整条替换：不显式处理就会静默丢映射。
        // The detail exposes key names only and the PUT replaces the whole entry.
        fields.push(
          h(Notice, {
            tone: 'warn',
            text:
              `该服务已有环境变量映射（${currentEnvKeys.value.join('、')}），` +
              '但页面拿不到它们的值。保存会整条替换这个服务：请重新填写映射，或勾选下面的清空选项。',
          }),
          h('label', { class: 'check-row', for: 'resource-clear-env' }, [
            h('input', {
              id: 'resource-clear-env',
              type: 'checkbox',
              checked: clearEnv.value,
              onChange: (event: Event) => {
                const checked = (event.target as HTMLInputElement).checked
                clearEnv.value = checked
                if (checked) {
                  envText.value = ''
                }
              },
            }),
            h('span', '清空环境变量映射（本次保存后该服务不再有环境变量）'),
          ]),
        )
      }
      if (transport.value === 'http') {
        fields.push(
          h('label', { class: 'field', for: 'resource-headers' }, [
            h('span', '请求头（每行 头名=${VAR}；Authorization 装配为 Bearer）'),
            h('textarea', {
              id: 'resource-headers',
              class: 'editor',
              rows: 3,
              spellcheck: false,
              disabled: clearHeaders.value,
              value: headersText.value,
              placeholder: 'Authorization=${MCP_TOKEN}',
              onInput: (event: Event) => {
                headersText.value = (event.target as HTMLTextAreaElement).value
              },
            }),
          ]),
        )
        if (editing.value && currentHeaderKeys.value.length > 0) {
          fields.push(
            h(Notice, {
              tone: 'warn',
              text:
                `该服务已有请求头占位（${currentHeaderKeys.value.join('、')}），` +
                '页面拿不到它们的值。保存会整条替换这个服务：请重新填写头名=${VAR}，或勾选清空。',
            }),
            h('label', { class: 'check-row', for: 'resource-clear-headers' }, [
              h('input', {
                id: 'resource-clear-headers',
                type: 'checkbox',
                checked: clearHeaders.value,
                onChange: (event: Event) => {
                  const checked = (event.target as HTMLInputElement).checked
                  clearHeaders.value = checked
                  if (checked) {
                    headersText.value = ''
                  }
                },
              }),
              h('span', '清空请求头占位（本次保存后该服务不再有请求头）'),
            ]),
          )
        }
      }
      fields.push(...renderSlotSources())
      return fields
    }

    /**
     * 逐占位符来源卡片（14D）：槽位来自上面的映射文本，每张卡只选「值从哪里来」。
     * 「加密托管」在这里填值；「环境变量」不填值——定义里的 `${VAR}` 就是进程变量名。
     * One card per declared slot; the card only chooses where the value comes from.
     */
    function renderSlotSources(): VNodeChild[] {
      const slots = declaredSlots.value
      if (slots.length === 0) {
        return []
      }
      const cards: VNodeChild[] = [
        h(
          'p',
          { class: 'muted small' },
          '每个占位符都要显式选择来源：默认「加密托管」；「环境变量」不在这里填值，' +
            '定义里的 ${VAR} 就是进程环境变量名。保存时会按选择逐个核对——托管没有值、' +
            '环境变量未设置都会 fail-closed 拒绝写入。',
        ),
      ]
      for (const key of slots) {
        const draft = slotDraft(key)
        const existing = currentSlot(key)
        const reference = slotReference(key)
        cards.push(
          h(CredentialField, {
            id: `mcp-slot-${key}`,
            label: `占位符 ${key}`,
            mode: draft.mode,
            state: slotCredentialState(key),
            last4: existing?.last4 ?? '',
            reference: draft.mode === 'env' ? reference : '',
            value: draft.value,
            editableMode: true,
            valueVisible: draft.mode === 'managed',
            disabled: busy.value,
            noun: '凭据',
            placeholder: '粘贴完整的值（保存后不回显原值）',
            hint:
              draft.mode === 'managed'
                ? '值由服务端主密钥加密保存；留空 = 沿用已有托管值（没有旧值时必须填写）。'
                : `这里不填值：定义里的 ${reference === '' ? '${VAR}' : reference} 就是进程环境变量名，` +
                  '保存时若该变量缺失会被拒绝（503）。',
            'onUpdate:mode': (next: CredentialMode) => {
              setSlotDraft(key, { mode: next })
              error.value = ''
              problems.value = []
            },
            'onUpdate:value': (next: string) => {
              setSlotDraft(key, { value: next })
            },
          }),
        )
      }
      return [sectionLike('占位符来源', cards)]
    }

    /** 表单内部的字段分组：与页面 section 同一视觉，但不带页面级标题层级。 */
    function sectionLike(title: string, children: VNodeChild[]): VNode {
      return h('div', { class: 'form-section slot-sources' }, [
        h('h3', title),
        ...children,
      ])
    }

    function modeButton(mode: 'create' | 'import', label: string): VNode {
      return h(
        'button',
        {
          type: 'button',
          class: skillMode.value === mode ? 'primary' : '',
          'aria-pressed': skillMode.value === mode ? 'true' : 'false',
          disabled: busy.value,
          onClick: () => {
            skillMode.value = mode
            error.value = ''
            problems.value = []
          },
        },
        label,
      )
    }

    /** 编辑已有的单个文件：路径与正文都可改，整份清单提交为新版本。 */
    // Edit one file of the list; the whole list becomes a new immutable version.
    function updateSkillFile(index: number, patch: Partial<FileDraft>): void {
      skillFiles.value = skillFiles.value.map((file, i) =>
        i === index ? { ...file, ...patch } : file,
      )
    }

    function removeSkillFile(index: number): void {
      skillFiles.value = skillFiles.value.filter((_, i) => i !== index)
    }

    function addSkillFile(): void {
      skillFiles.value = [...skillFiles.value, { path: '', content: '' }]
    }

    function renderSkillEditor(): VNodeChild[] {
      const children: VNodeChild[] = [
        h(Notice, {
          tone: 'info',
          text: '保存会用下面的完整文件清单创建一个新版本；上一版本保持不变，仍可在版本列表查看。',
        }),
      ]
      if (skillFiles.value.length === 0) {
        children.push(h('p', { class: 'muted' }, '该技能没有任何文件；至少要保留 SKILL.md'))
      } else {
        children.push(
          h(
            'ul',
            { class: 'file-list skill-file-list' },
            skillFiles.value.map((file, index) =>
              h('li', { key: String(index), class: 'skill-file-editor' }, [
                h('div', { class: 'skill-file-head' }, [
                  h('input', {
                    type: 'text',
                    value: file.path,
                    placeholder: 'SKILL.md',
                    autocomplete: 'off',
                    spellcheck: false,
                    'aria-label': `文件路径 ${index + 1}`,
                    onInput: (event: Event) => {
                      updateSkillFile(index, { path: (event.target as HTMLInputElement).value })
                    },
                  }),
                  h(
                    'button',
                    {
                      type: 'button',
                      disabled: busy.value,
                      onClick: () => removeSkillFile(index),
                    },
                    '移除',
                  ),
                ]),
                h('textarea', {
                  class: 'editor',
                  rows: 8,
                  spellcheck: false,
                  value: file.content,
                  'aria-label': `文件正文 ${file.path === '' ? index + 1 : file.path}`,
                  onInput: (event: Event) => {
                    updateSkillFile(index, {
                      content: (event.target as HTMLTextAreaElement).value,
                    })
                  },
                }),
              ]),
            ),
          ),
        )
      }
      children.push(
        h('div', { class: 'form-actions' }, [
          h('button', { type: 'button', disabled: busy.value, onClick: addSkillFile }, '添加文件'),
        ]),
      )
      return children
    }

    function renderSkillFields(): VNodeChild[] {
      const fields: VNodeChild[] = []
      if (!editing.value) {
        fields.push(
          h('div', { class: 'form-actions' }, [
            modeButton('create', '新建技能'),
            modeButton('import', '从目录导入'),
          ]),
        )
      }
      fields.push(
        textField('resource-name', '名称', () => name.value, (next) => (name.value = next), {
          placeholder: '目录导入时默认取目录名',
          disabled: editing.value,
        }),
      )
      if (editing.value) {
        fields.push(...renderSkillEditor())
        return fields
      }
      if (skillMode.value === 'create') {
        fields.push(
          h('label', { class: 'field', for: 'resource-description' }, [
            h('span', '描述'),
            h('textarea', {
              id: 'resource-description',
              class: 'editor',
              rows: 3,
              spellcheck: false,
              value: description.value,
              placeholder: '一句话说明这个技能解决什么问题',
              onInput: (event: Event) => {
                description.value = (event.target as HTMLTextAreaElement).value
              },
            }),
          ]),
        )
        return fields
      }
      fields.push(
        h('label', { class: 'field', for: 'resource-directory' }, [
          h('span', '技能目录（只读取文本文件，不上传二进制）'),
          h('input', {
            id: 'resource-directory',
            type: 'file',
            multiple: true,
            // webkitdirectory 是浏览器目录选择的非标准属性；Vue 的 h() 会原样落到 DOM。
            webkitdirectory: '',
            onChange: (event: Event) => {
              void pickDirectory(event)
            },
          }),
        ]),
      )
      if (importFiles.value.length > 0) {
        fields.push(
          h(
            'ul',
            { class: 'file-list' },
            importFiles.value.map((file) =>
              h('li', { key: file.path }, [h('code', file.path)]),
            ),
          ),
        )
      }
      return fields
    }

    /** 提交按钮文案：忙碌时统一显示进行中文案。 */
    function submitLabel(): string {
      if (busy.value) {
        return '提交中…'
      }
      if (props.kind === 'skills') {
        if (editing.value) {
          return '保存新版本'
        }
        return skillMode.value === 'import' ? '导入技能' : '创建技能'
      }
      return editing.value ? '保存修改' : '创建并发布'
    }

    return (): VNode => {
      const fields: VNodeChild[] =
        props.kind === 'skills'
          ? renderSkillFields()
          : props.kind === 'mcp-servers'
            ? renderMcpFields()
            : renderModelConnectionFields()

      const children: VNodeChild[] = []
      if (editing.value && etag.value !== '') {
        children.push(
          h('p', { class: 'muted small' }, [
            '当前 ETag：',
            h('code', { class: 'resource-etag' }, etag.value),
          ]),
        )
      }
      if (loading.value) {
        children.push(h('p', { class: 'muted' }, '读取中…'))
      }
      children.push(...fields)
      if (props.kind === 'model-connections') {
        children.push(renderSecretsDialog())
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
      if (notice.value !== '') {
        children.push(h(Notice, { tone: 'info', text: notice.value }))
      }
      const buttons: VNodeChild[] = [
        h(
          'button',
          {
            type: 'button',
            class: 'primary',
            disabled: busy.value || loading.value,
            onClick: () => {
              void submit()
            },
          },
          submitLabel(),
        ),
      ]
      // MCP 的第二步动作：保存成功后立刻应用一次（仍走 If-Match 条件写）。
      if (props.kind === 'mcp-servers') {
        buttons.push(
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value || loading.value,
              onClick: () => {
                void submit(true)
              },
            },
            busy.value ? '提交中…' : editing.value ? '保存并应用' : '创建并应用',
          ),
        )
      }
      buttons.push(
        h(
          'button',
          {
            type: 'button',
            disabled: busy.value,
            onClick: () => emit('cancel'),
          },
          '取消',
        ),
      )
      children.push(h('div', { class: 'form-actions' }, buttons))
      if (props.kind !== 'skills') {
        children.push(
          h('p', { class: 'muted small' }, '保存即发布：定义文件与发布指针在同一提交边界内更新。'),
        )
      }
      return h('section', { class: ['card', 'resource-form'] }, [
        h('h2', heading.value),
        ...children,
      ])
    }
  },
})
