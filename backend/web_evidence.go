package main

import (
	"regexp"
	"sort"
	"strings"
)

// T61：组件证据类型（SPEC 第十一轮）。meta 为 <meta name="generator"> 命中，body 为正文结构匹配；
// banner 沿用 T53 的服务指纹证据；status / favicon / llm 保留给只按状态码、favicon hash、模型推断
// 产生证据的路径（当前只由 LLM 回写产生 llm）。
const (
	evidencePath    = "path"
	evidenceHeader  = "header"
	evidenceMeta    = "meta"
	evidenceBody    = "body"
	evidenceTitle   = "title"
	evidenceJS      = "js"
	evidenceFavicon = "favicon"
	evidenceStatus  = "status"
	evidenceLLM     = "llm"
	evidenceBanner  = "banner"
)

const (
	confidenceHigh   = serviceConfidenceHigh
	confidenceMedium = serviceConfidenceMedium
	confidenceLow    = serviceConfidenceLow
)

// evidenceSnippetMax：证据片段上限，避免把整段正文写进库。
const evidenceSnippetMax = 120

// componentEvidence：url_components / asset_components.evidence 的 T61 结构。
type componentEvidence struct {
	Type    string `json:"type"`
	Value   string `json:"value"`
	Snippet string `json:"snippet"`
}

// techMatchEvidence：一次 Web 识别命中的证据，以及由证据类型推出的置信度。
type techMatchEvidence struct {
	Evidence   componentEvidence
	Confidence string
}

// evidenceConfidence：明确 path / 响应头 / meta generator / JS 包名 / favicon hash 记 high；
// 标题与正文结构记 medium；仅 Server 头、LLM 推断与无证据记 low。
func evidenceConfidence(typ string, serverHeader bool) string {
	switch typ {
	case evidencePath, evidenceHeader, evidenceMeta, evidenceJS, evidenceFavicon:
		if serverHeader {
			return confidenceLow
		}
		return confidenceHigh
	case evidenceTitle, evidenceBody, evidenceStatus:
		return confidenceMedium
	}
	return confidenceLow
}

func evidenceSnippet(s string) string {
	return clipRunes(strings.Join(strings.Fields(s), " "), evidenceSnippetMax)
}

// bodySnippet 取命中位置前后的一段正文，只截断不改写。
func bodySnippet(body string, idx, n int) string {
	if idx < 0 || idx >= len(body) {
		return ""
	}
	lo := max(0, idx-evidenceSnippetMax/2)
	hi := min(len(body), idx+n+evidenceSnippetMax/2)
	return evidenceSnippet(validUTF8(body[lo:hi]))
}

var (
	metaTagRe     = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	scriptTagRe   = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	resourceTagRe = regexp.MustCompile(`(?is)<(?:link|a|img|iframe|form|source|object|embed|video|audio|area|base)\b[^>]*>`)
)

// tagAttrs 解析单个标签的属性名（小写）与去引号的值；同名属性取第一次出现。
func tagAttrs(tag string) map[string]string {
	attrs := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		k := strings.ToLower(m[1])
		v := m[2]
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, ok := attrs[k]; !ok {
			attrs[k] = v
		}
	}
	return attrs
}

// deriveTechEvidence 对一次识别出的全部组件反推证据；正文只归一化一次。
func deriveTechEvidence(r *probeResult, names []string) map[string]techMatchEvidence {
	if r == nil || len(names) == 0 {
		return nil
	}
	body := string(r.body)
	lowerBody := strings.ToLower(body)
	lowerTitle := strings.ToLower(r.Title)
	out := make(map[string]techMatchEvidence, len(names))
	for _, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if ev, ok := deriveWebEvidence(r, key, lowerTitle, body, lowerBody); ok {
			out[key] = ev
		}
	}
	return out
}

// webEvidenceFor 反推单个组件的证据；找不到来源时 ok=false（调用方按 low 落库，evidence 记空）。
func webEvidenceFor(r *probeResult, name string) (techMatchEvidence, bool) {
	if r == nil {
		return techMatchEvidence{}, false
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return techMatchEvidence{}, false
	}
	body := string(r.body)
	return deriveWebEvidence(r, key, strings.ToLower(r.Title), body, strings.ToLower(body))
}

// deriveWebEvidence 在已取回的响应里找组件名出现的位置（needle 为小写组件名）。
// 优先级：非 Server 响应头 > meta generator > script 包名 > 资源 path > 标题 > 正文 > 仅 Server 头。
// 找不到来源返回 false，不编造证据。
func deriveWebEvidence(r *probeResult, needle, lowerTitle, body, lowerBody string) (techMatchEvidence, bool) {
	keys := make([]string, 0, len(r.Headers))
	for k := range r.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var serverHit componentEvidence
	for _, k := range keys {
		v := strings.TrimSpace(r.Headers[k])
		if v == "" || !strings.Contains(strings.ToLower(v), needle) {
			continue
		}
		ev := componentEvidence{Type: evidenceHeader, Value: clipRunes(k+": "+v, 253), Snippet: evidenceSnippet(v)}
		if strings.EqualFold(k, "Server") {
			if serverHit.Type == "" {
				serverHit = ev
			}
			continue
		}
		return techMatchEvidence{Evidence: ev, Confidence: evidenceConfidence(evidenceHeader, false)}, true
	}

	for _, tag := range metaTagRe.FindAllString(body, -1) {
		attrs := tagAttrs(tag)
		content := strings.TrimSpace(attrs["content"])
		if !strings.EqualFold(attrs["name"], "generator") || !strings.Contains(strings.ToLower(content), needle) {
			continue
		}
		return techMatchEvidence{
			Evidence:   componentEvidence{Type: evidenceMeta, Value: evidenceSnippet(content), Snippet: evidenceSnippet(tag)},
			Confidence: evidenceConfidence(evidenceMeta, false),
		}, true
	}

	for _, m := range scriptTagRe.FindAllStringSubmatch(body, -1) {
		if src := strings.TrimSpace(tagAttrs(m[1])["src"]); src != "" && strings.Contains(strings.ToLower(src), needle) {
			return techMatchEvidence{
				Evidence:   componentEvidence{Type: evidenceJS, Value: clipRunes(src, 253), Snippet: evidenceSnippet(m[0])},
				Confidence: evidenceConfidence(evidenceJS, false),
			}, true
		}
		if content := m[2]; strings.Contains(strings.ToLower(content), needle) {
			idx := strings.Index(strings.ToLower(content), needle)
			return techMatchEvidence{
				Evidence:   componentEvidence{Type: evidenceJS, Value: "", Snippet: bodySnippet(content, idx, len(needle))},
				Confidence: confidenceMedium, // 内联脚本只有结构匹配，不是打包文件路径
			}, true
		}
	}

	for _, tag := range resourceTagRe.FindAllString(body, -1) {
		attrs := tagAttrs(tag)
		for _, k := range []string{"href", "src", "action", "data"} {
			v := strings.TrimSpace(attrs[k])
			if v == "" || !strings.Contains(strings.ToLower(v), needle) {
				continue
			}
			return techMatchEvidence{
				Evidence:   componentEvidence{Type: evidencePath, Value: clipRunes(v, 253), Snippet: evidenceSnippet(tag)},
				Confidence: evidenceConfidence(evidencePath, false),
			}, true
		}
	}

	if strings.Contains(lowerTitle, needle) {
		return techMatchEvidence{
			Evidence:   componentEvidence{Type: evidenceTitle, Value: evidenceSnippet(r.Title), Snippet: evidenceSnippet(r.Title)},
			Confidence: evidenceConfidence(evidenceTitle, false),
		}, true
	}

	if idx := strings.Index(lowerBody, needle); idx >= 0 {
		return techMatchEvidence{
			Evidence:   componentEvidence{Type: evidenceBody, Value: "", Snippet: bodySnippet(body, idx, len(needle))},
			Confidence: evidenceConfidence(evidenceBody, false),
		}, true
	}

	if serverHit.Type != "" {
		return techMatchEvidence{Evidence: serverHit, Confidence: evidenceConfidence(evidenceHeader, true)}, true
	}
	return techMatchEvidence{}, false
}

// identifyHitEvidence：标题规则记 title/medium，Server 头记 header/low，其它响应头记 header/high。
func identifyHitEvidence(hit identifyHit) (componentEvidence, string) {
	if hit.Source == "title" {
		ev := componentEvidence{Type: evidenceTitle, Value: evidenceSnippet(hit.Value), Snippet: evidenceSnippet(hit.Value)}
		return ev, evidenceConfidence(evidenceTitle, false)
	}
	ev := componentEvidence{Type: evidenceHeader, Snippet: evidenceSnippet(hit.Value)}
	if hit.Header != "" {
		ev.Value = clipRunes(hit.Header+": "+strings.TrimSpace(hit.Value), 253)
	} else {
		ev.Value = evidenceSnippet(hit.Value)
	}
	return ev, evidenceConfidence(evidenceHeader, strings.EqualFold(hit.Header, "Server"))
}
