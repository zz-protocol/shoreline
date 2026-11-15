-- T51 返工（架构 11-15 定）：cdn_fronted 三态。NULL 表示未判定，不能按非 CDN 放行。
-- 已走过 DNS 判定的行保留 true/false；从未判定（resolved_at 为空且仍为默认 false）改回 NULL。
ALTER TABLE assets ALTER COLUMN cdn_fronted DROP NOT NULL;
ALTER TABLE assets ALTER COLUMN cdn_fronted SET DEFAULT NULL;
UPDATE assets SET cdn_fronted = NULL
	WHERE cdn_fronted IS NOT DISTINCT FROM false
	  AND resolved_at IS NULL;
