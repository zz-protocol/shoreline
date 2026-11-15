package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// T55：服务扫描结果列表/详情/统计。只返回主机名与探测结论，不返回 IP，也不返回 banner 原文。
type serviceResultFilter struct {
	Q             string
	Port          int
	Kind          string
	IncludeClosed bool
	Limit         int
	Cursor        int64
}

func serviceResultFilterFromQuery(w http.ResponseWriter, r *http.Request) (serviceResultFilter, bool) {
	f := serviceResultFilter{Q: strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))}
	if strings.ContainsRune(f.Q, 0) {
		writeErr(w, 400, "NUL character not allowed")
		return f, false
	}
	if v := strings.TrimSpace(r.URL.Query().Get("port")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			writeErr(w, 400, "port must be 1..65535")
			return f, false
		}
		f.Port = n
	}
	f.Kind = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("response_kind")))
	switch f.Kind {
	case "", serviceResponseNone, serviceResponseBanner, serviceResponseProbe, serviceResponseReverseProxy:
	default:
		writeErr(w, 400, "response_kind must be none|banner|probe|reverse_proxy")
		return f, false
	}
	closed, ok := boolParam(w, r, "include_closed")
	if !ok {
		return f, false
	}
	f.IncludeClosed = closed != nil && *closed
	return f, true
}

// conds 是列表与统计共用的筛选条件（不含分页与“只看开放”）。
func (f serviceResultFilter) conds() (string, []any) {
	args := []any{f.Q, f.Port, f.Kind}
	return `WHERE ($1::text = '' OR strpos(lower(host), $1) > 0)
	  AND ($2::int = 0 OR port = $2)
	  AND ($3::text = '' OR response_kind = $3)`, args
}

const serviceResultCols = `id, asset_id, host, port, open, verified, response_kind, name, version, probed_at`

func (a *api) queryServiceResults(ctx context.Context, f serviceResultFilter) ([]map[string]any, any, map[string]any, error) {
	if err := checkPage(f.Limit, f.Cursor); err != nil {
		return nil, nil, nil, err
	}
	where, args := f.conds()
	args = append(args, f.IncludeClosed, f.Cursor, f.Limit+1)
	q := `SELECT ` + serviceResultCols + ` FROM asset_services ` + where + `
	  AND ($4::bool OR open)
	  AND ($5::bigint = 0 OR id < $5)
	ORDER BY id DESC
	LIMIT $6`
	list, err := rowsJSON(a.db.Query(ctx, q, args...))
	if err != nil {
		return nil, nil, nil, err
	}
	list, next := cutPage(list, f.Limit)
	stats, err := a.serviceResultStats(ctx, f)
	if err != nil {
		return nil, nil, nil, err
	}
	return list, next, stats, nil
}

func (a *api) listServiceResults(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := page(w, r)
	if !ok {
		return
	}
	f, ok := serviceResultFilterFromQuery(w, r)
	if !ok {
		return
	}
	f.Limit, f.Cursor = limit, cursor
	list, next, stats, err := a.queryServiceResults(r.Context(), f)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": list, "next_cursor": next, "stats": stats})
}

// serviceResultStats：默认“开放”数只计 verified=true，同时保留 TCP 可达数（open=true，含未验证）。
func (a *api) serviceResultStats(ctx context.Context, f serviceResultFilter) (map[string]any, error) {
	where, args := f.conds()
	var verified, tcpReachable, named, reverseProxy, total int64
	if err := a.db.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE open AND verified)::bigint,
		count(*) FILTER (WHERE open)::bigint,
		count(*) FILTER (WHERE open AND verified AND name <> '')::bigint,
		count(*) FILTER (WHERE open AND response_kind = 'reverse_proxy')::bigint,
		count(*)::bigint
		FROM asset_services `+where, args...).Scan(&verified, &tcpReachable, &named, &reverseProxy, &total); err != nil {
		return nil, err
	}
	return map[string]any{
		"open":          verified,
		"verified":      verified,
		"tcp_reachable": tcpReachable,
		"named":         named,
		"reverse_proxy": reverseProxy,
		"total":         total,
	}, nil
}

func (a *api) getServiceResult(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	a.one(w, r, 200, `SELECT `+serviceResultCols+` FROM asset_services WHERE id = $1`, id)
}
