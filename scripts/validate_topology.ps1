param(
    [string]$Config = "config\example.json",
    [string]$Python = ""
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot
if (-not $Python) {
    $candidate = Join-Path $repoRoot ".venv\Scripts\python.exe"
    $Python = if (Test-Path -LiteralPath $candidate) { $candidate } else { "python" }
}

if (-not $env:MSSQL_SA_PASSWORD) {
    throw "Set MSSQL_SA_PASSWORD before validating topology."
}

$env:PYTHONPATH = Join-Path $repoRoot "src"
& $Python -m ordersync validate --config $Config
exit $LASTEXITCODE

