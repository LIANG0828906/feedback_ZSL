using System.Collections.Concurrent;

namespace OrderGateway;

public sealed record QueryObservation(
    string KeyOrder,
    string IndexName,
    string AccessMethod,
    bool IndexMatched,
    double ElapsedMs,
    DateTimeOffset AtUtc);

public sealed record ErrorEvent(
    string Code,
    DateTimeOffset AtUtc,
    IReadOnlyDictionary<string, object?> Context);

public sealed record IndexSwitchEvent(
    string Phase,
    string IndexName,
    DateTimeOffset AtUtc,
    IReadOnlyDictionary<string, object?> Detail);

/// <summary>
/// Thread-safe in-memory observability sink. Everything stored here is
/// logical only: no machine names, file paths or external identifiers.
/// </summary>
public sealed class MetricsCollector
{
    private readonly ConcurrentQueue<QueryObservation> _observations = new();
    private readonly ConcurrentQueue<ErrorEvent> _errors = new();
    private readonly ConcurrentQueue<IndexSwitchEvent> _switches = new();
    private readonly ConcurrentDictionary<string, string> _firstPlanByOrder = new();

    public void RecordQuery(QueryObservation observation) => _observations.Enqueue(observation);
    public void RecordError(ErrorEvent error) => _errors.Enqueue(error);

    public void RecordError(string code, IReadOnlyDictionary<string, object?> context)
        => _errors.Enqueue(new ErrorEvent(code, DateTimeOffset.UtcNow, context));

    public void RecordSwitch(string phase, string indexName, IReadOnlyDictionary<string, object?> detail)
        => _switches.Enqueue(new IndexSwitchEvent(phase, indexName, DateTimeOffset.UtcNow, detail));

    public IReadOnlyList<QueryObservation> Observations => _observations.ToArray();
    public IReadOnlyList<ErrorEvent> Errors => _errors.ToArray();
    public IReadOnlyList<IndexSwitchEvent> Switches => _switches.ToArray();

    /// <summary>Retains one representative actual showplan per logical key order.</summary>
    public void RecordPlanXml(string keyOrder, string showplanXml)
        => _firstPlanByOrder.TryAdd(keyOrder, showplanXml);

    public IReadOnlyDictionary<string, string> FirstPlans => _firstPlanByOrder;

    public Dictionary<string, object> Summarize(
        int peakConnections,
        int configuredMaximum,
        int leaseRequests,
        int connectionTimeouts,
        int currentlyLeased)
    {
        var observations = _observations.ToArray();
        var errors = _errors.ToArray();
        var switches = _switches.ToArray();
        var switchOrigin = switches.Length == 0 ? DateTimeOffset.UtcNow : switches.Min(s => s.AtUtc);

        var byOrder = observations
            .GroupBy(o => o.KeyOrder)
            .OrderBy(g => g.Key, StringComparer.Ordinal)
            .ToDictionary(
                g => g.Key,
                g =>
                {
                    var grouped = g.GroupBy(o => $"{o.IndexName}|{o.AccessMethod}")
                        .OrderBy(x => x.Key, StringComparer.Ordinal)
                        .ToDictionary(x => x.Key, x => x.Count());
                    return new
                    {
                        totalQueries = g.Count(),
                        matchedQueries = g.Count(o => o.IndexMatched),
                        mismatchedQueries = g.Count(o => !o.IndexMatched),
                        accessPaths = grouped,
                    };
                });

        var errorCounts = new Dictionary<string, int>(StringComparer.Ordinal)
        {
            [GatewayError.PlanMismatch] = 0,
            [GatewayError.Deadlock] = 0,
            [GatewayError.ConnectionTimeout] = 0,
        };
        foreach (var group in errors.GroupBy(e => e.Code))
            errorCounts[group.Key] = group.Count();

        return new Dictionary<string, object>
        {
            ["totalQueries"] = observations.Length,
            ["keyOrders"] = byOrder,
            ["errorCounts"] = errorCounts,
            ["recentErrors"] = errors.Select(e => new
            {
                code = e.Code,
                occurredAtUtc = e.AtUtc.ToString("O"),
                context = e.Context,
            }).ToList(),
            ["indexSwitches"] = switches.Select(s => new
            {
                phase = s.Phase,
                indexName = s.IndexName,
                occurredAtUtc = s.AtUtc.ToString("O"),
                offsetMs = Math.Round((s.AtUtc - switchOrigin).TotalMilliseconds, 3),
                detail = s.Detail,
            }).ToList(),
            ["connectionPool"] = new
            {
                configuredMaximum,
                inUse = currentlyLeased,
                peakInUse = peakConnections,
                leaseRequests,
                acquisitionTimeouts = connectionTimeouts,
            },
            ["pagingSnapshots"] = new
            {
                sessionMode = "snapshot-isolation",
                fullWalkGuarantee = "one transaction per pagination walk",
            },
        };
    }
}
