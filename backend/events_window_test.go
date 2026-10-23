package main

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// T20：时间窗 since/until、after_id 轮询、各厂商统计、整列复制均基于全量记录（与分页无关）。
func TestEventsWindowStatsValues(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	v1 := e.vendorID()
	o := c.want(201, "POST", "/api/vendors", map[string]any{"name": fmt.Sprintf("%s-b-%d", t.Name(), time.Now().UnixNano())})
	v2 := int64(o["id"].(float64))
	r2 := c.want(201, "POST", fmt.Sprintf("/api/vendors/%d/roots", v2), map[string]any{"domain": fmt.Sprintf("t20b%d.example.test", time.Now().UnixNano())})
	root2 := int64(r2["id"].(float64))
	dom2 := r2["domain"].(string)
	var root1 string
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root1)

	now := time.Now().UTC().Truncate(time.Second)
	at := func(d time.Duration) time.Time { return now.Add(d) }
	ins := func(rootID int64, host string, created time.Time, withURL bool) {
		t.Helper()
		var aid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, rootID, host).Scan(&aid); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, detail, created_at) VALUES ('new', $1, '{}', $2)`, aid, created); err != nil {
			t.Fatal(err)
		}
		if withURL {
			var uid int64
			if err := e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port) VALUES ($1, 'https', $2, 443) RETURNING id`, aid, host).Scan(&uid); err != nil {
				t.Fatal(err)
			}
			if _, err := e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, url_id, detail, created_at) VALUES ('new', $1, $2, '{}', $3)`, aid, uid, created); err != nil {
				t.Fatal(err)
			}
		}
	}
	// v1：窗口内 60 个（超过一页 50），窗口外（40 天前）1 个；v2：窗口内 3 个
	for i := 0; i < 60; i++ {
		ins(e.rootID, fmt.Sprintf("w%02d.%s", i, root1), at(-time.Duration(i)*time.Hour), i%2 == 0)
	}
	ins(e.rootID, "old."+root1, at(-40*24*time.Hour), true)
	for i := 0; i < 3; i++ {
		ins(root2, fmt.Sprintf("b%d.%s", i, dom2), at(-time.Duration(i)*time.Hour), true)
	}

	since := url.QueryEscape(at(-7 * 24 * time.Hour).Format(time.RFC3339))
	until := url.QueryEscape(at(time.Minute).Format(time.RFC3339))
	win := fmt.Sprintf("type=new&since=%s&until=%s", since, until)

	// 列表：分页拿全 63 条，不含 40 天前的（库里可能有其它测试的事件：按本测试两个厂商分别取）
	n := 0
	for _, vid := range []int64{v1, v2} {
		cursor := ""
		for {
			p := fmt.Sprintf("/api/events?limit=50&vendor=%d&%s", vid, win)
			if cursor != "" {
				p += "&cursor=" + cursor
			}
			out := c.want(200, "GET", p, nil)
			n += len(items(out))
			if out["next_cursor"] == nil {
				break
			}
			cursor = out["next_cursor"].(string)
		}
	}
	if n != 63 {
		t.Fatalf("window list %d want 63", n)
	}
	// until 截止：只看 10 小时前以前
	u10 := url.QueryEscape(at(-10*time.Hour + time.Second).Format(time.RFC3339))
	old := items(c.want(200, "GET", fmt.Sprintf("/api/events?limit=500&vendor=%d&type=new&since=%s&until=%s", v1, since, u10), nil))
	if len(old) != 50 { // w10..w59
		t.Fatalf("until window %d want 50", len(old))
	}
	c.want(400, "GET", "/api/events?since="+until+"&until="+since, nil)
	c.want(400, "GET", "/api/events?until=nope", nil)
	c.want(400, "GET", "/api/events?after_id=-1", nil)

	// 统计：全量，与分页无关
	st := c.want(200, "GET", "/api/events/stats?"+win, nil)
	got := map[int64]int64{}
	for _, it := range items(st) {
		got[int64(it["vendor_id"].(float64))] = int64(it["count"].(float64))
	}
	var sum int64
	for _, c := range got {
		sum += c
	}
	if got[v1] != 60 || got[v2] != 3 || int64(st["total"].(float64)) != sum {
		t.Fatalf("stats %v total %v", got, st["total"])
	}
	one := c.want(200, "GET", fmt.Sprintf("/api/events/stats?vendor=%d&%s", v2, win), nil)
	if len(items(one)) != 1 || one["total"].(float64) != 3 {
		t.Fatalf("stats vendor %v", one)
	}

	// 整列复制：子域 63 条去重；URL（level=asset）取同窗口 URL 级事件 30+3
	subs := c.want(200, "GET", fmt.Sprintf("/api/events/values?field=subdomain&vendor=%d&%s", v1, win), nil)
	if subs["count"].(float64) != 60 || subs["truncated"] != false {
		t.Fatalf("values subdomain %v", subs["count"])
	}
	list := subs["items"].([]any)
	strs := make([]string, len(list))
	for i, x := range list {
		strs[i] = x.(string)
	}
	if !sort.StringsAreSorted(strs) {
		t.Fatal("values not sorted")
	}
	urls := c.want(200, "GET", fmt.Sprintf("/api/events/values?field=url&vendor=%d&%s", v1, win), nil)
	if urls["count"].(float64) != 30 {
		t.Fatalf("values url %v", urls["count"])
	}
	urlsL := c.want(200, "GET", fmt.Sprintf("/api/events/values?field=url&level=url&vendor=%d&%s", v2, win), nil)
	if urlsL["count"].(float64) != 3 {
		t.Fatalf("values url level=url %v", urlsL["count"])
	}
	c.want(400, "GET", "/api/events/values?field=ip", nil)

	// after_id：只取更新的
	vw := fmt.Sprintf("vendor=%d&%s", v1, win)
	top := items(c.want(200, "GET", "/api/events?limit=1&"+vw, nil))[0]
	maxID := int64(top["id"].(float64))
	if l := items(c.want(200, "GET", fmt.Sprintf("/api/events?after_id=%d&%s", maxID, vw), nil)); len(l) != 0 {
		t.Fatalf("after_id returned %d", len(l))
	}
	ins(e.rootID, "fresh."+root1, at(30*time.Second), false)
	l := items(c.want(200, "GET", fmt.Sprintf("/api/events?after_id=%d&%s", maxID, vw), nil))
	if len(l) != 1 || l[0]["subdomain"] != "fresh."+root1 || int64(l[0]["vendor_id"].(float64)) != v1 {
		t.Fatalf("after_id new %v", l)
	}
	t.Logf("窗口 63 条（分页取全）；stats v1=%d v2=%d total=%v（含库内其它）；values(v1) 子域 %v、URL %v；after_id 只取新增 1 条", got[v1], got[v2], st["total"], subs["count"], urls["count"])
}

// 事件不自动清理：扫描 backend 下全部非测试 .go 文件，无删除 events 的代码（迁移 008 一次性去重、厂商/根域级联不在 .go 里）。
func TestNoEventCleanupCode(t *testing.T) {
	n := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, "_test.go") || !(strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sql")) {
			return nil
		}
		n++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bad := eventDeletes(filepath.ToSlash(path), string(b)); len(bad) > 0 {
			t.Errorf("%s deletes events: %q", path, bad)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Fatalf("only %d source files scanned", n)
	}
	t.Logf("递归扫描 %d 个非测试 .go/.sql 文件，除白名单外无删除/清空 events 的语句", n)
}

// eventDeleteRe：DELETE FROM / TRUNCATE [TABLE] [ONLY] [schema.]["]events["]，大小写不敏感，任意空白/换行。
var eventDeleteRe = regexp.MustCompile(`(?is)\b(delete\s+from|truncate(\s+table)?)\s+(only\s+)?("?[a-z_][a-z0-9_]*"?\s*\.\s*)?"?events"?([^a-z0-9_]|$)`)

// eventDeleteAllow：白名单，精确到 文件 + 归一化后的整条语句（到分号）。
var eventDeleteAllow = map[string]string{
	"migrations/008_event_dedup.sql": "delete from events e using events o where e.type = 'new' and o.type = 'new' and e.id > o.id and e.asset_id is not distinct from o.asset_id and e.url_id is not distinct from o.url_id",
}

func normSQL(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// eventDeletes 返回 src 中未列入白名单的删除/清空 events 语句（归一化后）。
func eventDeletes(path, src string) []string {
	var out []string
	for _, m := range eventDeleteRe.FindAllStringIndex(src, -1) {
		stmt := src[m[0]:]
		if i := strings.IndexByte(stmt, ';'); i >= 0 {
			stmt = stmt[:i]
		}
		if st := normSQL(stmt); eventDeleteAllow[path] != st {
			out = append(out, st)
		}
	}
	return out
}

func TestEventDeletesPatterns(t *testing.T) {
	allowed := "DELETE FROM events e USING events o\n WHERE e.type = 'new' AND o.type = 'new' AND e.id > o.id\n   AND e.asset_id IS NOT DISTINCT FROM o.asset_id AND e.url_id IS NOT DISTINCT FROM o.url_id;"
	for _, c := range []struct {
		path, src string
		bad       bool
	}{
		{"x.go", "DELETE  FROM events WHERE id = 1", true},
		{"x.go", "DELETE FROM public.events", true},
		{"x.go", `DELETE FROM "events"`, true},
		{"x.go", `DELETE FROM public."events" WHERE true`, true},
		{"x.go", "DELETE\nFROM events", true},
		{"sub/zz.go", "x := `delete from events`", true},
		{"x.go", "TRUNCATE events", true},
		{"x.go", "truncate table ONLY events;", true},
		{"x.sql", "delete from events where created_at < now()", true},
		{"migrations/008_event_dedup.sql", allowed, false},
		{"migrations/008_event_dedup.sql", allowed + "\nDELETE FROM events WHERE type = 'changed';", true},
		{"migrations/009_x.sql", allowed, true}, // 白名单精确到文件
		{"x.go", "DELETE FROM events_archive; SELECT * FROM events", false},
		{"x.go", "DELETE FROM scope_rules WHERE id = $1", false},
	} {
		if got := eventDeletes(c.path, c.src); (len(got) > 0) != c.bad {
			t.Errorf("%s %q: got %q want bad=%v", c.path, c.src, got, c.bad)
		}
	}
}

// 恢复的资产：窗口内只有资产级 recovered 事件（URL 早已存在、无 URL 级事件），资产维度复制 URL 应返回其当前 URL。
func TestEventValuesRecoveredAssetURL(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	o := c.want(201, "POST", "/api/vendors", map[string]any{"name": fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())})
	vid := int64(o["id"].(float64))
	r := c.want(201, "POST", fmt.Sprintf("/api/vendors/%d/roots", vid), map[string]any{"domain": fmt.Sprintf("t25r%d.example.test", time.Now().UnixNano())})
	host := "rec." + r["domain"].(string)
	var aid, uid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, int64(r["id"].(float64)), host).Scan(&aid); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port) VALUES ($1, 'https', $2, 443) RETURNING id`, aid, host).Scan(&uid)
	e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, url_id, detail, created_at) VALUES ('new', $1, $2, '{}', $3)`, aid, uid, old)
	e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, detail, created_at) VALUES ('new', $1, '{}', $2)`, aid, old)
	if _, err := e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, detail) VALUES ('recovered', $1, '{}')`, aid); err != nil {
		t.Fatal(err)
	}
	since := url.QueryEscape(time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339))
	for _, typ := range []string{"recovered", "new,recovered"} {
		out := c.want(200, "GET", fmt.Sprintf("/api/events/values?field=url&level=asset&vendor=%d&type=%s&since=%s", vid, typ, since), nil)
		got := out["items"].([]any)
		if len(got) != 1 || got[0] != "https://"+host+":443" {
			t.Fatalf("type=%s values url %v", typ, got)
		}
	}
	_ = e
}

// 默认口径：不带 type = new,recovered；显式含 changed 才计 changed（events / stats / values 一致）。
func TestEventsDefaultTypes(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	vid := e.vendorID()
	var root string
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	for i, typ := range []string{"new", "new", "recovered", "changed", "changed", "changed"} {
		var aid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2) RETURNING id`, e.rootID, fmt.Sprintf("d%d.%s", i, root)).Scan(&aid); err != nil {
			t.Fatal(err)
		}
		detail := "{}"
		if typ == "changed" { // 有实质变更键才可见；空 changes 由 TestHideEmptyChanged 覆盖
			detail = `{"changes":{"cnames":{"old":[],"new":["x.cdn.test"]}}}`
		}
		e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, detail) VALUES ($1, $2, $3::jsonb)`, typ, aid, detail)
	}
	cnt := func(q string) (int, float64) {
		ev := len(items(c.want(200, "GET", fmt.Sprintf("/api/events?limit=500&vendor=%d%s", vid, q), nil)))
		st := c.want(200, "GET", fmt.Sprintf("/api/events/stats?vendor=%d%s", vid, q), nil)
		vals := c.want(200, "GET", fmt.Sprintf("/api/events/values?field=subdomain&vendor=%d%s", vid, q), nil)
		if int(vals["count"].(float64)) != ev {
			t.Fatalf("%s values %v events %d", q, vals["count"], ev)
		}
		// 各类之和 == count（总计与各厂商）
		sum := func(m map[string]any) float64 {
			return m["new"].(float64) + m["recovered"].(float64) + m["changed"].(float64)
		}
		tt := st["totals"].(map[string]any)
		if sum(tt) != st["total"].(float64) {
			t.Fatalf("%s totals %v != total %v", q, tt, st["total"])
		}
		for _, it := range st["items"].([]any) {
			if m := it.(map[string]any); sum(m) != m["count"].(float64) {
				t.Fatalf("%s vendor %v sum != count", q, m)
			}
		}
		if q == "&type=changed" && (tt["changed"].(float64) != 3 || tt["new"].(float64) != 0) {
			t.Fatalf("changed-only totals %v", tt)
		}
		return ev, st["total"].(float64)
	}
	if ev, tot := cnt(""); ev != 3 || tot != 3 {
		t.Fatalf("default events %d stats %v want 3 (new+recovered)", ev, tot)
	}
	if ev, tot := cnt("&type=new,recovered,changed"); ev != 6 || tot != 6 {
		t.Fatalf("with changed events %d stats %v want 6", ev, tot)
	}
	if ev, tot := cnt("&type=changed"); ev != 3 || tot != 3 {
		t.Fatalf("changed only %d %v", ev, tot)
	}
}

// T28 补丁：空 changes（或仅剩非实质键 ips）的 changed 行保留在库，但列表/stats/values/MCP 共用路径一律不返回、不计入。
func TestHideEmptyChanged(t *testing.T) {
	e, c, a := newAPITest(t, 100)
	ctx := context.Background()
	v := e.vendorID()
	var root string
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	ins := func(host, detail string) int64 {
		t.Helper()
		var id, eid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, resolved) VALUES ($1, $2, true) RETURNING id`, e.rootID, host).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := e.pool.QueryRow(ctx, `INSERT INTO events (type, asset_id, detail) VALUES ('changed', $1, $2::jsonb) RETURNING id`, id, detail).Scan(&eid); err != nil {
			t.Fatal(err)
		}
		return eid
	}
	emptyID := ins("empty."+root, `{"subdomain":"x","changes":{}}`)
	ipsOnly := ins("ips."+root, `{"subdomain":"x","changes":{"ips":{"old":["192.0.2.1"],"new":["192.0.2.2"]}}}`)
	nullCh := ins("nullch."+root, `{"subdomain":"x"}`) // 无 changes 键
	keepHost := "keep." + root
	keepID := ins(keepHost, `{"subdomain":"x","changes":{"cnames":{"old":[],"new":["cdn.example.test"]}}}`)
	var newAID int64
	e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, resolved) VALUES ($1, $2, true) RETURNING id`, e.rootID, "new."+root).Scan(&newAID)
	e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, detail) VALUES ('new', $1, '{}')`, newAID)

	// 库里 4 条 changed 都在
	var nDB int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'changed' AND asset_id IN (
		SELECT id FROM assets WHERE subdomain LIKE ANY(ARRAY['empty.%','ips.%','nullch.%','keep.%']) AND root_id = $1)`, e.rootID).Scan(&nDB)
	if nDB != 4 {
		t.Fatalf("db changed rows %d want 4", nDB)
	}

	base := fmt.Sprintf("vendor=%d&type=changed&level=asset", v)
	list := items(c.want(200, "GET", "/api/events?limit=500&"+base, nil))
	if len(list) != 1 || list[0]["id"].(float64) != float64(keepID) {
		t.Fatalf("list got %#v want only keep id=%d", list, keepID)
	}
	st := c.want(200, "GET", "/api/events/stats?"+base, nil)
	if int64(st["total"].(float64)) != 1 || int64(st["totals"].(map[string]any)["changed"].(float64)) != 1 {
		t.Fatalf("stats %#v", st)
	}
	vals := c.want(200, "GET", "/api/events/values?field=subdomain&"+base, nil)
	got := vals["items"].([]any)
	if len(got) != 1 || got[0].(string) != keepHost {
		t.Fatalf("values %#v want [%s]", vals, keepHost)
	}

	// 共用 queryEvents（MCP list_new_assets 同源）：显式 type=changed 也看不到空行
	qlist, err := a.queryEvents(ctx, eventFilter{Level: "asset", Type: "changed", Vendor: &v, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(qlist) != 1 || qlist[0]["id"].(int64) != keepID {
		t.Fatalf("queryEvents %#v", qlist)
	}
	// MCP list_new_assets 只含 new，不含空 changed
	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callTool(t, s, "list_new_assets", map[string]any{"vendor": v, "limit": 500})
	if isErr {
		t.Fatalf("mcp %v", out)
	}
	for _, it := range items(out) {
		id := int64(it["id"].(float64))
		if id == emptyID || id == ipsOnly || id == nullCh || id == keepID {
			t.Fatalf("list_new_assets leaked changed id=%d", id)
		}
	}
	t.Logf("empty/ips-only/null-changes hidden; cnames changed id=%d visible; db kept %d changed rows", keepID, nDB)
}

// T26：q 对子域名/URL 包含匹配（不区分大小写）；列表、stats、values、MCP 一致；可与 vendor/type 叠加。
func TestEventsSearchQ(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	v := e.vendorID()
	var root string
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	mk := func(host string, withURL bool) {
		t.Helper()
		var aid int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, resolved) VALUES ($1, $2, true) RETURNING id`, e.rootID, host).Scan(&aid); err != nil {
			t.Fatal(err)
		}
		e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, detail) VALUES ('new', $1, '{}')`, aid)
		if withURL {
			var uid int64
			e.pool.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port) VALUES ($1, 'https', $2, 443) RETURNING id`, aid, host).Scan(&uid)
			e.pool.Exec(ctx, `INSERT INTO events (type, asset_id, url_id, detail) VALUES ('new', $1, $2, '{}')`, aid, uid)
		}
	}
	mk("api."+root, true)
	mk("www."+root, true)
	mk("mail."+root, false)
	base := fmt.Sprintf("vendor=%d&type=new&level=asset", v)

	list := items(c.want(200, "GET", "/api/events?limit=500&"+base+"&q=API.", nil))
	if len(list) != 1 || !strings.HasPrefix(list[0]["subdomain"].(string), "api.") {
		t.Fatalf("subdomain q %#v", list)
	}
	st := c.want(200, "GET", "/api/events/stats?"+base+"&q=API.", nil)
	if int64(st["total"].(float64)) != 1 {
		t.Fatalf("stats %#v", st)
	}
	vals := c.want(200, "GET", "/api/events/values?field=subdomain&"+base+"&q=API.", nil)
	if int(vals["count"].(float64)) != 1 || !strings.HasPrefix(vals["items"].([]any)[0].(string), "api.") {
		t.Fatalf("values %#v", vals)
	}
	// URL 匹配（资产级：经 EXISTS 当前 URL）
	byURL := items(c.want(200, "GET", fmt.Sprintf("/api/events?limit=500&%s&q=%s", base, url.QueryEscape("https://www."+root+":443")), nil))
	if len(byURL) != 1 || !strings.HasPrefix(byURL[0]["subdomain"].(string), "www.") {
		t.Fatalf("url q %#v", byURL)
	}
	// 与 type 叠加：只搜 changed 时搜不到 new
	onlyCh := items(c.want(200, "GET", fmt.Sprintf("/api/events?limit=500&vendor=%d&type=changed&level=asset&q=api.", v), nil))
	if len(onlyCh) != 0 {
		t.Fatalf("type+q %#v", onlyCh)
	}
	c.want(400, "GET", "/api/events?q=%00", nil)

	s, err := mcpSession(t, c.srv.URL, testBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callTool(t, s, "list_new_assets", map[string]any{"vendor": v, "q": "API.", "limit": 500})
	if isErr {
		t.Fatalf("mcp %v", out)
	}
	got := items(out)
	if len(got) != 1 || !strings.HasPrefix(got[0]["subdomain"].(string), "api.") {
		t.Fatalf("mcp %#v", got)
	}
	t.Logf("q=API. → list/stats/values/mcp 均为 1 条 api.*；URL 搜索命中 www；type=changed 叠加为空")
}
