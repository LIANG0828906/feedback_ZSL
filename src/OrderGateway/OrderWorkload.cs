using Microsoft.Data.SqlClient;

namespace OrderGateway;

/// <summary>
/// Write-side workload used by the mixed-load concurrency scenario.
/// Updates rotate Amount and bump UpdatedAt on a fixed batch so readers and
/// writers contend on the same rows while snapshot reads stay repeatable.
/// </summary>
public sealed class OrderWorkload
{
    private readonly ConnectionGate _gate;
    private readonly GatewayOptions _options;
    private readonly MetricsCollector _metrics;

    public OrderWorkload(ConnectionGate gate, GatewayOptions options, MetricsCollector metrics)
    {
        _gate = gate;
        _options = options;
        _metrics = metrics;
    }

    public async Task<int> UpdateBatchAsync(
        int tenantId,
        byte status,
        int batchSize,
        int round,
        CancellationToken cancellationToken = default)
    {
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = """
            UPDATE TOP (@BatchSize) dbo.Orders
            SET Amount = ((Amount + @Round) % 10000),
                UpdatedAt = SYSUTCDATETIME()
            WHERE TenantId = @TenantId
              AND Status = @Status;
            """;
        command.Parameters.AddWithValue("@BatchSize", batchSize);
        command.Parameters.AddWithValue("@Round", round + 1);
        command.Parameters.AddWithValue("@TenantId", tenantId);
        command.Parameters.AddWithValue("@Status", status);

        try
        {
            return await command.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (SqlException ex) when (ex.Number is 1205 or 3960 or 3961)
        {
            var context = new Dictionary<string, object?>
            {
                ["tenantId"] = tenantId,
                ["status"] = status,
                ["batchSize"] = batchSize,
                ["round"] = round,
                ["sqlErrorNumber"] = ex.Number,
            };
            _metrics.RecordError(GatewayError.Deadlock, context);
            throw new GatewayException(
                GatewayError.Deadlock,
                "Deadlock victim during update workload",
                context,
                ex);
        }
    }

    /// <summary>Read-only long query (~2s) for the connection-starvation scenario.</summary>
    public async Task RunLongQueryAsync(TimeSpan duration, CancellationToken cancellationToken = default)
    {
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = "WAITFOR DELAY @Delay; SELECT 1 AS Done;";
        command.Parameters.AddWithValue("@Delay", duration.ToString(@"hh\:mm\:ss"));
        command.CommandTimeout = (int)duration.TotalSeconds + 30;
        await command.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
    }
}
