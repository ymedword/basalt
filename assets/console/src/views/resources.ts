// 只读资源视图：skills / mcp-servers / model-connections 三类（FR18）。
// 模型连接只展示环境变量引用名；MCP 展示占位符键名与凭据档位（不写尾号），
// 两类都绝不回显凭据值。
// Read-only resources. Model connections show the environment-variable reference
// only; MCP entries show placeholder keys and credential states (never a tail).
// Credential values never reach the page.
import { defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type {
  McpCredentialKeyState,
  McpServerView,
  ModelConnectionView,
  SkillView,
} from '../api'
import type { McpApplyOutcome } from '../components/resource-form'
import { ResourceForm, createdApplyNotice } from '../components/resource-form'
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
    '查看 MCP 服务的连接状态、工具与已应用版本。这里只展示占位符键名与凭据档位，不回显任何值；连接状态不是连接测试结果，测试与诊断在详情页分轴显示。',
  'model-connections':
    '列表只列名称、默认模型、使用范围与本地配置就绪；地址、凭据、版本、所有者与配置引用 Agent 都在详情页。本地配置就绪不代表已实测连通——实测结果与时间单独记录，两种信息不互相冒充。',
}

/** 列表用的范围短标签（完整说明在资源详情页的范围选择器下方）。 */
// Short scope labels for the list; the full sentence lives on the detail page.
const SCOPE_SHORT = { shared: '共享', exclusive: '专属' } as const

/**
 * 列表「凭据」列的四档（契约 `McpCredentialKeyView.state`）：与详情页同一套
 * 标签与色调，多槽位时按档计数。列表不展开键名——键名在「占位符键」列，
 * 逐槽位明细与撤销入口在详情页。
 * The list column groups slots by their four contract states; per-slot detail
 * and the revoke entry stay on the detail page.
 */
const SLOT_STATE_SUMMARY: Readonly<
  Record<McpCredentialKeyState, { readonly label: string; readonly tone: 'ok' | 'bad' | 'muted' | 'warn' }>
> = {
  managed: { label: '托管有效', tone: 'ok' },
  env: { label: '环境变量', tone: 'muted' },
  missing: { label: '缺托管值', tone: 'warn' },
  managed_revoked: { label: '托管已撤销', tone: 'bad' },
}

/** 汇总顺序固定：有效 → 环境变量 → 缺值 → 已撤销（坏消息靠右，扫读不跳跃）。 */
const SLOT_SUMMARY_ORDER = ['managed', 'env', 'missing', 'managed_revoked'] as const

/**
 * 「凭据」列的取值：未发布不判断（`—`），没有占位符写「无」，其余按四档状态
 * 计数；未声明来源的槽位单独计数并标红——它会让下一次保存被拒，不能在列表里
 * 隐形。列表不写尾号，也不展开键名（键名在「占位符键」列）。
 * Unpublished entries show "—" and slot-less ones "无"; undeclared slots are
 * counted separately because they block the next save.
 */
function credentialCell(item: McpServerView): VNodeChild {
  if (item.published_version_id === 0) {
    return '—'
  }
  const declared = new Set(item.credential_keys.map((slot) => slot.key))
  const undeclared = [...item.env_keys, ...item.header_keys].filter((key) => !declared.has(key))
  if (item.credential_keys.length === 0 && undeclared.length === 0) {
    return '无'
  }
  const counts = new Map<McpCredentialKeyState, number>()
  for (const slot of item.credential_keys) {
    counts.set(slot.state, (counts.get(slot.state) ?? 0) + 1)
  }
  const cells: VNodeChild[] = SLOT_SUMMARY_ORDER.flatMap((state) => {
    const count = counts.get(state) ?? 0
    if (count === 0) {
      return []
    }
    const copy = SLOT_STATE_SUMMARY[state]
    return [badge(count > 1 ? `${copy.label} ×${count}` : copy.label, copy.tone)]
  })
  if (undeclared.length > 0) {
    cells.push(badge(`未声明来源 ×${undeclared.length}`, 'bad'))
  }
  return cells
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

    // 新建表单是列表页的展开式组件（不占新路由）；写入后由本页重新读取，
    // 表单本身不做乐观更新，也不维护第二份列表。
    const formOpen = ref(false)
    const createdNotice = ref('')
    // 已创建但应用没切换运行版本属 warn，不能挂在 info 上轻描淡写。
    const createdTone = ref<'info' | 'warn'>('info')

    function openForm(): void {
      createdNotice.value = ''
      createdTone.value = 'info'
      formOpen.value = true
    }

    function closeForm(): void {
      formOpen.value = false
    }

    function onCreated(_view: unknown, apply?: unknown): void {
      formOpen.value = false
      // 表单卸载后它自己的提示随之消失，所以保存 / 应用两段的结论在这里重说一遍。
      const notice = createdApplyNotice(apply as McpApplyOutcome | null | undefined)
      createdNotice.value = `${notice.text}列表将从服务端重新读取。`
      createdTone.value = notice.tone
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
        headers: ['名称', '传输', '可执行名', '占位符键', '凭据', '连接', '工具', '发布版本', '已应用', '待重启'],
        tableClass: 'mcp-table',
        columnClasses: ['col-name', 'col-short', 'col-command', 'col-keys', 'col-state', 'col-state', 'col-keys', 'col-number', 'col-number', 'col-state'],
        rows: data.map((item) => ({
          key: item.name,
          cells: [
            identifier(item.name, resourceDetailHref('mcp-servers', item.name)),
            item.transport,
            // 可执行名可能是很长的绝对路径：与标识列同一渲染（省略号 + 悬停全值），
            // 直接输出原文会在定宽表里溢出、压到相邻列（用户实测 2026-10-27）。
            // Commands can be long absolute paths, so they render as an identifier
            // (ellipsis plus title) instead of raw text overflowing its column.
            item.command === '' ? '—' : identifier(item.command),
            // env 键与请求头名共用同一名字空间（契约保证不交叉），列表一并列出。
            // Env keys and header names are one namespace; the list shows both.
            [...item.env_keys, ...item.header_keys].length === 0
              ? '无'
              : [...item.env_keys, ...item.header_keys].join('、'),
            credentialCell(item),
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
        content.push(h(Notice, { tone: createdTone.value, text: createdNotice.value }))
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
