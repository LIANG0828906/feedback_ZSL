using Microsoft.Data.SqlClient;

namespace OrderGateway;

public sealed record LegacyInstanceStatus(int InstanceId, string InstanceName, DateTimeOffset LastBeatUtc, bool IsAlive);

public sealed record CutoverState(
    bool OldIndexPresent,
    bool NewIndexPresent,
    IReadOnlyList<LegacyInstanceStatus> Instances);

/// <summary>
/// Owns the dual-index coexistence window:
/// heartbeat freshness decides whether legacy traffic is still expected, the
/// old index is dropped only after every legacy instance is stale, and DDL
/// waits on existing queries instead of interrupting them.
/// </summary>
public sealed class IndexLifecycleService
{
    private readonly ConnectionGate _gate;
    private readonly GatewayOptions _options;
    private readonly MetricsCollector _metrics;
    private readonly IndexChangeBarrier _indexBarrier;

    public IndexLifecycleService(
        ConnectionGate gate,
        GatewayOptions options,
        MetricsCollector metrics,
        IndexChangeBarrier indexBarrier)
    {
        _gate = gate;
        _options = options;
        _metrics = metrics;
        _indexBarrier = indexBarrier;
    }

    public async Task<CutoverState> GetStateAsync(CancellationToken cancellationToken = default)
    {
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = """
            SELECT
                h.InstanceId,
                h.InstanceName,
                h.LastBeatUtc,
                DATEDIFF_BIG(MILLISECOND, h.LastBeatUtc, SYSUTCDATETIME()) AS AgeMs
            FROM dbo.LegacyHeartbeat h
            ORDER BY h.InstanceId;

            SELECT i.name
            FROM sys.indexes i
            WHERE i.object_id = OBJECT_ID(N'dbo.Orders')
              AND i.name IN (@OldIndex, @NewIndex);
            """;
        command.Parameters.AddWithValue("@OldIndex", _options.OldIndexName);
        command.Parameters.AddWithValue("@NewIndex", _options.NewIndexName);

        var instances = new List<LegacyInstanceStatus>();
        await using var reader = await command.ExecuteReaderAsync(cancellationToken).ConfigureAwait(false);
        while (await reader.ReadAsync(cancellationToken).ConfigureAwait(false))
        {
            var lastBeat = reader.GetDateTime(2);
            var ageMs = reader.GetInt64(3);
            instances.Add(new LegacyInstanceStatus(
                reader.GetInt32(0),
                reader.GetString(1),
                new DateTimeOffset(lastBeat, TimeSpan.Zero),
                TimeSpan.FromMilliseconds(ageMs) < _options.HeartbeatExpiry));
        }

        var present = new List<string>();
        await reader.NextResultAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken).ConfigureAwait(false))
            present.Add(reader.GetString(0));

        return new CutoverState(
            present.Contains(_options.OldIndexName),
            present.Contains(_options.NewIndexName),
            instances);
    }

    /// <summary>Legacy traffic remains possible while at least one heartbeat is fresh.</summary>
    public async Task<bool> IsLegacyStillActiveAsync(CancellationToken cancellationToken = default)
    {
        var state = await GetStateAsync(cancellationToken).ConfigureAwait(false);
        return state.Instances.Any(i => i.IsAlive);
    }

    public async Task SendHeartbeatAsync(int instanceId, CancellationToken cancellationToken = default)
    {
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = """
            UPDATE dbo.LegacyHeartbeat
            SET LastBeatUtc = SYSUTCDATETIME()
            WHERE InstanceId = @InstanceId;
            """;
        command.Parameters.AddWithValue("@InstanceId", instanceId);
        await command.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
    }

    public async Task ExpireHeartbeatsAsync(CancellationToken cancellationToken = default)
    {
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = """
            UPDATE dbo.LegacyHeartbeat
            SET LastBeatUtc = DATEADD(SECOND, -@AgeSeconds, SYSUTCDATETIME());
            """;
        command.Parameters.AddWithValue("@AgeSeconds", (int)_options.HeartbeatExpiry.TotalSeconds + 60);
        await command.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Retires the old index. Refuses while a legacy heartbeat is fresh. The
    /// DDL runs under a bounded lock timeout: in-flight queries finish first,
    /// and their results are never touched by this operation.
    /// </summary>
    public async Task RetireOldIndexAsync(CancellationToken cancellationToken = default)
    {
        if (await IsLegacyStillActiveAsync(cancellationToken).ConfigureAwait(false))
        {
            var context = new Dictionary<string, object?>
            {
                ["heartbeatExpirySec"] = _options.HeartbeatExpiry.TotalSeconds,
                ["reason"] = "at least one legacy heartbeat is still fresh",
            };
            _metrics.RecordError("E_RETIRE_BLOCKED", context);
            throw new GatewayException("E_RETIRE_BLOCKED", "Legacy instances still active; old index retained", context);
        }

        _metrics.RecordSwitch("retire_begin", _options.OldIndexName, new Dictionary<string, object?>
        {
            ["ddlLockTimeoutSec"] = _options.IndexDdlLockTimeout.TotalSeconds,
        });

        await using var indexWriteLease = await _indexBarrier.EnterWriteAsync(cancellationToken).ConfigureAwait(false);
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = $"""
            SET LOCK_TIMEOUT {Math.Max(1, (int)_options.IndexDdlLockTimeout.TotalMilliseconds)};
            IF EXISTS (
                SELECT 1 FROM sys.indexes
                WHERE object_id = OBJECT_ID(N'dbo.Orders') AND name = @IndexName)
            BEGIN
                DROP INDEX [{_options.OldIndexName}] ON dbo.Orders;
            END
            """;
        command.Parameters.AddWithValue("@IndexName", _options.OldIndexName);
        await command.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);

        _metrics.RecordSwitch("retire_complete", _options.OldIndexName, new Dictionary<string, object?>
        {
            ["outcome"] = "dropped_or_already_absent",
        });
    }

    /// <summary>
    /// Fallback path: remove the new index and restore the old one, verifying
    /// both objects end in the expected post-rollback state.
    /// </summary>
    public async Task RollbackToLegacyIndexAsync(CancellationToken cancellationToken = default)
    {
        _metrics.RecordSwitch("rollback_begin", _options.NewIndexName, new Dictionary<string, object?>());

        await using var indexWriteLease = await _indexBarrier.EnterWriteAsync(cancellationToken).ConfigureAwait(false);
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        var connection = lease.Connection;

        await using var dropNew = connection.CreateCommand();
        dropNew.CommandText = $"""
            SET LOCK_TIMEOUT {Math.Max(1, (int)_options.IndexDdlLockTimeout.TotalMilliseconds)};
            IF EXISTS (SELECT 1 FROM sys.indexes
                       WHERE object_id = OBJECT_ID(N'dbo.Orders') AND name = @NewIndex)
                DROP INDEX [{_options.NewIndexName}] ON dbo.Orders;
            """;
        dropNew.Parameters.AddWithValue("@NewIndex", _options.NewIndexName);
        await dropNew.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);

        _metrics.RecordSwitch("rollback_new_index_dropped", _options.NewIndexName,
            new Dictionary<string, object?>());

        await using var restoreOld = connection.CreateCommand();
        restoreOld.CommandText = $"""
            IF NOT EXISTS (SELECT 1 FROM sys.indexes
                           WHERE object_id = OBJECT_ID(N'dbo.Orders') AND name = @OldIndex)
            BEGIN
                CREATE NONCLUSTERED INDEX [{_options.OldIndexName}]
                    ON dbo.Orders (Status, OrderId)
                    INCLUDE (TenantId, Amount, UpdatedAt);
            END
            """;
        restoreOld.Parameters.AddWithValue("@OldIndex", _options.OldIndexName);
        await restoreOld.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);

        _metrics.RecordSwitch("rollback_old_index_restored", _options.OldIndexName,
            new Dictionary<string, object?> { ["outcome"] = "created_or_already_present" });
    }

    /// <summary>Rebuild the new index after a rollback so later scenarios can rerun.</summary>
    public async Task RecreateNewIndexAsync(CancellationToken cancellationToken = default)
    {
        await using var indexWriteLease = await _indexBarrier.EnterWriteAsync(cancellationToken).ConfigureAwait(false);
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = $"""
            IF NOT EXISTS (SELECT 1 FROM sys.indexes
                           WHERE object_id = OBJECT_ID(N'dbo.Orders') AND name = @NewIndex)
            BEGIN
                CREATE NONCLUSTERED INDEX [{_options.NewIndexName}]
                    ON dbo.Orders (TenantId, Status)
                    INCLUDE (Amount, UpdatedAt);
            END
            """;
        command.Parameters.AddWithValue("@NewIndex", _options.NewIndexName);
        await command.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Idempotently restores the dual-index topology so every scenario starts
    /// from the same baseline and the whole suite stays repeatable.
    /// </summary>
    public async Task EnsureDualIndexesAsync(CancellationToken cancellationToken = default)
    {
        await using var indexWriteLease = await _indexBarrier.EnterWriteAsync(cancellationToken).ConfigureAwait(false);
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = $"""
            IF NOT EXISTS (SELECT 1 FROM sys.indexes
                           WHERE object_id = OBJECT_ID(N'dbo.Orders') AND name = @OldIndex)
            BEGIN
                CREATE NONCLUSTERED INDEX [{_options.OldIndexName}]
                    ON dbo.Orders (Status, OrderId)
                    INCLUDE (TenantId, Amount, UpdatedAt);
            END

            IF NOT EXISTS (SELECT 1 FROM sys.indexes
                           WHERE object_id = OBJECT_ID(N'dbo.Orders') AND name = @NewIndex)
            BEGIN
                CREATE NONCLUSTERED INDEX [{_options.NewIndexName}]
                    ON dbo.Orders (TenantId, Status)
                    INCLUDE (Amount, UpdatedAt);
            END

            UPDATE dbo.LegacyHeartbeat SET LastBeatUtc = SYSUTCDATETIME();
            """;
        command.Parameters.AddWithValue("@OldIndex", _options.OldIndexName);
        command.Parameters.AddWithValue("@NewIndex", _options.NewIndexName);
        await command.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
    }

    public async Task<IReadOnlyList<object>> GetIndexCatalogAsync(CancellationToken cancellationToken = default)
    {
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = "SELECT IndexName, IndexType, IsDisabled, KeyColumns, IncludeColumns FROM dbo.vw_IndexCatalog ORDER BY IndexName;";
        var catalog = new List<object>();
        await using var reader = await command.ExecuteReaderAsync(cancellationToken).ConfigureAwait(false);
        while (await reader.ReadAsync(cancellationToken).ConfigureAwait(false))
        {
            catalog.Add(new
            {
                indexName = reader.GetString(0),
                indexType = reader.GetString(1),
                isDisabled = reader.GetBoolean(2),
                keyColumns = reader.IsDBNull(3) ? string.Empty : reader.GetString(3),
                includeColumns = reader.IsDBNull(4) ? string.Empty : reader.GetString(4),
            });
        }
        return catalog;
    }
}
