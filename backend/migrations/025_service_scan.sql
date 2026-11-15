-- T49：非 Web 探测授权范围与全局开关；默认关闭。不存 IP。SQL 须幂等（schema_migrations 清空后可重跑）。
CREATE TABLE IF NOT EXISTS service_scan_settings (
	id INT PRIMARY KEY CHECK (id = 1),
	enabled BOOLEAN NOT NULL DEFAULT false,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS service_scan_scopes (
	id BIGSERIAL PRIMARY KEY,
	pattern TEXT NOT NULL UNIQUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS service_scan_skips (
	id BIGSERIAL PRIMARY KEY,
	host TEXT NOT NULL,
	asset_id BIGINT REFERENCES assets(id) ON DELETE CASCADE,
	reason TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS service_scan_skips_host_idx ON service_scan_skips (host, created_at DESC);
