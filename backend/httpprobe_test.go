package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const wildPage = `<html><head><title>Default Site</title></head><body>
<h1>Welcome %s</h1><p>This domain is parked by the hosting provider. If you are the owner of this
domain please log in to the control panel to configure your website and upload your content.</p>
<p>Powered by the default virtual host configuration of this server cluster.</p></body></html>`

const lbPage = `<html><head><title>LB Console &amp; Status</title></head><body>
<h1>Load balancer console</h1><p>Backend pool health: 3 of 3 members up. Sign in with your corporate
account to manage listeners, certificates and routing rules.</p><script src="/static/app.js"></script></body></html>`

// localProbeServers 起本地 http/https 服务，按 Host 返回不同页面；Dial 把 :80/:443 指向它们。
func localProbeServers(t *testing.T, hits *atomic.Int64) httpConfig {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" || r.URL.Path != "/" {
			http.Error(w, "only GET /", 400)
			return
		}
		host, _, _ := net.SplitHostPort(r.Host)
		if host == "" {
			host = r.Host
		}
		switch {
		case host == "lb.wild.test":
			w.Header().Set("Server", "nginx")
			fmt.Fprint(w, lbPage)
		case host == "redir.plain.test":
			http.Redirect(w, r, "https://login.plain.test/sso", http.StatusFound)
		case strings.HasSuffix(host, ".wild.test"):
			fmt.Fprintf(w, wildPage, host)
		default:
			w.Header().Set("Server", "nginx")
			fmt.Fprint(w, `<html><head><title> Plain
			Site </title></head><body>hello</body></html>`)
		}
	})
	plain := httptest.NewServer(h)
	tls := httptest.NewTLSServer(h)
	t.Cleanup(plain.Close)
	t.Cleanup(tls.Close)
	cfg, _ := httpConfigFromEnv()
	cfg.RPS = 1000
	cfg.Timeout = 2 * time.Second
	cfg.Dial = func(addr string) string {
		if strings.HasSuffix(addr, ":443") {
			return tls.Listener.Addr().String()
		}
		return plain.Listener.Addr().String()
	}
	return cfg
}

func TestProbeFields(t *testing.T) {
	var hits atomic.Int64
	p := newProber(localProbeServers(t, &hits))
	ctx := context.Background()
	r, err := p.get(ctx, "https", "www.plain.test")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != 200 || r.Title != "Plain Site" || r.Port != 443 || len(r.Fingerprint) != 64 {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(strings.Join(r.Tech, ","), "Nginx") {
		t.Fatalf("tech %v, want Nginx", r.Tech)
	}
	r2, _ := p.get(ctx, "http", "redir.plain.test")
	if r2.Status != 302 || r2.Redirect != "https://login.plain.test/sso" || r2.Port != 80 {
		t.Fatalf("redirect not recorded (or followed): %+v", r2)
	}
	lb, _ := p.get(ctx, "http", "lb.wild.test")
	if lb.Title != "LB Console & Status" {
		t.Fatalf("title unescape: %q", lb.Title)
	}
	t.Logf("www: status=%d title=%q tech=%v fp=%s…; redir: %d -> %s; requests=%d", r.Status, r.Title, r.Tech, r.Fingerprint[:12], r2.Status, r2.Redirect, hits.Load())
}

func TestProbeRateLimit(t *testing.T) {
	var hits atomic.Int64
	cfg := localProbeServers(t, &hits)
	cfg.RPS = 5
	p := newProber(cfg)
	start := time.Now()
	for i := 0; i < 11; i++ {
		p.get(context.Background(), "http", "www.plain.test")
	}
	// 突发 1，之后每 200ms 一个：11 次请求至少 2s
	if el := time.Since(start); el < 1900*time.Millisecond {
		t.Fatalf("11 requests at 5 rps took %v, want >= 2s", el)
	} else {
		t.Logf("11 requests at HTTP_RPS=5 took %v", el.Round(time.Millisecond))
	}
}

func TestSimilarity(t *testing.T) {
	var hits atomic.Int64
	p := newProber(localProbeServers(t, &hits))
	ctx := context.Background()
	a, _ := p.get(ctx, "http", "x1.wild.test")
	b, _ := p.get(ctx, "http", "wc-0123456789abcdef.wild.test")
	lb, _ := p.get(ctx, "http", "lb.wild.test")
	if s := similarity(a, b); s < 0.99 {
		t.Fatalf("wildcard pages (host echoed) similarity %.3f, want ~1", s)
	}
	if s := similarity(lb, b); s >= 0.3 {
		t.Fatalf("lb vs wildcard similarity %.3f, want low", s)
	}
	t.Logf("sim(x1, wc)=%.3f sim(lb, wc)=%.3f", similarity(a, b), similarity(lb, b))
}

// lb.wild.test 与泛解析共用 IP（DNS 阶段被标 wildcard），但页面不同：不能被误标。
func TestHTTPHandlerWildcardRealHostKept(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	clean := func() {
		c := context.Background()
		e.pool.Exec(c, `DELETE FROM urls WHERE host LIKE '%.wild.test' OR host LIKE '%.plain.test'`)
		e.pool.Exec(c, `DELETE FROM assets WHERE subdomain LIKE '%.wild.test' OR subdomain LIKE '%.plain.test'`)
		e.pool.Exec(c, `DELETE FROM root_domains WHERE domain IN ('wild.test', 'plain.test')`)
	}
	clean()
	t.Cleanup(clean)
	var wildRoot, plainRoot int64
	e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'wild.test') RETURNING id`, vendorID).Scan(&wildRoot)
	e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'plain.test') RETURNING id`, vendorID).Scan(&plainRoot)
	sharedIP := []string{"192.0.2.66"} // 泛解析 IP，lb 与 fake 共用
	for _, h := range []string{"lb.wild.test", "fakea.wild.test", "fakeb.wild.test"} {
		e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, ips, resolved, wildcard) VALUES ($1, $2, $3, true, true)`, wildRoot, h, sharedIP)
	}
	e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, ips, resolved, wildcard) VALUES ($1, 'www.plain.test', '{192.0.2.1}', true, false)`, plainRoot)

	var hits atomic.Int64
	h := httpHandler(e.pool, newProber(localProbeServers(t, &hits)))
	if _, err := h(ctx, 0, []string{"lb.wild.test", "fakea.wild.test", "fakeb.wild.test", "www.plain.test"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := e.pool.Query(ctx, `SELECT a.subdomain, a.wildcard, count(u.id) FROM assets a LEFT JOIN urls u ON u.asset_id = a.id
		WHERE a.root_id IN ($1, $2) GROUP BY 1, 2 ORDER BY 1`, wildRoot, plainRoot)
	got := map[string]string{}
	for rows.Next() {
		var s string
		var w bool
		var n int
		rows.Scan(&s, &w, &n)
		got[s] = fmt.Sprintf("wildcard=%v urls=%d", w, n)
	}
	want := map[string]string{
		"lb.wild.test":    "wildcard=false urls=2",
		"fakea.wild.test": "wildcard=true urls=0",
		"fakeb.wild.test": "wildcard=true urls=0",
		"www.plain.test":  "wildcard=false urls=2",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q want %q (all %v)", k, got[k], v, got)
		}
	}
	var title, fp string
	var status int
	var tech []string
	e.pool.QueryRow(ctx, `SELECT status_code, title, fingerprint_hash, tech FROM urls WHERE scheme = 'https' AND host = 'lb.wild.test' AND port = 443`).Scan(&status, &title, &fp, &tech)
	if status != 200 || title != "LB Console & Status" || fp == "" {
		t.Fatalf("lb url row: %d %q %q %v", status, title, fp, tech)
	}
	// 再跑一次：upsert 不重复
	h(ctx, 0, []string{"www.plain.test"})
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM urls WHERE host = 'www.plain.test'`).Scan(&n)
	if n != 2 {
		t.Fatalf("urls for www.plain.test = %d, want 2", n)
	}
	t.Logf("%v; lb https: %d %q tech=%v; requests=%d", got, status, title, tech, hits.Load())
}

// localTitleServer：第 1 轮标题 "A v1"，第 2 轮 "A v2"。
func localTitleServer(t *testing.T, round *atomic.Int64) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html><head><title>A v%d</title></head><body>app</body></html>", round.Load())
	}))
	t.Cleanup(s.Close)
	return s.Listener.Addr().String()
}
