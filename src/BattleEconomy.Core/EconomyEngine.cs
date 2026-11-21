using System.Collections.Immutable;

namespace BattleEconomy.Core;

public sealed class EconomyEngine
{
    private readonly object _publishLock = new();
    private readonly EconomySnapshotStore _store;
    private readonly TrajectoryLog? _trajectory;
    private readonly string? _recoveryCacheProblem;
    private EconomySnapshot _published;
    private int _batchBusy;
    private long _handleSequence;

    private EconomyEngine(
        TerrainMap map,
        EconomySnapshot published,
        EconomySnapshotStore store,
        TrajectoryLog? trajectory,
        string? recoveryCacheProblem,
        RecoveryPendingBatch? pendingBatch)
    {
        Map = map;
        _published = published;
        _store = store;
        _trajectory = trajectory;
        _recoveryCacheProblem = recoveryCacheProblem;
        PendingBatch = pendingBatch;
    }

    public TerrainMap Map { get; }
    public EconomySnapshotStore Store => _store;
    public string LogPath => _trajectory?.Path ?? string.Empty;
    public RecoveryPendingBatch? PendingBatch { get; private set; }
    public bool Busy => Volatile.Read(ref _batchBusy) != 0;

    public long CurrentVersion
    {
        get
        {
            lock (_publishLock)
            {
                return _published.Version;
            }
        }
    }

    public static EngineOpenResult OpenOrCreate(
        TerrainMap map,
        EconomySnapshotStore store,
        TrajectoryLog? trajectory = null)
    {
        var terrainError = map.Validate();
        if (terrainError is not null)
        {
            return new EngineOpenResult(
                false,
                new OperationError(
                    ErrorCodes.TerrainInvalid,
                    terrainError.Message,
                    TerrainId: terrainError.TerrainId),
                null,
                null);
        }

        if (!Directory.Exists(store.RootDirectory))
        {
            Directory.CreateDirectory(store.RootDirectory);
        }

        var load = store.Load(map);
        if (!load.Success || load.Error is not null)
        {
            return new EngineOpenResult(false, load.Error, null, null);
        }

        if (load.Snapshot is null)
        {
            var baseline = BuildBaseline(map);
            try
            {
                store.SaveSnapshot(baseline);
            }
            catch (Exception exception)
            {
                return new EngineOpenResult(
                    false,
                    new OperationError(
                        ErrorCodes.RecoveryInvalid,
                        $"baseline snapshot cannot be persisted: {exception.GetType().Name}",
                        Phase: BatchPhases.Commit),
                    null,
                    null);
            }

            return new EngineOpenResult(
                true,
                null,
                new EconomyEngine(map, baseline, store, trajectory, null, null),
                null);
        }

        var workspace = SettlementWorkspace.FromSnapshot(map, load.Snapshot, out var cacheProblem);
        if (cacheProblem is not null)
        {
            // Hash-valid snapshots should not reach this branch. If they do, the next batch is
            // conservatively recomputed and the reason is carried into the trace.
        }

        var pending = load.PendingBatch;
        if (pending is not null)
        {
            if (pending.BatchNumber <= load.Snapshot.BatchSequence)
            {
                var allCommitted = pending.Destructions.All(item =>
                    load.Snapshot.CommittedDestructionKeys.Contains(item.Key));
                if (!allCommitted)
                {
                    return new EngineOpenResult(
                        false,
                        new OperationError(
                            ErrorCodes.RecoveryInvalid,
                            "pending batch is not reflected atomically in the committed snapshot",
                            FirstBrokenBatch: checked((int)Math.Clamp(pending.BatchNumber, 0, int.MaxValue))),
                        null,
                        null);
                }

                store.DeletePendingBatch();
                pending = null;
            }
            else if (!string.Equals(
                         pending.ExpectedSnapshotHash,
                         load.Snapshot.ContentHash,
                         StringComparison.Ordinal))
            {
                return new EngineOpenResult(
                    false,
                    new OperationError(
                        ErrorCodes.RecoveryInvalid,
                        "pending batch base snapshot hash does not match recovery snapshot",
                        FirstBrokenBatch: checked((int)Math.Clamp(pending.BatchNumber, 0, int.MaxValue))),
                    null,
                    null);
            }
        }

        var engine = new EconomyEngine(
            map,
            load.Snapshot,
            store,
            trajectory,
            cacheProblem,
            pending);
        return new EngineOpenResult(true, null, engine, pending);
    }

    public PublishedSnapshotHandle AcquirePublishedSnapshot()
    {
        lock (_publishLock)
        {
            var sequence = Interlocked.Increment(ref _handleSequence);
            return new PublishedSnapshotHandle(_published, sequence);
        }
    }

    public SnapshotQueryResult Query(
        PublishedSnapshotHandle handle,
        long? expectedVersion = null)
    {
        EconomySnapshot snapshot;
        lock (_publishLock)
        {
            if (!ReferenceEquals(handle.Snapshot, _published))
            {
                return FailedQuery(
                    new OperationError(
                        ErrorCodes.StaleHandle,
                        "snapshot handle was retired by a newer publish",
                        ExpectedVersion: expectedVersion,
                        CurrentVersion: _published.Version));
            }

            snapshot = _published;
        }

        if (expectedVersion is not null && expectedVersion.Value != snapshot.Version)
        {
            return FailedQuery(
                new OperationError(
                    ErrorCodes.VersionStale,
                    "requested version is not the current published version",
                    ExpectedVersion: expectedVersion,
                    CurrentVersion: snapshot.Version));
        }

        return new SnapshotQueryResult(
            true,
            null,
            snapshot.Version,
            snapshot.EconomicStep,
            snapshot.ContentHash,
            snapshot.Contributions.Values.OrderBy(item => item.TerrainId).ToImmutableArray(),
            snapshot.Factions);
    }

    public SnapshotQueryResult QueryCurrent(long? expectedVersion = null)
    {
        return Query(AcquirePublishedSnapshot(), expectedVersion);
    }

    public async Task<BatchResult> SettleBatchAsync(
        BatchRequest request,
        CancellationToken cancellationToken = default)
    {
        if (Interlocked.CompareExchange(ref _batchBusy, 1, 0) != 0)
        {
            return new BatchResult(
                false,
                new OperationError(
                    ErrorCodes.Busy,
                    "another batch settlement is already active for this battlefield",
                    Phase: BatchPhases.Validation),
                null,
                null);
        }

        try
        {
            return await Task.Run(
                () => SettleBatchCore(request, cancellationToken),
                CancellationToken.None).ConfigureAwait(false);
        }
        finally
        {
            Volatile.Write(ref _batchBusy, 0);
        }
    }

    public Task<BatchResult> ResumePendingBatchAsync(CancellationToken cancellationToken = default)
    {
        var pending = PendingBatch;
        if (pending is null)
        {
            return Task.FromResult(
                new BatchResult(
                    false,
                    new OperationError(
                        ErrorCodes.BatchFailed,
                        "there is no recoverable pending batch",
                        Phase: BatchPhases.Validation),
                    null,
                    null));
        }

        var request = new BatchRequest
        {
            BatchNumber = pending.BatchNumber,
            Destructions = pending.Destructions,
            EconomicSteps = pending.EconomicSteps
        };
        return SettleBatchAsync(request, cancellationToken);
    }

    private BatchResult SettleBatchCore(BatchRequest request, CancellationToken cancellationToken)
    {
        EconomySnapshot baseline;
        lock (_publishLock)
        {
            baseline = _published;
        }

        var phase = BatchPhases.Validation;
        var processedIndex = -1;
        var batchNumber = request.BatchNumber ?? (baseline.BatchSequence + 1);
        var totalDestructions = request.Destructions.IsDefault ? 0 : request.Destructions.Length;

        try
        {
            var validation = ValidateRequest(request, baseline, batchNumber);
            if (validation.Error is not null)
            {
                return Fail(
                    baseline,
                    batchNumber,
                    totalDestructions,
                    validation.Error,
                    processedIndex);
            }

            var uncommitted = validation.Uncommitted;
            var skipped = validation.Skipped;
            if (uncommitted.Length == 0)
            {
                WriteSkippedLog(batchNumber, totalDestructions, skipped.Length, baseline);
                return new BatchResult(true, null, null, baseline, NoOp: true);
            }

            var options = request.ExecutionOptions ?? new BatchExecutionOptions();
            ThrowIfCancelled(cancellationToken);
            ThrowIfInjected(options, BatchPhases.Merge, null);

            var pendingIntent = new RecoveryPendingBatch(
                batchNumber,
                request.EconomicSteps,
                uncommitted,
                baseline.ContentHash);
            try
            {
                _store.SavePendingBatch(pendingIntent);
            }
            catch (Exception exception)
            {
                phase = BatchPhases.Commit;
                return Fail(
                    baseline,
                    batchNumber,
                    totalDestructions,
                    BatchFailure(
                        phase,
                        processedIndex,
                        $"pending batch intent cannot be persisted: {exception.GetType().Name}"),
                    processedIndex);
            }

            var workspace = SettlementWorkspace.FromSnapshot(Map, baseline, out var cacheProblem);
            var conservativeReasons = new[] { cacheProblem, _recoveryCacheProblem }
                .Where(item => !string.IsNullOrWhiteSpace(item))
                .ToList();
            if (options.ForceConservativeFallback)
            {
                conservativeReasons.Add(options.ConservativeReason ?? "FORCED_BY_OPERATION_OPTIONS");
            }

            var forceConservative = conservativeReasons.Count > 0;
            var conservativeReason = forceConservative
                ? string.Join(";", conservativeReasons.Distinct(StringComparer.Ordinal))
                : null;

            phase = BatchPhases.Settlement;
            var analysis = workspace.Clone();
            var impacts = ImmutableArray.CreateBuilder<DestructionImpact>(uncommitted.Length);
            var closureBuilder = new SortedSet<int>();
            for (var index = 0; index < uncommitted.Length; index++)
            {
                ThrowIfCancelled(cancellationToken);
                processedIndex = index;
                ThrowIfInjected(options, BatchPhases.Settlement, index);
                var step = analysis.ApplyForAnalysis(uncommitted[index].TerrainId);
                closureBuilder.UnionWith(step.Changed);
                impacts.Add(new DestructionImpact(
                    index,
                    uncommitted[index].TerrainId,
                    uncommitted[index].Key,
                    step.Changed,
                    step.Candidates.Length));
            }

            ThrowIfCancelled(cancellationToken);
            ThrowIfInjected(options, BatchPhases.Merge, null);
            phase = BatchPhases.Merge;

            var merged = workspace.SettleMerged(
                uncommitted.Select(item => item.TerrainId),
                forceConservative,
                conservativeReason);
            var closure = closureBuilder.ToImmutableArray();
            var closureValidated = merged.Affected.ToHashSet().SetEquals(closure);

            var closureEquivalent = merged.Affected.Length == closure.Length;
            if (closureEquivalent)
            {
                var affectedSet = merged.Affected.ToHashSet();
                closureEquivalent = closure.All(affectedSet.Contains);
            }

            if (!closureValidated || !closureEquivalent || !WorkspacesEquivalent(analysis, workspace))
            {
                return Fail(
                    baseline,
                    batchNumber,
                    totalDestructions,
                    BatchFailure(
                        BatchPhases.Merge,
                        processedIndex,
                        "merged impact set does not equal the transitive closure of individual impacts"),
                    processedIndex);
            }

            var contributions = workspace.Contributions.ToImmutableDictionary();
            var destroyed = baseline.DestroyedTerrainIds
                .Union(uncommitted.Select(item => item.TerrainId))
                .ToImmutableSortedSet();
            var committedKeys = baseline.CommittedDestructionKeys
                .Union(uncommitted.Select(item => item.Key))
                .ToImmutableHashSet(StringComparer.Ordinal);
            var factions = SettlementMath.Accumulate(
                contributions.Values,
                baseline.Factions,
                request.EconomicSteps);

            var nextSnapshot = BuildNextSnapshot(
                baseline,
                batchNumber,
                request.EconomicSteps,
                contributions,
                destroyed,
                committedKeys,
                factions);

            if (options.DelayBeforeCommitMilliseconds > 0)
            {
                Thread.Sleep(options.DelayBeforeCommitMilliseconds);
            }

            phase = BatchPhases.Commit;
            ThrowIfCancelled(cancellationToken);
            ThrowIfInjected(options, BatchPhases.Commit, null);

            lock (_publishLock)
            {
                _store.SaveSnapshot(nextSnapshot);
                _published = nextSnapshot;
            }

            _store.DeletePendingBatch();
            PendingBatch = null;

            var trace = new BatchTrace(
                batchNumber,
                baseline.Version,
                nextSnapshot.Version,
                baseline.ContentHash,
                nextSnapshot.ContentHash,
                totalDestructions,
                uncommitted.Length,
                skipped.Length,
                impacts.ToImmutable(),
                merged.Affected,
                closure,
                true,
                "MATCH",
                merged.Candidates.Length,
                merged.RecomputedTiles,
                merged.ReusedTiles,
                merged.ConservativeFallback,
                merged.ConservativeReason,
                factions);

            WriteCommittedLog(trace, nextSnapshot);
            return new BatchResult(true, null, trace, nextSnapshot);
        }
        catch (OperationCanceledException)
        {
            _store.DeletePendingBatch();
            return Fail(
                baseline,
                batchNumber,
                totalDestructions,
                new OperationError(
                    ErrorCodes.BatchFailed,
                    "batch was cooperatively cancelled and rolled back",
                    Phase: BatchPhases.Cancelled,
                    ProcessedDestructionIndex: processedIndex,
                    Cancelled: true),
                processedIndex);
        }
        catch (InjectedBatchException)
        {
            _store.DeletePendingBatch();
            return Fail(
                baseline,
                batchNumber,
                totalDestructions,
                BatchFailure(
                    phase,
                    processedIndex,
                    "injected failure requested by verification options"),
                processedIndex);
        }
        catch (Exception exception)
        {
            _store.DeletePendingBatch();
            return Fail(
                baseline,
                batchNumber,
                totalDestructions,
                BatchFailure(
                    phase,
                    processedIndex,
                    $"unexpected {exception.GetType().Name}: {exception.Message}"),
                processedIndex);
        }
    }

    private ValidationOutcome ValidateRequest(
        BatchRequest request,
        EconomySnapshot baseline,
        long batchNumber)
    {
        if (request.Destructions.IsDefaultOrEmpty)
        {
            return ValidationOutcome.Failure(
                ErrorCodes.BatchFailed,
                "batch must contain at least one destruction",
                BatchPhases.Validation);
        }

        if (request.EconomicSteps <= 0)
        {
            return ValidationOutcome.Failure(
                ErrorCodes.BatchFailed,
                "economic steps must be positive",
                BatchPhases.Validation);
        }

        if (request.Destructions.Select(item => item.Key).Distinct(StringComparer.Ordinal).Count() !=
            request.Destructions.Length)
        {
            return ValidationOutcome.Failure(
                ErrorCodes.BatchFailed,
                "batch contains duplicate destruction keys",
                BatchPhases.Validation);
        }

        var uncommitted = ImmutableArray.CreateBuilder<DestructionEvent>();
        var skipped = ImmutableArray.CreateBuilder<DestructionEvent>();
        for (var index = 0; index < request.Destructions.Length; index++)
        {
            var item = request.Destructions[index];
            if (!Map.TryGetNode(item.TerrainId, out _))
            {
                return ValidationOutcome.Failure(
                    ErrorCodes.TerrainInvalid,
                    $"destruction terrain {item.TerrainId} does not exist",
                    BatchPhases.Validation,
                    item.TerrainId,
                    index);
            }

            if (string.IsNullOrWhiteSpace(item.Key))
            {
                return ValidationOutcome.Failure(
                    ErrorCodes.BatchFailed,
                    "destruction key is empty",
                    BatchPhases.Validation,
                    item.TerrainId,
                    index);
            }

            if (baseline.CommittedDestructionKeys.Contains(item.Key))
            {
                skipped.Add(item);
                continue;
            }

            if (baseline.DestroyedTerrainIds.Contains(item.TerrainId))
            {
                return ValidationOutcome.Failure(
                    ErrorCodes.BatchFailed,
                    $"terrain {item.TerrainId} is already destroyed",
                    BatchPhases.Validation,
                    item.TerrainId,
                    index);
            }

            uncommitted.Add(item);
        }

        if (skipped.Count > 0 && uncommitted.Count == 0)
        {
            return new ValidationOutcome(null, uncommitted.ToImmutable(), skipped.ToImmutable());
        }

        if (batchNumber != baseline.BatchSequence + 1)
        {
            return ValidationOutcome.Failure(
                ErrorCodes.BatchFailed,
                $"batch number {batchNumber} does not continue watermark {baseline.BatchSequence}",
                BatchPhases.Validation);
        }

        var duplicateTiles = uncommitted
            .GroupBy(item => item.TerrainId)
            .FirstOrDefault(group => group.Count() > 1);
        if (duplicateTiles is not null)
        {
            var index = request.Destructions.IndexOf(duplicateTiles.Last());
            return ValidationOutcome.Failure(
                ErrorCodes.BatchFailed,
                $"terrain {duplicateTiles.Key} appears more than once in the batch",
                BatchPhases.Validation,
                duplicateTiles.Key,
                index);
        }

        return new ValidationOutcome(null, uncommitted.ToImmutable(), skipped.ToImmutable());
    }

    private BatchResult Fail(
        EconomySnapshot baseline,
        long batchNumber,
        int destructionCount,
        OperationError error,
        int processedIndex)
    {
        WriteFailedLog(batchNumber, destructionCount, error, baseline, processedIndex);
        return new BatchResult(false, error, null, baseline);
    }

    private static OperationError BatchFailure(
        string phase,
        int processedIndex,
        string message,
        int? terrainId = null)
    {
        return new OperationError(
            ErrorCodes.BatchFailed,
            message,
            Phase: phase,
            ProcessedDestructionIndex: processedIndex >= 0 ? processedIndex : null,
            TerrainId: terrainId);
    }

    private static void ThrowIfCancelled(CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
    }

    private static void ThrowIfInjected(BatchExecutionOptions options, string phase, int? index)
    {
        if (options.FailAtPhase is null ||
            !string.Equals(options.FailAtPhase, phase, StringComparison.OrdinalIgnoreCase))
        {
            return;
        }

        if (index is not null &&
            options.FailAtDestructionIndex is not null &&
            options.FailAtDestructionIndex.Value != index.Value)
        {
            return;
        }

        throw new InjectedBatchException();
    }

    private static bool WorkspacesEquivalent(
        SettlementWorkspace left,
        SettlementWorkspace right)
    {
        if (left.Contributions.Count != right.Contributions.Count)
        {
            return false;
        }

        foreach (var item in left.Contributions)
        {
            if (!right.Contributions.TryGetValue(item.Key, out var other) ||
                other.ContentHash != item.Value.ContentHash)
            {
                return false;
            }
        }

        return true;
    }

    private static EconomySnapshot BuildBaseline(TerrainMap map)
    {
        var workspace = SettlementWorkspace.Initial(map);
        var contributions = workspace.Contributions.ToImmutableDictionary();
        var factions = Enum.GetValues<Faction>()
            .Select(faction => new FactionEconomy(faction, 0m, 0m))
            .ToImmutableArray();
        var destroyed = ImmutableSortedSet<int>.Empty;
        var committedKeys = ImmutableHashSet<string>.Empty.WithComparer(StringComparer.Ordinal);
        var contentHash = StableHash.SnapshotContent(
            0,
            0,
            0,
            0,
            StableHash.Genesis,
            contributions.Values,
            destroyed,
            committedKeys,
            factions);
        var chainHash = StableHash.ChainEntry(0, 0, 0, contentHash, StableHash.Genesis);

        return new EconomySnapshot
        {
            Version = 0,
            BatchSequence = 0,
            EconomicStep = 0,
            CommitWatermark = 0,
            PreviousContentHash = StableHash.Genesis,
            ContentHash = contentHash,
            Contributions = contributions,
            DestroyedTerrainIds = destroyed,
            CommittedDestructionKeys = committedKeys,
            Factions = factions,
            HashChain = [new SnapshotChainEntry(0, 0, 0, contentHash, StableHash.Genesis, chainHash)]
        };
    }

    private static EconomySnapshot BuildNextSnapshot(
        EconomySnapshot baseline,
        long batchNumber,
        int economicSteps,
        ImmutableDictionary<int, TerrainContribution> contributions,
        ImmutableSortedSet<int> destroyed,
        ImmutableHashSet<string> committedKeys,
        ImmutableArray<FactionEconomy> factions)
    {
        var version = baseline.Version + 1;
        var economicStep = baseline.EconomicStep + economicSteps;
        var previousChainHash = baseline.HashChain[^1].ChainHash;
        var contentHash = StableHash.SnapshotContent(
            version,
            batchNumber,
            economicStep,
            batchNumber,
            baseline.ContentHash,
            contributions.Values,
            destroyed,
            committedKeys,
            factions);
        var chainHash = StableHash.ChainEntry(
            version,
            batchNumber,
            batchNumber,
            contentHash,
            previousChainHash);

        return new EconomySnapshot
        {
            Version = version,
            BatchSequence = batchNumber,
            EconomicStep = economicStep,
            CommitWatermark = batchNumber,
            PreviousContentHash = baseline.ContentHash,
            ContentHash = contentHash,
            Contributions = contributions,
            DestroyedTerrainIds = destroyed,
            CommittedDestructionKeys = committedKeys,
            Factions = factions,
            HashChain = baseline.HashChain.Add(
                new SnapshotChainEntry(
                    version,
                    batchNumber,
                    batchNumber,
                    contentHash,
                    previousChainHash,
                    chainHash))
        };
    }

    private void WriteCommittedLog(BatchTrace trace, EconomySnapshot snapshot)
    {
        _trajectory?.Write(new
        {
            eventName = "batch_committed",
            batch = trace.BatchNumber,
            destructions = trace.DestructionCount,
            committedDestructions = trace.CommittedDestructionCount,
            skippedDestructions = trace.SkippedDestructionCount,
            perDestruction = trace.PerDestruction.Select(item => new
            {
                index = item.Index,
                terrain = item.TerrainId,
                key = item.Key,
                impact = item.ImpactSet,
                candidateCount = item.CandidateCount
            }).ToArray(),
            mergedImpact = trace.MergedImpactSet,
            mergedImpactSize = trace.MergedImpactSet.Length,
            closure = trace.ClosureSet,
            closureValidated = trace.ClosureValidated,
            closureConclusion = trace.ClosureConclusion,
            probedTiles = trace.ProbedTiles,
            recomputedTiles = trace.RecomputedTiles,
            reusedTiles = trace.ReusedTiles,
            conservativeFallback = trace.ConservativeFallback,
            conservativeReason = trace.ConservativeReason,
            version = snapshot.Version,
            versionHash = snapshot.ContentHash,
            previousVersionHash = snapshot.PreviousContentHash,
            economicStep = snapshot.EconomicStep,
            factions = snapshot.Factions.Select(item => new
            {
                faction = item.Faction.ToString(),
                yieldPerStep = item.YieldPerStep,
                totalResources = item.TotalResources
            }).ToArray()
        });
    }

    private void WriteSkippedLog(
        long batchNumber,
        int destructionCount,
        int skippedCount,
        EconomySnapshot snapshot)
    {
        _trajectory?.Write(new
        {
            eventName = "batch_skipped",
            batch = batchNumber,
            destructions = destructionCount,
            skippedDestructions = skippedCount,
            reason = "ALL_DESTRUCTIONS_ALREADY_COMMITTED",
            version = snapshot.Version,
            versionHash = snapshot.ContentHash
        });
    }

    private void WriteFailedLog(
        long batchNumber,
        int destructionCount,
        OperationError error,
        EconomySnapshot snapshot,
        int processedIndex)
    {
        _trajectory?.Write(new
        {
            eventName = "batch_failed",
            batch = batchNumber,
            destructions = destructionCount,
            phase = error.Phase,
            processedDestructionIndex = error.ProcessedDestructionIndex ?? processedIndex,
            errorCode = error.Code,
            cancelled = error.Cancelled,
            rolledBack = true,
            version = snapshot.Version,
            versionHash = snapshot.ContentHash
        });
    }

    private static SnapshotQueryResult FailedQuery(OperationError error)
    {
        return new SnapshotQueryResult(
            false,
            error,
            0,
            -1,
            string.Empty,
            [],
            []);
    }

    private sealed record ValidationOutcome(
        OperationError? Error,
        ImmutableArray<DestructionEvent> Uncommitted,
        ImmutableArray<DestructionEvent> Skipped)
    {
        public static ValidationOutcome Failure(
            string code,
            string message,
            string phase,
            int? terrainId = null,
            int? processedIndex = null)
        {
            return new ValidationOutcome(
                new OperationError(
                    code,
                    message,
                    Phase: phase,
                    ProcessedDestructionIndex: processedIndex,
                    TerrainId: terrainId),
                [],
                []);
        }
    }

    private sealed class InjectedBatchException : Exception;
}
