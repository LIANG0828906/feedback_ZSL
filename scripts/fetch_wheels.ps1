param(
    [string]$Wheelhouse = "wheelhouse"
)

$ErrorActionPreference = "Stop"
Write-Host "Downloading pinned Windows cp311 wheels into $Wheelhouse"
& py -3.11 -m pip download `
    --only-binary=:all: `
    -r requirements-windows.lock `
    -d $Wheelhouse

if ($LASTEXITCODE -ne 0) {
    throw "wheel download failed"
}

Write-Host "Wheelhouse ready. Copy this directory with the project for offline install."

