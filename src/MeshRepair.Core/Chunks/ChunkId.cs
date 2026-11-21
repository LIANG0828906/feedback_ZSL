using System.Globalization;

namespace MeshRepair.Core.Chunks;

public readonly record struct ChunkId(int X, int Y, int Z) : IComparable<ChunkId>
{
    public int CompareTo(ChunkId other)
    {
        var x = X.CompareTo(other.X);
        if (x != 0)
        {
            return x;
        }

        var y = Y.CompareTo(other.Y);
        return y != 0 ? y : Z.CompareTo(other.Z);
    }

    public override string ToString()
        => string.Create(CultureInfo.InvariantCulture, $"{X},{Y},{Z}");

    public static ChunkId Parse(string value)
    {
        var parts = value.Split(',');
        if (parts.Length != 3)
        {
            throw new FormatException($"invalid chunk id '{value}'");
        }

        return new ChunkId(
            int.Parse(parts[0], CultureInfo.InvariantCulture),
            int.Parse(parts[1], CultureInfo.InvariantCulture),
            int.Parse(parts[2], CultureInfo.InvariantCulture));
    }
}

public readonly record struct ChunkPair(ChunkId A, ChunkId B) : IComparable<ChunkPair>
{
    public static ChunkPair Create(ChunkId first, ChunkId second)
    {
        if (first == second)
        {
            throw new ArgumentException("a chunk pair requires two distinct chunks");
        }

        return first.CompareTo(second) <= 0
            ? new ChunkPair(first, second)
            : new ChunkPair(second, first);
    }

    public string Id => $"{A}|{B}";

    public bool Contains(ChunkId chunk) => chunk == A || chunk == B;

    public int CompareTo(ChunkPair other) => string.CompareOrdinal(Id, other.Id);

    public override string ToString() => Id;
}
