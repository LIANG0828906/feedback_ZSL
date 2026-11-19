namespace OrderRouter;

public enum QueryClass { Interactive, Batch }

/// <summary>
/// Bounds the number of concurrent SQL connections. Interactive (short, realtime)
/// queries may use every slot; batch (long historical) queries are limited to
/// maxConnections - reservedInteractive so short queries always get a slot.
/// Acquisition never waits longer than the configured timeout: on expiry the
/// caller gets a stable E_CONN business error.
/// </summary>
public sealed class ConnectionGate
{
    private readonly SemaphoreSlim _interactiveSlots;
    private readonly SemaphoreSlim _batchSlots;
    private readonly int _acquireTimeoutMs;
    private int _inUse;
    private int _waiting;

    public int Max { get; }
    public int BatchMax { get; }
    public int InUse => Volatile.Read(ref _inUse);
    public int Waiting => Volatile.Read(ref _waiting);

    public ConnectionGate(int maxConnections, int reservedInteractive, int acquireTimeoutMs)
    {
        if (maxConnections < 1) throw new ArgumentOutOfRangeException(nameof(maxConnections));
        Max = maxConnections;
        BatchMax = Math.Max(1, maxConnections - Math.Max(0, reservedInteractive));
        _interactiveSlots = new SemaphoreSlim(maxConnections, maxConnections);
        _batchSlots = new SemaphoreSlim(BatchMax, BatchMax);
        _acquireTimeoutMs = acquireTimeoutMs;
    }

    public async Task<IAsyncDisposable> AcquireAsync(QueryClass cls, CancellationToken ct = default)
    {
        var sem = cls == QueryClass.Batch ? _batchSlots : _interactiveSlots;
        Interlocked.Increment(ref _waiting);
        bool ok;
        try
        {
            ok = await sem.WaitAsync(_acquireTimeoutMs, ct).ConfigureAwait(false);
        }
        finally
        {
            Interlocked.Decrement(ref _waiting);
        }
        if (!ok)
            throw new ConnPoolException(
                $"connection pool exhausted (class={cls}, max={(cls == QueryClass.Batch ? BatchMax : Max)}, timeoutMs={_acquireTimeoutMs})");
        Interlocked.Increment(ref _inUse);
        return new Releaser(this, sem);
    }

    private void Release(SemaphoreSlim sem)
    {
        Interlocked.Decrement(ref _inUse);
        sem.Release();
    }

    private sealed class Releaser : IAsyncDisposable
    {
        private ConnectionGate? _gate;
        private readonly SemaphoreSlim _sem;
        public Releaser(ConnectionGate gate, SemaphoreSlim sem) { _gate = gate; _sem = sem; }
        public ValueTask DisposeAsync()
        {
            var g = Interlocked.Exchange(ref _gate, null);
            g?.Release(_sem);
            return ValueTask.CompletedTask;
        }
    }
}
