// Agent 页三段面板（14D 设计 §2.5.1）：需要你提供 / 系统已准备 / 未启用原因。
// 三段只回答「离能跑还差什么」——阻断项来自就绪视图（与启用前置共用同一份判定），
// 提示项只点名、不冒充阻断；系统已准备只显示**读得到**的事实（服务版本、MCP 版本、
// 脚本与样例库目录），读不到的项写「暂不可读」而不是「已就绪」。
// Three panels answering what still blocks an agent. Pending items come from the
// readiness projection (the same verdict as enable), notices never masquerade as
// blockers, and only readable facts are shown as prepared.
import { defineComponent, h } from 'vue'
import type { PropType, VNode } from 'vue'
import type {
  AgentConfigView,
  AgentReadinessItem,
  AgentReadinessView,
  AgentState,
  InfoView,
  McpServerView,
  NotifyMode,
} from '../api'
import type { WorkspaceEntry } from '../api-admin'
import { StatusBadge } from './ui'
import type { Tone } from './ui'

/** 一行判定：标签、说明、色调、状态词与可选修复入口。 */
// One row: label, detail, tone, status word and an optional repair link.
interface ReadyRow {
  readonly key: string
  readonly label: string
  readonly detail: string
  readonly tone: Tone
  readonly status: string
  readonly href?: string
}

const NOTIFY_MODE_LABELS: Readonly<Record<NotifyMode, string>> = {
  none: '不发送通知',
  all_eligible: '自动发送（全部合格渠道）',
  allowlist: '按声明渠道发送',
}

/** 修复入口的链接文字：路径都是控制台既有页面，未知路径不伪造标签。 */
// Link labels for console paths that already exist in the router.
function fixLabel(fix: string): string {
  if (fix.endsWith('/config')) {
    return 'Agent 配置'
  }
  if (fix === '/console/resources/model-connections') {
    return '模型与连接'
  }
  if (fix === '/console/resources/mcp-servers') {
    return 'MCP 服务'
  }
  if (fix.startsWith('/console/resources/mcp-servers/')) {
    return 'MCP 详情'
  }
  if (fix === '/console/resources/skills') {
    return '技能'
  }
  if (fix === '/console/settings/notifications') {
    return '通知渠道'
  }
  return '前往修复'
}

/** `mcp_server/name#KEY` → { name, key }；其他形态返回空。 */
// Splits the machine name of a credential item; other shapes yield empty parts.
function splitCredentialItem(item: string): { name: string; key: string } {
  const slash = item.indexOf('/')
  const hash = item.indexOf('#')
  if (!item.startsWith('mcp_server/') || slash < 0 || hash < slash) {
    return { name: '', key: '' }
  }
  return { name: item.slice(slash + 1, hash), key: item.slice(hash + 1) }
}

/** 就绪条目的行标签：不把「资源没发布」说成「缺凭据」。 */
// Row labels per kind; a missing published resource is not a missing credential.
function pendingLabel(item: AgentReadinessItem): string {
  switch (item.kind) {
    case 'model_connection':
      return '模型连接'
    case 'credential': {
      const parts = splitCredentialItem(item.item)
      if (parts.name !== '') {
        return `${parts.name} 凭据（${parts.key}）`
      }
      return '模型连接凭据'
    }
    case 'resource': {
      if (item.item.startsWith('skill/')) {
        return `技能 ${item.item.slice('skill/'.length)}`
      }
      if (item.item.startsWith('mcp_server/')) {
        return `MCP 服务 ${item.item.slice('mcp_server/'.length)}`
      }
      return '配置项'
    }
    case 'notify_channel':
      return item.item === '' ? '通知渠道（可选）' : `通知渠道 ${item.item}（可选）`
    case 'budget':
      return '预算上限'
    case 'concurrency':
      return '并发上限'
    default:
      return '待补项'
  }
}

function pendingRow(item: AgentReadinessItem, index: number): ReadyRow {
  return {
    key: `pending-${index}`,
    label: pendingLabel(item),
    detail: item.detail,
    tone: 'warn',
    status: '待补',
    ...(item.fix === '' ? {} : { href: item.fix }),
  }
}

function noticeRow(item: AgentReadinessItem, index: number): ReadyRow {
  return {
    key: `notice-${index}`,
    label: pendingLabel(item),
    detail: item.detail,
    tone: 'warn',
    status: '提示',
    ...(item.fix === '' ? {} : { href: item.fix }),
  }
}

/** 形状守卫：测试桩或旧服务端返回非对象时按「不可读」处理。 */
// Shape guard for non-object readiness payloads.
function readinessOf(value: AgentReadinessView | null): AgentReadinessView | null {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    return null
  }
  return Array.isArray(value.pending) && Array.isArray(value.notices) ? value : null
}

/** 形状守卫：配置视图读不到时不让 `.fields` 访问炸掉整页。 */
// Shape guard for the config payload; anything else is treated as unreadable.
function configOf(value: AgentConfigView | null): AgentConfigView | null {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    return null
  }
  return value.fields === undefined || value.fields === null ? null : value
}

export const AgentReadinessPanels = defineComponent({
  name: 'AgentReadinessPanels',
  props: {
    readiness: { type: Object as PropType<AgentReadinessView | null>, default: null },
    readinessError: { type: String, default: '' },
    config: { type: Object as PropType<AgentConfigView | null>, default: null },
    configError: { type: String, default: '' },
    /** null = 尚未读取或读取失败；空数组是有效值（没有 MCP 服务）。 */
    mcps: { type: Array as PropType<readonly McpServerView[] | null>, default: null },
    info: { type: Object as PropType<InfoView | null>, default: null },
    infoError: { type: String, default: '' },
    /** null = 工作区树读不到；空数组 = 目录为空。 */
    entries: { type: Array as PropType<readonly WorkspaceEntry[] | null>, default: null },
    entriesError: { type: String, default: '' },
    lifecycle: { type: String as PropType<AgentState | null>, default: null },
  },
  setup(props) {
    function rowsList(rows: readonly ReadyRow[]): VNode {
      return h(
        'ul',
        { class: 'ready-rows' },
        rows.map((row) =>
          h('li', { key: row.key, class: 'ready-row' }, [
            h('div', [h('strong', row.label), h('p', row.detail)]),
            h('span', { class: 'ready-side' }, [
              h(StatusBadge, { label: row.status, tone: row.tone }),
              row.href === undefined
                ? null
                : h('a', { href: row.href, class: 'ready-fix' }, `${fixLabel(row.href)} →`),
            ]),
          ]),
        ),
      )
    }

    function panel(key: string, title: string, eyebrow: string, rows: readonly ReadyRow[]): VNode {
      return h('section', { key, class: 'card ready-panel' }, [
        h('div', { class: 'panel-heading' }, [
          h('h2', title),
          h('span', { class: 'eyebrow' }, eyebrow),
        ]),
        rows.length === 0 ? h('p', { class: 'muted small' }, '暂无条目。') : rowsList(rows),
      ])
    }

    /** 需要你提供：阻断项 + 可选通知 + 已就绪的模型 / 凭据摘要。 */
    // What the operator must provide; blockers first, optional notify last.
    function providePanel(): VNode {
      const readiness = readinessOf(props.readiness)
      const config = configOf(props.config)
      const rows: ReadyRow[] = []
      if (props.readinessError !== '') {
        rows.push({
          key: 'readiness-error',
          label: '就绪状态',
          detail: `读取失败：${props.readinessError}`,
          tone: 'bad',
          status: '不可读',
        })
      } else if (readiness === null) {
        rows.push({
          key: 'readiness-loading',
          label: '就绪状态',
          detail: '正在计算离能跑还差什么…',
          tone: 'muted',
          status: '读取中',
        })
      } else {
        readiness.pending.forEach((item, index) => rows.push(pendingRow(item, index)))
        readiness.notices
          .filter((item) => item.kind === 'notify_channel')
          .forEach((item, index) => rows.push(noticeRow(item, index)))
      }

      // 已就绪的事实：只在没有同类阻断项时显示，避免同一行既「待补」又「就绪」。
      // Ready facts appear only when the same category has no blocker.
      const pending = readiness?.pending ?? []
      if (
        config !== null &&
        config.fields.provider.name !== '' &&
        !pending.some((item) => item.kind === 'model_connection' || item.kind === 'credential')
      ) {
        rows.push({
          key: 'model-ready',
          label: '模型连接',
          detail: `${config.fields.provider.name} / ${config.fields.provider.model}`,
          tone: 'ok',
          status: '就绪',
        })
      }
      const declared = config?.fields.mcp_servers ?? []
      const known = (props.mcps ?? []).filter((server) => declared.includes(server.name))
      const slots = known.reduce(
        (total, server) =>
          total +
          server.credential_keys.filter(
            (key) => key.state === 'managed' || key.state === 'env',
          ).length,
        0,
      )
      if (slots > 0 && !pending.some((item) => item.kind === 'credential')) {
        rows.push({
          key: 'credentials-ready',
          label: '服务凭据',
          detail: `${slots} 个槽位已配置（加密托管或环境变量）`,
          tone: 'ok',
          status: '就绪',
        })
      }
      if (config !== null) {
        if (config.notify_mode === 'none') {
          rows.push({
            key: 'notify-none',
            label: '通知渠道',
            detail: '此 Agent 不发送通知，无需配置。',
            tone: 'muted',
            status: '不需要',
          })
        } else if (!rows.some((row) => row.label.startsWith('通知渠道'))) {
          rows.push({
            key: 'notify-ready',
            label: '通知渠道',
            detail: NOTIFY_MODE_LABELS[config.notify_mode] ?? '通知意图未声明',
            tone: 'ok',
            status: '就绪',
          })
        }
      }
      if (props.configError !== '') {
        rows.push({
          key: 'config-error',
          label: 'Agent 定义',
          detail: `读取失败：${props.configError}`,
          tone: 'warn',
          status: '不可读',
        })
      }
      return panel('provide', '需要你提供', 'TO PROVIDE', rows)
    }

    /** 系统已准备：服务版本、MCP 组件、脚本与样例库。 */
    // What the system already has: service version, MCP components, assets.
    function preparedPanel(): VNode {
      const rows: ReadyRow[] = []
      // 响应可能来自测试桩或旧服务端：非对象按「不可读」处理，不读未校验字段。
      // Non-object payloads read as unavailable rather than rendering undefined.
      const info =
        props.info !== null && typeof props.info === 'object' && !Array.isArray(props.info)
          ? props.info
          : null
      if (info !== null) {
        rows.push({
          key: 'version',
          label: '服务版本',
          detail: `${info.version}（运行时组件随安装包提供）`,
          tone: 'ok',
          status: '就绪',
        })
      } else {
        rows.push({
          key: 'version',
          label: '服务版本',
          detail:
            props.infoError === ''
              ? '正在读取服务版本…'
              : `读取失败：${props.infoError}`,
          tone: 'muted',
          status: props.infoError === '' ? '读取中' : '不可读',
        })
      }

      const config = configOf(props.config)
      const declared = config?.fields.mcp_servers ?? []
      if (config === null) {
        rows.push({
          key: 'mcp-config',
          label: 'MCP 组件',
          detail: 'Agent 定义读取中或不可读，暂不能核对组件。',
          tone: 'muted',
          status: '暂不可读',
        })
      } else if (declared.length === 0) {
        rows.push({
          key: 'mcp-none',
          label: 'MCP 组件',
          detail: '此 Agent 未声明 MCP 依赖。',
          tone: 'muted',
          status: '不需要',
        })
      } else if (props.mcps === null) {
        rows.push({
          key: 'mcp-loading',
          label: 'MCP 组件',
          detail: '正在读取已发布的 MCP 服务…',
          tone: 'muted',
          status: '读取中',
        })
      } else {
        for (const name of declared) {
          const server = props.mcps.find((item) => item.name === name)
          if (server === undefined) {
            rows.push({
              key: `mcp-${name}`,
              label: `MCP ${name}`,
              detail: '未安装或未发布。',
              tone: 'bad',
              status: '未就绪',
              href: '/console/resources/mcp-servers',
            })
            continue
          }
          if (server.pending_restart) {
            rows.push({
              key: `mcp-${name}`,
              label: `MCP ${name}`,
              detail: `已发布 v${server.published_version_no}，运行中仍是 v${server.applied_version_no}，需重启服务后生效。`,
              tone: 'warn',
              status: '待重启',
              href: `/console/resources/mcp-servers/${encodeURIComponent(name)}`,
            })
            continue
          }
          rows.push({
            key: `mcp-${name}`,
            label: `MCP ${name}`,
            detail: server.connected
              ? `已连接 · 已应用 v${server.applied_version_no} · 提供 ${server.tools.length} 个工具`
              : `已应用 v${server.applied_version_no}，当前未连接；运行时会以分类失败暴露。`,
            tone: server.connected ? 'ok' : 'warn',
            status: server.connected ? '就绪' : '未连接',
            href: `/console/resources/mcp-servers/${encodeURIComponent(name)}`,
          })
        }
      }

      // 脚本与样例库：目录存在即「已就位」；不存在时按中性显示——控制台无法区分
      // 内置 Demo（缺失是缺陷）与自定义 Agent（本来就不要求这两个目录）。
      // Scripts and fixtures are neutral when absent: the console cannot tell a
      // built-in demo from a user-defined agent.
      for (const entry of [
        { key: 'scripts', path: 'scripts', label: '脚本' },
        { key: 'fixtures', path: 'fixtures', label: '样例库' },
      ]) {
        if (props.entries === null) {
          rows.push({
            key: entry.key,
            label: entry.label,
            detail:
              props.entriesError === ''
                ? '正在读取 Agent 目录…'
                : `读取失败：${props.entriesError}`,
            tone: 'muted',
            status: props.entriesError === '' ? '读取中' : '暂不可读',
          })
          continue
        }
        const found = props.entries.some(
          (item) => item.path === entry.path && item.type === 'dir',
        )
        rows.push({
          key: entry.key,
          label: entry.label,
          detail: found ? '已就位。' : '目录不存在（自定义 Agent 不要求）。',
          tone: found ? 'ok' : 'muted',
          status: found ? '就绪' : '无',
        })
      }
      return panel('prepared', '系统已准备', 'PREPARED', rows)
    }

    /** 未启用原因：一句话 + 跳转；提示项单独列，不使用阻断语气。 */
    // Why it is not enabled yet; advisory items are listed separately.
    function reasonPanel(): VNode {
      const readiness = readinessOf(props.readiness)
      const rows: ReadyRow[] = []
      if (props.readinessError !== '') {
        rows.push({
          key: 'reason-error',
          label: '无法确认启用前置',
          detail: `读取就绪状态失败：${props.readinessError}`,
          tone: 'bad',
          status: '不可读',
        })
      } else if (readiness === null) {
        rows.push({
          key: 'reason-loading',
          label: '正在确认',
          detail: '读取就绪状态后再判断能否启用。',
          tone: 'muted',
          status: '读取中',
        })
      } else if (readiness.pending.length > 0) {
        const fixes: string[] = []
        for (const item of readiness.pending) {
          if (item.fix !== '' && !fixes.includes(item.fix)) {
            fixes.push(item.fix)
          }
        }
        rows.push({
          key: 'reason-pending',
          label: `还差 ${readiness.pending.length} 项`,
          detail: '补全上方「需要你提供」中的项目后才能启用；启用时服务端会再核对一次。',
          tone: 'warn',
          status: '待补',
          ...(fixes.length === 0 ? {} : { href: fixes[0]! }),
        })
      } else {
        switch (props.lifecycle) {
          case 'active':
            rows.push({
              key: 'reason-active',
              label: '已启用',
              detail: '启用前置已满足，按计划运行。',
              tone: 'ok',
              status: '就绪',
            })
            break
          case 'disabled':
            rows.push({
              key: 'reason-disabled',
              label: '已停用',
              detail: '启用前置已满足；点击页头「启用 Agent」后开始运行。',
              tone: 'warn',
              status: '待启用',
            })
            break
          case 'archived':
            rows.push({
              key: 'reason-archived',
              label: '已归档',
              detail: '归档的 Agent 不再运行；可在归档页恢复。',
              tone: 'muted',
              status: '已归档',
            })
            break
          default:
            rows.push({
              key: 'reason-unknown',
              label: '状态文件不可读',
              detail: '无法确认生命周期；新运行会被拒绝。',
              tone: 'bad',
              status: '不可读',
            })
        }
      }
      const notices = readiness?.notices.filter((item) => item.kind !== 'notify_channel') ?? []
      notices.forEach((item, index) => rows.push(noticeRow(item, index)))
      return panel('reason', '未启用原因', 'NOT ENABLED', rows)
    }

    return (): VNode =>
      h('div', { class: 'ready-grid' }, [providePanel(), preparedPanel(), reasonPanel()])
  },
})
