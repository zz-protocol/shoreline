package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdentifyRulesLocked(t *testing.T) {
	want := []identifyRule{
		{Name: "Apache HTTP Server", Source: "header", Header: "Server", Contains: "Apache"},
		{Name: "Nginx", Source: "header", Header: "Server", Contains: "nginx"},
		{Name: "IIS", Source: "header", Header: "Server", Contains: "Microsoft-IIS"},
		{Name: "OpenResty", Source: "header", Header: "Server", Contains: "openresty"},
		{Name: "PHP", Source: "header", Header: "X-Powered-By", Contains: "PHP"},
		{Name: "Drupal", Source: "header", Header: "X-Generator", Contains: "Drupal"},
		{Name: "WordPress", Source: "title", Contains: "WordPress"},
		{Name: "phpMyAdmin", Source: "title", Contains: "phpMyAdmin"},
		{Name: "Jenkins", Source: "title", Contains: "Jenkins"},
		{Name: "GitLab", Source: "title", Contains: "GitLab"},
	}
	if len(identifyRules) != len(want) {
		t.Fatalf("len=%d want %d", len(identifyRules), len(want))
	}
	for i, r := range identifyRules {
		if r != want[i] {
			t.Fatalf("rule %d %+v want %+v", i, r, want[i])
		}
	}
}

func TestDeclaredFaviconSameHost(t *testing.T) {
	body := []byte(`<html><head><link rel="icon" href="/static/icon.png"></head></body>`)
	if got := declaredFaviconHref(body); got != "/static/icon.png" {
		t.Fatalf("href %q", got)
	}
	u, ok := sameHostFaviconURL("https", "app.example.test", "/static/icon.png")
	if !ok || u != "https://app.example.test/static/icon.png" {
		t.Fatalf("same host %q %v", u, ok)
	}
	if _, ok := sameHostFaviconURL("https", "app.example.test", "https://cdn.example.test/favicon.ico"); ok {
		t.Fatal("off-site href should be rejected")
	}
}

// t45IdentifyServer 只允许 GET / 和一次 favicon；其它路径记下来供断言。
func t45IdentifyServer(t *testing.T, iconBody []byte) (cfg httpConfig, paths *[]string) {
	t.Helper()
	var seen []string
	paths = &seen
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		seen = append(seen, r.Method+" "+r.URL.Path+" host="+strings.ToLower(host))
		switch r.URL.Path {
		case "/":
			w.Header().Set("Server", "nginx")
			w.Header().Set("X-Powered-By", "PHP/8.2")
			w.Header().Set("X-Generator", "Drupal 10 (https://www.drupal.org)")
			fmt.Fprint(w, `<html><head><title>phpMyAdmin setup</title><link rel="icon" href="/static/icon.png"></head><body>ok</body></html>`)
		case "/static/icon.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(iconBody)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	})
	plain := httptest.NewServer(h)
	t.Cleanup(plain.Close)
	cfg, _ = httpConfigFromEnv()
	cfg.RPS, cfg.VerifyRPS, cfg.Schemes, cfg.Timeout = 1000, 1000, []string{"http"}, 2*time.Second
	cfg.Dial = func(string) string { return plain.Listener.Addr().String() }
	return cfg, paths
}

func TestT45HeaderTitleFaviconStored(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	icon := []byte("favicon-bytes-t45")
	wantHash := sha256.Sum256(icon)
	host := "app." + strings.ToLower(t.Name()) + ".example.test"
	if _, err := e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, alive) VALUES ($1, $2, '{http}', true, 'alive')`, e.rootID, host); err != nil {
		t.Fatal(err)
	}
	cfg, paths := t45IdentifyServer(t, icon)
	if _, err := httpHandler(e.pool, newProber(cfg))(ctx, 0, []string{host}); err != nil {
		t.Fatal(err)
	}
	var headers map[string]string
	var fav string
	var deep any
	if err := e.pool.QueryRow(ctx, `SELECT http_headers, favicon_hash, deep_probed_at IS NOT NULL FROM urls WHERE host = $1 AND scheme = 'http'`, host).
		Scan(&headers, &fav, &deep); err != nil {
		t.Fatal(err)
	}
	if headers["Server"] != "nginx" || headers["X-Powered-By"] != "PHP/8.2" || !strings.Contains(headers["X-Generator"], "Drupal") {
		t.Fatalf("headers %#v", headers)
	}
	if fav != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("favicon_hash %q want %s", fav, hex.EncodeToString(wantHash[:]))
	}
	if deep != true {
		t.Fatalf("deep_probed_at not set: %v", deep)
	}
	rows, err := e.pool.Query(ctx, `SELECT c.name, uc.source FROM url_components uc JOIN components c ON c.id = uc.component_id
		WHERE uc.url_id = (SELECT id FROM urls WHERE host = $1 AND scheme = 'http') ORDER BY c.name, uc.source`, host)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, src string
		if err := rows.Scan(&name, &src); err != nil {
			t.Fatal(err)
		}
		got[name] = src
	}
	// 标题规则 phpMyAdmin 必为 title；头规则命中的组件应已关联（wappalyzer 先写入时保留其来源）。
	if got["phpMyAdmin"] != "title" {
		t.Fatalf("phpMyAdmin source=%q want title (all=%v)", got["phpMyAdmin"], got)
	}
	for _, name := range []string{"Drupal", "PHP", "Nginx"} {
		if got[name] == "" {
			t.Fatalf("missing component %s (all=%v)", name, got)
		}
	}
	hits := matchIdentifyRules("phpMyAdmin setup", map[string]string{
		"Server": "nginx", "X-Powered-By": "PHP/8.2", "X-Generator": "Drupal 10 (https://www.drupal.org)",
	})
	kinds := map[string]string{}
	for _, h := range hits {
		kinds[h.Name] = h.Source
	}
	if kinds["Drupal"] != "header" || kinds["PHP"] != "header" || kinds["Nginx"] != "header" || kinds["phpMyAdmin"] != "title" {
		t.Fatalf("rule hits %v", kinds)
	}
	for _, pth := range *paths {
		if strings.Contains(pth, " /") && !strings.Contains(pth, "GET / ") && !strings.Contains(pth, "GET /static/icon.png ") {
			t.Fatalf("unexpected request %q in %v", pth, *paths)
		}
	}
	var root, iconN int
	for _, pth := range *paths {
		if strings.Contains(pth, "GET / ") {
			root++
		}
		if strings.Contains(pth, "GET /static/icon.png ") {
			iconN++
		}
	}
	if root != 1 || iconN != 1 {
		t.Fatalf("requests %v (GET /=%d icon=%d)", *paths, root, iconN)
	}
}

func TestT45NoExtraPathsAndOffsiteFavicon(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	host := "off." + strings.ToLower(t.Name()) + ".example.test"
	var paths []string
	icon := []byte("ico-t45")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<html><head><title>Plain</title><link rel="icon" href="https://cdn.example.test/cdn.ico"></head><body>x</body></html>`)
		case "/favicon.ico":
			w.Write(icon)
		default:
			http.Error(w, "no", 404)
		}
	}))
	t.Cleanup(srv.Close)
	if _, err := e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, alive) VALUES ($1, $2, '{http}', true, 'alive')`, e.rootID, host); err != nil {
		t.Fatal(err)
	}
	cfg, _ := httpConfigFromEnv()
	cfg.RPS, cfg.Schemes, cfg.Timeout = 1000, []string{"http"}, 2*time.Second
	cfg.Dial = func(string) string { return srv.Listener.Addr().String() }
	if _, err := httpHandler(e.pool, newProber(cfg))(ctx, 0, []string{host}); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(icon)
	var fav string
	if err := e.pool.QueryRow(ctx, `SELECT favicon_hash FROM urls WHERE host = $1`, host).Scan(&fav); err != nil {
		t.Fatal(err)
	}
	if fav != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("hash %q", fav)
	}
	for _, p := range paths {
		if p != "/" && p != "/favicon.ico" {
			t.Fatalf("disallowed path requested: %q in %v", p, paths)
		}
	}
	if len(paths) != 2 || paths[0] != "/" || paths[1] != "/favicon.ico" {
		t.Fatalf("paths %v", paths)
	}
}

func TestT45MigrateDoesNotProbeOrBackfill(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	host := "old." + strings.ToLower(t.Name()) + ".example.test"
	var aid, uid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, alive) VALUES ($1, $2, '{http}', 'alive') RETURNING id`,
		e.rootID, host).Scan(&aid); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, tech, sources)
		VALUES ($1, 'https', $2, 443, 200, 'legacy', '{}', '{http}') RETURNING id`, aid, host).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	var scansBefore int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks`).Scan(&scansBefore)
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err := migrate(ctx, conn.Conn()); err != nil {
		t.Fatal(err)
	}
	var deep *time.Time
	var headers map[string]string
	if err := e.pool.QueryRow(ctx, `SELECT deep_probed_at, http_headers FROM urls WHERE id = $1`, uid).Scan(&deep, &headers); err != nil {
		t.Fatal(err)
	}
	if deep != nil {
		t.Fatalf("migrate must not mark historical URL deep_probed: %v", deep)
	}
	var scansAfter int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks`).Scan(&scansAfter)
	if scansAfter != scansBefore {
		t.Fatalf("migrate inserted scan_tasks: %d -> %d", scansBefore, scansAfter)
	}
}
