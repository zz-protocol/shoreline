package main

import (
	"strings"
	"testing"
)

func TestT51SanitizeInetAtonWithIPContext(t *testing.T) {
	// 有明显 IP 语境的两段/三段十进制简写应替换；无语境的版本号必须保留。
	replaced := []struct{ in, leak string }{
		{"host=10.1.2 denied", "10.1.2"},
		{"Host=10.1.2 denied", "10.1.2"},
		{"addr=10.1.2 x", "10.1.2"},
		{"from 10.1 x", "10.1"},
		{"FROM 10.1 x", "10.1"},
	}
	for _, c := range replaced {
		got := sanitizeServiceBanner([]byte(c.in))
		if strings.Contains(got, c.leak) {
			t.Errorf("context shorthand not redacted: in=%q out=%q still has %q", c.in, got, c.leak)
		}
		if !strings.Contains(got, "[ip]") {
			t.Errorf("expected [ip] in out=%q (in=%q)", got, c.in)
		}
	}

	kept := []string{
		"MySQL 8.0.36",
		"version 10.11.6",
		"MariaDB 5.5.5",
	}
	for _, in := range kept {
		got := sanitizeServiceBanner([]byte(in))
		if got != in {
			t.Errorf("version injured: in=%q out=%q", in, got)
		}
		if strings.Contains(got, "[ip]") {
			t.Errorf("version replaced: in=%q out=%q", in, got)
		}
	}

	// 前缀须为独立词，避免误伤 platform / ghost。
	if got := sanitizeServiceBanner([]byte("platform 10.11.6")); got != "platform 10.11.6" {
		t.Errorf("platform version injured: %q", got)
	}
	if got := sanitizeServiceBanner([]byte("ghost=10.1.2")); strings.Contains(got, "[ip]") && !strings.Contains(got, "10.1.2") {
		t.Errorf("ghost= should not count as host= context: %q", got)
	}
}
