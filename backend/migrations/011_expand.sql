-- T11：ICP/子公司扩展
-- 厂商工商主体全称（ICP 查询输入）
ALTER TABLE vendors ADD COLUMN IF NOT EXISTS company_name TEXT;

-- 子公司名单（手工导入或其他子公司源），持股比例为百分比
CREATE TABLE IF NOT EXISTS vendor_subsidiaries (
    id         BIGSERIAL PRIMARY KEY,
    vendor_id  BIGINT NOT NULL REFERENCES vendors(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    ratio      NUMERIC(5,2) NOT NULL CHECK (ratio >= 0 AND ratio <= 100),
    source     TEXT NOT NULL DEFAULT 'manual',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (vendor_id, name)
);

-- 待确认根域：不自动加入扫描，确认后才写入 root_domains；忽略的不再提示
CREATE TABLE IF NOT EXISTS root_candidates (
    id         BIGSERIAL PRIMARY KEY,
    vendor_id  BIGINT NOT NULL REFERENCES vendors(id) ON DELETE CASCADE,
    domain     TEXT NOT NULL UNIQUE CHECK (domain = normalize_host(domain)),
    sources    TEXT[] NOT NULL DEFAULT '{}',
    evidence   JSONB NOT NULL DEFAULT '[]',
    status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'confirmed', 'ignored')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS root_candidates_vendor_status_idx ON root_candidates (vendor_id, status);

-- 扫描类型加 expand（仅手动触发）
ALTER TABLE scan_tasks DROP CONSTRAINT IF EXISTS scan_tasks_kind_check;
ALTER TABLE scan_tasks ADD CONSTRAINT scan_tasks_kind_check CHECK (kind IN ('passive', 'active', 'dns', 'http', 'expand'));
