# SRC 资产发现工具调研（只做子域名 + 可访问业务 URL，不带攻击行为）

> 调研日期：2026-10-21。内容根据各项目公开的 README/文档整理，版本细节以上游仓库为准。

## 1. 主流工具对比

| 工具 | 语言/许可 | 被动来源 | 主动方式 | 验证 | 性能要点 | 存储/监控/API |
|---|---|---|---|---|---|---|
| subfinder (PD) | Go / MIT | 约 50 个源：crt.sh、certspotter、VirusTotal、SecurityTrails、Chaos、Shodan、FOFA、Quake、Hunter(新版)、Wayback、AlienVault、BufferOver、DNSDumpster 等；用 provider-config.yaml 配 key | 无（只做被动） | 可选 -nW 解析 | 各源并发、限速，可当库用 | 无持久化；Go SDK 可嵌入 |
| amass (OWASP) | Go / Apache-2.0 | 几十个数据源，包括 CT、被动 DNS、ASN/whois 反查、Web 存档 | 字典爆破、alterations 变形、zone transfer、递归 | 内置解析和泛解析检测 | 慢、吃资源；v4 改成 OAM 资产图 | 图数据库（SQLite/Postgres），有 track/diff 跟踪新增 |
| OneForAll | Python / GPL-3.0 | 证书、DNS 数据集、搜索引擎（百度/必应/谷歌/FOFA/Quake/Hunter/Shodan）、威胁情报、crossdomain.xml、CSP 头、robots/sitemap、JS 爬取 | 爆破（调 massdns）、AXFR、NSEC 遍历、SPF/MX/TXT 记录、子域接管检测 | 解析加 HTTP 请求，泛解析按 IP/TTL/页面相似度过滤 | 按模块并发；大规模时偏慢 | 结果存 SQLite/CSV；无 API |
| ksubdomain | Go / MIT | — | 无状态 DNS 爆破（自己构造原始包、靠网卡发包，同类里最快之一）；verify 模式批量验证 | 泛解析过滤 | 可达每秒几十万包；需要 pcap/root 权限 | 无 |
| shuffledns / puredns | Go | — | 基于 massdns 的爆破和批量解析 | puredns 用可信 resolver 复核，泛解析检测做得好 | 公共 resolver 加可信 resolver 两阶段 | 无 |
| dnsx (PD) | Go / MIT | — | 批量解析 A/AAAA/CNAME/MX/TXT/PTR/AXFR，也能爆破 | 泛解析过滤（-wd） | 高并发、可设限速 | JSON 输出 |
| httpx (PD) | Go / MIT | — | HTTP 存活探测：状态码、标题、技术指纹、favicon mmh3、TLS SAN、CSP 域名、jarm、CDN 识别 | 存活即结果 | 高并发，-rl 限速 | JSONL；可作库调用 |
| chaos (PD) | Go 客户端 | ProjectDiscovery 预收集的漏洞赏金厂商子域库（需 key） | — | — | — | API；Chaos 公开数据集含新增 diff |
| assetfinder | Go / MIT | crt.sh、certspotter、hackertarget、threatcrowd、wayback、FB CT 等少量源 | — | — | 轻量 | — |
| findomain | Rust / GPL-3.0 | 以 CT 等 API 为主 | 可选爆破 | 解析加 HTTP | 快 | 付费版有监控、DB 和 Webhook 通知（新增子域推送） |
| ENScan_GO | Go / Apache-2.0 | 爱企查、天眼查、企查查、小蓝本等：企业信息、ICP 备案、对外投资/控股子公司（按持股比例递归）、APP、小程序、公众号 | — | — | 递归深度、持股比例可调 | 有 API/MCP 模式，可供 AI agent 调用 |
| bbot | Python / AGPL-3.0（以上游为准） | 一百多个模块：CT、被动 DNS、API、JS/网页爬取、证书 SAN、GitHub、云存储 | 爆破、变形（dnsbrute_mutations）、递归事件驱动 | 自带泛解析检测，作用域（scope）控制严格 | 事件图驱动，新发现的域名会自动回灌进下一轮 | Neo4j/JSON/Postgres 输出 |
| reNgine | Python Django / GPL-3.0 | 编排 subfinder/amass 等工具 | 编排 | 编排 httpx | Celery 分布式任务 | Web UI、定时扫描、新增 diff、通知（Slack/Discord/TG）、REST API。是本项目最接近的参考 |
| notify (PD) | Go / MIT | — | — | — | — | 把输出推送到 Slack/Discord/Telegram/邮件/Webhook |
| Certstream | Python/Go | 实时 CT 日志流（WebSocket） | — | — | 实时，几乎零成本，最早拿到新证书 | 适合做实时新增 |
| 其他 | | crt.sh 支持 PG 直连（psql crt.sh）；Censys；Netlas；VirusTotal；SecurityTrails；360 Quake；鹰图 Hunter；零零信安 | alterx（PD 变形）、gotator、dnsgen；katana/gau/waybackurls（爬取 URL）| | | |

## 2. 推荐发现方法（按优先级排）

| # | 方法 | 被动/主动 | 要 key | 说明 |
|---|---|---|---|---|
| 1 | CT 实时流（Certstream / 自建 CT log 拉取） | 被动 | 否 | 最快发现新上线系统，契合“谁先发现谁先有洞” |
| 2 | crt.sh / certspotter 历史证书 | 被动 | 否 | 种子全量 |
| 3 | 测绘 API：FOFA / Quake / Hunter / Shodan / Censys（domain=、cert=、icon_hash=、title=） | 被动 | 是 | 国内覆盖好 |
| 4 | ICP 备案加子公司扩展（ENScan 思路：控股比例 ≥50% 递归 → 备案主域 → 回灌种子） | 被动 | 部分要 | 发现新根域，扩大范围的关键 |
| 5 | 被动 DNS / 威胁情报（VirusTotal、SecurityTrails、AlienVault OTX、微步） | 被动 | 多数要 | |
| 6 | Web 存档 / CommonCrawl（wayback、gau） | 被动 | 否 | 子域名和 URL 路径都能拿到 |
| 7 | 响应内容提取：TLS 证书 SAN、CSP 头、CORS、302 跳转、JS/HTML 里的域名、crossdomain.xml、robots/sitemap | 半被动（只请求已知站点） | 否 | 新域名回灌，递归发现 |
| 8 | favicon mmh3 hash、标题、证书组织字段（O=Alibaba）反查测绘平台 | 被动 | 是 | 找无名资产、新根域 |
| 9 | DNS 字典爆破（ksubdomain/puredns，限速，可信 resolver 复核） | 主动（只发 DNS 查询） | 否 | |
| 10 | 变形/排列（alterx：dev-、-test、数字递增、环境词） | 主动 DNS | 否 | 对新增子域特别有效 |
| 11 | DNS 记录挖掘：AXFR、NSEC/NSEC3 遍历、SPF/TXT/MX、CNAME 链 | 主动 DNS | 否 | |
| 12 | 递归：新发现的子域当作下一级根继续跑 | — | — | 设深度上限 |
| 13 | 其他来源：GitHub 代码搜索、APP/小程序抓域名、反向 IP（同 IP 段 PTR）、ASN 网段 | 被动 | GitHub 要 token | |

验证流水线：解析（dnsx，多 resolver）→ 泛解析过滤（随机子域探测，比对 IP 集合和页面相似度 simhash）→ CDN/云识别 → httpx 存活（限速，只发 GET /）→ 记录标题、指纹、favicon → 归属判断（是否在 SRC 范围表里、ICP 主体是否匹配）。

## 3. 大规模场景技术栈草图

- 语言：Go 后端（能直接把 subfinder、dnsx、httpx、alterx 当库引用，均为 MIT 许可），Web 前端用 React/Vue 加 shadcn 或 Ant Design。
- 架构：调度器（cron 加事件触发）→ 任务队列（NATS/Redis Streams/Asynq）→ 无状态 worker 可横向扩展（被动 worker、DNS worker、HTTP worker 分开，各自限速）。
- 存储：PostgreSQL 存资产主表（domain、first_seen、last_seen、source[]、status）；ClickHouse 存海量解析/探测历史（可选）；Redis 做去重布隆过滤器和限速。
- Diff 逻辑：用唯一键 upsert，`first_seen = now` 即为新增；对 HTTP 指纹做 hash，变化时触发“变更”事件；CT 流实时入队。
- 播报：内置 notify 风格多渠道（TG/飞书/钉钉/企微/Slack/Webhook），支持按厂商订阅、去重和聚合。
- API：REST 加 OpenAPI，外加一个 MCP server 给 AI agent（查询资产、增量、触发扫描、管理范围）；API Key 鉴权。
- 范围管理：每家 SRC 的 in-scope/out-of-scope 通配规则，所有主动动作都受范围约束（参考 bbot 的 scope 设计）。
- 许可：建议 MIT/Apache-2.0。注意：如果直接复用 OneForAll（GPL）或 bbot/reNgine 的代码，会受 GPL/AGPL 传染，只借鉴思路、不复制代码。
