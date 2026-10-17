# Deterministic acceptance only; forwards exit 0/1/2 without launching an Agent.
$ErrorActionPreference = 'Stop'
$code = 2
$exe = $null
Push-Location (Join-Path $PSScriptRoot '..')
try {
    New-Item -ItemType Directory -Force -Path '.loop/bin' | Out-Null
    if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn' }
    $exe = Join-Path $PWD ('.loop/bin/acceptance.{0}.exe' -f $PID)
    go build -o $exe ./scripts/acceptance
    if ($LASTEXITCODE -eq 0) {
        & $exe @args
        $code = $LASTEXITCODE
    }
}
finally {
    if ($exe -and (Test-Path -LiteralPath $exe)) { Remove-Item -LiteralPath $exe }
    Pop-Location
}
exit $code
