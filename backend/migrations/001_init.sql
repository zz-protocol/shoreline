-- 厂商
CREATE TABLE IF NOT EXISTS vendors (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 根域
CREATE TABLE IF NOT EXISTS root_domains (
    id         BIGSERIAL PRIMARY KEY,
    vendor_id  BIGINT NOT NULL REFERENCES vendors(id) ON DELETE CASCADE,
    domain     TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 范围规则：include/exclude，pattern 支持通配（如 *.example.com）
CREATE TABLE IF NOT EXISTS scope_rules (
    id         BIGSERIAL PRIMARY KEY,
    vendor_id  BIGINT NOT NULL REFERENCES vendors(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('include', 'exclude')),
    pattern    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (vendor_id, kind, pattern)
);

-- 资产（子域名），唯一键：子域名
CREATE TABLE IF NOT EXISTS assets (
    id         BIGSERIAL PRIMARY KEY,
    root_id    BIGINT NOT NULL REFERENCES root_domains(id) ON DELETE CASCADE,
    subdomain  TEXT NOT NULL UNIQUE,
    ips        TEXT[] NOT NULL DEFAULT '{}',
    in_scope   BOOLEAN,
    sources    TEXT[] NOT NULL DEFAULT '{}',
    first_seen TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS assets_root_id_idx ON assets (root_id);
CREATE INDEX IF NOT EXISTS assets_first_seen_idx ON assets (first_seen);

-- URL，唯一键：scheme+host+port
CREATE TABLE IF NOT EXISTS urls (
    id               BIGSERIAL PRIMARY KEY,
    asset_id         BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    scheme           TEXT NOT NULL,
    host             TEXT NOT NULL,
    port             INT NOT NULL,
    status_code      INT,
    title            TEXT,
    fingerprint_hash TEXT,
    redirect_to      TEXT,
    sources          TEXT[] NOT NULL DEFAULT '{}',
    first_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scheme, host, port)
);
CREATE INDEX IF NOT EXISTS urls_asset_id_idx ON urls (asset_id);

-- 事件：new / changed
CREATE TABLE IF NOT EXISTS events (
    id         BIGSERIAL PRIMARY KEY,
    type       TEXT NOT NULL CHECK (type IN ('new', 'changed')),
    asset_id   BIGINT REFERENCES assets(id) ON DELETE CASCADE,
    url_id     BIGINT REFERENCES urls(id) ON DELETE CASCADE,
    detail     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS events_type_created_idx ON events (type, created_at);

-- 扫描任务
CREATE TABLE IF NOT EXISTS scan_tasks (
    id          BIGSERIAL PRIMARY KEY,
    vendor_id   BIGINT REFERENCES vendors(id) ON DELETE CASCADE,
    root_id     BIGINT REFERENCES root_domains(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('passive', 'dns', 'http')),
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'done', 'failed')),
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
