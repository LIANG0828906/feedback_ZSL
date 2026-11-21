using MeshRepair.Core.Chunks;
using MeshRepair.Core.Geometry;

namespace MeshRepair.Core.Editing;

internal sealed class MutableChunk
{
    public MutableChunk(ChunkSnapshot snapshot)
    {
        Id = snapshot.Id;
        Vertices = snapshot.Vertices.ToDictionary(pair => pair.Key, pair => pair.Value);
        Faces = snapshot.Faces.ToDictionary(face => face.FaceId, face => face);
        NormalRevision = snapshot.NormalRevision;
    }

    public ChunkId Id { get; }

    public Dictionary<int, Point3> Vertices { get; }

    public Dictionary<ulong, FaceRecord> Faces { get; }

    public int NormalRevision { get; set; }

    public void RemoveUnreferencedVertices()
    {
        var referenced = Faces.Values
            .SelectMany(face => new[] { face.Triangle.A, face.Triangle.B, face.Triangle.C })
            .ToHashSet();
        foreach (var vertexId in Vertices.Keys.Where(vertexId => !referenced.Contains(vertexId)).ToArray())
        {
            Vertices.Remove(vertexId);
        }
    }

    public ChunkSnapshot ToSnapshot(long version, IReadOnlyDictionary<int, Point3> fallbackPositions)
    {
        var positions = new Dictionary<int, Point3>(fallbackPositions);
        foreach (var pair in Vertices)
        {
            positions[pair.Key] = pair.Value;
        }

        return ChunkSnapshot.Create(
            Id,
            version,
            positions,
            Faces.Values,
            NormalRevision);
    }
}
