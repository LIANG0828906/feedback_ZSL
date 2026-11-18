using System.Diagnostics;
using System.Globalization;
using Microsoft.Data.Sqlite;
using SnapshotGateway.Configuration;
using SnapshotGateway.Domain;
using SnapshotGateway.Services;

namespace SnapshotGateway.Infrastructure;

public sealed record ReplicaDescriptor(int Id, string Name, string DatabasePath);

public sealed class ReplicaManager
{
    private readonly GatewayOptions _options;
    private readonly SqliteConnectionFactory _factory;
    private readonly GatewayMetrics _metrics;
    private readonly ReplicaDescriptor[] _replicas;
    private readonly SemaphoreSlim[] _replicaLocks;
    private readonly SemaphoreSlim _catchupGate = new(1, 1);
    private int _roundRobin = -1;

    public ReplicaManager(
        GatewayOptions options,
        SqliteConnectionFactory factory,
        GatewayMetrics metrics)
    {
        _options = options;
        _factory = factory;
        _metrics = metrics;
        _replicas = Enumerable.Range(1, options.ReplicaCount)
            .Select(id => new ReplicaDescriptor(
                id,
                $"replica-{id}",
                options.ReplicaDatabasePath(id)))
            .ToArray();
        _replicaLocks = Enumerable.Range(0, options.ReplicaCount)
            .Select(_ => new SemaphoreSlim(1, 1))
            .ToArray();
    }

    public IReadOnlyList<ReplicaDescriptor> Replicas => _replicas;

    public int NextReplicaId()
    {
        if (_replicas.Length == 1)
        {
            return _replicas[0].Id;
        }

        var index = Interlocked.Increment(ref _roundRobin);
        return _replicas[(int)((uint)index % (uint)_replicas.Length)].Id;
    }

    public async Task InitializeAllAsync(CancellationToken cancellationToken = default)
    {
        foreach (var replica in _replicas)
        {
            Directory.CreateDirectory(Path.GetDirectoryName(replica.DatabasePath)!);
        }

        var result = await CatchUpAllAsync(await ReadPrimaryDataVersionAsync(cancellationToken), cancellationToken);
        if (!result.Completed)
        {
            throw new GatewayException(
                ErrorCodes.Database,
                "Failed to initialize replica snapshots.",
                result,
                StatusCodes.Status500InternalServerError);
        }
    }

    public async Task<long> ReadPrimaryDataVersionAsync(CancellationToken cancellationToken = default)
    {
        await using var lease = await _factory.OpenAsync(
            _options.PrimaryDatabasePath, readOnly: true, cancellationToken);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText =
            "SELECT CAST(value AS INTEGER) FROM app_metadata WHERE key='data_snapshot_version';";
        var value = await command.ExecuteScalarAsync(cancellationToken);
        return value is null or DBNull ? -1 : Convert.ToInt64(value, CultureInfo.InvariantCulture);
    }

    public async Task<ReplicaVersionInfo> ReadVersionAsync(
        ReplicaDescriptor replica,
        CancellationToken cancellationToken = default)
    {
        await using var replicaLease = await AcquireReplicaLockAsync(replica.Id, cancellationToken);
        return await ReadVersionUnderLockAsync(replica, cancellationToken);
    }

    public async Task<IReadOnlyList<ReplicaVersionInfo>> ReadAllVersionsAsync(
        CancellationToken cancellationToken = default)
    {
        var versions = new List<ReplicaVersionInfo>(_replicas.Length);
        foreach (var replica in _replicas)
        {
            versions.Add(await ReadVersionAsync(replica, cancellationToken));
        }

        return versions;
    }

    public async Task<ReplicaLockHandle> AcquireReplicaLockAsync(
        int replicaId,
        CancellationToken cancellationToken)
    {
        var index = replicaId - 1;
        if (index < 0 || index >= _replicaLocks.Length)
        {
            throw new GatewayException(
                ErrorCodes.Validation,
                $"Replica id must be between 1 and {_replicas.Length}.",
                new { replicaId },
                StatusCodes.Status400BadRequest);
        }

        await _replicaLocks[index].WaitAsync(cancellationToken);
        return new ReplicaLockHandle(_replicaLocks[index]);
    }

    public ReplicaDescriptor GetReplica(int replicaId)
    {
        return _replicas.FirstOrDefault(replica => replica.Id == replicaId)
            ?? throw new GatewayException(
                ErrorCodes.Validation,
                $"Replica id must be between 1 and {_replicas.Length}.",
                new { replicaId },
                StatusCodes.Status400BadRequest);
    }

    public async Task<CatchupResult> CatchUpAllAsync(
        long targetVersion,
        CancellationToken cancellationToken = default)
    {
        if (!await _catchupGate.WaitAsync(0, cancellationToken))
        {
            return new CatchupResult(
                Started: false,
                Completed: false,
                targetVersion,
                0,
                Array.Empty<ReplicaCatchupRecord>(),
                await ReadAllVersionsAsync(cancellationToken));
        }

        var total = Stopwatch.StartNew();
        var records = new List<ReplicaCatchupRecord>();
        string? error = null;
        try
        {
            await using var source = await _factory.OpenAsync(
                _options.PrimaryDatabasePath, readOnly: true, cancellationToken);

            foreach (var replica in _replicas)
            {
                var record = await SyncOneAsync(source.Connection, replica, targetVersion, cancellationToken);
                records.Add(record);
                _metrics.RecordCatchup(record);
                if (!record.Succeeded)
                {
                    error = record.Error;
                }
            }
        }
        catch (GatewayException ex) when (ex.Code == ErrorCodes.ConnectionLimit)
        {
            throw;
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            error = ex.Message;
        }
        finally
        {
            _catchupGate.Release();
        }

        total.Stop();
        IReadOnlyList<ReplicaVersionInfo> after;
        try
        {
            after = await ReadAllVersionsAsync(cancellationToken);
        }
        catch (Exception ex)
        {
            after = Array.Empty<ReplicaVersionInfo>();
            error ??= ex.Message;
        }

        var completed = error is null
                        && after.Count == _replicas.Length
                        && after.All(version => version.SnapshotVersion == targetVersion);
        return new CatchupResult(
            Started: true,
            Completed: completed,
            targetVersion,
            total.Elapsed.TotalMilliseconds,
            records,
            after,
            error);
    }

    private async Task<ReplicaCatchupRecord> SyncOneAsync(
        SqliteConnection source,
        ReplicaDescriptor replica,
        long targetVersion,
        CancellationToken cancellationToken)
    {
        var stopwatch = Stopwatch.StartNew();
        long appliedVersion = -1;
        try
        {
            await using var replicaLease = await AcquireReplicaLockAsync(replica.Id, cancellationToken);
            await using (var destination = await _factory.OpenAsync(
                             replica.DatabasePath, readOnly: false, cancellationToken))
            {
                source.BackupDatabase(destination.Connection);
                await using var configure = destination.Connection.CreateCommand();
                configure.CommandText = """
                    CREATE TABLE IF NOT EXISTS replica_meta (
                        replica_id INTEGER PRIMARY KEY,
                        snapshot_version INTEGER NOT NULL,
                        applied_at_utc TEXT NOT NULL
                    );
                    PRAGMA journal_mode=WAL;
                    """;
                await configure.ExecuteNonQueryAsync(cancellationToken);

                await using var readVersion = destination.Connection.CreateCommand();
                readVersion.CommandText =
                    "SELECT CAST(value AS INTEGER) FROM app_metadata WHERE key='data_snapshot_version';";
                appliedVersion = Convert.ToInt64(
                    await readVersion.ExecuteScalarAsync(cancellationToken),
                    CultureInfo.InvariantCulture);

                await using var writeMeta = destination.Connection.CreateCommand();
                writeMeta.CommandText = """
                    INSERT INTO replica_meta(replica_id, snapshot_version, applied_at_utc)
                    VALUES ($id, $version, $at)
                    ON CONFLICT(replica_id) DO UPDATE SET
                        snapshot_version=excluded.snapshot_version,
                        applied_at_utc=excluded.applied_at_utc;
                    """;
                writeMeta.Parameters.AddWithValue("$id", replica.Id);
                writeMeta.Parameters.AddWithValue("$version", appliedVersion);
                writeMeta.Parameters.AddWithValue(
                    "$at", DateTimeOffset.UtcNow.ToString("O", CultureInfo.InvariantCulture));
                await writeMeta.ExecuteNonQueryAsync(cancellationToken);
            }

            stopwatch.Stop();
            return new ReplicaCatchupRecord(
                replica.Id,
                targetVersion,
                appliedVersion,
                stopwatch.Elapsed.TotalMilliseconds,
                Succeeded: true,
                Error: null,
                DateTimeOffset.UtcNow);
        }
        catch (GatewayException ex) when (ex.Code == ErrorCodes.ConnectionLimit)
        {
            throw;
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            stopwatch.Stop();
            return new ReplicaCatchupRecord(
                replica.Id,
                targetVersion,
                appliedVersion,
                stopwatch.Elapsed.TotalMilliseconds,
                Succeeded: false,
                ex.Message,
                DateTimeOffset.UtcNow);
        }
    }

    private async Task<ReplicaVersionInfo> ReadVersionUnderLockAsync(
        ReplicaDescriptor replica,
        CancellationToken cancellationToken)
    {
        if (!File.Exists(replica.DatabasePath))
        {
            return new ReplicaVersionInfo(
                replica.Id, replica.Name, replica.DatabasePath, -1, "MISSING");
        }

        try
        {
            await using var lease = await _factory.OpenAsync(
                replica.DatabasePath, readOnly: true, cancellationToken);
            await using var command = lease.Connection.CreateCommand();
            command.CommandText = """
                SELECT CAST(value AS INTEGER)
                FROM app_metadata
                WHERE key='data_snapshot_version';
                """;
            var value = await command.ExecuteScalarAsync(cancellationToken);
            var version = value is null or DBNull
                ? -1
                : Convert.ToInt64(value, CultureInfo.InvariantCulture);
            return new ReplicaVersionInfo(
                replica.Id, replica.Name, replica.DatabasePath, version, "READY");
        }
        catch (GatewayException ex) when (ex.Code == ErrorCodes.ConnectionLimit)
        {
            throw;
        }
        catch (Exception ex)
        {
            return new ReplicaVersionInfo(
                replica.Id, replica.Name, replica.DatabasePath, -1, $"ERROR: {ex.Message}");
        }
    }

    public sealed class ReplicaLockHandle : IAsyncDisposable, IDisposable
    {
        private SemaphoreSlim? _semaphore;

        public ReplicaLockHandle(SemaphoreSlim semaphore)
        {
            _semaphore = semaphore;
        }

        public void Dispose() => Interlocked.Exchange(ref _semaphore, null)?.Release();

        public ValueTask DisposeAsync()
        {
            Dispose();
            return ValueTask.CompletedTask;
        }
    }
}
