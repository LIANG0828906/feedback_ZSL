using System.Numerics;

namespace GameEditor.Spatial;

public enum EditPreparationMode
{
    Sequential,
    PrepareParallel
}

public sealed record AppliedEdit(string EditId, EditApplicationResult Result);

public static class EditSequenceRunner
{
    public static Result<IReadOnlyList<AppliedEdit>> Apply(
        SpatialRuntime runtime,
        IReadOnlyList<SceneEdit> edits,
        EditPreparationMode mode = EditPreparationMode.Sequential,
        CancellationToken cancellationToken = default)
    {
        try
        {
            return ApplyCore(runtime, edits, mode, cancellationToken);
        }
        catch (OperationCanceledException)
        {
            return Result<IReadOnlyList<AppliedEdit>>.Fail(
                ErrorCode.E_CANCELED,
                "Edit sequence was canceled.",
                "$.edits");
        }
    }

    private static Result<IReadOnlyList<AppliedEdit>> ApplyCore(
        SpatialRuntime runtime,
        IReadOnlyList<SceneEdit> edits,
        EditPreparationMode mode,
        CancellationToken cancellationToken)
    {
        var prepared = new PreparedEdit[edits.Count];
        var preparationErrors = new DomainError?[edits.Count];
        Action<int> prepare = index =>
        {
            var edit = edits[index];
            var result = Prepare(edit);
            if (result.IsSuccess)
            {
                prepared[index] = result.Value!;
            }
            else
            {
                preparationErrors[index] = result.Error;
            }
        };

        if (mode == EditPreparationMode.PrepareParallel && edits.Count > 1)
        {
            Parallel.For(
                0,
                edits.Count,
                new ParallelOptions
                {
                    MaxDegreeOfParallelism = runtime.Options.MaxDegreeOfParallelism,
                    CancellationToken = cancellationToken
                },
                prepare);
        }
        else
        {
            for (var i = 0; i < edits.Count; i++)
            {
                cancellationToken.ThrowIfCancellationRequested();
                prepare(i);
            }
        }

        var firstError = preparationErrors.FirstOrDefault(error => error is not null);
        if (firstError is not null)
        {
            return Result<IReadOnlyList<AppliedEdit>>.Fail(firstError.Code, firstError.Message, firstError.Path);
        }

        var applied = new List<AppliedEdit>(edits.Count);
        for (var i = 0; i < edits.Count; i++)
        {
            cancellationToken.ThrowIfCancellationRequested();
            var result = ApplyPrepared(runtime, prepared[i], cancellationToken);
            if (!result.IsSuccess)
            {
                return Result<IReadOnlyList<AppliedEdit>>.Fail(
                    result.Error!.Code,
                    result.Error.Message,
                    result.Error.Path);
            }

            applied.Add(new AppliedEdit(edits[i].Id, result.Value!));
        }

        return Result<IReadOnlyList<AppliedEdit>>.Ok(applied);
    }

    private static Result<PreparedEdit> Prepare(SceneEdit edit)
    {
        switch (edit.Kind)
        {
            case "transform":
                if (edit.NodeId is null || edit.LocalMatrix is not { Length: 16 })
                {
                    return Result<PreparedEdit>.Fail(
                        ErrorCode.E_SCHEMA,
                        "Transform edit requires nodeId and localMatrix.",
                        edit.Id);
                }

                var matrix = SpatialMath.MatrixFromRowMajor(edit.LocalMatrix);
                return SpatialMath.IsFinite(matrix)
                    ? Result<PreparedEdit>.Ok(new PreparedEdit(edit, matrix, null, null))
                    : Result<PreparedEdit>.Fail(ErrorCode.E_SCHEMA, "Transform is not finite.", edit.Id);

            case "addGeometry":
                return edit.Geometry is null
                    ? Result<PreparedEdit>.Fail(ErrorCode.E_SCHEMA, "Add edit requires geometry.", edit.Id)
                    : Result<PreparedEdit>.Ok(new PreparedEdit(edit, null, edit.Geometry, null));

            case "removeGeometry":
                return string.IsNullOrWhiteSpace(edit.GeometryId)
                    ? Result<PreparedEdit>.Fail(ErrorCode.E_SCHEMA, "Remove edit requires geometryId.", edit.Id)
                    : Result<PreparedEdit>.Ok(new PreparedEdit(edit, null, null, edit.GeometryId));

            case "pivot":
            {
                if (edit.Selection is null || edit.Selection.Count == 0 || edit.Pivot is not { Length: 3 })
                {
                    return Result<PreparedEdit>.Fail(
                        ErrorCode.E_SCHEMA,
                        "Pivot edit requires selection and pivot.",
                        edit.Id);
                }

                var operation = PivotOperation.FromEdit(edit);
                return operation.IsSuccess
                    ? Result<PreparedEdit>.Ok(new PreparedEdit(edit, null, null, null, operation.Value))
                    : Result<PreparedEdit>.Fail(
                        operation.Error!.Code,
                        operation.Error.Message,
                        operation.Error.Path);
            }

            default:
                return Result<PreparedEdit>.Fail(
                    ErrorCode.E_INVALID_OP,
                    $"Unsupported edit kind '{edit.Kind}'.",
                    edit.Id);
        }
    }

    private static Result<EditApplicationResult> ApplyPrepared(
        SpatialRuntime runtime,
        PreparedEdit prepared,
        CancellationToken cancellationToken)
    {
        var edit = prepared.Edit;
        switch (edit.Kind)
        {
            case "transform":
                return runtime.ApplyTransformBatch(
                    new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
                    {
                        [edit.NodeId!] = prepared.Matrix!.Value
                    },
                    cancellationToken: cancellationToken);

            case "addGeometry":
                return runtime.ApplyAddGeometry(prepared.Geometry!, cancellationToken: cancellationToken);

            case "removeGeometry":
                return runtime.ApplyRemoveGeometry(prepared.GeometryId!, cancellationToken: cancellationToken);

            case "pivot":
            {
                var session = runtime.BeginPivotSession(
                    edit.Selection!,
                    SpatialMath.VectorFromArray(edit.Pivot!));
                if (!session.IsSuccess)
                {
                    return Result<EditApplicationResult>.Fail(
                        session.Error!.Code,
                        session.Error.Message,
                        session.Error.Path);
                }

                var applied = session.Value!.Apply(prepared.PivotOperation!);
                if (!applied.IsSuccess)
                {
                    return Result<EditApplicationResult>.Fail(
                        applied.Error!.Code,
                        applied.Error.Message,
                        applied.Error.Path);
                }

                return session.Value.Commit(cancellationToken);
            }

            default:
                return Result<EditApplicationResult>.Fail(
                    ErrorCode.E_INVALID_OP,
                    $"Unsupported edit kind '{edit.Kind}'.",
                    edit.Id);
        }
    }

    private sealed record PreparedEdit(
        SceneEdit Edit,
        Matrix4x4? Matrix,
        GeometryDefinition? Geometry,
        string? GeometryId,
        PivotOperation? PivotOperation = null);
}
