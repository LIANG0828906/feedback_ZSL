namespace OrderGateway;

/// <summary>
/// Writer-preferring async gate shared by readers and index DDL. A query
/// enters the read side before it leases a SQL connection; switch/rollback
/// enters the write side before running DDL. Lock order is always barrier then
/// connection, and a waiting writer blocks new readers so retirement cannot be
/// starved while in-flight reads are allowed to finish.
/// </summary>
public sealed class IndexChangeBarrier
{
    private readonly SemaphoreSlim _turnstile = new(1, 1);
    private readonly SemaphoreSlim _roomEmpty = new(1, 1);
    private int _readers;

    public async ValueTask<IAsyncDisposable> EnterReadAsync(CancellationToken cancellationToken = default)
    {
        await _turnstile.WaitAsync(cancellationToken).ConfigureAwait(false);
        try
        {
            if (Interlocked.Increment(ref _readers) == 1)
                await _roomEmpty.WaitAsync(cancellationToken).ConfigureAwait(false);
        }
        catch
        {
            if (Interlocked.Decrement(ref _readers) == 0)
                _roomEmpty.Release();
            throw;
        }
        finally
        {
            _turnstile.Release();
        }

        return new Releaser(this);
    }

    public async ValueTask<IAsyncDisposable> EnterWriteAsync(CancellationToken cancellationToken = default)
    {
        await _turnstile.WaitAsync(cancellationToken).ConfigureAwait(false);
        try
        {
            await _roomEmpty.WaitAsync(cancellationToken).ConfigureAwait(false);
        }
        catch
        {
            _turnstile.Release();
            throw;
        }

        return new Releaser(this, isWriter: true);
    }

    private void ExitRead()
    {
        if (Interlocked.Decrement(ref _readers) == 0)
            _roomEmpty.Release();
    }

    private void ExitWrite()
    {
        _roomEmpty.Release();
        _turnstile.Release();
    }

    private sealed class Releaser : IAsyncDisposable
    {
        private readonly IndexChangeBarrier _owner;
        private readonly bool _isWriter;
        private int _disposed;

        public Releaser(IndexChangeBarrier owner, bool isWriter = false)
        {
            _owner = owner;
            _isWriter = isWriter;
        }

        public ValueTask DisposeAsync()
        {
            if (Interlocked.Exchange(ref _disposed, 1) == 0)
            {
                if (_isWriter) _owner.ExitWrite();
                else _owner.ExitRead();
            }
            return ValueTask.CompletedTask;
        }
    }
}
