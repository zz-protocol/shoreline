package main

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func t52AssetAlive(t *testing.T, e *testEnv, id int64) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT alive FROM assets WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestT52ServiceBannerMarksAliveAndDefaultList(t *testing.T) {
	e, c, _ := newAPITest(t, 500)
	resetServiceScan(t, e)
	ctx := context.Background()
	domain := t51RootDomain(t, e)
	t51Enable(t, e, domain)

	bannerHost := "banner." + domain
	closedHost := "closed." + domain
	emptyHost := "empty." + domain
	bid := t51InsertAsset(t, e, bannerHost, false)
	cid := t51InsertAsset(t, e, closedHost, false)
	eid := t51InsertAsset(t, e, emptyHost, false)
	if got := t52AssetAlive(t, e, bid); got != "unknown" {
		t.Fatalf("banner host pre-scan alive=%q, want unknown", got)
	}
	if got := t52AssetAlive(t, e, cid); got != "unknown" {
		t.Fatalf("closed host pre-scan alive=%q, want unknown", got)
	}
	if got := t52AssetAlive(t, e, eid); got != "unknown" {
		t.Fatalf("empty host pre-scan alive=%q, want unknown", got)
	}

	mysqlBanner := append([]byte{0x0a}, []byte("8.0.36\x00mysql_native_password")...)
	base := serviceFingerprintConfig{Ports: []int{3306}, RPS: 100, Timeout: 300 * time.Millisecond}
	rules, err := loadServiceFingerprintRules("")
	if err != nil {
		t.Fatal(err)
	}

	var d1, d2, d3 atomic.Int32
	bannerCfg := base
	bannerCfg.Dialer = t51Dialer(&d1, map[int]string{3306: t51BannerOnConnect(t, mysqlBanner)})
	if err := fingerprintOriginServices(ctx, e.pool, bannerCfg, []serviceScanTarget{{AssetID: bid, Host: bannerHost}}); err != nil {
		t.Fatal(err)
	}
	if err := syncAssetServicesToComponents(ctx, e.pool, []serviceScanTarget{{AssetID: bid, Host: bannerHost}}, rules); err != nil {
		t.Fatal(err)
	}
	if t52AssetAlive(t, e, bid) != "alive" {
		t.Fatalf("banner/identify must mark host alive, got %q", t52AssetAlive(t, e, bid))
	}

	// 默认列表（不含 include_dead）应出现 MySQL；响应不含 IP。
	list := c.want(200, "GET", "/api/components?include_wildcard=true&include_customer_hosted=true", nil)
	t51NoIP(t, list, "default component list")
	if !contains(namesOf(items(list)), "MySQL") {
		t.Fatalf("default list missing MySQL after service banner: %v", namesOf(items(list)))
	}

	closedCfg := base
	closedCfg.Dialer = t51Dialer(&d2, map[int]string{})
	if err := fingerprintOriginServices(ctx, e.pool, closedCfg, []serviceScanTarget{{AssetID: cid, Host: closedHost}}); err != nil {
		t.Fatal(err)
	}
	if t52AssetAlive(t, e, cid) != "unknown" {
		t.Fatalf("closed ports must leave alive unchanged, got %q", t52AssetAlive(t, e, cid))
	}

	emptyCfg := base
	emptyCfg.Dialer = t51Dialer(&d3, map[int]string{
		3306: t51Listen(t, func(conn net.Conn) { defer conn.Close() }),
	})
	if err := fingerprintOriginServices(ctx, e.pool, emptyCfg, []serviceScanTarget{{AssetID: eid, Host: emptyHost}}); err != nil {
		t.Fatal(err)
	}
	if t52AssetAlive(t, e, eid) != "unknown" {
		t.Fatalf("open port with no banner must leave alive unchanged, got %q", t52AssetAlive(t, e, eid))
	}

	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM asset_components WHERE asset_id = $1`, bid)
		e.pool.Exec(context.Background(), `DELETE FROM components c
			WHERE c.name_key = 'mysql'
			  AND NOT EXISTS (SELECT 1 FROM url_components uc WHERE uc.component_id = c.id)
			  AND NOT EXISTS (SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id)`)
	})
}
