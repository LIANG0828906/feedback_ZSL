param(
    [int]$Port = 5188,
    [int]$MaxConnections = 8,
    [switch]$Reset
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $arguments = @(
        "run",
        "--project", "src/SnapshotGateway",
        "--",
        "--port", "$Port",
        "--max-connections", "$MaxConnections"
    )
    if ($Reset) {
        $arguments += "--reset"
    }

    dotnet @arguments
    exit $LASTEXITCODE
}
finally {
    Pop-Location
}
