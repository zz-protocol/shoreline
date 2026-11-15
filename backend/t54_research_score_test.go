package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// T54 测试只用本地测试库与纯函数，不访问真实厂商或 GitHub/OSV。

func t54Unique(prefix string) string {
	return fmt.Sprintf("T54%s%d", prefix, time.Now().UnixNano())
}

// t54VendorRoot 建独立厂商 + 根域；清理时删厂商（级联资产/URL）。
func t54VendorRoot(t *testing.T, e *testEnv, suffix string) (vendorID, rootID int64, domain string) {
	t.Helper()
	ctx := context.Background()
	name := "t54-" + strings.NewReplacer("/", "-", " ", "-").Replace(strings.ToLower(t.Name())) + "-" + suffix
	domain = name + ".example.test"
	if err := e.pool.QueryRow(ctx, `INSERT INTO vendors (name) VALUES ($1) RETURNING id`, name).Scan(&vendorID); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, $2) RETURNING id`, vendorID, domain).Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM vendors WHERE id = $1`, vendorID) })
	return vendorID, rootID, domain
}

// t54Component 直接插入带指定四维输入的组件；清理时先删关联再删组件。
func t54Component(t *testing.T, e *testEnv, name, source string, categories []string,
	repo *string, advisory *int, enrichedAt *time.Time, enrichErr *string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO components
		(name, name_key, source, confirmed, categories, github_repo, advisory_count, enriched_at, enrich_error)
		VALUES ($1, $2, $3, true, $4, $5, $6, $7, $8) RETURNING id`,
		name, strings.ToLower(name), source, categories, repo, advisory, enrichedAt, enrichErr).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM asset_components WHERE component_id = $1`, id)
		e.pool.Exec(ctx, `DELETE FROM url_components WHERE component_id = $1`, id)
		e.pool.Exec(ctx, `DELETE FROM components WHERE id = $1`, id)
	})
	return id
}

// t54URLAsset 建存活资产（同主机复用）+ https URL，并挂 url_components 关联。
func t54URLAsset(t *testing.T, e *testEnv, rootID int64, host string, port int, componentID int64) int64 {
	t.Helper()
	ctx := context.Background()
	var assetID, urlID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, alive)
		VALUES ($1, $2, '{http}', 'alive')
		ON CONFLICT (subdomain) DO UPDATE SET alive = 'alive' RETURNING id`, rootID, host).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, sources)
		VALUES ($1, 'https', $2, $3, 200, '{http}') RETURNING id`, assetID, host, port).Scan(&urlID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO url_components (url_id, component_id, source)
		VALUES ($1, $2, 'wappalyzer')`, urlID, componentID); err != nil {
		t.Fatal(err)
	}
	return assetID
}

// t54ServiceAsset 建存活资产并挂 asset_components（服务指纹来源）。
func t54ServiceAsset(t *testing.T, e *testEnv, rootID int64, host string, port int, componentID int64) int64 {
	t.Helper()
	ctx := context.Background()
	var assetID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, alive)
		VALUES ($1, $2, '{dns}', 'alive') RETURNING id`, rootID, host).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, source, evidence)
		VALUES ($1, $2, $3, 'service', '{}')`, assetID, componentID, port); err != nil {
		t.Fatal(err)
	}
	return assetID
}

func t54Float(t *testing.T, v any) float64 {
	t.Helper()
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	default:
		t.Fatalf("not a number: %#v", v)
		return 0
	}
}

func t54Parts(t *testing.T, m map[string]any) map[string]float64 {
	t.Helper()
	raw, ok := m["research_score_parts"].(map[string]any)
	if !ok {
		t.Fatalf("research_score_parts is not a JSON object: %#v", m["research_score_parts"])
	}
	out := map[string]float64{}
	for k, v := range raw {
		out[k] = t54Float(t, v)
	}
	return out
}

func t54RowByName(t *testing.T, list []map[string]any, name string) map[string]any {
	t.Helper()
	for _, m := range list {
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("component %q not in list %v", name, field(list, "name"))
	return nil
}

func TestT54FullScoreAndDirections(t *testing.T) {
	// 四维全满 = 100，四维等权。
	if got := researchTotal(researchScoreParts{Attack: 1, Source: 1, Advisory: 1, Usage: 1}); got != 100 {
		t.Fatalf("full score=%v want 100", got)
	}
	e, c, _ := newAPITest(t, 500)
	_, rootID, domain := t54VendorRoot(t, e, "top")
	name := t54Unique("Top")
	repo := "acme/t54top"
	enrichedAt := time.Now()
	zero := 0
	id := t54Component(t, e, name, "service", []string{"Databases"}, &repo, &zero, &enrichedAt, nil)
	// 同一厂商 20 个资产：usage = 20/25 = 0.8，总分 (1+1+1+0.8)*25 = 95。
	for i := 0; i < 20; i++ {
		t54ServiceAsset(t, e, rootID, fmt.Sprintf("top%02d.%s", i, domain), 3306, id)
	}
	if n, err := recomputeResearchScores(context.Background(), e.pool); err != nil || n < 1 {
		t.Fatalf("recompute n=%d err=%v", n, err)
	}

	list := items(c.want(200, "GET", "/api/components?q="+name, nil))
	row := t54RowByName(t, list, name)
	parts := t54Parts(t, row)
	if parts["attack"] != 1 || parts["source"] != 1 || parts["advisory"] != 1 {
		t.Fatalf("service+Databases+repo+0 advisory parts=%v", parts)
	}
	if math.Abs(parts["usage"]-0.8) > 1e-9 {
		t.Fatalf("usage=%v want 0.8", parts["usage"])
	}
	if got := t54Float(t, row["research_score"]); got != 95 {
		t.Fatalf("research_score=%v want 95", got)
	}

	// 方向：Web 组件（无 service 分类）攻击面更低。
	webName := t54Unique("Web")
	webID := t54Component(t, e, webName, "wappalyzer", []string{"Web servers"}, nil, nil, nil, nil)
	if n, err := recomputeResearchScores(context.Background(), e.pool); err != nil || n < 1 {
		t.Fatalf("recompute web n=%d err=%v", n, err)
	}
	web := c.want(200, "GET", fmt.Sprintf("/api/components/%d", webID), nil)
	webParts := t54Parts(t, web)
	if webParts["attack"] != researchAttackWeb {
		t.Fatalf("web attack=%v want %v", webParts["attack"], researchAttackWeb)
	}
	if t54Float(t, web["research_score"]) >= t54Float(t, row["research_score"]) {
		t.Fatalf("web score %v must be lower than service score %v", web["research_score"], row["research_score"])
	}
}

func TestT54EqualWeightPerDimension(t *testing.T) {
	base := researchScoreParts{Attack: 0.4, Source: 0.5, Advisory: 0.5, Usage: 0}
	delta := 0.25
	cases := []struct {
		name   string
		mutate func(*researchScoreParts)
	}{
		{"attack", func(p *researchScoreParts) { p.Attack += delta }},
		{"source", func(p *researchScoreParts) { p.Source += delta }},
		{"advisory", func(p *researchScoreParts) { p.Advisory += delta }},
		{"usage", func(p *researchScoreParts) { p.Usage += delta }},
	}
	before := researchTotal(base)
	for _, tc := range cases {
		changed := base
		tc.mutate(&changed)
		want := researchScoreWeight * delta * 100
		if got := researchTotal(changed) - before; math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s delta: total change=%v want %v", tc.name, got, want)
		}
	}

	// 数据侧：只改披露维度（0 条 vs NULL），总分差 = 0.25 * 0.5 * 100 = 12.5。
	e, c, _ := newAPITest(t, 500)
	prefix := t54Unique("Equal")
	zero := 0
	aID := t54Component(t, e, prefix+"A", "wappalyzer", []string{"Web servers"}, nil, &zero, nil, nil)
	bID := t54Component(t, e, prefix+"B", "wappalyzer", []string{"Web servers"}, nil, nil, nil, nil)
	if n, err := recomputeResearchScores(context.Background(), e.pool); err != nil || n < 2 {
		t.Fatalf("recompute n=%d err=%v", n, err)
	}
	a := c.want(200, "GET", fmt.Sprintf("/api/components/%d", aID), nil)
	b := c.want(200, "GET", fmt.Sprintf("/api/components/%d", bID), nil)
	if intOf(a["id"]) != aID || intOf(b["id"]) != bID {
		t.Fatalf("ids a=%v b=%v", a["id"], b["id"])
	}
	if diff := t54Float(t, a["research_score"]) - t54Float(t, b["research_score"]); math.Abs(diff-12.5) > 1e-9 {
		t.Fatalf("advisory-only diff=%v want 12.5", diff)
	}
}

func TestT54NullIsNeutral(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	prefix := t54Unique("Null")
	repo := "acme/t54null"
	enrichedAt := time.Now()
	enrichErr := "github: HTTP 403"
	// NULL 披露 + 无 repo → advisory/source 都中性 0.5。
	neutralID := t54Component(t, e, prefix+"Neutral", "wappalyzer", []string{"Web servers"}, nil, nil, nil, nil)
	// 有 repo 但未富集 → source 仍 0.5。
	notEnrichedID := t54Component(t, e, prefix+"NotEnriched", "wappalyzer", []string{"Web servers"}, &repo, nil, nil, nil)
	// 有 repo 且富集失败 → source 0.3。
	failedID := t54Component(t, e, prefix+"Failed", "wappalyzer", []string{"Web servers"}, &repo, nil, &enrichedAt, &enrichErr)
	if n, err := recomputeResearchScores(context.Background(), e.pool); err != nil || n < 3 {
		t.Fatalf("recompute n=%d err=%v", n, err)
	}
	neutral := t54Parts(t, c.want(200, "GET", fmt.Sprintf("/api/components/%d", neutralID), nil))
	if neutral["advisory"] != researchAdvisoryUnknown || neutral["source"] != researchSourceUnknown {
		t.Fatalf("null advisory/source parts=%v", neutral)
	}
	notEnriched := t54Parts(t, c.want(200, "GET", fmt.Sprintf("/api/components/%d", notEnrichedID), nil))
	if notEnriched["source"] != researchSourceUnknown {
		t.Fatalf("not-enriched source=%v want 0.5", notEnriched["source"])
	}
	failed := t54Parts(t, c.want(200, "GET", fmt.Sprintf("/api/components/%d", failedID), nil))
	if failed["source"] != researchSourceFailed {
		t.Fatalf("enrich-failed source=%v want 0.3", failed["source"])
	}
}

func TestT54ZeroAdvisoriesNotSafetyClaim(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	name := t54Unique("ZeroAdvisory")
	repo := "acme/t54zero"
	enrichedAt := time.Now()
	zero := 0
	id := t54Component(t, e, name, "service", []string{"Caching"}, &repo, &zero, &enrichedAt, nil)
	if n, err := recomputeResearchScores(context.Background(), e.pool); err != nil || n < 1 {
		t.Fatalf("recompute n=%d err=%v", n, err)
	}
	row := c.want(200, "GET", fmt.Sprintf("/api/components/%d", id), nil)
	if got := t54Parts(t, row)["advisory"]; got != researchAdvisoryNone {
		t.Fatalf("0 advisories part=%v want 1", got)
	}
	for k := range row {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "safe") || strings.Contains(lower, "secure") {
			t.Fatalf("API must not claim safety, got field %q", k)
		}
	}
	// 文档明确 0 披露只用于排序，不代表安全。
	doc, err := os.ReadFile("../docs/REFERENCE.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"0 披露", "只用于排序", "不代表安全"} {
		if !strings.Contains(string(doc), want) {
			t.Fatalf("REFERENCE must state %q for zero advisories", want)
		}
	}
}

func TestT54UsageGlobalAcrossVendors(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	_, rootA, domainA := t54VendorRoot(t, e, "a")
	vendorB, rootB, domainB := t54VendorRoot(t, e, "b")
	name := t54Unique("Global")
	id := t54Component(t, e, name, "service", []string{"Caching"}, nil, nil, nil, nil)
	// 厂商 A：2 个资产，每个 2 个 URL → 去重后 2。
	t54URLAsset(t, e, rootA, "a1."+domainA, 443, id)
	t54URLAsset(t, e, rootA, "a1."+domainA, 8443, id)
	t54URLAsset(t, e, rootA, "a2."+domainA, 443, id)
	// 厂商 B：3 个资产走服务关联；其中一个同时有 url 关联，跨来源仍按资产去重。
	t54ServiceAsset(t, e, rootB, "b1."+domainB, 6379, id)
	t54ServiceAsset(t, e, rootB, "b2."+domainB, 6379, id)
	t54ServiceAsset(t, e, rootB, "b3."+domainB, 6379, id)
	t54URLAsset(t, e, rootB, "b3."+domainB, 443, id)

	if n, err := recomputeResearchScores(context.Background(), e.pool); err != nil || n < 1 {
		t.Fatalf("recompute n=%d err=%v", n, err)
	}
	// usage = 单厂商最大去重资产数 3（不是 2+3，也不是跨源相加），3/8 = 0.375。
	row := t54RowByName(t, items(c.want(200, "GET", "/api/components?q="+name, nil)), name)
	if got := t54Parts(t, row)["usage"]; math.Abs(got-0.375) > 1e-9 {
		t.Fatalf("usage=%v want 0.375 (max assets in one vendor)", got)
	}
	base := t54Float(t, row["research_score"])
	// 切换 vendor（列表本就不按厂商筛）与详情，分数不变。
	for _, path := range []string{
		fmt.Sprintf("/api/components?q=%s&vendor=%d", name, vendorB),
		fmt.Sprintf("/api/components/%d", intOf(row["id"])),
		fmt.Sprintf("/api/components/%d?include_wildcard=true", intOf(row["id"])),
	} {
		got := c.want(200, "GET", path, nil)
		if strings.Contains(path, "/api/components?") {
			got = t54RowByName(t, items(got), name)
		}
		if score := t54Float(t, got["research_score"]); score != base {
			t.Fatalf("%s score=%v want %v", path, score, base)
		}
	}
}

func TestT54TieOrderStableById(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	_, rootID, domain := t54VendorRoot(t, e, "tie")
	base := t54Unique("Tie")
	aID := t54Component(t, e, base+"A", "wappalyzer", []string{"Web servers"}, nil, nil, nil, nil)
	bID := t54Component(t, e, base+"B", "wappalyzer", []string{"Web servers"}, nil, nil, nil, nil)
	t54URLAsset(t, e, rootID, "tiea."+domain, 443, aID)
	t54URLAsset(t, e, rootID, "tieb."+domain, 443, bID)
	if n, err := recomputeResearchScores(context.Background(), e.pool); err != nil || n < 2 {
		t.Fatalf("recompute n=%d err=%v", n, err)
	}
	first := c.want(200, "GET", "/api/components?q="+base+"&sort=research&limit=1", nil)
	page1 := items(first)
	if len(page1) != 1 {
		t.Fatalf("page1 %v", page1)
	}
	if intOf(page1[0]["id"]) != bID {
		t.Fatalf("tie must order by id desc: got %v want %d", page1[0]["id"], bID)
	}
	if next := fmt.Sprint(first["next_cursor"]); next != fmt.Sprint(bID) {
		t.Fatalf("next_cursor=%v want %d", first["next_cursor"], bID)
	}
	second := c.want(200, "GET", fmt.Sprintf("/api/components?q=%s&sort=research&limit=1&cursor=%d", base, bID), nil)
	page2 := items(second)
	if len(page2) != 1 || intOf(page2[0]["id"]) != aID {
		t.Fatalf("page2 %v want id %d", page2, aID)
	}
	// 同分两次查询顺序稳定。
	again := items(c.want(200, "GET", "/api/components?q="+base+"&sort=research", nil))
	if len(again) != 2 || intOf(again[0]["id"]) != bID || intOf(again[1]["id"]) != aID {
		t.Fatalf("stable order %v", again)
	}
	// min_score 过滤与非法值。
	score := t54Float(t, page1[0]["research_score"])
	keep := int(score)
	if got := items(c.want(200, "GET", fmt.Sprintf("/api/components?q=%s&sort=research&min_score=%d", base, keep+1), nil)); len(got) != 0 {
		t.Fatalf("min_score=%d should filter out score %v, got %v", keep+1, score, field(got, "name"))
	}
	if got := items(c.want(200, "GET", fmt.Sprintf("/api/components?q=%s&min_score=%d", base, keep), nil)); len(got) != 2 {
		t.Fatalf("min_score=%d should keep score %v, got %v", keep, score, field(got, "name"))
	}
	// 复制本列沿用同一 min_score 口径。
	if vals := c.want(200, "GET", fmt.Sprintf("/api/components/values?field=name&q=%s&min_score=%d", base, keep+1), nil); intOf(vals["count"]) != 0 {
		t.Fatalf("values min_score=%d count=%v want 0", keep+1, vals["count"])
	}
	c.want(400, "GET", "/api/components?min_score=101", nil)
	c.want(400, "GET", "/api/components?min_score=-1", nil)
}

func TestT54MCPResearchSortAndMinScore(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	_, rootID, domain := t54VendorRoot(t, e, "mcp")
	base := t54Unique("MCP")
	repo := "acme/t54mcp"
	enrichedAt := time.Now()
	zero := 0
	highID := t54Component(t, e, base+"High", "service", []string{"Databases"}, &repo, &zero, &enrichedAt, nil)
	lowID := t54Component(t, e, base+"Low", "wappalyzer", []string{"Web servers"}, nil, nil, nil, nil)
	t54ServiceAsset(t, e, rootID, "mcphigh."+domain, 3306, highID)
	t54URLAsset(t, e, rootID, "mcplow."+domain, 443, lowID)
	if n, err := recomputeResearchScores(context.Background(), e.pool); err != nil || n < 2 {
		t.Fatalf("recompute n=%d err=%v", n, err)
	}
	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callTool(t, s, "search_components", map[string]any{"q": base, "sort": "research"})
	if isErr {
		t.Fatalf("search_components %v", out)
	}
	mcpItems := out["items"].([]any)
	if len(mcpItems) != 2 || intOf(mcpItems[0].(map[string]any)["id"]) != highID {
		t.Fatalf("mcp research order %v want first id %d", mcpItems, highID)
	}
	if _, ok := mcpItems[0].(map[string]any)["research_score_parts"].(map[string]any); !ok {
		t.Fatalf("MCP items must expose research_score_parts object: %v", mcpItems[0])
	}
	out, isErr = callTool(t, s, "search_components", map[string]any{"q": base, "min_score": 60})
	if isErr || len(out["items"].([]any)) != 1 {
		t.Fatalf("mcp min_score=60 %v isErr=%v", out, isErr)
	}
	if _, isErr := callTool(t, s, "search_components", map[string]any{"min_score": 101}); !isErr {
		t.Fatal("MCP min_score=101 must be rejected")
	}
}

func TestT54MigrationRerunAndDefaults(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	sql, err := migrationFS.ReadFile("migrations/031_research_score.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for i := 0; i < 2; i++ {
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration rerun %d: %v", i+1, err)
		}
	}
	var cols int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'components' AND column_name IN ('research_score','research_score_parts','research_scored_at')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 3 {
		t.Fatalf("research columns=%d want 3", cols)
	}
	var idx int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes
		WHERE tablename = 'components' AND indexname = 'components_research_score_idx'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("research score index missing")
	}
	name := t54Unique("Default")
	id := t54Component(t, e, name, "wappalyzer", []string{"Web servers"}, nil, nil, nil, nil)
	var score float64
	var parts string
	if err := e.pool.QueryRow(ctx, `SELECT research_score::float8, research_score_parts::text FROM components WHERE id = $1`, id).
		Scan(&score, &parts); err != nil {
		t.Fatal(err)
	}
	if score != 0 || parts != "{}" {
		t.Fatalf("defaults score=%v parts=%q", score, parts)
	}
}
