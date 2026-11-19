using System.Data;
using InventoryMigration.Api.Domain;
using InventoryMigration.Api.Infrastructure;
using InventoryMigration.Api.Options;
using Microsoft.Extensions.Options;
using Npgsql;

namespace InventoryMigration.Api.Services;

public sealed class MigrationService
{
    private readonly MigrationOptions _options;
    private readonly LedgerStore _ledger;
    private readonly DatabasePool _oldPool;
    private readonly DatabasePool _newPool;
    private readonly WriteGate _writeGate;
    private readonly ValidationService _validation;
    private readonly MetricsRegistry _metrics;
    private readonly FaultInjector _faults;
    private readonly ILogger<MigrationService> _logger;
    private readonly object _runSync = new();
    private CancellationTokenSource? _runnerCts;
    private Task? _runnerTask;

    public MigrationService(
        IOptions<MigrationOptions> options,
        LedgerStore ledger,
        DatabasePools pools,
        WriteGate writeGate,
        ValidationService validation,
        MetricsRegistry metrics,
        FaultInjector faults,
        ILogger<MigrationService> logger)
    {
        _options = options.Value;
        _ledger = ledger;
        _oldPool = pools.Old;
        _newPool = pools.New;
        _writeGate = writeGate;
        _validation = validation;
        _metrics = metrics;
        _faults = faults;
        _logger = logger;
    }

    public bool IsRunning
    {
        get
        {
            lock (_runSync)
            {
                return _runnerTask is { IsCompleted: false };
            }
        }
    }

    public async Task<bool> StartAsync(CancellationToken cancellationToken)
    {
        var state = await _ledger.GetStateAsync(cancellationToken);
        if (state.LastCommittedEnd >= state.ProductMax && state.CurrentBatchId is null)
        {
            await _ledger.SetServiceStateAsync("completed", cancellationToken);
            return false;
        }

        lock (_runSync)
        {
            if (_runnerTask is { IsCompleted: false })
            {
                return false;
            }

            _runnerCts?.Dispose();
            _runnerCts = new CancellationTokenSource();
            _runnerTask = Task.Run(() => RunAsync(_runnerCts.Token), CancellationToken.None);
            return true;
        }
    }

    public async Task<bool> AutoResumeAsync(CancellationToken cancellationToken)
    {
        if (!_options.AutoResume)
        {
            return false;
        }

        var state = await _ledger.GetStateAsync(cancellationToken);
        var hasWork = state.CurrentBatchId.HasValue || state.LastCommittedEnd < state.ProductMax;
        if (!state.Started || !hasWork)
        {
            if (state.Started)
            {
                await _ledger.SetServiceStateAsync("completed", cancellationToken);
            }

            return false;
        }

        _logger.LogInformation(
            "Auto-resuming migration from last committed product {LastCommittedEnd}",
            state.LastCommittedEnd);
        return await StartAsync(cancellationToken);
    }

    public async Task PauseAsync(CancellationToken cancellationToken)
    {
        CancellationTokenSource? cts;
        lock (_runSync)
        {
            cts = _runnerCts;
        }

        cts?.Cancel();
        await _ledger.SetServiceStateAsync("paused", cancellationToken);
    }

    private async Task RunAsync(CancellationToken cancellationToken)
    {
        await _ledger.SetServiceStateAsync("running", cancellationToken);
        try
        {
            while (!cancellationToken.IsCancellationRequested)
            {
                var delay = _faults.MigrationBatchDelayMs;
                if (delay > 0)
                {
                    await Task.Delay(delay, cancellationToken);
                }

                var batch = await _ledger.TryGetCurrentOrNextBatchAsync(
                    _options.BatchSize,
                    cancellationToken);
                if (batch is null)
                {
                    await _ledger.SetServiceStateAsync("completed", cancellationToken);
                    _logger.LogInformation("Migration completed.");
                    return;
                }

                try
                {
                    await ExecuteBatchAsync(batch, cancellationToken);
                }
                catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
                {
                    throw;
                }
                catch (ServiceException exception)
                {
                    _logger.LogError(
                        exception,
                        "Batch {BatchId} failed with {Code}; it remains the resumable current batch.",
                        batch.BatchId,
                        exception.Code);
                    await _ledger.MarkBatchFailedAsync(batch.BatchId, exception.Message, cancellationToken);
                    await _ledger.SetServiceStateAsync(
                        exception.Code == ErrorCodes.Connection ? "waiting_connection" : "failed",
                        cancellationToken);
                    await Task.Delay(TimeSpan.FromSeconds(1), cancellationToken);
                }
                catch (Exception exception)
                {
                    _logger.LogError(
                        exception,
                        "Batch {BatchId} failed; it remains the resumable current batch.",
                        batch.BatchId);
                    await _ledger.MarkBatchFailedAsync(batch.BatchId, exception.Message, cancellationToken);
                    await _ledger.SetServiceStateAsync("failed", cancellationToken);
                    await Task.Delay(TimeSpan.FromSeconds(2), cancellationToken);
                }
            }
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            await _ledger.SetServiceStateAsync("paused", CancellationToken.None);
        }
    }

    private async Task ExecuteBatchAsync(BatchRecord batch, CancellationToken cancellationToken)
    {
        var state = await _ledger.GetStateAsync(cancellationToken);
        if (batch.RangeStart <= state.LastCommittedEnd)
        {
            throw new InvalidOperationException(
                $"Batch {batch.BatchId} starts at {batch.RangeStart}, inside committed range ending {state.LastCommittedEnd}.");
        }

        var baseline = await EnsureBaselineAsync(batch, cancellationToken);
        _logger.LogInformation(
            "Starting batch {BatchId} products {RangeStart}-{RangeEnd}, baseline seq {StartSeq}",
            batch.BatchId,
            batch.RangeStart,
            batch.RangeEnd,
            baseline.StartSeq);

        using var cutover = await _writeGate.BeginCutoverAsync(cancellationToken);
        var final = await CaptureFinalOldStateAsync(batch, cancellationToken);
        if (final.CutoffSeq < baseline.StartSeq)
        {
            throw new InvalidOperationException(
                $"Outbox moved backwards for batch {batch.BatchId}: start={baseline.StartSeq}, cutoff={final.CutoffSeq}.");
        }

        var flowCount = await WriteFinalStateAsync(batch, final, cancellationToken);
        var validation = await _validation.ValidateBatchAsync(
            batch,
            baseline.StartSeq,
            final.CutoffSeq,
            baseline.Snapshot,
            cancellationToken);

        await _ledger.CommitBatchAsync(
            batch,
            final.Snapshot,
            final.CutoffSeq,
            flowCount,
            validation,
            cancellationToken);
        _metrics.SetValidationMismatchProducts(validation.MismatchCount);

        _logger.LogInformation(
            "Committed batch {BatchId} products {RangeStart}-{RangeEnd}; flows {FlowCount}; mismatches {Mismatches}",
            batch.BatchId,
            batch.RangeStart,
            batch.RangeEnd,
            flowCount,
            validation.MismatchCount);
    }

    private async Task<BaselineInfo> EnsureBaselineAsync(
        BatchRecord batch,
        CancellationToken cancellationToken)
    {
        var existing = await ReadBaselineMetaAsync(batch.BatchId, cancellationToken);
        if (existing is not null)
        {
            if (batch.StartSeq != existing.StartSeq || batch.StartSnapshot != existing.Snapshot)
            {
                await _ledger.SetBatchSnapshotAsync(
                    batch.BatchId,
                    existing.Snapshot,
                    existing.StartSeq,
                    cancellationToken);
            }

            return new BaselineInfo(existing.Snapshot, existing.StartSeq);
        }

        var capture = await CaptureBaselineFromOldAsync(batch, cancellationToken);
        await WriteBaselineAsync(batch, capture, cancellationToken);
        await _ledger.SetBatchSnapshotAsync(
            batch.BatchId,
            capture.Snapshot,
            capture.Seq,
            cancellationToken);
        return new BaselineInfo(capture.Snapshot, capture.Seq);
    }

    private async Task<BaselineMeta?> ReadBaselineMetaAsync(
        long batchId,
        CancellationToken cancellationToken)
    {
        await using var lease = await _newPool.AcquireAsync(cancellationToken);
        await using var command = new NpgsqlCommand(
            """
            SELECT start_snapshot, start_seq
            FROM migration_batch_meta
            WHERE batch_id = @batch;
            """,
            lease.Connection);
        command.Parameters.AddWithValue("batch", batchId);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        return await reader.ReadAsync(cancellationToken)
            ? new BaselineMeta(reader.GetString(0), reader.GetInt64(1))
            : null;
    }

    private async Task<SnapshotCapture> CaptureBaselineFromOldAsync(
        BatchRecord batch,
        CancellationToken cancellationToken)
    {
        await using var lease = await _oldPool.AcquireAsync(cancellationToken);
        await using var transaction = await lease.Connection.BeginTransactionAsync(
            IsolationLevel.ReadCommitted,
            cancellationToken);
        await LockMigrationTablesAsync(lease.Connection, transaction, cancellationToken);

        string snapshot;
        long seq;
        await using (var metadata = new NpgsqlCommand(
            """
            SELECT pg_current_snapshot()::text,
                   (SELECT COALESCE(MAX(seq), 0) FROM migration_outbox);
            """,
            lease.Connection,
            transaction))
        await using (var reader = await metadata.ExecuteReaderAsync(cancellationToken))
        {
            await reader.ReadAsync(cancellationToken);
            snapshot = reader.GetString(0);
            seq = reader.GetInt64(1);
        }

        var inventory = await ReadInventoryAsync(
            lease.Connection,
            transaction,
            batch.RangeStart,
            batch.RangeEnd,
            cancellationToken);
        await transaction.CommitAsync(cancellationToken);
        return new SnapshotCapture(snapshot, seq, inventory);
    }

    private async Task WriteBaselineAsync(
        BatchRecord batch,
        SnapshotCapture capture,
        CancellationToken cancellationToken)
    {
        await using var lease = await _newPool.AcquireAsync(cancellationToken);
        await using var transaction = await lease.Connection.BeginTransactionAsync(cancellationToken);
        await using (var meta = new NpgsqlCommand(
            """
            INSERT INTO migration_batch_meta(
                batch_id, range_start, range_end, start_snapshot, start_seq, started_at)
            VALUES (@batch, @start, @end, @snapshot, @seq, clock_timestamp())
            ON CONFLICT(batch_id) DO NOTHING;
            """,
            lease.Connection,
            transaction))
        {
            meta.Parameters.AddWithValue("batch", batch.BatchId);
            meta.Parameters.AddWithValue("start", batch.RangeStart);
            meta.Parameters.AddWithValue("end", batch.RangeEnd);
            meta.Parameters.AddWithValue("snapshot", capture.Snapshot);
            meta.Parameters.AddWithValue("seq", capture.Seq);
            await meta.ExecuteNonQueryAsync(cancellationToken);
        }

        await using (var create = new NpgsqlCommand(
            """
            CREATE TEMP TABLE stage_baseline(
                product_id BIGINT NOT NULL,
                old_stock_qty BIGINT NOT NULL
            ) ON COMMIT DROP;
            """,
            lease.Connection,
            transaction))
        {
            await create.ExecuteNonQueryAsync(cancellationToken);
        }

        await using (var writer = await lease.Connection.BeginBinaryImportAsync(
            "COPY stage_baseline(product_id, old_stock_qty) FROM STDIN (FORMAT BINARY)",
            cancellationToken))
        {
            foreach (var row in capture.Inventory)
            {
                await writer.StartRowAsync(cancellationToken);
                await writer.WriteAsync(row.ProductId, NpgsqlTypes.NpgsqlDbType.Bigint, cancellationToken);
                await writer.WriteAsync(row.StockQty, NpgsqlTypes.NpgsqlDbType.Bigint, cancellationToken);
            }

            await writer.CompleteAsync(cancellationToken);
        }

        await using (var insert = new NpgsqlCommand(
            """
            INSERT INTO migration_product_baseline(batch_id, product_id, old_stock_qty)
            SELECT @batch, product_id, old_stock_qty
            FROM stage_baseline
            ON CONFLICT(batch_id, product_id) DO NOTHING;
            """,
            lease.Connection,
            transaction))
        {
            insert.Parameters.AddWithValue("batch", batch.BatchId);
            await insert.ExecuteNonQueryAsync(cancellationToken);
        }

        await transaction.CommitAsync(cancellationToken);
    }

    private async Task<FinalCapture> CaptureFinalOldStateAsync(
        BatchRecord batch,
        CancellationToken cancellationToken)
    {
        await using var lease = await _oldPool.AcquireAsync(cancellationToken);
        await using var transaction = await lease.Connection.BeginTransactionAsync(
            IsolationLevel.ReadCommitted,
            cancellationToken);
        await LockMigrationTablesAsync(lease.Connection, transaction, cancellationToken);

        string snapshot;
        long cutoff;
        await using (var metadata = new NpgsqlCommand(
            """
            SELECT pg_current_snapshot()::text,
                   (SELECT COALESCE(MAX(seq), 0) FROM migration_outbox);
            """,
            lease.Connection,
            transaction))
        await using (var reader = await metadata.ExecuteReaderAsync(cancellationToken))
        {
            await reader.ReadAsync(cancellationToken);
            snapshot = reader.GetString(0);
            cutoff = reader.GetInt64(1);
        }

        var inventory = await ReadInventoryAsync(
            lease.Connection,
            transaction,
            batch.RangeStart,
            batch.RangeEnd,
            cancellationToken);
        var flows = await ReadFlowsAsync(
            lease.Connection,
            transaction,
            batch.RangeStart,
            batch.RangeEnd,
            cancellationToken);
        await transaction.CommitAsync(cancellationToken);
        return new FinalCapture(snapshot, cutoff, inventory, flows);
    }

    private async Task<long> WriteFinalStateAsync(
        BatchRecord batch,
        FinalCapture capture,
        CancellationToken cancellationToken)
    {
        await using var lease = await _newPool.AcquireAsync(cancellationToken);
        await using var transaction = await lease.Connection.BeginTransactionAsync(cancellationToken);

        await using (var create = new NpgsqlCommand(
            """
            CREATE TEMP TABLE stage_inventory(
                product_id BIGINT NOT NULL,
                stock_qty BIGINT NOT NULL
            ) ON COMMIT DROP;
            CREATE TEMP TABLE stage_flow(
                flow_id BIGINT NOT NULL,
                product_id BIGINT NOT NULL,
                direction TEXT NOT NULL,
                change_qty INTEGER NOT NULL,
                created_at TIMESTAMPTZ NOT NULL,
                shard_no SMALLINT NOT NULL
            ) ON COMMIT DROP;
            """,
            lease.Connection,
            transaction))
        {
            await create.ExecuteNonQueryAsync(cancellationToken);
        }

        await using (var writer = await lease.Connection.BeginBinaryImportAsync(
            "COPY stage_inventory(product_id, stock_qty) FROM STDIN (FORMAT BINARY)",
            cancellationToken))
        {
            foreach (var row in capture.Inventory)
            {
                await writer.StartRowAsync(cancellationToken);
                await writer.WriteAsync(row.ProductId, NpgsqlTypes.NpgsqlDbType.Bigint, cancellationToken);
                await writer.WriteAsync(row.StockQty, NpgsqlTypes.NpgsqlDbType.Bigint, cancellationToken);
            }

            await writer.CompleteAsync(cancellationToken);
        }

        await using (var writer = await lease.Connection.BeginBinaryImportAsync(
            """
            COPY stage_flow(flow_id, product_id, direction, change_qty, created_at, shard_no)
            FROM STDIN (FORMAT BINARY)
            """,
            cancellationToken))
        {
            foreach (var row in capture.Flows)
            {
                await writer.StartRowAsync(cancellationToken);
                await writer.WriteAsync(row.FlowId, NpgsqlTypes.NpgsqlDbType.Bigint, cancellationToken);
                await writer.WriteAsync(row.ProductId, NpgsqlTypes.NpgsqlDbType.Bigint, cancellationToken);
                await writer.WriteAsync(row.Direction, NpgsqlTypes.NpgsqlDbType.Text, cancellationToken);
                await writer.WriteAsync(row.ChangeQty, NpgsqlTypes.NpgsqlDbType.Integer, cancellationToken);
                await writer.WriteAsync(
                    row.CreatedAt,
                    NpgsqlTypes.NpgsqlDbType.TimestampTz,
                    cancellationToken);
                await writer.WriteAsync(
                    ShardFor(row.ProductId),
                    NpgsqlTypes.NpgsqlDbType.Smallint,
                    cancellationToken);
            }

            await writer.CompleteAsync(cancellationToken);
        }

        await using (var upsertInventory = new NpgsqlCommand(
            """
            INSERT INTO inventory(product_id, stock_qty, version, updated_at)
            SELECT product_id, stock_qty, 0, clock_timestamp()
            FROM stage_inventory
            ON CONFLICT(product_id) DO UPDATE SET
                stock_qty = EXCLUDED.stock_qty,
                version = inventory.version + 1,
                updated_at = clock_timestamp();
            """,
            lease.Connection,
            transaction))
        {
            await upsertInventory.ExecuteNonQueryAsync(cancellationToken);
        }

        await using (var insertFlows = new NpgsqlCommand(
            """
            INSERT INTO deduction_flow(
                flow_id, product_id, direction, change_qty, created_at, shard_no)
            SELECT flow_id, product_id, direction, change_qty, created_at, shard_no
            FROM stage_flow
            ON CONFLICT(flow_id, shard_no) DO NOTHING;
            """,
            lease.Connection,
            transaction))
        {
            await insertFlows.ExecuteNonQueryAsync(cancellationToken);
        }

        long totalFlows;
        await using (var countFlows = new NpgsqlCommand(
            """
            SELECT COUNT(*)
            FROM deduction_flow
            WHERE product_id BETWEEN @start AND @end;
            """,
            lease.Connection,
            transaction))
        {
            countFlows.Parameters.AddWithValue("start", batch.RangeStart);
            countFlows.Parameters.AddWithValue("end", batch.RangeEnd);
            totalFlows = (long)(await countFlows.ExecuteScalarAsync(cancellationToken)
                ?? throw new InvalidOperationException("Flow count query returned no value."));
        }

        await transaction.CommitAsync(cancellationToken);
        return totalFlows;
    }

    private static async Task LockMigrationTablesAsync(
        NpgsqlConnection connection,
        NpgsqlTransaction transaction,
        CancellationToken cancellationToken)
    {
        await using (var inventoryLock = new NpgsqlCommand(
            "LOCK TABLE inventory IN SHARE ROW EXCLUSIVE MODE;",
            connection,
            transaction))
        {
            await inventoryLock.ExecuteNonQueryAsync(cancellationToken);
        }

        await using var flowLock = new NpgsqlCommand(
            "LOCK TABLE deduction_flow IN SHARE ROW EXCLUSIVE MODE;",
            connection,
            transaction);
        await flowLock.ExecuteNonQueryAsync(cancellationToken);
    }

    private static async Task<List<InventoryRow>> ReadInventoryAsync(
        NpgsqlConnection connection,
        NpgsqlTransaction transaction,
        long rangeStart,
        long rangeEnd,
        CancellationToken cancellationToken)
    {
        var rows = new List<InventoryRow>(capacity: 1000);
        await using var command = new NpgsqlCommand(
            """
            SELECT product_id, stock_qty
            FROM inventory
            WHERE product_id BETWEEN @start AND @end
            ORDER BY product_id;
            """,
            connection,
            transaction);
        command.Parameters.AddWithValue("start", rangeStart);
        command.Parameters.AddWithValue("end", rangeEnd);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            rows.Add(new InventoryRow(reader.GetInt64(0), reader.GetInt64(1)));
        }

        return rows;
    }

    private static async Task<List<FlowRow>> ReadFlowsAsync(
        NpgsqlConnection connection,
        NpgsqlTransaction transaction,
        long rangeStart,
        long rangeEnd,
        CancellationToken cancellationToken)
    {
        var rows = new List<FlowRow>(capacity: 4096);
        await using var command = new NpgsqlCommand(
            """
            SELECT flow_id, product_id, direction, change_qty, created_at
            FROM deduction_flow
            WHERE product_id BETWEEN @start AND @end
            ORDER BY flow_id;
            """,
            connection,
            transaction);
        command.Parameters.AddWithValue("start", rangeStart);
        command.Parameters.AddWithValue("end", rangeEnd);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            rows.Add(new FlowRow(
                reader.GetInt64(0),
                reader.GetInt64(1),
                reader.GetString(2),
                reader.GetInt32(3),
                reader.GetFieldValue<DateTime>(4)));
        }

        return rows;
    }

    internal short ShardFor(long productId) =>
        (short)(productId % Math.Max(1, _options.ShardCount));

    private sealed record BaselineInfo(string Snapshot, long StartSeq);

    private sealed record BaselineMeta(string Snapshot, long StartSeq);

    private sealed record SnapshotCapture(string Snapshot, long Seq, List<InventoryRow> Inventory);

    private sealed record FinalCapture(
        string Snapshot,
        long CutoffSeq,
        List<InventoryRow> Inventory,
        List<FlowRow> Flows);

    private sealed record InventoryRow(long ProductId, long StockQty);

    private sealed record FlowRow(
        long FlowId,
        long ProductId,
        string Direction,
        int ChangeQty,
        DateTime CreatedAt);
}
