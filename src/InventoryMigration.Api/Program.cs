using System.Globalization;
using System.Text;
using InventoryMigration.Api.Domain;
using InventoryMigration.Api.Infrastructure;
using InventoryMigration.Api.Options;
using InventoryMigration.Api.Services;
using Microsoft.Extensions.Options;
using Npgsql;

var builder = WebApplication.CreateBuilder(args);

builder.Services.Configure<MigrationOptions>(
    builder.Configuration.GetSection(MigrationOptions.SectionName));
builder.Services.AddSingleton<FaultInjector>();
builder.Services.AddSingleton<LatencyTracker>();
builder.Services.AddSingleton<MetricsRegistry>();
builder.Services.AddSingleton<WriteGate>();
builder.Services.AddSingleton<LedgerStore>();
builder.Services.AddSingleton<DatabasePools>();
builder.Services.AddSingleton<ValidationService>();
builder.Services.AddSingleton<DebitService>();
builder.Services.AddSingleton<MigrationService>();
builder.Services.AddHostedService<CaptureWorker>();

var app = builder.Build();

app.Use(async (context, next) =>
{
    try
    {
        await next(context);
    }
    catch (ServiceException exception)
    {
        context.Response.StatusCode = exception.StatusCode;
        context.Response.ContentType = "application/json; charset=utf-8";
        await context.Response.WriteAsJsonAsync(
            new ErrorResponse(exception.Code, exception.Message, exception.ProductId));
    }
    catch (BadHttpRequestException exception)
    {
        context.Response.StatusCode = StatusCodes.Status400BadRequest;
        context.Response.ContentType = "application/json; charset=utf-8";
        await context.Response.WriteAsJsonAsync(
            new ErrorResponse(ErrorCodes.BadRequest, exception.Message, null));
    }
});

var ledger = app.Services.GetRequiredService<LedgerStore>();
var options = app.Services.GetRequiredService<IOptions<MigrationOptions>>().Value;
var logger = app.Services.GetRequiredService<ILoggerFactory>().CreateLogger("Startup");
await ledger.InitializeAsync(CancellationToken.None);
var (productMin, productMax) = await ResolveProductRangeAsync(
    app.Services.GetRequiredService<DatabasePools>().Old,
    options,
    logger);
await ledger.EnsureProductRangeAsync(productMin, productMax, CancellationToken.None);

app.MapGet("/", () => Results.Json(new
{
    service = "inventory-migration",
    status = "/api/status",
    metrics = "/metrics",
    migrate = "POST /api/migration/start",
    debit = "POST /api/debit"
}));

app.MapGet("/health", async (
    LedgerStore store,
    DatabasePools pools,
    CancellationToken cancellationToken) =>
{
    var state = await store.GetStateAsync(cancellationToken);
    return Results.Json(new
    {
        status = "ok",
        ledger = state.State,
        oldPool = pools.Old.Status,
        newPool = pools.New.Status
    });
});

app.MapPost("/api/migration/start", async (
    MigrationService migration,
    CancellationToken cancellationToken) =>
{
    var started = await migration.StartAsync(cancellationToken);
    return Results.Accepted("/api/status", new { started });
});

app.MapPost("/api/migration/resume", async (
    MigrationService migration,
    CancellationToken cancellationToken) =>
{
    var started = await migration.StartAsync(cancellationToken);
    return Results.Accepted("/api/status", new { started });
});

app.MapPost("/api/migration/pause", async (
    MigrationService migration,
    CancellationToken cancellationToken) =>
{
    await migration.PauseAsync(cancellationToken);
    return Results.Json(new { paused = true });
});

app.MapGet("/api/migration/batches", async (
    LedgerStore store,
    CancellationToken cancellationToken) =>
    Results.Json(await store.GetCommittedBatchesAsync(cancellationToken)));

app.MapPost("/api/debit", DebitHandler);
app.MapPost("/api/debits", DebitHandler);

app.MapPost("/api/rollback", RollbackHandler);
app.MapPost("/api/rollbacks", RollbackHandler);

app.MapGet("/api/rollbacks", async (
    LedgerStore store,
    CancellationToken cancellationToken) =>
    Results.Json(await store.GetRollbacksAsync(cancellationToken)));

app.MapGet("/api/validation/latest", async (
    LedgerStore store,
    CancellationToken cancellationToken) =>
{
    var validation = await store.GetLastValidationAsync(cancellationToken);
    return validation is null ? Results.NotFound() : Results.Json(validation);
});

app.MapGet("/api/validation/mismatches", async (
    LedgerStore store,
    CancellationToken cancellationToken) =>
    Results.Json(await store.GetValidationMismatchesAsync(cancellationToken)));

app.MapGet("/api/validation/{batchId:long}", async (
    long batchId,
    LedgerStore store,
    CancellationToken cancellationToken) =>
{
    var validation = await store.GetValidationByBatchAsync(batchId, cancellationToken);
    return validation is null ? Results.NotFound() : Results.Json(validation);
});

app.MapPost("/api/admin/faults", (
    FaultRequest request,
    FaultInjector faults) =>
{
    faults.Set(request);
    return Results.Json(new
    {
        oldDbUnavailable = faults.OldDbUnavailable,
        migrationBatchDelayMs = faults.MigrationBatchDelayMs,
        validationCorruptProductIds = faults.GetValidationCorruptionProducts().OrderBy(id => id)
    });
});

app.MapDelete("/api/admin/faults", (FaultInjector faults) =>
{
    faults.Clear();
    return Results.Json(new { cleared = true });
});

app.MapPost("/api/admin/direct-old-change", async (
    DebitRequest request,
    DebitService debit,
    CancellationToken cancellationToken) =>
    Results.Json(await debit.ApplyDirectOldChangeAsync(request, cancellationToken)));

app.MapGet("/api/status", async (
    LedgerStore store,
    DatabasePools pools,
    CancellationToken cancellationToken) =>
{
    var state = await store.GetStateAsync(cancellationToken);
    var rollbacks = await store.GetRollbacksAsync(cancellationToken);
    var validation = await store.GetLastValidationAsync(cancellationToken);
    var completed = state.LastCommittedEnd >= state.ProductMax && state.CurrentBatchId is null;
    var position = new MigrationPositionStatus(
        state.ProductMin,
        state.ProductMax,
        state.LastCommittedEnd,
        Math.Min(state.ProductMax + 1, state.LastCommittedEnd + 1),
        state.CurrentBatchId,
        state.CurrentBatch,
        completed);
    var connectionPools = new ConnectionPoolsStatus(
        ToStatus(pools.Old.Status),
        ToStatus(pools.New.Status));
    return Results.Json(new StatusResponse(
        "inventory-migration",
        state.State,
        DateTimeOffset.UtcNow,
        position,
        connectionPools,
        rollbacks,
        validation));
});

app.MapGet("/metrics", async (
    LedgerStore store,
    DatabasePools pools,
    MetricsRegistry metrics,
    CancellationToken cancellationToken) =>
{
    var summary = await store.GetCommittedSummaryAsync(cancellationToken);
    var snapshot = metrics.Snapshot(summary.Products, summary.Flows, summary.Rollbacks);
    var text = RenderMetrics(snapshot, pools.Old.Status, pools.New.Status);
    return Results.Text(text, "text/plain; version=0.0.4; charset=utf-8");
});

var migration = app.Services.GetRequiredService<MigrationService>();
await migration.AutoResumeAsync(CancellationToken.None);
await app.RunAsync();

static async Task<IResult> DebitHandler(
    DebitRequest request,
    DebitService debit,
    CancellationToken cancellationToken)
{
    return Results.Json(await debit.DebitAsync(request, cancellationToken));
}

static async Task<IResult> RollbackHandler(
    RollbackRequest request,
    LedgerStore ledger,
    CancellationToken cancellationToken)
{
    if (request.ProductId <= 0)
    {
        throw new ServiceException(
            ErrorCodes.BadRequest,
            "productId 必须为正整数",
            StatusCodes.Status400BadRequest,
            request.ProductId);
    }

    var existing = await ledger.GetRollbackAsync(request.ProductId, cancellationToken);
    if (existing is not null)
    {
        return Results.Json(existing);
    }

    var route = await ledger.GetRouteAsync(request.ProductId, cancellationToken);
    if (route.Route != ProductRoute.New || !route.BatchId.HasValue)
    {
        throw new ServiceException(
            ErrorCodes.Rollback,
            "只有已提交到新库的商品可以回滚",
            StatusCodes.Status409Conflict,
            request.ProductId);
    }

    if (!request.Force && !await ledger.IsValidationMismatchAsync(
            request.ProductId,
            route.BatchId.Value,
            cancellationToken))
    {
        throw new ServiceException(
            ErrorCodes.Validation,
            "该商品不在对应批次的校验差异清单中；如确需回滚请传 force=true",
            StatusCodes.Status409Conflict,
            request.ProductId);
    }

    var rollback = await ledger.AddRollbackAsync(
        request.ProductId,
        route.BatchId.Value,
        string.IsNullOrWhiteSpace(request.Reason) ? "validation_mismatch" : request.Reason,
        cancellationToken);
    return Results.Json(rollback);
}

static PoolStatus ToStatus(DatabasePoolStatus status) =>
    new(status.Limit, status.InUse, status.Available, status.Waiting);

static string RenderMetrics(
    ServiceMetricsSnapshot snapshot,
    DatabasePoolStatus oldPool,
    DatabasePoolStatus newPool)
{
    var text = new StringBuilder();
    text.AppendLine("# HELP inventory_migration_migrated_products_total Products committed to inventory_new.");
    text.AppendLine("# TYPE inventory_migration_migrated_products_total gauge");
    text.AppendLine(
        $"inventory_migration_migrated_products_total{{source=\"inventory_old\",target=\"inventory_new\"}} {snapshot.MigratedProducts}");
    text.AppendLine("# HELP inventory_migration_migrated_flows_total Flow rows committed to inventory_new.");
    text.AppendLine("# TYPE inventory_migration_migrated_flows_total gauge");
    text.AppendLine(
        $"inventory_migration_migrated_flows_total{{source=\"inventory_old\",target=\"inventory_new\"}} {snapshot.MigratedFlows}");
    text.AppendLine("# HELP inventory_migration_captured_changes_total Legacy changes captured by the outbox trigger.");
    text.AppendLine("# TYPE inventory_migration_captured_changes_total counter");
    text.AppendLine(
        $"inventory_migration_captured_changes_total{{database=\"inventory_old\"}} {snapshot.CapturedChanges}");
    text.AppendLine("# HELP inventory_migration_routed_old_debits_total Debit requests routed to inventory_old.");
    text.AppendLine("# TYPE inventory_migration_routed_old_debits_total counter");
    text.AppendLine(
        $"inventory_migration_routed_old_debits_total{{database=\"inventory_old\"}} {snapshot.RoutedOldDebits}");
    text.AppendLine("# HELP inventory_migration_routed_new_debits_total Debit requests routed to inventory_new.");
    text.AppendLine("# TYPE inventory_migration_routed_new_debits_total counter");
    text.AppendLine(
        $"inventory_migration_routed_new_debits_total{{database=\"inventory_new\"}} {snapshot.RoutedNewDebits}");
    text.AppendLine("# HELP inventory_migration_e_migrating_total Debit requests rejected with E_MIGRATING.");
    text.AppendLine("# TYPE inventory_migration_e_migrating_total counter");
    text.AppendLine(
        $"inventory_migration_e_migrating_total{{reason=\"batch_visibility\"}} {snapshot.EMigrating}");
    text.AppendLine("# HELP inventory_migration_validation_mismatch_products Products in the latest validation mismatch list.");
    text.AppendLine("# TYPE inventory_migration_validation_mismatch_products gauge");
    text.AppendLine(
        $"inventory_migration_validation_mismatch_products{{database=\"inventory_new\"}} {snapshot.ValidationMismatchProducts}");
    text.AppendLine("# HELP inventory_migration_rollback_products Products routed back to inventory_old.");
    text.AppendLine("# TYPE inventory_migration_rollback_products gauge");
    text.AppendLine(
        $"inventory_migration_rollback_products{{database=\"inventory_old\"}} {snapshot.RollbackProducts}");
    text.AppendLine("# HELP inventory_migration_capture_p95_ms P95 routed write and capture latency in milliseconds.");
    text.AppendLine("# TYPE inventory_migration_capture_p95_ms gauge");
    text.AppendLine(
        $"inventory_migration_capture_p95_ms{{database=\"inventory_old\"}} {snapshot.CaptureP95Ms.ToString("0.###", CultureInfo.InvariantCulture)}");
    text.AppendLine("# HELP inventory_migration_connection_pool Bounded connections currently in use.");
    text.AppendLine("# TYPE inventory_migration_connection_pool gauge");
    text.AppendLine(
        $"inventory_migration_connection_pool{{instance=\"old\",database=\"inventory_old\"}} {oldPool.InUse}");
    text.AppendLine(
        $"inventory_migration_connection_pool{{instance=\"new\",database=\"inventory_new\"}} {newPool.InUse}");
    text.AppendLine("# HELP inventory_migration_connection_pool_limit Configured per-instance connection limit.");
    text.AppendLine("# TYPE inventory_migration_connection_pool_limit gauge");
    text.AppendLine(
        $"inventory_migration_connection_pool_limit{{instance=\"old\",database=\"inventory_old\"}} {oldPool.Limit}");
    text.AppendLine(
        $"inventory_migration_connection_pool_limit{{instance=\"new\",database=\"inventory_new\"}} {newPool.Limit}");
    return text.ToString();
}

static async Task<(long Min, long Max)> ResolveProductRangeAsync(
    DatabasePool oldPool,
    MigrationOptions options,
    ILogger logger)
{
    if (options.ProductMin.HasValue && options.ProductMax.HasValue)
    {
        return (options.ProductMin.Value, options.ProductMax.Value);
    }

    try
    {
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        await using var lease = await oldPool.AcquireAsync(timeout.Token);
        await using var command = new NpgsqlCommand(
            "SELECT COALESCE(MIN(product_id), 1), COALESCE(MAX(product_id), 50000) FROM inventory;",
            lease.Connection);
        await using var reader = await command.ExecuteReaderAsync(timeout.Token);
        await reader.ReadAsync(timeout.Token);
        return (
            options.ProductMin ?? reader.GetInt64(0),
            options.ProductMax ?? reader.GetInt64(1));
    }
    catch (Exception exception)
    {
        logger.LogWarning(
            exception,
            "Could not read product range from inventory_old; using configured/default range.");
        return (options.ProductMin ?? 1, options.ProductMax ?? 50000);
    }
}
