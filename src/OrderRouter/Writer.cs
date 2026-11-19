using Microsoft.Data.SqlClient;

namespace OrderRouter;

public sealed record UpdateResult(long OrderId, bool HotVisible, bool ArchiveChanged, DateTime LastUpdatedUtc);

/// <summary>
/// Online write path. Every update lands in the hot table (re-materializing
/// the row there if it only exists in the archive) and, when the order has
/// been migrated, also produces the corresponding change record on the
/// archive side. Both writes commit in one transaction so query-side merges
/// always converge on the newest LastUpdated.
/// </summary>
public sealed class Writer
{
    private readonly AppOptions _opts;
    private readonly ConnectionGate _gate;
    private readonly Metrics _metrics;

    public Writer(AppOptions opts, ConnectionGate gate, Metrics metrics)
    {
        _opts = opts; _gate = gate; _metrics = metrics;
    }

    public async Task<UpdateResult> UpdateAsync(long orderId, decimal? amount, string? currency, CancellationToken ct = default)
    {
        IAsyncDisposable lease;
        try
        {
            lease = await _gate.AcquireAsync(QueryClass.Interactive, ct).ConfigureAwait(false);
        }
        catch (ConnPoolException)
        {
            _metrics.Inc(Metrics.EConnTotal);
            throw;
        }
        await using (lease)
        {
            var now = DateTime.UtcNow;
            await using var conn = new SqlConnection(_opts.ConnectionString);
            await conn.OpenAsync(ct).ConfigureAwait(false);
            await using var tx = (SqlTransaction)await conn.BeginTransactionAsync(ct).ConfigureAwait(false);

            // 1) Hot table must reflect the write. If the hot row is gone
            //    (already migrated + deleted), re-materialize it from the archive.
            await using (var cmd = new SqlCommand("""
                UPDATE dbo.Orders
                SET Amount = COALESCE(@amount, Amount),
                    Currency = COALESCE(@currency, Currency),
                    LastUpdated = @now
                WHERE OrderId = @id;
                """, conn, tx))
            {
                cmd.Parameters.AddWithValue("@amount", (object?)amount ?? DBNull.Value);
                cmd.Parameters.AddWithValue("@currency", (object?)currency ?? DBNull.Value);
                cmd.Parameters.AddWithValue("@now", now);
                cmd.Parameters.AddWithValue("@id", orderId);
                var n = await cmd.ExecuteNonQueryAsync(ct).ConfigureAwait(false);
                if (n == 0)
                {
                    await using var ins = new SqlCommand("""
                        INSERT INTO dbo.Orders (OrderId, OrderNo, CustomerId, CreatedAt, LastUpdated, Amount, Currency, Status)
                        SELECT OrderId, OrderNo, CustomerId, CreatedAt, @now,
                               COALESCE(@amount, Amount), COALESCE(@currency, Currency), Status
                        FROM dbo.OrdersArchive WHERE OrderId = @id;
                        """, conn, tx);
                    ins.Parameters.AddWithValue("@amount", (object?)amount ?? DBNull.Value);
                    ins.Parameters.AddWithValue("@currency", (object?)currency ?? DBNull.Value);
                    ins.Parameters.AddWithValue("@now", now);
                    ins.Parameters.AddWithValue("@id", orderId);
                    var inserted = await ins.ExecuteNonQueryAsync(ct).ConfigureAwait(false);
                    if (inserted == 0)
                        throw new KeyNotFoundException($"order {orderId} not found in hot or archive table");
                }
            }

            // 2) If the order was migrated, append the change on the archive
            //    side so the merge never serves the stale archived copy.
            int archiveChanged;
            await using (var cmd = new SqlCommand("""
                UPDATE dbo.OrdersArchive
                SET Amount = COALESCE(@amount, Amount),
                    Currency = COALESCE(@currency, Currency),
                    LastUpdated = @now
                WHERE OrderId = @id;
                """, conn, tx))
            {
                cmd.Parameters.AddWithValue("@amount", (object?)amount ?? DBNull.Value);
                cmd.Parameters.AddWithValue("@currency", (object?)currency ?? DBNull.Value);
                cmd.Parameters.AddWithValue("@now", now);
                cmd.Parameters.AddWithValue("@id", orderId);
                archiveChanged = await cmd.ExecuteNonQueryAsync(ct).ConfigureAwait(false);
            }

            await tx.CommitAsync(ct).ConfigureAwait(false);
            return new UpdateResult(orderId, true, archiveChanged > 0, now);
        }
    }
}
