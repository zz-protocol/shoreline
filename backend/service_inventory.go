package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// serviceComponentNames 把指纹短名映射到库存展示名（尽量与 Wappalyzer 对齐以便分类与去重）。
var serviceComponentNames = map[string]string{
	"mysql":         "MySQL",
	"postgresql":    "PostgreSQL",
	"redis":         "Redis",
	"mongodb":       "MongoDB",
	"elasticsearch": "Elasticsearch",
	"kafka":         "Kafka",
	"etcd":          "etcd",
	"nacos":         "Nacos",
	"rabbitmq":      "RabbitMQ",
	"memcached":     "Memcached",
}

// serviceFallbackCategories：指纹库没有该组件时使用的分类（Wappalyzer 有 Databases/Caching；无 Message queues / Application server，按 SPEC 用这两类名）。
var serviceFallbackCategories = map[string][]string{
	"kafka":     {"Message queues"},
	"rabbitmq":  {"Message queues"},
	"memcached": {"Caching"},
	"etcd":      {"Application server"},
	"nacos":     {"Application server"},
}

func serviceComponentName(fingerprint string) string {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	if n, ok := serviceComponentNames[fingerprint]; ok {
		return n
	}
	return ""
}

func serviceComponentCategories(display, fingerprint string) []string {
	if cats := categoriesForName(display); len(cats) > 0 {
		return cats
	}
	return sortedCats(serviceFallbackCategories[strings.ToLower(strings.TrimSpace(fingerprint))])
}

func unidentifiedServiceName(banner string) string {
	sum := sha256.Sum256([]byte(banner))
	return "unidentified-service-" + hex.EncodeToString(sum[:])[:12]
}

// syncAssetServicesToComponents 把 asset_services 识别结果写入 components（source=service），不含 IP。
// 已有组件的 source / 非空 categories / 数据行不覆盖、不删除。
func syncAssetServicesToComponents(ctx context.Context, q serviceScanQ, targets []serviceScanTarget) error {
	seen := map[string]struct{}{}
	var hosts []string
	for _, t := range targets {
		h := normalizeHost(t.Host)
		if h == "" || rejectedServiceScanHost(h) {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		return nil
	}
	rs, err := q.Query(ctx, `SELECT asset_id, host, port, open, name, version, banner
		FROM asset_services WHERE host = ANY($1)`, hosts)
	if err != nil {
		return err
	}
	defer rs.Close()
	type row struct {
		assetID        int64
		host           string
		port           int
		open           bool
		name, ver, ban string
	}
	var rows []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.assetID, &r.host, &r.port, &r.open, &r.name, &r.ver, &r.ban); err != nil {
			return err
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		return err
	}
	for _, r := range rows {
		if !r.open {
			continue
		}
		r.ban = sanitizeServiceBanner([]byte(r.ban))
		var display string
		var confirmed bool
		var cats []string
		var fp string
		if n := serviceComponentName(r.name); n != "" {
			display, confirmed, fp = n, true, strings.ToLower(r.name)
			cats = serviceComponentCategories(display, fp)
		} else if strings.TrimSpace(r.ban) != "" {
			display, confirmed = unidentifiedServiceName(r.ban), false
		} else {
			continue
		}
		var id int64
		if err := q.QueryRow(ctx, `INSERT INTO components (name, name_key, source, confirmed, categories)
			VALUES ($1, $2, 'service', $3, $4)
			ON CONFLICT (name_key) DO UPDATE SET
				updated_at = now(),
				categories = CASE WHEN cardinality(components.categories) > 0 THEN components.categories ELSE EXCLUDED.categories END
			RETURNING id`, display, strings.ToLower(display), confirmed, sortedCats(cats)).Scan(&id); err != nil {
			return err
		}
		ev, _ := json.Marshal(map[string]any{"host": r.host, "port": r.port, "banner": r.ban})
		if _, err := q.Exec(ctx, `INSERT INTO asset_components (asset_id, component_id, port, version, source, evidence)
			VALUES ($1, $2, $3, $4, 'service', $5)
			ON CONFLICT (asset_id, component_id, port) DO UPDATE SET
				version = CASE WHEN EXCLUDED.version <> '' THEN EXCLUDED.version ELSE asset_components.version END,
				evidence = EXCLUDED.evidence,
				ignored_at = NULL`,
			r.assetID, id, r.port, r.ver, ev); err != nil {
			return err
		}
	}
	return nil
}
