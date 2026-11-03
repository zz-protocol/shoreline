package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP server（T13）：Streamable HTTP 挂在 /mcp，复用 API 的 Bearer token 认证；四个工具复用 T12 的查询逻辑。

type listNewAssetsIn struct {
	Q               string `json:"q,omitempty" jsonschema:"substring of subdomain or URL (case-insensitive)"`
	Vendor          *int64 `json:"vendor,omitempty" jsonschema:"only this vendor id (>= 1)"`
	Since           string `json:"since,omitempty" jsonschema:"RFC3339; only new events at or after this time"`
	IncludeWildcard bool   `json:"include_wildcard,omitempty" jsonschema:"include wildcard assets (default false)"`
	IncludeHosted   bool   `json:"include_customer_hosted,omitempty" jsonschema:"include customer-hosted cloud assets (default false)"`
	Limit           *int   `json:"limit,omitempty" jsonschema:"1..500, default 50"`
	Cursor          string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

type searchAssetsIn struct {
	Q               string `json:"q,omitempty" jsonschema:"substring of subdomain or URL title (case-insensitive)"`
	Vendor          *int64 `json:"vendor,omitempty" jsonschema:"only this vendor id (>= 1)"`
	InScope         *bool  `json:"in_scope,omitempty" jsonschema:"filter by in_scope"`
	NewSince        string `json:"new_since,omitempty" jsonschema:"RFC3339; asset new event at or after this time"`
	IncludeWildcard bool   `json:"include_wildcard,omitempty" jsonschema:"include wildcard assets (default false)"`
	IncludeHosted   bool   `json:"include_customer_hosted,omitempty" jsonschema:"include customer-hosted cloud assets (default false)"`
	Alive           string `json:"alive,omitempty" jsonschema:"filter by liveness: alive | dead | unknown"`
	AliveSince      string `json:"alive_since,omitempty" jsonschema:"RFC3339; last_alive_at >= this time"`
	AliveUntil      string `json:"alive_until,omitempty" jsonschema:"RFC3339; last_alive_at < this time (half-open)"`
	MineStatus      string `json:"mine_status,omitempty" jsonschema:"filter by mine status: unset|claimed|done|deferred|abandoned"`
	InCooldown      *bool  `json:"in_cooldown,omitempty" jsonschema:"if true, only assets still in cooldown; if false, no cooldown or expired"`
	Limit           *int   `json:"limit,omitempty" jsonschema:"1..500, default 50"`
	Cursor          string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

type claimMineIn struct {
	ClaimedBy        string `json:"claimed_by" jsonschema:"required; who is claiming (agent name), stored as mine_claimed_by"`
	Limit            *int   `json:"limit,omitempty" jsonschema:"1..100, default 20"`
	VendorID         *int64 `json:"vendor_id,omitempty" jsonschema:"only this vendor id (>= 1)"`
	Since            string `json:"since,omitempty" jsonschema:"RFC3339; first_seen >= this time (new assets)"`
	Q                string `json:"q,omitempty" jsonschema:"substring of subdomain (case-insensitive)"`
	IncludeAbandoned bool   `json:"include_abandoned,omitempty" jsonschema:"also claim abandoned assets (default false)"`
}

type heartbeatMineIn struct {
	AssetIDs []int64 `json:"asset_ids" jsonschema:"required; asset ids to renew claim lock"`
}

type completeMineIn struct {
	AssetIDs  []int64 `json:"asset_ids" jsonschema:"required; asset ids to mark complete"`
	Status    string  `json:"status" jsonschema:"required; done | deferred | abandoned"`
	Note      string  `json:"note,omitempty" jsonschema:"short conclusion, max 2000 chars"`
	ReportURL string  `json:"report_url,omitempty" jsonschema:"optional report URL, max 2000 chars"`
}

type checkScopeIn struct {
	Host string `json:"host" jsonschema:"host name to check, e.g. www.example.com"`
}

type triggerScanIn struct {
	RootID int64  `json:"root_id" jsonschema:"root domain id"`
	Kind   string `json:"kind" jsonschema:"passive | active | expand (active requires the vendor's active_enabled)"`
}

type pageOut struct {
	Items      []map[string]any `json:"items"`
	NextCursor any              `json:"next_cursor"`
}

type mapOut map[string]any

// parsePage：未传 limit 用默认 50；传了（含 0）按 REST 同样规则校验。
func parsePage(limit *int, cursor string) (int, int64, error) {
	l := 50
	if limit != nil {
		l = *limit
	}
	var c int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 1 {
			return 0, 0, errf(400, "invalid cursor")
		}
		c = n
	}
	return l, c, checkPage(l, c)
}

func parseTime(name, s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, errf(400, "%s must be RFC3339", name)
	}
	return &t, nil
}

// toolErr：业务错误作为工具错误结果返回（isError=true，文本为 {"error":...}），其它错误交给 SDK。
func toolErr[T any](err error) (*mcp.CallToolResult, T, error) {
	var zero T
	msg := err.Error()
	if _, ok := err.(*httpError); !ok {
		msg = "internal error"
	}
	b, _ := json.Marshal(map[string]string{"error": msg})
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, zero, nil
}

func (a *api) mcpServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "src-asset-monitor", Version: "1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "list_new_assets", Description: "List asset-level new events (newest first), each with nested url_events. Wildcard and customer-hosted assets are hidden by default."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listNewAssetsIn) (*mcp.CallToolResult, pageOut, error) {
			limit, cursor, err := parsePage(in.Limit, in.Cursor)
			if err != nil {
				return toolErr[pageOut](err)
			}
			since, err := parseTime("since", in.Since)
			if err != nil {
				return toolErr[pageOut](err)
			}
			list, err := a.queryEvents(ctx, eventFilter{Level: "asset", Type: "new", Since: since, Vendor: in.Vendor, IncludeWildcard: in.IncludeWildcard, IncludeHosted: in.IncludeHosted, Q: in.Q, Limit: limit, Cursor: cursor})
			if err != nil {
				return toolErr[pageOut](err)
			}
			list, next := cutPage(list, limit)
			return nil, pageOut{list, next}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "search_assets", Description: "Search assets (same filters as GET /api/assets). Wildcard and customer-hosted assets are hidden by default."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchAssetsIn) (*mcp.CallToolResult, pageOut, error) {
			limit, cursor, err := parsePage(in.Limit, in.Cursor)
			if err != nil {
				return toolErr[pageOut](err)
			}
			since, err := parseTime("new_since", in.NewSince)
			if err != nil {
				return toolErr[pageOut](err)
			}
			aliveSince, err := parseTime("alive_since", in.AliveSince)
			if err != nil {
				return toolErr[pageOut](err)
			}
			aliveUntil, err := parseTime("alive_until", in.AliveUntil)
			if err != nil {
				return toolErr[pageOut](err)
			}
			list, err := a.queryAssets(ctx, assetFilter{
				Vendor: in.Vendor, InScope: in.InScope, IncludeWildcard: in.IncludeWildcard, Q: in.Q,
				NewSince: since, Limit: limit, Cursor: cursor, Alive: in.Alive, IncludeHosted: in.IncludeHosted,
				AliveSince: aliveSince, AliveUntil: aliveUntil, MineStatus: in.MineStatus, InCooldown: in.InCooldown,
			})
			if err != nil {
				return toolErr[pageOut](err)
			}
			list, next := cutPage(list, limit)
			if err := attachAssetURLText(ctx, a.db, list); err != nil {
				return toolErr[pageOut](err)
			}
			return nil, pageOut{list, next}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "check_scope", Description: "Check whether a host is in scope: owning vendor/root (longest match), in_scope, matched rules, asset id if known."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in checkScopeIn) (*mcp.CallToolResult, mapOut, error) {
			out, err := a.checkScope(ctx, in.Host)
			if err != nil {
				return toolErr[mapOut](err)
			}
			return nil, out, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "trigger_scan", Description: "Manually trigger a scan for a root domain: passive | active | expand. active requires the vendor's active_enabled."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in triggerScanIn) (*mcp.CallToolResult, mapOut, error) {
			out, err := a.triggerScan(ctx, in.RootID, in.Kind)
			if err != nil {
				return toolErr[mapOut](err)
			}
			return nil, out, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_agent_guide", Description: "Return the Shoreline agent guide (markdown, same as docs/AGENT_GUIDE.md and GET /api/agent-guide): auth, claim -> heartbeat -> complete loop with curl and MCP examples, mine_status meanings, defaults (MINE_CLAIM_TTL_HOURS=24, MINE_COOLDOWN_DAYS=30), error codes, and a system prompt snippet. Read this first."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mapOut, error) {
			return nil, mapOut{"guide": string(agentGuideMD)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "claim_mine_targets", Description: "Claim up to N alive, non-customer-hosted assets for vulnerability mining (POST /api/mine/claim). Default candidates: mine_status unset, or done/deferred with cooldown expired, or claimed with claim TTL expired. Atomic (SKIP LOCKED): concurrent callers never get the same asset. Pass claimed_by (agent name). Optional: limit (default 20, max 100), vendor_id, since (first_seen), q, include_abandoned. Returns claimed assets with mine_* fields and claim_expires_at. Heartbeat before expiry; complete when done."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in claimMineIn) (*mcp.CallToolResult, mapOut, error) {
			by := strings.TrimSpace(in.ClaimedBy)
			if by == "" {
				return toolErr[mapOut](errf(400, "claimed_by is required"))
			}
			limit := mineClaimDefaultLimit
			if in.Limit != nil {
				limit = *in.Limit
			}
			if limit < 1 || limit > mineClaimMaxLimit {
				return toolErr[mapOut](errf(400, "limit must be 1..%d", mineClaimMaxLimit))
			}
			if err := checkVendor(in.VendorID); err != nil {
				return toolErr[mapOut](err)
			}
			since, err := parseTime("since", in.Since)
			if err != nil {
				return toolErr[mapOut](err)
			}
			list, err := a.doClaimMine(ctx, by, limit, in.VendorID, since, in.Q, in.IncludeAbandoned)
			if err != nil {
				return toolErr[mapOut](err)
			}
			return nil, mapOut{"items": list, "claimed_by": by, "count": len(list)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "heartbeat_mine", Description: "Renew claim lock on asset_ids (POST /api/mine/heartbeat). Extends mine_claim_expires_at by MINE_CLAIM_TTL_HOURS (default 24h) for assets still in claimed status. Call periodically while mining so the claim does not expire and get reclaimed by others."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in heartbeatMineIn) (*mcp.CallToolResult, mapOut, error) {
			if len(in.AssetIDs) == 0 {
				return toolErr[mapOut](errf(400, "asset_ids is required"))
			}
			if len(in.AssetIDs) > mineClaimMaxLimit {
				return toolErr[mapOut](errf(400, "asset_ids at most %d", mineClaimMaxLimit))
			}
			ttl := a.cfg.mineClaimTTL()
			tag, err := a.db.Exec(ctx, `UPDATE assets SET mine_claim_expires_at = now() + $2::interval WHERE id = ANY($1) AND mine_status = 'claimed'`, in.AssetIDs, ttl.String())
			if err != nil {
				return toolErr[mapOut](err)
			}
			return nil, mapOut{"renewed": tag.RowsAffected()}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "complete_mine", Description: "Write back mining result (POST /api/mine/complete). status=done|deferred|abandoned; sets mine_completed_at, note, report_url; clears claim lock. done/deferred set mine_cooldown_until = now()+MINE_COOLDOWN_DAYS (default 30; 0 means never auto-requeue). abandoned stays out of the claim queue unless claim_mine_targets is called with include_abandoned=true."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in completeMineIn) (*mcp.CallToolResult, mapOut, error) {
			if len(in.AssetIDs) == 0 {
				return toolErr[mapOut](errf(400, "asset_ids is required"))
			}
			if len(in.AssetIDs) > mineClaimMaxLimit {
				return toolErr[mapOut](errf(400, "asset_ids at most %d", mineClaimMaxLimit))
			}
			st := strings.TrimSpace(in.Status)
			if st != "done" && st != "deferred" && st != "abandoned" {
				return toolErr[mapOut](errf(400, "status must be done|deferred|abandoned"))
			}
			if len(in.Note) > 2000 || len(in.ReportURL) > 2000 {
				return toolErr[mapOut](errf(400, "note and report_url must be at most 2000 characters"))
			}
			days := a.cfg.MineCooldownDays
			if days < 0 {
				days = defaultMineCooldownDays
			}
			var cooldown any
			if (st == "done" || st == "deferred") && days > 0 {
				cooldown = (time.Duration(days) * 24 * time.Hour).String()
			}
			tag, err := a.db.Exec(ctx, `UPDATE assets SET mine_status = $2, mine_note = $3, mine_report_url = $4, mine_completed_at = now(),
				mine_cooldown_until = CASE WHEN $5::text IS NULL THEN NULL ELSE now() + $5::interval END,
				mine_claimed_by = NULL, mine_claim_expires_at = NULL WHERE id = ANY($1)`, in.AssetIDs, st, in.Note, in.ReportURL, cooldown)
			if err != nil {
				return toolErr[mapOut](err)
			}
			return nil, mapOut{"updated": tag.RowsAffected(), "status": st}, nil
		})
	return s
}

// mcpHandler：无状态 Streamable HTTP，JSON 响应；外面套 API 的 token 认证。
func (a *api) mcpHandler() http.Handler {
	s := a.mcpServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return a.auth(h.ServeHTTP)
}
