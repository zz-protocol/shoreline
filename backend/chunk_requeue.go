package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
)

// requeueIncompleteChunks（T40）：启动时把分片协调者下未完成、且 asynq 中找不到的子任务重新入队。
// running/retrying 先重置为 pending；hosts 从子任务 result.hosts 读取。返回重投条数。
// 须在 failOrphanScans 之前调用，避免未入队分片被标 failed。
func requeueIncompleteChunks(ctx context.Context, d *pgxpool.Pool, client *asynq.Client, opt asynq.RedisConnOpt, maxRetry int) (int, error) {
	insp := asynq.NewInspector(opt)
	defer insp.Close()
	queued := map[int64]bool{}
	for _, q := range scanKinds {
		lists := []func(string, ...asynq.ListOption) ([]*asynq.TaskInfo, error){
			insp.ListPendingTasks, insp.ListActiveTasks, insp.ListScheduledTasks, insp.ListRetryTasks,
		}
		for _, list := range lists {
			for page := 1; ; page++ {
				ts, err := list(q, asynq.PageSize(1000), asynq.Page(page))
				if err != nil {
					if errors.Is(err, asynq.ErrQueueNotFound) {
						break
					}
					return 0, err
				}
				for _, t := range ts {
					var p scanPayload
					if json.Unmarshal(t.Payload, &p) == nil {
						queued[p.ScanID] = true
					}
				}
				if len(ts) < 1000 {
					break
				}
			}
		}
	}
	rows, err := d.Query(ctx, `SELECT c.id, c.kind, c.result->'hosts'
		FROM scan_tasks c
		JOIN scan_tasks p ON p.id = c.parent_id
		WHERE COALESCE((p.result->>'chunked')::boolean, false)
		  AND c.status IN ('pending', 'running', 'retrying')
		  AND c.result ? 'hosts'`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var kind string
		var hostsRaw []byte
		if err := rows.Scan(&id, &kind, &hostsRaw); err != nil {
			return n, err
		}
		if queued[id] {
			continue
		}
		var hosts []string
		if err := json.Unmarshal(hostsRaw, &hosts); err != nil || len(hosts) == 0 {
			continue
		}
		if _, err := d.Exec(ctx, `UPDATE scan_tasks SET status = 'pending', error = NULL, started_at = NULL, finished_at = NULL WHERE id = $1`, id); err != nil {
			return n, err
		}
		if err := deliverScan(ctx, d, client, id, kind, hosts, maxRetry); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	// 仍有未完成子任务的分片父任务保持 running
	_, err = d.Exec(ctx, `UPDATE scan_tasks p SET status = 'running', started_at = coalesce(started_at, now()), finished_at = NULL
		WHERE COALESCE((p.result->>'chunked')::boolean, false)
		  AND p.status IN ('pending', 'retrying', 'running')
		  AND EXISTS (
		    SELECT 1 FROM scan_tasks c WHERE c.parent_id = p.id AND c.status IN ('pending', 'running', 'retrying')
		  )`)
	return n, err
}
