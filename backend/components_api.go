package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// componentFilter：列表/详情计数/下钻/复制共用的统计口径（T43）。
// 默认排除泛解析、客户托管、失活（只统计 alive），并排除常见组件（仅列表）。
type componentFilter struct {
	Q                    string
	MinAssets            int
	MinVendors           int
	ExcludeCommon        bool
	IncludeWild          bool
	IncludeHosted        bool
	IncludeDead          bool
	Vendor               *int64
	MaxStars             *int
	MaxAdvisories        *int
	MinScore             *int     // 研究价值最低分（0–100）
	MinConfidence        string   // T60：high/medium/low，按组件-资产关系置信度过滤
	Exposures            []string // T60：live_app/api/admin，按资产暴露状态过滤
	MissingRepo          bool
	Sort                 string // "", "stars", "advisories", "research"
	IncludeAllCategories bool
	Categories           []string
	Limit                int
	Cursor               int64
}

// scopedURLSQL：符合统计口径的 url_components 行（不含常见排除与关键字，那些在聚合后过滤）。
const scopedURLSQL = `FROM url_components uc
	JOIN components c0 ON c0.id = uc.component_id AND c0.confirmed
	JOIN urls u ON u.id = uc.url_id
	JOIN assets a ON a.id = u.asset_id
	JOIN root_domains r ON r.id = a.root_id
	JOIN vendors v ON v.id = r.vendor_id
	WHERE ($1::bool OR NOT a.wildcard)
	  AND ($2::bool OR NOT a.customer_hosted)
	  AND ($3::bool OR a.alive = 'alive')
	  AND ($4::bigint IS NULL OR r.vendor_id = $4)`

// scopedAssetSQL：服务指纹（T52）资产级关联，口径与 scopedURLSQL 相同。
const scopedAssetSQL = `FROM asset_components ac
	JOIN components c0 ON c0.id = ac.component_id AND c0.confirmed
	JOIN assets a ON a.id = ac.asset_id
	JOIN root_domains r ON r.id = a.root_id
	JOIN vendors v ON v.id = r.vendor_id
	WHERE ($1::bool OR NOT a.wildcard)
	  AND ($2::bool OR NOT a.customer_hosted)
	  AND ($3::bool OR a.alive = 'alive')
	  AND ($4::bigint IS NULL OR r.vendor_id = $4)
	  AND ac.ignored_at IS NULL`

// relationFilterSQL：T60 关系级筛选（$6 置信度下限、$7 暴露状态），只拼在组件列表统计的分支上，
// 研究价值评分（researchScoreRowsSQL）仍用基础口径。conf 为分支的 confidence 列。
func relationFilterSQL(conf string) string {
	return `
	  AND ($6::int = 0 OR CASE ` + conf + ` WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END >= $6)
	  AND (COALESCE(cardinality($7::text[]), 0) = 0 OR a.exposure_state = ANY($7))`
}

const commonExistsSQL = `EXISTS (
	SELECT 1 FROM common_component_names n
	WHERE n.deleted_at IS NULL AND n.enabled AND n.name_key = c.name_key)`

func (f componentFilter) scopeArgs() []any {
	return []any{f.IncludeWild, f.IncludeHosted, f.IncludeDead, f.Vendor}
}

// minConfidenceRank：high=3、medium=2、low=1；空为 0（不过滤）。
func (f componentFilter) minConfidenceRank() int {
	switch f.MinConfidence {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

func componentFilterFromQuery(w http.ResponseWriter, r *http.Request, withListExtras bool) (componentFilter, bool) {
	wild, ok1 := boolParam(w, r, "include_wildcard")
	hosted, ok2 := boolParam(w, r, "include_customer_hosted")
	dead, ok3 := boolParam(w, r, "include_dead")
	if !ok1 || !ok2 || !ok3 {
		return componentFilter{}, false
	}
	f := componentFilter{
		Q:             r.URL.Query().Get("q"),
		IncludeWild:   wild != nil && *wild,
		IncludeHosted: hosted != nil && *hosted,
		IncludeDead:   dead != nil && *dead,
	}
	if strings.ContainsRune(f.Q, 0) {
		writeErr(w, 400, "NUL character not allowed")
		return componentFilter{}, false
	}
	vendor, ok := int64Param(w, r, "vendor")
	if !ok {
		return componentFilter{}, false
	}
	f.Vendor = vendor
	if withListExtras {
		ex, ok := boolParam(w, r, "exclude_common")
		if !ok {
			return componentFilter{}, false
		}
		f.ExcludeCommon = ex == nil || *ex // 默认 true
		var okA, okV bool
		if f.MinAssets, okA = intQuery(w, r, "min_assets", 0); !okA {
			return componentFilter{}, false
		}
		if f.MinVendors, okV = intQuery(w, r, "min_vendors", 0); !okV {
			return componentFilter{}, false
		}
		var okS, okD bool
		if f.MaxStars, okS = optIntQuery(w, r, "max_stars"); !okS {
			return componentFilter{}, false
		}
		if f.MaxAdvisories, okD = optIntQuery(w, r, "max_advisories"); !okD {
			return componentFilter{}, false
		}
		if f.MinScore, okS = optScoreQuery(w, r, "min_score"); !okS {
			return componentFilter{}, false
		}
		switch mc := strings.TrimSpace(r.URL.Query().Get("min_confidence")); mc {
		case "", "high", "medium", "low":
			f.MinConfidence = mc
		default:
			writeErr(w, 400, "min_confidence must be high, medium, or low")
			return componentFilter{}, false
		}
		var okE bool
		if f.Exposures, okE = exposureQuery(w, r, "exposure"); !okE {
			return componentFilter{}, false
		}
		miss, okM := boolParam(w, r, "missing_repo")
		if !okM {
			return componentFilter{}, false
		}
		f.MissingRepo = miss != nil && *miss
		f.Sort = strings.TrimSpace(r.URL.Query().Get("sort"))
		switch f.Sort {
		case "", "stars", "advisories", "research":
		default:
			writeErr(w, 400, "sort must be stars, advisories, research, or empty")
			return componentFilter{}, false
		}
		all, okAll := boolParam(w, r, "include_all_categories")
		if !okAll {
			return componentFilter{}, false
		}
		f.IncludeAllCategories = all != nil && *all
		for _, v := range r.URL.Query()["category"] {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if strings.ContainsRune(v, 0) {
				writeErr(w, 400, "NUL character not allowed")
				return componentFilter{}, false
			}
			f.Categories = append(f.Categories, v)
		}
	}
	return f, true
}

func intQuery(w http.ResponseWriter, r *http.Request, name string, def int) (int, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		writeErr(w, 400, name+" must be an integer >= 0")
		return 0, false
	}
	return n, true
}

func optIntQuery(w http.ResponseWriter, r *http.Request, name string) (*int, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		writeErr(w, 400, name+" must be an integer >= 0")
		return nil, false
	}
	return &n, true
}

// optScoreQuery 解析 0–100 分（research_score 的筛选阈值）。
func optScoreQuery(w http.ResponseWriter, r *http.Request, name string) (*int, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 100 {
		writeErr(w, 400, name+" must be an integer between 0 and 100")
		return nil, false
	}
	return &n, true
}

// exposureQuery 解析 exposure 筛选：可重复参数或逗号分隔，只认 live_app / api / admin，去重保序。
func exposureQuery(w http.ResponseWriter, r *http.Request, name string) ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range r.URL.Query()[name] {
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			switch v {
			case "live_app", "api", "admin":
			default:
				writeErr(w, 400, name+" must be live_app, api, admin")
				return nil, false
			}
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out, true
}

var componentCountSQL = `SELECT c.id, c.name, c.source, c.confirmed, c.github_repo, c.stars, c.advisory_count, c.enrich_error, c.enriched_at, c.categories,
	c.research_score::float8 AS research_score, c.research_score_parts, ` + commonExistsSQL + ` AS is_common,
	count(DISTINCT s.url_id)::bigint AS url_count,
	count(DISTINCT s.asset_id)::bigint AS asset_count,
	count(DISTINCT s.vendor_id)::bigint AS vendor_count
FROM components c
JOIN (
	SELECT uc.component_id, u.id AS url_id, a.id AS asset_id, r.vendor_id
	` + scopedURLSQL + relationFilterSQL("uc.confidence") + `
	UNION ALL
	SELECT ac.component_id, NULL::bigint AS url_id, a.id AS asset_id, r.vendor_id
	` + scopedAssetSQL + relationFilterSQL("ac.confidence") + `
) s ON s.component_id = c.id
WHERE c.confirmed
  AND ($5::text = '' OR strpos(lower(c.name), $5) > 0)
GROUP BY c.id`

func (f componentFilter) countArgs() []any {
	return append(f.scopeArgs(), strings.ToLower(strings.TrimSpace(f.Q)), f.minConfidenceRank(), f.Exposures)
}

func componentSortSQL(sort string) (order, cursorPred string) {
	switch sort {
	case "stars":
		return "c.stars ASC NULLS FIRST, c.id ASC",
			`$C::bigint = 0 OR EXISTS (SELECT 1 FROM counted cc WHERE cc.id = $C AND (
				(c.stars IS NULL AND cc.stars IS NULL AND c.id > cc.id) OR
				(cc.stars IS NOT NULL AND c.stars IS NULL) OR
				(c.stars IS NOT NULL AND cc.stars IS NOT NULL AND (c.stars, c.id) > (cc.stars, cc.id))
			))`
	case "advisories":
		return "c.advisory_count ASC NULLS FIRST, c.id ASC",
			`$C::bigint = 0 OR EXISTS (SELECT 1 FROM counted cc WHERE cc.id = $C AND (
				(c.advisory_count IS NULL AND cc.advisory_count IS NULL AND c.id > cc.id) OR
				(cc.advisory_count IS NOT NULL AND c.advisory_count IS NULL) OR
				(c.advisory_count IS NOT NULL AND cc.advisory_count IS NOT NULL AND (c.advisory_count, c.id) > (cc.advisory_count, cc.id))
			))`
	case "research":
		return "c.research_score DESC, c.id DESC",
			`$C::bigint = 0 OR (c.research_score, c.id) < (
				SELECT cc.research_score, cc.id FROM counted cc WHERE cc.id = $C)`
	default:
		return "c.vendor_count DESC, c.asset_count DESC, c.id DESC",
			`$C::bigint = 0 OR (c.vendor_count, c.asset_count, c.id) < (
				SELECT cc.vendor_count, cc.asset_count, cc.id FROM counted cc WHERE cc.id = $C)`
	}
}

// componentListValuesMax：组件列表整列复制上限，与资产页 assetValuesMax 一致（测试可临时下调）。
var componentListValuesMax = assetValuesMax

// listFilterSQL：与 GET /api/components 相同的 having + 外层筛选（不含排序与分页）。
func (f componentFilter) listFilterSQL() (having, filters string, args []any) {
	having = `HAVING count(DISTINCT s.asset_id) >= $8 AND count(DISTINCT s.vendor_id) >= $9`
	if f.ExcludeCommon {
		having += ` AND NOT (` + commonExistsSQL + `)`
	}
	args = append(f.countArgs(), f.MinAssets, f.MinVendors)
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	filters = `WHERE 1=1`
	if f.MaxStars != nil {
		filters += ` AND c.stars <= ` + add(*f.MaxStars)
	}
	if f.MaxAdvisories != nil {
		filters += ` AND c.advisory_count <= ` + add(*f.MaxAdvisories)
	}
	if f.MinScore != nil {
		filters += ` AND c.research_score >= ` + add(*f.MinScore)
	}
	if f.MissingRepo {
		filters += ` AND (c.github_repo IS NULL OR c.github_repo = '')`
	}
	if len(f.Categories) > 0 {
		filters += ` AND c.categories && ` + add(f.Categories) + `::text[]`
	} else if !f.IncludeAllCategories {
		filters += ` AND (cardinality(c.categories) = 0 OR NOT (c.categories <@ ` + add(hiddenCategories) + `::text[]))`
	}
	return having, filters, args
}

func (a *api) queryComponents(ctx context.Context, f componentFilter) ([]map[string]any, error) {
	if err := checkPage(f.Limit, f.Cursor); err != nil {
		return nil, err
	}
	if f.Vendor != nil {
		if err := checkVendor(f.Vendor); err != nil {
			return nil, err
		}
	}
	having, filters, args := f.listFilterSQL()
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	filterArgs := append([]any{}, args...)
	order, curPred := componentSortSQL(f.Sort)
	curP := add(f.Cursor)
	limP := add(f.Limit + 1)
	curPred = strings.ReplaceAll(curPred, "$C", curP)
	q := `WITH counted AS (` + componentCountSQL + ` ` + having + `)
SELECT * FROM counted c
` + filters + ` AND (` + curPred + `)
ORDER BY ` + order + `
LIMIT ` + limP
	if f.Cursor != 0 {
		var found bool
		if err := a.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM (`+componentCountSQL+` `+having+`) c `+filters+` AND c.id = `+curP+`)`,
			append(filterArgs, f.Cursor)...).Scan(&found); err != nil {
			return nil, err
		}
		if !found {
			return nil, errf(400, "invalid cursor")
		}
	}
	list, err := rowsJSON(a.db.Query(ctx, q, args...))
	if err != nil {
		return nil, err
	}
	normalizeResearchScoreParts(list)
	return list, nil
}

func (a *api) listComponents(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := page(w, r)
	if !ok {
		return
	}
	f, ok := componentFilterFromQuery(w, r, true)
	if !ok {
		return
	}
	// 列表不按厂商筛（厂商筛选在下钻）；忽略误传的 vendor。
	f.Vendor, f.Limit, f.Cursor = nil, limit, cursor
	list, err := a.queryComponents(r.Context(), f)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	list, next := cutPage(list, limit)
	writeJSON(w, 200, map[string]any{"items": list, "next_cursor": next})
}

// listComponentValues：组件列表整列复制（T48）。口径与列表筛选相同，返回去重排序后的全部组件名，上限同资产页。
func (a *api) listComponentValues(w http.ResponseWriter, r *http.Request) {
	f, ok := componentFilterFromQuery(w, r, true)
	if !ok {
		return
	}
	f.Vendor = nil
	field := r.URL.Query().Get("field")
	if field != "" && field != "name" {
		writeErr(w, 400, "field must be name")
		return
	}
	if field == "" {
		field = "name"
	}
	having, filters, args := f.listFilterSQL()
	args = append(args, componentListValuesMax+1)
	limP := fmt.Sprintf("$%d", len(args))
	q := `WITH counted AS (` + componentCountSQL + ` ` + having + `)
SELECT DISTINCT c.name FROM counted c
` + filters + `
ORDER BY 1
LIMIT ` + limP
	rows, err := a.db.Query(r.Context(), q, args...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	truncated := len(out) > componentListValuesMax
	if truncated {
		out = out[:componentListValuesMax]
	}
	writeJSON(w, 200, map[string]any{"field": field, "items": out, "count": len(out), "truncated": truncated})
}

func (a *api) getComponent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	f, ok := componentFilterFromQuery(w, r, false)
	if !ok {
		return
	}
	f.Vendor = nil
	q := `WITH counted AS (` + componentCountSQL + `)
SELECT c.id, c.name, c.source, c.confirmed, c.github_repo, c.stars, c.advisory_count, c.enrich_error, c.enriched_at, c.categories,
	c.research_score::float8 AS research_score, c.research_score_parts,
	COALESCE(x.is_common, ` + commonExistsSQL + `) AS is_common,
	COALESCE(x.url_count, 0) AS url_count,
	COALESCE(x.asset_count, 0) AS asset_count,
	COALESCE(x.vendor_count, 0) AS vendor_count
FROM components c
LEFT JOIN counted x ON x.id = c.id
WHERE c.id = $8`
	list, err := rowsJSON(a.db.Query(r.Context(), q, append(f.countArgs(), id)...))
	if err != nil {
		dbErr(w, err)
		return
	}
	if len(list) == 0 {
		writeErr(w, 404, "not found")
		return
	}
	normalizeResearchScoreParts(list)
	writeJSON(w, 200, list[0])
}

// componentAssetSQL：组件-资产关系明细（Web 取 url_components，服务取 asset_components）。
// 每条关系都带 T60 的 8 字段；$1 组件 id、$2 资产 id 数组。服务关系 matched_url 为空。
const componentAssetSQL = `SELECT u.asset_id AS id, a.subdomain, a.alive, a.wildcard, a.customer_hosted,
	r.vendor_id, v.name AS vendor,
	u.id AS url_id,
	u.scheme || '://' || u.host || ':' || u.port::text AS url,
	COALESCE(u.title, '') AS title,
	uc.version AS version,
	uc.source AS source,
	NULL::int AS port,
	uc.evidence AS component_evidence,
	uc.confidence AS component_confidence,
	COALESCE(NULLIF(uc.matched_url, ''), u.scheme || '://' || u.host || ':' || u.port::text) AS matched_url,
	uc.version AS version_hint,
	a.exposure_state, a.auth_state,
	COALESCE(NULLIF(uc.candidate_repo, ''), c.github_repo, '') AS component_candidate_repo,
	uc.created_at AS capture_time
FROM url_components uc
JOIN urls u ON u.id = uc.url_id
JOIN assets a ON a.id = u.asset_id
JOIN root_domains r ON r.id = a.root_id
JOIN vendors v ON v.id = r.vendor_id
JOIN components c ON c.id = uc.component_id
WHERE uc.component_id = $1 AND u.asset_id = ANY($2)
UNION ALL
SELECT ac.asset_id AS id, a.subdomain, a.alive, a.wildcard, a.customer_hosted,
	r.vendor_id, v.name AS vendor,
	0::bigint AS url_id,
	''::text AS url,
	''::text AS title,
	ac.version AS version,
	ac.source AS source,
	ac.port,
	ac.evidence AS component_evidence,
	ac.confidence AS component_confidence,
	''::text AS matched_url,
	ac.version AS version_hint,
	a.exposure_state, a.auth_state,
	COALESCE(NULLIF(ac.candidate_repo, ''), c.github_repo, '') AS component_candidate_repo,
	ac.created_at AS capture_time
FROM asset_components ac
JOIN assets a ON a.id = ac.asset_id
JOIN root_domains r ON r.id = a.root_id
JOIN vendors v ON v.id = r.vendor_id
JOIN components c ON c.id = ac.component_id
WHERE ac.component_id = $1 AND ac.asset_id = ANY($2) AND ac.ignored_at IS NULL
ORDER BY id DESC, url_id DESC, port DESC NULLS LAST`

// queryComponentAssets 下钻组件-资产关系：仍按资产分页（cursor 为资产 id），返回的每条关系自带 8 字段。
func (a *api) queryComponentAssets(ctx context.Context, id int64, f componentFilter) ([]map[string]any, any, error) {
	if err := checkPage(f.Limit, f.Cursor); err != nil {
		return nil, nil, err
	}
	if f.Vendor != nil {
		if err := checkVendor(f.Vendor); err != nil {
			return nil, nil, err
		}
	}
	var exists bool
	if err := a.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM components WHERE id = $1)`, id).Scan(&exists); err != nil {
		return nil, nil, err
	}
	if !exists {
		return nil, nil, errf(404, "not found")
	}
	args := append(f.scopeArgs(), id, f.Cursor, f.Limit+1)
	rows, err := a.db.Query(ctx, `SELECT a.id
		FROM assets a
		JOIN root_domains r ON r.id = a.root_id
		WHERE a.id IN (
			SELECT DISTINCT a2.id
			FROM url_components uc
			JOIN urls u ON u.id = uc.url_id
			JOIN assets a2 ON a2.id = u.asset_id
			JOIN root_domains r2 ON r2.id = a2.root_id
			WHERE uc.component_id = $5
			  AND ($1::bool OR NOT a2.wildcard)
			  AND ($2::bool OR NOT a2.customer_hosted)
			  AND ($3::bool OR a2.alive = 'alive')
			  AND ($4::bigint IS NULL OR r2.vendor_id = $4)
			UNION
			SELECT DISTINCT a2.id
			FROM asset_components ac
			JOIN assets a2 ON a2.id = ac.asset_id
			JOIN root_domains r2 ON r2.id = a2.root_id
			WHERE ac.component_id = $5
			  AND ac.ignored_at IS NULL
			  AND ($1::bool OR NOT a2.wildcard)
			  AND ($2::bool OR NOT a2.customer_hosted)
			  AND ($3::bool OR a2.alive = 'alive')
			  AND ($4::bigint IS NULL OR r2.vendor_id = $4)
		)
		  AND ($6::bigint = 0 OR a.id < $6)
		ORDER BY a.id DESC
		LIMIT $7`, args...)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, f.Limit+1)
	for rows.Next() {
		var aid int64
		if err := rows.Scan(&aid); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, aid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	var next any
	if len(ids) > f.Limit {
		next = strconv.FormatInt(ids[f.Limit-1], 10)
		ids = ids[:f.Limit]
	}
	out := []map[string]any{}
	if len(ids) == 0 {
		return out, next, nil
	}
	list, err := rowsJSON(a.db.Query(ctx, componentAssetSQL, id, ids))
	if err != nil {
		return nil, nil, err
	}
	for _, m := range list {
		// 兼容旧前端：保留 urls 数组（Web 关系一项，服务关系为空）。
		urls := []any{}
		if urlID, ok := m["url_id"].(int64); ok && urlID != 0 {
			urls = append(urls, map[string]any{
				"id": urlID, "url": m["url"], "title": m["title"], "version": m["version"], "source": m["source"],
			})
		}
		m["urls"] = urls
	}
	return list, next, nil
}

func (a *api) listComponentAssets(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	limit, cursor, ok := page(w, r)
	if !ok {
		return
	}
	f, ok := componentFilterFromQuery(w, r, false)
	if !ok {
		return
	}
	f.Limit, f.Cursor = limit, cursor
	list, next, err := a.queryComponentAssets(r.Context(), id, f)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": list, "next_cursor": next})
}

func (a *api) componentValues(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	f, ok := componentFilterFromQuery(w, r, false)
	if !ok {
		return
	}
	if f.Vendor != nil {
		if err := checkVendor(f.Vendor); err != nil {
			writeAnyErr(w, err)
			return
		}
	}
	var exists bool
	if err := a.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM components WHERE id = $1)`, id).Scan(&exists); err != nil {
		dbErr(w, err)
		return
	}
	if !exists {
		writeErr(w, 404, "not found")
		return
	}
	field := r.URL.Query().Get("field")
	var q string
	switch field {
	case "subdomain":
		q = `SELECT DISTINCT v FROM (
			SELECT a.subdomain AS v
			FROM url_components uc
			JOIN urls u ON u.id = uc.url_id
			JOIN assets a ON a.id = u.asset_id
			JOIN root_domains r ON r.id = a.root_id
			WHERE uc.component_id = $5
			  AND ($1::bool OR NOT a.wildcard)
			  AND ($2::bool OR NOT a.customer_hosted)
			  AND ($3::bool OR a.alive = 'alive')
			  AND ($4::bigint IS NULL OR r.vendor_id = $4)
			UNION
			SELECT a.subdomain AS v
			FROM asset_components ac
			JOIN assets a ON a.id = ac.asset_id
			JOIN root_domains r ON r.id = a.root_id
			WHERE ac.component_id = $5
			  AND ac.ignored_at IS NULL
			  AND ($1::bool OR NOT a.wildcard)
			  AND ($2::bool OR NOT a.customer_hosted)
			  AND ($3::bool OR a.alive = 'alive')
			  AND ($4::bigint IS NULL OR r.vendor_id = $4)
			) s
			ORDER BY 1 LIMIT $6`
	case "url":
		q = `SELECT DISTINCT u.scheme || '://' || u.host || ':' || u.port::text AS v
			FROM url_components uc
			JOIN urls u ON u.id = uc.url_id
			JOIN assets a ON a.id = u.asset_id
			JOIN root_domains r ON r.id = a.root_id
			WHERE uc.component_id = $5
			  AND ($1::bool OR NOT a.wildcard)
			  AND ($2::bool OR NOT a.customer_hosted)
			  AND ($3::bool OR a.alive = 'alive')
			  AND ($4::bigint IS NULL OR r.vendor_id = $4)
			ORDER BY 1 LIMIT $6`
	default:
		writeErr(w, 400, "field must be subdomain|url")
		return
	}
	rows, err := a.db.Query(r.Context(), q, append(f.scopeArgs(), id, assetValuesMax+1)...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	truncated := len(out) > assetValuesMax
	if truncated {
		out = out[:assetValuesMax]
	}
	writeJSON(w, 200, map[string]any{"field": field, "items": out, "count": len(out), "truncated": truncated})
}

func (a *api) listCommonComponents(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT id, name, builtin, enabled, created_at
		FROM common_component_names WHERE deleted_at IS NULL ORDER BY name`)
}

func (a *api) createCommonComponent(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &b) {
		return
	}
	name := strings.TrimSpace(b.Name)
	if name == "" || strings.ContainsRune(name, 0) {
		writeErr(w, 400, "name required")
		return
	}
	var id int64
	err := a.db.QueryRow(r.Context(), `INSERT INTO common_component_names (name, name_key)
		VALUES ($1, $2)
		ON CONFLICT (name_key) DO UPDATE SET deleted_at = NULL, enabled = true, name = EXCLUDED.name
			WHERE common_component_names.deleted_at IS NOT NULL
		RETURNING id`, name, strings.ToLower(name)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 409, "already exists")
		return
	}
	if err != nil {
		dbErr(w, err)
		return
	}
	a.one(w, r, 201, `SELECT id, name, builtin, enabled, created_at FROM common_component_names WHERE id = $1`, id)
}

func (a *api) updateCommonComponent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var b struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Enabled == nil {
		writeErr(w, 400, "enabled required")
		return
	}
	tag, err := a.db.Exec(r.Context(), `UPDATE common_component_names SET enabled = $2 WHERE id = $1 AND deleted_at IS NULL`, id, *b.Enabled)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "not found")
		return
	}
	a.one(w, r, 200, `SELECT id, name, builtin, enabled, created_at FROM common_component_names WHERE id = $1`, id)
}

func (a *api) deleteCommonComponent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	tag, err := a.db.Exec(r.Context(), `UPDATE common_component_names SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "not found")
		return
	}
	w.WriteHeader(204)
}

func (a *api) updateComponent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var b struct {
		GitHubRepo *string `json:"github_repo"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.GitHubRepo == nil {
		writeErr(w, 400, "github_repo is required")
		return
	}
	repo, err := parseGitHubRepo(*b.GitHubRepo)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	var repoArg any
	if repo != "" {
		repoArg = repo
	}
	tag, err := a.db.Exec(r.Context(), `UPDATE components SET github_repo = $2, stars = NULL, advisory_count = NULL, enrich_error = NULL, enriched_at = NULL, updated_at = now() WHERE id = $1`, id, repoArg)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "not found")
		return
	}
	if repo != "" {
		a.scheduleEnrich(id)
	}
	a.getComponent(w, r)
}

func (a *api) listComponentCandidates(w http.ResponseWriter, r *http.Request) {
	configured := a.llm.enabled()
	list, err := rowsJSON(a.db.Query(r.Context(), `SELECT c.id, c.name, c.source, c.confirmed,
		COALESCE(c.github_repo, MAX(x.github_repo)) AS github_repo,
		MAX(x.reason) AS reason,
		count(DISTINCT x.url_id)::bigint AS url_count
		FROM components c
		JOIN (
			SELECT uc.component_id, uc.url_id, uc.evidence->>'github_repo' AS github_repo, uc.evidence->>'reason' AS reason
			FROM url_components uc
			WHERE uc.source = 'llm' AND uc.ignored_at IS NULL AND $1::bool
			UNION ALL
			SELECT ac.component_id, NULL::bigint AS url_id, '' AS github_repo, ac.evidence->>'banner' AS reason
			FROM asset_components ac
			WHERE ac.source = 'service' AND ac.ignored_at IS NULL
		) x ON x.component_id = c.id
		WHERE NOT c.confirmed
		GROUP BY c.id
		ORDER BY c.id DESC`, configured))
	if err != nil {
		dbErr(w, err)
		return
	}
	if list == nil {
		list = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{"configured": configured, "items": list})
}

func (a *api) confirmComponent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var evRepo string
	err := a.db.QueryRow(r.Context(), `SELECT COALESCE(c.github_repo, (SELECT uc.evidence->>'github_repo' FROM url_components uc
			WHERE uc.component_id = c.id AND uc.source = 'llm' AND uc.ignored_at IS NULL
			AND COALESCE(uc.evidence->>'github_repo','') <> '' LIMIT 1), '')
		FROM components c WHERE c.id = $1`, id).Scan(&evRepo)
	if err != nil {
		dbErr(w, err)
		return
	}
	repo, err := parseGitHubRepo(evRepo)
	if err != nil {
		repo = ""
	}
	var repoArg any
	if repo != "" {
		repoArg = repo
	}
	if _, err := a.db.Exec(r.Context(), `UPDATE components SET confirmed = true, github_repo = COALESCE($2, github_repo), updated_at = now() WHERE id = $1`, id, repoArg); err != nil {
		dbErr(w, err)
		return
	}
	if _, err := a.db.Exec(r.Context(), `UPDATE url_components SET ignored_at = NULL WHERE component_id = $1 AND source = 'llm'`, id); err != nil {
		dbErr(w, err)
		return
	}
	if _, err := a.db.Exec(r.Context(), `UPDATE asset_components SET ignored_at = NULL WHERE component_id = $1 AND source = 'service'`, id); err != nil {
		dbErr(w, err)
		return
	}
	if repo != "" {
		a.scheduleEnrich(id)
	}
	a.getComponent(w, r)
}

func (a *api) ignoreComponent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var exists bool
	if err := a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM components WHERE id = $1)`, id).Scan(&exists); err != nil {
		dbErr(w, err)
		return
	}
	if !exists {
		writeErr(w, 404, "not found")
		return
	}
	if _, err := a.db.Exec(r.Context(), `UPDATE url_components SET ignored_at = now()
		WHERE component_id = $1 AND source = 'llm' AND ignored_at IS NULL`, id); err != nil {
		dbErr(w, err)
		return
	}
	if _, err := a.db.Exec(r.Context(), `UPDATE asset_components SET ignored_at = now()
		WHERE component_id = $1 AND source = 'service' AND ignored_at IS NULL`, id); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
