[CmdletBinding()]
param(
    [switch]$KeepEtcd
)

$ErrorActionPreference = "SilentlyContinue"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$PidFile = Join-Path $Root ".run\pids.json"
if (!(Test-Path $PidFile)) {
    exit 0
}
$state = Get-Content -Raw -Encoding UTF8 $PidFile | ConvertFrom-Json
foreach ($workerPid in @($state.workers.PSObject.Properties.Value)) {
    if ($workerPid) {
        Stop-Process -Id ([int]$workerPid) -Force
    }
}
if ($state.coordinator) {
    Stop-Process -Id ([int]$state.coordinator) -Force
}
if (!$KeepEtcd -and $state.etcd) {
    Stop-Process -Id ([int]$state.etcd) -Force
}
Remove-Item -Force $PidFile
