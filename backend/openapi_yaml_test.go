package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPIYAMLStrict：openapi.yaml 必须能被严格 YAML 解析器解析（T18：/docs 曾报 missed comma between flow collection entries）。
func TestOpenAPIYAMLStrict(t *testing.T) {
	var doc map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(openapiYAML))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Fatalf("openapi version %v", doc["openapi"])
	}
	paths := doc["paths"].(map[string]any)
	ops := 0
	for _, p := range paths {
		for m := range p.(map[string]any) {
			if strings.Contains("get post patch delete put", m) {
				ops++
			}
		}
	}
	if ops != 28 {
		t.Fatalf("operations %d want 28", ops)
	}
	task := doc["components"].(map[string]any)["schemas"].(map[string]any)["ScopeAccepted"].(map[string]any)["properties"].(map[string]any)["task_id"].(map[string]any)
	if task["description"] != "GET /api/scope-tasks/{id} 查询 in_scope 重算进度" {
		t.Fatalf("task_id description %q", task["description"])
	}
	// 约定：description/summary 一律加引号（或块标量），防止 { } [ ] # : 等再次破坏解析
	bad := regexp.MustCompile(`(?m)\b(description|summary): [^"'|>]`)
	if m := bad.FindAll(openapiYAML, -1); len(m) > 0 {
		t.Fatalf("unquoted description/summary: %q", m)
	}
	t.Logf("openapi %v：%d 个 path、%d 个接口，YAML 严格解析通过", doc["openapi"], len(paths), ops)
}
