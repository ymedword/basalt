// 压缩记录面板（US-16 §3.1.9 / T089）：挂进会话详情与运行详情，只读展示该范围
// **当前 `chain_epoch`** 的压缩链。四条口径：
//
//  1. **只读**：面板不渲染任何触发 / 重试 / 删除按钮——压缩由引擎在运行中决定，
//     契约也没有手动触发端点。按钮"禁用着放在那里"同样会暗示可操作，所以不放。
//  2. **失败必须显示降级原因**：`failed` / `stale` 条目的 `reason_code` 与
//     `stale_at` 一并展示；把失败行隐去会让"压缩坏了"看起来像"从没压缩过"。
//  3. **两个引用集合分开计数**：`source_refs`（审计集）与 `included_refs`（当前
//     依赖集）各列一列，不合并成一个布尔——注入资格只看后者。
//  4. **摘要是派生视图**：正文默认折叠，展开处注明它不是消息原文、也不是引用依据；
//     引用必须回来源视图逐条校验。
//
// The compaction panel is read-only by construction: no trigger, retry or delete
// control exists. Failures keep their reason codes, the two reference sets stay
// separate, and the summary is labelled as a derived view rather than a citation.
import { defineComponent, h } from 'vue'
import type { PropType, VNode, VNodeChild } from 'vue'
import { CompactionApi } from '../api-compaction'
import type { CompactionStatus, CompactionView } from '../api-compaction'
import { DataTable, Notice, formatTimestamp } from '../components/ui'
import type { TableRow } from '../components/ui'
import { badge, section, useLoader } from './shared'
import type { ConsoleContext } from './shared'

const STATUS_LABELS: Record<CompactionStatus, string> = {
  committed: '已提交',
  failed: '失败',
  stale: '已失效',
}

function statusTone(status: CompactionStatus): 'ok' | 'bad' | 'warn' {
  if (status === 'committed') {
    return 'ok'
  }
  return status === 'failed' ? 'bad' : 'warn'
}

/** 覆盖区间：`覆盖 a–b → 保留自 c`；切点是最后一条被覆盖的消息。 */
// The covered span and the first kept message, as the contract names them.
function coverage(entry: CompactionView): string {
  return `覆盖 ${entry.covered_from_message_id}–${entry.cut_message_id} → 保留自 ${entry.first_kept_message_id}`
}

/** 状态列：徽标 + 原因码；失效条目附失效时间。 */
// Status plus the reason code; stale entries also carry their stale_at.
function statusCell(entry: CompactionView): VNodeChild {
  const parts: VNodeChild[] = [badge(STATUS_LABELS[entry.status], statusTone(entry.status))]
  if (entry.reason_code !== '') {
    parts.push(h('span', { class: 'muted small' }, ` ${entry.reason_code}`))
  }
  if (entry.stale_at !== '') {
    parts.push(h('span', { class: 'muted small' }, ` · ${formatTimestamp(entry.stale_at)} 失效`))
  }
  return h('div', { class: 'compaction-status' }, parts)
}

/** 用量列：模型 + 压缩前后 token；未记录时为破折号（不写 0）。 */
// Model and token delta; unrecorded values render as a dash, never as zero.
function usageCell(entry: CompactionView): VNodeChild {
  const model = entry.model === '' ? '—' : entry.model
  const tokens =
    entry.tokens_before === 0 && entry.tokens_after === 0
      ? '用量未记录'
      : `${entry.tokens_before} → ${entry.tokens_after} tokens`
  return h('div', [h('code', { title: entry.model }, model), h('div', { class: 'muted small' }, tokens)])
}

/** 引用列：两个集合各自计数（**不**合并成一个布尔）。 */
// The two reference sets are counted separately and never collapsed into one flag.
function refsCell(entry: CompactionView): VNodeChild {
  return h('div', { class: 'muted small' }, [
    `审计 ${entry.source_refs.length} 条`,
    h('br'),
    `当前依赖 ${entry.included_refs.length} 条`,
  ])
}

/** 摘要列：默认折叠；展开处声明它是派生视图。 */
// The summary is collapsed by default and labelled as a derived view when expanded.
function summaryCell(entry: CompactionView): VNodeChild {
  const text = entry.summary_view.summary
  if (text === '') {
    return h('span', { class: 'muted small' }, entry.status === 'failed' ? '未产出摘要（失败）' : '无摘要')
  }
  return h('details', { class: 'compaction-summary' }, [
    h('summary', entry.summary_view.truncated ? '查看摘要（截断）' : '查看摘要'),
    h('p', { class: 'muted small' }, '派生视图：不是消息原文，也不是引用依据；引用请回来源视图逐条校验。'),
    h('p', { class: 'summary-text' }, text),
  ])
}

/** 表格：列为 条目 / 代次 / 覆盖区间 / 状态 / 模型与用量 / 策略与提示 / 引用 / 创建时间 / 摘要。 */
// The panel table; every contract field the card asks for has a column.
function chainTable(entries: readonly CompactionView[]): VNode {
  const rows: TableRow[] = entries.map((entry) => ({
    key: entry.compaction_id,
    cells: [
      h('code', { class: 'cell-id', title: entry.compaction_id }, entry.compaction_id),
      String(entry.chain_epoch),
      coverage(entry),
      statusCell(entry),
      usageCell(entry),
      h('div', { class: 'muted small' }, [
        `策略 ${entry.strategy_version === '' ? '—' : entry.strategy_version}`,
        h('br'),
        `提示 ${entry.prompt_version === '' ? '—' : entry.prompt_version}`,
      ]),
      refsCell(entry),
      formatTimestamp(entry.created_at),
      summaryCell(entry),
    ],
  }))
  return h(DataTable, {
    headers: ['条目', '代次', '覆盖区间', '状态', '模型与用量', '策略与提示', '引用', '创建时间', '摘要'],
    rows,
    empty: '当前范围没有压缩记录',
    tableClass: 'compaction-table',
  })
}

export const CompactionPanel = defineComponent({
  name: 'CompactionPanel',
  props: {
    ctx: { type: Object as PropType<ConsoleContext>, required: true },
    // `sessionId` 与 `runId` **恰给一个**：两个都给或不给都是契约 400，由调用方保证。
    // Exactly one scope id is supplied by the embedding view.
    sessionId: { type: String, default: '' },
    runId: { type: String, default: '' },
  },
  setup(props) {
    const api = new CompactionApi(props.ctx.api)
    const chain = useLoader(() =>
      api.listChain({ sessionId: props.sessionId, runId: props.runId }),
    )

    return (): VNode => {
      const children: VNodeChild[] = [
        h('p', { class: 'muted small' }, [
          '只读派生视图：压缩由引擎在运行中触发，此处不提供手动触发；',
          '摘要不等于消息原文，引用请回来源视图逐条校验。',
        ]),
      ]
      if (chain.error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: chain.error.value }))
      }
      if (chain.loading.value && chain.data.value === null) {
        children.push(h('p', { class: 'muted' }, '加载中…'))
      }
      const entries = chain.data.value
      if (entries !== null) {
        children.push(chainTable(entries))
        const failed = entries.filter((entry) => entry.status === 'failed')
        if (failed.length > 0) {
          children.push(
            h(Notice, {
              tone: 'warn',
              text: `本次已记录 ${failed.length} 次压缩失败；失败不阻断对话，但摘要不会进入上下文。`,
            }),
          )
        }
      }
      return section('压缩记录', children)
    }
  },
})
