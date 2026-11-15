[CmdletBinding()]
param(
    [ValidateSet("T49", "T50", "T51", "T52", "T53", "T54", "All")]
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
    "All" {
        Invoke-T49
        Invoke-T50
        Invoke-T51
        Invoke-T52
        Invoke-T53
        Invoke-T54
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
