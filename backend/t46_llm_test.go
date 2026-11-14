package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLLMCandidates(t *testing.T) {
	one := parseLLMCandidates("```json\n{\"name\":\"RareLLMApp\",\"github_repo\":\"acme/rare-llm-app\",\"reason\":\"custom app\"}\n```")
	if len(one) != 1 || one[0].Name != "RareLLMApp" || one[0].GitHubRepo != "acme/rare-llm-app" {
		t.Fatalf("fenced object: %+v", one)
	}
	badRepo := parseLLMCandidates(`{"name":"X","github_repo":"https://github.com/a/b","reason":"r"}`)
	if len(badRepo) != 1 || badRepo[0].GitHubRepo != "" {
		t.Fatalf("invalid repo should be dropped: %+v", badRepo)
	}
	if len(parseLLMCandidates(`{"reason":"no name"}`)) != 0 {
		t.Fatal("empty name")
	}
}

func TestLLMUserPromptOmitsBodyAndIP(t *testing.T) {
	p := llmUserPrompt("app.example.test", "/", "Welcome", map[string]string{"Server": "nginx"}, "abcd")
	if !strings.Contains(p, "Host: app.example.test") || !strings.Contains(p, "Path: /") || !strings.Contains(p, "Title: Welcome") || !strings.Contains(p, "Favicon-SHA256: abcd") || !strings.Contains(p, "Server: nginx") {
		t.Fatalf("missing fields: %s", p)
	}
	if strings.Contains(p, "127.0.0.1") || strings.Contains(strings.ToLower(p), "secret_body") {
		t.Fatalf("leaked body or IP: %s", p)
	}
}

func TestLLMConfigFromEnv(t *testing.T) {
	t.Setenv("LLM_API_URL", "")
	t.Setenv("LLM_MODEL", "")
	c, err := llmConfigFromEnv()
	if err != nil || c.enabled() {
		t.Fatalf("empty should be disabled: %+v %v", c, err)
	}
	t.Setenv("LLM_API_URL", "http://llm.example.test/v1")
	t.Setenv("LLM_MODEL", "demo")
	c, err = llmConfigFromEnv()
	if err != nil || !c.enabled() || c.endpoint() != "http://llm.example.test/v1/chat/completions" {
		t.Fatalf("configured %+v %v", c, err)
	}
	t.Setenv("LLM_API_URL", "not-a-url")
	if _, err := llmConfigFromEnv(); err == nil {
		t.Fatal("invalid URL accepted")
	}
	t.Setenv("LLM_API_URL", "http://llm.example.test/v1")
	t.Setenv("LLM_RPS", "0")
	if _, err := llmConfigFromEnv(); err == nil {
		t.Fatal("LLM_RPS=0 accepted")
	}
}

func mockChatServer(t *testing.T, content string, bodies *[]string, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions" {
			http.Error(w, "unexpected "+r.URL.Path, 404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if bodies != nil {
			*bodies = append(*bodies, string(raw))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func insertPlainURL(t *testing.T, e *testEnv, host string) (aid, uid int64) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, alive) VALUES ($1, $2, '{http}', true, 'alive') RETURNING id`,
		e.rootID, host).Scan(&aid); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, sources) VALUES ($1, 'https', $2, 443, 200, 'Welcome', '{http}') RETURNING id`,
		aid, host).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	return
}

func TestT46SuggestUnconfirmedThenConfirm(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	ctx := context.Background()
	if _, err := seedCommonComponents(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	host := "app." + strings.ToLower(t.Name()) + ".example.test"
	name := "Cand" + t.Name()
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM components WHERE name_key = $1`, strings.ToLower(name))
	})
	insertPlainURL(t, e, host)
	var bodies []string
	var calls atomic.Int64
	modelOut, _ := json.Marshal(map[string]string{"name": name, "github_repo": "acme/rare-llm-app", "reason": "custom title"})
	srv := mockChatServer(t, string(modelOut), &bodies, &calls)
	p := newProber(httpConfig{Timeout: 2 * time.Second}).withLLM(llmConfig{URL: srv.URL + "/v1", Key: "test-key", Model: "demo", RPS: 100})
	p.llmHTTP = srv.Client()
	if err := p.suggestLLM(ctx, e.pool, &probeResult{Scheme: "https", Host: host, Port: 443, Title: "Welcome", Headers: map[string]string{"Server": "nginx"}, FaviconHash: "deadbeef"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	sent := bodies[0]
	if !strings.Contains(sent, "Host: "+host) || !strings.Contains(sent, "Path: /") || !strings.Contains(sent, "deadbeef") {
		t.Fatalf("prompt missing fields: %s", sent)
	}
	if strings.Contains(sent, "127.0.0.1") || strings.Contains(sent, "SECRET_BODY") {
		t.Fatalf("payload leaked body or IP: %s", sent)
	}
	if err := p.suggestLLM(ctx, e.pool, &probeResult{Scheme: "https", Host: host, Port: 443, Title: "Welcome", Headers: map[string]string{"Server": "nginx"}, FaviconHash: "deadbeef"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected no second call, got %d", calls.Load())
	}

	a.llm = llmConfig{URL: "http://llm.example.test/v1", Model: "demo"}
	official := items(c.want(200, "GET", "/api/components", nil))
	if contains(namesOf(official), name) {
		t.Fatalf("unconfirmed should not appear in official list: %v", namesOf(official))
	}
	cands := c.want(200, "GET", "/api/component-candidates", nil)
	if cands["configured"] != true {
		t.Fatalf("configured %v", cands["configured"])
	}
	list := items(cands)
	if len(list) != 1 || list[0]["name"] != name {
		t.Fatalf("candidates %v", list)
	}
	id := intOf(list[0]["id"])
	out := c.want(200, "POST", fmt.Sprintf("/api/components/%d/confirm", id), nil)
	if out["confirmed"] != true && fmt.Sprint(out["confirmed"]) != "true" {
		t.Fatalf("confirm resp %v", out)
	}
	official = items(c.want(200, "GET", "/api/components", nil))
	if !contains(namesOf(official), name) {
		t.Fatalf("confirmed missing from official list: %v", namesOf(official))
	}
	cands = c.want(200, "GET", "/api/component-candidates", nil)
	if len(items(cands)) != 0 {
		t.Fatalf("confirmed still in candidates: %v", items(cands))
	}
	var src string
	var confirmed bool
	if err := e.pool.QueryRow(ctx, `SELECT source, confirmed FROM components WHERE name = $1`, name).Scan(&src, &confirmed); err != nil {
		t.Fatal(err)
	}
	if src != "llm" || !confirmed {
		t.Fatalf("component source=%s confirmed=%v", src, confirmed)
	}
}

func TestT46IgnoreAndUnconfiguredZeroRequests(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	ctx := context.Background()
	host := "ig." + strings.ToLower(t.Name()) + ".example.test"
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM components WHERE name_key = $1`, strings.ToLower(t.Name()))
	})
	insertPlainURL(t, e, host)
	var calls atomic.Int64
	srv := mockChatServer(t, `{"name":"`+t.Name()+`","reason":"x"}`, nil, &calls)
	p := newProber(httpConfig{Timeout: 2 * time.Second}).withLLM(llmConfig{URL: srv.URL + "/v1", Model: "demo", RPS: 100})
	p.llmHTTP = srv.Client()
	if err := p.suggestLLM(ctx, e.pool, &probeResult{Scheme: "https", Host: host, Port: 443, Title: "X"}); err != nil {
		t.Fatal(err)
	}
	a.llm = llmConfig{URL: "http://llm.example.test/v1", Model: "demo"}
	list := items(c.want(200, "GET", "/api/component-candidates", nil))
	if len(list) != 1 {
		t.Fatalf("cands %v", list)
	}
	id := intOf(list[0]["id"])
	c.want(200, "POST", fmt.Sprintf("/api/components/%d/ignore", id), nil)
	if len(items(c.want(200, "GET", "/api/component-candidates", nil))) != 0 {
		t.Fatal("ignored still listed")
	}
	if contains(namesOf(items(c.want(200, "GET", "/api/components", nil))), t.Name()) {
		t.Fatal("ignored appeared in official list")
	}

	var trip atomic.Int64
	trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { trip.Add(1) }))
	t.Cleanup(trap.Close)
	p2 := newProber(httpConfig{Timeout: 2 * time.Second})
	p2.llmHTTP = trap.Client()
	host2 := "off." + strings.ToLower(t.Name()) + ".example.test"
	insertPlainURL(t, e, host2)
	if err := p2.suggestLLM(ctx, e.pool, &probeResult{Scheme: "https", Host: host2, Port: 443, Title: "Y"}); err != nil {
		t.Fatal(err)
	}
	if trip.Load() != 0 {
		t.Fatalf("unconfigured sent %d requests", trip.Load())
	}
	a.llm = llmConfig{}
	st := c.want(200, "GET", "/api/component-candidates", nil)
	if st["configured"] != false && fmt.Sprint(st["configured"]) != "false" {
		t.Fatalf("want unconfigured, got %v", st)
	}
}

func TestT46HTTPHandlerNoUncommonSkipsOrSuggests(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := seedCommonComponents(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	host := "scan." + strings.ToLower(t.Name()) + ".example.test"
	if _, err := e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, alive) VALUES ($1, $2, '{http}', true, 'alive')`, e.rootID, host); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var bodies []string
	const secret = "SECRET_BODY_SHOULD_NOT_BE_SENT"
	name := "Scan" + t.Name()
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM components WHERE name_key = $1`, strings.ToLower(name))
	})
	cand, _ := json.Marshal(map[string]string{"name": name, "reason": "plain page"})
	llm := mockChatServer(t, string(cand), &bodies, &calls)
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
		fmt.Fprintf(w, "<html><head><title>Default Site</title></head><body>%s</body></html>", secret)
	}))
	t.Cleanup(page.Close)
	cfg, _ := httpConfigFromEnv()
	cfg.RPS, cfg.Schemes, cfg.Timeout = 1000, []string{"http"}, 2*time.Second
	cfg.Dial = func(string) string { return page.Listener.Addr().String() }
	p := newProber(cfg).withLLM(llmConfig{URL: llm.URL + "/v1", Model: "demo", RPS: 100})
	p.llmHTTP = llm.Client()
	if _, err := httpHandler(e.pool, p)(ctx, 0, []string{host}); err != nil {
		t.Fatal(err)
	}
	for _, b := range bodies {
		if strings.Contains(b, secret) {
			t.Fatalf("body sent to model: %s", b)
		}
		if strings.Contains(b, "127.0.0.1") {
			t.Fatalf("IP sent to model: %s", b)
		}
		if addr := page.Listener.Addr().String(); strings.Contains(b, addr) {
			t.Fatalf("dial addr sent to model: %s", b)
		}
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM components WHERE name=$1 AND confirmed=false`, name).Scan(&n)
	if calls.Load() == 0 && n == 0 {
		var tech []string
		e.pool.QueryRow(ctx, `SELECT tech FROM urls WHERE host=$1`, host).Scan(&tech)
		t.Fatalf("LLM not called; url tech=%v (want eligible after excluding common Nginx)", tech)
	}
	if calls.Load() != 1 || n != 1 {
		t.Fatalf("calls=%d components=%d bodies=%v", calls.Load(), n, bodies)
	}
	if !strings.Contains(bodies[0], "Host: "+host) || !strings.Contains(bodies[0], "Path: /") {
		t.Fatalf("prompt: %s", bodies[0])
	}
}

func TestT46UncommonURLDoesNotCallLLM(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	host := "rare." + strings.ToLower(t.Name()) + ".example.test"
	_, uid := insertPlainURL(t, e, host)
	if err := syncURLComponents(ctx, e.pool, uid, []string{"RareCMS"}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	srv := mockChatServer(t, `{"name":"ShouldNotAppear","reason":"x"}`, nil, &calls)
	p := newProber(httpConfig{Timeout: 2 * time.Second}).withLLM(llmConfig{URL: srv.URL + "/v1", Model: "demo", RPS: 100})
	p.llmHTTP = srv.Client()
	if err := p.suggestLLM(ctx, e.pool, &probeResult{Scheme: "https", Host: host, Port: 443, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("should not call LLM for uncommon URL, calls=%d", calls.Load())
	}
}

func TestT46LLMDisabledWhenURLInvalidOrMissingModel(t *testing.T) {
	if (llmConfig{URL: "http://llm.example.test", Model: ""}).enabled() {
		t.Fatal("missing model should be disabled")
	}
	if (llmConfig{}).enabled() {
		t.Fatal("empty enabled")
	}
}
