// 归档视图（/console/archived）：列出 `agent_archives` 行并支持恢复——留空新名称 =
// 原名恢复（归档态条件迁移为停用、generation 递增）；填写新名称 = 异名恢复（Bootstrap
// 为停用、generation=1）。两条路径都默认停用并清空定时。恢复面板本身就是二次确认：
// 先选行、再核对目标名后提交，**不**做乐观更新——成功后进入恢复后的 Agent 详情
// （停用徽标可见），409 冲突原样展示服务端点名句子。
// Archived agents view: rows of agent_archives plus restore. An empty new name keeps
// the original directory with a conditional archived→disabled transition; a new name
// bootstraps generation=1. Both restore disabled and clear schedules. Success enters the
// restored Agent, whose detail page shows the disabled badge; no optimistic update.
import { defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import { adminApi } from '../api-admin'
import type { ArchivedAgentView } from '../api-admin'
import { DataTable, Notice, formatTimestamp, shortHash } from '../components/ui'
import type { TableRow } from '../components/ui'
import { agentHref } from '../router'
import {
  badge,
  definitions,
  identifier,
  pageHeading,
  section,
  useLoader,
  viewProps,
} from './shared'

/** 恢复失败的固定引导：409 是名称 / 状态冲突，其余交给服务端固定文案。 */
// A fixed lead-in for 409 conflicts; every other failure keeps the server message.
function restoreError(caught: unknown): string {
  const text = errorMessage(caught)
  if (caught instanceof ApiError && caught.status === 409) {
    return `恢复被拒绝：目标名称已被占用，或该归档的状态已变化。${text}`
  }
  return text
}

export const ArchivedAgentsView = defineComponent({
  name: 'ArchivedAgentsView',
  props: viewProps,
  setup(props) {
    const client = adminApi(props.ctx.api)
    const router = props.ctx.router
    const archives = useLoader(() => client.listArchivedAgents())

    const busy = ref(false)
    const error = ref('')
    const notice = ref('')
    // pending 是"已选中、等待二次确认"的归档行；null = 面板关闭。
    const pending = ref<ArchivedAgentView | null>(null)
    const newName = ref('')

    function openRestore(row: ArchivedAgentView): void {
      error.value = ''
      notice.value = ''
      newName.value = ''
      pending.value = row
    }

    function closeRestore(): void {
      pending.value = null
      newName.value = ''
    }

    /** 提交恢复：newName 留空 = 原名；成功后进入恢复后的 Agent（停用徽标可见）。 */
    // Submits the restore; an empty newName keeps the original directory name. Success
    // navigates to the restored Agent so its disabled state is shown by the detail page.
    async function confirmRestore(): Promise<void> {
      const row = pending.value
      if (row === null || busy.value) {
        return
      }
      const target = newName.value.trim()
      busy.value = true
      error.value = ''
      notice.value = ''
      try {
        const restored = await client.restoreArchivedAgent(
          row.archive_id,
          target === '' ? undefined : target,
        )
        closeRestore()
        const message =
          target === ''
            ? `已按原名恢复「${restored.agent}」；generation 已递增，默认停用、定时任务已清空。`
            : `已恢复为「${restored.agent}」；generation=1，默认停用、定时任务已清空。`
        if (!router.navigate(agentHref(restored.agent) + '?restored=1')) {
          // 跳转被拦下（守卫）时退回本页说明，并重新读取归档列表。
          // If navigation is blocked, stay truthful here and re-read the archives.
          notice.value = message
          await archives.reload()
        }
      } catch (caught) {
        error.value = restoreError(caught)
      } finally {
        busy.value = false
      }
    }

    /** 一行的状态列：未恢复可恢复，已恢复显示恢复目标与时间。 */
    // Status cell: restorable, or the restored target and time.
    function statusCell(row: ArchivedAgentView): VNodeChild {
      if (row.restored_at === '') {
        return badge('可恢复', 'ok')
      }
      const target = row.restored_agent_name === '' ? row.agent : row.restored_agent_name
      return h('div', { class: 'cell-stack' }, [
        badge('已恢复', 'muted'),
        h('small', `→ ${target} · ${formatTimestamp(row.restored_at)}`),
      ])
    }

    function archiveRows(data: readonly ArchivedAgentView[]): TableRow[] {
      return data.map((row) => ({
        key: row.archive_id,
        cells: [
          identifier(row.archive_id),
          identifier(row.agent),
          formatTimestamp(row.archived_at),
          h('code', { title: row.content_hash }, shortHash(row.content_hash)),
          statusCell(row),
          row.restored_at === ''
            ? h(
                'button',
                {
                  type: 'button',
                  disabled: busy.value,
                  onClick: () => {
                    openRestore(row)
                  },
                },
                '恢复',
              )
            : h('span', { class: 'muted small' }, '—'),
        ],
      }))
    }

    /** 恢复面板：选中行 + 目标名输入 + 明确写着目标名的确认按钮。 */
    // The restore panel doubles as the confirmation step and names the target.
    function renderRestorePanel(): VNodeChild {
      const row = pending.value
      if (row === null) {
        return null
      }
      const target = newName.value.trim()
      const effective = target === '' ? row.agent : target
      const renamed = target !== '' && target !== row.agent
      return section('恢复归档', [
        definitions([
          ['归档标识', identifier(row.archive_id)],
          ['原名称', identifier(row.agent)],
          ['目标名称', identifier(effective)],
          [
            '恢复方式',
            renamed
              ? '异名恢复：Bootstrap 为停用、generation=1，清空定时任务'
              : '原名恢复：条件迁移归档态 → 停用，generation 递增，清空定时任务',
          ],
        ]),
        h('label', { class: 'field' }, [
          h('span', '新名称（留空 = 原名）'),
          h('input', {
            type: 'text',
            value: newName.value,
            placeholder: row.agent,
            autocomplete: 'off',
            onInput: (event: Event) => {
              newName.value = (event.target as HTMLInputElement).value
            },
          }),
        ]),
        h('p', { class: 'muted small' }, '恢复后的 Agent 默认停用；原归档副本保留，不在这里物理清除。'),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: busy.value,
              onClick: () => {
                void confirmRestore()
              },
            },
            busy.value ? '恢复中…' : `确认恢复为「${effective}」`,
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value,
              onClick: closeRestore,
            },
            '取消',
          ),
        ]),
      ])
    }

    return (): VNode => {
      const data = archives.data.value
      const children: VNodeChild[] = []

      if (archives.error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: archives.error.value }))
      }
      if (error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: error.value }))
      }
      if (notice.value !== '') {
        children.push(h(Notice, { tone: 'info', text: notice.value }))
      }

      children.push(renderRestorePanel())

      children.push(
        section('归档记录', [
          data === null
            ? h('p', { class: 'muted' }, archives.loading.value ? '正在读取归档…' : '暂无归档')
            : h(DataTable, {
                headers: ['归档标识', '原名称', '归档时间', '内容哈希', '状态', '操作'],
                rows: archiveRows(data),
                empty: '还没有归档的 Agent；删除 Agent 会在这里留下可恢复的记录。',
                tableClass: 'archived-table',
                columnClasses: ['col-id', 'col-id', 'col-time', 'col-hash', 'col-state', 'col-actions'],
              }),
        ]),
      )

      return h('section', { class: 'view view-archived-agents' }, [
        pageHeading(
          'WORKSPACE / ARCHIVED',
          '已归档 Agents',
          '归档只把目录移出活动区：会话、记忆与审计保留；恢复默认停用。',
          h('span', { class: 'page-count' }, data === null ? '正在读取归档' : `${data.length} 条归档记录`),
        ),
        ...children,
      ])
    }
  },
})
