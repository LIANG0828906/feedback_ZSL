using System.Security.Cryptography;
using System.Text;

namespace TacticalField.Core;

internal static class Hashing
{
    public static readonly byte[] ZeroHash = new byte[32];

    public static byte[] Sha256(ReadOnlySpan<byte> data) => SHA256.HashData(data);

    public static byte[] Sha256Utf8(string value) => SHA256.HashData(Encoding.UTF8.GetBytes(value));

    public static string Hex(ReadOnlySpan<byte> value) => Convert.ToHexString(value).ToLowerInvariant();

    public static string HashHex(string value) => Hex(Sha256Utf8(value));

    public static string CombineHex(string prefix, params byte[][] parts)
    {
        using var hash = IncrementalHash.CreateHash(HashAlgorithmName.SHA256);
        hash.AppendData(Encoding.UTF8.GetBytes(prefix));
        foreach (var part in parts)
        {
            hash.AppendData(part);
        }

        return Hex(hash.GetHashAndReset());
    }

    public static string CombineHex(string prefix, string first, string second)
        => CombineHex(prefix, Encoding.UTF8.GetBytes(first), Encoding.UTF8.GetBytes(second));
}

internal sealed class MerkleNode
{
    public MerkleNode(byte[] hash, int start, int end, MerkleNode? left = null, MerkleNode? right = null)
    {
        Hash = hash;
        Start = start;
        End = end;
        Left = left;
        Right = right;
    }

    public byte[] Hash { get; }
    public int Start { get; }
    public int End { get; }
    public MerkleNode? Left { get; }
    public MerkleNode? Right { get; }
}

internal sealed class MerkleTree
{
    private MerkleTree(MerkleNode root, int leafCount)
    {
        Root = root;
        LeafCount = leafCount;
    }

    public MerkleNode Root { get; }
    public int LeafCount { get; }

    public static MerkleTree Build(IReadOnlyList<byte[]> leaves)
    {
        if (leaves.Count == 0)
        {
            throw new ArgumentException("A Merkle tree needs at least one leaf.", nameof(leaves));
        }

        return new MerkleTree(BuildRange(leaves, 0, leaves.Count, null, null), leaves.Count);
    }

    public MerkleTree Update(IReadOnlyDictionary<int, byte[]> replacements)
    {
        if (replacements.Count == 0)
        {
            return this;
        }

        foreach (var index in replacements.Keys)
        {
            if (index < 0 || index >= LeafCount)
            {
                throw new ArgumentOutOfRangeException(nameof(replacements), "Leaf index out of range.");
            }
        }

        var root = BuildRange(Array.Empty<byte[]>(), 0, LeafCount, Root, replacements);
        return new MerkleTree(root, LeafCount);
    }

    private static MerkleNode BuildRange(
        IReadOnlyList<byte[]> leaves,
        int start,
        int end,
        MerkleNode? oldNode,
        IReadOnlyDictionary<int, byte[]>? replacements)
    {
        if (end - start == 1)
        {
            if (replacements is not null && replacements.TryGetValue(start, out var replacement))
            {
                return new MerkleNode(replacement, start, end);
            }

            if (oldNode is not null)
            {
                return oldNode;
            }

            return new MerkleNode(leaves[start], start, end);
        }

        if (oldNode is not null && !HasReplacementInRange(replacements, start, end))
        {
            return oldNode;
        }

        var middle = start + ((end - start) / 2);
        var left = BuildRange(leaves, start, middle, oldNode?.Left, replacements);
        var right = BuildRange(leaves, middle, end, oldNode?.Right, replacements);
        var hash = Hashing.CombineHex("TF-MERKLE-NODE-V1", left.Hash, right.Hash);
        return new MerkleNode(Convert.FromHexString(hash), start, end, left, right);
    }

    private static bool HasReplacementInRange(IReadOnlyDictionary<int, byte[]>? replacements, int start, int end)
    {
        if (replacements is null || replacements.Count == 0)
        {
            return false;
        }

        foreach (var index in replacements.Keys)
        {
            if (index >= start && index < end)
            {
                return true;
            }
        }

        return false;
    }
}

