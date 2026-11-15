-- T51 补充：扫描类型 service（手动触发端口指纹；dns/http 仍为链路内部步骤）。SQL 须幂等。
ALTER TABLE scan_tasks DROP CONSTRAINT IF EXISTS scan_tasks_kind_check;
ALTER TABLE scan_tasks ADD CONSTRAINT scan_tasks_kind_check CHECK (kind IN ('passive', 'active', 'dns', 'http', 'expand', 'service'));
