package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// T32 挖掘认领：claim / heartbeat / complete；冷却与认领 TTL 来自环境变量。

const (
	defaultMineCooldownDays  = 30
	defaultMineClaimTTLHours = 24
	mineClaimDefaultLimit    = 20
	mineClaimMaxLimit        = 100
)

func apiConfigFromEnvMine(c *apiConfig) error {
	c.MineCooldownDays = defaultMineCooldownDays
	c.MineClaimTTLHours = defaultMineClaimTTLHours
	if v := os.Getenv("MINE_COOLDOWN_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("MINE_COOLDOWN_DAYS must be an integer >= 0, got %q", v)
		}
		c.MineCooldownDays = n
	}
	if v := os.Getenv("MINE_CLAIM_TTL_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("MINE_CLAIM_TTL_HOURS must be an integer >= 1, got %q", v)
		}
		c.MineClaimTTLHours = n
	}
	return nil
}

func (c apiConfig) mineClaimTTL() time.Duration {
	h := c.MineClaimTTLHours
	if h < 1 {
		h = defaultMineClaimTTLHours
	}
	return time.Duration(h) * time.Hour
}

// claimablePred：默认可领（$1 = include_abandoned）。alive、非 customer_hosted；
// unset，或 done/deferred 且冷却已过，或 claimed 且锁已过期；abandoned 仅在 include_abandoned 时。
const claimablePred = `a.alive = 'alive' AND NOT a.customer_hosted AND (
		a.mine_status = 'unset'
		OR (a.mine_status IN ('done', 'deferred') AND a.mine_cooldown_until IS NOT NULL AND a.mine_cooldown_until <= now())
		OR (a.mine_status = 'claimed' AND a.mine_claim_expires_at IS NOT NULL AND a.mine_claim_expires_at <= now())
		OR ($1::bool AND a.mine_status = 'abandoned')
	)`

type mineClaimIn struct {
	ClaimedBy        string `json:"claimed_by"`
	Limit            *int   `json:"limit"`
	VendorID         *int64 `json:"vendor_id"`
	Since            string `json:"since"`
	Q                string `json:"q"`
	IncludeAbandoned bool   `json:"include_abandoned"`
}

type mineHeartbeatIn struct {
	AssetIDs []int64 `json:"asset_ids"`
}

type mineCompleteIn struct {
	AssetIDs  []int64 `json:"asset_ids"`
	Status    string  `json:"status"`
	Note      string  `json:"note"`
	ReportURL string  `json:"report_url"`
}

func (a *api) claimMine(w http.ResponseWriter, r *http.Request) {
	var in mineClaimIn
	if !decode(w, r, &in) {
		return
	}
	by := strings.TrimSpace(in.ClaimedBy)
	if by == "" {
		writeErr(w, 400, "claimed_by is required")
		return
	}
	if strings.ContainsRune(by, 0) || strings.ContainsRune(in.Q, 0) {
		writeErr(w, 400, "NUL character not allowed")
		return
	}
	limit := mineClaimDefaultLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	if limit < 1 || limit > mineClaimMaxLimit {
		writeErr(w, 400, fmt.Sprintf("limit must be 1..%d", mineClaimMaxLimit))
		return
	}
	if err := checkVendor(in.VendorID); err != nil {
		writeAnyErr(w, err)
		return
	}
	since, err := parseTime("since", in.Since)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	list, err := a.doClaimMine(r.Context(), by, limit, in.VendorID, since, in.Q, in.IncludeAbandoned)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": list, "claimed_by": by, "count": len(list)})
}

func (a *api) doClaimMine(ctx context.Context, by string, limit int, vendor *int64, since *time.Time, q string, includeAbandoned bool) ([]map[string]any, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	ttl := a.cfg.mineClaimTTL()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		WITH cand AS (
			SELECT a.id FROM assets a
			JOIN root_domains r ON r.id = a.root_id
			WHERE `+claimablePred+`
			  AND ($2::bigint IS NULL OR r.vendor_id = $2)
			  AND ($3::timestamptz IS NULL OR a.first_seen >= $3)
			  AND ($4::text = '' OR strpos(a.subdomain, $4) > 0)
			ORDER BY a.id
			LIMIT $5
			FOR UPDATE OF a SKIP LOCKED
		)
		UPDATE assets a SET
			mine_status = 'claimed',
			mine_claimed_by = $6,
			mine_claimed_at = now(),
			mine_claim_expires_at = now() + $7::interval
		FROM cand WHERE a.id = cand.id
		RETURNING a.id`,
		includeAbandoned, vendor, since, q, limit, by, ttl.String())
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []map[string]any{}, nil
	}
	// 提交后再读：WITH 修改 CTE 与主查询同快照，同语句里 SELECT 看不到 UPDATE 后的值。
	return rowsJSON(a.db.Query(ctx, `SELECT `+assetCols+` FROM assets a JOIN root_domains r ON r.id = a.root_id
		WHERE a.id = ANY($1) ORDER BY a.id`, ids))
}

func (a *api) heartbeatMine(w http.ResponseWriter, r *http.Request) {
	var in mineHeartbeatIn
	if !decode(w, r, &in) {
		return
	}
	if len(in.AssetIDs) == 0 {
		writeErr(w, 400, "asset_ids is required")
		return
	}
	if len(in.AssetIDs) > mineClaimMaxLimit {
		writeErr(w, 400, fmt.Sprintf("asset_ids at most %d", mineClaimMaxLimit))
		return
	}
	ttl := a.cfg.mineClaimTTL()
	tag, err := a.db.Exec(r.Context(), `
		UPDATE assets SET mine_claim_expires_at = now() + $2::interval
		WHERE id = ANY($1) AND mine_status = 'claimed'`, in.AssetIDs, ttl.String())
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"renewed": tag.RowsAffected()})
}

func (a *api) completeMine(w http.ResponseWriter, r *http.Request) {
	var in mineCompleteIn
	if !decode(w, r, &in) {
		return
	}
	if len(in.AssetIDs) == 0 {
		writeErr(w, 400, "asset_ids is required")
		return
	}
	if len(in.AssetIDs) > mineClaimMaxLimit {
		writeErr(w, 400, fmt.Sprintf("asset_ids at most %d", mineClaimMaxLimit))
		return
	}
	st := strings.TrimSpace(in.Status)
	if st != "done" && st != "deferred" && st != "abandoned" {
		writeErr(w, 400, "status must be done|deferred|abandoned")
		return
	}
	if strings.ContainsRune(in.Note, 0) || strings.ContainsRune(in.ReportURL, 0) {
		writeErr(w, 400, "NUL character not allowed")
		return
	}
	if len(in.Note) > 2000 || len(in.ReportURL) > 2000 {
		writeErr(w, 400, "note and report_url must be at most 2000 characters")
		return
	}
	days := a.cfg.MineCooldownDays
	if days < 0 {
		days = defaultMineCooldownDays
	}
	var cooldown any
	if (st == "done" || st == "deferred") && days > 0 {
		cooldown = (time.Duration(days) * 24 * time.Hour).String()
	}
	tag, err := a.db.Exec(r.Context(), `
		UPDATE assets SET
			mine_status = $2,
			mine_note = $3,
			mine_report_url = $4,
			mine_completed_at = now(),
			mine_cooldown_until = CASE WHEN $5::text IS NULL THEN NULL ELSE now() + $5::interval END,
			mine_claimed_by = NULL,
			mine_claim_expires_at = NULL
		WHERE id = ANY($1)`, in.AssetIDs, st, in.Note, in.ReportURL, cooldown)
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"updated": tag.RowsAffected(), "status": st})
}
