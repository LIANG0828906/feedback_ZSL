using Xunit;
using OrderRouter;

namespace OrderRouter.Tests;

public static class TestEnv
{
    public static AppOptions CreateOptions(string database, string ledgerPath)
    {
        var cs = Environment.GetEnvironmentVariable("ORDERROUTER_TEST_CS")
                 ?? "Server=127.0.0.1,1433;User Id=sa;Password=Orders_Sa_2026!;Encrypt=False;TrustServerCertificate=True;Connect Timeout=5";
        return new AppOptions
        {
            Database = database,
            ConnectionString = cs + $";Initial Catalog={database}",
            AdminConnectionString = cs + ";Initial Catalog=master",
            ArchiveCutoffUtc = new DateTime(2026, 8, 21, 0, 0, 0, DateTimeKind.Utc),
            MaxConnections = 8,
            ReservedInteractiveConnections = 3,
            AcquireTimeoutMs = 1000,
            LedgerPath = ledgerPath,
            BatchSize = 200,
            Seed = 298
        };
    }
}

[CollectionDefinition("sql")]
public sealed class SqlCollection : ICollectionFixture<SqlFixture> { }

public sealed class SqlFixture : IAsyncLifetime
{
    public AppOptions Opts { get; }
    public Ledger Ledger { get; }

    public SqlFixture()
    {
        var dir = Path.Combine(Path.GetTempPath(), "orderrouter-tests");
        Directory.CreateDirectory(dir);
        Opts = TestEnv.CreateOptions("OrderRouterTestDb", Path.Combine(dir, "ledger-test.db"));
        if (File.Exists(Opts.LedgerPath)) File.Delete(Opts.LedgerPath);
        Ledger = new Ledger(Opts.LedgerPath);
    }

    public async Task InitializeAsync()
    {
        var seeder = new Seeder(Opts);
        await seeder.InitDatabaseAsync();
        await seeder.SeedAsync(2000, 3000);
    }

    public Task DisposeAsync() => Task.CompletedTask;
}
