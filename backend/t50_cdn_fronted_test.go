package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
)

func mockDNSRecords(t *testing.T, zone map[string][]dns.RR) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		q := req.Question[0]
		name := strings.ToLower(q.Name)
		m := new(dns.Msg)
		m.SetReply(req)
		for _, rr := range zone[name] {
			if rr.Header().Rrtype == q.Qtype || rr.Header().Rrtype == dns.TypeCNAME {
				m.Answer = append(m.Answer, rr)
			}
		}
		if len(m.Answer) == 0 {
			m.Rcode = dns.RcodeNameError
		}
		w.WriteMsg(m)
	})}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })
	return pc.LocalAddr().String()
}

func rr(s string) dns.RR { r, _ := dns.NewRR(s); return r }

func assertNoIPKeys(t *testing.T, v any, path string) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			lk := strings.ToLower(k)
			if lk == "ip" || lk == "ips" || lk == "ipv4" || lk == "ipv6" || strings.HasSuffix(lk, "_ip") || strings.HasPrefix(lk, "ip_") {
				t.Errorf("IP field %s at %s", k, path)
			}
			assertNoIPKeys(t, val, path+"."+k)
		}
	case []any:
		for i, val := range x {
			assertNoIPKeys(t, val, path+"[]")
			_ = i
		}
	}
}

func jsonNoIP(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	assertNoIPKeys(t, x, "")
}

func TestT50StaticLibraryAndFile(t *testing.T) {
	t.Cleanup(resetStaticCDNLibrary)
	resetStaticCDNLibrary()
	if got := cdnFromStaticLibrary([]string{"x.kunlunsl.com"}, nil); got != "" {
		t.Fatalf("built-in CNAME is in cdnOf, not static extras: %q", got)
	}
	registerCDNSuffix("testdoc.example.test", "testdoc")
	if got := cdnFromStaticLibrary([]string{"edge.testdoc.example.test"}, nil); got != "testdoc" {
		t.Fatalf("extra suffix %q", got)
	}
	if err := registerCDNCIDR("192.0.2.0/24", "testdoc"); err != nil {
		t.Fatal(err)
	}
	if got := cdnFromStaticLibrary(nil, []string{"192.0.2.80"}); got != "testdoc" {
		t.Fatalf("doc CIDR %q", got)
	}
	if got := cdnFromStaticLibrary(nil, []string{"198.51.100.9"}); got != "" {
		t.Fatalf("other doc net should not match: %q", got)
	}

	dir := t.TempDir()
	cnamePath := filepath.Join(dir, "cname.txt")
	cidrPath := filepath.Join(dir, "cidr.txt")
	os.WriteFile(cnamePath, []byte("# comment\nstatic-cname.example.test staticcdn\n"), 0o644)
	os.WriteFile(cidrPath, []byte("198.51.100.0/24 staticcdn\n203.0.113.0/24 staticcdn\n"), 0o644)
	resetStaticCDNLibrary()
	if err := loadCDNLibraryFile(cnamePath, "cname"); err != nil {
		t.Fatal(err)
	}
	if err := loadCDNLibraryFile(cidrPath, "cidr"); err != nil {
		t.Fatal(err)
	}
	if got := cdnFromStaticLibrary([]string{"www.static-cname.example.test"}, nil); got != "staticcdn" {
		t.Fatalf("file suffix %q", got)
	}
	if got := cdnFromStaticLibrary(nil, []string{"198.51.100.10"}); got != "staticcdn" {
		t.Fatalf("file cidr %q", got)
	}
}

func TestT50DNSMarksFrontedAndOriginQueue(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	t.Cleanup(resetStaticCDNLibrary)
	resetStaticCDNLibrary()
	if err := registerCDNCIDR("192.0.2.0/24", "testdoc"); err != nil {
		t.Fatal(err)
	}

	pfx := strings.ToLower(t.Name())
	base := pfx + ".example.test"
	cnameHost := "cdn-cname." + base
	ipHost := "cdn-ip." + base
	originHost := "origin." + base
	cnameTarget := "edge.cloudflare.net."
	zone := map[string][]dns.RR{
		cnameHost + ".":  {rr(cnameHost + ". 60 IN CNAME " + cnameTarget), rr(cnameTarget + " 60 IN A 198.51.100.1")},
		cnameTarget:      {rr(cnameTarget + " 60 IN A 198.51.100.1")},
		ipHost + ".":     {rr(ipHost + ". 60 IN A 192.0.2.80")},
		originHost + ".": {rr(originHost + ". 60 IN A 198.51.100.9")},
	}
	addr := mockDNSRecords(t, zone)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, h := range []string{cnameHost, ipHost, originHost} {
		var id int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved) VALUES ($1, $2, '{dns}', false) RETURNING id`,
			e.rootID, h).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[h] = id
	}

	var rootScan int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, status) SELECT vendor_id, id, 'dns', 'running' FROM root_domains WHERE id = $1 RETURNING id`,
		e.rootID).Scan(&rootScan); err != nil {
		t.Fatal(err)
	}
	next, err := dnsHandler(e.pool, testResolver(addr))(ctx, rootScan, []string{cnameHost, ipHost, originHost})
	if err != nil {
		t.Fatal(err)
	}
	// CDN 资产仍交给 HTTP 识别路径
	joined := strings.Join(next, ",")
	for _, h := range []string{cnameHost, ipHost, originHost} {
		if !strings.Contains(joined, h) {
			t.Fatalf("HTTP path missing %s, got %v", h, next)
		}
	}

	assertMark := func(host string, fronted bool, wantCDN string) {
		t.Helper()
		var gotFronted bool
		var cdn *string
		if err := e.pool.QueryRow(ctx, `SELECT cdn_fronted, cdn FROM assets WHERE subdomain = $1`, host).Scan(&gotFronted, &cdn); err != nil {
			t.Fatal(err)
		}
		if gotFronted != fronted {
			t.Fatalf("%s cdn_fronted=%v want %v (cdn=%v)", host, gotFronted, fronted, cdn)
		}
		if wantCDN != "" && (cdn == nil || *cdn != wantCDN) {
			t.Fatalf("%s cdn=%v want %s", host, cdn, wantCDN)
		}
		if !fronted && cdn != nil && *cdn != "" {
			t.Fatalf("%s origin should not have cdn, got %v", host, *cdn)
		}
	}
	assertMark(cnameHost, true, "cloudflare")
	assertMark(ipHost, true, "testdoc")
	assertMark(originHost, false, "")

	// 确认解析 IP 未落库
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND column_name ~ '(^|_)ips?($|_)'`).Scan(&n)
	if n != 0 {
		t.Fatal("IP columns exist")
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE detail::text LIKE '%192.0.2.80%' OR detail::text LIKE '%198.51.100%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("events leaked doc IPs: %d", n)
	}

	// T49 开启并登记范围后：仅源站进入端口扫描队列
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_scopes (pattern) VALUES ($1)`, base); err != nil {
		t.Fatal(err)
	}
	targets := []serviceScanTarget{
		{AssetID: ids[cnameHost], Host: cnameHost},
		{AssetID: ids[ipHost], Host: ipHost},
		{AssetID: ids[originHost], Host: originHost},
	}
	queue, err := planOriginServiceScan(ctx, e.pool, targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].Host != originHost {
		t.Fatalf("queue %v, want only origin", queue)
	}
	var dials atomic.Int32
	var seen []string
	probe := func(_ context.Context, host string) error {
		dials.Add(1)
		seen = append(seen, host)
		return nil
	}
	for _, tgt := range queue {
		if err := probe(ctx, tgt.Host); err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != 1 || strings.Join(seen, ",") != originHost {
		t.Fatalf("probes %d %v", dials.Load(), seen)
	}
	var skips []string
	rows, err := e.pool.Query(ctx, `SELECT host || '=' || reason FROM service_scan_skips ORDER BY host`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		skips = append(skips, s)
	}
	wantSkips := []string{cnameHost + "=" + skipServiceScanCDN, ipHost + "=" + skipServiceScanCDN}
	if strings.Join(skips, ",") != strings.Join(wantSkips, ",") {
		t.Fatalf("skips %v want %v", skips, wantSkips)
	}
}

func TestT50DisabledSkipsQueueAndAPIHasNoIP(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	resetServiceScan(t, e)
	t.Cleanup(resetStaticCDNLibrary)
	resetStaticCDNLibrary()
	_ = registerCDNCIDR("192.0.2.0/24", "testdoc")

	ctx := context.Background()
	host := "cdn-ip." + strings.ToLower(t.Name()) + ".example.test"
	origin := "origin." + strings.ToLower(t.Name()) + ".example.test"
	var cid, oid int64
	e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, cdn, cdn_fronted) VALUES ($1, $2, '{dns}', true, 'testdoc', true) RETURNING id`,
		e.rootID, host).Scan(&cid)
	e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, cdn_fronted) VALUES ($1, $2, '{dns}', true, false) RETURNING id`,
		e.rootID, origin).Scan(&oid)

	var dials atomic.Int32
	if err := runOriginServiceScan(ctx, e.pool, []serviceScanTarget{{AssetID: cid, Host: host}, {AssetID: oid, Host: origin}},
		func(context.Context, string) error { dials.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("default-off must not enqueue probes, dials=%d", dials.Load())
	}

	list := c.want(200, "GET", "/api/assets?include_wildcard=true&limit=50", nil)
	jsonNoIP(t, list)
	if strings.Contains(mustJSON(list), "192.0.2.") || strings.Contains(mustJSON(list), "198.51.100.") {
		t.Fatal("list leaked doc IPs")
	}
	var sawFronted, sawOrigin bool
	for _, it := range items(list) {
		if it["subdomain"] == host {
			sawFronted = true
			if it["cdn_fronted"] != true {
				t.Fatalf("list cdn_fronted %v", it["cdn_fronted"])
			}
		}
		if it["subdomain"] == origin {
			sawOrigin = true
			if it["cdn_fronted"] != false {
				t.Fatalf("origin cdn_fronted %v", it["cdn_fronted"])
			}
		}
	}
	if !sawFronted || !sawOrigin {
		t.Fatal("list missing test assets")
	}
	one := c.want(200, "GET", "/api/assets/"+strconv.FormatInt(cid, 10), nil)
	jsonNoIP(t, one)
	ev := c.want(200, "GET", "/api/events?level=all", nil)
	jsonNoIP(t, ev)
	ss := c.want(200, "GET", "/api/service-scan", nil)
	jsonNoIP(t, ss)
	cs := c.want(200, "GET", "/api/check-scope?host="+host, nil)
	jsonNoIP(t, cs)
	comp := c.want(200, "GET", "/api/components", nil)
	jsonNoIP(t, comp)

	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"search_assets", map[string]any{"q": host, "include_wildcard": true}},
		{"list_new_assets", map[string]any{}},
		{"get_service_scan", map[string]any{}},
		{"check_service_scan", map[string]any{"host": host}},
		{"check_scope", map[string]any{"host": host}},
		{"search_components", map[string]any{"include_all_categories": true}},
	} {
		out, isErr := callTool(t, s, tool.name, tool.args)
		if isErr && tool.name != "check_service_scan" {
			t.Fatalf("%s error %v", tool.name, out)
		}
		jsonNoIP(t, out)
	}
	dec, isErr := callTool(t, s, "check_service_scan", map[string]any{"host": host})
	if isErr {
		t.Fatalf("check_service_scan %v", dec)
	}
	if dec["cdn_fronted"] != true {
		t.Fatalf("mcp cdn_fronted %v", dec)
	}
	if dec["allow"] != false || dec["reason"] != skipServiceScanDisabled {
		// 默认关：reason 仍是 disabled（T49 先判定），但标记了 cdn_fronted
		if dec["reason"] != skipServiceScanDisabled {
			t.Fatalf("disabled reason %v", dec)
		}
	}

	// 开启后 CDN 主机 reason 变为 cdn_fronted
	c.want(200, "PATCH", "/api/service-scan", map[string]any{"enabled": true})
	c.want(201, "POST", "/api/service-scan/scopes", map[string]any{"pattern": strings.ToLower(t.Name()) + ".example.test"})
	dec, isErr = callTool(t, s, "check_service_scan", map[string]any{"host": host})
	if isErr || dec["allow"] != false || dec["reason"] != skipServiceScanCDN || dec["cdn_fronted"] != true {
		t.Fatalf("enabled CDN decision %v", dec)
	}
	dec, isErr = callTool(t, s, "check_service_scan", map[string]any{"host": origin})
	if isErr || dec["allow"] != true || dec["cdn_fronted"] != false {
		t.Fatalf("origin decision %v", dec)
	}
	c.want(200, "PATCH", "/api/service-scan", map[string]any{"enabled": false})
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
