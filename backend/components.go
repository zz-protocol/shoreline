package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// builtinCommonComponents：内置常见组件名单，拼写与 wappalyzergo 对齐（T43，测试锁定）。
// 「核心 CDN 标识」取 wappalyzer 中常见 CDN 产品名。
var builtinCommonComponents = []string{
	"Nginx",
	"Apache HTTP Server",
	"IIS",
	"OpenResty",
	"Cloudflare",
	"Akamai",
	"jQuery",
	"Bootstrap",
	"React",
	"Vue.js",
	"Angular",
	"Font Awesome",
	"PHP",
	"OpenSSL",
	"Google Analytics",
	"Google Tag Manager",
	"Webpack",
	"Amazon CloudFront",
	"Fastly",
	"jsDelivr",
	"cdnjs",
	"Unpkg",
	"Google Cloud CDN",
	"Azure CDN",
	"Alibaba Cloud CDN",
	"Tencent Cloud",
	"Wangsu",
	"BootstrapCDN",
	"jQuery CDN",
}

// parseTech 从 wappalyzer 的 urls.tech 项拆出名称与版本（第一个 ':' 之后为版本）。
// 版本只作展示，不参与唯一键。
func parseTech(s string) (name, version string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

// seedCommonComponents 补种内置常见名单：按 name_key 去重，已存在（含被删墓碑）的不动。
func seedCommonComponents(ctx context.Context, d *pgxpool.Pool) (int, error) {
	n := 0
	for _, name := range builtinCommonComponents {
		tag, err := d.Exec(ctx, `INSERT INTO common_component_names (name, name_key, builtin)
			VALUES ($1, $2, true) ON CONFLICT (name_key) DO NOTHING`, name, strings.ToLower(name))
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

func (a *api) initCommon(ctx context.Context) error {
	_, err := seedCommonComponents(ctx, a.db)
	return err
}

type parsedTech struct {
	name, version, raw string
}

func collectTech(tech []string) []parsedTech {
	seen := map[string]parsedTech{}
	var order []string
	for _, raw := range tech {
		name, ver := parseTech(raw)
		if name == "" {
			continue
		}
		k := strings.ToLower(name)
		if prev, ok := seen[k]; ok {
			if prev.version == "" && ver != "" {
				seen[k] = parsedTech{name: prev.name, version: ver, raw: raw}
			}
			continue
		}
		seen[k] = parsedTech{name: name, version: ver, raw: raw}
		order = append(order, k)
	}
	out := make([]parsedTech, 0, len(order))
	for _, k := range order {
		out = append(out, seen[k])
	}
	return out
}

// syncURLComponents：按当前 urls.tech 维护该 URL 的 wappalyzer 关联；不碰其它来源的关联。
func syncURLComponents(ctx context.Context, q dbq, urlID int64, tech []string) error {
	items := collectTech(tech)
	keep := make([]int64, 0, len(items))
	for _, it := range items {
		var id int64
		if err := q.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed)
			VALUES ($1, $2, 'wappalyzer', true)
			ON CONFLICT (name_key) DO UPDATE SET updated_at = now()
			RETURNING id`, it.name, strings.ToLower(it.name)).Scan(&id); err != nil {
			return err
		}
		ev, _ := json.Marshal(map[string]string{"tech": it.raw})
		if _, err := q.Exec(ctx, `INSERT INTO url_components (url_id, component_id, version, source, evidence)
			VALUES ($1, $2, $3, 'wappalyzer', $4)
			ON CONFLICT (url_id, component_id) DO UPDATE SET version = EXCLUDED.version, evidence = EXCLUDED.evidence`,
			urlID, id, it.version, ev); err != nil {
			return err
		}
		keep = append(keep, id)
	}
	_, err := q.Exec(ctx, `DELETE FROM url_components
		WHERE url_id = $1 AND source = 'wappalyzer' AND NOT (component_id = ANY($2::bigint[]))`, urlID, keep)
	return err
}
