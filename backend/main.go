package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	cfg, err := configFromEnv() // 配置非法时启动即退出
	if err != nil {
		log.Fatal(err)
	}
	pcfg, err := passiveConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	cs, err := certstreamConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	icp, err := icpSourceFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	acfg, err := activeConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	hcfg, err := httpConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	dcfg, err := dnsConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	dbURL := os.Getenv("DATABASE_URL")
	if err := runMigrations(dbURL); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_ADDR")})
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		log.Fatal(err)
	}
	handlers := map[string]ScanHandler{
		"passive": passiveHandler(pool, passiveSources(pcfg)),
		"dns":     dnsHandler(pool, newResolver(dcfg)),
		"http":    httpHandler(pool, newProber(hcfg)),
		"active":  activeHandler(pool, newActiveScanner(acfg, dcfg)),
		"expand":  expandHandler(pool, icp, dbSubsidiaries{pool}),
	}
	startedAt := time.Now()
	srv, wmux := newWorker(asynq.RedisClientOpt{Addr: os.Getenv("REDIS_ADDR")}, pool, cfg, handlers)
	go func() { log.Fatal(srv.Run(wmux)) }()
	if cs.URL != "" { // 未配置 CERTSTREAM_URL 则不启动
		client := asynq.NewClient(asynq.RedisClientOpt{Addr: os.Getenv("REDIS_ADDR")})
		go runCertstream(context.Background(), cs, pool, func(rootID int64, hosts []string) {
			if _, err := enqueueScan(context.Background(), pool, client, rootID, "dns", hosts, 0, cfg.MaxRetry); err != nil {
				log.Printf("certstream: dispatch dns: %v", err)
			}
		})
	}
	checks := map[string]func(context.Context) error{
		"postgres": func(ctx context.Context) error {
			conn, err := pgx.Connect(ctx, dbURL)
			if err != nil {
				return err
			}
			defer conn.Close(ctx)
			return conn.Ping(ctx)
		},
		"redis": func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
	}
	scfg, err := scheduleConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	apiCfg, err := apiConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	apiCfg.MaxRetry = cfg.MaxRetry
	if apiCfg.BootstrapToken == "" {
		log.Printf("API_BOOTSTRAP_TOKEN not set: only tokens in api_tokens are accepted")
	}
	asynqClient := asynq.NewClient(asynq.RedisClientOpt{Addr: os.Getenv("REDIS_ADDR")})
	a := &api{db: pool, client: asynqClient, cfg: apiCfg, bg: context.Background()}
	if scfg.Passive > 0 || scfg.Active > 0 || scfg.Alive > 0 {
		go newScheduler(pool, asynqClient, scfg, cfg.MaxRetry).Run(context.Background())
		log.Printf("scheduler: passive=%v active=%v alive=%v", scfg.Passive, scfg.Active, scfg.Alive)
	}
	if n, err := a.resumeScopeTasks(context.Background()); err != nil {
		log.Fatalf("resume scope tasks: %v", err)
	} else if n > 0 {
		log.Printf("resumed %d unfinished scope tasks", n)
	}
	if n, err := failOrphanScans(context.Background(), pool, asynq.RedisClientOpt{Addr: os.Getenv("REDIS_ADDR")}, startedAt); err != nil {
		log.Printf("orphan scans check: %v", err)
	} else if n > 0 {
		log.Printf("marked %d orphan pending scans failed", n)
	}
	if err := a.initHosted(context.Background()); err != nil {
		log.Fatalf("customer_hosted init: %v", err)
	}
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, newMux(checks, a)))
}

// newMux 注册路由；checks 为依赖连通检查，任一失败 /healthz 返回 503。
// a 为 nil 时不挂 API。
func newMux(checks map[string]func(context.Context) error, a *api) *http.ServeMux {
	mux := http.NewServeMux()
	if a != nil {
		a.register(mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for name, check := range checks {
			if err := check(ctx); err != nil {
				http.Error(w, name+": "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		w.Write([]byte("ok"))
	})
	return mux
}

// runMigrations 启动时执行迁移；数据库可能晚于后端就绪，重试 30 秒。
func runMigrations(dbURL string) error {
	ctx := context.Background()
	var err error
	for i := 0; i < 30; i++ {
		var conn *pgx.Conn
		if conn, err = pgx.Connect(ctx, dbURL); err == nil {
			err = migrate(ctx, conn)
			conn.Close(ctx)
			if err == nil {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return err
}
