package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func loadSample(t *testing.T) [][]byte {
	f, err := os.Open("testdata/certstream_sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines [][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	return lines
}

func TestCertDomains(t *testing.T) {
	lines := loadSample(t)
	if got := certDomains(lines[0]); got != nil {
		t.Errorf("heartbeat: %v", got)
	}
	if got := certDomains(lines[1]); !reflect.DeepEqual(got, []string{"www.alpha.test", "alpha.test"}) {
		t.Errorf("cert: %v", got)
	}
	if got := certDomains([]byte("not json")); got != nil {
		t.Errorf("garbage: %v", got)
	}
}

func TestRootMatcher(t *testing.T) {
	m := &rootMatcher{}
	m.set([]string{"Alpha.Test", "sub.beta.test"})
	got := m.match([]string{
		"www.alpha.test", "ALPHA.TEST", "*.api.alpha.test", "api.alpha.test", "a.b.alpha.test.",
		"notalpha.test", "alpha.test.evil.test", "beta.test", "pay.sub.beta.test",
	})
	sort.Strings(got)
	want := []string{"a.b.alpha.test", "alpha.test", "api.alpha.test", "pay.sub.beta.test", "www.alpha.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// 回放样本流：本地 websocket 服务按样本逐行推送后断开；客户端重连后再推一次。
// 断言：匹配的子域以来源 certstream 入库，无关/伪后缀域名不入库，重复出现只更新 last_seen。
func TestCertstreamReplay(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	cleanup := func() {
		e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE '%alpha.test' OR subdomain LIKE '%beta.test'`)
		e.pool.Exec(context.Background(), `DELETE FROM root_domains WHERE domain IN ('alpha.test', 'sub.beta.test')`)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := e.pool.Exec(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'alpha.test'), ($1, 'sub.beta.test')`, vendorID); err != nil {
		t.Fatal(err)
	}

	lines := loadSample(t)
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conns.Add(1)
		for _, l := range lines {
			if c.Write(r.Context(), websocket.MessageText, l) != nil {
				return
			}
		}
		c.Close(websocket.StatusNormalClosure, "end of sample")
	}))
	defer srv.Close()

	var mu sync.Mutex
	dispatched := map[int64][]string{}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runCertstream(runCtx, certstreamConfig{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Reconnect: 100 * time.Millisecond,
			Refresh: time.Minute, BatchSize: 1000, BatchDelay: 150 * time.Millisecond}, e.pool,
			func(rootID int64, hosts []string) {
				mu.Lock()
				dispatched[rootID] = append(dispatched[rootID], hosts...)
				mu.Unlock()
			})
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond) // 等超时触发的批次
	cancel()
	<-done
	if conns.Load() < 2 {
		t.Fatalf("reconnect not observed, conns=%d", conns.Load())
	}

	rows, err := e.pool.Query(ctx, `SELECT subdomain, sources FROM assets WHERE subdomain LIKE '%alpha.test' OR subdomain LIKE '%beta.test' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for rows.Next() {
		var s string
		var src []string
		rows.Scan(&s, &src)
		got[s] = src
	}
	rows.Close()
	want := map[string][]string{
		"alpha.test":        {"certstream"},
		"www.alpha.test":    {"certstream"},
		"api.alpha.test":    {"certstream"},
		"pay.sub.beta.test": {"certstream"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored %v\nwant   %v", got, want)
	}
	var first, last time.Time
	e.pool.QueryRow(ctx, `SELECT first_seen, last_seen FROM assets WHERE subdomain = 'www.alpha.test'`).Scan(&first, &last)
	if !last.After(first) {
		t.Errorf("www.alpha.test seen again should bump last_seen: first=%v last=%v", first, last)
	}

	// 只有首次入库的 4 个子域交给 dns（重复回放不再派发），按根域分组
	mu.Lock()
	defer mu.Unlock()
	var alphaID, betaID int64
	e.pool.QueryRow(ctx, `SELECT id FROM root_domains WHERE domain = 'alpha.test'`).Scan(&alphaID)
	e.pool.QueryRow(ctx, `SELECT id FROM root_domains WHERE domain = 'sub.beta.test'`).Scan(&betaID)
	for _, h := range dispatched {
		sort.Strings(h)
	}
	wantDispatch := map[int64][]string{alphaID: {"alpha.test", "api.alpha.test", "www.alpha.test"}, betaID: {"pay.sub.beta.test"}}
	if !reflect.DeepEqual(dispatched, wantDispatch) {
		t.Fatalf("dispatched %v, want %v", dispatched, wantDispatch)
	}
}

func TestBatcherFlushOnSize(t *testing.T) {
	got := make(chan map[int64][]string, 4)
	b := newBatcher(3, time.Hour, func(m map[int64][]string) { got <- m })
	b.add(1, "a")
	b.add(2, "b")
	select {
	case m := <-got:
		t.Fatalf("flushed early: %v", m)
	case <-time.After(50 * time.Millisecond):
	}
	b.add(1, "c") // 第 3 条：立即触发
	select {
	case m := <-got:
		if !reflect.DeepEqual(m, map[int64][]string{1: {"a", "c"}, 2: {"b"}}) {
			t.Fatalf("batch %v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("size trigger did not flush")
	}
}

func TestBatcherFlushOnTimeout(t *testing.T) {
	got := make(chan map[int64][]string, 4)
	b := newBatcher(100, 200*time.Millisecond, func(m map[int64][]string) { got <- m })
	start := time.Now()
	b.add(1, "a")
	b.add(1, "b")
	select {
	case m := <-got:
		el := time.Since(start)
		if el < 180*time.Millisecond || el > 600*time.Millisecond {
			t.Errorf("timeout flush after %v, want ~200ms", el)
		}
		if !reflect.DeepEqual(m, map[int64][]string{1: {"a", "b"}}) {
			t.Fatalf("batch %v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout trigger did not flush")
	}
	select { // 已清空，计时器不再重复触发
	case m := <-got:
		t.Fatalf("extra flush %v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestCertstreamConfigFromEnv(t *testing.T) {
	c, err := certstreamConfigFromEnv()
	if err != nil || c.URL != "" || c.BatchSize != 100 || c.BatchDelay != time.Minute {
		t.Fatalf("defaults %+v %v", c, err)
	}
	t.Setenv("CERTSTREAM_BATCH_SIZE", "10")
	t.Setenv("CERTSTREAM_BATCH_INTERVAL", "5s")
	if c, _ = certstreamConfigFromEnv(); c.BatchSize != 10 || c.BatchDelay != 5*time.Second {
		t.Fatalf("overrides %+v", c)
	}
	for k, vs := range map[string][]string{"CERTSTREAM_BATCH_SIZE": {"0", "-1", "x", "1.5"}, "CERTSTREAM_BATCH_INTERVAL": {"0s", "500ms", "abc", "-1s"}} {
		for _, v := range vs {
			t.Setenv(k, v)
			if _, err := certstreamConfigFromEnv(); err == nil {
				t.Errorf("%s=%s should be rejected", k, v)
			}
		}
		t.Setenv(k, "")
	}
}
