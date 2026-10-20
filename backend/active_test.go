package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// zoneDNS 是本地 mock DNS：records 精确匹配，wild 后缀下任意子域返回固定 IP（泛解析）。
func zoneDNS(t *testing.T, records map[string]string, wild map[string]string) (string, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := &dns.Server{Addr: "127.0.0.1:0", Net: "udp", Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		n.Add(1)
		q := req.Question[0]
		name := strings.TrimSuffix(strings.ToLower(q.Name), ".")
		m := new(dns.Msg)
		m.SetReply(req)
		ip := records[name]
		for suf, wip := range wild {
			if ip == "" && strings.HasSuffix(name, "."+suf) {
				ip = wip
			}
		}
		if ip == "" {
			m.Rcode = dns.RcodeNameError
		} else if q.Qtype == dns.TypeA {
			rr, _ := dns.NewRR(q.Name + " 60 IN A " + ip)
			m.Answer = append(m.Answer, rr)
		}
		w.WriteMsg(m)
	})}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go srv.ListenAndServe()
	<-ready
	t.Cleanup(func() { srv.Shutdown() })
	return srv.PacketConn.LocalAddr().String(), &n
}

var actRecords = map[string]string{
	"www.act.test":      "192.0.2.21",
	"api.act.test":      "192.0.2.22",
	"dev.api.act.test":  "192.0.2.23", // 递归一层命中
	"app2.act.test":     "192.0.2.24", // 已知子域
	"app3.act.test":     "192.0.2.25", // 数字变形命中
	"app2-dev.act.test": "192.0.2.26", // 后缀变形命中
	"cdn.act.test":      "192.0.2.27", // 其下为泛解析
}

func activeTestScanner(t *testing.T, words []string) (*activeScanner, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	trustedRec := map[string]string{}
	for k, v := range actRecords {
		trustedRec[k] = v
	}
	bulkRec := map[string]string{"poison.act.test": "198.51.100.66"} // 只有批量 resolver 解析：投毒
	for k, v := range actRecords {
		bulkRec[k] = v
	}
	wild := map[string]string{"cdn.act.test": "192.0.2.99"}
	bulk, nb := zoneDNS(t, bulkRec, wild)
	trusted, nt := zoneDNS(t, trustedRec, wild)
	dcfg := dnsConfig{Resolvers: []string{bulk}, Timeout: 500 * time.Millisecond, WildcardProbe: 3, WildcardStable: 5,
		WildcardMax: 60, Concurrency: 8, VerifiedTTL: time.Hour}
	return newActiveScanner(activeConfig{QPS: 5000, MaxCandidates: 20000, Trusted: []string{trusted}, Words: words}, dcfg), nb, nt
}

var testWords = []string{"www", "api", "dev", "mail", "cdn", "poison", "nx"}

func TestMutations(t *testing.T) {
	got := mutations("app2.x.test")
	for _, w := range []string{"app1.x.test", "app3.x.test", "dev-app2.x.test", "app2-api.x.test", "staging.app2.x.test"} {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Fatalf("missing %s in %v", w, got)
		}
	}
	if n := len(mutations("a1b2c3d4e5f6g7h8i9j10k11l12m13n14o15p16q17r18s19t20u21v22w23x24.x.test")); n != maxMutations {
		t.Fatalf("mutations not capped: %d", n)
	}
	if m := mutations("db0.x.test"); m[0] != "db1.x.test" { // 0-1 不生成负数
		t.Fatalf("db0: %v", m)
	}
	t.Logf("app2.x.test -> %d 个变形，如 %v", len(got), got[:6])
}

func TestDiscoverMockDNS(t *testing.T) {
	a, nb, nt := activeTestScanner(t, testWords)
	known := []string{"app2.act.test"}
	hits, tried := a.discover(context.Background(), "act.test", known, known)
	want := []string{"api.act.test", "app2-dev.act.test", "app3.act.test", "cdn.act.test", "dev.api.act.test", "www.act.test"}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits %v\nwant %v", hits, want)
	}
	t.Logf("候选 %d 个，命中 %v；批量 resolver 查询 %d 次，可信 resolver 查询 %d 次（poison 被复核剔除，cdn 下泛解析被过滤）",
		tried, hits, nb.Load(), nt.Load())
}

func TestDiscoverCandidateCap(t *testing.T) {
	a, nb, _ := activeTestScanner(t, testWords)
	a.cfg.MaxCandidates = 3
	_, tried := a.discover(context.Background(), "act.test", nil, nil)
	if tried != 3 || nb.Load() > 6 { // 3 个候选 × A/AAAA
		t.Fatalf("tried=%d bulk queries=%d", tried, nb.Load())
	}
	t.Logf("ACTIVE_MAX_CANDIDATES=3：候选 %d，批量查询 %d 次", tried, nb.Load())
}

func TestActiveQPS(t *testing.T) {
	a, nb, nt := activeTestScanner(t, []string{"nx1", "nx2", "nx3", "nx4", "nx5"})
	a.bulk.limiter.SetLimit(20) // ACTIVE_QPS=20，批量与可信共享
	start := time.Now()
	a.discover(context.Background(), "act.test", nil, nil)
	el := time.Since(start)
	q := nb.Load() + nt.Load()
	// 5 个候选 × A/AAAA = 10 次查询，20 qps 突发 1：>= 450ms
	if q != 10 || el < 430*time.Millisecond {
		t.Fatalf("queries=%d elapsed=%v", q, el)
	}
	t.Logf("ACTIVE_QPS=20：%d 次查询耗时 %v", q, el.Round(time.Millisecond))
}

func TestActiveConfigFromEnv(t *testing.T) {
	c, err := activeConfigFromEnv()
	if err != nil || c.QPS != 20 || c.MaxCandidates != 20000 || len(c.Words) < 4800 ||
		!reflect.DeepEqual(c.Trusted, []string{"223.5.5.5:53", "119.29.29.29:53", "1.1.1.1:53", "8.8.8.8:53"}) {
		t.Fatalf("defaults: qps=%v max=%d words=%d trusted=%v err=%v", c.QPS, c.MaxCandidates, len(c.Words), c.Trusted, err)
	}
	t.Logf("内置字典有效词 %d 个（原 5000 行，跳过不合法/重复）", len(c.Words))
	p := filepath.Join(t.TempDir(), "words.txt")
	os.WriteFile(p, []byte("# comment\nWWW\n\nbad_word\nfoo.bar\nwww\n"), 0o644)
	t.Setenv("ACTIVE_WORDLIST", p)
	t.Setenv("ACTIVE_TRUSTED_RESOLVERS", "127.0.0.1:5353, 10.0.0.1")
	c, err = activeConfigFromEnv()
	if err != nil || !reflect.DeepEqual(c.Words, []string{"www", "foo.bar"}) || !reflect.DeepEqual(c.Trusted, []string{"127.0.0.1:5353", "10.0.0.1:53"}) {
		t.Fatalf("external: %v %v %v", c.Words, c.Trusted, err)
	}
	t.Setenv("ACTIVE_WORDLIST", "")
	t.Setenv("ACTIVE_TRUSTED_RESOLVERS", "")
	for k, vs := range map[string][]string{"ACTIVE_QPS": {"0", "-1", "NaN", "Inf", "x"}, "ACTIVE_MAX_CANDIDATES": {"0", "-5", "1.5", "x"},
		"ACTIVE_TRUSTED_RESOLVERS": {" , "}, "ACTIVE_WORDLIST": {"/nonexistent/words.txt"}} {
		for _, v := range vs {
			t.Setenv(k, v)
			if _, err := activeConfigFromEnv(); err == nil {
				t.Fatalf("%s=%q accepted", k, v)
			}
			t.Setenv(k, "")
		}
	}
}

// 全链路：厂商开关关闭时不发任何 DNS 查询；打开后 active → dns → http，命中子域入库（来源 active）。
func TestActiveSwitchEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	a, nb, nt := activeTestScanner(t, testWords)
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	clean := func() {
		e.pool.Exec(context.Background(), `DELETE FROM assets WHERE root_id IN (SELECT id FROM root_domains WHERE domain = 'act.test')`)
		e.pool.Exec(context.Background(), `DELETE FROM root_domains WHERE domain = 'act.test'`)
		e.pool.Exec(context.Background(), `UPDATE vendors SET active_enabled = false WHERE id = $1`, vendorID)
	}
	clean()
	t.Cleanup(clean)
	var rootID int64
	e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'act.test') RETURNING id`, vendorID).Scan(&rootID)
	e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, resolved, wildcard) VALUES ($1, 'app2.act.test', true, false)`, rootID)
	e.rootID = rootID
	var mu sync.Mutex
	var httpIn []string
	e.start(t, testConfig(fastRates), map[string]ScanHandler{
		"active": activeHandler(e.pool, a),
		"dns":    dnsHandler(e.pool, a.trusted),
		"http": func(_ context.Context, _ int64, hosts []string) ([]string, error) {
			mu.Lock()
			httpIn = append(httpIn, hosts...)
			mu.Unlock()
			return nil, nil
		},
	})
	// 开关关闭（默认）
	if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "active", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "active", "done", 1)
	var dnsTasks, n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE root_id = $1 AND kind = 'dns'`, rootID).Scan(&dnsTasks)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE root_id = $1`, rootID).Scan(&n)
	if nb.Load()+nt.Load() != 0 || dnsTasks != 0 || n != 1 {
		t.Fatalf("switch off: queries=%d dns tasks=%d assets=%d", nb.Load()+nt.Load(), dnsTasks, n)
	}
	t.Logf("开关关闭：DNS 查询 0 次，无后续任务")
	// 打开
	e.pool.Exec(ctx, `UPDATE vendors SET active_enabled = true WHERE id = $1`, vendorID)
	if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "active", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "http", "done", 1)
	rows, _ := e.pool.Query(ctx, `SELECT subdomain FROM assets WHERE root_id = $1 AND 'active' = ANY(sources) ORDER BY 1`, rootID)
	got, _ := pgxCollectStrings(rows)
	want := []string{"api.act.test", "app2-dev.act.test", "app3.act.test", "cdn.act.test", "dev.api.act.test", "www.act.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active assets %v want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(httpIn) != len(want) {
		t.Fatalf("http got %v", httpIn)
	}
	t.Logf("开关打开：入库 %v，经 dns 交 http %d 个", got, len(httpIn))
}

func pgxCollectStrings(rows interface {
	Next() bool
	Scan(...any) error
	Close()
}) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
