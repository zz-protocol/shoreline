package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/idna"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
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
	provider  string // "generic"（默认，records 约定）或 "icp_yanzhenma"
	client    *http.Client
}

// ICP_PROVIDER 预设：generic 为本项目约定格式；icp_yanzhenma 为用户自建的 zz-protocol/icp_yanzhenma（FastAPI，MIT）服务。
var icpProviders = []string{"generic", "icp_yanzhenma"}

func icpSourceFromEnv() (ICPSource, error) {
	base := os.Getenv("ICP_API_URL")
	provider := os.Getenv("ICP_PROVIDER")
	if provider == "" {
		provider = "generic"
	}
	if !slices.Contains(icpProviders, provider) {
		return nil, fmt.Errorf("ICP_PROVIDER must be one of %s, got %q", strings.Join(icpProviders, "|"), provider)
	}
	if base == "" {
		return nil, nil // 未配置：跳过 ICP 扩展（退回手工导入）
	}
	if hasControl(base) || hasControl(os.Getenv("ICP_API_KEY")) {
		return nil, fmt.Errorf("ICP_API_URL/ICP_API_KEY must not contain control characters")
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ICP_API_URL: invalid %q", base)
	}
	return &httpICP{base: base, key: os.Getenv("ICP_API_KEY"), provider: provider, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return true
		}
	}
	return false
}

// expandStats：ICP 查询失败次数与各类丢弃计数（写入 scan_tasks.result）。
type expandStats struct {
	ICPFailures      int    `json:"icp_failures"`
	SkippedIP        int    `json:"skipped_ip"`
	SkippedInvalid   int    `json:"skipped_invalid"`
	SkippedKnownRoot int    `json:"skipped_known_root"`
	LastErr          string `json:"-"` // 最近一次 ICP 失败原因（degraded_reason）
}

var hexLabelRe = regexp.MustCompile(`^0x[0-9a-f]*$`)

// icpIsIP：ICP 记录的"域名"实为 IP。先去 scheme、path、userinfo（@ 前）、端口/方括号、IPv6 zone（%eth0）、末尾点，
// 再判：能解析为 IP，或末级标签全数字 / 0x 十六进制（覆盖 127.1、0x7f.0.0.1、010.0.0.1 这类 inet_aton 写法）。
func icpIsIP(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	switch {
	case strings.HasPrefix(s, "["):
		if i := strings.IndexByte(s, ']'); i >= 0 {
			s = s[1:i]
		}
	case strings.Count(s, ":") == 1:
		s = s[:strings.IndexByte(s, ':')]
	}
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	last := s[strings.LastIndexByte(s, '.')+1:]
	if hexLabelRe.MatchString(last) {
		return true
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	return last != ""
}

// normalizeICPDomain 规范化 ICP 返回的域名：去 scheme、path/query、端口、"*."、末尾点，转小写；非法返回 false。
func normalizeICPDomain(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "*."), ".")
	if a, err := idna.Lookup.ToASCII(s); err == nil { // 中文等 IDN 转 punycode
		s = a
	} else {
		return s, false
	}
	return s, validDomain(s)
}

// underRoot：d 是某个根域本身或其子域。
func underRoot(d string, roots map[string]bool) bool {
	for {
		if roots[d] {
			return true
		}
		i := strings.IndexByte(d, '.')
		if i < 0 {
			return false
		}
		d = d[i+1:]
	}
}

func (h *httpICP) query(ctx context.Context, param, value string) ([]ICPRecord, error) {
	if h.provider == "icp_yanzhenma" {
		return h.queryYanzhenma(ctx, value)
	}
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

// queryYanzhenma 调 icp_yanzhenma 的 GET <ICP_API_URL>/query?domain=<值>。该服务把参数作为工信部查询的 unitName 透传，
// 因此主体名和域名都走这一个接口（推断，待真实联调确认）。响应 {"status":"success|failed","domain":...,"error":...,"data":<工信部原始结果>}，
// 记录在 data.params.list（或 data.list），每项取 domain / unitName / mainLicence（domain 字段取法为推断，仓库未明写，待真实联调确认）。status 非 success 或 HTTP 非 200 记为失败。
func (h *httpICP) queryYanzhenma(ctx context.Context, value string) ([]ICPRecord, error) {
	u, _ := url.Parse(strings.TrimSuffix(h.base, "/") + "/query")
	u.RawQuery = url.Values{"domain": {value}}.Encode()
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
		return nil, fmt.Errorf("icp_yanzhenma %q: HTTP %d", value, resp.StatusCode)
	}
	type item struct {
		Domain      string `json:"domain"`
		UnitName    string `json:"unitName"`
		MainLicence string `json:"mainLicence"`
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			List   []item `json:"list"`
			Params struct {
				List []item `json:"list"`
			} `json:"params"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("icp_yanzhenma %q: %w", value, err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("icp_yanzhenma %q: status %q: %s", value, body.Status, body.Error)
	}
	list := body.Data.Params.List
	if list == nil {
		list = body.Data.List
	}
	var out []ICPRecord
	for _, it := range list {
		out = append(out, ICPRecord{Domain: it.Domain, Company: it.UnitName, License: it.MainLicence})
	}
	return out, nil
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

var ratioRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// parseRatio 解析持股百分比：十进制数字，可带 "%"/"％"（前面可有空格），范围 0-100。
func parseRatio(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "%"), "％"))
	if !ratioRe.MatchString(s) {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil && v >= 0 && v <= 100
}

var (
	headerRatios = []string{"比例", "持股", "持股比例", "持股百分比", "持股比例(%)", "持股比例%", "ratio", "share", "percent", "percentage"}
)

// isSubsidiaryHeader：第一行所有列都是非数字列名才算表头；为避免把 "子D,abc" 这类坏数据当表头，
// 比例列还必须是已知比例列名（比例/持股/ratio 等）。
func isSubsidiaryHeader(rec []string) bool {
	for _, c := range rec {
		if c = strings.TrimSpace(c); c == "" || ratioRe.MatchString(strings.TrimSpace(strings.TrimRight(c, "%％ "))) {
			return false
		}
	}
	return slices.Contains(headerRatios, strings.ToLower(strings.TrimSpace(rec[1])))
}

// importSubsidiaries 手工导入子公司名单：CSV 每行 "名称,持股百分比(0-100)"，# 开头为注释；同名覆盖比例。返回导入行数。
func importSubsidiaries(ctx context.Context, d *pgxpool.Pool, vendorID int64, r io.Reader) (int, error) {
	br := bufio.NewReader(r)
	if b, err := br.Peek(3); err == nil && string(b) == "\xef\xbb\xbf" { // 去 UTF-8 BOM
		br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.Comment = '#'
	cr.FieldsPerRecord = 2
	cr.TrimLeadingSpace = true
	type row struct {
		name  string
		ratio float64
	}
	var rows []row
	for first := true; ; first = false {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		line, _ := cr.FieldPos(0)
		if first && isSubsidiaryHeader(rec) { // 只有明显表头才跳过
			continue
		}
		name := strings.TrimSpace(rec[0])
		ratio, ok := parseRatio(rec[1])
		if name == "" || !ok {
			return 0, fmt.Errorf("line %d: invalid %q", line, strings.Join(rec, ","))
		}
		rows = append(rows, row{name, ratio})
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("no valid rows")
	}
	err := pgx.BeginFunc(ctx, d, func(tx pgx.Tx) error {
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
func expandVendor(ctx context.Context, icp ICPSource, subs SubsidiarySource, vendorID int64, company string, roots []string) (map[string][]candidate, expandStats, error) {
	out := map[string][]candidate{}
	isRoot := map[string]bool{}
	for _, r := range roots {
		isRoot[normalizeHost(r)] = true
	}
	var errs []error
	var st expandStats
	seen := map[string]bool{}
	add := func(recs []ICPRecord, source string, extra map[string]any) {
		for _, rec := range recs {
			if icpIsIP(rec.Domain) { // IP（v4/v6）不生成候选
				st.SkippedIP++
				continue
			}
			dom, ok := normalizeICPDomain(rec.Domain)
			if !ok {
				st.SkippedInvalid++
				continue
			}
			if underRoot(dom, isRoot) { // 已有根域及其子域：丢弃
				st.SkippedKnownRoot++
				continue
			}
			ev := map[string]any{"via": source, "company": rec.Company, "license": rec.License}
			for k, v := range extra {
				ev[k] = v
			}
			key, _ := json.Marshal([]any{source, ev})
			if seen[dom+"\x00"+string(key)] { // 同批同域名重复记录：证据去重
				continue
			}
			seen[dom+"\x00"+string(key)] = true
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
			st.ICPFailures++
			st.LastErr = err.Error()
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
			st.ICPFailures++
			st.LastErr = err.Error()
			errs = append(errs, err)
			continue
		}
		for _, rec := range recs {
			byCompany(rec.Company, "icp:reverse", map[string]any{"from_root": r})
		}
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, st, errors.Join(errs...)
	}
	for _, err := range errs {
		log.Printf("expand vendor %d: %v", vendorID, err)
	}
	return out, st, nil
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
			WHERE NOT EXISTS (SELECT 1 FROM root_domains WHERE domain = $2 OR right($2, length(domain) + 1) = '.' || domain)
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

var errRootTaken = errors.New("root domain already taken by another vendor")

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
		tag, err := tx.Exec(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, $2) ON CONFLICT (domain) DO NOTHING`, vendorID, dom)
		if err != nil || tag.RowsAffected() == 1 {
			return err
		}
		var owner int64
		if err := tx.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE domain = $1`, dom).Scan(&owner); err != nil {
			return err
		}
		if owner != vendorID { // 已被别的厂商占用：回滚，候选保持 pending
			return fmt.Errorf("%w: %s belongs to vendor %d", errRootTaken, dom, owner)
		}
		return nil
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
		if err != nil && fails.ICPFailures > 0 { // ICP 服务不可达/查询失败：降级，记失败次数，扫描不失败
			log.Printf("expand vendor %d: icp degraded: %v", vendorID, err)
			_, err = d.Exec(ctx, `UPDATE scan_tasks SET result = $2 WHERE id = $1`, scanID, map[string]any{
				"found": 0, "new_candidates": 0, "icp_failures": fails.ICPFailures, "degraded": true, "degraded_reason": err.Error()})
			return nil, err
		}
		if err != nil {
			d.Exec(ctx, `UPDATE scan_tasks SET result = $2 WHERE id = $1`, scanID, fails)
			return nil, err
		}
		created, err := storeCandidates(ctx, d, vendorID, found)
		if err != nil {
			return nil, err
		}
		log.Printf("expand vendor %d: %d domains found, %d new candidates", vendorID, len(found), created)
		res := map[string]any{"found": len(found), "new_candidates": created, "icp_failures": fails.ICPFailures,
			"skipped_ip": fails.SkippedIP, "skipped_invalid": fails.SkippedInvalid, "skipped_known_root": fails.SkippedKnownRoot}
		if fails.ICPFailures > 0 { // 部分 ICP 查询失败也标降级
			res["degraded"], res["degraded_reason"] = true, fails.LastErr
		}
		_, err = d.Exec(ctx, `UPDATE scan_tasks SET result = $2 WHERE id = $1`, scanID, res)
		return nil, err
	}
}
