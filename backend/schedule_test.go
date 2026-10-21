package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestScheduleConfigFromEnv(t *testing.T) {
	t.Setenv("SCHEDULE_PASSIVE", "")
	t.Setenv("SCHEDULE_ACTIVE", "")
	c, err := scheduleConfigFromEnv()
	if err != nil || c.Passive != 6*time.Hour || c.Active != 24*time.Hour {
		t.Fatalf("%v %+v", err, c)
	}
	t.Setenv("SCHEDULE_PASSIVE", "30m")
	t.Setenv("SCHEDULE_ACTIVE", "off")
	c, err = scheduleConfigFromEnv()
	if err != nil || c.Passive != 30*time.Minute || c.Active != 0 {
		t.Fatalf("%v %+v", err, c)
	}
	t.Setenv("SCHEDULE_PASSIVE", "nope")
	if _, err := scheduleConfigFromEnv(); err == nil {
		t.Fatal("want error")
	}
}

func TestSchedulerTickAndConfigChange(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	v := e.vendorID()
	e.pool.Exec(ctx, `UPDATE vendors SET active_enabled = false WHERE id = $1`, v)
	var root2 int64
	e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, $2) RETURNING id`, v, strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))+".sched.test").Scan(&root2)

	s := newScheduler(e.pool, e.client, scheduleConfig{Passive: time.Hour, Active: time.Hour}, 1)

	// passive：两个根域都触发
	n, err := s.tick(ctx, "passive")
	if err != nil || n != 2 {
		t.Fatalf("passive tick: n=%d err=%v", n, err)
	}
	var pending int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE kind = 'passive' AND status = 'pending' AND root_id IN ($1,$2)`, e.rootID, root2).Scan(&pending)
	if pending != 2 {
		t.Fatalf("pending passive %d", pending)
	}
	// 已有 pending 再 tick → 跳过
	n, err = s.tick(ctx, "passive")
	if err != nil || n != 0 {
		t.Fatalf("skip busy: n=%d err=%v", n, err)
	}

	// active：厂商未开 → 0
	n, err = s.tick(ctx, "active")
	if err != nil || n != 0 {
		t.Fatalf("active disabled: n=%d err=%v", n, err)
	}
	e.pool.Exec(ctx, `UPDATE vendors SET active_enabled = true WHERE id = $1`, v)
	n, err = s.tick(ctx, "active")
	if err != nil || n != 2 {
		t.Fatalf("active enabled: n=%d err=%v", n, err)
	}

	// 配置修改生效：长间隔改为短间隔后，loop 应在新间隔内再次 tick
	e.pool.Exec(ctx, `UPDATE scan_tasks SET status = 'done', finished_at = now() WHERE status = 'pending'`)
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(ctx2) // 初始 Passive=1h（newScheduler 时），Active=1h
	time.Sleep(30 * time.Millisecond)
	before, _ := countKind(e, "passive")
	s.SetConfig(scheduleConfig{Passive: 40 * time.Millisecond, Active: 0})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		after, _ := countKind(e, "passive")
		if after > before {
			t.Logf("config change生效：passive 任务 %d → %d", before, after)
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("config change did not trigger within 2s")
}

func countKind(e *testEnv, kind string) (int, error) {
	var n int
	err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM scan_tasks WHERE kind = $1`, kind).Scan(&n)
	return n, err
}
