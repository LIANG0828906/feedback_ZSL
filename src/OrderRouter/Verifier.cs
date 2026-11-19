using System.Collections.Concurrent;
using Microsoft.Data.SqlClient;

namespace OrderRouter;

public sealed record FieldMismatch(long OrderId, string OrderNo, string Field, string QuerySide, string DirectSide);

public sealed record VerificationReport(
    string StartedUtc, int Checked, int MismatchCount,
    IReadOnlyList<FieldMismatch> Mismatches, string Verdict);

/// <summary>
/// Transactional snapshot consistency check. Both the routed-merge read and
/// the direct two-table merge run inside one SNAPSHOT transaction, so the
/// verdict reflects exactly the data committed at the start of the check
/// while migration and writes keep flowing. Orders mid-migration are never
/// reported missing because the snapshot sees a consistent committed state.
/// </summary>
public sealed class Verifier
{
    private readonly AppOptions _opts;
    private readonly Router _router;

    // Recently read-and-updated order ids observed by the service.
    private readonly ConcurrentDictionary<long, byte> _readOrders = new();
    private readonly ConcurrentDictionary<long, byte> _updatedOrders = new();
    private VerificationReport? _last;

    public Verifier(AppOptions opts, Router router)
    {
        _opts = opts; _router = router;
    }

    public void TrackRead(IEnumerable<long> orderIds)
    {
        foreach (var id in orderIds) _readOrders.TryAdd(id, 0);
        Trim(_readOrders);
    }

    public void TrackUpdated(long orderId)
    {
        _updatedOrders.TryAdd(orderId, 0);
        Trim(_updatedOrders);
    }

    private static void Trim(ConcurrentDictionary<long, byte> map)
    {
        if (map.Count <= 20000) return;
        foreach (var k in map.Keys.Take(map.Count - 10000)) map.TryRemove(k, out _);
    }

    public VerificationReport? Last => _last;

    public async Task<VerificationReport> VerifyAsync(int count, CancellationToken ct = default)
    {
        var started = DateTime.UtcNow;
        var candidates = _updatedOrders.Keys.Where(_readOrders.ContainsKey).Take(count).ToList();
        var mismatches = new List<FieldMismatch>();

        if (candidates.Count > 0)
        {
            await using var conn = new SqlConnection(_opts.ConnectionString);
            await conn.OpenAsync(ct).ConfigureAwait(false);
            await using var tx = (SqlTransaction)await conn.BeginTransactionAsync(
                System.Data.IsolationLevel.Snapshot, ct).ConfigureAwait(false);

            foreach (var chunk in Chunk(candidates, 200))
            {
                long lo = chunk.Min(), hi = chunk.Max();
                // Routed merge inside the snapshot transaction.
                var cond = new QueryCondition(null, null, lo, hi, QueryClass.Interactive);
                var routed = await _router.ReadMergedInTransactionAsync(cond, conn, tx, ct).ConfigureAwait(false);
                var routedById = routed.rows.Where(r => chunk.Contains(r.OrderId)).ToDictionary(r => r.OrderId);

                // Direct merge of both tables inside the same snapshot.
                var directById = await DirectMergeAsync(conn, tx, chunk, ct).ConfigureAwait(false);

                foreach (var id in chunk)
                {
                    var hasR = routedById.TryGetValue(id, out var r);
                    var hasD = directById.TryGetValue(id, out var d);
                    if (hasR != hasD)
                    {
                        mismatches.Add(new FieldMismatch(id, r?.OrderNo ?? d?.OrderNo ?? "?",
                            "presence", hasR ? "present" : "missing", hasD ? "present" : "missing"));
                        continue;
                    }
                    if (!hasR) continue;
                    if (r!.Amount != d!.Amount)
                        mismatches.Add(new FieldMismatch(id, r.OrderNo, "amount", r.Amount.ToString("F2"), d.Amount.ToString("F2")));
                    if (!string.Equals(r.Currency, d.Currency, StringComparison.Ordinal))
                        mismatches.Add(new FieldMismatch(id, r.OrderNo, "currency", r.Currency, d.Currency));
                    if (r.LastUpdated != d.LastUpdated)
                        mismatches.Add(new FieldMismatch(id, r.OrderNo, "lastUpdated",
                            r.LastUpdated.ToString("O"), d.LastUpdated.ToString("O")));
                }
            }
            await tx.CommitAsync(ct).ConfigureAwait(false);
        }

        var report = new VerificationReport(
            started.ToString("O"), candidates.Count, mismatches.Count,
            mismatches, mismatches.Count == 0 ? "ok" : "mismatch");
        _last = report;
        return report;
    }

    private static async Task<Dictionary<long, OrderRow>> DirectMergeAsync(
        SqlConnection conn, SqlTransaction tx, List<long> ids, CancellationToken ct)
    {
        var idList = string.Join(',', ids);
        var sql = $"""
            SELECT OrderId, OrderNo, CustomerId, CreatedAt, LastUpdated, Amount, Currency, Status, Src
            FROM (
                SELECT *, ROW_NUMBER() OVER (PARTITION BY OrderId ORDER BY LastUpdated DESC, Src ASC) AS rn
                FROM (
                    SELECT OrderId, OrderNo, CustomerId, CreatedAt, LastUpdated, Amount, Currency, Status, 'Orders' AS Src
                    FROM dbo.Orders WHERE OrderId IN ({idList})
                    UNION ALL
                    SELECT OrderId, OrderNo, CustomerId, CreatedAt, LastUpdated, Amount, Currency, Status, 'OrdersArchive' AS Src
                    FROM dbo.OrdersArchive WHERE OrderId IN ({idList})
                ) AS both
            ) AS ranked
            WHERE rn = 1;
            """;
        await using var cmd = new SqlCommand(sql, conn, tx);
        var map = new Dictionary<long, OrderRow>();
        await using var reader = await cmd.ExecuteReaderAsync(ct).ConfigureAwait(false);
        while (await reader.ReadAsync(ct).ConfigureAwait(false))
        {
            var row = new OrderRow(
                reader.GetInt64(0), reader.GetString(1), reader.GetInt32(2),
                DateTime.SpecifyKind(reader.GetDateTime(3), DateTimeKind.Utc),
                DateTime.SpecifyKind(reader.GetDateTime(4), DateTimeKind.Utc),
                reader.GetDecimal(5), reader.GetString(6).Trim(), reader.GetString(7), reader.GetString(8));
            map[row.OrderId] = row;
        }
        return map;
    }

    private static IEnumerable<List<long>> Chunk(List<long> ids, int size)
    {
        for (int i = 0; i < ids.Count; i += size)
            yield return ids.GetRange(i, Math.Min(size, ids.Count - i));
    }
}
