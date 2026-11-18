using System.Globalization;
using Microsoft.Data.Sqlite;
using SnapshotGateway.Configuration;
using SnapshotGateway.Domain;

namespace SnapshotGateway.Infrastructure;

public sealed class DatabaseInitializer
{
    public const int OrderCount = 20_000;
    private static readonly string[] Statuses = ["NEW", "PROCESSING", "SHIPPED", "CANCELLED"];
    private readonly GatewayOptions _options;
    private readonly SqliteConnectionFactory _factory;
    private readonly ReplicaManager _replicas;

    public DatabaseInitializer(
        GatewayOptions options,
        SqliteConnectionFactory factory,
        ReplicaManager replicas)
    {
        _options = options;
        _factory = factory;
        _replicas = replicas;
    }

    public async Task InitializeAsync(bool reset, CancellationToken cancellationToken = default)
    {
        if (reset)
        {
            DeleteDatabaseFiles(_options.PrimaryDatabasePath);
            for (var replicaId = 1; replicaId <= _options.ReplicaCount; replicaId++)
            {
                DeleteDatabaseFiles(_options.ReplicaDatabasePath(replicaId));
            }
        }

        Directory.CreateDirectory(Path.GetDirectoryName(_options.PrimaryDatabasePath)!);
        await using (var lease = await _factory.OpenAsync(
                         _options.PrimaryDatabasePath, readOnly: false, cancellationToken))
        {
            var connection = lease.Connection;
            await ExecuteAsync(connection, "PRAGMA journal_mode=WAL;", cancellationToken);
            await ExecuteAsync(connection, "PRAGMA synchronous=NORMAL;", cancellationToken);
            await ExecuteAsync(connection, "PRAGMA wal_autocheckpoint=1000;", cancellationToken);
            await ExecuteAsync(connection, CreateSchemaSql, cancellationToken);

            var count = Convert.ToInt64(
                await ScalarAsync(connection, "SELECT COUNT(*) FROM orders;", cancellationToken),
                CultureInfo.InvariantCulture);

            if (count == 0)
            {
                await SeedAsync(connection, cancellationToken);
            }
            else if (count != OrderCount)
            {
                throw new GatewayException(
                    ErrorCodes.Database,
                    $"Existing primary database contains {count} orders; expected {OrderCount}. Run with --reset.",
                    new { actual = count, expected = OrderCount },
                    StatusCodes.Status500InternalServerError);
            }
            else
            {
                await EnsureIndexesAndMetadataAsync(connection, cancellationToken);
            }
        }

        await _replicas.InitializeAllAsync(cancellationToken);
    }

    private static async Task SeedAsync(SqliteConnection connection, CancellationToken cancellationToken)
    {
        var createdAtBase = new DateTimeOffset(2026, 1, 1, 0, 0, 0, TimeSpan.Zero);
        await using var transaction =
            (SqliteTransaction)await connection.BeginTransactionAsync(cancellationToken);
        await using var insert = connection.CreateCommand();
        insert.Transaction = transaction;
        insert.CommandText = """
            INSERT INTO orders(OrderId, TenantId, Status, Amount, UpdatedAt, CreatedAt, LastWriteToken)
            VALUES ($id, $tenant, $status, $amount, $updated, $created, NULL);
            """;
        var idParameter = insert.Parameters.Add("$id", SqliteType.Integer);
        var tenantParameter = insert.Parameters.Add("$tenant", SqliteType.Text);
        var statusParameter = insert.Parameters.Add("$status", SqliteType.Text);
        var amountParameter = insert.Parameters.Add("$amount", SqliteType.Integer);
        var updatedParameter = insert.Parameters.Add("$updated", SqliteType.Text);
        var createdParameter = insert.Parameters.Add("$created", SqliteType.Text);

        for (var orderId = 1; orderId <= OrderCount; orderId++)
        {
            var tenant = ((orderId - 1) % 20) + 1;
            var status = Statuses[((orderId - 1) / 20) % Statuses.Length];
            var updatedAt = createdAtBase
                .AddSeconds((orderId * 7919L) % 1_500_000L)
                .ToString("O", CultureInfo.InvariantCulture);
            var createdAt = createdAtBase
                .AddSeconds(orderId)
                .ToString("O", CultureInfo.InvariantCulture);

            idParameter.Value = orderId;
            tenantParameter.Value = $"T{tenant:D3}";
            statusParameter.Value = status;
            amountParameter.Value = (orderId * 37L) % 10_000L;
            updatedParameter.Value = updatedAt;
            createdParameter.Value = createdAt;
            await insert.ExecuteNonQueryAsync(cancellationToken);
        }

        await transaction.CommitAsync(cancellationToken);
        await EnsureIndexesAndMetadataAsync(connection, cancellationToken);
        await ExecuteAsync(connection, "PRAGMA wal_checkpoint(FULL);", cancellationToken);
    }

    private static async Task EnsureIndexesAndMetadataAsync(
        SqliteConnection connection,
        CancellationToken cancellationToken)
    {
        await ExecuteAsync(connection, CreateIndexesSql, cancellationToken);
        var now = DateTimeOffset.UtcNow.ToString("O", CultureInfo.InvariantCulture);
        await ExecuteAsync(connection, $"""
            INSERT INTO app_metadata(key, value) VALUES
                ('data_snapshot_version', '1'),
                ('control_version', '1'),
                ('active_generation', 'OLD'),
                ('old_index_state', 'ACTIVE'),
                ('new_index_state', 'READY'),
                ('schema_version', '1'),
                ('seed_version', 'deterministic-20000-v1')
            ON CONFLICT(key) DO NOTHING;

            INSERT INTO index_catalog(key_order, index_name, columns, state, enabled, updated_at)
            VALUES
                ('old', 'idx_old_status_orderid', 'Status,OrderId', 'ACTIVE', 1, '{now}'),
                ('new', 'idx_new_tenant_status_updated_orderid',
                 'TenantId,Status,UpdatedAt,OrderId', 'READY', 1, '{now}')
            ON CONFLICT(key_order) DO NOTHING;
            """, cancellationToken);
        await ExecuteAsync(connection, "ANALYZE;", cancellationToken);
    }

    private static void DeleteDatabaseFiles(string databasePath)
    {
        foreach (var path in new[] { databasePath, databasePath + "-wal", databasePath + "-shm" })
        {
            if (File.Exists(path))
            {
                File.Delete(path);
            }
        }
    }

    private static async Task ExecuteAsync(
        SqliteConnection connection,
        string sql,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.CommandText = sql;
        await command.ExecuteNonQueryAsync(cancellationToken);
    }

    private static async Task<object?> ScalarAsync(
        SqliteConnection connection,
        string sql,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.CommandText = sql;
        return await command.ExecuteScalarAsync(cancellationToken);
    }

    private const string CreateSchemaSql = """
        CREATE TABLE IF NOT EXISTS orders (
            OrderId INTEGER PRIMARY KEY,
            TenantId TEXT NOT NULL,
            Status TEXT NOT NULL,
            Amount INTEGER NOT NULL,
            UpdatedAt TEXT NOT NULL,
            CreatedAt TEXT NOT NULL,
            LastWriteToken TEXT NULL
        );

        CREATE TABLE IF NOT EXISTS app_metadata (
            key TEXT PRIMARY KEY,
            value TEXT NOT NULL
        );

        CREATE TABLE IF NOT EXISTS index_catalog (
            key_order TEXT PRIMARY KEY,
            index_name TEXT NOT NULL,
            columns TEXT NOT NULL,
            state TEXT NOT NULL,
            enabled INTEGER NOT NULL,
            updated_at TEXT NOT NULL
        );

        CREATE TABLE IF NOT EXISTS switch_audit (
            audit_id INTEGER PRIMARY KEY AUTOINCREMENT,
            operation TEXT NOT NULL,
            old_index_state TEXT NOT NULL,
            new_index_state TEXT NOT NULL,
            snapshot_version INTEGER NOT NULL,
            result TEXT NOT NULL,
            details TEXT NULL,
            created_at TEXT NOT NULL
        );
        """;

    private const string CreateIndexesSql = """
        CREATE INDEX IF NOT EXISTS idx_old_status_orderid
            ON orders(Status, OrderId);
        CREATE INDEX IF NOT EXISTS idx_new_tenant_status_updated_orderid
            ON orders(TenantId, Status, UpdatedAt, OrderId);
        """;
}
