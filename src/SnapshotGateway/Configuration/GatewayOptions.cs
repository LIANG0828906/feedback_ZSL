namespace SnapshotGateway.Configuration;

public sealed class GatewayOptions
{
    public string DataDirectory { get; init; } = Path.Combine(Environment.CurrentDirectory, "data");
    public string ReportPath { get; init; } = Path.Combine(Environment.CurrentDirectory, "artifacts", "report.json");
    public int Port { get; init; } = 5188;
    public int MaxConnections { get; init; } = 8;
    public int DefaultPageSize { get; init; } = 25;
    public int MaxPageSize { get; init; } = 200;
    public double PerfThresholdMs { get; init; } = 200;
    public int ReplicaCount { get; init; } = 3;
    public bool SelfTest { get; init; }
    public bool Reset { get; init; }
    public bool ShowHelp { get; init; }

    public string PrimaryDatabasePath => Path.Combine(DataDirectory, "primary", "orders-primary.db");

    public string ReplicaDatabasePath(int replicaId) =>
        Path.Combine(DataDirectory, "replicas", $"replica-{replicaId}", $"orders-replica-{replicaId}.db");

    public static GatewayOptions Parse(string[] args)
    {
        var values = new Dictionary<string, string?>(StringComparer.OrdinalIgnoreCase);
        for (var i = 0; i < args.Length; i++)
        {
            var arg = args[i];
            if (!arg.StartsWith("--", StringComparison.Ordinal))
            {
                continue;
            }

            var equals = arg.IndexOf('=');
            if (equals > 2)
            {
                values[arg[2..equals]] = arg[(equals + 1)..];
                continue;
            }

            var key = arg[2..];
            if (i + 1 < args.Length && !args[i + 1].StartsWith("--", StringComparison.Ordinal))
            {
                values[key] = args[++i];
            }
            else
            {
                values[key] = null;
            }
        }

        var baseDir = FindWorkspaceRoot();
        var dataDir = GetValue(values, "data-dir")
            ?? Environment.GetEnvironmentVariable("SNAPSHOT_GATEWAY_DATA")
            ?? Path.Combine(baseDir, "data");
        var reportPath = GetValue(values, "report")
            ?? Path.Combine(baseDir, "artifacts", "report.json");

        return new GatewayOptions
        {
            DataDirectory = Path.GetFullPath(dataDir, baseDir),
            ReportPath = Path.GetFullPath(reportPath, baseDir),
            Port = ParseInt(GetValue(values, "port"), 5188),
            MaxConnections = ParseInt(GetValue(values, "max-connections"), 8),
            DefaultPageSize = ParseInt(GetValue(values, "default-page-size"), 25),
            MaxPageSize = ParseInt(GetValue(values, "max-page-size"), 200),
            PerfThresholdMs = ParseDouble(GetValue(values, "perf-threshold-ms"), 200),
            ReplicaCount = Math.Clamp(ParseInt(GetValue(values, "replica-count"), 3), 1, 5),
            SelfTest = values.ContainsKey("self-test"),
            Reset = values.ContainsKey("reset"),
            ShowHelp = values.ContainsKey("help") || values.ContainsKey("h")
        };
    }

    private static string? GetValue(Dictionary<string, string?> values, string key) =>
        values.TryGetValue(key, out var value) ? value : null;

    private static int ParseInt(string? value, int fallback) =>
        int.TryParse(value, out var parsed) ? parsed : fallback;

    private static double ParseDouble(string? value, double fallback) =>
        double.TryParse(value, System.Globalization.NumberStyles.Float,
            System.Globalization.CultureInfo.InvariantCulture, out var parsed)
            ? parsed
            : fallback;

    private static string FindWorkspaceRoot()
    {
        foreach (var start in new[] { Environment.CurrentDirectory, AppContext.BaseDirectory })
        {
            var current = new DirectoryInfo(start);
            while (current is not null)
            {
                if (current.EnumerateFiles("*.sln").Any())
                {
                    return current.FullName;
                }

                current = current.Parent;
            }
        }

        return Environment.CurrentDirectory;
    }
}
