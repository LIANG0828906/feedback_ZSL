using SaveKernel;
using SaveKernel.Compare;
using SaveKernel.Export;
using SaveKernel.Loaders;
using SaveKernel.Pipeline;
using SaveKernel.Sampling;

var root = args.Length > 0 ? args[0] : "data";
var dataDir = Path.Combine(root, "saves");
var ckptDir = Path.Combine(root, "checkpoints");

Console.WriteLine("=== 存档与版本兼容内核 演示 ===");
Console.WriteLine($"数据目录: {Path.GetFullPath(dataDir)}");
Console.WriteLine();

// 1. 确定性样本生成（固定种子 42，版本清单 1/2/3）
var manifests = SampleGenerator.Generate(dataDir);
Console.WriteLine($"[样本] 已生成 {manifests.Count} 份清单");
foreach (var m in manifests)
    Console.WriteLine($"  {m.SaveId}.v{m.Version}: 道具 {m.Truth.ItemCount} 条, contentHash={m.ContentHash[..12]}…, currentHash={m.CurrentHash[..12]}…");
Console.WriteLine();

var pipeline = new MigrationPipeline();

// 2. 逐版本加载 + 迁移 + 哈希校验 + 真值观测
foreach (var m in manifests)
{
    var raw = File.ReadAllText(Path.Combine(dataDir, $"{m.SaveId}.v{m.Version}.sav"));
    var result = pipeline.Run(raw, new PipelineOptions(), m);
    var truth = m.Truth;
    var ok = result.Model.LevelId == truth.LevelId && result.Model.Gold == truth.Gold
        && result.Model.Inventory.Count == truth.ItemCount
        && result.Model.PosX == truth.PosX && result.Model.PosY == truth.PosY;
    Console.WriteLine($"[迁移] {m.SaveId}.v{m.Version} -> v{result.Model.Version}: " +
        $"模式={result.Mode}, 窗口={result.Windows.Count}, 痕迹={result.Traces.Count} 条, " +
        $"真值校验={(ok ? "通过" : "失败")} (关卡={result.Model.LevelId}, 金币={result.Model.Gold}, " +
        $"道具={result.Model.Inventory.Count}, 坐标=({result.Model.PosX},{result.Model.PosY}))");
}
Console.WriteLine();

// 3. 降级导出与损失清单
var largeRaw = File.ReadAllText(Path.Combine(dataDir, "large.v3.sav"));
var largeMigrated = pipeline.Run(largeRaw, new PipelineOptions(), manifests.First(x => x.SaveId == "large" && x.Version == 3));
foreach (var target in new[] { 2, 1 })
{
    var (text, losses) = SaveExporter.Export(largeMigrated.Model, target);
    Console.WriteLine($"[降级] v3 -> v{target}: E_LOSSY 警告 {losses.Entries.Count} 条");
    foreach (var entry in losses.Entries.Take(3))
        Console.WriteLine($"  {entry.Code} {entry.FieldPath} 原值={entry.OriginalValue} 处置={entry.Disposition}");
    if (losses.Entries.Count > 3) Console.WriteLine($"  … 其余 {losses.Entries.Count - 3} 条从略");
    // 往返：降级产物可被对应版本加载器读回
    var reloaded = new LoaderRegistry().Load(text);
    var roundTrip = pipeline.Run(text, new PipelineOptions());
    ConsistencyVerifier.VerifyExpressibleEqual(largeMigrated.Model, roundTrip.Model, target);
    Console.WriteLine($"  往返校验: v{target} 可表达字段逐项等价 (读回 {reloaded.Inventory.Count} 条道具)");
}
Console.WriteLine();

// 4. 三条路径一致性：一次性全量 / 实时分段 / 断点恢复
var largeV1Raw = File.ReadAllText(Path.Combine(dataDir, "large.v1.sav"));
var largeV1Manifest = manifests.First(x => x.SaveId == "large" && x.Version == 1);

var full = pipeline.Run(largeV1Raw, new PipelineOptions
{
    ChunkThreshold = int.MaxValue, ExportToVersion = 1 // 强制内存整体迁移（一次性全量）
}, largeV1Manifest);
var chunked = pipeline.Run(largeV1Raw, new PipelineOptions { ExportToVersion = 1 }, largeV1Manifest);

var ckpt1 = Path.Combine(ckptDir, "large-v1");
if (Directory.Exists(ckpt1)) Directory.Delete(ckpt1, true);
var crashed = false;
try
{
    pipeline.Run(largeV1Raw, new PipelineOptions
    {
        CheckpointDir = ckpt1, ExportToVersion = 1, CrashAfterCommits = 3
    }, largeV1Manifest);
}
catch (SimulatedCrashException ex)
{
    crashed = true;
    Console.WriteLine($"[断点] {ex.Message}");
}
var resumed = pipeline.Run(largeV1Raw, new PipelineOptions
{
    CheckpointDir = ckpt1, ExportToVersion = 1
}, largeV1Manifest);
Console.WriteLine($"[断点] 崩溃={crashed}, 恢复续跑={resumed.ResumedFromCheckpoint}, 窗口={resumed.Windows.Count}");

ConsistencyVerifier.VerifyAllEqual(new[] { full, chunked, resumed });
Console.WriteLine("[一致] 全量 / 分段 / 断点恢复 三条路径逐字段一致（模型+痕迹+损失清单）");

// 5. 并行 vs 串行
var parallel = pipeline.Run(largeV1Raw, new PipelineOptions
{
    MaxParallelism = 4, BufferCapacity = 2, ExportToVersion = 1
}, largeV1Manifest);
ConsistencyVerifier.VerifyPair(full, parallel);
var commitOrder = string.Join(',', parallel.Windows.Select(w => w.Index));
Console.WriteLine($"[并行] 4 线程并行迁移完成，窗口提交序列=[{commitOrder}]，与串行结果一致");
Console.WriteLine();
Console.WriteLine("=== 演示完成 ===");
