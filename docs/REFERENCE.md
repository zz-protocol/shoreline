# 配置与接口

本文说明岸线 Shoreline 的环境变量、扫描行为、REST / MCP 与 Web 端细节。部署步骤见 [DEPLOY.md](DEPLOY.md)，Agent 使用说明见 [AGENT_GUIDE.md](AGENT_GUIDE.md)，压测报告见 [bench.md](bench.md)。

本地 `docker compose` 队列用 Valkey 8（Redis 兼容，BSD-3）。公网 VPS、反代、Certstream、ICP 的部署步骤见 [部署指南](DEPLOY.md)。

## Worker 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| RATE_PASSIVE / RATE_DNS / RATE_HTTP / RATE_SERVICE | 1 / 10 / 5 / 1 | 每类任务每秒处理数（单实例）；须为有限正数。`RATE_SERVICE` 为端口指纹任务级限速（T51），与 `SERVICE_SCAN_RPS`（每端口连接）叠加 |
| MAX_RETRY | 3 | 失败重试次数；重试中状态为 retrying，用尽后为 failed（终态） |
| HOST_CHUNK_SIZE | 2000 | dns/http 单任务 hosts 超过此数则拆成子 scan_task（parent_id）；父状态由子推导；重启只重投未完成分片（T40） |
| WORKER_CONCURRENCY | 10 | Asynq worker 并发任务数，范围 1–1000。提高可加速，但要同时看 `RATE_*`、`SERVICE_SCAN_RPS` 和目标承受能力 |
| RETRY_BASE_SECONDS | 1 | 指数退避基数（最小 1，asynq 重试调度为秒级精度），第 k 次重试前等待 base × 2^(k-1) 秒，默认即 1s/2s/4s |
| SERVICE_SCAN_ENABLED | false | 非 Web（端口/服务）探测全局开关，默认关。未在网页/API 写入过时读此变量；一旦 PATCH `/api/service-scan` 则以库为准。关闭时不发起探测，不影响 Web 扫描（T49） |
| CDN_CNAME_FILE | 空 | 额外 CNAME 后缀库文件（每行 `suffix vendor`，`#` 开头为注释）；与内置后缀合并，供 CDN 判定（T50） |
| CDN_CIDR_FILE | 空 | 额外 CDN/WAF IP 段库文件（每行 `cidr vendor`）；仅本地匹配，测试用文档保留段。内置仍含 CNAME 后缀与 cdncheck 段（T50） |
| SERVICE_SCAN_PORTS | 空（取 YAML 规则端口） | 端口指纹扫描端口（逗号分隔，不接受范围；T51）。为空时使用 `fingerprints/services.yaml` 声明的全部端口，新增 YAML 规则即自动纳入默认扫描 |
| SERVICE_SCAN_RPS | 2 | 端口指纹每秒连接上限（全局，burst 1；T51） |
| SERVICE_SCAN_TIMEOUT | 2s | 单端口连接 + banner 读取超时；拨号超时/拒绝记端口关闭，不重试（T51） |
| SERVICE_DNS_NEGATIVE_TTL | 6h | DNS 负缓存窗口（T58）。`resolved=false` 且 `resolved_at` 在此窗口内的资产不再逐轮重解析，记 `dns_negative_cached`；允许 6h–24h，越界拒绝启动 |
| SERVICE_FINGERPRINT_RULES | 空 | 额外服务指纹 YAML 文件；同名规则整条覆盖内置规则，其余追加。启动时校验，非法规则（坏正则、端口越界、未知置信度、重复 name、probe 超 256 字节等）直接拒绝启动（T53） |

## 被动源配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| FOFA_KEY / QUAKE_KEY / HUNTER_KEY | 空 | 测绘 API key；为空则跳过该源。网页「测绘 Key」设置优先，未配置时回退到此处；改网页配置后下次扫描生效、无需重启 |
| PASSIVE_PAGES | 1 | 测绘源每次查询最多翻页数 |
| PASSIVE_PAGE_SIZE | 100 | 每页条数 |
| SUBFINDER_MAX_TIME | 5m | subfinder 单次枚举最长时间（Go duration，整秒，>= 1s） |
| SUBFINDER_TIMEOUT | 30s | subfinder 单源请求超时（整秒，>= 1s） |

> ⚠️ 测绘 API（FOFA/Quake/Hunter）的请求与响应格式按公开文档实现，**尚未用真实 key 核对**，测试仅基于 mock。

crt.sh 与 subfinder 免 key 源始终启用，结果合并去重，资产记录全部来源。
passive 任务把 `{sources_ok, sources_failed:[{source,error}], found}` 写入 `scan_tasks.result`；subfinder 各内部源错误会进入 `sources_failed`。`found==0` 且有来源失败时标 `degraded=true` 并写 `degraded_reason`（T39）。

主机名（根域、子域、URL host）在入库层统一为小写并去掉末尾点（数据库触发器），大写录入的根域也能正确匹配。

第三方依赖许可见 [THIRD_PARTY_LICENSES](../THIRD_PARTY_LICENSES)。

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

T58 起解析健壮性（dns 扫描与服务拨号共用）：

- 只有 `RcodeSuccess` / `RcodeNameError`（NXDOMAIN）当权威答案；`REFUSED` / `FORMERR` / `SERVFAIL` / 超时换下一台 resolver，每次查询最多 3 次尝试（含 100ms 起退避）。
- 查询带 EDNS0（1232 字节）；响应 `TC=1` 被截断时对同一台 resolver 改 TCP 重查一次。
- A 与 AAAA 分开计成功：任一类拿到地址即视为解析成功，不会因 AAAA 超时丢弃已拿到的 A。
- NXDOMAIN 必须由另一台 resolver 复核（或已有负缓存）才落负答案；复核给不出结论时按超时/错误处理，不写 `dns_nxdomain`。只配置一台 resolver 时没有可交叉复核的对象，直接采用其负答案。

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
复核用 HTTP_VERIFY_RPS 单独限速，每根域每轮最多 HTTP_VERIFY_MAX 个。每个父域每个 scheme 取 2 个随机子域 `wc-<随机>.<父域>` 的页面作对照（某次请求失败或超时时重取 1 个随机子域；仍不足 2 页、两页状态码不一致、或两页公共内容不足 5 个词（空页/几乎全是动态内容，相同跳转除外），一律不放行：本轮该父域候选保持 wildcard、不入库 URL，留待下轮）：
先规范化（去掉主机名、uuid、>= 8 位十六进制串、含数字的词、长度 >= 20 的词、英文星期/月份名），
再把两页之间不同的词视为动态内容掩码掉（两页最长公共词序列为模板，差异词数为动态配额），覆盖规则外的随机词、短 base64、文本日期等。
候选页相似度 = 与模板的最长公共词数 /（模板词数 + 超出动态配额的多余词数），状态码须相同，>= HTTP_SIMILARITY 视为泛解析。
任一协议页面不相似即视为真实主机（wildcard=false、入库 URL、此时才生成新增事件）；否则标 wildcard、不入库 URL。

已知局限：泛解析页本身为空或全动态时，该父域下真实主机救不回、保持 wildcard。
短页模式（T8 封版）：每个父域每协议取 3 张随机子域对照页（失败重取 1 次），三张都返回、状态码一致、非空才可用，否则本轮该父域候选保持 wildcard。三张对照页归一化后的公共词都 < 5（如 nginx 默认 404、正文 "not found"、"ok"+随机字母）时不比 3-gram，而是：
- 候选状态码与对照页不同 → 真实（如泛解析 404、真实主机 200）。
- 候选与对照页同状态码时：先取回本轮全部候选再判定；同父域同状态码的候选有 2 个及以上 → 父域视为不稳定，这些候选一律保持 wildcard（长度差异大说明泛解析页在变；长度相近说明是泛解析的另一种模板）。
- 只有 1 个同状态码候选时才用长度/hash：比最长对照页长出 max(64B,30%)，或对照页长度稳定（相差 ≤10%）而候选长度差 > max(64B,30%)，或对照页 hash 一致而候选 hash 不同 → 真实；否则保持 wildcard。

已知局限（原则：宁可漏救不误放）：泛解析返回短页且与真实主机同状态码时，同一轮里只要还有其它同状态码候选（通常都有），真实主机（如 lb/blog 返回 200 长页而泛解析返回 200 短页）救不回，保持 wildcard；只有它是本轮该父域唯一的同状态码候选时才可能救回。
若子域全部 IP 都落在泛解析 IP 集合内则标记 `wildcard`、不交给 http；有 IP 不在集合内时在剩余预算内补探（应对轮换 IP），仍未覆盖则视为真实记录。
共用泛解析 IP 的真实主机由 T8 页面相似度进一步区分。CDN 先按 CNAME 后缀识别（阿里、腾讯、字节、网宿、Cloudflare、Akamai、CloudFront、Fastly），未命中再用 cdncheck（MIT）内置 IP 段匹配 CDN/WAF（本地匹配，不发 DNS/HTTP 请求；数据随依赖版本更新）。注意：cdncheck 包初始化时会对 [2001:4860:4860::8888]:53 做一次 UDP connect 以探测本机 IPv6（UDP connect 不发包）。

泛解析命中的子域不删除，标记 `wildcard=true`；API/列表默认过滤、不产生新增事件（T9/T12 实现），可用参数查看。
结果写入 assets 的 cnames / resolved / wildcard / cdn / cdn_fronted / resolved_at。解析出的 IP 只在内存里用于泛解析、CDN 与存活判断，不入库、不出现在 API/MCP/页面（T28 / T50）。CNAME 或 IP 段命中时 `cdn_fronted=true`，端口扫描跳过、仍交给 HTTP 识别（T50）。

## Certstream

| 变量 | 默认 | 说明 |
|---|---|---|
| CERTSTREAM_URL | 空（不启动） | 实时 CT 流 websocket 地址；断线 5s 重连，根域列表每分钟刷新 |
| CERTSTREAM_BATCH_SIZE | 100 | 新子域攒满 N 条即交 dns 扫描（整数 >= 1） |
| CERTSTREAM_BATCH_INTERVAL | 60s | 首条进批后最多等 T 交 dns（Go duration，>= 1s） |

匹配任一根域的证书域名以来源 `certstream` 入库；**首次入库**的子域按根域分组攒批后创建 dns 扫描任务。

自建 CT 流（可选，镜像 `0rickyy0/certstream-server-go:v1.10.1`，MIT）：

    CERTSTREAM_URL=ws://certstream:8080/ docker compose --profile certstream up --build

不加 `--profile certstream` 时该服务不启动；配置在 `../certstream/config.yaml`（只允许 docker 内网访问，不对宿主暴露端口）。

> ⚠️ `../certstream/config.yaml` 为最小配置，开发阶段只做过 `docker compose config` 校验、未实际启动，**部署时需核对**（省略字段是否取默认值、CT 日志列表、白名单网段）。
也可把 CERTSTREAM_URL 指向公共服务（如 wss://certstream.calidog.io/）而不启自建服务。

> ⚠️ 测试用的 `../backend/testdata/certstream_sample.jsonl` 是按 certstream 公开消息格式**手写的合成样本，未用真实流核对**。

限速按单进程计：**总速率 = 单实例速率 × 实例数**。

扫描自动串联：passive → dns → http（只传解析成功、非泛解析的主机）。

### 新增与变更事件（T9）

| 变量 | 默认 | 说明 |
|---|---|---|
| WILDCARD_VERIFIED_TTL | 168h | HTTP 页面复核为真实主机（assets.verified_real_at）的有效期；期内 DNS 不重标 wildcard，过期后重新复核 |

- 资产新增 `new`：子域首次解析成功且非泛解析时生成一次；解析失败照样入库但不发。wildcard=true 的不生成；父域有泛解析的主机等 HTTP 复核为真实后才生成。
- 资产变更 `changed`：上次与本次都解析成功且非泛解析，只看第一跳 CNAME 变化（含恢复到原值；T37：第一跳不变、仅后续跳变化不报，与 cdn 字段无关，新值照常保存），detail.changes.cnames 记新旧值；IP 变化不报（T28）；CDN 不单独算。历史上「只改过 IP」的 changed（`changes` 为空或仅剩 ips）保留在库，但列表 / stats / values / MCP 一律不返回、不计入。
- 资产探活（T21/T38）：`alive` = alive / dead / unknown，`last_alive_at`，`dead_reason` = nxdomain / no_http / null。HTTP 阶段判定：DNS 能解析且任一协议有响应（任何状态码）为 alive，一旦 alive 不再改回、不再复查，并清空 dead_reason；DNS 失败为 dead+nxdomain；HTTP 无响应为 dead+no_http；未探测或 HTTP 复核确认为泛解析为 unknown。dead 资产由 `SCHEDULE_ALIVE` 定时复查。
- 资产恢复 `recovered`：仅在 dead→alive 时产生一条（同一事务锁行，并发只发一次）。DNS 解析失败时仍保留最后一次成功的 CNAME，只记 resolved=false，DNS 层不再发 recovered。`GET /api/assets?alive=` 可筛选。
- 已复核为真实的主机在有效期内解析结果回落到泛解析时不报（v1）。
- URL 新增 `new`：scheme+host+port 首次入库时生成一次（url_id 非空）。资产 new 与 URL new 都存，默认按资产聚合留给 T12/T13。
- URL 变更 `changed`：状态码或标题变化，detail.changes 记各字段新旧值（指纹 hash 同时变化也一并记入）；只有指纹 hash 变化不产生、不计入（T37，新值照常保存）；只改正文不算。已有历史事件不动。
- 新旧值比较在 `SELECT ... FOR UPDATE` 事务里做，并发同结果同一字段只记一条 changed；`new` 另由唯一部分索引（迁移 008，建索引前先去重存量重复）保证只生成一次。

迁移编号：004 含大小写重复根域合并（先合并再加约束）；008 事件去重+唯一索引；009 assets.parent_wildcard / verified_real_at。首个公开版本发布前允许原地修改迁移，开发库需重建。

### 主动发现（T10）

按厂商总开关 `vendors.active_enabled`（默认关）；关闭时 active 任务直接跳过、不发任何 DNS 查询。只支持手动触发（`TriggerScan(..., "active", ...)`），定时在 T15。产出交 dns → http。

| 变量 | 默认 | 说明 |
|---|---|---|
| ACTIVE_QPS | 20 | 主动发现所有 DNS 查询（批量解析、可信复核、泛解析探测）共享的每秒查询数（单实例），>0 |
| ACTIVE_MAX_CANDIDATES | 20000 | 每根域每轮候选上限（含递归一层；递归层预留 20%，第一层没用完的额度转给递归层），整数 >= 1 |
| ACTIVE_TRUSTED_RESOLVERS | 223.5.5.5,119.29.29.29,1.1.1.1,8.8.8.8 | 可信 resolver，逗号分隔，未写端口默认 53 |
| ACTIVE_WORDLIST | 内置 | 外部字典文件（每行一词，# 开头为注释），设置后替换内置字典 |
| RATE_ACTIVE | 1 | active 任务速率（任务/秒，同 RATE_*） |

非法值启动即退出。

- 字典：内置 SecLists `Discovery/DNS/subdomains-top1million-5000.txt`（MIT，取自 commit 6106054），转小写、跳过不合法标签与重复。
- 变形：对已解析、非泛解析的已知子域首个标签做数字 ±1、dev/test/staging/api 前后缀（`-` 连接）及作为新一级前缀，每子域最多 50 个。
- 流程：批量用 DNS_RESOLVERS 解析 → 命中者用可信 resolver 复核（不通过视为投毒剔除）→ 按 T7 规则（父域随机子域 IP 集合）过滤泛解析 → 以本轮新增的非泛解析子域为父域再跑一层字典 → 入库。来源细分为 `active:wordlist`（字典 × 根域）、`active:mutation`（变形）、`active:recursive`（递归一层）；同一子域被多种方式生成或已有其他来源时，按现有 sources 语义合并去重。已入库子域不作候选。
- 每次 active 扫描把统计写入 `scan_tasks.result`（candidates、first_layer、recursive、hits、trusted_rejected＝可信复核剔除数），并打一行日志。
- 总速率仍按单实例 × 实例数计算。

### 测试

- 全量：`cd backend && TEST_DATABASE_URL=postgres://sam:sam@127.0.0.1:5432/sam?sslmode=disable go test ./...`（外部依赖全部本地 mock）。
- 真实时间的重试间隔测试 `TestRetryIntervals` 依赖 asynq 调度时序，放在 `//go:build integration` 文件里，全量不含，单独跑：
  `TEST_DATABASE_URL=... go test -tags integration -count=10 -run TestRetryIntervals .`

### ICP/子公司扩展（T11）

只手动触发（`TriggerScan(..., "expand", ...)`，按任务所属根域的厂商跑；Web「厂商 / 根域」页点厂商后有「一键发现新根域（expand）」按钮，显示最近一次运行状态与结果，T23；同一厂商已有 pending/running/retrying 的 expand 时再触发返回 409，REST 与 MCP 一致），发现的新根域进 `root_candidates`（待确认），不自动加入扫描、不串联后续任务。

| 变量 | 默认 | 说明 |
|---|---|---|
| ICP_API_URL | 空（跳过扩展） | 通用 HTTP JSON ICP 接口地址，须 http/https；非法值启动即退出 |
| ICP_PROVIDER | generic | 接口预设：`generic`（下方约定格式）或 `icp_yanzhenma`；其他值启动即退出（exit 1） |
| ICP_API_KEY | 空 | 有值时请求带 `Authorization: Bearer <key>`；URL/KEY 含控制字符启动即退出（exit 1） |
| RATE_EXPAND | 1 | expand 任务速率（任务/秒，同 RATE_*） |
| SCHEDULE_PASSIVE | 6h | 定时被动扫描间隔（Go duration）；`0`/`off` 关闭；非法启动退出 |
| SCHEDULE_ACTIVE | 24h | 定时主动扫描间隔；仅 `active_enabled` 厂商的根域；`0`/`off` 关闭 |
| SCHEDULE_ALIVE | 24h | dead 资产复查间隔（重做 DNS+HTTP）；alive 不再复查；`0`/`off` 关闭；非法启动退出 |
| SCHEDULE_SERVICE | 空（禁用） | 定时端口指纹间隔（Go duration）；默认空/未配置则禁用，只能手动触发；配了间隔才为所有根域入队 `service`；`0`/`off` 关闭；非法启动退出。计划任务仍受 T49 开关、授权范围和 T50 `cdn_fronted` 门控；无厂商级 409 |

- ICP 接口约定：`GET <ICP_API_URL>?company=<主体名>` 或 `?domain=<域名>`，响应 `{"records":[{"domain":"...","company":"...","license":"备案号"}]}`。对接具体服务商时需按此约定加一层转换。
- `ICP_PROVIDER=icp_yanzhenma`：对接自建的 [zz-protocol/icp_yanzhenma](https://github.com/zz-protocol/icp_yanzhenma)（FastAPI，MIT；本仓库不含其代码，只走 HTTP）。`ICP_API_URL` 填该服务根地址（如 `http://icp:8000`），请求 `GET <ICP_API_URL>/query?domain=<值>`；该服务把参数作为工信部查询的 unitName 透传，主体名与域名都走此接口（**推断，待真实联调确认**）。响应 `status=success` 时从 `data.params.list`（或 `data.list`）取 `domain`/`unitName`/`mainLicence`（`domain` 字段取法为**推断，待真实联调确认**）；`status=failed`、HTTP 非 200、超时（30s）或连不上都计入 `icp_failures`，若本轮全部失败扫描一律为 done 并标 `degraded:true`，不重试；`GET /api/scans/{id}` 返回 `degraded` 与 `degraded_reason`。该服务按其自述**仅限安全研究**用途，需控制请求频率：本项目超时 30s，`RATE_EXPAND` 默认 1 任务/秒。真实联调留到部署时做。
- 未配置 `ICP_API_URL` 时 ICP 扩展自动关闭（扫描结果 `skipped: icp not configured`），只用手工导入的子公司名单。
- 链路：厂商主体名（`vendors.company_name`）→ ICP；子公司（持股 >= 50%，只一层）→ ICP；已有根域 → ICP 反查主体 → 该主体的 ICP。ICP 返回的域名先规范化（去 `*.`、scheme、path/query、端口、末尾点，转小写），中文等 IDN 转 punycode（golang.org/x/net/idna，BSD-3）入库，非法的丢弃；IP 不生成候选（先去 scheme、userinfo、端口、路径、IPv6 zone、末尾点，再判：可解析为 IPv4/IPv6，或末级标签全数字 / 0x 十六进制，覆盖 127.1、0x7f.0.0.1、010.0.0.1 等写法）；扫描结果 `scan_tasks.result` 记 `icp_failures`、`skipped_ip`、`skipped_invalid`、`skipped_known_root`；已有根域及其子域（如 www.corp.test）不进候选；同批同域名重复记录证据去重。
- 子公司只查导入名单里的公司（不自动展开股权链）；孙公司是否被查询，取决于导入名单里是否列入了它。
- 子公司源可插拔；当前实现为手工导入名单 `vendor_subsidiaries`（CSV：`名称,持股百分比`，# 开头为注释，同名覆盖比例；自动去掉 UTF-8 BOM；第一行所有列都是非数字列名、且比例列是已知列名（比例/持股/持股比例/持股百分比/ratio/share/percent/percentage 等）时才视为表头跳过，否则按数据校验；比例为十进制数字，可带 `%`/`％`（如 `51 %`、`51％`），范围 0-100；任一行非法整批拒绝并报行号（API 返回 400）；没有有效数据行（空文件、只有注释或表头）也返回 400）。
- `root_candidates`：厂商、域名、来源（icp:company / icp:subsidiary / icp:reverse，可多种）、依据（主体、备案号、子公司及比例、反查的根域）、状态（pending 待确认 / confirmed 已确认 / ignored 已忽略）、时间。
  确认写入 `root_domains`（不自动扫描）；忽略后再次扩展也不再提示。确认/忽略的操作接口在 T12/T14，当前为函数 `confirmCandidate` / `ignoreCandidate`。
- 每次 expand 把 found / new_candidates 写入 `scan_tasks.result`。

## 挖掘认领与回写（T32）

自动化管理「该挖 / 正在挖 / 挖过」：资产带 `mine_status`（unset|claimed|done|deferred|abandoned）及认领人/锁过期/完成时间/冷却/结论。挖掘 agent 用 REST 或 MCP（`claim_mine_targets` / `heartbeat_mine` / `complete_mine`）自助领取与回写；Web 资产页可按挖掘状态筛选，并展示认领人与最近挖掘时间。不内置漏洞扫描、不存原始流量。

## 客户托管排除（T24）

云厂商分配给客户的默认域名（OSS/COS/OBS 桶、函数、网站托管、CDN 默认域等）是托管在云上的客户资产，不是厂商自身资产。资产子域名命中「客户托管规则」时标记 `customer_hosted=true`（不删除），资产列表和播报默认隐藏，`include_customer_hosted=true`（Web 上「含客户托管」开关）可查看。

- 只匹配资产子域名本身，不看 CNAME；规则两种写法：① 以 `*.` 开头（匹配 ≥1 层子域）；② （T31）不以 `*.` 开头、至少一个标签含 `*`，**标签数必须与主机名完全一致**、逐层匹配，如 `*-vitesandbox.miaoda.cn` 命中 `app-8t89ep1cj4zl-vitesandbox.miaoda.cn`，不命中 `api.miaoda.cn`、`miaoda.cn`、`a.b-vitesandbox.miaoda.cn`；不含 `*`（或含 `**`）的规则返回 400。标签内 `*`（如 `oss-*`）只匹配本层；每条规则至少比服务端点多一层，端点本身不命中；内置与新增规则走同一校验：最后一个含 `*` 的标签之后的固定后缀须比 PSL ICANN 段公共后缀至少多一层（golang.org/x/net/publicsuffix，BSD-3，只看 ICANN 段、不看私有段），`*.*.com`、`*.co.uk` 返回 400，`*.github.io` 这类允许。
- 内置 64 条（AWS/Azure/GCP/阿里云/腾讯云/华为云/火山/百度/七牛/Cloudflare/Akamai/Fastly/GitHub Pages/Heroku），每条在 `../backend/customer_hosted.go` 注释云产品和官方文档。启动时补种；内置规则可在「客户托管规则」页停用或删除，删除后升级不会加回。
- 改规则后后台异步重算（`GET /api/customer-hosted-tasks/{id}` 看进度）。
- 未内置，可自行添加：火山引擎 CDN（*.volccdn.com）、又拍云、网宿、金山云（KS3）、京东云、UCloud、Vercel、Netlify。

## REST API（T12）

文档：`GET /api/openapi.yaml`（嵌入，无需认证；Swagger UI 留 T14）。Agent 使用说明：`AGENT_GUIDE.md`，后端嵌入副本 `../backend/agent_guide.md`（构建上下文只有 backend/，改文档后需 `cp AGENT_GUIDE.md ../backend/agent_guide.md`，测试校验两份一致），经 `GET /api/agent-guide`（无需认证）、`/docs` 页与 MCP `get_agent_guide` 提供（T36）。其余 `/api/*` 需 `Authorization: Bearer <token>`。

| 变量 | 默认 | 说明 |
|---|---|---|
| API_BOOTSTRAP_TOKEN | 空 | 初始 token，用它调 `POST /api/tokens` 生成正式 token（明文只返回一次，库里只存 sha256，不分权限）。为空时只认 api_tokens 里的 token |
| SCOPE_BATCH_SIZE | 500 | 改范围规则后异步重算 in_scope 的每批行数（整数 >= 1，非法启动退出） |
| MINE_COOLDOWN_DAYS | 30 | 挖掘 complete（done/deferred）后的冷却天数；`0` 表示完成后不再自动进入可领队列（整数 >= 0，非法启动退出） |
| MINE_CLAIM_TTL_HOURS | 24 | 挖掘认领锁小时数；超时未 complete/heartbeat 可被再领（整数 >= 1，非法启动退出） |

- 错误统一 `{"error":"..."}`；查询参数、路径或 JSON 字段含 NUL 一律 400；JSON body 只能有一个值（其后仅允许空白），否则 400；分页 `limit`（默认 50，最大 500）+ `cursor`（按 id 倒序，传上页 `next_cursor`）。
- `GET /api/assets`：`vendor`、`in_scope`、`alive`、`dead_reason`（nxdomain|no_http；T38）、`alive_since`/`alive_until`（最后存活时间半开区间 `[since,until)`，可与 `alive` 叠加；T27）、`mine_status` / `in_cooldown`（挖掘状态与冷却；T32）、`q`（子域名或 URL 标题子串）、`new_since`（资产 new 事件时间，RFC3339）、`include_wildcard`（默认过滤泛解析）。`GET /api/assets/values?field=subdomain|url`：当前筛选下整列复制（去重、上限 100000；T29，与播报 values 同套路）。
- 挖掘认领（T32）：`POST /api/mine/claim`（原子领取，body：`claimed_by` 必填，`limit` 默认 20 上限 100，`vendor_id`/`since`/`q`/`include_abandoned`）；`POST /api/mine/heartbeat`（`asset_ids` 续锁）；`POST /api/mine/complete`（`asset_ids` + `status=done|deferred|abandoned`，可选 `note`/`report_url`）。默认只领 alive 且非客户托管；冷却与认领 TTL 见环境变量。
- `GET /api/events`：默认资产级（`level=asset`），`level=url|all` 查 URL 事件；不传 `type` 默认 `new,recovered`（显式传 `changed` 才含）；`since`/`until`、`asset_id`、`vendor`、`include_wildcard`、`q`（子域名或 URL 子串，不区分大小写；`/stats` 与 `/values`、MCP `list_new_assets` 同步）。
- 范围规则挂在厂商下（`/api/vendors/{id}/scopes`，pattern 为 `域名` 或 `*.域名`）；新增/删除返回 202 + `task_id`，`GET /api/scope-tasks/{id}` 查进度；按 id 分批更新、只改有变化的行，不长时间锁表。重算幂等：进程启动时自动重跑 pending/running 的任务（从头重算、计数清零）。
- 根域格式校验，被任一厂商占用返回 409。`POST /api/scans` 手动触发（kind 只允许 passive/active/expand/service，dns/http 是链路内部步骤；kind=active 需厂商 `active_enabled`，否则 409；`service` 走端口指纹，受 T49/T50 门控，关闭开关时任务仍完成但不拨号）。
- 扫描授权范围（T49）：`GET/PATCH /api/service-scan`（`{enabled, items}`，默认 `enabled=false`）；`POST /api/service-scan/scopes` 登记域（`example.test` 覆盖该域及其子域，`*.example.test` 只匹配子域，不接受 IP）；`DELETE /api/service-scan/scopes/{id}`。认证同现有 API。此范围独立于厂商 `in_scope` 规则，只约束非 Web 探测。
- 服务探测结果（T55）：`GET /api/service-results`（`q`、`port`、`response_kind=none|banner|probe|reverse_proxy`、`include_closed` 默认 false、`limit`/`cursor`）返回 `items`、`next_cursor` 与 `stats`（`open`/`verified` 只计已验证、`tcp_reachable` 计全部开放、`named`、`reverse_proxy`、`total`）；`GET /api/service-results/{id}` 查单条。不返回 IP 与 banner 原文。`GET /api/assets/{id}` 详情附带 `services`（同字段）。
- 待确认根域：`GET /api/vendors/{id}/root-candidates`，`POST /api/root-candidates/{id}/confirm|ignore|reassign`（仅 pending；确认时域名已是别厂商根域返回 409，候选保持 pending）。`GET /api/events` 里资产级 new 事件嵌套 `url_events`（该资产的 URL 级 new 事件）。子公司导入：`POST /api/vendors/{id}/subsidiaries`（CSV：名称,持股比例）。

## MCP server（T13）

Streamable HTTP（无状态、JSON 响应），地址 `POST /mcp`，认证同 API（`Authorization: Bearer <token>`）。基于官方 `modelcontextprotocol/go-sdk`（Apache-2.0/MIT）。

| 工具 | 参数 | 说明 |
|---|---|---|
| list_new_assets | vendor, since, include_wildcard, limit, cursor | 资产级 new 事件（新→旧），嵌套 url_events；默认过滤 wildcard |
| search_assets | q, vendor, in_scope, new_since, include_wildcard, limit, cursor | 同 `GET /api/assets` |
| check_scope | host | 归属厂商/根域（最长匹配）、in_scope、命中规则、已知资产 id；不要求已是资产 |
| get_service_scan | 无 | 同 `GET /api/service-scan`：非 Web 探测开关与授权范围列表，默认关 |
| list_service_results | q, port, response_kind, include_closed, limit, cursor | 同 `GET /api/service-results`：探测结果与 verified/response_kind 统计；不返回 IP 与 banner |
| check_service_scan | host | 判定该主机是否会被允许做非 Web 探测（enabled / authorized / cdn_fronted / allow / reason）；不发起探测。CDN 前置时 allow=false、reason=cdn_fronted（开关关闭时 reason 仍为 service_scan_disabled） |
| trigger_scan | root_id, kind | kind 只允许 passive/active/expand/service；active 需厂商开关；service 为非 CDN 源站端口指纹 |
| claim_mine_targets | claimed_by, limit, vendor_id, since, q, include_abandoned | 同 `POST /api/mine/claim`；agent 自助领取 |
| heartbeat_mine | asset_ids | 同 `POST /api/mine/heartbeat`；续认领锁 |
| complete_mine | asset_ids, status, note, report_url | 同 `POST /api/mine/complete`；回写结果 |
| get_agent_guide | 无 | 返回 Agent 使用说明 markdown（T36，同 `AGENT_GUIDE.md`） |

`GET /api/check-scope?host=` 与 `check_scope` 共用同一实现，输出一致（非法 host 400）。

**DNS rebinding 防护（保留，go-sdk 默认开启）**：请求如果经回环地址（127.0.0.1 / ::1）到达后端，而 Host 头不是回环地址（如 `assets.example.com`），`/mcp` 返回 403（纯文本，非 JSON）。只影响 `/mcp`，`/api/*` 不受影响。反代部署：
- 反代与后端同机、后端监听 `ADDR=127.0.0.1:8080`：反代转发时把 Host 设为上游地址，nginx 用默认的 `proxy_set_header Host $proxy_host;`（即 `127.0.0.1:8080`），**不要**设成 `$host`；Caddy `reverse_proxy` 需加 `header_up Host {upstream_hostport}`。
- 反代与后端不同机（后端监听内网地址如 `10.0.0.5:8080`）：请求不经回环，防护不触发，Host 可原样透传。
- docker compose：前端 nginx（`../frontend/nginx.conf` 的 `location /mcp`）经 compose 内网转发到 `backend:8080`，请求不经回环地址，**DNS rebinding 防护在这条路径上不生效**，由 Bearer token 认证兜底（宿主端口仍只绑定 127.0.0.1）。nginx 保持默认 `Host $proxy_host`、`proxy_http_version 1.1`、`proxy_buffering off`。
- 不要为此关闭防护；客户端直连本机时用 `http://127.0.0.1:8080/mcp` 或 `http://localhost:8080/mcp`。

业务错误返回工具结果 `isError: true`，文本 `{"error":"..."}`。curl 示例：

```
curl -s http://127.0.0.1:8080/mcp -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"check_scope","arguments":{"host":"www.example.com"}}}'
```



### 定时调度（T15）
- 进程内调度：到点为**所有根域**触发 `passive`；为 **`vendors.active_enabled=true`** 的根域触发 `active`。`SCHEDULE_SERVICE` 默认禁用；配了间隔才为所有根域入队 `service`。不自动跑 expand（仍手动）。
- 若该根域已有同 kind 的 `pending`/`running` 任务则跳过，避免堆积。
- 间隔可读环境变量；运行中改配置需重启进程，或测试里 `SetConfig` 热更新（会打断当前等待，新间隔立即生效）。

### 压测（T16）
- 10 万条模拟子域：`TEST_BENCH=1 TEST_DATABASE_URL=... go test -count=1 -run TestBenchListAssets -timeout 30m ./backend`（默认跳过）。
- 验收：`GET /api/assets` 列表（limit≤50）单次 < 1s；报告见 `bench.md`。

## Web 端（T14）
- React + Vite + Ant Design 5（MIT，深色算法主题，不做浅色切换）+ swagger-ui-dist（Apache-2.0）做 `/docs`。
- 首屏输入 API token，存 `localStorage`；侧栏：新增播报 / 资产 / 厂商·根域 / 客户托管规则 / 扫描任务 / 扫描授权范围 / 测绘 Key / API Key / API 文档。
- 资产列表可显示 CDN 前置标签（`cdn_fronted` / 厂商名）；任何列表与详情都不展示解析 IP（T28 / T50）。
- 测绘 Key（T30）：FOFA/Quake/Hunter 可在网页增删改，库内保存；API/页面只返掩码（后 4 位）；「测试连通」只调账户信息接口（mock 测），不耗查询额度。
- 厂商 `active_enabled` 开关走 `PATCH`；删厂商/根域二次确认；资产 `include_wildcard`；事件 `level` + 每 30s 轮询 `since`。
- http 扫描标题按 Content-Type/meta 声明的 charset 解码（`golang.org/x/net/html/charset`），无法判定时回退 GB18030；title、redirect、被动子域名入库前清洗为合法 UTF-8；单个 host 的 URL 入库失败只记日志，任务仍为 done，`result` 记 `store_failures` 并标 `degraded:true`（T33）。历史失败任务不自动重跑。
- 扫描记录删除（T35）：`DELETE /api/scan-tasks/{id}` 只删终态（done/failed/cancelled）记录，返回 204；pending/running/retrying 或仍有进行中的下游任务返回 409。只删这一条（下游记录保留，其 parent_id/root_scan_id 置空），不影响 assets/urls/events。页面上非终态的删除按钮置灰，删除需确认。
- 扫描列表展示 `degraded`/`degraded_reason`；`passive`/`active`/`expand`/`service` 可重跑（再 `POST /api/scans`）；`dns`/`http` 无重跑钮，提示从 passive 重跑。
- 本地：`cd frontend && npm install && npm test && npm run build`；`npm run dev` 反代到 `127.0.0.1:8080`。

## 组件库存（T43）

从已探测 URL 的 `urls.tech`（wappalyzer）聚合开源组件。升级时迁移会回填现有技术栈，不必重扫。名称若带版本（第一个 `:` 之后），版本只作展示，不参与唯一键，也不自动对漏洞库。

默认统计口径与资产列表一致：排除泛解析、客户托管，且只统计仍存活（`alive`）的资产；可切换「含泛解析 / 含客户托管 / 含失活」。列表默认再排除「常见」组件（内置可编辑名单，启动时补种；停用或删除后升级不会加回）。

- Web：导航「组件」（资产之后，全宽）。列：组件名、来源、URL 数、资产数、厂商数、是否常见；默认按厂商数、再按资产数降序。筛选：关键字、最小资产数、最小厂商数、排除常见、含泛解析/客户托管/失活，以及 T63 的 `min_confidence`、`exposure` 多选（见下节）。下钻可按厂商筛，并复制本组件资产主机名或 URL（当前筛选全量、去重，规则与资产页相同）；展开行与详情抽屉展示组件-资产关系的 8 字段（T63）。
- REST：`GET /api/components`、`GET /api/components/{id}`、`GET /api/components/{id}/assets`、`GET /api/components/{id}/values`；常见名单 `GET/POST /api/common-components`、`PATCH/DELETE /api/common-components/{id}`。
- MCP：`search_components`、`list_component_assets`、`ai_review_component`（写回，见「候选 repo、AI 写回与前端（T63）」），口径与 REST 一致。
- 后续 HTTP 扫描更新 `urls.tech` 时同步维护关联。

岸线只做识别、聚合、筛选和下钻，不内置代码审计、不按版本自动匹配 CVE。


## 组件富集（T44）

用户可为组件填写 `github_repo`（格式 `owner/repo`，非法请求 400）。后台异步向 GitHub 公开接口拉取 `stars`、向 OSV 公开接口按仓库 purl（`pkg:github/owner/repo`）拉取公开公告并去重写入 `advisory_count`，结果缓存在组件行上。可选环境变量 `GITHUB_TOKEN`（提高 GitHub 限额；未配置也可访问公开接口）。任一侧失败则该组件标记 `enrich_error`，**不挡住列表**。列表可用 `max_stars` / `max_advisories` 上限筛选，按 `stars` 或 `advisories` 升序（空值在前），或筛 `missing_repo=true` 看未关联仓库的组件。公开披露数仅供筛选，0 条不代表安全。

| 变量 | 默认 | 说明 |
|---|---|---|
| GITHUB_TOKEN | 空 | 可选。有值时 GitHub 请求带 `Authorization: Bearer`；为空仍访问公开接口，失败则该组件富集标失败 |


## 识别面加深（T45）

新的 http 扫描（含后续复扫）在现有 GET `/` 之外，会记下少量响应头（`Server`、`X-Powered-By`、`X-Generator`，以及规则中出现的头），并在页面声明了同主机 favicon 或回退 `/favicon.ico` 时再发 **一次** GET，计算 sha256 写入 `urls.favicon_hash`。走现有 HTTP 限速，不跟随站外跳转，不目录爆破，不扫其它路径。内置少量标题/响应头规则把命中写成组件来源 `header` 或 `title`（与 wappalyzer 并存；若同一组件已有 wappalyzer 关联则保留其来源）。

新证据只在新的 http 扫描写入。迁移只加列，**升级时不对历史资产对外补探测**；历史 URL 会在下一次常规 http 复扫时加深识别。


## 未识别走 LLM（T46）

仅对「已有 HTTP 响应、且未关联任何非常见组件」的 URL，在新的 http 扫描入库后可选调用 OpenAI 兼容 Chat Completions。输入只有标题、已记录的少量响应头、favicon hash、主机与路径（截断），不传完整正文、不传 IP。同一 URL 在这些证据未变时不重复调用。模型建议入库为 `source=llm`、`confirmed=false`，不进入正式库存统计；用户在「组件」页确认后才计入，并可选用其 `github_repo` 走 T44 富集，或忽略。

| 变量 | 默认 | 说明 |
|---|---|---|
| LLM_API_URL | 空（关闭） | OpenAI 兼容接口根或完整 `/chat/completions` 地址；空则功能关闭，扫描不受影响 |
| LLM_API_KEY | 空 | 有值时请求带 `Authorization: Bearer` |
| LLM_MODEL | 空 | 模型名；与 URL 都非空才视为已配置 |
| LLM_RPS | 1 | 模型调用每秒上限（须为正数） |


## 组件分类降噪（T47）

`components.categories` 存 Wappalyzer 分类名（可空）。新的 http 扫描用 `FingerprintWithInfo` 把分类随组件写入。升级或启动时按组件名查本地指纹库回填历史分类，不重扫、不访问外网；查不到的留空。写入或回填时已有非空分类一律保留，仅当已有分类为空时才用指纹库分类补上。

`GET /api/components` 与 MCP `search_components` 默认 `include_all_categories=false`：仅当一个组件的**全部**分类都落在低价值集合时才隐藏（JavaScript libraries、JavaScript frameworks、UI frameworks、Font scripts、Analytics、Tag managers、Advertising、CDN、PaaS、IaaS、Cookie compliance、CAPTCHAs、WAF、Rich text editors、Maps、Documentation、Static site generator）。分类为空，或含任一非该集合的分类（如 Web servers、Web frameworks，或未列出的 Security 等）仍显示。`include_all_categories=true` 展开全部；`category` 多值按分类名并集精筛（指定后不再套用默认隐藏）。返回项含 `categories`。不删除已识别组件。


## 组件列表整列复制（T48）

组件列表「复制本列」复制**当前筛选条件下的全部匹配组件名**（非仅本页），一行一个、去重、按名称排序。口径与 `GET /api/components` 相同（关键字、常见排除、泛解析/客户托管/失活、冷门分上限、分类隐藏与 `category` 精筛等）。`GET /api/components/values` 返回 `{field, items, count, truncated}`，`field` 为 `name`（可省略）；上限与资产页相同（100000），超出则截断并 `truncated=true`。HTTP 下自动回退 textarea，失败弹出手动复制框。下钻详情的 `GET /api/components/{id}/values` 不变。

## 扫描授权范围（T49）

非 Web（端口/服务）探测的安全地基，供后续 T51 接入，本轮不实现端口扫描，也不做 CDN 判定。

- 全局开关默认关闭。关闭时 `runServiceScan` 完全不调用探测器（测试用计数假探测断言零外连），现有 Web 扫描不受影响。未在库中写入过时可读环境变量 `SERVICE_SCAN_ENABLED`；一旦经 `PATCH /api/service-scan` 写入则以库为准。
- 授权范围按域登记，与厂商资产「范围规则 / in_scope」相互独立：登记 `example.test` 覆盖该域及其子域（资产集合）；`*.example.test` 只匹配子域。无明确范围视为不在范围内。厂商 `in_scope=true` 不能绕过本范围。不接受 IP（含规范点分四段、libc `inet_aton` 可解析的简写/八进制/十六进制，以及末级标签全是数字的写法如 `127.1`、`1.2.3.4.5`）；REST/前端/MCP 一律拒绝，API/UI 不返回 IP。
- 开启但主机不在范围时跳过，并在 `service_scan_skips` 记录原因 `out_of_scope`；关闭时不按主机记跳过。在范围内才执行传入的 `ServiceProbeFunc`。
- Web「扫描授权范围」页可开关并增删范围；REST 认证同现有 API；MCP：`get_service_scan`、`check_service_scan`。

## CDN 识别门控（T50）

对子域做 CDN/WAF 判定，作为端口扫描（T51）之前的第二道门。本轮不实现端口扫描。

- 判定数据均为本地静态、可扩充，不调用付费接口：内置 CNAME 后缀库（阿里、腾讯、字节、网宿、Cloudflare、Akamai、CloudFront、Fastly 等）；可选文件 `CDN_CNAME_FILE` / `CDN_CIDR_FILE` 追加后缀与 IP 段；未命中时仍可用 cdncheck（MIT）内置段。测试只用文档保留段（`192.0.2.0/24`、`198.51.100.0/24`、`203.0.113.0/24`）与 `*.example.test`，经 mock DNS 注入，不触真实 DNS。
- 命中则资产标记 `cdn_fronted`（并保留原有 `cdn` 厂商名），**跳过端口扫描**，仍走现有 HTTP 识别（dns 任务照常把解析成功的主机交给 http）。
- 解析 IP 只在流水线内存中用于泛解析与 CDN 判定，每次 DNS 扫描重解析，不落库。
- T51 接入点：`planOriginServiceScan` / `runOriginServiceScan`。顺序与 T49 一致：先看 `SERVICE_SCAN_ENABLED`（关闭则完全不入队），再看授权范围（超范围记 `out_of_scope`），然后跳过 `cdn_fronted`（记原因 `cdn_fronted`）；余下源站进入端口扫描队列。不改 T49 的 `runServiceScan` 实现。
- MCP `check_service_scan` 增加 `cdn_fronted`；开启且在范围内的 CDN 主机会 `allow=false`。API/UI/MCP 响应不含 IP 字段。

## 端口 + 服务指纹（T51）

只对 T49 开关开启、在授权范围内、且未 `cdn_fronted` 的源站执行；入口 `fingerprintOriginServices` 经 `runOriginServiceScan` 走 T49/T50 门控，不改门控实现。主机校验复用 T49 修复（IP、`inet_aton` 简写、全数字末级标签一律不拨号）。

- 扫描类型 `service`（架构 11-15 定）：与 dns/http 一样进入 `scanKinds`、独立 Asynq 队列、`RATE_SERVICE`（默认 1 任务/秒）和 T40 分片（`HOST_CHUNK_SIZE`）。可经 REST `POST /api/scans` 与 MCP `trigger_scan` 手动触发（写入该根域非泛解析资产主机并入队）。**不**自动接在 dns/http 之后。`SCHEDULE_SERVICE` 默认空（禁用）；配了间隔才按计划为所有根域入队 `service`，仍受 T49 开关、授权范围和 T50 `cdn_fronted` 门控；开关关闭时任务会完成但不拨号。无 expand 那样的厂商级 409；同一根域已有 pending/running 的 service 则跳过（与 passive 相同）。
- 开关关闭时：触发仍返回 202 并产生 `service` 任务，worker 跑完标记 `done`，`planOriginServiceScan` 不入队、不拨号、不写 `asset_services`。


- 每个端口至少建立一次 TCP 连接；未声明 `probes` 的端口只读初始 banner、不写；声明了 `probes` 的端口每个 probe 用一次独立连接并读响应，响应同时匹配 `expect` 与规则 `banner` 时停止并采用，否则保留最后一段 banner 供 T52 归入未识别（是否识别仍由 `banner` 正则决定）。报文全部来自 `fingerprints/services.yaml`：PostgreSQL SSLRequest（8 字节 `00 00 00 08 04 d2 16 2f`，只问是否支持 TLS，不发 StartupMessage/用户名）、AMQP 协议头（`AMQP 00 00 09 01`，不发 start-ok/登录）、Redis `PING`、Memcached `version`、MongoDB OP_MSG `hello`、Kafka `ApiVersions`、Elasticsearch/etcd/Nacos 只读版本端点 `GET`。
- 识别规则来自 YAML（`name` / `component` / `category` / `confidence` / `ports` / `banner` / `version` / `probes`），启动时预编译；代码内不再有规则表。MySQL/MariaDB 按协议 v10 握手包解析：`caching_sha2_password` 的 8.0.36 识别为 mysql/8.0.36；`5.5.5-10.11.6-MariaDB` 版本取 10.11.6。
- 结果存 `asset_services`（主机名 + 端口唯一；open/verified/response_kind/name/version/banner 摘要），不含 IP 列。banner **先脱敏再截断到 512**：IPv4、IPv6（含 zone、`[addr]:port`、v4-mapped **整体**替换，不留 `::ffff:`）、`::1`、`127.1`、`0x7f000001`、`0177.0.0.1` 以及大于 65535 的十进制 inet_aton 整数（如 2130706433）换成 `[ip]`。三节全十进制版本号（8.0.36 / 10.11.6 / 5.5.5）不替换；两节全十进制除 127.x / 0.x 外不替换，以免误伤 `8.0`、`10.1` 这类版本。若两段或三段十进制简写紧跟明显 IP 语境前缀（`addr=`、`host=`、`from`，忽略大小写、独立词），仍替换为 `[ip]`，例如 `host=10.1.2`、`from 10.1`；无这类前缀的 `MySQL 8.0.36`、`version 10.11.6` 保留。识别结果由 T52 同步写入组件库存；探测结论经 `GET /api/service-results` 只读暴露（T55）。
- 未做 CDN 判定的资产（`cdn_fronted` 为 NULL）跳过端口扫描，记 `service_scan_skips.reason=cdn_unknown`，不按非 CDN 放行。只拨 CDN 判定时解析到的 IP（只在内存传递，不落库、不进响应或日志）。默认拨号器不再自己解析 DNS，且只接受字面 IP。拨号前拒绝回环、私网、链路本地、未指定地址和云元数据 `169.254.169.254`（含 v4-mapped），记 `private_address`。解析失败/负答案按 `dns_nxdomain` / `dns_nodata` / `dns_timeout` / `dns_error` 细分，负缓存命中记 `dns_negative_cached`（见下节）；不再写 `no_dial_ip`（历史行保留原值）。测试中的 127.0.0.1 假服务只通过注入拨号器或策略放行，没有生产可用的环境变量开关。
- 端口指纹限速器为进程级共享（burst 1），多个 service 任务并发时速率不叠加。
- 局限：未声明 probes 且不主动发 banner 的服务仍会记为开放但 name 为空；在 YAML 为其补一条只读 probe 即可纳入识别。


## 非 Web 服务并入组件库存（T52）

`service` 扫描在 `fingerprintOriginServices` 写完 `asset_services` 之后，由 `serviceScanHandler` 调用 `syncAssetServicesToComponents` 入库（不在指纹函数内写入，以免无触发的指纹调用改库存）。来源按扫描结果：`asset_services` → `components`，新建行 `source=service`，不含 IP。

- 关联：服务指纹通常没有 HTTP URL，新增 `asset_components`（资产 + 组件 + 端口，证据 JSON 含 host/port/banner 摘要与规则 `confidence`）。现有组件统计、下钻、整列复制 SQL 对 `url_components` 与 `asset_components` 做 UNION，筛选/降噪/alive 口径与 T43 一致。不新增 REST/MCP 路由，OpenAPI 仍与 `routes()` 对齐。
- 分类：先走 Wappalyzer `categoriesForName`（MySQL/PostgreSQL/Redis/MongoDB/Elasticsearch 等）。Wappalyzer 没有时用 YAML 规则的 `category`（Kafka/RabbitMQ → Message queues，Memcached → Caching，etcd/Nacos → Application server）。已有组件的非空 `categories` 仍走 T47 `CASE`，不覆盖；`name_key` 冲突时不改已有 `source` / `confirmed`，也不删除已识别组件行或其它来源的关联。
- 未识别：端口**已验证**（`response_kind` 为 `banner`/`probe`）、banner 非空、又没有指纹短名时，按归一化后的脱敏 banner SHA-256 聚类，名称 `unidentified-service-<12 hex>`，`confirmed=false`，不进默认正式列表。可在「待确认候选」中确认或忽略（LLM 未配置时也展示 service 候选；忽略写 `asset_components.ignored_at`）。关闭端口、开放但无 banner（仅 TCP 可达）或反代/CDN 响应都不归档。补 YAML 指纹后由 T53 启动重识别自动回填。
- 服务扫描拿到 banner 或识别成功时，把该主机标为 `alive`（写 `last_alive_at`、清空 `dead_reason`；已 alive 不再改；从不因端口关闭或无 banner 改成 dead）。默认正式列表仍按 T43 过滤 `alive='alive'`，因此这类主机不会被默认列表隐藏。

## 可插拔 YAML 指纹库（T53）

服务指纹规则集中在 `backend/fingerprints/services.yaml`，经 `go:embed` 编入二进制；识别、版本提取、探测报文都只读这份规则，新增服务只改 YAML，不改扫描代码。

- 规则字段：顶层 `version: 1`；每条规则含 `name`（小写短名，写入 `asset_services.name`）、`component`（库存展示名，省略时用 `name`）、`category`（Wappalyzer 分类名，Wappalyzer 查不到时作为组件分类）、`confidence`（`high` / `medium` / `low`）、`repo`（可选，官方开源仓库 `owner/repo`，仅供 T54 评分）、`ports`（1–65535）、`banner`（必须命中的正则）、`version`（可选，需带捕获组）、`probes`（可空；每项 `text` 或 `hex` 二选一，加 `expect` 响应正则）。
- `probes` 只允许无认证、不读写业务数据的只读识别报文，单条不超过 256 字节。未声明 `probes` 的端口只读初始 banner、不写；声明后每个 probe 用一次连接，响应同时匹配 `expect` 与规则 `banner` 时停止并采用，否则保留最后一段 banner 供 T52 归档。默认扫描端口等于全部规则声明的端口，`SERVICE_SCAN_PORTS` 可覆盖。
- `SERVICE_FINGERPRINT_RULES` 指向额外 YAML，与内置规则合并：**同名规则整条覆盖**，其余追加；不接受删除内置规则。加载在启动时完成，正则预编译，扫描热路径不再编译；改规则需重启（随发布更新，不做热加载）。
- 启动校验失败即拒绝启动（`main` 中 `log.Fatal`）：Banner/version/expect 正则不可编译、端口不在 1–65535、`confidence` 不是三个枚举值、同一文件内 `name` 重复或非法、`category` 为空、`repo` 不是合法 `owner/repo`、probe 同时给 `text` 与 `hex` 或都为空、probe 超过 256 字节、YAML 出现未知字段。
- 置信度随规则写入 `asset_components.evidence.confidence`（不改表结构），例如 `{"host":"...","port":6379,"banner":"...","confidence":"high"}`。
- 内置 10 条规则声明官方 `repo`（`mysql/mysql-server`、`postgres/postgres`、`redis/redis`、`mongodb/mongo`、`elastic/elasticsearch`、`apache/kafka`、`etcd-io/etcd`、`alibaba/nacos`、`rabbitmq/rabbitmq-server`、`memcached/memcached`）。识别写入组件时，仅当 `components.github_repo` 为空才填入，不覆盖用户已填值，也**不**自动发起外网富集（`stars` / `advisory_count` 仍由用户在组件页填 repo 或后续富集写入）。
- 启动时 `reidentifyStoredServices` 用当前规则重匹配 `asset_services` 中 `open=true`、`banner` 非空且 `name` 为空或以 `unidentified-service-` 开头的记录；命中则回填 `name` / `version` 并同步 T52 组件。新建的识别组件按一次新扫描的口径 `confirmed=true`（已存在组件的 `confirmed` 仍按 T52 冲突口径不覆盖）；同资产同端口旧的 `unidentified-service-*` 关联置 `ignored_at`，避免同一端口同时出现在正式列表和待确认候选。无资产关联（`asset_id` 为空）的历史行只回填 name/version，不进组件库存。重识别失败只记日志，不阻塞服务启动。
- 不新增 REST/MCP 路由。

## 研究价值评分（T54）

`components.research_score` / `research_score_parts` / `research_scored_at` 由 `recomputeResearchScores` 批量重算，列表只按落库列排序/筛选，不在查询时动态计算。**四维等权，各 0.25；总分 0–100，落库两位小数**：`score = round((attack + source + advisory + usage) × 0.25 × 100, 2)`，`research_score_parts` 存四维原始分（0–1）。

| 维度 | 映射 | 说明 |
|---|---|---|
| 攻击面 attack | `source=service` 且分类含 `Databases` / `Caching` / `Message queues` → 1.0；其它 `service` → 0.75；Web 组件 → 0.4；无分类 → 0.3 | T52 写入的 service 组件一定带分类，空分类档只出现在 Web 组件 |
| 源码可得性 source | `github_repo` 非空且富集成功（`enriched_at` 非空、`enrich_error` 空）→ 1.0；repo 为空或尚未富集 → 0.5；repo 非空但富集失败 → 0.3 | 闭源/找不到 repo 无法白盒，降权 |
| 公开披露稀缺 advisory | `advisory_count IS NULL` → 0.5；`0` → 1.0；`n>0` → `1/(1+n)` | **0 披露只用于排序，不代表安全**；API 不输出任何「安全」结论字段 |
| 本 SRC 使用频度 usage | 该组件在单一厂商内出现的最大去重资产数 `n`（`url_components` 与 `asset_components` 两条来源合并去重），`n/(n+5)` | 全局口径，与当前 vendor 筛选无关；覆盖厂商数只展示、不参与评分 |

- 默认统计口径与组件列表一致：排除泛解析（`wildcard`）、客户托管（`customer_hosted`）、非 alive 资产，只统计 `confirmed` 组件的关联。列表切换 `include_wildcard` / `include_customer_hosted` / `include_dead` **不触发重算**，分数仍是默认口径的全局分。
- 重算时机：启动时、T44 富集完成写入 `stars` / `advisory_count` 后、T52 `syncAssetServicesToComponents` 写完服务关联后。三处重算失败都只记日志，不阻塞启动、不影响富集与扫描结果。一次聚合查询 + 分批（1000 行）`UPDATE`，不逐组件查询；重复执行幂等（`research_scored_at` 会刷新）。
- 列表：`GET /api/components` 支持 `sort=research`（`research_score DESC, id DESC`，游标规则与其它排序一致）与 `min_score`（0–100，含端点，越界 400）；返回项含 `research_score` 与 `research_score_parts`。`GET /api/components/values` 沿用同一 `min_score` 筛选。MCP `search_components` 同步 `sort=research` 与 `min_score`。前端「研究价值」列显示 1 位小数，tooltip 展示四分项。
- 新识别的组件在下一次重算（默认是下一次启动/富集完成/服务同步）时获得分数；未重算前为默认 0 分与空分项。

## 服务探测三态与反代识别（T55–T57）

`asset_services` 增加 `response_kind`（`none` / `banner` / `probe` / `reverse_proxy`）与 `verified`。空 banner 只表示 TCP 可达（`open=true`、`verified=false`、`response_kind=none`），不算已确认开放服务；只有收到初始 banner 或 YAML 探测响应才 `verified=true`。迁移 `032_service_response_kind.sql` 为旧行回填 `verified=false`、`response_kind=none`，可重跑。启动时 `backfillServiceResponseKinds` 把 `none` 历史行按已存 banner 重新分档（含反代），不覆盖新扫描结果。

- 分档：无 probes 的规则 `banner`；有 probes 且匹配成功 `probe`；连上无响应 `none`。T52 `syncAssetServicesToComponents` 只对 `verified=true` 或 `response_kind in ('banner','probe')` 生成候选，空 banner 不生成 `unidentified-service-*`。
- 反代/CDN（T56）：HTTP 400/403 且带 `Server: openresty|nginx|Tengine`、`via: ...CHN-*`、`X-CCDN-FORBID-CODE`、`X-Cache`、`X-CDN` 特征时记 `reverse_proxy`，不生成未识别候选，并记 `service_scan_skips.reason=reverse_proxy`。迁移 `033_reverse_proxy_backfill.sql` 把历史 `asset_services` 与未识别候选按同特征归档（`ignored_at`），不删除组件，可重跑。200 应用响应不受影响。
- 聚类归一化（T57）：`normalizeServiceBannerForHash` 在 SHA-256 前按 CRLF 段去掉 `Date` / `Request_id` / `Node_info` / `Client_ip` / `Content-Length` / `via`，保留状态行、`Server` 与其余结构；同类 403 因此合并为一个候选。迁移 `034_unidentified_recluster.sql` 重算现有 `unidentified-service-*` 名称并合并关联，保留 `confirmed` / `ignored`，不删除组件行，可重跑。
- REST：`GET /api/service-results`（`q` / `port` / `response_kind` / `include_closed`；默认只列开放端口，返回 `items`、`next_cursor`、`stats`）、`GET /api/service-results/{id}`；`GET /api/assets/{id}` 详情带 `services`。默认统计 `stats.open` 只计 `verified=true`，`stats.tcp_reachable` 保留全部开放；不返回 IP，也不返回 banner 原文。MCP 增加 `list_service_results`，口径同 REST。
- Web：「扫描授权范围」页下方「探测结果」展示已验证开放 / TCP 可达 / 已识别 / 反代/CDN 计数与逐条状态（已验证 / 仅 TCP 可达 / 反代/CDN），不含 IP。

## no_dial_ip 收口（T58）

历史 `no_dial_ip` 是「没有可拨地址」的兜底原因，实测绝大多数是 DNS 负答案。现在按原因细分，`no_dial_ip` 不再写入（旧行保留原值）。

| 原因 | 含义 |
|---|---|
| `dns_nxdomain` | A/AAAA 均为 NXDOMAIN，且由另一台 resolver 复核确认（只配置一台 resolver 时直接采用其负答案） |
| `dns_nodata` | 域名存在但没有 A/AAAA 记录（权威 NODATA），或解析成功但无地址 |
| `dns_timeout` | 所有尝试均为超时，或负答案复核超时 |
| `dns_error` | REFUSED/FORMERR/SERVFAIL 等非权威回码，或负答案无法复核 |
| `dns_negative_cached` | 资产 `resolved=false` 且 `resolved_at` 在 `SERVICE_DNS_NEGATIVE_TTL`（默认 6h，允许 6–24h）内，本轮不再解析 |
| `private_address` | 解析到回环/私网/链路本地/未指定/云元数据，全部被拨号策略拒绝（保留原口径） |

- 负缓存：`resolved=false` 的资产在窗口内直接跳过，服务扫描目标枚举仍包含这些资产（不特殊过滤），只在拨号准备阶段预过滤。
- 去重：迁移 `036_service_skip_dedup.sql` 给 `service_scan_skips` 加 `scan_task_id`、`first_seen`、`last_seen`、`count`；唯一键为 `(asset_id, reason)`（`asset_id` 为空时按 `(host, reason)`）。写入改为 upsert：同一资产同一原因只保留一行，`count` 累加、`last_seen` 刷新、`scan_task_id` 取最近一次非空任务。迁移可重跑：旧行回填 `first_seen=last_seen=created_at`、`count=1`，历史重复行合并为一行并累计 `count`。
- 统计口径：按原因统计资产数用 `count(DISTINCT asset_id)`，不要用行数（行数会被多轮任务与历史重复放大）。
- 拨号前 CDN 复判：`resolverFromDNS` 保留解析链 CNAME；`prepareServiceDialIPs` 用既有 CDN 判定（CNAME 后缀 / cdncheck 段 / 静态库）复核本次解析结果，命中即记 `cdn_fronted` 并跳过。`cdn_fronted` 为 NULL（未判定）仍由 T50 门控跳过，不进入拨号准备；不改 T49/T50 门控顺序。
- 仍不写 IP：解析地址只在内存参与 CDN 匹配与拨号，不落库、不进响应/日志；跳过记录只有 host 与原因。

## 组件-资产 8 字段（T60）

迁移 `037_component_asset_fields.sql`（可重跑）给 `assets` 加 `exposure_state` / `auth_state`（默认 `unknown`，建 `assets_exposure_state_idx`），给 `url_components` / `asset_components` 加 `confidence`（默认 `low`）、`matched_url`、`candidate_repo`（默认空串，各建 confidence 索引），给 `components` 加 `repo_confidence`（默认 `low`）与 `ai_review`（jsonb，默认 `{}`）。历史行只由默认值回填，**不伪造证据**；evidence/confidence/exposure 的写入在 T61/T62。

- `GET /api/components/{id}/assets` 改为一条组件-资产关系一项：保留 `id`、`subdomain`、`vendor`、`url`、`title`、`version`、`source`、`port`、`urls`，并返回 8 字段 `component_evidence`（evidence jsonb 原样）、`component_confidence`、`matched_url`（Web 关系取具体 URL；服务关系为空）、`version_hint`、`exposure_state`、`auth_state`、`component_candidate_repo`（关系上 `candidate_repo` 为空时回退 `components.github_repo`）、`capture_time`（`created_at`，RFC3339）。分页仍按资产计，`next_cursor` 传下一页；MCP `list_component_assets` 与 REST 同口径。
- `GET /api/components`（以及 `/api/components/values`、MCP `search_components`）新增 `min_confidence=high|medium|low` 与 `exposure`（可重复或逗号分隔，T63 起接受 9 个值：`live_app` / `login_only` / `api` / `admin` / `placeholder` / `waf` / `error_page` / `static_only` / `unknown`）：只保留至少一条组件-资产关系（`url_components` 或 `asset_components`）满足置信度下限、且命中资产暴露状态落在所选集合的组件；`url_count` / `asset_count` / `vendor_count` 与列表用同一关系口径。未传这两个参数时行为不变。
- 仍不写 IP：组件关系只带 host、端口、URL 与证据，不落库、不进 API/MCP/前端；`confidence` 由岸线规则推断，LLM 不改写。

## 证据与置信度 / exposure-auth（T61/T62）

写入时机：HTTP 扫描的 `storeURL` 在写 `urls` 的同一事务里写 `url_components` 的关系证据与 `assets` 的 exposure/auth；服务组件在 `syncAssetServicesToComponents` 里写。历史行不重算，仍为 `low` / `unknown`。

- Web 证据（`url_components.evidence`）为 `{type,value,snippet}`：`type` 取 `path` / `header` / `meta` / `title` / `js` / `favicon` / `status` / `llm`（服务组件为 `banner`）。判定顺序：非 Server 响应头 > `<meta name="generator">` > `<script src>` 包名 > 资源 path（link/a/img/form 等的 href/src/action/data）> 标题 > 正文 > 仅 Server 头。
- 置信度：明确 path / 非 Server 响应头 / meta generator / JS 包名 / favicon hash = `high`；标题、正文结构、内联脚本结构匹配 = `medium`；仅 Server 头、LLM 推断、找不到来源 = `low`。找不到来源时不编造证据，evidence 记 `{}`。
- `matched_url` 写该 URL（`scheme://host:port`），服务组件为空；`candidate_repo` 由 T63 的组件名映射写入（见下节）。
- title/header 规则（`identifyRules`）同样写证据：标题记 `title`/`medium`，`Server` 头记 `header`/`low`，其它响应头记 `header`/`high`；wappalyzer 已有关联时保留其证据、置信度与 `matched_url`。无响应证据的兼容入口 `syncURLComponents`（测试/历史路径）仍写 `{"tech": ...}` 与 `low`。
- LLM 候选写 `type=llm`、`confidence=low`，保留 `reason` / `github_repo` 供 AI 关联提示。
- 服务组件：`asset_components.confidence` 取 YAML 规则的 `confidence`（未声明记 `low`），`candidate_repo` 优先取规则的 `repo`、未声明时退回 T63 的组件名映射，`evidence` 保留 `host` / `port` / `banner` / `response_kind` / `confidence` 并加 `type: "banner"`；`matched_url` 为空。
- `exposure_state`（按顺序判定）：WAF challenge/人机校验（含 Cloudflare 头 + 401/403/429/503、拦截词）→ `waf`；登录表单（含密码框）或登录标题 → `login_only`；401 与跳转登录 → 保持 `unknown`；404/500/502/503/504 → `error_page`；200 时 JSON → `api`、默认页/停放页 → `placeholder`、后台标题 → `admin`、目录列表/二进制/非 HTML 文本 → `static_only`、其余 HTML → `live_app`；其它状态码与空响应 → `unknown`。
- `auth_state`：登录表单/登录标题、401、302 到登录 → `login_required`；200 业务 JSON → `anonymous`；其余 `unknown`。
- 写库用 `UPDATE assets SET exposure_state/auth_state`；`unknown` 表示本次判不出来，不覆盖已有结论。服务/非 Web 资产保持 `unknown`，只用内存里的状态码/标题/响应头/正文判定，不落正文、不写 IP。

## 候选 repo、AI 写回与前端（T63）

**组件名 → 候选仓库映射**（`backend/component_repos.go`，`componentCandidateRepo`）：按组件名归一（小写、去感叹号、压缩空白）查表，命中写 `url_components.candidate_repo` / `asset_components.candidate_repo`，未命中记空串（不猜、不联网）。映射只做候选提示，**不写 `components.github_repo`**（那是确认仓库）。

| 组件名 / 别名 | 候选仓库 |
|---|---|
| Apache Superset、Superset | `apache/superset` |
| Jenkins | `jenkinsci/jenkins` |
| WordPress | `WordPress/WordPress` |
| Discuz! X、Discuz! | `Discuz/DiscuzX` |
| GitLab | `gitlabhq/gitlabhq` |
| Gitea | `go-gitea/gitea` |
| Nginx | `nginx/nginx` |
| Apache HTTP Server、httpd | `apache/httpd` |
| PHP | `php/php-src` |
| MySQL | `mysql/mysql-server` |
| Redis | `redis/redis` |
| MongoDB、Mongo | `mongodb/mongo` |
| Elasticsearch | `elastic/elasticsearch` |
| Kafka、Apache Kafka | `apache/kafka` |
| RabbitMQ | `rabbitmq/rabbitmq-server` |
| Memcached | `memcached/memcached` |
| Nacos | `alibaba/nacos` |
| etcd | `etcd-io/etcd` |
| Tomcat、Apache Tomcat | `apache/tomcat` |
| Spring Boot | `spring-projects/spring-boot` |

- 写入点：Web wappalyzer（`syncURLComponentsEvidence`）与 title/header 规则（`syncIdentifyComponentsEvidence`）都写 `url_components.candidate_repo`；服务（`syncAssetServicesToComponents`）写 `asset_components.candidate_repo`。
- 已有值不覆盖：关系上 `candidate_repo` 非空时，后续识别不再改写（人工/AI 填的候选保留）。服务例外仍按 T54：YAML 声明的 `repo` 优先，未声明时才用映射，映射不覆盖已有值。
- 8 字段 API 的 `component_candidate_repo` 仍是关系值优先、为空再回退 `components.github_repo`（T60 口径不变）。

**AI 写回 `POST /api/components/ai-review`**（鉴权同其它 `/api` 的 Bearer；MCP 工具 `ai_review_component` 同逻辑）

- 入参：`component_id`（必填）、`asset_id`（可选）、`repo`（`owner/repo`，空串表示不改仓库）、`repo_confidence`（`high`/`medium`/`low`）、`audit_priority`（`P0`/`P1`/`P2`/`P3`）、`reason`（≤2000 字符）。
- 校验：`component_id` 不存在 → 404；`asset_id` 不存在 → 404；`repo` 非 `owner/repo`、枚举非法、`reason` 超长 → 400。
- 写：`repo` 非空时更新 `components.github_repo`；`repo_confidence` 非空时更新；条目写入 `components.ai_review`（jsonb）。
- `ai_review` 结构：`{"by_asset": {"<asset_id>": {...}}}`，未给 `asset_id` 时键为 `"component"`；条目保存 `asset_id` / `repo` / `repo_confidence` / `audit_priority` / `reason` / `updated_at`（RFC3339），顶层另有 `updated_at`。
- **幂等**：同一 `component + asset` 重复提交**覆盖该条**（键固定，内容以最后一次为准，只有 `updated_at` 前移），不会堆出多条；不同资产或组件级各占一条，互不影响。
- 仓库真变了才清空 `stars` / `advisory_count` / `enrich_error` / `enriched_at`，避免旧仓库的富集结果挂在新仓库上；本接口不联网富集，需要时再用 `PATCH /api/components/{id}`。
- 响应：更新后的组件行（`id`、`name`、`github_repo`、`repo_confidence`、`ai_review`、`stars`、`advisory_count`、`updated_at`）。

**组件页前端（`frontend/src/pages/Components.jsx`）**

- 列表筛选新增 `min_confidence`（high/medium/low，空=不筛）与 `exposure`（多选，9 个值）；与 T60 口径一致，计数随筛选收窄。
- 展开行展示该组件前 5 条组件-资产关系的可信度、暴露、认证、候选仓库、版本线索、采集时间（避免主表硬挤列）。
- 详情抽屉的资产表展示 8 字段：子域、厂商、URL、匹配 URL、暴露、认证、可信度、候选仓库、版本线索、采集时间、证据；证据默认只给 `type / value`，点「展开证据」看 `snippet`。
- 页面不渲染 IP：关系行与证据都只有 host / 端口 / URL。
