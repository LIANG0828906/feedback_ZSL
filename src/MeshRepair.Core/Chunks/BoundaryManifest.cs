using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;

namespace MeshRepair.Core.Chunks;

public sealed record BoundaryEdge(string Id, int A, int B, ChunkId ChunkA, ChunkId ChunkB);

public sealed class BoundaryManifest
{
    public BoundaryManifest(
        ChunkPair pair,
        long manifestVersion,
        long chunkAVersion,
        long chunkBVersion,
        string chunkAContentHash,
        string chunkBContentHash,
        IReadOnlyList<int> sharedVertexIds,
        IReadOnlyList<BoundaryEdge> edges,
        IReadOnlyDictionary<string, int> stableVertexIds,
        string? previousContentHash,
        string contentHash)
    {
        Pair = pair;
        ManifestVersion = manifestVersion;
        ChunkAVersion = chunkAVersion;
        ChunkBVersion = chunkBVersion;
        ChunkAContentHash = chunkAContentHash;
        ChunkBContentHash = chunkBContentHash;
        SharedVertexIds = sharedVertexIds;
        Edges = edges;
        StableVertexIds = stableVertexIds;
        PreviousContentHash = previousContentHash;
        ContentHash = contentHash;
    }

    public ChunkPair Pair { get; }

    public long ManifestVersion { get; }

    public long ChunkAVersion { get; }

    public long ChunkBVersion { get; }

    public string ChunkAContentHash { get; }

    public string ChunkBContentHash { get; }

    public IReadOnlyList<int> SharedVertexIds { get; }

    public IReadOnlyList<BoundaryEdge> Edges { get; }

    public IReadOnlyDictionary<string, int> StableVertexIds { get; }

    public string? PreviousContentHash { get; }

    public string ContentHash { get; }

    public bool MatchesChunks(ChunkSnapshot a, ChunkSnapshot b)
        => Pair.A == a.Id
            && Pair.B == b.Id
            && ManifestVersion == a.Version
            && ManifestVersion == b.Version
            && ChunkAVersion == a.Version
            && ChunkBVersion == b.Version
            && ChunkAContentHash == a.ContentHash
            && ChunkBContentHash == b.ContentHash;

    public static BoundaryManifest Create(
        ChunkPair pair,
        long manifestVersion,
        ChunkSnapshot chunkA,
        ChunkSnapshot chunkB,
        IReadOnlyList<int> sharedVertexIds,
        IReadOnlyList<BoundaryEdge> edges,
        IReadOnlyDictionary<string, int> stableVertexIds,
        string? previousContentHash)
    {
        var orderedVertices = sharedVertexIds.Distinct().Order().ToArray();
        var orderedEdges = edges
            .OrderBy(edge => edge.A)
            .ThenBy(edge => edge.B)
            .ToArray();
        var orderedStable = stableVertexIds
            .OrderBy(pairItem => pairItem.Key, StringComparer.Ordinal)
            .ToArray();
        var lines = new List<string>
        {
            $"pair={pair.Id}",
            $"manifestVersion={manifestVersion}",
            $"chunkA={chunkA.Id}@{chunkA.Version}:{chunkA.ContentHash}",
            $"chunkB={chunkB.Id}@{chunkB.Version}:{chunkB.ContentHash}",
            $"previous={previousContentHash ?? "none"}"
        };
        lines.AddRange(orderedVertices.Select(id => $"v={id}"));
        lines.AddRange(orderedEdges.Select(edge => $"e={edge.Id};{edge.A},{edge.B};{edge.ChunkA}|{edge.ChunkB}"));
        lines.AddRange(orderedStable.Select(entry => $"s={entry.Key}={entry.Value}"));
        var contentHash = HashUtil.Sha256Hex(string.Join("\n", lines));
        return new BoundaryManifest(
            pair,
            manifestVersion,
            chunkA.Version,
            chunkB.Version,
            chunkA.ContentHash,
            chunkB.ContentHash,
            orderedVertices,
            orderedEdges,
            new Dictionary<string, int>(orderedStable.ToDictionary(entry => entry.Key, entry => entry.Value), StringComparer.Ordinal),
            previousContentHash,
            contentHash);
    }
}
