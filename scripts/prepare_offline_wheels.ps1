param(
    [string]$WheelDir = "vendor/wheels"
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $WheelDir | Out-Null

python -m pip download `
    --only-binary=:all: `
    --dest $WheelDir `
    -r requirements.txt

Write-Host "Pinned wheels cached in $WheelDir"
Write-Host "Offline install:"
Write-Host "python -m pip install --no-index --find-links $WheelDir -r requirements.txt"

