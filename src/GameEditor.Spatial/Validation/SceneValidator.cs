namespace GameEditor.Spatial;

public static class SpatialParameterVersion
{
    public const string Current = "hier-aabb/ray-moller/box-projected-aabb/snapshot-v1";
}

public static class SceneValidator
{
    public static Result<SceneDocument> ValidateScene(SceneDocument scene)
    {
        if (!string.Equals(scene.Format, SceneFormats.Scene, StringComparison.Ordinal))
        {
            return Fail(ErrorCode.E_SCHEMA, $"Expected format '{SceneFormats.Scene}'.", "$.format");
        }

        var cameraError = ValidateCamera(scene.Camera);
        if (cameraError is not null)
        {
            return Result<SceneDocument>.Fail(cameraError.Code, cameraError.Message, cameraError.Path);
        }

        if (scene.Nodes.Count == 0)
        {
            return Fail(ErrorCode.E_SCHEMA, "Scene must contain at least one node.", "$.nodes");
        }

        var nodes = new Dictionary<string, SceneNodeDefinition>(StringComparer.Ordinal);
        for (var i = 0; i < scene.Nodes.Count; i++)
        {
            var node = scene.Nodes[i];
            var path = $"$.nodes[{i}]";
            if (string.IsNullOrWhiteSpace(node.Id))
            {
                return Fail(ErrorCode.E_SCHEMA, "Node id is required.", $"{path}.id");
            }

            if (!nodes.TryAdd(node.Id, node))
            {
                return Fail(ErrorCode.E_SCHEMA, $"Duplicate node id '{node.Id}'.", $"{path}.id");
            }

            if (node.LocalMatrix is not { Length: 16 })
            {
                return Fail(ErrorCode.E_SCHEMA, "Node localMatrix must contain 16 numbers.", $"{path}.localMatrix");
            }

            for (var valueIndex = 0; valueIndex < node.LocalMatrix.Length; valueIndex++)
            {
                if (!float.IsFinite(node.LocalMatrix[valueIndex]))
                {
                    return Fail(
                        ErrorCode.E_SCHEMA,
                        "Node transform contains a non-finite value.",
                        $"{path}.localMatrix[{valueIndex}]");
                }
            }
        }

        foreach (var node in scene.Nodes)
        {
            if (node.ParentId is not null && !nodes.ContainsKey(node.ParentId))
            {
                return Fail(
                    ErrorCode.E_REF,
                    $"Parent node '{node.ParentId}' does not exist.",
                    $"$.nodes[{scene.Nodes.IndexOf(node)}].parentId");
            }
        }

        var cycle = FindCycle(scene.Nodes, nodes);
        if (cycle is not null)
        {
            var cycleStart = cycle.Split(" -> ", StringSplitOptions.None)[0];
            var startIndex = scene.Nodes.FindIndex(node => node.Id == cycleStart);
            var cyclePath = startIndex >= 0
                ? $"$.nodes[{startIndex}].parentId"
                : "$.nodes";
            return Fail(ErrorCode.E_CYCLE, $"Parent cycle detected: {cycle}.", cyclePath);
        }

        var geometries = new Dictionary<string, GeometryDefinition>(StringComparer.Ordinal);
        for (var i = 0; i < scene.Geometries.Count; i++)
        {
            var geometry = scene.Geometries[i];
            var path = $"$.geometries[{i}]";
            if (string.IsNullOrWhiteSpace(geometry.Id))
            {
                return Fail(ErrorCode.E_SCHEMA, "Geometry id is required.", $"{path}.id");
            }

            if (!geometries.TryAdd(geometry.Id, geometry))
            {
                return Fail(ErrorCode.E_SCHEMA, $"Duplicate geometry id '{geometry.Id}'.", $"{path}.id");
            }

            if (!nodes.ContainsKey(geometry.NodeId))
            {
                return Fail(
                    ErrorCode.E_REF,
                    $"Geometry owner node '{geometry.NodeId}' does not exist.",
                    $"{path}.nodeId");
            }

            var geometryError = ValidateGeometryShape(geometry, path);
            if (geometryError is not null)
            {
                return Result<SceneDocument>.Fail(geometryError.Code, geometryError.Message, geometryError.Path);
            }
        }

        var state = SceneState.FromDocument(scene);
        if (scene.Declaration.NodeCount != scene.Nodes.Count)
        {
            return Fail(ErrorCode.E_SCHEMA, "Declared nodeCount does not match nodes.length.", "$.declaration.nodeCount");
        }

        if (scene.Declaration.GeometryCount != scene.Geometries.Count)
        {
            return Fail(ErrorCode.E_SCHEMA, "Declared geometryCount does not match geometries.length.", "$.declaration.geometryCount");
        }

        if (!string.Equals(scene.Declaration.ContentHash, state.ContentHash, StringComparison.Ordinal))
        {
            return Fail(ErrorCode.E_SCHEMA, "Declared contentHash does not match scene content.", "$.declaration.contentHash");
        }

        if (!string.Equals(scene.Declaration.StructureHash, state.StructureHash, StringComparison.Ordinal))
        {
            return Fail(ErrorCode.E_SCHEMA, "Declared structureHash does not match scene structure.", "$.declaration.structureHash");
        }

        if (!string.Equals(scene.Declaration.ParamVersion, SpatialParameterVersion.Current, StringComparison.Ordinal))
        {
            return Fail(ErrorCode.E_SCHEMA, "Declared parameter version is unsupported.", "$.declaration.paramVersion");
        }

        scene.RebuildLookup();
        return Result<SceneDocument>.Ok(scene);
    }

    public static Result<QueryManifest> ValidateQueries(QueryManifest queries, SceneDocument scene)
    {
        if (!string.Equals(queries.Format, SceneFormats.Queries, StringComparison.Ordinal))
        {
            return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, $"Expected format '{SceneFormats.Queries}'.", "$.format");
        }

        if (!string.Equals(queries.SceneContentHash, scene.Declaration.ContentHash, StringComparison.Ordinal))
        {
            return Result<QueryManifest>.Fail(
                ErrorCode.E_SCHEMA,
                "Query manifest references a different scene content hash.",
                "$.sceneContentHash");
        }

        var ids = new HashSet<string>(StringComparer.Ordinal);
        for (var i = 0; i < queries.Rays.Count; i++)
        {
            var query = queries.Rays[i];
            var path = $"$.rays[{i}]";
            if (string.IsNullOrWhiteSpace(query.Id) || !ids.Add(query.Id))
            {
                return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Ray query ids must be non-empty and unique.", $"{path}.id");
            }

            if (!float.IsFinite(query.ScreenX) || !float.IsFinite(query.ScreenY))
            {
                return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Ray screen coordinates must be finite.", path);
            }
        }

        for (var i = 0; i < queries.Boxes.Count; i++)
        {
            var query = queries.Boxes[i];
            var path = $"$.boxes[{i}]";
            if (string.IsNullOrWhiteSpace(query.Id) || !ids.Add(query.Id))
            {
                return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Box query ids must be non-empty and unique.", $"{path}.id");
            }

            if (query.Rect is not { Length: 4 })
            {
                return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Box rectangle must contain 4 numbers.", $"{path}.rect");
            }

            for (var j = 0; j < query.Rect.Length; j++)
            {
                if (!float.IsFinite(query.Rect[j]))
                {
                    return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Box rectangle values must be finite.", $"{path}.rect[{j}]");
                }
            }

            if (query.Rect[2] < 0 || query.Rect[3] < 0)
            {
                return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Box rectangle width and height must be non-negative.", $"{path}.rect");
            }
        }

        return Result<QueryManifest>.Ok(queries);
    }

    public static Result<GroundTruthManifest> ValidateTruth(
        GroundTruthManifest truth,
        QueryManifest queries,
        SceneDocument scene)
    {
        if (!string.Equals(truth.Format, SceneFormats.GroundTruth, StringComparison.Ordinal))
        {
            return Result<GroundTruthManifest>.Fail(ErrorCode.E_SCHEMA, "Unexpected ground-truth format.", "$.format");
        }

        if (!string.Equals(truth.SceneContentHash, scene.Declaration.ContentHash, StringComparison.Ordinal))
        {
            return Result<GroundTruthManifest>.Fail(
                ErrorCode.E_SCHEMA,
                "Ground truth references a different scene content hash.",
                "$.sceneContentHash");
        }

        if (truth.Rays.Count != queries.Rays.Count)
        {
            return Result<GroundTruthManifest>.Fail(ErrorCode.E_SCHEMA, "Ground-truth ray count does not match queries.", "$.rays");
        }

        if (truth.Boxes.Count != queries.Boxes.Count)
        {
            return Result<GroundTruthManifest>.Fail(ErrorCode.E_SCHEMA, "Ground-truth box count does not match queries.", "$.boxes");
        }

        var queryIds = queries.Rays.Select(item => item.Id).ToHashSet(StringComparer.Ordinal);
        for (var i = 0; i < truth.Rays.Count; i++)
        {
            var ray = truth.Rays[i];
            if (!queryIds.Remove(ray.QueryId))
            {
                return Result<GroundTruthManifest>.Fail(
                    ErrorCode.E_SCHEMA,
                    $"Ground truth references unknown ray query '{ray.QueryId}'.",
                    $"$.rays[{i}].queryId");
            }

            if (ray.Hit)
            {
                if (string.IsNullOrWhiteSpace(ray.NodeId) ||
                    string.IsNullOrWhiteSpace(ray.GeometryId) ||
                    !float.IsFinite(ray.Distance) || ray.Distance < 0 ||
                    ray.Point is not { Length: 3 } ||
                    !SpatialMath.IsFinite(SpatialMath.VectorFromArray(ray.Point)))
                {
                    return Result<GroundTruthManifest>.Fail(
                        ErrorCode.E_SCHEMA,
                        "Hit truth contains invalid distance or point.",
                        $"$.rays[{i}]");
                }

                if (!scene.GeometryById.ContainsKey(ray.GeometryId) ||
                    !scene.GeometryById[ray.GeometryId].NodeId.Equals(ray.NodeId, StringComparison.Ordinal))
                {
                    return Result<GroundTruthManifest>.Fail(
                        ErrorCode.E_REF,
                        "Ray truth references an unknown geometry/node pair.",
                        $"$.rays[{i}]");
                }
            }

            if (ray.Hit != (ray.AllHits.Count > 0))
            {
                return Result<GroundTruthManifest>.Fail(
                    ErrorCode.E_SCHEMA,
                    "Ray hit flag is inconsistent with per-geometry hits.",
                    $"$.rays[{i}].hit");
            }

            for (var hitIndex = 0; hitIndex < ray.AllHits.Count; hitIndex++)
            {
                var hit = ray.AllHits[hitIndex];
                if (string.IsNullOrWhiteSpace(hit.GeometryId) ||
                    string.IsNullOrWhiteSpace(hit.NodeId) ||
                    !float.IsFinite(hit.Distance) ||
                    hit.Distance < 0 ||
                    hit.Point is not { Length: 3 } ||
                    !SpatialMath.IsFinite(SpatialMath.VectorFromArray(hit.Point)))
                {
                    return Result<GroundTruthManifest>.Fail(
                        ErrorCode.E_SCHEMA,
                        "Per-geometry ray truth is invalid.",
                        $"$.rays[{i}].allHits[{hitIndex}]");
                }

                if (!scene.GeometryById.TryGetValue(hit.GeometryId, out var hitGeometry) ||
                    !hitGeometry.NodeId.Equals(hit.NodeId, StringComparison.Ordinal))
                {
                    return Result<GroundTruthManifest>.Fail(
                        ErrorCode.E_REF,
                        "Per-geometry ray truth references an unknown geometry/node pair.",
                        $"$.rays[{i}].allHits[{hitIndex}]");
                }
            }

            if (ray.Hit && ray.AllHits.Count > 0 &&
                (!ray.AllHits[0].GeometryId.Equals(ray.GeometryId, StringComparison.Ordinal) ||
                 !ray.AllHits[0].NodeId.Equals(ray.NodeId, StringComparison.Ordinal)))
            {
                return Result<GroundTruthManifest>.Fail(
                    ErrorCode.E_SCHEMA,
                    "Nearest hit does not match the first per-geometry hit.",
                    $"$.rays[{i}]");
            }
        }

        var boxIds = queries.Boxes.Select(item => item.Id).ToHashSet(StringComparer.Ordinal);
        for (var i = 0; i < truth.Boxes.Count; i++)
        {
            if (!boxIds.Remove(truth.Boxes[i].QueryId))
            {
                return Result<GroundTruthManifest>.Fail(
                    ErrorCode.E_SCHEMA,
                    $"Ground truth references unknown box query '{truth.Boxes[i].QueryId}'.",
                    $"$.boxes[{i}].queryId");
            }

            foreach (var nodeId in truth.Boxes[i].NodeIds)
            {
                if (!scene.Nodes.Any(node => node.Id == nodeId))
                {
                    return Result<GroundTruthManifest>.Fail(
                        ErrorCode.E_REF,
                        $"Box truth references unknown node '{nodeId}'.",
                        $"$.boxes[{i}].nodeIds");
                }
            }
        }

        return Result<GroundTruthManifest>.Ok(truth);
    }

    public static DomainError? ValidateGeometryDefinition(
        GeometryDefinition geometry,
        SceneState scene,
        string path)
    {
        if (string.IsNullOrWhiteSpace(geometry.Id))
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Geometry id is required.", $"{path}.id");
        }

        if (!scene.Nodes.ContainsKey(geometry.NodeId))
        {
            return new DomainError(
                ErrorCode.E_REF,
                $"Geometry owner node '{geometry.NodeId}' does not exist.",
                $"{path}.nodeId");
        }

        return ValidateGeometryShape(geometry, path);
    }

    private static DomainError? ValidateCamera(CameraDefinition camera)
    {
        if (camera.Position is not { Length: 3 } || !SpatialMath.IsFinite(SpatialMath.VectorFromArray(camera.Position)))
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Camera position must contain 3 finite numbers.", "$.camera.position");
        }

        if (camera.Target is not { Length: 3 } || !SpatialMath.IsFinite(SpatialMath.VectorFromArray(camera.Target)))
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Camera target must contain 3 finite numbers.", "$.camera.target");
        }

        if (camera.Up is not { Length: 3 } || !SpatialMath.IsFinite(SpatialMath.VectorFromArray(camera.Up)))
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Camera up vector must contain 3 finite numbers.", "$.camera.up");
        }

        if (!float.IsFinite(camera.FieldOfViewYDegrees) || camera.FieldOfViewYDegrees <= 0 || camera.FieldOfViewYDegrees >= 180)
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Camera field of view is invalid.", "$.camera.fieldOfViewYDegrees");
        }

        if (!float.IsFinite(camera.NearPlane) || camera.NearPlane <= 0 ||
            !float.IsFinite(camera.FarPlane) || camera.FarPlane <= camera.NearPlane)
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Camera near/far planes are invalid.", "$.camera.nearPlane");
        }

        if (camera.ViewportWidth <= 0 || camera.ViewportHeight <= 0)
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Camera viewport must be positive.", "$.camera.viewportWidth");
        }

        return null;
    }

    private static DomainError? ValidateGeometryShape(GeometryDefinition geometry, string path)
    {
        switch (geometry.GeometryKind)
        {
            case GeometryKind.Aabb:
                if (geometry.BoundsMin is not { Length: 3 } || geometry.BoundsMax is not { Length: 3 })
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "AABB bounds must contain 3 numbers each.", $"{path}.boundsMin");
                }

                if (!SpatialMath.IsFinite(SpatialMath.VectorFromArray(geometry.BoundsMin)) ||
                    !SpatialMath.IsFinite(SpatialMath.VectorFromArray(geometry.BoundsMax)))
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "AABB bounds must be finite.", $"{path}.boundsMin");
                }

                var min = SpatialMath.VectorFromArray(geometry.BoundsMin);
                var max = SpatialMath.VectorFromArray(geometry.BoundsMax);
                if (min.X > max.X || min.Y > max.Y || min.Z > max.Z)
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "AABB minimum must not exceed maximum.", $"{path}.boundsMin");
                }

                return null;

            case GeometryKind.Sphere:
                if (geometry.Center is not { Length: 3 } || !SpatialMath.IsFinite(SpatialMath.VectorFromArray(geometry.Center)))
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "Sphere center must contain 3 finite numbers.", $"{path}.center");
                }

                if (geometry.Radius is null || !float.IsFinite(geometry.Radius.Value) || geometry.Radius.Value <= 0)
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "Sphere radius must be positive and finite.", $"{path}.radius");
                }

                return null;

            case GeometryKind.TriangleMesh:
                if (geometry.Vertices is null || geometry.Vertices.Length < 3)
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "Triangle mesh requires at least 3 vertices.", $"{path}.vertices");
                }

                for (var i = 0; i < geometry.Vertices.Length; i++)
                {
                    if (geometry.Vertices[i] is not { Length: 3 } ||
                        !SpatialMath.IsFinite(SpatialMath.VectorFromArray(geometry.Vertices[i])))
                    {
                        return new DomainError(ErrorCode.E_SCHEMA, "Mesh vertices must be finite 3D points.", $"{path}.vertices[{i}]");
                    }
                }

                if (geometry.Indices is null || geometry.Indices.Length == 0 || geometry.Indices.Length % 3 != 0)
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "Mesh indices must contain complete triangles.", $"{path}.indices");
                }

                for (var i = 0; i < geometry.Indices.Length; i++)
                {
                    if (geometry.Indices[i] < 0 || geometry.Indices[i] >= geometry.Vertices.Length)
                    {
                        return new DomainError(
                            ErrorCode.E_REF,
                            "Mesh index is outside the vertex range.",
                            $"{path}.indices[{i}]");
                    }
                }

                return null;

            default:
                return new DomainError(ErrorCode.E_SCHEMA, $"Unknown geometry kind '{geometry.Kind}'.", $"{path}.kind");
        }
    }

    private static string? FindCycle(
        IReadOnlyList<SceneNodeDefinition> nodes,
        IReadOnlyDictionary<string, SceneNodeDefinition> byId)
    {
        var state = new Dictionary<string, int>(StringComparer.Ordinal);
        foreach (var node in nodes)
        {
            var path = new List<string>();
            var current = node;
            while (true)
            {
                if (state.TryGetValue(current.Id, out var status))
                {
                    if (status == 1)
                    {
                        var start = path.IndexOf(current.Id);
                        var cycle = start >= 0 ? path[start..] : path;
                        cycle.Add(current.Id);
                        return string.Join(" -> ", cycle);
                    }

                    break;
                }

                state[current.Id] = 1;
                path.Add(current.Id);
                if (current.ParentId is null || !byId.TryGetValue(current.ParentId, out current!))
                {
                    break;
                }
            }

            foreach (var id in path)
            {
                state[id] = 2;
            }
        }

        return null;
    }

    private static Result<SceneDocument> Fail(ErrorCode code, string message, string path) =>
        Result<SceneDocument>.Fail(code, message, path);
}
