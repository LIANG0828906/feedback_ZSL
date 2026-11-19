param(
    [string]$PrimaryInstance = "localhost,1433",
    [string]$ReplicaInstance = "localhost,1434",
    [string]$PrimaryDatabase = "OrderMain",
    [string]$ReplicaDatabase = "OrderReplica",
    [string]$ShareDirectory = "C:\ordersync\logship",
    [string]$PauseFile = (Join-Path $PSScriptRoot "..\artifacts\replication.pause"),
    [int]$IntervalSeconds = 2,
    [string]$SqlUser = "sa",
    [string]$Password = $env:MSSQL_SA_PASSWORD
)

$ErrorActionPreference = "Stop"
if ($IntervalSeconds -lt 1) {
    throw "IntervalSeconds must be at least 1."
}

$cycleScript = Join-Path $PSScriptRoot "manual_log_shipping_cycle.ps1"
while ($true) {
    if (Test-Path -LiteralPath $PauseFile) {
        Start-Sleep -Seconds $IntervalSeconds
        continue
    }
    try {
        & $cycleScript `
            -PrimaryInstance $PrimaryInstance `
            -ReplicaInstance $ReplicaInstance `
            -PrimaryDatabase $PrimaryDatabase `
            -ReplicaDatabase $ReplicaDatabase `
            -ShareDirectory $ShareDirectory `
            -SqlUser $SqlUser `
            -Password $Password
    } catch {
        Write-Error $_
    }
    Start-Sleep -Seconds $IntervalSeconds
}

