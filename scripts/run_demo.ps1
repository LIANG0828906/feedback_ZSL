param(
    [int]$Count = 20000,
    [int]$Port = 8090
)

$ErrorActionPreference = "Stop"
$workspace = Resolve-Path (Join-Path $PSScriptRoot "..")
$demoDir = Join-Path $workspace "data\demo"
$logDir = Join-Path $demoDir "logs"
New-Item -ItemType Directory -Force -Path $demoDir, $logDir | Out-Null
Set-Location $workspace

python -m ordergroup generate `
    --backend simulator `
    --partitions 12 `
    --count $Count `
    --seed 290 `
    --output-dir (Join-Path $demoDir "simulated") | Out-Null

$coordinatorOut = Join-Path $logDir "coordinator.out.log"
$coordinatorErr = Join-Path $logDir "coordinator.err.log"
$coordinator = Start-Process -FilePath "python" -ArgumentList @(
    "-m", "ordergroup", "coordinator",
    "--backend", "simulator",
    "--simulator-dir", (Join-Path $demoDir "simulated"),
    "--db", (Join-Path $demoDir "coordinator.sqlite3"),
    "--host", "127.0.0.1",
    "--port", "$Port",
    "--quiet"
) -WorkingDirectory $workspace -RedirectStandardOutput $coordinatorOut `
  -RedirectStandardError $coordinatorErr -WindowStyle Hidden -PassThru

Start-Sleep -Seconds 1
$workers = @()
for ($index = 1; $index -le 3; $index++) {
    $member = "worker-$index"
    $worker = Start-Process -FilePath "python" -ArgumentList @(
        "-m", "ordergroup", "worker",
        "--coordinator", "http://127.0.0.1:$Port",
        "--member-id", $member,
        "--session-timeout-ms", "5000",
        "--heartbeat-interval-ms", "500",
        "--batch-size", "100",
        "--consume-delay-ms", "1",
        "--processed-log", (Join-Path $logDir "$member.processed.jsonl")
    ) -WorkingDirectory $workspace `
      -RedirectStandardOutput (Join-Path $logDir "$member.out.log") `
      -RedirectStandardError (Join-Path $logDir "$member.err.log") `
      -WindowStyle Hidden -PassThru
    $workers += $worker
}
@($coordinator.Id) + @($workers.Id) | Set-Content (Join-Path $demoDir "pids.txt")

Write-Host "Coordinator: http://127.0.0.1:$Port"
Write-Host "Status:      http://127.0.0.1:$Port/v1/status"
Write-Host "Metrics:     http://127.0.0.1:$Port/metrics"
Write-Host "Lag check:   http://127.0.0.1:$Port/v1/lag"
Write-Host "Logs:        $logDir"
Write-Host "Press Ctrl+C to stop all processes."
try {
    Wait-Process -Id (@($coordinator.Id) + @($workers.Id)) -ErrorAction SilentlyContinue
} finally {
    @($coordinator.Id) + @($workers.Id) |
        ForEach-Object { Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue }
}

