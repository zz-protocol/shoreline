package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerRT struct{ tok string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.tok != "" {
		r.Header.Set("Authorization", "Bearer "+b.tok)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func mcpSession(t *testing.T, url, tok string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "t13-test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{tok}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
}

// callTool 返回 结构化结果（JSON）与 isError。
func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out := map[string]any{}
	if res.IsError {
		json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out)
		return out, true
	}
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	return out, false
}

func TestMCPTools(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	v := e.vendorID()
	root := ""
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	e.pool.Exec(ctx, `INSERT INTO scope_rules (vendor_id, kind, pattern) VALUES ($1, 'include', $2), ($1, 'exclude', $3)`, v, "*."+root, "admin."+root)
	var www, wc, uid int64
	e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, in_scope) VALUES ($1, $2, true) RETURNING id`, e.rootID, "www."+root).Scan(&www)
	e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, in_scope, wildcard) VALUES ($1, $2, true, true) RETURNING id`, e.rootID, "wc."+root).Scan(&wc)
	e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, in_scope) VALUES ($1, $2, false)`, e.rootID, "admin."+root)
	e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title) VALUES ($1, 'https', $2, 443, 200, 'Login Portal') RETURNING id`, www, "www."+root).Scan(&uid)
	emitNew(ctx, e.pool, www, nil, map[string]any{})
	emitNew(ctx, e.pool, www, &uid, map[string]any{})

	// 无 token / 错 token 连接失败
	if _, err := mcpSession(t, c.srv.URL, ""); err == nil {
		t.Fatal("connected without token")
	}
	if _, err := mcpSession(t, c.srv.URL, "wrong"); err == nil {
		t.Fatal("connected with bad token")
	}
	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tools, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "check_scope,list_new_assets,search_assets,trigger_scan" {
		t.Fatalf("tools %v", names)
	}
	t.Logf("tools/list：%v", names)

	// list_new_assets：资产级 new，嵌套 url_events
	out, isErr := callTool(t, s, "list_new_assets", map[string]any{"vendor": v, "since": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	items := out["items"].([]any)
	first := items[0].(map[string]any)
	if isErr || len(items) != 1 || first["subdomain"] != "www."+root || len(first["url_events"].([]any)) != 1 {
		t.Fatalf("list_new_assets %v", out)
	}
	t.Logf("list_new_assets → %d 条，%s url_events=%d", len(items), first["subdomain"], len(first["url_events"].([]any)))

	// search_assets：q 子域名/标题、in_scope、wildcard 默认过滤、分页
	sub := func(args map[string]any) string {
		args["vendor"] = v
		out, isErr := callTool(t, s, "search_assets", args)
		if isErr {
			t.Fatalf("search_assets %v: %v", args, out)
		}
		var l []string
		for _, it := range out["items"].([]any) {
			l = append(l, strings.TrimSuffix(it.(map[string]any)["subdomain"].(string), "."+root))
		}
		return strings.Join(l, ",")
	}
	for args, want := range map[string]string{
		`{}`: "admin,www", `{"include_wildcard":true}`: "admin,wc,www", `{"q":"portal"}`: "www", `{"q":"ADMIN."}`: "admin",
		`{"in_scope":false}`: "admin", `{"limit":1}`: "admin",
	} {
		m := map[string]any{}
		json.Unmarshal([]byte(args), &m)
		if got := sub(m); got != want {
			t.Fatalf("search_assets %s → %s want %s", args, got, want)
		}
		t.Logf("search_assets %s → %s", args, want)
	}
	if out, isErr := callTool(t, s, "search_assets", map[string]any{"limit": 501}); !isErr || out["error"] == nil {
		t.Fatalf("limit 501 accepted: %v", out)
	}

	// check_scope
	for host, want := range map[string]string{
		"WWW." + root + ".": "true", "new.deep." + root: "true", "admin." + root: "false", "other-unknown.test": "false",
	} {
		out, isErr := callTool(t, s, "check_scope", map[string]any{"host": host})
		if isErr || fmt.Sprint(out["in_scope"]) != want {
			t.Fatalf("check_scope %s → %v", host, out)
		}
		t.Logf("check_scope %s → in_scope=%v vendor=%v root=%v rules=%v asset=%v", host, out["in_scope"], out["vendor_id"], out["root_domain"], out["matched_rules"], out["asset_id"])
	}
	if out, isErr := callTool(t, s, "check_scope", map[string]any{"host": "bad host"}); !isErr {
		t.Fatalf("bad host accepted: %v", out)
	}

	// trigger_scan：只允许 passive/active/expand；active 需开关；只入队不执行（测试未启动 worker，不发外网请求）
	for _, k := range []string{"dns", "http", "nope"} {
		if out, isErr := callTool(t, s, "trigger_scan", map[string]any{"root_id": e.rootID, "kind": k}); !isErr {
			t.Fatalf("kind %s accepted: %v", k, out)
		}
	}
	if out, isErr := callTool(t, s, "trigger_scan", map[string]any{"root_id": e.rootID, "kind": "active"}); !isErr || !strings.Contains(fmt.Sprint(out["error"]), "disabled") {
		t.Fatalf("active without switch: %v", out)
	}
	if out, isErr := callTool(t, s, "trigger_scan", map[string]any{"root_id": 999999999, "kind": "passive"}); !isErr || out["error"] != "root not found" {
		t.Fatalf("missing root: %v", out)
	}
	e.pool.Exec(ctx, `UPDATE vendors SET active_enabled = true WHERE id = $1`, v)
	var kinds []string
	for _, k := range []string{"passive", "active", "expand"} {
		out, isErr := callTool(t, s, "trigger_scan", map[string]any{"root_id": e.rootID, "kind": k})
		if isErr || out["kind"] != k || out["status"] != "pending" {
			t.Fatalf("trigger %s: %v", k, out)
		}
		kinds = append(kinds, fmt.Sprintf("%s#%v", k, out["id"]))
	}
	t.Logf("trigger_scan：dns/http/nope 拒绝；active 未开关拒绝；开启后 %v 入队 pending", kinds)
}

// GET /api/check-scope 与 MCP check_scope 输出一致；非法 host 400；需认证。
func TestCheckScopeRESTMatchesMCP(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	v := e.vendorID()
	root := ""
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	e.pool.Exec(ctx, `INSERT INTO scope_rules (vendor_id, kind, pattern) VALUES ($1, 'include', $2), ($1, 'exclude', $3)`, v, "*."+root, "admin."+root)
	e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, in_scope) VALUES ($1, $2, true)`, e.rootID, "www."+root)
	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, host := range []string{"WWW." + root + ".", "new.deep." + root, "admin." + root, root, "nobody-here.test"} {
		rest := c.want(200, "GET", "/api/check-scope?host="+host, nil)
		viaMCP, isErr := callTool(t, s, "check_scope", map[string]any{"host": host})
		if isErr || !reflect.DeepEqual(rest, viaMCP) {
			t.Fatalf("%s: rest %v mcp %v", host, rest, viaMCP)
		}
		t.Logf("%s → in_scope=%v root=%v rules=%d asset=%v（REST=MCP）", host, rest["in_scope"], rest["root_domain"], len(rest["matched_rules"].([]any)), rest["asset_id"])
	}
	for _, bad := range []string{"", "nodot", "a%20b.test", "bad_host.test", "-a.test", "a..test", "%00.test", "*.a.test", "http://a.test"} {
		out := c.want(400, "GET", "/api/check-scope?host="+bad, nil)
		t.Logf("host=%q → 400 %v", bad, out["error"])
	}
	(&apiClient{t: t, srv: c.srv}).want(401, "GET", "/api/check-scope?host="+root, nil)
}

// DNS rebinding 防护：经回环地址到达、Host 非回环 → 403；Host 为回环 → 正常。
func TestMCPRebindingProtection(t *testing.T) {
	_, c, _ := newAPITest(t, 500)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for host, want := range map[string]int{"evil.example.com": 403, "assets.example.com:443": 403, "127.0.0.1:8080": 200, "localhost": 200} {
		req, _ := http.NewRequest("POST", c.srv.URL+"/mcp", strings.NewReader(body))
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+testBootstrap)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("Host %s: %d want %d", host, resp.StatusCode, want)
		}
		t.Logf("经 127.0.0.1 到达，Host=%s → %d", host, resp.StatusCode)
	}
}
