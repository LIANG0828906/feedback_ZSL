using System.Numerics;

namespace GameEditor.Spatial;

public static class ReferenceEvaluator
{
    public static Dictionary<string, Matrix4x4> ComputeWorldTransforms(SceneState scene)
    {
        var result = new Dictionary<string, Matrix4x4>(StringComparer.Ordinal);
        foreach (var nodeId in scene.GetTopologicalOrder())
        {
            var node = scene.Nodes[nodeId];
            result[nodeId] = node.ParentId is { } parentId
                ? node.LocalTransform * result[parentId]
                : node.LocalTransform;
        }

        return result;
    }

    public static IReadOnlyList<RayHit> RaycastAll(
        SceneState scene,
        CameraModel camera,
        RayQueryDefinition query)
    {
        var world = ComputeWorldTransforms(scene);
        var ray = camera.ScreenToWorldRay(query.ScreenX, query.ScreenY);
        var hits = new List<RayHit>();
        foreach (var geometry in scene.Geometries.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            var node = scene.Nodes[geometry.NodeId];
            if (!SpatialMath.TryInvert(world[node.Id], out _))
            {
                continue;
            }

            if (GeometryIntersections.TryIntersect(geometry, world[node.Id], ray, out var hit))
            {
                hits.Add(new RayHit(hit.NodeId, hit.GeometryId, hit.Distance, hit.Point));
            }
        }

        hits.Sort(CompareHits);
        return hits;
    }

    public static IReadOnlyList<string> SelectBox(
        SceneState scene,
        CameraModel camera,
        BoxQueryDefinition query)
    {
        var world = ComputeWorldTransforms(scene);
        var rect = new ScreenRect(query.Rect[0], query.Rect[1], query.Rect[2], query.Rect[3]);
        var result = new HashSet<string>(StringComparer.Ordinal);
        foreach (var geometry in scene.Geometries.Values)
        {
            var bounds = geometry.LocalBounds.Transform(world[geometry.NodeId]);
            var projected = camera.ProjectBounds(bounds);
            if (projected.IsValid && projected.Intersects(rect))
            {
                result.Add(geometry.NodeId);
            }
        }

        return result.OrderBy(item => item, StringComparer.Ordinal).ToList();
    }

    public static Bounds3 ComputeNodeBounds(SceneState scene, string nodeId, IReadOnlyDictionary<string, Matrix4x4> world)
    {
        var bounds = Bounds3.Empty;
        foreach (var geometry in scene.Geometries.Values)
        {
            if (geometry.NodeId == nodeId)
            {
                bounds = bounds.Encapsulate(geometry.LocalBounds.Transform(world[nodeId]));
            }
        }

        return bounds;
    }

    public static Result<SceneState> ApplyEdit(SceneState scene, SceneEdit edit)
    {
        switch (edit.Kind)
        {
            case "transform":
            {
                if (edit.NodeId is null || !scene.Nodes.ContainsKey(edit.NodeId))
                {
                    return Result<SceneState>.Fail(ErrorCode.E_REF, "Transform edit references an unknown node.", edit.Id);
                }

                if (edit.LocalMatrix is not { Length: 16 })
                {
                    return Result<SceneState>.Fail(ErrorCode.E_SCHEMA, "Transform edit requires localMatrix.", edit.Id);
                }

                var matrix = SpatialMath.MatrixFromRowMajor(edit.LocalMatrix);
                if (!SpatialMath.IsFinite(matrix))
                {
                    return Result<SceneState>.Fail(ErrorCode.E_SCHEMA, "Transform edit is not finite.", edit.Id);
                }

                return Result<SceneState>.Ok(scene.WithNodeTransforms(
                    new Dictionary<string, Matrix4x4>(StringComparer.Ordinal) { [edit.NodeId] = matrix }));
            }

            case "addGeometry":
            {
                if (edit.Geometry is null)
                {
                    return Result<SceneState>.Fail(ErrorCode.E_SCHEMA, "Add edit requires geometry.", edit.Id);
                }

                if (scene.Geometries.ContainsKey(edit.Geometry.Id))
                {
                    return Result<SceneState>.Fail(ErrorCode.E_SCHEMA, "Geometry id already exists.", edit.Id);
                }

                var error = SceneValidator.ValidateGeometryDefinition(edit.Geometry, scene, edit.Id);
                return error is not null
                    ? Result<SceneState>.Fail(error.Code, error.Message, error.Path)
                    : Result<SceneState>.Ok(scene.WithGeometryAdded(edit.Geometry));
            }

            case "removeGeometry":
            {
                if (edit.GeometryId is null || !scene.Geometries.ContainsKey(edit.GeometryId))
                {
                    return Result<SceneState>.Fail(ErrorCode.E_REF, "Remove edit references an unknown geometry.", edit.Id);
                }

                return Result<SceneState>.Ok(scene.WithGeometryRemoved(edit.GeometryId));
            }

            case "pivot":
            {
                var operationResult = PivotOperation.FromEdit(edit);
                if (!operationResult.IsSuccess)
                {
                    return Result<SceneState>.Fail(
                        operationResult.Error!.Code,
                        operationResult.Error.Message,
                        operationResult.Error.Path);
                }

                if (edit.Selection is null ||
                    edit.Selection.Count == 0 ||
                    edit.Pivot is not { Length: 3 } ||
                    !SpatialMath.IsFinite(SpatialMath.VectorFromArray(edit.Pivot)))
                {
                    return Result<SceneState>.Fail(ErrorCode.E_SCHEMA, "Pivot edit requires selection and finite pivot.", edit.Id);
                }

                var selection = new HashSet<string>(edit.Selection, StringComparer.Ordinal);
                foreach (var nodeId in selection)
                {
                    if (!scene.Nodes.ContainsKey(nodeId))
                    {
                        return Result<SceneState>.Fail(ErrorCode.E_REF, $"Unknown selected node '{nodeId}'.", edit.Id);
                    }
                }

                var roots = selection
                    .Where(candidate => !scene.GetAncestors(candidate).Any(selection.Contains))
                    .OrderBy(item => item, StringComparer.Ordinal)
                    .ToList();
                var world = ComputeWorldTransforms(scene);
                var pivot = SpatialMath.VectorFromArray(edit.Pivot);
                var updates = new Dictionary<string, Matrix4x4>(StringComparer.Ordinal);
                foreach (var rootId in roots)
                {
                    var node = scene.Nodes[rootId];
                    var parentWorld = node.ParentId is { } parentId ? world[parentId] : Matrix4x4.Identity;
                    if (!SpatialMath.TryInvert(parentWorld, out var inverseParent))
                    {
                        return Result<SceneState>.Fail(ErrorCode.E_INVALID_OP, "Parent transform is singular.", edit.Id);
                    }

                    var local = node.LocalTransform;
                    var operation = operationResult.Value!;
                    Matrix4x4 pivotTransform;
                    if (operation.Kind == PivotOperationKind.Translate)
                    {
                        pivotTransform = Matrix4x4.CreateTranslation(
                            Vector3.TransformNormal(operation.Translation, inverseParent));
                    }
                    else
                    {
                        var worldOperation = operation.Kind == PivotOperationKind.Rotate
                            ? Matrix4x4.CreateFromAxisAngle(
                                SpatialMath.SafeNormalize(operation.Axis, Vector3.UnitY),
                                operation.AngleDegrees * MathF.PI / 180f)
                            : Matrix4x4.CreateScale(operation.ScaleFactor);
                        var worldPivotOperation =
                            Matrix4x4.CreateTranslation(-pivot) *
                            worldOperation *
                            Matrix4x4.CreateTranslation(pivot);
                        pivotTransform = parentWorld * worldPivotOperation * inverseParent;
                    }

                    updates[rootId] = local * pivotTransform;
                }

                return Result<SceneState>.Ok(scene.WithNodeTransforms(updates));
            }

            default:
                return Result<SceneState>.Fail(ErrorCode.E_INVALID_OP, $"Unsupported edit kind '{edit.Kind}'.", edit.Id);
        }
    }

    public static List<NodeTransformTruth> CaptureNodeTruth(SceneState scene)
    {
        var world = ComputeWorldTransforms(scene);
        var result = new List<NodeTransformTruth>();
        foreach (var node in scene.Nodes.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
        {
            var bounds = ComputeNodeBounds(scene, node.Id, world);
            result.Add(new NodeTransformTruth
            {
                NodeId = node.Id,
                WorldMatrix = SpatialMath.MatrixToRowMajor(world[node.Id]),
                BoundsMin = bounds.IsEmpty ? [0, 0, 0] : bounds.ToMinArray(),
                BoundsMax = bounds.IsEmpty ? [0, 0, 0] : bounds.ToMaxArray(),
                BoundsEmpty = bounds.IsEmpty
            });
        }

        return result;
    }

    public static List<RayTruth> BuildRayTruth(SceneState scene, QueryManifest queries)
    {
        var camera = new CameraModel(scene.Camera);
        var result = new List<RayTruth>();
        foreach (var query in queries.Rays)
        {
            var hits = RaycastAll(scene, camera, query);
            var first = hits.Count > 0 ? hits[0] : null;
            result.Add(new RayTruth
            {
                QueryId = query.Id,
                Hit = first is not null,
                NodeId = first?.NodeId,
                GeometryId = first?.GeometryId,
                Distance = first?.Distance ?? 0,
                Point = first is null ? null : SpatialMath.VectorToArray(first.Point),
                AllHits = hits.Select(hit => new GeometryRayTruth
                {
                    GeometryId = hit.GeometryId,
                    NodeId = hit.NodeId,
                    Distance = hit.Distance,
                    Point = SpatialMath.VectorToArray(hit.Point)
                }).ToList()
            });
        }

        return result;
    }

    public static List<BoxTruth> BuildBoxTruth(SceneState scene, QueryManifest queries)
    {
        var camera = new CameraModel(scene.Camera);
        return queries.Boxes.Select(query => new BoxTruth
        {
            QueryId = query.Id,
            NodeIds = SelectBox(scene, camera, query).ToList()
        }).ToList();
    }

    private static int CompareHits(RayHit left, RayHit right)
    {
        var distance = left.Distance.CompareTo(right.Distance);
        if (distance != 0)
        {
            return distance;
        }

        var geometry = string.CompareOrdinal(left.GeometryId, right.GeometryId);
        return geometry != 0 ? geometry : string.CompareOrdinal(left.NodeId, right.NodeId);
    }
}
