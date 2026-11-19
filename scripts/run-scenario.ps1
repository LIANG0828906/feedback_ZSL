param(
    [string]$BaseUrl = "http://127.0.0.1:5080",
    [int]$Duration = 20,
    [int]$Rate = 300,
    [int]$Seed = 20261119,
    [string]$Report = "",
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Extra
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($Report)) {
    $Report = Join-Path $Root "artifacts\scenario-report.json"
}

$arguments = @(
    "run", "--project", (Join-Path $Root "src\InventoryMigration.LoadGen\InventoryMigration.LoadGen.csproj"),
    "--",
    "--base-url", $BaseUrl,
    "--duration", $Duration,
    "--rate", $Rate,
    "--seed", $Seed,
    "--report", $Report
)
if ($Extra) {
    $arguments += $Extra
}

& dotnet @arguments
