using MeshRepair.Core.Chunks;
using MeshRepair.Core.Geometry;

namespace MeshRepair.Core.Versioning;

public static class VersionLookup
{
    public static int? FindVertexId(MeshVersion version, Point3 position, double tolerance = 1e-6)
        => version.Chunks.Values
            .SelectMany(chunk => chunk.Vertices)
            .Where(pair => pair.Value.DistanceTo(position) <= tolerance)
            .Select(pair => (int?)pair.Key)
            .Order()
            .FirstOrDefault();

    public static IReadOnlyList<ChunkId> ChunksAt(MeshVersion version, Point3 position)
        => version.Chunks.Values
            .Where(chunk => chunk.Vertices.Values.Any(vertex => vertex.DistanceTo(position) <= 1e-6))
            .Select(chunk => chunk.Id)
            .Order()
            .ToArray();

    public static IReadOnlyList<(int First, int Second)> FindCrossChunkDuplicates(
        MeshVersion version,
        double tolerance = 1e-6)
    {
        var entries = version.Chunks.Values
            .SelectMany(chunk => chunk.Vertices.Select(pair => (Chunk: chunk.Id, Id: pair.Key, Point: pair.Value)))
            .OrderBy(entry => entry.Id)
            .ToArray();
        var results = new List<(int, int)>();
        for (var first = 0; first < entries.Length; first++)
        {
            for (var second = first + 1; second < entries.Length; second++)
            {
                if (entries[first].Point.DistanceTo(entries[second].Point) <= tolerance
                    && entries[first].Id != entries[second].Id)
                {
                    results.Add((entries[first].Id, entries[second].Id));
                    break;
                }
            }
        }

        return results;
    }
}
