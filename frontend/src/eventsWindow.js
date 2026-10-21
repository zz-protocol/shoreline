/** 时间窗 → [since, until)（ISO）。today=当天 0 点起；7d/30d=现在往前；custom=[开始, 结束]。until 为空表示不设上限（轮询可接新事件）。 */
export function windowRange(win, custom, now = new Date()) {
  const day = 24 * 60 * 60 * 1000
  if (win === 'today') {
    const d = new Date(now)
    d.setHours(0, 0, 0, 0)
    return { since: d.toISOString(), until: null }
  }
  if (win === '30d') return { since: new Date(now.getTime() - 30 * day).toISOString(), until: null }
  if (win === 'custom' && custom?.[0] && custom?.[1]) {
    return { since: new Date(custom[0]).toISOString(), until: new Date(custom[1]).toISOString() }
  }
  return { since: new Date(now.getTime() - 7 * day).toISOString(), until: null }
}

/** 公共筛选参数（列表 / 统计 / 整列复制共用，保证同一时间窗）。 */
export function filterParams({ level, vendor, since, until }) {
  const p = new URLSearchParams({ level, type: 'new' })
  if (vendor) p.set('vendor', String(vendor))
  if (since) p.set('since', since)
  if (until) p.set('until', until)
  return p
}

/** 列表中最大 id，作为轮询 after_id 游标。 */
export function maxId(items) {
  let m = 0
  for (const it of items || []) if (it?.id > m) m = it.id
  return m
}

/** 新事件放顶部，旧列表保留（不替换），按 id 去重。 */
export function prependEvents(prev, incoming) {
  const seen = new Set()
  const out = []
  for (const it of [...(incoming || []), ...(prev || [])]) {
    if (it == null || it.id == null || seen.has(it.id)) continue
    seen.add(it.id)
    out.push(it)
  }
  return out
}

/** 追加下一页（滚动/加载更多），按 id 去重。 */
export function appendEvents(prev, page) {
  return prependEvents([], [...(prev || []), ...(page || [])])
}
