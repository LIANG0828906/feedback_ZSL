using System.Globalization;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;

namespace MeshRepair.Core.Chunks;

public readonly record struct FaceRecord(ulong FaceId, Triangle Triangle);

public sealed class ChunkSnapshot
{
    private readonly IReadOnlyDictionary<int, Point3> _vertices;

    public ChunkSnapshot(
        ChunkId id,
        long version,
        IReadOnlyDictionary<int, Point3> vertices,
        IReadOnlyList<FaceRecord> faces,
        int normalRevision,
        string contentHash)
    {
        Id = id;
        Version = version;
        _vertices = vertices;
        Faces = faces;
        NormalRevision = normalRevision;
        ContentHash = contentHash;
    }

    public ChunkId Id { get; }

    public long Version { get; }

    public IReadOnlyDictionary<int, Point3> Vertices => _vertices;

    public IReadOnlyList<FaceRecord> Faces { get; }

    public IReadOnlyList<int> ReferencedVertexIds => _vertices.Keys.Order().ToArray();

    public int NormalRevision { get; }

    public string ContentHash { get; }

    public bool TryGetVertex(int vertexId, out Point3 point) => _vertices.TryGetValue(vertexId, out point);

    public static ChunkSnapshot Create(
        ChunkId id,
        long version,
        IReadOnlyDictionary<int, Point3> allPositions,
        IEnumerable<FaceRecord> faces,
        int normalRevision = 0)
    {
        var orderedFaces = faces.OrderBy(face => face.FaceId).ToArray();
        var vertexIds = orderedFaces
            .SelectMany(face => new[] { face.Triangle.A, face.Triangle.B, face.Triangle.C })
            .Distinct()
            .Order()
            .ToArray();
        var vertices = vertexIds.ToDictionary(vertexId => vertexId, vertexId => allPositions[vertexId]);
        var hash = ComputeContentHash(id, vertices, orderedFaces, normalRevision);
        return new ChunkSnapshot(id, version, vertices, orderedFaces, normalRevision, hash);
    }

    public ChunkSnapshot WithVersion(long version)
        => new(Id, version, _vertices, Faces, NormalRevision, ContentHash);

    public static string ComputeContentHash(
        ChunkId id,
        IReadOnlyDictionary<int, Point3> vertices,
        IReadOnlyList<FaceRecord> faces,
        int normalRevision)
    {
        var lines = new List<string>(vertices.Count + faces.Count + 2)
        {
            $"chunk={id}",
            $"normalRevision={normalRevision.ToString(CultureInfo.InvariantCulture)}"
        };
        lines.AddRange(vertices
            .OrderBy(pair => pair.Key)
            .Select(pair => string.Create(
                CultureInfo.InvariantCulture,
                $"v={pair.Key};{pair.Value.CanonicalString()}")));
        lines.AddRange(faces
            .OrderBy(face => face.FaceId)
            .Select(face => string.Create(
                CultureInfo.InvariantCulture,
                $"f={face.FaceId};{face.Triangle.A},{face.Triangle.B},{face.Triangle.C}")));
        return HashUtil.Sha256Hex(string.Join("\n", lines));
    }
}
