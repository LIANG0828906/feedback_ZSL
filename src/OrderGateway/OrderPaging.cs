namespace OrderGateway;

/// <summary>The two supported logical access orders from the change ticket.</summary>
public enum KeyOrder
{
    /// <summary>Legacy: Status + OrderId (IX_Orders_Status).</summary>
    Legacy = 0,

    /// <summary>New: TenantId + Status + UpdatedAt + OrderId (IX_Orders_TenantStatus).</summary>
    New = 1,
}

public sealed record PageQuery
{
    public required int TenantId { get; init; }
    public required byte Status { get; init; }
    public required int PageSize { get; init; }

    /// <summary>Opaque cursor from the previous page; null means the first page.</summary>
    public PageCursor? Cursor { get; init; }
}

/// <summary>Serializable cursor. Both key orders carry the same OrderId anchor.</summary>
public abstract record PageCursor;

public sealed record LegacyCursor(long LastOrderId) : PageCursor;

public sealed record NewCursor(DateTime UpdatedAt, long LastOrderId) : PageCursor;

public sealed record OrderRow(
    long OrderId,
    int TenantId,
    byte Status,
    decimal Amount,
    DateTimeOffset UpdatedAt);

public sealed record PageResult(
    IReadOnlyList<OrderRow> Rows,
    PageCursor? NextCursor,
    bool HasMore,
    string UsedIndex,
    string AccessMethod);

internal static class KeyOrderExtensions
{
    public static string AsLabel(this KeyOrder order) =>
        order == KeyOrder.Legacy ? "legacy_status_orderid" : "new_tenant_status_updated_orderid";

    public static string ExpectedIndex(this KeyOrder order, GatewayOptions options) =>
        order == KeyOrder.Legacy ? options.OldIndexName : options.NewIndexName;
}
