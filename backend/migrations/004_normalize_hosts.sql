-- 入库层统一主机名：小写、去首尾空白和末尾点（root_domains.domain、assets.subdomain、urls.host）
CREATE OR REPLACE FUNCTION normalize_host(h TEXT) RETURNS TEXT
    LANGUAGE sql IMMUTABLE AS $$ SELECT rtrim(lower(btrim(h)), '.') $$;

-- 先合并规范化后重名的根域（如 Example.COM 与 example.com）：保留 id 最小者，
-- assets / scan_tasks 改指保留的根域后删除重复行，并以 NOTICE 报告；然后规范化剩余根域。
-- 合并期间临时停用这几张表的用户触发器，避免改指时触发主机名规范化造成连带冲突。
ALTER TABLE root_domains DISABLE TRIGGER USER;
ALTER TABLE assets DISABLE TRIGGER USER;
ALTER TABLE scan_tasks DISABLE TRIGGER USER;
DO $$
DECLARE
    r RECORD;
    keep BIGINT;
BEGIN
    FOR r IN SELECT id, vendor_id, normalize_host(domain) AS n FROM root_domains d
             WHERE EXISTS (SELECT 1 FROM root_domains o WHERE o.id < d.id AND normalize_host(o.domain) = normalize_host(d.domain))
             ORDER BY id LOOP
        SELECT id INTO keep FROM root_domains WHERE normalize_host(domain) = r.n ORDER BY id LIMIT 1;
        UPDATE assets SET root_id = keep WHERE root_id = r.id;
        UPDATE scan_tasks SET root_id = keep WHERE root_id = r.id;
        DELETE FROM root_domains WHERE id = r.id;
        RAISE NOTICE 'normalize: merged root_domain % into % (%)', r.id, keep, r.n;
    END LOOP;
    UPDATE root_domains SET domain = normalize_host(domain) WHERE domain <> normalize_host(domain);
END $$;
ALTER TABLE root_domains ENABLE TRIGGER USER;
ALTER TABLE assets ENABLE TRIGGER USER;
ALTER TABLE scan_tasks ENABLE TRIGGER USER;

CREATE OR REPLACE FUNCTION normalize_host_trigger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'root_domains' THEN
        NEW.domain := normalize_host(NEW.domain);
    ELSIF TG_TABLE_NAME = 'assets' THEN
        NEW.subdomain := normalize_host(NEW.subdomain);
    ELSE
        NEW.host := normalize_host(NEW.host);
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS normalize_host ON root_domains;
CREATE TRIGGER normalize_host BEFORE INSERT OR UPDATE ON root_domains FOR EACH ROW EXECUTE FUNCTION normalize_host_trigger();
DROP TRIGGER IF EXISTS normalize_host ON assets;
CREATE TRIGGER normalize_host BEFORE INSERT OR UPDATE ON assets FOR EACH ROW EXECUTE FUNCTION normalize_host_trigger();
DROP TRIGGER IF EXISTS normalize_host ON urls;
CREATE TRIGGER normalize_host BEFORE INSERT OR UPDATE ON urls FOR EACH ROW EXECUTE FUNCTION normalize_host_trigger();

