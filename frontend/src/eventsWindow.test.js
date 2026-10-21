import { describe, expect, it } from 'vitest'
import { appendEvents, filterParams, maxId, prependEvents, windowRange } from './eventsWindow'

const now = new Date('2026-10-21T10:00:00Z')

describe('eventsWindow', () => {
  it('默认近 7 天，until 不设', () => {
    expect(windowRange('7d', null, now)).toEqual({ since: '2026-10-14T10:00:00.000Z', until: null })
    expect(windowRange(undefined, null, now).since).toBe('2026-10-14T10:00:00.000Z')
  })
  it('近 30 天 / 今日 / 自定义', () => {
    expect(windowRange('30d', null, now).since).toBe('2026-09-21T10:00:00.000Z')
    const t = new Date(windowRange('today', null, now).since)
    expect(t.getHours()).toBe(0)
    expect(t.getMinutes()).toBe(0)
    expect(windowRange('custom', ['2026-10-01T00:00:00Z', '2026-10-02T00:00:00Z'], now)).toEqual({
      since: '2026-10-01T00:00:00.000Z',
      until: '2026-10-02T00:00:00.000Z',
    })
    // 自定义未选全回落 7 天
    expect(windowRange('custom', [null, null], now).since).toBe('2026-10-14T10:00:00.000Z')
  })
  it('filterParams 列表/统计/复制共用同一时间窗', () => {
    const p = filterParams({ level: 'asset', vendor: 3, since: 'S', until: 'U' })
    expect(p.toString()).toBe('level=asset&type=new&vendor=3&since=S&until=U')
    expect(filterParams({ level: 'url' }).toString()).toBe('level=url&type=new')
  })
  it('轮询：新事件置顶、不替换旧列表、按 id 去重', () => {
    const old = [{ id: 5 }, { id: 4 }, { id: 3 }]
    expect(maxId(old)).toBe(5)
    expect(maxId([])).toBe(0)
    const out = prependEvents(old, [{ id: 7 }, { id: 6 }, { id: 5 }])
    expect(out.map((x) => x.id)).toEqual([7, 6, 5, 4, 3])
  })
  it('加载更多：追加到底部去重', () => {
    expect(appendEvents([{ id: 9 }, { id: 8 }], [{ id: 8 }, { id: 7 }]).map((x) => x.id)).toEqual([9, 8, 7])
  })
})
