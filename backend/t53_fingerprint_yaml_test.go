package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func t53WriteRules(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "services.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func t53MustLoad(t *testing.T, path string) *serviceFingerprintRuleSet {
	t.Helper()
	rules, err := loadServiceFingerprintRules(path)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestT53BuiltinRulesLoadAndKeepMySQLHandshake(t *testing.T) {
	rules := t53MustLoad(t, "")
	for _, name := range []string{"mysql", "postgresql", "redis", "mongodb", "elasticsearch", "kafka", "etcd", "nacos", "rabbitmq", "memcached"} {
		r, ok := rules.ruleByName(name)
		if !ok {
			t.Fatalf("missing builtin rule %q", name)
		}
		if r.Component == "" || r.Category == "" {
			t.Errorf("rule %q component=%q category=%q", name, r.Component, r.Category)
		}
		switch r.Confidence {
		case serviceConfidenceHigh, serviceConfidenceMedium, serviceConfidenceLow:
		default:
			t.Errorf("rule %q invalid confidence %q", name, r.Confidence)
		}
		if len(r.Ports) == 0 {
			t.Errorf("rule %q has no ports", name)
		}
		for _, p := range r.Ports {
			if p < 1 || p > 65535 {
				t.Errorf("rule %q port %d out of range", name, p)
			}
		}
	}
	for _, name := range []string{"postgresql", "redis", "mongodb", "elasticsearch", "kafka", "etcd", "nacos", "rabbitmq", "memcached"} {
		r, ok := rules.ruleByName(name)
		if !ok || len(r.Probes) == 0 {
			t.Errorf("rule %q must declare at least one read-only probe", name)
		}
	}
	if got := rules.ports(); len(got) != 10 {
		t.Fatalf("default ports = %v, want 10", got)
	}
	// T53 不得回退 MySQL/MariaDB 握手版本行为。
	if n, v, c := rules.match(3306, t51Handshake("8.0.36", "caching_sha2_password")); n != "mysql" || v != "8.0.36" || c != serviceConfidenceHigh {
		t.Fatalf("mysql8 match name=%q version=%q confidence=%q", n, v, c)
	}
	if n, v, _ := rules.match(3306, t51Handshake("5.5.5-10.11.6-MariaDB-0+deb12u1", "mysql_native_password")); n != "mysql" || v != "10.11.6" {
		t.Fatalf("mariadb match name=%q version=%q, want mysql/10.11.6", n, v)
	}
	// 历史 banner 已脱敏时同样取 MariaDB 真版本。
	if _, v, _ := rules.match(3306, []byte(".5.5.5-10.11.6-MariaDB-0+deb12u1.abc")); v != "10.11.6" {
		t.Fatalf("sanitized mariadb version=%q, want 10.11.6", v)
	}
	// 只读探测的真实响应也能识别（不是只认测试里的服务名文本）。
	realistic := []struct {
		port    int
		banner  []byte
		name    string
		version string
	}{
		{5432, []byte("N"), "postgresql", ""},
		{6379, []byte("+PONG\r\n"), "redis", ""},
		{27017, []byte("\x00\x00\x00\x00helloOk"), "mongodb", ""},
		{9200, []byte(`{"cluster_name":"docker-cluster","version":{"number":"8.12.0"}}`), "elasticsearch", "8.12.0"},
		{9092, []byte{0, 0, 0, 0x20, 0, 0, 0, 1, 0, 0}, "kafka", ""},
		{2379, []byte(`{"etcdserver":"3.5.12","etcdcluster":"3.5.0"}`), "etcd", "3.5"},
		{8848, []byte("<html>nacos console</html>"), "nacos", ""},
		{5672, []byte("AMQP\x00\x00\x09\x01"), "rabbitmq", ""},
		{11211, []byte("VERSION 1.6.21\r\n"), "memcached", "1.6.21"},
	}
	for _, tc := range realistic {
		name, version, conf := rules.match(tc.port, tc.banner)
		if name != tc.name {
			t.Errorf("port %d banner %q matched %q, want %q", tc.port, tc.banner, name, tc.name)
		}
		if tc.version != "" && version != tc.version {
			t.Errorf("port %d version=%q, want %q", tc.port, version, tc.version)
		}
		if conf == "" {
			t.Errorf("port %d missing confidence", tc.port)
		}
	}
}

func TestT53TempYAMLRuleIdentifiesNewService(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "click." + domain
	aid := t51InsertAsset(t, e, host, false)

	path := t53WriteRules(t, `
version: 1
rules:
  - name: clickhouse
    component: ClickHouse
    category: Databases
    confidence: high
    ports: [9000]
    banner: '(?i)clickhouse server'
    version: '([0-9]+\.[0-9]+\.[0-9]+)'
    probes: []
`)
	rules := t53MustLoad(t, path)

	var dials atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   []int{9000},
		RPS:     100,
		Timeout: time.Second,
		Dialer:  t51Dialer(&dials, map[int]string{9000: t51BannerOnConnect(t, []byte("ClickHouse server version 24.3.2"))}),
		Rules:   rules,
	}
	if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: aid, Host: host}}); err != nil {
		t.Fatal(err)
	}
	var name, version string
	if err := e.pool.QueryRow(ctx, `SELECT name, version FROM asset_services WHERE host = $1 AND port = 9000`, host).Scan(&name, &version); err != nil {
		t.Fatal(err)
	}
	if name != "clickhouse" || version != "24.3.2" {
		t.Fatalf("identified name=%q version=%q, want clickhouse/24.3.2", name, version)
	}

	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}, rules); err != nil {
		t.Fatal(err)
	}
	var display string
	var confirmed bool
	var cats []string
	var confidence string
	if err := e.pool.QueryRow(ctx, `SELECT c.name, c.confirmed, c.categories, ac.evidence->>'confidence'
		FROM components c JOIN asset_components ac ON ac.component_id = c.id
		WHERE ac.asset_id = $1 AND ac.port = 9000`, aid).Scan(&display, &confirmed, &cats, &confidence); err != nil {
		t.Fatal(err)
	}
	if display != "ClickHouse" || !confirmed {
		t.Fatalf("component name=%q confirmed=%v", display, confirmed)
	}
	if !contains(cats, "Databases") {
		t.Fatalf("categories=%v want Databases", cats)
	}
	if confidence != serviceConfidenceHigh {
		t.Fatalf("evidence confidence=%q want high", confidence)
	}
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE asset_id = $1`, aid)
		e.pool.Exec(context.Background(), `DELETE FROM components c WHERE c.name_key = 'clickhouse'
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)`)
	})
}

func TestT53InvalidRulesRejected(t *testing.T) {
	longHex := strings.Repeat("00", serviceFingerprintMaxProbeSize+1)
	cases := map[string]string{
		"bad banner regex": `
version: 1
rules:
  - name: badre
    component: Bad
    category: Databases
    confidence: high
    ports: [9000]
    banner: '(?i'
`,
		"port below range": `
version: 1
rules:
  - name: badport
    component: Bad
    category: Databases
    confidence: high
    ports: [0]
    banner: 'x'
`,
		"port above range": `
version: 1
rules:
  - name: badport2
    component: Bad
    category: Databases
    confidence: high
    ports: [70000]
    banner: 'x'
`,
		"unknown confidence": `
version: 1
rules:
  - name: badconf
    component: Bad
    category: Databases
    confidence: certain
    ports: [9000]
    banner: 'x'
`,
		"duplicate name": `
version: 1
rules:
  - name: dup
    component: Dup
    category: Databases
    confidence: high
    ports: [9000]
    banner: 'x'
  - name: dup
    component: Dup2
    category: Databases
    confidence: high
    ports: [9001]
    banner: 'y'
`,
		"probe too long": fmt.Sprintf(`
version: 1
rules:
  - name: longprobe
    component: Long
    category: Databases
    confidence: high
    ports: [9000]
    banner: 'x'
    probes:
      - hex: '%s'
        expect: '.*'
`, longHex),
		"unknown field": `
version: 1
rules:
  - name: unknownfield
    component: Unknown
    category: Databases
    confidence: high
    ports: [9000]
    banners: 'x'
`,
		"probe missing expect": `
version: 1
rules:
  - name: noexpect
    component: NoExpect
    category: Databases
    confidence: high
    ports: [9000]
    banner: 'x'
    probes:
      - text: "PING\r\n"
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := t53WriteRules(t, body)
			if _, err := loadServiceFingerprintRules(path); err == nil {
				t.Fatalf("invalid rule %q was accepted", name)
			}
		})
	}

	// 启动路径同样拒绝非法规则（main 中 log.Fatal，进程不启动）。
	t.Setenv("SERVICE_FINGERPRINT_RULES", t53WriteRules(t, cases["unknown confidence"]))
	t.Setenv("SERVICE_SCAN_PORTS", "")
	if _, err := serviceFingerprintConfigFromEnv(); err == nil {
		t.Fatal("startup must reject invalid fingerprint rules")
	}
	cfg := serviceFingerprintConfig{}
	if err := cfg.ready(); err == nil {
		t.Fatal("ready must reject invalid fingerprint rules")
	}
}

func TestT53OnlyDeclaredProbesAreSent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)
	host := "probes." + domain
	aid := t51InsertAsset(t, e, host, false)

	probe := []byte("VERIFY\r\n")
	path := t53WriteRules(t, `
version: 1
rules:
  - name: probedsvc
    component: ProbedSvc
    category: Databases
    confidence: medium
    ports: [19000]
    banner: 'OK-VERIFY'
    probes:
      - text: "VERIFY\r\n"
        expect: '^OK-VERIFY'
  - name: silentsvc
    component: SilentSvc
    category: Application server
    confidence: low
    ports: [19001]
    banner: 'SILENT-SERVICE'
    probes: []
  - name: unmatchedsvc
    component: UnmatchedSvc
    category: Databases
    confidence: medium
    ports: [19002]
    banner: 'NEVER-MATCH'
    probes:
      - text: "PING\r\n"
        expect: '^\+PONG'
`)
	rules := t53MustLoad(t, path)
	rec := newT51ByteRec()
	routes := map[int]string{
		19000: rec.fake(t, "probed", func(c net.Conn, readN func(int) []byte) {
			if b := readN(len(probe)); len(b) > 0 {
				_, _ = c.Write([]byte("OK-VERIFY\r\n"))
			}
		}),
		19001: rec.fake(t, "silent", func(c net.Conn, readN func(int) []byte) {
			_, _ = c.Write([]byte("SILENT-SERVICE 1.0"))
		}),
		19002: rec.fake(t, "unmatched", func(c net.Conn, readN func(int) []byte) {
			if b := readN(len("PING\r\n")); len(b) > 0 {
				_, _ = c.Write([]byte("UNEXPECTED-BANNER 9.9.9"))
			}
		}),
	}
	var dials atomic.Int32
	cfg := serviceFingerprintConfig{
		Ports:   []int{19000, 19001, 19002},
		RPS:     100,
		Timeout: time.Second,
		Dialer:  t51Dialer(&dials, routes),
		Rules:   rules,
	}
	if err := fingerprintOriginServices(ctx, e.pool, cfg, []serviceScanTarget{{AssetID: aid, Host: host}}); err != nil {
		t.Fatal(err)
	}
	rec.wg.Wait()
	if got := rec.bytesOf("probed"); !bytes.Equal(got, probe) {
		t.Fatalf("probed service received %q, want declared %q only", got, probe)
	}
	if got := rec.bytesOf("silent"); len(got) != 0 {
		t.Fatalf("service without declared probes received %q, want zero bytes", got)
	}
	if got := rec.bytesOf("unmatched"); !bytes.Equal(got, []byte("PING\r\n")) {
		t.Fatalf("unmatched service received %q, want declared PING only", got)
	}
	var name string
	if err := e.pool.QueryRow(ctx, `SELECT name FROM asset_services WHERE host = $1 AND port = 19000`, host).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "probedsvc" {
		t.Fatalf("probed service name=%q, want probedsvc", name)
	}
	if err := e.pool.QueryRow(ctx, `SELECT name FROM asset_services WHERE host = $1 AND port = 19001`, host).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "silentsvc" {
		t.Fatalf("silent service name=%q, want silentsvc", name)
	}
	var unmatchedName, unmatchedBanner string
	if err := e.pool.QueryRow(ctx, `SELECT name, banner FROM asset_services WHERE host = $1 AND port = 19002`, host).Scan(&unmatchedName, &unmatchedBanner); err != nil {
		t.Fatal(err)
	}
	if unmatchedName != "" || !strings.Contains(unmatchedBanner, "UNEXPECTED-BANNER") {
		t.Fatalf("unmatched service name=%q banner=%q, want open banner kept for T52 archive", unmatchedName, unmatchedBanner)
	}

	// 置信度来自规则并写入 T52 证据；未匹配的 banner 进入未识别归档。
	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: aid, Host: host}}, rules); err != nil {
		t.Fatal(err)
	}
	var conf string
	if err := e.pool.QueryRow(ctx, `SELECT ac.evidence->>'confidence' FROM asset_components ac
		WHERE ac.asset_id = $1 AND ac.port = 19001`, aid).Scan(&conf); err != nil {
		t.Fatal(err)
	}
	if conf != serviceConfidenceLow {
		t.Fatalf("silent service evidence confidence=%q want low", conf)
	}
	var unmatchedComponent string
	var unmatchedConfirmed bool
	if err := e.pool.QueryRow(ctx, `SELECT c.name, c.confirmed FROM components c
		JOIN asset_components ac ON ac.component_id = c.id
		WHERE ac.asset_id = $1 AND ac.port = 19002`, aid).Scan(&unmatchedComponent, &unmatchedConfirmed); err != nil {
		t.Fatal(err)
	}
	if unmatchedConfirmed || !strings.HasPrefix(unmatchedComponent, "unidentified-service-") {
		t.Fatalf("unmatched banner name=%q confirmed=%v, want unidentified archive", unmatchedComponent, unmatchedConfirmed)
	}
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE asset_id = $1`, aid)
		e.pool.Exec(context.Background(), `DELETE FROM components c WHERE c.name_key IN ('probedsvc','silentsvc')
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)`)
		e.pool.Exec(context.Background(), `DELETE FROM components c WHERE c.name_key LIKE 'unidentified-service-%'
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)`)
	})
}

func TestT53ExtraRulesAppendAndOverride(t *testing.T) {
	base := t53MustLoad(t, "")
	path := t53WriteRules(t, `
version: 1
rules:
  - name: redis
    component: Redis
    category: Caching
    confidence: low
    ports: [6379]
    banner: '(?im)-NOAUTH|^\+PONG'
    probes:
      - text: "PING\r\n"
        expect: '^\+PONG'
  - name: customsvc
    component: CustomSvc
    category: Application server
    confidence: medium
    ports: [19002]
    banner: '(?i)customsvc'
    probes: []
`)
	rules := t53MustLoad(t, path)
	if got, want := len(rules.Rules), len(base.Rules)+1; got != want {
		t.Fatalf("merged rules=%d, want %d (override redis + append customsvc)", got, want)
	}
	if got := rules.confidence("redis"); got != serviceConfidenceLow {
		t.Fatalf("overridden redis confidence=%q want low", got)
	}
	if got := base.confidence("redis"); got != serviceConfidenceHigh {
		t.Fatalf("builtin rules mutated: redis confidence=%q", got)
	}
	if got := rules.componentName("mysql"); got != "MySQL" {
		t.Fatalf("override removed builtin mysql: %q", got)
	}
	if !hasInt(rules.ports(), 19002) {
		t.Fatalf("appended rule port missing from default ports: %v", rules.ports())
	}

	t.Setenv("SERVICE_FINGERPRINT_RULES", path)
	t.Setenv("SERVICE_SCAN_PORTS", "")
	cfg, err := serviceFingerprintConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Rules.confidence("redis"); got != serviceConfidenceLow {
		t.Fatalf("SERVICE_FINGERPRINT_RULES override not applied: %q", got)
	}
	if !hasInt(cfg.Ports, 19002) || !hasInt(cfg.Ports, 6379) {
		t.Fatalf("SERVICE_FINGERPRINT_RULES ports not merged: %v", cfg.Ports)
	}
}

func TestT53ReidentifyStoredServices(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	path := t53WriteRules(t, `
version: 1
rules:
  - name: customsql
    component: CustomSQL
    category: Databases
    confidence: medium
    ports: [19003]
    banner: '(?i)customsql server'
    version: '([0-9]+\.[0-9]+\.[0-9]+)'
    probes: []
`)
	rules := t53MustLoad(t, path)

	h1 := "reid1." + domain
	h2 := "reid2." + domain
	h3 := "reid3." + domain
	h4 := "reid4." + domain
	id1 := t51InsertAsset(t, e, h1, false)
	id2 := t51InsertAsset(t, e, h2, false)
	id3 := t51InsertAsset(t, e, h3, false)
	for _, row := range []struct {
		id     int64
		host   string
		name   string
		banner string
	}{
		{id1, h1, "", "CustomSQL server 3.4.5"},
		{id2, h2, "unidentified-service-deadbeef", "CustomSQL server 3.4.6"},
		{id3, h3, "", "totally unknown banner"},
	} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, name, banner)
			VALUES ($1, $2, 19003, true, $3, $4)`, row.id, row.host, row.name, row.banner); err != nil {
			t.Fatal(err)
		}
	}
	// 无资产关联的历史行也应回填 name/version，但不进组件库存（asset_components.asset_id 非空）。
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_services (asset_id, host, port, open, name, banner)
		VALUES (NULL, $1, 19003, true, '', 'CustomSQL server 3.4.7')`, h4); err != nil {
		t.Fatal(err)
	}

	// 旧的未识别组件关联在重识别后应被忽略。
	var staleID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed)
		VALUES ('unidentified-service-0123456789ab', 'unidentified-service-0123456789ab', 'service', false)
		RETURNING id`).Scan(&staleID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, source, evidence)
		VALUES ($1, $2, 19003, 'service', '{"banner":"CustomSQL server 3.4.5"}')`, id1, staleID); err != nil {
		t.Fatal(err)
	}

	n, err := reidentifyStoredServices(ctx, e.pool, rules)
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("reidentified rows=%d, want at least 3", n)
	}
	for _, want := range []struct {
		host, name, version string
	}{
		{h1, "customsql", "3.4.5"},
		{h2, "customsql", "3.4.6"},
		{h4, "customsql", "3.4.7"},
	} {
		var name, version string
		if err := e.pool.QueryRow(ctx, `SELECT name, version FROM asset_services WHERE host = $1 AND port = 19003`, want.host).Scan(&name, &version); err != nil {
			t.Fatal(err)
		}
		if name != want.name || version != want.version {
			t.Fatalf("host %s reidentified name=%q version=%q, want %s/%s", want.host, name, version, want.name, want.version)
		}
	}
	var unchanged string
	if err := e.pool.QueryRow(ctx, `SELECT name FROM asset_services WHERE host = $1 AND port = 19003`, h3).Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged != "" {
		t.Fatalf("unmatched banner must stay unidentified, name=%q", unchanged)
	}

	var display string
	var confirmed bool
	var conf string
	if err := e.pool.QueryRow(ctx, `SELECT c.name, c.confirmed, ac.evidence->>'confidence'
		FROM components c JOIN asset_components ac ON ac.component_id = c.id
		WHERE ac.asset_id = $1 AND ac.port = 19003 AND ac.ignored_at IS NULL`, id1).Scan(&display, &confirmed, &conf); err != nil {
		t.Fatal(err)
	}
	if display != "CustomSQL" || !confirmed || conf != serviceConfidenceMedium {
		t.Fatalf("reidentified component name=%q confirmed=%v confidence=%q", display, confirmed, conf)
	}
	var ignored bool
	if err := e.pool.QueryRow(ctx, `SELECT ignored_at IS NOT NULL FROM asset_components WHERE asset_id = $1 AND component_id = $2`, id1, staleID).Scan(&ignored); err != nil {
		t.Fatal(err)
	}
	if !ignored {
		t.Fatal("stale unidentified component must be ignored after reidentify")
	}

	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_services WHERE host = $1`, h4)
		e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE asset_id IN ($1,$2,$3)`, id1, id2, id3)
		e.pool.Exec(context.Background(), `DELETE FROM components c WHERE c.name_key IN ('customsql','unidentified-service-0123456789ab')
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)`)
		e.pool.Exec(context.Background(), `DELETE FROM components c WHERE c.name_key LIKE 'unidentified-service-%'
			AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)
			AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)`)
	})
}
