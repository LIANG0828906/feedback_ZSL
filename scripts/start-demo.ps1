param(
    [int]$Port = 18473,
    [string]$MergeWindow = "500ms",
    [switch]$KeepRunning,
    [switch]$KeepData
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
$BinDir = Join-Path $Root "bin"
$TmpDir = Join-Path $Root "tmp"
$Exe = Join-Path $BinDir "configcenter.exe"
$Go = if ($env:GO) { $env:GO } else { "go" }
$BaseURL = "http://127.0.0.1:$Port"
$ServerProcess = $null
$ClientProcesses = @()

Set-Location $Root
New-Item -ItemType Directory -Force -Path $BinDir, $TmpDir | Out-Null

if (-not $KeepData) {
    Get-ChildItem -Path $TmpDir -Filter "demo.db*" -ErrorAction SilentlyContinue | Remove-Item -Force
    Get-ChildItem -Path $TmpDir -Filter "*.log" -ErrorAction SilentlyContinue | Remove-Item -Force
}

$env:GOMAXPROCS = "2"
$env:GOMEMLIMIT = "900MiB"
$env:CGO_ENABLED = "0"

& $Go version | Out-Host
& $Go build -trimpath -o $Exe ./cmd/configcenter
if ($LASTEXITCODE -ne 0) {
    throw "go build failed with exit code $LASTEXITCODE"
}

try {
    $ServerOut = Join-Path $TmpDir "server.out.log"
    $ServerErr = Join-Path $TmpDir "server.err.log"
    $ServerArgs = @(
        "server",
        "--addr", "127.0.0.1:$Port",
        "--db", (Join-Path $TmpDir "demo.db"),
        "--merge-window", $MergeWindow,
        "--merge-max-wait", "2s"
    )
    $ServerProcess = Start-Process -FilePath $Exe -ArgumentList $ServerArgs `
        -RedirectStandardOutput $ServerOut -RedirectStandardError $ServerErr -PassThru

    $Deadline = (Get-Date).AddSeconds(20)
    $Healthy = $false
    while ((Get-Date) -lt $Deadline) {
        if ($ServerProcess.HasExited) {
            throw "configuration center exited before becoming healthy"
        }
        try {
            $Health = Invoke-RestMethod -Uri "$BaseURL/healthz" -TimeoutSec 1
            if ($Health.status -eq "ok") {
                $Healthy = $true
                break
            }
        } catch {
            Start-Sleep -Milliseconds 200
        }
    }
    if (-not $Healthy) {
        throw "configuration center did not become healthy within 20 seconds"
    }

    for ($i = 1; $i -le 3; $i++) {
        $ClientOut = Join-Path $TmpDir "client-$i.out.log"
        $ClientErr = Join-Path $TmpDir "client-$i.err.log"
        $ClientArgs = @(
            "client",
            "--base-url", $BaseURL,
            "--namespace", "orders",
            "--client-id", "client-$i",
            "--poll-timeout", "10s",
            "--apply-delay", "20ms"
        )
        $ClientProcess = Start-Process -FilePath $Exe -ArgumentList $ClientArgs `
            -RedirectStandardOutput $ClientOut -RedirectStandardError $ClientErr -PassThru
        $ClientProcesses += $ClientProcess
    }
    Write-Host "Started server PID $($ServerProcess.Id) and 3 client PIDs: $($ClientProcesses.Id -join ', ')"
    Start-Sleep -Milliseconds 1200

    & $Exe stress `
        --base-url $BaseURL `
        --namespace orders `
        --clients 3 `
        --merge-window $MergeWindow
    $StressExitCode = $LASTEXITCODE
    if ($StressExitCode -ne 0) {
        throw "stress verification failed with exit code $StressExitCode"
    }

    Write-Host "`n--- JSON status ---"
    $Status = Invoke-RestMethod -Uri "$BaseURL/v1/status" -TimeoutSec 5
    $Status | ConvertTo-Json -Depth 10 | Out-Host

    Write-Host "`n--- Prometheus sample ---"
    $Metrics = Invoke-WebRequest -UseBasicParsing -Uri "$BaseURL/metrics" -TimeoutSec 5
    $Metrics.Content -split "`n" |
        Where-Object { $_ -match "^config_(long_poll|merged|client_)" } |
        Out-Host
} finally {
    Write-Host "`n--- Client apply logs ---"
    for ($i = 1; $i -le 3; $i++) {
        $ClientOut = Join-Path $TmpDir "client-$i.out.log"
        if (Test-Path $ClientOut) {
            Write-Host "client-$i:"
            Get-Content $ClientOut | Where-Object { $_ -match "READY|APPLIED|STALE|ERROR" } | Out-Host
        }
    }
    Write-Host "`n--- Server log ---"
    $ServerOut = Join-Path $TmpDir "server.out.log"
    $ServerErr = Join-Path $TmpDir "server.err.log"
    if (Test-Path $ServerOut) { Get-Content $ServerOut | Out-Host }
    if (Test-Path $ServerErr) { Get-Content $ServerErr | Out-Host }

    if (-not $KeepRunning) {
        foreach ($ClientProcess in $ClientProcesses) {
            if ($null -ne $ClientProcess -and -not $ClientProcess.HasExited) {
                Stop-Process -Id $ClientProcess.Id -Force -ErrorAction SilentlyContinue
            }
        }
        if ($null -ne $ServerProcess -and -not $ServerProcess.HasExited) {
            Stop-Process -Id $ServerProcess.Id -Force -ErrorAction SilentlyContinue
        }
    } else {
        Write-Host "KeepRunning enabled. Stop PID $($ServerProcess.Id) and client PIDs $($ClientProcesses.Id -join ', ') manually."
    }
}
