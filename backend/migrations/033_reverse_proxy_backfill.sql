-- T56：反代/CDN 边缘识别。HTTP 400/403 + 反代特征不算已确认服务，也不做未识别候选。
-- 只更新状态与归档（ignored_at），不删除任何组件；重复执行无副作用。
-- 特征：Server: openresty/nginx/Tengine、via: ...CHN-*、X-CCDN-FORBID-CODE、X-Cache、X-CDN。

-- 1) 历史 asset_services：banner 命中反代特征的行标 reverse_proxy（不再计入 verified）。
UPDATE asset_services
SET response_kind = 'reverse_proxy', verified = false
WHERE open
  AND banner <> ''
  AND response_kind <> 'reverse_proxy'
  AND banner ~* '^[[:space:]]*HTTP/[0-9]+(\.[0-9]+)?[[:space:]]+(400|403)([^0-9]|$)'
  AND banner ~* '(server[[:space:]]*:[[:space:]]*(openresty|nginx|tengine)|via[[:space:]]*:[[:space:]]*[^.]*CHN-|x-ccdn-forbid-code[[:space:]]*:|x-cache[[:space:]]*:|x-cdn[[:space:]]*:)';

-- 2) 历史未识别候选：证据 banner 命中同样特征 → 归档，保留组件行与关联。
UPDATE asset_components ac
SET ignored_at = now()
FROM components c
WHERE ac.component_id = c.id
  AND ac.ignored_at IS NULL
  AND ac.source = 'service'
  AND c.name LIKE 'unidentified-service-%'
  AND COALESCE(ac.evidence->>'banner', '') <> ''
  AND (ac.evidence->>'banner') ~* '^[[:space:]]*HTTP/[0-9]+(\.[0-9]+)?[[:space:]]+(400|403)([^0-9]|$)'
  AND (ac.evidence->>'banner') ~* '(server[[:space:]]*:[[:space:]]*(openresty|nginx|tengine)|via[[:space:]]*:[[:space:]]*[^.]*CHN-|x-ccdn-forbid-code[[:space:]]*:|x-cache[[:space:]]*:|x-cdn[[:space:]]*:)';
