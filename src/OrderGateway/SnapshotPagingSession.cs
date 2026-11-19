using System.Data;
using Microsoft.Data.SqlClient;

namespace OrderGateway;

/// <summary>
/// A single snapshot transaction held for an entire pagination walk, so every
/// page observes the same point-in-time visible set even while writers commit.
/// One leased connection stays dedicated to the session for its lifetime.
/// </summary>
public sealed class SnapshotPagingSession : IAsyncDisposable
{
    private readonly ConnectionGate.LeasedConnection _lease;
    private readonly SqlTransaction _transaction;
    private readonly GatewayOptions _options;
    private readonly MetricsCollector _metrics;
    private readonly KeyOrder _keyOrder;
    private readonly IAsyncDisposable _indexReadLease;

    private SnapshotPagingSession(
        ConnectionGate.LeasedConnection lease,
        SqlTransaction transaction,
        GatewayOptions options,
        MetricsCollector metrics,
        KeyOrder keyOrder,
        IAsyncDisposable indexReadLease)
    {
        _lease = lease;
        _transaction = transaction;
        _options = options;
        _metrics = metrics;
        _keyOrder = keyOrder;
        _indexReadLease = indexReadLease;
    }

    public static async Task<SnapshotPagingSession> BeginAsync(
        ConnectionGate gate,
        GatewayOptions options,
        MetricsCollector metrics,
        IndexChangeBarrier indexBarrier,
        KeyOrder keyOrder,
        CancellationToken cancellationToken)
    {
        var indexReadLease = await indexBarrier.EnterReadAsync(cancellationToken).ConfigureAwait(false);
        var lease = await gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        try
        {
            await using (var setup = lease.Connection.CreateCommand())
            {
                setup.CommandText = "SET TRANSACTION ISOLATION LEVEL SNAPSHOT;";
                await setup.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
            }

            var transaction = (SqlTransaction)await lease.Connection.BeginTransactionAsync(
                IsolationLevel.Snapshot, cancellationToken);
            return new SnapshotPagingSession(lease, transaction, options, metrics, keyOrder, indexReadLease);
        }
        catch
        {
            await lease.DisposeAsync();
            await indexReadLease.DisposeAsync();
            throw;
        }
    }

    public async Task<PageResult> ReadPageAsync(PageQuery query, CancellationToken cancellationToken)
    {
        var sw = System.Diagnostics.Stopwatch.StartNew();
        var repository = new QueryBuilder(_options);
        var (sql, parameters) = repository.Build(_keyOrder, query, forcedIndexName: null);

        try
        {
            await using var command = _lease.Connection.CreateCommand();
            command.Transaction = _transaction;
            command.CommandText = sql;
            command.CommandTimeout = (int)Math.Ceiling(_options.CommandTimeout.TotalSeconds);
            foreach (var parameter in parameters) command.Parameters.Add(parameter);

            var rows = new List<OrderRow>(query.PageSize + 1);
            await using (var reader = await command.ExecuteReaderAsync(cancellationToken).ConfigureAwait(false))
            {
                while (await reader.ReadAsync(cancellationToken).ConfigureAwait(false))
                {
                    rows.Add(new OrderRow(
                        reader.GetInt64(reader.GetOrdinal("OrderId")),
                        reader.GetInt32(reader.GetOrdinal("TenantId")),
                        reader.GetByte(reader.GetOrdinal("Status")),
                        reader.GetDecimal(reader.GetOrdinal("Amount")),
                        new DateTimeOffset(reader.GetDateTime(reader.GetOrdinal("UpdatedAt")), TimeSpan.Zero)));
                }
            }

            var dominant = await CapturePlanAsync(sql, parameters, cancellationToken);
            var expectedIndex = _keyOrder.ExpectedIndex(_options);
            bool matched = dominant.IndexName == expectedIndex && !dominant.IsTableScan;

            _metrics.RecordQuery(new QueryObservation(
                _keyOrder.AsLabel(), dominant.IndexName, dominant.AccessMethod, matched,
                sw.Elapsed.TotalMilliseconds, DateTimeOffset.UtcNow));

            if (!matched)
            {
                var context = new Dictionary<string, object?>
                {
                    ["tenantId"] = query.TenantId,
                    ["status"] = query.Status,
                    ["pageSize"] = query.PageSize,
                    ["keyOrder"] = _keyOrder.AsLabel(),
                    ["expectedIndex"] = expectedIndex,
                    ["actualIndex"] = dominant.IndexName,
                    ["accessMethod"] = dominant.AccessMethod,
                    ["sessionScoped"] = true,
                };
                _metrics.RecordError(GatewayError.PlanMismatch, context);
                throw new GatewayException(GatewayError.PlanMismatch,
                    $"Snapshot session expected {expectedIndex} but used {dominant.IndexName}", context);
            }

            bool hasMore = rows.Count > query.PageSize;
            if (hasMore) rows.RemoveAt(rows.Count - 1);
            PageCursor? nextCursor = null;
            if (hasMore && rows.Count > 0)
            {
                var last = rows[^1];
                nextCursor = _keyOrder == KeyOrder.Legacy
                    ? new LegacyCursor(last.OrderId)
                    : new NewCursor(last.UpdatedAt.UtcDateTime, last.OrderId);
            }
            return new PageResult(rows, nextCursor, hasMore, dominant.IndexName, dominant.AccessMethod);
        }
        catch (SqlException ex) when (IsDeadlock(ex))
        {
            var context = new Dictionary<string, object?>
            {
                ["tenantId"] = query.TenantId,
                ["status"] = query.Status,
                ["keyOrder"] = _keyOrder.AsLabel(),
                ["pageSize"] = query.PageSize,
                ["cursor"] = query.Cursor?.ToString(),
                ["sessionScoped"] = true,
            };
            _metrics.RecordError(GatewayError.Deadlock, context);
            throw new GatewayException(GatewayError.Deadlock, "Deadlock victim in snapshot session", context, ex);
        }
    }

    private async Task<PlanAccessPath> CapturePlanAsync(
        string sql,
        List<SqlParameter> parameters,
        CancellationToken cancellationToken)
    {
        await using var onCmd = _lease.Connection.CreateCommand();
        onCmd.Transaction = _transaction;
        onCmd.CommandText = "SET STATISTICS XML ON;";
        await onCmd.ExecuteNonQueryAsync(cancellationToken);

        try
        {
            await using var planCmd = _lease.Connection.CreateCommand();
            planCmd.Transaction = _transaction;
            planCmd.CommandText = sql;
            foreach (var p in parameters)
                planCmd.Parameters.Add(new SqlParameter(p.ParameterName, p.SqlDbType) { Value = p.Value });

            string? xml = null;
            await using var reader = await planCmd.ExecuteReaderAsync(cancellationToken);
            do
            {
                while (await reader.ReadAsync(cancellationToken))
                {
                    if (reader.GetName(0).Contains("Showplan", StringComparison.Ordinal))
                        xml = reader.GetString(0);
                }
            } while (await reader.NextResultAsync(cancellationToken));

            if (xml is not null) _metrics.RecordPlanXml(_keyOrder.AsLabel(), xml);
            return ExecutionPlanGuard.SelectDominantPath(
                ExecutionPlanGuard.ExtractOrderAccessPaths(xml!));
        }
        finally
        {
            await using var offCmd = _lease.Connection.CreateCommand();
            offCmd.Transaction = _transaction;
            offCmd.CommandText = "SET STATISTICS XML OFF;";
            await offCmd.ExecuteNonQueryAsync(cancellationToken);
        }
    }

    private static bool IsDeadlock(SqlException ex)
    {
        foreach (SqlError error in ex.Errors)
        {
            if (error.Number is 1205 or 3960 or 3961) return true;
        }
        return false;
    }

    public async ValueTask DisposeAsync()
    {
        try { await _transaction.RollbackAsync(); } catch { }
        await _lease.DisposeAsync();
        await _indexReadLease.DisposeAsync();
    }
}
