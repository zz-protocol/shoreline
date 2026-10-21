package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// T25-2：总数恰为页大小整数倍时最后一页不返回 next_cursor（REST assets/events/scans + MCP search_assets/list_new_assets）。
// T25-5：stats 总计与各厂商按 type 分别返回 new / recovered。
func TestPagingExactMultipleAndStatsByType(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	s := fmt.Sprint(time.Now().UnixNano())
	o := c.want(201, "POST", "/api/vendors", map[string]any{"name": "pg-" + s})
	v := int64(o["id"].(float64))
	r := c.want(201, "POST", fmt.Sprintf("/api/vendors/%d/roots", v), map[string]any{"domain": "pg" + s + ".test"})
	root := int64(r["id"].(float64))
	var assetIDs []int64
	for i := 0; i < 4; i++ {
		var id int64
		e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, root, fmt.Sprintf("h%d.pg%s.test", i, s)).Scan(&id)
		assetIDs = append(assetIDs, id)
		emitNew(ctx, e.pool, id, nil, map[string]any{})
		e.pool.Exec(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, status) VALUES ($1, $2, 'passive', 'done')`, v, root)
	}
	// 再加 1 条 recovered：type=new 时仍是 4 条
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'dead' WHERE id = $1`, assetIDs[0])
	if err := markAlive(ctx, e.pool, assetIDs[0], "h0", true); err != nil {
		t.Fatal(err)
	}

	walk := func(base string) []string {
		var log []string
		cursor := ""
		for i := 0; i < 5; i++ {
			p := base + "&limit=2"
			if cursor != "" {
				p += "&cursor=" + cursor
			}
			out := c.want(200, "GET", p, nil)
			log = append(log, fmt.Sprintf("%d/%v", len(items(out)), out["next_cursor"] != nil))
			if out["next_cursor"] == nil {
				return log
			}
			cursor = out["next_cursor"].(string)
		}
		return log
	}
	for _, base := range []string{
		fmt.Sprintf("/api/assets?vendor=%d", v),
		fmt.Sprintf("/api/events?vendor=%d&type=new", v),
		fmt.Sprintf("/api/scans?root=%d", root),
	} {
		got := fmt.Sprint(walk(base))
		if got != "[2/true 2/false]" {
			t.Fatalf("%s: %s", base, got)
		}
		t.Logf("%s 共 4 条 limit=2 → %s", base, got)
	}
	// 不足一页 / 多于整页
	if got := fmt.Sprint(walk(fmt.Sprintf("/api/events?vendor=%d&type=new,recovered", v))); got != "[2/true 2/true 1/false]" {
		t.Fatalf("5 条: %s", got)
	}

	// MCP
	ms, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	for _, tool := range []string{"search_assets", "list_new_assets"} {
		var log []string
		args := map[string]any{"vendor": v, "limit": 2}
		for i := 0; i < 5; i++ {
			out, isErr := callTool(t, ms, tool, args)
			if isErr {
				t.Fatalf("%s %v", tool, out)
			}
			log = append(log, fmt.Sprintf("%d/%v", len(out["items"].([]any)), out["next_cursor"] != nil))
			if out["next_cursor"] == nil {
				break
			}
			args["cursor"] = out["next_cursor"]
		}
		if fmt.Sprint(log) != "[2/true 2/false]" {
			t.Fatalf("mcp %s: %v", tool, log)
		}
		t.Logf("MCP %s 共 4 条 limit=2 → %v", tool, log)
	}

	// stats 按 type 分别计数
	st := c.want(200, "GET", fmt.Sprintf("/api/events/stats?vendor=%d&type=new,recovered", v), nil)
	it := items(st)[0]
	tt := st["totals"].(map[string]any)
	if it["new"].(float64) != 4 || it["recovered"].(float64) != 1 || it["count"].(float64) != 5 ||
		tt["new"].(float64) != 4 || tt["recovered"].(float64) != 1 || st["total"].(float64) != 5 {
		t.Fatalf("stats %v", st)
	}
	t.Logf("stats：厂商 new=%v recovered=%v；totals=%v total=%v", it["new"], it["recovered"], tt, st["total"])
}
