using System.Globalization;

namespace MeshRepair.Core.Geometry;

public readonly record struct Point3(double X, double Y, double Z)
{
    public static readonly Point3 Origin = new(0, 0, 0);

    public bool IsFinite => double.IsFinite(X) && double.IsFinite(Y) && double.IsFinite(Z);

    public Point3 Add(Point3 other) => new(X + other.X, Y + other.Y, Z + other.Z);

    public Point3 Subtract(Point3 other) => new(X - other.X, Y - other.Y, Z - other.Z);

    public Point3 Scale(double factor) => new(X * factor, Y * factor, Z * factor);

    public double Dot(Point3 other) => (X * other.X) + (Y * other.Y) + (Z * other.Z);

    public Point3 Cross(Point3 other) => new(
        (Y * other.Z) - (Z * other.Y),
        (Z * other.X) - (X * other.Z),
        (X * other.Y) - (Y * other.X));

    public double LengthSquared => Dot(this);

    public double Length => Math.Sqrt(LengthSquared);

    public Point3 Normalized()
    {
        var length = Length;
        return length <= double.Epsilon ? Origin : Scale(1.0 / length);
    }

    public double DistanceTo(Point3 other) => Subtract(other).Length;

    public string CanonicalString() =>
        string.Create(CultureInfo.InvariantCulture, $"{X:R},{Y:R},{Z:R}");
}

public readonly record struct Bounds3(Point3 Min, Point3 Max)
{
    public static Bounds3 Empty => new(
        new Point3(double.PositiveInfinity, double.PositiveInfinity, double.PositiveInfinity),
        new Point3(double.NegativeInfinity, double.NegativeInfinity, double.NegativeInfinity));

    public bool IsEmpty => Min.X > Max.X || Min.Y > Max.Y || Min.Z > Max.Z;

    public Bounds3 Include(Point3 point) => new(
        new Point3(Math.Min(Min.X, point.X), Math.Min(Min.Y, point.Y), Math.Min(Min.Z, point.Z)),
        new Point3(Math.Max(Max.X, point.X), Math.Max(Max.Y, point.Y), Math.Max(Max.Z, point.Z)));

    public static Bounds3 FromPoints(IEnumerable<Point3> points)
    {
        var bounds = Empty;
        foreach (var point in points)
        {
            bounds = bounds.Include(point);
        }

        return bounds;
    }
}
