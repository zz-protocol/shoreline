package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 泛解析页：回显主机名 + 两个随机 token（visitor id、request-id）+ 时间戳，每次请求都不同。
func randomTokenWildcardServer(t *testing.T) httpConfig {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if strings.HasPrefix(host, "lb.") {
			fmt.Fprint(w, lbPage)
			return
		}
		b := make([]byte, 24)
		rand.Read(b)
		vid, rid := hex.EncodeToString(b[:8]), hex.EncodeToString(b[8:])
		uuid := rid[:8] + "-" + rid[8:12] + "-" + rid[12:16] + "-" + rid[16:20] + "-" + rid[20:]
		// 规则外动态内容（交付复现）：10 位纯字母随机词、短 base64、文本星期/月份日期
		letters := make([]byte, 10)
		for i := range letters {
			letters[i] = 'a' + b[i%len(b)]%26
		}
		b64 := base64.StdEncoding.EncodeToString(b[:7])
		days := []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
		months := []string{"January", "March", "May", "July", "October", "December"}
		date := days[int(b[0])%7] + ", " + months[int(b[1])%6] + " " + fmt.Sprint(1+int(b[2])%28)
		fmt.Fprintf(w, `<html><head><title>Welcome</title></head><body>
<h1>Welcome %s</h1><p>This domain is parked by the hosting provider. If you are the owner of this
domain please log in to the control panel to configure your website.</p>
<p>visitor id: %s</p><p>request-id: %s</p><p>generated at %d</p>
<p>session %s token %s</p><p>Last updated %s</p></body></html>`, host, vid, uuid, time.Now().UnixNano(), letters, b64, date)
	})
	plain, tls := httptest.NewServer(h), httptest.NewTLSServer(h)
	t.Cleanup(plain.Close)
	t.Cleanup(tls.Close)
	cfg, _ := httpConfigFromEnv()
	cfg.RPS, cfg.VerifyRPS, cfg.Timeout = 1000, 1000, 2*time.Second
	cfg.Dial = func(addr string) string {
		if strings.HasSuffix(addr, ":443") {
			return tls.Listener.Addr().String()
		}
		return plain.Listener.Addr().String()
	}
	return cfg
}

// 交付打回复现：40 个假子域 + lb（与泛解析共用 IP），泛解析页含随机 token；单 IP、轮换 8、轮换 60 三种。
// 期望：urls 只有 lb 的 2 条，假子域全部 wildcard=true，lb 被救回；事件只有 lb 的 new。
func TestWildcardRandomTokensEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	addr, _ := mockDNS(t)
	ctx := context.Background()
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	zones := []string{"s1.test", "r8.test", "r60.test"}
	clean := func() {
		for _, z := range zones {
			e.pool.Exec(context.Background(), `DELETE FROM assets WHERE root_id IN (SELECT id FROM root_domains WHERE domain = $1)`, z)
			e.pool.Exec(context.Background(), `DELETE FROM root_domains WHERE domain = $1`, z)
		}
	}
	clean()
	t.Cleanup(clean)
	res := newResolver(dnsConfig{Resolvers: []string{addr}, Timeout: 500 * time.Millisecond, QPS: 10000,
		WildcardProbe: 3, WildcardStable: 5, WildcardMax: 60, Concurrency: 4, VerifiedTTL: 168 * time.Hour}) // 生产默认 5/60
	dnsH := dnsHandler(e.pool, res)
	missed := map[string]int{}
	e.start(t, testConfig(fastRates), map[string]ScanHandler{
		"passive": passiveHandler(e.pool, []Source{{"crtsh", func(_ context.Context, root string) ([]string, error) {
			hs := []string{"lb." + root}
			for i := 0; i < 40; i++ {
				hs = append(hs, fmt.Sprintf("fake%02d.%s", i, root))
			}
			return hs, nil
		}}}),
		"dns": func(ctx context.Context, scanID int64, hosts []string) ([]string, error) {
			next, err := dnsH(ctx, scanID, hosts)
			var n int
			var root string
			e.pool.QueryRow(ctx, `SELECT r.domain FROM scan_tasks s JOIN root_domains r ON r.id = s.root_id WHERE s.id = $1`, scanID).Scan(&root)
			e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE subdomain LIKE 'fake%.' || $1 AND NOT wildcard`, root).Scan(&n)
			missed[root] = n
			return next, err
		},
		"http": httpHandler(e.pool, newProber(randomTokenWildcardServer(t))),
	})
	for _, z := range zones {
		var rootID int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, $2) RETURNING id`, vendorID, z).Scan(&rootID); err != nil {
			t.Fatal(err)
		}
		e.rootID = rootID
		if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "passive", 3); err != nil {
			t.Fatal(err)
		}
		e.waitStatus(t, "http", "done", 1)
		var urls, wildFakes, lbWild, lbURLs, events int
		e.pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM urls u JOIN assets a ON a.id = u.asset_id WHERE a.root_id = $1),
			(SELECT count(*) FROM assets WHERE root_id = $1 AND subdomain LIKE 'fake%' AND wildcard),
			(SELECT count(*) FROM assets WHERE root_id = $1 AND subdomain LIKE 'lb.%' AND wildcard),
			(SELECT count(*) FROM urls WHERE host = 'lb.' || $2),
			(SELECT count(*) FROM events ev JOIN assets a ON a.id = ev.asset_id WHERE a.root_id = $1 AND a.subdomain NOT LIKE 'lb.%')`,
			rootID, z).Scan(&urls, &wildFakes, &lbWild, &lbURLs, &events)
		t.Logf("%s: DNS 漏标 %d/40；urls=%d（lb %d） 假子域 wildcard=%d/40 lb.wildcard=%v 假子域事件=%d",
			z, missed[z], urls, lbURLs, wildFakes, lbWild == 1, events)
		if z == "r60.test" && missed[z] < 2 {
			t.Fatalf("r60: expected DNS to miss >= 2 fakes, missed %d", missed[z])
		}
		if urls != 2 || lbURLs != 2 || wildFakes != 40 || lbWild != 0 || events != 0 {
			t.Fatalf("%s: urls=%d lbURLs=%d wildFakes=%d lbWild=%d fakeEvents=%d", z, urls, lbURLs, wildFakes, lbWild, events)
		}
	}
}

func TestPageNormalization(t *testing.T) {
	cfg := randomTokenWildcardServer(t)
	p := newProber(cfg)
	ctx := context.Background()
	a, _ := p.get(ctx, "http", "x1.s1.test")
	b, _ := p.get(ctx, "http", "wc-0011223344556677.s1.test")
	lb, _ := p.get(ctx, "http", "lb.s1.test")
	cache := &pageCache{m: map[string]*wildcardTemplate{}, ok: map[string]bool{}}
	tpl := p.controlTemplate(ctx, cache, "http", "s1.test")
	if s := tpl.similarity(a); s < 0.9 {
		t.Fatalf("random-token wildcard page vs template %.3f", s)
	}
	if s := tpl.similarity(lb); s >= 0.3 {
		t.Fatalf("lb vs template %.3f", s)
	}
	t.Logf("两页直接 Jaccard=%.3f（规则外随机词/base64/日期拉低）；掩码模板：泛解析页 sim=%.3f，lb sim=%.3f（模板 %d 词，动态配额 %d）",
		similarity(a, b), tpl.similarity(a), tpl.similarity(lb), len(tpl.template), tpl.allowed)
}

// 复核单独限速与每轮上限：超出上限的候选不探测、直接标 wildcard。
func TestVerifyRateAndCap(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE '%.cap.test'`) })
	var hosts []string
	for i := 0; i < 6; i++ {
		h := fmt.Sprintf("f%d.cap.test", i)
		hosts = append(hosts, h)
		e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, resolved, wildcard, parent_wildcard) VALUES ($1, $2, true, false, true)`, e.rootID, h)
	}
	cfg := randomTokenWildcardServer(t)
	cfg.Schemes, cfg.VerifyRPS, cfg.VerifyMax = []string{"http"}, 5, 3
	start := time.Now()
	if _, err := httpHandler(e.pool, newProber(cfg))(ctx, 0, hosts); err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	var wild int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE subdomain LIKE '%.cap.test' AND wildcard`).Scan(&wild)
	// 3 个复核 + 1 个对照页 = 4 个请求，5 rps 突发 1：>= 600ms
	if wild != 6 || el < 550*time.Millisecond {
		t.Fatalf("wild=%d elapsed=%v", wild, el)
	}
	t.Logf("HTTP_VERIFY_MAX=3 HTTP_VERIFY_RPS=5：6 个候选全部标 wildcard（3 个复核、3 个超限），耗时 %v", el.Round(time.Millisecond))
}
