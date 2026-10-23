-- T30：网页配置的测绘 API key（FOFA/Quake/Hunter）；明文仅存库内，API 只返回掩码
CREATE TABLE IF NOT EXISTS probe_keys (
    provider   TEXT PRIMARY KEY CHECK (provider IN ('fofa', 'quake', 'hunter')),
    key_value  TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
