$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
$Python = Join-Path $Root ".venv\Scripts\python.exe"
if (-not (Test-Path $Python)) {
    $Python = "python"
    $Deps = Join-Path $Root ".deps"
    if (Test-Path $Deps) {
        $env:PYTHONPATH = "$Deps;$Root"
    }
}
$env:QT_QPA_PLATFORM = "offscreen"
& $Python run_checks.py
if ($LASTEXITCODE -ne 0) {
    throw "Tests failed with exit code $LASTEXITCODE"
}
