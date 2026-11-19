using InventoryMigration.Api.Domain;

namespace InventoryMigration.Api.Services;

public sealed class MetricsRegistry
{
    private long _capturedChanges;
    private long _routedOldDebits;
    private long _routedNewDebits;
    private long _migratingRejections;
    private int _validationMismatchProducts;

    public MetricsRegistry(LatencyTracker latency)
    {
        Latency = latency;
    }

    public LatencyTracker Latency { get; }

    public void SetCapturedChanges(long value) => Interlocked.Exchange(ref _capturedChanges, Math.Max(0, value));

    public void RecordCapturedChanges(int count) => Interlocked.Add(ref _capturedChanges, count);

    public void RecordRoute(ProductRoute route)
    {
        switch (route)
        {
            case ProductRoute.Old:
                Interlocked.Increment(ref _routedOldDebits);
                break;
            case ProductRoute.New:
                Interlocked.Increment(ref _routedNewDebits);
                break;
            case ProductRoute.Migrating:
                Interlocked.Increment(ref _migratingRejections);
                break;
        }
    }

    public void SetValidationMismatchProducts(int value) =>
        Interlocked.Exchange(ref _validationMismatchProducts, Math.Max(0, value));

    public void RecordWriteLatency(TimeSpan elapsed) => Latency.Record(elapsed);

    public ServiceMetricsSnapshot Snapshot(long migratedProducts, long migratedFlows, int rollbackProducts) =>
        new(
            migratedProducts,
            migratedFlows,
            Interlocked.Read(ref _capturedChanges),
            Interlocked.Read(ref _routedOldDebits),
            Interlocked.Read(ref _routedNewDebits),
            Interlocked.Read(ref _migratingRejections),
            Volatile.Read(ref _validationMismatchProducts),
            rollbackProducts,
            Latency.P95Ms());
}
