package main

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
)

// 新增与变更判定（T9）：
//   - 资产新增：首次解析成功且非泛解析时生成一次 new（wildcard=true 的不生成；父域有泛解析的等 HTTP 复核为真实后生成）；
//   - 资产变更：上次解析成功、本次解析成功且非泛解析，IP 或 CNAME 集合变化 → changed（新增 IPv6、恢复原值都算）；
//   - 资产恢复：解析失败时保留最后一次成功的 IP/CNAME，再次解析成功 → recovered（detail 带失联前与当前的 IP/CNAME）；
//   - URL 新增：首次入库生成一次 new（url_id 非空）；
//   - URL 变更：状态码、标题或指纹 hash 变化 → changed。
// 新旧值比较在 SELECT ... FOR UPDATE 的事务里做，并发同结果只有第一个真正改值的记 changed；
// new 另由唯一部分索引保证只生成一次（迁移 008）。

type change struct {
	Old any `json:"old"`
	New any `json:"new"`
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func emitNew(ctx context.Context, d execer, assetID int64, urlID *int64, detail map[string]any) error {
	_, err := d.Exec(ctx, `INSERT INTO events (type, asset_id, url_id, detail) VALUES ('new', $1, $2, $3) ON CONFLICT DO NOTHING`,
		assetID, urlID, detail)
	return err
}

func emitEvent(ctx context.Context, d execer, typ string, assetID int64, urlID *int64, detail map[string]any) error {
	_, err := d.Exec(ctx, `INSERT INTO events (type, asset_id, url_id, detail) VALUES ($1, $2, $3, $4)`, typ, assetID, urlID, detail)
	return err
}

// sameSet 比较两个列表（忽略顺序和重复）。
func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}
