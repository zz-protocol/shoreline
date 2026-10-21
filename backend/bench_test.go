package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBenchListAssets：插入 10 万模拟子域，测 GET /api/assets 列表耗时（须 < 1s），结果写入 docs/bench.md。
// 默认跳过；跑：TEST_BENCH=1 TEST_DATABASE_URL=... go test -count=1 -run TestBenchListAssets -timeout 30m .
func TestBenchListAssets(t *testing.T) {
	if os.Getenv("TEST_BENCH") == "" {
		t.Skip("set TEST_BENCH=1 to run 100k bench")
	}
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	v := e.vendorID()
	root := e.rootID
	const n = 100_000
	prefix := fmt.Sprintf("b16-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE $1`, prefix+"%")
	})

	seedStart := time.Now()
	const batch = 2000
	for i := 0; i < n; i += batch {
		end := i + batch
		if end > n {
			end = n
		}
		_, err := e.pool.Exec(ctx, `
			INSERT INTO assets (root_id, subdomain, ips, in_scope, sources, wildcard)
			SELECT $1,
				$2 || i::text || '.bench.example.test',
				ARRAY['10.0.0.' || ((i % 250)+1)::text],
				(i % 2 = 0),
				ARRAY['bench'],
				(i % 17 = 0)
			FROM generate_series($3::int, $4::int) AS i
			ON CONFLICT (subdomain) DO NOTHING`, root, prefix, i, end-1)
		if err != nil {
			t.Fatal(err)
		}
	}
	seedDur := time.Since(seedStart)
	var got int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE subdomain LIKE $1`, prefix+"%").Scan(&got)
	if got < n {
		t.Fatalf("seeded %d want %d", got, n)
	}
	t.Logf("seeded %d assets in %s", got, seedDur)

	type run struct {
		Name string
		Path string
	}
	runs := []run{
		{"list default limit=50", "/api/assets?limit=50"},
		{"list vendor filter", fmt.Sprintf("/api/assets?vendor=%d&limit=50", v)},
		{"list include_wildcard", fmt.Sprintf("/api/assets?vendor=%d&include_wildcard=true&limit=50", v)},
		{"list q substring", fmt.Sprintf("/api/assets?vendor=%d&q=%s&limit=50", v, prefix+"1")},
		{"list cursor page2", ""},
	}

	c.want(200, "GET", "/api/assets?limit=50", nil)

	var lines []string
	lines = append(lines, "# 列表接口压测（T16）", "")
	lines = append(lines, fmt.Sprintf("- 时间：%s（Asia/Hong_Kong）", time.Now().Format("2006-01-02 15:04:05")))
	lines = append(lines, fmt.Sprintf("- 数据：%d 条模拟子域（前缀 `%s`），入库耗时 %s", got, prefix, seedDur.Round(time.Millisecond)))
	lines = append(lines, "- 标准：GET /api/assets 列表（limit≤50）单次 < 1s", "")
	lines = append(lines, "| 场景 | 耗时 | 条数 | 结果 |", "|---|---|---|---|")

	var cursor any
	pass := true
	for _, r := range runs {
		path := r.Path
		if r.Name == "list cursor page2" {
			if cursor == nil {
				t.Fatal("no cursor from first page")
			}
			path = fmt.Sprintf("/api/assets?limit=50&cursor=%v", cursor)
		}
		start := time.Now()
		out := c.want(200, "GET", path, nil)
		dur := time.Since(start)
		items := out["items"].([]any)
		if r.Name == "list default limit=50" {
			cursor = out["next_cursor"]
		}
		ok := dur < time.Second
		if !ok {
			pass = false
		}
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		}
		line := fmt.Sprintf("| %s | %s | %d | %s |", r.Name, dur.Round(time.Millisecond), len(items), mark)
		lines = append(lines, line)
		t.Log(line)
		if !ok {
			t.Errorf("%s took %v (>= 1s)", r.Name, dur)
		}
	}
	lines = append(lines, "")
	if pass {
		lines = append(lines, "**结论**：全部场景 < 1s，达标。")
	} else {
		lines = append(lines, "**结论**：存在 ≥1s 场景，未达标。")
	}

	doc := filepath.Join("..", "docs", "bench.md")
	if err := os.MkdirAll(filepath.Dir(doc), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for i, l := range lines {
		if i > 0 {
			body += "\n"
		}
		body += l
	}
	body += "\n"
	if err := os.WriteFile(doc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", doc)
}
