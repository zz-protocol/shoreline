[CmdletBinding()]
param(
    [ValidateSet("All", "Go", "Frontend")]
    [string]$Scope = "All",
    [string]$Run = "",
    [int]$Count = 1,
    [switch]$Fmt,
    [switch]$SkipVet,
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
$backend = Join-Path $repo "backend"
$frontend = Join-Path $repo "frontend"
$pgContainer = "shoreline-postgres-1"
$testDb = "sam_test"
$dbUrl = "postgres://sam:sam@${pgContainer}:5432/${testDb}?sslmode=disable"
$goImage = "golang:1.26-alpine"
$network = "shoreline_default"
$moduleVolume = "shoreline-go-mod"

function Write-Step($text) { Write-Host "`n==> $text" -ForegroundColor Cyan }
function Fail($text) { Write-Host "FAIL: $text" -ForegroundColor Red; exit 1 }
function Pass($text) { Write-Host "PASS: $text" -ForegroundColor Green }

function Test-Docker {
    docker info *> $null
    if ($LASTEXITCODE -ne 0) { Fail "Docker 不可用，请先启动 Docker Desktop" }
}

function Test-DockerObjectExists([string]$name) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "SilentlyContinue"
    docker inspect $name *> $null
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev
    return ($code -eq 0)
}

function Ensure-TestDatabase {
    if (-not (Test-DockerObjectExists $pgContainer)) { Fail "找不到容器 $pgContainer，请先 docker compose up -d" }
    $exists = docker exec $pgContainer psql -U sam -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$testDb'"
    if ($LASTEXITCODE -ne 0) { Fail "无法查询 PostgreSQL" }
    if ("$exists".Trim() -ne "1") {
        Write-Step "创建测试库 $testDb"
        docker exec $pgContainer psql -U sam -d postgres -c "CREATE DATABASE $testDb" | Out-Null
        if ($LASTEXITCODE -ne 0) { Fail "创建测试库失败" }
    }
}

function Ensure-ModuleVolume {
    if (-not (Test-DockerObjectExists $moduleVolume)) { docker volume create $moduleVolume | Out-Null }
}

function Invoke-GoCheck {
    Ensure-TestDatabase
    Ensure-ModuleVolume
    if (-not $Fmt) { Write-Host "（跳过 gofmt：Windows checkout 为 CRLF；需要时加 -Fmt，CI 用 LF）" -ForegroundColor DarkGray }

    $backendPosix = $backend -replace '\\', '/'
    $scriptsPosix = (Join-Path $repo "scripts") -replace '\\', '/'
    $fmtFlag = if ($Fmt) { "0" } else { "1" }
    $vetFlag = if ($SkipVet) { "1" } else { "0" }

    $dockerArgs = @(
        "run", "--rm",
        "--network", $network,
        "-v", "${backendPosix}:/src",
        "-v", "${scriptsPosix}/check-go.sh:/check.sh:ro",
        "-v", "${moduleVolume}:/go/pkg/mod",
        "-w", "/src",
        "-e", "TEST_DATABASE_URL=$dbUrl",
        "-e", "GOPROXY=https://goproxy.cn,direct",
        "-e", "COUNT=$Count",
        "-e", "RUN_ARG=$Run",
        "-e", "SKIP_FMT=$fmtFlag",
        "-e", "SKIP_VET=$vetFlag",
        $goImage,
        "sh", "/check.sh"
    )
    & docker @dockerArgs
    if ($LASTEXITCODE -ne 0) { Fail "后端检查失败" }
    Pass "后端 gofmt / vet / go test -count=$Count 通过"
}

function Invoke-FrontendCheck {
    if (-not (Test-Path (Join-Path $frontend "node_modules"))) {
        Write-Step "安装前端依赖（npm ci）"
        Push-Location $frontend
        try {
            npm ci
            if ($LASTEXITCODE -ne 0) { Fail "npm ci 失败" }
        }
        finally { Pop-Location }
    }

    Write-Step "前端单元测试（vitest run）"
    Push-Location $frontend
    try {
        npm test
        if ($LASTEXITCODE -ne 0) { Fail "前端测试失败" }
        Pass "vitest 通过"

        if (-not $SkipBuild) {
            Write-Step "前端生产构建（vite build）"
            npm run build
            if ($LASTEXITCODE -ne 0) { Fail "前端构建失败" }
            Pass "前端构建通过"
        }
    }
    finally { Pop-Location }
}

Write-Host "岸线检查：Scope=$Scope Count=$Count" -ForegroundColor White
Test-Docker

if ($Scope -eq "All" -or $Scope -eq "Go") {
    Write-Step "后端检查（容器 $goImage）"
    Invoke-GoCheck
}

if ($Scope -eq "All" -or $Scope -eq "Frontend") {
    Write-Step "前端检查"
    Invoke-FrontendCheck
}

Write-Host "`n全部通过。" -ForegroundColor Green
