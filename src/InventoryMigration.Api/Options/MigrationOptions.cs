namespace InventoryMigration.Api.Options;

public sealed class MigrationOptions
{
    public const string SectionName = "Migration";

    public string OldConnectionString { get; set; } =
        "Host=127.0.0.1;Port=55432;Database=inventory_old;Username=postgres;Password=postgres;Maximum Pool Size=4";

    public string NewConnectionString { get; set; } =
        "Host=127.0.0.1;Port=55433;Database=inventory_new;Username=postgres;Password=postgres;Maximum Pool Size=4";

    public string LedgerPath { get; set; } = "data/migration-ledger.db";

    public int BatchSize { get; set; } = 1000;

    public int ConnectionLimitPerInstance { get; set; } = 4;

    public int ConnectionAcquireTimeoutMs { get; set; } = 750;

    public int DbCommandTimeoutSeconds { get; set; } = 15;

    public int CapturePollMs { get; set; } = 100;

    public int ShardCount { get; set; } = 16;

    public long? ProductMin { get; set; }

    public long? ProductMax { get; set; }

    public bool AutoResume { get; set; } = true;

    public bool ApplyLateCapturedChanges { get; set; } = true;
}
