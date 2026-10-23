package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed openapi.yaml
var openapiYAML []byte

// apiConfig：API_BOOTSTRAP_TOKEN（可空；为空且无 api_tokens 时所有 /api 请求 401）、SCOPE_BATCH_SIZE（默认 500，>=1）。
type apiConfig struct {
	BootstrapToken string
	ScopeBatch     int
	MaxRetry       int
}

func apiConfigFromEnv() (apiConfig, error) {
	c := apiConfig{BootstrapToken: os.Getenv("API_BOOTSTRAP_TOKEN"), ScopeBatch: 500}
	if v := os.Getenv("SCOPE_BATCH_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("SCOPE_BATCH_SIZE must be an integer >= 1, got %q", v)
		}
		c.ScopeBatch = n
	}
	return c, nil
}

type api struct {
	db     *pgxpool.Pool
	client *asynq.Client
	cfg    apiConfig
	bg     context.Context // 异步任务用（不随请求取消）
}

// apiRoutes 是全部受保护路由；openapi.yaml 必须与之一致（测试校验）。
func (a *api) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/vendors":                          a.listVendors,
		"POST /api/vendors":                         a.createVendor,
		"PATCH /api/vendors/{id}":                   a.updateVendor,
		"DELETE /api/vendors/{id}":                  a.deleteVendor,
		"GET /api/vendors/{id}/roots":               a.listRoots,
		"POST /api/vendors/{id}/roots":              a.createRoot,
		"DELETE /api/vendors/{id}/roots/{root_id}":  a.deleteRoot,
		"GET /api/vendors/{id}/scopes":              a.listScopes,
		"POST /api/vendors/{id}/scopes":             a.createScope,
		"DELETE /api/vendors/{id}/scopes/{rule_id}": a.deleteScope,
		"POST /api/vendors/{id}/subsidiaries":       a.importSubs,
		"GET /api/vendors/{id}/root-candidates":     a.listCandidates,
		"POST /api/root-candidates/{id}/confirm":    a.confirmCand,
		"POST /api/root-candidates/{id}/ignore":     a.ignoreCand,
		"POST /api/root-candidates/{id}/reassign":   a.reassignCand,
		"GET /api/scope-tasks/{id}":                 a.getScopeTask,
		"GET /api/customer-hosted-rules":            a.listHostedRules,
		"POST /api/customer-hosted-rules":           a.createHostedRule,
		"PATCH /api/customer-hosted-rules/{id}":     a.updateHostedRule,
		"DELETE /api/customer-hosted-rules/{id}":    a.deleteHostedRule,
		"GET /api/customer-hosted-tasks/{id}":       a.getHostedTask,
		"GET /api/assets":                           a.listAssets,
		"GET /api/check-scope":                      a.checkScopeHTTP,
		"GET /api/assets/{id}":                      a.getAsset,
		"GET /api/events":                           a.listEvents,
		"GET /api/events/stats":                     a.eventStats,
		"GET /api/events/values":                    a.eventValues,
		"GET /api/scans":                            a.listScans,
		"POST /api/scans":                           a.createScan,
		"GET /api/scans/{id}":                       a.getScan,
		"GET /api/tokens":                           a.listTokens,
		"POST /api/tokens":                          a.createToken,
		"DELETE /api/tokens/{id}":                   a.deleteToken,
	}
}

// register 把 API 挂到 mux：/api/openapi.yaml 公开，其余需 Bearer token。
func (a *api) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(openapiYAML)
	})
	for pat, h := range a.routes() {
		mux.Handle(pat, a.auth(h))
	}
	mux.Handle("/mcp", a.mcpHandler())
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeErr(w, 404, "not found") })
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func (a *api) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.ContainsRune(r.URL.Path, 0) || strings.ContainsRune(r.URL.RawQuery, 0) || hasNUL([]byte(r.URL.Query().Encode())) ||
			strings.Contains(strings.ToLower(r.URL.RawQuery), "%00") || strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%00") {
			writeErr(w, 400, "NUL character not allowed")
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok = strings.TrimSpace(tok)
		if !ok || tok == "" {
			writeErr(w, 401, "missing bearer token")
			return
		}
		if a.cfg.BootstrapToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.BootstrapToken)) == 1 {
			h(w, r)
			return
		}
		tag, err := a.db.Exec(r.Context(), `UPDATE api_tokens SET last_used_at = now() WHERE token_hash = $1`, hashToken(tok))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if tag.RowsAffected() == 0 {
			writeErr(w, 401, "invalid token")
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// dbErr 把常见数据库错误映射为 HTTP 状态。
func dbErr(w http.ResponseWriter, err error) {
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeErr(w, 404, "not found")
	case errors.As(err, &pe) && pe.Code == "23505":
		writeErr(w, 409, "already exists")
	case errors.As(err, &pe) && pe.Code == "22021": // 非法字符（如 NUL）兜底
		writeErr(w, 400, "invalid character in input")
	case errors.As(err, &pe) && pe.Code == "23503":
		writeErr(w, 404, "referenced resource not found")
	default:
		log.Printf("api: %v", err)
		writeErr(w, 500, "internal error")
	}
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid "+name)
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, "invalid body: "+err.Error())
		return false
	}
	if hasNUL(raw) {
		writeErr(w, 400, "NUL character not allowed")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return false
	}
	if _, err := dec.Token(); err != io.EOF { // 第一个值之后只能是空白
		writeErr(w, 400, "invalid json: trailing data after value")
		return false
	}
	return true
}

// hasNUL：原始 NUL 字节或 JSON 转义 \u0000（Postgres 文本不接受 NUL）。
func hasNUL(b []byte) bool {
	return bytes.IndexByte(b, 0) >= 0 || bytes.Contains(bytes.ToLower(b), []byte(`\u0000`))
}

// rowsJSON 把查询结果转为 []map（列名为键）。
func rowsJSON(rows pgx.Rows, err error) ([]map[string]any, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToMap)
	if out == nil {
		out = []map[string]any{}
	}
	return out, err
}

func (a *api) one(w http.ResponseWriter, r *http.Request, code int, q string, args ...any) {
	list, err := rowsJSON(a.db.Query(r.Context(), q, args...))
	if err != nil {
		dbErr(w, err)
		return
	}
	if len(list) == 0 {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, code, list[0])
}

func (a *api) list(w http.ResponseWriter, r *http.Request, q string, args ...any) {
	list, err := rowsJSON(a.db.Query(r.Context(), q, args...))
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": list})
}

// page 解析 limit（默认 50，最大 500）与 cursor（上一页最后一条 id，按 id 倒序）。
func page(w http.ResponseWriter, r *http.Request) (limit int, cursor int64, ok bool) {
	limit = 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeErr(w, 400, "limit must be 1..500")
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeErr(w, 400, "invalid cursor")
			return 0, 0, false
		}
		cursor = n
	}
	return limit, cursor, true
}

// ---- 厂商 ----

func (a *api) listVendors(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT id, name, company_name, active_enabled, created_at FROM vendors ORDER BY id`)
}

type vendorBody struct {
	Name          *string `json:"name"`
	CompanyName   *string `json:"company_name"`
	ActiveEnabled *bool   `json:"active_enabled"`
}

func (a *api) createVendor(w http.ResponseWriter, r *http.Request) {
	var b vendorBody
	if !decode(w, r, &b) {
		return
	}
	if b.Name == nil || strings.TrimSpace(*b.Name) == "" {
		writeErr(w, 400, "name required")
		return
	}
	a.one(w, r, 201, `INSERT INTO vendors (name, company_name, active_enabled) VALUES ($1, $2, COALESCE($3, false))
		RETURNING id, name, company_name, active_enabled, created_at`, strings.TrimSpace(*b.Name), b.CompanyName, b.ActiveEnabled)
}

func (a *api) updateVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	var b vendorBody
	if !ok || !decode(w, r, &b) {
		return
	}
	if b.Name != nil && strings.TrimSpace(*b.Name) == "" {
		writeErr(w, 400, "name must not be empty")
		return
	}
	a.one(w, r, 200, `UPDATE vendors SET name = COALESCE($2, name), company_name = COALESCE($3, company_name),
		active_enabled = COALESCE($4, active_enabled) WHERE id = $1
		RETURNING id, name, company_name, active_enabled, created_at`, id, b.Name, b.CompanyName, b.ActiveEnabled)
}

// deleteVendor：级联删下属根域/资产/URL/事件/候选/扫描任务/范围规则/子公司/in_scope 任务；
// 有 pending/running 扫描时返回 409。
func (a *api) deleteVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := a.checkNoActiveScans(r.Context(), &id, nil); err != nil {
		writeAnyErr(w, err)
		return
	}
	tag, err := a.db.Exec(r.Context(), `DELETE FROM vendors WHERE id = $1`, id)
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

// ---- 根域 ----

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validDomain：至少两段、每段合法 LDH、总长 <= 253。
func validDomain(d string) bool {
	if len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for _, l := range strings.Split(d, ".") {
		if !labelRe.MatchString(l) {
			return false
		}
	}
	return true
}

// validScopePattern：域名，或 "*." + 域名。
func validScopePattern(p string) bool {
	return validDomain(strings.TrimPrefix(p, "*."))
}

func (a *api) listRoots(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r, "id"); ok {
		a.list(w, r, `SELECT id, vendor_id, domain, created_at FROM root_domains WHERE vendor_id = $1 ORDER BY id`, id)
	}
}

func (a *api) createRoot(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	var b struct {
		Domain string `json:"domain"`
	}
	if !ok || !decode(w, r, &b) {
		return
	}
	dom := normalizeHost(b.Domain)
	if !validDomain(dom) {
		writeErr(w, 400, "invalid domain")
		return
	}
	var owner int64
	if err := a.db.QueryRow(r.Context(), `SELECT vendor_id FROM root_domains WHERE domain = $1`, dom).Scan(&owner); err == nil {
		writeErr(w, 409, fmt.Sprintf("root domain already belongs to vendor %d", owner))
		return
	}
	a.one(w, r, 201, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, $2) RETURNING id, vendor_id, domain, created_at`, id, dom)
}

// ---- 范围规则（改动后异步重算 in_scope，返回 202 + task id）----

// deleteRoot：级联删该根域下资产/URL/事件/扫描任务；有 pending/running 扫描时 409。
func (a *api) deleteRoot(w http.ResponseWriter, r *http.Request) {
	vendorID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rootID, ok := pathID(w, r, "root_id")
	if !ok {
		return
	}
	var owner int64
	err := a.db.QueryRow(r.Context(), `SELECT vendor_id FROM root_domains WHERE id = $1`, rootID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "not found")
		return
	} else if err != nil {
		dbErr(w, err)
		return
	}
	if owner != vendorID {
		writeErr(w, 404, "not found")
		return
	}
	if err := a.checkNoActiveScans(r.Context(), nil, &rootID); err != nil {
		writeAnyErr(w, err)
		return
	}
	tag, err := a.db.Exec(r.Context(), `DELETE FROM root_domains WHERE id = $1`, rootID)
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

func (a *api) listScopes(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r, "id"); ok {
		a.list(w, r, `SELECT id, vendor_id, kind, pattern, created_at FROM scope_rules WHERE vendor_id = $1 ORDER BY id`, id)
	}
}

func (a *api) createScope(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	var b struct {
		Kind    string `json:"kind"`
		Pattern string `json:"pattern"`
	}
	if !ok || !decode(w, r, &b) {
		return
	}
	pat := normalizeHost(b.Pattern)
	if b.Kind != "include" && b.Kind != "exclude" {
		writeErr(w, 400, "kind must be include or exclude")
		return
	}
	if !validScopePattern(pat) {
		writeErr(w, 400, "invalid pattern: use domain or *.domain")
		return
	}
	var ruleID int64
	if err := a.db.QueryRow(r.Context(), `INSERT INTO scope_rules (vendor_id, kind, pattern) VALUES ($1, $2, $3) RETURNING id`,
		id, b.Kind, pat).Scan(&ruleID); err != nil {
		dbErr(w, err)
		return
	}
	a.scopeChanged(w, r, id, map[string]any{"rule_id": ruleID})
}

func (a *api) deleteScope(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ruleID, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	tag, err := a.db.Exec(r.Context(), `DELETE FROM scope_rules WHERE id = $1 AND vendor_id = $2`, ruleID, id)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "not found")
		return
	}
	a.scopeChanged(w, r, id, map[string]any{"rule_id": ruleID})
}

func (a *api) scopeChanged(w http.ResponseWriter, r *http.Request, vendorID int64, extra map[string]any) {
	taskID, err := a.startScopeTask(r.Context(), vendorID)
	if err != nil {
		dbErr(w, err)
		return
	}
	extra["task_id"] = taskID
	writeJSON(w, 202, extra)
}

// startScopeTask 建 scope_tasks 记录并在后台分批重算该厂商资产的 in_scope。
func (a *api) startScopeTask(ctx context.Context, vendorID int64) (int64, error) {
	var id int64
	if err := a.db.QueryRow(ctx, `INSERT INTO scope_tasks (vendor_id) VALUES ($1) RETURNING id`, vendorID).Scan(&id); err != nil {
		return 0, err
	}
	go a.runScopeTask(id, vendorID)
	return id, nil
}

func (a *api) runScopeTask(id, vendorID int64) {
	if err := recalcInScope(a.bg, a.db, id, vendorID, a.cfg.ScopeBatch); err != nil {
		log.Printf("scope task %d: %v", id, err)
		a.db.Exec(a.bg, `UPDATE scope_tasks SET status = 'failed', error = $2, finished_at = now() WHERE id = $1`, id, err.Error())
	}
}

// resumeScopeTasks 启动时重跑上次进程退出时未完成（pending/running）的重算任务，返回重跑个数。
func (a *api) resumeScopeTasks(ctx context.Context) (int, error) {
	rows, err := a.db.Query(ctx, `SELECT id, vendor_id FROM scope_tasks WHERE status IN ('pending', 'running') ORDER BY id`)
	if err != nil {
		return 0, err
	}
	type task struct{ id, vendor int64 }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (task, error) {
		var t task
		return t, r.Scan(&t.id, &t.vendor)
	})
	if err != nil {
		return 0, err
	}
	for _, t := range list {
		go a.runScopeTask(t.id, t.vendor)
	}
	return len(list), nil
}

// recalcInScope 按 id 分批（每批一个短语句，不长时间锁表）重算 in_scope，只更新有变化的行，进度写 scope_tasks。
// 规则在每批开始时重新读取，过程中规则再变也以最新规则为准（新任务会再跑一遍）。
func recalcInScope(ctx context.Context, d *pgxpool.Pool, taskID, vendorID int64, batch int) error {
	// 从头重算并清零计数：任务可重复执行（重启后续跑同一任务结果一致）
	if _, err := d.Exec(ctx, `UPDATE scope_tasks SET status = 'running', processed = 0, changed = 0, error = NULL WHERE id = $1`, taskID); err != nil {
		return err
	}
	var cursor int64
	for {
		var rules []ScopeRule
		rows, err := d.Query(ctx, `SELECT kind, pattern FROM scope_rules WHERE vendor_id = $1`, vendorID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s ScopeRule
			if err := rows.Scan(&s.Kind, &s.Pattern); err != nil {
				return err
			}
			rules = append(rules, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = d.Query(ctx, `SELECT a.id, a.subdomain FROM assets a JOIN root_domains r ON r.id = a.root_id
			WHERE r.vendor_id = $1 AND a.id > $2 ORDER BY a.id LIMIT $3`, vendorID, cursor, batch)
		if err != nil {
			return err
		}
		var ids []int64
		var vals []bool
		for rows.Next() {
			var id int64
			var host string
			if err := rows.Scan(&id, &host); err != nil {
				return err
			}
			ids, vals = append(ids, id), append(vals, InScope(host, rules))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			_, err := d.Exec(ctx, `UPDATE scope_tasks SET status = 'done', finished_at = now() WHERE id = $1`, taskID)
			return err
		}
		tag, err := d.Exec(ctx, `UPDATE assets SET in_scope = v.s FROM unnest($1::bigint[], $2::bool[]) v(id, s)
			WHERE assets.id = v.id AND assets.in_scope IS DISTINCT FROM v.s`, ids, vals)
		if err != nil {
			return err
		}
		if _, err := d.Exec(ctx, `UPDATE scope_tasks SET processed = processed + $2, changed = changed + $3 WHERE id = $1`,
			taskID, len(ids), tag.RowsAffected()); err != nil {
			return err
		}
		cursor = ids[len(ids)-1]
	}
}

func (a *api) getScopeTask(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r, "id"); ok {
		a.one(w, r, 200, `SELECT id, vendor_id, status, processed, changed, error, created_at, finished_at FROM scope_tasks WHERE id = $1`, id)
	}
}

// ---- 子公司导入、待确认根域 ----

func (a *api) importSubs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var exists bool
	a.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM vendors WHERE id = $1)`, id).Scan(&exists)
	if !exists {
		writeErr(w, 404, "vendor not found")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || bytes.IndexByte(raw, 0) >= 0 {
		writeErr(w, 400, "invalid body")
		return
	}
	n, err := importSubsidiaries(r.Context(), a.db, id, bytes.NewReader(raw))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int{"imported": n})
}

func (a *api) listCandidates(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	if status != "pending" && status != "confirmed" && status != "ignored" && status != "all" {
		writeErr(w, 400, "status must be pending|confirmed|ignored|all")
		return
	}
	a.list(w, r, `SELECT id, vendor_id, domain, sources, evidence, status, created_at, updated_at FROM root_candidates
		WHERE vendor_id = $1 AND ($2::text = 'all' OR status = $2) ORDER BY id`, id, status)
}

// candidateOp 执行确认/忽略/改归属：不存在 404，非 pending 409，域名已被占 409。
func (a *api) candidateOp(w http.ResponseWriter, r *http.Request, op func(ctx context.Context, id int64) error) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var status string
	if err := a.db.QueryRow(r.Context(), `SELECT status FROM root_candidates WHERE id = $1`, id).Scan(&status); err != nil {
		dbErr(w, err)
		return
	}
	if status != "pending" {
		writeErr(w, 409, "candidate is "+status)
		return
	}
	if err := op(r.Context(), id); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			dbErr(w, err)
		} else {
			writeErr(w, 409, err.Error())
		}
		return
	}
	a.one(w, r, 200, `SELECT id, vendor_id, domain, sources, evidence, status, created_at, updated_at FROM root_candidates WHERE id = $1`, id)
}

func (a *api) confirmCand(w http.ResponseWriter, r *http.Request) {
	a.candidateOp(w, r, func(ctx context.Context, id int64) error { return confirmCandidate(ctx, a.db, id) })
}

func (a *api) ignoreCand(w http.ResponseWriter, r *http.Request) {
	a.candidateOp(w, r, func(ctx context.Context, id int64) error { return ignoreCandidate(ctx, a.db, id) })
}

func (a *api) reassignCand(w http.ResponseWriter, r *http.Request) {
	var b struct {
		VendorID int64 `json:"vendor_id"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.VendorID <= 0 {
		writeErr(w, 400, "vendor_id required")
		return
	}
	a.candidateOp(w, r, func(ctx context.Context, id int64) error { return reassignCandidate(ctx, a.db, id, b.VendorID) })
}

// ---- 资产与事件 ----

// boolParam 解析 true/false；空返回 nil。
func boolParam(w http.ResponseWriter, r *http.Request, name string) (*bool, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		writeErr(w, 400, "invalid "+name)
		return nil, false
	}
	return &b, true
}

func timeParam(w http.ResponseWriter, r *http.Request, name string) (*time.Time, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		writeErr(w, 400, name+" must be RFC3339")
		return nil, false
	}
	return &t, true
}

func int64Param(w http.ResponseWriter, r *http.Request, name string) (*int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		writeErr(w, 400, "invalid "+name)
		return nil, false
	}
	return &n, true
}

const assetCols = `a.id, a.subdomain, a.root_id, r.domain AS root_domain, r.vendor_id, a.cnames, a.cdn, a.resolved,
	a.wildcard, a.customer_hosted, a.in_scope, a.sources, a.first_seen, a.last_seen, a.alive, a.last_alive_at`

// listAssets：vendor / in_scope / q（子域名或 URL 标题子串）/ new_since（资产 new 事件时间）/ include_wildcard（默认过滤）。
// httpError 带状态码的业务错误（REST 映射为状态码，MCP 映射为工具错误）。
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func errf(code int, format string, args ...any) error {
	return &httpError{code, fmt.Sprintf(format, args...)}
}

// writeAnyErr：httpError 按其状态码，其余按数据库错误映射。
func writeAnyErr(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeErr(w, he.code, he.msg)
		return
	}
	dbErr(w, err)
}

// assetFilter / eventFilter 是 REST 与 MCP 共用的查询条件（T13 复用）。
type assetFilter struct {
	Vendor          *int64
	InScope         *bool
	IncludeWildcard bool
	Q               string
	NewSince        *time.Time
	Limit           int
	Cursor          int64
	Alive           string // alive / dead / unknown；空=不筛
	IncludeHosted   bool   // include_customer_hosted：默认隐藏客户托管资产（T24）
}

// checkVendor：传了 vendor 必须 >= 1（REST 与 MCP 共用）。
func checkVendor(v *int64) error {
	if v != nil && *v < 1 {
		return errf(400, "invalid vendor")
	}
	return nil
}

func checkPage(limit int, cursor int64) error {
	if limit < 1 || limit > 500 {
		return errf(400, "limit must be 1..500")
	}
	if cursor < 0 {
		return errf(400, "invalid cursor")
	}
	return nil
}

// cutPage：查询多取 1 行（LIMIT limit+1）；有第 limit+1 行才返回 next_cursor，并截掉多取的那行。
// 总数恰为页大小整数倍时最后一页不再返回 next_cursor。
func cutPage(list []map[string]any, limit int) ([]map[string]any, any) {
	if limit > 0 && len(list) > limit {
		list = list[:limit]
		return list, strconv.FormatInt(list[limit-1]["id"].(int64), 10)
	}
	return list, nil
}

// queryAssets：vendor / in_scope / q（子域名或 URL 标题子串）/ new_since（资产 new 事件时间）/ include_wildcard（默认过滤）。
func (a *api) queryAssets(ctx context.Context, f assetFilter) ([]map[string]any, error) {
	if err := checkVendor(f.Vendor); err != nil {
		return nil, err
	}
	if err := checkPage(f.Limit, f.Cursor); err != nil {
		return nil, err
	}
	if strings.ContainsRune(f.Q, 0) {
		return nil, errf(400, "NUL character not allowed")
	}
	if f.Alive != "" && f.Alive != "alive" && f.Alive != "dead" && f.Alive != "unknown" {
		return nil, errf(400, "alive must be alive|dead|unknown")
	}
	q := strings.ToLower(strings.TrimSpace(f.Q))
	return rowsJSON(a.db.Query(ctx, `SELECT `+assetCols+` FROM assets a JOIN root_domains r ON r.id = a.root_id
		WHERE ($1::bigint = 0 OR a.id < $1)
		  AND ($2::bigint IS NULL OR r.vendor_id = $2)
		  AND ($3::bool IS NULL OR a.in_scope IS NOT DISTINCT FROM $3)
		  AND ($4::bool OR NOT a.wildcard)
		  AND ($5::text = '' OR strpos(a.subdomain, $5) > 0
		       OR EXISTS (SELECT 1 FROM urls u WHERE u.asset_id = a.id AND strpos(lower(u.title), $5) > 0))
		  AND ($6::timestamptz IS NULL OR EXISTS (SELECT 1 FROM events e WHERE e.asset_id = a.id AND e.url_id IS NULL
		       AND e.type = 'new' AND e.created_at >= $6))
		  AND ($8::text = '' OR a.alive = $8)
		  AND ($9::bool OR NOT a.customer_hosted)
		ORDER BY a.id DESC LIMIT $7`, f.Cursor, f.Vendor, f.InScope, f.IncludeWildcard, q, f.NewSince, f.Limit+1, f.Alive, f.IncludeHosted))
}

func (a *api) listAssets(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := page(w, r)
	if !ok {
		return
	}
	vendor, ok1 := int64Param(w, r, "vendor")
	inScope, ok2 := boolParam(w, r, "in_scope")
	if !ok1 || !ok2 {
		return
	}
	wild, ok3 := boolParam(w, r, "include_wildcard")
	newSince, ok4 := timeParam(w, r, "new_since")
	hosted, ok5 := boolParam(w, r, "include_customer_hosted")
	if !ok3 || !ok4 || !ok5 {
		return
	}
	list, err := a.queryAssets(r.Context(), assetFilter{vendor, inScope, wild != nil && *wild, r.URL.Query().Get("q"), newSince, limit, cursor, r.URL.Query().Get("alive"), hosted != nil && *hosted})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	list, next := cutPage(list, limit)
	writeJSON(w, 200, map[string]any{"items": list, "next_cursor": next})
}

func (a *api) getAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	list, err := rowsJSON(a.db.Query(r.Context(), `SELECT `+assetCols+` FROM assets a JOIN root_domains r ON r.id = a.root_id WHERE a.id = $1`, id))
	if err != nil {
		dbErr(w, err)
		return
	}
	if len(list) == 0 {
		writeErr(w, 404, "not found")
		return
	}
	urls, err := rowsJSON(a.db.Query(r.Context(), `SELECT id, scheme, host, port, status_code, title, fingerprint_hash, redirect_to,
		tech, first_seen, last_seen FROM urls WHERE asset_id = $1 ORDER BY id`, id))
	if err != nil {
		dbErr(w, err)
		return
	}
	list[0]["urls"] = urls
	writeJSON(w, 200, list[0])
}

type eventFilter struct {
	Level, Type     string
	Since           *time.Time
	AssetID, Vendor *int64
	IncludeWildcard bool
	Limit           int
	Cursor          int64
	Until           *time.Time // created_at < until（T20 时间窗）
	AfterID         int64      // id > after_id（T20 轮询只取新增）
	IncludeHosted   bool       // include_customer_hosted：默认隐藏客户托管资产的事件（T24）
}

// checkEventFilter：level/type/vendor/时间窗校验（列表、统计、整列复制共用）。
func checkEventFilter(f *eventFilter) error {
	if err := checkVendor(f.Vendor); err != nil {
		return err
	}
	if f.Level == "" {
		f.Level = "asset"
	}
	if f.Level != "asset" && f.Level != "url" && f.Level != "all" {
		return errf(400, "level must be asset|url|all")
	}
	if f.Type == "" {
		f.Type = "new,recovered" // 默认口径（SPEC 10-21）：只有显式传 changed 才含 changed
	}
	{
		for _, t := range strings.Split(f.Type, ",") { // 可逗号分隔多选，如 new,recovered
			if t != "new" && t != "changed" && t != "recovered" {
				return errf(400, "type must be new|changed|recovered")
			}
		}
	}
	if f.Since != nil && f.Until != nil && !f.Until.After(*f.Since) {
		return errf(400, "until must be after since")
	}
	if f.AfterID < 0 {
		return errf(400, "invalid after_id")
	}
	return nil
}

// eventWhere：事件筛选条件，参数 $1..$10 依次为 cursor/level/type/since/asset_id/vendor/include_wildcard/until/after_id/include_customer_hosted。
// T28：type=changed 且 changes 为空（或去掉非实质键 ips 后为空）的行一律不返回、不计入（历史「只改过 IP」的残留；显式 type=changed 也不返回）。
const eventWhere = `FROM events e JOIN assets a ON a.id = e.asset_id JOIN root_domains r ON r.id = a.root_id JOIN vendors v ON v.id = r.vendor_id LEFT JOIN urls u ON u.id = e.url_id
		WHERE ($1::bigint = 0 OR e.id < $1)
		  AND ($2::text = 'all' OR ($2 = 'asset') = (e.url_id IS NULL))
		  AND ($3::text = '' OR e.type = ANY(string_to_array($3, ',')))
		  AND ($4::timestamptz IS NULL OR e.created_at >= $4)
		  AND ($5::bigint IS NULL OR e.asset_id = $5)
		  AND ($6::bigint IS NULL OR r.vendor_id = $6)
		  AND ($7::bool OR NOT a.wildcard)
		  AND ($8::timestamptz IS NULL OR e.created_at < $8)
		  AND ($9::bigint = 0 OR e.id > $9)
		  AND ($10::bool OR NOT a.customer_hosted)
		  AND NOT (e.type = 'changed' AND COALESCE(e.detail->'changes', '{}'::jsonb) - 'ips' = '{}'::jsonb)`

func (f eventFilter) args() []any {
	return []any{f.Cursor, f.Level, f.Type, f.Since, f.AssetID, f.Vendor, f.IncludeWildcard, f.Until, f.AfterID, f.IncludeHosted}
}

// queryEvents：level=asset（默认，资产级）|url|all；type 不传默认 new,recovered（显式传 changed 才含 changed）；资产级 new 事件嵌套 url_events。
// since/until 为时间窗 [since, until)；after_id 只取更新的事件（轮询）。
func (a *api) queryEvents(ctx context.Context, f eventFilter) ([]map[string]any, error) {
	if err := checkPage(f.Limit, f.Cursor); err != nil {
		return nil, err
	}
	if err := checkEventFilter(&f); err != nil {
		return nil, err
	}
	list, err := rowsJSON(a.db.Query(ctx, `SELECT e.id, e.type, CASE WHEN e.url_id IS NULL THEN 'asset' ELSE 'url' END AS level,
		e.asset_id, a.subdomain, a.customer_hosted, r.vendor_id, e.url_id, u.scheme || '://' || u.host || ':' || u.port AS url, e.detail, e.created_at
		`+eventWhere+` ORDER BY e.id DESC LIMIT $11`, append(f.args(), f.Limit+1)...))
	if err == nil {
		err = a.nestURLEvents(ctx, list)
	}
	return list, err
}

// eventFilterFromQuery：列表/统计/整列复制共用的查询参数解析（不含分页）。
func eventFilterFromQuery(w http.ResponseWriter, r *http.Request) (eventFilter, bool) {
	qs := r.URL.Query()
	since, ok1 := timeParam(w, r, "since")
	if !ok1 {
		return eventFilter{}, false
	}
	until, ok2 := timeParam(w, r, "until")
	if !ok2 {
		return eventFilter{}, false
	}
	assetID, ok3 := int64Param(w, r, "asset_id")
	if !ok3 {
		return eventFilter{}, false
	}
	vendor, ok4 := int64Param(w, r, "vendor")
	if !ok4 {
		return eventFilter{}, false
	}
	wild, ok5 := boolParam(w, r, "include_wildcard")
	if !ok5 {
		return eventFilter{}, false
	}
	hosted, ok6 := boolParam(w, r, "include_customer_hosted")
	if !ok6 {
		return eventFilter{}, false
	}
	var after int64
	if v := qs.Get("after_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, 400, "invalid after_id")
			return eventFilter{}, false
		}
		after = n
	}
	return eventFilter{Level: qs.Get("level"), Type: qs.Get("type"), Since: since, Until: until, AssetID: assetID,
		Vendor: vendor, IncludeWildcard: wild != nil && *wild, AfterID: after, IncludeHosted: hosted != nil && *hosted}, true
}

func (a *api) listEvents(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := page(w, r)
	if !ok {
		return
	}
	f, ok := eventFilterFromQuery(w, r)
	if !ok {
		return
	}
	f.Limit, f.Cursor = limit, cursor
	list, err := a.queryEvents(r.Context(), f)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	list, next := cutPage(list, limit)
	writeJSON(w, 200, map[string]any{"items": list, "next_cursor": next})
}

// eventStats：时间窗内按厂商统计事件数（全量，服务端聚合；不受分页影响）。
func (a *api) eventStats(w http.ResponseWriter, r *http.Request) {
	f, ok := eventFilterFromQuery(w, r)
	if !ok {
		return
	}
	if err := checkEventFilter(&f); err != nil {
		writeAnyErr(w, err)
		return
	}
	list, err := rowsJSON(a.db.Query(r.Context(), `SELECT r.vendor_id, v.name AS vendor, count(*) AS count,
		count(*) FILTER (WHERE e.type = 'new') AS new, count(*) FILTER (WHERE e.type = 'recovered') AS recovered,
		count(*) FILTER (WHERE e.type = 'changed') AS changed
		`+eventWhere+` GROUP BY r.vendor_id, v.name ORDER BY count(*) DESC, r.vendor_id`,
		f.args()...))
	if err != nil {
		dbErr(w, err)
		return
	}
	total := int64(0)
	// 各 type 分别计数（未选中的 type 为 0），new+recovered+changed == count
	totals := map[string]int64{"new": 0, "recovered": 0, "changed": 0}
	for _, m := range list {
		total += m["count"].(int64)
		for k := range totals {
			totals[k] += m[k].(int64)
		}
	}
	writeJSON(w, 200, map[string]any{"items": list, "total": total, "totals": totals})
}

// eventValuesMax：整列复制的上限，防止一次返回过大。
const eventValuesMax = 100000

// eventValues：整列复制（field=subdomain|url），时间窗内全部匹配事件，去重排序，一行一个。
// field=url 且 level=asset 时，取这些资产当前的 URL（资产级事件本身无 URL；恢复的资产窗口内没有 URL 级事件）。
func (a *api) eventValues(w http.ResponseWriter, r *http.Request) {
	f, ok := eventFilterFromQuery(w, r)
	if !ok {
		return
	}
	if err := checkEventFilter(&f); err != nil {
		writeAnyErr(w, err)
		return
	}
	field := r.URL.Query().Get("field")
	var q string
	switch {
	case field == "subdomain":
		q = `SELECT DISTINCT a.subdomain AS v ` + eventWhere + ` ORDER BY 1 LIMIT $11`
	case field == "url" && f.Level == "asset":
		// 资产维度：取窗口内事件对应资产的当前 URL（恢复的资产窗口内没有 URL 级事件，不能按 URL 事件取）
		q = `SELECT DISTINCT uu.scheme || '://' || uu.host || ':' || uu.port AS v
			FROM urls uu WHERE uu.asset_id IN (SELECT e.asset_id ` + eventWhere + `)
			ORDER BY 1 LIMIT $11`
	case field == "url":
		q = `SELECT DISTINCT u.scheme || '://' || u.host || ':' || u.port AS v ` + eventWhere + ` AND u.id IS NOT NULL ORDER BY 1 LIMIT $11`
	default:
		writeErr(w, 400, "field must be subdomain|url")
		return
	}
	rows, err := a.db.Query(r.Context(), q, append(f.args(), eventValuesMax+1)...)
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
	truncated := len(out) > eventValuesMax
	if truncated {
		out = out[:eventValuesMax]
	}
	writeJSON(w, 200, map[string]any{"field": field, "items": out, "count": len(out), "truncated": truncated})
}

// ---- 扫描 ----

// expandActiveIdx：同厂商进行中 expand 的部分唯一索引（迁移 015）。
const expandActiveIdx = "scan_tasks_expand_active_uidx"

// triggerExpand：同一厂商已有 pending/running/retrying 的 expand（任意根域）时返回 409。
// 原子性由部分唯一索引保证：INSERT 撞约束即 409（无锁、无「先查后插」竞态）。
// INSERT 与回填 root_scan_id 在同一事务；commit 后才投递，投递失败由 deliverScan 用不可取消的 ctx 标 failed。
func (a *api) triggerExpand(ctx context.Context, vendorID, rootID int64) (int64, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	id, err := insertScan(ctx, tx, rootID, "expand", 0)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == expandActiveIdx {
		tx.Rollback(context.WithoutCancel(ctx)) // 先归还连接，再查进行中的任务（避免小池下一请求占两个连接）
		var running int64
		a.db.QueryRow(context.WithoutCancel(ctx), `SELECT id FROM scan_tasks WHERE vendor_id = $1 AND kind = 'expand'
			AND status IN ('pending', 'running', 'retrying') ORDER BY id LIMIT 1`, vendorID).Scan(&running)
		return 0, errf(409, "expand already in progress for this vendor (scan %d)", running)
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, deliverScan(ctx, a.db, a.client, id, "expand", nil, a.cfg.MaxRetry)
}

// manualKinds 是可手动触发的扫描类型（dns/http 是链路内部步骤，不对外）。
var manualKinds = []string{"passive", "active", "expand"}

// triggerScan 手动触发：kind 只允许 passive/active/expand；active 需厂商 active_enabled。REST 与 MCP 共用。
func (a *api) triggerScan(ctx context.Context, rootID int64, kind string) (map[string]any, error) {
	if !slices.Contains(manualKinds, kind) {
		return nil, errf(400, "kind must be one of %s", strings.Join(manualKinds, "|"))
	}
	var active bool
	var vendorID int64
	if err := a.db.QueryRow(ctx, `SELECT v.active_enabled, v.id FROM root_domains r JOIN vendors v ON v.id = r.vendor_id WHERE r.id = $1`,
		rootID).Scan(&active, &vendorID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errf(404, "root not found")
		}
		return nil, err
	}
	if kind == "active" && !active {
		return nil, errf(409, "active discovery disabled for this vendor")
	}
	var id int64
	var err error
	if kind == "expand" {
		id, err = a.triggerExpand(ctx, vendorID, rootID)
	} else {
		id, err = TriggerScan(ctx, a.db, a.client, rootID, kind, a.cfg.MaxRetry)
	}
	if err != nil {
		return nil, err
	}
	list, err := rowsJSON(a.db.Query(ctx, scanSelect+` WHERE id = $1`, id))
	if err != nil || len(list) == 0 {
		return nil, fmt.Errorf("scan %d: %v", id, err)
	}
	return list[0], nil
}

func (a *api) createScan(w http.ResponseWriter, r *http.Request) {
	var b struct {
		RootID int64  `json:"root_id"`
		Kind   string `json:"kind"`
	}
	if !decode(w, r, &b) {
		return
	}
	s, err := a.triggerScan(r.Context(), b.RootID, b.Kind)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, 202, s)
}

const scanSelect = `SELECT id, vendor_id, root_id, kind, status, error, result, COALESCE((result->>'degraded')::boolean, false) AS degraded, result->>'degraded_reason' AS degraded_reason, parent_id, root_scan_id, created_at, started_at, finished_at FROM scan_tasks`

var scanStatuses = []string{"pending", "running", "retrying", "done", "failed"}

// checkNoActiveScans：有 pending/running 扫描时返回 409（删除厂商/根域前调用）。
func (a *api) checkNoActiveScans(ctx context.Context, vendorID, rootID *int64) error {
	var n int
	err := a.db.QueryRow(ctx, `SELECT count(*) FROM scan_tasks
		WHERE status IN ('pending', 'running')
		  AND ($1::bigint IS NULL OR vendor_id = $1)
		  AND ($2::bigint IS NULL OR root_id = $2)`, vendorID, rootID).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return errf(409, "vendor/root has pending or running scans")
	}
	return nil
}

// listScans：按 vendor/root/status/kind/degraded 过滤，limit/cursor 校验复用 checkPage。
func (a *api) listScans(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := page(w, r)
	if !ok {
		return
	}
	vendor, ok1 := int64Param(w, r, "vendor")
	root, ok2 := int64Param(w, r, "root")
	degraded, ok3 := boolParam(w, r, "degraded")
	if !ok1 || !ok2 || !ok3 {
		return
	}
	if err := checkVendor(vendor); err != nil {
		writeAnyErr(w, err)
		return
	}
	if err := checkPage(limit, cursor); err != nil {
		writeAnyErr(w, err)
		return
	}
	status, kind := r.URL.Query().Get("status"), r.URL.Query().Get("kind")
	if status != "" && !slices.Contains(scanStatuses, status) {
		writeErr(w, 400, "status must be one of "+strings.Join(scanStatuses, "|"))
		return
	}
	if kind != "" && !slices.Contains(scanKinds, kind) {
		writeErr(w, 400, "kind must be one of "+strings.Join(scanKinds, "|"))
		return
	}
	var deg *bool
	if degraded != nil {
		deg = degraded
	}
	list, err := rowsJSON(a.db.Query(r.Context(), scanSelect+`
		WHERE ($1::bigint = 0 OR id < $1)
		  AND ($2::bigint IS NULL OR vendor_id = $2)
		  AND ($3::bigint IS NULL OR root_id = $3)
		  AND ($4::text = '' OR status = $4)
		  AND ($5::text = '' OR kind = $5)
		  AND ($6::bool IS NULL OR COALESCE((result->>'degraded')::boolean, false) = $6)
		ORDER BY id DESC LIMIT $7`, cursor, vendor, root, status, kind, deg, limit+1))
	if err != nil {
		dbErr(w, err)
		return
	}
	list, next := cutPage(list, limit)
	writeJSON(w, 200, map[string]any{"items": list, "next_cursor": next})
}

func (a *api) getScan(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r, "id"); ok {
		a.one(w, r, 200, scanSelect+` WHERE id = $1`, id)
	}
}

// ---- 范围检查（REST GET /api/check-scope 与 MCP check_scope 共用 checkScope）----

// checkScope：主机名属于哪个厂商/根域（最长匹配）、是否在范围内、命中哪些规则；不要求已是资产。
func (a *api) checkScope(ctx context.Context, host string) (map[string]any, error) {
	h := normalizeHost(host)
	if strings.ContainsRune(h, 0) {
		return nil, errf(400, "NUL character not allowed")
	}
	if !validDomain(h) {
		return nil, errf(400, "invalid host")
	}
	out := map[string]any{"host": h, "in_scope": false, "vendor_id": nil, "vendor": nil, "root_domain": nil, "matched_rules": []map[string]any{}, "asset_id": nil}
	var vendorID int64
	var vendor, root string
	err := a.db.QueryRow(ctx, `SELECT v.id, v.name, r.domain FROM root_domains r JOIN vendors v ON v.id = r.vendor_id
		WHERE r.domain = $1 OR right($1, length(r.domain) + 1) = '.' || r.domain ORDER BY length(r.domain) DESC LIMIT 1`, h).Scan(&vendorID, &vendor, &root)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	out["vendor_id"], out["vendor"], out["root_domain"] = vendorID, vendor, root
	rows, err := a.db.Query(ctx, `SELECT id, kind, pattern FROM scope_rules WHERE vendor_id = $1 ORDER BY id`, vendorID)
	if err != nil {
		return nil, err
	}
	var rules []ScopeRule
	var matched []map[string]any
	for rows.Next() {
		var id int64
		var s ScopeRule
		if err := rows.Scan(&id, &s.Kind, &s.Pattern); err != nil {
			return nil, err
		}
		rules = append(rules, s)
		if matchPattern(h, s.Pattern) {
			matched = append(matched, map[string]any{"id": id, "kind": s.Kind, "pattern": s.Pattern})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if matched != nil {
		out["matched_rules"] = matched
	}
	out["in_scope"] = InScope(h, rules)
	var assetID int64
	if a.db.QueryRow(ctx, `SELECT id FROM assets WHERE subdomain = $1`, h).Scan(&assetID) == nil {
		out["asset_id"] = assetID
	}
	return out, nil
}

// ---- token ----

func (a *api) listTokens(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT id, name, created_at, last_used_at FROM api_tokens ORDER BY id`)
}

// createToken 生成随机 token，明文只在响应中返回一次，库里只存 sha256。
func (a *api) createToken(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.Name) == "" {
		writeErr(w, 400, "name required")
		return
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	tok := "sam_" + hex.EncodeToString(raw)
	var id int64
	var created time.Time
	if err := a.db.QueryRow(r.Context(), `INSERT INTO api_tokens (name, token_hash) VALUES ($1, $2) RETURNING id, created_at`,
		strings.TrimSpace(b.Name), hashToken(tok)).Scan(&id, &created); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "name": strings.TrimSpace(b.Name), "token": tok, "created_at": created})
}

func (a *api) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	tag, err := a.db.Exec(r.Context(), `DELETE FROM api_tokens WHERE id = $1`, id)
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

// nestURLEvents 给资产级 new 事件挂上该资产的 URL 级 new 事件（url_events 数组）。
func (a *api) nestURLEvents(ctx context.Context, list []map[string]any) error {
	var ids []int64
	for _, m := range list {
		if m["level"] == "asset" && m["type"] == "new" {
			m["url_events"] = []map[string]any{}
			ids = append(ids, m["asset_id"].(int64))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sub, err := rowsJSON(a.db.Query(ctx, `SELECT e.id, e.asset_id, e.url_id, u.scheme || '://' || u.host || ':' || u.port AS url,
		u.status_code, u.title, e.detail, e.created_at
		FROM events e JOIN urls u ON u.id = e.url_id WHERE e.asset_id = ANY($1) AND e.type = 'new' ORDER BY e.id`, ids))
	if err != nil {
		return err
	}
	by := map[int64][]map[string]any{}
	for _, s := range sub {
		by[s["asset_id"].(int64)] = append(by[s["asset_id"].(int64)], s)
	}
	for _, m := range list {
		if v, ok := m["url_events"]; ok && v != nil {
			if s := by[m["asset_id"].(int64)]; s != nil {
				m["url_events"] = s
			}
		}
	}
	return nil
}

func (a *api) checkScopeHTTP(w http.ResponseWriter, r *http.Request) {
	out, err := a.checkScope(r.Context(), r.URL.Query().Get("host"))
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}
