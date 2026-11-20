using System.Collections.Concurrent;

namespace TacticalField.Core;

public sealed class BattlefieldEngine
{
    private readonly object _publishGate = new();
    private readonly object _cacheGate = new();
    private readonly object _orderGate = new();
    private readonly ConcurrentDictionary<FieldKey, byte> _activeRecalculations = new();
    private readonly EngineOptions _options;
    private readonly int _hazardPenalty;
    private BattlefieldView _published;
    private List<ChainLink> _history;
    private Dictionary<PathCacheKey, PathCacheEntry> _cache;
    private long _nextTicket;
    private long _servingTicket;

    private BattlefieldEngine(
        BattlefieldView view,
        IEnumerable<ChainLink> history,
        IReadOnlyDictionary<PathCacheKey, PathCacheEntry> cache,
        int hazardPenalty,
        EngineOptions options)
    {
        _published = view;
        _history = history.ToList();
        _cache = cache.ToDictionary(pair => pair.Key, pair => pair.Value);
        _hazardPenalty = hazardPenalty;
        _options = options;
    }

    internal Action<FieldKey>? RecalculationStartedHook { get; set; }

    public BattlefieldView Current => Volatile.Read(ref _published);
    public int HazardPenalty => _hazardPenalty;
    public string? SnapshotPath => _options.SnapshotPath;

    public static BattlefieldEngine Create(BattlefieldDefinition definition, EngineOptions? options = null)
    {
        ArgumentNullException.ThrowIfNull(definition);
        options ??= new EngineOptions();
        if (options.HazardPenalty < 0)
        {
            throw new ArgumentOutOfRangeException(nameof(options), "Hazard penalty cannot be negative.");
        }

        var units = definition.Units.ToDictionary(unit => unit.Id, unit => unit, StringComparer.Ordinal);
        var fields = new Dictionary<FieldKey, CostFieldSnapshot>();
        foreach (var unit in definition.Units.OrderBy(unit => unit.Id, StringComparer.Ordinal))
        {
            foreach (var target in unit.Targets.OrderBy(value => value))
            {
                var field = CostFieldCalculator.BuildFull(
                    definition.Grid,
                    unit,
                    target,
                    options.HazardPenalty,
                    fieldVersion: 1,
                    batchSequence: 0);
                fields.Add(field.Key, field);
            }
        }

        var provisional = new BattlefieldView(
            version: 1,
            batchSequence: 0,
            definition.Grid,
            units,
            fields,
            SnapshotCodec.ZeroChain);
        var link = SnapshotCodec.CreateChainLink(provisional, options.HazardPenalty, SnapshotCodec.ZeroChain);
        var view = new BattlefieldView(
            version: 1,
            batchSequence: 0,
            definition.Grid,
            units,
            fields,
            link.ChainHash);
        var engine = new BattlefieldEngine(
            view,
            new[] { link },
            new Dictionary<PathCacheKey, PathCacheEntry>(),
            options.HazardPenalty,
            options);

        if (engine.ShouldPersist)
        {
            SnapshotCodec.Save(
                engine._options.SnapshotPath!,
                view,
                options.HazardPenalty,
                engine._history,
                engine._cache);
        }

        return engine;
    }

    public static EngineOpenResult Recover(string snapshotPath, EngineOptions? options = null)
    {
        if (string.IsNullOrWhiteSpace(snapshotPath))
        {
            return new EngineOpenResult(ErrorCodes.RecoveryInvalid, null, "Snapshot path is required.");
        }

        var loaded = SnapshotCodec.Load(snapshotPath);
        if (!loaded.IsSuccess || loaded.Snapshot is null)
        {
            return new EngineOpenResult(loaded.Code, null, loaded.Message);
        }

        var effectiveOptions = new EngineOptions
        {
            SnapshotPath = snapshotPath,
            HazardPenalty = loaded.Snapshot.HazardPenalty,
            PersistSnapshots = options?.PersistSnapshots ?? true
        };
        var engine = new BattlefieldEngine(
            loaded.Snapshot.View,
            loaded.Snapshot.History,
            loaded.Snapshot.Cache,
            loaded.Snapshot.HazardPenalty,
            effectiveOptions);
        return new EngineOpenResult(ErrorCodes.Ok, engine);
    }

    public CostFieldSnapshot? GetField(FieldKey key)
        => Current.Fields.TryGetValue(key, out var field) ? field : null;

    public CostQueryResult QueryCost(
        string unitId,
        int target,
        long? expectedFieldVersion = null,
        string? expectedFieldHash = null)
    {
        var view = Current;
        var key = new FieldKey(unitId, target);
        if (!view.Fields.TryGetValue(key, out var field))
        {
            return new CostQueryResult(ErrorCodes.NotFound, unitId, target, 0, string.Empty, view.Version, -1, "Field not found.");
        }

        if (IsStale(field, expectedFieldVersion, expectedFieldHash))
        {
            return new CostQueryResult(
                ErrorCodes.FieldStale,
                unitId,
                target,
                field.FieldVersion,
                field.ContentHash,
                view.Version,
                -1,
                "Expected field version/hash does not match the published field.");
        }

        return new CostQueryResult(
            ErrorCodes.Ok,
            unitId,
            target,
            field.FieldVersion,
            field.ContentHash,
            view.Version,
            field.GetCost(field.Key.Target));
    }

    public PathQueryResult QueryPath(
        string unitId,
        int target,
        int? startCell = null,
        long? expectedFieldVersion = null,
        string? expectedFieldHash = null)
    {
        var view = Current;
        var key = new FieldKey(unitId, target);
        if (!view.Fields.TryGetValue(key, out var field))
        {
            return PathQueryResult.Error(ErrorCodes.NotFound, unitId, target, startCell ?? -1, view.Version, "Field not found.");
        }

        if (IsStale(field, expectedFieldVersion, expectedFieldHash))
        {
            return new PathQueryResult(
                ErrorCodes.FieldStale,
                unitId,
                target,
                startCell ?? view.Units[unitId].StartCell,
                field.FieldVersion,
                field.ContentHash,
                view.Version,
                -1,
                Array.Empty<int>(),
                string.Empty,
                false,
                Array.Empty<int>(),
                "Expected field version/hash does not match the published field.");
        }

        var start = startCell ?? view.Units[unitId].StartCell;
        if (start < 0 || start >= view.Grid.CellCount)
        {
            return PathQueryResult.Error(ErrorCodes.InvalidArgument, unitId, target, start, view.Version, "Start cell is out of range.");
        }

        var cost = field.GetCost(start);
        if (cost < 0)
        {
            return new PathQueryResult(
                ErrorCodes.Unreachable,
                unitId,
                target,
                start,
                field.FieldVersion,
                field.ContentHash,
                view.Version,
                -1,
                Array.Empty<int>(),
                string.Empty,
                false,
                Array.Empty<int>(),
                "Target is unreachable from the start cell.");
        }

        var cacheKey = new PathCacheKey(key, start, field.FieldVersion, field.ContentHash);
        PathCacheEntry? cached = null;
        lock (_cacheGate)
        {
            if (_cache.TryGetValue(cacheKey, out cached) && !cached.IsValid(field))
            {
                _cache.Remove(cacheKey);
                cached = null;
            }
        }

        if (cached is not null)
        {
            return new PathQueryResult(
                ErrorCodes.Ok,
                unitId,
                target,
                start,
                field.FieldVersion,
                field.ContentHash,
                view.Version,
                cost,
                (int[])cached.Cells.Clone(),
                cached.PathHash,
                true,
                (int[])cached.Dependencies.Clone());
        }

        var cells = CostFieldCalculator.ComputePath(field, start);
        var pathHash = CostFieldCalculator.ComputePathHash(unitId, target, cost, cells);
        var entry = new PathCacheEntry(cacheKey, cost, cells, pathHash);
        lock (_cacheGate)
        {
            _cache.TryAdd(cacheKey, entry);
        }

        return new PathQueryResult(
            ErrorCodes.Ok,
            unitId,
            target,
            start,
            field.FieldVersion,
            field.ContentHash,
            view.Version,
            cost,
            (int[])entry.Cells.Clone(),
            pathHash,
            false,
            (int[])entry.Dependencies.Clone());
    }

    public CostFieldView QueryField(FieldKey key)
    {
        var view = Current;
        if (!view.Fields.TryGetValue(key, out var field))
        {
            return new CostFieldView(ErrorCodes.NotFound, key, 0, view.Version, string.Empty, 0, Array.Empty<long>(), Array.Empty<int>(), "Field not found.");
        }

        return new CostFieldView(
            ErrorCodes.Ok,
            key,
            field.FieldVersion,
            view.Version,
            field.ContentHash,
            field.UnitMask,
            field.GetCostsCopy(),
            field.GetNextCopy());
    }

    public BatchResult SubmitBatch(IReadOnlyList<BattleChange> changes)
    {
        if (changes is null)
        {
            return Failure(ErrorCodes.BatchAborted, "Batch changes are required.");
        }

        var current = Current;
        if (!TryResolveReservationKeys(current, changes, out var keys, out var error))
        {
            return Failure(ErrorCodes.BatchAborted, error ?? "Batch reservation failed.");
        }

        if (!TryReserve(keys))
        {
            return Failure(ErrorCodes.Busy, "A recalculation is already active for at least one affected field.");
        }

        var ticket = EnqueueTicket();
        try
        {
            WaitTicket(ticket);
            return CommitReserved(changes);
        }
        finally
        {
            CompleteTicket(ticket);
            Release(keys);
        }
    }

    public RecalcSubmissionResult SubmitBatchAsync(IReadOnlyList<BattleChange> changes)
    {
        if (changes is null)
        {
            return new RecalcSubmissionResult(ErrorCodes.BatchAborted, null, null, "Batch changes are required.");
        }

        var current = Current;
        if (!TryResolveReservationKeys(current, changes, out var keys, out var error))
        {
            return new RecalcSubmissionResult(ErrorCodes.BatchAborted, null, null, error ?? "Batch reservation failed.");
        }

        if (!TryReserve(keys))
        {
            return new RecalcSubmissionResult(ErrorCodes.Busy, null, null, "A recalculation is already active for at least one affected field.");
        }

        var ticket = EnqueueTicket();
        var task = Task.Run(() =>
        {
            try
            {
                WaitTicket(ticket);
                if (keys.Count > 0)
                {
                    RecalculationStartedHook?.Invoke(keys[0]);
                }

                return CommitReserved(changes);
            }
            finally
            {
                CompleteTicket(ticket);
                Release(keys);
            }
        });

        return new RecalcSubmissionResult(ErrorCodes.Ok, keys.Count > 0 ? keys[0] : null, task);
    }

    public RecalcSubmissionResult SubmitPathRecalculation(FieldKey key)
    {
        var current = Current;
        if (!current.Fields.ContainsKey(key))
        {
            return new RecalcSubmissionResult(ErrorCodes.NotFound, key, null, "Field not found.");
        }

        if (!TryReserve(new[] { key }))
        {
            return new RecalcSubmissionResult(ErrorCodes.Busy, key, null, "A recalculation is already active for this unit/target.");
        }

        var ticket = EnqueueTicket();
        var task = Task.Run(() =>
        {
            try
            {
                WaitTicket(ticket);
                RecalculationStartedHook?.Invoke(key);
                return RecalculateCache(key);
            }
            finally
            {
                CompleteTicket(ticket);
                Release(new[] { key });
            }
        });

        return new RecalcSubmissionResult(ErrorCodes.Ok, key, task);
    }

    private BatchResult CommitReserved(IReadOnlyList<BattleChange> changes)
    {
        lock (_publishGate)
        {
            var oldView = Current;
            try
            {
                var (newGrid, newUnits) = ApplyChanges(oldView, changes);
                var newVersion = oldView.Version + 1;
                var newSequence = oldView.BatchSequence + 1;
                var newFields = new Dictionary<FieldKey, CostFieldSnapshot>();
                var deltas = new List<FieldDelta>();

                foreach (var pair in oldView.Fields.OrderBy(pair => pair.Key.UnitId, StringComparer.Ordinal).ThenBy(pair => pair.Key.Target))
                {
                    var key = pair.Key;
                    var oldField = pair.Value;
                    var oldUnit = oldView.Units[key.UnitId];
                    var newUnit = newUnits[key.UnitId];
                    var update = CostFieldCalculator.UpdateIncremental(
                        oldField,
                        oldView.Grid,
                        newGrid,
                        oldUnit,
                        newUnit,
                        key.Target,
                        _hazardPenalty,
                        oldField.FieldVersion + 1,
                        newSequence);
                    newFields.Add(key, update.Field);
                    if (!ReferenceEquals(update.Field, oldField))
                    {
                        deltas.Add(new FieldDelta(
                            key,
                            update.ImpactCells,
                            update.RecomputedCells,
                            update.ReusedCells,
                            oldField.FieldVersion,
                            update.Field.FieldVersion,
                            oldField.ContentHash,
                            update.Field.ContentHash));
                    }
                }

                var provisional = new BattlefieldView(
                    newVersion,
                    newSequence,
                    newGrid,
                    newUnits,
                    newFields,
                    oldView.ChainHash);
                var link = SnapshotCodec.CreateChainLink(provisional, _hazardPenalty, oldView.ChainHash);
                var newView = new BattlefieldView(
                    newVersion,
                    newSequence,
                    newGrid,
                    newUnits,
                    newFields,
                    link.ChainHash);
                var newHistory = new List<ChainLink>(_history) { link };

                lock (_cacheGate)
                {
                    var migratedCache = MigrateCache(oldView, newView, deltas);
                    if (ShouldPersist)
                    {
                        SnapshotCodec.Save(
                            _options.SnapshotPath!,
                            newView,
                            _hazardPenalty,
                            newHistory,
                            migratedCache);
                    }

                    _cache = migratedCache;
                    _history = newHistory;
                    Volatile.Write(ref _published, newView);
                }

                return new BatchResult(
                    ErrorCodes.Ok,
                    newVersion,
                    newSequence,
                    link.ChainHash,
                    deltas.OrderBy(delta => delta.Key.UnitId, StringComparer.Ordinal).ThenBy(delta => delta.Key.Target).ToArray());
            }
            catch (Exception exception)
            {
                return Failure(
                    ErrorCodes.BatchAborted,
                    $"Batch aborted: {exception.GetType().Name}.");
            }
        }
    }

    private BatchResult RecalculateCache(FieldKey key)
    {
        lock (_publishGate)
        {
            var view = Current;
            if (!view.Fields.ContainsKey(key))
            {
                return Failure(ErrorCodes.NotFound, "Field not found.");
            }

            lock (_cacheGate)
            {
                var removed = _cache
                    .Where(pair => pair.Key.Field == key)
                    .ToDictionary(pair => pair.Key, pair => pair.Value);
                foreach (var cacheKey in removed.Keys)
                {
                    _cache.Remove(cacheKey);
                }

                if (ShouldPersist)
                {
                    try
                    {
                        SnapshotCodec.Save(
                            _options.SnapshotPath!,
                            view,
                            _hazardPenalty,
                            _history,
                            _cache);
                    }
                    catch (Exception exception)
                    {
                        foreach (var pair in removed)
                        {
                            _cache[pair.Key] = pair.Value;
                        }

                        return Failure(ErrorCodes.BatchAborted, $"Cache persistence failed: {exception.GetType().Name}.");
                    }
                }
            }

            return new BatchResult(
                ErrorCodes.Ok,
                view.Version,
                view.BatchSequence,
                view.ChainHash,
                Array.Empty<FieldDelta>());
        }
    }

    private Dictionary<PathCacheKey, PathCacheEntry> MigrateCache(
        BattlefieldView oldView,
        BattlefieldView newView,
        IReadOnlyList<FieldDelta> deltas)
    {
        var changed = deltas.ToDictionary(delta => delta.Key);
        var result = new Dictionary<PathCacheKey, PathCacheEntry>();
        foreach (var entry in _cache.Values)
        {
            var fieldKey = entry.Key.Field;
            if (!oldView.Fields.TryGetValue(fieldKey, out var oldField)
                || !newView.Fields.TryGetValue(fieldKey, out var newField))
            {
                continue;
            }

            if (entry.Key.FieldVersion != oldField.FieldVersion
                || !string.Equals(entry.Key.FieldHash, oldField.ContentHash, StringComparison.Ordinal))
            {
                continue;
            }

            if (oldField.FieldVersion == newField.FieldVersion
                && string.Equals(oldField.ContentHash, newField.ContentHash, StringComparison.Ordinal))
            {
                result[entry.Key] = entry;
                continue;
            }

            if (!changed.TryGetValue(fieldKey, out var delta) || Intersects(delta.ImpactCells, entry.Dependencies))
            {
                continue;
            }

            var migrated = entry.Migrate(new PathCacheKey(
                fieldKey,
                entry.Key.StartCell,
                newField.FieldVersion,
                newField.ContentHash));
            result[migrated.Key] = migrated;
        }

        return result;
    }

    private static (BattleGrid Grid, Dictionary<string, UnitState> Units) ApplyChanges(
        BattlefieldView view,
        IReadOnlyList<BattleChange> changes)
    {
        var baseCosts = view.Grid.CopyBaseCosts();
        var passMasks = view.Grid.CopyPassMasks();
        var hazards = view.Grid.CopyHazards();
        var obstacles = view.Grid.CopyObstacles();
        var units = view.Units.ToDictionary(pair => pair.Key, pair => pair.Value, StringComparer.Ordinal);

        foreach (var change in changes)
        {
            if (change is null || !Enum.IsDefined(change.Kind))
            {
                throw new InvalidOperationException("Unknown batch change.");
            }

            switch (change.Kind)
            {
                case BattleChangeKind.SetObstacle:
                    ValidateCell(change.CellIndex, view.Grid.CellCount);
                    obstacles[change.CellIndex] = change.BoolValue;
                    break;
                case BattleChangeKind.SetHazard:
                    ValidateCell(change.CellIndex, view.Grid.CellCount);
                    hazards[change.CellIndex] = change.BoolValue;
                    break;
                case BattleChangeKind.SetUnitTraversalMask:
                    if (string.IsNullOrWhiteSpace(change.UnitId) || !units.ContainsKey(change.UnitId))
                    {
                        throw new InvalidOperationException("Unknown unit in batch change.");
                    }

                    units[change.UnitId] = units[change.UnitId].WithMask(change.MaskValue);
                    break;
                default:
                    throw new InvalidOperationException("Unknown batch change.");
            }
        }

        var grid = new BattleGrid(view.Grid.Width, view.Grid.Height, baseCosts, passMasks, hazards, obstacles);
        return (grid, units);
    }

    private static bool TryResolveReservationKeys(
        BattlefieldView view,
        IReadOnlyList<BattleChange> changes,
        out List<FieldKey> keys,
        out string? error)
    {
        var set = new SortedSet<FieldKey>(FieldKeyComparer.Instance);
        foreach (var change in changes)
        {
            if (change is null || !Enum.IsDefined(change.Kind))
            {
                keys = new List<FieldKey>();
                error = "Unknown batch change.";
                return false;
            }

            switch (change.Kind)
            {
                case BattleChangeKind.SetObstacle:
                case BattleChangeKind.SetHazard:
                    if (change.CellIndex < 0 || change.CellIndex >= view.Grid.CellCount)
                    {
                        keys = new List<FieldKey>();
                        error = "Batch cell index is out of range.";
                        return false;
                    }

                    foreach (var key in view.Fields.Keys)
                    {
                        set.Add(key);
                    }

                    break;
                case BattleChangeKind.SetUnitTraversalMask:
                    if (string.IsNullOrWhiteSpace(change.UnitId) || !view.Units.ContainsKey(change.UnitId))
                    {
                        keys = new List<FieldKey>();
                        error = "Batch references an unknown unit.";
                        return false;
                    }

                    foreach (var key in view.Fields.Keys.Where(key => string.Equals(key.UnitId, change.UnitId, StringComparison.Ordinal)))
                    {
                        set.Add(key);
                    }

                    break;
                default:
                    keys = new List<FieldKey>();
                    error = "Unknown batch change.";
                    return false;
            }
        }

        keys = set.ToList();
        error = null;
        return true;
    }

    private bool TryReserve(IReadOnlyList<FieldKey> keys)
    {
        var acquired = new List<FieldKey>(keys.Count);
        foreach (var key in keys)
        {
            if (!_activeRecalculations.TryAdd(key, 0))
            {
                foreach (var acquiredKey in acquired)
                {
                    _activeRecalculations.TryRemove(acquiredKey, out _);
                }

                return false;
            }

            acquired.Add(key);
        }

        return true;
    }

    private void Release(IReadOnlyList<FieldKey> keys)
    {
        foreach (var key in keys)
        {
            _activeRecalculations.TryRemove(key, out _);
        }
    }

    private long EnqueueTicket()
    {
        lock (_orderGate)
        {
            return _nextTicket++;
        }
    }

    private void WaitTicket(long ticket)
    {
        lock (_orderGate)
        {
            while (_servingTicket != ticket)
            {
                Monitor.Wait(_orderGate);
            }
        }
    }

    private void CompleteTicket(long ticket)
    {
        lock (_orderGate)
        {
            if (_servingTicket == ticket)
            {
                _servingTicket++;
                Monitor.PulseAll(_orderGate);
            }
        }
    }

    private bool ShouldPersist => _options.PersistSnapshots && !string.IsNullOrWhiteSpace(_options.SnapshotPath);

    private BatchResult Failure(string code, string message)
    {
        var view = Current;
        return new BatchResult(code, view.Version, view.BatchSequence, view.ChainHash, Array.Empty<FieldDelta>(), message);
    }

    private static bool IsStale(
        CostFieldSnapshot field,
        long? expectedFieldVersion,
        string? expectedFieldHash)
    {
        if (expectedFieldVersion.HasValue && expectedFieldVersion.Value != field.FieldVersion)
        {
            return true;
        }

        return expectedFieldHash is not null
            && !string.Equals(expectedFieldHash, field.ContentHash, StringComparison.Ordinal);
    }

    private static bool Intersects(int[] left, int[] right)
    {
        var leftIndex = 0;
        var rightIndex = 0;
        while (leftIndex < left.Length && rightIndex < right.Length)
        {
            if (left[leftIndex] == right[rightIndex])
            {
                return true;
            }

            if (left[leftIndex] < right[rightIndex])
            {
                leftIndex++;
            }
            else
            {
                rightIndex++;
            }
        }

        return false;
    }

    private static void ValidateCell(int cell, int cellCount)
    {
        if (cell < 0 || cell >= cellCount)
        {
            throw new ArgumentOutOfRangeException(nameof(cell), "Batch cell index is out of range.");
        }
    }
}
