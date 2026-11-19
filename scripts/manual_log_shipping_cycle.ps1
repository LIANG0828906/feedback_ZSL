param(
    [string]$PrimaryInstance = "localhost,1433",
    [string]$ReplicaInstance = "localhost,1434",
    [string]$PrimaryDatabase = "OrderMain",
    [string]$ReplicaDatabase = "OrderReplica",
    [string]$ShareDirectory = "C:\ordersync\logship",
    [string]$SqlUser = "sa",
    [string]$Password = $env:MSSQL_SA_PASSWORD,
    [string]$SqlCmd = "sqlcmd"
)

$ErrorActionPreference = "Stop"
if (-not $Password) {
    throw "Set MSSQL_SA_PASSWORD or pass -Password."
}

New-Item -ItemType Directory -Path $ShareDirectory -Force | Out-Null
$appliedDirectory = Join-Path $ShareDirectory "applied"
New-Item -ItemType Directory -Path $appliedDirectory -Force | Out-Null
$undoFile = Join-Path $ShareDirectory "$ReplicaDatabase-undo.bak"
$stamp = Get-Date -Format "yyyyMMddHHmmssfff"
$logBackup = Join-Path $ShareDirectory "$PrimaryDatabase-$stamp.trn"

$previousPassword = $env:SQLCMDPASSWORD
$env:SQLCMDPASSWORD = $Password

function Invoke-SqlCmdChecked {
    param(
        [string]$Instance,
        [string]$Query
    )
    & $SqlCmd -S $Instance -U $SqlUser -C -b -r1 -Q $Query
    if ($LASTEXITCODE -ne 0) {
        throw "sqlcmd failed against $Instance with exit code $LASTEXITCODE"
    }
}

try {
    $escapedLogBackup = $logBackup.Replace("'", "''")
    Invoke-SqlCmdChecked -Instance $PrimaryInstance -Query @"
BACKUP LOG [$PrimaryDatabase]
TO DISK = N'$escapedLogBackup'
WITH INIT, CHECKSUM, COMPRESSION;
"@

    $pending = @(
        Get-ChildItem -LiteralPath $ShareDirectory -Filter "$PrimaryDatabase-*.trn" |
            Sort-Object Name
    )
    if ($pending.Count -eq 0) {
        throw "No transaction log backup was found in $ShareDirectory."
    }

    $escapedUndo = $undoFile.Replace("'", "''")
    foreach ($file in $pending) {
        $escapedSource = $file.FullName.Replace("'", "''")
        Invoke-SqlCmdChecked -Instance $ReplicaInstance -Query @"
RESTORE LOG [$ReplicaDatabase]
FROM DISK = N'$escapedSource'
WITH
    STANDBY = N'$escapedUndo',
    CHECKSUM,
    STATS = 10;
"@
        Move-Item -LiteralPath $file.FullName `
            -Destination (Join-Path $appliedDirectory $file.Name)
        Write-Output "log shipping cycle applied: $($file.Name)"
    }
} finally {
    $env:SQLCMDPASSWORD = $previousPassword
}

