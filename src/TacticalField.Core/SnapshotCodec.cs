using System.Text.Json;

namespace TacticalField.Core;

internal sealed record SnapshotLoadResult(
    string Code,
    LoadedSnapshot? Snapshot,
    string? Message)
{
    public bool IsSuccess => string.Equals(Code, ErrorCodes.Ok, StringComparison.Ordinal);
}

internal static class SnapshotCodec
{
    public const string SnapshotMagic = "TACTICAL-FIELD-SNAPSHOT";
    public const int SnapshotFormatVersion = 1;
    internal const string ZeroChain = "0000000000000000000000000000000000000000000000000000000000000000";
    private static int _tempCounter;

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = null,
        WriteIndented = false,
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.Never
    };

    public static SnapshotStateDto CreateStateDto(BattlefieldView view, int hazardPenalty)
    {
        return new SnapshotStateDto
        {
            Version = view.Version,
            BatchSequence = view.BatchSequence,
            HazardPenalty = hazardPenalty,
            Grid = new GridDto
            {
                Width = view.Grid.Width,
                Height = view.Grid.Height,
                BaseCosts = view.Grid.CopyBaseCosts(),
                PassMasks = view.Grid.CopyPassMasks(),
                Hazards = view.Grid.CopyHazards(),
                Obstacles = view.Grid.CopyObstacles()
            },
            Units = view.Units.Values
                .OrderBy(unit => unit.Id, StringComparer.Ordinal)
                .Select(unit => new UnitDto
                {
                    Id = unit.Id,
                    TraversalMask = unit.TraversalMask,
                    StartCell = unit.StartCell,
                    Targets = unit.Targets.ToArray()
                })
                .ToArray(),
            Fields = view.Fields.Values
                .OrderBy(field => field.Key.UnitId, StringComparer.Ordinal)
                .ThenBy(field => field.Key.Target)
                .Select(field => new FieldDto
                {
                    UnitId = field.Key.UnitId,
                    Target = field.Key.Target,
                    Width = field.Width,
                    Height = field.Height,
                    UnitMask = field.UnitMask,
                    FieldVersion = field.FieldVersion,
                    BatchSequence = field.BatchSequence,
                    Costs = field.GetCostsCopy(),
                    Next = field.GetNextCopy(),
                    CellHashesHex = Hashing.Hex(field.CellHashes),
                    ContentHash = field.ContentHash
                })
                .ToArray()
        };
    }

    public static string ComputeStateHash(SnapshotStateDto state)
    {
        var bytes = JsonSerializer.SerializeToUtf8Bytes(state, JsonOptions);
        return Hashing.Hex(Hashing.Sha256(bytes));
    }

    public static string ComputeChainHash(string previousChainHash, long batchSequence, long version, string stateHash)
        => Hashing.HashHex($"TF-CHAIN-V1|{previousChainHash}|{batchSequence}|{version}|{stateHash}");

    public static ChainLink CreateChainLink(
        BattlefieldView view,
        int hazardPenalty,
        string previousChainHash)
    {
        var stateHash = ComputeStateHash(CreateStateDto(view, hazardPenalty));
        var chainHash = ComputeChainHash(previousChainHash, view.BatchSequence, view.Version, stateHash);
        return new ChainLink(view.BatchSequence, view.Version, previousChainHash, stateHash, chainHash);
    }

    public static string InitialChainHash(BattlefieldView view, int hazardPenalty)
        => CreateChainLink(view, hazardPenalty, ZeroChain).ChainHash;

    public static void Save(
        string path,
        BattlefieldView view,
        int hazardPenalty,
        IReadOnlyList<ChainLink> history,
        IReadOnlyDictionary<PathCacheKey, PathCacheEntry> cache)
    {
        var document = new SnapshotDocument
        {
            Magic = SnapshotMagic,
            FormatVersion = SnapshotFormatVersion,
            State = CreateStateDto(view, hazardPenalty),
            History = history.Select(link => new ChainLinkDto
            {
                BatchSequence = link.BatchSequence,
                PublicationVersion = link.PublicationVersion,
                PreviousChainHash = link.PreviousChainHash,
                StateHash = link.StateHash,
                ChainHash = link.ChainHash
            }).ToArray(),
            Cache = cache.Values
                .OrderBy(entry => entry.Key.Field.UnitId, StringComparer.Ordinal)
                .ThenBy(entry => entry.Key.Field.Target)
                .ThenBy(entry => entry.Key.StartCell)
                .ThenBy(entry => entry.Key.FieldVersion)
                .Select(entry => new CacheEntryDto
                {
                    UnitId = entry.Key.Field.UnitId,
                    Target = entry.Key.Field.Target,
                    StartCell = entry.Key.StartCell,
                    FieldVersion = entry.Key.FieldVersion,
                    FieldHash = entry.Key.FieldHash,
                    PathHash = entry.PathHash,
                    Cells = (int[])entry.Cells.Clone(),
                    Dependencies = (int[])entry.Dependencies.Clone()
                }).ToArray()
        };

        var bytes = SerializeWithIntegrity(document);
        var directory = Path.GetDirectoryName(Path.GetFullPath(path));
        if (!string.IsNullOrEmpty(directory))
        {
            Directory.CreateDirectory(directory);
        }

        var temp = $"{path}.tmp.{Environment.ProcessId}.{Interlocked.Increment(ref _tempCounter)}";
        try
        {
            using (var stream = new FileStream(temp, FileMode.CreateNew, FileAccess.Write, FileShare.None))
            {
                stream.Write(bytes);
                stream.Flush(flushToDisk: true);
            }

            File.Move(temp, path, overwrite: true);
        }
        catch
        {
            try
            {
                if (File.Exists(temp))
                {
                    File.Delete(temp);
                }
            }
            catch
            {
                // The original snapshot remains untouched. A leftover temp file is ignored by recovery.
            }

            throw;
        }
    }

    public static SnapshotLoadResult Load(string path)
    {
        try
        {
            if (!File.Exists(path))
            {
                return new SnapshotLoadResult(ErrorCodes.RecoveryInvalid, null, "Snapshot file does not exist.");
            }

            var bytes = File.ReadAllBytes(path);
            var document = JsonSerializer.Deserialize<SnapshotDocument>(bytes, JsonOptions);
            if (document is null)
            {
                return Invalid("Snapshot JSON is empty.");
            }

            if (!string.Equals(document.Magic, SnapshotMagic, StringComparison.Ordinal)
                || document.FormatVersion != SnapshotFormatVersion)
            {
                return Invalid("Snapshot magic or format version is invalid.");
            }

            if (document.State is null || document.History is null || document.Cache is null)
            {
                return Invalid("Snapshot is missing required sections.");
            }

            var expectedIntegrity = document.IntegrityHash;
            if (string.IsNullOrWhiteSpace(expectedIntegrity))
            {
                return Invalid("Snapshot integrity hash is missing.");
            }

            document.IntegrityHash = null;
            var actualIntegrity = Hashing.Hex(Hashing.Sha256(JsonSerializer.SerializeToUtf8Bytes(document, JsonOptions)));
            if (!string.Equals(expectedIntegrity, actualIntegrity, StringComparison.OrdinalIgnoreCase))
            {
                return Invalid("Snapshot integrity hash does not match.");
            }

            return Materialize(document);
        }
        catch (Exception exception)
        {
            return Invalid($"Snapshot could not be parsed: {exception.GetType().Name}.");
        }
    }

    private static SnapshotLoadResult Materialize(SnapshotDocument document)
    {
        var state = document.State!;
        var gridDto = state.Grid;
        if (gridDto is null
            || gridDto.BaseCosts is null
            || gridDto.PassMasks is null
            || gridDto.Hazards is null
            || gridDto.Obstacles is null
            || state.Units is null
            || state.Fields is null)
        {
            return Invalid("Snapshot state contains missing fields.");
        }

        BattleGrid grid;
        try
        {
            grid = new BattleGrid(
                gridDto.Width,
                gridDto.Height,
                gridDto.BaseCosts,
                gridDto.PassMasks,
                gridDto.Hazards,
                gridDto.Obstacles);
        }
        catch (Exception)
        {
            return Invalid("Snapshot grid is invalid.");
        }

        var units = new Dictionary<string, UnitState>(StringComparer.Ordinal);
        foreach (var unitDto in state.Units)
        {
            if (unitDto is null || string.IsNullOrWhiteSpace(unitDto.Id) || unitDto.Targets is null)
            {
                return Invalid("Snapshot unit record is incomplete.");
            }

            try
            {
                if (!units.TryAdd(unitDto.Id, new UnitState(unitDto.Id, unitDto.TraversalMask, unitDto.StartCell, unitDto.Targets)))
                {
                    return Invalid("Snapshot contains duplicate unit ids.");
                }
            }
            catch (Exception)
            {
                return Invalid($"Snapshot unit {unitDto.Id} is invalid.");
            }
        }

        if (units.Count == 0)
        {
            return Invalid("Snapshot contains no units.");
        }

        var fields = new Dictionary<FieldKey, CostFieldSnapshot>();
        foreach (var fieldDto in state.Fields)
        {
            if (fieldDto is null
                || string.IsNullOrWhiteSpace(fieldDto.UnitId)
                || fieldDto.Costs is null
                || fieldDto.Next is null
                || string.IsNullOrWhiteSpace(fieldDto.CellHashesHex)
                || string.IsNullOrWhiteSpace(fieldDto.ContentHash))
            {
                return Invalid("Snapshot field record is incomplete.");
            }

            if (!units.TryGetValue(fieldDto.UnitId, out var unit)
                || !unit.Targets.Contains(fieldDto.Target)
                || unit.TraversalMask != fieldDto.UnitMask
                || fieldDto.FieldVersion <= 0
                || fieldDto.Width != grid.Width
                || fieldDto.Height != grid.Height)
            {
                return Invalid($"Snapshot field {fieldDto.UnitId}@{fieldDto.Target} metadata is invalid.");
            }

            if (fieldDto.Costs.Length != grid.CellCount || fieldDto.Next.Length != grid.CellCount)
            {
                return Invalid($"Snapshot field {fieldDto.UnitId}@{fieldDto.Target} has wrong dimensions.");
            }

            if (fieldDto.CellHashesHex.Length != grid.CellCount * 64)
            {
                return Invalid($"Snapshot field {fieldDto.UnitId}@{fieldDto.Target} has malformed cell hashes.");
            }

            var leaves = new byte[grid.CellCount][];
            try
            {
                for (var index = 0; index < leaves.Length; index++)
                {
                    leaves[index] = Convert.FromHexString(fieldDto.CellHashesHex.Substring(index * 64, 64));
                }
            }
            catch (FormatException)
            {
                return Invalid($"Snapshot field {fieldDto.UnitId}@{fieldDto.Target} contains invalid hash text.");
            }

            var tree = MerkleTree.Build(leaves);
            var field = new CostFieldSnapshot(
                new FieldKey(fieldDto.UnitId, fieldDto.Target),
                fieldDto.Width,
                fieldDto.Height,
                fieldDto.UnitMask,
                fieldDto.FieldVersion,
                fieldDto.BatchSequence,
                fieldDto.Costs,
                fieldDto.Next,
                fieldDto.CellHashesHex is null ? Array.Empty<byte>() : Convert.FromHexString(fieldDto.CellHashesHex),
                tree,
                CostFieldCalculator.BuildDependents(fieldDto.Next));

            if (!string.Equals(field.ContentHash, fieldDto.ContentHash, StringComparison.Ordinal))
            {
                return Invalid($"Snapshot field {fieldDto.UnitId}@{fieldDto.Target} content hash is invalid.");
            }

            var validation = ValidateField(field, grid, unit, state.HazardPenalty, leaves);
            if (validation is not null)
            {
                return Invalid(validation);
            }

            if (!fields.TryAdd(field.Key, field))
            {
                return Invalid("Snapshot contains duplicate fields.");
            }
        }

        foreach (var unit in units.Values)
        {
            foreach (var target in unit.Targets)
            {
                if (!fields.ContainsKey(new FieldKey(unit.Id, target)))
                {
                    return Invalid($"Snapshot is missing field {unit.Id}@{target}.");
                }
            }
        }

        if (state.Version <= 0 || state.BatchSequence < 0)
        {
            return Invalid("Snapshot publication counters are invalid.");
        }

        var history = new List<ChainLink>();
        string previous = ZeroChain;
        for (var index = 0; index < document.History!.Length; index++)
        {
            var link = document.History[index];
            if (link is null
                || string.IsNullOrWhiteSpace(link.PreviousChainHash)
                || string.IsNullOrWhiteSpace(link.StateHash)
                || string.IsNullOrWhiteSpace(link.ChainHash))
            {
                return Invalid("Snapshot hash chain contains an incomplete link.");
            }

            if (link.BatchSequence != index
                || link.PublicationVersion != index + 1
                || !string.Equals(link.PreviousChainHash, previous, StringComparison.Ordinal))
            {
                return Invalid("Snapshot hash chain is broken or out of order.");
            }

            var expectedChain = ComputeChainHash(previous, link.BatchSequence, link.PublicationVersion, link.StateHash);
            if (!string.Equals(expectedChain, link.ChainHash, StringComparison.Ordinal))
            {
                return Invalid("Snapshot hash chain link hash is invalid.");
            }

            history.Add(new ChainLink(
                link.BatchSequence,
                link.PublicationVersion,
                link.PreviousChainHash,
                link.StateHash,
                link.ChainHash));
            previous = link.ChainHash;
        }

        if (history.Count == 0)
        {
            return Invalid("Snapshot hash chain is empty.");
        }

        var last = history[^1];
        var expectedStateHash = ComputeStateHash(state);
        if (last.BatchSequence != state.BatchSequence
            || last.PublicationVersion != state.Version
            || !string.Equals(last.StateHash, expectedStateHash, StringComparison.Ordinal)
            || !string.Equals(last.ChainHash, ComputeChainHash(last.PreviousChainHash, state.BatchSequence, state.Version, expectedStateHash), StringComparison.Ordinal))
        {
            return Invalid("Snapshot hash chain does not match the published state.");
        }

        var cache = new Dictionary<PathCacheKey, PathCacheEntry>();
        foreach (var cacheDto in document.Cache!)
        {
            if (cacheDto is null
                || string.IsNullOrWhiteSpace(cacheDto.UnitId)
                || string.IsNullOrWhiteSpace(cacheDto.FieldHash)
                || string.IsNullOrWhiteSpace(cacheDto.PathHash)
                || cacheDto.Cells is null
                || cacheDto.Dependencies is null)
            {
                return Invalid("Snapshot path cache record is incomplete.");
            }

            var key = new FieldKey(cacheDto.UnitId, cacheDto.Target);
            if (!fields.TryGetValue(key, out var field)
                || field.FieldVersion != cacheDto.FieldVersion
                || !string.Equals(field.ContentHash, cacheDto.FieldHash, StringComparison.Ordinal)
                || cacheDto.StartCell < 0
                || cacheDto.StartCell >= grid.CellCount)
            {
                return Invalid("Snapshot path cache references a stale or invalid field.");
            }

            var entry = new PathCacheEntry(
                new PathCacheKey(key, cacheDto.StartCell, cacheDto.FieldVersion, cacheDto.FieldHash),
                field.GetCost(cacheDto.StartCell),
                cacheDto.Cells,
                cacheDto.PathHash);

            if (!entry.Dependencies.SequenceEqual(cacheDto.Dependencies) || !entry.IsValid(field))
            {
                return Invalid("Snapshot path cache entry failed validation.");
            }

            if (!cache.TryAdd(entry.Key, entry))
            {
                return Invalid("Snapshot path cache contains a duplicate key.");
            }
        }

        var view = new BattlefieldView(state.Version, state.BatchSequence, grid, units, fields, last.ChainHash);
        return new SnapshotLoadResult(
            ErrorCodes.Ok,
            new LoadedSnapshot(view, history.AsReadOnly(), cache, state.HazardPenalty),
            null);
    }

    private static string? ValidateField(
        CostFieldSnapshot field,
        BattleGrid grid,
        UnitState unit,
        int hazardPenalty,
        IReadOnlyList<byte[]> savedLeaves)
    {
        var target = field.Key.Target;
        if (field.GetCost(target) != 0 || field.GetNext(target) != -1)
        {
            return $"Snapshot field {field.Key} has an invalid target cell.";
        }

        for (var index = 0; index < grid.CellCount; index++)
        {
            var cost = field.GetCost(index);
            var next = field.GetNext(index);
            var weight = grid.GetMovementWeight(index, unit.TraversalMask, hazardPenalty);
            var expectedCellHash = CostFieldSnapshot.CreateCellHash(index, cost, next);
            if (!expectedCellHash.AsSpan().SequenceEqual(savedLeaves[index]))
            {
                return $"Snapshot field {field.Key} has an invalid cell hash at {index}.";
            }

            if (index == target)
            {
                continue;
            }

            if (cost < 0)
            {
                if (next != -1)
                {
                    return $"Snapshot field {field.Key} has a next pointer on an unreachable cell.";
                }

                continue;
            }

            if (weight == int.MaxValue || next < 0 || next >= grid.CellCount || field.GetCost(next) < 0)
            {
                return $"Snapshot field {field.Key} has an invalid finite distance at {index}.";
            }

            if (!grid.GetNeighbors(index).Contains(next) || cost != field.GetCost(next) + weight)
            {
                return $"Snapshot field {field.Key} violates the shortest-path recurrence at {index}.";
            }
        }

        for (var start = 0; start < grid.CellCount; start++)
        {
            if (field.GetCost(start) < 0)
            {
                continue;
            }

            var seen = new HashSet<int>();
            var current = start;
            while (current != target)
            {
                if (!seen.Add(current))
                {
                    return $"Snapshot field {field.Key} contains a path cycle.";
                }

                current = field.GetNext(current);
                if (current < 0 || current >= grid.CellCount)
                {
                    return $"Snapshot field {field.Key} contains a broken path.";
                }
            }
        }

        return null;
    }

    private static byte[] SerializeWithIntegrity(SnapshotDocument document)
    {
        document.IntegrityHash = null;
        var unsigned = JsonSerializer.SerializeToUtf8Bytes(document, JsonOptions);
        document.IntegrityHash = Hashing.Hex(Hashing.Sha256(unsigned));
        return JsonSerializer.SerializeToUtf8Bytes(document, JsonOptions);
    }

    private static SnapshotLoadResult Invalid(string message)
        => new(ErrorCodes.RecoveryInvalid, null, message);
}
