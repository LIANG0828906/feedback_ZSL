using InventoryMigration.Api.Domain;
using InventoryMigration.Api.Infrastructure;
using Npgsql;

namespace InventoryMigration.Api.Services;

public sealed class ValidationService
{
    private readonly DatabasePool _oldPool;
    private readonly DatabasePool _newPool;
    private readonly FaultInjector _faults;
    private readonly ILogger<ValidationService> _logger;

    public ValidationService(
        DatabasePools pools,
        FaultInjector faults,
        ILogger<ValidationService> logger)
    {
        _oldPool = pools.Old;
        _newPool = pools.New;
        _faults = faults;
        _logger = logger;
    }

    public async Task<ValidationReport> ValidateBatchAsync(
        BatchRecord batch,
        long startSeq,
        long cutoffSeq,
        string startSnapshot,
        CancellationToken cancellationToken)
    {
        var startedAt = DateTimeOffset.UtcNow;
        await ApplyInjectedCorruptionAsync(batch, cancellationToken);

        var deltasTask = ReadOldDeltasAsync(batch, startSeq, cutoffSeq, cancellationToken);
        var newStateTask = ReadNewStateAsync(batch.BatchId, cancellationToken);
        await Task.WhenAll(deltasTask, newStateTask);
        var deltas = await deltasTask;
        var newRows = await newStateTask;

        var mismatches = new List<ValidationMismatch>();
        foreach (var row in newRows)
        {
            deltas.TryGetValue(row.ProductId, out var delta);
            var oldProjected = row.OldStockQty + delta.ChangeQty;
            if (oldProjected == row.NewQty)
            {
                continue;
            }

            mismatches.Add(new ValidationMismatch(
                row.ProductId,
                oldProjected,
                row.NewQty,
                delta.LastSeq > 0 ? delta.LastSeq : startSeq,
                row.NewLastSeq));
        }

        var finishedAt = DateTimeOffset.UtcNow;
        _logger.LogInformation(
            "Validation batch {BatchId} range {RangeStart}-{RangeEnd}: {Checked} checked, {Mismatches} mismatches",
            batch.BatchId,
            batch.RangeStart,
            batch.RangeEnd,
            newRows.Count,
            mismatches.Count);

        return new ValidationReport(
            batch.BatchId,
            batch.RangeStart,
            batch.RangeEnd,
            newRows.Count,
            mismatches.Count,
            mismatches,
            startedAt,
            finishedAt,
            startSnapshot);
    }

    private async Task ApplyInjectedCorruptionAsync(
        BatchRecord batch,
        CancellationToken cancellationToken)
    {
        var products = _faults.GetValidationCorruptionProducts()
            .Where(id => id >= batch.RangeStart && id <= batch.RangeEnd)
            .ToArray();
        if (products.Length == 0)
        {
            return;
        }

        await using var lease = await _newPool.AcquireAsync(cancellationToken);
        await using var transaction = await lease.Connection.BeginTransactionAsync(cancellationToken);
        foreach (var productId in products)
        {
            await using var command = new NpgsqlCommand(
                """
                UPDATE inventory
                SET stock_qty = stock_qty + 1,
                    version = version + 1,
                    updated_at = clock_timestamp()
                WHERE product_id = @product;
                """,
                lease.Connection,
                transaction);
            command.Parameters.AddWithValue("product", productId);
            await command.ExecuteNonQueryAsync(cancellationToken);
        }

        await transaction.CommitAsync(cancellationToken);
        _logger.LogWarning(
            "Injected validation corruption for batch {BatchId}: {Products}",
            batch.BatchId,
            string.Join(',', products));
    }

    private async Task<Dictionary<long, OldDelta>> ReadOldDeltasAsync(
        BatchRecord batch,
        long startSeq,
        long cutoffSeq,
        CancellationToken cancellationToken)
    {
        var results = new Dictionary<long, OldDelta>();
        if (cutoffSeq <= startSeq)
        {
            return results;
        }

        await using var lease = await _oldPool.AcquireAsync(cancellationToken);
        await using var command = new NpgsqlCommand(
            """
            SELECT product_id,
                   COALESCE(SUM(change_qty), 0)::bigint AS change_qty,
                   MAX(seq) AS last_seq
            FROM migration_outbox
            WHERE product_id BETWEEN @start AND @end
              AND seq > @start_seq
              AND seq <= @cutoff_seq
            GROUP BY product_id;
            """,
            lease.Connection);
        command.Parameters.AddWithValue("start", batch.RangeStart);
        command.Parameters.AddWithValue("end", batch.RangeEnd);
        command.Parameters.AddWithValue("start_seq", startSeq);
        command.Parameters.AddWithValue("cutoff_seq", cutoffSeq);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            results[reader.GetInt64(0)] = new OldDelta(reader.GetInt64(1), reader.GetInt64(2));
        }

        return results;
    }

    private async Task<List<NewStateRow>> ReadNewStateAsync(
        long batchId,
        CancellationToken cancellationToken)
    {
        var results = new List<NewStateRow>();
        await using var lease = await _newPool.AcquireAsync(cancellationToken);
        await using var command = new NpgsqlCommand(
            """
            SELECT b.product_id,
                   b.old_stock_qty,
                   n.stock_qty,
                   COALESCE(
                       (SELECT MAX(f.flow_id)
                        FROM deduction_flow f
                        WHERE f.product_id = b.product_id),
                       0)
            FROM migration_product_baseline b
            JOIN inventory n ON n.product_id = b.product_id
            WHERE b.batch_id = @batch
            ORDER BY b.product_id;
            """,
            lease.Connection);
        command.Parameters.AddWithValue("batch", batchId);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            results.Add(new NewStateRow(
                reader.GetInt64(0),
                reader.GetInt64(1),
                reader.GetInt64(2),
                reader.GetInt64(3)));
        }

        return results;
    }

    private readonly record struct OldDelta(long ChangeQty, long LastSeq);

    private sealed record NewStateRow(long ProductId, long OldStockQty, long NewQty, long NewLastSeq);
}
