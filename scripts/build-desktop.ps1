$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

if ($env:CGO_ENABLED -ne "1") {
    $env:CGO_ENABLED = "1"
}

if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
    throw "gcc not found. Install MinGW-w64 (MSYS2 or TDM-GCC) and add it to PATH."
}
if (-not (Get-Command wails -ErrorAction SilentlyContinue)) {
    throw "Wails CLI not found. Run: go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2"
}

wails doctor
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

wails build -clean
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
