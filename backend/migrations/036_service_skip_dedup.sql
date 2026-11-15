-- T58：service_scan_skips 去重。同一资产同一原因只保留一行，用 count 累计轮次；
-- asset_id 为空（超范围等主机级跳过）时按 host + reason 去重。可重跑：先补列、合并旧行，再建唯一索引。
ALTER TABLE service_scan_skips ADD COLUMN IF NOT EXISTS scan_task_id BIGINT REFERENCES scan_tasks(id) ON DELETE SET NULL;
ALTER TABLE service_scan_skips ADD COLUMN IF NOT EXISTS first_seen TIMESTAMPTZ;
ALTER TABLE service_scan_skips ADD COLUMN IF NOT EXISTS last_seen TIMESTAMPTZ;
ALTER TABLE service_scan_skips ADD COLUMN IF NOT EXISTS count BIGINT;

-- 旧行回填：first_seen/last_seen 取原 created_at，count 为 1。
UPDATE service_scan_skips
	SET first_seen = COALESCE(first_seen, created_at),
		last_seen = COALESCE(last_seen, created_at),
		count = COALESCE(count, 1);

-- 合并历史重复行：保留最小 id，count 为合并行数，first/last 取极值。重复执行时无重复行即为空操作。
WITH g AS (
	SELECT min(id) AS keep_id,
		count(*) AS n,
		min(created_at) AS first_ts,
		max(created_at) AS last_ts
	FROM service_scan_skips
	GROUP BY COALESCE(asset_id, 0), CASE WHEN asset_id IS NULL THEN host ELSE '' END, reason
	HAVING count(*) > 1
)
UPDATE service_scan_skips s
	SET count = g.n, first_seen = g.first_ts, last_seen = g.last_ts
	FROM g WHERE s.id = g.keep_id;

WITH ranked AS (
	SELECT id, row_number() OVER (
			PARTITION BY COALESCE(asset_id, 0), CASE WHEN asset_id IS NULL THEN host ELSE '' END, reason
			ORDER BY id) AS rn
	FROM service_scan_skips
)
DELETE FROM service_scan_skips s USING ranked r WHERE s.id = r.id AND r.rn > 1;

ALTER TABLE service_scan_skips ALTER COLUMN first_seen SET DEFAULT now();
ALTER TABLE service_scan_skips ALTER COLUMN last_seen SET DEFAULT now();
ALTER TABLE service_scan_skips ALTER COLUMN count SET DEFAULT 1;
ALTER TABLE service_scan_skips ALTER COLUMN first_seen SET NOT NULL;
ALTER TABLE service_scan_skips ALTER COLUMN last_seen SET NOT NULL;
ALTER TABLE service_scan_skips ALTER COLUMN count SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS service_scan_skips_asset_reason_key
	ON service_scan_skips (asset_id, reason) WHERE asset_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS service_scan_skips_host_reason_key
	ON service_scan_skips (host, reason) WHERE asset_id IS NULL;
CREATE INDEX IF NOT EXISTS service_scan_skips_reason_idx ON service_scan_skips (reason);
