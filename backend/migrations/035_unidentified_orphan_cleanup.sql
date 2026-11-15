-- T57 补：034 把旧关联搬到归一化后的新组件，但保留旧组件行，会留下无关联的
-- unidentified-service-* 孤儿。这里只清理没有关联的未识别组件，不动已识别组件。
-- 幂等：重复执行无副作用。
DELETE FROM components c
WHERE c.source = 'service'
  AND c.name LIKE 'unidentified-service-%'
  AND NOT EXISTS (
      SELECT 1 FROM asset_components ac WHERE ac.component_id = c.id
  );
