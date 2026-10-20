package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// mockDNS 在 127.0.0.1 起本地 UDP DNS 服务。zone：
//
//	a.example.test           A 192.0.2.1
//	v6.example.test          AAAA 2001:db8::1
//	*.wild.example.test      A 192.0.2.9（泛解析）
//	real.wild.example.test   A 192.0.2.2（泛解析下的真实记录）
//	cdn.example.test         CNAME x.kunlunsl.com → A 192.0.2.3
//	其他                     NXDOMAIN
//
// mvIP 是 mv.example.test 的当前解析结果（T9 变更测试用）。
var mvIP atomic.Value

func init() { mvIP.Store("192.0.2.10") }

func mockDNS(t *testing.T) (addr string, queried func() []string) {
	var mu sync.Mutex
	var names []string
	rot := 0
	rotZone := map[string]int{}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := func(name, ip string) dns.RR { rr, _ := dns.NewRR(name + " 60 IN A " + ip); return rr }
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		q := req.Question[0]
		name := strings.ToLower(q.Name)
		mu.Lock()
		names = append(names, strings.TrimSuffix(name, "."))
		mu.Unlock()
		m := new(dns.Msg)
		m.SetReply(req)
		isA, isAAAA := q.Qtype == dns.TypeA, q.Qtype == dns.TypeAAAA
		switch {
		case name == "a.example.test.":
			if isA {
				m.Answer = append(m.Answer, a(q.Name, "192.0.2.1"))
			}
		case name == "mv.example.test.": // T9：IP 可在两轮之间变化
			if isA {
				m.Answer = append(m.Answer, a(q.Name, mvIP.Load().(string)))
			}
		case name == "v6.example.test.":
			if isAAAA {
				rr, _ := dns.NewRR(q.Name + " 60 IN AAAA 2001:db8::1")
				m.Answer = append(m.Answer, rr)
			}
		case name == "real.wild.example.test.":
			if isA {
				m.Answer = append(m.Answer, a(q.Name, "192.0.2.2"))
			}
		case strings.HasSuffix(name, ".wild.example.test."):
			if isA {
				m.Answer = append(m.Answer, a(q.Name, "192.0.2.9"))
			}
		case strings.HasSuffix(name, ".s1.test.") || strings.HasSuffix(name, ".r8.test.") || strings.HasSuffix(name, ".r60.test."):
			if isA { // T8 端到端：lb.* 与泛解析共用 IP；s1 单 IP，r8/r60 轮换 8/60 个
				zone := name[strings.Index(name, ".")+1:]
				n := map[string]int{"s1.test.": 1, "r8.test.": 8, "r60.test.": 60}[zone]
				base := map[string]string{"s1.test.": "10.4.0.", "r8.test.": "10.5.0.", "r60.test.": "10.6.0."}[zone]
				ip := 1
				switch {
				case strings.HasPrefix(name, "lb."):
				case zone == "r60.test." && (strings.HasPrefix(name, "fake00.") || strings.HasPrefix(name, "fake01.")):
					ip = 99 // 泛解析返回、但随机子域探测从未见过的 IP：确定性复现 DNS 漏标
				case n > 1:
					ip = mrand.IntN(n) + 1 // 随机轮换
				}
				_ = rotZone
				m.Answer = append(m.Answer, a(q.Name, fmt.Sprintf("%s%d", base, ip)))
			}
		case name == "real.rot.test.":
			if isA {
				m.Answer = append(m.Answer, a(q.Name, "10.8.8.8"))
			}
		case strings.HasSuffix(name, ".rot.test."):
			if isA { // 泛解析轮换 10.1.0.1–10.1.0.8
				mu.Lock()
				rot++
				n := rot%8 + 1
				mu.Unlock()
				m.Answer = append(m.Answer, a(q.Name, fmt.Sprintf("10.1.0.%d", n)))
			}
		case name == "cdn.example.test.":
			if isA {
				rr, _ := dns.NewRR(q.Name + " 60 IN CNAME x.kunlunsl.com.")
				m.Answer = append(m.Answer, rr, a("x.kunlunsl.com.", "192.0.2.3"))
			}
		default:
			m.Rcode = dns.RcodeNameError
		}
		w.WriteMsg(m)
	})}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })
	return pc.LocalAddr().String(), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), names...) }
}

func testResolver(addrs ...string) *resolver {
	return newResolver(dnsConfig{Resolvers: addrs, Timeout: 500 * time.Millisecond, QPS: 10000, WildcardProbe: 3, WildcardStable: 5, WildcardMax: 30, Concurrency: 4, VerifiedTTL: 168 * time.Hour})
}

func TestResolveHostsMock(t *testing.T) {
	addr, queried := mockDNS(t)
	r := testResolver(addr)
	hosts := []string{"a.example.test", "v6.example.test", "real.wild.example.test", "fake1.wild.example.test",
		"fake2.wild.example.test", "cdn.example.test", "nx.example.test"}
	got := map[string]hostDNS{}
	for _, hd := range r.resolveHosts(context.Background(), "example.test", hosts) {
		got[hd.Host] = hd
	}
	check := func(h string, resolved, wildcard bool, ips []string, cdn string) {
		t.Helper()
		g := got[h]
		if g.Resolved != resolved || g.Wildcard != wildcard || g.CDN != cdn || (ips != nil && !reflect.DeepEqual(g.Result.IPs, ips)) {
			t.Errorf("%s: %+v", h, g)
		}
	}
	check("a.example.test", true, false, []string{"192.0.2.1"}, "")
	check("v6.example.test", true, false, []string{"2001:db8::1"}, "")
	check("real.wild.example.test", true, false, []string{"192.0.2.2"}, "")
	check("fake1.wild.example.test", true, true, []string{"192.0.2.9"}, "")
	check("fake2.wild.example.test", true, true, []string{"192.0.2.9"}, "")
	check("cdn.example.test", true, false, []string{"192.0.2.3"}, "aliyun")
	check("nx.example.test", false, false, nil, "")
	if c := got["cdn.example.test"].Result.CNAMEs; !reflect.DeepEqual(c, []string{"x.kunlunsl.com"}) {
		t.Errorf("cnames %v", c)
	}
	for _, n := range queried() { // 只查询根域内的名字
		if !strings.HasSuffix(n, ".example.test") {
			t.Errorf("queried outside root: %s", n)
		}
	}
}

// 泛解析轮换 IP：40 个随机子域全部标 wildcard，real.rot.test（10.8.8.8）不误杀。
func TestWildcardRotatingIPs(t *testing.T) {
	addr, queried := mockDNS(t)
	r := testResolver(addr)
	hosts := []string{"real.rot.test"}
	for i := 0; i < 40; i++ {
		b := make([]byte, 6)
		cryptoRand(b)
		hosts = append(hosts, fmt.Sprintf("r%x.rot.test", b))
	}
	before := len(queried())
	marked := 0
	for _, hd := range r.resolveHosts(context.Background(), "rot.test", hosts) {
		if hd.Host == "real.rot.test" {
			if hd.Wildcard || !hd.Resolved {
				t.Errorf("real.rot.test misclassified: %+v", hd)
			}
			continue
		}
		if hd.Wildcard {
			marked++
		}
	}
	probes := 0
	for _, n := range queried()[before:] {
		if strings.HasPrefix(n, "wc-") {
			probes++
		}
	}
	t.Logf("wildcard marked %d/40; probe queries (A+AAAA) %d", marked, probes)
	if marked != 40 {
		t.Fatalf("marked %d/40", marked)
	}
	if probes > 2*30 {
		t.Fatalf("probes %d exceed budget", probes)
	}
}

func TestResolverFallback(t *testing.T) {
	addr, _ := mockDNS(t)
	dead, _ := net.ListenPacket("udp", "127.0.0.1:0") // 不应答的 resolver
	defer dead.Close()
	r := testResolver(dead.LocalAddr().String(), addr)
	res, err := r.lookup(context.Background(), "a.example.test")
	if err != nil || !reflect.DeepEqual(res.IPs, []string{"192.0.2.1"}) {
		t.Fatalf("fallback: %+v %v", res, err)
	}
}

func TestCDNOf(t *testing.T) {
	cases := map[string]string{"x.kunlunsl.com": "aliyun", "a.cdn.dnsv1.com.": "tencent", "X.CloudFront.NET": "cloudfront",
		"notkunlunsl.com": "", "kunlunsl.com.evil.test": ""}
	for c, want := range cases {
		if got := cdnOf([]string{c}); got != want {
			t.Errorf("cdnOf(%s) = %q want %q", c, got, want)
		}
	}
}

// cdncheck 内置 IP 段：本地匹配，不联网（IP 取自其内置数据 cloudfront 段）。
func TestCDNOfIPs(t *testing.T) {
	if got := cdnOfIPs([]string{"192.0.2.1", "120.52.22.100"}); got != "cloudfront" {
		t.Errorf("cdnOfIPs = %q, want cloudfront", got)
	}
	if got := cdnOfIPs([]string{"192.0.2.1", "not-an-ip"}); got != "" {
		t.Errorf("non-CDN = %q", got)
	}
}

// QPS 限速：30 次查询 @20/s（burst 1）应约 1.45s。
func TestDNSQPS(t *testing.T) {
	addr, queried := mockDNS(t)
	r := newResolver(dnsConfig{Resolvers: []string{addr}, Timeout: time.Second, QPS: 20, WildcardProbe: 0, Concurrency: 10})
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 15; i++ { // 每个 host 查 A + AAAA = 2 次
		wg.Add(1)
		go func() { defer wg.Done(); r.lookup(context.Background(), "a.example.test") }()
	}
	wg.Wait()
	el := time.Since(start)
	n := len(queried())
	t.Logf("%d queries @20qps took %v", n, el.Round(time.Millisecond))
	if n != 30 || el < 1300*time.Millisecond || el > 2200*time.Millisecond {
		t.Fatalf("%d queries in %v, want 30 in ~1.45s", n, el)
	}
}

func TestDNSConfigFromEnv(t *testing.T) {
	t.Setenv("DNS_RESOLVERS", "127.0.0.1:5353, 10.0.0.1")
	t.Setenv("DNS_TIMEOUT", "1s")
	c, err := dnsConfigFromEnv()
	if err != nil || !reflect.DeepEqual(c.Resolvers, []string{"127.0.0.1:5353", "10.0.0.1:53"}) || c.Timeout != time.Second {
		t.Fatalf("%+v %v", c, err)
	}
	if c.QPS != 50 {
		t.Fatalf("default QPS %v", c.QPS)
	}
	t.Setenv("DNS_QPS", "12.5")
	if c, _ = dnsConfigFromEnv(); c.QPS != 12.5 {
		t.Fatalf("QPS override %v", c.QPS)
	}
	if c.WildcardStable != 5 || c.WildcardMax != 60 {
		t.Fatalf("wildcard defaults %+v", c)
	}
	t.Setenv("DNS_WILDCARD_STABLE", "10")
	t.Setenv("DNS_WILDCARD_MAX", "5")
	if _, err := dnsConfigFromEnv(); err == nil {
		t.Error("MAX < STABLE should be rejected")
	}
	t.Setenv("DNS_WILDCARD_STABLE", "")
	t.Setenv("DNS_WILDCARD_MAX", "")
	for k, vs := range map[string][]string{"DNS_TIMEOUT": {"0s", "x"}, "DNS_QPS": {"0", "-1", "NaN", "Inf", "x"},
		"DNS_WILDCARD_STABLE": {"0", "x"}, "DNS_WILDCARD_MAX": {"0", "-3"}} {
		for _, v := range vs {
			t.Setenv(k, v)
			if _, err := dnsConfigFromEnv(); err == nil {
				t.Errorf("%s=%s should be rejected", k, v)
			}
		}
		t.Setenv(k, "")
	}
}

// 端到端：passive（mock 源）→ dns（本地 mock DNS）→ http；泛解析测试域的假子域不交给 http，资产被标记。
func TestDNSHandlerWildcardNoFakeAssets(t *testing.T) {
	e := newTestEnv(t)
	addr, _ := mockDNS(t)
	ctx := context.Background()
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	clean := func() {
		e.pool.Exec(context.Background(), `DELETE FROM assets WHERE subdomain LIKE '%.example.test' AND root_id IN (SELECT id FROM root_domains WHERE domain = 'example.test')`)
		e.pool.Exec(context.Background(), `DELETE FROM root_domains WHERE domain = 'example.test'`)
	}
	clean()
	t.Cleanup(clean)
	var rootID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'example.test') RETURNING id`, vendorID).Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	e.rootID = rootID

	fakes := []string{}
	for i := 0; i < 20; i++ {
		fakes = append(fakes, "fake"+string(rune('a'+i))+".wild.example.test")
	}
	var mu sync.Mutex
	var httpIn []string
	e.start(t, testConfig(fastRates), map[string]ScanHandler{
		"passive": passiveHandler(e.pool, []Source{{"crtsh", func(context.Context, string) ([]string, error) {
			return append([]string{"a.example.test", "real.wild.example.test", "cdn.example.test", "nx.example.test"}, fakes...), nil
		}}}),
		"dns": dnsHandler(e.pool, testResolver(addr)),
		"http": func(_ context.Context, _ int64, hosts []string) ([]string, error) {
			mu.Lock()
			httpIn = append(httpIn, hosts...)
			mu.Unlock()
			return nil, nil
		},
	})
	if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "passive", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "http", "done", 1)
	mu.Lock()
	sort.Strings(httpIn)
	mu.Unlock()
	// T8 起 IP 判为泛解析的候选也交给 http 做页面相似度复核
	want := append([]string{"a.example.test", "cdn.example.test"}, fakes...)
	want = append(want, "real.wild.example.test")
	sort.Strings(want)
	if !reflect.DeepEqual(httpIn, want) {
		t.Fatalf("http got %v, want %v", httpIn, want)
	}
	var wild, live, unresolved int
	e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE wildcard), count(*) FILTER (WHERE resolved AND NOT wildcard),
		count(*) FILTER (WHERE resolved = false) FROM assets WHERE root_id = $1`, rootID).Scan(&wild, &live, &unresolved)
	if wild != 20 || live != 3 || unresolved != 1 {
		t.Fatalf("wildcard=%d live=%d unresolved=%d, want 20/3/1", wild, live, unresolved)
	}
	var cdn string
	var ips []string
	e.pool.QueryRow(ctx, `SELECT cdn, ips FROM assets WHERE subdomain = 'cdn.example.test'`).Scan(&cdn, &ips)
	if cdn != "aliyun" || !reflect.DeepEqual(ips, []string{"192.0.2.3"}) {
		t.Fatalf("cdn asset: %s %v", cdn, ips)
	}
	t.Logf("live(non-wildcard) assets: %d of %d candidates; %d wildcard marked", live, 24, wild)
}

func cryptoRand(b []byte) { crand.Read(b) }
