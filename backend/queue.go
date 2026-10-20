package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

// 三类扫描，各自一个 Asynq 队列和独立限速。
var scanKinds = []string{"passive", "dns", "http"}

// 自动串联：passive 发现的子域交给 dns，dns 输出（解析成功且非泛解析）交给 http。
var nextKind = map[string]string{"passive": "dns", "dns": "http"}

// 默认限速（任务/秒，单实例），可用 RATE_PASSIVE / RATE_DNS / RATE_HTTP 覆盖。
var defaultRates = map[string]float64{"passive": 1, "dns": 10, "http": 5}

type scanPayload struct {
	ScanID int64    `json:"scan_id"`
	Hosts  []string `json:"hosts,omitempty"` // 上一阶段交下来的主机名；passive 为空
}

// ScanHandler 执行一类扫描（T5+ 实现）。hosts 为上一阶段的输入；
// 返回值交给下一阶段（dns 的返回值应已过滤掉解析失败和泛解析，见 T7）。
type ScanHandler func(ctx context.Context, scanID int64, hosts []string) ([]string, error)

// workerConfig 汇总 worker 可配置项。
type workerConfig struct {
	Rates      map[string]rate.Limit
	MaxRetry   int           // MAX_RETRY，默认 3
	RetryBase  time.Duration // RETRY_BASE_SECONDS，默认 1s，最小 1s；第 k 次重试前等待 base*2^(k-1)
	Concurrent int
	PollDelay  time.Duration // 到期重试任务的检查间隔，0 为 Asynq 默认（5s），测试中调小
}

func configFromEnv() (workerConfig, error) {
	c := workerConfig{Rates: map[string]rate.Limit{}, MaxRetry: 3, RetryBase: time.Second, Concurrent: 10}
	for _, k := range scanKinds {
		v := defaultRates[k]
		name := "RATE_" + strings.ToUpper(k)
		if s := os.Getenv(name); s != "" {
			f, err := strconv.ParseFloat(s, 64)
			if !validPositive(f, err) {
				return c, fmt.Errorf("%s: invalid %q", name, s)
			}
			v = f
		}
		c.Rates[k] = rate.Limit(v)
	}
	if s := os.Getenv("MAX_RETRY"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return c, fmt.Errorf("MAX_RETRY: invalid %q", s)
		}
		c.MaxRetry = n
	}
	if s := os.Getenv("RETRY_BASE_SECONDS"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if !validPositive(f, err) || f < 1 { // asynq 重试调度为秒级精度，<1s 会被截断
			return c, fmt.Errorf("RETRY_BASE_SECONDS: invalid %q (must be >= 1)", s)
		}
		c.RetryBase = time.Duration(f * float64(time.Second))
	}
	return c, nil
}

// validPositive 拒绝解析失败、NaN、±Inf 和 <= 0。
func validPositive(f float64, err error) bool {
	return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0
}

// retryDelay 指数退避：base * 2^n。n 为 asynq 传入的已重试次数，首次失败为 0。
func retryDelay(base time.Duration, n int) time.Duration {
	return time.Duration(float64(base) * math.Pow(2, float64(n)))
}

// TriggerScan 手动触发扫描：写入 scan_tasks（pending）并投递到对应队列，返回任务 id。
func TriggerScan(ctx context.Context, d *pgxpool.Pool, client *asynq.Client, rootID int64, kind string, maxRetry int) (int64, error) {
	return enqueueScan(ctx, d, client, rootID, kind, nil, maxRetry)
}

func enqueueScan(ctx context.Context, d *pgxpool.Pool, client *asynq.Client, rootID int64, kind string, hosts []string, maxRetry int) (int64, error) {
	if !validKind(kind) {
		return 0, fmt.Errorf("unknown scan kind %q", kind)
	}
	var id int64
	err := d.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind)
		SELECT vendor_id, id, $2 FROM root_domains WHERE id = $1 RETURNING id`, rootID, kind).Scan(&id)
	if err != nil {
		return 0, err
	}
	payload, _ := json.Marshal(scanPayload{ScanID: id, Hosts: hosts})
	task := asynq.NewTask("scan:"+kind, payload)
	if _, err := client.EnqueueContext(ctx, task, asynq.Queue(kind), asynq.MaxRetry(maxRetry)); err != nil {
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
			if l := limiters[strings.TrimPrefix(t.Type(), "scan:")]; l != nil {
				if err := l.Wait(ctx); err != nil {
					return err
				}
			}
			return next.ProcessTask(ctx, t)
		})
	}
}

// newWorker 创建处理三类队列的 Asynq server 和 mux；handlers 缺省的类型不产出、不串联。
func newWorker(redisOpt asynq.RedisClientOpt, d *pgxpool.Pool, cfg workerConfig, handlers map[string]ScanHandler) (*asynq.Server, *asynq.ServeMux) {
	queues := map[string]int{}
	limiters := map[string]*rate.Limiter{}
	for _, k := range scanKinds {
		queues[k] = 1
		limiters[k] = rate.NewLimiter(cfg.Rates[k], 1)
	}
	srv := asynq.NewServer(redisOpt, asynq.Config{
		Queues:                   queues,
		Concurrency:              cfg.Concurrent,
		DelayedTaskCheckInterval: cfg.PollDelay,
		RetryDelayFunc: func(n int, _ error, _ *asynq.Task) time.Duration {
			return retryDelay(cfg.RetryBase, n)
		},
	})
	client := asynq.NewClient(redisOpt)
	mux := asynq.NewServeMux()
	mux.Use(rateLimit(limiters))
	for _, k := range scanKinds {
		kind, h := k, handlers[k]
		mux.HandleFunc("scan:"+kind, func(ctx context.Context, t *asynq.Task) error {
			var p scanPayload
			if err := json.Unmarshal(t.Payload(), &p); err != nil {
				return fmt.Errorf("%v: %w", err, asynq.SkipRetry)
			}
			// failed/done 为终态：已结束的任务不再执行、不再变更
			tag, err := d.Exec(ctx, `UPDATE scan_tasks SET status = 'running',
				started_at = coalesce(started_at, now()) WHERE id = $1 AND status NOT IN ('failed', 'done')`, p.ScanID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return nil
			}
			var out []string
			if h != nil {
				out, err = h(ctx, p.ScanID, p.Hosts)
			}
			if err == nil && len(out) > 0 && nextKind[kind] != "" {
				var rootID int64
				if err = d.QueryRow(ctx, `SELECT root_id FROM scan_tasks WHERE id = $1`, p.ScanID).Scan(&rootID); err == nil {
					_, err = enqueueScan(ctx, d, client, rootID, nextKind[kind], out, cfg.MaxRetry)
				}
			}
			if err != nil {
				retried, _ := asynq.GetRetryCount(ctx)
				maxRetry, _ := asynq.GetMaxRetry(ctx)
				if retried >= maxRetry || errors.Is(err, asynq.SkipRetry) { // 重试用尽：failed（终态）
					d.Exec(ctx, `UPDATE scan_tasks SET status = 'failed', error = $2, finished_at = now() WHERE id = $1`, p.ScanID, err.Error())
					return fmt.Errorf("%v: %w", err, asynq.SkipRetry)
				}
				d.Exec(ctx, `UPDATE scan_tasks SET status = 'retrying', error = $2 WHERE id = $1`, p.ScanID, err.Error())
				return err
			}
			_, err = d.Exec(ctx, `UPDATE scan_tasks SET status = 'done', error = NULL, finished_at = now() WHERE id = $1`, p.ScanID)
			return err
		})
	}
	return srv, mux
}
