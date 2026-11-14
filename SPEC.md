# 岸线 Shoreline（原代号 src-asset-monitor）

状态：开发完成（2026-10-21）。开源仓库 zz-protocol/shoreline；部署见 docs/DEPLOY.md。

## 目标
1. 按"厂商 → 根域"配置目标，自动收集子域名和可访问的业务 URL 并入库。
2. 定时复扫，比出新增和变更资产，在 Web 端播报。
3. 判断资产归属哪家厂商、是否在该 SRC 范围内（范围规则可配：通配、排除）。
4. 提供 REST API（附 OpenAPI 文档）和 MCP server，供其他 AI agent 调用。
5. Web 管理端：厂商/根域/范围管理、资产列表检索、新增播报流、任务状态、API key 配置。
6. 通用、可开源：MIT 许可，单机 docker compose 能起，能扩展到百万级子域名。

## 非目标
- 不做漏洞扫描、端口扫描、目录爆破或任何攻击行为。
- 第一版不做 IM 推送（TG/飞书等），只留 Webhook 接口位。
- 不做服务/端口采集，只到子域名和 HTTP(S) URL。
- 不复制 GPL/AGPL 项目代码（OneForAll、reNgine、bbot），只借鉴思路。

## 首批厂商
腾讯、阿里、字节、蚂蚁。根域和范围表由用户/src挖掘提供，系统支持随时增删。

## 发现方式（按优先级）
被动（默认开启）：
1. Certstream 实时 CT 流，按根域匹配，最快发现新上线系统。
2. crt.sh 历史证书。
3. subfinder 库的免 key 被动源。
4. 测绘 API：FOFA/Quake/Hunter（有 key 才启用）。
5. ICP 备案 + 子公司扩展，找新根域（只提示，用户确认后才加入目标）。
6. 存档（Wayback 等）。
7. 响应内容提取：证书 SAN、CSP 头、跳转、JS 中的域名。
主动（可按厂商开关，默认限速）：
8. DNS 字典爆破（puredns 式可信 resolver 复核）。
9. 变形（alterx 式）。
10. 新发现子域递归一层。

## 验证流水线
解析(dnsx) → 泛解析过滤（IP 比对 + 页面相似度）→ CDN 标记 → HTTP 探测（httpx，限速，只 GET /，记状态码/标题/指纹/跳转）→ 范围判定。

## 新增与变更判定
- 资产唯一键：子域名；URL 唯一键：scheme+host+port。
- 首次出现 = 新增；CNAME 集合、状态码、标题或指纹 hash 变化 = 变更（不再保存或比较 IP）。
- 每条记录 first_seen、last_seen、来源列表。

## 接口草图
- GET/POST /api/vendors, /api/vendors/{id}/roots, /api/scopes
- GET /api/assets?vendor=&new_since=&in_scope=&q=
- GET /api/events?type=new|changed&since=
- POST /api/scans（手动触发），GET /api/scans/{id}
- MCP tools：list_new_assets、search_assets、check_scope、trigger_scan
- 认证：API token。

## 技术选型
- 后端 Go：主流工具都是 Go，可直接引用 ProjectDiscovery 的 MIT 库，单二进制好分发。
- 任务队列 Asynq（基于 Redis）：简单，够用；被动/DNS/HTTP 三类 worker 分开限速，可横向扩。
- 存储 PostgreSQL 存资产与事件；Redis 做队列和去重。ClickHouse 不进第一版。
- 前端 React + Vite + 一个成熟组件库，深色主题。
- 部署形态：docker compose（开发阶段只写好，不上线）。
- 默认频率：Certstream 常驻；被动源每 6 小时；主动每天一次，均可配。

## 验收标准
1. docker compose up 后，Web 端可增删厂商、根域、范围规则。
2. 对一个测试根域跑被动收集，资产入库并显示来源。
3. 二次扫描后，新出现的子域出现在新增播报流，未变的不重复播报。
4. 泛解析测试域不产生大量假资产。
5. HTTP 探测结果含状态码、标题，速率受配置限制。
6. 范围判定：通配和排除规则对测试用例全部正确。
7. API 和 MCP 能列出新增资产、按条件检索；OpenAPI 文档可访问。
8. 用 10 万条模拟子域压测：入库和列表查询不卡（列表接口 < 1s）。
9. 代码中无 GPL/AGPL 代码，LICENSE 为 MIT。

## 补充决定（2026-10-21）
- 开发环境：允许在 box 装 docker，仅用于本地构建和测试，不对外暴露、不部署。
- 组件库：Ant Design 5（深色算法主题）。
- /healthz 检查 Postgres 和 Redis 连通，任一不通返回 503。
- 前端容器用生产构建 + nginx 托管；本地开发仍可用 Vite dev。
- 限速按单进程计，总速率 = 单实例速率 × 实例数（写进 README）。
- 扫描链路自动串联：passive → dns → http（只传解析成功、非泛解析的）。
- 失败重试最多 3 次，指数退避，可配置；重试期间状态记为 retrying，用尽后记 failed，failed 是终态。
- 泛解析：命中的子域不删，标 wildcard=true；API/列表默认过滤，不产生新增事件，可用参数查看。
- 泛解析的页面相似度判定放在 T8。CDN 识别加 cdncheck（MIT）IP 段。
- DNS 加每秒查询数限速，可配置，默认 50/s。
- Redis 换成 Valkey 8（BSD-3），保证整套都是开源许可。
- 泛解析候选主机允许做一次 HTTP GET 页面对照（受限速），任一协议页面不同即视为真实主机；相似阈值默认 0.9。
- 指纹 = sha256(status|title|tech)，不含正文。HTTP 并发可配置。
- 迁移规则：首个公开版本发布前，允许原地修改迁移（开发库可重建）；发布后只许新增迁移。根域大小写合并直接放在 004 内，先合并再加约束。
- 只改正文不算变更，符合预期。
- 泛解析 HTTP 复核：单独限速（默认 3 rps），每个根域每轮封顶 200 个（可配），超出的保持 wildcard；相似度比较前先归一化页面（去掉随机 token、数字、时间戳等动态内容）。
- 新增以首次解析成功为准；资产 new 与 URL new 都存，播报默认按资产级聚合。
- 资产变更只看 CNAME 集合。已复核为真实的主机不被 DNS 重标 wildcard，复核有效期默认 7 天。
- 事件规则：CNAME 恢复原值也算 changed；失联（解析失败）后重新解析成功发 recovered 事件；已救回的主机若再次落入泛解析判定则不报（v1 接受）。同一字段同一轮并发只记一条 changed。不再因 IP 变化产生 changed。
- 主动发现：默认关，按厂商一个总开关；开了也限速（ACTIVE_QPS 默认 20，每根域每轮候选上限 20000）。字典用 SecLists top5000（MIT）+ 可外挂 ACTIVE_WORDLIST；自写变形规则，每子域上限 50；对本轮新增非泛解析子域递归一层；可信 resolver 默认 223.5.5.5、119.29.29.29、1.1.1.1、8.8.8.8。T10 只做手动触发，定时在 T15。
- T8 封版原则：宁可漏救，不误放。对照页 3 张；父域不稳定时候选保持 wildcard；其余边角写入 README 已知局限。
- 中文域名统一转 punycode 入库。
- REST 增加 GET /api/check-scope?host=，与 MCP check_scope 对齐。

### ICP 数据源（2026-10-21 用户确认）
- 用户已有自建服务 zz-protocol/icp_yanzhenma（FastAPI，工信部备案查询）。在通用 ICP HTTP 适配器里给它加一个预设（ICP_PROVIDER=icp_yanzhenma，ICP_API_URL 指向该服务）。
- 本仓库不引入、不打包该服务代码，只走 HTTP 调用；没配置时 ICP 功能自动关闭，退回手工导入。
- 验收：用 mock 服务模拟它的响应格式，测试解析和失败降级；真实联调留到部署时做。
- 没有天眼查、企查查等 key，这部分不做。
- 补充（10-21）：ICP 失败一律标 done+degraded，不重试；前端和 API 要显示降级原因。超时 30s，RATE_EXPAND 默认 1/s。按企业名查询和 domain 字段的取法都是推断，要等真实联调确认。

### Web 端与配套 API（2026-10-21，T14 前确认）
1. 删除：DELETE /api/vendors/{id}、DELETE /api/vendors/{id}/roots/{root_id}，级联删下属数据；有 pending/running 扫描时返回 409；前端二次确认。
2. 扫描列表：GET /api/scans?vendor=&root=&status=&kind=&degraded=&limit=&cursor=，分页校验与现有列表一致。
3. 手动重跑：同根域同 kind 再 POST /api/scans；dns/http 不提供重跑按钮，提示从 passive 重跑整条链；不新增 retry 接口。
4. API 文档：前端 swagger-ui-dist（Apache-2.0）做 /docs，读 /api/openapi.yaml，用页面 token 试调。
5. 登录：首屏输入 API token，存 localStorage；API key 管理页复用 tokens 接口（创建明文只显示一次、列表、删除）。
6. UI 开关：厂商 active_enabled、资产 include_wildcard、事件 level；只做深色主题，不做浅色切换。
7. 播报流：每 30s 轮询 GET /api/events?since=，不做 SSE/WebSocket。
- 补充（10-21）：事件播报轮询 since 首次可用「现在−1天」，之后按本轮最新事件时间推进，避免重复拉取。

## 第二轮需求（2026-10-21 用户确认）
顺序：T18 → T22 → T20 → T21 → T24 → T23 → T19。新代码在 zz-protocol/shoreline 的 master 上提交，并推到远程。

- T18 Swagger 修复：openapi.yaml 里的 flow 写法中，含 {} 的描述一律加引号。加一个单测：用 YAML 解析器加载整份文档，并校验 openapi 版本字段。交付必须用真实后端打开 /docs 验证。
- T22 文案：根域、范围、待确认根域，以及 passive/active/expand 三种扫描类型，在界面上都要有中文说明（tooltip 或旁注），空状态要有引导。
- T20 播报：
  - 可按厂商筛选。
  - 时间窗可选今日、近 7 天、自定义，并显示各厂商在该时间窗内的新增数。统计接口在后端实现（例如 GET /api/events/stats?from=&to=&vendor=）。
  - 子域列、URL 列各加「复制本列」，复制当前筛选结果的全部数据，不只当前页；一行一个，去重。
- T21 探活：
  - 资产增加 alive 状态，取值 alive / dead / unknown，并记录 last_alive_at。判定为存活的条件：DNS 能解析，且 HTTP 任一协议有响应（任何状态码都算）。
  - 一旦判定存活，就不再复查。dead 的资产随定时任务复查，默认 24h，可配置。
  - 由 dead 转为 alive 时生成 recovered 事件进播报，每次转换只产生一次。
  - 列表可按状态筛选。
- T24 客户托管排除：
  - 规则表 customer_hosted_rules，Web 上可增删。命中的资产标记 customer_hosted=true，不删除；列表和播报默认隐藏，可切换查看。改规则后异步重算。
  - 内置规则清单先发给架构审核（每条附云产品名和官方文档依据），审核过即可写入，不再给用户过目（用户 10-21 授权）。
- T23 一键 expand：厂商页放显眼按钮，结果进待确认根域；运行状态要能看到。
  - 同一厂商同时只允许一个 expand（pending 或 running），由部分唯一索引保证，冲突时返回 409。只有主体名、没有根域的厂商，按钮禁用并提示先添加根域。
  - 孤儿任务：服务启动时，把状态为 pending 但 asynq 里没有对应任务的 scan_tasks 标为 failed，并写明原因；不重新入队，由用户重新触发。
- T19 换 UI：参考 grok.com 官网风格（极简、深色、大留白）。先出 2–3 张关键页面的设计稿截图（资产、播报、厂商），用户确认后再全量改。
- T20 补充（10-21 用户）：播报是可回溯的记录，不是流过即走。
  - 事件永久保留，不做自动清理，也不加保留期开关（10-21 定）。
  - 页面默认展示近 7 天的历史，分页或滚动加载。时间窗可选今日、近 7 天、近 30 天、自定义。
  - 30 秒轮询只负责把新事件追加到顶部，不替换列表。
  - 复制本列和统计都基于所选时间窗的全部记录。
- T21 定稿（10-21）：
  - recovered 只在 dead→alive 时产生，去掉 DNS 层原来那条 recovered；在同一事务内锁行，保证只产生一次。
  - 资产一旦 alive 就不再改回；passive 照常更新 CNAME 和 URL，但不动 alive。
  - dead 的资产由 SCHEDULE_ALIVE 负责复查，默认 24h，设 0 或 off 关闭；复查只针对 dead 重做 DNS+HTTP。
  - 判定在 HTTP 阶段做：DNS 能解析且任一协议有响应，即为 alive；DNS 解析失败直接 dead；还没探测过的是 unknown。复核确认为泛解析的主机保持 unknown。
  - 迁移回填：解析成功且有 URL 记录的标 alive，其余标 unknown；回填不产生事件。
- 播报口径（10-21）：播报页默认显示 new 和 recovered 两类，「复制本列」也包含两类；统计数分开计算，页面显示为“新增 X · 恢复 Y”，各厂商的数也分开。这条并入 T25。
- T24 审核结论（10-21）：
  - 只匹配子域本身，不看 CNAME；支持 `*.` 和标签内 `*`；规则表加 builtin 和 enabled 两列，内置规则删除后升级不加回。
  - 每条规则至少要多出一层客户子域，服务端点本身不命中，用通用单测守住。
  - 共 64 条，按工程清单收录，修正如下：fc 改为 `*.*.fc.aliyuncs.com`；去掉 `*.fastly.net`；cos-website 要找到官方文档才收。
  - 不收 volccdn、又拍云、网宿、金山云、京东云、UCloud、Vercel、Netlify，由用户手动添加。
- stats 口径（10-21）：events、stats、values 三个接口不带 type 时，默认 type=new,recovered；count 等于所选各 type 计数之和；只有显式传入 changed 才统计 changed。事件清理守卫用正则递归扫描 .go 和 .sql 文件，白名单精确到具体文件和语句。

## 第三轮需求（10-24 用户）
- T26 播报搜索：播报页加关键字搜索，对子域名和 URL 做包含匹配，不区分大小写。能和厂商、时间段、类型筛选叠加；分厂商计数和「复制本列」都按搜索后的结果算。API 参数 `q`，MCP list_new_assets 同步加上。
- T27 资产按存活时间筛选：资产页可以按「最后存活时间」筛选，提供今天、近 7 天、近 30 天和自定义区间，能和存活状态筛选叠加。API 参数 `alive_since` 和 `alive_until`。
- T28 去掉 IP：前端不展示 IP，数据库也不再保存 IP。迁移时删除 IP 相关字段和表。DNS 解析结果只在内存里用于判断存活和泛解析。资产的 changed 只看 CNAME 集合是否变化。API、MCP、openapi 去掉 IP 字段，播报里不再出现 IP 变化。
  - 迁移后，历史上「只改过 IP」的 changed 行保留（UPDATE 成空 changes，不 DELETE）；列表、stats、values、MCP 默认隐藏 `type=changed` 且 changes 为空的行；count 也不计入。仍可用显式参数查看时也不返回这些空行。
- T29 资产页一键复制：资产页给子域名和 URL 列加「复制本列」按钮。复制范围是当前筛选条件下的全部结果，不只当前页；一行一个，去重。实现方式和播报页一致，在 HTTP 下要能回退。
- T30 网页配置查询 key：FOFA、Quake、Hunter 的 key 可以在网页设置页手动添加、修改和删除，保存在数据库里。页面只显示掩码（后 4 位），API 不返回明文。网页填写的优先，没有填时回退到环境变量。改完后下次扫描就生效，不用重启。每个 key 提供一个「测试连通」按钮，调用对应服务的账户信息接口，只读，不消耗查询额度。
- T31 客户托管规则支持精确层数匹配（10-24 用户）：用户要排除 `app-xxxx-vitesandbox.miaoda.cn` 这类域名，但不能误拦 `api.miaoda.cn`。规则写法放开：除了 `*.` 开头（匹配 ≥1 层）之外，也允许不以 `*.` 开头的规则，标签数必须与主机名完全一致，逐层匹配，标签内 `*` 只匹配本层（如 `*-vitesandbox.miaoda.cn` 命中 `app-8t89ep1cj4zl-vitesandbox.miaoda.cn`，不命中 `api.miaoda.cn` 和 `a.b-vitesandbox.miaoda.cn`）。校验：至少有一个标签含 `*`；最后一个含 `*` 的标签之后的固定后缀仍须是 eTLD+1 或更长；`*.` 开头的规则行为不变。规则页提示和 openapi、README 同步说明两种写法。
  - T31 补充：不以 `*.` 开头的规则拒绝连续 `**`（避免与 `*.` 混淆）；`*` 可匹配空串（与内置 `oss-*` 一致），规则落库统一小写。

## 第四轮需求（11-03 用户）：挖掘工作流
目标：自动化管理「该挖 / 正在挖 / 挖过」的域名资产，避免重复挖掘，让接入的挖掘 agent 通过 API/MCP 自助领取和回写。
- T32 挖掘认领与回写：
  - 资产增加挖掘字段（或等价侧表）：status = unset|claimed|done|deferred|abandoned；claimed_by、claimed_at、claim_expires_at；completed_at、cooldown_until；note（短结论）、report_url（可选）。
  - 冷却默认 30 天（环境变量 `MINE_COOLDOWN_DAYS`，0 表示完成后不再自动进入可领队列）；认领锁默认 24h（`MINE_CLAIM_TTL_HOURS`），超时未 complete/heartbeat 则自动回到可领。
  - API（Bearer）：
    - `POST /api/mine/claim`：按筛选领取最多 N 条（默认 20，上限 100）。默认条件：alive、非 customer_hosted、status 为 unset 或（done/deferred 且 cooldown_until 已过）或（claimed 且 claim 已过期）。支持 vendor_id、since（对应 first_seen/新资产）、q。原子领取（行锁或等价），返回资产列表；冲突不重复发给两个调用方。
    - `POST /api/mine/heartbeat`：续锁，body 含 asset_ids。
    - `POST /api/mine/complete`：回写 status（done|deferred|abandoned）、note、report_url；写 completed_at；done/deferred 时按冷却策略写 cooldown_until；abandoned 默认不再进入队列，除非显式重新 claim 时带 include_abandoned。
    - `GET /api/assets` 增加 mine_status、cooldown 等筛选；列表返回上述挖掘字段。
  - MCP：`claim_mine_targets`、`complete_mine`、`heartbeat_mine`，工具描述写清用法，便于 agent 自读。
  - Web：资产页可按挖掘状态筛选；展示最近挖掘时间/认领人；不提供「假装挖完」以外的手工编辑也可，但至少能看到状态。设置或文案里说明冷却与认领 TTL。
  - 非目标：不内置实际漏洞扫描；不存扫描原始流量；不做多租户权限隔离（仍是单实例 Bearer）。
  - 验收：两并发 claim 同一批候选，任意资产只被一方领到；heartbeat 能续期；TTL 过期后可被再领；complete 后在冷却期内 claim 不到；MCP 与 REST 行为一致；openapi/MCP 描述可读；迁移可在旧库执行。

## 第五轮需求（11-03 用户）：bug 修复与易用性

- **T33（高优 bug）非 UTF-8 标题导致 HTTP 扫描任务整体失败**。报告见 `docs/bugs/T33-utf8-title.md`。
  - 方案A：响应体按 Content-Type/meta 用 `golang.org/x/net/html/charset` 解码，无法判定时回退 GB18030。
  - 方案B：title、redirect、被动子域名入库前统一清洗为合法 UTF-8（`strings.ToValidUTF8`）。
  - 方案C：单个 host 入库失败只记日志并计入 degraded，不让整个任务失败。
  - 已失败的历史任务不自动重跑，由用户手动重新触发。
  - 测试只用 httptest，不访问真实厂商。
- **T34 厂商根域名就地添加**：每个厂商行/卡片内提供"添加根域名"按钮和输入框，厂商已预选；删除页面底部的添加表单；校验错误就地显示。
- **T35 扫描任务记录删除**：`DELETE /api/scan-tasks/{id}`，只允许终态（done/failed/cancelled），成功返回 204；pending/running/retrying 返回 409，页面按钮置灰；单条删除需确认；不影响 assets/urls/events。openapi 同步。只删本条，下游任务的 parent_id/root_scan_id 置空；下游有进行中的任务则返回 409，不做级联删除；cancelled 算终态。
- **T36 Agent 使用说明**：新增 `docs/AGENT_GUIDE.md`，在 /docs 页面展示，并通过 MCP（`get_agent_guide` 工具或 resource）提供。内容：Bearer 认证；claim→heartbeat→complete 循环的 curl 与 MCP 示例；状态含义（claimed/done/deferred/abandoned）；`MINE_CLAIM_TTL_HOURS`=24、`MINE_COOLDOWN_DAYS`=30 默认值；错误码（409 等）；可直接粘贴的 system prompt 片段，要求 agent 挖完必须带状态调用 complete。

## 第五轮补充（11-03 用户运行审计，见 docs/bugs/audit-2026-11-03.md）

- **T37 changed 事件降噪**：
  - 只有 fingerprint_hash 变化时不再产生、也不计入 changed（新值照常保存）。
  - cnames 变化时，只有第一跳 CNAME 变了才产生 changed。例如第一跳仍是 www.haimian.com.bytedns3.com，就不产生事件。不论 cdn 字段是什么都按此处理。
  - 旧事件不动。
- **T38 死亡原因**：
  - 新增 `assets.dead_reason` 枚举：nxdomain | no_http | null，由 dns/http 阶段写入。
  - 泛解析资产沿用现有存活逻辑，页面显示"泛解析"标签。
  - 页面的存活筛选把"死亡"拆成"DNS不解析"和"解析但HTTP不通"；API 增加 `dead_reason` 筛选。
  - 迁移回填：resolved=false 回填为 nxdomain；resolved=true 且已死回填为 no_http。
- **T39 被动收集统计**：
  - passive 任务的 result 写入 `{sources_ok, sources_failed:[{source,error}], found}`。
  - subfinderEnum 要把每个来源的错误暴露出来。
  - found==0 且有来源失败时，记 degraded=true 并写 degraded_reason。
- **T40 大根域分片**：
  - dns 和 http 处理器按 `HOST_CHUNK_SIZE`（默认 2000）把 hosts 分片，每片作为独立 scan_task 入队，通过 parent_id 关联父任务。
  - 父任务状态由子任务推导；重启只重跑未完成的分片。
  - 与 T23 的孤儿任务处理和 409 规则保持一致。
- **不在开发范围（用户自行运维）**：first_seen 回填 SQL、SCHEDULE_PASSIVE 调整、触发新根域。

## 第六轮需求（11-12 用户）：开源对外整理

仓库已是公开 MIT（https://github.com/zz-protocol/shoreline）。本轮只整理对外文档与元数据，让首页像开源产品，而不是内部规格。

### 目标
1. 重写根目录 `README.md`：一句话定位、能做什么、一分钟启动；详细配置与 API 迁到 `docs/`。
2. README「产品演示」一节留 3 个固定图位，先放带说明的占位图；用户按同名文件替换即可，不必改 Markdown。
3. 补 `CONTRIBUTING.md`、`SECURITY.md`；LICENSE 版权人改为 `Shoreline contributors`。
4. `SPEC.md` / `TASKS.md` 留在仓库根目录，只是不再当 README 的主要入口。

### 非目标
- 不改业务功能、API、数据库、前端逻辑、docker-compose 端口绑定或默认调度。
- 不部署、不对真实厂商扫描、不改线上环境。
- 不在仓库任何对外文档中写生产/演示公网地址或 IP。
- 不另写完整英文 README（中文为主；标题下可有一句英文定位）。
- 不删除 `research.md`、`docs/bugs/`、已有 `docs/screenshots/t14-*.png`。
- 不把 SPEC/TASKS 移出根目录。

### README 结构（按此顺序）
1. 标题「岸线 Shoreline」+ 一句话定位（攻击面的海岸线：SRC / 厂商资产发现与新增监控；被动 + 可选主动，无攻击行为）+ MIT 许可。
2. 功能要点（短列表即可）：厂商→根域配置；子域名与可访问 URL 发现；新增/恢复播报；范围与客户托管排除；REST / OpenAPI / MCP；挖掘认领与回写；Web 管理端。不写任务编号（T14 等）作标题。
3. **产品演示**：三张图，固定路径与含义（顺序不可改）：
   - `docs/screenshots/demo-vendors.png`：厂商与根域
   - `docs/screenshots/demo-assets.png`：资产列表
   - `docs/screenshots/demo-events.png`：新增播报
   用标准 Markdown 图片，alt 与图题用上述中文名。占位图须能显示（非空 PNG），图上写清「请将此文件替换为『…』截图」。用户之后用同名文件覆盖即可。已有 `t14-*.png` 保留，不充当这三张演示图。
4. 一分钟本地起：`cp .env.example .env`，填 `API_BOOTSTRAP_TOKEN`，`docker compose up --build -d`；写明 Web `http://127.0.0.1:5173`、API `http://127.0.0.1:8080`。升级注意（T28 删 IP，不可逆、先备份）保留，并链到 DEPLOY「备份与升级」。
5. 文档入口：部署 `docs/DEPLOY.md`，配置与接口 `docs/REFERENCE.md`（新建，见下），Agent `docs/AGENT_GUIDE.md`。SPEC/TASKS 如需提及，放在「开发」或页脚，不当主入口。

### 详细文档迁移
- 将当前 README 中的环境变量、被动源、DNS/HTTP、Certstream、事件、主动发现、测试、ICP、挖掘认领、客户托管、REST、MCP、定时调度、压测、Web 端等技术细节**完整迁入** `docs/REFERENCE.md`，不得丢条目或默认值。
- 迁入后允许整理标题、去掉「作为 README 首页」的口吻，但技术事实保持与迁出前一致。
- `docs/DEPLOY.md`、`docs/AGENT_GUIDE.md`、`docs/bench.md` 继续独立存在；REFERENCE 用链接指向，不复制成第二份。

### 元数据
- `LICENSE`：版权行改为 `Copyright (c) 2026 Shoreline contributors`；MIT 正文其余不动。
- `CONTRIBUTING.md`：本地启动、测试命令（后端 `go test`、前端 `vitest` / `build`）、PR 约定、禁止对真实厂商扫描、如何用同名文件替换三张演示图。
- `SECURITY.md`：通过 GitHub Security Advisories 私下报告漏洞；不要用公开 Issue 报未公开漏洞。不编造邮箱。

### 验收
- README 按上述结构可读；按一分钟步骤能找到启动命令；产品演示三张图路径、顺序、图题正确，占位图可显示。
- 现 README 中的配置/接口细节都能在 `docs/REFERENCE.md` 找到，无信息丢失。
- LICENSE / CONTRIBUTING / SECURITY 符合上文；根目录仍有 SPEC.md、TASKS.md。
- 全仓库对外文档不含生产公网地址或 IP；业务代码与 compose 行为与本轮之前一致。

## 第六轮补充（11-12 用户）：README 宣传语与技术栈

用户确认将 README 开头一段话替换为下面这版（保留现有英文一句定位或改成与中文同义的短句即可，不另写长英文）。「功能要点」列表保留；产品演示、一分钟启动、文档入口不变。

宣传语（原样写入，可分段排版，不改措辞）：

岸线帮你持续盯住 SRC 厂商的资产变化：按「厂商 → 根域」收集子域名和可访问的业务 URL，定时复扫后只播报新增和恢复，不用每次从头扫一遍。范围规则和客户托管排除用来判断该不该看；REST / OpenAPI / MCP 让 agent 可以查询、认领任务并在挖完后回写。只做发现与监控，不做攻击。MIT 许可。

技术栈（原样写入，可做小节标题「技术栈」）：

后端 Go，前端 React + Vite + Ant Design；PostgreSQL 持久化，Valkey + Asynq 做任务队列，被动 / DNS / HTTP 分类限速，大根域按片扫描、重启只重跑未完成分片。列表在 10 万级模拟子域下单次查询低于 1 秒。单机 Docker Compose 即可启动，对外提供 REST（OpenAPI）和 MCP。

非目标：不改功能代码；不部署；不改三张演示图位。

## 第七轮需求（11-14 用户）：开源组件库存与冷门筛选

目的：从已有资产里聚合开源组件，方便筛选「出现多次、公开审计和 CVE 较少」的冷门组件，用户自行做代码审计后再反打对应资产。岸线只做识别、聚合、筛选和下钻，不在产品里做代码审计或攻击。

用户确认分层方案，按任务顺序交付；T43 完成后即可使用，不必等后续任务。开发与验收不部署、不对真实厂商扫描、不写生产地址、不保存 IP。

### 非目标
- 不内置代码审计、漏洞扫描、目录爆破或任何攻击行为。
- 不根据版本自动匹配 CVE 并当成「已确认漏洞」（版本识别不准，噪音大）。
- 不把完整响应体长期入库；不保存 IP。
- 不在未配置时强制调用 LLM 或 GitHub。
- 第一版不改 README 宣传结构（文档入口可在 REFERENCE 补一节，非阻塞）。

### 数据模型
- `components`：规范化名称（大小写不敏感、唯一）、可选 `github_repo`（`owner/repo`）、`stars`、`advisory_count`（CVE/GHSA 一类公开公告的去重计数，来源可先用 OSV 公开接口）、`is_common`（是否常见排除）、`source`（见识别来源）、`confirmed`（人工确认，LLM 候选默认为 false）、时间戳。
- `url_components`：url_id + component_id 唯一；保存证据摘要（如 header 值、标题片段、favicon hash、规则名），不存全文。
- 名称规范化：从 `urls.tech` 拆出组件名；若 wappalyzer 带版本，名称与版本分开，版本只作展示字段，不参与唯一键、不自动对漏洞库。
- 默认统计口径与资产列表一致：排除泛解析与客户托管；可切换「含泛解析 / 含客户托管」。默认只统计仍存活的 URL（有存活 HTTP 记录）；可切换含失活。
- 聚合字段（查询时计算或物化均可，但必须一致）：`url_count`、`asset_count`、`vendor_count`。
- 迁移必须可在已有库上执行；T43 升级后从现有 `urls.tech` **回填**，不要求重跑扫描就能看到组件库存。

### 常见排除
- 内置一份可编辑的常见组件名单（启动时补种；可停用或删除，删除后升级不自动加回；可新增）。用于默认从「待审冷门」视图里拿掉人人都在看的通用栈。
- 至少包括：Nginx、Apache、IIS、OpenResty、Cloudflare、Akamai、jQuery、Bootstrap、React、Vue.js、Angular、Font Awesome、PHP、OpenSSL、Google Analytics / Tag Manager、Webpack、核心 CDN 标识。具体拼写与 wappalyzer 名称对齐，测试锁定。
- 列表默认 `exclude_common=true`；用户可关闭或管理名单。

### T43 组件库存（先做，可独立验收）
- Web 新增导航「组件」（放在「资产」之后），页面全宽。
- 列表列：组件名、来源、URL 数、资产数、厂商数、是否常见；默认按厂商数、再按资产数降序，并排除常见。
- 筛选：关键字、最小资产数、最小厂商数、排除常见（默认开）、含泛解析/客户托管/失活。
- 点进组件可看到关联资产与 URL（分页），支持按厂商筛选；提供「复制本组件资产主机名」和「复制 URL」，规则与资产页相同（当前筛选全量、去重、HTTP 下可回退）。
- REST：`GET /api/components`（筛选 + 分页 cursor）、`GET /api/components/{id}`、`GET /api/components/{id}/assets`（或等价嵌套）、`GET /api/components/{id}/values`（复制用）。openapi 同步。认证同现有 API。
- MCP：`search_components`、`list_component_assets`，口径与 REST 一致。
- 后续 HTTP 扫描更新 `urls.tech` 时同步维护关联，不丢回填结果。
- 验收：用测试数据（至少 3 个厂商、含常见 Nginx 和两个非常见组件名，部分标记 wildcard/customer_hosted/失活）验证默认统计不含后三类、排除常见后列表只剩非常见、计数与下钻一致、复制内容与筛选一致；迁移在已有 `urls.tech` 上幂等回填；无真实网络。

### T44 冷门分（筛选核心）
- 用户可给组件填写或修改 `github_repo`（格式 `owner/repo`，非法 400）。
- 配置可选 `GITHUB_TOKEN`（提高限额；未配置也可访问 GitHub 公开接口，失败则该组件富集标失败、不挡列表）。公开公告计数用 OSV 公开 API，按仓库或已知包名查询，失败同样降级。
- 后台异步富集 `stars` 与 `advisory_count`，结果缓存；列表可按「最多 star / 最多公告」上限筛选，并可按 star、公告数升序排序（空值排在需补仓或单独可筛「未关联仓库」）。
- 不把「0 条公告」自动解释为安全；界面注明这是公开披露数量，仅供筛选。
- 验收：mock GitHub/OSV 下写入 star 与公告数；筛选 max_stars、max_advisories 正确；无 token 时失败不 500；非法 repo 400；不访问真实外网（测试用 httptest）。

### T45 识别面加深（下次 HTTP 扫描生效）
- 在现有只 GET `/` 的基础上，对已确认要探测的 URL 额外允许：记录 `Server`、`X-Powered-By`、`X-Generator`（及其他明确写入规则的少量响应头）；若页面声明了 favicon 或可请求同主机 `/favicon.ico`，再发 **一次** GET 计算 favicon hash（sha256 即可）。走现有 HTTP 限速，不跟随跳转到站外，不目录爆破，不扫其他路径。
- 内置少量标题/响应头规则，把命中写成组件来源 `header` 或 `title`（与 wappalyzer 并存）。规则可后续扩展，本任务先做可测试的最小集（例如 Server 值、title 含明确产品名）。
- 新证据只在新的 http 扫描（含后续复扫）写入；不强制对历史 10 万资产立刻补探测。可提供「对未加深识别的存活 URL 补探测」的可选任务，默认关，避免升级后突然打满外网。
- 验收：httptest 下 header/title/favicon 规则命中并入库；不额外请求未允许的路径；限速测试仍过；默认不在迁移时对外补扫。

### T46 未识别走 LLM（可选）
- 环境变量 `LLM_API_URL`、`LLM_API_KEY`、`LLM_MODEL`（OpenAI 兼容 chat completions）。未配置则功能关闭，页面提示未配置，扫描不受影响（与 ICP 相同策略）。
- 仅对「有 HTTP 响应、且未关联任何非常见组件」的 URL 生成候选；输入只用标题、已记录的少量头、favicon hash、URL 主机与路径，截断后发送，不传完整正文、不传 IP。
- 模型输出候选组件名、可选 github_repo、理由；入库为 `source=llm`、`confirmed=false`，**不自动进入正式统计**，待用户在页面确认（或忽略）。确认后才计入库存，并可选用其 repo 走 T44 富集。
- 限速与可关；同一 URL 在证据未变时不重复调用。
- 验收：mock LLM 下候选可见、未确认不计入默认列表、确认后计入；未配置时完全不发请求；测试不打真实模型。
