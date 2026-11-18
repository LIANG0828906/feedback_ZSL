using System.Diagnostics;
using System.Globalization;
using System.Text.RegularExpressions;
using Microsoft.Data.Sqlite;
using SnapshotGateway.Configuration;
using SnapshotGateway.Domain;
using SnapshotGateway.Infrastructure;

namespace SnapshotGateway.Services;

public sealed class OrderQueryService
{
    public const string OldIndexName = "idx_old_status_orderid";
    public const string NewIndexName = "idx_new_tenant_status_updated_orderid";
    private static readonly Regex IndexRegex = new(
        @"USING\s+(?:COVERING\s+)?INDEX\s+([A-Za-z0-9_]+)",
        RegexOptions.IgnoreCase | RegexOptions.Compiled);
    private static readonly Regex TableScanRegex = new(
        @"^\s*SCAN\s+orders\b",
        RegexOptions.IgnoreCase | RegexOptions.Compiled);

    private readonly GatewayOptions _options;
    private readonly SqliteConnectionFactory _factory;
    private readonly ReplicaManager _replicas;
    private readonly SnapshotCoordinator _coordinator;
    private readonly GatewayMetrics _metrics;

    public OrderQueryService(
        GatewayOptions options,
        SqliteConnectionFactory factory,
        ReplicaManager replicas,
        SnapshotCoordinator coordinator,
        GatewayMetrics metrics)
    {
        _options = options;
        _factory = factory;
        _replicas = replicas;
        _coordinator = coordinator;
        _metrics = metrics;
    }

    public static string ExpectedIndexFor(string keyOrder) =>
        keyOrder.Equals(KeyOrders.New, StringComparison.OrdinalIgnoreCase)
            ? NewIndexName
            : OldIndexName;

    public async Task<QueryResult> QueryAsync(
        QueryFilter input,
        CancellationToken cancellationToken = default)
    {
        var stopwatch = Stopwatch.StartNew();
        Stopwatch? executionStopwatch = null;
        var successful = false;
        QueryFilter? filter = null;
        try
        {
            filter = Normalize(input);
            var keyOrderIndex = filter.IndexHintOverride ?? ExpectedIndexFor(filter.KeyOrder);
            var expectedIndex = filter.ExpectedIndexOverride ?? keyOrderIndex;
            var decision = await _coordinator.EnsureConsistentAsync(cancellationToken);
            if (!decision.Consistent)
            {
                _metrics.IncrementSnapshotLag();
                RecordFailure(
                    filter,
                    ErrorCodes.SnapshotLag,
                    expectedIndex,
                    decision.VersionsBefore.Select(version => $"replica-{version.ReplicaId}:v{version.SnapshotVersion}")
                        .ToArray(),
                    $"Primary snapshot v{decision.PrimaryVersion}; replica versions were " +
                    string.Join(",", decision.VersionsBefore.Select(version => version.SnapshotVersion)) +
                    $"; catch-up attempted={decision.CatchupAttempted}.");
                stopwatch.Stop();
                return new QueryResult
                {
                    Success = false,
                    ErrorCode = ErrorCodes.SnapshotLag,
                    Message =
                        "Replica snapshots are not aligned. Catch-up was triggered; retry after it completes.",
                    KeyOrder = filter.KeyOrder,
                    ExpectedIndex = expectedIndex,
                    PrimarySnapshotVersion = decision.PrimaryVersion,
                    SnapshotVersion = decision.PrimaryVersion,
                    Filter = filter,
                    ReplicaVersions = decision.VersionsBefore,
                    Details = new
                    {
                        keyOrder = filter.KeyOrder,
                        filter = new
                        {
                            filter.TenantId,
                            filter.Status,
                            filter.Page,
                            filter.PageSize
                        },
                        primaryVersion = decision.PrimaryVersion,
                        versionsBefore = decision.VersionsBefore,
                        versionsAfter = decision.VersionsAfter,
                        catchupAttempted = decision.CatchupAttempted,
                        catchupCompleted = decision.CatchupCompleted,
                        catchupDurationMs = Round(decision.CatchupDurationMs)
                    },
                    DurationMs = Round(stopwatch.Elapsed.TotalMilliseconds)
                };
            }

            executionStopwatch = Stopwatch.StartNew();
            var replicaId = filter.ReplicaId ?? _replicas.NextReplicaId();
            var replica = _replicas.GetReplica(replicaId);
            await using var replicaLock = await _replicas.AcquireReplicaLockAsync(
                replicaId, cancellationToken);
            await using var lease = await _factory.OpenAsync(
                replica.DatabasePath, readOnly: true, cancellationToken);

            var connection = lease.Connection;
            await using var transaction = connection.BeginTransaction(
                System.Data.IsolationLevel.Serializable,
                deferred: true);

            var replicaVersion = await ReadReplicaVersionAsync(
                connection, transaction, cancellationToken);
            if (replicaVersion != decision.PrimaryVersion)
            {
                _metrics.IncrementSnapshotLag();
                RecordFailure(
                    filter,
                    ErrorCodes.SnapshotLag,
                    expectedIndex,
                    new[] { $"replica-{replicaId}:v{replicaVersion}" },
                    $"Expected snapshot v{decision.PrimaryVersion}, selected replica was v{replicaVersion}.");
                stopwatch.Stop();
                return new QueryResult
                {
                    Success = false,
                    ErrorCode = ErrorCodes.SnapshotLag,
                    Message = "Replica changed snapshot between coordination and pagination.",
                    KeyOrder = filter.KeyOrder,
                    ExpectedIndex = expectedIndex,
                    PrimarySnapshotVersion = decision.PrimaryVersion,
                    SnapshotVersion = replicaVersion,
                    ReplicaId = replicaId,
                    Filter = filter,
                    ReplicaVersions = decision.VersionsAfter,
                    Details = new
                    {
                        expectedSnapshotVersion = decision.PrimaryVersion,
                        actualSnapshotVersion = replicaVersion
                    },
                    DurationMs = Round(stopwatch.Elapsed.TotalMilliseconds)
                };
            }

            var sql = BuildSql(filter, keyOrderIndex);
            var countPlan = await ExplainAsync(
                connection, transaction, sql.CountSql, filter, includePaging: false,
                statement: "count", cancellationToken);
            var dataPlan = await ExplainAsync(
                connection, transaction, sql.PageSql, filter, includePaging: true,
                statement: "data", cancellationToken);
            var analysis = AnalyzePlan(expectedIndex, countPlan, dataPlan);

            var total = await ExecuteCountAsync(
                connection, transaction, sql.CountSql, filter, cancellationToken);
            var offset = checked((long)(filter.Page - 1) * filter.PageSize);
            var items = await ExecutePageAsync(
                connection, transaction, sql.PageSql, filter, cancellationToken);
            await transaction.CommitAsync(cancellationToken);
            executionStopwatch.Stop();
            stopwatch.Stop();

            var executionDurationMs = Round(executionStopwatch.Elapsed.TotalMilliseconds);
            var totalDurationMs = Round(stopwatch.Elapsed.TotalMilliseconds);
            var plan = BuildPlanInfo(analysis, executionDurationMs);
            RecordMeasurement(filter.KeyOrder, plan, analysis);

            if (!analysis.Matched)
            {
                _metrics.IncrementPlanMismatch();
                RecordFailure(
                    filter,
                    ErrorCodes.PlanMismatch,
                    expectedIndex,
                    analysis.ActualIndexes,
                    string.Join(" | ", analysis.MismatchReasons));
                return new QueryResult
                {
                    Success = false,
                    ErrorCode = ErrorCodes.PlanMismatch,
                    Message = "EXPLAIN QUERY PLAN did not match the expected key-order index.",
                    KeyOrder = filter.KeyOrder,
                    ExpectedIndex = expectedIndex,
                    ActualIndex = plan.ActualIndexes.FirstOrDefault(),
                    AccessMethod = plan.AccessMethod,
                    PrimarySnapshotVersion = decision.PrimaryVersion,
                    SnapshotVersion = replicaVersion,
                    ReplicaId = replicaId,
                    Filter = filter,
                    Plan = plan,
                    ReplicaVersions = decision.VersionsAfter,
                    Details = new
                    {
                        keyOrder = filter.KeyOrder,
                        filter = new
                        {
                            filter.TenantId,
                            filter.Status,
                            filter.Page,
                            filter.PageSize
                        },
                        expectedIndex,
                        actualIndexes = analysis.ActualIndexes,
                        reasons = analysis.MismatchReasons,
                        planLines = plan.Lines
                    },
                    DurationMs = totalDurationMs,
                    ExecutionDurationMs = executionDurationMs
                };
            }

            _metrics.IncrementIndexHit(filter.KeyOrder);
            successful = true;
            var warnings = plan.WithinBudget
                ? Array.Empty<string>()
                : new[]
                {
                    $"{ErrorCodes.PerformanceBudget}: query duration {executionDurationMs:F2}ms exceeded " +
                    $"{_options.PerfThresholdMs:F0}ms."
                };
            return new QueryResult
            {
                Success = true,
                KeyOrder = filter.KeyOrder,
                ExpectedIndex = expectedIndex,
                ActualIndex = plan.ActualIndexes.FirstOrDefault(),
                AccessMethod = plan.AccessMethod,
                PrimarySnapshotVersion = decision.PrimaryVersion,
                SnapshotVersion = replicaVersion,
                ReplicaId = replicaId,
                Filter = filter,
                Page = new PagePayload
                {
                    Page = filter.Page,
                    PageSize = filter.PageSize,
                    Offset = offset,
                    TotalCount = total,
                    Items = items
                },
                Plan = plan,
                ReplicaVersions = decision.VersionsAfter,
                Warnings = warnings,
                DurationMs = totalDurationMs,
                ExecutionDurationMs = executionDurationMs
            };
        }
        catch (GatewayException ex)
        {
            if (ex.Code == ErrorCodes.ConnectionLimit)
            {
                _metrics.IncrementConnectionLimit();
            }

            stopwatch.Stop();
            if (filter is not null)
            {
                RecordFailure(
                    filter,
                    ex.Code,
                    filter.ExpectedIndexOverride ?? ExpectedIndexFor(filter.KeyOrder),
                    Array.Empty<string>(),
                    ex.Message);
            }

            return FailureResult(filter, ex, stopwatch.Elapsed.TotalMilliseconds);
        }
        catch (SqliteException ex) when (filter is not null && IsMissingExpectedIndex(ex))
        {
            executionStopwatch?.Stop();
            stopwatch.Stop();
            _metrics.IncrementPlanMismatch();
            var executionDurationMs = executionStopwatch is null
                ? Round(stopwatch.Elapsed.TotalMilliseconds)
                : Round(executionStopwatch.Elapsed.TotalMilliseconds);
            var totalDurationMs = Round(stopwatch.Elapsed.TotalMilliseconds);
            var expectedIndex = filter.ExpectedIndexOverride ?? ExpectedIndexFor(filter.KeyOrder);
            var plan = new QueryPlanInfo(
                expectedIndex,
                Array.Empty<string>(),
                "NONE",
                Matched: false,
                NoTableScan: true,
                NoTemporarySort: true,
                Array.Empty<PlanLine>(),
                new[] { ex.Message },
                _options.PerfThresholdMs,
                executionDurationMs,
                executionDurationMs <= _options.PerfThresholdMs,
                executionDurationMs <= _options.PerfThresholdMs ? null : ErrorCodes.PerformanceBudget);
            _metrics.RecordMeasurement(new PerformanceMeasurement(
                filter.KeyOrder,
                expectedIndex,
                "NONE",
                "NONE",
                executionDurationMs,
                _options.PerfThresholdMs,
                executionDurationMs <= _options.PerfThresholdMs,
                Array.Empty<string>(),
                DateTimeOffset.UtcNow));
            RecordFailure(
                filter,
                ErrorCodes.PlanMismatch,
                expectedIndex,
                Array.Empty<string>(),
                ex.Message);
            return new QueryResult
            {
                Success = false,
                ErrorCode = ErrorCodes.PlanMismatch,
                Message = "The expected index is unavailable; SQLite could not produce a query plan.",
                KeyOrder = filter.KeyOrder,
                ExpectedIndex = expectedIndex,
                ActualIndex = null,
                AccessMethod = "NONE",
                Filter = filter,
                Plan = plan,
                Details = new
                {
                    keyOrder = filter.KeyOrder,
                    filter = new
                    {
                        filter.TenantId,
                        filter.Status,
                        filter.Page,
                        filter.PageSize
                    },
                    expectedIndex,
                    actualIndexes = Array.Empty<string>(),
                    reasons = new[] { ex.Message },
                    planLines = Array.Empty<PlanLine>()
                },
                DurationMs = totalDurationMs,
                ExecutionDurationMs = executionDurationMs
            };
        }
        catch (SqliteException ex)
        {
            stopwatch.Stop();
            return FailureResult(
                filter,
                new GatewayException(
                    ErrorCodes.Database,
                    $"SQLite query failed: {ex.Message}",
                    new { sqliteErrorCode = ex.SqliteErrorCode },
                    StatusCodes.Status500InternalServerError),
                stopwatch.Elapsed.TotalMilliseconds);
        }
        catch (OverflowException ex)
        {
            stopwatch.Stop();
            return FailureResult(
                filter,
                new GatewayException(
                    ErrorCodes.Validation,
                    ex.Message,
                    null,
                    StatusCodes.Status400BadRequest),
                stopwatch.Elapsed.TotalMilliseconds);
        }
        finally
        {
            _metrics.IncrementQuery(successful);
        }
    }

    private QueryFilter Normalize(QueryFilter input)
    {
        var keyOrder = (input.KeyOrder ?? string.Empty).Trim().ToLowerInvariant();
        keyOrder = keyOrder switch
        {
            "old" or "status_orderid" or "status+orderid" => KeyOrders.Old,
            "new" or "tenant_status_updated" or
                "tenantid+status+updatedat+orderid" => KeyOrders.New,
            _ => throw new GatewayException(
                ErrorCodes.Validation,
                "keyOrder must be 'old' or 'new'.",
                new { keyOrder = input.KeyOrder },
                StatusCodes.Status400BadRequest)
        };

        var tenantId = (input.TenantId ?? string.Empty).Trim();
        var status = (input.Status ?? string.Empty).Trim().ToUpperInvariant();
        if (tenantId.Length == 0)
        {
            throw new GatewayException(
                ErrorCodes.Validation,
                "tenantId is required.",
                null,
                StatusCodes.Status400BadRequest);
        }

        if (status.Length == 0)
        {
            throw new GatewayException(
                ErrorCodes.Validation,
                "status is required.",
                null,
                StatusCodes.Status400BadRequest);
        }

        if (input.Page < 1)
        {
            throw new GatewayException(
                ErrorCodes.Validation,
                "page must be at least 1.",
                new { page = input.Page },
                StatusCodes.Status400BadRequest);
        }

        if (input.PageSize < 1 || input.PageSize > _options.MaxPageSize)
        {
            throw new GatewayException(
                ErrorCodes.Validation,
                $"pageSize must be between 1 and {_options.MaxPageSize}.",
                new { pageSize = input.PageSize, maxPageSize = _options.MaxPageSize },
                StatusCodes.Status400BadRequest);
        }

        if (input.ReplicaId is < 1)
        {
            throw new GatewayException(
                ErrorCodes.Validation,
                "replica must be a positive replica id.",
                new { replica = input.ReplicaId },
                StatusCodes.Status400BadRequest);
        }

        return new QueryFilter
        {
            TenantId = tenantId,
            Status = status,
            KeyOrder = keyOrder,
            Page = input.Page,
            PageSize = input.PageSize,
            ReplicaId = input.ReplicaId,
            ExpectedIndexOverride = input.ExpectedIndexOverride,
            IndexHintOverride = input.IndexHintOverride
        };
    }

    private static (string CountSql, string PageSql) BuildSql(
        QueryFilter filter,
        string expectedIndex)
    {
        var orderBy = filter.KeyOrder == KeyOrders.Old
            ? "Status ASC, OrderId ASC"
            : "TenantId ASC, Status ASC, UpdatedAt ASC, OrderId ASC";
        var countSql = $"""
            SELECT COUNT(*)
            FROM orders INDEXED BY {expectedIndex}
            WHERE TenantId=$tenant AND Status=$status;
            """;
        var pageSql = $"""
            SELECT OrderId, TenantId, Status, Amount, UpdatedAt, CreatedAt, LastWriteToken
            FROM orders INDEXED BY {expectedIndex}
            WHERE TenantId=$tenant AND Status=$status
            ORDER BY {orderBy}
            LIMIT $limit OFFSET $offset;
            """;
        return (countSql, pageSql);
    }

    private static async Task<long> ReadReplicaVersionAsync(
        SqliteConnection connection,
        SqliteTransaction transaction,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.Transaction = transaction;
        command.CommandText =
            "SELECT CAST(value AS INTEGER) FROM app_metadata WHERE key='data_snapshot_version';";
        return Convert.ToInt64(
            await command.ExecuteScalarAsync(cancellationToken),
            CultureInfo.InvariantCulture);
    }

    private static async Task<List<PlanLine>> ExplainAsync(
        SqliteConnection connection,
        SqliteTransaction transaction,
        string sql,
        QueryFilter filter,
        bool includePaging,
        string statement,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.Transaction = transaction;
        command.CommandText = "EXPLAIN QUERY PLAN " + sql;
        AddFilterParameters(command, filter);
        if (includePaging)
        {
            command.Parameters.AddWithValue("$limit", filter.PageSize);
            command.Parameters.AddWithValue(
                "$offset", checked((long)(filter.Page - 1) * filter.PageSize));
        }

        var lines = new List<PlanLine>();
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            var id = reader.GetInt32(0);
            var parent = reader.GetInt32(1);
            var detail = reader.GetString(3);
            var indexMatch = IndexRegex.Match(detail);
            var indexName = indexMatch.Success ? indexMatch.Groups[1].Value : null;
            var trimmed = detail.TrimStart();
            var accessMethod = trimmed.StartsWith("SEARCH", StringComparison.OrdinalIgnoreCase)
                ? "SEARCH"
                : trimmed.StartsWith("SCAN", StringComparison.OrdinalIgnoreCase)
                    ? "SCAN"
                    : "OTHER";
            var fullTableScan = TableScanRegex.IsMatch(detail) && indexName is null;
            var tempBTree = detail.Contains("TEMP B-TREE", StringComparison.OrdinalIgnoreCase);
            lines.Add(new PlanLine(
                statement,
                id,
                parent,
                detail,
                accessMethod,
                indexName,
                fullTableScan,
                tempBTree));
        }

        return lines;
    }

    private static async Task<long> ExecuteCountAsync(
        SqliteConnection connection,
        SqliteTransaction transaction,
        string sql,
        QueryFilter filter,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.Transaction = transaction;
        command.CommandText = sql;
        AddFilterParameters(command, filter);
        return Convert.ToInt64(
            await command.ExecuteScalarAsync(cancellationToken),
            CultureInfo.InvariantCulture);
    }

    private static async Task<IReadOnlyList<OrderRow>> ExecutePageAsync(
        SqliteConnection connection,
        SqliteTransaction transaction,
        string sql,
        QueryFilter filter,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.Transaction = transaction;
        command.CommandText = sql;
        AddFilterParameters(command, filter);
        command.Parameters.AddWithValue("$limit", filter.PageSize);
        command.Parameters.AddWithValue(
            "$offset", checked((long)(filter.Page - 1) * filter.PageSize));

        var items = new List<OrderRow>(filter.PageSize);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            items.Add(new OrderRow(
                reader.GetInt64(0),
                reader.GetString(1),
                reader.GetString(2),
                reader.GetInt64(3),
                reader.GetString(4),
                reader.GetString(5),
                reader.IsDBNull(6) ? null : reader.GetString(6)));
        }

        return items;
    }

    private static void AddFilterParameters(SqliteCommand command, QueryFilter filter)
    {
        command.Parameters.AddWithValue("$tenant", filter.TenantId);
        command.Parameters.AddWithValue("$status", filter.Status);
    }

    private static PlanAnalysis AnalyzePlan(
        string expectedIndex,
        IReadOnlyList<PlanLine> countPlan,
        IReadOnlyList<PlanLine> dataPlan)
    {
        var all = countPlan.Concat(dataPlan).ToArray();
        var actualIndexes = all
            .Select(line => line.IndexName)
            .Where(name => !string.IsNullOrWhiteSpace(name))
            .Cast<string>()
            .Distinct(StringComparer.OrdinalIgnoreCase)
            .ToArray();
        var reasons = new List<string>();
        var dataLines = dataPlan.ToArray();
        var dataExpected = dataLines.FirstOrDefault(
            line => string.Equals(line.IndexName, expectedIndex, StringComparison.OrdinalIgnoreCase));
        if (dataExpected is null)
        {
            reasons.Add($"Data query did not use expected index {expectedIndex}.");
        }
        else if (dataExpected.AccessMethod != "SEARCH")
        {
            reasons.Add($"Data query access method was {dataExpected.AccessMethod}, expected SEARCH.");
        }

        if (!countPlan.Any(line =>
                string.Equals(line.IndexName, expectedIndex, StringComparison.OrdinalIgnoreCase)))
        {
            reasons.Add($"Count query did not use expected index {expectedIndex}.");
        }

        if (all.Any(line => line.FullTableScan))
        {
            reasons.Add("A plan line performs a full table scan of orders.");
        }

        if (dataLines.Any(line => line.UsesTempBTree))
        {
            reasons.Add("The data query requires a temporary B-tree sort.");
        }

        var accessMethod = dataExpected?.AccessMethod
            ?? dataLines.FirstOrDefault()?.AccessMethod
            ?? "NONE";
        return new PlanAnalysis(
            all,
            actualIndexes,
            accessMethod,
            Matched: reasons.Count == 0,
            NoTableScan: all.All(line => !line.FullTableScan),
            NoTemporarySort: dataLines.All(line => !line.UsesTempBTree),
            reasons)
        {
            ExpectedIndex = expectedIndex
        };
    }

    private QueryPlanInfo BuildPlanInfo(PlanAnalysis analysis, double durationMs)
    {
        var withinBudget = durationMs <= _options.PerfThresholdMs;
        return new QueryPlanInfo(
            analysis.ExpectedIndex,
            analysis.ActualIndexes,
            analysis.AccessMethod,
            analysis.Matched,
            analysis.NoTableScan,
            analysis.NoTemporarySort,
            analysis.Lines,
            analysis.MismatchReasons,
            _options.PerfThresholdMs,
            durationMs,
            withinBudget,
            withinBudget ? null : ErrorCodes.PerformanceBudget);
    }

    private void RecordMeasurement(
        string keyOrder,
        QueryPlanInfo plan,
        PlanAnalysis analysis)
    {
        _metrics.RecordMeasurement(new PerformanceMeasurement(
            keyOrder,
            analysis.ExpectedIndex,
            plan.ActualIndexes.FirstOrDefault() ?? "NONE",
            plan.AccessMethod,
            plan.DurationMs,
            plan.ThresholdMs,
            plan.WithinBudget,
            plan.Lines.Select(line => line.Detail).ToArray(),
            DateTimeOffset.UtcNow));
    }

    private void RecordFailure(
        QueryFilter filter,
        string errorCode,
        string? expectedIndex,
        IReadOnlyList<string> actualIndexes,
        string detail)
    {
        _metrics.RecordFailure(new QueryFailureRecord(
            errorCode,
            filter.KeyOrder,
            filter.TenantId,
            filter.Status,
            filter.Page,
            filter.PageSize,
            expectedIndex,
            actualIndexes,
            detail,
            DateTimeOffset.UtcNow));
    }

    private static QueryResult FailureResult(
        QueryFilter? filter,
        GatewayException exception,
        double durationMs)
    {
        return new QueryResult
        {
            Success = false,
            ErrorCode = exception.Code,
            Message = exception.Message,
            KeyOrder = filter?.KeyOrder ?? string.Empty,
            ExpectedIndex = filter is null
                ? string.Empty
                : ExpectedIndexFor(filter.KeyOrder),
            Filter = filter,
            Details = exception.Details,
            DurationMs = Round(durationMs)
        };
    }

    private static double Round(double value) => Math.Round(value, 3);

    private static bool IsMissingExpectedIndex(SqliteException exception) =>
        exception.SqliteErrorCode == 1
        && (exception.Message.Contains("no such index", StringComparison.OrdinalIgnoreCase)
            || exception.Message.Contains("no query solution", StringComparison.OrdinalIgnoreCase));

    private sealed record PlanAnalysis(
        IReadOnlyList<PlanLine> Lines,
        IReadOnlyList<string> ActualIndexes,
        string AccessMethod,
        bool Matched,
        bool NoTableScan,
        bool NoTemporarySort,
        IReadOnlyList<string> MismatchReasons)
    {
        public string ExpectedIndex { get; init; } = string.Empty;
    }
}
