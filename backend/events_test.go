package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// 两轮模拟扫描（passive→dns→http 全链路，DNS/HTTP 均为本地 mock）：
// 第 1 轮全部为新增；第 2 轮未变资产不重复播报，新子域只播一次，IP/状态码/标题变化识别为变更，泛解析不产生事件。
func TestEventsTwoRounds(t *testing.T) {
	e := newTestEnv(t)
	addr, _ := mockDNS(t)
	ctx := context.Background()
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	clean := func() {
		c := context.Background()
		e.pool.Exec(c, `DELETE FROM assets WHERE root_id IN (SELECT id FROM root_domains WHERE domain = 'example.test')`)
		e.pool.Exec(c, `DELETE FROM root_domains WHERE domain = 'example.test'`)
		mvIP.Store("192.0.2.10")
	}
	clean()
	t.Cleanup(clean)
	var rootID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'example.test') RETURNING id`, vendorID).Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	e.rootID = rootID

	var round atomic.Int64
	round.Store(1)
	var hits atomic.Int64
	hcfg := localProbeServers(t, &hits)
	hcfg.Schemes = []string{"http"}
	// 第 2 轮 a.example.test 标题改变：用 Dial 指到另一台本地服务
	r2 := localTitleServer(t, &round)
	plainDial := hcfg.Dial
	hcfg.Dial = func(addr string) string {
		if strings.HasPrefix(addr, "a.example.test:") {
			return r2
		}
		return plainDial(addr)
	}

	fakes := []string{"fakea.wild.example.test", "fakeb.wild.example.test", "fakec.wild.example.test"}
	e.start(t, testConfig(fastRates), map[string]ScanHandler{
		"passive": passiveHandler(e.pool, []Source{{"crtsh", func(context.Context, string) ([]string, error) {
			hs := append([]string{"a.example.test", "mv.example.test", "real.wild.example.test", "nx.example.test"}, fakes...)
			if round.Load() == 2 {
				hs = append(hs, "new2.wild.example.test", "v6.example.test")
			}
			return hs, nil
		}}}),
		"dns":  dnsHandler(e.pool, testResolver(addr)),
		"http": httpHandler(e.pool, newProber(hcfg)),
	})
	events := func() []string {
		rows, _ := e.pool.Query(ctx, `SELECT e.type, a.subdomain, e.url_id IS NOT NULL, e.detail->'changes'
			FROM events e JOIN assets a ON a.id = e.asset_id WHERE a.root_id = $1 ORDER BY e.id`, rootID)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var typ, sub string
			var isURL bool
			var ch map[string]any
			rows.Scan(&typ, &sub, &isURL, &ch)
			kind := "asset"
			if isURL {
				kind = "url"
			}
			keys := []string{}
			for k := range ch {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out = append(out, fmt.Sprintf("%s %s %s%v", typ, kind, sub, keys))
		}
		sort.Strings(out)
		return out
	}

	if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "passive", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "http", "done", 1)
	got1 := events()
	want1 := []string{
		"new asset a.example.test[]", "new asset mv.example.test[]", "new asset real.wild.example.test[]",
		"new url a.example.test[]", "new url mv.example.test[]", "new url real.wild.example.test[]",
	}
	if strings.Join(got1, "|") != strings.Join(want1, "|") {
		t.Fatalf("round 1 events:\n%v\nwant\n%v", got1, want1)
	}
	t.Logf("round 1: %d events %v", len(got1), got1)

	// 第 2 轮：mv 换 IP；a 的标题变化；新增 v6（AAAA）；new2 落在泛解析
	round.Store(2)
	mvIP.Store("192.0.2.11")
	if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "passive", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "http", "done", 2)
	got2 := events()
	want2 := append(append([]string{}, want1...),
		"changed asset mv.example.test[ips]",
		"changed url a.example.test[fingerprint_hash title]",
		"new asset v6.example.test[]", "new url v6.example.test[]")
	sort.Strings(want2)
	if strings.Join(got2, "|") != strings.Join(want2, "|") {
		t.Fatalf("round 2 events:\n%v\nwant\n%v", got2, want2)
	}
	var wildEvents int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM events e JOIN assets a ON a.id = e.asset_id WHERE a.root_id = $1 AND a.wildcard`, rootID).Scan(&wildEvents)
	if wildEvents != 0 {
		t.Fatalf("wildcard assets have %d events", wildEvents)
	}
	var detail string
	e.pool.QueryRow(ctx, `SELECT e.detail::text FROM events e JOIN assets a ON a.id = e.asset_id WHERE e.type = 'changed' AND a.subdomain = 'mv.example.test'`).Scan(&detail)
	t.Logf("round 2: %d events total, %d new this round; mv detail %s", len(got2), len(got2)-len(got1), detail)
}

func TestEmitNewOnlyOnce(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var id int64
	e.pool.Exec(ctx, `DELETE FROM assets WHERE subdomain = 'once.events.test'`)
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, 'once.events.test') RETURNING id`, e.rootID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	for i := 0; i < 5; i++ {
		if err := emitNew(ctx, e.pool, id, nil, map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE asset_id = $1 AND type = 'new'`, id).Scan(&n)
	if n != 1 {
		t.Fatalf("new events = %d, want 1", n)
	}
}
