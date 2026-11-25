[CmdletBinding()]
param(
    [string]$Addr = "127.0.0.1:18080",
    [switch]$NoServe
)

$ErrorActionPreference = "Stop"

go run ./cmd/ssw demo --addr $Addr
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

go run ./cmd/ssw verify --db data\checkpoints\resume.sqlite --truth artifacts\truth.json
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

if (-not $NoServe) {
    Write-Host "Starting dashboard at http://$Addr/"
    go run ./cmd/ssw serve --state artifacts\dashboard-state.json --addr $Addr
}
