package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ICP/子公司扩展（T11）：找新根域放入 root_candidates（待确认），不自动加入扫描；确认后才写 root_domains。
// 链路：厂商主体名 → ICP；子公司（持股 >= 50%，只一层）→ ICP；已有根域 → ICP 反查主体 → ICP。只手动触发（kind expand）。

// ICPRecord 一条备案记录。
type ICPRecord struct {
	Domain  string `json:"domain"`
	Company string `json:"company"`
	License string `json:"license"` // 备案号
}

// ICPSource 可插拔 ICP 源。
type ICPSource interface {
	ByCompany(ctx context.Context, company string) ([]ICPRecord, error)
	ByDomain(ctx context.Context, domain string) ([]ICPRecord, error)
}

// Subsidiary 子公司；Ratio 为持股百分比。
type Subsidiary struct {
	Name  string
	Ratio float64
}

// SubsidiarySource 可插拔子公司源。
type SubsidiarySource interface {
	Subsidiaries(ctx context.Context, vendorID int64, company string) ([]Subsidiary, error)
}

// httpICP 通用 HTTP JSON 适配器：GET {ICP_API_URL}?company=<主体名> 或 ?domain=<域名>，
// 有 ICP_API_KEY 时带 Authorization: Bearer <key>；响应 {"records":[{"domain","company","license"}]}。
type httpICP struct {
	base, key string
	client    *http.Client
}

func icpSourceFromEnv() (ICPSource, error) {
	base := os.Getenv("ICP_API_URL")
	if base == "" {
		return nil, nil // 未配置：跳过 ICP 扩展
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ICP_API_URL: invalid %q", base)
	}
	return &httpICP{base: base, key: os.Getenv("ICP_API_KEY"), client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (h *httpICP) query(ctx context.Context, param, value string) ([]ICPRecord, error) {
	u, _ := url.Parse(h.base)
	q := u.Query()
	q.Set(param, value)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	if h.key != "" {
		req.Header.Set("Authorization", "Bearer "+h.key)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("icp %s=%s: HTTP %d", param, value, resp.StatusCode)
	}
	var body struct {
		Records []ICPRecord `json:"records"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("icp %s=%s: %w", param, value, err)
	}
	return body.Records, nil
}

func (h *httpICP) ByCompany(ctx context.Context, company string) ([]ICPRecord, error) {
	return h.query(ctx, "company", company)
}

func (h *httpICP) ByDomain(ctx context.Context, domain string) ([]ICPRecord, error) {
	return h.query(ctx, "domain", domain)
}

// dbSubsidiaries 从 vendor_subsidiaries 表读子公司（手工导入的名单）。
type dbSubsidiaries struct{ d *pgxpool.Pool }

func (s dbSubsidiaries) Subsidiaries(ctx context.Context, vendorID int64, _ string) ([]Subsidiary, error) {
	rows, err := s.d.Query(ctx, `SELECT name, ratio::float8 FROM vendor_subsidiaries WHERE vendor_id = $1 ORDER BY name`, vendorID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subsidiary, error) {
		var s Subsidiary
		err := r.Scan(&s.Name, &s.Ratio)
		return s, err
	})
}

// importSubsidiaries 手工导入子公司名单：CSV 每行 "名称,持股百分比(0-100)"，# 开头为注释；同名覆盖比例。返回导入行数。
func importSubsidiaries(ctx context.Context, d *pgxpool.Pool, vendorID int64, r io.Reader) (int, error) {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.FieldsPerRecord = 2
	cr.TrimLeadingSpace = true
	recs, err := cr.ReadAll()
	if err != nil {
		return 0, err
	}
	type row struct {
		name  string
		ratio float64
	}
	var rows []row
	for i, rec := range recs {
		name := strings.TrimSpace(rec[0])
		ratio, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(rec[1]), "%"), 64)
		if name == "" || err != nil || ratio < 0 || ratio > 100 || ratio != ratio {
			return 0, fmt.Errorf("line %d: invalid %q", i+1, strings.Join(rec, ","))
		}
		rows = append(rows, row{name, ratio})
	}
	err = pgx.BeginFunc(ctx, d, func(tx pgx.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(ctx, `INSERT INTO vendor_subsidiaries (vendor_id, name, ratio) VALUES ($1, $2, $3)
				ON CONFLICT (vendor_id, name) DO UPDATE SET ratio = EXCLUDED.ratio`, vendorID, r.name, r.ratio); err != nil {
				return err
			}
		}
		return nil
	})
	return len(rows), err
}

const minSubsidiaryRatio = 50 // 持股 >= 50% 的子公司才扩展

type candidate struct {
	source   string
	evidence map[string]any
}

// expandVendor 跑扩展链路，返回 域名 → 发现依据（已是根域的不返回）。
// 第二个返回值为 ICP 查询失败次数（子公司源失败不计）。
func expandVendor(ctx context.Context, icp ICPSource, subs SubsidiarySource, vendorID int64, company string, roots []string) (map[string][]candidate, int, error) {
	out := map[string][]candidate{}
	isRoot := map[string]bool{}
	for _, r := range roots {
		isRoot[normalizeHost(r)] = true
	}
	var errs []error
	icpFails := 0
	add := func(recs []ICPRecord, source string, extra map[string]any) {
		for _, rec := range recs {
			dom := normalizeHost(rec.Domain)
			if dom == "" || !strings.Contains(dom, ".") || isRoot[dom] {
				continue
			}
			ev := map[string]any{"via": source, "company": rec.Company, "license": rec.License}
			for k, v := range extra {
				ev[k] = v
			}
			out[dom] = append(out[dom], candidate{source, ev})
		}
	}
	queried := map[string]bool{}
	byCompany := func(name, source string, extra map[string]any) {
		if name == "" || queried[name] {
			return
		}
		queried[name] = true
		recs, err := icp.ByCompany(ctx, name)
		if err != nil {
			icpFails++
			errs = append(errs, err)
			return
		}
		add(recs, source, extra)
	}
	// 1) 厂商主体名 → ICP
	byCompany(company, "icp:company", nil)
	// 2) 子公司（>= 50%，只一层）→ ICP
	if subs != nil && company != "" {
		list, err := subs.Subsidiaries(ctx, vendorID, company)
		if err != nil {
			errs = append(errs, err)
		}
		for _, s := range list {
			if s.Ratio >= minSubsidiaryRatio {
				byCompany(s.Name, "icp:subsidiary", map[string]any{"subsidiary": s.Name, "ratio": s.Ratio})
			}
		}
	}
	// 3) 已有根域 → ICP 反查主体 → 该主体的 ICP
	for _, r := range roots {
		recs, err := icp.ByDomain(ctx, r)
		if err != nil {
			icpFails++
			errs = append(errs, err)
			continue
		}
		for _, rec := range recs {
			byCompany(rec.Company, "icp:reverse", map[string]any{"from_root": r})
		}
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, icpFails, errors.Join(errs...)
	}
	for _, err := range errs {
		log.Printf("expand vendor %d: %v", vendorID, err)
	}
	return out, icpFails, nil
}

// storeCandidates 写入 root_candidates：新域名为 pending；已存在且 pending 的合并来源与依据；已确认/已忽略的不动（忽略不再提示）。
// 返回新增条数。
func storeCandidates(ctx context.Context, d *pgxpool.Pool, vendorID int64, found map[string][]candidate) (int, error) {
	doms := make([]string, 0, len(found))
	for k := range found {
		doms = append(doms, k)
	}
	sort.Strings(doms)
	created := 0
	for _, dom := range doms {
		var srcs []string
		var evs []map[string]any
		for _, c := range found[dom] {
			srcs = append(srcs, c.source)
			ev := map[string]any{"vendor_id": vendorID} // 记录发现该域名的厂商（同域名被别的厂商发现时证据里可见）
			for k, v := range c.evidence {
				ev[k] = v
			}
			evs = append(evs, ev)
		}
		var inserted bool
		err := d.QueryRow(ctx, `INSERT INTO root_candidates (vendor_id, domain, sources, evidence)
			SELECT $1, $2, ARRAY(SELECT DISTINCT unnest($3::text[]) ORDER BY 1), $4
			WHERE NOT EXISTS (SELECT 1 FROM root_domains WHERE domain = $2)
			ON CONFLICT (domain) DO UPDATE SET
				sources = ARRAY(SELECT DISTINCT unnest(root_candidates.sources || EXCLUDED.sources) ORDER BY 1),
				evidence = (SELECT jsonb_agg(DISTINCT e) FROM jsonb_array_elements(root_candidates.evidence || EXCLUDED.evidence) e),
				updated_at = now()
			WHERE root_candidates.status = 'pending'
			RETURNING xmax = 0`, vendorID, dom, srcs, evs).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // 已是根域，或已确认/已忽略
		} else if err != nil {
			return created, err
		}
		if inserted {
			created++
		}
	}
	return created, nil
}

// confirmCandidate 确认：写入 root_domains（归属该厂商），状态改 confirmed。不自动触发扫描。
func confirmCandidate(ctx context.Context, d *pgxpool.Pool, id int64) error {
	return pgx.BeginFunc(ctx, d, func(tx pgx.Tx) error {
		var vendorID int64
		var dom string
		if err := tx.QueryRow(ctx, `UPDATE root_candidates SET status = 'confirmed', updated_at = now()
			WHERE id = $1 AND status = 'pending' RETURNING vendor_id, domain`, id).Scan(&vendorID, &dom); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("candidate %d not pending", id)
			}
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, $2) ON CONFLICT (domain) DO NOTHING`, vendorID, dom)
		return err
	})
}

// reassignCandidate 改归属：仅 pending 状态可改，改到另一厂商。
func reassignCandidate(ctx context.Context, d *pgxpool.Pool, id, vendorID int64) error {
	tag, err := d.Exec(ctx, `UPDATE root_candidates SET vendor_id = $2, updated_at = now() WHERE id = $1 AND status = 'pending'`, id, vendorID)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("candidate %d not pending", id)
	}
	return err
}

// ignoreCandidate 忽略：以后扩展再发现也不再提示。
func ignoreCandidate(ctx context.Context, d *pgxpool.Pool, id int64) error {
	tag, err := d.Exec(ctx, `UPDATE root_candidates SET status = 'ignored', updated_at = now() WHERE id = $1 AND status = 'pending'`, id)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("candidate %d not pending", id)
	}
	return err
}

// expandHandler 是 expand 类扫描（只手动触发）：对任务所属根域的厂商跑扩展，结果进 root_candidates，不串联后续扫描。
func expandHandler(d *pgxpool.Pool, icp ICPSource, subs SubsidiarySource) ScanHandler {
	return func(ctx context.Context, scanID int64, _ []string) ([]string, error) {
		var vendorID int64
		var company *string
		if err := d.QueryRow(ctx, `SELECT v.id, v.company_name FROM scan_tasks s JOIN root_domains r ON r.id = s.root_id
			JOIN vendors v ON v.id = r.vendor_id WHERE s.id = $1`, scanID).Scan(&vendorID, &company); err != nil {
			return nil, err
		}
		if icp == nil {
			log.Printf("expand vendor %d: ICP_API_URL not set, skipped", vendorID)
			_, err := d.Exec(ctx, `UPDATE scan_tasks SET result = '{"skipped": "icp not configured"}' WHERE id = $1`, scanID)
			return nil, err
		}
		roots, err := querySubdomains(ctx, d, `SELECT domain FROM root_domains WHERE vendor_id = $1 ORDER BY 1`, vendorID)
		if err != nil {
			return nil, err
		}
		name := ""
		if company != nil {
			name = strings.TrimSpace(*company)
		}
		found, fails, err := expandVendor(ctx, icp, subs, vendorID, name, roots)
		if err != nil {
			d.Exec(ctx, `UPDATE scan_tasks SET result = $2 WHERE id = $1`, scanID, map[string]int{"icp_failures": fails})
			return nil, err
		}
		created, err := storeCandidates(ctx, d, vendorID, found)
		if err != nil {
			return nil, err
		}
		log.Printf("expand vendor %d: %d domains found, %d new candidates", vendorID, len(found), created)
		_, err = d.Exec(ctx, `UPDATE scan_tasks SET result = $2 WHERE id = $1`, scanID,
			map[string]int{"found": len(found), "new_candidates": created, "icp_failures": fails})
		return nil, err
	}
}
