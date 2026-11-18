param(
    [int]$Count = 20000,
    [ValidateSet("simulator", "kafka")]
    [string]$Backend = "simulator",
    [string]$Bootstrap = "127.0.0.1:9092",
    [ValidateSet("none", "full")]
    [string]$Faults = "full"
)

$ErrorActionPreference = "Stop"
$workspace = Resolve-Path (Join-Path $PSScriptRoot "..")
Set-Location $workspace
python -m tools.acceptance `
    --backend $Backend `
    --bootstrap $Bootstrap `
    --count $Count `
    --faults $Faults
exit $LASTEXITCODE

