package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaskKey(t *testing.T) {
	if maskKey("") != "" || maskKey("ab") != "****" || maskKey("abcdefgh") != "****efgh" {
		t.Fatalf("mask %q %q %q", maskKey(""), maskKey("ab"), maskKey("abcdefgh"))
	}
}

// mockProbeAccounts：FOFA/Quake/Hunter 账户信息接口；校验认证且不走搜索路径。
func mockProbeAccounts(t *testing.T, wantKey string) *httptest.Server {
	t.Helper()
	var searchHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/info/my": // FOFA
			if r.URL.Query().Get("key") != wantKey {
				json.NewEncoder(w).Encode(map[string]any{"error": true, "errmsg": "Account Invalid"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"error": false, "email": "a@example.test", "fcoin": 1})
		case r.URL.Path == "/api/v3/user/info": // Quake
			if r.Header.Get("X-QuakeToken") != wantKey {
				json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "invalid token"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "Successful.", "data": map[string]any{"username": "q"}})
		case r.URL.Path == "/openApi/userInfo": // Hunter
			if r.URL.Query().Get("api-key") != wantKey {
				json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "auth failed"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"rest_free_point": 10}})
		case strings.Contains(r.URL.Path, "search"):
			searchHits++
			http.Error(w, "search must not be called", 500)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		if searchHits != 0 {
			t.Errorf("search endpoints hit %d times (must be 0)", searchHits)
		}
	})
	return srv
}

func TestCheckProbeAccountMock(t *testing.T) {
	const secret = "test-secret-key-9999"
	srv := mockProbeAccounts(t, secret)
	for _, p := range probeProviders {
		if err := checkProbeAccount(context.Background(), srv.Client(), p, secret, srv.URL); err != nil {
			t.Fatalf("%s ok: %v", p, err)
		}
		err := checkProbeAccount(context.Background(), srv.Client(), p, "wrong", srv.URL)
		if err == nil {
			t.Fatalf("%s bad key should fail", p)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "wrong") {
			t.Fatalf("%s error leaked key: %v", p, err)
		}
	}
}

func TestAPIProbeKeysNoPlaintextAndEnvFallback(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	ctx := context.Background()
	e.pool.Exec(ctx, `DELETE FROM probe_keys`)
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM probe_keys`) })
	const plain = "plain-fofa-key-ABCD"
	srv := mockProbeAccounts(t, plain)
	a.probeHTTP = srv.Client()
	a.probeBases = map[string]string{"fofa": srv.URL, "quake": srv.URL, "hunter": srv.URL}

	// 初始：无 DB、无 env
	t.Setenv("FOFA_KEY", "")
	t.Setenv("QUAKE_KEY", "")
	t.Setenv("HUNTER_KEY", "")
	items0 := items(c.want(200, "GET", "/api/probe-keys", nil))
	if len(items0) != 3 {
		t.Fatalf("items %d", len(items0))
	}
	for _, it := range items0 {
		if it["configured"] != false || it["source"] != "none" || it["mask"] != "" {
			t.Fatalf("empty: %v", it)
		}
		raw, _ := json.Marshal(it)
		if strings.Contains(string(raw), "key_value") || strings.Contains(string(raw), `"key"`) {
			t.Fatalf("list leaked key field: %s", raw)
		}
	}

	// 环境变量回退
	t.Setenv("QUAKE_KEY", "env-quake-key-WXYZ")
	got := items(c.want(200, "GET", "/api/probe-keys", nil))
	var quake map[string]any
	for _, it := range got {
		if it["provider"] == "quake" {
			quake = it
		}
	}
	if quake["source"] != "env" || quake["mask"] != "****WXYZ" || quake["configured"] != true {
		t.Fatalf("env quake %v", quake)
	}
	rawAll, _ := json.Marshal(got)
	if strings.Contains(string(rawAll), "env-quake-key-WXYZ") {
		t.Fatalf("GET leaked env plaintext: %s", rawAll)
	}

	// 写入网页 key：优先于 env；响应无明文
	resp := c.want(200, "PUT", "/api/probe-keys/fofa", map[string]any{"key": plain})
	if resp["source"] != "db" || resp["mask"] != "****ABCD" {
		t.Fatalf("put %v", resp)
	}
	rawPut, _ := json.Marshal(resp)
	if strings.Contains(string(rawPut), plain) {
		t.Fatalf("PUT response leaked plaintext")
	}
	c.want(400, "PUT", "/api/probe-keys/fofa", map[string]any{"key": "  "})
	c.want(400, "PUT", "/api/probe-keys/nope", map[string]any{"key": "x"})

	// DB 优先：同时设 FOFA env，列表仍 source=db
	t.Setenv("FOFA_KEY", "env-fofa-should-not-win-ZZZZ")
	list := items(c.want(200, "GET", "/api/probe-keys", nil))
	for _, it := range list {
		if it["provider"] == "fofa" && it["source"] != "db" {
			t.Fatalf("db should win: %v", it)
		}
	}
	rawList, _ := json.Marshal(list)
	if strings.Contains(string(rawList), plain) || strings.Contains(string(rawList), "env-fofa-should-not-win-ZZZZ") {
		t.Fatalf("list leaked: %s", rawList)
	}

	// 测试连通（mock 账户信息，不打搜索）
	ok := c.want(200, "POST", "/api/probe-keys/fofa/test", nil)
	if ok["ok"] != true || ok["source"] != "db" {
		t.Fatalf("test ok %v", ok)
	}
	rawTest, _ := json.Marshal(ok)
	if strings.Contains(string(rawTest), plain) {
		t.Fatalf("test leaked: %s", rawTest)
	}
	// 错误 key 存在 DB 时改成错的再测
	c.want(200, "PUT", "/api/probe-keys/quake", map[string]any{"key": "bad-quake"})
	a.probeBases["quake"] = srv.URL
	bad := c.want(200, "POST", "/api/probe-keys/quake/test", nil)
	if bad["ok"] != false {
		t.Fatalf("bad test %v", bad)
	}
	if errStr, _ := bad["error"].(string); strings.Contains(errStr, "bad-quake") || strings.Contains(errStr, plain) {
		t.Fatalf("test error leaked key: %v", bad)
	}

	// 删除网页配置 → 回退 env
	c.want(204, "DELETE", "/api/probe-keys/fofa", nil)
	c.want(404, "DELETE", "/api/probe-keys/fofa", nil)
	after := items(c.want(200, "GET", "/api/probe-keys", nil))
	for _, it := range after {
		if it["provider"] == "fofa" {
			if it["source"] != "env" || it["mask"] != "****ZZZZ" {
				t.Fatalf("fallback after delete: %v", it)
			}
		}
	}

	// overlay：网页 key 盖住 env，供下次扫描用
	c.want(200, "PUT", "/api/probe-keys/hunter", map[string]any{"key": "db-hunter-1111"})
	t.Setenv("HUNTER_KEY", "env-hunter-0000")
	pcfg, err := passiveConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := overlayProbeKeys(ctx, e.pool, &pcfg); err != nil {
		t.Fatal(err)
	}
	if pcfg.HunterKey != "db-hunter-1111" {
		t.Fatalf("overlay hunter %q", pcfg.HunterKey)
	}
	names := map[string]bool{}
	for _, s := range passiveSources(pcfg) {
		names[s.Name] = true
	}
	if !names["hunter"] {
		t.Fatal("hunter source should be enabled after db key")
	}

	// 未配置时测试连通 → 400
	e.pool.Exec(ctx, `DELETE FROM probe_keys WHERE provider = 'fofa'`)
	t.Setenv("FOFA_KEY", "")
	c.want(400, "POST", "/api/probe-keys/fofa/test", nil)

	t.Logf("T30: 掩码/优先/回退/连通 mock 通过；响应与错误均无明文")
}

func TestPassiveHandlerPicksDBKeyWithoutRestart(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.pool.Exec(ctx, `DELETE FROM probe_keys`)
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM probe_keys`) })
	t.Setenv("FOFA_KEY", "")
	t.Setenv("QUAKE_KEY", "")
	t.Setenv("HUNTER_KEY", "")
	pcfg, err := passiveConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	pcfg.Subfinder = false
	if _, err := e.pool.Exec(ctx, `INSERT INTO probe_keys (provider, key_value) VALUES ('fofa', $1)
		ON CONFLICT (provider) DO UPDATE SET key_value = EXCLUDED.key_value`, "runtime-fofa-KEY1"); err != nil {
		t.Fatal(err)
	}
	c := pcfg
	if err := overlayProbeKeys(ctx, e.pool, &c); err != nil {
		t.Fatal(err)
	}
	var usedFofa bool
	for _, s := range passiveSources(c) {
		if s.Name == "fofa" {
			usedFofa = true
		}
	}
	if !usedFofa || c.FofaKey != "runtime-fofa-KEY1" {
		t.Fatalf("runtime pick: fofa=%v key=%q", usedFofa, c.FofaKey)
	}
	e.pool.Exec(ctx, `DELETE FROM probe_keys WHERE provider = 'fofa'`)
	c2 := pcfg
	if err := overlayProbeKeys(ctx, e.pool, &c2); err != nil {
		t.Fatal(err)
	}
	for _, s := range passiveSources(c2) {
		if s.Name == "fofa" {
			t.Fatal("fofa should be off after delete")
		}
	}
}

func TestGetJSONDoesNotLeakKeyInError(t *testing.T) {
	// 关闭的端口 → request failed，错误不含 query
	c := passiveConfig{HTTP: &http.Client{}, FofaURL: "http://127.0.0.1:1", FofaKey: "super-secret-key-LEAK"}
	req, _ := http.NewRequest("GET", c.FofaURL+"/api/v1/search/all?key="+c.FofaKey, nil)
	err := c.getJSON(context.Background(), req, &struct{}{})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "super-secret-key-LEAK") || strings.Contains(err.Error(), "key=") {
		t.Fatalf("leaked: %v", err)
	}
}
