-- T52：非 Web 服务并入组件库存。按资产+端口关联，证据不含 IP。SQL 须幂等。
CREATE TABLE IF NOT EXISTS asset_components (
	asset_id      BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
	component_id  BIGINT NOT NULL REFERENCES components(id) ON DELETE CASCADE,
	port          INTEGER NOT NULL CHECK (port > 0 AND port <= 65535),
	version       TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL DEFAULT 'service',
	evidence      JSONB NOT NULL DEFAULT '{}',
	ignored_at    TIMESTAMPTZ,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (asset_id, component_id, port)
);
CREATE INDEX IF NOT EXISTS asset_components_component_idx ON asset_components (component_id);
