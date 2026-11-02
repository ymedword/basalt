import { describe, expect, it } from 'vitest'
import { historyBounds, initialHistoryRange, MAX_HISTORY_MS } from '../src/observation-range'

describe('explicit observation history range', () => {
  const now = new Date('2026-11-02T02:00:00Z')
  it('defaults aggregation to a visible one-day range and keeps anchored history complete', () => {
    const aggregate = initialHistoryRange(new URLSearchParams(), false, now)
    expect(aggregate.preset).toBe('1d')
    expect(aggregate.from).toBe('2026-11-01T02:00:00.000Z')
    expect(aggregate.to).toBe(now.toISOString())
    expect(initialHistoryRange(new URLSearchParams(), true, now)).toEqual({preset: 'all', from: '', to: ''})
  })
  it('supports seven and thirty days without unbounded or implicit all-history aggregation', () => {
    expect(Date.parse(historyBounds('7d', now).to) - Date.parse(historyBounds('7d', now).from)).toBe(7 * 86400000)
    expect(Date.parse(historyBounds('30d', now).to) - Date.parse(historyBounds('30d', now).from)).toBe(MAX_HISTORY_MS)
    expect(initialHistoryRange(new URLSearchParams('range=all'), false, now).preset).toBe('1d')
  })
  it('preserves custom deep links and leaves malformed times for validation instead of crashing', () => {
    const custom = initialHistoryRange(new URLSearchParams('from=2026-10-31T00:00:00Z&to=2026-11-01T00:00:00Z'), false, now)
    expect(custom.preset).toBe('custom')
    expect(custom.from).toBe('2026-10-31T00:00:00Z')
    expect(() => initialHistoryRange(new URLSearchParams('to=invalid'), false, now)).not.toThrow()
  })
})
