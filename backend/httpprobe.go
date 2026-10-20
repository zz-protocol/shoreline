package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	wappalyzer "github.com/projectdiscovery/wappalyzergo"
	"golang.org/x/time/rate"
)

// httpConfig：HTTP_RPS 每秒请求数（单实例，含泛解析对照请求）；HTTP_TIMEOUT 单请求超时；
// HTTP_SIMILARITY 泛解析页面相似度阈值（0–1）；HTTP_CONCURRENCY 单个扫描任务内同时探测的主机数。
type httpConfig struct {
	RPS         float64
	Timeout     time.Duration
	Similarity  float64
	Schemes     []string
	MaxBody     int64
	Concurrency int
	VerifyRPS   float64 // HTTP_VERIFY_RPS：泛解析页面复核单独限速（含对照页）
	VerifyMax   int     // HTTP_VERIFY_MAX：每根域每轮最多复核主机数，超出的标 wildcard
	// Dial 仅测试用：改写拨号地址（把任意主机指向本地 httptest 服务）。
	Dial func(addr string) string
}

func httpConfigFromEnv() (httpConfig, error) {
	c := httpConfig{RPS: 10, Timeout: 10 * time.Second, Similarity: 0.9, Schemes: []string{"https", "http"}, MaxBody: 1 << 20, Concurrency: 10, VerifyRPS: 3, VerifyMax: 200}
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
	if s := os.Getenv("HTTP_CONCURRENCY"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("HTTP_CONCURRENCY: invalid %q (integer >= 1)", s)
		}
		c.Concurrency = n
	}
	if s := os.Getenv("HTTP_VERIFY_RPS"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if !validPositive(f, err) {
			return c, fmt.Errorf("HTTP_VERIFY_RPS: invalid %q", s)
		}
		c.VerifyRPS = f
	}
	if s := os.Getenv("HTTP_VERIFY_MAX"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return c, fmt.Errorf("HTTP_VERIFY_MAX: invalid %q (integer >= 0)", s)
		}
		c.VerifyMax = n
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
	verify  *rate.Limiter // 泛解析复核专用
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
		verify:  rate.NewLimiter(rate.Limit(cfg.VerifyRPS), 1),
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

// get 只发 GET /（普通限速），读取至多 MaxBody 字节。
func (p *prober) get(ctx context.Context, scheme, host string) (*probeResult, error) {
	return p.getWith(ctx, p.limiter, scheme, host)
}

func (p *prober) getWith(ctx context.Context, lim *rate.Limiter, scheme, host string) (*probeResult, error) {
	if err := lim.Wait(ctx); err != nil {
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
func (p *prober) probeHost(ctx context.Context, lim *rate.Limiter, host string) []*probeResult {
	var out []*probeResult
	for _, s := range p.cfg.Schemes {
		if r, err := p.getWith(ctx, lim, s, host); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// pageWords 规范化页面并切词（最多 maxPageWords 个）：去掉主机名本身（泛解析页常回显 Host），
// 再去掉规则内的动态 token——uuid、>= 8 位十六进制串、含数字的词（时间戳、数字、多数随机 ID）、
// 长度 >= 20 的词（长随机串）、英文星期/月份名（文本日期）。规则外的动态内容由两张对照页的差异掩码处理。
func pageWords(r *probeResult) []string {
	text := strings.ToLower(string(r.body))
	text = strings.ReplaceAll(text, strings.ToLower(r.Host), "")
	text = dynamicRe.ReplaceAllString(text, " ")
	words := strings.FieldsFunc(text, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c > 127)
	})
	words = slices.DeleteFunc(words, dynamicToken)
	if len(words) > maxPageWords {
		words = words[:maxPageWords]
	}
	return words
}

const maxPageWords = 3000

// minTemplateWords：两张对照页公共词少于此数时改用短页模式（状态码 / hash / 长度比较）。
const minTemplateWords = 5

// pageTokens：pageWords 的 3-gram 集合。
func pageTokens(r *probeResult) map[string]bool {
	words := pageWords(r)
	set := map[string]bool{}
	for i := 0; i+3 <= len(words); i++ {
		set[strings.Join(words[i:i+3], " ")] = true
	}
	if len(words) < 3 && len(words) > 0 {
		set[strings.Join(words, " ")] = true
	}
	return set
}

// dynamicRe：uuid（含各段纯字母的情况）与 >= 8 位十六进制串。
var dynamicRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|\b[0-9a-f]{8,}\b`)

var dateWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`monday tuesday wednesday thursday friday saturday sunday mon tue tues wed thu thur thurs fri sat sun
		january february march april june july august september october november december jan feb mar apr jun jul aug sep sept oct nov dec`) {
		dateWords[w] = true
	}
}

// dynamicToken：含数字的词、长度 >= 20 的词、英文星期/月份名。
func dynamicToken(w string) bool {
	return len(w) >= 20 || strings.ContainsAny(w, "0123456789") || dateWords[w]
}

// similarity 两页的 3-gram Jaccard：状态码不同为 0（两者都无内容时比较跳转目标）。
func similarity(a, b *probeResult) float64 {
	if a.Status != b.Status {
		return 0
	}
	ta, tb := pageTokens(a), pageTokens(b)
	if len(ta) == 0 && len(tb) == 0 {
		return sameRedirect(a, b)
	}
	inter := 0
	for k := range ta {
		if tb[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

func sameRedirect(a, b *probeResult) float64 {
	ra := strings.ReplaceAll(strings.ToLower(a.Redirect), strings.ToLower(a.Host), "")
	rb := strings.ReplaceAll(strings.ToLower(b.Redirect), strings.ToLower(b.Host), "")
	if ra == rb {
		return 1
	}
	return 0
}

// lcs 返回两个词序列的最长公共子序列。
func lcs(a, b []string) []string {
	dp := make([][]int32, len(a)+1)
	for i := range dp {
		dp[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var out []string
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i, j = i+1, j+1
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}

// wildcardTemplate 由同一父域两个随机子域的页面构成：两页共同的词序列为模板，
// 两页之间不同的词（规则外的随机词、base64、日期等）视为动态内容被掩码，allowed 为每页动态词数。
type wildcardTemplate struct {
	sample   *probeResult
	template []string
	allowed  int
	// 短页模式（模板公共词 < minTemplateWords）：候选状态码与对照页不同 → 真实；
	// 两张对照页 hash 相同时，候选 hash 不同 → 真实；只有长度接近时，候选长度明显不同 → 真实；其余按泛解析。
	short                 bool
	hash                  string
	length                int
	stableHash, stableLen bool
	maxLen                int // 两张短对照页中较长的正文长度
}

// pageHash 规范化后（去主机名与动态 token）的词序列 hash。
func pageHash(r *probeResult) string {
	sum := sha256.Sum256([]byte(strings.Join(pageWords(r), " ")))
	return hex.EncodeToString(sum[:])
}

// bodyLen 去掉主机名后的正文长度。
func bodyLen(r *probeResult) int {
	return len(strings.ReplaceAll(strings.ToLower(string(r.body)), strings.ToLower(r.Host), ""))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// templateSimilarity 候选页与模板的相似度：模板覆盖率，超出动态词配额的多余词计入分母。
func (t *wildcardTemplate) similarity(r *probeResult) float64 {
	if r.Status != t.sample.Status {
		return 0
	}
	if t.short {
		// 对照页长度稳定时以长度为准（hash 可能因归一化后残留的随机片段而抖动）；仅 hash 稳定时才比 hash。
		switch {
		case bodyLen(r) > t.maxLen+64: // 比两张短对照页中较长的一张还长出 64B 以上：判真实（对照页长度/hash 都不稳定时也适用）
			return 0
		case t.stableLen && abs(bodyLen(r)-t.length) > max(64, t.length*3/10):
			return 0
		case !t.stableLen && t.stableHash && pageHash(r) != t.hash:
			return 0
		}
		return 1
	}
	words := pageWords(r)
	if len(t.template) == 0 {
		if len(words) == 0 {
			return sameRedirect(r, t.sample)
		}
		return 0
	}
	common := len(lcs(words, t.template))
	extra := max(0, len(words)-common-t.allowed)
	return float64(common) / float64(len(t.template)+extra)
}

// pageCache 缓存每个 parent+scheme 的泛解析模板。
type pageCache struct {
	mu sync.Mutex
	m  map[string]*wildcardTemplate
	ok map[string]bool
}

func (p *prober) randomPage(ctx context.Context, scheme, parent string) *probeResult {
	b := make([]byte, 8)
	rand.Read(b)
	r, err := p.getWith(ctx, p.verify, scheme, "wc-"+hex.EncodeToString(b)+"."+parent)
	if err != nil {
		return nil
	}
	return r
}

// controlTemplate 取父域 2 个随机子域的对照页建立模板（按 parent+scheme 缓存）。某次请求失败（含超时）时重取一个随机子域；
// 仍不足 2 页、两页状态码不一致、或有空页，返回 insufficient=true：拿不到可用对照页一律不放行，本轮该父域候选保持 wildcard。
// 模板太短时改用短页模式（见 wildcardTemplate.short）。
func (p *prober) controlTemplate(ctx context.Context, c *pageCache, scheme, parent string) (t *wildcardTemplate, insufficient bool) {
	key := scheme + "://" + parent
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok[key] {
		return c.m[key], c.m[key] == nil
	}
	c.ok[key] = true
	var pages []*probeResult
	for tries := 0; tries < 3 && len(pages) < 2; tries++ { // 2 页 + 失败时重取 1 次
		if r := p.randomPage(ctx, scheme, parent); r != nil {
			pages = append(pages, r)
		}
	}
	if len(pages) < 2 {
		return nil, true
	}
	a, b := pages[0], pages[1]
	wa, wb := pageWords(a), pageWords(b)
	tpl := lcs(wa, wb)
	redirectOnly := len(wa) == 0 && len(wb) == 0 && a.Redirect != "" && sameRedirect(a, b) == 1
	if a.Status != b.Status || (len(a.body) == 0 || len(b.body) == 0) && !redirectOnly {
		return nil, true // 对照页不可用（状态码不一致、空页）：不放行，候选保持 wildcard 留待下轮
	}
	if len(tpl) < minTemplateWords && !redirectOnly {
		// 模板太短（如 nginx 默认 404、"not found"）：改用短页模式，见 wildcardTemplate.short
		ha, hb := pageHash(a), pageHash(b)
		la, lb := bodyLen(a), bodyLen(b)
		c.m[key] = &wildcardTemplate{sample: a, short: true, hash: ha, length: la, maxLen: max(la, lb),
			stableHash: ha == hb, stableLen: abs(la-lb) <= max(la, lb)/10}
		return c.m[key], false
	}
	c.m[key] = &wildcardTemplate{sample: a, template: tpl, allowed: max(len(wa), len(wb)) - len(tpl)}
	return c.m[key], false
}

// isWildcardPage：结果与同 scheme 的父域泛解析模板相似度 >= 阈值；对照页不足 2 页时按泛解析处理（保持 wildcard）。
func (p *prober) isWildcardPage(ctx context.Context, c *pageCache, r *probeResult) bool {
	dot := strings.IndexByte(r.Host, '.')
	if dot < 0 {
		return false
	}
	t, insufficient := p.controlTemplate(ctx, c, r.Scheme, r.Host[dot+1:])
	if insufficient {
		return true
	}
	return t.similarity(r) >= p.cfg.Similarity
}

// httpHandler 是 http 类扫描：对每个主机 GET /，记录状态码/标题/指纹/跳转并 upsert urls。
// 泛解析候选（DNS 标 wildcard，或父域存在泛解析——轮换 IP 可能漏标）走复核：用 HTTP_VERIFY_RPS 单独限速，
// 每根域每轮最多 HTTP_VERIFY_MAX 个，与父域随机子域的对照页比较；任一协议页面不相似即为真实主机
// （wildcard=false、入库 URL、生成新增），否则标 wildcard、不入库 URL。超出上限的候选直接标 wildcard。
func httpHandler(d *pgxpool.Pool, p *prober) ScanHandler {
	return func(ctx context.Context, scanID int64, hosts []string) ([]string, error) {
		cache := &pageCache{m: map[string]*wildcardTemplate{}, ok: map[string]bool{}}
		type item struct {
			host      string
			assetID   int64
			candidate bool
		}
		var items []item
		verified := 0
		for _, h := range hosts {
			it := item{host: normalizeHost(h)}
			var wild, parentWild bool
			if err := d.QueryRow(ctx, `SELECT id, wildcard, parent_wildcard FROM assets WHERE subdomain = $1`, it.host).
				Scan(&it.assetID, &wild, &parentWild); err != nil {
				continue // 未入库的主机不探测
			}
			if it.candidate = wild || parentWild; it.candidate {
				if verified >= p.cfg.VerifyMax {
					if _, err := d.Exec(ctx, `UPDATE assets SET wildcard = true WHERE id = $1`, it.assetID); err != nil {
						return nil, err
					}
					continue
				}
				verified++
			}
			items = append(items, it)
		}
		var mu sync.Mutex
		var firstErr error
		fail := func(err error) {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}
		sem := make(chan struct{}, p.cfg.Concurrency)
		var wg sync.WaitGroup
		for _, it := range items {
			wg.Add(1)
			sem <- struct{}{}
			go func(it item) {
				defer wg.Done()
				defer func() { <-sem }()
				lim := p.limiter
				if it.candidate {
					lim = p.verify
				}
				results := p.probeHost(ctx, lim, it.host)
				if len(results) == 0 {
					return
				}
				if it.candidate {
					allWild := true
					for _, r := range results {
						allWild = allWild && p.isWildcardPage(ctx, cache, r)
					}
					_, err := d.Exec(ctx, `UPDATE assets SET wildcard = $2, parent_wildcard = $2,
						verified_real_at = CASE WHEN $2 THEN NULL ELSE now() END WHERE id = $1`, it.assetID, allWild)
					if err == nil && !allWild { // 页面复核为真实主机：此时才算新增资产
						err = emitNew(ctx, d, it.assetID, nil, map[string]any{"subdomain": it.host, "verified_real": true})
					}
					if err != nil || allWild {
						if err != nil {
							fail(err)
						}
						return // 确认泛解析：不入库 URL
					}
				}
				for _, r := range results {
					if err := p.storeURL(ctx, d, it.assetID, r); err != nil {
						fail(err)
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

// storeURL upsert URL，并按新增/变更生成事件（状态码、标题、指纹 hash 任一变化为变更）。
// 在事务里 SELECT ... FOR UPDATE 锁行后比较新旧值：并发同结果时只有第一个真正改值的记 changed。
func (p *prober) storeURL(ctx context.Context, d *pgxpool.Pool, assetID int64, r *probeResult) error {
	url := fmt.Sprintf("%s://%s:%d", r.Scheme, r.Host, r.Port)
	for {
		retry := false
		err := pgx.BeginFunc(ctx, d, func(tx pgx.Tx) error {
			var urlID int64
			var oldStatus *int
			var oldTitle, oldFP *string
			err := tx.QueryRow(ctx, `SELECT id, status_code, title, fingerprint_hash FROM urls
				WHERE scheme = $1 AND host = $2 AND port = $3 FOR UPDATE`, r.Scheme, r.Host, r.Port).Scan(&urlID, &oldStatus, &oldTitle, &oldFP)
			if errors.Is(err, pgx.ErrNoRows) {
				err = tx.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, fingerprint_hash, redirect_to, tech, sources)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '{http}') ON CONFLICT (scheme, host, port) DO NOTHING RETURNING id`,
					assetID, r.Scheme, r.Host, r.Port, r.Status, r.Title, r.Fingerprint, nullIfEmpty(r.Redirect), nonNil(r.Tech)).Scan(&urlID)
				if errors.Is(err, pgx.ErrNoRows) { // 并发插入：重来走更新路径
					retry = true
					return nil
				} else if err != nil {
					return err
				}
				return emitNew(ctx, tx, assetID, &urlID, map[string]any{"url": url, "status_code": r.Status, "title": r.Title})
			} else if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE urls SET asset_id = $2, status_code = $3, title = $4, fingerprint_hash = $5,
					redirect_to = $6, tech = $7, last_seen = now() WHERE id = $1`,
				urlID, assetID, r.Status, r.Title, r.Fingerprint, nullIfEmpty(r.Redirect), nonNil(r.Tech)); err != nil {
				return err
			}
			changes := map[string]change{}
			if oldStatus == nil || *oldStatus != r.Status {
				changes["status_code"] = change{oldStatus, r.Status}
			}
			if oldTitle == nil || *oldTitle != r.Title {
				changes["title"] = change{oldTitle, r.Title}
			}
			if oldFP == nil || *oldFP != r.Fingerprint {
				changes["fingerprint_hash"] = change{oldFP, r.Fingerprint}
			}
			if len(changes) == 0 {
				return nil
			}
			return emitEvent(ctx, tx, "changed", assetID, &urlID, map[string]any{"url": url, "changes": changes})
		})
		if err != nil || !retry {
			return err
		}
	}
}
