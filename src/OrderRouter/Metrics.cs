using System.Collections.Concurrent;
using System.Text;

namespace OrderRouter;

/// <summary>In-process counters/gauges rendered in Prometheus text format.</summary>
public sealed class Metrics
{
    private readonly ConcurrentDictionary<string, long> _counters = new();
    private readonly Func<long>? _connInUse;
    private readonly Func<long>? _migrationCommittedRanges;

    public const string HotQueries = "orderrouter_hot_queries_total";
    public const string ColdQueries = "orderrouter_cold_queries_total";
    public const string MergedQueries = "orderrouter_merged_queries_total";
    public const string DedupDropped = "orderrouter_dedup_dropped_total";
    public const string MigrationCommittedRanges = "orderrouter_migration_committed_ranges_total";
    public const string ConnPoolInUse = "orderrouter_connection_pool_in_use";
    public const string EConnTotal = "orderrouter_econn_total";

    public Metrics(Func<long>? connInUse = null, Func<long>? migrationCommittedRanges = null)
    {
        _connInUse = connInUse;
        _migrationCommittedRanges = migrationCommittedRanges;
    }

    public void Inc(string name, long delta = 1) => _counters.AddOrUpdate(name, delta, (_, v) => v + delta);
    public long Get(string name) => _counters.TryGetValue(name, out var v) ? v : 0;

    public string Render()
    {
        var sb = new StringBuilder();
        AppendCounter(sb, HotQueries, "Routed queries served from the hot table only.");
        AppendCounter(sb, ColdQueries, "Routed queries served from the cold table only.");
        AppendCounter(sb, MergedQueries, "Routed queries merged across hot and cold tables.");
        AppendCounter(sb, DedupDropped, "Rows dropped by hot/cold overlap deduplication.");
        AppendCounter(sb, EConnTotal, "Requests rejected with stable error code E_CONN.");
        long ranges = _migrationCommittedRanges?.Invoke() ?? 0;
        sb.Append("# HELP ").Append(MigrationCommittedRanges).Append(" Migration batch ranges committed in the ledger.\n");
        sb.Append("# TYPE ").Append(MigrationCommittedRanges).Append(" gauge\n");
        sb.Append(MigrationCommittedRanges).Append(' ').Append(ranges).Append('\n');
        long inUse = _connInUse?.Invoke() ?? 0;
        sb.Append("# HELP ").Append(ConnPoolInUse).Append(" Connections currently checked out of the pool gate.\n");
        sb.Append("# TYPE ").Append(ConnPoolInUse).Append(" gauge\n");
        sb.Append(ConnPoolInUse).Append(' ').Append(inUse).Append('\n');
        return sb.ToString();
    }

    private void AppendCounter(StringBuilder sb, string name, string help)
    {
        sb.Append("# HELP ").Append(name).Append(' ').Append(help).Append('\n');
        sb.Append("# TYPE ").Append(name).Append(" counter\n");
        sb.Append(name).Append(' ').Append(Get(name)).Append('\n');
    }
}
