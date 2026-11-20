namespace TacticalField.Core;

public sealed class CostFieldSnapshot
{
    private readonly long[] _costs;
    private readonly int[] _next;
    private readonly byte[] _cellHashes;
    private readonly int[][] _dependents;
    private readonly MerkleTree _tree;

    internal CostFieldSnapshot(
        FieldKey key,
        int width,
        int height,
        int unitMask,
        long fieldVersion,
        long batchSequence,
        long[] costs,
        int[] next,
        byte[] cellHashes,
        MerkleTree tree,
        int[][] dependents)
    {
        Key = key;
        Width = width;
        Height = height;
        UnitMask = unitMask;
        FieldVersion = fieldVersion;
        BatchSequence = batchSequence;
        _costs = costs;
        _next = next;
        _cellHashes = cellHashes;
        _tree = tree;
        _dependents = dependents;
        ContentHash = ComputeContentHash();
        ReachableCount = _costs.Count(cost => cost >= 0);
    }

    public FieldKey Key { get; }
    public int Width { get; }
    public int Height { get; }
    public int UnitMask { get; }
    public long FieldVersion { get; }
    public long BatchSequence { get; }
    public string ContentHash { get; }
    public int ReachableCount { get; }
    public int CellCount => Width * Height;

    public long GetCost(int index) => _costs[ValidateIndex(index)];
    public int GetNext(int index) => _next[ValidateIndex(index)];
    public long[] GetCostsCopy() => (long[])_costs.Clone();
    public int[] GetNextCopy() => (int[])_next.Clone();

    public string GetCellHash(int index)
    {
        ValidateIndex(index);
        return Hashing.Hex(_cellHashes.AsSpan(index * 32, 32));
    }

    public byte[] CopyCellHashes() => (byte[])_cellHashes.Clone();

    public int[] GetDependents(int index)
    {
        ValidateIndex(index);
        return (int[])_dependents[index].Clone();
    }

    internal long[] Costs => _costs;
    internal int[] Next => _next;
    internal byte[] CellHashes => _cellHashes;
    internal MerkleTree Tree => _tree;

    internal string ComputeContentHash()
    {
        var metadata = $"TF-FIELD-CONTENT-V1|{Width}|{Height}|{UnitMask}|{Key.Target}|{Hashing.Hex(_tree.Root.Hash)}";
        return Hashing.HashHex(metadata);
    }

    internal static byte[] CreateCellHash(int index, long cost, int next)
    {
        var value = $"TF-CELL-V1|{index}|{cost.ToString(System.Globalization.CultureInfo.InvariantCulture)}|{next.ToString(System.Globalization.CultureInfo.InvariantCulture)}";
        return Hashing.Sha256Utf8(value);
    }

    private int ValidateIndex(int index)
    {
        if (index < 0 || index >= CellCount)
        {
            throw new ArgumentOutOfRangeException(nameof(index));
        }

        return index;
    }
}

