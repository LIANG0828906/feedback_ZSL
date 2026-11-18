using System.Globalization;
using System.Text.Json;
using SnapshotGateway.Configuration;
using SnapshotGateway.Domain;
using SnapshotGateway.Infrastructure;

namespace SnapshotGateway.Services;

public sealed class ReportService
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        WriteIndented = true,
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase
    };

    private readonly GatewayOptions _options;
    private readonly SqliteConnectionFactory _factory;
    private readonly GatewayMetrics _metrics;
    private readonly ReplicaManager _replicas;
    private readonly IndexStateService _indexes;

    public ReportService(
        GatewayOptions options,
        SqliteConnectionFactory factory,
        GatewayMetrics metrics,
        ReplicaManager replicas,
        IndexStateService indexes)
    {
        _options = options;
        _factory = factory;
        _metrics = metrics;
        _replicas = replicas;
        _indexes = indexes;
    }

    public async Task<object> BuildAsync(
        object? selfTest = null,
        CancellationToken cancellationToken = default)
    {
        var state = await _indexes.GetStateAsync(cancellationToken);
        var audit = await _indexes.GetAuditAsync(200, cancellationToken);
        var replicaVersions = await _replicas.ReadAllVersionsAsync(cancellationToken);
        var metrics = _metrics.Snapshot();
        var sqliteVersion = await ReadSqliteVersionAsync(cancellationToken);
        var p95ByOrder = metrics.Measurements
            .GroupBy(measurement => measurement.KeyOrder, StringComparer.OrdinalIgnoreCase)
            .ToDictionary(
                group => group.Key,
                group => new
                {
                    sampleCount = group.Count(),
                    p95Ms = P95(group.Select(measurement => measurement.DurationMs)),
                    thresholdMs = _options.PerfThresholdMs,
                    passed = P95(group.Select(measurement => measurement.DurationMs))
                             <= _options.PerfThresholdMs
                },
                StringComparer.OrdinalIgnoreCase);
        var p95WithinBudget = p95ByOrder.Values.All(group => group.passed);
        var performanceBudgetCount = Math.Max(
            metrics.PerformanceBudgetCount,
            p95WithinBudget ? 0 : 1);

        return new
        {
            generatedAtUtc = DateTimeOffset.UtcNow,
            topology = new
            {
                mode = "local-simulated-single-primary-three-read-replicas",
                primary = new
                {
                    databasePath = _options.PrimaryDatabasePath,
                    role = "PRIMARY"
                },
                replicas = _replicas.Replicas.Select(replica => new
                {
                    replica.Id,
                    replica.Name,
                    replica.DatabasePath,
                    role = "READ_REPLICA"
                }),
                sqliteVersion,
                maxSqliteConnections = _options.MaxConnections
            },
            snapshot = new
            {
                finalVersion = state.DataSnapshotVersion,
                controlVersion = state.ControlVersion,
                replicaVersions,
                consistent = replicaVersions.Count > 0
                             && replicaVersions.All(version =>
                                 version.SnapshotVersion == state.DataSnapshotVersion),
                catchups = metrics.Catchups,
                catchupCount = metrics.Catchups.Count,
                totalCatchupDurationMs = Round(metrics.Catchups.Sum(record => record.DurationMs)),
                maxCatchupDurationMs = metrics.Catchups.Count == 0
                    ? 0
                    : Round(metrics.Catchups.Max(record => record.DurationMs))
            },
            indexes = new
            {
                activeGeneration = state.ActiveGeneration,
                oldIndex = new
                {
                    name = state.OldIndexName,
                    columns = "Status+OrderId",
                    state = state.OldIndexState,
                    hits = metrics.IndexHits.GetValueOrDefault("old")
                },
                newIndex = new
                {
                    name = state.NewIndexName,
                    columns = "TenantId+Status+UpdatedAt+OrderId",
                    state = state.NewIndexState,
                    hits = metrics.IndexHits.GetValueOrDefault("new")
                }
            },
            errors = new
            {
                snapshotLagCount = metrics.SnapshotLagCount,
                planMismatchCount = metrics.PlanMismatchCount,
                connectionLimitCount = metrics.ConnectionLimitCount,
                performanceBudgetCount = performanceBudgetCount,
                failureLog = metrics.Failures
            },
            queries = new
            {
                total = metrics.QueryCount,
                successful = metrics.SuccessfulQueryCount,
                p95ByKeyOrder = p95ByOrder
            },
            performance = new
            {
                thresholdMs = _options.PerfThresholdMs,
                concurrencyProbe = metrics.Probe,
                individualOverBudgetCount = metrics.Measurements.Count(measurement =>
                    !measurement.WithinBudget),
                planChecks = metrics.Measurements
                    .OrderByDescending(measurement => measurement.AtUtc)
                    .Select(measurement => new
                    {
                        keyOrder = measurement.KeyOrder,
                        expectedIndex = measurement.ExpectedIndex,
                        actualIndex = measurement.ActualIndex,
                        accessMethod = measurement.AccessMethod,
                        durationMs = measurement.DurationMs,
                        thresholdMs = measurement.ThresholdMs,
                        withinBudget = measurement.WithinBudget,
                        budgetErrorCode = measurement.WithinBudget
                            ? null
                            : ErrorCodes.PerformanceBudget,
                        planLines = measurement.PlanLines,
                        measuredAtUtc = measurement.AtUtc
                    })
                    .ToArray()
            },
            audit,
            selfTest
        };
    }

    public async Task<string> WriteAsync(
        object report,
        string? path = null,
        CancellationToken cancellationToken = default)
    {
        var target = Path.GetFullPath(path ?? _options.ReportPath);
        var directory = Path.GetDirectoryName(target);
        if (!string.IsNullOrEmpty(directory))
        {
            Directory.CreateDirectory(directory);
        }

        var temporary = target + ".tmp";
        var json = JsonSerializer.Serialize(report, JsonOptions);
        await File.WriteAllTextAsync(temporary, json, cancellationToken);
        File.Move(temporary, target, overwrite: true);
        return target;
    }

    public static string Serialize(object report) =>
        JsonSerializer.Serialize(report, JsonOptions);

    private async Task<string> ReadSqliteVersionAsync(CancellationToken cancellationToken)
    {
        try
        {
            await using var lease = await _factory.OpenAsync(
                _options.PrimaryDatabasePath, readOnly: true, cancellationToken);
            await using var command = lease.Connection.CreateCommand();
            command.CommandText = "SELECT sqlite_version();";
            return Convert.ToString(
                await command.ExecuteScalarAsync(cancellationToken),
                CultureInfo.InvariantCulture) ?? "unknown";
        }
        catch
        {
            return "unavailable";
        }
    }

    private static double P95(IEnumerable<double> values)
    {
        var sorted = values.OrderBy(value => value).ToArray();
        if (sorted.Length == 0)
        {
            return 0;
        }

        var rank = (int)Math.Ceiling(sorted.Length * 0.95) - 1;
        return Round(sorted[Math.Clamp(rank, 0, sorted.Length - 1)]);
    }

    private static double Round(double value) => Math.Round(value, 3);
}
