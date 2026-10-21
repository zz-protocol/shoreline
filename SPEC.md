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
- 首次出现 = 新增；解析 IP、状态码、标题或指纹 hash 变化 = 变更。
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
- 资产变更看 IP 和 CNAME。已复核为真实的主机不被 DNS 重标 wildcard，复核有效期默认 7 天。
- 事件规则：新增 IPv6 算 changed；恢复原值也算 changed；失联（解析失败）后重新解析成功发 recovered 事件；已救回的主机 IP 全部回落到泛解析 IP 时不报（v1 接受）。同一字段同一轮并发只记一条 changed。
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
