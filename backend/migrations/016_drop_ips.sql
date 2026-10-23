-- T28：不再保存 IP（不可逆，升级前先备份，见 docs/DEPLOY.md「升级」）。
-- DNS 解析结果只在内存里用于存活与泛解析判断；资产 changed 只看 CNAME。
ALTER TABLE assets DROP COLUMN IF EXISTS ips;
-- 历史事件 detail 里的 IP（new 事件的 ips、changed 的 changes.ips）一并去掉，播报不再出现 IP。
UPDATE events SET detail = detail - 'ips' - 'last_ips' WHERE detail ?| array['ips', 'last_ips'];
UPDATE events SET detail = jsonb_set(detail, '{changes}', (detail->'changes') - 'ips')
    WHERE jsonb_typeof(detail->'changes') = 'object' AND detail->'changes' ? 'ips';
