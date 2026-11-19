$env:PYTHONPATH = (Join-Path $PSScriptRoot "..\src")
if (-not $env:SLOTGUARD_PRIMARY_DSN) {
    $env:SLOTGUARD_PRIMARY_DSN = "host=127.0.0.1 port=55432 user=postgres dbname=postgres"
}
if (-not $env:SLOTGUARD_REPLICA_DSN) {
    $env:SLOTGUARD_REPLICA_DSN = "host=127.0.0.1 port=55433 user=postgres dbname=postgres"
}
if (-not $env:SLOTGUARD_REDIS_URL) {
    $env:SLOTGUARD_REDIS_URL = "redis://127.0.0.1:6379/0"
}
