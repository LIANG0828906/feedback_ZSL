using System.Numerics;

namespace GameEditor.Spatial;

public sealed record Divergence(
    string ObjectId,
    string Field,
    string IncrementalValue,
    string FullValue);

public sealed class EquivalenceReport
{
    public bool IsEqual => Divergence is null;
    public DomainError? Error => IsEqual
        ? null
        : new DomainError(
            ErrorCode.E_DIVERGE,
            $"First divergence at '{Divergence!.ObjectId}' field '{Divergence.Field}'.",
            Divergence.ObjectId);
    public Divergence? Divergence { get; init; }
    public required RuntimeSnapshot IncrementalSnapshot { get; init; }
    public required RuntimeSnapshot FullRebuildSnapshot { get; init; }
    public required IReadOnlyList<RayHit?> IncrementalRays { get; init; }
    public required IReadOnlyList<RayHit?> FullRays { get; init; }
    public required IReadOnlyList<IReadOnlyList<string>> IncrementalBoxes { get; init; }
    public required IReadOnlyList<IReadOnlyList<string>> FullBoxes { get; init; }

    public string ToSummary() => IsEqual
        ? $"EQUAL: {IncrementalSnapshot.Nodes.Count} nodes, {IncrementalSnapshot.Entries.Count} entries, " +
          $"{IncrementalRays.Count} rays, {IncrementalBoxes.Count} boxes."
        : Error!.ToString();
}

public static class EquivalenceChecker
{
    public static EquivalenceReport Compare(
        SpatialRuntime incremental,
        SpatialRuntime fullRebuild,
        IReadOnlyList<RayQueryDefinition> rays,
        IReadOnlyList<BoxQueryDefinition> boxes,
        CancellationToken cancellationToken = default)
    {
        var left = incremental.CaptureSnapshot();
        var right = fullRebuild.CaptureSnapshot();
        var leftRays = new List<RayHit?>();
        var rightRays = new List<RayHit?>();
        var leftBoxes = new List<IReadOnlyList<string>>();
        var rightBoxes = new List<IReadOnlyList<string>>();

        foreach (var ray in rays)
        {
            cancellationToken.ThrowIfCancellationRequested();
            leftRays.Add(incremental.Raycast(ray, cancellationToken).Value);
            rightRays.Add(fullRebuild.Raycast(ray, cancellationToken).Value);
        }

        foreach (var box in boxes)
        {
            cancellationToken.ThrowIfCancellationRequested();
            leftBoxes.Add(incremental.SelectBox(box, cancellationToken).Value!);
            rightBoxes.Add(fullRebuild.SelectBox(box, cancellationToken).Value!);
        }

        var divergence = FindDivergence(left, right, rays, leftRays, rightRays, boxes, leftBoxes, rightBoxes);
        return new EquivalenceReport
        {
            Divergence = divergence,
            IncrementalSnapshot = left,
            FullRebuildSnapshot = right,
            IncrementalRays = leftRays,
            FullRays = rightRays,
            IncrementalBoxes = leftBoxes,
            FullBoxes = rightBoxes
        };
    }

    private static Divergence? FindDivergence(
        RuntimeSnapshot left,
        RuntimeSnapshot right,
        IReadOnlyList<RayQueryDefinition> rays,
        IReadOnlyList<RayHit?> leftRays,
        IReadOnlyList<RayHit?> rightRays,
        IReadOnlyList<BoxQueryDefinition> boxes,
        IReadOnlyList<IReadOnlyList<string>> leftBoxes,
        IReadOnlyList<IReadOnlyList<string>> rightBoxes)
    {
        var leftNodes = left.Nodes.ToDictionary(item => item.NodeId, StringComparer.Ordinal);
        var rightNodes = right.Nodes.ToDictionary(item => item.NodeId, StringComparer.Ordinal);
        foreach (var id in leftNodes.Keys.Union(rightNodes.Keys).OrderBy(item => item, StringComparer.Ordinal))
        {
            if (!leftNodes.TryGetValue(id, out var leftNode))
            {
                return new Divergence(id, "node.exists", "missing", "present");
            }

            if (!rightNodes.TryGetValue(id, out var rightNode))
            {
                return new Divergence(id, "node.exists", "present", "missing");
            }

            if (leftNode.IsReady != rightNode.IsReady)
            {
                return new Divergence(id, "node.isReady", leftNode.IsReady.ToString(), rightNode.IsReady.ToString());
            }

            var matrixDivergence = CompareMatrix(id, "worldMatrix", leftNode.WorldTransform, rightNode.WorldTransform);
            if (matrixDivergence is not null)
            {
                return matrixDivergence;
            }

            var boundsDivergence = CompareBounds(id, "worldBounds", leftNode.WorldBounds, rightNode.WorldBounds);
            if (boundsDivergence is not null)
            {
                return boundsDivergence;
            }
        }

        var leftEntries = left.Entries.ToDictionary(item => item.GeometryId, StringComparer.Ordinal);
        var rightEntries = right.Entries.ToDictionary(item => item.GeometryId, StringComparer.Ordinal);
        foreach (var id in leftEntries.Keys.Union(rightEntries.Keys).OrderBy(item => item, StringComparer.Ordinal))
        {
            if (!leftEntries.TryGetValue(id, out var leftEntry))
            {
                return new Divergence($"entry:{id}", "entry.exists", "missing", "present");
            }

            if (!rightEntries.TryGetValue(id, out var rightEntry))
            {
                return new Divergence($"entry:{id}", "entry.exists", "present", "missing");
            }

            if (leftEntry.IsReady != rightEntry.IsReady)
            {
                return new Divergence($"entry:{id}", "entry.isReady", leftEntry.IsReady.ToString(), rightEntry.IsReady.ToString());
            }

            var boundsDivergence = CompareBounds($"entry:{id}", "worldBounds", leftEntry.WorldBounds, rightEntry.WorldBounds);
            if (boundsDivergence is not null)
            {
                return boundsDivergence;
            }
        }

        var leftIndex = left.IndexNodes.ToDictionary(item => item.NodeId, StringComparer.Ordinal);
        var rightIndex = right.IndexNodes.ToDictionary(item => item.NodeId, StringComparer.Ordinal);
        foreach (var id in leftIndex.Keys.Union(rightIndex.Keys).OrderBy(item => item, StringComparer.Ordinal))
        {
            if (!leftIndex.TryGetValue(id, out var leftNode))
            {
                return new Divergence($"index:{id}", "index.exists", "missing", "present");
            }

            if (!rightIndex.TryGetValue(id, out var rightNode))
            {
                return new Divergence($"index:{id}", "index.exists", "present", "missing");
            }

            var boundsDivergence = CompareBounds($"index:{id}", "aggregateBounds", leftNode.AggregateBounds, rightNode.AggregateBounds);
            if (boundsDivergence is not null)
            {
                return boundsDivergence;
            }
        }

        if (left.ContentHash != right.ContentHash)
        {
            return new Divergence("scene:content", "contentHash", left.ContentHash, right.ContentHash);
        }

        if (left.StructureHash != right.StructureHash)
        {
            return new Divergence("scene:structure", "structureHash", left.StructureHash, right.StructureHash);
        }

        for (var i = 0; i < rays.Count; i++)
        {
            var leftHit = leftRays[i];
            var rightHit = rightRays[i];
            if (leftHit is null != rightHit is null)
            {
                return new Divergence($"ray:{rays[i].Id}", "hit", Describe(leftHit), Describe(rightHit));
            }

            if (leftHit is not null && rightHit is not null)
            {
                if (leftHit.NodeId != rightHit.NodeId)
                {
                    return new Divergence($"ray:{rays[i].Id}", "hit.nodeId", leftHit.NodeId, rightHit.NodeId);
                }

                if (leftHit.GeometryId != rightHit.GeometryId)
                {
                    return new Divergence($"ray:{rays[i].Id}", "hit.geometryId", leftHit.GeometryId, rightHit.GeometryId);
                }

                if (!SameFloat(leftHit.Distance, rightHit.Distance))
                {
                    return new Divergence($"ray:{rays[i].Id}", "hit.distance", Describe(leftHit.Distance), Describe(rightHit.Distance));
                }

                if (!SameVector(leftHit.Point, rightHit.Point))
                {
                    return new Divergence($"ray:{rays[i].Id}", "hit.point", Describe(leftHit.Point), Describe(rightHit.Point));
                }
            }
        }

        for (var i = 0; i < boxes.Count; i++)
        {
            if (!leftBoxes[i].SequenceEqual(rightBoxes[i], StringComparer.Ordinal))
            {
                return new Divergence(
                    $"box:{boxes[i].Id}",
                    "nodeIds",
                    string.Join(",", leftBoxes[i]),
                    string.Join(",", rightBoxes[i]));
            }
        }

        return null;
    }

    private static Divergence? CompareMatrix(string id, string field, Matrix4x4 left, Matrix4x4 right)
    {
        var leftValues = SpatialMath.MatrixToRowMajor(left);
        var rightValues = SpatialMath.MatrixToRowMajor(right);
        for (var i = 0; i < 16; i++)
        {
            if (!SameFloat(leftValues[i], rightValues[i]))
            {
                return new Divergence(id, $"{field}[{i}]", Describe(leftValues[i]), Describe(rightValues[i]));
            }
        }

        return null;
    }

    private static Divergence? CompareBounds(string id, string field, Bounds3 left, Bounds3 right)
    {
        if (left.IsEmpty != right.IsEmpty)
        {
            return new Divergence(id, field, left.IsEmpty ? "empty" : "non-empty", right.IsEmpty ? "empty" : "non-empty");
        }

        if (left.IsEmpty)
        {
            return null;
        }

        if (!SameVector(left.Min, right.Min))
        {
            return new Divergence(id, $"{field}.min", Describe(left.Min), Describe(right.Min));
        }

        if (!SameVector(left.Max, right.Max))
        {
            return new Divergence(id, $"{field}.max", Describe(left.Max), Describe(right.Max));
        }

        return null;
    }

    private static bool SameFloat(float left, float right) =>
        BitConverter.SingleToInt32Bits(left) == BitConverter.SingleToInt32Bits(right);

    private static bool SameVector(Vector3 left, Vector3 right) =>
        SameFloat(left.X, right.X) && SameFloat(left.Y, right.Y) && SameFloat(left.Z, right.Z);

    private static string Describe(RayHit? hit) =>
        hit is null
            ? "<miss>"
            : $"{hit.NodeId}/{hit.GeometryId}@{SpatialMath.F(hit.Distance)}";

    private static string Describe(float value) => SpatialMath.F(value);

    private static string Describe(Vector3 value) =>
        $"({SpatialMath.F(value.X)},{SpatialMath.F(value.Y)},{SpatialMath.F(value.Z)})";
}
