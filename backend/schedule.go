package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scheduleConfig：定时调度间隔。被动默认 6h，主动默认 24h；0 表示关闭该类定时。
// 环境变量 SCHEDULE_PASSIVE / SCHEDULE_ACTIVE，Go duration（如 6h、24h、30m）；非法启动退出。
type scheduleConfig struct {
	Passive time.Duration
	Active  time.Duration
}

func scheduleConfigFromEnv() (scheduleConfig, error) {
	c := scheduleConfig{Passive: 6 * time.Hour, Active: 24 * time.Hour}
	parse := func(name string, dst *time.Duration) error {
		s := os.Getenv(name)
		if s == "" {
			return nil
		}
		if s == "0" || s == "off" {
			*dst = 0
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return fmt.Errorf("%s: invalid %q (Go duration, or 0/off to disable)", name, s)
		}
		*dst = d
		return nil
	}
	if err := parse("SCHEDULE_PASSIVE", &c.Passive); err != nil {
		return c, err
	}
	if err := parse("SCHEDULE_ACTIVE", &c.Active); err != nil {
		return c, err
	}
	return c, nil
}

// scheduler 按间隔为所有根域触发 passive，为 active_enabled 厂商的根域触发 active。
// 已有 pending/running 同 kind 任务则跳过。SetConfig 可热更新间隔（测试「配置修改生效」）。
type scheduler struct {
	db       *pgxpool.Pool
	client   *asynq.Client
	maxRetry int

	mu     sync.Mutex
	cfg    scheduleConfig
	reload chan struct{}
}

func newScheduler(db *pgxpool.Pool, client *asynq.Client, cfg scheduleConfig, maxRetry int) *scheduler {
	return &scheduler{db: db, client: client, maxRetry: maxRetry, cfg: cfg, reload: make(chan struct{})}
}

func (s *scheduler) Config() scheduleConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// SetConfig 热更新间隔并广播打断所有等待 loop，使新间隔尽快生效。
func (s *scheduler) SetConfig(c scheduleConfig) {
	s.mu.Lock()
	s.cfg = c
	close(s.reload)
	s.reload = make(chan struct{})
	s.mu.Unlock()
}

func (s *scheduler) reloadCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reload
}

func (s *scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.loop(ctx, "passive") }()
	go func() { defer wg.Done(); s.loop(ctx, "active") }()
	wg.Wait()
}

func (s *scheduler) loop(ctx context.Context, kind string) {
	for {
		s.mu.Lock()
		d := s.cfg.Passive
		if kind == "active" {
			d = s.cfg.Active
		}
		s.mu.Unlock()
		if d <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.reloadCh():
				continue
			}
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.reloadCh():
			timer.Stop()
			continue
		case <-timer.C:
			n, err := s.tick(ctx, kind)
			if err != nil {
				log.Printf("schedule %s: %v", kind, err)
			} else if n > 0 {
				log.Printf("schedule %s: triggered %d roots", kind, n)
			}
		}
	}
}

// tick 为符合条件的根域触发扫描；返回触发数量。可单测直接调用。
func (s *scheduler) tick(ctx context.Context, kind string) (int, error) {
	if kind != "passive" && kind != "active" {
		return 0, fmt.Errorf("schedule kind %q not supported", kind)
	}
	q := `SELECT r.id FROM root_domains r`
	if kind == "active" {
		q += ` JOIN vendors v ON v.id = r.vendor_id WHERE v.active_enabled`
	}
	q += ` ORDER BY r.id`
	rows, err := s.db.Query(ctx, q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var roots []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		roots = append(roots, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, rootID := range roots {
		var busy int
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM scan_tasks
			WHERE root_id = $1 AND kind = $2 AND status IN ('pending', 'running')`, rootID, kind).Scan(&busy); err != nil {
			return n, err
		}
		if busy > 0 {
			continue
		}
		if _, err := TriggerScan(ctx, s.db, s.client, rootID, kind, s.maxRetry); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
