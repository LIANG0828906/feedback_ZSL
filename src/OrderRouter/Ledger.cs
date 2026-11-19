using Microsoft.Data.Sqlite;

namespace OrderRouter;

public sealed record LedgerBatch(long BatchId, long IdFromExclusive, long IdToInclusive, int RowsCopied, string CommittedUtc);

/// <summary>
/// SQLite migration ledger. Each committed batch range is appended after the
/// SQL Server batch transaction commits, so a crash between the two simply
/// re-runs an idempotent batch. WAL mode lets the serve process read the
/// ledger while the migrator writes.
/// </summary>
public sealed class Ledger
{
    private readonly string _path;

    public Ledger(string path)
    {
        _path = path;
        var dir = Path.GetDirectoryName(Path.GetFullPath(path));
        if (!string.IsNullOrEmpty(dir)) Directory.CreateDirectory(dir);
        Initialize();
    }

    private string ConnString => new SqliteConnectionStringBuilder
    {
        DataSource = _path,
        Mode = SqliteOpenMode.ReadWriteCreate,
        Cache = SqliteCacheMode.Shared
    }.ToString();

    private void Initialize()
    {
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = """
            PRAGMA journal_mode=WAL;
            CREATE TABLE IF NOT EXISTS batches(
                batch_id INTEGER PRIMARY KEY,
                id_from_exclusive INTEGER NOT NULL,
                id_to_inclusive INTEGER NOT NULL,
                rows_copied INTEGER NOT NULL,
                committed_utc TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS meta(
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL
            );
            """;
        cmd.ExecuteNonQuery();
    }

    public void RecordBatch(long batchId, long idFromExclusive, long idToInclusive, int rowsCopied)
    {
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = """
            INSERT OR REPLACE INTO batches(batch_id, id_from_exclusive, id_to_inclusive, rows_copied, committed_utc)
            VALUES ($id, $from, $to, $rows, $ts);
            """;
        cmd.Parameters.AddWithValue("$id", batchId);
        cmd.Parameters.AddWithValue("$from", idFromExclusive);
        cmd.Parameters.AddWithValue("$to", idToInclusive);
        cmd.Parameters.AddWithValue("$rows", rowsCopied);
        cmd.Parameters.AddWithValue("$ts", DateTime.UtcNow.ToString("O"));
        cmd.ExecuteNonQuery();
    }

    public LedgerBatch? LastBatch()
    {
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = """
            SELECT batch_id, id_from_exclusive, id_to_inclusive, rows_copied, committed_utc
            FROM batches ORDER BY batch_id DESC LIMIT 1;
            """;
        using var r = cmd.ExecuteReader();
        if (!r.Read()) return null;
        return new LedgerBatch(r.GetInt64(0), r.GetInt64(1), r.GetInt64(2), r.GetInt32(3), r.GetString(4));
    }

    public List<LedgerBatch> ListBatches()
    {
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = """
            SELECT batch_id, id_from_exclusive, id_to_inclusive, rows_copied, committed_utc
            FROM batches ORDER BY batch_id;
            """;
        var list = new List<LedgerBatch>();
        using var r = cmd.ExecuteReader();
        while (r.Read())
            list.Add(new LedgerBatch(r.GetInt64(0), r.GetInt64(1), r.GetInt64(2), r.GetInt32(3), r.GetString(4)));
        return list;
    }

    public long CommittedRangeCount()
    {
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = "SELECT COUNT(*) FROM batches;";
        return (long)(cmd.ExecuteScalar() ?? 0L);
    }

    public bool RangeMigrated(long idFromInclusive, long idToInclusive)
    {
        // True when committed batches fully cover [idFromInclusive, idToInclusive].
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = "SELECT id_to_inclusive FROM batches ORDER BY batch_id;";
        long covered = idFromInclusive - 1;
        using var r = cmd.ExecuteReader();
        while (r.Read())
        {
            var to = r.GetInt64(0);
            if (to > covered) covered = to;
            if (covered >= idToInclusive) return true;
        }
        return false;
    }

    public long MaxCommittedIdTo()
    {
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = "SELECT COALESCE(MAX(id_to_inclusive), 0) FROM batches;";
        return (long)(cmd.ExecuteScalar() ?? 0L);
    }

    public void SetMeta(string key, string value)
    {
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = "INSERT OR REPLACE INTO meta(key, value) VALUES ($k, $v);";
        cmd.Parameters.AddWithValue("$k", key);
        cmd.Parameters.AddWithValue("$v", value);
        cmd.ExecuteNonQuery();
    }

    public string? GetMeta(string key)
    {
        using var conn = new SqliteConnection(ConnString);
        conn.Open();
        using var cmd = conn.CreateCommand();
        cmd.CommandText = "SELECT value FROM meta WHERE key = $k;";
        cmd.Parameters.AddWithValue("$k", key);
        return cmd.ExecuteScalar() as string;
    }
}
