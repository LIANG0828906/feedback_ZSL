using MeshRepair.Core.Chunks;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Preview;

public readonly record struct PreviewVertex(Point3 Position, uint PackedColor);

public readonly record struct PreviewLine(Point3 A, Point3 B, uint PackedColor);

public sealed class PreviewGeometry
{
    public PreviewGeometry(
        long version,
        string contentHash,
        IReadOnlyList<PreviewVertex> triangles,
        IReadOnlyList<PreviewLine> boundaryLines,
        IReadOnlyList<ChunkId> chunks)
    {
        Version = version;
        ContentHash = contentHash;
        Triangles = triangles;
        BoundaryLines = boundaryLines;
        Chunks = chunks;
    }

    public long Version { get; }

    public string ContentHash { get; }

    public IReadOnlyList<PreviewVertex> Triangles { get; }

    public IReadOnlyList<PreviewLine> BoundaryLines { get; }

    public IReadOnlyList<ChunkId> Chunks { get; }

    public Bounds3 Bounds => Bounds3.FromPoints(Triangles.Select(vertex => vertex.Position));
}

public static class PreviewGeometryBuilder
{
    private static readonly uint[] Palette =
    {
        0x5B8FF9,
        0x61DDAA,
        0xF6BD16,
        0x7262FD,
        0x78D3F8,
        0xF6903D,
        0x9661BC,
        0xF08BB2
    };

    public static PreviewGeometry Build(MeshVersion version)
    {
        var positions = new Dictionary<int, Point3>();
        foreach (var chunk in version.Chunks.Values.OrderBy(chunk => chunk.Id))
        {
            foreach (var vertex in chunk.Vertices)
            {
                positions[vertex.Key] = vertex.Value;
            }
        }

        var colors = version.Chunks.Keys
            .Order()
            .Select((chunk, index) => (chunk, color: Palette[index % Palette.Length]))
            .ToDictionary(entry => entry.chunk, entry => entry.color);
        var triangles = new List<PreviewVertex>();
        foreach (var chunk in version.Chunks.Values.OrderBy(chunk => chunk.Id))
        {
            var color = colors[chunk.Id];
            foreach (var face in chunk.Faces.OrderBy(face => face.FaceId))
            {
                triangles.Add(new PreviewVertex(positions[face.Triangle.A], color));
                triangles.Add(new PreviewVertex(positions[face.Triangle.B], color));
                triangles.Add(new PreviewVertex(positions[face.Triangle.C], color));
            }
        }

        var lines = version.Boundaries.Values
            .OrderBy(manifest => manifest.Pair)
            .SelectMany(manifest => manifest.Edges
                .OrderBy(edge => edge.A)
                .ThenBy(edge => edge.B)
                .Select(edge => new PreviewLine(
                    positions[edge.A],
                    positions[edge.B],
                    0xFFE84D)))
            .ToArray();
        return new PreviewGeometry(version.Version, version.ContentHash, triangles, lines, colors.Keys.Order().ToArray());
    }
}
