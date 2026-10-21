package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const testBootstrap = "boot-secret"

type apiClient struct {
	t   *testing.T
	srv *httptest.Server
	tok string
}

// do 发请求，返回状态码与 JSON；非 2xx 必须是 {"error": "..."}。
func (c *apiClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	ctype := "application/json"
	switch b := body.(type) {
	case nil:
	case string:
		rd, ctype = strings.NewReader(b), "text/csv"
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rd)
	req.Header.Set("Content-Type", ctype)
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: non-json body %q", method, path, raw)
		}
	}
	if resp.StatusCode >= 300 {
		if e, ok := out["error"].(string); !ok || e == "" || len(out) != 1 {
			c.t.Fatalf("%s %s: error body %s", method, path, raw)
		}
	}
	return resp.StatusCode, out
}

func (c *apiClient) want(code int, method, path string, body any) map[string]any {
	c.t.Helper()
	got, out := c.do(method, path, body)
	if got != code {
		c.t.Fatalf("%s %s: got %d want %d: %v", method, path, got, code, out)
	}
	return out
}

func items(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, it := range m["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func field(list []map[string]any, k string) []string {
	var out []string
	for _, m := range list {
		out = append(out, fmt.Sprint(m[k]))
	}
	return out
}

func newAPITest(t *testing.T, batch int) (*testEnv, *apiClient, *api) {
	e := newTestEnv(t)
	a := &api{db: e.pool, client: e.client, cfg: apiConfig{BootstrapToken: testBootstrap, ScopeBatch: batch, MaxRetry: 1}, bg: context.Background()}
	srv := httptest.NewServer(newMux(nil, a))
	t.Cleanup(srv.Close)
	return e, &apiClient{t: t, srv: srv, tok: testBootstrap}, a
}

func (e *testEnv) vendorID() (v int64) {
	e.pool.QueryRow(context.Background(), `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&v)
	return
}

func TestOpenAPIMatchesRoutes(t *testing.T) {
	a := &api{}
	var want []string
	for k := range a.routes() {
		want = append(want, k)
	}
	want = append(want, "GET /api/openapi.yaml", "POST /mcp")
	sort.Strings(want)
	var got []string
	path := ""
	sc := bufio.NewScanner(bytes.NewReader(openapiYAML))
	inPaths := false
	for sc.Scan() {
		l := sc.Text()
		if l == "paths:" {
			inPaths = true
			continue
		}
		if !inPaths {
			continue
		}
		if strings.HasPrefix(l, "  /") && strings.HasSuffix(l, ":") {
			path = strings.TrimSuffix(strings.TrimSpace(l), ":")
		} else if m := strings.TrimSpace(l); strings.HasPrefix(l, "    ") && !strings.HasPrefix(l, "      ") &&
			strings.Contains("get: post: patch: delete: put:", m) {
			got = append(got, strings.ToUpper(strings.TrimSuffix(m, ":"))+" "+path)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("openapi paths\n%v\nroutes\n%v", got, want)
	}
	srv := httptest.NewServer(newMux(nil, a))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/openapi.yaml")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("openapi.yaml: %v %v", err, resp)
	}
	t.Logf("openapi.yaml 与路由一致：%d 个接口；无需认证可访问", len(got))
}

func TestAPIAuthAndTokens(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	anon := &apiClient{t: t, srv: c.srv}
	anon.want(401, "GET", "/api/vendors", nil)
	(&apiClient{t: t, srv: c.srv, tok: "wrong"}).want(401, "GET", "/api/vendors", nil)
	c.want(400, "POST", "/api/tokens", map[string]any{"name": " "})
	tok := c.want(201, "POST", "/api/tokens", map[string]any{"name": "t12-test"})
	plain := tok["token"].(string)
	id := int64(tok["id"].(float64))
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM api_tokens WHERE name = 't12-test'`) })
	var stored string
	e.pool.QueryRow(ctx, `SELECT token_hash FROM api_tokens WHERE id = $1`, id).Scan(&stored)
	if stored != hashToken(plain) || strings.Contains(stored, plain) {
		t.Fatalf("stored %q", stored)
	}
	user := &apiClient{t: t, srv: c.srv, tok: plain}
	user.want(200, "GET", "/api/vendors", nil)
	list := items(user.want(200, "GET", "/api/tokens", nil))
	for _, m := range list {
		if _, leak := m["token"]; leak {
			t.Fatal("token list leaks plaintext")
		}
	}
	c.want(204, "DELETE", fmt.Sprintf("/api/tokens/%d", id), nil)
	c.want(404, "DELETE", fmt.Sprintf("/api/tokens/%d", id), nil)
	user.want(401, "GET", "/api/vendors", nil)
	c.want(404, "GET", "/api/nope", nil)
	t.Logf("无 token/错 token 401；生成 token 可用、库里只存 sha256；吊销后 401")
}

func TestAPIVendorsRootsScopes(t *testing.T) {
	e, c, _ := newAPITest(t, 2) // 批大小 2，验证分批
	ctx := context.Background()
	v := e.vendorID()
	vp := fmt.Sprintf("/api/vendors/%d", v)
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM vendors WHERE name LIKE 't12-v-%'`) })
	c.want(400, "POST", "/api/vendors", map[string]any{"name": ""})
	c.want(400, "POST", "/api/vendors", map[string]any{"nme": "x"})
	nv := c.want(201, "POST", "/api/vendors", map[string]any{"name": "t12-v-other", "company_name": "某公司"})
	c.want(409, "POST", "/api/vendors", map[string]any{"name": "t12-v-other"})
	other := int64(nv["id"].(float64))
	up := c.want(200, "PATCH", vp, map[string]any{"active_enabled": true, "company_name": "示例"})
	if up["active_enabled"] != true || up["company_name"] != "示例" {
		t.Fatalf("patch %v", up)
	}
	c.want(404, "PATCH", "/api/vendors/999999999", map[string]any{"active_enabled": true})
	c.want(400, "PATCH", "/api/vendors/abc", map[string]any{})
	if n := len(items(c.want(200, "GET", "/api/vendors", nil))); n < 2 {
		t.Fatalf("vendors %d", n)
	}

	// 根域：格式校验、被占 409、外键 404
	for _, bad := range []string{"", "nodot", "-a.test", "a..test", "*.a.test", "a b.test"} {
		c.want(400, "POST", vp+"/roots", map[string]any{"domain": bad})
	}
	r := c.want(201, "POST", vp+"/roots", map[string]any{"domain": " API-New.Example.TEST. "})
	if r["domain"] != "api-new.example.test" {
		t.Fatalf("root %v", r)
	}
	taken := c.want(409, "POST", fmt.Sprintf("/api/vendors/%d/roots", other), map[string]any{"domain": "api-new.example.test"})
	c.want(404, "POST", "/api/vendors/999999999/roots", map[string]any{"domain": "zz-none.test"})
	if n := len(items(c.want(200, "GET", vp+"/roots", nil))); n != 2 {
		t.Fatalf("roots %d", n)
	}

	// 范围规则：格式校验；202 + task id，异步分批重算 in_scope
	hosts := []string{"a", "b", "c", "x.d", "e"}
	for _, h := range hosts {
		e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, in_scope) SELECT $1, $2 || '.' || domain, false FROM root_domains WHERE id = $1`, e.rootID, h)
	}
	root := ""
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	for _, bad := range []map[string]any{{"kind": "inc", "pattern": "*." + root}, {"kind": "include", "pattern": "**." + root},
		{"kind": "include", "pattern": "a.*." + root}, {"kind": "include", "pattern": "http://" + root}, {"kind": "include", "pattern": ""}} {
		c.want(400, "POST", vp+"/scopes", bad)
	}
	wait := func(task map[string]any) map[string]any {
		t.Helper()
		id := int64(task["task_id"].(float64))
		for i := 0; i < 100; i++ {
			st := c.want(200, "GET", fmt.Sprintf("/api/scope-tasks/%d", id), nil)
			if st["status"] == "done" {
				return st
			}
			if st["status"] == "failed" {
				t.Fatalf("scope task %v", st)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("scope task timeout")
		return nil
	}
	inScope := func() string {
		var s string
		e.pool.QueryRow(ctx, `SELECT string_agg(split_part(subdomain, '.', 1) || '=' || in_scope::text, ',' ORDER BY subdomain)
			FROM assets WHERE root_id = $1`, e.rootID).Scan(&s)
		return s
	}
	t1 := c.want(202, "POST", vp+"/scopes", map[string]any{"kind": "include", "pattern": "*." + strings.ToUpper(root)})
	st1 := wait(t1)
	c.want(409, "POST", vp+"/scopes", map[string]any{"kind": "include", "pattern": "*." + root})
	t2 := c.want(202, "POST", vp+"/scopes", map[string]any{"kind": "exclude", "pattern": "*.d." + root})
	st2 := wait(t2)
	s2 := inScope()
	if s2 != "a=true,b=true,c=true,e=true,x=false" || st1["processed"].(float64) != 5 || st1["changed"].(float64) != 5 || st2["changed"].(float64) != 1 {
		t.Fatalf("after rules: %s %v %v", s2, st1, st2)
	}
	rules := items(c.want(200, "GET", vp+"/scopes", nil))
	c.want(404, "DELETE", fmt.Sprintf("/api/vendors/%d/scopes/%v", other, rules[1]["id"]), nil)
	st3 := wait(c.want(202, "DELETE", fmt.Sprintf("%s/scopes/%v", vp, rules[1]["id"]), nil))
	s3 := inScope()
	if s3 != "a=true,b=true,c=true,e=true,x=true" {
		t.Fatalf("after delete: %s", s3)
	}
	c.want(404, "GET", "/api/scope-tasks/999999999", nil)
	t.Logf("厂商 400/409/404 正确；根域被占 → %v；规则 202 task=%v 批大小 2：%v；排除后 %s；删除后 %s（%v）", taken["error"], t1["task_id"], st1, s2, s3, st3["changed"])
}

func TestAPIAssetsEventsScans(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	v := e.vendorID()
	root := ""
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	add := func(name string, wild bool, title string) (aid int64) {
		e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, wildcard, in_scope) VALUES ($1, $2, $3, $4) RETURNING id`,
			e.rootID, name+"."+root, wild, name != "out").Scan(&aid)
		if !wild {
			if err := emitNew(ctx, e.pool, aid, nil, map[string]any{}); err != nil {
				t.Fatal(err)
			}
		}
		if title != "" {
			var uid int64
			e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title) VALUES ($1, 'https', $2, 443, 200, $3) RETURNING id`,
				aid, name+"."+root, title).Scan(&uid)
			e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, url_id) VALUES ('new', $1, $2)`, aid, uid)
		}
		return
	}
	add("www", false, "Welcome Portal")
	api1 := add("api", false, "")
	add("out", false, "")
	add("wc-fake", true, "")
	e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, detail) VALUES ('changed', $1, '{"field":"ips"}')`, api1)
	// 一条旧 new 事件，用于 new_since
	old := add("old", false, "")
	e.pool.Exec(ctx, `UPDATE events SET created_at = now() - interval '10 days' WHERE asset_id = $1`, old)

	base := fmt.Sprintf("/api/assets?vendor=%d", v)
	names := func(path string) []string {
		var out []string
		for _, s := range field(items(c.want(200, "GET", path, nil)), "subdomain") {
			out = append(out, strings.TrimSuffix(s, "."+root))
		}
		return out
	}
	check := func(path string, want ...string) {
		t.Helper()
		if got := names(path); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v want %v", path, got, want)
		}
		t.Logf("%s → %v", strings.Replace(path, fmt.Sprint(v), "V", 1), want)
	}
	check(base, "old", "out", "api", "www")
	check(base+"&include_wildcard=true", "old", "wc-fake", "out", "api", "www")
	check(base+"&q=API.", "api")
	check(base+"&q=portal", "www")
	check(base+"&in_scope=false", "out")
	check(base+"&new_since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "out", "api", "www")
	p1 := c.want(200, "GET", base+"&limit=2", nil)
	if p1["next_cursor"] == nil {
		t.Fatal("no next_cursor")
	}
	check(base+"&limit=2&cursor="+p1["next_cursor"].(string), "api", "www")
	last := c.want(200, "GET", base+"&limit=10", nil)
	if last["next_cursor"] != nil {
		t.Fatal("next_cursor on last page")
	}
	for _, bad := range []string{"&limit=0", "&limit=501", "&cursor=x", "&new_since=yesterday", "&in_scope=maybe", "&include_wildcard=2"} {
		c.want(400, "GET", base+bad, nil)
	}
	det := c.want(200, "GET", fmt.Sprintf("/api/assets/%d", api1-1), nil) // www
	if u := det["urls"].([]any); len(u) != 1 || u[0].(map[string]any)["title"] != "Welcome Portal" {
		t.Fatalf("asset detail %v", det)
	}
	c.want(404, "GET", "/api/assets/999999999", nil)
	c.want(400, "GET", "/api/assets?vendor=x", nil)

	// 事件
	ev := fmt.Sprintf("/api/events?vendor=%d", v)
	evs := func(path string) string {
		var out []string
		for _, m := range items(c.want(200, "GET", path, nil)) {
			out = append(out, fmt.Sprintf("%s/%s/%s", m["level"], m["type"], strings.TrimSuffix(m["subdomain"].(string), "."+root)))
		}
		return strings.Join(out, " ")
	}
	cases := map[string]string{
		ev:                                 "asset/new/old asset/new/out asset/new/api asset/new/www", // 默认 new,recovered
		ev + "&type=new,changed,recovered": "asset/new/old asset/changed/api asset/new/out asset/new/api asset/new/www",
		ev + "&type=changed":               "asset/changed/api",
		ev + "&level=url":                  "url/new/www",
		ev + "&level=all&type=new,changed": "asset/new/old asset/changed/api asset/new/out asset/new/api url/new/www asset/new/www",
		ev + "&since=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + "&type=new": "asset/new/out asset/new/api asset/new/www",
		fmt.Sprintf("%s&level=url&asset_id=%d", ev, api1-1):                                  "url/new/www",
	}
	for path, want := range cases {
		if got := evs(path); got != want {
			t.Fatalf("%s: %q want %q", path, got, want)
		}
		t.Logf("%s → %s", strings.Replace(path, fmt.Sprint(v), "V", 1), want)
	}
	for _, bad := range []string{"&type=foo", "&level=x", "&since=1"} {
		c.want(400, "GET", ev+bad, nil)
	}

	// 扫描：active 需开关
	vp := fmt.Sprintf("/api/vendors/%d", v)
	c.want(400, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "nope"})
	c.want(404, "POST", "/api/scans", map[string]any{"root_id": 999999999, "kind": "passive"})
	c.want(400, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "dns"})
	c.want(400, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "http"})
	c.want(409, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "active"})
	c.want(200, "PATCH", vp, map[string]any{"active_enabled": true})
	s := c.want(202, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "active"})
	x := c.want(202, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "expand"})
	g := c.want(200, "GET", fmt.Sprintf("/api/scans/%v", s["id"]), nil)
	if g["kind"] != "active" || g["status"] != "pending" || x["kind"] != "expand" {
		t.Fatalf("scan %v %v", g, x)
	}
	c.want(404, "GET", "/api/scans/999999999", nil)
	t.Logf("扫描：未开 active 409；开启后 active/expand 202，GET 状态 %v", g["status"])
}

func TestAPICandidatesSubsidiaries(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	v := e.vendorID()
	var v2 int64
	e.pool.QueryRow(ctx, `INSERT INTO vendors (name) VALUES ('t12-cand-other') RETURNING id`).Scan(&v2)
	t.Cleanup(func() {
		c := context.Background()
		e.pool.Exec(c, `DELETE FROM root_domains WHERE domain LIKE 't12cand-%'`)
		e.pool.Exec(c, `DELETE FROM root_candidates WHERE domain LIKE 't12cand-%'`)
		e.pool.Exec(c, `DELETE FROM vendors WHERE id = $1`, v2)
	})
	storeCandidates(ctx, e.pool, v, map[string][]candidate{
		"t12cand-a.test": {{"icp:company", map[string]any{}}}, "t12cand-b.test": {{"icp:company", map[string]any{}}},
		"t12cand-c.test": {{"icp:company", map[string]any{}}}})
	storeCandidates(ctx, e.pool, v2, map[string][]candidate{"t12cand-c.test": {{"icp:reverse", map[string]any{}}}})
	vp := fmt.Sprintf("/api/vendors/%d", v)
	list := items(c.want(200, "GET", vp+"/root-candidates", nil))
	ids := map[string]any{}
	for _, m := range list {
		ids[m["domain"].(string)] = m["id"]
	}
	if len(ids) != 3 {
		t.Fatalf("candidates %v", list)
	}
	cp := func(d, op string) string { return fmt.Sprintf("/api/root-candidates/%v/%s", ids[d], op) }
	conf := c.want(200, "POST", cp("t12cand-a.test", "confirm"), nil)
	c.want(409, "POST", cp("t12cand-a.test", "ignore"), nil)
	c.want(200, "POST", cp("t12cand-b.test", "ignore"), nil)
	c.want(409, "POST", cp("t12cand-b.test", "confirm"), nil)
	c.want(400, "POST", cp("t12cand-c.test", "reassign"), map[string]any{})
	c.want(404, "POST", cp("t12cand-c.test", "reassign"), map[string]any{"vendor_id": 999999999})
	re := c.want(200, "POST", cp("t12cand-c.test", "reassign"), map[string]any{"vendor_id": v2})
	c.want(404, "POST", "/api/root-candidates/999999999/confirm", nil)
	c.want(400, "GET", vp+"/root-candidates?status=x", nil)
	all := field(items(c.want(200, "GET", vp+"/root-candidates?status=all", nil)), "status")
	var rootOwner int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE domain = 't12cand-a.test'`).Scan(&rootOwner)
	if conf["status"] != "confirmed" || rootOwner != v || int64(re["vendor_id"].(float64)) != v2 || len(all) != 2 {
		t.Fatalf("conf %v owner %d re %v all %v", conf, rootOwner, re, all)
	}
	// 子公司导入
	imp := c.want(200, "POST", vp+"/subsidiaries", "子公司甲,60\n子公司乙,30\n")
	c.want(400, "POST", vp+"/subsidiaries", "坏行\n")
	c.want(404, "POST", "/api/vendors/999999999/subsidiaries", "x,1\n")
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM vendor_subsidiaries WHERE vendor_id = $1`, v).Scan(&n)
	if imp["imported"].(float64) != 2 || n != 2 {
		t.Fatalf("import %v n=%d", imp, n)
	}
	t.Logf("确认→root_domains 归属 %d；忽略后再确认 409；改归属→%v（证据 %v）；子公司导入 %v", rootOwner, re["vendor_id"], re["evidence"], imp)
}

// T12 小改 1：模拟重启——遗留 running/pending 任务在新进程启动时重跑，结果幂等。
func TestAPIResumeScopeTasks(t *testing.T) {
	e, c, a := newAPITest(t, 2)
	ctx := context.Background()
	v := e.vendorID()
	root := ""
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	for _, h := range []string{"a", "b", "c"} {
		e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, in_scope) VALUES ($1, $2, false)`, e.rootID, h+"."+root)
	}
	e.pool.Exec(ctx, `INSERT INTO scope_rules (vendor_id, kind, pattern) VALUES ($1, 'include', $2)`, v, "*."+root)
	// 上一个进程：一个跑到一半（running，processed=2）、一个还没开始（pending）就退出了
	var t1, t2 int64
	e.pool.QueryRow(ctx, `INSERT INTO scope_tasks (vendor_id, status, processed, changed) VALUES ($1, 'running', 2, 2) RETURNING id`, v).Scan(&t1)
	e.pool.QueryRow(ctx, `INSERT INTO scope_tasks (vendor_id) VALUES ($1) RETURNING id`, v).Scan(&t2)
	e.pool.Exec(ctx, `UPDATE assets SET in_scope = true WHERE subdomain = $1`, "a."+root) // 中断前已改了一行
	n, err := a.resumeScopeTasks(ctx)
	if err != nil || n < 2 {
		t.Fatalf("resumed %d %v", n, err)
	}
	var sts []string
	for _, id := range []int64{t1, t2} {
		for i := 0; ; i++ {
			st := c.want(200, "GET", fmt.Sprintf("/api/scope-tasks/%d", id), nil)
			if st["status"] == "done" {
				sts = append(sts, fmt.Sprintf("task%d=done processed=%v", id, st["processed"]))
				if st["processed"].(float64) != 3 {
					t.Fatalf("task %v", st)
				}
				break
			}
			if i > 100 {
				t.Fatalf("task %d not done: %v", id, st)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	var in int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE root_id = $1 AND in_scope`, e.rootID).Scan(&in)
	if in != 3 {
		t.Fatalf("in_scope %d/3", in)
	}
	if n2, _ := a.resumeScopeTasks(ctx); n2 != 0 {
		t.Fatalf("resumed again %d", n2)
	}
	t.Logf("模拟重启：重跑 %d 个遗留任务 %v，in_scope 3/3，再次启动无任务可重跑", n, sts)
}

// T12 小改 2/3：确认被占域名 409 且保持 pending；资产级 new 事件嵌套 url_events。
func TestAPIConfirmTakenAndNestedURLEvents(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	v := e.vendorID()
	var v2 int64
	e.pool.QueryRow(ctx, `INSERT INTO vendors (name) VALUES ('t12-taken-other') RETURNING id`).Scan(&v2)
	t.Cleanup(func() {
		c := context.Background()
		e.pool.Exec(c, `DELETE FROM root_candidates WHERE domain = 't12taken.test'`)
		e.pool.Exec(c, `DELETE FROM vendors WHERE id = $1`, v2)
	})
	storeCandidates(ctx, e.pool, v, map[string][]candidate{"t12taken.test": {{"icp:company", map[string]any{}}}})
	e.pool.Exec(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 't12taken.test')`, v2)
	var cid int64
	e.pool.QueryRow(ctx, `SELECT id FROM root_candidates WHERE domain = 't12taken.test'`).Scan(&cid)
	res := c.want(409, "POST", fmt.Sprintf("/api/root-candidates/%d/confirm", cid), nil)
	var st string
	e.pool.QueryRow(ctx, `SELECT status FROM root_candidates WHERE id = $1`, cid).Scan(&st)
	if st != "pending" {
		t.Fatalf("status %s", st)
	}

	root := ""
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	var aid, aid2, u1, u2 int64
	e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, e.rootID, "www."+root).Scan(&aid)
	e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, e.rootID, "bare."+root).Scan(&aid2)
	emitNew(ctx, e.pool, aid, nil, map[string]any{})
	emitNew(ctx, e.pool, aid2, nil, map[string]any{})
	for _, s := range []string{"http", "https"} {
		var u int64
		e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title) VALUES ($1, $2, $3, 443, 200, 'Home') RETURNING id`,
			aid, s, "www."+root).Scan(&u)
		emitNew(ctx, e.pool, aid, &u, map[string]any{})
		if u1 == 0 {
			u1 = u
		} else {
			u2 = u
		}
	}
	list := items(c.want(200, "GET", fmt.Sprintf("/api/events?vendor=%d", v), nil))
	got := map[string]int{}
	for _, m := range list {
		got[strings.SplitN(m["subdomain"].(string), ".", 2)[0]] = len(m["url_events"].([]any))
	}
	urlOnly := items(c.want(200, "GET", fmt.Sprintf("/api/events?vendor=%d&level=url", v), nil))
	if !reflect.DeepEqual(got, map[string]int{"www": 2, "bare": 0}) || len(urlOnly) != 2 {
		t.Fatalf("nested %v urlOnly %d", got, len(urlOnly))
	}
	ue := list[len(list)-1]["url_events"].([]any)[0].(map[string]any)
	t.Logf("被占域名确认 → 409 %q，候选仍 %s；资产级 new 嵌套 url_events 数 %v（url_id %d,%d），level=url 单独查 %d 条；样例 %v",
		res["error"], st, got, u1, u2, len(urlOnly), ue)
}

// T12 打回：NUL 字符一律 400（不 500）；JSON 后跟多余内容 400。
func TestAPIRejectNULAndTrailingJSON(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM vendors WHERE name IN ('ok-trailing', 'ok2-trailing')`)
	})
	raw := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testBootstrap)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	v := e.vendorID()
	cases := []struct{ m, p, b string }{
		{"GET", "/api/assets?q=%00", ""},
		{"GET", "/api/events?type=new%00", ""},
		{"POST", "/api/vendors", `{"name":"a\u0000b"}`},
		{"POST", "/api/vendors", "{\"name\":\"a\x00b\"}"},
		{"PATCH", fmt.Sprintf("/api/vendors/%d", v), `{"company_name":"x\u0000"}`},
		{"POST", "/api/tokens", `{"name":"t\u0000"}`},
		{"POST", fmt.Sprintf("/api/vendors/%d/roots", v), `{"domain":"a\u0000.test"}`},
		{"POST", fmt.Sprintf("/api/vendors/%d/subsidiaries", v), "a\x00,60\n"},
		{"POST", "/api/vendors", `{"name":"ok-trailing"} trailing`},
		{"POST", "/api/vendors", `{"name":"ok-trailing"}{"name":"ok2-trailing"}`},
		{"POST", "/api/tokens", `{"name":"x"} 1`},
		{"POST", "/api/scans", `{"root_id":1,"kind":"dns"} x`},
	}
	for _, cs := range cases {
		code, body := raw(cs.m, cs.p, cs.b)
		var m map[string]any
		if code != 400 || json.Unmarshal([]byte(body), &m) != nil || m["error"] == nil || len(m) != 1 {
			t.Fatalf("%s %s %q → %d %s", cs.m, cs.p, cs.b, code, body)
		}
		t.Logf("%s %s %q → %d %s", cs.m, cs.p, cs.b, code, body)
	}
	if code, body := raw("POST", "/api/vendors", "{\"name\":\"ok-trailing\"}\n  "); code != 201 {
		t.Fatalf("trailing whitespace rejected: %d %s", code, body)
	}
}

func TestDBErr22021(t *testing.T) {
	rec := httptest.NewRecorder()
	dbErr(rec, fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "22021"}))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	t.Logf("SQLSTATE 22021 → %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))
}
