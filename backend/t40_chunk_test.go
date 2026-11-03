package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestChunkHosts5000(t *testing.T) {
	hosts := make([]string, 5000)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.chunk.example.test", i)
	}
	got := chunkHosts(hosts, 2000)
	if len(got) != 3 || len(got[0]) != 2000 || len(got[1]) != 2000 || len(got[2]) != 1000 {
		t.Fatalf("chunks=%d lens=%d,%d,%d", len(got), len(got[0]), len(got[1]), len(got[2]))
	}
	if got[0][0] != hosts[0] || got[2][999] != hosts[4999] {
		t.Fatal("chunk boundaries wrong")
	}
}

func TestT40SplitAndDeriveParent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	cfg := testConfig(fastRates)
	cfg.HostChunkSize = 2000
	var handled int
	e.start(t, cfg, map[string]ScanHandler{
		"dns": func(_ context.Context, _ int64, hosts []string) ([]string, error) {
			handled += len(hosts)
			return nil, nil
		},
	})
	hosts := make([]string, 5000)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("s%d.%d.example.test", e.rootID, i)
	}
	parentID, err := enqueueScan(ctx, e.pool, e.client, e.rootID, "dns", hosts, 0, cfg.MaxRetry)
	if err != nil {
		t.Fatal(err)
	}
	// 等 3 个子任务 done，再等父 done
	deadline := time.Now().Add(30 * time.Second)
	for {
		var children, parentDone int
		e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE parent_id = $1 AND status = 'done'),
			(SELECT CASE WHEN status = 'done' THEN 1 ELSE 0 END FROM scan_tasks WHERE id = $1)
			FROM scan_tasks`, parentID).Scan(&children, &parentDone)
		if children >= 3 && parentDone == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("children_done=%d parent_done=%d handled=%d", children, parentDone, handled)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if handled != 5000 {
		t.Fatalf("handler saw %d hosts, want 5000", handled)
	}
	var chunks int
	var chunked bool
	e.pool.QueryRow(ctx, `SELECT COALESCE((result->>'chunks')::int,0), COALESCE((result->>'chunked')::boolean,false) FROM scan_tasks WHERE id = $1`, parentID).Scan(&chunks, &chunked)
	if !chunked || chunks != 3 {
		t.Fatalf("parent result chunked=%v chunks=%d", chunked, chunks)
	}
	var nChildren int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE parent_id = $1`, parentID).Scan(&nChildren)
	if nChildren != 3 {
		t.Fatalf("children=%d", nChildren)
	}
	t.Logf("parent=%d chunks=3 handled=%d", parentID, handled)
}

func TestT40ParentFailedWhenChunkFails(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	cfg := testConfig(fastRates)
	cfg.HostChunkSize = 2000
	cfg.MaxRetry = 0
	e.start(t, cfg, map[string]ScanHandler{
		"dns": func(_ context.Context, id int64, hosts []string) ([]string, error) {
			if len(hosts) < 1500 { // 最后一小片失败
				return nil, fmt.Errorf("boom chunk")
			}
			return nil, nil
		},
	})
	hosts := make([]string, 5000)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("f%d.%d.example.test", e.rootID, i)
	}
	parentID, err := enqueueScan(ctx, e.pool, e.client, e.rootID, "dns", hosts, 0, cfg.MaxRetry)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var st string
		e.pool.QueryRow(ctx, `SELECT status FROM scan_tasks WHERE id = $1`, parentID).Scan(&st)
		if st == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent status=%s want failed", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var failedChildren int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE parent_id = $1 AND status = 'failed'`, parentID).Scan(&failedChildren)
	if failedChildren < 1 {
		t.Fatalf("expected failed child, got %d", failedChildren)
	}
}

func TestT40RequeueIncompleteChunks(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	// 手工建分片父+子（子 pending、hosts 在 result，不入队）
	var parentID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, status, result)
		SELECT vendor_id, id, 'dns', 'running', '{"chunked":true,"chunks":2}'::jsonb FROM root_domains WHERE id = $1 RETURNING id`, e.rootID).Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	hosts := []string{"a.requeue.example.test", "b.requeue.example.test"}
	raw, _ := json.Marshal(map[string]any{"chunk_index": 0, "hosts": hosts})
	var childID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, status, parent_id, root_scan_id, result)
		SELECT vendor_id, id, 'dns', 'running', $2, $2, $3::jsonb FROM root_domains WHERE id = $1 RETURNING id`, e.rootID, parentID, raw).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(map[string]rate.Limit{"dns": 100, "http": 100, "passive": 100, "active": 100, "expand": 100})
	cfg.HostChunkSize = 2000
	var saw []string
	e.start(t, cfg, map[string]ScanHandler{
		"dns": func(_ context.Context, _ int64, h []string) ([]string, error) {
			saw = append(saw, h...)
			return nil, nil
		},
	})
	n, err := requeueIncompleteChunks(ctx, e.pool, e.client, e.redis, cfg.MaxRetry)
	if err != nil || n != 1 {
		t.Fatalf("requeue n=%d err=%v", n, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var st string
		e.pool.QueryRow(ctx, `SELECT status FROM scan_tasks WHERE id = $1`, childID).Scan(&st)
		if st == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child status=%s", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 父应随之 done
	for {
		var st string
		e.pool.QueryRow(ctx, `SELECT status FROM scan_tasks WHERE id = $1`, parentID).Scan(&st)
		if st == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent status=%s", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(saw) != 2 {
		t.Fatalf("handler hosts=%v", saw)
	}
}

func TestMainCallsRequeueIncompleteChunks(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var mainFn *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" {
			mainFn = fn
			break
		}
	}
	if mainFn == nil {
		t.Fatal("main not found")
	}
	var calls []string
	ast.Inspect(mainFn, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			calls = append(calls, fn.Name)
		case *ast.SelectorExpr:
			calls = append(calls, fn.Sel.Name)
		}
		return true
	})
	idx := func(name string) int {
		for i, c := range calls {
			if c == name {
				return i
			}
		}
		return -1
	}
	rq, orph, run := idx("requeueIncompleteChunks"), idx("failOrphanScans"), idx("Run")
	if rq < 0 || orph < 0 || run < 0 || !(rq < orph && orph < run) {
		t.Fatalf("order requeue=%d orphan=%d Run=%d in %v", rq, orph, run, calls)
	}
}

func TestHostChunkSizeConfig(t *testing.T) {
	c, err := configFromEnv()
	if err != nil || c.HostChunkSize != 2000 {
		t.Fatalf("default %+v err=%v", c, err)
	}
	t.Setenv("HOST_CHUNK_SIZE", "100")
	c, err = configFromEnv()
	if err != nil || c.HostChunkSize != 100 {
		t.Fatalf("override %+v err=%v", c, err)
	}
	t.Setenv("HOST_CHUNK_SIZE", "0")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("HOST_CHUNK_SIZE=0 should fail")
	}
}
