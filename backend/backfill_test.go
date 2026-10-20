package main

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// 迁移 006：绕过触发器写入旧格式数据，执行回刷 SQL，验证规范化与重复合并。
func TestBackfillHosts(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	sql, err := migrationFS.ReadFile("migrations/006_backfill_hosts.sql")
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		e.pool.Exec(context.Background(), `DELETE FROM assets WHERE lower(subdomain) LIKE '%bf.example.test%'`)
	}
	clean()
	t.Cleanup(clean)
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	c := conn.Conn()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := c.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mustExec(`ALTER TABLE assets DISABLE TRIGGER normalize_host`)
	mustExec(`ALTER TABLE urls DISABLE TRIGGER normalize_host`)
	defer c.Exec(context.Background(), `ALTER TABLE assets ENABLE TRIGGER normalize_host`)
	defer c.Exec(context.Background(), `ALTER TABLE urls ENABLE TRIGGER normalize_host`)
	var keepID, dupID, soloID int64
	c.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, first_seen, last_seen) VALUES ($1, 'www.bf.example.test', '{crtsh}', $2, $2) RETURNING id`,
		e.rootID, t0.Add(time.Hour)).Scan(&keepID)
	c.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, first_seen, last_seen) VALUES ($1, 'WWW.BF.Example.Test.', '{fofa}', $2, $3) RETURNING id`,
		e.rootID, t0, t0.Add(2*time.Hour)).Scan(&dupID)
	c.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, ' Solo.BF.example.test ') RETURNING id`, e.rootID).Scan(&soloID)
	var u1, u2 int64
	c.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, sources) VALUES ($1, 'https', 'www.bf.example.test', 443, '{probe}') RETURNING id`, keepID).Scan(&u1)
	c.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, sources) VALUES ($1, 'https', 'WWW.bf.example.test.', 443, '{old}') RETURNING id`, dupID).Scan(&u2)
	mustExec(`INSERT INTO events (type, asset_id, url_id) VALUES ('new', $1, $2)`, dupID, u2)
	mustExec(`ALTER TABLE assets ENABLE TRIGGER normalize_host`)
	mustExec(`ALTER TABLE urls ENABLE TRIGGER normalize_host`)

	for i := 0; i < 2; i++ { // 可重复执行
		mustExec(string(sql))
	}

	var n int
	c.QueryRow(ctx, `SELECT count(*) FROM assets WHERE id = $1`, dupID).Scan(&n)
	if n != 0 {
		t.Fatal("duplicate asset not merged")
	}
	var sources []string
	var first, last time.Time
	c.QueryRow(ctx, `SELECT sources, first_seen, last_seen FROM assets WHERE id = $1`, keepID).Scan(&sources, &first, &last)
	if !reflect.DeepEqual(sources, []string{"crtsh", "fofa"}) || !first.Equal(t0) || !last.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("merged asset: %v %v %v", sources, first, last)
	}
	var solo string
	c.QueryRow(ctx, `SELECT subdomain FROM assets WHERE id = $1`, soloID).Scan(&solo)
	if solo != "solo.bf.example.test" {
		t.Fatalf("solo = %q", solo)
	}
	c.QueryRow(ctx, `SELECT count(*) FROM urls WHERE id = $1`, u2).Scan(&n)
	var usrc []string
	var uAsset int64
	c.QueryRow(ctx, `SELECT sources, asset_id FROM urls WHERE id = $1`, u1).Scan(&usrc, &uAsset)
	if n != 0 || !reflect.DeepEqual(usrc, []string{"old", "probe"}) || uAsset != keepID {
		t.Fatalf("url merge: dup=%d sources=%v asset=%d", n, usrc, uAsset)
	}
	var evAsset, evURL int64
	c.QueryRow(ctx, `SELECT asset_id, url_id FROM events WHERE asset_id = $1`, keepID).Scan(&evAsset, &evURL)
	if evAsset != keepID || evURL != u1 {
		t.Fatalf("event repointed to asset %d url %d", evAsset, evURL)
	}
	c.Exec(ctx, `DELETE FROM events WHERE asset_id = $1`, keepID)
}
