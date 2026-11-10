// 技能发布联动重发的逐 Agent 结果（US-25C §2.3.7 / FR-25C-17）。发布本身
// 成功与否看版本号；联动重发是它的**后续动作**，逐 Agent 给出 ok / skipped /
// superseded / missing / failed——missing 与 failed 是告警，其余三类不是失败：
// superseded 表示复查时技能指针已被更新的版本覆盖，本次不写，由控制面修复器
// 在后续轮次有界重试。页面不得把「已发布」与「所有依赖 Agent 都重发成功」混成
// 一句话。
// Per-agent results of the US-25C skill-publish dependent republish.
import type { AgentRepublishView } from '../api-admin'

/** 状态标签：页面文案固定中文，未知状态按告警处理（fail-closed 显示）。 */
export const REPUBLISH_STATUS_LABELS: Record<AgentRepublishView['status'], string> = {
  ok: '已重发',
  skipped: '无需重发',
  superseded: '已被更新的版本超越',
  missing: '定义缺失',
  failed: '重发失败',
}

/** 两种必须告警的状态：未写入且需要人看一眼（修复器会重试，但页面要说清）。 */
const REPUBLISH_ALERT_STATUSES = new Set<AgentRepublishView['status']>(['missing', 'failed'])

/**
 * 逐 Agent 结果的展示行。未知状态（服务端未来扩展）按告警标签显示原值，
 * 不静默丢弃、也不假装成功。
 * Projects each result into a display row; unknown statuses surface verbatim.
 */
export function republishRows(
  publish: AgentRepublishView[] | undefined,
): Array<{ agent: string; label: string; error: string; alert: boolean }> {
  if (publish === undefined) {
    return []
  }
  return publish.map((item) => {
    const known = item.status in REPUBLISH_STATUS_LABELS
    const label = known ? REPUBLISH_STATUS_LABELS[item.status] : item.status
    return {
      agent: item.agent,
      label,
      error: item.error ?? '',
      alert: !known || REPUBLISH_ALERT_STATUSES.has(item.status),
    }
  })
}

/**
 * 告警文案：只列 missing / failed（含未知状态）的 Agent 与固定原因；没有告警
 * 返回空串。文案只陈述事实与后续动作，不承诺「稍后一定成功」。
 * Warning text for missing/failed dependents; empty when nothing needs attention.
 */
export function republishWarningText(publish: AgentRepublishView[] | undefined): string {
  const rows = republishRows(publish).filter((row) => row.alert)
  if (rows.length === 0) {
    return ''
  }
  const detail = rows
    .map((row) => (row.error === '' ? `${row.agent}（${row.label}）` : `${row.agent}（${row.label}：${row.error}）`))
    .join('；')
  return `技能已发布，但以下依赖 Agent 的重发未完成：${detail}。控制面修复器会在后续轮次自动重试；持续失败会转为阻塞，需人工处理。`
}

/**
 * 联动结果汇总：`共 N 个依赖 Agent：已重发 a、无需重发 b、已被超越 c`——只列
 * 非零项；未触发联动（undefined / 空数组）返回空串，不渲染「0 个」。
 * Summary line of the republish batch; empty when no republish was triggered.
 */
export function republishSummaryText(publish: AgentRepublishView[] | undefined): string {
  const rows = republishRows(publish)
  if (rows.length === 0) {
    return ''
  }
  const counts = new Map<string, number>()
  for (const row of rows) {
    counts.set(row.label, (counts.get(row.label) ?? 0) + 1)
  }
  const parts: string[] = []
  for (const [label, count] of counts) {
    parts.push(`${label} ${count}`)
  }
  return `联动重发：共 ${rows.length} 个依赖 Agent（${parts.join('、')}）`
}
