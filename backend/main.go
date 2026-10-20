package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	dbURL := os.Getenv("DATABASE_URL")
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_ADDR")})
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
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, newMux(checks)))
}

// newMux 注册路由；checks 为依赖连通检查，任一失败 /healthz 返回 503。
func newMux(checks map[string]func(context.Context) error) *http.ServeMux {
	mux := http.NewServeMux()
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
