package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// t60InsertAsset：T60 测试资产，直接写暴露/鉴权状态，始终存活；不含任何 IP。
func t60InsertAsset(t *testing.T, e *testEnv, host, exposure, auth string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO assets (root_id, subdomain, sources, alive, exposure_state, auth_state)
		VALUES ($1, $2, '{http}', 'alive', $3, $4) RETURNING id`, e.rootID, host, exposure, auth).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// t60InsertURL：写一条 URL 并按 wappalyzer 同步组件关联（组件、evidence 由 syncURLComponents 生成）。
func t60InsertURL(t *testing.T, e *testEnv, assetID int64, host string, tech []string) int64 {
	t.Helper()
	ctx := context.Background()
	var uid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, tech, sources)
		VALUES ($1, 'https', $2, 443, 200, $3, $4, '{http}') RETURNING id`, assetID, host, "t60 "+host, tech).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := syncURLComponents(ctx, e.pool, uid, tech); err != nil {
		t.Fatal(err)
	}
	return uid
}

func t60ComponentID(t *testing.T, e *testEnv, nameKey string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM components WHERE name_key = $1`, nameKey).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// T60：迁移 037 可重跑；旧行只吃默认值（unknown / low / 空），显式写入不被重跑覆盖。
func TestT60MigrationDefaultsAndRerun(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("migrations", "037_component_asset_fields.sql"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for i := 0; i < 2; i++ {
		if _, err := conn.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("rerun %d: %v", i+1, err)
		}
	}

	pfx := strings.ToLower(t.Name())
	host := "old." + pfx + ".example.test"
	svcHost := "old-svc." + pfx + ".example.test"
	var aid, svcAid int64
	// 只写老列，模拟迁移前的历史行。
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, alive)
		VALUES ($1, $2, '{http}', 'alive') RETURNING id`, e.rootID, host).Scan(&aid); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, alive)
		VALUES ($1, $2, '{dns}', 'alive') RETURNING id`, e.rootID, svcHost).Scan(&svcAid); err != nil {
		t.Fatal(err)
	}
	uid := t60InsertURL(t, e, aid, host, []string{"T60OldWeb"})
	webID := t60ComponentID(t, e, "t60oldweb")
	var svcID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed)
		VALUES ('T60OldSvc', 't60oldsvc', 'service', true)
		ON CONFLICT (name_key) DO UPDATE SET source = EXCLUDED.source RETURNING id`).Scan(&svcID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, version, source)
		VALUES ($1, $2, 6379, '7.0', 'service')
		ON CONFLICT (asset_id, component_id, port) DO NOTHING`, svcAid, svcID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = ANY($1)`, []int64{aid, svcAid})
		e.pool.Exec(ctx, `DELETE FROM components WHERE name_key IN ('t60oldweb', 't60oldsvc')`)
	})

	var exposure, auth string
	if err := e.pool.QueryRow(ctx, `SELECT exposure_state, auth_state FROM assets WHERE id = $1`, aid).Scan(&exposure, &auth); err != nil {
		t.Fatal(err)
	}
	if exposure != "unknown" || auth != "unknown" {
		t.Fatalf("旧资产默认值 exposure=%q auth=%q", exposure, auth)
	}
	var confidence, matchedURL, candidate string
	if err := e.pool.QueryRow(ctx, `SELECT confidence, matched_url, candidate_repo FROM url_components
		WHERE url_id = $1 AND component_id = $2`, uid, webID).Scan(&confidence, &matchedURL, &candidate); err != nil {
		t.Fatal(err)
	}
	if confidence != "low" || matchedURL != "" || candidate != "" {
		t.Fatalf("旧 Web 关联默认值 confidence=%q matched_url=%q candidate_repo=%q", confidence, matchedURL, candidate)
	}
	if err := e.pool.QueryRow(ctx, `SELECT confidence, matched_url, candidate_repo FROM asset_components
		WHERE asset_id = $1 AND component_id = $2 AND port = 6379`, svcAid, svcID).Scan(&confidence, &matchedURL, &candidate); err != nil {
		t.Fatal(err)
	}
	if confidence != "low" || matchedURL != "" || candidate != "" {
		t.Fatalf("旧服务关联默认值 confidence=%q matched_url=%q candidate_repo=%q", confidence, matchedURL, candidate)
	}
	var repoConf, aiReview string
	if err := e.pool.QueryRow(ctx, `SELECT repo_confidence, ai_review::text FROM components WHERE id = $1`, webID).Scan(&repoConf, &aiReview); err != nil {
		t.Fatal(err)
	}
	if repoConf != "low" || aiReview != "{}" {
		t.Fatalf("旧组件默认值 repo_confidence=%q ai_review=%q", repoConf, aiReview)
	}

	var idx int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public'
		AND indexname = ANY($1::text[])`,
		[]string{"assets_exposure_state_idx", "url_components_confidence_idx", "asset_components_confidence_idx"}).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 3 {
		t.Fatalf("T60 索引缺失：%d/3", idx)
	}

	// 显式写入后再重跑：已有值不被默认值覆盖。
	if _, err := e.pool.Exec(ctx, `UPDATE assets SET exposure_state = 'api', auth_state = 'anonymous' WHERE id = $1`, aid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE url_components SET confidence = 'high', matched_url = 'https://kept.example.test:443',
		candidate_repo = 'owner/kept' WHERE url_id = $1 AND component_id = $2`, uid, webID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT exposure_state, auth_state FROM assets WHERE id = $1`, aid).Scan(&exposure, &auth); err != nil {
		t.Fatal(err)
	}
	if exposure != "api" || auth != "anonymous" {
		t.Fatalf("重跑覆盖了资产状态 exposure=%q auth=%q", exposure, auth)
	}
	if err := e.pool.QueryRow(ctx, `SELECT confidence, matched_url, candidate_repo FROM url_components
		WHERE url_id = $1 AND component_id = $2`, uid, webID).Scan(&confidence, &matchedURL, &candidate); err != nil {
		t.Fatal(err)
	}
	if confidence != "high" || matchedURL != "https://kept.example.test:443" || candidate != "owner/kept" {
		t.Fatalf("重跑覆盖了关联字段 confidence=%q matched_url=%q candidate_repo=%q", confidence, matchedURL, candidate)
	}
}

// T60：下钻一条组件-资产关系一项，8 字段齐全；服务关系 matched_url 为空，Web 关系是具体 URL。
func TestT60ComponentAssetRelationFields(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	pfx := strings.ToLower(t.Name())

	webHost := "web." + pfx + ".example.test"
	webURL := "https://" + webHost + ":443"
	webAid := t60InsertAsset(t, e, webHost, "api", "anonymous")
	uid := t60InsertURL(t, e, webAid, webHost, []string{"T60WebComp:1.18"})
	webID := t60ComponentID(t, e, "t60webcomp")
	if _, err := e.pool.Exec(ctx, `UPDATE url_components SET confidence = 'high', candidate_repo = 'owner/rel'
		WHERE url_id = $1 AND component_id = $2`, uid, webID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE components SET github_repo = 'owner/fallback' WHERE id = $1`, webID); err != nil {
		t.Fatal(err)
	}

	svcHost := "svc." + pfx + ".example.test"
	svcAid := t60InsertAsset(t, e, svcHost, "live_app", "login_required")
	var svcID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed, github_repo)
		VALUES ('T60SvcComp', 't60svccomp', 'service', true, 'owner/svc')
		ON CONFLICT (name_key) DO UPDATE SET github_repo = EXCLUDED.github_repo, confirmed = true RETURNING id`).Scan(&svcID); err != nil {
		t.Fatal(err)
	}
	evidence := fmt.Sprintf(`{"host":%q,"port":6379,"banner":"redis 7.0","confidence":"medium"}`, svcHost)
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, version, source, evidence, confidence)
		VALUES ($1, $2, 6379, '7.0', 'service', $3::jsonb, 'medium')
		ON CONFLICT (asset_id, component_id, port) DO UPDATE SET evidence = EXCLUDED.evidence, confidence = EXCLUDED.confidence`,
		svcAid, svcID, evidence); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = ANY($1)`, []int64{webAid, svcAid})
		e.pool.Exec(ctx, `DELETE FROM components WHERE name_key IN ('t60webcomp', 't60svccomp')`)
	})

	web := items(c.want(200, "GET", fmt.Sprintf("/api/components/%d/assets", webID), nil))
	t51NoIP(t, map[string]any{"items": toAny(web)}, "T60 详情")
	if len(web) != 1 {
		t.Fatalf("Web 关系数 %d: %v", len(web), web)
	}
	w := web[0]
	want := map[string]string{
		"url":                      webURL,
		"title":                    "t60 " + webHost,
		"version":                  "1.18",
		"source":                   "wappalyzer",
		"component_confidence":     "high",
		"matched_url":              webURL,
		"version_hint":             "1.18",
		"exposure_state":           "api",
		"auth_state":               "anonymous",
		"component_candidate_repo": "owner/rel",
	}
	for k, v := range want {
		if fmt.Sprint(w[k]) != v {
			t.Fatalf("Web 关系字段 %s = %v，期望 %q（整项 %v）", k, w[k], v, w)
		}
	}
	ev, ok := w["component_evidence"].(map[string]any)
	if !ok || ev["tech"] != "T60WebComp:1.18" {
		t.Fatalf("Web evidence %#v", w["component_evidence"])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(w["capture_time"])); err != nil {
		t.Fatalf("capture_time 不是 RFC3339：%v（%v）", w["capture_time"], err)
	}
	urls, ok := w["urls"].([]any)
	if !ok || len(urls) != 1 || urls[0].(map[string]any)["url"] != webURL {
		t.Fatalf("兼容字段 urls %#v", w["urls"])
	}

	svc := items(c.want(200, "GET", fmt.Sprintf("/api/components/%d/assets", svcID), nil))
	t51NoIP(t, map[string]any{"items": toAny(svc)}, "T60 服务详情")
	if len(svc) != 1 {
		t.Fatalf("服务关系数 %d: %v", len(svc), svc)
	}
	s := svc[0]
	if s["matched_url"] != "" || s["url"] != "" {
		t.Fatalf("服务关系 matched_url/url 应为空：%v", s)
	}
	svcWant := map[string]string{
		"component_confidence":     "medium",
		"version_hint":             "7.0",
		"exposure_state":           "live_app",
		"auth_state":               "login_required",
		"component_candidate_repo": "owner/svc",
	}
	for k, v := range svcWant {
		if fmt.Sprint(s[k]) != v {
			t.Fatalf("服务关系字段 %s = %v，期望 %q（整项 %v）", k, s[k], v, s)
		}
	}
	if intOf(s["port"]) != 6379 {
		t.Fatalf("服务关系端口 %v", s["port"])
	}
	sev, ok := s["component_evidence"].(map[string]any)
	if !ok || sev["banner"] != "redis 7.0" {
		t.Fatalf("服务 evidence %#v", s["component_evidence"])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(s["capture_time"])); err != nil {
		t.Fatalf("服务 capture_time 不是 RFC3339：%v（%v）", s["capture_time"], err)
	}
	if urls, ok := s["urls"].([]any); !ok || len(urls) != 0 {
		t.Fatalf("服务关系 urls 应为空数组：%#v", s["urls"])
	}
}

// T60：min_confidence / exposure 按组件-资产关系筛选，计数与列表同口径；默认行为不变。
func TestT60ComponentFiltersAndCounts(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	pfx := strings.ToLower(t.Name())

	apiHost := "api." + pfx + ".example.test"
	liveHost := "live." + pfx + ".example.test"
	lowHost := "low." + pfx + ".example.test"
	adminHost := "admin." + pfx + ".example.test"
	apiAid := t60InsertAsset(t, e, apiHost, "api", "anonymous")
	liveAid := t60InsertAsset(t, e, liveHost, "live_app", "login_required")
	lowAid := t60InsertAsset(t, e, lowHost, "live_app", "anonymous")
	adminAid := t60InsertAsset(t, e, adminHost, "admin", "login_required")

	highURL := t60InsertURL(t, e, apiAid, apiHost, []string{"T60Alpha:2.0"})
	lowURL := t60InsertURL(t, e, liveAid, liveHost, []string{"T60Alpha:2.0"})
	t60InsertURL(t, e, lowAid, lowHost, []string{"T60Beta:1.0"})
	alphaID := t60ComponentID(t, e, "t60alpha")
	if _, err := e.pool.Exec(ctx, `UPDATE url_components SET confidence = 'high' WHERE url_id = $1 AND component_id = $2`, highURL, alphaID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE url_components SET confidence = 'low' WHERE url_id = $1 AND component_id = $2`, lowURL, alphaID); err != nil {
		t.Fatal(err)
	}
	var gammaID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed)
		VALUES ('T60Gamma', 't60gamma', 'service', true)
		ON CONFLICT (name_key) DO UPDATE SET source = EXCLUDED.source RETURNING id`).Scan(&gammaID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, version, source, confidence)
		VALUES ($1, $2, 9200, '1.0', 'service', 'medium')
		ON CONFLICT (asset_id, component_id, port) DO UPDATE SET confidence = EXCLUDED.confidence`, adminAid, gammaID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = ANY($1)`, []int64{apiAid, liveAid, lowAid, adminAid})
		e.pool.Exec(ctx, `DELETE FROM components WHERE name_key IN ('t60alpha', 't60beta', 't60gamma')`)
	})

	counts := func(m map[string]any) (int64, int64, int64) {
		return intOf(m["url_count"]), intOf(m["asset_count"]), intOf(m["vendor_count"])
	}
	only := func(path string, n int) []map[string]any {
		t.Helper()
		list := items(c.want(200, "GET", path, nil))
		t51NoIP(t, map[string]any{"items": toAny(list)}, path)
		if len(list) != n {
			t.Fatalf("%s 返回 %d 项，期望 %d：%v", path, len(list), n, list)
		}
		return list
	}
	expect := func(path string, urlN, assetN, vendorN int64) {
		t.Helper()
		m := only(path, 1)[0]
		if u, a, v := counts(m); u != urlN || a != assetN || v != vendorN {
			t.Fatalf("%s 计数 url=%d asset=%d vendor=%d，期望 %d/%d/%d（%v）", path, u, a, v, urlN, assetN, vendorN, m)
		}
	}

	// 默认口径不变：两条关系都算。
	expect("/api/components?q=T60Alpha", 2, 2, 1)
	expect("/api/components?q=T60Beta", 1, 1, 1)
	expect("/api/components?q=T60Gamma", 0, 1, 1)

	// 置信度：low 关系在 high/medium 下不计入，计数同步收窄。
	expect("/api/components?q=T60Alpha&min_confidence=high", 1, 1, 1)
	expect("/api/components?q=T60Alpha&min_confidence=medium", 1, 1, 1)
	expect("/api/components?q=T60Alpha&min_confidence=low", 2, 2, 1)
	expect("/api/components?q=T60Beta&min_confidence=low", 1, 1, 1)
	expect("/api/components?q=T60Gamma&min_confidence=medium", 0, 1, 1)

	// 不满足条件的组件不入选。
	only("/api/components?q=T60Beta&min_confidence=high", 0)
	only("/api/components?q=T60Gamma&min_confidence=high", 0)

	// 暴露状态：api / live_app 各命中一条关系，多值并集。
	expect("/api/components?q=T60Alpha&exposure=api", 1, 1, 1)
	expect("/api/components?q=T60Alpha&exposure=live_app", 1, 1, 1)
	expect("/api/components?q=T60Alpha&exposure=api,live_app", 2, 2, 1)
	expect("/api/components?q=T60Alpha&exposure=api&min_confidence=high", 1, 1, 1)
	expect("/api/components?q=T60Gamma&exposure=admin", 0, 1, 1)
	only("/api/components?q=T60Gamma&exposure=api", 0)
	only("/api/components?q=T60Beta&exposure=admin", 0)

	// 参数校验。
	c.want(400, "GET", "/api/components?min_confidence=weird", nil)
	c.want(400, "GET", "/api/components?exposure=bogus", nil)

	// 复制本列与列表同口径。
	vals := c.want(200, "GET", "/api/components/values?field=name&q=T60Alpha&min_confidence=high", nil)
	if intOf(vals["count"]) != 1 || stringsOf(vals["items"])[0] != "T60Alpha" {
		t.Fatalf("values 筛选 %+v", vals)
	}
	vals = c.want(200, "GET", "/api/components/values?field=name&q=T60Alpha&exposure=admin", nil)
	if intOf(vals["count"]) != 0 {
		t.Fatalf("values exposure 筛选 %+v", vals)
	}

	// MCP 与 REST 同口径。
	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callTool(t, s, "search_components", map[string]any{"q": "T60Alpha", "min_confidence": "high"})
	if isErr {
		t.Fatalf("search_components %v", out)
	}
	mcpItems := out["items"].([]any)
	if len(mcpItems) != 1 {
		t.Fatalf("MCP 筛选 %d 项：%v", len(mcpItems), mcpItems)
	}
	row := mcpItems[0].(map[string]any)
	if u, a, v := intOf(row["url_count"]), intOf(row["asset_count"]), intOf(row["vendor_count"]); u != 1 || a != 1 || v != 1 {
		t.Fatalf("MCP 计数 %d/%d/%d：%v", u, a, v, row)
	}
	out, isErr = callTool(t, s, "search_components", map[string]any{"q": "T60Gamma", "exposure": []string{"admin"}})
	if isErr || len(out["items"].([]any)) != 1 {
		t.Fatalf("MCP exposure 筛选 %v（isErr=%v）", out, isErr)
	}
	out, isErr = callTool(t, s, "search_components", map[string]any{"min_confidence": "bogus"})
	if !isErr {
		t.Fatalf("MCP 非法 min_confidence 未报错：%v", out)
	}

	// MCP 下钻同样带 8 字段。
	out, isErr = callTool(t, s, "list_component_assets", map[string]any{"component_id": alphaID})
	if isErr {
		t.Fatalf("list_component_assets %v", out)
	}
	down := out["items"].([]any)
	if len(down) != 2 {
		t.Fatalf("MCP 下钻关系数 %d：%v", len(down), down)
	}
	for _, raw := range down {
		m := raw.(map[string]any)
		for _, k := range []string{"component_evidence", "component_confidence", "matched_url", "version_hint",
			"exposure_state", "auth_state", "component_candidate_repo", "capture_time"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("MCP 下钻缺少 %s：%v", k, m)
			}
		}
	}
}
