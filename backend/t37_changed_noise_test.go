package main

import (
	"context"
	"fmt"
	"testing"
)

// T37：haimian 示例（审计 docs/bugs/audit-2026-11-03.md，域名替换为 *.example.test）。
// 第一跳不变、后续跳变化 → 无事件（不论 cdn 字段）；第一跳变化 → changed。
func TestT37CNAMEFirstHopOnly(t *testing.T) {
	e := newTestEnv(t)
	r := testResolver("127.0.0.1:1")
	h := "www.haimian.example.test"
	id := newRulesAsset(t, e, h)
	cn := func(c ...string) hostDNS {
		return hostDNS{Resolved: true, Result: dnsResult{IPs: []string{"192.0.2.10"}, CNAMEs: c}}
	}
	first := "www.haimian.example.test.bytedns3.example.test"
	dnsRound(t, e, r, h, cn(first, "www.haimian.example.test.230b2a2545cfa773.w.cdngslb.example.test"))
	// 第一跳不变，后续跳变化（cdn 字段也给不同值）：不报
	hd := cn(first, "www.haimian.example.test.c.dsa.cdnbuild.example.test", "l7-online-self-max-v6.s.dsa.cdnbuild.example.test")
	hd.CDN = "bytedance"
	dnsRound(t, e, r, h, hd)
	var stored []string
	e.pool.QueryRow(context.Background(), `SELECT cnames FROM assets WHERE id = $1`, id).Scan(&stored)
	if len(stored) != 3 {
		t.Fatalf("new cnames not stored: %v", stored)
	}
	if got := fmt.Sprint(eventLog(t, e, id)); got != "[new]" {
		t.Fatalf("first hop unchanged: events %s, want [new]", got)
	}
	// 第一跳变化：changed
	dnsRound(t, e, r, h, cn("www.haimian.example.test.other.example.test", "l7-online-self-max-v6.s.dsa.cdnbuild.example.test"))
	log := eventLog(t, e, id)
	if len(log) != 2 || log[1][:7] != "changed" {
		t.Fatalf("first hop changed: events %v, want new+changed", log)
	}
	t.Logf("events: %v", log)
}

// T37：只有 fingerprint_hash 变化 → 不产生 changed、统计不计，新值照常入库；伴随标题变化时照常 changed。
func TestT37FingerprintOnlyNoChanged(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	h := "fp.haimian.example.test"
	id := newRulesAsset(t, e, h)
	p := newProber(httpConfig{RPS: 1000, VerifyRPS: 1000, Concurrency: 1, Timeout: 1})
	put := func(title, fp string) {
		if err := p.storeURL(ctx, e.pool, id, &probeResult{Scheme: "http", Host: h, Port: 80, Status: 200, Title: title, Fingerprint: fp}); err != nil {
			t.Fatal(err)
		}
	}
	v := e.vendorID()
	stat := func() float64 {
		st := c.want(200, "GET", fmt.Sprintf("/api/events/stats?type=changed&level=all&vendor=%d", v), nil)
		return st["totals"].(map[string]any)["changed"].(float64)
	}
	listed := func() int {
		return len(items(c.want(200, "GET", fmt.Sprintf("/api/events?type=changed&level=all&asset_id=%d", id), nil)))
	}
	put("t", "fp1")
	before := stat()
	put("t", "fp2")
	var fp string
	e.pool.QueryRow(ctx, `SELECT fingerprint_hash FROM urls WHERE host = $1`, h).Scan(&fp)
	if fp != "fp2" {
		t.Fatalf("fingerprint not stored: %q", fp)
	}
	if n := listed(); n != 0 {
		t.Fatalf("fingerprint-only: %d changed events, want 0", n)
	}
	if after := stat(); after != before {
		t.Fatalf("stats changed %v -> %v, want unchanged", before, after)
	}
	put("t2", "fp3") // 标题也变：照常 changed
	if n := listed(); n != 1 {
		t.Fatalf("title change: %d changed events, want 1", n)
	}
	if after := stat(); after != before+1 {
		t.Fatalf("stats changed %v -> %v, want +1", before, after)
	}
}
