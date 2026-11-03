package main

import (
	"context"
	"fmt"
	"testing"
)

func deadReasonOf(t *testing.T, e *testEnv, id int64) (alive string, reason *string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT alive, dead_reason FROM assets WHERE id = $1`, id).Scan(&alive, &reason); err != nil {
		t.Fatal(err)
	}
	return
}

// T38：DNS 失败写 nxdomain；HTTP 无响应写 no_http；复活清空；筛选两类死亡；迁移按规则回填。
func TestT38DeadReasonDNSAndHTTP(t *testing.T) {
	e := newTestEnv(t)
	r := testResolver("127.0.0.1:1")
	ctx := context.Background()
	nx := newRulesAsset(t, e, "nx.dead.example.test")
	dnsRound(t, e, r, "nx.dead.example.test", hostDNS{})
	alive, reason := deadReasonOf(t, e, nx)
	if alive != "dead" || reason == nil || *reason != "nxdomain" {
		t.Fatalf("DNS fail: alive=%s reason=%v", alive, reason)
	}
	httpHost := newRulesAsset(t, e, "http.dead.example.test")
	e.pool.Exec(ctx, `UPDATE assets SET resolved = true WHERE id = $1`, httpHost)
	if err := markAlive(ctx, e.pool, httpHost, "http.dead.example.test", false); err != nil {
		t.Fatal(err)
	}
	alive, reason = deadReasonOf(t, e, httpHost)
	if alive != "dead" || reason == nil || *reason != "no_http" {
		t.Fatalf("HTTP fail: alive=%s reason=%v", alive, reason)
	}
	if err := markAlive(ctx, e.pool, httpHost, "http.dead.example.test", true); err != nil {
		t.Fatal(err)
	}
	alive, reason = deadReasonOf(t, e, httpHost)
	if alive != "alive" || reason != nil {
		t.Fatalf("recovered: alive=%s reason=%v", alive, reason)
	}
	// 已 alive：DNS 失败不改 alive / dead_reason
	e.pool.Exec(ctx, `UPDATE assets SET dead_reason = NULL WHERE id = $1`, httpHost)
	dnsRound(t, e, r, "http.dead.example.test", hostDNS{})
	alive, reason = deadReasonOf(t, e, httpHost)
	if alive != "alive" || reason != nil {
		t.Fatalf("alive+NXDOMAIN: alive=%s reason=%v", alive, reason)
	}
}

func TestT38DeadReasonFilterAndBackfill(t *testing.T) {
	e, c, a := newAPITest(t, 100)
	ctx := context.Background()
	v := e.vendorID()
	mk := func(host, alive string, resolved bool, wantReason string) int64 {
		id := newRulesAsset(t, e, host)
		e.pool.Exec(ctx, `UPDATE assets SET alive = $2, resolved = $3, dead_reason = NULL WHERE id = $1`, id, alive, resolved)
		return id
	}
	nxID := mk("bf-nx.dead.example.test", "dead", false, "nxdomain")
	httpID := mk("bf-http.dead.example.test", "dead", true, "no_http")
	aliveID := mk("bf-alive.dead.example.test", "alive", false, "")
	unkID := mk("bf-unk.dead.example.test", "unknown", true, "")
	// 再塞一条已写死原因的，回填不得覆盖
	kept := mk("bf-kept.dead.example.test", "dead", false, "")
	e.pool.Exec(ctx, `UPDATE assets SET dead_reason = 'no_http' WHERE id = $1`, kept)

	sql, err := migrationFS.ReadFile("migrations/019_dead_reason.sql")
	if err != nil {
		t.Fatal(err)
	}
	// 只重跑 UPDATE（列已存在）；CHECK/ADD 可重复
	if _, err := e.pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	check := func(id int64, wantAlive string, want *string) {
		alive, reason := deadReasonOf(t, e, id)
		if alive != wantAlive {
			t.Fatalf("id=%d alive=%s want %s", id, alive, wantAlive)
		}
		if (reason == nil) != (want == nil) || (reason != nil && *reason != *want) {
			t.Fatalf("id=%d reason=%v want %v", id, reason, want)
		}
	}
	nx, nh := "nxdomain", "no_http"
	check(nxID, "dead", &nx)
	check(httpID, "dead", &nh)
	check(aliveID, "alive", nil) // 非 dead 清空
	check(unkID, "unknown", nil)
	check(kept, "dead", &nh) // 已有值不覆盖

	list := items(c.want(200, "GET", fmt.Sprintf("/api/assets?vendor=%d&q=dead.example.test&dead_reason=nxdomain&include_wildcard=true&include_customer_hosted=true", v), nil))
	if len(list) != 1 || list[0]["id"].(float64) != float64(nxID) || list[0]["dead_reason"] != "nxdomain" {
		t.Fatalf("nxdomain filter: %v", list)
	}
	list = items(c.want(200, "GET", fmt.Sprintf("/api/assets?vendor=%d&q=dead.example.test&alive=dead&dead_reason=no_http&include_wildcard=true&include_customer_hosted=true", v), nil))
	ids := map[float64]bool{}
	for _, it := range list {
		ids[it["id"].(float64)] = true
	}
	if !ids[float64(httpID)] || !ids[float64(kept)] || ids[float64(nxID)] {
		t.Fatalf("no_http filter ids=%v", ids)
	}
	c.want(400, "GET", "/api/assets?dead_reason=dns", nil)
	_, err = a.queryAssets(ctx, assetFilter{Limit: 50, DeadReason: "dns"})
	if err == nil || err.Error() != "dead_reason must be nxdomain|no_http" {
		t.Fatalf("mcp err %v", err)
	}
	t.Logf("backfill+filter ok; nx=%d http=%d", nxID, httpID)
}
