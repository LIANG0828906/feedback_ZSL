param(
    [string]$Configuration = "Release"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    dotnet restore SnapshotGateway.sln
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    dotnet build SnapshotGateway.sln -c $Configuration --no-restore
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    dotnet run --project src/SnapshotGateway -c $Configuration --no-build -- `
        --self-test `
        --report artifacts/report.json
    exit $LASTEXITCODE
}
finally {
    Pop-Location
}
