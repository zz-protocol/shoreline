package main

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// T53：服务指纹规则改由 YAML 声明。内置规则随二进制嵌入，启动时校验并预编译。
//
//go:embed fingerprints/services.yaml
var builtinServiceFingerprintYAML []byte

const (
	serviceFingerprintDocVersion   = 1
	serviceFingerprintMaxProbeSize = 256

	serviceConfidenceHigh   = "high"
	serviceConfidenceMedium = "medium"
	serviceConfidenceLow    = "low"
)

var (
	serviceFingerprintNameRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	serviceFingerprintMariaDBVerRE = regexp.MustCompile(`5\.5\.5-([0-9]+\.[0-9]+\.[0-9]+)`)
)

// serviceFingerprintDocument 是 YAML 顶层结构；version 仅用于拒绝未来格式。
type serviceFingerprintDocument struct {
	Version int                          `yaml:"version"`
	Rules   []serviceFingerprintYAMLRule `yaml:"rules"`
}

type serviceFingerprintYAMLRule struct {
	Name       string                        `yaml:"name"`
	Component  string                        `yaml:"component"`
	Category   string                        `yaml:"category"`
	Confidence string                        `yaml:"confidence"`
	Repo       string                        `yaml:"repo"`
	Ports      []int                         `yaml:"ports"`
	Banner     string                        `yaml:"banner"`
	Version    string                        `yaml:"version"`
	Probes     []serviceFingerprintYAMLProbe `yaml:"probes"`
}

type serviceFingerprintYAMLProbe struct {
	Text   string `yaml:"text"`
	Hex    string `yaml:"hex"`
	Expect string `yaml:"expect"`
}

// serviceFingerprintRule 是预编译后的规则；扫描热路径只读，不再编译正则。
type serviceFingerprintRule struct {
	Name       string
	Component  string
	Category   string
	Confidence string
	Repo       string
	Ports      []int
	Probes     []serviceFingerprintProbe
	BannerRE   *regexp.Regexp
	VersionRE  *regexp.Regexp
}

type serviceFingerprintProbe struct {
	Bytes  []byte
	Expect *regexp.Regexp
}

type serviceFingerprintRuleSet struct {
	Rules  []serviceFingerprintRule
	byName map[string]int
	byPort map[int]int
}

// loadServiceFingerprintRules 加载内置规则，并按 extraPath（SERVICE_FINGERPRINT_RULES）合并额外规则。
// 额外文件同名规则整条覆盖内置规则，其余追加；加载与校验失败返回 error（启动即退出）。
func loadServiceFingerprintRules(extraPath string) (*serviceFingerprintRuleSet, error) {
	builtin, err := parseServiceFingerprintYAML(builtinServiceFingerprintYAML, "内置 fingerprints/services.yaml")
	if err != nil {
		return nil, err
	}
	merged := builtin
	if path := strings.TrimSpace(extraPath); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("SERVICE_FINGERPRINT_RULES: %w", err)
		}
		extra, err := parseServiceFingerprintYAML(data, path)
		if err != nil {
			return nil, err
		}
		merged = mergeServiceFingerprintRules(builtin, extra)
	}
	return newServiceFingerprintRuleSet(merged)
}

func parseServiceFingerprintYAML(data []byte, source string) ([]serviceFingerprintRule, error) {
	var doc serviceFingerprintDocument
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if doc.Version != 0 && doc.Version != serviceFingerprintDocVersion {
		return nil, fmt.Errorf("%s: unsupported version %d", source, doc.Version)
	}
	if len(doc.Rules) == 0 {
		return nil, fmt.Errorf("%s: rules is empty", source)
	}
	out := make([]serviceFingerprintRule, 0, len(doc.Rules))
	seen := map[string]int{}
	for i, raw := range doc.Rules {
		r, err := compileServiceFingerprintRule(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: rule %d: %w", source, i+1, err)
		}
		if prev, ok := seen[r.Name]; ok {
			return nil, fmt.Errorf("%s: duplicate rule name %q (rules %d and %d)", source, r.Name, prev+1, i+1)
		}
		seen[r.Name] = i
		out = append(out, r)
	}
	return out, nil
}

func compileServiceFingerprintRule(raw serviceFingerprintYAMLRule) (serviceFingerprintRule, error) {
	name := strings.ToLower(strings.TrimSpace(raw.Name))
	if !serviceFingerprintNameRE.MatchString(name) {
		return serviceFingerprintRule{}, fmt.Errorf("invalid name %q", raw.Name)
	}
	component := strings.TrimSpace(raw.Component)
	if component == "" {
		component = name
	}
	category := strings.TrimSpace(raw.Category)
	if category == "" {
		return serviceFingerprintRule{}, fmt.Errorf("rule %q: category required", name)
	}
	confidence := strings.ToLower(strings.TrimSpace(raw.Confidence))
	switch confidence {
	case serviceConfidenceHigh, serviceConfidenceMedium, serviceConfidenceLow:
	default:
		return serviceFingerprintRule{}, fmt.Errorf("rule %q: invalid confidence %q", name, raw.Confidence)
	}
	// 官方仓库仅用于「源码可得性」评分；非法 owner/repo 视为规则错误，启动即拒绝。
	repo := strings.TrimSpace(raw.Repo)
	if repo != "" {
		if _, err := parseGitHubRepo(repo); err != nil {
			return serviceFingerprintRule{}, fmt.Errorf("rule %q: repo must be owner/repo", name)
		}
	}
	if len(raw.Ports) == 0 {
		return serviceFingerprintRule{}, fmt.Errorf("rule %q: ports required", name)
	}
	ports := make([]int, 0, len(raw.Ports))
	seenPort := map[int]struct{}{}
	for _, p := range raw.Ports {
		if p < 1 || p > 65535 {
			return serviceFingerprintRule{}, fmt.Errorf("rule %q: port %d out of range 1-65535", name, p)
		}
		if _, ok := seenPort[p]; ok {
			continue
		}
		seenPort[p] = struct{}{}
		ports = append(ports, p)
	}
	banner := strings.TrimSpace(raw.Banner)
	if banner == "" {
		return serviceFingerprintRule{}, fmt.Errorf("rule %q: banner regex required", name)
	}
	bannerRE, err := regexp.Compile(banner)
	if err != nil {
		return serviceFingerprintRule{}, fmt.Errorf("rule %q: banner regex: %w", name, err)
	}
	var versionRE *regexp.Regexp
	if v := strings.TrimSpace(raw.Version); v != "" {
		versionRE, err = regexp.Compile(v)
		if err != nil {
			return serviceFingerprintRule{}, fmt.Errorf("rule %q: version regex: %w", name, err)
		}
		if versionRE.NumSubexp() < 1 {
			return serviceFingerprintRule{}, fmt.Errorf("rule %q: version regex needs a capture group", name)
		}
	}
	probes := make([]serviceFingerprintProbe, 0, len(raw.Probes))
	for i, p := range raw.Probes {
		probe, err := compileServiceFingerprintProbe(p)
		if err != nil {
			return serviceFingerprintRule{}, fmt.Errorf("rule %q: probe %d: %w", name, i+1, err)
		}
		probes = append(probes, probe)
	}
	return serviceFingerprintRule{
		Name:       name,
		Component:  component,
		Category:   category,
		Confidence: confidence,
		Repo:       repo,
		Ports:      ports,
		Probes:     probes,
		BannerRE:   bannerRE,
		VersionRE:  versionRE,
	}, nil
}

func compileServiceFingerprintProbe(raw serviceFingerprintYAMLProbe) (serviceFingerprintProbe, error) {
	text := raw.Text != ""
	hexText := strings.TrimSpace(raw.Hex) != ""
	if text == hexText {
		return serviceFingerprintProbe{}, fmt.Errorf("exactly one of text or hex is required")
	}
	var b []byte
	if text {
		b = []byte(raw.Text)
	} else {
		h := strings.TrimSpace(raw.Hex)
		h = strings.TrimPrefix(h, "0x")
		h = strings.TrimPrefix(h, "0X")
		h = strings.Join(strings.Fields(h), "")
		decoded, err := hex.DecodeString(h)
		if err != nil {
			return serviceFingerprintProbe{}, fmt.Errorf("invalid hex: %w", err)
		}
		b = decoded
	}
	if len(b) == 0 {
		return serviceFingerprintProbe{}, fmt.Errorf("probe bytes required")
	}
	if len(b) > serviceFingerprintMaxProbeSize {
		return serviceFingerprintProbe{}, fmt.Errorf("probe bytes %d exceed %d", len(b), serviceFingerprintMaxProbeSize)
	}
	expect := strings.TrimSpace(raw.Expect)
	if expect == "" {
		return serviceFingerprintProbe{}, fmt.Errorf("expect regex required")
	}
	expectRE, err := regexp.Compile(expect)
	if err != nil {
		return serviceFingerprintProbe{}, fmt.Errorf("expect regex: %w", err)
	}
	return serviceFingerprintProbe{Bytes: b, Expect: expectRE}, nil
}

func mergeServiceFingerprintRules(base, extra []serviceFingerprintRule) []serviceFingerprintRule {
	out := append([]serviceFingerprintRule(nil), base...)
	idx := make(map[string]int, len(out))
	for i, r := range out {
		idx[r.Name] = i
	}
	for _, r := range extra {
		if i, ok := idx[r.Name]; ok {
			out[i] = r
			continue
		}
		idx[r.Name] = len(out)
		out = append(out, r)
	}
	return out
}

func newServiceFingerprintRuleSet(rules []serviceFingerprintRule) (*serviceFingerprintRuleSet, error) {
	if len(rules) == 0 {
		return nil, fmt.Errorf("fingerprint rules is empty")
	}
	rs := &serviceFingerprintRuleSet{
		Rules:  append([]serviceFingerprintRule(nil), rules...),
		byName: make(map[string]int, len(rules)),
		byPort: make(map[int]int),
	}
	for i := range rs.Rules {
		r := &rs.Rules[i]
		if _, ok := rs.byName[r.Name]; ok {
			return nil, fmt.Errorf("duplicate rule name %q", r.Name)
		}
		rs.byName[r.Name] = i
		for _, p := range r.Ports {
			if _, ok := rs.byPort[p]; !ok {
				rs.byPort[p] = i
			}
		}
	}
	return rs, nil
}

func (rs *serviceFingerprintRuleSet) ruleByName(name string) (serviceFingerprintRule, bool) {
	if rs == nil {
		return serviceFingerprintRule{}, false
	}
	i, ok := rs.byName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return serviceFingerprintRule{}, false
	}
	return rs.Rules[i], true
}

// ruleForPort 返回该端口的第一条规则（含其 probes）；同一端口多条规则时以先声明者为准。
func (rs *serviceFingerprintRuleSet) ruleForPort(port int) serviceFingerprintRule {
	if rs == nil {
		return serviceFingerprintRule{}
	}
	if i, ok := rs.byPort[port]; ok {
		return rs.Rules[i]
	}
	return serviceFingerprintRule{}
}

func (rs *serviceFingerprintRuleSet) componentName(name string) string {
	if r, ok := rs.ruleByName(name); ok {
		return r.Component
	}
	return ""
}

func (rs *serviceFingerprintRuleSet) category(name string) string {
	if r, ok := rs.ruleByName(name); ok {
		return r.Category
	}
	return ""
}

// repo 返回规则声明的官方仓库（owner/repo）；未声明时为空。
func (rs *serviceFingerprintRuleSet) repo(name string) string {
	if r, ok := rs.ruleByName(name); ok {
		return r.Repo
	}
	return ""
}

func (rs *serviceFingerprintRuleSet) confidence(name string) string {
	if r, ok := rs.ruleByName(name); ok {
		return r.Confidence
	}
	return ""
}

// ports 返回规则声明的全部端口（按规则顺序去重），作为默认扫描端口集。
func (rs *serviceFingerprintRuleSet) ports() []int {
	if rs == nil {
		return nil
	}
	seen := map[int]struct{}{}
	var out []int
	for _, r := range rs.Rules {
		for _, p := range r.Ports {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// probes 返回全部声明的探测字节（去重），供测试断言发送字节集。
func (rs *serviceFingerprintRuleSet) probes() [][]byte {
	if rs == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out [][]byte
	for _, r := range rs.Rules {
		for _, p := range r.Probes {
			k := string(p.Bytes)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, append([]byte(nil), p.Bytes...))
		}
	}
	return out
}

// match 按 YAML 规则识别 banner。端口命中优先，否则按声明顺序回退第一条 banner 命中。
// MySQL/MariaDB 仍走协议握手解析，保证 5.5.5-10.11.6-MariaDB 取 10.11.6。
func (rs *serviceFingerprintRuleSet) match(port int, banner []byte) (name, version, confidence string) {
	if rs == nil || len(banner) == 0 {
		return "", "", ""
	}
	if n, v, ok := parseMySQLHandshake(banner); ok {
		return n, v, rs.confidence(n)
	}
	text := bannerText(banner)
	raw := string(banner)
	var fallbackName, fallbackVersion string
	for i := range rs.Rules {
		r := &rs.Rules[i]
		if r.BannerRE == nil || (!r.BannerRE.Match(banner) && !r.BannerRE.MatchString(text)) {
			continue
		}
		ver := serviceFingerprintVersion(r, text, raw)
		if hasInt(r.Ports, port) {
			return r.Name, ver, r.Confidence
		}
		if fallbackName == "" {
			fallbackName, fallbackVersion = r.Name, ver
		}
	}
	if fallbackName != "" {
		return fallbackName, fallbackVersion, rs.confidence(fallbackName)
	}
	return "", "", ""
}

func serviceFingerprintVersion(r *serviceFingerprintRule, text, raw string) string {
	if r == nil || r.VersionRE == nil {
		return ""
	}
	// 已脱敏的历史 banner 会把 5.5.5-10.11.6 变成 .5.5.5-10.11.6，显式取 MariaDB 真版本。
	if strings.Contains(strings.ToLower(text), "mariadb") {
		if m := serviceFingerprintMariaDBVerRE.FindStringSubmatch(text); len(m) > 1 {
			return m[1]
		}
	}
	if m := r.VersionRE.FindStringSubmatch(text); len(m) > 1 {
		return m[1]
	}
	if m := r.VersionRE.FindStringSubmatch(raw); len(m) > 1 {
		return m[1]
	}
	return ""
}

func hasInt(nums []int, want int) bool {
	for _, n := range nums {
		if n == want {
			return true
		}
	}
	return false
}
