using System.Collections.Concurrent;
using System.Diagnostics;
using System.Text.Json;
using System.Text.Json.Nodes;

namespace OrderRouter;

public sealed record TypeStats(
    long Submitted, long Succeeded, long Failed,
    long HotRoutes, long ColdRoutes, long MergedRoutes, long DedupDropped,
    IReadOnlyDictionary<string, long> TablesAccessed, double P95Ms, double P50Ms);

/// <summary>
/// Controlled load replay: drives the running service with a fixed-seed mix of
/// hot queries, cold queries, hot+cold merged queries and updates to migrated
/// orders, then emits a per-type report with route distribution, dedup drops,
/// accessed tables and latency percentiles.
/// </summary>
public sealed class Replay
{
    private readonly ReplayOptions _opts;
    private readonly AppOptions _app;

    public Replay(ReplayOptions opts, AppOptions app) { _opts = opts; _app = app; }

    private sealed class Acc
    {
        public long Submitted, Succeeded, Failed, Hot, Cold, Merged, Dedup;
        public ConcurrentDictionary<string, long> Tables = new();
        public List<double> Latencies = new();
        public object Lock = new();
    }

    public async Task<JsonObject> RunAsync(CancellationToken ct = default)
    {
        var rng = new Random(_opts.Seed);
        var cutoff = _app.ArchiveCutoffUtc;
        using var http = new HttpClient { BaseAddress = new Uri(_opts.Url), Timeout = TimeSpan.FromSeconds(30) };

        var types = new[] { "hot", "cold", "merged", "update" };
        var accs = types.ToDictionary(t => t, _ => new Acc());

        // Pre-build the request plan deterministically.
        var plan = new List<(string type, string uri, string? body)>();
        foreach (var type in types)
        {
            for (int i = 0; i < _opts.RequestsPerType; i++)
            {
                switch (type)
                {
                    case "hot":
                    {
                        var start = cutoff.AddMinutes(rng.Next(0, 60 * 24 * 80));
                        var end = start.AddHours(6);
                        plan.Add((type, $"query?createdFrom={start:O}&createdTo={end:O}&class=interactive", null));
                        break;
                    }
                    case "cold":
                    {
                        var start = cutoff.AddDays(-30).AddMinutes(rng.Next(0, 60 * 24 * 20));
                        var end = start.AddDays(5);
                        if (end > cutoff) end = cutoff.AddTicks(-1);
                        plan.Add((type, $"query?createdFrom={start:O}&createdTo={end:O}&class=batch", null));
                        break;
                    }
                    case "merged":
                    {
                        var start = cutoff.AddMinutes(-rng.Next(1, 60 * 24 * 10));
                        var end = cutoff.AddMinutes(rng.Next(1, 60 * 24 * 10));
                        plan.Add((type, $"query?createdFrom={start:O}&createdTo={end:O}&class=batch", null));
                        break;
                    }
                    case "update":
                    {
                        // Migrated orders live below the cutoff; ids are spread
                        // across the cold id range (seeded deterministically).
                        long id = 1 + (long)(rng.NextDouble() * _opts.SeedOrders * 0.25);
                        var body = JsonSerializer.Serialize(new
                        {
                            orderId = id,
                            amount = Math.Round((decimal)(rng.NextDouble() * 1999 + 1), 2),
                            currency = new[] { "USD", "EUR", "CNY", "JPY" }[rng.Next(4)]
                        });
                        plan.Add((type, "update", body));
                        break;
                    }
                }
            }
        }
        // Deterministic shuffle so types interleave.
        plan = plan.OrderBy(_ => rng.Next()).ToList();

        var sw = Stopwatch.StartNew();
        await Parallel.ForEachAsync(plan,
            new ParallelOptions { MaxDegreeOfParallelism = _opts.Concurrency, CancellationToken = ct },
            async (req, token) =>
            {
                var acc = accs[req.type];
                Interlocked.Increment(ref acc.Submitted);
                var t0 = Stopwatch.GetTimestamp();
                try
                {
                    HttpResponseMessage resp;
                    if (req.body is null)
                        resp = await http.GetAsync(req.uri, token).ConfigureAwait(false);
                    else
                        resp = await http.PostAsync(req.uri,
                            new StringContent(req.body, System.Text.Encoding.UTF8, "application/json"), token).ConfigureAwait(false);
                    var elapsed = Stopwatch.GetElapsedTime(t0).TotalMilliseconds;
                    var payload = await resp.Content.ReadAsStringAsync(token).ConfigureAwait(false);
                    if (!resp.IsSuccessStatusCode)
                    {
                        Interlocked.Increment(ref acc.Failed);
                        return;
                    }
                    Interlocked.Increment(ref acc.Succeeded);
                    lock (acc.Lock) acc.Latencies.Add(elapsed);
                    if (req.body is null)
                    {
                        using var doc = JsonDocument.Parse(payload);
                        var root = doc.RootElement;
                        var route = root.GetProperty("route").GetString() ?? "";
                        switch (route)
                        {
                            case "hot": Interlocked.Increment(ref acc.Hot); break;
                            case "cold": Interlocked.Increment(ref acc.Cold); break;
                            default: Interlocked.Increment(ref acc.Merged); break;
                        }
                        Interlocked.Add(ref acc.Dedup, root.GetProperty("dedupDropped").GetInt64());
                        foreach (var t in root.GetProperty("tablesAccessed").EnumerateArray())
                            acc.Tables.AddOrUpdate(t.GetString() ?? "?", 1, (_, v) => v + 1);
                    }
                }
                catch (Exception)
                {
                    Interlocked.Increment(ref acc.Failed);
                }
            }).ConfigureAwait(false);
        sw.Stop();

        static double Percentile(List<double> values, double p)
        {
            if (values.Count == 0) return 0;
            var sorted = values.OrderBy(v => v).ToList();
            var idx = Math.Min(sorted.Count - 1, (int)Math.Ceiling(p / 100.0 * sorted.Count) - 1);
            return sorted[Math.Max(0, idx)];
        }

        var perType = new JsonObject();
        foreach (var type in types)
        {
            var a = accs[type];
            List<double> lats;
            lock (a.Lock) lats = a.Latencies.ToList();
            perType[type] = new JsonObject
            {
                ["dedupDropped"] = a.Dedup,
                ["failed"] = a.Failed,
                ["p50Ms"] = Math.Round(Percentile(lats, 50), 2),
                ["p95Ms"] = Math.Round(Percentile(lats, 95), 2),
                ["routes"] = new JsonObject
                {
                    ["cold"] = a.Cold,
                    ["hot"] = a.Hot,
                    ["merged"] = a.Merged
                },
                ["submitted"] = a.Submitted,
                ["succeeded"] = a.Succeeded,
                ["tablesAccessed"] = new JsonObject(a.Tables
                    .OrderBy(kv => kv.Key)
                    .Select(kv => new KeyValuePair<string, JsonNode?>(kv.Key, JsonValue.Create(kv.Value))))
            };
        }

        var report = new JsonObject
        {
            ["durationMs"] = sw.ElapsedMilliseconds,
            ["concurrency"] = _opts.Concurrency,
            ["requestsPerType"] = _opts.RequestsPerType,
            ["seed"] = _opts.Seed,
            ["types"] = perType
        };

        var dir = Path.GetDirectoryName(Path.GetFullPath(_opts.ReportPath));
        if (!string.IsNullOrEmpty(dir)) Directory.CreateDirectory(dir);
        await File.WriteAllTextAsync(_opts.ReportPath,
            report.ToJsonString(new JsonSerializerOptions { WriteIndented = true }), ct).ConfigureAwait(false);
        return report;
    }
}
