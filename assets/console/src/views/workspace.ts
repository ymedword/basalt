// 只读页与执行环境的白名单编辑入口（R8 裁决 2：保存即热生效，不重启）。
// Read-only views plus the live network-allowlist editor on the execution page.
import { defineComponent, h, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type { SandboxView as SandboxViewData } from '../api'
import { adminApi } from '../api-admin'
import { NotifyChannels } from '../components/notify-channels'
import { CodeBlock, DataTable, Notice, formatTimestamp } from '../components/ui'
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
const EMPTY_ALLOWLIST_COPY = '未配置允许域名，受此策略约束的出站请求将被拒绝。模型连接使用独立配置。'

/** 白名单来源的展示文案：种子只发生一次，之后以管理台保存为准。 */
function allowlistSourceLabel(source: SandboxViewData['network_allowlist']['source']): string {
  return source === 'config_seed' ? '配置种子（首次启动写入）' : '管理台保存'
}

export const SandboxView = defineComponent({
  name: 'SandboxView', props: viewProps,
  setup(props) {
    const sandbox = useLoader(() => props.ctx.api.getSandbox())
    const admin = adminApi(props.ctx.api)
    const draft = ref('')
    const dirty = ref(false)
    const saving = ref(false)
    const feedback = ref<{ tone: NoticeTone; text: string } | null>(null)

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

    function renderAllowlist(view: SandboxViewData): VNode {
      const live = view.network_allowlist
      return section('网络域名白名单', [
        h('p', { class: 'muted small' }, '控制沙箱内 HTTP 工具与 MCP http 连接可以访问的域名。保存后立即生效，不需要重启。basalt.yaml 的 http.allowed_domains 只在数据库还没有策略行时作为首次种子，此后改配置不会覆盖这里保存的值。已发出的请求无法追溯撤回；在途 Run 之后的网络请求按新规则判定。模型连接的访问范围由模型连接配置决定，不受这里影响。'),
        definitions([
          ['实际生效', `${view.http_allowed_domains.length} 个允许域名 · 修订 ${live.revision}`],
          ['来源', allowlistSourceLabel(live.source)],
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
        h('p', { class: 'muted small' }, '保存时全量替换当前列表；空列表表示拒绝全部出站请求。支持 *.example.com 形式，只匹配子域、不匹配 example.com 本身。域名不带协议、端口或路径。'),
        view.http_allowed_domains.length === 0 ? h('p', { class: 'muted allowlist-empty' }, EMPTY_ALLOWLIST_COPY) : null,
        feedback.value ? h(Notice, { tone: feedback.value.tone, text: feedback.value.text }) : null,
        h('div', { class: 'form-actions' }, [
          h('button', {
            type: 'button',
            class: 'primary',
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
        h('details', { class: 'technical-details' }, [
          h('summary', '技术细节'),
          definitions([
            ['配置校验标识（ETag）', h('code', live.etag === '' ? '尚未装载' : live.etag)],
            ['修订号', String(live.revision)],
            ['来源标识', live.source],
          ]),
        ]),
      ])
    }

    return () => {
      const data = sandbox.data.value
      return page('执行环境', '查看并管理当前实例的文件、命令与网络访问边界。', [
        sandbox.error.value ? h(Notice, { tone: 'error', text: sandbox.error.value }) : null,
        sandbox.loading.value ? h('p', { class: 'loading' }, '正在读取执行策略…') : null,
        data === null ? null : h('div', { class: 'policy-grid' }, [
          ['文件访问路径', data.file_allowed_paths], ['允许的命令', data.shell_allowed_commands],
          ['受控解释器', data.shell_interpreters],
        ].map(([title, values]) => section(title as string, [(values as string[]).length ? h('ul', { class: 'plain-list' }, (values as string[]).map(value => h('li', { key: value }, h('code', value)))) : h('p', { class: 'muted' }, '未配置额外允许项')]))),
        data?.file_allowed_paths.some(path => path.includes('<agent>')) ? h('p', { class: 'muted small path-note' }, '文件路径中的 <agent> 是目录模板，实际执行时按当前 Agent 名替换。') : null,
        data === null ? null : renderAllowlist(data),
        h('p', { class: 'muted small' }, '文件、命令与解释器属于实例级配置，修改 basalt.yaml 后需重启生效。网络域名白名单在本页保存即生效；本页不能放开文件与命令边界。'),
      ])
    }
  },
})
