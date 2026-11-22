using System.Numerics;

namespace GameEditor.Spatial;

public enum PivotOperationKind
{
    Translate,
    Rotate,
    Scale
}

public sealed class PivotOperation
{
    private PivotOperation(PivotOperationKind kind)
    {
        Kind = kind;
    }

    public PivotOperationKind Kind { get; }
    public Vector3 Translation { get; private init; }
    public Vector3 Axis { get; private init; }
    public float AngleDegrees { get; private init; }
    public float ScaleFactor { get; private init; } = 1f;

    public static PivotOperation Translate(Vector3 delta) =>
        new(PivotOperationKind.Translate) { Translation = delta };

    public static PivotOperation Rotate(Vector3 axis, float angleDegrees) =>
        new(PivotOperationKind.Rotate) { Axis = axis, AngleDegrees = angleDegrees };

    public static PivotOperation Scale(float factor) =>
        new(PivotOperationKind.Scale) { ScaleFactor = factor };

    public static Result<PivotOperation> FromEdit(SceneEdit edit)
    {
        if (edit.Kind != "pivot")
        {
            return Result<PivotOperation>.Fail(ErrorCode.E_INVALID_OP, "Edit is not a pivot operation.", edit.Id);
        }

        if (edit.Selection is null || edit.Selection.Count == 0)
        {
            return Result<PivotOperation>.Fail(
                ErrorCode.E_INVALID_OP,
                "Pivot edit requires a non-empty selection.",
                edit.Id);
        }

        return edit.Operation switch
        {
            "translate" when edit.Vector is { Length: 3 } &&
                             SpatialMath.IsFinite(SpatialMath.VectorFromArray(edit.Vector)) =>
                Result<PivotOperation>.Ok(Translate(SpatialMath.VectorFromArray(edit.Vector))),
            "rotate" when edit.Axis is { Length: 3 } &&
                          SpatialMath.IsFinite(SpatialMath.VectorFromArray(edit.Axis)) &&
                          SpatialMath.VectorFromArray(edit.Axis).LengthSquared() > SpatialMath.Epsilon * SpatialMath.Epsilon &&
                          edit.AngleDegrees is not null &&
                          float.IsFinite(edit.AngleDegrees.Value) =>
                Result<PivotOperation>.Ok(Rotate(
                    SpatialMath.VectorFromArray(edit.Axis),
                    edit.AngleDegrees.Value)),
            "scale" when edit.Factor is not null &&
                         float.IsFinite(edit.Factor.Value) &&
                         MathF.Abs(edit.Factor.Value) > SpatialMath.Epsilon =>
                Result<PivotOperation>.Ok(Scale(edit.Factor.Value)),
            _ => Result<PivotOperation>.Fail(
                ErrorCode.E_INVALID_OP,
                "Pivot edit requires matching operation payload.",
                edit.Id)
        };
    }
}

public sealed class PivotOperationSession
{
    private readonly SpatialRuntime _runtime;
    private readonly IReadOnlyList<string> _selection;
    private readonly IReadOnlyList<string> _roots;
    private readonly SceneState _scene;
    private readonly long _baseRevision;
    private readonly Vector3 _pivot;
    private readonly Dictionary<string, Matrix4x4> _baseLocal;
    private readonly Dictionary<string, Matrix4x4> _workingLocal;
    private readonly Dictionary<string, Matrix4x4> _parentWorld;
    private bool _committed;
    private bool _canceled;

    private PivotOperationSession(
        SpatialRuntime runtime,
        IReadOnlyList<string> selection,
        IReadOnlyList<string> roots,
        SceneState scene,
        RuntimeSnapshot snapshot,
        Vector3 pivot)
    {
        _runtime = runtime;
        _selection = selection;
        _roots = roots;
        _scene = scene;
        _baseRevision = snapshot.Revision;
        _pivot = pivot;
        _baseLocal = new Dictionary<string, Matrix4x4>(StringComparer.Ordinal);
        _workingLocal = new Dictionary<string, Matrix4x4>(StringComparer.Ordinal);
        _parentWorld = new Dictionary<string, Matrix4x4>(StringComparer.Ordinal);

        var nodeWorld = snapshot.Nodes.ToDictionary(item => item.NodeId, item => item.WorldTransform, StringComparer.Ordinal);
        foreach (var rootId in roots)
        {
            var node = scene.Nodes[rootId];
            _baseLocal[rootId] = node.LocalTransform;
            _workingLocal[rootId] = node.LocalTransform;
            _parentWorld[rootId] = node.ParentId is { } parentId
                ? nodeWorld[parentId]
                : Matrix4x4.Identity;
        }
    }

    public long BaseRevision => _baseRevision;
    public Vector3 Pivot => _pivot;
    public IReadOnlyList<string> Selection => _selection;
    public IReadOnlyList<string> SelectionRoots => _roots;
    public bool IsCommitted => _committed;
    public bool IsCanceled => _canceled;

    internal static Result<PivotOperationSession> Create(
        SpatialRuntime runtime,
        IReadOnlyList<string> selection,
        IReadOnlyList<string> roots,
        SceneState scene,
        RuntimeSnapshot snapshot,
        Vector3 pivot)
    {
        if (!SpatialMath.IsFinite(pivot))
        {
            return Result<PivotOperationSession>.Fail(
                ErrorCode.E_SCHEMA,
                "Pivot point must be finite.",
                "$.pivot");
        }

        return Result<PivotOperationSession>.Ok(
            new PivotOperationSession(runtime, selection, roots, scene, snapshot, pivot));
    }

    public Result<bool> Apply(PivotOperation operation)
    {
        if (_committed)
        {
            return Result<bool>.Fail(ErrorCode.E_INVALID_OP, "Committed session cannot be edited.", "$");
        }

        if (_canceled)
        {
            return Result<bool>.Fail(ErrorCode.E_INVALID_OP, "Canceled session cannot be edited.", "$");
        }

        if (operation.Kind == PivotOperationKind.Rotate)
        {
            if (!SpatialMath.IsFinite(operation.Axis) ||
                operation.Axis.LengthSquared() <= SpatialMath.Epsilon * SpatialMath.Epsilon ||
                !float.IsFinite(operation.AngleDegrees))
            {
                return Result<bool>.Fail(ErrorCode.E_INVALID_OP, "Rotation axis and angle are invalid.", "$.operation");
            }
        }
        else if (operation.Kind == PivotOperationKind.Translate)
        {
            if (!SpatialMath.IsFinite(operation.Translation))
            {
                return Result<bool>.Fail(ErrorCode.E_INVALID_OP, "Translation vector is invalid.", "$.operation");
            }
        }
        else if (!float.IsFinite(operation.ScaleFactor) || MathF.Abs(operation.ScaleFactor) <= SpatialMath.Epsilon)
        {
            return Result<bool>.Fail(ErrorCode.E_INVALID_OP, "Scale factor must be finite and non-zero.", "$.operation");
        }

        var updates = new Dictionary<string, Matrix4x4>(StringComparer.Ordinal);
        foreach (var rootId in _roots)
        {
            if (!SpatialMath.TryInvert(_parentWorld[rootId], out var inverseParent))
            {
                return Result<bool>.Fail(
                    ErrorCode.E_INVALID_OP,
                    $"Parent world transform for '{rootId}' is singular.",
                    $"$.nodes.{rootId}.parent");
            }

            var local = _workingLocal[rootId];
            Matrix4x4 pivotTransform;
            switch (operation.Kind)
            {
                case PivotOperationKind.Translate:
                {
                    var deltaParent = Vector3.TransformNormal(operation.Translation, inverseParent);
                    pivotTransform = Matrix4x4.CreateTranslation(deltaParent);
                    break;
                }
                case PivotOperationKind.Rotate:
                {
                    var worldRotation = Matrix4x4.CreateFromAxisAngle(
                        SpatialMath.SafeNormalize(operation.Axis, Vector3.UnitY),
                        operation.AngleDegrees * MathF.PI / 180f);
                    var worldPivotOperation =
                        Matrix4x4.CreateTranslation(-_pivot) *
                        worldRotation *
                        Matrix4x4.CreateTranslation(_pivot);
                    pivotTransform = _parentWorld[rootId] * worldPivotOperation * inverseParent;
                    break;
                }
                case PivotOperationKind.Scale:
                {
                    var worldPivotOperation =
                        Matrix4x4.CreateTranslation(-_pivot) *
                        Matrix4x4.CreateScale(operation.ScaleFactor) *
                        Matrix4x4.CreateTranslation(_pivot);
                    pivotTransform = _parentWorld[rootId] * worldPivotOperation * inverseParent;
                    break;
                }
                default:
                    return Result<bool>.Fail(ErrorCode.E_INVALID_OP, "Unknown pivot operation.", "$.operation");
            }

            var updated = local * pivotTransform;
            if (!SpatialMath.IsFinite(updated))
            {
                return Result<bool>.Fail(ErrorCode.E_INVALID_OP, "Pivot operation produced a non-finite transform.", "$.operation");
            }

            updates[rootId] = updated;
        }

        foreach (var pair in updates)
        {
            _workingLocal[pair.Key] = pair.Value;
        }

        return Result<bool>.Ok(true);
    }

    public IReadOnlyDictionary<string, Matrix4x4> PreviewWorldTransforms()
    {
        var result = new Dictionary<string, Matrix4x4>(StringComparer.Ordinal);
        var topo = _scene.GetTopologicalOrder();
        foreach (var nodeId in topo)
        {
            var node = _scene.Nodes[nodeId];
            var local = _workingLocal.TryGetValue(nodeId, out var working)
                ? working
                : node.LocalTransform;
            result[nodeId] = node.ParentId is { } parentId
                ? local * result[parentId]
                : local;
        }

        return result;
    }

    public IReadOnlyDictionary<string, Matrix4x4> GetWorkingLocalTransforms() =>
        new Dictionary<string, Matrix4x4>(_workingLocal, StringComparer.Ordinal);

    public Result<EditApplicationResult> Commit(CancellationToken cancellationToken = default)
    {
        if (_committed)
        {
            return Result<EditApplicationResult>.Fail(ErrorCode.E_INVALID_OP, "Session is already committed.", "$");
        }

        if (_canceled)
        {
            return Result<EditApplicationResult>.Fail(ErrorCode.E_INVALID_OP, "Session is canceled.", "$");
        }

        var result = _runtime.ApplyTransformBatch(_workingLocal, _baseRevision, cancellationToken);
        if (result.IsSuccess)
        {
            _committed = true;
        }

        return result;
    }

    public void Cancel()
    {
        if (_committed)
        {
            return;
        }

        _canceled = true;
        foreach (var pair in _baseLocal)
        {
            _workingLocal[pair.Key] = pair.Value;
        }
    }

    public IReadOnlyDictionary<string, Matrix4x4> GetBaseLocalTransforms() =>
        new Dictionary<string, Matrix4x4>(_baseLocal, StringComparer.Ordinal);
}
