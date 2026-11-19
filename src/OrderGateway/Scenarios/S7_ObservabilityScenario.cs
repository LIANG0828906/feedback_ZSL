namespace OrderGateway.Scenarios;

/// <summary>
/// Requirement 7: validates that the in-memory telemetry carries every
/// required field - per-key-order index names, hit counts, business error
/// counters, pool peak and switch timestamps - with no absolute paths or
/// external identifiers.
/// </summary>
public sealed class ObservabilityScenario : IScenario
{
    public string Name => "s7_observability";

    public Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken)
    {
        var metrics = context.Metrics;
        var observations = metrics.Observations;
        var errors = metrics.Errors;
        var switches = metrics.Switches;

        var keyOrdersPresent = observations
            .Select(o => o.KeyOrder)
            .Distinct(StringComparer.Ordinal)
            .OrderBy(x => x, StringComparer.Ordinal)
            .ToArray();

        var hitByIndex = observations
            .GroupBy(o => o.IndexName)
            .ToDictionary(g => g.Key, g => g.Count());

        var errorCounts = errors
            .GroupBy(e => e.Code)
            .ToDictionary(g => g.Key, g => g.Count());

        bool hasPlanMismatchEvent = errorCounts.ContainsKey(GatewayError.PlanMismatch);
        bool hasSwitchTimestamps = switches.Any(s => s.Phase == "retire_begin")
                                   && switches.Any(s => s.Phase == "retire_complete");
        bool poolPeakRecorded = context.Gate.PeakLeased > 0 &&
                                context.Gate.PeakLeased <= context.Options.MaxConcurrentConnections;

        var leakedPathLikeValues = observations
            .SelectMany(o => new[] { o.IndexName, o.AccessMethod, o.KeyOrder })
            .Any(v => v.Contains('/') || v.Contains('\\') || v.Contains(':'));

        var passed = keyOrdersPresent.Length == 2
                     && hitByIndex.Count >= 2
                     && hasPlanMismatchEvent
                     && hasSwitchTimestamps
                     && poolPeakRecorded
                     && !leakedPathLikeValues;

        var evidence = new Dictionary<string, object>
        {
            ["keyOrdersObserved"] = keyOrdersPresent,
            ["hitCountByIndex"] = hitByIndex,
            ["errorCounts"] = errorCounts,
            ["deadlockCount"] = errorCounts.GetValueOrDefault(GatewayError.Deadlock, 0),
            ["planMismatchCount"] = errorCounts.GetValueOrDefault(GatewayError.PlanMismatch, 0),
            ["indexSwitchPhases"] = switches.Select(s => s.Phase).ToArray(),
            ["connectionPoolPeak"] = context.Gate.PeakLeased,
            ["connectionPoolCap"] = context.Options.MaxConcurrentConnections,
            ["noAbsolutePathsInLogicalFields"] = !leakedPathLikeValues,
        };

        return Task.FromResult(new ScenarioResult(
            Name,
            passed,
            passed ? "Telemetry contains every required logical field without path/host leakage." : "Observability fields incomplete.",
            evidence));
    }
}
