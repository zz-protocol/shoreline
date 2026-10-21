-- T23：同一厂商同时只能有一个进行中的 expand（pending/running/retrying），由部分唯一索引原子保证；
-- 插入撞约束即 409。建索引前把历史重复的进行中 expand（每厂商保留最新一条）标 failed。
UPDATE scan_tasks s SET status = 'failed', error = '迁移 015：同厂商重复的进行中 expand，已标记失败', finished_at = now()
    WHERE kind = 'expand' AND status IN ('pending', 'running', 'retrying')
      AND id < (SELECT max(id) FROM scan_tasks t WHERE t.vendor_id = s.vendor_id AND t.kind = 'expand'
                AND t.status IN ('pending', 'running', 'retrying'));
CREATE UNIQUE INDEX IF NOT EXISTS scan_tasks_expand_active_uidx ON scan_tasks (vendor_id)
    WHERE kind = 'expand' AND status IN ('pending', 'running', 'retrying');
