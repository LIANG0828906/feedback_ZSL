using MeshRepair.Core.Geometry;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Core.Model;

public sealed class BaselineInfo
{
    public BaselineInfo(
        int vertexCount,
        int faceCount,
        string contentHash,
        IReadOnlyDictionary<ulong, Point3> faceNormals)
    {
        VertexCount = vertexCount;
        FaceCount = faceCount;
        ContentHash = contentHash;
        FaceNormals = faceNormals;
    }

    public int VertexCount { get; }

    public int FaceCount { get; }

    public string ContentHash { get; }

    public IReadOnlyDictionary<ulong, Point3> FaceNormals { get; }

    public static BaselineInfo FromMesh(MeshData mesh)
    {
        var normals = new Dictionary<ulong, Point3>();
        for (var faceIndex = 0; faceIndex < mesh.Faces.Count; faceIndex++)
        {
            normals[(ulong)(faceIndex + 1)] = MeshGeometry.FaceNormal(mesh.Faces[faceIndex], mesh.Vertices);
        }

        return new BaselineInfo(
            mesh.Vertices.Count,
            mesh.Faces.Count,
            mesh.ContentHash,
            normals);
    }
}

public sealed record NormalDeviationStats(
    int ComparedFaces,
    double MeanDegrees,
    double MaxDegrees,
    double P95Degrees);

public sealed record SpliceReport(
    long Version,
    int BaselineVertexCount,
    int CurrentVertexCount,
    int BaselineFaceCount,
    int CurrentFaceCount,
    int WeldedVertexReduction,
    int FilledFaceCount,
    int BoundaryEdgeCount,
    int NonManifoldEdgeCount,
    int OpenEdgeCount,
    NormalDeviationStats NormalDeviation,
    string VersionContentHash);

public static class MeshGeometry
{
    public static Point3 FaceNormal(Triangle face, IReadOnlyList<Point3> positions)
    {
        var a = positions[face.A];
        var b = positions[face.B];
        var c = positions[face.C];
        return b.Subtract(a).Cross(c.Subtract(a)).Normalized();
    }

    public static Point3 FaceNormal(Triangle face, IReadOnlyDictionary<int, Point3> positions)
    {
        var a = positions[face.A];
        var b = positions[face.B];
        var c = positions[face.C];
        return b.Subtract(a).Cross(c.Subtract(a)).Normalized();
    }

    public static NormalDeviationStats ComputeNormalDeviation(
        MeshVersion version,
        BaselineInfo baseline)
    {
        var positions = version.Chunks.Values
            .SelectMany(chunk => chunk.Vertices)
            .GroupBy(pair => pair.Key)
            .ToDictionary(group => group.Key, group => group.First().Value);
        var currentById = version.Chunks.Values
            .SelectMany(chunk => chunk.Faces)
            .ToDictionary(face => face.FaceId, face => face.Triangle);
        var angles = new List<double>();
        foreach (var entry in baseline.FaceNormals.OrderBy(pair => pair.Key))
        {
            if (!currentById.TryGetValue(entry.Key, out var face))
            {
                continue;
            }

            var current = FaceNormal(face, positions);
            if (current.LengthSquared <= 0 || entry.Value.LengthSquared <= 0)
            {
                continue;
            }

            var dot = Math.Clamp(current.Dot(entry.Value), -1.0, 1.0);
            angles.Add(Math.Acos(dot) * 180.0 / Math.PI);
        }

        if (angles.Count == 0)
        {
            return new NormalDeviationStats(0, 0, 0, 0);
        }

        var ordered = angles.Order().ToArray();
        var index = Math.Min(ordered.Length - 1, (int)Math.Ceiling(ordered.Length * 0.95) - 1);
        return new NormalDeviationStats(
            ordered.Length,
            ordered.Average(),
            ordered[^1],
            ordered[Math.Max(0, index)]);
    }
}
