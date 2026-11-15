package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// M3b：库里开关显式 false 且已登记范围时，planOriginServiceScan 不入队、不记逐主机跳过。
func TestT50PlanDisabledRecordsNoSkips(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	base := "m3b." + strings.ToLower(t.Name()) + ".example.test"
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_scopes (pattern) VALUES ($1)`, base); err != nil {
		t.Fatal(err)
	}
	targets := []serviceScanTarget{{Host: "a." + base}, {Host: "x.outside.example.test"}}
	q, err := planOriginServiceScan(ctx, e.pool, targets)
	if err != nil || len(q) != 0 {
		t.Fatalf("disabled queue=%v err=%v", q, err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM service_scan_skips`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("disabled must not record skips, got %d", n)
	}
}

// M4b：开关开时范围外主机记字面量 "out_of_scope"，且不探测。
func TestT50OriginOutOfScopeLiteralReason(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_scopes (pattern) VALUES ('m4b-in.example.test')`); err != nil {
		t.Fatal(err)
	}
	out := "x.m4b-out.example.test"
	var dials atomic.Int32
	if err := runOriginServiceScan(ctx, e.pool, []serviceScanTarget{{Host: out}}, func(context.Context, string) error {
		dials.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("out-of-scope probed, dials=%d", dials.Load())
	}
	var reason string
	if err := e.pool.QueryRow(ctx, `SELECT reason FROM service_scan_skips WHERE host = $1`, out).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "out_of_scope" {
		t.Fatalf("reason %q, want out_of_scope", reason)
	}
}

// M6：库停在 025（无 cdn_fronted 列、026 未记录）时已有 cdn 非空的资产，执行 026 后 cdn_fronted=true。
func TestT50Migration026Backfill(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	t.Cleanup(func() {
		c, err := e.pool.Acquire(context.Background())
		if err != nil {
			return
		}
		defer c.Release()
		_ = migrate(context.Background(), c.Conn())
	})
	if _, err := conn.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS cdn_fronted`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN ('026_cdn_fronted.sql', '030_cdn_fronted_nullable.sql')`); err != nil {
		t.Fatal(err)
	}
	host := "m6." + strings.ToLower(t.Name()) + ".example.test"
	plain := "m6plain." + strings.ToLower(t.Name()) + ".example.test"
	if _, err := conn.Exec(ctx, `INSERT INTO assets (root_id, subdomain, sources, cdn) VALUES ($1, $2, '{dns}', 'cloudflare'), ($1, $3, '{dns}', NULL)`,
		e.rootID, host, plain); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, conn.Conn()); err != nil {
		t.Fatal(err)
	}
	var fronted bool
	var plainFronted *bool
	if err := conn.QueryRow(ctx, `SELECT cdn_fronted FROM assets WHERE subdomain = $1`, host).Scan(&fronted); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT cdn_fronted FROM assets WHERE subdomain = $1`, plain).Scan(&plainFronted); err != nil {
		t.Fatal(err)
	}
	// 026 把已有 cdn 标为前置；030 把从未判定（resolved_at 为空）的默认 false 改回 NULL。
	if !fronted || plainFronted != nil {
		t.Fatalf("026+030 backfill: cdn asset fronted=%v, unjudged plain=%v", fronted, plainFronted)
	}
}
