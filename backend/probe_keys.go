package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var probeProviders = []string{"fofa", "quake", "hunter"}

// maskKey：页面/API 只露后 4 位；空串返回空。
func maskKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 4 {
		return "****"
	}
	return "****" + k[len(k)-4:]
}

func envProbeKey(provider string) string {
	switch provider {
	case "fofa":
		return strings.TrimSpace(os.Getenv("FOFA_KEY"))
	case "quake":
		return strings.TrimSpace(os.Getenv("QUAKE_KEY"))
	case "hunter":
		return strings.TrimSpace(os.Getenv("HUNTER_KEY"))
	default:
		return ""
	}
}

func dbProbeKey(ctx context.Context, db *pgxpool.Pool, provider string) (string, error) {
	var v string
	err := db.QueryRow(ctx, `SELECT key_value FROM probe_keys WHERE provider = $1`, provider).Scan(&v)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(v), nil
}

// effectiveProbeKey：网页配置优先，否则环境变量。source 为 db|env|none。
func effectiveProbeKey(ctx context.Context, db *pgxpool.Pool, provider string) (key, source string, err error) {
	dbk, err := dbProbeKey(ctx, db, provider)
	if err != nil {
		return "", "", err
	}
	if dbk != "" {
		return dbk, "db", nil
	}
	if ek := envProbeKey(provider); ek != "" {
		return ek, "env", nil
	}
	return "", "none", nil
}

// overlayProbeKeys：把库里的 key 盖到配置上（网页优先）；env 已在 base 里。
func overlayProbeKeys(ctx context.Context, db *pgxpool.Pool, c *passiveConfig) error {
	rows, err := db.Query(ctx, `SELECT provider, key_value FROM probe_keys`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p, v string
		if err := rows.Scan(&p, &v); err != nil {
			return err
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch p {
		case "fofa":
			c.FofaKey = v
		case "quake":
			c.QuakeKey = v
		case "hunter":
			c.HunterKey = v
		}
	}
	return rows.Err()
}

// passiveHandlerFromConfig：每次扫描现取 key，网页改完下次扫描生效、无需重启（T30）。
func passiveHandlerFromConfig(d *pgxpool.Pool, base passiveConfig) ScanHandler {
	return func(ctx context.Context, scanID int64, hosts []string) ([]string, error) {
		c := base
		if err := overlayProbeKeys(ctx, d, &c); err != nil {
			return nil, err
		}
		return passiveHandler(d, passiveSources(c))(ctx, scanID, hosts)
	}
}

func defaultProbeBase(provider string) string {
	switch provider {
	case "fofa":
		return "https://fofa.info"
	case "quake":
		return "https://quake.360.net"
	case "hunter":
		return "https://hunter.qianxin.com"
	default:
		return ""
	}
}

// checkProbeAccount：只调账户信息接口（只读、不耗查询额度）；base 可换 httptest。
// 错误信息不得包含 key。
func checkProbeAccount(ctx context.Context, client *http.Client, provider, key, base string) error {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	base = strings.TrimRight(base, "/")
	var req *http.Request
	var err error
	switch provider {
	case "fofa":
		// 与搜索一致：仅 key（个人 API）；账户信息 GET /api/v1/info/my
		u := base + "/api/v1/info/my?key=" + url.QueryEscape(key)
		req, err = http.NewRequestWithContext(ctx, "GET", u, nil)
	case "quake":
		req, err = http.NewRequestWithContext(ctx, "GET", base+"/api/v3/user/info", nil)
		if err == nil {
			req.Header.Set("X-QuakeToken", key)
		}
	case "hunter":
		u := base + "/openApi/userInfo?api-key=" + url.QueryEscape(key)
		req, err = http.NewRequestWithContext(ctx, "GET", u, nil)
	default:
		return fmt.Errorf("unknown provider")
	}
	if err != nil {
		return fmt.Errorf("build request failed")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: unreachable", provider)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", provider, resp.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return fmt.Errorf("%s: invalid json", provider)
	}
	switch provider {
	case "fofa":
		if errFlag, _ := raw["error"].(bool); errFlag {
			msg, _ := raw["errmsg"].(string)
			if msg == "" {
				msg = "account invalid"
			}
			return fmt.Errorf("fofa: %s", msg)
		}
	case "quake":
		if fmt.Sprint(raw["code"]) != "0" {
			msg, _ := raw["message"].(string)
			if msg == "" {
				msg = "auth failed"
			}
			return fmt.Errorf("quake: %s", msg)
		}
	case "hunter":
		// 成功 code 为 200（数字或字符串）
		if fmt.Sprint(raw["code"]) != "200" {
			msg, _ := raw["message"].(string)
			if msg == "" {
				msg, _ = raw["msg"].(string)
			}
			if msg == "" {
				msg = "auth failed"
			}
			return fmt.Errorf("hunter: %s", msg)
		}
	}
	return nil
}

func pathProvider(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.ToLower(strings.TrimSpace(r.PathValue("provider")))
	for _, x := range probeProviders {
		if p == x {
			return p, true
		}
	}
	writeErr(w, 400, "provider must be fofa, quake, or hunter")
	return "", false
}

func (a *api) probeBase(provider string) string {
	if a.probeBases != nil {
		if b := a.probeBases[provider]; b != "" {
			return b
		}
	}
	return defaultProbeBase(provider)
}

func (a *api) listProbeKeys(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0, len(probeProviders))
	for _, p := range probeProviders {
		key, source, err := effectiveProbeKey(r.Context(), a.db, p)
		if err != nil {
			dbErr(w, err)
			return
		}
		items = append(items, map[string]any{
			"provider":   p,
			"configured": key != "",
			"source":     source,
			"mask":       maskKey(key),
		})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *api) putProbeKey(w http.ResponseWriter, r *http.Request) {
	provider, ok := pathProvider(w, r)
	if !ok {
		return
	}
	var b struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &b) {
		return
	}
	key := strings.TrimSpace(b.Key)
	if key == "" {
		writeErr(w, 400, "key required")
		return
	}
	if len(key) > 512 {
		writeErr(w, 400, "key too long")
		return
	}
	_, err := a.db.Exec(r.Context(), `INSERT INTO probe_keys (provider, key_value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (provider) DO UPDATE SET key_value = EXCLUDED.key_value, updated_at = now()`, provider, key)
	if err != nil {
		dbErr(w, err)
		return
	}
	// 响应绝不回说明文
	writeJSON(w, 200, map[string]any{
		"provider":   provider,
		"configured": true,
		"source":     "db",
		"mask":       maskKey(key),
	})
}

func (a *api) deleteProbeKey(w http.ResponseWriter, r *http.Request) {
	provider, ok := pathProvider(w, r)
	if !ok {
		return
	}
	tag, err := a.db.Exec(r.Context(), `DELETE FROM probe_keys WHERE provider = $1`, provider)
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

func (a *api) testProbeKey(w http.ResponseWriter, r *http.Request) {
	provider, ok := pathProvider(w, r)
	if !ok {
		return
	}
	key, source, err := effectiveProbeKey(r.Context(), a.db, provider)
	if err != nil {
		dbErr(w, err)
		return
	}
	if key == "" {
		writeErr(w, 400, "key not configured")
		return
	}
	if err := checkProbeAccount(r.Context(), a.probeHTTP, provider, key, a.probeBase(provider)); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "source": source, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "source": source})
}
