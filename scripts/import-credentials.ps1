#Requires -Version 5.1
<#
一次性初始化凭据导入（US-12 R7）
=================================
把用户本地填写的初始化模板导入**现有**管理资源：更新既有 `deepseek`（模型连接）与
`feishu-ops`（通知渠道），保留 resource_uid，不删除重建。

硬约束：
- 模板必须放在仓库之外；脚本拒绝读取仓库内的文件。模板内容只在进程内存中使用，
  不打印、不落盘、不写日志。
- 只调用现有管理写入端点，复用服务端的加密、审计与版本发布路径；不直接写数据库，
  也不接触主密钥。
- 幂等：目标已是托管凭据且尾号一致 → 跳过；尾号不同且未显式 `-Rotate` → 不覆盖并
  以退出码 3 报告；重复导入不会重复创建资源。
- `-Test` 会真实发起一次模型最小调用与一次渠道连通性测试（固定文案
  `Basalt 连通性测试`）；测试结论按 success / failed / unknown 展示，unknown 不得
  显示成已验证。

模板格式（KEY=VALUE，每行一条，`#` 开头为注释）：
  BASALT_BASE_URL=http://127.0.0.1:8080
  DEEPSEEK_API_KEY=...
  DEEPSEEK_BASE_URL=https://api.deepseek.com
  DEEPSEEK_DEFAULT_MODEL=deepseek-chat
  FEISHU_WEBHOOK_URL=...
  BASALT_ADMIN_TOKEN=...        # 也可改由进程环境变量提供，推荐后者

示例：powershell -NoProfile -File scripts/import-credentials.ps1 `
        -TemplatePath D:\code\basalt-demo-us12\credentials.init -Test
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$TemplatePath,
    [string]$BaseUrl = '',
    [string]$AdminToken = $env:BASALT_ADMIN_TOKEN,
    [switch]$Rotate,
    [switch]$Test
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# 中文输出在 UTF-8 终端与重定向下保持一致（PowerShell 5.1 默认用 OEM 代码页）。
[Console]::OutputEncoding = [Text.Encoding]::UTF8

function Get-Last4([string]$Value) {
    if ($Value.Length -ge 8) { return $Value.Substring($Value.Length - 4) }
    return ''
}

function Invoke-Admin {
    param(
        [string]$Method,
        [string]$Path,
        $Body,
        [hashtable]$ExtraHeaders
    )
    $headers = @{ Authorization = "Bearer $script:token" }
    if ($ExtraHeaders) {
        foreach ($key in $ExtraHeaders.Keys) { $headers[$key] = $ExtraHeaders[$key] }
    }
    $params = @{
        Method      = $Method
        Uri         = ($script:baseUrl + '/api/v1' + $Path)
        Headers     = $headers
        ErrorAction = 'Stop'
    }
    if ($null -ne $Body) {
        $params.Body = ($Body | ConvertTo-Json -Depth 8 -Compress)
        $params.ContentType = 'application/json'
    }
    try {
        $response = Invoke-RestMethod @params
        return @{ ok = $true; status = 200; data = $response }
    } catch {
        $status = 0
        if ($_.Exception.Response -and $_.Exception.Response.StatusCode) {
            $status = [int]$_.Exception.Response.StatusCode
        }
        $detail = ''
        if ($_.ErrorDetails -and $_.ErrorDetails.Message) { $detail = [string]$_.ErrorDetails.Message }
        if ($detail.Length -gt 300) { $detail = $detail.Substring(0, 300) + '…' }
        return @{ ok = $false; status = $status; detail = $detail }
    }
}

function Write-Step([string]$Message) { Write-Output $Message }

# --- 0. 模板位置与读取条件 -------------------------------------------------

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$templateFull = [IO.Path]::GetFullPath($TemplatePath)
if (-not (Test-Path -LiteralPath $templateFull -PathType Leaf)) {
    throw "模板不存在：$templateFull"
}
$separator = [IO.Path]::DirectorySeparatorChar
$repoPrefix = $repoRoot.TrimEnd($separator) + $separator
if ($templateFull.StartsWith($repoPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw '模板必须放在仓库之外：仓库内文件会被版本控制、备份或评审读取。'
}

$values = @{}
$lineNo = 0
foreach ($line in Get-Content -LiteralPath $templateFull -Encoding UTF8) {
    $lineNo++
    $trimmed = $line.Trim()
    if ($trimmed -eq '' -or $trimmed.StartsWith('#')) { continue }
    $idx = $trimmed.IndexOf('=')
    if ($idx -lt 1) { throw "模板第 $lineNo 行格式不对：应为 KEY=VALUE" }
    $key = $trimmed.Substring(0, $idx).Trim().ToUpperInvariant()
    $value = $trimmed.Substring($idx + 1).Trim()
    if ($value -eq '') { continue }
    $values[$key] = $value
}

if ($BaseUrl -eq '' -and $values.ContainsKey('BASALT_BASE_URL')) { $BaseUrl = $values['BASALT_BASE_URL'] }
if ([string]::IsNullOrWhiteSpace($BaseUrl)) { $BaseUrl = 'http://127.0.0.1:8080' }
if ([string]::IsNullOrWhiteSpace($AdminToken) -and $values.ContainsKey('BASALT_ADMIN_TOKEN')) {
    $AdminToken = $values['BASALT_ADMIN_TOKEN']
}
if ([string]::IsNullOrWhiteSpace($AdminToken)) {
    throw '缺少 BASALT_ADMIN_TOKEN：请在进程环境中注入，或写进仓库外的模板文件；不要把 token 贴进对话。'
}

$apiKey = $values['DEEPSEEK_API_KEY']
$webhook = $values['FEISHU_WEBHOOK_URL']
if ([string]::IsNullOrWhiteSpace($apiKey)) { throw '模板缺少 DEEPSEEK_API_KEY' }
if ([string]::IsNullOrWhiteSpace($webhook)) { throw '模板缺少 FEISHU_WEBHOOK_URL' }
if (-not $webhook.StartsWith('https://', [StringComparison]::OrdinalIgnoreCase)) {
    throw 'FEISHU_WEBHOOK_URL 必须是 https 地址'
}
if ($apiKey -match '\s') { throw 'DEEPSEEK_API_KEY 含空白字符，模板可能写坏了' }
if ($webhook -match '\s') { throw 'FEISHU_WEBHOOK_URL 含空白字符，模板可能写坏了' }

$script:token = $AdminToken
$script:baseUrl = $BaseUrl.TrimEnd('/')
$apiKeyLast4 = Get-Last4 $apiKey
$conflict = $false

Write-Step "初始化导入目标：$($script:baseUrl)（凭据值不显示）"

# --- 1. 模型连接 deepseek --------------------------------------------------

$modelName = 'deepseek'
$modelPath = "/model-connections/$modelName"
$current = $null
$get = Invoke-Admin -Method 'GET' -Path $modelPath
if ($get.ok) {
    $current = $get.data.data
} elseif ($get.status -ne 404) {
    throw "读取模型连接失败：HTTP $($get.status) $($get.detail)"
}

$desiredBase = 'https://api.deepseek.com'
if ($values.ContainsKey('DEEPSEEK_BASE_URL')) { $desiredBase = $values['DEEPSEEK_BASE_URL'] }
$desiredModel = $values['DEEPSEEK_DEFAULT_MODEL']
if ([string]::IsNullOrWhiteSpace($desiredModel) -and $null -ne $current) {
    $desiredModel = $current.default_model
}
if ([string]::IsNullOrWhiteSpace($desiredModel)) { $desiredModel = 'deepseek-chat' }

$sendKey = $true
if ($null -eq $current) {
    $body = @{
        name           = $modelName
        base_url       = $desiredBase
        default_model  = $desiredModel
        api_key        = $apiKey
        credential_mode = 'managed'
    }
    $res = Invoke-Admin -Method 'POST' -Path '/model-connections' -Body $body
    if (-not $res.ok) { throw "创建模型连接失败：HTTP $($res.status) $($res.detail)" }
    Write-Step "[deepseek] 已创建：credential_mode=managed，default_model=$desiredModel，base_url=$desiredBase"
} else {
    $managedActive = ($current.credential_mode -eq 'managed' -and $current.credential_state -eq 'active')
    if ($managedActive -and -not $Rotate) {
        if ($apiKeyLast4 -ne '' -and $current.last4 -eq $apiKeyLast4) {
            $sendKey = $false
        } else {
            $conflict = $true
            $sendKey = $false
            Write-Warning "[deepseek] 目标已是托管凭据且尾号与模板不同（现有 $(($current.last4))，模板 $(($apiKeyLast4))）；未覆盖，如需轮换请加 -Rotate"
        }
    } elseif ($managedActive -and $Rotate) {
        $sendKey = $true
    } else {
        # env / 未激活 → 一次性切换到托管
        $sendKey = $true
    }

    $needsUpdate = $sendKey -or ($current.base_url -ne $desiredBase) -or ($current.default_model -ne $desiredModel)
    if (-not $needsUpdate) {
        Write-Step "[deepseek] 已是最新：credential_mode=$($current.credential_mode)，last4=$($current.last4)，base_url=$($current.base_url)"
    } else {
        $body = @{
            name          = $modelName
            base_url      = $desiredBase
            default_model = $desiredModel
        }
        if ($sendKey) {
            $body.api_key = $apiKey
            $body.credential_mode = 'managed'
        }
        $headers = @{ 'If-Match' = [string]$current.etag }
        $res = Invoke-Admin -Method 'PUT' -Path $modelPath -Body $body -ExtraHeaders $headers
        if (-not $res.ok) { throw "更新模型连接失败：HTTP $($res.status) $($res.detail)" }
        $updated = $res.data.data
        Write-Step "[deepseek] 已更新：credential_mode=$($updated.credential_mode)，last4=$($updated.last4)，availability=$($updated.availability)，base_url=$($updated.base_url)"
    }
}

# --- 2. 通知渠道 feishu-ops ------------------------------------------------

$channelName = 'feishu-ops'
$channelPath = "/notify-channels/$channelName"
$channel = $null
# 通知渠道没有单条 GET（契约只提供列表 + 写端点）；按名字在列表里找，找不到才创建。
$get = Invoke-Admin -Method 'GET' -Path '/notify-channels'
if (-not $get.ok) {
    throw "读取通知渠道列表失败：HTTP $($get.status) $($get.detail)"
}
foreach ($item in @($get.data.data)) {
    if ($null -ne $item -and [string]$item.name -eq $channelName) { $channel = $item; break }
}

if ($null -eq $channel) {
    $body = @{
        name            = $channelName
        type            = 'feishu'
        url             = $webhook
        credential_mode = 'managed'
    }
    $res = Invoke-Admin -Method 'POST' -Path '/notify-channels' -Body $body
    if (-not $res.ok) { throw "创建通知渠道失败：HTTP $($res.status) $($res.detail)" }
    Write-Step "[feishu-ops] 已创建：credential_mode=managed"
} else {
    $managedConfigured = ($channel.credential_mode -eq 'managed' -and $channel.address_status -eq 'managed_configured')
    if ($managedConfigured -and -not $Rotate) {
        Write-Step "[feishu-ops] 已是最新：address_status=$($channel.address_status)，url_host=$($channel.url_host)（地址值不显示；如需替换请加 -Rotate）"
    } else {
        $body = @{
            name            = $channelName
            type            = $channel.type
            url             = $webhook
            credential_mode = 'managed'
        }
        $res = Invoke-Admin -Method 'PUT' -Path $channelPath -Body $body
        if (-not $res.ok) { throw "更新通知渠道失败：HTTP $($res.status) $($res.detail)" }
        $updated = $res.data.data
        Write-Step "[feishu-ops] 已更新：credential_mode=$($updated.credential_mode)，address_status=$($updated.address_status)，url_host=$($updated.url_host)"
    }
}

# --- 3. 可选：真实连通性测试 ----------------------------------------------

$testFailed = $false
if ($Test) {
    $modelTest = Invoke-Admin -Method 'POST' -Path '/model-connections/deepseek/test'
    if (-not $modelTest.ok) {
        $testFailed = $true
        Write-Warning "[deepseek] 测试请求失败：HTTP $($modelTest.status) $($modelTest.detail)"
    } else {
        $r = $modelTest.data.data
        Write-Step "[deepseek] 测试：ok=$($r.ok)，model=$($r.model)，latency_ms=$($r.latency_ms)，error_code=$($r.error_code)，error_message=$($r.error_message)"
        if (-not $r.ok) { $testFailed = $true }
    }

    $notifyTest = Invoke-Admin -Method 'POST' -Path '/notify-channels/feishu-ops/test'
    if (-not $notifyTest.ok) {
        $testFailed = $true
        Write-Warning "[feishu-ops] 测试请求失败：HTTP $($notifyTest.status) $($notifyTest.detail)"
    } else {
        $r = $notifyTest.data.data
        $hint = ''
        if ($r.status -eq 'unknown') { $hint = '（可能已送达，未确认；不自动重发）' }
        Write-Step "[feishu-ops] 测试：status=$($r.status)，ok=$($r.ok)，latency_ms=$($r.latency_ms)，detail=$($r.detail)$hint"
        if (-not $r.ok) { $testFailed = $true }
    }
}

if ($conflict) {
    Write-Warning '存在未覆盖的凭据冲突（见上）；如需轮换请显式加 -Rotate 重跑。'
    exit 3
}
if ($testFailed) {
    Write-Warning '导入已完成，但真实连通性测试未全部通过；请按上面的脱敏原因处理后重试。'
    exit 1
}
Write-Step '初始化导入完成。运行期只依赖主密钥文件与管理认证，不需要再输入 API Key / Webhook。'
exit 0
