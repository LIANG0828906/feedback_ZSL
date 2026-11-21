namespace MeshRepair.Core.Errors;

public enum MeshErrorCode
{
    None = 0,
    E_MESH_INVALID,
    E_BOUNDARY_STALE,
    E_FILL_FAILED,
    E_SPLICE_INVALID,
    E_RECOVERY_INVALID,
    E_STALE_HANDLE,
    E_BUSY,
    E_CANCELLED,
    E_NOT_FOUND,
    E_INVALID_ARGUMENT,
    E_IO
}

public sealed record MeshError(
    MeshErrorCode Code,
    string Message,
    string? Field = null,
    string? Chunk = null,
    string? ChunkPair = null,
    long? ManifestVersion = null,
    string? LoopId = null,
    string? EdgeId = null,
    string? Snapshot = null)
{
    public string BusinessCode => Code.ToString();

    public override string ToString()
    {
        var location = Field is not null ? $" field={Field}" :
            ChunkPair is not null ? $" pair={ChunkPair}" :
            Chunk is not null ? $" chunk={Chunk}" : string.Empty;
        return $"{BusinessCode}: {Message}{location}";
    }
}

public sealed class MeshBusinessException : Exception
{
    public MeshBusinessException(MeshError error)
        : base(error.ToString())
    {
        Error = error;
    }

    public MeshError Error { get; }
}

public sealed class MeshResult<T>
{
    private MeshResult(T? value, MeshError? error)
    {
        Value = value;
        Error = error;
    }

    public bool IsSuccess => Error is null;

    public T? Value { get; }

    public MeshError? Error { get; }

    public T RequireValue()
    {
        return IsSuccess
            ? Value!
            : throw new InvalidOperationException(Error!.ToString());
    }

    public static MeshResult<T> Ok(T value) => new(value, null);

    public static MeshResult<T> Fail(MeshError error) => new(default, error);

    public static MeshResult<T> Fail(
        MeshErrorCode code,
        string message,
        string? field = null,
        string? chunk = null,
        string? chunkPair = null,
        long? manifestVersion = null,
        string? loopId = null,
        string? edgeId = null,
        string? snapshot = null)
        => Fail(new MeshError(code, message, field, chunk, chunkPair, manifestVersion, loopId, edgeId, snapshot));
}

public sealed class MeshResult
{
    private MeshResult(MeshError? error)
    {
        Error = error;
    }

    public bool IsSuccess => Error is null;

    public MeshError? Error { get; }

    public static MeshResult Ok() => new(null);

    public static MeshResult Fail(MeshError error) => new(error);

    public static MeshResult Fail(
        MeshErrorCode code,
        string message,
        string? field = null,
        string? chunk = null,
        string? chunkPair = null,
        long? manifestVersion = null,
        string? loopId = null,
        string? edgeId = null,
        string? snapshot = null)
        => Fail(new MeshError(code, message, field, chunk, chunkPair, manifestVersion, loopId, edgeId, snapshot));
}
