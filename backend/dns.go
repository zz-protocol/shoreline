package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/miekg/dns"
	"github.com/projectdiscovery/cdncheck"
	"golang.org/x/time/rate"
)

// dnsConfig：DNS_RESOLVERS 逗号分隔 host:port；DNS_TIMEOUT 单次查询超时；DNS_QPS 每秒查询数（单实例）。
type dnsConfig struct {
	Resolvers      []string
	Timeout        time.Duration
	QPS            float64
	WildcardProbe  int // 前 N 次探测均无解析则判定无泛解析
	WildcardStable int // DNS_WILDCARD_STABLE：连续 N 次无新 IP 停止初始探测（默认 5）
	WildcardMax    int // DNS_WILDCARD_MAX：每个父域探测总上限，含补探（默认 60）
	Concurrency    int
}

func dnsConfigFromEnv() (dnsConfig, error) {
	c := dnsConfig{Resolvers: []string{"223.5.5.5:53", "119.29.29.29:53", "1.1.1.1:53"}, Timeout: 2 * time.Second, QPS: 50, WildcardProbe: 3, WildcardStable: 5, WildcardMax: 60, Concurrency: 20}
	if s := os.Getenv("DNS_RESOLVERS"); s != "" {
		c.Resolvers = nil
		for _, r := range strings.Split(s, ",") {
			r = strings.TrimSpace(r)
			if !strings.Contains(r, ":") {
				r += ":53"
			}
			c.Resolvers = append(c.Resolvers, r)
		}
	}
	if s := os.Getenv("DNS_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("DNS_TIMEOUT: invalid %q", s)
		}
		c.Timeout = d
	}
	for name, p := range map[string]*int{"DNS_WILDCARD_STABLE": &c.WildcardStable, "DNS_WILDCARD_MAX": &c.WildcardMax} {
		if s := os.Getenv(name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				return c, fmt.Errorf("%s: invalid %q (integer >= 1)", name, s)
			}
			*p = n
		}
	}
	if c.WildcardMax < c.WildcardStable {
		return c, fmt.Errorf("DNS_WILDCARD_MAX (%d) must be >= DNS_WILDCARD_STABLE (%d)", c.WildcardMax, c.WildcardStable)
	}
	if s := os.Getenv("DNS_QPS"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if !validPositive(f, err) {
			return c, fmt.Errorf("DNS_QPS: invalid %q", s)
		}
		c.QPS = f
	}
	return c, nil
}

// cdnCNAMESuffixes：CNAME 后缀 → CDN 厂商（常见国内外 CDN，可按需补充）。
var cdnCNAMESuffixes = map[string]string{
	"kunlunsl.com": "aliyun", "kunlunca.com": "aliyun", "alikunlun.com": "aliyun", "alicdn.com": "aliyun", "aliyuncs.com": "aliyun",
	"cdn.dnsv1.com": "tencent", "dsa.dnsv1.com": "tencent", "tdnsv5.com": "tencent", "cdntip.com": "tencent", "qcloudcdn.com": "tencent",
	"bytecdn.cn": "bytedance", "volcgslb.com": "bytedance", "bytegslb.com": "bytedance", "volccdn.com": "bytedance",
	"wscdns.com": "wangsu", "chinanetcenter.com": "wangsu", "lxdns.com": "wangsu",
	"cloudflare.net": "cloudflare", "akamaiedge.net": "akamai", "edgekey.net": "akamai", "akamaitechnologies.com": "akamai",
	"cloudfront.net": "cloudfront", "fastly.net": "fastly", "edgesuite.net": "akamai",
}

// cdnIPs 用 cdncheck（MIT）内置 IP 段数据识别 CDN/WAF，只做本地匹配，不发起网络请求。
var cdnIPs = sync.OnceValue(func() *cdncheck.Client {
	c, _ := cdncheck.NewWithOpts(1, []string{"127.0.0.1:53"}) // resolver 不会被使用（只调 CheckCDN/CheckWAF）
	return c
})

func cdnOfIPs(ips []string) string {
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if ok, v, err := cdnIPs().CheckCDN(ip); err == nil && ok && v != "" {
			return v
		}
		if ok, v, err := cdnIPs().CheckWAF(ip); err == nil && ok && v != "" {
			return v
		}
	}
	return ""
}

func cdnOf(cnames []string) string {
	for _, c := range cnames {
		c = normalizeHost(c)
		for suf, name := range cdnCNAMESuffixes {
			if c == suf || strings.HasSuffix(c, "."+suf) {
				return name
			}
		}
	}
	return ""
}

type dnsResult struct {
	IPs    []string
	CNAMEs []string
}

type resolver struct {
	cfg     dnsConfig
	client  *dns.Client
	limiter *rate.Limiter // 所有查询（含泛解析探测、回退重试）共享
}

func newResolver(cfg dnsConfig) *resolver {
	if cfg.QPS <= 0 {
		cfg.QPS = 50
	}
	return &resolver{cfg: cfg, client: &dns.Client{Timeout: cfg.Timeout}, limiter: rate.NewLimiter(rate.Limit(cfg.QPS), 1)}
}

// lookup 查询 A 与 AAAA；依次尝试各 resolver，NXDOMAIN/无记录返回空结果。
func (r *resolver) lookup(ctx context.Context, host string) (dnsResult, error) {
	var res dnsResult
	seen := map[string]bool{}
	for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA} {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(host), qt)
		var in *dns.Msg
		var err error
		for _, srv := range r.cfg.Resolvers {
			if err = r.limiter.Wait(ctx); err != nil {
				return res, err
			}
			if in, _, err = r.client.ExchangeContext(ctx, m, srv); err == nil && in.Rcode != dns.RcodeServerFailure {
				break
			}
		}
		if err != nil {
			return res, err
		}
		for _, rr := range in.Answer {
			var v string
			switch x := rr.(type) {
			case *dns.A:
				v = x.A.String()
				if !seen[v] {
					res.IPs = append(res.IPs, v)
				}
			case *dns.AAAA:
				v = x.AAAA.String()
				if !seen[v] {
					res.IPs = append(res.IPs, v)
				}
			case *dns.CNAME:
				v = "cname:" + normalizeHost(x.Target)
				if !seen[v] {
					res.CNAMEs = append(res.CNAMEs, normalizeHost(x.Target))
				}
			}
			seen[v] = true
		}
	}
	sort.Strings(res.IPs)
	return res, nil
}

// 泛解析探测（自适应）：对每个父域用随机子域探测，探到新 IP 就继续，直到连续 WildcardStable 次无新 IP
// 或总次数达 WildcardMax；前 WildcardProbe 次都无解析则视为无泛解析。遇到 IP 不在集合内的子域时，
// 在剩余预算内补探（应对轮换 IP 的泛解析），预算用尽仍未覆盖则视为真实记录。
type wildcardEntry struct {
	mu     sync.Mutex
	ips    map[string]bool
	probes int
	ready  bool
}

type wildcardCache struct {
	mu sync.Mutex
	m  map[string]*wildcardEntry
}

func (c *wildcardCache) entry(parent string) *wildcardEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[parent]
	if e == nil {
		e = &wildcardEntry{ips: map[string]bool{}}
		c.m[parent] = e
	}
	return e
}

// probeOnce 探测一个随机子域，返回是否发现新 IP。调用方持有 e.mu。
func (r *resolver) probeOnce(ctx context.Context, e *wildcardEntry, parent string) bool {
	e.probes++
	b := make([]byte, 8)
	rand.Read(b)
	res, err := r.lookup(ctx, "wc-"+hex.EncodeToString(b)+"."+parent)
	if err != nil {
		return false
	}
	found := false
	for _, ip := range res.IPs {
		if !e.ips[ip] {
			e.ips[ip] = true
			found = true
		}
	}
	return found
}

// isWildcard 判断 ips 是否全部落在 parent 的泛解析 IP 集合内（必要时补探）。
func (r *resolver) isWildcard(ctx context.Context, cache *wildcardCache, parent string, ips []string) bool {
	e := cache.entry(parent)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready {
		e.ready = true
		stable := 0
		for e.probes < r.cfg.WildcardMax && stable < r.cfg.WildcardStable {
			if r.probeOnce(ctx, e, parent) {
				stable = 0
			} else {
				stable++
			}
			if len(e.ips) == 0 && e.probes >= r.cfg.WildcardProbe {
				break // 无泛解析
			}
		}
	}
	if len(e.ips) == 0 {
		return false
	}
	covered := func() bool {
		for _, ip := range ips {
			if !e.ips[ip] {
				return false
			}
		}
		return true
	}
	for !covered() && e.probes < r.cfg.WildcardMax {
		r.probeOnce(ctx, e, parent)
	}
	return covered()
}

type hostDNS struct {
	Host     string
	Result   dnsResult
	Resolved bool
	Wildcard bool
	CDN      string
}

// resolveHosts 解析并做泛解析过滤与 CDN 标记。泛解析判定：host 的全部 IP 都落在其父域随机子域的解析 IP 集合内。
// 只探测 root 本身或其子域作为父域（不对根域之外的域名发探测）。
func (r *resolver) resolveHosts(ctx context.Context, root string, hosts []string) []hostDNS {
	root = normalizeHost(root)
	cache := &wildcardCache{m: map[string]*wildcardEntry{}}
	out := make([]hostDNS, len(hosts))
	sem := make(chan struct{}, max(1, r.cfg.Concurrency))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, h string) {
			defer wg.Done()
			defer func() { <-sem }()
			hd := hostDNS{Host: h}
			res, err := r.lookup(ctx, h)
			if err == nil && len(res.IPs) > 0 {
				hd.Result, hd.Resolved = res, true
				if dot := strings.IndexByte(h, '.'); dot > 0 && (h[dot+1:] == root || strings.HasSuffix(h[dot+1:], "."+root)) {
					hd.Wildcard = r.isWildcard(ctx, cache, h[dot+1:], res.IPs)
				}
				if hd.CDN = cdnOf(res.CNAMEs); hd.CDN == "" {
					hd.CDN = cdnOfIPs(res.IPs)
				}
			}
			out[i] = hd
		}(i, h)
	}
	wg.Wait()
	return out
}

// dnsHandler 是 dns 类扫描：解析、泛解析（IP 比对）标记、CDN 标记并更新资产；返回解析成功的主机交给 http，
// 其中 IP 判为泛解析的由 http 阶段页面相似度复核（相似才确认泛解析，不入库 URL）。
func dnsHandler(d *pgxpool.Pool, r *resolver) ScanHandler {
	return func(ctx context.Context, scanID int64, hosts []string) ([]string, error) {
		var root string
		if err := d.QueryRow(ctx, `SELECT r.domain FROM scan_tasks s JOIN root_domains r ON r.id = s.root_id WHERE s.id = $1`,
			scanID).Scan(&root); err != nil {
			return nil, err
		}
		var next []string
		for _, hd := range r.resolveHosts(ctx, root, hosts) {
			var cdn *string
			if hd.CDN != "" {
				cdn = &hd.CDN
			}
			ips, cnames := hd.Result.IPs, hd.Result.CNAMEs
			if ips == nil {
				ips = []string{}
			}
			if cnames == nil {
				cnames = []string{}
			}
			if _, err := d.Exec(ctx, `UPDATE assets SET ips = $2, cnames = $3, resolved = $4, wildcard = $5, cdn = $6,
				resolved_at = now() WHERE subdomain = $1`, normalizeHost(hd.Host), ips, cnames, hd.Resolved, hd.Wildcard, cdn); err != nil {
				return nil, err
			}
			if hd.Resolved { // 泛解析候选也交给 http，由页面相似度最终判定（T8）
				next = append(next, hd.Host)
			}
		}
		return next, nil
	}
}
