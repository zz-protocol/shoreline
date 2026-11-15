package main

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func t51NoIP(t *testing.T, v any, path string) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if strings.EqualFold(k, "ip") || strings.EqualFold(k, "ips") {
				t.Fatalf("response has IP field %s at %s", k, path)
			}
			t51NoIP(t, val, path+"."+k)
		}
	case []any:
		for i, val := range x {
			t51NoIP(t, val, path)
			_ = i
		}
	case string:
		if net.ParseIP(x) != nil {
			t.Fatalf("response value looks like IP at %s: %q", path, x)
		}
	}
}

func TestT51ServiceScanKindTrigger(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	origin := "origin." + domain
	cdnHost := "cdn." + domain
	outHost := "other.out-of-scope.example.test"
	oid := t51InsertAsset(t, e, origin, false)
	cid := t51InsertAsset(t, e, cdnHost, true)
	_ = t51InsertAsset(t, e, outHost, false)
	_ = oid
	_ = cid

	mysqlBanner := append([]byte{0x0a}, []byte("8.0.36\x00mysql_native_password")...)
	var dials atomic.Int32
	seen := map[string]int{}
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306},
		RPS:     100,
		Timeout: time.Second,
		Dialer: func(ctx context.Context, network, host string, port int) (net.Conn, error) {
			dials.Add(1)
			seen[host]++
			if rejectedServiceScanHost(host) {
				t.Errorf("dialer got rejected host %q", host)
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", t51BannerOnConnect(t, mysqlBanner))
		},
	}
	rates := map[string]rate.Limit{"passive": 100, "active": 100, "dns": 100, "http": 100, "expand": 100, "service": 100}
	e.start(t, testConfig(rates), map[string]ScanHandler{
		"service": serviceScanHandler(e.pool, cfg),
	})

	// 默认关：任务可入队并完成，但不拨号、不写结果。
	off := c.want(202, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "service"})
	t51NoIP(t, off, "POST off")
	if off["kind"] != "service" {
		t.Fatalf("kind %v", off["kind"])
	}
	e.waitStatus(t, "service", "done", 1)
	if dials.Load() != 0 {
		t.Fatalf("default-off must not dial, dials=%d", dials.Load())
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_services`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("default-off stored %d rows", n)
	}

	t51Enable(t, e, domain)
	on := c.want(202, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "service"})
	t51NoIP(t, on, "POST on")
	e.waitStatus(t, "service", "done", 2)
	if dials.Load() != 1 || seen[origin] != 1 {
		t.Fatalf("only origin should be dialed, dials=%d seen=%v", dials.Load(), seen)
	}
	var name string
	var open bool
	if err := e.pool.QueryRow(ctx, `SELECT open, name FROM asset_services WHERE host = $1 AND port = 3306`, origin).Scan(&open, &name); err != nil {
		t.Fatal(err)
	}
	if !open || name != "mysql" {
		t.Fatalf("origin fingerprint open=%v name=%q", open, name)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_services WHERE host IN ($1, $2)`, cdnHost, outHost).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cdn/out-of-scope must not be stored, rows=%d", n)
	}

	// MCP 同样可触发；再跑一次只增加 origin 拨号。
	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dec, isErr := callTool(t, s, "trigger_scan", map[string]any{"root_id": e.rootID, "kind": "service"})
	if isErr {
		t.Fatalf("mcp trigger_scan service: %v", dec)
	}
	t51NoIP(t, dec, "MCP trigger")
	e.waitStatus(t, "service", "done", 3)
	if dials.Load() != 2 || seen[origin] != 2 {
		t.Fatalf("mcp trigger should dial origin once more, dials=%d seen=%v", dials.Load(), seen)
	}

	// dns/http 仍不能手动触发。
	c.want(400, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "dns"})
	if !strings.Contains(string(openapiYAML), "kind: {type: string, enum: [passive, active, expand, service]}") {
		t.Fatal("openapi POST /api/scans must list service as a manual kind")
	}
}

func TestT51ServiceKindRejectedOnOldManualList(t *testing.T) {
	// 锁定 manualKinds 含 service，且 config 有 RATE_SERVICE 默认。
	if !validKind("service") {
		t.Fatal("service must be a scan kind")
	}
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rates["service"] != 1 {
		t.Fatalf("RATE_SERVICE default %v, want 1", cfg.Rates["service"])
	}
}
