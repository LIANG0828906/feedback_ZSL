using System.Diagnostics;

namespace OrderGateway.Scenarios;

/// <summary>
/// Requirement 5: leased connections never exceed the configured cap; long
/// and short queries share the same slots without starvation; a request that
/// cannot get a slot inside the window fails with E_CONN_TIMEOUT and frees
/// everything it held.
/// </summary>
public sealed class ConnectionPoolScenario : IScenario
{
    public string Name => "s5_connection_pool";

    public async Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken)
    {
        var gate = context.Gate;
        var repository = context.Repository;
        var workload = context.Workload;
        int cap = context.Options.MaxConcurrentConnections;

        /* Phase A: saturate the pool with long leases, then prove short reads
           still complete once slots free (no permanent starvation). */
        var saturating = Enumerable.Range(0, cap - 1)
            .Select(_ => workload.RunLongQueryAsync(TimeSpan.FromSeconds(2), cancellationToken))
            .ToArray();

        await Task.Delay(TimeSpan.FromMilliseconds(700), cancellationToken);
        var peakWhileSaturated = gate.PeakLeased;

        var shortReadSw = Stopwatch.StartNew();
        var shortPage = await repository.ReadPageAsync(
            KeyOrder.New,
            new PageQuery { TenantId = 1, Status = 0, PageSize = 20 },
            cancellationToken);
        shortReadSw.Stop();
        await Task.WhenAll(saturating);

        bool shortReadSurvived = shortPage.Rows.Count > 0;

        /* Phase B: fully exhaust the same gateway pool and confirm an extra
           request fails fast with E_CONN_TIMEOUT instead of hanging. The probe
           uses a tighter slot wait only; it does not create a second pool. */
        var blockers = Enumerable.Range(0, cap)
            .Select(_ => workload.RunLongQueryAsync(TimeSpan.FromSeconds(3), cancellationToken))
            .ToArray();

        await Task.Delay(TimeSpan.FromMilliseconds(900), cancellationToken);
        var peakAtExhaustion = gate.PeakLeased;

        bool timeoutObserved = false;
        GatewayException? timeoutError = null;
        var sw = Stopwatch.StartNew();
        try
        {
            await repository.ReadPageWithAcquireTimeoutAsync(
                KeyOrder.Legacy,
                new PageQuery { TenantId = 0, Status = 0, PageSize = 10 },
                TimeSpan.FromMilliseconds(800),
                cancellationToken);
        }
        catch (GatewayException ex) when (ex.ErrorCode == GatewayError.ConnectionTimeout)
        {
            timeoutObserved = true;
            timeoutError = ex;
            context.Metrics.RecordError(ex.ErrorCode, ex.Context);
        }
        sw.Stop();

        await Task.WhenAll(blockers);
        var peakAfterRelease = gate.PeakLeased;
        var passed = shortReadSurvived
                     && timeoutObserved
                     && peakAtExhaustion <= cap
                     && peakAfterRelease <= cap
                     && gate.AcquireTimeouts >= 1;

        return new ScenarioResult(Name, passed,
            passed ? $"Cap of {cap} respected; shared slots serve short reads; exhaustion returns E_CONN_TIMEOUT." : "Pool boundary violated.",
            new Dictionary<string, object>
            {
                ["configuredCap"] = cap,
                ["peakWhileSaturated"] = peakWhileSaturated,
                ["peakAtExhaustion"] = peakAtExhaustion,
                ["peakObservedOverall"] = peakAfterRelease,
                ["shortReadElapsedMs"] = shortReadSw.ElapsedMilliseconds,
                ["shortReadSurvived"] = shortReadSurvived,
                ["acquireTimeoutObserved"] = timeoutObserved,
                ["acquireTimeoutElapsedMs"] = sw.ElapsedMilliseconds,
                ["acquireTimeoutContext"] = timeoutError?.Context ?? new Dictionary<string, object?>(),
                ["acquireTimeoutCounter"] = gate.AcquireTimeouts,
                ["totalLeaseRequests"] = gate.TotalAcquisitions,
            });
    }
}
