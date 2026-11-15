-- T57：未识别服务候选哈希归一化。哈希前去掉动态字段，再合并同类候选。
-- 归一化与 backend/service_inventory.go 的 normalizeServiceBannerForHash 保持一致：
--   按 CRLF（脱敏后为连续点）切段，丢掉 Date / Request_id / Node_info / Client_ip / Content-Length / via 段，再用 ".." 拼回。
-- 只合并关联，保留 confirmed / ignored 状态；不删除组件行；重复执行无副作用。

DROP TABLE IF EXISTS pg_temp.t57_unidentified_recluster;
CREATE TEMP TABLE t57_unidentified_recluster ON COMMIT DROP AS
SELECT ac.asset_id,
       ac.port,
       ac.component_id AS old_component_id,
       'unidentified-service-' || left(encode(sha256(convert_to(n.normalized, 'UTF8')), 'hex'), 12) AS new_name
FROM asset_components ac
JOIN components c ON c.id = ac.component_id
CROSS JOIN LATERAL (
    SELECT COALESCE(string_agg(p.part, '..' ORDER BY p.ord), '') AS normalized
    FROM regexp_split_to_table(COALESCE(ac.evidence->>'banner', ''), '\.{2,}') WITH ORDINALITY AS p(part, ord)
    WHERE p.part <> ''
      AND p.part !~* '^[[:space:]]*(date|request[-_]?id|node[-_]?info|client[-_]?ip|content-length|via)[[:space:]]*:'
) n
WHERE ac.source = 'service'
  AND c.name LIKE 'unidentified-service-%'
  AND COALESCE(ac.evidence->>'banner', '') <> ''
  -- 反代/CDN 边缘响应由 T56 归档，不参与重新聚类。
  AND NOT (
    (ac.evidence->>'banner') ~* '^[[:space:]]*HTTP/[0-9]+(\.[0-9]+)?[[:space:]]+(400|403)([^0-9]|$)'
    AND (ac.evidence->>'banner') ~* '(server[[:space:]]*:[[:space:]]*(openresty|nginx|tengine)|via[[:space:]]*:[[:space:]]*[^.]*CHN-|x-ccdn-forbid-code[[:space:]]*:|x-cache[[:space:]]*:|x-cdn[[:space:]]*:)'
  );

-- 新名字对应的组件：已有则合并 confirmed（只升不降），没有则新建为未确认候选。
INSERT INTO components (name, name_key, source, confirmed)
SELECT r.new_name, lower(r.new_name), 'service', bool_or(c.confirmed)
FROM t57_unidentified_recluster r
JOIN components c ON c.id = r.old_component_id
GROUP BY r.new_name
ON CONFLICT (name_key) DO UPDATE
    SET confirmed = components.confirmed OR EXCLUDED.confirmed,
        updated_at = now();

-- 把旧关联搬到新组件；同一资产+端口已有目标关联时合并，保留最早的 ignored_at。
WITH moved AS (
    SELECT r.asset_id,
           nc.id AS component_id,
           r.port,
           (array_agg(ac.version ORDER BY ac.created_at DESC))[1] AS version,
           (array_agg(ac.source ORDER BY ac.created_at DESC))[1] AS source,
           (array_agg(ac.evidence ORDER BY ac.created_at DESC))[1] AS evidence,
           min(ac.ignored_at) AS ignored_at,
           min(ac.created_at) AS created_at
    FROM t57_unidentified_recluster r
    JOIN asset_components ac ON ac.asset_id = r.asset_id AND ac.port = r.port AND ac.component_id = r.old_component_id
    JOIN components nc ON nc.name_key = lower(r.new_name)
    WHERE nc.id <> r.old_component_id
    GROUP BY r.asset_id, nc.id, r.port
)
INSERT INTO asset_components (asset_id, component_id, port, version, source, evidence, ignored_at, created_at)
SELECT asset_id, component_id, port, version, source, evidence, ignored_at, created_at
FROM moved
ON CONFLICT (asset_id, component_id, port) DO UPDATE
    SET version = CASE WHEN EXCLUDED.version <> '' THEN EXCLUDED.version ELSE asset_components.version END,
        evidence = EXCLUDED.evidence,
        ignored_at = LEAST(asset_components.ignored_at, EXCLUDED.ignored_at);

-- 旧关联已经合并到新组件后删除旧行（组件行本身保留）。
DELETE FROM asset_components ac
USING t57_unidentified_recluster r, components nc
WHERE ac.asset_id = r.asset_id
  AND ac.port = r.port
  AND ac.component_id = r.old_component_id
  AND nc.name_key = lower(r.new_name)
  AND nc.id <> r.old_component_id;
