using System.Diagnostics;

namespace OrderGateway.Scenarios;

/// <summary>
/// Requirement 8: on the 30k-row table, with the pool saturated by six
/// concurrent readers, index-backed single-page reads must stay at or below
/// 150ms P95 and must never use a table/clustered scan. The saved actual
/// showplans provide the required execution-plan evidence.
/// </summary>
public sealed class PerformanceBoundaryScenario : IScenario
{
    public string Name => "s8_performance_boundary";

    public async Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken)
    {
        var repository = context.Repository;
        const int concurrency = 6;
        const int iterationsPerReader = 10;
        const double p95LimitMs = 150;

        var filters = new (int Tenant, byte Status)[]
        {
            (0, 0), (1, 1), (2, 2), (3, 3), (4, 4), (5, 0),
        };

        var samples = new List<double>();
        var lockObject = new object();
        var accessMethods = new List<(string KeyOrder, string Index, string Method, bool IsScan)>();

        var readerTasks = Enumerable.Range(0, concurrency).Select(async slot =>
        {
            var local = new List<double>();
            var (tenant, status) = filters[slot];
            var keyOrder = slot % 2 == 0 ? KeyOrder.New : KeyOrder.Legacy;

            for (int i = 0; i < iterationsPerReader; i++)
            {
                var sw = Stopwatch.StartNew();
                var page = await repository.ReadPageAsync(
                    keyOrder,
                    new PageQuery { TenantId = tenant, Status = status, PageSize = 50 },
                    cancellationToken);
                sw.Stop();
                local.Add(sw.Elapsed.TotalMilliseconds);

                lock (lockObject)
                {
                    bool isScan = page.AccessMethod.Contains("Scan", StringComparison.OrdinalIgnoreCase);
                    accessMethods.Add((keyOrder.AsLabel(), page.UsedIndex, page.AccessMethod, isScan));
                }
            }

            lock (lockObject) samples.AddRange(local);
        }).ToArray();

        await Task.WhenAll(readerTasks);

        var ordered = samples.OrderBy(x => x).ToArray();
        double p95 = ordered[(int)Math.Ceiling(0.95 * ordered.Length) - 1];
        double p50 = ordered[ordered.Length / 2];
        double max = ordered[^1];

        bool noScans = accessMethods.All(a => !a.IsScan && !a.Index.Contains("PK_Orders", StringComparison.Ordinal));
        bool allSeeks = accessMethods.All(a =>
            a.Method.Contains("Seek", StringComparison.OrdinalIgnoreCase));
        bool p95Passed = p95 <= p95LimitMs;

        var byPath = accessMethods
            .GroupBy(a => $"{a.KeyOrder} -> {a.Index} [{a.Method}]")
            .ToDictionary(g => g.Key, g => g.Count());

        var passed = p95Passed && noScans && allSeeks;

        return new ScenarioResult(Name, passed,
            passed ? $"P95 {p95:F1}ms across {samples.Count} saturated reads; all index seeks." : "Performance boundary breached.",
            new Dictionary<string, object>
            {
                ["concurrency"] = concurrency,
                ["totalSamples"] = samples.Count,
                ["p50Ms"] = Math.Round(p50, 2),
                ["p95Ms"] = Math.Round(p95, 2),
                ["p95LimitMs"] = p95LimitMs,
                ["maxMs"] = Math.Round(max, 2),
                ["allAccessPaths"] = byPath,
                ["noTableOrClusteredScan"] = noScans,
                ["allIndexSeeks"] = allSeeks,
                ["planEvidenceFiles"] = new[]
                {
                    $"actual_plan_{KeyOrder.Legacy.AsLabel()}.sqlplan",
                    $"actual_plan_{KeyOrder.New.AsLabel()}.sqlplan",
                },
            });
    }
}
