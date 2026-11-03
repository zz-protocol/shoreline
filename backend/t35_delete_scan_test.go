package main

import (
	"context"
	"fmt"
	"testing"
)

// T35：DELETE /api/scan-tasks/{id}：终态 204，pending/running/retrying 409；只删一条；不影响 assets/urls/events。
func TestT35DeleteScanTask(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	v := e.vendorID()
	mk := func(status string, parent *int64) int64 {
		t.Helper()
		var id int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, status, parent_id, root_scan_id)
			VALUES ($1, $2, 'passive', $3, $4, $4) RETURNING id`, v, e.rootID, status, parent).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	var aid, uid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, 'a.t35.example.test') RETURNING id`, e.rootID).Scan(&aid); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port) VALUES ($1, 'http', 'a.t35.example.test', 80) RETURNING id`, aid).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := emitNew(ctx, e.pool, aid, &uid, map[string]any{"subdomain": "a.t35.example.test"}); err != nil {
		t.Fatal(err)
	}
	counts := func() string {
		var a, u, ev int
		e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM assets), (SELECT count(*) FROM urls), (SELECT count(*) FROM events)`).Scan(&a, &u, &ev)
		return fmt.Sprint(a, u, ev)
	}
	before := counts()
	exists := func(id int64) (ok bool) {
		e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scan_tasks WHERE id = $1)`, id).Scan(&ok)
		return
	}

	for _, st := range []string{"pending", "running", "retrying"} {
		id := mk(st, nil)
		c.want(409, "DELETE", fmt.Sprintf("/api/scan-tasks/%d", id), nil)
		if !exists(id) {
			t.Fatalf("%s task deleted", st)
		}
	}
	for _, st := range []string{"done", "failed"} {
		id := mk(st, nil)
		c.want(204, "DELETE", fmt.Sprintf("/api/scan-tasks/%d", id), nil)
		if exists(id) {
			t.Fatalf("%s task not deleted", st)
		}
	}
	c.want(404, "DELETE", "/api/scan-tasks/999999999", nil)
	c.want(400, "DELETE", "/api/scan-tasks/abc", nil)

	// 终态父任务仍有进行中的下游：409；下游结束后可删，且只删父这一条（下游保留，不被级联删）。
	parent := mk("done", nil)
	child := mk("running", &parent)
	c.want(409, "DELETE", fmt.Sprintf("/api/scan-tasks/%d", parent), nil)
	e.pool.Exec(ctx, `UPDATE scan_tasks SET status = 'done' WHERE id = $1`, child)
	c.want(204, "DELETE", fmt.Sprintf("/api/scan-tasks/%d", parent), nil)
	if exists(parent) || !exists(child) {
		t.Fatalf("parent exists=%v child exists=%v", exists(parent), exists(child))
	}
	if after := counts(); after != before {
		t.Fatalf("assets/urls/events changed: %s -> %s", before, after)
	}
}
