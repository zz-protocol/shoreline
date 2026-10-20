-- T9：新增事件每个资产/URL 只生成一次（重复扫描、并发、重试都不重复播报）
CREATE UNIQUE INDEX IF NOT EXISTS events_new_asset_uniq ON events (asset_id) WHERE type = 'new' AND url_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS events_new_url_uniq ON events (url_id) WHERE type = 'new' AND url_id IS NOT NULL;
