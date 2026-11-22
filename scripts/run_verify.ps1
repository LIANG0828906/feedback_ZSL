param(
    [int]$Frames = 8,
    [string]$Workdir = "artifacts\\verification"
)

$ErrorActionPreference = "Stop"
$Python = Join-Path (Get-Location) ".venv\\Scripts\\python.exe"
if (-not (Test-Path $Python)) {
    $Python = "python"
}
& $Python -m space_audio verify --workdir $Workdir --frames $Frames
if ($LASTEXITCODE -ne 0) {
    throw "Verification failed with exit code $LASTEXITCODE"
}
