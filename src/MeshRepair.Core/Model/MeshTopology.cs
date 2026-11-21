using MeshRepair.Core.Geometry;

namespace MeshRepair.Core.Model;

public readonly record struct EdgeKey(int Low, int High) : IComparable<EdgeKey>
{
    public static EdgeKey Create(int a, int b) => a <= b ? new EdgeKey(a, b) : new EdgeKey(b, a);

    public bool IsSelfEdge => Low == High;

    public string Id => $"E:{Low}-{High}";

    public int CompareTo(EdgeKey other)
    {
        var low = Low.CompareTo(other.Low);
        return low != 0 ? low : High.CompareTo(other.High);
    }
}

public sealed record FaceEdgeRef(int FaceIndex, ulong FaceId, int Corner, bool Forward);

public sealed class EdgeInfo
{
    public EdgeInfo(EdgeKey key, IReadOnlyList<FaceEdgeRef> incidences)
    {
        Key = key;
        Incidences = incidences;
    }

    public EdgeKey Key { get; }

    public IReadOnlyList<FaceEdgeRef> Incidences { get; }
}

public sealed class MeshTopology
{
    private MeshTopology(
        IReadOnlyList<IReadOnlyList<int>> vertexAdjacency,
        IReadOnlyDictionary<EdgeKey, EdgeInfo> edges)
    {
        VertexAdjacency = vertexAdjacency;
        Edges = edges;
    }

    public IReadOnlyList<IReadOnlyList<int>> VertexAdjacency { get; }

    public IReadOnlyDictionary<EdgeKey, EdgeInfo> Edges { get; }

    public IEnumerable<EdgeInfo> BoundaryEdges => Edges.Values.Where(edge => edge.Incidences.Count == 1);

    public IEnumerable<EdgeInfo> NonManifoldEdges => Edges.Values.Where(edge => edge.Incidences.Count > 2);

    public static MeshTopology Build(
        IReadOnlyList<Point3> vertices,
        IReadOnlyList<Triangle> faces,
        Func<int, ulong>? faceIdForIndex = null)
    {
        var adjacency = Enumerable.Range(0, vertices.Count)
            .Select(_ => new HashSet<int>())
            .ToArray();
        var edgeMap = new Dictionary<EdgeKey, List<FaceEdgeRef>>();

        for (var faceIndex = 0; faceIndex < faces.Count; faceIndex++)
        {
            var face = faces[faceIndex];
            var faceId = faceIdForIndex?.Invoke(faceIndex) ?? (ulong)(faceIndex + 1);
            for (var corner = 0; corner < 3; corner++)
            {
                var a = face[corner];
                var b = face[(corner + 1) % 3];
                if (a == b)
                {
                    continue;
                }

                adjacency[a].Add(b);
                adjacency[b].Add(a);
                var key = EdgeKey.Create(a, b);
                if (!edgeMap.TryGetValue(key, out var incidences))
                {
                    incidences = new List<FaceEdgeRef>();
                    edgeMap.Add(key, incidences);
                }

                incidences.Add(new FaceEdgeRef(faceIndex, faceId, corner, a < b));
            }
        }

        var orderedEdges = edgeMap
            .OrderBy(pair => pair.Key)
            .ToDictionary(pair => pair.Key, pair => new EdgeInfo(pair.Key, pair.Value));
        var orderedAdjacency = adjacency
            .Select(neighbors => (IReadOnlyList<int>)neighbors.Order().ToArray())
            .ToArray();
        return new MeshTopology(orderedAdjacency, orderedEdges);
    }
}
