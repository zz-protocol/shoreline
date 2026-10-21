# TASKS（规格见 SPEC.md）
状态取值：待做/进行中/待验收/完成/打回

| 编号 | 任务 | 验收标准 | 状态 |
|---|---|---|---|
| T1 | 项目骨架：Go 后端、React+Vite 前端、docker compose（Postgres、Redis）、MIT LICENSE | compose up 后后端健康检查返回 200，前端首页可打开 | 完成（交付 10-21 验收通过） |
| T2 | 数据模型与迁移：厂商、根域、范围规则、资产、URL、事件、扫描任务 | 迁移可重复执行；含 first_seen/last_seen/来源字段与唯一键 | 完成（交付 10-21 验收通过） |
| T3 | 范围判定模块（通配、排除） | 单测覆盖通配、多级、排除、大小写，全部通过 | 完成（交付 10-21 验收通过） |
| T4 | 任务队列与 worker 框架（Asynq，被动/DNS/HTTP 三类，独立限速配置） | 可手动触发扫描；限速配置生效有测试 | 完成（交付 10-21 复验 1bda24e 通过） |
| T5 | 被动源：crt.sh + subfinder 免 key 源 + FOFA/Quake/Hunter（有 key 启用） | 对测试根域产出子域并入库，记录来源；无 key 时自动跳过 | 完成（交付 10-21 验收 a75e04e 通过） |
| T6 | Certstream 常驻订阅，按根域匹配入库 | 用合成样本回放测试（字段按公开消息格式写全，另生成 ≥1 万条做体量测试），匹配正确；配置 CERTSTREAM_URL 后常驻运行、断线自动重连 | 完成（交付 10-21 复验 cecf794 通过） |
| T7 | 解析 + 泛解析过滤 + CDN 标记 | 泛解析测试域不产生假资产（有测试） | 完成（交付 10-21 复验 dc26b99 通过；004 原地改动风险待架构定） |
| T8 | HTTP 探测（只 GET /，限速，记状态码/标题/指纹/跳转） | 本地测试服务器验证字段与速率 | 完成（封版 8e76afd：所有反例假资产 0；短200模板按已知局限） |
| T9 | 新增/变更判定与事件生成 | 两轮模拟扫描：新增只播一次，变更正确识别 | 完成（交付 10-21 复验 dd6ac12 通过；泛解析相关依赖 T8） |
| T10 | 主动发现：字典爆破、变形、递归一层（按厂商开关，默认关或限速） | 本地 mock DNS 下可跑通；开关生效 | 完成（交付 10-21 验 35c98e7 + 复验 337e2d3 sources 细分通过） |
| T11 | ICP/子公司扩展：发现的新根域进"待确认"列表 | 不自动加入扫描；确认后才加入 | 完成（复验 498ce29） |
| T12 | REST API + OpenAPI + token 认证 | 接口与 SPEC 接口草图一致；文档可访问 | 完成（复验 6edc406） |
| T13 | MCP server：list_new_assets、search_assets、check_scope、trigger_scan | MCP 客户端能调通四个工具 | 完成（c2549b4：校验一致；nginx /mcp 实测通，响应为 JSON 非流式） |
| T14 | Web 端：厂商/根域/范围管理、资产检索、新增播报流、扫描状态、API key 配置；深色主题 | 页面全部可用，交互走真实 API | 完成（cffaee7+since 5e21c00+切 level 清 since 6394e50：配套 API；npm 10 PASS） |
| T15 | 定时调度（被动 6h、主动 1d，可配） | 配置修改生效有测试 | 完成（交付复验 e515426：默认 6h/24h、off、不 expand、pending 跳过、非法 duration 退出） |
| T16 | 压测：10 万条模拟子域 | 列表接口 < 1s，报告写入 docs/bench.md | 完成（交付复验 27a432d：TEST_BENCH 100k 五场景均 <1s；无 flag SKIP；bench.md 有报告） |
| T17 | ICP 适配器预设 icp_yanzhenma（见 SPEC「ICP 数据源」）：读该仓库 API 定义写客户端，配置 ICP_PROVIDER/ICP_API_URL | mock 服务下按根域/企业名查询能解析入"待确认"；服务不可达时降级不报错；未配置时自动关闭 | 完成（交付复验 24a1b49：yanzhenma 解析/IP丢弃/punycode；不可达与 status=failed→done+degraded；未配置关闭；非法 provider 退出；compose 三变量；MIT 未复制） |

约束：只开发，不对真实厂商跑扫描、不部署。不复制 GPL/AGPL 代码。

## 待办（用户 10-21 反馈，先记下，后面再排）
| T18 | 修 /docs Swagger：openapi.yaml 第 55 行附近 YAML 解析错误（flow 集合缺逗号），导致读不到 openapi 版本字段 | 真实后端下 /docs 能渲染全部 28 个接口；加 YAML 解析单测 | 完成（复验 fe19f5f：真实栈 /docs 渲染 28 接口无错，Try it out 200/401；js-yaml/PyYAML/yaml.v3 旧版报错新版通过） |
| T19 | UI 换风格：参考 grok.com 官网（极简、深色、大留白）；含 /docs Swagger 深色适配（对比度、Authorize 白条） | 用户看过设计稿确认后再做 | 待排 |
| T20 | 新增播报（可回溯记录，默认近7天，事件不自动清）：按厂商筛选；按时间窗（今日/近7天/自定义）筛选并显示各厂商新增数统计；子域列、URL 列各加「一键复制本列」，复制当前筛选结果的全部数据（非仅当前页），一行一个、去重 | 筛选与统计数与 API 一致；复制内容与筛选结果一致 | 完成（复验 a5fbf25：真实栈+Chromium；list/stats/values 六组口径一致；轮询顶部追加；复制全窗；全量 6 连跑无偶发） |
| T21 | 资产探活：列表显示存活/失活（DNS 解析 + HTTP 可达）与最后存活时间，可按状态筛选。规则（用户 10-21 定）：一旦确认存活就不再复查；失活的资产定时复查，恢复为存活时生成播报事件。 | 存活后不再复查；失活转存活产生一条恢复播报且不重复 | 完成（d2662aa 复验：空库 -count=3、残留库全量 -count=2 均 ok） |
| T22 | 文案与引导：根域/范围/待确认根域、扫描类型 passive/active/expand 加中文说明和提示 | 用户不看文档能看懂 | 完成（复验 55efa20：真实后端+生产构建+Chromium，说明/空状态/范围表单 37 项全过；范围文案与 check-scope 一致） |
| T23 | 根域自动发现入口更显眼：一键对厂商跑 expand（ICP/子公司），结果进待确认 | 页面可直接触发并看到结果 | 待验收 |
| T24 | 云上客户资产排除：内置可编辑的「客户托管」规则表（如 *.aliyuncs.com 的 OSS/FC、*.myqcloud.com 的 COS、*.volces.com（volccdn 有意不内置，用户可自加）、各家 CDN/函数/网站托管默认域），命中的资产标记 customer_hosted（不删），列表/播报默认隐藏、可切换查看；Web 可增删规则；改规则后异步重算 | 内置样例命中正确；默认隐藏、可查看；改规则后重算生效 | 待验收 |
| T25 | T20/T21 尾巴（含 stats 按 type 分开计数；事件类型标签中文化；修复恢复资产按资产维度复制 URL 为空）：①复制在非安全上下文（HTTP 内网地址）回退到 textarea+execCommand，失败时弹出文本框供手动复制；②总数正好是页大小整数倍时不返回 next_cursor；③轮询单次新增超 500 时显示「有更多新事件，点此刷新」；④清理守卫：递归扫描 backend 全部非测试 .go/.sql，正则识别任意空白/换行、schema 前缀、引号表名、TRUNCATE，白名单精确到 008 文件+语句；⑤事件类型标签中文：new「新增」、recovered「恢复」、changed「变更」；⑥恢复的资产按资产维度复制 URL 列返回该资产当前 URL；⑦events/stats/values 不带 type 默认 new,recovered（count=所选各 type 之和）；手动复制框截断时标题注明 | 七项各有测试或实测 | 完成（9836363+95428b2 复验：守卫全变体 FAIL、默认 type 排除 changed、恢复 URL 复制、截断标题、回归、go test -count=2 均过；changed 蓝标签仅源码+vitest 佐证） |
