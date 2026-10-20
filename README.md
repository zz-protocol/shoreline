# src-asset-monitor

SRC 资产发现与新增监控。MIT 许可。规格见 SPEC.md，任务见 TASKS.md。

## 本地运行

    docker compose up --build    # 后端 127.0.0.1:8080，前端 127.0.0.1:5173

## Worker 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| RATE_PASSIVE / RATE_DNS / RATE_HTTP | 1 / 10 / 5 | 每类任务每秒处理数（单实例）；须为有限正数 |
| MAX_RETRY | 3 | 失败重试次数；重试中状态为 retrying，用尽后为 failed（终态） |
| RETRY_BASE_SECONDS | 1 | 指数退避基数，第 n 次重试等待 base × 2^(n-1) 秒 |

限速按单进程计：**总速率 = 单实例速率 × 实例数**。

扫描自动串联：passive → dns → http（只传解析成功、非泛解析的主机）。
