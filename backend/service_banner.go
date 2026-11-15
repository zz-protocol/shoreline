package main

import (
	"bytes"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const serviceBannerMax = 512

var (
	bracketAddrRE = regexp.MustCompile(`\[[0-9A-Fa-f:.%]+\]`)
	v4MappedRE    = regexp.MustCompile(`(?i)::ffff:(?:\d{1,3}\.){3}\d{1,3}`)
	ipv6CandRE    = regexp.MustCompile(`(?i)(?:[0-9a-f]{0,4}:){1,7}[0-9a-f]{0,4}(?:%[0-9A-Za-z._~-]+)?`)
	ipv4QuadRE    = regexp.MustCompile(`(?:\d{1,3}\.){3}\d{1,3}`)
	hexInetRE     = regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b`)
	decInetRE     = regexp.MustCompile(`\b\d{6,10}\b`)
	octalQuadRE   = regexp.MustCompile(`\b(?:0[0-7]{1,3}|0x[0-9a-fA-F]+|\d{1,3})(?:\.(?:0[0-7]{1,3}|0x[0-9a-fA-F]+|\d{1,3})){3}\b`)
	twoPartLoopRE = regexp.MustCompile(`(?:^|[^0-9.])((?:127|0)\.[0-9]+)(?:[^0-9.]|$)`)
)

func sanitizeServiceBanner(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var s strings.Builder
	s.Grow(len(b))
	for i := 0; i < len(b); {
		r, sz := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && sz == 1 {
			s.WriteByte('.')
			i++
			continue
		}
		i += sz
		if r < 32 || r == 127 {
			s.WriteByte('.')
			continue
		}
		s.WriteRune(r)
	}
	out := redactBannerIPs(s.String())
	if len(out) > serviceBannerMax {
		out = truncateBanner(out, serviceBannerMax)
	}
	return out
}

func truncateBanner(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// 按字节截断，但不切到 rune 中间。
	for max > 0 && !utf8.ValidString(s[:max]) {
		max--
	}
	return s[:max]
}

// redactBannerIPs 把可解析的 IP 写法换成 [ip]。先处理 IPv6（含 zone、[addr]:port、v4-mapped 整体），再处理 IPv4 与 inet_aton。
// 不替换版本号 8.0.36 / 10.11.6 / 5.5.5；三节全十进制一律保留。单节十进制只替换 >65535 且能 parseInetAton 的整数（如 2130706433），以免误伤端口。
func redactBannerIPs(s string) string {
	s = replaceAllVerified(s, bracketAddrRE, func(m string) bool {
		inner := strings.TrimSuffix(strings.TrimPrefix(m, "["), "]")
		inner = zoneStrip(inner)
		return net.ParseIP(inner) != nil
	})
	s = replaceAllVerified(s, v4MappedRE, func(m string) bool {
		return net.ParseIP(m) != nil
	})
	s = replaceAllVerified(s, ipv6CandRE, func(m string) bool {
		return net.ParseIP(zoneStrip(m)) != nil
	})
	s = replaceAllVerified(s, octalQuadRE, func(m string) bool {
		if isStandardDottedQuad(m) {
			return net.ParseIP(m) != nil
		}
		return parseInetAton(m) != nil
	})
	s = replaceAllVerified(s, ipv4QuadRE, func(m string) bool {
		return isStandardDottedQuad(m) && net.ParseIP(m) != nil
	})
	s = replaceAllVerified(s, hexInetRE, func(m string) bool {
		return parseInetAton(m) != nil
	})
	s = replaceAllVerified(s, decInetRE, func(m string) bool {
		n, err := strconv.ParseUint(m, 10, 64)
		if err != nil || n <= 65535 {
			return false
		}
		return parseInetAton(m) != nil
	})
	s = replaceTwoPartLoopback(s)
	return s
}

func replaceTwoPartLoopback(s string) string {
	return twoPartLoopRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := twoPartLoopRE.FindStringSubmatch(m)
		if len(sub) < 2 || parseInetAton(sub[1]) == nil {
			return m
		}
		return strings.Replace(m, sub[1], "[ip]", 1)
	})
}

func zoneStrip(s string) string {
	if i := strings.IndexByte(s, '%'); i >= 0 {
		return s[:i]
	}
	return s
}

func isStandardDottedQuad(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.HasPrefix(strings.ToLower(p), "0x") {
			return false
		}
		if len(p) > 1 && p[0] == '0' {
			return false // 八进制标签走 inet_aton 路径
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

func replaceAllVerified(s string, re *regexp.Regexp, ok func(string) bool) string {
	idxs := re.FindAllStringIndex(s, -1)
	if len(idxs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, ij := range idxs {
		i, j := ij[0], ij[1]
		if i < last {
			continue
		}
		m := s[i:j]
		if !ok(m) {
			continue
		}
		b.WriteString(s[last:i])
		b.WriteString("[ip]")
		last = j
	}
	b.WriteString(s[last:])
	return b.String()
}

// parseMySQLHandshake 按协议包结构解析 v10 握手（含可选 4 字节包头）。比纯正则更稳。
func parseMySQLHandshake(b []byte) (name, version string, ok bool) {
	p := b
	if len(p) >= 5 && p[4] == 0x0a {
		plen := int(p[0]) | int(p[1])<<8 | int(p[2])<<16
		if plen > 0 && 4+plen <= len(p) {
			p = p[4 : 4+plen]
		} else {
			p = p[4:]
		}
	}
	if len(p) < 3 || p[0] != 0x0a {
		return "", "", false
	}
	end := bytes.IndexByte(p[1:], 0)
	if end < 0 {
		return "", "", false
	}
	ver := string(p[1 : 1+end])
	if ver == "" || !looksLikeMySQLVersion(ver) {
		return "", "", false
	}
	rest := []byte{}
	if 2+end < len(p) {
		rest = p[2+end:]
	}
	blob := strings.ToLower(ver + " " + string(bytes.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		if r > 127 {
			return -1
		}
		return r
	}, rest)))
	if !strings.Contains(strings.ToLower(ver), "mariadb") &&
		!strings.Contains(blob, "mysql") &&
		!strings.Contains(blob, "caching_sha2") &&
		!strings.Contains(blob, "native_password") &&
		!looksLikeMySQLVersion(ver) {
		return "", "", false
	}
	return "mysql", mysqlServerVersion(ver), true
}

func looksLikeMySQLVersion(ver string) bool {
	if ver == "" {
		return false
	}
	for _, r := range ver {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' || r == '+' {
			continue
		}
		return false
	}
	return strings.ContainsAny(ver, "0123456789")
}

func mysqlServerVersion(ver string) string {
	low := strings.ToLower(ver)
	if strings.HasPrefix(ver, "5.5.5-") && strings.Contains(low, "mariadb") {
		rest := ver[len("5.5.5-"):]
		if m := mysqlVerPrefix.FindStringSubmatch(rest); len(m) > 1 {
			return m[1]
		}
	}
	if m := mysqlVerPrefix.FindStringSubmatch(ver); len(m) > 1 {
		return m[1]
	}
	return ver
}

var mysqlVerPrefix = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)`)
