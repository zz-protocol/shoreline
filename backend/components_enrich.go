package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	defaultGitHubAPI = "https://api.github.com"
	defaultOSVAPI    = "https://api.osv.dev"
	enrichUA         = "Shoreline-ComponentEnrich"
)

// owner/repo：恰好一段斜杠，允许 GitHub 常见字符；拒绝 URL 与多余路径。
var githubRepoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

func parseGitHubRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.Contains(s, "://") || strings.ContainsAny(s, " \t\n") || strings.Count(s, "/") != 1 || !githubRepoRe.MatchString(s) {
		return "", errf(400, "github_repo must be owner/repo")
	}
	parts := strings.SplitN(s, "/", 2)
	if parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", errf(400, "github_repo must be owner/repo")
	}
	return s, nil
}

func (a *api) enrichClient() *http.Client {
	if a.enrichHTTP != nil {
		return a.enrichHTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (a *api) githubAPIBase() string {
	if a.githubBase != "" {
		return strings.TrimRight(a.githubBase, "/")
	}
	return defaultGitHubAPI
}

func (a *api) osvAPIBase() string {
	if a.osvBase != "" {
		return strings.TrimRight(a.osvBase, "/")
	}
	return defaultOSVAPI
}

func (a *api) scheduleEnrich(id int64) {
	run := func() { _ = a.enrichComponent(a.bg, id) }
	if a.enrichSync {
		run()
		return
	}
	go run()
}

func clipErr(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > 500 {
		return msg[:500]
	}
	return msg
}

func (a *api) enrichComponent(ctx context.Context, id int64) error {
	var repo string
	if err := a.db.QueryRow(ctx, `SELECT COALESCE(github_repo, '') FROM components WHERE id = $1`, id).Scan(&repo); err != nil {
		return err
	}
	if repo == "" {
		return nil
	}
	stars, errGH := a.fetchGitHubStars(ctx, repo)
	advisories, errOSV := a.fetchOSVAdvisories(ctx, repo)
	switch {
	case errGH != nil && errOSV != nil:
		_, err := a.db.Exec(ctx, `UPDATE components SET enrich_error = $2, enriched_at = now(), updated_at = now() WHERE id = $1`,
			id, clipErr(errGH.Error()+"; "+errOSV.Error()))
		if err != nil {
			return err
		}
		return errGH
	case errGH != nil:
		_, err := a.db.Exec(ctx, `UPDATE components SET advisory_count = $2, enrich_error = $3, enriched_at = now(), updated_at = now() WHERE id = $1`,
			id, advisories, clipErr(errGH.Error()))
		if err != nil {
			return err
		}
		return errGH
	case errOSV != nil:
		_, err := a.db.Exec(ctx, `UPDATE components SET stars = $2, enrich_error = $3, enriched_at = now(), updated_at = now() WHERE id = $1`,
			id, stars, clipErr(errOSV.Error()))
		if err != nil {
			return err
		}
		return errOSV
	default:
		_, err := a.db.Exec(ctx, `UPDATE components SET stars = $2, advisory_count = $3, enrich_error = NULL, enriched_at = now(), updated_at = now() WHERE id = $1`,
			id, stars, advisories)
		return err
	}
}

func (a *api) fetchGitHubStars(ctx context.Context, repo string) (int, error) {
	u := a.githubAPIBase() + "/repos/" + repo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", enrichUA)
	if tok := strings.TrimSpace(a.cfg.GitHubToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := a.enrichClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Stars int `json:"stargazers_count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("github: invalid json")
	}
	return out.Stars, nil
}

func (a *api) fetchOSVAdvisories(ctx context.Context, repo string) (int, error) {
	seen := map[string]struct{}{}
	token := ""
	for range 10 {
		payload := map[string]any{"package": map[string]string{"purl": "pkg:github/" + repo}}
		if token != "" {
			payload["page_token"] = token
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.osvAPIBase()+"/v1/query", bytes.NewReader(raw))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", enrichUA)
		resp, err := a.enrichClient().Do(req)
		if err != nil {
			return 0, fmt.Errorf("osv: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("osv: HTTP %d", resp.StatusCode)
		}
		var out struct {
			Vulns []struct {
				ID string `json:"id"`
			} `json:"vulns"`
			Next string `json:"next_page_token"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return 0, fmt.Errorf("osv: invalid json")
		}
		for _, v := range out.Vulns {
			if v.ID != "" {
				seen[v.ID] = struct{}{}
			}
		}
		if out.Next == "" {
			break
		}
		token = out.Next
	}
	return len(seen), nil
}
