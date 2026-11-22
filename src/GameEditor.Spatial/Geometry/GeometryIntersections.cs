using System.Numerics;

namespace GameEditor.Spatial;

public readonly record struct GeometryHit(
    string GeometryId,
    string NodeId,
    float Distance,
    Vector3 Point);

public static class GeometryIntersections
{
    public static bool TryIntersect(
        GeometryDefinition geometry,
        Matrix4x4 world,
        WorldRay ray,
        out GeometryHit hit)
    {
        hit = default;
        if (!SpatialMath.TryInvert(world, out var inverse))
        {
            return false;
        }

        var localOrigin = Vector3.Transform(ray.Origin, inverse);
        var localDirection = Vector3.TransformNormal(ray.Direction, inverse);
        if (!SpatialMath.IsFinite(localOrigin) ||
            !SpatialMath.IsFinite(localDirection) ||
            localDirection.LengthSquared() <= SpatialMath.Epsilon * SpatialMath.Epsilon)
        {
            return false;
        }

        float distance;
        switch (geometry.GeometryKind)
        {
            case GeometryKind.Aabb:
                if (!IntersectAabb(localOrigin, localDirection, geometry.LocalBounds, out distance))
                {
                    return false;
                }

                break;
            case GeometryKind.Sphere:
                if (!IntersectSphere(
                        localOrigin,
                        localDirection,
                        SpatialMath.VectorFromArray(geometry.Center!),
                        geometry.Radius!.Value,
                        out distance))
                {
                    return false;
                }

                break;
            case GeometryKind.TriangleMesh:
                if (!IntersectMesh(
                        localOrigin,
                        localDirection,
                        geometry.Vertices!,
                        geometry.Indices!,
                        out distance))
                {
                    return false;
                }

                break;
            default:
                return false;
        }

        if (!float.IsFinite(distance) || distance < 0)
        {
            return false;
        }

        var point = ray.Origin + ray.Direction * distance;
        hit = new GeometryHit(geometry.Id, geometry.NodeId, distance, point);
        return true;
    }

    public static bool IntersectAabb(
        Vector3 origin,
        Vector3 direction,
        Bounds3 bounds,
        out float distance)
    {
        distance = 0;
        var tMin = 0f;
        var tMax = float.PositiveInfinity;

        if (!ClipAxis(origin.X, direction.X, bounds.Min.X, bounds.Max.X, ref tMin, ref tMax) ||
            !ClipAxis(origin.Y, direction.Y, bounds.Min.Y, bounds.Max.Y, ref tMin, ref tMax) ||
            !ClipAxis(origin.Z, direction.Z, bounds.Min.Z, bounds.Max.Z, ref tMin, ref tMax))
        {
            return false;
        }

        if (tMax < 0 || tMin > tMax)
        {
            return false;
        }

        distance = tMin >= 0 ? tMin : tMax;
        return distance >= 0;
    }

    public static bool IntersectSphere(
        Vector3 origin,
        Vector3 direction,
        Vector3 center,
        float radius,
        out float distance)
    {
        distance = 0;
        var offset = origin - center;
        var a = Vector3.Dot(direction, direction);
        if (a <= SpatialMath.Epsilon * SpatialMath.Epsilon)
        {
            return false;
        }

        var b = 2f * Vector3.Dot(offset, direction);
        var c = Vector3.Dot(offset, offset) - radius * radius;
        var discriminant = b * b - 4f * a * c;
        if (discriminant < 0)
        {
            return false;
        }

        var root = MathF.Sqrt(discriminant);
        var t0 = (-b - root) / (2f * a);
        var t1 = (-b + root) / (2f * a);
        if (t0 >= 0)
        {
            distance = t0;
            return true;
        }

        if (t1 >= 0)
        {
            distance = t1;
            return true;
        }

        return false;
    }

    public static bool IntersectMesh(
        Vector3 origin,
        Vector3 direction,
        IReadOnlyList<float[]> vertices,
        IReadOnlyList<int> indices,
        out float distance)
    {
        distance = float.PositiveInfinity;
        for (var i = 0; i + 2 < indices.Count; i += 3)
        {
            var a = SpatialMath.VectorFromArray(vertices[indices[i]]);
            var b = SpatialMath.VectorFromArray(vertices[indices[i + 1]]);
            var c = SpatialMath.VectorFromArray(vertices[indices[i + 2]]);
            if (IntersectTriangle(origin, direction, a, b, c, out var t) && t < distance)
            {
                distance = t;
            }
        }

        return float.IsFinite(distance);
    }

    public static bool IntersectTriangle(
        Vector3 origin,
        Vector3 direction,
        Vector3 a,
        Vector3 b,
        Vector3 c,
        out float distance)
    {
        const float epsilon = 1e-7f;
        distance = 0;
        var edge1 = b - a;
        var edge2 = c - a;
        var p = Vector3.Cross(direction, edge2);
        var determinant = Vector3.Dot(edge1, p);
        if (MathF.Abs(determinant) < epsilon)
        {
            return false;
        }

        var inverseDeterminant = 1f / determinant;
        var t = origin - a;
        var u = Vector3.Dot(t, p) * inverseDeterminant;
        if (u < 0 || u > 1)
        {
            return false;
        }

        var q = Vector3.Cross(t, edge1);
        var v = Vector3.Dot(direction, q) * inverseDeterminant;
        if (v < 0 || u + v > 1)
        {
            return false;
        }

        var hitDistance = Vector3.Dot(edge2, q) * inverseDeterminant;
        if (hitDistance < 0)
        {
            return false;
        }

        distance = hitDistance;
        return true;
    }

    private static bool ClipAxis(
        float origin,
        float direction,
        float min,
        float max,
        ref float tMin,
        ref float tMax)
    {
        if (MathF.Abs(direction) <= SpatialMath.Epsilon)
        {
            return origin >= min && origin <= max;
        }

        var inverse = 1f / direction;
        var t0 = (min - origin) * inverse;
        var t1 = (max - origin) * inverse;
        if (t0 > t1)
        {
            (t0, t1) = (t1, t0);
        }

        tMin = MathF.Max(tMin, t0);
        tMax = MathF.Min(tMax, t1);
        return tMin <= tMax;
    }
}
