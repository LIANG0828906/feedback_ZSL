using Microsoft.Data.SqlClient;
using OrderRouter;
using Xunit;

namespace OrderRouter.Tests;

[Collection("sql")]
public class MigrationTests
{
    private readonly SqlFixture _fx;
    public MigrationTests(SqlFixture fx) => _fx = fx;

    [Fact]
    public async Task Migration_commits_ranges_resumes_and_stays_idempotent()
    {
        var opts = _fx.Opts;
        var ledger = _fx.Ledger;
        var migrator = new Migrator(opts, ledger, batchSize: 200);

        // Partial run, then "crash".
        var partial = await migrator.RunAsync(maxBatches: 2);
        Assert.Equal(2, partial.BatchesCommitted);
        Assert.False(partial.Complete);
        var lastAfterPartial = ledger.LastBatch();
        Assert.NotNull(lastAfterPartial);

        // Resume: must continue after the last committed range, no overlap, no gaps.
        var rest = await migrator.RunAsync();
        Assert.True(rest.Complete);
        var batches = AllBatches(ledger);
        Assert.Equal(2 + rest.BatchesCommitted, batches.Count);
        long prev = batches[0].IdFromExclusive;
        foreach (var b in batches)
        {
            Assert.Equal(prev, b.IdFromExclusive);
            prev = b.IdToInclusive;
        }

        // Re-running after completion is a no-op.
        var again = await migrator.RunAsync();
        Assert.Equal(0, again.BatchesCommitted);

        // No cold rows left unmigrated in hot; archive holds them exactly once.
        await using var conn = new SqlConnection(opts.ConnectionString);
        await conn.OpenAsync();
        await using (var cmd = new SqlCommand("""
            SELECT
              (SELECT COUNT(*) FROM dbo.Orders WHERE CreatedAt < @cutoff),
              (SELECT COUNT(*) FROM dbo.OrdersArchive),
              (SELECT COUNT(*) FROM (SELECT OrderId FROM dbo.OrdersArchive GROUP BY OrderId HAVING COUNT(*) > 1) d);
            """, conn))
        {
            cmd.Parameters.AddWithValue("@cutoff", opts.ArchiveCutoffUtc);
            await using var r = await cmd.ExecuteReaderAsync();
            Assert.True(await r.ReadAsync());
            Assert.Equal(0L, Convert.ToInt64(r.GetValue(0)));   // hot has no unmigrated cold rows
            Assert.True(Convert.ToInt64(r.GetValue(1)) > 0);    // archive populated
            Assert.Equal(0L, Convert.ToInt64(r.GetValue(2)));   // no duplicate archive rows
        }
    }

    private static List<LedgerBatch> AllBatches(Ledger ledger) => ledger.ListBatches();
}
