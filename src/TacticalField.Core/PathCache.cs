namespace TacticalField.Core;

internal readonly record struct PathCacheKey(
    FieldKey Field,
    int StartCell,
    long FieldVersion,
    string FieldHash);

internal sealed class PathCacheEntry
{
    public PathCacheEntry(PathCacheKey key, long cost, int[] cells, string pathHash)
    {
        Key = key;
        Cost = cost;
        Cells = (int[])cells.Clone();
        Dependencies = cells.Distinct().OrderBy(value => value).ToArray();
        PathHash = pathHash;
    }

    public PathCacheKey Key { get; }
    public long Cost { get; }
    public int[] Cells { get; }
    public int[] Dependencies { get; }
    public string PathHash { get; }

    public PathCacheEntry Migrate(PathCacheKey newKey)
        => new(newKey, Cost, Cells, PathHash);

    public bool IsValid(CostFieldSnapshot field)
    {
        if (Key.StartCell < 0 || Key.StartCell >= field.CellCount || Cells.Length == 0)
        {
            return false;
        }

        if (Cells[0] != Key.StartCell || Cells[^1] != field.Key.Target)
        {
            return false;
        }

        for (var index = 0; index + 1 < Cells.Length; index++)
        {
            if (field.GetNext(Cells[index]) != Cells[index + 1])
            {
                return false;
            }
        }

        var expectedPathHash = CostFieldCalculator.ComputePathHash(
            field.Key.UnitId,
            field.Key.Target,
            field.GetCost(Key.StartCell),
            Cells);
        if (!string.Equals(expectedPathHash, PathHash, StringComparison.Ordinal))
        {
            return false;
        }

        var expectedDependencies = Cells.Distinct().OrderBy(value => value).ToArray();
        return expectedDependencies.SequenceEqual(Dependencies);
    }
}

