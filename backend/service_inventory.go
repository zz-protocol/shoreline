package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"regexp"
	"strings"
)

// T57：聚类哈希前去掉动态字段，避免同类 403 因 Date / Request_id / 节点编号不同被拆成多个候选。
var (
	serviceBannerSegmentRE    = regexp.MustCompile(`\.{2,}`)
	serviceBannerDynamicHdrRE = regexp.MustCompile(`(?i)^\s*(?:date|request[-_]?id|node[-_]?info|client[-_]?ip|content-length|via)\s*:`)
)

func normalizeServiceBannerForHash(banner string) string {
	if banner == "" {
		return ""
	}
	parts := serviceBannerSegmentRE.Split(banner, -1)
	out := parts[:0]
	for _, p := range parts {
		if p == "" || serviceBannerDynamicHdrRE.MatchString(p) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "..")
}

// serviceComponentCategories：优先 Wappalyzer 分类；没有时用 YAML 规则声明的分类。
func serviceComponentCategories(display, fingerprint string, rules *serviceFingerprintRuleSet) []string {
	if cats := categoriesForName(display); len(cats) > 0 {
		return cats
	}
	if c := rules.category(fingerprint); c != "" {
		return sortedCats([]string{c})
	}
	return nil
}

func unidentifiedServiceName(banner string) string {
	sum := sha256.Sum256([]byte(normalizeServiceBannerForHash(banner)))
	return "unidentified-service-" + hex.EncodeToString(sum[:])[:12]
}

// syncAssetServicesToComponents 把 asset_services 识别结果写入 components（source=service），不含 IP。
// 组件展示名与分类来自 YAML 规则；证据 JSON 带规则置信度。已有组件的 source / 非空 categories / 数据行不覆盖、不删除。
func syncAssetServicesToComponents(ctx context.Context, q serviceScanQ, targets []serviceScanTarget, rules *serviceFingerprintRuleSet) error {
	if rules == nil {
		loaded, err := loadServiceFingerprintRules(os.Getenv("SERVICE_FINGERPRINT_RULES"))
		if err != nil {
			return err
		}
		rules = loaded
	}
	seen := map[string]struct{}{}
	var hosts []string
	for _, t := range targets {
		h := normalizeHost(t.Host)
		if h == "" || rejectedServiceScanHost(h) {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		return nil
	}
	rs, err := q.Query(ctx, `SELECT asset_id, host, port, open, verified, response_kind, name, version, banner
		FROM asset_services WHERE host = ANY($1)`, hosts)
	if err != nil {
		return err
	}
	defer rs.Close()
	type row struct {
		assetID        *int64
		host           string
		port           int
		open, verified bool
		kind           string
		name, ver, ban string
	}
	var rows []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.assetID, &r.host, &r.port, &r.open, &r.verified, &r.kind, &r.name, &r.ver, &r.ban); err != nil {
			return err
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		return err
	}
	for _, r := range rows {
		// asset_components.asset_id 非空；无资产关联的历史行仍保留在 asset_services，但不进组件库存。
		if r.assetID == nil || *r.assetID <= 0 {
			continue
		}
		// T55：只有收到 banner/probe 响应才算已验证；仅 TCP 可达（空响应）与反代/CDN 不生成候选。
		if !r.open || !(r.verified || r.kind == serviceResponseBanner || r.kind == serviceResponseProbe) {
			continue
		}
		r.ban = sanitizeServiceBanner([]byte(r.ban))
		var display string
		var confirmed bool
		var cats []string
		var fp string
		var repo string
		if n := rules.componentName(r.name); n != "" {
			display, confirmed, fp = n, true, strings.ToLower(r.name)
			cats = serviceComponentCategories(display, fp, rules)
			// T54：YAML 声明的官方 repo 只在组件尚无 repo 时写入，不覆盖用户已填值，也不触发外网富集。
			repo = rules.repo(fp)
		} else if strings.TrimSpace(r.ban) != "" {
			display, confirmed = unidentifiedServiceName(r.ban), false
		} else {
			continue
		}
		var repoArg any
		if repo != "" {
			repoArg = repo
		}
		var id int64
		if err := q.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed, categories, github_repo)
			VALUES ($1, $2, 'service', $3, $4, $5)
			ON CONFLICT (name_key) DO UPDATE SET
				updated_at = now(),
				github_repo = CASE WHEN components.github_repo IS NULL OR components.github_repo = '' THEN EXCLUDED.github_repo ELSE components.github_repo END,
				categories = CASE WHEN cardinality(components.categories) > 0 THEN components.categories ELSE EXCLUDED.categories END
			RETURNING id`, display, strings.ToLower(display), confirmed, sortedCats(cats), repoArg).Scan(&id); err != nil {
			return err
		}
		ev := map[string]any{"host": r.host, "port": r.port, "banner": r.ban, "response_kind": r.kind}
		if c := rules.confidence(r.name); c != "" {
			ev["confidence"] = c
		}
		raw, _ := json.Marshal(ev)
		if _, err := q.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, version, source, evidence)
			VALUES ($1, $2, $3, $4, 'service', $5)
			ON CONFLICT (asset_id, component_id, port) DO UPDATE SET
				version = CASE WHEN EXCLUDED.version <> '' THEN EXCLUDED.version ELSE asset_components.version END,
				evidence = EXCLUDED.evidence,
				ignored_at = NULL`,
			*r.assetID, id, r.port, r.ver, raw); err != nil {
			return err
		}
	}
	// T54：服务组件关联写入后重算研究价值分；失败只记日志，不影响 T52 同步结果。
	if _, err := recomputeResearchScores(ctx, q); err != nil {
		log.Printf("research score after service sync: %v", err)
	}
	return nil
}

// reidentifyStoredServices 用当前 YAML 规则重匹配历史未识别记录（name 为空或以 unidentified-service- 开头）。
// 命中后回填 name/version，并按 T52 流程同步组件：新建的识别组件 confirmed=true（与一次新扫描口径一致）；
// 同资产同端口旧的 unidentified 组件关联置为 ignored，避免同一端口同时挂在正式列表和待确认候选里。
func reidentifyStoredServices(ctx context.Context, q serviceScanQ, rules *serviceFingerprintRuleSet) (int, error) {
	if rules == nil {
		loaded, err := loadServiceFingerprintRules(os.Getenv("SERVICE_FINGERPRINT_RULES"))
		if err != nil {
			return 0, err
		}
		rules = loaded
	}
	rs, err := q.Query(ctx, `SELECT host, port, asset_id, banner FROM asset_services
		WHERE open AND banner <> '' AND response_kind <> 'reverse_proxy'
		  AND (name = '' OR name LIKE 'unidentified-service-%')
		ORDER BY host, port`)
	if err != nil {
		return 0, err
	}
	defer rs.Close()
	type hit struct {
		host    string
		port    int
		assetID *int64
	}
	var hits []hit
	for rs.Next() {
		var h hit
		var banner string
		if err := rs.Scan(&h.host, &h.port, &h.assetID, &banner); err != nil {
			return 0, err
		}
		name, version, _ := rules.match(h.port, []byte(banner))
		if name == "" {
			continue
		}
		// 命中规则说明确实收到了可识别响应：补记为已验证 banner，避免旧行（迁移 032 后 kind=none）漏同步组件。
		tag, err := q.Exec(ctx, `UPDATE asset_services SET name = $1, version = $2,
			response_kind = CASE WHEN response_kind = 'none' THEN 'banner' ELSE response_kind END,
			verified = true
			WHERE host = $3 AND port = $4 AND open AND banner <> ''
			  AND response_kind <> 'reverse_proxy'
			  AND (name = '' OR name LIKE 'unidentified-service-%')`,
			name, version, h.host, h.port)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		hits = append(hits, h)
	}
	if err := rs.Err(); err != nil {
		return 0, err
	}
	if len(hits) == 0 {
		return 0, nil
	}
	seen := map[string]struct{}{}
	targets := make([]serviceScanTarget, 0, len(hits))
	for _, h := range hits {
		host := normalizeHost(h.host)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		var aid int64
		if h.assetID != nil && *h.assetID > 0 {
			aid = *h.assetID
		}
		targets = append(targets, serviceScanTarget{AssetID: aid, Host: host})
	}
	if err := syncAssetServicesToComponents(ctx, q, targets, rules); err != nil {
		return 0, err
	}
	for _, h := range hits {
		if h.assetID == nil || *h.assetID <= 0 {
			continue
		}
		if _, err := q.Exec(ctx, `UPDATE asset_components ac SET ignored_at = now()
			FROM components c
			WHERE ac.component_id = c.id AND ac.asset_id = $1 AND ac.port = $2
			  AND c.name LIKE 'unidentified-service-%' AND ac.ignored_at IS NULL`, *h.assetID, h.port); err != nil {
			return 0, err
		}
	}
	return len(hits), nil
}

// backfillServiceResponseKinds 把迁移 032 后 response_kind='none' 的历史行按已存 banner 重新分档：
// 有响应 → banner（verified=true），反代/CDN 特征 → reverse_proxy（verified=false）；空 banner 保持 none。
// 只处理 none 行，不覆盖新扫描结果，可重复执行。
func backfillServiceResponseKinds(ctx context.Context, q serviceScanQ) (int, error) {
	rs, err := q.Query(ctx, `SELECT host, port, banner FROM asset_services
		WHERE open AND banner <> '' AND response_kind = 'none'`)
	if err != nil {
		return 0, err
	}
	defer rs.Close()
	type row struct {
		host string
		port int
		kind string
	}
	var rows []row
	for rs.Next() {
		var h string
		var p int
		var banner string
		if err := rs.Scan(&h, &p, &banner); err != nil {
			return 0, err
		}
		kind := serviceResponseKind(true, banner, false)
		if kind == serviceResponseNone {
			continue
		}
		rows = append(rows, row{host: h, port: p, kind: kind})
	}
	if err := rs.Err(); err != nil {
		return 0, err
	}
	for _, r := range rows {
		if _, err := q.Exec(ctx, `UPDATE asset_services SET response_kind = $1, verified = $2
			WHERE host = $3 AND port = $4 AND response_kind = 'none'`,
			r.kind, serviceResponseVerified(r.kind), r.host, r.port); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
