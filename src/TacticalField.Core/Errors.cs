namespace TacticalField.Core;

public static class ErrorCodes
{
    public const string Ok = "OK";
    public const string FieldStale = "E_FIELD_STALE";
    public const string Busy = "E_BUSY";
    public const string BatchAborted = "E_BATCH_ABORTED";
    public const string RecoveryInvalid = "E_RECOVERY_INVALID";
    public const string InvalidArgument = "E_INVALID_ARGUMENT";
    public const string NotFound = "E_NOT_FOUND";
    public const string Unreachable = "E_UNREACHABLE";
}

