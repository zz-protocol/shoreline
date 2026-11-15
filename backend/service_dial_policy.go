package main

import (
	"context"
	"errors"
	"net"
	"sync"

	"golang.org/x/time/rate"
)

// 跳过原因（架构 11-15 定）：未做 CDN 判定；无可拨地址；命中内网/回环/链路本地/未指定/云元数据。
const (
	skipServiceScanCDNUnknown = "cdn_unknown"
	skipPrivateAddress        = "private_address"
	skipNoDialIP              = "no_dial_ip"
)

// serviceIPResolver 在规划阶段解析 CDN 判定所用的 A/AAAA。结果只放在内存里，不落库、不进响应或日志。
type serviceIPResolver func(ctx context.Context, host string) ([]net.IP, error)

// serviceIPDialer 按字面 IP 拨号（生产默认拨号器）；测试可注入以记录拨号目标，不得把 IP 写进产品日志。
type serviceIPDialer func(ctx context.Context, ip net.IP, port int) (net.Conn, error)

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

func resolverFromDNS(r *resolver) serviceIPResolver {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, host string) ([]net.IP, error) {
		res, err := r.lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		var ips []net.IP
		for _, s := range res.IPs {
			if ip := net.ParseIP(s); ip != nil {
				ips = append(ips, ip)
			}
		}
		return ips, nil
	}
}

var errServiceDialNotIP = errors.New("service dialer requires a literal IP")

func prepareServiceDialIPs(ctx context.Context, q serviceScanQ, cfg serviceFingerprintConfig, t serviceScanTarget) ([]net.IP, error) {
	if cfg.Dialer != nil {
		return nil, nil
	}
	var raw []net.IP
	if cfg.Resolve != nil {
		ips, err := cfg.Resolve(ctx, t.Host)
		if err != nil {
			return nil, err
		}
		raw = ips
	}
	allowed, blocked := filterDialIPs(raw)
	if len(allowed) == 0 {
		reason := skipNoDialIP
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
