// MCP 目录视图（控制台内部路由 /console/catalog/mcp）：只读配方库 + “添加到配置”。
// 目录是“怎么配”的建议，不是运行配置：本页只读目录与现有 server 名单，**不**安装、
// **不**启动进程、**不**写配置文件；“添加到配置”只把配方摆进新建表单，保存仍由用户
// 自己点按钮，走同一条创建 / env 闸门 / 发布路径。
// 配方状态只有“配方已实测 / 配方未实测”两种，`verified=false` 一律显示后者——页面
// 不出现“已验证”徽章，也不把未实测配方说成可用。
// The read-only MCP catalog: recipes, filters and a prefill action. The page never
// installs, spawns or writes; "add to configuration" only fills the create form.
import { computed, defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type { MCPCatalogEntry, MCPCatalogWarning, McpServerView } from '../api'
import { catalogApi } from '../api-catalog'
import type { McpServerWrite } from '../api-admin'
import type { McpApplyOutcome } from '../components/resource-form'
import { ResourceForm, createdApplyNotice } from '../components/resource-form'
import { DataTable, Notice } from '../components/ui'
import type { TableRow } from '../components/ui'
import { pageHeading, badge, definitions, identifier, section, useLoader, viewProps } from './shared'

/**
 * 目录 warnings 的前端文案（D-10）：后端只回固定 code，文案留在这一张表里，
 * 新增 code 必须同时改契约与这里。未知 code 原样显示，不编造解释。
 * Fixed code → text map; an unknown code is shown verbatim rather than explained.
 */
const CATALOG_WARNING_LABELS: Record<MCPCatalogWarning, string> = {
  read_only_tool_face_required: '只能以只读工具面接入：写入能力需另建 server',
  server_write_capability: '该 server 具备写入能力，授予前先确认影响范围',
  write_tool_name_collision: '写入类工具名可能与内置工具重名，注册时会被跳过',
  browser_required: '需要浏览器运行环境，未安装时无法启动',
  sqlite_sample_only: '附带的 SQLite 只是样例，生产环境需替换',
  package_entry_unverified: '包入口未实测，命令能否定位以诊断结果为准',
  stdio_child_egress_unconstrained: 'stdio 子进程的出网不受本仓网络策略约束',
}

/** 传输筛选与配方状态筛选的取值（“全部”用空串）。 */
const TRANSPORTS = ['stdio', 'http'] as const
type TransportFilter = '' | (typeof TRANSPORTS)[number]
type RecipeFilter = '' | 'verified' | 'unverified'

/** 配方状态文案：只有这两个值，不出现“已验证”这类总括徽章。 */
function recipeBadge(entry: MCPCatalogEntry): VNode {
  return entry.verified ? badge('配方已实测', 'ok') : badge('配方未实测', 'muted')
}

/** 依赖短语：目录的 requires 至少一项，直接并列展示。 */
function requiresText(entry: MCPCatalogEntry): string {
  return entry.requires.length === 0 ? '—' : entry.requires.join('、')
}

/** 目录条目 → 新建表单的预填载荷；env 逐条转成 `KEY=${VAR}` 的初始文本。 */
function prefillOf(entry: MCPCatalogEntry): McpServerWrite {
  const env: Record<string, string> = {}
  for (const item of entry.env) {
    env[item.key] = item.placeholder
  }
  return {
    // 名称取 id：目录 id 是 kebab-case 的稳定标识，适合直接作为 server 名；
    // 用户仍可改。显示名留给页面标题。
    name: entry.id,
    transport: entry.transport,
    command: entry.command,
    args: [...entry.args],
    url: entry.url,
    env,
  }
}

export const CatalogView = defineComponent({
  name: 'CatalogView',
  props: viewProps,
  setup(props) {
    const api = props.ctx.api
    const client = catalogApi(api)

    const catalog = useLoader(() => client.getMCPCatalog())
    // 配置状态来自既有 server 名单：读取失败时列显示“—”，不把失败说成“未配置”。
    // The configured column reads the live server list; a failure shows "—", never
    // "not configured", because the two are different facts.
    const configured = useLoader(async () => {
      const servers: McpServerView[] = await api.listMcpServers()
      return new Set(servers.map((server) => server.name))
    })

    const keyword = ref('')
    const category = ref('')
    const transport = ref<TransportFilter>('')
    const recipe = ref<RecipeFilter>('')
    const selectedId = ref('')
    const prefillEntry = ref<MCPCatalogEntry | null>(null)
    const formNotice = ref('')
    // 已创建但应用没切换运行版本属 warn，不能挂在 info 上轻描淡写。
    const formNoticeTone = ref<'info' | 'warn'>('info')

    const entries = computed<readonly MCPCatalogEntry[]>(() => catalog.data.value?.items ?? [])

    const categories = computed<readonly string[]>(() => {
      const seen = new Set<string>()
      for (const entry of entries.value) {
        seen.add(entry.category)
      }
      return [...seen].sort((left, right) => left.localeCompare(right, 'zh-Hans-CN'))
    })

    const filtered = computed<readonly MCPCatalogEntry[]>(() => {
      const needle = keyword.value.trim().toLowerCase()
      return entries.value.filter((entry) => {
        if (category.value !== '' && entry.category !== category.value) {
          return false
        }
        if (transport.value !== '' && entry.transport !== transport.value) {
          return false
        }
        if (recipe.value === 'verified' && !entry.verified) {
          return false
        }
        if (recipe.value === 'unverified' && entry.verified) {
          return false
        }
        if (needle === '') {
          return true
        }
        const haystack = [entry.id, entry.display_name, entry.summary, entry.category, entry.command]
          .join('\n')
          .toLowerCase()
        return haystack.includes(needle)
      })
    })

    const selected = computed<MCPCatalogEntry | null>(
      () => entries.value.find((entry) => entry.id === selectedId.value) ?? null,
    )

    /** 配置状态：名单未读到之前一律“—”，读到后按名字判断。 */
    function configuredCell(entry: MCPCatalogEntry): VNodeChild {
      if (configured.error.value !== '') {
        return '—'
      }
      const names = configured.data.value
      if (names === null) {
        return '—'
      }
      return names.has(entry.id) ? badge('已配置', 'ok') : badge('未配置', 'muted')
    }

    function openPrefill(entry: MCPCatalogEntry): void {
      selectedId.value = entry.id
      prefillEntry.value = entry
      formNoticeTone.value = 'info'
      formNotice.value = `已按配方“${entry.display_name}”预填新建表单；页面不会自动保存，请核对后点“创建并发布”。`
    }

    function closePrefill(): void {
      prefillEntry.value = null
    }

    function onCreated(_view: unknown, apply?: unknown): void {
      const entry = prefillEntry.value
      prefillEntry.value = null
      // 表单卸载后它自己的提示随之消失：保存 / 应用两段的结论在这里重说一遍。
      const notice = createdApplyNotice(apply as McpApplyOutcome | null | undefined)
      const recipe = entry === null ? '' : `配方“${entry.display_name}”本身没有被修改；`
      formNotice.value = `${notice.text}${recipe}列表将从服务端重新读取。`
      formNoticeTone.value = notice.tone
      void catalog.reload()
      void configured.reload()
    }

    function rows(data: readonly MCPCatalogEntry[]): TableRow[] {
      return data.map((entry) => ({
        key: entry.id,
        cells: [
          identifier(entry.display_name),
          entry.category,
          entry.transport,
          requiresText(entry),
          recipeBadge(entry),
          configuredCell(entry),
          h('div', { class: 'form-actions' }, [
            h(
              'button',
              {
                type: 'button',
                onClick: () => {
                  selectedId.value = entry.id
                },
              },
              '详情',
            ),
            h(
              'button',
              {
                type: 'button',
                class: 'primary',
                onClick: () => {
                  openPrefill(entry)
                },
              },
              '添加到配置',
            ),
          ]),
        ],
      }))
    }

    function renderFilters(): VNode {
      return h('form', { class: 'filters', onSubmit: (event: Event) => event.preventDefault() }, [
        h('label', [
          '关键词',
          h('input', {
            type: 'search',
            value: keyword.value,
            placeholder: '名称、摘要或命令',
            onInput: (event: Event) => {
              keyword.value = (event.target as HTMLInputElement).value
            },
          }),
        ]),
        h('label', [
          '类别',
          h(
            'select',
            {
              value: category.value,
              onChange: (event: Event) => {
                category.value = (event.target as HTMLSelectElement).value
              },
            },
            [
              h('option', { value: '' }, '全部'),
              ...categories.value.map((item) => h('option', { key: item, value: item }, item)),
            ],
          ),
        ]),
        h('label', [
          '传输',
          h(
            'select',
            {
              value: transport.value,
              onChange: (event: Event) => {
                const next = (event.target as HTMLSelectElement).value
                transport.value = (TRANSPORTS as readonly string[]).includes(next)
                  ? (next as TransportFilter)
                  : ''
              },
            },
            [
              h('option', { value: '' }, '全部'),
              ...TRANSPORTS.map((item) => h('option', { key: item, value: item }, item)),
            ],
          ),
        ]),
        h('label', [
          '配方状态',
          h(
            'select',
            {
              value: recipe.value,
              onChange: (event: Event) => {
                const next = (event.target as HTMLSelectElement).value
                recipe.value = next === 'verified' || next === 'unverified' ? next : ''
              },
            },
            [
              h('option', { value: '' }, '全部'),
              h('option', { value: 'verified' }, '配方已实测'),
              h('option', { value: 'unverified' }, '配方未实测'),
            ],
          ),
        ]),
      ])
    }

    function renderEntryDetail(entry: MCPCatalogEntry): VNode {
      const pairs: Array<readonly [string, VNodeChild]> = [
        ['类别', entry.category],
        ['传输', entry.transport],
        entry.transport === 'stdio'
          ? ['可执行名', identifier(entry.command)]
          : ['URL', identifier(entry.url)],
        ['参数（每行一个）', entry.args.length === 0 ? '无' : h('code', entry.args.join(' '))],
        // 配方只写“需要什么”，不知道本机是否就位：标签里带前提，不让静态清单冒充读数。
        // The recipe states requirements, never readiness.
        ['依赖（配方要求）', requiresText(entry)],
        ['配方状态', recipeBadge(entry)],
        ['验证说明', entry.verification_note === '' ? '—' : entry.verification_note],
        ['验证时间', entry.verified_at === '' ? '—' : entry.verified_at],
        ['验证平台', entry.verified_platforms.length === 0 ? '—' : entry.verified_platforms.join('、')],
        [
          '来源',
          entry.source_url.startsWith('https://')
            ? h('a', { href: entry.source_url, target: '_blank', rel: 'noreferrer noopener' }, entry.source_url)
            : entry.source_url,
        ],
      ]
      const children: VNodeChild[] = [
        h('p', { class: 'muted' }, entry.summary),
        definitions(pairs),
        h(
          'p',
          { class: 'muted small' },
          '依赖是配方写明的运行时要求，不是就位状态：命令能否定位、组件版本是否匹配，以保存后详情页的「依赖」轴诊断为准。',
        ),
      ]
      if (entry.env.length > 0) {
        children.push(
          h('h3', '环境变量占位'),
          h(DataTable, {
            tableClass: 'catalog-env-table',
            columnClasses: ['col-name', 'col-name', 'col-short', ''],
            headers: ['键', '占位符', '必需', '说明'],
            rows: entry.env.map((item) => ({
              key: item.key,
              cells: [identifier(item.key), h('code', item.placeholder), item.required ? '是' : '否', item.description],
            })),
            empty: '该配方不需要环境变量',
          }),
          h(
            'p',
            { class: 'muted small' },
            '占位符只是“该填哪个变量”的提示，不是值；保存时服务端会核对当前进程环境，缺失会拒绝写入（503）。',
          ),
        )
      }
      if (entry.warnings.length > 0) {
        children.push(
          h('h3', '已知边界'),
          h(
            'ul',
            { class: 'problems' },
            entry.warnings.map((code) =>
              h('li', { key: code }, CATALOG_WARNING_LABELS[code] ?? `未知边界（${code}）`),
            ),
          ),
        )
      }
      children.push(
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              onClick: () => {
                openPrefill(entry)
              },
            },
            '添加到配置',
          ),
        ]),
      )
      return section(`配方详情：${entry.display_name}`, children)
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        h(
          'p',
          { class: 'muted' },
          '目录是配方库，不是安装器：这里不安装包、不启动进程、不写配置文件。' +
            '“添加到配置”只预填新建表单，保存仍走既有的写入与环境变量闸门。',
        ),
        renderFilters(),
      ]

      if (catalog.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: catalog.error.value }))
      }
      if (configured.error.value !== '') {
        content.push(
          h(Notice, {
            tone: 'warn',
            text: `读取现有 MCP 服务失败，配置状态列暂不可用：${configured.error.value}`,
          }),
        )
      }
      if (formNotice.value !== '') {
        content.push(h(Notice, { tone: formNoticeTone.value, text: formNotice.value }))
      }
      if (prefillEntry.value !== null) {
        content.push(
          h(ResourceForm, {
            api,
            kind: 'mcp-servers',
            prefill: prefillOf(prefillEntry.value),
            onCreated,
            onCancel: closePrefill,
          }),
        )
      }
      if (catalog.loading.value && catalog.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      if (catalog.data.value !== null) {
        content.push(
          h(DataTable, {
            tableClass: 'catalog-table',
            columnClasses: ['col-name', 'col-name', 'col-short', 'col-keys', 'col-state', 'col-state', 'col-action'],
            headers: ['名称', '类别', '传输', '依赖', '配方状态', '配置状态', '操作'],
            rows: rows(filtered.value),
            empty: '没有符合筛选条件的配方',
          }),
        )
      }
      if (selected.value !== null) {
        content.push(renderEntryDetail(selected.value))
      }

      return h('section', { class: 'view view-catalog' }, [
        pageHeading(
          'LIBRARY / MCP CATALOG',
          'MCP 目录',
          '推荐接入的 MCP server 配方：看得到怎么填、缺什么、哪些边界还没实测。',
        ),
        ...content,
        h(
          'p',
          { class: 'muted small resource-note' },
          '配方未实测不等于不可用，也不等于已验证：命令能否定位、环境变量是否就绪，以保存后的诊断与测试结果为准。',
        ),
      ])
    }
  },
})
