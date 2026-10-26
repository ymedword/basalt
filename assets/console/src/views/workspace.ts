// 只读页与执行环境的白名单编辑入口（R8 裁决 2：保存即热生效，不重启）。
// Read-only views plus the live network-allowlist editor on the execution page.
import { defineComponent, h, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type {
  ExecutionPolicyView,
  ExecutionProfile,
  McpServerDiagnostics,
  McpServerView,
  NetworkAllowlistView,
  NetworkExceptionView,
  PendingPolicyConfirmation,
  SandboxView as SandboxViewData,
} from '../api'
import { adminApi } from '../api-admin'
import { catalogApi } from '../api-catalog'
import { NotifyChannels } from '../components/notify-channels'
import { CodeBlock, ConfirmDialog, DataTable, Notice, StatusBadge, formatTimestamp } from '../components/ui'
import type { Tone } from '../components/ui'
import { resourceDetailHref } from '../router'
import type { NoticeTone } from './shared'
import { capabilityTabs, definitions, identifier, pageHeading, section, useLoader, viewProps } from './shared'

function page(title: string, note: string, content: VNodeChild[]): VNode {
  return h('section', { class: 'view view-library' }, [
    pageHeading('WORKSPACE RESOURCES', title, note),
    ...content,
  ])
}

export const ToolsView = defineComponent({
  name: 'ToolsView', props: viewProps,
  setup(props) {
    const listing = useLoader(() => props.ctx.api.listTools())
    const sourceLabels = { builtin: '内置工具', mcp: 'MCP', code: '代码工具' }
    return () => page('工具与 MCP', '公共能力目录。每个 Agent 使用哪些能力，由它自己的配置声明。', [
      capabilityTabs('tools'),
      listing.error.value ? h(Notice, { tone: 'error', text: listing.error.value }) : null,
      listing.loading.value ? h('p', { class: 'loading' }, '正在读取工具目录…') : null,
      listing.data.value === null ? null : h(DataTable, { tableClass: 'tools-table', columnClasses: ['col-tool', '', 'col-source'], headers: ['工具', '说明', '来源'], rows: listing.data.value.map(tool => ({ key: tool.name, cells: [identifier(tool.name), tool.description || '—', sourceLabels[tool.source]] })), empty: '当前没有可用工具' }),
    ])
  },
})

export const GlobalMemoryView = defineComponent({
  name: 'GlobalMemoryView', props: viewProps,
  setup(props) {
    const memory = useLoader(() => props.ctx.api.getGlobalMemory())
    // 分区按行为命名（常驻 / 可检索）；文件里的标题固定为「核心记忆 / 归档记忆」，
    // 两者是同一份数据，不是两套存储。
    // Sections are named by behaviour; the file headings stay 核心记忆 / 归档记忆.
    return () => page('全局记忆', '工作区独立维护的一层长期记忆：不是各 Agent 记忆的汇总，也不是会话历史。页面只读；Agent 自己的记忆在对应 Agent 详情中查看。', [
      memory.error.value ? h(Notice, { tone: 'error', text: memory.error.value }) : null,
      memory.loading.value ? h('p', { class: 'loading' }, '正在读取全局记忆…') : null,
      memory.data.value === null ? null : h('div', { class: 'memory-columns' }, [
        section('常驻记忆', [
          h('p', { class: 'muted small' }, '运行开始时整段注入系统提示词；每次运行只读一次，运行内不再变化。'),
          memory.data.value.core ? h(CodeBlock, { text: memory.data.value.core }) : h('p', { class: 'empty' }, '还没有全局常驻记忆。'),
        ]),
        section('可检索记忆', [
          h('p', { class: 'muted small' }, '不自动进入上下文；模型需要时按关键词取回，整串子串匹配、区分大小写——是关键词检索，不是语义检索。'),
          memory.data.value.archive ? h(CodeBlock, { text: memory.data.value.archive }) : h('p', { class: 'empty' }, '还没有全局可检索记忆。'),
        ]),
      ]),
      h('p', { class: 'muted small' }, '生效规则：有 Agent 上下文且该层任一分区有内容 → 用 Agent 层；否则回退到这一层。两层不叠加，Agent 层只有可检索条目时也不会回退。'),
      h('p', { class: 'muted small' }, '写入只由记忆工具 save_memory 完成：只有 Agent 在运行中成功调用才会产生条目，不会从会话历史或所有对话里自动抽取。会话消息会自动保存为会话历史，但会话历史不会自动变成记忆条目。正常运行的 Agent 只写自己那一层，因此这里通常由维护者直接编辑 .basalt/memory/MEMORY.md（分区标题固定写作「核心记忆 / 归档记忆」）；控制台不提供写入。'),
    ])
  },
})

export const NotificationsView = defineComponent({
  name: 'NotificationsView', props: viewProps,
  setup(props) {
    const channels = useLoader(() => props.ctx.api.listNotifyChannels())
    return () => page('通知渠道', '管理 Agent 发送结果通知时使用的渠道；地址只接受 ${ENV_VAR} 占位，列表只显示目标服务。每个 Agent 能用哪些渠道，在 Agent 详情的「通知渠道」中声明。飞书等平台的入站消息路由到某个 Agent 属于后续「消息接入与路由」，不在此页。', [
      channels.error.value ? h(Notice, { tone: 'error', text: channels.error.value }) : null,
      channels.loading.value ? h('p', { class: 'loading' }, '正在读取通知渠道…') : null,
      h(NotifyChannels, {
        api: props.ctx.api,
        channels: channels.data.value ?? [],
        // 写成功后由页面重新 GET；组件不做乐观更新（注 20）。
        onChanged: () => {
          void channels.reload()
        },
      }),
    ])
  },
})

/** 空态固定文案（R8.2.2）：说清后果与边界，不写含糊的"暂无数据"。 */
const EMPTY_ALLOWLIST_COPY = '未配置允许域名，受本页策略约束的出站请求将被拒绝。模型连接与 stdio MCP 子进程不受本页约束。'
/** 标准档下的空白名单不拒绝公网：空态文案随档位变化，否则页面会说出与实际相反的话。 */
const EMPTY_ALLOWLIST_COPY_STANDARD = '未配置允许域名；标准模式仍允许未命中禁止域名的公网访问，切到严格模式后出网只剩访问例外。'

/** 档位中文标签（与契约字面量一一对应）。 */
const PROFILE_LABELS: Record<ExecutionProfile, string> = { standard: '标准', strict: '严格' }

/** 档位徽标色调：标准是正常态，不因它不是严格档就显示黄色警告。 */
const PROFILE_BADGE_TONES: Record<ExecutionProfile, Tone> = { standard: 'muted', strict: 'ok' }

/** 随包组件显示名；未知组件原样显示，不编造产品名。 */
const RUNTIME_COMPONENT_LABELS: Record<string, string> = {
  node: 'Node.js',
  python: 'Python',
  dbhub: 'DBHub',
  fetch: 'Fetch',
  'docs-readonly': 'docs-readonly',
  'github-mcp-server': 'GitHub MCP Server',
  'brave-search': 'Brave Search',
}

/**
 * 生效元数据的来源文案：档位行可能来自迁移或实例初始化，白名单只有前两种。
 * The profile row can also be seeded by a migration or by instance init.
 */
function policySourceLabel(source: NetworkAllowlistView['source']): string {
  switch (source) {
    case 'config_seed':
      return '配置种子（首次启动写入）'
    case 'migration':
      return '版本迁移写入'
    case 'instance_init':
      return '实例初始化写入'
    default:
      return '管理台保存'
  }
}

/** 待确认项的两个轴各自成组展示，处置动作也按轴不同（例外只覆盖网络）。 */
const PENDING_AXIS_LABELS: Record<PendingPolicyConfirmation['axis'], string> = {
  network_allowlist: '出网白名单',
  shell_command: '命令白名单',
}

function pendingReasonLabel(item: PendingPolicyConfirmation): string {
  if (item.reason === 'shell_name') {
    return '命中交互式 shell 名称：即使写进命令白名单也不放行'
  }
  return item.resolved_protected.length === 0
    ? '解析到受保护地址段'
    : `解析到受保护地址：${item.resolved_protected.join('、')}`
}

/** 例外类型标签（与契约 kind 一一对应）。 */
const EXCEPTION_KIND_LABELS: Record<NetworkExceptionView['kind'], string> = {
  host: '主机名',
  ip_port: 'IP:端口',
}

type Feedback = { tone: NoticeTone; text: string } | null

export const SandboxView = defineComponent({
  name: 'SandboxView', props: viewProps,
  setup(props) {
    const sandbox = useLoader(() => props.ctx.api.getSandbox())
    const policy = useLoader(() => props.ctx.api.getExecutionPolicy())
    const exceptions = useLoader(() => props.ctx.api.listNetworkExceptions())
    const admin = adminApi(props.ctx.api)

    // —— 运行环境：已配置 MCP 服务 + 各自诊断 ——
    // 页面只读既有端点：存在性来自诊断的依赖轴（只做 os.Stat），可用性只以 supervisor
    // 的真实连接为准，执行授权只看命令是否登记。三者分列，互不代替；不另造“组件清单”
    // 端点，也不以配置数组为空推断“没安装”。
    // The runtime section reuses the existing server list and per-server diagnosis:
    // existence is stat-based, availability is a live connection, authorization is the
    // command registration — three separate facts that never stand in for each other.
    const runtime = useLoader(async () => {
      const servers = await props.ctx.api.listMcpServers()
      if (!Array.isArray(servers)) {
        throw new Error('MCP 服务列表返回了非列表结构，运行环境状态无法确认。')
      }
      const client = catalogApi(props.ctx.api)
      const diagnoses = new Map<string, McpServerDiagnostics | null>()
      await Promise.all(servers.map(async (server) => {
        try {
          diagnoses.set(server.name, await client.getMCPDiagnostics(server.name))
        } catch {
          // 单个 server 的诊断读不到不拖垮整节：那一行按“未确认”展示，不猜就绪。
          diagnoses.set(server.name, null)
        }
      }))
      return { servers, diagnoses }
    })

    // —— 网络域名白名单 ——
    const draft = ref('')
    const dirty = ref(false)
    const saving = ref(false)
    const feedback = ref<Feedback>(null)

    // 服务端真值只在用户没有未保存输入时覆盖草稿：保存失败必须保留输入。
    // Live values seed the editor only while there is no unsaved input.
    function syncDraft(view: SandboxViewData): void {
      if (!dirty.value) {
        draft.value = view.http_allowed_domains.join('\n')
      }
    }
    watch(sandbox.data, (view) => {
      if (view !== null) {
        syncDraft(view)
      }
    }, { immediate: true })

    async function save(): Promise<void> {
      const view = sandbox.data.value
      if (view === null || saving.value) {
        return
      }
      const etag = view.network_allowlist.etag
      if (etag === '') {
        feedback.value = { tone: 'warn', text: '服务端尚未装载白名单，暂不能保存；请刷新页面后重试。' }
        return
      }
      const domains = draft.value.split('\n').map(line => line.trim()).filter(line => line !== '')
      saving.value = true
      feedback.value = null
      try {
        // 成功响应是服务端重新读取的视图，不是请求体回显：页面按它回显，不做乐观更新。
        const updated = await admin.updateNetworkDomains(domains, etag)
        sandbox.data.value = updated
        dirty.value = false
        syncDraft(updated)
        feedback.value = {
          tone: 'info',
          text: `已保存并立即生效：修订 ${updated.network_allowlist.revision}，当前允许 ${updated.http_allowed_domains.length} 个域名。`,
        }
      } catch (error) {
        // 412 / 409：条件未满足或别处已写入。重新读取实际生效状态，输入原样保留，
        // 绝不自动重发这次写入——是否再保存由用户核对后决定。
        if (error instanceof ApiError && (error.status === 412 || error.status === 409)) {
          await sandbox.reload()
          // reload 自身失败时 useLoader 只置 error（不抛）：不能笼统宣称“已重新读取”。
          const reread = sandbox.error.value === '' ? '页面已重新读取最新状态；' : '重新读取最新状态失败，请稍后再试；'
          feedback.value = {
            tone: 'warn',
            text: `${errorMessage(error)} ${reread}你的输入已保留，请核对后再保存。`,
          }
        } else {
          feedback.value = { tone: 'error', text: errorMessage(error) }
        }
      } finally {
        saving.value = false
      }
    }

    // —— 禁止域名表（与白名单同构的第二行）——
    const deniedDraft = ref('')
    const deniedDirty = ref(false)
    const deniedSaving = ref(false)
    const deniedFeedback = ref<Feedback>(null)

    function syncDeniedDraft(view: SandboxViewData): void {
      if (!deniedDirty.value) {
        deniedDraft.value = view.denied_domains.join('\n')
      }
    }
    watch(sandbox.data, (view) => {
      if (view !== null) {
        syncDeniedDraft(view)
      }
    }, { immediate: true })

    async function saveDenied(): Promise<void> {
      const view = sandbox.data.value
      if (view === null || deniedSaving.value) {
        return
      }
      const etag = view.network_denylist.etag
      if (etag === '') {
        deniedFeedback.value = { tone: 'warn', text: '服务端尚未装载禁止域名表，暂不能保存；请刷新页面后重试。' }
        return
      }
      const domains = deniedDraft.value.split('\n').map(line => line.trim()).filter(line => line !== '')
      deniedSaving.value = true
      deniedFeedback.value = null
      try {
        const updated = await admin.updateDeniedDomains(domains, etag)
        sandbox.data.value = updated
        deniedDirty.value = false
        syncDeniedDraft(updated)
        deniedFeedback.value = {
          tone: 'info',
          text: `已保存并立即生效：修订 ${updated.network_denylist.revision}，当前禁止 ${updated.denied_domains.length} 个域名。`,
        }
      } catch (error) {
        if (error instanceof ApiError && (error.status === 412 || error.status === 409)) {
          await sandbox.reload()
          const reread = sandbox.error.value === '' ? '页面已重新读取最新状态；' : '重新读取最新状态失败，请稍后再试；'
          deniedFeedback.value = {
            tone: 'warn',
            text: `${errorMessage(error)} ${reread}你的输入已保留，请核对后再保存。`,
          }
        } else {
          deniedFeedback.value = { tone: 'error', text: errorMessage(error) }
        }
      } finally {
        deniedSaving.value = false
      }
    }

    // —— 执行档位切换（变化方向随确认框冻结进请求）——
    const confirmTarget = ref<ExecutionProfile | null>(null)
    const expectedAtOpen = ref<ExecutionProfile | null>(null)
    const switching = ref(false)
    const profileFeedback = ref<Feedback>(null)

    function openProfileSwitch(target: ExecutionProfile): void {
      const view = policy.data.value
      if (view === null || view.profile === target) {
        return
      }
      // 确认框渲染时看到的档位，就是稍后写进 expected_profile 的方向基准。
      expectedAtOpen.value = view.profile
      confirmTarget.value = target
      profileFeedback.value = null
    }

    /**
     * 档位切换的确认文案：两个方向都只描述**真实**变化的授权面。
     * 标准档不等于“可以执行任意命令”——随包程序仍要过清单、真实路径与保护状态校验；
     * 严格档则把授权收缩到白名单、主机名例外与已登记的可执行件。
     * The confirmation copy names the real authorization delta in both directions and
     * never implies that the standard profile permits arbitrary commands.
     */
    function switchMessage(from: ExecutionProfile, to: ExecutionProfile): string {
      if (to === 'standard') {
        return '将允许访问未被禁止的公网域名，并自动授权满足校验条件的随包程序。内网访问仍需单独授权，文件访问范围和禁用命令规则保持不变。切换后，后续操作按新策略执行；已发出的请求无法撤回。'
      }
      return `从${PROFILE_LABELS[from]}模式切到严格模式会收紧授权范围：只有白名单域名、主机名访问例外与登记过的可执行件可以继续出网或执行，其余请求在执行前被拒绝。内网访问仍需单独授权，文件访问范围和禁用命令规则保持不变。切换后，后续操作按新策略执行；已发出的请求无法撤回。`
    }

    async function confirmSwitch(): Promise<void> {
      const target = confirmTarget.value
      const expected = expectedAtOpen.value
      const view = policy.data.value
      if (target === null || expected === null || view === null || switching.value) {
        return
      }
      if (view.etag === '') {
        confirmTarget.value = null
        profileFeedback.value = { tone: 'warn', text: '服务端尚未装载执行档位，暂不能切换；请刷新页面后重试。' }
        return
      }
      switching.value = true
      try {
        const updated = await admin.updateExecutionPolicy(target, expected, view.etag)
        policy.data.value = updated
        profileFeedback.value = {
          tone: 'info',
          text: `已切换到${PROFILE_LABELS[updated.profile]}模式，后续操作按新策略执行。`,
        }
      } catch (error) {
        // 412（陈旧 ETag）与 409（方向不一致 / 并发 CAS）都零写入：重新读取，绝不自动重发。
        if (error instanceof ApiError && (error.status === 412 || error.status === 409)) {
          await policy.reload()
          const reread = policy.error.value === '' ? '页面已重新读取当前档位；' : '重新读取当前档位失败，请稍后再试；'
          profileFeedback.value = {
            tone: 'warn',
            text: `${errorMessage(error)} ${reread}请核对方向后重新确认。`,
          }
        } else {
          profileFeedback.value = { tone: 'error', text: errorMessage(error) }
        }
      } finally {
        switching.value = false
        confirmTarget.value = null
        expectedAtOpen.value = null
        syncDraftFromSandbox()
      }
    }

    /** 档位行与 `GET /sandbox` 的投影是同一份状态：切换后重读，避免页面两处不一致。 */
    function syncDraftFromSandbox(): void {
      void sandbox.reload()
    }

    // —— 内网访问例外 ——
    const exceptionKind = ref<NetworkExceptionView['kind']>('ip_port')
    const exceptionValue = ref('')
    const exceptionNote = ref('')
    const addingException = ref(false)
    const exceptionFeedback = ref<Feedback>(null)
    const deleteTarget = ref<NetworkExceptionView | null>(null)
    const deleting = ref(false)

    async function reloadAll(): Promise<void> {
      await Promise.all([sandbox.reload(), policy.reload(), exceptions.reload()])
    }

    async function addException(): Promise<void> {
      if (addingException.value) {
        return
      }
      const value = exceptionValue.value.trim()
      const note = exceptionNote.value.trim()
      if (value === '' || note === '') {
        exceptionFeedback.value = { tone: 'warn', text: '地址与理由都必须填写：放行必须带理由。' }
        return
      }
      addingException.value = true
      exceptionFeedback.value = null
      try {
        const result = await admin.createNetworkException({ kind: exceptionKind.value, value, note })
        exceptionValue.value = ''
        exceptionNote.value = ''
        await reloadAll()
        exceptionFeedback.value = {
          tone: 'info',
          text: `已添加例外：${result.exception.value}（下一次授权判定即生效，在途连接不追溯撤回）。`,
        }
      } catch (error) {
        exceptionFeedback.value = { tone: 'error', text: errorMessage(error) }
      } finally {
        addingException.value = false
      }
    }

    async function confirmDelete(): Promise<void> {
      const target = deleteTarget.value
      const list = exceptions.data.value
      if (target === null || list === null || deleting.value) {
        return
      }
      if (list.etag === '') {
        deleteTarget.value = null
        exceptionFeedback.value = { tone: 'warn', text: '例外集合还没有可用的 ETag，暂不能删除；请刷新页面后重试。' }
        return
      }
      deleting.value = true
      try {
        await admin.deleteNetworkException(target.exception_id, list.etag)
        deleteTarget.value = null
        await reloadAll()
        exceptionFeedback.value = {
          tone: 'info',
          text: `已删除例外：${target.value}（下一次授权判定不再放行该地址）。`,
        }
      } catch (error) {
        // 404（已被别处删除）与 412（集合已变）都按"重新读取后由用户决定"处理。
        if (error instanceof ApiError && (error.status === 404 || error.status === 412 || error.status === 409)) {
          deleteTarget.value = null
          await reloadAll()
          const reread = exceptions.error.value === '' ? '页面已重新读取例外清单；' : '重新读取例外清单失败，请稍后再试；'
          exceptionFeedback.value = { tone: 'warn', text: `${errorMessage(error)} ${reread}` }
        } else {
          exceptionFeedback.value = { tone: 'error', text: errorMessage(error) }
        }
      } finally {
        deleting.value = false
      }
    }

    // —— 迁移待确认项：「加例外」/「保持拒绝」（未确认不放行）——
    const pendingNoteFor = ref('')
    const pendingNote = ref('')
    const pendingBusy = ref('')
    const pendingFeedback = ref<Feedback>(null)

    function openPendingException(item: PendingPolicyConfirmation): void {
      pendingNoteFor.value = item.id
      pendingNote.value = ''
      pendingFeedback.value = null
    }

    async function addPendingException(item: PendingPolicyConfirmation): Promise<void> {
      if (pendingBusy.value !== '') {
        return
      }
      const note = pendingNote.value.trim()
      if (note === '') {
        pendingFeedback.value = { tone: 'warn', text: '放行必须带理由：请填写这条例外的用途。' }
        return
      }
      pendingBusy.value = item.id
      try {
        await admin.createNetworkException({ kind: 'host', value: item.value, note })
        pendingNoteFor.value = ''
        pendingNote.value = ''
        await reloadAll()
        pendingFeedback.value = {
          tone: 'info',
          text: `已为 ${item.value} 添加主机名例外；该项已从待确认列表移除。`,
        }
      } catch (error) {
        pendingFeedback.value = { tone: 'error', text: errorMessage(error) }
      } finally {
        pendingBusy.value = ''
      }
    }

    async function keepDenied(item: PendingPolicyConfirmation): Promise<void> {
      if (pendingBusy.value !== '') {
        return
      }
      pendingBusy.value = item.id
      try {
        const result = await admin.confirmPolicyDenial(item.id)
        await policy.reload()
        pendingFeedback.value = {
          tone: 'info',
          text: result.already_recorded
            ? `此前已记录「保持拒绝」：${item.value}（操作 ${result.operation_id}）。`
            : `已记录「保持拒绝」：${item.value}（操作 ${result.operation_id}）。授权结论不变，该项不再列出。`,
        }
      } catch (error) {
        pendingFeedback.value = { tone: 'error', text: errorMessage(error) }
      } finally {
        pendingBusy.value = ''
      }
    }

    // —— 渲染 ——

    function renderSummary(policyView: ExecutionPolicyView, sandboxView: SandboxViewData): VNode {
      const profile = policyView.profile
      const switchLabel = profile === 'strict' ? '切换到标准…' : '切换到严格…'
      return section('执行概览', [
        definitions([
          ['执行方式', '本机进程（无容器隔离）'],
          ['执行策略', h('span', { class: 'profile-switch' }, [
            h(StatusBadge, { label: PROFILE_LABELS[profile], tone: PROFILE_BADGE_TONES[profile] }),
            profile === 'strict'
              ? '只放行白名单域名、主机名访问例外与登记过的可执行件。'
              : '允许访问未被禁止的公网域名；内网地址需访问例外，禁用命令与文件范围不变。',
            h('button', {
              type: 'button',
              id: 'profile-switch',
              disabled: switching.value || policyView.etag === '',
              onClick: () => {
                openProfileSwitch(profile === 'strict' ? 'standard' : 'strict')
              },
            }, switchLabel),
          ])],
          ['出网基础防护', 'DNS 解析校验 · 私有网段拒绝 · 重定向复查 · 云元数据地址永不放行（两档相同）'],
          ['网络授权', profile === 'strict'
            ? `只放行白名单域名与主机名访问例外（当前 ${sandboxView.http_allowed_domains.length} 个允许域名、${policyView.exceptions_count} 条例外）`
            : '公网访问允许（未命中禁止域名即可）；内网等受保护地址需访问例外'],
          ['文件范围', '各 Agent 按自身授权目录访问文件；完整清单见「高级设置」'],
        ]),
        profileFeedback.value ? h(Notice, { tone: profileFeedback.value.tone, text: profileFeedback.value.text }) : null,
      ])
    }

    /**
     * 严格模式且允许域名与访问例外都为空：受本页策略约束的请求会被全部拒绝。
     * 只提示并提供本页入口，不替用户放宽存量策略。
     * Strict with nothing allowed: say plainly that this page's requests are all
     * blocked, and link to the two entries that can fix it.
     */
    function renderStrictEmptyWarning(policyView: ExecutionPolicyView, sandboxView: SandboxViewData): VNode | null {
      if (policyView.profile !== 'strict' || sandboxView.http_allowed_domains.length > 0 || policyView.exceptions_count > 0) {
        return null
      }
      return h('div', { class: ['notice', 'notice-warn', 'strict-empty-warning'], role: 'status' }, [
        h('span', '严格模式下允许域名与访问例外都为空：受本页策略约束的请求（内置 HTTP 工具、web_search、HTTP MCP 连接）将被全部拒绝。'),
        h('a', { href: '#network-allowlist-editor' }, '配置允许域名'),
        h('span', ' 或 '),
        h('a', { href: '#exception-value' }, '添加访问例外'),
      ])
    }

    /**
     * 网络规则的真实适用范围：本页只管受控 HTTP / 搜索 / HTTP MCP 连接，
     * 不把“本机所有进程的所有网络访问”说成已受约束。
     * The real scope of this page's network rules; stdio children and model
     * connections are explicitly outside it.
     */
    function renderNetworkScope(): VNode {
      return section('网络规则的适用范围', [
        h('p', { class: 'muted small' }, '本页的禁止域名、执行档位、允许域名与访问例外只约束下列请求；它不接管本机所有进程的网络访问。'),
        definitions([
          ['内置 HTTP 工具（http_get / http_post）', '受本页约束：每次请求与每一次重定向都按禁止域名 → 端口 → 档位判定；拨号前解析目标，受保护地址段与云元数据地址拒绝。'],
          ['内置 web_search', '受本页约束：固定访问 api.deepseek.com/v1/tools/search，每次调用按同一条 HTTP 规则判定；严格模式下需把它列入允许域名。'],
          ['HTTP MCP（transport: http）', '连接与测试时按 server URL 判定一次；收紧域名规则后，不再被允许的连接会被断开。连接建立后的下游交互沿用该连接，不逐条复查域名。'],
          ['stdio MCP 子进程', '不受本页约束：子进程自行出网，本页既不代理也不拦截（诊断中以 stdio_child_egress_unconstrained 标注）。'],
          ['模型连接', '使用独立配置：模型请求不走本页的域名与档位判定。'],
          ['显式代理（http.proxy）', '启用后只校验并固定代理地址，目标域名由代理解析；域名规则仍按请求 URL 判定，但本机无法核验目标最终连到哪个地址。环境变量代理不被采用。'],
        ]),
      ])
    }

    /** 运行环境一行的三类独立事实与引用关系。 */
    interface RuntimeRow {
      key: string
      name: string
      detail: string
      exists: string
      existsTone: Tone
      available: string
      availableTone: Tone
      authorized: string
      authorizedTone: Tone
      agents: string
      server: string
    }

    /** 从启动命令认出随包解释器；只认可执行件基名，不按参数猜。 */
    function launcherOf(command: string): 'node' | 'python' | '' {
      const base = command.replace(/\\/g, '/').split('/').pop()?.toLowerCase() ?? ''
      if (base === 'node' || base === 'node.exe') {
        return 'node'
      }
      if (base === 'python' || base === 'python.exe' || base === 'python3' || base === 'python3.exe') {
        return 'python'
      }
      return ''
    }

    /**
     * 执行授权只按命令登记口径判断：`shell.allowed_commands` 或 `shell.interpreters`
     * 里有这个整串才算已登记；配置数组为空不代表组件没安装。
     * Authorization is the registered-command fact alone; an empty array never stands
     * in for "not installed".
     */
    function authorizationOf(command: string, view: SandboxViewData): { text: string; tone: Tone } {
      const cmd = command.trim()
      if (cmd === '') {
        return { text: '不适用', tone: 'muted' }
      }
      if (view.shell_allowed_commands.includes(cmd) || view.shell_interpreters.includes(cmd)) {
        return { text: '已登记', tone: 'ok' }
      }
      return { text: '未登记', tone: 'warn' }
    }

    function runtimeRows(
      servers: McpServerView[],
      diagnoses: Map<string, McpServerDiagnostics | null>,
      view: SandboxViewData,
    ): RuntimeRow[] {
      const rows: RuntimeRow[] = []
      const launchers = new Map<string, RuntimeRow>()
      for (const server of servers) {
        const diagnosis = diagnoses.get(server.name) ?? null
        const agents = diagnosis === null
          ? '未确认'
          : (diagnosis.references.agents.length === 0 ? '无' : diagnosis.references.agents.join('、'))
        // 存在性与版本只取依赖轴（os.Stat + 清单）；该轴 null 表示“不适用”，
        // 与“缺失”是两种事实。
        const dependency = diagnosis?.dependency ?? null
        let exists = '未确认'
        let existsTone: Tone = 'muted'
        let detail = '诊断读取失败，存在性未确认'
        if (dependency !== null) {
          detail = dependency.found_version !== '' && dependency.found_version !== dependency.expected_version
            ? `组件 ${dependency.component} · 期望 ${dependency.expected_version} · 实际 ${dependency.found_version}`
            : `组件 ${dependency.component} · 期望 ${dependency.expected_version}`
          if (dependency.state === 'ready') {
            exists = '已就位'
            existsTone = 'ok'
          } else if (dependency.state === 'version_mismatch') {
            exists = '版本不符'
            existsTone = 'warn'
          } else {
            exists = '缺失'
            existsTone = 'bad'
          }
        } else if (diagnosis !== null) {
          exists = '不适用'
          existsTone = 'muted'
          detail = '自定义命令：没有登记的运行时组件'
        }
        const authorization = authorizationOf(server.command, view)
        rows.push({
          key: `server:${server.name}`,
          name: server.name,
          detail,
          exists,
          existsTone,
          available: server.connected ? '已连接' : (server.pending_restart ? '待重启后尝试' : '未连接'),
          availableTone: server.connected ? 'ok' : 'warn',
          authorized: authorization.text,
          authorizedTone: authorization.tone,
          agents,
          server: server.name,
        })

        // 解释器组件（Node.js / Python）单列一行：它是 server 的启动命令，
        // 存在性看命令三态，可用性看真实连接，授权看命令登记。
        const launcher = launcherOf(server.command)
        if (launcher === '') {
          continue
        }
        const commandState = diagnosis?.command?.state ?? ''
        const launcherExists = commandState === 'located' ? '已定位' : (commandState === 'not_found' ? '缺失' : '未确认')
        const launcherTone: Tone = commandState === 'located' ? 'ok' : (commandState === 'not_found' ? 'bad' : 'muted')
        const existing = launchers.get(launcher)
        if (existing !== undefined) {
          if (server.connected && existing.availableTone !== 'ok') {
            existing.available = `已通过 ${server.name} 启动`
            existing.availableTone = 'ok'
          }
          continue
        }
        launchers.set(launcher, {
          key: `runtime:${launcher}`,
          name: RUNTIME_COMPONENT_LABELS[launcher] ?? launcher,
          detail: `启动命令 ${server.command}（${server.name}）`,
          exists: launcherExists,
          existsTone: launcherTone,
          available: server.connected ? `已通过 ${server.name} 启动` : '未连接',
          availableTone: server.connected ? 'ok' : 'warn',
          authorized: authorization.text,
          authorizedTone: authorization.tone,
          agents,
          server: server.name,
        })
      }
      return [...rows, ...launchers.values()]
    }

    /** 缺件 / 未登记会阻断哪些 Agent，以及系统实际怎么补（不在运行时下载组件）。 */
    function runtimeBlockers(rows: RuntimeRow[]): string[] {
      const notes: string[] = []
      for (const row of rows) {
        if (row.exists === '缺失' || row.exists === '版本不符') {
          const affected = row.agents === '无' || row.agents === '未确认'
            ? '当前没有已登记的 Agent 引用它'
            : `引用它的 Agent（${row.agents}）会因该 server 无法连接而整只不注册`
          notes.push(`${row.name}：${row.detail}；${affected}。组件随完整发布包提供，Basalt 不会在运行时下载或安装：请解压完整发布包后重启，再回本页核对；若该 MCP 条目本身也缺失，在「MCP 目录」按配方添加。`)
        }
        if (row.authorized === '未登记') {
          notes.push(`${row.name}：stdio 启动命令未登记执行授权，server 会在启动前被拒绝。把该命令加入 basalt.yaml 的 shell.allowed_commands（解释器命令也可登记在 shell.interpreters）后重启；shell 配置不热加载。`)
        }
      }
      return notes
    }

    function renderRuntime(view: SandboxViewData): VNode {
      const listing = runtime.data.value
      const rows = listing === null ? [] : runtimeRows(listing.servers, listing.diagnoses, view)
      const blockers = runtimeBlockers(rows)
      return section('运行环境', [
        h('p', { class: 'muted small' }, '随包组件是否就位、能否实际连接、stdio 启动命令是否已登记执行授权。三列分别来自运行时目录检查、真实连接与配置登记，互不代替；这里只列出已配置 MCP 服务引用到的组件。'),
        runtime.error.value ? h(Notice, { tone: 'error', text: runtime.error.value }) : null,
        runtime.loading.value ? h('p', { class: 'loading' }, '正在读取运行环境…') : null,
        listing === null ? null : h(DataTable, {
          tableClass: 'runtime-table',
          headers: ['组件', '存在', '可用', '执行授权', '引用 Agent', ''],
          columnClasses: ['col-runtime', '', '', '', '', 'col-action'],
          rows: rows.map(row => ({
            key: row.key,
            cells: [
              h('div', { class: 'runtime-component' }, [
                h('strong', row.name),
                h('span', { class: 'muted small', title: row.detail }, row.detail),
              ]),
              h(StatusBadge, { label: row.exists, tone: row.existsTone }),
              h(StatusBadge, { label: row.available, tone: row.availableTone }),
              h(StatusBadge, { label: row.authorized, tone: row.authorizedTone }),
              row.agents,
              h('a', { href: resourceDetailHref('mcp-servers', row.server) }, '查看依赖'),
            ],
          })),
          empty: '当前没有已发布的 MCP 服务，无法从连接与诊断确认组件状态；未配置服务不代表组件未安装。',
        }),
        blockers.length === 0 ? null : h('div', { class: 'runtime-blockers' }, [
          h('h3', '会阻断内置 Agent 的问题'),
          h('ul', { class: 'plain-list' }, blockers.map(text => h('li', { key: text }, text))),
        ]),
      ])
    }

    function renderPendingGroup(items: PendingPolicyConfirmation[], axis: PendingPolicyConfirmation['axis']): VNode | null {
      if (items.length === 0) {
        return null
      }
      return h('div', { class: 'pending-group' }, [
        h('h3', PENDING_AXIS_LABELS[axis]),
        h('p', { class: 'muted small' }, axis === 'network_allowlist'
          ? '这些域名解析到受保护地址段。默认拒绝；逐条选择「加例外」或「保持拒绝」，未确认不放行。'
          : '这些条目命中交互式 shell 名称，任何档位都不会放行；「保持拒绝」只记录处置，不改变结论。'),
        ...items.map(item => h('div', { class: 'pending-item', key: item.id }, [
          h('div', { class: 'pending-head' }, [
            h('code', { class: 'cell-id', title: item.value }, item.value),
            h('span', { class: 'muted small' }, pendingReasonLabel(item)),
          ]),
          h('div', { class: 'form-actions' }, [
            axis === 'network_allowlist'
              ? h('button', {
                type: 'button',
                disabled: pendingBusy.value !== '',
                onClick: () => {
                  openPendingException(item)
                },
              }, '加例外（放行）')
              : null,
            h('button', {
              type: 'button',
              disabled: pendingBusy.value !== '',
              onClick: () => {
                void keepDenied(item)
              },
            }, pendingBusy.value === item.id ? '正在记录…' : '保持拒绝'),
          ]),
          pendingNoteFor.value === item.id
            ? h('div', { class: 'pending-exception-form' }, [
              h('label', { class: 'field', for: `pending-note-${item.id}` }, [
                h('span', '放行理由（必填，进审计）'),
                h('input', {
                  id: `pending-note-${item.id}`,
                  type: 'text',
                  value: pendingNote.value,
                  maxlength: 500,
                  onInput: (event: Event) => {
                    pendingNote.value = (event.target as HTMLInputElement).value
                  },
                }),
              ]),
              h('div', { class: 'form-actions' }, [
                h('button', {
                  type: 'button',
                  class: 'primary',
                  id: 'pending-exception-confirm',
                  disabled: pendingBusy.value !== '' || pendingNote.value.trim() === '',
                  onClick: () => {
                    void addPendingException(item)
                  },
                }, '确认加例外'),
                h('button', {
                  type: 'button',
                  onClick: () => {
                    pendingNoteFor.value = ''
                    pendingNote.value = ''
                  },
                }, '取消'),
              ]),
            ])
            : null,
        ])),
      ])
    }

    function renderPending(view: ExecutionPolicyView): VNode | null {
      const items = view.pending_confirmations
      if (items.length === 0) {
        return null
      }
      return section('待确认项', [
        h('p', { class: 'muted small' }, '升级迁移的只读扫描结果：受保护地址段与 shell 名称默认拒绝，管理员逐条看过并处置后才从列表移除。这里的记录不改变授权结论。'),
        renderPendingGroup(items.filter(item => item.axis === 'network_allowlist'), 'network_allowlist'),
        renderPendingGroup(items.filter(item => item.axis === 'shell_command'), 'shell_command'),
      ])
    }

    function renderExceptions(view: ExecutionPolicyView): VNode {
      const list = exceptions.data.value
      const rows = (list?.exceptions ?? []).map(exception => ({
        key: exception.exception_id,
        cells: [
          h('code', { class: 'cell-id', title: exception.value }, exception.value),
          EXCEPTION_KIND_LABELS[exception.kind],
          exception.note || '—',
          h('button', {
            type: 'button',
            disabled: deleting.value,
            onClick: () => {
              deleteTarget.value = exception
            },
          }, '删除'),
        ],
      }))
      return section('内网访问例外', [
        h('p', { class: 'muted small' }, `只为受本页策略约束的请求放行受保护地址段上的目标，例如内部 HTTP 服务 10.20.0.8:8080。严格模式下 host 例外等价于一条白名单条目。云元数据地址任何档位都不放行，也不能登记为例外。当前生效 ${view.exceptions_count} 条。`),
        h('p', { class: 'muted small' }, '这条例外不等于给 stdio MCP 子进程统一配置网络权限：子进程自行出网，不受本页约束。'),
        exceptions.error.value ? h(Notice, { tone: 'error', text: exceptions.error.value }) : null,
        list === null ? h('p', { class: 'loading' }, '正在读取例外清单…') : h(DataTable, {
          headers: ['地址', '类型', '理由', '操作'],
          rows,
          empty: '还没有内网访问例外。',
          columnClasses: ['col-exception-value', 'col-exception-kind', '', 'col-actions'],
        }),
        h('div', { class: 'exception-form' }, [
          h('label', { class: 'field', for: 'exception-kind' }, [
            h('span', '类型'),
            h('select', {
              id: 'exception-kind',
              value: exceptionKind.value,
              disabled: addingException.value,
              onChange: (event: Event) => {
                exceptionKind.value = (event.target as HTMLSelectElement).value as NetworkExceptionView['kind']
              },
            }, [
              h('option', { value: 'ip_port' }, EXCEPTION_KIND_LABELS.ip_port),
              h('option', { value: 'host' }, EXCEPTION_KIND_LABELS.host),
            ]),
          ]),
          h('label', { class: 'field', for: 'exception-value' }, [
            h('span', exceptionKind.value === 'host' ? '主机名（可 *.example.internal）' : 'IP:端口'),
            h('input', {
              id: 'exception-value',
              type: 'text',
              spellcheck: false,
              value: exceptionValue.value,
              placeholder: exceptionKind.value === 'host' ? 'report.internal.example' : '10.20.0.8:8080',
              disabled: addingException.value,
              onInput: (event: Event) => {
                exceptionValue.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('label', { class: 'field', for: 'exception-note' }, [
            h('span', '理由（必填，进审计）'),
            h('input', {
              id: 'exception-note',
              type: 'text',
              maxlength: 500,
              value: exceptionNote.value,
              disabled: addingException.value,
              onInput: (event: Event) => {
                exceptionNote.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('div', { class: 'form-actions' }, [
            h('button', {
              type: 'button',
              id: 'exception-add',
              disabled: addingException.value || exceptionValue.value.trim() === '' || exceptionNote.value.trim() === '',
              onClick: () => {
                void addException()
              },
            }, addingException.value ? '正在添加…' : '添加例外'),
          ]),
        ]),
        exceptionFeedback.value ? h(Notice, { tone: exceptionFeedback.value.tone, text: exceptionFeedback.value.text }) : null,
        h(ConfirmDialog, {
          open: deleteTarget.value !== null,
          title: '删除内网访问例外',
          message: deleteTarget.value === null
            ? ''
            : `删除后下一次授权判定不再放行 ${deleteTarget.value.value}（理由：${deleteTarget.value.note}）。在途连接与已发出的请求不追溯撤回。`,
          confirmText: deleting.value ? '正在删除…' : '删除例外',
          busy: deleting.value,
          onConfirm: () => {
            void confirmDelete()
          },
          onCancel: () => {
            deleteTarget.value = null
          },
        }),
      ])
    }

    function renderDeniedDomains(view: SandboxViewData): VNode {
      const live = view.network_denylist
      return section('禁止域名', [
        h('p', { class: 'muted small' }, '两档都先生效：命中即拒，不再看白名单或档位。语法与白名单相同（支持 *.example.com，只匹配子域）。保存后立即生效；已发出的请求无法追溯撤回。'),
        definitions([
          ['实际生效', `${view.denied_domains.length} 个禁止域名 · 修订 ${live.revision}`],
          ['来源', policySourceLabel(live.source)],
          ['最近保存', `${formatTimestamp(live.updated_at)}${live.updated_by === '' ? '' : ` · 写入者 ${live.updated_by}`}`],
        ]),
        h('label', { class: 'field', for: 'denied-domains-editor' }, [
          h('span', '禁止域名（每行一个）'),
          h('textarea', {
            id: 'denied-domains-editor',
            class: 'editor',
            rows: 4,
            spellcheck: false,
            value: deniedDraft.value,
            placeholder: '*.internal.example.com',
            disabled: deniedSaving.value,
            onInput: (event: Event) => {
              deniedDraft.value = (event.target as HTMLTextAreaElement).value
              deniedDirty.value = true
              deniedFeedback.value = null
            },
          }),
        ]),
        h('p', { class: 'muted small' }, '保存时全量替换当前列表；空列表表示没有禁止条目。'),
        deniedFeedback.value ? h(Notice, { tone: deniedFeedback.value.tone, text: deniedFeedback.value.text }) : null,
        h('div', { class: 'form-actions' }, [
          h('button', {
            type: 'button',
            class: 'primary',
            id: 'denied-domains-save',
            disabled: deniedSaving.value || live.etag === '',
            onClick: () => {
              void saveDenied()
            },
          }, deniedSaving.value ? '正在保存…' : '保存禁止域名'),
          h('button', {
            type: 'button',
            disabled: deniedSaving.value || !deniedDirty.value,
            onClick: () => {
              deniedDraft.value = view.denied_domains.join('\n')
              deniedDirty.value = false
              deniedFeedback.value = null
            },
          }, '撤销未保存的修改'),
        ]),
        h('details', { class: 'technical-details denylist-details' }, [
          h('summary', '技术细节'),
          definitions([
            ['校验标识（ETag）', h('code', live.etag === '' ? '尚未装载' : live.etag)],
            ['修订号', String(live.revision)],
            ['来源标识', live.source],
          ]),
        ]),
      ])
    }

    /**
     * 白名单内容：严格模式下单独成节并排在前面；标准模式下不限制出网，
     * 因此嵌进高级设置，避免让人误以为先要配白名单。
     * Allowlist body; strict shows it as a top-level section, standard nests it
     * under advanced settings.
     */
    function renderAllowlistBody(view: SandboxViewData): VNodeChild[] {
      const live = view.network_allowlist
      const strict = view.execution_profile === 'strict'
      return [
        h('p', { class: 'muted small' }, strict
          ? '严格模式下，只有这里的域名与 host 类型的访问例外可以出网；保存后立即生效，不需要重启。'
          : '标准模式下这里不限制出网（禁止域名优先）；切到严格模式后，只有这里的域名与 host 类型的访问例外可以出网。保存后立即生效，不需要重启。'),
        h('p', { class: 'muted small' }, 'basalt.yaml 的 http.allowed_domains 只在数据库还没有策略行时作为首次种子，此后改配置不会覆盖这里保存的值。已发出的请求无法追溯撤回；在途 Run 之后的网络请求按新规则判定。'),
        definitions([
          ['实际生效', `${view.http_allowed_domains.length} 个允许域名 · 修订 ${live.revision}`],
          ['来源', policySourceLabel(live.source)],
          ['最近保存', `${formatTimestamp(live.updated_at)}${live.updated_by === '' ? '' : ` · 写入者 ${live.updated_by}`}`],
        ]),
        h('label', { class: 'field', for: 'network-allowlist-editor' }, [
          h('span', '允许域名（每行一个）'),
          h('textarea', {
            id: 'network-allowlist-editor',
            class: 'editor',
            rows: 6,
            spellcheck: false,
            value: draft.value,
            placeholder: 'api.example.com\n*.internal.example.com',
            disabled: saving.value,
            onInput: (event: Event) => {
              draft.value = (event.target as HTMLTextAreaElement).value
              dirty.value = true
              feedback.value = null
            },
          }),
        ]),
        h('p', { class: 'muted small' }, '保存时全量替换当前列表。支持 *.example.com 形式，只匹配子域、不匹配 example.com 本身。域名不带协议、端口或路径。'),
        view.http_allowed_domains.length === 0
          ? h('p', { class: 'muted allowlist-empty' }, strict ? EMPTY_ALLOWLIST_COPY : EMPTY_ALLOWLIST_COPY_STANDARD)
          : null,
        feedback.value ? h(Notice, { tone: feedback.value.tone, text: feedback.value.text }) : null,
        h('div', { class: 'form-actions' }, [
          h('button', {
            type: 'button',
            class: 'primary',
            id: 'network-allowlist-save',
            disabled: saving.value || live.etag === '',
            onClick: () => {
              void save()
            },
          }, saving.value ? '正在保存…' : '保存白名单'),
          h('button', {
            type: 'button',
            disabled: saving.value || !dirty.value,
            onClick: () => {
              draft.value = view.http_allowed_domains.join('\n')
              dirty.value = false
              feedback.value = null
            },
          }, '撤销未保存的修改'),
        ]),
        h('details', { class: 'technical-details allowlist-details' }, [
          h('summary', '技术细节'),
          definitions([
            ['配置校验标识（ETag）', h('code', live.etag === '' ? '尚未装载' : live.etag)],
            ['修订号', String(live.revision)],
            ['来源标识', live.source],
          ]),
        ]),
      ]
    }

    /** 严格模式下白名单是主入口；标准模式下由高级设置承载。 */
    function renderAllowlist(view: SandboxViewData): VNode {
      return section('网络域名白名单', renderAllowlistBody(view))
    }

    function renderAdvanced(view: SandboxViewData, policyView: ExecutionPolicyView, allowlist: VNode | null): VNode {
      return h('details', { class: 'technical-details advanced-settings' }, [
        h('summary', '高级设置'),
        allowlist,
        h('p', { class: 'muted small advanced-note' }, '新实例默认标准模式；从旧版本升级的实例保持严格模式，需要在本页确认后才放宽。'),
        h('div', { class: 'policy-grid' }, [
          ['文件访问路径', view.file_allowed_paths],
          ['允许的命令', view.shell_allowed_commands],
          ['受控解释器', view.shell_interpreters],
          ['受保护地址段', policyView.protected_addresses],
          ['交互式 shell 名称', policyView.shell_names],
        ].map(([title, values]) => section(title as string, [
          (values as string[]).length
            ? h('ul', { class: 'plain-list' }, (values as string[]).map(value => h('li', { key: value }, h('code', value))))
            : h('p', { class: 'muted' }, '未配置额外允许项'),
        ]))),
        view.file_allowed_paths.some(path => path.includes('<agent>'))
          ? h('p', { class: 'muted small path-note' }, '文件路径中的 <agent> 是目录模板，实际执行时按当前 Agent 名替换。')
          : null,
        definitions([
          ['档位校验标识（ETag）', h('code', policyView.etag === '' ? '尚未装载' : policyView.etag)],
          ['档位修订号', String(policyView.revision)],
          ['档位来源', policySourceLabel(policyView.source)],
        ]),
        h('p', { class: 'muted small' }, '文件、命令与解释器属于实例级配置，修改 basalt.yaml 后需重启生效。档位、禁止域名与访问例外在本页保存即生效；本页不能放开文件与命令边界。'),
      ])
    }

    return () => {
      const data = sandbox.data.value
      const policyView = policy.data.value
      const switchFrom = expectedAtOpen.value
      const switchTo = confirmTarget.value
      const strict = policyView?.profile === 'strict'
      // 标准模式：概览 → 禁止域名 → 内网访问例外 → 运行环境 → 规则适用范围；
      // 白名单收进高级设置。严格模式：白名单是主入口，紧随概览单独成节。
      const allowlist = data === null ? null : renderAllowlist(data)
      return page('执行环境', '查看并管理当前实例的文件、命令与网络访问边界。', [
        sandbox.error.value ? h(Notice, { tone: 'error', text: sandbox.error.value }) : null,
        policy.error.value ? h(Notice, { tone: 'error', text: policy.error.value }) : null,
        sandbox.loading.value ? h('p', { class: 'loading' }, '正在读取执行策略…') : null,
        data === null || policyView === null ? null : renderSummary(policyView, data),
        data === null || policyView === null ? null : renderStrictEmptyWarning(policyView, data),
        strict ? allowlist : null,
        data === null ? null : renderDeniedDomains(data),
        policyView === null ? null : renderExceptions(policyView),
        data === null ? null : renderRuntime(data),
        data === null ? null : renderNetworkScope(),
        policyView === null ? null : renderPending(policyView),
        // 处置反馈放在卡片外：最后一条被处置后卡片会消失，反馈不能跟着消失。
        pendingFeedback.value ? h(Notice, { tone: pendingFeedback.value.tone, text: pendingFeedback.value.text }) : null,
        data === null || policyView === null ? null : renderAdvanced(data, policyView, strict ? null : allowlist),
        h(ConfirmDialog, {
          open: switchTo !== null,
          title: switchTo === null ? '' : `切换到${PROFILE_LABELS[switchTo]}模式？`,
          message: switchFrom === null || switchTo === null ? '' : switchMessage(switchFrom, switchTo),
          confirmText: switching.value ? '正在切换…' : (switchTo === null ? '' : `切换为${PROFILE_LABELS[switchTo]}模式`),
          busy: switching.value,
          onConfirm: () => {
            void confirmSwitch()
          },
          onCancel: () => {
            confirmTarget.value = null
            expectedAtOpen.value = null
          },
        }),
      ])
    }
  },
})
