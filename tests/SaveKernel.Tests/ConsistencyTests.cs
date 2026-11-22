using SaveKernel.Compare;
using SaveKernel.Migration;
using SaveKernel.Pipeline;

namespace SaveKernel.Tests;

[Collection("TestData")]
public sealed class ConsistencyTests
{
    private readonly TestDataFixture _data;
    private readonly MigrationPipeline _pipeline = new();

    public ConsistencyTests(TestDataFixture data) => _data = data;

    [Fact]
    public void ThreePaths_Full_Chunked_Resumed_AreIdentical()
    {
        var raw = _data.Raw("large", 1);
        var manifest = _data.Manifest("large", 1);
        var options = new PipelineOptions { ExportToVersion = 1 };

        var full = _pipeline.Run(raw, options with { ChunkThreshold = int.MaxValue }, manifest);
        var chunked = _pipeline.Run(raw, options, manifest);

        var dir = _data.NewCheckpointDir("three-paths");
        Assert.Throws<SimulatedCrashException>(() => _pipeline.Run(raw,
            options with { CheckpointDir = dir, CrashAfterCommits = 4 }, manifest));
        var resumed = _pipeline.Run(raw, options with { CheckpointDir = dir }, manifest);

        ConsistencyVerifier.VerifyAllEqual(new[] { full, chunked, resumed });
        Assert.Equal(full.ContentHash, resumed.ContentHash);
        Assert.Equal(full.Traces, resumed.Traces);
        Assert.Equal(full.Losses!.Entries, resumed.Losses!.Entries);
    }

    [Fact]
    public void Divergence_ModelField_ReportsPathAndWindow()
    {
        var raw = _data.Raw("large", 1);
        var reference = _pipeline.Run(raw, new PipelineOptions());
        var tampered = _pipeline.Run(raw, new PipelineOptions());
        tampered.Model.Inventory[300].Count += 1; // 第 300 条 -> 窗口 1 (256/段)

        var ex = Assert.Throws<SaveKernelException>(
            () => ConsistencyVerifier.VerifyPair(reference, tampered));
        Assert.Equal(ErrorCode.E_DIVERGE, ex.Code);
        Assert.Contains("ModelField", ex.Message);
        Assert.Equal("inventory.000300.count", ex.FieldPath);
        Assert.Equal(1, ex.WindowIndex);
    }

    [Fact]
    public void Divergence_TraceSequence_IsDistinguished()
    {
        var raw = _data.Raw("large", 1);
        var reference = _pipeline.Run(raw, new PipelineOptions());
        var tampered = _pipeline.Run(raw, new PipelineOptions());
        tampered.Traces.RemoveAt(0); // 删除首条痕迹（头部单位换算）

        var ex = Assert.Throws<SaveKernelException>(
            () => ConsistencyVerifier.VerifyPair(reference, tampered));
        Assert.Equal(ErrorCode.E_DIVERGE, ex.Code);
        Assert.Contains("TraceSequence", ex.Message);
    }

    [Fact]
    public void Divergence_LossList_IsDistinguished()
    {
        var raw = _data.Raw("small", 3);
        var options = new PipelineOptions { ExportToVersion = 1 };
        var reference = _pipeline.Run(raw, options);
        var tampered = _pipeline.Run(raw, options);
        tampered.Losses!.Entries.ToList();
        // 通过反射之外的方式：重新导出一份并删除一条损失
        var modified = new SaveKernel.Export.LossReport();
        foreach (var e in tampered.Losses!.Entries.Skip(1)) modified.Add(e.FieldPath, e.OriginalValue, e.Disposition);
        tampered.Losses = modified;

        var ex = Assert.Throws<SaveKernelException>(
            () => ConsistencyVerifier.VerifyPair(reference, tampered));
        Assert.Equal(ErrorCode.E_DIVERGE, ex.Code);
        Assert.Contains("LossList", ex.Message);
    }
}
