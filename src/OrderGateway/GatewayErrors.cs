namespace OrderGateway;

/// <summary>Stable business error codes required by the change ticket.</summary>
public static class GatewayError
{
    public const string PlanMismatch = "E_PLAN_MISMATCH";
    public const string Deadlock = "E_DEADLOCK";
    public const string ConnectionTimeout = "E_CONN_TIMEOUT";
    public const string CommandTimeout = "E_COMMAND_TIMEOUT";
}

public sealed class GatewayException : Exception
{
    public string ErrorCode { get; }
    public IReadOnlyDictionary<string, object?> Context { get; }

    public GatewayException(
        string errorCode,
        string message,
        IReadOnlyDictionary<string, object?>? context = null,
        Exception? inner = null)
        : base(message, inner)
    {
        ErrorCode = errorCode;
        Context = context ?? new Dictionary<string, object?>();
    }
}
