-- T7：解析结果、泛解析与 CDN 标记
ALTER TABLE assets ADD COLUMN IF NOT EXISTS cnames TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE assets ADD COLUMN IF NOT EXISTS resolved BOOLEAN;          -- NULL 未解析过；false 解析失败
ALTER TABLE assets ADD COLUMN IF NOT EXISTS wildcard BOOLEAN NOT NULL DEFAULT false; -- 解析结果落在泛解析 IP 集合内
ALTER TABLE assets ADD COLUMN IF NOT EXISTS cdn TEXT;                  -- CDN 厂商名，NULL 表示未识别为 CDN
ALTER TABLE assets ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ;
