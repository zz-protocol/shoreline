package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
)

// orphanMsg：启动时清理孤儿任务写入 scan_tasks.error 的说明。
const orphanMsg = "启动时发现未入队的孤儿任务，已标记失败，可重新触发"

// failOrphanScans：启动时把 created_at < before、status=pending、但 asynq 各状态队列里都找不到的 scan_tasks 标 failed（不重新入队）。
// asynq 任务 id 是随机的，因此按 payload 里的 scan_id 对账。返回标记条数。
func failOrphanScans(ctx context.Context, d *pgxpool.Pool, opt asynq.RedisConnOpt, before time.Time) (int64, error) {
	insp := asynq.NewInspector(opt)
	defer insp.Close()
	queued := []int64{}
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
						queued = append(queued, p.ScanID)
					}
				}
				if len(ts) < 1000 {
					break
				}
			}
		}
	}
	tag, err := d.Exec(ctx, `UPDATE scan_tasks SET status = 'failed', error = $1, finished_at = now()
		WHERE status = 'pending' AND created_at < $2 AND NOT (id = ANY($3))`, orphanMsg, before, queued)
	return tag.RowsAffected(), err
}
