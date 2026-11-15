package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func t51Handshake(version, plugin string) []byte {
	p := []byte{0x0a}
	p = append(p, version...)
	p = append(p, 0)
	p = append(p, 1, 0, 0, 0)
	p = append(p, "abcdefgh"...)
	p = append(p, 0, 0xff, 0xff, 0xff, 0x02, 0x00, 0xff, 0xdf, 21)
	p = append(p, make([]byte, 10)...)
	p = append(p, "ijklmnopqrst"...)
	p = append(p, 0)
	p = append(p, plugin...)
	p = append(p, 0)
	h := []byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), 0}
	return append(h, p...)
}

func t51MySQLDenied(ip string) []byte {
	p := []byte{0xff, 0x6a, 0x04}
	p = append(p, "Host '"+ip+"' is not allowed to connect to this MySQL server"...)
	h := []byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), 0}
	return append(h, p...)
}

func t51AMQPStart() []byte {
	tbl := &bytes.Buffer{}
	ent := func(k, v string) {
		tbl.WriteByte(byte(len(k)))
		tbl.WriteString(k)
		tbl.WriteByte('S')
		binary.Write(tbl, binary.BigEndian, uint32(len(v)))
		tbl.WriteString(v)
	}
	ent("product", "RabbitMQ")
	ent("version", "3.13.7")
	pl := &bytes.Buffer{}
	pl.Write([]byte{0, 10, 0, 10, 0, 9})
	binary.Write(pl, binary.BigEndian, uint32(tbl.Len()))
	pl.Write(tbl.Bytes())
	for _, s := range []string{"PLAIN AMQPLAIN", "en_US"} {
		binary.Write(pl, binary.BigEndian, uint32(len(s)))
		pl.WriteString(s)
	}
	fr := &bytes.Buffer{}
	fr.Write([]byte{1, 0, 0})
	binary.Write(fr, binary.BigEndian, uint32(pl.Len()))
	fr.Write(pl.Bytes())
	fr.WriteByte(0xCE)
	return fr.Bytes()
}

func TestT51SanitizeBannerRework(t *testing.T) {
	if s := sanitizeServiceBanner(bytes.Repeat([]byte("A"), 600)); len(s) > 512 {
		t.Fatalf("banner not truncated: len=%d", len(s))
	}
	// 先脱敏再截断：IP 跨 512 边界不得残留半截。
	edge := append(bytes.Repeat([]byte("x"), 505), []byte("10.20.30.40")...)
	out := sanitizeServiceBanner(edge)
	if len(out) > 512 {
		t.Fatalf("truncate after redact: len=%d", len(out))
	}
	if strings.Contains(out, "10.") || strings.Contains(out, "20.30") || strings.Contains(out, "30.40") {
		t.Fatalf("IP remnant after boundary truncate: %q", out[max(0, len(out)-20):])
	}
	if !strings.Contains(out, "[ip]") {
		t.Fatalf("expected [ip] after redact+truncate, got tail %q", out[max(0, len(out)-16):])
	}

	cases := []struct{ in, leak, keep string }{
		{"Host '2001:db8::5' is not allowed", "2001:db8::5", "[ip]"},
		{"::1 denied", "::1", "[ip]"},
		{"peer fe80::1%eth0 x", "fe80::1", "[ip]"},
		{"[2001:db8::7]:5432", "2001:db8::7", "[ip]"},
		{"mapped ::ffff:192.0.2.7 x", "::ffff:", "[ip]"},
		{"mapped ::ffff:192.0.2.7 x", "192.0.2.7", "[ip]"},
		{"Host 10.20.30.40 denied", "10.20.30.40", "[ip]"},
		{"int 2130706433 x", "2130706433", "[ip]"},
		{"hex 0x7f000001 x", "0x7f000001", "[ip]"},
		{"octal 0177.0.0.1 x", "0177.0.0.1", "[ip]"},
		{"loop 127.1 x", "127.1", "[ip]"},
		{"version 8.0.36 keep", "REPLACE_NONE", "8.0.36"},
		{"version 10.11.6 keep", "REPLACE_NONE", "10.11.6"},
		{"version 5.5.5 keep", "REPLACE_NONE", "5.5.5"},
	}
	for _, c := range cases {
		got := sanitizeServiceBanner([]byte(c.in))
		if c.leak == "REPLACE_NONE" {
			if got != c.in && !strings.Contains(got, c.keep) {
				t.Errorf("version injured in=%q out=%q", c.in, got)
			}
			if strings.Contains(got, "[ip]") {
				t.Errorf("version replaced in=%q out=%q", c.in, got)
			}
			continue
		}
		if strings.Contains(got, c.leak) {
			t.Errorf("LEAK in=%q out=%q still has %q", c.in, got, c.leak)
		}
		if c.keep != "" && !strings.Contains(got, c.keep) {
			t.Errorf("expected %q in out=%q (in=%q)", c.keep, got, c.in)
		}
	}

	// 端到端：假 MySQL 1130 包里的 IPv6 不得落库。
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "deny." + domain
	id := t51InsertAsset(t, e, host, false)
	var dials atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306},
		RPS:     100,
		Timeout: time.Second,
		Dialer:  t51Dialer(&dials, map[int]string{3306: t51BannerOnConnect(t, t51MySQLDenied("2001:db8::5"))}),
	}
	if err := fingerprintOriginServices(context.Background(), e.pool, cfg, []serviceScanTarget{{AssetID: id, Host: host}}); err != nil {
		t.Fatal(err)
	}
	var banner string
	if err := e.pool.QueryRow(context.Background(), `SELECT banner FROM asset_services WHERE host=$1 AND port=3306`, host).Scan(&banner); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(banner, "2001:db8::5") || strings.Contains(banner, "db8") {
		t.Fatalf("IPv6 leaked into asset_services.banner: %q", banner)
	}
	if !strings.Contains(banner, "[ip]") {
		t.Fatalf("expected [ip] in stored banner, got %q", banner)
	}
}

func TestT51MySQL8AndMariaDBHandshake(t *testing.T) {
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	ctx := context.Background()
	h8 := "my8." + domain
	hm := "maria." + domain
	id8 := t51InsertAsset(t, e, h8, false)
	idm := t51InsertAsset(t, e, hm, false)
	var d1, d2 atomic.Int32
	cfg8 := serviceFingerprintConfig{Ports: []int{3306}, RPS: 100, Timeout: time.Second,
		Dialer: t51Dialer(&d1, map[int]string{3306: t51BannerOnConnect(t, t51Handshake("8.0.36", "caching_sha2_password"))})}
	cfgM := serviceFingerprintConfig{Ports: []int{3306}, RPS: 100, Timeout: time.Second,
		Dialer: t51Dialer(&d2, map[int]string{3306: t51BannerOnConnect(t, t51Handshake("5.5.5-10.11.6-MariaDB-0+deb12u1", "mysql_native_password"))})}
	if err := fingerprintOriginServices(ctx, e.pool, cfg8, []serviceScanTarget{{AssetID: id8, Host: h8}}); err != nil {
		t.Fatal(err)
	}
	if err := fingerprintOriginServices(ctx, e.pool, cfgM, []serviceScanTarget{{AssetID: idm, Host: hm}}); err != nil {
		t.Fatal(err)
	}
	var n8, v8, nm, vm string
	e.pool.QueryRow(ctx, `SELECT name, version FROM asset_services WHERE host=$1 AND port=3306`, h8).Scan(&n8, &v8)
	e.pool.QueryRow(ctx, `SELECT name, version FROM asset_services WHERE host=$1 AND port=3306`, hm).Scan(&nm, &vm)
	if n8 != "mysql" || v8 != "8.0.36" {
		t.Fatalf("mysql8 name=%q version=%q, want mysql/8.0.36", n8, v8)
	}
	if nm != "mysql" || vm != "10.11.6" {
		t.Fatalf("mariadb name=%q version=%q, want mysql/10.11.6", nm, vm)
	}
}

type t51ByteRec struct {
	mu  sync.Mutex
	got map[string][]byte
	wg  sync.WaitGroup
}

func newT51ByteRec() *t51ByteRec { return &t51ByteRec{got: map[string][]byte{}} }

func (r *t51ByteRec) fake(t *testing.T, name string, behave func(c net.Conn, readN func(int) []byte)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.wg.Add(1)
			go func(c net.Conn) {
				defer r.wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				readN := func(n int) []byte {
					buf := make([]byte, 0, n)
					tmp := make([]byte, 4096)
					for len(buf) < n {
						k, err := c.Read(tmp)
						if k > 0 {
							buf = append(buf, tmp[:k]...)
							r.mu.Lock()
							r.got[name] = append(r.got[name], tmp[:k]...)
							r.mu.Unlock()
						}
						if err != nil {
							break
						}
					}
					return buf
				}
				behave(c, readN)
				readN(1 << 20)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func (r *t51ByteRec) bytesOf(name string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.got[name]...)
}

// TestT51RealisticProbeBytes 覆盖 M1b/M2c/M3b：假服务记录全部收到的字节。
// Redis 不主动发 banner，T53 起发送 YAML 声明的 PING；AMQP 只在协议头后回 Start；PG 只回 S/N，之后不得再收到 StartupMessage。
func TestT51RealisticProbeBytes(t *testing.T) {
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	ctx := context.Background()
	rec := newT51ByteRec()
	hPG, hR, hA := "pg."+domain, "redis."+domain, "amqp."+domain
	routes := map[int]string{
		5432: rec.fake(t, "pg", func(c net.Conn, readN func(int) []byte) {
			if len(readN(8)) == 8 {
				c.Write([]byte("N"))
			}
		}),
		6379: rec.fake(t, "redis", func(c net.Conn, readN func(int) []byte) {
			if b := readN(1); len(b) > 0 {
				c.Write([]byte("+PONG\r\n"))
			}
		}),
		5672: rec.fake(t, "amqp", func(c net.Conn, readN func(int) []byte) {
			if len(readN(8)) == 8 {
				c.Write(t51AMQPStart())
			}
		}),
	}
	var dials atomic.Int32
	cfg := serviceFingerprintConfig{Ports: []int{5432, 6379, 5672}, RPS: 100, Timeout: 400 * time.Millisecond, Dialer: t51Dialer(&dials, routes)}
	targets := []serviceScanTarget{
		{AssetID: t51InsertAsset(t, e, hPG, false), Host: hPG},
		{AssetID: t51InsertAsset(t, e, hR, false), Host: hR},
		{AssetID: t51InsertAsset(t, e, hA, false), Host: hA},
	}
	// 每个 target 只用自己的端口：t51Dialer 按端口路由，三台主机会共享端口路由。
	// 分三次扫描，避免串台。
	for i, port := range []int{5432, 6379, 5672} {
		one := cfg
		one.Ports = []int{port}
		one.Dialer = t51Dialer(&dials, routes)
		if err := fingerprintOriginServices(ctx, e.pool, one, []serviceScanTarget{targets[i]}); err != nil {
			t.Fatal(err)
		}
	}
	rec.wg.Wait()
	pgWant := []byte{0, 0, 0, 8, 4, 0xd2, 0x16, 0x2f}
	amqpWant := []byte{'A', 'M', 'Q', 'P', 0, 0, 9, 1}
	redisWant := []byte("PING\r\n")
	if !bytes.Equal(rec.bytesOf("pg"), pgWant) {
		t.Fatalf("PG received %x want SSLRequest only (M1b)", rec.bytesOf("pg"))
	}
	if !bytes.Equal(rec.bytesOf("redis"), redisWant) {
		t.Fatalf("Redis received %q want declared PING only (T53)", rec.bytesOf("redis"))
	}
	if !bytes.Equal(rec.bytesOf("amqp"), amqpWant) {
		t.Fatalf("AMQP received %x want protocol header only (M3b)", rec.bytesOf("amqp"))
	}
}

func TestT51SharedLimiterConcurrentTasks(t *testing.T) {
	resetSharedServiceLimiter()
	t.Cleanup(resetSharedServiceLimiter)
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	ctx := context.Background()
	h1, h2 := "r1."+domain, "r2."+domain
	id1, id2 := t51InsertAsset(t, e, h1, false), t51InsertAsset(t, e, h2, false)
	var times []time.Time
	var mu sync.Mutex
	dial := func(ctx context.Context, network, host string, port int) (net.Conn, error) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}
	}
	run := func(host string, id int64) error {
		cfg := serviceFingerprintConfig{Ports: []int{1, 2, 3}, RPS: 2, Timeout: 50 * time.Millisecond, Dialer: dial}
		return fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: id, Host: host}})
	}
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	var err1, err2 error
	go func() { defer wg.Done(); err1 = run(h1, id1) }()
	go func() { defer wg.Done(); err2 = run(h2, id2) }()
	wg.Wait()
	if err1 != nil || err2 != nil {
		t.Fatalf("scan err %v %v", err1, err2)
	}
	el := time.Since(start)
	// 6 次拨号共享 2 RPS burst 1，至少约 2.5s；若每任务一个限速器会叠加快约一倍。
	if len(times) != 6 {
		t.Fatalf("dials=%d want 6", len(times))
	}
	minNeed := 5 * 450 * time.Millisecond
	if el < minNeed {
		t.Fatalf("concurrent tasks stacked rate limiters: elapsed=%v want >= %v (M5b)", el, minNeed)
	}
}

func TestT51DisabledPlanRecordsNoSkips(t *testing.T) {
	e := newTestEnv(t)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	host := "x." + domain
	t51InsertAsset(t, e, host, false)
	q, err := planOriginServiceScan(ctx, e.pool, []serviceScanTarget{{Host: host}})
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 0 {
		t.Fatalf("disabled must not enqueue, got %d", len(q))
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM service_scan_skips`).Scan(&n)
	if n != 0 {
		t.Fatalf("disabled must not record skips (M7b), got %d", n)
	}
}

func TestT51ScanHostRejectsIPLikeHosts(t *testing.T) {
	e := newTestEnv(t)
	var dials atomic.Int32
	cfg := serviceFingerprintConfig{Ports: []int{3306}, RPS: 100, Timeout: 100 * time.Millisecond, Dialer: t51Dialer(&dials, map[int]string{})}
	cfg.ready()
	ctx := context.Background()
	for _, h := range []string{"127.1", "1.0x7f", "0x7f000001", "2130706433", "0177.0.0.1", "127.0.0.1", "::1"} {
		if err := scanHostServices(ctx, e.pool, cfg, serviceScanTarget{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != 0 {
		t.Fatalf("IP-like hosts dialed (M8), dials=%d", dials.Load())
	}
}

func TestT51UpsertUniqueAndRescan(t *testing.T) {
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	ctx := context.Background()
	host := "up." + domain
	id := t51InsertAsset(t, e, host, false)
	var n atomic.Int32
	addr := t51Listen(t, func(c net.Conn) {
		defer c.Close()
		k := n.Add(1)
		c.Write(t51Handshake(fmt.Sprintf("5.7.%d", 40+k), "mysql_native_password"))
		time.Sleep(30 * time.Millisecond)
	})
	var dials atomic.Int32
	cfg := serviceFingerprintConfig{Ports: []int{3306}, RPS: 100, Timeout: 300 * time.Millisecond, Dialer: t51Dialer(&dials, map[int]string{3306: addr})}
	for i := 0; i < 3; i++ {
		if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: id, Host: host}}); err != nil {
			t.Fatal(err)
		}
	}
	var cnt int
	var ver string
	e.pool.QueryRow(ctx, `SELECT count(*), max(version) FROM asset_services WHERE host=$1 AND port=3306`, host).Scan(&cnt, &ver)
	if cnt != 1 || ver != "5.7.43" {
		t.Fatalf("upsert 3 scans rows=%d version=%q (M16a/M16b)", cnt, ver)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (host, port, open) VALUES ($1, 3306, true)`, host); err == nil {
		t.Fatal("UNIQUE (host,port) missing (M16a)")
	}
	sql, err := os.ReadFile("migrations/027_asset_services.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("027 rerun: %v", err)
	}
}

func TestT51ReadDeadlineAndDefaultDialer(t *testing.T) {
	// M19：读必须设 deadline。假服务接受但不写，扫描应在超时内返回。
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "hang." + domain
	id := t51InsertAsset(t, e, host, false)
	addr := t51Listen(t, func(c net.Conn) {
		defer c.Close()
		time.Sleep(5 * time.Second)
	})
	var dials atomic.Int32
	cfg := serviceFingerprintConfig{Ports: []int{3306}, RPS: 100, Timeout: 200 * time.Millisecond, Dialer: t51Dialer(&dials, map[int]string{3306: addr})}
	st := time.Now()
	if err := fingerprintOriginServices(context.Background(), e.pool, cfg, []serviceScanTarget{{AssetID: id, Host: host}}); err != nil {
		t.Fatal(err)
	}
	if time.Since(st) > 2*time.Second {
		t.Fatalf("read without deadline hung (M19): %v", time.Since(st))
	}

	// M20：默认拨号器带 Timeout，且拒绝主机名（不再自己解析 DNS）。
	d := defaultServiceDialer(200 * time.Millisecond)
	if _, err := d(context.Background(), "tcp", "localhost", 1); err == nil {
		t.Fatal("default dialer must refuse hostname and not resolve DNS")
	}
	st = time.Now()
	_, err := d(context.Background(), "tcp", "192.0.2.1", 1) // TEST-NET，不可达，应超时或立即失败，但不得做 DNS
	_ = err
	if time.Since(st) > 3*time.Second {
		t.Fatalf("default dialer timeout not applied (M20): %v", time.Since(st))
	}
}

func TestT51UnjudgedCDNSkipped(t *testing.T) {
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	ctx := context.Background()
	unk := "unk." + domain
	origin := "origin." + domain
	if _, err := e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain, sources, resolved) VALUES ($1, $2, '{dns}', true)`, e.rootID, unk); err != nil {
		t.Fatal(err)
	}
	oid := t51InsertAsset(t, e, origin, false)
	var unkID int64
	e.pool.QueryRow(ctx, `SELECT id FROM assets WHERE subdomain=$1`, unk).Scan(&unkID)
	var fronted *bool
	e.pool.QueryRow(ctx, `SELECT cdn_fronted FROM assets WHERE id=$1`, unkID).Scan(&fronted)
	if fronted != nil {
		t.Fatalf("unjudged asset should have NULL cdn_fronted, got %v", fronted)
	}

	var dials atomic.Int32
	cfg := serviceFingerprintConfig{Ports: []int{3306}, RPS: 100, Timeout: 100 * time.Millisecond, Dialer: t51Dialer(&dials, map[int]string{})}
	if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{
		{AssetID: unkID, Host: unk},
		{AssetID: oid, Host: origin},
	}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("only judged origin may be dialed, dials=%d", dials.Load())
	}
	var reason string
	if err := e.pool.QueryRow(ctx, `SELECT reason FROM service_scan_skips WHERE host=$1`, unk).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != skipServiceScanCDNUnknown {
		t.Fatalf("unjudged skip reason=%q want %s", reason, skipServiceScanCDNUnknown)
	}
}

func TestT51DefaultPolicyRejectsPrivateIPs(t *testing.T) {
	samples := []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1", "fe80::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "0.0.0.0", "::"}
	for _, s := range samples {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("parse %q", s)
		}
		if denyDialIP(ip) != skipPrivateAddress {
			t.Errorf("denyDialIP(%s) = %q, want %s", s, denyDialIP(ip), skipPrivateAddress)
		}
	}
	if denyDialIP(net.ParseIP("192.0.2.10")) != "" {
		t.Fatalf("TEST-NET should be allowed for policy unit test, got %q", denyDialIP(net.ParseIP("192.0.2.10")))
	}

	// 生产路径（无注入 Dialer）：Resolve 给出回环/内网时不得拨号，并记 skip。
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "priv." + domain
	id := t51InsertAsset(t, e, host, false)
	var dialed atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306},
		RPS:     100,
		Timeout: 100 * time.Millisecond,
		Resolve: func(ctx context.Context, h string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1"), net.ParseIP("169.254.169.254"), net.ParseIP("::1"), net.ParseIP("fe80::1")}, nil
		},
		DialIP: func(ctx context.Context, ip net.IP, port int) (net.Conn, error) {
			dialed.Add(1)
			t.Errorf("production path dialed blocked address")
			return nil, errors.New("should not dial")
		},
	}
	if err := fingerprintOriginServices(context.Background(), e.pool, cfg, []serviceScanTarget{{AssetID: id, Host: host}}); err != nil {
		t.Fatal(err)
	}
	if dialed.Load() != 0 {
		t.Fatalf("blocked addresses were dialed: %d", dialed.Load())
	}
	var reason string
	if err := e.pool.QueryRow(context.Background(), `SELECT reason FROM service_scan_skips WHERE host=$1`, host).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != skipPrivateAddress {
		t.Fatalf("skip reason=%q want %s", reason, skipPrivateAddress)
	}
}

func TestT51OnlyDialsResolvedIPs(t *testing.T) {
	e := newTestEnv(t)
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "src." + domain
	id := t51InsertAsset(t, e, host, false)
	want := net.ParseIP("192.0.2.10")
	var seen string
	var n atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   []int{3306},
		RPS:     100,
		Timeout: 100 * time.Millisecond,
		Resolve: func(ctx context.Context, h string) ([]net.IP, error) {
			if h != host {
				t.Errorf("resolve host mismatch")
			}
			return []net.IP{want}, nil
		},
		DialIP: func(ctx context.Context, ip net.IP, port int) (net.Conn, error) {
			n.Add(1)
			seen = ip.String()
			return nil, errors.New("refused")
		},
	}
	if err := fingerprintOriginServices(context.Background(), e.pool, cfg, []serviceScanTarget{{AssetID: id, Host: host}}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 || seen != want.String() {
		t.Fatalf("dialed %q count=%d, want only resolved TEST-NET IP", seen, n.Load())
	}
}

func TestT51ProbeByteSetStillOnlyReadOnly(t *testing.T) {
	// T53：探测集来自 YAML 声明，仍是只读识别报文（PG SSLRequest + AMQP 头保留）。
	rules, err := loadServiceFingerprintRules("")
	if err != nil {
		t.Fatal(err)
	}
	probes := rules.probes()
	if len(probes) < 2 {
		t.Fatalf("probe set size=%d, want at least PG + AMQP", len(probes))
	}
	wantPG := []byte{0, 0, 0, 8, 4, 0xd2, 0x16, 0x2f}
	wantAMQP := []byte{'A', 'M', 'Q', 'P', 0, 0, 9, 1}
	var hasPG, hasAMQP bool
	for _, p := range probes {
		if bytes.Equal(p, wantPG) {
			hasPG = true
		}
		if bytes.Equal(p, wantAMQP) {
			hasAMQP = true
		}
		s := strings.ToUpper(string(p))
		for _, f := range []string{"AUTH", "LOGIN", "PASSWORD", "SELECT", "INSERT", "UPDATE", "DELETE", "FLUSH", "CONFIG"} {
			if strings.Contains(s, f) {
				t.Errorf("probe contains %q", f)
			}
		}
	}
	if !hasPG || !hasAMQP {
		t.Fatalf("probe set missing SSLRequest or AMQP preamble: pg=%v amqp=%v", hasPG, hasAMQP)
	}
	_ = rate.Limit(1)
}
