-- T8 修正：父域存在泛解析（DNS 随机子域探测有解析）。此类主机即使 IP 未被判为泛解析，也须经 HTTP 页面相似度复核。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS parent_wildcard BOOLEAN NOT NULL DEFAULT false;
-- T9 补充：HTTP 页面复核为真实主机的时间；有效期内（WILDCARD_VERIFIED_TTL，默认 7 天）DNS 不再重标 wildcard。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS verified_real_at TIMESTAMPTZ;
