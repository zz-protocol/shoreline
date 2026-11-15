-- T55：探测结果三态。空 banner 只代表 TCP 可达，不算已确认开放服务。
-- response_kind: none | banner | probe | reverse_proxy；verified 只在收到 banner/probe 响应时为 true。
-- 旧行由列默认值回填为 verified=false、response_kind='none'；重复执行不覆盖新扫描结果。
ALTER TABLE asset_services ADD COLUMN IF NOT EXISTS response_kind TEXT NOT NULL DEFAULT 'none';
ALTER TABLE asset_services ADD COLUMN IF NOT EXISTS verified BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS asset_services_verified_idx ON asset_services (verified, probed_at DESC);
