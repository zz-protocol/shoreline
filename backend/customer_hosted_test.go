package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func builtinPatterns() []string {
	var ps []string
	for _, r := range builtinHostedRules {
		ps = append(ps, r.Pattern)
	}
	return ps
}

func TestBuiltinHostedSamples(t *testing.T) {
	ps := builtinPatterns()
	hit := []string{"examplebucket-1250000000.cos.ap-guangzhou.myqcloud.com", "b.oss-cn-hangzhou.aliyuncs.com", "x.cn-hangzhou.fcapp.run",
		"d111.cloudfront.net", "my-lb-123.elb.us-east-2.amazonaws.com", "app.azurewebsites.net", "www.example.com.cdn.cloudflare.net",
		"123.cn-hangzhou.fc.aliyuncs.com", "examplebucket-1250000000.cos-website.ap-guangzhou.myqcloud.com",
		"my-lb-1.us-east-1.elb.amazonaws.com", "ec2-1-2-3-4.compute-1.amazonaws.com", "ec2-1-2-3-4.us-west-2.compute.amazonaws.com",
		"bkt.storage.googleapis.com", "bkt.s3.amazonaws.com", "bkt.s3.us-west-2.amazonaws.com", "bkt.s3-website-us-east-1.amazonaws.com",
		"a.global.fastly.net", "u.github.io"}
	miss := []string{"oss-cn-hangzhou.aliyuncs.com", "ecs.aliyuncs.com", "cos.ap-guangzhou.myqcloud.com", "ec2.us-east-1.amazonaws.com",
		"storage.googleapis.com", "appspot.com", "www.aliyun.com", "cn-hangzhou.fc.aliyuncs.com", "global.fastly.net",
		"compute.amazonaws.com", "us-east-1.compute.amazonaws.com", "elb.amazonaws.com", "us-east-1.elb.amazonaws.com",
		"s3.amazonaws.com", "s3.us-west-2.amazonaws.com", "x.fastly.net", "a.b.volccdn.com", "oss.aliyuncs.com"}
	for _, h := range hit {
		if !isCustomerHosted(h, ps) {
			t.Errorf("want hit: %s", h)
		}
	}
	for _, h := range miss {
		if isCustomerHosted(h, ps) {
			t.Errorf("want miss: %s", h)
		}
	}
}

// 每条内置规则：去掉第一个 * 标签得到服务端点样例（剩余 * 换成 x），整套内置规则都不应命中。
func TestBuiltinRulesSkipEndpoints(t *testing.T) {
	ps := builtinPatterns()
	seen := map[string]bool{}
	for _, p := range ps {
		if seen[p] {
			t.Errorf("builtin dup: %s", p)
		}
		seen[p] = true
		ep := strings.ReplaceAll(strings.TrimPrefix(p, "*."), "*", "x")
		for _, q := range ps {
			if matchHosted(q, ep) {
				t.Errorf("endpoint %s (from %s) hit by %s", ep, p, q)
			}
		}
		// 多一层客户子域则命中
		if !matchHosted(p, "c."+ep) {
			t.Errorf("%s should hit c.%s", p, ep)
		}
	}
	t.Logf("内置规则 %d 条，端点样例均不命中", len(ps))
}

func TestHostedPatternSyntax(t *testing.T) {
	for _, c := range []struct {
		p, h string
		ok   bool
	}{
		{"*.a.com", "x.a.com", true}, {"*.a.com", "x.y.a.com", true}, {"*.a.com", "a.com", false},
		{"*.oss-*.a.com", "b.oss-cn.a.com", true}, {"*.oss-*.a.com", "b.oss-cn.x.a.com", false}, // 标签内 * 不跨点
		{"*.oss-*.a.com", "b.ossx.a.com", false}, {"*.*.b.com", "x.b.com", false}, {"*.*.b.com", "y.x.b.com", true},
	} {
		if matchHosted(c.p, c.h) != c.ok {
			t.Errorf("match(%s,%s) want %v", c.p, c.h, c.ok)
		}
	}
	for _, bad := range []string{"a.com", "*.com", "*.a.*", "*.a..com", "*.a/b.com", "*.A_B.com", "**.a.com", "",
		"*.*.com", "*.*-*.com", "*.*.*", "*.x-*.com", "*.*.*.com", "*.oss-*.*.com", "*.co.uk", "*.*.co.uk", "*.com.cn", "*.x-*.com.cn"} {
		if validHostedPattern(bad) {
			t.Errorf("should be invalid: %q", bad)
		}
	}
}

// 删除一条内置规则后重新跑迁移 + 补种，不会复活。
func TestHostedBuiltinTombstone(t *testing.T) {
	e, c, a := newAPITest(t, 100)
	ctx := context.Background()
	if err := a.initHosted(ctx); err != nil {
		t.Fatal(err)
	}
	pat := "*.r2.dev"
	var id int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM customer_hosted_rules WHERE pattern = $1 AND builtin`, pat).Scan(&id); err != nil {
		t.Fatal(err)
	}
	e.pool.Exec(ctx, `UPDATE customer_hosted_rules SET deleted_at = NULL, enabled = true WHERE id = $1`, id)
	t.Cleanup(func() {
		e.pool.Exec(ctx, `UPDATE customer_hosted_rules SET deleted_at = NULL, enabled = true WHERE id = $1`, id)
	})
	c.want(202, "DELETE", fmt.Sprintf("/api/customer-hosted-rules/%d", id), nil)
	c.want(404, "DELETE", fmt.Sprintf("/api/customer-hosted-rules/%d", id), nil)
	conn, _ := e.pool.Acquire(ctx)
	err := migrate(ctx, conn.Conn())
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}
	n, err := seedHostedRules(ctx, e.pool)
	if err != nil || n != 0 {
		t.Fatalf("seed n=%d err=%v", n, err)
	}
	for _, it := range items(c.want(200, "GET", "/api/customer-hosted-rules", nil)) {
		if it["pattern"] == pat {
			t.Fatal("deleted builtin revived")
		}
	}
	var cnt int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM customer_hosted_rules WHERE builtin AND deleted_at IS NULL`).Scan(&cnt)
	if cnt != len(builtinHostedRules)-1 {
		t.Fatalf("builtin live %d want %d", cnt, len(builtinHostedRules)-1)
	}
}

func waitHosted(t *testing.T, c *apiClient, resp map[string]any) {
	t.Helper()
	id := int64(resp["task_id"].(float64))
	for i := 0; i < 200; i++ {
		st := c.want(200, "GET", fmt.Sprintf("/api/customer-hosted-tasks/%d", id), nil)
		if st["status"] == "done" {
			return
		}
		if st["status"] == "failed" {
			t.Fatalf("task %v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("hosted task timeout")
}

// 加规则 → 异步重算 → 资产/播报默认隐藏、include_customer_hosted=true 可见；停用恢复；新入库资产直接按规则标记。
func TestHostedRulesRecalcAndHide(t *testing.T) {
	e, c, _ := newAPITest(t, 2)
	ctx := context.Background()
	vid := e.vendorID()
	var root string
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	hostedHost, plain := "bkt.oss-x."+root, "www."+root
	for _, h := range []string{hostedHost, plain, "a." + root, "b." + root} {
		var aid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, e.rootID, h).Scan(&aid); err != nil {
			t.Fatal(err)
		}
		e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, detail) VALUES ('new', $1, '{}')`, aid)
	}
	pat := "*.oss-*." + root
	c.want(400, "POST", "/api/customer-hosted-rules", map[string]any{"pattern": "oss." + root})
	c.want(400, "POST", "/api/customer-hosted-rules", map[string]any{"pattern": "*.a.*"})
	c.want(400, "POST", "/api/customer-hosted-rules", map[string]any{"pattern": "*.*.com"})
	c.want(400, "POST", "/api/customer-hosted-rules", map[string]any{"pattern": "*.*-*.com"})
	res := c.want(202, "POST", "/api/customer-hosted-rules", map[string]any{"pattern": strings.ToUpper(pat), "note": "测试"})
	rid := int64(res["rule_id"].(float64))
	t.Cleanup(func() { e.pool.Exec(ctx, `DELETE FROM customer_hosted_rules WHERE id = $1`, rid) })
	c.want(409, "POST", "/api/customer-hosted-rules", map[string]any{"pattern": pat})
	waitHosted(t, c, res)

	subs := func(path string) map[string]bool {
		m := map[string]bool{}
		for _, it := range items(c.want(200, "GET", path, nil)) {
			m[it["subdomain"].(string)] = true
		}
		return m
	}
	ap := fmt.Sprintf("/api/assets?vendor=%d&limit=500", vid)
	ep := fmt.Sprintf("/api/events?vendor=%d&limit=500", vid)
	if m := subs(ap); m[hostedHost] || !m[plain] {
		t.Fatalf("default assets %v", m)
	}
	if m := subs(ap + "&include_customer_hosted=true"); !m[hostedHost] {
		t.Fatalf("include assets %v", m)
	}
	if m := subs(ep); m[hostedHost] || !m[plain] {
		t.Fatalf("default events %v", m)
	}
	if m := subs(ep + "&include_customer_hosted=true"); !m[hostedHost] {
		t.Fatalf("include events %v", m)
	}
	// 事件条目带 customer_hosted（REST 与 MCP list_new_assets）
	flags := func(list []map[string]any) map[string]any {
		m := map[string]any{}
		for _, it := range list {
			m[it["subdomain"].(string)] = it["customer_hosted"]
		}
		return m
	}
	if f := flags(items(c.want(200, "GET", ep+"&include_customer_hosted=true", nil))); f[hostedHost] != true || f[plain] != false {
		t.Fatalf("REST event customer_hosted %v", f)
	}
	sess, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, isErr := callTool(t, sess, "list_new_assets", map[string]any{"vendor": vid, "include_customer_hosted": true})
	var mi []map[string]any
	for _, x := range out["items"].([]any) {
		mi = append(mi, x.(map[string]any))
	}
	if f := flags(mi); isErr || f[hostedHost] != true || f[plain] != false {
		t.Fatalf("MCP event customer_hosted %v %v", f, isErr)
	}
	out, _ = callTool(t, sess, "list_new_assets", map[string]any{"vendor": vid})
	for _, x := range out["items"].([]any) {
		if x.(map[string]any)["subdomain"] == hostedHost {
			t.Fatal("MCP default should hide customer_hosted")
		}
	}
	st := c.want(200, "GET", fmt.Sprintf("/api/events/stats?vendor=%d", vid), nil)
	if st["total"].(float64) != 3 {
		t.Fatalf("stats total %v want 3", st["total"])
	}
	c.want(400, "GET", ap+"&include_customer_hosted=x", nil)

	// 新入库的资产直接按规则标记
	newHost := "n.oss-y." + root
	if _, err := storeAssets(ctx, e.pool, map[string][]string{newHost: {"test"}}); err != nil {
		t.Fatal(err)
	}
	var ch bool
	e.pool.QueryRow(ctx, `SELECT customer_hosted FROM assets WHERE subdomain = $1`, newHost).Scan(&ch)
	if !ch {
		t.Fatal("storeAssets did not mark customer_hosted")
	}

	// 停用 → 重算后可见；再启用 → 隐藏；删除 → 可见
	c.want(400, "PATCH", fmt.Sprintf("/api/customer-hosted-rules/%d", rid), map[string]any{})
	waitHosted(t, c, c.want(202, "PATCH", fmt.Sprintf("/api/customer-hosted-rules/%d", rid), map[string]any{"enabled": false}))
	if m := subs(ap); !m[hostedHost] || !m[newHost] {
		t.Fatalf("after disable %v", m)
	}
	waitHosted(t, c, c.want(202, "PATCH", fmt.Sprintf("/api/customer-hosted-rules/%d", rid), map[string]any{"enabled": true}))
	if m := subs(ap); m[hostedHost] {
		t.Fatalf("after enable %v", m)
	}
	waitHosted(t, c, c.want(202, "DELETE", fmt.Sprintf("/api/customer-hosted-rules/%d", rid), nil))
	if m := subs(ap); !m[hostedHost] {
		t.Fatalf("after delete %v", m)
	}
	// 删除后同名重新添加：恢复该条
	res2 := c.want(202, "POST", "/api/customer-hosted-rules", map[string]any{"pattern": pat})
	if int64(res2["rule_id"].(float64)) != rid {
		t.Fatalf("re-add id %v want %d", res2["rule_id"], rid)
	}
	waitHosted(t, c, res2)
}

func TestHostedPatternPSL(t *testing.T) {
	for p, ok := range map[string]bool{
		"*.example.com": true, "*.oss-*.aliyuncs.com": true, "*.a.example.co.uk": true, "*.example.co.uk": true,
		"*.*.com": false, "*.*-*.com": false, "*.com": false, "*.*.*": false, "*.co.uk": false, "*.x-*.co.uk": false,
	} {
		if got := validHostedPattern(p); got != ok {
			t.Errorf("valid(%s)=%v want %v (%s)", p, got, ok, hostedPatternErr(p))
		}
	}
}

// pslPrivatePending：固定后缀落在 PSL 私有段（如 github.io、appspot.com）的内置规则，publicsuffix 校验不通过。
// 处理方案待架构定（只用 ICANN 段 / 内置免检）；这里钉住清单，其余内置必须通过校验。
var pslPrivatePending = map[string]bool{
	"*.cloudfront.net": true, "*.s3.amazonaws.com": true, "*.elasticbeanstalk.com": true, "*.amplifyapp.com": true,
	"*.azurewebsites.net": true, "*.blob.core.windows.net": true, "*.web.core.windows.net": true, "*.file.core.windows.net": true,
	"*.azureedge.net": true, "*.azurefd.net": true, "*.azurestaticapps.net": true, "*.trafficmanager.net": true, "*.azure-api.net": true,
	"*.appspot.com": true, "*.cloudfunctions.net": true, "*.web.app": true, "*.firebaseapp.com": true,
	"*.cdn.cloudflare.net": true, "*.workers.dev": true, "*.pages.dev": true, "*.r2.dev": true,
	"*.edgekey.net": true, "*.edgesuite.net": true, "*.akamaized.net": true, "*.github.io": true, "*.herokuapp.com": true,
}

func TestBuiltinHostedPassValidation(t *testing.T) {
	var fail []string
	for _, r := range builtinHostedRules {
		if !validHostedPattern(r.Pattern) {
			fail = append(fail, r.Pattern)
			if !pslPrivatePending[r.Pattern] {
				t.Errorf("builtin %s fails validation: %s", r.Pattern, hostedPatternErr(r.Pattern))
			}
		} else if pslPrivatePending[r.Pattern] {
			t.Errorf("builtin %s now passes; remove from pending", r.Pattern)
		}
	}
	t.Logf("内置 %d 条，%d 条通过；私有后缀待定 %d 条：%v", len(builtinHostedRules), len(builtinHostedRules)-len(fail), len(fail), fail)
}
