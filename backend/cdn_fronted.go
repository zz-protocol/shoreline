package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// T50：命中 CDN/WAF 时跳过端口扫描的原因（测试锁定）。
const skipServiceScanCDN = "cdn_fronted"

type cdnNet struct {
	net  *net.IPNet
	name string
}

var (
	staticCDNMu      sync.Mutex
	extraCDNSuffixes = map[string]string{}
	extraCDNNets     []cdnNet
)

// registerCDNSuffix 追加可配置 CNAME 后缀（测试或 CDN_CNAME_FILE）。name 为 CDN 厂商标识。
func registerCDNSuffix(suffix, name string) {
	suffix = normalizeHost(suffix)
	name = strings.ToLower(strings.TrimSpace(name))
	if suffix == "" || name == "" {
		return
	}
	staticCDNMu.Lock()
	extraCDNSuffixes[suffix] = name
	staticCDNMu.Unlock()
}

// registerCDNCIDR 追加可配置 IP 段（测试或 CDN_CIDR_FILE）。仅用于本地匹配，不发起网络请求。
func registerCDNCIDR(cidr, name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	_, n, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil || name == "" {
		if err == nil {
			err = errors.New("empty CDN name")
		}
		return err
	}
	staticCDNMu.Lock()
	extraCDNNets = append(extraCDNNets, cdnNet{net: n, name: name})
	staticCDNMu.Unlock()
	return nil
}

func resetStaticCDNLibrary() {
	staticCDNMu.Lock()
	extraCDNSuffixes = map[string]string{}
	extraCDNNets = nil
	staticCDNMu.Unlock()
}

func loadCDNLibraryFile(path string, kind string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch kind {
		case "cname":
			registerCDNSuffix(fields[0], fields[1])
		case "cidr":
			if err := registerCDNCIDR(fields[0], fields[1]); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func loadCDNLibraryFromEnv() error {
	if err := loadCDNLibraryFile(os.Getenv("CDN_CNAME_FILE"), "cname"); err != nil {
		return err
	}
	return loadCDNLibraryFile(os.Getenv("CDN_CIDR_FILE"), "cidr")
}

func cdnFromStaticLibrary(cnames, ips []string) string {
	staticCDNMu.Lock()
	suffixes := make(map[string]string, len(extraCDNSuffixes))
	for k, v := range extraCDNSuffixes {
		suffixes[k] = v
	}
	nets := append([]cdnNet(nil), extraCDNNets...)
	staticCDNMu.Unlock()
	for _, c := range cnames {
		c = normalizeHost(c)
		for suf, name := range suffixes {
			if c == suf || strings.HasSuffix(c, "."+suf) {
				return name
			}
		}
	}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		for _, n := range nets {
			if n.net.Contains(ip) {
				return n.name
			}
		}
	}
	return ""
}

func lookupCDNFronted(ctx context.Context, q serviceScanQ, t serviceScanTarget) (bool, error) {
	var fronted bool
	var err error
	if t.AssetID > 0 {
		err = q.QueryRow(ctx, `SELECT cdn_fronted FROM assets WHERE id = $1`, t.AssetID).Scan(&fronted)
	} else if h := normalizeHost(t.Host); h != "" {
		err = q.QueryRow(ctx, `SELECT cdn_fronted FROM assets WHERE subdomain = $1`, h).Scan(&fronted)
	} else {
		return false, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return fronted, err
}

// planOriginServiceScan 是 T51 的队列接入点：先走 T49 开关与授权范围，再跳过 cdn_fronted，返回可进端口扫描的源站。
// 关闭时不记录逐主机跳过、不入队；超范围或 CDN 前置会记原因。不发起探测。
func planOriginServiceScan(ctx context.Context, q serviceScanQ, targets []serviceScanTarget) ([]serviceScanTarget, error) {
	enabled, err := loadServiceScanEnabled(ctx, q)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	patterns, err := loadServiceScanPatterns(ctx, q)
	if err != nil {
		return nil, err
	}
	var queue []serviceScanTarget
	for _, t := range targets {
		allow, reason := decideServiceScan(true, patterns, t.Host)
		if !allow {
			if err := recordServiceScanSkip(ctx, q, t, reason); err != nil {
				return nil, err
			}
			continue
		}
		fronted, err := lookupCDNFronted(ctx, q, t)
		if err != nil {
			return nil, err
		}
		if fronted {
			if err := recordServiceScanSkip(ctx, q, t, skipServiceScanCDN); err != nil {
				return nil, err
			}
			continue
		}
		queue = append(queue, t)
	}
	return queue, nil
}

// runOriginServiceScan 对 planOriginServiceScan 给出的源站调用 probe（T51 接入点）。CDN 前置资产不会调用 probe。
func runOriginServiceScan(ctx context.Context, q serviceScanQ, targets []serviceScanTarget, probe ServiceProbeFunc) error {
	queue, err := planOriginServiceScan(ctx, q, targets)
	if err != nil {
		return err
	}
	for _, t := range queue {
		if probe != nil {
			if err := probe(ctx, normalizeHost(t.Host)); err != nil {
				return err
			}
		}
	}
	return nil
}

func enrichOriginServiceScanDecision(ctx context.Context, q serviceScanQ, out map[string]any) error {
	host, _ := out["host"].(string)
	fronted, err := lookupCDNFronted(ctx, q, serviceScanTarget{Host: host})
	if err != nil {
		return err
	}
	out["cdn_fronted"] = fronted
	if fronted {
		if allow, _ := out["allow"].(bool); allow {
			out["allow"] = false
			out["reason"] = skipServiceScanCDN
		}
	}
	return nil
}
