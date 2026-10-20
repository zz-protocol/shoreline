package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/projectdiscovery/subfinder/v2/pkg/passive"
	"github.com/projectdiscovery/subfinder/v2/pkg/subscraping"
)

// Source 是一个被动子域来源。
type Source struct {
	Name string
	Enum func(ctx context.Context, domain string) ([]string, error)
}

// passiveConfig：key 为空的测绘源自动跳过；Base URL 仅为测试替换用。
type passiveConfig struct {
	FofaKey, QuakeKey, HunterKey         string
	Pages, PageSize                      int
	CrtshURL, FofaURL, QuakeURL, HuntURL string
	Subfinder                            bool
	HTTP                                 *http.Client
}

func passiveConfigFromEnv() (passiveConfig, error) {
	c := passiveConfig{
		FofaKey: os.Getenv("FOFA_KEY"), QuakeKey: os.Getenv("QUAKE_KEY"), HunterKey: os.Getenv("HUNTER_KEY"),
		Pages: 1, PageSize: 100,
		CrtshURL: "https://crt.sh", FofaURL: "https://fofa.info", QuakeURL: "https://quake.360.net", HuntURL: "https://hunter.qianxin.com",
		Subfinder: true,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
	for name, p := range map[string]*int{"PASSIVE_PAGES": &c.Pages, "PASSIVE_PAGE_SIZE": &c.PageSize} {
		if s := os.Getenv(name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return c, fmt.Errorf("%s: invalid %q", name, s)
			}
			*p = n
		}
	}
	return c, nil
}

// passiveSources 按配置返回启用的来源：crt.sh、subfinder 免 key 源始终启用，测绘源有 key 才启用。
func passiveSources(c passiveConfig) []Source {
	srcs := []Source{{"crtsh", c.crtsh}}
	if c.Subfinder {
		srcs = append(srcs, Source{"subfinder", subfinderEnum})
	}
	if c.FofaKey != "" {
		srcs = append(srcs, Source{"fofa", c.fofa})
	}
	if c.QuakeKey != "" {
		srcs = append(srcs, Source{"quake", c.quake})
	}
	if c.HunterKey != "" {
		srcs = append(srcs, Source{"hunter", c.hunter})
	}
	return srcs
}

func (c passiveConfig) getJSON(ctx context.Context, req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", req.URL.Host, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// crt.sh：GET /?q=%.domain&output=json，name_value 可能含多行。
func (c passiveConfig) crtsh(ctx context.Context, domain string) ([]string, error) {
	req, _ := http.NewRequest("GET", c.CrtshURL+"/?output=json&q="+url.QueryEscape("%."+domain), nil)
	var rows []struct {
		NameValue string `json:"name_value"`
	}
	if err := c.getJSON(ctx, req, &rows); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		out = append(out, strings.Split(r.NameValue, "\n")...)
	}
	return out, nil
}

// FOFA：GET /api/v1/search/all?key=&qbase64=base64(domain="x")&fields=host&page=&size=
func (c passiveConfig) fofa(ctx context.Context, domain string) ([]string, error) {
	var out []string
	q := base64.StdEncoding.EncodeToString([]byte(`domain="` + domain + `"`))
	for page := 1; page <= c.Pages; page++ {
		u := fmt.Sprintf("%s/api/v1/search/all?key=%s&qbase64=%s&fields=host&page=%d&size=%d",
			c.FofaURL, url.QueryEscape(c.FofaKey), url.QueryEscape(q), page, c.PageSize)
		req, _ := http.NewRequest("GET", u, nil)
		var r struct {
			Error   bool              `json:"error"`
			Errmsg  string            `json:"errmsg"`
			Results []json.RawMessage `json:"results"`
		}
		if err := c.getJSON(ctx, req, &r); err != nil {
			return out, err
		}
		if r.Error {
			return out, errors.New("fofa: " + r.Errmsg)
		}
		for _, raw := range r.Results { // 单字段时为字符串，多字段时为数组
			var s string
			var arr []string
			if json.Unmarshal(raw, &s) == nil {
				out = append(out, s)
			} else if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
				out = append(out, arr[0])
			}
		}
		if len(r.Results) < c.PageSize {
			break
		}
	}
	return out, nil
}

// Quake：POST /api/v3/search/quake_service，X-QuakeToken。
func (c passiveConfig) quake(ctx context.Context, domain string) ([]string, error) {
	var out []string
	for page := 0; page < c.Pages; page++ {
		body, _ := json.Marshal(map[string]any{
			"query": `domain:"` + domain + `"`, "start": page * c.PageSize, "size": c.PageSize,
			"include": []string{"domain", "service.http.host"},
		})
		req, _ := http.NewRequest("POST", c.QuakeURL+"/api/v3/search/quake_service", bytes.NewReader(body))
		req.Header.Set("X-QuakeToken", c.QuakeKey)
		req.Header.Set("Content-Type", "application/json")
		var r struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
			Data    []struct {
				Domain  string `json:"domain"`
				Service struct {
					HTTP struct {
						Host string `json:"host"`
					} `json:"http"`
				} `json:"service"`
			} `json:"data"`
		}
		if err := c.getJSON(ctx, req, &r); err != nil {
			return out, err
		}
		if fmt.Sprint(r.Code) != "0" {
			return out, fmt.Errorf("quake: %v %s", r.Code, r.Message)
		}
		for _, d := range r.Data {
			out = append(out, d.Domain, d.Service.HTTP.Host)
		}
		if len(r.Data) < c.PageSize {
			break
		}
	}
	return out, nil
}

// Hunter：GET /openApi/search?api-key=&search=base64url(domain.suffix="x")&page=&page_size=
func (c passiveConfig) hunter(ctx context.Context, domain string) ([]string, error) {
	var out []string
	q := base64.URLEncoding.EncodeToString([]byte(`domain.suffix="` + domain + `"`))
	for page := 1; page <= c.Pages; page++ {
		u := fmt.Sprintf("%s/openApi/search?api-key=%s&search=%s&page=%d&page_size=%d",
			c.HuntURL, url.QueryEscape(c.HunterKey), url.QueryEscape(q), page, c.PageSize)
		req, _ := http.NewRequest("GET", u, nil)
		var r struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Arr []struct {
					Domain string `json:"domain"`
				} `json:"arr"`
			} `json:"data"`
		}
		if err := c.getJSON(ctx, req, &r); err != nil {
			return out, err
		}
		if r.Code != 200 {
			return out, fmt.Errorf("hunter: %d %s", r.Code, r.Message)
		}
		for _, a := range r.Data.Arr {
			out = append(out, a.Domain)
		}
		if len(r.Data.Arr) < c.PageSize {
			break
		}
	}
	return out, nil
}

// subfinderFreeSources 返回 subfinder 中无需 key 的源名（排除 crtsh，已单独实现）。
func subfinderFreeSources() []string {
	var names []string
	for _, s := range passive.AllSources {
		if s.KeyRequirement() == subscraping.NoKey && s.Name() != "crtsh" {
			names = append(names, s.Name())
		}
	}
	sort.Strings(names)
	return names
}

func subfinderEnum(ctx context.Context, domain string) ([]string, error) {
	agent := passive.NewWithProviderConfig(subfinderFreeSources(), nil, false, false, nil)
	var out []string
	for r := range agent.EnumerateSubdomainsWithCtx(ctx, domain, "", 0, 30, 5*time.Minute) {
		if r.Type == subscraping.Subdomain {
			out = append(out, r.Value)
		}
	}
	return out, nil
}

// normalizeSubdomain 去掉 scheme、端口、通配前缀，转小写；不属于 domain 的返回空。
func normalizeSubdomain(s, domain string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "*."), ".")
	if s == domain || strings.HasSuffix(s, "."+domain) {
		return s
	}
	return ""
}

// collectPassive 运行所有来源，返回 子域 → 来源列表；单个源失败只记日志，全部失败才报错。
func collectPassive(ctx context.Context, domain string, srcs []Source) (map[string][]string, error) {
	domain = normalizeHost(domain)
	found := map[string][]string{}
	var errs []error
	for _, src := range srcs {
		hosts, err := src.Enum(ctx, domain)
		if err != nil {
			log.Printf("passive %s %s: %v", src.Name, domain, err)
			errs = append(errs, fmt.Errorf("%s: %w", src.Name, err))
		}
		seen := map[string]bool{}
		for _, h := range hosts {
			if h = normalizeSubdomain(h, domain); h != "" && !seen[h] {
				seen[h] = true
				found[h] = append(found[h], src.Name)
			}
		}
	}
	if len(errs) == len(srcs) && len(srcs) > 0 {
		return nil, errors.Join(errs...)
	}
	return found, nil
}

// storeAssets upsert 资产：归属取最长匹配根域，in_scope 按该厂商范围规则判定，sources 合并去重，更新 last_seen。
func storeAssets(ctx context.Context, d *pgxpool.Pool, found map[string][]string) error {
	type root struct {
		id     int64
		domain string
		vendor int64
	}
	var roots []root
	rows, err := d.Query(ctx, `SELECT id, domain, vendor_id FROM root_domains`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r root
		if err := rows.Scan(&r.id, &r.domain, &r.vendor); err != nil {
			return err
		}
		r.domain = normalizeHost(r.domain)
		roots = append(roots, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rules := map[int64][]ScopeRule{}
	rows, err = d.Query(ctx, `SELECT vendor_id, kind, pattern FROM scope_rules`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int64
		var r ScopeRule
		if err := rows.Scan(&v, &r.Kind, &r.Pattern); err != nil {
			return err
		}
		rules[v] = append(rules[v], r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for host, srcs := range found {
		var best *root
		for i, r := range roots {
			if (host == r.domain || strings.HasSuffix(host, "."+r.domain)) && (best == nil || len(r.domain) > len(best.domain)) {
				best = &roots[i]
			}
		}
		if best == nil {
			continue
		}
		_, err := d.Exec(ctx, `INSERT INTO assets (root_id, subdomain, sources, in_scope) VALUES ($1, $2, $3, $4)
			ON CONFLICT (subdomain) DO UPDATE SET
				last_seen = now(), root_id = EXCLUDED.root_id, in_scope = EXCLUDED.in_scope,
				sources = ARRAY(SELECT DISTINCT unnest(assets.sources || EXCLUDED.sources) ORDER BY 1)`,
			best.id, host, srcs, InScope(host, rules[best.vendor]))
		if err != nil {
			return err
		}
	}
	return nil
}

// passiveHandler 是 passive 类扫描：收集、入库，返回子域交给 dns。
func passiveHandler(d *pgxpool.Pool, srcs []Source) ScanHandler {
	return func(ctx context.Context, scanID int64, _ []string) ([]string, error) {
		var domain string
		if err := d.QueryRow(ctx, `SELECT r.domain FROM scan_tasks s JOIN root_domains r ON r.id = s.root_id WHERE s.id = $1`,
			scanID).Scan(&domain); err != nil {
			return nil, err
		}
		found, err := collectPassive(ctx, domain, srcs)
		if err != nil {
			return nil, err
		}
		if err := storeAssets(ctx, d, found); err != nil {
			return nil, err
		}
		hosts := make([]string, 0, len(found))
		for h := range found {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		return hosts, nil
	}
}
