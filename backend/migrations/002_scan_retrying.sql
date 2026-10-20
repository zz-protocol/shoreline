-- 扫描任务增加 retrying 状态（重试中）；failed 为终态
ALTER TABLE scan_tasks DROP CONSTRAINT IF EXISTS scan_tasks_status_check;
ALTER TABLE scan_tasks ADD CONSTRAINT scan_tasks_status_check
    CHECK (status IN ('pending', 'running', 'retrying', 'done', 'failed'));
