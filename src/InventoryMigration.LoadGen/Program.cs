using System.Collections.Concurrent;
using System.Diagnostics;
using System.Globalization;
using System.Net.Http.Json;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.RegularExpressions;

var arguments = ParseArguments(args);
var baseUrl = Get("base-url", "http://127.0.0.1:5080");
var durationSeconds = GetInt("duration", 20);
var rate = GetInt("rate", 300);
var seed = GetInt("seed", 20261119);
var reportPath = Get("report", "artifacts/scenario-report.json");
var faultOldDownMs = GetInt("fault-old-down-ms", 0);
var faultBatchDelayMs = GetInt("fault-batch-delay-ms", 10);
var validationProduct = GetInt("fault-validation-product", 0);
var jsonOptions = new JsonSerializerOptions(JsonSerializerDefaults.Web)
{
    WriteIndented = true,
    DefaultIgnoreCondition = JsonIgnoreCondition.Never
};
var oldDownSet = false;

using var client = new HttpClient
{
    BaseAddress = new Uri(baseUrl.TrimEnd('/') + "/"),
    Timeout = TimeSpan.FromSeconds(5)
};

var startedAt = DateTimeOffset.UtcNow;
Console.WriteLine($"Load scenario seed={seed} rate={rate}/s duration={durationSeconds}s");

if (faultBatchDelayMs > 0 || validationProduct > 0)
{
    await PostJsonAsync("api/admin/faults", new
    {
        migrationBatchDelayMs = faultBatchDelayMs,
        validationCorruptProductIds = validationProduct > 0 ? new[] { validationProduct } : []
    });
}

await PostJsonAsync("api/migration/start", null);

var stopwatch = Stopwatch.StartNew();
var routeCounts = new ConcurrentDictionary<string, long>();
var statusCodeCounts = new ConcurrentDictionary<string, long>();
var requestLatencies = new ConcurrentQueue<double>();
var stages = new ConcurrentDictionary<int, StageReport>();
var latestStatus = new JsonDocumentHolder();
var monitorCts = new CancellationTokenSource();
var monitor = MonitorAsync();
var tasks = new HashSet<Task>();
var sent = 0;

while (stopwatch.Elapsed.TotalSeconds < durationSeconds)
{
    var target = (int)Math.Min(
        (long)rate * durationSeconds,
        Math.Floor(stopwatch.Elapsed.TotalSeconds * rate));
    while (sent < target)
    {
        tasks.RemoveWhere(task => task.IsCompleted);
        tasks.Add(SendDebitAsync(sent++));
    }

    if (faultOldDownMs > 0 &&
        stopwatch.ElapsedMilliseconds >= 500 &&
        stopwatch.ElapsedMilliseconds < 500 + faultOldDownMs &&
        !oldDownSet)
    {
        oldDownSet = true;
        await PostJsonAsync("api/admin/faults", new { oldDbUnavailable = true });
        _ = RestoreOldDatabaseAsync();
    }

    await Task.Delay(25);
}

await Task.WhenAll(tasks);
await WaitForCompletionAsync();

if (validationProduct > 0)
{
    var status = await GetDocumentAsync("api/status");
    var mismatch = status.RootElement
        .GetProperty("lastValidation")
        .ValueKind != JsonValueKind.Null &&
        status.RootElement.GetProperty("lastValidation").GetProperty("mismatches")
            .EnumerateArray()
            .Any(item => item.GetProperty("productId").GetInt64() == validationProduct);
    if (mismatch)
    {
        await PostJsonAsync("api/rollback", new
        {
            productId = validationProduct,
            reason = "injected_validation_failure"
        });
    }
}

monitorCts.Cancel();
try
{
    await monitor;
}
catch (OperationCanceledException)
{
}

await PostJsonAsync("api/admin/faults", new
{
    oldDbUnavailable = false,
    migrationBatchDelayMs = 0,
    validationCorruptProductIds = Array.Empty<int>()
}, throwOnFailure: false);

var finalStatus = await GetDocumentAsync("api/status");
var finalMetrics = await client.GetStringAsync("metrics");
var report = BuildReport(finalStatus, finalMetrics);
var directory = Path.GetDirectoryName(Path.GetFullPath(reportPath));
if (!string.IsNullOrWhiteSpace(directory))
{
    Directory.CreateDirectory(directory);
}

await File.WriteAllTextAsync(
    reportPath,
    JsonSerializer.Serialize(report, jsonOptions));
Console.WriteLine($"Scenario report written to {Path.GetFullPath(reportPath)}");

async Task MonitorAsync()
{
    var lastStage = -1;
    while (!monitorCts.IsCancellationRequested)
    {
        try
        {
            var statusJson = await client.GetStringAsync("api/status", monitorCts.Token);
            var metricsText = await client.GetStringAsync("metrics", monitorCts.Token);
            latestStatus.Value = JsonDocument.Parse(statusJson);
            var progress = stopwatch.Elapsed.TotalSeconds / durationSeconds;
            var stage = Math.Min(4, (int)(progress * 4));
            if (stage != lastStage)
            {
                lastStage = stage;
                stages[stage] = CaptureStage(statusJson, metricsText, $"stage_{stage}");
            }
        }
        catch (Exception) when (!monitorCts.IsCancellationRequested)
        {
            await Task.Delay(250, monitorCts.Token);
        }

        await Task.Delay(200, monitorCts.Token);
    }
}

async Task SendDebitAsync(int sequence)
{
    int productId;
    var max = GetLongProperty(latestStatus.Value?.RootElement, "migrationPosition", "productMax") ?? 50000;
    var committed = GetLongProperty(latestStatus.Value?.RootElement, "migrationPosition", "lastCommittedEnd") ?? 0;
    var currentBatch = latestStatus.Value?.RootElement
        .GetProperty("migrationPosition")
        .GetProperty("currentBatch");
    var category = (sequence * 37 + seed) % 10;

    if (category < 2 && currentBatch is { ValueKind: JsonValueKind.Object })
    {
        var start = currentBatch.Value.GetProperty("rangeStart").GetInt64();
        var end = currentBatch.Value.GetProperty("rangeEnd").GetInt64();
        productId = (int)(start + ((sequence * 17L) % Math.Max(1, end - start + 1)));
    }
    else if (category < 5 && committed > 0)
    {
        productId = (int)(1 + ((sequence * 71L) % committed));
    }
    else
    {
        var lower = Math.Min(max, committed + 1);
        productId = (int)(lower + ((sequence * 53L) % Math.Max(1, max - lower + 1)));
    }

    var direction = sequence % 5 == 0 ? "restock" : "deduct";
    var quantity = 1 + (int)((sequence * 13L + seed) % 3);
    var requestStopwatch = Stopwatch.StartNew();
    try
    {
        using var response = await client.PostAsJsonAsync(
            "api/debit",
            new { productId, quantity, direction });
        requestStopwatch.Stop();
        requestLatencies.Enqueue(requestStopwatch.Elapsed.TotalMilliseconds);
        Increment(statusCodeCounts, ((int)response.StatusCode).ToString(CultureInfo.InvariantCulture));

        var body = await response.Content.ReadAsStringAsync();
        using var document = JsonDocument.Parse(body);
        if (response.IsSuccessStatusCode)
        {
            var route = document.RootElement.GetProperty("route").GetString() ?? "unknown";
            Increment(routeCounts, route);
        }
        else
        {
            var code = document.RootElement.TryGetProperty("code", out var codeElement)
                ? codeElement.GetString() ?? "E_UNKNOWN"
                : "E_UNKNOWN";
            Increment(routeCounts, code);
        }
    }
    catch (Exception exception)
    {
        requestStopwatch.Stop();
        requestLatencies.Enqueue(requestStopwatch.Elapsed.TotalMilliseconds);
        Increment(routeCounts, exception is TaskCanceledException ? "E_TIMEOUT" : "E_HTTP");
    }
}

async Task RestoreOldDatabaseAsync()
{
    await Task.Delay(faultOldDownMs);
    await PostJsonAsync("api/admin/faults", new { oldDbUnavailable = false }, throwOnFailure: false);
}

async Task WaitForCompletionAsync()
{
    var deadline = DateTimeOffset.UtcNow.AddSeconds(Math.Max(30, durationSeconds * 2));
    while (DateTimeOffset.UtcNow < deadline)
    {
        using var status = await GetDocumentAsync("api/status");
        var completed = status.RootElement
            .GetProperty("migrationPosition")
            .GetProperty("completed")
            .GetBoolean();
        if (completed)
        {
            return;
        }

        await Task.Delay(500);
    }
}

StageReport CaptureStage(string statusJson, string metricsText, string name)
{
    using var status = JsonDocument.Parse(statusJson);
    var position = status.RootElement.GetProperty("migrationPosition");
    return new StageReport(
        name,
        (long)stopwatch.Elapsed.TotalMilliseconds,
        position.GetProperty("lastCommittedEnd").GetInt64(),
        ReadMetric(metricsText, "inventory_migration_captured_changes_total"),
        routeCounts.TryGetValue("old", out var oldCount) ? oldCount : 0,
        routeCounts.TryGetValue("new", out var newCount) ? newCount : 0,
        routeCounts.TryGetValue("E_MIGRATING", out var migratingCount) ? migratingCount : 0);
}

object BuildReport(JsonDocument status, string metricsText)
{
    var position = status.RootElement.GetProperty("migrationPosition");
    var lastValidation = status.RootElement.GetProperty("lastValidation");
    var rollbacks = status.RootElement.GetProperty("rollbacks");
    var routing = routeCounts.OrderBy(pair => pair.Key, StringComparer.Ordinal)
        .ToDictionary(pair => pair.Key, pair => pair.Value);
    var statusCodes = statusCodeCounts.OrderBy(pair => pair.Key, StringComparer.Ordinal)
        .ToDictionary(pair => pair.Key, pair => pair.Value);

    return new
    {
        seed,
        startedAt = startedAt.ToString("O"),
        finishedAt = DateTimeOffset.UtcNow.ToString("O"),
        durationMs = (long)stopwatch.Elapsed.TotalMilliseconds,
        targetRate = rate,
        migratedProducts = position.GetProperty("lastCommittedEnd").GetInt64(),
        capturedChanges = ReadMetric(metricsText, "inventory_migration_captured_changes_total"),
        routeDistribution = routing,
        statusCodes,
        stages = stages.OrderBy(pair => pair.Key).Select(pair => new
        {
            name = pair.Value.Name,
            elapsedMs = pair.Value.ElapsedMs,
            migratedProducts = pair.Value.MigratedProducts,
            capturedChanges = pair.Value.CapturedChanges,
            routedToOld = pair.Value.RoutedToOld,
            routedToNew = pair.Value.RoutedToNew,
            rejectedMigrating = pair.Value.RejectedMigrating
        }),
        validation = lastValidation.ValueKind == JsonValueKind.Null
            ? null
            : JsonSerializer.Deserialize<object>(lastValidation.GetRawText()),
        rollbacks = JsonSerializer.Deserialize<object>(rollbacks.GetRawText()),
        captureP95Ms = ReadMetric(metricsText, "inventory_migration_capture_p95_ms"),
        httpP95Ms = Percentile(requestLatencies.ToArray(), 0.95),
        normalLoadP95Within120Ms =
            ReadMetric(metricsText, "inventory_migration_capture_p95_ms") <= 120
    };
}

async Task PostJsonAsync(string path, object? body, bool throwOnFailure = true)
{
    using var response = await client.PostAsJsonAsync(path, body ?? new { });
    if (throwOnFailure && !response.IsSuccessStatusCode)
    {
        var text = await response.Content.ReadAsStringAsync();
        throw new InvalidOperationException($"POST {path} failed: {(int)response.StatusCode} {text}");
    }
}

async Task<JsonDocument> GetDocumentAsync(string path)
{
    var text = await client.GetStringAsync(path);
    return JsonDocument.Parse(text);
}

int GetInt(string key, int fallback) =>
    arguments.TryGetValue(key, out var value) && int.TryParse(value, out var parsed)
        ? parsed
        : fallback;

string Get(string key, string fallback) =>
    arguments.TryGetValue(key, out var value) ? value : fallback;

static void Increment(ConcurrentDictionary<string, long> dictionary, string key) =>
    dictionary.AddOrUpdate(key, 1, (_, value) => value + 1);

static long? GetLongProperty(
    JsonElement? element,
    string objectName,
    string propertyName)
{
    if (element is not { ValueKind: JsonValueKind.Object } value ||
        !value.TryGetProperty(objectName, out var child) ||
        !child.TryGetProperty(propertyName, out var property) ||
        property.ValueKind != JsonValueKind.Number)
    {
        return null;
    }

    return property.GetInt64();
}

static double ReadMetric(string text, string name)
{
    var match = Regex.Match(
        text,
        "^" + Regex.Escape(name) + @"(?:\{[^}]*\})?\s+([0-9.]+)$",
        RegexOptions.Multiline | RegexOptions.CultureInvariant);
    return match.Success &&
           double.TryParse(match.Groups[1].Value, NumberStyles.Float, CultureInfo.InvariantCulture, out var value)
        ? value
        : 0;
}

static double Percentile(double[] values, double percentile)
{
    if (values.Length == 0)
    {
        return 0;
    }

    Array.Sort(values);
    var index = (int)Math.Ceiling(values.Length * percentile) - 1;
    return Math.Round(values[Math.Clamp(index, 0, values.Length - 1)], 3);
}

static Dictionary<string, string> ParseArguments(string[] args)
{
    var result = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
    for (var index = 0; index < args.Length; index++)
    {
        if (!args[index].StartsWith("--", StringComparison.Ordinal))
        {
            continue;
        }

        var key = args[index][2..];
        if (key.Contains('=', StringComparison.Ordinal))
        {
            var separator = key.IndexOf('=');
            result[key[..separator]] = key[(separator + 1)..];
        }
        else if (index + 1 < args.Length && !args[index + 1].StartsWith("--", StringComparison.Ordinal))
        {
            result[key] = args[++index];
        }
        else
        {
            result[key] = "true";
        }
    }

    return result;
}

sealed class JsonDocumentHolder
{
    public JsonDocument? Value { get; set; }
}

sealed record StageReport(
    string Name,
    long ElapsedMs,
    long MigratedProducts,
    double CapturedChanges,
    long RoutedToOld,
    long RoutedToNew,
    long RejectedMigrating);
