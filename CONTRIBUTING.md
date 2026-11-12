# 参与贡献

感谢你对岸线 Shoreline 的兴趣。

## 本地启动

```bash
cp .env.example .env   # 填入 API_BOOTSTRAP_TOKEN
docker compose up --build -d
```

Web：`http://127.0.0.1:5173`　·　API：`http://127.0.0.1:8080`。更完整的部署说明见 [docs/DEPLOY.md](docs/DEPLOY.md)。

## 测试

后端（外部依赖全部本地 mock）：

```bash
cd backend && TEST_DATABASE_URL=postgres://sam:sam@127.0.0.1:5432/sam?sslmode=disable go test ./...
```

真实时间的重试间隔测试 `TestRetryIntervals` 不在全量里，需加 `-tags integration`。配置与接口细节见 [docs/REFERENCE.md](docs/REFERENCE.md)。

前端：

```bash
cd frontend && npm install && npm test && npm run build
```

本地开发可用 `npm run dev`（反代到 `127.0.0.1:8080`）。测试为 vitest。

## Pull request 约定

- 一个 PR 聚焦一件事，说明动机与如何验证。
- 不要在仓库对外文档中写入生产或演示用的公网地址、IP。
- 不要对真实厂商或真实域名发起扫描（见下）。
- 行为变更请同步相关文档（配置与接口见 `docs/REFERENCE.md`）。规格以根目录 `SPEC.md` / `TASKS.md` 为准，有疑问先讨论，不要擅自改需求。

## 禁止对真实厂商扫描

贡献与本地调试只用合成或测试域名（例如 `*.example.test`），不要对真实厂商、真实域名做枚举、解析或 HTTP 探测。

## 替换产品演示截图

README「产品演示」使用固定路径与顺序。请用同名文件覆盖，不必改 Markdown：

1. `docs/screenshots/demo-vendors.png`：厂商与根域
2. `docs/screenshots/demo-assets.png`：资产列表
3. `docs/screenshots/demo-events.png`：新增播报

已有 `docs/screenshots/t14-*.png` 请保留，不要用它们充当这三张演示图。
