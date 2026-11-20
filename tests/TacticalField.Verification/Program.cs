using System.Diagnostics;
using System.Globalization;
using System.Text.Json.Nodes;
using TacticalField.Core;

namespace TacticalField.Verification;

internal static class Program
{
    private static int _checks;

    public static int Main()
    {
        var tests = new (string Name, Action Body)[]
        {
            ("full-field-determinism", FullFieldDeterminism),
            ("randomized-incremental-vs-full", RandomizedIncrementalMatchesFull),
            ("batch-impact-and-cache", BatchImpactAndCache),
            ("versioned-query-isolation", VersionedQueryIsolation),
            ("ordered-async-batches", OrderedAsyncBatches),
            ("busy-and-batch-rollback", BusyAndBatchRollback),
            ("snapshot-recovery-and-corruption", SnapshotRecoveryAndCorruption),
            ("forced-termination-recovery", ForcedTerminationRecovery),
            ("deterministic-samples", DeterministicSamples)
        };

        var failed = 0;
        foreach (var (name, body) in tests)
        {
            var before = _checks;
            try
            {
                body();
                Console.WriteLine($"PASS {name} checks={_checks - before}");
            }
            catch (Exception exception)
            {
                failed++;
                Console.WriteLine($"FAIL {name}: {exception.Message}");
                Console.WriteLine(exception.StackTrace);
            }
        }

        Console.WriteLine($"VERIFICATION {(failed == 0 ? "PASS" : "FAIL")} tests={tests.Length - failed}/{tests.Length} checks={_checks}");
        return failed == 0 ? 0 : 2;
    }

    private static void FullFieldDeterminism()
    {
        var definition = TestDefinition(9, 7, 1001);
        var unit = definition.Units[0];
        var target = unit.Targets[0];
        var first = CostFieldCalculator.BuildFull(definition.Grid, unit, target, 5);
        var second = CostFieldCalculator.BuildFull(definition.Grid, unit, target, 5);
        Check(first.ContentHash == second.ContentHash, "Full field content hash is not deterministic.");
        Check(first.GetCostsCopy().SequenceEqual(second.GetCostsCopy()), "Full field costs are not deterministic.");
        Check(first.GetNextCopy().SequenceEqual(second.GetNextCopy()), "Full field next pointers are not deterministic.");
        for (var index = 0; index < definition.Grid.CellCount; index++)
        {
            Check(first.GetCellHash(index) == second.GetCellHash(index), $"Cell hash mismatch at {index}.");
        }
    }

    private static void RandomizedIncrementalMatchesFull()
    {
        var definition = TestDefinition(11, 9, 2002);
        var engine = BattlefieldEngine.Create(definition, new EngineOptions { PersistSnapshots = false });
        var random = new Random(7007);
        for (var iteration = 0; iteration < 60; iteration++)
        {
            var changes = new List<BattleChange>();
            var count = 1 + random.Next(4);
            for (var changeIndex = 0; changeIndex < count; changeIndex++)
            {
                var kind = random.Next(5);
                var cell = random.Next(definition.Grid.CellCount);
                if (kind == 0)
                {
                    changes.Add(BattleChange.SetObstacle(cell, random.Next(2) == 0));
                }
                else if (kind == 1)
                {
                    changes.Add(BattleChange.SetHazard(cell, random.Next(2) == 0));
                }
                else
                {
                    var unit = definition.Units[random.Next(definition.Units.Count)];
                    var masks = new[] { 1, 3, 5, 7 };
                    changes.Add(BattleChange.SetUnitMask(unit.Id, masks[random.Next(masks.Length)]));
                }
            }

            var before = engine.Current;
            var result = engine.SubmitBatch(changes);
            Check(result.IsSuccess, $"Random batch {iteration} failed: {result.Message}");
            var after = engine.Current;
            Check(after.Version == before.Version + 1, "Publication version did not increment exactly once.");
            Check(after.BatchSequence == before.BatchSequence + 1, "Batch sequence did not increment exactly once.");

            AssertMatchesFull(engine, after);
            AssertImpactMatchesDelta(before, after, result);
            AssertUnaffectedCellHashes(before, after, result);
        }
    }

    private static void BatchImpactAndCache()
    {
        var definition = TestDefinition(6, 6, 3003);
        var engine = BattlefieldEngine.Create(definition, new EngineOptions { PersistSnapshots = false });
        var unit = definition.Units[0];
        var target = unit.Targets[0];
        var first = engine.QueryPath(unit.Id, target);
        Check(first.Code == ErrorCodes.Ok && !first.CacheHit, "Initial path query should be a cache miss.");
        var path = first.Cells;
        var dependencySet = first.Dependencies.ToHashSet();
        var offPath = Enumerable.Range(0, definition.Grid.CellCount)
            .First(index => !dependencySet.Contains(index) && index != unit.StartCell && index != target);

        var offPathChange = BattleChange.SetHazard(offPath, true);
        var before = engine.Current;
        var batch = engine.SubmitBatch(new[] { offPathChange });
        Check(batch.IsSuccess, "Off-path batch failed.");
        var after = engine.Current;
        Check(!ReferenceEquals(before, after), "Publication should create a new immutable view.");
        Check(before.Fields.Values.All(field => field.ContentHash.Length == 64), "Old view was mutated.");

        var second = engine.QueryPath(unit.Id, target, expectedFieldVersion: after.Fields[new FieldKey(unit.Id, target)].FieldVersion);
        Check(second.CacheHit, "A cache entry whose dependency set is disjoint from the impact set must hit.");
        Check(second.PathHash == first.PathHash, "Unaffected cached path hash changed.");
        Check(second.Cells.SequenceEqual(path), "Unaffected cached path cells changed.");

        var full = CostFieldCalculator.BuildFull(after.Grid, after.Units[unit.Id], target, engine.HazardPenalty);
        var independentPath = CostFieldCalculator.ComputePath(full, unit.StartCell);
        var independentHash = CostFieldCalculator.ComputePathHash(unit.Id, target, full.GetCost(unit.StartCell), independentPath);
        Check(second.PathHash == independentHash, "Cache hit bytes differ from a full recomputation.");
        Check(second.Cells.SequenceEqual(independentPath), "Cache hit cells differ from a full recomputation.");

        var pathCell = path.Length > 2 ? path[path.Length / 2] : -1;
        Check(pathCell >= 0 && pathCell != target && pathCell != unit.StartCell, "Test path has no invalidatable middle cell.");
        var obstacleBatch = engine.SubmitBatch(new[] { BattleChange.SetObstacle(pathCell, true) });
        Check(obstacleBatch.IsSuccess, "Path obstacle batch failed.");
        var third = engine.QueryPath(unit.Id, target);
        Check(third.Code == ErrorCodes.Ok, "Rerouted path query failed.");
        Check(!third.CacheHit, "A cache entry intersecting the impact set must be invalidated.");
        var fourth = engine.QueryPath(unit.Id, target);
        Check(fourth.CacheHit && fourth.PathHash == third.PathHash, "Recomputed path was not cached with identical bytes.");

        var finalFull = CostFieldCalculator.BuildFull(engine.Current.Grid, engine.Current.Units[unit.Id], target, engine.HazardPenalty);
        var finalIndependentPath = CostFieldCalculator.ComputePath(finalFull, unit.StartCell);
        var finalHash = CostFieldCalculator.ComputePathHash(unit.Id, target, finalFull.GetCost(unit.StartCell), finalIndependentPath);
        Check(third.PathHash == finalHash && third.Cells.SequenceEqual(finalIndependentPath), "Invalidated path differs from full recomputation.");
    }

    private static void VersionedQueryIsolation()
    {
        var definition = TestDefinition(10, 8, 4004);
        var engine = BattlefieldEngine.Create(definition, new EngineOptions { PersistSnapshots = false });
        var unit = definition.Units[0];
        var target = unit.Targets[0];
        _ = engine.QueryPath(unit.Id, target);
        var oldView = engine.Current;
        var oldKey = new FieldKey(unit.Id, target);
        var oldField = oldView.Fields[oldKey];

        using var started = new ManualResetEventSlim();
        using var release = new ManualResetEventSlim();
        engine.RecalculationStartedHook = _ =>
        {
            started.Set();
            release.Wait(TimeSpan.FromSeconds(10));
        };

        var changes = new[] { BattleChange.SetHazard(1, true), BattleChange.SetObstacle(2, true) };
        var submission = engine.SubmitBatchAsync(changes);
        Check(submission.IsSuccess && submission.Task is not null, "Async batch was not accepted.");
        Check(started.Wait(TimeSpan.FromSeconds(10)), "Async batch did not reach the deterministic barrier.");

        var during = engine.QueryCost(unit.Id, target);
        Check(during.Code == ErrorCodes.Ok, "Query during recalculation failed.");
        Check(during.PublicationVersion == oldView.Version, "Query observed an unpublished version.");
        Check(during.FieldHash == oldField.ContentHash, "Query observed a partially updated field.");

        var duringPath = engine.QueryPath(unit.Id, target);
        Check(duringPath.PublicationVersion == oldView.Version, "Path query observed a mixed publication.");

        var queryFailures = 0;
        var readers = Enumerable.Range(0, 4).Select(_ => Task.Run(() =>
        {
            for (var iteration = 0; iteration < 100; iteration++)
            {
                var view = engine.Current;
                try
                {
                    foreach (var field in view.Fields.Values)
                    {
                        if (field.GetCost(field.Key.Target) != 0)
                        {
                            Interlocked.Increment(ref queryFailures);
                        }

                        if (field.ContentHash.Length != 64 || field.GetCostsCopy().Length != view.Grid.CellCount)
                        {
                            Interlocked.Increment(ref queryFailures);
                        }
                    }
                }
                catch
                {
                    Interlocked.Increment(ref queryFailures);
                }
            }
        })).ToArray();

        release.Set();
        var batch = submission.Task!.GetAwaiter().GetResult();
        Check(batch.IsSuccess, "Async batch failed.");
        Task.WaitAll(readers);
        Check(queryFailures == 0, "Concurrent readers observed malformed field state.");

        var staleVersion = engine.QueryCost(unit.Id, target, expectedFieldVersion: oldField.FieldVersion);
        Check(staleVersion.Code == ErrorCodes.FieldStale, "Old version query did not return E_FIELD_STALE.");
        var staleHash = engine.QueryCost(unit.Id, target, expectedFieldHash: new string('f', 64));
        Check(staleHash.Code == ErrorCodes.FieldStale, "Wrong hash query did not return E_FIELD_STALE.");
        Check(oldView.Fields[oldKey].ContentHash == oldField.ContentHash, "Published old view was not immutable.");
    }

    private static void BusyAndBatchRollback()
    {
        var definition = TestDefinition(8, 8, 5005);
        var engine = BattlefieldEngine.Create(definition, new EngineOptions { PersistSnapshots = false });
        var unit = definition.Units[0];
        var key = new FieldKey(unit.Id, unit.Targets[0]);
        var before = engine.Current;

        var invalid = engine.SubmitBatch(new[] { BattleChange.SetObstacle(9999, true) });
        Check(invalid.Code == ErrorCodes.BatchAborted, "Invalid batch did not return E_BATCH_ABORTED.");
        Check(ReferenceEquals(before, engine.Current), "Invalid batch polluted published state.");

        using var started = new ManualResetEventSlim();
        using var release = new ManualResetEventSlim();
        engine.RecalculationStartedHook = _ =>
        {
            started.Set();
            release.Wait(TimeSpan.FromSeconds(10));
        };

        var first = engine.SubmitPathRecalculation(key);
        Check(first.IsSuccess && first.Task is not null, "Path recalculation was not accepted.");
        Check(started.Wait(TimeSpan.FromSeconds(10)), "Path recalculation did not reach the barrier.");

        var duplicate = engine.SubmitPathRecalculation(key);
        Check(duplicate.Code == ErrorCodes.Busy, "Duplicate path recalculation did not return E_BUSY.");
        var conflictingBatch = engine.SubmitBatch(new[] { BattleChange.SetHazard(key.Target, true) });
        Check(conflictingBatch.Code == ErrorCodes.Busy, "Conflicting batch did not return E_BUSY.");
        var stillReadable = engine.QueryCost(unit.Id, key.Target);
        Check(stillReadable.Code == ErrorCodes.Ok, "Published field was unavailable during recalculation.");

        release.Set();
        var completed = first.Task!.GetAwaiter().GetResult();
        Check(completed.IsSuccess, "Path recalculation task failed.");
        var after = engine.SubmitPathRecalculation(key);
        Check(after.IsSuccess, "Recalculation lease was not released after completion.");
        after.Task!.GetAwaiter().GetResult();

        var validBatch = engine.SubmitBatch(new[] { BattleChange.SetHazard(0, true) });
        Check(validBatch.IsSuccess, "Valid batch failed.");
        var published = engine.Current;
        var laterInvalid = engine.SubmitBatch(new[] { BattleChange.SetUnitMask("missing", 1) });
        Check(laterInvalid.Code == ErrorCodes.BatchAborted, "Later invalid batch did not abort.");
        Check(ReferenceEquals(published, engine.Current), "Later invalid batch polluted published state.");
    }

    private static void OrderedAsyncBatches()
    {
        var definition = TestDefinition(8, 8, 5505);
        var engine = BattlefieldEngine.Create(definition, new EngineOptions { PersistSnapshots = false });
        using var started = new ManualResetEventSlim();
        using var release = new ManualResetEventSlim();
        var hookCalls = 0;
        engine.RecalculationStartedHook = _ =>
        {
            if (Interlocked.Increment(ref hookCalls) == 1)
            {
                started.Set();
                release.Wait(TimeSpan.FromSeconds(10));
            }
        };

        var first = engine.SubmitBatchAsync(new[] { BattleChange.SetUnitMask("alpha", 3) });
        Check(first.IsSuccess && first.Task is not null, "First ordered batch was not accepted.");
        Check(started.Wait(TimeSpan.FromSeconds(10)), "First ordered batch did not reach the barrier.");
        var second = engine.SubmitBatchAsync(new[] { BattleChange.SetUnitMask("bravo", 7) });
        Check(second.IsSuccess && second.Task is not null, "Second disjoint ordered batch was rejected.");

        release.Set();
        var firstResult = first.Task!.GetAwaiter().GetResult();
        var secondResult = second.Task!.GetAwaiter().GetResult();
        Check(firstResult.IsSuccess && secondResult.IsSuccess, "Ordered async batches failed.");
        Check(firstResult.BatchSequence == 1, "First submitted batch did not receive sequence 1.");
        Check(secondResult.BatchSequence == 2, "Second submitted batch did not receive sequence 2.");
        Check(firstResult.PublicationVersion == 2 && secondResult.PublicationVersion == 3, "Ordered publication versions are not stable.");
        Check(engine.Current.BatchSequence == 2 && engine.Current.Version == 3, "Final publication order is not stable.");
    }

    private static void SnapshotRecoveryAndCorruption()
    {
        var root = NewTempDirectory("snapshot-recovery");
        var snapshotPath = Path.Combine(root, "runtime", "snapshot.json");
        var definition = TestDefinition(7, 6, 6006);
        var engine = BattlefieldEngine.Create(definition, new EngineOptions
        {
            SnapshotPath = snapshotPath,
            PersistSnapshots = true
        });
        var unit = definition.Units[0];
        var target = unit.Targets[0];
        _ = engine.QueryPath(unit.Id, target);
        var batch = engine.SubmitBatch(new[]
        {
            BattleChange.SetHazard(1, true),
            BattleChange.SetUnitMask(definition.Units[1].Id, 7)
        });
        Check(batch.IsSuccess, "Snapshot source batch failed.");
        var afterBatch = engine.QueryPath(unit.Id, target);
        var noOp = engine.SubmitBatch(Array.Empty<BattleChange>());
        Check(noOp.IsSuccess, "No-op publication used to flush cache failed.");
        var beforeRecovery = engine.Current;
        Check(File.Exists(snapshotPath), "Snapshot was not written.");
        var snapshotBytes = File.ReadAllBytes(snapshotPath);

        var recovered = BattlefieldEngine.Recover(snapshotPath);
        Check(recovered.IsSuccess && recovered.Engine is not null, $"Snapshot recovery failed: {recovered.Message}");
        var recoveredView = recovered.Engine!.Current;
        Check(recoveredView.Version == beforeRecovery.Version, "Recovered publication version mismatch.");
        Check(recoveredView.BatchSequence == beforeRecovery.BatchSequence, "Recovered batch sequence mismatch.");
        Check(recoveredView.ChainHash == beforeRecovery.ChainHash, "Recovered chain hash mismatch.");
        foreach (var field in beforeRecovery.Fields.Values)
        {
            Check(recoveredView.Fields[field.Key].ContentHash == field.ContentHash, $"Recovered field hash mismatch for {field.Key}.");
        }

        var recoveredPath = recovered.Engine.QueryPath(unit.Id, target);
        Check(recoveredPath.Code == ErrorCodes.Ok, "Recovered path query failed.");
        Check(recoveredPath.PathHash == afterBatch.PathHash, "Recovered path bytes differ.");
        Check(recoveredPath.CacheHit, "Recovered cache dependency entry was not reusable.");

        var truncated = Path.Combine(root, "truncated.json");
        File.WriteAllBytes(truncated, snapshotBytes.AsSpan(0, snapshotBytes.Length / 2).ToArray());
        var invalid = BattlefieldEngine.Recover(truncated);
        Check(invalid.Code == ErrorCodes.RecoveryInvalid, "Truncated snapshot did not return E_RECOVERY_INVALID.");

        var brokenChain = Path.Combine(root, "broken-chain.json");
        var node = JsonNode.Parse(File.ReadAllText(snapshotPath))!;
        node["History"]!.AsArray()[^1]!["ChainHash"] = new string('a', 64);
        File.WriteAllText(brokenChain, node.ToJsonString());
        var chainInvalid = BattlefieldEngine.Recover(brokenChain);
        Check(chainInvalid.Code == ErrorCodes.RecoveryInvalid, "Broken chain snapshot did not return E_RECOVERY_INVALID.");

        var missingField = Path.Combine(root, "missing-cache.json");
        var missingNode = JsonNode.Parse(File.ReadAllText(snapshotPath))!;
        missingNode.AsObject().Remove("Cache");
        File.WriteAllText(missingField, missingNode.ToJsonString());
        var missingInvalid = BattlefieldEngine.Recover(missingField);
        Check(missingInvalid.Code == ErrorCodes.RecoveryInvalid, "Missing snapshot section did not return E_RECOVERY_INVALID.");
    }

    private static void ForcedTerminationRecovery()
    {
        var root = FindRepositoryRoot();
        var cliCandidates = new[]
        {
            Path.Combine(root, "src", "TacticalField.Cli", "bin", "Release", "net8.0", "TacticalField.Cli.dll"),
            Path.Combine(root, "src", "TacticalField.Cli", "bin", "Debug", "net8.0", "TacticalField.Cli.dll")
        };
        var cli = cliCandidates.FirstOrDefault(File.Exists)
            ?? throw new InvalidOperationException("Build TacticalField.Cli before running forced termination verification.");
        var dataDir = NewTempDirectory("forced-termination");

        var crash = RunProcess(cli, "crash-demo", "--data-dir", dataDir);
        Check(crash.ExitCode != 0, "crash-demo did not terminate the process.");
        Check(crash.StandardOutput.Contains("SNAPSHOT_READY", StringComparison.Ordinal), "crash-demo did not persist a ready snapshot.");

        var recover = RunProcess(cli, "recover", "--data-dir", dataDir);
        Check(recover.ExitCode == 0, $"Recovery after forced termination failed: {recover.StandardError}");
        Check(recover.StandardOutput.Contains("RECOVERY code=OK", StringComparison.Ordinal), "Recovery output did not contain RECOVERY code=OK.");
    }

    private static void DeterministicSamples()
    {
        var dataDir = NewTempDirectory("samples");
        var samples = SampleGenerator.EnsureGenerated(dataDir, force: true);
        Check(samples.Count == 3, "Expected three local samples.");
        foreach (var sample in samples)
        {
            var baseline = SampleGenerator.LoadBaseline(sample.BaselinePath);
            var first = SampleRunner.Run(sample.Definition);
            var second = SampleRunner.Run(sample.Definition);
            Check(first.Trace == second.Trace, $"Trace is not byte-identical for {sample.Name}.");
            Check(first.TraceHash == second.TraceHash, $"Trace hash changed for {sample.Name}.");
            Check(first.TraceHash == baseline.TraceHash, $"Baseline trace hash mismatch for {sample.Name}.");
            Check(first.FinalView.ChainHash == baseline.FinalChainHash, $"Baseline chain mismatch for {sample.Name}.");
            foreach (var field in first.FinalView.Fields.Values)
            {
                Check(baseline.FinalFieldHashes[field.Key.ToString()] == field.ContentHash, $"Baseline field hash mismatch for {field.Key}.");
            }
        }
    }

    private static void AssertMatchesFull(BattlefieldEngine engine, BattlefieldView view)
    {
        foreach (var field in view.Fields.Values)
        {
            var unit = view.Units[field.Key.UnitId];
            var full = CostFieldCalculator.BuildFull(
                view.Grid,
                unit,
                field.Key.Target,
                engine.HazardPenalty,
                field.FieldVersion,
                field.BatchSequence);
            Check(field.ContentHash == full.ContentHash, $"Incremental hash differs from full at {field.Key}.");
            Check(field.GetCostsCopy().SequenceEqual(full.GetCostsCopy()), $"Incremental costs differ from full at {field.Key}.");
            Check(field.GetNextCopy().SequenceEqual(full.GetNextCopy()), $"Incremental next pointers differ from full at {field.Key}.");
            for (var index = 0; index < view.Grid.CellCount; index++)
            {
                Check(field.GetCellHash(index) == full.GetCellHash(index), $"Incremental cell hash differs at {field.Key}:{index}.");
            }
        }
    }

    private static void AssertImpactMatchesDelta(BattlefieldView before, BattlefieldView after, BatchResult result)
    {
        var deltas = result.Deltas.ToDictionary(delta => delta.Key);
        foreach (var key in before.Fields.Keys)
        {
            var oldField = before.Fields[key];
            var newField = after.Fields[key];
            var actual = new List<int>();
            for (var index = 0; index < before.Grid.CellCount; index++)
            {
                if (oldField.GetCost(index) != newField.GetCost(index)
                    || oldField.GetNext(index) != newField.GetNext(index))
                {
                    actual.Add(index);
                }
            }

            if (actual.Count == 0 && ReferenceEquals(oldField, newField))
            {
                Check(!deltas.ContainsKey(key), $"Unchanged field {key} produced a delta.");
                continue;
            }

            Check(deltas.TryGetValue(key, out var delta), $"Changed field {key} is missing a delta.");
            Check(delta!.ImpactCells.SequenceEqual(actual), $"Impact set is not exact for {key}.");
            Check(delta.NewHash == newField.ContentHash, $"Delta hash mismatch for {key}.");
        }
    }

    private static void AssertUnaffectedCellHashes(BattlefieldView before, BattlefieldView after, BatchResult result)
    {
        var deltas = result.Deltas.ToDictionary(delta => delta.Key);
        foreach (var key in before.Fields.Keys)
        {
            var oldField = before.Fields[key];
            var newField = after.Fields[key];
            if (!deltas.TryGetValue(key, out var delta))
            {
                Check(ReferenceEquals(oldField, newField), $"Unchanged field object was rebuilt for {key}.");
                continue;
            }

            var impact = delta.ImpactCells.ToHashSet();
            for (var index = 0; index < before.Grid.CellCount; index++)
            {
                if (!impact.Contains(index))
                {
                    Check(oldField.GetCellHash(index) == newField.GetCellHash(index), $"Reused cell hash changed at {key}:{index}.");
                }
            }
        }
    }

    private static BattlefieldDefinition TestDefinition(int width, int height, int seed)
    {
        var random = new Random(seed);
        var count = width * height;
        var costs = new int[count];
        var masks = new int[count];
        var hazards = new bool[count];
        var obstacles = new bool[count];
        for (var index = 0; index < count; index++)
        {
            costs[index] = 1 + random.Next(5);
            masks[index] = 7;
            hazards[index] = random.Next(10) == 0;
            obstacles[index] = false;
        }

        var target = count / 2;
        var start = count - 1;
        var target2 = 0;
        var start2 = width - 1;
        foreach (var cell in new[] { target, start, target2, start2 })
        {
            hazards[cell] = false;
            obstacles[cell] = false;
        }

        var grid = new BattleGrid(width, height, costs, masks, hazards, obstacles);
        return new BattlefieldDefinition(grid, new[]
        {
            new UnitState("alpha", 7, start, new[] { target }),
            new UnitState("bravo", 3, start2, new[] { target2 })
        });
    }

    private static string NewTempDirectory(string name)
    {
        var root = FindRepositoryRoot();
        var path = Path.Combine(root, "artifacts", "test-tmp", $"{name}-{Environment.ProcessId}-{Guid.NewGuid():N}");
        Directory.CreateDirectory(path);
        return path;
    }

    private static string FindRepositoryRoot()
    {
        var directory = new DirectoryInfo(AppContext.BaseDirectory);
        while (directory is not null)
        {
            if (File.Exists(Path.Combine(directory.FullName, "TacticalField.sln")))
            {
                return directory.FullName;
            }

            directory = directory.Parent;
        }

        throw new InvalidOperationException("Could not locate TacticalField.sln.");
    }

    private static ProcessResult RunProcess(string assembly, params string[] arguments)
    {
        var startInfo = new ProcessStartInfo("dotnet")
        {
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false
        };
        startInfo.ArgumentList.Add(assembly);
        foreach (var argument in arguments)
        {
            startInfo.ArgumentList.Add(argument);
        }

        startInfo.Environment["DOTNET_SYSTEM_GLOBALIZATION_INVARIANT"] = "1";
        startInfo.Environment["DOTNET_CLI_TELEMETRY_OPTOUT"] = "1";
        startInfo.Environment["DOTNET_DbgEnableMiniDump"] = "0";
        using var process = Process.Start(startInfo) ?? throw new InvalidOperationException("Could not start dotnet process.");
        var stdout = process.StandardOutput.ReadToEnd();
        var stderr = process.StandardError.ReadToEnd();
        Check(process.WaitForExit(60_000), "Child process timed out.");
        return new ProcessResult(process.ExitCode, stdout, stderr);
    }

    private static void Check(bool condition, string message)
    {
        _checks++;
        if (!condition)
        {
            throw new InvalidOperationException(message);
        }
    }

    private sealed record ProcessResult(int ExitCode, string StandardOutput, string StandardError);
}
