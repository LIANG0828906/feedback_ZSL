using System.Numerics;

namespace GameEditor.Spatial;

public readonly record struct WorldRay(Vector3 Origin, Vector3 Direction);

public readonly record struct ScreenRect(float X, float Y, float Width, float Height)
{
    public float MaxX => X + Width;
    public float MaxY => Y + Height;

    public bool Intersects(float minX, float minY, float maxX, float maxY) =>
        X <= maxX && MaxX >= minX && Y <= maxY && MaxY >= minY;
}

public readonly record struct ProjectedBounds(float MinX, float MinY, float MaxX, float MaxY)
{
    public bool IsValid => MinX <= MaxX && MinY <= MaxY;
    public bool Intersects(ScreenRect rect) => rect.Intersects(MinX, MinY, MaxX, MaxY);
}

public sealed class CameraModel
{
    private static readonly int[] EdgeMasks = [1, 2, 4];

    public CameraModel(CameraDefinition definition)
    {
        Position = definition.PositionVector;
        Target = definition.TargetVector;
        Up = definition.UpVector;
        FieldOfViewYDegrees = definition.FieldOfViewYDegrees;
        NearPlane = definition.NearPlane;
        FarPlane = definition.FarPlane;
        Width = definition.ViewportWidth;
        Height = definition.ViewportHeight;

        Forward = SpatialMath.SafeNormalize(Target - Position, new Vector3(0, 0, 1));
        Right = SpatialMath.SafeNormalize(Vector3.Cross(Forward, Up), new Vector3(1, 0, 0));
        CameraUp = SpatialMath.SafeNormalize(Vector3.Cross(Forward, Right), new Vector3(0, 1, 0));
        TanHalfFov = MathF.Tan(FieldOfViewYDegrees * MathF.PI / 360f);
        Aspect = Width / (float)Height;
    }

    public Vector3 Position { get; }
    public Vector3 Target { get; }
    public Vector3 Up { get; }
    public Vector3 Forward { get; }
    public Vector3 Right { get; }
    public Vector3 CameraUp { get; }
    public float FieldOfViewYDegrees { get; }
    public float NearPlane { get; }
    public float FarPlane { get; }
    public int Width { get; }
    public int Height { get; }
    public float TanHalfFov { get; }
    public float Aspect { get; }

    public WorldRay ScreenToWorldRay(float screenX, float screenY)
    {
        var ndcX = (2f * (screenX + 0.5f) / Width) - 1f;
        var ndcY = 1f - (2f * (screenY + 0.5f) / Height);
        var direction = Forward +
                        Right * (ndcX * TanHalfFov * Aspect) +
                        CameraUp * (ndcY * TanHalfFov);
        return new WorldRay(Position, SpatialMath.SafeNormalize(direction, Forward));
    }

    public bool TryProjectPoint(Vector3 world, out Vector2 screen)
    {
        var relative = world - Position;
        var depth = Vector3.Dot(relative, Forward);
        if (depth <= NearPlane || depth > FarPlane)
        {
            screen = default;
            return false;
        }

        var ndcX = Vector3.Dot(relative, Right) / (depth * TanHalfFov * Aspect);
        var ndcY = Vector3.Dot(relative, CameraUp) / (depth * TanHalfFov);
        screen = new Vector2(
            ((ndcX + 1f) * 0.5f * Width) - 0.5f,
            ((1f - ndcY) * 0.5f * Height) - 0.5f);
        return true;
    }

    public ProjectedBounds ProjectBounds(Bounds3 bounds)
    {
        if (bounds.IsEmpty)
        {
            return new ProjectedBounds(0, 0, -1, -1);
        }

        var corners = GetCorners(bounds);
        var depths = new float[8];
        var minX = float.PositiveInfinity;
        var minY = float.PositiveInfinity;
        var maxX = float.NegativeInfinity;
        var maxY = float.NegativeInfinity;
        var hasPoint = false;

        for (var i = 0; i < corners.Length; i++)
        {
            depths[i] = Vector3.Dot(corners[i] - Position, Forward);
            if (depths[i] >= NearPlane &&
                TryProjectPoint(corners[i], out var projected))
            {
                hasPoint = true;
                Encapsulate(projected, ref minX, ref minY, ref maxX, ref maxY);
            }
        }

        for (var mask = 0; mask < 8; mask++)
        {
            foreach (var bit in EdgeMasks)
            {
                if ((mask & bit) != 0)
                {
                    continue;
                }

                var other = mask | bit;
                var z0 = depths[mask];
                var z1 = depths[other];
                if ((z0 < NearPlane && z1 >= NearPlane) ||
                    (z1 < NearPlane && z0 >= NearPlane))
                {
                    var denominator = z1 - z0;
                    if (MathF.Abs(denominator) > SpatialMath.Epsilon)
                    {
                        var t = (NearPlane - z0) / denominator;
                        var point = Vector3.Lerp(corners[mask], corners[other], t);
                        if (TryProjectPoint(point, out var projected))
                        {
                            hasPoint = true;
                            Encapsulate(projected, ref minX, ref minY, ref maxX, ref maxY);
                        }
                    }
                }
            }
        }

        return hasPoint
            ? new ProjectedBounds(minX, minY, maxX, maxY)
            : new ProjectedBounds(0, 0, -1, -1);
    }

    public bool FrustumIntersects(Bounds3 bounds)
    {
        if (bounds.IsEmpty)
        {
            return false;
        }

        foreach (var plane in GetFrustumPlanes())
        {
            var positive = new Vector3(
                plane.Normal.X >= 0 ? bounds.Max.X : bounds.Min.X,
                plane.Normal.Y >= 0 ? bounds.Max.Y : bounds.Min.Y,
                plane.Normal.Z >= 0 ? bounds.Max.Z : bounds.Min.Z);
            if (Vector3.Dot(plane.Normal, positive) + plane.D < 0)
            {
                return false;
            }
        }

        return true;
    }

    private IReadOnlyList<Plane> GetFrustumPlanes()
    {
        var tanY = TanHalfFov;
        var tanX = TanHalfFov * Aspect;
        var near = Position + Forward * NearPlane;
        var far = Position + Forward * FarPlane;
        return
        [
            Plane.Normalize(new Plane(Forward, -Vector3.Dot(Forward, near))),
            Plane.Normalize(new Plane(-Forward, Vector3.Dot(Forward, far))),
            Plane.Normalize(new Plane(Forward * tanX + Right, -Vector3.Dot(Forward * tanX + Right, Position))),
            Plane.Normalize(new Plane(Forward * tanX - Right, -Vector3.Dot(Forward * tanX - Right, Position))),
            Plane.Normalize(new Plane(Forward * tanY + CameraUp, -Vector3.Dot(Forward * tanY + CameraUp, Position))),
            Plane.Normalize(new Plane(Forward * tanY - CameraUp, -Vector3.Dot(Forward * tanY - CameraUp, Position)))
        ];
    }

    private static Vector3[] GetCorners(Bounds3 bounds) =>
    [
        new(bounds.Min.X, bounds.Min.Y, bounds.Min.Z),
        new(bounds.Max.X, bounds.Min.Y, bounds.Min.Z),
        new(bounds.Min.X, bounds.Max.Y, bounds.Min.Z),
        new(bounds.Max.X, bounds.Max.Y, bounds.Min.Z),
        new(bounds.Min.X, bounds.Min.Y, bounds.Max.Z),
        new(bounds.Max.X, bounds.Min.Y, bounds.Max.Z),
        new(bounds.Min.X, bounds.Max.Y, bounds.Max.Z),
        new(bounds.Max.X, bounds.Max.Y, bounds.Max.Z)
    ];

    private static void Encapsulate(Vector2 point, ref float minX, ref float minY, ref float maxX, ref float maxY)
    {
        minX = MathF.Min(minX, point.X);
        minY = MathF.Min(minY, point.Y);
        maxX = MathF.Max(maxX, point.X);
        maxY = MathF.Max(maxY, point.Y);
    }
}
