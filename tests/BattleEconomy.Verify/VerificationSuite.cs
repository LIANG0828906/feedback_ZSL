using System.Diagnostics;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using BattleEconomy.Core;

namespace BattleEconomy.Verify;

internal sealed class VerificationSuite
{
    private readonly List<string> _failures = [];
    private readonly List<string> _passes = [];
    private readonly string _localData;
    private int _checkCount;

    private VerificationSuite(string root)
    {
        _localData = Path.Combine(root, "local-data");
    }

    public static async Task<int> RunAsync()
    {
        var suite = new VerificationSuite(FindRepositoryRoot());
        try
        {
            await suite.ExecuteAsync();
        }
        catch (Exception exception)
        {
            suite._failures.Add($"UNHANDLED {exception.GetType().Name}: {exception.Message}\n{exception.StackTrace}");
        }

        Console.WriteLine(suite.WriteReport());
        return suite._failures.Count == 0 ? 0 : 1;
    }

    private async Task ExecuteAsync()
    {
        Directory.CreateDirectory(_localData);
        Console.WriteLine($"repository: {FindRepositoryRoot()}");
        Console.WriteLine("phase 1/7 terrain validation");
        VerifyTerrainValidation();

        Console.WriteLine("phase 2/7 deterministic generators and baseline hashes");
        var bundles = VerifyGeneratorDeterminism();

        Console.WriteLine("phase 3/7 incremental batches, closure and fence");
        await VerifyScenarioRunsAsync(bundles);

        Console.WriteLine("phase 4/7 concurrent queries, busy gate and stale handles");
        await VerifyConcurrencyAsync();

        Console.WriteLine("phase 5/7 rollback and cancellation");
        await VerifyRollbackAsync();

        Console.WriteLine("phase 6/7 recovery, tamper detection and idempotence");
        await VerifyRecoveryAsync(bundles);

        Console.WriteLine("phase 7/7 forced termination recovery");
        await VerifyForcedTerminationAsync(bundles);

        VerifyByteIdenticalTrajectories();
    }

    private void VerifyTerrainValidation()
    {
        var bundle = DeterministicScenarioGenerator.Generate(ScenarioCatalog.BuiltIns[0]);
        var tempRoot = Path.Combine(_localData, "terrain-validation");
        ResetDirectory(tempRoot);

        var nonPositive = bundle.Terrain.Nodes.SetItem(
            0,
            bundle.Terrain.Nodes[0] with { YieldPerStep = 0m });
        var nonPositiveResult = EconomyEngine.OpenOrCreate(
            new TerrainMap(nonPositive, bundle.Terrain.Outposts),
            new EconomySnapshotStore(Path.Combine(tempRoot, "yield")));
        Check(
            !nonPositiveResult.Success &&
            nonPositiveResult.Error?.Code == ErrorCodes.TerrainInvalid &&
            nonPositiveResult.Error.TerrainId == bundle.Terrain.Nodes[0].Id,
            "non-positive yield returns E_TERRAIN_INVALID with terrain id");

        var dangling = bundle.Terrain.Nodes.SetItem(
            1,
            bundle.Terrain.Nodes[1] with { OutpostId = "OUTPOST-DOES-NOT-EXIST" });
        var danglingResult = EconomyEngine.OpenOrCreate(
            new TerrainMap(dangling, bundle.Terrain.Outposts),
            new EconomySnapshotStore(Path.Combine(tempRoot, "outpost")));
        Check(
            !danglingResult.Success &&
            danglingResult.Error?.Code == ErrorCodes.TerrainInvalid &&
            danglingResult.Error.TerrainId == bundle.Terrain.Nodes[1].Id,
            "dangling outpost returns E_TERRAIN_INVALID with terrain id");

        var first = bundle.Terrain.Nodes[0];
        var invalidAdjacency = bundle.Terrain.Nodes.SetItem(
            0,
            first with { Neighbors = first.Neighbors.Add(999_999) });
        var adjacencyResult = EconomyEngine.OpenOrCreate(
            new TerrainMap(invalidAdjacency, bundle.Terrain.Outposts),
            new EconomySnapshotStore(Path.Combine(tempRoot, "adjacency")));
        Check(
            !adjacencyResult.Success &&
            adjacencyResult.Error?.Code == ErrorCodes.TerrainInvalid &&
            adjacencyResult.Error.TerrainId == first.Id,
            "out-of-range adjacency returns E_TERRAIN_INVALID with terrain id");
    }

    private Dictionary<string, ScenarioBundle> VerifyGeneratorDeterminism()
    {
        var bundles = new Dictionary<string, ScenarioBundle>(StringComparer.Ordinal);
        foreach (var spec in ScenarioCatalog.BuiltIns)
        {
            var first = DeterministicScenarioGenerator.Generate(spec);
            var second = DeterministicScenarioGenerator.Generate(spec);
            var firstDirectory = Path.Combine(_localData, "scenarios", spec.Name);
            var secondDirectory = Path.Combine(_localData, "scenario-repeat", spec.Name);
            ResetDirectory(firstDirectory);
            ResetDirectory(secondDirectory);
            var firstPath = DeterministicScenarioGenerator.WriteScenario(first, firstDirectory);
            var secondPath = DeterministicScenarioGenerator.WriteScenario(second, secondDirectory);
            Check(
                File.ReadAllBytes(firstPath).SequenceEqual(File.ReadAllBytes(secondPath)),
                $"{spec.Name}: same seed produces byte-identical scenario JSON");
            Check(
                first.Batches.SelectMany(item => item.Destructions).Select(item => item.Key)
                    .SequenceEqual(second.Batches.SelectMany(item => item.Destructions).Select(item => item.Key)),
                $"{spec.Name}: destruction sequence is deterministic");
            bundles[spec.Name] = first;
        }

        return bundles;
    }

    private async Task VerifyScenarioRunsAsync(IReadOnlyDictionary<string, ScenarioBundle> bundles)
    {
        foreach (var spec in ScenarioCatalog.BuiltIns)
        {
            var bundle = bundles[spec.Name];
            var runDirectory = Path.Combine(_localData, "runs", spec.Name);
            ResetDirectory(runDirectory);
            var store = new EconomySnapshotStore(Path.Combine(runDirectory, "snapshots"));
            var logPath = Path.Combine(runDirectory, "trajectory.jsonl");
            using var log = new TrajectoryLog(logPath);
            var opened = EconomyEngine.OpenOrCreate(bundle.Terrain, store, log);
            Check(opened.Success && opened.Engine is not null, $"{spec.Name}: engine opens");
            if (!opened.Success || opened.Engine is null)
            {
                continue;
            }

            var engine = opened.Engine;
            var baseline = engine.QueryCurrent();
            Check(baseline.Success, $"{spec.Name}: baseline query succeeds");
            File.WriteAllText(
                Path.Combine(runDirectory, "baseline.sha256"),
                baseline.ContentHash + "\n",
                new UTF8Encoding(false));

            var previous = baseline;
            foreach (var batch in bundle.Batches)
            {
                var beforeHandle = engine.AcquirePublishedSnapshot();
                var result = await engine.SettleBatchAsync(batch);
                Check(
                    result.Success,
                    $"{spec.Name} batch {batch.BatchNumber}: settlement succeeds ({result.Error?.Code})");
                if (!result.Success || result.Trace is null || result.Snapshot is null)
                {
                    continue;
                }

                var trace = result.Trace;
                var perEventUnion = trace.PerDestruction
                    .SelectMany(item => item.ImpactSet)
                    .ToHashSet();
                Check(trace.ClosureValidated, $"{spec.Name} batch {batch.BatchNumber}: closure validated");
                Check(
                    trace.ClosureConclusion == "MATCH",
                    $"{spec.Name} batch {batch.BatchNumber}: closure conclusion matches");
                Check(
                    trace.MergedImpactSet.ToHashSet().SetEquals(perEventUnion),
                    $"{spec.Name} batch {batch.BatchNumber}: merged set equals union of per-event impacts");
                Check(
                    trace.ClosureSet.ToHashSet().SetEquals(perEventUnion),
                    $"{spec.Name} batch {batch.BatchNumber}: recorded closure equals per-event union");
                Check(
                    trace.MergedImpactSet.Length <= trace.ProbedTiles,
                    $"{spec.Name} batch {batch.BatchNumber}: exact impact is within probed candidates");
                Check(
                    trace.RecomputedTiles + trace.ReusedTiles == bundle.Terrain.Nodes.Length,
                    $"{spec.Name} batch {batch.BatchNumber}: recompute plus reuse covers the terrain");
                Check(
                    trace.Version == previous.Version + 1,
                    $"{spec.Name} batch {batch.BatchNumber}: exactly one version is published");
                Check(
                    trace.PreviousVersionHash == previous.ContentHash,
                    $"{spec.Name} batch {batch.BatchNumber}: previous version hash is linked");
                Check(
                    trace.VersionHash == result.Snapshot.ContentHash,
                    $"{spec.Name} batch {batch.BatchNumber}: version hash matches result");

                var after = engine.QueryCurrent();
                Check(after.Success, $"{spec.Name} batch {batch.BatchNumber}: post-publish query succeeds");
                Check(
                    after.ContentHash == result.Snapshot.ContentHash &&
                    after.Version == result.Snapshot.Version,
                    $"{spec.Name} batch {batch.BatchNumber}: query returns the committed version");

                var affected = trace.MergedImpactSet.ToHashSet();
                var previousHashes = previous.Contributions
                    .ToDictionary(item => item.TerrainId, item => item.ContentHash);
                var reusedExactly = after.Contributions
                    .Where(item => !affected.Contains(item.TerrainId))
                    .All(item => previousHashes[item.TerrainId] == item.ContentHash);
                Check(
                    reusedExactly,
                    $"{spec.Name} batch {batch.BatchNumber}: unaffected terrain hashes are reused");

                var expectedStep = previous.EconomicStep + batch.EconomicSteps;
                Check(
                    after.EconomicStep == expectedStep,
                    $"{spec.Name} batch {batch.BatchNumber}: economic step advances once");
                CheckFactionAccumulation(
                    previous,
                    after,
                    batch.EconomicSteps,
                    spec.Name,
                    batch.BatchNumber ?? previous.Version + 1);

                var staleHandleResult = engine.Query(beforeHandle);
                Check(
                    !staleHandleResult.Success &&
                    staleHandleResult.Error?.Code == ErrorCodes.StaleHandle,
                    $"{spec.Name} batch {batch.BatchNumber}: pre-publish handle is stale");
                var staleVersionResult = engine.QueryCurrent(previous.Version);
                Check(
                    !staleVersionResult.Success &&
                    staleVersionResult.Error?.Code == ErrorCodes.VersionStale,
                    $"{spec.Name} batch {batch.BatchNumber}: old expected version is rejected");

                previous = after;
            }

            var finalSnapshot = engine.QueryCurrent();
            var summary = new
            {
                scenario = spec.Name,
                seed = spec.Seed,
                rows = spec.Rows,
                columns = spec.Columns,
                adjacencyDensity = spec.AdjacencyDensity,
                batches = bundle.Batches.Length,
                baselineHash = baseline.ContentHash,
                finalVersion = finalSnapshot.Version,
                finalHash = finalSnapshot.ContentHash,
                economicStep = finalSnapshot.EconomicStep,
                factions = finalSnapshot.Factions.Select(item => new
                {
                    faction = item.Faction.ToString(),
                    yieldPerStep = item.YieldPerStep,
                    totalResources = item.TotalResources
                }).ToArray()
            };
            File.WriteAllText(
                Path.Combine(runDirectory, "summary.json"),
                JsonSerializer.Serialize(summary, new JsonSerializerOptions { WriteIndented = true }) + "\n",
                new UTF8Encoding(false));
        }
    }

    private void CheckFactionAccumulation(
        SnapshotQueryResult previous,
        SnapshotQueryResult current,
        int economicSteps,
        string scenario,
        long batch)
    {
        foreach (var faction in Enum.GetValues<Faction>())
        {
            var before = previous.Factions.Single(item => item.Faction == faction);
            var after = current.Factions.Single(item => item.Faction == faction);
            var expected = before.TotalResources + (after.YieldPerStep * economicSteps);
            Check(
                after.TotalResources == expected,
                $"{scenario} batch {batch}: {faction} output accumulates once per economic step");
        }
    }

    private async Task VerifyConcurrencyAsync()
    {
        var spec = ScenarioCatalog.BuiltIns[0];
        var bundle = DeterministicScenarioGenerator.Generate(spec);
        var root = Path.Combine(_localData, "concurrency");
        ResetDirectory(root);
        var opened = EconomyEngine.OpenOrCreate(
            bundle.Terrain,
            new EconomySnapshotStore(Path.Combine(root, "snapshots")));
        Check(opened.Success && opened.Engine is not null, "concurrency: engine opens");
        if (!opened.Success || opened.Engine is null)
        {
            return;
        }

        var engine = opened.Engine;
        var baseline = engine.QueryCurrent();
        var staleHandle = engine.AcquirePublishedSnapshot();
        var batch = bundle.Batches[0] with
        {
            ExecutionOptions = new BatchExecutionOptions(DelayBeforeCommitMilliseconds: 350)
        };
        var settlement = engine.SettleBatchAsync(batch);
        Check(engine.Busy, "concurrency: settlement gate is held as soon as work is accepted");
        var busy = await engine.SettleBatchAsync(batch);
        Check(
            !busy.Success && busy.Error?.Code == ErrorCodes.Busy,
            "concurrency: duplicate submission returns E_BUSY");
        var during = engine.Query(staleHandle);
        Check(
            during.Success &&
            during.ContentHash == baseline.ContentHash &&
            during.Version == baseline.Version,
            "concurrency: query during settlement sees the complete prior publish");
        var currentDuring = engine.QueryCurrent();
        Check(
            currentDuring.Success && currentDuring.ContentHash == baseline.ContentHash,
            "concurrency: current-version query during settlement is fenced");
        var settled = await settlement;
        Check(settled.Success, "concurrency: delayed settlement completes");
        Check(!engine.Busy, "concurrency: settlement gate is released");
        var retired = engine.Query(staleHandle);
        Check(
            !retired.Success && retired.Error?.Code == ErrorCodes.StaleHandle,
            "concurrency: retired handle returns E_STALE_HANDLE");
        var fresh = engine.AcquirePublishedSnapshot();
        var mismatch = engine.Query(fresh, baseline.Version);
        Check(
            !mismatch.Success && mismatch.Error?.Code == ErrorCodes.VersionStale,
            "concurrency: stale expected version returns E_VERSION_STALE");
        var next = bundle.Batches[1];
        var canceled = new CancellationTokenSource();
        canceled.Cancel();
        var cancelledResult = await engine.SettleBatchAsync(next, canceled.Token);
        Check(
            !cancelledResult.Success &&
            cancelledResult.Error?.Code == ErrorCodes.BatchFailed &&
            cancelledResult.Error.Cancelled,
            "concurrency: cooperative cancellation rolls back with E_BATCH_FAILED");
    }

    private async Task VerifyRollbackAsync()
    {
        var spec = ScenarioCatalog.BuiltIns[1];
        var bundle = DeterministicScenarioGenerator.Generate(spec);
        var root = Path.Combine(_localData, "rollback");
        ResetDirectory(root);
        var store = new EconomySnapshotStore(Path.Combine(root, "snapshots"));
        var opened = EconomyEngine.OpenOrCreate(bundle.Terrain, store);
        Check(opened.Success && opened.Engine is not null, "rollback: engine opens");
        if (!opened.Success || opened.Engine is null)
        {
            return;
        }

        var engine = opened.Engine;
        var baseline = engine.QueryCurrent();
        var baselineBytes = File.ReadAllBytes(store.SnapshotPath);
        var settlementFailure = bundle.Batches[0] with
        {
            ExecutionOptions = new BatchExecutionOptions(
                FailAtPhase: BatchPhases.Settlement,
                FailAtDestructionIndex: 1)
        };
        var failed = await engine.SettleBatchAsync(settlementFailure);
        Check(
            !failed.Success &&
            failed.Error?.Code == ErrorCodes.BatchFailed &&
            failed.Error.Phase == BatchPhases.Settlement &&
            failed.Error.ProcessedDestructionIndex == 1,
            "rollback: settlement failure returns E_BATCH_FAILED with phase and index");
        var afterSettlementFailure = engine.QueryCurrent();
        Check(
            afterSettlementFailure.ContentHash == baseline.ContentHash &&
            afterSettlementFailure.Version == baseline.Version,
            "rollback: settlement failure preserves the published hash");
        Check(
            File.ReadAllBytes(store.SnapshotPath).SequenceEqual(baselineBytes),
            "rollback: settlement failure leaves snapshot bytes unchanged");
        Check(!File.Exists(store.PendingBatchPath), "rollback: settlement failure removes pending intent");

        var commitFailure = bundle.Batches[0] with
        {
            ExecutionOptions = new BatchExecutionOptions(FailAtPhase: BatchPhases.Commit)
        };
        var commitFailed = await engine.SettleBatchAsync(commitFailure);
        Check(
            !commitFailed.Success &&
            commitFailed.Error?.Code == ErrorCodes.BatchFailed &&
            commitFailed.Error.Phase == BatchPhases.Commit,
            "rollback: commit failure returns E_BATCH_FAILED at commit phase");
        Check(
            File.ReadAllBytes(store.SnapshotPath).SequenceEqual(baselineBytes),
            "rollback: commit failure leaves snapshot bytes unchanged");
        Check(!File.Exists(store.PendingBatchPath), "rollback: commit failure removes pending intent");

        var retry = await engine.SettleBatchAsync(bundle.Batches[0]);
        Check(retry.Success, "rollback: retry after rollback succeeds");
        Check(
            retry.Trace?.Version == baseline.Version + 1,
            "rollback: retry publishes exactly one new version");
        Check(
            retry.Trace?.VersionHash == engine.QueryCurrent().ContentHash,
            "rollback: retry query hash matches trace");

        var conservative = bundle.Batches[1] with
        {
            ExecutionOptions = new BatchExecutionOptions(
                ForceConservativeFallback: true,
                ConservativeReason: "PROOF_UNAVAILABLE")
        };
        var conservativeResult = await engine.SettleBatchAsync(conservative);
        Check(
            conservativeResult.Success &&
            conservativeResult.Trace?.ConservativeFallback == true &&
            conservativeResult.Trace.ConservativeReason == "PROOF_UNAVAILABLE" &&
            conservativeResult.Trace.ReusedTiles == 0 &&
            conservativeResult.Trace.RecomputedTiles == bundle.Terrain.Nodes.Length,
            "rollback: unavailable proof triggers conservative full recompute with reason");
    }

    private async Task VerifyRecoveryAsync(IReadOnlyDictionary<string, ScenarioBundle> bundles)
    {
        var spec = ScenarioCatalog.BuiltIns[0];
        var bundle = bundles[spec.Name];
        var root = Path.Combine(_localData, "recovery");
        ResetDirectory(root);
        var store = new EconomySnapshotStore(Path.Combine(root, "base"));
        var opened = EconomyEngine.OpenOrCreate(bundle.Terrain, store);
        Check(opened.Success && opened.Engine is not null, "recovery: source engine opens");
        if (!opened.Success || opened.Engine is null)
        {
            return;
        }

        var source = opened.Engine;
        var firstResult = await source.SettleBatchAsync(bundle.Batches[0]);
        var secondResult = await source.SettleBatchAsync(bundle.Batches[1]);
        Check(firstResult.Success && secondResult.Success, "recovery: source commits two batches");
        var committed = source.QueryCurrent();
        var snapshotBytes = File.ReadAllBytes(store.SnapshotPath);
        File.WriteAllText(
            Path.Combine(root, "committed.sha256"),
            committed.ContentHash + "\n",
            new UTF8Encoding(false));

        var reopened = EconomyEngine.OpenOrCreate(bundle.Terrain, store);
        Check(reopened.Success && reopened.Engine is not null, "recovery: committed snapshot reopens");
        if (reopened.Success && reopened.Engine is not null)
        {
            var restored = reopened.Engine.QueryCurrent();
            Check(
                restored.ContentHash == committed.ContentHash &&
                restored.Version == committed.Version &&
                restored.EconomicStep == committed.EconomicStep,
                "recovery: version, hash and economic step restore exactly");
            Check(
                File.ReadAllBytes(store.SnapshotPath).SequenceEqual(snapshotBytes),
                "recovery: reopening does not rewrite the committed snapshot");

            var alreadyCommitted = await reopened.Engine.SettleBatchAsync(bundle.Batches[1]);
            Check(
                alreadyCommitted.Success &&
                alreadyCommitted.NoOp &&
                reopened.Engine.QueryCurrent().Version == committed.Version,
                "recovery: committed destruction is not settled twice");
        }

        var brokenChainDirectory = Path.Combine(root, "broken-chain");
        CopyDirectory(store.RootDirectory, brokenChainDirectory);
        var brokenChainPath = Path.Combine(brokenChainDirectory, "economy-snapshot.json");
        var brokenChain = JsonNode.Parse(File.ReadAllBytes(brokenChainPath))!.AsObject();
        brokenChain["HashChain"]![1]!["ChainHash"] = new string('0', 64);
        File.WriteAllText(
            brokenChainPath,
            brokenChain.ToJsonString(new JsonSerializerOptions { WriteIndented = true }),
            new UTF8Encoding(false));
        var brokenChainResult = EconomyEngine.OpenOrCreate(
            bundle.Terrain,
            new EconomySnapshotStore(brokenChainDirectory));
        Check(
            !brokenChainResult.Success &&
            brokenChainResult.Error?.Code == ErrorCodes.RecoveryInvalid &&
            brokenChainResult.Error.FirstBrokenBatch == 1,
            "recovery: broken chain returns E_RECOVERY_INVALID at first broken batch");

        var missingFieldDirectory = Path.Combine(root, "missing-field");
        CopyDirectory(store.RootDirectory, missingFieldDirectory);
        var missingFieldPath = Path.Combine(missingFieldDirectory, "economy-snapshot.json");
        var missingField = JsonNode.Parse(File.ReadAllBytes(missingFieldPath))!.AsObject();
        missingField["Current"]!.AsObject().Remove("Contributions");
        File.WriteAllText(
            missingFieldPath,
            missingField.ToJsonString(new JsonSerializerOptions { WriteIndented = true }),
            new UTF8Encoding(false));
        var missingFieldResult = EconomyEngine.OpenOrCreate(
            bundle.Terrain,
            new EconomySnapshotStore(missingFieldDirectory));
        Check(
            !missingFieldResult.Success &&
            missingFieldResult.Error?.Code == ErrorCodes.RecoveryInvalid &&
            missingFieldResult.Error.FirstBrokenBatch == 2,
            "recovery: missing contribution field returns E_RECOVERY_INVALID at current batch");
    }

    private async Task VerifyForcedTerminationAsync(IReadOnlyDictionary<string, ScenarioBundle> bundles)
    {
        var spec = ScenarioCatalog.BuiltIns[0];
        var bundle = bundles[spec.Name];
        var root = Path.Combine(_localData, "forced-termination");
        ResetDirectory(root);
        var storeDirectory = Path.Combine(root, "snapshots");
        var store = new EconomySnapshotStore(storeDirectory);
        var opened = EconomyEngine.OpenOrCreate(bundle.Terrain, store);
        if (!opened.Success || opened.Engine is null)
        {
            Check(false, "forced termination: setup engine opens");
            return;
        }

        var committed = await opened.Engine.SettleBatchAsync(bundle.Batches[0]);
        Check(committed.Success, "forced termination: setup commits batch 1");
        var preCrash = opened.Engine.QueryCurrent();
        var readyFile = Path.Combine(root, "ready.txt");
        var child = StartCrashChild(storeDirectory, spec.Name, pendingBatchNumber: 2, readyFile);
        try
        {
            var readyDeadline = Stopwatch.StartNew();
            while (!File.Exists(readyFile) && readyDeadline.Elapsed < TimeSpan.FromSeconds(15))
            {
                Thread.Sleep(50);
            }

            Check(File.Exists(readyFile), "forced termination: child persisted pending intent before kill");
            if (!File.Exists(readyFile))
            {
                return;
            }

            child.Kill(entireProcessTree: true);
            child.WaitForExit(10_000);
            Check(child.HasExited, "forced termination: child was forcibly terminated without normal exit");
        }
        finally
        {
            if (!child.HasExited)
            {
                child.Kill(entireProcessTree: true);
            }

            child.Dispose();
        }

        var recovered = EconomyEngine.OpenOrCreate(bundle.Terrain, store);
        Check(recovered.Success && recovered.Engine is not null, "forced termination: snapshot recovery succeeds");
        if (!recovered.Success || recovered.Engine is null)
        {
            return;
        }

        var recoveredEngine = recovered.Engine;
        var recoveredQuery = recoveredEngine.QueryCurrent();
        Check(
            recoveredQuery.ContentHash == preCrash.ContentHash &&
            recoveredQuery.Version == preCrash.Version,
            "forced termination: only the committed snapshot is restored");
        Check(
            recovered.PendingBatch?.BatchNumber == 2,
            "forced termination: unfinished batch intent is recovered");
        var resumed = await recoveredEngine.ResumePendingBatchAsync();
        Check(resumed.Success, "forced termination: unfinished batch resumes from committed watermark");
        Check(
            resumed.Trace?.SkippedDestructionCount == 0 &&
            resumed.Trace?.CommittedDestructionCount == bundle.Batches[1].Destructions.Length,
            "forced termination: unfinished batch settles each destruction exactly once");
        Check(
            recoveredEngine.QueryCurrent().Version == preCrash.Version + 1,
            "forced termination: resumed batch publishes exactly one version");

        store.SavePendingBatch(new RecoveryPendingBatch(
            bundle.Batches[1].BatchNumber!.Value,
            bundle.Batches[1].EconomicSteps,
            bundle.Batches[1].Destructions,
            preCrash.ContentHash));
        var staleIntent = EconomyEngine.OpenOrCreate(bundle.Terrain, store);
        Check(
            staleIntent.Success &&
            staleIntent.PendingBatch is null &&
            !File.Exists(store.PendingBatchPath),
            "forced termination: committed batch intent is discarded without resettlement");
    }

    private void VerifyByteIdenticalTrajectories()
    {
        var spec = ScenarioCatalog.BuiltIns[0];
        var bundle = DeterministicScenarioGenerator.Generate(spec);
        var first = Path.Combine(_localData, "determinism", "a");
        var second = Path.Combine(_localData, "determinism", "b");
        ResetDirectory(first);
        ResetDirectory(second);
        RunSynchronously(bundle, first);
        RunSynchronously(bundle, second);
        Check(
            File.ReadAllBytes(Path.Combine(first, "trajectory.jsonl"))
                .SequenceEqual(File.ReadAllBytes(Path.Combine(second, "trajectory.jsonl"))),
            "determinism: same seed and operation sequence produce byte-identical trajectory logs");
        Check(
            File.ReadAllBytes(Path.Combine(first, "economy-snapshot.json"))
                .SequenceEqual(File.ReadAllBytes(Path.Combine(second, "economy-snapshot.json"))),
            "determinism: same operation sequence produces byte-identical committed snapshots");
    }

    private void RunSynchronously(ScenarioBundle bundle, string directory)
    {
        var store = new EconomySnapshotStore(directory);
        using var log = new TrajectoryLog(Path.Combine(directory, "trajectory.jsonl"));
        var opened = EconomyEngine.OpenOrCreate(bundle.Terrain, store, log);
        Check(opened.Success && opened.Engine is not null, "determinism: engine opens");
        if (!opened.Success || opened.Engine is null)
        {
            return;
        }

        foreach (var batch in bundle.Batches)
        {
            var result = opened.Engine.SettleBatchAsync(batch).GetAwaiter().GetResult();
            Check(result.Success, "determinism: batch succeeds");
        }
    }

    private static Process StartCrashChild(
        string storeDirectory,
        string scenarioName,
        int pendingBatchNumber,
        string readyFile)
    {
        var assemblyPath = Path.Combine(AppContext.BaseDirectory, "BattleEconomy.Verify.dll");
        var host = Environment.GetEnvironmentVariable("DOTNET_HOST_PATH") ?? "dotnet";
        var startInfo = new ProcessStartInfo(host)
        {
            UseShellExecute = false,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            CreateNoWindow = true,
            WorkingDirectory = Path.GetDirectoryName(assemblyPath)!
        };
        startInfo.ArgumentList.Add(assemblyPath);
        startInfo.ArgumentList.Add("--crash-child");
        startInfo.ArgumentList.Add(storeDirectory);
        startInfo.ArgumentList.Add(scenarioName);
        startInfo.ArgumentList.Add(pendingBatchNumber.ToString());
        startInfo.ArgumentList.Add(readyFile);
        startInfo.Environment["DOTNET_SYSTEM_GLOBALIZATION_INVARIANT"] = "1";
        return Process.Start(startInfo) ??
               throw new InvalidOperationException("failed to start crash child");
    }

    private void Check(bool condition, string description)
    {
        _checkCount++;
        if (condition)
        {
            _passes.Add(description);
        }
        else
        {
            _failures.Add(description);
        }
    }

    private string WriteReport()
    {
        Directory.CreateDirectory(_localData);
        var builder = new StringBuilder();
        builder.AppendLine("BattleEconomy local verification");
        builder.AppendLine($"checks={_checkCount} passed={_passes.Count} failed={_failures.Count}");
        builder.AppendLine($"runtime={Environment.Version}");
        if (_failures.Count > 0)
        {
            builder.AppendLine();
            builder.AppendLine("FAILURES");
            foreach (var failure in _failures)
            {
                builder.AppendLine($"- {failure}");
            }
        }

        builder.AppendLine();
        builder.AppendLine("PASSED");
        foreach (var pass in _passes)
        {
            builder.AppendLine($"- {pass}");
        }

        var report = builder.ToString();
        File.WriteAllText(
            Path.Combine(_localData, "verification-report.txt"),
            report,
            new UTF8Encoding(false));
        return $"checks={_checkCount} passed={_passes.Count} failed={_failures.Count}; " +
               $"report={Path.Combine(_localData, "verification-report.txt")}";
    }

    private static void ResetDirectory(string path)
    {
        if (Directory.Exists(path))
        {
            Directory.Delete(path, recursive: true);
        }

        Directory.CreateDirectory(path);
    }

    private static void CopyDirectory(string source, string destination)
    {
        ResetDirectory(destination);
        foreach (var file in Directory.EnumerateFiles(source))
        {
            File.Copy(file, Path.Combine(destination, Path.GetFileName(file)));
        }
    }

    private static string FindRepositoryRoot()
    {
        var current = new DirectoryInfo(Directory.GetCurrentDirectory());
        while (current is not null)
        {
            if (File.Exists(Path.Combine(current.FullName, "BattleEconomy.Replay.sln")))
            {
                return current.FullName;
            }

            current = current.Parent;
        }

        current = new DirectoryInfo(AppContext.BaseDirectory);
        while (current is not null)
        {
            if (File.Exists(Path.Combine(current.FullName, "BattleEconomy.Replay.sln")))
            {
                return current.FullName;
            }

            current = current.Parent;
        }

        return Directory.GetCurrentDirectory();
    }
}
