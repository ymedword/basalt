param([switch]$AllowAuthorizedRealModel, [ValidateSet(90, 115, 125)][int]$AuthorizedChatLimit = 90)
$ErrorActionPreference = 'Stop'
if (-not $AllowAuthorizedRealModel) { throw 'Explicit authorized real-model opt-in is required.' }
$repo = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '../../..')).Path
Set-Location -LiteralPath $repo
$python = & (Join-Path $repo 'dev-env/tools/python/Scripts/python.exe') -c 'import sys; print(sys._base_executable)'
if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $python)) { throw 'Python interpreter unavailable.' }
$ready = Join-Path $repo '.loop/us19-budget-gateway-ready.json'
$journal = Join-Path $repo '.loop/us19-real-model-budget.json'
if (Test-Path -LiteralPath ($journal + '.lock')) { throw 'Existing gateway lock: reconcile the original process and usage; do not reset the budget.' }
if (Test-Path -LiteralPath $ready) { throw 'Existing readiness record: inspect the previous run before starting another.' }
if (Test-Path -LiteralPath '.loop/us19-business-release') { throw 'This fixture has already been released; retain its evidence.' }
. ./dev-env/scripts/Enter-DevEnv.ps1 -WithBailian
$bytes = New-Object byte[] 32
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
$rng.Dispose()
$env:BASALT_US19_GATEWAY_TOKEN = [Convert]::ToBase64String($bytes)
$env:PYTHONIOENCODING = 'utf-8'
$gateway = $null
try {
    $gateway = Start-Process -FilePath $python -ArgumentList @('examples/workflows/bid-readiness/budget_gateway.py', '--allow-real', '--journal', '.loop/us19-real-model-budget.json', '--ready', '.loop/us19-budget-gateway-ready.json', '--authorized-chat-limit', $AuthorizedChatLimit) -WindowStyle Hidden -PassThru -RedirectStandardOutput '.loop/us19-budget-gateway.stdout.log' -RedirectStandardError '.loop/us19-budget-gateway.stderr.log'
} finally {
    # The application/test processes must never inherit the real upstream key.
    Remove-Item Env:DASHSCOPE_API_KEY -ErrorAction SilentlyContinue
    Remove-Item Env:BASALT_DEV_EMBEDDING_BASE_URL -ErrorAction SilentlyContinue
}
try {
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    while (-not (Test-Path -LiteralPath $ready)) {
        if ($gateway.HasExited -or [DateTime]::UtcNow -gt $deadline) { throw 'Gateway failed to become ready; inspect its safe log.' }
        Start-Sleep -Milliseconds 100
    }
    $metadata = Get-Content -LiteralPath $ready -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($metadata.pid -ne $gateway.Id) { throw 'Gateway readiness process mismatch.' }
    $env:BASALT_US19_DEMO_GATEWAY = $metadata.base_url
    $env:BASALT_US19_REPO = $repo
    $env:BASALT_E2E_BINARY = Join-Path $repo 'bin/us19-basalt.exe'
    Remove-Item Env:BASALT_E2E_ARTIFACT -ErrorAction SilentlyContinue
    $overlay = @{ Replace = @{} }
    $overlay.Replace[(Join-Path $repo 'internal/e2e/us19_business_review_overlay_test.go')] = Join-Path $PSScriptRoot 'serve_fixture_test.go.txt'
    [IO.File]::WriteAllText((Join-Path $repo '.loop/us19-business-overlay.json'), ($overlay | ConvertTo-Json -Depth 4), (New-Object Text.UTF8Encoding $false))
    $ErrorActionPreference = 'Continue'
    go test -overlay .loop/us19-business-overlay.json -tags=e2e -run '^TestFlow_BusinessReviewFixture$' -count=1 -timeout=25h -v ./internal/e2e *> .loop/us19-business-fixture.log
    $result = $LASTEXITCODE
} finally {
    if ($gateway -and -not $gateway.HasExited) { Stop-Process -Id $gateway.Id }
    Remove-Item Env:BASALT_US19_GATEWAY_TOKEN -ErrorAction SilentlyContinue
}
exit $result
