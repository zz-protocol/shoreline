package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestParseTech(t *testing.T) {
	cases := []struct{ in, name, ver string }{
		{"Nginx", "Nginx", ""},
		{"Nginx:1.18.0", "Nginx", "1.18.0"},
		{"  Vue.js:3.2  ", "Vue.js", "3.2"},
		{"Apache HTTP Server", "Apache HTTP Server", ""},
		{"", "", ""},
		{"  :1.0", "", "1.0"},
	}
	for _, c := range cases {
		n, v := parseTech(c.in)
		if n != c.name || v != c.ver {
			t.Errorf("parseTech(%q)=%q,%q want %q,%q", c.in, n, v, c.name, c.ver)
		}
	}
}

func TestBuiltinCommonNamesLocked(t *testing.T) {
	want := []string{
		"Nginx", "Apache HTTP Server", "IIS", "OpenResty", "Cloudflare", "Akamai",
		"jQuery", "Bootstrap", "React", "Vue.js", "Angular", "Font Awesome", "PHP", "OpenSSL",
		"Google Analytics", "Google Tag Manager", "Webpack",
		"Amazon CloudFront", "Fastly", "jsDelivr", "cdnjs", "Unpkg",
		"Google Cloud CDN", "Azure CDN", "Alibaba Cloud CDN", "Tencent Cloud", "Wangsu",
		"BootstrapCDN", "jQuery CDN",
	}
	if got := append([]string{}, builtinCommonComponents...); !equalStr(got, want) {
		t.Fatalf("builtin names changed:\n got %v\nwant %v", got, want)
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// seedT43Fixture：至少 3 个厂商；常见 Nginx + 两个非常见；部分 wildcard / customer_hosted / 失活。
func seedT43Fixture(t *testing.T, e *testEnv) (rareA, rareB int64, nginx int64) {
	t.Helper()
	ctx := context.Background()
	e.pool.Exec(ctx, `UPDATE common_component_names SET deleted_at = NULL, enabled = true WHERE builtin`)
	if _, err := seedCommonComponents(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	type row struct {
		vendor, sub, tech string
		wild, hosted      bool
		alive             string
	}
	// 3 vendors: VA, VB, VC. RareCMS on VA+VB (alive); RareLib on VA+VB+VC (alive).
	// Nginx on many including wildcard/hosted/dead should not count by default.
	pfx := strings.ToLower(t.Name())
	rows := []row{
		{"VA", pfx + "-a.va.example.test", "RareCMS,Nginx:1.18", false, false, "alive"},
		{"VB", pfx + "-b.vb.example.test", "RareCMS,RareLib", false, false, "alive"},
		{"VC", pfx + "-c.vc.example.test", "RareLib", false, false, "alive"},
		{"VA", pfx + "-n.va.example.test", "Nginx", false, false, "alive"},
		{"VB", pfx + "-n.vb.example.test", "Nginx:1.24", false, false, "alive"},
		{"VA", pfx + "-w.va.example.test", "RareCMS,Nginx", true, false, "alive"},   // wildcard
		{"VB", pfx + "-h.vb.example.test", "RareLib,Nginx", false, true, "alive"},   // customer_hosted
		{"VC", pfx + "-d.vc.example.test", "RareCMS,RareLib", false, false, "dead"}, // 失活
	}
	vendors := map[string]int64{}
	for _, r := range rows {
		if _, ok := vendors[r.vendor]; !ok {
			var vid, rid int64
			if err := e.pool.QueryRow(ctx, `INSERT INTO vendors (name) VALUES ($1) RETURNING id`, "t43-"+t.Name()+"-"+r.vendor).Scan(&vid); err != nil {
				t.Fatal(err)
			}
			if err := e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, $2) RETURNING id`,
				vid, strings.ToLower(r.vendor)+"."+t.Name()+".example.test").Scan(&rid); err != nil {
				t.Fatal(err)
			}
			vendors[r.vendor] = rid
			cleanupID := vid
			t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM vendors WHERE id = $1`, cleanupID) })
		}
		var aid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, wildcard, customer_hosted, alive)
			VALUES ($1, $2, '{http}', $3, $4, $5) RETURNING id`,
			vendors[r.vendor], r.sub, r.wild, r.hosted, r.alive).Scan(&aid); err != nil {
			t.Fatal(err)
		}
		tech := strings.Split(r.tech, ",")
		var uid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, tech, sources)
			VALUES ($1, 'https', $2, 443, 200, '', $3, '{http}') RETURNING id`, aid, r.sub, tech).Scan(&uid); err != nil {
			t.Fatal(err)
		}
		if err := syncURLComponents(ctx, e.pool, uid, tech); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.pool.QueryRow(ctx, `SELECT id FROM components WHERE name_key = 'rarecms'`).Scan(&rareA); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT id FROM components WHERE name_key = 'rarelib'`).Scan(&rareB); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT id FROM components WHERE name_key = 'nginx'`).Scan(&nginx); err != nil {
		t.Fatal(err)
	}
	return
}

func TestT43InventoryFiltersAndCounts(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	if _, err := seedCommonComponents(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	_ = a
	rareA, rareB, nginx := seedT43Fixture(t, e)
	pfx := strings.ToLower(t.Name())

	out := c.want(200, "GET", "/api/components", nil)
	list := items(out)
	var names []string
	by := map[string]map[string]any{}
	for _, m := range list {
		n := m["name"].(string)
		names = append(names, n)
		by[n] = m
	}
	if contains(names, "Nginx") {
		t.Fatalf("default list should exclude common Nginx, got %v", names)
	}
	if !contains(names, "RareCMS") || !contains(names, "RareLib") {
		t.Fatalf("default should have uncommon only, got %v", names)
	}
	// RareCMS: alive non-wild non-hosted = a.va + b.vb → 2 urls, 2 assets, 2 vendors (wildcard/dead excluded)
	if intOf(by["RareCMS"]["url_count"]) != 2 || intOf(by["RareCMS"]["asset_count"]) != 2 || intOf(by["RareCMS"]["vendor_count"]) != 2 {
		t.Fatalf("RareCMS counts %+v", by["RareCMS"])
	}
	// RareLib: b.vb + c.vc → 2 urls, 2 assets, 2 vendors (hosted/dead excluded)
	if intOf(by["RareLib"]["url_count"]) != 2 || intOf(by["RareLib"]["asset_count"]) != 2 || intOf(by["RareLib"]["vendor_count"]) != 2 {
		t.Fatalf("RareLib counts %+v", by["RareLib"])
	}
	// 排序：厂商数相同则资产数降序；相同则 id 降序。两者都是 2/2，只检查都在。
	if out["next_cursor"] != nil {
		t.Fatalf("unexpected cursor %v", out["next_cursor"])
	}

	// 关闭排除常见：出现 Nginx。默认口径下 Nginx 来自 n.va + n.vb（alive, 非 wild/hosted）= 2/2/2
	out = c.want(200, "GET", "/api/components?exclude_common=false", nil)
	list = items(out)
	names = field(list, "name")
	if !contains(names, "Nginx") {
		t.Fatalf("include common: %v", names)
	}

	// 含泛解析：RareCMS 再计入 w.va → 3 urls, 3 assets, 仍 2 vendors
	out = c.want(200, "GET", "/api/components?q=RareCMS&include_wildcard=true", nil)
	m := items(out)[0]
	if intOf(m["url_count"]) != 3 || intOf(m["asset_count"]) != 3 || intOf(m["vendor_count"]) != 2 {
		t.Fatalf("wildcard RareCMS %+v", m)
	}

	// 含客户托管：RareLib 再计入 h.vb → 3/3/2
	out = c.want(200, "GET", "/api/components?q=RareLib&include_customer_hosted=true", nil)
	m = items(out)[0]
	if intOf(m["url_count"]) != 3 || intOf(m["asset_count"]) != 3 || intOf(m["vendor_count"]) != 2 {
		t.Fatalf("hosted RareLib %+v", m)
	}

	// 含失活：RareCMS 再计入 d.vc → 3/3/3
	out = c.want(200, "GET", "/api/components?q=RareCMS&include_dead=true", nil)
	m = items(out)[0]
	if intOf(m["url_count"]) != 3 || intOf(m["asset_count"]) != 3 || intOf(m["vendor_count"]) != 3 {
		t.Fatalf("dead RareCMS %+v", m)
	}

	// 下钻与计数一致
	out = c.want(200, "GET", fmt.Sprintf("/api/components/%d/assets", rareA), nil)
	got := items(out)
	if len(got) != 2 {
		t.Fatalf("drill RareCMS default %d items: %v", len(got), got)
	}
	var subs []string
	for _, it := range got {
		subs = append(subs, it["subdomain"].(string))
	}
	sort.Strings(subs)
	wantSubs := pfx + "-a.va.example.test," + pfx + "-b.vb.example.test"
	if strings.Join(subs, ",") != wantSubs {
		t.Fatalf("drill subs %v", subs)
	}

	// 复制与筛选一致
	out = c.want(200, "GET", fmt.Sprintf("/api/components/%d/values?field=subdomain", rareA), nil)
	if intOf(out["count"]) != 2 || out["truncated"] != false {
		t.Fatalf("values %+v", out)
	}
	vals := stringsOf(out["items"])
	sort.Strings(vals)
	if strings.Join(vals, ",") != wantSubs {
		t.Fatalf("values items %v", vals)
	}
	out = c.want(200, "GET", fmt.Sprintf("/api/components/%d/values?field=subdomain&include_dead=true", rareA), nil)
	if intOf(out["count"]) != 3 {
		t.Fatalf("values with dead %+v", out)
	}

	// 下钻按厂商
	var va int64
	if err := e.pool.QueryRow(context.Background(), `SELECT vendor_id FROM root_domains WHERE domain = $1`,
		"va."+strings.ToLower(t.Name())+".example.test").Scan(&va); err != nil || va < 1 {
		t.Fatalf("lookup vendor: %v id=%d", err, va)
	}
	out = c.want(200, "GET", fmt.Sprintf("/api/components/%d/assets?vendor=%d", rareA, va), nil)
	if len(items(out)) != 1 || items(out)[0]["subdomain"] != pfx+"-a.va.example.test" {
		t.Fatalf("vendor filter %+v", out)
	}

	// 详情
	out = c.want(200, "GET", fmt.Sprintf("/api/components/%d", rareB), nil)
	if out["name"] != "RareLib" || intOf(out["vendor_count"]) != 2 {
		t.Fatalf("detail %+v", out)
	}
	_ = nginx
}

func TestT43CommonListSeedAndTombstone(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	e.pool.Exec(ctx, `UPDATE common_component_names SET deleted_at = NULL, enabled = true WHERE builtin`)
	e.pool.Exec(ctx, `UPDATE common_component_names SET deleted_at = now() WHERE NOT builtin`)
	if _, err := seedCommonComponents(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	n, err := seedCommonComponents(ctx, e.pool)
	if err != nil || n != 0 {
		t.Fatalf("idempotent seed n=%d err=%v", n, err)
	}
	var live int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM common_component_names WHERE builtin AND deleted_at IS NULL`).Scan(&live)
	if live != len(builtinCommonComponents) {
		t.Fatalf("live builtin %d want %d", live, len(builtinCommonComponents))
	}
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `UPDATE common_component_names SET deleted_at = NULL, enabled = true WHERE builtin`)
		e.pool.Exec(context.Background(), `UPDATE common_component_names SET deleted_at = now() WHERE NOT builtin`)
	})
	list := items(c.want(200, "GET", "/api/common-components", nil))
	if len(list) != len(builtinCommonComponents) {
		t.Fatalf("list %d", len(list))
	}
	var nginxID int64
	for _, m := range list {
		if fmt.Sprint(m["name"]) == "Nginx" {
			nginxID = intOf(m["id"])
		}
	}
	if nginxID == 0 {
		t.Fatal("Nginx not in list")
	}
	c.want(204, "DELETE", fmt.Sprintf("/api/common-components/%d", nginxID), nil)
	n, err = seedCommonComponents(ctx, e.pool)
	if err != nil || n != 0 {
		t.Fatalf("reseed after delete n=%d err=%v", n, err)
	}
	var deleted int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM common_component_names WHERE name_key='nginx' AND deleted_at IS NOT NULL`).Scan(&deleted)
	if deleted != 1 {
		t.Fatal("tombstone missing")
	}
	for _, m := range items(c.want(200, "GET", "/api/common-components", nil)) {
		if m["name"] == "Nginx" {
			t.Fatal("deleted Nginx still listed")
		}
	}
	// 新增与停用
	created := c.want(201, "POST", "/api/common-components", map[string]any{"name": "RareCMS"})
	id := intOf(created["id"])
	c.want(200, "PATCH", fmt.Sprintf("/api/common-components/%d", id), map[string]any{"enabled": false})
	c.want(409, "POST", "/api/common-components", map[string]any{"name": "RareCMS"})
}

func TestT43MigrationBackfillIdempotent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	// 已迁移的库：插入带 tech 的 URL，手动清空关联后重跑 020 SQL，应回填且可重复。
	var vid, rid, aid, uid int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id=$1`, e.rootID).Scan(&vid)
	rid = e.rootID
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, alive) VALUES ($1, $2, '{http}', 'alive') RETURNING id`,
		rid, "old."+t.Name()+".example.test").Scan(&aid); err != nil {
		t.Fatal(err)
	}
	tech := []string{"Nginx:1.18", "RareBackfill"}
	if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, tech, sources)
		VALUES ($1, 'https', $2, 443, 200, $3, '{http}') RETURNING id`, aid, "old."+t.Name()+".example.test", tech).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	e.pool.Exec(ctx, `DELETE FROM url_components WHERE url_id=$1`, uid)
	e.pool.Exec(ctx, `DELETE FROM components WHERE name_key IN ('nginx','rarebackfill')`)

	sql, err := os.ReadFile(filepath.Join("migrations", "020_components.sql"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	var n, n2 int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM components WHERE name_key IN ('nginx','rarebackfill')`).Scan(&n)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM url_components WHERE url_id=$1`, uid).Scan(&n2)
	if n != 2 || n2 != 2 {
		t.Fatalf("backfill components=%d links=%d", n, n2)
	}
	// 幂等再跑
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM components WHERE name_key IN ('nginx','rarebackfill')`).Scan(&n)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM url_components WHERE url_id=$1`, uid).Scan(&n2)
	if n != 2 || n2 != 2 {
		t.Fatalf("idempotent components=%d links=%d", n, n2)
	}
	var ver string
	e.pool.QueryRow(ctx, `SELECT uc.version FROM url_components uc JOIN components c ON c.id=uc.component_id
		WHERE uc.url_id=$1 AND c.name_key='nginx'`, uid).Scan(&ver)
	if ver != "1.18" {
		t.Fatalf("version %q", ver)
	}
}

func TestT43ScanSyncsAssociations(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var aid, uid int64
	host := "sync." + strings.ToLower(t.Name()) + ".example.test"
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, alive) VALUES ($1, $2, '{http}', 'alive') RETURNING id`,
		e.rootID, host).Scan(&aid); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, tech, sources)
		VALUES ($1, 'https', $2, 443, 200, $3, '{http}') RETURNING id`,
		aid, host, []string{"OldTech"}).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := syncURLComponents(ctx, e.pool, uid, []string{"OldTech"}); err != nil {
		t.Fatal(err)
	}
	p := &prober{}
	if err := p.storeURL(ctx, e.pool, aid, &probeResult{
		Scheme: "https", Host: host, Port: 443,
		Status: 200, Title: "x", Fingerprint: "abc", Tech: []string{"NewTech:2.0"},
	}); err != nil {
		t.Fatal(err)
	}
	var names []string
	rows, _ := e.pool.Query(ctx, `SELECT c.name, uc.version FROM url_components uc JOIN components c ON c.id=uc.component_id WHERE uc.url_id=$1`, uid)
	defer rows.Close()
	var ver string
	for rows.Next() {
		var n, v string
		rows.Scan(&n, &v)
		names = append(names, n)
		ver = v
	}
	if len(names) != 1 || names[0] != "NewTech" || ver != "2.0" {
		t.Fatalf("after sync names=%v ver=%q", names, ver)
	}
	// OldTech 组件行仍在（不丢组件定义），只是该 URL 不再关联
	var old int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM components WHERE name_key='oldtech'`).Scan(&old)
	if old != 1 {
		t.Fatal("component row should remain")
	}
}

func TestT43MCPMatchesREST(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	if _, err := seedCommonComponents(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	rareA, _, _ := seedT43Fixture(t, e)
	_ = a
	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callTool(t, s, "search_components", map[string]any{})
	if isErr {
		t.Fatalf("search_components %v", out)
	}
	rest := items(c.want(200, "GET", "/api/components", nil))
	mcpItems := out["items"].([]any)
	if len(mcpItems) != len(rest) {
		t.Fatalf("mcp %d rest %d", len(mcpItems), len(rest))
	}
	out, isErr = callTool(t, s, "list_component_assets", map[string]any{"component_id": rareA})
	if isErr {
		t.Fatalf("list_component_assets %v", out)
	}
	restA := items(c.want(200, "GET", fmt.Sprintf("/api/components/%d/assets", rareA), nil))
	if len(out["items"].([]any)) != len(restA) {
		t.Fatalf("assets mcp %d rest %d", len(out["items"].([]any)), len(restA))
	}
}

func intOf(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float64:
		return int64(x)
	default:
		var n int64
		fmt.Sscan(fmt.Sprint(v), &n)
		return n
	}
}

func stringsOf(v any) []string {
	var out []string
	for _, it := range v.([]any) {
		out = append(out, fmt.Sprint(it))
	}
	return out
}

func contains(ss []string, w string) bool {
	for _, s := range ss {
		if s == w {
			return true
		}
	}
	return false
}

// 保证 pgx 在测试包里被引用（迁移回填用）。
var _ = pgx.ErrNoRows
