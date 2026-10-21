package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
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
