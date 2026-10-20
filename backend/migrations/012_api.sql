-- T12：API token（只存 sha256，不分权限）
CREATE TABLE IF NOT EXISTS api_tokens (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

-- 改范围规则后的 in_scope 异步重算任务
CREATE TABLE IF NOT EXISTS scope_tasks (
    id          BIGSERIAL PRIMARY KEY,
    vendor_id   BIGINT NOT NULL REFERENCES vendors(id) ON DELETE CASCADE,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'done', 'failed')),
    processed   INT NOT NULL DEFAULT 0,
    changed     INT NOT NULL DEFAULT 0,
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS events_asset_id_idx ON events (asset_id);
