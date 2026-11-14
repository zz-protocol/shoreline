package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseGitHubRepo(t *testing.T) {
	ok := []string{"acme/rarecms", "A-B/c_d.e", "  owner/repo  "}
	want := []string{"acme/rarecms", "A-B/c_d.e", "owner/repo"}
	for i, in := range ok {
		got, err := parseGitHubRepo(in)
		if err != nil || got != want[i] {
			t.Errorf("parseGitHubRepo(%q)=%q,%v want %q", in, got, err, want[i])
		}
	}
	if got, err := parseGitHubRepo(""); err != nil || got != "" {
		t.Errorf("empty should clear, got %q %v", got, err)
	}
	bads := []string{"https://github.com/acme/rarecms", "acme", "acme/rarecms/extra", "acme /repo", "acme/repo.git/tree", "/repo", "acme/", "./repo", "acme/.."}
	for _, in := range bads {
		if _, err := parseGitHubRepo(in); err == nil {
			t.Errorf("parseGitHubRepo(%q) want error", in)
		}
	}
}

// mockEnrichServers 模拟 GitHub / OSV；不访问外网。按路径返回预设 star 与公告数。
func mockEnrichServers(t *testing.T, stars map[string]int, vulns map[string][]string, ghStatus, osvStatus map[string]int) (gh, osv *httptest.Server) {
	t.Helper()
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" || strings.HasSuffix(r.URL.Path, "/fail") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		repo := strings.TrimPrefix(r.URL.Path, "/repos/")
		if st, ok := ghStatus[repo]; ok && st != 0 {
			w.WriteHeader(st)
			return
		}
		n, ok := stars[repo]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"stargazers_count": n})
	}))
	osv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Package struct {
				Purl string `json:"purl"`
			} `json:"package"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		repo := strings.TrimPrefix(body.Package.Purl, "pkg:github/")
		if st, ok := osvStatus[repo]; ok && st != 0 {
			w.WriteHeader(st)
			return
		}
		ids := vulns[repo]
		var list []map[string]string
		for _, id := range ids {
			list = append(list, map[string]string{"id": id})
		}
		if list == nil {
			list = []map[string]string{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vulns": list})
	}))
	t.Cleanup(gh.Close)
	t.Cleanup(osv.Close)
	return gh, osv
}

func attachEnrich(a *api, gh, osv *httptest.Server) {
	a.enrichSync = true
	a.githubBase = gh.URL
	a.osvBase = osv.URL
	a.enrichHTTP = gh.Client()
}

func namesOf(list []map[string]any) []string {
	return field(list, "name")
}

func resetEnrich(t *testing.T, e *testEnv, keys ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE components SET github_repo = NULL, stars = NULL, advisory_count = NULL, enrich_error = NULL, enriched_at = NULL WHERE name_key = ANY($1)`, keys); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `UPDATE components SET github_repo = NULL, stars = NULL, advisory_count = NULL, enrich_error = NULL, enriched_at = NULL WHERE name_key = ANY($1)`, keys)
	})
}

func TestT44EnrichFilterSortAndInvalidRepo(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	if _, err := seedCommonComponents(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	rareA, rareB, _ := seedT43Fixture(t, e)
	resetEnrich(t, e, "rarecms", "rarelib")
	gh, osv := mockEnrichServers(t,
		map[string]int{"acme/rarecms": 42, "acme/rarelib": 5000},
		map[string][]string{"acme/rarecms": {"GHSA-aaaa-bbbb-cccc", "CVE-2024-0001"}, "acme/rarelib": {"GHSA-1", "GHSA-2", "GHSA-3"}},
		nil, nil)
	attachEnrich(a, gh, osv)
	a.cfg.GitHubToken = "" // 无 token 也可富集

	missing := items(c.want(200, "GET", "/api/components?missing_repo=true", nil))
	if !contains(namesOf(missing), "RareCMS") || !contains(namesOf(missing), "RareLib") {
		t.Fatalf("missing_repo before patch: %v", namesOf(missing))
	}

	out := c.want(200, "PATCH", fmt.Sprintf("/api/components/%d", rareA), map[string]any{"github_repo": "acme/rarecms"})
	if fmt.Sprint(out["github_repo"]) != "acme/rarecms" {
		t.Fatalf("repo %v", out["github_repo"])
	}
	if intOf(out["stars"]) != 42 || intOf(out["advisory_count"]) != 2 {
		t.Fatalf("enriched RareCMS stars=%v adv=%v", out["stars"], out["advisory_count"])
	}

	missing = items(c.want(200, "GET", "/api/components?missing_repo=true", nil))
	if contains(namesOf(missing), "RareCMS") || !contains(namesOf(missing), "RareLib") {
		t.Fatalf("missing_repo after one patch: %v", namesOf(missing))
	}

	out = c.want(200, "PATCH", fmt.Sprintf("/api/components/%d", rareB), map[string]any{"github_repo": " acme/rarelib "})
	if intOf(out["stars"]) != 5000 || intOf(out["advisory_count"]) != 3 {
		t.Fatalf("enriched RareLib stars=%v adv=%v", out["stars"], out["advisory_count"])
	}

	byStars := items(c.want(200, "GET", "/api/components?max_stars=100", nil))
	if len(byStars) != 1 || byStars[0]["name"] != "RareCMS" {
		t.Fatalf("max_stars=100 got %v", namesOf(byStars))
	}
	byAdv := items(c.want(200, "GET", "/api/components?max_advisories=2", nil))
	if len(byAdv) != 1 || byAdv[0]["name"] != "RareCMS" {
		t.Fatalf("max_advisories=2 got %v", namesOf(byAdv))
	}

	sorted := items(c.want(200, "GET", "/api/components?sort=stars", nil))
	if len(sorted) < 2 || sorted[0]["name"] != "RareCMS" || sorted[1]["name"] != "RareLib" {
		t.Fatalf("sort=stars got %v", namesOf(sorted))
	}
	sortedA := items(c.want(200, "GET", "/api/components?sort=advisories", nil))
	if len(sortedA) < 2 || sortedA[0]["name"] != "RareCMS" || sortedA[1]["name"] != "RareLib" {
		t.Fatalf("sort=advisories got %v", namesOf(sortedA))
	}

	code, errBody := c.do("PATCH", fmt.Sprintf("/api/components/%d", rareA), map[string]any{"github_repo": "https://github.com/acme/rarecms"})
	if code != 400 {
		t.Fatalf("url repo got %d %v", code, errBody)
	}
	code, errBody = c.do("PATCH", fmt.Sprintf("/api/components/%d", rareA), map[string]any{"github_repo": "acme/rarecms/extra"})
	if code != 400 {
		t.Fatalf("extra path got %d %v", code, errBody)
	}
	c.want(400, "GET", "/api/components?sort=popular", nil)
	c.want(400, "GET", "/api/components?max_stars=-1", nil)
}

func TestT44NoTokenFailureDoesNot500(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	if _, err := seedCommonComponents(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	rareA, _, _ := seedT43Fixture(t, e)
	resetEnrich(t, e, "rarecms", "rarelib")
	gh, osv := mockEnrichServers(t, map[string]int{}, map[string][]string{},
		map[string]int{"acme/missing": http.StatusForbidden},
		map[string]int{"acme/missing": http.StatusServiceUnavailable})
	attachEnrich(a, gh, osv)
	a.cfg.GitHubToken = ""

	out := c.want(200, "PATCH", fmt.Sprintf("/api/components/%d", rareA), map[string]any{"github_repo": "acme/missing"})
	if fmt.Sprint(out["github_repo"]) != "acme/missing" {
		t.Fatalf("repo saved? %v", out)
	}
	if out["enrich_error"] == nil || fmt.Sprint(out["enrich_error"]) == "" || fmt.Sprint(out["enrich_error"]) == "<nil>" {
		t.Fatalf("want enrich_error, got %v", out)
	}
	// 列表不 500，仍能看到该组件
	list := items(c.want(200, "GET", "/api/components", nil))
	if !contains(namesOf(list), "RareCMS") {
		t.Fatalf("list after failed enrich: %v", namesOf(list))
	}
}

func TestT44MCPMatchesRESTFilters(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	if _, err := seedCommonComponents(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	rareA, rareB, _ := seedT43Fixture(t, e)
	resetEnrich(t, e, "rarecms", "rarelib")
	gh, osv := mockEnrichServers(t,
		map[string]int{"acme/rarecms": 42, "acme/rarelib": 5000},
		map[string][]string{"acme/rarecms": {"GHSA-a"}, "acme/rarelib": {"GHSA-1", "GHSA-2"}},
		nil, nil)
	attachEnrich(a, gh, osv)
	c.want(200, "PATCH", fmt.Sprintf("/api/components/%d", rareA), map[string]any{"github_repo": "acme/rarecms"})
	c.want(200, "PATCH", fmt.Sprintf("/api/components/%d", rareB), map[string]any{"github_repo": "acme/rarelib"})

	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	max := 100
	out, isErr := callTool(t, s, "search_components", map[string]any{"max_stars": max, "sort": "stars"})
	if isErr {
		t.Fatalf("search_components %v", out)
	}
	rest := items(c.want(200, "GET", "/api/components?max_stars=100&sort=stars", nil))
	mcpItems := out["items"].([]any)
	if len(mcpItems) != len(rest) {
		t.Fatalf("mcp %d rest %d", len(mcpItems), len(rest))
	}
	if len(rest) != 1 || rest[0]["name"] != "RareCMS" {
		t.Fatalf("rest filter %v", namesOf(rest))
	}
	if fmt.Sprint(mcpItems[0].(map[string]any)["name"]) != "RareCMS" {
		t.Fatalf("mcp items %v", mcpItems)
	}
}
