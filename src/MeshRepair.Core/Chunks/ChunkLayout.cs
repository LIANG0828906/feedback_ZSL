using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;

namespace MeshRepair.Core.Chunks;

public sealed record FaceAssignment(
    ulong FaceId,
    ChunkId Owner,
    IReadOnlyList<ChunkId> CandidateChunks,
    string Basis);

public sealed class ChunkLayout
{
    private readonly double[] _xBounds;
    private readonly double[] _yBounds;
    private readonly double[] _zBounds;

    public ChunkLayout(
        int divisionsX,
        int divisionsY,
        int divisionsZ,
        Bounds3 bounds,
        double interfaceTolerance = 1e-7)
    {
        if (divisionsX < 1 || divisionsY < 1 || divisionsZ < 1)
        {
            throw new ArgumentOutOfRangeException(nameof(divisionsX), "chunk divisions must be positive");
        }

        if (bounds.IsEmpty)
        {
            bounds = new Bounds3(Point3.Origin, Point3.Origin);
        }

        DivisionsX = divisionsX;
        DivisionsY = divisionsY;
        DivisionsZ = divisionsZ;
        Bounds = bounds;
        InterfaceTolerance = interfaceTolerance;
        _xBounds = MakeBounds(bounds.Min.X, bounds.Max.X, divisionsX);
        _yBounds = MakeBounds(bounds.Min.Y, bounds.Max.Y, divisionsY);
        _zBounds = MakeBounds(bounds.Min.Z, bounds.Max.Z, divisionsZ);
    }

    public int DivisionsX { get; }

    public int DivisionsY { get; }

    public int DivisionsZ { get; }

    public Bounds3 Bounds { get; }

    public double InterfaceTolerance { get; }

    public static ChunkLayout FromMesh(
        MeshData mesh,
        int divisionsX = 2,
        int divisionsY = 1,
        int divisionsZ = 1)
    {
        var bounds = Bounds3.FromPoints(mesh.Vertices);
        return new ChunkLayout(divisionsX, divisionsY, divisionsZ, bounds);
    }

    public ChunkId CellForPoint(Point3 point)
        => new(
            FindCell(point.X, _xBounds, DivisionsX),
            FindCell(point.Y, _yBounds, DivisionsY),
            FindCell(point.Z, _zBounds, DivisionsZ));

    public FaceAssignment AssignFace(
        ulong faceId,
        Triangle face,
        IReadOnlyDictionary<int, Point3> vertices)
    {
        var a = vertices[face.A];
        var b = vertices[face.B];
        var c = vertices[face.C];
        var centroid = new Point3(
            (a.X + b.X + c.X) / 3.0,
            (a.Y + b.Y + c.Y) / 3.0,
            (a.Z + b.Z + c.Z) / 3.0);
        var owner = CellForPoint(centroid);

        var min = new Point3(
            Math.Min(a.X, Math.Min(b.X, c.X)),
            Math.Min(a.Y, Math.Min(b.Y, c.Y)),
            Math.Min(a.Z, Math.Min(b.Z, c.Z)));
        var max = new Point3(
            Math.Max(a.X, Math.Max(b.X, c.X)),
            Math.Max(a.Y, Math.Max(b.Y, c.Y)),
            Math.Max(a.Z, Math.Max(b.Z, c.Z)));

        var candidates = new HashSet<ChunkId>();
        foreach (var x in CellRange(min.X, max.X, _xBounds, DivisionsX))
        {
            foreach (var y in CellRange(min.Y, max.Y, _yBounds, DivisionsY))
            {
                foreach (var z in CellRange(min.Z, max.Z, _zBounds, DivisionsZ))
                {
                    candidates.Add(new ChunkId(x, y, z));
                }
            }
        }

        candidates.Add(owner);
        var ordered = candidates.Order().ToArray();
        var basis = ordered.Length > 1
            ? $"centroid={centroid.CanonicalString()}; crossed={string.Join(";", ordered.Select(chunk => chunk.ToString()))}"
            : $"centroid={centroid.CanonicalString()}";
        return new FaceAssignment(faceId, owner, ordered, basis);
    }

    public bool AreNeighbors(ChunkId first, ChunkId second)
    {
        var manhattan = Math.Abs(first.X - second.X)
            + Math.Abs(first.Y - second.Y)
            + Math.Abs(first.Z - second.Z);
        return manhattan == 1;
    }

    public bool TryGetInterfacePlane(ChunkPair pair, out int axis, out double coordinate)
    {
        axis = -1;
        coordinate = 0;
        if (pair.A.Y == pair.B.Y && pair.A.Z == pair.B.Z && Math.Abs(pair.A.X - pair.B.X) == 1)
        {
            axis = 0;
            coordinate = InterfaceValue(_xBounds, Math.Min(pair.A.X, pair.B.X));
            return true;
        }

        if (pair.A.X == pair.B.X && pair.A.Z == pair.B.Z && Math.Abs(pair.A.Y - pair.B.Y) == 1)
        {
            axis = 1;
            coordinate = InterfaceValue(_yBounds, Math.Min(pair.A.Y, pair.B.Y));
            return true;
        }

        if (pair.A.X == pair.B.X && pair.A.Y == pair.B.Y && Math.Abs(pair.A.Z - pair.B.Z) == 1)
        {
            axis = 2;
            coordinate = InterfaceValue(_zBounds, Math.Min(pair.A.Z, pair.B.Z));
            return true;
        }

        return false;
    }

    public bool IsOnInterface(Point3 point, ChunkPair pair)
    {
        if (!TryGetInterfacePlane(pair, out var axis, out var coordinate))
        {
            return false;
        }

        var value = axis switch
        {
            0 => point.X,
            1 => point.Y,
            _ => point.Z
        };
        return Math.Abs(value - coordinate) <= InterfaceTolerance;
    }

    private static double[] MakeBounds(double min, double max, int divisions)
    {
        if (max <= min)
        {
            max = min + 1e-9;
        }

        var bounds = new double[divisions + 1];
        for (var index = 0; index <= divisions; index++)
        {
            bounds[index] = min + ((max - min) * index / divisions);
        }

        return bounds;
    }

    private static int FindCell(double value, IReadOnlyList<double> bounds, int divisions)
    {
        if (value <= bounds[0])
        {
            return 0;
        }

        if (value >= bounds[divisions])
        {
            return divisions - 1;
        }

        var index = Array.BinarySearch(bounds.ToArray(), value);
        if (index >= 0)
        {
            return Math.Min(index, divisions - 1);
        }

        var insertion = ~index - 1;
        return Math.Clamp(insertion, 0, divisions - 1);
    }

    private static IEnumerable<int> CellRange(
        double min,
        double max,
        IReadOnlyList<double> bounds,
        int divisions)
    {
        var first = FindCell(min, bounds, divisions);
        var last = FindCell(max, bounds, divisions);
        for (var index = first; index <= last; index++)
        {
            yield return index;
        }
    }

    private static double InterfaceValue(IReadOnlyList<double> bounds, int lowerCell)
        => bounds[lowerCell + 1];
}
