-- T38：死亡原因。nxdomain=DNS 不解析；no_http=解析但 HTTP 不通；存活/未知为 NULL。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS dead_reason TEXT
    CHECK (dead_reason IS NULL OR dead_reason IN ('nxdomain', 'no_http'));
-- 回填：resolved=false → nxdomain；resolved=true 且已死 → no_http（不覆盖已有值；非 dead 清空）。
UPDATE assets SET dead_reason = 'nxdomain'
    WHERE NOT COALESCE(resolved, false) AND dead_reason IS NULL;
UPDATE assets SET dead_reason = 'no_http'
    WHERE resolved IS TRUE AND alive = 'dead' AND dead_reason IS NULL;
UPDATE assets SET dead_reason = NULL WHERE alive <> 'dead' AND dead_reason IS NOT NULL;
