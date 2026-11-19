using System.Data;
using Microsoft.Data.SqlClient;

namespace OrderGateway;

/// <summary>
/// Data access for the cutover gateway. Every read:
///   1. runs under an explicit snapshot transaction so the page sees a stable
///      point-in-time visible set;
///   2. captures the actual XML showplan and enforces the expected index;
///   3. translates engine errors into the ticket's business error codes.
/// </summary>
public sealed class OrderRepository
{
    private readonly ConnectionGate _gate;
    private readonly GatewayOptions _options;
    private readonly MetricsCollector _metrics;
    private readonly QueryBuilder _queryBuilder;
    private readonly IndexChangeBarrier _indexBarrier;

    public OrderRepository(
        ConnectionGate gate,
        GatewayOptions options,
        MetricsCollector metrics,
        IndexChangeBarrier indexBarrier)
    {
        _gate = gate;
        _options = options;
        _metrics = metrics;
        _queryBuilder = new QueryBuilder(options);
        _indexBarrier = indexBarrier;
    }

    public Task<PageResult> ReadPageAsync(
        KeyOrder keyOrder,
        PageQuery query,
        CancellationToken cancellationToken = default)
        => ReadPageInternalAsync(keyOrder, query, forceIndexName: null, expectedIndexOverride: null,
            acquireTimeout: null, cancellationToken: cancellationToken);

    /// <summary>Test/probe hook that overrides only the connection-slot wait.</summary>
    public Task<PageResult> ReadPageWithAcquireTimeoutAsync(
        KeyOrder keyOrder,
        PageQuery query,
        TimeSpan acquireTimeout,
        CancellationToken cancellationToken = default)
        => ReadPageInternalAsync(keyOrder, query, forceIndexName: null, expectedIndexOverride: null,
            acquireTimeout: acquireTimeout, cancellationToken: cancellationToken);

    /// <summary>
    /// Negative-test hook: force a different access path so the plan guard has
    /// something real to reject. Never used by normal gateway traffic.
    /// </summary>
    public Task<PageResult> ReadPageWithForcedIndexAsync(
        KeyOrder keyOrder,
        PageQuery query,
        string forcedIndexName,
        CancellationToken cancellationToken = default)
        => ReadPageInternalAsync(keyOrder, query, forcedIndexName, expectedIndexOverride: null,
            acquireTimeout: null, cancellationToken: cancellationToken);

    /// <summary>
    /// Used by the rollback scenario where the physical topology intentionally
    /// changed: the caller states which index a key order is expected to use.
    /// </summary>
    public Task<PageResult> ReadPageWithExpectedIndexAsync(
        KeyOrder keyOrder,
        PageQuery query,
        string expectedIndexName,
        CancellationToken cancellationToken = default)
        => ReadPageInternalAsync(keyOrder, query, forceIndexName: null, expectedIndexName,
            allowFallbackWhenIndexMissing: true, acquireTimeout: null, cancellationToken);

    /// <summary>
    /// Holds an explicit snapshot read transaction open for a fixed hold time,
    /// simulating a page query still executing when index DDL starts.
    /// Schema-stability locks force DROP INDEX to wait; the read completes first.
    /// </summary>
    public async Task<int> ReadPageWhileHoldingSnapshotAsync(
        KeyOrder keyOrder,
        PageQuery query,
        TimeSpan holdTime,
        CancellationToken cancellationToken = default)
    {
        await using var indexReadLease = await _indexBarrier.EnterReadAsync(cancellationToken).ConfigureAwait(false);
        await using var lease = await _gate.LeaseAsync(cancellationToken).ConfigureAwait(false);
        var connection = lease.Connection;

        await using (var setup = connection.CreateCommand())
        {
            setup.CommandText = "SET TRANSACTION ISOLATION LEVEL SNAPSHOT;";
            await setup.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
        }

        await using var transaction = (SqlTransaction)await connection.BeginTransactionAsync(
            IsolationLevel.Snapshot, cancellationToken);
        try
        {
            var (sql, parameters) = _queryBuilder.Build(keyOrder, query, null);
            await using var command = connection.CreateCommand();
            command.Transaction = transaction;
            command.CommandText = sql;
            foreach (var parameter in parameters) command.Parameters.Add(parameter);

            int rows;
            await using (var reader = await command.ExecuteReaderAsync(cancellationToken).ConfigureAwait(false))
            {
                rows = 0;
                while (await reader.ReadAsync(cancellationToken).ConfigureAwait(false)) rows++;
            }

            var showplanXml = await CaptureActualPlanAsync(connection, transaction, sql, parameters, cancellationToken)
                .ConfigureAwait(false);
            var dominant = ExecutionPlanGuard.SelectDominantPath(
                ExecutionPlanGuard.ExtractOrderAccessPaths(showplanXml));
            var expectedIndex = keyOrder.ExpectedIndex(_options);
            bool matched = dominant.IndexName == expectedIndex && !dominant.IsTableScan;
            _metrics.RecordQuery(new QueryObservation(
                keyOrder.AsLabel(), dominant.IndexName, dominant.AccessMethod, matched, 0,
                DateTimeOffset.UtcNow));
            if (!matched)
            {
                var context = new Dictionary<string, object?>
                {
                    ["keyOrder"] = keyOrder.AsLabel(),
                    ["tenantId"] = query.TenantId,
                    ["status"] = query.Status,
                    ["pageSize"] = query.PageSize,
                    ["expectedIndex"] = expectedIndex,
                    ["actualIndex"] = dominant.IndexName,
                    ["accessMethod"] = dominant.AccessMethod,
                    ["inFlightDuringDdl"] = true,
                };
                _metrics.RecordError(GatewayError.PlanMismatch, context);
                throw new GatewayException(
                    GatewayError.PlanMismatch,
                    $"In-flight read used {dominant.IndexName}, expected {expectedIndex}", context);
            }

            await Task.Delay(holdTime, cancellationToken).ConfigureAwait(false);
            await transaction.CommitAsync(cancellationToken).ConfigureAwait(false);
            return rows;
        }
        catch
        {
            try { await transaction.RollbackAsync(); } catch { }
            throw;
        }
    }

    private async Task<PageResult> ReadPageInternalAsync(
        KeyOrder keyOrder,
        PageQuery query,
        string? forceIndexName,
        string? expectedIndexOverride,
        bool allowFallbackWhenIndexMissing = false,
        TimeSpan? acquireTimeout = null,
        CancellationToken cancellationToken = default)
    {
        var sw = System.Diagnostics.Stopwatch.StartNew();
        await using var indexReadLease = await _indexBarrier.EnterReadAsync(cancellationToken).ConfigureAwait(false);
        await using var lease = await _gate.LeaseAsync(acquireTimeout, cancellationToken).ConfigureAwait(false);
        var connection = lease.Connection;

        SqlTransaction? transaction = null;
        try
        {
            /* Probe topology before opening snapshot isolation. The probe uses
               the already-leased connection, so a rollback read can never open
               a seventh physical connection or bypass the configured cap. */
            var effectiveForce = forceIndexName;
            if (forceIndexName is null && allowFallbackWhenIndexMissing &&
                !await IndexExistsAsync(connection, keyOrder.ExpectedIndex(_options), cancellationToken)
                    .ConfigureAwait(false))
            {
                effectiveForce = string.Empty;
            }

            await using (var setup = connection.CreateCommand())
            {
                setup.CommandText = "SET TRANSACTION ISOLATION LEVEL SNAPSHOT;";
                await setup.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
            }

            transaction = (SqlTransaction)await connection.BeginTransactionAsync(
                IsolationLevel.Snapshot, cancellationToken);

            var (sql, parameters) = _queryBuilder.Build(keyOrder, query, effectiveForce);
            await using var command = connection.CreateCommand();
            command.Transaction = transaction;
            command.CommandText = sql;
            command.CommandTimeout = (int)Math.Ceiling(_options.CommandTimeout.TotalSeconds);
            foreach (var parameter in parameters) command.Parameters.Add(parameter);

            var rows = new List<OrderRow>(query.PageSize + 1);
            SqlDataReader? reader = null;
            try
            {
                reader = await command.ExecuteReaderAsync(cancellationToken).ConfigureAwait(false);
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
            finally
            {
                if (reader is not null) await reader.DisposeAsync().ConfigureAwait(false);
            }

            var showplanXml = await CaptureActualPlanAsync(connection, transaction, sql, parameters, cancellationToken)
                .ConfigureAwait(false);
            await transaction.CommitAsync(cancellationToken).ConfigureAwait(false);

            var allPaths = ExecutionPlanGuard.ExtractOrderAccessPaths(showplanXml);
            var dominant = ExecutionPlanGuard.SelectDominantPath(allPaths);
            var expectedIndex = expectedIndexOverride ?? keyOrder.ExpectedIndex(_options);
            bool matched = string.Equals(dominant.IndexName, expectedIndex, StringComparison.Ordinal)
                           && !dominant.IsTableScan;

            _metrics.RecordPlanXml(keyOrder.AsLabel(), showplanXml);
            _metrics.RecordQuery(new QueryObservation(
                keyOrder.AsLabel(),
                dominant.IndexName,
                dominant.AccessMethod,
                matched,
                sw.Elapsed.TotalMilliseconds,
                DateTimeOffset.UtcNow));

            if (!matched)
            {
                var context = new Dictionary<string, object?>
                {
                    ["tenantId"] = query.TenantId,
                    ["status"] = query.Status,
                    ["pageSize"] = query.PageSize,
                    ["cursor"] = query.Cursor?.ToString(),
                    ["keyOrder"] = keyOrder.AsLabel(),
                    ["expectedIndex"] = expectedIndex,
                    ["actualIndex"] = dominant.IndexName,
                    ["accessMethod"] = dominant.AccessMethod,
                    ["allAccessPaths"] = allPaths.Select(p => $"{p.IndexName}:{p.AccessMethod}").ToArray(),
                };
                _metrics.RecordError(GatewayError.PlanMismatch, context);
                throw new GatewayException(
                    GatewayError.PlanMismatch,
                    $"Query for {keyOrder.AsLabel()} used {dominant.IndexName}/{dominant.AccessMethod}, expected {expectedIndex}",
                    context);
            }

            bool hasMore = rows.Count > query.PageSize;
            if (hasMore) rows.RemoveAt(rows.Count - 1);

            PageCursor? nextCursor = null;
            if (hasMore && rows.Count > 0)
            {
                var last = rows[^1];
                nextCursor = keyOrder switch
                {
                    KeyOrder.Legacy => new LegacyCursor(last.OrderId),
                    KeyOrder.New => new NewCursor(last.UpdatedAt.UtcDateTime, last.OrderId),
                    _ => nextCursor,
                };
            }

            return new PageResult(rows, nextCursor, hasMore, dominant.IndexName, dominant.AccessMethod);
        }
        catch (GatewayException)
        {
            if (transaction is not null) await RollbackQuietlyAsync(transaction);
            throw;
        }
        catch (SqlException ex) when (IsDeadlock(ex))
        {
            if (transaction is not null) await RollbackQuietlyAsync(transaction);
            var context = new Dictionary<string, object?>
            {
                ["tenantId"] = query.TenantId,
                ["status"] = query.Status,
                ["keyOrder"] = keyOrder.AsLabel(),
                ["pageSize"] = query.PageSize,
                ["cursor"] = query.Cursor?.ToString(),
                ["sqlErrorNumber"] = ex.Number,
            };
            _metrics.RecordError(GatewayError.Deadlock, context);
            throw new GatewayException(GatewayError.Deadlock, "Deadlock victim during cutover window", context, ex);
        }
        catch (SqlException ex) when (ex.Number == -2)
        {
            if (transaction is not null) await RollbackQuietlyAsync(transaction);
            var context = new Dictionary<string, object?>
            {
                ["tenantId"] = query.TenantId,
                ["status"] = query.Status,
                ["keyOrder"] = keyOrder.AsLabel(),
                ["commandTimeoutSec"] = _options.CommandTimeout.TotalSeconds,
            };
            _metrics.RecordError(GatewayError.CommandTimeout, context);
            throw new GatewayException(GatewayError.CommandTimeout, "Command timeout", context, ex);
        }
        catch
        {
            if (transaction is not null) await RollbackQuietlyAsync(transaction);
            throw;
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

    private static async Task<bool> IndexExistsAsync(
        SqlConnection connection,
        string indexName,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.CommandText = """
            SELECT COUNT_BIG(1)
            FROM sys.indexes i
            JOIN sys.tables t ON t.object_id = i.object_id
            WHERE t.name = N'Orders' AND i.name = @IndexName;
            """;
        command.Parameters.Add(new SqlParameter("@IndexName", SqlDbType.NVarChar, 128) { Value = indexName });
        var result = await command.ExecuteScalarAsync(cancellationToken).ConfigureAwait(false);
        return Convert.ToInt64(result) > 0;
    }

    private static async Task RollbackQuietlyAsync(SqlTransaction transaction)
    {
        try { await transaction.RollbackAsync(); }
        catch { /* snapshot txn may already be doomed */ }
    }

    private async Task<string> CaptureActualPlanAsync(
        SqlConnection connection,
        SqlTransaction transaction,
        string sql,
        IEnumerable<SqlParameter> parameters,
        CancellationToken cancellationToken)
    {
        await using var onCommand = connection.CreateCommand();
        onCommand.Transaction = transaction;
        onCommand.CommandText = "SET STATISTICS XML ON;";
        await onCommand.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);

        try
        {
            await using var planCommand = connection.CreateCommand();
            planCommand.Transaction = transaction;
            planCommand.CommandText = sql;
            planCommand.CommandTimeout = (int)Math.Ceiling(_options.CommandTimeout.TotalSeconds);
            foreach (var parameter in parameters)
            {
                planCommand.Parameters.Add(new SqlParameter(parameter.ParameterName, parameter.SqlDbType)
                {
                    Value = parameter.Value
                });
            }

            string? showplanXml = null;
            await using var reader = await planCommand.ExecuteReaderAsync(cancellationToken).ConfigureAwait(false);
            do
            {
                while (await reader.ReadAsync(cancellationToken).ConfigureAwait(false))
                {
                    if (reader.GetName(0).Equals("Microsoft SQL Server 2005 XML Showplan", StringComparison.Ordinal))
                        showplanXml = reader.GetString(0);
                }
            } while (await reader.NextResultAsync(cancellationToken).ConfigureAwait(false));

            return showplanXml ?? throw new InvalidOperationException("Actual showplan XML was not returned.");
        }
        finally
        {
            await using var offCommand = connection.CreateCommand();
            offCommand.Transaction = transaction;
            offCommand.CommandText = "SET STATISTICS XML OFF;";
            await offCommand.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
        }
    }
}
