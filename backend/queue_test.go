package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

func TestConfigFromEnv(t *testing.T) {
	c, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Rates["passive"] != 1 || c.Rates["dns"] != 10 || c.Rates["http"] != 5 || c.MaxRetry != 3 || c.RetryBase != time.Second {
		t.Fatalf("defaults: %+v", c)
	}
	t.Setenv("RATE_DNS", "2.5")
	t.Setenv("MAX_RETRY", "5")
	t.Setenv("RETRY_BASE_SECONDS", "1.5")
	if c, err = configFromEnv(); err != nil {
		t.Fatal(err)
	}
	if c.Rates["dns"] != 2.5 || c.MaxRetry != 5 || c.RetryBase != 1500*time.Millisecond {
		t.Fatalf("overrides: %+v", c)
	}
	for k, v := range map[string]string{
		"RATE_HTTP": "0", "RATE_DNS": "NaN", "RATE_PASSIVE": "Inf", "RATE_PASSIVE_NEG": "-Inf",
		"MAX_RETRY": "-1", "RETRY_BASE_SECONDS": "NaN", "RETRY_BASE_SECONDS_NEG": "0.5",
	} {
		t.Run(k+"="+v, func(t *testing.T) {
			t.Setenv(strings.TrimSuffix(k, "_NEG"), v)
			if _, err := configFromEnv(); err == nil {
				t.Fatalf("%s=%s should be rejected", k, v)
			}
		})
	}
}

func TestRetryDelayExponential(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for i, w := range want {
		if got := retryDelay(time.Second, i); got != w { // asynq: 首次失败 n=0
			t.Errorf("retry %d: got %v want %v", i+1, got, w)
		}
	}
}

// 限速中间件：每类独立计速，一类慢不影响另一类。
func TestRateLimitPerKind(t *testing.T) {
	limiters := map[string]*rate.Limiter{
		"http": rate.NewLimiter(10, 1),
		"dns":  rate.NewLimiter(1000, 1),
	}
	h := rateLimit(limiters)(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil }))
	run := func(kind string, n int) time.Duration {
		start := time.Now()
		for i := 0; i < n; i++ {
			if err := h.ProcessTask(context.Background(), asynq.NewTask("scan:"+kind, nil)); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start)
	}
	if d := run("http", 6); d < 450*time.Millisecond || d > 900*time.Millisecond {
		t.Errorf("http 6 tasks @10/s took %v, want ~500ms", d)
	}
	if d := run("dns", 6); d > 50*time.Millisecond {
		t.Errorf("dns 6 tasks @1000/s took %v, want <50ms", d)
	}
}

// 集成测试环境：本地 Postgres（TEST_DATABASE_URL）+ miniredis。
type testEnv struct {
	pool   *pgxpool.Pool
	client *asynq.Client
	redis  asynq.RedisClientOpt
	rootID int64
}

func newTestEnv(t *testing.T) *testEnv {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, conn.Conn()); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	name := "t4-" + t.Name()
	pool.Exec(ctx, `DELETE FROM vendors WHERE name = $1`, name)
	e := &testEnv{pool: pool}
	if err := pool.QueryRow(ctx, `WITH v AS (INSERT INTO vendors (name) VALUES ($1) RETURNING id)
		INSERT INTO root_domains (vendor_id, domain) SELECT id, $1 || '.example.test' FROM v RETURNING id`, name).Scan(&e.rootID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM vendors WHERE name = $1`, name) })
	mr := miniredis.RunT(t)
	e.redis = asynq.RedisClientOpt{Addr: mr.Addr()}
	e.client = asynq.NewClient(e.redis)
	t.Cleanup(func() { e.client.Close() })
	return e
}

func (e *testEnv) start(t *testing.T, cfg workerConfig, handlers map[string]ScanHandler) {
	srv, mux := newWorker(e.redis, e.pool, cfg, handlers)
	if err := srv.Start(mux); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
}

// waitStatus 等到该根域下 kind 类任务有 n 个达到 status。
func (e *testEnv) waitStatus(t *testing.T, kind, status string, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var c int
		e.pool.QueryRow(context.Background(), `SELECT count(*) FROM scan_tasks WHERE root_id = $1 AND kind = $2 AND status = $3`,
			e.rootID, kind, status).Scan(&c)
		if c >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s tasks %s: %d/%d", kind, status, c, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func testConfig(rates map[string]rate.Limit) workerConfig {
	return workerConfig{Rates: rates, MaxRetry: 3, RetryBase: time.Second, Concurrent: 10, PollDelay: 100 * time.Millisecond}
}

var fastRates = map[string]rate.Limit{"passive": 100, "dns": 100, "http": 100}

// 手动触发三类扫描，全部 done，http 受限速。
func TestTriggerScanEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var httpCalls atomic.Int32
	e.start(t, testConfig(map[string]rate.Limit{"passive": 100, "dns": 100, "http": 4}), map[string]ScanHandler{
		"http": func(context.Context, int64, []string) ([]string, error) { httpCalls.Add(1); return nil, nil },
	})
	if _, err := TriggerScan(ctx, e.pool, e.client, e.rootID, "bogus", 3); err == nil {
		t.Fatal("unknown kind should fail")
	}
	start := time.Now()
	for _, k := range []string{"passive", "dns", "http", "http", "http", "http", "http"} {
		if _, err := TriggerScan(ctx, e.pool, e.client, e.rootID, k, 3); err != nil {
			t.Fatal(err)
		}
	}
	e.waitStatus(t, "passive", "done", 1)
	e.waitStatus(t, "dns", "done", 1)
	e.waitStatus(t, "http", "done", 5)
	elapsed := time.Since(start)
	if httpCalls.Load() != 5 {
		t.Errorf("http handler calls = %d, want 5", httpCalls.Load())
	}
	if elapsed < 900*time.Millisecond { // 5 个 @4/s，burst 1
		t.Errorf("5 http tasks @4/s finished in %v, rate limit not applied", elapsed)
	}
	t.Logf("7 scans done in %v", elapsed)
}

// 串联：passive → dns → http；dns（mock 泛解析过滤）只把通过的主机交给 http。
func TestChainPassiveDNSHTTP(t *testing.T) {
	e := newTestEnv(t)
	var mu sync.Mutex
	var dnsIn, httpIn []string
	e.start(t, testConfig(fastRates), map[string]ScanHandler{
		"passive": func(context.Context, int64, []string) ([]string, error) {
			return []string{"a.example.test", "wild.example.test", "c.example.test"}, nil
		},
		"dns": func(_ context.Context, _ int64, hosts []string) ([]string, error) {
			mu.Lock()
			dnsIn = append(dnsIn, hosts...)
			mu.Unlock()
			var ok []string
			for _, h := range hosts {
				if h != "wild.example.test" { // mock：泛解析/解析失败的被过滤
					ok = append(ok, h)
				}
			}
			return ok, nil
		},
		"http": func(_ context.Context, _ int64, hosts []string) ([]string, error) {
			mu.Lock()
			httpIn = append(httpIn, hosts...)
			mu.Unlock()
			return []string{"ignored"}, nil // http 是末级，不再串联
		},
	})
	if _, err := TriggerScan(context.Background(), e.pool, e.client, e.rootID, "passive", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "http", "done", 1)
	time.Sleep(300 * time.Millisecond) // 确认没有多余任务
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(httpIn)
	if len(dnsIn) != 3 || len(httpIn) != 2 || httpIn[0] != "a.example.test" || httpIn[1] != "c.example.test" {
		t.Fatalf("dns got %v, http got %v", dnsIn, httpIn)
	}
	var total int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM scan_tasks WHERE root_id = $1`, e.rootID).Scan(&total)
	if total != 3 {
		t.Fatalf("scan_tasks = %d, want 3 (passive, dns, http)", total)
	}
}

// dns 无输出时不触发 http。
func TestChainStopsWhenEmpty(t *testing.T) {
	e := newTestEnv(t)
	e.start(t, testConfig(fastRates), map[string]ScanHandler{
		"passive": func(context.Context, int64, []string) ([]string, error) { return []string{"x.example.test"}, nil },
		"dns":     func(context.Context, int64, []string) ([]string, error) { return nil, nil },
	})
	TriggerScan(context.Background(), e.pool, e.client, e.rootID, "passive", 3)
	e.waitStatus(t, "dns", "done", 1)
	time.Sleep(300 * time.Millisecond)
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM scan_tasks WHERE root_id = $1 AND kind = 'http'`, e.rootID).Scan(&n)
	if n != 0 {
		t.Fatalf("http tasks = %d, want 0", n)
	}
}

// waitSeen 轮询直到某任务出现过 status（用于观察 retrying 中间态）。
func (e *testEnv) waitSeen(t *testing.T, kind, status string) {
	t.Helper()
	e.waitStatus(t, kind, status, 1)
}

// 重试：retrying → done（前 2 次失败第 3 次成功）；retrying → failed（1+3 次后终态）。
func TestRetry(t *testing.T) {
	e := newTestEnv(t)
	var flaky, broken atomic.Int32
	cfg := testConfig(fastRates)
	cfg.RetryBase = time.Second // 1s, 2s, 4s
	e.start(t, cfg, map[string]ScanHandler{
		"dns": func(context.Context, int64, []string) ([]string, error) {
			if flaky.Add(1) <= 2 {
				return nil, errors.New("temporary")
			}
			return nil, nil
		},
		"http": func(context.Context, int64, []string) ([]string, error) {
			broken.Add(1)
			return nil, errors.New("always fails")
		},
	})
	ctx := context.Background()
	TriggerScan(ctx, e.pool, e.client, e.rootID, "dns", 3)
	httpID, _ := TriggerScan(ctx, e.pool, e.client, e.rootID, "http", 3)
	e.waitSeen(t, "dns", "retrying")
	e.waitSeen(t, "http", "retrying")
	e.waitStatus(t, "dns", "done", 1)
	e.waitStatus(t, "http", "failed", 1)
	time.Sleep(500 * time.Millisecond)
	if flaky.Load() != 3 {
		t.Errorf("flaky attempts = %d, want 3", flaky.Load())
	}
	if broken.Load() != 4 {
		t.Errorf("broken attempts = %d, want 4 (1 + MAX_RETRY 3)", broken.Load())
	}
	var errMsg string
	e.pool.QueryRow(ctx, `SELECT error FROM scan_tasks WHERE id = $1`, httpID).Scan(&errMsg)
	if errMsg != "always fails" {
		t.Errorf("error = %q", errMsg)
	}

	// failed 是终态：同一 scan_id 的任务再次投递也不执行、不改状态
	payload, _ := json.Marshal(scanPayload{ScanID: httpID})
	if _, err := e.client.Enqueue(asynq.NewTask("scan:http", payload), asynq.Queue("http")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	var status string
	e.pool.QueryRow(ctx, `SELECT status FROM scan_tasks WHERE id = $1`, httpID).Scan(&status)
	if status != "failed" || broken.Load() != 4 {
		t.Errorf("after re-enqueue: status=%s attempts=%d, want failed/4", status, broken.Load())
	}
}

// 真实重试间隔：base=1s、MaxRetry=3 → 相邻调用间隔约 1s/2s/4s（误差 <1s）且递增。
func TestRetryIntervals(t *testing.T) {
	e := newTestEnv(t)
	var mu sync.Mutex
	var calls []time.Time
	e.start(t, testConfig(fastRates), map[string]ScanHandler{
		"dns": func(context.Context, int64, []string) ([]string, error) {
			mu.Lock()
			calls = append(calls, time.Now())
			mu.Unlock()
			return nil, errors.New("fail")
		},
	})
	TriggerScan(context.Background(), e.pool, e.client, e.rootID, "dns", 3)
	e.waitStatus(t, "dns", "failed", 1)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(calls))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	var prev time.Duration
	for i, w := range want {
		got := calls[i+1].Sub(calls[i])
		t.Logf("retry %d interval %v (want ~%v)", i+1, got.Round(time.Millisecond), w)
		if d := got - w; d < -time.Second || d > time.Second {
			t.Errorf("retry %d interval %v, want %v ±1s", i+1, got, w)
		}
		if got <= prev {
			t.Errorf("retry %d interval %v not increasing (prev %v)", i+1, got, prev)
		}
		prev = got
	}
}
