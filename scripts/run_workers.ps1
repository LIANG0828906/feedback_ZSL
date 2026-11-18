param(
    [string]$Coordinator = "http://127.0.0.1:8090",
    [int]$SessionTimeoutMs = 10000,
    [int]$HeartbeatIntervalMs = 1000,
    [int]$BatchSize = 250
)

$ErrorActionPreference = "Stop"
$workspace = Resolve-Path (Join-Path $PSScriptRoot "..")
$logDir = Join-Path $workspace "data\logs"
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$processes = @()
for ($index = 1; $index -le 3; $index++) {
    $member = "worker-$index"
    $out = Join-Path $logDir "$member.out.log"
    $err = Join-Path $logDir "$member.err.log"
    $workerArgs = @(
        "-m", "ordergroup", "worker",
        "--coordinator", $Coordinator,
        "--member-id", $member,
        "--session-timeout-ms", "$SessionTimeoutMs",
        "--heartbeat-interval-ms", "$HeartbeatIntervalMs",
        "--batch-size", "$BatchSize"
    )
    $process = Start-Process -FilePath "python" -ArgumentList $workerArgs `
        -WorkingDirectory $workspace `
        -RedirectStandardOutput $out `
        -RedirectStandardError $err `
        -WindowStyle Hidden -PassThru
    $processes += $process
}
$processes | ForEach-Object { $_.Id } | Set-Content (Join-Path $logDir "workers.pid")
Write-Host "Started 3 workers. PIDs: $($processes.Id -join ', ')"

