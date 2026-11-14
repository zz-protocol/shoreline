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
	Q             string
	MinAssets     int
	MinVendors    int
	ExcludeCommon bool
	IncludeWild   bool
	IncludeHosted bool
	IncludeDead   bool
	Vendor        *int64
	MaxStars      *int
	MaxAdvisories *int
	MissingRepo   bool
	Sort          string // "", "stars", "advisories"
	Limit         int
	Cursor        int64
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

const commonExistsSQL = `EXISTS (
	SELECT 1 FROM common_component_names n
	WHERE n.deleted_at IS NULL AND n.enabled AND n.name_key = c.name_key)`

func (f componentFilter) scopeArgs() []any {
	return []any{f.IncludeWild, f.IncludeHosted, f.IncludeDead, f.Vendor}
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
		miss, okM := boolParam(w, r, "missing_repo")
		if !okM {
			return componentFilter{}, false
		}
		f.MissingRepo = miss != nil && *miss
		f.Sort = strings.TrimSpace(r.URL.Query().Get("sort"))
		switch f.Sort {
		case "", "stars", "advisories":
		default:
			writeErr(w, 400, "sort must be stars, advisories, or empty")
			return componentFilter{}, false
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

const componentCountSQL = `SELECT c.id, c.name, c.source, c.confirmed, c.github_repo, c.stars, c.advisory_count, c.enrich_error, c.enriched_at, ` + commonExistsSQL + ` AS is_common,
	count(DISTINCT s.url_id)::bigint AS url_count,
	count(DISTINCT s.asset_id)::bigint AS asset_count,
	count(DISTINCT s.vendor_id)::bigint AS vendor_count
FROM components c
JOIN (
	SELECT uc.component_id, u.id AS url_id, a.id AS asset_id, r.vendor_id
	` + scopedURLSQL + `
) s ON s.component_id = c.id
WHERE c.confirmed
  AND ($5::text = '' OR strpos(lower(c.name), $5) > 0)
GROUP BY c.id`

func (f componentFilter) countArgs() []any {
	return append(f.scopeArgs(), strings.ToLower(strings.TrimSpace(f.Q)))
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
	default:
		return "c.vendor_count DESC, c.asset_count DESC, c.id DESC",
			`$C::bigint = 0 OR (c.vendor_count, c.asset_count, c.id) < (
				SELECT cc.vendor_count, cc.asset_count, cc.id FROM counted cc WHERE cc.id = $C)`
	}
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
	having := `HAVING count(DISTINCT s.asset_id) >= $6 AND count(DISTINCT s.vendor_id) >= $7`
	if f.ExcludeCommon {
		having += ` AND NOT (` + commonExistsSQL + `)`
	}
	args := append(f.countArgs(), f.MinAssets, f.MinVendors)
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	filters := `WHERE 1=1`
	if f.MaxStars != nil {
		filters += ` AND c.stars <= ` + add(*f.MaxStars)
	}
	if f.MaxAdvisories != nil {
		filters += ` AND c.advisory_count <= ` + add(*f.MaxAdvisories)
	}
	if f.MissingRepo {
		filters += ` AND (c.github_repo IS NULL OR c.github_repo = '')`
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
	return rowsJSON(a.db.Query(ctx, q, args...))
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
SELECT c.id, c.name, c.source, c.confirmed, c.github_repo, c.stars, c.advisory_count, c.enrich_error, c.enriched_at,
	COALESCE(x.is_common, ` + commonExistsSQL + `) AS is_common,
	COALESCE(x.url_count, 0) AS url_count,
	COALESCE(x.asset_count, 0) AS asset_count,
	COALESCE(x.vendor_count, 0) AS vendor_count
FROM components c
LEFT JOIN counted x ON x.id = c.id
WHERE c.id = $6`
	list, err := rowsJSON(a.db.Query(r.Context(), q, append(f.countArgs(), id)...))
	if err != nil {
		dbErr(w, err)
		return
	}
	if len(list) == 0 {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, list[0])
}

func (a *api) queryComponentAssets(ctx context.Context, id int64, f componentFilter) ([]map[string]any, error) {
	if err := checkPage(f.Limit, f.Cursor); err != nil {
		return nil, err
	}
	if f.Vendor != nil {
		if err := checkVendor(f.Vendor); err != nil {
			return nil, err
		}
	}
	var exists bool
	if err := a.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM components WHERE id = $1)`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errf(404, "not found")
	}
	args := append(f.scopeArgs(), id, f.Cursor, f.Limit+1)
	assets, err := rowsJSON(a.db.Query(ctx, `SELECT a.id, a.subdomain, a.alive, a.wildcard, a.customer_hosted,
		r.vendor_id, v.name AS vendor, a.id AS cursor_id
		FROM assets a
		JOIN root_domains r ON r.id = a.root_id
		JOIN vendors v ON v.id = r.vendor_id
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
		)
		  AND ($6::bigint = 0 OR a.id < $6)
		ORDER BY a.id DESC
		LIMIT $7`, args...))
	if err != nil {
		return nil, err
	}
	if err := attachComponentURLs(ctx, a, id, f, assets); err != nil {
		return nil, err
	}
	return assets, nil
}

func attachComponentURLs(ctx context.Context, a *api, componentID int64, f componentFilter, assets []map[string]any) error {
	if len(assets) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(assets))
	idx := map[int64]int{}
	for i, m := range assets {
		id, _ := m["id"].(int64)
		if id == 0 {
			if v, ok := m["id"].(int32); ok {
				id = int64(v)
			}
		}
		ids = append(ids, id)
		idx[id] = i
		m["urls"] = []any{}
	}
	rows, err := a.db.Query(ctx, `SELECT u.asset_id, u.id, u.scheme || '://' || u.host || ':' || u.port::text AS url,
		u.title, uc.version, uc.source
		FROM url_components uc
		JOIN urls u ON u.id = uc.url_id
		WHERE uc.component_id = $1 AND u.asset_id = ANY($2)
		ORDER BY u.asset_id, u.id`, componentID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	type ur struct {
		assetID int64
		row     map[string]any
	}
	var list []ur
	for rows.Next() {
		var assetID, urlID int64
		var url, title, version, source string
		var titlePtr *string
		if err := rows.Scan(&assetID, &urlID, &url, &titlePtr, &version, &source); err != nil {
			return err
		}
		if titlePtr != nil {
			title = *titlePtr
		}
		list = append(list, ur{assetID, map[string]any{"id": urlID, "url": url, "title": title, "version": version, "source": source}})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, it := range list {
		if i, ok := idx[it.assetID]; ok {
			assets[i]["urls"] = append(assets[i]["urls"].([]any), it.row)
		}
	}
	return nil
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
	list, err := a.queryComponentAssets(r.Context(), id, f)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	list, next := cutPage(list, limit)
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
		q = `SELECT DISTINCT a.subdomain AS v
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
