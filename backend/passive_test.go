package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projectdiscovery/subfinder/v2/pkg/passive"
	"github.com/projectdiscovery/subfinder/v2/pkg/subscraping"
)

// mockAPIs 模拟 crt.sh / FOFA / Quake / Hunter，校验请求参数，记录每个路径的请求次数。
func mockAPIs(t *testing.T) (*httptest.Server, map[string]int, *sync.Mutex) {
	calls := map[string]int{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		q := r.URL.Query()
		switch r.URL.Path {
		case "/": // crt.sh
			if q.Get("q") != "%.example.test" || q.Get("output") != "json" {
				t.Errorf("crtsh query %v", q)
			}
			json.NewEncoder(w).Encode([]map[string]string{
				{"name_value": "a.example.test\n*.b.example.test"},
				{"name_value": "A.Example.Test"},
				{"name_value": "other.test"},
			})
		case "/api/v1/search/all": // FOFA
			qb, _ := base64.StdEncoding.DecodeString(q.Get("qbase64"))
			if q.Get("key") != "fk" || string(qb) != `domain="example.test"` || q.Get("fields") != "host" {
				t.Errorf("fofa query %v %s", q, qb)
			}
			page, _ := strconv.Atoi(q.Get("page"))
			res := []any{"https://f1.example.test:8443", []string{"f2.example.test"}} // 满页（size=2）
			if page == 2 {
				res = []any{"f3.example.test"}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": false, "results": res})
		case "/api/v3/search/quake_service":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if r.Method != "POST" || r.Header.Get("X-QuakeToken") != "qk" || body["query"] != `domain:"example.test"` {
				t.Errorf("quake %s %v %v", r.Method, r.Header, body)
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []any{
				map[string]any{"domain": "q1.example.test", "service": map[string]any{"http": map[string]any{"host": "q2.example.test"}}},
			}})
		case "/openApi/search": // Hunter
			qb, _ := base64.URLEncoding.DecodeString(q.Get("search"))
			if q.Get("api-key") != "hk" || string(qb) != `domain.suffix="example.test"` {
				t.Errorf("hunter query %v %s", q, qb)
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"arr": []any{
				map[string]any{"domain": "h1.example.test"}, map[string]any{"domain": "a.example.test"},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls, &mu
}

func mockConfig(base string) passiveConfig {
	return passiveConfig{
		Pages: 1, PageSize: 2, CrtshURL: base, FofaURL: base, QuakeURL: base, HuntURL: base,
		HTTP: &http.Client{Timeout: 5 * time.Second},
	}
}

func names(srcs []Source) []string {
	var n []string
	for _, s := range srcs {
		n = append(n, s.Name)
	}
	return n
}

func TestPassiveSourcesSkipWithoutKey(t *testing.T) {
	c := passiveConfig{Subfinder: true}
	if got := names(passiveSources(c)); !reflect.DeepEqual(got, []string{"crtsh", "subfinder"}) {
		t.Fatalf("no keys: %v", got)
	}
	c.FofaKey, c.HunterKey = "f", "h"
	if got := names(passiveSources(c)); !reflect.DeepEqual(got, []string{"crtsh", "subfinder", "fofa", "hunter"}) {
		t.Fatalf("fofa+hunter keys: %v", got)
	}
	c.QuakeKey = "q"
	if got := names(passiveSources(c)); len(got) != 5 {
		t.Fatalf("all keys: %v", got)
	}
}

func TestPassiveConfigFromEnv(t *testing.T) {
	t.Setenv("FOFA_KEY", "")
	c, err := passiveConfigFromEnv()
	if err != nil || c.Pages != 1 || c.PageSize != 100 || c.FofaKey != "" {
		t.Fatalf("defaults %+v %v", c, err)
	}
	t.Setenv("PASSIVE_PAGES", "3")
	t.Setenv("FOFA_KEY", "k")
	if c, _ = passiveConfigFromEnv(); c.Pages != 3 || c.FofaKey != "k" {
		t.Fatalf("overrides %+v", c)
	}
	t.Setenv("PASSIVE_PAGES", "0")
	if _, err := passiveConfigFromEnv(); err == nil {
		t.Fatal("PASSIVE_PAGES=0 should be rejected")
	}
	t.Setenv("PASSIVE_PAGES", "")
	if c, _ = passiveConfigFromEnv(); c.SubfinderMaxTime != 5*time.Minute || c.SubfinderTimeout != 30*time.Second {
		t.Fatalf("subfinder defaults %+v", c)
	}
	t.Setenv("SUBFINDER_MAX_TIME", "2m")
	t.Setenv("SUBFINDER_TIMEOUT", "10s")
	if c, _ = passiveConfigFromEnv(); c.SubfinderMaxTime != 2*time.Minute || c.SubfinderTimeout != 10*time.Second {
		t.Fatalf("subfinder overrides %+v", c)
	}
	for _, v := range []string{"abc", "0s", "-5s", "500ms", "1.5s"} {
		for _, k := range []string{"SUBFINDER_MAX_TIME", "SUBFINDER_TIMEOUT"} {
			t.Setenv(k, v)
			if _, err := passiveConfigFromEnv(); err == nil {
				t.Errorf("%s=%s should be rejected", k, v)
			}
			t.Setenv(k, "30s")
		}
	}
}

func TestSubfinderFreeSources(t *testing.T) {
	free := subfinderFreeSources()
	if len(free) == 0 {
		t.Fatal("no free sources")
	}
	byName := map[string]subscraping.Source{}
	for _, s := range passive.AllSources {
		byName[s.Name()] = s
	}
	for _, n := range free {
		if n == "crtsh" || byName[n].KeyRequirement() != subscraping.NoKey {
			t.Errorf("%s should not be in free list", n)
		}
	}
	t.Logf("subfinder free sources (%d): %s", len(free), strings.Join(free, ","))
}

// 全部来源走 mock：去重、规范化、来源记录、翻页、无关域过滤。subfinder 用假源替代（不联网）。
func TestCollectPassiveMock(t *testing.T) {
	srv, calls, mu := mockAPIs(t)
	c := mockConfig(srv.URL)
	c.FofaKey, c.QuakeKey, c.HunterKey = "fk", "qk", "hk"
	c.Pages = 3
	srcs := append(passiveSources(c), Source{"subfinder", func(context.Context, string) ([]string, error) {
		return []string{"s1.example.test", "a.example.test", "evil-example.test"}, nil
	}})
	found, err := collectPassive(context.Background(), "Example.Test", srcs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"a.example.test":  {"crtsh", "hunter", "subfinder"},
		"b.example.test":  {"crtsh"},
		"f1.example.test": {"fofa"},
		"f2.example.test": {"fofa"},
		"f3.example.test": {"fofa"},
		"q1.example.test": {"quake"},
		"q2.example.test": {"quake"},
		"h1.example.test": {"hunter"},
		"s1.example.test": {"subfinder"},
	}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("got  %v\nwant %v", found, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["/api/v1/search/all"] != 2 { // 第 2 页不满，停止
		t.Errorf("fofa pages = %d, want 2", calls["/api/v1/search/all"])
	}
	if calls["/api/v3/search/quake_service"] != 1 { // 首页即不满
		t.Errorf("quake calls %v", calls)
	}
	if calls["/openApi/search"] != 3 { // hunter 每页都满，受 Pages=3 上限
		t.Errorf("hunter calls %v", calls)
	}
}

func TestCollectPassiveErrors(t *testing.T) {
	ok := Source{"ok", func(context.Context, string) ([]string, error) { return []string{"x.example.test"}, nil }}
	bad := Source{"bad", func(context.Context, string) ([]string, error) { return nil, errors.New("boom") }}
	found, err := collectPassive(context.Background(), "example.test", []Source{bad, ok})
	if err != nil || len(found) != 1 {
		t.Fatalf("one failing source should not fail: %v %v", found, err)
	}
	if _, err := collectPassive(context.Background(), "example.test", []Source{bad}); err == nil {
		t.Fatal("all sources failing should error")
	}
	// HTTP 错误码与 API 错误
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/search/all" {
			json.NewEncoder(w).Encode(map[string]any{"error": true, "errmsg": "bad key"})
			return
		}
		w.WriteHeader(502)
	}))
	defer srv.Close()
	c := mockConfig(srv.URL)
	if _, err := c.crtsh(context.Background(), "example.test"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("crtsh 502: %v", err)
	}
	c.FofaKey = "x"
	if _, err := c.fofa(context.Background(), "example.test"); err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("fofa error: %v", err)
	}
}

// 入库：对测试根域跑 passive（全 mock），资产入库并记录来源；归属最长匹配根域；范围判定；二次扫描合并来源、first_seen 不变。
func TestPassiveHandlerStores(t *testing.T) {
	e := newTestEnv(t) // 根域 <name>.example.test，见 queue_test.go
	ctx := context.Background()
	var root string
	var vendorA int64
	e.pool.QueryRow(ctx, `SELECT domain, vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&root, &vendorA) // 入库层已小写
	e.pool.Exec(ctx, `INSERT INTO scope_rules (vendor_id, kind, pattern) VALUES ($1, 'include', $2), ($1, 'exclude', $3)`,
		vendorA, "*."+root, "dev."+root)
	// 另一厂商拥有更长的根域 sub.<root>
	e.pool.Exec(ctx, `DELETE FROM vendors WHERE name = 't5-vendor-b'`)
	var subRootID int64
	e.pool.QueryRow(ctx, `WITH v AS (INSERT INTO vendors (name) VALUES ('t5-vendor-b') RETURNING id)
		INSERT INTO root_domains (vendor_id, domain) SELECT id, $1 FROM v RETURNING id`, "sub."+root).Scan(&subRootID)
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM vendors WHERE name = 't5-vendor-b'`) })

	var scanID int64
	e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind) VALUES ($1, $2, 'passive') RETURNING id`,
		vendorA, e.rootID).Scan(&scanID)
	fake := func(name string, hosts ...string) Source {
		return Source{name, func(context.Context, string) ([]string, error) { return hosts, nil }}
	}
	out, err := passiveHandler(e.pool, []Source{
		fake("crtsh", "www."+root, "dev."+root, "x.sub."+root, "unrelated.test"),
		fake("subfinder", "www."+root),
	})(ctx, scanID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dev." + root, "www." + root, "x.sub." + root}; !reflect.DeepEqual(out, want) {
		t.Fatalf("handler output %v, want %v", out, want)
	}
	type asset struct {
		root    int64
		sources []string
		inScope *bool
		first   time.Time
		last    time.Time
	}
	get := func(host string) asset {
		var a asset
		if err := e.pool.QueryRow(ctx, `SELECT root_id, sources, in_scope, first_seen, last_seen FROM assets WHERE subdomain = $1`, host).
			Scan(&a.root, &a.sources, &a.inScope, &a.first, &a.last); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		return a
	}
	www := get("www." + root)
	if www.root != e.rootID || !reflect.DeepEqual(www.sources, []string{"crtsh", "subfinder"}) || !*www.inScope {
		t.Errorf("www: %+v", www)
	}
	if dev := get("dev." + root); *dev.inScope {
		t.Errorf("dev should be excluded: %+v", dev)
	}
	if x := get("x.sub." + root); x.root != subRootID || *x.inScope { // 归 vendor B，无规则 → 不在范围
		t.Errorf("x.sub: %+v", x)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE subdomain = 'unrelated.test'`).Scan(&n)
	if n != 0 {
		t.Error("unrelated host stored")
	}

	time.Sleep(20 * time.Millisecond)
	if _, err := passiveHandler(e.pool, []Source{fake("fofa", "WWW."+root)})(ctx, scanID, nil); err != nil {
		t.Fatal(err)
	}
	www2 := get("www." + root)
	if !reflect.DeepEqual(www2.sources, []string{"crtsh", "fofa", "subfinder"}) || !www2.first.Equal(www.first) || !www2.last.After(www.last) {
		t.Errorf("second scan: before %+v after %+v", www, www2)
	}
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE '%' || $1`, root)
	})
}

// 入库层规范化：大写/末尾点的根域按小写存储，被动收集能匹配并入库。
func TestUppercaseRootMatches(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	e.pool.Exec(ctx, `DELETE FROM root_domains WHERE domain = 'upper.test'`)
	var rootID int64
	var stored string
	if err := e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, ' UPPER.Test. ') RETURNING id, domain`,
		vendorID).Scan(&rootID, &stored); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM root_domains WHERE id = $1`, rootID) })
	if stored != "upper.test" {
		t.Fatalf("stored root %q", stored)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'upper.TEST')`, vendorID); err == nil {
		t.Fatal("case-variant duplicate root should violate unique key")
	}
	var scanID int64
	e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind) VALUES ($1, $2, 'passive') RETURNING id`, vendorID, rootID).Scan(&scanID)
	src := Source{"crtsh", func(context.Context, string) ([]string, error) { return []string{"WWW.Upper.Test."}, nil }}
	if _, err := passiveHandler(e.pool, []Source{src})(ctx, scanID, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE subdomain = 'www.upper.test' AND root_id = $1`, rootID).Scan(&n)
	if n != 1 {
		t.Fatalf("www.upper.test not stored under root (n=%d)", n)
	}
	// 直接写入大写子域也被规范化
	e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, 'API.UPPER.TEST.')`, rootID)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE subdomain = 'api.upper.test'`).Scan(&n)
	if n != 1 {
		t.Fatal("asset subdomain not normalized")
	}
}
