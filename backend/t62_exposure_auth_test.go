package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
)

// t62Asset：T62 测试资产，直接写库；不含 IP。
func t62Asset(t *testing.T, e *testEnv, host string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO assets (root_id, subdomain, sources, alive)
		VALUES ($1, $2, '{http}', 'alive') RETURNING id`, e.rootID, host).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func t62Probe(status int, title, body string) *probeResult {
	return &probeResult{Scheme: "https", Host: "t62.example.test", Port: 443, Status: status, Title: title, body: []byte(body)}
}

// T62：200 登录页 / 401 / JSON / placeholder / WAF / 404 / 静态文件等样本判定正确，普通 200 页面不误判。
func TestT62ClassifyExposureAuth(t *testing.T) {
	loginHTML := `<html><head><title>Sign in</title></head><body>
		<form method="post" action="/login"><input type="text" name="user"><input type="password" name="pwd"></form></body></html>`
	cases := []struct {
		label    string
		r        *probeResult
		wantExp  string
		wantAuth string
	}{
		{"200 登录表单", t62Probe(200, "Sign in", loginHTML), exposureLoginOnly, authLoginRequired},
		{"200 中文登录页", t62Probe(200, "统一认证登录", "<html><body>请登录</body></html>"), exposureLoginOnly, authLoginRequired},
		{"401", t62Probe(401, "", "unauthorized"), exposureUnknown, authLoginRequired},
		{"302 到登录", func() *probeResult {
			r := t62Probe(302, "", "")
			r.Redirect = "/login?redirect=/home"
			return r
		}(), exposureUnknown, authLoginRequired},
		{"302 到首页", func() *probeResult {
			r := t62Probe(302, "", "")
			r.Redirect = "/home"
			return r
		}(), exposureUnknown, authUnknown},
		{"200 JSON API", t62Probe(200, "", `{"items":[],"total":0,"page":1}`), exposureAPI, authAnonymous},
		{"200 JSON 数组", t62Probe(200, "", `[{"id":1,"name":"a"}]`), exposureAPI, authAnonymous},
		{"200 placeholder", t62Probe(200, "Welcome to nginx!", "<html><body><h1>Welcome to nginx!</h1></body></html>"), exposurePlaceholder, authUnknown},
		{"200 JS challenge", t62Probe(200, "Just a moment...", "<html><body>Checking your browser before accessing</body></html>"), exposureWAF, authUnknown},
		{"403 WAF 拦截页", t62Probe(403, "Access denied", "<html><body>Access denied</body></html>"), exposureWAF, authUnknown},
		{"403 普通", t62Probe(403, "Forbidden", "<html><body>Forbidden</body></html>"), exposureUnknown, authUnknown},
		{"404", t62Probe(404, "Not Found", "<html><body>Not Found</body></html>"), exposureErrorPage, authUnknown},
		{"503", t62Probe(503, "", ""), exposureErrorPage, authUnknown},
		{"200 静态二进制", t62Probe(200, "", "%PDF-1.4\n%âãÏÓ\n\x00\x01\x02binary"), exposureStaticOnly, authUnknown},
		{"200 目录列表", t62Probe(200, "Index of /", `<html><head><title>Index of /</title></head><body><h1>Index of /</h1><a href="parent directory">Parent Directory</a></body></html>`), exposureStaticOnly, authUnknown},
		{"200 后台", t62Probe(200, "Admin Console", "<html><body>admin</body></html>"), exposureAdmin, authUnknown},
		{"200 普通页面", t62Probe(200, "Example Domain", "<html><head><title>Example Domain</title></head><body><h1>Hello</h1></body></html>"), exposureLiveApp, authUnknown},
		{"200 空响应", t62Probe(200, "", ""), exposureUnknown, authUnknown},
		{"301 跳转", t62Probe(301, "", ""), exposureUnknown, authUnknown},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			exposure, auth := classifyExposureAuth(c.r)
			if exposure != c.wantExp || auth != c.wantAuth {
				t.Fatalf("%s：exposure=%q auth=%q，期望 %q/%q", c.label, exposure, auth, c.wantExp, c.wantAuth)
			}
		})
	}

	if exposure, auth := classifyExposureAuth(nil); exposure != exposureUnknown || auth != authUnknown {
		t.Fatalf("nil 探测结果应为 unknown/unknown，实得 %q/%q", exposure, auth)
	}
}

// T62：HTTP 探测后把 exposure/auth 写进 assets；后续扫描覆盖；判不出来时不冲掉已有结论。
func TestT62StoreURLWritesExposureAuth(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	host := "state." + strings.ToLower(t.Name()) + ".example.test"
	aid := t62Asset(t, e, host)
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM events WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM url_components WHERE url_id IN (SELECT id FROM urls WHERE asset_id = $1)`, aid)
		e.pool.Exec(ctx, `DELETE FROM urls WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM components WHERE name_key = 't62comp'`)
	})
	p := &prober{}
	login := `<html><head><title>Sign in</title></head><body><form><input type="password" name="pwd"></form></body></html>`
	store := func(status int, title, body string) (string, string) {
		t.Helper()
		r := t62Probe(status, title, body)
		r.Host, r.Scheme, r.Port = host, "https", 443
		if err := p.storeURL(ctx, e.pool, aid, r); err != nil {
			t.Fatal(err)
		}
		var exposure, auth string
		if err := e.pool.QueryRow(ctx, `SELECT exposure_state, auth_state FROM assets WHERE id = $1`, aid).Scan(&exposure, &auth); err != nil {
			t.Fatal(err)
		}
		return exposure, auth
	}

	if exposure, auth := store(200, "Sign in", login); exposure != exposureLoginOnly || auth != authLoginRequired {
		t.Fatalf("登录页写入 exposure=%q auth=%q", exposure, auth)
	}
	if exposure, auth := store(200, "", `{"items":[{"id":1}],"total":1}`); exposure != exposureAPI || auth != authAnonymous {
		t.Fatalf("JSON 写入 exposure=%q auth=%q", exposure, auth)
	}
	// 404 覆盖 exposure，但判不出来的 auth 不把 anonymous 冲掉。
	if exposure, auth := store(404, "Not Found", "<html><body>Not Found</body></html>"); exposure != exposureErrorPage || auth != authAnonymous {
		t.Fatalf("404 写入 exposure=%q auth=%q", exposure, auth)
	}
	// 301 两边都判不出来：保留上一轮结论。
	if exposure, auth := store(301, "", ""); exposure != exposureErrorPage || auth != authAnonymous {
		t.Fatalf("301 不应覆盖 exposure=%q auth=%q", exposure, auth)
	}

	// 组件关系 API 返回同一资产上的 exposure/auth。
	store(200, "", `{"items":[],"total":0}`)
	var urlID int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM urls WHERE asset_id = $1 AND scheme = 'https'`, aid).Scan(&urlID); err != nil {
		t.Fatal(err)
	}
	if err := syncURLComponents(ctx, e.pool, urlID, []string{"T62Comp"}); err != nil {
		t.Fatal(err)
	}
	var cid int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM components WHERE name_key = 't62comp'`).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	list := items(c.want(200, "GET", fmt.Sprintf("/api/components/%d/assets", cid), nil))
	t51NoIP(t, map[string]any{"items": toAny(list)}, "T62 详情")
	if len(list) != 1 {
		t.Fatalf("关系数 %d：%v", len(list), list)
	}
	if fmt.Sprint(list[0]["exposure_state"]) != exposureAPI || fmt.Sprint(list[0]["auth_state"]) != authAnonymous {
		t.Fatalf("API exposure=%v auth=%v，期望 api/anonymous", list[0]["exposure_state"], list[0]["auth_state"])
	}
}

// T62：服务/非 Web 资产不参与 exposure 判定，保持 unknown；资产行不含 IP。
func TestT62ServiceAssetStaysUnknown(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	host := "db." + strings.ToLower(t.Name()) + ".example.test"
	aid := t62Asset(t, e, host)
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, verified, response_kind, name, version, banner)
		VALUES ($1, $2, 6379, true, true, 'banner', 'redis', '7.0', '+PONG')`, aid, host); err != nil {
		t.Fatal(err)
	}
	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}, t53MustLoad(t, "")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM asset_components WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM asset_services WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, aid)
	})

	var exposure, auth, subdomain string
	if err := e.pool.QueryRow(ctx, `SELECT exposure_state, auth_state, subdomain FROM assets WHERE id = $1`, aid).
		Scan(&exposure, &auth, &subdomain); err != nil {
		t.Fatal(err)
	}
	if exposure != exposureUnknown || auth != authUnknown {
		t.Fatalf("服务资产应为 unknown/unknown，实得 %q/%q", exposure, auth)
	}
	if net.ParseIP(subdomain) != nil {
		t.Fatalf("资产主机看起来是 IP：%q", subdomain)
	}
	t51NoIP(t, map[string]any{"subdomain": subdomain, "exposure": exposure, "auth": auth}, "T62 资产行")
}
