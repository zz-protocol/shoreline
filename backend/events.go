package main

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 新增与变更判定（T9）：
//   - 资产新增：首次解析成功且非泛解析时生成一次 new（wildcard=true 的不生成）；
//   - 资产变更：前后两次都解析成功、非泛解析，IP 集合变化 → changed；
//   - URL 新增：首次入库生成一次 new（url_id 非空）；
//   - URL 变更：状态码、标题或指纹 hash 变化 → changed。
// new 由唯一部分索引保证只生成一次（迁移 008）。

type change struct {
	Old any `json:"old"`
	New any `json:"new"`
}

func emitNew(ctx context.Context, d *pgxpool.Pool, assetID int64, urlID *int64, detail map[string]any) error {
	_, err := d.Exec(ctx, `INSERT INTO events (type, asset_id, url_id, detail) VALUES ('new', $1, $2, $3) ON CONFLICT DO NOTHING`,
		assetID, urlID, detail)
	return err
}

func emitChanged(ctx context.Context, d *pgxpool.Pool, assetID int64, urlID *int64, detail map[string]any) error {
	_, err := d.Exec(ctx, `INSERT INTO events (type, asset_id, url_id, detail) VALUES ('changed', $1, $2, $3)`, assetID, urlID, detail)
	return err
}

// sameSet 比较两个 IP 列表（忽略顺序和重复）。
func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}
