package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// eventLog 返回资产的事件摘要（类型 + changes 键）。
func eventLog(t *testing.T, e *testEnv, assetID int64) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT type, COALESCE(detail->'changes', '{}')::text
		FROM events WHERE asset_id = $1 AND url_id IS NULL ORDER BY id`, assetID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, ch string
		rows.Scan(&typ, &ch)
		if typ == "changed" {
			out = append(out, "changed "+ch)
		} else {
			out = append(out, typ)
		}
	}
	return out
}

func newRulesAsset(t *testing.T, e *testEnv, host string) int64 {
	t.Helper()
	ctx := context.Background()
	e.pool.Exec(ctx, `DELETE FROM assets WHERE subdomain = $1`, host)
	var id int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, e.rootID, host).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	return id
}

func dnsRound(t *testing.T, e *testEnv, r *resolver, host string, hd hostDNS) {
	t.Helper()
	hd.Host = host
	cn := hd.Result.CNAMEs
	if cn == nil {
		cn = []string{}
	}
	if err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		_, err := r.storeDNS(context.Background(), tx, host, hd, cn, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func ok(ips ...string) hostDNS { return hostDNS{Resolved: true, Result: dnsResult{IPs: ips}} }

// 规则（T28）：只看 CNAME 集合——IP 变化（新增 IPv6、换 IP）不报；CNAME 新增、恢复原值 = changed；失联保留最后 CNAME，失联与再解析成功 DNS 层都不报（T21：recovered 只在 HTTP 阶段 dead→alive 产生）。
func TestAssetEventRules(t *testing.T) {
	e := newTestEnv(t)
	r := testResolver("127.0.0.1:1")
	h := "rules.events.test"
	id := newRulesAsset(t, e, h)
	steps := []hostDNS{
		{}, // 首次未解析：不发 new
		ok("192.0.2.10"),
		ok("192.0.2.10"),                // 不变
		ok("192.0.2.10", "2001:db8::1"), // 新增 IPv6
		ok("192.0.2.10"),                // 去掉 IPv6
		{Resolved: true, Result: dnsResult{IPs: []string{"192.0.2.10"}, CNAMEs: []string{"x.cdn.test"}}}, // CNAME 变化
		{Resolved: true, Result: dnsResult{IPs: []string{"192.0.2.99"}, CNAMEs: []string{"x.cdn.test"}}}, // CNAME 不变、只换 IP：不报
		{}, // 失联：保留最后 CNAME，不报
		{Resolved: true, Result: dnsResult{IPs: []string{"192.0.2.20"}, CNAMEs: []string{"x.cdn.test"}}}, // 失联后同 CNAME 再现：不报
		ok("192.0.2.20"), // CNAME 恢复原值（空）
	}
	for _, s := range steps {
		dnsRound(t, e, r, h, s)
	}
	got := fmt.Sprint(eventLog(t, e, id))
	want := fmt.Sprint([]string{"new",
		`changed {"cnames": {"new": ["x.cdn.test"], "old": []}}`,
		`changed {"cnames": {"new": [], "old": ["x.cdn.test"]}}`,
	})
	if got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	t.Logf("events: %s", got)

	// 失联时保留最后成功 CNAME
	dnsRound(t, e, r, h, hostDNS{Resolved: true, Result: dnsResult{IPs: []string{"192.0.2.20"}, CNAMEs: []string{"y.cdn.test"}}})
	dnsRound(t, e, r, h, hostDNS{})
	var resolved bool
	var cn []string
	e.pool.QueryRow(context.Background(), `SELECT cnames, resolved FROM assets WHERE id = $1`, id).Scan(&cn, &resolved)
	if resolved || fmt.Sprint(cn) != "[y.cdn.test]" {
		t.Fatalf("after NXDOMAIN cnames=%v resolved=%v", cn, resolved)
	}
}

// 已复核为真实：有效期内 DNS 不重标 wildcard；回落到泛解析不报；CNAME 变化照常 changed（只换 IP 不报）；过期后重新按 DNS 判定。
func TestVerifiedRealHost(t *testing.T) {
	e := newTestEnv(t)
	r := testResolver("127.0.0.1:1")
	h := "lb.verified.events.test"
	id := newRulesAsset(t, e, h)
	ctx := context.Background()
	e.pool.Exec(ctx, `UPDATE assets SET resolved = true, wildcard = false, verified_real_at = now() WHERE id = $1`, id)
	e.pool.Exec(ctx, `INSERT INTO events (type, asset_id) VALUES ('new', $1)`, id)
	wild := hostDNS{Resolved: true, Wildcard: true, ParentWild: true, Result: dnsResult{IPs: []string{"10.9.0.9"}}}
	dnsRound(t, e, r, h, wild) // IP 回落到泛解析 IP：不报、不重标
	var w, pw bool
	e.pool.QueryRow(ctx, `SELECT wildcard, parent_wildcard FROM assets WHERE id = $1`, id).Scan(&w, &pw)
	if w || pw {
		t.Fatalf("verified host re-marked: wildcard=%v parent_wildcard=%v", w, pw)
	}
	dnsRound(t, e, r, h, hostDNS{Resolved: true, ParentWild: true, Result: dnsResult{IPs: []string{"10.9.0.2"}}})                                  // 只换 IP：不报
	dnsRound(t, e, r, h, hostDNS{Resolved: true, ParentWild: true, Result: dnsResult{IPs: []string{"10.9.0.2"}, CNAMEs: []string{"lb.cdn.test"}}}) // CNAME 变化
	got := fmt.Sprint(eventLog(t, e, id))
	want := fmt.Sprint([]string{"new", `changed {"cnames": {"new": ["lb.cdn.test"], "old": []}}`})
	if got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	// 过期（默认 7 天）：重新按 DNS 判定，标回 wildcard 等待复核
	e.pool.Exec(ctx, `UPDATE assets SET verified_real_at = now() - interval '8 days' WHERE id = $1`, id)
	dnsRound(t, e, r, h, wild)
	e.pool.QueryRow(ctx, `SELECT wildcard FROM assets WHERE id = $1`, id).Scan(&w)
	if !w {
		t.Fatal("expired verification should let DNS re-mark wildcard")
	}
	t.Logf("verified host events: %s; expired -> wildcard=%v", got, w)
}

// 并发：10 个 goroutine 以同一结果同时更新，同一字段只记一条 changed（URL 与资产两层）。
func TestConcurrentChangedOnce(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	h := "conc.events.test"
	id := newRulesAsset(t, e, h)
	p := newProber(httpConfig{RPS: 1000, VerifyRPS: 1000, Concurrency: 1, Timeout: 1})
	base := &probeResult{Scheme: "http", Host: h, Port: 80, Status: 200, Title: "v1", Fingerprint: "fp1"}
	if err := p.storeURL(ctx, e.pool, id, base); err != nil {
		t.Fatal(err)
	}
	r := testResolver("127.0.0.1:1")
	dnsRound(t, e, r, h, ok("192.0.2.1"))
	cnameChg := hostDNS{Resolved: true, Result: dnsResult{IPs: []string{"192.0.2.2"}, CNAMEs: []string{"c.cdn.test"}}}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := p.storeURL(ctx, e.pool, id, &probeResult{Scheme: "http", Host: h, Port: 80, Status: 200, Title: "v2", Fingerprint: "fp2"}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			dnsRound(t, e, r, h, cnameChg)
		}()
	}
	wg.Wait()
	var urlChanged, assetChanged, urlNew int
	e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type = 'changed' AND url_id IS NOT NULL),
		count(*) FILTER (WHERE type = 'changed' AND url_id IS NULL), count(*) FILTER (WHERE type = 'new' AND url_id IS NOT NULL)
		FROM events WHERE asset_id = $1`, id).Scan(&urlChanged, &assetChanged, &urlNew)
	if urlChanged != 1 || assetChanged != 1 || urlNew != 1 {
		t.Fatalf("url changed=%d asset changed=%d url new=%d, want 1/1/1", urlChanged, assetChanged, urlNew)
	}
	// 并发首次插入同一 URL：只有一条 new
	h2 := "conc2.events.test"
	id2 := newRulesAsset(t, e, h2)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.storeURL(ctx, e.pool, id2, &probeResult{Scheme: "http", Host: h2, Port: 80, Status: 200, Title: "t", Fingerprint: "f"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n2 int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE asset_id = $1`, id2).Scan(&n2)
	if n2 != 1 {
		t.Fatalf("concurrent first insert: %d events, want 1 new", n2)
	}
	t.Logf("10 并发同结果：URL changed=%d，资产 changed=%d；10 并发首次插入：事件 %d 条", urlChanged, assetChanged, n2)
}

// 008：存量重复 new 先去重再建唯一索引，可重复执行。
func TestEventDedupMigration(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	id := newRulesAsset(t, e, "dup.events.test")
	m008, _ := migrationFS.ReadFile("migrations/008_event_dedup.sql")
	e.pool.Exec(ctx, `DROP INDEX IF EXISTS events_new_asset_uniq`)
	e.pool.Exec(ctx, `DROP INDEX IF EXISTS events_new_url_uniq`)
	for i := 0; i < 3; i++ {
		e.pool.Exec(ctx, `INSERT INTO events (type, asset_id) VALUES ('new', $1)`, id)
	}
	e.pool.Exec(ctx, `INSERT INTO events (type, asset_id) VALUES ('changed', $1), ('changed', $1)`, id)
	for i := 0; i < 2; i++ {
		if _, err := e.pool.Exec(ctx, string(m008)); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	var nNew, nChanged, idx int
	e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type = 'new'), count(*) FILTER (WHERE type = 'changed') FROM events WHERE asset_id = $1`, id).Scan(&nNew, &nChanged)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname IN ('events_new_asset_uniq', 'events_new_url_uniq')`).Scan(&idx)
	if nNew != 1 || nChanged != 2 || idx != 2 {
		t.Fatalf("new=%d changed=%d indexes=%d", nNew, nChanged, idx)
	}
	t.Logf("3 条重复 new → %d，changed 保留 %d，索引 %d 个", nNew, nChanged, idx)
}

// T28：库里没有 IP 列，事件 detail 不带 IP，openapi 不暴露 ips。
func TestNoIPStored(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var cols []string
	e.pool.QueryRow(ctx, `SELECT COALESCE(array_agg(table_name || '.' || column_name), '{}') FROM information_schema.columns
		WHERE table_schema = 'public' AND (column_name ~ '(^|_)ips?($|_)')`).Scan(&cols)
	if len(cols) != 0 {
		t.Fatalf("IP columns still exist: %v", cols)
	}
	r := testResolver("127.0.0.1:1")
	h := "noip.events.test"
	id := newRulesAsset(t, e, h)
	dnsRound(t, e, r, h, ok("192.0.2.7"))
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE asset_id = $1 AND detail::text ~ '192\.0\.2\.7|"ips"'`, id).Scan(&n)
	if n != 0 {
		t.Fatalf("%d events still carry IP", n)
	}
	spec, _ := os.ReadFile("openapi.yaml")
	if strings.Contains(string(spec), "ips:") {
		t.Fatal("openapi still exposes ips")
	}
}
