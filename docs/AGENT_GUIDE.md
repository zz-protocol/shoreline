# Shoreline 挖掘 Agent 使用说明

面向自动化挖洞 agent：从 Shoreline 领取存活资产 → 挖掘期间续约 → 挖完带状态回写。
本文所有命令都可在 bash 中逐字执行（需要 `curl` 和 `jq`）。

同一份内容的获取方式：
- Web：`/docs` 页面上方「Agent 使用说明」；
- REST：`GET /api/agent-guide`（无需认证，`text/markdown`）；
- MCP：工具 `get_agent_guide`（无参数，返回 `{"guide": "<本文 markdown>"}`）。

## 1. 认证

除 `/api/openapi.yaml`、`/api/agent-guide`、`/healthz` 外，所有 `/api/*` 与 `/mcp` 都要带请求头：

```
Authorization: Bearer <token>
```

token 由管理员在 Web「API Key」页生成，或用部署时配置的 `API_BOOTSTRAP_TOKEN` 调 `POST /api/tokens` 生成（明文只返回这一次）：

```bash
export BASE=http://127.0.0.1:8080
curl -s -X POST "$BASE/api/tokens" \
  -H "Authorization: Bearer $API_BOOTSTRAP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"agent-1"}'
```

把返回的 `token` 字段设为环境变量（下文都用 `$BASE` 和 `$TOKEN`）：

```bash
export TOKEN=<上一步返回的 token>
```

## 2. 工作循环：claim → heartbeat → complete

### 2.1 claim：领取资产（REST）

```bash
curl -s -X POST "$BASE/api/mine/claim" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"claimed_by":"agent-1","limit":1}' | tee /tmp/claim.json
export ASSET_ID=$(jq -r '.items[0].id' /tmp/claim.json)
echo "$ASSET_ID"
```

- 请求体：`claimed_by`（必填，agent 名）、`limit`（默认 20，最大 100）、可选 `vendor_id`、`since`（RFC3339，按 first_seen 过滤）、`q`（子域名包含）、`include_abandoned`（默认 false）。
- 返回 `{"items":[资产…], "claimed_by":"agent-1", "count":N}`。每个资产带 `mine_status`（=claimed）、`mine_claimed_at`、`mine_claim_expires_at`。
- 只会领到：存活（alive）、非客户托管，且 `mine_status` 为 unset / 冷却已过的 done、deferred / 认领已过期的 claimed（abandoned 仅在 `include_abandoned=true` 时）。
- 原子领取（`SKIP LOCKED`）：多个 agent 同时 claim 不会领到同一资产。
- `count` 为 0（`items` 为空）表示当前没有可领资产，稍后再试，不是错误。

### 2.2 heartbeat：续约（REST）

挖掘超过认领 TTL（默认 24 小时）前必须续约，否则资产会被别的 agent 领走。建议每 1 小时一次：

```bash
curl -s -X POST "$BASE/api/mine/heartbeat" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"asset_ids\":[$ASSET_ID]}"
```

返回 `{"renewed":N}`：N 为成功续约的条数。只续仍处于 claimed 的资产；返回 0 说明认领已结束（已被 complete），应停止该资产的工作。

### 2.3 complete：回写结果（REST）

```bash
curl -s -X POST "$BASE/api/mine/complete" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"asset_ids\":[$ASSET_ID],\"status\":\"done\",\"note\":\"checked login and api, no finding\",\"report_url\":\"\"}"
```

- `status` 必填：`done` | `deferred` | `abandoned`。
- `note`、`report_url` 可选，各最多 2000 字符。
- 返回 `{"updated":N,"status":"done"}`。资产写入 `mine_completed_at`（挖掘时间），清除认领人与锁。

### 2.4 确认结果

```bash
curl -s "$BASE/api/assets/$ASSET_ID" -H "Authorization: Bearer $TOKEN" \
  | jq '{id, subdomain, mine_status, mine_completed_at, mine_cooldown_until}'
```

Web「资产」页可按「挖掘状态」筛选，「最近挖掘」列显示挖掘时间。

## 3. MCP 示例

MCP 地址 `POST $BASE/mcp`（Streamable HTTP，无状态、JSON 响应），认证同上。工具与 REST 一一对应：

| 工具 | 参数 | 对应 REST |
|---|---|---|
| `claim_mine_targets` | claimed_by, limit, vendor_id, since, q, include_abandoned | POST /api/mine/claim |
| `heartbeat_mine` | asset_ids | POST /api/mine/heartbeat |
| `complete_mine` | asset_ids, status, note, report_url | POST /api/mine/complete |
| `get_agent_guide` | 无 | GET /api/agent-guide |

用 curl 直接调 MCP（同样领 1 个 → 续约 → 回写；`ASSET_ID` 取自 MCP claim 的结果）：

```bash
curl -s "$BASE/mcp" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"claim_mine_targets","arguments":{"claimed_by":"agent-1","limit":1}}}' \
  | tee /tmp/mcp_claim.json
export ASSET_ID=$(jq -r '.result.structuredContent.items[0].id' /tmp/mcp_claim.json)
echo "$ASSET_ID"

curl -s "$BASE/mcp" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"heartbeat_mine\",\"arguments\":{\"asset_ids\":[$ASSET_ID]}}}"

curl -s "$BASE/mcp" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"complete_mine\",\"arguments\":{\"asset_ids\":[$ASSET_ID],\"status\":\"done\",\"note\":\"via mcp\"}}}"
```

结果在 `result.structuredContent`。业务错误时 `result.isError` 为 true，文本内容为 `{"error":"..."}`。
MCP 客户端（如 Claude Desktop、Cursor）配置：URL 填 `$BASE/mcp`，Header 填 `Authorization: Bearer <token>`。

## 4. 状态含义（mine_status）

| 状态 | 含义 | 何时可再被 claim |
|---|---|---|
| unset | 从未被挖掘 | 随时 |
| claimed | 已被某 agent 领取，正在挖；`mine_claimed_by` 为领取人 | 锁过期（`mine_claim_expires_at` 已过）后 |
| done | 挖完了（有无漏洞都算） | 冷却期（`mine_cooldown_until`）过后 |
| deferred | 暂缓，以后再挖（如需要账号、环境暂不可用） | 冷却期过后 |
| abandoned | 放弃（不值得挖、超出能力） | 默认不再派发；claim 时传 `include_abandoned=true` 才会领到 |

## 5. 默认值（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `MINE_CLAIM_TTL_HOURS` | 24 | 认领锁时长（小时，≥1）；claim 与每次 heartbeat 都把过期时间设为 now()+TTL |
| `MINE_COOLDOWN_DAYS` | 30 | done/deferred 后的冷却天数（≥0）；0 表示不自动回到可领队列 |
| claim `limit` | 20（最大 100） | 每次领取上限 |

## 6. 错误码

所有错误都是 JSON：`{"error":"<原因>"}`。

| HTTP | 场景 | agent 应该 |
|---|---|---|
| 400 | 参数错误：缺 `claimed_by`、`limit` 不在 1..100、`asset_ids` 为空或超过 100、`status` 不是 done/deferred/abandoned、`note`/`report_url` 超 2000 字符、含 NUL、JSON 非法、`since` 非 RFC3339 | 修正参数，不要原样重试 |
| 401 | 缺少 token（`missing bearer token`）或 token 无效（`invalid token`） | 停止并请人更换 token |
| 404 | 路径不存在，或 `vendor_id` 等引用的对象不存在 | 检查 URL / 参数 |
| 409 | 冲突（例如其它接口里的重复创建、进行中任务不可删）。mine 三个接口目前不返回 409：并发 claim 冲突由 SKIP LOCKED 处理，抢不到时只是 `count` 变小 | 按 `error` 处理，不盲目重试 |
| 500 | 服务端错误 | 指数退避后重试 |

注意：heartbeat 对已不是 claimed 的资产返回 `renewed` 少于提交数，而不是报错；complete 不校验领取人，请只回写自己领到的资产。

## 7. 可直接粘贴的 system prompt

```
你是一个漏洞挖掘 agent，资产来源是 Shoreline。
- 服务地址 $BASE，所有请求带 Authorization: Bearer $TOKEN。
- 开始工作：调用 claim_mine_targets（或 POST /api/mine/claim），claimed_by 用你的名字，limit 不超过你能处理的数量；items 为空就结束本轮。
- 只测试领到的资产（items 里的 subdomain/url），不要扩展到其他主机。
- 挖掘期间每 1 小时对仍在处理的资产调用 heartbeat_mine（POST /api/mine/heartbeat）；renewed 小于提交数时，停止处理未续上的资产。
- 每个领到的资产挖完都必须调用 complete_mine（POST /api/mine/complete）并带 status：
  - done：已完成（有无漏洞都算），note 写结论，有报告时填 report_url；
  - deferred：暂时无法继续（缺账号/环境），note 写原因；
  - abandoned：不值得挖或超出范围，note 写原因。
- 绝不能领了不回写：即使出错或中途退出，也要对未完成的资产调用 complete_mine，status=deferred。
- 遇到 400 修正参数；401 立即停止并报告；500 退避后重试。
```
