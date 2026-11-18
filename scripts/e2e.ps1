$ErrorActionPreference = "Stop"

$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $Root

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go 1.22 or newer must be available on PATH"
}

$RunId = if ($env:RUN_ID) { $env:RUN_ID } else { (Get-Date).ToUniversalTime().ToString("yyyyMMddTHHmmssZ") }
$Port = if ($env:PORT) { $env:PORT } else { "18080" }
$Server = "http://127.0.0.1:$Port"
$DataDir = Join-Path $Root "data\e2e-$RunId"
$LogDir = Join-Path $Root "logs\e2e-$RunId"
$ReportDir = Join-Path $Root "reports\e2e-$RunId"
$TmpDir = Join-Path $Root "tmp\e2e-$RunId"

New-Item -ItemType Directory -Force -Path $DataDir, $LogDir, $ReportDir, $TmpDir, (Join-Path $Root "bin"), (Join-Path $Root ".cache\go-build"), (Join-Path $Root ".cache\gomod") | Out-Null
$env:GOCACHE = Join-Path $Root ".cache\go-build"
$env:GOMODCACHE = Join-Path $Root ".cache\gomod"
$env:GOTMPDIR = $TmpDir
$env:TMP = $TmpDir
$env:TEMP = $TmpDir

Write-Host "build"
go build -trimpath -o "bin\configd.exe" ./cmd/configd
go build -trimpath -o "bin\simclient.exe" ./cmd/simclient
go build -trimpath -o "bin\scenario.exe" ./cmd/scenario

$ServerProcess = $null
$SimProcess = $null
try {
    Write-Host "start config center"
    $env:GOMAXPROCS = "1"
    $env:GOMEMLIMIT = "768MiB"
    $ServerProcess = Start-Process -FilePath (Join-Path $Root "bin\configd.exe") -ArgumentList @(
        "-addr", "127.0.0.1:$Port",
        "-db", (Join-Path $DataDir "config.db"),
        "-log", (Join-Path $LogDir "configd.log"),
        "-seed", "300",
        "-seed-namespace", "payments",
        "-canary-percent", "10",
        "-observation-seconds", "8",
        "-observer-interval", "50ms",
        "-history-limit", "20"
    ) -RedirectStandardOutput (Join-Path $LogDir "configd.stdout.log") -RedirectStandardError (Join-Path $LogDir "configd.stderr.log") -PassThru -NoNewWindow

    $healthy = $false
    for ($i = 0; $i -lt 100; $i++) {
        try {
            Invoke-RestMethod -Uri "$Server/healthz" -TimeoutSec 2 | Out-Null
            $healthy = $true
            break
        } catch {
            Start-Sleep -Milliseconds 100
        }
        if ($ServerProcess.HasExited) {
            throw "config center exited before becoming healthy"
        }
    }
    if (-not $healthy) {
        throw "config center did not become healthy"
    }

    Write-Host "start 24 simulated clients"
    $env:GOMEMLIMIT = "384MiB"
    $SimProcess = Start-Process -FilePath (Join-Path $Root "bin\simclient.exe") -ArgumentList @(
        "-server", $Server,
        "-namespace", "payments",
        "-instances", "24",
        "-instance-prefix", "sim",
        "-poll", "2s",
        "-poll-count", "25",
        "-duration", "120s",
        "-seed", "263",
        "-fault-5xx-rate", "0.03",
        "-network-error-rate", "0.02",
        "-latency-probability", "0.30",
        "-latency-min", "200ms",
        "-latency-max", "2s",
        "-read-workers", "6",
        "-read-interval", "200ms",
        "-state-db", (Join-Path $DataDir "simclient.db"),
        "-report", (Join-Path $ReportDir "simulation-report.json"),
        "-log", (Join-Path $LogDir "simclient.log")
    ) -RedirectStandardOutput (Join-Path $LogDir "simclient.stdout.log") -RedirectStandardError (Join-Path $LogDir "simclient.stderr.log") -PassThru -NoNewWindow

    Write-Host "drive releases and validate"
    $env:GOMEMLIMIT = "64MiB"
    & (Join-Path $Root "bin\scenario.exe") `
        -server $Server `
        -namespace payments `
        -instances 24 `
        -observation 8s `
        -client-wait 30s `
        -state-wait 30s `
        -report (Join-Path $ReportDir "simulation-report.json") `
        -report-wait 150s `
        -summary (Join-Path $ReportDir "scenario-summary.json")
    if ($LASTEXITCODE -ne 0) {
        throw "scenario failed with exit code $LASTEXITCODE"
    }

    if ($SimProcess -and -not $SimProcess.HasExited) {
        Wait-Process -Id $SimProcess.Id
    }
    $SimProcess = $null

    Write-Host ""
    Write-Host "artifacts"
    Write-Host "  report:  $(Join-Path $ReportDir 'simulation-report.json')"
    Write-Host "  summary: $(Join-Path $ReportDir 'scenario-summary.json')"
    Write-Host "  logs:    $LogDir"
    Write-Host "  data:    $DataDir"
} finally {
    if ($SimProcess -and -not $SimProcess.HasExited) {
        Stop-Process -Id $SimProcess.Id -Force
    }
    if ($ServerProcess -and -not $ServerProcess.HasExited) {
        Stop-Process -Id $ServerProcess.Id -Force
    }
}
