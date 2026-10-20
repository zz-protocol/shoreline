-- 入库层统一主机名：小写、去首尾空白和末尾点（root_domains.domain、assets.subdomain、urls.host）
CREATE OR REPLACE FUNCTION normalize_host(h TEXT) RETURNS TEXT
    LANGUAGE sql IMMUTABLE AS $$ SELECT rtrim(lower(btrim(h)), '.') $$;

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

UPDATE root_domains SET domain = normalize_host(domain) WHERE domain <> normalize_host(domain);
