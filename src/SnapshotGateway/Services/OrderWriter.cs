using System.Diagnostics;
using System.Globalization;
using Microsoft.Data.Sqlite;
using SnapshotGateway.Configuration;
using SnapshotGateway.Infrastructure;

namespace SnapshotGateway.Services;

public sealed record OrderUpdateResult(
    long SnapshotVersion,
    int UpdatedRows,
    double DurationMs,
    string WriteToken,
    IReadOnlyList<long> OrderIds);

public sealed class OrderWriter
{
    private readonly GatewayOptions _options;
    private readonly SqliteConnectionFactory _factory;
    private readonly PrimaryWriteGate _writeGate;
    private long _batchSequence;

    public OrderWriter(
        GatewayOptions options,
        SqliteConnectionFactory factory,
        PrimaryWriteGate writeGate)
    {
        _options = options;
        _factory = factory;
        _writeGate = writeGate;
    }

    public async Task<OrderUpdateResult> UpdateBatchAsync(
        int count = 25,
        CancellationToken cancellationToken = default)
    {
        count = Math.Clamp(count, 1, 500);
        var batch = Interlocked.Increment(ref _batchSequence);
        var token = $"write-{batch:D8}";
        var stopwatch = Stopwatch.StartNew();
        using var writeLease = await _writeGate.AcquireAsync(cancellationToken);
        await using var lease = await _factory.OpenAsync(
            _options.PrimaryDatabasePath, readOnly: false, cancellationToken);
        var connection = lease.Connection;
        await using var transaction = connection.BeginTransaction(
            System.Data.IsolationLevel.Serializable,
            deferred: false);

        var orderIds = new List<long>(count);
        await using (var update = connection.CreateCommand())
        {
            update.Transaction = transaction;
            update.CommandText = """
                UPDATE orders
                SET UpdatedAt=$updatedAt,
                    Amount=Amount+1,
                    LastWriteToken=$token
                WHERE OrderId=$id;
                """;
            var idParameter = update.Parameters.Add("$id", SqliteType.Integer);
            var updatedParameter = update.Parameters.Add("$updatedAt", SqliteType.Text);
            update.Parameters.AddWithValue("$token", token);
            var baseTime = new DateTimeOffset(2026, 3, 1, 0, 0, 0, TimeSpan.Zero);
            for (var i = 0; i < count; i++)
            {
                var orderId = ((batch * count + i) % DatabaseInitializer.OrderCount) + 1;
                orderIds.Add(orderId);
                idParameter.Value = orderId;
                updatedParameter.Value = baseTime
                    .AddSeconds((batch * 10_000L) + i)
                    .ToString("O", CultureInfo.InvariantCulture);
                await update.ExecuteNonQueryAsync(cancellationToken);
            }
        }

        long snapshotVersion;
        await using (var bump = connection.CreateCommand())
        {
            bump.Transaction = transaction;
            bump.CommandText = """
                UPDATE app_metadata
                SET value=CAST(value AS INTEGER)+1
                WHERE key='data_snapshot_version';
                SELECT CAST(value AS INTEGER)
                FROM app_metadata
                WHERE key='data_snapshot_version';
                """;
            snapshotVersion = Convert.ToInt64(
                await bump.ExecuteScalarAsync(cancellationToken),
                CultureInfo.InvariantCulture);
        }

        await transaction.CommitAsync(cancellationToken);
        stopwatch.Stop();
        return new OrderUpdateResult(
            snapshotVersion,
            count,
            stopwatch.Elapsed.TotalMilliseconds,
            token,
            orderIds);
    }
}
