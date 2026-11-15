package main

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// T58：假 resolver / 假 DNS 服务都绑 127.0.0.1，不触真实网络；IP 只用文档段或 TEST-NET。

func t58Resolver(addrs ...string) *resolver {
	return newResolver(dnsConfig{Resolvers: addrs, Timeout: 150 * time.Millisecond, QPS: 10000,
		WildcardProbe: 0, Concurrency: 1, VerifiedTTL: time.Hour})
}

func t58UDPServer(t *testing.T, handler dns.Handler) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: handler}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })
	return pc.LocalAddr().String()
}

// t58DeadResolver 绑定但不读写的 UDP 地址，用于制造解析超时。
func t58DeadResolver(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc.LocalAddr().String()
}

func t58Rcode(rcode int, hits *atomic.Int32) dns.Handler {
	return dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		if hits != nil {
			hits.Add(1)
		}
		m := new(dns.Msg)
		m.SetReply(req)
		m.Rcode = rcode
		_ = w.WriteMsg(m)
	})
}

func t58Empty(hits *atomic.Int32) dns.Handler {
	return dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		if hits != nil {
			hits.Add(1)
		}
		m := new(dns.Msg)
		m.SetReply(req)
		_ = w.WriteMsg(m)
	})
}

func t58AnswerA(ip string, hits *atomic.Int32) dns.Handler {
	return dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		if hits != nil {
			hits.Add(1)
		}
		m := new(dns.Msg)
		m.SetReply(req)
		if req.Question[0].Qtype == dns.TypeA {
			rr, err := dns.NewRR(req.Question[0].Name + " 60 IN A " + ip)
			if err == nil {
				m.Answer = append(m.Answer, rr)
			}
		}
		_ = w.WriteMsg(m)
	})
}

// 原因分类：NXDOMAIN/NODATA/REFUSED/SERVFAIL/超时各自落到对应 skip 原因，不再统一 no_dial_ip。
func TestT58SkipReasonClassification(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)

	nx := t58UDPServer(t, t58Rcode(dns.RcodeNameError, nil))
	nodata := t58UDPServer(t, t58Empty(nil))
	refused := t58UDPServer(t, t58Rcode(dns.RcodeRefused, nil))
	servfail := t58UDPServer(t, t58Rcode(dns.RcodeServerFailure, nil))
	dead := t58DeadResolver(t)

	cases := []struct {
		name   string
		lookup serviceDNSLookup
		want   string
	}{
		{"nxdomain", resolverFromDNS(t58Resolver(nx)), skipDNSNXDomain},
		{"nodata", resolverFromDNS(t58Resolver(nodata)), skipDNSNoData},
		{"refused", resolverFromDNS(t58Resolver(refused)), skipDNSError},
		{"servfail", resolverFromDNS(t58Resolver(servfail)), skipDNSError},
		{"timeout", resolverFromDNS(t58Resolver(dead)), skipDNSTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host := c.name + "." + domain
			id := t51InsertAsset(t, e, host, false)
			ips, err := prepareServiceDialIPs(ctx, e.pool, serviceFingerprintConfig{Lookup: c.lookup},
				serviceScanTarget{AssetID: id, Host: host})
			if err != nil {
				t.Fatal(err)
			}
			if len(ips) != 0 {
				t.Fatalf("skip case returned dial IPs")
			}
			var reason string
			if err := e.pool.QueryRow(ctx, `SELECT reason FROM service_scan_skips WHERE asset_id = $1`, id).Scan(&reason); err != nil {
				t.Fatal(err)
			}
			if reason != c.want {
				t.Fatalf("reason=%q want %q", reason, c.want)
			}
			if reason == skipNoDialIP {
				t.Fatalf("no_dial_ip must not be written anymore")
			}
		})
	}
}

// NXDOMAIN 必须由第二台 resolver 复核；第二台给出地址时按解析成功处理，不能落 dn_nxdomain。
func TestT58NXDomainSecondResolver(t *testing.T) {
	ctx := context.Background()
	var first, second, third atomic.Int32
	nx := t58UDPServer(t, t58Rcode(dns.RcodeNameError, &first))
	answer := t58UDPServer(t, t58AnswerA("192.0.2.64", &second))

	res, st := t58Resolver(nx, answer).lookupStatus(ctx, "nx-review.example.test")
	if st != dnsLookupOK || len(res.IPs) != 1 || res.IPs[0] != "192.0.2.64" {
		t.Fatalf("second resolver answer not used: status=%v ips=%v", st, res.IPs)
	}
	if first.Load() == 0 || second.Load() == 0 {
		t.Fatalf("both resolvers must be queried (first=%d second=%d)", first.Load(), second.Load())
	}

	// 两台都 NXDOMAIN 才落 dns_nxdomain。
	nx2 := t58UDPServer(t, t58Rcode(dns.RcodeNameError, &third))
	if _, st := t58Resolver(nx, nx2).lookupStatus(ctx, "nx-both.example.test"); st != dnsLookupNXDomain {
		t.Fatalf("two NXDOMAIN resolvers: status=%v want nxdomain", st)
	}
	if third.Load() == 0 {
		t.Fatalf("NXDOMAIN was not reviewed by the second resolver")
	}

	// 第二台无法复核（REFUSED）时不落 dns_nxdomain。
	refused := t58UDPServer(t, t58Rcode(dns.RcodeRefused, nil))
	if _, st := t58Resolver(nx, refused).lookupStatus(ctx, "nx-unconfirmed.example.test"); st == dnsLookupNXDomain {
		t.Fatalf("unconfirmed NXDOMAIN must not be authoritative")
	}
}

// A 成功、AAAA 超时：仍按解析成功并允许拨号。
func TestT58AWithoutAAAARemainsDialable(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)

	aOnly := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		if req.Question[0].Qtype != dns.TypeA {
			return
		}
		m := new(dns.Msg)
		m.SetReply(req)
		if rr, err := dns.NewRR(req.Question[0].Name + " 60 IN A 192.0.2.62"); err == nil {
			m.Answer = append(m.Answer, rr)
		}
		_ = w.WriteMsg(m)
	})
	lookup := resolverFromDNS(t58Resolver(t58UDPServer(t, aOnly)))

	answer, err := lookup(ctx, "a-only.example.test")
	if err != nil || len(answer.IPs) != 1 || answer.IPs[0].String() != "192.0.2.62" {
		t.Fatalf("A record dropped when AAAA times out: %v %v", answer.IPs, err)
	}

	host := "a-only." + domain
	id := t51InsertAsset(t, e, host, false)
	ips, err := prepareServiceDialIPs(ctx, e.pool, serviceFingerprintConfig{Lookup: lookup},
		serviceScanTarget{AssetID: id, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || ips[0].String() != "192.0.2.62" {
		t.Fatalf("dial IPs=%v want the A record", ips)
	}
	var skips int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM service_scan_skips WHERE asset_id = $1`, id).Scan(&skips)
	if skips != 0 {
		t.Fatalf("A success must not record a skip, got %d", skips)
	}
}

// EDNS0 随查询发出；UDP 响应 TC=1 时改 TCP 重查。
func TestT58EDNS0AndTCPFallback(t *testing.T) {
	var udpEDNS, tcpEDNS, tcpHits atomic.Int32
	udpHandler := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		if req.IsEdns0() != nil {
			udpEDNS.Store(1)
		}
		m := new(dns.Msg)
		m.SetReply(req)
		m.Truncated = true
		_ = w.WriteMsg(m)
	})
	tcpHandler := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		if req.IsEdns0() != nil {
			tcpEDNS.Store(1)
		}
		tcpHits.Add(1)
		m := new(dns.Msg)
		m.SetReply(req)
		if req.Question[0].Qtype == dns.TypeA {
			if rr, err := dns.NewRR(req.Question[0].Name + " 60 IN A 192.0.2.63"); err == nil {
				m.Answer = append(m.Answer, rr)
			}
		}
		_ = w.WriteMsg(m)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	udpSrv := &dns.Server{PacketConn: pc, Handler: udpHandler}
	tcpSrv := &dns.Server{Listener: ln, Handler: tcpHandler}
	go udpSrv.ActivateAndServe()
	go tcpSrv.ActivateAndServe()
	t.Cleanup(func() { udpSrv.Shutdown(); tcpSrv.Shutdown() })

	res, st := t58Resolver(pc.LocalAddr().String()).lookupStatus(context.Background(), "tc.example.test")
	if st != dnsLookupOK || len(res.IPs) != 1 || res.IPs[0] != "192.0.2.63" {
		t.Fatalf("TCP fallback: status=%v ips=%v", st, res.IPs)
	}
	if udpEDNS.Load() == 0 {
		t.Fatalf("EDNS0 missing on UDP query")
	}
	if tcpHits.Load() == 0 {
		t.Fatalf("truncated UDP response was not retried over TCP")
	}
	if tcpEDNS.Load() == 0 {
		t.Fatalf("EDNS0 missing on TCP retry")
	}
}

// 负缓存窗口内不解析；超出窗口才重新解析。
func TestT58NegativeCachePrefilters(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)

	recentHost := "negative-recent." + domain
	oldHost := "negative-old." + domain
	var recentID, oldID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, resolved_at)
		VALUES ($1, $2, '{dns}', false, now()) RETURNING id`, e.rootID, recentHost).Scan(&recentID); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved, resolved_at)
		VALUES ($1, $2, '{dns}', false, now() - interval '7 hours') RETURNING id`, e.rootID, oldHost).Scan(&oldID); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	cfg := serviceFingerprintConfig{Lookup: func(ctx context.Context, host string) (serviceDNSAnswer, error) {
		calls.Add(1)
		return serviceDNSAnswer{IPs: []net.IP{net.ParseIP("192.0.2.61")}}, nil
	}}

	ips, err := prepareServiceDialIPs(ctx, e.pool, cfg, serviceScanTarget{AssetID: recentID, Host: recentHost})
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 0 || calls.Load() != 0 {
		t.Fatalf("negative cache hit must not resolve: ips=%v calls=%d", ips, calls.Load())
	}
	var reason string
	if err := e.pool.QueryRow(ctx, `SELECT reason FROM service_scan_skips WHERE asset_id = $1`, recentID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != skipDNSNegativeCached {
		t.Fatalf("reason=%q want %q", reason, skipDNSNegativeCached)
	}

	ips, err = prepareServiceDialIPs(ctx, e.pool, cfg, serviceScanTarget{AssetID: oldID, Host: oldHost})
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || calls.Load() != 1 {
		t.Fatalf("expired negative cache should resolve: ips=%v calls=%d", ips, calls.Load())
	}
	var skips int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM service_scan_skips WHERE asset_id = $1`, oldID).Scan(&skips)
	if skips != 0 {
		t.Fatalf("expired negative cache must not record a skip, got %d", skips)
	}
}

func TestT58NegativeTTLConfig(t *testing.T) {
	t.Setenv("SERVICE_DNS_NEGATIVE_TTL", "")
	c, err := serviceFingerprintConfigFromEnv()
	if err != nil || c.NegativeTTL != 6*time.Hour {
		t.Fatalf("default negative TTL=%v err=%v", c.NegativeTTL, err)
	}
	t.Setenv("SERVICE_DNS_NEGATIVE_TTL", "8h")
	if c, err = serviceFingerprintConfigFromEnv(); err != nil || c.NegativeTTL != 8*time.Hour {
		t.Fatalf("negative TTL override=%v err=%v", c.NegativeTTL, err)
	}
	for _, v := range []string{"5h", "25h", "0s", "x"} {
		t.Setenv("SERVICE_DNS_NEGATIVE_TTL", v)
		if _, err := serviceFingerprintConfigFromEnv(); err == nil {
			t.Errorf("SERVICE_DNS_NEGATIVE_TTL=%s should be rejected", v)
		}
	}
}

// 同一资产同一原因多轮只保留一行：count 累加、last_seen 刷新、scan_task_id 取最近非空。
func TestT58SkipUpsertDedup(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	host := "dedup." + domain
	id := t51InsertAsset(t, e, host, false)

	newTask := func() int64 {
		var taskID int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind)
			SELECT vendor_id, id, 'service' FROM root_domains WHERE id = $1 RETURNING id`, e.rootID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		return taskID
	}
	first, second := newTask(), newTask()
	for _, taskID := range []int64{first, second} {
		if err := recordServiceScanSkip(ctx, e.pool, serviceScanTarget{AssetID: id, Host: host, ScanTaskID: taskID}, skipDNSNXDomain); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	var count, taskID int64
	var firstSeen, lastSeen *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT count(*), max(count), max(scan_task_id), max(first_seen), max(last_seen)
		FROM service_scan_skips WHERE asset_id = $1 AND reason = $2`, id, skipDNSNXDomain).
		Scan(&rows, &count, &taskID, &firstSeen, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || count != 2 || taskID != second || firstSeen == nil || lastSeen == nil {
		t.Fatalf("rows=%d count=%d task=%d first=%v last=%v", rows, count, taskID, firstSeen, lastSeen)
	}
	var distinct int
	e.pool.QueryRow(ctx, `SELECT count(DISTINCT asset_id) FROM service_scan_skips WHERE reason = $1`, skipDNSNXDomain).Scan(&distinct)
	if distinct != 1 {
		t.Fatalf("statistics must count distinct assets, got %d", distinct)
	}

	// 主机级跳过（asset_id 为空）按 host + reason 去重。
	hostOnly := "out." + domain
	for i := 0; i < 2; i++ {
		if err := recordServiceScanSkip(ctx, e.pool, serviceScanTarget{Host: hostOnly}, skipServiceScanOutOfScope); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*), max(count) FROM service_scan_skips WHERE host = $1 AND reason = $2`,
		hostOnly, skipServiceScanOutOfScope).Scan(&rows, &count); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || count != 2 {
		t.Fatalf("host-only rows=%d count=%d", rows, count)
	}
}

// 迁移 036 可重跑：旧行回填 count=1，历史重复行合并为一行并累计 count。
func TestT58Migration036RerunAndMerge(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	sqlBytes, err := migrationFS.ReadFile("migrations/036_service_skip_dedup.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for i := 0; i < 2; i++ {
		if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("migration rerun %d: %v", i+1, err)
		}
	}
	var cols int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'service_scan_skips' AND column_name IN ('scan_task_id', 'first_seen', 'last_seen', 'count')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 4 {
		t.Fatalf("service_scan_skips new columns=%d want 4", cols)
	}
	var idx int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'service_scan_skips'
		AND indexname IN ('service_scan_skips_asset_reason_key', 'service_scan_skips_host_reason_key')`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 2 {
		t.Fatalf("dedup indexes=%d want 2", idx)
	}

	// 模拟历史重复行：先去掉唯一索引，再插入同资产同原因的两条旧行。
	domain := t51RootDomain(t, e)
	host := "legacy-dup." + domain
	id := t51InsertAsset(t, e, host, false)
	if _, err := e.pool.Exec(ctx, `DROP INDEX IF EXISTS service_scan_skips_asset_reason_key`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_skips (host, asset_id, reason) VALUES ($1, $2, $3)`,
			host, id, skipDNSNoData); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("migration merge: %v", err)
	}
	var rows int
	var count int64
	if err := e.pool.QueryRow(ctx, `SELECT count(*), max(count) FROM service_scan_skips WHERE asset_id = $1 AND reason = $2`,
		id, skipDNSNoData).Scan(&rows, &count); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || count != 2 {
		t.Fatalf("merged rows=%d count=%d want 1/2", rows, count)
	}
	if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("migration rerun after merge: %v", err)
	}
}

// 重解析命中 CDN：记 cdn_fronted 并跳过，不拨号；resolverFromDNS 保留 CNAME。
func TestT58ResolvedCDNSkippedBeforeDial(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	t.Cleanup(resetStaticCDNLibrary)
	resetStaticCDNLibrary()
	registerCDNSuffix("example-cdn.test", "testcdn")
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	host := "cdn-recheck." + domain
	id := t51InsertAsset(t, e, host, false)

	lookup := func(ctx context.Context, h string) (serviceDNSAnswer, error) {
		return serviceDNSAnswer{
			IPs:    []net.IP{net.ParseIP("192.0.2.65")},
			CNAMEs: []string{"edge.example-cdn.test"},
		}, nil
	}
	ips, err := prepareServiceDialIPs(ctx, e.pool, serviceFingerprintConfig{Lookup: lookup},
		serviceScanTarget{AssetID: id, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 0 {
		t.Fatalf("CDN answer must not be dialed, got %d address(es)", len(ips))
	}
	var reason string
	if err := e.pool.QueryRow(ctx, `SELECT reason FROM service_scan_skips WHERE asset_id = $1`, id).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != skipServiceScanCDN {
		t.Fatalf("reason=%q want %q", reason, skipServiceScanCDN)
	}
	var stored string
	if err := e.pool.QueryRow(ctx, `SELECT host || '|' || reason FROM service_scan_skips WHERE asset_id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "192.0.2") {
		t.Fatalf("resolved IP leaked into skip row: %q", stored)
	}

	// resolverFromDNS 必须把 CNAME 带给拨号前复判。
	addr, _ := mockDNS(t)
	answer, err := resolverFromDNS(t58Resolver(addr))(ctx, "cdn.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.CNAMEs) != 1 || answer.CNAMEs[0] != "x.kunlunsl.com" {
		t.Fatalf("resolverFromDNS dropped CNAME: %v", answer.CNAMEs)
	}

	// CDN 未判定（cdn_fronted 为 NULL）仍由 T50 门控跳过，不进拨号准备。
	unkHost := "cdn-unknown." + domain
	var unkID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved)
		VALUES ($1, $2, '{dns}', true) RETURNING id`, e.rootID, unkHost).Scan(&unkID); err != nil {
		t.Fatal(err)
	}
	t51Enable(t, e, domain)
	queue, err := planOriginServiceScan(ctx, e.pool, []serviceScanTarget{{AssetID: unkID, Host: unkHost}})
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 0 {
		t.Fatalf("unjudged CDN must stay skipped, queue=%v", queue)
	}
	if err := e.pool.QueryRow(ctx, `SELECT reason FROM service_scan_skips WHERE asset_id = $1`, unkID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != skipServiceScanCDNUnknown {
		t.Fatalf("unjudged reason=%q want %q", reason, skipServiceScanCDNUnknown)
	}
}
