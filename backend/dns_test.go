package main

import (
	"context"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// mockDNS 在 127.0.0.1 起本地 UDP DNS 服务。zone：
//
//	a.example.test           A 1.1.1.1
//	v6.example.test          AAAA 2001:db8::1
//	*.wild.example.test      A 9.9.9.9（泛解析）
//	real.wild.example.test   A 2.2.2.2（泛解析下的真实记录）
//	cdn.example.test         CNAME x.kunlunsl.com → A 3.3.3.3
//	其他                     NXDOMAIN
func mockDNS(t *testing.T) (addr string, queried func() []string) {
	var mu sync.Mutex
	var names []string
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
				m.Answer = append(m.Answer, a(q.Name, "1.1.1.1"))
			}
		case name == "v6.example.test.":
			if isAAAA {
				rr, _ := dns.NewRR(q.Name + " 60 IN AAAA 2001:db8::1")
				m.Answer = append(m.Answer, rr)
			}
		case name == "real.wild.example.test.":
			if isA {
				m.Answer = append(m.Answer, a(q.Name, "2.2.2.2"))
			}
		case strings.HasSuffix(name, ".wild.example.test."):
			if isA {
				m.Answer = append(m.Answer, a(q.Name, "9.9.9.9"))
			}
		case name == "cdn.example.test.":
			if isA {
				rr, _ := dns.NewRR(q.Name + " 60 IN CNAME x.kunlunsl.com.")
				m.Answer = append(m.Answer, rr, a("x.kunlunsl.com.", "3.3.3.3"))
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
	return newResolver(dnsConfig{Resolvers: addrs, Timeout: 500 * time.Millisecond, WildcardProbe: 3, Concurrency: 4})
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
	check("a.example.test", true, false, []string{"1.1.1.1"}, "")
	check("v6.example.test", true, false, []string{"2001:db8::1"}, "")
	check("real.wild.example.test", true, false, []string{"2.2.2.2"}, "")
	check("fake1.wild.example.test", true, true, []string{"9.9.9.9"}, "")
	check("fake2.wild.example.test", true, true, []string{"9.9.9.9"}, "")
	check("cdn.example.test", true, false, []string{"3.3.3.3"}, "aliyun")
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

func TestResolverFallback(t *testing.T) {
	addr, _ := mockDNS(t)
	dead, _ := net.ListenPacket("udp", "127.0.0.1:0") // 不应答的 resolver
	defer dead.Close()
	r := testResolver(dead.LocalAddr().String(), addr)
	res, err := r.lookup(context.Background(), "a.example.test")
	if err != nil || !reflect.DeepEqual(res.IPs, []string{"1.1.1.1"}) {
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

func TestDNSConfigFromEnv(t *testing.T) {
	t.Setenv("DNS_RESOLVERS", "127.0.0.1:5353, 10.0.0.1")
	t.Setenv("DNS_TIMEOUT", "1s")
	c, err := dnsConfigFromEnv()
	if err != nil || !reflect.DeepEqual(c.Resolvers, []string{"127.0.0.1:5353", "10.0.0.1:53"}) || c.Timeout != time.Second {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("DNS_TIMEOUT", "0s")
	if _, err := dnsConfigFromEnv(); err == nil {
		t.Fatal("DNS_TIMEOUT=0s should be rejected")
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
	if want := []string{"a.example.test", "cdn.example.test", "real.wild.example.test"}; !reflect.DeepEqual(httpIn, want) {
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
	if cdn != "aliyun" || !reflect.DeepEqual(ips, []string{"3.3.3.3"}) {
		t.Fatalf("cdn asset: %s %v", cdn, ips)
	}
	t.Logf("live(non-wildcard) assets: %d of %d candidates; %d wildcard marked", live, 24, wild)
}
