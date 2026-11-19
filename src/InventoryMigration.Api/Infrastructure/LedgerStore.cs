using System.Globalization;
using System.Text.Json;
using InventoryMigration.Api.Domain;
using InventoryMigration.Api.Options;
using Microsoft.Data.Sqlite;
using Microsoft.Extensions.Options;

namespace InventoryMigration.Api.Infrastructure;

public sealed class LedgerStore
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web);
    private readonly string _connectionString;
    private readonly SemaphoreSlim _writeLock = new(1, 1);
    private readonly ILogger<LedgerStore> _logger;

    public LedgerStore(IOptions<MigrationOptions> options, IHostEnvironment environment, ILogger<LedgerStore> logger)
    {
        _logger = logger;
        var configuredPath = options.Value.LedgerPath;
        var path = Path.IsPathRooted(configuredPath)
            ? configuredPath
            : Path.Combine(environment.ContentRootPath, configuredPath);
        var directory = Path.GetDirectoryName(path);
        if (!string.IsNullOrWhiteSpace(directory))
        {
            Directory.CreateDirectory(directory);
        }

        LedgerPath = Path.GetFullPath(path);
        _connectionString = new SqliteConnectionStringBuilder
        {
            DataSource = LedgerPath,
            Mode = SqliteOpenMode.ReadWriteCreate,
            Cache = SqliteCacheMode.Shared,
            Pooling = true,
            DefaultTimeout = 5
        }.ToString();
    }

    public string LedgerPath { get; }

    public async Task InitializeAsync(CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            """
            PRAGMA journal_mode=WAL;
            PRAGMA synchronous=NORMAL;
            PRAGMA busy_timeout=5000;

            CREATE TABLE IF NOT EXISTS migration_state (
                id                  INTEGER PRIMARY KEY CHECK (id = 1),
                product_min         INTEGER NOT NULL,
                product_max         INTEGER NOT NULL,
                started             INTEGER NOT NULL DEFAULT 0,
                state               TEXT NOT NULL DEFAULT 'idle',
                last_committed_end  INTEGER NOT NULL DEFAULT 0,
                current_batch_id    INTEGER NULL,
                updated_at          TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS batches (
                id              INTEGER PRIMARY KEY AUTOINCREMENT,
                range_start     INTEGER NOT NULL UNIQUE,
                range_end       INTEGER NOT NULL UNIQUE,
                status          TEXT NOT NULL,
                start_snapshot  TEXT NULL,
                start_seq       INTEGER NULL,
                cutoff_seq      INTEGER NULL,
                snapshot        TEXT NULL,
                product_count   INTEGER NOT NULL DEFAULT 0,
                flow_count      INTEGER NOT NULL DEFAULT 0,
                started_at      TEXT NOT NULL,
                committed_at    TEXT NULL,
                error           TEXT NULL
            );

            CREATE TABLE IF NOT EXISTS validations (
                id                  INTEGER PRIMARY KEY AUTOINCREMENT,
                batch_id            INTEGER NOT NULL,
                range_start         INTEGER NOT NULL,
                range_end           INTEGER NOT NULL,
                checked_products    INTEGER NOT NULL,
                mismatch_count      INTEGER NOT NULL,
                started_at          TEXT NOT NULL,
                finished_at         TEXT NOT NULL,
                report_json         TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS validation_mismatch_products (
                batch_id    INTEGER NOT NULL,
                product_id  INTEGER NOT NULL,
                PRIMARY KEY (batch_id, product_id)
            );

            CREATE TABLE IF NOT EXISTS rollbacks (
                product_id      INTEGER PRIMARY KEY,
                batch_id        INTEGER NOT NULL,
                rolled_back_at  TEXT NOT NULL,
                reason          TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS captures (
                id              INTEGER PRIMARY KEY CHECK (id = 1),
                last_seq        INTEGER NOT NULL DEFAULT 0,
                captured_total  INTEGER NOT NULL DEFAULT 0
            );

            INSERT INTO captures(id, last_seq, captured_total)
            VALUES (1, 0, 0)
            ON CONFLICT(id) DO NOTHING;
            """;
        await command.ExecuteNonQueryAsync(cancellationToken);
        _logger.LogInformation("SQLite migration ledger initialized at {LedgerPath}", LedgerPath);
    }

    public async Task EnsureProductRangeAsync(
        long productMin,
        long productMax,
        CancellationToken cancellationToken)
    {
        await _writeLock.WaitAsync(cancellationToken);
        try
        {
            await using var connection = await OpenAsync(cancellationToken);
            var command = connection.CreateCommand();
            command.CommandText =
                """
                INSERT INTO migration_state(
                    id, product_min, product_max, started, state, last_committed_end, current_batch_id, updated_at)
                VALUES (1, $min, $max, 0, 'idle', 0, NULL, $now)
                ON CONFLICT(id) DO UPDATE SET
                    product_max = MAX(migration_state.product_max, excluded.product_max),
                    updated_at = excluded.updated_at;
                """;
            command.Parameters.AddWithValue("$min", productMin);
            command.Parameters.AddWithValue("$max", productMax);
            command.Parameters.AddWithValue("$now", Now());
            await command.ExecuteNonQueryAsync(cancellationToken);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    public async Task<MigrationStateRecord> GetStateAsync(CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            """
            SELECT product_min, product_max, started, state, last_committed_end, current_batch_id, updated_at
            FROM migration_state WHERE id = 1;
            """;

        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        if (!await reader.ReadAsync(cancellationToken))
        {
            throw new InvalidOperationException("Migration ledger state is not initialized.");
        }

        var currentBatchId = reader.IsDBNull(5) ? (long?)null : reader.GetInt64(5);
        BatchRecord? currentBatch = null;
        if (currentBatchId.HasValue)
        {
            currentBatch = await GetBatchByIdAsync(currentBatchId.Value, cancellationToken);
        }

        return new MigrationStateRecord(
            reader.GetInt64(0),
            reader.GetInt64(1),
            reader.GetInt64(2) != 0,
            reader.GetInt64(4),
            currentBatchId,
            currentBatch,
            reader.GetString(3),
            reader.GetString(6));
    }

    public async Task SetServiceStateAsync(string state, CancellationToken cancellationToken)
    {
        await _writeLock.WaitAsync(cancellationToken);
        try
        {
            await using var connection = await OpenAsync(cancellationToken);
            var command = connection.CreateCommand();
            command.CommandText =
                """
                UPDATE migration_state
                SET started = CASE WHEN $state IN ('running', 'completed', 'waiting_connection', 'failed')
                                   THEN 1 ELSE started END,
                    state = $state,
                    updated_at = $now
                WHERE id = 1;
                """;
            command.Parameters.AddWithValue("$state", state);
            command.Parameters.AddWithValue("$now", Now());
            await command.ExecuteNonQueryAsync(cancellationToken);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    public async Task<BatchRecord?> TryGetCurrentOrNextBatchAsync(
        int batchSize,
        CancellationToken cancellationToken)
    {
        await _writeLock.WaitAsync(cancellationToken);
        try
        {
            await using var connection = await OpenAsync(cancellationToken);
            await using var transaction = await connection.BeginTransactionAsync(cancellationToken);

            var stateCommand = connection.CreateCommand();
            stateCommand.Transaction = (SqliteTransaction)transaction;
            stateCommand.CommandText =
                "SELECT product_max, last_committed_end, current_batch_id FROM migration_state WHERE id = 1;";
            long productMax;
            long lastCommittedEnd;
            long? currentBatchId;
            await using (var reader = await stateCommand.ExecuteReaderAsync(cancellationToken))
            {
                if (!await reader.ReadAsync(cancellationToken))
                {
                    throw new InvalidOperationException("Migration ledger state is not initialized.");
                }

                productMax = reader.GetInt64(0);
                lastCommittedEnd = reader.GetInt64(1);
                currentBatchId = reader.IsDBNull(2) ? null : reader.GetInt64(2);
            }

            if (currentBatchId.HasValue)
            {
                var existing = await GetBatchByIdAsync(
                    currentBatchId.Value,
                    cancellationToken,
                    connection,
                    (SqliteTransaction)transaction);
                if (existing is not null && existing.Status != "committed")
                {
                    await transaction.CommitAsync(cancellationToken);
                    return existing;
                }

                if (existing is not null)
                {
                    lastCommittedEnd = Math.Max(lastCommittedEnd, existing.RangeEnd);
                    var repair = connection.CreateCommand();
                    repair.Transaction = (SqliteTransaction)transaction;
                    repair.CommandText =
                        """
                        UPDATE migration_state
                        SET last_committed_end = $end, current_batch_id = NULL, updated_at = $now
                        WHERE id = 1;
                        """;
                    repair.Parameters.AddWithValue("$end", lastCommittedEnd);
                    repair.Parameters.AddWithValue("$now", Now());
                    await repair.ExecuteNonQueryAsync(cancellationToken);
                }
            }

            if (lastCommittedEnd >= productMax)
            {
                await transaction.CommitAsync(cancellationToken);
                return null;
            }

            var rangeStart = lastCommittedEnd + 1;
            var rangeEnd = Math.Min(productMax, lastCommittedEnd + Math.Max(1, batchSize));
            var insert = connection.CreateCommand();
            insert.Transaction = (SqliteTransaction)transaction;
            insert.CommandText =
                """
                INSERT INTO batches(range_start, range_end, status, started_at)
                VALUES ($start, $end, 'migrating', $now);
                SELECT last_insert_rowid();
                """;
            insert.Parameters.AddWithValue("$start", rangeStart);
            insert.Parameters.AddWithValue("$end", rangeEnd);
            insert.Parameters.AddWithValue("$now", Now());
            var batchId = (long)(await insert.ExecuteScalarAsync(cancellationToken)
                ?? throw new InvalidOperationException("Could not create migration batch."));

            var update = connection.CreateCommand();
            update.Transaction = (SqliteTransaction)transaction;
            update.CommandText =
                """
                UPDATE migration_state
                SET current_batch_id = $batch, started = 1, state = 'running', updated_at = $now
                WHERE id = 1;
                """;
            update.Parameters.AddWithValue("$batch", batchId);
            update.Parameters.AddWithValue("$now", Now());
            await update.ExecuteNonQueryAsync(cancellationToken);
            await transaction.CommitAsync(cancellationToken);

            return new BatchRecord(
                batchId,
                rangeStart,
                rangeEnd,
                "migrating",
                null,
                null,
                null,
                0,
                0,
                Now(),
                null);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    public async Task SetBatchSnapshotAsync(
        long batchId,
        string startSnapshot,
        long startSeq,
        CancellationToken cancellationToken)
    {
        await _writeLock.WaitAsync(cancellationToken);
        try
        {
            await using var connection = await OpenAsync(cancellationToken);
            var command = connection.CreateCommand();
            command.CommandText =
                """
                UPDATE batches
                SET start_snapshot = $snapshot, start_seq = $seq
                WHERE id = $batch AND status <> 'committed';
                """;
            command.Parameters.AddWithValue("$snapshot", startSnapshot);
            command.Parameters.AddWithValue("$seq", startSeq);
            command.Parameters.AddWithValue("$batch", batchId);
            await command.ExecuteNonQueryAsync(cancellationToken);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    public async Task CommitBatchAsync(
        BatchRecord batch,
        string snapshot,
        long cutoffSeq,
        long flowCount,
        ValidationReport validation,
        CancellationToken cancellationToken)
    {
        await _writeLock.WaitAsync(cancellationToken);
        try
        {
            await using var connection = await OpenAsync(cancellationToken);
            await using var transaction = await connection.BeginTransactionAsync(cancellationToken);

            var guard = connection.CreateCommand();
            guard.Transaction = (SqliteTransaction)transaction;
            guard.CommandText =
                """
                SELECT COUNT(*) FROM batches
                WHERE status = 'committed' AND id <> $batch AND range_end >= $start;
                """;
            guard.Parameters.AddWithValue("$batch", batch.BatchId);
            guard.Parameters.AddWithValue("$start", batch.RangeStart);
            var overlap = Convert.ToInt64(await guard.ExecuteScalarAsync(cancellationToken));
            if (overlap != 0)
            {
                throw new InvalidOperationException(
                    $"Batch {batch.BatchId} would overlap an already committed range.");
            }

            var update = connection.CreateCommand();
            update.Transaction = (SqliteTransaction)transaction;
            update.CommandText =
                """
                UPDATE batches
                SET status = 'committed',
                    snapshot = $snapshot,
                    cutoff_seq = $cutoff,
                    product_count = $products,
                    flow_count = $flows,
                    committed_at = $now,
                    error = NULL
                WHERE id = $batch AND status <> 'committed';
                """;
            update.Parameters.AddWithValue("$snapshot", snapshot);
            update.Parameters.AddWithValue("$cutoff", cutoffSeq);
            update.Parameters.AddWithValue("$products", validation.CheckedProducts);
            update.Parameters.AddWithValue("$flows", flowCount);
            update.Parameters.AddWithValue("$now", Now());
            update.Parameters.AddWithValue("$batch", batch.BatchId);
            if (await update.ExecuteNonQueryAsync(cancellationToken) != 1)
            {
                throw new InvalidOperationException($"Batch {batch.BatchId} is no longer committable.");
            }

            var stateUpdate = connection.CreateCommand();
            stateUpdate.Transaction = (SqliteTransaction)transaction;
            stateUpdate.CommandText =
                """
                UPDATE migration_state
                SET last_committed_end = $end,
                    current_batch_id = NULL,
                    started = 1,
                    state = 'running',
                    updated_at = $now
                WHERE id = 1;
                """;
            stateUpdate.Parameters.AddWithValue("$end", batch.RangeEnd);
            stateUpdate.Parameters.AddWithValue("$now", Now());
            await stateUpdate.ExecuteNonQueryAsync(cancellationToken);

            var reportJson = JsonSerializer.Serialize(validation, JsonOptions);
            var validationInsert = connection.CreateCommand();
            validationInsert.Transaction = (SqliteTransaction)transaction;
            validationInsert.CommandText =
                """
                INSERT INTO validations(
                    batch_id, range_start, range_end, checked_products, mismatch_count,
                    started_at, finished_at, report_json)
                VALUES ($batch, $start, $end, $checked, $mismatch, $started, $finished, $json);
                """;
            validationInsert.Parameters.AddWithValue("$batch", batch.BatchId);
            validationInsert.Parameters.AddWithValue("$start", batch.RangeStart);
            validationInsert.Parameters.AddWithValue("$end", batch.RangeEnd);
            validationInsert.Parameters.AddWithValue("$checked", validation.CheckedProducts);
            validationInsert.Parameters.AddWithValue("$mismatch", validation.MismatchCount);
            validationInsert.Parameters.AddWithValue("$started", validation.StartedAt.ToString("O"));
            validationInsert.Parameters.AddWithValue("$finished", validation.FinishedAt.ToString("O"));
            validationInsert.Parameters.AddWithValue("$json", reportJson);
            await validationInsert.ExecuteNonQueryAsync(cancellationToken);

            var mismatchDelete = connection.CreateCommand();
            mismatchDelete.Transaction = (SqliteTransaction)transaction;
            mismatchDelete.CommandText =
                "DELETE FROM validation_mismatch_products WHERE batch_id = $batch;";
            mismatchDelete.Parameters.AddWithValue("$batch", batch.BatchId);
            await mismatchDelete.ExecuteNonQueryAsync(cancellationToken);

            foreach (var mismatch in validation.Mismatches)
            {
                var mismatchInsert = connection.CreateCommand();
                mismatchInsert.Transaction = (SqliteTransaction)transaction;
                mismatchInsert.CommandText =
                    """
                    INSERT INTO validation_mismatch_products(batch_id, product_id)
                    VALUES ($batch, $product)
                    ON CONFLICT(batch_id, product_id) DO NOTHING;
                    """;
                mismatchInsert.Parameters.AddWithValue("$batch", batch.BatchId);
                mismatchInsert.Parameters.AddWithValue("$product", mismatch.ProductId);
                await mismatchInsert.ExecuteNonQueryAsync(cancellationToken);
            }

            await transaction.CommitAsync(cancellationToken);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    public async Task MarkBatchFailedAsync(
        long batchId,
        string error,
        CancellationToken cancellationToken)
    {
        await _writeLock.WaitAsync(cancellationToken);
        try
        {
            await using var connection = await OpenAsync(cancellationToken);
            var command = connection.CreateCommand();
            command.CommandText =
                """
                UPDATE batches SET status = 'failed', error = $error
                WHERE id = $batch AND status <> 'committed';
                """;
            command.Parameters.AddWithValue("$error", error.Length > 2000 ? error[..2000] : error);
            command.Parameters.AddWithValue("$batch", batchId);
            await command.ExecuteNonQueryAsync(cancellationToken);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    public async Task<RouteDecision> GetRouteAsync(long productId, CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var rollbackCommand = connection.CreateCommand();
        rollbackCommand.CommandText = "SELECT 1 FROM rollbacks WHERE product_id = $product LIMIT 1;";
        rollbackCommand.Parameters.AddWithValue("$product", productId);
        if (await rollbackCommand.ExecuteScalarAsync(cancellationToken) is not null)
        {
            return new RouteDecision(ProductRoute.Old, null, "rolled_back");
        }

        var command = connection.CreateCommand();
        command.CommandText =
            """
            SELECT id, status
            FROM batches
            WHERE $product BETWEEN range_start AND range_end
            ORDER BY CASE WHEN status = 'committed' THEN 0 ELSE 1 END, id DESC
            LIMIT 1;
            """;
        command.Parameters.AddWithValue("$product", productId);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        if (!await reader.ReadAsync(cancellationToken))
        {
            return new RouteDecision(ProductRoute.Old, null, "not_migrated");
        }

        var batchId = reader.GetInt64(0);
        var status = reader.GetString(1);
        return status == "committed"
            ? new RouteDecision(ProductRoute.New, batchId, "committed")
            : new RouteDecision(ProductRoute.Migrating, batchId, "batch_in_progress");
    }

    public async Task<long?> GetCommittedCutoffAsync(long productId, CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            """
            SELECT cutoff_seq FROM batches
            WHERE status = 'committed'
              AND cutoff_seq IS NOT NULL
              AND $product BETWEEN range_start AND range_end
            ORDER BY id DESC LIMIT 1;
            """;
        command.Parameters.AddWithValue("$product", productId);
        var value = await command.ExecuteScalarAsync(cancellationToken);
        return value is null or DBNull ? null : Convert.ToInt64(value, CultureInfo.InvariantCulture);
    }

    public async Task<ValidationReport?> GetLastValidationAsync(CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            "SELECT report_json FROM validations ORDER BY id DESC LIMIT 1;";
        var json = await command.ExecuteScalarAsync(cancellationToken) as string;
        return json is null ? null : JsonSerializer.Deserialize<ValidationReport>(json, JsonOptions);
    }

    public async Task<ValidationReport?> GetValidationByBatchAsync(
        long batchId,
        CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            "SELECT report_json FROM validations WHERE batch_id = $batch ORDER BY id DESC LIMIT 1;";
        command.Parameters.AddWithValue("$batch", batchId);
        var json = await command.ExecuteScalarAsync(cancellationToken) as string;
        return json is null ? null : JsonSerializer.Deserialize<ValidationReport>(json, JsonOptions);
    }

    public async Task<IReadOnlyList<ValidationMismatchRecord>> GetValidationMismatchesAsync(
        CancellationToken cancellationToken)
    {
        var results = new List<ValidationMismatchRecord>();
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            """
            SELECT m.batch_id, m.product_id
            FROM validation_mismatch_products m
            ORDER BY m.batch_id, m.product_id;
            """;
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            results.Add(new ValidationMismatchRecord(reader.GetInt64(0), reader.GetInt64(1)));
        }

        return results;
    }

    public async Task<IReadOnlyList<RollbackRecord>> GetRollbacksAsync(CancellationToken cancellationToken)
    {
        var results = new List<RollbackRecord>();
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            """
            SELECT product_id, batch_id, rolled_back_at, reason
            FROM rollbacks ORDER BY product_id;
            """;
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            results.Add(new RollbackRecord(
                reader.GetInt64(0),
                reader.GetInt64(1),
                DateTimeOffset.Parse(reader.GetString(2), CultureInfo.InvariantCulture),
                reader.GetString(3)));
        }

        return results;
    }

    public async Task<RollbackRecord?> GetRollbackAsync(long productId, CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            """
            SELECT product_id, batch_id, rolled_back_at, reason
            FROM rollbacks WHERE product_id = $product;
            """;
        command.Parameters.AddWithValue("$product", productId);
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        if (!await reader.ReadAsync(cancellationToken))
        {
            return null;
        }

        return new RollbackRecord(
            reader.GetInt64(0),
            reader.GetInt64(1),
            DateTimeOffset.Parse(reader.GetString(2), CultureInfo.InvariantCulture),
            reader.GetString(3));
    }

    public async Task<bool> IsValidationMismatchAsync(
        long productId,
        long batchId,
        CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            """
            SELECT 1 FROM validation_mismatch_products
            WHERE product_id = $product AND batch_id = $batch LIMIT 1;
            """;
        command.Parameters.AddWithValue("$product", productId);
        command.Parameters.AddWithValue("$batch", batchId);
        return await command.ExecuteScalarAsync(cancellationToken) is not null;
    }

    public async Task<RollbackRecord> AddRollbackAsync(
        long productId,
        long batchId,
        string reason,
        CancellationToken cancellationToken)
    {
        await _writeLock.WaitAsync(cancellationToken);
        try
        {
            await using var connection = await OpenAsync(cancellationToken);
            var now = DateTimeOffset.UtcNow;
            var command = connection.CreateCommand();
            command.CommandText =
                """
                INSERT INTO rollbacks(product_id, batch_id, rolled_back_at, reason)
                VALUES ($product, $batch, $now, $reason)
                ON CONFLICT(product_id) DO UPDATE SET
                    batch_id = excluded.batch_id,
                    rolled_back_at = excluded.rolled_back_at,
                    reason = excluded.reason;
                """;
            command.Parameters.AddWithValue("$product", productId);
            command.Parameters.AddWithValue("$batch", batchId);
            command.Parameters.AddWithValue("$now", now.ToString("O"));
            command.Parameters.AddWithValue("$reason", reason);
            await command.ExecuteNonQueryAsync(cancellationToken);
            return new RollbackRecord(productId, batchId, now, reason);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    public async Task<(long Products, long Flows, int Rollbacks)> GetCommittedSummaryAsync(
        CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            """
            SELECT
                COALESCE(SUM(product_count), 0),
                COALESCE(SUM(flow_count), 0),
                (SELECT COUNT(*) FROM rollbacks)
            FROM batches WHERE status = 'committed';
            """;
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        await reader.ReadAsync(cancellationToken);
        return (reader.GetInt64(0), reader.GetInt64(1), reader.GetInt32(2));
    }

    public async Task<(long LastSeq, long CapturedTotal)> GetCaptureStateAsync(
        CancellationToken cancellationToken)
    {
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText = "SELECT last_seq, captured_total FROM captures WHERE id = 1;";
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        await reader.ReadAsync(cancellationToken);
        return (reader.GetInt64(0), reader.GetInt64(1));
    }

    public async Task SetCaptureStateAsync(
        long lastSeq,
        long capturedTotal,
        CancellationToken cancellationToken)
    {
        await _writeLock.WaitAsync(cancellationToken);
        try
        {
            await using var connection = await OpenAsync(cancellationToken);
            var command = connection.CreateCommand();
            command.CommandText =
                """
                UPDATE captures
                SET last_seq = $last, captured_total = $total
                WHERE id = 1;
                """;
            command.Parameters.AddWithValue("$last", lastSeq);
            command.Parameters.AddWithValue("$total", capturedTotal);
            await command.ExecuteNonQueryAsync(cancellationToken);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    public async Task<IReadOnlyList<BatchRecord>> GetCommittedBatchesAsync(
        CancellationToken cancellationToken)
    {
        var results = new List<BatchRecord>();
        await using var connection = await OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText =
            $"{BatchSelectSql} WHERE status = 'committed' ORDER BY id;";
        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            results.Add(ReadBatch(reader));
        }

        return results;
    }

    public async Task<BatchRecord?> GetBatchByIdAsync(
        long batchId,
        CancellationToken cancellationToken,
        SqliteConnection? existingConnection = null,
        SqliteTransaction? existingTransaction = null)
    {
        var connection = existingConnection ?? await OpenAsync(cancellationToken);
        try
        {
            var command = connection.CreateCommand();
            command.Transaction = existingTransaction;
            command.CommandText = $"{BatchSelectSql} WHERE id = $id;";
            command.Parameters.AddWithValue("$id", batchId);
            await using var reader = await command.ExecuteReaderAsync(cancellationToken);
            return await reader.ReadAsync(cancellationToken) ? ReadBatch(reader) : null;
        }
        finally
        {
            if (existingConnection is null)
            {
                await connection.DisposeAsync();
            }
        }
    }

    private const string BatchSelectSql =
        """
        SELECT id, range_start, range_end, status, start_snapshot, start_seq, cutoff_seq,
               product_count, flow_count, started_at, committed_at
        FROM batches
        """;

    private static BatchRecord ReadBatch(SqliteDataReader reader) =>
        new(
            reader.GetInt64(0),
            reader.GetInt64(1),
            reader.GetInt64(2),
            reader.GetString(3),
            reader.IsDBNull(4) ? null : reader.GetString(4),
            reader.IsDBNull(5) ? null : reader.GetInt64(5),
            reader.IsDBNull(6) ? null : reader.GetInt64(6),
            reader.GetInt64(7),
            reader.GetInt64(8),
            reader.GetString(9),
            reader.IsDBNull(10) ? null : reader.GetString(10));

    private async Task<SqliteConnection> OpenAsync(CancellationToken cancellationToken)
    {
        var connection = new SqliteConnection(_connectionString);
        await connection.OpenAsync(cancellationToken);
        var command = connection.CreateCommand();
        command.CommandText = "PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;";
        await command.ExecuteNonQueryAsync(cancellationToken);
        return connection;
    }

    private static string Now() => DateTimeOffset.UtcNow.ToString("O", CultureInfo.InvariantCulture);
}
