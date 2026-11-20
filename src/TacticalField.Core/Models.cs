using System.Collections.ObjectModel;
using System.Text;

namespace TacticalField.Core;

public enum BattleChangeKind
{
    SetObstacle,
    SetHazard,
    SetUnitTraversalMask
}

public sealed record BattleChange(
    BattleChangeKind Kind,
    int CellIndex,
    bool BoolValue,
    string UnitId,
    int MaskValue)
{
    public static BattleChange SetObstacle(int cellIndex, bool blocked)
        => new(BattleChangeKind.SetObstacle, cellIndex, blocked, string.Empty, 0);

    public static BattleChange SetHazard(int cellIndex, bool active)
        => new(BattleChangeKind.SetHazard, cellIndex, active, string.Empty, 0);

    public static BattleChange SetUnitMask(string unitId, int mask)
        => new(BattleChangeKind.SetUnitTraversalMask, -1, false, unitId, mask);

    public string Canonical()
    {
        return Kind switch
        {
            BattleChangeKind.SetObstacle => $"set_obstacle[{CellIndex}]={BoolValue.ToString().ToLowerInvariant()}",
            BattleChangeKind.SetHazard => $"set_hazard[{CellIndex}]={BoolValue.ToString().ToLowerInvariant()}",
            BattleChangeKind.SetUnitTraversalMask => $"set_unit_mask[{UnitId}]={MaskValue}",
            _ => throw new InvalidOperationException("Unknown change kind.")
        };
    }
}

public sealed class UnitState
{
    public UnitState(string id, int traversalMask, int startCell, IEnumerable<int> targets)
    {
        if (string.IsNullOrWhiteSpace(id))
        {
            throw new ArgumentException("Unit id is required.", nameof(id));
        }

        Id = id;
        TraversalMask = traversalMask;
        StartCell = startCell;
        var ordered = targets.Distinct().OrderBy(value => value).ToArray();
        if (ordered.Length == 0)
        {
            throw new ArgumentException("At least one target is required.", nameof(targets));
        }

        Targets = Array.AsReadOnly(ordered);
    }

    public string Id { get; }
    public int TraversalMask { get; }
    public int StartCell { get; }
    public IReadOnlyList<int> Targets { get; }

    public UnitState WithMask(int traversalMask) => new(Id, traversalMask, StartCell, Targets);

    public UnitState WithStart(int startCell) => new(Id, TraversalMask, startCell, Targets);
}

public sealed record FieldKey(string UnitId, int Target)
{
    public override string ToString() => $"{UnitId}@{Target}";
}

public sealed class ChainLink
{
    public ChainLink(
        long batchSequence,
        long publicationVersion,
        string previousChainHash,
        string stateHash,
        string chainHash)
    {
        BatchSequence = batchSequence;
        PublicationVersion = publicationVersion;
        PreviousChainHash = previousChainHash;
        StateHash = stateHash;
        ChainHash = chainHash;
    }

    public long BatchSequence { get; }
    public long PublicationVersion { get; }
    public string PreviousChainHash { get; }
    public string StateHash { get; }
    public string ChainHash { get; }
}

public sealed class BattlefieldView
{
    private readonly IReadOnlyDictionary<string, UnitState> _units;
    private readonly IReadOnlyDictionary<FieldKey, CostFieldSnapshot> _fields;

    public BattlefieldView(
        long version,
        long batchSequence,
        BattleGrid grid,
        IReadOnlyDictionary<string, UnitState> units,
        IReadOnlyDictionary<FieldKey, CostFieldSnapshot> fields,
        string chainHash)
    {
        Version = version;
        BatchSequence = batchSequence;
        Grid = grid;
        var unitCopy = units.ToDictionary(pair => pair.Key, pair => pair.Value, StringComparer.Ordinal);
        var fieldCopy = fields.ToDictionary(pair => pair.Key, pair => pair.Value);
        _units = new ReadOnlyDictionary<string, UnitState>(
            new SortedDictionary<string, UnitState>(unitCopy, StringComparer.Ordinal));
        _fields = new ReadOnlyDictionary<FieldKey, CostFieldSnapshot>(
            new SortedDictionary<FieldKey, CostFieldSnapshot>(fieldCopy, FieldKeyComparer.Instance));
        ChainHash = chainHash;
    }

    public long Version { get; }
    public long BatchSequence { get; }
    public BattleGrid Grid { get; }
    public IReadOnlyDictionary<string, UnitState> Units => _units;
    public IReadOnlyDictionary<FieldKey, CostFieldSnapshot> Fields => _fields;
    public string ChainHash { get; }
}

public sealed record PathQueryResult(
    string Code,
    string UnitId,
    int Target,
    int StartCell,
    long FieldVersion,
    string FieldHash,
    long PublicationVersion,
    long Cost,
    int[] Cells,
    string PathHash,
    bool CacheHit,
    int[] Dependencies,
    string? Message = null)
{
    public static PathQueryResult Error(
        string code,
        string unitId,
        int target,
        int startCell,
        long publicationVersion,
        string? message = null)
        => new(code, unitId, target, startCell, 0, string.Empty, publicationVersion, -1, Array.Empty<int>(), string.Empty, false, Array.Empty<int>(), message);
}

public sealed record CostQueryResult(
    string Code,
    string UnitId,
    int Target,
    long FieldVersion,
    string FieldHash,
    long PublicationVersion,
    long Cost,
    string? Message = null);

public sealed record CostFieldView(
    string Code,
    FieldKey Key,
    long FieldVersion,
    long PublicationVersion,
    string FieldHash,
    int UnitMask,
    long[] Costs,
    int[] Next,
    string? Message = null);

public sealed class FieldDelta
{
    public FieldDelta(
        FieldKey key,
        int[] impactCells,
        int[] recomputedCells,
        int reusedCells,
        long oldFieldVersion,
        long newFieldVersion,
        string oldHash,
        string newHash)
    {
        Key = key;
        ImpactCells = impactCells;
        RecomputedCells = recomputedCells;
        ReusedCells = reusedCells;
        OldFieldVersion = oldFieldVersion;
        NewFieldVersion = newFieldVersion;
        OldHash = oldHash;
        NewHash = newHash;
    }

    public FieldKey Key { get; }
    public int[] ImpactCells { get; }
    public int[] RecomputedCells { get; }
    public int ReusedCells { get; }
    public long OldFieldVersion { get; }
    public long NewFieldVersion { get; }
    public string OldHash { get; }
    public string NewHash { get; }
    public bool FieldReused => OldFieldVersion == NewFieldVersion && string.Equals(OldHash, NewHash, StringComparison.Ordinal);
}

public sealed record BatchResult(
    string Code,
    long PublicationVersion,
    long BatchSequence,
    string ChainHash,
    FieldDelta[] Deltas,
    string? Message = null)
{
    public bool IsSuccess => string.Equals(Code, ErrorCodes.Ok, StringComparison.Ordinal);
}

public sealed record RecalcSubmissionResult(
    string Code,
    FieldKey? Key,
    Task<BatchResult>? Task,
    string? Message = null)
{
    public bool IsSuccess => string.Equals(Code, ErrorCodes.Ok, StringComparison.Ordinal);
}

public sealed record EngineOpenResult(
    string Code,
    BattlefieldEngine? Engine,
    string? Message = null)
{
    public bool IsSuccess => string.Equals(Code, ErrorCodes.Ok, StringComparison.Ordinal);
}

public sealed class EngineOptions
{
    public string? SnapshotPath { get; init; }
    public int HazardPenalty { get; init; } = 5;
    public bool PersistSnapshots { get; init; } = true;
}

public sealed class BattlefieldDefinition
{
    public BattlefieldDefinition(BattleGrid grid, IEnumerable<UnitState> units)
    {
        Grid = grid;
        var unitArray = units.ToArray();
        if (unitArray.Length == 0)
        {
            throw new ArgumentException("At least one unit is required.", nameof(units));
        }

        if (unitArray.Select(unit => unit.Id).Distinct(StringComparer.Ordinal).Count() != unitArray.Length)
        {
            throw new ArgumentException("Unit ids must be unique.", nameof(units));
        }

        foreach (var unit in unitArray)
        {
            if (unit.StartCell < 0 || unit.StartCell >= grid.CellCount)
            {
                throw new ArgumentOutOfRangeException(nameof(units), $"Invalid start cell for unit {unit.Id}.");
            }

            foreach (var target in unit.Targets)
            {
                if (target < 0 || target >= grid.CellCount)
                {
                    throw new ArgumentOutOfRangeException(nameof(units), $"Invalid target for unit {unit.Id}.");
                }
            }
        }

        Units = Array.AsReadOnly(unitArray);
    }

    public BattleGrid Grid { get; }
    public IReadOnlyList<UnitState> Units { get; }
}

internal sealed class FieldKeyComparer : IComparer<FieldKey>
{
    public static readonly FieldKeyComparer Instance = new();

    public int Compare(FieldKey? x, FieldKey? y)
    {
        if (x is null)
        {
            return y is null ? 0 : -1;
        }

        if (y is null)
        {
            return 1;
        }

        var unit = string.CompareOrdinal(x.UnitId, y.UnitId);
        return unit != 0 ? unit : x.Target.CompareTo(y.Target);
    }
}

internal static class CanonicalText
{
    public static string JoinInts(IEnumerable<int> values)
    {
        var builder = new StringBuilder();
        var first = true;
        foreach (var value in values)
        {
            if (!first)
            {
                builder.Append(',');
            }

            builder.Append(value.ToString(System.Globalization.CultureInfo.InvariantCulture));
            first = false;
        }

        return builder.ToString();
    }
}

