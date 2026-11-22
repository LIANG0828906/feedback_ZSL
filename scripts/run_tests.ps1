$ErrorActionPreference = "Stop"
$Python = Join-Path (Get-Location) ".venv\\Scripts\\python.exe"
if (-not (Test-Path $Python)) {
    $Python = "python"
}
$env:QT_QPA_PLATFORM = "offscreen"
& $Python run_checks.py
