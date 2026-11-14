package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	wappalyzer "github.com/projectdiscovery/wappalyzergo"
)

func TestT47HiddenCategoriesLocked(t *testing.T) {
	want := []string{
		"JavaScript libraries", "JavaScript frameworks", "UI frameworks", "Font scripts",
		"Analytics", "Tag managers", "Advertising", "CDN", "PaaS", "IaaS",
		"Cookie compliance", "CAPTCHAs", "WAF", "Rich text editors", "Maps",
		"Documentation", "Static site generator",
	}
	if len(hiddenCategories) != len(want) {
		t.Fatalf("hiddenCategories len %d != %d: %v", len(hiddenCategories), len(want), hiddenCategories)
	}
	for i := range want {
		if hiddenCategories[i] != want[i] {
			t.Fatalf("hiddenCategories[%d]=%q want %q", i, hiddenCategories[i], want[i])
		}
	}
}

func TestT47IsHiddenByCategories(t *testing.T) {
	if isHiddenByCategories(nil) || isHiddenByCategories([]string{}) {
		t.Fatal("empty should show")
	}
	if !isHiddenByCategories([]string{"JavaScript libraries"}) {
		t.Fatal("pure frontend should hide")
	}
	if !isHiddenByCategories([]string{"JavaScript frameworks", "Static site generator"}) {
		t.Fatal("all hidden should hide")
	}
	if isHiddenByCategories([]string{"JavaScript frameworks", "Web frameworks"}) {
		t.Fatal("cross-class should show")
	}
	if isHiddenByCategories([]string{"Security"}) {
		t.Fatal("unknown-to-hide-set category should show")
	}
}

func TestT47FingerprintWithInfoHasCategories(t *testing.T) {
	w, err := wappalyzer.New()
	if err != nil {
		t.Fatal(err)
	}
	info := w.FingerprintWithInfo(map[string][]string{"Server": {"nginx"}}, []byte(`<script src="/jquery-3.6.0.min.js"></script>`))
	var nginx, jquery bool
	for tech, app := range info {
		name, _ := parseTech(tech)
		cats := strings.Join(sortedCats(app.Categories), ",")
		if strings.EqualFold(name, "Nginx") {
			nginx = true
			if !contains(app.Categories, "Web servers") && !strings.Contains(cats, "Web servers") {
				t.Fatalf("Nginx cats %v", app.Categories)
			}
		}
		if strings.EqualFold(name, "jQuery") {
			jquery = true
			if !contains(app.Categories, "JavaScript libraries") && !strings.Contains(cats, "JavaScript libraries") {
				t.Fatalf("jQuery cats %v", app.Categories)
			}
		}
	}
	if !nginx {
		t.Fatalf("expected Nginx in FingerprintWithInfo, got %v", keysOfInfo(info))
	}
	// jQuery 视指纹库而定；分类名查找必须锁定。
	if cats := categoriesForName("jQuery"); strings.Join(cats, ",") != "JavaScript libraries" {
		t.Fatalf("jQuery lookup %v", cats)
	}
	if cats := categoriesForName("Django"); !contains(cats, "Web frameworks") {
		t.Fatalf("Django lookup %v", cats)
	}
	next := categoriesForName("Next.js")
	if !contains(next, "JavaScript frameworks") || !contains(next, "Web frameworks") {
		t.Fatalf("Next.js should be cross-class, got %v", next)
	}
	if jquery {
		t.Log("jquery also fingerprinted from script src")
	}
}

func keysOfInfo(m map[string]wappalyzer.AppInfo) []string {
	var k []string
	for t := range m {
		k = append(k, t)
	}
	return k
}

func TestT47BackfillIdempotentNoNetwork(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	// 清空后再回填，验证按名字补分类；第二次无变更。
	for _, name := range []string{"jQuery", "Django", "Next.js", "NoSuchComp" + t.Name()} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO components (name, name_key, source, confirmed, categories)
			VALUES ($1, $2, 'wappalyzer', true, '{}')
			ON CONFLICT (name_key) DO UPDATE SET categories = '{}'`, name, strings.ToLower(name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM components WHERE name_key = $1`, strings.ToLower("NoSuchComp"+t.Name()))
	})
	n, err := backfillComponentCategories(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("first backfill updated %d, want at least jQuery/Django/Next.js", n)
	}
	assertCats := func(name string, want ...string) {
		t.Helper()
		var cats []string
		if err := e.pool.QueryRow(ctx, `SELECT categories FROM components WHERE name_key=$1`, strings.ToLower(name)).Scan(&cats); err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if !contains(cats, w) {
				t.Fatalf("%s cats %v missing %s", name, cats, w)
			}
		}
	}
	assertCats("jQuery", "JavaScript libraries")
	assertCats("Django", "Web frameworks")
	assertCats("Next.js", "JavaScript frameworks", "Web frameworks")
	var empty []string
	if err := e.pool.QueryRow(ctx, `SELECT categories FROM components WHERE name_key=$1`, strings.ToLower("NoSuchComp"+t.Name())).Scan(&empty); err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown name should stay empty, got %v", empty)
	}
	n2, err := backfillComponentCategories(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("idempotent backfill should update 0, got %d", n2)
	}
}

func seedT47CategoryFixture(t *testing.T, e *testEnv) (front, srv, cross, empty, other string) {
	t.Helper()
	ctx := context.Background()
	pfx := strings.ToLower(t.Name())
	front, srv, cross, empty, other = "FrontLib"+t.Name(), "SrvFrame"+t.Name(), "CrossSSR"+t.Name(), "EmptyCat"+t.Name(), "SecOnly"+t.Name()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, n := range []string{front, srv, cross, empty, other} {
			e.pool.Exec(ctx, `DELETE FROM components WHERE name_key = $1`, strings.ToLower(n))
		}
	})
	var vid, rid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO vendors (name) VALUES ($1) RETURNING id`, "t47-"+t.Name()).Scan(&vid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM vendors WHERE id = $1`, vid) })
	if err := e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, $2) RETURNING id`,
		vid, pfx+".example.test").Scan(&rid); err != nil {
		t.Fatal(err)
	}
	insert := func(host, name string, cats []string) {
		t.Helper()
		if cats == nil {
			cats = []string{}
		}
		var aid, uid, cid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, alive) VALUES ($1, $2, '{http}', true, 'alive') RETURNING id`,
			rid, host).Scan(&aid); err != nil {
			t.Fatal(err)
		}
		if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, sources) VALUES ($1, 'https', $2, 443, 200, '', '{http}') RETURNING id`,
			aid, host).Scan(&uid); err != nil {
			t.Fatal(err)
		}
		if err := e.pool.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed, categories)
			VALUES ($1, $2, 'wappalyzer', true, $3) RETURNING id`, name, strings.ToLower(name), cats).Scan(&cid); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO url_components (url_id, component_id, version, source, evidence) VALUES ($1, $2, '', 'wappalyzer', '{}')`, uid, cid); err != nil {
			t.Fatal(err)
		}
	}
	insert("front."+pfx+".example.test", front, []string{"JavaScript libraries"})
	insert("srv."+pfx+".example.test", srv, []string{"Web frameworks"})
	insert("cross."+pfx+".example.test", cross, []string{"JavaScript frameworks", "Web frameworks"})
	insert("empty."+pfx+".example.test", empty, []string{})
	insert("sec."+pfx+".example.test", other, []string{"Security"})
	return
}

func TestT47ListDefaultHideAndFilters(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	if _, err := seedCommonComponents(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	_ = a
	front, srv, cross, empty, other := seedT47CategoryFixture(t, e)

	names := namesOf(items(c.want(200, "GET", "/api/components", nil)))
	if contains(names, front) {
		t.Fatalf("default should hide pure frontend, got %v", names)
	}
	for _, n := range []string{srv, cross, empty, other} {
		if !contains(names, n) {
			t.Fatalf("default should include %s (server/cross/empty/other), got %v", n, names)
		}
	}

	all := namesOf(items(c.want(200, "GET", "/api/components?include_all_categories=true", nil)))
	if !contains(all, front) || !contains(all, srv) || !contains(all, cross) {
		t.Fatalf("include_all should list frontend too, got %v", all)
	}

	onlyJS := namesOf(items(c.want(200, "GET", "/api/components?category=JavaScript+libraries", nil)))
	if !contains(onlyJS, front) || contains(onlyJS, srv) {
		t.Fatalf("category=JavaScript libraries got %v", onlyJS)
	}
	onlyWF := namesOf(items(c.want(200, "GET", "/api/components?category=Web+frameworks", nil)))
	if !contains(onlyWF, srv) || !contains(onlyWF, cross) || contains(onlyWF, front) || contains(onlyWF, empty) {
		t.Fatalf("category=Web frameworks got %v", onlyWF)
	}
	multi := namesOf(items(c.want(200, "GET", "/api/components?category=JavaScript+libraries&category=Web+frameworks", nil)))
	if !contains(multi, front) || !contains(multi, srv) || !contains(multi, cross) {
		t.Fatalf("multi category got %v", multi)
	}

	// 计数随本组件口径：每件 1 url / 1 asset / 1 vendor；下钻主机名一致。
	out := c.want(200, "GET", "/api/components?q="+srv, nil)
	list := items(out)
	if len(list) != 1 {
		t.Fatalf("q=srv %v", list)
	}
	if intOf(list[0]["url_count"]) != 1 || intOf(list[0]["asset_count"]) != 1 || intOf(list[0]["vendor_count"]) != 1 {
		t.Fatalf("counts %+v", list[0])
	}
	if !contains(stringsOf(list[0]["categories"]), "Web frameworks") {
		t.Fatalf("categories field %+v", list[0]["categories"])
	}
	id := intOf(list[0]["id"])
	assets := items(c.want(200, "GET", fmt.Sprintf("/api/components/%d/assets", id), nil))
	if len(assets) != 1 || assets[0]["subdomain"] != "srv."+strings.ToLower(t.Name())+".example.test" {
		t.Fatalf("drill %+v", assets)
	}
	detail := c.want(200, "GET", fmt.Sprintf("/api/components/%d", id), nil)
	if !contains(stringsOf(detail["categories"]), "Web frameworks") {
		t.Fatalf("detail categories %+v", detail)
	}

	// MCP 同步
	sess, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	mcpNames := func(out map[string]any) []string {
		var ns []string
		for _, it := range out["items"].([]any) {
			ns = append(ns, it.(map[string]any)["name"].(string))
		}
		return ns
	}
	got, isErr := callTool(t, sess, "search_components", map[string]any{})
	if isErr {
		t.Fatalf("search_components %v", got)
	}
	mn := mcpNames(got)
	if contains(mn, front) || !contains(mn, srv) || !contains(mn, cross) {
		t.Fatalf("mcp default %v", mn)
	}
	got, isErr = callTool(t, sess, "search_components", map[string]any{"include_all_categories": true})
	if isErr {
		t.Fatalf("mcp all %v", got)
	}
	if !contains(mcpNames(got), front) {
		t.Fatalf("mcp include_all %v", mcpNames(got))
	}
	got, isErr = callTool(t, sess, "search_components", map[string]any{"category": []any{"JavaScript libraries"}})
	if isErr {
		t.Fatalf("mcp category %v", got)
	}
	if !contains(mcpNames(got), front) || contains(mcpNames(got), srv) {
		t.Fatalf("mcp category %v", mcpNames(got))
	}
}

func TestT47HTTPScanWritesCategories(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	host := "scan." + strings.ToLower(t.Name()) + ".example.test"
	if _, err := e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, alive) VALUES ($1, $2, '{http}', true, 'alive')`, e.rootID, host); err != nil {
		t.Fatal(err)
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/favicon.ico" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Server", "nginx")
		io.WriteString(w, `<html><head><title>Default Site</title></head><body><script src="/jquery-3.6.0.min.js"></script></body></html>`)
	}))
	t.Cleanup(page.Close)
	cfg, _ := httpConfigFromEnv()
	cfg.RPS, cfg.Schemes, cfg.Timeout = 1000, []string{"http"}, 2*time.Second
	cfg.Dial = func(string) string { return page.Listener.Addr().String() }
	p := newProber(cfg)
	if _, err := httpHandler(e.pool, p)(ctx, 0, []string{host}); err != nil {
		t.Fatal(err)
	}
	var cats []string
	err := e.pool.QueryRow(ctx, `SELECT c.categories FROM components c WHERE c.name_key = 'nginx'`).Scan(&cats)
	if err != nil {
		t.Fatalf("nginx component missing after scan: %v", err)
	}
	if !contains(cats, "Web servers") {
		t.Fatalf("scan should write Nginx categories, got %v", cats)
	}
}
