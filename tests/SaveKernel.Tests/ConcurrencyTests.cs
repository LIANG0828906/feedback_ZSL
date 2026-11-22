using SaveKernel.Compare;
using SaveKernel.Pipeline;

namespace SaveKernel.Tests;

[Collection("TestData")]
public sealed class ConcurrencyTests
{
    private readonly TestDataFixture _data;
    private readonly MigrationPipeline _pipeline = new();

    public ConcurrencyTests(TestDataFixture data) => _data = data;

    [Theory]
    [InlineData(2)]
    [InlineData(4)]
    [InlineData(8)]
    public void Parallel_MatchesSerial_FieldByField_SameCommitSequence(int maxParallelism)
    {
        var raw = _data.Raw("large", 1);
        var serial = _pipeline.Run(raw, new PipelineOptions());
        var parallel = _pipeline.Run(raw, new PipelineOptions
        {
            MaxParallelism = maxParallelism,
            BufferCapacity = 2 // 小缓冲：耗尽时阻塞排队而非丢弃
        });
        ConsistencyVerifier.VerifyPair(serial, parallel);
        Assert.Equal(serial.Windows.Select(w => w.Index), parallel.Windows.Select(w => w.Index));
        Assert.Equal(serial.ContentHash, parallel.ContentHash);
    }

    [Fact]
    public void Parallel_WithCheckpoint_CommitsInWindowOrder()
    {
        var dir = _data.NewCheckpointDir("parallel-ckpt");
        var result = _pipeline.Run(_data.Raw("large", 1), new PipelineOptions
        {
            CheckpointDir = dir, MaxParallelism = 4, BufferCapacity = 2
        });
        Assert.Equal(Enumerable.Range(0, 6), result.Windows.Select(w => w.Index));
        var store = new SaveKernel.Checkpoint.CheckpointStore(dir);
        Assert.Equal(6, store.ReadManifest()!.CommittedWindows);
    }

    [Fact]
    public void WindowFailure_IsRetried_WithoutPollutingOthers()
    {
        var raw = _data.Raw("large", 1);
        var reference = _pipeline.Run(raw, new PipelineOptions());
        var withFailure = _pipeline.Run(raw, new PipelineOptions
        {
            FailWindowOnce = 2, // 窗口 2 首次执行抛内部异常
            WindowRetryCount = 1
        });
        // 重试后结果与无故障运行一致
        ConsistencyVerifier.VerifyPair(reference, withFailure);
    }

    [Fact]
    public void WindowFailure_ExhaustingRetries_FailsOnlyThatWindow()
    {
        var raw = _data.Raw("large", 1);
        var ex = Assert.Throws<SaveKernelException>(() => _pipeline.Run(raw,
            new PipelineOptions { FailWindowOnce = 1, WindowRetryCount = 0 }));
        Assert.Equal(1, ex.WindowIndex);
    }

    [Fact]
    public async Task Cancellation_LeavesOnlyCommittedWindowsForResume()
    {
        var dir = _data.NewCheckpointDir("cancel");
        using var cts = new CancellationTokenSource();
        var raw = _data.Raw("large", 1);
        var run = Task.Run(() => _pipeline.Run(raw, new PipelineOptions
        {
            CheckpointDir = dir, MaxParallelism = 2, Cancellation = cts.Token
        }));
        cts.CancelAfter(50);
        try { await run; } catch { /* 取消或完成均可 */ }

        // 重启：只能依据已持久化且版本匹配的窗口恢复，结果必须与全量一致
        var resumed = _pipeline.Run(raw, new PipelineOptions { CheckpointDir = dir });
        var full = _pipeline.Run(raw, new PipelineOptions());
        ConsistencyVerifier.VerifyPair(full, resumed);
    }
}
