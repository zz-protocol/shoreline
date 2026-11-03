# Bug 报告：非 UTF-8 网页标题导致 HTTP 扫描任务整体失败

> 提交对象：Shoreline（岸线）后端开发
> 版本：commit `20eedb8`（master）
> 环境：Windows + Docker Desktop，PostgreSQL 16，Valkey 8，backend 镜像 `shoreline-backend`
> 严重级别：**高**（单条脏数据 → 整个根域扫描任务 100% 失败，且重试无意义）
> 发现时间：2026-11-03

---

## 一、症状

数据库里大量 `http` 类型扫描任务的 `status = failed`，`error` 全是同一个：

```
ERROR: invalid byte sequence for encoding "UTF8": 0xd3 0xc3 (SQLSTATE 22021)
ERROR: invalid byte sequence for encoding "UTF8": 0xbd (SQLSTATE 22021)
ERROR: invalid byte sequence for encoding "UTF8": 0xcc 0xda (SQLSTATE 22021)
```

backend 日志对应：

```
asynq: pid=1 ... WARN: Retry exhausted for task id=93f0dd4e-28dc-4375-acea-b63c36372ecc
```

任务重试耗尽后进入终态 `failed`，该根域当轮扫描结果 **全部丢失**。

### 影响面统计（本机库）

```
failed 任务总数        96
其中 UTF-8 报错        35   (36%)
按类型：http 44 / active 47 / dns 5
```

受影响根域（`fails` = 因此失败的扫描次数）：

| 根域 | 失败次数 |
|---|---|
| fenbi.com | 9 |
| xdf.cn | 7 |
| mi.com | 6 |
| 360.cn | 2 |
| duxiaoman.com | 2 |
| oppoit.com | 2 |
| qq.com | 2 |
| weizhipin.com | 2 |
| mendale.com | 1 |
| huazhu.com | 1 |
| 95516.com | 1 |

> 受影响资产池 32,517 个；因任务中断而始终没拿到 URL 的存活资产 47 个（每轮还会继续累积）。

---

## 二、根因

**位置：`backend/httpprobe.go:157-159`**

```go
if m := titleRe.FindSubmatch(body); m != nil {
    r.Title = strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
}
```

`body` 是**原始响应字节**（`io.ReadAll` 得到），这里直接把 `<title>` 的原始字节 **当成 UTF-8 字符串** 使用，**没有做 charset 解码**。

中文站大量使用 GBK / GB18030 / GB2312 编码（`Content-Type: text/html; charset=gb18030`），其标题字节（如 `D3 C3 BB A7 B5 C7 C2 BC` = "用户登录"）不是合法 UTF-8。这个非法字符串一路带到写库：

**`backend/httpprobe.go:624-626`（INSERT 新 URL）**

```go
err = tx.QueryRow(ctx, `INSERT INTO urls (asset_id, scheme, host, port, status_code, title, fingerprint_hash, redirect_to, tech, sources)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '{http}') ...
    assetID, r.Scheme, r.Host, r.Port, r.Status, r.Title, ...)
```

**`backend/httpprobe.go:637-639`（UPDATE 已有 URL）**

```go
tx.Exec(ctx, `UPDATE urls SET asset_id = $2, status_code = $3, title = $4, ...`)
```

`urls.title` 是 `text` 类型 → PostgreSQL 拒绝非法 UTF-8 → 报 `SQLSTATE 22021`。

---

## 三、为什么整个任务全废（而不是跳过这一条）

**`backend/httpprobe.go:592-593`**

```go
wg.Wait()
return nil, firstErr
```

`httpHandler` 里任意一个 host 的 `storeURL` 失败都会 `fail(err)` 记录 `firstErr`，最后**整个任务返回 error**。

`queue.go` 对 error 的处理：未超过 `MaxRetry` → 标记 `retrying` 并重投；超过 → `failed` 终态。

因为失败是**确定性的**（同一个 GBK 页面每次都会返回同样的字节），重试必然再次失败 → 3 次后 `Retry exhausted`。**等价于：根域里只要有一个 GBK 站点，这个根域的 HTTP 扫描就永远跑不完。**

---

## 四、确定性复现

```bash
curl -s -D- -o /tmp/p.html http://gdinfo.95516.com/
# Content-Type: text/html; charset=gb18030      ← 关键

python3 - <<'PY'
import re
body = open('/tmp/p.html','rb').read()
raw = re.search(rb'(?is)<title[^>]*>(.*?)</title>', body).group(1)
print('raw bytes :', raw)                 # b'\xd3\xc3\xbb\xa7\xb5\xc7\xc2\xbc'
print('gb18030   :', raw.decode('gb18030'))  # 用户登录
try:
    raw.decode('utf-8')
except UnicodeDecodeError as e:
    print('utf-8 FAIL:', e)               # invalid start byte 0xd3
PY
```

实测结果（本机验证）：

```
HTTP 200
Content-Type: text/html; charset=gb18030
raw title bytes: b'\xd3\xc3\xbb\xa7\xb5\xc7\xc2\xbc'
utf-8 -> INVALID: can't decode byte 0xd3 in position 0
```

而任务 `9081`（root=410 / `95516.com`）报的正是 `0xd3 0xc3` —— **字节完全对上**。

> 该 root 下 141 个已解析主机 × 2 scheme 共 282 次探测，只有这 1 个站是 GB 编码，就把整轮扫描废掉了。

---

## 五、修复建议

### 方案 A（推荐，正解）：按 charset 解码后再取标题

`golang.org/x/net` **已经是直接依赖**（`go.mod:16`），`html/charset` 就在其中，**无需新增任何依赖**：

```go
import (
    "bytes"
    "golang.org/x/net/html/charset"
    "golang.org/x/text/encoding/simplifiedchinese"
    "strings"
)

// decodeHTML 按 Content-Type/meta 声明的 charset 转成 UTF-8；
// 声明缺失或解码失败时回退 GB18030；最后兜底剔除非法字节。
func decodeHTML(contentType string, body []byte) []byte {
    if r, err := charset.NewReader(bytes.NewReader(body), contentType); err == nil {
        if b, err := io.ReadAll(r); err == nil {
            return b
        }
    }
    if b, err := simplifiedchinese.GB18030.NewDecoder().Bytes(body); err == nil {
        return b
    }
    return []byte(strings.ToValidUTF8(string(body), "\uFFFD"))
}
```

改 `getWith`：

```go
r := &probeResult{...}
decoded := decodeHTML(resp.Header.Get("Content-Type"), body)   // ← 新增
if m := titleRe.FindSubmatch(decoded); m != nil {              // ← 用解码后的 body
    r.Title = strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
}
```

> 注意：`wappalyzergo.Fingerprint(header, body)` 继续用原始 body 即可（它自己处理编码）；只有取文本的 `title` 需要解码。

### 方案 B（兜底，强烈建议同时加）：入库前统一 sanitize

即使做了 A，`Redirect`（来自 `Location` 响应头）也可能携带非 UTF-8 字节（Go 的 header 值是原始字节），`title` 也可能有其它边缘编码。建议在 `storeURL` 组装参数前统一过一遍：

```go
func toValidUTF8(s string) string {
    if s == "" || utf8.ValidString(s) {
        return s
    }
    return strings.ToValidUTF8(s, "\uFFFD")
}
// storeURL 内：
title := toValidUTF8(r.Title)
redirect := toValidUTF8(r.Redirect)
```

### 方案 C（可选，容错性）：单条失败不废整任务

`httpprobe.go:592` 现在是 `return nil, firstErr`。可考虑：单 host 写库失败只记日志并计入 `result` 统计（degraded），不中断整任务——这样脏数据只丢一条而不是一轮。

---

## 六、同类风险点（顺带排查）

| 位置 | 字段 | 来源 | 是否同样风险 |
|---|---|---|---|
| `httpprobe.go:126` | `probeResult.Title` | HTML `<title>` 原始字节 | ✅ **本次 bug** |
| `httpprobe.go:156` | `probeResult.Redirect` | `resp.Header.Get("Location")` 原始字节 | ⚠️ 理论存在（建议一并 sanitize） |
| `httpprobe.go:633` | `events.detail` 里的 `title` | 同上 Title | 同源，修 Title 即解决（`json.Marshal` 会把非法字节替成 U+FFFD，故非首个报错点） |
| `dns.go` | `cnames` | DNS 报文（ASCII） | ❌ 无风险 |
| `passive.go:247` | `normalizeSubdomain` | crt.sh / subfinder 结果 | ❌ 本次已验证 payload JSON 可正常解码；但建议加 `utf8.ValidString` 兜底，成本极低 |
| `expand.go` | ICP 企业名 | JSON API（`json.Unmarshal` 会替换非法字节） | ❌ 无风险 |

---

## 七、验收标准

1. `curl http://gdinfo.95516.com/` 对应主机的 `urls.title` 正确落库为 **「用户登录」**（不是乱码、不是空、不报错）。
2. 重新触发 `95516.com` / `fenbi.com` / `xdf.cn` / `mi.com` 的 http 扫描，任务 `status = done`，无 `invalid byte sequence` 报错。
3. 回归测试建议：构造一个 GBK 页面（`Content-Type: text/html; charset=gbk`，title 为 GBK 字节）作为 fixture，断言 `probeResult.Title == "用户登录"` 且 `storeURL` 不报错。
4. 全库校验 SQL（应为 0）：

```sql
SELECT count(*) FROM scan_tasks
WHERE status='failed' AND error LIKE '%invalid byte sequence%';
```

---

## 八、附：本机验证命令

```bash
# 1) 看错误分布
docker compose exec -T postgres psql -U sam -d sam -c "
SELECT r.domain, count(*) AS fails
FROM scan_tasks t JOIN root_domains r ON r.id=t.root_id
WHERE t.error LIKE '%invalid byte sequence%'
GROUP BY r.domain ORDER BY fails DESC;"

# 2) 复现脏数据
curl -s -D- -o /dev/null http://gdinfo.95516.com/ | grep -i content-type
# => text/html; charset=gb18030

# 3) 确认 PG 拒收（模拟）
docker compose exec -T postgres psql -U sam -d sam -c "
SELECT convert_from('\xd3\xc3\xbb\xa7\xb5\xc7\xc2\xbc'::bytea, 'GB18030');"     -- 用户登录（PG 认得 GB18030）
-- 而 Go 侧传的是「原始字节冒充 UTF-8 字符串」，PG 以 UTF8 数据库编码校验 → 拒绝
```
