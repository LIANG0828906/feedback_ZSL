namespace OrderGateway.Scenarios;

/// <summary>
/// Walks every page of a filter until the cursor is exhausted. Each page is a
/// fresh snapshot read, so callers needing a single point-in-time view freeze
/// updates first or compare via order-id multisets.
/// </summary>
public static class PaginationWalker
{
    public static async Task<List<long>> WalkAllOrderIdsAsync(
        OrderRepository repository,
        KeyOrder keyOrder,
        int tenantId,
        byte status,
        int pageSize,
        CancellationToken cancellationToken = default)
    {
        var ids = new List<long>();
        PageCursor? cursor = null;
        int guard = 0;

        while (true)
        {
            var query = new PageQuery
            {
                TenantId = tenantId,
                Status = status,
                PageSize = pageSize,
                Cursor = cursor,
            };

            var page = await repository.ReadPageAsync(keyOrder, query, cancellationToken).ConfigureAwait(false);
            ids.AddRange(page.Rows.Select(r => r.OrderId));

            if (!page.HasMore || page.NextCursor is null) break;
            cursor = page.NextCursor;
            if (++guard > 10_000) throw new InvalidOperationException("Pagination walk exceeded safety bound.");
        }

        return ids;
    }

    public static async Task<List<OrderRow>> WalkAllRowsAsync(
        OrderRepository repository,
        KeyOrder keyOrder,
        int tenantId,
        byte status,
        int pageSize,
        string? expectedIndexOverride = null,
        CancellationToken cancellationToken = default)
    {
        var rows = new List<OrderRow>();
        PageCursor? cursor = null;

        while (true)
        {
            var page = expectedIndexOverride is null
                ? await repository.ReadPageAsync(keyOrder, new PageQuery
                {
                    TenantId = tenantId,
                    Status = status,
                    PageSize = pageSize,
                    Cursor = cursor,
                }, cancellationToken).ConfigureAwait(false)
                : await repository.ReadPageWithExpectedIndexAsync(keyOrder, new PageQuery
            {
                TenantId = tenantId,
                Status = status,
                PageSize = pageSize,
                Cursor = cursor,
                }, expectedIndexOverride, cancellationToken).ConfigureAwait(false);

            rows.AddRange(page.Rows);
            if (!page.HasMore || page.NextCursor is null) break;
            cursor = page.NextCursor;
        }

        return rows;
    }
}
