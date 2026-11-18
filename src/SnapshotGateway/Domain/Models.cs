using System.Text.Json.Serialization;

namespace SnapshotGateway.Domain;

public static class ErrorCodes
{
    public const string SnapshotLag = "SNAPSHOT_LAG";
    public const string PlanMismatch = "E_PLAN_MISMATCH";
    public const string ConnectionLimit = "E_CONN_LIMIT";
    public const string PerformanceBudget = "E_PERF_BUDGET";
    public const string StateConflict = "E_STATE_CONFLICT";
    public const string Validation = "E_VALIDATION";
    public const string Database = "E_DATABASE";
}

public static class KeyOrders
{
    public const string Old = "old";
    public const string New = "new";
}

public sealed class GatewayException : Exception
{
    public GatewayException(string code, string message, object? details = null, int httpStatus = 400)
        : base(message)
    {
        Code = code;
        Details = details;
        HttpStatus = httpStatus;
    }

    public string Code { get; }
    public object? Details { get; }
    public int HttpStatus { get; }
}

public sealed record OrderRow(
    long OrderId,
    string TenantId,
    string Status,
    long Amount,
    string UpdatedAt,
    string CreatedAt,
    string? LastWriteToken);

public sealed record ReplicaVersionInfo(
    int ReplicaId,
    string Name,
    string DatabasePath,
    long SnapshotVersion,
    string State);

public sealed record ReplicaCatchupRecord(
    int ReplicaId,
    long TargetVersion,
    long AppliedVersion,
    double DurationMs,
    bool Succeeded,
    string? Error,
    DateTimeOffset AtUtc);

public sealed record CatchupResult(
    bool Started,
    bool Completed,
    long TargetVersion,
    double TotalDurationMs,
    IReadOnlyList<ReplicaCatchupRecord> ReplicaRecords,
    IReadOnlyList<ReplicaVersionInfo> VersionsAfter,
    string? Error = null);

public sealed record SnapshotDecision(
    bool Consistent,
    long PrimaryVersion,
    IReadOnlyList<ReplicaVersionInfo> VersionsBefore,
    IReadOnlyList<ReplicaVersionInfo> VersionsAfter,
    bool CatchupAttempted,
    bool CatchupCompleted,
    double CatchupDurationMs);

public sealed record PlanLine(
    string Statement,
    int Id,
    int Parent,
    string Detail,
    string AccessMethod,
    string? IndexName,
    bool FullTableScan,
    bool UsesTempBTree);

public sealed record QueryPlanInfo(
    string ExpectedIndex,
    IReadOnlyList<string> ActualIndexes,
    string AccessMethod,
    bool Matched,
    bool NoTableScan,
    bool NoTemporarySort,
    IReadOnlyList<PlanLine> Lines,
    IReadOnlyList<string> MismatchReasons,
    double ThresholdMs,
    double DurationMs,
    bool WithinBudget,
    string? BudgetErrorCode);

public sealed class PagePayload
{
    public int Page { get; init; }
    public int PageSize { get; init; }
    public long Offset { get; init; }
    public long TotalCount { get; init; }
    public IReadOnlyList<OrderRow> Items { get; init; } = Array.Empty<OrderRow>();
}

public sealed class QueryFilter
{
    public string TenantId { get; init; } = string.Empty;
    public string Status { get; init; } = string.Empty;
    public string KeyOrder { get; init; } = KeyOrders.Old;
    public int Page { get; init; } = 1;
    public int PageSize { get; init; } = 25;
    public int? ReplicaId { get; init; }

    [JsonIgnore]
    public string? ExpectedIndexOverride { get; init; }

    [JsonIgnore]
    public string? IndexHintOverride { get; init; }
}

public sealed class QueryResult
{
    public bool Success { get; init; }
    public string? ErrorCode { get; init; }
    public string? Message { get; init; }
    public string KeyOrder { get; init; } = string.Empty;
    public string ExpectedIndex { get; init; } = string.Empty;
    public string? ActualIndex { get; init; }
    public string? AccessMethod { get; init; }
    public long PrimarySnapshotVersion { get; init; }
    public long SnapshotVersion { get; init; }
    public int ReplicaId { get; init; }
    public QueryFilter? Filter { get; init; }
    public PagePayload? Page { get; init; }
    public QueryPlanInfo? Plan { get; init; }
    public object? Details { get; init; }
    public IReadOnlyList<ReplicaVersionInfo> ReplicaVersions { get; init; } = Array.Empty<ReplicaVersionInfo>();
    public IReadOnlyList<string> Warnings { get; init; } = Array.Empty<string>();
    public double DurationMs { get; init; }
    public double? ExecutionDurationMs { get; init; }
    public DateTimeOffset CompletedAtUtc { get; init; } = DateTimeOffset.UtcNow;
}

public sealed record IndexStateSnapshot(
    string ActiveGeneration,
    string OldIndexState,
    string NewIndexState,
    string OldIndexName,
    string NewIndexName,
    long DataSnapshotVersion,
    long ControlVersion);

public sealed class TransitionResult
{
    public bool Success { get; init; }
    public string? ErrorCode { get; init; }
    public string? Message { get; init; }
    public string Operation { get; init; } = string.Empty;
    public long AuditId { get; init; }
    public long SnapshotVersion { get; init; }
    public long ControlVersion { get; init; }
    public IndexStateSnapshot? State { get; init; }
    public IReadOnlyList<ReplicaVersionInfo> ReplicaVersions { get; init; } = Array.Empty<ReplicaVersionInfo>();
    public double ReplicaCatchupMs { get; init; }
    public string? WarningCode { get; init; }
    public string? WarningMessage { get; init; }
}

public sealed record AuditRecord(
    long AuditId,
    string Operation,
    string OldIndexState,
    string NewIndexState,
    long SnapshotVersion,
    string Result,
    string? Details,
    string CreatedAtUtc);

public sealed record PerformanceMeasurement(
    string KeyOrder,
    string ExpectedIndex,
    string ActualIndex,
    string AccessMethod,
    double DurationMs,
    double ThresholdMs,
    bool WithinBudget,
    IReadOnlyList<string> PlanLines,
    DateTimeOffset AtUtc);

public sealed record PerformanceProbe(
    string Name,
    int Concurrency,
    int SampleCount,
    double P95Ms,
    double ThresholdMs,
    bool Passed,
    IReadOnlyList<double> Samples);

public sealed record QueryFailureRecord(
    string ErrorCode,
    string KeyOrder,
    string TenantId,
    string Status,
    int Page,
    int PageSize,
    string? ExpectedIndex,
    IReadOnlyList<string> ActualIndexes,
    string Detail,
    DateTimeOffset AtUtc);
