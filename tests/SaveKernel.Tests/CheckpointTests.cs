using SaveKernel.Checkpoint;
using SaveKernel.Compare;
using SaveKernel.Migration;
using SaveKernel.Pipeline;

namespace SaveKernel.Tests;

[Collection("TestData")]
public sealed class CheckpointTests
{
    private readonly TestDataFixture _data;
    private readonly MigrationPipeline _pipeline = new();

    public CheckpointTests(TestDataFixture data) => _data = data;

    [Fact]
    public void Crash_ThenResume_ContinuesFromCommittedWindows()
    {
        var dir = _data.NewCheckpointDir("resume");
        var raw = _data.Raw("large", 1);
        var manifest = _data.Manifest("large", 1);

        Assert.Throws<SimulatedCrashException>(() => _pipeline.Run(raw,
            new PipelineOptions { CheckpointDir = dir, CrashAfterCommits = 2 }, manifest));

        var store = new CheckpointStore(dir);
        var ckpt = store.ReadManifest();
        Assert.NotNull(ckpt);
        Assert.Equal(2, ckpt!.CommittedWindows);
        Assert.False(ckpt.Completed);

        var resumed = _pipeline.Run(raw, new PipelineOptions { CheckpointDir = dir }, manifest);
        Assert.True(resumed.ResumedFromCheckpoint);
        Assert.Equal(6, resumed.Windows.Count);

        var full = _pipeline.Run(raw, new PipelineOptions(), manifest);
        ConsistencyVerifier.VerifyPair(full, resumed);
    }

    [Fact]
    public void RuleVersionMismatch_InvalidatesWholeSnapshot()
    {
        var dir = _data.NewCheckpointDir("rule-mismatch");
        var raw = _data.Raw("large", 1);
        Assert.Throws<SimulatedCrashException>(() => _pipeline.Run(raw,
            new PipelineOptions { CheckpointDir = dir, CrashAfterCommits = 4 }));

        // 篡改清单中的规则版本 -> 整体作废，禁止部分混用
        var store = new CheckpointStore(dir);
        var ckpt = store.ReadManifest()!;
        store.WriteManifest(ckpt with { RuleVersion = "rules-0.9" });

        var rerun = _pipeline.Run(raw, new PipelineOptions { CheckpointDir = dir });
        Assert.False(rerun.ResumedFromCheckpoint); // 全量重算
        Assert.Equal(MigrationRules.RuleVersion, new CheckpointStore(dir).ReadManifest()!.RuleVersion);
    }

    [Fact]
    public void InputHashMismatch_InvalidatesWholeSnapshot()
    {
        var dir = _data.NewCheckpointDir("hash-mismatch");
        Assert.Throws<SimulatedCrashException>(() => _pipeline.Run(_data.Raw("large", 1),
            new PipelineOptions { CheckpointDir = dir, CrashAfterCommits = 4 }));

        // 换一份输入（内容哈希不同） -> 快照作废
        var rerun = _pipeline.Run(_data.Raw("large", 2), new PipelineOptions { CheckpointDir = dir });
        Assert.False(rerun.ResumedFromCheckpoint);
    }

    [Fact]
    public void CorruptedWindowFile_InvalidatesWholeSnapshot()
    {
        var dir = _data.NewCheckpointDir("corrupt-window");
        var raw = _data.Raw("large", 1);
        Assert.Throws<SimulatedCrashException>(() => _pipeline.Run(raw,
            new PipelineOptions { CheckpointDir = dir, CrashAfterCommits = 3 }));
        File.WriteAllText(Path.Combine(dir, "windows", "w000001.json"), "{ not json");
        var rerun = _pipeline.Run(raw, new PipelineOptions { CheckpointDir = dir });
        Assert.False(rerun.ResumedFromCheckpoint);
    }

    [Fact]
    public void CommittedWindows_AreAtomicallyReadable_AfterCrash()
    {
        var dir = _data.NewCheckpointDir("atomic");
        Assert.Throws<SimulatedCrashException>(() => _pipeline.Run(_data.Raw("large", 1),
            new PipelineOptions { CheckpointDir = dir, CrashAfterCommits = 2 }));
        var store = new CheckpointStore(dir);
        var windows = store.LoadCommittedWindows(store.ReadManifest()!);
        Assert.NotNull(windows);
        Assert.Equal(2, windows!.Count);
        Assert.Equal((0, 256), (windows[0].Start, windows[0].End));
        Assert.All(windows, w => Assert.False(string.IsNullOrEmpty(w.WindowHash)));
    }
}
