package main

import (
	"context"
	"strings"
	"testing"
)

func TestT57NormalizeServiceBannerForHash(t *testing.T) {
	base := "HTTP/1.1 403 Forbidden..Server: Apache/2.4.57..Date: Wed, 15 Nov 2026 08:00:00 GMT.." +
		"Request_id: 7f3a1b..Node_info: node-a..Client_ip: [ip]..Content-Length: 150..Via: 1.1 edge-node-a..Content-Type: text/html...."
	other := "HTTP/1.1 403 Forbidden..Server: Apache/2.4.57..Date: Thu, 16 Nov 2026 09:10:11 GMT.." +
		"Request_id: 9c2d4e..Node_info: node-b..Client_ip: [ip]..Content-Length: 999..Via: 1.1 edge-node-b..Content-Type: text/html...."
	a := normalizeServiceBannerForHash(base)
	b := normalizeServiceBannerForHash(other)
	if a != b {
		t.Fatalf("dynamic headers must normalize to the same hash input:\n%q\n%q", a, b)
	}
	if !strings.Contains(a, "Server: Apache/2.4.57") || !strings.Contains(a, "HTTP/1.1 403 Forbidden") {
		t.Fatalf("normalized banner lost Server/status: %q", a)
	}
	for _, dropped := range []string{"Date:", "Request_id:", "Node_info:", "Client_ip:", "Content-Length:", "Via:"} {
		if strings.Contains(a, dropped) {
			t.Fatalf("normalized banner still contains %s: %q", dropped, a)
		}
	}
	if unidentifiedServiceName(base) != unidentifiedServiceName(other) {
		t.Fatal("same-class 403 banners must share one unidentified name")
	}
	if unidentifiedServiceName("HTTP/1.1 200 OK..Server: openresty..Content-Type: application/json....{}") == unidentifiedServiceName(base) {
		t.Fatal("different real responses must keep different names")
	}
	if unidentifiedServiceName("HTTP/1.1 403 Forbidden..Server: nginx..") == unidentifiedServiceName(base) {
		t.Fatal("different Server must keep different names")
	}
}

// 迁移 034：同类 403 合并为一个候选；不同真实响应分开；confirmed / ignored 保留；重复执行幂等。
func TestT57MigrationRecluster(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	hostA := "h034-a." + domain
	hostB := "h034-b." + domain
	hostC := "h034-c." + domain
	idA := t51InsertAsset(t, e, hostA, false)
	idB := t51InsertAsset(t, e, hostB, false)
	idC := t51InsertAsset(t, e, hostC, false)

	bannerA := "HTTP/1.1 403 Forbidden..Server: Apache/2.4.57..Date: Wed, 15 Nov 2026 08:00:00 GMT.." +
		"Request_id: aaa..Node_info: node-a..Client_ip: [ip]..Content-Length: 150..Via: 1.1 edge-node-a..Content-Type: text/html...."
	bannerB := "HTTP/1.1 403 Forbidden..Server: Apache/2.4.57..Date: Thu, 16 Nov 2026 09:10:11 GMT.." +
		"Request_id: bbb..Node_info: node-b..Client_ip: [ip]..Content-Length: 999..Via: 1.1 edge-node-b..Content-Type: text/html...."
	bannerC := "HTTP/1.1 200 OK..Server: Apache/2.4.57..Content-Type: application/json....{}"

	type fixture struct {
		name    string
		assetID int64
		port    int
		banner  string
	}
	oldA := "unidentified-service-111111111111"
	oldB := "unidentified-service-222222222222"
	oldC := "unidentified-service-333333333333"
	fixtures := []fixture{
		{oldA, idA, 18091, bannerA},
		{oldB, idB, 18092, bannerB},
		{oldC, idC, 18093, bannerC},
	}
	ids := map[string]int64{}
	for _, f := range fixtures {
		var cid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed)
			VALUES ($1, $1, 'service', $2) RETURNING id`, f.name, f.name == oldA).Scan(&cid); err != nil {
			t.Fatal(err)
		}
		ids[f.name] = cid
		ignored := "NULL"
		if f.name == oldB {
			ignored = "now()"
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, source, evidence, ignored_at)
			VALUES ($1, $2, $3, 'service', jsonb_build_object('banner', $4::text), `+ignored+`)`, f.assetID, cid, f.port, f.banner); err != nil {
			t.Fatal(err)
		}
	}

	sql, err := migrationFS.ReadFile("migrations/034_unidentified_recluster.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for i := 0; i < 2; i++ {
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration rerun %d: %v", i+1, err)
		}
	}

	mergedName := unidentifiedServiceName(bannerA)
	if mergedName != unidentifiedServiceName(bannerB) {
		t.Fatal("fixture banners must share a normalized name")
	}
	var mergedID int64
	var confirmed bool
	if err := e.pool.QueryRow(ctx, `SELECT id, confirmed FROM components WHERE name_key = $1`, strings.ToLower(mergedName)).
		Scan(&mergedID, &confirmed); err != nil {
		t.Fatalf("merged component %s: %v", mergedName, err)
	}
	if !confirmed {
		t.Fatal("confirmed status must survive merge")
	}
	type assoc struct {
		assetID int64
		ignored bool
	}
	rows, err := e.pool.Query(ctx, `SELECT asset_id, ignored_at IS NOT NULL FROM asset_components
		WHERE component_id = $1 AND port BETWEEN 18091 AND 18092 ORDER BY asset_id`, mergedID)
	if err != nil {
		t.Fatal(err)
	}
	var got []assoc
	for rows.Next() {
		var a assoc
		if err := rows.Scan(&a.assetID, &a.ignored); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("merged associations=%d want 2: %v", len(got), got)
	}
	if got[0].assetID != idA || got[0].ignored {
		t.Fatalf("asset A association=%+v", got[0])
	}
	if got[1].assetID != idB || !got[1].ignored {
		t.Fatalf("asset B association must keep ignored: %+v", got[1])
	}

	otherName := unidentifiedServiceName(bannerC)
	if otherName == mergedName {
		t.Fatal("different responses must not merge")
	}
	var otherID int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM components WHERE name_key = $1`, strings.ToLower(otherName)).Scan(&otherID); err != nil {
		t.Fatalf("different response component missing: %v", err)
	}
	var otherCount int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_components WHERE component_id = $1`, otherID).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if otherCount != 1 {
		t.Fatalf("different response associations=%d want 1", otherCount)
	}

	// 重复执行后关联数不增长；旧组件行保留。
	var mergedCount int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_components WHERE component_id = $1`, mergedID).Scan(&mergedCount); err != nil {
		t.Fatal(err)
	}
	if mergedCount != 2 {
		t.Fatalf("merged associations after rerun=%d want 2", mergedCount)
	}
	var oldRows int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM components WHERE name_key = ANY($1)`, []string{oldA, oldB, oldC}).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 3 {
		t.Fatalf("old component rows=%d want 3 (no physical delete)", oldRows)
	}

	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE asset_id IN ($1,$2,$3)`, idA, idB, idC)
		e.pool.Exec(context.Background(), `DELETE FROM components WHERE name LIKE 'unidentified-service-%'
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = components.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = components.id)`)
	})
}
