-- T9：新增事件每个资产/URL 只生成一次（重复扫描、并发、重试都不重复播报）。
-- 建索引前先去重：同一资产（或 URL）的多条 new 只保留 id 最小的一条。
DELETE FROM events e USING events o
 WHERE e.type = 'new' AND o.type = 'new' AND e.id > o.id
   AND e.asset_id IS NOT DISTINCT FROM o.asset_id AND e.url_id IS NOT DISTINCT FROM o.url_id;
CREATE UNIQUE INDEX IF NOT EXISTS events_new_asset_uniq ON events (asset_id) WHERE type = 'new' AND url_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS events_new_url_uniq ON events (url_id) WHERE type = 'new' AND url_id IS NOT NULL;
