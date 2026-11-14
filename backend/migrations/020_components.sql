-- T43：开源组件库存。从现有 urls.tech 回填，升级后不必重扫。
CREATE TABLE IF NOT EXISTS components (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    name_key        TEXT NOT NULL UNIQUE,
    github_repo     TEXT,
    stars           INTEGER,
    advisory_count  INTEGER,
    source          TEXT NOT NULL DEFAULT 'wappalyzer',
    confirmed       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS url_components (
    url_id        BIGINT NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    component_id  BIGINT NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    version       TEXT NOT NULL DEFAULT '',
    source        TEXT NOT NULL DEFAULT 'wappalyzer',
    evidence      JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (url_id, component_id)
);
CREATE INDEX IF NOT EXISTS url_components_component_id_idx ON url_components (component_id);

-- 可编辑的常见组件名单；删除留墓碑，升级补种不会加回。
CREATE TABLE IF NOT EXISTS common_component_names (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    name_key    TEXT NOT NULL UNIQUE,
    builtin     BOOLEAN NOT NULL DEFAULT false,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    deleted_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 回填：wappalyzer 名称与版本以第一个 ':' 分隔；版本只作展示，不参与唯一键。
WITH parsed AS (
    SELECT u.id AS url_id,
           NULLIF(btrim(split_part(t, ':', 1)), '') AS name,
           NULLIF(btrim(substr(t, length(split_part(t, ':', 1)) + 2)), '') AS version,
           t AS raw
    FROM urls u
    CROSS JOIN LATERAL unnest(u.tech) AS t
    WHERE btrim(t) <> ''
)
INSERT INTO components (name, name_key, source, confirmed)
SELECT DISTINCT ON (lower(name)) name, lower(name), 'wappalyzer', true
FROM parsed
WHERE name IS NOT NULL
ORDER BY lower(name), name
ON CONFLICT (name_key) DO NOTHING;

WITH parsed AS (
    SELECT u.id AS url_id,
           NULLIF(btrim(split_part(t, ':', 1)), '') AS name,
           NULLIF(btrim(substr(t, length(split_part(t, ':', 1)) + 2)), '') AS version,
           t AS raw
    FROM urls u
    CROSS JOIN LATERAL unnest(u.tech) AS t
    WHERE btrim(t) <> ''
)
INSERT INTO url_components (url_id, component_id, version, source, evidence)
SELECT p.url_id, c.id, COALESCE(p.version, ''), 'wappalyzer', jsonb_build_object('tech', p.raw)
FROM parsed p
JOIN components c ON c.name_key = lower(p.name)
WHERE p.name IS NOT NULL
ON CONFLICT (url_id, component_id) DO UPDATE
    SET version = CASE WHEN EXCLUDED.version <> '' THEN EXCLUDED.version ELSE url_components.version END,
        evidence = EXCLUDED.evidence;
