package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// T49 跳过原因（稳定字符串，测试锁定）。
const (
	skipServiceScanDisabled     = "service_scan_disabled"
	skipServiceScanOutOfScope   = "out_of_scope"
	skipServiceScanReverseProxy = "reverse_proxy" // T56：HTTP 400/403 + 反代/CDN 特征
)

// ServiceProbeFunc 是 T51 的接入点：仅在开关开启且主机落在授权范围时被调用。不得在此实现端口扫描。
type ServiceProbeFunc func(ctx context.Context, host string) error

// serviceScanTarget 是一次非 Web 探测的候选（host 为域名，不含 IP）。
type serviceScanTarget struct {
	AssetID int64
	Host    string
}

type serviceScanQ interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func envServiceScanEnabled() bool {
	s := strings.TrimSpace(os.Getenv("SERVICE_SCAN_ENABLED"))
	if s == "" {
		return false
	}
	on, err := strconv.ParseBool(s)
	return err == nil && on
}

// loadServiceScanEnabled：已通过 REST/Web 写入则以库为准；否则读环境变量，默认关。
func loadServiceScanEnabled(ctx context.Context, q serviceScanQ) (bool, error) {
	var enabled bool
	err := q.QueryRow(ctx, `SELECT enabled FROM service_scan_settings WHERE id = 1`).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return envServiceScanEnabled(), nil
	}
	return enabled, err
}

func loadServiceScanPatterns(ctx context.Context, q serviceScanQ) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT pattern FROM service_scan_scopes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// isIPHost 识别规范 IP（net.ParseIP）以及 libc inet_aton 能解析的 IPv4 简写/八进制/十六进制。
func isIPHost(s string) bool {
	s = normalizeHost(s)
	s = strings.TrimPrefix(s, "*.")
	s = strings.Trim(s, "[]")
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	if net.ParseIP(s) != nil {
		return true
	}
	return parseInetAton(s) != nil
}

// hasAllNumericLastLabel 拒绝末级标签全是数字的写法（如 127.1、1.2.3.4.5）。合法域名末级不会是纯数字。
func hasAllNumericLastLabel(s string) bool {
	s = normalizeHost(s)
	s = strings.TrimPrefix(s, "*.")
	if s == "" {
		return false
	}
	last := s
	if i := strings.LastIndex(s, "."); i >= 0 {
		last = s[i+1:]
	}
	if last == "" {
		return false
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func rejectedServiceScanHost(s string) bool {
	return isIPHost(s) || hasAllNumericLastLabel(s)
}

// parseInetAton 按 inet_aton(3) 解析 1–4 段十进制/八进制/十六进制 IPv4，无法解析时返回 nil。
func parseInetAton(s string) net.IP {
	if s == "" || strings.ContainsAny(s, ":[]/%") {
		return nil
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return nil
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := parseInetAtonPart(p)
		if !ok {
			return nil
		}
		nums[i] = n
	}
	var a, b, c, d uint64
	switch len(nums) {
	case 1:
		if nums[0] > 0xffffffff {
			return nil
		}
		a, b, c, d = nums[0]>>24, (nums[0]>>16)&0xff, (nums[0]>>8)&0xff, nums[0]&0xff
	case 2:
		if nums[0] > 0xff || nums[1] > 0xffffff {
			return nil
		}
		a = nums[0]
		b, c, d = (nums[1]>>16)&0xff, (nums[1]>>8)&0xff, nums[1]&0xff
	case 3:
		if nums[0] > 0xff || nums[1] > 0xff || nums[2] > 0xffff {
			return nil
		}
		a, b = nums[0], nums[1]
		c, d = (nums[2]>>8)&0xff, nums[2]&0xff
	default:
		for _, n := range nums {
			if n > 0xff {
				return nil
			}
		}
		a, b, c, d = nums[0], nums[1], nums[2], nums[3]
	}
	return net.IPv4(byte(a), byte(b), byte(c), byte(d))
}

func parseInetAtonPart(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	if len(p) >= 2 && p[0] == '0' && (p[1] == 'x' || p[1] == 'X') {
		if len(p) == 2 {
			return 0, false
		}
		n, err := strconv.ParseUint(p[2:], 16, 32)
		return n, err == nil
	}
	if p[0] == '0' && len(p) > 1 {
		n, err := strconv.ParseUint(p, 8, 32)
		return n, err == nil
	}
	n, err := strconv.ParseUint(p, 10, 32)
	return n, err == nil
}

// hostInServiceScope：登记域本身及其子域视为范围内的资产集合；*.domain 只匹配子域。
func hostInServiceScope(host string, patterns []string) bool {
	host = normalizeHost(host)
	if host == "" || rejectedServiceScanHost(host) {
		return false
	}
	for _, p := range patterns {
		p = normalizeHost(p)
		if p == "" {
			continue
		}
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == p || strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

func decideServiceScan(enabled bool, patterns []string, host string) (allow bool, reason string) {
	if !enabled {
		return false, skipServiceScanDisabled
	}
	if !hostInServiceScope(host, patterns) {
		return false, skipServiceScanOutOfScope
	}
	return true, ""
}

func recordServiceScanSkip(ctx context.Context, q serviceScanQ, t serviceScanTarget, reason string) error {
	var aid any
	if t.AssetID > 0 {
		aid = t.AssetID
	}
	_, err := q.Exec(ctx, `INSERT INTO service_scan_skips (host, asset_id, reason) VALUES ($1, $2, $3)`,
		normalizeHost(t.Host), aid, reason)
	return err
}

// runServiceScan 是供 T51 调用的唯一门控入口：关闭则完全不发起探测；超范围跳过并记录原因；在范围内才调用 probe。
func runServiceScan(ctx context.Context, q serviceScanQ, targets []serviceScanTarget, probe ServiceProbeFunc) error {
	enabled, err := loadServiceScanEnabled(ctx, q)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	patterns, err := loadServiceScanPatterns(ctx, q)
	if err != nil {
		return err
	}
	for _, t := range targets {
		allow, reason := decideServiceScan(true, patterns, t.Host)
		if !allow {
			if err := recordServiceScanSkip(ctx, q, t, reason); err != nil {
				return err
			}
			continue
		}
		if probe != nil {
			if err := probe(ctx, normalizeHost(t.Host)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *api) getServiceScan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	enabled, err := loadServiceScanEnabled(ctx, a.db)
	if err != nil {
		dbErr(w, err)
		return
	}
	items, err := rowsJSON(a.db.Query(ctx, `SELECT id, pattern, created_at FROM service_scan_scopes ORDER BY id`))
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": enabled, "items": items})
}

func (a *api) patchServiceScan(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Enabled == nil {
		writeErr(w, 400, "enabled required")
		return
	}
	if _, err := a.db.Exec(r.Context(), `INSERT INTO service_scan_settings (id, enabled, updated_at) VALUES (1, $1, now())
		ON CONFLICT (id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`, *b.Enabled); err != nil {
		dbErr(w, err)
		return
	}
	a.getServiceScan(w, r)
}

func (a *api) createServiceScanScope(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Pattern string `json:"pattern"`
	}
	if !decode(w, r, &b) {
		return
	}
	pat := normalizeHost(b.Pattern)
	if rejectedServiceScanHost(pat) || !validScopePattern(pat) {
		writeErr(w, 400, "invalid pattern: use domain or *.domain")
		return
	}
	var id int64
	var created any
	if err := a.db.QueryRow(r.Context(), `INSERT INTO service_scan_scopes (pattern) VALUES ($1) RETURNING id, created_at`, pat).
		Scan(&id, &created); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "pattern": pat, "created_at": created})
}

func (a *api) deleteServiceScanScope(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	tag, err := a.db.Exec(r.Context(), `DELETE FROM service_scan_scopes WHERE id = $1`, id)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "not found")
		return
	}
	w.WriteHeader(204)
}

func (a *api) serviceScanDecision(ctx context.Context, host string) (map[string]any, error) {
	host = normalizeHost(host)
	if host == "" || !validDomain(host) || rejectedServiceScanHost(host) {
		return nil, errf(400, "invalid host")
	}
	enabled, err := loadServiceScanEnabled(ctx, a.db)
	if err != nil {
		return nil, err
	}
	patterns, err := loadServiceScanPatterns(ctx, a.db)
	if err != nil {
		return nil, err
	}
	authorized := hostInServiceScope(host, patterns)
	allow, reason := decideServiceScan(enabled, patterns, host)
	return map[string]any{
		"host":       host,
		"enabled":    enabled,
		"authorized": authorized,
		"allow":      allow,
		"reason":     reason,
	}, nil
}
