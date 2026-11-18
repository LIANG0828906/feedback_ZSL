using System.Text.Json;
using Microsoft.AspNetCore.Http.Json;
using SnapshotGateway.Configuration;
using SnapshotGateway.Domain;
using SnapshotGateway.Infrastructure;
using SnapshotGateway.Services;
using SnapshotGateway.Validation;

var options = GatewayOptions.Parse(args);
if (options.ShowHelp)
{
    Console.Out.WriteLine("""
        SnapshotGateway - SQLite primary/three-replica snapshot consistency gateway

        Usage:
          dotnet run --project src/SnapshotGateway -- [options]

        Options:
          --self-test                 Reset data, run the full validation suite, write JSON report.
          --reset                     Recreate primary and replica databases on startup.
          --data-dir <path>           Data directory (default: ./data).
          --report <path>             JSON report path (default: ./artifacts/report.json).
          --port <port>               HTTP port (default: 5188).
          --max-connections <n>       SQLite concurrent connection cap (default: 8).
          --perf-threshold-ms <n>     Page P95 budget in milliseconds (default: 200).
          --help                      Show this help.

        HTTP endpoints:
          GET  /api/orders?tenantId=T001&status=NEW&keyOrder=old&page=1&pageSize=25
          POST /api/index/switch
          POST /api/index/rollback
          POST /api/orders/update?count=25
          GET  /api/status
          GET  /api/report
          POST /api/report/write
        """);
    return 0;
}

var builder = WebApplication.CreateBuilder(new WebApplicationOptions
{
    Args = Array.Empty<string>(),
    ApplicationName = typeof(OrderQueryService).Assembly.FullName,
    ContentRootPath = Environment.CurrentDirectory
});
builder.WebHost.UseUrls($"http://127.0.0.1:{options.Port}");
if (options.SelfTest)
{
    builder.Logging.ClearProviders();
}

builder.Services.Configure<JsonOptions>(json =>
{
    json.SerializerOptions.PropertyNamingPolicy = JsonNamingPolicy.CamelCase;
    json.SerializerOptions.WriteIndented = false;
});
builder.Services.AddSingleton(options);
builder.Services.AddSingleton(new ConnectionGate(options.MaxConnections));
builder.Services.AddSingleton<SqliteConnectionFactory>();
builder.Services.AddSingleton<PrimaryWriteGate>();
builder.Services.AddSingleton<GatewayMetrics>();
builder.Services.AddSingleton<ReplicaManager>();
builder.Services.AddSingleton<DatabaseInitializer>();
builder.Services.AddSingleton<SnapshotCoordinator>();
builder.Services.AddSingleton<OrderQueryService>();
builder.Services.AddSingleton<OrderWriter>();
builder.Services.AddSingleton<IndexStateService>();
builder.Services.AddSingleton<ReportService>();
builder.Services.AddSingleton<SelfTestRunner>();

var app = builder.Build();
try
{
    var initializer = app.Services.GetRequiredService<DatabaseInitializer>();
    await initializer.InitializeAsync(options.Reset || options.SelfTest);
}
catch (Exception ex)
{
    Console.Error.WriteLine($"Initialization failed: {ex}");
    return 1;
}

if (options.SelfTest)
{
    var runner = app.Services.GetRequiredService<SelfTestRunner>();
    return await runner.RunAsync();
}

app.MapGet("/", () => Results.Ok(new
{
    service = "SnapshotGateway",
    topology = "1 primary + 3 read replicas (local SQLite)",
    endpoints = new[]
    {
        "GET /api/orders?tenantId=T001&status=NEW&keyOrder=old&page=1&pageSize=25",
        "GET /api/orders?tenantId=T001&status=NEW&keyOrder=new&page=1&pageSize=25",
        "POST /api/index/switch",
        "POST /api/index/rollback",
        "POST /api/orders/update?count=25",
        "GET /api/status",
        "GET /api/replicas",
        "GET /api/audit",
        "GET /api/report",
        "POST /api/report/write"
    }
}));

app.MapGet("/health", async (
    IndexStateService indexes,
    ReplicaManager replicas,
    CancellationToken cancellationToken) =>
{
    var state = await indexes.GetStateAsync(cancellationToken);
    var versions = await replicas.ReadAllVersionsAsync(cancellationToken);
    return Results.Ok(new
    {
        status = "ok",
        snapshotVersion = state.DataSnapshotVersion,
        activeGeneration = state.ActiveGeneration,
        replicaVersions = versions
    });
});

app.MapGet("/api/orders", async (
    HttpRequest request,
    OrderQueryService queries,
    GatewayOptions gatewayOptions,
    CancellationToken cancellationToken) =>
{
    var query = request.Query;
    var keyOrder = query["keyOrder"].FirstOrDefault()
                   ?? query["orderBy"].FirstOrDefault()
                   ?? KeyOrders.Old;
    var page = ParseInt(query["page"].FirstOrDefault(), 1);
    var pageSize = ParseInt(query["pageSize"].FirstOrDefault(), gatewayOptions.DefaultPageSize);
    int? replicaId = ParseNullableInt(query["replica"].FirstOrDefault());
    var result = await queries.QueryAsync(new QueryFilter
    {
        TenantId = query["tenantId"].FirstOrDefault() ?? string.Empty,
        Status = query["status"].FirstOrDefault() ?? string.Empty,
        KeyOrder = keyOrder,
        Page = page,
        PageSize = pageSize,
        ReplicaId = replicaId
    }, cancellationToken);
    return QueryHttpResult(result);
});

app.MapPost("/api/index/switch", async (
    IndexStateService indexes,
    CancellationToken cancellationToken) =>
{
    try
    {
        return Results.Ok(await indexes.SwitchToNewAsync(cancellationToken));
    }
    catch (GatewayException ex)
    {
        return ErrorHttpResult(ex);
    }
});

app.MapPost("/api/index/rollback", async (
    IndexStateService indexes,
    CancellationToken cancellationToken) =>
{
    try
    {
        return Results.Ok(await indexes.RollbackAsync(cancellationToken));
    }
    catch (GatewayException ex)
    {
        return ErrorHttpResult(ex);
    }
});

app.MapPost("/api/orders/update", async (
    HttpRequest request,
    OrderWriter writer,
    CancellationToken cancellationToken) =>
{
    var count = ParseInt(request.Query["count"].FirstOrDefault(), 25);
    return Results.Ok(await writer.UpdateBatchAsync(count, cancellationToken));
});

app.MapPost("/api/simulate/update", async (
    HttpRequest request,
    OrderWriter writer,
    CancellationToken cancellationToken) =>
{
    var count = ParseInt(request.Query["count"].FirstOrDefault(), 25);
    return Results.Ok(await writer.UpdateBatchAsync(count, cancellationToken));
});

app.MapGet("/api/status", async (
    IndexStateService indexes,
    ReplicaManager replicas,
    GatewayOptions gatewayOptions,
    GatewayMetrics metrics,
    CancellationToken cancellationToken) =>
{
    var state = await indexes.GetStateAsync(cancellationToken);
    var versions = await replicas.ReadAllVersionsAsync(cancellationToken);
    var primaryVersion = await replicas.ReadPrimaryDataVersionAsync(cancellationToken);
    var snapshot = metrics.Snapshot();
    return Results.Ok(new
    {
        state,
        primaryVersion,
        replicaVersions = versions,
        consistent = versions.All(version => version.SnapshotVersion == primaryVersion),
        connectionCap = gatewayOptions.MaxConnections,
        counters = new
        {
            snapshotLag = snapshot.SnapshotLagCount,
            planMismatch = snapshot.PlanMismatchCount,
            connectionLimit = snapshot.ConnectionLimitCount,
            performanceBudget = snapshot.PerformanceBudgetCount,
            indexHits = snapshot.IndexHits
        }
    });
});

app.MapGet("/api/replicas", async (
    ReplicaManager replicas,
    CancellationToken cancellationToken) =>
{
    var primaryVersion = await replicas.ReadPrimaryDataVersionAsync(cancellationToken);
    return Results.Ok(new
    {
        primaryVersion,
        replicas = await replicas.ReadAllVersionsAsync(cancellationToken)
    });
});

app.MapGet("/api/audit", async (
    IndexStateService indexes,
    CancellationToken cancellationToken) =>
    Results.Ok(await indexes.GetAuditAsync(200, cancellationToken)));

app.MapGet("/api/report", async (
    ReportService report,
    CancellationToken cancellationToken) =>
    Results.Ok(await report.BuildAsync(cancellationToken: cancellationToken)));

app.MapPost("/api/report/write", async (
    ReportService report,
    CancellationToken cancellationToken) =>
{
    var document = await report.BuildAsync(cancellationToken: cancellationToken);
    var path = await report.WriteAsync(document, cancellationToken: cancellationToken);
    return Results.Ok(new { path, report = document });
});

app.Lifetime.ApplicationStopped.Register(() =>
{
    try
    {
        var report = app.Services.GetRequiredService<ReportService>();
        var document = report.BuildAsync().GetAwaiter().GetResult();
        var path = report.WriteAsync(document).GetAwaiter().GetResult();
        Console.Error.WriteLine($"Final report written to {path}");
    }
    catch (Exception ex)
    {
        Console.Error.WriteLine($"Unable to write final report: {ex.Message}");
    }
});

await app.RunAsync();
return 0;

static int ParseInt(string? value, int fallback) =>
    int.TryParse(value, out var parsed) ? parsed : fallback;

static int? ParseNullableInt(string? value) =>
    int.TryParse(value, out var parsed) ? parsed : null;

static IResult QueryHttpResult(QueryResult result)
{
    var statusCode = result.ErrorCode switch
    {
        ErrorCodes.SnapshotLag => StatusCodes.Status409Conflict,
        ErrorCodes.PlanMismatch => StatusCodes.Status409Conflict,
        ErrorCodes.ConnectionLimit => StatusCodes.Status429TooManyRequests,
        ErrorCodes.Validation => StatusCodes.Status400BadRequest,
        ErrorCodes.Database => StatusCodes.Status500InternalServerError,
        _ => StatusCodes.Status200OK
    };
    return Results.Json(result, statusCode: statusCode);
}

static IResult ErrorHttpResult(GatewayException exception) =>
    Results.Json(new
    {
        success = false,
        errorCode = exception.Code,
        message = exception.Message,
        details = exception.Details
    }, statusCode: exception.HttpStatus);
