using Microsoft.Data.SqlClient;

namespace OrderGateway;

/// <summary>
/// All tunables for the gateway. Values are environment-overridable so the
/// same binary serves every validation scenario without recompilation.
/// </summary>
public sealed record GatewayOptions
{
    /// <summary>
    /// Built from OG_CONNECTION_STRING, or from OG_SQL_SERVER / OG_DATABASE /
    /// OG_SA_PASSWORD by <see cref="FromEnvironment"/>. No credential is baked
    /// into the assembly.
    /// </summary>
    public string ConnectionString { get; init; } = string.Empty;

    public string DatabaseName { get; init; } = "OrdersDb";

    /// <summary>Hard cap on concurrently leased SQL connections.</summary>
    public int MaxConcurrentConnections { get; init; } = 6;

    /// <summary>How long a request waits for a connection slot before E_CONN_TIMEOUT.</summary>
    public TimeSpan ConnectionAcquireTimeout { get; init; } = TimeSpan.FromSeconds(4);

    /// <summary>Command timeout for ordinary single-page reads.</summary>
    public TimeSpan CommandTimeout { get; init; } = TimeSpan.FromSeconds(15);

    /// <summary>
    /// A legacy instance is considered alive only when its heartbeat row is
    /// fresher than this age. Once all are stale the old index may retire.
    /// </summary>
    public TimeSpan HeartbeatExpiry { get; init; } = TimeSpan.FromSeconds(10);

    /// <summary>Schema modification lock wait used while retiring an index.</summary>
    public TimeSpan IndexDdlLockTimeout { get; init; } = TimeSpan.FromSeconds(30);

    public string OldIndexName { get; init; } = "IX_Orders_Status";
    public string NewIndexName { get; init; } = "IX_Orders_TenantStatus";

    public static GatewayOptions FromEnvironment()
    {
        static string? Get(string key) => Environment.GetEnvironmentVariable(key);

        var configuredDatabase = Get("OG_DATABASE");
        var database = configuredDatabase is { Length: > 0 } ? configuredDatabase : "OrdersDb";
        var maxConnections = int.TryParse(Get("OG_MAX_CONNECTIONS"), out var parsedMax)
            ? parsedMax
            : 6;
        if (maxConnections is < 1 or > 64)
            throw new InvalidOperationException("OG_MAX_CONNECTIONS must be between 1 and 64.");

        SqlConnectionStringBuilder builder;
        if (Get("OG_CONNECTION_STRING") is { Length: > 0 } supplied)
        {
            builder = new SqlConnectionStringBuilder(supplied);
            if (string.IsNullOrEmpty(configuredDatabase) && builder.InitialCatalog is { Length: > 0 } catalog)
                database = catalog;
        }
        else
        {
            var password = Get("OG_SA_PASSWORD") ?? Get("SA_PASSWORD");
            if (string.IsNullOrEmpty(password))
            {
                throw new InvalidOperationException(
                    "Set OG_CONNECTION_STRING or OG_SA_PASSWORD (SA_PASSWORD is also accepted).");
            }

            builder = new SqlConnectionStringBuilder
            {
                DataSource = Get("OG_SQL_SERVER") is { Length: > 0 } server ? server : "127.0.0.1,1433",
                UserID = "sa",
                Password = password,
                Encrypt = false,
                TrustServerCertificate = true,
                Pooling = true,
                ConnectTimeout = 5,
            };
        }

        builder.InitialCatalog = database;
        builder.MaxPoolSize = maxConnections;
        builder.Pooling = true;
        var connectionString = builder.ConnectionString;

        var options = new GatewayOptions
        {
            ConnectionString = connectionString,
            DatabaseName = database,
            MaxConcurrentConnections = maxConnections,
        };

        if (double.TryParse(Get("OG_ACQUIRE_TIMEOUT_SEC"), out var acquire))
            options = options with { ConnectionAcquireTimeout = TimeSpan.FromSeconds(acquire) };
        if (double.TryParse(Get("OG_COMMAND_TIMEOUT_SEC"), out var command))
            options = options with { CommandTimeout = TimeSpan.FromSeconds(command) };
        if (double.TryParse(Get("OG_HEARTBEAT_EXPIRY_SEC"), out var expiry))
            options = options with { HeartbeatExpiry = TimeSpan.FromSeconds(expiry) };
        if (double.TryParse(Get("OG_DDL_LOCK_TIMEOUT_SEC"), out var ddl))
            options = options with { IndexDdlLockTimeout = TimeSpan.FromSeconds(ddl) };

        return options;
    }
}
