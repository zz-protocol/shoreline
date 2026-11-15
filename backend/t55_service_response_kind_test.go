package main

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestT55ResponseKindClassification(t *testing.T) {
	cases := []struct {
		name         string
		banner       string
		probe        bool
		wantKind     string
		wantVerified bool
	}{
		{"403 openresty", "HTTP/1.1 403 Forbidden\r\nServer: openresty/1.21.4.1\r\nContent-Length: 0\r\n\r\n", false, serviceResponseReverseProxy, false},
		{"400 nginx", "HTTP/1.1 400 Bad Request\r\nServer: nginx\r\nContent-Length: 0\r\n\r\n", false, serviceResponseReverseProxy, false},
		{"403 via CHN", "HTTP/1.1 403 Forbidden\r\nVia: CHN-shanghai-3\r\nContent-Length: 0\r\n\r\n", false, serviceResponseReverseProxy, false},
		{"403 ccdn", "HTTP/1.1 403 Forbidden\r\nX-CCDN-FORBID-CODE: 403\r\nContent-Length: 0\r\n\r\n", false, serviceResponseReverseProxy, false},
		{"403 x-cache", "HTTP/1.1 403 Forbidden\r\nX-Cache: HIT\r\nContent-Length: 0\r\n\r\n", false, serviceResponseReverseProxy, false},
		{"200 nginx app", "HTTP/1.1 200 OK\r\nServer: nginx\r\nContent-Type: application/json\r\n\r\n{}", false, serviceResponseBanner, true},
		{"403 plain app", "HTTP/1.1 403 Forbidden\r\nContent-Type: text/html\r\n\r\n", false, serviceResponseBanner, true},
		{"404 openresty", "HTTP/1.1 404 Not Found\r\nServer: openresty\r\n\r\n", false, serviceResponseBanner, true},
		{"probe response", "+PONG", true, serviceResponseProbe, true},
		{"tcp only", "", false, serviceResponseNone, false},
	}
	for _, c := range cases {
		for _, banner := range []string{c.banner, sanitizeServiceBanner([]byte(c.banner))} {
			if c.banner == "" {
				banner = ""
			}
			kind := serviceResponseKind(true, banner, c.probe)
			if kind != c.wantKind {
				t.Fatalf("%s: kind=%q want %q (banner=%q)", c.name, kind, c.wantKind, banner)
			}
			if got := serviceResponseVerified(kind); got != c.wantVerified {
				t.Fatalf("%s: verified=%v want %v", c.name, got, c.wantVerified)
			}
		}
	}
	if got := serviceResponseKind(false, "HTTP/1.1 200 OK", false); got != serviceResponseNone {
		t.Fatalf("closed port kind=%q want none", got)
	}
}

// 空 banner 只算 TCP 可达：open=true、verified=false、response_kind=none，且不生成组件候选。
func TestT55EmptyBannerNotVerifiedOrComponent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "empty-banner." + domain
	aid := t51InsertAsset(t, e, host, false)

	empty := t51Listen(t, func(c net.Conn) { _ = c.Close() })
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306},
		RPS:     100,
		Timeout: 300 * time.Millisecond,
		Dialer:  t51Dialer(new(atomic.Int32), map[int]string{3306: empty}),
	}
	rules, err := loadServiceFingerprintRules("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Rules = rules
	if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: aid, Host: host}}); err != nil {
		t.Fatal(err)
	}

	var open, verified bool
	var kind string
	if err := e.pool.QueryRow(ctx, `SELECT open, verified, response_kind FROM asset_services WHERE host = $1 AND port = 3306`, host).
		Scan(&open, &verified, &kind); err != nil {
		t.Fatal(err)
	}
	if !open || verified || kind != serviceResponseNone {
		t.Fatalf("empty banner open=%v verified=%v kind=%q, want true/false/none", open, verified, kind)
	}

	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}, rules); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_components WHERE asset_id = $1`, aid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("empty banner generated %d component candidate(s)", n)
	}

	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_services WHERE host = $1`, host)
		e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE asset_id = $1`, aid)
	})
}

// 迁移 032 可重跑：旧行默认 none/false；启动回填后有 banner 的行变为 verified。
func TestT55Migration032RerunAndBackfill(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	sql, err := migrationFS.ReadFile("migrations/032_service_response_kind.sql")
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
	var cols int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'asset_services' AND column_name IN ('response_kind', 'verified')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 2 {
		t.Fatalf("asset_services response columns=%d want 2", cols)
	}

	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	hosts := []string{"legacy-empty." + domain, "legacy-proxy." + domain, "legacy-banner." + domain}
	for i, host := range hosts {
		banner := ""
		if i == 1 {
			banner = sanitizeServiceBanner([]byte("HTTP/1.1 403 Forbidden\r\nServer: openresty/1.21.4.1\r\nContent-Length: 0\r\n\r\n"))
		} else if i == 2 {
			banner = "+PONG"
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (host, port, open, banner) VALUES ($1, $2, true, $3)`,
			host, 30000+i, banner); err != nil {
			t.Fatal(err)
		}
	}
	// 旧行由默认值回填。
	var kind string
	var verified bool
	if err := e.pool.QueryRow(ctx, `SELECT response_kind, verified FROM asset_services WHERE host = $1`, hosts[0]).
		Scan(&kind, &verified); err != nil {
		t.Fatal(err)
	}
	if kind != serviceResponseNone || verified {
		t.Fatalf("legacy row kind=%q verified=%v, want none/false", kind, verified)
	}

	n, err := backfillServiceResponseKinds(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("classified rows=%d want 2", n)
	}
	var verifiedCnt int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_services WHERE host = ANY($1) AND verified`, hosts).Scan(&verifiedCnt); err != nil {
		t.Fatal(err)
	}
	var bannerCnt int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_services WHERE host = ANY($1)
		AND open AND banner <> '' AND response_kind <> 'reverse_proxy'`, hosts).Scan(&bannerCnt); err != nil {
		t.Fatal(err)
	}
	if verifiedCnt != bannerCnt || verifiedCnt != 1 {
		t.Fatalf("verified=%d want equal to banner/probe rows=%d (1)", verifiedCnt, bannerCnt)
	}
	if err := e.pool.QueryRow(ctx, `SELECT response_kind, verified FROM asset_services WHERE host = $1`, hosts[1]).
		Scan(&kind, &verified); err != nil {
		t.Fatal(err)
	}
	if kind != serviceResponseReverseProxy || verified {
		t.Fatalf("proxy row kind=%q verified=%v, want reverse_proxy/false", kind, verified)
	}
	if n, err := backfillServiceResponseKinds(ctx, e.pool); err != nil || n != 0 {
		t.Fatalf("backfill rerun n=%d err=%v, want 0", n, err)
	}

	t.Cleanup(func() {
		for _, host := range hosts {
			e.pool.Exec(context.Background(), `DELETE FROM asset_services WHERE host = $1`, host)
		}
	})
}

// REST 列表/详情/统计返回 verified 与 response_kind；默认开放数只计 verified，另保留 TCP 可达数。
func TestT55ServiceResultsAPI(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	host := "svc-results." + domain
	aid := t51InsertAsset(t, e, host, false)
	rows := []struct {
		port     int
		open     bool
		verified bool
		kind     string
		name     string
	}{
		{3306, true, true, serviceResponseBanner, "mysql"},
		{5432, true, true, serviceResponseProbe, "postgresql"},
		{6379, true, false, serviceResponseNone, ""},
		{9200, true, false, serviceResponseReverseProxy, ""},
	}
	for _, r := range rows {
		if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services
			(asset_id, host, port, open, verified, response_kind, name, banner)
			VALUES ($1, $2, $3, $4, $5, $6, $7, '')`, aid, host, r.port, r.open, r.verified, r.kind, r.name); err != nil {
			t.Fatal(err)
		}
	}
	out := c.want(200, "GET", "/api/service-results?q="+host, nil)
	t51NoIP(t, out, "GET /api/service-results")
	stats := out["stats"].(map[string]any)
	wantStats := map[string]float64{
		"open": 2, "verified": 2, "tcp_reachable": 4, "named": 2, "reverse_proxy": 1, "total": 4,
	}
	for k, want := range wantStats {
		if got, _ := stats[k].(float64); got != want {
			t.Fatalf("stats[%s]=%v want %v (%v)", k, stats[k], want, stats)
		}
	}
	list := items(out)
	if len(list) != 4 {
		t.Fatalf("items=%d want 4", len(list))
	}
	for _, it := range list {
		if _, ok := it["verified"].(bool); !ok {
			t.Fatalf("item missing verified: %v", it)
		}
		if _, ok := it["response_kind"].(string); !ok {
			t.Fatalf("item missing response_kind: %v", it)
		}
		if _, leak := it["banner"]; leak {
			t.Fatalf("list must not return banner: %v", it)
		}
	}
	id := int64(list[0]["id"].(float64))
	detail := c.want(200, "GET", "/api/service-results/"+strconv.FormatInt(id, 10), nil)
	t51NoIP(t, detail, "GET /api/service-results/{id}")
	if _, ok := detail["verified"].(bool); !ok {
		t.Fatalf("detail missing verified: %v", detail)
	}
	c.want(400, "GET", "/api/service-results?response_kind=bogus", nil)

	asset := c.want(200, "GET", "/api/assets/"+strconv.FormatInt(aid, 10), nil)
	t51NoIP(t, asset, "GET /api/assets/{id}")
	services, _ := asset["services"].([]any)
	if len(services) != 4 {
		t.Fatalf("asset detail services=%d want 4 (%v)", len(services), asset["services"])
	}

	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_services WHERE host = $1`, host)
	})
}
