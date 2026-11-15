package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func t51RootDomain(t *testing.T, e *testEnv) string {
	t.Helper()
	var domain string
	if err := e.pool.QueryRow(context.Background(), `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	return domain
}

func t51Enable(t *testing.T, e *testEnv, pattern string) {
	t.Helper()
	resetServiceScan(t, e)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_settings (id, enabled) VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO service_scan_scopes (pattern) VALUES ($1)`, pattern); err != nil {
		t.Fatal(err)
	}
}

func t51InsertAsset(t *testing.T, e *testEnv, host string, fronted bool) int64 {
	t.Helper()
	var id int64
	err := e.pool.QueryRow(context.Background(),
		`INSERT INTO assets (root_id, subdomain, sources, resolved, cdn_fronted) VALUES ($1, $2, '{dns}', true, $3) RETURNING id`,
		e.rootID, host, fronted).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// t51Listen 只绑 127.0.0.1，测完由 t.Cleanup 关闭。
func t51Listen(t *testing.T, onAccept func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go onAccept(c)
		}
	}()
	addr := ln.Addr().String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("fake service must bind 127.0.0.1, got %s", addr)
	}
	return addr
}

func t51BannerOnConnect(t *testing.T, banner []byte) string {
	t.Helper()
	return t51Listen(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write(banner)
		time.Sleep(50 * time.Millisecond)
	})
}

func t51ReplyAfterRead(t *testing.T, wantMin int, reply []byte) string {
	t.Helper()
	return t51Listen(t, func(c net.Conn) {
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return
		}
		if n < wantMin {
			return
		}
		_, _ = c.Write(reply)
	})
}

// t51Dialer 把逻辑端口转到 127.0.0.1 上的假服务，不解析主机名、不触外网。
func t51Dialer(dials *atomic.Int32, routes map[int]string) serviceDialer {
	return func(ctx context.Context, network, host string, port int) (net.Conn, error) {
		dials.Add(1)
		if net.ParseIP(host) != nil {
			return nil, fmt.Errorf("dialer must not receive an IP host: %s", host)
		}
		if host == "127.0.0.1" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return nil, fmt.Errorf("dialer must be given a hostname, got %s", host)
		}
		addr, ok := routes[port]
		if !ok {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			return nil, fmt.Errorf("route is not loopback: %s", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func TestT51ProbeByteSet(t *testing.T) {
	probes := serviceFingerprintProbes()
	if len(probes) != 2 {
		t.Fatalf("complete probe set size = %d, want 2 (PostgreSQL SSLRequest + AMQP preamble only)", len(probes))
	}
	wantPG := []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}
	wantAMQP := []byte{'A', 'M', 'Q', 'P', 0x00, 0x00, 0x09, 0x01}
	var hasPG, hasAMQP bool
	for _, p := range probes {
		if bytes.Equal(p, wantPG) {
			hasPG = true
		} else if bytes.Equal(p, wantAMQP) {
			hasAMQP = true
		} else {
			t.Fatalf("unexpected probe bytes %q", p)
		}
	}
	if !hasPG || !hasAMQP {
		t.Fatalf("probe set missing SSLRequest or AMQP preamble: pg=%v amqp=%v", hasPG, hasAMQP)
	}
	forbidden := []string{
		"AUTH", "LOGIN", "PASSWORD", "INFO", "PING",
		"isMaster", "ismaster", "hello", "helloOk",
		"GET /", "GET /_", "POST ", "STATS", "stats\r\n", "version\r\n",
		"SELECT", "INSERT", "UPDATE", "DELETE", "EVAL", "KEYS", "CONFIG",
		"FLUSH", "QUIT", "STARTUP", "User=", "password",
	}
	for _, p := range probes {
		s := string(p)
		upper := strings.ToUpper(s)
		for _, f := range forbidden {
			if strings.Contains(s, f) || strings.Contains(upper, strings.ToUpper(f)) {
				t.Errorf("probe %q contains forbidden %q", p, f)
			}
		}
	}
	// 默认端口恰好覆盖 SPEC 点名的 10 种服务，不是全端口。
	ports := defaultServiceScanPorts
	if len(ports) != 10 {
		t.Fatalf("default ports = %v, want 10 named services", ports)
	}
	wantPorts := []int{3306, 5432, 6379, 27017, 9200, 9092, 2379, 8848, 5672, 11211}
	if len(ports) != len(wantPorts) {
		t.Fatalf("default ports %v", ports)
	}
	for i, p := range wantPorts {
		if ports[i] != p {
			t.Fatalf("default ports = %v, want %v", ports, wantPorts)
		}
	}
	// 规则表每条默认端口都有对应行，便于 T53 整表替换。
	covered := map[int]string{}
	for _, r := range builtinServiceFingerprintRules() {
		for _, p := range r.Ports {
			covered[p] = r.Name
		}
	}
	for _, p := range wantPorts {
		if covered[p] == "" {
			t.Errorf("default port %d has no fingerprint rule", p)
		}
	}
}

func TestT51ConfigFromEnv(t *testing.T) {
	t.Setenv("SERVICE_SCAN_PORTS", "")
	t.Setenv("SERVICE_SCAN_RPS", "")
	t.Setenv("SERVICE_SCAN_TIMEOUT", "")
	cfg, err := serviceFingerprintConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ports) != 10 || cfg.RPS != 2 || cfg.Timeout != 2*time.Second {
		t.Fatalf("defaults: ports=%v rps=%v timeout=%v", cfg.Ports, cfg.RPS, cfg.Timeout)
	}
	t.Setenv("SERVICE_SCAN_PORTS", "3306,9999")
	t.Setenv("SERVICE_SCAN_RPS", "1.5")
	t.Setenv("SERVICE_SCAN_TIMEOUT", "250ms")
	cfg, err = serviceFingerprintConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ports) != 2 || cfg.Ports[0] != 3306 || cfg.Ports[1] != 9999 || cfg.RPS != 1.5 || cfg.Timeout != 250*time.Millisecond {
		t.Fatalf("override: %+v", cfg)
	}
	t.Setenv("SERVICE_SCAN_PORTS", "1-65535")
	if _, err := serviceFingerprintConfigFromEnv(); err == nil {
		t.Fatal("port range must be rejected")
	}
}

func TestT51DefaultOffNoDial(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	host := "db." + domain
	id := t51InsertAsset(t, e, host, false)
	var dials atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306, 5432},
		RPS:     100,
		Timeout: time.Second,
		Dialer:  t51Dialer(&dials, map[int]string{3306: t51BannerOnConnect(t, []byte("mysql_native_password"))}),
	}
	if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: id, Host: host}}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("default-off must not dial, dials=%d", dials.Load())
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_services`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("default-off must not store results, got %d", n)
	}
}

func TestT51OpenPortIdentifyAndClosed(t *testing.T) {
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	ctx := context.Background()

	origin := "origin." + domain
	cdnHost := "cdn." + domain
	outHost := "other.out-of-scope.example.test"
	oid := t51InsertAsset(t, e, origin, false)
	cid := t51InsertAsset(t, e, cdnHost, true)
	xid := t51InsertAsset(t, e, outHost, false)

	mysqlBanner := []byte{0x0a}
	mysqlBanner = append(mysqlBanner, []byte("8.0.36")...)
	mysqlBanner = append(mysqlBanner, 0)
	mysqlBanner = append(mysqlBanner, []byte("mysql_native_password")...)

	routes := map[int]string{
		3306:  t51BannerOnConnect(t, mysqlBanner),
		5432:  t51ReplyAfterRead(t, 8, []byte("N")),
		6379:  t51BannerOnConnect(t, []byte("-NOAUTH Authentication required.\r\n")),
		27017: t51BannerOnConnect(t, []byte("MongoDB isMaster disabled in test banner")),
		9200:  t51BannerOnConnect(t, []byte("elasticsearch 8.12.0 mock banner")),
		9092:  t51BannerOnConnect(t, []byte("kafka_2.13 mock banner")),
		2379:  t51BannerOnConnect(t, []byte("etcd 3.5 mock banner")),
		8848:  t51BannerOnConnect(t, []byte("nacos mock banner")),
		5672:  t51ReplyAfterRead(t, 4, []byte("AMQP\x00\x00\x09\x01rabbitmq")),
		11211: t51BannerOnConnect(t, []byte("ERROR unauthorized memcached mock\r\n")),
		// 6378 deliberately missing → closed
	}

	var dials atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   append(append([]int{}, defaultServiceScanPorts...), 19999),
		RPS:     100,
		Timeout: time.Second,
		Dialer:  t51Dialer(&dials, routes),
	}

	var beforeComp int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM components`).Scan(&beforeComp); err != nil {
		t.Fatal(err)
	}

	targets := []serviceScanTarget{
		{AssetID: oid, Host: origin},
		{AssetID: cid, Host: cdnHost},
		{AssetID: xid, Host: outHost},
	}
	if err := fingerprintOriginServices(ctx, e.pool, cfg, targets); err != nil {
		t.Fatal(err)
	}

	// 仅源站被拨号：11 个端口各 1 次，CDN / 超范围 0 次，不重试。
	if got := dials.Load(); got != 11 {
		t.Fatalf("dials=%d, want 11 (one per origin port, no retry, no CDN/out-of-scope)", got)
	}

	want := map[int]struct {
		open            bool
		name            string
		versionContains string
	}{
		3306:  {true, "mysql", "8.0.36"},
		5432:  {true, "postgresql", ""},
		6379:  {true, "redis", ""},
		27017: {true, "mongodb", ""},
		9200:  {true, "elasticsearch", "8.12.0"},
		9092:  {true, "kafka", ""},
		2379:  {true, "etcd", "3.5"},
		8848:  {true, "nacos", ""},
		5672:  {true, "rabbitmq", ""},
		11211: {true, "memcached", ""},
		19999: {false, "", ""},
	}
	rows, err := e.pool.Query(ctx, `SELECT host, port, open, name, version, banner FROM asset_services ORDER BY port`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int]struct {
		host, name, version, banner string
		open                        bool
	}{}
	for rows.Next() {
		var host, name, version, banner string
		var port int
		var open bool
		if err := rows.Scan(&host, &port, &open, &name, &version, &banner); err != nil {
			t.Fatal(err)
		}
		if host != origin {
			t.Fatalf("stored host %q, want hostname %q (no IP)", host, origin)
		}
		if net.ParseIP(host) != nil {
			t.Fatalf("stored host looks like IP: %q", host)
		}
		if looksLikeIP(banner) {
			t.Fatalf("banner stored an IP: %q", banner)
		}
		got[port] = struct {
			host, name, version, banner string
			open                        bool
		}{host, name, version, banner, open}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("stored %d ports, want %d: %+v", len(got), len(want), got)
	}
	for p, w := range want {
		g, ok := got[p]
		if !ok {
			t.Errorf("missing port %d", p)
			continue
		}
		if g.open != w.open || g.name != w.name {
			t.Errorf("port %d open=%v name=%q, want open=%v name=%q (banner=%q)", p, g.open, g.name, w.open, w.name, g.banner)
		}
		if w.versionContains != "" && !strings.Contains(g.version, w.versionContains) {
			t.Errorf("port %d version=%q, want to contain %q", p, g.version, w.versionContains)
		}
	}

	var afterComp int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM components`).Scan(&afterComp); err != nil {
		t.Fatal(err)
	}
	if afterComp != beforeComp {
		t.Fatalf("T51 must not write components inventory, before=%d after=%d", beforeComp, afterComp)
	}

	var cols int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'asset_services' AND column_name ILIKE '%ip%'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatalf("asset_services must not have an IP column, found %d", cols)
	}

	// 拨号超时：记端口关闭，不重试。已建立连接但无 banner 仍视为开放（上面 19999 是拒绝连接）。
	dials.Store(0)
	timeoutHost := "timeout." + domain
	tid := t51InsertAsset(t, e, timeoutHost, false)
	cfg.Ports = []int{3306}
	cfg.Timeout = 150 * time.Millisecond
	cfg.Dialer = func(ctx context.Context, network, host string, port int) (net.Conn, error) {
		dials.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: tid, Host: timeoutHost}}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("timeout must not retry, dials=%d", dials.Load())
	}
	var open bool
	if err := e.pool.QueryRow(ctx, `SELECT open FROM asset_services WHERE host = $1 AND port = 3306`, timeoutHost).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open {
		t.Fatal("dial timeout must be recorded as closed")
	}
}

func TestT51RejectsIPShorthandHosts(t *testing.T) {
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	ctx := context.Background()
	origin := "ok." + domain
	oid := t51InsertAsset(t, e, origin, false)
	var dials atomic.Int32
	seen := map[string]int{}
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306},
		RPS:     100,
		Timeout: time.Second,
		Dialer: func(ctx context.Context, network, host string, port int) (net.Conn, error) {
			dials.Add(1)
			seen[host]++
			if rejectedServiceScanHost(host) || net.ParseIP(host) != nil {
				t.Errorf("dialer must not be invoked for rejected host %q", host)
			}
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	targets := []serviceScanTarget{
		{AssetID: oid, Host: origin},
		{Host: "127.1"},
		{Host: "10.1"},
		{Host: "0177.0.0.1"},
		{Host: "0x7f.0.0.1"},
		{Host: "1.2.3.4.5"},
	}
	if err := fingerprintOriginServices(ctx, e.pool, cfg, targets); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 || seen[origin] != 1 {
		t.Fatalf("only the legal origin may be dialed, dials=%d seen=%v", dials.Load(), seen)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM asset_services WHERE host IN ('127.1','10.1','0177.0.0.1','0x7f.0.0.1','1.2.3.4.5')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("must not store IP-like hosts, rows=%d", n)
	}
}

func TestT51RateLimit(t *testing.T) {
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	ctx := context.Background()
	host := "rate." + domain
	id := t51InsertAsset(t, e, host, false)
	var dials atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   []int{1, 2, 3, 4, 5},
		RPS:     2,
		Timeout: 50 * time.Millisecond,
		Dialer:  t51Dialer(&dials, map[int]string{}), // all refused = closed
	}
	start := time.Now()
	if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: id, Host: host}}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if dials.Load() != 5 {
		t.Fatalf("dials=%d, want 5", dials.Load())
	}
	// 5 次 @2/s、burst 1，至少约 2s。
	if elapsed < 2*time.Second {
		t.Fatalf("5 probes @2/s finished in %v, rate limit not applied", elapsed)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("rate-limited scan took too long: %v", elapsed)
	}
}

func looksLikeIP(s string) bool {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == ',' || r == '/' || r == '\n' || r == '\r' || r == '\t'
	}) {
		if net.ParseIP(strings.Trim(f, "[]")) != nil {
			return true
		}
	}
	// dotted IPv4 even if surrounded by other chars
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		// try parse from here
		j := i
		for j < len(s) && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
			j++
		}
		if net.ParseIP(s[i:j]) != nil {
			return true
		}
	}
	return false
}
