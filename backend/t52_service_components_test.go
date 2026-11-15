package main

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestT52ServiceInventoryFromAssetServices(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "db." + domain
	aid := t51InsertAsset(t, e, host, false)
	if _, err := e.pool.Exec(ctx, `UPDATE assets SET alive = 'alive' WHERE id = $1`, aid); err != nil {
		t.Fatal(err)
	}
	banner := append([]byte{0x0a}, []byte("8.0.36\x00mysql_native_password")...)
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306, 19999},
		RPS:     100,
		Timeout: time.Second,
		Dialer: func(ctx context.Context, network, h string, port int) (net.Conn, error) {
			if port != 3306 {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", t51BannerOnConnect(t, banner))
		},
	}
	rates := map[string]rate.Limit{"passive": 100, "active": 100, "dns": 100, "http": 100, "expand": 100, "service": 100}
	e.start(t, testConfig(rates), map[string]ScanHandler{"service": serviceScanHandler(e.pool, cfg)})

	var before int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM components`).Scan(&before)
	c.want(202, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "service"})
	e.waitStatus(t, "service", "done", 1)

	var name, source string
	var confirmed bool
	var cats []string
	if err := e.pool.QueryRow(ctx, `SELECT c.name, c.source, c.confirmed, c.categories
		FROM components c
		JOIN asset_components ac ON ac.component_id = c.id
		WHERE ac.asset_id = $1 AND ac.port = 3306`, aid).Scan(&name, &source, &confirmed, &cats); err != nil {
		t.Fatal(err)
	}
	if name != "MySQL" || source != "service" || !confirmed {
		t.Fatalf("component name=%q source=%q confirmed=%v", name, source, confirmed)
	}
	if !contains(cats, "Databases") {
		t.Fatalf("categories %v want Databases", cats)
	}

	// 正式列表可见，整列复制含该名；响应无 IP。
	list := items(c.want(200, "GET", "/api/components?include_wildcard=true&include_customer_hosted=true&include_dead=true", nil))
	t51NoIP(t, map[string]any{"items": toAny(list)}, "GET /api/components")
	if !contains(namesOf(list), "MySQL") {
		t.Fatalf("official list missing MySQL: %v", namesOf(list))
	}
	vals := c.want(200, "GET", "/api/components/values?field=name&include_wildcard=true&include_customer_hosted=true&include_dead=true", nil)
	t51NoIP(t, vals, "values")
	found := false
	for _, it := range vals["items"].([]any) {
		if it == "MySQL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("values copy missing MySQL: %v", vals["items"])
	}

	// 下钻到资产，不含 IP。
	var cid int64
	e.pool.QueryRow(ctx, `SELECT id FROM components WHERE name_key = 'mysql'`).Scan(&cid)
	assets := c.want(200, "GET", "/api/components/"+strconv.FormatInt(cid, 10)+"/assets?include_wildcard=true&include_customer_hosted=true&include_dead=true", nil)
	t51NoIP(t, assets, "assets")
	gotHosts := field(items(assets), "subdomain")
	if !contains(gotHosts, host) {
		t.Fatalf("drill-down missing host %s: %v", host, gotHosts)
	}

	// 未识别 banner 归档为 confirmed=false，不进默认列表；确认后入库。
	unkHost := "unk." + domain
	uid := t51InsertAsset(t, e, unkHost, false)
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'alive' WHERE id = $1`, uid)
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, name, version, banner)
		VALUES ($1, $2, 9200, true, '', '', 'weird-banner-no-rule')`, uid, unkHost); err != nil {
		t.Fatal(err)
	}
	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: uid, Host: unkHost}}); err != nil {
		t.Fatal(err)
	}
	var uname string
	var uconf bool
	if err := e.pool.QueryRow(ctx, `SELECT c.name, c.confirmed FROM components c
		JOIN asset_components ac ON ac.component_id = c.id WHERE ac.asset_id = $1`, uid).Scan(&uname, &uconf); err != nil {
		t.Fatal(err)
	}
	if uconf || !strings.HasPrefix(uname, "unidentified-service-") {
		t.Fatalf("unidentified name=%q confirmed=%v", uname, uconf)
	}
	official := namesOf(items(c.want(200, "GET", "/api/components?include_wildcard=true&include_customer_hosted=true&include_dead=true", nil)))
	if contains(official, uname) {
		t.Fatalf("unidentified leaked into official list: %v", official)
	}
	cands := items(c.want(200, "GET", "/api/component-candidates", nil))
	t51NoIP(t, map[string]any{"items": toAny(cands)}, "candidates")
	var candID int64
	for _, it := range cands {
		if it["name"] == uname {
			candID = int64(it["id"].(float64))
		}
	}
	if candID == 0 {
		t.Fatalf("unidentified not in candidates: %v", namesOf(cands))
	}
	c.want(200, "POST", "/api/components/"+strconv.FormatInt(candID, 10)+"/confirm", nil)
	if !contains(namesOf(items(c.want(200, "GET", "/api/components?include_wildcard=true&include_customer_hosted=true&include_dead=true", nil))), uname) {
		t.Fatal("confirmed unidentified not in official list")
	}

	// 不覆盖已有分类 / 不删已识别组件。
	if _, err := e.pool.Exec(ctx, `UPDATE components SET categories = '{Kept}' WHERE name_key = 'mysql'`); err != nil {
		t.Fatal(err)
	}
	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}); err != nil {
		t.Fatal(err)
	}
	var cats2 []string
	e.pool.QueryRow(ctx, `SELECT categories FROM components WHERE name_key = 'mysql'`).Scan(&cats2)
	if !contains(cats2, "Kept") {
		t.Fatalf("T47 categories overwritten: %v", cats2)
	}
	var after int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM components`).Scan(&after)
	if after < before {
		t.Fatalf("components deleted: before=%d after=%d", before, after)
	}

	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE asset_id IN ($1,$2)`, aid, uid)
		e.pool.Exec(context.Background(), `DELETE FROM components c
			WHERE c.id IN ($1,$2)
			  AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)
			  AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)`, cid, candID)
	})
}

func toAny(list []map[string]any) []any {
	out := make([]any, len(list))
	for i, m := range list {
		out[i] = m
	}
	return out
}
