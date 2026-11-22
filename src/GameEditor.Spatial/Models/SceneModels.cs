using System.Numerics;
using System.Text.Json.Serialization;

namespace GameEditor.Spatial;

public static class SceneFormats
{
    public const string Scene = "game-editor-scene/v1";
    public const string Queries = "game-editor-queries/v1";
    public const string GroundTruth = "game-editor-ground-truth/v1";
    public const string Edits = "game-editor-edits/v1";
    public const string EditTruth = "game-editor-edit-truth/v1";
    public const string Snapshot = "game-editor-spatial-snapshot/v1";
}

public sealed class CameraDefinition
{
    public float[] Position { get; set; } = [0, 7, 18];
    public float[] Target { get; set; } = [0, 1, 0];
    public float[] Up { get; set; } = [0, 1, 0];
    public float FieldOfViewYDegrees { get; set; } = 50;
    public float NearPlane { get; set; } = 0.1f;
    public float FarPlane { get; set; } = 200;
    public int ViewportWidth { get; set; } = 1280;
    public int ViewportHeight { get; set; } = 720;

    [JsonIgnore]
    public Vector3 PositionVector => SpatialMath.VectorFromArray(Position);

    [JsonIgnore]
    public Vector3 TargetVector => SpatialMath.VectorFromArray(Target);

    [JsonIgnore]
    public Vector3 UpVector => SpatialMath.VectorFromArray(Up);
}

public sealed class SceneNodeDefinition
{
    public string Id { get; set; } = string.Empty;
    public string? ParentId { get; set; }
    public float[] LocalMatrix { get; set; } = new float[16];
}

public sealed class GeometryDefinition
{
    public string Id { get; set; } = string.Empty;
    public string NodeId { get; set; } = string.Empty;
    public string Kind { get; set; } = string.Empty;
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public float[]? BoundsMin { get; set; }
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public float[]? BoundsMax { get; set; }
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public float[]? Center { get; set; }
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public float? Radius { get; set; }
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public float[][]? Vertices { get; set; }
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public int[]? Indices { get; set; }

    [JsonIgnore]
    public GeometryKind GeometryKind => Kind switch
    {
        "aabb" => GeometryKind.Aabb,
        "sphere" => GeometryKind.Sphere,
        "triangleMesh" => GeometryKind.TriangleMesh,
        _ => GeometryKind.Unknown
    };

    [JsonIgnore]
    public Bounds3 LocalBounds
    {
        get
        {
            if (GeometryKind == GeometryKind.Aabb)
            {
                return new Bounds3(
                    SpatialMath.VectorFromArray(BoundsMin!),
                    SpatialMath.VectorFromArray(BoundsMax!));
            }

            if (GeometryKind == GeometryKind.Sphere)
            {
                var center = SpatialMath.VectorFromArray(Center!);
                var radius = Radius!.Value;
                return new Bounds3(center - new Vector3(radius), center + new Vector3(radius));
            }

            var bounds = Bounds3.Empty;
            foreach (var vertex in Vertices!)
            {
                bounds = bounds.Encapsulate(SpatialMath.VectorFromArray(vertex));
            }

            return bounds;
        }
    }
}

public enum GeometryKind
{
    Unknown = 0,
    Aabb,
    Sphere,
    TriangleMesh
}

public sealed class SceneDeclaration
{
    public int NodeCount { get; set; }
    public int GeometryCount { get; set; }
    public string ContentHash { get; set; } = string.Empty;
    public string StructureHash { get; set; } = string.Empty;
    public string ParamVersion { get; set; } = string.Empty;
}

public sealed class SceneDocument
{
    public string Format { get; set; } = SceneFormats.Scene;
    public int Seed { get; set; }
    public CameraDefinition Camera { get; set; } = new();
    public List<SceneNodeDefinition> Nodes { get; set; } = [];
    public List<GeometryDefinition> Geometries { get; set; } = [];
    public SceneDeclaration Declaration { get; set; } = new();

    [JsonIgnore]
    public Dictionary<string, GeometryDefinition> GeometryById { get; private set; } = [];

    public void RebuildLookup()
    {
        GeometryById = Geometries.ToDictionary(item => item.Id, StringComparer.Ordinal);
    }
}

public sealed class RayQueryDefinition
{
    public string Id { get; set; } = string.Empty;
    public float ScreenX { get; set; }
    public float ScreenY { get; set; }
}

public sealed class BoxQueryDefinition
{
    public string Id { get; set; } = string.Empty;
    public float[] Rect { get; set; } = new float[4];
}

public sealed class QueryManifest
{
    public string Format { get; set; } = SceneFormats.Queries;
    public string SceneContentHash { get; set; } = string.Empty;
    public List<RayQueryDefinition> Rays { get; set; } = [];
    public List<BoxQueryDefinition> Boxes { get; set; } = [];
}

public sealed class GeometryRayTruth
{
    public string GeometryId { get; set; } = string.Empty;
    public string NodeId { get; set; } = string.Empty;
    public float Distance { get; set; }
    public float[] Point { get; set; } = new float[3];
}

public sealed class RayTruth
{
    public string QueryId { get; set; } = string.Empty;
    public bool Hit { get; set; }
    public string? NodeId { get; set; }
    public string? GeometryId { get; set; }
    public float Distance { get; set; }
    public float[]? Point { get; set; }
    public List<GeometryRayTruth> AllHits { get; set; } = [];
}

public sealed class BoxTruth
{
    public string QueryId { get; set; } = string.Empty;
    public List<string> NodeIds { get; set; } = [];
}

public sealed class GroundTruthManifest
{
    public string Format { get; set; } = SceneFormats.GroundTruth;
    public string SceneContentHash { get; set; } = string.Empty;
    public List<RayTruth> Rays { get; set; } = [];
    public List<BoxTruth> Boxes { get; set; } = [];
}

public sealed class SceneEdit
{
    public string Id { get; set; } = string.Empty;
    public string Kind { get; set; } = string.Empty;
    public string? NodeId { get; set; }
    public float[]? LocalMatrix { get; set; }
    public GeometryDefinition? Geometry { get; set; }
    public string? GeometryId { get; set; }
    public List<string>? Selection { get; set; }
    public float[]? Pivot { get; set; }
    public string? Operation { get; set; }
    public float[]? Vector { get; set; }
    public float[]? Axis { get; set; }
    public float? AngleDegrees { get; set; }
    public float? Factor { get; set; }
}

public sealed class EditSequenceDefinition
{
    public string Format { get; set; } = SceneFormats.Edits;
    public string BaseContentHash { get; set; } = string.Empty;
    public List<SceneEdit> Edits { get; set; } = [];
}

public sealed class NodeTransformTruth
{
    public string NodeId { get; set; } = string.Empty;
    public float[] WorldMatrix { get; set; } = new float[16];
    public float[] BoundsMin { get; set; } = new float[3];
    public float[] BoundsMax { get; set; } = new float[3];
    public bool BoundsEmpty { get; set; }
}

public sealed class EditTruthManifest
{
    public string Format { get; set; } = SceneFormats.EditTruth;
    public string BaseContentHash { get; set; } = string.Empty;
    public string FinalContentHash { get; set; } = string.Empty;
    public List<EditStepTruth> Steps { get; set; } = [];
    public List<NodeTransformTruth> FinalNodes { get; set; } = [];
    public List<RayTruth> FinalRays { get; set; } = [];
    public List<BoxTruth> FinalBoxes { get; set; } = [];
}

public sealed class EditStepTruth
{
    public string EditId { get; set; } = string.Empty;
    public string ContentHash { get; set; } = string.Empty;
    public long StructureVersion { get; set; }
    public List<NodeTransformTruth> Nodes { get; set; } = [];
}
