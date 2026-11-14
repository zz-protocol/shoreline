package main

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	wappalyzer "github.com/projectdiscovery/wappalyzergo"
)

// hiddenCategories：默认列表隐藏的低价值 Wappalyzer 分类名（T47，与 SPEC 对齐并测试锁定）。
// 一个组件只有全部分类都在此集合中才隐藏；有任一非此集合分类、或分类为空，则显示。
var hiddenCategories = []string{
	"JavaScript libraries",
	"JavaScript frameworks",
	"UI frameworks",
	"Font scripts",
	"Analytics",
	"Tag managers",
	"Advertising",
	"CDN",
	"PaaS",
	"IaaS",
	"Cookie compliance",
	"CAPTCHAs",
	"WAF",
	"Rich text editors",
	"Maps",
	"Documentation",
	"Static site generator",
}

// defaultShownCategoryExamples：SPEC 列举的默认显示服务端类别，供界面勾选；不是互斥白名单。
// 不在 hiddenCategories 中的分类（含此处未列出的，如 Security）一律显示。
var defaultShownCategoryExamples = []string{
	"Web servers",
	"Web frameworks",
	"Programming languages",
	"Databases",
	"Message queues",
	"Application server",
	"CMS",
	"Caching",
	"Operating systems",
}

func hiddenCategorySet() map[string]struct{} {
	m := make(map[string]struct{}, len(hiddenCategories))
	for _, n := range hiddenCategories {
		m[n] = struct{}{}
	}
	return m
}

// isHiddenByCategories：全部分类都在隐藏集才隐藏；空分类或含任一非隐藏类则显示。
func isHiddenByCategories(cats []string) bool {
	if len(cats) == 0 {
		return false
	}
	hide := hiddenCategorySet()
	for _, c := range cats {
		if _, ok := hide[c]; !ok {
			return false
		}
	}
	return true
}

func sortedCats(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

var (
	wappalyzerCatsOnce sync.Once
	wappalyzerCatsMap  map[string][]string
	wappalyzerCatsErr  error
)

// wappalyzerCategoriesByKey 从本地指纹库建立 name_key → 分类名；不访问外网。
func wappalyzerCategoriesByKey() (map[string][]string, error) {
	wappalyzerCatsOnce.Do(func() {
		w, err := wappalyzerClient()
		if err != nil {
			wappalyzerCatsErr = err
			return
		}
		out := map[string][]string{}
		for name, fp := range w.GetCompiledFingerprints().Apps {
			cats := sortedCats(wappalyzer.AppInfoFromFingerprint(fp).Categories)
			if len(cats) == 0 {
				continue
			}
			out[strings.ToLower(name)] = cats
		}
		wappalyzerCatsMap = out
	})
	return wappalyzerCatsMap, wappalyzerCatsErr
}

func categoriesForName(name string) []string {
	name, _ = parseTech(name)
	if name == "" {
		return nil
	}
	m, err := wappalyzerCategoriesByKey()
	if err != nil || m == nil {
		return nil
	}
	return m[strings.ToLower(name)]
}

type categoryDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// backfillComponentCategories 按组件名查本地指纹库补 categories；查不到的留空，不重扫、不触外网。可重复执行。
func backfillComponentCategories(ctx context.Context, q categoryDB) (int, error) {
	byKey, err := wappalyzerCategoriesByKey()
	if err != nil {
		return 0, err
	}
	rows, err := q.Query(ctx, `SELECT id, name_key FROM components`)
	if err != nil {
		return 0, err
	}
	type rec struct {
		id  int64
		key string
	}
	var list []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.key); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range list {
		cats := byKey[r.key]
		if len(cats) == 0 {
			continue
		}
		tag, err := q.Exec(ctx, `UPDATE components SET categories = $2, updated_at = now()
			WHERE id = $1 AND cardinality(categories) = 0 AND categories IS DISTINCT FROM $2`, r.id, cats)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}
