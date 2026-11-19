using InventoryMigration.Api.Domain;
using InventoryMigration.Api.Options;
using InventoryMigration.Api.Services;
using Microsoft.Extensions.Options;
using Npgsql;

namespace InventoryMigration.Api.Infrastructure;

public sealed class DatabasePool : IAsyncDisposable
{
    private readonly NpgsqlDataSource _dataSource;
    private readonly SemaphoreSlim _slots;
    private readonly int _limit;
    private readonly int _acquireTimeoutMs;
    private readonly int _commandTimeoutSeconds;
    private readonly FaultInjector? _faultInjector;
    private int _inUse;
    private int _waiting;

    public DatabasePool(
        string connectionString,
        int limit,
        int acquireTimeoutMs,
        int commandTimeoutSeconds,
        bool isOldDatabase,
        FaultInjector? faultInjector)
    {
        var builder = new NpgsqlConnectionStringBuilder(connectionString)
        {
            MaxPoolSize = limit,
            Timeout = Math.Max(1, (int)Math.Ceiling(acquireTimeoutMs / 1000.0)),
            CommandTimeout = commandTimeoutSeconds,
            Pooling = true
        };

        _dataSource = new NpgsqlDataSourceBuilder(builder.ConnectionString).Build();
        _slots = new SemaphoreSlim(limit, limit);
        _limit = limit;
        _acquireTimeoutMs = acquireTimeoutMs;
        _commandTimeoutSeconds = commandTimeoutSeconds;
        IsOldDatabase = isOldDatabase;
        _faultInjector = faultInjector;
    }

    public bool IsOldDatabase { get; }

    public DatabasePoolStatus Status => new(
        _limit,
        Volatile.Read(ref _inUse),
        _limit - Volatile.Read(ref _inUse),
        Volatile.Read(ref _waiting));

    public async ValueTask<PooledConnection> AcquireAsync(CancellationToken cancellationToken)
    {
        if (IsOldDatabase && _faultInjector?.OldDbUnavailable == true)
        {
            throw new ServiceException(
                ErrorCodes.Connection,
                "旧库连接不可用（故障注入已启用）",
                StatusCodes.Status503ServiceUnavailable);
        }

        var acquired = _slots.Wait(0);
        if (!acquired)
        {
            Interlocked.Increment(ref _waiting);
            try
            {
                acquired = await _slots.WaitAsync(
                    TimeSpan.FromMilliseconds(_acquireTimeoutMs),
                    cancellationToken).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                throw;
            }
            finally
            {
                Interlocked.Decrement(ref _waiting);
            }
        }

        if (!acquired)
        {
            throw new ServiceException(
                ErrorCodes.Connection,
                "数据库连接池已满或连接超时",
                StatusCodes.Status503ServiceUnavailable);
        }

        NpgsqlConnection? connection = null;
        try
        {
            if (IsOldDatabase && _faultInjector?.OldDbUnavailable == true)
            {
                throw new ServiceException(
                    ErrorCodes.Connection,
                    "旧库连接不可用（故障注入已启用）",
                    StatusCodes.Status503ServiceUnavailable);
            }

            connection = _dataSource.CreateConnection();

            using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
            timeout.CancelAfter(_acquireTimeoutMs);
            await connection.OpenAsync(timeout.Token).ConfigureAwait(false);

            Interlocked.Increment(ref _inUse);
            return new PooledConnection(this, connection);
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            if (connection is not null)
            {
                await connection.DisposeAsync().ConfigureAwait(false);
            }

            _slots.Release();
            throw new ServiceException(
                ErrorCodes.Connection,
                "数据库连接超时",
                StatusCodes.Status503ServiceUnavailable);
        }
        catch (ServiceException)
        {
            if (connection is not null)
            {
                await connection.DisposeAsync().ConfigureAwait(false);
            }

            _slots.Release();
            throw;
        }
        catch (Exception exception) when (exception is NpgsqlException or IOException)
        {
            if (connection is not null)
            {
                await connection.DisposeAsync().ConfigureAwait(false);
            }

            _slots.Release();
            throw new ServiceException(
                ErrorCodes.Connection,
                $"数据库连接失败: {exception.Message}",
                StatusCodes.Status503ServiceUnavailable);
        }
        catch
        {
            if (connection is not null)
            {
                await connection.DisposeAsync().ConfigureAwait(false);
            }

            _slots.Release();
            throw;
        }
    }

    internal void Release()
    {
        Interlocked.Decrement(ref _inUse);
        _slots.Release();
    }

    public async ValueTask DisposeAsync()
    {
        await _dataSource.DisposeAsync().ConfigureAwait(false);
        _slots.Dispose();
    }

    public sealed class PooledConnection : IAsyncDisposable
    {
        private DatabasePool? _owner;

        internal PooledConnection(DatabasePool owner, NpgsqlConnection connection)
        {
            _owner = owner;
            Connection = connection;
        }

        public NpgsqlConnection Connection { get; }

        public async ValueTask DisposeAsync()
        {
            var owner = Interlocked.Exchange(ref _owner, null);
            if (owner is null)
            {
                return;
            }

            await Connection.DisposeAsync().ConfigureAwait(false);
            owner.Release();
        }
    }
}

public sealed record DatabasePoolStatus(int Limit, int InUse, int Available, int Waiting);
