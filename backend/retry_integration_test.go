//go:build integration

// 真实时间的重试间隔测试：依赖 asynq 调度时序，全量负载下易抖动，单独跑：
//   TEST_DATABASE_URL=... go test -tags integration -count=10 -run TestRetryIntervals .

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 真实重试间隔：base=2s、MaxRetry=3 → 相邻调用间隔约 2s/4s/8s，各自误差 -1s/+1.5s（asynq 轮询抖动 <1s，
// 用 2s 基数使相邻期望值相差 >= 2s，不再单独断言严格递增）。
func TestRetryIntervals(t *testing.T) {
	e := newTestEnv(t)
	var mu sync.Mutex
	var calls []time.Time
	cfg := testConfig(fastRates)
	cfg.RetryBase = 2 * time.Second
	e.start(t, cfg, map[string]ScanHandler{
		"dns": func(context.Context, int64, []string) ([]string, error) {
			mu.Lock()
			calls = append(calls, time.Now())
			mu.Unlock()
			return nil, errors.New("fail")
		},
	})
	TriggerScan(context.Background(), e.pool, e.client, e.rootID, "dns", 3)
	e.waitStatus(t, "dns", "failed", 1)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(calls))
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		got := calls[i+1].Sub(calls[i])
		t.Logf("retry %d interval %v (want ~%v)", i+1, got.Round(time.Millisecond), w)
		if d := got - w; d < -time.Second || d > 1500*time.Millisecond { // 全量负载下 asynq 轮询抖动：上限 +1.5s
			t.Errorf("retry %d interval %v, want %v -1s/+1.5s", i+1, got, w)
		}
	}
}
