using System.Globalization;
using System.Text.Json;
using Microsoft.Data.Sqlite;
using SnapshotGateway.Configuration;
using SnapshotGateway.Domain;
using SnapshotGateway.Infrastructure;

namespace SnapshotGateway.Services;

public sealed class IndexStateService
{
    private readonly GatewayOptions _options;
    private readonly SqliteConnectionFactory _factory;
    private readonly PrimaryWriteGate _writeGate;
    private readonly SnapshotCoordinator _coordinator;

    public IndexStateService(
        GatewayOptions options,
        SqliteConnectionFactory factory,
        PrimaryWriteGate writeGate,
        SnapshotCoordinator coordinator)
    {
        _options = options;
        _factory = factory;
        _writeGate = writeGate;
        _coordinator = coordinator;
    }

    public async Task<IndexStateSnapshot> GetStateAsync(CancellationToken cancellationToken = default)
    {
        await using var lease = await _factory.OpenAsync(
            _options.PrimaryDatabasePath, readOnly: true, cancellationToken);
        return await ReadStateAsync(lease.Connection, null, cancellationToken);
    }

    public Task<TransitionResult> SwitchToNewAsync(CancellationToken cancellationToken = default) =>
        TransitionAsync(rollback: false, cancellationToken);

    public Task<TransitionResult> RollbackAsync(CancellationToken cancellationToken = default) =>
        TransitionAsync(rollback: true, cancellationToken);

    public async Task<IReadOnlyList<AuditRecord>> GetAuditAsync(
        int limit = 100,
        CancellationToken cancellationToken = default)
    {
        await using var lease = await _factory.OpenAsync(
            _options.PrimaryDatabasePath, readOnly: true, cancellationToken);
        await using var command = lease.Connection.CreateCommand();
        command.CommandText = """
            SELECT audit_id, operation, old_index_state, new_index_state,
                   snapshot_version, result, details, created_at
            FROM switch_audit
            ORDER BY audit_id DESC
            LIMIT $limit;
            """;
        command.Parameters.AddWithValue("$limit", Math.Clamp(limit, 1, 1_000));
        var records = new List<AuditRecord>();
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            records.Add(new AuditRecord(
                reader.GetInt64(0),
                reader.GetString(1),
                reader.GetString(2),
                reader.GetString(3),
                reader.GetInt64(4),
                reader.GetString(5),
                reader.IsDBNull(6) ? null : reader.GetString(6),
                reader.GetString(7)));
        }

        return records;
    }

    private async Task<TransitionResult> TransitionAsync(
        bool rollback,
        CancellationToken cancellationToken)
    {
        using var writeLease = await _writeGate.AcquireAsync(cancellationToken);
        await using var lease = await _factory.OpenAsync(
            _options.PrimaryDatabasePath, readOnly: false, cancellationToken);
        var connection = lease.Connection;
        await using var transaction = connection.BeginTransaction(
            System.Data.IsolationLevel.Serializable,
            deferred: false);

        var before = await ReadStateAsync(connection, transaction, cancellationToken);
        var operation = rollback ? "ROLLBACK" : "SWITCH";
        var now = DateTimeOffset.UtcNow.ToString("O", CultureInfo.InvariantCulture);

        var valid = rollback
            ? before.ActiveGeneration == "NEW"
            : before.ActiveGeneration == "OLD";
        if (!valid)
        {
            var reason = rollback
                ? "Rollback is allowed only while the new index generation is active."
                : "Switch is allowed only while the old index generation is active.";
            var details = JsonSerializer.Serialize(new
            {
                reason,
                before.ActiveGeneration,
                before.OldIndexState,
                before.NewIndexState
            });
            var rejectedAuditId = await InsertAuditAsync(
                connection,
                transaction,
                operation,
                before.OldIndexState,
                before.NewIndexState,
                before.DataSnapshotVersion,
                "REJECTED",
                details,
                now,
                cancellationToken);
            await transaction.CommitAsync(cancellationToken);
            return new TransitionResult
            {
                Success = false,
                ErrorCode = ErrorCodes.StateConflict,
                Message = reason,
                Operation = operation,
                AuditId = rejectedAuditId,
                SnapshotVersion = before.DataSnapshotVersion,
                ControlVersion = before.ControlVersion,
                State = before
            };
        }

        var oldState = rollback ? "ACTIVE" : "ACTIVE_LEGACY";
        var newState = rollback ? "READY" : "ACTIVE";
        var generation = rollback ? "OLD" : "NEW";
        var controlVersion = before.ControlVersion + 1;

        await using (var updateCatalog = connection.CreateCommand())
        {
            updateCatalog.Transaction = transaction;
            updateCatalog.CommandText = """
                UPDATE index_catalog
                SET state=$oldState, updated_at=$now
                WHERE key_order='old';
                UPDATE index_catalog
                SET state=$newState, updated_at=$now
                WHERE key_order='new';
                """;
            updateCatalog.Parameters.AddWithValue("$oldState", oldState);
            updateCatalog.Parameters.AddWithValue("$newState", newState);
            updateCatalog.Parameters.AddWithValue("$now", now);
            await updateCatalog.ExecuteNonQueryAsync(cancellationToken);
        }

        await using (var updateMetadata = connection.CreateCommand())
        {
            updateMetadata.Transaction = transaction;
            updateMetadata.CommandText = """
                UPDATE app_metadata SET value=$oldState WHERE key='old_index_state';
                UPDATE app_metadata SET value=$newState WHERE key='new_index_state';
                UPDATE app_metadata SET value=$generation WHERE key='active_generation';
                UPDATE app_metadata SET value=$controlVersion WHERE key='control_version';
                """;
            updateMetadata.Parameters.AddWithValue("$oldState", oldState);
            updateMetadata.Parameters.AddWithValue("$newState", newState);
            updateMetadata.Parameters.AddWithValue("$generation", generation);
            updateMetadata.Parameters.AddWithValue(
                "$controlVersion", controlVersion.ToString(CultureInfo.InvariantCulture));
            await updateMetadata.ExecuteNonQueryAsync(cancellationToken);
        }

        var auditDetails = JsonSerializer.Serialize(new
        {
            before = new
            {
                before.ActiveGeneration,
                before.OldIndexState,
                before.NewIndexState,
                before.ControlVersion
            },
            after = new
            {
                ActiveGeneration = generation,
                OldIndexState = oldState,
                NewIndexState = newState,
                ControlVersion = controlVersion
            },
            indexNames = new { before.OldIndexName, before.NewIndexName },
            snapshotVersionUnchanged = true
        });
        var auditId = await InsertAuditAsync(
            connection,
            transaction,
            operation,
            oldState,
            newState,
            before.DataSnapshotVersion,
            "COMMITTED",
            auditDetails,
            now,
            cancellationToken);
        await transaction.CommitAsync(cancellationToken);

        var reconcile = await _coordinator.ReconcileAfterTransitionAsync(cancellationToken);
        var state = before with
        {
            ActiveGeneration = generation,
            OldIndexState = oldState,
            NewIndexState = newState,
            ControlVersion = controlVersion
        };
        return new TransitionResult
        {
            Success = true,
            Operation = operation,
            AuditId = auditId,
            SnapshotVersion = before.DataSnapshotVersion,
            ControlVersion = controlVersion,
            State = state,
            ReplicaVersions = reconcile.Versions,
            ReplicaCatchupMs = reconcile.CatchupDurationMs,
            WarningCode = reconcile.Aligned ? null : ErrorCodes.SnapshotLag,
            WarningMessage = reconcile.Aligned ? null : reconcile.Error
        };
    }

    private static async Task<IndexStateSnapshot> ReadStateAsync(
        SqliteConnection connection,
        SqliteTransaction? transaction,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.Transaction = transaction;
        command.CommandText = """
            SELECT
                MAX(CASE WHEN key='active_generation' THEN value END),
                MAX(CASE WHEN key='old_index_state' THEN value END),
                MAX(CASE WHEN key='new_index_state' THEN value END),
                MAX(CASE WHEN key='data_snapshot_version' THEN CAST(value AS INTEGER) END),
                MAX(CASE WHEN key='control_version' THEN CAST(value AS INTEGER) END)
            FROM app_metadata;
            """;
        string activeGeneration;
        string oldState;
        string newState;
        long snapshotVersion;
        long controlVersion;
        await using (var reader = await command.ExecuteReaderAsync(cancellationToken))
        {
            if (!await reader.ReadAsync(cancellationToken))
            {
                throw new GatewayException(
                    ErrorCodes.Database,
                    "Index metadata is missing.",
                    null,
                    StatusCodes.Status500InternalServerError);
            }

            activeGeneration = reader.GetString(0);
            oldState = reader.GetString(1);
            newState = reader.GetString(2);
            snapshotVersion = reader.GetInt64(3);
            controlVersion = reader.GetInt64(4);
        }

        string oldIndexName;
        string newIndexName;
        await using (var indexes = connection.CreateCommand())
        {
            indexes.Transaction = transaction;
            indexes.CommandText = """
                SELECT
                    MAX(CASE WHEN key_order='old' THEN index_name END),
                    MAX(CASE WHEN key_order='new' THEN index_name END)
                FROM index_catalog;
                """;
            await using var reader = await indexes.ExecuteReaderAsync(cancellationToken);
            await reader.ReadAsync(cancellationToken);
            oldIndexName = reader.GetString(0);
            newIndexName = reader.GetString(1);
        }

        return new IndexStateSnapshot(
            activeGeneration,
            oldState,
            newState,
            oldIndexName,
            newIndexName,
            snapshotVersion,
            controlVersion);
    }

    private static async Task<long> InsertAuditAsync(
        SqliteConnection connection,
        SqliteTransaction transaction,
        string operation,
        string oldState,
        string newState,
        long snapshotVersion,
        string result,
        string details,
        string now,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.Transaction = transaction;
        command.CommandText = """
            INSERT INTO switch_audit(
                operation, old_index_state, new_index_state, snapshot_version,
                result, details, created_at)
            VALUES($operation, $oldState, $newState, $snapshotVersion, $result, $details, $now);
            SELECT last_insert_rowid();
            """;
        command.Parameters.AddWithValue("$operation", operation);
        command.Parameters.AddWithValue("$oldState", oldState);
        command.Parameters.AddWithValue("$newState", newState);
        command.Parameters.AddWithValue("$snapshotVersion", snapshotVersion);
        command.Parameters.AddWithValue("$result", result);
        command.Parameters.AddWithValue("$details", details);
        command.Parameters.AddWithValue("$now", now);
        return Convert.ToInt64(await command.ExecuteScalarAsync(cancellationToken),
            CultureInfo.InvariantCulture);
    }
}
