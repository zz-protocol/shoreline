-- 串联派生的扫描任务：parent_id 指向直接上游，root_scan_id 指向整条链的起点（起点指向自身）
ALTER TABLE scan_tasks ADD COLUMN IF NOT EXISTS parent_id BIGINT REFERENCES scan_tasks(id) ON DELETE CASCADE;
ALTER TABLE scan_tasks ADD COLUMN IF NOT EXISTS root_scan_id BIGINT REFERENCES scan_tasks(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS scan_tasks_root_scan_id_idx ON scan_tasks (root_scan_id);
