param(
    [string]$PrimaryInstance = "localhost,1433",
    [string]$ReplicaInstance = "localhost,1434",
    [string]$PrimaryDatabase = "OrderMain",
    [string]$ReplicaDatabase = "OrderReplica",
    [string]$ShareDirectory = "C:\ordersync\logship",
    [string]$DataLogicalName = "OrderMain",
    [string]$LogLogicalName = "OrderMain_log",
    [string]$SqlUser = "sa",
    [string]$Password = $env:MSSQL_SA_PASSWORD,
    [string]$SqlCmd = "sqlcmd"
)

$ErrorActionPreference = "Stop"
if (-not $Password) {
    throw "Set MSSQL_SA_PASSWORD or pass -Password."
}

New-Item -ItemType Directory -Path $ShareDirectory -Force | Out-Null
$stamp = Get-Date -Format "yyyyMMddHHmmssfff"
$backupFile = Join-Path $ShareDirectory "$PrimaryDatabase-full-$stamp.bak"
$undoFile = Join-Path $ShareDirectory "$ReplicaDatabase-undo.bak"
$dataPath = Join-Path $ShareDirectory "$ReplicaDatabase.mdf"
$logPath = Join-Path $ShareDirectory "$ReplicaDatabase_log.ldf"

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
    $escapedBackup = $backupFile.Replace("'", "''")
    Invoke-SqlCmdChecked -Instance $PrimaryInstance -Query @"
BACKUP DATABASE [$PrimaryDatabase]
TO DISK = N'$escapedBackup'
WITH INIT, CHECKSUM, COMPRESSION;
"@

    $escapedUndo = $undoFile.Replace("'", "''")
    $escapedData = $dataPath.Replace("'", "''")
    $escapedLog = $logPath.Replace("'", "''")
    $dropAndRestore = @"
IF DB_ID(N'$ReplicaDatabase') IS NOT NULL
BEGIN
    ALTER DATABASE [$ReplicaDatabase] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    DROP DATABASE [$ReplicaDatabase];
END;
RESTORE DATABASE [$ReplicaDatabase]
FROM DISK = N'$escapedBackup'
WITH
    MOVE N'$DataLogicalName' TO N'$escapedData',
    MOVE N'$LogLogicalName' TO N'$escapedLog',
    STANDBY = N'$escapedUndo',
    CHECKSUM,
    STATS = 10;
"@
    Invoke-SqlCmdChecked -Instance $ReplicaInstance -Query $dropAndRestore
    Write-Output "log shipping initialized"
} finally {
    $env:SQLCMDPASSWORD = $previousPassword
}

