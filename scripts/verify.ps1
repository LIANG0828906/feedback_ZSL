$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
if (-not $env:OG_CONNECTION_STRING -and -not $env:OG_SA_PASSWORD) {
    throw 'Set OG_SA_PASSWORD or OG_CONNECTION_STRING before running verification.'
}

$env:OG_DATABASE = if ($env:OG_DATABASE) { $env:OG_DATABASE } else { 'OrdersDb' }
$env:OG_MAX_CONNECTIONS = if ($env:OG_MAX_CONNECTIONS) { $env:OG_MAX_CONNECTIONS } else { '6' }
$env:OG_ARTIFACT_DIR = if ($env:OG_ARTIFACT_DIR) { $env:OG_ARTIFACT_DIR } else { Join-Path $root 'artifacts/run' }
$env:DOTNET_PROCESSOR_COUNT = '2'
$env:DOTNET_gcServer = '0'
$env:DOTNET_GCHeapHardLimit = '268435456'
New-Item -ItemType Directory -Force -Path $env:OG_ARTIFACT_DIR | Out-Null

dotnet build (Join-Path $root 'src/OrderGateway/OrderGateway.csproj') -c Release -m:2
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
dotnet run --project (Join-Path $root 'src/OrderGateway/OrderGateway.csproj') -c Release --no-build
exit $LASTEXITCODE
