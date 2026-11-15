package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

// 默认常见端口（SPEC 点名的 10 种后端服务，非全 65535 端口）。
var defaultServiceScanPorts = []int{3306, 5432, 6379, 27017, 9200, 9092, 2379, 8848, 5672, 11211}

// 明确列出的最小无害握手字节（不认证、不查询、不写入）。
// PostgreSQL SSLRequest：8 字节魔术 80877103，只询问是否支持 TLS，不是 StartupMessage。
var probePostgreSQLSSLRequest = []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}

// AMQP 协议头：客户端前言，不是 connection.start-ok / 登录 / publish / consume。
var probeAMQPProtocolHeader = []byte{'A', 'M', 'Q', 'P', 0x00, 0x00, 0x09, 0x01}

// serviceFingerprintRule 是 T51 识别规则行。T53 可用 YAML 整表替换，扫描侧只读这张表。
type serviceFingerprintRule struct {
	Name      string
	Ports     []int
	Probe     []byte
	BannerRE  string
	VersionRE string
}

func builtinServiceFingerprintRules() []serviceFingerprintRule {
	return []serviceFingerprintRule{
		{Name: "mysql", Ports: []int{3306}, BannerRE: `(?i)mysql_native_password|mariadb|\x00mysql`, VersionRE: `([0-9]+\.[0-9]+\.[0-9]+)`},
		{Name: "postgresql", Ports: []int{5432}, Probe: probePostgreSQLSSLRequest, BannerRE: `^[SN]$`},
		{Name: "redis", Ports: []int{6379}, BannerRE: `(?i)-NOAUTH|redis_version|-DENIED Redis|(?m)^-ERR .+[Rr]edis`},
		{Name: "mongodb", Ports: []int{27017}, BannerRE: `(?i)mongo`},
		{Name: "elasticsearch", Ports: []int{9200}, BannerRE: `(?i)elasticsearch`, VersionRE: `([0-9]+\.[0-9]+\.[0-9]+)`},
		{Name: "kafka", Ports: []int{9092}, BannerRE: `(?i)kafka`},
		{Name: "etcd", Ports: []int{2379}, BannerRE: `(?i)etcd`, VersionRE: `([0-9]+\.[0-9]+)`},
		{Name: "nacos", Ports: []int{8848}, BannerRE: `(?i)nacos`},
		{Name: "rabbitmq", Ports: []int{5672}, Probe: probeAMQPProtocolHeader, BannerRE: `(?i)AMQP|rabbitmq`},
		{Name: "memcached", Ports: []int{11211}, BannerRE: `(?i)memcached`},
	}
}

// serviceFingerprintProbes 返回规则表中全部非空探测字节（完整集合，供测试断言）。
func serviceFingerprintProbes() [][]byte {
	seen := map[string]struct{}{}
	var out [][]byte
	for _, r := range builtinServiceFingerprintRules() {
		if len(r.Probe) == 0 {
			continue
		}
		k := string(r.Probe)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, append([]byte(nil), r.Probe...))
	}
	return out
}

// serviceDialer 可注入。测试把主机名转到 127.0.0.1 假服务，不走真实 DNS/外网。
type serviceDialer func(ctx context.Context, network, host string, port int) (net.Conn, error)

type serviceFingerprintConfig struct {
	Ports   []int
	RPS     float64
	Timeout time.Duration
	Dialer  serviceDialer
	limiter *rate.Limiter
}

func defaultServiceDialer(timeout time.Duration) serviceDialer {
	d := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, host string, port int) (net.Conn, error) {
		if network == "" {
			network = "tcp"
		}
		return d.DialContext(ctx, network, net.JoinHostPort(host, strconv.Itoa(port)))
	}
}

func serviceFingerprintConfigFromEnv() (serviceFingerprintConfig, error) {
	c := serviceFingerprintConfig{Ports: append([]int(nil), defaultServiceScanPorts...), RPS: 2, Timeout: 2 * time.Second}
	if s := strings.TrimSpace(os.Getenv("SERVICE_SCAN_PORTS")); s != "" {
		var ports []int
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if strings.ContainsAny(part, "-:/") {
				return c, fmt.Errorf("SERVICE_SCAN_PORTS: invalid %q (comma-separated ports only, no ranges)", part)
			}
			n, err := strconv.Atoi(part)
			if err != nil || n <= 0 || n > 65535 {
				return c, fmt.Errorf("SERVICE_SCAN_PORTS: invalid %q", part)
			}
			ports = append(ports, n)
		}
		if len(ports) == 0 {
			return c, fmt.Errorf("SERVICE_SCAN_PORTS: empty")
		}
		c.Ports = ports
	}
	if s := os.Getenv("SERVICE_SCAN_RPS"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if !validPositive(f, err) {
			return c, fmt.Errorf("SERVICE_SCAN_RPS: invalid %q", s)
		}
		c.RPS = f
	}
	if s := os.Getenv("SERVICE_SCAN_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("SERVICE_SCAN_TIMEOUT: invalid %q", s)
		}
		c.Timeout = d
	}
	return c, nil
}

func (c *serviceFingerprintConfig) ready() error {
	if len(c.Ports) == 0 {
		c.Ports = append([]int(nil), defaultServiceScanPorts...)
	}
	if c.RPS <= 0 {
		c.RPS = 2
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Second
	}
	if c.Dialer == nil {
		c.Dialer = defaultServiceDialer(c.Timeout)
	}
	if c.limiter == nil {
		c.limiter = rate.NewLimiter(rate.Limit(c.RPS), 1)
	}
	return nil
}

// fingerprintOriginServices 经 planOriginServiceScan / runOriginServiceScan 接入：
// 关 T49 开关时不拨号；CDN 前置与超范围由既有门控跳过。不改 T49/T50 门控实现。
func fingerprintOriginServices(ctx context.Context, q serviceScanQ, cfg serviceFingerprintConfig, targets []serviceScanTarget) error {
	if err := cfg.ready(); err != nil {
		return err
	}
	byHost := map[string]int64{}
	for _, t := range targets {
		h := normalizeHost(t.Host)
		if h == "" {
			continue
		}
		byHost[h] = t.AssetID
	}
	return runOriginServiceScan(ctx, q, targets, func(ctx context.Context, host string) error {
		return scanHostServices(ctx, q, cfg, host, byHost[normalizeHost(host)])
	})
}

func scanHostServices(ctx context.Context, q serviceScanQ, cfg serviceFingerprintConfig, host string, assetID int64) error {
	host = normalizeHost(host)
	// 复用 T49 修复后的主机校验（规范 IP、inet_aton 简写、全数字末级标签），不改门控实现。
	if host == "" || rejectedServiceScanHost(host) {
		return nil
	}
	rules := builtinServiceFingerprintRules()
	for _, port := range cfg.Ports {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cfg.limiter != nil {
			if err := cfg.limiter.Wait(ctx); err != nil {
				return err
			}
		}
		open, banner := probeServicePort(ctx, cfg, host, port, ruleForPort(rules, port))
		name, version := matchServiceFingerprint(rules, port, banner)
		if err := upsertAssetService(ctx, q, assetID, host, port, open, name, version, sanitizeServiceBanner(banner)); err != nil {
			return err
		}
	}
	return nil
}

func ruleForPort(rules []serviceFingerprintRule, port int) serviceFingerprintRule {
	for _, r := range rules {
		for _, p := range r.Ports {
			if p == port {
				return r
			}
		}
	}
	return serviceFingerprintRule{}
}

func probeServicePort(ctx context.Context, cfg serviceFingerprintConfig, host string, port int, rule serviceFingerprintRule) (bool, []byte) {
	dialCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	conn, err := cfg.Dialer(dialCtx, "tcp", host, port)
	if err != nil {
		return false, nil
	}
	defer conn.Close()
	deadline := time.Now().Add(cfg.Timeout)
	_ = conn.SetDeadline(deadline)
	if len(rule.Probe) > 0 {
		if _, err := conn.Write(rule.Probe); err != nil {
			// 已连上但写入失败仍视为开放，无 banner。
			return true, nil
		}
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if n > 0 {
		return true, buf[:n]
	}
	if err != nil && err != io.EOF {
		// 超时或对端立即断开：已建立过连接则视为开放但无 banner（拒绝/超时在 Dial 失败时已记关闭）。
		// 读超时：连接已建立，算开放。
		if ne, ok := err.(net.Error); ok && ne.Timeout() && len(rule.Probe) == 0 {
			return true, nil
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() && len(rule.Probe) > 0 {
			// 发了握手仍无响应：端口开着但无法识别。
			return true, nil
		}
	}
	return true, nil
}

func matchServiceFingerprint(rules []serviceFingerprintRule, port int, banner []byte) (name, version string) {
	if len(banner) == 0 {
		return "", ""
	}
	text := bannerText(banner)
	var fallbackName, fallbackVer string
	for _, r := range rules {
		if r.BannerRE == "" {
			continue
		}
		re, err := regexp.Compile(r.BannerRE)
		if err != nil || (!re.Match(banner) && !re.MatchString(text)) {
			continue
		}
		ver := ""
		if r.VersionRE != "" {
			if vre, err := regexp.Compile(r.VersionRE); err == nil {
				if m := vre.FindStringSubmatch(text); len(m) > 1 {
					ver = m[1]
				} else if m := vre.FindStringSubmatch(string(banner)); len(m) > 1 {
					ver = m[1]
				}
			}
		}
		portHit := false
		for _, p := range r.Ports {
			if p == port {
				portHit = true
				break
			}
		}
		if portHit {
			return r.Name, ver
		}
		if fallbackName == "" {
			fallbackName, fallbackVer = r.Name, ver
		}
	}
	return fallbackName, fallbackVer
}

func bannerText(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		if c >= 32 && c < 127 {
			s.WriteByte(c)
		} else if c == '\n' || c == '\r' || c == '\t' {
			s.WriteByte(' ')
		}
	}
	return s.String()
}

func sanitizeServiceBanner(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	const max = 512
	if len(b) > max {
		b = b[:max]
	}
	var s strings.Builder
	for i := 0; i < len(b); {
		r, sz := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && sz == 1 {
			s.WriteByte('.')
			i++
			continue
		}
		i += sz
		if r < 32 || r == 127 {
			s.WriteByte('.')
			continue
		}
		s.WriteRune(r)
	}
	out := s.String()
	out = ipv4Pattern.ReplaceAllString(out, "[ip]")
	out = ipv6Pattern.ReplaceAllString(out, "[ip]")
	return out
}

var (
	ipv4Pattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Pattern = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b`)
)

func upsertAssetService(ctx context.Context, q serviceScanQ, assetID int64, host string, port int, open bool, name, version, banner string) error {
	var aid any
	if assetID > 0 {
		aid = assetID
	}
	_, err := q.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, name, version, banner, probed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (host, port) DO UPDATE SET
			asset_id = EXCLUDED.asset_id,
			open = EXCLUDED.open,
			name = EXCLUDED.name,
			version = EXCLUDED.version,
			banner = EXCLUDED.banner,
			probed_at = EXCLUDED.probed_at`,
		aid, host, port, open, name, version, banner)
	return err
}

// serviceScanHandler 是扫描类型 service 的 worker 入口：对根域资产做端口指纹。
// hosts 为空时从该根域已有资产加载（跳过泛解析）；由 T49/T50 门控，关闭开关时不拨号、任务仍标记 done。
func serviceScanHandler(d *pgxpool.Pool, cfg serviceFingerprintConfig) ScanHandler {
	return func(ctx context.Context, scanID int64, hosts []string) ([]string, error) {
		var rootID int64
		if err := d.QueryRow(ctx, `SELECT root_id FROM scan_tasks WHERE id = $1`, scanID).Scan(&rootID); err != nil {
			return nil, err
		}
		targets, err := loadServiceScanTargets(ctx, d, rootID, hosts)
		if err != nil {
			return nil, err
		}
		if err := fingerprintOriginServices(ctx, d, cfg, targets); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// listRootServiceHosts 供手动触发时写入任务 payload，以便 T40 按 HOST_CHUNK_SIZE 分片。
func listRootServiceHosts(ctx context.Context, q serviceScanQ, rootID int64) ([]string, error) {
	targets, err := loadServiceScanTargets(ctx, q, rootID, nil)
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(targets))
	for _, t := range targets {
		hosts = append(hosts, t.Host)
	}
	return hosts, nil
}

func loadServiceScanTargets(ctx context.Context, q serviceScanQ, rootID int64, hosts []string) ([]serviceScanTarget, error) {
	sql := `SELECT id, subdomain FROM assets WHERE root_id = $1 AND NOT wildcard AND NOT parent_wildcard`
	var args []any
	args = append(args, rootID)
	if len(hosts) > 0 {
		norm := make([]string, 0, len(hosts))
		for _, h := range hosts {
			h = normalizeHost(h)
			if h == "" || rejectedServiceScanHost(h) {
				continue
			}
			norm = append(norm, h)
		}
		if len(norm) == 0 {
			return nil, nil
		}
		sql += ` AND subdomain = ANY($2)`
		args = append(args, norm)
	}
	sql += ` ORDER BY id`
	rs, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []serviceScanTarget
	for rs.Next() {
		var t serviceScanTarget
		if err := rs.Scan(&t.AssetID, &t.Host); err != nil {
			return nil, err
		}
		t.Host = normalizeHost(t.Host)
		if t.Host == "" || rejectedServiceScanHost(t.Host) {
			continue
		}
		out = append(out, t)
	}
	return out, rs.Err()
}
