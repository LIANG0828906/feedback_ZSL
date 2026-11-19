using System.Net;
using System.Text.Json;
using System.Text.Json.Nodes;

namespace OrderRouter;

/// <summary>Minimal HTTP API: routed queries, writes, metrics, status, verification.</summary>
public sealed class HttpApi : IAsyncDisposable
{
    private readonly AppOptions _opts;
    private readonly Router _router;
    private readonly Writer _writer;
    private readonly Verifier _verifier;
    private readonly ConnectionGate _gate;
    private readonly Ledger _ledger;
    private readonly Metrics _metrics;
    private readonly HttpListener _listener = new();
    private readonly CancellationTokenSource _cts = new();

    private static readonly JsonSerializerOptions JsonOpts = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        WriteIndented = false
    };

    public HttpApi(AppOptions opts, Router router, Writer writer, Verifier verifier,
        ConnectionGate gate, Ledger ledger, Metrics metrics)
    {
        _opts = opts; _router = router; _writer = writer; _verifier = verifier;
        _gate = gate; _ledger = ledger; _metrics = metrics;
        _listener.Prefixes.Add(opts.ListenPrefix);
    }

    public Task RunAsync()
    {
        _listener.Start();
        return Task.Run(async () =>
        {
            while (!_cts.IsCancellationRequested)
            {
                HttpListenerContext ctx;
                try { ctx = await _listener.GetContextAsync().ConfigureAwait(false); }
                catch (Exception) when (_cts.IsCancellationRequested) { break; }
                _ = Task.Run(() => HandleAsync(ctx));
            }
        });
    }

    private async Task HandleAsync(HttpListenerContext ctx)
    {
        try
        {
            var path = ctx.Request.Url?.AbsolutePath ?? "/";
            switch (path)
            {
                case "/query": await HandleQueryAsync(ctx); break;
                case "/update" when ctx.Request.HttpMethod == "POST": await HandleUpdateAsync(ctx); break;
                case "/metrics": await WriteTextAsync(ctx, _metrics.Render(), "text/plain; version=0.0.4"); break;
                case "/status": await WriteJsonAsync(ctx, BuildStatus()); break;
                case "/connections": await WriteJsonAsync(ctx, BuildConnections()); break;
                case "/verify" when ctx.Request.HttpMethod == "POST": await HandleVerifyAsync(ctx); break;
                case "/debug/slowquery" when _opts.EnableDebugHooks: await HandleSlowQueryAsync(ctx); break;
                default:
                    ctx.Response.StatusCode = 404;
                    await WriteJsonAsync(ctx, new { errorCode = "E_NOTFOUND", message = "unknown route" });
                    break;
            }
        }
        catch (ConnPoolException ex)
        {
            ctx.Response.StatusCode = 503;
            await WriteJsonAsync(ctx, new { errorCode = ConnPoolException.Code, message = ex.Message });
        }
        catch (KeyNotFoundException ex)
        {
            ctx.Response.StatusCode = 404;
            await WriteJsonAsync(ctx, new { errorCode = "E_NOTFOUND", message = ex.Message });
        }
        catch (Exception ex)
        {
            ctx.Response.StatusCode = 500;
            await WriteJsonAsync(ctx, new { errorCode = "E_INTERNAL", message = ex.Message });
        }
        finally
        {
            ctx.Response.Close();
        }
    }

    private async Task HandleQueryAsync(HttpListenerContext ctx)
    {
        var q = ctx.Request.QueryString;
        DateTime? from = ParseDate(q["createdFrom"]);
        DateTime? to = ParseDate(q["createdTo"]);
        long? idFrom = ParseLong(q["idFrom"]);
        long? idTo = ParseLong(q["idTo"]);
        var cls = string.Equals(q["class"], "batch", StringComparison.OrdinalIgnoreCase)
            ? QueryClass.Batch : QueryClass.Interactive;
        var result = await _router.QueryAsync(
            new QueryCondition(from, to, idFrom, idTo, cls)).ConfigureAwait(false);
        _verifier.TrackRead(result.Rows.Select(r => r.OrderId));
        await WriteJsonAsync(ctx, new
        {
            route = result.Route,
            tablesAccessed = result.TablesAccessed,
            splitCount = result.SplitCount,
            dedupDropped = result.DedupDropped,
            rowCount = result.Rows.Count,
            rows = result.Rows.Select(r => new
            {
                orderId = r.OrderId,
                orderNo = r.OrderNo,
                customerId = r.CustomerId,
                createdAt = r.CreatedAt.ToString("O"),
                lastUpdated = r.LastUpdated.ToString("O"),
                amount = r.Amount,
                currency = r.Currency,
                status = r.Status,
                source = r.Source
            })
        });
    }

    private async Task HandleUpdateAsync(HttpListenerContext ctx)
    {
        using var doc = JsonDocument.Parse(await ReadBodyAsync(ctx).ConfigureAwait(false));
        var root = doc.RootElement;
        var orderId = root.GetProperty("orderId").GetInt64();
        decimal? amount = root.TryGetProperty("amount", out var a) && a.ValueKind == JsonValueKind.Number
            ? a.GetDecimal() : null;
        string? currency = root.TryGetProperty("currency", out var c) && c.ValueKind == JsonValueKind.String
            ? c.GetString() : null;
        var result = await _writer.UpdateAsync(orderId, amount, currency).ConfigureAwait(false);
        _verifier.TrackUpdated(orderId);
        await WriteJsonAsync(ctx, new
        {
            orderId = result.OrderId,
            hotVisible = result.HotVisible,
            archiveChanged = result.ArchiveChanged,
            lastUpdated = result.LastUpdatedUtc.ToString("O")
        });
    }

    private async Task HandleVerifyAsync(HttpListenerContext ctx)
    {
        var q = ctx.Request.QueryString;
        int count = int.TryParse(q["count"], out var n) ? n : 500;
        var report = await _verifier.VerifyAsync(count).ConfigureAwait(false);
        await WriteJsonAsync(ctx, report);
    }

    private async Task HandleSlowQueryAsync(HttpListenerContext ctx)
    {
        var q = ctx.Request.QueryString;
        int ms = int.TryParse(q["ms"], out var n) ? n : 3000;
        var cls = string.Equals(q["class"], "batch", StringComparison.OrdinalIgnoreCase)
            ? QueryClass.Batch : QueryClass.Interactive;
        await using var lease = await _gate.AcquireAsync(cls).ConfigureAwait(false);
        await Task.Delay(ms).ConfigureAwait(false);
        await WriteJsonAsync(ctx, new { held = ms, inUse = _gate.InUse });
    }

    private JsonObject BuildStatus()
    {
        var last = _ledger.LastBatch();
        var verify = _verifier.Last;
        var pool = new JsonObject
        {
            ["acquireTimeoutMs"] = _opts.AcquireTimeoutMs,
            ["batchMax"] = _gate.BatchMax,
            ["inUse"] = _gate.InUse,
            ["max"] = _gate.Max,
            ["waiting"] = _gate.Waiting
        };
        var migration = new JsonObject
        {
            ["committedRanges"] = _ledger.CommittedRangeCount(),
            ["complete"] = _ledger.GetMeta("migration_complete") == "true",
            ["lastCommittedIdTo"] = last?.IdToInclusive ?? 0,
            ["lastBatchRowsCopied"] = last?.RowsCopied ?? 0,
            ["ledger"] = _opts.LedgerPath
        };
        JsonObject? verification = verify is null ? null : new JsonObject
        {
            ["checked"] = verify.Checked,
            ["mismatchCount"] = verify.MismatchCount,
            ["startedUtc"] = verify.StartedUtc,
            ["verdict"] = verify.Verdict
        };
        // SortedDictionary keeps field order stable; no absolute paths emitted.
        var sorted = new SortedDictionary<string, object?>
        {
            ["archiveCutoffUtc"] = _opts.ArchiveCutoffUtc.ToString("O"),
            ["connectionPool"] = pool,
            ["lastVerification"] = verification,
            ["migration"] = migration
        };
        var node = new JsonObject();
        foreach (var kv in sorted) node[kv.Key] = kv.Value as JsonNode ?? JsonValue.Create(kv.Value);
        return node;
    }

    private JsonObject BuildConnections() => new()
    {
        ["max"] = _gate.Max,
        ["batchMax"] = _gate.BatchMax,
        ["inUse"] = _gate.InUse,
        ["waiting"] = _gate.Waiting
    };

    private static DateTime? ParseDate(string? s)
        => DateTime.TryParse(s, null, System.Globalization.DateTimeStyles.RoundtripKind, out var d)
            ? DateTime.SpecifyKind(d, d.Kind == DateTimeKind.Unspecified ? DateTimeKind.Utc : d.Kind).ToUniversalTime()
            : null;

    private static long? ParseLong(string? s) => long.TryParse(s, out var v) ? v : null;

    private static async Task<string> ReadBodyAsync(HttpListenerContext ctx)
    {
        using var reader = new StreamReader(ctx.Request.InputStream);
        return await reader.ReadToEndAsync().ConfigureAwait(false);
    }

    private static async Task WriteJsonAsync(HttpListenerContext ctx, object payload)
    {
        var bytes = JsonSerializer.SerializeToUtf8Bytes(payload, JsonOpts);
        ctx.Response.ContentType = "application/json";
        ctx.Response.ContentLength64 = bytes.Length;
        await ctx.Response.OutputStream.WriteAsync(bytes).ConfigureAwait(false);
    }

    private static async Task WriteTextAsync(HttpListenerContext ctx, string text, string contentType)
    {
        var bytes = System.Text.Encoding.UTF8.GetBytes(text);
        ctx.Response.ContentType = contentType;
        ctx.Response.ContentLength64 = bytes.Length;
        await ctx.Response.OutputStream.WriteAsync(bytes).ConfigureAwait(false);
    }

    public async ValueTask DisposeAsync()
    {
        _cts.Cancel();
        _listener.Stop();
        _listener.Close();
        await Task.CompletedTask;
    }
}
