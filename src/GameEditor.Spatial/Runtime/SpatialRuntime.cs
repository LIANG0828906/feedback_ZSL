using System.Numerics;

namespace GameEditor.Spatial;

public sealed class SpatialRuntime
{
    private readonly ReaderWriterLockSlim _lock = new(LockRecursionPolicy.NoRecursion);
    private readonly SpatialRuntimeOptions _options;
    private readonly CameraModel _camera;

    private SceneState _scene;
    private Dictionary<string, NodeDerived> _nodes = new(StringComparer.Ordinal);
    private Dictionary<string, GeometryEntry> _entries = new(StringComparer.Ordinal);
    private Dictionary<string, IndexNode> _indexNodes = new(StringComparer.Ordinal);
    private DependencyGraph _dependencies;
    private long _revision;
    private long _transformVersionClock;
    private bool _initialized;

    private SpatialRuntime(SceneState scene, SpatialRuntimeOptions options)
    {
        _scene = scene;
        _options = new SpatialRuntimeOptions
        {
            MaxDegreeOfParallelism = Math.Max(1, options.MaxDegreeOfParallelism),
            MaxQueryBatchSize = Math.Max(1, options.MaxQueryBatchSize),
            MaxCandidateBuffer = Math.Max(1, options.MaxCandidateBuffer),
            ParameterVersion = options.ParameterVersion
        };
        _camera = new CameraModel(scene.Camera);
        _dependencies = DependencyGraph.Build(scene);
    }

    public long Revision => _revision;
    public string ContentHash => _scene.ContentHash;
    public string StructureHash => _scene.StructureHash;
    public long StructureVersion => _scene.StructureVersion;
    public string ParameterVersion => _options.ParameterVersion;
    public bool IsInitialized => _initialized;
    public SpatialRuntimeOptions Options => _options;
    public CameraModel Camera => _camera;

    public static Result<SpatialRuntime> Create(SceneDocument scene, SpatialRuntimeOptions? options = null)
    {
        var validation = SceneValidator.ValidateScene(scene);
        if (!validation.IsSuccess)
        {
            return Result<SpatialRuntime>.Fail(
                validation.Error!.Code,
                validation.Error.Message,
                validation.Error.Path);
        }

        var runtime = new SpatialRuntime(SceneState.FromDocument(scene), options ?? new SpatialRuntimeOptions());
        var build = runtime.FullRebuild();
        return build.IsSuccess
            ? Result<SpatialRuntime>.Ok(runtime)
            : Result<SpatialRuntime>.Fail(build.Error!.Code, build.Error.Message, build.Error.Path);
    }

    public static Result<SpatialRuntime> CreateShell(
        SceneDocument scene,
        SpatialRuntimeOptions? options = null)
    {
        var validation = SceneValidator.ValidateScene(scene);
        if (!validation.IsSuccess)
        {
            return Result<SpatialRuntime>.Fail(
                validation.Error!.Code,
                validation.Error.Message,
                validation.Error.Path);
        }

        return Result<SpatialRuntime>.Ok(
            CreateUninitialized(scene, options ?? new SpatialRuntimeOptions()));
    }

    public Result<SpatialRuntime> CreateFullRebuild()
    {
        _lock.EnterReadLock();
        try
        {
            var clone = new SpatialRuntime(_scene.Clone(), _options);
            var build = clone.FullRebuild();
            return build.IsSuccess
                ? Result<SpatialRuntime>.Ok(clone)
                : Result<SpatialRuntime>.Fail(build.Error!.Code, build.Error.Message, build.Error.Path);
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<bool> FullRebuild(CancellationToken cancellationToken = default)
    {
        _lock.EnterWriteLock();
        try
        {
            return FullRebuildUnderLock(cancellationToken);
        }
        finally
        {
            _lock.ExitWriteLock();
        }
    }

    public RuntimeSnapshot CaptureSnapshot()
    {
        _lock.EnterReadLock();
        try
        {
            EnsureInitialized();
            return CaptureSnapshotUnderLock();
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<bool> ValidateDerivedState()
    {
        _lock.EnterReadLock();
        try
        {
            return ValidateDerivedStateUnderLock();
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<RayHit?> Raycast(RayQueryDefinition query, CancellationToken cancellationToken = default)
    {
        var all = RaycastAll(query, cancellationToken);
        if (!all.IsSuccess)
        {
            return Result<RayHit?>.Fail(all.Error!.Code, all.Error.Message, all.Error.Path);
        }

        return Result<RayHit?>.Ok(all.Value!.Count == 0 ? null : all.Value[0]);
    }

    public Result<IReadOnlyList<RayHit>> RaycastAll(
        RayQueryDefinition query,
        CancellationToken cancellationToken = default)
    {
        _lock.EnterReadLock();
        try
        {
            var freshness = ValidateDerivedStateUnderLock();
            if (!freshness.IsSuccess)
            {
                return Result<IReadOnlyList<RayHit>>.Fail(
                    freshness.Error!.Code,
                    freshness.Error.Message,
                    freshness.Error.Path);
            }

            cancellationToken.ThrowIfCancellationRequested();
            var ray = _camera.ScreenToWorldRay(query.ScreenX, query.ScreenY);
            var hits = new List<RayHit>();
            var stack = new Stack<string>(_scene.Roots.AsEnumerable().Reverse());
            while (stack.Count > 0)
            {
                cancellationToken.ThrowIfCancellationRequested();
                var nodeId = stack.Pop();
                var indexNode = _indexNodes[nodeId];
                if (!indexNode.IsReady || indexNode.AggregateBounds.IsEmpty ||
                    !GeometryIntersections.IntersectAabb(ray.Origin, ray.Direction, indexNode.AggregateBounds, out _))
                {
                    continue;
                }

                foreach (var geometry in GeometriesForNode(nodeId))
                {
                    var entry = _entries[geometry.Id];
                    if (!entry.IsReady)
                    {
                        continue;
                    }

                    var node = _nodes[nodeId];
                    if (GeometryIntersections.TryIntersect(geometry, node.WorldTransform, ray, out var hit))
                    {
                        hits.Add(new RayHit(hit.NodeId, hit.GeometryId, hit.Distance, hit.Point));
                    }
                }

                if (_scene.Children.TryGetValue(nodeId, out var children))
                {
                    for (var i = children.Count - 1; i >= 0; i--)
                    {
                        stack.Push(children[i]);
                    }
                }
            }

            hits.Sort(CompareHits);
            if (hits.Count > _options.MaxCandidateBuffer)
            {
                return Result<IReadOnlyList<RayHit>>.Fail(
                    ErrorCode.E_LIMIT,
                    "Ray candidate buffer limit exceeded.",
                    query.Id);
            }

            return Result<IReadOnlyList<RayHit>>.Ok(hits);
        }
        catch (OperationCanceledException)
        {
            return Result<IReadOnlyList<RayHit>>.Fail(ErrorCode.E_CANCELED, "Ray query was canceled.", query.Id);
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<IReadOnlyList<string>> SelectBox(
        BoxQueryDefinition query,
        CancellationToken cancellationToken = default)
    {
        _lock.EnterReadLock();
        try
        {
            var freshness = ValidateDerivedStateUnderLock();
            if (!freshness.IsSuccess)
            {
                return Result<IReadOnlyList<string>>.Fail(
                    freshness.Error!.Code,
                    freshness.Error.Message,
                    freshness.Error.Path);
            }

            cancellationToken.ThrowIfCancellationRequested();
            var rect = new ScreenRect(query.Rect[0], query.Rect[1], query.Rect[2], query.Rect[3]);
            var selected = new HashSet<string>(StringComparer.Ordinal);
            var stack = new Stack<string>(_scene.Roots.AsEnumerable().Reverse());
            while (stack.Count > 0)
            {
                cancellationToken.ThrowIfCancellationRequested();
                var nodeId = stack.Pop();
                var indexNode = _indexNodes[nodeId];
                if (!indexNode.IsReady || indexNode.AggregateBounds.IsEmpty)
                {
                    continue;
                }

                var projectedNode = _camera.ProjectBounds(indexNode.AggregateBounds);
                if (!projectedNode.IsValid || !projectedNode.Intersects(rect))
                {
                    continue;
                }

                foreach (var geometry in GeometriesForNode(nodeId))
                {
                    var entry = _entries[geometry.Id];
                    if (!entry.IsReady)
                    {
                        continue;
                    }

                    var projected = _camera.ProjectBounds(entry.WorldBounds);
                    if (projected.IsValid && projected.Intersects(rect))
                    {
                        selected.Add(nodeId);
                    }
                }

                if (_scene.Children.TryGetValue(nodeId, out var children))
                {
                    for (var i = children.Count - 1; i >= 0; i--)
                    {
                        stack.Push(children[i]);
                    }
                }
            }

            return Result<IReadOnlyList<string>>.Ok(
                selected.OrderBy(item => item, StringComparer.Ordinal).ToList());
        }
        catch (OperationCanceledException)
        {
            return Result<IReadOnlyList<string>>.Fail(ErrorCode.E_CANCELED, "Box query was canceled.", query.Id);
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<FrustumSelection> SelectFrustum(CancellationToken cancellationToken = default)
    {
        _lock.EnterReadLock();
        try
        {
            var freshness = ValidateDerivedStateUnderLock();
            if (!freshness.IsSuccess)
            {
                return Result<FrustumSelection>.Fail(
                    freshness.Error!.Code,
                    freshness.Error.Message,
                    freshness.Error.Path);
            }

            var selected = new HashSet<string>(StringComparer.Ordinal);
            var stack = new Stack<string>(_scene.Roots.AsEnumerable().Reverse());
            while (stack.Count > 0)
            {
                cancellationToken.ThrowIfCancellationRequested();
                var nodeId = stack.Pop();
                var indexNode = _indexNodes[nodeId];
                if (!indexNode.IsReady || indexNode.AggregateBounds.IsEmpty ||
                    !_camera.FrustumIntersects(indexNode.AggregateBounds))
                {
                    continue;
                }

                foreach (var geometry in GeometriesForNode(nodeId))
                {
                    var entry = _entries[geometry.Id];
                    if (entry.IsReady && _camera.FrustumIntersects(entry.WorldBounds))
                    {
                        selected.Add(nodeId);
                    }
                }

                if (_scene.Children.TryGetValue(nodeId, out var children))
                {
                    for (var i = children.Count - 1; i >= 0; i--)
                    {
                        stack.Push(children[i]);
                    }
                }
            }

            return Result<FrustumSelection>.Ok(new FrustumSelection(
                selected.OrderBy(item => item, StringComparer.Ordinal).ToList()));
        }
        catch (OperationCanceledException)
        {
            return Result<FrustumSelection>.Fail(ErrorCode.E_CANCELED, "Frustum query was canceled.", null);
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<IReadOnlyList<RayHit?>> RaycastBatch(
        IReadOnlyList<RayQueryDefinition> queries,
        bool parallel,
        CancellationToken cancellationToken = default)
    {
        if (queries.Count > _options.MaxQueryBatchSize)
        {
            return Result<IReadOnlyList<RayHit?>>.Fail(
                ErrorCode.E_LIMIT,
                "Query batch exceeds configured limit.",
                "$.rays");
        }

        var results = new RayHit?[queries.Count];
        var errors = new DomainError?[queries.Count];
        Action<int> execute = index =>
        {
            var result = Raycast(queries[index], cancellationToken);
            if (result.IsSuccess)
            {
                results[index] = result.Value;
            }
            else
            {
                errors[index] = result.Error;
            }
        };

        try
        {
            if (parallel && queries.Count > 1)
            {
                Parallel.For(
                    0,
                    queries.Count,
                    new ParallelOptions
                    {
                        MaxDegreeOfParallelism = _options.MaxDegreeOfParallelism,
                        CancellationToken = cancellationToken
                    },
                    execute);
            }
            else
            {
                for (var i = 0; i < queries.Count; i++)
                {
                    cancellationToken.ThrowIfCancellationRequested();
                    execute(i);
                }
            }
        }
        catch (OperationCanceledException)
        {
            return Result<IReadOnlyList<RayHit?>>.Fail(ErrorCode.E_CANCELED, "Ray batch was canceled.", "$.rays");
        }

        var firstError = errors.FirstOrDefault(error => error is not null);
        return firstError is not null
            ? Result<IReadOnlyList<RayHit?>>.Fail(firstError.Code, firstError.Message, firstError.Path)
            : Result<IReadOnlyList<RayHit?>>.Ok(results);
    }

    public Result<IReadOnlyList<IReadOnlyList<string>>> SelectBoxBatch(
        IReadOnlyList<BoxQueryDefinition> queries,
        bool parallel,
        CancellationToken cancellationToken = default)
    {
        if (queries.Count > _options.MaxQueryBatchSize)
        {
            return Result<IReadOnlyList<IReadOnlyList<string>>>.Fail(
                ErrorCode.E_LIMIT,
                "Query batch exceeds configured limit.",
                "$.boxes");
        }

        var results = new IReadOnlyList<string>[queries.Count];
        var errors = new DomainError?[queries.Count];
        Action<int> execute = index =>
        {
            var result = SelectBox(queries[index], cancellationToken);
            if (result.IsSuccess)
            {
                results[index] = result.Value!;
            }
            else
            {
                errors[index] = result.Error;
            }
        };

        try
        {
            if (parallel && queries.Count > 1)
            {
                Parallel.For(
                    0,
                    queries.Count,
                    new ParallelOptions
                    {
                        MaxDegreeOfParallelism = _options.MaxDegreeOfParallelism,
                        CancellationToken = cancellationToken
                    },
                    execute);
            }
            else
            {
                for (var i = 0; i < queries.Count; i++)
                {
                    cancellationToken.ThrowIfCancellationRequested();
                    execute(i);
                }
            }
        }
        catch (OperationCanceledException)
        {
            return Result<IReadOnlyList<IReadOnlyList<string>>>.Fail(
                ErrorCode.E_CANCELED,
                "Box batch was canceled.",
                "$.boxes");
        }

        var firstError = errors.FirstOrDefault(error => error is not null);
        return firstError is not null
            ? Result<IReadOnlyList<IReadOnlyList<string>>>.Fail(firstError.Code, firstError.Message, firstError.Path)
            : Result<IReadOnlyList<IReadOnlyList<string>>>.Ok(results);
    }

    public Result<EditApplicationResult> ApplyTransformBatch(
        IReadOnlyDictionary<string, Matrix4x4> transforms,
        long? expectedRevision = null,
        CancellationToken cancellationToken = default)
    {
        if (transforms.Count == 0)
        {
            return Result<EditApplicationResult>.Fail(ErrorCode.E_INVALID_OP, "Transform batch is empty.", "$.transforms");
        }

        _lock.EnterWriteLock();
        try
        {
            if (!_initialized)
            {
                return Result<EditApplicationResult>.Fail(ErrorCode.E_STALE, "Runtime is not initialized.", "$");
            }

            if (expectedRevision is not null && expectedRevision.Value != _revision)
            {
                return Result<EditApplicationResult>.Fail(
                    ErrorCode.E_STALE,
                    $"Expected revision {expectedRevision.Value}, current revision is {_revision}.",
                    "$.revision");
            }

            foreach (var pair in transforms)
            {
                if (!_scene.Nodes.ContainsKey(pair.Key))
                {
                    return Result<EditApplicationResult>.Fail(
                        ErrorCode.E_REF,
                        $"Node '{pair.Key}' does not exist.",
                        $"$.nodes.{pair.Key}");
                }

                if (!SpatialMath.IsFinite(pair.Value))
                {
                    return Result<EditApplicationResult>.Fail(
                        ErrorCode.E_SCHEMA,
                        "Transform contains a non-finite value.",
                        $"$.transforms.{pair.Key}");
                }
            }

            cancellationToken.ThrowIfCancellationRequested();
            return ApplyTransformBatchUnderLock(transforms, cancellationToken);
        }
        catch (OperationCanceledException)
        {
            return Result<EditApplicationResult>.Fail(ErrorCode.E_CANCELED, "Transform batch was canceled.", "$");
        }
        finally
        {
            _lock.ExitWriteLock();
        }
    }

    public Result<EditApplicationResult> ApplyAddGeometry(
        GeometryDefinition geometry,
        long? expectedRevision = null,
        CancellationToken cancellationToken = default)
    {
        _lock.EnterWriteLock();
        try
        {
            if (!_initialized)
            {
                return Result<EditApplicationResult>.Fail(ErrorCode.E_STALE, "Runtime is not initialized.", "$");
            }

            if (expectedRevision is not null && expectedRevision.Value != _revision)
            {
                return Result<EditApplicationResult>.Fail(
                    ErrorCode.E_STALE,
                    $"Expected revision {expectedRevision.Value}, current revision is {_revision}.",
                    "$.revision");
            }

            var storedGeometry = SceneState.CloneGeometry(geometry);
            if (_scene.Geometries.ContainsKey(storedGeometry.Id))
            {
                return Result<EditApplicationResult>.Fail(
                    ErrorCode.E_SCHEMA,
                    $"Geometry '{storedGeometry.Id}' already exists.",
                    "$.geometry.id");
            }

            var shapeError = SceneValidator.ValidateGeometryDefinition(storedGeometry, _scene, "$.geometry");
            if (shapeError is not null)
            {
                return Result<EditApplicationResult>.Fail(
                    shapeError.Code,
                    shapeError.Message,
                    shapeError.Path);
            }

            cancellationToken.ThrowIfCancellationRequested();
            return ApplyAddGeometryUnderLock(storedGeometry);
        }
        catch (OperationCanceledException)
        {
            return Result<EditApplicationResult>.Fail(ErrorCode.E_CANCELED, "Geometry add was canceled.", "$");
        }
        finally
        {
            _lock.ExitWriteLock();
        }
    }

    public Result<EditApplicationResult> ApplyRemoveGeometry(
        string geometryId,
        long? expectedRevision = null,
        CancellationToken cancellationToken = default)
    {
        _lock.EnterWriteLock();
        try
        {
            if (!_initialized)
            {
                return Result<EditApplicationResult>.Fail(ErrorCode.E_STALE, "Runtime is not initialized.", "$");
            }

            if (expectedRevision is not null && expectedRevision.Value != _revision)
            {
                return Result<EditApplicationResult>.Fail(
                    ErrorCode.E_STALE,
                    $"Expected revision {expectedRevision.Value}, current revision is {_revision}.",
                    "$.revision");
            }

            if (!_scene.Geometries.ContainsKey(geometryId))
            {
                return Result<EditApplicationResult>.Fail(
                    ErrorCode.E_REF,
                    $"Geometry '{geometryId}' does not exist.",
                    "$.geometryId");
            }

            cancellationToken.ThrowIfCancellationRequested();
            return ApplyRemoveGeometryUnderLock(geometryId);
        }
        catch (OperationCanceledException)
        {
            return Result<EditApplicationResult>.Fail(ErrorCode.E_CANCELED, "Geometry remove was canceled.", "$");
        }
        finally
        {
            _lock.ExitWriteLock();
        }
    }

    public Result<bool> Reparent(string nodeId, string? newParentId)
    {
        _lock.EnterReadLock();
        try
        {
            if (!_scene.Nodes.ContainsKey(nodeId))
            {
                return Result<bool>.Fail(ErrorCode.E_REF, $"Node '{nodeId}' does not exist.", "$.nodeId");
            }

            if (newParentId is not null && !_scene.Nodes.ContainsKey(newParentId))
            {
                return Result<bool>.Fail(ErrorCode.E_REF, $"Node '{newParentId}' does not exist.", "$.newParentId");
            }

            return Result<bool>.Fail(
                ErrorCode.E_TOPO,
                "Topology changes cannot preserve incremental cache ownership; state is unchanged.",
                $"$.nodes.{nodeId}.parentId");
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<bool> RetryFailedEntries(CancellationToken cancellationToken = default)
    {
        _lock.EnterWriteLock();
        try
        {
            var failed = _entries.Values.Where(entry => !entry.IsReady)
                .OrderBy(entry => entry.GeometryId, StringComparer.Ordinal)
                .ToList();
            if (failed.Count == 0)
            {
                return Result<bool>.Ok(false);
            }

            var version = _revision + 1;
            var newEntries = new Dictionary<string, GeometryEntry>(_entries, StringComparer.Ordinal);
            var ownerNodes = new HashSet<string>(StringComparer.Ordinal);
            foreach (var oldEntry in failed)
            {
                cancellationToken.ThrowIfCancellationRequested();
                var geometry = _scene.Geometries[oldEntry.GeometryId];
                var node = _nodes[oldEntry.NodeId];
                newEntries[oldEntry.GeometryId] = BuildEntry(geometry, node, version);
                ownerNodes.Add(oldEntry.NodeId);
            }

            var nodeBounds = ComputeNodeBounds(newEntries);
            var newNodes = new Dictionary<string, NodeDerived>(_nodes, StringComparer.Ordinal);
            foreach (var nodeId in ownerNodes)
            {
                newNodes[nodeId] = newNodes[nodeId] with
                {
                    WorldBounds = nodeBounds[nodeId],
                    BoundsVersion = version,
                    CacheVersion = version
                };
            }

            var indexAffected = new HashSet<string>(ownerNodes, StringComparer.Ordinal);
            foreach (var nodeId in ownerNodes)
            {
                foreach (var ancestor in _scene.GetAncestors(nodeId))
                {
                    indexAffected.Add(ancestor);
                }
            }

            var newIndex = new Dictionary<string, IndexNode>(_indexNodes, StringComparer.Ordinal);
            RecomputeIndexNodesInto(
                _scene.GetTopologicalOrder(),
                newIndex,
                newNodes,
                indexAffected,
                version);
            _entries = newEntries;
            _nodes = newNodes;
            _indexNodes = newIndex;
            _revision = version;
            return Result<bool>.Ok(true);
        }
        catch (OperationCanceledException)
        {
            return Result<bool>.Fail(ErrorCode.E_CANCELED, "Retry was canceled.", null);
        }
        finally
        {
            _lock.ExitWriteLock();
        }
    }

    public IReadOnlyList<GeometryDefinition> GetGeometryDefinitions()
    {
        _lock.EnterReadLock();
        try
        {
            return _scene.Geometries.Values.OrderBy(item => item.Id, StringComparer.Ordinal).ToList();
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<IReadOnlyList<RenderGeometry>> GetRenderGeometry()
    {
        _lock.EnterReadLock();
        try
        {
            var result = new List<RenderGeometry>();
            foreach (var geometry in _scene.Geometries.Values.OrderBy(item => item.Id, StringComparer.Ordinal))
            {
                var entry = _entries[geometry.Id];
                var node = _nodes[geometry.NodeId];
                result.Add(new RenderGeometry(
                    geometry.Id,
                    geometry.NodeId,
                    geometry.GeometryKind,
                    entry.LocalBounds,
                    entry.WorldBounds,
                    node.WorldTransform,
                    entry.IsReady));
            }

            return Result<IReadOnlyList<RenderGeometry>>.Ok(result);
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<IReadOnlyList<string>> GetChildren(string nodeId)
    {
        _lock.EnterReadLock();
        try
        {
            if (!_scene.Nodes.ContainsKey(nodeId))
            {
                return Result<IReadOnlyList<string>>.Fail(ErrorCode.E_REF, $"Node '{nodeId}' does not exist.", "$.nodeId");
            }

            return Result<IReadOnlyList<string>>.Ok(
                _scene.Children.TryGetValue(nodeId, out var children) ? children.ToList() : []);
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public SceneDocument ExportSceneDocument()
    {
        _lock.EnterReadLock();
        try
        {
            var nodes = _scene.Nodes.Values
                .OrderBy(item => item.Id, StringComparer.Ordinal)
                .Select(node => new SceneNodeDefinition
                {
                    Id = node.Id,
                    ParentId = node.ParentId,
                    LocalMatrix = SpatialMath.MatrixToRowMajor(node.LocalTransform)
                })
                .ToList();
            var geometries = _scene.Geometries.Values
                .OrderBy(item => item.Id, StringComparer.Ordinal)
                .Select(SceneState.CloneGeometry)
                .ToList();
            return new SceneDocument
            {
                Format = SceneFormats.Scene,
                Seed = _scene.Seed,
                Camera = SceneState.CloneCamera(_scene.Camera),
                Nodes = nodes,
                Geometries = geometries,
                Declaration = new SceneDeclaration
                {
                    NodeCount = nodes.Count,
                    GeometryCount = geometries.Count,
                    ContentHash = _scene.ContentHash,
                    StructureHash = _scene.StructureHash,
                    ParamVersion = _options.ParameterVersion
                }
            };
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<Matrix4x4> GetLocalTransform(string nodeId)
    {
        _lock.EnterReadLock();
        try
        {
            return _scene.Nodes.TryGetValue(nodeId, out var node)
                ? Result<Matrix4x4>.Ok(node.LocalTransform)
                : Result<Matrix4x4>.Fail(ErrorCode.E_REF, $"Node '{nodeId}' does not exist.", "$.nodeId");
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<IReadOnlyList<string>> GetSelectionRoots(IEnumerable<string> selection)
    {
        _lock.EnterReadLock();
        try
        {
            var selected = new HashSet<string>(selection, StringComparer.Ordinal);
            foreach (var nodeId in selected)
            {
                if (!_scene.Nodes.ContainsKey(nodeId))
                {
                    return Result<IReadOnlyList<string>>.Fail(
                        ErrorCode.E_REF,
                        $"Selected node '{nodeId}' does not exist.",
                        "$.selection");
                }
            }

            var roots = selected.Where(candidate =>
                    !_scene.GetAncestors(candidate).Any(selected.Contains))
                .OrderBy(item => item, StringComparer.Ordinal)
                .ToList();
            return Result<IReadOnlyList<string>>.Ok(roots);
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    public Result<PivotOperationSession> BeginPivotSession(
        IReadOnlyList<string> selection,
        Vector3 pivot)
    {
        _lock.EnterReadLock();
        try
        {
            var selected = new HashSet<string>(selection, StringComparer.Ordinal);
            if (selected.Count == 0)
            {
                return Result<PivotOperationSession>.Fail(
                    ErrorCode.E_INVALID_OP,
                    "Pivot session requires a non-empty selection.",
                    "$.selection");
            }

            foreach (var nodeId in selected)
            {
                if (!_scene.Nodes.ContainsKey(nodeId))
                {
                    return Result<PivotOperationSession>.Fail(
                        ErrorCode.E_REF,
                        $"Selected node '{nodeId}' does not exist.",
                        "$.selection");
                }
            }

            var roots = selected
                .Where(candidate => !_scene.GetAncestors(candidate).Any(selected.Contains))
                .OrderBy(item => item, StringComparer.Ordinal)
                .ToList();
            return PivotOperationSession.Create(
                this,
                selected.OrderBy(item => item, StringComparer.Ordinal).ToList(),
                roots,
                _scene.Clone(),
                CaptureSnapshotUnderLock(),
                pivot);
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    internal SceneState SceneState
    {
        get
        {
            _lock.EnterReadLock();
            try
            {
                return _scene.Clone();
            }
            finally
            {
                _lock.ExitReadLock();
            }
        }
    }

    internal static SpatialRuntime CreateUninitialized(SceneDocument scene, SpatialRuntimeOptions options)
    {
        return new SpatialRuntime(SceneState.FromDocument(scene), options);
    }

    internal int CaptureShellNodeCount()
    {
        _lock.EnterReadLock();
        try
        {
            return _scene.Nodes.Count;
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    internal int CaptureShellGeometryCount()
    {
        _lock.EnterReadLock();
        try
        {
            return _scene.Geometries.Count;
        }
        finally
        {
            _lock.ExitReadLock();
        }
    }

    internal Result<RuntimeSnapshot> RestoreSnapshotState(
        SnapshotPayload payload,
        Dictionary<string, NodeDerived> nodes,
        Dictionary<string, GeometryEntry> entries,
        Dictionary<string, IndexNode> indexNodes,
        long revision,
        long transformVersionClock)
    {
        _lock.EnterWriteLock();
        try
        {
            if (payload.ContentHash != _scene.ContentHash ||
                payload.StructureHash != _scene.StructureHash ||
                payload.StructureVersion != _scene.StructureVersion ||
                payload.ParameterVersion != _options.ParameterVersion)
            {
                return Result<RuntimeSnapshot>.Fail(
                    ErrorCode.E_SNAP,
                    "Snapshot metadata does not match the current scene.",
                    "$.snapshot");
            }

            var candidateNodes = nodes;
            var candidateEntries = entries;
            var candidateIndex = indexNodes;
            var oldNodes = _nodes;
            var oldEntries = _entries;
            var oldIndex = _indexNodes;
            var oldInitialized = _initialized;
            var oldRevision = _revision;
            var oldClock = _transformVersionClock;
            _nodes = candidateNodes;
            _entries = candidateEntries;
            _indexNodes = candidateIndex;
            _revision = revision;
            _transformVersionClock = transformVersionClock;
            _initialized = true;
            var validation = ValidateDerivedStateUnderLock();
            if (!validation.IsSuccess)
            {
                _nodes = oldNodes;
                _entries = oldEntries;
                _indexNodes = oldIndex;
                _initialized = oldInitialized;
                _revision = oldRevision;
                _transformVersionClock = oldClock;
                return Result<RuntimeSnapshot>.Fail(
                    ErrorCode.E_SNAP,
                    validation.Error!.Message,
                    validation.Error.Path);
            }

            return Result<RuntimeSnapshot>.Ok(CaptureSnapshotUnderLock());
        }
        finally
        {
            _lock.ExitWriteLock();
        }
    }

    private Result<bool> FullRebuildUnderLock(CancellationToken cancellationToken)
    {
        try
        {
            var topo = _scene.GetTopologicalOrder();
            var nodes = new Dictionary<string, NodeDerived>(StringComparer.Ordinal);
            var entries = new Dictionary<string, GeometryEntry>(StringComparer.Ordinal);
            var transformVersion = _transformVersionClock;
            foreach (var nodeId in topo)
            {
                cancellationToken.ThrowIfCancellationRequested();
                var node = _scene.Nodes[nodeId];
                var world = node.ParentId is { } parentId
                    ? node.LocalTransform * nodes[parentId].WorldTransform
                    : node.LocalTransform;
                if (!SpatialMath.IsFinite(world))
                {
                    return Result<bool>.Fail(
                        ErrorCode.E_SCHEMA,
                        $"World transform for node '{nodeId}' is not finite.",
                        $"$.nodes.{nodeId}");
                }

                transformVersion++;
                nodes[nodeId] = new NodeDerived(
                    nodeId,
                    world,
                    Bounds3.Empty,
                    transformVersion,
                    _revision + 1,
                    _revision + 1,
                    true,
                    null);
            }

            var geometries = _scene.Geometries.Values.OrderBy(item => item.Id, StringComparer.Ordinal).ToList();
            var builtEntries = new GeometryEntry[geometries.Count];
            Parallel.For(
                0,
                geometries.Count,
                new ParallelOptions
                {
                    MaxDegreeOfParallelism = _options.MaxDegreeOfParallelism,
                    CancellationToken = cancellationToken
                },
                index =>
                {
                    var geometry = geometries[index];
                    builtEntries[index] = BuildEntry(geometry, nodes[geometry.NodeId], _revision + 1);
                });

            for (var i = 0; i < geometries.Count; i++)
            {
                entries[geometries[i].Id] = builtEntries[i];
            }

            var nodeBounds = ComputeNodeBounds(entries);
            for (var i = 0; i < topo.Count; i++)
            {
                var nodeId = topo[i];
                nodes[nodeId] = nodes[nodeId] with
                {
                    WorldBounds = nodeBounds[nodeId],
                    BoundsVersion = _revision + 1
                };
            }

            var indexNodes = new Dictionary<string, IndexNode>(StringComparer.Ordinal);
            for (var i = topo.Count - 1; i >= 0; i--)
            {
                var nodeId = topo[i];
                var bounds = nodes[nodeId].WorldBounds;
                if (_scene.Children.TryGetValue(nodeId, out var children))
                {
                    foreach (var child in children)
                    {
                        bounds = bounds.Encapsulate(indexNodes[child].AggregateBounds);
                    }
                }

                indexNodes[nodeId] = new IndexNode(nodeId, bounds, _revision + 1, true);
            }

            _nodes = nodes;
            _entries = entries;
            _indexNodes = indexNodes;
            _dependencies = DependencyGraph.Build(_scene);
            _transformVersionClock = transformVersion;
            _revision = _revision == 0 ? 1 : _revision;
            _initialized = true;
            return Result<bool>.Ok(true);
        }
        catch (OperationCanceledException)
        {
            return Result<bool>.Fail(ErrorCode.E_CANCELED, "Full rebuild was canceled.", "$");
        }
        catch (Exception exception)
        {
            return Result<bool>.Fail(ErrorCode.E_INTERNAL, exception.Message, "$");
        }
    }

    private Result<EditApplicationResult> ApplyTransformBatchUnderLock(
        IReadOnlyDictionary<string, Matrix4x4> transforms,
        CancellationToken cancellationToken)
    {
        var changedInputs = transforms.Keys.Select(DependencyGraph.TransformInput);
        var invalidations = _dependencies.CollectInvalidations(changedInputs);
        var affectedNodes = new HashSet<string>(StringComparer.Ordinal);
        foreach (var nodeId in transforms.Keys)
        {
            affectedNodes.Add(nodeId);
            foreach (var descendant in _scene.GetDescendants(nodeId))
            {
                affectedNodes.Add(descendant);
            }
        }

        var candidateScene = _scene.WithNodeTransforms(transforms);
        var targetVersion = _revision + 1;
        var versionClock = _transformVersionClock;
        var newNodes = new Dictionary<string, NodeDerived>(_nodes, StringComparer.Ordinal);
        var topo = _scene.GetTopologicalOrder();
        foreach (var nodeId in topo)
        {
            cancellationToken.ThrowIfCancellationRequested();
            if (!affectedNodes.Contains(nodeId))
            {
                continue;
            }

            var node = candidateScene.Nodes[nodeId];
            var world = node.ParentId is { } parentId
                ? node.LocalTransform * newNodes[parentId].WorldTransform
                : node.LocalTransform;
            if (!SpatialMath.IsFinite(world))
            {
                return Result<EditApplicationResult>.Fail(
                    ErrorCode.E_SCHEMA,
                    $"World transform for node '{nodeId}' is not finite.",
                    $"$.nodes.{nodeId}");
            }

            versionClock++;
            newNodes[nodeId] = new NodeDerived(
                nodeId,
                world,
                Bounds3.Empty,
                versionClock,
                targetVersion,
                targetVersion,
                true,
                null);
        }

        var newEntries = new Dictionary<string, GeometryEntry>(_entries, StringComparer.Ordinal);
        var geometryToRebuild = _scene.Geometries.Values
            .Where(geometry => affectedNodes.Contains(geometry.NodeId))
            .OrderBy(geometry => geometry.Id, StringComparer.Ordinal)
            .ToList();
        var rebuiltEntries = new GeometryEntry[geometryToRebuild.Count];
        Parallel.For(
            0,
            geometryToRebuild.Count,
            new ParallelOptions
            {
                MaxDegreeOfParallelism = _options.MaxDegreeOfParallelism,
                CancellationToken = cancellationToken
            },
            index =>
            {
                var geometry = geometryToRebuild[index];
                rebuiltEntries[index] = BuildEntry(geometry, newNodes[geometry.NodeId], targetVersion);
            });

        for (var i = 0; i < geometryToRebuild.Count; i++)
        {
            newEntries[geometryToRebuild[i].Id] = rebuiltEntries[i];
        }

        var nodeBounds = ComputeNodeBounds(newEntries);
        foreach (var nodeId in affectedNodes)
        {
            newNodes[nodeId] = newNodes[nodeId] with
            {
                WorldBounds = nodeBounds[nodeId],
                BoundsVersion = targetVersion
            };
        }

        var indexAffected = new HashSet<string>(affectedNodes, StringComparer.Ordinal);
        foreach (var nodeId in affectedNodes)
        {
            foreach (var ancestor in _scene.GetAncestors(nodeId))
            {
                indexAffected.Add(ancestor);
            }
        }

        var newIndex = new Dictionary<string, IndexNode>(_indexNodes, StringComparer.Ordinal);
        RecomputeIndexNodesInto(topo, newIndex, newNodes, indexAffected, targetVersion);

        var stats = BuildStats(_nodes, newNodes, _entries, newEntries, _indexNodes, newIndex);
        _scene = candidateScene;
        _nodes = newNodes;
        _entries = newEntries;
        _indexNodes = newIndex;
        _revision = targetVersion;
        _transformVersionClock = versionClock;
        return Result<EditApplicationResult>.Ok(new EditApplicationResult(
            _revision,
            invalidations,
            stats,
            _scene.ContentHash,
            _scene.StructureVersion));
    }

    private Result<EditApplicationResult> ApplyAddGeometryUnderLock(GeometryDefinition geometry)
    {
        var candidateScene = _scene.WithGeometryAdded(geometry);
        var candidateGraph = _dependencies.Clone();
        candidateGraph.AddGeometryEdges(geometry);
        var invalidations = candidateGraph.CollectInvalidations([DependencyGraph.GeometryInput(geometry.Id)]);
        var targetVersion = _revision + 1;
        var newEntries = new Dictionary<string, GeometryEntry>(_entries, StringComparer.Ordinal);
        var newEntry = BuildEntry(geometry, _nodes[geometry.NodeId], targetVersion);
        newEntries[geometry.Id] = newEntry;
        var nodeBounds = ComputeNodeBounds(newEntries);
        var newNodes = new Dictionary<string, NodeDerived>(_nodes, StringComparer.Ordinal);
        newNodes[geometry.NodeId] = newNodes[geometry.NodeId] with
        {
            WorldBounds = nodeBounds[geometry.NodeId],
            BoundsVersion = targetVersion,
            CacheVersion = targetVersion
        };
        var indexAffected = new HashSet<string>(StringComparer.Ordinal) { geometry.NodeId };
        foreach (var ancestor in _scene.GetAncestors(geometry.NodeId))
        {
            indexAffected.Add(ancestor);
        }

        var newIndex = new Dictionary<string, IndexNode>(_indexNodes, StringComparer.Ordinal);
        RecomputeIndexNodesInto(_scene.GetTopologicalOrder(), newIndex, newNodes, indexAffected, targetVersion);
        var stats = BuildStats(_nodes, newNodes, _entries, newEntries, _indexNodes, newIndex);
        _scene = candidateScene;
        _nodes = newNodes;
        _entries = newEntries;
        _indexNodes = newIndex;
        _dependencies = candidateGraph;
        _revision = targetVersion;
        return Result<EditApplicationResult>.Ok(new EditApplicationResult(
            _revision,
            invalidations,
            stats,
            _scene.ContentHash,
            _scene.StructureVersion));
    }

    private Result<EditApplicationResult> ApplyRemoveGeometryUnderLock(string geometryId)
    {
        var removed = _scene.Geometries[geometryId];
        var invalidations = _dependencies.CollectInvalidations([DependencyGraph.GeometryInput(geometryId)]);
        var candidateScene = _scene.WithGeometryRemoved(geometryId);
        var targetVersion = _revision + 1;
        var newEntries = new Dictionary<string, GeometryEntry>(_entries, StringComparer.Ordinal);
        newEntries.Remove(geometryId);
        var nodeBounds = ComputeNodeBounds(newEntries);
        var newNodes = new Dictionary<string, NodeDerived>(_nodes, StringComparer.Ordinal);
        newNodes[removed.NodeId] = newNodes[removed.NodeId] with
        {
            WorldBounds = nodeBounds[removed.NodeId],
            BoundsVersion = targetVersion,
            CacheVersion = targetVersion
        };
        var indexAffected = new HashSet<string>(StringComparer.Ordinal) { removed.NodeId };
        foreach (var ancestor in _scene.GetAncestors(removed.NodeId))
        {
            indexAffected.Add(ancestor);
        }

        var newIndex = new Dictionary<string, IndexNode>(_indexNodes, StringComparer.Ordinal);
        RecomputeIndexNodesInto(_scene.GetTopologicalOrder(), newIndex, newNodes, indexAffected, targetVersion);
        var stats = BuildStats(_nodes, newNodes, _entries, newEntries, _indexNodes, newIndex);
        _scene = candidateScene;
        _nodes = newNodes;
        _entries = newEntries;
        _indexNodes = newIndex;
        var candidateGraph = _dependencies.Clone();
        candidateGraph.RemoveGeometryEdges(geometryId);
        _dependencies = candidateGraph;
        _revision = targetVersion;
        return Result<EditApplicationResult>.Ok(new EditApplicationResult(
            _revision,
            invalidations,
            stats,
            _scene.ContentHash,
            _scene.StructureVersion));
    }

    private void RecomputeIndexNodesInto(
        IReadOnlyList<string> topo,
        Dictionary<string, IndexNode> target,
        IReadOnlyDictionary<string, NodeDerived> nodes,
        HashSet<string> affected,
        long version)
    {
        for (var i = topo.Count - 1; i >= 0; i--)
        {
            var nodeId = topo[i];
            if (!affected.Contains(nodeId))
            {
                continue;
            }

            var bounds = nodes[nodeId].WorldBounds;
            if (_scene.Children.TryGetValue(nodeId, out var children))
            {
                foreach (var child in children)
                {
                    bounds = bounds.Encapsulate(target[child].AggregateBounds);
                }
            }

            target[nodeId] = new IndexNode(nodeId, bounds, version, true);
        }
    }

    private Dictionary<string, Bounds3> ComputeNodeBounds(IReadOnlyDictionary<string, GeometryEntry> entries)
    {
        var result = new Dictionary<string, Bounds3>(StringComparer.Ordinal);
        foreach (var nodeId in _scene.Nodes.Keys)
        {
            result[nodeId] = Bounds3.Empty;
        }

        foreach (var entry in entries.Values)
        {
            result[entry.NodeId] = result[entry.NodeId].Encapsulate(entry.WorldBounds);
        }

        return result;
    }

    private GeometryEntry BuildEntry(GeometryDefinition geometry, NodeDerived node, long version)
    {
        try
        {
            var localBounds = geometry.LocalBounds;
            var worldBounds = localBounds.Transform(node.WorldTransform);
            if (!SpatialMath.TryInvert(node.WorldTransform, out _))
            {
                return new GeometryEntry(
                    $"entry:{geometry.Id}",
                    geometry.Id,
                    geometry.NodeId,
                    geometry.GeometryKind,
                    localBounds,
                    worldBounds,
                    node.TransformVersion,
                    version,
                    false,
                    "Owner world transform is singular; ray intersection cannot be evaluated.");
            }

            if (!worldBounds.IsFinite)
            {
                return new GeometryEntry(
                    $"entry:{geometry.Id}",
                    geometry.Id,
                    geometry.NodeId,
                    geometry.GeometryKind,
                    localBounds,
                    worldBounds,
                    node.TransformVersion,
                    version,
                    false,
                    "World bounds are non-finite.");
            }

            return new GeometryEntry(
                $"entry:{geometry.Id}",
                geometry.Id,
                geometry.NodeId,
                geometry.GeometryKind,
                localBounds,
                worldBounds,
                node.TransformVersion,
                version,
                true,
                null);
        }
        catch (Exception exception)
        {
            return new GeometryEntry(
                $"entry:{geometry.Id}",
                geometry.Id,
                geometry.NodeId,
                geometry.GeometryKind,
                Bounds3.Empty,
                Bounds3.Empty,
                node.TransformVersion,
                version,
                false,
                exception.Message);
        }
    }

    private Result<bool> ValidateDerivedStateUnderLock()
    {
        if (!_initialized)
        {
            return Result<bool>.Fail(ErrorCode.E_STALE, "Runtime is not initialized.", "$");
        }

        if (_nodes.Count != _scene.Nodes.Count)
        {
            return Result<bool>.Fail(ErrorCode.E_STALE, "Derived node cache count is stale.", "$.nodes");
        }

        foreach (var node in _scene.Nodes.Values)
        {
            if (!_nodes.TryGetValue(node.Id, out var derived))
            {
                return Result<bool>.Fail(ErrorCode.E_STALE, $"Missing derived cache for node '{node.Id}'.", $"$.nodes.{node.Id}");
            }

            if (!derived.IsReady || !SpatialMath.IsFinite(derived.WorldTransform))
            {
                return Result<bool>.Fail(ErrorCode.E_STALE, $"Derived cache for node '{node.Id}' is invalid.", $"$.nodes.{node.Id}");
            }
        }

        if (_entries.Count != _scene.Geometries.Count)
        {
            return Result<bool>.Fail(ErrorCode.E_STALE, "Derived geometry entry count is stale.", "$.entries");
        }

        foreach (var geometry in _scene.Geometries.Values)
        {
            if (!_entries.TryGetValue(geometry.Id, out var entry))
            {
                return Result<bool>.Fail(
                    ErrorCode.E_STALE,
                    $"Missing index entry for geometry '{geometry.Id}'.",
                    $"$.entries.{geometry.Id}");
            }

            if (!_nodes.TryGetValue(entry.NodeId, out var node) ||
                entry.NodeTransformVersion != node.TransformVersion)
            {
                return Result<bool>.Fail(
                    ErrorCode.E_STALE,
                    $"Index entry '{entry.EntryId}' uses a stale transform version.",
                    $"$.entries.{geometry.Id}.nodeTransformVersion");
            }
        }

        foreach (var node in _scene.Nodes.Values)
        {
            if (!_indexNodes.TryGetValue(node.Id, out var indexNode) ||
                !indexNode.IsReady ||
                (!indexNode.AggregateBounds.IsEmpty && !indexNode.AggregateBounds.IsFinite))
            {
                return Result<bool>.Fail(
                    ErrorCode.E_STALE,
                    $"Index node '{node.Id}' is invalid or missing.",
                    $"$.indexNodes.{node.Id}");
            }
        }

        return Result<bool>.Ok(true);
    }

    private RuntimeSnapshot CaptureSnapshotUnderLock()
    {
        return new RuntimeSnapshot(
            _revision,
            _scene.ContentHash,
            _scene.StructureHash,
            _scene.StructureVersion,
            _options.ParameterVersion,
            _nodes.Values.OrderBy(item => item.NodeId, StringComparer.Ordinal).ToList(),
            _entries.Values.OrderBy(item => item.GeometryId, StringComparer.Ordinal).ToList(),
            _indexNodes.Values.OrderBy(item => item.NodeId, StringComparer.Ordinal).ToList(),
            _dependencies.Edges);
    }

    private static IncrementalStats BuildStats(
        IReadOnlyDictionary<string, NodeDerived> oldNodes,
        IReadOnlyDictionary<string, NodeDerived> newNodes,
        IReadOnlyDictionary<string, GeometryEntry> oldEntries,
        IReadOnlyDictionary<string, GeometryEntry> newEntries,
        IReadOnlyDictionary<string, IndexNode> oldIndex,
        IReadOnlyDictionary<string, IndexNode> newIndex)
    {
        var rebuiltNodes = newNodes.Keys.Count(id =>
            !oldNodes.TryGetValue(id, out var previous) || !ReferenceEquals(previous, newNodes[id]));
        var rebuiltEntries = newEntries.Keys.Count(id =>
            !oldEntries.TryGetValue(id, out var previous) || !ReferenceEquals(previous, newEntries[id]));
        var rebuiltIndex = newIndex.Keys.Count(id =>
            !oldIndex.TryGetValue(id, out var previous) || !ReferenceEquals(previous, newIndex[id]));
        return new IncrementalStats
        {
            TotalNodes = newNodes.Count,
            RebuiltNodes = rebuiltNodes,
            ReusedNodes = newNodes.Count - rebuiltNodes,
            TotalEntries = newEntries.Count,
            RebuiltEntries = rebuiltEntries,
            ReusedEntries = newEntries.Count - rebuiltEntries,
            TotalIndexNodes = newIndex.Count,
            RebuiltIndexNodes = rebuiltIndex,
            ReusedIndexNodes = newIndex.Count - rebuiltIndex
        };
    }

    private IEnumerable<GeometryDefinition> GeometriesForNode(string nodeId)
    {
        return _scene.Geometries.Values
            .Where(geometry => string.Equals(geometry.NodeId, nodeId, StringComparison.Ordinal))
            .OrderBy(geometry => geometry.Id, StringComparer.Ordinal);
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

    private void EnsureInitialized()
    {
        if (!_initialized)
        {
            throw new InvalidOperationException("Runtime is not initialized.");
        }
    }
}
