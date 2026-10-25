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
  McpServerView,
  ModelConnectionView,
  ResourceScope,
  SkillView,
} from '../api'
import { adminApi, problemsOf } from '../api-admin'
import type {
  FileDraft,
  McpServerDetail,
  McpServerUpsert,
  McpServerWrite,
  ModelConnectionUpsert,
  SkillDetail,
} from '../api-admin'
import type { ResourceKind } from '../router'
import { CredentialField } from './credential-field'
import { ScopePicker } from './scope-picker'
import { Notice } from './ui'

const KIND_NOUNS: Record<ResourceKind, string> = {
  skills: '技能',
  'mcp-servers': 'MCP 服务',
  'model-connections': '模型连接',
}

const ENV_PLACEHOLDER = /^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/
const ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]*$/

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
    created: (_view: unknown) => true,
    saved: (_view: unknown) => true,
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
    // 详情只回键名：编辑已有映射时要求重输，或显式勾选清空。
    const currentEnvKeys = ref<string[]>([])
    const clearEnv = ref(false)

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
      currentEnvKeys.value = []
      clearEnv.value = false
    }

    /** 修改入口依赖详情 ETag；三类资源都先 GET 详情再填表。 */
    // Edits always start from the detail GET, which is also the ETag source.
    onMounted(() => {
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

    /** 用 MCP 详情填表；args 每行一个，env 只回键名、留空等待重输。 */
    // MCP detail: args one per line; env values are never echoed back.
    function applyMcpServer(view: McpServerDetail, etagValue: string): void {
      name.value = view.name
      transport.value = view.transport
      command.value = view.command
      argsText.value = view.args.join('\n')
      url.value = view.url
      envText.value = ''
      currentEnvKeys.value = [...view.env_keys]
      clearEnv.value = false
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

    async function submit(): Promise<void> {
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
      await submitMcpServer()
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
          emit('created', view)
          return
        }
        await client.updateModelConnection(current, body, etag.value)
        const view = await rereadModelConnection(current)
        notice.value = '已保存并发布；上面是重新读取的服务端真值。'
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

    /** 逐行解析 `KEY=${VAR}`；不合法就在本地拒绝，不发出请求。 */
    function collectEnv(): Record<string, string> | null {
      const env: Record<string, string> = {}
      for (const line of lines(envText.value)) {
        const separator = line.indexOf('=')
        const key = separator === -1 ? '' : line.slice(0, separator).trim()
        const value = separator === -1 ? '' : line.slice(separator + 1).trim()
        if (!ENV_KEY.test(key) || !ENV_PLACEHOLDER.test(value)) {
          error.value = `环境变量必须是 KEY=\${VAR} 的占位，例如 TOKEN=\${MCP_TOKEN}：${line}`
          return null
        }
        env[key] = value
      }
      return env
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
      if (typedCount > 0) {
        body.env = typedEnv
      }
      return body
    }

    async function submitMcpServer(): Promise<void> {
      const body = mcpServerBody()
      if (body === null) {
        return
      }
      busy.value = true
      try {
        const current = props.edit
        if (current === null) {
          const view: McpServerView = await client.createMcpServer(body)
          notice.value = '已保存并发布；MCP 连接在重启后生效（列表会标“需重启”）。'
          emit('created', view)
          return
        }
        await client.updateMcpServer(current, body, etag.value)
        const view = await rereadMcpServer(current)
        notice.value = '已保存并发布；MCP 连接在重启后生效（详情会标“需重启”）。'
        emit('saved', view)
      } catch (caught) {
        const current = props.edit
        await handleFailure(caught, current === null ? null : () => refreshEtag(current))
      } finally {
        busy.value = false
      }
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

    function modelCredentialField(): VNode {
      return h(CredentialField, {
        id: 'resource-api-key',
        label: editing.value ? 'API 密钥（留空 = 保留当前凭据）' : 'API 密钥',
        mode: credentialMode.value,
        state: editing.value ? currentCredential.value.state : '',
        version: editing.value ? currentCredential.value.version : 0,
        last4: editing.value ? currentCredential.value.last4 : '',
        reference: editing.value ? currentCredential.value.envRef : '',
        value: credentialValue.value,
        editableMode: true,
        disabled: busy.value,
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
      return fields
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
      children.push(
        h('div', { class: 'form-actions' }, [
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
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value,
              onClick: () => emit('cancel'),
            },
            '取消',
          ),
        ]),
      )
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
