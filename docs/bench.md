# 列表接口压测（T16）

- 时间：2026-10-21 09:12:50（Asia/Hong_Kong）
- 数据：100000 条模拟子域（前缀 `b16-1792545168089120383`），入库耗时 2.553s
- 标准：GET /api/assets 列表（limit≤50）单次 < 1s

| 场景 | 耗时 | 条数 | 结果 |
|---|---|---|---|
| list default limit=50 | 2ms | 50 | PASS |
| list vendor filter | 1ms | 50 | PASS |
| list include_wildcard | 2ms | 50 | PASS |
| list q substring | 13ms | 50 | PASS |
| list cursor page2 | 2ms | 50 | PASS |

**结论**：全部场景 < 1s，达标。
