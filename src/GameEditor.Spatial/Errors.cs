using System.Globalization;

namespace GameEditor.Spatial;

public enum ErrorCode
{
    None = 0,
    E_SCHEMA,
    E_CYCLE,
    E_REF,
    E_TOPO,
    E_STALE,
    E_SNAP,
    E_DIVERGE,
    E_IO,
    E_CANCELED,
    E_LIMIT,
    E_INVALID_OP,
    E_INTERNAL
}

public sealed record DomainError(ErrorCode Code, string Message, string? Path = null)
{
    public override string ToString()
    {
        var location = string.IsNullOrWhiteSpace(Path) ? string.Empty : $" at {Path}";
        return $"{Code}: {Message}{location}";
    }
}

public readonly record struct Result<T>(bool IsSuccess, T? Value, DomainError? Error)
{
    public static Result<T> Ok(T value) => new(true, value, null);
    public static Result<T> Fail(ErrorCode code, string message, string? path = null) =>
        new(false, default, new DomainError(code, message, path));
}

public static class ErrorText
{
    public static string Float(float value) => value.ToString("R", CultureInfo.InvariantCulture);
}
