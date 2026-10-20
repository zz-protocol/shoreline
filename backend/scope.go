package main

import "strings"

// ScopeRule 对应 scope_rules 表的一行：Kind 为 "include" 或 "exclude"。
type ScopeRule struct {
	Kind    string
	Pattern string
}

// InScope 判断完整主机名 host 是否在厂商范围内：至少命中一条 include，且未命中任何 exclude（exclude 优先）。
// 匹配不区分大小写，忽略首尾空白和末尾的点。
// 模式 "example.com" 只匹配自身；"*.example.com" 匹配任意层级子域，不含 example.com 本身。
func InScope(host string, rules []ScopeRule) bool {
	host = normalizeHost(host)
	included := false
	for _, r := range rules {
		if !matchPattern(host, r.Pattern) {
			continue
		}
		if r.Kind == "exclude" {
			return false
		}
		if r.Kind == "include" {
			included = true
		}
	}
	return included
}

func matchPattern(host, pattern string) bool {
	pattern = normalizeHost(pattern)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return host != "" && host == pattern
}

func normalizeHost(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}
