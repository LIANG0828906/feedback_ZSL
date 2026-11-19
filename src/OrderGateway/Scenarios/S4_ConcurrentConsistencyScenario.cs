using System.Diagnostics;

namespace OrderGateway.Scenarios;

/// <summary>
/// Requirement 4: under mixed reads (both key orders) and repeated updates of
/// the same batch, every individual page must be duplicate-free and every
/// full walk must be a complete 500-row multiset; no deadlock victim allowed.
/// </summary>
public sealed class ConcurrentConsistencyScenario : IScenario
{
    public string Name => "s4_concurrent_consistency";

    public async Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken)
    {
        const int tenantId = 6;
        const byte status = 2;
        var stopwatch = Stopwatch.StartNew();
        using var cts = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        var token = cts.Token;

        var writerTask = Task.Run(async () =>
        {
            int round = 0;
            while (!token.IsCancellationRequested)
            {
                await context.Workload.UpdateBatchAsync(tenantId, status, batchSize: 25, round, token);
                round++;
                await Task.Delay(TimeSpan.FromMilliseconds(25), token);
            }
            return round;
        });

        var walkTasks = new List<Task<WalkOutcome>>();
        foreach (var pageSize in new[] { 13, 100 })
        {
            walkTasks.Add(Task.Run(() => WalkAndValidateAsync(context, KeyOrder.Legacy, tenantId, status, pageSize, token)));
            walkTasks.Add(Task.Run(() => WalkAndValidateAsync(context, KeyOrder.New, tenantId, status, pageSize, token)));
        }

        var outcomes = await Task.WhenAll(walkTasks);
        cts.Cancel();
        int updateRounds;
        try { updateRounds = await writerTask; }
        catch { updateRounds = -1; }

        var deadlockEvents = context.Metrics.Errors
            .Where(e => e.Code == GatewayError.Deadlock)
            .ToList();

        var passed = outcomes.All(o => o.Passed) && deadlockEvents.Count == 0;

        return new ScenarioResult(Name, passed,
            passed ? $"All {outcomes.Length} concurrent walks complete and duplicate-free; zero deadlocks." : "Duplicate/missing rows or deadlock observed.",
            new Dictionary<string, object>
            {
                ["durationMs"] = stopwatch.ElapsedMilliseconds,
                ["updateRounds"] = updateRounds,
                ["deadlockEvents"] = deadlockEvents.Count,
                ["walks"] = outcomes.Select(o => new
                {
                    o.KeyOrder,
                    o.PageSize,
                    o.TotalRows,
                    o.DuplicateRows,
                    o.MissingRows,
                    o.Passed,
                }).ToArray(),
            });
    }

    private static async Task<WalkOutcome> WalkAndValidateAsync(
        ScenarioContext context,
        KeyOrder keyOrder,
        int tenantId,
        byte status,
        int pageSize,
        CancellationToken cancellationToken)
    {
        var ids = new List<long>();
        PageCursor? cursor = null;
        int duplicateRows = 0;
        var seen = new HashSet<long>();

        await using var session = await SnapshotPagingSession.BeginAsync(
            context.Gate, context.Options, context.Metrics, context.IndexBarrier,
            keyOrder, cancellationToken);

        while (true)
        {
            var page = await session.ReadPageAsync(new PageQuery
            {
                TenantId = tenantId,
                Status = status,
                PageSize = pageSize,
                Cursor = cursor,
            }, cancellationToken);

            foreach (var row in page.Rows)
            {
                if (!seen.Add(row.OrderId)) duplicateRows++;
                ids.Add(row.OrderId);
            }

            if (!page.HasMore || page.NextCursor is null) break;
            cursor = page.NextCursor;
        }

        var distinct = ids.Distinct().Count();
        return new WalkOutcome(
            keyOrder.AsLabel(),
            pageSize,
            ids.Count,
            duplicateRows,
            Math.Max(0, 500 - distinct),
            duplicateRows == 0 && distinct == 500);
    }

    private sealed record WalkOutcome(
        string KeyOrder,
        int PageSize,
        int TotalRows,
        int DuplicateRows,
        int MissingRows,
        bool Passed);
}
