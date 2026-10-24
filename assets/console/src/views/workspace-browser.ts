// 工作区只读浏览视图（/console/workspace）：`.basalt/agents/` 的目录树与单文件
// 预览。浏览根由服务端固定，页面只发相对路径（`/` 分隔）——不归一化、不拼本地路径；
// 软链接只显示类型、不可进入（服务端按不存在拒绝）；`.staging` / `.backup` 残骸由
// 服务端隐藏，页面拿到的就是可展示集合。全部只读：本视图没有任何写入口，越界 /
// 超限（400）与不存在（404）直接展示服务端固定文案。
// Read-only workspace browser: the server owns the root and every path rule; the view
// only forwards root-relative paths. Symlinks are shown by type and never traversed,
// and there is no write affordance anywhere in this page.
import { defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import { adminApi } from '../api-admin'
import type { WorkspaceEntry, WorkspaceFile } from '../api-admin'
import { CodeBlock, DataTable, Notice } from '../components/ui'
import type { TableRow } from '../components/ui'
import { agentConfigHref } from '../router'
import {
  badge,
  definitions,
  identifier,
  pageHeading,
  section,
  useLoader,
  viewProps,
} from './shared'

/** 条目类型的中文标签与色调（契约枚举固定为 file / dir / symlink）。 */
// Labels and tones for the three contract entry types.
const ENTRY_LABELS: Record<WorkspaceEntry['type'], { label: string; tone: 'ok' | 'warn' | 'muted' }> = {
  dir: { label: '目录', tone: 'muted' },
  file: { label: '文件', tone: 'ok' },
  symlink: { label: '软链接', tone: 'warn' },
}

/** 浏览根文案：路径恒相对 `.basalt/agents/`，页面不暴露宿主绝对路径。 */
// The browse root caption; host absolute paths never reach the page.
const BROWSE_ROOT = '.basalt/agents/'

/** 字节数 → B / KiB / MiB（工作区预览上限 256 KiB，不借用运行指标的 MB 格式）。 */
// Sizes are rendered in B / KiB / MiB because previews cap at 256 KiB.
function formatSize(value: number): string {
  if (value < 1024) {
    return `${value} B`
  }
  if (value < 1024 * 1024) {
    return `${(value / 1024).toFixed(1)} KiB`
  }
  return `${(value / (1024 * 1024)).toFixed(1)} MiB`
}

/** 取相对路径的最后一段作为展示名；根目录条目本身就是一段。 */
// The last path segment is the display name inside the current directory.
function baseName(path: string): string {
  const segments = path.split('/')
  return segments[segments.length - 1] ?? path
}

export const WorkspaceBrowserView = defineComponent({
  name: 'WorkspaceBrowserView',
  props: viewProps,
  setup(props) {
    const client = adminApi(props.ctx.api)

    // 当前目录（相对浏览根；空串 = 浏览根）。树加载函数读取它，切换目录后 reload。
    // currentPath is the only navigation state; the loader always reads its latest value.
    const currentPath = ref('')
    const listing = useLoader(() => client.getWorkspaceTree(currentPath.value))

    // 预览是一次显式点击的结果，不是页面加载的一部分：成功前保留旧面板，失败清空。
    // The preview is click-driven; stale responses are dropped by ticket accounting.
    const preview = ref<WorkspaceFile | null>(null)
    const previewPath = ref('')
    const previewError = ref('')
    const previewLoading = ref(false)
    let previewTicket = 0

    function resetPreview(): void {
      previewTicket += 1
      preview.value = null
      previewPath.value = ''
      previewError.value = ''
      previewLoading.value = false
    }

    function openDir(path: string): void {
      currentPath.value = path
      resetPreview()
      void listing.reload()
    }

    function goRoot(): void {
      if (currentPath.value !== '') {
        openDir('')
      } else {
        void listing.reload()
      }
    }

    function goUp(): void {
      const segments = currentPath.value === '' ? [] : currentPath.value.split('/')
      segments.pop()
      openDir(segments.join('/'))
    }

    /** 预览一个文件：只认最后一次点击的结果，旧请求晚到不会覆盖新面板。 */
    // Previews one file; only the latest click is allowed to update the panel.
    async function openFile(path: string): Promise<void> {
      const ticket = ++previewTicket
      previewPath.value = path
      previewError.value = ''
      previewLoading.value = true
      try {
        const file = await client.getWorkspaceFile(path)
        if (ticket === previewTicket) {
          preview.value = file
        }
      } catch (caught) {
        if (ticket === previewTicket) {
          preview.value = null
          previewError.value = errorMessage(caught)
        }
      } finally {
        if (ticket === previewTicket) {
          previewLoading.value = false
        }
      }
    }

    /** 面包屑：根 + 逐级目录；最后一级不可点，中间级可回退。 */
    // Breadcrumbs: root plus each segment; the current segment is not a button.
    function breadcrumb(): VNode {
      const segments = currentPath.value === '' ? [] : currentPath.value.split('/')
      const crumbs: VNodeChild[] = [
        segments.length === 0
          ? h('strong', 'Agents')
          : h('button', { type: 'button', class: 'link-button', onClick: goRoot }, 'Agents'),
      ]
      segments.forEach((segment, index) => {
        crumbs.push(h('span', { 'aria-hidden': 'true' }, ' / '))
        const target = segments.slice(0, index + 1).join('/')
        crumbs.push(
          index === segments.length - 1
            ? h('strong', segment)
            : h(
                'button',
                { type: 'button', class: 'link-button', onClick: () => { openDir(target) } },
                segment,
              ),
        )
      })
      return h('div', { class: 'breadcrumb', 'aria-label': '当前浏览路径' }, crumbs)
    }

    function entryRows(entries: readonly WorkspaceEntry[]): TableRow[] {
      return entries.map((entry) => {
        const meta = ENTRY_LABELS[entry.type]
        let action: VNodeChild
        if (entry.type === 'dir') {
          action = h(
            'button',
            { type: 'button', onClick: () => { openDir(entry.path) } },
            '打开',
          )
        } else if (entry.type === 'file') {
          action = h(
            'button',
            { type: 'button', onClick: () => { void openFile(entry.path) } },
            '预览',
          )
        } else {
          // 软链接只报告类型：服务端拒绝跟随，页面不给"进入"入口。
          action = h('span', { class: 'muted small' }, '不跟随')
        }
        return {
          key: entry.path,
          cells: [
            identifier(baseName(entry.path)),
            badge(meta.label, meta.tone),
            entry.type === 'file' ? h('span', formatSize(entry.size)) : h('span', { class: 'muted' }, '—'),
            action,
          ],
        }
      })
    }

    /**
     * 预览的 AGENT.md 恰好是某个 Agent 的定义文件时（`.basalt/agents/<name>/AGENT.md`）
     * 给出结构化配置入口；更深的同名文件不是定义文件，不提供跳转。
     * Only the agent's own definition file links to the structured config page.
     */
    function agentConfigOf(path: string): string | null {
      const segments = path.split('/')
      return segments.length === 2 && segments[1] === 'AGENT.md' ? (segments[0] ?? null) : null
    }

    function renderPreview(): VNodeChild {
      if (previewPath.value === '') {
        return null
      }
      const configAgent = agentConfigOf(previewPath.value)
      const actions: VNodeChild[] = []
      if (configAgent !== null) {
        actions.push(
          h(
            'a',
            { href: agentConfigHref(configAgent), class: 'link-button' },
            '打开 Agent 配置',
          ),
        )
      }
      actions.push(h('button', { type: 'button', onClick: resetPreview }, '关闭预览'))
      const children: VNodeChild[] = [
        h('div', { class: 'agent-toolbar' }, [
          h('p', { class: 'muted small' }, `${BROWSE_ROOT}${previewPath.value}`),
          h('div', { class: 'form-actions' }, actions),
        ]),
      ]
      if (previewLoading.value) {
        children.push(h('p', { class: 'loading' }, '正在读取文件…'))
      }
      if (previewError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: previewError.value }))
      }
      const file = preview.value
      if (file !== null) {
        children.push(
          definitions([
            ['路径', identifier(file.path)],
            ['大小', formatSize(file.size)],
            ['截断', file.truncated ? '是（契约要求为否，请报告）' : '否'],
          ]),
          h(CodeBlock, { text: file.content }),
        )
      }
      children.push(h('p', { class: 'muted small' }, '只读预览：不提供编辑、上传或下载；超过 256 KiB 的文件直接拒绝，不截断。'))
      return section('文件预览', children)
    }

    function renderListing(): VNodeChild {
      if (listing.error.value !== '') {
        const actions: VNodeChild[] = []
        if (currentPath.value !== '') {
          actions.push(
            h(
              'button',
              { type: 'button', onClick: goRoot },
              '返回浏览根',
            ),
          )
        }
        actions.push(
          h(
            'button',
            { type: 'button', onClick: () => { void listing.reload() } },
            '重试',
          ),
        )
        return section('目录', [
          h(Notice, { tone: 'error', text: listing.error.value }),
          h('div', { class: 'form-actions' }, actions),
        ])
      }
      const data = listing.data.value
      if (data === null) {
        return section('目录', [h('p', { class: 'loading' }, '正在读取目录…')])
      }
      return section('目录', [
        h(DataTable, {
          headers: ['名称', '类型', '大小', '操作'],
          rows: entryRows(data),
          empty: '这个目录没有可浏览的条目（staging / backup 残骸不会显示）。',
          tableClass: 'workspace-table',
          columnClasses: ['col-name', 'col-type', 'col-size', 'col-actions'],
        }),
      ])
    }

    return (): VNode => {
      const actions: VNodeChild[] = [
        currentPath.value === ''
          ? null
          : h('button', { type: 'button', onClick: goUp }, '上一级'),
        h(
          'button',
          { type: 'button', disabled: listing.loading.value, onClick: () => { void listing.reload() } },
          listing.loading.value ? '读取中…' : '刷新',
        ),
      ]
      const crumbs = breadcrumb()
      return h('section', { class: 'view view-workspace-browser' }, [
        pageHeading(
          'WORKSPACE / FILES',
          '工作区文件',
          '只读浏览 Agent 目录：软链接不跟随，staging / backup 残骸不可见，单文件上限 256 KiB。',
          h('span', { class: 'page-count' }, `浏览根 ${BROWSE_ROOT}`),
        ),
        h('div', { class: 'agent-toolbar' }, [crumbs, h('div', { class: 'form-actions' }, actions)]),
        renderListing(),
        renderPreview(),
      ])
    }
  },
})
