package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// t61Asset：T61 测试资产，直接写库；不含 IP。
func t61Asset(t *testing.T, e *testEnv, host string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO assets (root_id, subdomain, sources, alive)
		VALUES ($1, $2, '{http}', 'alive') RETURNING id`, e.rootID, host).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// T61：header / meta / title / path / js 样本各自给出正确的 evidence.type 与 confidence；
// 只有 Server 头记 low，标题与正文结构记 medium。
func TestT61WebEvidenceTypesAndConfidence(t *testing.T) {
	cases := []struct {
		label      string
		component  string
		r          *probeResult
		wantType   string
		wantConf   string
		wantInVal  string
		wantInSnip string
	}{
		{
			label:     "header",
			component: "T61HeaderComp",
			r: &probeResult{
				Headers: map[string]string{"X-Powered-By": "T61HeaderComp/1.2.3"},
				body:    []byte("<html><body>ok</body></html>"),
			},
			wantType:  evidenceHeader,
			wantConf:  confidenceHigh,
			wantInVal: "X-Powered-By: T61HeaderComp/1.2.3",
		},
		{
			label:     "server 头只算 low",
			component: "T61ServerComp",
			r: &probeResult{
				Headers: map[string]string{"Server": "T61ServerComp/2.0"},
				body:    []byte("<html><body>ok</body></html>"),
			},
			wantType:  evidenceHeader,
			wantConf:  confidenceLow,
			wantInVal: "Server: T61ServerComp/2.0",
		},
		{
			label:     "meta generator",
			component: "T61MetaComp",
			r: &probeResult{
				body: []byte(`<html><head><meta name="generator" content="T61MetaComp 6.4"></head><body>ok</body></html>`),
			},
			wantType:  evidenceMeta,
			wantConf:  confidenceHigh,
			wantInVal: "T61MetaComp 6.4",
		},
		{
			label:     "title",
			component: "T61TitleComp",
			r: &probeResult{
				Title: "T61TitleComp demo",
				body:  []byte("<html><head><title>T61TitleComp demo</title></head><body>ok</body></html>"),
			},
			wantType:  evidenceTitle,
			wantConf:  confidenceMedium,
			wantInVal: "T61TitleComp demo",
		},
		{
			label:     "path",
			component: "t61pathlib",
			r: &probeResult{
				body: []byte(`<html><head><link rel="stylesheet" href="/assets/t61pathlib/style.css"></head></html>`),
			},
			wantType:  evidencePath,
			wantConf:  confidenceHigh,
			wantInVal: "/assets/t61pathlib/style.css",
		},
		{
			label:     "js 包名",
			component: "T61JsLib",
			r: &probeResult{
				body: []byte(`<html><body><script src="/static/js/T61JsLib.min.js"></script></body></html>`),
			},
			wantType:  evidenceJS,
			wantConf:  confidenceHigh,
			wantInVal: "/static/js/T61JsLib.min.js",
		},
		{
			label:     "内联脚本只算结构匹配",
			component: "T61InlineLib",
			r: &probeResult{
				body: []byte(`<html><body><script>window.T61InlineLib={version:"1"}</script></body></html>`),
			},
			wantType:   evidenceJS,
			wantConf:   confidenceMedium,
			wantInSnip: "T61InlineLib",
		},
		{
			label:     "正文结构匹配",
			component: "T61BodyComp",
			r: &probeResult{
				Title: "demo",
				body:  []byte("<html><body>Powered by T61BodyComp rock-solid</body></html>"),
			},
			wantType:   evidenceBody,
			wantConf:   confidenceMedium,
			wantInSnip: "T61BodyComp",
		},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			ev, ok := webEvidenceFor(c.r, c.component)
			if !ok {
				t.Fatalf("%s：应有证据", c.label)
			}
			if ev.Evidence.Type != c.wantType || ev.Confidence != c.wantConf {
				t.Fatalf("%s：type=%q confidence=%q，期望 %q/%q（%+v）",
					c.label, ev.Evidence.Type, ev.Confidence, c.wantType, c.wantConf, ev.Evidence)
			}
			if c.wantInVal != "" && !strings.Contains(ev.Evidence.Value, c.wantInVal) {
				t.Fatalf("%s：value=%q 不含 %q", c.label, ev.Evidence.Value, c.wantInVal)
			}
			if c.wantInSnip != "" && !strings.Contains(ev.Evidence.Snippet, c.wantInSnip) {
				t.Fatalf("%s：snippet=%q 不含 %q", c.label, ev.Evidence.Snippet, c.wantInSnip)
			}
			if len(ev.Evidence.Snippet) > evidenceSnippetMax {
				t.Fatalf("%s：snippet 超长 %d", c.label, len(ev.Evidence.Snippet))
			}
		})
	}

	// 找不到来源：不编造证据，交回 false（调用方记 low + 空证据）。
	if ev, ok := webEvidenceFor(&probeResult{Title: "demo", body: []byte("<html><body>hello</body></html>")}, "T61GhostComp"); ok {
		t.Fatalf("无来源却给出了证据 %+v", ev)
	}
	if got := deriveTechEvidence(&probeResult{body: []byte("x")}, nil); got != nil {
		t.Fatalf("无组件名应返回 nil，实得 %v", got)
	}
}

// T61：扫描写入 evidence/confidence/matched_url；无来源的组件记 low + 空证据；API 8 字段同口径。
func TestT61ScanWritesEvidenceConfidenceAndMatchedURL(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	host := "ev." + strings.ToLower(t.Name()) + ".example.test"
	aid := t61Asset(t, e, host)
	url := "https://" + host + ":443"
	r := &probeResult{
		Scheme: "https", Host: host, Port: 443, Status: 200, Fingerprint: "t61fp",
		Title:   "T61TitleComp demo",
		Headers: map[string]string{"X-Powered-By": "T61HeaderComp/1.2.3"},
		body:    []byte(`<html><head><title>T61TitleComp demo</title><meta name="generator" content="T61MetaComp 6.4"></head><body>ok</body></html>`),
		Tech:    []string{"T61HeaderComp:1.2.3", "T61MetaComp:6.4", "T61TitleComp", "T61GhostComp"},
	}
	r.TechEvidence = deriveTechEvidence(r, []string{"T61HeaderComp", "T61MetaComp", "T61TitleComp", "T61GhostComp"})
	if err := (&prober{}).storeURL(ctx, e.pool, aid, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM events WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM url_components WHERE url_id IN (SELECT id FROM urls WHERE asset_id = $1)`, aid)
		e.pool.Exec(ctx, `DELETE FROM urls WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM components WHERE name_key = ANY($1)`,
			[]string{"t61headercomp", "t61metacomp", "t61titlecomp", "t61ghostcomp"})
	})

	type row struct {
		confidence, evType, value, snippet, matchedURL, evidence, version string
	}
	read := func(nameKey string) row {
		t.Helper()
		var out row
		if err := e.pool.QueryRow(ctx, `SELECT uc.confidence, COALESCE(uc.evidence->>'type', ''),
				COALESCE(uc.evidence->>'value', ''), COALESCE(uc.evidence->>'snippet', ''),
				uc.matched_url, uc.evidence::text, uc.version
			FROM url_components uc
			JOIN components c ON c.id = uc.component_id
			JOIN urls u ON u.id = uc.url_id
			WHERE u.asset_id = $1 AND c.name_key = $2`, aid, nameKey).Scan(
			&out.confidence, &out.evType, &out.value, &out.snippet, &out.matchedURL, &out.evidence, &out.version); err != nil {
			t.Fatal(err)
		}
		return out
	}

	header := read("t61headercomp")
	if header.evType != evidenceHeader || header.confidence != confidenceHigh || !strings.Contains(header.value, "T61HeaderComp/1.2.3") {
		t.Fatalf("header 证据 %+v", header)
	}
	if header.matchedURL != url || header.version != "1.2.3" {
		t.Fatalf("header matched_url=%q version=%q，期望 %q/1.2.3", header.matchedURL, header.version, url)
	}
	meta := read("t61metacomp")
	if meta.evType != evidenceMeta || meta.confidence != confidenceHigh || !strings.Contains(meta.value, "T61MetaComp 6.4") {
		t.Fatalf("meta 证据 %+v", meta)
	}
	title := read("t61titlecomp")
	if title.evType != evidenceTitle || title.confidence != confidenceMedium {
		t.Fatalf("title 证据 %+v", title)
	}
	ghost := read("t61ghostcomp")
	if ghost.confidence != confidenceLow || ghost.evidence != "{}" {
		t.Fatalf("无来源组件应 low + {}，实得 %+v", ghost)
	}
	if ghost.matchedURL != url {
		t.Fatalf("无来源组件 matched_url=%q，期望 %q", ghost.matchedURL, url)
	}

	// 8 字段 API 同口径返回证据与置信度。
	var cid int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM components WHERE name_key = 't61headercomp'`).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	list := items(c.want(200, "GET", fmt.Sprintf("/api/components/%d/assets", cid), nil))
	t51NoIP(t, map[string]any{"items": toAny(list)}, "T61 详情")
	if len(list) != 1 {
		t.Fatalf("关系数 %d：%v", len(list), list)
	}
	if fmt.Sprint(list[0]["component_confidence"]) != confidenceHigh {
		t.Fatalf("API 置信度 %v", list[0]["component_confidence"])
	}
	if fmt.Sprint(list[0]["matched_url"]) != url {
		t.Fatalf("API matched_url %v，期望 %q", list[0]["matched_url"], url)
	}
	ev, ok := list[0]["component_evidence"].(map[string]any)
	if !ok || ev["type"] != evidenceHeader {
		t.Fatalf("API 证据 %#v", list[0]["component_evidence"])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(list[0]["capture_time"])); err != nil {
		t.Fatalf("capture_time %v", list[0]["capture_time"])
	}
}

// T61：Wappalyzer 识别链路把证据/置信度带进库；隐含组件（MySQL）没有来源，记 low + 空证据。
func TestT61WappalyzerPipelineEvidence(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	host := "wapp." + strings.ToLower(t.Name()) + ".example.test"
	aid := t61Asset(t, e, host)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx")
		w.Header().Set("X-Powered-By", "PHP/8.2")
		fmt.Fprint(w, `<html><head><title>T61 WordPress site</title><meta name="generator" content="WordPress 6.4"></head>`+
			`<body><script src="/wp-includes/js/jquery/jquery.min.js"></script>ok</body></html>`)
	}))
	defer srv.Close()
	cfg, _ := httpConfigFromEnv()
	cfg.RPS, cfg.VerifyRPS, cfg.Schemes, cfg.Timeout = 1000, 1000, []string{"http"}, 2*time.Second
	cfg.Dial = func(string) string { return srv.Listener.Addr().String() }
	p := newProber(cfg)
	r, err := p.getWith(ctx, p.limiter, "http", host)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.storeURL(ctx, e.pool, aid, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM events WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM url_components WHERE url_id IN (SELECT id FROM urls WHERE asset_id = $1)`, aid)
		e.pool.Exec(ctx, `DELETE FROM urls WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, aid)
	})

	want := map[string]struct{ evType, confidence string }{
		"wordpress": {evidenceMeta, confidenceHigh},
		"jquery":    {evidenceJS, confidenceHigh},
		"php":       {evidenceHeader, confidenceHigh},
		"nginx":     {evidenceHeader, confidenceLow},
		"mysql":     {"", confidenceLow},
	}
	rows, err := e.pool.Query(ctx, `SELECT c.name_key, uc.confidence, COALESCE(uc.evidence->>'type', ''), uc.evidence::text, uc.matched_url
		FROM url_components uc JOIN components c ON c.id = uc.component_id
		JOIN urls u ON u.id = uc.url_id WHERE u.asset_id = $1`, aid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]struct{ evType, confidence, evidence, matchedURL string }{}
	for rows.Next() {
		var key, confidence, evType, evidence, matchedURL string
		if err := rows.Scan(&key, &confidence, &evType, &evidence, &matchedURL); err != nil {
			t.Fatal(err)
		}
		got[key] = struct{ evType, confidence, evidence, matchedURL string }{evType, confidence, evidence, matchedURL}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	url := "http://" + host + ":80"
	for key, w := range want {
		row, ok := got[key]
		if !ok {
			t.Fatalf("缺少组件 %s：%v", key, got)
		}
		if row.evType != w.evType || row.confidence != w.confidence {
			t.Fatalf("%s：type=%q confidence=%q，期望 %q/%q（evidence=%s）",
				key, row.evType, row.confidence, w.evType, w.confidence, row.evidence)
		}
		if row.matchedURL != url {
			t.Fatalf("%s：matched_url=%q，期望 %q", key, row.matchedURL, url)
		}
		if key == "mysql" && row.evidence != "{}" {
			t.Fatalf("隐含组件 MySQL 应记空证据，实得 %s", row.evidence)
		}
	}
}

// T61：服务组件沿用 T53 置信度与 banner 证据，写入 confidence/candidate_repo，matched_url 为空。
func TestT61ServiceEvidenceKept(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	host := "svc." + strings.ToLower(t.Name()) + ".example.test"
	aid := t61Asset(t, e, host)
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, verified, response_kind, name, version, banner)
		VALUES ($1, $2, 19611, true, true, 'banner', 't61svc', '1.2.3', 'T61-service banner')`, aid, host); err != nil {
		t.Fatal(err)
	}
	rules := t53MustLoad(t, t53WriteRules(t, `
version: 1
rules:
  - name: t61svc
    component: T61SvcComp
    category: Databases
    confidence: medium
    repo: t61/svc
    ports: [19611]
    banner: '(?i)t61-service'
    version: '([0-9]+\.[0-9]+\.[0-9]+)'
`))
	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}, rules); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM asset_components WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM asset_services WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM components c WHERE c.name_key = 't61svccomp'
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)`)
	})

	var confidence, evType, banner, evConfidence, candidate, matched string
	if err := e.pool.QueryRow(ctx, `SELECT ac.confidence, COALESCE(ac.evidence->>'type', ''),
			COALESCE(ac.evidence->>'banner', ''), COALESCE(ac.evidence->>'confidence', ''),
			ac.candidate_repo, ac.matched_url
		FROM asset_components ac JOIN components c ON c.id = ac.component_id
		WHERE ac.asset_id = $1 AND ac.port = 19611`, aid).Scan(
		&confidence, &evType, &banner, &evConfidence, &candidate, &matched); err != nil {
		t.Fatal(err)
	}
	if confidence != serviceConfidenceMedium || evConfidence != serviceConfidenceMedium {
		t.Fatalf("服务置信度 column=%q evidence=%q，期望 medium", confidence, evConfidence)
	}
	if evType != evidenceBanner || !strings.Contains(banner, "T61-service") {
		t.Fatalf("服务证据 type=%q banner=%q", evType, banner)
	}
	if candidate != "t61/svc" || matched != "" {
		t.Fatalf("服务 candidate_repo=%q matched_url=%q，期望 t61/svc 与空", candidate, matched)
	}
}
