using System.Numerics;

namespace GameEditor.Spatial;

public sealed class SpatialRuntimeOptions
{
    public int MaxDegreeOfParallelism { get; init; } = Math.Max(1, Environment.ProcessorCount / 2);
    public int MaxQueryBatchSize { get; init; } = 512;
    public int MaxCandidateBuffer { get; init; } = 4096;
    public string ParameterVersion { get; init; } = SpatialParameterVersion.Current;
}

public sealed record NodeDerived(
    string NodeId,
    Matrix4x4 WorldTransform,
    Bounds3 WorldBounds,
    long TransformVersion,
    long BoundsVersion,
    long CacheVersion,
    bool IsReady,
    string? Failure);

public sealed record GeometryEntry(
    string EntryId,
    string GeometryId,
    string NodeId,
    GeometryKind Kind,
    Bounds3 LocalBounds,
    Bounds3 WorldBounds,
    long NodeTransformVersion,
    long CacheVersion,
    bool IsReady,
    string? Failure);

public sealed record IndexNode(
    string NodeId,
    Bounds3 AggregateBounds,
    long CacheVersion,
    bool IsReady);

public sealed record RuntimeSnapshot(
    long Revision,
    string ContentHash,
    string StructureHash,
    long StructureVersion,
    string ParameterVersion,
    IReadOnlyList<NodeDerived> Nodes,
    IReadOnlyList<GeometryEntry> Entries,
    IReadOnlyList<IndexNode> IndexNodes,
    IReadOnlyList<DependencyEdge> Dependencies);

public sealed record RayHit(string NodeId, string GeometryId, float Distance, Vector3 Point);

public sealed record FrustumSelection(IReadOnlyList<string> NodeIds);

public sealed class IncrementalStats
{
    public int TotalNodes { get; init; }
    public int RebuiltNodes { get; init; }
    public int ReusedNodes { get; init; }
    public int TotalEntries { get; init; }
    public int RebuiltEntries { get; init; }
    public int ReusedEntries { get; init; }
    public int TotalIndexNodes { get; init; }
    public int RebuiltIndexNodes { get; init; }
    public int ReusedIndexNodes { get; init; }

    public bool IsIncremental => ReusedNodes > 0 || ReusedEntries > 0 || ReusedIndexNodes > 0;

    public override string ToString() =>
        $"nodes {RebuiltNodes}/{TotalNodes} rebuilt ({ReusedNodes} reused), " +
        $"entries {RebuiltEntries}/{TotalEntries} rebuilt ({ReusedEntries} reused), " +
        $"index {RebuiltIndexNodes}/{TotalIndexNodes} rebuilt ({ReusedIndexNodes} reused)";
}

public sealed record EditApplicationResult(
    long Revision,
    IReadOnlyList<InvalidationRecord> Invalidations,
    IncrementalStats Stats,
    string ContentHash,
    long StructureVersion);

public sealed record RenderGeometry(
    string GeometryId,
    string NodeId,
    GeometryKind Kind,
    Bounds3 LocalBounds,
    Bounds3 WorldBounds,
    Matrix4x4 WorldTransform,
    bool IsReady);
