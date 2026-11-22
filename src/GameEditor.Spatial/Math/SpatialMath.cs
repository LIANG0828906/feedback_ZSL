using System.Globalization;
using System.Text;
using System.Numerics;

namespace GameEditor.Spatial;

public readonly struct Bounds3 : IEquatable<Bounds3>
{
    public Bounds3(Vector3 min, Vector3 max)
    {
        Min = min;
        Max = max;
    }

    public Vector3 Min { get; }
    public Vector3 Max { get; }
    public Vector3 Center => (Min + Max) * 0.5f;
    public Vector3 Size => Max - Min;
    public bool IsEmpty => Min.X > Max.X || Min.Y > Max.Y || Min.Z > Max.Z;
    public bool IsFinite => SpatialMath.IsFinite(Min) && SpatialMath.IsFinite(Max);

    public static Bounds3 Empty { get; } =
        new(new Vector3(float.PositiveInfinity), new Vector3(float.NegativeInfinity));

    public Bounds3 Encapsulate(Bounds3 other)
    {
        if (other.IsEmpty)
        {
            return this;
        }

        if (IsEmpty)
        {
            return other;
        }

        return new Bounds3(Vector3.Min(Min, other.Min), Vector3.Max(Max, other.Max));
    }

    public Bounds3 Encapsulate(Vector3 point) =>
        new(Vector3.Min(Min, point), Vector3.Max(Max, point));

    public bool Intersects(Bounds3 other) =>
        !IsEmpty && !other.IsEmpty &&
        Min.X <= other.Max.X && Max.X >= other.Min.X &&
        Min.Y <= other.Max.Y && Max.Y >= other.Min.Y &&
        Min.Z <= other.Max.Z && Max.Z >= other.Min.Z;

    public Bounds3 Transform(Matrix4x4 matrix)
    {
        if (IsEmpty)
        {
            return Empty;
        }

        var result = Empty;
        for (var mask = 0; mask < 8; mask++)
        {
            var corner = new Vector3(
                (mask & 1) == 0 ? Min.X : Max.X,
                (mask & 2) == 0 ? Min.Y : Max.Y,
                (mask & 4) == 0 ? Min.Z : Max.Z);
            result = result.Encapsulate(Vector3.Transform(corner, matrix));
        }

        return result;
    }

    public bool Equals(Bounds3 other) => Min.Equals(other.Min) && Max.Equals(other.Max);
    public override bool Equals(object? obj) => obj is Bounds3 other && Equals(other);
    public override int GetHashCode() => HashCode.Combine(Min, Max);

    public float[] ToMinArray() => [Min.X, Min.Y, Min.Z];
    public float[] ToMaxArray() => [Max.X, Max.Y, Max.Z];
}

public static class SpatialMath
{
    public const float Epsilon = 1e-6f;

    public static bool IsFinite(float value) => float.IsFinite(value);

    public static bool IsFinite(Vector3 value) =>
        float.IsFinite(value.X) && float.IsFinite(value.Y) && float.IsFinite(value.Z);

    public static bool IsFinite(Matrix4x4 matrix) =>
        IsFinite(matrix.M11) && IsFinite(matrix.M12) && IsFinite(matrix.M13) && IsFinite(matrix.M14) &&
        IsFinite(matrix.M21) && IsFinite(matrix.M22) && IsFinite(matrix.M23) && IsFinite(matrix.M24) &&
        IsFinite(matrix.M31) && IsFinite(matrix.M32) && IsFinite(matrix.M33) && IsFinite(matrix.M34) &&
        IsFinite(matrix.M41) && IsFinite(matrix.M42) && IsFinite(matrix.M43) && IsFinite(matrix.M44);

    public static Matrix4x4 MatrixFromRowMajor(IReadOnlyList<float> values)
    {
        if (values.Count != 16)
        {
            throw new ArgumentException("A matrix must have 16 values.", nameof(values));
        }

        return new Matrix4x4(
            values[0], values[1], values[2], values[3],
            values[4], values[5], values[6], values[7],
            values[8], values[9], values[10], values[11],
            values[12], values[13], values[14], values[15]);
    }

    public static float[] MatrixToRowMajor(Matrix4x4 matrix) =>
    [
        matrix.M11, matrix.M12, matrix.M13, matrix.M14,
        matrix.M21, matrix.M22, matrix.M23, matrix.M24,
        matrix.M31, matrix.M32, matrix.M33, matrix.M34,
        matrix.M41, matrix.M42, matrix.M43, matrix.M44
    ];

    public static Vector3 VectorFromArray(IReadOnlyList<float> values)
    {
        if (values.Count != 3)
        {
            throw new ArgumentException("A vector must have 3 values.", nameof(values));
        }

        return new Vector3(values[0], values[1], values[2]);
    }

    public static float[] VectorToArray(Vector3 value) => [value.X, value.Y, value.Z];

    public static Matrix4x4 ComposeTRS(Vector3 translation, Quaternion rotation, Vector3 scale) =>
        Matrix4x4.CreateScale(scale) *
        Matrix4x4.CreateFromQuaternion(rotation) *
        Matrix4x4.CreateTranslation(translation);

    public static bool TryInvert(Matrix4x4 matrix, out Matrix4x4 inverse) =>
        Matrix4x4.Invert(matrix, out inverse) && IsFinite(inverse);

    public static Vector3 SafeNormalize(Vector3 value, Vector3 fallback)
    {
        var lengthSquared = value.LengthSquared();
        if (!float.IsFinite(lengthSquared) || lengthSquared <= Epsilon * Epsilon)
        {
            return fallback;
        }

        return value / MathF.Sqrt(lengthSquared);
    }

    public static string F(float value) => value.ToString("R", CultureInfo.InvariantCulture);

    public static string CanonicalFloat(float value) => F(value == 0f ? 0f : value);

    public static void AppendVector(StringBuilder builder, Vector3 value)
    {
        builder.Append(CanonicalFloat(value.X)).Append(',')
            .Append(CanonicalFloat(value.Y)).Append(',')
            .Append(CanonicalFloat(value.Z));
    }

    public static void AppendMatrix(StringBuilder builder, Matrix4x4 matrix)
    {
        var values = MatrixToRowMajor(matrix);
        for (var i = 0; i < values.Length; i++)
        {
            if (i > 0)
            {
                builder.Append(',');
            }

            builder.Append(CanonicalFloat(values[i]));
        }
    }
}
