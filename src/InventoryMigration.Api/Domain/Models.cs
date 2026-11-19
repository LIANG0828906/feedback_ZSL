namespace InventoryMigration.Api.Domain;

public enum ProductRoute
{
    Old,
    New,
    Migrating
}

public sealed record RouteDecision(ProductRoute Route, long? BatchId, string Reason);

public sealed record BatchRecord(
    long BatchId,
    long RangeStart,
    long RangeEnd,
    string Status,
    string? StartSnapshot,
    long? StartSeq,
    long? CutoffSeq,
    long ProductCount,
    long FlowCount,
    string StartedAt,
    string? CommittedAt);

public sealed record MigrationStateRecord(
    long ProductMin,
    long ProductMax,
    bool Started,
    long LastCommittedEnd,
    long? CurrentBatchId,
    BatchRecord? CurrentBatch,
    string State,
    string UpdatedAt);

public sealed record ValidationMismatch(
    long ProductId,
    long OldProjectedQty,
    long NewQty,
    long OldLastSeq,
    long NewLastSeq);

public sealed record ValidationReport(
    long BatchId,
    long RangeStart,
    long RangeEnd,
    int CheckedProducts,
    int MismatchCount,
    IReadOnlyList<ValidationMismatch> Mismatches,
    DateTimeOffset StartedAt,
    DateTimeOffset FinishedAt,
    string Snapshot);

public sealed record RollbackRecord(
    long ProductId,
    long BatchId,
    DateTimeOffset RolledBackAt,
    string Reason);

public sealed record ValidationMismatchRecord(long BatchId, long ProductId);

public sealed record DebitRequest(long ProductId, int Quantity, string Direction);

public sealed record DebitResponse(
    long ProductId,
    string Direction,
    int Quantity,
    long ChangeQty,
    long Balance,
    long FlowId,
    string Route);

public sealed record RollbackRequest(long ProductId, string? Reason, bool Force = false);

public sealed record FaultRequest(
    bool? OldDbUnavailable = null,
    int? MigrationBatchDelayMs = null,
    IReadOnlyList<long>? ValidationCorruptProductIds = null);

public sealed record PoolStatus(int Limit, int InUse, int Available, int Waiting);

public sealed record ConnectionPoolsStatus(PoolStatus InventoryOld, PoolStatus InventoryNew);

public sealed record MigrationPositionStatus(
    long ProductMin,
    long ProductMax,
    long LastCommittedEnd,
    long NextProductId,
    long? CurrentBatchId,
    BatchRecord? CurrentBatch,
    bool Completed);

public sealed record StatusResponse(
    string Service,
    string State,
    DateTimeOffset CapturedAt,
    MigrationPositionStatus MigrationPosition,
    ConnectionPoolsStatus ConnectionPools,
    IReadOnlyList<RollbackRecord> Rollbacks,
    ValidationReport? LastValidation);

public sealed record ErrorResponse(string Code, string Message, long? ProductId);

public sealed record ServiceMetricsSnapshot(
    long MigratedProducts,
    long MigratedFlows,
    long CapturedChanges,
    long RoutedOldDebits,
    long RoutedNewDebits,
    long EMigrating,
    int ValidationMismatchProducts,
    int RollbackProducts,
    double CaptureP95Ms);
