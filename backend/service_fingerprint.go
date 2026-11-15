package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

// T55/T56：探测结果分档。none=TCP 可达但无响应；banner=初始响应；probe=YAML 探测响应；
// reverse_proxy=HTTP 400/403 + 反代/CDN 特征，不算已确认服务。
const (
	serviceResponseNone         = "none"
	serviceResponseBanner       = "banner"
	serviceResponseProbe        = "probe"
	serviceResponseReverseProxy = "reverse_proxy"
)

var (
	serviceReverseProxyStatusRE = regexp.MustCompile(`(?i)^\s*HTTP/\d(?:\.\d)?\s+(?:400|403)(?:\s|$)`)
	// 脱敏后 CRLF 变为连续点，所以边界同时接受空白与点。
	serviceReverseProxyHeaderRE = regexp.MustCompile(`(?i)(?:^|[.\s])(?:server\s*:\s*(?:openresty|nginx|tengine)(?:[\s/.]|$)|via\s*:\s*[^.\s]*CHN-|x-ccdn-forbid-code\s*:|x-cache\s*:|x-cdn\s*:)`)
)

// isReverseProxyResponse 判断 HTTP 400/403 + 反代/CDN 特征（T56）。原始响应与入库后的脱敏摘要都适用。
func isReverseProxyResponse(banner string) bool {
	if banner == "" {
		return false
	}
	return serviceReverseProxyStatusRE.MatchString(banner) && serviceReverseProxyHeaderRE.MatchString(banner)
}

// serviceResponseKind 按响应内容分档；probe=true 表示该响应来自 YAML 声明的探测报文。
func serviceResponseKind(open bool, banner string, probe bool) string {
	if !open || banner == "" {
		return serviceResponseNone
	}
	if isReverseProxyResponse(banner) {
		return serviceResponseReverseProxy
	}
	if probe {
		return serviceResponseProbe
	}
	return serviceResponseBanner
}

func serviceResponseVerified(kind string) bool {
	return kind == serviceResponseBanner || kind == serviceResponseProbe
}

// serviceDialer 可注入。测试把主机名转到 127.0.0.1 假服务，不走真实 DNS/外网。
type serviceDialer func(ctx context.Context, network, host string, port int) (net.Conn, error)

type serviceFingerprintConfig struct {
	Ports   []int
	RPS     float64
	Timeout time.Duration
	Dialer  serviceDialer // 测试注入：按主机名转到假服务。生产路径保持为 nil，只拨判定所得 IP。
	Resolve serviceIPResolver
	DialIP  serviceIPDialer // 测试注入生产拨号路径；默认 defaultServiceDialer，只接受字面 IP。
	Rules   *serviceFingerprintRuleSet
	limiter *rate.Limiter
}

// defaultServiceDialer 只拨字面 IP，自己不再解析 DNS。传入主机名则拒绝。
func defaultServiceDialer(timeout time.Duration) serviceDialer {
	d := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, addr string, port int) (net.Conn, error) {
		if network == "" {
			network = "tcp"
		}
		ip := net.ParseIP(addr)
		if ip == nil {
			return nil, errServiceDialNotIP
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	}
}

func serviceFingerprintConfigFromEnv() (serviceFingerprintConfig, error) {
	rules, err := loadServiceFingerprintRules(os.Getenv("SERVICE_FINGERPRINT_RULES"))
	if err != nil {
		return serviceFingerprintConfig{}, err
	}
	// 默认扫描端口来自 YAML 规则声明，新增服务只改 YAML。
	c := serviceFingerprintConfig{Ports: rules.ports(), RPS: 2, Timeout: 2 * time.Second, Rules: rules}
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
	if c.Rules == nil {
		rules, err := loadServiceFingerprintRules(os.Getenv("SERVICE_FINGERPRINT_RULES"))
		if err != nil {
			return err
		}
		c.Rules = rules
	}
	if len(c.Ports) == 0 {
		c.Ports = c.Rules.ports()
	}
	if c.RPS <= 0 {
		c.RPS = 2
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Second
	}
	// 生产不在这里填默认拨号器：无注入 Dialer 时只拨 CDN 判定解析到的 IP。
	if c.limiter == nil {
		c.limiter = sharedServiceLimiter(c.RPS)
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
	dialIPs, err := prepareServiceDialIPs(ctx, q, cfg, serviceScanTarget{AssetID: assetID, Host: host})
	if err != nil {
		return err
	}
	if cfg.Dialer == nil && len(dialIPs) == 0 {
		return nil
	}
	rules := cfg.Rules
	reached := false
	reverseProxyRecorded := false
	for _, port := range cfg.Ports {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cfg.limiter != nil {
			if err := cfg.limiter.Wait(ctx); err != nil {
				return err
			}
		}
		open, banner, kind := probeServicePort(ctx, cfg, host, port, rules.ruleForPort(port), dialIPs)
		name, version, _ := rules.match(port, banner)
		if err := upsertAssetService(ctx, q, assetID, host, port, open, kind, name, version, sanitizeServiceBanner(banner)); err != nil {
			return err
		}
		// T56：反代/CDN 边缘响应不生成候选，记一次跳过原因（每主机每轮一次）。
		if kind == serviceResponseReverseProxy && !reverseProxyRecorded {
			if err := recordServiceScanSkip(ctx, q, serviceScanTarget{AssetID: assetID, Host: host}, skipServiceScanReverseProxy); err != nil {
				return err
			}
			reverseProxyRecorded = true
		}
		// 服务有 banner 或识别成功说明可达：标 alive，避免默认组件列表把 unknown 主机藏掉（架构 11-15 定）。
		// 端口关闭或开放但无 banner 时不改 alive。
		if len(banner) > 0 || name != "" {
			reached = true
		}
	}
	if reached && assetID != 0 {
		if err := markAssetAliveFromService(ctx, q, assetID); err != nil {
			return err
		}
	}
	return nil
}

// markAssetAliveFromService 只把主机提升为 alive（写 last_alive_at、清空 dead_reason），从不因服务扫描改为 dead。
// 已 alive 不再改。不产生 recovered 事件（未要求服务扫描改 T21 事件流）。
func markAssetAliveFromService(ctx context.Context, q serviceScanQ, assetID int64) error {
	_, err := q.Exec(ctx, `UPDATE assets SET alive = 'alive', last_alive_at = now(), dead_reason = NULL
		WHERE id = $1 AND alive IS DISTINCT FROM 'alive'`, assetID)
	return err
}

// probeServicePort 只发送 YAML 声明的探测报文：没有 probes 的端口只读初始 banner，不写。
// 每个 probe 用一次独立连接；响应同时匹配 expect 与规则 banner 时停止并采用该 banner，
// 否则保留最后一段 banner 供 T52 归入未识别，是否识别仍由规则的 banner 正则决定。
// 返回 response_kind（T55）：初始响应=banner、YAML 探测响应=probe、连上无响应=none、反代/CDN=reverse_proxy。
func probeServicePort(ctx context.Context, cfg serviceFingerprintConfig, host string, port int, rule serviceFingerprintRule, ips []net.IP) (bool, []byte, string) {
	if len(rule.Probes) == 0 {
		ok, banner := readServicePort(ctx, cfg, host, port, nil, ips)
		return ok, banner, serviceResponseKind(ok, bannerText(banner), false)
	}
	open := false
	var last []byte
	for _, p := range rule.Probes {
		ok, banner := readServicePort(ctx, cfg, host, port, p.Bytes, ips)
		if !ok {
			// 拨号失败：端口关闭，不再尝试后续报文。
			return false, nil, serviceResponseNone
		}
		open = true
		if len(banner) == 0 {
			continue
		}
		last = banner
		text := bannerText(banner)
		if isReverseProxyResponse(text) {
			// 反代/CDN 边缘响应继续发探测也只会重复 400/403，直接采用该响应。
			return true, banner, serviceResponseReverseProxy
		}
		expectOK := p.Expect != nil && (p.Expect.Match(banner) || p.Expect.MatchString(text))
		bannerOK := rule.BannerRE != nil && (rule.BannerRE.Match(banner) || rule.BannerRE.MatchString(text))
		if expectOK && bannerOK {
			return true, banner, serviceResponseProbe
		}
	}
	return open, last, serviceResponseKind(open, bannerText(last), true)
}

func readServicePort(ctx context.Context, cfg serviceFingerprintConfig, host string, port int, probe []byte, ips []net.IP) (bool, []byte) {
	dialCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	conn, err := dialServicePort(dialCtx, cfg, host, port, ips)
	if err != nil {
		return false, nil
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
	if len(probe) > 0 {
		if _, err := conn.Write(probe); err != nil {
			// 已连上但写入失败仍视为开放，无 banner。
			return true, nil
		}
	}
	buf := make([]byte, 1024)
	n, _ := conn.Read(buf)
	if n > 0 {
		return true, buf[:n]
	}
	// 超时或对端立即断开：已建立过连接则视为开放但无 banner（拒绝/超时在 Dial 失败时已记关闭）。
	return true, nil
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

func upsertAssetService(ctx context.Context, q serviceScanQ, assetID int64, host string, port int, open bool, kind, name, version, banner string) error {
	var aid any
	if assetID > 0 {
		aid = assetID
	}
	if kind == "" {
		kind = serviceResponseNone
	}
	_, err := q.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, verified, response_kind, name, version, banner, probed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (host, port) DO UPDATE SET
			asset_id = EXCLUDED.asset_id,
			open = EXCLUDED.open,
			verified = EXCLUDED.verified,
			response_kind = EXCLUDED.response_kind,
			name = EXCLUDED.name,
			version = EXCLUDED.version,
			banner = EXCLUDED.banner,
			probed_at = EXCLUDED.probed_at`,
		aid, host, port, open, open && serviceResponseVerified(kind), kind, name, version, banner)
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
		// 用局部副本 ready，避免多个 service 任务并发时改同一份 cfg。
		runCfg := cfg
		if err := runCfg.ready(); err != nil {
			return nil, err
		}
		if err := fingerprintOriginServices(ctx, d, runCfg, targets); err != nil {
			return nil, err
		}
		return nil, syncAssetServicesToComponents(ctx, d, targets, runCfg.Rules)
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
