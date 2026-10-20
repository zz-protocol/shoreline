-- 回刷 004 之前写入的 assets.subdomain / urls.host（小写、去首尾空白和末尾点）。
-- 规范化后与已有记录重复时合并：sources 取并集，first_seen 取早、last_seen 取晚，
-- 下游引用（urls/events）改指保留记录后删除重复行，并以 NOTICE 报告。
DO $$
DECLARE
    r RECORD;
    keep BIGINT;
BEGIN
    -- 先处理 urls（assets 合并时改 urls.asset_id 会触发 urls 的规范化触发器）
    FOR r IN SELECT id, scheme, port, normalize_host(host) AS n FROM urls
             WHERE host <> normalize_host(host) ORDER BY id LOOP
        SELECT id INTO keep FROM urls WHERE scheme = r.scheme AND port = r.port AND host = r.n;
        IF keep IS NULL THEN
            UPDATE urls SET host = r.n WHERE id = r.id;
        ELSE
            UPDATE urls k SET sources = ARRAY(SELECT DISTINCT unnest(k.sources || d.sources) ORDER BY 1),
                              first_seen = least(k.first_seen, d.first_seen),
                              last_seen = greatest(k.last_seen, d.last_seen)
            FROM urls d WHERE k.id = keep AND d.id = r.id;
            UPDATE events SET url_id = keep WHERE url_id = r.id;
            DELETE FROM urls WHERE id = r.id;
            RAISE NOTICE 'backfill: merged url % into % (%://%:%)', r.id, keep, r.scheme, r.n, r.port;
        END IF;
    END LOOP;

    FOR r IN SELECT id, normalize_host(subdomain) AS n FROM assets
             WHERE subdomain <> normalize_host(subdomain) ORDER BY id LOOP
        SELECT id INTO keep FROM assets WHERE subdomain = r.n;
        IF keep IS NULL THEN
            UPDATE assets SET subdomain = r.n WHERE id = r.id;
        ELSE
            UPDATE assets k SET sources = ARRAY(SELECT DISTINCT unnest(k.sources || d.sources) ORDER BY 1),
                                first_seen = least(k.first_seen, d.first_seen),
                                last_seen = greatest(k.last_seen, d.last_seen)
            FROM assets d WHERE k.id = keep AND d.id = r.id;
            UPDATE urls SET asset_id = keep WHERE asset_id = r.id;
            UPDATE events SET asset_id = keep WHERE asset_id = r.id;
            DELETE FROM assets WHERE id = r.id;
            RAISE NOTICE 'backfill: merged asset % into % (%)', r.id, keep, r.n;
        END IF;
    END LOOP;
END $$;
