param(
    [string]$Config = "config.json",
    [switch]$Bootstrap,
    [switch]$SkipLoad,
    [double]$LoadDuration = 20
)

$ErrorActionPreference = "Stop"
$Python = Join-Path $PSScriptRoot "..\.venv\Scripts\python.exe"
if (-not (Test-Path $Python)) {
    throw "Create the Python 3.11 virtual environment and install requirements first."
}

$Arguments = @(
    (Join-Path $PSScriptRoot "run_acceptance.py"),
    "--config", $Config,
    "--report", (Join-Path $PSScriptRoot "..\artifacts\acceptance.json")
)
if (-not $Bootstrap) { $Arguments += "--skip-bootstrap" }
if ($SkipLoad) { $Arguments += "--skip-load" }
$Arguments += @("--load-duration", $LoadDuration)

& $Python @Arguments
exit $LASTEXITCODE

