const STORAGE_KEY = 'sam_events_since'

/** 首次 since：localStorage 记住的上次，否则现在 − 1 天。 */
export function initialSince(now = new Date(), storage = globalThis.localStorage) {
  try {
    const saved = storage?.getItem?.(STORAGE_KEY)
    if (saved) {
      const t = Date.parse(saved)
      if (!Number.isNaN(t)) return new Date(t).toISOString()
    }
  } catch {
    /* ignore */
  }
  return new Date(now.getTime() - 24 * 60 * 60 * 1000).toISOString()
}

/** 本轮返回里最新事件时间（ISO）；无事件则返回 null。 */
export function newestCreatedAt(items) {
  let max = null
  for (const it of items || []) {
    const t = Date.parse(it?.created_at)
    if (Number.isNaN(t)) continue
    if (max == null || t > max) max = t
  }
  return max == null ? null : new Date(max).toISOString()
}

/**
 * 用本轮最新事件时间推进 since（API since 为 >=，边界事件可能再出现一次，靠本地去重兜底）。
 * 无新事件则保持原 since。
 */
export function advanceSince(prevSince, items, storage = globalThis.localStorage) {
  const newest = newestCreatedAt(items)
  if (!newest) return prevSince
  const prev = prevSince ? Date.parse(prevSince) : NaN
  const next = Date.parse(newest)
  const advanced = Number.isNaN(prev) || next > prev ? newest : prevSince
  try {
    storage?.setItem?.(STORAGE_KEY, advanced)
  } catch {
    /* ignore */
  }
  return advanced
}

/** 按 id 去重合并（新在前），兜底 since 边界重复。 */
export function mergeEvents(prev, incoming, limit = 50) {
  const seen = new Set()
  const out = []
  for (const it of [...(incoming || []), ...(prev || [])]) {
    if (it == null || it.id == null || seen.has(it.id)) continue
    seen.add(it.id)
    out.push(it)
    if (out.length >= limit) break
  }
  return out
}

/** 切换筛选时：清掉记住的 since，回到现在−1天，便于重拉近一天事件。 */
export function resetSince(now = new Date(), storage = globalThis.localStorage) {
  try {
    storage?.removeItem?.(STORAGE_KEY)
  } catch {
    /* ignore */
  }
  return initialSince(now, storage)
}

export const EVENTS_SINCE_KEY = STORAGE_KEY
