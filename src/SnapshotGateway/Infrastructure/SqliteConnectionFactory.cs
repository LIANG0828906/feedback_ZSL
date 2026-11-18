using Microsoft.Data.Sqlite;
using SnapshotGateway.Domain;

namespace SnapshotGateway.Infrastructure;

public sealed class ConnectionGate
{
    private readonly SemaphoreSlim _semaphore;

    public ConnectionGate(int maxConnections)
    {
        MaxConnections = Math.Max(1, maxConnections);
        _semaphore = new SemaphoreSlim(MaxConnections, MaxConnections);
    }

    public int MaxConnections { get; }

    public async Task<ConnectionGateLease> AcquireAsync(CancellationToken cancellationToken)
    {
        if (!await _semaphore.WaitAsync(0, cancellationToken))
        {
            throw new GatewayException(
                ErrorCodes.ConnectionLimit,
                $"SQLite concurrent connection limit {MaxConnections} reached.",
                new { maxConnections = MaxConnections },
                StatusCodes.Status429TooManyRequests);
        }

        return new ConnectionGateLease(_semaphore);
    }

    public sealed class ConnectionGateLease : IDisposable
    {
        private SemaphoreSlim? _semaphore;

        public ConnectionGateLease(SemaphoreSlim semaphore)
        {
            _semaphore = semaphore;
        }

        public void Dispose()
        {
            Interlocked.Exchange(ref _semaphore, null)?.Release();
        }
    }
}

public sealed class SqliteConnectionFactory
{
    private readonly ConnectionGate _gate;

    public SqliteConnectionFactory(ConnectionGate gate)
    {
        _gate = gate;
    }

    public int MaxConnections => _gate.MaxConnections;

    public async Task<SqliteLease> OpenAsync(
        string databasePath,
        bool readOnly,
        CancellationToken cancellationToken = default)
    {
        var gateLease = await _gate.AcquireAsync(cancellationToken);
        var directory = Path.GetDirectoryName(databasePath);
        if (!string.IsNullOrEmpty(directory))
        {
            Directory.CreateDirectory(directory);
        }

        var builder = new SqliteConnectionStringBuilder
        {
            DataSource = databasePath,
            Mode = readOnly && File.Exists(databasePath) ? SqliteOpenMode.ReadOnly : SqliteOpenMode.ReadWriteCreate,
            Cache = SqliteCacheMode.Private,
            Pooling = false,
            DefaultTimeout = 10
        };

        var connection = new SqliteConnection(builder.ToString());
        try
        {
            await connection.OpenAsync(cancellationToken);
            await using var command = connection.CreateCommand();
            command.CommandText = "PRAGMA busy_timeout=10000; PRAGMA temp_store=MEMORY;";
            if (readOnly)
            {
                command.CommandText += " PRAGMA query_only=ON;";
            }

            await command.ExecuteNonQueryAsync(cancellationToken);
            return new SqliteLease(connection, gateLease);
        }
        catch (SqliteException ex)
        {
            await connection.DisposeAsync();
            gateLease.Dispose();
            throw new GatewayException(
                ErrorCodes.Database,
                $"Unable to open SQLite database '{databasePath}': {ex.Message}",
                new { sqliteErrorCode = ex.SqliteErrorCode },
                StatusCodes.Status500InternalServerError);
        }
        catch
        {
            await connection.DisposeAsync();
            gateLease.Dispose();
            throw;
        }
    }
}

public sealed class SqliteLease : IAsyncDisposable, IDisposable
{
    private readonly SqliteConnection _connection;
    private readonly ConnectionGate.ConnectionGateLease _gateLease;
    private bool _disposed;

    public SqliteLease(SqliteConnection connection, ConnectionGate.ConnectionGateLease gateLease)
    {
        _connection = connection;
        _gateLease = gateLease;
    }

    public SqliteConnection Connection => _connection;

    public void Dispose()
    {
        if (_disposed)
        {
            return;
        }

        _disposed = true;
        _connection.Dispose();
        _gateLease.Dispose();
    }

    public async ValueTask DisposeAsync()
    {
        if (_disposed)
        {
            return;
        }

        _disposed = true;
        await _connection.DisposeAsync();
        _gateLease.Dispose();
    }
}
