[CmdletBinding()]
param(
    [switch]$SkipBuild,
    [switch]$Fault
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$RunDir = Join-Path $Root ".run\acceptance"
$LogDir = Join-Path $RunDir "logs"
$DataDir = Join-Path $Root "data"
$ArtifactDir = Join-Path $Root "artifacts"
$Database = Join-Path $DataDir "acceptance.db"
$EtcdData = Join-Path $RunDir "etcd-data"
$CoordinatorExe = Join-Path $Root "build-win\coordinator.exe"
$WorkerExe = Join-Path $Root "build-win\worker.exe"
$TaskgenExe = Join-Path $Root "build-win\taskgen.exe"
$ProbeExe = Join-Path $Root "build-win\probe.exe"
$VerifyExe = Join-Path $Root "build-win\verify.exe"
$EtcdExe = Join-Path $Root "etcd\etcd.exe"
$tracked = New-Object System.Collections.ArrayList

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
    [void]$tracked.Add($process)
    Start-Sleep -Milliseconds 150
    if ($process.HasExited) {
        throw "$Name exited during startup with code $($process.ExitCode); see $err"
    }
    return $process
}

function Test-Alive {
    param($Process)
    return ($null -ne $Process -and -not $Process.HasExited)
}

function Write-Utf8NoBom {
    param([string]$Path, [string]$Content)
    $encoding = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($Path, $Content, $encoding)
}

New-Item -ItemType Directory -Force -Path $RunDir, $LogDir, $DataDir, $ArtifactDir | Out-Null
if (Test-Path (Join-Path $Root ".run\pids.json")) {
    & (Join-Path $PSScriptRoot "stop.ps1")
}
if (!$SkipBuild) {
    & (Join-Path $PSScriptRoot "build.ps1")
}
if (!(Test-Path $EtcdExe)) { throw "missing etcd executable: $EtcdExe" }
foreach ($required in @($CoordinatorExe, $WorkerExe, $TaskgenExe, $ProbeExe, $VerifyExe)) {
    if (!(Test-Path $required)) { throw "missing binary $required; run scripts\build.ps1" }
}

Get-ChildItem -Path $DataDir -Filter "acceptance.db*" -File -ErrorAction SilentlyContinue |
    Remove-Item -Force
$reuseEtcd = $false
try {
    Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:2379/health" -TimeoutSec 1 | Out-Null
    $reuseEtcd = $true
}
catch {
    if (Test-Path $EtcdData) {
        Remove-Item -Recurse -Force $EtcdData
    }
    New-Item -ItemType Directory -Force -Path $EtcdData | Out-Null
}
if ($reuseEtcd) {
    Write-Host "Reusing etcd already listening on 127.0.0.1:2379"
}

$scenarioSeconds = 0.0
$peakWorkingSet = 0L
$faultTriggered = $false
try {
    $env:GOTOOLCHAIN = "local"
    $env:GOMAXPROCS = "1"
    & $TaskgenExe -out $DataDir | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "task generation failed" }

    if (-not $reuseEtcd) {
        $etcdArgs = "--name acceptance --data-dir `"$EtcdData`" " +
            "--listen-client-urls http://127.0.0.1:2379 " +
            "--advertise-client-urls http://127.0.0.1:2379 " +
            "--listen-peer-urls http://127.0.0.1:2380 " +
            "--initial-advertise-peer-urls http://127.0.0.1:2380 " +
            "--initial-cluster acceptance=http://127.0.0.1:2380 --initial-cluster-state new"
        [void](Start-Logged -FilePath $EtcdExe -Arguments $etcdArgs -Name "etcd")
    }
    Wait-Http -Uri "http://127.0.0.1:2379/health"

    $mainConfig = Get-Content -Raw -Encoding UTF8 (Join-Path $Root "configs\coordinator.json") |
        ConvertFrom-Json
    $probeConfig = $mainConfig.PSObject.Copy()
    $probeConfig.key_prefix = "/settlement/probe-" + [Guid]::NewGuid().ToString("N")
    $probeConfigPath = Join-Path $RunDir "coordinator-probe.json"
    Write-Utf8NoBom -Path $probeConfigPath -Content ($probeConfig | ConvertTo-Json -Depth 6)
    $probeCoordinator = Start-Logged -FilePath $CoordinatorExe `
        -Arguments "-config `"$probeConfigPath`"" -Name "coordinator-probe"
    Wait-Http -Uri "http://127.0.0.1:19090/healthz"
    & $ProbeExe -grpc "127.0.0.1:17070" -out (Join-Path $ArtifactDir "lock-probe-report.json") | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "lock probe failed" }
    Stop-Process -Id $probeCoordinator.Id -Force
    Start-Sleep -Milliseconds 300

    $scenarioConfig = $mainConfig.PSObject.Copy()
    $scenarioConfig.key_prefix = "/settlement/acceptance-" + [Guid]::NewGuid().ToString("N")
    $scenarioConfigPath = Join-Path $RunDir "coordinator-scenario.json"
    Write-Utf8NoBom -Path $scenarioConfigPath -Content ($scenarioConfig | ConvertTo-Json -Depth 6)
    $coordinator = Start-Logged -FilePath $CoordinatorExe `
        -Arguments "-config `"$scenarioConfigPath`"" -Name "coordinator"
    Wait-Http -Uri "http://127.0.0.1:19090/healthz"

    $workMS = if ($Fault) { 1200 } else { 250 }
    $workers = @{}
    foreach ($worker in @("A", "B", "C")) {
        $cfg = @{
            worker_id = $worker
            grpc_address = "127.0.0.1:17070"
            database_path = $Database
            tasks_file = (Join-Path $DataDir "tasks.jsonl")
            seed = 28120261119
            batch_count = 20
            tasks_per_batch = 100
            lease_ttl_ms = 6000
            wait_timeout_ms = 1500
            renew_interval_ms = 2000
            batch_work_ms = $workMS
            retry_delay_ms = 150
            max_runtime_ms = 110000
        }
        $cfgPath = Join-Path $RunDir ("worker-" + $worker + ".json")
        Write-Utf8NoBom -Path $cfgPath -Content ($cfg | ConvertTo-Json -Depth 4)
        $workers[$worker] = Start-Logged -FilePath $WorkerExe `
            -Arguments "-config `"$cfgPath`"" -Name ("worker-" + $worker)
    }

    $scenarioStarted = Get-Date
    if ($Fault) {
        $faultDeadline = (Get-Date).AddSeconds(30)
        while ((Get-Date) -lt $faultDeadline -and !$faultTriggered) {
            if (-not (Test-Alive -Process $workers["B"])) {
                throw "worker B exited before a fault could be injected"
            }
            $statuses = Invoke-RestMethod -Uri "http://127.0.0.1:19090/v1/status" -TimeoutSec 3
            $target = @($statuses.statuses | Where-Object {
                $_.holder_id -eq "B" -and $_.held -and -not $_.committed
            } | Select-Object -First 1)
            if ($target.Count -gt 0) {
                Stop-Process -Id $workers["B"].Id -Force
                $faultTriggered = $true
                Write-Host ("Injected fault: killed B while holding " + $target[0].batch_id)
            }
            else {
                Start-Sleep -Milliseconds 50
            }
        }
        if (!$faultTriggered) {
            throw "worker B never held an uncommitted batch; fault injection was not exercised"
        }
        Start-Sleep -Milliseconds 7500
        $workers["B"] = Start-Logged -FilePath $WorkerExe `
            -Arguments "-config `"$(Join-Path $RunDir 'worker-B.json')`"" -Name "worker-B-restarted"
    }

    $deadline = (Get-Date).AddSeconds(110)
    while ((Get-Date) -lt $deadline) {
        $alive = @($workers.Values | Where-Object { Test-Alive -Process $_ })
        $workingSet = 0L
        foreach ($process in $alive) {
            try {
                $workingSet += [int64](Get-Process -Id $process.Id).WorkingSet64
            }
            catch {}
        }
        if ($workingSet -gt $peakWorkingSet) {
            $peakWorkingSet = $workingSet
        }
        if ($alive.Count -eq 0) {
            break
        }
        Start-Sleep -Milliseconds 100
    }
    $scenarioSeconds = ((Get-Date) - $scenarioStarted).TotalSeconds
    $stillAlive = @($workers.Values | Where-Object { Test-Alive -Process $_ })
    if ($stillAlive.Count -gt 0) {
        throw "workers did not finish within 110 seconds"
    }

    & $VerifyExe -grpc "127.0.0.1:17070" -db $Database `
        -out (Join-Path $ArtifactDir "acceptance-report.json")
    if ($LASTEXITCODE -ne 0) { throw "final verification failed" }
    $report = Get-Content -Raw -Encoding UTF8 (Join-Path $ArtifactDir "acceptance-report.json") |
        ConvertFrom-Json
    if (!$report.ok) { throw "verification report is not OK" }
    if ($report.committed_batches -ne 20 -or $report.persisted_batches -ne 20) {
        throw "expected 20 coordinator and SQLite batches"
    }
    if ($report.settlement_task_rows -ne 2000) {
        throw "expected 2000 settlement task rows"
    }
    if ($Fault -and $report.lease_expired_releases -lt 1) {
        throw "fault drill did not record a lease_expired release event"
    }
    if ($scenarioSeconds -ge 120) {
        throw "scenario exceeded the 120 second limit"
    }

    $statusResponse = Invoke-RestMethod -Uri "http://127.0.0.1:19090/v1/status" -TimeoutSec 5
    if (@($statusResponse.statuses).Count -ne 20) {
        throw "status endpoint did not return exactly 20 batches"
    }
    Write-Utf8NoBom -Path (Join-Path $ArtifactDir "acceptance-status.json") `
        -Content ($statusResponse | ConvertTo-Json -Depth 8)
    $metrics = (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:19090/metrics" -TimeoutSec 5).Content
    Write-Utf8NoBom -Path (Join-Path $ArtifactDir "acceptance-metrics.txt") -Content $metrics
    $requiredMetrics = @(
        'settlement_lock_held{batch="batch-001"}',
        'settlement_lock_owner_info{batch="batch-001",holder=',
        'settlement_lock_lease_remaining_seconds{batch="batch-001"}',
        'settlement_lock_acquire_requests_total{batch="batch-001"}',
        'settlement_lock_renew_requests_total{batch="batch-001"}',
        'settlement_lock_release_events_total{batch="batch-001",reason=',
        'settlement_lock_wait_timeouts_total{batch="batch-001"}'
    )
    foreach ($needle in $requiredMetrics) {
        if (!$metrics.Contains($needle)) {
            throw "missing Prometheus metric: $needle"
        }
    }

    $summary = [ordered]@{
        ok = $true
        fault_enabled = [bool]$Fault
        fault_triggered = $faultTriggered
        scenario_seconds = [math]::Round($scenarioSeconds, 3)
        peak_worker_working_set_mib = [math]::Round($peakWorkingSet / 1MB, 1)
        committed_batches = $report.committed_batches
        persisted_batches = $report.persisted_batches
        settlement_task_rows = $report.settlement_task_rows
        release_events = $report.release_events
        lease_expired_releases = $report.lease_expired_releases
    }
    $summaryJson = $summary | ConvertTo-Json -Depth 4
    Write-Utf8NoBom -Path (Join-Path $ArtifactDir "acceptance-summary.json") -Content $summaryJson
    $summary | ConvertTo-Json -Depth 4
}
finally {
    foreach ($process in $tracked) {
        try {
            if (-not $process.HasExited) {
                Stop-Process -Id $process.Id -Force
            }
        }
        catch {}
    }
}
