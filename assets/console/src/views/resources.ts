// 只读资源视图：skills / mcp-servers / model-connections 三类（FR18）。
// 模型连接只展示环境变量引用名，MCP 只展示变量键名，绝不回显凭据值。
// Read-only resources. Model connections show the environment-variable reference
// only; MCP entries show key names only. Credential values never reach the page.
import { defineComponent, h } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type { McpServerView, ModelConnectionView, SkillView } from '../api'
import { DataTable, Notice, shortHash } from '../components/ui'
import type { TableRow } from '../components/ui'
import type { ResourceKind } from '../router'
import { badge, capabilityTabs, identifier, pageHeading, useLoader, viewProps } from './shared'

const KIND_LABELS: Record<ResourceKind, string> = {
  skills: '技能',
  'mcp-servers': 'MCP 服务',
  'model-connections': '模型与连接',
}

const KIND_NOTES: Record<ResourceKind, string> = {
  skills: '工作区已发布的技能。Agent 通过配置引用，运行使用准入时固定的版本。',
  'mcp-servers': '查看 MCP 服务的连接状态、工具与已应用版本。这里只展示环境变量键名。',
  'model-connections': '查看模型服务的地址、默认模型与引用关系。配置存在不代表服务已经探测可用。',
}

type ResourceRow = SkillView | McpServerView | ModelConnectionView

interface TableProps {
  headers: string[]
  rows: TableRow[]
  empty: string
  columnClasses?: string[]
  tableClass?: string
}

export const ResourcesView = defineComponent({
  name: 'ResourcesView',
  props: viewProps,
  setup(props) {
    const api = props.ctx.api
    const kind = (props.params.kind ?? 'skills') as ResourceKind

    const listing = useLoader<ResourceRow[]>(() => {
      switch (kind) {
        case 'skills':
          return api.listSkills()
        case 'mcp-servers':
          return api.listMcpServers()
        case 'model-connections':
          return api.listModelConnections()
      }
    })

    function skillRows(data: SkillView[]): TableProps {
      return {
        headers: ['技能', '描述', '版本', '文件数', '被引用', '内容哈希'],
        tableClass: 'skills-table',
        columnClasses: ['col-name', '', 'col-number', 'col-number', 'col-name', 'col-hash'],
        rows: data.map((item) => ({
          key: item.name,
          cells: [
            identifier(item.name),
            item.description === '' ? '—' : item.description,
            String(item.version_no),
            String(item.file_count),
            item.referenced_by.length === 0 ? '无' : item.referenced_by.join('、'),
            h('code', { title: item.content_hash }, shortHash(item.content_hash)),
          ],
        })),
        empty: '还没有已发布的技能',
      }
    }

    function mcpRows(data: McpServerView[]): TableProps {
      return {
        headers: ['名称', '传输', '可执行名', '环境变量键', '连接', '工具', '发布版本', '已应用', '待重启'],
        tableClass: 'mcp-table',
        columnClasses: ['col-name', 'col-short', 'col-name', 'col-keys', 'col-state', 'col-keys', 'col-number', 'col-number', 'col-state'],
        rows: data.map((item) => ({
          key: item.name,
          cells: [
            identifier(item.name),
            item.transport,
            item.command,
            item.env_keys.length === 0 ? '无' : item.env_keys.join('、'),
            item.connected ? '已连接' : '未连接',
            item.tools.length === 0 ? '无' : item.tools.join('、'),
            String(item.published_version_no),
            String(item.applied_version_no),
            item.pending_restart ? badge('需重启', 'warn') : '否',
          ],
        })),
        empty: '还没有 MCP 服务定义',
      }
    }

    function modelRows(data: ModelConnectionView[]): TableProps {
      return {
        headers: ['名称', 'Base URL', '凭据引用', '默认模型', '发布版本', '已应用', '被引用'],
        tableClass: 'models-table',
        columnClasses: ['col-name', 'col-target', 'col-keys', 'col-name', 'col-number', 'col-number', 'col-name'],
        rows: data.map((item) => ({
          key: item.name,
          cells: [
            identifier(item.name),
            identifier(item.base_url),
            item.api_key_env === '' ? '未配置' : identifier(item.api_key_env),
            identifier(item.default_model),
            String(item.published_version_no),
            String(item.applied_version_no),
            item.referenced_by.length === 0 ? '无' : item.referenced_by.join('、'),
          ],
        })),
        empty: '还没有模型连接定义',
      }
    }

    function renderTable(): VNodeChild {
      const data = listing.data.value
      if (data === null) {
        return null
      }
      if (kind === 'skills') {
        return h(DataTable, skillRows(data as SkillView[]))
      }
      if (kind === 'mcp-servers') {
        return h(DataTable, mcpRows(data as McpServerView[]))
      }
      return h(DataTable, modelRows(data as ModelConnectionView[]))
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        kind === 'mcp-servers' ? capabilityTabs('mcp') : null,
      ]

      if (listing.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: listing.error.value }))
      }
      if (listing.loading.value && listing.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      content.push(renderTable())

      return h('section', { class: 'view view-resources' }, [
        pageHeading('RESOURCES / ' + (kind === 'skills' ? '05' : kind === 'mcp-servers' ? '04' : '03'), kind === 'mcp-servers' ? '工具与 MCP' : KIND_LABELS[kind], KIND_NOTES[kind]),
        ...content,
        h('p', { class: 'muted small resource-note' }, '此页为只读目录，不显示凭据值。'),
      ])
    }
  },
})
