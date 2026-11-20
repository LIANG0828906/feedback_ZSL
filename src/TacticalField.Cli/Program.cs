using System.Globalization;
using TacticalField.Core;

namespace TacticalField.Cli;

internal static class Program
{
    public static int Main(string[] args)
    {
        if (args.Length == 0)
        {
            PrintHelp();
            return 0;
        }

        try
        {
            var command = args[0];
            var options = ParseOptions(args.Skip(1).ToArray());
            return command switch
            {
                "generate-samples" => GenerateSamples(options),
                "verify-samples" => VerifySamples(options),
                "run-sample" => RunSample(options),
                "recover" => Recover(options),
                "snapshot-demo" => SnapshotDemo(options, crash: false),
                "crash-demo" => SnapshotDemo(options, crash: true),
                "help" or "--help" or "-h" => Help(),
                _ => Unknown(command)
            };
        }
        catch (Exception exception)
        {
            Console.Error.WriteLine($"ERROR {exception.GetType().Name}: {exception.Message}");
            return 1;
        }
    }

    private static int GenerateSamples(Dictionary<string, string?> options)
    {
        var dataDir = DataDirectory(options);
        var force = options.ContainsKey("force");
        var samples = SampleGenerator.EnsureGenerated(dataDir, force);
        foreach (var sample in samples)
        {
            Console.WriteLine(
                $"GENERATED sample={sample.Name} grid={sample.Definition.Width}x{sample.Definition.Height} " +
                $"baseline={sample.Baseline.TraceHash} final_chain={sample.Baseline.FinalChainHash}");
        }

        Console.WriteLine($"SAMPLE PASS count={samples.Count} data_dir={Path.GetFullPath(dataDir)}");
        return 0;
    }

    private static int VerifySamples(Dictionary<string, string?> options)
    {
        var dataDir = DataDirectory(options);
        var samples = SampleGenerator.EnsureGenerated(dataDir);
        var failures = 0;
        foreach (var sample in samples)
        {
            var expected = SampleGenerator.LoadBaseline(sample.BaselinePath);
            var first = SampleRunner.Run(sample.Definition);
            var second = SampleRunner.Run(sample.Definition);
            var sameTrace = string.Equals(first.Trace, second.Trace, StringComparison.Ordinal);
            var traceMatchesBaseline = string.Equals(first.TraceHash, expected.TraceHash, StringComparison.Ordinal);
            var fieldsMatch = sample.Definition.Units.All(unit =>
                unit.Targets.All(target =>
                {
                    var key = new FieldKey(unit.Id, target);
                    return string.Equals(first.FinalView.Fields[key].ContentHash, expected.FinalFieldHashes[key.ToString()], StringComparison.Ordinal);
                }));
            var chainMatches = string.Equals(first.FinalView.ChainHash, expected.FinalChainHash, StringComparison.Ordinal);
            var ok = sameTrace && traceMatchesBaseline && fieldsMatch && chainMatches;
            if (!ok)
            {
                failures++;
            }

            Console.WriteLine(
                $"{(ok ? "PASS" : "FAIL")} sample={sample.Name} trace={first.TraceHash} " +
                $"deterministic={sameTrace.ToString().ToLowerInvariant()} " +
                $"baseline={traceMatchesBaseline.ToString().ToLowerInvariant()} " +
                $"fields={fieldsMatch.ToString().ToLowerInvariant()} " +
                $"chain={chainMatches.ToString().ToLowerInvariant()} " +
                $"cache_hits={first.CacheHits} cache_misses={first.CacheMisses}");
        }

        Console.WriteLine($"VERIFY {(failures == 0 ? "PASS" : "FAIL")} {samples.Count - failures}/{samples.Count}");
        return failures == 0 ? 0 : 2;
    }

    private static int RunSample(Dictionary<string, string?> options)
    {
        var dataDir = DataDirectory(options);
        var name = options.GetValueOrDefault("sample") ?? options.GetValueOrDefault("_0");
        if (string.IsNullOrWhiteSpace(name))
        {
            throw new ArgumentException("run-sample requires a sample name.");
        }

        var samples = SampleGenerator.EnsureGenerated(dataDir);
        var sample = samples.FirstOrDefault(value => string.Equals(value.Name, name, StringComparison.Ordinal))
            ?? throw new ArgumentException($"Unknown sample '{name}'.");
        var snapshotPath = options.GetValueOrDefault("snapshot");
        var engineOptions = new EngineOptions
        {
            SnapshotPath = snapshotPath,
            PersistSnapshots = !string.IsNullOrWhiteSpace(snapshotPath),
            HazardPenalty = sample.Definition.HazardPenalty
        };
        var result = SampleRunner.Run(sample.Definition, engineOptions);
        var output = options.GetValueOrDefault("out");
        if (!string.IsNullOrWhiteSpace(output))
        {
            var directory = Path.GetDirectoryName(Path.GetFullPath(output));
            if (!string.IsNullOrEmpty(directory))
            {
                Directory.CreateDirectory(directory);
            }

            File.WriteAllText(output, result.Trace, new System.Text.UTF8Encoding(encoderShouldEmitUTF8Identifier: false));
        }
        else
        {
            Console.Write(result.Trace);
        }

        Console.Error.WriteLine(
            $"TRACE sample={sample.Name} hash={result.TraceHash} cache_hits={result.CacheHits} cache_misses={result.CacheMisses}");
        return 0;
    }

    private static int Recover(Dictionary<string, string?> options)
    {
        var dataDir = DataDirectory(options);
        var snapshotPath = options.GetValueOrDefault("snapshot") ?? Path.Combine(dataDir, "runtime", "snapshot.json");
        var result = BattlefieldEngine.Recover(snapshotPath);
        if (!result.IsSuccess || result.Engine is null)
        {
            Console.WriteLine($"RECOVERY code={result.Code} message={result.Message}");
            return 2;
        }

        var view = result.Engine.Current;
        Console.WriteLine(
            $"RECOVERY code=OK version={view.Version.ToString(CultureInfo.InvariantCulture)} " +
            $"sequence={view.BatchSequence.ToString(CultureInfo.InvariantCulture)} chain={view.ChainHash} fields={view.Fields.Count}");
        foreach (var field in view.Fields.Values.OrderBy(field => field.Key.UnitId, StringComparer.Ordinal).ThenBy(field => field.Key.Target))
        {
            var query = result.Engine.QueryPath(field.Key.UnitId, field.Key.Target);
            Console.WriteLine(
                $"RESTORED_FIELD key={field.Key} version={field.FieldVersion} hash={field.ContentHash} " +
                $"path_code={query.Code} path_hash={query.PathHash} cache={query.CacheHit}");
        }

        return 0;
    }

    private static int SnapshotDemo(Dictionary<string, string?> options, bool crash)
    {
        var dataDir = DataDirectory(options);
        var snapshotPath = options.GetValueOrDefault("snapshot") ?? Path.Combine(dataDir, "runtime", "snapshot.json");
        var samples = SampleGenerator.EnsureGenerated(dataDir);
        var sample = samples[0];

        BattlefieldEngine engine;
        if (File.Exists(snapshotPath))
        {
            var recovered = BattlefieldEngine.Recover(snapshotPath);
            if (!recovered.IsSuccess || recovered.Engine is null)
            {
                Console.WriteLine($"RECOVERY code={recovered.Code} message={recovered.Message}");
                return 2;
            }

            engine = recovered.Engine;
        }
        else
        {
            engine = BattlefieldEngine.Create(sample.Definition.ToBattlefieldDefinition(), new EngineOptions
            {
                SnapshotPath = snapshotPath,
                PersistSnapshots = true,
                HazardPenalty = sample.Definition.HazardPenalty
            });
        }

        if (engine.Current.BatchSequence == 0 && sample.Definition.Batches.Length > 0)
        {
            var changes = sample.Definition.Batches[0].Changes.Select(change => change.ToBattleChange()).ToArray();
            var batch = engine.SubmitBatch(changes);
            Console.WriteLine(
                $"SNAPSHOT_COMMIT code={batch.Code} version={batch.PublicationVersion} " +
                $"sequence={batch.BatchSequence} chain={batch.ChainHash}");
        }

        foreach (var field in engine.Current.Fields.Values.OrderBy(field => field.Key.UnitId, StringComparer.Ordinal).ThenBy(field => field.Key.Target))
        {
            _ = engine.QueryPath(field.Key.UnitId, field.Key.Target);
        }

        var flush = engine.SubmitBatch(Array.Empty<BattleChange>());
        if (!flush.IsSuccess)
        {
            Console.WriteLine($"SNAPSHOT_FLUSH code={flush.Code} message={flush.Message}");
            return 2;
        }

        var view = engine.Current;
        Console.WriteLine(
            $"SNAPSHOT_READY version={view.Version} sequence={view.BatchSequence} chain={view.ChainHash} " +
            $"snapshot={Path.GetFullPath(snapshotPath)}");
        Console.Out.Flush();
        if (crash)
        {
            Environment.FailFast("forced-termination-demo");
        }

        return 0;
    }

    private static int Help()
    {
        PrintHelp();
        return 0;
    }

    private static int Unknown(string command)
    {
        Console.Error.WriteLine($"ERROR unknown command '{command}'");
        PrintHelp();
        return 1;
    }

    private static void PrintHelp()
    {
        Console.WriteLine(
            "TacticalField CLI\n" +
            "  generate-samples [--data-dir DIR] [--force]\n" +
            "  verify-samples   [--data-dir DIR]\n" +
            "  run-sample NAME  [--data-dir DIR] [--out FILE] [--snapshot FILE]\n" +
            "  snapshot-demo    [--data-dir DIR] [--snapshot FILE]\n" +
            "  crash-demo       [--data-dir DIR] [--snapshot FILE]\n" +
            "  recover          [--data-dir DIR] [--snapshot FILE]");
    }

    private static string DataDirectory(Dictionary<string, string?> options)
        => Path.GetFullPath(options.GetValueOrDefault("data-dir") ?? "data");

    private static Dictionary<string, string?> ParseOptions(string[] args)
    {
        var result = new Dictionary<string, string?>(StringComparer.Ordinal);
        var positional = 0;
        for (var index = 0; index < args.Length; index++)
        {
            var value = args[index];
            if (value.StartsWith("--", StringComparison.Ordinal))
            {
                var name = value[2..];
                if (index + 1 < args.Length && !args[index + 1].StartsWith("--", StringComparison.Ordinal))
                {
                    result[name] = args[++index];
                }
                else
                {
                    result[name] = null;
                }
            }
            else
            {
                result[$"_{positional++}"] = value;
            }
        }

        return result;
    }
}
