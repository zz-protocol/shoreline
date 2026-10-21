package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// mockYanzhenma 按 zz-protocol/icp_yanzhenma 的接口形状模拟：GET /query?domain=<值>，
// 成功 {"status":"success","domain":...,"data":{"params":{"list":[{domain,unitName,mainLicence,natureName}]}}}，
// 失败 {"status":"failed","domain":...,"error":"..."}（HTTP 200）或 503。
func mockYanzhenma(t *testing.T, failed map[string]bool) (*httptest.Server, *[]string) {
	data := map[string][]map[string]string{
		"示例科技有限公司": {
			{"domain": "corp-new.test", "unitName": "示例科技有限公司", "mainLicence": "京ICP备00000001号", "natureName": "企业"},
			{"domain": "例子.中国", "unitName": "示例科技有限公司", "mainLicence": "京ICP备00000001号", "natureName": "企业"},
			{"domain": "1.2.3.4", "unitName": "示例科技有限公司", "mainLicence": "京ICP备00000001号", "natureName": "企业"},
		},
		"yz-root.test": {{"domain": "yz-root.test", "unitName": "示例集团有限公司", "mainLicence": "京ICP备00000004号", "natureName": "企业"}},
		"示例集团有限公司":     {{"domain": "group-new.test", "unitName": "示例集团有限公司", "mainLicence": "京ICP备00000004号", "natureName": "企业"}},
	}
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query().Get("domain")
		mu.Lock()
		reqs = append(reqs, r.URL.Path+"?domain="+v)
		mu.Unlock()
		if r.URL.Path != "/query" || v == "" {
			http.Error(w, `{"detail":"Not Found"}`, 404)
			return
		}
		if failed[v] {
			json.NewEncoder(w).Encode(map[string]any{"status": "failed", "domain": v, "error": "验证码验证失败", "data": nil})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "domain": v, "error": nil,
			"data": map[string]any{"code": 200, "success": true, "params": map[string]any{"list": data[v], "total": len(data[v])}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func yzEnv(t *testing.T, url string) ICPSource {
	t.Setenv("ICP_PROVIDER", "icp_yanzhenma")
	t.Setenv("ICP_API_URL", url)
	t.Setenv("ICP_API_KEY", "")
	src, err := icpSourceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// 跑一次 expand 扫描，返回该厂商候选（domain|sources）与扫描结果。
func runYZExpand(t *testing.T, src ICPSource) (map[string]string, map[string]any, string) {
	e := newTestEnv(t)
	ctx := context.Background()
	v := e.vendorID()
	var rootID int64
	clean := func() {
		c := context.Background()
		e.pool.Exec(c, `DELETE FROM root_candidates WHERE vendor_id = $1`, v)
		e.pool.Exec(c, `DELETE FROM root_domains WHERE domain = 'yz-root.test'`)
	}
	clean()
	t.Cleanup(clean)
	e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'yz-root.test') RETURNING id`, v).Scan(&rootID)
	e.pool.Exec(ctx, `UPDATE vendors SET company_name = '示例科技有限公司' WHERE id = $1`, v)
	e.rootID = rootID
	e.start(t, testConfig(fastRates), map[string]ScanHandler{"expand": expandHandler(e.pool, src, dbSubsidiaries{e.pool})})
	if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "expand", 0); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "expand", "done", 1)
	rows, _ := e.pool.Query(ctx, `SELECT domain || '|' || array_to_string(sources, '+') FROM root_candidates WHERE vendor_id = $1 ORDER BY 1`, v)
	list, _ := pgxCollectStrings(rows)
	cands := map[string]string{}
	for _, l := range list {
		p := strings.SplitN(l, "|", 2)
		cands[p[0]] = p[1]
	}
	var raw, status string
	e.pool.QueryRow(ctx, `SELECT COALESCE(result::text, '{}'), status FROM scan_tasks WHERE id = (SELECT max(id) FROM scan_tasks WHERE root_id = $1)`, rootID).Scan(&raw, &status)
	res := map[string]any{}
	json.Unmarshal([]byte(raw), &res)
	return cands, res, status
}

func TestYanzhenmaParse(t *testing.T) {
	srv, reqs := mockYanzhenma(t, nil)
	cands, res, status := runYZExpand(t, yzEnv(t, srv.URL+"/"))
	want := map[string]string{"corp-new.test": "icp:company", "xn--fsqu00a.xn--fiqs8s": "icp:company", "group-new.test": "icp:reverse"}
	if status != "done" || !reflect.DeepEqual(cands, want) || res["skipped_ip"] != float64(1) || res["icp_failures"] != float64(0) {
		t.Fatalf("status %s cands %v result %v", status, cands, res)
	}
	t.Logf("解析：候选 %v；result %v；请求 %v", cands, res, *reqs)
}

func TestYanzhenmaUnreachableDegrades(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close() // 端口已关闭：连接被拒
	cands, res, status := runYZExpand(t, yzEnv(t, dead))
	if status != "done" || len(cands) != 0 || res["icp_failures"].(float64) < 2 {
		t.Fatalf("status %s cands %v result %v", status, cands, res)
	}
	t.Logf("服务不可达：扫描 %s，候选 %d，result %v", status, len(cands), res)

	// 服务可达但返回 status=failed（验证码失败）：同样计失败、不报错
	srv, _ := mockYanzhenma(t, map[string]bool{"示例科技有限公司": true, "yz-root.test": true})
	cands, res, status = runYZExpand(t, yzEnv(t, srv.URL))
	if status != "done" || len(cands) != 0 || res["icp_failures"] != float64(2) {
		t.Fatalf("failed status: %s cands %v result %v", status, cands, res)
	}
	t.Logf("服务返回 status=failed：扫描 %s，候选 %d，result %v", status, len(cands), res)
}

func TestYanzhenmaNotConfiguredAndInvalidProvider(t *testing.T) {
	t.Setenv("ICP_PROVIDER", "icp_yanzhenma")
	t.Setenv("ICP_API_URL", "")
	src, err := icpSourceFromEnv()
	if err != nil || src != nil {
		t.Fatalf("not configured: %v %v", src, err)
	}
	_, res, status := runYZExpand(t, src)
	if status != "done" || res["skipped"] != "icp not configured" {
		t.Fatalf("%s %v", status, res)
	}
	t.Logf("未配置 ICP_API_URL：ICP 关闭，扫描 %s，result %v", status, res)
	for _, bad := range []string{"tianyancha", "ICP_YANZHENMA", " icp_yanzhenma"} {
		t.Setenv("ICP_PROVIDER", bad)
		if _, err := icpSourceFromEnv(); err == nil {
			t.Fatalf("provider %q accepted", bad)
		} else {
			t.Logf("ICP_PROVIDER=%q → %v", bad, err)
		}
	}
}
