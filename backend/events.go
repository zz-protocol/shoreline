package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
)

// 新增与变更判定（T9）：
//   - 资产新增：首次解析成功且非泛解析时生成一次 new（wildcard=true 的不生成；父域有泛解析的等 HTTP 复核为真实后生成）；
//   - 资产变更：上次解析成功、本次解析成功且非泛解析，第一跳 CNAME 变化 → changed（T28：IP 不保存、不参与；T37：后续跳变化不报，与 cdn 无关）；
//   - 资产恢复：解析失败时保留最后一次成功的 CNAME，再次解析成功 → recovered；
//   - URL 新增：首次入库生成一次 new（url_id 非空）；
//   - URL 变更：状态码或标题变化 → changed（T37：只有指纹 hash 变化不报、不计，新值照常保存；与状态码/标题同时变时一并记入 changes）。
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

// firstHop 返回 CNAME 链第一跳（空链为 ""）。
func firstHop(c []string) string {
	if len(c) == 0 {
		return ""
	}
	return c[0]
}
