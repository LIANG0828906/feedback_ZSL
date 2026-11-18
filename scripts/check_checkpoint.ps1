param(
    [Parameter(Mandatory=$true)]
    [string]$Path
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
& ".\.venv\Scripts\python.exe" -m mlp_ps.checkpoint verify --path $Path
exit $LASTEXITCODE
