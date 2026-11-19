using Microsoft.Data.SqlClient;
using OrderRouter;
using Xunit;

namespace OrderRouter.Tests;

[Collection("sql")]
public class RoutingTests
{
    private readonly SqlFixture _fx;
    public RoutingTests(SqlFixture fx) => _fx = fx;

    private (Router, ConnectionGate, Metrics) BuildRouter(Ledger? ledger = null)
    {
        var gate = new ConnectionGate(_fx.Opts.MaxConnections, _fx.Opts.ReservedInteractiveConnections, _fx.Opts.AcquireTimeoutMs);
        var metrics = new Metrics();
        return (new Router(_fx.Opts, gate, ledger ?? _fx.Ledger, metrics), gate, metrics);
    }

    [Fact]
    public async Task Hot_range_routes_to_hot_only()
    {
        var (router, _, _) = BuildRouter();
        var from = _fx.Opts.ArchiveCutoffUtc.AddDays(1);
        var result = await router.QueryAsync(new QueryCondition(from, from.AddDays(2), null, null, QueryClass.Interactive));
        Assert.Equal("hot", result.Route);
        Assert.Equal(new[] { "Orders" }, result.TablesAccessed);
        Assert.Equal(1, result.SplitCount);
        AssertOrderedUnique(result.Rows);
    }

    [Fact]
    public async Task Boundary_range_splits_to_both_sides_and_matches_unsplit()
    {
        var (router, _, _) = BuildRouter();
        var from = _fx.Opts.ArchiveCutoffUtc.AddDays(-2);
        var to = _fx.Opts.ArchiveCutoffUtc.AddDays(2);
        var cond = new QueryCondition(from, to, null, null, QueryClass.Interactive);
        var result = await router.QueryAsync(cond);
        Assert.Equal("merged", result.Route);
        Assert.Equal(2, result.SplitCount);
        Assert.Equal(new[] { "Orders", "OrdersArchive" }, result.TablesAccessed);
        AssertOrderedUnique(result.Rows);

        // Unsplit baseline: direct merged read over both tables.
        var baseline = await DirectMergeAsync(from, to, null, null);
        Assert.Equal(baseline.Select(r => r.OrderId), result.Rows.Select(r => r.OrderId));
        foreach (var (b, r) in baseline.Zip(result.Rows))
        {
            Assert.Equal(b.Amount, r.Amount);
            Assert.Equal(b.Currency, r.Currency);
            Assert.Equal(b.LastUpdated, r.LastUpdated);
        }
    }

    [Fact]
    public async Task Overlap_rows_resolve_to_newer_lastupdated_with_source()
    {
        // Plant an artificial overlap: same order in both tables, archive newer.
        long probeId = 9_000_001;
        // Id-range routing consults committed ledger ranges, so register the
        // probe range in a private ledger (the shared fixture ledger belongs
        // to the migration tests).
        var probeLedger = new Ledger(Path.Combine(Path.GetTempPath(), $"ledger-probe-{Guid.NewGuid():N}.db"));
        probeLedger.RecordBatch(1, 0, probeId, 1);
        var (router, _, _) = BuildRouter(probeLedger);
        var created = _fx.Opts.ArchiveCutoffUtc.AddDays(-5);
        await ExecAsync($"""
            INSERT INTO dbo.Orders VALUES ({probeId}, 'ORDPROBE0001', 1, '{created:O}', '2026-10-01T00:00:00', 100.00, 'USD', 'NEW');
            INSERT INTO dbo.OrdersArchive VALUES ({probeId}, 'ORDPROBE0001', 1, '{created:O}', '2026-11-01T00:00:00', 200.00, 'EUR', 'PAID');
            """);
        try
        {
            var result = await router.QueryAsync(new QueryCondition(null, null, probeId, probeId, QueryClass.Interactive));
            var row = Assert.Single(result.Rows);
            Assert.Equal(1, result.DedupDropped);
            Assert.Equal(200.00m, row.Amount);
            Assert.Equal("EUR", row.Currency);
            Assert.Equal("OrdersArchive", row.Source);

            // Now update via the write path: hot becomes newest and wins.
            var gate = new ConnectionGate(4, 1, 1000);
            var writer = new Writer(_fx.Opts, gate, new Metrics());
            await writer.UpdateAsync(probeId, 300.00m, "CNY");
            var after = await router.QueryAsync(new QueryCondition(null, null, probeId, probeId, QueryClass.Interactive));
            var updated = Assert.Single(after.Rows);
            Assert.Equal(300.00m, updated.Amount);
            Assert.Equal("CNY", updated.Currency);
            Assert.Equal("Orders", updated.Source);
        }
        finally
        {
            await ExecAsync($"DELETE FROM dbo.Orders WHERE OrderId = {probeId}; DELETE FROM dbo.OrdersArchive WHERE OrderId = {probeId};");
        }
    }

    private static void AssertOrderedUnique(IReadOnlyList<OrderRow> rows)
    {
        var nos = rows.Select(r => r.OrderNo).ToList();
        Assert.Equal(nos.OrderBy(n => n, StringComparer.Ordinal), nos);
        Assert.Equal(rows.Count, rows.Select(r => r.OrderId).Distinct().Count());
    }

    private async Task<List<OrderRow>> DirectMergeAsync(DateTime? from, DateTime? to, long? idFrom, long? idTo)
    {
        var sql = """
            SELECT OrderId, OrderNo, CustomerId, CreatedAt, LastUpdated, Amount, Currency, Status, Src
            FROM (
                SELECT *, ROW_NUMBER() OVER (PARTITION BY OrderId ORDER BY LastUpdated DESC, Src ASC) AS rn
                FROM (
                    SELECT *, 'Orders' AS Src FROM dbo.Orders
                    UNION ALL
                    SELECT *, 'OrdersArchive' AS Src FROM dbo.OrdersArchive
                ) AS both
                WHERE (@cf IS NULL OR CreatedAt >= @cf) AND (@ct IS NULL OR CreatedAt < @ct)
                  AND (@idf IS NULL OR OrderId >= @idf) AND (@idt IS NULL OR OrderId <= @idt)
            ) AS ranked WHERE rn = 1 ORDER BY OrderNo;
            """;
        await using var conn = new SqlConnection(_fx.Opts.ConnectionString);
        await conn.OpenAsync();
        await using var cmd = new SqlCommand(sql, conn);
        cmd.Parameters.AddWithValue("@cf", (object?)from ?? DBNull.Value);
        cmd.Parameters.AddWithValue("@ct", (object?)to ?? DBNull.Value);
        cmd.Parameters.AddWithValue("@idf", (object?)idFrom ?? DBNull.Value);
        cmd.Parameters.AddWithValue("@idt", (object?)idTo ?? DBNull.Value);
        var rows = new List<OrderRow>();
        await using var r = await cmd.ExecuteReaderAsync();
        while (await r.ReadAsync())
            rows.Add(new OrderRow(r.GetInt64(0), r.GetString(1), r.GetInt32(2),
                DateTime.SpecifyKind(r.GetDateTime(3), DateTimeKind.Utc),
                DateTime.SpecifyKind(r.GetDateTime(4), DateTimeKind.Utc),
                r.GetDecimal(5), r.GetString(6).Trim(), r.GetString(7), r.GetString(8)));
        return rows;
    }

    private async Task ExecAsync(string sql)
    {
        await using var conn = new SqlConnection(_fx.Opts.ConnectionString);
        await conn.OpenAsync();
        await using var cmd = new SqlCommand(sql, conn);
        await cmd.ExecuteNonQueryAsync();
    }
}
