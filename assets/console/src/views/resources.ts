// 只读资源视图：skills / mcp-servers / model-connections 三类（FR18）。
// 模型连接只展示环境变量引用名，MCP 只展示变量键名，绝不回显凭据值。
// Read-only resources. Model connections show the environment-variable reference
// only; MCP entries show key names only. Credential values never reach the page.
import { defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type { McpServerView, ModelConnectionView, SkillView } from '../api'
import { ResourceForm } from '../components/resource-form'
import { DataTable, Notice, shortHash } from '../components/ui'
import type { TableRow } from '../components/ui'
import type { ResourceKind } from '../router'
import { mcpCatalogHref, resourceDetailHref } from '../router'
import {
  AVAILABILITY_LABELS,
  availabilityTone,
  badge,
  capabilityTabs,
  identifier,
  pageHeading,
  useLoader,
  viewProps,
} from './shared'

const KIND_LABELS: Record<ResourceKind, string> = {
  skills: '技能',
  'mcp-servers': 'MCP 服务',
  'model-connections': '模型与连接',
}

const KIND_NOTES: Record<ResourceKind, string> = {
  skills: '工作区已发布的技能。Agent 通过配置引用，运行使用准入时固定的版本。',
  'mcp-servers':
    '查看 MCP 服务的连接状态、工具与已应用版本。这里只展示环境变量键名；连接状态不是连接测试结果，测试与诊断在详情页分轴显示。',
  'model-connections':
    '列表只列名称、默认模型、使用范围与本地配置就绪；地址、凭据、版本、所有者与配置引用 Agent 都在详情页。本地配置就绪不代表已实测连通——实测结果与时间单独记录，两种信息不互相冒充。',
}

/** 列表用的范围短标签（完整说明在资源详情页的范围选择器下方）。 */
// Short scope labels for the list; the full sentence lives on the detail page.
const SCOPE_SHORT = { shared: '共享', exclusive: '专属' } as const

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

    // 新建表单是列表页的展开式组件（不占新路由）；写入后由本页重新读取，
    // 表单本身不做乐观更新，也不维护第二份列表。
    const formOpen = ref(false)
    const createdNotice = ref('')

    function openForm(): void {
      createdNotice.value = ''
      formOpen.value = true
    }

    function closeForm(): void {
      formOpen.value = false
    }

    function onCreated(): void {
      formOpen.value = false
      createdNotice.value = '已创建并发布；列表将从服务端重新读取。'
      void listing.reload()
    }

    function skillRows(data: SkillView[]): TableProps {
      return {
        headers: ['技能', '描述', '版本', '文件数', '配置引用 Agent', '内容哈希'],
        tableClass: 'skills-table',
        columnClasses: ['col-name', '', 'col-number', 'col-number', 'col-name', 'col-hash'],
        rows: data.map((item) => ({
          key: item.name,
          cells: [
            identifier(item.name, resourceDetailHref('skills', item.name)),
            item.description === '' ? '—' : item.description,
            `v${item.version_no}`,
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
            identifier(item.name, resourceDetailHref('mcp-servers', item.name)),
            item.transport,
            item.command,
            item.env_keys.length === 0 ? '无' : item.env_keys.join('、'),
            // 未配置就是“—”：不把“没配置过”渲染成“连不上”。列表只有连接与工具
            // 两列事实；连接测试结果与诊断在详情页分轴显示，不在此合并。
            // Unconfigured is "—", never "disconnected"; test results stay on detail.
            item.published_version_id === 0 ? '—' : item.connected ? '已连接' : '未连接',
            item.tools.length === 0 ? '无' : item.tools.join('、'),
            `v${item.published_version_no}`,
            `v${item.applied_version_no}`,
            item.pending_restart ? badge('需重启', 'warn') : '否',
          ],
        })),
        empty: '还没有 MCP 服务定义',
      }
    }

    function modelRows(data: ModelConnectionView[]): TableProps {
      return {
        // R7：列表只留选择连接需要看的信息（名称 / 默认模型 / 范围 / 本地就绪）；
        // 地址、凭据、版本、所有者与引用者都在详情页，避免列表横向铺满。
        // R7: the list keeps only what choosing a connection needs; everything else
        // (address, credential, versions, owner, referencers) lives on the detail page.
        headers: ['名称', '默认模型', '范围', '可用性'],
        tableClass: 'models-table',
        columnClasses: ['col-name', 'col-name', 'col-short', 'col-state'],
        rows: data.map((item) => ({
          key: item.name,
          cells: [
            identifier(item.name, resourceDetailHref('model-connections', item.name)),
            identifier(item.default_model),
            badge(SCOPE_SHORT[item.scope], item.scope === 'exclusive' ? 'warn' : 'muted'),
            badge(AVAILABILITY_LABELS[item.availability], availabilityTone(item.availability)),
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
      if (createdNotice.value !== '') {
        content.push(h(Notice, { tone: 'info', text: createdNotice.value }))
      }
      if (formOpen.value) {
        content.push(h(ResourceForm, { api, kind, onCreated, onCancel: closeForm }))
      }
      if (listing.loading.value && listing.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      content.push(renderTable())

      return h('section', { class: 'view view-resources' }, [
        pageHeading(
          'RESOURCES / ' + (kind === 'skills' ? '05' : kind === 'mcp-servers' ? '04' : '03'),
          kind === 'mcp-servers' ? '工具与 MCP' : KIND_LABELS[kind],
          KIND_NOTES[kind],
          // MCP 服务页多一个“目录”入口：配方库只读，选一条再回来新建。
          // The MCP page also links to the read-only recipe catalog.
          h('div', { class: 'form-actions' }, [
            kind === 'mcp-servers'
              ? h('a', { class: 'button', href: mcpCatalogHref() }, 'MCP 目录')
              : null,
            h('button', { type: 'button', class: 'primary', onClick: openForm }, '新建'),
          ]),
        ),
        ...content,
        h(
          'p',
          { class: 'muted small resource-note' },
          '点击名称进入详情：查看版本、修改定义与删除。列表只读取服务端当前状态，任何页面都不回显凭据值。',
        ),
      ])
    }
  },
})
