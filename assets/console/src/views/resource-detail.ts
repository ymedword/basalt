// 资源详情视图（/console/resources/:kind/:id）：版本列表 / 版本详情、发布 /
// 撤回（用旧版本内容创建新版本）、删除、引用面板与一次性连接测试；
// R4 追加模型连接的范围卡片（共享 ⇄ 专属）与托管凭据卡片（撤销确认）。
// 条件写一律使用详情 GET 返回的响应头 ETag；412 重新读取 ETag 与版本列表并
// **保留**已选版本；409 / 400 展示服务端固定文案与点名；写成功后重新 GET。
// Resource detail: versions, publish/revert, delete, references and one-shot
// connection tests, plus the R4 scope and managed-credential cards. Conditional
// writes use the detail GET's ETag; a 412 re-reads the ETag while keeping the
// selected version. No optimistic updates.
import { computed, defineComponent, h, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage, resourceErrorCopy } from '../api'
import type {
  ConnectionTestView,
  McpServerView,
  ModelConnectionView,
  ResourceScope,
  SkillView,
} from '../api'
import { adminApi } from '../api-admin'
import type {
  AdminResourceKind,
  McpServerTestResult,
  ModelConnectionTestResult,
  ResourcePublishResult,
  ResourceReferencesView,
  ResourceVersionDetail,
  ResourceVersionView,
} from '../api-admin'
import {
  CREDENTIAL_MODE_LABELS,
  CREDENTIAL_STATE_LABELS,
} from '../components/credential-field'
import { ResourceForm } from '../components/resource-form'
import { SCOPE_LABELS, ScopePicker } from '../components/scope-picker'
import {
  CodeBlock,
  ConfirmDialog,
  DataTable,
  Notice,
  formatTimestamp,
  shortHash,
} from '../components/ui'
import type { TableRow } from '../components/ui'
import { resourceHref } from '../router'
import type { ResourceKind } from '../router'
import {
  AVAILABILITY_LABELS,
  availabilityTone,
  badge,
  definitions,
  identifier,
  pageHeading,
  section,
  useLoader,
  viewProps,
} from './shared'

const KIND_NOUNS: Record<ResourceKind, string> = {
  skills: '技能',
  'mcp-servers': 'MCP 服务',
  'model-connections': '模型连接',
}

const KIND_CODES: Record<ResourceKind, string> = {
  skills: '05',
  'mcp-servers': '04',
  'model-connections': '03',
}

/** 页面种类（路由段）与契约种类（API 字面量）的固定映射。 */
const ADMIN_KINDS: Record<ResourceKind, AdminResourceKind> = {
  skills: 'skill',
  'mcp-servers': 'mcp_server',
  'model-connections': 'model_connection',
}

interface DetailState {
  readonly view: ModelConnectionView | McpServerView | SkillView
  readonly etag: string
}

export const ResourceDetailView = defineComponent({
  name: 'ResourceDetailView',
  props: viewProps,
  setup(props) {
    const kind = (props.params.kind ?? 'skills') as ResourceKind
    const id = props.params.id ?? ''
    const client = adminApi(props.ctx.api)
    const adminKind = ADMIN_KINDS[kind]
    const noun = KIND_NOUNS[kind]

    // --- 数据加载 ---------------------------------------------------------
    async function fetchDetail(): Promise<DetailState> {
      switch (kind) {
        case 'model-connections': {
          const detail = await client.getModelConnection(id)
          return { view: detail.data, etag: detail.etag }
        }
        case 'mcp-servers': {
          const detail = await client.getMcpServer(id)
          return { view: detail.data, etag: detail.etag }
        }
        case 'skills': {
          const detail = await client.getSkill(id)
          return { view: detail.data, etag: detail.etag }
        }
      }
    }

    const detail = useLoader(fetchDetail)
    const versions = useLoader(() => client.listResourceVersions(adminKind, id))
    const references = useLoader(() => client.getResourceReferences(adminKind, id))

    async function reloadAll(): Promise<void> {
      await Promise.all([detail.reload(), versions.reload(), references.reload()])
    }

    // --- 范围与托管凭据（仅模型连接有这两项）------------------------------
    // Scope and managed credentials exist for model connections only; the other
    // two kinds have no scope/credential fields in their views.
    const agents = useLoader(() => props.ctx.api.listProfiles(), false)
    const agentNames = computed(() => (agents.data.value ?? []).map((profile) => profile.name))
    const scopeEditing = ref(false)
    const draftScope = ref<ResourceScope>('shared')
    const draftOwner = ref('')
    const revokeOpen = ref(false)
    /** 被拒绝的写动作：决定点名卡片的标题与说明（删除 vs 切换范围）。 */
    const denialContext = ref<'delete' | 'scope'>('delete')

    function modelState(): ModelConnectionView | null {
      const state = detail.data.value
      if (state === null || kind !== 'model-connections') {
        return null
      }
      return state.view as ModelConnectionView
    }

    watch(detail.data, (next) => {
      // 每次重新读取都回到服务端真值：编辑器收起，草稿重置。
      scopeEditing.value = false
      if (next === null || kind !== 'model-connections') {
        draftScope.value = 'shared'
        draftOwner.value = ''
        return
      }
      const item = next.view as ModelConnectionView
      draftScope.value = item.scope
      draftOwner.value = item.owner_agent
    })

    function openScopeEditor(): void {
      resetFeedback()
      scopeEditing.value = true
      // 所有者候选只在真正要切换时才读取；读取失败保留编辑器并显示错误。
      if (agents.data.value === null && !agents.loading.value) {
        void agents.reload()
      }
    }

    async function changeScope(): Promise<void> {
      const item = modelState()
      const ifMatch = currentEtag()
      if (busy.value || item === null || ifMatch === null) {
        return
      }
      resetFeedback()
      const owner = draftOwner.value.trim()
      if (draftScope.value === 'exclusive' && owner === '') {
        error.value = '转专属时必须选择所有者 Agent'
        return
      }
      if (draftScope.value === item.scope && owner === item.owner_agent) {
        notice.value = '使用范围没有变化'
        return
      }
      const resourceUid = currentResourceUID()
      if (resourceUid === '') {
        error.value = '资源身份尚未读取，请刷新页面后再切换'
        return
      }
      denialContext.value = 'scope'
      busy.value = true
      try {
        const result = await client.setResourceScope(
          'model_connection',
          id,
          {
            resource_uid: resourceUid,
            scope: draftScope.value,
            ...(draftScope.value === 'exclusive' ? { owner_agent: owner } : {}),
          },
          ifMatch,
        )
        const affected = result.affected_eligible_agents
        scopeEditing.value = false
        if (result.scope === 'shared') {
          notice.value =
            affected.length === 0
              ? '已切换为共享（所有 Agent 可关联）。'
              : `已切换为共享；以下 Agent 可用但不自动改动：${affected.join('、')}。`
        } else {
          notice.value =
            affected.length === 0
              ? `已切换为专属（${result.owner_agent}）。`
              : `已切换为专属（${result.owner_agent}）；原本可用的 ${affected.join('、')} 需要另行关联。`
        }
        await reloadAll()
      } catch (caught) {
        await handleWriteFailure(caught)
      } finally {
        busy.value = false
      }
    }

    async function confirmRevoke(): Promise<void> {
      const item = modelState()
      const ifMatch = currentEtag()
      revokeOpen.value = false
      if (busy.value || item === null || ifMatch === null) {
        return
      }
      resetFeedback()
      const resourceUid = currentResourceUID()
      if (resourceUid === '') {
        error.value = '资源身份尚未读取，请刷新页面后再撤销'
        return
      }
      busy.value = true
      try {
        const result = await client.revokeCredential(
          'model_connection',
          id,
          { resource_uid: resourceUid, confirm: true },
          ifMatch,
        )
        notice.value = `已撤销 ${result.revoked_versions} 个凭据版本（撤销序号 ${result.revocation_epoch}）；旧版本不会复活，重新录入会生成新版本。`
        await reloadAll()
      } catch (caught) {
        await handleWriteFailure(caught)
      } finally {
        busy.value = false
      }
    }

    // --- 版本详情（不可变，412 不清空）------------------------------------
    const versionDetail = ref<ResourceVersionDetail | null>(null)
    const versionLoading = ref(false)
    const versionError = ref('')

    async function openVersion(versionId: number): Promise<void> {
      versionLoading.value = true
      versionError.value = ''
      try {
        versionDetail.value = await client.getResourceVersion(versionId)
      } catch (caught) {
        versionDetail.value = null
        versionError.value = errorMessage(caught)
      } finally {
        versionLoading.value = false
      }
    }

    // --- 写面状态 ---------------------------------------------------------
    const busy = ref(false)
    const error = ref('')
    const problems = ref<string[]>([])
    const deniedAgents = ref<string[]>([])
    const deniedRuns = ref<string[]>([])
    const notice = ref('')
    const deleteOpen = ref(false)
    const testResult = ref<ModelConnectionTestResult | McpServerTestResult | null>(null)
    // 引用解析与连接结果是两件事：503 = 环境变量未设置（测试未发出）；200 才说明
    // 服务进程读到了非空值，ok 只代表供应商是否接受这次调用。
    // Reference resolution is separate from call outcome: 503 means the variable was
    // missing (no call was made); a 200 proves the process read a non-empty value.
    const testReference = ref<'unknown' | 'resolved' | 'unresolved'>('unknown')

    function resetFeedback(): void {
      error.value = ''
      problems.value = []
      deniedAgents.value = []
      deniedRuns.value = []
      notice.value = ''
      testResult.value = null
      testReference.value = 'unknown'
    }

    /**
     * 失败信封：资源 / 凭据错误码优先用页面文案（它给出下一步），码表外回退
     * 服务端固定短语；点名名单单独渲染，不替代主提示。
     */
    function showFailure(caught: unknown): void {
      if (!(caught instanceof ApiError)) {
        error.value = errorMessage(caught)
        return
      }
      const copy = resourceErrorCopy(caught)
      error.value = copy === '' ? errorMessage(caught) : copy
      const data = caught.data
      if (data === null) {
        return
      }
      problems.value = data.problems ?? []
      deniedAgents.value = data.agents ?? []
      const runs = data.active_runs ?? []
      deniedRuns.value = runs.length > 0 ? runs : (data.run_ids ?? [])
    }

    /**
     * 412 = ETag 已前移：固定提示 + 重新读取详情 / 版本 / 引用；已选版本与
     * 页面输入原样保留，**不**自动重试。其它错误展示服务端文案。
     */
    async function handleWriteFailure(caught: unknown): Promise<void> {
      if (caught instanceof ApiError && caught.status === 412) {
        error.value = '资源已被他人修改，本次保存没有写入；已重新读取 ETag，请确认内容后再提交'
        await reloadAll()
        return
      }
      showFailure(caught)
    }

    function currentEtag(): string | null {
      const state = detail.data.value
      return state === null ? null : state.etag
    }

    async function publish(versionId?: number): Promise<void> {
      const ifMatch = currentEtag()
      if (busy.value || ifMatch === null) {
        return
      }
      resetFeedback()
      busy.value = true
      try {
        const result: ResourcePublishResult = await client.publishResource(
          adminKind,
          id,
          ifMatch,
          versionId,
        )
        if (result.pending_restart) {
          notice.value = `已发布版本 ${result.version.version_no}；MCP 连接在重启后生效（列表会标“需重启”）。`
        } else if (versionId === undefined) {
          notice.value = `已发布版本 ${result.version.version_no}；版本列表与引用已重新读取。`
        } else {
          notice.value = `已用版本 ${versionId} 的内容创建版本 ${result.version.version_no} 并发布；旧版本原样保留。`
        }
        await reloadAll()
      } catch (caught) {
        await handleWriteFailure(caught)
      } finally {
        busy.value = false
      }
    }

    /**
     * 删除的身份：契约要求 `If-Match` 之外显式给出 `resource_uid`，且必须等于
     * 当前指针。技能 / MCP 的详情视图不带 UID，页面从引用视图（`GET /resources/
     * {kind}/{name}/references`）取——读不到身份就不发请求。
     */
    function currentResourceUID(): string {
      const refs = references.data.value
      return refs === null ? '' : refs.resource_uid
    }

    async function confirmDelete(): Promise<void> {
      const ifMatch = currentEtag()
      const resourceUid = currentResourceUID()
      if (busy.value || ifMatch === null) {
        return
      }
      if (resourceUid === '') {
        error.value = '资源身份尚未读取，请刷新页面后再删除'
        return
      }
      resetFeedback()
      denialContext.value = 'delete'
      busy.value = true
      try {
        switch (kind) {
          case 'model-connections':
            await client.deleteModelConnection(id, resourceUid, ifMatch)
            break
          case 'mcp-servers':
            await client.deleteMcpServer(id, resourceUid, ifMatch)
            break
          case 'skills':
            await client.deleteSkill(id, resourceUid, ifMatch)
            break
        }
        deleteOpen.value = false
        // 删除后详情不再可读：回到列表（列表在挂载时重新 GET）。
        props.ctx.router.navigate(resourceHref(kind), { replace: true })
      } catch (caught) {
        deleteOpen.value = false
        await handleWriteFailure(caught)
      } finally {
        busy.value = false
      }
    }

    async function runTest(): Promise<void> {
      if (busy.value) {
        return
      }
      resetFeedback()
      busy.value = true
      try {
        if (kind === 'model-connections') {
          try {
            testResult.value = await client.testModelConnection(id)
            testReference.value = 'resolved'
          } catch (caught) {
            if (caught instanceof ApiError) {
              if (caught.status === 503) {
                testReference.value = 'unresolved'
              } else if (caught.status === 504) {
                // 超时发生在客户端构建之后，说明引用已经解析成功。
                testReference.value = 'resolved'
              }
            }
            showFailure(caught)
          }
          // 测试结论投影（last_test）由服务端记录；重新读取摘要，页面不自行拼一条。
          await detail.reload()
          return
        }
        if (kind === 'mcp-servers') {
          testResult.value = await client.testMcpServer(id)
        }
      } catch (caught) {
        showFailure(caught)
      } finally {
        busy.value = false
      }
    }

    // --- 投影 -------------------------------------------------------------

    /**
     * 最近实测：只来自服务端 `last_test` 记录。`null` = 从未测试；本地「配置就绪」
     * 不是一次测试，二者在同一块里分两行显示，绝不互相冒充。
     * The last recorded test only; local readiness is a separate row, never a proxy.
     */
    function lastTestValue(test: ConnectionTestView | null): VNodeChild {
      if (test === null) {
        return h('span', { class: 'muted' }, '从未测试（配置就绪不代表已实测）')
      }
      const tone = test.status === 'success' ? 'ok' : test.status === 'failed' ? 'bad' : 'muted'
      const label =
        test.status === 'success' ? '成功' : test.status === 'failed' ? '失败' : '结果未知'
      return h('span', {}, [
        badge(label, tone),
        h(
          'span',
          { class: 'muted small' },
          ` · ${formatTimestamp(test.tested_at)}${test.detail === '' ? '' : ` · ${test.detail}`}`,
        ),
      ])
    }

    function renderModelSummary(item: ModelConnectionView, etag: string): VNode {
      return h('div', {}, [
        definitions([
          ['名称', identifier(item.name)],
          ['Base URL', identifier(item.base_url)],
          ['默认模型', identifier(item.default_model)],
          ['凭据来源', CREDENTIAL_MODE_LABELS[item.credential_mode]],
          [
            '环境变量名',
            item.api_key_env === '' ? '—（加密托管）' : identifier(item.api_key_env),
          ],
          [
            '本地配置就绪',
            badge(AVAILABILITY_LABELS[item.availability], availabilityTone(item.availability)),
          ],
          ['最近实测', lastTestValue(item.last_test)],
          ['发布版本', `v${item.published_version_no}`],
          ['已应用', `v${item.applied_version_no}`],
          ['配置引用 Agent', item.referenced_by.length === 0 ? '无' : item.referenced_by.join('、')],
        ]),
        h(
          'p',
          { class: 'muted small' },
          '「本地配置就绪」只看本机配置与凭据状态；「最近实测」只来自一次真实连接测试。两者分开显示：配置就绪不等于已实测可用，测试失败也不会显示成已验证。',
        ),
        h(
          'p',
          { class: 'muted small' },
          '「环境变量」来源只保存变量名，变量值由部署者注入进程环境（替换后需要重启）；「加密托管」来源只保存加密结果与尾号。两种来源都不在页面上回显原值。',
        ),
        h('details', { class: 'technical-details' }, [
          h('summary', '技术细节'),
          definitions([
            ['配置校验标识（ETag）', h('code', { class: 'resource-etag' }, etag)],
            ['发布版本 ID', String(item.published_version_id)],
            ['已应用版本 ID', String(item.applied_version_id)],
            ['资源 UID', h('code', {}, item.resource_uid)],
          ]),
        ]),
      ])
    }

    /** 凭据状态徽标：复用凭据组件的中文标签与色调，避免两处措辞分叉。 */
    function credentialBadge(item: ModelConnectionView): VNode {
      const info = CREDENTIAL_STATE_LABELS[item.credential_state]
      return badge(info.label, info.tone)
    }

    /** 使用范围卡片：共享 ⇄ 专属；共享转专属被引用时 409 点名，无强制选项。 */
    function renderScopeCard(): VNodeChild {
      const item = modelState()
      if (item === null) {
        return null
      }
      const pairs: Array<readonly [string, VNodeChild]> = [
        ['当前范围', SCOPE_LABELS[item.scope]],
      ]
      if (item.scope === 'exclusive') {
        pairs.push(['所有者', item.owner_agent === '' ? '—' : identifier(item.owner_agent)])
      }
      const children: VNodeChild[] = [
        definitions(pairs),
        h(
          'p',
          { class: 'muted small' },
          '共享 = 所有 Agent 可关联；专属 = 只有所有者可关联。共享转专属会先检查其他 Agent 的配置引用与运行占用，仍被使用时返回 409 并点名，不提供强制选项，也不会自动解除关联。',
        ),
      ]
      if (!scopeEditing.value) {
        children.push(
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                disabled: busy.value,
                onClick: () => {
                  openScopeEditor()
                },
              },
              '切换范围',
            ),
          ]),
        )
      } else {
        children.push(
          h(ScopePicker, {
            id: 'resource-scope',
            scope: draftScope.value,
            ownerAgent: draftOwner.value,
            agents: agentNames.value,
            disabled: busy.value,
            'onUpdate:scope': (next: ResourceScope) => {
              draftScope.value = next
            },
            'onUpdate:ownerAgent': (next: string) => {
              draftOwner.value = next
            },
          }),
          agents.error.value === '' ? null : h(Notice, { tone: 'error', text: agents.error.value }),
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
              busy.value ? '提交中…' : '确认切换',
            ),
            h(
              'button',
              {
                type: 'button',
                disabled: busy.value,
                onClick: () => {
                  scopeEditing.value = false
                },
              },
              '取消',
            ),
          ]),
        )
      }
      return section('使用范围', children)
    }

    /** 托管凭据卡片：只读状态 + 撤销确认；重新录入在下方修改表单里完成。 */
    function renderCredentialCard(): VNodeChild {
      const item = modelState()
      if (item === null) {
        return null
      }
      const children: VNodeChild[] = [
        definitions([
          ['凭据来源', CREDENTIAL_MODE_LABELS[item.credential_mode]],
          ['状态', credentialBadge(item)],
          ['凭据版本', item.credential_version === 0 ? '—' : `v${item.credential_version}`],
          ['尾号', item.last4 === '' ? '—' : `····${item.last4}`],
          ['凭据引用', item.credential_ref === '' ? '—' : identifier(item.credential_ref)],
        ]),
      ]
      if (item.credential_mode === 'env') {
        children.push(
          h(
            'p',
            { class: 'muted small' },
            '「环境变量」来源：资源只保存 ${ENV_VAR} 占位，没有可撤销的加密凭据。',
          ),
        )
      } else {
        children.push(
          h(
            'p',
            { class: 'muted small' },
            item.credential_state === 'active'
              ? '加密托管的凭据由服务端主密钥加密；撤销后所有旧版本永不复活，在途 Run 会在下一次模型调用检查点失败。重新录入会生成新的凭据版本（用下方修改表单）。'
              : '当前没有可用的加密凭据；在下方修改表单里重新录入会生成新的凭据版本，旧版本不会复活。',
          ),
        )
        if (item.credential_state === 'active') {
          children.push(
            h('div', { class: 'form-actions' }, [
              h(
                'button',
                {
                  type: 'button',
                  class: 'danger',
                  disabled: busy.value,
                  onClick: () => {
                    resetFeedback()
                    revokeOpen.value = true
                  },
                },
                '撤销凭据',
              ),
            ]),
          )
        }
      }
      return section('凭据管理', children)
    }

    function renderMcpSummary(item: McpServerView, etag: string): VNode {
      return h('div', {}, [
        definitions([
          ['名称', identifier(item.name)],
          ['传输', item.transport],
          ['可执行名', identifier(item.command)],
          ['环境变量键', item.env_keys.length === 0 ? '无' : item.env_keys.join('、')],
          ['连接', item.connected ? badge('已连接', 'ok') : badge('未连接', 'bad')],
          ['工具', item.tools.length === 0 ? '无' : item.tools.join('、')],
          ['发布版本', `v${item.published_version_no}`],
          ['已应用', `v${item.applied_version_no}`],
          ['待重启', item.pending_restart ? badge('需重启', 'warn') : '否'],
        ]),
        h('details', { class: 'technical-details' }, [
          h('summary', '技术细节'),
          definitions([
            ['配置校验标识（ETag）', h('code', { class: 'resource-etag' }, etag)],
            ['发布版本 ID', String(item.published_version_id)],
            ['已应用版本 ID', String(item.applied_version_id)],
          ]),
        ]),
      ])
    }

    function renderSkillSummary(item: SkillView, etag: string): VNode {
      return h('div', {}, [
        definitions([
          ['名称', identifier(item.name)],
          ['描述', item.description === '' ? '—' : item.description],
          ['当前版本', `v${item.version_no}`],
          ['文件数', String(item.file_count)],
          ['配置引用 Agent', item.referenced_by.length === 0 ? '无' : item.referenced_by.join('、')],
        ]),
        h('details', { class: 'technical-details' }, [
          h('summary', '技术细节'),
          definitions([
            ['内容哈希', identifier(item.content_hash)],
            ['配置校验标识（ETag）', h('code', { class: 'resource-etag' }, etag)],
            ['版本 ID', String(item.version_id)],
          ]),
        ]),
      ])
    }

    function renderSummary(state: DetailState): VNode {
      if (kind === 'model-connections') {
        return renderModelSummary(state.view as ModelConnectionView, state.etag)
      }
      if (kind === 'mcp-servers') {
        return renderMcpSummary(state.view as McpServerView, state.etag)
      }
      return renderSkillSummary(state.view as SkillView, state.etag)
    }

    /** 来源版本号：优先从已读到的版本列表按 version_id 反查。 */
    function sourceVersionNo(
      item: ResourceVersionView,
      data: readonly ResourceVersionView[],
    ): string {
      const sourceId = item.reverted_from_version_id
      if (sourceId === null) {
        return '—'
      }
      const source = data.find((entry) => entry.version_id === sourceId)
      return source === undefined ? `撤回自 #${sourceId}` : `撤回自 v${source.version_no}`
    }

    function versionRows(data: readonly ResourceVersionView[]): TableRow[] {
      return data.map((item) => ({
        key: String(item.version_id),
        cells: [
          `v${item.version_no}`,
          h('code', { title: item.content_hash }, shortHash(item.content_hash)),
          formatTimestamp(item.created_at),
          identifier(item.operation_id),
          item.published ? badge('已发布', 'ok') : badge('历史', 'muted'),
          item.applied ? badge('已应用', 'ok') : '—',
          sourceVersionNo(item, data),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                disabled: busy.value,
                onClick: () => {
                  void openVersion(item.version_id)
                },
              },
              '详情',
            ),
            item.published
              ? h('span', { class: 'muted small' }, '当前发布')
              : h(
                  'button',
                  {
                    type: 'button',
                    disabled: busy.value,
                    title: '用该版本内容创建新版本并发布；历史版本不被改写',
                    onClick: () => {
                      void publish(item.version_id)
                    },
                  },
                  '撤回至此版本',
                ),
          ]),
        ],
      }))
    }

    /**
     * 按 UID 精确绑定的 Agent：归档的也列出（仍占引用）；迁移前没有 UID 证据的
     * 旧状态文件如实标“引用未记录”，不补造身份。
     */
    function boundAgentList(value: ResourceReferencesView): VNodeChild {
      if (value.bound_agents.length === 0) {
        return '无'
      }
      return h(
        'ul',
        { class: 'plain-list' },
        value.bound_agents.map((ref) =>
          h('li', { key: `${ref.agent}:${ref.agent_uid}` }, [
            h(
              'span',
              { title: ref.agent_uid === '' ? undefined : `Agent UID：${ref.agent_uid}` },
              ref.agent,
            ),
            ref.archived ? badge('已归档', 'muted') : null,
            ref.agent_uid === ''
              ? h('span', { class: 'muted small' }, ' · 引用未记录（迁移前无法证明身份）')
              : null,
          ]),
        ),
      )
    }

    function renderReferences(): VNodeChild {
      const value = references.data.value
      if (value === null) {
        return references.loading.value ? h('p', { class: 'muted' }, '加载中…') : null
      }
      return h('div', {}, [
        definitions([
          ['配置引用 Agent（精确）', boundAgentList(value)],
          ['文档提及 Agent（扫描）', value.agents.length === 0 ? '无' : value.agents.join('、')],
          ['运行占用', value.active_runs.length === 0 ? '无' : value.active_runs.join('、')],
        ]),
        h(
          'p',
          { class: 'muted small' },
          '「配置引用」按资源 UID 精确关联（含已归档 Agent）；「文档提及」是对活动与归档 AGENT.md 的整词扫描，只是扫描出的线索，不代表资源可用。',
        ),
        h('details', { class: 'technical-details' }, [
          h('summary', '技术细节'),
          definitions([
            ['资源 UID', identifier(value.resource_uid)],
            ['发布版本 ID', String(value.published_version_id)],
          ]),
        ]),
      ])
    }

    function renderVersionDetail(): VNodeChild {
      if (versionLoading.value) {
        return h('p', { class: 'muted' }, '读取版本详情…')
      }
      if (versionError.value !== '') {
        return h(Notice, { tone: 'error', text: versionError.value })
      }
      const value = versionDetail.value
      if (value === null) {
        return h('p', { class: 'muted' }, '在版本列表点“详情”查看该版本的定义与清单。')
      }
      return h('div', {}, [
        h('h3', `版本 v${value.version_no}`),
        h('p', { class: 'muted small' }, [
          '资源身份 ',
          h('code', {}, value.resource_uid),
          '（同名重建后的新旧版本靠它区分）',
        ]),
        value.reverted_from_version_id === null
          ? null
          : h('p', { class: 'muted small' }, `撤回自版本 ID（内部标识）${value.reverted_from_version_id}`),
        h('p', { class: 'muted small' }, '定义原文（已脱敏；技能恒为空，内容以文件清单为准）'),
        h(CodeBlock, { text: value.definition_yaml === '' ? '（空）' : value.definition_yaml }),
        h('p', { class: 'muted small' }, '清单 manifest'),
        h(CodeBlock, { text: JSON.stringify(value.manifest, null, 2) }),
      ])
    }

    function renderTestResult(): VNodeChild {
      const result = testResult.value
      if (result === null && testReference.value === 'unknown') {
        return null
      }
      const pairs: Array<readonly [string, VNodeChild]> = []
      if (kind === 'model-connections') {
        if (testReference.value === 'resolved') {
          pairs.push(['引用解析', badge('已解析', 'ok')])
        } else if (testReference.value === 'unresolved') {
          pairs.push(['引用解析', badge('不可解析', 'bad')])
        }
      }
      if (result === null) {
        // 只有模型连接会出现：503 = 环境变量未设置（测试未发出）；504 = 引用已解析
        // 但调用超时，结论是“未完成”而不是“引用不可解析”。
        pairs.push([
          '结果',
          testReference.value === 'unresolved'
            ? '未执行（密钥引用无法解析）'
            : '未完成（测试超时，详见上方错误）',
        ])
      } else {
        pairs.push(['结果', result.ok ? badge('成功', 'ok') : badge('失败', 'bad')])
        pairs.push(['耗时', `${result.latency_ms} ms`])
        if ('tools' in result) {
          pairs.push(['发现工具', result.tools.length === 0 ? '无' : result.tools.join('、')])
        } else {
          pairs.push(['模型', identifier(result.model)])
        }
        pairs.push(['错误码', identifier(result.error_code === '' ? '—' : result.error_code)])
        pairs.push(['错误信息', result.error_message === '' ? '—' : result.error_message])
      }
      return section('测试结果', [
        definitions(pairs),
        kind === 'model-connections'
          ? h(
              'p',
              { class: 'muted small' },
              testReference.value === 'unresolved'
                ? '“引用不可解析”只表示服务进程没有读到该环境变量的非空值，测试没有发出；请检查部署环境并重启服务。这不等于密钥无效。'
                : result === null
                  ? '“引用已解析”只表示服务进程读到了非空环境变量；本次测试超时，未得出密钥是否有效。'
                  : '“引用已解析”只表示服务进程读到了非空环境变量，不代表密钥被供应商认可；密钥是否有效以连接测试结果为准。',
            )
          : null,
      ])
    }

    function renderDenial(): VNodeChild {
      if (deniedAgents.value.length === 0 && deniedRuns.value.length === 0) {
        return null
      }
      const scope = denialContext.value === 'scope'
      return h('div', { class: 'card' }, [
        h('h2', scope ? '切换范围被拒绝：仍有配置引用或运行占用' : '删除被拒绝：仍有配置引用或运行占用'),
        h(
          'ul',
          { class: 'problems' },
          [
            ...deniedAgents.value.map((name) => h('li', { key: `agent:${name}` }, `Agent 配置引用：${name}`)),
            ...deniedRuns.value.map((runId) => h('li', { key: `run:${runId}` }, `运行占用：${runId}`)),
          ],
        ),
        h(
          'p',
          { class: 'muted small' },
          scope
            ? '先在对应 Agent 里解除关联或等待运行结束；共享转专属不提供强制选项，也不会自动解除关联。'
            : '先解除配置引用或等待运行占用结束；资源删除不提供强制选项。',
        ),
      ])
    }

    function renderProblems(): VNodeChild {
      if (problems.value.length === 0) {
        return null
      }
      return h(
        'ul',
        { class: 'problems' },
        problems.value.map((problem) => h('li', { key: problem }, problem)),
      )
    }

    function actionButtons(): VNode {
      const buttons: VNodeChild[] = [
        h(
          'button',
          {
            type: 'button',
            class: 'primary',
            disabled: busy.value || detail.data.value === null,
            onClick: () => {
              void publish()
            },
          },
          busy.value ? '提交中…' : '发布最新版本',
        ),
      ]
      if (kind !== 'skills') {
        buttons.push(
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value || detail.data.value === null,
              onClick: () => {
                void runTest()
              },
            },
            '测试连接',
          ),
        )
      }
      buttons.push(
        h(
          'button',
          {
            type: 'button',
            class: 'danger',
            disabled: busy.value || detail.data.value === null,
            onClick: () => {
              deleteOpen.value = true
            },
          },
          '删除',
        ),
      )
      return h('div', { class: 'form-actions' }, buttons)
    }

    return (): VNode => {
      const state = detail.data.value
      const children: VNodeChild[] = [
        h('p', { class: 'back-link' }, [
          h('a', { href: resourceHref(kind) }, `← 返回${noun}列表`),
        ]),
      ]

      if (detail.error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: detail.error.value }))
      }
      if (detail.loading.value && state === null) {
        children.push(h('p', { class: 'muted' }, '加载中…'))
      }
      if (state !== null) {
        children.push(section('定义与现状', [renderSummary(state)]))
      }

      if (error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: error.value }))
      }
      children.push(renderProblems())
      children.push(renderDenial())
      if (notice.value !== '') {
        children.push(h(Notice, { tone: 'info', text: notice.value }))
      }

      children.push(
        section('引用与占用', [renderReferences()]),
        renderScopeCard(),
        renderCredentialCard(),
        section('版本（只追加；撤回会创建新版本）', [
          versions.error.value === '' ? null : h(Notice, { tone: 'error', text: versions.error.value }),
          versions.data.value === null
            ? h('p', { class: 'muted' }, versions.loading.value ? '加载中…' : '暂无版本')
            : h(DataTable, {
                headers: ['版本', '内容哈希', '创建时间', '操作标识', '状态', '已应用', '来源', '操作'],
                rows: versionRows(versions.data.value),
                empty: '暂无版本',
                tableClass: 'versions-table',
                columnClasses: ['col-number', 'col-hash', 'col-time', 'col-id', 'col-state', 'col-state', 'col-short', 'col-actions'],
              }),
        ]),
        section('版本详情', [renderVersionDetail()]),
      )

      children.push(renderTestResult())

      // 三类资源都可以在详情页修改：表单自己 GET 详情取 ETag，保存后整页重读。
      // All three kinds are editable here; the form GETs its own ETag.
      if (state !== null) {
        children.push(
          h(ResourceForm, {
            api: props.ctx.api,
            kind,
            edit: id,
            onSaved: () => {
              void reloadAll()
            },
          }),
        )
      }

      children.push(
        h(
          ConfirmDialog,
          {
            open: deleteOpen.value,
            title: `删除${noun}`,
            message: `删除「${id}」后历史版本与运行占用记录保留，运行对象立即移除；此操作不可撤销。`,
            confirmText: '删除',
            busy: busy.value,
            onConfirm: () => {
              void confirmDelete()
            },
            onCancel: () => {
              deleteOpen.value = false
            },
          },
        ),
        // 撤销凭据是另一条独立确认：不删除资源、不改历史版本，只撤销身份。
        h(ConfirmDialog, {
          open: revokeOpen.value,
          title: '撤销加密凭据',
          message: `撤销「${id}」的全部凭据版本后，旧版本永不复活；在途 Run 会在下一次模型调用检查点失败，通知渠道会在下一次发送时失败。需要恢复时只能重新录入（生成新的凭据版本）。`,
          confirmText: '撤销凭据',
          busy: busy.value,
          onConfirm: () => {
            void confirmRevoke()
          },
          onCancel: () => {
            revokeOpen.value = false
          },
        }),
      )

      return h('section', { class: 'view view-resource-detail' }, [
        pageHeading(
          'RESOURCES / ' + KIND_CODES[kind],
          `${noun}：${id}`,
          '版本记录与发布指针来自服务端；保存时用配置校验标识（ETag）避免覆盖他人的修改。',
          actionButtons(),
        ),
        ...children,
      ])
    }
  },
})
