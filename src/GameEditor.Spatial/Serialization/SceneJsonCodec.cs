using System.Text.Json;

namespace GameEditor.Spatial;

public static class SceneJsonCodec
{
    public static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        PropertyNameCaseInsensitive = false,
        WriteIndented = true
    };

    public static Result<SceneDocument> LoadScene(string path)
    {
        var document = ReadJson(path);
        if (!document.IsSuccess)
        {
            return Result<SceneDocument>.Fail(document.Error!.Code, document.Error.Message, document.Error.Path);
        }

        using var json = document.Value!;
        var root = json.RootElement;
        if (!TryRequireObject(root, "$", out var rootError))
        {
            return Fail<SceneDocument>(rootError!);
        }

        foreach (var field in new[] { "format", "seed", "camera", "nodes", "geometries", "declaration" })
        {
            if (!root.TryGetProperty(field, out _))
            {
                return Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, $"Missing required field '{field}'.", $"$.{field}");
            }
        }

        var cameraValidation = ValidateCameraSchema(root.GetProperty("camera"));
        if (cameraValidation is not null)
        {
            return Fail<SceneDocument>(cameraValidation);
        }

        var nodes = root.GetProperty("nodes");
        if (nodes.ValueKind != JsonValueKind.Array)
        {
            return Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, "'nodes' must be an array.", "$.nodes");
        }

        var nodeIndex = 0;
        foreach (var node in nodes.EnumerateArray())
        {
            var nodePath = $"$.nodes[{nodeIndex}]";
            if (node.ValueKind != JsonValueKind.Object)
            {
                return Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, "Node must be an object.", nodePath);
            }

            foreach (var field in new[] { "id", "parentId", "localMatrix" })
            {
                if (!node.TryGetProperty(field, out _))
                {
                    return Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, $"Missing required field '{field}'.", $"{nodePath}.{field}");
                }
            }

            if (node.GetProperty("id").ValueKind != JsonValueKind.String)
            {
                return Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, "Node id must be a string.", $"{nodePath}.id");
            }

            if (node.GetProperty("parentId").ValueKind is not (JsonValueKind.String or JsonValueKind.Null))
            {
                return Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, "Node parentId must be a string or null.", $"{nodePath}.parentId");
            }

            var matrixError = ValidateNumberArray(node.GetProperty("localMatrix"), 16, $"{nodePath}.localMatrix");
            if (matrixError is not null)
            {
                return Fail<SceneDocument>(matrixError);
            }

            nodeIndex++;
        }

        var geometries = root.GetProperty("geometries");
        if (geometries.ValueKind != JsonValueKind.Array)
        {
            return Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, "'geometries' must be an array.", "$.geometries");
        }

        var geometryIndex = 0;
        foreach (var geometry in geometries.EnumerateArray())
        {
            var geometryError = ValidateGeometrySchema(geometry, $"$.geometries[{geometryIndex}]");
            if (geometryError is not null)
            {
                return Fail<SceneDocument>(geometryError);
            }

            geometryIndex++;
        }

        var declarationError = ValidateDeclarationSchema(root.GetProperty("declaration"));
        if (declarationError is not null)
        {
            return Fail<SceneDocument>(declarationError);
        }

        SceneDocument? scene;
        try
        {
            scene = JsonSerializer.Deserialize<SceneDocument>(root.GetRawText(), JsonOptions);
        }
        catch (Exception exception) when (exception is JsonException or InvalidOperationException or FormatException)
        {
            return Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, $"Cannot bind scene document: {exception.Message}", "$");
        }

        return scene is null
            ? Result<SceneDocument>.Fail(ErrorCode.E_SCHEMA, "Scene document is null.", "$")
            : SceneValidator.ValidateScene(scene);
    }

    public static Result<QueryManifest> LoadQueries(string path, SceneDocument scene)
    {
        var document = ReadJson(path);
        if (!document.IsSuccess)
        {
            return Result<QueryManifest>.Fail(document.Error!.Code, document.Error.Message, document.Error.Path);
        }

        using var json = document.Value!;
        var root = json.RootElement;
        if (!TryRequireObject(root, "$", out var rootError))
        {
            return Fail<QueryManifest>(rootError!);
        }

        foreach (var field in new[] { "format", "sceneContentHash", "rays", "boxes" })
        {
            if (!root.TryGetProperty(field, out _))
            {
                return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, $"Missing required field '{field}'.", $"$.{field}");
            }
        }

        var rayArray = root.GetProperty("rays");
        if (rayArray.ValueKind != JsonValueKind.Array)
        {
            return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "'rays' must be an array.", "$.rays");
        }

        var rayIndex = 0;
        foreach (var ray in rayArray.EnumerateArray())
        {
            var rayPath = $"$.rays[{rayIndex}]";
            if (ray.ValueKind != JsonValueKind.Object ||
                !ray.TryGetProperty("id", out var id) || id.ValueKind != JsonValueKind.String ||
                !ray.TryGetProperty("screenX", out _) || !ray.TryGetProperty("screenY", out _))
            {
                return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Ray query requires id, screenX and screenY.", rayPath);
            }

            rayIndex++;
        }

        var boxArray = root.GetProperty("boxes");
        if (boxArray.ValueKind != JsonValueKind.Array)
        {
            return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "'boxes' must be an array.", "$.boxes");
        }

        var boxIndex = 0;
        foreach (var box in boxArray.EnumerateArray())
        {
            var boxPath = $"$.boxes[{boxIndex}]";
            if (box.ValueKind != JsonValueKind.Object ||
                !box.TryGetProperty("id", out var id) || id.ValueKind != JsonValueKind.String ||
                !box.TryGetProperty("rect", out var rect))
            {
                return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Box query requires id and rect.", boxPath);
            }

            var rectError = ValidateNumberArray(rect, 4, $"{boxPath}.rect");
            if (rectError is not null)
            {
                return Fail<QueryManifest>(rectError);
            }

            boxIndex++;
        }

        QueryManifest? queries;
        try
        {
            queries = JsonSerializer.Deserialize<QueryManifest>(root.GetRawText(), JsonOptions);
        }
        catch (Exception exception) when (exception is JsonException or InvalidOperationException or FormatException)
        {
            return Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, $"Cannot bind query manifest: {exception.Message}", "$");
        }

        return queries is null
            ? Result<QueryManifest>.Fail(ErrorCode.E_SCHEMA, "Query manifest is null.", "$")
            : SceneValidator.ValidateQueries(queries, scene);
    }

    public static Result<GroundTruthManifest> LoadTruth(string path, QueryManifest queries, SceneDocument scene)
    {
        var document = ReadJson(path);
        if (!document.IsSuccess)
        {
            return Result<GroundTruthManifest>.Fail(document.Error!.Code, document.Error.Message, document.Error.Path);
        }

        using var json = document.Value!;
        var root = json.RootElement;
        if (!TryRequireObject(root, "$", out var rootError))
        {
            return Fail<GroundTruthManifest>(rootError!);
        }

        foreach (var field in new[] { "format", "sceneContentHash", "rays", "boxes" })
        {
            if (!root.TryGetProperty(field, out _))
            {
                return Result<GroundTruthManifest>.Fail(ErrorCode.E_SCHEMA, $"Missing required field '{field}'.", $"$.{field}");
            }
        }

        if (root.GetProperty("rays").ValueKind != JsonValueKind.Array ||
            root.GetProperty("boxes").ValueKind != JsonValueKind.Array)
        {
            return Result<GroundTruthManifest>.Fail(ErrorCode.E_SCHEMA, "Ground truth rays and boxes must be arrays.", "$");
        }

        var rayIndex = 0;
        foreach (var ray in root.GetProperty("rays").EnumerateArray())
        {
            var rayPath = $"$.rays[{rayIndex}]";
            if (ray.ValueKind != JsonValueKind.Object ||
                !ray.TryGetProperty("queryId", out var queryId) || queryId.ValueKind != JsonValueKind.String ||
                !ray.TryGetProperty("hit", out var hit) || hit.ValueKind is not (JsonValueKind.True or JsonValueKind.False) ||
                !ray.TryGetProperty("allHits", out var allHits) || allHits.ValueKind != JsonValueKind.Array)
            {
                return Result<GroundTruthManifest>.Fail(
                    ErrorCode.E_SCHEMA,
                    "Ray truth requires queryId, hit and allHits.",
                    rayPath);
            }

            if (hit.GetBoolean())
            {
                foreach (var field in new[] { "nodeId", "geometryId", "point" })
                {
                    if (!ray.TryGetProperty(field, out _))
                    {
                        return Result<GroundTruthManifest>.Fail(
                            ErrorCode.E_SCHEMA,
                            $"Hit truth requires '{field}'.",
                            $"{rayPath}.{field}");
                    }
                }

                if (!ray.TryGetProperty("distance", out var distance) || distance.ValueKind != JsonValueKind.Number)
                {
                    return Result<GroundTruthManifest>.Fail(
                        ErrorCode.E_SCHEMA,
                        "Hit truth requires numeric distance.",
                        $"{rayPath}.distance");
                }

                var pointError = ValidateNumberArray(ray.GetProperty("point"), 3, $"{rayPath}.point");
                if (pointError is not null)
                {
                    return Fail<GroundTruthManifest>(pointError);
                }
            }

            var hitIndex = 0;
            foreach (var geometryHit in allHits.EnumerateArray())
            {
                var hitPath = $"{rayPath}.allHits[{hitIndex}]";
                if (geometryHit.ValueKind != JsonValueKind.Object ||
                    !geometryHit.TryGetProperty("geometryId", out var geometryId) || geometryId.ValueKind != JsonValueKind.String ||
                    !geometryHit.TryGetProperty("nodeId", out var nodeId) || nodeId.ValueKind != JsonValueKind.String ||
                    !geometryHit.TryGetProperty("distance", out var distance) || distance.ValueKind != JsonValueKind.Number ||
                    !geometryHit.TryGetProperty("point", out var point))
                {
                    return Result<GroundTruthManifest>.Fail(
                        ErrorCode.E_SCHEMA,
                        "Geometry hit truth requires geometryId, nodeId, distance and point.",
                        hitPath);
                }

                var pointError = ValidateNumberArray(point, 3, $"{hitPath}.point");
                if (pointError is not null)
                {
                    return Fail<GroundTruthManifest>(pointError);
                }

                hitIndex++;
            }

            rayIndex++;
        }

        var boxIndex = 0;
        foreach (var box in root.GetProperty("boxes").EnumerateArray())
        {
            var boxPath = $"$.boxes[{boxIndex}]";
            if (box.ValueKind != JsonValueKind.Object ||
                !box.TryGetProperty("queryId", out var queryId) || queryId.ValueKind != JsonValueKind.String ||
                !box.TryGetProperty("nodeIds", out var nodeIds) || nodeIds.ValueKind != JsonValueKind.Array ||
                nodeIds.EnumerateArray().Any(item => item.ValueKind != JsonValueKind.String))
            {
                return Result<GroundTruthManifest>.Fail(
                    ErrorCode.E_SCHEMA,
                    "Box truth requires queryId and string nodeIds array.",
                    boxPath);
            }

            boxIndex++;
        }

        GroundTruthManifest? truth;
        try
        {
            truth = JsonSerializer.Deserialize<GroundTruthManifest>(root.GetRawText(), JsonOptions);
        }
        catch (Exception exception) when (exception is JsonException or InvalidOperationException or FormatException)
        {
            return Result<GroundTruthManifest>.Fail(ErrorCode.E_SCHEMA, $"Cannot bind ground truth: {exception.Message}", "$");
        }

        return truth is null
            ? Result<GroundTruthManifest>.Fail(ErrorCode.E_SCHEMA, "Ground truth is null.", "$")
            : SceneValidator.ValidateTruth(truth, queries, scene);
    }

    public static Result<EditSequenceDefinition> LoadEdits(string path)
    {
        var document = ReadJson(path);
        if (!document.IsSuccess)
        {
            return Result<EditSequenceDefinition>.Fail(document.Error!.Code, document.Error.Message, document.Error.Path);
        }

        using var json = document.Value!;
        var root = json.RootElement;
        if (root.ValueKind != JsonValueKind.Object ||
            !root.TryGetProperty("format", out _) ||
            !root.TryGetProperty("baseContentHash", out _) ||
            !root.TryGetProperty("edits", out var edits) ||
            edits.ValueKind != JsonValueKind.Array)
        {
            return Result<EditSequenceDefinition>.Fail(
                ErrorCode.E_SCHEMA,
                "Edit sequence requires format, baseContentHash and edits.",
                "$");
        }

        EditSequenceDefinition? sequence;
        try
        {
            sequence = JsonSerializer.Deserialize<EditSequenceDefinition>(root.GetRawText(), JsonOptions);
        }
        catch (Exception exception) when (exception is JsonException or InvalidOperationException or FormatException)
        {
            return Result<EditSequenceDefinition>.Fail(ErrorCode.E_SCHEMA, $"Cannot bind edit sequence: {exception.Message}", "$");
        }

        if (sequence is null || !string.Equals(sequence.Format, SceneFormats.Edits, StringComparison.Ordinal))
        {
            return Result<EditSequenceDefinition>.Fail(ErrorCode.E_SCHEMA, "Edit sequence format is invalid.", "$.format");
        }

        var ids = new HashSet<string>(StringComparer.Ordinal);
        for (var i = 0; i < sequence.Edits.Count; i++)
        {
            var edit = sequence.Edits[i];
            if (string.IsNullOrWhiteSpace(edit.Id) || !ids.Add(edit.Id))
            {
                return Result<EditSequenceDefinition>.Fail(
                    ErrorCode.E_SCHEMA,
                    "Edit ids must be non-empty and unique.",
                    $"$.edits[{i}].id");
            }

            if (edit.Kind is not ("transform" or "addGeometry" or "removeGeometry" or "pivot"))
            {
                return Result<EditSequenceDefinition>.Fail(
                    ErrorCode.E_SCHEMA,
                    $"Unsupported edit kind '{edit.Kind}'.",
                    $"$.edits[{i}].kind");
            }
        }

        return Result<EditSequenceDefinition>.Ok(sequence);
    }

    public static Result<EditTruthManifest> LoadEditTruth(string path)
    {
        var document = ReadJson(path);
        if (!document.IsSuccess)
        {
            return Result<EditTruthManifest>.Fail(document.Error!.Code, document.Error.Message, document.Error.Path);
        }

        using var json = document.Value!;
        try
        {
            var truth = JsonSerializer.Deserialize<EditTruthManifest>(json.RootElement.GetRawText(), JsonOptions);
            if (truth is null || truth.Format != SceneFormats.EditTruth)
            {
                return Result<EditTruthManifest>.Fail(ErrorCode.E_SCHEMA, "Edit truth format is invalid.", "$.format");
            }

            return Result<EditTruthManifest>.Ok(truth);
        }
        catch (Exception exception) when (exception is JsonException or InvalidOperationException or FormatException)
        {
            return Result<EditTruthManifest>.Fail(ErrorCode.E_SCHEMA, $"Cannot bind edit truth: {exception.Message}", "$");
        }
    }

    public static Result<bool> WriteJson<T>(string path, T value)
    {
        try
        {
            var directory = Path.GetDirectoryName(Path.GetFullPath(path));
            if (!string.IsNullOrEmpty(directory))
            {
                Directory.CreateDirectory(directory);
            }

            var json = JsonSerializer.Serialize(value, JsonOptions);
            File.WriteAllText(path, json + "\n");
            return Result<bool>.Ok(true);
        }
        catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
        {
            return Result<bool>.Fail(ErrorCode.E_IO, exception.Message, path);
        }
    }

    private static Result<JsonDocument> ReadJson(string path)
    {
        try
        {
            if (!File.Exists(path))
            {
                return Result<JsonDocument>.Fail(ErrorCode.E_IO, "File does not exist.", path);
            }

            return Result<JsonDocument>.Ok(JsonDocument.Parse(File.ReadAllText(path)));
        }
        catch (JsonException exception)
        {
            var location = exception.LineNumber is null ? "$" : $"$ (line {exception.LineNumber + 1})";
            return Result<JsonDocument>.Fail(ErrorCode.E_SCHEMA, exception.Message, location);
        }
        catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
        {
            return Result<JsonDocument>.Fail(ErrorCode.E_IO, exception.Message, path);
        }
    }

    private static DomainError? ValidateCameraSchema(JsonElement camera)
    {
        if (camera.ValueKind != JsonValueKind.Object)
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Camera must be an object.", "$.camera");
        }

        foreach (var field in new[]
                 {
                     "position", "target", "up", "fieldOfViewYDegrees", "nearPlane", "farPlane",
                     "viewportWidth", "viewportHeight"
                 })
        {
            if (!camera.TryGetProperty(field, out _))
            {
                return new DomainError(ErrorCode.E_SCHEMA, $"Missing required field '{field}'.", $"$.camera.{field}");
            }
        }

        foreach (var field in new[] { "position", "target", "up" })
        {
            var error = ValidateNumberArray(camera.GetProperty(field), 3, $"$.camera.{field}");
            if (error is not null)
            {
                return error;
            }
        }

        return null;
    }

    private static DomainError? ValidateGeometrySchema(JsonElement geometry, string path)
    {
        if (geometry.ValueKind != JsonValueKind.Object)
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Geometry must be an object.", path);
        }

        foreach (var field in new[] { "id", "nodeId", "kind" })
        {
            if (!geometry.TryGetProperty(field, out var value) || value.ValueKind != JsonValueKind.String)
            {
                return new DomainError(ErrorCode.E_SCHEMA, $"Geometry requires string field '{field}'.", $"{path}.{field}");
            }
        }

        var kind = geometry.GetProperty("kind").GetString();
        switch (kind)
        {
            case "aabb":
                if (!geometry.TryGetProperty("boundsMin", out var boundsMin) ||
                    !geometry.TryGetProperty("boundsMax", out var boundsMax))
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "AABB requires boundsMin and boundsMax.", path);
                }

                return ValidateNumberArray(boundsMin, 3, $"{path}.boundsMin") ??
                       ValidateNumberArray(boundsMax, 3, $"{path}.boundsMax");

            case "sphere":
                if (!geometry.TryGetProperty("center", out var center) ||
                    !geometry.TryGetProperty("radius", out var radius) ||
                    radius.ValueKind != JsonValueKind.Number)
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "Sphere requires center and radius.", path);
                }

                return ValidateNumberArray(center, 3, $"{path}.center");

            case "triangleMesh":
                if (!geometry.TryGetProperty("vertices", out var vertices) ||
                    vertices.ValueKind != JsonValueKind.Array ||
                    !geometry.TryGetProperty("indices", out var indices) ||
                    indices.ValueKind != JsonValueKind.Array)
                {
                    return new DomainError(ErrorCode.E_SCHEMA, "Triangle mesh requires vertices and indices arrays.", path);
                }

                var vertexIndex = 0;
                foreach (var vertex in vertices.EnumerateArray())
                {
                    var error = ValidateNumberArray(vertex, 3, $"{path}.vertices[{vertexIndex}]");
                    if (error is not null)
                    {
                        return error;
                    }

                    vertexIndex++;
                }

                var elementIndex = 0;
                foreach (var element in indices.EnumerateArray())
                {
                    if (element.ValueKind != JsonValueKind.Number || !element.TryGetInt32(out _))
                    {
                        return new DomainError(ErrorCode.E_SCHEMA, "Mesh index must be an integer.", $"{path}.indices[{elementIndex}]");
                    }

                    elementIndex++;
                }

                return null;

            default:
                return new DomainError(ErrorCode.E_SCHEMA, $"Unknown geometry kind '{kind}'.", $"{path}.kind");
        }
    }

    private static DomainError? ValidateDeclarationSchema(JsonElement declaration)
    {
        if (declaration.ValueKind != JsonValueKind.Object)
        {
            return new DomainError(ErrorCode.E_SCHEMA, "Declaration must be an object.", "$.declaration");
        }

        foreach (var field in new[] { "nodeCount", "geometryCount", "contentHash", "structureHash", "paramVersion" })
        {
            if (!declaration.TryGetProperty(field, out _))
            {
                return new DomainError(ErrorCode.E_SCHEMA, $"Missing required field '{field}'.", $"$.declaration.{field}");
            }
        }

        return null;
    }

    private static DomainError? ValidateNumberArray(JsonElement element, int length, string path)
    {
        if (element.ValueKind != JsonValueKind.Array || element.GetArrayLength() != length)
        {
            return new DomainError(ErrorCode.E_SCHEMA, $"Expected an array of {length} numbers.", path);
        }

        var index = 0;
        foreach (var item in element.EnumerateArray())
        {
            if (item.ValueKind != JsonValueKind.Number)
            {
                return new DomainError(ErrorCode.E_SCHEMA, "Expected a number.", $"{path}[{index}]");
            }

            if (!item.TryGetSingle(out var value) || !float.IsFinite(value))
            {
                return new DomainError(ErrorCode.E_SCHEMA, "Number is not finite or outside float range.", $"{path}[{index}]");
            }

            index++;
        }

        return null;
    }

    private static bool TryRequireObject(JsonElement element, string path, out DomainError? error)
    {
        if (element.ValueKind == JsonValueKind.Object)
        {
            error = null;
            return true;
        }

        error = new DomainError(ErrorCode.E_SCHEMA, "JSON root must be an object.", path);
        return false;
    }

    private static Result<T> Fail<T>(DomainError error) =>
        Result<T>.Fail(error.Code, error.Message, error.Path);
}
