package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
)

func collectListNames(t *testing.T, c *apiClient, rawQuery string) []string {
	t.Helper()
	var names []string
	cursor := ""
	for {
		path := "/api/components?limit=1&" + rawQuery
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		out := c.want(200, "GET", path, nil)
		for _, n := range namesOf(items(out)) {
			names = append(names, n)
		}
		if out["next_cursor"] == nil {
			break
		}
		cursor = fmt.Sprint(intOf(out["next_cursor"]))
	}
	sort.Strings(names)
	return names
}

func TestT48ComponentListValues(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	_ = a
	if _, err := seedCommonComponents(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	front, srv, cross, empty, other := seedT47CategoryFixture(t, e)
	pfx := url.QueryEscape(t.Name())

	// 与分页列表（limit=1 跨页）同一筛选：默认隐藏纯前端，名称去重排序。
	q := "q=" + pfx
	listNames := collectListNames(t, c, q)
	vals := c.want(200, "GET", "/api/components/values?"+q, nil)
	got := stringsOf(vals["items"])
	if vals["truncated"] != false || vals["field"] != "name" || intOf(vals["count"]) != int64(len(got)) {
		t.Fatalf("values meta %+v", vals)
	}
	if strings.Join(got, ",") != strings.Join(listNames, ",") {
		t.Fatalf("values %v != list %v", got, listNames)
	}
	if contains(got, front) || !contains(got, srv) || !contains(got, cross) || !contains(got, empty) || !contains(got, other) {
		t.Fatalf("default filter names %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("not sorted: %v", got)
		}
		if got[i-1] == got[i] {
			t.Fatalf("duplicate: %v", got)
		}
	}

	// 展开全部：含纯前端，且与跨页列表一致
	qAll := q + "&include_all_categories=true"
	allList := collectListNames(t, c, qAll)
	allVals := stringsOf(c.want(200, "GET", "/api/components/values?"+qAll, nil)["items"])
	if !contains(allVals, front) || strings.Join(allVals, ",") != strings.Join(allList, ",") {
		t.Fatalf("include_all values %v list %v", allVals, allList)
	}

	// category 精筛
	qCat := q + "&category=" + url.QueryEscape("JavaScript libraries")
	catList := collectListNames(t, c, qCat)
	catVals := stringsOf(c.want(200, "GET", "/api/components/values?"+qCat, nil)["items"])
	if !contains(catVals, front) || contains(catVals, srv) || strings.Join(catVals, ",") != strings.Join(catList, ",") {
		t.Fatalf("category values %v list %v", catVals, catList)
	}

	// 既有下钻复制路径不受影响
	id := intOf(items(c.want(200, "GET", "/api/components?q="+url.QueryEscape(srv), nil))[0]["id"])
	drill := c.want(200, "GET", fmt.Sprintf("/api/components/%d/values?field=subdomain", id), nil)
	if intOf(drill["count"]) != 1 || drill["truncated"] != false {
		t.Fatalf("drill values %+v", drill)
	}

	c.want(400, "GET", "/api/components/values?field=subdomain", nil)

	// 超上限截断
	old := componentListValuesMax
	componentListValuesMax = 2
	t.Cleanup(func() { componentListValuesMax = old })
	trunc := c.want(200, "GET", "/api/components/values?"+qAll, nil)
	if trunc["truncated"] != true || intOf(trunc["count"]) != 2 || len(stringsOf(trunc["items"])) != 2 {
		t.Fatalf("truncated %+v", trunc)
	}
	wantTrunc := allVals
	if len(wantTrunc) > 2 {
		wantTrunc = wantTrunc[:2]
	}
	if strings.Join(stringsOf(trunc["items"]), ",") != strings.Join(wantTrunc, ",") {
		t.Fatalf("truncated items %v want %v", trunc["items"], wantTrunc)
	}
}

func TestT48ValuesMatchesT43Filters(t *testing.T) {
	e, c, a := newAPITest(t, 500)
	_ = a
	if _, err := seedCommonComponents(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	seedT43Fixture(t, e)
	// 只看本轮夹具里以测试名称为前缀的罕见组件（RareCMS/RareLib 是固定名，用 exclude_common 默认列表再按精确 q 对齐）
	for _, q := range []string{
		"q=RareCMS",
		"q=RareLib&include_customer_hosted=true",
		"q=RareCMS&include_wildcard=true",
		"q=RareCMS&include_dead=true",
		"exclude_common=false&q=Nginx",
	} {
		list := collectListNames(t, c, q)
		vals := stringsOf(c.want(200, "GET", "/api/components/values?"+q, nil)["items"])
		if strings.Join(vals, ",") != strings.Join(list, ",") {
			t.Fatalf("%s: values %v list %v", q, vals, list)
		}
		if len(vals) == 0 {
			t.Fatalf("%s: empty values", q)
		}
	}
}
