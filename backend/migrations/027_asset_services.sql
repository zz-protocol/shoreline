-- T51：非 CDN 源站端口/服务指纹结果。按主机名+端口存储，不含 IP。SQL 须幂等。
CREATE TABLE IF NOT EXISTS asset_services (
	id         BIGSERIAL PRIMARY KEY,
	asset_id   BIGINT REFERENCES assets(id) ON DELETE CASCADE,
	host       TEXT NOT NULL,
	port       INTEGER NOT NULL CHECK (port > 0 AND port <= 65535),
	open       BOOLEAN NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	version    TEXT NOT NULL DEFAULT '',
	banner     TEXT NOT NULL DEFAULT '',
	probed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (host, port)
);
CREATE INDEX IF NOT EXISTS asset_services_asset_idx ON asset_services (asset_id);
CREATE INDEX IF NOT EXISTS asset_services_host_idx ON asset_services (host);
