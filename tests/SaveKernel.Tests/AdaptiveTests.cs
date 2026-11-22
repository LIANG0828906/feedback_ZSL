using SaveKernel.Compare;
using SaveKernel.Pipeline;

namespace SaveKernel.Tests;

[Collection("TestData")]
public sealed class AdaptiveTests
{
    private readonly TestDataFixture _data;
    private readonly MigrationPipeline _pipeline = new();

    public AdaptiveTests(TestDataFixture data) => _data = data;

    [Fact]
    public void SmallSave_UsesInMemory_LargeSave_UsesChunked()
    {
        var small = _pipeline.Run(_data.Raw("small", 1), new PipelineOptions());
        Assert.Equal(MigrationMode.InMemory, small.Mode);
        Assert.Single(small.Windows);

        var large = _pipeline.Run(_data.Raw("large", 1), new PipelineOptions());
        Assert.Equal(MigrationMode.Chunked, large.Mode);
        Assert.Equal(6, large.Windows.Count); // 1500 / 256 -> 6 段
    }

    [Fact]
    public void Chunked_RecordsSegmentBoundaries_AndLimitsChunkSize()
    {
        var result = _pipeline.Run(_data.Raw("large", 1), new PipelineOptions { ChunkSize = 256 });
        var expectedStart = 0;
        foreach (var w in result.Windows)
        {
            Assert.Equal(expectedStart, w.Start);
            Assert.True(w.End - w.Start <= 256); // 每段条目数受限
            expectedStart = w.End;
        }
        Assert.Equal(1500, expectedStart);
    }

    [Fact]
    public void TwoModes_ProduceIdenticalModelAndTraces()
    {
        var inMemory = _pipeline.Run(_data.Raw("large", 1),
            new PipelineOptions { ChunkThreshold = int.MaxValue });
        var chunked = _pipeline.Run(_data.Raw("large", 1),
            new PipelineOptions { ChunkSize = 128 });
        Assert.Equal(MigrationMode.InMemory, inMemory.Mode);
        Assert.Equal(MigrationMode.Chunked, chunked.Mode);
        ConsistencyVerifier.VerifyPair(inMemory, chunked);
        Assert.Equal(inMemory.ContentHash, chunked.ContentHash);
    }
}
