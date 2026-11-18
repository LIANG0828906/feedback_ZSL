using System.Collections.Concurrent;
using SnapshotGateway.Domain;

namespace SnapshotGateway.Services;

public sealed class GatewayMetrics
{
    private const int MaxMeasurements = 4_000;
    private readonly ConcurrentDictionary<string, long> _indexHits =
        new(StringComparer.OrdinalIgnoreCase) { ["old"] = 0, ["new"] = 0 };
    private readonly ConcurrentQueue<ReplicaCatchupRecord> _catchups = new();
    private readonly ConcurrentQueue<PerformanceMeasurement> _measurements = new();
    private readonly ConcurrentQueue<QueryFailureRecord> _failures = new();
    private long _snapshotLagCount;
    private long _planMismatchCount;
    private long _connectionLimitCount;
    private long _performanceBudgetCount;
    private long _queryCount;
    private long _successfulQueryCount;
    private PerformanceProbe? _probe;

    public void IncrementIndexHit(string keyOrder) =>
        _indexHits.AddOrUpdate(keyOrder.ToLowerInvariant(), 1, (_, current) => current + 1);

    public void IncrementSnapshotLag() => Interlocked.Increment(ref _snapshotLagCount);

    public void IncrementPlanMismatch() => Interlocked.Increment(ref _planMismatchCount);

    public void IncrementConnectionLimit() => Interlocked.Increment(ref _connectionLimitCount);

    public void IncrementQuery(bool successful)
    {
        Interlocked.Increment(ref _queryCount);
        if (successful)
        {
            Interlocked.Increment(ref _successfulQueryCount);
        }
    }

    public void RecordCatchup(ReplicaCatchupRecord record) => _catchups.Enqueue(record);

    public void RecordFailure(QueryFailureRecord record)
    {
        _failures.Enqueue(record);
        while (_failures.Count > 2_000 && _failures.TryDequeue(out _))
        {
        }
    }

    public void RecordMeasurement(PerformanceMeasurement measurement)
    {
        _measurements.Enqueue(measurement);
        while (_measurements.Count > MaxMeasurements && _measurements.TryDequeue(out _))
        {
        }
    }

    public void SetPerformanceProbe(PerformanceProbe probe)
    {
        Interlocked.Exchange(ref _probe, probe);
        if (!probe.Passed)
        {
            Interlocked.Increment(ref _performanceBudgetCount);
        }
    }

    public MetricsSnapshot Snapshot()
    {
        return new MetricsSnapshot(
            new Dictionary<string, long>(_indexHits),
            Interlocked.Read(ref _snapshotLagCount),
            Interlocked.Read(ref _planMismatchCount),
            Interlocked.Read(ref _connectionLimitCount),
            Interlocked.Read(ref _performanceBudgetCount),
            Interlocked.Read(ref _queryCount),
            Interlocked.Read(ref _successfulQueryCount),
            _catchups.ToArray(),
            _measurements.ToArray(),
            _failures.ToArray(),
            Volatile.Read(ref _probe));
    }
}

public sealed record MetricsSnapshot(
    IReadOnlyDictionary<string, long> IndexHits,
    long SnapshotLagCount,
    long PlanMismatchCount,
    long ConnectionLimitCount,
    long PerformanceBudgetCount,
    long QueryCount,
    long SuccessfulQueryCount,
    IReadOnlyList<ReplicaCatchupRecord> Catchups,
    IReadOnlyList<PerformanceMeasurement> Measurements,
    IReadOnlyList<QueryFailureRecord> Failures,
    PerformanceProbe? Probe);
