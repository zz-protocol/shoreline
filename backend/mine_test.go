package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedAliveAssets(t *testing.T, e *testEnv, n int, prefix string) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		host := fmt.Sprintf("%s%d.mine.example.test", prefix, i)
		var id int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, alive, customer_hosted, resolved)
			VALUES ($1, $2, 'alive', false, true) RETURNING id`, e.rootID, host).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = ANY($1)`, ids)
	})
	return ids
}

func idsOf(items []map[string]any) []int64 {
	var out []int64
	for _, it := range items {
		out = append(out, int64(it["id"].(float64)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func TestMineClaimConcurrentNoOverlap(t *testing.T) {
	e, c, a := newAPITest(t, 100)
	_ = a
	ids := seedAliveAssets(t, e, 40, "c")
	var mu sync.Mutex
	var gotA, gotB []int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		res := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "agent-a", "limit": 20})
		mu.Lock()
		gotA = idsOf(items(res))
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		// 第二个 client 同 token
		res := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "agent-b", "limit": 20})
		mu.Lock()
		gotB = idsOf(items(res))
		mu.Unlock()
	}()
	wg.Wait()
	if len(gotA) == 0 || len(gotB) == 0 {
		t.Fatalf("both should get some: a=%v b=%v (pool %d)", gotA, gotB, len(ids))
	}
	seen := map[int64]string{}
	for _, id := range gotA {
		seen[id] = "a"
	}
	for _, id := range gotB {
		if who, ok := seen[id]; ok {
			t.Fatalf("asset %d claimed by both a and %s", id, who)
		}
		seen[id] = "b"
	}
	if len(gotA)+len(gotB) > len(ids) {
		t.Fatalf("claimed more than pool")
	}
	t.Logf("并发 claim：A=%d B=%d 无交集", len(gotA), len(gotB))
}

func TestMineHeartbeatTTLCooldownAndFilters(t *testing.T) {
	e, c, a := newAPITest(t, 100)
	ctx := context.Background()
	ids := seedAliveAssets(t, e, 5, "t")
	// 死资产 / 客户托管不可领
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'dead' WHERE id = $1`, ids[3])
	e.pool.Exec(ctx, `UPDATE assets SET customer_hosted = true WHERE id = $1`, ids[4])

	res := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "bot-1", "limit": 10})
	claimed := items(res)
	if len(claimed) != 3 {
		t.Fatalf("claimable want 3 got %d %v", len(claimed), claimed)
	}
	cid := int64(claimed[0]["id"].(float64))
	if claimed[0]["mine_status"] != "claimed" || claimed[0]["mine_claimed_by"] != "bot-1" {
		t.Fatalf("claim fields %v", claimed[0])
	}
	var exp1 time.Time
	e.pool.QueryRow(ctx, `SELECT mine_claim_expires_at FROM assets WHERE id = $1`, cid).Scan(&exp1)

	hb := c.want(200, "POST", "/api/mine/heartbeat", map[string]any{"asset_ids": []int64{cid}})
	if int(hb["renewed"].(float64)) != 1 {
		t.Fatalf("heartbeat %v", hb)
	}
	var exp2 time.Time
	e.pool.QueryRow(ctx, `SELECT mine_claim_expires_at FROM assets WHERE id = $1`, cid).Scan(&exp2)
	if !exp2.After(exp1) {
		t.Fatalf("heartbeat did not extend: %v -> %v", exp1, exp2)
	}

	// TTL 过期后可再领
	e.pool.Exec(ctx, `UPDATE assets SET mine_claim_expires_at = now() - interval '1 second' WHERE id = $1`, cid)
	res2 := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "bot-2", "limit": 10})
	found := false
	for _, it := range items(res2) {
		if int64(it["id"].(float64)) == cid {
			found = true
			if it["mine_claimed_by"] != "bot-2" {
				t.Fatalf("reclaim by %v", it["mine_claimed_by"])
			}
		}
	}
	if !found {
		t.Fatal("expired claim not reclaimable")
	}

	// complete → 冷却期内不可领
	a.cfg.MineCooldownDays = 30
	c.want(200, "POST", "/api/mine/complete", map[string]any{
		"asset_ids": []int64{cid}, "status": "done", "note": "ok", "report_url": "https://report.example.test/1",
	})
	var st string
	var cool *time.Time
	e.pool.QueryRow(ctx, `SELECT mine_status, mine_cooldown_until FROM assets WHERE id = $1`, cid).Scan(&st, &cool)
	if st != "done" || cool == nil || !cool.After(time.Now()) {
		t.Fatalf("after complete status=%s cool=%v", st, cool)
	}
	res3 := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "bot-3", "limit": 10})
	for _, it := range items(res3) {
		if int64(it["id"].(float64)) == cid {
			t.Fatal("in cooldown still claimable")
		}
	}
	// 冷却过期后再领
	e.pool.Exec(ctx, `UPDATE assets SET mine_cooldown_until = now() - interval '1 second' WHERE id = $1`, cid)
	res4 := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "bot-4", "limit": 10})
	found = false
	for _, it := range items(res4) {
		if int64(it["id"].(float64)) == cid {
			found = true
		}
	}
	if !found {
		t.Fatal("after cooldown should be claimable")
	}

	// abandoned 默认不可领；include_abandoned 可领
	idAb := ids[1]
	c.want(200, "POST", "/api/mine/complete", map[string]any{"asset_ids": []int64{idAb}, "status": "abandoned"})
	res5 := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "bot-5", "limit": 10})
	for _, it := range items(res5) {
		if int64(it["id"].(float64)) == idAb {
			t.Fatal("abandoned claimed without flag")
		}
	}
	res6 := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "bot-5", "limit": 10, "include_abandoned": true})
	found = false
	for _, it := range items(res6) {
		if int64(it["id"].(float64)) == idAb {
			found = true
		}
	}
	if !found {
		t.Fatal("include_abandoned should claim abandoned")
	}

	// 列表筛选 mine_status
	list := items(c.want(200, "GET", "/api/assets?mine_status=claimed&include_wildcard=true&include_customer_hosted=true&limit=500", nil))
	for _, it := range list {
		if it["mine_status"] != "claimed" {
			t.Fatalf("filter leaked %v", it["mine_status"])
		}
	}
	c.want(400, "GET", "/api/assets?mine_status=nope", nil)
	c.want(400, "POST", "/api/mine/claim", map[string]any{"limit": 1})
	c.want(400, "POST", "/api/mine/complete", map[string]any{"asset_ids": []int64{cid}, "status": "claimed"})

	// MINE_COOLDOWN_DAYS=0 → 完成后不再入队
	a.cfg.MineCooldownDays = 0
	id0 := ids[2]
	c.want(200, "POST", "/api/mine/complete", map[string]any{"asset_ids": []int64{id0}, "status": "done"})
	var cool0 *time.Time
	e.pool.QueryRow(ctx, `SELECT mine_cooldown_until FROM assets WHERE id = $1`, id0).Scan(&cool0)
	if cool0 != nil {
		t.Fatalf("cooldown 0 should leave null, got %v", cool0)
	}
	res7 := c.want(200, "POST", "/api/mine/claim", map[string]any{"claimed_by": "bot-0", "limit": 10, "include_abandoned": true})
	for _, it := range items(res7) {
		if int64(it["id"].(float64)) == id0 {
			t.Fatal("cooldown 0 must not requeue done")
		}
	}
	t.Logf("heartbeat 续期、TTL 过期可再领、冷却期内不可领、abandoned/冷却0 行为正确")
}

func TestMineMCPMatchesREST(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ids := seedAliveAssets(t, e, 3, "m")
	sess, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, isErr := callTool(t, sess, "claim_mine_targets", map[string]any{"claimed_by": "mcp-bot", "limit": 2})
	if isErr {
		t.Fatalf("claim_mine_targets %v", out)
	}
	its := out["items"].([]any)
	if len(its) != 2 {
		t.Fatalf("mcp claim %v", out)
	}
	aid := int64(its[0].(map[string]any)["id"].(float64))
	out, isErr = callTool(t, sess, "heartbeat_mine", map[string]any{"asset_ids": []any{aid}})
	if isErr || int64(out["renewed"].(float64)) != 1 {
		t.Fatalf("heartbeat_mine %v %v", out, isErr)
	}
	out, isErr = callTool(t, sess, "complete_mine", map[string]any{"asset_ids": []any{aid}, "status": "deferred", "note": "later"})
	if isErr || out["status"] != "deferred" {
		t.Fatalf("complete_mine %v %v", out, isErr)
	}
	_ = ids
	t.Logf("MCP claim/heartbeat/complete 与 REST 一致")
}

func TestMineMigrationOnExistingDB(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	// 模拟旧库：去掉 018 与列，再 migrate
	e.pool.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS mine_report_url`)
	e.pool.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS mine_note`)
	e.pool.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS mine_cooldown_until`)
	e.pool.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS mine_completed_at`)
	e.pool.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS mine_claim_expires_at`)
	e.pool.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS mine_claimed_at`)
	e.pool.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS mine_claimed_by`)
	e.pool.Exec(ctx, `ALTER TABLE assets DROP COLUMN IF EXISTS mine_status`)
	e.pool.Exec(ctx, `DROP INDEX IF EXISTS assets_mine_status_idx`)
	e.pool.Exec(ctx, `DROP INDEX IF EXISTS assets_mine_claim_exp_idx`)
	e.pool.Exec(ctx, `DROP INDEX IF EXISTS assets_mine_cooldown_idx`)
	e.pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = '018_mine.sql'`)

	var aid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`,
		e.rootID, "legacy.mine.example.test").Scan(&aid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, aid) })

	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err := migrate(ctx, conn.Conn()); err != nil {
		t.Fatalf("migrate on old DB: %v", err)
	}
	var st string
	if err := e.pool.QueryRow(ctx, `SELECT mine_status FROM assets WHERE id = $1`, aid).Scan(&st); err != nil {
		t.Fatalf("column missing after migrate: %v", err)
	}
	if st != "unset" {
		t.Fatalf("default mine_status %q", st)
	}
	// 幂等再跑
	if err := migrate(ctx, conn.Conn()); err != nil {
		t.Fatalf("migrate idempotent: %v", err)
	}
	sql, err := os.ReadFile(filepath.Join("migrations", "018_mine.sql"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(sql)
	if len(s) < 100 || !strings.Contains(s, "mine_status") || !strings.Contains(s, "IF NOT EXISTS") {
		t.Fatalf("018_mine.sql unexpected: %s", sql)
	}
	t.Logf("旧库迁移：补齐 mine_* 列，存量资产默认 unset；可重复执行")
}

func TestMineConfigFromEnv(t *testing.T) {
	t.Setenv("MINE_COOLDOWN_DAYS", "0")
	t.Setenv("MINE_CLAIM_TTL_HOURS", "2")
	c, err := apiConfigFromEnv()
	if err != nil || c.MineCooldownDays != 0 || c.MineClaimTTLHours != 2 {
		t.Fatalf("%v %+v", err, c)
	}
	t.Setenv("MINE_CLAIM_TTL_HOURS", "0")
	if _, err := apiConfigFromEnv(); err == nil {
		t.Fatal("TTL 0 should fail")
	}
	t.Setenv("MINE_COOLDOWN_DAYS", "-1")
	t.Setenv("MINE_CLAIM_TTL_HOURS", "24")
	if _, err := apiConfigFromEnv(); err == nil {
		t.Fatal("negative cooldown should fail")
	}
}
