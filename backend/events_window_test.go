package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
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
	r2 := c.want(201, "POST", fmt.Sprintf("/api/vendors/%d/roots", v2), map[string]any{"domain": "t20b.example.test"})
	root2 := int64(r2["id"].(float64))
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
		ins(root2, fmt.Sprintf("b%d.t20b.example.test", i), at(-time.Duration(i)*time.Hour), true)
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

// 事件不自动清理：仓库内除迁移 008 一次性去重、删除厂商/根域级联外，无删除 events 的代码。
func TestNoEventCleanupCode(t *testing.T) {
	for _, f := range []string{"api.go", "events.go", "queue.go", "schedule.go", "dns.go", "httpprobe.go", "passive.go", "active.go", "expand.go", "certstream.go", "main.go"} {
		b, err := readSource(f)
		if err != nil {
			t.Fatal(err)
		}
		if containsFold(b, "delete from events") || containsFold(b, "truncate events") {
			t.Fatalf("%s deletes events", f)
		}
	}
}

func readSource(f string) (string, error) {
	b, err := os.ReadFile(f)
	return string(b), err
}

func containsFold(s, sub string) bool { return strings.Contains(strings.ToLower(s), sub) }
