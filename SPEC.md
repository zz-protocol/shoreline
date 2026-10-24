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
