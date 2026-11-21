using MeshRepair.Core.Binary;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;

namespace MeshRepair.Core.Sample;

public sealed record HoleVertexPositions(
    Point3 BottomLeft,
    Point3 BottomRight,
    Point3 TopRight,
    Point3 TopLeft);

public sealed record MeshSample(
    MeshData Mesh,
    int Seed,
    int WeldFirstVertexId,
    int WeldSecondVertexId,
    Point3 WeldPosition,
    HoleVertexPositions HoleVertices,
    int FragmentVertexA,
    int FragmentVertexB,
    int FragmentVertexC,
    string NonManifoldEdge,
    int DegenerateFaceIndex,
    int HoleQuadColumn,
    int HoleQuadRow);

public static class MeshSampleGenerator
{
    public const int GridColumns = 6;
    public const int GridRows = 4;
    public const int HoleColumn = 2;
    public const int HoleRow = 1;

    public static MeshSample Generate(int seed = 1337)
    {
        var vertices = new List<Point3>();
        var indexByGrid = new Dictionary<(int X, int Y), int>();
        for (var y = 0; y < GridRows; y++)
        {
            for (var x = 0; x < GridColumns; x++)
            {
                var z = ((x * 17 + y * 31 + seed) % 7) == 0 ? 0.0 : 0.0;
                indexByGrid[(x, y)] = vertices.Count;
                vertices.Add(new Point3(x, y, z));
            }
        }

        int Grid(int x, int y) => indexByGrid[(x, y)];

        var faces = new List<Triangle>();
        for (var y = 0; y < GridRows - 1; y++)
        {
            for (var x = 0; x < GridColumns - 1; x++)
            {
                if (x == HoleColumn && y == HoleRow)
                {
                    continue;
                }

                var v00 = Grid(x, y);
                var v10 = Grid(x + 1, y);
                var v11 = Grid(x + 1, y + 1);
                var v01 = Grid(x, y + 1);
                faces.Add(new Triangle(v00, v10, v11));
                faces.Add(new Triangle(v00, v11, v01));
            }
        }

        var weldFirst = Grid(2, 2);
        var weldPosition = vertices[weldFirst];
        var weldSecond = vertices.Count;
        vertices.Add(weldPosition);
        var duplicateTriangles = faces
            .Select((face, index) => (face, index))
            .Where(entry =>
            {
                var a = vertices[entry.face.A];
                var b = vertices[entry.face.B];
                var c = vertices[entry.face.C];
                var centroidX = (a.X + b.X + c.X) / 3.0;
                return entry.face.A == weldFirst
                    && entry.face.B == Grid(3, 2)
                    && entry.face.C == Grid(3, 3)
                    && centroidX > 2.5;
            })
            .Select(entry => entry.index)
            .ToArray();
        foreach (var index in duplicateTriangles)
        {
            var face = faces[index];
            faces[index] = face with { A = weldSecond };
        }

        var fragmentA = vertices.Count;
        vertices.Add(new Point3(0.25, -1.0, 0.0));
        var fragmentB = vertices.Count;
        vertices.Add(new Point3(0.75, -1.0, 0.0));
        var fragmentC = vertices.Count;
        vertices.Add(new Point3(0.5, -0.5, 0.0));
        faces.Add(new Triangle(fragmentA, fragmentB, fragmentC));

        var nonManifoldVertex = vertices.Count;
        vertices.Add(new Point3(1.25, -0.5, 0.0));
        faces.Add(new Triangle(Grid(1, 0), Grid(1, 1), nonManifoldVertex));

        var degenerateFaceIndex = faces.Count;
        faces.Add(new Triangle(Grid(0, 0), Grid(0, 0), Grid(0, 1)));

        var contentHash = MeshBinaryFormat.ComputeContentHash(
            MeshCoordinateType.Float32,
            MeshIndexWidth.UInt16,
            vertices,
            faces);
        var mesh = new MeshData(
            1,
            MeshCoordinateType.Float32,
            MeshIndexWidth.UInt16,
            vertices,
            faces,
            contentHash,
            string.Empty);
        var holeVertices = new HoleVertexPositions(
            new Point3(HoleColumn, HoleRow, 0),
            new Point3(HoleColumn + 1, HoleRow, 0),
            new Point3(HoleColumn + 1, HoleRow + 1, 0),
            new Point3(HoleColumn, HoleRow + 1, 0));
        return new MeshSample(
            mesh,
            seed,
            weldFirst,
            weldSecond,
            weldPosition,
            holeVertices,
            fragmentA,
            fragmentB,
            fragmentC,
            $"E:{Math.Min(Grid(1, 0), Grid(1, 1))}-{Math.Max(Grid(1, 0), Grid(1, 1))}",
            degenerateFaceIndex,
            HoleColumn,
            HoleRow);
    }
}
