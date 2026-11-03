package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// "用户登陆" 的 GBK/GB18030 字节（注：bug 报告所列 D3C3BBA7B5C7C2BC 实为「用户登录」，「陆」=C2BD）
const gbkTitle = "\xd3\xc3\xbb\xa7\xb5\xc7\xc2\xbd"

// gbkServer：按 Host 返回 GBK 标题页；charset 来自 header / meta / 不声明。
func gbkServer(t *testing.T) httpConfig {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]
		switch {
		case strings.HasPrefix(host, "hdr."):
			w.Header().Set("Content-Type", "text/html; charset=gbk")
			w.Write([]byte("<html><head><title>" + gbkTitle + "</title></head></html>"))
		case strings.HasPrefix(host, "meta."):
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><head><meta charset="gb18030"><title>` + gbkTitle + "</title></head></html>"))
		case strings.HasPrefix(host, "none."): // 不声明 charset：回退 GB18030
			w.Write([]byte("<html><head><title>" + gbkTitle + "</title></head></html>"))
		case strings.HasPrefix(host, "loc."): // Location 头带非法 UTF-8
			w.Header().Set("Location", "/x\xff\xfe")
			w.WriteHeader(302)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<title>普通 UTF-8</title>"))
		}
	}))
	t.Cleanup(srv.Close)
	cfg, _ := httpConfigFromEnv()
	cfg.RPS, cfg.VerifyRPS, cfg.Schemes, cfg.Timeout = 1000, 1000, []string{"http"}, 2*time.Second
	cfg.Dial = func(string) string { return srv.Listener.Addr().String() }
	return cfg
}

func TestT33GBKTitleDecodedAndStored(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	p := newProber(gbkServer(t))
	for _, h := range []string{"hdr.t33.example.test", "meta.t33.example.test", "none.t33.example.test"} {
		r, err := p.get(ctx, "http", h)
		if err != nil {
			t.Fatal(err)
		}
		if r.Title != "用户登陆" {
			t.Fatalf("%s: Title=%q want 用户登陆", h, r.Title)
		}
		var aid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, e.rootID, h).Scan(&aid); err != nil {
			t.Fatal(err)
		}
		if err := p.storeURL(ctx, e.pool, aid, r); err != nil {
			t.Fatalf("%s storeURL: %v", h, err)
		}
		var got string
		e.pool.QueryRow(ctx, `SELECT title FROM urls WHERE host = $1`, h).Scan(&got)
		if got != "用户登陆" {
			t.Fatalf("%s stored title %q", h, got)
		}
	}
	if r, _ := p.get(ctx, "http", "u.t33.example.test"); r.Title != "普通 UTF-8" {
		t.Fatalf("utf-8 page title %q", r.Title)
	}
	t.Logf("GBK（header/meta/未声明）标题均解码为「用户登陆」并成功入库")
}

// 含 GBK 标题和坏 Location 的 http 任务最终 done（旧代码：22021 → retry 耗尽 failed）。
func TestT33HTTPTaskWithBadHostDone(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	hosts := []string{"hdr.t33t.example.test", "loc.t33t.example.test", "ok.t33t.example.test"}
	for _, h := range hosts {
		e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, resolved, wildcard) VALUES ($1, $2, true, false)`, e.rootID, h)
	}
	cfg := testConfig(fastRates)
	cfg.MaxRetry = 1
	e.start(t, cfg, map[string]ScanHandler{"http": httpHandler(e.pool, newProber(gbkServer(t)))})
	if _, err := enqueueScan(ctx, e.pool, e.client, e.rootID, "http", hosts, 0, cfg.MaxRetry); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "http", "done", 1)
	var redirect string
	e.pool.QueryRow(ctx, `SELECT coalesce(redirect_to, '') FROM urls WHERE host = 'loc.t33t.example.test'`).Scan(&redirect)
	if !strings.HasPrefix(redirect, "/x") || !strings.Contains(redirect, "\uFFFD") {
		t.Fatalf("redirect %q", redirect)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM urls WHERE host = ANY($1)`, hosts).Scan(&n)
	if n != 3 {
		t.Fatalf("urls stored %d want 3", n)
	}
	t.Logf("含 GBK 标题 + 非法 Location 的 http 任务 done，3 条 URL 均入库")
}
