export type HistoryPreset = 'all' | '1d' | '7d' | '30d' | 'custom'
export const MAX_HISTORY_MS = 30 * 86400000
export function historyBounds(preset: HistoryPreset, now: Date): {from: string; to: string} {
  if (preset === 'all') return {from: '', to: ''}
  const days = preset === '7d' ? 7 : preset === '30d' ? 30 : 1
  return {from: new Date(now.getTime() - days * 86400000).toISOString(), to: now.toISOString()}
}
export function initialHistoryRange(query: URLSearchParams, anchored: boolean, now: Date): {preset: HistoryPreset; from: string; to: string} {
  const raw = query.get('range')
  const allowed = ['1d', '7d', '30d', 'custom', ...(anchored ? ['all'] : [])]
  const preset = (raw && allowed.includes(raw) ? raw : query.has('from') || query.has('to') ? 'custom' : anchored ? 'all' : '1d') as HistoryPreset
  const defaults = historyBounds(preset, now)
  if (preset === 'all') return {preset, ...defaults}
  const end = query.get('to') || defaults.to
  const parsedEnd = Date.parse(end)
  const from = query.get('from') || (Number.isFinite(parsedEnd) ? new Date(parsedEnd - (preset === '7d' ? 7 : preset === '30d' ? 30 : 1) * 86400000).toISOString() : defaults.from)
  return {preset, from, to: end}
}
