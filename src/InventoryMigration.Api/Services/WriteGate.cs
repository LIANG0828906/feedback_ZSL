namespace InventoryMigration.Api.Services;

/// <summary>
/// Admission barrier for routed writes. A cutover waits for all old writes to
/// finish, changes the ledger visibility boundary, then admits new writes.
/// </summary>
public sealed class WriteGate
{
    private readonly object _sync = new();
    private int _active;
    private bool _cutover;
    private TaskCompletionSource? _drained;
    private TaskCompletionSource? _reopened;

    public async ValueTask<Lease> EnterAsync(CancellationToken cancellationToken)
    {
        while (true)
        {
            Task? waitTask = null;
            lock (_sync)
            {
                if (!_cutover)
                {
                    _active++;
                    return new Lease(this);
                }

                _reopened ??= new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
                waitTask = _reopened.Task;
            }

            await waitTask.WaitAsync(cancellationToken).ConfigureAwait(false);
        }
    }

    public async Task<CutoverLease> BeginCutoverAsync(CancellationToken cancellationToken)
    {
        Task drainedTask;
        lock (_sync)
        {
            if (_cutover)
            {
                throw new InvalidOperationException("A cutover is already active.");
            }

            _cutover = true;
            if (_active == 0)
            {
                drainedTask = Task.CompletedTask;
            }
            else
            {
                _drained = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
                drainedTask = _drained.Task;
            }
        }

        try
        {
            await drainedTask.WaitAsync(cancellationToken).ConfigureAwait(false);
            return new CutoverLease(this);
        }
        catch
        {
            EndCutover();
            throw;
        }
    }

    private void Exit()
    {
        TaskCompletionSource? drained = null;
        lock (_sync)
        {
            _active--;
            if (_active == 0)
            {
                drained = _drained;
                _drained = null;
            }
        }

        drained?.TrySetResult();
    }

    private void EndCutover()
    {
        TaskCompletionSource? reopened;
        lock (_sync)
        {
            if (!_cutover)
            {
                return;
            }

            _cutover = false;
            reopened = _reopened;
            _reopened = null;
        }

        reopened?.TrySetResult();
    }

    public sealed class Lease : IDisposable
    {
        private WriteGate? _owner;

        internal Lease(WriteGate owner)
        {
            _owner = owner;
        }

        public void Dispose()
        {
            Interlocked.Exchange(ref _owner, null)?.Exit();
        }
    }

    public sealed class CutoverLease : IDisposable
    {
        private WriteGate? _owner;

        internal CutoverLease(WriteGate owner)
        {
            _owner = owner;
        }

        public void Dispose()
        {
            Interlocked.Exchange(ref _owner, null)?.EndCutover();
        }
    }
}
