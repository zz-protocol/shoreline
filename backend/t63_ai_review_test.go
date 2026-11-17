package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// T63：组件名/别名 → 候选仓库映射命中，且每个 repo 都合法（owner/repo，不含 URL/空白）。
func TestT63ComponentRepoMapping(t *testing.T) {
	hits := map[string]string{
		"Apache Superset":    "apache/superset",
		"Jenkins":            "jenkinsci/jenkins",
		"WordPress":          "WordPress/WordPress",
		"Discuz! X":          "Discuz/DiscuzX",
		"Discuz!":            "Discuz/DiscuzX",
		"GitLab":             "gitlabhq/gitlabhq",
		"Gitea":              "go-gitea/gitea",
		"Nginx":              "nginx/nginx",
		"Apache HTTP Server": "apache/httpd",
		"PHP":                "php/php-src",
		"MySQL":              "mysql/mysql-server",
		"Redis":              "redis/redis",
		"MongoDB":            "mongodb/mongo",
		"Elasticsearch":      "elastic/elasticsearch",
		"Kafka":              "apache/kafka",
		"Apache Kafka":       "apache/kafka",
		"RabbitMQ":           "rabbitmq/rabbitmq-server",
		"Memcached":          "memcached/memcached",
		"Nacos":              "alibaba/nacos",
		"etcd":               "etcd-io/etcd",
		"Tomcat":             "apache/tomcat",
		"Apache Tomcat":      "apache/tomcat",
		"Spring Boot":        "spring-projects/spring-boot",
		"  nginx  ":          "nginx/nginx", // 空白归一
	}
	for name, want := range hits {
		if got := componentCandidateRepo(name); got != want {
			t.Fatalf("componentCandidateRepo(%q) = %q，期望 %q", name, got, want)
		}
	}
	for _, name := range []string{"", "SomeUnknownComponent", "unidentified-service-0123456789ab"} {
		if got := componentCandidateRepo(name); got != "" {
			t.Fatalf("未收录的组件 %q 不应猜仓库，实得 %q", name, got)
		}
	}
	seen := map[string]bool{}
	for key, repo := range componentRepos {
		if key != componentRepoKey(key) {
			t.Fatalf("映射键 %q 未归一（应为 %q）", key, componentRepoKey(key))
		}
		if _, err := parseGitHubRepo(repo); err != nil {
			t.Fatalf("映射 %q → %q 不是合法 owner/repo：%v", key, repo, err)
		}
		seen[repo] = true
	}
	if len(seen) < 20 {
		t.Fatalf("映射覆盖仓库数 %d，少于要求的 20 个组件", len(seen))
	}
}

// T63：Web 识别（wappalyzer 与 title/header 规则）写 candidate_repo；已有值不被后续识别覆盖。
func TestT63WebCandidateRepoWrittenOnce(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	pfx := strings.ToLower(t.Name())

	host := "web." + pfx + ".example.test"
	aid := t61Asset(t, e, host)
	var uid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, tech, sources)
		VALUES ($1, 'https', $2, 443, 200, 't63 web', $3, '{http}') RETURNING id`, aid, host, []string{"Nginx"}).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM url_components WHERE url_id = $1`, uid)
		e.pool.Exec(ctx, `DELETE FROM urls WHERE id = $1`, uid)
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM components c WHERE c.name_key IN ('nginx', 'wordpress', 'php')
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)`)
	})

	if err := syncURLComponentsEvidence(ctx, e.pool, uid, []string{"Nginx:1.25"}, nil, nil, "https://"+host+":443"); err != nil {
		t.Fatal(err)
	}
	var candidate string
	if err := e.pool.QueryRow(ctx, `SELECT candidate_repo FROM url_components uc
		JOIN components c ON c.id = uc.component_id
		WHERE uc.url_id = $1 AND c.name_key = 'nginx'`, uid).Scan(&candidate); err != nil {
		t.Fatal(err)
	}
	if candidate != "nginx/nginx" {
		t.Fatalf("wappalyzer 识别的 candidate_repo = %q，期望 nginx/nginx", candidate)
	}

	// 已有值（人工/AI 填的候选）不被下一次识别覆盖。
	if _, err := e.pool.Exec(ctx, `UPDATE url_components SET candidate_repo = 'owner/kept'
		WHERE url_id = $1 AND component_id = (SELECT id FROM components WHERE name_key = 'nginx')`, uid); err != nil {
		t.Fatal(err)
	}
	if err := syncURLComponentsEvidence(ctx, e.pool, uid, []string{"Nginx:1.27"}, nil, nil, "https://"+host+":443"); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT candidate_repo FROM url_components uc
		JOIN components c ON c.id = uc.component_id
		WHERE uc.url_id = $1 AND c.name_key = 'nginx'`, uid).Scan(&candidate); err != nil {
		t.Fatal(err)
	}
	if candidate != "owner/kept" {
		t.Fatalf("重复识别覆盖了已有 candidate_repo，实得 %q", candidate)
	}

	// title/header 规则同样写映射：标题命中 WordPress。
	hits := matchIdentifyRules("My WordPress Blog", map[string]string{"Server": "nginx"})
	if err := syncIdentifyComponentsEvidence(ctx, e.pool, uid, hits, "https://"+host+":443"); err != nil {
		t.Fatal(err)
	}
	rows, err := e.pool.Query(ctx, `SELECT c.name, uc.candidate_repo FROM url_components uc
		JOIN components c ON c.id = uc.component_id
		WHERE uc.url_id = $1 AND c.name_key IN ('wordpress', 'nginx') ORDER BY c.name_key`, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, repo string
		if err := rows.Scan(&name, &repo); err != nil {
			t.Fatal(err)
		}
		got[name] = repo
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got["WordPress"] != "WordPress/WordPress" {
		t.Fatalf("title 规则命中未写候选仓库：%v", got)
	}
	if got["Nginx"] != "owner/kept" {
		t.Fatalf("header 规则覆盖了已有候选仓库：%v", got)
	}
}

// T63：服务组件优先用 YAML 声明的 repo，没有声明时退回映射；映射不覆盖已有值。
func TestT63ServiceCandidateRepoPrecedence(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	host := "svc." + strings.ToLower(t.Name()) + ".example.test"
	aid := t61Asset(t, e, host)
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, verified, response_kind, name, version, banner)
		VALUES ($1, $2, 19621, true, true, 'banner', 't63declared', '1.0.0', 'T63-declared banner'),
		       ($1, $2, 19622, true, true, 'banner', 't63mapped', '1.0.0', 'T63-mapped banner')`, aid, host); err != nil {
		t.Fatal(err)
	}
	rules := t53MustLoad(t, t53WriteRules(t, `
version: 1
rules:
  - name: t63declared
    component: T63Declared
    category: Databases
    confidence: high
    repo: t63/declared
    ports: [19621]
    banner: '(?i)t63-declared'
  - name: t63mapped
    component: Nginx
    category: Web servers
    confidence: high
    ports: [19622]
    banner: '(?i)t63-mapped'
`))
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM asset_components WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM asset_services WHERE asset_id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, aid)
		e.pool.Exec(ctx, `DELETE FROM components c WHERE c.name_key IN ('t63declared', 'nginx')
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)`)
	})

	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}, rules); err != nil {
		t.Fatal(err)
	}
	candidateOf := func(port int) string {
		t.Helper()
		var repo string
		if err := e.pool.QueryRow(ctx, `SELECT candidate_repo FROM asset_components
			WHERE asset_id = $1 AND port = $2`, aid, port).Scan(&repo); err != nil {
			t.Fatal(err)
		}
		return repo
	}
	if got := candidateOf(19621); got != "t63/declared" {
		t.Fatalf("YAML 声明 repo 的服务 candidate_repo = %q，期望 t63/declared", got)
	}
	if got := candidateOf(19622); got != "nginx/nginx" {
		t.Fatalf("YAML 未声明 repo 时应退回映射，实得 %q", got)
	}
	// 组件级 github_repo 保持确认语义：映射不写它（沿用该组件上已有的值）。
	var githubBefore, githubAfter string
	if err := e.pool.QueryRow(ctx, `SELECT COALESCE(github_repo, '') FROM components WHERE name_key = 'nginx'`).Scan(&githubBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE asset_components SET candidate_repo = 'owner/kept'
		WHERE asset_id = $1 AND port IN (19621, 19622)`, aid); err != nil {
		t.Fatal(err)
	}
	// 已有候选不被映射覆盖；YAML 声明仍以 YAML 为准（T54 口径）。
	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}, rules); err != nil {
		t.Fatal(err)
	}
	if got := candidateOf(19621); got != "t63/declared" {
		t.Fatalf("YAML 声明的 repo 应覆盖已有候选，实得 %q", got)
	}
	if got := candidateOf(19622); got != "owner/kept" {
		t.Fatalf("映射覆盖了已有候选仓库，实得 %q", got)
	}
	if err := e.pool.QueryRow(ctx, `SELECT COALESCE(github_repo, '') FROM components WHERE name_key = 'nginx'`).Scan(&githubAfter); err != nil {
		t.Fatal(err)
	}
	if githubAfter != githubBefore {
		t.Fatalf("映射改动了 components.github_repo：%q → %q", githubBefore, githubAfter)
	}
}

// aiReviewKeys 取 ai_review.by_asset 的键集合。
func aiReviewKeys(t *testing.T, e *testEnv, id int64) map[string]map[string]any {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT ai_review FROM components WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatalf("ai_review 为空")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("ai_review 不是对象：%v（%s）", err, raw)
	}
	byAsset, ok := doc["by_asset"].(map[string]any)
	if !ok {
		t.Fatalf("ai_review 缺少 by_asset：%s", raw)
	}
	out := map[string]map[string]any{}
	for k, v := range byAsset {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("by_asset[%s] 不是对象：%v", k, v)
		}
		out[k] = m
	}
	return out
}

// T63：AI 写回接口鉴权同现有、校验枚举、幂等覆盖同一 component+asset 条目。
func TestT63AIReviewWriteback(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	ctx := context.Background()
	pfx := strings.ToLower(t.Name())

	var cid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed)
		VALUES ('T63Review', 't63review', 'service', true)
		ON CONFLICT (name_key) DO UPDATE SET source = EXCLUDED.source RETURNING id`).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	hostA := "a." + pfx + ".example.test"
	hostB := "b." + pfx + ".example.test"
	aidA := t61Asset(t, e, hostA)
	aidB := t61Asset(t, e, hostB)
	t.Cleanup(func() {
		ctx := context.Background()
		e.pool.Exec(ctx, `DELETE FROM assets WHERE id = ANY($1)`, []int64{aidA, aidB})
		e.pool.Exec(ctx, `DELETE FROM components WHERE name_key = 't63review'`)
	})

	// 鉴权同现有 Bearer：无 token 401。
	noAuth := &apiClient{t: t, srv: c.srv}
	noAuth.want(401, "POST", "/api/components/ai-review", map[string]any{"component_id": cid})

	// 参数校验。
	c.want(400, "POST", "/api/components/ai-review", map[string]any{})
	c.want(400, "POST", "/api/components/ai-review", map[string]any{"component_id": cid, "repo": "https://github.com/owner/repo"})
	c.want(400, "POST", "/api/components/ai-review", map[string]any{"component_id": cid, "repo": "owner"})
	c.want(400, "POST", "/api/components/ai-review", map[string]any{"component_id": cid, "repo_confidence": "certain"})
	c.want(400, "POST", "/api/components/ai-review", map[string]any{"component_id": cid, "audit_priority": "P9"})
	c.want(400, "POST", "/api/components/ai-review", map[string]any{"component_id": cid, "reason": strings.Repeat("x", aiReviewReasonMax+1)})
	c.want(404, "POST", "/api/components/ai-review", map[string]any{"component_id": 999999999})
	c.want(404, "POST", "/api/components/ai-review", map[string]any{"component_id": cid, "asset_id": 999999999})

	body := map[string]any{
		"component_id":    cid,
		"asset_id":        aidA,
		"repo":            "owner/repo-a",
		"repo_confidence": "high",
		"audit_priority":  "P1",
		"reason":          "后台暴露且版本可读",
	}
	out := c.want(200, "POST", "/api/components/ai-review", body)
	t51NoIP(t, out, "T63 ai-review")
	if fmt.Sprint(out["github_repo"]) != "owner/repo-a" || fmt.Sprint(out["repo_confidence"]) != "high" {
		t.Fatalf("写回组件字段 %v", out)
	}

	var repoConf string
	var githubRepo string
	if err := e.pool.QueryRow(ctx, `SELECT repo_confidence, COALESCE(github_repo, '') FROM components WHERE id = $1`, cid).Scan(&repoConf, &githubRepo); err != nil {
		t.Fatal(err)
	}
	if repoConf != "high" || githubRepo != "owner/repo-a" {
		t.Fatalf("components 写回 repo_confidence=%q github_repo=%q", repoConf, githubRepo)
	}
	entries := aiReviewKeys(t, e, cid)
	keyA := fmt.Sprint(aidA)
	if len(entries) != 1 {
		t.Fatalf("首次写回应有 1 条 ai_review，实得 %d：%v", len(entries), entries)
	}
	entry := entries[keyA]
	if fmt.Sprint(entry["audit_priority"]) != "P1" || fmt.Sprint(entry["reason"]) != "后台暴露且版本可读" ||
		fmt.Sprint(entry["repo"]) != "owner/repo-a" || fmt.Sprint(entry["repo_confidence"]) != "high" {
		t.Fatalf("ai_review 条目 %v", entry)
	}
	if _, ok := entry["updated_at"].(string); !ok {
		t.Fatalf("ai_review 条目缺少 updated_at：%v", entry)
	}

	// 幂等：同 component+asset 重复提交覆盖该条，不堆重复条目，仓库/优先级不漂移。
	for i := 0; i < 2; i++ {
		again := c.want(200, "POST", "/api/components/ai-review", body)
		if fmt.Sprint(again["github_repo"]) != "owner/repo-a" {
			t.Fatalf("第 %d 次重复提交仓库漂移：%v", i+2, again)
		}
	}
	entries = aiReviewKeys(t, e, cid)
	if len(entries) != 1 {
		t.Fatalf("重复提交后 ai_review 条目数 %d，期望 1：%v", len(entries), entries)
	}
	if fmt.Sprint(entries[keyA]["audit_priority"]) != "P1" {
		t.Fatalf("重复提交后条目内容漂移：%v", entries[keyA])
	}

	// 另一个资产是另一条；空 repo 不改仓库，只更新置信度与新条目。
	out = c.want(200, "POST", "/api/components/ai-review", map[string]any{
		"component_id":    cid,
		"asset_id":        aidB,
		"repo":            "",
		"repo_confidence": "medium",
		"audit_priority":  "P3",
	})
	if fmt.Sprint(out["github_repo"]) != "owner/repo-a" || fmt.Sprint(out["repo_confidence"]) != "medium" {
		t.Fatalf("空 repo 不应改仓库：%v", out)
	}
	entries = aiReviewKeys(t, e, cid)
	if len(entries) != 2 {
		t.Fatalf("不同资产应合并成 2 条，实得 %d：%v", len(entries), entries)
	}
	if _, ok := entries[fmt.Sprint(aidB)]; !ok {
		t.Fatalf("缺少第二个资产的条目：%v", entries)
	}

	// 未给 asset 时用组件级键。
	c.want(200, "POST", "/api/components/ai-review", map[string]any{
		"component_id":   cid,
		"audit_priority": "P0",
		"reason":         "官方仓库已确认，优先审计",
	})
	entries = aiReviewKeys(t, e, cid)
	if _, ok := entries["component"]; !ok || len(entries) != 3 {
		t.Fatalf("组件级条目不正确：%v", entries)
	}

	// 换仓库后 star/披露数清空（不联网，等下次富集）。
	if _, err := e.pool.Exec(ctx, `UPDATE components SET stars = 123, advisory_count = 4 WHERE id = $1`, cid); err != nil {
		t.Fatal(err)
	}
	c.want(200, "POST", "/api/components/ai-review", map[string]any{"component_id": cid, "repo": "owner/repo-b"})
	var stars, advisories int64
	if err := e.pool.QueryRow(ctx, `SELECT COALESCE(stars, -1), COALESCE(advisory_count, -1) FROM components WHERE id = $1`, cid).Scan(&stars, &advisories); err != nil {
		t.Fatal(err)
	}
	if stars != -1 || advisories != -1 {
		t.Fatalf("换仓库后应清空 star/披露数，实得 %d/%d", stars, advisories)
	}
	// 同仓库重复写不清空。
	if _, err := e.pool.Exec(ctx, `UPDATE components SET stars = 7 WHERE id = $1`, cid); err != nil {
		t.Fatal(err)
	}
	c.want(200, "POST", "/api/components/ai-review", map[string]any{"component_id": cid, "repo": "owner/repo-b"})
	if err := e.pool.QueryRow(ctx, `SELECT COALESCE(stars, -1) FROM components WHERE id = $1`, cid).Scan(&stars); err != nil {
		t.Fatal(err)
	}
	if stars != 7 {
		t.Fatalf("同仓库重复写不应清空 star，实得 %d", stars)
	}
}

// T63：OpenAPI 增加 /api/components/ai-review，exposure 枚举扩到 9 值。
func TestT63OpenAPIAIReview(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	paths := doc["paths"].(map[string]any)
	raw, ok := paths["/api/components/ai-review"]
	if !ok {
		t.Fatalf("OpenAPI 缺少 /api/components/ai-review")
	}
	op := raw.(map[string]any)["post"]
	if op == nil {
		t.Fatalf("/api/components/ai-review 缺少 post")
	}
	post := op.(map[string]any)
	req := post["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
	schema := req["schema"].(map[string]any)["$ref"]
	if schema != "#/components/schemas/AIReviewInput" {
		t.Fatalf("ai-review 请求体 schema = %v", schema)
	}
	input := doc["components"].(map[string]any)["schemas"].(map[string]any)["AIReviewInput"].(map[string]any)
	props := input["properties"].(map[string]any)
	for _, k := range []string{"component_id", "asset_id", "repo", "repo_confidence", "audit_priority", "reason"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("AIReviewInput 缺少字段 %s：%v", k, props)
		}
	}
	required := input["required"].([]any)
	if len(required) != 1 || required[0] != "component_id" {
		t.Fatalf("AIReviewInput required 应为 [component_id]，实得 %v", required)
	}
	params := paths["/api/components"].(map[string]any)["get"].(map[string]any)["parameters"].([]any)
	var enum []any
	for _, p := range params {
		m := p.(map[string]any)
		if m["name"] != "exposure" {
			continue
		}
		enum = m["schema"].(map[string]any)["items"].(map[string]any)["enum"].([]any)
	}
	if len(enum) != len(exposureStates) {
		t.Fatalf("exposure 枚举 %v，期望 %d 个值", enum, len(exposureStates))
	}
	for _, want := range exposureStates {
		found := false
		for _, got := range enum {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("exposure 枚举缺少 %q：%v", want, enum)
		}
	}
}

// T63：前端可选的全部 exposure 取值都能被列表筛选接受。
func TestT63ExposureFilterAcceptsAllStates(t *testing.T) {
	_, c, _ := newAPITest(t, 500)
	for _, v := range exposureStates {
		c.want(200, "GET", "/api/components?exposure="+v, nil)
	}
	c.want(400, "GET", "/api/components?exposure=bogus", nil)
}
