param(
    [string]$DataDir = ".\archive-data",
    [int]$Port = 8080,
    [string]$HostAddress = "127.0.0.1"
)

$ErrorActionPreference = "Stop"
$python = Get-Command py -ErrorAction SilentlyContinue
if ($python) {
    & py -3.11 -m archive_service --data-dir $DataDir --host $HostAddress --port $Port
} else {
    & python -m archive_service --data-dir $DataDir --host $HostAddress --port $Port
}
