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
	if host != outHost || reason != skipServiceScanOutOfScope {
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
	if reason != skipServiceScanOutOfScope {
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
	dec, isErr := callTool(t, s, "check_service_scan", map[string]any{"host": inHost})
	if isErr || dec["allow"] != true || dec["authorized"] != true || dec["enabled"] != true {
		t.Fatalf("in-scope decision %v", dec)
	}
	dec, isErr = callTool(t, s, "check_service_scan", map[string]any{"host": outHost})
	if isErr || dec["allow"] != false || dec["reason"] != skipServiceScanOutOfScope {
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
