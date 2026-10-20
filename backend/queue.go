package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

// 三类扫描，各自一个 Asynq 队列和独立限速。
var scanKinds = []string{"passive", "dns", "http"}

// 默认限速（任务/秒），可用 RATE_PASSIVE / RATE_DNS / RATE_HTTP 覆盖。
var defaultRates = map[string]float64{"passive": 1, "dns": 10, "http": 5}

type scanPayload struct {
	ScanID int64 `json:"scan_id"`
}

// ScanHandler 执行一类扫描的具体工作（T5+ 实现），scanID 对应 scan_tasks.id。
type ScanHandler func(ctx context.Context, scanID int64) error

// ratesFromEnv 读取每类 worker 的限速配置。
func ratesFromEnv() (map[string]rate.Limit, error) {
	rates := map[string]rate.Limit{}
	for _, k := range scanKinds {
		v := defaultRates[k]
		if s := os.Getenv("RATE_" + strings.ToUpper(k)); s != "" {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || f <= 0 {
				return nil, fmt.Errorf("RATE_%s: invalid %q", strings.ToUpper(k), s)
			}
			v = f
		}
		rates[k] = rate.Limit(v)
	}
	return rates, nil
}

// TriggerScan 手动触发扫描：写入 scan_tasks（pending）并投递到对应队列，返回任务 id。
func TriggerScan(ctx context.Context, d *pgxpool.Pool, client *asynq.Client, rootID int64, kind string) (int64, error) {
	if !validKind(kind) {
		return 0, fmt.Errorf("unknown scan kind %q", kind)
	}
	var id int64
	err := d.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind)
		SELECT vendor_id, id, $2 FROM root_domains WHERE id = $1 RETURNING id`, rootID, kind).Scan(&id)
	if err != nil {
		return 0, err
	}
	payload, _ := json.Marshal(scanPayload{ScanID: id})
	if _, err := client.EnqueueContext(ctx, asynq.NewTask("scan:"+kind, payload), asynq.Queue(kind)); err != nil {
		d.Exec(ctx, `UPDATE scan_tasks SET status = 'failed', error = $2, finished_at = now() WHERE id = $1`, id, err.Error())
		return 0, err
	}
	return id, nil
}

func validKind(kind string) bool {
	for _, k := range scanKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// rateLimit 返回一个中间件：同一类任务共享一个限速器，处理前等待令牌。
func rateLimit(limiters map[string]*rate.Limiter) asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
			if l := limiters[t.Type()[len("scan:"):]]; l != nil {
				if err := l.Wait(ctx); err != nil {
					return err
				}
			}
			return next.ProcessTask(ctx, t)
		})
	}
}

// newWorker 创建处理三类队列的 Asynq server 和 mux；handlers 缺省的类型只更新状态。
func newWorker(redisOpt asynq.RedisClientOpt, d *pgxpool.Pool, rates map[string]rate.Limit, handlers map[string]ScanHandler) (*asynq.Server, *asynq.ServeMux) {
	queues := map[string]int{}
	limiters := map[string]*rate.Limiter{}
	for _, k := range scanKinds {
		queues[k] = 1
		limiters[k] = rate.NewLimiter(rates[k], 1)
	}
	srv := asynq.NewServer(redisOpt, asynq.Config{Queues: queues, Concurrency: 10})
	mux := asynq.NewServeMux()
	mux.Use(rateLimit(limiters))
	for _, k := range scanKinds {
		h := handlers[k]
		mux.HandleFunc("scan:"+k, func(ctx context.Context, t *asynq.Task) error {
			var p scanPayload
			if err := json.Unmarshal(t.Payload(), &p); err != nil {
				return fmt.Errorf("%v: %w", err, asynq.SkipRetry)
			}
			if _, err := d.Exec(ctx, `UPDATE scan_tasks SET status = 'running', started_at = now() WHERE id = $1`, p.ScanID); err != nil {
				return err
			}
			var err error
			if h != nil {
				err = h(ctx, p.ScanID)
			}
			if err != nil {
				d.Exec(ctx, `UPDATE scan_tasks SET status = 'failed', error = $2, finished_at = now() WHERE id = $1`, p.ScanID, err.Error())
				return err
			}
			_, err = d.Exec(ctx, `UPDATE scan_tasks SET status = 'done', finished_at = now() WHERE id = $1`, p.ScanID)
			return err
		})
	}
	return srv, mux
}
