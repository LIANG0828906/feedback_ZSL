param(
    [string]$Urls = "http://127.0.0.1:5080"
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
$env:ASPNETCORE_URLS = $Urls
dotnet run --project (Join-Path $Root "src\InventoryMigration.Api\InventoryMigration.Api.csproj")
