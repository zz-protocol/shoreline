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
	Resolvers     []string
	Timeout       time.Duration
	QPS           float64
	WildcardProbe int // 每个父域探测的随机子域个数
	Concurrency   int
}

func dnsConfigFromEnv() (dnsConfig, error) {
	c := dnsConfig{Resolvers: []string{"223.5.5.5:53", "119.29.29.29:53", "1.1.1.1:53"}, Timeout: 2 * time.Second, QPS: 50, WildcardProbe: 3, Concurrency: 20}
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

// wildcardIPs 用随机子域探测 parent 是否泛解析，返回泛解析 IP 集合（空表示无泛解析）；按 parent 缓存。
type wildcardCache struct {
	mu sync.Mutex
	m  map[string]map[string]bool
}

func (r *resolver) wildcardIPs(ctx context.Context, cache *wildcardCache, parent string) map[string]bool {
	cache.mu.Lock()
	if v, ok := cache.m[parent]; ok {
		cache.mu.Unlock()
		return v
	}
	cache.mu.Unlock()
	ips := map[string]bool{}
	for i := 0; i < r.cfg.WildcardProbe; i++ {
		b := make([]byte, 8)
		rand.Read(b)
		res, err := r.lookup(ctx, "wc-"+hex.EncodeToString(b)+"."+parent)
		if err != nil {
			continue
		}
		for _, ip := range res.IPs {
			ips[ip] = true
		}
	}
	cache.mu.Lock()
	cache.m[parent] = ips
	cache.mu.Unlock()
	return ips
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
	cache := &wildcardCache{m: map[string]map[string]bool{}}
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
					wc := r.wildcardIPs(ctx, cache, h[dot+1:])
					if len(wc) > 0 {
						all := true
						for _, ip := range res.IPs {
							all = all && wc[ip]
						}
						hd.Wildcard = all
					}
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

// dnsHandler 是 dns 类扫描：解析、泛解析过滤、CDN 标记并更新资产；返回解析成功且非泛解析的主机交给 http。
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
			if hd.Resolved && !hd.Wildcard {
				next = append(next, hd.Host)
			}
		}
		return next, nil
	}
}
