using SaveKernel.Compare;
using SaveKernel.Export;
using SaveKernel.Loaders;
using SaveKernel.Pipeline;

namespace SaveKernel.Tests;

[Collection("TestData")]
public sealed class ExportTests
{
    private readonly TestDataFixture _data;
    private readonly MigrationPipeline _pipeline = new();

    public ExportTests(TestDataFixture data) => _data = data;

    private SaveKernel.Model.SaveModel MigrateToCurrent(string saveId, int version) =>
        _pipeline.Run(_data.Raw(saveId, version), new PipelineOptions()).Model;

    [Fact]
    public void Downgrade_ToV2_ReportsSkillTreeLoss_WithPathValueDisposition()
    {
        var model = MigrateToCurrent("small", 3);
        var (text, losses) = SaveExporter.Export(model, 2);
        Assert.True(losses.HasLoss);
        Assert.Equal(2, losses.Entries.Count); // vitality + focus
        var entry = Assert.Single(losses.Entries, e => e.FieldPath == "skillTree.vitality");
        Assert.Equal(ErrorCode.E_LOSSY, entry.Code);
        Assert.Equal("2", entry.OriginalValue);
        Assert.Equal(SaveExporter.DispositionDropped, entry.Disposition);
        Assert.Contains("version=2", text);
    }

    [Fact]
    public void Downgrade_ToV1_ReportsSkillDurabilityAndUnitConversion()
    {
        var model = MigrateToCurrent("small", 3);
        var (_, losses) = SaveExporter.Export(model, 1);
        Assert.Equal(12, losses.Entries.Count(e => e.FieldPath.EndsWith(".durability")));
        Assert.Equal(2, losses.Entries.Count(e => e.FieldPath.StartsWith("skillTree.")));
        var time = Assert.Single(losses.Entries, e => e.FieldPath == "playtime");
        Assert.Equal(SaveExporter.DispositionConverted, time.Disposition); // 整除无截断
    }

    [Fact]
    public void Downgrade_ToV1_TruncatesSubSecondRemainder()
    {
        var model = MigrateToCurrent("small", 3);
        model.Playtime = 3_600_500; // 非整秒
        var (text, losses) = SaveExporter.Export(model, 1);
        var time = Assert.Single(losses.Entries, e => e.FieldPath == "playtime");
        Assert.Equal(SaveExporter.DispositionTruncated, time.Disposition);
        Assert.Contains("playtime_sec=3600", text);
    }

    [Theory]
    [InlineData(1)]
    [InlineData(2)]
    [InlineData(3)]
    public void Downgrade_Product_CanBeReloaded_AndRoundTripExpressibleEqual(int target)
    {
        var model = MigrateToCurrent("large", 3);
        var (text, losses) = SaveExporter.Export(model, target);
        // 降级产物必须能重新被对应版本加载器读回
        var reloaded = new LoaderRegistry().Load(text);
        Assert.Equal(target, reloaded.SourceVersion);
        Assert.Equal(model.Inventory.Count, reloaded.Inventory.Count);
        // 往返后源版本可表达字段逐项等价
        var roundTrip = _pipeline.Run(text, new PipelineOptions());
        ConsistencyVerifier.VerifyExpressibleEqual(model, roundTrip.Model, target);
        if (target == 3) Assert.False(losses.HasLoss);
    }
}
