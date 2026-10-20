package main

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

func TestRatesFromEnv(t *testing.T) {
	t.Setenv("RATE_DNS", "2.5")
	r, err := ratesFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if r["dns"] != 2.5 || r["passive"] != 1 || r["http"] != 5 {
		t.Fatalf("got %v", r)
	}
	t.Setenv("RATE_HTTP", "0")
	if _, err := ratesFromEnv(); err == nil {
		t.Fatal("RATE_HTTP=0 should be rejected")
	}
}

// 限速中间件：每类独立计速，一类慢不影响另一类。
func TestRateLimitPerKind(t *testing.T) {
	limiters := map[string]*rate.Limiter{
		"http": rate.NewLimiter(10, 1),   // 10/s
		"dns":  rate.NewLimiter(1000, 1), // 基本不限
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

// 端到端：mock Redis（miniredis）+ 本地 Postgres，手动触发三类扫描，worker 处理后状态为 done，且 http 受限速。
func TestTriggerScanEndToEnd(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, conn.Conn()); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	pool.Exec(ctx, `DELETE FROM vendors WHERE name = 't4-vendor'`)
	var rootID int64
	if err := pool.QueryRow(ctx, `WITH v AS (INSERT INTO vendors (name) VALUES ('t4-vendor') RETURNING id)
		INSERT INTO root_domains (vendor_id, domain) SELECT id, 't4.example.test' FROM v RETURNING id`).Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM vendors WHERE name = 't4-vendor'`)

	mr := miniredis.RunT(t)
	redisOpt := asynq.RedisClientOpt{Addr: mr.Addr()}
	client := asynq.NewClient(redisOpt)
	defer client.Close()

	var httpCalls atomic.Int32
	handlers := map[string]ScanHandler{
		"http": func(context.Context, int64) error { httpCalls.Add(1); return nil },
	}
	rates := map[string]rate.Limit{"passive": 100, "dns": 100, "http": 4}
	srv, mux := newWorker(redisOpt, pool, rates, handlers)
	if err := srv.Start(mux); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	if _, err := TriggerScan(ctx, pool, client, rootID, "bogus"); err == nil {
		t.Fatal("unknown kind should fail")
	}
	var ids []int64
	start := time.Now()
	for _, k := range []string{"passive", "dns", "http", "http", "http", "http", "http"} {
		id, err := TriggerScan(ctx, pool, client, rootID, k)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var done int
		pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE id = ANY($1) AND status = 'done'`, ids).Scan(&done)
		if done == len(ids) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d done", done, len(ids))
		}
		time.Sleep(50 * time.Millisecond)
	}
	elapsed := time.Since(start)
	if httpCalls.Load() != 5 {
		t.Errorf("http handler calls = %d, want 5", httpCalls.Load())
	}
	// 5 个 http 任务 @4/s，burst 1：至少约 1s
	if elapsed < 900*time.Millisecond {
		t.Errorf("5 http tasks @4/s finished in %v, rate limit not applied", elapsed)
	}
	t.Logf("7 scans done in %v", elapsed)
}
