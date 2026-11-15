package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"
)

// 跳过原因（架构 11-15 定）：未做 CDN 判定；无可拨地址；命中内网/回环/链路本地/未指定/云元数据。
// T58 把 no_dial_ip 细分为 DNS 负答案/失败几类；private_address 保留。
const (
	skipServiceScanCDNUnknown = "cdn_unknown"
	skipPrivateAddress        = "private_address"
	skipNoDialIP              = "no_dial_ip" // 历史原因，只用于识读旧行，不再写入
	skipDNSNXDomain           = "dns_nxdomain"
	skipDNSNoData             = "dns_nodata"
	skipDNSTimeout            = "dns_timeout"
	skipDNSError              = "dns_error"
	skipDNSNegativeCached     = "dns_negative_cached"
)

const (
	serviceDNSNegativeDefaultTTL = 6 * time.Hour
	serviceDNSNegativeMinTTL     = 6 * time.Hour
	serviceDNSNegativeMaxTTL     = 24 * time.Hour
)

// serviceDNSFailure 把 DNS 负答案/失败映射成 skip 原因。不包装底层错误，避免把 resolver 地址等细节带进库或日志。
type serviceDNSFailure struct {
	Kind string
}

func (e *serviceDNSFailure) Error() string { return "service dns: " + e.Kind }

// serviceIPResolver 在规划阶段解析 CDN 判定所用的 A/AAAA。结果只放在内存里，不落库、不进响应或日志。
type serviceIPResolver func(ctx context.Context, host string) ([]net.IP, error)

// serviceDNSAnswer 是服务拨号前的一次解析结果：IP 只留在内存，CNAME 仅供 CDN 复判。
type serviceDNSAnswer struct {
	IPs    []net.IP
	CNAMEs []string
}

// serviceDNSLookup 是生产解析入口，保留 CNAME 供拨号前复判 CDN。测试可改用 Resolve 注入纯 IP 结果。
type serviceDNSLookup func(ctx context.Context, host string) (serviceDNSAnswer, error)

// serviceIPDialer 按字面 IP 拨号（生产默认拨号器）；测试可注入以记录拨号目标，不得把 IP 写进产品日志。
type serviceIPDialer func(ctx context.Context, ip net.IP, port int) (net.Conn, error)

var errServiceDialNotIP = errors.New("service dialer requires a literal IP")

var metadataIPv4 = net.IPv4(169, 254, 169, 254)

// denyDialIP 在拨号前拒绝回环、私网、链路本地、未指定地址和云元数据 169.254.169.254。
// v4-mapped IPv6 按内嵌 IPv4 判定，整地址拒绝。
func denyDialIP(ip net.IP) string {
	if ip == nil || ip.IsUnspecified() {
		return skipPrivateAddress
	}
	if v4 := ip.To4(); v4 != nil {
		if v4.IsLoopback() || v4.IsUnspecified() || v4.IsLinkLocalUnicast() || v4.IsPrivate() || v4.Equal(metadataIPv4) {
			return skipPrivateAddress
		}
		return ""
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
		return skipPrivateAddress
	}
	return ""
}

var (
	serviceLimiterMu  sync.Mutex
	serviceLimiter    *rate.Limiter
	serviceLimiterRPS float64
)

func sharedServiceLimiter(rps float64) *rate.Limiter {
	if rps <= 0 {
		rps = 2
	}
	serviceLimiterMu.Lock()
	defer serviceLimiterMu.Unlock()
	if serviceLimiter == nil || serviceLimiterRPS != rps {
		serviceLimiter = rate.NewLimiter(rate.Limit(rps), 1)
		serviceLimiterRPS = rps
	}
	return serviceLimiter
}

func resetSharedServiceLimiter() {
	serviceLimiterMu.Lock()
	serviceLimiter = nil
	serviceLimiterRPS = 0
	serviceLimiterMu.Unlock()
}

// filterDialIPs 返回允许拨号的地址。调用方不得把 IP 写入日志或响应。
func filterDialIPs(ips []net.IP) (allowed []net.IP, blocked int) {
	for _, ip := range ips {
		if ip == nil {
			blocked++
			continue
		}
		if denyDialIP(ip) != "" {
			blocked++
			continue
		}
		allowed = append(allowed, ip)
	}
	return allowed, blocked
}

// resolverFromDNS 把 DNS 解析结果转成服务拨号候选；CNAME 保留给 prepareServiceDialIPs 复判 CDN。
func resolverFromDNS(r *resolver) serviceDNSLookup {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, host string) (serviceDNSAnswer, error) {
		res, st := r.lookupStatus(ctx, host)
		answer := serviceDNSAnswer{CNAMEs: res.CNAMEs}
		for _, s := range res.IPs {
			if ip := net.ParseIP(s); ip != nil {
				answer.IPs = append(answer.IPs, ip)
			}
		}
		switch st {
		case dnsLookupOK:
			return answer, nil
		case dnsLookupNXDomain:
			return answer, &serviceDNSFailure{Kind: skipDNSNXDomain}
		case dnsLookupNoData:
			return answer, &serviceDNSFailure{Kind: skipDNSNoData}
		case dnsLookupTimeout:
			return answer, &serviceDNSFailure{Kind: skipDNSTimeout}
		default:
			return answer, &serviceDNSFailure{Kind: skipDNSError}
		}
	}
}

func serviceDNSSkipReason(err error) string {
	var f *serviceDNSFailure
	if errors.As(err, &f) {
		switch f.Kind {
		case skipDNSNXDomain, skipDNSNoData, skipDNSTimeout, skipDNSError:
			return f.Kind
		}
	}
	return skipDNSError
}

// serviceDNSNegativeCached 判断资产是否处于 DNS 负缓存窗口：resolved=false 且 resolved_at 在 TTL 内。
// 命中则本轮不再解析，直接记 dns_negative_cached。
func serviceDNSNegativeCached(ctx context.Context, q serviceScanQ, t serviceScanTarget, ttl time.Duration) (bool, error) {
	if q == nil || ttl <= 0 {
		return false, nil
	}
	const cond = `COALESCE(resolved, true) = false AND resolved_at IS NOT NULL
		AND resolved_at > now() - make_interval(secs => $2)`
	var cached bool
	var err error
	if t.AssetID > 0 {
		err = q.QueryRow(ctx, `SELECT `+cond+` FROM assets WHERE id = $1`, t.AssetID, ttl.Seconds()).Scan(&cached)
	} else if h := normalizeHost(t.Host); h != "" {
		err = q.QueryRow(ctx, `SELECT `+cond+` FROM assets WHERE subdomain = $1`, h, ttl.Seconds()).Scan(&cached)
	} else {
		return false, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return cached, err
}

func resolveServiceDial(ctx context.Context, cfg serviceFingerprintConfig, host string) (serviceDNSAnswer, error) {
	switch {
	case cfg.Lookup != nil:
		return cfg.Lookup(ctx, host)
	case cfg.Resolve != nil:
		ips, err := cfg.Resolve(ctx, host)
		return serviceDNSAnswer{IPs: ips}, err
	default:
		return serviceDNSAnswer{}, nil
	}
}

// cdnFromAnswer 用既有 CDN 判定（CNAME 后缀 / cdncheck 段 / 可配置静态库）复判本次解析结果。
// IP 只在内存里参与本地匹配，不落库、不写日志。
func cdnFromAnswer(answer serviceDNSAnswer) string {
	if name := cdnOf(answer.CNAMEs); name != "" {
		return name
	}
	ips := make([]string, 0, len(answer.IPs))
	for _, ip := range answer.IPs {
		if ip != nil {
			ips = append(ips, ip.String())
		}
	}
	if name := cdnOfIPs(ips); name != "" {
		return name
	}
	return cdnFromStaticLibrary(answer.CNAMEs, ips)
}

// prepareServiceDialIPs 为一次服务探测准备拨号地址：
// 负缓存命中直接跳过；DNS 负答案/失败按原因记 skip；解析结果命中 CDN 时记 cdn_fronted 并跳过；
// 其余只保留公网地址。返回空表示本轮不拨号。
func prepareServiceDialIPs(ctx context.Context, q serviceScanQ, cfg serviceFingerprintConfig, t serviceScanTarget) ([]net.IP, error) {
	if cfg.Dialer != nil {
		return nil, nil
	}
	if cached, err := serviceDNSNegativeCached(ctx, q, t, cfg.negativeTTL()); err != nil {
		return nil, err
	} else if cached {
		return nil, recordServiceScanSkip(ctx, q, t, skipDNSNegativeCached)
	}
	answer, err := resolveServiceDial(ctx, cfg, t.Host)
	if err != nil {
		if rerr := recordServiceScanSkip(ctx, q, t, serviceDNSSkipReason(err)); rerr != nil {
			return nil, rerr
		}
		return nil, nil
	}
	if cdn := cdnFromAnswer(answer); cdn != "" {
		return nil, recordServiceScanSkip(ctx, q, t, skipServiceScanCDN)
	}
	allowed, blocked := filterDialIPs(answer.IPs)
	if len(allowed) == 0 {
		reason := skipDNSNoData
		if blocked > 0 {
			reason = skipPrivateAddress
		}
		if err := recordServiceScanSkip(ctx, q, t, reason); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return allowed, nil
}

func dialServicePort(ctx context.Context, cfg serviceFingerprintConfig, host string, port int, ips []net.IP) (net.Conn, error) {
	if cfg.Dialer != nil {
		return cfg.Dialer(ctx, "tcp", host, port)
	}
	var last error
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		var conn net.Conn
		var err error
		if cfg.DialIP != nil {
			conn, err = cfg.DialIP(ctx, ip, port)
		} else {
			conn, err = defaultServiceDialer(cfg.Timeout)(ctx, "tcp", ip.String(), port)
		}
		if err == nil {
			return conn, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no address to dial")
	}
	return nil, last
}
