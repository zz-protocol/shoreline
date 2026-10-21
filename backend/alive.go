package main

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// markAlive 在 HTTP 阶段判定探活（T21）：responded=任一协议有响应。事务内锁行：
// 已 alive 不再改；有响应 → alive（若原为 dead 则产生一条 recovered）；无响应 → dead。
func markAlive(ctx context.Context, d *pgxpool.Pool, assetID int64, host string, responded bool) error {
	return pgx.BeginFunc(ctx, d, func(tx pgx.Tx) error {
		var old string
		if err := tx.QueryRow(ctx, `SELECT alive FROM assets WHERE id = $1 FOR UPDATE`, assetID).Scan(&old); err != nil {
			return err
		}
		if old == "alive" {
			return nil
		}
		if !responded {
			_, err := tx.Exec(ctx, `UPDATE assets SET alive = 'dead' WHERE id = $1`, assetID)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET alive = 'alive', last_alive_at = now() WHERE id = $1`, assetID); err != nil {
			return err
		}
		if old == "dead" {
			return emitEvent(ctx, tx, "recovered", assetID, nil, map[string]any{"subdomain": host})
		}
		return nil
	})
}
