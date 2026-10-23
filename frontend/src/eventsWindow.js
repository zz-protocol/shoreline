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

/** 公共筛选参数（列表 / 统计 / 整列复制共用，保证同一时间窗与搜索词）。 */
export function filterParams({ level, vendor, since, until, hosted, types, q }) {
  const p = new URLSearchParams({ level, type: (types?.length ? types : ['new', 'recovered']).join(',') })
  if (hosted) p.set('include_customer_hosted', 'true')
  if (vendor) p.set('vendor', String(vendor))
  if (since) p.set('since', since)
  if (until) p.set('until', until)
  if (q?.trim()) p.set('q', q.trim())
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

/**
 * 轮询结果处理：after_id 取回的一页（limit 条）还有 next_cursor 说明两次轮询间新增超过一页，
 * 此时不合并（避免中间缺口），由页面提示「有更多新事件，点此刷新」。
 */
export function applyPoll(prev, page) {
  if (page?.next_cursor) return { items: prev, overflow: true }
  return { items: prependEvents(prev, page?.items), overflow: false }
}

/** 统计文案：「新增 X · 恢复 Y」。 */
/** 统计文案：只显示已选中的类型（默认新增+恢复），如「新增 4 · 恢复 1 · 变更 2」。 */
export function statsLabel(s, types = ['new', 'recovered']) {
  const order = ['new', 'recovered', 'changed']
  const sel = order.filter((t) => (types?.length ? types : ['new', 'recovered']).includes(t))
  return sel.map((t) => `${TYPE_LABEL[t]} ${s?.[t] ?? 0}`).join(' · ')
}
const TYPE_LABEL = { new: '新增', recovered: '恢复', changed: '变更' }

/** 事件类型标签：新增 / 恢复 / 变更。 */
export const TYPE_TAG = {
  new: { label: '新增', color: 'green' },
  recovered: { label: '恢复', color: 'gold' },
  changed: { label: '变更', color: 'blue' },
}
