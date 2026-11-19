param(
    [switch]$Rollback
)

$ErrorActionPreference = "Stop"

$selected = if ($Rollback) {
    $env:ORDERSYNC_ROLLBACK_ROLES_COMMAND
} else {
    $env:ORDERSYNC_REVERSE_ROLES_COMMAND
}

if (-not $selected) {
    Write-Error @"
No physical role-reversal hook is configured.

Set ORDERSYNC_REVERSE_ROLES_COMMAND to a command that:
  1. uses ORDERSYNC_OLD_PRIMARY_* and ORDERSYNC_OLD_REPLICA_* environment variables;
  2. reverses log shipping or transactional replication;
  3. makes the old replica writable and the old primary read-only;
  4. exits non-zero unless the reversal is complete.

Set ORDERSYNC_ROLLBACK_ROLES_COMMAND to the matching compensation command.
The service closes its write gate and verifies the final watermark before running this hook.
"@
    exit 78
}

& ([scriptblock]::Create($selected))
exit $LASTEXITCODE

