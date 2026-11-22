using System.Collections.Concurrent;
using SaveKernel.Checkpoint;
using SaveKernel.Export;
using SaveKernel.Hashing;
using SaveKernel.Loaders;
using SaveKernel.Migration;
using SaveKernel.Model;
using SaveKernel.Sampling;

namespace SaveKernel.Pipeline;

public enum MigrationMode { InMemory, Chunked }

/// <summary>测试钩子：模拟进程被强制终止（已提交窗口已持久化，未提交窗口丢失）。</summary>
public sealed class SimulatedCrashException : Exception
{
    public SimulatedCrashException(string message) : base(message) { }
}

public sealed record WindowInfo(int Index, int Start, int End, string Hash, bool Committed, bool Failed = false);

public sealed record PipelineOptions
{
    public int ChunkSize { get; init; } = 256;
    public int ChunkThreshold { get; init; } = 512;
    public int MaxParallelism { get; init; } = 1;
    public int BufferCapacity { get; init; } = 4;
    public int WindowRetryCount { get; init; } = 1;
    public string? CheckpointDir { get; init; }
    public int? CrashAfterCommits { get; init; }
    public int? FailWindowOnce { get; init; }
    public int? ExportToVersion { get; init; }
    public CancellationToken Cancellation { get; init; }
}

public sealed class PipelineResult
{
    public required SaveModel Model { get; init; }
    public required List<ConversionTrace> Traces { get; init; }
    public required MigrationMode Mode { get; init; }
    public required List<WindowInfo> Windows { get; init; }
    public required string ContentHash { get; init; }
    public required int SourceVersion { get; init; }
    public string RuleVersion => MigrationRules.RuleVersion;
    public bool ResumedFromCheckpoint { get; init; }
    public LossReport? Losses { get; set; }
    public string? ExportedText { get; set; }
}

/// <summary>
/// 迁移流水线：加载 -> 校验 -> 显式迁移链 -> 分段窗口迁移 -> 检查点提交 -> 哈希校验 -> 可选降级导出。
/// 支持内存整体迁移、分段迁移、并行窗口迁移与断点续跑，四条路径结果逐字段一致。
/// </summary>
public sealed class MigrationPipeline
{
    private readonly LoaderRegistry _loaders;
    private readonly MigrationChain _chain;

    /// <summary>单次运行的上下文：提交计数、窗口失败记录、检查点清单参数。</summary>
    private sealed class RunContext
    {
        public int Commits;
        public readonly HashSet<int> FailedOnce = new();
        public string InputHash = "";
        public int SourceVersion;
        public int ChunkSize;
        public int TotalWindows;
    }

    public MigrationPipeline(LoaderRegistry? loaders = null, MigrationChain? chain = null)
    {
        _loaders = loaders ?? new LoaderRegistry();
        _chain = chain ?? new MigrationChain();
    }

    public PipelineResult Run(string raw, PipelineOptions options, SampleManifest? manifest = null)
    {
        var model = _loaders.Load(raw);
        var sourceVersion = model.SourceVersion;

        if (manifest is not null)
            VerifyHash(model, manifest.ContentHash, manifest.Fields, "加载后");

        var route = _chain.FindRoute(sourceVersion, SaveModel.CurrentVersion);

        // 头部迁移（非道具字段），每步的痕迹按步骤缓存。
        var headerTraces = new Dictionary<string, List<ConversionTrace>>();
        foreach (var step in route)
        {
            var traces = new List<ConversionTrace>();
            step.ApplyHeader(model, traces);
            headerTraces[StepName(step)] = traces;
        }

        var itemSteps = route.Where(s => s.TouchesItems).ToList();
        var itemCount = model.Inventory.Count;
        var mode = itemCount > options.ChunkThreshold ? MigrationMode.Chunked : MigrationMode.InMemory;
        var chunkSize = mode == MigrationMode.InMemory ? Math.Max(itemCount, 1) : options.ChunkSize;

        var ctx = new RunContext { SourceVersion = sourceVersion, ChunkSize = chunkSize };
        var ranges = SplitRanges(itemCount, chunkSize);
        ctx.TotalWindows = ranges.Count;
        var windowResults = new WindowSnapshot?[ranges.Count];
        var windowInfos = new List<WindowInfo>();
        var resumed = false;

        CheckpointStore? store = null;
        CheckpointManifest? ckptManifest = null;
        var inputHash = Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(
            System.Text.Encoding.UTF8.GetBytes(raw))).ToLowerInvariant();
        ctx.InputHash = inputHash;

        var startWindow = 0;
        if (options.CheckpointDir is not null)
        {
            store = new CheckpointStore(options.CheckpointDir);
            ckptManifest = store.ReadManifest();
            var valid = ckptManifest is not null
                && ckptManifest.InputHash == inputHash
                && ckptManifest.RuleVersion == MigrationRules.RuleVersion
                && ckptManifest.SourceVersion == sourceVersion
                && ckptManifest.ChunkSize == chunkSize;
            List<WindowSnapshot>? committed = null;
            if (valid && ckptManifest!.CommittedWindows > 0)
            {
                committed = store.LoadCommittedWindows(ckptManifest);
                // 校验每个窗口内容哈希，任一损坏即整体作废
                if (committed is not null && committed.Any(w => w.WindowHash != HashWindowItems(w)))
                    committed = null;
            }
            if (committed is null && ckptManifest is not null)
                store.Invalidate(); // 哈希或规则版本不匹配：整体快照作废，全量重算

            if (committed is not null && committed.Count > 0)
            {
                resumed = true;
                foreach (var w in committed)
                {
                    windowResults[w.Index] = w;
                    ApplyWindowItems(model, w);
                    windowInfos.Add(new WindowInfo(w.Index, w.Start, w.End, w.WindowHash, Committed: true));
                }
                startWindow = committed.Count;
                store.ResetCommitCursor(startWindow);
                ctx.Commits = startWindow;
            }
            else
            {
                store.WriteManifest(new CheckpointManifest(
                    inputHash, MigrationRules.RuleVersion, sourceVersion,
                    chunkSize, ranges.Count, 0, false, null));
            }
        }

        // 执行剩余窗口
        var pending = Enumerable.Range(startWindow, ranges.Count - startWindow)
            .Select(i => (Index: i, Range: ranges[i])).ToList();

        if (options.MaxParallelism > 1 && pending.Count > 1)
            RunParallel(model, itemSteps, pending, options, store, windowResults, windowInfos, ctx);
        else
            RunSerial(model, itemSteps, pending, options, store, windowResults, windowInfos, ctx);

        // 汇总痕迹：步骤 -> 头部 -> 条目（按窗口下标顺序）
        var allTraces = new List<ConversionTrace>();
        foreach (var step in route)
        {
            allTraces.AddRange(headerTraces[StepName(step)]);
            var name = StepName(step);
            foreach (var w in windowResults)
                if (w is not null)
                    allTraces.AddRange(w.Traces.Where(t => t.Step == name));
        }

        model.Version = SaveModel.CurrentVersion;
        var contentHash = CanonicalForm.ComputeHash(model);
        if (manifest is not null)
            VerifyHash(model, manifest.CurrentHash, manifest.CurrentFields, "迁移后");

        store?.WriteManifest(new CheckpointManifest(
            inputHash, MigrationRules.RuleVersion, sourceVersion,
            chunkSize, ranges.Count, ranges.Count, true, contentHash));

        var result = new PipelineResult
        {
            Model = model,
            Traces = allTraces,
            Mode = mode,
            Windows = windowInfos,
            ContentHash = contentHash,
            SourceVersion = sourceVersion,
            ResumedFromCheckpoint = resumed
        };

        if (options.ExportToVersion is int target)
        {
            var (text, losses) = SaveExporter.Export(model, target);
            result.ExportedText = text;
            result.Losses = losses;
        }
        return result;
    }

    private void RunSerial(
        SaveModel model, List<IMigrationStep> itemSteps,
        List<(int Index, (int Start, int End) Range)> pending,
        PipelineOptions options, CheckpointStore? store,
        WindowSnapshot?[] results, List<WindowInfo> infos, RunContext ctx)
    {
        foreach (var (index, range) in pending)
        {
            options.Cancellation.ThrowIfCancellationRequested();
            var snapshot = ExecuteWindowWithRetry(model, itemSteps, index, range, options, ctx);
            results[index] = snapshot;
            Commit(store, snapshot, options, ctx);
            infos.Add(new WindowInfo(index, range.Start, range.End, snapshot.WindowHash, store is not null));
        }
    }

    private void RunParallel(
        SaveModel model, List<IMigrationStep> itemSteps,
        List<(int Index, (int Start, int End) Range)> pending,
        PipelineOptions options, CheckpointStore? store,
        WindowSnapshot?[] results, List<WindowInfo> infos, RunContext ctx)
    {
        // 有界队列：缓冲区耗尽时工作线程阻塞排队，不丢弃任何窗口结果。
        using var queue = new BlockingCollection<WindowSnapshot>(options.BufferCapacity);
        var infosSync = new object();
        var committer = Task.Run(() =>
        {
            var buffer = new SortedDictionary<int, WindowSnapshot>();
            var next = pending[0].Index;
            foreach (var snapshot in queue.GetConsumingEnumerable())
            {
                buffer[snapshot.Index] = snapshot;
                while (buffer.Remove(next, out var ordered))
                {
                    // 共享快照提交串行化，窗口顺序稳定
                    Commit(store, ordered, options, ctx);
                    lock (infosSync)
                        infos.Add(new WindowInfo(ordered.Index, ordered.Start, ordered.End, ordered.WindowHash, store is not null));
                    next++;
                }
            }
        });

        Parallel.ForEach(pending,
            new ParallelOptions { MaxDegreeOfParallelism = options.MaxParallelism, CancellationToken = options.Cancellation },
            item =>
            {
                var snapshot = ExecuteWindowWithRetry(model, itemSteps, item.Index, item.Range, options, ctx);
                results[item.Index] = snapshot;
                queue.Add(snapshot, options.Cancellation);
            });
        queue.CompleteAdding();
        committer.GetAwaiter().GetResult(); // 解包异常（如模拟崩溃）
        infos.Sort(static (a, b) => a.Index.CompareTo(b.Index));
    }

    private WindowSnapshot ExecuteWindowWithRetry(
        SaveModel model, List<IMigrationStep> itemSteps,
        int index, (int Start, int End) range, PipelineOptions options, RunContext ctx)
    {
        var attempts = 1 + options.WindowRetryCount;
        for (var attempt = 0; attempt < attempts; attempt++)
        {
            try
            {
                return ExecuteWindow(model, itemSteps, index, range, options, ctx);
            }
            catch (SaveKernelException) when (attempt < attempts - 1)
            {
                // 单窗口失败：重试，不污染其他窗口
            }
        }
        throw new SaveKernelException(ErrorCode.E_FORMAT, "不可达", windowIndex: index);
    }

    private WindowSnapshot ExecuteWindow(
        SaveModel model, List<IMigrationStep> itemSteps,
        int index, (int Start, int End) range, PipelineOptions options, RunContext ctx)
    {
        // 测试钩子：模拟窗口内部异常（仅首次）
        if (options.FailWindowOnce == index && ctx.FailedOnce.Add(index))
            throw new SaveKernelException(ErrorCode.E_DECODE, $"窗口 {index} 模拟内部异常", windowIndex: index);

        var traces = new List<ConversionTrace>();
        var items = new List<ItemEntry>(range.End - range.Start);
        for (var i = range.Start; i < range.End; i++)
        {
            var item = model.Inventory[i];
            foreach (var step in itemSteps)
                step.ApplyItem(model, i, item, traces);
            items.Add(item.Clone());
        }
        var snapshot = new WindowSnapshot(index, range.Start, range.End, items, traces, null!);
        return snapshot with { WindowHash = HashWindowItems(snapshot) };
    }

    private void Commit(CheckpointStore? store, WindowSnapshot snapshot, PipelineOptions options, RunContext ctx)
    {
        if (store is not null)
        {
            store.CommitWindow(snapshot);
            // 每提交一个窗口即原子更新恢复元数据（已提交窗口数）
            store.WriteManifest(new CheckpointManifest(
                ctx.InputHash, MigrationRules.RuleVersion, ctx.SourceVersion,
                ctx.ChunkSize, ctx.TotalWindows, snapshot.Index + 1, false, null));
        }
        var committed = Interlocked.Increment(ref ctx.Commits);
        if (options.CrashAfterCommits is int k && committed >= k)
            throw new SimulatedCrashException($"模拟进程崩溃：已提交 {committed} 个窗口后强制终止");
    }

    private static void ApplyWindowItems(SaveModel model, WindowSnapshot window)
    {
        for (var i = 0; i < window.Items.Count; i++)
        {
            var target = model.Inventory[window.Start + i];
            var source = window.Items[i];
            target.ItemId = source.ItemId;
            target.Count = source.Count;
            target.Durability = source.Durability;
        }
    }

    private static string HashWindowItems(WindowSnapshot window)
    {
        var entries = new List<KeyValuePair<string, string>>();
        for (var i = 0; i < window.Items.Count; i++)
        {
            var globalIndex = window.Start + i;
            var item = window.Items[i];
            entries.Add(new(CanonicalForm.ItemPath(globalIndex, "itemId"), item.ItemId));
            entries.Add(new(CanonicalForm.ItemPath(globalIndex, "count"), item.Count.ToString(System.Globalization.CultureInfo.InvariantCulture)));
            entries.Add(new(CanonicalForm.ItemPath(globalIndex, "durability"),
                item.Durability?.ToString(System.Globalization.CultureInfo.InvariantCulture) ?? "<none>"));
        }
        return CanonicalForm.HashEntries(entries);
    }

    private static List<(int Start, int End)> SplitRanges(int count, int chunkSize)
    {
        var ranges = new List<(int, int)>();
        for (var start = 0; start < count; start += chunkSize)
            ranges.Add((start, Math.Min(start + chunkSize, count)));
        if (ranges.Count == 0) ranges.Add((0, 0));
        return ranges;
    }

    private static string StepName(IMigrationStep step) => $"{step.FromVersion}->{step.ToVersion}";

    private static void VerifyHash(
        SaveModel model, string expectedHash,
        IReadOnlyDictionary<string, string> expectedFields, string stage)
    {
        var actualEntries = CanonicalForm.Entries(model);
        var actualHash = CanonicalForm.HashEntries(actualEntries);
        if (actualHash == expectedHash) return;
        var expectedEntries = expectedFields
            .Select(kv => new KeyValuePair<string, string>(kv.Key, kv.Value))
            .OrderBy(kv => kv.Key, StringComparer.Ordinal).ToList();
        var diff = CanonicalForm.FirstDiff(expectedEntries, actualEntries);
        throw new SaveKernelException(
            ErrorCode.E_HASH,
            $"{stage}内容哈希不一致: 期望 {expectedHash}，实际 {actualHash}",
            diff?.Path, diff?.Expected, diff?.Actual);
    }
}
