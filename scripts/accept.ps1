[CmdletBinding()]
param(
    [ValidateSet("T49", "T50", "T51", "T52", "T53", "T54", "T55", "T56", "T57", "T58", "T60", "All")]
    [string]$Task = "All",
    [int]$Count = 1,
    [switch]$SkipFrontend
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
$backend = Join-Path $repo "backend"
$frontend = Join-Path $repo "frontend"
$checkScript = Join-Path $PSScriptRoot "check.ps1"
$script:results = @()

function Add-Result([string]$task, [string]$name, [bool]$ok, [string]$detail) {
    $script:results += [pscustomobject]@{
        Task   = $task
        Check  = $name
        Result = $(if ($ok) { "PASS" } else { "FAIL" })
        Detail = $detail
    }
}

function Assert-Exists([string]$path, [string]$desc) {
    if (-not (Test-Path $path)) { throw "缺少${desc}：$path" }
}

function Assert-Text([string]$path, [string]$pattern, [string]$desc) {
    if (-not (Select-String -Path $path -Pattern $pattern -Quiet)) {
        throw "$desc（$path 未匹配 /$pattern/）"
    }
}

function Invoke-Check([string]$task, [string]$name, [scriptblock]$body) {
    Write-Host "  - $name" -NoNewline
    try {
        & $body
        Write-Host "  PASS" -ForegroundColor Green
        Add-Result $task $name $true ""
    }
    catch {
        Write-Host "  FAIL" -ForegroundColor Red
        Add-Result $task $name $false $_.Exception.Message
    }
}

function Invoke-GoTests([string]$pattern) {
    & powershell -NoProfile -ExecutionPolicy Bypass -File $checkScript -Scope Go -Run $pattern -Count $Count
    if ($LASTEXITCODE -ne 0) { throw "go test -run '$pattern' 失败" }
}

function Invoke-FrontendTests {
    if ($SkipFrontend) { return }
    if (-not (Test-Path (Join-Path $frontend "node_modules"))) {
        throw "前端依赖未安装；先跑 scripts/check.ps1 -Scope Frontend 或去掉 -SkipFrontend"
    }
    Push-Location $frontend
    try {
        npm test
        if ($LASTEXITCODE -ne 0) { throw "vitest 失败" }
    }
    finally { Pop-Location }
}

function Assert-NoIPExposure([string]$task) {
    Invoke-Check $task "API/UI 不含 IP 字段" {
        Assert-Text (Join-Path $backend "migrations\027_asset_services.sql") "CREATE TABLE" "asset_services 迁移缺失"
        if (Select-String -Path (Join-Path $backend "migrations\027_asset_services.sql") -Pattern "(?m)^\s*ip\s+(TEXT|INTEGER|BIGINT|INET|CIDR|VARCHAR)" -Quiet) {
            throw "asset_services 迁移出现 ip 字段"
        }
        if (Select-String -Path (Join-Path $backend "openapi.yaml") -Pattern "^\s+ip:" -Quiet) {
            throw "OpenAPI 出现 ip 属性"
        }
        $pageFiles = @(Get-ChildItem (Join-Path $frontend "src\pages") -Filter "*.jsx" | Select-Object -ExpandProperty FullName)
        if ($pageFiles.Count -gt 0 -and (Select-String -Path $pageFiles -Pattern "dataIndex:\s*['`"]ip['`"]" -Quiet)) {
            throw "前端页面出现 IP 列"
        }
    }
}

function Invoke-T49 {
    Write-Host "`nT49 授权范围门控" -ForegroundColor Cyan
    Invoke-Check "T49" "实现文件存在" {
        Assert-Exists (Join-Path $backend "service_scan.go") "service_scan.go"
        Assert-Exists (Join-Path $backend "t49_service_scan_test.go") "T49 测试"
    }
    Invoke-Check "T49" "测试通过（T49）" { Invoke-GoTests "T49" }
    Invoke-Check "T49" "SERVICE_SCAN_ENABLED 默认 false" {
        Assert-Text (Join-Path $repo "docs\REFERENCE.md") "SERVICE_SCAN_ENABLED\s*\|\s*false" "REFERENCE 默认值不是 false"
    }
}

function Invoke-T50 {
    Write-Host "`nT50 CDN 识别门控" -ForegroundColor Cyan
    Invoke-Check "T50" "实现文件存在" {
        Assert-Exists (Join-Path $backend "cdn_fronted.go") "cdn_fronted.go"
        Assert-Exists (Join-Path $backend "t50_cdn_fronted_test.go") "T50 测试"
    }
    Invoke-Check "T50" "测试通过（T50）" { Invoke-GoTests "T50" }
    Invoke-Check "T50" "三态迁移 030 存在" {
        Assert-Exists (Join-Path $backend "migrations\030_cdn_fronted_nullable.sql") "030 迁移"
    }
}

function Invoke-T51 {
    Write-Host "`nT51 端口 + 服务指纹" -ForegroundColor Cyan
    Invoke-Check "T51" "实现文件存在" {
        Assert-Exists (Join-Path $backend "service_fingerprint.go") "service_fingerprint.go"
        Assert-Exists (Join-Path $backend "service_banner.go") "service_banner.go"
        Assert-Exists (Join-Path $backend "service_dial_policy.go") "service_dial_policy.go"
    }
    Invoke-Check "T51" "测试通过（T51）" { Invoke-GoTests "T51" }
    Invoke-Check "T51" "打回点：banner 先脱敏后截断" {
        Assert-Text (Join-Path $backend "t51_rework_test.go") "TestT51SanitizeBannerRework" "缺少脱敏回归测试"
        Assert-Text (Join-Path $backend "service_banner.go") "sanitizeServiceBanner" "缺少脱敏实现"
    }
    Invoke-Check "T51" "打回点：MySQL/MariaDB 握手版本" {
        Assert-Text (Join-Path $backend "t51_rework_test.go") "TestT51MySQL8AndMariaDBHandshake" "缺少握手测试"
    }
    Invoke-Check "T51" "未判定 CDN 跳过" {
        Assert-Text (Join-Path $backend "t51_rework_test.go") "TestT51UnjudgedCDNSkipped" "缺少未判定测试"
    }
    Invoke-Check "T51" "只拨判定 IP 且拒内网" {
        Assert-Text (Join-Path $backend "t51_rework_test.go") "TestT51OnlyDialsResolvedIPs" "缺少只拨判定 IP 测试"
        Assert-Text (Join-Path $backend "t51_rework_test.go") "TestT51DefaultPolicyRejectsPrivateIPs" "缺少拒内网测试"
    }
    Invoke-Check "T51" "SCHEDULE_SERVICE 默认禁用" {
        Assert-Text (Join-Path $repo "docs\REFERENCE.md") "SCHEDULE_SERVICE.*(禁用|空)" "REFERENCE 未写明默认禁用"
    }
    Assert-NoIPExposure "T51"
}

function Invoke-T52 {
    Write-Host "`nT52 非 Web 服务并入组件库存" -ForegroundColor Cyan
    Invoke-Check "T52" "实现文件存在" {
        Assert-Exists (Join-Path $backend "service_inventory.go") "service_inventory.go"
        Assert-Exists (Join-Path $backend "migrations\029_asset_components.sql") "029 迁移"
    }
    Invoke-Check "T52" "测试通过（T52）" { Invoke-GoTests "T52" }
    Invoke-Check "T52" "未识别 banner 聚类" {
        Assert-Text (Join-Path $backend "service_inventory.go") "unidentified-service-" "缺少聚类命名"
        Assert-Text (Join-Path $backend "service_inventory.go") "confirmed" "缺少 confirmed 口径"
    }
    Invoke-Check "T52" "前端用例与页面" {
        Assert-Exists (Join-Path $frontend "src\t52.test.jsx") "T52 前端测试"
        Assert-Exists (Join-Path $frontend "src\pages\Components.jsx") "组件页面"
    }
    Invoke-Check "T52" "前端测试通过" { Invoke-FrontendTests }
    Assert-NoIPExposure "T52"
}

function Invoke-T53 {
    Write-Host "`nT53 可插拔 YAML 指纹库" -ForegroundColor Cyan
    $impl = Join-Path $backend "service_fingerprint_yaml.go"
    $rules = Join-Path $backend "fingerprints\services.yaml"
    $test = Join-Path $backend "t53_fingerprint_yaml_test.go"

    Invoke-Check "T53" "实现文件存在" {
        Assert-Exists $impl "YAML 加载实现"
        Assert-Exists $rules "指纹规则文件"
        Assert-Exists $test "T53 测试"
    }
    if ((Test-Path $impl) -and (Test-Path $test)) {
        Invoke-Check "T53" "测试通过（T53）" { Invoke-GoTests "T53" }
        Invoke-Check "T53" "非法规则被拦" {
            Assert-Text $test "(?i)invalid|illegal|非法" "缺少非法规则测试"
        }
        Invoke-Check "T53" "只发 YAML 声明的只读报文" {
            Assert-Text $test "(?i)probe|报文|declared" "缺少探测报文测试"
        }
    }
    Invoke-Check "T53" "环境变量入口" {
        Assert-Text (Join-Path $repo "docs\REFERENCE.md") "SERVICE_FINGERPRINT_RULES" "REFERENCE 未记录规则文件变量"
    }
}

function Invoke-T55 {
    Write-Host "`nT55 探测结果三态" -ForegroundColor Cyan
    Invoke-Check "T55" "实现与迁移存在" {
        Assert-Exists (Join-Path $backend "migrations\032_service_response_kind.sql") "032 迁移"
        Assert-Exists (Join-Path $backend "t55_service_response_kind_test.go") "T55 测试"
    }
    Invoke-Check "T55" "测试通过（T55）" { Invoke-GoTests "T55" }
    Invoke-Check "T55" "空 banner 不生成候选" {
        Assert-Text (Join-Path $backend "t55_service_response_kind_test.go") "(?i)empty|空" "缺少空 banner 用例"
    }
    Invoke-Check "T55" "迁移可重跑" {
        Assert-Text (Join-Path $backend "migrations\032_service_response_kind.sql") "IF NOT EXISTS" "迁移不可重跑"
    }
    Invoke-Check "T55" "API 与前端展示 verified" {
        Assert-Text (Join-Path $backend "openapi.yaml") "verified" "OpenAPI 未暴露 verified"
        Assert-Text (Join-Path $frontend "src\pages\ServiceScan.jsx") "verified" "服务扫描页未展示 verified"
    }
}

function Invoke-T56 {
    Write-Host "`nT56 反代/CDN 边缘识别" -ForegroundColor Cyan
    Invoke-Check "T56" "实现与迁移存在" {
        Assert-Exists (Join-Path $backend "migrations\033_reverse_proxy_backfill.sql") "033 迁移"
        Assert-Exists (Join-Path $backend "t56_reverse_proxy_test.go") "T56 测试"
    }
    Invoke-Check "T56" "测试通过（T56）" { Invoke-GoTests "T56" }
    Invoke-Check "T56" "反代特征覆盖 openresty/nginx/Tengine" {
        Assert-Text (Join-Path $backend "migrations\033_reverse_proxy_backfill.sql") "openresty|nginx|Tengine" "迁移缺少反代特征"
    }
    Invoke-Check "T56" "400/403 与 200 对照用例" {
        Assert-Text (Join-Path $backend "t56_reverse_proxy_test.go") "403" "缺少 403 用例"
        Assert-Text (Join-Path $backend "t56_reverse_proxy_test.go") "200" "缺少 200 不误伤用例"
    }
    Invoke-Check "T56" "记 reverse_proxy 跳过原因" {
        Assert-Text (Join-Path $backend "service_fingerprint.go") "reverse_proxy" "未写 reverse_proxy 分类"
    }
}

function Invoke-T57 {
    Write-Host "`nT57 未识别聚类归一化" -ForegroundColor Cyan
    Invoke-Check "T57" "实现与迁移存在" {
        Assert-Exists (Join-Path $backend "migrations\034_unidentified_recluster.sql") "034 迁移"
        Assert-Exists (Join-Path $backend "migrations\035_unidentified_orphan_cleanup.sql") "035 孤儿清理迁移"
        Assert-Exists (Join-Path $backend "t57_unidentified_recluster_test.go") "T57 测试"
    }
    Invoke-Check "T57" "测试通过（T57）" { Invoke-GoTests "T57" }
    Invoke-Check "T57" "动态字段归一化" {
        Assert-Text (Join-Path $backend "service_inventory.go") "normalizeServiceBannerForHash" "缺少归一化函数"
        Assert-Text (Join-Path $backend "service_inventory.go") "(?i)Date|Request_id|Client_ip" "归一化未覆盖动态字段"
    }
    Invoke-Check "T57" "回填可重跑且保留状态" {
        Assert-Text (Join-Path $backend "migrations\034_unidentified_recluster.sql") "ON CONFLICT|IF NOT EXISTS|DROP TABLE IF EXISTS" "迁移不可重跑"
        Assert-Text (Join-Path $backend "migrations\034_unidentified_recluster.sql") "confirmed" "回填未保留 confirmed"
    }
}

function Invoke-T58 {
    Write-Host "`nT58 no_dial_ip 收口" -ForegroundColor Cyan
    Invoke-Check "T58" "实现与迁移存在" {
        Assert-Exists (Join-Path $backend "migrations\036_service_skip_dedup.sql") "036 迁移"
        Assert-Exists (Join-Path $backend "t58_no_dial_ip_test.go") "T58 测试"
    }
    Invoke-Check "T58" "测试通过（T58）" { Invoke-GoTests "T58" }
    Invoke-Check "T58" "skip 原因细分" {
        Assert-Text (Join-Path $backend "service_dial_policy.go") "dns_nxdomain" "缺少 dns_nxdomain"
        Assert-Text (Join-Path $backend "service_dial_policy.go") "dns_negative_cached" "缺少 dns_negative_cached"
    }
    Invoke-Check "T58" "NXDOMAIN 双 resolver 与 A/AAAA 分开" {
        Assert-Text (Join-Path $backend "t58_no_dial_ip_test.go") "NXDomain" "缺少 NXDOMAIN 复核用例"
        Assert-Text (Join-Path $backend "t58_no_dial_ip_test.go") "AAAA" "缺少 A/AAAA 用例"
    }
    Invoke-Check "T58" "skip 去重与 scan_task_id" {
        Assert-Text (Join-Path $backend "migrations\036_service_skip_dedup.sql") "scan_task_id" "缺少 scan_task_id"
        Assert-Text (Join-Path $backend "migrations\036_service_skip_dedup.sql") "CREATE UNIQUE INDEX" "缺少唯一索引"
    }
    Invoke-Check "T58" "负缓存 TTL 配置" {
        Assert-Text (Join-Path $backend "service_fingerprint.go") "SERVICE_DNS_NEGATIVE_TTL" "缺少 TTL 配置"
        Assert-Text (Join-Path $repo "docs\REFERENCE.md") "SERVICE_DNS_NEGATIVE_TTL" "REFERENCE 未记录 TTL"
    }
    Invoke-Check "T58" "CDN 复判用例" {
        Assert-Text (Join-Path $backend "t58_no_dial_ip_test.go") "CDN" "缺少 CDN 复判用例"
    }
}

function Invoke-T60 {
    Write-Host "`nT60 组件-资产 8 字段" -ForegroundColor Cyan
    Invoke-Check "T60" "实现与迁移存在" {
        Assert-Exists (Join-Path $backend "migrations\037_component_asset_fields.sql") "037 迁移"
        Assert-Exists (Join-Path $backend "t60_component_fields_test.go") "T60 测试"
    }
    Invoke-Check "T60" "测试通过（T60）" { Invoke-GoTests "T60" }
    Invoke-Check "T60" "8 字段 API" {
        foreach ($f in @("component_evidence", "component_confidence", "matched_url", "version_hint", "exposure_state", "auth_state", "component_candidate_repo", "capture_time")) {
            Assert-Text (Join-Path $backend "components_api.go") $f "缺少 $f"
        }
    }
    Invoke-Check "T60" "筛选参数" {
        Assert-Text (Join-Path $backend "components_api.go") "min_confidence" "缺少 min_confidence"
        Assert-Text (Join-Path $backend "components_api.go") "exposure" "缺少 exposure"
        Assert-Text (Join-Path $backend "openapi.yaml") "min_confidence" "OpenAPI 未同步"
    }
    Invoke-Check "T60" "回填默认 unknown/low" {
        Assert-Text (Join-Path $backend "migrations\037_component_asset_fields.sql") "DEFAULT 'unknown'" "缺少 unknown 默认值"
        Assert-Text (Join-Path $backend "migrations\037_component_asset_fields.sql") "DEFAULT 'low'" "缺少 low 默认值"
    }
    Invoke-Check "T60" "无 IP" {
        Assert-Text (Join-Path $backend "t60_component_fields_test.go") "NoIP|noIP|IP" "缺少无 IP 断言"
    }
}

function Invoke-T54 {
    Write-Host "`nT54 研究价值评分" -ForegroundColor Cyan
    Invoke-Check "T54" "实现文件存在" {
        Assert-Exists (Join-Path $backend "research_score.go") "research_score.go"
        Assert-Exists (Join-Path $backend "migrations\031_research_score.sql") "031 迁移"
        Assert-Exists (Join-Path $backend "t54_research_score_test.go") "T54 测试"
    }
    Invoke-Check "T54" "测试通过（T54）" { Invoke-GoTests "T54" }
    Invoke-Check "T54" "四维等权与 NULL 口径" {
        Assert-Text (Join-Path $backend "research_score.go") "0\.25" "缺少等权权重"
        Assert-Text (Join-Path $backend "t54_research_score_test.go") "(?i)null" "缺少 NULL 用例"
    }
    Invoke-Check "T54" "API 支持研究价值排序" {
        Assert-Text (Join-Path $backend "openapi.yaml") "research" "OpenAPI 未暴露研究价值排序"
    }
    Invoke-Check "T54" "前端用例与列" {
        Assert-Exists (Join-Path $frontend "src\t54.test.jsx") "T54 前端测试"
        Assert-Text (Join-Path $frontend "src\pages\Components.jsx") "research" "组件页未加研究价值列"
    }
    Invoke-Check "T54" "服务规则声明 repo" {
        Assert-Text (Join-Path $backend "fingerprints\services.yaml") "repo:" "YAML 未声明服务官方 repo"
    }
    Invoke-Check "T54" "REFERENCE 记录公式与权重" {
        Assert-Text (Join-Path $repo "docs\REFERENCE.md") "0\.25" "REFERENCE 未记录权重"
    }
    Invoke-Check "T54" "前端测试通过" { Invoke-FrontendTests }
}

switch ($Task) {
    "T49" { Invoke-T49 }
    "T50" { Invoke-T50 }
    "T51" { Invoke-T51 }
    "T52" { Invoke-T52 }
    "T53" { Invoke-T53 }
    "T54" { Invoke-T54 }
    "T55" { Invoke-T55 }
    "T56" { Invoke-T56 }
    "T57" { Invoke-T57 }
    "T58" { Invoke-T58 }
    "T60" { Invoke-T60 }
    "All" {
        Invoke-T49
        Invoke-T50
        Invoke-T51
        Invoke-T52
        Invoke-T53
        Invoke-T54
        Invoke-T55
        Invoke-T56
        Invoke-T57
        Invoke-T58
        Invoke-T60
    }
}

Write-Host "`n验收结果" -ForegroundColor White
$script:results | Format-Table -AutoSize | Out-Host

$failed = @($script:results | Where-Object { $_.Result -eq "FAIL" })
if ($failed.Count -gt 0) {
    Write-Host "未通过：$($failed.Count) 项" -ForegroundColor Red
    exit 1
}
Write-Host "全部通过。" -ForegroundColor Green
