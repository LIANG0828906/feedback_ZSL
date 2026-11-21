using MeshRepair.Core.Geometry;

namespace MeshRepair.Core.Model;

public enum MeshCoordinateType : byte
{
    Float32 = 1,
    Float64 = 2
}

public enum MeshIndexWidth : byte
{
    UInt16 = 2,
    UInt32 = 4
}

public sealed class MeshData
{
    public MeshData(
        ulong version,
        MeshCoordinateType coordinateType,
        MeshIndexWidth indexWidth,
        IReadOnlyList<Point3> vertices,
        IReadOnlyList<Triangle> faces,
        string contentHash,
        string fileHash)
    {
        Version = version;
        CoordinateType = coordinateType;
        IndexWidth = indexWidth;
        Vertices = vertices;
        Faces = faces;
        ContentHash = contentHash;
        FileHash = fileHash;
    }

    public ulong Version { get; }

    public MeshCoordinateType CoordinateType { get; }

    public MeshIndexWidth IndexWidth { get; }

    public IReadOnlyList<Point3> Vertices { get; }

    public IReadOnlyList<Triangle> Faces { get; }

    public string ContentHash { get; }

    public string FileHash { get; }
}
