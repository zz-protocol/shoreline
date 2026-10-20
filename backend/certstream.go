package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// certstreamMessage 只解析需要的字段（certstream 公开消息格式）。
type certstreamMessage struct {
	MessageType string `json:"message_type"`
	Data        struct {
		LeafCert struct {
			AllDomains []string `json:"all_domains"`
		} `json:"leaf_cert"`
	} `json:"data"`
}

// certDomains 解析一条消息，返回证书中的域名；非证书消息（如 heartbeat）或格式错误返回 nil。
func certDomains(raw []byte) []string {
	var m certstreamMessage
	if json.Unmarshal(raw, &m) != nil || m.MessageType != "certificate_update" {
		return nil
	}
	return m.Data.LeafCert.AllDomains
}

// rootMatcher 缓存根域列表，定期从库刷新。
type rootMatcher struct {
	mu    sync.RWMutex
	roots []string
}

func (m *rootMatcher) set(roots []string) {
	norm := make([]string, 0, len(roots))
	for _, r := range roots {
		norm = append(norm, normalizeHost(r))
	}
	m.mu.Lock()
	m.roots = norm
	m.mu.Unlock()
}

// match 返回属于任一根域的规范化子域（去重）。
func (m *rootMatcher) match(domains []string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, d := range domains {
		for _, r := range m.roots {
			if h := normalizeSubdomain(d, r); h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
				break
			}
		}
	}
	return out
}

func (m *rootMatcher) refresh(ctx context.Context, d *pgxpool.Pool) error {
	rows, err := d.Query(ctx, `SELECT domain FROM root_domains`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return err
		}
		roots = append(roots, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.set(roots)
	return nil
}

// certstreamConfig：CERTSTREAM_URL 为空（默认）则不启动，开发阶段不连外网；部署时设为 wss://certstream.calidog.io/ 等。
type certstreamConfig struct {
	URL       string
	Reconnect time.Duration // 断线重连间隔
	Refresh   time.Duration // 根域列表刷新间隔
}

func certstreamConfigFromEnv() certstreamConfig {
	return certstreamConfig{URL: os.Getenv("CERTSTREAM_URL"), Reconnect: 5 * time.Second, Refresh: time.Minute}
}

// runCertstream 常驻订阅：断线自动重连，匹配根域的域名以来源 certstream 入库，直到 ctx 结束。
func runCertstream(ctx context.Context, cfg certstreamConfig, d *pgxpool.Pool) {
	m := &rootMatcher{}
	if err := m.refresh(ctx, d); err != nil {
		log.Printf("certstream: load roots: %v", err)
	}
	go func() {
		t := time.NewTicker(cfg.Refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := m.refresh(ctx, d); err != nil {
					log.Printf("certstream: refresh roots: %v", err)
				}
			}
		}
	}()
	for ctx.Err() == nil {
		if err := streamOnce(ctx, cfg.URL, m, d); err != nil && ctx.Err() == nil {
			log.Printf("certstream: %v; reconnect in %v", err, cfg.Reconnect)
		}
		select {
		case <-ctx.Done():
		case <-time.After(cfg.Reconnect):
		}
	}
}

func streamOnce(ctx context.Context, url string, m *rootMatcher, d *pgxpool.Pool) error {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		hosts := m.match(certDomains(raw))
		if len(hosts) == 0 {
			continue
		}
		found := map[string][]string{}
		for _, h := range hosts {
			found[h] = []string{"certstream"}
		}
		if err := storeAssets(ctx, d, found); err != nil {
			log.Printf("certstream: store: %v", err)
		}
	}
}
