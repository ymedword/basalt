import type { AgentPublishView } from '../api'

/**
 * 资产发布失败的页面兜底文案（2026-11-09 用户裁决，修订 D-25-10）：写已生效、
 * 发布未完成时，下一次 Run 会被准入拒绝；服务端通常随响应给出同一含义的固定
 * `error`，这里只服务"跳转后只剩状态"或响应缺 `error` 的场合。
 * Fixed fallback when a write response only carries the failure status.
 */
export const publishFailedFallback =
  '变更已保存，但资产发布未完成：该 Agent 的下一次运行会被准入拒绝。请重新保存；仍未恢复时执行 basalt assets install --agent <名称> 重新发布。'

/**
 * 服务端发布结果 → 页面警告文案：只在 `failed` 时给出；`published` / `skipped` /
 * 省略（只读读取）一律无警告。保存本身成功，失败的是发布——两者必须分开显示。
 * Only a failed publication produces a warning; the write itself stays successful.
 */
export function publishWarningText(publish: AgentPublishView | undefined): string {
  return publish?.status === 'failed' ? publish.error ?? publishFailedFallback : ''
}

/**
 * 复制 / 恢复成功后会跳到新 Agent 的页面：把发布失败作为到达参数带过去，
 * 让到达页显示同一条警告（发布结果不落库，URL 是唯一可传递的载体）。
 * Carries a failed publication across the copy/restore redirect.
 */
export function publishFailedQuery(publish: AgentPublishView | undefined): string {
  return publish?.status === 'failed' ? '&publish=failed' : ''
}
