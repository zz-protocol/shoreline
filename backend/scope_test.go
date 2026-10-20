package main

import "testing"

func TestInScope(t *testing.T) {
	rules := []ScopeRule{
		{"include", "*.example.test"},
		{"include", "example.test"},
		{"include", "Login.Other.test"},
		{"exclude", "*.corp.example.test"},
		{"exclude", "blog.example.test"},
	}
	cases := []struct {
		host string
		want bool
	}{
		// 通配
		{"a.example.test", true},
		{"example.test", true}, // 根域由单独 include 覆盖
		{"badexample.test", false},
		{"example.test.evil.test", false},
		// 多级
		{"a.b.c.example.test", true},
		// 排除（排除优先于 include，排除通配也覆盖多级）
		{"blog.example.test", false},
		{"x.blog.example.test", true}, // 精确排除不影响其子域
		{"vpn.corp.example.test", false},
		{"a.b.corp.example.test", false},
		{"corp.example.test", true}, // 排除通配不含自身
		// 大小写、末尾点、空白
		{"A.EXAMPLE.TEST", true},
		{"a.example.test.", true},
		{" Blog.Example.Test ", false},
		{"login.other.test", true},
		{"x.login.other.test", false}, // 精确规则不含子域
		// 无匹配
		{"other.test", false},
		{"", false},
	}
	for _, c := range cases {
		if got := InScope(c.host, rules); got != c.want {
			t.Errorf("InScope(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestInScopeEdge(t *testing.T) {
	if InScope("example.test", []ScopeRule{{"include", "*.example.test"}}) {
		t.Error("*.example.test should not match example.test")
	}
	if InScope("a.example.test", nil) {
		t.Error("no rules should be out of scope")
	}
	if InScope("a.example.test", []ScopeRule{{"exclude", "*.example.test"}}) {
		t.Error("exclude only should be out of scope")
	}
}
