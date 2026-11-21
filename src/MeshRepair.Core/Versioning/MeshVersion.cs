using MeshRepair.Core.Chunks;
using MeshRepair.Core.Geometry;

namespace MeshRepair.Core.Versioning;

public enum MeshEditKind
{
    Load,
    Weld,
    FillHole,
    DeleteFragments,
    RecomputeNormals,
    Rollback
}

public sealed record MeshEditInfo(
    MeshEditKind Kind,
    string Editor,
    IReadOnlyList<ChunkId> AffectedBlocks,
    long? BoundaryManifestVersion,
    string? BoundaryContentHash,
    int WeldVertexReduction,
    int FilledFaceCount,
    long CommitWatermark);

public sealed class MeshVersion
{
    public MeshVersion(
        long version,
        long parentVersion,
        string editor,
        MeshEditInfo edit,
        IReadOnlyDictionary<ChunkId, ChunkSnapshot> chunks,
        IReadOnlyDictionary<ChunkPair, BoundaryManifest> boundaries,
        IReadOnlyDictionary<ulong, FaceAssignment> assignments,
        int vertexCount,
        int faceCount,
        string contentHash,
        long? revertedFromVersion = null)
    {
        Version = version;
        ParentVersion = parentVersion;
        Editor = editor;
        Edit = edit;
        Chunks = chunks;
        Boundaries = boundaries;
        Assignments = assignments;
        VertexCount = vertexCount;
        FaceCount = faceCount;
        ContentHash = contentHash;
        RevertedFromVersion = revertedFromVersion;
    }

    public long Version { get; }

    public long ParentVersion { get; }

    public string Editor { get; }

    public MeshEditInfo Edit { get; }

    public IReadOnlyDictionary<ChunkId, ChunkSnapshot> Chunks { get; }

    public IReadOnlyDictionary<ChunkPair, BoundaryManifest> Boundaries { get; }

    public IReadOnlyDictionary<ulong, FaceAssignment> Assignments { get; }

    public int VertexCount { get; }

    public int FaceCount { get; }

    public string ContentHash { get; }

    public long? RevertedFromVersion { get; }

    public static string ComputeContentHash(
        long version,
        IReadOnlyDictionary<ChunkId, ChunkSnapshot> chunks,
        IReadOnlyDictionary<ChunkPair, BoundaryManifest> boundaries,
        int vertexCount,
        int faceCount)
    {
        var lines = new List<string>
        {
            $"version={version}",
            $"vertices={vertexCount}",
            $"faces={faceCount}"
        };
        lines.AddRange(chunks.OrderBy(pair => pair.Key).Select(pair => $"c={pair.Key}:{pair.Value.ContentHash}"));
        lines.AddRange(boundaries.OrderBy(pair => pair.Key).Select(pair => $"b={pair.Key}:{pair.Value.ContentHash}"));
        return HashUtil.Sha256Hex(string.Join("\n", lines));
    }

    public MeshVersion WithRecomputedPublication(
        long version,
        long parentVersion,
        string editor,
        MeshEditInfo edit,
        IReadOnlyDictionary<ChunkId, ChunkSnapshot> chunks,
        IReadOnlyDictionary<ChunkPair, BoundaryManifest> boundaries,
        IReadOnlyDictionary<ulong, FaceAssignment> assignments,
        int vertexCount,
        int faceCount,
        long? revertedFromVersion)
    {
        var hash = ComputeContentHash(version, chunks, boundaries, vertexCount, faceCount);
        return new MeshVersion(
            version,
            parentVersion,
            editor,
            edit,
            chunks,
            boundaries,
            assignments,
            vertexCount,
            faceCount,
            hash,
            revertedFromVersion);
    }
}
