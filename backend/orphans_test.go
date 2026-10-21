package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hibiken/asynq"
)

// T23：启动时 pending 但不在 asynq 里的 scan_tasks 标 failed（不重新入队）；已入队的、启动后新建的不动。
func TestFailOrphanScans(t *testing.T) {
	e, _, a := newAPITest(t, 100)
	ctx := context.Background()
	var orphan int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind) SELECT vendor_id, id, 'passive' FROM root_domains WHERE id = $1 RETURNING id`, e.rootID).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	queued, err := TriggerScan(ctx, e.pool, e.client, e.rootID, "passive", a.cfg.MaxRetry)
	if err != nil {
		t.Fatal(err)
	}
	// 已入队但为 scheduled（延迟执行）的也不动
	var sched int64
	e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind) SELECT vendor_id, id, 'dns' FROM root_domains WHERE id = $1 RETURNING id`, e.rootID).Scan(&sched)
	pl, _ := json.Marshal(scanPayload{ScanID: sched})
	if _, err := e.client.Enqueue(asynq.NewTask("scan:dns", pl), asynq.Queue("dns"), asynq.ProcessIn(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	time.Sleep(10 * time.Millisecond)
	var fresh int64
	e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind) SELECT vendor_id, id, 'dns' FROM root_domains WHERE id = $1 RETURNING id`, e.rootID).Scan(&fresh)
	n, err := failOrphanScans(ctx, e.pool, e.redis, before)
	if err != nil {
		t.Fatal(err)
	}
	st := func(id int64) (s, msg string) {
		e.pool.QueryRow(ctx, `SELECT status, COALESCE(error, '') FROM scan_tasks WHERE id = $1`, id).Scan(&s, &msg)
		return
	}
	if s, msg := st(orphan); s != "failed" || msg != orphanMsg {
		t.Fatalf("orphan %s %q", s, msg)
	}
	if s, _ := st(queued); s != "pending" {
		t.Fatalf("queued task touched: %s", s)
	}
	if s, _ := st(sched); s != "pending" {
		t.Fatalf("scheduled task touched: %s", s)
	}
	if s, _ := st(fresh); s != "pending" {
		t.Fatalf("post-start task touched: %s", s)
	}
	if n < 1 {
		t.Fatalf("n=%d", n)
	}
	// 孤儿 expand 被标 failed 后，该厂商可再次触发
	e.pool.Exec(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, created_at) SELECT vendor_id, id, 'expand', now() - interval '1 minute' FROM root_domains WHERE id = $1`, e.rootID)
	if _, err := failOrphanScans(ctx, e.pool, e.redis, time.Now()); err != nil {
		t.Fatal(err)
	}
	newID, err := a.triggerExpand(ctx, e.vendorID(), e.rootID)
	if err != nil {
		t.Fatalf("expand after orphan cleanup: %v", err)
	}
	// 已入队未开始的 expand（pending，真在 asynq 里）再跑一次清理也不动
	if _, err := failOrphanScans(ctx, e.pool, e.redis, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s, _ := st(newID); s != "pending" {
		t.Fatalf("queued expand touched: %s", s)
	}
}
