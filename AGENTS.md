# AGENTS.md

给进入本仓库的 agent 的入口约束。动手前先读：

1. `docs/TEAM.md`（角色、流水线、状态机、验收）
2. `SPEC.md` 当前轮（第九轮 T49–T54）
3. `TASKS.md` 对应任务行与状态
4. `docs/REFERENCE.md`（环境变量、门控顺序、接口事实）

## 角色

- **架构**：写 SPEC/TASKS，派工，最终向用户汇报。
- **工程**：实现 + 测试 + 文档同步，改「待验收」。
- **交付**：独立重跑验收，改「完成」或「打回」。

同一会话兼做时也要分三步，实现者不得自己宣布验收通过。

## 命令

```powershell
# 全量检查（Go 用 golang:1.26-alpine 容器 + sam_test 测试库；前端 vitest + build）
powershell -ExecutionPolicy Bypass -File scripts/check.ps1

# 只跑后端相关测试
powershell -ExecutionPolicy Bypass -File scripts/check.ps1 -Scope Go -Run 'T51|T52'

# 按任务验收
powershell -ExecutionPolicy Bypass -File scripts/accept.ps1 -Task T51
```

Windows checkout 是 CRLF，`check.ps1` 默认跳过 `gofmt`；CI 或 Linux 环境加 `-Fmt`。

## 状态机

`待做 → 进行中 → 待验收 → 完成 / 打回`。只有交付能把「待验收」改为「完成」或「打回」。工程不写「完成」。

## 红线

- 不部署、不对真实厂商扫描、不写生产地址或 IP 实值。
- IP 只留在扫描内存，不落库、不进 API/UI/MCP/日志；banner 中脱敏为 `[ip]`。
- 非 Web 探测默认关；授权范围为空等于未授权；未判定 CDN 的资产跳过。
- 探测只发 YAML 声明的无认证只读报文，不登录、不读写业务数据。
- 不改 SPEC 口径；需求变化先由架构改 SPEC。
- 不删除已识别组件数据；迁移必须可重跑。

## 当前代码状态

- 本机 HEAD：`778de62`，工作区干净。
- 前一份交接提到的 `c3130ff`、`5844c22` 与 T53 未提交实现（`backend/fingerprints/services.yaml`、`backend/service_fingerprint_yaml.go`、`backend/t53_fingerprint_yaml_test.go`）不在本机。
- T53 处于「待做」，开工前先确认代码来源；不要假定这些文件存在。
- 本机没有 Go 工具链，后端测试走 `scripts/check.ps1` 的容器路径。

## 提交与文档

- 提交信息带任务号，例如 `T53: 可插拔 YAML 指纹库`。
- 实现提交与「TASKS 改为待验收」的文档提交分开。
- 行为或配置变更同步 `docs/REFERENCE.md`；新口径先写进 `SPEC.md`。
