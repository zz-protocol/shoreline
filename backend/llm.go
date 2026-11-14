package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

type llmConfig struct {
	URL   string
	Key   string
	Model string
	RPS   float64
}

func (c llmConfig) enabled() bool {
	return strings.TrimSpace(c.URL) != "" && strings.TrimSpace(c.Model) != ""
}

func (c llmConfig) endpoint() string {
	u := strings.TrimRight(strings.TrimSpace(c.URL), "/")
	if strings.HasSuffix(u, "/chat/completions") {
		return u
	}
	return u + "/chat/completions"
}

func llmConfigFromEnv() (llmConfig, error) {
	c := llmConfig{
		URL:   strings.TrimSpace(os.Getenv("LLM_API_URL")),
		Key:   strings.TrimSpace(os.Getenv("LLM_API_KEY")),
		Model: strings.TrimSpace(os.Getenv("LLM_MODEL")),
		RPS:   1,
	}
	if hasControl(c.URL) || hasControl(c.Key) || hasControl(c.Model) {
		return c, fmt.Errorf("LLM_API_URL/LLM_API_KEY/LLM_MODEL must not contain control characters")
	}
	if c.URL != "" {
		if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
			return c, fmt.Errorf("LLM_API_URL: invalid %q", c.URL)
		}
	}
	if s := os.Getenv("LLM_RPS"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if !validPositive(f, err) {
			return c, fmt.Errorf("LLM_RPS: invalid %q", s)
		}
		c.RPS = f
	}
	return c, nil
}

func (p *prober) withLLM(c llmConfig) *prober {
	p.llm = c
	if c.enabled() {
		if p.llmLim == nil {
			p.llmLim = rate.NewLimiter(rate.Limit(c.RPS), 1)
		}
		if p.llmHTTP == nil {
			p.llmHTTP = &http.Client{Timeout: 30 * time.Second}
		}
	}
	return p
}

const (
	llmTitleMax  = 200
	llmHeaderMax = 200
	llmReasonMax = 500
	llmNameMax   = 100
	llmMaxCands  = 5
	llmPromptMax = 2000
)

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || s == "" {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func llmEvidenceHash(host, path, title string, headers map[string]string, favicon string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "host=%s\npath=%s\ntitle=%s\nfavicon=%s\n", host, path, title, favicon)
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, headers[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// llmUserPrompt 只含标题、已记录头、favicon hash、主机与路径；不含正文、不含 IP。
func llmUserPrompt(host, path, title string, headers map[string]string, favicon string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Host: %s\nPath: %s\nTitle: %s\n", clipRunes(host, 253), clipRunes(path, 200), clipRunes(title, llmTitleMax))
	fmt.Fprintf(&b, "Favicon-SHA256: %s\nHeaders:\n", clipRunes(favicon, 64))
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, clipRunes(headers[k], llmHeaderMax))
	}
	s := b.String()
	if len(s) > llmPromptMax {
		return s[:llmPromptMax]
	}
	return s
}

type llmCandidate struct {
	Name       string `json:"name"`
	GitHubRepo string `json:"github_repo"`
	Reason     string `json:"reason"`
}

func parseLLMCandidates(raw string) []llmCandidate {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
		s = strings.TrimSpace(s)
	}
	var list []llmCandidate
	if err := json.Unmarshal([]byte(s), &list); err != nil {
		var one llmCandidate
		if err := json.Unmarshal([]byte(s), &one); err != nil {
			return nil
		}
		list = []llmCandidate{one}
	}
	out := make([]llmCandidate, 0, len(list))
	for _, c := range list {
		c.Name = clipRunes(c.Name, llmNameMax)
		c.Reason = clipRunes(c.Reason, llmReasonMax)
		c.GitHubRepo = strings.TrimSpace(c.GitHubRepo)
		if c.Name == "" {
			continue
		}
		if _, err := parseGitHubRepo(c.GitHubRepo); err != nil {
			c.GitHubRepo = ""
		} else {
			c.GitHubRepo, _ = parseGitHubRepo(c.GitHubRepo)
		}
		out = append(out, c)
		if len(out) >= llmMaxCands {
			break
		}
	}
	return out
}

func (p *prober) suggestLLM(ctx context.Context, d *pgxpool.Pool, r *probeResult) error {
	if p == nil || !p.llm.enabled() || r == nil {
		return nil
	}
	path := "/"
	hash := llmEvidenceHash(r.Host, path, r.Title, r.Headers, r.FaviconHash)
	var urlID int64
	var prev *string
	if err := d.QueryRow(ctx, `SELECT id, llm_evidence_hash FROM urls WHERE scheme = $1 AND host = $2 AND port = $3`,
		r.Scheme, r.Host, r.Port).Scan(&urlID, &prev); err != nil {
		return err
	}
	if prev != nil && *prev == hash {
		return nil
	}
	var hasUncommon bool
	if err := d.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM url_components uc
		JOIN components c ON c.id = uc.component_id
		WHERE uc.url_id = $1 AND uc.ignored_at IS NULL
		  AND NOT (`+commonExistsSQL+`)
	)`, urlID).Scan(&hasUncommon); err != nil {
		return err
	}
	if hasUncommon {
		return nil
	}
	if p.llmLim != nil {
		if err := p.llmLim.Wait(ctx); err != nil {
			return err
		}
	}
	cands, err := p.callLLM(ctx, llmUserPrompt(r.Host, path, r.Title, r.Headers, r.FaviconHash))
	if err != nil {
		return err
	}
	if err := storeLLMCandidates(ctx, d, urlID, cands); err != nil {
		return err
	}
	_, err = d.Exec(ctx, `UPDATE urls SET llm_evidence_hash = $2 WHERE id = $1`, urlID, hash)
	return err
}

func storeLLMCandidates(ctx context.Context, d *pgxpool.Pool, urlID int64, cands []llmCandidate) error {
	for _, c := range cands {
		var repo any
		if c.GitHubRepo != "" {
			repo = c.GitHubRepo
		}
		var id int64
		if err := d.QueryRow(ctx, `INSERT INTO components (name, name_key, source, github_repo, confirmed)
			VALUES ($1, $2, 'llm', $3, false)
			ON CONFLICT (name_key) DO UPDATE SET updated_at = now()
			RETURNING id`, c.Name, strings.ToLower(c.Name), repo).Scan(&id); err != nil {
			return err
		}
		ev, _ := json.Marshal(map[string]string{"reason": c.Reason, "github_repo": c.GitHubRepo})
		if _, err := d.Exec(ctx, `INSERT INTO url_components (url_id, component_id, version, source, evidence)
			VALUES ($1, $2, '', 'llm', $3)
			ON CONFLICT (url_id, component_id) DO UPDATE SET
				evidence = CASE WHEN url_components.source = 'wappalyzer' THEN url_components.evidence ELSE EXCLUDED.evidence END,
				source = CASE WHEN url_components.source = 'wappalyzer' THEN url_components.source ELSE EXCLUDED.source END,
				ignored_at = CASE WHEN url_components.source = 'wappalyzer' THEN url_components.ignored_at ELSE NULL END`,
			urlID, id, ev); err != nil {
			return err
		}
	}
	return nil
}

func (p *prober) callLLM(ctx context.Context, user string) ([]llmCandidate, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       p.llm.Model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": "Identify web application software from the given metadata. Reply with JSON only: {\"name\":\"component name\",\"github_repo\":\"owner/repo or empty\",\"reason\":\"short reason\"}. No body text was provided; do not invent IPs."},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.llm.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Shoreline-LLMSuggest")
	if tok := strings.TrimSpace(p.llm.Key); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := p.llmHTTP
	if client == nil {
		return nil, fmt.Errorf("llm client not configured")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("llm: invalid json")
	}
	if len(out.Choices) == 0 {
		return nil, nil
	}
	return parseLLMCandidates(out.Choices[0].Message.Content), nil
}

func (p *prober) suggestLLMLogged(ctx context.Context, d *pgxpool.Pool, scanID int64, r *probeResult) {
	if err := p.suggestLLM(ctx, d, r); err != nil {
		log.Printf("http scan %d: llm suggest %s://%s: %v", scanID, r.Scheme, r.Host, err)
	}
}
