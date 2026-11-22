using System.Numerics;
using System.Security.Cryptography;
using System.Text;

namespace GameEditor.Spatial;

public sealed record SceneNodeState(string Id, string? ParentId, Matrix4x4 LocalTransform);

public sealed class SceneState
{
    private string? _contentHash;
    private string? _structureHash;

    private SceneState(
        int seed,
        CameraDefinition camera,
        Dictionary<string, SceneNodeState> nodes,
        Dictionary<string, GeometryDefinition> geometries)
    {
        Seed = seed;
        Camera = camera;
        Nodes = nodes;
        Geometries = geometries;
        RebuildChildren();
    }

    public int Seed { get; }
    public CameraDefinition Camera { get; }
    public Dictionary<string, SceneNodeState> Nodes { get; }
    public Dictionary<string, GeometryDefinition> Geometries { get; }
    public Dictionary<string, List<string>> Children { get; private set; } = [];
    public List<string> Roots { get; private set; } = [];

    public string ContentHash => _contentHash ??= SceneHashing.ComputeContentHash(this);
    public string StructureHash => _structureHash ??= SceneHashing.ComputeStructureHash(this);

    public long StructureVersion
    {
        get
        {
            var bytes = SHA256.HashData(Encoding.UTF8.GetBytes(StructureHash));
            return BitConverter.ToInt64(bytes, 0) & 0x7FFFFFFFFFFFFFFF;
        }
    }

    public static SceneState FromDocument(SceneDocument document)
    {
        var nodes = document.Nodes.ToDictionary(
            node => node.Id,
            node => new SceneNodeState(
                node.Id,
                node.ParentId,
                SpatialMath.MatrixFromRowMajor(node.LocalMatrix)),
            StringComparer.Ordinal);
        var geometries = document.Geometries.ToDictionary(
            geometry => geometry.Id,
            CloneGeometry,
            StringComparer.Ordinal);
        return new SceneState(document.Seed, CloneCamera(document.Camera), nodes, geometries);
    }

    public SceneState Clone()
    {
        return new SceneState(
            Seed,
            Camera,
            new Dictionary<string, SceneNodeState>(Nodes, StringComparer.Ordinal),
            new Dictionary<string, GeometryDefinition>(Geometries, StringComparer.Ordinal));
    }

    public SceneState WithNodeTransforms(IReadOnlyDictionary<string, Matrix4x4> transforms)
    {
        var nodes = new Dictionary<string, SceneNodeState>(Nodes, StringComparer.Ordinal);
        foreach (var pair in transforms)
        {
            var node = nodes[pair.Key];
            nodes[pair.Key] = node with { LocalTransform = pair.Value };
        }

        return new SceneState(Seed, Camera, nodes, new Dictionary<string, GeometryDefinition>(Geometries, StringComparer.Ordinal));
    }

    public SceneState WithGeometryAdded(GeometryDefinition geometry)
    {
        var geometries = new Dictionary<string, GeometryDefinition>(Geometries, StringComparer.Ordinal)
        {
            [geometry.Id] = geometry
        };
        return new SceneState(Seed, Camera, new Dictionary<string, SceneNodeState>(Nodes, StringComparer.Ordinal), geometries);
    }

    public SceneState WithGeometryRemoved(string geometryId)
    {
        var geometries = new Dictionary<string, GeometryDefinition>(Geometries, StringComparer.Ordinal);
        geometries.Remove(geometryId);
        return new SceneState(Seed, Camera, new Dictionary<string, SceneNodeState>(Nodes, StringComparer.Ordinal), geometries);
    }

    public IReadOnlyList<string> GetTopologicalOrder()
    {
        var result = new List<string>(Nodes.Count);
        var remaining = new Dictionary<string, int>(StringComparer.Ordinal);
        var ready = new SortedSet<string>(StringComparer.Ordinal);
        foreach (var node in Nodes.Values)
        {
            var childCount = Children.TryGetValue(node.Id, out var children) ? children.Count : 0;
            remaining[node.Id] = childCount;
            if (childCount == 0)
            {
                ready.Add(node.Id);
            }
        }

        var reverse = new List<string>(Nodes.Count);
        while (ready.Count > 0)
        {
            var nodeId = ready.Min!;
            ready.Remove(nodeId);
            reverse.Add(nodeId);
            var parentId = Nodes[nodeId].ParentId;
            if (parentId is null)
            {
                continue;
            }

            remaining[parentId]--;
            if (remaining[parentId] == 0)
            {
                ready.Add(parentId);
            }
        }

        if (reverse.Count != Nodes.Count)
        {
            throw new InvalidOperationException("Scene topology contains a cycle.");
        }

        for (var i = reverse.Count - 1; i >= 0; i--)
        {
            result.Add(reverse[i]);
        }

        return result;
    }

    public IReadOnlyList<string> GetDescendants(string nodeId)
    {
        var result = new List<string>();
        var stack = new Stack<string>();
        stack.Push(nodeId);
        while (stack.Count > 0)
        {
            var current = stack.Pop();
            if (!Children.TryGetValue(current, out var children))
            {
                continue;
            }

            foreach (var child in children)
            {
                result.Add(child);
                stack.Push(child);
            }
        }

        result.Sort(StringComparer.Ordinal);
        return result;
    }

    public IReadOnlyList<string> GetAncestors(string nodeId)
    {
        var result = new List<string>();
        var current = nodeId;
        while (Nodes[current].ParentId is { } parentId)
        {
            result.Add(parentId);
            current = parentId;
        }

        return result;
    }

    public static GeometryDefinition CloneGeometry(GeometryDefinition source)
    {
        return new GeometryDefinition
        {
            Id = source.Id,
            NodeId = source.NodeId,
            Kind = source.Kind,
            BoundsMin = source.BoundsMin?.ToArray(),
            BoundsMax = source.BoundsMax?.ToArray(),
            Center = source.Center?.ToArray(),
            Radius = source.Radius,
            Vertices = source.Vertices?.Select(vertex => vertex.ToArray()).ToArray(),
            Indices = source.Indices?.ToArray()
        };
    }

    public static CameraDefinition CloneCamera(CameraDefinition source)
    {
        return new CameraDefinition
        {
            Position = source.Position.ToArray(),
            Target = source.Target.ToArray(),
            Up = source.Up.ToArray(),
            FieldOfViewYDegrees = source.FieldOfViewYDegrees,
            NearPlane = source.NearPlane,
            FarPlane = source.FarPlane,
            ViewportWidth = source.ViewportWidth,
            ViewportHeight = source.ViewportHeight
        };
    }

    private void RebuildChildren()
    {
        Children = new Dictionary<string, List<string>>(StringComparer.Ordinal);
        var roots = new List<string>();
        foreach (var node in Nodes.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            if (node.ParentId is null)
            {
                roots.Add(node.Id);
                continue;
            }

            if (!Children.TryGetValue(node.ParentId, out var children))
            {
                children = [];
                Children[node.ParentId] = children;
            }

            children.Add(node.Id);
        }

        foreach (var children in Children.Values)
        {
            children.Sort(StringComparer.Ordinal);
        }

        roots.Sort(StringComparer.Ordinal);
        Roots = roots;
    }
}

public static class SceneHashing
{
    public static string ComputeContentHash(SceneState scene)
    {
        var builder = new StringBuilder();
        builder.Append(SceneFormats.Scene).Append('\n');
        builder.Append(scene.Seed).Append('\n');
        AppendCamera(builder, scene.Camera);
        foreach (var node in scene.Nodes.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            builder.Append("N|").Append(node.Id).Append('|')
                .Append(node.ParentId ?? "-").Append('|');
            SpatialMath.AppendMatrix(builder, node.LocalTransform);
            builder.Append('\n');
        }

        foreach (var geometry in scene.Geometries.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            AppendGeometry(builder, geometry);
        }

        return Hash(builder.ToString());
    }

    public static string ComputeContentHash(SceneDocument scene)
    {
        return ComputeContentHash(SceneState.FromDocument(scene));
    }

    public static string ComputeStructureHash(SceneState scene)
    {
        var builder = new StringBuilder();
        foreach (var node in scene.Nodes.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            builder.Append("N|").Append(node.Id).Append('|')
                .Append(node.ParentId ?? "-").Append('\n');
        }

        foreach (var geometry in scene.Geometries.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            builder.Append("G|").Append(geometry.Id).Append('|')
                .Append(geometry.NodeId).Append('|')
                .Append(geometry.Kind).Append('\n');
        }

        return Hash(builder.ToString());
    }

    public static string ComputeStructureHash(SceneDocument scene) =>
        ComputeStructureHash(SceneState.FromDocument(scene));

    public static string Hash(string value) =>
        Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(value))).ToLowerInvariant();

    private static void AppendCamera(StringBuilder builder, CameraDefinition camera)
    {
        builder.Append("C|");
        SpatialMath.AppendVector(builder, camera.PositionVector);
        builder.Append('|');
        SpatialMath.AppendVector(builder, camera.TargetVector);
        builder.Append('|');
        SpatialMath.AppendVector(builder, camera.UpVector);
        builder.Append('|').Append(SpatialMath.CanonicalFloat(camera.FieldOfViewYDegrees))
            .Append('|').Append(SpatialMath.CanonicalFloat(camera.NearPlane))
            .Append('|').Append(SpatialMath.CanonicalFloat(camera.FarPlane))
            .Append('|').Append(camera.ViewportWidth)
            .Append('|').Append(camera.ViewportHeight).Append('\n');
    }

    private static void AppendGeometry(StringBuilder builder, GeometryDefinition geometry)
    {
        builder.Append("G|").Append(geometry.Id).Append('|')
            .Append(geometry.NodeId).Append('|')
            .Append(geometry.Kind).Append('|');
        if (geometry.BoundsMin is not null)
        {
            AppendFloats(builder, geometry.BoundsMin);
        }

        builder.Append('|');
        if (geometry.BoundsMax is not null)
        {
            AppendFloats(builder, geometry.BoundsMax);
        }

        builder.Append('|');
        if (geometry.Center is not null)
        {
            AppendFloats(builder, geometry.Center);
        }

        builder.Append('|');
        if (geometry.Radius is not null)
        {
            builder.Append(SpatialMath.CanonicalFloat(geometry.Radius.Value));
        }

        builder.Append('|');
        if (geometry.Vertices is not null)
        {
            foreach (var vertex in geometry.Vertices)
            {
                AppendFloats(builder, vertex);
                builder.Append(';');
            }
        }

        builder.Append('|');
        if (geometry.Indices is not null)
        {
            foreach (var index in geometry.Indices)
            {
                builder.Append(index).Append(',');
            }
        }

        builder.Append('\n');
    }

    private static void AppendFloats(StringBuilder builder, IReadOnlyList<float> values)
    {
        for (var i = 0; i < values.Count; i++)
        {
            if (i > 0)
            {
                builder.Append(',');
            }

            builder.Append(SpatialMath.CanonicalFloat(values[i]));
        }
    }
}
