# 岸线 Shoreline 部署指南

攻击面的「海岸线」：发现子域名与可访问业务 URL，监控新增与变更。MIT。

推荐单机 Docker Compose。默认只监听本机环回地址，公网访问请在前面加反代。

## 1. 机器建议

| 场景 | 配置 |
|---|---|
| 试跑 / 少量根域 | 2 vCPU / 4 GB / 40 GB SSD |
| 四家 SRC 起步（舒适） | 4 vCPU / 8 GB / 80–100 GB SSD |

常驻组件：Postgres 16、Valkey 8、backend（API + 队列 worker + 定时调度）、frontend（nginx）。可选：Certstream、自建 ICP 服务。

不适合单独丢到 Vercel / Cloudflare Pages：需要常驻 worker 与数据库。

## 2. 安装依赖

- Docker Engine + Docker Compose v2
- 开放出站（DNS、HTTP 探测、crt.sh / subfinder / 测绘 API）

## 3. 获取代码与配置

```bash
git clone https://github.com/zz-protocol/shoreline.git
cd shoreline
cp .env.example .env
```

编辑 `.env`，至少设置：

```bash
API_BOOTSTRAP_TOKEN=请换成足够长的随机串
```

可选：

- `FOFA_KEY` / `QUAKE_KEY` / `HUNTER_KEY`：测绘源
- `CERTSTREAM_URL`：CT 流（见第 6 节）
- `ICP_PROVIDER` + `ICP_API_URL`：ICP 扩展（见第 7 节）
- `SCHEDULE_PASSIVE` / `SCHEDULE_ACTIVE`：默认 `6h` / `24h`，设 `0` 或 `off` 关闭

## 4. 启动

```bash
docker compose up --build -d
```

等待健康后：

| 地址 | 说明 |
|---|---|
| http://127.0.0.1:5173 | Web 管理端（深色） |
| http://127.0.0.1:8080 | 后端 API / MCP（一般经前端反代即可） |
| http://127.0.0.1:5173/docs | OpenAPI 试调页 |

首次打开 Web：输入 `API_BOOTSTRAP_TOKEN`，进入后立刻在「API Key」页创建正式 token，之后可用正式 token，不必长期依赖 bootstrap。

健康检查：

```bash
curl -s http://127.0.0.1:8080/healthz
```

## 5. 公网访问（VPS）

Compose 默认绑定 `127.0.0.1`，外网进不来。任选其一：

**A. 宿主机反代（推荐）**

用 Caddy / nginx 终止 TLS，反代到 `http://127.0.0.1:5173`。前端容器已把 `/api`、`/mcp`、`/healthz` 转到 backend。

Caddy 示例：

```
your.domain.com {
    reverse_proxy 127.0.0.1:5173
}
```

若 MCP 客户端直连后端且与 backend 同机、backend 只听环回：反代到 backend 时 Host 要用上游地址（nginx 默认 `proxy_set_header Host $proxy_host`），不要改成 `$host`。详见 README「MCP」一节。

**B. 改端口映射**

把 `docker-compose.yml` 里 `127.0.0.1:5173:80` 改成 `0.0.0.0:80:80`（或 443 由外层处理）。务必仍用强 token，并考虑防火墙。

大陆机房若用域名给公网 Web，通常需要备案；仅 IP + VPN / SSH 隧道可以不走备案域名。

## 6. Certstream（可选）

自建：

```bash
# .env
CERTSTREAM_URL=ws://certstream:8080/

docker compose --profile certstream up --build -d
```

配置文件：`certstream/config.yaml`（开发阶段仅做过 config 校验，上线请再核对 CT 日志列表与访问控制）。

或指向公共服务（不启 profile）：

```bash
CERTSTREAM_URL=wss://certstream.calidog.io/
```

## 7. ICP 扩展（可选）

对接自建 [icp_yanzhenma](https://github.com/zz-protocol/icp_yanzhenma)（本仓库不包含其代码）：

```bash
ICP_PROVIDER=icp_yanzhenma
ICP_API_URL=http://<你的服务>:8000
# ICP_API_KEY=可选 Bearer
```

未配置时 ICP 自动关闭，可改用 Web 上 CSV 导入子公司 / 手工确认根域。该服务按其说明仅限安全研究，请控制频率（本项目默认 expand 约 1 次/秒，超时 30s）。

## 8. 日常使用顺序

1. Web 登录 → 创建正式 API Key  
2. 添加厂商 → 添加根域与范围规则  
3. 触发被动扫描（或等定时）→ 看资产与播报流  
4. 需要时按厂商打开主动发现  
5. 其他 agent：REST（`Authorization: Bearer <token>`）或 MCP `POST /mcp`

OpenAPI：`GET /api/openapi.yaml`；页面试调：`/docs`。

## 9. 备份与升级

### 升级

> ⚠️ 部分迁移不可逆（如 016_drop_ips：删除 IP 列并抹掉历史播报里的 IP）。**不可逆，先备份**，回退只能靠备份恢复。

① 升级前备份（自定义格式 `-Fc`，可用 `pg_restore` 恢复），并记下当前版本：

```bash
git rev-parse HEAD > shoreline-prev-version.txt
docker compose exec -T postgres pg_dump -Fc -U sam -d sam > shoreline-$(date +%F).dump
ls -l shoreline-*.dump   # 确认文件非空
```

② 升级：拉代码、重建并启动。backend 启动时自动按编号执行未执行过的迁移（记录在 `schema_migrations`），无需手动跑 SQL：

```bash
git pull
docker compose up --build -d
docker compose logs --tail=50 backend   # 迁移失败会以 migrate: ... 退出
```

③ 出问题时回退：先回到旧版本镜像，再用备份恢复库（新版迁移已改过表结构，旧版本不能直接用升级后的库）：

```bash
docker compose stop backend
git checkout "$(cat shoreline-prev-version.txt)"
docker compose build backend
docker compose exec -T postgres dropdb -U sam sam
docker compose exec -T postgres createdb -U sam -O sam sam
docker compose exec -T postgres pg_restore -U sam -d sam --no-owner --exit-on-error < shoreline-YYYY-MM-DD.dump
docker compose up -d
```

恢复后旧版本按备份里的 `schema_migrations` 启动，不会再执行新迁移。

数据卷名：`pgdata`。改调度等环境变量后需重启 backend（`docker compose up -d backend`）。

## 10. 上线前建议自检

- [ ] 已换成强 `API_BOOTSTRAP_TOKEN`，并创建了正式 token  
- [ ] Postgres 默认口令 `sam/sam` 仅用于单机；若库对公网暴露请改密并改 `DATABASE_URL`  
- [ ] FOFA / Quake / Hunter：有 key 时各跑一轮核对（开发期仅 mock）  
- [ ] Certstream：接真实流或自建服务实测  
- [ ] ICP：与真实 icp_yanzhenma 联调（企业名查询与 `domain` 字段曾为推断）  
- [ ] 主动发现默认关；对目标授权范围内使用，注意限速  

更细的配置表、泛解析局限、压测报告见仓库根目录 `README.md` 与 `docs/bench.md`。
