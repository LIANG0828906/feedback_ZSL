using OrderRouter;

static string? OptOrNull(string[] args, string name)
{
    for (int i = 0; i < args.Length - 1; i++)
        if (args[i] == name) return args[i + 1];
    return null;
}

if (args.Length == 0)
{
    Console.Error.WriteLine("usage: orderrouter <init-db|seed|serve|migrate|replay|verify> [--config path] [options]");
    return 2;
}

var command = args[0];
var configPath = OptOrNull(args, "--config") ?? "config/appsettings.json";
var opts = AppOptions.Load(configPath);

switch (command)
{
    case "init-db":
    {
        var seeder = new Seeder(opts);
        await seeder.InitDatabaseAsync();
        Console.WriteLine($"[init-db] database {opts.Database} ready (snapshot isolation on)");
        break;
    }
    case "seed":
    {
        var orders = int.Parse(OptOrNull(args, "--orders") ?? "20000");
        var items = int.Parse(OptOrNull(args, "--items") ?? "30000");
        var seeder = new Seeder(opts);
        var (o, i) = await seeder.SeedAsync(orders, items);
        Console.WriteLine($"[seed] orders={o} items={i} cutoff={opts.ArchiveCutoffUtc:O}");
        break;
    }
    case "serve":
    {
        var gate = new ConnectionGate(opts.MaxConnections, opts.ReservedInteractiveConnections, opts.AcquireTimeoutMs);
        var ledger = new Ledger(opts.LedgerPath);
        var metrics = new Metrics(() => gate.InUse, () => ledger.CommittedRangeCount());
        var router = new Router(opts, gate, ledger, metrics);
        var writer = new Writer(opts, gate, metrics);
        var verifier = new Verifier(opts, router);
        var api = new HttpApi(opts, router, writer, verifier, gate, ledger, metrics);
        await api.RunAsync();
        Console.WriteLine($"[serve] listening on {opts.ListenPrefix} cutoff={opts.ArchiveCutoffUtc:O} maxConn={opts.MaxConnections}");
        var done = new TaskCompletionSource();
        Console.CancelKeyPress += (_, e) => { e.Cancel = true; done.TrySetResult(); };
        AppDomain.CurrentDomain.ProcessExit += (_, _) => done.TrySetResult();
        await done.Task;
        await api.DisposeAsync();
        break;
    }
    case "migrate":
    {
        var ledger = new Ledger(opts.LedgerPath);
        int? batchSize = OptOrNull(args, "--batch-size") is { } bs ? int.Parse(bs) : null;
        int batchDelay = OptOrNull(args, "--batch-delay-ms") is { } bd ? int.Parse(bd) : 0;
        var migrator = new Migrator(opts, ledger, batchSize, batchDelay);
        int? maxBatches = OptOrNull(args, "--max-batches") is { } mb ? int.Parse(mb) : null;
        var progress = await migrator.RunAsync(maxBatches,
            p => Console.WriteLine($"[migrate] batch committed: idTo={p.LastIdTo} rows={p.RowsCopied}"));
        Console.WriteLine($"[migrate] done batches={progress.BatchesCommitted} rows={progress.RowsCopied} complete={progress.Complete} lastIdTo={progress.LastIdTo}");
        break;
    }
    case "replay":
    {
        var replayOpts = ReplayOptions.Load(OptOrNull(args, "--replay-config") ?? "config/replay.json");
        var replay = new Replay(replayOpts, opts);
        var report = await replay.RunAsync();
        Console.WriteLine(report.ToJsonString(new System.Text.Json.JsonSerializerOptions { WriteIndented = true }));
        Console.WriteLine($"[replay] report written to {replayOpts.ReportPath}");
        break;
    }
    case "verify":
    {
        var count = int.Parse(OptOrNull(args, "--count") ?? "500");
        var url = OptOrNull(args, "--url") ?? "http://127.0.0.1:8298/";
        using var http = new HttpClient { BaseAddress = new Uri(url), Timeout = TimeSpan.FromSeconds(120) };
        var resp = await http.PostAsync($"verify?count={count}", null);
        var body = await resp.Content.ReadAsStringAsync();
        Console.WriteLine(body);
        resp.EnsureSuccessStatusCode();
        break;
    }
    default:
        Console.Error.WriteLine($"unknown command {command}");
        return 2;
}
return 0;
