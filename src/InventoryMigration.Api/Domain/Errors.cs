namespace InventoryMigration.Api.Domain;

public static class ErrorCodes
{
    public const string Migrating = "E_MIGRATING";
    public const string Connection = "E_CONN";
    public const string NotFound = "E_NOT_FOUND";
    public const string Stock = "E_STOCK";
    public const string Validation = "E_VALIDATION";
    public const string Rollback = "E_ROLLBACK";
    public const string BadRequest = "E_BAD_REQUEST";
}

public sealed class ServiceException : Exception
{
    public ServiceException(string code, string message, int statusCode, long? productId = null)
        : base(message)
    {
        Code = code;
        StatusCode = statusCode;
        ProductId = productId;
    }

    public string Code { get; }

    public int StatusCode { get; }

    public long? ProductId { get; }
}
