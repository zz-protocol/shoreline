package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
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

// certstreamConfig：CERTSTREAM_URL 为空（默认）则不启动，开发阶段不连外网；部署时设为自建 certstream-server-go 或公共服务地址。
type certstreamConfig struct {
	URL        string
	Reconnect  time.Duration // 断线重连间隔
	Refresh    time.Duration // 根域列表刷新间隔
	BatchSize  int           // CERTSTREAM_BATCH_SIZE：新子域攒满 N 条交 dns（默认 100）
	BatchDelay time.Duration // CERTSTREAM_BATCH_INTERVAL：首条进批后最多等 T 交 dns（默认 60s，>= 1s）
}

func certstreamConfigFromEnv() (certstreamConfig, error) {
	c := certstreamConfig{URL: os.Getenv("CERTSTREAM_URL"), Reconnect: 5 * time.Second, Refresh: time.Minute,
		BatchSize: 100, BatchDelay: time.Minute}
	if s := os.Getenv("CERTSTREAM_BATCH_SIZE"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return c, fmt.Errorf("CERTSTREAM_BATCH_SIZE: invalid %q (integer >= 1)", s)
		}
		c.BatchSize = n
	}
	if s := os.Getenv("CERTSTREAM_BATCH_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Second {
			return c, fmt.Errorf("CERTSTREAM_BATCH_INTERVAL: invalid %q (duration >= 1s, e.g. 60s)", s)
		}
		c.BatchDelay = d
	}
	return c, nil
}

// batcher 在内存攒批：满 size 条立即 flush；否则首条进批后满 delay 时 flush。
type batcher struct {
	mu      sync.Mutex
	size    int
	delay   time.Duration
	pending map[int64][]string // 根域 id → 子域
	count   int
	timer   *time.Timer
	flush   func(map[int64][]string)
}

func newBatcher(size int, delay time.Duration, flush func(map[int64][]string)) *batcher {
	return &batcher{size: size, delay: delay, pending: map[int64][]string{}, flush: flush}
}

func (b *batcher) add(rootID int64, host string) {
	b.mu.Lock()
	b.pending[rootID] = append(b.pending[rootID], host)
	b.count++
	if b.count >= b.size {
		b.flushLocked()
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.delay, func() {
			b.mu.Lock()
			b.flushLocked()
		})
	}
	b.mu.Unlock()
}

// flushLocked 取走当前批并解锁，再在锁外调用 flush。
func (b *batcher) flushLocked() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	batch := b.pending
	b.pending, b.count = map[int64][]string{}, 0
	b.mu.Unlock()
	if len(batch) > 0 {
		b.flush(batch)
	}
}

// dispatchFunc 把一批新子域交给 dns（按根域分组）。
type dispatchFunc func(rootID int64, hosts []string)

// runCertstream 常驻订阅：断线自动重连，匹配根域的域名以来源 certstream 入库，
// 首次入库的子域攒批后交给 dns，直到 ctx 结束。
func runCertstream(ctx context.Context, cfg certstreamConfig, d *pgxpool.Pool, dispatch dispatchFunc) {
	b := newBatcher(cfg.BatchSize, cfg.BatchDelay, func(batch map[int64][]string) {
		for rootID, hosts := range batch {
			dispatch(rootID, hosts)
		}
	})
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
		if err := streamOnce(ctx, cfg.URL, m, d, b); err != nil && ctx.Err() == nil {
			log.Printf("certstream: %v; reconnect in %v", err, cfg.Reconnect)
		}
		select {
		case <-ctx.Done():
		case <-time.After(cfg.Reconnect):
		}
	}
}

func streamOnce(ctx context.Context, url string, m *rootMatcher, d *pgxpool.Pool, b *batcher) error {
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
		created, err := storeAssets(ctx, d, found)
		if err != nil {
			log.Printf("certstream: store: %v", err)
		}
		for h, rootID := range created {
			b.add(rootID, h)
		}
	}
}
