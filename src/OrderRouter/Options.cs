using System.Text.Json;
using System.Text.Json.Serialization;

namespace OrderRouter;

public sealed class AppOptions
{
    public string Database { get; set; } = "OrdersDb298";
    public string ConnectionString { get; set; } = "";
    public string AdminConnectionString { get; set; } = "";
    public DateTime ArchiveCutoffUtc { get; set; } = DateTime.UtcNow.AddDays(-90);
    public int MaxConnections { get; set; } = 8;
    public int ReservedInteractiveConnections { get; set; } = 3;
    public int AcquireTimeoutMs { get; set; } = 2000;
    public string LedgerPath { get; set; } = "data/ledger.db";
    public int BatchSize { get; set; } = 500;
    public string ListenPrefix { get; set; } = "http://127.0.0.1:8298/";
    public bool EnableDebugHooks { get; set; }
    public int Seed { get; set; } = 298;

    private static readonly JsonSerializerOptions JsonOpts = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        ReadCommentHandling = JsonCommentHandling.Skip,
        Converters = { new JsonStringEnumConverter() }
    };

    public static AppOptions Load(string path)
    {
        var json = File.ReadAllText(path);
        var opts = JsonSerializer.Deserialize<AppOptions>(json, JsonOpts)
                   ?? throw new InvalidOperationException($"cannot parse {path}");
        if (opts.ArchiveCutoffUtc.Kind != DateTimeKind.Utc)
            opts.ArchiveCutoffUtc = DateTime.SpecifyKind(opts.ArchiveCutoffUtc, DateTimeKind.Utc);
        return opts;
    }
}

public sealed class ReplayOptions
{
    public string Url { get; set; } = "http://127.0.0.1:8298/";
    public int Concurrency { get; set; } = 16;
    public int RequestsPerType { get; set; } = 400;
    public int Seed { get; set; } = 298;
    public int SeedOrders { get; set; } = 20000;
    public int SeedItems { get; set; } = 30000;
    public string ReportPath { get; set; } = "data/replay-report.json";
    public Dictionary<string, double> Mix { get; set; } = new()
    {
        ["hot"] = 0.4, ["cold"] = 0.2, ["merged"] = 0.2, ["update"] = 0.2
    };

    public static ReplayOptions Load(string path)
    {
        var json = File.ReadAllText(path);
        return JsonSerializer.Deserialize<ReplayOptions>(json,
            new JsonSerializerOptions { PropertyNamingPolicy = JsonNamingPolicy.CamelCase })
            ?? throw new InvalidOperationException($"cannot parse {path}");
    }
}

/// <summary>Stable business error returned when the connection pool is exhausted.</summary>
public sealed class ConnPoolException : Exception
{
    public const string Code = "E_CONN";
    public ConnPoolException(string message) : base(message) { }
}
