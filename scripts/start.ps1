[CmdletBinding()]
param(
    [switch]$ResetEtcd,
    [switch]$NoWorkers,
    [string]$CoordinatorConfig = "configs\coordinator.json"
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$RunDir = Join-Path $Root ".run"
$LogDir = Join-Path $RunDir "logs"
$PidFile = Join-Path $RunDir "pids.json"
$EtcdExe = Join-Path $Root "etcd\etcd.exe"
$CoordinatorExe = Join-Path $Root "build-win\coordinator.exe"
$WorkerExe = Join-Path $Root "build-win\worker.exe"
$EtcdData = Join-Path $RunDir "etcd-data"

function Wait-Http {
    param([string]$Uri, [int]$Seconds = 20)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        try {
            Invoke-WebRequest -UseBasicParsing -Uri $Uri -TimeoutSec 1 | Out-Null
            return
        }
        catch {
            Start-Sleep -Milliseconds 100
        }
    }
    throw "HTTP endpoint did not become ready: $Uri"
}

function Start-Logged {
    param(
        [string]$FilePath,
        [string]$Arguments,
        [string]$Name
    )
    $out = Join-Path $LogDir ($Name + ".log")
    $err = Join-Path $LogDir ($Name + ".err.log")
    $process = Start-Process -FilePath $FilePath -ArgumentList $Arguments `
        -WorkingDirectory $Root -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $out -RedirectStandardError $err
    Start-Sleep -Milliseconds 150
    if ($process.HasExited) {
        throw "$Name exited during startup with code $($process.ExitCode); see $err"
    }
    return $process
}

New-Item -ItemType Directory -Force -Path $RunDir, $LogDir | Out-Null
if (Test-Path $PidFile) {
    & (Join-Path $PSScriptRoot "stop.ps1")
}
if (!(Test-Path $EtcdExe)) { throw "missing etcd executable: $EtcdExe" }
if (!(Test-Path $CoordinatorExe)) { throw "missing coordinator executable; run scripts\build.ps1" }
if (!(Test-Path $WorkerExe)) { throw "missing worker executable; run scripts\build.ps1" }

if ($ResetEtcd) {
    if (Test-Path $EtcdData) {
        Remove-Item -Recurse -Force $EtcdData
    }
}
New-Item -ItemType Directory -Force -Path $EtcdData | Out-Null

$state = @{ etcd = $null; coordinator = $null; workers = @{} }
$etcdRunning = $false
try {
    Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:2379/health" -TimeoutSec 1 | Out-Null
    $etcdRunning = $true
    Write-Host "Reusing etcd already listening on 127.0.0.1:2379"
}
catch {
    $etcdArgs = "--name default --data-dir `"$EtcdData`" " +
        "--listen-client-urls http://127.0.0.1:2379 " +
        "--advertise-client-urls http://127.0.0.1:2379 " +
        "--listen-peer-urls http://127.0.0.1:2380 " +
        "--initial-advertise-peer-urls http://127.0.0.1:2380 " +
        "--initial-cluster default=http://127.0.0.1:2380 --initial-cluster-state new"
    $etcd = Start-Logged -FilePath $EtcdExe -Arguments $etcdArgs -Name "etcd"
    $state.etcd = $etcd.Id
}
Wait-Http -Uri "http://127.0.0.1:2379/health"

$env:GOMAXPROCS = "1"
$coordinatorPath = Join-Path $Root $CoordinatorConfig
$coordinator = Start-Logged -FilePath $CoordinatorExe `
    -Arguments "-config `"$coordinatorPath`"" -Name "coordinator"
$state.coordinator = $coordinator.Id
Wait-Http -Uri "http://127.0.0.1:19090/healthz"

if (-not $NoWorkers) {
    foreach ($worker in @("A", "B", "C")) {
        $workerConfig = Join-Path $Root ("configs\worker-" + $worker + ".json")
        $process = Start-Logged -FilePath $WorkerExe `
            -Arguments "-config `"$workerConfig`"" -Name ("worker-" + $worker)
        $state.workers[$worker] = $process.Id
    }
}
$state | ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 $PidFile
Write-Host "Started coordinator and workers. PID state: $PidFile"
