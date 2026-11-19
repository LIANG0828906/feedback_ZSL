param(
    [switch]$Pause,
    [string]$PauseFile = (Join-Path $PSScriptRoot "..\artifacts\replication.pause")
)

$ErrorActionPreference = "Stop"
$resolved = [System.IO.Path]::GetFullPath($PauseFile)
$directory = Split-Path -Parent $resolved
New-Item -ItemType Directory -Path $directory -Force | Out-Null

if ($Pause) {
    Set-Content -Path $resolved -Value (Get-Date -Format "o") -Encoding ascii
    Write-Output "replication paused"
} else {
    Remove-Item -Path $resolved -Force -ErrorAction SilentlyContinue
    Write-Output "replication resumed"
}

