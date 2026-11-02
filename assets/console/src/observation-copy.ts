// 检索过滤与工具拒绝是不同事实；旧诊断页与OBS时间线使用同一口径。
// Retrieval filtering and tool refusal are different facts shared by both diagnostic views.
export function isVisibilityFilter(kind: string, reason: string): boolean {
  return kind === 'retrieval' && reason === 'permission_denied'
}

export function observationReasonLabel(kind: string, reason: string, fallback: string): string {
  return isVisibilityFilter(kind, reason) ? '已按访问范围筛选' : fallback
}

export function visibilityFilterNote(value: unknown): string {
  const count = typeof value === 'number' && Number.isFinite(value) && value >= 0 ? String(value) : '未记录'
  return `混合过滤计数 ${count}；统计包含不同口径，不能换算成文档数量或当前查询命中数。`
}
