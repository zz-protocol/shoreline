-- T44：组件冷门分。富集失败标记不挡列表；stars / advisory_count 已在 020 建表。
ALTER TABLE components ADD COLUMN IF NOT EXISTS enrich_error TEXT;
ALTER TABLE components ADD COLUMN IF NOT EXISTS enriched_at TIMESTAMPTZ;
