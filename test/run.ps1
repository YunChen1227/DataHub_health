# DataHub_health fixed test-suite entrypoint (Windows / PowerShell).
#
# Flow: make result dir test_res/<date> -> build + start mock_health(:9116) +
# relay(:8080, live Aliyun PG+Redis) -> wait /healthz -> run test/cases/*.go in
# order -> aggregate REPORT.md -> stop services.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File .\test\run.ps1
#   powershell -ExecutionPolicy Bypass -File .\test\run.ps1 -ConfigFile config.local.mem.yaml
param(
    [string]$ConfigFile = "config.aliyun.e2e.yaml"
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

$date = Get-Date -Format "yyyy-MM-dd"
$resultDir = Join-Path $repo "test_res\$date"
New-Item -ItemType Directory -Force -Path $resultDir | Out-Null

Write-Host "== DataHub_health test suite =="
Write-Host "  repo      : $repo"
Write-Host "  config    : $ConfigFile"
Write-Host "  resultDir : $resultDir"

$env:CONFIG_FILE    = $ConfigFile
$env:RESULT_DIR     = $resultDir
$env:RELAY_BASE_URL = "http://localhost:8080"

$procs = New-Object System.Collections.ArrayList

function Stop-All {
    foreach ($p in $procs) {
        try { if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } } catch {}
    }
}

function Wait-Health([string]$url, [int]$tries = 40) {
    for ($i = 0; $i -lt $tries; $i++) {
        try {
            $r = Invoke-WebRequest -UseBasicParsing -Uri $url -TimeoutSec 3
            if ($r.StatusCode -eq 200) { return $true }
        } catch {}
        Start-Sleep -Milliseconds 500
    }
    return $false
}

$anyFail = $false
try {
    $healthExe = Join-Path $resultDir "mock_health.exe"
    $relayExe  = Join-Path $resultDir "relay.exe"
    Write-Host "building mocks + relay ..."
    go build -o $healthExe ./scripts/mock_health.go
    if ($LASTEXITCODE -ne 0) { throw "go build mock_health failed" }
    go build -o $relayExe ./cmd/relay
    if ($LASTEXITCODE -ne 0) { throw "go build relay failed" }

    # postgres 模式：在启动 relay 前重建各版本库 (datahub_*_db)。
    $cfgText = Get-Content -Raw -Path (Join-Path $repo $ConfigFile)
    if ($cfgText -match 'driver:\s*"?postgres"?') {
        Write-Host "postgres mode: recreating per-domain databases (with demo seed) ..."
        $env:SEED_DEMO = "1"   # e2e 需要各路由的 demo license；生产建库不要设置
        go run ./scripts/recreate_databases.go
        if ($LASTEXITCODE -ne 0) { throw "recreate_databases failed" }
    } else {
        Write-Host "memory mode: skipping database recreate."
    }

    $health = Start-Process -FilePath $healthExe -WorkingDirectory $repo -PassThru -RedirectStandardOutput (Join-Path $resultDir "mock_health.log") -RedirectStandardError (Join-Path $resultDir "mock_health.err.log")
    [void]$procs.Add($health)

    $relay = Start-Process -FilePath $relayExe -WorkingDirectory $repo -PassThru -RedirectStandardOutput (Join-Path $resultDir "relay.log") -RedirectStandardError (Join-Path $resultDir "relay.err.log")
    [void]$procs.Add($relay)

    Write-Host "waiting for relay /healthz ..."
    if (-not (Wait-Health "http://localhost:8080/healthz")) {
        throw "relay /healthz not ready; see $resultDir\relay.err.log (PG/Redis connect or migration failure)"
    }
    Write-Host "relay is up."

    $cases = Get-ChildItem (Join-Path $repo "test\cases\*.go") | Sort-Object Name
    foreach ($c in $cases) {
        $name = [IO.Path]::GetFileNameWithoutExtension($c.Name)
        $log = Join-Path $resultDir "$name.log"
        Write-Host "---- running $name ----"
        go run $c.FullName 2>&1 | Tee-Object -FilePath $log
        if ($LASTEXITCODE -ne 0) { $anyFail = $true }
    }

    Write-Host "---- aggregating report ----"
    go run (Join-Path $repo "test\report.go") $resultDir
    if ($LASTEXITCODE -ne 0) { $anyFail = $true }
}
finally {
    Write-Host "---- stopping services ----"
    Stop-All
}

Write-Host ""
Write-Host "== done. report: $resultDir\REPORT.md =="
if ($anyFail) { exit 1 } else { exit 0 }
