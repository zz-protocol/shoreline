package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// 假 403 openresty / 400 nginx / 200 应用响应分类正确（T56）。
func TestT56EdgeResponseClassification(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want string
	}{
		{"403 openresty", "HTTP/1.1 403 Forbidden\r\nServer: openresty/1.21.4.1\r\n\r\n", serviceResponseReverseProxy},
		{"400 nginx", "HTTP/1.1 400 Bad Request\r\nServer: nginx/1.24.0\r\n\r\n", serviceResponseReverseProxy},
		{"403 tengine", "HTTP/1.1 403 Forbidden\r\nServer: Tengine\r\n\r\n", serviceResponseReverseProxy},
		{"403 via CHN", "HTTP/1.1 403 Forbidden\r\nVia: CHN-beijing-2\r\n\r\n", serviceResponseReverseProxy},
		{"403 ccdn", "HTTP/1.1 403 Forbidden\r\nX-CCDN-FORBID-CODE: 403\r\n\r\n", serviceResponseReverseProxy},
		{"403 x-cache", "HTTP/1.1 403 Forbidden\r\nX-Cache: HIT\r\n\r\n", serviceResponseReverseProxy},
		{"200 app", "HTTP/1.1 200 OK\r\nServer: nginx\r\nContent-Type: application/json\r\n\r\n{\"ok\":true}", serviceResponseBanner},
	}
	for _, c := range cases {
		for _, banner := range []string{c.resp, sanitizeServiceBanner([]byte(c.resp))} {
			if got := serviceResponseKind(true, banner, false); got != c.want {
				t.Fatalf("%s: kind=%q want %q (banner=%q)", c.name, got, c.want, banner)
			}
		}
	}
}

// 反代/CDN 边缘响应不生成候选，记 service_scan_skips.reason=reverse_proxy。
func TestT56ScanReverseProxyNoCandidate(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "edge." + domain
	aid := t51InsertAsset(t, e, host, false)

	banner := []byte("HTTP/1.1 403 Forbidden\r\nServer: openresty/1.21.4.1\r\nContent-Length: 0\r\n\r\n")
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306},
		RPS:     100,
		Timeout: time.Second,
		Dialer:  t51Dialer(new(atomic.Int32), map[int]string{3306: t51BannerOnConnect(t, banner)}),
	}
	rules, err := loadServiceFingerprintRules("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Rules = rules
	if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: aid, Host: host}}); err != nil {
		t.Fatal(err)
	}

	var kind string
	var verified bool
	if err := e.pool.QueryRow(ctx, `SELECT response_kind, verified FROM asset_services WHERE host = $1 AND port = 3306`, host).
		Scan(&kind, &verified); err != nil {
		t.Fatal(err)
	}
	if kind != serviceResponseReverseProxy || verified {
		t.Fatalf("kind=%q verified=%v, want reverse_proxy/false", kind, verified)
	}
	var skipped int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM service_scan_skips WHERE host = $1 AND reason = $2`, host, skipServiceScanReverseProxy).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if skipped == 0 {
		t.Fatal("reverse_proxy skip reason not recorded")
	}

	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}, rules); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_components WHERE asset_id = $1`, aid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reverse_proxy generated %d candidate(s)", n)
	}

	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_services WHERE host = $1`, host)
		e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE asset_id = $1`, aid)
		e.pool.Exec(context.Background(), `DELETE FROM service_scan_skips WHERE host = $1`, host)
	})
}

// 迁移 033 幂等回填：历史未识别候选按 banner 命中反代特征归档；200 应用不误伤。
func TestT56Migration033ReverseProxyBackfill(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	proxyHost := "h033-proxy." + domain
	appHost := "h033-app." + domain
	plainHost := "h033-plain." + domain
	proxyID := t51InsertAsset(t, e, proxyHost, false)
	appID := t51InsertAsset(t, e, appHost, false)
	plainID := t51InsertAsset(t, e, plainHost, false)

	proxyBanner := "HTTP/1.1 403 Forbidden..Server: openresty/1.21.4.1..Content-Length: 0...."
	appBanner := "HTTP/1.1 200 OK..Server: nginx..Content-Type: application/json....{}"
	plainBanner := "HTTP/1.1 403 Forbidden..Content-Type: text/html...."
	type fixture struct {
		name    string
		host    string
		assetID int64
		port    int
		banner  string
		kind    string
	}
	fixtures := []fixture{
		{"unidentified-service-aaaaaaaaaaaa", proxyHost, proxyID, 18081, proxyBanner, serviceResponseNone},
		{"unidentified-service-bbbbbbbbbbbb", appHost, appID, 18082, appBanner, serviceResponseBanner},
		{"unidentified-service-cccccccccccc", plainHost, plainID, 18083, plainBanner, serviceResponseBanner},
	}
	componentIDs := map[string]int64{}
	for _, f := range fixtures {
		var cid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed)
			VALUES ($1, $1, 'service', false) RETURNING id`, f.name).Scan(&cid); err != nil {
			t.Fatal(err)
		}
		componentIDs[f.name] = cid
		if _, err := e.pool.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, source, evidence)
			VALUES ($1, $2, $3, 'service', jsonb_build_object('banner', $4::text))`, f.assetID, cid, f.port, f.banner); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, verified, response_kind, banner)
			VALUES ($1, $2, $3, true, $4, $5, $6)`, f.assetID, f.host, f.port, f.kind == serviceResponseBanner, f.kind, f.banner); err != nil {
			t.Fatal(err)
		}
	}

	sql, err := migrationFS.ReadFile("migrations/033_reverse_proxy_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for i := 0; i < 2; i++ {
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration rerun %d: %v", i+1, err)
		}
	}

	ignored := func(cid int64) bool {
		var v bool
		if err := e.pool.QueryRow(ctx, `SELECT ignored_at IS NOT NULL FROM asset_components WHERE component_id = $1`, cid).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if !ignored(componentIDs[fixtures[0].name]) {
		t.Fatal("reverse_proxy candidate not ignored")
	}
	if ignored(componentIDs[fixtures[1].name]) {
		t.Fatal("200 application candidate must not be ignored")
	}
	if ignored(componentIDs[fixtures[2].name]) {
		t.Fatal("plain 403 without proxy headers must not be ignored")
	}
	var kind string
	var verified bool
	if err := e.pool.QueryRow(ctx, `SELECT response_kind, verified FROM asset_services WHERE host = $1`, proxyHost).Scan(&kind, &verified); err != nil {
		t.Fatal(err)
	}
	if kind != serviceResponseReverseProxy || verified {
		t.Fatalf("proxy service kind=%q verified=%v", kind, verified)
	}
	if err := e.pool.QueryRow(ctx, `SELECT response_kind, verified FROM asset_services WHERE host = $1`, appHost).Scan(&kind, &verified); err != nil {
		t.Fatal(err)
	}
	if kind != serviceResponseBanner || !verified {
		t.Fatalf("200 application kind=%q verified=%v, want banner/true", kind, verified)
	}

	t.Cleanup(func() {
		for _, f := range fixtures {
			e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE component_id = $1`, componentIDs[f.name])
		}
		e.pool.Exec(context.Background(), `DELETE FROM components WHERE name LIKE 'unidentified-service-%'
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = components.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = components.id)
			AND id = ANY($1)`, []int64{componentIDs[fixtures[0].name], componentIDs[fixtures[1].name], componentIDs[fixtures[2].name]})
		for _, f := range fixtures {
			e.pool.Exec(context.Background(), `DELETE FROM asset_services WHERE host = $1`, f.host)
		}
	})
}
