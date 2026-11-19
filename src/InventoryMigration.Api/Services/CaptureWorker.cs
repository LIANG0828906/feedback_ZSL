using InventoryMigration.Api.Domain;
using InventoryMigration.Api.Infrastructure;
using InventoryMigration.Api.Options;
using Microsoft.Extensions.Options;
using Npgsql;

namespace InventoryMigration.Api.Services;

public sealed class CaptureWorker : BackgroundService
{
    private readonly MigrationOptions _options;
    private readonly DatabasePool _oldPool;
    private readonly DatabasePool _newPool;
    private readonly LedgerStore _ledger;
    private readonly WriteGate _writeGate;
    private readonly MetricsRegistry _metrics;
    private readonly ILogger<CaptureWorker> _logger;
    private long _lastSeq;
    private long _capturedTotal;

    public CaptureWorker(
        IOptions<MigrationOptions> options,
        DatabasePools pools,
        LedgerStore ledger,
        WriteGate writeGate,
        MetricsRegistry metrics,
        ILogger<CaptureWorker> logger)
    {
        _options = options.Value;
        _oldPool = pools.Old;
        _newPool = pools.New;
        _ledger = ledger;
        _writeGate = writeGate;
        _metrics = metrics;
        _logger = logger;
    }

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        await Task.Yield();
        try
        {
            (_lastSeq, _capturedTotal) = await _ledger.GetCaptureStateAsync(stoppingToken);
            _metrics.SetCapturedChanges(_capturedTotal);
        }
        catch (Exception exception)
        {
            _logger.LogError(exception, "Could not initialize capture checkpoint.");
        }

        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                var events = await ReadEventsAsync(stoppingToken);
                if (events.Count == 0)
                {
                    await Task.Delay(_options.CapturePollMs, stoppingToken);
                    continue;
                }

                var advanced = false;
                long lastAdvanced = _lastSeq;
                foreach (var item in events)
                {
                    var shouldStop = await ProcessEventAsync(item, stoppingToken);
                    if (shouldStop)
                    {
                        break;
                    }

                    lastAdvanced = item.Seq;
                    _capturedTotal++;
                    _metrics.RecordCapturedChanges(1);
                    advanced = true;
                }

                if (advanced)
                {
                    _lastSeq = lastAdvanced;
                    await _ledger.SetCaptureStateAsync(_lastSeq, _capturedTotal, stoppingToken);
                }
                else
                {
                    await Task.Delay(_options.CapturePollMs, stoppingToken);
                }
            }
            catch (OperationCanceledException) when (stoppingToken.IsCancellationRequested)
            {
                return;
            }
            catch (ServiceException exception)
            {
                _logger.LogWarning(
                    "Capture worker delayed with {Code}: {Message}",
                    exception.Code,
                    exception.Message);
                await Task.Delay(TimeSpan.FromSeconds(1), stoppingToken);
            }
            catch (Exception exception)
            {
                _logger.LogError(exception, "Capture worker failed; retrying.");
                await Task.Delay(TimeSpan.FromSeconds(1), stoppingToken);
            }
        }
    }

    private async Task<List<CapturedEvent>> ReadEventsAsync(CancellationToken cancellationToken)
    {
        var events = new List<CapturedEvent>();
        await using var lease = await _oldPool.AcquireAsync(cancellationToken);
        await using var command = new NpgsqlCommand(
            """
            SELECT seq, product_id, flow_id, direction, change_qty, occurred_at
            FROM migration_outbox
            WHERE seq > @last
            ORDER BY seq
            LIMIT 500;
            """,
            lease.Connection);
        command.Parameters.AddWithValue("last", _lastSeq);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            events.Add(new CapturedEvent(
                reader.GetInt64(0),
                reader.GetInt64(1),
                reader.GetInt64(2),
                reader.GetString(3),
                reader.GetInt32(4),
                reader.GetFieldValue<DateTime>(5)));
        }

        return events;
    }

    /// <returns>True when processing must pause at this event.</returns>
    private async Task<bool> ProcessEventAsync(
        CapturedEvent capturedEvent,
        CancellationToken cancellationToken)
    {
        var route = await _ledger.GetRouteAsync(capturedEvent.ProductId, cancellationToken);
        if (route.Route == ProductRoute.Migrating)
        {
            return true;
        }

        if (route.Route == ProductRoute.Old || !_options.ApplyLateCapturedChanges)
        {
            return false;
        }

        var cutoff = await _ledger.GetCommittedCutoffAsync(
            capturedEvent.ProductId,
            cancellationToken);
        if (!cutoff.HasValue || capturedEvent.Seq <= cutoff.Value)
        {
            return false;
        }

        using var admission = await _writeGate.EnterAsync(cancellationToken);
        route = await _ledger.GetRouteAsync(capturedEvent.ProductId, cancellationToken);
        if (route.Route != ProductRoute.New)
        {
            return route.Route == ProductRoute.Migrating;
        }

        await ApplyToNewAsync(capturedEvent, route.BatchId, cancellationToken);
        return false;
    }

    private async Task ApplyToNewAsync(
        CapturedEvent capturedEvent,
        long? batchId,
        CancellationToken cancellationToken)
    {
        await using var lease = await _newPool.AcquireAsync(cancellationToken);
        await using var transaction = await lease.Connection.BeginTransactionAsync(cancellationToken);

        bool firstApplication;
        await using (var dedupe = new NpgsqlCommand(
            """
            INSERT INTO migration_applied_event(seq)
            VALUES (@seq)
            ON CONFLICT(seq) DO NOTHING
            RETURNING seq;
            """,
            lease.Connection,
            transaction))
        {
            dedupe.Parameters.AddWithValue("seq", capturedEvent.Seq);
            firstApplication = await dedupe.ExecuteScalarAsync(cancellationToken) is not null;
        }

        if (!firstApplication)
        {
            await transaction.RollbackAsync(cancellationToken);
            return;
        }

        await using (var update = new NpgsqlCommand(
            """
            UPDATE inventory
            SET stock_qty = stock_qty + @change,
                version = version + 1,
                updated_at = clock_timestamp()
            WHERE product_id = @product
              AND stock_qty + @change >= 0;
            """,
            lease.Connection,
            transaction))
        {
            update.Parameters.AddWithValue("change", capturedEvent.ChangeQty);
            update.Parameters.AddWithValue("product", capturedEvent.ProductId);
            await update.ExecuteNonQueryAsync(cancellationToken);
        }

        await using (var insertFlow = new NpgsqlCommand(
            """
            INSERT INTO deduction_flow(
                flow_id, product_id, direction, change_qty, created_at, shard_no)
            VALUES (@flow, @product, @direction, @change, @created, @shard)
            ON CONFLICT(flow_id, shard_no) DO NOTHING;
            """,
            lease.Connection,
            transaction))
        {
            insertFlow.Parameters.AddWithValue("flow", capturedEvent.FlowId);
            insertFlow.Parameters.AddWithValue("product", capturedEvent.ProductId);
            insertFlow.Parameters.AddWithValue("direction", capturedEvent.Direction);
            insertFlow.Parameters.AddWithValue("change", capturedEvent.ChangeQty);
            insertFlow.Parameters.AddWithValue("created", capturedEvent.OccurredAt);
            insertFlow.Parameters.AddWithValue(
                "shard",
                (short)(capturedEvent.ProductId % Math.Max(1, _options.ShardCount)));
            await insertFlow.ExecuteNonQueryAsync(cancellationToken);
        }

        await transaction.CommitAsync(cancellationToken);
        _logger.LogInformation(
            "Applied late legacy change seq {Seq} to new database for product {ProductId} (batch {BatchId})",
            capturedEvent.Seq,
            capturedEvent.ProductId,
            batchId);
    }

    private sealed record CapturedEvent(
        long Seq,
        long ProductId,
        long FlowId,
        string Direction,
        int ChangeQty,
        DateTime OccurredAt);
}
