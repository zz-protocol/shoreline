package main

import (
	"encoding/json"

	"context"
	"fmt"
	"github.com/hibiken/asynq"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func aliveOf(t *testing.T, e *testEnv, id int64) (string, bool) {
	t.Helper()
	var s string
	var at *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT alive, last_alive_at FROM assets WHERE id = $1`, id).Scan(&s, &at); err != nil {
		t.Fatal(err)
	}
	return s, at != nil
}

func recoveredCount(t *testing.T, e *testEnv, id int64) int {
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE asset_id = $1 AND type = 'recovered'`, id).Scan(&n)
	return n
}

// 状态机：unknown→dead→alive（1 条 recovered）；alive 不改回；unknown→alive 不发 recovered。
func TestAliveTransitions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	a := newRulesAsset(t, e, "a.alive.test")
	b := newRulesAsset(t, e, "b.alive.test")
	var log []string
	step := func(id int64, host string, resp bool) {
		if err := markAlive(ctx, e.pool, id, host, resp); err != nil {
			t.Fatal(err)
		}
		s, at := aliveOf(t, e, id)
		log = append(log, fmt.Sprintf("%s resp=%v → %s last_alive_at=%v recovered=%d", host, resp, s, at, recoveredCount(t, e, id)))
	}
	if s, _ := aliveOf(t, e, a); s != "unknown" {
		t.Fatalf("default %s", s)
	}
	step(a, "a.alive.test", false)
	step(a, "a.alive.test", false)
	step(a, "a.alive.test", true)
	step(a, "a.alive.test", false)
	step(a, "a.alive.test", true)
	step(b, "b.alive.test", true)
	want := []string{
		"a.alive.test resp=false → dead last_alive_at=false recovered=0",
		"a.alive.test resp=false → dead last_alive_at=false recovered=0",
		"a.alive.test resp=true → alive last_alive_at=true recovered=1",
		"a.alive.test resp=false → alive last_alive_at=true recovered=1",
		"a.alive.test resp=true → alive last_alive_at=true recovered=1",
		"b.alive.test resp=true → alive last_alive_at=true recovered=0",
	}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Fatalf("\n got %v\nwant %v", log, want)
	}
	for _, l := range log {
		t.Log(l)
	}
}

// 并发：同一 dead 资产 32 个 goroutine 同时判定存活，只产生 1 条 recovered。
func TestAliveRecoveredOnceConcurrent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	id := newRulesAsset(t, e, "c.alive.test")
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'dead' WHERE id = $1`, id)
	var wg sync.WaitGroup
	var errs atomic.Int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := markAlive(ctx, e.pool, id, "c.alive.test", true); err != nil {
				errs.Add(1)
			}
		}()
	}
	wg.Wait()
	if errs.Load() != 0 || recoveredCount(t, e, id) != 1 {
		t.Fatalf("errs=%d recovered=%d", errs.Load(), recoveredCount(t, e, id))
	}
	t.Logf("32 并发 dead→alive：recovered=%d", recoveredCount(t, e, id))
}

// DNS 层：解析失败 → dead（已 alive 不改回）；失联后再解析不发 recovered。
func TestAliveDNSLayer(t *testing.T) {
	e := newTestEnv(t)
	r := testResolver("127.0.0.1:1")
	ctx := context.Background()
	d := newRulesAsset(t, e, "d.alive.test")
	dnsRound(t, e, r, "d.alive.test", hostDNS{})
	if s, _ := aliveOf(t, e, d); s != "dead" {
		t.Fatalf("nx → %s", s)
	}
	al := newRulesAsset(t, e, "al.alive.test")
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'alive', last_alive_at = now() WHERE id = $1`, al)
	dnsRound(t, e, r, "al.alive.test", ok("192.0.2.1"))
	dnsRound(t, e, r, "al.alive.test", hostDNS{})
	dnsRound(t, e, r, "al.alive.test", ok("192.0.2.2"))
	if s, _ := aliveOf(t, e, al); s != "alive" || recoveredCount(t, e, al) != 0 {
		t.Fatalf("alive asset → %s recovered=%d", s, recoveredCount(t, e, al))
	}
	t.Log("NXDOMAIN → dead；alive 资产失联不改回；失联后再解析 DNS 层 recovered=0")
}

// HTTP 阶段：有响应 → alive；无响应 → dead；复核确认泛解析 → unknown（原 dead 也回到 unknown，不进复查）。
// dead 主机恢复响应 → alive + 1 条 recovered，再跑一次不重复。
func TestAliveHTTPStage(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	clean := func() {
		c := context.Background()
		e.pool.Exec(c, `DELETE FROM assets WHERE subdomain LIKE '%.wild.test' OR subdomain LIKE '%.plain.test'`)
		e.pool.Exec(c, `DELETE FROM root_domains WHERE domain IN ('wild.test', 'plain.test')`)
	}
	clean()
	t.Cleanup(clean)
	var wildRoot, plainRoot int64
	e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'wild.test') RETURNING id`, vendorID).Scan(&wildRoot)
	e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'plain.test') RETURNING id`, vendorID).Scan(&plainRoot)
	for _, h := range []string{"lb.wild.test", "fakea.wild.test", "fakeb.wild.test"} {
		e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, ips, resolved, wildcard) VALUES ($1, $2, '{192.0.2.66}', true, true)`, wildRoot, h)
	}
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'dead' WHERE subdomain = 'fakeb.wild.test'`)
	for _, h := range []string{"www.plain.test", "down.plain.test"} {
		e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, ips, resolved) VALUES ($1, $2, '{192.0.2.1}', true)`, plainRoot, h)
	}
	var hits atomic.Int64
	cfg := localProbeServers(t, &hits)
	var downUp atomic.Bool
	dial := cfg.Dial
	cfg.Dial = func(addr string) string {
		if h, _, _ := net.SplitHostPort(addr); h == "down.plain.test" && !downUp.Load() {
			return "127.0.0.1:1" // 无响应
		}
		return dial(addr)
	}
	h := httpHandler(e.pool, newProber(cfg))
	hosts := []string{"lb.wild.test", "fakea.wild.test", "fakeb.wild.test", "www.plain.test", "down.plain.test"}
	state := func() string {
		rows, _ := e.pool.Query(ctx, `SELECT a.subdomain, a.alive, (SELECT count(*) FROM events v WHERE v.asset_id = a.id AND v.type = 'recovered')
			FROM assets a WHERE a.root_id IN ($1, $2) ORDER BY 1`, wildRoot, plainRoot)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s, al string
			var n int
			rows.Scan(&s, &al, &n)
			out = append(out, fmt.Sprintf("%s=%s/r%d", s, al, n))
		}
		return strings.Join(out, " ")
	}
	if _, err := h(ctx, 0, hosts); err != nil {
		t.Fatal(err)
	}
	got1 := state()
	want1 := "down.plain.test=dead/r0 fakea.wild.test=unknown/r0 fakeb.wild.test=unknown/r0 lb.wild.test=alive/r0 www.plain.test=alive/r0"
	if got1 != want1 {
		t.Fatalf("round1\n got %s\nwant %s", got1, want1)
	}
	downUp.Store(true)
	h(ctx, 0, []string{"down.plain.test"})
	h(ctx, 0, []string{"down.plain.test", "www.plain.test"})
	got2 := state()
	want2 := "down.plain.test=alive/r1 fakea.wild.test=unknown/r0 fakeb.wild.test=unknown/r0 lb.wild.test=alive/r0 www.plain.test=alive/r0"
	if got2 != want2 {
		t.Fatalf("round2\n got %s\nwant %s", got2, want2)
	}
	t.Logf("第 1 轮：%s", got1)
	t.Logf("恢复后两轮：%s", got2)
}

// SCHEDULE_ALIVE：默认 24h，0/off 关闭，非法报错。
func TestScheduleAliveConfig(t *testing.T) {
	t.Setenv("SCHEDULE_ALIVE", "")
	c, err := scheduleConfigFromEnv()
	if err != nil || c.Alive != 24*time.Hour {
		t.Fatalf("%v %+v", err, c)
	}
	for _, v := range []string{"0", "off"} {
		t.Setenv("SCHEDULE_ALIVE", v)
		if c, err := scheduleConfigFromEnv(); err != nil || c.Alive != 0 {
			t.Fatalf("%s: %v %+v", v, err, c)
		}
	}
	t.Setenv("SCHEDULE_ALIVE", "2h")
	if c, _ := scheduleConfigFromEnv(); c.Alive != 2*time.Hour {
		t.Fatal(c)
	}
	for _, v := range []string{"nope", "-1h"} {
		t.Setenv("SCHEDULE_ALIVE", v)
		if _, err := scheduleConfigFromEnv(); err == nil {
			t.Fatalf("%s: want error", v)
		}
	}
}

// 复查只针对 dead（非泛解析）：交给 dns 任务；已有 pending dns 任务跳过。
func TestSchedulerTickAlive(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.pool.Exec(ctx, `DELETE FROM scan_tasks WHERE kind = 'dns'`)
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'unknown' WHERE alive = 'dead'`)
	mk := func(h, alive string, wild bool) {
		id := newRulesAsset(t, e, h)
		e.pool.Exec(ctx, `UPDATE assets SET alive = $2, wildcard = $3 WHERE id = $1`, id, alive, wild)
	}
	mk("d1.tick.alive.test", "dead", false)
	mk("d2.tick.alive.test", "dead", false)
	mk("ok.tick.alive.test", "alive", false)
	mk("u.tick.alive.test", "unknown", false)
	mk("w.tick.alive.test", "dead", true)
	s := newScheduler(e.pool, e.client, scheduleConfig{Alive: time.Hour}, 1)
	n, err := s.tick(ctx, "alive")
	if err != nil || n != 1 {
		t.Fatalf("tick n=%d err=%v", n, err)
	}
	var kind string
	var id int64
	e.pool.QueryRow(ctx, `SELECT id, kind FROM scan_tasks WHERE root_id = $1 AND kind = 'dns' AND status = 'pending'`, e.rootID).Scan(&id, &kind)
	hosts := pendingHosts(t, e, id)
	if fmt.Sprint(hosts) != "[d1.tick.alive.test d2.tick.alive.test]" {
		t.Fatalf("hosts %v", hosts)
	}
	if n, _ := s.tick(ctx, "alive"); n != 0 {
		t.Fatalf("busy skip n=%d", n)
	}
	t.Logf("复查任务 dns#%d hosts=%v；已有 pending 再 tick 跳过", id, hosts)
}

// 迁移回填：解析成功且有 URL → alive，其余 unknown；不产生事件。
func TestAliveBackfillNoEvents(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	b, err := os.ReadFile("migrations/013_alive.sql")
	if err != nil {
		t.Fatal(err)
	}
	var upd string
	for _, stmt := range strings.Split(string(b), ";") {
		if strings.Contains(stmt, "UPDATE assets") {
			upd = stmt
		}
	}
	withURL := newRulesAsset(t, e, "u.bf.alive.test")
	noURL := newRulesAsset(t, e, "n.bf.alive.test")
	unres := newRulesAsset(t, e, "x.bf.alive.test")
	e.pool.Exec(ctx, `UPDATE assets SET resolved = true WHERE id IN ($1, $2)`, withURL, noURL)
	e.pool.Exec(ctx, `UPDATE assets SET resolved = false WHERE id = $1`, unres)
	for _, id := range []int64{withURL, unres} {
		e.pool.Exec(ctx, `INSERT INTO urls (asset_id, scheme, host, port) SELECT id, 'https', subdomain, 443 FROM assets WHERE id = $1`, id)
	}
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'unknown', last_alive_at = NULL WHERE id IN ($1, $2, $3)`, withURL, noURL, unres)
	var before, after int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&before)
	if _, err := e.pool.Exec(ctx, upd); err != nil {
		t.Fatal(err)
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&after)
	s1, at := aliveOf(t, e, withURL)
	s2, _ := aliveOf(t, e, noURL)
	s3, _ := aliveOf(t, e, unres)
	if s1 != "alive" || !at || s2 != "unknown" || s3 != "unknown" || before != after {
		t.Fatalf("%s/%v %s %s events %d→%d", s1, at, s2, s3, before, after)
	}
	t.Logf("回填：有 URL=%s，无 URL=%s，未解析=%s；events %d→%d", s1, s2, s3, before, after)
}

// API / MCP：alive 字段与筛选，非法值两边同错。
func TestAliveAPIFilter(t *testing.T) {
	e, c, a := newAPITest(t, 100)
	ctx := context.Background()
	for i, st := range []string{"alive", "dead", "unknown"} {
		id := newRulesAsset(t, e, fmt.Sprintf("%s%d.api.alive.test", st, i))
		e.pool.Exec(ctx, `UPDATE assets SET alive = $2, last_alive_at = CASE WHEN $2 = 'alive' THEN now() END WHERE id = $1`, id, st)
	}
	v := e.vendorID()
	for _, st := range []string{"alive", "dead", "unknown"} {
		list := items(c.want(200, "GET", fmt.Sprintf("/api/assets?vendor=%d&q=api.alive.test&alive=%s", v, st), nil))
		if len(list) != 1 || list[0]["alive"] != st || (st == "alive") != (list[0]["last_alive_at"] != nil) {
			t.Fatalf("%s: %v", st, list)
		}
	}
	c.want(400, "GET", "/api/assets?alive=yes", nil)
	_, err := a.queryAssets(ctx, assetFilter{Limit: 50, Alive: "yes"})
	if err == nil || err.Error() != "alive must be alive|dead|unknown" {
		t.Fatalf("mcp err %v", err)
	}
	// 播报：type=new,recovered 能看到 dead→alive 产生的 recovered
	rid := newRulesAsset(t, e, "rec.api.alive.test")
	e.pool.Exec(ctx, `UPDATE assets SET alive = 'dead' WHERE id = $1`, rid)
	if err := markAlive(ctx, e.pool, rid, "rec.api.alive.test", true); err != nil {
		t.Fatal(err)
	}
	ev := items(c.want(200, "GET", fmt.Sprintf("/api/events?type=new,recovered&asset_id=%d", rid), nil))
	if len(ev) != 1 || ev[0]["type"] != "recovered" {
		t.Fatalf("broadcast %v", ev)
	}
	st := c.want(200, "GET", fmt.Sprintf("/api/events/stats?type=recovered&vendor=%d", v), nil)
	if st["total"].(float64) < 1 {
		t.Fatalf("stats %v", st)
	}
	c.want(400, "GET", "/api/events?type=new,bogus", nil)
	t.Log("alive=alive|dead|unknown 各 1 条；alive=yes REST 400 / MCP 同错；播报 type=new,recovered 含 recovered；type=new,bogus 400")
}

// pendingHosts 从队列（miniredis）读出该扫描任务的 hosts。
func pendingHosts(t *testing.T, e *testEnv, scanID int64) []string {
	t.Helper()
	ins := asynq.NewInspector(e.redis)
	defer ins.Close()
	tasks, err := ins.ListPendingTasks("dns")
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		var p scanPayload
		json.Unmarshal(tk.Payload, &p)
		if p.ScanID == scanID {
			return p.Hosts
		}
	}
	t.Fatalf("scan %d not queued", scanID)
	return nil
}
