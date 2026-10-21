package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// T23：同一厂商 expand 进行中再触发返回 409（任意根域都算）；不同厂商互不影响；done 后可再触发；并发只成功一个；MCP 同逻辑。
func TestExpandDedup(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	v1 := e.vendorID()
	n := time.Now().UnixNano()
	r2 := c.want(201, "POST", fmt.Sprintf("/api/vendors/%d/roots", v1), map[string]any{"domain": fmt.Sprintf("t23b%d.example.test", n)})
	o := c.want(201, "POST", "/api/vendors", map[string]any{"name": fmt.Sprintf("%s-o-%d", t.Name(), n)})
	v2 := int64(o["id"].(float64))
	rv2 := c.want(201, "POST", fmt.Sprintf("/api/vendors/%d/roots", v2), map[string]any{"domain": fmt.Sprintf("t23c%d.example.test", n)})
	root2, rootOther := int64(r2["id"].(float64)), int64(rv2["id"].(float64))

	first := c.want(202, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "expand"})
	c.want(409, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "expand"})
	c.want(409, "POST", "/api/scans", map[string]any{"root_id": root2, "kind": "expand"})     // 同厂商另一根域
	c.want(202, "POST", "/api/scans", map[string]any{"root_id": root2, "kind": "passive"})    // 其它 kind 不受影响
	c.want(202, "POST", "/api/scans", map[string]any{"root_id": rootOther, "kind": "expand"}) // 不同厂商

	sess, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, isErr := callTool(t, sess, "trigger_scan", map[string]any{"root_id": root2, "kind": "expand"}); !isErr {
		t.Fatal("MCP trigger_scan expand should fail while in progress")
	}

	for _, st := range []string{"running", "retrying"} {
		e.pool.Exec(ctx, `UPDATE scan_tasks SET status = $2 WHERE id = $1`, int64(first["id"].(float64)), st)
		c.want(409, "POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "expand"})
	}
	e.pool.Exec(ctx, `UPDATE scan_tasks SET status = 'done' WHERE id = $1`, int64(first["id"].(float64)))
	again := c.want(202, "POST", "/api/scans", map[string]any{"root_id": root2, "kind": "expand"})

	// 并发：done 后同时触发 8 次，只成功 1 次
	e.pool.Exec(ctx, `UPDATE scan_tasks SET status = 'done' WHERE id = $1`, int64(again["id"].(float64)))
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := c.do("POST", "/api/scans", map[string]any{"root_id": e.rootID, "kind": "expand"})
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[202] != 1 || codes[409] != 7 {
		t.Fatalf("concurrent codes %v", codes)
	}
	t.Logf("并发 8 次：%v", codes)
}

// T23 打回①：小连接池（4）下同一厂商 22 并发 expand × 20 轮：每轮恰 1 个 202、其余 409，无超时，无孤儿行。
func TestExpandConcurrentSmallPool(t *testing.T) {
	e, c, a0 := newAPITest(t, 100)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	a := *a0
	a.db = small
	srv := httptest.NewServer(newMux(nil, &a))
	defer srv.Close()
	v := e.vendorID()
	hc := &http.Client{Timeout: 15 * time.Second}
	const n, rounds = 22, 20
	for r := 0; r < rounds; r++ {
		small.Exec(ctx, `UPDATE scan_tasks SET status = 'done' WHERE vendor_id = $1 AND kind = 'expand'`, v)
		var before int
		small.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE vendor_id = $1 AND kind = 'expand'`, v).Scan(&before)
		var wg sync.WaitGroup
		var mu sync.Mutex
		codes := map[int]int{}
		start := time.Now()
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest("POST", srv.URL+"/api/scans", strings.NewReader(fmt.Sprintf(`{"root_id":%d,"kind":"expand"}`, e.rootID)))
				req.Header.Set("Authorization", "Bearer "+testBootstrap)
				req.Header.Set("Content-Type", "application/json")
				code := -1 // 超时/连接错误
				if resp, err := hc.Do(req); err == nil {
					code = resp.StatusCode
					resp.Body.Close()
				}
				mu.Lock()
				codes[code]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		var after, pending int
		small.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'pending') FROM scan_tasks WHERE vendor_id = $1 AND kind = 'expand'`, v).Scan(&after, &pending)
		if codes[202] != 1 || codes[409] != n-1 || after-before != 1 || pending > 1 {
			t.Fatalf("round %d: codes %v, new rows %d, pending %d, %v", r, codes, after-before, pending, time.Since(start))
		}
	}
	_ = c
}
