package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 方案 C：单 host storeURL 失败只记日志 + degraded，任务仍 done，其它 host 照常入库。
func TestT33StoreFailureDegradedNotFatal(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	hosts := []string{"hdr.t33c.example.test", "ok.t33c.example.test"}
	for _, h := range hosts {
		e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, resolved, wildcard) VALUES ($1, $2, true, false)`, e.rootID, h)
	}
	storeURLHook = func(r *probeResult) error {
		if strings.HasPrefix(r.Host, "hdr.") {
			return errors.New("injected store failure")
		}
		return nil
	}
	t.Cleanup(func() { storeURLHook = nil })
	cfg := testConfig(fastRates)
	cfg.MaxRetry = 1
	e.start(t, cfg, map[string]ScanHandler{"http": httpHandler(e.pool, newProber(gbkServer(t)))})
	if _, err := enqueueScan(ctx, e.pool, e.client, e.rootID, "http", hosts, 0, cfg.MaxRetry); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "http", "done", 1)
	var degraded bool
	var failures int
	var reason string
	if err := e.pool.QueryRow(ctx, `SELECT coalesce((result->>'degraded')::bool, false), coalesce((result->>'store_failures')::int, 0),
		coalesce(result->>'degraded_reason', '') FROM scan_tasks WHERE root_id = $1 AND kind = 'http'`, e.rootID).Scan(&degraded, &failures, &reason); err != nil {
		t.Fatal(err)
	}
	if !degraded || failures != 1 || !strings.Contains(reason, "injected") {
		t.Fatalf("degraded=%v failures=%d reason=%q", degraded, failures, reason)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM urls WHERE host = 'ok.t33c.example.test'`).Scan(&n)
	if n != 1 {
		t.Fatalf("good host not stored")
	}
	t.Logf("单 host 入库失败：任务 done、degraded=true、store_failures=1，其它 host 照常入库")
}

func TestT33NormalizeSubdomainUTF8(t *testing.T) {
	if got := normalizeSubdomain("a\xff.example.test", "example.test"); got != "" && !strings.Contains(got, "\uFFFD") {
		t.Fatalf("got %q", got)
	}
	if got := normalizeSubdomain("A.Example.Test", "example.test"); got != "a.example.test" {
		t.Fatalf("got %q", got)
	}
}
