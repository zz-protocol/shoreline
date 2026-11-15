package main

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestT51ScheduleServiceDefaultOff(t *testing.T) {
	t.Setenv("SCHEDULE_PASSIVE", "")
	t.Setenv("SCHEDULE_ACTIVE", "")
	t.Setenv("SCHEDULE_ALIVE", "")
	t.Setenv("SCHEDULE_SERVICE", "")
	c, err := scheduleConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Service != 0 {
		t.Fatalf("SCHEDULE_SERVICE empty must disable, got %v", c.Service)
	}
	t.Setenv("SCHEDULE_SERVICE", "30m")
	c, err = scheduleConfigFromEnv()
	if err != nil || c.Service != 30*time.Minute {
		t.Fatalf("SCHEDULE_SERVICE=30m: %v %+v", err, c)
	}
	t.Setenv("SCHEDULE_SERVICE", "off")
	c, err = scheduleConfigFromEnv()
	if err != nil || c.Service != 0 {
		t.Fatalf("off should disable: %v %+v", err, c)
	}
	t.Setenv("SCHEDULE_SERVICE", "nope")
	if _, err := scheduleConfigFromEnv(); err == nil {
		t.Fatal("invalid SCHEDULE_SERVICE should be rejected")
	}

	e := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newScheduler(e.pool, e.client, scheduleConfig{Passive: 0, Active: 0, Alive: 0, Service: 0}, 1)
	go s.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE kind = 'service' AND root_id = $1`, e.rootID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unconfigured schedule must not enqueue service, got %d", n)
	}
}

func TestT51ScheduleServiceIntervalAndDisabledDials(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	host := "svc." + domain
	t51InsertAsset(t, e, host, false)

	var dials atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306},
		RPS:     100,
		Timeout: 200 * time.Millisecond,
		Dialer: func(ctx context.Context, network, host string, port int) (net.Conn, error) {
			dials.Add(1)
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
		},
	}
	rates := map[string]rate.Limit{"passive": 100, "active": 100, "dns": 100, "http": 100, "expand": 100, "service": 100}
	e.start(t, testConfig(rates), map[string]ScanHandler{"service": serviceScanHandler(e.pool, cfg)})

	s := newScheduler(e.pool, e.client, scheduleConfig{Service: 40 * time.Millisecond}, 1)
	// 配置后 tick 入队 service（开关默认关）。
	n, err := s.tick(ctx, "service")
	if err != nil || n < 1 {
		t.Fatalf("tick service: n=%d err=%v", n, err)
	}
	var pending int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE kind = 'service' AND root_id = $1`, e.rootID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending < 1 {
		t.Fatal("configured schedule must enqueue service")
	}
	e.waitStatus(t, "service", "done", 1)
	if dials.Load() != 0 {
		t.Fatalf("switch off: scheduled service must not dial, dials=%d", dials.Load())
	}

	// 已有 pending/running 跳过；完成后 loop 按间隔再入队。
	e.pool.Exec(ctx, `UPDATE scan_tasks SET status = 'done', finished_at = now() WHERE kind = 'service'`)
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(ctx2)
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE kind = 'service' AND root_id = $1`, e.rootID).Scan(&after)
		if after >= 2 {
			cancel()
			t.Logf("interval enqueued again, tasks=%d", after)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("schedule interval did not enqueue again, tasks=%d", after)
}

func TestT51ScheduleServiceDoesNotNeedVendorLock(t *testing.T) {
	// 并发规则与 passive 相同：同一根域已有 pending 才跳过，不做厂商级 409。
	e := newTestEnv(t)
	ctx := context.Background()
	s := newScheduler(e.pool, e.client, scheduleConfig{Service: time.Hour}, 1)
	n, err := s.tick(ctx, "service")
	if err != nil || n < 1 {
		t.Fatalf("first tick: n=%d err=%v", n, err)
	}
	n, err = s.tick(ctx, "service")
	if err != nil {
		t.Fatal(err)
	}
	var pending int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE kind = 'service' AND root_id = $1 AND status = 'pending'`, e.rootID).Scan(&pending)
	if pending != 1 {
		t.Fatalf("busy root should skip, pending=%d second-tick-n=%d", pending, n)
	}
}
