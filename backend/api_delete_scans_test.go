package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestAPIDeleteVendorAndRoot(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	v := e.vendorID()
	dom := strings.ToLower(t.Name()) + ".del.test"

	other := c.want(201, "POST", "/api/vendors", map[string]any{"name": t.Name() + "-other"})
	oid := int64(other["id"].(float64))
	root := c.want(201, "POST", fmt.Sprintf("/api/vendors/%d/roots", oid), map[string]any{"domain": dom})
	rid := int64(root["id"].(float64))
	if _, err := e.pool.Exec(ctx, `INSERT INTO assets (root_id, subdomain) VALUES ($1, $2)`, rid, "a."+dom); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO root_candidates (vendor_id, domain, sources, evidence, status) VALUES ($1, $2, '{icp:company}', '{}', 'pending')`, oid, "cand."+dom); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO vendor_subsidiaries (vendor_id, name, ratio) VALUES ($1, '子', 60)`, oid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO scope_rules (vendor_id, kind, pattern) VALUES ($1, 'include', $2)`, oid, "*."+dom); err != nil {
		t.Fatal(err)
	}

	var sid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, status)
		VALUES ($1, $2, 'passive', 'pending') RETURNING id`, oid, rid).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	c.want(409, "DELETE", fmt.Sprintf("/api/vendors/%d/roots/%d", oid, rid), nil)
	c.want(409, "DELETE", fmt.Sprintf("/api/vendors/%d", oid), nil)

	e.pool.Exec(ctx, `UPDATE scan_tasks SET status = 'done' WHERE id = $1`, sid)
	c.want(204, "DELETE", fmt.Sprintf("/api/vendors/%d/roots/%d", oid, rid), nil)
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM root_domains WHERE id = $1`, rid).Scan(&n)
	if n != 0 {
		t.Fatal("root still there")
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE subdomain = $1`, "a."+dom).Scan(&n)
	if n != 0 {
		t.Fatal("asset not cascaded")
	}
	c.want(404, "DELETE", fmt.Sprintf("/api/vendors/%d/roots/%d", oid, e.rootID), nil)
	c.want(404, "DELETE", fmt.Sprintf("/api/vendors/%d/roots/999999999", oid), nil)

	c.want(201, "POST", fmt.Sprintf("/api/vendors/%d/roots", oid), map[string]any{"domain": "2." + dom})
	c.want(204, "DELETE", fmt.Sprintf("/api/vendors/%d", oid), nil)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM vendors WHERE id = $1`, oid).Scan(&n)
	if n != 0 {
		t.Fatal("vendor still there")
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM root_candidates WHERE vendor_id = $1`, oid).Scan(&n)
	if n != 0 {
		t.Fatal("candidates not cascaded")
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM vendor_subsidiaries WHERE vendor_id = $1`, oid).Scan(&n)
	if n != 0 {
		t.Fatal("subs not cascaded")
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scope_rules WHERE vendor_id = $1`, oid).Scan(&n)
	if n != 0 {
		t.Fatal("scopes not cascaded")
	}
	c.want(404, "DELETE", fmt.Sprintf("/api/vendors/%d", oid), nil)
	found := false
	for _, it := range items(c.want(200, "GET", "/api/vendors", nil)) {
		if int64(it["id"].(float64)) == v {
			found = true
		}
	}
	if !found {
		t.Fatal("original vendor gone")
	}
	t.Log("delete vendor/root: 409 on active scans, 204 + cascade, 404 ok")
}

func TestAPIListScans(t *testing.T) {
	e, c, _ := newAPITest(t, 100)
	ctx := context.Background()
	v := e.vendorID()
	e.pool.Exec(ctx, `DELETE FROM scan_tasks WHERE vendor_id = $1`, v)

	ins := func(kind, status string, deg bool) int64 {
		t.Helper()
		var id int64
		res := `{}`
		if deg {
			res = `{"degraded":true,"degraded_reason":"icp down","icp_failures":1}`
		}
		if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, status, result)
			VALUES ($1, $2, $3, $4, $5::jsonb) RETURNING id`, v, e.rootID, kind, status, res).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	idDone := ins("expand", "done", true)
	idPend := ins("passive", "pending", false)
	_ = ins("active", "running", false)

	all := items(c.want(200, "GET", "/api/scans?limit=10", nil))
	if len(all) < 3 {
		t.Fatalf("list %d", len(all))
	}
	sawDeg := false
	for _, it := range all {
		if int64(it["id"].(float64)) == idDone {
			if it["degraded"] != true || it["degraded_reason"] != "icp down" || it["status"] != "done" {
				t.Fatalf("degraded item %v", it)
			}
			sawDeg = true
		}
	}
	if !sawDeg {
		t.Fatal("missing degraded scan")
	}
	deg := items(c.want(200, "GET", "/api/scans?degraded=true", nil))
	if len(deg) != 1 || int64(deg[0]["id"].(float64)) != idDone {
		t.Fatalf("degraded filter %v", deg)
	}
	pend := items(c.want(200, "GET", fmt.Sprintf("/api/scans?status=pending&vendor=%d", v), nil))
	if len(pend) != 1 || int64(pend[0]["id"].(float64)) != idPend {
		t.Fatalf("status filter %v", pend)
	}
	byRoot := items(c.want(200, "GET", fmt.Sprintf("/api/scans?root=%d&kind=expand", e.rootID), nil))
	if len(byRoot) != 1 || int64(byRoot[0]["id"].(float64)) != idDone {
		t.Fatalf("root+kind %v", byRoot)
	}
	page1 := c.want(200, "GET", "/api/scans?limit=1", nil)
	if page1["next_cursor"] == nil {
		t.Fatal("want next_cursor")
	}
	page2 := items(c.want(200, "GET", fmt.Sprintf("/api/scans?limit=1&cursor=%v", page1["next_cursor"]), nil))
	if len(page2) != 1 || int64(page2[0]["id"].(float64)) >= int64(items(page1)[0]["id"].(float64)) {
		t.Fatalf("cursor page %v after %v", page2, page1)
	}
	c.want(400, "GET", "/api/scans?status=nope", nil)
	c.want(400, "GET", "/api/scans?kind=nope", nil)
	c.want(400, "GET", "/api/scans?vendor=-1", nil)
	c.want(400, "GET", "/api/scans?limit=0", nil)
	c.want(400, "GET", "/api/scans?degraded=maybe", nil)
	t.Logf("list scans: %d items, degraded=%v pending=%v", len(all), deg[0]["id"], pend[0]["id"])
}
