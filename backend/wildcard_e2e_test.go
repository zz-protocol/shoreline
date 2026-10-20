package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	mrand "math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	tpl, _ := p.controlTemplate(ctx, cache, "http", "s1.test")
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

// 对照页请求失败：重取一个随机子域；仍不足 2 页时本轮该父域候选保持 wildcard（lb 也不救回、不入库 URL）。
func TestControlPageFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failFirst int64 // 前 N 个 wc- 请求断开连接
		lbRescued bool
		ctrlReqs  int64
	}{
		{"首个对照页失败、重取成功", 1, true, 4},
		{"对照页一直失败", 1 << 30, false, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			ctx := context.Background()
			var ctrl atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host, _, _ := net.SplitHostPort(r.Host)
				if host == "" {
					host = r.Host
				}
				if strings.HasPrefix(host, "wc-") {
					if ctrl.Add(1) <= tc.failFirst {
						c, _, _ := w.(http.Hijacker).Hijack()
						c.Close() // 请求失败
						return
					}
				}
				if strings.HasPrefix(host, "lb.") {
					fmt.Fprint(w, lbPage)
					return
				}
				fmt.Fprintf(w, wildPage, host)
			}))
			t.Cleanup(srv.Close)
			cfg, _ := httpConfigFromEnv()
			cfg.RPS, cfg.VerifyRPS, cfg.Schemes, cfg.Timeout = 1000, 1000, []string{"http"}, 2*time.Second
			cfg.Dial = func(string) string { return srv.Listener.Addr().String() }
			t.Cleanup(func() {
				e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE '%.cf.test'`)
			})
			hosts := []string{"lb.cf.test", "fake1.cf.test", "fake2.cf.test"}
			for _, h := range hosts {
				e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, resolved, wildcard, parent_wildcard) VALUES ($1, $2, true, true, true)`, e.rootID, h)
			}
			if _, err := httpHandler(e.pool, newProber(cfg))(ctx, 0, hosts); err != nil {
				t.Fatal(err)
			}
			var lbWild bool
			var urls, wildFakes int
			e.pool.QueryRow(ctx, `SELECT wildcard FROM assets WHERE subdomain = 'lb.cf.test'`).Scan(&lbWild)
			e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM urls WHERE host LIKE '%.cf.test'),
				(SELECT count(*) FROM assets WHERE subdomain LIKE 'fake%.cf.test' AND wildcard)`).Scan(&urls, &wildFakes)
			wantURLs := 0
			if tc.lbRescued {
				wantURLs = 1
			}
			if lbWild == tc.lbRescued || urls != wantURLs || wildFakes != 2 || ctrl.Load() != tc.ctrlReqs {
				t.Fatalf("lb.wildcard=%v urls=%d wildFakes=%d controlRequests=%d", lbWild, urls, wildFakes, ctrl.Load())
			}
			t.Logf("对照页请求 %d 次；lb.wildcard=%v urls=%d 假子域 wildcard=%d/2", ctrl.Load(), lbWild, urls, wildFakes)
		})
	}
}

// 交付复现：wc-* 对照子域响应超过 HTTP_TIMEOUT 或为空页（假主机正常返回停放页），以及整页随机内容的泛解析 → 不得放行，全部保持 wildcard、0 URL、0 事件。
func TestControlPageTimeoutOrEmpty(t *testing.T) {
	for _, mode := range []string{"timeout", "empty", "allDynamic"} {
		t.Run(mode, func(t *testing.T) {
			e := newTestEnv(t)
			ctx := context.Background()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host, _, _ := net.SplitHostPort(r.Host)
				if host == "" {
					host = r.Host
				}
				if mode == "allDynamic" { // 泛解析页整页都是随机内容（对照页与假主机同样）
					b := make([]byte, 16)
					rand.Read(b)
					fmt.Fprintf(w, "<p>%s</p>", base64.RawURLEncoding.EncodeToString(b))
					return
				}
				if strings.HasPrefix(host, "wc-") {
					switch mode {
					case "timeout":
						time.Sleep(400 * time.Millisecond) // > HTTP_TIMEOUT(200ms)
					case "empty":
						return
					}
				}
				fmt.Fprintf(w, wildPage, host)
			}))
			t.Cleanup(srv.Close)
			cfg, _ := httpConfigFromEnv()
			cfg.RPS, cfg.VerifyRPS, cfg.Schemes, cfg.Timeout = 1000, 1000, []string{"http"}, 200*time.Millisecond
			cfg.Dial = func(string) string { return srv.Listener.Addr().String() }
			t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE '%.to.test'`) })
			var hosts []string
			for i := 0; i < 45; i++ {
				h := fmt.Sprintf("fake%02d.to.test", i)
				hosts = append(hosts, h)
				e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, resolved, wildcard, parent_wildcard) VALUES ($1, $2, true, $3, true)`,
					e.rootID, h, i%3 != 0) // 1/3 模拟 DNS 漏标
			}
			if _, err := httpHandler(e.pool, newProber(cfg))(ctx, 0, hosts); err != nil {
				t.Fatal(err)
			}
			var urls, wild, events int
			e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM urls WHERE host LIKE '%.to.test'),
				(SELECT count(*) FROM assets WHERE subdomain LIKE '%.to.test' AND wildcard),
				(SELECT count(*) FROM events ev JOIN assets a ON a.id = ev.asset_id WHERE a.subdomain LIKE '%.to.test')`).Scan(&urls, &wild, &events)
			if urls != 0 || wild != 45 || events != 0 {
				t.Fatalf("%s: urls=%d wildcard=%d/45 events=%d", mode, urls, wild, events)
			}
			t.Logf("对照页%s：urls=%d wildcard=%d/45 events=%d", mode, urls, wild, events)
		})
	}
}

// T8 返工：泛解析父域返回短 404（nginx 默认页 / "not found"），真实主机 lb/blog 返回 200 → 必须救回；假主机保持 wildcard。
// 另含 200 短页（hash 稳定）与长度稳定但内容随机的短页。
func TestShortWildcardPage(t *testing.T) {
	nginx404 := "<html>\r\n<head><title>404 Not Found</title></head>\r\n<body>\r\n<center><h1>404 Not Found</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"
	for _, tc := range []struct {
		name   string
		status int
		body   func() string
	}{
		// 404 场景：lb/blog 状态码不同，必须救回；200 场景：同状态码短页，按"宁可漏救"不要求救回（只要求 0 假资产）
		{"nginx404", 404, func() string { return nginx404 }},
		{"notfound404", 404, func() string { return "not found" }},
		{"short200", 200, func() string { return "ok" }},
		{"random200", 200, func() string { // 长度稳定、内容随机的短页
			b := make([]byte, 12)
			rand.Read(b)
			return hex.EncodeToString(b)[:12] + "zz"
		}},
		{"okRandom200", 200, func() string { // 交付复现："ok"+4~12 个随机字母（6~14B，对照页长度差常 >10%、hash 也不同）
			n := 4 + mrand.Intn(9)
			b := make([]byte, n)
			for i := range b {
				b[i] = byte('a' + mrand.Intn(26))
			}
			return "ok" + string(b)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			ctx := context.Background()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host, _, _ := net.SplitHostPort(r.Host)
				if host == "" {
					host = r.Host
				}
				switch {
				case strings.HasPrefix(host, "lb."):
					fmt.Fprint(w, lbPage)
				case strings.HasPrefix(host, "blog."):
					fmt.Fprint(w, `<html><head><title>Blog</title></head><body><p>Latest posts from our engineering team.</p></body></html>`)
				default:
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body())
				}
			}))
			t.Cleanup(srv.Close)
			cfg, _ := httpConfigFromEnv()
			cfg.RPS, cfg.VerifyRPS, cfg.Schemes, cfg.Timeout = 1000, 1000, []string{"http"}, 2*time.Second
			cfg.Dial = func(string) string { return srv.Listener.Addr().String() }
			t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE '%.short.test'`) })
			hosts := []string{"lb.short.test", "blog.short.test"}
			for i := 0; i < 20; i++ {
				hosts = append(hosts, fmt.Sprintf("fake%02d.short.test", i))
			}
			for i, h := range hosts {
				e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, resolved, wildcard, parent_wildcard) VALUES ($1, $2, true, $3, true)`,
					e.rootID, h, i%3 != 0)
			}
			if _, err := httpHandler(e.pool, newProber(cfg))(ctx, 0, hosts); err != nil {
				t.Fatal(err)
			}
			var realURLs, fakeURLs, wildFakes, realWild int
			e.pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM urls WHERE host IN ('lb.short.test', 'blog.short.test')),
				(SELECT count(*) FROM urls WHERE host LIKE 'fake%.short.test'),
				(SELECT count(*) FROM assets WHERE subdomain LIKE 'fake%.short.test' AND wildcard),
				(SELECT count(*) FROM assets WHERE subdomain IN ('lb.short.test', 'blog.short.test') AND wildcard)`).Scan(&realURLs, &fakeURLs, &wildFakes, &realWild)
			wantReal := 0
			if tc.status != 200 {
				wantReal = 2
			}
			if realURLs != wantReal || realWild != 2-wantReal || fakeURLs != 0 || wildFakes != 20 {
				t.Fatalf("%s: real urls=%d real wildcard=%d fake urls=%d fake wildcard=%d/20", tc.name, realURLs, realWild, fakeURLs, wildFakes)
			}
			t.Logf("%s：lb/blog urls=%d（救回 %d/2），假主机 urls=%d wildcard=%d/20", tc.name, realURLs, 2-realWild, fakeURLs, wildFakes)
		})
	}
}

// T8 封版反例：对照页都短，假主机返回长页（随机长度 / 同一长度 / 与对照交替 / 随机长短），45 个假主机 + lb，每种跑 15 轮：0 假资产。
func TestShortControlsLongFakes(t *testing.T) {
	long := func(n int) string {
		return "<html><body>" + strings.Repeat("welcome to our parking page ", n) + "</body></html>"
	}
	modes := map[string]func(host string, i int64) (int, string){
		"随机长度长页": func(_ string, _ int64) (int, string) { return 200, long(10 + mrand.Intn(40)) },
		"同一长度长页": func(_ string, _ int64) (int, string) { return 200, long(20) },
		"短长交替": func(_ string, i int64) (int, string) {
			if i%2 == 0 {
				return 200, "ok"
			}
			return 200, long(20)
		},
		"随机长短": func(_ string, _ int64) (int, string) {
			if mrand.Intn(2) == 0 {
				return 200, "ok" + strings.Repeat("x", mrand.Intn(8))
			}
			return 200, long(5 + mrand.Intn(40))
		},
	}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			lbSaved := 0
			e := newTestEnv(t)
			ctx := context.Background()
			for round := 0; round < 15; round++ {
				var n atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					host, _, _ := net.SplitHostPort(r.Host)
					if host == "" {
						host = r.Host
					}
					switch {
					case strings.HasPrefix(host, "wc-"):
						fmt.Fprint(w, "ok"+strings.Repeat("y", mrand.Intn(8))) // 对照页都短
					case strings.HasPrefix(host, "lb."):
						fmt.Fprint(w, lbPage)
					default:
						code, body := mode(host, n.Add(1))
						w.WriteHeader(code)
						fmt.Fprint(w, body)
					}
				}))
				cfg, _ := httpConfigFromEnv()
				cfg.RPS, cfg.VerifyRPS, cfg.Schemes, cfg.Timeout = 1000, 1000, []string{"http"}, 2*time.Second
				cfg.Dial = func(string) string { return srv.Listener.Addr().String() }
				hosts := []string{"lb.lf.test"}
				for i := 0; i < 45; i++ {
					hosts = append(hosts, fmt.Sprintf("fake%02d.lf.test", i))
				}
				for i, h := range hosts {
					e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, resolved, wildcard, parent_wildcard) VALUES ($1, $2, true, $3, true)`, e.rootID, h, i%3 != 0)
				}
				if _, err := httpHandler(e.pool, newProber(cfg))(ctx, 0, hosts); err != nil {
					t.Fatal(err)
				}
				var fakeURLs, fakeWild, lbURLs int
				e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM urls WHERE host LIKE 'fake%.lf.test'),
					(SELECT count(*) FROM assets WHERE subdomain LIKE 'fake%.lf.test' AND wildcard),
					(SELECT count(*) FROM urls WHERE host = 'lb.lf.test')`).Scan(&fakeURLs, &fakeWild, &lbURLs)
				srv.Close()
				e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE '%.lf.test'`)
				if fakeURLs != 0 || fakeWild != 45 {
					t.Fatalf("round %d: fake urls=%d wildcard=%d/45", round, fakeURLs, fakeWild)
				}
				if lbURLs > 0 {
					lbSaved++
				}
			}
			t.Logf("%s：15 轮假资产 0（假主机 wildcard 45/45）；lb 救回 %d/15", name, lbSaved)
		})
	}
}
