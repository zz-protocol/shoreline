package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// 需要本地 Postgres：TEST_DATABASE_URL=postgres://... go test ./...
func TestMigrate(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	// 整个 schema 重建：只删部分表时 CASCADE 会把留存表（如 root_candidates）的外键删掉，而 CREATE TABLE IF NOT EXISTS 不会补回
	if _, err := conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // 可重复执行
		if err := migrate(ctx, conn); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	// 历史迁移记录被清掉后再执行一次，SQL 本身也要幂等
	if _, err := conn.Exec(ctx, "DELETE FROM schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, conn); err != nil {
		t.Fatalf("rerun sql: %v", err)
	}

	// 测试用了显式 id，结束时清空数据并重置序列，避免影响其他测试
	t.Cleanup(func() {
		conn.Exec(context.Background(), `TRUNCATE vendors, root_domains, scope_rules, assets, urls, events, scan_tasks RESTART IDENTITY CASCADE`)
	})
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustFail := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err == nil {
			t.Fatalf("expected unique violation: %s", sql)
		}
	}
	mustExec(`INSERT INTO vendors (id, name) VALUES (1, 'test-vendor')`)
	mustExec(`INSERT INTO root_domains (id, vendor_id, domain) VALUES (1, 1, 'example.test')`)
	mustExec(`INSERT INTO scope_rules (vendor_id, kind, pattern) VALUES (1, 'include', '*.example.test')`)
	mustFail(`INSERT INTO scope_rules (vendor_id, kind, pattern) VALUES (1, 'include', '*.example.test')`)
	mustExec(`INSERT INTO assets (id, root_id, subdomain, sources) VALUES (1, 1, 'a.example.test', '{crtsh}')`)
	mustFail(`INSERT INTO assets (root_id, subdomain) VALUES (1, 'a.example.test')`)
	mustExec(`INSERT INTO urls (id, asset_id, scheme, host, port) VALUES (1, 1, 'https', 'a.example.test', 443)`)
	mustFail(`INSERT INTO urls (asset_id, scheme, host, port) VALUES (1, 'https', 'a.example.test', 443)`)
	mustExec(`INSERT INTO urls (asset_id, scheme, host, port) VALUES (1, 'http', 'a.example.test', 80)`)
	mustExec(`INSERT INTO events (type, asset_id) VALUES ('new', 1)`)
	mustExec(`INSERT INTO scan_tasks (vendor_id, root_id, kind) VALUES (1, 1, 'passive')`)

	var firstSeen, lastSeen bool
	var sources []string
	if err := conn.QueryRow(ctx, `SELECT first_seen IS NOT NULL, last_seen IS NOT NULL, sources FROM assets WHERE id = 1`).
		Scan(&firstSeen, &lastSeen, &sources); err != nil {
		t.Fatal(err)
	}
	if !firstSeen || !lastSeen || len(sources) != 1 || sources[0] != "crtsh" {
		t.Fatalf("asset fields: %v %v %v", firstSeen, lastSeen, sources)
	}
}

// 新库迁移后所有引用 vendors 的表都带外键（防止测试或迁移顺序把外键弄丢）。
func TestMigrateForeignKeys(t *testing.T) {
	e := newTestEnv(t)
	var missing []string
	rows, _ := e.pool.Query(context.Background(), `SELECT c.relname FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid
		WHERE a.attname = 'vendor_id' AND c.relkind = 'r' AND c.relnamespace = 'public'::regnamespace
		AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'f' AND a.attnum = ANY(k.conkey))`)
	missing, _ = pgxCollectStrings(rows)
	if len(missing) > 0 {
		t.Fatalf("vendor_id without FK: %v", missing)
	}
}
