param(
    [int]$Frames = 8,
    [string]$Workdir = "artifacts\\verification"
)

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
& $Python -m space_audio verify --workdir $Workdir --frames $Frames
if ($LASTEXITCODE -ne 0) {
    throw "Verification failed with exit code $LASTEXITCODE"
}
