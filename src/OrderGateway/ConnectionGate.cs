using System.Diagnostics;
using Microsoft.Data.SqlClient;

namespace OrderGateway;

/// <summary>
/// Bounds the number of concurrently open physical SQL connections.
/// Long and short queries compete for the same slots; a request that cannot
/// acquire one inside the configured window fails fast with E_CONN_TIMEOUT
/// instead of queueing forever and starving callers.
/// </summary>
public sealed class ConnectionGate : IDisposable
{
    private readonly SemaphoreSlim _slots;
    private readonly GatewayOptions _options;
    private int _peakLeased;
    private int _leased;
    private int _totalAcquisitions;
    private int _acquireTimeouts;

    public ConnectionGate(GatewayOptions options)
    {
        _options = options;
        _slots = new SemaphoreSlim(options.MaxConcurrentConnections, options.MaxConcurrentConnections);
    }

    public int PeakLeased => Volatile.Read(ref _peakLeased);
    public int CurrentlyLeased => Volatile.Read(ref _leased);
    public int TotalAcquisitions => Volatile.Read(ref _totalAcquisitions);
    public int AcquireTimeouts => Volatile.Read(ref _acquireTimeouts);

    public Task<LeasedConnection> LeaseAsync(CancellationToken cancellationToken = default)
        => LeaseAsync(null, cancellationToken);

    /// <summary>Lease with a caller-specific slot wait used by starvation probes.</summary>
    public async Task<LeasedConnection> LeaseAsync(
        TimeSpan? acquireTimeout,
        CancellationToken cancellationToken = default)
    {
        var wait = acquireTimeout ?? _options.ConnectionAcquireTimeout;
        var sw = Stopwatch.StartNew();
        bool acquired;
        try
        {
            acquired = await _slots.WaitAsync(wait, cancellationToken).ConfigureAwait(false);
        }
        catch (OperationCanceledException)
        {
            throw;
        }

        if (!acquired)
        {
            Interlocked.Increment(ref _acquireTimeouts);
            throw new GatewayException(
                GatewayError.ConnectionTimeout,
                $"Connection slot not acquired within {wait.TotalMilliseconds}ms",
                new Dictionary<string, object?>
                {
                    ["maxConcurrentConnections"] = _options.MaxConcurrentConnections,
                    ["currentlyLeased"] = CurrentlyLeased,
                    ["acquireTimeoutMs"] = wait.TotalMilliseconds,
                });
        }

        var current = Interlocked.Increment(ref _leased);
        Interlocked.Increment(ref _totalAcquisitions);
        UpdatePeak(current);

        SqlConnection connection;
        try
        {
            connection = new SqlConnection(_options.ConnectionString);
            await connection.OpenAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (OperationCanceledException)
        {
            Interlocked.Decrement(ref _leased);
            _slots.Release();
            throw;
        }
        catch (Exception ex) when (ex is SqlException or TimeoutException)
        {
            Interlocked.Decrement(ref _leased);
            _slots.Release();
            var context = new Dictionary<string, object?>
            {
                ["maxConcurrentConnections"] = _options.MaxConcurrentConnections,
                ["connectTimeoutMs"] = _options.ConnectionAcquireTimeout.TotalMilliseconds,
                ["failureKind"] = ex.GetType().Name,
            };
            throw new GatewayException(
                GatewayError.ConnectionTimeout,
                "SQL connection could not be opened within the configured window",
                context,
                ex);
        }
        catch
        {
            Interlocked.Decrement(ref _leased);
            _slots.Release();
            throw;
        }

        return new LeasedConnection(this, connection, sw);
    }

    private void UpdatePeak(int current)
    {
        int peak;
        do
        {
            peak = Volatile.Read(ref _peakLeased);
            if (current <= peak) return;
        } while (Interlocked.CompareExchange(ref _peakLeased, current, peak) != peak);
    }

    private void Release(LeasedConnection lease)
    {
        try
        {
            lease.Connection.Dispose();
        }
        finally
        {
            Interlocked.Decrement(ref _leased);
            _slots.Release();
        }
    }

    public void Dispose() => _slots.Dispose();

    public sealed class LeasedConnection : IAsyncDisposable
    {
        private readonly ConnectionGate _owner;
        private int _disposed;

        internal LeasedConnection(ConnectionGate owner, SqlConnection connection, Stopwatch leaseStopwatch)
        {
            _owner = owner;
            Connection = connection;
            LeaseStopwatch = leaseStopwatch;
        }

        public SqlConnection Connection { get; }
        public Stopwatch LeaseStopwatch { get; }

        public async ValueTask DisposeAsync()
        {
            if (Interlocked.Exchange(ref _disposed, 1) != 0) return;
            await Connection.CloseAsync().ConfigureAwait(false);
            _owner.Release(this);
        }
    }
}
