# 岸线 Shoreline

攻击面的海岸线：SRC / 厂商资产发现与新增监控；被动 + 可选主动，无攻击行为。MIT 许可。

The coastline of your attack surface: discover and watch vendor assets for SRC programs.

## 功能要点

- 厂商 → 根域配置
- 子域名与可访问 URL 发现
- 新增 / 恢复播报
- 范围与客户托管排除
- REST / OpenAPI / MCP
- 挖掘认领与回写
- Web 管理端

## 产品演示

![厂商与根域](docs/screenshots/demo-vendors.png)

*厂商与根域*

![资产列表](docs/screenshots/demo-assets.png)

*资产列表*

![新增播报](docs/screenshots/demo-events.png)

*新增播报*

## 一分钟本地起

```bash
cp .env.example .env   # 填入 API_BOOTSTRAP_TOKEN
docker compose up --build -d
```

Web：`http://127.0.0.1:5173`　·　API：`http://127.0.0.1:8080`

公网 VPS、反代、Certstream、ICP 见 [部署指南](docs/DEPLOY.md)。

> ⚠️ **升级注意（T28，迁移 016_drop_ips）：删除 `assets.ips` 列并抹掉历史播报里的 IP，「不可逆，先备份」。** 升级前按 [docs/DEPLOY.md「9. 备份与升级」](docs/DEPLOY.md#9-备份与升级) 用 `pg_dump -Fc` 备份；回退只能靠旧镜像 + `pg_restore`。

## 文档

- 部署：[docs/DEPLOY.md](docs/DEPLOY.md)
- 配置与接口：[docs/REFERENCE.md](docs/REFERENCE.md)
- Agent：[docs/AGENT_GUIDE.md](docs/AGENT_GUIDE.md)

## 开发

规格见 [`SPEC.md`](SPEC.md)，任务进度见 [`TASKS.md`](TASKS.md)。欢迎阅读 [`CONTRIBUTING.md`](CONTRIBUTING.md) 与 [`SECURITY.md`](SECURITY.md)。
