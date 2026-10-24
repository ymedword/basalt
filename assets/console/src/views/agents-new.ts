// 新建 Agent 视图（/console/agents/new）：一句描述 → 作者模型起草 → 页面内编辑 →
// 创建（默认停用）→ 提交多文件 → 启用（图 H）。
// 所有展示都以服务端重新读取为准：草稿是唯一例外（未落盘），提交成功后不再回显草稿，
// 而是展示写后重读的受管文件集合；412 刷新集合 ETag 并保留草稿输入。
// Agents-new: description → author draft → create (disabled) → submit files → enable.
// Drafts live in memory only; every persisted view comes from a server re-read.
import { computed, defineComponent, h, onMounted, onUnmounted, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type { AgentContentChanges, AgentFilesView, AgentLifecycleView, ModelConnectionView } from '../api'
import { adminApi, problemsOf } from '../api-admin'
import type { AgentCreateRequest, AgentDraftAnswer, AgentDraftQuestion, FileDraft } from '../api-admin'
import { splitAgentBody } from '../agent-body'
import { Notice } from '../components/ui'
import { agentHref } from '../router'
import {
  AGENT_STATE_LABELS,
  badge,
  definitions,
  lifecycleTone,
  pageHeading,
  section,
  viewProps,
} from './shared'

/** 草稿文件：只存在于页面内存，未落盘、不注册（契约 AgentDraftView）。 */
interface DraftFile {
  path: string
  content: string
}

export const AgentsNewView = defineComponent({
  name: 'AgentsNewView',
  props: viewProps,
  setup(props) {
    const client = adminApi(props.ctx.api)
    const router = props.ctx.router

    const name = ref('')
    const description = ref('')
    const model = ref('')

    // --- 模型连接 ---------------------------------------------------------
    // 新 Agent 还不存在，只有已发布的共享连接对它「有资格」；专属连接不可能先于
    // Agent 存在，因此不出现在可选列表，但要可见并说明原因（不得伪造候选）。
    // A brand-new Agent has no UID yet, so only published shared connections are
    // eligible; exclusive ones are shown as unavailable with the reason.
    const connections = ref<ModelConnectionView[]>([])
    const connectionsLoading = ref(false)
    const connectionsError = ref('')
    /** 当前选择的连接 UID（名称只是标签；提交时以 UID 写入绑定）。 */
    const connectionUID = ref('')
    /** 用户是否手动选过连接：选过之后重新起草不再覆盖选择。 */
    const connectionTouched = ref(false)

    // --- 作者草稿 ---------------------------------------------------------
    const draft = ref<DraftFile[]>([])
    const draftLoading = ref(false)
    const draftError = ref('')
    const draftProblems = ref<string[]>([])
    const authorHint = ref('')

    // --- 问答补全（R6.3）----------------------------------------------------
    // 信息足够时作者模型直接出文件；只有信息不足才追问（最多两轮答案）。
    // 答案只存在于内存，随本次请求进入提示词，不落盘、不写日志，离开页面即丢弃。
    // The author only asks when the description is insufficient (two answer rounds
    // at most); answers live in memory and are discarded on navigation.
    const MAX_ANSWER_ROUNDS = 2
    const pendingQuestions = ref<AgentDraftQuestion[]>([])
    /** 已经发出的提问轮数（0 表示还没追问过）。 */
    const answerRound = ref(0)
    const answerDrafts = ref<Record<string, string>>({})
    const answerError = ref('')
    /** 累积的答案：后续每次继续生成都带上全部已答内容。 */
    let answersHistory: AgentDraftAnswer[] = []

    // --- 创建 -------------------------------------------------------------
    const creating = ref(false)
    const createError = ref('')
    const created = ref<AgentFilesView | null>(null)

    // --- 提交 -------------------------------------------------------------
    const saving = ref(false)
    const saveError = ref('')
    const saved = ref<AgentFilesView | null>(null)

    // --- 生命周期与启用 ---------------------------------------------------
    const lifecycle = ref<AgentLifecycleView | null>(null)
    const lifecycleError = ref('')
    const lifecyclePending = ref(false)
    let lifecycleRetryCancelled = false
    onUnmounted(() => {
      lifecycleRetryCancelled = true
    })
    const enabling = ref(false)
    const enableError = ref('')

    /** 可当新 Agent 模型连接的候选：已发布 + 共享（按服务端返回顺序，不排序）。 */
    function sharedPool(): readonly ModelConnectionView[] {
      return connections.value.filter(
        (connection) => connection.published_version_id > 0 && connection.scope === 'shared',
      )
    }
    /** 有资源身份但当前不能选择的连接：显示出来并解释，而不是当作不存在。 */
    function unavailablePool(): readonly ModelConnectionView[] {
      return connections.value.filter(
        (connection) => connection.published_version_id <= 0 || connection.scope !== 'shared',
      )
    }
    function selectedConnection(): ModelConnectionView | null {
      return sharedPool().find((connection) => connection.resource_uid === connectionUID.value) ?? null
    }
    /** 选中连接时把模型预填为连接默认模型；用户仍可改成该连接支持的其他模型。 */
    function chooseConnection(uid: string): void {
      connectionUID.value = uid
      const connection = selectedConnection()
      if (connection !== null) {
        model.value = connection.default_model
      }
    }

    const canCreate = computed(
      () => name.value.trim() !== '' && !creating.value && created.value === null,
    )
    const canGenerate = computed(
      () => name.value.trim() !== '' && description.value.trim() !== '' && !draftLoading.value,
    )
    /** 逐项必填：任何一题为空都不发请求（答案留空帮不了作者模型确定草稿）。 */
    const canAnswer = computed(
      () =>
        pendingQuestions.value.length > 0 &&
        !draftLoading.value &&
        pendingQuestions.value.every((question) => (answerDrafts.value[question.id] ?? '').trim() !== ''),
    )
    // 提交是把草稿正文 + 模型连接绑定合并进同一次内容写；没有连接就没有可写的绑定，
    // 服务端会以 AGENT_RESOURCE_BINDING_MISSING 拒绝，因此这里先拦住。
    const canSubmit = computed(
      () =>
        created.value !== null &&
        draft.value.length > 0 &&
        selectedConnection() !== null &&
        !saving.value &&
        saved.value === null,
    )
    const canEnable = computed(
      () =>
        lifecycle.value !== null &&
        lifecycle.value.valid &&
        lifecycle.value.state === 'disabled' &&
        !enabling.value,
    )

    /**
     * 名字建议是确定性哈希（FNV-1a 32 位），同样的描述总得到同一个名字：不调用
     * 任何服务、不读随机数，用户也可以直接改成自己喜欢的名字。
     * The suggested name is a deterministic hash so the same description always yields
     * the same name; the user is free to overwrite it.
     */
    function suggestAgentName(text: string): string {
      let hash = 0x811c9dc5
      for (const byte of new TextEncoder().encode(text)) {
        hash ^= byte
        hash = Math.imul(hash, 0x01000193) >>> 0
      }
      return `agent-${hash.toString(36)}`
    }

    // 列表页「一句话创建」把描述带到这里：预填并自动起草一次。自动起草只发生一次，
    // 之后仍由用户决定是否重新生成；创建与否始终要用户再点一次。
    // The list page hands the description over: prefill and author exactly one draft.
    onMounted(() => {
      void loadConnections()
      const seeded = props.query.get('description')?.trim() ?? ''
      if (seeded !== '') {
        description.value = seeded
        if (name.value.trim() === '') {
          name.value = suggestAgentName(seeded)
        }
      }
      if (props.query.get('autodraft') === '1' && description.value.trim() !== '') {
        void generateDraft()
      }
    })

    /** 读取已发布连接；失败只提示，不伪造候选列表。 */
    async function loadConnections(): Promise<void> {
      connectionsLoading.value = true
      connectionsError.value = ''
      try {
        connections.value = await props.ctx.api.listModelConnections()
        if (connectionUID.value === '') {
          applyDefaultConnection()
        }
      } catch (caught) {
        connectionsError.value = errorMessage(caught)
      } finally {
        connectionsLoading.value = false
      }
    }

    /** 默认连接：优先草稿 frontmatter 写的 provider 名，否则按列表顺序第一项。 */
    function applyDefaultConnection(): void {
      if (connectionTouched.value) {
        return
      }
      const pool = sharedPool()
      if (pool.length === 0) {
        connectionUID.value = ''
        return
      }
      const draftProvider = draftProviderName()
      const match = pool.find((connection) => connection.name === draftProvider)
      chooseConnection((match ?? pool[0]!).resource_uid)
    }

    /** 从草稿 AGENT.md 的 frontmatter 里取 provider.name；仅用于默认选中，不作写依据。 */
    function draftProviderName(): string {
      const agentFile = draft.value.find((file) => file.path === 'AGENT.md')
      if (agentFile === undefined) {
        return ''
      }
      const match = /^provider:\s*\r?\n\s+name:\s*(.+)$/m.exec(agentFile.content)
      if (match === null) {
        return ''
      }
      return match[1]!.trim().replace(/^["']|["']$/g, '')
    }

    /**
     * 生命周期读取走 /profiles：AgentFilesView 的 lifecycle 只保证 state / valid /
     * error 三项，generation 在写面响应里是零值，不能拿去拼启停迁移。
     * 注册表由文件监听异步刷新，写响应可能先于注册完成；写后读取在约 5 秒内
     * 有界重试，等待期间显示“正在等待”，不把刚创建的 Agent 误报成不存在。
     * The registry updates asynchronously after a write, so post-write reads retry
     * briefly and never report a just-created agent as missing while waiting.
     */
    async function refreshLifecycle(agentName: string, waitForRegistration = false): Promise<void> {
      lifecycleRetryCancelled = false
      lifecyclePending.value = waitForRegistration
      lifecycleError.value = ''
      try {
        for (let attempt = 0; ; attempt += 1) {
          if (lifecycleRetryCancelled) {
            return
          }
          try {
            const profiles = await props.ctx.api.listProfiles()
            const profile = profiles.find((item) => item.name === agentName)
            lifecycle.value = profile?.lifecycle ?? null
            if (profile !== undefined) {
              lifecycleError.value = ''
              return
            }
            if (!waitForRegistration || attempt >= 20) {
              lifecycleError.value = waitForRegistration
                ? '已创建，但注册表在等待时间内仍未出现该 Agent：请检查 AGENT.md 是否有效，或稍后刷新。'
                : '已创建的 Agent 尚未出现在注册表中：请检查文件是否有效，或稍后刷新。'
              return
            }
          } catch (caught) {
            lifecycleError.value = errorMessage(caught)
            return
          }
          await new Promise((resolve) => window.setTimeout(resolve, 250))
        }
      } finally {
        lifecyclePending.value = false
      }
    }

    /**
     * 一次作者模型调用：返回文件则替换草稿；返回问题则进入问答（不覆盖已有草稿）。
     * 超过两轮仍在追问时停止并给出可执行的下一步，而不是无限循环。
     * One author call: files replace the draft, questions enter the Q&A card, and a
     * third question round is refused with actionable guidance.
     */
    async function callAuthor(): Promise<void> {
      draftLoading.value = true
      draftError.value = ''
      draftProblems.value = []
      authorHint.value = ''
      answerError.value = ''
      try {
        const view = await client.generateAgentFiles(
          name.value.trim(),
          description.value.trim(),
          answersHistory,
        )
        const questions = view.questions ?? []
        if (questions.length > 0) {
          if (answerRound.value >= MAX_ANSWER_ROUNDS) {
            pendingQuestions.value = []
            answerDrafts.value = {}
            draftError.value = `作者模型连续 ${MAX_ANSWER_ROUNDS} 轮仍在追问：请把描述写得更具体（做什么、多久一次、输出到哪里）后重新生成。`
            return
          }
          answerRound.value += 1
          pendingQuestions.value = [...questions]
          answerDrafts.value = {}
          return
        }
        pendingQuestions.value = []
        answerDrafts.value = {}
        draft.value = view.files.map((file) => ({ path: file.path, content: file.content }))
        // 草稿的 provider 名如果对应一个可用连接，默认选中它（用户手选过就不覆盖）。
        applyDefaultConnection()
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 503) {
          authorHint.value = errorMessage(caught)
          return
        }
        draftProblems.value = problemsOf(caught)
        draftError.value = errorMessage(caught)
      } finally {
        draftLoading.value = false
      }
    }

    /** 从头起草：清空问答历史与轮数，丢弃上一轮答案。 */
    async function generateDraft(): Promise<void> {
      if (!canGenerate.value) {
        draftError.value = '请先填写 Agent 名字与一句描述'
        return
      }
      answersHistory = []
      answerRound.value = 0
      pendingQuestions.value = []
      answerDrafts.value = {}
      await callAuthor()
    }

    /** 逐项必填后继续生成：本次答案累积进问答历史，下一次请求带全部答案。 */
    async function answerQuestions(): Promise<void> {
      if (pendingQuestions.value.length === 0 || draftLoading.value) {
        return
      }
      const missing = pendingQuestions.value.some(
        (question) => (answerDrafts.value[question.id] ?? '').trim() === '',
      )
      if (missing) {
        answerError.value = '请逐项回答后再继续生成（留空的问题无法帮助作者模型确定草稿）。'
        return
      }
      answerError.value = ''
      const answered = pendingQuestions.value.map((question) => ({
        id: question.id,
        question: question.question,
        answer: (answerDrafts.value[question.id] ?? '').trim(),
      }))
      answersHistory = [...answersHistory, ...answered]
      pendingQuestions.value = []
      await callAuthor()
    }

    /** 放弃本轮追问并重新起草；答案只在内存里，丢弃没有残留。 */
    function discardQuestions(): void {
      if (draftLoading.value) {
        return
      }
      pendingQuestions.value = []
      answerDrafts.value = {}
      answerError.value = ''
      answersHistory = []
      answerRound.value = 0
    }

    async function createAgent(): Promise<void> {
      if (!canCreate.value) {
        return
      }
      creating.value = true
      createError.value = ''
      const request: AgentCreateRequest = { name: name.value.trim() }
      if (description.value.trim() !== '') {
        request.description = description.value.trim()
      }
      if (model.value.trim() !== '') {
        request.model = model.value.trim()
      }
      try {
        created.value = await client.createAgent(request)
        saved.value = null
        // 不阻塞“已创建”区块渲染；注册表在后台有界重试确认。
        void refreshLifecycle(name.value.trim(), true)
      } catch (caught) {
        createError.value = errorMessage(caught)
      } finally {
        creating.value = false
      }
    }

    /**
     * 提交草稿：AGENT.md 走结构化补丁（正文 + provider + 模型连接绑定），其余文件
     * 走多文件提交。绑定只能由模板创建、表单显式选择或原文显式写出产生——这里属于
     * 「表单显式选择」，因此必须先把所选连接的 UID 合并进同一次内容写。
     * 提交后展示服务端重读的文件集合；412 刷新基准并保留草稿。
     */
    async function submitDraft(): Promise<void> {
      const current = created.value
      const connection = selectedConnection()
      if (current === null || saving.value) {
        return
      }
      if (connection === null) {
        saveError.value = '请先选择一个可用的模型连接：资源关联要写入它当前指针的 resource_uid'
        return
      }
      const agentFile = draft.value.find((file) => file.path === 'AGENT.md')
      if (agentFile === undefined) {
        saveError.value = '草稿里没有 AGENT.md：无法写入正文与资源关联'
        return
      }
      const split = splitAgentBody(agentFile.content)
      if (!split.ok) {
        saveError.value =
          '草稿的 AGENT.md 缺少 frontmatter 围栏，无法按结构化补丁提交：请修正草稿后重试'
        return
      }
      const modelName = model.value.trim() === '' ? connection.default_model : model.value.trim()
      if (modelName === '') {
        saveError.value = '请填写模型名：连接的默认模型为空时不能提交'
        return
      }
      saving.value = true
      saveError.value = ''
      try {
        // 结构化补丁以内容 ETag 为条件写基准（与配置页共用同一份 content_etag）。
        const content = await props.ctx.api.getAgentContent(current.agent)
        const changes: AgentContentChanges = {
          body: split.body,
          provider: { name: connection.name, model: modelName },
          bindings: [
            {
              resource_kind: 'model_connection',
              ref_name: connection.name,
              resource_uid: connection.resource_uid,
            },
          ],
        }
        let updated = await props.ctx.api.updateAgentContent(
          current.agent,
          { changes },
          content.etag,
        )
        // AGENT.md 以外的草稿文件用集合 ETag 单独提交；未列出的文件由服务端保留。
        const extras: FileDraft[] = draft.value
          .filter((file) => file.path !== 'AGENT.md')
          .map((file) => ({ path: file.path, content: file.content }))
        if (extras.length > 0) {
          updated = await client.submitAgentFiles(current.agent, extras, updated.etag)
        }
        saved.value = updated
        void refreshLifecycle(current.agent, true)
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 412) {
          saveError.value =
            'AGENT.md 或文件集合已被他人修改，本次提交没有写入磁盘；已保留草稿，请确认内容后再提交'
          try {
            created.value = await client.getAgentFiles(current.agent)
          } catch (refreshError) {
            saveError.value = `${saveError.value}（刷新 ETag 失败：${errorMessage(refreshError)}）`
          }
        } else {
          saveError.value = errorMessage(caught)
        }
      } finally {
        saving.value = false
      }
    }

    async function enableAgent(): Promise<void> {
      const current = lifecycle.value
      if (!canEnable.value || current === null) {
        return
      }
      enabling.value = true
      enableError.value = ''
      try {
        await props.ctx.api.enableAgent(current.agent, {
          expected_state: 'disabled',
          expected_generation: current.generation,
        })
        router.navigate(agentHref(current.agent))
      } catch (caught) {
        enableError.value = errorMessage(caught)
        await refreshLifecycle(current.agent)
      } finally {
        enabling.value = false
      }
    }

    function renderFileList(view: AgentFilesView): VNode {
      return h(
        'ul',
        { class: 'file-list' },
        view.files.map((file) =>
          h('li', { key: file.path }, [
            h('code', file.path),
            h('span', { class: 'muted small' }, ` ${file.size} 字节`),
          ]),
        ),
      )
    }

    function renderBasicFields(): VNode[] {
      const locked = created.value !== null
      return [
        h('label', { class: 'field', for: 'agent-name' }, [
          h('span', '名字'),
          h('input', {
            id: 'agent-name',
            type: 'text',
            value: name.value,
            placeholder: '例如 ops-assistant',
            autocomplete: 'off',
            spellcheck: false,
            disabled: locked || creating.value,
            onInput: (event: Event) => {
              name.value = (event.target as HTMLInputElement).value
            },
          }),
        ]),
        h('label', { class: 'field', for: 'agent-description' }, [
          h('span', '一句描述'),
          h('textarea', {
            id: 'agent-description',
            class: 'editor',
            rows: 3,
            spellcheck: false,
            value: description.value,
            placeholder: '说明这个 Agent 要解决什么问题；作者模型会据此起草文件',
            disabled: locked || draftLoading.value,
            onInput: (event: Event) => {
              description.value = (event.target as HTMLTextAreaElement).value
            },
          }),
        ]),
        renderConnectionField(locked),
        h('label', { class: 'field', for: 'agent-model' }, [
          h('span', '模型'),
          h('input', {
            id: 'agent-model',
            type: 'text',
            value: model.value,
            placeholder:
              selectedConnection() === null
                ? '选择连接后自动填入默认模型'
                : `默认 ${selectedConnection()!.default_model}；可改成该连接支持的模型`,
            autocomplete: 'off',
            spellcheck: false,
            disabled: locked || selectedConnection() === null,
            onInput: (event: Event) => {
              model.value = (event.target as HTMLInputElement).value
            },
          }),
        ]),
        h(
          'p',
          { class: 'muted small' },
          '提交时把所选连接写入 Agent 配置的资源关联（按资源 UID 精确关联，名称只是标签）；模型调用走服务端保存的连接，页面不接收 API 密钥。',
        ),
      ]
    }

    /** 模型连接选择器：只列已发布且有资格的（共享）连接，其余可见但说明不可选原因。 */
    function renderConnectionField(locked: boolean): VNode {
      const pool = sharedPool()
      const unavailable = unavailablePool()
      const children: VNodeChild[] = [
        h('span', '模型连接'),
        h(
          'select',
          {
            id: 'agent-model-connection',
            disabled: locked || connectionsLoading.value || pool.length === 0,
            onChange: (event: Event) => {
              connectionTouched.value = true
              chooseConnection((event.target as HTMLSelectElement).value)
            },
          },
          [
            h(
              'option',
              { value: '', selected: selectedConnection() === null },
              connectionsLoading.value ? '读取中…' : '（请选择模型连接）',
            ),
            ...pool.map((connection) =>
              h(
                'option',
                {
                  key: connection.resource_uid,
                  value: connection.resource_uid,
                  selected: connection.resource_uid === connectionUID.value,
                },
                `${connection.name}（默认模型 ${connection.default_model}）`,
              ),
            ),
          ],
        ),
      ]
      if (connectionsError.value !== '') {
        children.push(
          h(Notice, {
            tone: 'error',
            text: `读取模型连接失败：${connectionsError.value}；没有连接就不能提交草稿。`,
          }),
        )
      } else if (!connectionsLoading.value && pool.length === 0) {
        children.push(
          h(Notice, {
            tone: 'warn',
            text: '没有可用于新 Agent 的模型连接：先到「模型与连接」发布一个共享连接，再回到这里提交草稿。',
          }),
          h('p', {}, [h('a', { href: '/console/resources/model-connections' }, '前往「模型与连接」→')]),
        )
      }
      if (unavailable.length > 0) {
        children.push(
          h(
            'p',
            { class: 'muted small' },
            `不可选：${unavailable
              .map((connection) =>
                connection.published_version_id <= 0
                  ? `${connection.name}（未发布）`
                  : `${connection.name}（专属 · ${connection.owner_agent || '其他 Agent'}）`,
              )
              .join('、')}。专属连接只能由它所属的 Agent 使用；新 Agent 还不能拥有专属连接。`,
          ),
        )
      }
      return h('label', { class: 'field', for: 'agent-model-connection' }, children)
    }

    function byteLength(text: string): number {
      return new TextEncoder().encode(text).length
    }

    /**
     * 结构化预览：把「将要创建的名字 / 描述 / 模型」和草稿文件摊平给人看，
     * 只展示页面上已有的输入，不补任何猜测值。草稿为空时整段不出现。
     * The preview shows only what the page already holds; it invents nothing.
     */
    function renderPreview(): VNode | null {
      // 创建后由「已创建」区块接管文件清单，预览不再重复。
      if (draft.value.length === 0 || created.value !== null) {
        return null
      }
      const modelName = model.value.trim()
      const connection = selectedConnection()
      const files = h(
        'ul',
        { class: 'file-list preview-files' },
        draft.value.map((file) =>
          h('li', { key: file.path }, [
            h('code', file.path),
            h('span', { class: 'muted small' }, ` ${byteLength(file.content)} 字节`),
          ]),
        ),
      )
      return section('结构化预览', [
        definitions([
          ['名字', h('code', name.value.trim())],
          [
            '一句描述',
            description.value.trim() === ''
              ? h('span', { class: 'muted' }, '未填写')
              : description.value.trim(),
          ],
          [
            '模型连接',
            connection === null
              ? h('span', { class: 'muted' }, '尚未选择：没有连接就不能提交草稿（服务端拒绝没有资源关联的引用）')
              : h('code', `${connection.name} · ${connection.default_model}`),
          ],
          [
            '模型',
            modelName === ''
              ? h('span', { class: 'muted' }, '选择连接后自动填入默认模型，可改')
              : h('code', modelName),
          ],
          ['草稿文件', files],
          ['创建后状态', '停用（generation=1、定时任务为空）；提交文件并显式启用后才可运行'],
        ]),
        h(
          'p',
          { class: 'muted small' },
          '预览来自作者模型草稿，尚未落盘、不注册；改动上面的字段后可以重新生成草稿。',
        ),
      ])
    }

    /** 问答卡：只在作者模型主动追问时出现；逐项必填，第二轮仍不足即停止。 */
    // The Q&A card exists only when the author asked; a third round is refused.
    function renderQuestionsSection(): VNode | null {
      if (pendingQuestions.value.length === 0) {
        return null
      }
      return section('作者需要补充信息', [
        h(
          'p',
          { class: 'muted' },
          `现有描述还不足以直接起草（第 ${answerRound.value} 轮，最多 ${MAX_ANSWER_ROUNDS} 轮）。逐项回答后继续生成；答案只随本次请求进入提示词，不落盘、不写日志，离开页面即丢弃。`,
        ),
        ...pendingQuestions.value.map((question) =>
          h('label', { class: 'field', key: question.id, for: `draft-answer-${question.id}` }, [
            h('span', {}, question.question),
            h('input', {
              id: `draft-answer-${question.id}`,
              type: 'text',
              autocomplete: 'off',
              value: answerDrafts.value[question.id] ?? '',
              onInput: (event: Event) => {
                answerDrafts.value = {
                  ...answerDrafts.value,
                  [question.id]: (event.target as HTMLInputElement).value,
                }
              },
            }),
          ]),
        ),
        answerError.value === '' ? null : h(Notice, { tone: 'error', text: answerError.value }),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              id: 'draft-answer-continue',
              disabled: !canAnswer.value,
              onClick: () => {
                void answerQuestions()
              },
            },
            draftLoading.value ? '生成中…' : '继续生成',
          ),
          h(
            'button',
            {
              type: 'button',
              id: 'draft-answer-discard',
              disabled: draftLoading.value,
              onClick: discardQuestions,
            },
            '放弃回答',
          ),
        ]),
      ])
    }

    function renderDraftSection(): VNode | null {
      if (draft.value.length === 0) {
        return null
      }
      const children: VNodeChild[] = draft.value.map((file) =>
        h('div', { class: 'draft-file', key: file.path }, [
          h('div', { class: 'file-path' }, h('code', file.path)),
          h('textarea', {
            class: ['editor', 'draft-content'],
            spellcheck: false,
            rows: 8,
            value: file.content,
            onInput: (event: Event) => {
              file.content = (event.target as HTMLTextAreaElement).value
            },
          }),
        ]),
      )
      children.push(
        h(
          'p',
          { class: 'muted small' },
          '草稿只在内存中：未落盘、不注册；提交后才写入磁盘。提交写回草稿正文，frontmatter 的 provider 与 resource_bindings 以所选模型连接为准（服务端合并，不按名称猜 UID）；其余 frontmatter 字段（如 tools / skills）保留模板值，创建后到「配置」页调整。',
        ),
      )
      return section('草稿文件', children)
    }

    function renderCreatedSection(): VNode | null {
      const current = created.value
      if (current === null) {
        return null
      }
      const shown = saved.value ?? current
      // 分段说真话：创建成功不等于草稿已提交，更不等于已启用。三段状态分开写，
      // 失败时用户能看出该重试哪一步、哪一步不要重复做。
      // Three separate stages: created, files submitted, enabled.
      const createdLine =
        saved.value === null
          ? '已创建（默认停用），草稿尚未提交：磁盘上现在是模板 AGENT.md。不要重复创建——提交文件或直接启用模板即可。'
          : '已创建（默认停用），草稿文件已提交。'
      const children: VNodeChild[] = [
        h('p', {}, createdLine),
        h(
          'p',
          { class: 'muted small' },
          selectedConnection() === null
            ? '还没有选择模型连接：没有连接就没有可写入的资源关联，提交会被服务端拒绝（AGENT_RESOURCE_BINDING_MISSING）。先在上面选择连接，或到「模型与连接」发布一个共享连接。'
            : `提交会把正文与「${selectedConnection()!.name}」的关联（以 resource_uid 为准）合并进同一次内容写；模型调用使用服务端保存的这份连接，页面上不出现 API 密钥。`,
        ),
        h('p', { class: 'muted small' }, ['集合 ETag：', h('code', { class: 'created-etag' }, shown.etag)]),
        renderFileList(shown),
      ]
      if (saveError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: saveError.value }))
      }
      if (saved.value !== null) {
        children.push(h(Notice, { tone: 'info', text: '草稿文件已提交；上面是服务端重读后的受管文件集合。' }))
      } else if (draft.value.length > 0) {
        children.push(
          h(
            'p',
            { class: 'muted small' },
            '提交会用草稿正文替换模板正文，并把所选模型连接写入 provider 与 resource_bindings；AGENT.md 的其余草稿文件按原样提交。文件已被他人修改时返回 412 并保留草稿。',
          ),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                class: 'primary',
                disabled: !canSubmit.value,
                onClick: () => {
                  void submitDraft()
                },
              },
              saving.value ? '提交中…' : '提交文件',
            ),
          ]),
        )
      }
      children.push(renderLifecycle(current))
      return section('已创建', children)
    }

    function renderLifecycle(current: AgentFilesView): VNodeChild {
      if (lifecycleError.value !== '') {
        return h(Notice, { tone: 'warn', text: lifecycleError.value })
      }
      if (lifecyclePending.value) {
        return h(Notice, {
          tone: 'info',
          text: '正在等待注册表刷新…（通常 1 秒内完成；写入已成功，无需重复创建）',
        })
      }
      const state = lifecycle.value
      if (state === null) {
        return h('p', { class: 'muted' }, '生命周期读取中…')
      }
      const parts: VNodeChild[] = [
        h('p', { class: 'agent-meta' }, [
          badge(
            state.state === null ? '状态无效' : AGENT_STATE_LABELS[state.state],
            lifecycleTone(state.state),
          ),
          h('span', { class: 'muted small' }, ` 版本 ${state.generation}`),
        ]),
      ]
      if (!state.valid) {
        parts.push(
          h(Notice, {
            tone: 'error',
            text:
              state.lifecycle_error === ''
                ? '生命周期状态无效：请检查定义文件后再启用'
                : state.lifecycle_error,
          }),
        )
      }
      if (enableError.value !== '') {
        parts.push(h(Notice, { tone: 'error', text: enableError.value }))
      }
      if (state.state === 'disabled') {
        parts.push(
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                class: 'primary',
                disabled: !canEnable.value,
                onClick: () => {
                  void enableAgent()
                },
              },
              enabling.value ? '启用中…' : '启用 Agent',
            ),
          ]),
          h('p', { class: 'muted small' }, '启用后新运行可立即使用该 Agent；也可以稍后在 Agent 页面启用。'),
        )
      }
      parts.push(h('p', {}, [h('a', { href: agentHref(current.agent) }, '打开 Agent 页面 →')]))
      return h('div', { class: 'created-lifecycle' }, parts)
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        pageHeading(
          'WORKSPACE / AGENT',
          '新建 Agent',
          '一句描述 → 作者模型起草 → 页面内编辑 → 创建（默认停用）→ 提交文件 → 启用。',
        ),
      ]

      const basicChildren: VNodeChild[] = renderBasicFields()
      if (authorHint.value !== '') {
        basicChildren.push(h(Notice, { tone: 'warn', text: authorHint.value }))
      }
      if (draftError.value !== '') {
        basicChildren.push(h(Notice, { tone: 'error', text: draftError.value }))
      }
      if (draftProblems.value.length > 0) {
        basicChildren.push(
          h(
            'ul',
            { class: 'problems' },
            draftProblems.value.map((problem) => h('li', { key: problem }, problem)),
          ),
        )
      }
      basicChildren.push(
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              disabled: !canGenerate.value || created.value !== null || pendingQuestions.value.length > 0,
              onClick: () => {
                void generateDraft()
              },
            },
            draftLoading.value ? '生成中…' : '生成草稿',
          ),
        ]),
      )
      content.push(section('基本信息', basicChildren))

      const questionsSection = renderQuestionsSection()
      if (questionsSection !== null) {
        content.push(questionsSection)
      }

      const preview = renderPreview()
      if (preview !== null) {
        content.push(preview)
      }

      const draftSection = renderDraftSection()
      if (draftSection !== null) {
        content.push(draftSection)
      }

      if (created.value === null) {
        const createChildren: VNodeChild[] = [
          h('p', { class: 'muted' }, '创建会写入模板 AGENT.md 并注册为停用状态；同名已存在时返回冲突文案。'),
        ]
        if (createError.value !== '') {
          createChildren.push(
            h(Notice, { tone: 'error', text: createError.value }),
            // FR26 零残余：创建失败会回滚状态与目录，所以可以原样重试。
            h(
              'p',
              { class: 'muted small' },
              '创建没有发生：工作区没有新增目录，也没有注册记录。修正上面的输入后可以直接重试，不会产生重复 Agent。',
            ),
          )
        }
        createChildren.push(
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                class: 'primary',
                disabled: !canCreate.value,
                onClick: () => {
                  void createAgent()
                },
              },
              creating.value ? '创建中…' : '创建 Agent',
            ),
          ]),
        )
        content.push(section('创建', createChildren))
      } else {
        const createdSection = renderCreatedSection()
        if (createdSection !== null) {
          content.push(createdSection)
        }
      }

      return h('section', { class: 'view view-agents-new' }, content)
    }
  },
})
