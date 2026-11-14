package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/time/rate"
)

// identifyRule 是可测试的最小标题/响应头规则集（T45，测试锁定）。
// Header 为空表示匹配标题。Contains 不区分大小写。
type identifyRule struct {
	Name     string
	Source   string // header | title
	Header   string
	Contains string
}

var identifyRules = []identifyRule{
	{Name: "Apache HTTP Server", Source: "header", Header: "Server", Contains: "Apache"},
	{Name: "Nginx", Source: "header", Header: "Server", Contains: "nginx"},
	{Name: "IIS", Source: "header", Header: "Server", Contains: "Microsoft-IIS"},
	{Name: "OpenResty", Source: "header", Header: "Server", Contains: "openresty"},
	{Name: "PHP", Source: "header", Header: "X-Powered-By", Contains: "PHP"},
	{Name: "Drupal", Source: "header", Header: "X-Generator", Contains: "Drupal"},
	{Name: "WordPress", Source: "title", Contains: "WordPress"},
	{Name: "phpMyAdmin", Source: "title", Contains: "phpMyAdmin"},
	{Name: "Jenkins", Source: "title", Contains: "Jenkins"},
	{Name: "GitLab", Source: "title", Contains: "GitLab"},
}

// 始终记录的少量响应头；规则里出现的其它头也会一并记下（不存全文）。
var recordedHeaders = []string{"Server", "X-Powered-By", "X-Generator"}

const maxHeaderValue = 200

func clipHeader(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxHeaderValue {
		return s[:maxHeaderValue]
	}
	return s
}

func collectProbeHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := out[name]; ok {
			return
		}
		if v := clipHeader(h.Get(name)); v != "" {
			out[name] = v
		}
	}
	for _, name := range recordedHeaders {
		add(name)
	}
	for _, rule := range identifyRules {
		add(rule.Header)
	}
	return out
}

type identifyHit struct {
	Name, Source, Header, Value string
}

func matchIdentifyRules(title string, headers map[string]string) []identifyHit {
	var hits []identifyHit
	titleFold := strings.ToLower(title)
	for _, rule := range identifyRules {
		need := strings.ToLower(rule.Contains)
		if need == "" {
			continue
		}
		if rule.Source == "title" {
			if strings.Contains(titleFold, need) {
				hits = append(hits, identifyHit{Name: rule.Name, Source: "title", Value: title})
			}
			continue
		}
		if rule.Header == "" || headers == nil {
			continue
		}
		val, ok := headers[rule.Header]
		if !ok {
			val = headers[http.CanonicalHeaderKey(rule.Header)]
		}
		if val != "" && strings.Contains(strings.ToLower(val), need) {
			hits = append(hits, identifyHit{Name: rule.Name, Source: "header", Header: rule.Header, Value: val})
		}
	}
	return hits
}

func syncIdentifyComponents(ctx context.Context, q dbq, urlID int64, hits []identifyHit) error {
	keep := make([]int64, 0, len(hits))
	for _, hit := range hits {
		var id int64
		if err := q.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed)
			VALUES ($1, $2, $3, true)
			ON CONFLICT (name_key) DO UPDATE SET updated_at = now()
			RETURNING id`, hit.Name, strings.ToLower(hit.Name), hit.Source).Scan(&id); err != nil {
			return err
		}
		ev, _ := json.Marshal(map[string]string{"rule": hit.Name, "header": hit.Header, "value": clipHeader(hit.Value)})
		if _, err := q.Exec(ctx, `INSERT INTO url_components (url_id, component_id, version, source, evidence)
			VALUES ($1, $2, '', $3, $4)
			ON CONFLICT (url_id, component_id) DO UPDATE SET
				evidence = CASE WHEN url_components.source = 'wappalyzer' THEN url_components.evidence ELSE EXCLUDED.evidence END,
				source = CASE WHEN url_components.source = 'wappalyzer' THEN url_components.source ELSE EXCLUDED.source END`,
			urlID, id, hit.Source, ev); err != nil {
			return err
		}
		keep = append(keep, id)
	}
	_, err := q.Exec(ctx, `DELETE FROM url_components
		WHERE url_id = $1 AND source IN ('header', 'title') AND NOT (component_id = ANY($2::bigint[]))`, urlID, keep)
	return err
}

var linkTagRe = regexp.MustCompile(`(?is)<link\b[^>]*>`)
var attrRe = regexp.MustCompile(`(?is)([^\s=]+)\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)

func declaredFaviconHref(body []byte) string {
	for _, tag := range linkTagRe.FindAll(body, -1) {
		attrs := map[string]string{}
		for _, m := range attrRe.FindAllSubmatch(tag, -1) {
			k := strings.ToLower(string(m[1]))
			v := string(m[2])
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
				v = v[1 : len(v)-1]
			}
			attrs[k] = v
		}
		rel := strings.ToLower(attrs["rel"])
		if rel == "" || attrs["href"] == "" {
			continue
		}
		if strings.Contains(rel, "icon") { // icon / shortcut icon / apple-touch-icon
			return strings.TrimSpace(attrs["href"])
		}
	}
	return ""
}

// sameHostFaviconURL 把 href 解析到与页面同主机的绝对 URL；站外返回 ok=false。
func sameHostFaviconURL(scheme, host, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "data:") {
		return "", false
	}
	base, err := url.Parse(scheme + "://" + host + "/")
	if err != nil {
		return "", false
	}
	u, err := base.Parse(href)
	if err != nil || u.User != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if !strings.EqualFold(u.Hostname(), host) {
		return "", false
	}
	u.Fragment = ""
	return u.String(), true
}

// fetchFavicon 至多再发一次同主机 GET（声明的 favicon，否则 /favicon.ico），走传入限速，不跟随站外跳转。
func (p *prober) fetchFavicon(ctx context.Context, lim *rate.Limiter, r *probeResult) {
	if r == nil {
		return
	}
	target := ""
	if href := declaredFaviconHref(r.body); href != "" {
		if u, ok := sameHostFaviconURL(r.Scheme, r.Host, href); ok {
			target = u
		}
	}
	if target == "" {
		if u, ok := sameHostFaviconURL(r.Scheme, r.Host, "/favicon.ico"); ok {
			target = u
		}
	}
	if target == "" {
		return
	}
	// 声明指向当前已取的 / 时不再请求（避免把 HTML 当 favicon）。
	if u, err := url.Parse(target); err == nil && (u.Path == "" || u.Path == "/") {
		return
	}
	if lim != nil {
		if err := lim.Wait(ctx); err != nil {
			return
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "src-asset-monitor/0.1")
	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, p.cfg.MaxBody))
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		return
	}
	sum := sha256.Sum256(body)
	r.FaviconHash = hex.EncodeToString(sum[:])
}
