package main

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func resetServiceScan(t *testing.T, e *testEnv) {
	t.Helper()
	t.Setenv("SERVICE_SCAN_ENABLED", "")
	ctx := context.Background()
	wipe := func() {
		e.pool.Exec(context.Background(), `DELETE FROM service_scan_skips`)
		e.pool.Exec(context.Background(), `DELETE FROM service_scan_scopes`)
		e.pool.Exec(context.Background(), `DELETE FROM service_scan_settings`)
	}
	wipe()
	t.Cleanup(wipe)
	_ = ctx
}

func TestT49HostInServiceScope(t *testing.T) {
	if hostInServiceScope("app.example.test", []string{"example.test"}) != true {
		t.Fatal("domain should cover itself and subdomains")
	}
	if hostInServiceScope("example.test", []string{"example.test"}) != true {
		t.Fatal("apex should be in scope")
	}
	if hostInServiceScope("other.example.test", []string{"app.example.test"}) {
		t.Fatal("sibling must not match a more specific domain")
	}
	if hostInServiceScope("example.test", []string{"*.example.test"}) {
		t.Fatal("*.domain should not include the apex")
	}
	if !hostInServiceScope("www.example.test", []string{"*.example.test"}) {
		t.Fatal("*.domain should match one or more subdomain labels")
	}
	if hostInServiceScope("example.test", nil) {
		t.Fatal("empty scope must deny")
	}
	if hostInServiceScope(net.IPv4(127, 0, 0, 1).String(), []string{"example.test"}) {
		t.Fatal("IP must not be authorized")
	}
}

func TestT49DefaultOffZeroProbe(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	var dials atomic.Int32
	probe := func(context.Context, string) error { dials.Add(1); return nil }
	targets := []serviceScanTarget{
		{Host: "a." + strings.ToLower(t.Name()) + ".example.test"},
		{Host: "b." + strings.ToLower(t.Name()) + ".example.test"},
	}
	if err := runServiceScan(ctx, e.pool, targets, probe); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("default-off must not probe, dials=%d", dials.Load())
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM service_scan_skips`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("disabled path should not record per-host skips, got %d", n)
	}
}

func TestT49EnabledOutOfScopeSkipsAndInScopeProbes(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	inHost := "in." + strings.ToLower(t.Name()) + ".example.test"
	outHost := "out." + strings.ToLower(t.Name()) + ".example.test"
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_scopes (pattern) VALUES ($1)`, inHost); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	var seen []string
	probe := func(_ context.Context, host string) error {
		dials.Add(1)
		seen = append(seen, host)
		return nil
	}
	if err := runServiceScan(ctx, e.pool, []serviceScanTarget{{Host: outHost}, {Host: inHost}}, probe); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 || len(seen) != 1 || seen[0] != inHost {
		t.Fatalf("only in-scope host should be probed, dials=%d seen=%v", dials.Load(), seen)
	}
	var reason, host string
	if err := e.pool.QueryRow(ctx, `SELECT host, reason FROM service_scan_skips`).Scan(&host, &reason); err != nil {
		t.Fatal(err)
	}
	if host != outHost || reason != "out_of_scope" {
		t.Fatalf("skip got host=%s reason=%s", host, reason)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM service_scan_skips`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("skip rows %d", n)
	}
}

func TestT49EmptyScopeDeniesWhenEnabled(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	host := "none." + strings.ToLower(t.Name()) + ".example.test"
	if err := runServiceScan(ctx, e.pool, []serviceScanTarget{{Host: host}}, func(context.Context, string) error {
		dials.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("empty scope must not probe, dials=%d", dials.Load())
	}
	var reason string
	if err := e.pool.QueryRow(ctx, `SELECT reason FROM service_scan_skips WHERE host = $1`, host).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "out_of_scope" {
		t.Fatalf("reason %s", reason)
	}
}

func TestT49EnvEnablesWhenNoDBRow(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	t.Setenv("SERVICE_SCAN_ENABLED", "true")
	ctx := context.Background()
	host := "env." + strings.ToLower(t.Name()) + ".example.test"
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_scopes (pattern) VALUES ($1)`, host); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	if err := runServiceScan(ctx, e.pool, []serviceScanTarget{{Host: host}}, func(context.Context, string) error {
		dials.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("env true with no settings row should probe in-scope, dials=%d", dials.Load())
	}
	// 库中显式关闭后环境变量不得再打开
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, false)`); err != nil {
		t.Fatal(err)
	}
	dials.Store(0)
	if err := runServiceScan(ctx, e.pool, []serviceScanTarget{{Host: host}}, func(context.Context, string) error {
		dials.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("DB off must win over env, dials=%d", dials.Load())
	}
}

func TestT49ServiceScanRESTAndMCP(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	resetServiceScan(t, e)
	anon := &apiClient{t: t, srv: c.srv, tok: ""}
	anon.want(401, "GET", "/api/service-scan", nil)

	out := c.want(200, "GET", "/api/service-scan", nil)
	if out["enabled"] != false {
		t.Fatalf("default enabled=%v", out["enabled"])
	}
	if _, ok := out["items"].([]any); !ok {
		t.Fatalf("items %v", out["items"])
	}
	if len(items(out)) != 0 {
		t.Fatalf("default scopes %v", items(out))
	}
	if _, ok := out["ip"]; ok {
		t.Fatal("API must not return ip")
	}

	c.want(400, "PATCH", "/api/service-scan", map[string]any{})
	c.want(400, "POST", "/api/service-scan/scopes", map[string]any{"pattern": "not a domain"})
	c.want(400, "POST", "/api/service-scan/scopes", map[string]any{"pattern": net.IPv4(127, 0, 0, 1).String()})

	pat := strings.ToLower(t.Name()) + ".example.test"
	created := c.want(201, "POST", "/api/service-scan/scopes", map[string]any{"pattern": strings.ToUpper(pat)})
	if created["pattern"] != pat {
		t.Fatalf("pattern not normalized: %v", created)
	}
	c.want(409, "POST", "/api/service-scan/scopes", map[string]any{"pattern": pat})
	id := int64(created["id"].(float64))

	on := c.want(200, "PATCH", "/api/service-scan", map[string]any{"enabled": true})
	if on["enabled"] != true {
		t.Fatalf("enabled after patch: %v", on)
	}
	got := items(on)
	if len(got) != 1 || got[0]["pattern"] != pat {
		t.Fatalf("scopes %v", got)
	}

	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gs, isErr := callTool(t, s, "get_service_scan", map[string]any{})
	if isErr || gs["enabled"] != true {
		t.Fatalf("get_service_scan %v err=%v", gs, isErr)
	}
	inHost := "app." + pat
	outHost := "other.example.test"
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO assets (root_id, subdomain, sources, resolved, cdn_fronted) VALUES ($1, $2, '{dns}', true, false)`, e.rootID, inHost); err != nil {
		t.Fatal(err)
	}
	dec, isErr := callTool(t, s, "check_service_scan", map[string]any{"host": inHost})
	if isErr || dec["allow"] != true || dec["authorized"] != true || dec["enabled"] != true {
		t.Fatalf("in-scope decision %v", dec)
	}
	dec, isErr = callTool(t, s, "check_service_scan", map[string]any{"host": outHost})
	if isErr || dec["allow"] != false || dec["reason"] != "out_of_scope" {
		t.Fatalf("out-of-scope decision %v", dec)
	}
	if _, isErr = callTool(t, s, "check_service_scan", map[string]any{"host": net.IPv4(127, 0, 0, 1).String()}); !isErr {
		t.Fatal("IP host should be rejected")
	}

	c.want(204, "DELETE", "/api/service-scan/scopes/"+strconv.FormatInt(id, 10), nil)
	c.want(404, "DELETE", "/api/service-scan/scopes/"+strconv.FormatInt(id, 10), nil)
	c.want(200, "PATCH", "/api/service-scan", map[string]any{"enabled": false})
	off, isErr := callTool(t, s, "check_service_scan", map[string]any{"host": inHost})
	if isErr || off["allow"] != false || off["reason"] != skipServiceScanDisabled {
		t.Fatalf("disabled decision %v", off)
	}
}

// t49RejectedIPPatterns 是打回列出的非规范 IPv4 / 全数字末级标签写法，必须一律拒绝。
var t49RejectedIPPatterns = []string{
	"127.1", "10.1", "192.168.1", "127.0.1",
	"0177.0.0.1", "0x7f.0.0.1", "1.2.3.4.5",
	"*.127.1", "*.0.1",
}

func TestT49RejectsIPShorthandAndNumericTLD(t *testing.T) {
	for _, p := range t49RejectedIPPatterns {
		if validScopePattern(normalizeHost(p)) {
			t.Errorf("validScopePattern(%q) = true, want false", p)
		}
		if hostInServiceScope(p, []string{p, "example.test"}) {
			t.Errorf("hostInServiceScope(%q) must be false even if the same pattern is listed", p)
		}
		allow, _ := decideServiceScan(true, []string{p, "example.test"}, p)
		if allow {
			t.Errorf("decideServiceScan must deny %q", p)
		}
	}
	// 规范点分四段仍拒；正规域名仍接受。
	if validScopePattern("127.0.0.1") || hostInServiceScope("127.0.0.1", []string{"example.test"}) {
		t.Fatal("dotted-quad IPv4 must stay rejected")
	}
	for _, p := range []string{"example.test", "*.example.test", "app.example.test", "1.example.test"} {
		if !validScopePattern(p) {
			t.Errorf("validScopePattern(%q) = false, want true", p)
		}
	}
	if !hostInServiceScope("1.example.test", []string{"example.test"}) {
		t.Fatal("numeric non-TLD label must still be in scope")
	}
}

func TestT49RESTAndMCPRejectIPShorthand(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	resetServiceScan(t, e)
	c.want(200, "PATCH", "/api/service-scan", map[string]any{"enabled": true})
	for _, p := range append([]string{net.IPv4(127, 0, 0, 1).String()}, t49RejectedIPPatterns...) {
		c.want(400, "POST", "/api/service-scan/scopes", map[string]any{"pattern": p})
	}
	out := c.want(200, "GET", "/api/service-scan", nil)
	if len(items(out)) != 0 {
		t.Fatalf("no shorthand pattern may be stored, got %v", items(out))
	}

	// 正规域仍可登记
	pat := strings.ToLower(t.Name()) + ".example.test"
	created := c.want(201, "POST", "/api/service-scan/scopes", map[string]any{"pattern": pat})
	if created["pattern"] != pat {
		t.Fatalf("legal domain rejected: %v", created)
	}

	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, h := range []string{"127.1", "10.1", "192.168.1", "127.0.1", "0177.0.0.1", "0x7f.0.0.1", "1.2.3.4.5", net.IPv4(127, 0, 0, 1).String()} {
		dec, isErr := callTool(t, s, "check_service_scan", map[string]any{"host": h})
		if !isErr {
			t.Fatalf("check_service_scan host=%q must be rejected, got %v", h, dec)
		}
		if allow, _ := dec["allow"].(bool); allow {
			t.Fatalf("check_service_scan host=%q allow=true", h)
		}
	}
	legal := "app." + pat
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO assets (root_id, subdomain, sources, resolved, cdn_fronted) VALUES ($1, $2, '{dns}', true, false)`, e.rootID, legal); err != nil {
		t.Fatal(err)
	}
	dec, isErr := callTool(t, s, "check_service_scan", map[string]any{"host": legal})
	if isErr || dec["allow"] != true {
		t.Fatalf("legal in-scope host should be allowed, got %v err=%v", dec, isErr)
	}
}

func TestT49RunServiceScanIgnoresIPShorthandEvenIfStored(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	// 绕过 REST，模拟库里已有简写（打回复现路径）。
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_scopes (pattern) VALUES ('127.1'), ('10.1')`); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	var seen []string
	targets := []serviceScanTarget{{Host: "127.1"}, {Host: "10.1"}}
	if err := runServiceScan(ctx, e.pool, targets, func(_ context.Context, h string) error {
		dials.Add(1)
		seen = append(seen, h)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("IP shorthand reached probe: dials=%d seen=%v", dials.Load(), seen)
	}
}

func TestT49VendorInScopeDoesNotBypassServiceScope(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	// 授权范围是另一个域；本资产虽 in_scope=true，仍不得探测。
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_scopes (pattern) VALUES ('other.example.test')`); err != nil {
		t.Fatal(err)
	}
	host := "vend." + strings.ToLower(t.Name()) + ".example.test"
	var id int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, in_scope) VALUES ($1, $2, '{dns}', true) RETURNING id`,
		e.rootID, host).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var flag bool
	if err := e.pool.QueryRow(ctx, `SELECT in_scope FROM assets WHERE id = $1`, id).Scan(&flag); err != nil || !flag {
		t.Fatalf("setup in_scope: %v %v", flag, err)
	}
	var dials atomic.Int32
	if err := runServiceScan(ctx, e.pool, []serviceScanTarget{{AssetID: id, Host: host}}, func(context.Context, string) error {
		dials.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("vendor in_scope=true must not union into service-scan scope, dials=%d", dials.Load())
	}
	var reason string
	if err := e.pool.QueryRow(ctx, `SELECT reason FROM service_scan_skips WHERE host = $1`, host).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "out_of_scope" {
		t.Fatalf("reason %q, want out_of_scope", reason)
	}
}

// t49InetAtonNonNumericLast 末级标签不是纯数字，只能靠 parseInetAton 拦住（去函数后本测试必须失败）。
var t49InetAtonNonNumericLast = []string{"1.0x7f", "0x7f.1", "0x7f000001"}

func TestT49InetAtonNonNumericLastLabel(t *testing.T) {
	for _, p := range t49InetAtonNonNumericLast {
		if parseInetAton(p) == nil {
			t.Errorf("parseInetAton(%q) = nil, want an IPv4", p)
		}
		if !isIPHost(p) {
			t.Errorf("isIPHost(%q) = false, want true (inet_aton)", p)
		}
		if hostInServiceScope(p, []string{p, "example.test"}) {
			t.Errorf("hostInServiceScope(%q) must be false", p)
		}
		allow, _ := decideServiceScan(true, []string{p, "example.test"}, p)
		if allow {
			t.Errorf("decideServiceScan must deny %q", p)
		}
	}
	// 1.0x7f 末级不是纯数字，hasAllNumericLastLabel 拦不住，validScopePattern 只能靠 inet_aton。
	if hasAllNumericLastLabel("1.0x7f") {
		t.Fatal("1.0x7f last label is not all digits")
	}
	if validScopePattern("1.0x7f") || validScopePattern("*.1.0x7f") {
		t.Fatal("validScopePattern must reject 1.0x7f via inet_aton")
	}
	if parseInetAton("example.test") != nil || isIPHost("1.example.test") {
		t.Fatal("legal domains must not look like inet_aton")
	}
}
