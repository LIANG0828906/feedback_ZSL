using Microsoft.Data.SqlClient;

namespace OrderRouter;

/// <summary>
/// Deterministic fixed-seed data generator. Produces orders spread across
/// [cutoff-30d, cutoff+90d) so roughly a quarter falls into the cold region,
/// plus order line items. Defaults: 20k orders / ~30k items for functional
/// verification; scale to 100k/200k for the full performance profile.
/// </summary>
public sealed class Seeder
{
    private static readonly string[] Currencies = { "USD", "EUR", "CNY", "JPY" };
    private static readonly string[] Statuses = { "NEW", "PAID", "SHIPPED", "CLOSED" };

    private readonly AppOptions _opts;
    public Seeder(AppOptions opts) { _opts = opts; }

    public async Task InitDatabaseAsync(CancellationToken ct = default)
    {
        await using (var admin = new SqlConnection(_opts.AdminConnectionString))
        {
            await admin.OpenAsync(ct).ConfigureAwait(false);
            await using var cmd = admin.CreateCommand();
            cmd.CommandTimeout = 180;
            cmd.CommandText = $"""
                IF DB_ID(N'{_opts.Database}') IS NOT NULL
                BEGIN
                    ALTER DATABASE [{_opts.Database}] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
                    DROP DATABASE [{_opts.Database}];
                END
                CREATE DATABASE [{_opts.Database}];
                ALTER DATABASE [{_opts.Database}] SET ALLOW_SNAPSHOT_ISOLATION ON;
                ALTER DATABASE [{_opts.Database}] SET RECOVERY SIMPLE;
                """;
            await cmd.ExecuteNonQueryAsync(ct).ConfigureAwait(false);
        }
        var script = File.ReadAllText(ResolveScriptPath());
        await using var conn = new SqlConnection(_opts.ConnectionString);
        await conn.OpenAsync(ct).ConfigureAwait(false);
        await using (var cmd = new SqlCommand(script, conn))
        {
            cmd.CommandTimeout = 180;
            await cmd.ExecuteNonQueryAsync(ct).ConfigureAwait(false);
        }
    }

    public async Task<(int orders, int items)> SeedAsync(int orderCount, int itemTarget, CancellationToken ct = default)
    {
        var rng = new Random(_opts.Seed);
        var cutoff = _opts.ArchiveCutoffUtc;
        var coldStart = cutoff.AddDays(-30);
        var hotEnd = cutoff.AddDays(90);
        var spanMinutes = (hotEnd - coldStart).TotalMinutes;

        await using var conn = new SqlConnection(_opts.ConnectionString);
        await conn.OpenAsync(ct).ConfigureAwait(false);

        var ordersTable = new System.Data.DataTable();
        foreach (var (name, type) in new (string, Type)[]
        {
            ("OrderId", typeof(long)), ("OrderNo", typeof(string)), ("CustomerId", typeof(int)),
            ("CreatedAt", typeof(DateTime)), ("LastUpdated", typeof(DateTime)),
            ("Amount", typeof(decimal)), ("Currency", typeof(string)), ("Status", typeof(string))
        }) ordersTable.Columns.Add(new System.Data.DataColumn(name, type));

        var itemsTable = new System.Data.DataTable();
        foreach (var (name, type) in new (string, Type)[]
        {
            ("ItemId", typeof(long)), ("OrderId", typeof(long)), ("Sku", typeof(string)),
            ("Quantity", typeof(int)), ("Price", typeof(decimal))
        }) itemsTable.Columns.Add(new System.Data.DataColumn(name, type));

        long itemId = 0;
        int items = 0;
        for (long id = 1; id <= orderCount; id++)
        {
            var created = coldStart.AddMinutes(rng.NextDouble() * spanMinutes);
            var updated = created.AddMinutes(rng.Next(0, 60 * 24));
            var amount = Math.Round((decimal)(rng.NextDouble() * 1999 + 1), 2);
            ordersTable.Rows.Add(id, $"ORD{id:D10}", rng.Next(1, 50000), created, updated,
                amount, Currencies[rng.Next(Currencies.Length)], Statuses[rng.Next(Statuses.Length)]);

            int lineCount = items < itemTarget ? rng.Next(0, 4) : 0;
            for (int li = 0; li < lineCount && items < itemTarget; li++)
            {
                itemsTable.Rows.Add(++itemId, id, $"SKU{rng.Next(1, 9000):D5}",
                    rng.Next(1, 6), Math.Round((decimal)(rng.NextDouble() * 499 + 1), 2));
                items++;
            }
        }

        await BulkAsync(conn, "dbo.Orders", ordersTable, ct).ConfigureAwait(false);
        await BulkAsync(conn, "dbo.OrderItems", itemsTable, ct).ConfigureAwait(false);
        return (orderCount, items);
    }

    private static string ResolveScriptPath()
    {
        var dir = Directory.GetCurrentDirectory();
        for (int i = 0; i < 8; i++)
        {
            var candidate = Path.Combine(dir, "sql", "01_init.sql");
            if (File.Exists(candidate)) return candidate;
            var parent = Directory.GetParent(dir);
            if (parent is null) break;
            dir = parent.FullName;
        }
        throw new FileNotFoundException("sql/01_init.sql not found from current directory upward");
    }

    private static async Task BulkAsync(SqlConnection conn, string table, System.Data.DataTable data, CancellationToken ct)
    {
        using var bulk = new SqlBulkCopy(conn) { DestinationTableName = table, BatchSize = 5000, BulkCopyTimeout = 300 };
        foreach (System.Data.DataColumn col in data.Columns)
            bulk.ColumnMappings.Add(col.ColumnName, col.ColumnName);
        await bulk.WriteToServerAsync(data, ct).ConfigureAwait(false);
    }
}
