using System.Security.Cryptography;
using OrderGateway;
using OrderGateway.Scenarios;

var cancellationToken = CancellationToken.None;
var options = GatewayOptions.FromEnvironment();

var artifactDir = Environment.GetEnvironmentVariable("OG_ARTIFACT_DIR")
                  ?? Path.Combine(Directory.GetCurrentDirectory(), "artifacts", "run");
Directory.CreateDirectory(artifactDir);

var initSqlPath = ResolveInitSqlPath();

Console.WriteLine($"[boot] initializing database '{options.DatabaseName}' from deterministic script");
var inputFingerprint = Convert.ToHexString(
    SHA256.HashData(await File.ReadAllBytesAsync(initSqlPath, cancellationToken)))[..16];
var bootstrapper = new DbBootstrapper(options);
await bootstrapper.InitializeAsync(initSqlPath, cancellationToken);
var rowCount = await bootstrapper.CountOrdersAsync(cancellationToken);
Console.WriteLine($"[boot] orders ready: {rowCount} rows");

using var gate = new ConnectionGate(options);
var metrics = new MetricsCollector();
var indexBarrier = new IndexChangeBarrier();
var repository = new OrderRepository(gate, options, metrics, indexBarrier);
var lifecycle = new IndexLifecycleService(gate, options, metrics, indexBarrier);
var workload = new OrderWorkload(gate, options, metrics);

var context = new ScenarioContext
{
    Options = options,
    Gate = gate,
    Metrics = metrics,
    Repository = repository,
    Lifecycle = lifecycle,
    Workload = workload,
    IndexBarrier = indexBarrier,
    ArtifactDirectory = artifactDir,
};

var scenarios = new IScenario[]
{
    new PaginationEquivalenceScenario(),
    new PlanGuardScenario(),
    new DualIndexCutoverScenario(),
    new ConcurrentConsistencyScenario(),
    new ConnectionPoolScenario(),
    new RollbackScenario(),
    new ObservabilityScenario(),
    new PerformanceBoundaryScenario(),
};

var results = new List<ScenarioResult>();
foreach (var scenario in scenarios)
{
    await lifecycle.EnsureDualIndexesAsync(cancellationToken);
    Console.WriteLine($"[run] {scenario.Name}");
    try
    {
        var result = await scenario.RunAsync(context, cancellationToken);
        results.Add(result);
        Console.WriteLine($"[run] {scenario.Name} => {(result.Passed ? "PASS" : "FAIL")} :: {result.Summary}");
    }
    catch (Exception ex)
    {
        results.Add(new ScenarioResult(
            scenario.Name,
            false,
            $"Unhandled exception: {ex.GetType().Name}: {ex.Message}",
            new Dictionary<string, object>()));
        Console.WriteLine($"[run] {scenario.Name} => ERROR :: {ex}");
    }
}

foreach (var (keyOrder, xml) in metrics.FirstPlans)
    StageReportWriter.WritePlanEvidence(artifactDir, keyOrder, xml);

var catalog = await lifecycle.GetIndexCatalogAsync(cancellationToken);
var summary = metrics.Summarize(
    gate.PeakLeased,
    options.MaxConcurrentConnections,
    gate.TotalAcquisitions,
    gate.AcquireTimeouts,
    gate.CurrentlyLeased);
summary["inputFingerprint"] = inputFingerprint;
summary["configuration"] = new
{
    maxConnections = options.MaxConcurrentConnections,
    connectionAcquireTimeoutSeconds = options.ConnectionAcquireTimeout.TotalSeconds,
    commandTimeoutSeconds = options.CommandTimeout.TotalSeconds,
    heartbeatExpirySeconds = options.HeartbeatExpiry.TotalSeconds,
    indexDdlLockTimeoutSeconds = options.IndexDdlLockTimeout.TotalSeconds,
    performanceConcurrency = 6,
    p95ThresholdMs = 150,
    processorCountVisibleToProcess = Environment.ProcessorCount,
    processWorkingSetMiB = Math.Round(Environment.WorkingSet / 1024d / 1024d, 1),
};
summary["databaseProfile"] = "local-order-gateway";
summary["scenarioResults"] = results.Select(r => new
{
    name = r.Name,
    passed = r.Passed,
    summary = r.Summary,
    evidence = r.Evidence,
}).ToArray();
summary["finalIndexCatalog"] = catalog;
summary["dataset"] = new { orderRows = rowCount, tenants = 12, statuses = 5, yearSpan = 3 };

var reportPath = StageReportWriter.Write(artifactDir, "full", summary);

var allPassed = results.All(r => r.Passed);
Console.WriteLine();
Console.WriteLine($"[report] {reportPath}");
Console.WriteLine($"[result] overall={(allPassed ? "PASS" : "FAIL")} passed={results.Count(r => r.Passed)}/{results.Count}");

return allPassed ? 0 : 2;

static string ResolveInitSqlPath()
{
    var candidates = new[]
    {
        Environment.GetEnvironmentVariable("OG_INIT_SQL"),
        Path.Combine(AppContext.BaseDirectory, "sql", "01_init.sql"),
        Path.Combine(Directory.GetCurrentDirectory(), "sql", "01_init.sql"),
        Path.GetFullPath(Path.Combine(Directory.GetCurrentDirectory(), "..", "..", "..", "..", "sql", "01_init.sql")),
    };

    foreach (var candidate in candidates)
    {
        if (!string.IsNullOrWhiteSpace(candidate) && File.Exists(candidate)) return candidate;
    }

    throw new FileNotFoundException("Could not locate sql/01_init.sql. Set OG_INIT_SQL explicitly.");
}
