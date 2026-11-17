package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// T63：AI 写回。岸线只按规则推断 confidence 与候选仓库；确认仓库、审计优先级与理由由 AI 写回。
// 幂等语义：ai_review.by_asset 以 asset_id（未给 asset 时用 "component"）为键，同一 component+asset
// 重复提交覆盖该条，不会堆出多条；除 updated_at 外，同参数重复提交内容一致。
const (
	aiReviewComponentKey = "component"
	aiReviewReasonMax    = 2000
)

// aiReviewIn 是 REST 与 MCP 共用的写回入参（REST 用 json，MCP 用 jsonschema）。
type aiReviewIn struct {
	ComponentID    int64  `json:"component_id" jsonschema:"required; component id from search_components"`
	AssetID        *int64 `json:"asset_id,omitempty" jsonschema:"optional; asset id this review applies to (one entry per component+asset)"`
	Repo           string `json:"repo,omitempty" jsonschema:"optional; owner/repo to confirm; empty keeps components.github_repo unchanged"`
	RepoConfidence string `json:"repo_confidence,omitempty" jsonschema:"optional; high | medium | low"`
	AuditPriority  string `json:"audit_priority,omitempty" jsonschema:"optional; P0 | P1 | P2 | P3"`
	Reason         string `json:"reason,omitempty" jsonschema:"optional; why this repo/priority; max 2000 characters"`
}

func validRepoConfidence(s string) bool {
	switch s {
	case "", confidenceHigh, confidenceMedium, confidenceLow:
		return true
	}
	return false
}

func validAuditPriority(s string) bool {
	switch s {
	case "", "P0", "P1", "P2", "P3":
		return true
	}
	return false
}

// normalizeAIReview 校验并归一入参：repo 走 owner/repo 校验（空表示不改），枚举非法返回 400。
func normalizeAIReview(in aiReviewIn) (aiReviewIn, error) {
	if in.ComponentID < 1 {
		return in, errf(400, "component_id is required")
	}
	if in.AssetID != nil && *in.AssetID < 1 {
		return in, errf(400, "asset_id must be >= 1")
	}
	repo, err := parseGitHubRepo(in.Repo)
	if err != nil {
		return in, err
	}
	in.Repo = repo
	in.RepoConfidence = strings.ToLower(strings.TrimSpace(in.RepoConfidence))
	if !validRepoConfidence(in.RepoConfidence) {
		return in, errf(400, "repo_confidence must be high, medium, or low")
	}
	in.AuditPriority = strings.ToUpper(strings.TrimSpace(in.AuditPriority))
	if !validAuditPriority(in.AuditPriority) {
		return in, errf(400, "audit_priority must be P0, P1, P2, or P3")
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > aiReviewReasonMax {
		return in, errf(400, "reason must be at most %d characters", aiReviewReasonMax)
	}
	return in, nil
}

// writeAIReview 合并写回 ai_review 并更新 components 的仓库/置信度，返回更新后的组件行。
func (a *api) writeAIReview(ctx context.Context, in aiReviewIn) (map[string]any, error) {
	in, err := normalizeAIReview(in)
	if err != nil {
		return nil, err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var raw []byte
	var currentRepo string
	if err := tx.QueryRow(ctx, `SELECT ai_review, COALESCE(github_repo, '') FROM components WHERE id = $1 FOR UPDATE`,
		in.ComponentID).Scan(&raw, &currentRepo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errf(404, "not found")
		}
		return nil, err
	}
	if in.AssetID != nil {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1)`, *in.AssetID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, errf(404, "asset not found")
		}
	}

	// 旧值可能是历史 {} 或人工写入的结构；解析失败只重置，不影响写回。
	review := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &review); err != nil || review == nil {
			review = map[string]any{}
		}
	}
	byAsset, _ := review["by_asset"].(map[string]any)
	if byAsset == nil {
		byAsset = map[string]any{}
	}
	key := aiReviewComponentKey
	entry := map[string]any{}
	if in.AssetID != nil {
		key = strconv.FormatInt(*in.AssetID, 10)
		entry["asset_id"] = *in.AssetID
	}
	if in.Repo != "" {
		entry["repo"] = in.Repo
	}
	if in.RepoConfidence != "" {
		entry["repo_confidence"] = in.RepoConfidence
	}
	if in.AuditPriority != "" {
		entry["audit_priority"] = in.AuditPriority
	}
	if in.Reason != "" {
		entry["reason"] = in.Reason
	}
	now := time.Now().UTC().Format(time.RFC3339)
	entry["updated_at"] = now
	byAsset[key] = entry
	review["by_asset"] = byAsset
	review["updated_at"] = now
	merged, err := json.Marshal(review)
	if err != nil {
		return nil, err
	}

	// 仓库真变了才清空 star/披露数，避免旧仓库的富集结果挂在新仓库上；富集仍由
	// PATCH /api/components/{id} 或后续写回触发，本接口不联网。
	repoChanged := in.Repo != "" && !strings.EqualFold(in.Repo, currentRepo)
	if _, err := tx.Exec(ctx, `UPDATE components SET
			github_repo = CASE WHEN $2::text <> '' THEN $2 ELSE github_repo END,
			repo_confidence = CASE WHEN $3::text <> '' THEN $3 ELSE repo_confidence END,
			stars = CASE WHEN $5::bool THEN NULL ELSE stars END,
			advisory_count = CASE WHEN $5::bool THEN NULL ELSE advisory_count END,
			enrich_error = CASE WHEN $5::bool THEN NULL ELSE enrich_error END,
			enriched_at = CASE WHEN $5::bool THEN NULL ELSE enriched_at END,
			ai_review = $4::jsonb,
			updated_at = now()
		WHERE id = $1`, in.ComponentID, in.Repo, in.RepoConfidence, merged, repoChanged); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	list, err := rowsJSON(a.db.Query(ctx, `SELECT id, name, github_repo, repo_confidence, ai_review, stars, advisory_count, updated_at
		FROM components WHERE id = $1`, in.ComponentID))
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errf(404, "not found")
	}
	return list[0], nil
}

// aiReviewComponent：POST /api/components/ai-review，鉴权同现有 Bearer。
func (a *api) aiReviewComponent(w http.ResponseWriter, r *http.Request) {
	var in aiReviewIn
	if !decode(w, r, &in) {
		return
	}
	out, err := a.writeAIReview(r.Context(), in)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}
