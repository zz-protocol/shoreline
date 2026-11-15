-- T50：CDN 前置标记。不存解析 IP。已有 cdn 非空的资产视为 cdn_fronted。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS cdn_fronted BOOLEAN NOT NULL DEFAULT false;
UPDATE assets SET cdn_fronted = true WHERE COALESCE(cdn, '') <> '' AND cdn_fronted IS DISTINCT FROM true;
