using System.Collections.Immutable;

namespace BattleEconomy.Core;

public enum Faction
{
    Aurora = 0,
    Bastion = 1
}

public static class ErrorCodes
{
    public const string TerrainInvalid = "E_TERRAIN_INVALID";
    public const string BatchFailed = "E_BATCH_FAILED";
    public const string VersionStale = "E_VERSION_STALE";
    public const string StaleHandle = "E_STALE_HANDLE";
    public const string Busy = "E_BUSY";
    public const string RecoveryInvalid = "E_RECOVERY_INVALID";
}

public static class BatchPhases
{
    public const string Validation = "VALIDATION";
    public const string Merge = "MERGE";
    public const string Settlement = "SETTLEMENT";
    public const string Commit = "COMMIT";
    public const string Cancelled = "CANCELLED";
}

public sealed record OperationError(
    string Code,
    string Message,
    string? Phase = null,
    int? ProcessedDestructionIndex = null,
    int? TerrainId = null,
    int? FirstBrokenBatch = null,
    long? ExpectedVersion = null,
    long? CurrentVersion = null,
    bool Cancelled = false);

public sealed record DestructionEvent(string Key, int TerrainId);

public sealed record BatchExecutionOptions(
    string? FailAtPhase = null,
    int? FailAtDestructionIndex = null,
    int DelayBeforeCommitMilliseconds = 0,
    bool ForceConservativeFallback = false,
    string? ConservativeReason = null);

public sealed record BatchRequest
{
    public long? BatchNumber { get; init; }
    public required ImmutableArray<DestructionEvent> Destructions { get; init; }
    public int EconomicSteps { get; init; } = 1;
    public BatchExecutionOptions? ExecutionOptions { get; init; }
}

public sealed record DestructionImpact(
    int Index,
    int TerrainId,
    string Key,
    ImmutableArray<int> ImpactSet,
    int CandidateCount);

public sealed record BatchTrace(
    long BatchNumber,
    long PreviousVersion,
    long Version,
    string PreviousVersionHash,
    string VersionHash,
    int DestructionCount,
    int CommittedDestructionCount,
    int SkippedDestructionCount,
    ImmutableArray<DestructionImpact> PerDestruction,
    ImmutableArray<int> MergedImpactSet,
    ImmutableArray<int> ClosureSet,
    bool ClosureValidated,
    string ClosureConclusion,
    int ProbedTiles,
    int RecomputedTiles,
    int ReusedTiles,
    bool ConservativeFallback,
    string? ConservativeReason,
    ImmutableArray<FactionEconomy> Factions);

public sealed record BatchResult(
    bool Success,
    OperationError? Error,
    BatchTrace? Trace,
    EconomySnapshot? Snapshot,
    bool NoOp = false);

public sealed record FactionEconomy(
    Faction Faction,
    decimal YieldPerStep,
    decimal TotalResources);

public sealed record SnapshotChainEntry(
    long Version,
    long BatchSequence,
    long CommitWatermark,
    string ContentHash,
    string PreviousChainHash,
    string ChainHash);

public sealed class PublishedSnapshotHandle
{
    internal PublishedSnapshotHandle(EconomySnapshot snapshot, long handleSequence)
    {
        Snapshot = snapshot;
        HandleSequence = handleSequence;
    }

    public EconomySnapshot Snapshot { get; }
    internal long HandleSequence { get; }

    public long Version => Snapshot.Version;
    public long EconomicStep => Snapshot.EconomicStep;
    public string ContentHash => Snapshot.ContentHash;
}

public sealed record SnapshotQueryResult(
    bool Success,
    OperationError? Error,
    long Version,
    long EconomicStep,
    string ContentHash,
    ImmutableArray<TerrainContribution> Contributions,
    ImmutableArray<FactionEconomy> Factions);

public sealed record RecoveryPendingBatch(
    long BatchNumber,
    int EconomicSteps,
    ImmutableArray<DestructionEvent> Destructions,
    string ExpectedSnapshotHash);

public sealed record EngineOpenResult(
    bool Success,
    OperationError? Error,
    EconomyEngine? Engine,
    RecoveryPendingBatch? PendingBatch);
