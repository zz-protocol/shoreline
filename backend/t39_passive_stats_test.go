package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// T39：mock 来源——result 含 sources_ok / sources_failed / found；found==0 且有失败 → degraded；全成功不 degraded。
func TestT39PassiveResultDegradedOnSourceFailure(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var domain string
	if err := e.pool.QueryRow(ctx, `SELECT domain FROM root_domains WHERE id = $1`, e.rootID).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	newScan := func() int64 {
		var id int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO scan_tasks (vendor_id, root_id, kind, status) VALUES ($1, $2, 'passive', 'pending') RETURNING id`,
			e.vendorID(), e.rootID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	readResult := func(id int64) map[string]any {
		var raw []byte
		if err := e.pool.QueryRow(ctx, `SELECT result FROM scan_tasks WHERE id = $1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("result %s: %v", raw, err)
		}
		return m
	}

	// 1) 一源失败一源空成功、found=0 → degraded
	id := newScan()
	okEmpty := Source{Name: "okempty", Enum: func(context.Context, string) ([]string, error) { return nil, nil }}
	bad := Source{Name: "bad", Enum: func(context.Context, string) ([]string, error) { return nil, errors.New("boom") }}
	out, err := passiveHandler(e.pool, []Source{bad, okEmpty})(ctx, id, nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("handler: out=%v err=%v", out, err)
	}
	r := readResult(id)
	if r["found"].(float64) != 0 || r["sources_ok"].(float64) != 1 || r["degraded"] != true {
		t.Fatalf("degraded case: %v", r)
	}
	fails, _ := r["sources_failed"].([]any)
	if len(fails) != 1 || fails[0].(map[string]any)["source"] != "bad" {
		t.Fatalf("sources_failed: %v", fails)
	}
	if fmt.Sprint(r["degraded_reason"]) == "" || !strings.Contains(fmt.Sprint(r["degraded_reason"]), "bad") {
		t.Fatalf("degraded_reason: %v", r["degraded_reason"])
	}

	// 2) 全成功有结果 → 不 degraded
	id2 := newScan()
	good := Source{Name: "good", Enum: func(context.Context, string) ([]string, error) {
		return []string{"a." + domain, "b." + domain}, nil
	}}
	out, err = passiveHandler(e.pool, []Source{good})(ctx, id2, nil)
	if err != nil || len(out) != 2 {
		t.Fatalf("success handler: out=%v err=%v", out, err)
	}
	r = readResult(id2)
	if r["found"].(float64) != 2 || r["sources_ok"].(float64) != 1 || r["degraded"] != nil {
		t.Fatalf("success case: %v", r)
	}
	fails, _ = r["sources_failed"].([]any)
	if len(fails) != 0 {
		t.Fatalf("sources_failed want []: %v", fails)
	}

	// 3) Detailed（模拟 subfinder）暴露内部源错误
	id3 := newScan()
	sf := Source{
		Name: "subfinder",
		Detailed: func(context.Context, string) ([]string, []sourceFailure, error) {
			return nil, []sourceFailure{{Source: "threatminer", Error: "timeout"}, {Source: "urlscan", Error: "403"}}, fmt.Errorf("subfinder: 2 source(s) failed")
		},
	}
	out, err = passiveHandler(e.pool, []Source{sf})(ctx, id3, nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("subfinder mock: out=%v err=%v", out, err)
	}
	r = readResult(id3)
	if r["degraded"] != true || r["sources_ok"].(float64) != 0 {
		t.Fatalf("subfinder degraded: %v", r)
	}
	fails, _ = r["sources_failed"].([]any)
	if len(fails) != 2 {
		t.Fatalf("subfinder failed sources: %v", fails)
	}
	t.Logf("results: deg=%v ok=%v all=%v sub=%v", readResult(id)["degraded"], readResult(id2)["found"], r["sources_failed"], r["degraded_reason"])
}

func TestT39SubfinderCollectExposesSourceErrors(t *testing.T) {
	// 不联网：用 Detailed 同构逻辑已在上一个测试覆盖；这里断言 subfinderCollect 签名与 Source 注册。
	c := passiveConfig{Subfinder: true, SubfinderTimeout: time.Second, SubfinderMaxTime: time.Second}
	srcs := passiveSources(c)
	var sf *Source
	for i := range srcs {
		if srcs[i].Name == "subfinder" {
			sf = &srcs[i]
			break
		}
	}
	if sf == nil || sf.Detailed == nil {
		t.Fatal("subfinder Source must expose Detailed for per-source errors")
	}
}
