using Microsoft.Data.SqlClient;

namespace OrderRouter;

public sealed record OrderRow(
    long OrderId, string OrderNo, int CustomerId,
    DateTime CreatedAt, DateTime LastUpdated,
    decimal Amount, string Currency, string Status, string Source);

public sealed record QueryCondition(
    DateTime? CreatedFromUtc, DateTime? CreatedToUtc,
    long? IdFrom, long? IdTo, QueryClass Class);

public sealed record RoutedResult(
    string Route, IReadOnlyList<string> TablesAccessed, int SplitCount,
    int DedupDropped, IReadOnlyList<OrderRow> Rows);

/// <summary>
/// Splits a query condition into hot/cold/both table access using the
/// configured archive cutoff (date conditions) or committed ledger ranges
/// (id-range conditions), then merges rows by OrderId keeping the side with
/// the newest LastUpdated. Output is ordered by OrderNo and row-identical to
/// a merged read over both tables without splitting.
/// </summary>
public sealed class Router
{
    public const string HotTable = "Orders";
    public const string ColdTable = "OrdersArchive";

    private readonly AppOptions _opts;
    private readonly ConnectionGate _gate;
    private readonly Ledger _ledger;
    private readonly Metrics _metrics;

    public Router(AppOptions opts, ConnectionGate gate, Ledger ledger, Metrics metrics)
    {
        _opts = opts; _gate = gate; _ledger = ledger; _metrics = metrics;
    }

    public async Task<RoutedResult> QueryAsync(QueryCondition cond, CancellationToken ct = default)
    {
        bool needHot, needCold;
        if (cond.CreatedFromUtc is not null || cond.CreatedToUtc is not null)
        {
            var from = cond.CreatedFromUtc ?? DateTime.MinValue;
            var to = cond.CreatedToUtc ?? DateTime.MaxValue;
            needHot = to >= _opts.ArchiveCutoffUtc;
            // Rows older than the cutoff may still sit in the hot table until
            // migration commits and deletes them, so cold ranges need both
            // sides unless the ledger proves the range fully migrated.
            bool migrationComplete = _ledger.GetMeta("migration_complete") == "true";
            needCold = from < _opts.ArchiveCutoffUtc;
            if (needCold && !needHot && migrationComplete) needHot = false;
            else if (needCold && !needHot && !migrationComplete) needHot = true;
        }
        else if (cond.IdFrom is not null || cond.IdTo is not null)
        {
            long lo = cond.IdFrom ?? 0, hi = cond.IdTo ?? long.MaxValue;
            needCold = _ledger.MaxCommittedIdTo() >= lo;
            // Hot may hold not-yet-migrated rows, overlap rows pending delete,
            // or re-materialized updates of migrated orders.
            needHot = true;
            if (!needCold) needHot = true;
        }
        else
        {
            needHot = true; needCold = true;
        }

        var tables = new List<string>(2);
        if (needHot) tables.Add(HotTable);
        if (needCold) tables.Add(ColdTable);
        var route = needHot && needCold ? "merged" : needHot ? "hot" : "cold";

        IAsyncDisposable lease;
        try
        {
            lease = await _gate.AcquireAsync(cond.Class, ct).ConfigureAwait(false);
        }
        catch (ConnPoolException)
        {
            _metrics.Inc(Metrics.EConnTotal);
            throw;
        }
        await using (lease)
        {
            var rows = await ReadMergedAsync(cond, needHot, needCold, ct).ConfigureAwait(false);
            var dedup = rows.dropped;
            _metrics.Inc(route == "merged" ? Metrics.MergedQueries : route == "hot" ? Metrics.HotQueries : Metrics.ColdQueries);
            if (dedup > 0) _metrics.Inc(Metrics.DedupDropped, dedup);
            return new RoutedResult(route, tables, tables.Count, dedup, rows.rows);
        }
    }

    private async Task<(List<OrderRow> rows, int dropped)> ReadMergedAsync(
        QueryCondition cond, bool needHot, bool needCold, CancellationToken ct,
        SqlConnection? existing = null, SqlTransaction? tx = null)
    {
        SqlConnection? owned = null;
        SqlConnection conn;
        if (existing is null)
        {
            owned = new SqlConnection(_opts.ConnectionString);
            await owned.OpenAsync(ct).ConfigureAwait(false);
            conn = owned;
        }
        else
        {
            conn = existing;
        }
        try
        {
            var byId = new Dictionary<long, OrderRow>();
            int dropped = 0;

            async Task ReadTableAsync(string table)
            {
                var sql = $"""
                    SELECT OrderId, OrderNo, CustomerId, CreatedAt, LastUpdated, Amount, Currency, Status
                    FROM dbo.{table}
                    WHERE (@cf IS NULL OR CreatedAt >= @cf)
                      AND (@ct IS NULL OR CreatedAt <  @ct)
                      AND (@idf IS NULL OR OrderId >= @idf)
                      AND (@idt IS NULL OR OrderId <= @idt)
                    """;
                await using var cmd = new SqlCommand(sql, conn, tx);
                cmd.Parameters.AddWithValue("@cf", (object?)cond.CreatedFromUtc ?? DBNull.Value);
                cmd.Parameters.AddWithValue("@ct", (object?)cond.CreatedToUtc ?? DBNull.Value);
                cmd.Parameters.AddWithValue("@idf", (object?)cond.IdFrom ?? DBNull.Value);
                cmd.Parameters.AddWithValue("@idt", (object?)cond.IdTo ?? DBNull.Value);
                await using var reader = await cmd.ExecuteReaderAsync(ct).ConfigureAwait(false);
                while (await reader.ReadAsync(ct).ConfigureAwait(false))
                {
                    var row = new OrderRow(
                        reader.GetInt64(0), reader.GetString(1), reader.GetInt32(2),
                        DateTime.SpecifyKind(reader.GetDateTime(3), DateTimeKind.Utc),
                        DateTime.SpecifyKind(reader.GetDateTime(4), DateTimeKind.Utc),
                        reader.GetDecimal(5), reader.GetString(6).Trim(), reader.GetString(7), table);
                    if (byId.TryGetValue(row.OrderId, out var cur))
                    {
                        dropped++;
                        // Newer LastUpdated wins; ties keep the hot side so the
                        // source annotation is deterministic.
                        if (row.LastUpdated > cur.LastUpdated ||
                            (row.LastUpdated == cur.LastUpdated && cur.Source != HotTable))
                            byId[row.OrderId] = row;
                    }
                    else
                    {
                        byId[row.OrderId] = row;
                    }
                }
            }

            // Hot first so an equal-timestamp archive row never displaces it.
            if (needHot) await ReadTableAsync(HotTable).ConfigureAwait(false);
            if (needCold) await ReadTableAsync(ColdTable).ConfigureAwait(false);

            var rows = byId.Values.ToList();
            rows.Sort((a, b) => string.CompareOrdinal(a.OrderNo, b.OrderNo));
            return (rows, dropped);
        }
        finally
        {
            if (owned is not null) await owned.DisposeAsync().ConfigureAwait(false);
        }
    }

    /// <summary>Direct merged read over both tables inside an existing snapshot transaction (used by the verifier).</summary>
    public async Task<(List<OrderRow> rows, int dropped)> ReadMergedInTransactionAsync(
        QueryCondition cond, SqlConnection conn, SqlTransaction tx, CancellationToken ct = default)
        => await ReadMergedAsync(cond, true, true, ct, conn, tx).ConfigureAwait(false);
}
