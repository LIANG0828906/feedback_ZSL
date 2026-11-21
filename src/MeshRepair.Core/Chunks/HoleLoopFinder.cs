using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Core.Chunks;

public static class HoleLoopFinder
{
    public static IReadOnlyList<HoleLoop> Find(MeshVersion version)
    {
        var faceEntries = version.Chunks.Values
            .SelectMany(chunk => chunk.Faces.Select(face => (Chunk: chunk.Id, Face: face)))
            .ToArray();
        var edgeIncidence = new Dictionary<EdgeKey, List<(ChunkId Chunk, ulong FaceId)>>();
        foreach (var entry in faceEntries)
        {
            for (var corner = 0; corner < 3; corner++)
            {
                var a = entry.Face.Triangle[corner];
                var b = entry.Face.Triangle[(corner + 1) % 3];
                if (a == b)
                {
                    continue;
                }

                var key = EdgeKey.Create(a, b);
                if (!edgeIncidence.TryGetValue(key, out var incidences))
                {
                    incidences = new List<(ChunkId, ulong)>();
                    edgeIncidence.Add(key, incidences);
                }

                incidences.Add((entry.Chunk, entry.Face.FaceId));
            }
        }

        var openEdges = edgeIncidence
            .Where(pair => pair.Value.Count == 1)
            .OrderBy(pair => pair.Key)
            .ToDictionary(pair => pair.Key, pair => pair.Value[0].Chunk);
        var adjacency = new Dictionary<int, List<int>>();
        foreach (var edge in openEdges.Keys)
        {
            AddNeighbor(adjacency, edge.Low, edge.High);
            AddNeighbor(adjacency, edge.High, edge.Low);
        }

        var visited = new HashSet<EdgeKey>();
        var loops = new List<HoleLoop>();
        foreach (var startEdge in openEdges.Keys)
        {
            if (visited.Contains(startEdge))
            {
                continue;
            }

            var path = TraverseLoop(startEdge, openEdges.Keys.ToHashSet(), adjacency, visited);
            if (!path.HasValue || path.Value.Vertices.Count < 3)
            {
                continue;
            }

            var vertexIds = path.Value.Vertices;
            var loopEdges = path.Value.Edges;
            var chunkSet = new HashSet<ChunkId>();
            foreach (var edgeKey in loopEdges)
            {
                if (openEdges.TryGetValue(edgeKey, out var chunk))
                {
                    chunkSet.Add(chunk);
                }
            }

            var orderedChunks = chunkSet.Order().ToArray();
            var orderedEdges = loopEdges.OrderBy(edge => edge).ToArray();
            var loopId = HashUtil.StableId("HL", orderedEdges.Select(edge => edge.Id));
            loops.Add(new HoleLoop(loopId, vertexIds, orderedEdges, orderedChunks));
        }

        return loops
            .OrderBy(loop => loop.LoopId, StringComparer.Ordinal)
            .ToArray();
    }

    private static (IReadOnlyList<int> Vertices, IReadOnlyList<EdgeKey> Edges)? TraverseLoop(
        EdgeKey startEdge,
        HashSet<EdgeKey> openEdges,
        IReadOnlyDictionary<int, List<int>> adjacency,
        HashSet<EdgeKey> visited)
    {
        var start = startEdge.Low;
        var current = startEdge.High;
        var previous = start;
        var vertices = new List<int> { start, current };
        var edges = new List<EdgeKey> { startEdge };
        visited.Add(startEdge);
        var guard = openEdges.Count + 1;
        while (guard-- > 0)
        {
            if (current == start)
            {
                return (vertices, edges);
            }

            if (!adjacency.TryGetValue(current, out var neighbors))
            {
                return null;
            }

            var next = neighbors
                .Where(neighbor => neighbor != previous)
                .Select(neighbor => EdgeKey.Create(current, neighbor))
                .Where(openEdges.Contains)
                .OrderBy(edge => visited.Contains(edge))
                .ThenBy(edge => edge)
                .FirstOrDefault();
            if (next == default)
            {
                return null;
            }

            visited.Add(next);
            var nextVertex = next.Low == current ? next.High : next.Low;
            edges.Add(next);
            previous = current;
            current = nextVertex;
            if (current != start)
            {
                vertices.Add(current);
            }
        }

        return null;
    }

    private static void AddNeighbor(Dictionary<int, List<int>> adjacency, int from, int to)
    {
        if (!adjacency.TryGetValue(from, out var neighbors))
        {
            neighbors = new List<int>();
            adjacency.Add(from, neighbors);
        }

        if (!neighbors.Contains(to))
        {
            neighbors.Add(to);
        }
    }
}
