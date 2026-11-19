using Microsoft.Data.SqlClient;

namespace OrderRouter;

public sealed record MigrationProgress(long BatchesCommitted, long RowsCopied, bool Complete, long LastIdTo);

/// <summary>
/// Moves orders older than the configured archive cutoff from Orders to
/// OrdersArchive in primary-key ranged batches. Each batch runs under
/// SNAPSHOT isolation: it reads a stable set of hot rows, upserts them into
/// the archive (keeping the newer LastUpdated on conflicts), deletes from
/// the hot table only rows whose archived copy is at least as new, and
/// commits atomically — readers never observe a half-migrated order. The
/// committed range is then recorded in the SQLite ledger; on restart the
/// migrator resumes after the last committed range. Re-running a batch is
/// idempotent because the archive upsert and the guarded delete converge.
/// </summary>
public sealed class Migrator
{
    private readonly AppOptions _opts;
    private readonly Ledger _ledger;
    private readonly int _batchSize;
    private readonly int _batchDelayMs;

    public Migrator(AppOptions opts, Ledger ledger, int? batchSize = null, int batchDelayMs = 0)
    {
        _opts = opts;
        _ledger = ledger;
        _batchSize = batchSize ?? opts.BatchSize;
        _batchDelayMs = batchDelayMs;
    }

    public async Task<MigrationProgress> RunAsync(int? maxBatches = null, Action<MigrationProgress>? onBatch = null, CancellationToken ct = default)
    {
        var cutoff = _opts.ArchiveCutoffUtc;
        var last = _ledger.LastBatch();
        long cursor = last?.IdToInclusive ?? 0;
        long batchId = last?.BatchId ?? 0;
        long totalRows = 0;
        long batches = 0;

        while (true)
        {
            ct.ThrowIfCancellationRequested();
            if (maxBatches is not null && batches >= maxBatches.Value) break;

            MigrationProgress? step = null;
            for (int attempt = 1; attempt <= 8 && step is null; attempt++)
            {
                try
                {
                    step = await RunBatchOnceAsync(cursor, batchId, cutoff, ct).ConfigureAwait(false);
                }
                catch (SqlException ex) when (ex.Number == 3960)
                {
                    // Snapshot update conflict: a concurrent write touched a row
                    // in this range after our snapshot started. The batch is
                    // idempotent, so back off and retry the whole batch.
                    await Task.Delay(TimeSpan.FromMilliseconds(25 * attempt), ct).ConfigureAwait(false);
                }
            }
            if (step is null)
                throw new InvalidOperationException($"migration batch after id {cursor} kept hitting snapshot conflicts");

            if (step.Complete && step.LastIdTo == cursor && step.RowsCopied == 0 && step.BatchesCommitted == 0)
                break; // no more candidates

            batchId = step.LastIdTo == cursor ? batchId : batchId + 1;
            // RunBatchOnceAsync already recorded the ledger entry when a range committed.
            if (step.RowsCopied >= 0 && step.LastIdTo != cursor)
            {
                cursor = step.LastIdTo;
                totalRows += step.RowsCopied;
                batches++;
                onBatch?.Invoke(new MigrationProgress(batches, totalRows, false, cursor));
                if (_batchDelayMs > 0)
                    await Task.Delay(_batchDelayMs, ct).ConfigureAwait(false);
            }
            else if (step.Complete)
            {
                break;
            }
        }

        var complete = _ledger.GetMeta("migration_complete") == "true";
        return new MigrationProgress(batches, totalRows, complete, cursor);
    }

    /// <summary>
    /// Runs one batch attempt. Returns (0 rows, same cursor, complete) when no
    /// candidates remain; otherwise commits the range and records the ledger.
    /// </summary>
    private async Task<MigrationProgress> RunBatchOnceAsync(long cursor, long batchId, DateTime cutoff, CancellationToken ct)
    {
            await using var conn = new SqlConnection(_opts.ConnectionString);
            await conn.OpenAsync(ct).ConfigureAwait(false);
            await using var tx = (SqlTransaction)await conn.BeginTransactionAsync(
                System.Data.IsolationLevel.Snapshot, ct).ConfigureAwait(false);

            // Pick the next PK range of archive candidates after the cursor.
            long rangeStart = cursor;
            long rangeEnd;
            await using (var cmd = new SqlCommand("""
                SELECT @from + 1 AS RangeStart, COALESCE(MAX(OrderId), 0) AS RangeEnd
                FROM (
                    SELECT TOP (@n) OrderId
                    FROM dbo.Orders
                    WHERE CreatedAt < @cutoff AND OrderId > @from
                    ORDER BY OrderId
                ) AS batch;
                """, conn, tx))
            {
                // The range scan itself must see a stable snapshot of the batch.
                cmd.Parameters.AddWithValue("@from", cursor);
                cmd.Parameters.AddWithValue("@n", _batchSize);
                cmd.Parameters.AddWithValue("@cutoff", cutoff);
                await using var r = await cmd.ExecuteReaderAsync(ct).ConfigureAwait(false);
                await r.ReadAsync(ct).ConfigureAwait(false);
                rangeEnd = r.GetInt64(1);
            }

            if (rangeEnd == 0)
            {
                await tx.CommitAsync(ct).ConfigureAwait(false);
                _ledger.SetMeta("migration_complete", "true");
                _ledger.SetMeta("migration_completed_utc", DateTime.UtcNow.ToString("O"));
                return new MigrationProgress(0, 0, true, cursor);
            }

            // Upsert the snapshot-visible rows of this range into the archive.
            int copied;
            await using (var cmd = new SqlCommand("""
                MERGE dbo.OrdersArchive AS target
                USING (SELECT OrderId, OrderNo, CustomerId, CreatedAt, LastUpdated, Amount, Currency, Status
                       FROM dbo.Orders
                       WHERE CreatedAt < @cutoff AND OrderId > @from AND OrderId <= @to) AS src
                ON target.OrderId = src.OrderId
                WHEN MATCHED AND src.LastUpdated > target.LastUpdated THEN
                    UPDATE SET OrderNo = src.OrderNo, CustomerId = src.CustomerId, CreatedAt = src.CreatedAt,
                               LastUpdated = src.LastUpdated, Amount = src.Amount,
                               Currency = src.Currency, Status = src.Status
                WHEN NOT MATCHED THEN
                    INSERT (OrderId, OrderNo, CustomerId, CreatedAt, LastUpdated, Amount, Currency, Status)
                    VALUES (src.OrderId, src.OrderNo, src.CustomerId, src.CreatedAt, src.LastUpdated,
                            src.Amount, src.Currency, src.Status);
                """, conn, tx))
            {
                cmd.Parameters.AddWithValue("@cutoff", cutoff);
                cmd.Parameters.AddWithValue("@from", rangeStart);
                cmd.Parameters.AddWithValue("@to", rangeEnd);
                cmd.CommandTimeout = 120;
                copied = await cmd.ExecuteNonQueryAsync(ct).ConfigureAwait(false);
            }

            // Delete from hot only rows whose archived copy is at least as new.
            // Rows updated after the snapshot stay hot and are picked up by the
            // overlap merge (or by a later batch re-run via the ledger resume).
            await using (var cmd = new SqlCommand("""
                DELETE h FROM dbo.Orders h
                INNER JOIN dbo.OrdersArchive a ON a.OrderId = h.OrderId
                WHERE h.CreatedAt < @cutoff AND h.OrderId > @from AND h.OrderId <= @to
                  AND a.LastUpdated >= h.LastUpdated;
                """, conn, tx))
            {
                cmd.Parameters.AddWithValue("@cutoff", cutoff);
                cmd.Parameters.AddWithValue("@from", rangeStart);
                cmd.Parameters.AddWithValue("@to", rangeEnd);
                cmd.CommandTimeout = 120;
                await cmd.ExecuteNonQueryAsync(ct).ConfigureAwait(false);
            }

            await tx.CommitAsync(ct).ConfigureAwait(false);

            _ledger.RecordBatch(batchId + 1, rangeStart, rangeEnd, copied);
            return new MigrationProgress(0, copied, false, rangeEnd);
    }
}
