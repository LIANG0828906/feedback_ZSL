param(
    [string]$PgBin = "C:\Program Files\PostgreSQL\16\bin",
    [string]$HostName = "127.0.0.1",
    [int]$OldPort = 55432,
    [int]$NewPort = 55433,
    [string]$User = "postgres",
    [string]$Password = "postgres",
    [switch]$KeepLedger
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
$env:PGPASSWORD = $Password
$Psql = Join-Path $PgBin "psql.exe"
$Createdb = Join-Path $PgBin "createdb.exe"

function Ensure-Database {
    param([int]$Port, [string]$Name)
    $found = & $Psql -h $HostName -p $Port -U $User -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$Name'"
    if ($LASTEXITCODE -ne 0) {
        throw "psql failed while checking $Name"
    }
    if ($found.Trim() -ne "1") {
        & $Createdb -h $HostName -p $Port -U $User $Name
        if ($LASTEXITCODE -ne 0) {
            throw "createdb failed for $Name"
        }
    }
}

Ensure-Database -Port $OldPort -Name "inventory_old"
Ensure-Database -Port $NewPort -Name "inventory_new"

& $Psql -v ON_ERROR_STOP=1 -h $HostName -p $OldPort -U $User -d inventory_old -f (Join-Path $Root "sql\old_schema.sql")
& $Psql -v ON_ERROR_STOP=1 -h $HostName -p $OldPort -U $User -d inventory_old -f (Join-Path $Root "sql\seed_old.sql")
& $Psql -v ON_ERROR_STOP=1 -h $HostName -p $NewPort -U $User -d inventory_new -f (Join-Path $Root "sql\new_schema.sql")

if (-not $KeepLedger) {
    $LedgerDir = Join-Path $Root "src\InventoryMigration.Api\data"
    if (Test-Path $LedgerDir) {
        Get-ChildItem -Path $LedgerDir -File | Remove-Item -Force
    }
}

Write-Host "inventory_old and inventory_new are ready."
