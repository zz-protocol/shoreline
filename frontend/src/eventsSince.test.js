import { describe, it, expect, beforeEach } from 'vitest'
import { advanceSince, initialSince, mergeEvents, newestCreatedAt, resetSince, EVENTS_SINCE_KEY } from './eventsSince'

function mem() {
  const m = new Map()
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
    _m: m,
  }
}

describe('events since', () => {
  it('initialSince uses now-1d when empty', () => {
    const now = new Date('2026-10-21T08:00:00.000Z')
    const s = mem()
    expect(initialSince(now, s)).toBe('2026-10-20T08:00:00.000Z')
  })

  it('initialSince restores localStorage', () => {
    const s = mem()
    s.setItem(EVENTS_SINCE_KEY, '2026-10-21T01:02:03.000Z')
    expect(initialSince(new Date(), s)).toBe('2026-10-21T01:02:03.000Z')
  })

  it('advanceSince moves to newest created_at and persists', () => {
    const s = mem()
    const prev = '2026-10-21T00:00:00.000Z'
    const items = [
      { id: 1, created_at: '2026-10-21T00:10:00.000Z' },
      { id: 2, created_at: '2026-10-21T00:30:00.000Z' },
      { id: 3, created_at: '2026-10-21T00:20:00.000Z' },
    ]
    const next = advanceSince(prev, items, s)
    expect(next).toBe('2026-10-21T00:30:00.000Z')
    expect(s.getItem(EVENTS_SINCE_KEY)).toBe('2026-10-21T00:30:00.000Z')
    // 再拉一批更新的
    const next2 = advanceSince(next, [{ id: 4, created_at: '2026-10-21T01:00:00.000Z' }], s)
    expect(next2).toBe('2026-10-21T01:00:00.000Z')
    expect(newestCreatedAt([])).toBeNull()
    expect(advanceSince(next2, [], s)).toBe(next2)
  })

  it('does not move since backwards', () => {
    const s = mem()
    const prev = '2026-10-21T02:00:00.000Z'
    expect(advanceSince(prev, [{ id: 1, created_at: '2026-10-21T01:00:00.000Z' }], s)).toBe(prev)
  })

  it('resetSince clears storage and returns now-1d', () => {
    const s = mem()
    s.setItem(EVENTS_SINCE_KEY, '2026-10-21T01:02:03.000Z')
    const now = new Date('2026-10-21T08:00:00.000Z')
    expect(resetSince(now, s)).toBe('2026-10-20T08:00:00.000Z')
    expect(s.getItem(EVENTS_SINCE_KEY)).toBeNull()
    // 清掉后再 initialSince 也应是现在−1天（不再读到旧值）
    expect(initialSince(now, s)).toBe('2026-10-20T08:00:00.000Z')
  })

  it('mergeEvents dedups by id (local fallback)', () => {
    const a = [{ id: 2, created_at: 't2' }, { id: 1, created_at: 't1' }]
    const b = [{ id: 3, created_at: 't3' }, { id: 2, created_at: 't2' }]
    expect(mergeEvents(a, b).map((x) => x.id)).toEqual([3, 2, 1])
  })
})
