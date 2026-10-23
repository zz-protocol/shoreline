package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
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
	Limit           *int   `json:"limit,omitempty" jsonschema:"1..500, default 50"`
	Cursor          string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
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
				AliveSince: aliveSince, AliveUntil: aliveUntil,
			})
			if err != nil {
				return toolErr[pageOut](err)
			}
			list, next := cutPage(list, limit)
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
	return s
}

// mcpHandler：无状态 Streamable HTTP，JSON 响应；外面套 API 的 token 认证。
func (a *api) mcpHandler() http.Handler {
	s := a.mcpServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return a.auth(h.ServeHTTP)
}
