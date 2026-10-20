package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 主动发现（T10）：字典爆破 + 变形 + 对本轮新增非泛解析子域递归一层。按厂商总开关 vendors.active_enabled（默认关），
// 开了也限速。批量解析用 DNS_RESOLVERS，命中后用 ACTIVE_TRUSTED_RESOLVERS 复核（puredns 式），再按 T7 规则过滤泛解析。
// 只手动触发（定时在 T15），产出交 dns → http。

//go:embed wordlists/subdomains-top1million-5000.txt
var builtinWordlist []byte // SecLists（MIT），见 THIRD_PARTY_LICENSES

const maxMutations = 50 // 每个子域最多生成的变形数

type activeConfig struct {
	QPS           float64  // ACTIVE_QPS：主动发现所有 DNS 查询（批量+复核+泛解析探测）共享限速
	MaxCandidates int      // ACTIVE_MAX_CANDIDATES：每根域每轮候选上限（含递归）
	Trusted       []string // ACTIVE_TRUSTED_RESOLVERS
	Words         []string // 内置字典，或 ACTIVE_WORDLIST 指定的外部文件（替换内置）
}

func activeConfigFromEnv() (activeConfig, error) {
	c := activeConfig{QPS: 20, MaxCandidates: 20000,
		Trusted: []string{"223.5.5.5:53", "119.29.29.29:53", "1.1.1.1:53", "8.8.8.8:53"}}
	if s := os.Getenv("ACTIVE_QPS"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if !validPositive(f, err) {
			return c, fmt.Errorf("ACTIVE_QPS: invalid %q", s)
		}
		c.QPS = f
	}
	if s := os.Getenv("ACTIVE_MAX_CANDIDATES"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("ACTIVE_MAX_CANDIDATES: invalid %q (integer >= 1)", s)
		}
		c.MaxCandidates = n
	}
	if s := os.Getenv("ACTIVE_TRUSTED_RESOLVERS"); s != "" {
		c.Trusted = nil
		for _, r := range strings.Split(s, ",") {
			if r = strings.TrimSpace(r); r == "" {
				continue
			}
			if !strings.Contains(r, ":") {
				r += ":53"
			}
			c.Trusted = append(c.Trusted, r)
		}
		if len(c.Trusted) == 0 {
			return c, fmt.Errorf("ACTIVE_TRUSTED_RESOLVERS: empty")
		}
	}
	var src io.Reader = bytes.NewReader(builtinWordlist)
	if p := os.Getenv("ACTIVE_WORDLIST"); p != "" {
		f, err := os.Open(p)
		if err != nil {
			return c, fmt.Errorf("ACTIVE_WORDLIST: %w", err)
		}
		defer f.Close()
		src = f
	}
	words, err := readWordlist(src)
	if err != nil {
		return c, err
	}
	if len(words) == 0 {
		return c, fmt.Errorf("ACTIVE_WORDLIST: no valid words")
	}
	c.Words = words
	return c, nil
}

var wordRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// readWordlist：每行一个词，转小写去空白，跳过空行、注释和不合法的标签，去重保序。
func readWordlist(r io.Reader) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		w := strings.ToLower(strings.TrimSpace(sc.Text()))
		if w == "" || strings.HasPrefix(w, "#") || !wordRe.MatchString(w) || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out, sc.Err()
}

var digitsRe = regexp.MustCompile(`\d+`)

var mutationWords = []string{"dev", "test", "staging", "api"}

// mutations 对子域首个标签做变形：数字增减（±1）、dev/test/staging/api 前后缀（- 连接）及作为新一级前缀；最多 50 个。
func mutations(host string) []string {
	dot := strings.IndexByte(host, '.')
	if dot <= 0 {
		return nil
	}
	label, parent := host[:dot], host[dot+1:]
	var out []string
	seen := map[string]bool{host: true}
	add := func(l string) {
		h := l + "." + parent
		if len(out) < maxMutations && !seen[h] && wordRe.MatchString(l) {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, loc := range digitsRe.FindAllStringIndex(label, -1) {
		n, err := strconv.Atoi(label[loc[0]:loc[1]])
		if err != nil {
			continue
		}
		for _, d := range []int{-1, 1} {
			if m := n + d; m >= 0 {
				add(label[:loc[0]] + fmt.Sprintf("%0*d", loc[1]-loc[0], m) + label[loc[1]:])
			}
		}
	}
	for _, w := range mutationWords {
		add(w + "-" + label)
		add(label + "-" + w)
		add(w + "." + label)
	}
	return out
}

type activeScanner struct {
	cfg     activeConfig
	bulk    *resolver // DNS_RESOLVERS，限速 ACTIVE_QPS
	trusted *resolver // ACTIVE_TRUSTED_RESOLVERS，与 bulk 共享限速
}

func newActiveScanner(cfg activeConfig, dcfg dnsConfig) *activeScanner {
	dcfg.QPS = cfg.QPS
	bulk := newResolver(dcfg)
	tcfg := dcfg
	tcfg.Resolvers = cfg.Trusted
	trusted := newResolver(tcfg)
	trusted.limiter = bulk.limiter
	return &activeScanner{cfg: cfg, bulk: bulk, trusted: trusted}
}

// resolve 批量解析候选，命中者用可信 resolver 复核，再按 T7 规则（父域随机子域 IP 集合）过滤泛解析。
func (a *activeScanner) resolve(ctx context.Context, root string, cands []string, wc *wildcardCache) []string {
	var mu sync.Mutex
	var hits []string
	sem := make(chan struct{}, max(1, a.bulk.cfg.Concurrency))
	var wg sync.WaitGroup
	for _, h := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			if res, err := a.bulk.lookup(ctx, h); err != nil || len(res.IPs) == 0 {
				return
			}
			res, err := a.trusted.lookup(ctx, h)
			if err != nil || len(res.IPs) == 0 {
				return // 可信 resolver 复核不通过（批量 resolver 投毒/劫持）
			}
			if dot := strings.IndexByte(h, '.'); dot > 0 && (h[dot+1:] == root || strings.HasSuffix(h[dot+1:], "."+root)) {
				if wild, _ := a.trusted.isWildcard(ctx, wc, h[dot+1:], res.IPs); wild {
					return
				}
			}
			mu.Lock()
			hits = append(hits, h)
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	sort.Strings(hits)
	return hits
}

// discover 生成候选并解析：字典 × 根域、mutable（已解析非泛解析子域）的变形；再以本轮新增的非泛解析子域为父域跑一层字典。
// 已入库的子域不作候选；候选总数受 ACTIVE_MAX_CANDIDATES 限制。
func (a *activeScanner) discover(ctx context.Context, root string, known, mutable []string) (hits []string, tried int) {
	root = normalizeHost(root)
	seen := map[string]bool{root: true}
	for _, k := range known {
		seen[k] = true
	}
	budget := a.cfg.MaxCandidates
	collect := func(add func(func(string))) []string {
		var c []string
		add(func(h string) {
			if budget > 0 && !seen[h] {
				seen[h] = true
				budget--
				c = append(c, h)
			}
		})
		return c
	}
	wc := &wildcardCache{m: map[string]*wildcardEntry{}}
	first := collect(func(push func(string)) {
		for _, w := range a.cfg.Words {
			push(w + "." + root)
		}
		for _, k := range mutable {
			for _, m := range mutations(k) {
				push(m)
			}
		}
	})
	hits = a.resolve(ctx, root, first, wc)
	second := collect(func(push func(string)) { // 递归一层：只对本轮新增的非泛解析子域
		for _, h := range hits {
			for _, w := range a.cfg.Words {
				push(w + "." + h)
			}
		}
	})
	hits = append(hits, a.resolve(ctx, root, second, wc)...)
	sort.Strings(hits)
	return hits, len(first) + len(second)
}

// activeHandler 是 active 类扫描：厂商开关关闭时直接跳过（不发任何 DNS 查询）。
func activeHandler(d *pgxpool.Pool, a *activeScanner) ScanHandler {
	return func(ctx context.Context, scanID int64, _ []string) ([]string, error) {
		var rootID int64
		var root string
		var enabled bool
		if err := d.QueryRow(ctx, `SELECT r.id, r.domain, v.active_enabled FROM scan_tasks s
			JOIN root_domains r ON r.id = s.root_id JOIN vendors v ON v.id = r.vendor_id WHERE s.id = $1`, scanID).
			Scan(&rootID, &root, &enabled); err != nil {
			return nil, err
		}
		if !enabled {
			log.Printf("active %s: vendor switch off, skipped", root)
			return nil, nil
		}
		known, err := querySubdomains(ctx, d, `SELECT subdomain FROM assets WHERE root_id = $1 ORDER BY 1`, rootID)
		if err != nil {
			return nil, err
		}
		// 只对已解析、非泛解析的已知子域做变形
		mutable, err := querySubdomains(ctx, d, `SELECT subdomain FROM assets WHERE root_id = $1 AND resolved AND NOT wildcard ORDER BY 1`, rootID)
		if err != nil {
			return nil, err
		}
		hits, tried := a.discover(ctx, root, known, mutable)
		log.Printf("active %s: %d candidates, %d hits", root, tried, len(hits))
		found := map[string][]string{}
		for _, h := range hits {
			found[h] = []string{"active"}
		}
		if _, err := storeAssets(ctx, d, found); err != nil {
			return nil, err
		}
		return hits, nil
	}
}

func querySubdomains(ctx context.Context, d *pgxpool.Pool, q string, args ...any) ([]string, error) {
	rows, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
