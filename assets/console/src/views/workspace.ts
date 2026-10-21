// 已有只读 API 的页面入口；资源配置写入仍由相应 US 的受控写面交付。
// Navigation views over existing read APIs; no additional configuration write path.
import { defineComponent, h } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { CodeBlock, DataTable, Notice } from '../components/ui'
import { capabilityTabs, identifier, pageHeading, section, useLoader, viewProps } from './shared'

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
    return () => page('全局记忆', '工作区共享的长期记忆。Agent 自己的记忆在对应的 Agent 详情中查看。', [
      memory.error.value ? h(Notice, { tone: 'error', text: memory.error.value }) : null,
      memory.loading.value ? h('p', { class: 'loading' }, '正在读取全局记忆…') : null,
      memory.data.value === null ? null : h('div', { class: 'memory-columns' }, [
        section('核心记忆', [memory.data.value.core ? h(CodeBlock, { text: memory.data.value.core }) : h('p', { class: 'empty' }, '还没有全局核心记忆')]),
        section('归档记忆', [memory.data.value.archive ? h(CodeBlock, { text: memory.data.value.archive }) : h('p', { class: 'empty' }, '还没有归档内容')]),
      ]),
      h('p', { class: 'muted small' }, '这里为只读视图；记忆写入由现有记忆工具完成。'),
    ])
  },
})

export const NotificationsView = defineComponent({
  name: 'NotificationsView', props: viewProps,
  setup(props) {
    const channels = useLoader(() => props.ctx.api.listNotifyChannels())
    return () => page('消息渠道', '查看 Agent 发送结果通知时可使用的渠道。', [
      channels.error.value ? h(Notice, { tone: 'error', text: channels.error.value }) : null,
      channels.loading.value ? h('p', { class: 'loading' }, '正在读取通知渠道…') : null,
      channels.data.value === null ? null : h(DataTable, { headers: ['渠道', '类型', '说明', '目标服务'], rows: channels.data.value.map(channel => ({ key: channel.name, cells: [channel.name, channel.type, channel.description || '—', channel.url_host] })), empty: '还没有配置通知渠道' }),
      h('p', { class: 'muted small' }, '目标仅展示服务地址，不显示凭据或完整 webhook。'),
    ])
  },
})

export const SandboxView = defineComponent({
  name: 'SandboxView', props: viewProps,
  setup(props) {
    const sandbox = useLoader(() => props.ctx.api.getSandbox())
    return () => {
      const data = sandbox.data.value
      return page('执行环境', '查看当前实例的文件、命令与网络访问边界。', [
        sandbox.error.value ? h(Notice, { tone: 'error', text: sandbox.error.value }) : null,
        sandbox.loading.value ? h('p', { class: 'loading' }, '正在读取执行策略…') : null,
        data === null ? null : h('div', { class: 'policy-grid' }, [
          ['文件访问路径', data.file_allowed_paths], ['允许的命令', data.shell_allowed_commands],
          ['受控解释器', data.shell_interpreters], ['允许的网络域名', data.http_allowed_domains],
        ].map(([title, values]) => section(title as string, [(values as string[]).length ? h('ul', { class: 'plain-list' }, (values as string[]).map(value => h('li', { key: value }, h('code', value)))) : h('p', { class: 'muted' }, '未配置额外允许项')]))),
        data?.file_allowed_paths.some(path => path.includes('<agent>')) ? h('p', { class: 'muted small path-note' }, '文件路径中的 <agent> 是目录模板，实际执行时按当前 Agent 名替换。') : null,
        h('p', { class: 'muted small' }, '策略为实例级配置，修改配置后需重启生效。此页不改变任何执行权限。'),
      ])
    }
  },
})
