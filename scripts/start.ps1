param(
    [string]$MinioBinary = "D:\tools\minio.exe",
    [string]$MinioData = "D:\s3data",
    [string]$DataRoot = "D:\s3gw",
    [string]$GatewayBinary = (Join-Path $PSScriptRoot "..\bin\s3gw.exe"),
    [string]$MinioAddress = "127.0.0.1:9000",
    [string]$Listen = "127.0.0.1:9100",
    [string]$BackendAccessKey = "minioadmin",
    [string]$BackendSecretKey = "minioadmin",
    [string]$ClientAccessKey = "gateway",
    [string]$ClientSecretKey = "gateway-secret",
    [string]$PartRetention = "336h",
    [switch]$SkipMinio
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $MinioData | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $DataRoot "data") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $DataRoot "tmp") | Out-Null

$minioProcess = $null
if (-not $SkipMinio) {
    if (-not (Test-Path $MinioBinary)) {
        throw "MinIO binary not found: $MinioBinary"
    }
    $env:MINIO_ROOT_USER = $BackendAccessKey
    $env:MINIO_ROOT_PASSWORD = $BackendSecretKey
    $minioProcess = Start-Process -FilePath $MinioBinary `
        -ArgumentList @("server", $MinioData, "--address", $MinioAddress) `
        -PassThru -WindowStyle Hidden

    $health = "http://$MinioAddress/minio/health/live"
    $ready = $false
    for ($i = 0; $i -lt 60; $i++) {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri $health -TimeoutSec 2
            if ($response.StatusCode -eq 200) {
                $ready = $true
                break
            }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    if (-not $ready) {
        Stop-Process -Id $minioProcess.Id -Force -ErrorAction SilentlyContinue
        throw "MinIO did not become ready at $health"
    }
}

try {
    & $GatewayBinary `
        -listen $Listen `
        -backend "http://$MinioAddress" `
        -data (Join-Path $DataRoot "data\gateway.db") `
        -temp-dir (Join-Path $DataRoot "tmp") `
        -backend-access-key $BackendAccessKey `
        -backend-secret-key $BackendSecretKey `
        -access-key $ClientAccessKey `
        -secret-key $ClientSecretKey `
        -part-retention $PartRetention
} finally {
    if ($null -ne $minioProcess -and -not $minioProcess.HasExited) {
        Stop-Process -Id $minioProcess.Id -Force -ErrorAction SilentlyContinue
    }
}
