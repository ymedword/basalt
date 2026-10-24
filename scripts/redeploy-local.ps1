#Requires -Version 5.1
<#
本机重部署（US-12 R7）
======================
把当前仓库构建成新的 `bin/basalt` 并替换正在运行的本机实例，不手工重启、不丢数据。

流程：识别监听 PID → 确认它确实是本仓库的 `basalt serve` → 构建到 `bin/basalt.new`
→ 优雅停止（仅失败时才强制并记录）→ 换二进制（旧件保留为 `bin/basalt.old`）
→ 以同一工作目录启动新构建 → 校验存活 / 管理认证 / 前端资源哈希 → 报告
PID、地址、构建标识与日志路径（从不打印管理凭据）。

安全边界：
- 只停止命令行含 `serve` 且可执行文件就是这个仓库 `bin/basalt` 的进程；形态不符即拒绝。
- 工作目录必须含 `basalt.yaml`；脚本不改写配置、工作区、数据库与主密钥文件。
- 管理认证从进程环境 `BASALT_ADMIN_TOKEN` 继承（服务端与校验共用）；脚本不读取、
  不打印、不落盘任何凭据。
- 前端默认沿用已构建的 `assets/console/dist`；加 `-Frontend` 才会重跑 `npm run build`
  并重新生成 `integrity.json`。

示例（在实例目录执行，凭据在进程环境中）：
  powershell -NoProfile -File D:\code\basalt\scripts\redeploy-local.ps1
  powershell -NoProfile -File D:\code\basalt\scripts\redeploy-local.ps1 -Frontend
#>
[CmdletBinding()]
param(
    [int]$Port = 8080,
    [string]$WorkDir = '',
    [switch]$Frontend,
    [switch]$SkipBuild,
    [switch]$AllowOtherBinary,
    [int]$StopTimeoutSec = 30,
    [int]$HealthTimeoutSec = 60,
    # 内部模式：作为子 PowerShell 附加到目标控制台并发送 CTRL_BREAK。
    [int]$StopPid = 0
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# 中文输出在 UTF-8 终端与重定向下保持一致（PowerShell 5.1 默认用 OEM 代码页）。
[Console]::OutputEncoding = [Text.Encoding]::UTF8

# --- 内部模式：优雅停止助手 ------------------------------------------------

if ($StopPid -gt 0) {
    $signature = @'
using System;
using System.Runtime.InteropServices;
public static class BasaltConsoleCtl {
    [DllImport("kernel32.dll", SetLastError = true)] public static extern bool FreeConsole();
    [DllImport("kernel32.dll", SetLastError = true)] public static extern bool AttachConsole(uint pid);
    [DllImport("kernel32.dll", SetLastError = true)] public static extern bool SetConsoleCtrlHandler(IntPtr handler, bool add);
    [DllImport("kernel32.dll", SetLastError = true)] public static extern bool GenerateConsoleCtrlEvent(uint evt, uint group);
}
'@
    Add-Type -TypeDefinition $signature
    [BasaltConsoleCtl]::FreeConsole() | Out-Null
    if (-not [BasaltConsoleCtl]::AttachConsole([uint32]$StopPid)) { exit 2 }
    [BasaltConsoleCtl]::SetConsoleCtrlHandler([IntPtr]::Zero, $true) | Out-Null
    # 先只发给目标自己的进程组（组号 = 创建时带 CREATE_NEW_PROCESS_GROUP 的进程 PID），
    # 避免 CTRL_BREAK 波及同一控制台上的其它进程（部署终端、并行任务）；目标不是组长时
    # 该事件不会送达，2 秒后仍存活才退回整控制台（组 0）。助手自身可能一并退出，主脚本靠轮询判定。
    [BasaltConsoleCtl]::GenerateConsoleCtrlEvent(1, [uint32]$StopPid) | Out-Null
    Start-Sleep -Seconds 2
    $deadline = (Get-Date).AddSeconds(5)
    while ((Get-Date) -lt $deadline -and (Get-Process -Id $StopPid -ErrorAction SilentlyContinue)) { Start-Sleep -Milliseconds 300 }
    if (Get-Process -Id $StopPid -ErrorAction SilentlyContinue) {
        [BasaltConsoleCtl]::GenerateConsoleCtrlEvent(1, 0) | Out-Null
    }
    Start-Sleep -Seconds 2
    exit 0
}

function Write-Step([string]$Message) { Write-Output $Message }

# --- 0. 前置条件 -----------------------------------------------------------

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$binaryPath = Join-Path $repoRoot 'bin\basalt'
$newPath = Join-Path $repoRoot 'bin\basalt.new'
$oldPath = Join-Path $repoRoot 'bin\basalt.old'
if ([string]::IsNullOrWhiteSpace($WorkDir)) { $WorkDir = (Get-Location).Path }
$WorkDir = [IO.Path]::GetFullPath($WorkDir)
$configPath = Join-Path $WorkDir 'basalt.yaml'

if (-not (Test-Path -LiteralPath $configPath -PathType Leaf)) {
    throw "工作目录里没有 basalt.yaml：$WorkDir。请在实例目录执行，或显式传 -WorkDir。"
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw '找不到 go 命令；先按 docs/DevelopmentLoop.md §10 准备工具链。'
}

$baseUrl = "http://127.0.0.1:$Port"
$adminToken = $env:BASALT_ADMIN_TOKEN

function Get-ListenerPid {
    $conn = Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue |
        Where-Object { $_.LocalAddress -in @('127.0.0.1', '0.0.0.0', '::1', '::') } |
        Select-Object -First 1
    if ($null -eq $conn) { return 0 }
    return [int]$conn.OwningProcess
}

function Invoke-Json([string]$Path, [string]$Token) {
    $headers = @{}
    if (-not [string]::IsNullOrWhiteSpace($Token)) { $headers.Authorization = "Bearer $Token" }
    return Invoke-RestMethod -Method 'GET' -Uri ($baseUrl + $Path) -Headers $headers -TimeoutSec 5
}

# --- 1. 识别并核验当前实例 ------------------------------------------------

$oldPid = Get-ListenerPid
$wasRunning = $oldPid -gt 0
if ($wasRunning) {
    $proc = Get-CimInstance Win32_Process -Filter "ProcessId=$oldPid"
    if ($null -eq $proc) { throw "端口 $Port 的监听 PID $oldPid 查询不到进程信息。" }
    $procExe = [string]$proc.ExecutablePath
    $procCmd = [string]$proc.CommandLine
    Write-Step "当前实例：PID=$oldPid exe=$procExe"
    if ([IO.Path]::GetFileName($procExe) -ne 'basalt') {
        if (-not $AllowOtherBinary) { throw "端口 $Port 上的进程不是 basalt（$procExe）；拒绝操作，确需替换请加 -AllowOtherBinary。" }
    }
    if ($procCmd -notmatch '(^|\s)serve(\s|$)') {
        throw "PID $oldPid 的命令行不含 serve，不是 HTTP 服务进程；拒绝停止。命令行：$procCmd"
    }
    if (-not $AllowOtherBinary -and [IO.Path]::GetFullPath($procExe) -ne [IO.Path]::GetFullPath($binaryPath)) {
        throw "PID $oldPid 使用的二进制不是本仓库 $binaryPath（实际 $procExe）；拒绝替换，确需替换请加 -AllowOtherBinary。"
    }
    if ($null -eq (Get-Process -Id $oldPid -ErrorAction SilentlyContinue)) {
        throw "PID $oldPid 已不存在；请重跑脚本。"
    }
} else {
    Write-Step "端口 $Port 当前没有监听进程；本次只构建并启动。"
}

# --- 2. 构建 ---------------------------------------------------------------

if (-not $SkipBuild) {
    if ($Frontend) {
        $consoleDir = Join-Path $repoRoot 'assets\console'
        if (-not (Test-Path -LiteralPath (Join-Path $consoleDir 'package.json'))) {
            throw "前端目录不完整：$consoleDir"
        }
        Write-Step '构建前端（npm run build，含 integrity 生成）…'
        Push-Location $consoleDir
        try {
            & npm run build
            if ($LASTEXITCODE -ne 0) { throw "前端构建失败（exit $LASTEXITCODE）" }
        } finally { Pop-Location }
    }

    $env:GOPROXY = 'https://goproxy.cn'
    $env:CGO_ENABLED = '0'
    $version = 'dev'
    Push-Location $repoRoot
    try {
        $described = & git describe --tags --always 2>$null
        if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($described)) { $version = $described.Trim() }
    } finally { Pop-Location }

    Write-Step "构建后端：version=$version -> $newPath"
    Push-Location $repoRoot
    try {
        & go build -trimpath -ldflags "-s -w -X main.version=$version" -o $newPath ./cmd/basalt
        if ($LASTEXITCODE -ne 0) { throw "go build 失败（exit $LASTEXITCODE）" }
    } finally { Pop-Location }
} else {
    Write-Step '跳过构建（-SkipBuild）：直接使用现有 bin\basalt.new。'
}
if (-not (Test-Path -LiteralPath $newPath -PathType Leaf)) { throw "找不到待部署二进制：$newPath" }

# --- 3. 停止（优雅优先，失败才强制） --------------------------------------

$stopMode = 'skipped'
if ($wasRunning) {
    $helper = Start-Process -FilePath 'powershell.exe' `
        -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'RemoteSigned', '-File', $PSCommandPath, '-StopPid', "$oldPid") `
        -WindowStyle Hidden -PassThru
    $deadline = (Get-Date).AddSeconds($StopTimeoutSec)
    while ((Get-Date) -lt $deadline) {
        if ($null -eq (Get-Process -Id $oldPid -ErrorAction SilentlyContinue)) { break }
        Start-Sleep -Milliseconds 500
    }
    if ($null -eq (Get-Process -Id $oldPid -ErrorAction SilentlyContinue)) {
        $stopMode = 'graceful'
    } else {
        Write-Warning "优雅停止超时（${StopTimeoutSec}s），改用 taskkill /F 并记录。"
        & taskkill /PID $oldPid /F | Out-Null
        Start-Sleep -Seconds 2
        if ($null -ne (Get-Process -Id $oldPid -ErrorAction SilentlyContinue)) {
            throw "PID $oldPid 无法停止；未替换任何文件。"
        }
        $stopMode = 'forced'
    }
    if ($null -ne (Get-Process -Id $helper.Id -ErrorAction SilentlyContinue)) {
        Stop-Process -Id $helper.Id -Force -ErrorAction SilentlyContinue
    }
    Write-Step "停止结果：$stopMode（旧 PID=$oldPid）"
}

# --- 4. 换二进制 -----------------------------------------------------------

if (Test-Path -LiteralPath $binaryPath -PathType Leaf) {
    if (Test-Path -LiteralPath $oldPath) { Remove-Item -LiteralPath $oldPath -Force }
    Move-Item -LiteralPath $binaryPath -Destination $oldPath
}
Move-Item -LiteralPath $newPath -Destination $binaryPath
Write-Step "已替换：$binaryPath（上一版保留在 $oldPath）"

# --- 5. 启动新构建（独立隐藏控制台，日志落文件） --------------------------

$logDir = Join-Path $WorkDir 'logs'
New-Item -ItemType Directory -Path $logDir -Force | Out-Null
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$outLog = Join-Path $logDir "basalt-$stamp.out.log"
$errLog = Join-Path $logDir "basalt-$stamp.err.log"
# 必须按完整路径直接启动：经 cmd 启动时，无扩展名的 `bin\basalt` 会被同目录残留的
# `basalt.exe` 抢先解析（CreateProcess 追加 .exe 的规则），部署了旧构建却以为成功。
# 演练实测：cmd 启动实际执行的是 bin\basalt.exe（旧 schema），Start-Process 直连才落到新构建。
Start-Process -FilePath $binaryPath -ArgumentList 'serve' -WorkingDirectory $WorkDir `
    -WindowStyle Hidden -RedirectStandardOutput $outLog -RedirectStandardError $errLog | Out-Null

$deadline = (Get-Date).AddSeconds($HealthTimeoutSec)
$newPid = 0
$healthy = $false
while ((Get-Date) -lt $deadline) {
    $newPid = Get-ListenerPid
    if ($newPid -gt 0) {
        try {
            $health = Invoke-Json '/api/v1/health' ''
            if ($health.data.status -eq 'ok') { $healthy = $true; break }
        } catch { }
    }
    Start-Sleep -Milliseconds 500
}
if (-not $healthy) {
    Write-Output "启动失败或健康检查超时。日志：$outLog ; $errLog"
    if (Test-Path -LiteralPath $errLog) { Get-Content -LiteralPath $errLog -Tail 20 }
    exit 1
}

# --- 6. 校验管理认证与前端资源 --------------------------------------------

$authState = 'unverified'
$buildVersion = ''
$schemaVersion = ''
if (-not [string]::IsNullOrWhiteSpace($adminToken)) {
    try {
        $overview = Invoke-Json '/api/v1/system/overview' $adminToken
        $authState = 'auth-ok'
        if ($null -ne $overview.data.database.schema_version) { $schemaVersion = [string]$overview.data.database.schema_version }
    } catch {
        $authState = 'auth-failed'
        Write-Warning "管理认证校验失败：$($_.Exception.Message)"
    }
    try {
        $info = Invoke-Json '/api/v1/info' $adminToken
        $buildVersion = [string]$info.data.version
    } catch {
        Write-Warning "/info 读取失败：$($_.Exception.Message)"
    }
} else {
    Write-Warning '进程环境没有 BASALT_ADMIN_TOKEN：管理认证与 /info 未验证（admin=unverified）。'
    try {
        $info = Invoke-Json '/api/v1/info' ''
        $buildVersion = [string]$info.data.version
    } catch { }
}

$integrityPath = Join-Path $repoRoot 'assets\console\dist\integrity.json'
$assetOk = 0
$assetTotal = 0
if (-not (Test-Path -LiteralPath $integrityPath -PathType Leaf)) {
    Write-Warning "找不到前端完整性清单：$integrityPath（前端资源未校验）"
} else {
    $integrity = Get-Content -LiteralPath $integrityPath -Raw | ConvertFrom-Json
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("basalt-asset-" + [Guid]::NewGuid().ToString('N'))
    foreach ($prop in $integrity.PSObject.Properties) {
        $assetTotal++
        $rel = [string]$prop.Name
        $url = if ($rel -eq 'index.html') { "$baseUrl/console/" } else { "$baseUrl/console/$rel" }
        try {
            Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing -TimeoutSec 10
            $actual = (Get-FileHash -LiteralPath $tmp -Algorithm SHA256).Hash.ToLowerInvariant()
            $expected = ([string]$prop.Value).ToLowerInvariant()
            if ($actual -eq $expected) { $assetOk++ } else { Write-Warning "资源哈希不符：$rel 期望 $expected 实际 $actual" }
        } catch {
            Write-Warning "资源读取失败：$rel $($_.Exception.Message)"
        }
    }
    Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
}

$ok = ($authState -ne 'auth-failed') -and ($assetTotal -gt 0) -and ($assetOk -eq $assetTotal)
Write-Output ''
Write-Output 'Redeploy 结果'
Write-Output "  PID=$newPid Address=$baseUrl WorkDir=$WorkDir"
Write-Output "  Build=$buildVersion Schema=$schemaVersion Admin=$authState Assets=$assetOk/$assetTotal Stop=$stopMode"
Write-Output "  Logs=$outLog ; $errLog"
Write-Output '  凭据：本脚本未读取、未打印、未落盘。'
if (-not $ok) { exit 1 }
exit 0
