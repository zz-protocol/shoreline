package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	wappalyzer "github.com/projectdiscovery/wappalyzergo"
	"golang.org/x/time/rate"
)

// httpConfig：HTTP_RPS 每秒请求数（单实例，含泛解析对照请求）；HTTP_TIMEOUT 单请求超时；
// HTTP_SIMILARITY 泛解析页面相似度阈值（0–1）。
type httpConfig struct {
	RPS         float64
	Timeout     time.Duration
	Similarity  float64
	Schemes     []string
	MaxBody     int64
	Concurrency int
	// Dial 仅测试用：改写拨号地址（把任意主机指向本地 httptest 服务）。
	Dial func(addr string) string
}

func httpConfigFromEnv() (httpConfig, error) {
	c := httpConfig{RPS: 10, Timeout: 10 * time.Second, Similarity: 0.9, Schemes: []string{"https", "http"}, MaxBody: 1 << 20, Concurrency: 10}
	if s := os.Getenv("HTTP_RPS"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if !validPositive(f, err) {
			return c, fmt.Errorf("HTTP_RPS: invalid %q", s)
		}
		c.RPS = f
	}
	if s := os.Getenv("HTTP_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("HTTP_TIMEOUT: invalid %q", s)
		}
		c.Timeout = d
	}
	if s := os.Getenv("HTTP_SIMILARITY"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if !validPositive(f, err) || f > 1 {
			return c, fmt.Errorf("HTTP_SIMILARITY: invalid %q (0 < x <= 1)", s)
		}
		c.Similarity = f
	}
	return c, nil
}

type prober struct {
	cfg     httpConfig
	client  *http.Client
	limiter *rate.Limiter
}

var wappalyzerClient = sync.OnceValues(wappalyzer.New)

func newProber(cfg httpConfig) *prober {
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 只做探测，不校验证书
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if cfg.Dial != nil {
				addr = cfg.Dial(addr)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		MaxIdleConnsPerHost: 2,
	}
	return &prober{
		cfg: cfg,
		client: &http.Client{Timeout: cfg.Timeout, Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, // 不跟随跳转
		limiter: rate.NewLimiter(rate.Limit(cfg.RPS), 1),
	}
}

type probeResult struct {
	Scheme, Host string
	Port         int
	Status       int
	Title        string
	Redirect     string
	Tech         []string
	Fingerprint  string // sha256(status|title|tech)
	body         []byte
}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// get 只发 GET /，读取至多 MaxBody 字节。
func (p *prober) get(ctx context.Context, scheme, host string) (*probeResult, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", scheme+"://"+host+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "src-asset-monitor/0.1")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, p.cfg.MaxBody))
	r := &probeResult{Scheme: scheme, Host: host, Port: map[string]int{"https": 443, "http": 80}[scheme], Status: resp.StatusCode,
		Redirect: resp.Header.Get("Location"), body: body}
	if m := titleRe.FindSubmatch(body); m != nil {
		r.Title = strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	}
	if w, err := wappalyzerClient(); err == nil {
		for t := range w.Fingerprint(resp.Header, body) {
			r.Tech = append(r.Tech, t)
		}
		sort.Strings(r.Tech)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", r.Status, r.Title, strings.Join(r.Tech, ","))))
	r.Fingerprint = hex.EncodeToString(sum[:])
	return r, nil
}

// probeHost 对每个 scheme 发一次 GET /，返回成功的结果。
func (p *prober) probeHost(ctx context.Context, host string) []*probeResult {
	var out []*probeResult
	for _, s := range p.cfg.Schemes {
		if r, err := p.get(ctx, s, host); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// pageTokens 规范化页面：去掉主机名本身（泛解析页常回显 Host），按词切分取 3-gram 集合。
func pageTokens(r *probeResult) map[string]bool {
	text := strings.ToLower(string(r.body))
	text = strings.ReplaceAll(text, strings.ToLower(r.Host), "")
	words := strings.FieldsFunc(text, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c > 127)
	})
	set := map[string]bool{}
	for i := 0; i+3 <= len(words); i++ {
		set[strings.Join(words[i:i+3], " ")] = true
	}
	if len(words) < 3 && len(words) > 0 {
		set[strings.Join(words, " ")] = true
	}
	return set
}

// similarity 页面相似度：状态码不同为 0；否则为 3-gram Jaccard（两者都无内容时比较跳转目标）。
func similarity(a, b *probeResult) float64 {
	if a.Status != b.Status {
		return 0
	}
	ta, tb := pageTokens(a), pageTokens(b)
	if len(ta) == 0 && len(tb) == 0 {
		ra := strings.ReplaceAll(strings.ToLower(a.Redirect), strings.ToLower(a.Host), "")
		rb := strings.ReplaceAll(strings.ToLower(b.Redirect), strings.ToLower(b.Host), "")
		if ra == rb {
			return 1
		}
		return 0
	}
	inter := 0
	for k := range ta {
		if tb[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

// wildcardPage 取父域随机子域的对照页（按 parent+scheme 缓存）。
type pageCache struct {
	mu sync.Mutex
	m  map[string]*probeResult
	ok map[string]bool
}

func (p *prober) controlPage(ctx context.Context, c *pageCache, scheme, parent string) *probeResult {
	key := scheme + "://" + parent
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok[key] {
		return c.m[key]
	}
	b := make([]byte, 8)
	rand.Read(b)
	r, err := p.get(ctx, scheme, "wc-"+hex.EncodeToString(b)+"."+parent)
	if err != nil {
		r = nil
	}
	c.m[key], c.ok[key] = r, true
	return r
}

// isWildcardPage：结果与同 scheme 的父域随机子域页面相似度 >= 阈值。
func (p *prober) isWildcardPage(ctx context.Context, c *pageCache, r *probeResult) bool {
	dot := strings.IndexByte(r.Host, '.')
	if dot < 0 {
		return false
	}
	ctrl := p.controlPage(ctx, c, r.Scheme, r.Host[dot+1:])
	return ctrl != nil && similarity(r, ctrl) >= p.cfg.Similarity
}

// httpHandler 是 http 类扫描：对每个主机 GET /，记录状态码/标题/指纹/跳转并 upsert urls。
// DNS 阶段被标为泛解析（IP 落在泛解析集合）的主机也会探测：页面与父域随机子域对照页不相似则
// 视为真实主机（wildcard=false 并入库 URL），相似则保持 wildcard、不入库 URL。
func httpHandler(d *pgxpool.Pool, p *prober) ScanHandler {
	return func(ctx context.Context, scanID int64, hosts []string) ([]string, error) {
		cache := &pageCache{m: map[string]*probeResult{}, ok: map[string]bool{}}
		type item struct {
			host    string
			assetID int64
			dnsWild bool
		}
		var items []item
		for _, h := range hosts {
			it := item{host: normalizeHost(h)}
			if err := d.QueryRow(ctx, `SELECT id, wildcard FROM assets WHERE subdomain = $1`, it.host).Scan(&it.assetID, &it.dnsWild); err != nil {
				continue // 未入库的主机不探测
			}
			items = append(items, it)
		}
		var mu sync.Mutex
		var firstErr error
		sem := make(chan struct{}, max(1, p.cfg.Concurrency))
		var wg sync.WaitGroup
		for _, it := range items {
			wg.Add(1)
			sem <- struct{}{}
			go func(it item) {
				defer wg.Done()
				defer func() { <-sem }()
				results := p.probeHost(ctx, it.host)
				if len(results) == 0 {
					return
				}
				if it.dnsWild {
					allWild := true
					for _, r := range results {
						allWild = allWild && p.isWildcardPage(ctx, cache, r)
					}
					if allWild {
						return // 确认泛解析：不入库 URL
					}
					if _, err := d.Exec(ctx, `UPDATE assets SET wildcard = false WHERE id = $1`, it.assetID); err != nil {
						mu.Lock()
						firstErr = err
						mu.Unlock()
						return
					}
				}
				for _, r := range results {
					_, err := d.Exec(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, fingerprint_hash, redirect_to, tech, sources)
						VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '{http}')
						ON CONFLICT (scheme, host, port) DO UPDATE SET asset_id = EXCLUDED.asset_id, status_code = EXCLUDED.status_code,
							title = EXCLUDED.title, fingerprint_hash = EXCLUDED.fingerprint_hash, redirect_to = EXCLUDED.redirect_to,
							tech = EXCLUDED.tech, last_seen = now()`,
						it.assetID, r.Scheme, r.Host, r.Port, r.Status, r.Title, r.Fingerprint, nullIfEmpty(r.Redirect), nonNil(r.Tech))
					if err != nil {
						mu.Lock()
						firstErr = err
						mu.Unlock()
					}
				}
			}(it)
		}
		wg.Wait()
		return nil, firstErr
	}
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
