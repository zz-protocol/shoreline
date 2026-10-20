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
| DNS_WILDCARD_STABLE | 5 | 泛解析初始探测：连续 N 次无新 IP 即停止 |
| DNS_WILDCARD_MAX | 60 | 每个父域泛解析探测总上限（含补探），须 >= DNS_WILDCARD_STABLE |
| DNS_QPS | 50 | 每秒 DNS 查询数（单实例，含泛解析探测与回退重试）；须为有限正数 |

查询 A/AAAA（记录 CNAME 链）。泛解析过滤：对子域的父域（仅限根域及其子域）自适应探测随机子域——
探到新 IP 就继续，直到连续 DNS_WILDCARD_STABLE 次无新 IP 或达 DNS_WILDCARD_MAX；前 3 次均无解析视为无泛解析。

### HTTP 探测（T8）

| 变量 | 默认 | 说明 |
|---|---|---|
| HTTP_RPS | 10 | 每实例每秒 HTTP 请求数（含泛解析对照请求），>0 |
| HTTP_TIMEOUT | 10s | 单请求超时（Go duration） |
| HTTP_SIMILARITY | 0.9 | 泛解析页面相似度阈值，0 < x <= 1 |
| HTTP_CONCURRENCY | 10 | 单个扫描任务内同时探测的主机数，整数 >= 1；非法值启动即退出 |
| HTTP_VERIFY_RPS | 3 | 泛解析页面复核的单独限速（每实例，含对照页请求），>0 |
| HTTP_VERIFY_MAX | 200 | 每根域每轮最多复核的候选主机数，超出的直接标 wildcard，整数 >= 0 |

对每个主机按 https、http 各发一次 `GET /`，不跟随跳转（记录 Location 为 redirect_to），不校验证书，正文最多读 1MB；
记录状态码、标题、技术栈（wappalyzergo）和指纹 fingerprint_hash = sha256(状态码|标题|技术栈)。
泛解析候选走页面复核：DNS 阶段 IP 落在泛解析集合的主机，以及父域存在泛解析但 IP 未命中的主机（轮换 IP 时 DNS 可能漏标，记 assets.parent_wildcard）。
复核用 HTTP_VERIFY_RPS 单独限速，每根域每轮最多 HTTP_VERIFY_MAX 个，与父域随机子域 `wc-<随机>.<父域>` 的同 scheme 页面比较：
先规范化（去掉主机名、uuid、>= 8 位十六进制串、含数字的词、长度 >= 20 的词，即时间戳/随机 ID/uuid 等动态 token），
再要求状态码相同且 3-gram Jaccard >= HTTP_SIMILARITY。任一协议页面不相似即视为真实主机（wildcard=false、入库 URL、此时才生成新增事件）；
否则标 wildcard、不入库 URL。纯字母的随机 token（无数字、非十六进制）不会被去掉，会拉低相似度。
若子域全部 IP 都落在泛解析 IP 集合内则标记 `wildcard`、不交给 http；有 IP 不在集合内时在剩余预算内补探（应对轮换 IP），仍未覆盖则视为真实记录。
共用泛解析 IP 的真实主机由 T8 页面相似度进一步区分。CDN 先按 CNAME 后缀识别（阿里、腾讯、字节、网宿、Cloudflare、Akamai、CloudFront、Fastly），未命中再用 cdncheck（MIT）内置 IP 段匹配 CDN/WAF（本地匹配，不发 DNS/HTTP 请求；数据随依赖版本更新）。注意：cdncheck 包初始化时会对 [2001:4860:4860::8888]:53 做一次 UDP connect 以探测本机 IPv6（UDP connect 不发包）。

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

### 新增与变更事件（T9）

| 变量 | 默认 | 说明 |
|---|---|---|
| WILDCARD_VERIFIED_TTL | 168h | HTTP 页面复核为真实主机（assets.verified_real_at）的有效期；期内 DNS 不重标 wildcard，过期后重新复核 |

- 资产新增 `new`：子域首次解析成功且非泛解析时生成一次；解析失败照样入库但不发。wildcard=true 的不生成；父域有泛解析的主机等 HTTP 复核为真实后才生成。
- 资产变更 `changed`：上次与本次都解析成功且非泛解析，IP 或 CNAME 集合变化（含新增 IPv6、恢复到原值），detail.changes.ips / cnames 记新旧值；CDN 不单独算。
- 资产恢复 `recovered`：解析失败（如 NXDOMAIN）时保留最后一次成功的 IP/CNAME 只记 resolved=false；再次解析成功时发 recovered，detail 带 last_ips/ips、last_cnames/cnames。
- 已复核为真实的主机在有效期内 IP 全部回落到泛解析 IP 时不报（v1）。
- URL 新增 `new`：scheme+host+port 首次入库时生成一次（url_id 非空）。资产 new 与 URL new 都存，默认按资产聚合留给 T12/T13。
- URL 变更 `changed`：状态码、标题或指纹 hash 变化，detail.changes 记各字段新旧值；只改正文不算。
- 新旧值比较在 `SELECT ... FOR UPDATE` 事务里做，并发同结果同一字段只记一条 changed；`new` 另由唯一部分索引（迁移 008，建索引前先去重存量重复）保证只生成一次。

迁移编号：004 含大小写重复根域合并（先合并再加约束）；008 事件去重+唯一索引；009 assets.parent_wildcard / verified_real_at。首个公开版本发布前允许原地修改迁移，开发库需重建。

### 主动发现（T10）

按厂商总开关 `vendors.active_enabled`（默认关）；关闭时 active 任务直接跳过、不发任何 DNS 查询。只支持手动触发（`TriggerScan(..., "active", ...)`），定时在 T15。产出交 dns → http。

| 变量 | 默认 | 说明 |
|---|---|---|
| ACTIVE_QPS | 20 | 主动发现所有 DNS 查询（批量解析、可信复核、泛解析探测）共享的每秒查询数（单实例），>0 |
| ACTIVE_MAX_CANDIDATES | 20000 | 每根域每轮候选上限（含递归一层），整数 >= 1 |
| ACTIVE_TRUSTED_RESOLVERS | 223.5.5.5,119.29.29.29,1.1.1.1,8.8.8.8 | 可信 resolver，逗号分隔，未写端口默认 53 |
| ACTIVE_WORDLIST | 内置 | 外部字典文件（每行一词，# 开头为注释），设置后替换内置字典 |
| RATE_ACTIVE | 1 | active 任务速率（任务/秒，同 RATE_*） |

非法值启动即退出。

- 字典：内置 SecLists `Discovery/DNS/subdomains-top1million-5000.txt`（MIT，取自 commit 6106054），转小写、跳过不合法标签与重复。
- 变形：对已解析、非泛解析的已知子域首个标签做数字 ±1、dev/test/staging/api 前后缀（`-` 连接）及作为新一级前缀，每子域最多 50 个。
- 流程：批量用 DNS_RESOLVERS 解析 → 命中者用可信 resolver 复核（不通过视为投毒剔除）→ 按 T7 规则（父域随机子域 IP 集合）过滤泛解析 → 以本轮新增的非泛解析子域为父域再跑一层字典 → 入库（来源 active）。已入库子域不作候选。
- 总速率仍按单实例 × 实例数计算。
