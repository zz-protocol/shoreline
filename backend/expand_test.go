package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// mockICP 本地 ICP 接口：按 company / domain 返回备案记录，要求 Bearer key。
func mockICP(t *testing.T) (*httpICP, *[]string) {
	t.Helper()
	byCompany := map[string][]ICPRecord{
		"示例科技有限公司": {{"corp.test", "示例科技有限公司", "京ICP备00000001号"}, {"corp-cloud.test", "示例科技有限公司", "京ICP备00000001号"},
			{"CORP-Shop.test.", "示例科技有限公司", "京ICP备00000001号"}},
		"子公司甲":     {{"suba.test", "子公司甲", "沪ICP备00000002号"}},
		"子公司乙":     {{"subb.test", "子公司乙", "沪ICP备00000003号"}},
		"示例集团有限公司": {{"group.test", "示例集团有限公司", "京ICP备00000004号"}, {"corp-cloud.test", "示例集团有限公司", "京ICP备00000001号"}},
	}
	byDomain := map[string][]ICPRecord{"corp.test": {{"corp.test", "示例集团有限公司", "京ICP备00000004号"}}}
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		mu.Lock()
		reqs = append(reqs, r.URL.RawQuery)
		mu.Unlock()
		var recs []ICPRecord
		if c := r.URL.Query().Get("company"); c != "" {
			recs = byCompany[c]
		} else {
			recs = byDomain[r.URL.Query().Get("domain")]
		}
		json.NewEncoder(w).Encode(map[string]any{"records": recs})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ICP_API_URL", srv.URL+"/icp")
	t.Setenv("ICP_API_KEY", "test-key")
	src, err := icpSourceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return src.(*httpICP), &reqs
}

func TestICPSourceFromEnv(t *testing.T) {
	t.Setenv("ICP_API_URL", "")
	if s, err := icpSourceFromEnv(); s != nil || err != nil {
		t.Fatalf("unset: %v %v", s, err)
	}
	for _, v := range []string{"ftp://x", "not a url", "http://"} {
		t.Setenv("ICP_API_URL", v)
		if _, err := icpSourceFromEnv(); err == nil {
			t.Fatalf("ICP_API_URL=%q accepted", v)
		}
	}
}

func TestImportSubsidiaries(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM vendor_subsidiaries WHERE vendor_id = $1`, vendorID)
	})
	n, err := importSubsidiaries(ctx, e.pool, vendorID, strings.NewReader("# 名称,持股%\n子公司甲,60\n子公司乙, 30%\n子公司甲,65\n"))
	if err != nil || n != 3 {
		t.Fatalf("import: %d %v", n, err)
	}
	got, _ := dbSubsidiaries{e.pool}.Subsidiaries(ctx, vendorID, "")
	if want := []Subsidiary{{"子公司乙", 30}, {"子公司甲", 65}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, bad := range []string{"x,101\n", "x,-1\n", "y,10\nx,abc\n", ",50\n", "x\n", "x,NaN\n"} {
		if _, err := importSubsidiaries(ctx, e.pool, vendorID, strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	t.Logf("导入 %d 行，同名覆盖后 %v；非法行拒绝", n, got)
}

// 全链路：expand 只手动触发；主体名→ICP、子公司(>=50%)→ICP、已有根域反查主体→ICP；结果进待确认，不自动加入扫描；
// 确认后才写 root_domains；忽略的再次扩展不再提示。
func TestExpandEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	icp, reqs := mockICP(t)
	var vendorID int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&vendorID)
	clean := func() {
		c := context.Background()
		e.pool.Exec(c, `DELETE FROM root_candidates WHERE vendor_id = $1`, vendorID)
		e.pool.Exec(c, `DELETE FROM vendor_subsidiaries WHERE vendor_id = $1`, vendorID)
		e.pool.Exec(c, `DELETE FROM root_domains WHERE domain LIKE '%corp%.test' OR domain IN ('suba.test', 'subb.test', 'group.test')`)
		e.pool.Exec(c, `UPDATE vendors SET company_name = NULL WHERE id = $1`, vendorID)
	}
	clean()
	t.Cleanup(clean)
	var rootID int64
	e.pool.QueryRow(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 'corp.test') RETURNING id`, vendorID).Scan(&rootID)
	e.pool.Exec(ctx, `UPDATE vendors SET company_name = '示例科技有限公司' WHERE id = $1`, vendorID)
	importSubsidiaries(ctx, e.pool, vendorID, strings.NewReader("子公司甲,60\n子公司乙,30\n"))
	e.rootID = rootID
	e.start(t, testConfig(fastRates), map[string]ScanHandler{"expand": expandHandler(e.pool, icp, dbSubsidiaries{e.pool})})

	candidates := func() map[string]string {
		rows, _ := e.pool.Query(ctx, `SELECT domain || '|' || status || '|' || array_to_string(sources, '+') FROM root_candidates WHERE vendor_id = $1 ORDER BY 1`, vendorID)
		list, _ := pgxCollectStrings(rows)
		out := map[string]string{}
		for _, l := range list {
			p := strings.SplitN(l, "|", 2)
			out[p[0]] = p[1]
		}
		return out
	}
	rootsCount := func() (n int) {
		e.pool.QueryRow(ctx, `SELECT count(*) FROM root_domains WHERE vendor_id = $1`, vendorID).Scan(&n)
		return
	}
	before := rootsCount()
	if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "expand", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "expand", "done", 1)
	got := candidates()
	want := map[string]string{
		"corp-cloud.test": "pending|icp:company+icp:reverse",
		"corp-shop.test":  "pending|icp:company",
		"group.test":      "pending|icp:reverse",
		"suba.test":       "pending|icp:subsidiary",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates %v\nwant %v", got, want)
	}
	var others int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM scan_tasks WHERE root_id = $1 AND kind <> 'expand'`, rootID).Scan(&others)
	if rootsCount() != before || others != 0 {
		t.Fatalf("auto-added: roots %d->%d, follow-up tasks %d", before, rootsCount(), others)
	}
	var ev, result string
	e.pool.QueryRow(ctx, `SELECT evidence::text FROM root_candidates WHERE domain = 'suba.test'`).Scan(&ev)
	e.pool.QueryRow(ctx, `SELECT result::text FROM scan_tasks WHERE root_id = $1 AND kind = 'expand'`, rootID).Scan(&result)
	t.Logf("第 1 轮：待确认 %v；根域数不变 %d；suba 依据 %s；result %s；ICP 请求 %v", got, before, ev, result, *reqs)

	// 确认 suba.test → 写 root_domains；忽略 group.test
	var subaID, groupID int64
	e.pool.QueryRow(ctx, `SELECT id FROM root_candidates WHERE domain = 'suba.test'`).Scan(&subaID)
	e.pool.QueryRow(ctx, `SELECT id FROM root_candidates WHERE domain = 'group.test'`).Scan(&groupID)
	if err := confirmCandidate(ctx, e.pool, subaID); err != nil {
		t.Fatal(err)
	}
	if err := ignoreCandidate(ctx, e.pool, groupID); err != nil {
		t.Fatal(err)
	}
	if err := confirmCandidate(ctx, e.pool, groupID); err == nil {
		t.Fatal("confirmed an ignored candidate")
	}
	var subaRoot int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM root_domains WHERE domain = 'suba.test' AND vendor_id = $1`, vendorID).Scan(&subaRoot)
	if subaRoot != 1 {
		t.Fatal("confirmed candidate not in root_domains")
	}
	// 第 2 轮：忽略的不再提示，已确认的（已是根域）不再出现，无新增
	if _, err := TriggerScan(ctx, e.pool, e.client, rootID, "expand", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "expand", "done", 2)
	got2 := candidates()
	want2 := map[string]string{
		"corp-cloud.test": "pending|icp:company+icp:reverse",
		"corp-shop.test":  "pending|icp:company",
		"group.test":      "ignored|icp:reverse",
		"suba.test":       "confirmed|icp:subsidiary",
	}
	if !reflect.DeepEqual(got2, want2) {
		t.Fatalf("round 2 %v\nwant %v", got2, want2)
	}
	var result2 string
	e.pool.QueryRow(ctx, `SELECT result::text FROM scan_tasks WHERE root_id = $1 AND kind = 'expand' ORDER BY id DESC LIMIT 1`, rootID).Scan(&result2)
	if !strings.Contains(result2, `"new_candidates": 0`) {
		t.Fatalf("round 2 result %s", result2)
	}
	t.Logf("第 2 轮：%v；result %s", got2, result2)
}

func TestExpandSkippedWithoutICP(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.start(t, testConfig(fastRates), map[string]ScanHandler{"expand": expandHandler(e.pool, nil, dbSubsidiaries{e.pool})})
	if _, err := TriggerScan(ctx, e.pool, e.client, e.rootID, "expand", 3); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, "expand", "done", 1)
	var result string
	e.pool.QueryRow(ctx, `SELECT result::text FROM scan_tasks WHERE root_id = $1 AND kind = 'expand'`, e.rootID).Scan(&result)
	if !strings.Contains(result, "icp not configured") {
		t.Fatalf("result %s", result)
	}
	t.Logf("未配置 ICP_API_URL：%s", result)
}

type failICP struct{}

func (failICP) ByCompany(context.Context, string) ([]ICPRecord, error) {
	return nil, errors.New("icp down")
}
func (failICP) ByDomain(context.Context, string) ([]ICPRecord, error) {
	return nil, errors.New("icp down")
}

// T11 补：同域名被另一厂商发现时证据记该厂商；pending 时可改归属；ICP 失败次数计入结果。
func TestCandidateOtherVendorReassignAndICPFailures(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var v1, v2 int64
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_domains WHERE id = $1`, e.rootID).Scan(&v1)
	e.pool.QueryRow(ctx, `INSERT INTO vendors (name) VALUES ('另一厂商-test') RETURNING id`).Scan(&v2)
	t.Cleanup(func() {
		c := context.Background()
		e.pool.Exec(c, `DELETE FROM root_candidates WHERE domain = 'shared.test'`)
		e.pool.Exec(c, `DELETE FROM vendors WHERE id = $1`, v2)
	})
	found := map[string][]candidate{"shared.test": {{"icp:company", map[string]any{"company": "甲"}}}}
	if _, err := storeCandidates(ctx, e.pool, v1, found); err != nil {
		t.Fatal(err)
	}
	if _, err := storeCandidates(ctx, e.pool, v2, found); err != nil {
		t.Fatal(err)
	}
	var id, owner int64
	var vendors []int64
	e.pool.QueryRow(ctx, `SELECT id, vendor_id, ARRAY(SELECT DISTINCT (e->>'vendor_id')::bigint FROM jsonb_array_elements(evidence) e ORDER BY 1)
		FROM root_candidates WHERE domain = 'shared.test'`).Scan(&id, &owner, &vendors)
	if owner != v1 || !reflect.DeepEqual(vendors, []int64{min(v1, v2), max(v1, v2)}) {
		t.Fatalf("owner %d evidence vendors %v", owner, vendors)
	}
	if err := reassignCandidate(ctx, e.pool, id, v2); err != nil {
		t.Fatal(err)
	}
	e.pool.QueryRow(ctx, `SELECT vendor_id FROM root_candidates WHERE id = $1`, id).Scan(&owner)
	if owner != v2 {
		t.Fatalf("reassign: owner %d", owner)
	}
	ignoreCandidate(ctx, e.pool, id)
	if err := reassignCandidate(ctx, e.pool, id, v1); err == nil {
		t.Fatal("reassigned a non-pending candidate")
	}
	_, fails, err := expandVendor(ctx, failICP{}, nil, v1, "甲", []string{"a.test", "b.test"})
	if err == nil || fails.ICPFailures != 3 {
		t.Fatalf("fails=%+v err=%v", fails, err)
	}
	t.Logf("证据厂商 %v，改归属 %d→%d 成功，非 pending 改归属被拒；ICP 失败次数 %d", vendors, v1, v2, fails.ICPFailures)
}

// T11 打回 1/5：ICP 域名规范化、非法拒绝、已有根域及其子域丢弃、同批重复证据去重。
func TestICPDomainNormalizeAndDedup(t *testing.T) {
	cases := map[string]string{
		"*.Corp-X.test": "corp-x.test", "https://Shop.Corp-X.test:8443/path?a=1": "shop.corp-x.test",
		"http://user@a.test/": "a.test", "  B.TEST.  ": "b.test", "c.test:80": "c.test",
		"bad_domain.test": "", "nodot": "", "": "", "http://": "", "a..test": "", "-a.test": "",
	}
	for in, want := range cases {
		got, ok := normalizeICPDomain(in)
		if (want == "") == ok || (ok && got != want) {
			t.Fatalf("%q → %q %v, want %q", in, got, ok, want)
		}
	}
	src := stubICP{company: map[string][]ICPRecord{"甲": {
		{"*.New-A.test", "甲", "L1"}, {"https://new-a.test/x", "甲", "L1"}, {"NEW-A.test", "甲", "L1"}, // 规范化后同一条，证据去重
		{"new-a.test", "甲", "L2"},                                  // 备案号不同，保留
		{"www.corp.test", "甲", "L1"}, {"corp.test:443", "甲", "L1"}, // 已有根域及其子域：丢弃
		{"bad_domain", "甲", "L1"}, {"http://", "甲", "L1"}, // 非法：丢弃
	}}}
	found, fails, err := expandVendor(context.Background(), src, nil, 1, "甲", []string{"corp.test"})
	if err != nil || fails.ICPFailures != 0 {
		t.Fatal(err)
	}
	if len(found) != 1 || len(found["new-a.test"]) != 2 {
		t.Fatalf("found %v", found)
	}
	t.Logf("规范化 %d 例通过；候选 %v", len(cases), found)
}

type stubICP struct{ company map[string][]ICPRecord }

func (s stubICP) ByCompany(_ context.Context, c string) ([]ICPRecord, error) {
	return s.company[c], nil
}
func (s stubICP) ByDomain(context.Context, string) ([]ICPRecord, error) { return nil, nil }

// 已是其它厂商根域的子域，入库层也丢弃。
func TestStoreCandidatesSkipsSubdomainOfAnyRoot(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root := ""
	e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&root)
	n, err := storeCandidates(ctx, e.pool, e.vendorID(), map[string][]candidate{"www." + root: {{"icp:company", map[string]any{}}}, root: {{"icp:company", map[string]any{}}}})
	if err != nil || n != 0 {
		t.Fatalf("stored %d %v", n, err)
	}
}

// T11 打回 2：ICP_API_URL/KEY 含控制字符启动拒绝（main 中 log.Fatal → exit 1）。
func TestICPEnvControlChars(t *testing.T) {
	for _, c := range []struct{ url, key string }{{"http://icp.local/q\n", ""}, {"http://icp.local/q", "k\r\nX-Evil: 1"}, {"http://icp.local/\tq", ""}} {
		t.Setenv("ICP_API_URL", c.url)
		t.Setenv("ICP_API_KEY", c.key)
		if _, err := icpSourceFromEnv(); err == nil {
			t.Fatalf("accepted %q %q", c.url, c.key)
		}
	}
}

// T11 打回 3：CSV 去 BOM、首行表头跳过。
func TestImportSubsidiariesBOMHeader(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	v := e.vendorID()
	n, err := importSubsidiaries(ctx, e.pool, v, strings.NewReader("\ufeff名称,持股比例\n子公司甲,60\n子公司乙,30%\n"))
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	var names []string
	rows, _ := e.pool.Query(ctx, `SELECT name || '=' || ratio::text FROM vendor_subsidiaries WHERE vendor_id = $1 ORDER BY name`, v)
	names, _ = pgxCollectStrings(rows)
	if strings.Join(names, ",") != "子公司乙=30.00,子公司甲=60.00" {
		t.Fatalf("%v", names)
	}
	if _, err := importSubsidiaries(ctx, e.pool, v, strings.NewReader("子公司丙,60\n子公司丁,abc\n")); err == nil {
		t.Fatal("non-numeric ratio on line 2 accepted")
	}
	n, err = importSubsidiaries(ctx, e.pool, v, strings.NewReader("\ufeff子公司丙,55\n"))
	if err != nil || n != 1 {
		t.Fatalf("BOM without header: %d %v", n, err)
	}
	t.Logf("BOM+表头 → 导入 %v；无表头带 BOM 首行为数据 → 1 行", names)
}

// T11 打回 4：pending 期间域名成了别厂商根域，函数层 confirm 返回错误，候选保持 pending。
func TestConfirmCandidateTakenFunction(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	v := e.vendorID()
	var v2, id int64
	e.pool.QueryRow(ctx, `INSERT INTO vendors (name) VALUES ('t11-taken-other') RETURNING id`).Scan(&v2)
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM root_candidates WHERE domain = 't11taken.test'`)
		e.pool.Exec(context.Background(), `DELETE FROM vendors WHERE id = $1`, v2)
	})
	storeCandidates(ctx, e.pool, v, map[string][]candidate{"t11taken.test": {{"icp:company", map[string]any{}}}})
	e.pool.QueryRow(ctx, `SELECT id FROM root_candidates WHERE domain = 't11taken.test'`).Scan(&id)
	e.pool.Exec(ctx, `INSERT INTO root_domains (vendor_id, domain) VALUES ($1, 't11taken.test')`, v2)
	err := confirmCandidate(ctx, e.pool, id)
	var st string
	e.pool.QueryRow(ctx, `SELECT status FROM root_candidates WHERE id = $1`, id).Scan(&st)
	if !errors.Is(err, errRootTaken) || st != "pending" {
		t.Fatalf("err %v status %s", err, st)
	}
	t.Logf("confirm → %v；候选 %s", err, st)
}

// T11 复验：IP（v4/v6）丢弃计入 skipped_ip；中文域名转 punycode。
func TestICPSkipIPAndPunycode(t *testing.T) {
	src := stubICP{company: map[string][]ICPRecord{"甲": {
		{"1.2.3.4", "甲", "L"}, {"http://10.0.0.1:8080/x", "甲", "L"}, {"2001:db8::1", "甲", "L"}, {"[2001:db8::2]:443", "甲", "L"}, {"https://[::1]/", "甲", "L"},
		{"例子.中国", "甲", "L"}, {"https://WWW.测试.公司/x", "甲", "L"}, {"bad_domain", "甲", "L"}, {"www.corp.test", "甲", "L"},
	}}}
	found, st, err := expandVendor(context.Background(), src, nil, 1, "甲", []string{"corp.test"})
	if err != nil {
		t.Fatal(err)
	}
	var doms []string
	for d := range found {
		doms = append(doms, d)
	}
	sort.Strings(doms)
	want := []string{"www.xn--0zwm56d.xn--55qx5d", "xn--fsqu00a.xn--fiqs8s"}
	if !reflect.DeepEqual(doms, want) || st != (expandStats{SkippedIP: 5, SkippedInvalid: 1, SkippedKnownRoot: 1}) {
		t.Fatalf("doms %v stats %+v", doms, st)
	}
	t.Logf("候选 %v；统计 %+v", doms, st)
}

// T11 复验：CSV 表头只认"所有列都是非数字列名且比例列是已知列名"；"51 %"、"51％" 可用；坏比例第 1/2 行都报错带行号。
func TestImportSubsidiariesStrict(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	v := e.vendorID()
	n, err := importSubsidiaries(ctx, e.pool, v, strings.NewReader("子A,51 %\n子H,51％\n"))
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := dbSubsidiaries{e.pool}.Subsidiaries(ctx, v, "")
	t.Logf("子A,51 %% / 子H,51％ → 导入 %v", got)
	for _, hdr := range []string{"名称,比例\n", "name,ratio\n", "子公司名称,持股比例\n", "Company,Share\n"} {
		if n, err := importSubsidiaries(ctx, e.pool, v, strings.NewReader(hdr+"子E,60\n")); err != nil || n != 1 {
			t.Fatalf("header %q: %d %v", hdr, n, err)
		}
	}
	for _, bad := range []string{"子B,五十一", "子I,0x33", "子L,", "子D,abc", "子M,1e2", "子N,Inf", "子O,NaN", "子P,101"} {
		for _, pos := range []int{1, 2} {
			in := bad + "\n"
			if pos == 2 {
				in = "子Z,60\n" + bad + "\n"
			}
			_, err := importSubsidiaries(ctx, e.pool, v, strings.NewReader(in))
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("line %d", pos)) {
				t.Fatalf("%q at line %d: %v", bad, pos, err)
			}
			t.Logf("%q 第 %d 行 → %v", bad, pos, err)
		}
	}
}
