# src-asset-monitor

SRC 资产发现与新增监控。MIT 许可。规格见 SPEC.md，任务见 TASKS.md。

## 本地运行

    docker compose up --build    # 后端 127.0.0.1:8080，前端 127.0.0.1:5173；队列存储用 Valkey 8（Redis 兼容，BSD-3）

## Worker 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| RATE_PASSIVE / RATE_DNS / RATE_HTTP | 1 / 10 / 5 | 每类任务每秒处理数（单实例）；须为有限正数 |
| MAX_RETRY | 3 | 失败重试次数；重试中状态为 retrying，用尽后为 failed（终态） |
| RETRY_BASE_SECONDS | 1 | 指数退避基数（最小 1，asynq 重试调度为秒级精度），第 k 次重试前等待 base × 2^(k-1) 秒，默认即 1s/2s/4s |

## 被动源配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| FOFA_KEY / QUAKE_KEY / HUNTER_KEY | 空 | 测绘 API key；为空则跳过该源 |
| PASSIVE_PAGES | 1 | 测绘源每次查询最多翻页数 |
| PASSIVE_PAGE_SIZE | 100 | 每页条数 |
| SUBFINDER_MAX_TIME | 5m | subfinder 单次枚举最长时间（Go duration，整秒，>= 1s） |
| SUBFINDER_TIMEOUT | 30s | subfinder 单源请求超时（整秒，>= 1s） |

> ⚠️ 测绘 API（FOFA/Quake/Hunter）的请求与响应格式按公开文档实现，**尚未用真实 key 核对**，测试仅基于 mock。

crt.sh 与 subfinder 免 key 源始终启用，结果合并去重，资产记录全部来源。

主机名（根域、子域、URL host）在入库层统一为小写并去掉末尾点（数据库触发器），大写录入的根域也能正确匹配。

第三方依赖许可见 THIRD_PARTY_LICENSES。

## DNS 解析（dns 扫描）

| 变量 | 默认 | 说明 |
|---|---|---|
| DNS_RESOLVERS | 223.5.5.5:53,119.29.29.29:53,1.1.1.1:53 | 逗号分隔，依次回退；无端口默认 53 |
| DNS_TIMEOUT | 2s | 单次查询超时 |
| DNS_QPS | 50 | 每秒 DNS 查询数（单实例，含泛解析探测与回退重试）；须为有限正数 |

查询 A/AAAA（记录 CNAME 链）。泛解析过滤：对子域的父域（仅限根域及其子域）各探测 3 个随机子域，
若该子域全部 IP 都落在探测得到的 IP 集合内则标记 `wildcard`，不交给 http。CDN 先按 CNAME 后缀识别（阿里、腾讯、字节、网宿、Cloudflare、Akamai、CloudFront、Fastly），未命中再用 cdncheck（MIT）内置 IP 段匹配 CDN/WAF（本地匹配，不联网；数据随依赖版本更新）。

泛解析命中的子域不删除，标记 `wildcard=true`；API/列表默认过滤、不产生新增事件（T9/T12 实现），可用参数查看。
结果写入 assets 的 ips / cnames / resolved / wildcard / cdn / resolved_at。

## Certstream

| 变量 | 默认 | 说明 |
|---|---|---|
| CERTSTREAM_URL | 空（不启动） | 实时 CT 流 websocket 地址；断线 5s 重连，根域列表每分钟刷新 |
| CERTSTREAM_BATCH_SIZE | 100 | 新子域攒满 N 条即交 dns 扫描（整数 >= 1） |
| CERTSTREAM_BATCH_INTERVAL | 60s | 首条进批后最多等 T 交 dns（Go duration，>= 1s） |

匹配任一根域的证书域名以来源 `certstream` 入库；**首次入库**的子域按根域分组攒批后创建 dns 扫描任务。

自建 CT 流（可选，镜像 `0rickyy0/certstream-server-go:v1.10.1`，MIT）：

    CERTSTREAM_URL=ws://certstream:8080/ docker compose --profile certstream up --build

不加 `--profile certstream` 时该服务不启动；配置在 `certstream/config.yaml`（只允许 docker 内网访问，不对宿主暴露端口）。

> ⚠️ `certstream/config.yaml` 为最小配置，开发阶段只做过 `docker compose config` 校验、未实际启动，**部署时需核对**（省略字段是否取默认值、CT 日志列表、白名单网段）。
也可把 CERTSTREAM_URL 指向公共服务（如 wss://certstream.calidog.io/）而不启自建服务。

> ⚠️ 测试用的 `backend/testdata/certstream_sample.jsonl` 是按 certstream 公开消息格式**手写的合成样本，未用真实流核对**。

限速按单进程计：**总速率 = 单实例速率 × 实例数**。

扫描自动串联：passive → dns → http（只传解析成功、非泛解析的主机）。
