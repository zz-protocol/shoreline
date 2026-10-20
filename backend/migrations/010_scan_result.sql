-- T10 补充：扫描结果统计（如 active 的候选数、命中数、可信复核剔除数）
ALTER TABLE scan_tasks ADD COLUMN IF NOT EXISTS result JSONB;
