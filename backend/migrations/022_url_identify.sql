-- T45：识别面加深。只加列，不对历史资产对外补探测；新证据在新的 http 扫描（含复扫）写入。
ALTER TABLE urls ADD COLUMN IF NOT EXISTS http_headers JSONB NOT NULL DEFAULT '{}';
ALTER TABLE urls ADD COLUMN IF NOT EXISTS favicon_hash TEXT;
ALTER TABLE urls ADD COLUMN IF NOT EXISTS deep_probed_at TIMESTAMPTZ;
