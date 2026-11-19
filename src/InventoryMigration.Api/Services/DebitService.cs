using System.Diagnostics;
using InventoryMigration.Api.Domain;
using InventoryMigration.Api.Infrastructure;
using InventoryMigration.Api.Options;
using Microsoft.Extensions.Options;
using Npgsql;

namespace InventoryMigration.Api.Services;

public sealed class DebitService
{
    private readonly LedgerStore _ledger;
    private readonly DatabasePool _oldPool;
    private readonly DatabasePool _newPool;
    private readonly WriteGate _writeGate;
    private readonly MetricsRegistry _metrics;
    private readonly MigrationOptions _options;

    public DebitService(
        LedgerStore ledger,
        DatabasePools pools,
        WriteGate writeGate,
        MetricsRegistry metrics,
        IOptions<MigrationOptions> options)
    {
        _ledger = ledger;
        _oldPool = pools.Old;
        _newPool = pools.New;
        _writeGate = writeGate;
        _metrics = metrics;
        _options = options.Value;
    }

    public async Task<DebitResponse> DebitAsync(
        DebitRequest request,
        CancellationToken cancellationToken)
    {
        ValidateRequest(request);
        var stopwatch = Stopwatch.StartNew();
        using var admission = await _writeGate.EnterAsync(cancellationToken);
        var route = await _ledger.GetRouteAsync(request.ProductId, cancellationToken);
        _metrics.RecordRoute(route.Route);
        if (route.Route == ProductRoute.Migrating)
        {
            throw new ServiceException(
                ErrorCodes.Migrating,
                $"商品 {request.ProductId} 正在迁移切换中，请稍后重试",
                StatusCodes.Status409Conflict,
                request.ProductId);
        }

        var changeQty = request.Direction == "deduct"
            ? -(long)request.Quantity
            : request.Quantity;

        try
        {
            var result = route.Route == ProductRoute.Old
                ? await ExecuteChangeAsync(
                    _oldPool,
                    request,
                    changeQty,
                    shardNo: null,
                    cancellationToken)
                : await ExecuteChangeAsync(
                    _newPool,
                    request,
                    changeQty,
                    ShardFor(request.ProductId),
                    cancellationToken);
            return result with { Route = route.Route == ProductRoute.Old ? "old" : "new" };
        }
        finally
        {
            _metrics.RecordWriteLatency(stopwatch.Elapsed);
        }
    }

    public async Task<DebitResponse> ApplyDirectOldChangeAsync(
        DebitRequest request,
        CancellationToken cancellationToken)
    {
        ValidateRequest(request);
        var changeQty = request.Direction == "deduct"
            ? -(long)request.Quantity
            : request.Quantity;
        using var admission = await _writeGate.EnterAsync(cancellationToken);
        return await ExecuteChangeAsync(
            _oldPool,
            request,
            changeQty,
            shardNo: null,
            cancellationToken);
    }

    private async Task<DebitResponse> ExecuteChangeAsync(
        DatabasePool pool,
        DebitRequest request,
        long changeQty,
        short? shardNo,
        CancellationToken cancellationToken)
    {
        try
        {
            await using var lease = await pool.AcquireAsync(cancellationToken);
            await using var transaction = await lease.Connection.BeginTransactionAsync(cancellationToken);

            long flowId;
            await using (var sequence = new NpgsqlCommand(
                "SELECT nextval('flow_id_seq');",
                lease.Connection,
                transaction))
            {
                flowId = (long)(await sequence.ExecuteScalarAsync(cancellationToken)
                    ?? throw new InvalidOperationException("flow_id_seq returned no value."));
            }

            long balance;
            await using (var update = new NpgsqlCommand(
                """
                UPDATE inventory
                SET stock_qty = stock_qty + @change,
                    version = version + 1,
                    updated_at = clock_timestamp()
                WHERE product_id = @product
                  AND stock_qty + @change >= 0
                RETURNING stock_qty;
                """,
                lease.Connection,
                transaction))
            {
                update.Parameters.AddWithValue("change", changeQty);
                update.Parameters.AddWithValue("product", request.ProductId);
                var value = await update.ExecuteScalarAsync(cancellationToken);
                if (value is null or DBNull)
                {
                    throw await ClassifyMissingUpdateAsync(
                        lease.Connection,
                        transaction,
                        request.ProductId,
                        cancellationToken);
                }

                balance = Convert.ToInt64(value);
            }

            if (shardNo.HasValue)
            {
                await using var insertFlow = new NpgsqlCommand(
                    """
                    INSERT INTO deduction_flow(
                        flow_id, product_id, direction, change_qty, created_at, shard_no)
                    VALUES (@flow, @product, @direction, @change, clock_timestamp(), @shard);
                    """,
                    lease.Connection,
                    transaction);
                insertFlow.Parameters.AddWithValue("flow", flowId);
                insertFlow.Parameters.AddWithValue("product", request.ProductId);
                insertFlow.Parameters.AddWithValue("direction", request.Direction);
                insertFlow.Parameters.AddWithValue("change", changeQty);
                insertFlow.Parameters.AddWithValue("shard", shardNo.Value);
                await insertFlow.ExecuteNonQueryAsync(cancellationToken);
            }
            else
            {
                await using var insertFlow = new NpgsqlCommand(
                    """
                    INSERT INTO deduction_flow(
                        flow_id, product_id, direction, change_qty, created_at)
                    VALUES (@flow, @product, @direction, @change, clock_timestamp());
                    """,
                    lease.Connection,
                    transaction);
                insertFlow.Parameters.AddWithValue("flow", flowId);
                insertFlow.Parameters.AddWithValue("product", request.ProductId);
                insertFlow.Parameters.AddWithValue("direction", request.Direction);
                insertFlow.Parameters.AddWithValue("change", changeQty);
                await insertFlow.ExecuteNonQueryAsync(cancellationToken);
            }

            await transaction.CommitAsync(cancellationToken);
            return new DebitResponse(
                request.ProductId,
                request.Direction,
                request.Quantity,
                changeQty,
                balance,
                flowId,
                pool.IsOldDatabase ? "old" : "new");
        }
        catch (ServiceException)
        {
            throw;
        }
        catch (NpgsqlException exception)
        {
            throw new ServiceException(
                ErrorCodes.Connection,
                $"数据库写入失败: {exception.Message}",
                StatusCodes.Status503ServiceUnavailable,
                request.ProductId);
        }
    }

    private static async Task<ServiceException> ClassifyMissingUpdateAsync(
        NpgsqlConnection connection,
        NpgsqlTransaction transaction,
        long productId,
        CancellationToken cancellationToken)
    {
        await using var command = new NpgsqlCommand(
            "SELECT 1 FROM inventory WHERE product_id = @product;",
            connection,
            transaction);
        command.Parameters.AddWithValue("product", productId);
        var exists = await command.ExecuteScalarAsync(cancellationToken) is not null;
        return exists
            ? new ServiceException(
                ErrorCodes.Stock,
                $"商品 {productId} 库存不足",
                StatusCodes.Status409Conflict,
                productId)
            : new ServiceException(
                ErrorCodes.NotFound,
                $"商品 {productId} 不存在",
                StatusCodes.Status404NotFound,
                productId);
    }

    private static void ValidateRequest(DebitRequest request)
    {
        if (request.ProductId <= 0 || request.Quantity <= 0)
        {
            throw new ServiceException(
                ErrorCodes.BadRequest,
                "productId 与 quantity 必须为正整数",
                StatusCodes.Status400BadRequest,
                request.ProductId);
        }

        if (request.Direction is not ("deduct" or "restock"))
        {
            throw new ServiceException(
                ErrorCodes.BadRequest,
                "direction 必须为 deduct 或 restock",
                StatusCodes.Status400BadRequest,
                request.ProductId);
        }
    }

    private short ShardFor(long productId) =>
        (short)(productId % Math.Max(1, _options.ShardCount));
}
